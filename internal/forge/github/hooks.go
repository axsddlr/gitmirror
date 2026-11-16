package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	gh "github.com/google/go-github/v93/github"

	"gitmirror/internal/forge"
)

var _ forge.HookProvider = (*Provider)(nil)

// Hook management uses GitHub's REST webhook endpoints:
//
//	https://docs.github.com/en/rest/repos/webhooks  (GET/POST /repos/{owner}/{repo}/hooks)
//	https://docs.github.com/en/rest/orgs/webhooks   (GET/POST /orgs/{org}/hooks)
//
// A create body is {"name":"web","active":true,"events":[...],"config":{"url",
// "content_type","secret","insecure_ssl"}}; go-github sets name to "web".
// GitHub has no account-wide hooks for a user account: only repositories and
// organisations carry hooks.

// HookEvents implements forge.HookProvider.
func (p *Provider) HookEvents() []string {
	return []string{"push", "create", "delete", "release", "repository", "gollum"}
}

func toHook(h *gh.Hook) forge.Hook {
	out := forge.Hook{ID: h.GetID(), Events: append([]string(nil), h.Events...), Active: h.GetActive()}
	if h.Config != nil {
		out.URL = h.Config.GetURL()
		out.ContentType = h.Config.GetContentType()
	}
	return out
}

func toHooks(hs []*gh.Hook) []forge.Hook {
	out := make([]forge.Hook, 0, len(hs))
	for _, h := range hs {
		out = append(out, toHook(h))
	}
	return out
}

func newHook(o forge.HookOptions) *gh.Hook {
	active, ct, insecure := true, "json", "0"
	cfg := &gh.HookConfig{
		URL:         &o.URL,
		ContentType: &ct,
		InsecureSSL: &insecure,
	}
	if o.Secret != "" {
		cfg.Secret = &o.Secret
	}
	return &gh.Hook{Active: &active, Events: o.Events, Config: cfg}
}

// hookErr wraps err like wrap, adds a permission hint for 403 and a
// validation hint for 422, and scrubs the webhook secret from the text.
func (p *Provider) hookErr(op, secret, perm string, err error) error {
	if err == nil {
		return nil
	}
	wrapped := p.wrap(op, err)
	msg := wrapped.Error()
	var er *gh.ErrorResponse
	if errors.As(err, &er) && er.Response != nil {
		switch er.Response.StatusCode {
		case http.StatusForbidden:
			msg += "; forbidden: the token needs " + perm + " (classic) or Webhooks: write (fine-grained) and admin rights on the target"
		case http.StatusUnprocessableEntity:
			msg += "; GitHub rejected the webhook (validation failed): check the payload URL, events and whether the hook already exists"
		}
	}
	if secret != "" {
		msg = strings.ReplaceAll(msg, secret, "***")
	}
	return &apiError{msg: msg, errs: []error{wrapped}}
}

// RepoHooks implements forge.HookProvider.
func (p *Provider) RepoHooks(ctx context.Context, owner, repo string) ([]forge.Hook, error) {
	hs, err := paginate(func(page int) ([]*gh.Hook, *gh.Response, error) {
		return p.client.Repositories.ListHooks(ctx, owner, repo, &gh.ListOptions{Page: page, PerPage: perPage})
	})
	if err != nil {
		return nil, p.hookErr(fmt.Sprintf("list hooks of %s/%s", owner, repo), "", "the admin:repo_hook scope", err)
	}
	return toHooks(hs), nil
}

// OrgHooks implements forge.HookProvider.
func (p *Provider) OrgHooks(ctx context.Context, org string) ([]forge.Hook, error) {
	hs, err := paginate(func(page int) ([]*gh.Hook, *gh.Response, error) {
		return p.client.Organizations.ListHooks(ctx, org, &gh.ListOptions{Page: page, PerPage: perPage})
	})
	if err != nil {
		return nil, p.hookErr("list hooks of org "+org, "", "the admin:org_hook scope", err)
	}
	return toHooks(hs), nil
}

// CreateRepoHook implements forge.HookProvider.
func (p *Provider) CreateRepoHook(ctx context.Context, owner, repo string, o forge.HookOptions) (forge.Hook, error) {
	h, _, err := p.client.Repositories.CreateHook(ctx, owner, repo, newHook(o))
	if err != nil {
		return forge.Hook{}, p.hookErr(fmt.Sprintf("create hook on %s/%s", owner, repo), o.Secret, "the admin:repo_hook scope", err)
	}
	return toHook(h), nil
}

// CreateOrgHook implements forge.HookProvider.
func (p *Provider) CreateOrgHook(ctx context.Context, org string, o forge.HookOptions) (forge.Hook, error) {
	h, _, err := p.client.Organizations.CreateHook(ctx, org, newHook(o))
	if err != nil {
		return forge.Hook{}, p.hookErr("create hook on org "+org, o.Secret, "the admin:org_hook scope", err)
	}
	return toHook(h), nil
}
