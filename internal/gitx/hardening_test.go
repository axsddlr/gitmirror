package gitx

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// recorder is an HTTP handler wrapper that records every request it sees and
// whether it carried an Authorization header.
type recorder struct {
	mu    sync.Mutex
	hits  []string
	authd []string
	next  http.Handler // nil: answer 404
}

func (rc *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rc.mu.Lock()
	rc.hits = append(rc.hits, r.Method+" "+r.URL.Path)
	if len(r.Header.Values("Authorization")) > 0 {
		rc.authd = append(rc.authd, r.Method+" "+r.URL.Path)
	}
	rc.mu.Unlock()
	if rc.next == nil {
		io.Copy(io.Discard, r.Body)
		http.NotFound(w, r)
		return
	}
	rc.next.ServeHTTP(w, r)
}

func (rc *recorder) seen() (hits, authd []string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([]string(nil), rc.hits...), append([]string(nil), rc.authd...)
}

func startServer(t *testing.T, h http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestHTTPRedirectToOtherHostNotFollowed: the source origin answers every
// request with a 302 to a second server that would happily serve the repo.
// git must not follow it, so the second server never sees a request (and in
// particular never the source's Authorization header).
func TestHTTPRedirectToOtherHostNotFollowed(t *testing.T) {
	f := newFixture(t)
	f.commit("a.txt")
	f.push("main")
	tok := "SRC-" + randomHex(t, 12)

	target := &recorder{next: gitBackend(t, f.root)}
	targetURL := startServer(t, target)
	origin := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		http.Redirect(w, r, targetURL+r.URL.RequestURI(), http.StatusFound)
	}))
	src := Remote{URL: origin + "/src.git", Token: tok}

	_, lsErr := f.r.LsRemote(context.Background(), src)
	_, syncErr := f.c.Sync(context.Background(), src, f.dstRemote, SyncOptions{Prune: true})

	hits, authd := target.seen()
	if len(authd) > 0 {
		t.Fatalf("redirect target received the source token on %v", authd)
	}
	if len(hits) > 0 {
		t.Fatalf("git followed the redirect to another host: %v", hits)
	}
	if lsErr == nil || syncErr == nil {
		t.Fatalf("ls-remote/sync through a redirect succeeded: ls=%v sync=%v", lsErr, syncErr)
	}
	if n := len(refsOf(t, f.env, f.dst)); n != 0 {
		t.Fatalf("dst got %d refs through a redirect", n)
	}
	for _, c := range f.captured() {
		if !cmdHasArgPair(c.Args, "-c", "http.followRedirects=false") {
			t.Errorf("command %q does not set http.followRedirects=false", c.Args)
		}
	}
}

