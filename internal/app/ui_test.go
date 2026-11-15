package app

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"gitmirror/internal/config"
	"gitmirror/internal/forge"
	"gitmirror/internal/forge/fake"
	"gitmirror/internal/secretbox"
	"gitmirror/internal/state"
)

const uiToken = "SENTINEL-ui-token-0003"

// blocker wraps a fake provider; while armed, GetRepo blocks until opened.
type blocker struct {
	*fake.Provider
	mu      sync.Mutex
	armed   bool
	release chan struct{}
	entered chan struct{}
}

func (b *blocker) arm() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.armed, b.release, b.entered = true, make(chan struct{}), make(chan struct{}, 16)
}

func (b *blocker) open() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.armed {
		b.armed = false
		close(b.release)
	}
}

func (b *blocker) GetRepo(ctx context.Context, owner, name string) (*forge.Repo, error) {
	b.mu.Lock()
	armed, rel, ent := b.armed, b.release, b.entered
	b.mu.Unlock()
	if armed {
		ent <- struct{}{}
		select {
		case <-rel:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return b.Provider.GetRepo(ctx, owner, name)
}

// uiFixture is a fixture whose provider override also has the forges the
// tests add in the UI: "uisrc" (a source) and "uidst" (a destination).
type uiFixture struct {
	*fixture
	uisrc, uidst *fake.Provider
	srcGate      *blocker
}

func newUIFixture(t *testing.T) *uiFixture {
	f := newFixture(t)
	return &uiFixture{
		fixture: f,
		uisrc:   fake.New(fake.Options{GitBaseDir: t.TempDir(), Username: "u", Token: uiToken}),
		uidst:   fake.New(fake.Options{GitBaseDir: t.TempDir(), Username: "u", Token: uiToken}),
		srcGate: &blocker{Provider: f.src},
	}
}

func (u *uiFixture) options() Options {
	o := u.fixture.options()
	o.Providers = map[string]forge.Provider{"src": u.srcGate, "dst": u.dst, "uisrc": u.uisrc, "uidst": u.uidst}
	return o
}

func (u *uiFixture) build(o Options) *App {
	u.t.Helper()
	a, err := Build(context.Background(), u.config(), o)
	if err != nil {
		u.t.Fatalf("Build: %v", err)
	}
	u.t.Cleanup(func() { a.Close() })
	return a
}

// addUISource seeds owner/name with one commit on the uisrc forge.
func (u *uiFixture) addUISource(owner, name string) {
	u.t.Helper()
	if err := u.uisrc.AddRepo(forge.Repo{Owner: owner, Name: name, DefaultBranch: "main"}); err != nil {
		u.t.Fatal(err)
	}
	work := filepath.Join(u.t.TempDir(), "work")
	u.git(u.t.TempDir(), "init", "-q", "-b", "main", work)
	u.git(work, "remote", "add", "origin", u.uisrc.GitURL(owner, name))
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte(name+"\n"), 0o644); err != nil {
		u.t.Fatal(err)
	}
	u.git(work, "add", "a.txt")
	u.git(work, "commit", "-q", "-m", "init")
	u.git(work, "push", "-q", "origin", "main")
}

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func addUISourceForgeAndRule(u *UIConfig) error {
	if err := u.PutForge("uisrc", config.Forge{Type: "fake", Token: uiToken}); err != nil {
		return err
	}
	return u.PutRule(config.Rule{Name: "ui1", From: config.Source{Forge: "uisrc", Owner: "carol"}, To: config.Dest{Forge: "dst", Owner: "carolm"}})
}

func TestUpdateUIAddsForgeAndRuleForNextSync(t *testing.T) {
	f := newUIFixture(t)
	f.addUISource("carol", "c1")
	a := f.build(f.options())
	before := a.Snapshot()
	if len(before.Config.Rules) != 2 {
		t.Fatalf("rules = %d", len(before.Config.Rules))
	}
	ch := a.Changed()
	if err := a.UpdateUI(context.Background(), addUISourceForgeAndRule); err != nil {
		t.Fatal(err)
	}
	if !closed(ch) || !closed(before.Changed()) {
		t.Fatal("Changed not signalled")
	}
	if closed(a.Changed()) {
		t.Fatal("new Changed channel already closed")
	}
	cfg := a.Config()
	if len(cfg.Rules) != 3 || cfg.RuleOrigin("ui1") != config.OriginUI || cfg.ForgeOrigin("uisrc") != config.OriginUI ||
		cfg.RuleOrigin("r1") != config.OriginFile {
		t.Fatalf("effective config rules=%d origins ui1=%q uisrc=%q r1=%q", len(cfg.Rules),
			cfg.RuleOrigin("ui1"), cfg.ForgeOrigin("uisrc"), cfg.RuleOrigin("r1"))
	}
	if _, ok := a.Providers()["uisrc"]; !ok {
		t.Fatal("no provider for the UI forge")
	}
	if len(before.Config.Rules) != 2 {
		t.Fatal("old snapshot was modified")
	}
	res, err := a.Sync(context.Background(), []string{"ui1"}, nil, false, "cli")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Reports) != 1 || res.Reports[0].Status != state.RunOK {
		t.Fatalf("reports = %+v", res.Reports)
	}
	if !f.dstHas("carolm", "c1") {
		t.Fatal("UI rule did not mirror")
	}
	ui, err := a.UI(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ui.Forges["uisrc"].Token != uiToken || ui.Rules["ui1"].From.Owner != "carol" || len(ui.Forges) != 1 || len(ui.Rules) != 1 {
		t.Fatalf("UI() = %v", ui)
	}
	if ui.Rules["ui1"].From.Filter != nil {
		t.Fatal("UI() returns the rule with defaults applied; want it as entered")
	}
}

func TestUpdateUIInvalidChangeChangesNothing(t *testing.T) {
	f := newUIFixture(t)
	a := f.build(f.options())
	ctx := context.Background()
	if err := a.UpdateUI(ctx, func(u *UIConfig) error {
		return u.PutRule(config.Rule{Name: "lit", From: config.Source{Forge: "src", Repo: "alice/x"}, To: config.Dest{Forge: "dst", Repo: "m/x"}})
	}); err != nil {
		t.Fatal(err)
	}
	snap := a.Snapshot()
	rowsBefore, _ := a.Store.UIRules(ctx)
	bad := map[string]func(*UIConfig) error{
		"unknown forge": func(u *UIConfig) error {
			return u.PutRule(config.Rule{Name: "u", From: config.Source{Forge: "ghost", Owner: "o"}, To: config.Dest{Forge: "dst", Owner: "m"}})
		},
		"collision": func(u *UIConfig) error {
			return u.PutRule(config.Rule{Name: "dup", From: config.Source{Forge: "src", Repo: "bob/y"}, To: config.Dest{Forge: "dst", Repo: "M/X"}})
		},
		"forge still used": func(u *UIConfig) error {
			if err := u.PutForge("uidst", config.Forge{Type: "fake", Token: uiToken}); err != nil {
				return err
			}
			return u.PutRule(config.Rule{Name: "z", From: config.Source{Forge: "src", Owner: "o"}, To: config.Dest{Forge: "nope", Owner: "m"}})
		},
		"mutation error": func(u *UIConfig) error {
			delete(u.Rules, "lit")
			return errors.New("user aborted")
		},
	}
	for name, fn := range bad {
		t.Run(name, func(t *testing.T) {
			if err := a.UpdateUI(ctx, fn); err == nil {
				t.Fatal("invalid change accepted")
			}
			if a.Snapshot() != snap || closed(snap.Changed()) {
				t.Fatal("snapshot replaced by a failed change")
			}
			rows, _ := a.Store.UIRules(ctx)
			fs, _ := a.Store.UIForges(ctx)
			if len(rows) != len(rowsBefore) || len(fs) != 0 {
				t.Fatalf("db changed: rules %d (was %d), forges %d", len(rows), len(rowsBefore), len(fs))
			}
		})
	}
}

func TestUpdateUIRefusesFileEntries(t *testing.T) {
	f := newUIFixture(t)
	a := f.build(f.options())
	ctx := context.Background()
	snap := a.Snapshot()
	for name, fn := range map[string]func(*UIConfig) error{
		"put file rule":    func(u *UIConfig) error { return u.PutRule(config.Rule{Name: "r1"}) },
		"delete file rule": func(u *UIConfig) error { return u.DeleteRule("r2") },
		"put file forge":   func(u *UIConfig) error { return u.PutForge("src", config.Forge{Type: "fake", Token: "x"}) },
		"delete file forge": func(u *UIConfig) error {
			return u.DeleteForge("dst")
		},
		"map edit of file rule": func(u *UIConfig) error {
			u.Rules["r1"] = config.Rule{Name: "r1", From: config.Source{Forge: "src", Owner: "z"}, To: config.Dest{Forge: "dst", Owner: "z"}}
			return nil
		},
		"map edit of file forge": func(u *UIConfig) error {
			u.Forges["src"] = config.Forge{Type: "fake", Token: "x"}
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := a.UpdateUI(ctx, fn)
			if !errors.Is(err, config.ErrFileDefined) {
				t.Fatalf("err = %v, want ErrFileDefined", err)
			}
			if a.Snapshot() != snap {
				t.Fatal("snapshot replaced")
			}
		})
	}
	err := a.UpdateUI(ctx, func(u *UIConfig) error { return u.DeleteRule("never") })
	if !errors.Is(err, ErrUnknownEntry) {
		t.Fatalf("delete unknown rule err = %v", err)
	}
	err = a.UpdateUI(ctx, func(u *UIConfig) error {
		u.Rules["key"] = config.Rule{Name: "other"}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "key") {
		t.Fatalf("rule stored under another name accepted: %v", err)
	}
}

func TestUITokensNotStoredInPlaintext(t *testing.T) {
	f := newUIFixture(t)
	a := f.build(f.options())
	if err := a.UpdateUI(context.Background(), addUISourceForgeAndRule); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(f.stateDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	var tok []byte
	if err := db.QueryRow(`SELECT token FROM ui_forges WHERE name = 'uisrc'`).Scan(&tok); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if len(tok) == 0 || bytes.Contains(tok, []byte(uiToken)) {
		t.Fatalf("stored token = %q", tok)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"state.db", "state.db-wal"} {
		b, err := os.ReadFile(filepath.Join(f.stateDir, name))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(uiToken)) {
			t.Fatalf("%s contains the plaintext token", name)
		}
	}
	if fi, err := os.Stat(filepath.Join(f.stateDir, secretbox.KeyFile)); err != nil || fi.Size() != secretbox.KeySize {
		t.Fatalf("key file: %v", err)
	}
}

func TestBuildLoadsUIEntriesOnRestart(t *testing.T) {
	f := newUIFixture(t)
	f.addUISource("carol", "c1")
	a, err := Build(context.Background(), f.config(), f.options())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.UpdateUI(context.Background(), addUISourceForgeAndRule); err != nil {
		t.Fatal(err)
	}
	a.Close()

	var redacted []string
	o := f.options()
	o.Redact = func(s ...string) { redacted = append(redacted, s...) }
	b := f.build(o)
	cfg := b.Config()
	if cfg.RuleOrigin("ui1") != config.OriginUI || cfg.Forges["uisrc"].Token != uiToken {
		t.Fatalf("restart lost UI entries: %v", cfg)
	}
	found := false
	for _, s := range redacted {
		found = found || s == uiToken
	}
	if !found {
		t.Fatal("UI token not passed to Options.Redact")
	}
	if _, err := b.Sync(context.Background(), []string{"ui1"}, nil, false, "cli"); err != nil || !f.dstHas("carolm", "c1") {
		t.Fatalf("sync after restart: %v", err)
	}

	// Read-only builds see them too.
	o = f.options()
	o.ReadOnly = true
	ro := f.build(o)
	if ro.Config().RuleOrigin("ui1") != config.OriginUI {
		t.Fatal("read-only build lost UI entries")
	}
	if err := ro.UpdateUI(context.Background(), func(*UIConfig) error { return nil }); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("UpdateUI on a read-only app = %v", err)
	}
}

