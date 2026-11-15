package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// tokenServer is a Gitea-like server that records the tokens it is sent.
type tokenServer struct {
	*httptest.Server
	mu     sync.Mutex
	tokens []string
}

func newTokenServer(t *testing.T, login string) *tokenServer {
	t.Helper()
	s := &tokenServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.tokens = append(s.tokens, r.Header.Get("Authorization"))
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1,"login":"` + login + `"}`))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *tokenServer) saw(tok string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range s.tokens {
		if strings.Contains(h, tok) {
			return true
		}
	}
	return false
}

func TestEditSiteNewAddressNeedsToken(t *testing.T) {
	const sentinel = "SENTINEL-site-a-token"
	a := newTokenServer(t, "alice")
	b := newTokenServer(t, "mallory")
	h := newHarness(t)
	c := h.admin()
	r := c.postForm("/sites/new", "/sites/new", url.Values{"kind": {"gitea"}, "url": {a.URL}, "token": {sentinel}, "name": {"home"}})
	if r.code != http.StatusSeeOther {
		t.Fatalf("add = %d\n%.3000s", r.code, r.body)
	}
	if !a.saw(sentinel) {
		t.Fatal("server A was not sent the token")
	}

	for _, form := range []url.Values{
		{"kind": {"gitea"}, "url": {b.URL}, "token": {""}},
		{"kind": {"forgejo"}, "url": {b.URL}, "token": {"  "}},
		{"kind": {"github"}, "url": {a.URL}, "token": {""}},
	} {
		r = c.postForm("/sites/home/edit", "/sites/home/edit", form)
		if r.code != http.StatusBadRequest || !strings.Contains(r.body, "Paste the token again when you change the address.") {
			t.Fatalf("edit %v with blank token = %d\n%.3000s", form, r.code, r.body)
		}
		if strings.Contains(r.body, sentinel) {
			t.Fatal("token on the page")
		}
	}
	if b.saw(sentinel) {
		t.Fatal("the old token was sent to the new address")
	}
	if f := h.app.Config().Forges["home"]; f.URL != a.URL || f.Token != sentinel {
		t.Fatalf("site changed: %q", f.URL)
	}

	// Same address, blank token: the stored token is kept.
	r = c.postForm("/sites/home/edit", "/sites/home/edit", url.Values{"kind": {"gitea"}, "url": {a.URL + "/"}, "token": {""}})
	if r.code != http.StatusSeeOther {
		t.Fatalf("unchanged address = %d\n%.3000s", r.code, r.body)
	}

	// A newly pasted token for the new address works.
	r = c.postForm("/sites/home/edit", "/sites/home/edit", url.Values{"kind": {"gitea"}, "url": {b.URL}, "token": {"SENTINEL-site-b-token"}})
	if r.code != http.StatusSeeOther {
		t.Fatalf("edit with new token = %d\n%.3000s", r.code, r.body)
	}
	if b.saw(sentinel) || !b.saw("SENTINEL-site-b-token") {
		t.Fatal("server B did not get (only) the new token")
	}
	if f := h.app.Config().Forges["home"]; f.URL != b.URL || f.Token != "SENTINEL-site-b-token" || f.Username != "mallory" {
		t.Fatalf("site after edit: %q %q", f.URL, f.Username)
	}
}
