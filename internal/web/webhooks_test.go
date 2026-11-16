package web

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"gitmirror/internal/app"
	"gitmirror/internal/config"
	"gitmirror/internal/forge"
	"gitmirror/internal/forge/fake"
	"gitmirror/internal/server"
)

func TestWebhookSecretGeneratedSealed(t *testing.T) {
	w := newWorld(t, "")
	w.addSites()
	c := w.admin()
	page := c.get("/webhooks")
	mustContain(t, page, "within seconds", "<details", "Show the secret", "Create a new secret")
	sec, fromFile := w.app.WebhookSecret()
	if sec == "" || fromFile {
		t.Fatal("no secret generated on first visit")
	}
	sts, err := w.app.Store.Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sts {
		if bytes.Contains(s.Value, []byte(sec)) {
			t.Fatalf("secret stored in plain text under %s", s.Key)
		}
	}
	if strings.Count(page.body, sec) != 1 {
		t.Fatalf("secret shown %d times, want once (inside the reveal)", strings.Count(page.body, sec))
	}
	if strings.Contains(w.logs.String(), sec) {
		t.Fatal("secret logged")
	}
	if again := c.get("/webhooks"); !strings.Contains(again.body, sec) {
		t.Fatal("second visit made a new secret")
	}
	confirm := c.get("/webhooks/secret/new")
	mustContain(t, confirm, "btn-danger", "must be updated")
	if strings.Contains(confirm.body, sec) {
		t.Fatal("confirmation page shows the secret")
	}
	if r := c.postForm("/webhooks/secret/new", "/webhooks/secret/new", nil); r.code != http.StatusSeeOther {
		t.Fatalf("new secret = %d", r.code)
	}
	if s2, _ := w.app.WebhookSecret(); s2 == sec || s2 == "" {
		t.Fatal("secret not replaced")
	}
}

func TestWebhookSecretFromFileShownAsSet(t *testing.T) {
	w := newWorld(t, "server: {webhook_secret: 'SENTINEL-hook-secret-1', external_url: 'https://m.example.com'}\n")
	c := w.admin()
	page := c.get("/webhooks")
	mustContain(t, page, "set in the config file", "https://m.example.com")
	if strings.Contains(page.body, "SENTINEL-hook-secret-1") || strings.Contains(page.body, `name="address"`) {
		t.Fatalf("file secret shown or address editable:\n%.3000s", page.body)
	}
	if r := c.postForm("/webhooks", "/webhooks/address", url.Values{"address": {"https://x.example.com"}}); r.code != http.StatusForbidden {
		t.Fatalf("crafted address = %d", r.code)
	}
	if r := c.postForm("/webhooks", "/webhooks/secret/new", nil); r.code != http.StatusForbidden {
		t.Fatalf("crafted secret = %d", r.code)
	}
}

