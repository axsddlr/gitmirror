package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"gitmirror/internal/forge"
)

var _ forge.HookProvider = (*Provider)(nil)

const hookSecret = "s3cr3t-hook-SENTINEL"

func hookJSON(id int64, url string) map[string]any {
	return map[string]any{
		"id": id, "name": "web", "active": true,
		"events": []string{"push", "create"},
		"config": map[string]any{"url": url, "content_type": "json", "insecure_ssl": "0", "secret": "********"},
	}
}

func TestHookEvents(t *testing.T) {
	p := newFakeAPI(t).provider()
	want := []string{"push", "create", "delete", "release", "repository", "gollum"}
	if got := p.HookEvents(); !reflect.DeepEqual(got, want) {
		t.Fatalf("HookEvents = %v, want %v", got, want)
	}
}

func TestListRepoHooksPaginates(t *testing.T) {
	f := newFakeAPI(t)
	f.handle("GET /repos/o/r/hooks", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "", "1":
			w.Header().Set("Link", fmt.Sprintf(`<%s/api/v3/repos/o/r/hooks?page=2>; rel="next"`, f.srv.URL))
			writeJSON(w, 200, []any{hookJSON(1, "https://a.example/hook")})
		case "2":
			writeJSON(w, 200, []any{hookJSON(2, "https://b.example/hook")})
		default:
			t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
		}
	})
	hooks, err := f.provider().RepoHooks(context.Background(), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	if len(hooks) != 2 || hooks[0].ID != 1 || hooks[1].URL != "https://b.example/hook" {
		t.Fatalf("hooks = %+v", hooks)
	}
	h := hooks[0]
	if !h.Active || h.ContentType != "json" || !reflect.DeepEqual(h.Events, []string{"push", "create"}) {
		t.Fatalf("hook mapped wrongly: %+v", h)
	}
	for _, a := range f.auth {
		if a != "Bearer "+sentinel {
			t.Errorf("Authorization = %q", a)
		}
	}
}

func TestListOrgHooks(t *testing.T) {
	f := newFakeAPI(t)
	f.handle("GET /orgs/acme/hooks", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, []any{hookJSON(7, "https://a.example/hook")})
	})
	hooks, err := f.provider().OrgHooks(context.Background(), "acme")
	if err != nil || len(hooks) != 1 || hooks[0].ID != 7 {
		t.Fatalf("hooks = %+v, err = %v", hooks, err)
	}
}

func checkCreateBody(t *testing.T, r *http.Request) {
	t.Helper()
	b := decodeBody(t, r)
	if b["name"] != "web" || b["active"] != true {
		t.Errorf("body = %v", b)
	}
	if !reflect.DeepEqual(b["events"], []any{"push", "release"}) {
		t.Errorf("events = %v", b["events"])
	}
	cfg, _ := b["config"].(map[string]any)
	if cfg["url"] != "https://gm.example/hook" || cfg["content_type"] != "json" ||
		cfg["secret"] != hookSecret || cfg["insecure_ssl"] != "0" {
		t.Errorf("config = %v", cfg)
	}
}

var hookOpts = forge.HookOptions{URL: "https://gm.example/hook", Secret: hookSecret, Events: []string{"push", "release"}}

func TestCreateRepoHook(t *testing.T) {
	f := newFakeAPI(t)
	f.handle("POST /repos/o/r/hooks", func(w http.ResponseWriter, r *http.Request) {
		checkCreateBody(t, r)
		writeJSON(w, 201, hookJSON(11, "https://gm.example/hook"))
	})
	h, err := f.provider().CreateRepoHook(context.Background(), "o", "r", hookOpts)
	if err != nil {
		t.Fatal(err)
	}
	if h.ID != 11 || h.URL != "https://gm.example/hook" || !h.Active {
		t.Fatalf("hook = %+v", h)
	}
}

func TestCreateOrgHook(t *testing.T) {
	f := newFakeAPI(t)
	f.handle("POST /orgs/acme/hooks", func(w http.ResponseWriter, r *http.Request) {
		checkCreateBody(t, r)
		writeJSON(w, 201, hookJSON(12, "https://gm.example/hook"))
	})
	h, err := f.provider().CreateOrgHook(context.Background(), "acme", hookOpts)
	if err != nil || h.ID != 12 {
		t.Fatalf("hook = %+v, err = %v", h, err)
	}
}

func TestHookErrors(t *testing.T) {
	cases := []struct {
		status int
		want   string
		notFnd bool
	}{
		{404, "not found", true},
		{403, "admin:", false},
		{422, "422", false},
	}
	for _, c := range cases {
		f := newFakeAPI(t)
		f.handle("POST /repos/o/r/hooks", func(w http.ResponseWriter, r *http.Request) {
			// A hostile or sloppy server echoing the secret must not leak it.
			writeJSON(w, c.status, map[string]string{"message": "bad " + hookSecret + " " + sentinel})
		})
		f.handle("POST /orgs/o/hooks", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, c.status, map[string]string{"message": "bad " + hookSecret})
		})
		f.handle("GET /repos/o/r/hooks", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, c.status, map[string]string{"message": "x"})
		})
		p := f.provider()
		_, err1 := p.CreateRepoHook(context.Background(), "o", "r", hookOpts)
		_, err2 := p.CreateOrgHook(context.Background(), "o", hookOpts)
		_, err3 := p.RepoHooks(context.Background(), "o", "r")
		for i, err := range []error{err1, err2, err3} {
			if err == nil {
				t.Fatalf("%d: status %d: expected error", i, c.status)
			}
			if errors.Is(err, forge.ErrNotFound) != c.notFnd {
				t.Errorf("%d: status %d: ErrNotFound = %v", i, c.status, !c.notFnd)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("%d: status %d: error %q lacks %q", i, c.status, err, c.want)
			}
			if (i < 2 && strings.Contains(err.Error(), hookSecret)) || strings.Contains(err.Error(), sentinel) {
				t.Errorf("%d: error leaks a secret: %v", i, err)
			}
		}
	}
}

func TestOrgHook403MentionsOrgScope(t *testing.T) {
	f := newFakeAPI(t)
	f.handle("POST /orgs/o/hooks", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 403, map[string]string{"message": "Must have admin rights"})
	})
	_, err := f.provider().CreateOrgHook(context.Background(), "o", hookOpts)
	if err == nil || !strings.Contains(err.Error(), "admin:org_hook") {
		t.Fatalf("err = %v", err)
	}
}
