package gitea

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"gitmirror/internal/forge"
)

var _ forge.HookProvider = (*Provider)(nil)

const hookSecret = "s3cr3t-hook-SENTINEL"

var hookOpts = forge.HookOptions{URL: "https://gm.example/hook", Secret: hookSecret, Events: []string{"push", "wiki"}}

func hookObj(id int64, url string) map[string]any {
	return map[string]any{
		"id": id, "type": "gitea", "active": true,
		"events": []string{"push", "create"},
		"config": map[string]string{"url": url, "content_type": "json"},
	}
}

func TestHookEvents(t *testing.T) {
	p := newProv(t, "http://x")
	want := []string{"push", "create", "delete", "release", "repository", "wiki"}
	if got := p.HookEvents(); !reflect.DeepEqual(got, want) {
		t.Fatalf("HookEvents = %v, want %v", got, want)
	}
}

func TestListHooksPaginates(t *testing.T) {
	mux := http.NewServeMux()
	// 51 hooks force a second page at limit=50.
	mux.HandleFunc("GET /api/v1/repos/o/r/hooks", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Total-Count", "51")
		switch r.URL.Query().Get("page") {
		case "1":
			var l []any
			for i := 1; i <= 50; i++ {
				l = append(l, hookObj(int64(i), "https://h/"+strconv.Itoa(i)))
			}
			writeJSON(w, 200, l)
		case "2":
			writeJSON(w, 200, []any{hookObj(51, "https://h/51")})
		default:
			writeJSON(w, 200, []any{})
		}
	})
	mux.HandleFunc("GET /api/v1/orgs/acme/hooks", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "1" {
			writeJSON(w, 200, []any{})
			return
		}
		writeJSON(w, 200, []any{hookObj(9, "https://h/9")})
	})
	srv, _ := serve(t, mux)
	p := newProv(t, srv.URL)
	hooks, err := p.RepoHooks(context.Background(), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	if len(hooks) != 51 || hooks[50].ID != 51 || hooks[0].URL != "https://h/1" {
		t.Fatalf("len=%d", len(hooks))
	}
	h := hooks[0]
	if !h.Active || h.ContentType != "json" || !reflect.DeepEqual(h.Events, []string{"push", "create"}) {
		t.Fatalf("hook = %+v", h)
	}
	oh, err := p.OrgHooks(context.Background(), "acme")
	if err != nil || len(oh) != 1 || oh[0].ID != 9 {
		t.Fatalf("org hooks = %+v, %v", oh, err)
	}
}

func checkHookBody(t *testing.T, r *http.Request) {
	t.Helper()
	if r.Header.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q", r.Header.Get("Content-Type"))
	}
	b := readBody(t, r)
	if b["type"] != "gitea" || b["active"] != true || b["branch_filter"] != "*" {
		t.Errorf("body = %v", b)
	}
	if !reflect.DeepEqual(b["events"], []any{"push", "wiki"}) {
		t.Errorf("events = %v", b["events"])
	}
	cfg, _ := b["config"].(map[string]any)
	if cfg["url"] != "https://gm.example/hook" || cfg["content_type"] != "json" || cfg["secret"] != hookSecret {
		t.Errorf("config = %v", cfg)
	}
}

func TestCreateHooks(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/repos/o/r/hooks", func(w http.ResponseWriter, r *http.Request) {
		checkHookBody(t, r)
		writeJSON(w, 201, hookObj(5, "https://gm.example/hook"))
	})
	mux.HandleFunc("POST /api/v1/orgs/acme/hooks", func(w http.ResponseWriter, r *http.Request) {
		checkHookBody(t, r)
		writeJSON(w, 201, hookObj(6, "https://gm.example/hook"))
	})
	srv, calls := serve(t, mux)
	p := newProv(t, srv.URL)
	h, err := p.CreateRepoHook(context.Background(), "o", "r", hookOpts)
	if err != nil || h.ID != 5 || h.URL != "https://gm.example/hook" || !h.Active {
		t.Fatalf("repo hook = %+v, %v", h, err)
	}
	h, err = p.CreateOrgHook(context.Background(), "acme", hookOpts)
	if err != nil || h.ID != 6 {
		t.Fatalf("org hook = %+v, %v", h, err)
	}
	_ = calls
}

func TestHookErrors(t *testing.T) {
	for _, c := range []struct {
		status int
		want   string
		notFnd bool
	}{
		{404, "404", true},
		{403, "write:repository", false},
		{422, "422", false},
	} {
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			msg(w, c.status, "bad "+hookSecret)
		})
		srv, _ := serve(t, mux)
		p := newProv(t, srv.URL)
		_, err1 := p.CreateRepoHook(context.Background(), "o", "r", hookOpts)
		_, err2 := p.CreateOrgHook(context.Background(), "o", hookOpts)
		_, err3 := p.OrgHooks(context.Background(), "o")
		for i, err := range []error{err1, err2, err3} {
			if err == nil {
				t.Fatalf("%d: status %d: expected error", i, c.status)
			}
			if errors.Is(err, forge.ErrNotFound) != c.notFnd {
				t.Errorf("%d: status %d: ErrNotFound mismatch: %v", i, c.status, err)
			}
			if i < 2 || c.status != 403 {
				if !strings.Contains(err.Error(), c.want) {
					t.Errorf("%d: status %d: error %q lacks %q", i, c.status, err, c.want)
				}
			}
			if i < 2 && strings.Contains(err.Error(), hookSecret) {
				t.Errorf("%d: error leaks the secret: %v", i, err)
			}
		}
	}
}
