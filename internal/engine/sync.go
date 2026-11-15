package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"gitmirror/internal/config"
	"gitmirror/internal/forge"
	"gitmirror/internal/gitx"
	"gitmirror/internal/resolve"
	"gitmirror/internal/state"
)

// Step names recorded in run history.
const (
	StepPrepare  = "prepare"
	StepGit      = "git"
	StepLFS      = "lfs"
	StepSettings = "settings"
	StepReleases = "releases"
	StepWiki     = "wiki"
	// StepFinalize is recorded only when the destination's archive state is
	// changed (or fails to change) at the end of a run.
	StepFinalize = "finalize"
)

// restoreTimeout bounds the final archive call, which runs even after the
// run's context has been cancelled.
const restoreTimeout = 30 * time.Second

// pairRun is the mutable state of one SyncPair call.
type pairRun struct {
	e   *Engine
	p   resolve.Pair
	opt Options
	key state.PairKey
	log *slog.Logger

	srcP, dstP forge.Provider
	src        *forge.Repo // fresh source metadata
	dst        *forge.Repo // destination (planned stand-in in a dry run before creation)
	dstExists  bool        // dst really exists on the forge
	wantPriv   bool

	origArchived bool // destination was archived when the run started
	unarchived   bool // we (or, in a dry run, would) unarchive it
	// markerSet: the pair's "unarchived by gitmirror" marker is set in the
	// store (by this run before unarchiving, or left by an interrupted run).
	markerSet bool

	ownHow state.Ownership // how gitmirror owns the destination (after prepare)

	srcRefs      []gitx.Ref
	srcRefsKnown bool
	gitDone      bool // git step succeeded in a real run
	pushRejected bool // git step failed because the destination rejected refs
	digest       string
	refCount     int

	steps    []state.StepResult
	actions  []string
	aborted  bool
	failed   bool
	err      error
	stepErrs []error
}

func (r *pairRun) dry() bool { return r.opt.DryRun }

func (r *pairRun) includes(f string) bool { return r.p.Settings.Includes(f) }

func (r *pairRun) action(format string, args ...any) {
	r.actions = append(r.actions, fmt.Sprintf(format, args...))
}

func (r *pairRun) record(step string, start time.Time, st state.StepStatus, detail string) {
	d := time.Duration(0)
	if !start.IsZero() {
		d = time.Since(start)
	}
	r.steps = append(r.steps, state.StepResult{Step: step, Status: st, Detail: detail, Duration: d})
	r.log.Debug("engine: step", "step", step, "status", st, "detail", detail)
}

func (r *pairRun) skip(step, reason string) {
	r.record(step, time.Time{}, state.StepSkipped, reason)
}

// fatal marks the run failed (core failure).
func (r *pairRun) fatal(err error) {
	r.failed = true
	if r.err == nil {
		r.err = err
	}
}

// stepFailed records a non-core step failure (run becomes partial).
func (r *pairRun) stepFailed(step string, start time.Time, err error) {
	r.stepErrs = append(r.stepErrs, fmt.Errorf("%s: %w", step, err))
	r.record(step, start, state.StepFailed, err.Error())
	r.log.Warn("engine: step failed", "step", step, "err", err)
}

func (r *pairRun) dstName() string { return r.p.DstForge + ":" + r.p.DstFullName() }

func remote(url string, a forge.Auth) gitx.Remote {
	return gitx.Remote{URL: url, Username: a.Username, Token: a.Token}
}

func (r *pairRun) srcRemote() gitx.Remote {
	return remote(r.srcP.GitURL(r.src.Owner, r.src.Name), r.srcP.GitAuth())
}

func (r *pairRun) dstRemote() gitx.Remote {
	return remote(r.dstP.GitURL(r.p.DstOwner, r.p.DstName), r.dstP.GitAuth())
}

// writes reports whether any included feature writes to the destination.
func (r *pairRun) writes() bool {
	for _, f := range []string{"git", "settings", "releases", "wiki"} {
		if r.includes(f) {
			return true
		}
	}
	return false
}

