package web

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"gitmirror/internal/app"
	"gitmirror/internal/config"
	"gitmirror/internal/forge"
)

const testPassword = "correct horse battery"

func TestMain(m *testing.M) {
	// Real hashes take ~0.5s each; tests only need the format.
	hashIterations = 1000
	os.Exit(m.Run())
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type harness struct {
	t     *testing.T
	app   *app.App
	ui    *UI
	srv   *httptest.Server
	clock *testClock
	logs  *syncBuffer
	// configure, when set, is applied to every new UI before Handler.
	configure func(*UI)
}

// buildApp builds an App with no forges or rules on a temp state dir.
func buildApp(t *testing.T, serverYAML string) *app.App {
	return buildAppOpts(t, serverYAML, nil)
}

// gitEnv isolates git from the host configuration under home.
func gitEnv(home string) []string {
	return []string{"HOME=" + home, "USERPROFILE=" + home, "XDG_CONFIG_HOME=" + home, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + filepath.Join(home, ".gitconfig")}
}

// buildAppOpts builds an App from the config YAML (state_dir is added)
// whose providers are provs (nil: built from the config).
func buildAppOpts(t *testing.T, yaml string, provs map[string]forge.Provider) *app.App {
	t.Helper()
	home := t.TempDir()
	y := "state_dir: '" + filepath.ToSlash(filepath.Join(t.TempDir(), "state")) + "'\n" + yaml
	cfg, err := config.Parse([]byte(y), func(string) (string, bool) { return "", false },
		func(string) ([]byte, error) { return nil, os.ErrNotExist })
	if err != nil {
		t.Fatal(err)
	}
	a, err := app.Build(context.Background(), cfg, app.Options{Providers: provs, GitEnv: gitEnv(home)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

func newHarness(t *testing.T) *harness {
	return newHarnessYAML(t, "")
}

func newHarnessYAML(t *testing.T, serverYAML string) *harness {
	t.Helper()
	h := &harness{t: t, app: buildApp(t, serverYAML),
		clock: &testClock{now: time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)}}
	h.restart()
	return h
}

// restart replaces the UI and test server with a fresh instance on the same
// App (as after a process restart).
func (h *harness) restart() {
	h.t.Helper()
	if h.srv != nil {
		h.srv.Close()
	}
	h.logs = &syncBuffer{}
	h.ui = &UI{
		App:     h.app,
		Logger:  slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Clock:   h.clock.Now,
		BaseURL: "http://gitmirror.test",
	}
	if h.configure != nil {
		h.configure(h.ui)
	}
	h.srv = httptest.NewServer(h.ui.Handler())
	h.t.Cleanup(h.srv.Close)
}

var setupCodeRe = regexp.MustCompile(`enter setup code ([A-Z0-9]{4}-[A-Z0-9]{4}-[A-Z0-9]{4})`)

// setupCode returns the setup code logged by the UI.
func (h *harness) setupCode() string {
	h.t.Helper()
	m := setupCodeRe.FindStringSubmatch(h.logs.String())
	if m == nil {
		h.t.Fatalf("no setup code logged:\n%s", h.logs.String())
	}
	return m[1]
}

type client struct {
	h   *harness
	c   *http.Client
	jar *cookiejar.Jar
}

func (h *harness) client() *client {
	jar, err := cookiejar.New(nil)
	if err != nil {
		h.t.Fatal(err)
	}
	return &client{h: h, jar: jar, c: &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

type resp struct {
	code   int
	body   string
	header http.Header
}

func (r resp) location() string { return r.header.Get("Location") }

func (c *client) do(req *http.Request) resp {
	c.h.t.Helper()
	res, err := c.c.Do(req)
	if err != nil {
		c.h.t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		c.h.t.Fatal(err)
	}
	return resp{res.StatusCode, string(b), res.Header}
}

func (c *client) get(path string) resp {
	c.h.t.Helper()
	req, err := http.NewRequest(http.MethodGet, c.h.srv.URL+path, nil)
	if err != nil {
		c.h.t.Fatal(err)
	}
	return c.do(req)
}

// post sends form; hdr are extra header name/value pairs.
func (c *client) post(path string, form url.Values, hdr ...string) resp {
	c.h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, c.h.srv.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		c.h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	return c.do(req)
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

// csrf GETs path and returns the form CSRF token on the page.
func (c *client) csrf(path string) string {
	c.h.t.Helper()
	r := c.get(path)
	m := csrfRe.FindStringSubmatch(r.body)
	if m == nil {
		c.h.t.Fatalf("GET %s: no csrf field (code %d):\n%.2000s", path, r.code, r.body)
	}
	return m[1]
}

func (c *client) cookie(name string) *http.Cookie {
	u, _ := url.Parse(c.h.srv.URL)
	for _, k := range c.jar.Cookies(u) {
		if k.Name == name {
			return k
		}
	}
	return nil
}

func (c *client) setup(code, pw, confirm string) resp {
	c.h.t.Helper()
	tok := c.csrf("/setup")
	return c.post("/setup", url.Values{"csrf": {tok}, "code": {code}, "password": {pw}, "confirm": {confirm}})
}

func (c *client) login(pw string) resp {
	c.h.t.Helper()
	tok := c.csrf("/login")
	return c.post("/login", url.Values{"csrf": {tok}, "password": {pw}})
}

// admin completes setup with testPassword and returns the signed-in client.
func (h *harness) admin() *client {
	h.t.Helper()
	c := h.client()
	if r := c.setup(h.setupCode(), testPassword, testPassword); r.code != http.StatusSeeOther || r.location() != "/" {
		h.t.Fatalf("setup = %d %q\n%.2000s", r.code, r.location(), r.body)
	}
	return c
}

// signedIn reports whether GET / shows the dashboard.
func (c *client) signedIn() bool {
	c.h.t.Helper()
	r := c.get("/")
	return r.code == http.StatusOK
}
