package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"path"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gitmirror/internal/config"
	"gitmirror/internal/forge"
)

func uiRule(t *testing.T, w *world, name string) config.Rule {
	t.Helper()
	u, err := w.app.UI(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r, ok := u.Rules[name]
	if !ok {
		t.Fatalf("rule %q not saved; rules %v", name, u.Rules)
	}
	return r
}

func mustContain(t *testing.T, r resp, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(r.body, w) {
			t.Fatalf("page (code %d) lacks %q:\n%.4000s", r.code, w, r.body)
		}
	}
}

// startWizard walks the From step for site src in mode and returns step 2.
func startWizard(t *testing.T, c *client, src, mode string) resp {
	t.Helper()
	first := c.get("/mirrors/new")
	mustContain(t, first, `name="src"`, `value="pick"`, `value="owner"`, `value="starred"`)
	return c.next(first, "next", url.Values{"src": {src}, "mode": {mode}, "account": {""}})
}

func TestWizardSingleRepo(t *testing.T) {
	w := newWorld(t, "")
	w.addSites()
	w.addRepo(w.gh.Provider, "alice", "one")
	w.addRepo(w.gh.Provider, "alice", "two")
	c := w.admin()
	repos := startWizard(t, c, "github", "pick")
	mustContain(t, repos, `name="repo" value="one"`, `name="repo" value="two"`, "repo one", `name="q"`)

	// Server-side search narrows the list.
	found := c.next(repos, "search", url.Values{"q": {"tw"}})
	if strings.Contains(found.body, `name="repo" value="one"`) || !strings.Contains(found.body, `name="repo" value="two"`) {
		t.Fatalf("search:\n%.4000s", found.body)
	}
	if r := c.next(repos, "next", nil); r.code != http.StatusBadRequest || !strings.Contains(r.body, "Tick at least one repository") {
		t.Fatalf("no pick = %d", r.code)
	}
	to := c.next(repos, "next", url.Values{"repo": {"one"}})
	mustContain(t, to, `name="dst"`, `value="same"`, `value="prefix"`, `name="dstname"`)
	opts := c.next(to, "next", url.Values{"dst": {"git-example-com"}, "dstowner": {""}, "naming": {"same"}})
	mustContain(t, opts, `name="feat" value="lfs" checked`, `value="private" checked`, `name="interval"`, `name="adopt"`, `name="name"`)
	preview := c.next(opts, "preview", nil)
	mustContain(t, preview, "alice/one", "carol/one", "Will be created", "Save mirror")
	w.noSecrets(preview.body)
	saved := c.next(preview, "save", nil)
	if saved.code != http.StatusSeeOther || saved.location() != "/" {
		t.Fatalf("save = %d %q\n%.3000s", saved.code, saved.location(), saved.body)
	}
	name := hidden(preview.body).Get("name")
	r := uiRule(t, w, name)
	if r.From.Forge != "github" || r.From.Repo != "alice/one" || r.From.Owner != "" || r.To.Forge != "git-example-com" || r.To.Repo != "carol/one" {
		t.Fatalf("rule = %+v", r)
	}
	if r.Visibility != "private" || r.Interval != "1h" || r.Adopt || !slices.Equal(r.Include, []string{"git", "lfs", "settings", "releases", "wiki"}) {
		t.Fatalf("rule options = %+v", r)
	}
	if got := w.synced(); !slices.Equal(got, []string{name}) {
		t.Fatalf("SyncNow calls = %v", got)
	}
	mustContain(t, c.get("/"), "Mirror saved. The first copy starts in a moment.")
}

