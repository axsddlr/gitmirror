package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// A site without a token is a public, read-only site: it can be added,
// copied from, and is never offered as a destination.
func TestPublicReadOnlySite(t *testing.T) {
	w := newWorld(t, "")
	w.addSites()
	w.addRepo(w.fileSrc, "bob", "pub")
	c := w.admin()

	r := c.postForm("/sites/new", "/sites/new", url.Values{
		"kind": {"github"}, "url": {""}, "token": {""}, "name": {"filesite"},
	})
	if r.code != http.StatusSeeOther {
		t.Fatalf("add a public site = %d\n%.2000s", r.code, r.body)
	}
	if fg := w.app.Config().Forges["filesite"]; !fg.ReadOnly() {
		t.Fatalf("public site saved with token %q", fg.Token)
	}
	mustContain(t, c.get("/sites"), "filesite", "Public, read-only")

	// Copying from it works: the account is typed, since nobody is signed in.
	repos := c.next(c.get("/mirrors/new"), "next", url.Values{"src": {"filesite"}, "mode": {"pick"}, "account": {"bob"}})
	mustContain(t, repos, `name="repo" value="pub"`)

	to := c.next(repos, "next", url.Values{"repo": {"pub"}})
	if strings.Contains(to.body, `name="dst" value="filesite"`) {
		t.Fatal("a read-only site is offered as a destination")
	}
	mustContain(t, to, `name="dst" value="git-example-com"`)

	// Posting it as a destination is refused.
	bad := c.next(to, "next", url.Values{"dst": {"filesite"}, "dstowner": {"carol"}, "naming": {"same"}})
	if bad.code != http.StatusBadRequest || !strings.Contains(bad.body, "can only be copied from") {
		t.Fatalf("read-only destination = %d\n%.2000s", bad.code, bad.body)
	}
}
