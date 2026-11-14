package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gitmirror/internal/app"
	"gitmirror/internal/config"
	"gitmirror/internal/forge"
	"gitmirror/internal/forge/fake"
	"gitmirror/internal/state"
)

const sentinel = "SENTINEL-TOKEN-7c1d9e"

func init() {
	if !slices.Contains(config.KnownForgeTypes, "fake") {
		config.KnownForgeTypes = append(config.KnownForgeTypes, "fake")
	}
}

type testEnv struct {
	t        *testing.T
	gitEnv   []string
	src, dst *fake.Provider
	stateDir string
	cfgPath  string
}

const cfgYAML = `
state_dir: '@STATE@'
concurrency: 2
forges:
  src: {type: fake, token: @TOKEN@}
  dst: {type: fake, token: @TOKEN@}
rules:
  - name: r1
    from: {forge: src, owner: alice}
    to: {forge: dst, owner: mirrors}
`

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	home := t.TempDir()
	e := &testEnv{
		t: t,
		gitEnv: []string{
			"HOME=" + home,
			"USERPROFILE=" + home,
			"XDG_CONFIG_HOME=" + home,
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_CONFIG_GLOBAL=" + filepath.Join(home, ".gitconfig"),
		},
		src:      fake.New(fake.Options{GitBaseDir: t.TempDir(), Username: "u", Token: sentinel}),
		dst:      fake.New(fake.Options{GitBaseDir: t.TempDir(), Username: "u", Token: sentinel}),
		stateDir: filepath.Join(t.TempDir(), "state"),
	}
	y := strings.NewReplacer("@STATE@", filepath.ToSlash(e.stateDir), "@TOKEN@", sentinel).Replace(cfgYAML)
	e.cfgPath = filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(e.cfgPath, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	old := appOptions
	appOptions = func() app.Options {
		return app.Options{
			Providers: map[string]forge.Provider{"src": e.src, "dst": e.dst},
			GitEnv:    e.gitEnv,
		}
	}
	t.Cleanup(func() { appOptions = old })
	return e
}

func (e *testEnv) git(dir string, args ...string) {
	e.t.Helper()
	full := append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(kv), "GIT_") {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, e.gitEnv...)
	if out, err := cmd.CombinedOutput(); err != nil {
		e.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func (e *testEnv) addSource(owner, name string) {
	e.t.Helper()
	if err := e.src.AddRepo(forge.Repo{Owner: owner, Name: name, DefaultBranch: "main"}); err != nil {
		e.t.Fatal(err)
	}
	work := filepath.Join(e.t.TempDir(), "work")
	e.git(e.t.TempDir(), "init", "-q", "-b", "main", work)
	e.git(work, "remote", "add", "origin", e.src.GitURL(owner, name))
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte(name+"\n"), 0o644); err != nil {
		e.t.Fatal(err)
	}
	e.git(work, "add", "a.txt")
	e.git(work, "commit", "-q", "-m", "init")
	e.git(work, "push", "-q", "origin", "main")
}

// cli runs the command in-process with --config pointing at the test config.
func (e *testEnv) cli(args ...string) (code int, stdout, stderr string) {
	e.t.Helper()
	return runCLI(append([]string{"--config", e.cfgPath}, args...), nil)
}

func runCLI(args []string, env map[string]string) (code int, stdout, stderr string) {
	var so, se bytes.Buffer
	code = run(context.Background(), args, &so, &se, func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	})
	return code, so.String(), se.String()
}

func (e *testEnv) openState() *state.Store {
	e.t.Helper()
	st, err := state.Open(context.Background(), filepath.Join(e.stateDir, "state.db"))
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { st.Close() })
	return st
}

func wantCode(t *testing.T, got, want int, stdout, stderr string) {
	t.Helper()
	if got != want {
		t.Fatalf("exit code = %d, want %d\nstdout:\n%s\nstderr:\n%s", got, want, stdout, stderr)
	}
}

func TestVersion(t *testing.T) {
	code, out, errOut := runCLI([]string{"version"}, nil)
	wantCode(t, code, 0, out, errOut)
	if !strings.Contains(out, "gitmirror") || !strings.Contains(out, "go1") {
		t.Fatalf("out = %q", out)
	}
	code, out, errOut = runCLI([]string{"version", "--json"}, nil)
	wantCode(t, code, 0, out, errOut)
	var v map[string]string
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if v["version"] == "" || !strings.HasPrefix(v["go"], "go") {
		t.Fatalf("v = %v", v)
	}
}

