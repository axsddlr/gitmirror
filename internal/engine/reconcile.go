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
// complete, and the pair is neither in current nor in dropped. Even then
// only a positive "not found" from the source forge counts: if the source
// exists (it was merely filtered out) its missing record is cleared, and any
// other error leaves the pair untouched.
//
// Forges answer "not found" for repositories the token merely lost access
// to, so the policy is applied only once the source has been continuously
// missing for the rule's DeletionGrace, measured from the first not-found
// (Store.MarkMissing). Until then the pair gets a report saying when the
// policy would apply, and no run is recorded.
//
// Policies: "keep" does nothing; "archive" archives a destination gitmirror
// owns; "delete" deletes a destination gitmirror created, when the provider
// can delete repositories, and archives it otherwise (including adopted
// destinations). A deleted pair is removed from the state store. Applying a
// policy is recorded as one run with trigger "<trigger>:reconcile"; "keep"
// and an already archived destination record nothing, and an already
// archived destination produces no Report. A dry run writes nothing to any
// forge or to the state store.
func (e *Engine) Reconcile(ctx context.Context, current []resolve.Pair, complete map[string]bool, dropped []resolve.Pair, settings map[string]config.RuleSettings, opt Options) []Report {
	log := e.logger()
	states, err := e.Store.Pairs(ctx)
	if err != nil {
		log.Error("engine: reconcile: list pairs", "err", err)
		return nil
	}
	skip := map[string]bool{}
	for _, p := range current {
		skip[p.Key().String()] = true
	}
	for _, p := range dropped {
		skip[p.Key().String()] = true
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
		srcP, ok := e.Providers[k.SrcForge]
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
		if rep, ok := e.reconcileOne(ctx, k, rs, opt); ok {
			reps = append(reps, rep)
		}
	}
	return reps
}

func (e *Engine) now() time.Time {
	if e.Now == nil {
		return time.Now()
	}
	return e.Now()
}

// reconcileOne handles a pair whose source was reported as not found. ok is
// false when there is nothing to report.
func (e *Engine) reconcileOne(ctx context.Context, k state.PairKey, rs config.RuleSettings, opt Options) (rep Report, ok bool) {
	rep.Key = k
	unlock, err := e.lock(ctx, k.String())
	if err != nil {
		rep.Status, rep.Err = state.RunAborted, err
		return rep, true
	}
	defer unlock()
	log := e.logger().With("pair", k.String())
	act := func(format string, args ...any) {
		rep.Actions = append(rep.Actions, fmt.Sprintf(format, args...))
	}
	start := time.Now()
	noRun := func(status state.StepStatus, detail string) (Report, bool) {
		rep.Status = state.RunOK
		rep.Steps = []state.StepResult{{Step: StepReconcile, Status: status, Detail: detail, Duration: time.Since(start)}}
		return rep, true
	}

	// Grace period: nothing destructive until the source has been missing
	// for DeletionGrace since it was first seen missing.
	now := e.now()
	firstSeen := now
	if opt.DryRun {
		since, found, err := e.Store.MissingSince(ctx, k)
		if err != nil {
			rep.Status, rep.Err = state.RunFailed, fmt.Errorf("engine: read missing record: %w", err)
			return rep, true
		}
		if found {
			firstSeen = since
		}
	} else if firstSeen, err = e.Store.MarkMissing(ctx, k); err != nil {
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

	plan, perr := e.safePlanDeletion(ctx, k, rs, log)
	if perr == nil {
		switch plan.kind {
		case planKeep:
			act("source deleted; keeping destination")
			return noRun(state.StepUnchanged, "source deleted; keeping destination")
		case planArchived:
			log.Debug("engine: reconcile: destination already archived")
			return rep, false
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

// deletionPlan is what applying OnSourceDeleted to a pair would do.
type deletionPlan struct {
	kind planKind
	note string // why archiving replaces deleting, if it does
	dstP forge.Provider
	del  forge.RepoDeleter
}

// safePlanDeletion is planDeletion with a panic turned into an error.
func (e *Engine) safePlanDeletion(ctx context.Context, k state.PairKey, rs config.RuleSettings, log *slog.Logger) (plan deletionPlan, err error) {
	defer func() {
		if v := recover(); v != nil {
			log.Error("engine: panic during reconcile", "panic", v, "stack", string(debug.Stack()))
			plan, err = deletionPlan{}, fmt.Errorf("engine: panic: %v", v)
		}
	}()
	return e.planDeletion(ctx, k, rs)
}

// planDeletion decides what the pair's OnSourceDeleted policy would do,
// reading (never writing) the destination and the ownership record.
func (e *Engine) planDeletion(ctx context.Context, k state.PairKey, rs config.RuleSettings) (deletionPlan, error) {
	dst := k.DstForge + ":" + k.DstRepo
	if rs.OnSourceDeleted != "archive" && rs.OnSourceDeleted != "delete" {
		return deletionPlan{kind: planKeep}, nil
	}
	dstP, ok := e.Providers[k.DstForge]
	if !ok {
		return deletionPlan{}, fmt.Errorf("unknown destination forge %q", k.DstForge)
	}
	plan := deletionPlan{dstP: dstP}
	how, owned, err := e.Store.Owned(ctx, k.DstForge, k.DstRepo)
	if err != nil {
		return deletionPlan{}, fmt.Errorf("read ownership of %s: %w", dst, err)
	}
	if !owned {
		plan.kind = planNotOwned
		return plan, nil
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
	owner, name, _ := strings.Cut(k.DstRepo, "/")
	repo, err := dstP.GetRepo(ctx, owner, name)
	switch {
	case errors.Is(err, forge.ErrNotFound):
		plan.kind = planGone
	case err != nil:
		return deletionPlan{}, fmt.Errorf("get %s: %w", dst, err)
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
		return state.StepOK, withNote("destination archived"), nil
	case planDelete:
		act("source deleted; delete destination %s", dst)
		if dry {
			return state.StepOK, "would delete destination", nil
		}
		if err := plan.del.DeleteRepo(ctx, owner, name); err != nil && !errors.Is(err, forge.ErrNotFound) {
			return "", "", fmt.Errorf("delete %s: %w", dst, err)
		}
		if err := e.Store.ReleaseOwnership(context.WithoutCancel(ctx), k.DstForge, k.DstRepo); err != nil {
			return "", "", fmt.Errorf("release ownership of %s: %w", dst, err)
		}
		return state.StepOK, "destination deleted", nil
	default:
		return "", "", fmt.Errorf("engine: unexpected deletion plan %d", plan.kind)
	}
}
