package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestEndToEnd walks the whole first-use flow in the browser UI: setup, two
// sites, a mirror through the wizard with its preview, save (which syncs
// through the SyncNow hook) and the dashboard showing the copy as OK.
func TestEndToEnd(t *testing.T) {
	w := newWorld(t, "")
	w.addRepo(w.gh.Provider, "alice", "hello")
	w.configure = func(u *UI) {
		u.SyncNow = func(rule string) error {
			_, err := w.app.Sync(context.Background(), []string{rule}, nil, false, "schedule")
			return err
		}
	}
	w.restart()
	w.clock.now = time.Now()
	c := w.admin()
	mustContain(t, c.get("/"), "Start by adding a site")

	for _, f := range []url.Values{
		{"kind": {"github"}, "token": {ghToken}},
		{"kind": {"forgejo"}, "url": {"https://git.example.com"}, "token": {gtToken}},
	} {
		if r := c.postForm("/sites/new", "/sites/new", f); r.code != http.StatusSeeOther {
			t.Fatalf("add site %v = %d\n%.3000s", f["kind"], r.code, r.body)
		}
	}
	mustContain(t, c.get("/"), "Now set up your first mirror")

	repos := startWizard(t, c, "github", "pick")
	to := c.next(repos, "next", url.Values{"repo": {"hello"}})
	opts := c.next(to, "next", url.Values{"dst": {"git-example-com"}, "naming": {"same"}})
	preview := c.next(opts, "preview", nil)
	mustContain(t, preview, "alice/hello", "carol/hello", "Will be created")
	if r := c.next(preview, "save", nil); r.code != http.StatusSeeOther || r.location() != "/" {
		t.Fatalf("save = %d %q\n%.3000s", r.code, r.location(), r.body)
	}
	if _, err := w.gt.GetRepo(context.Background(), "carol", "hello"); err != nil {
		t.Fatalf("not copied: %v", err)
	}
	d := c.get("/")
	mustContain(t, d, "Mirror saved", "github: alice/hello", "git-example-com: carol/hello", "OK", "just now")
	if strings.Contains(d.body, "Failed") || strings.Contains(d.body, "Not copied yet") {
		t.Fatalf("dashboard:\n%.4000s", d.body)
	}
	w.noSecrets(d.body)
}