// TestLFSBatchRedirectToOtherHostDropsToken checks git-lfs's own redirect
// handling: the source's LFS batch endpoint redirects (307) to another host.
// git-lfs must not send our origin-scoped Authorization header there.
func TestLFSBatchRedirectToOtherHostDropsToken(t *testing.T) {
	requireLFS(t)
	f := newFixture(t)
	lfsCommit(t, f, "main")
	f.push("main")
	tok := "SRC-" + randomHex(t, 12)
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+tok))

	target := &recorder{}
	targetURL := startServer(t, target)
	backend := gitBackend(t, f.root)
	var lfsHits int
	var mu sync.Mutex
	origin := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != wantAuth {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if strings.Contains(r.URL.Path, "/info/lfs/") {
			mu.Lock()
			lfsHits++
			mu.Unlock()
			io.Copy(io.Discard, r.Body)
			http.Redirect(w, r, targetURL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	src := Remote{URL: origin + "/src.git", Token: tok}
	_, err := f.c.Sync(context.Background(), src, f.dstRemote, SyncOptions{Prune: true, LFS: true})
	hits, authd := target.seen()
	if len(authd) > 0 {
		t.Fatalf("git-lfs sent the source token to the redirect target: %v", authd)
	}
	if lfsHits == 0 {
		t.Fatalf("fixture: the source LFS endpoint was never asked (sync err %v)", err)
	}
	if err == nil {
		t.Fatal("sync succeeded although the LFS endpoint redirected to a server without the objects")
	}
	t.Logf("redirect target hits (without Authorization): %v; sync error: %v", hits, err)
}

func cmdHasArgPair(args []string, a, b string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == a && args[i+1] == b {
			return true
		}
	}
	return false
}

// lfsCommit commits one random LFS-tracked file on the work tree's current
// branch and returns its oid count expectations via lfsObjects later.
func lfsCommit(t *testing.T, f *fixture, branch string) {
	t.Helper()
	f.git("lfs", "install", "--local")
	f.git("lfs", "track", "*.bin")
	blob := make([]byte, 4096)
	rand.Read(blob)
	if err := os.WriteFile(filepath.Join(f.work, "a.bin"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
	f.git("add", ".gitattributes", "a.bin")
	f.git("commit", "-q", "-m", "lfs on "+branch)
}

// TestLFSConfigInMirroredContentIgnored: a source branch named master (the
// default HEAD of a fresh bare repo) carries a .lfsconfig that points LFS at
// an attacker endpoint. git-lfs must keep using the real remotes' endpoints.
func TestLFSConfigInMirroredContentIgnored(t *testing.T) {
	requireLFS(t)
	cases := []struct {
		name      string
		lfsconfig func(evil string) string
	}{
		{"http url", func(evil string) string {
			return "[lfs]\n\turl = " + evil + "/steal.git/info/lfs\n\tpushurl = " + evil + "/steal-push.git/info/lfs\n"
		}},
		{"ssh pushurl", func(string) string { return "[lfs]\n\tpushurl = ssh://git@evil.invalid/x/y.git\n" }},
		{"remote lfsurl", func(evil string) string {
			return "[remote \"origin\"]\n\tlfsurl = " + evil + "/r.git/info/lfs\n[lfs]\n\turl = " + evil + "/u.git/info/lfs\n"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			evil := &recorder{}
			evilURL := startServer(t, evil)
			lfsCommit(t, f, "main")
			f.push("main") // uploads the LFS object to src
			if err := os.WriteFile(filepath.Join(f.work, ".lfsconfig"), []byte(tc.lfsconfig(evilURL)), 0o644); err != nil {
				t.Fatal(err)
			}
			f.git("add", ".lfsconfig")
			f.git("commit", "-q", "-m", "lfsconfig")
			// --no-verify: the LFS pre-push hook would itself obey .lfsconfig.
			f.git("push", "-q", "--no-verify", "origin", "main:master")
			srcOIDs := lfsObjects(t, f.src)
			if len(srcOIDs) != 1 {
				t.Fatalf("fixture: src LFS objects = %v", srcOIDs)
			}

			_, err := f.c.Sync(context.Background(), f.srcRemote, f.dstRemote, SyncOptions{Prune: true, LFS: true})
			hits, _ := evil.seen()
			if len(hits) > 0 {
				t.Fatalf(".lfsconfig endpoint was contacted: %v (sync err %v)", hits, err)
			}
			// An ssh:// endpoint taken from .lfsconfig makes git-lfs run ssh
			// against evil.invalid, which fails the sync.
			if err != nil {
				t.Fatalf("Sync: %v", err)
			}
			equalStrings(t, "dst LFS objects", lfsObjects(t, f.dst), srcOIDs)
			if head := tgit(t, f.env, f.cache, "symbolic-ref", "HEAD"); head != "refs/gitmirror/none" {
				t.Fatalf("cache HEAD = %q, want refs/gitmirror/none", head)
			}
			var lfsCmds int
			for _, c := range f.captured() {
				j := strings.Join(c.Args, " ")
				if !strings.Contains(j, " lfs fetch ") && !strings.Contains(j, " lfs push ") {
					continue
				}
				lfsCmds++
				want := f.srcRemote.URL
				if strings.Contains(j, " lfs push ") {
					want = f.dstRemote.URL
				}
				if !cmdHasArgPair(c.Args, "-c", "lfs.url="+want) || !cmdHasArgPair(c.Args, "-c", "lfs.pushurl="+want) {
					t.Errorf("lfs command %q does not pin lfs.url/lfs.pushurl to %s", c.Args, want)
				}
			}
			if lfsCmds != 2 {
				t.Fatalf("saw %d lfs fetch/push commands, want 2", lfsCmds)
			}
		})
	}
}

// TestCacheHeadPointsNowhereForExistingCache: a cache created by an older
// version (HEAD = refs/heads/master) is re-pointed on the next sync.
func TestCacheHeadPointsNowhereForExistingCache(t *testing.T) {
	f := newFixture(t)
	f.commit("a.txt")
	f.push("main")
	tgit(t, f.env, f.root, "init", "-q", "--bare", f.cache)
	tgit(t, f.env, f.cache, "symbolic-ref", "HEAD", "refs/heads/master")
	f.sync(SyncOptions{})
	if head := tgit(t, f.env, f.cache, "symbolic-ref", "HEAD"); head != "refs/gitmirror/none" {
		t.Fatalf("cache HEAD = %q, want refs/gitmirror/none", head)
	}
}

func TestLFSEndpoint(t *testing.T) {
	for in, want := range map[string]string{
		"https://h.example/o/r":             "https://h.example/o/r.git/info/lfs",
		"https://h.example/o/r.git":         "https://h.example/o/r.git/info/lfs",
		"https://h.example/o/r/":            "https://h.example/o/r.git/info/lfs",
		"http://h.example:3000/sub/o/r.git": "http://h.example:3000/sub/o/r.git/info/lfs",
		"file:///C:/x/src.git":              "file:///C:/x/src.git",
		"file:///srv/x/src.git":             "file:///srv/x/src.git",
	} {
		if got := lfsEndpoint(in); got != want {
			t.Errorf("lfsEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

// tagFixture mirrors main plus n tags and returns the tag names.
func tagFixture(t *testing.T, f *fixture, n int) []string {
	t.Helper()
	f.commit("a.txt")
	var tags []string
	for i := range n {
		name := fmt.Sprintf("t%02d", i)
		f.git("tag", name)
		tags = append(tags, name)
	}
	f.push(append([]string{"main"}, tags...)...)
	f.sync(SyncOptions{Prune: true})
	return tags
}

func TestSyncCheckDeletionsAbortsBeforeAnyWrite(t *testing.T) {
	f := newFixture(t)
	tags := tagFixture(t, f, 10)
	f.git(append([]string{"push", "-q", "origin", "--delete"}, tags[:8]...)...)
	before := refsOf(t, f.env, f.dst)
	if len(before) != 11 {
		t.Fatalf("fixture: dst refs = %v", before)
	}

	boom := errors.New("too many deletions")
	var gotDel []string
	gotTotal := -1
	check := func(deleted []string, dstTotal int) error {
		gotDel, gotTotal = deleted, dstTotal
		return boom
	}
	for _, dry := range []bool{true, false} {
		gotDel, gotTotal = nil, -1
		f.resetCmds()
		res, err := f.c.Sync(context.Background(), f.srcRemote, f.dstRemote, SyncOptions{Prune: true, DryRun: dry, CheckDeletions: check})
		if !errors.Is(err, boom) {
			t.Fatalf("dry=%v: err = %v, want the hook's error", dry, err)
		}
		if len(gotDel) != 8 || gotTotal != 11 {
			t.Fatalf("dry=%v: hook got %d deletions of %d refs, want 8 of 11", dry, len(gotDel), gotTotal)
		}
		if res == nil || len(res.Deleted) != 8 {
			t.Fatalf("dry=%v: result = %+v, want the planned 8 deletions", dry, res)
		}
		if ranSub(f.captured(), "push") {
			t.Fatalf("dry=%v: pushed although the deletion check failed", dry)
		}
		equalMaps(t, "dst refs after refused sync", refsOf(t, f.env, f.dst), before)
	}

	// The hook approving lets the prune through.
	check = func([]string, int) error { return nil }
	res, err := f.c.Sync(context.Background(), f.srcRemote, f.dstRemote, SyncOptions{Prune: true, CheckDeletions: check})
	if err != nil || len(res.Deleted) != 8 {
		t.Fatalf("approved prune: res=%+v err=%v", res, err)
	}
	equalMaps(t, "dst refs", refsOf(t, f.env, f.dst), refsOf(t, f.env, f.src))
}

func TestSyncCheckDeletionsNotCalledWithoutDeletions(t *testing.T) {
	f := newFixture(t)
	tags := tagFixture(t, f, 3)
	called := false
	check := func([]string, int) error { called = true; return errors.New("no") }
	f.commit("b.txt")
	f.push("main")
	if _, err := f.c.Sync(context.Background(), f.srcRemote, f.dstRemote, SyncOptions{Prune: true, CheckDeletions: check}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	// Without Prune deletions are never planned.
	f.git(append([]string{"push", "-q", "origin", "--delete"}, tags...)...)
	if _, err := f.c.Sync(context.Background(), f.srcRemote, f.dstRemote, SyncOptions{CheckDeletions: check}); err != nil {
		t.Fatalf("Sync without prune: %v", err)
	}
	if called {
		t.Fatal("CheckDeletions called although nothing would be deleted")
	}
}

// TestSyncLFSHealsWhenRefsUnchanged: refs were mirrored without LFS; a later
// LFS-enabled sync with an LFSDigest that does not match the source copies the
// objects although no ref changed, and a matching LFSDigest skips LFS.
func TestSyncLFSHealsWhenRefsUnchanged(t *testing.T) {
	requireLFS(t)
	f := newFixture(t)
	lfsCommit(t, f, "main")
	f.push("main")
	srcOIDs := lfsObjects(t, f.src)

	f.sync(SyncOptions{Prune: true})
	if got := lfsObjects(t, f.dst); len(got) != 0 {
		t.Fatalf("fixture: dst has LFS objects %v after a git-only sync", got)
	}

	res := f.sync(SyncOptions{Prune: true, LFS: true})
	if res.Changed {
		t.Fatal("refs reported changed")
	}
	if res.LFSObjects != 1 {
		t.Fatalf("LFSObjects = %d, want 1 (LFS must run to heal)", res.LFSObjects)
	}
	equalStrings(t, "dst LFS objects", lfsObjects(t, f.dst), srcOIDs)

	f.resetCmds()
	res = f.sync(SyncOptions{Prune: true, LFS: true, LFSDigest: Digest(res.SrcRefs)})
	if res.LFSObjects != -1 || ranSub(f.captured(), "lfs") && hasLFSTransfer(f) {
		t.Fatalf("LFS ran although LFSDigest matches: LFSObjects=%d", res.LFSObjects)
	}
}

func hasLFSTransfer(f *fixture) bool {
	for _, c := range f.captured() {
		j := strings.Join(c.Args, " ")
		if strings.Contains(j, " lfs fetch ") || strings.Contains(j, " lfs push ") {
			return true
		}
	}
	return false
}

func TestCacheDirPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	f := newFixture(t)
	f.commit("a.txt")
	f.push("main")
	f.sync(SyncOptions{})
	fi, err := os.Stat(filepath.Dir(f.cache))
	if err != nil {
		t.Fatal(err)
	}
	if p := fi.Mode().Perm(); p&0o077 != 0 {
		t.Fatalf("cache parent dir mode = %v, want no group/other access", p)
	}
}