func TestWebhookReceiverUsesUISecret(t *testing.T) {
	w := newWorld(t, "")
	w.addSites()
	w.putRule(config.Rule{Name: "m1", From: config.Source{Forge: "github", Owner: "alice"}, To: config.Dest{Forge: "git-example-com", Owner: "carol"}})
	s := &server.Server{App: w.app, Logger: slog.New(slog.DiscardHandler)}
	body := []byte(`{"kind":"push","owner":"alice","name":"one"}`)
	send := func(sec string) int {
		req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
		req.Header.Set(fake.SignatureHeader, fake.Sign(sec, body))
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	if code := send("anything"); code != http.StatusNotFound {
		t.Fatalf("before a secret exists = %d", code)
	}
	c := w.admin()
	c.get("/webhooks") // creates the secret
	sec, _ := w.app.WebhookSecret()
	if code := send("wrong"); code != http.StatusUnauthorized {
		t.Fatalf("bad signature = %d", code)
	}
	if code := send(sec); code != http.StatusAccepted {
		t.Fatalf("signed delivery = %d", code)
	}
}

func TestPublicAddressSetting(t *testing.T) {
	w := newWorld(t, "")
	w.addSites()
	w.putRule(config.Rule{Name: "user", From: config.Source{Forge: "github", Owner: "alice"}, To: config.Dest{Forge: "git-example-com", Owner: "carol"}})
	c := w.admin()
	page := c.get("/webhooks")
	mustContain(t, page, `name="address"`, "Add a public address first")
	r := c.postForm("/webhooks", "/webhooks/address", url.Values{"address": {"mirror.example.com"}})
	if r.code != http.StatusBadRequest {
		t.Fatalf("bad address = %d", r.code)
	}
	mustContain(t, r, "Enter the full address")
	r = c.postForm("/webhooks", "/webhooks/address", url.Values{"address": {"https://mirror.example.com/"}})
	if r.code != http.StatusSeeOther {
		t.Fatalf("good address = %d\n%.2000s", r.code, r.body)
	}
	if u, _ := w.app.PublicAddress(); u != "https://mirror.example.com" {
		t.Fatalf("address = %q", u)
	}
	// The public address also decides the Secure cookie flag.
	if !w.ui.secureCookies(httptest.NewRequest(http.MethodGet, "/", nil)) {
		t.Fatal("https public address does not make cookies Secure")
	}
}

func TestSetUpWebhooksForMe(t *testing.T) {
	w := newWorld(t, "")
	w.addSites()
	w.gh.AddOwner("acme", true)
	w.addRepo(w.gh.Provider, "acme", "tool")
	w.addRepo(w.gh.Provider, "alice", "one")
	w.addRepo(w.gh.Provider, "alice", "two")
	w.addRepo(w.gh.Provider, "alice", "three")
	w.putRule(config.Rule{Name: "org", From: config.Source{Forge: "github", Owner: "acme"}, To: config.Dest{Forge: "git-example-com", Owner: "acme"}})
	w.putRule(config.Rule{Name: "user", From: config.Source{Forge: "github", Owner: "alice"}, To: config.Dest{Forge: "git-example-com", Owner: "carol"}})
	if err := w.app.SetPublicAddress(context.Background(), "https://mirror.example.com"); err != nil {
		t.Fatal(err)
	}
	c := w.admin()
	page := c.get("/webhooks")
	payload := "https://mirror.example.com/webhook/github"
	mustContain(t, page, payload, "push", "application/json", `action="/webhooks/github/setup"`, "reach")
	sec, _ := w.app.WebhookSecret()
	if _, err := w.gh.CreateRepoHook(context.Background(), "alice", "one", forge.HookOptions{URL: payload, Secret: sec, Events: []string{"push"}}); err != nil {
		t.Fatal(err)
	}
	w.gh.FailNext("CreateRepoHook", errors.New("github: create hook: 403 Must have admin rights to Repository; forbidden"))
	r := c.postForm("/webhooks", "/webhooks/github/setup", nil)
	if r.code != http.StatusOK {
		t.Fatalf("setup = %d\n%.3000s", r.code, r.body)
	}
	mustContain(t, r, "acme", "Added", "alice/one", "Already there", "Failed", "not allowed to manage webhooks")
	w.noSecrets(r.body)
	if strings.Contains(r.body, sec) {
		t.Fatal("result page shows the secret")
	}
	scopes := map[string]bool{}
	for _, h := range w.gh.CreatedHooks() {
		scopes[h.Scope] = true
		if h.Hook.URL != payload || h.Secret != sec {
			t.Fatalf("hook %+v secret ok %v", h.Hook, h.Secret == sec)
		}
	}
	// alice/one existed already; one of alice/two, alice/three failed.
	if !scopes["acme"] || scopes["acme/tool"] || len(scopes) != 3 {
		t.Fatalf("created scopes = %v", scopes)
	}
	// Running it again adds nothing new except the repo that failed.
	r = c.postForm("/webhooks", "/webhooks/github/setup", nil)
	mustContain(t, r, "Already there")
	if n := len(w.gh.CreatedHooks()); n != 4 {
		t.Fatalf("hooks after the second run = %d, want 4", n)
	}
}

func TestSetUpWebhooksNeedsAddress(t *testing.T) {
	w := newWorld(t, "")
	w.addSites()
	w.putRule(config.Rule{Name: "user", From: config.Source{Forge: "github", Owner: "alice"}, To: config.Dest{Forge: "git-example-com", Owner: "carol"}})
	c := w.admin()
	page := c.get("/webhooks")
	mustContain(t, page, "disabled", "Add a public address first")
	if r := c.postForm("/webhooks", "/webhooks/github/setup", nil); r.code != http.StatusBadRequest {
		t.Fatalf("setup without address = %d", r.code)
	}
	if n := len(w.gh.CreatedHooks()); n != 0 {
		t.Fatalf("hooks created: %d", n)
	}
	if r := c.postForm("/webhooks", "/webhooks/nosuch/setup", nil); r.code != http.StatusNotFound {
		t.Fatalf("unknown site = %d", r.code)
	}
	_ = app.SettingWebhookSecret
}

func TestSetUpWebhooksTimeBound(t *testing.T) {
	old := hookTimeout
	hookTimeout = 400 * time.Millisecond
	t.Cleanup(func() { hookTimeout = old })
	w := newWorld(t, "")
	w.addSites()
	for _, n := range []string{"one", "two", "three"} {
		w.addRepo(w.gh.Provider, "alice", n)
	}
	w.putRule(config.Rule{Name: "user", From: config.Source{Forge: "github", Owner: "alice"}, To: config.Dest{Forge: "git-example-com", Owner: "carol"}})
	if err := w.app.SetPublicAddress(context.Background(), "https://mirror.example.com"); err != nil {
		t.Fatal(err)
	}
	c := w.admin()
	w.gh.mu.Lock()
	w.gh.hookDelay = 250 * time.Millisecond
	w.gh.mu.Unlock()
	start := time.Now()
	r := c.postForm("/webhooks", "/webhooks/github/setup", nil)
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("setup took %v", took)
	}
	if r.code != http.StatusOK {
		t.Fatalf("setup = %d\n%.3000s", r.code, r.body)
	}
	mustContain(t, r, "alice/one", "Added", "alice/three", "Not done yet — run Set up again")
	if n := len(w.gh.CreatedHooks()); n != 1 {
		t.Fatalf("hooks created = %d, want 1", n)
	}
}

