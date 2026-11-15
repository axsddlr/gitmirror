// Package engine mirrors one source repository onto one destination
// repository per pair: git refs (and LFS objects), repository settings,
// releases with their assets, and the wiki.
//
// The engine is where gitmirror's data-loss safeguards live: it never writes
// to a destination repository it did not create unless the rule allows
// adoption, it refuses to prune a destination when a source that used to
// have refs suddenly has none, it only deletes releases (and repositories)
// it created itself, and a dry run performs no writes to any forge or to the
// state store.
package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"gitmirror/internal/forge"
	"gitmirror/internal/gitx"
	"gitmirror/internal/resolve"
	"gitmirror/internal/state"
)

// DefaultMaxAssetSize is the release asset size limit used when
// Engine.MaxAssetSize is zero (2 GiB).
const DefaultMaxAssetSize int64 = 2 << 30

// DefaultAssetIdleTimeout is the asset download idle timeout used when
// Engine.AssetIdleTimeout is zero.
const DefaultAssetIdleTimeout = 2 * time.Minute

// Engine synchronises mirror pairs. Its exported fields are configuration
// and must not change while it is in use; the zero value of the unexported
// state is ready to use.
type Engine struct {
	// Providers maps config forge names to providers.
	Providers map[string]forge.Provider
	// Store persists ownership, pair state, run history and release mappings.
	Store *state.Store
	// Git runs git; nil means a zero gitx.Runner.
	Git *gitx.Runner
	// CacheDir is the root directory for the bare cache repositories.
	CacheDir string
	// Logger receives progress and warnings; nil discards them.
	Logger *slog.Logger
	// Concurrency is the Run worker pool size; values below 1 mean 1.
	Concurrency int
	// MaxAssetSize is the largest source release asset copied; larger
	// assets are skipped with a step detail. 0 means DefaultMaxAssetSize.
	MaxAssetSize int64
	// AssetIdleTimeout aborts a release asset copy when the source download
	// delivers no data for this long. 0 means DefaultAssetIdleTimeout.
	AssetIdleTimeout time.Duration
	// ForgeURLs maps config forge names to their normalised base URLs. The
	// destination's URL is recorded with its ownership; a destination whose
	// forge URL changed is no longer treated as owned. A missing entry
	// disables that check for the forge.
	ForgeURLs map[string]string
	// Now is the clock used for the source-deletion grace period; nil means
	// time.Now. Tests set it together with Store.SetClock.
	Now func() time.Time

	lfsOnce sync.Once
	lfsOK   bool

	locksMu sync.Mutex
	locks   map[string]chan struct{}
}

// Report is the outcome of syncing (or reconciling) one pair.
type Report struct {
	// Key identifies the pair.
	Key state.PairKey
	// RunID is the recorded run id; 0 in a dry run or when no run was recorded.
	RunID int64
	// Status is the final run status.
	Status state.RunStatus
	// Steps are the per-step results in execution order.
	Steps []state.StepResult
	// Err is the primary error for failed/aborted runs, or the joined step
	// errors for partial runs; nil for ok.
	Err error
	// Actions are human-readable lines describing what was (or, in a dry
	// run, would be) done.
	Actions []string
}

// Options controls a sync.
type Options struct {
	// DryRun computes and reports the plan without writing to any forge or
	// to the state store.
	DryRun bool
	// Trigger records what started the run: "cli", "schedule" or "webhook".
	// Empty means "cli".
	Trigger string
	// AllowPrune bypasses the mass ref deletion guard (the rule's
	// MaxRefDeletions / MaxRefDeletionsPercent) for this run. The bypass is
	// logged and recorded in the git step detail.
	AllowPrune bool
	// Providers and ForgeURLs, when non-nil, replace Engine.Providers and
	// Engine.ForgeURLs for this call, so a caller whose configuration can
	// change at runtime gives every run one consistent snapshot.
	Providers map[string]forge.Provider
	ForgeURLs map[string]string
}

func (o Options) trigger() string {
	if o.Trigger == "" {
		return "cli"
	}
	return o.Trigger
}

// provider returns the provider of forge name for a call with opt.
func (e *Engine) provider(opt Options, name string) (forge.Provider, bool) {
	provs := e.Providers
	if opt.Providers != nil {
		provs = opt.Providers
	}
	p, ok := provs[name]
	return p, ok
}

// forgeURL returns the normalised base URL of forge name for a call with opt.
func (e *Engine) forgeURL(opt Options, name string) string {
	if opt.ForgeURLs != nil {
		return opt.ForgeURLs[name]
	}
	return e.ForgeURLs[name]
}

func (e *Engine) logger() *slog.Logger {
	if e.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return e.Logger
}

func (e *Engine) git() *gitx.Runner {
	if e.Git == nil {
		return &gitx.Runner{}
	}
	return e.Git
}

func (e *Engine) maxAssetSize() int64 {
	if e.MaxAssetSize <= 0 {
		return DefaultMaxAssetSize
	}
	return e.MaxAssetSize
}