func TestBuildUndecryptableTokenNamesForge(t *testing.T) {
	f := newUIFixture(t)
	a, err := Build(context.Background(), f.config(), f.options())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.UpdateUI(context.Background(), addUISourceForgeAndRule); err != nil {
		t.Fatal(err)
	}
	a.Close()
	keyPath := filepath.Join(f.stateDir, secretbox.KeyFile)
	if err := os.WriteFile(keyPath, bytes.Repeat([]byte{7}, secretbox.KeySize), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Build(context.Background(), f.config(), f.options())
	if err == nil || !strings.Contains(err.Error(), `"uisrc"`) || strings.Contains(err.Error(), uiToken) {
		t.Fatalf("Build err = %v, want it to name forge uisrc", err)
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	o := f.options()
	o.ReadOnly = true
	_, err = Build(context.Background(), f.config(), o)
	if err == nil || !strings.Contains(err.Error(), `"uisrc"`) {
		t.Fatalf("read-only Build without key err = %v", err)
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("read-only Build created the key file")
	}
}

func TestReadOnlyBuildWithoutUIDoesNotCreateKey(t *testing.T) {
	f := newUIFixture(t)
	o := f.options()
	o.ReadOnly = true
	a := f.build(o)
	if len(a.Config().Rules) != 2 {
		t.Fatal("file rules missing")
	}
	if _, err := os.Stat(filepath.Join(f.stateDir, secretbox.KeyFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("key file created by a read-only build: %v", err)
	}
}

func TestSyncInProgressKeepsItsSnapshot(t *testing.T) {
	f := newUIFixture(t)
	f.addSource("alice", "a")
	a := f.build(f.options())
	ctx := context.Background()
	if err := a.UpdateUI(ctx, func(u *UIConfig) error {
		if err := u.PutForge("uidst", config.Forge{Type: "fake", URL: "https://one.example", Token: uiToken}); err != nil {
			return err
		}
		return u.PutRule(config.Rule{Name: "ui1", From: config.Source{Forge: "src", Owner: "alice"}, To: config.Dest{Forge: "uidst", Owner: "m"}})
	}); err != nil {
		t.Fatal(err)
	}
	f.srcGate.arm()
	type out struct {
		res *SyncResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		res, err := a.Sync(ctx, []string{"ui1"}, nil, false, "cli")
		done <- out{res, err}
	}()
	select {
	case <-f.srcGate.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("sync did not start")
	}
	// The run is blocked inside the engine: re-point the UI forge.
	if err := a.UpdateUI(ctx, func(u *UIConfig) error {
		g := u.Forges["uidst"]
		g.URL = "https://two.example"
		return u.PutForge("uidst", g)
	}); err != nil {
		t.Fatal(err)
	}
	if got := a.Snapshot().ForgeURLs["uidst"]; got != "https://two.example" {
		t.Fatalf("new snapshot forge URL = %q", got)
	}
	f.srcGate.open()
	var o out
	select {
	case o = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("sync did not finish")
	}
	if o.err != nil || len(o.res.Reports) != 1 || o.res.Reports[0].Status != state.RunOK {
		t.Fatalf("sync = %+v, %v", o.res, o.err)
	}
	rec, ok, err := a.Store.OwnershipOf(ctx, "uidst", "m/a")
	if err != nil || !ok || rec.ForgeURL != "https://one.example" {
		t.Fatalf("ownership = %+v %v %v; the running sync must keep its snapshot's forge URL", rec, ok, err)
	}
}

func TestRemovedUIRuleNeverActsOnItsDestinations(t *testing.T) {
	f := newUIFixture(t)
	f.addUISource("carol", "c1")
	a := f.build(f.options())
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	a.Store.SetClock(clock)
	a.Engine.Now = clock
	if err := a.UpdateUI(ctx, func(u *UIConfig) error {
		if err := u.PutForge("uisrc", config.Forge{Type: "fake", Token: uiToken}); err != nil {
			return err
		}
		return u.PutRule(config.Rule{Name: "ui1", From: config.Source{Forge: "uisrc", Owner: "carol"},
			To: config.Dest{Forge: "dst", Owner: "carolm"}, OnSourceDeleted: "delete", DeletionGrace: "1h"})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Sync(ctx, nil, nil, false, "cli"); err != nil || !f.dstHas("carolm", "c1") {
		t.Fatalf("initial sync: %v", err)
	}
	if err := a.UpdateUI(ctx, func(u *UIConfig) error { return u.DeleteRule("ui1") }); err != nil {
		t.Fatal(err)
	}
	f.uisrc.RemoveRepo("carol", "c1")
	k := state.PairKey{Rule: "ui1", SrcForge: "uisrc", SrcRepo: "carol/c1", DstForge: "dst", DstRepo: "carolm/c1"}
	for range 3 {
		res, err := a.Sync(ctx, nil, nil, false, "cli")
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range res.Reconciled {
			if r.Key.Rule == "ui1" {
				t.Fatalf("reconcile acted on the removed rule: %+v", r)
			}
		}
		advance(2 * time.Hour)
	}
	repo, err := f.dst.GetRepo(ctx, "carolm", "c1")
	if err != nil || repo.Archived {
		t.Fatalf("destination of the removed rule = %+v, %v; want it untouched", repo, err)
	}
	if _, ok, _ := a.Store.MissingSince(ctx, k); ok {
		t.Fatal("reconcile observed the removed rule's pair")
	}
}

func TestUIConfigRedactsTokens(t *testing.T) {
	f := newUIFixture(t)
	a := f.build(f.options())
	if err := a.UpdateUI(context.Background(), addUISourceForgeAndRule); err != nil {
		t.Fatal(err)
	}
	u, err := a.UI(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var logged strings.Builder
	slog.New(slog.NewTextHandler(&logged, nil)).Info("ui", "cfg", u)
	for _, s := range []string{fmt.Sprintf("%v %+v %#v %s", u, u, u, u), logged.String()} {
		if strings.Contains(s, uiToken) || strings.Contains(s, "SENTINEL-src-0001") {
			t.Fatalf("token leaked: %s", s)
		}
	}
}
