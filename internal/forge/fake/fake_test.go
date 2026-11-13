package fake

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"gitmirror/internal/forge"
)

var ctx = context.Background()

var (
	_ forge.Provider        = (*Provider)(nil)
	_ forge.ReleaseProvider = (*Provider)(nil)
	_ forge.WikiProvider    = (*Provider)(nil)
	_ forge.StarProvider    = (*Provider)(nil)
	_ forge.OrgProvider     = (*Provider)(nil)
	_ forge.WebhookProvider = (*Provider)(nil)
)

func TestTypeDefaultAndCustom(t *testing.T) {
	if got := New(Options{}).Type(); got != "fake" {
		t.Fatalf("default type = %q", got)
	}
	if got := New(Options{Type: "x"}).Type(); got != "x" {
		t.Fatalf("type = %q", got)
	}
}

func TestGetRepoNotFoundAndCaseInsensitive(t *testing.T) {
	p := New(Options{})
	_, err := p.GetRepo(ctx, "o", "n")
	if !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	p.AddRepo(forge.Repo{Owner: "Alice", Name: "MyRepo", Description: "d"})
	r, err := p.GetRepo(ctx, "alice", "myrepo")
	if err != nil {
		t.Fatal(err)
	}
	if r.Owner != "Alice" || r.Name != "MyRepo" {
		t.Fatalf("casing not preserved: %+v", r)
	}
}

func TestListReposSortedAndUnknownOwner(t *testing.T) {
	p := New(Options{})
	p.AddRepo(forge.Repo{Owner: "o", Name: "zeta"})
	p.AddRepo(forge.Repo{Owner: "O", Name: "alpha"})
	p.AddRepo(forge.Repo{Owner: "other", Name: "x"})
	rs, err := p.ListRepos(ctx, "o")
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 || rs[0].Name != "alpha" || rs[1].Name != "zeta" {
		t.Fatalf("got %+v", rs)
	}
	if _, err := p.ListRepos(ctx, "nobody"); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	p.AddOwner("empty", true)
	rs, err = p.ListRepos(ctx, "empty")
	if err != nil || len(rs) != 0 {
		t.Fatalf("empty owner: %v %v", rs, err)
	}
}

func TestCopyIsolation(t *testing.T) {
	p := New(Options{})
	seed := forge.Repo{Owner: "o", Name: "n", Topics: []string{"a", "b"}}
	p.AddRepo(seed)
	seed.Topics[0] = "mutated-seed"

	r, _ := p.GetRepo(ctx, "o", "n")
	if r.Topics[0] != "a" {
		t.Fatalf("seed aliasing: %v", r.Topics)
	}
	r.Topics[0] = "x"
	r.Description = "changed"
	r2, _ := p.GetRepo(ctx, "o", "n")
	if r2.Topics[0] != "a" || r2.Description != "" {
		t.Fatalf("GetRepo aliasing: %+v", r2)
	}
	list, _ := p.ListRepos(ctx, "o")
	list[0].Topics[1] = "y"
	r3, _ := p.GetRepo(ctx, "o", "n")
	if r3.Topics[1] != "b" {
		t.Fatalf("ListRepos aliasing: %+v", r3)
	}
}