// identityMismatch explains why the destination cur is not the repository
// the ownership record rec was written for: a different forge repository id
// (deleted and re-created, or restored from a backup) or a different forge
// base URL (forges.<name>.url now points at another server). It returns ""
// when they match; unknown values (0 / "") on either side are not compared.
func (e *Engine) identityMismatch(rec state.OwnershipRecord, dstForge string, cur *forge.Repo, opt Options) string {
	var why []string
	if rec.RepoID != 0 && cur.ID != 0 && rec.RepoID != cur.ID {
		why = append(why, fmt.Sprintf("its repository id is %d but gitmirror recorded %d, so it was replaced "+
			"(deleted and re-created, or restored from a backup)", cur.ID, rec.RepoID))
	}
	if url := e.forgeURL(opt, dstForge); rec.ForgeURL != "" && url != "" && rec.ForgeURL != url {
		why = append(why, fmt.Sprintf("the forge URL of %q changed from %s to %s", dstForge, rec.ForgeURL, url))
	}
	return strings.Join(why, "; ")
}

// lfsAvailable reports (once per Engine) whether git-lfs can be run.
func (e *Engine) lfsAvailable(ctx context.Context) bool {
	e.lfsOnce.Do(func() {
		v, err := e.git().Check(context.WithoutCancel(ctx))
		e.lfsOK = err == nil && v.LFS != ""
	})
	return e.lfsOK
}

// cacheDir returns the bare cache path for a pair (or its wiki).
func (e *Engine) cacheDir(k state.PairKey, wiki bool) string {
	sum := sha256.Sum256([]byte(k.String()))
	name := hex.EncodeToString(sum[:])[:16]
	if wiki {
		name += ".wiki.git"
	} else {
		name += ".git"
	}
	return filepath.Join(e.CacheDir, name)
}

// lock serialises work on one pair key inside the process.
func (e *Engine) lock(ctx context.Context, key string) (func(), error) {
	e.locksMu.Lock()
	if e.locks == nil {
		e.locks = map[string]chan struct{}{}
	}
	ch, ok := e.locks[key]
	if !ok {
		ch = make(chan struct{}, 1)
		e.locks[key] = ch
	}
	e.locksMu.Unlock()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Run syncs pairs with a pool of Concurrency workers and returns one report
// per pair, in input order. A panic while syncing one pair is recovered into
// that pair's report (status failed) and does not affect the others. Pairs
// not yet started when ctx is cancelled are reported as aborted.
func (e *Engine) Run(ctx context.Context, pairs []resolve.Pair, opt Options) []Report {
	reps := make([]Report, len(pairs))
	n := e.Concurrency
	if n < 1 {
		n = 1
	}
	if n > len(pairs) {
		n = len(pairs)
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			for i := range jobs {
				reps[i] = e.safeSync(ctx, pairs[i], opt)
			}
		})
	}
	for i := range pairs {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return reps
}

func (e *Engine) safeSync(ctx context.Context, p resolve.Pair, opt Options) (rep Report) {
	defer func() {
		if v := recover(); v != nil {
			e.logger().Error("engine: panic syncing pair", "pair", p.Key().String(), "panic", v, "stack", string(debug.Stack()))
			rep = Report{Key: p.Key(), Status: state.RunFailed, Err: fmt.Errorf("engine: panic: %v", v)}
		}
	}()
	return e.SyncPair(ctx, p, opt)
}

// SyncPair makes the pair's destination match its source. Steps run in the
// order prepare, git (+lfs), settings, releases, wiki, then archive state
// is finalised. A real run is recorded in the state store and always
// finished, even on panic (which yields status failed). A dry run records
// nothing and writes nothing; Report.Actions describes the plan.
func (e *Engine) SyncPair(ctx context.Context, p resolve.Pair, opt Options) (rep Report) {
	key := p.Key()
	rep.Key = key
	if err := ctx.Err(); err != nil {
		rep.Status, rep.Err = state.RunAborted, err
		return rep
	}
	unlock, err := e.lock(ctx, key.String())
	if err != nil {
		rep.Status, rep.Err = state.RunAborted, err
		return rep
	}
	defer unlock()

	r := &pairRun{e: e, p: p, opt: opt, key: key, log: e.logger().With("pair", key.String())}
	if !opt.DryRun {
		id, err := e.Store.StartRun(ctx, key, opt.trigger())
		if err != nil {
			rep.Status, rep.Err = state.RunFailed, fmt.Errorf("engine: start run: %w", err)
			return rep
		}
		rep.RunID = id
	}
	defer func() {
		if v := recover(); v != nil {
			r.log.Error("engine: panic during sync", "panic", v, "stack", string(debug.Stack()))
			r.fatal(fmt.Errorf("engine: panic: %v", v))
		}
		r.report(ctx, &rep)
		if !opt.DryRun {
			msg := ""
			if rep.Err != nil {
				msg = rep.Err.Error()
			}
			digest, count := "", 0
			if r.gitDone {
				digest, count = r.digest, r.refCount
			}
			if err := e.Store.FinishRun(context.WithoutCancel(ctx), rep.RunID, rep.Status, rep.Steps, msg, digest, count); err != nil {
				r.log.Error("engine: finish run", "err", err)
			}
		}
		r.log.Info("engine: pair synced", "status", rep.Status, "dry_run", opt.DryRun)
	}()
	r.run(ctx)
	return rep
}
