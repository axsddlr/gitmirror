package engine

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitmirror/internal/config"
	"gitmirror/internal/forge"
	"gitmirror/internal/forge/fake"
	"gitmirror/internal/gitx"
	"gitmirror/internal/resolve"
	"gitmirror/internal/state"
)

var allFeatures = []string{"git", "lfs", "settings", "releases", "wiki"}

// readOnlyCalls are fake methods that never modify the forge.
var readOnlyCalls = map[string]bool{
	"Type": true, "GetRepo": true, "ListRepos": true, "GitURL": true, "GitAuth": true,
	"RateLimit": true, "WikiGitURL": true, "ListReleases": true, "ListStarred": true,
	"DownloadAsset": true,
}

type harness struct {
	t        *testing.T
	env      []string
	srcBase  string
	dstBase  string
	src, dst *fake.Provider
	store    *state.Store
	eng      *Engine
	n        int
}

func gitEnv(t *testing.T) []string {
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

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, env: gitEnv(t)}
	h.srcBase = filepath.Join(t.TempDir(), "src")
	h.dstBase = filepath.Join(t.TempDir(), "dst")
	// Tokens must not be substrings of git output: gitx scrubs them from
	// stdout, so a one-letter token would corrupt ref names.
	h.src = fake.New(fake.Options{Type: "fakesrc", GitBaseDir: h.srcBase, Username: "u", Token: "srctoken-1234"})
	h.dst = fake.New(fake.Options{Type: "fakedst", GitBaseDir: h.dstBase, Username: "u", Token: "dsttoken-5678"})
	st, err := state.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h.store = st
	h.eng = &Engine{
		Providers: map[string]forge.Provider{"src": h.src, "dst": h.dst},
		Store:     st,
		Git:       &gitx.Runner{Env: h.env, Timeout: 2 * time.Minute},
		CacheDir:  filepath.Join(t.TempDir(), "cache"),
	}
	return h
}

