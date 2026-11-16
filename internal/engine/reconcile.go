package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	"gitmirror/internal/config"
	"gitmirror/internal/forge"
	"gitmirror/internal/resolve"
	"gitmirror/internal/state"
)

// StepReconcile is the step name recorded for source-deletion handling.
const StepReconcile = "reconcile"

// DefaultDeletionGrace is the grace period used when a rule's
// RuleSettings.DeletionGrace is not positive (config always sets one).
const DefaultDeletionGrace = 24 * time.Hour

// Reconcile applies each rule's OnSourceDeleted policy to pairs that are
// known to the state store but no longer resolved.
//
// A stored pair is considered only when its rule is in settings (rules
// removed from the config are left alone), the rule's resolution was
// complete, the pair is neither in current nor in dropped, and its
// destination is not the destination of any pair in current or dropped, nor
// of a stored pair of a configured rule that was not resolved this run (a
// transferred source or a re-pointed rule leaves a stale pair behind whose
// destination a live pair still mirrors into; such a stale pair is skipped
// and its missing record cleared). Even then only a positive "not found"
// from the source forge counts: if the source exists (it was merely
// filtered out) its missing record is cleared, and any other error leaves
// the pair untouched. The source is checked again under the pair lock, so a
// sync that finished in between is not missed.
//
// Forges answer "not found" for repositories the token merely lost access
// to, so the policy is applied only once the source has been continuously
// missing for the rule's DeletionGrace, measured from the first not-found
// (Store.MarkMissingObserved). Observation must be continuous: when the
// previous not-found is older than max(2 x the rule's interval, 1h) (the
// process was down, or the rule's resolution was incomplete) the grace
// period restarts. Until then the pair gets a report saying when the policy
// would apply, and no run is recorded.
//
// Policies: "keep" does nothing; "archive" archives a destination gitmirror
// owns; "delete" deletes a destination gitmirror created, when the provider
// can delete repositories, and archives it otherwise (including adopted
// destinations). A deleted pair is removed from the state store. Applying a
// policy is recorded as one run with trigger "<trigger>:reconcile"; "keep"
// and an already archived destination record nothing, and an already
// archived destination produces no Report. An outcome that changes nothing
// (destination not owned, or already gone) is recorded as a run once per
// missing period; later cycles produce no Report and no run. A real archive
// or delete is also appended to the store's audit log, which survives the
// pair's removal. A dry run writes nothing to any forge or to the state store.
func (e *Engine) Reconcile(ctx context.Context, current []resolve.Pair, complete map[string]bool, dropped []resolve.Pair, settings map[string]config.RuleSettings, opt Options) []Report {
	log := e.logger()
	states, err := e.Store.Pairs(ctx)
	if err != nil {
		log.Error("engine: reconcile: list pairs", "err", err)
		return nil
	}
	skip := map[string]bool{}
	live := map[string]bool{} // destinations of current and dropped pairs
	for _, ps := range [][]resolve.Pair{current, dropped} {
		for _, p := range ps {
			skip[p.Key().String()] = true
			live[dstIdentity(p.DstForge, p.DstFullName())] = true
		}
	}
	// Rules that are configured but were not resolved in this run (e.g.
	// `sync --rule other`) may still mirror into a destination: treat their
	// stored pairs' destinations as live too.
	for _, st := range states {
		if _, configured := settings[st.Key.Rule]; configured && !complete[st.Key.Rule] {
			live[dstIdentity(st.Key.DstForge, st.Key.DstRepo)] = true
		}
	}
	var reps []Report
	for _, st := range states {
		if ctx.Err() != nil {
			break
		}
		k := st.Key
		if !complete[k.Rule] || skip[k.String()] {
			continue
		}
		rs, ok := settings[k.Rule]
		if !ok {
			continue
		}
		if live[dstIdentity(k.DstForge, k.DstRepo)] {
			// A live pair mirrors into this destination (the source was
			// transferred or the rule re-pointed): never act on it here.
			log.Debug("engine: reconcile: destination is mirrored by a live pair; skipping stale pair", "pair", k.String())
			if !opt.DryRun {
				if err := e.Store.ClearMissing(ctx, k); err != nil {
					log.Warn("engine: reconcile: clear missing record", "pair", k.String(), "err", err)
				}
			}
			continue
		}
		srcP, ok := e.provider(opt, k.SrcForge)
		if !ok {
			log.Warn("engine: reconcile: unknown source forge", "pair", k.String(), "forge", k.SrcForge)
			continue
		}
		owner, name, _ := strings.Cut(k.SrcRepo, "/")
		_, err := srcP.GetRepo(ctx, owner, name)
		if err == nil {
			// The source exists: any earlier not-found was transient.
			if !opt.DryRun {
				if err := e.Store.ClearMissing(ctx, k); err != nil {
					log.Warn("engine: reconcile: clear missing record", "pair", k.String(), "err", err)
				}
			}
			continue
		}
		if !errors.Is(err, forge.ErrNotFound) {
			log.Warn("engine: reconcile: cannot confirm source deletion; leaving pair alone", "pair", k.String(), "err", err)
			continue
		}
		if rep, ok := e.reconcileOne(ctx, k, srcP, rs, opt); ok {
			reps = append(reps, rep)
		}
	}
	return reps
}

