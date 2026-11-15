package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gitmirror/internal/app"
	"gitmirror/internal/config"
	"gitmirror/internal/forge"
	"gitmirror/internal/forge/fake"
	"gitmirror/internal/state"
)

const (
	sentinel = "SENTINEL-TOKEN-5e2b11"
	secret   = "SENTINEL-HOOK-SECRET-93ad"
)

func init() {
	if !slices.Contains(config.KnownForgeTypes, "fake") {
		config.KnownForgeTypes = append(config.KnownForgeTypes, "fake")
	}
}

// ---- fake clock ----

type waiter struct {
	at time.Time
	ch chan time.Time
}

type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []waiter
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, waiter{c.now.Add(d), ch})
	return ch
}

// Advance moves the clock and fires every waiter that became due.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if !w.at.After(c.now) {
			w.ch <- c.now
		} else {
			kept = append(kept, w)
		}
	}
	c.waiters = kept
}

// Waiters returns how many After channels have not fired yet.
func (c *fakeClock) Waiters() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

// ---- gated provider ----

// gate wraps a fake provider; while armed, GetRepo blocks until released or
// its context is cancelled. Embedding keeps the optional interfaces.
type gate struct {
	*fake.Provider
	mu      sync.Mutex
	armed   bool
	release chan struct{}
	entered chan struct{}
}

func newGate(p *fake.Provider) *gate {
	return &gate{Provider: p, entered: make(chan struct{}, 16)}
}

func (g *gate) arm() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.armed = true
	g.release = make(chan struct{})
}

func (g *gate) open() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.armed {
		g.armed = false
		close(g.release)
	}
}