func TestWizardCustomDestinationName(t *testing.T) {
	w := newWorld(t, "")
	w.addSites()
	w.addRepo(w.gh.Provider, "alice", "one")
	c := w.admin()
	f := url.Values{"step": {"preview"}, "src": {"github"}, "mode": {"pick"}, "account": {"alice"}, "repo": {"one"},
		"dst": {"git-example-com"}, "dstowner": {"team"}, "naming": {"custom"}, "dstname": {"renamed"},
		"feat": {"settings"}, "visibility": {"match-source"}, "interval": {"15m"}, "adopt": {"1"}, "name": {"custom-one"}, "optsset": {"1"}}
	r := c.postForm("/mirrors/new", "/mirrors/wizard", withGo(f, "save"))
	if r.code != http.StatusSeeOther {
		t.Fatalf("save = %d\n%.3000s", r.code, r.body)
	}
	got := uiRule(t, w, "custom-one")
	if got.To.Repo != "team/renamed" || got.Visibility != "match-source" || got.Interval != "15m" || !got.Adopt ||
		!slices.Equal(got.Include, []string{"git", "settings"}) {
		t.Fatalf("rule = %+v", got)
	}
}

func withGo(f url.Values, goTo string) url.Values {
	f = maps(f)
	f.Set("go", goTo)
	return f
}

func maps(f url.Values) url.Values {
	out := url.Values{}
	for k, v := range f {
		out[k] = slices.Clone(v)
	}
	return out
}

func TestWizardMultiPickEscapesNames(t *testing.T) {
	w := newWorld(t, "")
	w.addSites()
	// '*' and '?' cannot be file names on Windows; escapePattern covers them.
	for _, n := range []string{"one", "x[1]"} {
		if err := w.gh.AddRepo(forge.Repo{Owner: "alice", Name: n, DefaultBranch: "main"}); err != nil {
			t.Fatal(err)
		}
	}
	c := w.admin()
	f := url.Values{"step": {"preview"}, "src": {"github"}, "mode": {"pick"}, "account": {"alice"}, "repo": {"x[1]", "one"},
		"dst": {"git-example-com"}, "dstowner": {"carol"}, "naming": {"prefix"}, "visibility": {"private"}, "interval": {"6h"},
		"name": {"several"}, "optsset": {"1"}}
	r := c.postForm("/mirrors/new", "/mirrors/wizard", withGo(f, "save"))
	if r.code != http.StatusSeeOther {
		t.Fatalf("save = %d\n%.3000s", r.code, r.body)
	}
	got := uiRule(t, w, "several")
	if got.From.Owner != "alice" || got.From.Repo != "" || got.To.Owner != "carol" || got.To.Name != "{{.Owner}}-{{.Name}}" {
		t.Fatalf("rule = %+v", got)
	}
	flt := got.From.Filter
	if flt == nil || !slices.Equal(flt.Include, []string{"one", `x\[1]`}) || flt.Forks == nil || !*flt.Forks ||
		flt.Archived == nil || !*flt.Archived || flt.Private == nil || !*flt.Private {
		t.Fatalf("filter = %+v", flt)
	}
	if !slices.Equal(got.Include, []string{"git"}) || got.Interval != "6h" {
		t.Fatalf("options = %+v", got)
	}
}

func TestEscapePattern(t *testing.T) {
	for in, want := range map[string]string{"plain": "plain", "a*b": `a\*b`, "t?x": `t\?x`, "x[1]": `x\[1]`, `b\s`: `b\\s`} {
		got := escapePattern(in)
		if got != want {
			t.Errorf("escapePattern(%q) = %q, want %q", in, got, want)
		}
		if ok, err := path.Match(got, in); err != nil || !ok {
			t.Errorf("%q does not match %q: %v", got, in, err)
		}
		if back, ok := literalPattern(got); !ok || back != in {
			t.Errorf("literalPattern(%q) = %q %v", got, back, ok)
		}
	}
	if _, ok := literalPattern("*"); ok {
		t.Error("* is not literal")
	}
}

