package gitea

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"gitmirror/internal/forge"
)

var _ forge.HookProvider = (*Provider)(nil)

// Hook management uses the Gitea API (swagger: https://gitea.com/api/swagger,
// types in modules/structs/hook.go:
// https://github.com/go-gitea/gitea/blob/main/modules/structs/hook.go):
//
//	GET/POST /repos/{owner}/{repo}/hooks
//	GET/POST /orgs/{org}/hooks
//
// CreateHookOption is {"type","config"{"url","content_type","secret"},
// "events","branch_filter","active"}. The list endpoints do not return the
// secret. Forgejo shares this surface. Neither forge offers account-wide
// hooks for an ordinary user through the repo/org endpoints: /user/hooks
// only configures hooks of the authenticated account's own settings page.

// HookEvents implements forge.HookProvider.
func (p *Provider) HookEvents() []string {
	return []string{"push", "create", "delete", "release", "repository", "wiki"}
}

type apiHook struct {
	ID     int64             `json:"id"`
	Active bool              `json:"active"`
	Events []string          `json:"events"`
	Config map[string]string `json:"config"`
}

func (h apiHook) hook() forge.Hook {
	return forge.Hook{
		ID: h.ID, URL: h.Config["url"], Events: append([]string(nil), h.Events...),
		Active: h.Active, ContentType: h.Config["content_type"],
	}
}

type createHookBody struct {
	Type         string            `json:"type"`
	Active       bool              `json:"active"`
	Events       []string          `json:"events"`
	BranchFilter string            `json:"branch_filter"`
	Config       map[string]string `json:"config"`
}

func orgPath(org string) (string, error) {
	o, err := seg(org)
	if err != nil {
		return "", err
	}
	return "/orgs/" + o, nil
}

// redacted keeps the error chain (for errors.Is) but hides the secret in the text.
type redacted struct {
	msg string
	err error
}

func (e *redacted) Error() string { return e.msg }
func (e *redacted) Unwrap() error { return e.err }

// hookErr adds context, a permission hint to 403s, and scrubs secret.
func hookErr(op, secret string, err error) error {
	if err == nil {
		return nil
	}
	var ae *apiError
	if errors.As(err, &ae) && ae.status == http.StatusForbidden {
		err = fmt.Errorf("%s: forbidden: the token needs the write:repository (repo hooks) or write:organization (org hooks) scope and admin rights on the target: %w", op, err)
	} else {
		err = fmt.Errorf("%s: %w", op, err)
	}
	if secret == "" || !strings.Contains(err.Error(), secret) {
		return err
	}
	return &redacted{msg: strings.ReplaceAll(err.Error(), secret, "***"), err: err}
}

func (p *Provider) listHooks(ctx context.Context, base string) ([]forge.Hook, error) {
	hs, err := getPages[apiHook](ctx, p, base+"/hooks")
	if err != nil {
		return nil, err
	}
	out := make([]forge.Hook, 0, len(hs))
	for _, h := range hs {
		out = append(out, h.hook())
	}
	return out, nil
}

func (p *Provider) createHook(ctx context.Context, base string, o forge.HookOptions) (forge.Hook, error) {
	cfg := map[string]string{"url": o.URL, "content_type": "json"}
	if o.Secret != "" {
		cfg["secret"] = o.Secret
	}
	var h apiHook
	_, err := p.do(ctx, http.MethodPost, base+"/hooks", nil, createHookBody{
		Type: "gitea", Active: true, Events: o.Events, BranchFilter: "*", Config: cfg,
	}, &h)
	if err != nil {
		return forge.Hook{}, err
	}
	return h.hook(), nil
}

// RepoHooks implements forge.HookProvider.
func (p *Provider) RepoHooks(ctx context.Context, owner, repo string) ([]forge.Hook, error) {
	rp, err := repoPath(owner, repo)
	if err != nil {
		return nil, err
	}
	hs, err := p.listHooks(ctx, rp)
	if err != nil {
		return nil, hookErr(fmt.Sprintf("list hooks of %s/%s", owner, repo), "", err)
	}
	return hs, nil
}

// OrgHooks implements forge.HookProvider.
func (p *Provider) OrgHooks(ctx context.Context, org string) ([]forge.Hook, error) {
	op, err := orgPath(org)
	if err != nil {
		return nil, err
	}
	hs, err := p.listHooks(ctx, op)
	if err != nil {
		return nil, hookErr("list hooks of org "+org, "", err)
	}
	return hs, nil
}

// CreateRepoHook implements forge.HookProvider.
func (p *Provider) CreateRepoHook(ctx context.Context, owner, repo string, o forge.HookOptions) (forge.Hook, error) {
	rp, err := repoPath(owner, repo)
	if err != nil {
		return forge.Hook{}, err
	}
	h, err := p.createHook(ctx, rp, o)
	if err != nil {
		return forge.Hook{}, hookErr(fmt.Sprintf("create hook on %s/%s", owner, repo), o.Secret, err)
	}
	return h, nil
}

// CreateOrgHook implements forge.HookProvider.
func (p *Provider) CreateOrgHook(ctx context.Context, org string, o forge.HookOptions) (forge.Hook, error) {
	op, err := orgPath(org)
	if err != nil {
		return forge.Hook{}, err
	}
	h, err := p.createHook(ctx, op, o)
	if err != nil {
		return forge.Hook{}, hookErr("create hook on org "+org, o.Secret, err)
	}
	return h, nil
}
