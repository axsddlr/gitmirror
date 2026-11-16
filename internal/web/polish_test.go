package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"gitmirror/internal/app"
	"gitmirror/internal/config"
)

func TestWizardOneSitePreselectsDestination(t *testing.T) {
	w := newWorld(t, "")
	if err := w.app.UpdateUI(context.Background(), func(u *app.UIConfig) error {
		return u.PutForge("github", config.Forge{Type: "github", Token: ghToken, Username: "alice"})
	}); err != nil {
		t.Fatal(err)
	}
	w.addRepo(w.gh.Provider, "alice", "hello")
	c := w.admin()
	repos := startWizard(t, c, "github", "pick")
	to := c.next(repos, "next", url.Values{"repo": {"hello"}})
	mustContain(t, to, `name="dst" value="github" checked`, "alice/tool is copied as tool", "alice/tool is copied as alice-tool")
	if strings.Contains(to.body, "&lt;account&gt;") {
		t.Fatal("naming hint still shows <account>")
	}
	opts := c.next(to, "next", url.Values{"dstowner": {"team"}})
	if got := formValues(opts.body).Get("name"); got != "hello-copy" {
		t.Fatalf("suggested mirror name = %q, want hello-copy", got)
	}
}

func TestSuggestedNames(t *testing.T) {
	cfg := &config.Config{Forges: map[string]config.Forge{"gitea": {Type: "gitea"}}}
	for _, tc := range []struct{ kind, typ, addr, want string }{
		{"gitea", "gitea", "http://127.0.0.1:3311", "gitea-2"},
		{"forgejo", "gitea", "http://[::1]:3000", "forgejo"},
		{"gitea", "gitea", "https://git.example.com", "git-example-com"},
		{"github", "github", "", "github"},
	} {
		if got := suggestName(cfg, tc.kind, tc.typ, tc.addr); got != tc.want {
			t.Errorf("suggestName(%q, %q) = %q, want %q", tc.kind, tc.addr, got, tc.want)
		}
	}
	for _, tc := range []struct {
		w    wizard
		want string
	}{
		{wizard{Mode: modePick, Account: "alice", Picked: []string{"Hello"}, Dst: "x"}, "hello-copy"},
		{wizard{Mode: modePick, Account: "alice", Picked: []string{"a", "b"}, Dst: "x"}, "alice-repos"},
		{wizard{Mode: modeOwner, Account: "alice", Dst: "x"}, "alice-repos"},
		{wizard{Mode: modeStarred, Account: "alice", Dst: "x"}, "alice-stars"},
	} {
		if got := suggestMirrorName(&config.Config{}, &tc.w); got != tc.want {
			t.Errorf("suggestMirrorName(%+v) = %q, want %q", tc.w, got, tc.want)
		}
	}
}

func TestPreviewNoRepeatedSentence(t *testing.T) {
	w := newWorld(t, "")
	w.addSites()
	w.addRepo(w.gh.Provider, "alice", "one")
	c := w.admin()
	f := url.Values{"step": {"options"}, "src": {"github"}, "mode": {"pick"}, "account": {"alice"}, "repo": {"one"},
		"dst": {"git-example-com"}, "dstowner": {"carol"}, "naming": {"same"}, "visibility": {"private"}, "interval": {"1h"},
		"name": {"pv"}, "optsset": {"1"}}
	r := c.postForm("/mirrors/new", "/mirrors/wizard", withGo(f, "preview"))
	mustContain(t, r, "Will be created")
	if strings.Contains(r.body, "Will be created and filled") {
		t.Fatal("the preview repeats the pill in a sentence")
	}
	// An existing copy made by gitmirror: no fetch, so "Will be updated".
	w.putRule(config.Rule{Name: "pv", From: config.Source{Forge: "github", Repo: "alice/one"}, To: config.Dest{Forge: "git-example-com", Repo: "carol/one"}})
	if _, err := w.app.Sync(context.Background(), []string{"pv"}, nil, false, "cli"); err != nil {
		t.Fatal(err)
	}
	f.Set("edit", "pv")
	r = c.postForm("/mirrors", "/mirrors/wizard", withGo(f, "preview"))
	mustContain(t, r, "Will be updated", "Only what changed since the last copy is copied.")
}

func TestStepDetailPlain(t *testing.T) {
	for _, tc := range []struct{ step, detail, want string }{
		{"git", "0 created, 0 updated, 0 deleted", "No changes"},
		{"git", "3 refs up to date", "No changes"},
		{"git", "1 created, 2 updated, 0 deleted", "1 branch or tag added, 2 updated"},
		{"releases", "2 created, 1 updated, 0 deleted releases; 0 assets uploaded (0 replaced), 0 assets deleted", "2 releases added, 1 updated"},
		{"releases", "0 created, 0 updated, 0 deleted releases; 0 assets uploaded (0 replaced), 0 assets deleted; 1 drafts skipped", "No changes"},
		{"prepare", "destination created", "The copy was created"},
		{"settings", "updated description", ""},
	} {
		if got := stepDetail(tc.step, tc.detail); got != tc.want {
			t.Errorf("stepDetail(%q, %q) = %q, want %q", tc.step, tc.detail, got, tc.want)
		}
	}
}

func TestPairPagePlainDetails(t *testing.T) {
	w := newWorld(t, "")
	w.addSites()
	w.addRepo(w.gh.Provider, "alice", "one")
	w.putRule(config.Rule{Name: "m1", From: config.Source{Forge: "github", Repo: "alice/one"}, To: config.Dest{Forge: "git-example-com", Repo: "carol/one"}})
	if _, err := w.app.Sync(context.Background(), []string{"m1"}, nil, false, "cli"); err != nil {
		t.Fatal(err)
	}
	c := w.admin()
	dash := c.get("/")
	i := strings.Index(dash.body, "/pair?k=")
	if i < 0 {
		t.Fatalf("no pair link:\n%.3000s", dash.body)
	}
	link := dash.body[i : i+strings.IndexByte(dash.body[i:], '"')]
	page := c.get(strings.ReplaceAll(link, "&amp;", "&"))
	if page.code != http.StatusOK {
		t.Fatalf("pair page = %d", page.code)
	}
	mustContain(t, page, "The copy was created", "Technical details", "destination created")
}

func TestWebhooksPlainWordsAndEmptyState(t *testing.T) {
	w := newWorld(t, "")
	w.addSites()
	c := w.admin()
	empty := c.get("/webhooks")
	mustContain(t, empty, `class="card empty"`, "<h2>No mirrors yet</h2>", `href="/mirrors/new">New mirror</a>`)
	w.putRule(config.Rule{Name: "m1", From: config.Source{Forge: "github", Owner: "alice"}, To: config.Dest{Forge: "git-example-com", Owner: "carol"}})
	if err := w.app.SetPublicAddress(context.Background(), "https://mirror.example.com"); err != nil {
		t.Fatal(err)
	}
	page := c.get("/webhooks")
	mustContain(t, page, "gitmirror's webhook port", "Events to turn on", "Address to send to (Payload URL)",
		"Content type (choose JSON)", `<code class="url">https://mirror.example.com/webhook/github</code>`)
	for _, old := range []string{"server.listen", "Events to tick"} {
		if strings.Contains(page.body, old) {
			t.Errorf("page still says %q", old)
		}
	}
}