// tgit runs git directly for fixture setup.
func (h *harness) tgit(dir string, args ...string) string {
	h.t.Helper()
	full := append([]string{
		"-c", "user.name=t", "-c", "user.email=t@t",
		"-c", "init.defaultBranch=main", "-c", "commit.gpgsign=false",
		"-c", "tag.gpgsign=false", "-c", "core.autocrlf=false",
	}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	cmd.Env = append(cleanHostEnv(), h.env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		h.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func fileURL(p string) string {
	s := filepath.ToSlash(p)
	if !strings.HasPrefix(s, "/") {
		s = "/" + s
	}
	return (&url.URL{Scheme: "file", Path: s}).String()
}

func bareDir(base, owner, name string, wiki bool) string {
	suffix := ".git"
	if wiki {
		suffix = ".wiki.git"
	}
	return filepath.Join(base, strings.ToLower(owner), strings.ToLower(name)+suffix)
}

// srcRepo is a seeded source repository plus a work tree pushing to it.
type srcRepo struct {
	h    *harness
	repo forge.Repo
	bare string
	work string
}

// addSource seeds a source repo (owner "alice") and a work clone of it.
func (h *harness) addSource(r forge.Repo) *srcRepo {
	h.t.Helper()
	if r.Owner == "" {
		r.Owner = "alice"
	}
	if r.DefaultBranch == "" {
		r.DefaultBranch = "main"
	}
	if err := h.src.AddRepo(r); err != nil {
		h.t.Fatal(err)
	}
	s := &srcRepo{h: h, repo: r, bare: bareDir(h.srcBase, r.Owner, r.Name, false)}
	s.work = filepath.Join(h.t.TempDir(), "work")
	h.tgit(filepath.Dir(s.work), "init", "-q", "-b", "main", s.work)
	h.tgit(s.work, "remote", "add", "origin", fileURL(s.bare))
	return s
}

func (s *srcRepo) git(args ...string) string {
	s.h.t.Helper()
	return s.h.tgit(s.work, args...)
}

// commit writes a unique file and commits it, returning HEAD.
func (s *srcRepo) commit(name string) string {
	s.h.t.Helper()
	s.h.n++
	content := fmt.Sprintf("%s %d %d\n", name, s.h.n, time.Now().UnixNano())
	if err := os.WriteFile(filepath.Join(s.work, name), []byte(content), 0o644); err != nil {
		s.h.t.Fatal(err)
	}
	s.git("add", name)
	s.git("commit", "-q", "-m", "commit "+name)
	return s.git("rev-parse", "HEAD")
}

func (s *srcRepo) push(args ...string) {
	s.h.t.Helper()
	s.git(append([]string{"push", "-q", "origin"}, args...)...)
}

// refs lists refs/heads and refs/tags of a bare repo.
func (h *harness) refs(bare string) map[string]string {
	h.t.Helper()
	out := h.tgit(filepath.Dir(bare), "--git-dir="+bare, "for-each-ref", "--format=%(objectname) %(refname)", "refs/heads", "refs/tags")
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

func (h *harness) dstRefs(owner, name string) map[string]string {
	return h.refs(bareDir(h.dstBase, owner, name, false))
}

func equalRefs(t *testing.T, what string, got, want map[string]string) {
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

func settings(include ...string) config.RuleSettings {
	if len(include) == 0 {
		include = allFeatures
	}
	return config.RuleSettings{
		Interval:        time.Hour,
		Include:         include,
		Visibility:      "private",
		OnSourceDeleted: "keep",
	}
}

func (h *harness) pair(s *srcRepo, rs config.RuleSettings) resolve.Pair {
	return resolve.Pair{
		Rule:     "r1",
		Settings: rs,
		SrcForge: "src",
		Src:      s.repo,
		DstForge: "dst",
		DstOwner: "mirrors",
		DstName:  s.repo.Name,
	}
}

func (h *harness) sync(p resolve.Pair, opt Options) Report {
	h.t.Helper()
	if opt.Trigger == "" {
		opt.Trigger = "cli"
	}
	return h.eng.SyncPair(context.Background(), p, opt)
}

func step(r Report, name string) state.StepResult {
	for _, s := range r.Steps {
		if s.Step == name {
			return s
		}
	}
	return state.StepResult{Step: name, Status: "<missing>"}
}

func wantStatus(t *testing.T, r Report, want state.RunStatus) {
	t.Helper()
	if r.Status != want {
		t.Fatalf("status = %s, want %s; err=%v steps=%+v actions=%q", r.Status, want, r.Err, r.Steps, r.Actions)
	}
}

func wantStep(t *testing.T, r Report, name string, want state.StepStatus) state.StepResult {
	t.Helper()
	s := step(r, name)
	if s.Status != want {
		t.Fatalf("step %s = %s (%q), want %s; all steps=%+v err=%v", name, s.Status, s.Detail, want, r.Steps, r.Err)
	}
	return s
}

func (h *harness) dstRepo(owner, name string) *forge.Repo {
	h.t.Helper()
	r, err := h.dst.GetRepo(context.Background(), owner, name)
	if err != nil {
		h.t.Fatalf("dst GetRepo %s/%s: %v", owner, name, err)
	}
	return r
}

func writeCalls(calls []string) []string {
	var out []string
	for _, c := range calls {
		if !readOnlyCalls[c] {
			out = append(out, c)
		}
	}
	return out
}

func indexOf(calls []string, name string) int {
	for i, c := range calls {
		if c == name {
			return i
		}
	}
	return -1
}

func lastIndexOf(calls []string, name string) int {
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i] == name {
			return i
		}
	}
	return -1
}

func hasAction(r Report, substr string) bool {
	for _, a := range r.Actions {
		if strings.Contains(a, substr) {
			return true
		}
	}
	return false
}

func wantAction(t *testing.T, r Report, substr string) {
	t.Helper()
	if !hasAction(r, substr) {
		t.Fatalf("no action containing %q in %q", substr, r.Actions)
	}
}

// seedRelease creates a release on the source with the given assets.
func (h *harness) seedRelease(p *fake.Provider, owner, name string, rel forge.Release, assets map[string][]byte) *forge.Release {
	h.t.Helper()
	ctx := context.Background()
	out, err := p.CreateRelease(ctx, owner, name, rel)
	if err != nil {
		h.t.Fatal(err)
	}
	for n, data := range assets {
		if _, err := p.UploadAsset(ctx, owner, name, out.ID, forge.Asset{Name: n, ContentType: "application/octet-stream"}, bytes.NewReader(data)); err != nil {
			h.t.Fatal(err)
		}
	}
	return out
}

func releaseByTag(t *testing.T, p *fake.Provider, owner, name, tag string) *forge.Release {
	t.Helper()
	rels, err := p.ListReleases(context.Background(), owner, name)
	if err != nil {
		t.Fatal(err)
	}
	for i := range rels {
		if rels[i].Tag == tag {
			return &rels[i]
		}
	}
	return nil
}

func assetByName(r *forge.Release, name string) *forge.Asset {
	for i := range r.Assets {
		if r.Assets[i].Name == name {
			return &r.Assets[i]
		}
	}
	return nil
}

func blob(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i%7)
	}
	return b
}
