package fake

import (
	"context"
	"fmt"
	"strings"

	"gitmirror/internal/forge"
)

var _ forge.HookProvider = (*Provider)(nil)

// CreatedHook records one successful CreateRepoHook/CreateOrgHook call.
type CreatedHook struct {
	// Scope is "owner/repo" for a repository hook and the organisation name
	// for an organisation hook.
	Scope  string
	Secret string
	Hook   forge.Hook
}

// hookState is kept on the Provider's side table so Provider's struct and
// constructor stay untouched.
type hookState struct {
	next    int64
	byScope map[string][]forge.Hook
	created []CreatedHook
}

func (p *Provider) hooks() *hookState {
	if p.hookSt == nil {
		p.hookSt = &hookState{byScope: map[string][]forge.Hook{}}
	}
	return p.hookSt
}

// HookEvents implements forge.HookProvider.
func (p *Provider) HookEvents() []string {
	return []string{"push", "create", "delete", "release", "repository", "wiki"}
}

// CreatedHooks returns every hook created through the provider, in order.
func (p *Provider) CreatedHooks() []CreatedHook {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := append([]CreatedHook(nil), p.hooks().created...)
	for i := range out {
		out[i].Hook.Events = append([]string(nil), out[i].Hook.Events...)
	}
	return out
}

func cloneHooks(hs []forge.Hook) []forge.Hook {
	out := make([]forge.Hook, 0, len(hs))
	for _, h := range hs {
		h.Events = append([]string(nil), h.Events...)
		out = append(out, h)
	}
	return out
}

func (p *Provider) orgKnown(org string) bool {
	isOrg, ok := p.owners[strings.ToLower(org)]
	return ok && isOrg
}

func (p *Provider) listHooks(ctx context.Context, method, scope string, exists func() bool) ([]forge.Hook, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.begin(ctx, method); err != nil {
		return nil, err
	}
	if !exists() {
		return nil, fmt.Errorf("fake: %s: %w", scope, forge.ErrNotFound)
	}
	return cloneHooks(p.hooks().byScope[strings.ToLower(scope)]), nil
}

func (p *Provider) createHook(ctx context.Context, method, scope string, exists func() bool, o forge.HookOptions) (forge.Hook, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.begin(ctx, method); err != nil {
		return forge.Hook{}, err
	}
	if !exists() {
		return forge.Hook{}, fmt.Errorf("fake: %s: %w", scope, forge.ErrNotFound)
	}
	st := p.hooks()
	st.next++
	h := forge.Hook{ID: st.next, URL: o.URL, Events: append([]string(nil), o.Events...), Active: true, ContentType: "json"}
	k := strings.ToLower(scope)
	st.byScope[k] = append(st.byScope[k], h)
	st.created = append(st.created, CreatedHook{Scope: scope, Secret: o.Secret, Hook: h})
	return cloneHooks([]forge.Hook{h})[0], nil
}

func (p *Provider) repoExists(owner, repo string) bool {
	_, ok := p.repos[key(owner, repo)]
	return ok
}

// RepoHooks implements forge.HookProvider.
func (p *Provider) RepoHooks(ctx context.Context, owner, repo string) ([]forge.Hook, error) {
	return p.listHooks(ctx, "RepoHooks", owner+"/"+repo, func() bool { return p.repoExists(owner, repo) })
}

// CreateRepoHook implements forge.HookProvider.
func (p *Provider) CreateRepoHook(ctx context.Context, owner, repo string, o forge.HookOptions) (forge.Hook, error) {
	return p.createHook(ctx, "CreateRepoHook", owner+"/"+repo, func() bool { return p.repoExists(owner, repo) }, o)
}

// OrgHooks implements forge.HookProvider. The organisation must exist
// (AddOwner with isOrg, or EnsureOrg).
func (p *Provider) OrgHooks(ctx context.Context, org string) ([]forge.Hook, error) {
	return p.listHooks(ctx, "OrgHooks", org, func() bool { return p.orgKnown(org) })
}

// CreateOrgHook implements forge.HookProvider.
func (p *Provider) CreateOrgHook(ctx context.Context, org string, o forge.HookOptions) (forge.Hook, error) {
	return p.createHook(ctx, "CreateOrgHook", org, func() bool { return p.orgKnown(org) }, o)
}