func TestHelpListsCommandsAndExitCodes(t *testing.T) {
	code, out, errOut := runCLI([]string{"--help"}, nil)
	wantCode(t, code, 0, out, errOut)
	for _, w := range []string{"sync", "plan", "validate", "status", "version", "Exit codes", "partial"} {
		if !strings.Contains(out, w) {
			t.Fatalf("help lacks %q:\n%s", w, out)
		}
	}
}

func TestUnknownCommandAndFlagAreUsageErrors(t *testing.T) {
	for _, args := range [][]string{{"bogus"}, {"sync", "--nope"}, {"--log-level", "loud", "version"}} {
		code, out, errOut := runCLI(args, nil)
		wantCode(t, code, 2, out, errOut)
	}
}

func TestConfigFromEnv(t *testing.T) {
	e := newTestEnv(t)
	code, out, errOut := runCLI([]string{"validate", "--offline"}, map[string]string{"GITMIRROR_CONFIG": e.cfgPath})
	wantCode(t, code, 0, out, errOut)
}

func TestValidateOfflineGood(t *testing.T) {
	e := newTestEnv(t)
	code, out, errOut := e.cli("validate", "--offline")
	wantCode(t, code, 0, out, errOut)
	if !strings.Contains(out, "git: ") && strings.Contains(out, "git-lfs") {
		t.Fatalf("no git version in output: %q", out)
	}
}

