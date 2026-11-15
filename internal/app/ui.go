package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"gitmirror/internal/config"
	"gitmirror/internal/forge"
	"gitmirror/internal/secretbox"
	"gitmirror/internal/state"
)

// ErrReadOnly is returned by UpdateUI on an App built with Options.ReadOnly.
var ErrReadOnly = errors.New("app is read-only")

// ErrConfig is matched (errors.Is) by Build and LoadEffective errors caused
// by an invalid effective configuration: a web UI entry that clashes with
// the config file or fails validation together with it.
var ErrConfig = errors.New("invalid configuration")

// configError marks err as an ErrConfig without changing its message.
type configError struct{ error }

func (e configError) Is(target error) bool { return target == ErrConfig }
func (e configError) Unwrap() error        { return e.error }

// LoadEffective returns the effective configuration (cfg plus the forges and
// rules managed in the web UI) without building an App or its providers.
// It reads <StateDir>/state.db only when it exists and never creates it or
// the key file, and takes no lock, so it can run next to serve.
func LoadEffective(ctx context.Context, cfg *config.Config, o Options) (*config.Config, error) {
	path := filepath.Join(cfg.StateDir, "state.db")
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		eff, err := config.Merge(cfg, nil, nil)
		if err != nil {
			return nil, configError{err}
		}
		return eff, nil
	} else if err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	st, err := state.Open(ctx, path)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	a := &App{Store: st, file: cfg, opts: o, readOnly: true}
	ui, err := a.loadUI(ctx)
	if err != nil {
		return nil, err
	}
	return ui.merge()
}

// ErrUnknownEntry is wrapped by UIConfig.DeleteForge / DeleteRule errors for
// a name that is not managed in the web UI (nor in the config file).
var ErrUnknownEntry = errors.New("not found")

// Snapshot is one consistent view of the effective configuration: the
// merged config, the providers built from it and their normalised forge
// URLs. A Snapshot is immutable; callers must not modify it or its maps.
type Snapshot struct {
	// Config is the effective config. Config.ForgeOrigin and
	// Config.RuleOrigin tell file entries from web UI entries.
	Config *config.Config
	// Providers maps forge names to providers.
	Providers map[string]forge.Provider
	// ForgeURLs maps forge names to ForgeURL of the forge.
	ForgeURLs map[string]string

	changed chan struct{}
}

// Changed returns a channel that is closed once this snapshot has been
// replaced by a newer one.
func (s *Snapshot) Changed() <-chan struct{} { return s.changed }

// Snapshot returns the current snapshot. Use one snapshot for a whole unit
// of work so it sees a consistent configuration.
func (a *App) Snapshot() *Snapshot { return a.snap.Load() }

// Config returns the current effective config (Snapshot().Config).
func (a *App) Config() *config.Config { return a.Snapshot().Config }

// Providers returns the current providers (Snapshot().Providers).
func (a *App) Providers() map[string]forge.Provider { return a.Snapshot().Providers }

// FileConfig returns the config the App was built from (the config file
// alone, without web UI entries).
func (a *App) FileConfig() *config.Config { return a.file }

// Changed returns a channel that is closed when the current snapshot is
// replaced (Snapshot().Changed()). Take the snapshot first and wait on its
// Changed channel to never miss a change.
func (a *App) Changed() <-chan struct{} { return a.Snapshot().Changed() }

// UIConfig is the set of forges and rules managed in the web UI, with
// plaintext tokens. Rules are as entered (defaults not applied) and keyed by
// name. Use the Put/Delete methods, which refuse names defined in the
// config file; editing the maps directly is allowed too, and UpdateUI
// refuses file names either way. Its String and LogValue redact tokens.
type UIConfig struct {
	Forges map[string]config.Forge
	Rules  map[string]config.Rule

	file *config.Config
}

// PutForge adds or replaces the UI forge name.
func (u *UIConfig) PutForge(name string, f config.Forge) error {
	if u.file.ForgeOrigin(name) != "" {
		return fmt.Errorf("forge %q is %w and cannot be changed in the web UI", name, config.ErrFileDefined)
	}
	u.Forges[name] = f
	return nil
}

// DeleteForge removes the UI forge name.
func (u *UIConfig) DeleteForge(name string) error {
	if u.file.ForgeOrigin(name) != "" {
		return fmt.Errorf("forge %q is %w and cannot be removed in the web UI", name, config.ErrFileDefined)
	}
	if _, ok := u.Forges[name]; !ok {
		return fmt.Errorf("forge %q: %w", name, ErrUnknownEntry)
	}
	delete(u.Forges, name)
	return nil
}

