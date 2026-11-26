// Package github implements the forge provider contract for GitHub.com and
// GitHub Enterprise Server using the REST API.
//
// Retries and rate-limit waiting are not done here: they belong to the
// *http.Client supplied in forge.Options.HTTPClient.
package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v93/github"

	"gitmirror/internal/forge"
)

func init() {
	forge.Register("github", func(o forge.Options) (forge.Provider, error) {
		p, err := New(o)
		if err != nil {
			return nil, err
		}
		return p, nil
	})
}

var (
	_ forge.Provider        = (*Provider)(nil)
	_ forge.ReleaseProvider = (*Provider)(nil)
	_ forge.WikiProvider    = (*Provider)(nil)
	_ forge.StarProvider    = (*Provider)(nil)
	_ forge.OrgProvider     = (*Provider)(nil)
	_ forge.WebhookProvider = (*Provider)(nil)
)

const (
	perPage         = 100
	maxWebhookBytes = 25 << 20
)

// Provider talks to one GitHub (or GHE) instance with one token.
type Provider struct {
	client  *gh.Client
	plain   *http.Client // credential-free client for redirect targets
	token   string
	user    string
	gitBase string

	mu  sync.Mutex
	who string // cached authenticated login; empty until fetched successfully
}

// New builds a provider. o.URL is the web base URL: empty or
// https://github.com selects GitHub.com, anything else is treated as GitHub
// Enterprise Server. It makes no network calls.
func New(o forge.Options) (*Provider, error) {
	base := strings.TrimRight(strings.TrimSpace(o.URL), "/")
	if base == "" || strings.EqualFold(base, "https://github.com") {
		return newWithEndpoints(o, "https://api.github.com/", "https://uploads.github.com/", "https://github.com")
	}
	return newWithEndpoints(o, base+"/api/v3/", base+"/api/uploads/", base)
}

func newWithEndpoints(o forge.Options, apiURL, uploadURL, gitBase string) (*Provider, error) {
	hc := o.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	}
	opts := []gh.ClientOptionsFunc{
		gh.WithHTTPClient(hc),
		gh.WithEnterpriseURLs(apiURL, uploadURL),
		// Waiting out rate limits is the HTTP client's job, not go-github's.
		gh.WithDisableRateLimitCheck(),
	}
	// An empty token means a public, read-only site: requests go out
	// anonymously and only public repositories can be read.
	if o.Token != "" {
		opts = append(opts, gh.WithAuthToken(o.Token))
	}
	client, err := gh.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("github: build client: %w", err)
	}
	plain := *hc // same transport/retry behaviour, but never carries our token
	return &Provider{
		client:  client,
		plain:   &plain,
		token:   o.Token,
		user:    o.Username,
		gitBase: strings.TrimRight(gitBase, "/"),
	}, nil
}

// Type implements forge.Provider.
func (p *Provider) Type() string { return "github" }

// GitURL implements forge.Provider.
func (p *Provider) GitURL(owner, name string) string {
	return p.gitBase + "/" + owner + "/" + name + ".git"
}

// WikiGitURL implements forge.WikiProvider.
func (p *Provider) WikiGitURL(owner, name string) string {
	return p.gitBase + "/" + owner + "/" + name + ".wiki.git"
}

// GitAuth implements forge.Provider.
func (p *Provider) GitAuth() forge.Auth {
	u := p.user
	if u == "" {
		u = "x-access-token"
	}
	return forge.Auth{Username: u, Token: p.token}
}

// ---------------------------------------------------------------- errors

// apiError carries a redacted message while still unwrapping to the cause
// (and to forge sentinels), so errors.Is/As keep working.
type apiError struct {
	msg  string
	errs []error
}

func (e *apiError) Error() string   { return e.msg }
func (e *apiError) Unwrap() []error { return e.errs }

func (p *Provider) redact(s string) string {
	if p.token == "" {
		return s
	}
	return strings.ReplaceAll(s, p.token, "***")
}

