package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gitmirror/internal/config"
	"gitmirror/internal/forge"
	"gitmirror/internal/state"
)

func TestCheckForge(t *testing.T) {
	f := newUIFixture(t)
	a := f.build(f.options())
	login, err := a.CheckForge(context.Background(), "uisrc", config.Forge{Type: "fake", Token: uiToken})
	if err != nil || login != "u" {
		t.Fatalf("CheckForge = %q, %v", login, err)
	}
	if _, err := a.CheckForge(context.Background(), "nosuch", config.Forge{Type: "fake", Token: uiToken}); err == nil {
		t.Fatal("CheckForge of a forge without provider succeeded")
	}
	if got := a.Config().ForgeOrigin("uisrc"); got != "" {
		t.Fatalf("CheckForge saved the forge (origin %q)", got)
	}
}

// storeDump captures every state table the web UI could touch.
type storeDump struct {
	pairs    []state.PairState
	runs     []state.Run
	forges   []state.UIForge
	rules    []state.UIRule
	settings []state.Setting
	owned    bool
}

func dumpStore(t *testing.T, a *App) storeDump {
	t.Helper()
	ctx := context.Background()
	var d storeDump
	var err error
	if d.pairs, err = a.Store.Pairs(ctx); err != nil {
		t.Fatal(err)
	}
	if d.runs, err = a.Store.RecentRuns(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if d.forges, err = a.Store.UIForges(ctx); err != nil {
		t.Fatal(err)
	}
	if d.rules, err = a.Store.UIRules(ctx); err != nil {
		t.Fatal(err)
	}
	if d.settings, err = a.Store.Settings(ctx); err != nil {
		t.Fatal(err)
	}
	if _, d.owned, err = a.Store.Owned(ctx, "dst", "carolm/c1"); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestPreviewDryRunsWithoutSaving(t *testing.T) {
	f := newUIFixture(t)
	f.addUISource("carol", "c1")
	a := f.build(f.options())
	before := a.Snapshot()
	dump := dumpStore(t, a)

	res, err := a.Preview(context.Background(), addUISourceForgeAndRule, "ui1")
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if len(res.Reports) != 1 || res.Reports[0].Key.SrcRepo != "carol/c1" || res.Reports[0].Key.DstRepo != "carolm/c1" {
		t.Fatalf("reports = %+v", res.Reports)
	}
	if acts := strings.Join(res.Reports[0].Actions, "\n"); !strings.Contains(acts, "create repository") {
		t.Fatalf("actions = %q", acts)
	}
	for _, p := range res.Resolve.Pairs {
		if p.Rule != "ui1" {
			t.Fatalf("preview resolved another rule: %+v", p)
		}
	}
	if a.Snapshot() != before || closed(before.Changed()) {
		t.Fatal("Preview swapped the snapshot")
	}
	if a.Config().RuleOrigin("ui1") != "" || a.Config().ForgeOrigin("uisrc") != "" {
		t.Fatal("Preview saved the change")
	}
	if after := dumpStore(t, a); !reflect.DeepEqual(dump, after) {
		t.Fatalf("Preview wrote to the store:\nbefore %+v\nafter  %+v", dump, after)
	}
	if _, err := f.dst.GetRepo(context.Background(), "carolm", "c1"); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("Preview created the destination: %v", err)
	}
}

func TestPreviewValidationError(t *testing.T) {
	f := newUIFixture(t)
	a := f.build(f.options())
	err := func() error {
		_, err := a.Preview(context.Background(), func(u *UIConfig) error {
			return u.PutRule(config.Rule{Name: "self", From: config.Source{Forge: "src", Repo: "alice/a"}, To: config.Dest{Forge: "src", Repo: "alice/a"}})
		}, "self")
		return err
	}()
	if !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "same repo") {
		t.Fatalf("err = %v", err)
	}
	if _, err := a.Preview(context.Background(), func(*UIConfig) error { return nil }, "nosuch"); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("unknown rule: err = %v", err)
	}
}

// A candidate rule whose destination collides with an existing rule's is
// reported as dropped, which a preview of the rule alone would not see.
func TestPreviewSeesCollisionWithOtherRules(t *testing.T) {
	f := newUIFixture(t)
	f.addSource("alice", "a")
	f.addUISource("carol", "a")
	a := f.build(f.options())
	res, err := a.Preview(context.Background(), func(u *UIConfig) error {
		if err := u.PutForge("uisrc", config.Forge{Type: "fake", Token: uiToken}); err != nil {
			return err
		}
		return u.PutRule(config.Rule{Name: "clash", From: config.Source{Forge: "uisrc", Owner: "carol"}, To: config.Dest{Forge: "dst", Owner: "mirrors"}})
	}, "clash")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Reports) != 0 || len(res.Resolve.Dropped) != 1 || res.Resolve.Dropped[0].Rule != "clash" {
		t.Fatalf("reports %+v dropped %+v", res.Reports, res.Resolve.Dropped)
	}
	if len(res.Resolve.Errors) != 1 || !strings.Contains(res.Resolve.Errors[0].Error(), "multiple sources") {
		t.Fatalf("errors = %v", res.Resolve.Errors)
	}
}

