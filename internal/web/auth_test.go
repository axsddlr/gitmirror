package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestRedirectsToSetupThenLogin(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	for _, p := range []string{"/", "/sites", "/settings", "/login", "/nope"} {
		if r := c.get(p); r.code != http.StatusSeeOther || r.location() != "/setup" {
			t.Fatalf("before setup GET %s = %d %q, want 303 /setup", p, r.code, r.location())
		}
	}
	if r := c.get("/setup"); r.code != http.StatusOK || !strings.Contains(r.body, "setup code") {
		t.Fatalf("GET /setup = %d\n%.1000s", r.code, r.body)
	}
	if r := c.get("/static/app.css"); r.code != http.StatusOK {
		t.Fatalf("static before setup = %d", r.code)
	}
	if !strings.Contains(h.logs.String(), "level=WARN") ||
		!strings.Contains(h.logs.String(), "web UI setup: open http://gitmirror.test/setup and enter setup code ") {
		t.Fatalf("setup code not logged at WARN:\n%s", h.logs.String())
	}
	h.admin()

	anon := h.client()
	if r := anon.get("/"); r.code != http.StatusSeeOther || r.location() != "/login" {
		t.Fatalf("after setup GET / = %d %q, want 303 /login", r.code, r.location())
	}
	if r := anon.get("/sites"); r.code != http.StatusSeeOther || r.location() != "/login?next=%2Fsites" {
		t.Fatalf("after setup GET /sites = %d %q", r.code, r.location())
	}
	if r := anon.get("/setup"); r.code != http.StatusSeeOther || r.location() != "/login" {
		t.Fatalf("GET /setup after setup = %d %q", r.code, r.location())
	}
}

func TestSetup(t *testing.T) {
	h := newHarness(t)
	code := h.setupCode()
	c := h.client()

	if r := c.setup("AAAA-BBBB-CCCC", testPassword, testPassword); r.code != http.StatusUnauthorized ||
		!strings.Contains(r.body, "setup code is not right") {
		t.Fatalf("wrong code = %d\n%.2000s", r.code, r.body)
	}
	if r := c.setup(code, "short", "short"); r.code != http.StatusBadRequest || !strings.Contains(r.body, "at least 12") {
		t.Fatalf("short password = %d\n%.2000s", r.code, r.body)
	}
	long := strings.Repeat("x", 257)
	if r := c.setup(code, long, long); r.code != http.StatusBadRequest {
		t.Fatalf("long password = %d", r.code)
	}
	if r := c.setup(code, testPassword, testPassword+"!"); r.code != http.StatusBadRequest || !strings.Contains(r.body, "do not match") {
		t.Fatalf("mismatch = %d\n%.2000s", r.code, r.body)
	}
	if _, ok, _ := h.app.Store.Setting(context.Background(), adminPasswordKey); ok {
		t.Fatal("admin created by a failed setup")
	}
	// Lower case and spaces are accepted.
	if r := c.setup(strings.ToLower(strings.ReplaceAll(code, "-", " ")), testPassword, testPassword); r.code != http.StatusSeeOther || r.location() != "/" {
		t.Fatalf("setup = %d %q\n%.2000s", r.code, r.location(), r.body)
	}
	if !c.signedIn() {
		t.Fatal("not signed in after setup")
	}
	v, ok, err := h.app.Store.Setting(context.Background(), adminPasswordKey)
	if err != nil || !ok || strings.Contains(string(v), testPassword) {
		t.Fatalf("stored admin password %v %v", ok, err)
	}

	// A second setup is refused, even with the old code.
	other := h.client()
	if r := other.get("/setup"); r.code != http.StatusSeeOther || r.location() != "/login" {
		t.Fatalf("GET /setup again = %d %q", r.code, r.location())
	}
	tok := other.csrf("/login")
	r := other.post("/setup", url.Values{"csrf": {tok}, "code": {code}, "password": {"another password!"}, "confirm": {"another password!"}})
	if r.code != http.StatusSeeOther || r.location() != "/login" {
		t.Fatalf("POST /setup again = %d %q", r.code, r.location())
	}
	if v2, _, _ := h.app.Store.Setting(context.Background(), adminPasswordKey); string(v2) != string(v) {
		t.Fatal("second setup replaced the password")
	}
	if strings.Count(h.logs.String(), code) != 1 {
		t.Fatalf("setup code logged more than once:\n%s", h.logs.String())
	}
}