// PutRule adds or replaces the UI rule r.Name.
func (u *UIConfig) PutRule(r config.Rule) error {
	if u.file.RuleOrigin(r.Name) != "" {
		return fmt.Errorf("rule %q is %w and cannot be changed in the web UI", r.Name, config.ErrFileDefined)
	}
	u.Rules[r.Name] = config.CloneRule(r)
	return nil
}

// DeleteRule removes the UI rule name. Its stored pairs are kept but no
// longer reconciled, so its destinations are never archived or deleted.
func (u *UIConfig) DeleteRule(name string) error {
	if u.file.RuleOrigin(name) != "" {
		return fmt.Errorf("rule %q is %w and cannot be removed in the web UI", name, config.ErrFileDefined)
	}
	if _, ok := u.Rules[name]; !ok {
		return fmt.Errorf("rule %q: %w", name, ErrUnknownEntry)
	}
	delete(u.Rules, name)
	return nil
}

// String implements fmt.Stringer without exposing tokens.
func (u *UIConfig) String() string {
	return fmt.Sprintf("{forges=%v rules=%v}", u.Forges, slices.Sorted(maps.Keys(u.Rules)))
}

// GoString implements fmt.GoStringer without exposing tokens.
func (u *UIConfig) GoString() string { return "app.UIConfig" + u.String() }

// LogValue implements slog.LogValuer without exposing tokens.
func (u *UIConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Any("forges", slices.Sorted(maps.Keys(u.Forges))),
		slog.Any("rules", slices.Sorted(maps.Keys(u.Rules))),
	)
}

func (u *UIConfig) clone() *UIConfig {
	c := &UIConfig{Forges: maps.Clone(u.Forges), Rules: make(map[string]config.Rule, len(u.Rules)), file: u.file}
	for n, r := range u.Rules {
		c.Rules[n] = config.CloneRule(r)
	}
	return c
}

// UI returns the forges and rules currently managed in the web UI, read
// from the state store, with plaintext tokens.
func (a *App) UI(ctx context.Context) (*UIConfig, error) {
	a.updMu.Lock()
	defer a.updMu.Unlock()
	return a.loadUI(ctx)
}

// UpdateUI changes the forges and rules managed in the web UI: it loads the
// current UI set, applies fn to a copy, merges the result with the config
// file and validates it (config.Merge), builds the providers and only then
// persists the change in one transaction (tokens sealed) and swaps the
// snapshot. On any error, including one returned by fn, nothing is persisted
// and the current snapshot stays. Concurrent calls are serialised. A change
// naming a forge or rule of the config file fails with an error wrapping
// config.ErrFileDefined. Runs already in progress keep their snapshot.
func (a *App) UpdateUI(ctx context.Context, fn func(*UIConfig) error) error {
	if a.readOnly {
		return ErrReadOnly
	}
	a.updMu.Lock()
	defer a.updMu.Unlock()
	cur, err := a.loadUI(ctx)
	if err != nil {
		return err
	}
	next := cur.clone()
	if err := fn(next); err != nil {
		return err
	}
	for name, r := range next.Rules {
		if r.Name != name {
			return fmt.Errorf("rule stored under key %q is named %q", name, r.Name)
		}
	}
	eff, provs, err := a.effective(next)
	if err != nil {
		return err
	}
	ch, err := a.changes(cur, next)
	if err != nil {
		return err
	}
	if err := a.Store.ApplyUI(ctx, ch); err != nil {
		return fmt.Errorf("save web UI changes: %w", err)
	}
	a.install(eff, provs)
	return nil
}

// changes computes the store writes turning cur into next, sealing the
// tokens of new and changed forges.
func (a *App) changes(cur, next *UIConfig) (state.UIChanges, error) {
	var ch state.UIChanges
	for _, n := range slices.Sorted(maps.Keys(cur.Forges)) {
		if _, ok := next.Forges[n]; !ok {
			ch.DeleteForges = append(ch.DeleteForges, n)
		}
	}
	for _, n := range slices.Sorted(maps.Keys(next.Forges)) {
		f := next.Forges[n]
		if old, ok := cur.Forges[n]; ok && old == f {
			continue
		}
		box, err := a.secretBox(true)
		if err != nil {
			return ch, err
		}
		sealed, err := box.Seal([]byte(f.Token))
		if err != nil {
			return ch, fmt.Errorf("forge %q: seal token: %w", n, err)
		}
		ch.PutForges = append(ch.PutForges, state.UIForge{Name: n, Type: f.Type, URL: f.URL, Username: f.Username, SealedToken: sealed})
	}
	for _, n := range slices.Sorted(maps.Keys(cur.Rules)) {
		if _, ok := next.Rules[n]; !ok {
			ch.DeleteRules = append(ch.DeleteRules, n)
		}
	}
	for _, n := range slices.Sorted(maps.Keys(next.Rules)) {
		js, err := json.Marshal(next.Rules[n])
		if err != nil {
			return ch, fmt.Errorf("rule %q: encode: %w", n, err)
		}
		if old, ok := cur.Rules[n]; ok {
			if oj, err := json.Marshal(old); err == nil && string(oj) == string(js) {
				continue
			}
		}
		ch.PutRules = append(ch.PutRules, state.UIRule{Name: n, JSON: js})
	}
	return ch, nil
}