func (r *pairRun) run(ctx context.Context) {
	if !r.prepare(ctx) {
		return
	}
	ok := true
	if r.includes("git") {
		ok = r.syncGit(ctx)
	} else {
		r.skip(StepGit, "not included")
		if r.includes("lfs") {
			r.skip(StepLFS, "git not included")
		}
	}
	later := []struct {
		name string
		fn   func(context.Context)
	}{
		{StepSettings, r.syncSettings},
		{StepReleases, r.syncReleases},
		{StepWiki, r.syncWiki},
	}
	for _, s := range later {
		switch {
		case !r.includes(s.name):
			r.skip(s.name, "not included")
		case !ok && s.name == StepSettings && r.pushRejected:
			r.healDefaultBranch(ctx)
		case !ok:
			r.skip(s.name, "git step did not complete")
		default:
			s.fn(ctx)
		}
	}
	r.finalize(ctx)
}

// report fills rep from the run state.
func (r *pairRun) report(ctx context.Context, rep *Report) {
	rep.Steps = r.steps
	rep.Actions = r.actions
	switch {
	case r.aborted:
		rep.Status, rep.Err = state.RunAborted, r.err
	case r.failed:
		rep.Status, rep.Err = state.RunFailed, r.err
	case len(r.stepErrs) > 0:
		rep.Status, rep.Err = state.RunPartial, errors.Join(r.stepErrs...)
	default:
		rep.Status = state.RunOK
	}
	if cerr := ctx.Err(); cerr != nil && rep.Status != state.RunOK {
		rep.Status = state.RunAborted
		if rep.Err == nil || !errors.Is(rep.Err, cerr) {
			rep.Err = fmt.Errorf("%w: %v", cerr, rep.Err)
		}
	}
}

// ---- prepare ----