func TestWebhookSecretFromUI(t *testing.T) {
	f := newUIFixture(t)
	var redacted []string
	o := f.options()
	o.Redact = func(s ...string) { redacted = append(redacted, s...) }
	a := f.build(o)
	ctx := context.Background()
	if s, fromFile := a.WebhookSecret(); s != "" || fromFile {
		t.Fatalf("initial secret = %q %v", s, fromFile)
	}
	created, err := a.EnsureWebhookSecret(ctx)
	if err != nil || !created {
		t.Fatalf("EnsureWebhookSecret = %v, %v", created, err)
	}
	sec, _ := a.WebhookSecret()
	if len(sec) < 32 || a.Config().Server.WebhookSecret != sec {
		t.Fatalf("secret %q, effective %q", sec, a.Config().Server.WebhookSecret)
	}
	if !strings.Contains(strings.Join(redacted, " "), sec) {
		t.Fatal("webhook secret was not passed to Redact")
	}
	if again, err := a.EnsureWebhookSecret(ctx); err != nil || again {
		t.Fatalf("second EnsureWebhookSecret = %v, %v", again, err)
	}
	sts, err := a.Store.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range sts {
		if bytes.Contains(s.Value, []byte(sec)) {
			t.Fatalf("setting %s holds the plaintext secret", s.Key)
		}
		if s.Key == SettingWebhookSecret {
			found = true
		}
	}
	if !found {
		t.Fatal("no sealed secret stored")
	}
	if err := a.NewWebhookSecret(ctx); err != nil {
		t.Fatal(err)
	}
	if s2, _ := a.WebhookSecret(); s2 == sec || s2 == "" {
		t.Fatalf("NewWebhookSecret kept %q", s2)
	}
	s2, _ := a.WebhookSecret()

	// A restart reads the sealed secret back.
	a.Close()
	b := f.build(f.options())
	if got, _ := b.WebhookSecret(); got != s2 {
		t.Fatalf("after restart secret = %q, want %q", got, s2)
	}
}

func TestPublicAddressFromUI(t *testing.T) {
	f := newUIFixture(t)
	a := f.build(f.options())
	ctx := context.Background()
	for _, bad := range []string{"mirror.example.com", "ftp://x", "https://u:p@x", "https://x/?q=1"} {
		if err := a.SetPublicAddress(ctx, bad); !errors.Is(err, ErrConfig) {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
	if err := a.SetPublicAddress(ctx, " https://Mirror.example.com/ "); err != nil {
		t.Fatal(err)
	}
	if u, fromFile := a.PublicAddress(); u != "https://Mirror.example.com" || fromFile {
		t.Fatalf("PublicAddress = %q %v", u, fromFile)
	}
	if a.Config().Server.ExternalURL != "https://Mirror.example.com" {
		t.Fatalf("effective external_url = %q", a.Config().Server.ExternalURL)
	}
	if err := a.SetPublicAddress(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if a.Config().Server.ExternalURL != "" {
		t.Fatalf("cleared external_url = %q", a.Config().Server.ExternalURL)
	}
}

func TestServerSettingsFromFileWin(t *testing.T) {
	f := newUIFixture(t)
	cfgPath := filepath.Join(t.TempDir(), "c.yaml")
	y := fmt.Sprintf("state_dir: '%s'\nserver: {external_url: 'https://file.example.com', webhook_secret: 'file-secret-123456'}\n",
		filepath.ToSlash(f.stateDir))
	if err := os.WriteFile(cfgPath, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	a, err := Build(context.Background(), cfg, f.options())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ctx := context.Background()
	if err := a.SetPublicAddress(ctx, "https://ui.example.com"); !errors.Is(err, config.ErrFileDefined) {
		t.Fatalf("SetPublicAddress err = %v", err)
	}
	if created, err := a.EnsureWebhookSecret(ctx); err != nil || created {
		t.Fatalf("EnsureWebhookSecret = %v %v", created, err)
	}
	if err := a.NewWebhookSecret(ctx); !errors.Is(err, config.ErrFileDefined) {
		t.Fatalf("NewWebhookSecret err = %v", err)
	}
	if s, fromFile := a.WebhookSecret(); s != "file-secret-123456" || !fromFile {
		t.Fatalf("WebhookSecret = %q %v", s, fromFile)
	}
	if u, fromFile := a.PublicAddress(); u != "https://file.example.com" || !fromFile {
		t.Fatalf("PublicAddress = %q %v", u, fromFile)
	}
}