func TestSetupRateLimited(t *testing.T) {
	h := newHarness(t)
	code := h.setupCode()
	c := h.client()
	for i := range maxFailuresPerIP {
		if r := c.setup("AAAA-BBBB-CCCC", testPassword, testPassword); r.code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d", i, r.code)
		}
	}
	if r := c.setup(code, testPassword, testPassword); r.code != http.StatusTooManyRequests {
		t.Fatalf("after %d failures = %d", maxFailuresPerIP, r.code)
	}
	h.clock.Advance(failureWindow + time.Second)
	if r := c.setup(code, testPassword, testPassword); r.code != http.StatusSeeOther {
		t.Fatalf("after the window = %d\n%.2000s", r.code, r.body)
	}
}

func TestLogin(t *testing.T) {
	h := newHarness(t)
	h.admin()
	c := h.client()
	if r := c.login("wrong password!!"); r.code != http.StatusUnauthorized || !strings.Contains(r.body, "password is not right") {
		t.Fatalf("bad login = %d\n%.2000s", r.code, r.body)
	}
	if c.signedIn() {
		t.Fatal("signed in after a bad password")
	}
	if r := c.login(testPassword); r.code != http.StatusSeeOther || r.location() != "/" {
		t.Fatalf("good login = %d %q", r.code, r.location())
	}
	if !c.signedIn() {
		t.Fatal("not signed in after login")
	}
	if r := c.get("/login"); r.code != http.StatusSeeOther || r.location() != "/" {
		t.Fatalf("GET /login signed in = %d %q", r.code, r.location())
	}
}

func TestLoginNext(t *testing.T) {
	h := newHarness(t)
	h.admin()
	for next, want := range map[string]string{
		"/sites":            "/sites",
		"//evil.example":    "/",
		"https://evil.test": "/",
		"/\\evil.example":   "/",
	} {
		c := h.client()
		tok := c.csrf("/login")
		r := c.post("/login?next="+url.QueryEscape(next), url.Values{"csrf": {tok}, "password": {testPassword}})
		if r.code != http.StatusSeeOther || r.location() != want {
			t.Errorf("next=%q -> %d %q, want %q", next, r.code, r.location(), want)
		}
	}
}

func TestLoginRateLimit(t *testing.T) {
	h := newHarness(t)
	h.admin()
	c := h.client()
	for i := range maxFailuresPerIP {
		tok := c.csrf("/login")
		r := c.post("/login", url.Values{"csrf": {tok}, "password": {"wrong password!!"}}, "X-Forwarded-For", "10.0.0."+string(rune('1'+i)))
		if r.code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d", i, r.code)
		}
	}
	// The right password is refused too, and X-Forwarded-For does not help.
	for _, xff := range []string{"", "10.9.9.9", "203.0.113.7, 10.0.0.1"} {
		tok := c.csrf("/login")
		r := c.post("/login", url.Values{"csrf": {tok}, "password": {testPassword}}, "X-Forwarded-For", xff, "X-Real-IP", xff)
		if r.code != http.StatusTooManyRequests || !strings.Contains(r.body, "Too many") || r.header.Get("Retry-After") == "" {
			t.Fatalf("xff %q = %d\n%.2000s", xff, r.code, r.body)
		}
	}
	h.clock.Advance(failureWindow + time.Second)
	if r := c.login(testPassword); r.code != http.StatusSeeOther {
		t.Fatalf("after the window = %d", r.code)
	}
}