func (r *pairRun) prepare(ctx context.Context) bool {
	start := time.Now()
	fail := func(err error) bool {
		r.fatal(err)
		r.record(StepPrepare, start, state.StepFailed, err.Error())
		return false
	}
	p := r.p
	var ok bool
	if r.srcP, ok = r.e.provider(r.opt, p.SrcForge); !ok {
		return fail(fmt.Errorf("unknown source forge %q", p.SrcForge))
	}
	if r.dstP, ok = r.e.provider(r.opt, p.DstForge); !ok {
		return fail(fmt.Errorf("unknown destination forge %q", p.DstForge))
	}
	src, err := r.srcP.GetRepo(ctx, p.Src.Owner, p.Src.Name)
	if errors.Is(err, forge.ErrNotFound) {
		return fail(fmt.Errorf("source repository not found: %s:%s", p.SrcForge, p.Src.FullName()))
	}
	if err != nil {
		return fail(fmt.Errorf("get source %s:%s: %w", p.SrcForge, p.Src.FullName(), err))
	}
	r.src = src
	// The source exists, so any earlier not-found was transient: restart the
	// source-deletion grace period.
	if !r.dry() {
		if err := r.e.Store.ClearMissing(ctx, r.key); err != nil {
			r.log.Warn("engine: clear missing record", "err", err)
		}
	}
	// Only "match-source" can produce a public destination, and only for a
	// public source; anything else is private.
	r.wantPriv = p.Settings.Visibility != "match-source" || src.Private
	vis := "public"
	if r.wantPriv {
		vis = "private"
	}

	// A previous run that unarchived the destination and died before
	// restoring it leaves the marker set.
	prev, _, err := r.e.Store.Pair(ctx, r.key)
	if err != nil {
		return fail(fmt.Errorf("read pair state: %w", err))
	}
	r.markerSet = prev.Unarchived
	forgeURL := r.e.forgeURL(r.opt, p.DstForge)

	var detail string
	dst, err := r.dstP.GetRepo(ctx, p.DstOwner, p.DstName)
	switch {
	case errors.Is(err, forge.ErrNotFound):
		r.ownHow = state.OwnedCreated
		if r.markerSet {
			// The repository the marker was about is gone.
			r.clearMarker(ctx)
		}
		r.action("create repository %s (%s)", r.dstName(), vis)
		if r.dry() {
			r.dst = &forge.Repo{Owner: p.DstOwner, Name: p.DstName, Description: src.Description, Private: r.wantPriv}
			detail = "destination would be created"
			break
		}
		if op, ok := r.dstP.(forge.OrgProvider); ok {
			if err := op.EnsureOrg(ctx, p.DstOwner); err != nil {
				if errors.Is(err, forge.ErrUnsupported) {
					return fail(err)
				}
				return fail(fmt.Errorf("ensure destination owner %s: %w", p.DstOwner, err))
			}
		}
		created, err := r.dstP.CreateRepo(ctx, forge.CreateOptions{
			Owner: p.DstOwner, Name: p.DstName, Description: src.Description, Private: r.wantPriv,
		})
		if err != nil {
			return fail(fmt.Errorf("create destination %s: %w", r.dstName(), err))
		}
		if err := r.e.Store.MarkOwnedAs(context.WithoutCancel(ctx), p.DstForge, p.DstFullName(), state.OwnedCreated, created.ID, forgeURL); err != nil {
			return fail(fmt.Errorf("record ownership of %s: %w", r.dstName(), err))
		}
		r.e.audit(ctx, r.log, r.key, state.AuditCreate, fmt.Sprintf("created %s repository (trigger %s)", vis, r.opt.trigger()))
		r.dst, r.dstExists = created, true
		detail = "destination created"
	case err != nil:
		return fail(fmt.Errorf("get destination %s: %w", r.dstName(), err))
	default:
		rec, owned, err := r.e.Store.OwnershipOf(ctx, p.DstForge, p.DstFullName())
		if err != nil {
			return fail(fmt.Errorf("read ownership of %s: %w", r.dstName(), err))
		}
		mismatch := ""
		if owned {
			mismatch = r.e.identityMismatch(rec, p.DstForge, dst, r.opt)
		}
		switch {
		case (!owned || mismatch != "") && !p.Settings.Adopt:
			if mismatch != "" {
				return fail(fmt.Errorf("destination %s is not the repository gitmirror %s: %s; "+
					"refusing to write to it. Set adopt: true on rule %q to mirror into it anyway", r.dstName(), rec.How, mismatch, p.Rule))
			}
			return fail(fmt.Errorf("destination %s already exists and was not created by gitmirror; "+
				"refusing to write to it. Set adopt: true on rule %q to mirror into it", r.dstName(), p.Rule))
		case !owned || mismatch != "":
			r.action("adopt existing repository %s", r.dstName())
			if !r.dry() {
				if err := r.e.Store.MarkOwnedAs(context.WithoutCancel(ctx), p.DstForge, p.DstFullName(), state.OwnedAdopted, dst.ID, forgeURL); err != nil {
					return fail(fmt.Errorf("record ownership of %s: %w", r.dstName(), err))
				}
				what := "adopted existing repository"
				if mismatch != "" {
					what += " (replaced: " + mismatch + ")"
				}
				r.e.audit(ctx, r.log, r.key, state.AuditAdopt, fmt.Sprintf("%s (trigger %s)", what, r.opt.trigger()))
			}
			r.ownHow = state.OwnedAdopted
			detail = "destination adopted"
		default:
			// Ownership rows from before identities were recorded are
			// completed from the current destination once (trust on first use).
			if !r.dry() && (rec.RepoID == 0 && dst.ID != 0 || rec.ForgeURL == "" && forgeURL != "") {
				if err := r.e.Store.BackfillOwnership(context.WithoutCancel(ctx), p.DstForge, p.DstFullName(), dst.ID, forgeURL); err != nil {
					return fail(fmt.Errorf("record identity of %s: %w", r.dstName(), err))
				}
				r.log.Info("engine: recorded destination identity", "repo_id", dst.ID, "forge_url", forgeURL)
			}
			r.ownHow = rec.How
			detail = "destination exists (" + string(rec.How) + ")"
		}
		r.dst, r.dstExists = dst, true
	}

	r.origArchived = r.dst.Archived
	if r.markerSet && r.dstExists && !r.dst.Archived {
		// An interrupted run unarchived it: its original state is archived.
		r.origArchived, r.unarchived = true, true
		r.log.Warn("engine: destination was left unarchived by an interrupted run; it will be re-archived")
		detail += "; left unarchived by an interrupted run"
	}
	if r.dst.Archived && r.writes() {
		r.action("unarchive %s for the sync", r.dstName())
		if !r.dry() {
			if !r.markerSet {
				// Persist the intent first, so a crash before re-archiving
				// is repaired by the next run.
				if err := r.e.Store.SetUnarchived(context.WithoutCancel(ctx), r.key, true); err != nil {
					return fail(fmt.Errorf("record unarchive of %s: %w", r.dstName(), err))
				}
				r.markerSet = true
			}
			f := false
			if err := r.dstP.UpdateRepoSettings(ctx, p.DstOwner, p.DstName, forge.Settings{Archived: &f}); err != nil {
				return fail(fmt.Errorf("unarchive %s: %w", r.dstName(), err))
			}
		}
		r.unarchived = true
		r.dst.Archived = false
		detail += "; unarchived"
	}
	r.record(StepPrepare, start, state.StepOK, detail)
	return true
}

