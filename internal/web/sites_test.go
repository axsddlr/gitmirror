package web

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"gitmirror/internal/app"
	"gitmirror/internal/config"
)

func TestAddSiteGitHub(t *testing.T) {
	w := newWorld(t, "")
	c := w.admin()
	page := c.get("/sites/new")
	for _, want := range []string{`name="kind"`, `value="github"`, `value="forgejo"`, `type="password"`, `autocomplete="off"`,
		"https://github.com/settings/tokens", "/user/settings/applications", "repo"} {
		if !strings.Contains(page.body, want) {
			t.Errorf("add form lacks %q", want)
		}
	}
	r := c.postForm("/sites/new", "/sites/new", url.Values{"kind": {"github"}, "url": {""}, "token": {ghToken}, "name": {""}})
	if r.code != http.StatusSeeOther || r.location() != "/sites" {
		t.Fatalf("add = %d %q\n%.3000s", r.code, r.location(), r.body)
	}
	list := c.get("/sites")
	w.noSecrets(list.body)
	for _, want := range []string{"Connected as alice", "github", "GitHub", "alice", `href="/sites/github/edit"`} {
		if !strings.Contains(list.body, want) {
			t.Errorf("sites page lacks %q:\n%.3000s", want, list.body)
		}
	}
	f := w.app.Config().Forges["github"]
	if f.Type != "github" || f.URL != "" || f.Token != ghToken || f.Username != "alice" {
		t.Fatalf("stored forge = %v (token ok: %v)", f, f.Token == ghToken)
	}
	ufs, err := w.app.Store.UIForges(context.Background())
	if err != nil || len(ufs) != 1 || bytes.Contains(ufs[0].SealedToken, []byte(ghToken)) {
		t.Fatalf("stored token is not sealed (err %v)", err)
	}
	if strings.Contains(w.logs.String(), ghToken) {
		t.Fatal("token in the log")
	}
	// The edit form never shows the token.
	edit := c.get("/sites/github/edit")
	if edit.code != http.StatusOK {
		t.Fatalf("edit = %d", edit.code)
	}
	w.noSecrets(edit.body)
}

func TestAddSiteSuggestsNameFromHost(t *testing.T) {
	w := newWorld(t, "")
	c := w.admin()
	r := c.postForm("/sites/new", "/sites/new", url.Values{"kind": {"forgejo"}, "url": {"git.example.com/"}, "token": {gtToken}})
	if r.code != http.StatusSeeOther {
		t.Fatalf("add = %d\n%.3000s", r.code, r.body)
	}
	f, ok := w.app.Config().Forges["git-example-com"]
	if !ok || f.Type != "gitea" || f.URL != "https://git.example.com" || f.Username != "carol" {
		t.Fatalf("forges = %v", w.app.Config().Forges)
	}
	list := c.get("/sites")
	if !strings.Contains(list.body, "https://git.example.com") || !strings.Contains(list.body, "Gitea") {
		t.Fatalf("list:\n%.3000s", list.body)
	}
	if !strings.Contains(c.get("/sites/git-example-com/edit").body, "https://git.example.com/user/settings/applications") {
		t.Fatal("edit form lacks the token page link of the site")
	}
}

func TestAddSiteFailedTestNotSaved(t *testing.T) {
	w := newWorld(t, "")
	c := w.admin()
	w.gh.setErr(errors.New("github: get authenticated user: GET https://api.github.com/user: 401 Bad credentials []"))
	r := c.postForm("/sites/new", "/sites/new", url.Values{"kind": {"github"}, "token": {ghToken}})
	if r.code != http.StatusBadRequest || !strings.Contains(r.body, "The token was refused. Check that you copied all of it.") {
		t.Fatalf("refused token = %d\n%.3000s", r.code, r.body)
	}
	w.noSecrets(r.body)
	if _, ok := w.app.Config().Forges["github"]; ok {
		t.Fatal("site with a failed test was saved")
	}

	w.gt.setErr(&url.Error{Op: "Get", URL: "https://git.example.com/api/v1/user",
		Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}})
	r = c.postForm("/sites/new", "/sites/new", url.Values{"kind": {"gitea"}, "url": {"https://git.example.com"}, "token": {gtToken}})
	if r.code != http.StatusBadRequest || !strings.Contains(r.body, "Could not reach https://git.example.com") {
		t.Fatalf("unreachable = %d\n%.3000s", r.code, r.body)
	}
	w.noSecrets(r.body)
	if len(w.app.Config().Forges) != 0 {
		t.Fatal("unreachable site was saved")
	}
}

func TestAddSiteValidation(t *testing.T) {
	w := newWorld(t, "")
	c := w.admin()
	for _, tc := range []struct {
		form url.Values
		want string
	}{
		{url.Values{"kind": {"gitea"}, "token": {gtToken}}, "Enter the address of your server"},
		{url.Values{"kind": {"other"}, "token": {ghToken}}, "Choose the kind of site"},
		{url.Values{"kind": {"github"}, "token": {ghToken}, "name": {"Bad Name"}}, "Use only lowercase letters"},
		{url.Values{"kind": {"gitea"}, "url": {"ftp://x.example"}, "token": {gtToken}}, "Enter the address of your server"},
	} {
		r := c.postForm("/sites/new", "/sites/new", tc.form)
		if r.code != http.StatusBadRequest || !strings.Contains(r.body, tc.want) {
			t.Errorf("%v = %d, lacks %q\n%.2000s", tc.form, r.code, tc.want, r.body)
		}
		w.noSecrets(r.body)
	}
	w.addSites()
	r := c.postForm("/sites/new", "/sites/new", url.Values{"kind": {"github"}, "token": {ghToken}, "name": {"github"}})
	if r.code != http.StatusBadRequest || !strings.Contains(r.body, "already") {
		t.Fatalf("duplicate name = %d\n%.2000s", r.code, r.body)
	}
}