func TestWizardOwnerAndStarredModes(t *testing.T) {
	w := newWorld(t, "")
	w.addSites()
	w.addRepo(w.gh.Provider, "alice", "one")
	c := w.admin()
	step2 := startWizard(t, c, "github", "owner")
	mustContain(t, step2, `name="forks"`, `name="archived" value="1" checked`)
	to := c.next(step2, "next", nil)
	opts := c.next(to, "next", url.Values{"dst": {"git-example-com"}, "naming": {"same"}})
	preview := c.next(opts, "preview", url.Values{"name": {"all-alice"}})
	mustContain(t, preview, "Will be created", "alice/one")
	if r := c.next(preview, "save", nil); r.code != http.StatusSeeOther {
		t.Fatalf("save = %d\n%.3000s", r.code, r.body)
	}
	got := uiRule(t, w, "all-alice")
	if got.From.Owner != "alice" || got.From.Filter == nil || *got.From.Filter.Forks || !*got.From.Filter.Archived ||
		got.To.Owner != "carol" || got.To.Name != "{{.Name}}" {
		t.Fatalf("owner rule = %+v filter %+v", got, got.From.Filter)
	}

	step2 = startWizard(t, c, "github", "starred")
	to = c.next(step2, "next", url.Values{"forks": {"1"}})
	opts = c.next(to, "next", url.Values{"dst": {"git-example-com"}, "naming": {"prefix"}})
	preview = c.next(opts, "preview", url.Values{"name": {"stars"}})
	if r := c.next(preview, "save", nil); r.code != http.StatusSeeOther {
		t.Fatalf("save starred = %d\n%.3000s", r.code, r.body)
	}
	got = uiRule(t, w, "stars")
	if got.From.StarredBy != "alice" || !*got.From.Filter.Forks || got.To.Name != "{{.Owner}}-{{.Name}}" {
		t.Fatalf("starred rule = %+v", got)
	}
}

func TestPreviewShowsCreateAndConflictWritesNothing(t *testing.T) {
	w := newWorld(t, "")
	w.addSites()
	w.addRepo(w.gh.Provider, "alice", "one")
	w.addRepo(w.gh.Provider, "alice", "two")
	w.addRepo(w.gt.Provider, "carol", "two") // exists, not made by gitmirror
	c := w.admin()
	before := w.dump()
	f := url.Values{"step": {"options"}, "src": {"github"}, "mode": {"pick"}, "account": {"alice"}, "repo": {"one", "two"},
		"dst": {"git-example-com"}, "dstowner": {"carol"}, "naming": {"same"}, "feat": {"lfs", "settings", "releases", "wiki"},
		"visibility": {"private"}, "interval": {"1h"}, "name": {"pv"}, "optsset": {"1"}}
	r := c.postForm("/mirrors/new", "/mirrors/wizard", withGo(f, "preview"))
	mustContain(t, r, "alice/one", "Will be created", "alice/two", "already exists", "Allow copying into repositories that already exist")
	if after := w.dump(); !reflect.DeepEqual(before, after) {
		t.Fatalf("preview wrote to the store:\nbefore %+v\nafter  %+v", before, after)
	}
	if _, err := w.gt.GetRepo(context.Background(), "carol", "one"); !errors.Is(err, forge.ErrNotFound) {
		t.Fatal("preview created a repository")
	}
	if w.app.Config().RuleOrigin("pv") != "" {
		t.Fatal("preview saved the mirror")
	}
	// With adopt ticked the preview warns that content will be replaced.
	f.Set("adopt", "1")
	r = c.postForm("/mirrors/new", "/mirrors/wizard", withGo(f, "preview"))
	mustContain(t, r, "will be replaced")
}

