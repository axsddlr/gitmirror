package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"gitmirror/internal/config"
	"gitmirror/internal/engine"
	"gitmirror/internal/forge"
	"gitmirror/internal/resolve"
)

// Settings keys of the server settings managed in the web UI. Each one is
// used only while the config file leaves the matching server field unset.
const (
	// SettingExternalURL is the public address (server.external_url).
	SettingExternalURL = "ui_external_url"
	// SettingWebhookSecret is the sealed webhook secret
	// (server.webhook_secret).
	SettingWebhookSecret = "ui_webhook_secret"
)

// checkTimeout bounds CheckForge.
const checkTimeout = 30 * time.Second

// CheckForge builds a provider for forge f (as it would be named name) and
// asks it which account its token authenticates as, without saving
// anything. Errors come from the provider and never contain the token.
func (a *App) CheckForge(ctx context.Context, name string, f config.Forge) (login string, err error) {
	cfg := &config.Config{Forges: map[string]config.Forge{name: f}}
	provs, err := BuildProviders(cfg, a.opts)
	if err != nil {
		return "", err
	}
	p, ok := provs[name]
	if !ok {
		return "", fmt.Errorf("forge %q: no provider", name)
	}
	id, ok := p.(forge.Identity)
	if !ok {
		return "", fmt.Errorf("forge %q cannot report its account: %w", name, forge.ErrUnsupported)
	}
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	return id.Login(ctx)
}

// Preview shows what saving a web UI change would do to one rule: it
// applies fn to a copy of the UI set, merges and validates it exactly like
// UpdateUI (errors match ErrConfig) and builds its providers, then resolves
// the rule and dry-runs the engine on its pairs. Nothing is persisted and
// the current snapshot stays; the engine's dry run writes neither to a
// forge nor to the state store.
//
// The rules sharing the rule's destination forge (and those that could
// mirror back into its source) are resolved too, so a destination another
// rule already writes to shows up as a dropped pair with a RuleError.
// Only rule's pairs, drops and errors are returned. An unknown rule wraps
// ErrNoMatch.
func (a *App) Preview(ctx context.Context, fn func(*UIConfig) error, rule string) (*SyncResult, error) {
	eff, provs, err := a.candidate(ctx, fn)
	if err != nil {
		return nil, err
	}
	var cand *config.Rule
	for i := range eff.Rules {
		if eff.Rules[i].Name == rule {
			cand = &eff.Rules[i]
		}
	}
	if cand == nil {
		return nil, fmt.Errorf("%w: unknown rule %q", ErrNoMatch, rule)
	}
	var related []string
	for _, r := range eff.Rules {
		if r.Name == rule || r.To.Forge == cand.To.Forge || r.From.Forge == cand.To.Forge && r.To.Forge == cand.From.Forge {
			related = append(related, r.Name)
		}
	}
	all := resolve.Resolve(ctx, eff, provs, related...)
	res := resolve.Result{Complete: map[string]bool{rule: all.Complete[rule]}}
	for _, p := range all.Pairs {
		if p.Rule == rule {
			res.Pairs = append(res.Pairs, p)
		}
	}
	for _, p := range all.Dropped {
		if p.Rule == rule {
			res.Dropped = append(res.Dropped, p)
		}
	}
	for _, e := range all.Errors {
		if e.Rule == rule {
			res.Errors = append(res.Errors, e)
		}
	}
	reports := a.Engine.Run(ctx, res.Pairs, engine.Options{DryRun: true, Trigger: "preview",
		Providers: provs, ForgeURLs: ForgeURLs(eff)})
	return &SyncResult{Resolve: res, Reports: reports}, nil
}

// candidate applies fn to a copy of the UI set and returns the merged,
// validated config and its providers without installing or saving them.
func (a *App) candidate(ctx context.Context, fn func(*UIConfig) error) (*config.Config, map[string]forge.Provider, error) {
	a.updMu.Lock()
	defer a.updMu.Unlock()
	cur, err := a.loadUI(ctx)
	if err != nil {
		return nil, nil, err
	}
	next := cur.clone()
	if err := fn(next); err != nil {
		return nil, nil, err
	}
	if err := checkRuleKeys(next); err != nil {
		return nil, nil, err
	}
	eff, provs, err := a.effective(next)
	if err != nil {
		return nil, nil, err
	}
	if a.opts.Redact != nil {
		for _, f := range eff.Forges {
			a.opts.Redact(f.Token)
		}
	}
	return eff, provs, nil
}

func checkRuleKeys(u *UIConfig) error {
	for name, r := range u.Rules {
		if r.Name != name {
			return fmt.Errorf("rule stored under key %q is named %q", name, r.Name)
		}
	}
	return nil
}

// PublicAddress returns the effective server.external_url and whether the
// config file sets it (then the web UI cannot change it).
func (a *App) PublicAddress() (url string, fromFile bool) {
	return a.Config().Server.ExternalURL, a.file.Server.ExternalURL != ""
}