// wrap adds context, maps 404 to forge.ErrNotFound, describes rate-limit
// errors with their reset time and scrubs the token from the message.
func (p *Provider) wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	var rl *gh.RateLimitError
	var abuse *gh.AbuseRateLimitError
	var er *gh.ErrorResponse
	switch {
	case errors.As(err, &rl):
		msg := "rate limit exceeded"
		if !rl.Rate.Reset.IsZero() {
			msg += ", resets at " + rl.Rate.Reset.UTC().Format(time.RFC3339)
		}
		return &apiError{msg: p.redact(fmt.Sprintf("github: %s: %s: %s", op, msg, rl.Message)), errs: []error{err}}
	case errors.As(err, &abuse):
		msg := "secondary rate limit exceeded, retry later"
		if abuse.RetryAfter != nil {
			msg = "secondary rate limit exceeded, retry after " + abuse.RetryAfter.String()
		}
		return &apiError{msg: p.redact(fmt.Sprintf("github: %s: %s: %s", op, msg, abuse.Message)), errs: []error{err}}
	case errors.As(err, &er) && er.Response != nil && er.Response.StatusCode == http.StatusNotFound:
		return &apiError{
			msg:  p.redact(fmt.Sprintf("github: %s: %v: %s", op, forge.ErrNotFound, er.Message)),
			errs: []error{forge.ErrNotFound, err},
		}
	}
	return &apiError{msg: p.redact(fmt.Sprintf("github: %s: %v", op, err)), errs: []error{err}}
}

// ---------------------------------------------------------------- identity

// Login implements forge.Identity.
func (p *Provider) Login(ctx context.Context) (string, error) { return p.login(ctx) }

// login returns the authenticated user's login, fetching it once. Failures
// are not cached.
func (p *Provider) login(ctx context.Context) (string, error) {
	p.mu.Lock()
	who := p.who
	p.mu.Unlock()
	if who != "" {
		return who, nil
	}
	// Fetch outside the lock so a caller with a short deadline is not stuck
	// behind another caller's in-flight request.
	u, _, err := p.client.Users.Get(ctx, "")
	if err != nil {
		return "", p.wrap("get authenticated user", err)
	}
	if u.GetLogin() == "" {
		return "", errors.New("github: get authenticated user: empty login in response")
	}
	who = u.GetLogin()
	p.mu.Lock()
	p.who = who
	p.mu.Unlock()
	return who, nil
}

// ---------------------------------------------------------------- repos

// toRepo maps a GitHub repository. Empty is true when the reported size is 0;
// GitHub offers no cheaper signal, and size can lag behind a first push, so a
// just-pushed repo may briefly still read as empty.
func toRepo(r *gh.Repository) forge.Repo {
	vis := r.GetVisibility()
	return forge.Repo{
		ID:            r.GetID(),
		Owner:         r.GetOwner().GetLogin(),
		Name:          r.GetName(),
		Description:   r.GetDescription(),
		Homepage:      r.GetHomepage(),
		Topics:        append([]string(nil), r.Topics...),
		DefaultBranch: r.GetDefaultBranch(),
		Private:       r.GetPrivate() || vis == "private" || vis == "internal",
		Archived:      r.GetArchived(),
		Fork:          r.GetFork(),
		Empty:         r.GetSize() == 0,
		HasWiki:       r.GetHasWiki(),
	}
}

func toRepos(rs []*gh.Repository) []forge.Repo {
	out := make([]forge.Repo, 0, len(rs))
	for _, r := range rs {
		out = append(out, toRepo(r))
	}
	return out
}

// paginate collects every page, following NextPage.
func paginate[T any](fetch func(page int) ([]T, *gh.Response, error)) ([]T, error) {
	var all []T
	page := 0
	for {
		items, resp, err := fetch(page)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		if resp == nil || resp.NextPage == 0 {
			return all, nil
		}
		page = resp.NextPage
	}
}