// ---- git ----

func (r *pairRun) syncGit(ctx context.Context) bool {
	start := time.Now()
	lfsWanted := r.includes("lfs")
	lfsOK := lfsWanted && r.e.lfsAvailable(ctx)
	recordLFS := func(st state.StepStatus, detail string) {
		if lfsWanted {
			r.record(StepLFS, time.Time{}, st, detail)
		}
	}
	lfsMissing := func() {
		r.log.Warn("engine: lfs included but git-lfs is not installed; mirroring git only")
		recordLFS(state.StepSkipped, "git-lfs not installed")
	}
	gitFail := func(err error) bool {
		r.pushRejected = errors.Is(err, gitx.ErrPushRejected)
		r.fatal(fmt.Errorf("git: %w", err))
		r.record(StepGit, start, state.StepFailed, err.Error())
		recordLFS(state.StepSkipped, "git step did not complete")
		return false
	}

	src := r.srcRemote()
	if r.dry() && !r.dstExists {
		refs, err := r.e.git().LsRemote(ctx, src)
		if err != nil {
			return gitFail(err)
		}
		r.srcRefs, r.srcRefsKnown = refs, true
		if len(refs) == 0 {
			r.action("source is empty; nothing to push")
			r.record(StepGit, start, state.StepUnchanged, "source is empty; nothing to push")
		} else {
			r.action("push all %d refs", len(refs))
			r.record(StepGit, start, state.StepOK, fmt.Sprintf("would push %d refs", len(refs)))
		}
		if lfsWanted && !lfsOK {
			lfsMissing()
		} else {
			recordLFS(state.StepSkipped, "dry-run")
		}
		return true
	}

	prev, _, err := r.e.Store.Pair(ctx, r.key)
	if err != nil {
		return gitFail(fmt.Errorf("read pair state: %w", err))
	}
	cache := &gitx.Cache{Dir: r.e.cacheDir(r.key, false), R: r.e.git(), Logger: r.e.logger()}
	var bypass string
	res, err := cache.Sync(ctx, src, r.dstRemote(), gitx.SyncOptions{
		Prune:            true,
		LFS:              lfsOK,
		DryRun:           r.dry(),
		AllowEmptySource: prev.LastRefCount == 0,
		// Forges like Gitea make the first pushed branch the default of an
		// empty repository: push the source's default first.
		FirstBranch: r.src.DefaultBranch,
		CheckDeletions: func(deleted []string, dstTotal int) error {
			md := r.massDeletion(len(deleted), dstTotal)
			if md == nil {
				return nil
			}
			if r.opt.AllowPrune {
				bypass = fmt.Sprintf("deletion guard bypassed (--allow-prune): %d of %d refs", md.n, md.total)
				r.log.Warn("engine: mass ref deletion allowed by --allow-prune", "deleted", md.n, "destination_refs", md.total)
				return nil
			}
			return md
		},
		// LFS objects are re-sent when the refs they were last copied for
		// differ from the source's, even if no ref changed (heals refs that
		// were pushed without LFS).
		LFSDigest: prev.LFSDigest,
	})
	if md := (*massDeletionError)(nil); errors.As(err, &md) {
		r.aborted = true
		r.err = md
		r.action("refuse to sync: %v", md)
		r.record(StepGit, start, state.StepFailed, md.Error())
		recordLFS(state.StepSkipped, "git step did not complete")
		r.log.Warn("engine: mass ref deletion guard tripped", "deleted", md.n, "destination_refs", md.total)
		return false
	}
	if errors.Is(err, gitx.ErrEmptySource) {
		r.aborted = true
		r.err = fmt.Errorf("source has no refs but previously had %d; refusing to prune destination", prev.LastRefCount)
		r.action("refuse to sync: %v", r.err)
		r.record(StepGit, start, state.StepFailed, r.err.Error())
		recordLFS(state.StepSkipped, "git step did not complete")
		r.log.Warn("engine: empty-source guard tripped", "previous_refs", prev.LastRefCount)
		return false
	}
	if err != nil {
		return gitFail(err)
	}
	r.srcRefs, r.srcRefsKnown = res.SrcRefs, true
	counts := fmt.Sprintf("%d created, %d updated, %d deleted", len(res.Created), len(res.Updated), len(res.Deleted))
	if bypass != "" {
		counts += "; " + bypass
	}
	digest := gitx.Digest(res.SrcRefs)
	switch {
	case res.Changed:
		r.action("push %s refs", counts)
		r.record(StepGit, start, state.StepOK, counts)
	case len(res.SrcRefs) == 0:
		r.record(StepGit, start, state.StepUnchanged, "source is empty; nothing to push")
	default:
		r.record(StepGit, start, state.StepUnchanged, fmt.Sprintf("%d refs up to date", len(res.SrcRefs)))
	}
	switch {
	case !lfsWanted:
	case !lfsOK:
		lfsMissing()
	case r.dry():
		if digest != "" && (res.Changed || digest != prev.LFSDigest) {
			r.action("transfer LFS objects")
		}
		recordLFS(state.StepSkipped, "dry-run")
	case res.LFSObjects >= 0:
		detail := fmt.Sprintf("%d objects in cache", res.LFSObjects)
		if !res.Changed {
			detail += "; refs unchanged, LFS objects not yet copied for them"
		}
		recordLFS(state.StepOK, detail)
		if digest != prev.LFSDigest {
			if err := r.e.Store.SetLFSDigest(context.WithoutCancel(ctx), r.key, digest); err != nil {
				r.log.Warn("engine: record LFS digest", "err", err)
			}
		}
	default:
		recordLFS(state.StepUnchanged, "no transfer needed")
	}
	if !r.dry() {
		r.gitDone = true
		r.digest = digest
		r.refCount = len(res.SrcRefs)
	}
	return true
}

