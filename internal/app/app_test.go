package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitmirror/internal/config"
	"gitmirror/internal/forge"
	"gitmirror/internal/forge/fake"
	"gitmirror/internal/state"
)

func init() {
	config.KnownForgeTypes = append(config.KnownForgeTypes, "fake")
}

type fixture struct {
	t        *testing.T
	gitEnv   []string
	src, dst *fake.Provider
	stateDir string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	home := t.TempDir()
	return &fixture{
		t: t,
		gitEnv: []string{
			"HOME=" + home,
			"USERPROFILE=" + home,
			"XDG_CONFIG_HOME=" + home,
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_CONFIG_GLOBAL=" + filepath.Join(home, ".gitconfig"),
		},
		src:      fake.New(fake.Options{GitBaseDir: t.TempDir(), Username: "u", Token: "SENTINEL-src-0001"}),
		dst:      fake.New(fake.Options{GitBaseDir: t.TempDir(), Username: "u", Token: "SENTINEL-dst-0002"}),
		stateDir: filepath.Join(t.TempDir(), "state"),
	}
}

func (f *fixture) git(dir string, args ...string) {
	f.t.Helper()
	full := append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(kv), "GIT_") {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, f.gitEnv...)
	if out, err := cmd.CombinedOutput(); err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// addSource seeds owner/name on the source forge with one commit.
