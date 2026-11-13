package gitx

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMain doubles as a fake git binary: when GITX_FAKE_GIT=1 the test
// executable prints its auth-bearing environment to stderr (raw and decoded)
// and fails, which lets tests prove that git error output is scrubbed.
func TestMain(m *testing.M) {
	if os.Getenv("GITX_FAKE_GIT") == "1" {
		fakeGit()
		return
	}
	os.Exit(m.Run())
}

func fakeGit() {
	fmt.Fprintf(os.Stderr, "fatal: fake git args=%q\n", os.Args[1:])
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(kv), "GIT_CONFIG_VALUE_") {
			continue
		}
		fmt.Fprintln(os.Stderr, "env:", kv)
		if i := strings.Index(kv, "Basic "); i >= 0 {
			if raw, err := base64.StdEncoding.DecodeString(kv[i+len("Basic "):]); err == nil {
				fmt.Fprintln(os.Stderr, "decoded:", string(raw))
			}
		}
	}
	os.Exit(128)
}

// testEnv isolates git from the developer's global/system configuration.
func testEnv(t *testing.T) []string {
	t.Helper()
	home := t.TempDir()
	return []string{
		"HOME=" + home,
		"USERPROFILE=" + home,
		"XDG_CONFIG_HOME=" + home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + filepath.Join(home, ".gitconfig"),
	}
}

func cleanHostEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(strings.ToUpper(kv), "GIT_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// tgit runs git directly (not through Runner) for fixture setup.
func tgit(t *testing.T, env []string, dir string, args ...string) string {
	t.Helper()
	full := append([]string{
		"-c", "user.name=Test", "-c", "user.email=test@example.com",
		"-c", "init.defaultBranch=main", "-c", "commit.gpgsign=false",
		"-c", "tag.gpgsign=false", "-c", "core.autocrlf=false",
	}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	cmd.Env = append(cleanHostEnv(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func fileURL(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	s := filepath.ToSlash(abs)
	if !strings.HasPrefix(s, "/") {
		s = "/" + s
	}
	return (&url.URL{Scheme: "file", Path: s}).String()
}

type fixture struct {
	t                     *testing.T
	env                   []string
	root                  string
	src, dst, work, cache string
	r                     *Runner
	c                     *Cache
	n                     int
	mu                    sync.Mutex
	cmds                  []*exec.Cmd
	srcRemote, dstRemote  Remote
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{t: t, env: testEnv(t), root: root}
	f.src = filepath.Join(root, "src.git")
	f.dst = filepath.Join(root, "dst.git")
	f.work = filepath.Join(root, "work")
	f.cache = filepath.Join(root, "cache", "pair.git")
	tgit(t, f.env, root, "init", "-q", "--bare", f.src)
	tgit(t, f.env, root, "init", "-q", "--bare", f.dst)
	tgit(t, f.env, root, "init", "-q", f.work)
	tgit(t, f.env, f.work, "remote", "add", "origin", fileURL(t, f.src))
	f.r = &Runner{Env: f.env}
	f.r.hook = func(c *exec.Cmd) {
		f.mu.Lock()
		f.cmds = append(f.cmds, c)
		f.mu.Unlock()
	}
	f.c = &Cache{Dir: f.cache, R: f.r}
	f.srcRemote = Remote{URL: fileURL(t, f.src)}
	f.dstRemote = Remote{URL: fileURL(t, f.dst)}
	return f
}

func (f *fixture) git(args ...string) string {
	f.t.Helper()
	return tgit(f.t, f.env, f.work, args...)
}

// commit writes a unique file and commits it, returning the new HEAD hash.
func (f *fixture) commit(name string) string {
	f.t.Helper()
	f.n++
	content := fmt.Sprintf("%s %d %d\n", name, f.n, time.Now().UnixNano())
	if err := os.WriteFile(filepath.Join(f.work, name), []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	f.git("add", name)
	f.git("commit", "-q", "-m", "commit "+name)
	return f.git("rev-parse", "HEAD")
}

func (f *fixture) push(args ...string) {
	f.t.Helper()
	f.git(append([]string{"push", "-q", "origin"}, args...)...)
}

func (f *fixture) sync(opt SyncOptions) *SyncResult {
	f.t.Helper()
	res, err := f.c.Sync(context.Background(), f.srcRemote, f.dstRemote, opt)
	if err != nil {
		f.t.Fatalf("Sync: %v", err)
	}
	return res
}

func (f *fixture) resetCmds() {
	f.mu.Lock()
	f.cmds = nil
	f.mu.Unlock()
}

func (f *fixture) captured() []*exec.Cmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*exec.Cmd(nil), f.cmds...)
}

// ranSub reports whether any captured command used the given subcommand word.
func ranSub(cmds []*exec.Cmd, sub string) bool {
	for _, c := range cmds {
		for _, a := range c.Args[1:] {
			if a == sub {
				return true
			}
		}
	}
	return false
}

// allRefs lists every ref (any namespace) in a bare repo.
func allRefs(t *testing.T, env []string, bare string) map[string]string {
	t.Helper()
	out := tgit(t, env, bare, "for-each-ref", "--format=%(objectname) %(refname)")
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		hash, name, _ := strings.Cut(line, " ")
		m[name] = hash
	}
	return m
}

// refsOf lists refs/heads/* and refs/tags/* in a bare repo.
func refsOf(t *testing.T, env []string, bare string) map[string]string {
	t.Helper()
	m := allRefs(t, env, bare)
	for k := range m {
		if !strings.HasPrefix(k, "refs/heads/") && !strings.HasPrefix(k, "refs/tags/") {
			delete(m, k)
		}
	}
	return m
}

func equalStrings(t *testing.T, what string, got, want []string) {
	t.Helper()
	g := append([]string(nil), got...)
	w := append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	if strings.Join(g, ",") != strings.Join(w, ",") {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

func equalMaps(t *testing.T, what string, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s: ref %s = %q, want %q (got %v)", what, k, got[k], v, got)
		}
	}
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// secretForms returns every encoding of a credential that must never leak.
func secretForms(user, token string) []string {
	if user == "" {
		user = "x-access-token"
	}
	return []string{
		token,
		base64.StdEncoding.EncodeToString([]byte(user + ":" + token)),
	}
}

// startGitHTTP serves bare repos under root over smart HTTP via
// git-http-backend, requiring exactly one Authorization header matching the
// per-repo "user:token" in creds.
func startGitHTTP(t *testing.T, root string, creds map[string]string) string {
	t.Helper()
	execPath := tgit(t, nil, root, "--exec-path")
	backend := filepath.Join(filepath.FromSlash(execPath), "git-http-backend")
	if _, err := os.Stat(backend + ".exe"); err == nil {
		backend += ".exe"
	} else if _, err := os.Stat(backend); err != nil {
		t.Skipf("git-http-backend not found under %s", execPath)
	}
	home := t.TempDir()
	h := &cgi.Handler{
		Path: backend,
		Root: "/",
		Dir:  root,
		Env: []string{
			"GIT_PROJECT_ROOT=" + root,
			"GIT_HTTP_EXPORT_ALL=1",
			"REMOTE_USER=tester",
			"GIT_CONFIG_NOSYSTEM=1",
			"HOME=" + home,
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		repo, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		want, ok := creds[repo]
		got := r.Header.Values("Authorization")
		if !ok || len(got) != 1 || got[0] != "Basic "+base64.StdEncoding.EncodeToString([]byte(want)) {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// assertNoSecretsOnDisk walks dirs and fails if any file contains a secret.
func assertNoSecretsOnDisk(t *testing.T, secrets []string, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		_ = filepath.WalkDir(d, func(p string, de fs.DirEntry, err error) error {
			if err != nil || de.IsDir() {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return nil
			}
			for _, s := range secrets {
				if strings.Contains(string(b), s) {
					t.Errorf("secret %q found on disk in %s", s, p)
				}
			}
			return nil
		})
	}
}

// assertCmdHygiene checks argv carries no secret and that secrets appear in
// the environment only base64-encoded inside GIT_CONFIG_VALUE_* entries.
func assertCmdHygiene(t *testing.T, cmds []*exec.Cmd, user, token string) {
	t.Helper()
	forms := secretForms(user, token)
	for _, c := range cmds {
		for _, a := range c.Args {
			for _, s := range forms {
				if strings.Contains(a, s) {
					t.Errorf("argv %q contains secret", c.Args)
				}
			}
		}
		for _, kv := range c.Env {
			if strings.Contains(kv, token) {
				t.Errorf("raw token in env entry %q", kv)
			}
			if strings.Contains(kv, forms[1]) && !strings.HasPrefix(strings.ToUpper(kv), "GIT_CONFIG_VALUE_") {
				t.Errorf("encoded secret outside GIT_CONFIG_VALUE_*: %q", kv)
			}
		}
	}
}

func cmdHasSecret(c *exec.Cmd, user, token string) bool {
	enc := secretForms(user, token)[1]
	for _, kv := range c.Env {
		if strings.Contains(kv, enc) {
			return true
		}
	}
	return false
}

func cmdMentions(c *exec.Cmd, s string) bool {
	for _, a := range c.Args {
		if a == s {
			return true
		}
	}
	return false
}