// dstIdentity is the key of a destination: forge name and lowercase repo.
func dstIdentity(dstForge, dstRepo string) string {
	return dstForge + "\x00" + strings.ToLower(dstRepo)
}

// missingGap is the longest allowed time between two not-found
// observations of a source before its deletion grace restarts.
func missingGap(rs config.RuleSettings) time.Duration {
	return max(2*rs.Interval, time.Hour)
}

func (e *Engine) now() time.Time {
	if e.Now == nil {
		return time.Now()
	}
	return e.Now()
}

// reconcileOne handles a pair whose source was reported as not found. ok is
// false when there is nothing to report.
func (e *Engine) reconcileOne(ctx context.Context, k state.PairKey, srcP forge.Provider, rs config.RuleSettings, opt Options) (rep Report, ok bool) {
	rep.Key = k
	unlock, err := e.lock(ctx, k.String())
	if err != nil {
		rep.Status, rep.Err = state.RunAborted, err
		return rep, true
	}
	defer unlock()
	log := e.logger().With("pair", k.String())

	// A sync of this pair may have finished between the unlocked check and
	// taking the lock: confirm the source is still not found.
	srcOwner, srcName, _ := strings.Cut(k.SrcRepo, "/")
	if _, err := srcP.GetRepo(ctx, srcOwner, srcName); err == nil {
		log.Debug("engine: reconcile: source exists on re-check under the pair lock")
		if !opt.DryRun {
			if err := e.Store.ClearMissing(ctx, k); err != nil {
				log.Warn("engine: reconcile: clear missing record", "err", err)
			}
		}
		return rep, false
	} else if !errors.Is(err, forge.ErrNotFound) {
		log.Warn("engine: reconcile: cannot confirm source deletion; leaving pair alone", "err", err)
		return rep, false
	}
	act := func(format string, args ...any) {
		rep.Actions = append(rep.Actions, fmt.Sprintf(format, args...))
	}
	start := time.Now()
	noRun := func(status state.StepStatus, detail string) (Report, bool) {
		rep.Status = state.RunOK
		rep.Steps = []state.StepResult{{Step: StepReconcile, Status: status, Detail: detail, Duration: time.Since(start)}}
		return rep, true
	}

	// Grace period: nothing destructive until the source has been
	// continuously missing for DeletionGrace.
	now := e.now()
	firstSeen := now
	gap := missingGap(rs)
	if opt.DryRun {
		since, last, found, err := e.Store.MissingRecord(ctx, k)
		if err != nil {
			rep.Status, rep.Err = state.RunFailed, fmt.Errorf("engine: read missing record: %w", err)
			return rep, true
		}
		// As MarkMissingObserved would: a stale observation restarts the clock.
		if found && !last.IsZero() && now.Sub(last) <= gap {
			firstSeen = since
		}
	} else if firstSeen, err = e.Store.MarkMissingObserved(ctx, k, gap); err != nil {
		rep.Status, rep.Err = state.RunFailed, fmt.Errorf("engine: record missing source: %w", err)
		return rep, true
	}
	grace := rs.DeletionGrace
	if grace <= 0 {
		grace = DefaultDeletionGrace
	}
	if until := firstSeen.Add(grace); now.Before(until) {
		act("source missing since %s; waiting until %s before applying on_source_deleted=%s",
			firstSeen.UTC().Format(time.RFC3339), until.UTC().Format(time.RFC3339), rs.OnSourceDeleted)
		log.Info("engine: reconcile: source missing; within deletion grace",
			"since", firstSeen, "until", until, "policy", rs.OnSourceDeleted, "dry_run", opt.DryRun)
		return noRun(state.StepSkipped, "source missing; within deletion grace until "+until.UTC().Format(time.RFC3339))
	}

	plan, perr := e.safePlanDeletion(ctx, k, rs, opt, log)
	if perr == nil {
		switch plan.kind {
		case planKeep:
			act("source deleted; keeping destination")
			return noRun(state.StepUnchanged, "source deleted; keeping destination")
		case planArchived:
			log.Debug("engine: reconcile: destination already archived")
			return rep, false
		case planNotOwned, planGone:
			// These change nothing; record them once per missing period
			// (the flag lives on the missing record, which ClearMissing
			// resets), not on every reconcile cycle.
			reported, err := e.Store.MissingReported(ctx, k)
			if err != nil {
				rep.Status, rep.Err = state.RunFailed, fmt.Errorf("engine: read missing record: %w", err)
				return rep, true
			}
			if reported {
				log.Debug("engine: reconcile: outcome already recorded; nothing to do", "outcome", plan.kind.String())
				return rep, false
			}
		}
	}

	if !opt.DryRun {
		id, err := e.Store.StartRun(ctx, k, opt.trigger()+":reconcile")
		if err != nil {
			rep.Status, rep.Err = state.RunFailed, fmt.Errorf("engine: start run: %w", err)
			return rep, true
		}
		rep.RunID = id
	}
	var stepStatus state.StepStatus
	var detail string
	deleted := false
	defer func() {
		if v := recover(); v != nil {
			log.Error("engine: panic during reconcile", "panic", v, "stack", string(debug.Stack()))
			err = fmt.Errorf("engine: panic: %v", v)
			stepStatus, detail = state.StepFailed, err.Error()
			deleted = false
		}
		if err != nil {
			rep.Status, rep.Err = state.RunFailed, err
		} else {
			rep.Status = state.RunOK
		}
		rep.Steps = []state.StepResult{{Step: StepReconcile, Status: stepStatus, Detail: detail, Duration: time.Since(start)}}
		ok = true
		if opt.DryRun {
			return
		}
		msg := ""
		if rep.Err != nil {
			msg = rep.Err.Error()
		}
		if ferr := e.Store.FinishRun(context.WithoutCancel(ctx), rep.RunID, rep.Status, rep.Steps, msg, "", 0); ferr != nil {
			log.Error("engine: finish run", "err", ferr)
		}
		if rep.Err == nil && (plan.kind == planNotOwned || plan.kind == planGone) {
			if serr := e.Store.SetMissingReported(context.WithoutCancel(ctx), k); serr != nil {
				log.Error("engine: flag reconcile outcome as recorded", "err", serr)
			}
		}
		if deleted {
			// The destination is gone: forget the pair so it never
			// reappears in later cycles.
			if derr := e.Store.DeletePair(context.WithoutCancel(ctx), k); derr != nil {
				log.Error("engine: forget deleted pair", "err", derr)
			}
		}
	}()
	if perr != nil {
		err = perr
		stepStatus, detail = state.StepFailed, err.Error()
		return rep, true
	}
	stepStatus, detail, err = e.applyDeletionPlan(ctx, k, plan, opt.DryRun, act)
	if err != nil {
		stepStatus, detail = state.StepFailed, err.Error()
	} else {
		deleted = plan.kind == planDelete && !opt.DryRun
	}
	return rep, true
}

