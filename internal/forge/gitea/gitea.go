// Package gitea implements the forge provider contract for Gitea and its
// fork Forgejo (including Codeberg), which share the same /api/v1 surface.
//
// It uses only net/http so every call carries its own context. Retries and
// rate-limit waits belong to the *http.Client supplied in forge.Options.
package gitea

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"gitmirror/internal/forge"
)

const (
	pageLimit    = 50
	maxPages     = 100000
	errBodyLimit = 8 << 10
	maxHookBody  = 25 << 20
)

func init() {
	forge.Register("gitea", func(o forge.Options) (forge.Provider, error) {
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

// Provider talks to one Gitea/Forgejo instance.
type Provider struct {
	base     string // configured URL, no trailing slash
	api      string // base + /api/v1
	token    string
	username string
	hc       *http.Client

	mu    sync.Mutex
	login string

	assetURLs sync.Map // asset id (int64) -> browser_download_url (string)
}

// New builds a provider. It performs no network I/O.
func New(o forge.Options) (*Provider, error) {
	base := strings.TrimRight(strings.TrimSpace(o.URL), "/")
	if base == "" {
		return nil, errors.New("gitea: URL is required")
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("gitea: invalid URL %q", o.URL)
	}
	if o.Token == "" {
		return nil, errors.New("gitea: token is required")
	}
	hc := o.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	}
	return &Provider{base: base, api: base + "/api/v1", token: o.Token, username: o.Username, hc: hc}, nil
}

// Type implements forge.Provider.
func (p *Provider) Type() string { return "gitea" }

// GitURL implements forge.Provider.
func (p *Provider) GitURL(owner, name string) string {
	return p.base + "/" + owner + "/" + name + ".git"
}

// WikiGitURL implements forge.WikiProvider.
func (p *Provider) WikiGitURL(owner, name string) string {
	return p.base + "/" + owner + "/" + name + ".wiki.git"
}

// GitAuth returns the basic-auth pair for git over HTTP.
//
// VERIFIED: Gitea's services/auth/basic.go (parseAuthBasic/VerifyAuthToken,
// https://github.com/go-gitea/gitea/blob/main/services/auth/basic.go) uses the
// password as the token whenever it is not empty/"x-oauth-basic", looks the
// token up with GetAccessTokenBySHA and takes the user from the token. The
// username is never compared with the token owner, so any non-empty username
// works. Hence no /user lookup is needed here.
func (p *Provider) GitAuth() forge.Auth {
	u := p.username
	if u == "" {
		u = "git"
	}
	return forge.Auth{Username: u, Token: p.token}
}

// RateLimit implements forge.Provider; Gitea exposes no rate-limit API.
func (p *Provider) RateLimit(context.Context) (forge.Rate, error) { return forge.Rate{}, nil }

// ---- HTTP plumbing ----

type apiError struct {
	method, path string
	status       int
	message      string
}

func (e *apiError) Error() string {
	s := fmt.Sprintf("gitea: %s %s: status %d", e.method, e.path, e.status)
	if e.message != "" {
		s += ": " + e.message
	}
	return s
}

func (e *apiError) Unwrap() error {
	if e.status == http.StatusNotFound {
		return forge.ErrNotFound
	}
	return nil
}

func newAPIError(method, path string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyLimit))
	var m struct {
		Message string `json:"message"`
	}
	text := ""
	if json.Unmarshal(b, &m) == nil && m.Message != "" {
		text = m.Message
	} else {
		text = strings.TrimSpace(string(b))
	}
	if len(text) > 512 {
		text = text[:512] + "..."
	}
	return &apiError{method: method, path: path, status: resp.StatusCode, message: text}
}

func (p *Provider) newReq(ctx context.Context, method, path string, q url.Values, body io.Reader) (*http.Request, error) {
	u := p.api + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+p.token)
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// do performs a JSON request; in (if non-nil) is the body, out (if non-nil)
// receives the decoded response. It returns the response headers.
func (p *Provider) do(ctx context.Context, method, path string, q url.Values, in, out any) (http.Header, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := p.newReq(ctx, method, path, q, body)
	if err != nil {
		return nil, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return p.finish(req, method, path, out)
}

func (p *Provider) finish(req *http.Request, method, path string, out any) (http.Header, error) {
	resp, err := p.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gitea: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, newAPIError(method, path, resp)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return nil, fmt.Errorf("gitea: %s %s: decode response: %w", method, path, err)
		}
	} else {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, errBodyLimit))
	}
	return resp.Header, nil
}