// loadUI reads the UI forges and rules from the store and opens the tokens.
func (a *App) loadUI(ctx context.Context) (*UIConfig, error) {
	forges, err := a.Store.UIForges(ctx)
	if err != nil {
		return nil, err
	}
	rs, err := a.Store.UIRules(ctx)
	if err != nil {
		return nil, err
	}
	u := &UIConfig{Forges: make(map[string]config.Forge, len(forges)), Rules: make(map[string]config.Rule, len(rs)), file: a.file}
	for _, f := range forges {
		box, err := a.secretBox(false)
		if err != nil {
			return nil, fmt.Errorf("forge %q (web UI): cannot decrypt its token: %w", f.Name, err)
		}
		tok, err := box.Open(f.SealedToken)
		if err != nil {
			return nil, fmt.Errorf("forge %q (web UI): cannot decrypt its token (was %s replaced?): %w",
				f.Name, a.keyPath(), err)
		}
		u.Forges[f.Name] = config.Forge{Type: f.Type, URL: f.URL, Username: f.Username, Token: string(tok)}
	}
	for _, r := range rs {
		var rule config.Rule
		if err := json.Unmarshal(r.JSON, &rule); err != nil {
			return nil, fmt.Errorf("rule %q (web UI): decode: %w", r.Name, err)
		}
		if rule.Name != r.Name {
			return nil, fmt.Errorf("rule %q (web UI): stored rule is named %q", r.Name, rule.Name)
		}
		u.Rules[r.Name] = rule
	}
	return u, nil
}

// merge merges u with the config file (config.Merge); errors match ErrConfig.
func (u *UIConfig) merge() (*config.Config, error) {
	rules := make([]config.Rule, 0, len(u.Rules))
	for _, n := range slices.Sorted(maps.Keys(u.Rules)) {
		rules = append(rules, u.Rules[n])
	}
	eff, err := config.Merge(u.file, u.Forges, rules)
	if err != nil {
		return nil, configError{err}
	}
	return eff, nil
}

// effective merges u with the config file and builds its providers.
func (a *App) effective(u *UIConfig) (*config.Config, map[string]forge.Provider, error) {
	eff, err := u.merge()
	if err != nil {
		return nil, nil, err
	}
	provs, err := BuildProviders(eff, a.opts)
	if err != nil {
		return nil, nil, err
	}
	return eff, provs, nil
}

// install makes eff/provs the current snapshot and signals the old one's
// Changed channel. Callers serialise (Build, or UpdateUI under updMu).
func (a *App) install(eff *config.Config, provs map[string]forge.Provider) {
	if a.opts.Redact != nil {
		toks := make([]string, 0, len(eff.Forges))
		for _, f := range eff.Forges {
			toks = append(toks, f.Token)
		}
		a.opts.Redact(toks...)
	}
	s := &Snapshot{Config: eff, Providers: provs, ForgeURLs: ForgeURLs(eff), changed: make(chan struct{})}
	if old := a.snap.Swap(s); old != nil {
		close(old.changed)
	}
}

func (a *App) keyPath() string { return filepath.Join(a.file.StateDir, secretbox.KeyFile) }

// secretBox returns the key file's Box, creating the key file only when
// create is set (never on a read-only App).
func (a *App) secretBox(create bool) (*secretbox.Box, error) {
	a.boxMu.Lock()
	defer a.boxMu.Unlock()
	if a.box != nil {
		return a.box, nil
	}
	var b *secretbox.Box
	var err error
	if create && !a.readOnly {
		b, err = secretbox.LoadOrCreate(a.keyPath())
	} else {
		b, err = secretbox.Load(a.keyPath())
		if errors.Is(err, fs.ErrNotExist) {
			err = fmt.Errorf("secret key file %s is missing", a.keyPath())
		}
	}
	if err != nil {
		return nil, err
	}
	a.box = b
	return b, nil
}
