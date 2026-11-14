//go:build integration

// Package integration runs gitmirror against real Gitea servers started in
// Docker. Build with -tags integration; without a reachable Docker engine
// every test skips (set GITMIRROR_REQUIRE_DOCKER=1 to make that a failure).
package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gitmirror/internal/config"
	"gitmirror/internal/engine"
	"gitmirror/internal/forge"
	_ "gitmirror/internal/forge/gitea"
	"gitmirror/internal/forge/httpx"
	"gitmirror/internal/gitx"
	"gitmirror/internal/resolve"
	"gitmirror/internal/state"
)

// giteaImage is the pinned Gitea release: 28.3.0 is the newest stable
// release (GitHub release v28.3.0, 2026-11-01; Docker Hub "latest" and "28"
// carry the same digest). Override with GITMIRROR_GITEA_IMAGE.
const giteaImage = "gitea/gitea:28.3.0"

const (
	containerLabel = "gitmirror.integration=1"
	healthTimeout  = 90 * time.Second
	adminPassword  = "Integration-Pa55word!"
	// pollTimeout bounds every wait for an asynchronous server-side effect.
	pollTimeout = 60 * time.Second
)

// dockerSkip is non-empty when Docker is unavailable and tests must skip.
var dockerSkip string

func TestMain(m *testing.M) {
	out, err := exec.Command("docker", "info", "--format", "{{.OSType}}").CombinedOutput()
	switch {
	case err != nil:
		dockerSkip = fmt.Sprintf("docker info failed: %v: %s", err, strings.TrimSpace(string(out)))
	case strings.TrimSpace(string(out)) != "linux":
		dockerSkip = "docker engine is not running Linux containers (OSType " + strings.TrimSpace(string(out)) + ")"
	}
	if dockerSkip != "" {
		if os.Getenv("GITMIRROR_REQUIRE_DOCKER") == "1" {
			fmt.Fprintln(os.Stderr, "integration: GITMIRROR_REQUIRE_DOCKER=1 but", dockerSkip)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "integration: skipping all tests:", dockerSkip)
	} else {
		removeStale()
	}
	os.Exit(m.Run())
}

// removeStale removes containers of earlier runs that were killed before
// their cleanup ran (e.g. by go test -timeout). A suite takes minutes, so a
// labelled container older than an hour cannot belong to a live run.
func removeStale() {
	out, err := exec.Command("docker", "ps", "-a", "--filter", "label="+containerLabel, "--format", "{{.ID}} {{.CreatedAt}}").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		id, created, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		at, err := time.Parse("2006-01-02 15:04:05 -0700 MST", created)
		if err == nil && time.Since(at) > time.Hour {
			_ = exec.Command("docker", "rm", "-f", id).Run()
		}
	}
}

func requireDocker(t *testing.T) {
	t.Helper()
	if dockerSkip != "" {
		t.Skip(dockerSkip)
	}
}

func image() string {
	if v := os.Getenv("GITMIRROR_GITEA_IMAGE"); v != "" {
		return v
	}
	return giteaImage
}

// ---- Gitea container ----

