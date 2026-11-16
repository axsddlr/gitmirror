package web

import (
	"context"
	"html"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

func init() {
	if !slices.Contains(config.KnownForgeTypes, "fake") {
		config.KnownForgeTypes = append(config.KnownForgeTypes, "fake")
	}
}

const (
	ghToken = "SENTINEL-gh-token-91c2"
	gtToken = "SENTINEL-gt-token-57ab"
)

// loginFailer is a fake provider whose identity check fails.
type loginFailer struct {
	*fake.Provider
	mu  sync.Mutex
	err error
	// hookDelay slows down every RepoHooks call.
	hookDelay time.Duration
}

func (l *loginFailer) RepoHooks(ctx context.Context, owner, repo string) ([]forge.Hook, error) {
	l.mu.Lock()
	d := l.hookDelay
	l.mu.Unlock()
	if d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return l.Provider.RepoHooks(ctx, owner, repo)
}

func (l *loginFailer) setErr(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.err = err
}

func (l *loginFailer) Login(ctx context.Context) (string, error) {
	l.mu.Lock()
	err := l.err
	l.mu.Unlock()
	if err != nil {
		return "", err
	}
	return l.Provider.Login(ctx)
}

// world is a harness whose App has fake providers for the sites the tests
// add: "github" (account alice), "git-example-com" (account carol), and the
// config file's "filesite" / "filedst" when the file defines them.
type world struct {
	*harness
	gh, gt       *loginFailer
	fileSrc      *fake.Provider
	fileDst      *fake.Provider
	home         string
	syncCalls    []string
	syncAllCalls int
	mu           sync.Mutex
}

const fileYAML = `forges:
  filesite: {type: fake, token: SENTINEL-file-0001}
  filedst: {type: fake, token: SENTINEL-file-0002}
rules:
  - name: filerule
    from: {forge: filesite, owner: bob}
    to: {forge: filedst, owner: bobm}
`

func newWorld(t *testing.T, yaml string) *world {
	t.Helper()
	w := &world{
		gh:      &loginFailer{Provider: fake.New(fake.Options{GitBaseDir: t.TempDir(), Username: "alice", Token: ghToken})},
		gt:      &loginFailer{Provider: fake.New(fake.Options{GitBaseDir: t.TempDir(), Username: "carol", Token: gtToken})},
		fileSrc: fake.New(fake.Options{GitBaseDir: t.TempDir(), Username: "bob", Token: "SENTINEL-file-0001"}),
		fileDst: fake.New(fake.Options{GitBaseDir: t.TempDir(), Username: "bobm", Token: "SENTINEL-file-0002"}),
		home:    t.TempDir(),
	}
	provs := map[string]forge.Provider{"github": w.gh, "git-example-com": w.gt, "filesite": w.fileSrc, "filedst": w.fileDst}
	h := &harness{t: t, app: buildAppOpts(t, yaml, provs),
		clock: &testClock{now: time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)}}
	h.configure = func(u *UI) {
		u.SyncNow = func(rule string) error {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.syncCalls = append(w.syncCalls, rule)
			return nil
		}
		u.SyncAll = func() error {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.syncAllCalls++
			return nil
		}
	}
	h.restart()
	w.harness = h
	return w
}

func (w *world) synced() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.syncCalls)
}

// addSites stores the two UI sites directly (as if added in the UI).
func (w *world) addSites() {
	w.t.Helper()
	if err := w.app.UpdateUI(context.Background(), func(u *app.UIConfig) error {
		if err := u.PutForge("github", config.Forge{Type: "github", Token: ghToken, Username: "alice"}); err != nil {
			return err
		}
		return u.PutForge("git-example-com", config.Forge{Type: "gitea", URL: "https://git.example.com", Token: gtToken, Username: "carol"})
	}); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) putRule(r config.Rule) {
	w.t.Helper()
	if err := w.app.UpdateUI(context.Background(), func(u *app.UIConfig) error { return u.PutRule(r) }); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) git(dir string, args ...string) {
	w.t.Helper()
	full := append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(kv), "GIT_") {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, gitEnv(w.home)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		w.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// addRepo seeds owner/name with one commit on p.
func (w *world) addRepo(p *fake.Provider, owner, name string) {
	w.t.Helper()
	if err := p.AddRepo(forge.Repo{Owner: owner, Name: name, DefaultBranch: "main", Description: "repo " + name}); err != nil {
		w.t.Fatal(err)
	}
	work := filepath.Join(w.t.TempDir(), "work")
	w.git(w.t.TempDir(), "init", "-q", "-b", "main", work)
	w.git(work, "remote", "add", "origin", p.GitURL(owner, name))
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte(name+"\n"), 0o644); err != nil {
		w.t.Fatal(err)
	}
	w.git(work, "add", "a.txt")
	w.git(work, "commit", "-q", "-m", "init")
	w.git(work, "push", "-q", "origin", "main")
}