func TestWizardValidationErrorsInPlainWords(t *testing.T) {
	w := newWorld(t, "")
	w.addSites()
	w.addRepo(w.gh.Provider, "alice", "one")
	w.addRepo(w.gh.Provider, "bob", "one")
	c := w.admin()
	self := url.Values{"step": {"options"}, "src": {"github"}, "mode": {"pick"}, "account": {"alice"}, "repo": {"one"},
		"dst": {"github"}, "dstowner": {"alice"}, "naming": {"same"}, "visibility": {"private"}, "interval": {"1h"},
		"name": {"selfy"}, "optsset": {"1"}}
	r := c.postForm("/mirrors/new", "/mirrors/wizard", withGo(self, "preview"))
	if r.code != http.StatusBadRequest {
		t.Fatalf("self mirror = %d", r.code)
	}
	// Caused by step 3, so shown on step 3 next to the field.
	mustContain(t, r, "The original and the copy would be the same repository", "Step 3 of 5: Copy to", `id="dstowner-error"`)
	if strings.Contains(r.body, "rules[") {
		t.Fatalf("technical message shown:\n%.3000s", r.body)
	}

	w.putRule(config.Rule{Name: "first", From: config.Source{Forge: "github", Repo: "alice/one"}, To: config.Dest{Forge: "git-example-com", Repo: "carol/one"}})
	clash := maps(self)
	clash.Set("account", "bob")
	clash.Set("dst", "git-example-com")
	clash.Set("dstowner", "carol")
	clash.Set("name", "second")
	r = c.postForm("/mirrors/new", "/mirrors/wizard", withGo(clash, "save"))
	if r.code != http.StatusBadRequest {
		t.Fatalf("collision = %d", r.code)
	}
	mustContain(t, r, "Another mirror already copies into the same place", "Step 3 of 5: Copy to", `id="dstowner-error"`)
	if w.app.Config().RuleOrigin("second") != "" {
		t.Fatal("colliding mirror saved")
	}

	dup := maps(clash)
	dup.Set("name", "first")
	dup.Set("dstowner", "other")
	r = c.postForm("/mirrors/new", "/mirrors/wizard", withGo(dup, "save"))
	if r.code != http.StatusBadRequest {
		t.Fatalf("duplicate name = %d", r.code)
	}
	mustContain(t, r, "already used by another mirror", "Step 4 of 5: Options")
	bad := maps(dup)
	bad.Set("name", "Not Valid")
	r = c.postForm("/mirrors/new", "/mirrors/wizard", withGo(bad, "save"))
	mustContain(t, r, "Use only lowercase letters")
}

func TestEditMirrorPrefilledAndRenamed(t *testing.T) {
	w := newWorld(t, "")
	w.addSites()
	w.addRepo(w.gh.Provider, "alice", "one")
	w.putRule(config.Rule{Name: "old", From: config.Source{Forge: "github", Repo: "alice/one"}, To: config.Dest{Forge: "git-example-com", Repo: "carol/one"},
		Interval: "6h", Visibility: "private", Include: []string{"git", "wiki"}})
	c := w.admin()
	edit := c.get("/mirrors/old/edit")
	mustContain(t, edit, `value="github" checked`, `value="pick" checked`, `value="alice"`)
	h := hidden(edit.body)
	if h.Get("edit") != "old" || h.Get("interval") != "6h" || !slices.Equal(h["feat"], []string{"wiki"}) || h.Get("name") != "old" ||
		!slices.Equal(h["repo"], []string{"one"}) || h.Get("dstowner") != "carol" || h.Get("naming") != "same" {
		t.Fatalf("prefill = %v", h)
	}
	f := maps(h)
	f.Set("step", "preview")
	f.Set("src", "github")
	f.Set("mode", "pick")
	f.Set("account", "alice")
	f.Set("name", "renamed")
	r := c.postForm("/mirrors", "/mirrors/wizard", withGo(f, "save"))
	if r.code != http.StatusSeeOther {
		t.Fatalf("save = %d\n%.3000s", r.code, r.body)
	}
	cfg := w.app.Config()
	if cfg.RuleOrigin("old") != "" || cfg.RuleOrigin("renamed") == "" {
		t.Fatal("rename did not replace the mirror")
	}
	if got := uiRule(t, w, "renamed"); got.Interval != "6h" || !slices.Equal(got.Include, []string{"git", "wiki"}) {
		t.Fatalf("edited rule = %+v", got)
	}
}