type planKind int

const (
	planKeep     planKind = iota // policy "keep"
	planNotOwned                 // destination not owned by gitmirror
	planGone                     // destination already gone (archive path)
	planArchived                 // destination already archived
	planArchive                  // archive the destination
	planDelete                   // delete the destination
)

func (k planKind) String() string {
	switch k {
	case planKeep:
		return "keep"
	case planNotOwned:
		return "destination not owned"
	case planGone:
		return "destination already gone"
	case planArchived:
		return "destination already archived"
	case planArchive:
		return "archive"
	case planDelete:
		return "delete"
	}
	return fmt.Sprintf("plan(%d)", int(k))
}

// deletionPlan is what applying OnSourceDeleted to a pair would do.
type deletionPlan struct {
	kind planKind
	note string // why archiving replaces deleting, if it does
	dstP forge.Provider
	del  forge.RepoDeleter
}

// safePlanDeletion is planDeletion with a panic turned into an error.
func (e *Engine) safePlanDeletion(ctx context.Context, k state.PairKey, rs config.RuleSettings, opt Options, log *slog.Logger) (plan deletionPlan, err error) {
	defer func() {
		if v := recover(); v != nil {
			log.Error("engine: panic during reconcile", "panic", v, "stack", string(debug.Stack()))
			plan, err = deletionPlan{}, fmt.Errorf("engine: panic: %v", v)
		}
	}()
	return e.planDeletion(ctx, k, rs, opt)
}