func (g *gate) GetRepo(ctx context.Context, owner, name string) (*forge.Repo, error) {
	g.mu.Lock()
	armed, rel := g.armed, g.release
	g.mu.Unlock()
	if armed {
		select {
		case g.entered <- struct{}{}:
		default:
		}
		select {
		case <-rel:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return g.Provider.GetRepo(ctx, owner, name)
}

// plain hides every optional interface (notably WebhookProvider).
type plain struct{ forge.Provider }

// ---- fixture ----

type fixture struct {
	t        *testing.T
	gitEnv   []string
	src      *gate
	dst      *fake.Provider
	stateDir string
	clock    *fakeClock
	app      *app.App

	mu   sync.Mutex
	jobs []doneJob
}

type doneJob struct {
	Lane string
	Job  Job
}

const cfgTmpl = `
state_dir: '@STATE@'
concurrency: 2
forges:
  src: {type: fake, token: @TOKEN@}
  dst: {type: fake, token: @TOKEN@}
  plain: {type: fake, token: @TOKEN@}
server:
  listen: 127.0.0.1:0
  webhook_secret: '@SECRET@'
rules:
  - name: r1
    from: {forge: src, owner: alice}
    to: {forge: dst, owner: mirrors}
    on_source_deleted: archive
  - name: r2
    from: {forge: src, owner: bob}
    to: {forge: dst, owner: bobmirror}
`

func newFixture(t *testing.T) *fixture {
	return newFixtureSecret(t, secret)
}

func newFixtureSecret(t *testing.T, hookSecret string) *fixture {
	return newFixtureTmpl(t, hookSecret, cfgTmpl)
}

func newFixtureTmpl(t *testing.T, hookSecret, tmpl string) *fixture {
	t.Helper()
	home := t.TempDir()
	f := &fixture{
		t: t,
		gitEnv: []string{
			"HOME=" + home,
			"USERPROFILE=" + home,
			"XDG_CONFIG_HOME=" + home,
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_CONFIG_GLOBAL=" + filepath.Join(home, ".gitconfig"),
		},
		src:      newGate(fake.New(fake.Options{GitBaseDir: t.TempDir(), Username: "u", Token: sentinel})),
		dst:      fake.New(fake.Options{GitBaseDir: t.TempDir(), Username: "u", Token: sentinel}),
		stateDir: filepath.Join(t.TempDir(), "state"),
		clock:    newFakeClock(),
	}
	y := strings.NewReplacer("@STATE@", filepath.ToSlash(f.stateDir), "@TOKEN@", sentinel, "@SECRET@", hookSecret).Replace(tmpl)
	cfg, err := config.Parse([]byte(y), func(string) (string, bool) { return "", false },
		func(string) ([]byte, error) { return nil, os.ErrNotExist })
	if err != nil {
		t.Fatal(err)
	}
	a, err := app.Build(context.Background(), cfg, app.Options{
		Providers: map[string]forge.Provider{
			"src":   f.src,
			"dst":   f.dst,
			"plain": plain{fake.New(fake.Options{Username: "u", Token: sentinel})},
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		GitEnv: f.gitEnv,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	f.app = a
	return f
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

// server returns a Server on the fixture's app with the fake clock and a
// job recorder. InitialDelay is one second unless changed by the caller.
func (f *fixture) server() *Server {
	s := &Server{
		App:          f.app,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Clock:        f.clock,
		Debounce:     5 * time.Second,
		InitialDelay: time.Second,
	}
	s.onJobDone = func(lane string, j Job) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.jobs = append(f.jobs, doneJob{lane, j})
	}
	return s
}

func (f *fixture) doneJobs() []doneJob {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.jobs)
}

// waitJobs waits until at least n jobs finished and returns them.
func (f *fixture) waitJobs(n int) []doneJob {
	f.t.Helper()
	waitFor(f.t, 60*time.Second, func() bool { return len(f.doneJobs()) >= n }, "jobs to finish")
	return f.doneJobs()
}

// running is a Server started by start.
type running struct {
	s      *Server
	cancel context.CancelFunc
	done   chan error
}

// start runs s on a loopback listener until the test ends.
func (f *fixture) start(s *Server) *running {
	f.t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		f.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{s: s, cancel: cancel, done: make(chan error, 1)}
	go func() { r.done <- s.Run(ctx, ln) }()
	f.t.Cleanup(func() {
		cancel()
		f.src.open()
		select {
		case err := <-r.done:
			_ = err // shutdown error is not under test here
		case <-time.After(60 * time.Second):
			f.t.Error("Run did not return")
		}
	})
	waitFor(f.t, 10*time.Second, func() bool { return get(s.Handler(), "/readyz").Code == http.StatusOK }, "readyz")
	return r
}

// stop cancels Run and returns its error.
func (r *running) stop(t *testing.T, timeout time.Duration) error {
	t.Helper()
	r.cancel()
	select {
	case err := <-r.done:
		r.done <- err // keep it for the cleanup
		return err
	case <-time.After(timeout):
		t.Fatal("Run did not return")
		return nil
	}
}

// initialSync advances past InitialDelay and waits for the full syncs of
// both rules.
func (f *fixture) initialSync(s *Server) {
	f.t.Helper()
	waitFor(f.t, 10*time.Second, func() bool { return f.clock.Waiters() >= 1 }, "scheduler to wait")
	f.clock.Advance(s.InitialDelay)
	f.waitJobs(2)
}

// advanceDebounce waits until the webhook worker has armed its debounce
// timer (one waiter beside the idle scheduler's), then moves the clock past
// the window. Advancing before the timer is armed would push its deadline
// past the advanced time, so it would never fire.
func (f *fixture) advanceDebounce(s *Server) {
	f.t.Helper()
	waitFor(f.t, 10*time.Second, func() bool { return f.clock.Waiters() >= 2 }, "webhook debounce timer")
	f.clock.Advance(s.Debounce)
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func do(h http.Handler, method, path string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	h.ServeHTTP(rec, req)
	return rec
}

// hook posts a correctly signed fake webhook.
func hook(h http.Handler, forgeName, kind, owner, name string) *httptest.ResponseRecorder {
	body := []byte(`{"kind":"` + kind + `","owner":"` + owner + `","name":"` + name + `"}`)
	return do(h, http.MethodPost, "/webhook/"+forgeName, body, map[string]string{fake.SignatureHeader: fake.Sign(secret, body)})
}

func key(rule, owner, name, dstOwner string) state.PairKey {
	return state.PairKey{Rule: rule, SrcForge: "src", SrcRepo: owner + "/" + name, DstForge: "dst", DstRepo: dstOwner + "/" + name}
}

func (f *fixture) runs(k state.PairKey) []state.Run {
	f.t.Helper()
	runs, err := f.app.Store.Runs(context.Background(), k, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	return runs
}

func (f *fixture) dstHas(owner, name string) bool {
	_, err := f.dst.GetRepo(context.Background(), owner, name)
	return err == nil
}

var errBoom = errors.New("boom")
