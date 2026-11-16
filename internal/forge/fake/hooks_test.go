package fake

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"gitmirror/internal/forge"
)

var _ forge.HookProvider = (*Provider)(nil)

func TestHooksRepoAndOrg(t *testing.T) {
	ctx := context.Background()
	p := New(Options{})
	p.AddOwner("acme", true)
	if _, err := p.CreateRepo(ctx, forge.CreateOptions{Owner: "acme", Name: "r"}); err != nil {
		t.Fatal(err)
	}
	o := forge.HookOptions{URL: "https://gm/hook", Secret: "s", Events: []string{"push"}}

	h, err := p.CreateRepoHook(ctx, "acme", "r", o)
	if err != nil || h.ID == 0 || h.URL != o.URL || !h.Active || h.ContentType != "json" {
		t.Fatalf("hook = %+v, %v", h, err)
	}
	oh, err := p.CreateOrgHook(ctx, "acme", o)
	if err != nil || oh.ID == h.ID {
		t.Fatalf("org hook = %+v, %v", oh, err)
	}
	if l, _ := p.RepoHooks(ctx, "acme", "r"); len(l) != 1 || l[0].ID != h.ID {
		t.Fatalf("repo hooks = %+v", l)
	}
	if l, _ := p.OrgHooks(ctx, "ACME"); len(l) != 1 || l[0].ID != oh.ID {
		t.Fatalf("org hooks = %+v", l)
	}
	rec := p.CreatedHooks()
	if len(rec) != 2 || rec[0].Scope != "acme/r" || rec[1].Scope != "acme" || rec[0].Secret != "s" ||
		!reflect.DeepEqual(rec[0].Hook, h) {
		t.Fatalf("recorded = %+v", rec)
	}
	if len(p.HookEvents()) == 0 {
		t.Fatal("no HookEvents")
	}
}

func TestHooksNotFoundAndInjectedErrors(t *testing.T) {
	ctx := context.Background()
	p := New(Options{})
	if _, err := p.RepoHooks(ctx, "x", "y"); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("RepoHooks err = %v", err)
	}
	if _, err := p.CreateRepoHook(ctx, "x", "y", forge.HookOptions{}); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("CreateRepoHook err = %v", err)
	}
	if _, err := p.OrgHooks(ctx, "nope"); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("OrgHooks err = %v", err)
	}
	if _, err := p.CreateOrgHook(ctx, "nope", forge.HookOptions{}); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("CreateOrgHook err = %v", err)
	}
	p.AddOwner("acme", true)
	boom := errors.New("boom")
	p.FailNext("CreateOrgHook", boom)
	if _, err := p.CreateOrgHook(ctx, "acme", forge.HookOptions{}); !errors.Is(err, boom) {
		t.Fatalf("injected err = %v", err)
	}
	p.FailAlways("OrgHooks", boom)
	if _, err := p.OrgHooks(ctx, "acme"); !errors.Is(err, boom) {
		t.Fatalf("injected err = %v", err)
	}
	if len(p.CreatedHooks()) != 0 {
		t.Fatal("failed create was recorded")
	}
}