func TestSetUpWebhooksPickListUsesRepoHooks(t *testing.T) {
	w := newWorld(t, "")
	w.addSites()
	w.gh.AddOwner("acme", true)
	w.gh.AddOwner("bigco", true)
	for _, n := range []string{"a", "b", "c"} {
		w.addRepo(w.gh.Provider, "acme", n)
	}
	w.addRepo(w.gh.Provider, "bigco", "x")
	w.addRepo(w.gh.Provider, "bigco", "y")
	yes := true
	w.putRule(config.Rule{Name: "picked", From: config.Source{Forge: "github", Owner: "acme",
		Filter: &config.Filter{Include: []string{"a", "b"}, Forks: &yes, Archived: &yes, Private: &yes}},
		To: config.Dest{Forge: "git-example-com", Owner: "acme"}})
	w.putRule(config.Rule{Name: "single", From: config.Source{Forge: "github", Repo: "bigco/x"},
		To: config.Dest{Forge: "git-example-com", Repo: "carol/x"}})
	if err := w.app.SetPublicAddress(context.Background(), "https://mirror.example.com"); err != nil {
		t.Fatal(err)
	}
	c := w.admin()
	r := c.postForm("/webhooks", "/webhooks/github/setup", nil)
	if r.code != http.StatusOK {
		t.Fatalf("setup = %d\n%.3000s", r.code, r.body)
	}
	scopes := map[string]bool{}
	for _, h := range w.gh.CreatedHooks() {
		scopes[h.Scope] = true
	}
	if len(scopes) != 3 || !scopes["acme/a"] || !scopes["acme/b"] || !scopes["bigco/x"] {
		t.Fatalf("created scopes = %v, want only acme/a, acme/b and bigco/x", scopes)
	}
}