func TestDeleteMirrorKeepsDestination(t *testing.T) {
	w := newWorld(t, "")
	w.addSites()
	w.addRepo(w.gh.Provider, "alice", "one")
	w.putRule(config.Rule{Name: "m1", From: config.Source{Forge: "github", Repo: "alice/one"}, To: config.Dest{Forge: "git-example-com", Repo: "carol/one"}})
	if _, err := w.app.Sync(context.Background(), []string{"m1"}, nil, false, "cli"); err != nil {
		t.Fatal(err)
	}
	c := w.admin()
	confirm := c.get("/mirrors/m1/delete")
	mustContain(t, confirm, "The copies already made are kept; gitmirror just stops updating them.", "btn-danger")
	r := c.postForm("/mirrors/m1/delete", "/mirrors/m1/delete", nil)
	if r.code != http.StatusSeeOther {
		t.Fatalf("delete = %d\n%.3000s", r.code, r.body)
	}
	if w.app.Config().RuleOrigin("m1") != "" {
		t.Fatal("mirror not deleted")
	}
	if _, err := w.gt.GetRepo(context.Background(), "carol", "one"); err != nil {
		t.Fatalf("destination gone: %v", err)
	}
	if r := c.get("/mirrors/m1/delete"); r.code != http.StatusNotFound {
		t.Fatalf("delete page of a gone mirror = %d", r.code)
	}
}

func TestFileDefinedMirrorReadOnly(t *testing.T) {
	w := newWorld(t, fileYAML)
	c := w.admin()
	list := c.get("/mirrors")
	mustContain(t, list, "filerule", "From config file")
	if strings.Contains(list.body, "/mirrors/filerule/edit") || strings.Contains(list.body, "/mirrors/filerule/delete") {
		t.Fatalf("edit/delete offered for a file mirror:\n%.3000s", list.body)
	}
	if r := c.get("/mirrors/filerule/edit"); r.code != http.StatusForbidden {
		t.Fatalf("GET edit = %d", r.code)
	}
	tok := c.csrf("/mirrors")
	if r := c.post("/mirrors/filerule/delete", url.Values{"csrf": {tok}}); r.code != http.StatusForbidden {
		t.Fatalf("crafted delete = %d", r.code)
	}
	crafted := url.Values{"csrf": {tok}, "go": {"save"}, "step": {"preview"}, "edit": {"filerule"}, "src": {"filesite"}, "mode": {"owner"},
		"account": {"bob"}, "dst": {"filedst"}, "dstowner": {"evil"}, "naming": {"same"}, "name": {"filerule"}, "optsset": {"1"},
		"visibility": {"private"}, "interval": {"1h"}}
	if r := c.post("/mirrors/wizard", crafted); r.code != http.StatusForbidden {
		t.Fatalf("crafted edit = %d\n%.2000s", r.code, r.body)
	}
	crafted.Del("edit")
	if r := c.post("/mirrors/wizard", crafted); r.code != http.StatusBadRequest {
		t.Fatalf("crafted new mirror over a file name = %d\n%.2000s", r.code, r.body)
	}
	if r := w.app.Config().Rules[0]; r.Name != "filerule" || r.To.Owner != "bobm" {
		t.Fatalf("file mirror changed: %+v", r)
	}
}

func TestSyncNowButtons(t *testing.T) {
	w := newWorld(t, "")
	w.addSites()
	w.putRule(config.Rule{Name: "m1", From: config.Source{Forge: "github", Owner: "alice"}, To: config.Dest{Forge: "git-example-com", Owner: "carol"}})
	c := w.admin()
	list := c.get("/mirrors")
	mustContain(t, list, `action="/mirrors/m1/sync"`)
	if r := c.postForm("/mirrors", "/mirrors/m1/sync", nil); r.code != http.StatusSeeOther {
		t.Fatalf("sync now = %d", r.code)
	}
	if r := c.postForm("/", "/sync", nil); r.code != http.StatusSeeOther {
		t.Fatalf("sync all = %d", r.code)
	}
	if got := w.synced(); !slices.Equal(got, []string{"m1"}) || w.syncAllCalls != 1 {
		t.Fatalf("calls = %v, all %d", got, w.syncAllCalls)
	}
	if r := c.postForm("/mirrors", "/mirrors/nosuch/sync", nil); r.code != http.StatusNotFound {
		t.Fatalf("sync unknown = %d", r.code)
	}

	// Without the hook (no server) the buttons are hidden.
	w.configure = nil
	w.restart()
	c = w.admin2()
	if strings.Contains(c.get("/mirrors").body, "/sync") || strings.Contains(c.get("/").body, `action="/sync"`) {
		t.Fatal("sync buttons shown without a sync hook")
	}
}

