package web

import (
	"net/http"
	"net/url"
	"testing"
)

// The wizard offers "Do not let the copied workflows run", saves it with the
// rule, shows it in the summary and fills it back in when editing.
func TestWizardDisableActions(t *testing.T) {
	w := newWorld(t, "")
	w.addSites()
	w.addRepo(w.gh.Provider, "alice", "one")
	c := w.admin()
	base := url.Values{"step": {"options"}, "src": {"github"}, "mode": {"pick"}, "account": {"alice"}, "repo": {"one"},
		"dst": {"git-example-com"}, "dstowner": {"carol"}, "naming": {"same"}, "visibility": {"private"}, "interval": {"1h"},
		"feat": {"releases"}, "name": {"noacts"}, "optsset": {"1"}, "noactions": {"1"}}

	opts := c.postForm("/mirrors/new", "/mirrors/wizard", withGo(maps(base), "back"))
	mustContain(t, opts, `name="noactions"`)

	preview := c.postForm("/mirrors/new", "/mirrors/wizard", withGo(base, "preview"))
	mustContain(t, preview, "Turned off in the copy")

	if r := c.postForm("/mirrors/new", "/mirrors/wizard", withGo(base, "save")); r.code != http.StatusSeeOther {
		t.Fatalf("save = %d\n%.3000s", r.code, r.body)
	}
	if got := uiRule(t, w, "noacts"); !got.DisableActions {
		t.Fatalf("saved disable_actions = %+v", got)
	}
	// Editing offers the saved choice again.
	if h := hidden(c.get("/mirrors/noacts/edit").body); h.Get("noactions") != "1" {
		t.Fatalf("edit prefill noactions = %q", h.Get("noactions"))
	}

	// Unticked is the default and stays false.
	off := maps(base)
	off.Del("noactions")
	off.Set("name", "acts")
	off.Set("dstowner", "dave")
	if r := c.postForm("/mirrors/new", "/mirrors/wizard", withGo(off, "save")); r.code != http.StatusSeeOther {
		t.Fatalf("save without actions = %d\n%.3000s", r.code, r.body)
	}
	if got := uiRule(t, w, "acts"); got.DisableActions {
		t.Fatalf("disable_actions saved while unticked: %+v", got)
	}
}