func TestEditSiteBlankTokenKeepsToken(t *testing.T) {
	w := newWorld(t, "")
	w.addSites()
	c := w.admin()
	r := c.postForm("/sites/github/edit", "/sites/github/edit", url.Values{"kind": {"github"}, "url": {""}, "token": {""}})
	if r.code != http.StatusSeeOther {
		t.Fatalf("edit = %d\n%.3000s", r.code, r.body)
	}
	if f := w.app.Config().Forges["github"]; f.Token != ghToken {
		t.Fatal("blank token did not keep the token")
	}
	// A new token is tested first; a refused one is not saved.
	w.gh.setErr(errors.New("github: 401 Bad credentials"))
	r = c.postForm("/sites/github/edit", "/sites/github/edit", url.Values{"kind": {"github"}, "token": {"SENTINEL-new-token-1"}})
	if r.code != http.StatusBadRequest || !strings.Contains(r.body, "The token was refused") || strings.Contains(r.body, "SENTINEL-new-token-1") {
		t.Fatalf("bad new token = %d\n%.3000s", r.code, r.body)
	}
	if f := w.app.Config().Forges["github"]; f.Token != ghToken {
		t.Fatal("refused token was saved")
	}
	w.gh.setErr(nil)
	r = c.postForm("/sites/github/edit", "/sites/github/edit", url.Values{"kind": {"github"}, "token": {"SENTINEL-new-token-1"}})
	if r.code != http.StatusSeeOther {
		t.Fatalf("new token = %d\n%.3000s", r.code, r.body)
	}
	if f := w.app.Config().Forges["github"]; f.Token != "SENTINEL-new-token-1" {
		t.Fatal("new token not saved")
	}
	if r := c.get("/sites/nosuch/edit"); r.code != http.StatusNotFound {
		t.Fatalf("unknown site edit = %d", r.code)
	}
}

func TestDeleteSiteRefusedWhenUsed(t *testing.T) {
	w := newWorld(t, "")
	w.addSites()
	w.putRule(config.Rule{Name: "copy-alice", From: config.Source{Forge: "github", Owner: "alice"}, To: config.Dest{Forge: "git-example-com", Owner: "carol"}})
	c := w.admin()
	confirm := c.get("/sites/github/delete")
	if confirm.code != http.StatusOK || !strings.Contains(confirm.body, "copy-alice") || strings.Contains(confirm.body, "btn-danger") {
		t.Fatalf("delete page of a used site = %d\n%.3000s", confirm.code, confirm.body)
	}
	r := c.postForm("/sites/github/delete", "/sites/github/delete", nil)
	if r.code != http.StatusConflict || !strings.Contains(r.body, "copy-alice") {
		t.Fatalf("delete used = %d\n%.3000s", r.code, r.body)
	}
	if _, ok := w.app.Config().Forges["github"]; !ok {
		t.Fatal("used site was deleted")
	}
	if err := w.app.UpdateUI(context.Background(), func(u *app.UIConfig) error { return u.DeleteRule("copy-alice") }); err != nil {
		t.Fatal(err)
	}
	confirm = c.get("/sites/github/delete")
	if !strings.Contains(confirm.body, "btn-danger") {
		t.Fatalf("no red button:\n%.3000s", confirm.body)
	}
	r = c.postForm("/sites/github/delete", "/sites/github/delete", nil)
	if r.code != http.StatusSeeOther {
		t.Fatalf("delete = %d\n%.3000s", r.code, r.body)
	}
	if _, ok := w.app.Config().Forges["github"]; ok {
		t.Fatal("site not deleted")
	}
}

func TestFileDefinedSiteReadOnly(t *testing.T) {
	w := newWorld(t, fileYAML)
	c := w.admin()
	list := c.get("/sites")
	w.noSecrets(list.body)
	if !strings.Contains(list.body, "filesite") || !strings.Contains(list.body, "From config file") ||
		strings.Contains(list.body, "/sites/filesite/edit") || strings.Contains(list.body, "/sites/filesite/delete") {
		t.Fatalf("list:\n%.3000s", list.body)
	}
	if r := c.get("/sites/filesite/edit"); r.code != http.StatusForbidden {
		t.Fatalf("GET edit of file site = %d", r.code)
	}
	tok := c.csrf("/sites")
	r := c.post("/sites/filesite/edit", url.Values{"csrf": {tok}, "kind": {"github"}, "token": {"SENTINEL-evil-1"}})
	if r.code != http.StatusForbidden {
		t.Fatalf("crafted edit of file site = %d", r.code)
	}
	r = c.post("/sites/filesite/delete", url.Values{"csrf": {tok}})
	if r.code != http.StatusForbidden {
		t.Fatalf("crafted delete of file site = %d", r.code)
	}
	r = c.post("/sites/new", url.Values{"csrf": {tok}, "kind": {"github"}, "token": {ghToken}, "name": {"filesite"}})
	if r.code != http.StatusBadRequest {
		t.Fatalf("add over a file site name = %d", r.code)
	}
	if f := w.app.Config().Forges["filesite"]; f.Type != "fake" || f.Token != "SENTINEL-file-0001" {
		t.Fatal("file site changed")
	}
}