func TestCSRF(t *testing.T) {
	h := newHarness(t)
	c := h.admin()
	tok := c.csrf("/settings")
	form := func(tok string) url.Values {
		return url.Values{"csrf": {tok}, "current": {testPassword}, "password": {"new password 123"}, "confirm": {"new password 123"}}
	}
	if r := c.post("/settings/password", url.Values{"current": {testPassword}}); r.code != http.StatusForbidden {
		t.Fatalf("no token = %d", r.code)
	}
	if r := c.post("/settings/password", form("wrong-token")); r.code != http.StatusForbidden {
		t.Fatalf("wrong token = %d", r.code)
	}
	if r := c.post("/logout", url.Values{"csrf": {tok + "x"}}); r.code != http.StatusForbidden {
		t.Fatalf("logout wrong token = %d", r.code)
	}
	if r := c.post("/logout", url.Values{"csrf": {tok}}, "Origin", "https://evil.example"); r.code != http.StatusForbidden {
		t.Fatalf("cross-origin = %d", r.code)
	}
	if r := c.post("/logout", url.Values{"csrf": {tok}}, "Origin", "null"); r.code != http.StatusForbidden {
		t.Fatalf("null origin = %d", r.code)
	}
	if !c.signedIn() {
		t.Fatal("signed out by a rejected request")
	}
	// The header works too, and a same-origin Origin is accepted.
	r := c.post("/logout", nil, "X-CSRF-Token", tok, "Origin", h.srv.URL)
	if r.code != http.StatusSeeOther || r.location() != "/login" {
		t.Fatalf("logout with header = %d %q", r.code, r.location())
	}
	if c.signedIn() {
		t.Fatal("still signed in after logout")
	}

	// Login form: the pre-session token is required.
	anon := h.client()
	if r := anon.post("/login", url.Values{"password": {testPassword}}); r.code != http.StatusForbidden {
		t.Fatalf("login without token = %d", r.code)
	}
	anon.csrf("/login") // sets the cookie
	if r := anon.post("/login", url.Values{"csrf": {"forged"}, "password": {testPassword}}); r.code != http.StatusForbidden {
		t.Fatalf("login with forged token = %d", r.code)
	}
	if anon.signedIn() {
		t.Fatal("signed in without a valid token")
	}
}

func TestSetupCSRF(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	r := c.post("/setup", url.Values{"code": {h.setupCode()}, "password": {testPassword}, "confirm": {testPassword}})
	if r.code != http.StatusForbidden {
		t.Fatalf("setup without token = %d", r.code)
	}
}

func TestSessionCookieFlags(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	r := c.setup(h.setupCode(), testPassword, testPassword)
	k := findCookie(r.header, sessionCookie)
	if k == nil {
		t.Fatalf("no session cookie: %v", r.header["Set-Cookie"])
	}
	if !k.HttpOnly || k.SameSite != http.SameSiteStrictMode || k.Path != "/" || k.Secure {
		t.Fatalf("cookie flags %+v", k)
	}
	if len(k.Value) < 43 {
		t.Fatalf("session token too short: %d", len(k.Value))
	}
	pk := findCookie(h.client().get("/login").header, csrfCookie)
	if pk == nil || !pk.HttpOnly || pk.SameSite != http.SameSiteStrictMode {
		t.Fatalf("csrf cookie %+v", pk)
	}
	if strings.Contains(h.logs.String(), k.Value) {
		t.Fatal("session token logged")
	}
}

func TestSecureCookiesWithHTTPSExternalURL(t *testing.T) {
	h := newHarnessYAML(t, "server:\n  external_url: 'https://mirror.example.com'\n")
	c := h.client()
	tok := c.csrf("/setup")
	// The jar drops Secure cookies on http, so send the token cookie by hand.
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/setup", strings.NewReader(url.Values{
		"csrf": {tok}, "code": {h.setupCode()}, "password": {testPassword}, "confirm": {testPassword}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: csrfCookie, Value: tok})
	r := c.do(req)
	if r.code != http.StatusSeeOther {
		t.Fatalf("setup = %d\n%.2000s", r.code, r.body)
	}
	k := findCookie(r.header, sessionCookie)
	if k == nil || !k.Secure || !k.HttpOnly || k.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie %+v", k)
	}
	if !strings.Contains(h.logs.String(), "open https://mirror.example.com/setup") {
		t.Fatalf("setup URL should use external_url:\n%s", h.logs.String())
	}
}