func TestValidateOfflineBadConfig(t *testing.T) {
	e := newTestEnv(t)
	bad := `
forges:
  a: {type: nope, token: x}
  b: {type: fake}
rules:
  - name: r
    from: {forge: missing, owner: x}
    to: {forge: a}
`
	if err := os.WriteFile(e.cfgPath, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := e.cli("validate", "--offline")
	wantCode(t, code, 2, out, errOut)
	all := out + errOut
	for _, w := range []string{"forges.a: unknown type", "forges.b: one of token", "from.forge", "to: exactly one"} {
		if !strings.Contains(all, w) {
			t.Fatalf("output lacks %q:\n%s", w, all)
		}
	}
	// Each problem on its own line.
	if n := strings.Count(all, "forges."); n < 2 {
		t.Fatalf("problems not each printed: %s", all)
	}
}

func TestMissingConfigFileIsUsageError(t *testing.T) {
	code, out, errOut := runCLI([]string{"--config", filepath.Join(t.TempDir(), "nope.yaml"), "sync"}, nil)
	wantCode(t, code, 2, out, errOut)
}

func TestValidateOnline(t *testing.T) {
	e := newTestEnv(t)
	e.addSource("alice", "a")
	code, out, errOut := e.cli("validate")
	wantCode(t, code, 0, out, errOut)
	for _, w := range []string{"ok as u", "r1", "complete"} {
		if !strings.Contains(out, w) {
			t.Fatalf("output lacks %q:\n%s", w, out)
		}
	}
}

func TestValidateOnlineIncompleteRuleExits1(t *testing.T) {
	e := newTestEnv(t)
	e.src.FailAlways("ListRepos", errors.New("listing down"))
	code, out, errOut := e.cli("validate")
	wantCode(t, code, 1, out, errOut)
	if !strings.Contains(out, "listing down") {
		t.Fatalf("rule error not printed:\n%s", out)
	}
}

func TestSyncOK(t *testing.T) {
	e := newTestEnv(t)
	e.addSource("alice", "a")
	e.addSource("alice", "b")
	code, out, errOut := e.cli("sync")
	wantCode(t, code, 0, out, errOut)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 3 {
		t.Fatalf("out:\n%s", out)
	}
	for _, w := range []string{"ok", "src:alice/a", "→", "dst:mirrors/a", "git:ok", "src:alice/b"} {
		if !strings.Contains(out, w) {
			t.Fatalf("output lacks %q:\n%s", w, out)
		}
	}
	if !strings.Contains(lines[len(lines)-1], "2 pairs") {
		t.Fatalf("summary line = %q", lines[len(lines)-1])
	}
	t.Logf("sample sync output:\n%s", out)
	if _, err := e.dst.GetRepo(context.Background(), "mirrors", "a"); err != nil {
		t.Fatal(err)
	}
}

func TestSyncFailedPairExits1(t *testing.T) {
	e := newTestEnv(t)
	e.addSource("alice", "a")
	e.dst.FailAlways("CreateRepo", errors.New("create denied"))
	code, out, errOut := e.cli("sync")
	wantCode(t, code, 1, out, errOut)
	if !strings.Contains(out, "failed") || !strings.Contains(out, "create denied") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestSyncPartialExits3(t *testing.T) {
	e := newTestEnv(t)
	e.addSource("alice", "a")
	rel, err := e.src.CreateRelease(context.Background(), "alice", "a", forge.Release{Tag: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.src.UploadAsset(context.Background(), "alice", "a", rel.ID, forge.Asset{Name: "x.bin"}, strings.NewReader("data")); err != nil {
		t.Fatal(err)
	}
	e.dst.FailNext("UploadAsset", errors.New("upload boom"))
	code, out, errOut := e.cli("sync")
	wantCode(t, code, 3, out, errOut)
	if !strings.Contains(out, "partial") || !strings.Contains(out, "releases:failed") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestSyncRuleErrorExits3(t *testing.T) {
	e := newTestEnv(t)
	e.addSource("alice", "a")
	e.src.FailAlways("ListRepos", errors.New("listing down"))
	code, out, errOut := e.cli("sync")
	wantCode(t, code, 3, out, errOut)
	if !strings.Contains(out, "listing down") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestSyncUnknownRepoIsUsageError(t *testing.T) {
	e := newTestEnv(t)
	e.addSource("alice", "a")
	code, out, errOut := e.cli("sync", "--repo", "alice/zzz")
	wantCode(t, code, 2, out, errOut)
	if !strings.Contains(errOut, "alice/zzz") {
		t.Fatalf("stderr = %q", errOut)
	}
}

func TestSyncRepoFlagDoesNotReconcile(t *testing.T) {
	e := newTestEnv(t)
	e.addSource("alice", "a")
	e.addSource("alice", "b")
	if code, o, se := e.cli("sync"); code != 0 {
		wantCode(t, code, 0, o, se)
	}
	e.src.RemoveRepo("alice", "b")
	code, out, errOut := e.cli("sync", "--repo", "alice/a")
	wantCode(t, code, 0, out, errOut)
	st := e.openState()
	k := state.PairKey{Rule: "r1", SrcForge: "src", SrcRepo: "alice/b", DstForge: "dst", DstRepo: "mirrors/b"}
	if _, ok, _ := st.MissingSince(context.Background(), k); ok {
		t.Fatal("--repo sync started a deletion clock")
	}
}

func TestPlanPrintsActionsAndWritesNothing(t *testing.T) {
	e := newTestEnv(t)
	e.addSource("alice", "a")
	code, out, errOut := e.cli("plan")
	wantCode(t, code, 0, out, errOut)
	if !strings.Contains(out, "create repository dst:mirrors/a") {
		t.Fatalf("plan output lacks the create action:\n%s", out)
	}
	if _, err := e.dst.GetRepo(context.Background(), "mirrors", "a"); err == nil {
		t.Fatal("plan created the destination")
	}
	runs, err := e.openState().RecentRuns(context.Background(), 0)
	if err != nil || len(runs) != 0 {
		t.Fatalf("runs = %+v, err %v", runs, err)
	}
}

func TestStatusBeforeAndAfterSync(t *testing.T) {
	e := newTestEnv(t)
	// No providers must be needed: make building them impossible.
	appOptions = func() app.Options { panic("status must not build the app") }
	code, out, errOut := e.cli("status")
	wantCode(t, code, 0, out, errOut)
	if !strings.Contains(out, "no state yet") {
		t.Fatalf("out = %q", out)
	}

	e.addSource("alice", "a")
	old := appOptions
	appOptions = func() app.Options {
		return app.Options{Providers: map[string]forge.Provider{"src": e.src, "dst": e.dst}, GitEnv: e.gitEnv}
	}
	if code, o, se := e.cli("sync"); code != 0 {
		wantCode(t, code, 0, o, se)
	}
	appOptions = old

	code, out, errOut = e.cli("status", "--runs", "5")
	wantCode(t, code, 0, out, errOut)
	for _, w := range []string{"alice/a", "mirrors/a", "ok"} {
		if !strings.Contains(out, w) {
			t.Fatalf("status lacks %q:\n%s", w, out)
		}
	}
	code, out, errOut = e.cli("status", "--json", "--runs", "5")
	wantCode(t, code, 0, out, errOut)
	var doc struct {
		Pairs []map[string]any `json:"pairs"`
		Runs  []map[string]any `json:"runs"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || len(doc.Pairs) != 1 || len(doc.Runs) != 1 {
		t.Fatalf("doc = %+v err %v\n%s", doc, err, out)
	}
}

func TestStatusTruncatesLastError(t *testing.T) {
	e := newTestEnv(t)
	e.addSource("alice", "a")
	e.dst.FailAlways("CreateRepo", errors.New(strings.Repeat("x", 300)))
	e.cli("sync")
	code, out, errOut := e.cli("status")
	wantCode(t, code, 0, out, errOut)
	if strings.Contains(out, strings.Repeat("x", 100)) {
		t.Fatalf("error not truncated:\n%s", out)
	}
	if !strings.Contains(out, strings.Repeat("x", 30)) {
		t.Fatalf("error missing:\n%s", out)
	}
}

func TestJSONOutputsParse(t *testing.T) {
	e := newTestEnv(t)
	e.addSource("alice", "a")
	for _, args := range [][]string{{"sync", "--json"}, {"plan", "--json"}, {"validate", "--json"}, {"validate", "--offline", "--json"}} {
		code, out, errOut := e.cli(args...)
		wantCode(t, code, 0, out, errOut)
		var v map[string]any
		if err := json.Unmarshal([]byte(out), &v); err != nil {
			t.Fatalf("%v: stdout is not one JSON document: %v\n%s", args, err, out)
		}
	}
	code, out, errOut := e.cli("sync", "--json")
	wantCode(t, code, 0, out, errOut)
	var doc struct {
		Pairs []struct {
			Status string `json:"status"`
			Source string `json:"source"`
		} `json:"pairs"`
		RuleErrors []any `json:"rule_errors"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || len(doc.Pairs) != 1 || doc.Pairs[0].Status != "ok" {
		t.Fatalf("doc = %+v err %v", doc, err)
	}
}

func TestTokenNeverPrinted(t *testing.T) {
	e := newTestEnv(t)
	e.addSource("alice", "a")
	rel, _ := e.src.CreateRelease(context.Background(), "alice", "a", forge.Release{Tag: "v1"})
	e.src.UploadAsset(context.Background(), "alice", "a", rel.ID, forge.Asset{Name: "x.bin"}, strings.NewReader("d"))
	e.dst.FailNext("UploadAsset", errors.New("boom"))

	variants := [][]string{
		{"validate", "--offline"},
		{"validate"},
		{"validate", "--json"},
		{"plan"},
		{"plan", "--json"},
		{"sync", "--log-level", "debug"},
		{"sync", "--json", "--log-level", "debug", "--log-format", "json"},
		{"sync", "--log-level", "debug"},
		{"status", "--runs", "10"},
		{"status", "--json", "--runs", "10"},
		{"version"},
		{"--help"},
		{"sync", "--repo", "alice/nothere"},
	}
	for _, args := range variants {
		code, out, errOut := e.cli(args...)
		_ = code
		if strings.Contains(out, sentinel) || strings.Contains(errOut, sentinel) {
			t.Fatalf("token leaked by %v:\nstdout:\n%s\nstderr:\n%s", args, out, errOut)
		}
	}
	// A bad config with the token in it must not echo it either.
	bad := strings.Replace(cfgYAML, "type: fake, token: @TOKEN@}\n  dst", "type: bogus, token: "+sentinel+"}\n  dst", 1)
	bad = strings.NewReplacer("@STATE@", "x", "@TOKEN@", sentinel).Replace(bad)
	if err := os.WriteFile(e.cfgPath, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := e.cli("validate", "--offline")
	wantCode(t, code, 2, out, errOut)
	if strings.Contains(out, sentinel) || strings.Contains(errOut, sentinel) {
		t.Fatalf("token leaked by bad config:\n%s\n%s", out, errOut)
	}
}