// instance is one running Gitea server with an admin user and token.
type instance struct {
	name  string // "A" or "B", for messages
	id    string // container id
	url   string // http://127.0.0.1:<port>
	user  string
	token string
	prov  forge.Provider
	hc    *http.Client
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

func docker(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("docker %s: %v: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// startGitea starts a container and provisions user as an admin with an
// all-scopes token. The container is removed in t.Cleanup.
func startGitea(t *testing.T, name, user string) *instance {
	t.Helper()
	ctx := context.Background()
	port := freePort(t)
	root := fmt.Sprintf("http://127.0.0.1:%d", port)
	id, err := docker(ctx, "run", "-d", "--rm",
		"--label", containerLabel,
		"-p", fmt.Sprintf("127.0.0.1:%d:3000", port),
		"-e", "GITEA__security__INSTALL_LOCK=true",
		"-e", "GITEA__server__ROOT_URL="+root+"/",
		"-e", "GITEA__server__LFS_START_SERVER=true",
		"-e", "GITEA__database__DB_TYPE=sqlite3",
		"-e", "GITEA__service__DISABLE_REGISTRATION=true",
		// Gitea 28 refuses webhook targets on non-public addresses unless
		// allowed explicitly; "*" is not enough for host.docker.internal.
		"-e", "GITEA__webhook__ALLOWED_HOST_LIST=private",
		"-e", "GITEA__log__LEVEL=Warn",
		image())
	if err != nil {
		t.Fatalf("start gitea %s: %v", name, err)
	}
	t.Cleanup(func() {
		if t.Failed() && os.Getenv("GITMIRROR_KEEP_LOGS") == "1" {
			if logs, err := docker(context.Background(), "logs", "--tail", "200", id); err == nil {
				t.Logf("gitea %s logs:\n%s", name, logs)
			}
		}
		if _, err := docker(context.Background(), "rm", "-f", id); err != nil {
			t.Logf("remove gitea %s container: %v", name, err)
		}
	})

	deadline := time.Now().Add(healthTimeout)
	for {
		resp, err := http.Get(root + "/api/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			logs, _ := docker(ctx, "logs", "--tail", "50", id)
			t.Fatalf("gitea %s not healthy after %s (last err %v)\n%s", name, healthTimeout, err, logs)
		}
		time.Sleep(500 * time.Millisecond)
	}

	if _, err := docker(ctx, "exec", "-u", "git", id, "gitea", "admin", "user", "create", "--admin",
		"--username", user, "--password", adminPassword, "--email", user+"@example.com",
		"--must-change-password=false"); err != nil {
		t.Fatalf("gitea %s: create admin: %v", name, err)
	}
	tok, err := docker(ctx, "exec", "-u", "git", id, "gitea", "admin", "user", "generate-access-token",
		"--username", user, "--token-name", "gitmirror", "--scopes", "all", "--raw")
	if err != nil {
		t.Fatalf("gitea %s: create token: %v", name, err)
	}
	if len(tok) < 20 || strings.ContainsAny(tok, " \n") {
		t.Fatalf("gitea %s: unexpected token output %q", name, tok)
	}
	in := &instance{name: name, id: id, url: root, user: user, token: tok, hc: httpx.NewClient(nil)}
	p, err := forge.New("gitea", forge.Options{URL: root, Token: tok, Username: user, HTTPClient: in.hc})
	if err != nil {
		t.Fatal(err)
	}
	in.prov = p
	return in
}

// ---- raw API access ----

// api performs a JSON API call and returns the status and raw body.
func (in *instance) api(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, in.url+"/api/v1"+path, r)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "token "+in.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := in.hc.Do(req)
	if err != nil {
		t.Fatalf("%s %s %s: %v", in.name, method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b
}

// mustAPI is api that requires one of the given statuses and decodes into out.
func (in *instance) mustAPI(t *testing.T, method, path string, body, out any, want ...int) {
	t.Helper()
	code, b := in.api(t, method, path, body)
	ok := false
	for _, w := range want {
		ok = ok || code == w
	}
	if !ok {
		t.Fatalf("%s %s %s: status %d (want %v): %s", in.name, method, path, code, want, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s %s %s: decode: %v: %s", in.name, method, path, err, b)
		}
	}
}

// createRepo creates an empty repo for the instance's user.
func (in *instance) createRepo(t *testing.T, name, desc string, private bool) {
	t.Helper()
	in.mustAPI(t, http.MethodPost, "/user/repos",
		map[string]any{"name": name, "description": desc, "private": private, "auto_init": false}, nil, http.StatusCreated)
}

func (in *instance) getRepo(t *testing.T, owner, name string) *forge.Repo {
	t.Helper()
	r, err := in.prov.GetRepo(context.Background(), owner, name)
	if err != nil {
		t.Fatalf("%s GetRepo %s/%s: %v", in.name, owner, name, err)
	}
	return r
}

// waitNonEmpty polls until Gitea has processed a push into an empty repo;
// Gitea clears its "empty" flag asynchronously after receive-pack returns.
func (in *instance) waitNonEmpty(t *testing.T, owner, name string) {
	t.Helper()
	eventually(t, "repo "+owner+"/"+name+" on "+in.name+" no longer empty", func() (bool, string) {
		r, err := in.prov.GetRepo(context.Background(), owner, name)
		if err != nil {
			return false, err.Error()
		}
		return !r.Empty, fmt.Sprintf("empty=%v", r.Empty)
	})
}

func (in *instance) gitURL(owner, name string) string {
	return in.url + "/" + owner + "/" + name + ".git"
}

func (in *instance) wikiURL(owner, name string) string {
	return in.url + "/" + owner + "/" + name + ".wiki.git"
}

// eventually polls cond every 250ms until it holds or pollTimeout elapses.
func eventually(t *testing.T, what string, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(pollTimeout)
	for {
		ok, last := cond()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s (last: %s)", what, last)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// ---- suite ----

// suite holds the two servers shared by every scenario.
type suite struct {
	a, b *instance
	git  *gitEnv
}

func newSuite(t *testing.T) *suite {
	t.Helper()
	requireDocker(t)
	s := &suite{}
	// Start both servers concurrently. A failure inside startGitea marks t
	// failed and ends only its goroutine (Goexit), leaving the field nil;
	// cleanups registered before that still run.
	var wg sync.WaitGroup
	wg.Go(func() { s.a = startGitea(t, "A", "alice") })
	wg.Go(func() { s.b = startGitea(t, "B", "bob") })
	wg.Wait()
	if s.a == nil || s.b == nil {
		t.Fatal("starting the gitea servers failed (see above)")
	}
	s.git = newGitEnv(t, s.a, s.b)
	return s
}

// ---- git for fixtures and assertions ----

// gitEnv runs git for test fixtures with an isolated configuration: a temp
// HOME (with git-lfs filters installed there), no system config, and
// per-origin basic-auth headers for both servers.
type gitEnv struct {
	t   *testing.T
	env []string
	n   int
}

// isolatedEnv returns env entries that hide the host's git configuration.
func isolatedEnv(home string) []string {
	return []string{
		"HOME=" + home,
		"USERPROFILE=" + home,
		"XDG_CONFIG_HOME=" + home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + filepath.Join(home, ".gitconfig"),
		"GIT_TERMINAL_PROMPT=0",
		"GCM_INTERACTIVE=never",
	}
}

// hostEnv is the process environment without any GIT_* variable.
func hostEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(strings.ToUpper(kv), "GIT_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func basic(user, token string) string {
	return "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+token))
}

func newGitEnv(t *testing.T, servers ...*instance) *gitEnv {
	t.Helper()
	home := t.TempDir()
	env := append(hostEnv(), isolatedEnv(home)...)
	env = append(env, "GIT_CONFIG_COUNT="+strconv.Itoa(len(servers)))
	for i, s := range servers {
		n := strconv.Itoa(i)
		env = append(env, "GIT_CONFIG_KEY_"+n+"=http."+s.url+"/.extraHeader", "GIT_CONFIG_VALUE_"+n+"="+basic(s.user, s.token))
	}
	g := &gitEnv{t: t, env: env}
	g.run("", "lfs", "install", "--skip-repo")
	return g
}

func (g *gitEnv) cmd(dir string, args ...string) (string, error) {
	full := append([]string{
		"-c", "user.name=Integration", "-c", "user.email=it@example.com",
		"-c", "init.defaultBranch=main", "-c", "commit.gpgsign=false",
		"-c", "tag.gpgsign=false", "-c", "core.autocrlf=false",
		"-c", "protocol.version=2",
	}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	cmd.Env = g.env
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (g *gitEnv) run(dir string, args ...string) string {
	g.t.Helper()
	out, err := g.cmd(dir, args...)
	if err != nil {
		g.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return out
}

// lsRemote returns refs/heads and refs/tags (including peeled ^{} entries).
func (g *gitEnv) lsRemote(t *testing.T, url string) map[string]string {
	t.Helper()
	out, err := g.cmd("", "ls-remote", "--heads", "--tags", url)
	if err != nil {
		t.Fatalf("git ls-remote %s: %v\n%s", url, err, out)
	}
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		hash, name, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if ok {
			m[name] = hash
		}
	}
	return m
}

// work is a work tree whose origin is a repository on a server.
type work struct {
	g   *gitEnv
	dir string
}

func (g *gitEnv) newWork(t *testing.T, branch, url string) *work {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "work")
	g.run("", "init", "-q", "-b", branch, dir)
	g.run(dir, "remote", "add", "origin", url)
	return &work{g: g, dir: dir}
}

func (w *work) git(args ...string) string {
	w.g.t.Helper()
	return w.g.run(w.dir, args...)
}

// commit writes a unique file and commits it, returning the new HEAD.
func (w *work) commit(file string) string {
	w.g.t.Helper()
	w.g.n++
	content := fmt.Sprintf("%s %d %d\n", file, w.g.n, time.Now().UnixNano())
	if err := os.WriteFile(filepath.Join(w.dir, file), []byte(content), 0o644); err != nil {
		w.g.t.Fatal(err)
	}
	w.git("add", file)
	w.git("commit", "-q", "-m", "commit "+file)
	return w.git("rev-parse", "HEAD")
}

func (w *work) push(args ...string) {
	w.g.t.Helper()
	w.git(append([]string{"push", "-q", "origin"}, args...)...)
}

func equalRefs(t *testing.T, what string, got, want map[string]string) {
	t.Helper()
	if len(want) == 0 {
		t.Fatalf("%s: expected refs are empty", what)
	}
	if len(got) != len(want) {
		t.Fatalf("%s: got %d refs %v, want %d refs %v", what, len(got), got, len(want), want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s: ref %s = %q, want %q", what, k, got[k], v)
		}
	}
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func randomName(prefix string) string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

// ---- gitmirror wiring ----

// mirror is one gitmirror deployment: a config, providers for both servers,
// a state store and an engine.
type mirror struct {
	t   *testing.T
	s   *suite
	cfg *config.Config
	eng *engine.Engine
}

// newMirror parses rulesYAML (the "rules:" list body) into a config with
// forges a and b and builds an engine with a fresh state store.
func (s *suite) newMirror(t *testing.T, rulesYAML string) *mirror {
	t.Helper()
	dir := t.TempDir()
	st, err := state.Open(context.Background(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := &mirror{t: t, s: s}
	m.cfg = s.parseConfig(t, dir, rulesYAML)
	providers := map[string]forge.Provider{}
	for name, f := range m.cfg.Forges {
		p, err := forge.New(f.Type, forge.Options{URL: f.URL, Username: f.Username, Token: f.Token, HTTPClient: httpx.NewClient(nil)})
		if err != nil {
			t.Fatal(err)
		}
		providers[name] = p
	}
	m.eng = &engine.Engine{
		Providers:   providers,
		Store:       st,
		Git:         &gitx.Runner{Env: isolatedEnv(t.TempDir()), Timeout: 5 * time.Minute},
		CacheDir:    filepath.Join(dir, "cache"),
		Concurrency: 2,
	}
	return m
}

func (s *suite) parseConfig(t *testing.T, dir, rulesYAML string) *config.Config {
	t.Helper()
	doc := fmt.Sprintf(`state_dir: %q
concurrency: 2
forges:
  a:
    type: gitea
    url: %q
    token: %q
    username: %q
  b:
    type: gitea
    url: %q
    token: %q
    username: %q
defaults:
  interval: 1h
  visibility: private
rules:
%s`, filepath.ToSlash(dir), s.a.url, s.a.token, s.a.user, s.b.url, s.b.token, s.b.user, rulesYAML)
	cfg, err := config.Parse([]byte(doc), func(string) (string, bool) { return "", false }, os.ReadFile)
	if err != nil {
		t.Fatalf("config.Parse: %v\n%s", err, strings.ReplaceAll(strings.ReplaceAll(doc, s.a.token, "<A-token>"), s.b.token, "<B-token>"))
	}
	return cfg
}

// setRules swaps the configuration (same engine and state store).
func (m *mirror) setRules(rulesYAML string) {
	m.t.Helper()
	m.cfg = m.s.parseConfig(m.t, m.t.TempDir(), rulesYAML)
}

// run resolves the rules, syncs every pair, reconciles, and returns the sync
// reports keyed by destination full name. Resolve errors fail the test.
func (m *mirror) run() map[string]engine.Report {
	m.t.Helper()
	ctx := context.Background()
	res := resolve.Resolve(ctx, m.cfg, m.eng.Providers)
	if len(res.Errors) > 0 {
		m.t.Fatalf("resolve errors: %v", res.Errors)
	}
	if len(res.Pairs) == 0 {
		m.t.Fatalf("resolve produced no pairs")
	}
	reps := m.eng.Run(ctx, res.Pairs, engine.Options{Trigger: "cli"})
	settings := map[string]config.RuleSettings{}
	for _, r := range m.cfg.Rules {
		settings[r.Name] = r.Effective(m.cfg.Defaults)
	}
	for _, rr := range m.eng.Reconcile(ctx, res.Pairs, res.Complete, res.Dropped, settings, engine.Options{Trigger: "cli"}) {
		m.t.Logf("reconcile %s: %s %v", rr.Key, rr.Status, rr.Err)
	}
	out := map[string]engine.Report{}
	for i, p := range res.Pairs {
		out[p.DstForge+":"+p.DstFullName()] = reps[i]
		m.t.Logf("run %s -> %s:%s: %s err=%v", p.Key().SrcRepo, p.DstForge, p.DstFullName(), reps[i].Status, reps[i].Err)
		for _, st := range reps[i].Steps {
			m.t.Logf("    %-9s %-9s %s", st.Step, st.Status, st.Detail)
		}
	}
	return out
}

// one runs and returns the single report for dst ("b:owner/name").
func (m *mirror) one(dst string) engine.Report {
	m.t.Helper()
	reps := m.run()
	r, ok := reps[dst]
	if !ok {
		m.t.Fatalf("no report for %s (have %d reports)", dst, len(reps))
	}
	return r
}

func step(r engine.Report, name string) state.StepResult {
	for _, s := range r.Steps {
		if s.Step == name {
			return s
		}
	}
	return state.StepResult{Step: name, Status: "<missing>"}
}

func wantStatus(t *testing.T, r engine.Report, want state.RunStatus) {
	t.Helper()
	if r.Status != want {
		t.Fatalf("%s: status %s, want %s; err=%v steps=%+v", r.Key, r.Status, want, r.Err, r.Steps)
	}
}

func wantStep(t *testing.T, r engine.Report, name string, want state.StepStatus) state.StepResult {
	t.Helper()
	s := step(r, name)
	if s.Status != want {
		t.Fatalf("%s: step %s = %s (%q), want %s; steps=%+v", r.Key, name, s.Status, s.Detail, want, r.Steps)
	}
	return s
}