func findCookie(hdr http.Header, name string) *http.Cookie {
	for _, k := range (&http.Response{Header: hdr}).Cookies() {
		if k.Name == name {
			return k
		}
	}
	return nil
}

func TestSessionIdleTimeout(t *testing.T) {
	h := newHarness(t)
	c := h.admin()
	h.clock.Advance(23 * time.Hour)
	if !c.signedIn() {
		t.Fatal("signed out after 23h")
	}
	h.clock.Advance(23 * time.Hour) // 46h since sign-in, 23h since last seen
	if !c.signedIn() {
		t.Fatal("activity did not extend the session")
	}
	h.clock.Advance(sessionIdle + time.Minute)
	if c.signedIn() {
		t.Fatal("session survived the idle timeout")
	}
}

func TestSessionAbsoluteTimeout(t *testing.T) {
	h := newHarness(t)
	c := h.admin()
	if sessionMaxAge%(12*time.Hour) != 0 {
		t.Fatal("test assumes sessionMaxAge is a multiple of 12h")
	}
	for elapsed := 12 * time.Hour; elapsed < sessionMaxAge; elapsed += 12 * time.Hour {
		h.clock.Advance(12 * time.Hour)
		if !c.signedIn() {
			t.Fatalf("signed out early after %v", elapsed)
		}
	}
	h.clock.Advance(12*time.Hour + time.Minute) // just past sessionMaxAge
	if c.signedIn() {
		t.Fatal("session survived the absolute timeout")
	}
}

func TestLogoutInvalidatesSession(t *testing.T) {
	h := newHarness(t)
	c := h.admin()
	k := c.cookie(sessionCookie)
	tok := c.csrf("/")
	if r := c.post("/logout", url.Values{"csrf": {tok}}); r.code != http.StatusSeeOther {
		t.Fatalf("logout = %d", r.code)
	}
	// Replaying the old cookie does not work.
	other := h.client()
	u, _ := url.Parse(h.srv.URL)
	other.jar.SetCookies(u, []*http.Cookie{{Name: sessionCookie, Value: k.Value, Path: "/"}})
	if other.signedIn() {
		t.Fatal("old session cookie still valid after logout")
	}
	if r := c.get("/login"); !strings.Contains(r.body, "signed out") {
		t.Fatalf("no signed-out flash:\n%.2000s", r.body)
	}
}

func TestChangePassword(t *testing.T) {
	h := newHarness(t)
	c := h.admin()
	other := h.client()
	if r := other.login(testPassword); r.code != http.StatusSeeOther {
		t.Fatalf("second login = %d", r.code)
	}
	newPW := "a brand new password"
	tok := c.csrf("/settings")
	if r := c.post("/settings/password", url.Values{"csrf": {tok}, "current": {"not the password"}, "password": {newPW}, "confirm": {newPW}}); r.code != http.StatusUnauthorized {
		t.Fatalf("wrong current = %d", r.code)
	}
	if r := c.post("/settings/password", url.Values{"csrf": {tok}, "current": {testPassword}, "password": {"short"}, "confirm": {"short"}}); r.code != http.StatusBadRequest {
		t.Fatalf("short new = %d", r.code)
	}
	if r := c.post("/settings/password", url.Values{"csrf": {tok}, "current": {testPassword}, "password": {newPW}, "confirm": {newPW + "x"}}); r.code != http.StatusBadRequest {
		t.Fatalf("mismatch = %d", r.code)
	}
	r := c.post("/settings/password", url.Values{"csrf": {tok}, "current": {testPassword}, "password": {newPW}, "confirm": {newPW}})
	if r.code != http.StatusSeeOther || r.location() != "/settings" {
		t.Fatalf("change = %d %q\n%.2000s", r.code, r.location(), r.body)
	}
	if r := c.get("/settings"); r.code != http.StatusOK || !strings.Contains(r.body, "Password changed") {
		t.Fatalf("current session signed out or no flash: %d\n%.2000s", r.code, r.body)
	}
	if other.signedIn() {
		t.Fatal("other session survived password change")
	}
	if r := h.client().login(testPassword); r.code != http.StatusUnauthorized {
		t.Fatalf("old password still works: %d", r.code)
	}
	if r := h.client().login(newPW); r.code != http.StatusSeeOther {
		t.Fatalf("new password = %d", r.code)
	}
}

