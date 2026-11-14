// Package app is gitmirror's composition root: it builds the forge providers,
// state store and sync engine from a config and exposes the sync workflow
// shared by the CLI commands.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"gitmirror/internal/config"
	"gitmirror/internal/engine"
	"gitmirror/internal/forge"
	_ "gitmirror/internal/forge/gitea"  // registers the gitea provider
	_ "gitmirror/internal/forge/github" // registers the github provider
	"gitmirror/internal/forge/httpx"
	"gitmirror/internal/gitx"
	"gitmirror/internal/resolve"
	"gitmirror/internal/state"
)

// ErrNoMatch is wrapped by Sync errors caused by a --rule or --repo filter
// that names something that does not exist (a usage error).
var ErrNoMatch = errors.New("no match")

// Options customises Build.
type Options struct {
	// Providers, when non-nil, is used instead of building providers from the
	// config (tests).
	Providers map[string]forge.Provider
	// Logger receives log output; nil discards it.
	Logger *slog.Logger
	// GitEnv is appended to the git child environment (tests isolate git
	// config with it).
	GitEnv []string
	// ReadOnly skips the state dir lock and the abandoning of interrupted
	// runs, so it never disturbs a process that holds the lock. For commands
	// that do not write state (plan).
	ReadOnly bool
	// LockTimeout is how long Build waits for the state dir lock held by
	// another process: 0 fails immediately, >0 retries every 250ms until it
	// elapses or ctx is done. Ignored when ReadOnly.
	LockTimeout time.Duration
}

// App is a fully wired gitmirror instance.
type App struct {
	Config    *config.Config
	Providers map[string]forge.Provider
	Store     *state.Store
	Engine    *engine.Engine
	Logger    *slog.Logger

	lock *dirLock // nil when ReadOnly or after Close
}

// SyncResult is the outcome of App.Sync.
type SyncResult struct {
	Resolve    resolve.Result
	Reports    []engine.Report
	Reconciled []engine.Report
}

// BuildProviders returns o.Providers if set, otherwise one provider per
// configured forge, each with a retrying HTTP client.
func BuildProviders(cfg *config.Config, o Options) (map[string]forge.Provider, error) {
	if o.Providers != nil {
		return o.Providers, nil
	}
	provs := make(map[string]forge.Provider, len(cfg.Forges))
	for name, f := range cfg.Forges {
		p, err := forge.New(f.Type, forge.Options{
			URL:        f.URL,
			Username:   f.Username,
			Token:      f.Token,
			HTTPClient: httpx.NewClient(nil),
		})
		if err != nil {
			return nil, fmt.Errorf("forge %q: %w", name, err)
		}
		provs[name] = p
	}
	return provs, nil
}

// Build wires everything up. The caller must Close the App.
//
// Unless o.ReadOnly, Build first takes an exclusive OS-level lock on
// <StateDir>/gitmirror.lock (held until Close) and then marks runs left
// "running" by a dead process as aborted. A held lock yields an error
// wrapping ErrLocked.
func Build(ctx context.Context, cfg *config.Config, o Options) (*App, error) {
	log := o.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	provs, err := BuildProviders(cfg, o)
	if err != nil {
		return nil, err
	}
	git := &gitx.Runner{Env: o.GitEnv, UseHostConfig: cfg.Git.UseHostConfig}
	v, err := git.Check(ctx)
	if err != nil {
		return nil, err
	}
	log.Debug("tools", "git", v.Git, "git-lfs", v.LFS)
	if v.LFS == "" {
		for _, r := range cfg.Rules {
			if r.Effective(cfg.Defaults).Includes(config.FeatureLFS) {
				log.Warn("git-lfs is not installed; LFS objects will not be mirrored", "rule", r.Name)
				break
			}
		}
	}
	var lk *dirLock
	if !o.ReadOnly {
		if lk, err = acquireLock(ctx, cfg.StateDir, o.LockTimeout); err != nil {
			return nil, err
		}
	}
	st, err := state.Open(ctx, filepath.Join(cfg.StateDir, "state.db"))
	if err != nil {
		lk.release()
		return nil, err
	}
	if !o.ReadOnly {
		n, err := st.AbandonRunning(ctx)
		if err != nil {
			st.Close()
			lk.release()
			return nil, fmt.Errorf("abandon interrupted runs: %w", err)
		}
		if n > 0 {
			log.Warn("marked interrupted runs as aborted", "count", n)
		}
	}
	return &App{
		lock:      lk,
		Config:    cfg,
		Providers: provs,
		Store:     st,
		Logger:    log,
		Engine: &engine.Engine{
			Providers:   provs,
			Store:       st,
			Git:         git,
			CacheDir:    filepath.Join(cfg.StateDir, "cache"),
			Logger:      log,
			Concurrency: cfg.Concurrency,
		},
	}, nil
}