// admin2 signs in again (after restart) with the test password.
func (h *harness) admin2() *client {
	h.t.Helper()
	c := h.client()
	if r := c.login(testPassword); r.code != http.StatusSeeOther {
		h.t.Fatalf("login = %d", r.code)
	}
	return c
}

// Copying into the source account under the same names is caught when
// leaving step 3, not only on Preview (owner and pick modes, and a custom
// name equal to the original's).
func TestParseSourceRef(t *testing.T) {
	for _, tc := range []struct {
		in, host, owner, repo string
		ok                    bool
	}{
		{"bob", "", "bob", "", true},
		{"bob/Lichii", "", "bob", "Lichii", true},
		{"https://github.com/zortos293/Lichii", "github.com", "zortos293", "Lichii", true},
		{"https://github.com/zortos293/Lichii.git", "github.com", "zortos293", "Lichii", true},
		{"https://github.com/zortos293/Lichii/tree/main/src", "github.com", "zortos293", "Lichii", true},
		{"https://github.com/zortos293/", "github.com", "zortos293", "", true},
		{"http://GitHub.com/zortos293", "github.com", "zortos293", "", true},
		{"github.com/zortos293/Lichii", "github.com", "zortos293", "Lichii", true},
		{"www.github.com/zortos293/Lichii", "github.com", "zortos293", "Lichii", true},
		{"git@github.com:zortos293/Lichii.git", "github.com", "zortos293", "Lichii", true},
		{"https://git.example.com:3000/carol/x?tab=readme#top", "git.example.com", "carol", "x", true},
		{"https://github.com/", "", "", "", false},
		{"a/b/c", "", "", "", false},
		{"bad name", "", "", "", false},
	} {
		host, owner, repo, ok := parseSourceRef(tc.in)
		if ok != tc.ok || ok && (host != tc.host || owner != tc.owner || repo != tc.repo) {
			t.Errorf("parseSourceRef(%q) = %q %q %q %v, want %q %q %q %v", tc.in, host, owner, repo, ok, tc.host, tc.owner, tc.repo, tc.ok)
		}
	}
}

func TestWizardAcceptsRepoLink(t *testing.T) {
	w := newWorld(t, "")
	w.addSites()
	w.addRepo(w.gh.Provider, "zortos293", "Lichii")
	w.addRepo(w.gh.Provider, "zortos293", "other")
	c := w.admin()

	// A repository link picks that repository, even with "everything" chosen.
	first := c.get("/mirrors/new")
	repos := c.next(first, "next", url.Values{"src": {"github"}, "mode": {"owner"}, "account": {"https://github.com/zortos293/Lichii"}})
	if repos.code != http.StatusOK {
		t.Fatalf("link = %d\n%.3000s", repos.code, repos.body)
	}
	mustContain(t, repos, "Step 2 of 5", `name="repo" value="Lichii" checked`)
	if strings.Contains(repos.body, `name="repo" value="other" checked`) {
		t.Fatal("other repository ticked")
	}
	to := c.next(repos, "next", nil)
	mustContain(t, to, "Step 3 of 5: Copy to")

	// An account link keeps the chosen mode.
	repos = c.next(first, "next", url.Values{"src": {"github"}, "mode": {"owner"}, "account": {"https://github.com/zortos293"}})
	if repos.code != http.StatusOK || strings.Contains(repos.body, `name="repo" value="Lichii" checked`) {
		t.Fatalf("account link = %d\n%.3000s", repos.code, repos.body)
	}

	// A link to another site is caught on step 1.
	r := c.next(first, "next", url.Values{"src": {"git-example-com"}, "mode": {"pick"}, "account": {"https://github.com/zortos293/Lichii"}})
	if r.code != http.StatusBadRequest {
		t.Fatalf("other site = %d", r.code)
	}
	mustContain(t, r, "Step 1 of 5", "github.com", "git.example.com")
}

