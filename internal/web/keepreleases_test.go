package web

import (
	"net/http"
	"net/url"
	"testing"
)

func TestWizardKeepReleases(t *testing.T) {
	w := newWorld(t, "")
	w.addSites()
	w.addRepo(w.gh.Provider, "alice", "one")
	c := w.admin()
	base := url.Values{"step": {"options"}, "src": {"github"}, "mode": {"pick"}, "account": {"alice"}, "repo": {"one"},
		"dst": {"git-example-com"}, "dstowner": {"carol"}, "naming": {"same"}, "visibility": {"private"}, "interval": {"1h"},
		"feat": {"releases"}, "name": {"keepy"}, "optsset": {"1"}}

	opts := c.postForm("/mirrors/new", "/mirrors/wizard", withGo(maps(base), "back"))
	mustContain(t, opts, `name="keepreleases"`)

	bad := maps(base)
	bad.Set("keepreleases", "-2")
	r := c.postForm("/mirrors/new", "/mirrors/wizard", withGo(bad, "preview"))
	if r.code != http.StatusBadRequest {
		t.Fatalf("negative keep = %d", r.code)
	}
	mustContain(t, r, "Enter a whole number of releases to keep", `id="keepreleases-error"`)

	good := maps(base)
	good.Set("keepreleases", "3")
	preview := c.postForm("/mirrors/new", "/mirrors/wizard", withGo(good, "preview"))
	mustContain(t, preview, "Only the newest 3")
	if r := c.postForm("/mirrors/new", "/mirrors/wizard", withGo(good, "save")); r.code != http.StatusSeeOther {
		t.Fatalf("save = %d\n%.3000s", r.code, r.body)
	}
	if got := uiRule(t, w, "keepy"); got.KeepReleases != 3 {
		t.Fatalf("saved keep_releases = %d, want 3", got.KeepReleases)
	}
	// Editing shows the saved number.
	if h := hidden(c.get("/mirrors/keepy/edit").body); h.Get("keepreleases") != "3" {
		t.Fatalf("edit prefill keepreleases = %q", h.Get("keepreleases"))
	}

	// Without Releases ticked the number is not saved.
	noRel := maps(good)
	noRel.Del("feat")
	noRel.Set("name", "norel")
	noRel.Set("dstowner", "dave")
	if r := c.postForm("/mirrors/new", "/mirrors/wizard", withGo(noRel, "save")); r.code != http.StatusSeeOther {
		t.Fatalf("save without releases = %d\n%.3000s", r.code, r.body)
	}
	if got := uiRule(t, w, "norel"); got.KeepReleases != 0 {
		t.Fatalf("keep_releases saved without releases: %+v", got)
	}
}