// WebhookSecret returns the effective webhook secret ("" when webhooks are
// not configured) and whether the config file sets it.
func (a *App) WebhookSecret() (secret string, fromFile bool) {
	return a.Config().Server.WebhookSecret, a.file.Server.WebhookSecret != ""
}

// SetPublicAddress stores the public address used when the config file has
// no server.external_url; "" removes it. The value is trimmed of spaces and
// trailing slashes and validated like external_url (errors match
// ErrConfig). It applies at once, like UpdateUI. With external_url in the
// config file it fails with an error wrapping config.ErrFileDefined.
func (a *App) SetPublicAddress(ctx context.Context, raw string) error {
	if a.file.Server.ExternalURL != "" {
		return fmt.Errorf("server.external_url is %w", config.ErrFileDefined)
	}
	v := strings.TrimRight(strings.TrimSpace(raw), "/")
	if v != "" {
		if err := config.ValidateExternalURL(v); err != nil {
			return configError{err}
		}
	}
	return a.updateServerSettings(ctx, func(u *UIConfig) (func() error, error) {
		if u.externalURL == v {
			return nil, nil
		}
		u.externalURL = v
		return func() error {
			if v == "" {
				return a.Store.DeleteSetting(ctx, SettingExternalURL)
			}
			return a.Store.PutSetting(ctx, SettingExternalURL, []byte(v))
		}, nil
	})
}

// EnsureWebhookSecret creates a random webhook secret, stored sealed, when
// neither the config file nor the web UI has one, and reports whether it
// did. The new secret applies at once (the webhook receiver uses it).
func (a *App) EnsureWebhookSecret(ctx context.Context) (created bool, err error) {
	if a.file.Server.WebhookSecret != "" {
		return false, nil
	}
	err = a.updateServerSettings(ctx, func(u *UIConfig) (func() error, error) {
		if u.webhookSecret != "" {
			return nil, nil
		}
		created = true
		return a.replaceSecret(ctx, u)
	})
	return created, err
}

// NewWebhookSecret replaces the web UI's webhook secret with a new random
// one; webhooks set up with the old secret stop being accepted. With a
// secret in the config file it fails with an error wrapping
// config.ErrFileDefined.
func (a *App) NewWebhookSecret(ctx context.Context) error {
	if a.file.Server.WebhookSecret != "" {
		return fmt.Errorf("server.webhook_secret is %w", config.ErrFileDefined)
	}
	return a.updateServerSettings(ctx, func(u *UIConfig) (func() error, error) {
		return a.replaceSecret(ctx, u)
	})
}

func (a *App) replaceSecret(ctx context.Context, u *UIConfig) (func() error, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	sec := hex.EncodeToString(b)
	box, err := a.secretBox(true)
	if err != nil {
		return nil, err
	}
	sealed, err := box.Seal([]byte(sec))
	if err != nil {
		return nil, fmt.Errorf("seal webhook secret: %w", err)
	}
	u.webhookSecret = sec
	return func() error { return a.Store.PutSetting(ctx, SettingWebhookSecret, sealed) }, nil
}

// updateServerSettings is UpdateUI for the server settings: fn changes a
// copy of the UI set and returns the store write (nil for no change); the
// result is validated and its providers built before the write, then the
// new snapshot is installed.
func (a *App) updateServerSettings(ctx context.Context, fn func(*UIConfig) (func() error, error)) error {
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
	persist, err := fn(next)
	if err != nil || persist == nil {
		return err
	}
	eff, provs, err := a.effective(next)
	if err != nil {
		return err
	}
	if err := persist(); err != nil {
		return err
	}
	a.install(eff, provs)
	return nil
}

// loadServerSettings reads the web UI's server settings into u.
func (a *App) loadServerSettings(ctx context.Context, u *UIConfig) error {
	v, ok, err := a.Store.Setting(ctx, SettingExternalURL)
	if err != nil {
		return err
	}
	if ok {
		u.externalURL = string(v)
	}
	v, ok, err = a.Store.Setting(ctx, SettingWebhookSecret)
	if err != nil || !ok {
		return err
	}
	box, err := a.secretBox(false)
	if err != nil {
		return fmt.Errorf("web UI webhook secret: cannot decrypt it: %w", err)
	}
	sec, err := box.Open(v)
	if err != nil {
		return fmt.Errorf("web UI webhook secret: cannot decrypt it (was %s replaced?): %w", a.keyPath(), err)
	}
	u.webhookSecret = string(sec)
	return nil
}

// withServerSettings returns the config file with the UI's server settings
// filled in where the file leaves them unset.
func (u *UIConfig) withServerSettings() *config.Config {
	f := u.file
	eu := f.Server.ExternalURL == "" && u.externalURL != ""
	ws := f.Server.WebhookSecret == "" && u.webhookSecret != ""
	if !eu && !ws {
		return f
	}
	c := *f
	if eu {
		c.Server.ExternalURL = u.externalURL
	}
	if ws {
		c.Server.WebhookSecret = u.webhookSecret
	}
	return &c
}