func TestSessionsSurviveRestart(t *testing.T) {
	h := newHarness(t)
	c := h.admin()
	k := c.cookie(sessionCookie)
	h.restart()
	if strings.Contains(h.logs.String(), "setup code") {
		t.Fatal("setup code generated although an admin exists")
	}
	c2 := h.client()
	u, _ := url.Parse(h.srv.URL)
	c2.jar.SetCookies(u, []*http.Cookie{{Name: sessionCookie, Value: k.Value, Path: "/"}})
	if !c2.signedIn() {
		t.Fatal("session lost across a new UI instance")
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	check := func(r resp, html bool) {
		t.Helper()
		csp := r.header.Get("Content-Security-Policy")
		if csp != contentSecurityPolicy || strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
			t.Fatalf("CSP = %q", csp)
		}
		for k, v := range map[string]string{
			"X-Content-Type-Options": "nosniff",
			"Referrer-Policy":        "same-origin",
			"X-Frame-Options":        "DENY",
		} {
			if got := r.header.Get(k); got != v {
				t.Fatalf("%s = %q, want %q", k, got, v)
			}
		}
		if html && r.header.Get("Cache-Control") != "no-store" {
			t.Fatalf("Cache-Control = %q", r.header.Get("Cache-Control"))
		}
	}
	setup := c.get("/setup")
	check(setup, true)
	check(c.get("/"), false) // redirect
	check(c.get("/static/app.css"), false)
	if strings.Contains(setup.body, "<script>") || strings.Contains(setup.body, "style=") || strings.Contains(setup.body, "<style") {
		t.Fatal("inline script or style in page")
	}
	a := h.admin()
	for _, p := range []string{"/", "/sites", "/mirrors", "/webhooks", "/settings", "/does-not-exist"} {
		r := a.get(p)
		check(r, true)
		if strings.Contains(r.body, "<script>") || strings.Contains(r.body, "style=") || strings.Contains(r.body, "<style") {
			t.Fatalf("%s: inline script or style", p)
		}
	}
}

func TestPages(t *testing.T) {
	h := newHarness(t)
	c := h.admin()
	r := c.get("/")
	for _, want := range []string{"gitmirror", "Dashboard", "Sites", "Mirrors", "Webhooks", "Settings", "Sign out",
		"No mirrors yet", `href="/sites"`, `aria-current="page"`} {
		if !strings.Contains(r.body, want) {
			t.Errorf("dashboard lacks %q", want)
		}
	}
	for _, p := range []string{"/sites", "/mirrors", "/webhooks"} {
		if r := c.get(p); r.code != http.StatusOK || !strings.Contains(r.body, "coming soon") {
			t.Errorf("%s = %d", p, r.code)
		}
	}
	if r := c.get("/settings"); r.code != http.StatusOK || !strings.Contains(r.body, `for="current"`) {
		t.Errorf("/settings = %d", r.code)
	}
	if r := c.get("/does-not-exist"); r.code != http.StatusNotFound || !strings.Contains(r.body, "not found") {
		t.Errorf("404 page = %d", r.code)
	}
	if r := c.get("/login"); r.code != http.StatusSeeOther {
		t.Errorf("/login signed in = %d", r.code)
	}
}
