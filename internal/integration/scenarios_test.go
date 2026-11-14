//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitmirror/internal/forge"
	"gitmirror/internal/state"
)

// TestGitea runs every scenario against one pair of servers, A and B.
// Scenarios use disjoint repository names so they do not depend on order.
func TestGitea(t *testing.T) {
	start := time.Now()
	s := newSuite(t)
	t.Logf("gitea %s: A=%s B=%s (started in %s)", image(), s.a.url, s.b.url, time.Since(start).Round(time.Second))

	t.Run("APIContract", s.testAPIContract)
	t.Run("Webhook", s.testWebhook)
	t.Run("OwnerRule", s.testOwnerRule)
	t.Run("Incremental", s.testIncremental)
	t.Run("DefaultBranchAfterFirstPush", s.testDefaultBranchAfterFirstPush)
	t.Run("Releases", s.testReleases)
	t.Run("Wiki", s.testWiki)
	t.Run("LFS", s.testLFS)
	t.Run("ReverseDirection", s.testReverse)
	t.Run("AdoptSafety", s.testAdopt)
	t.Run("EmptySourceGuard", s.testEmptySource)
	t.Run("Archive", s.testArchive)
}

// seed creates the public repo owner/name on in with branch "main" plus extra
// branches, a lightweight tag "light" and an annotated tag "v1.0". It
// returns the work tree.
func (s *suite) seed(t *testing.T, in *instance, owner, name, desc string, branches ...string) *work {
	t.Helper()
	if owner == in.user {
		in.createRepo(t, name, desc, false)
	} else {
		in.mustAPI(t, http.MethodPost, "/orgs/"+owner+"/repos",
			map[string]any{"name": name, "description": desc, "private": false}, nil, http.StatusCreated)
	}
	w := s.git.newWork(t, "main", in.gitURL(owner, name))
	w.commit("README.md")
	w.git("tag", "light")
	w.commit("main.go")
	w.git("tag", "-a", "v1.0", "-m", "release 1.0")
	refs := []string{"main", "light", "v1.0"}
	for _, b := range branches {
		w.git("checkout", "-q", "-b", b, "main")
		w.commit(b + ".txt")
		refs = append(refs, b)
	}
	w.git("checkout", "-q", "main")
	w.push(refs...)
	in.waitNonEmpty(t, owner, name)
	return w
}

func ctxb() context.Context { return context.Background() }

// ---- API contract (the provider author's guesses, checked raw) ----