// getPages fetches every page of a list endpoint. It stops on an empty page
// (the server may cap limit below pageLimit) or once X-Total-Count is reached.
func getPages[T any](ctx context.Context, p *Provider, path string) ([]T, error) {
	var all []T
	for page := 1; page <= maxPages; page++ {
		q := url.Values{"page": {strconv.Itoa(page)}, "limit": {strconv.Itoa(pageLimit)}}
		var items []T
		h, err := p.do(ctx, http.MethodGet, path, q, nil, &items)
		if err != nil {
			return nil, err
		}
		if len(items) == 0 {
			break
		}
		all = append(all, items...)
		if total, err := strconv.Atoi(h.Get("X-Total-Count")); err == nil && len(all) >= total {
			break
		}
	}
	return all, nil
}

func seg(s string) (string, error) {
	if s == "" || s == "." || s == ".." || strings.Contains(s, "/") || strings.Contains(s, "\\") {
		return "", fmt.Errorf("gitea: invalid path segment %q", s)
	}
	return url.PathEscape(s), nil
}

func repoPath(owner, name string) (string, error) {
	o, err := seg(owner)
	if err != nil {
		return "", err
	}
	n, err := seg(name)
	if err != nil {
		return "", err
	}
	return "/repos/" + o + "/" + n, nil
}

// Login returns the authenticated user's login (cached after first success).
func (p *Provider) Login(ctx context.Context) (string, error) {
	p.mu.Lock()
	l := p.login
	p.mu.Unlock()
	if l != "" {
		return l, nil
	}
	var u struct {
		Login string `json:"login"`
	}
	if _, err := p.do(ctx, http.MethodGet, "/user", nil, nil, &u); err != nil {
		return "", err
	}
	if u.Login == "" {
		return "", errors.New("gitea: GET /user returned no login")
	}
	p.mu.Lock()
	p.login = u.Login
	p.mu.Unlock()
	return u.Login, nil
}

// ---- repositories ----

type apiRepo struct {
	Name  string `json:"name"`
	Owner struct {
		Login string `json:"login"`
	} `json:"owner"`
	Description   string   `json:"description"`
	Website       string   `json:"website"`
	Topics        []string `json:"topics"`
	DefaultBranch string   `json:"default_branch"`
	Private       bool     `json:"private"`
	Archived      bool     `json:"archived"`
	Fork          bool     `json:"fork"`
	Empty         bool     `json:"empty"`
	HasWiki       bool     `json:"has_wiki"`
}

func (a apiRepo) repo() forge.Repo {
	return forge.Repo{Owner: a.Owner.Login, Name: a.Name, Description: a.Description, Homepage: a.Website,
		Topics: a.Topics, DefaultBranch: a.DefaultBranch, Private: a.Private, Archived: a.Archived,
		Fork: a.Fork, Empty: a.Empty, HasWiki: a.HasWiki}
}

func repos(in []apiRepo) []forge.Repo {
	out := make([]forge.Repo, 0, len(in))
	for _, a := range in {
		out = append(out, a.repo())
	}
	return out
}

// GetRepo implements forge.Provider.
func (p *Provider) GetRepo(ctx context.Context, owner, name string) (*forge.Repo, error) {
	rp, err := repoPath(owner, name)
	if err != nil {
		return nil, err
	}
	var a apiRepo
	if _, err := p.do(ctx, http.MethodGet, rp, nil, nil, &a); err != nil {
		return nil, err
	}
	if a.Topics == nil {
		var t struct {
			Topics []string `json:"topics"`
		}
		if _, err := p.do(ctx, http.MethodGet, rp+"/topics", nil, nil, &t); err != nil {
			return nil, err
		}
		a.Topics = t.Topics
	}
	r := a.repo()
	return &r, nil
}

// ListRepos implements forge.Provider.
func (p *Provider) ListRepos(ctx context.Context, owner string) ([]forge.Repo, error) {
	o, err := seg(owner)
	if err != nil {
		return nil, err
	}
	isOrg := true
	if _, err := p.do(ctx, http.MethodGet, "/orgs/"+o, nil, nil, nil); err != nil {
		if !errors.Is(err, forge.ErrNotFound) {
			return nil, err
		}
		isOrg = false
	}
	if isOrg {
		list, err := getPages[apiRepo](ctx, p, "/orgs/"+o+"/repos")
		return repos(list), err
	}
	login, err := p.Login(ctx)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(login, owner) {
		// /user/repos includes private repos and collaborations; keep ours.
		list, err := getPages[apiRepo](ctx, p, "/user/repos")
		if err != nil {
			return nil, err
		}
		mine := list[:0]
		for _, a := range list {
			if strings.EqualFold(a.Owner.Login, owner) {
				mine = append(mine, a)
			}
		}
		return repos(mine), nil
	}
	list, err := getPages[apiRepo](ctx, p, "/users/"+o+"/repos")
	return repos(list), err
}