var hiddenRe = regexp.MustCompile(`<input type="hidden" name="([^"]+)" value="([^"]*)">`)

// hidden returns the hidden fields of the page's wizard form (not csrf).
func hidden(body string) url.Values {
	v := url.Values{}
	for _, m := range hiddenRe.FindAllStringSubmatch(body, -1) {
		if m[1] != "csrf" {
			v.Add(m[1], html.UnescapeString(m[2]))
		}
	}
	return v
}

var (
	inputRe  = regexp.MustCompile(`<input\s[^>]*>`)
	attrRe   = regexp.MustCompile(`([a-z-]+)(?:="([^"]*)")?`)
	selectRe = regexp.MustCompile(`(?s)<select[^>]*name="([^"]+)"[^>]*>(.*?)</select>`)
	optionRe = regexp.MustCompile(`<option value="([^"]*)"[^>]*\sselected`)
)

// formValues returns what a browser would submit from the page's inputs:
// hidden, text and search values, checked boxes and radios, and selected
// options (csrf excluded).
func formValues(body string) url.Values {
	v := url.Values{}
	for _, tag := range inputRe.FindAllString(body, -1) {
		attrs := map[string]string{}
		has := map[string]bool{}
		for _, m := range attrRe.FindAllStringSubmatch(tag[len("<input"):], -1) {
			attrs[m[1]] = html.UnescapeString(m[2])
			has[m[1]] = true
		}
		name := attrs["name"]
		if name == "" || name == "csrf" || has["disabled"] {
			continue
		}
		switch attrs["type"] {
		case "hidden", "text", "search", "url", "":
			v.Add(name, attrs["value"])
		case "checkbox", "radio":
			if has["checked"] {
				v.Add(name, attrs["value"])
			}
		}
	}
	for _, m := range selectRe.FindAllStringSubmatch(body, -1) {
		if o := optionRe.FindStringSubmatch(m[2]); o != nil {
			v.Set(m[1], html.UnescapeString(o[1]))
		}
	}
	return v
}

// next submits the wizard page prev as a browser would (formValues) with
// set replacing fields of the same name, and the button value goTo.
func (c *client) next(prev resp, goTo string, set url.Values) resp {
	c.h.t.Helper()
	m := csrfRe.FindStringSubmatch(prev.body)
	if m == nil {
		c.h.t.Fatalf("no csrf field on page (code %d):\n%.3000s", prev.code, prev.body)
	}
	form := formValues(prev.body)
	for k, vs := range set {
		form[k] = vs
	}
	form.Set("csrf", m[1])
	form.Set("go", goTo)
	return c.post("/mirrors/wizard", form)
}

// postForm GETs page for its CSRF token and POSTs form to action.
func (c *client) postForm(page, action string, form url.Values) resp {
	c.h.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	form.Set("csrf", c.csrf(page))
	return c.post(action, form)
}

// storeDump captures every state table a page could write.
type storeDump struct {
	Pairs    []state.PairState
	Runs     []state.Run
	Forges   []state.UIForge
	Rules    []state.UIRule
	Settings []state.Setting
}

func (w *world) dump() storeDump {
	w.t.Helper()
	ctx := context.Background()
	var d storeDump
	var err error
	if d.Pairs, err = w.app.Store.Pairs(ctx); err != nil {
		w.t.Fatal(err)
	}
	if d.Runs, err = w.app.Store.RecentRuns(ctx, 0); err != nil {
		w.t.Fatal(err)
	}
	if d.Forges, err = w.app.Store.UIForges(ctx); err != nil {
		w.t.Fatal(err)
	}
	if d.Rules, err = w.app.Store.UIRules(ctx); err != nil {
		w.t.Fatal(err)
	}
	if d.Settings, err = w.app.Store.Settings(ctx); err != nil {
		w.t.Fatal(err)
	}
	return d
}

// noSecrets fails if body contains a forge token or the webhook secret.
func (w *world) noSecrets(body string) {
	w.t.Helper()
	for _, s := range []string{ghToken, gtToken, "SENTINEL-file-0001", "SENTINEL-file-0002"} {
		if strings.Contains(body, s) {
			w.t.Fatalf("page contains token %s", s)
		}
	}
}