func (s *suite) testAPIContract(t *testing.T) {
	a := s.a
	// Create -> 201 with repo JSON; duplicate -> 409.
	var created map[string]any
	a.mustAPI(t, http.MethodPost, "/user/repos", map[string]any{"name": "api-c", "private": true, "auto_init": false}, &created, http.StatusCreated)
	if created["name"] != "api-c" || created["owner"].(map[string]any)["login"] != "alice" {
		t.Fatalf("create response is not repo JSON: %v", created)
	}
	code, body := a.api(t, http.MethodPost, "/user/repos", map[string]any{"name": "api-c"})
	if code != http.StatusConflict {
		t.Fatalf("duplicate create: status %d (%s), want 409", code, body)
	}
	if _, err := a.prov.CreateRepo(ctxb(), forge.CreateOptions{Owner: "alice", Name: "api-c"}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("provider duplicate create: %v", err)
	}
	t.Logf("duplicate create: 409 %s", body)

	// Topics: present as [] (never null) in repo JSON; PUT -> 204.
	var raw map[string]json.RawMessage
	a.mustAPI(t, http.MethodGet, "/repos/alice/api-c", nil, &raw, http.StatusOK)
	if string(raw["topics"]) != "[]" {
		t.Fatalf("topics in repo JSON = %s, want []", raw["topics"])
	}
	code, body = a.api(t, http.MethodPut, "/repos/alice/api-c/topics", map[string]any{"topics": []string{"zeta", "alpha"}})
	if code != http.StatusNoContent {
		t.Fatalf("PUT topics: %d %s", code, body)
	}
	if got := a.getRepo(t, "alice", "api-c").Topics; !slices.Equal(got, []string{"alpha", "zeta"}) {
		t.Fatalf("topics after PUT = %v", got)
	}
	// Archived repos reject topic changes (423) but the provider unarchives first.
	code, _ = a.api(t, http.MethodPatch, "/repos/alice/api-c", map[string]any{"archived": true})
	if code != http.StatusOK {
		t.Fatalf("archive: %d", code)
	}
	code, body = a.api(t, http.MethodPut, "/repos/alice/api-c/topics", map[string]any{"topics": []string{"x"}})
	if code != http.StatusLocked {
		t.Fatalf("PUT topics on archived repo: %d %s, want 423", code, body)
	}
	a.mustAPI(t, http.MethodPatch, "/repos/alice/api-c", map[string]any{"archived": false}, nil, http.StatusOK)

	// has_wiki is settable with PATCH /repos/{o}/{n}.
	a.mustAPI(t, http.MethodPatch, "/repos/alice/api-c", map[string]any{"has_wiki": false}, nil, http.StatusOK)
	a.mustAPI(t, http.MethodGet, "/repos/alice/api-c", nil, &raw, http.StatusOK)
	if string(raw["has_wiki"]) != "false" {
		t.Fatalf("has_wiki after PATCH false = %s", raw["has_wiki"])
	}
	a.mustAPI(t, http.MethodPatch, "/repos/alice/api-c", map[string]any{"has_wiki": true}, nil, http.StatusOK)

	// Release assets: multipart field "attachment"; the "name" query wins
	// over the part's filename; response is {id,name,size,...}; the list
	// uses "assets" with "browser_download_url".
	s.seed(t, a, "alice", "api-rel", "")
	var rel struct {
		ID int64 `json:"id"`
	}
	a.mustAPI(t, http.MethodPost, "/repos/alice/api-rel/releases", map[string]any{"tag_name": "v1.0", "name": "one"}, &rel, http.StatusCreated)
	data := randomBytes(t, 4321)
	var mp bytes.Buffer
	mw := multipart.NewWriter(&mp)
	part, _ := mw.CreateFormFile("attachment", "part-filename.bin")
	part.Write(data)
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, a.url+"/api/v1/repos/alice/api-rel/releases/"+itoa(rel.ID)+"/assets?name=query-name.bin", &mp)
	req.Header.Set("Authorization", "token "+a.token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := a.hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	ab, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var asset map[string]any
	if resp.StatusCode != http.StatusCreated || json.Unmarshal(ab, &asset) != nil {
		t.Fatalf("upload: %d %s", resp.StatusCode, ab)
	}
	if asset["name"] != "query-name.bin" || asset["size"] != float64(len(data)) || asset["id"] == nil || asset["browser_download_url"] == nil {
		t.Fatalf("upload response: %s", ab)
	}
	var list []map[string]json.RawMessage
	a.mustAPI(t, http.MethodGet, "/repos/alice/api-rel/releases", nil, &list, http.StatusOK)
	var assets []map[string]any
	if len(list) != 1 || json.Unmarshal(list[0]["assets"], &assets) != nil || len(assets) != 1 || assets[0]["browser_download_url"] == nil {
		t.Fatalf("release list assets: %s", list)
	}
	t.Logf("upload response browser_download_url=%v, list browser_download_url=%v", asset["browser_download_url"], assets[0]["browser_download_url"])
	// Asset downloads of a private repo need the token; anonymous gets 404.
	a.mustAPI(t, http.MethodPatch, "/repos/alice/api-rel", map[string]any{"private": true}, nil, http.StatusOK)
	for _, u := range []string{asset["browser_download_url"].(string), assets[0]["browser_download_url"].(string)} {
		anon, err := http.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		anon.Body.Close()
		if anon.StatusCode != http.StatusNotFound {
			t.Fatalf("anonymous download of private asset: %d", anon.StatusCode)
		}
	}
	rels, err := a.prov.(forge.ReleaseProvider).ListReleases(ctxb(), "alice", "api-rel")
	if err != nil {
		t.Fatal(err)
	}
	if got := download(t, a, "alice", "api-rel", rels[0].Assets[0]); !bytes.Equal(got, data) {
		t.Fatalf("provider download: %d bytes, want %d", len(got), len(data))
	}

	// Organisations: duplicate POST /orgs -> 422 "user already exists"; the
	// same answer is given when the name belongs to a user, which
	// EnsureOrg must not mistake for an existing organisation.
	op := a.prov.(forge.OrgProvider)
	if err := op.EnsureOrg(ctxb(), "api-org"); err != nil {
		t.Fatal(err)
	}
	if err := op.EnsureOrg(ctxb(), "api-org"); err != nil {
		t.Fatalf("EnsureOrg on existing org: %v", err)
	}
	code, body = a.api(t, http.MethodPost, "/orgs", map[string]any{"username": "api-org"})
	if code != http.StatusUnprocessableEntity || !strings.Contains(string(body), "already exist") {
		t.Fatalf("duplicate org: %d %s", code, body)
	}
	t.Logf("duplicate org: %d %s", code, body)
	a.mustAPI(t, http.MethodPost, "/admin/users", map[string]any{"username": "carol", "email": "carol@example.com",
		"password": adminPassword, "must_change_password": false}, nil, http.StatusCreated)
	code, body = a.api(t, http.MethodPost, "/orgs", map[string]any{"username": "carol"})
	t.Logf("POST /orgs for existing user: %d %s", code, body)
	if err := op.EnsureOrg(ctxb(), "carol"); err == nil {
		t.Fatal("EnsureOrg(carol) succeeded although carol is a user, not an organisation")
	} else {
		t.Logf("EnsureOrg(carol): %v", err)
	}

	// /user/repos also lists repositories of organisations alice belongs
	// to and those she collaborates on, so ListRepos must filter on
	// owner.login.
	a.mustAPI(t, http.MethodPost, "/orgs/api-org/repos", map[string]any{"name": "orgrepo"}, nil, http.StatusCreated)
	a.mustAPI(t, http.MethodPost, "/admin/users/carol/repos", map[string]any{"name": "carols"}, nil, http.StatusCreated)
	a.mustAPI(t, http.MethodPut, "/repos/carol/carols/collaborators/alice", map[string]any{"permission": "read"}, nil, http.StatusNoContent)
	var mine []struct {
		FullName string `json:"full_name"`
		Owner    struct {
			Login string `json:"login"`
		} `json:"owner"`
	}
	a.mustAPI(t, http.MethodGet, "/user/repos?limit=50", nil, &mine, http.StatusOK)
	var all, foreign []string
	for _, r := range mine {
		all = append(all, r.FullName)
		if r.Owner.Login != "alice" {
			foreign = append(foreign, r.FullName)
		}
	}
	t.Logf("/user/repos for alice: %v (not owned by alice: %v)", all, foreign)
	if len(foreign) == 0 {
		t.Fatalf("/user/repos lists only alice's own repos here, so the owner.login filter is not exercised: %v", all)
	}
	listed, err := a.prov.ListRepos(ctxb(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != len(all)-len(foreign) {
		t.Fatalf("ListRepos(alice) returned %d repos, want %d", len(listed), len(all)-len(foreign))
	}
	for _, r := range listed {
		if r.Owner != "alice" {
			t.Fatalf("ListRepos(alice) returned %s", r.FullName())
		}
	}

	// git over HTTP: any username with the token as password; a wrong
	// token is refused (api-c is private, so auth really is checked).
	g := newGitEnv(t) // no default credentials
	out, err := g.cmd("", "-c", "http.extraHeader="+basic("not-a-real-user", a.token), "ls-remote", a.gitURL("alice", "api-rel"))
	if err != nil || !strings.Contains(out, "refs/heads/main") {
		t.Fatalf("ls-remote with arbitrary username: %v\n%s", err, out)
	}
	if out, err := g.cmd("", "-c", "http.extraHeader="+basic("alice", "0000000000000000000000000000000000000000"), "ls-remote", a.gitURL("alice", "api-rel")); err == nil {
		t.Fatalf("ls-remote with a wrong token succeeded:\n%s", out)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func download(t *testing.T, in *instance, owner, name string, a forge.Asset) []byte {
	t.Helper()
	rc, err := in.prov.(forge.ReleaseProvider).DownloadAsset(ctxb(), owner, name, a)
	if err != nil {
		t.Fatalf("%s download %s: %v", in.name, a.Name, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ---- webhook ----

func (s *suite) testWebhook(t *testing.T) {
	a := s.a
	const secret = "hook-secret-123"
	type got struct {
		ev      *forge.Event
		err     error
		headers http.Header
	}
	ch := make(chan got, 8)
	wp := a.prov.(forge.WebhookProvider)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ev, err := wp.ParseWebhook(r, secret)
		ch <- got{ev, err, r.Header.Clone()}
	}))
	t.Cleanup(srv.Close)
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	w := s.seed(t, a, "alice", "hook", "")
	a.mustAPI(t, http.MethodPost, "/repos/alice/hook/hooks", map[string]any{
		"type":   "gitea",
		"active": true,
		"events": []string{"push"},
		"config": map[string]string{"url": "http://host.docker.internal:" + itoa(int64(port)) + "/hook", "content_type": "json", "secret": secret},
	}, nil, http.StatusCreated)
	w.commit("hooked.txt")
	w.push("main")
	select {
	case g := <-ch:
		if g.err != nil {
			t.Fatalf("ParseWebhook: %v (headers %v)", g.err, g.headers)
		}
		if g.ev.Kind != forge.EventPush || g.ev.Owner != "alice" || g.ev.Name != "hook" {
			t.Fatalf("event = %+v", g.ev)
		}
		t.Logf("webhook headers: X-Gitea-Event=%q X-Forgejo-Event=%q X-Gitea-Signature set=%v", g.headers.Get("X-Gitea-Event"), g.headers.Get("X-Forgejo-Event"), g.headers.Get("X-Gitea-Signature") != "")
	case <-time.After(pollTimeout):
		t.Skip("no webhook delivery within the timeout: the container cannot reach a listener on the host's 127.0.0.1 here")
	}
}

// ---- scenario 1: owner rule ----

func (s *suite) testOwnerRule(t *testing.T) {
	a, b := s.a, s.b
	names := []string{"s1-one", "s1-two"}
	for i, n := range names {
		s.seed(t, a, "alice", n, "description of "+n, "develop", "feature-"+itoa(int64(i)))
		a.mustAPI(t, http.MethodPatch, "/repos/alice/"+n, map[string]any{"default_branch": "develop"}, nil, http.StatusOK)
		a.mustAPI(t, http.MethodPut, "/repos/alice/"+n+"/topics", map[string]any{"topics": []string{"mirror", "go-" + itoa(int64(i))}}, nil, http.StatusNoContent)
	}
	// A repo the filter excludes.
	a.createRepo(t, "other-s1", "", false)

	m := s.newMirror(t, `
- name: s1
  from: {forge: a, owner: alice, filter: {include: ["s1-*"]}}
  to: {forge: b, owner: mirrors}
`)
	reps := m.run()
	if len(reps) != len(names) {
		t.Fatalf("got %d reports, want %d", len(reps), len(names))
	}
	for _, n := range names {
		r := reps["b:mirrors/"+n]
		wantStatus(t, r, state.RunOK)
		if d := wantStep(t, r, "prepare", state.StepOK).Detail; !strings.Contains(d, "destination created") {
			t.Fatalf("prepare detail %q", d)
		}
		wantStep(t, r, "git", state.StepOK)
		how, owned, err := m.eng.Store.Owned(ctxb(), "b", "mirrors/"+n)
		if err != nil || !owned || how != state.OwnedCreated {
			t.Fatalf("ownership of %s: %v %v %v", n, how, owned, err)
		}
		src, dst := a.getRepo(t, "alice", n), b.getRepo(t, "mirrors", n)
		if !dst.Private {
			t.Fatalf("%s: destination is public", n)
		}
		if dst.Description != src.Description || dst.DefaultBranch != "develop" || !slices.Equal(dst.Topics, src.Topics) {
			t.Fatalf("%s settings: dst %+v, src %+v", n, dst, src)
		}
		srcRefs := s.git.lsRemote(t, a.gitURL("alice", n))
		if _, ok := srcRefs["refs/tags/v1.0^{}"]; !ok {
			t.Fatalf("fixture lacks the annotated tag: %v", srcRefs)
		}
		equalRefs(t, n, s.git.lsRemote(t, b.gitURL("mirrors", n)), srcRefs)
	}
	if _, err := b.prov.GetRepo(ctxb(), "mirrors", "other-s1"); err == nil {
		t.Fatal("filtered-out repo was mirrored")
	}

	reps = m.run()
	for _, n := range names {
		r := reps["b:mirrors/"+n]
		wantStatus(t, r, state.RunOK)
		wantStep(t, r, "git", state.StepUnchanged)
		wantStep(t, r, "settings", state.StepUnchanged)
		wantStep(t, r, "releases", state.StepUnchanged)
	}
}

// ---- scenario 2: incremental changes ----

func (s *suite) testIncremental(t *testing.T) {
	a, b := s.a, s.b
	// Branch names sort after "main" on purpose: see DefaultBranchAfterFirstPush.
	w := s.seed(t, a, "alice", "s2", "", "topic", "work")
	m := s.newMirror(t, `
- name: s2
  from: {forge: a, repo: alice/s2}
  to: {forge: b, repo: mirrors/s2}
`)
	wantStatus(t, m.one("b:mirrors/s2"), state.RunOK)
	before := s.git.lsRemote(t, b.gitURL("mirrors", "s2"))
	b.waitNonEmpty(t, "mirrors", "s2")
	if got := b.getRepo(t, "mirrors", "s2").DefaultBranch; got != "main" {
		t.Fatalf("B default branch %q, want main", got)
	}

	w.commit("more.txt")
	w.push("main")
	w.git("checkout", "-q", "topic")
	w.git("reset", "-q", "--hard", "HEAD~1")
	w.commit("rewritten.txt")
	w.git("push", "-q", "--force", "origin", "topic")
	w.git("checkout", "-q", "main")
	w.push(":work", ":refs/tags/light")

	srcRefs := s.git.lsRemote(t, a.gitURL("alice", "s2"))
	if _, ok := srcRefs["refs/heads/work"]; ok {
		t.Fatal("work branch still on A")
	}
	r := m.one("b:mirrors/s2")
	wantStatus(t, r, state.RunOK)
	if d := wantStep(t, r, "git", state.StepOK).Detail; d != "0 created, 2 updated, 2 deleted" {
		t.Fatalf("git detail %q", d)
	}
	after := s.git.lsRemote(t, b.gitURL("mirrors", "s2"))
	equalRefs(t, "s2", after, srcRefs)
	if after["refs/heads/topic"] == before["refs/heads/topic"] || after["refs/heads/main"] == before["refs/heads/main"] {
		t.Fatal("B refs did not move")
	}
}

// testDefaultBranchAfterFirstPush is a regression test: Gitea makes the
// first branch of the first push into an empty repository its default
// branch, asynchronously, after receive-pack has returned. Pushing
// refs/heads/* in sorted order made a source with branch "feature" and
// default "main" yield a destination whose default branch was "feature";
// deleting "feature" at the source then made every later push fail (Gitea
// refuses to delete the default branch, and the push is atomic). gitx now
// pushes the source's default branch first into an empty destination, and
// the settings step corrects the default branch even after a rejected push.
func (s *suite) testDefaultBranchAfterFirstPush(t *testing.T) {
	a, b := s.a, s.b
	w := s.seed(t, a, "alice", "kb-default", "", "feature")
	m := s.newMirror(t, `
- name: kb
  from: {forge: a, repo: alice/kb-default}
  to: {forge: b, repo: mirrors/kb-default}
`)
	wantStatus(t, m.one("b:mirrors/kb-default"), state.RunOK)
	b.waitNonEmpty(t, "mirrors", "kb-default")
	got := b.getRepo(t, "mirrors", "kb-default").DefaultBranch
	if got != "main" {
		t.Errorf("B default branch %q, want main (the source's)", got)
	}
	w.push(":feature")
	r := m.one("b:mirrors/kb-default")
	if r.Status != state.RunOK {
		t.Errorf("after deleting branch feature at the source: status %s: %v", r.Status, r.Err)
	}
}

// ---- scenario 3: releases ----

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func (s *suite) testReleases(t *testing.T) {
	a, b := s.a, s.b
	s.seed(t, a, "alice", "s3", "")
	arp := a.prov.(forge.ReleaseProvider)
	brp := b.prov.(forge.ReleaseProvider)
	rel, err := arp.CreateRelease(ctxb(), "alice", "s3", forge.Release{Tag: "v1.0", Name: "Version 1", Body: "first body", Target: "main"})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"app.bin": randomBytes(t, 300<<10), "notes.txt": randomBytes(t, 1234)}
	for n, d := range files {
		if _, err := arp.UploadAsset(ctxb(), "alice", "s3", rel.ID, forge.Asset{Name: n, ContentType: "application/octet-stream"}, bytes.NewReader(d)); err != nil {
			t.Fatalf("upload %s to A: %v", n, err)
		}
	}
	m := s.newMirror(t, `
- name: s3
  from: {forge: a, repo: alice/s3}
  to: {forge: b, repo: mirrors/s3}
`)
	r := m.one("b:mirrors/s3")
	wantStatus(t, r, state.RunOK)
	wantStep(t, r, "releases", state.StepOK)

	check := func(wantBody string, want map[string][]byte) {
		t.Helper()
		rels, err := brp.ListReleases(ctxb(), "mirrors", "s3")
		if err != nil {
			t.Fatal(err)
		}
		if len(rels) != 1 || rels[0].Tag != "v1.0" || rels[0].Name != "Version 1" || rels[0].Body != wantBody {
			t.Fatalf("B releases: %+v", rels)
		}
		if len(rels[0].Assets) != len(want) {
			t.Fatalf("B assets: %+v", rels[0].Assets)
		}
		for _, as := range rels[0].Assets {
			d, ok := want[as.Name]
			if !ok {
				t.Fatalf("unexpected B asset %s", as.Name)
			}
			if got := download(t, b, "mirrors", "s3", as); sum(got) != sum(d) {
				t.Fatalf("B asset %s: sha256 %s (%d bytes), want %s (%d bytes)", as.Name, sum(got), len(got), sum(d), len(d))
			}
		}
	}
	check("first body", files)

	// Edit the body on A.
	cur, err := arp.ListReleases(ctxb(), "alice", "s3")
	if err != nil {
		t.Fatal(err)
	}
	upd := cur[0]
	upd.Body = "edited body"
	if _, err := arp.UpdateRelease(ctxb(), "alice", "s3", upd); err != nil {
		t.Fatal(err)
	}
	r = m.one("b:mirrors/s3")
	wantStatus(t, r, state.RunOK)
	if d := wantStep(t, r, "releases", state.StepOK).Detail; !strings.HasPrefix(d, "0 created, 1 updated") {
		t.Fatalf("releases detail %q", d)
	}
	check("edited body", files)

	// Replace an asset on A (new content, new size).
	for _, as := range cur[0].Assets {
		if as.Name == "app.bin" {
			if err := arp.DeleteAsset(ctxb(), "alice", "s3", cur[0].ID, as.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	files["app.bin"] = randomBytes(t, 200<<10)
	if _, err := arp.UploadAsset(ctxb(), "alice", "s3", cur[0].ID, forge.Asset{Name: "app.bin"}, bytes.NewReader(files["app.bin"])); err != nil {
		t.Fatal(err)
	}
	r = m.one("b:mirrors/s3")
	wantStatus(t, r, state.RunOK)
	if d := wantStep(t, r, "releases", state.StepOK).Detail; !strings.Contains(d, "1 assets uploaded (1 replaced)") {
		t.Fatalf("releases detail %q", d)
	}
	check("edited body", files)

	// An asset replaced by one of the same size is re-copied: the engine
	// tracks which source asset each destination asset was copied from.
	t.Run("SameSizeReplacement", func(t *testing.T) {
		cur, err := arp.ListReleases(ctxb(), "alice", "s3")
		if err != nil {
			t.Fatal(err)
		}
		for _, as := range cur[0].Assets {
			if as.Name == "notes.txt" {
				if err := arp.DeleteAsset(ctxb(), "alice", "s3", cur[0].ID, as.ID); err != nil {
					t.Fatal(err)
				}
			}
		}
		files["notes.txt"] = randomBytes(t, len(files["notes.txt"]))
		if _, err := arp.UploadAsset(ctxb(), "alice", "s3", cur[0].ID, forge.Asset{Name: "notes.txt"}, bytes.NewReader(files["notes.txt"])); err != nil {
			t.Fatal(err)
		}
		wantStatus(t, m.one("b:mirrors/s3"), state.RunOK)
		check("edited body", files)
	})
}

// ---- scenario 4: wiki ----

type wikiPage struct {
	Title string `json:"title"`
}

func (in *instance) wikiTitles(t *testing.T, owner, name string) []string {
	t.Helper()
	var pages []wikiPage
	in.mustAPI(t, http.MethodGet, "/repos/"+owner+"/"+name+"/wiki/pages?limit=50", nil, &pages, http.StatusOK)
	var out []string
	for _, p := range pages {
		out = append(out, p.Title)
	}
	slices.Sort(out)
	return out
}

func (s *suite) testWiki(t *testing.T) {
	a, b := s.a, s.b
	s.seed(t, a, "alice", "s4", "")
	for _, p := range []string{"Home", "Usage"} {
		a.mustAPI(t, http.MethodPost, "/repos/alice/s4/wiki/new", map[string]any{
			"title": p, "content_base64": base64.StdEncoding.EncodeToString([]byte("# " + p + "\n")), "message": "add " + p,
		}, nil, http.StatusCreated)
	}
	if got := a.getRepo(t, "alice", "s4"); !got.HasWiki {
		t.Fatalf("A repo with wiki pages: HasWiki=false")
	}
	m := s.newMirror(t, `
- name: s4
  from: {forge: a, repo: alice/s4}
  to: {forge: b, repo: mirrors/s4}
`)
	// Gitea refuses every git operation on a wiki that has no page yet; the
	// engine initialises the destination wiki (forge.WikiInitializer) before
	// the first push, so the very first run is ok.
	r := m.one("b:mirrors/s4")
	wantStatus(t, r, state.RunOK)
	wantStep(t, r, "wiki", state.StepOK)
	equalRefs(t, "wiki", s.git.lsRemote(t, b.wikiURL("mirrors", "s4")), s.git.lsRemote(t, a.wikiURL("alice", "s4")))
	if got, want := b.wikiTitles(t, "mirrors", "s4"), a.wikiTitles(t, "alice", "s4"); !slices.Equal(got, want) {
		t.Fatalf("B wiki pages %v, want %v", got, want)
	}

	// A later page propagates.
	a.mustAPI(t, http.MethodPost, "/repos/alice/s4/wiki/new", map[string]any{
		"title": "Later", "content_base64": base64.StdEncoding.EncodeToString([]byte("later\n")),
	}, nil, http.StatusCreated)
	r = m.one("b:mirrors/s4")
	wantStatus(t, r, state.RunOK)
	wantStep(t, r, "wiki", state.StepOK)
	if got, want := b.wikiTitles(t, "mirrors", "s4"), a.wikiTitles(t, "alice", "s4"); !slices.Equal(got, want) {
		t.Fatalf("B wiki pages %v, want %v", got, want)
	}
}

// ---- scenario 5: LFS ----

func (s *suite) testLFS(t *testing.T) {
	a, b := s.a, s.b
	a.createRepo(t, "s5", "", false)
	w := s.git.newWork(t, "main", a.gitURL("alice", "s5"))
	w.git("lfs", "install", "--local")
	w.git("lfs", "track", "*.bin")
	data := randomBytes(t, 512<<10)
	if err := os.WriteFile(filepath.Join(w.dir, "big.bin"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	w.git("add", ".gitattributes", "big.bin")
	w.git("commit", "-q", "-m", "lfs object")
	if out := w.git("lfs", "ls-files"); !strings.Contains(out, "big.bin") {
		t.Fatalf("big.bin is not an LFS file: %s", out)
	}
	w.push("main")
	a.waitNonEmpty(t, "alice", "s5")
	// A really holds the object (media endpoint resolves the pointer).
	if got := a.media(t, "alice", "s5", "big.bin"); !bytes.Equal(got, data) {
		t.Fatalf("A media: %d bytes", len(got))
	}

	m := s.newMirror(t, `
- name: s5
  from: {forge: a, repo: alice/s5}
  to: {forge: b, repo: mirrors/s5}
`)
	r := m.one("b:mirrors/s5")
	wantStatus(t, r, state.RunOK)
	wantStep(t, r, "lfs", state.StepOK)

	dir := filepath.Join(t.TempDir(), "clone")
	s.git.run("", "clone", "-q", b.gitURL("mirrors", "s5"), dir)
	got, err := os.ReadFile(filepath.Join(dir, "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("B clone big.bin: %d bytes (pointer? %v), want %d bytes", len(got), bytes.HasPrefix(got, []byte("version https://git-lfs")), len(data))
	}
	if got := b.media(t, "mirrors", "s5", "big.bin"); !bytes.Equal(got, data) {
		t.Fatalf("B media: %d bytes", len(got))
	}
}

// media fetches a file's content through the LFS-resolving media endpoint.
func (in *instance) media(t *testing.T, owner, name, file string) []byte {
	t.Helper()
	code, b := in.api(t, http.MethodGet, "/repos/"+owner+"/"+name+"/media/"+file, nil)
	if code != http.StatusOK {
		t.Fatalf("%s media %s: %d %s", in.name, file, code, b)
	}
	return b
}

// ---- scenario 6: both directions on one engine ----

func (s *suite) testReverse(t *testing.T) {
	a, b := s.a, s.b
	s.seed(t, a, "alice", "s6-fwd", "forward")
	s.seed(t, b, "bob", "s6-rev", "reverse", "dev")
	m := s.newMirror(t, `
- name: fwd
  from: {forge: a, repo: alice/s6-fwd}
  to: {forge: b, repo: mirrors/s6-fwd}
- name: rev
  from: {forge: b, repo: bob/s6-rev}
  to: {forge: a, repo: alice/s6-rev}
`)
	reps := m.run()
	wantStatus(t, reps["b:mirrors/s6-fwd"], state.RunOK)
	wantStatus(t, reps["a:alice/s6-rev"], state.RunOK)
	equalRefs(t, "fwd", s.git.lsRemote(t, b.gitURL("mirrors", "s6-fwd")), s.git.lsRemote(t, a.gitURL("alice", "s6-fwd")))
	equalRefs(t, "rev", s.git.lsRemote(t, a.gitURL("alice", "s6-rev")), s.git.lsRemote(t, b.gitURL("bob", "s6-rev")))
	if got := a.getRepo(t, "alice", "s6-rev"); !got.Private || got.Description != "reverse" {
		t.Fatalf("A reverse mirror: %+v", got)
	}
	if how, owned, _ := m.eng.Store.Owned(ctxb(), "a", "alice/s6-rev"); !owned || how != state.OwnedCreated {
		t.Fatalf("A reverse mirror ownership: %v %v", how, owned)
	}
}

// ---- scenario 7: adopt safety ----

func (s *suite) testAdopt(t *testing.T) {
	a, b := s.a, s.b
	if err := b.prov.(forge.OrgProvider).EnsureOrg(ctxb(), "mirrors"); err != nil {
		t.Fatal(err)
	}
	// Pre-existing B repo with its own content, not created by gitmirror.
	s.seed(t, b, "mirrors", "s7", "manual repo")
	s.seed(t, a, "alice", "s7", "from A")
	bBefore := s.git.lsRemote(t, b.gitURL("mirrors", "s7"))

	m := s.newMirror(t, `
- name: s7
  from: {forge: a, repo: alice/s7}
  to: {forge: b, repo: mirrors/s7}
`)
	r := m.one("b:mirrors/s7")
	wantStatus(t, r, state.RunFailed)
	if d := wantStep(t, r, "prepare", state.StepFailed).Detail; !strings.Contains(d, "not created by gitmirror") {
		t.Fatalf("prepare detail %q", d)
	}
	equalRefs(t, "B untouched", s.git.lsRemote(t, b.gitURL("mirrors", "s7")), bBefore)
	if got := b.getRepo(t, "mirrors", "s7"); got.Description != "manual repo" || got.Private {
		t.Fatalf("B repo changed: %+v", got)
	}

	m.setRules(`
- name: s7
  adopt: true
  from: {forge: a, repo: alice/s7}
  to: {forge: b, repo: mirrors/s7}
`)
	r = m.one("b:mirrors/s7")
	wantStatus(t, r, state.RunOK)
	if d := wantStep(t, r, "prepare", state.StepOK).Detail; !strings.Contains(d, "adopted") {
		t.Fatalf("prepare detail %q", d)
	}
	equalRefs(t, "adopted", s.git.lsRemote(t, b.gitURL("mirrors", "s7")), s.git.lsRemote(t, a.gitURL("alice", "s7")))
	if how, owned, _ := m.eng.Store.Owned(ctxb(), "b", "mirrors/s7"); !owned || how != state.OwnedAdopted {
		t.Fatalf("ownership: %v %v", how, owned)
	}
	if got := b.getRepo(t, "mirrors", "s7"); got.Description != "from A" || !got.Private {
		t.Fatalf("adopted B repo settings: %+v", got)
	}
}

// ---- scenario 8: empty-source guard ----

func (s *suite) testEmptySource(t *testing.T) {
	a, b := s.a, s.b
	w := s.seed(t, a, "alice", "s8", "", "develop")
	m := s.newMirror(t, `
- name: s8
  from: {forge: a, repo: alice/s8}
  to: {forge: b, repo: mirrors/s8}
`)
	wantStatus(t, m.one("b:mirrors/s8"), state.RunOK)
	bBefore := s.git.lsRemote(t, b.gitURL("mirrors", "s8"))

	// Delete every branch and tag on A. Gitea refuses to delete the default
	// branch over git, so that one goes last and is checked explicitly.
	w.push(":develop", ":refs/tags/light", ":refs/tags/v1.0")
	out, err := w.g.cmd(w.dir, "push", "origin", ":main")
	t.Logf("deleting the default branch over git: err=%v\n%s", err, out)
	if err != nil {
		// Make the source empty the only remaining way: delete the
		// repository and recreate it under the same name.
		a.mustAPI(t, http.MethodDelete, "/repos/alice/s8", nil, nil, http.StatusNoContent)
		a.createRepo(t, "s8", "", false)
	}
	if refs := s.git.lsRemote(t, a.gitURL("alice", "s8")); len(refs) != 0 {
		t.Fatalf("A still has refs: %v", refs)
	}

	r := m.one("b:mirrors/s8")
	wantStatus(t, r, state.RunAborted)
	if d := wantStep(t, r, "git", state.StepFailed).Detail; !strings.Contains(d, "refusing to prune") {
		t.Fatalf("git detail %q", d)
	}
	equalRefs(t, "B untouched", s.git.lsRemote(t, b.gitURL("mirrors", "s8")), bBefore)
}

// ---- scenario 9: archive ----

func (s *suite) testArchive(t *testing.T) {
	a, b := s.a, s.b
	w := s.seed(t, a, "alice", "s9", "")
	m := s.newMirror(t, `
- name: s9
  from: {forge: a, repo: alice/s9}
  to: {forge: b, repo: mirrors/s9}
`)
	wantStatus(t, m.one("b:mirrors/s9"), state.RunOK)
	if b.getRepo(t, "mirrors", "s9").Archived {
		t.Fatal("B archived before the source was")
	}

	// A last commit, then archive the source.
	w.commit("final.txt")
	w.push("main")
	yes := true
	if err := a.prov.UpdateRepoSettings(ctxb(), "alice", "s9", forge.Settings{Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	r := m.one("b:mirrors/s9")
	wantStatus(t, r, state.RunOK)
	wantStep(t, r, "git", state.StepOK)
	if d := wantStep(t, r, "finalize", state.StepOK).Detail; d != "archived destination" {
		t.Fatalf("finalize detail %q", d)
	}
	if !b.getRepo(t, "mirrors", "s9").Archived {
		t.Fatal("B not archived")
	}
	equalRefs(t, "s9", s.git.lsRemote(t, b.gitURL("mirrors", "s9")), s.git.lsRemote(t, a.gitURL("alice", "s9")))

	// Gitea refuses pushes into an archived repo, which is why the engine
	// archives only after every other step.
	bw := s.git.newWork(t, "main", b.gitURL("mirrors", "s9"))
	bw.git("fetch", "-q", "origin", "main")
	bw.git("reset", "-q", "--hard", "FETCH_HEAD")
	bw.commit("late.txt")
	out, err := bw.g.cmd(bw.dir, "push", "origin", "main")
	if err == nil || !strings.Contains(out, "archived") {
		t.Fatalf("push into archived B repo: err=%v\n%s", err, out)
	}
	t.Logf("push into archived repo refused: %s", lastLines(out, 2))

	// A further run unarchives, finds nothing to do, and re-archives.
	r = m.one("b:mirrors/s9")
	wantStatus(t, r, state.RunOK)
	wantStep(t, r, "git", state.StepUnchanged)
	if !b.getRepo(t, "mirrors", "s9").Archived {
		t.Fatal("B not archived after the second run")
	}
}

func lastLines(s string, n int) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, " | ")
}