// massDeletionError is returned by the deletion check when a push would
// delete too many destination refs.
type massDeletionError struct {
	n, total, maxRefs, maxPercent int
}

func (e *massDeletionError) Error() string {
	return fmt.Sprintf("sync would delete %d of %d destination refs (guard: more than %d refs and more than %d%%); "+
		"refusing to push. If the source really dropped them, run `gitmirror sync --allow-prune` once",
		e.n, e.total, e.maxRefs, e.maxPercent)
}

// massDeletion returns a *massDeletionError when deleting n of the
// destination's total refs exceeds both of the rule's limits, else nil.
func (r *pairRun) massDeletion(n, total int) *massDeletionError {
	maxRefs, maxPct := r.p.Settings.MaxRefDeletions, r.p.Settings.MaxRefDeletionsPercent
	if maxPct == 0 {
		maxRefs, maxPct = config.DefaultMaxRefDeletions, config.DefaultMaxRefDeletionsPercent
	}
	if n <= maxRefs || n*100 <= maxPct*total {
		return nil
	}
	return &massDeletionError{n: n, total: total, maxRefs: maxRefs, maxPercent: maxPct}
}

// ---- settings ----

func normTopics(ts []string) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		t = strings.ToLower(strings.TrimSpace(t))
		if t != "" && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	slices.Sort(out)
	return out
}

// sourceHasBranch reports whether refs/heads/<b> exists at the source.
func (r *pairRun) sourceHasBranch(ctx context.Context, b string) (bool, error) {
	if !r.srcRefsKnown {
		refs, err := r.e.git().LsRemote(ctx, r.srcRemote())
		if err != nil {
			return false, err
		}
		r.srcRefs, r.srcRefsKnown = refs, true
	}
	for _, ref := range r.srcRefs {
		if ref.Name == "refs/heads/"+b {
			return true, nil
		}
	}
	return false, nil
}

// defaultBranchChange returns the default branch to set on the destination
// cur (nil when it already matches or cannot be applied) and, when the
// source's default cannot be applied, a note saying why.
func (r *pairRun) defaultBranchChange(ctx context.Context, cur *forge.Repo) (*string, string) {
	b := r.src.DefaultBranch
	if b == "" || b == cur.DefaultBranch {
		return nil, ""
	}
	has, err := r.sourceHasBranch(ctx, b)
	switch {
	case err != nil:
		return nil, fmt.Sprintf("default branch %q left unchanged: %v", b, err)
	case !has:
		return nil, fmt.Sprintf("default branch %q does not exist in the source refs; left unchanged", b)
	}
	return &b, ""
}

