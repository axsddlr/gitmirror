//go:build integration

package integration

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"gitmirror/internal/forge"
)

// TestHookManagement creates a repository hook and an organisation hook on a
// real Gitea through the provider and reads them back.
func TestHookManagement(t *testing.T) {
	requireDocker(t)
	in := startGitea(t, "H", "hooker")
	ctx := context.Background()
	hp, ok := in.prov.(forge.HookProvider)
	if !ok {
		t.Fatal("gitea provider does not implement forge.HookProvider")
	}
	in.createRepo(t, "hooked", "", false)
	if err := in.prov.(forge.OrgProvider).EnsureOrg(ctx, "hookorg"); err != nil {
		t.Fatal(err)
	}
	o := forge.HookOptions{URL: "https://gitmirror.example.com/webhook", Secret: "integration-secret", Events: hp.HookEvents()}

	rh, err := hp.CreateRepoHook(ctx, "hooker", "hooked", o)
	if err != nil {
		t.Fatalf("CreateRepoHook: %v", err)
	}
	oh, err := hp.CreateOrgHook(ctx, "hookorg", o)
	if err != nil {
		t.Fatalf("CreateOrgHook: %v", err)
	}
	for name, h := range map[string]forge.Hook{"repo": rh, "org": oh} {
		if h.ID == 0 || h.URL != o.URL || !h.Active || h.ContentType != "json" {
			t.Errorf("%s hook = %+v", name, h)
		}
	}

	rl, err := hp.RepoHooks(ctx, "hooker", "hooked")
	if err != nil || len(rl) != 1 || rl[0].ID != rh.ID || rl[0].URL != o.URL {
		t.Fatalf("RepoHooks = %+v, %v", rl, err)
	}
	// Gitea does not preserve event order.
	want := append([]string(nil), hp.HookEvents()...)
	slices.Sort(want)
	for name, got := range map[string][]string{"repo created": rh.Events, "repo listed": rl[0].Events} {
		got = append([]string(nil), got...)
		slices.Sort(got)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s events = %v, want %v", name, got, want)
		}
	}
	ol, err := hp.OrgHooks(ctx, "hookorg")
	if err != nil || len(ol) != 1 || ol[0].ID != oh.ID || ol[0].URL != o.URL {
		t.Fatalf("OrgHooks = %+v, %v", ol, err)
	}
	t.Logf("repo hook events: %v; org hook events: %v", rl[0].Events, ol[0].Events)
}