// GetRepo implements forge.Provider.
func (p *Provider) GetRepo(ctx context.Context, owner, name string) (*forge.Repo, error) {
	r, _, err := p.client.Repositories.Get(ctx, owner, name)
	if err != nil {
		return nil, p.wrap(fmt.Sprintf("get repo %s/%s", owner, name), err)
	}
	out := toRepo(r)
	return &out, nil
}

// ListRepos implements forge.Provider.
func (p *Provider) ListRepos(ctx context.Context, owner string) ([]forge.Repo, error) {
	op := "list repos of " + owner
	me, err := p.login(ctx)
	if err != nil {
		return nil, err
	}
	var rs []*gh.Repository
	if strings.EqualFold(owner, me) {
		rs, err = paginate(func(page int) ([]*gh.Repository, *gh.Response, error) {
			return p.client.Repositories.ListByAuthenticatedUser(ctx, &gh.RepositoryListByAuthenticatedUserOptions{
				Affiliation: "owner", Visibility: "all", ListOptions: gh.ListOptions{Page: page, PerPage: perPage},
			})
		})
	} else {
		var u *gh.User
		u, _, err = p.client.Users.Get(ctx, owner)
		if err != nil {
			return nil, p.wrap(op, err)
		}
		if u.GetType() == "Organization" {
			rs, err = paginate(func(page int) ([]*gh.Repository, *gh.Response, error) {
				return p.client.Repositories.ListByOrg(ctx, owner, &gh.RepositoryListByOrgOptions{
					Type: "all", ListOptions: gh.ListOptions{Page: page, PerPage: perPage},
				})
			})
		} else {
			rs, err = paginate(func(page int) ([]*gh.Repository, *gh.Response, error) {
				return p.client.Repositories.ListByUser(ctx, owner, &gh.RepositoryListByUserOptions{
					Type: "owner", ListOptions: gh.ListOptions{Page: page, PerPage: perPage},
				})
			})
		}
	}
	if err != nil {
		return nil, p.wrap(op, err)
	}
	return toRepos(rs), nil
}

func ptr[T any](v T) *T { return &v }

// CreateRepo implements forge.Provider.
func (p *Provider) CreateRepo(ctx context.Context, opts forge.CreateOptions) (*forge.Repo, error) {
	op := fmt.Sprintf("create repo %s/%s", opts.Owner, opts.Name)
	me, err := p.login(ctx)
	if err != nil {
		return nil, err
	}
	org := opts.Owner
	if strings.EqualFold(org, me) {
		org = "" // authenticated user namespace
	}
	req := gh.CreateRepositoryRequest{
		Name:        opts.Name,
		Private:     ptr(opts.Private),
		AutoInit:    ptr(false),
		HasIssues:   ptr(false),
		HasProjects: ptr(false),
		HasWiki:     ptr(true), // the wiki must exist to be mirrored
	}
	if opts.Description != "" {
		req.Description = ptr(opts.Description)
	}
	r, _, err := p.client.Repositories.Create(ctx, org, req)
	if err != nil {
		var er *gh.ErrorResponse
		if errors.As(err, &er) && er.Response != nil && er.Response.StatusCode == http.StatusUnprocessableEntity && alreadyExists(er) {
			return nil, &apiError{
				msg:  p.redact(fmt.Sprintf("github: %s: repository already exists: %s", op, er.Message)),
				errs: []error{err},
			}
		}
		return nil, p.wrap(op, err)
	}
	out := toRepo(r)
	return &out, nil
}

func alreadyExists(er *gh.ErrorResponse) bool {
	if strings.Contains(strings.ToLower(er.Message), "already exists") {
		return true
	}
	for _, e := range er.Errors {
		if e.Code == "already_exists" || strings.Contains(strings.ToLower(e.Message), "already exists") {
			return true
		}
	}
	return false
}