// ListStarred implements forge.StarProvider.
func (p *Provider) ListStarred(ctx context.Context, user string) ([]forge.Repo, error) {
	u, err := seg(user)
	if err != nil {
		return nil, err
	}
	list, err := getPages[apiRepo](ctx, p, "/users/"+u+"/starred")
	return repos(list), err
}

// CreateRepo implements forge.Provider.
func (p *Provider) CreateRepo(ctx context.Context, opts forge.CreateOptions) (*forge.Repo, error) {
	o, err := seg(opts.Owner)
	if err != nil {
		return nil, err
	}
	if _, err := seg(opts.Name); err != nil {
		return nil, err
	}
	login, err := p.Login(ctx)
	if err != nil {
		return nil, err
	}
	path := "/orgs/" + o + "/repos"
	if strings.EqualFold(login, opts.Owner) {
		path = "/user/repos"
	}
	body := map[string]any{"name": opts.Name, "description": opts.Description, "private": opts.Private, "auto_init": false}
	var a apiRepo
	if _, err := p.do(ctx, http.MethodPost, path, nil, body, &a); err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.status == http.StatusConflict {
			return nil, fmt.Errorf("gitea: repository %s/%s already exists: %w", opts.Owner, opts.Name, err)
		}
		return nil, err
	}
	r := a.repo()
	if r.Owner == "" {
		r.Owner = opts.Owner
	}
	// Wiki mirroring needs the wiki enabled; best effort.
	rp, _ := repoPath(r.Owner, r.Name)
	if _, err := p.do(ctx, http.MethodPatch, rp, nil, map[string]any{"has_wiki": true}, nil); err == nil {
		r.HasWiki = true
	}
	return &r, nil
}

// UpdateRepoSettings implements forge.Provider. Unarchive happens first and
// archive last, because archived repos reject other changes.
func (p *Provider) UpdateRepoSettings(ctx context.Context, owner, name string, s forge.Settings) error {
	rp, err := repoPath(owner, name)
	if err != nil {
		return err
	}
	if s.Archived != nil && !*s.Archived {
		if _, err := p.do(ctx, http.MethodPatch, rp, nil, map[string]any{"archived": false}, nil); err != nil {
			return err
		}
	}
	main := map[string]any{}
	if s.Description != nil {
		main["description"] = *s.Description
	}
	if s.Homepage != nil {
		main["website"] = *s.Homepage
	}
	if s.DefaultBranch != nil {
		main["default_branch"] = *s.DefaultBranch
	}
	if s.Private != nil {
		main["private"] = *s.Private
	}
	if len(main) > 0 {
		if _, err := p.do(ctx, http.MethodPatch, rp, nil, main, nil); err != nil {
			return err
		}
	}
	if s.Topics != nil {
		topics := make([]string, 0, len(*s.Topics))
		for _, t := range *s.Topics {
			topics = append(topics, strings.ToLower(t))
		}
		if _, err := p.do(ctx, http.MethodPut, rp+"/topics", nil, map[string]any{"topics": topics}, nil); err != nil {
			return err
		}
	}
	if s.Archived != nil && *s.Archived {
		if _, err := p.do(ctx, http.MethodPatch, rp, nil, map[string]any{"archived": true}, nil); err != nil {
			return err
		}
	}
	return nil
}

// EnsureOrg implements forge.OrgProvider.
func (p *Provider) EnsureOrg(ctx context.Context, name string) error {
	n, err := seg(name)
	if err != nil {
		return err
	}
	_, err = p.do(ctx, http.MethodGet, "/orgs/"+n, nil, nil, nil)
	if err == nil {
		return nil
	}
	if !errors.Is(err, forge.ErrNotFound) {
		return err
	}
	login, err := p.Login(ctx)
	if err != nil {
		return err
	}
	if strings.EqualFold(login, name) {
		return nil
	}
	_, err = p.do(ctx, http.MethodPost, "/orgs", nil, map[string]any{"username": name, "visibility": "private"}, nil)
	var ae *apiError
	if errors.As(err, &ae) && ae.status == http.StatusUnprocessableEntity && strings.Contains(strings.ToLower(ae.message), "already exist") {
		return nil
	}
	return err
}

// ---- releases ----