func TestCreateRepoRules(t *testing.T) {
	p := New(Options{Username: "me"})
	r, err := p.CreateRepo(ctx, forge.CreateOptions{Owner: "me", Name: "n", Description: "d", Private: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.Description != "d" || !r.Private || !r.Empty || r.DefaultBranch == "" {
		t.Fatalf("unexpected repo %+v", r)
	}
	_, err = p.CreateRepo(ctx, forge.CreateOptions{Owner: "ME", Name: "N"})
	if err == nil || errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("duplicate: want non-NotFound error, got %v", err)
	}
	_, err = p.CreateRepo(ctx, forge.CreateOptions{Owner: "ghost", Name: "n"})
	if !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("missing owner: want ErrNotFound, got %v", err)
	}
	p.AddOwner("org", true)
	if _, err := p.CreateRepo(ctx, forge.CreateOptions{Owner: "org", Name: "n"}); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureOrgIdempotent(t *testing.T) {
	p := New(Options{})
	for i := 0; i < 2; i++ {
		if err := p.EnsureOrg(ctx, "neworg"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.CreateRepo(ctx, forge.CreateOptions{Owner: "neworg", Name: "r"}); err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureOrg(ctx, "neworg"); err != nil {
		t.Fatal(err)
	}
	rs, err := p.ListRepos(ctx, "neworg")
	if err != nil || len(rs) != 1 {
		t.Fatalf("EnsureOrg must not wipe repos: %v %v", rs, err)
	}
}

func TestUpdateRepoSettings(t *testing.T) {
	p := New(Options{})
	p.AddRepo(forge.Repo{Owner: "o", Name: "n", Description: "old", Homepage: "h", Topics: []string{"t"}, DefaultBranch: "master"})
	desc, br, priv, arch := "new", "main", true, true
	home := "http://x"
	topics := []string{"a", "b"}
	if err := p.UpdateRepoSettings(ctx, "O", "N", forge.Settings{Description: &desc}); err != nil {
		t.Fatal(err)
	}
	r, _ := p.GetRepo(ctx, "o", "n")
	if r.Description != "new" || r.Homepage != "h" || r.DefaultBranch != "master" || r.Private || r.Archived || len(r.Topics) != 1 {
		t.Fatalf("partial update leaked: %+v", r)
	}
	err := p.UpdateRepoSettings(ctx, "o", "n", forge.Settings{Homepage: &home, Topics: &topics, DefaultBranch: &br, Private: &priv, Archived: &arch})
	if err != nil {
		t.Fatal(err)
	}
	topics[0] = "mutated"
	r, _ = p.GetRepo(ctx, "o", "n")
	want := forge.Repo{Owner: "o", Name: "n", Description: "new", Homepage: home, Topics: []string{"a", "b"}, DefaultBranch: "main", Private: true, Archived: true}
	if !reflect.DeepEqual(*r, want) {
		t.Fatalf("got %+v want %+v", *r, want)
	}
	if err := p.UpdateRepoSettings(ctx, "o", "missing", forge.Settings{}); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestGitURLWithoutBaseDir(t *testing.T) {
	p := New(Options{})
	if got := p.GitURL("o", "n"); got != "https://fake.invalid/o/n.git" {
		t.Fatalf("got %q", got)
	}
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

func lsRemote(t *testing.T, url string) {
	t.Helper()
	out, err := exec.Command("git", "ls-remote", url).CombinedOutput()
	if err != nil {
		t.Fatalf("git ls-remote %s: %v\n%s", url, err, out)
	}
}

func TestGitBareRepoCreation(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	p := New(Options{GitBaseDir: dir, Username: "me"})
	if _, err := p.CreateRepo(ctx, forge.CreateOptions{Owner: "me", Name: "created"}); err != nil {
		t.Fatal(err)
	}
	if err := p.AddRepo(forge.Repo{Owner: "src", Name: "seeded"}); err != nil {
		t.Fatal(err)
	}
	for _, rn := range [][2]string{{"me", "created"}, {"src", "seeded"}} {
		u := p.GitURL(rn[0], rn[1])
		if !strings.HasPrefix(u, "file://") {
			t.Fatalf("not a file URL: %s", u)
		}
		t.Logf("GitURL = %s", u)
		lsRemote(t, u)
	}
	// AddRepo again must not fail on existing bare repo.
	if err := p.AddRepo(forge.Repo{Owner: "src", Name: "seeded"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(p.GitURL("src", "seeded"), filepath.ToSlash("/src/seeded.git")) {
		t.Fatalf("unexpected URL %s", p.GitURL("src", "seeded"))
	}
}

func TestWikiInit(t *testing.T) {
	requireGit(t)
	p := New(Options{GitBaseDir: t.TempDir()})
	p.AddRepo(forge.Repo{Owner: "o", Name: "n"})
	r, _ := p.GetRepo(ctx, "o", "n")
	if r.HasWiki {
		t.Fatal("HasWiki before InitWiki")
	}
	u := p.WikiGitURL("o", "n")
	if !strings.HasSuffix(u, "/o/n.wiki.git") {
		t.Fatalf("wiki url %s", u)
	}
	if err := p.InitWiki("o", "n"); err != nil {
		t.Fatal(err)
	}
	lsRemote(t, u)
	r, _ = p.GetRepo(ctx, "o", "n")
	if !r.HasWiki {
		t.Fatal("HasWiki not set")
	}
	if err := p.InitWiki("o", "missing"); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if got := New(Options{}).WikiGitURL("o", "n"); got != "https://fake.invalid/o/n.wiki.git" {
		t.Fatalf("got %q", got)
	}
}

func TestAuthAndRate(t *testing.T) {
	p := New(Options{Username: "u", Token: "tok"})
	if a := p.GitAuth(); a.Username != "u" || a.Token != "tok" {
		t.Fatalf("auth %+v", a)
	}
	rate := forge.Rate{Limit: 10, Remaining: 3, Reset: time.Unix(100, 0)}
	p.SetRate(rate)
	got, err := p.RateLimit(ctx)
	if err != nil || got != rate {
		t.Fatalf("rate %+v %v", got, err)
	}
}

func newReleaseFixture(t *testing.T) *Provider {
	t.Helper()
	p := New(Options{})
	p.AddRepo(forge.Repo{Owner: "o", Name: "n"})
	return p
}

func TestReleaseCRUD(t *testing.T) {
	p := newReleaseFixture(t)
	r1, err := p.CreateRelease(ctx, "o", "n", forge.Release{Tag: "v1", Name: "one"})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := p.CreateRelease(ctx, "O", "N", forge.Release{Tag: "v2", Name: "two", Draft: true})
	if err != nil {
		t.Fatal(err)
	}
	if r1.ID == 0 || r2.ID != r1.ID+1 {
		t.Fatalf("ids %d %d", r1.ID, r2.ID)
	}
	if _, err := p.CreateRelease(ctx, "o", "n", forge.Release{Tag: "v1"}); err == nil || errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("dup tag: %v", err)
	}
	list, err := p.ListReleases(ctx, "o", "n")
	if err != nil || len(list) != 2 || list[0].Tag != "v2" || list[1].Tag != "v1" {
		t.Fatalf("list %+v %v", list, err)
	}
	upd, err := p.UpdateRelease(ctx, "o", "n", forge.Release{ID: r1.ID, Tag: "v1", Name: "renamed", Body: "b", Prerelease: true})
	if err != nil || upd.Name != "renamed" || !upd.Prerelease {
		t.Fatalf("update %+v %v", upd, err)
	}
	if _, err := p.UpdateRelease(ctx, "o", "n", forge.Release{ID: 999}); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("update missing: %v", err)
	}
	if err := p.DeleteRelease(ctx, "o", "n", r1.ID); err != nil {
		t.Fatal(err)
	}
	if err := p.DeleteRelease(ctx, "o", "n", r1.ID); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("delete missing: %v", err)
	}
	// IDs never reused.
	r3, _ := p.CreateRelease(ctx, "o", "n", forge.Release{Tag: "v3"})
	if r3.ID <= r2.ID {
		t.Fatalf("id reuse %d", r3.ID)
	}
}

func TestReleaseOnMissingRepo(t *testing.T) {
	p := New(Options{})
	checks := map[string]error{}
	_, checks["list"] = p.ListReleases(ctx, "o", "n")
	_, checks["create"] = p.CreateRelease(ctx, "o", "n", forge.Release{Tag: "v"})
	_, checks["update"] = p.UpdateRelease(ctx, "o", "n", forge.Release{ID: 1})
	checks["delete"] = p.DeleteRelease(ctx, "o", "n", 1)
	_, checks["download"] = p.DownloadAsset(ctx, "o", "n", forge.Asset{ID: 1})
	_, checks["upload"] = p.UploadAsset(ctx, "o", "n", 1, forge.Asset{Name: "a"}, strings.NewReader("x"))
	checks["delasset"] = p.DeleteAsset(ctx, "o", "n", 1, 1)
	for k, err := range checks {
		if !errors.Is(err, forge.ErrNotFound) {
			t.Errorf("%s: want ErrNotFound, got %v", k, err)
		}
	}
}

func TestAssetRoundTrip(t *testing.T) {
	p := newReleaseFixture(t)
	rel, _ := p.CreateRelease(ctx, "o", "n", forge.Release{Tag: "v1"})
	payload := []byte("hello\x00binary\xffdata")
	a, err := p.UploadAsset(ctx, "o", "n", rel.ID, forge.Asset{Name: "f.bin", Size: 12345, ContentType: "application/x"}, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == 0 || a.Size != int64(len(payload)) || a.ContentType != "application/x" || a.Name != "f.bin" {
		t.Fatalf("asset %+v", a)
	}
	rc, err := p.DownloadAsset(ctx, "o", "n", *a)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, payload) {
		t.Fatalf("bytes %q", got)
	}
	if ab := p.AssetBytes("o", "n", rel.ID, a.ID); !bytes.Equal(ab, payload) {
		t.Fatalf("AssetBytes %q", ab)
	}
	// AssetBytes returns a copy.
	p.AssetBytes("o", "n", rel.ID, a.ID)[0] = 'X'
	if ab := p.AssetBytes("o", "n", rel.ID, a.ID); ab[0] != 'h' {
		t.Fatal("AssetBytes aliasing")
	}
	// Source slice mutation doesn't change stored bytes.
	payload[0] = 'Z'
	if ab := p.AssetBytes("o", "n", rel.ID, a.ID); ab[0] != 'h' {
		t.Fatal("upload aliasing")
	}
	if _, err := p.UploadAsset(ctx, "o", "n", rel.ID, forge.Asset{Name: "f.bin"}, strings.NewReader("y")); err == nil || errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("dup asset: %v", err)
	}
	if _, err := p.UploadAsset(ctx, "o", "n", 999, forge.Asset{Name: "z"}, strings.NewReader("y")); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("upload missing release: %v", err)
	}
	if _, err := p.DownloadAsset(ctx, "o", "n", forge.Asset{ID: 999}); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("download missing: %v", err)
	}
	if p.AssetBytes("o", "n", rel.ID, 999) != nil {
		t.Fatal("AssetBytes for missing should be nil")
	}
	// Asset listed in release.
	list, _ := p.ListReleases(ctx, "o", "n")
	if len(list[0].Assets) != 1 || list[0].Assets[0].Size != int64(len(got)) {
		t.Fatalf("assets %+v", list[0].Assets)
	}
	if err := p.DeleteAsset(ctx, "o", "n", rel.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := p.DeleteAsset(ctx, "o", "n", rel.ID, a.ID); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("delete asset missing: %v", err)
	}
	if err := p.DeleteAsset(ctx, "o", "n", 999, 1); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("delete asset missing release: %v", err)
	}
}