// UpdateRepoSettings implements forge.Provider. Archived repositories reject
// edits, so an unarchive is sent first and an archive last.
func (p *Provider) UpdateRepoSettings(ctx context.Context, owner, name string, s forge.Settings) error {
	op := fmt.Sprintf("update repo %s/%s", owner, name)
	patch := func(r *gh.Repository) error {
		_, _, err := p.client.Repositories.Edit(ctx, owner, name, r)
		return p.wrap(op, err)
	}

	if s.Archived != nil && !*s.Archived {
		if err := patch(&gh.Repository{Archived: ptr(false)}); err != nil {
			return err
		}
	}

	main := &gh.Repository{Description: s.Description, Homepage: s.Homepage, DefaultBranch: s.DefaultBranch, Private: s.Private}
	if s.Description != nil || s.Homepage != nil || s.DefaultBranch != nil || s.Private != nil {
		if err := patch(main); err != nil {
			return err
		}
	}

	if s.Topics != nil {
		topics := make([]string, 0, len(*s.Topics))
		for _, t := range *s.Topics {
			topics = append(topics, strings.ToLower(t))
		}
		if _, _, err := p.client.Repositories.ReplaceAllTopics(ctx, owner, name, topics); err != nil {
			return p.wrap(op+" topics", err)
		}
	}

	if s.Actions != nil {
		// Actions are a separate endpoint per repository, and turning them
		// off needs admin rights on the destination.
		if _, _, err := p.client.Repositories.UpdateActionsPermissions(ctx, owner, name,
			gh.ActionsPermissionsRepository{Enabled: *s.Actions}); err != nil {
			return p.wrap(op+" actions", err)
		}
	}

	if s.Archived != nil && *s.Archived {
		return patch(&gh.Repository{Archived: ptr(true)})
	}
	return nil
}

// RateLimit implements forge.Provider using the core resource.
// Probe implements forge.Probe. The rate-limit endpoint answers without a
// token, so it shows that this address speaks GitHub's API.
func (p *Provider) Probe(ctx context.Context) error {
	if _, _, err := p.client.RateLimit.Get(ctx); err != nil {
		return p.wrap("probe site", err)
	}
	return nil
}

func (p *Provider) RateLimit(ctx context.Context) (forge.Rate, error) {
	rl, _, err := p.client.RateLimit.Get(ctx)
	if err != nil {
		return forge.Rate{}, p.wrap("get rate limit", err)
	}
	if rl == nil || rl.Core == nil {
		return forge.Rate{}, nil
	}
	return forge.Rate{Limit: rl.Core.Limit, Remaining: rl.Core.Remaining, Reset: rl.Core.Reset.Time}, nil
}

// ---------------------------------------------------------------- releases

func toRelease(r *gh.RepositoryRelease) forge.Release {
	out := forge.Release{
		ID: r.ID, Tag: r.TagName, Name: r.GetName(), Body: r.GetBody(),
		Target: r.TargetCommitish, Draft: r.Draft, Prerelease: r.Prerelease,
	}
	for _, a := range r.Assets {
		out.Assets = append(out.Assets, toAsset(a))
	}
	return out
}

func toAsset(a *gh.ReleaseAsset) forge.Asset {
	return forge.Asset{ID: a.GetID(), Name: a.GetName(), Size: int64(a.GetSize()), ContentType: a.GetContentType()}
}

// ListReleases implements forge.ReleaseProvider.
func (p *Provider) ListReleases(ctx context.Context, owner, name string) ([]forge.Release, error) {
	rs, err := paginate(func(page int) ([]*gh.RepositoryRelease, *gh.Response, error) {
		return p.client.Repositories.ListReleases(ctx, owner, name, &gh.ListOptions{Page: page, PerPage: perPage})
	})
	if err != nil {
		return nil, p.wrap(fmt.Sprintf("list releases %s/%s", owner, name), err)
	}
	out := make([]forge.Release, 0, len(rs))
	for _, r := range rs {
		out = append(out, toRelease(r))
	}
	return out, nil
}

