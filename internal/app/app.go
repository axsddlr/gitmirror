// Package app is gitmirror's composition root: it builds the forge providers,
// state store and sync engine from a config and exposes the sync workflow
// shared by the CLI commands.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gitmirror/internal/config"
	"gitmirror/internal/engine"
	"gitmirror/internal/forge"
	_ "gitmirror/internal/forge/gitea"  // registers the gitea provider
	_ "gitmirror/internal/forge/github" // registers the github provider
	"gitmirror/internal/forge/httpx"
	"gitmirror/internal/gitx"
	"gitmirror/internal/resolve"
	"gitmirror/internal/secretbox"
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
	// Redact, when set, is called with every forge token of each effective
	// configuration before it is used (at Build and after every UpdateUI),
	// so the caller can mask them in its output.
	Redact func(secrets ...string)
}

// App is a fully wired gitmirror instance.
//
// The effective configuration (config file plus the forges and rules
// managed in the web UI) and the providers built from it form a Snapshot
// that UpdateUI can replace at runtime; read it through Snapshot, Config or
// Providers rather than caching it.
type App struct {
	Store  *state.Store
	Engine *engine.Engine
	Logger *slog.Logger

	file     *config.Config
	opts     Options
	readOnly bool
	snap     atomic.Pointer[Snapshot]
	updMu    sync.Mutex // serialises UpdateUI (and UI reads against it)
	boxMu    sync.Mutex
	box      *secretbox.Box // lazily loaded; nil until a UI token is opened or sealed

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

// ForgeURL returns the normalised base URL of a configured forge: scheme
// and host lowercased, default ports and trailing slashes removed, and
// "https://github.com" for a github forge without a URL. It is recorded with
// destination ownership so that re-pointing a forge name at another server
// is detected. "" means the forge has no URL.
func ForgeURL(f config.Forge) string {
	raw := strings.TrimRight(strings.TrimSpace(f.URL), "/")
	if raw == "" && f.Type == "github" {
		raw = "https://github.com"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	u.Scheme = strings.ToLower(u.Scheme)
	host, port := strings.ToLower(u.Hostname()), u.Port()
	defaultPort := u.Scheme == "https" && port == "443" || u.Scheme == "http" && port == "80"
	switch {
	case port != "" && !defaultPort:
		u.Host = net.JoinHostPort(host, port)
	case strings.Contains(host, ":"):
		u.Host = "[" + host + "]" // IPv6 literal
	default:
		u.Host = host
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String()
}

// ForgeURLs returns ForgeURL of every configured forge, by name.
func ForgeURLs(cfg *config.Config) map[string]string {
	out := make(map[string]string, len(cfg.Forges))
	for name, f := range cfg.Forges {
		out[name] = ForgeURL(f)
	}
	return out
}

// Build wires everything up from the config file cfg (as returned by
// config.Load or config.Parse). The caller must Close the App.
//
// Unless o.ReadOnly, Build first takes an exclusive OS-level lock on
// <StateDir>/gitmirror.lock (held until Close) and then marks runs left
// "running" by a dead process as aborted. A held lock yields an error
// wrapping ErrLocked.
//
// Build then loads the forges and rules managed in the web UI from the
// state store, opens their tokens with <StateDir>/secret.key, merges them
// with cfg (config.Merge) and builds the providers of the effective config.
// A UI forge whose token cannot be decrypted is an error naming the forge.
// Build never creates the key file; UpdateUI does when it first seals a token.
func Build(ctx context.Context, cfg *config.Config, o Options) (*App, error) {
	log := o.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	git := &gitx.Runner{Env: o.GitEnv, UseHostConfig: cfg.Git.UseHostConfig}
	v, err := git.Check(ctx)
	if err != nil {
		return nil, err
	}
	log.Debug("tools", "git", v.Git, "git-lfs", v.LFS)
	var lk *dirLock
	if !o.ReadOnly {
		if lk, err = acquireLock(ctx, cfg.StateDir, o.LockTimeout); err != nil {
			return nil, err
		}
	}
	st, err := state.Open(ctx, filepath.Join(cfg.StateDir, "state.db"))
	if err != nil {
		_ = lk.release() // best effort: the error above is what gets reported
		return nil, err
	}
	fail := func(err error) (*App, error) {
		st.Close()
		_ = lk.release() // best effort: the error above is what gets reported
		return nil, err
	}
	if !o.ReadOnly {
		n, err := st.AbandonRunning(ctx)
		if err != nil {
			return fail(fmt.Errorf("abandon interrupted runs: %w", err))
		}
		if n > 0 {
			log.Warn("marked interrupted runs as aborted", "count", n)
		}
	}
	a := &App{
		lock:     lk,
		Store:    st,
		Logger:   log,
		file:     cfg,
		opts:     o,
		readOnly: o.ReadOnly,
		Engine: &engine.Engine{
			Store:       st,
			Git:         git,
			CacheDir:    filepath.Join(cfg.StateDir, "cache"),
			Logger:      log,
			Concurrency: cfg.Concurrency,
		},
	}
	ui, err := a.loadUI(ctx)
	if err != nil {
		return fail(err)
	}
	eff, provs, err := a.effective(ui)
	if err != nil {
		return fail(err)
	}
	a.install(eff, provs)
	if v.LFS == "" {
		for _, r := range eff.Rules {
			if r.Effective(eff.Defaults).Includes(config.FeatureLFS) {
				log.Warn("git-lfs is not installed; LFS objects will not be mirrored", "rule", r.Name)
				break
			}
		}
	}
	return a, nil
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

// SyncWith is Sync with every option, including AllowPrune. The whole run
// (resolve, sync and reconcile) uses the snapshot current when it starts.
// With no rules configured it returns config.ErrNoRules.
func (a *App) SyncWith(ctx context.Context, req SyncRequest) (*SyncResult, error) {
	// One snapshot for the whole run: a concurrent UpdateUI affects only
	// later runs.
	snap := a.Snapshot()
	cfg := snap.Config
	if len(cfg.Rules) == 0 {
		return nil, config.ErrNoRules
	}
	rules, repos := req.Rules, req.Repos
	known := map[string]bool{}
	settings := map[string]config.RuleSettings{}
	for _, r := range cfg.Rules {
		known[r.Name] = true
		settings[r.Name] = r.Effective(cfg.Defaults)
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

	res := resolve.Resolve(ctx, cfg, snap.Providers, rules...)
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

	opt := engine.Options{DryRun: req.DryRun, Trigger: req.Trigger, AllowPrune: req.AllowPrune,
		Providers: snap.Providers, ForgeURLs: snap.ForgeURLs}
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