// planDeletion decides what the pair's OnSourceDeleted policy would do,
// reading (never writing) the destination and the ownership record.
func (e *Engine) planDeletion(ctx context.Context, k state.PairKey, rs config.RuleSettings, opt Options) (deletionPlan, error) {
	dst := k.DstForge + ":" + k.DstRepo
	if rs.OnSourceDeleted != "archive" && rs.OnSourceDeleted != "delete" {
		return deletionPlan{kind: planKeep}, nil
	}
	dstP, ok := e.provider(opt, k.DstForge)
	if !ok {
		return deletionPlan{}, fmt.Errorf("unknown destination forge %q", k.DstForge)
	}
	plan := deletionPlan{dstP: dstP}
	rec, owned, err := e.Store.OwnershipOf(ctx, k.DstForge, k.DstRepo)
	if err != nil {
		return deletionPlan{}, fmt.Errorf("read ownership of %s: %w", dst, err)
	}
	if !owned {
		plan.kind = planNotOwned
		return plan, nil
	}
	how := rec.How
	owner, name, _ := strings.Cut(k.DstRepo, "/")
	repo, err := dstP.GetRepo(ctx, owner, name)
	gone := errors.Is(err, forge.ErrNotFound)
	if err != nil && !gone {
		return deletionPlan{}, fmt.Errorf("get %s: %w", dst, err)
	}
	if !gone {
		if why := e.identityMismatch(rec, k.DstForge, repo, opt); why != "" {
			// Not the repository gitmirror created or adopted: never
			// archive or delete it.
			plan.kind, plan.note = planNotOwned, why
			return plan, nil
		}
	}
	if rs.OnSourceDeleted == "delete" {
		del, canDelete := dstP.(forge.RepoDeleter)
		switch {
		case how != state.OwnedCreated:
			plan.note = "destination was adopted, not created by gitmirror; archiving instead of deleting"
		case !canDelete:
			plan.note = dstP.Type() + " provider cannot delete repositories; archiving instead"
		default:
			plan.kind, plan.del = planDelete, del
			return plan, nil
		}
	}
	switch {
	case gone:
		plan.kind = planGone
	case repo.Archived:
		plan.kind = planArchived
	default:
		plan.kind = planArchive
	}
	return plan, nil
}

// applyDeletionPlan carries out (or, when dry, describes) a plan other than
// planKeep and planArchived.
func (e *Engine) applyDeletionPlan(ctx context.Context, k state.PairKey, plan deletionPlan, dry bool, act func(string, ...any)) (state.StepStatus, string, error) {
	dst := k.DstForge + ":" + k.DstRepo
	withNote := func(s string) string {
		if plan.note == "" {
			return s
		}
		return plan.note + "; " + s
	}
	owner, name, _ := strings.Cut(k.DstRepo, "/")
	switch plan.kind {
	case planNotOwned:
		act("source deleted; destination %s is not owned by gitmirror; leaving it", dst)
		return state.StepUnchanged, withNote("destination not owned"), nil
	case planGone:
		act("source deleted; destination %s is already gone", dst)
		return state.StepUnchanged, withNote("destination not found"), nil
	case planArchive:
		if plan.note != "" {
			act("source deleted; archive destination %s (%s)", dst, plan.note)
		} else {
			act("source deleted; archive destination %s", dst)
		}
		if dry {
			return state.StepOK, withNote("would archive destination"), nil
		}
		t := true
		if err := plan.dstP.UpdateRepoSettings(ctx, owner, name, forge.Settings{Archived: &t}); err != nil {
			return "", "", fmt.Errorf("archive %s: %w", dst, err)
		}
		e.audit(ctx, e.logger().With("pair", k.String()), k, state.AuditArchive, withNote("source deleted; destination archived"))
		return state.StepOK, withNote("destination archived"), nil
	case planDelete:
		act("source deleted; delete destination %s", dst)
		if dry {
			return state.StepOK, "would delete destination", nil
		}
		if err := plan.del.DeleteRepo(ctx, owner, name); err != nil && !errors.Is(err, forge.ErrNotFound) {
			return "", "", fmt.Errorf("delete %s: %w", dst, err)
		}
		e.audit(ctx, e.logger().With("pair", k.String()), k, state.AuditDelete, "source deleted; destination deleted")
		if err := e.Store.ReleaseOwnership(context.WithoutCancel(ctx), k.DstForge, k.DstRepo); err != nil {
			return "", "", fmt.Errorf("release ownership of %s: %w", dst, err)
		}
		return state.StepOK, "destination deleted", nil
	default:
		return "", "", fmt.Errorf("engine: unexpected deletion plan %d", plan.kind)
	}
}