type apiAsset struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	DownloadURL string `json:"browser_download_url"`
}

type apiRelease struct {
	ID         int64      `json:"id"`
	Tag        string     `json:"tag_name"`
	Name       string     `json:"name"`
	Body       string     `json:"body"`
	Target     string     `json:"target_commitish"`
	Draft      bool       `json:"draft"`
	Prerelease bool       `json:"prerelease"`
	Assets     []apiAsset `json:"assets"`
}

func (p *Provider) release(a apiRelease) forge.Release {
	r := forge.Release{ID: a.ID, Tag: a.Tag, Name: a.Name, Body: a.Body, Target: a.Target, Draft: a.Draft, Prerelease: a.Prerelease}
	for _, as := range a.Assets {
		if as.DownloadURL != "" {
			p.assetURLs.Store(as.ID, as.DownloadURL)
		}
		r.Assets = append(r.Assets, forge.Asset{ID: as.ID, Name: as.Name, Size: as.Size})
	}
	return r
}

func releaseBody(r forge.Release) map[string]any {
	return map[string]any{"tag_name": r.Tag, "name": r.Name, "body": r.Body, "target_commitish": r.Target,
		"draft": r.Draft, "prerelease": r.Prerelease}
}

// ListReleases implements forge.ReleaseProvider.
func (p *Provider) ListReleases(ctx context.Context, owner, name string) ([]forge.Release, error) {
	rp, err := repoPath(owner, name)
	if err != nil {
		return nil, err
	}
	list, err := getPages[apiRelease](ctx, p, rp+"/releases")
	if err != nil {
		return nil, err
	}
	out := make([]forge.Release, 0, len(list))
	for _, a := range list {
		out = append(out, p.release(a))
	}
	return out, nil
}

// CreateRelease implements forge.ReleaseProvider.
func (p *Provider) CreateRelease(ctx context.Context, owner, name string, r forge.Release) (*forge.Release, error) {
	rp, err := repoPath(owner, name)
	if err != nil {
		return nil, err
	}
	var a apiRelease
	if _, err := p.do(ctx, http.MethodPost, rp+"/releases", nil, releaseBody(r), &a); err != nil {
		return nil, err
	}
	out := p.release(a)
	return &out, nil
}

// UpdateRelease implements forge.ReleaseProvider.
func (p *Provider) UpdateRelease(ctx context.Context, owner, name string, r forge.Release) (*forge.Release, error) {
	rp, err := repoPath(owner, name)
	if err != nil {
		return nil, err
	}
	var a apiRelease
	if _, err := p.do(ctx, http.MethodPatch, rp+"/releases/"+strconv.FormatInt(r.ID, 10), nil, releaseBody(r), &a); err != nil {
		return nil, err
	}
	out := p.release(a)
	return &out, nil
}

// DeleteRepo implements forge.RepoDeleter. A 403 almost always means the token
// lacks deletion rights, so say which permission is needed.
func (p *Provider) DeleteRepo(ctx context.Context, owner, name string) error {
	rp, err := repoPath(owner, name)
	if err != nil {
		return err
	}
	_, err = p.do(ctx, http.MethodDelete, rp, nil, nil, nil)
	var ae *apiError
	if errors.As(err, &ae) && ae.status == http.StatusForbidden {
		return fmt.Errorf("gitea: delete repo %s/%s: forbidden: the token needs the write:repository scope and the user needs admin rights on the repository: %w", owner, name, err)
	}
	return err
}

// DeleteRelease implements forge.ReleaseProvider.
func (p *Provider) DeleteRelease(ctx context.Context, owner, name string, id int64) error {
	rp, err := repoPath(owner, name)
	if err != nil {
		return err
	}
	_, err = p.do(ctx, http.MethodDelete, rp+"/releases/"+strconv.FormatInt(id, 10), nil, nil, nil)
	return err
}

// DeleteAsset implements forge.ReleaseProvider.
func (p *Provider) DeleteAsset(ctx context.Context, owner, name string, releaseID, assetID int64) error {
	rp, err := repoPath(owner, name)
	if err != nil {
		return err
	}
	_, err = p.do(ctx, http.MethodDelete, rp+"/releases/"+strconv.FormatInt(releaseID, 10)+"/assets/"+strconv.FormatInt(assetID, 10), nil, nil, nil)
	return err
}

// sameOrigin reports whether u has the same scheme and host as the base URL.
func (p *Provider) sameOrigin(u *url.URL) bool {
	b, err := url.Parse(p.base)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Scheme, b.Scheme) && strings.EqualFold(u.Host, b.Host)
}