func (f *fixture) addSource(owner, name string) {
	f.t.Helper()
	if err := f.src.AddRepo(forge.Repo{Owner: owner, Name: name, DefaultBranch: "main"}); err != nil {
		f.t.Fatal(err)
	}
	work := filepath.Join(f.t.TempDir(), "work")
	f.git(f.t.TempDir(), "init", "-q", "-b", "main", work)
	f.git(work, "remote", "add", "origin", f.src.GitURL(owner, name))
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte(name+"\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
	f.git(work, "add", "a.txt")
	f.git(work, "commit", "-q", "-m", "init")
	f.git(work, "push", "-q", "origin", "main")
}

const cfgTmpl = `
state_dir: '@STATE@'
concurrency: 2
forges:
  src: {type: fake, token: SENTINEL-src-0001}
  dst: {type: fake, token: SENTINEL-dst-0002}
rules:
  - name: r1
    from: {forge: src, owner: alice}
    to: {forge: dst, owner: mirrors}
    on_source_deleted: archive
  - name: r2
    from: {forge: src, owner: bob}
    to: {forge: dst, owner: bobmirror}
`

func (f *fixture) config() *config.Config {
	f.t.Helper()
	y := strings.ReplaceAll(cfgTmpl, "@STATE@", filepath.ToSlash(f.stateDir))
	cfg, err := config.Parse([]byte(y), func(string) (string, bool) { return "", false },
		func(string) ([]byte, error) { return nil, os.ErrNotExist })
	if err != nil {
		f.t.Fatal(err)
	}
	return cfg
}

// options returns the Build options used by the fixture's apps.
func (f *fixture) options() Options {
	return Options{
		Providers: map[string]forge.Provider{"src": f.src, "dst": f.dst},
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		GitEnv:    f.gitEnv,
	}
}

func (f *fixture) build() *App {
	f.t.Helper()
	a, err := Build(context.Background(), f.config(), f.options())
	if err != nil {
		f.t.Fatalf("Build: %v", err)
	}
	f.t.Cleanup(func() { a.Close() })
	return a
}

func (f *fixture) dstHas(owner, name string) bool {
	_, err := f.dst.GetRepo(context.Background(), owner, name)
	return err == nil
}

func srcKey(rule, owner, name, dstOwner string) state.PairKey {
	return state.PairKey{Rule: rule, SrcForge: "src", SrcRepo: owner + "/" + name, DstForge: "dst", DstRepo: dstOwner + "/" + name}
}

func TestBuildWithProviderOverride(t *testing.T) {
	f := newFixture(t)
	a := f.build()
	if a.Providers()["src"] != forge.Provider(f.src) || a.Providers()["dst"] != forge.Provider(f.dst) {
		t.Fatalf("providers not the override: %v", a.Providers())
	}
	if a.Store == nil || a.Engine == nil || a.Logger == nil || a.Config() == nil {
		t.Fatalf("incomplete App: %+v", a)
	}
	if _, err := os.Stat(filepath.Join(f.stateDir, "state.db")); err != nil {
		t.Fatalf("state db: %v", err)
	}
	if want := filepath.Join(f.stateDir, "cache"); a.Engine.CacheDir != want {
		t.Fatalf("CacheDir = %q, want %q", a.Engine.CacheDir, want)
	}
	if a.Engine.Concurrency != 2 {
		t.Fatalf("Concurrency = %d", a.Engine.Concurrency)
	}
}

func TestBuildProvidersFromConfig(t *testing.T) {
	f := newFixture(t)
	y := `
state_dir: '` + filepath.ToSlash(f.stateDir) + `'
forges:
  gh: {type: github, token: ghtoken}
  home: {type: gitea, url: "https://git.example.com", token: gttoken}
rules:
  - name: r
    from: {forge: gh, owner: alice}
    to: {forge: home, owner: mirrors}
`
	cfg, err := config.Parse([]byte(y), func(string) (string, bool) { return "", false },
		func(string) ([]byte, error) { return nil, os.ErrNotExist })
	if err != nil {
		t.Fatal(err)
	}
	a, err := Build(context.Background(), cfg, Options{GitEnv: f.gitEnv})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.Providers()["gh"].Type() != "github" || a.Providers()["home"].Type() != "gitea" {
		t.Fatalf("providers = %v", a.Providers())
	}
}

func TestBuildAbandonsRunningRuns(t *testing.T) {
	f := newFixture(t)
	st, err := state.Open(context.Background(), filepath.Join(f.stateDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	k := srcKey("r1", "alice", "a", "mirrors")
	if _, err := st.StartRun(context.Background(), k, "cli"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	a := f.build()
	runs, err := a.Store.Runs(context.Background(), k, 0)
	if err != nil || len(runs) != 1 || runs[0].Status != state.RunAborted {
		t.Fatalf("runs = %+v, err %v", runs, err)
	}
}

func TestSyncMirrors(t *testing.T) {
	f := newFixture(t)
	f.addSource("alice", "a")
	f.addSource("alice", "b")
	f.addSource("bob", "c")
	a := f.build()
	res, err := a.Sync(context.Background(), nil, nil, false, "cli")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Reports) != 3 || len(res.Resolve.Pairs) != 3 {
		t.Fatalf("reports=%d pairs=%d", len(res.Reports), len(res.Resolve.Pairs))
	}
	for _, r := range res.Reports {
		if r.Status != state.RunOK {
			t.Fatalf("report %v: %s %v", r.Key, r.Status, r.Err)
		}
	}
	if !f.dstHas("mirrors", "a") || !f.dstHas("mirrors", "b") || !f.dstHas("bobmirror", "c") {
		t.Fatal("destinations not created")
	}
	ps, _ := a.Store.Pairs(context.Background())
	if len(ps) != 3 {
		t.Fatalf("store pairs = %d", len(ps))
	}
}

func TestSyncRepoFilterSyncsOnlyThatRepoAndDoesNotReconcile(t *testing.T) {
	f := newFixture(t)
	f.addSource("alice", "a")
	f.addSource("alice", "b")
	a := f.build()
	ctx := context.Background()
	if _, err := a.Sync(ctx, nil, nil, false, "cli"); err != nil {
		t.Fatal(err)
	}
	// The source of b disappears.
	f.src.RemoveRepo("alice", "b")
	bKey := srcKey("r1", "alice", "b", "mirrors")

	res, err := a.Sync(ctx, nil, []string{"ALICE/A"}, false, "cli")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Reports) != 1 || res.Reports[0].Key.SrcRepo != "alice/a" {
		t.Fatalf("reports = %+v", res.Reports)
	}
	if res.Reconciled != nil {
		t.Fatalf("Reconciled = %+v, want nil with a repo filter", res.Reconciled)
	}
	if _, ok, err := a.Store.MissingSince(ctx, bKey); err != nil || ok {
		t.Fatalf("MissingSince(b) = %v, %v after filtered sync; reconcile must not run", ok, err)
	}

	// Control: an unfiltered sync does notice the removed source.
	if _, err := a.Sync(ctx, nil, nil, false, "cli"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := a.Store.MissingSince(ctx, bKey); !ok {
		t.Fatal("unfiltered sync did not record b as missing")
	}
}

func TestSyncRuleFilter(t *testing.T) {
	f := newFixture(t)
	f.addSource("alice", "a")
	f.addSource("bob", "c")
	a := f.build()
	res, err := a.Sync(context.Background(), []string{"r2"}, nil, false, "cli")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Reports) != 1 || res.Reports[0].Key.Rule != "r2" {
		t.Fatalf("reports = %+v", res.Reports)
	}
	if f.dstHas("mirrors", "a") || !f.dstHas("bobmirror", "c") {
		t.Fatal("rule filter not honoured")
	}
	if len(res.Resolve.Complete) != 1 || !res.Resolve.Complete["r2"] {
		t.Fatalf("Complete = %v", res.Resolve.Complete)
	}
}

func TestSyncUnknownRepoErrors(t *testing.T) {
	f := newFixture(t)
	f.addSource("alice", "a")
	a := f.build()
	_, err := a.Sync(context.Background(), nil, []string{"alice/a", "nobody/ghost"}, false, "cli")
	if err == nil || !strings.Contains(err.Error(), "nobody/ghost") {
		t.Fatalf("err = %v, want it to list nobody/ghost", err)
	}
	if strings.Contains(err.Error(), "alice/a") {
		t.Fatalf("err lists a matched repo: %v", err)
	}
	if f.dstHas("mirrors", "a") {
		t.Fatal("nothing should have been synced")
	}
}

func TestSyncUnknownRuleErrors(t *testing.T) {
	f := newFixture(t)
	a := f.build()
	_, err := a.Sync(context.Background(), []string{"nope"}, nil, false, "cli")
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v", err)
	}
}

func TestSyncDryRunWritesNothing(t *testing.T) {
	f := newFixture(t)
	f.addSource("alice", "a")
	a := f.build()
	res, err := a.Sync(context.Background(), nil, nil, true, "cli")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Reports) != 1 || len(res.Reports[0].Actions) == 0 {
		t.Fatalf("reports = %+v", res.Reports)
	}
	if f.dstHas("mirrors", "a") {
		t.Fatal("dry run created a repo")
	}
	runs, _ := a.Store.RecentRuns(context.Background(), 0)
	if len(runs) != 0 {
		t.Fatalf("runs = %+v", runs)
	}
}