// CreateRelease implements forge.ReleaseProvider.
func (p *Provider) CreateRelease(ctx context.Context, owner, name string, r forge.Release) (*forge.Release, error) {
	req := gh.CreateReleaseRequest{
		TagName: r.Tag, Name: ptr(r.Name), Body: ptr(r.Body),
		Draft: ptr(r.Draft), Prerelease: ptr(r.Prerelease),
	}
	if r.Target != "" {
		req.TargetCommitish = ptr(r.Target)
	}
	got, _, err := p.client.Repositories.CreateRelease(ctx, owner, name, req)
	if err != nil {
		return nil, p.wrap(fmt.Sprintf("create release %s on %s/%s", r.Tag, owner, name), err)
	}
	out := toRelease(got)
	return &out, nil
}

// UpdateRelease implements forge.ReleaseProvider; the release is addressed by r.ID.
func (p *Provider) UpdateRelease(ctx context.Context, owner, name string, r forge.Release) (*forge.Release, error) {
	req := gh.UpdateReleaseRequest{
		TagName: ptr(r.Tag), Name: ptr(r.Name), Body: ptr(r.Body),
		Draft: ptr(r.Draft), Prerelease: ptr(r.Prerelease),
	}
	if r.Target != "" {
		req.TargetCommitish = ptr(r.Target)
	}
	got, _, err := p.client.Repositories.UpdateRelease(ctx, owner, name, r.ID, req)
	if err != nil {
		return nil, p.wrap(fmt.Sprintf("update release %d on %s/%s", r.ID, owner, name), err)
	}
	out := toRelease(got)
	return &out, nil
}

// DeleteRepo implements forge.RepoDeleter. A 403 almost always means the token
// lacks deletion rights, so say which permission is needed.
func (p *Provider) DeleteRepo(ctx context.Context, owner, name string) error {
	_, err := p.client.Repositories.Delete(ctx, owner, name)
	op := fmt.Sprintf("delete repo %s/%s", owner, name)
	var er *gh.ErrorResponse
	if errors.As(err, &er) && er.Response != nil && er.Response.StatusCode == http.StatusForbidden {
		return &apiError{
			msg:  p.redact(fmt.Sprintf("github: %s: forbidden: the token needs the delete_repo scope (classic) or Administration: write permission (fine-grained): %s", op, er.Message)),
			errs: []error{err},
		}
	}
	return p.wrap(op, err)
}

// DeleteRelease implements forge.ReleaseProvider.
func (p *Provider) DeleteRelease(ctx context.Context, owner, name string, id int64) error {
	_, err := p.client.Repositories.DeleteRelease(ctx, owner, name, id)
	return p.wrap(fmt.Sprintf("delete release %d on %s/%s", id, owner, name), err)
}

// DownloadAsset implements forge.ReleaseProvider. GitHub answers with a
// redirect to object storage; that hop is fetched with a client that does
// not carry the API token. The body is returned unbuffered.
func (p *Provider) DownloadAsset(ctx context.Context, owner, name string, a forge.Asset) (io.ReadCloser, error) {
	rc, _, err := p.client.Repositories.DownloadReleaseAsset(ctx, owner, name, a.ID, p.plain)
	if err != nil {
		return nil, p.wrap(fmt.Sprintf("download asset %d of %s/%s", a.ID, owner, name), err)
	}
	return rc, nil
}

// UploadAsset implements forge.ReleaseProvider, streaming body with the
// size declared in a.Size.
func (p *Provider) UploadAsset(ctx context.Context, owner, name string, releaseID int64, a forge.Asset, body io.Reader) (*forge.Asset, error) {
	op := fmt.Sprintf("upload asset %q to release %d of %s/%s", a.Name, releaseID, owner, name)
	ct := a.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	u := fmt.Sprintf("repos/%s/%s/releases/%d/assets?name=%s", owner, name, releaseID, url.QueryEscape(a.Name))
	req, err := p.client.NewUploadRequest(ctx, u, body, a.Size, ct)
	if err != nil {
		return nil, p.wrap(op, err)
	}
	var asset gh.ReleaseAsset
	if _, err := p.client.Do(req, &asset); err != nil {
		return nil, p.wrap(op, err)
	}
	out := toAsset(&asset)
	return &out, nil
}