// healDefaultBranch runs in place of the settings step when the destination
// rejected the git push. A wrong destination default branch can itself be
// the cause: Gitea makes the first pushed branch the default of an empty
// repository and refuses to delete the default branch, so once that branch
// is deleted at the source every (atomic) push is rejected. Only the default
// branch is corrected, against fresh destination metadata; the other
// settings wait for a successful git step.
func (r *pairRun) healDefaultBranch(ctx context.Context) {
	start := time.Now()
	cur, err := r.dstP.GetRepo(ctx, r.p.DstOwner, r.p.DstName)
	if err != nil {
		r.stepFailed(StepSettings, start, fmt.Errorf("get destination: %w", err))
		return
	}
	v, note := r.defaultBranchChange(ctx, cur)
	if v == nil {
		detail := "git step did not complete"
		if note != "" {
			detail += "; " + note
		}
		r.record(StepSettings, start, state.StepSkipped, detail)
		return
	}
	r.action("update settings of %s: default branch", r.dstName())
	if err := r.dstP.UpdateRepoSettings(ctx, r.p.DstOwner, r.p.DstName, forge.Settings{DefaultBranch: v}); err != nil {
		r.stepFailed(StepSettings, start, err)
		return
	}
	r.record(StepSettings, start, state.StepOK, fmt.Sprintf("updated default branch to %q only (git step failed; other settings skipped)", *v))
}

func (r *pairRun) syncSettings(ctx context.Context) {
	start := time.Now()
	cur := r.dst
	if r.dstExists && !r.dry() {
		fresh, err := r.dstP.GetRepo(ctx, r.p.DstOwner, r.p.DstName)
		if err != nil {
			r.stepFailed(StepSettings, start, fmt.Errorf("get destination: %w", err))
			return
		}
		cur = fresh
	}
	src := r.src
	var s forge.Settings
	var changed, notes []string
	if src.Description != cur.Description {
		v := src.Description
		s.Description = &v
		changed = append(changed, "description")
	}
	if src.Homepage != cur.Homepage {
		v := src.Homepage
		s.Homepage = &v
		changed = append(changed, "homepage")
	}
	if want := normTopics(src.Topics); !slices.Equal(want, normTopics(cur.Topics)) {
		s.Topics = &want
		changed = append(changed, "topics")
	}
	if v, note := r.defaultBranchChange(ctx, cur); v != nil {
		s.DefaultBranch = v
		changed = append(changed, "default branch")
	} else if note != "" {
		notes = append(notes, note)
	}
	switch {
	case r.wantPriv == cur.Private:
	case !r.wantPriv && r.ownHow == state.OwnedAdopted && !r.p.Settings.AllowPublish:
		// Never widen the visibility of a repository gitmirror did not
		// create unless the rule explicitly allows it.
		notes = append(notes, "visibility left private: the destination was adopted (set allow_publish: true on the rule to make it public)")
	default:
		v := r.wantPriv
		s.Private = &v
		changed = append(changed, "visibility")
	}
	if src.Archived && !cur.Archived {
		notes = append(notes, "archiving is applied after all other steps")
	}
	if len(changed) == 0 {
		r.record(StepSettings, start, state.StepUnchanged, strings.Join(notes, "; "))
		return
	}
	r.action("update settings of %s: %s", r.dstName(), strings.Join(changed, ", "))
	if !r.dry() {
		if err := r.dstP.UpdateRepoSettings(ctx, r.p.DstOwner, r.p.DstName, s); err != nil {
			r.stepFailed(StepSettings, start, err)
			return
		}
	}
	detail := "updated " + strings.Join(changed, ", ")
	if len(notes) > 0 {
		detail += "; " + strings.Join(notes, "; ")
	}
	r.record(StepSettings, start, state.StepOK, detail)
}

// ---- wiki ----

