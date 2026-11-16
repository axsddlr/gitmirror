package web

import (
	"net/http"
	"net/url"
	"testing"
)

var nextCases = []struct{ next, want string }{
	{"//evil.example", "/"},
	{`/\evil.example`, "/"},
	{`/./\evil.example`, "/"},
	{"/%2F%2Fevil.example", "/evil.example"},
	{"/%5Cevil.example", "/"},
	{"/\t/evil.example", "/"},
	{"/\n/evil.example", "/"},
	{"https://evil.example", "/"},
	{"javascript:alert(1)", "/"},
	{"evil.example", "/"},
	{"", "/"},
	{"/../..//evil.example", "/evil.example"},
	{"/mirrors?x=1", "/mirrors?x=1"},
	{"/pair?k=a%2Fb", "/pair?k=a%2Fb"},
	{"/sites", "/sites"},
}

func TestSafeNext(t *testing.T) {
	for _, tc := range nextCases {
		if got := safeNext(tc.next); got != tc.want {
			t.Errorf("safeNext(%q) = %q, want %q", tc.next, got, tc.want)
		}
	}
}

func TestLoginNextLocation(t *testing.T) {
	h := newHarness(t)
	h.admin()
	signedIn := h.client()
	if r := signedIn.login(testPassword); r.code != http.StatusSeeOther {
		t.Fatalf("login = %d", r.code)
	}
	for _, tc := range nextCases {
		q := "/login?next=" + url.QueryEscape(tc.next)
		c := h.client()
		tok := c.csrf("/login")
		if r := c.post(q, url.Values{"csrf": {tok}, "password": {testPassword}}); r.code != http.StatusSeeOther || r.location() != tc.want {
			t.Errorf("POST %s -> %d %q, want %q", q, r.code, r.location(), tc.want)
		}
		if r := signedIn.get(q); r.code != http.StatusSeeOther || r.location() != tc.want {
			t.Errorf("GET %s -> %d %q, want %q", q, r.code, r.location(), tc.want)
		}
	}
}