// DeleteAsset implements forge.ReleaseProvider.
func (p *Provider) DeleteAsset(ctx context.Context, owner, name string, releaseID, assetID int64) error {
	_, err := p.client.Repositories.DeleteReleaseAsset(ctx, owner, name, assetID)
	return p.wrap(fmt.Sprintf("delete asset %d of %s/%s", assetID, owner, name), err)
}

// ---------------------------------------------------------------- stars, orgs

// ListStarred implements forge.StarProvider.
func (p *Provider) ListStarred(ctx context.Context, user string) ([]forge.Repo, error) {
	rs, err := paginate(func(page int) ([]*gh.StarredRepository, *gh.Response, error) {
		return p.client.Activity.ListStarred(ctx, user, &gh.ActivityListStarredOptions{
			ListOptions: gh.ListOptions{Page: page, PerPage: perPage},
		})
	})
	if err != nil {
		return nil, p.wrap("list starred by "+user, err)
	}
	out := make([]forge.Repo, 0, len(rs))
	for _, s := range rs {
		if s.Repository != nil {
			out = append(out, toRepo(s.Repository))
		}
	}
	return out, nil
}

// EnsureOrg implements forge.OrgProvider. GitHub's API cannot create
// organizations, so this only verifies that the namespace exists.
func (p *Provider) EnsureOrg(ctx context.Context, name string) error {
	_, _, err := p.client.Organizations.Get(ctx, name)
	if err == nil {
		return nil
	}
	if !errors.Is(p.wrap("get org "+name, err), forge.ErrNotFound) {
		return p.wrap("get org "+name, err)
	}
	me, lerr := p.login(ctx)
	if lerr != nil {
		return lerr
	}
	if strings.EqualFold(name, me) {
		return nil
	}
	return fmt.Errorf("github: organization %q does not exist and the API cannot create it; create it in the GitHub UI first: %w", name, forge.ErrUnsupported)
}

// ---------------------------------------------------------------- webhooks

// ParseWebhook implements forge.WebhookProvider.
func (p *Provider) ParseWebhook(r *http.Request, secret string) (*forge.Event, error) {
	if secret == "" {
		return nil, errors.New("github: webhook secret must not be empty")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBytes+1))
	if err != nil {
		return nil, fmt.Errorf("github: read webhook body: %w", err)
	}
	if len(body) > maxWebhookBytes {
		return nil, fmt.Errorf("github: webhook body exceeds %d bytes", maxWebhookBytes)
	}

	sig, ok := strings.CutPrefix(r.Header.Get("X-Hub-Signature-256"), "sha256=")
	if !ok {
		return nil, fmt.Errorf("github: missing or malformed X-Hub-Signature-256: %w", forge.ErrBadSignature)
	}
	got, err := hex.DecodeString(sig)
	if err != nil {
		return nil, fmt.Errorf("github: signature is not hex: %w", forge.ErrBadSignature)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return nil, fmt.Errorf("github: signature mismatch: %w", forge.ErrBadSignature)
	}

	payload := body
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt == "application/x-www-form-urlencoded" {
		vals, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, fmt.Errorf("github: parse form webhook: %w", err)
		}
		pl := vals.Get("payload")
		if pl == "" {
			return nil, errors.New("github: form webhook has no payload field")
		}
		payload = []byte(pl)
	}

	var hook struct {
		Repository *struct {
			Name  string `json:"name"`
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(payload, &hook); err != nil {
		return nil, fmt.Errorf("github: parse webhook payload: %w", err)
	}

	ev := &forge.Event{Kind: eventKind(r.Header.Get("X-GitHub-Event"))}
	if hook.Repository != nil {
		ev.Owner, ev.Name = hook.Repository.Owner.Login, hook.Repository.Name
	}
	return ev, nil
}

func eventKind(h string) forge.EventKind {
	switch h {
	case "push", "create", "delete":
		return forge.EventPush
	case "release":
		return forge.EventRelease
	case "repository":
		return forge.EventRepo
	case "gollum":
		return forge.EventWiki
	}
	return forge.EventOther
}