func (r *pairRun) syncWiki(ctx context.Context) {
	start := time.Now()
	if !r.src.HasWiki {
		r.skip(StepWiki, "source has no wiki")
		return
	}
	sw, ok := r.srcP.(forge.WikiProvider)
	if !ok {
		r.skip(StepWiki, "unsupported by "+r.srcP.Type())
		return
	}
	dw, ok := r.dstP.(forge.WikiProvider)
	if !ok {
		r.skip(StepWiki, "unsupported by "+r.dstP.Type())
		return
	}
	// Some forges (Gitea) refuse git access to a wiki that was never
	// created; initialise it first (never in a dry run).
	wi, canInit := r.dstP.(forge.WikiInitializer)
	if r.dry() && !r.dstExists {
		if canInit {
			r.action("initialise destination wiki")
		}
		r.action("mirror wiki")
		r.skip(StepWiki, "destination does not exist yet (dry-run)")
		return
	}
	if canInit && !r.dry() {
		if err := wi.EnsureWiki(ctx, r.p.DstOwner, r.p.DstName); err != nil {
			r.stepFailed(StepWiki, start, fmt.Errorf("initialise destination wiki: %w", err))
			return
		}
	}
	src := remote(sw.WikiGitURL(r.src.Owner, r.src.Name), r.srcP.GitAuth())
	dst := remote(dw.WikiGitURL(r.p.DstOwner, r.p.DstName), r.dstP.GitAuth())
	cache := &gitx.Cache{Dir: r.e.cacheDir(r.key, true), R: r.e.git(), Logger: r.e.logger()}
	res, err := cache.Sync(ctx, src, dst, gitx.SyncOptions{Prune: true, DryRun: r.dry()})
	if errors.Is(err, gitx.ErrEmptySource) {
		r.record(StepWiki, start, state.StepSkipped, "source wiki is empty")
		return
	}
	if err != nil {
		if errors.Is(err, gitx.ErrDestination) {
			if r.dry() && canInit {
				// Probably not initialised yet; a real run would do that first.
				r.action("initialise destination wiki")
				r.action("mirror wiki")
				r.record(StepWiki, start, state.StepSkipped, fmt.Sprintf("destination wiki not readable; a real run initialises it first (dry-run): %v", err))
				return
			}
			if !canInit {
				err = fmt.Errorf("%w (the destination wiki may not exist yet: create one page in the destination's web UI, then retry)", err)
			}
		}
		r.stepFailed(StepWiki, start, err)
		return
	}
	if !res.Changed {
		r.record(StepWiki, start, state.StepUnchanged, fmt.Sprintf("%d refs up to date", len(res.SrcRefs)))
		return
	}
	counts := fmt.Sprintf("%d created, %d updated, %d deleted", len(res.Created), len(res.Updated), len(res.Deleted))
	r.action("push wiki %s refs", counts)
	r.record(StepWiki, start, state.StepOK, counts)
}

// ---- finalize ----

// finalize applies the destination's final archive state: archived when the
// source is archived (settings included), or restored to archived when we
// unarchived it and settings are not mirrored.
func (r *pairRun) finalize(ctx context.Context) {
	final := r.origArchived
	if r.includes("settings") {
		final = r.src.Archived
	}
	if !final || r.dst.Archived {
		r.clearMarker(ctx)
		return
	}
	start := time.Now()
	verb := "archive"
	if r.unarchived && r.origArchived {
		verb = "re-archive"
	}
	r.action("%s %s", verb, r.dstName())
	if r.dry() {
		r.record(StepFinalize, start, state.StepOK, "would "+verb+" destination")
		return
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), restoreTimeout)
	defer cancel()
	t := true
	if err := r.dstP.UpdateRepoSettings(rctx, r.p.DstOwner, r.p.DstName, forge.Settings{Archived: &t}); err != nil {
		r.stepFailed(StepFinalize, start, fmt.Errorf("%s destination: %w", verb, err))
		return
	}
	r.dst.Archived = true
	r.clearMarker(ctx)
	if verb == "archive" {
		// Re-archiving a destination unarchived only for this sync restores
		// its state and is not audited.
		r.e.audit(ctx, r.log, r.key, state.AuditArchive, fmt.Sprintf("source archived (trigger %s)", r.opt.trigger()))
	}
	r.record(StepFinalize, start, state.StepOK, verb+"d destination")
}

// clearMarker clears the pair's "unarchived by gitmirror" marker once the
// destination's archived state is final (never in a dry run).
func (r *pairRun) clearMarker(ctx context.Context) {
	if !r.markerSet || r.dry() {
		return
	}
	if err := r.e.Store.SetUnarchived(context.WithoutCancel(ctx), r.key, false); err != nil {
		r.log.Warn("engine: clear unarchived marker", "err", err)
		return
	}
	r.markerSet = false
}