func TestReleaseCopyIsolation(t *testing.T) {
	p := newReleaseFixture(t)
	rel, _ := p.CreateRelease(ctx, "o", "n", forge.Release{Tag: "v1"})
	if _, err := p.UploadAsset(ctx, "o", "n", rel.ID, forge.Asset{Name: "a"}, strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	list, _ := p.ListReleases(ctx, "o", "n")
	list[0].Assets[0].Name = "mutated"
	list[0].Tag = "mutated"
	list2, _ := p.ListReleases(ctx, "o", "n")
	if list2[0].Assets[0].Name != "a" || list2[0].Tag != "v1" {
		t.Fatalf("aliasing: %+v", list2[0])
	}
	// Create with input assets must not install assets.
	in := forge.Release{Tag: "v2", Assets: []forge.Asset{{ID: 5, Name: "ghost"}}}
	out, _ := p.CreateRelease(ctx, "o", "n", in)
	if len(out.Assets) != 0 {
		t.Fatalf("CreateRelease must ignore input assets: %+v", out.Assets)
	}
	// Update preserves assets.
	upd, _ := p.UpdateRelease(ctx, "o", "n", forge.Release{ID: rel.ID, Tag: "v1", Name: "n"})
	if len(upd.Assets) != 1 {
		t.Fatalf("update dropped assets: %+v", upd)
	}
}

func TestStars(t *testing.T) {
	p := New(Options{})
	p.Star("u", forge.Repo{Owner: "b", Name: "x", Topics: []string{"t"}})
	p.Star("u", forge.Repo{Owner: "a", Name: "z"})
	p.Star("u", forge.Repo{Owner: "a", Name: "y"})
	got, err := p.ListStarred(ctx, "u")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range got {
		names = append(names, r.FullName())
	}
	if !reflect.DeepEqual(names, []string{"a/y", "a/z", "b/x"}) {
		t.Fatalf("got %v", names)
	}
	got[2].Topics[0] = "mut"
	got2, _ := p.ListStarred(ctx, "u")
	if got2[2].Topics[0] != "t" {
		t.Fatal("aliasing")
	}
	none, err := p.ListStarred(ctx, "nobody")
	if err != nil || len(none) != 0 {
		t.Fatalf("nobody: %v %v", none, err)
	}
}

func webhookReq(secret string, body string, sig string) *http.Request {
	req := httptest.NewRequest("POST", "/hook", strings.NewReader(body))
	if sig == "auto" {
		sig = Sign(secret, []byte(body))
	}
	if sig != "" {
		req.Header.Set("X-Fake-Signature", sig)
	}
	return req
}

func TestWebhook(t *testing.T) {
	p := New(Options{})
	body := `{"kind":"push","owner":"o","name":"n"}`
	ev, err := p.ParseWebhook(webhookReq("s3", body, "auto"), "s3")
	if err != nil {
		t.Fatal(err)
	}
	if *ev != (forge.Event{Kind: forge.EventPush, Owner: "o", Name: "n"}) {
		t.Fatalf("event %+v", ev)
	}
	if !strings.HasPrefix(Sign("k", []byte("b")), "sha256=") {
		t.Fatal("Sign prefix")
	}
	ev, err = p.ParseWebhook(webhookReq("s3", `{"kind":"weird","owner":"o","name":"n"}`, "auto"), "s3")
	if err != nil || ev.Kind != forge.EventOther {
		t.Fatalf("unknown kind: %+v %v", ev, err)
	}
	ev, err = p.ParseWebhook(webhookReq("s3", `{"kind":"release","owner":"o","name":"n"}`, "auto"), "s3")
	if err != nil || ev.Kind != forge.EventRelease {
		t.Fatalf("release kind: %+v %v", ev, err)
	}
	for name, req := range map[string]*http.Request{
		"wrong secret": webhookReq("other", body, "auto"),
		"missing":      webhookReq("s3", body, ""),
		"garbage":      webhookReq("s3", body, "sha256=zz"),
		"tampered":     webhookReq("s3", body+" ", Sign("s3", []byte(body))),
	} {
		if _, err := p.ParseWebhook(req, "s3"); !errors.Is(err, forge.ErrBadSignature) {
			t.Errorf("%s: want ErrBadSignature, got %v", name, err)
		}
	}
	_, err = p.ParseWebhook(webhookReq("", body, "auto"), "")
	if err == nil {
		t.Fatal("empty secret must be rejected")
	}
	if _, err := p.ParseWebhook(webhookReq("s3", "not json", "auto"), "s3"); err == nil || errors.Is(err, forge.ErrBadSignature) {
		t.Fatalf("bad json: %v", err)
	}
}

func TestFailNextOneShot(t *testing.T) {
	p := New(Options{})
	boom := errors.New("boom")
	p.FailNext("CreateRepo", boom)
	p.AddOwner("o", false)
	if _, err := p.CreateRepo(ctx, forge.CreateOptions{Owner: "o", Name: "n"}); !errors.Is(err, boom) {
		t.Fatalf("want boom, got %v", err)
	}
	if _, err := p.CreateRepo(ctx, forge.CreateOptions{Owner: "o", Name: "n"}); err != nil {
		t.Fatalf("second call should succeed: %v", err)
	}
	// Failed call must have had no side effect: repo exists exactly once.
	rs, _ := p.ListRepos(ctx, "o")
	if len(rs) != 1 {
		t.Fatalf("repos %v", rs)
	}
	// Queued failures fire in order.
	e1, e2 := errors.New("e1"), errors.New("e2")
	p.FailNext("ListReleases", e1)
	p.FailNext("ListReleases", e2)
	if _, err := p.ListReleases(ctx, "o", "n"); !errors.Is(err, e1) {
		t.Fatalf("got %v", err)
	}
	if _, err := p.ListReleases(ctx, "o", "n"); !errors.Is(err, e2) {
		t.Fatalf("got %v", err)
	}
	if _, err := p.ListReleases(ctx, "o", "n"); err != nil {
		t.Fatal(err)
	}
}

func TestFailAlwaysAndClear(t *testing.T) {
	p := New(Options{})
	p.AddRepo(forge.Repo{Owner: "o", Name: "n"})
	boom := errors.New("boom")
	p.FailAlways("GetRepo", boom)
	for i := 0; i < 3; i++ {
		if _, err := p.GetRepo(ctx, "o", "n"); !errors.Is(err, boom) {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	p.FailNext("ListRepos", boom)
	p.ClearFailures()
	if _, err := p.GetRepo(ctx, "o", "n"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ListRepos(ctx, "o"); err != nil {
		t.Fatal(err)
	}
}

func TestFailureInjectionEveryErrorMethod(t *testing.T) {
	p := New(Options{})
	p.AddRepo(forge.Repo{Owner: "o", Name: "n"})
	boom := errors.New("boom")
	calls := map[string]func() error{
		"GetRepo":            func() error { _, e := p.GetRepo(ctx, "o", "n"); return e },
		"ListRepos":          func() error { _, e := p.ListRepos(ctx, "o"); return e },
		"CreateRepo":         func() error { _, e := p.CreateRepo(ctx, forge.CreateOptions{Owner: "o", Name: "m"}); return e },
		"UpdateRepoSettings": func() error { return p.UpdateRepoSettings(ctx, "o", "n", forge.Settings{}) },
		"RateLimit":          func() error { _, e := p.RateLimit(ctx); return e },
		"ListReleases":       func() error { _, e := p.ListReleases(ctx, "o", "n"); return e },
		"CreateRelease":      func() error { _, e := p.CreateRelease(ctx, "o", "n", forge.Release{Tag: "t"}); return e },
		"UpdateRelease":      func() error { _, e := p.UpdateRelease(ctx, "o", "n", forge.Release{}); return e },
		"DeleteRelease":      func() error { return p.DeleteRelease(ctx, "o", "n", 1) },
		"DownloadAsset":      func() error { _, e := p.DownloadAsset(ctx, "o", "n", forge.Asset{}); return e },
		"UploadAsset": func() error {
			_, e := p.UploadAsset(ctx, "o", "n", 1, forge.Asset{Name: "a"}, strings.NewReader(""))
			return e
		},
		"DeleteAsset":  func() error { return p.DeleteAsset(ctx, "o", "n", 1, 1) },
		"ListStarred":  func() error { _, e := p.ListStarred(ctx, "u"); return e },
		"EnsureOrg":    func() error { return p.EnsureOrg(ctx, "org") },
		"ParseWebhook": func() error { _, e := p.ParseWebhook(webhookReq("s", "{}", "auto"), "s"); return e },
	}
	for name, fn := range calls {
		p.FailNext(name, boom)
		if err := fn(); !errors.Is(err, boom) {
			t.Errorf("%s did not honour FailNext: %v", name, err)
		}
	}
}

func TestCallsOrder(t *testing.T) {
	p := New(Options{})
	p.AddRepo(forge.Repo{Owner: "o", Name: "n"}) // seeding helpers are not recorded
	p.GetRepo(ctx, "o", "n")
	p.GitURL("o", "n")
	p.ListRepos(ctx, "o")
	p.GetRepo(ctx, "o", "missing")
	got := p.Calls()
	want := []string{"GetRepo", "GitURL", "ListRepos", "GetRepo"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	got[0] = "mut"
	if p.Calls()[0] != "GetRepo" {
		t.Fatal("Calls aliasing")
	}
}

func TestCancelledContext(t *testing.T) {
	p := New(Options{})
	p.AddRepo(forge.Repo{Owner: "o", Name: "n"})
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.GetRepo(cctx, "o", "n"); !errors.Is(err, context.Canceled) {
		t.Fatalf("GetRepo: %v", err)
	}
	if _, err := p.ListReleases(cctx, "o", "n"); !errors.Is(err, context.Canceled) {
		t.Fatalf("ListReleases: %v", err)
	}
	if err := p.EnsureOrg(cctx, "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("EnsureOrg: %v", err)
	}
	if _, err := p.RateLimit(cctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("RateLimit: %v", err)
	}
	// Cancelled call must not mutate.
	if _, err := p.CreateRepo(cctx, forge.CreateOptions{Owner: "o", Name: "new"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("CreateRepo: %v", err)
	}
	if _, err := p.GetRepo(ctx, "o", "new"); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("cancelled create leaked: %v", err)
	}
}

func TestConcurrencySmoke(t *testing.T) {
	p := New(Options{Username: "me"})
	p.AddRepo(forge.Repo{Owner: "o", Name: "shared"})
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				name := fmt.Sprintf("r%d-%d", g, i)
				p.CreateRepo(ctx, forge.CreateOptions{Owner: "me", Name: name})
				p.GetRepo(ctx, "me", name)
				p.ListRepos(ctx, "me")
				tag := fmt.Sprintf("v%d-%d", g, i)
				rel, err := p.CreateRelease(ctx, "o", "shared", forge.Release{Tag: tag})
				if err == nil {
					p.UploadAsset(ctx, "o", "shared", rel.ID, forge.Asset{Name: "a"}, strings.NewReader("data"))
				}
				p.ListReleases(ctx, "o", "shared")
				p.SetRate(forge.Rate{Limit: i})
				p.RateLimit(ctx)
				p.Star("u", forge.Repo{Owner: "o", Name: name})
				p.ListStarred(ctx, "u")
				p.Calls()
				p.GitURL("o", name)
			}
		}(g)
	}
	wg.Wait()
	rs, err := p.ListRepos(ctx, "me")
	if err != nil || len(rs) != 16*50 {
		t.Fatalf("repos %d %v", len(rs), err)
	}
	rels, _ := p.ListReleases(ctx, "o", "shared")
	if len(rels) != 16*50 {
		t.Fatalf("releases %d", len(rels))
	}
}