func TestWizardSelfCopyCaughtOnStep3(t *testing.T) {
	w := newWorld(t, "")
	w.addSites()
	w.addRepo(w.gh.Provider, "alice", "one")
	c := w.admin()
	for _, tc := range []struct {
		mode, naming, dstname, field string
	}{
		{"pick", "same", "", "dstowner"},
		{"owner", "same", "", "dstowner"},
		{"pick", "custom", "ONE", "dstname"},
	} {
		repos := startWizard(t, c, "github", tc.mode)
		var to resp
		if tc.mode == "pick" {
			to = c.next(repos, "next", url.Values{"repo": {"one"}})
		} else {
			to = c.next(repos, "next", nil)
		}
		mustContain(t, to, "Step 3 of 5: Copy to")
		r := c.next(to, "next", url.Values{"dst": {"github"}, "dstowner": {""}, "naming": {tc.naming}, "dstname": {tc.dstname}})
		if r.code != http.StatusBadRequest {
			t.Fatalf("%+v: step 3 next = %d, want 400", tc, r.code)
		}
		mustContain(t, r, "The original and the copy would be the same repository", "Step 3 of 5: Copy to", `id="`+tc.field+`-error"`)
	}

	// Other names, or another account on the same site, are fine.
	repos := startWizard(t, c, "github", "pick")
	to := c.next(repos, "next", url.Values{"repo": {"one"}})
	for _, v := range []url.Values{
		{"dst": {"github"}, "dstowner": {""}, "naming": {"prefix"}},
		{"dst": {"github"}, "dstowner": {"backups"}, "naming": {"same"}},
		{"dst": {"github"}, "dstowner": {""}, "naming": {"custom"}, "dstname": {"one-copy"}},
	} {
		if r := c.next(to, "next", v); r.code != http.StatusOK || !strings.Contains(r.body, "Step 4 of 5: Options") {
			t.Fatalf("%v: got %d\n%.2000s", v, r.code, r.body)
		}
	}
}

// When every matching repository would be skipped, the preview says so and
// Save is disabled; a normal preview keeps Save enabled.
func TestWizardSaveDisabledWhenNothingCanBeCopied(t *testing.T) {
	w := newWorld(t, "")
	w.addSites()
	w.addRepo(w.gh.Provider, "alice", "one")
	w.addRepo(w.gh.Provider, "bob", "one")
	// Another mirror already writes carol/one, so a second one is skipped.
	w.putRule(config.Rule{Name: "first", From: config.Source{Forge: "github", Repo: "bob/one"}, To: config.Dest{Forge: "git-example-com", Repo: "carol/one"}})
	c := w.admin()
	f := url.Values{"step": {"options"}, "src": {"github"}, "mode": {"owner"}, "account": {"alice"},
		"dst": {"git-example-com"}, "dstowner": {"carol"}, "naming": {"same"}, "visibility": {"private"}, "interval": {"1h"},
		"name": {"second"}, "optsset": {"1"}, "filterset": {"1"}}
	r := c.postForm("/mirrors/new", "/mirrors/wizard", withGo(f, "preview"))
	if r.code != http.StatusOK {
		t.Fatalf("preview = %d\n%.3000s", r.code, r.body)
	}
	mustContain(t, r, "Will be skipped", "None of these repositories can be copied", `value="save" disabled`)

	f.Set("dstowner", "dave")
	r = c.postForm("/mirrors/new", "/mirrors/wizard", withGo(f, "preview"))
	mustContain(t, r, "Will be created", "Save mirror")
	if strings.Contains(r.body, `value="save" disabled`) || strings.Contains(r.body, "None of these repositories can be copied") {
		t.Fatalf("save disabled on a good preview:\n%.3000s", r.body)
	}
}