// DownloadAsset implements forge.ReleaseProvider. The token is sent only to
// the configured instance, never to another host.
func (p *Provider) DownloadAsset(ctx context.Context, owner, name string, a forge.Asset) (io.ReadCloser, error) {
	v, ok := p.assetURLs.Load(a.ID)
	if !ok {
		// Not seen yet: listing the releases fills the cache.
		if _, err := p.ListReleases(ctx, owner, name); err != nil {
			return nil, err
		}
		if v, ok = p.assetURLs.Load(a.ID); !ok {
			return nil, fmt.Errorf("gitea: asset %d in %s/%s: %w", a.ID, owner, name, forge.ErrNotFound)
		}
	}
	raw := v.(string)
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("gitea: bad asset URL for asset %d: %w", a.ID, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	if p.sameOrigin(u) {
		req.Header.Set("Authorization", "token "+p.token)
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gitea: download asset %d: %w", a.ID, err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		return nil, newAPIError(http.MethodGet, "asset "+strconv.FormatInt(a.ID, 10), resp)
	}
	return resp.Body, nil
}

// UploadAsset implements forge.ReleaseProvider. The multipart body is
// produced on the fly through a pipe, so the file is never held in memory.
func (p *Provider) UploadAsset(ctx context.Context, owner, name string, releaseID int64, a forge.Asset, body io.Reader) (*forge.Asset, error) {
	rp, err := repoPath(owner, name)
	if err != nil {
		return nil, err
	}
	path := rp + "/releases/" + strconv.FormatInt(releaseID, 10) + "/assets"
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	srcErr := make(chan error, 1)
	go func() {
		err := func() error {
			part, err := mw.CreateFormFile("attachment", a.Name)
			if err != nil {
				return err
			}
			if _, err := io.Copy(part, body); err != nil {
				return err
			}
			return mw.Close()
		}()
		srcErr <- err
		pw.CloseWithError(err) // nil closes normally
	}()
	req, err := p.newReq(ctx, http.MethodPost, path, url.Values{"name": {a.Name}}, pr)
	if err != nil {
		pr.CloseWithError(err)
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	var out apiAsset
	_, err = p.finish(req, http.MethodPost, path, &out)
	pr.CloseWithError(io.ErrClosedPipe) // unblock the writer if the server answered early
	if serr := <-srcErr; serr != nil && !errors.Is(serr, io.ErrClosedPipe) {
		return nil, fmt.Errorf("gitea: upload %q: reading source: %w", a.Name, serr)
	}
	if err != nil {
		return nil, err
	}
	if out.DownloadURL != "" {
		p.assetURLs.Store(out.ID, out.DownloadURL)
	}
	return &forge.Asset{ID: out.ID, Name: out.Name, Size: out.Size, ContentType: a.ContentType}, nil
}

// ---- webhooks ----

// ParseWebhook implements forge.WebhookProvider.
func (p *Provider) ParseWebhook(r *http.Request, secret string) (*forge.Event, error) {
	if secret == "" {
		return nil, errors.New("gitea: webhook secret must not be empty")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxHookBody+1))
	if err != nil {
		return nil, fmt.Errorf("gitea: read webhook body: %w", err)
	}
	if len(body) > maxHookBody {
		return nil, fmt.Errorf("gitea: webhook body exceeds %d bytes", maxHookBody)
	}
	sig := r.Header.Get("X-Gitea-Signature")
	if sig == "" {
		sig = r.Header.Get("X-Forgejo-Signature")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	if sig == "" || !hmac.Equal([]byte(sig), []byte(want)) {
		return nil, fmt.Errorf("gitea: webhook signature mismatch: %w", forge.ErrBadSignature)
	}
	event := r.Header.Get("X-Gitea-Event")
	if event == "" {
		event = r.Header.Get("X-Forgejo-Event")
	}
	var payload struct {
		Repository struct {
			Name  string `json:"name"`
			Owner struct {
				Login    string `json:"login"`
				Username string `json:"username"`
			} `json:"owner"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("gitea: decode webhook payload: %w", err)
	}
	owner := payload.Repository.Owner.Login
	if owner == "" {
		owner = payload.Repository.Owner.Username
	}
	kind := forge.EventOther
	switch event {
	case "push", "create", "delete":
		kind = forge.EventPush
	case "release":
		kind = forge.EventRelease
	case "repository":
		kind = forge.EventRepo
	case "wiki":
		kind = forge.EventWiki
	}
	return &forge.Event{Kind: kind, Owner: owner, Name: payload.Repository.Name}, nil
}