// Close releases the state store and then the state dir lock.
func (a *App) Close() error {
	err := a.Store.Close()
	lerr := a.lock.release()
	a.lock = nil
	return errors.Join(err, lerr)
}

// Sync resolves the (optionally filtered) rules, syncs the resulting pairs
// and, when no repo filter is given, reconciles pairs whose source vanished.
//
// Reconcile is skipped whenever repos is non-empty: a repo filter makes every
// other pair look missing, and reconciling would start deletion clocks.
func (a *App) Sync(ctx context.Context, rules, repos []string, dryRun bool, trigger string) (*SyncResult, error) {
	return a.SyncWith(ctx, SyncRequest{Rules: rules, Repos: repos, DryRun: dryRun, Trigger: trigger})
}

// SyncRequest is the input of App.SyncWith.
type SyncRequest struct {
	// Rules and Repos optionally restrict the run (see Sync).
	Rules, Repos []string
	// DryRun computes the plan without writing anything.
	DryRun bool
	// Trigger records what started the run ("cli", "schedule", "webhook").
	Trigger string
	// AllowPrune bypasses the mass ref deletion guard for this run only.
	AllowPrune bool
}

// SyncWith is Sync with every option, including AllowPrune.
func (a *App) SyncWith(ctx context.Context, req SyncRequest) (*SyncResult, error) {
	rules, repos := req.Rules, req.Repos
	known := map[string]bool{}
	settings := map[string]config.RuleSettings{}
	for _, r := range a.Config.Rules {
		known[r.Name] = true
		settings[r.Name] = r.Effective(a.Config.Defaults)
	}
	var unknownRules []string
	for _, r := range rules {
		if !known[r] && !slices.Contains(unknownRules, r) {
			unknownRules = append(unknownRules, r)
		}
	}
	if len(unknownRules) > 0 {
		return nil, fmt.Errorf("%w: unknown rule(s): %s", ErrNoMatch, strings.Join(unknownRules, ", "))
	}

	res := resolve.Resolve(ctx, a.Config, a.Providers, rules...)
	pairs := res.Pairs
	if len(repos) > 0 {
		matched := map[string]bool{}
		var kept []resolve.Pair
		for _, p := range pairs {
			for _, r := range repos {
				if strings.EqualFold(p.Src.FullName(), r) {
					matched[strings.ToLower(r)] = true
					kept = append(kept, p)
					break
				}
			}
		}
		var missing []string
		for _, r := range repos {
			if !matched[strings.ToLower(r)] && !slices.Contains(missing, r) {
				missing = append(missing, r)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			return nil, fmt.Errorf("%w: repo(s) matched no resolved pair: %s", ErrNoMatch, strings.Join(missing, ", "))
		}
		pairs = kept
	}

	opt := engine.Options{DryRun: req.DryRun, Trigger: req.Trigger, AllowPrune: req.AllowPrune}
	if req.AllowPrune && a.Logger != nil {
		a.Logger.Warn("mass ref deletion guard disabled for this run (--allow-prune)")
	}
	out := &SyncResult{Resolve: res}
	out.Resolve.Pairs = pairs
	out.Reports = a.Engine.Run(ctx, pairs, opt)
	if len(repos) == 0 && ctx.Err() == nil {
		out.Reconciled = a.Engine.Reconcile(ctx, res.Pairs, res.Complete, res.Dropped, settings, opt)
	}
	return out, nil
}
