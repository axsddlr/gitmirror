// Package fake is an in-memory forge used as a test double. It implements
// every forge interface, is safe for concurrent use, supports failure
// injection and records the calls made against it.
package fake

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"gitmirror/internal/forge"
)

var (
	_ forge.Provider        = (*Provider)(nil)
	_ forge.ReleaseProvider = (*Provider)(nil)
	_ forge.WikiProvider    = (*Provider)(nil)
	_ forge.StarProvider    = (*Provider)(nil)
	_ forge.OrgProvider     = (*Provider)(nil)
	_ forge.WebhookProvider = (*Provider)(nil)
	_ forge.RepoDeleter     = (*Provider)(nil)
)

// SignatureHeader carries the webhook signature.
const SignatureHeader = "X-Fake-Signature"

// Options configures a fake provider.
type Options struct {
	// Type is the provider type name; default "fake".
	Type string
	// GitBaseDir, when set, makes repos real bare git repositories on disk.
	GitBaseDir string
	// Username and Token are returned by GitAuth. Username also names the
	// user namespace that always exists.
	Username string
	Token    string
}

type repoEntry struct {
	repo     forge.Repo
	releases []forge.Release // ascending by ID
}

// Provider is the fake forge.
type Provider struct {
	opts Options

	mu          sync.Mutex
	owners      map[string]bool // lowercase name -> isOrg
	repos       map[string]*repoEntry
	stars       map[string]map[string]forge.Repo // user -> lower full name -> repo
	assetData   map[int64][]byte
	nextRelID   int64
	nextAssetID int64
	rate        forge.Rate
	calls       []string
	failNext    map[string][]error
	failAlways  map[string]error
}

// New builds a fake provider.
func New(opts Options) *Provider {
	if opts.Type == "" {
		opts.Type = "fake"
	}
	return &Provider{
		opts:       opts,
		owners:     map[string]bool{},
		repos:      map[string]*repoEntry{},
		stars:      map[string]map[string]forge.Repo{},
		assetData:  map[int64][]byte{},
		failNext:   map[string][]error{},
		failAlways: map[string]error{},
	}
}

func key(owner, name string) string { return strings.ToLower(owner + "/" + name) }

func cloneRepo(r forge.Repo) forge.Repo {
	if r.Topics != nil {
		r.Topics = append([]string(nil), r.Topics...)
	}
	return r
}

func cloneRelease(r forge.Release) forge.Release {
	if r.Assets != nil {
		r.Assets = append([]forge.Asset(nil), r.Assets...)
	}
	return r
}

// begin records the call, honours a cancelled context and injected failures.
// The caller must hold p.mu.
func (p *Provider) begin(ctx context.Context, method string) error {
	p.calls = append(p.calls, method)
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if q := p.failNext[method]; len(q) > 0 {
		err := q[0]
		if len(q) == 1 {
			delete(p.failNext, method)
		} else {
			p.failNext[method] = q[1:]
		}
		return err
	}
	return p.failAlways[method]
}

// ---- failure injection and inspection ----

// FailNext makes the next call to method return err (one-shot, queued).
func (p *Provider) FailNext(method string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failNext[method] = append(p.failNext[method], err)
}

// FailAlways makes every call to method return err until cleared.
func (p *Provider) FailAlways(method string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failAlways[method] = err
}

// ClearFailures removes all injected failures.
func (p *Provider) ClearFailures() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failNext = map[string][]error{}
	p.failAlways = map[string]error{}
}

// Calls returns the ordered names of interface methods invoked so far.
func (p *Provider) Calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

// SetRate sets the value returned by RateLimit.
func (p *Provider) SetRate(r forge.Rate) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rate = r
}

// ---- git helpers ----

func (p *Provider) bareDir(owner, name string, wiki bool) string {
	suffix := ".git"
	if wiki {
		suffix = ".wiki.git"
	}
	return filepath.Join(p.opts.GitBaseDir, strings.ToLower(owner), strings.ToLower(name)+suffix)
}

func (p *Provider) gitURL(owner, name string, wiki bool) string {
	if p.opts.GitBaseDir == "" {
		suffix := ".git"
		if wiki {
			suffix = ".wiki.git"
		}
		return "https://fake.invalid/" + owner + "/" + name + suffix
	}
	dir := p.bareDir(owner, name, wiki)
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	path := filepath.ToSlash(dir)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path // Windows drive paths: file:///D:/...
	}
	u := url.URL{Scheme: "file", Path: path}
	return u.String()
}

func initBare(dir, branch string) error {
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if branch == "" {
		branch = "main"
	}
	out, err := exec.Command("git", "init", "--bare", "-q", "--initial-branch="+branch, dir).CombinedOutput()
	if err != nil {
		return fmt.Errorf("fake: git init %s: %v: %s", dir, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ---- seeding helpers (not recorded in Calls, never fail-injected) ----

// AddOwner registers a user or organisation namespace.
func (p *Provider) AddOwner(name string, isOrg bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.owners[strings.ToLower(name)] = isOrg
}

// AddRepo seeds a repo (replacing one with the same name, keeping its
// releases) and creates its bare git repository when GitBaseDir is set.
func (p *Provider) AddRepo(r forge.Repo) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	r = cloneRepo(r)
	if p.opts.GitBaseDir != "" {
		if err := initBare(p.bareDir(r.Owner, r.Name, false), r.DefaultBranch); err != nil {
			return err
		}
	}
	ol := strings.ToLower(r.Owner)
	if _, ok := p.owners[ol]; !ok {
		p.owners[ol] = false
	}
	k := key(r.Owner, r.Name)
	if e, ok := p.repos[k]; ok {
		e.repo = r
		return nil
	}
	p.repos[k] = &repoEntry{repo: r}
	return nil
}

// RemoveRepo deletes a repo, its releases and asset data, and its bare git
// (and wiki) repositories when GitBaseDir is set, as if it had been deleted
// on the forge. A missing repo is a no-op.
func (p *Provider) RemoveRepo(owner, name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.removeLocked(owner, name)
}

// removeLocked implements RemoveRepo; the caller must hold p.mu. It reports
// whether the repo existed.
func (p *Provider) removeLocked(owner, name string) bool {
	k := key(owner, name)
	e, ok := p.repos[k]
	if !ok {
		return false
	}
	for _, rel := range e.releases {
		for _, a := range rel.Assets {
			delete(p.assetData, a.ID)
		}
	}
	delete(p.repos, k)
	if p.opts.GitBaseDir != "" {
		os.RemoveAll(p.bareDir(owner, name, false))
		os.RemoveAll(p.bareDir(owner, name, true))
	}
	return true
}

// Star records that user has starred repo.
func (p *Provider) Star(user string, repo forge.Repo) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.stars[strings.ToLower(user)]
	if m == nil {
		m = map[string]forge.Repo{}
		p.stars[strings.ToLower(user)] = m
	}
	m[key(repo.Owner, repo.Name)] = cloneRepo(repo)
}

// InitWiki creates the bare wiki repository and marks the repo as having one.
func (p *Provider) InitWiki(owner, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.repos[key(owner, name)]
	if !ok {
		return fmt.Errorf("fake: repo %s/%s: %w", owner, name, forge.ErrNotFound)
	}
	if p.opts.GitBaseDir != "" {
		if err := initBare(p.bareDir(owner, name, true), "master"); err != nil {
			return err
		}
	}
	e.repo.HasWiki = true
	return nil
}

// AssetBytes returns a copy of an uploaded asset's content, or nil.
func (p *Provider) AssetBytes(owner, name string, releaseID, assetID int64) []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.repos[key(owner, name)]
	if !ok {
		return nil
	}
	for _, rel := range e.releases {
		if rel.ID != releaseID {
			continue
		}
		for _, a := range rel.Assets {
			if a.ID == assetID {
				return append([]byte(nil), p.assetData[assetID]...)
			}
		}
	}
	return nil
}

// ---- forge.Provider ----

// Type implements forge.Provider.
func (p *Provider) Type() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, "Type")
	return p.opts.Type
}

func (p *Provider) notFound(owner, name string) error {
	return fmt.Errorf("fake: repo %s/%s: %w", owner, name, forge.ErrNotFound)
}

// GetRepo implements forge.Provider.
func (p *Provider) GetRepo(ctx context.Context, owner, name string) (*forge.Repo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.begin(ctx, "GetRepo"); err != nil {
		return nil, err
	}
	e, ok := p.repos[key(owner, name)]
	if !ok {
		return nil, p.notFound(owner, name)
	}
	r := cloneRepo(e.repo)
	return &r, nil
}

// ListRepos implements forge.Provider.
func (p *Provider) ListRepos(ctx context.Context, owner string) ([]forge.Repo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.begin(ctx, "ListRepos"); err != nil {
		return nil, err
	}
	ol := strings.ToLower(owner)
	if _, ok := p.owners[ol]; !ok {
		return nil, fmt.Errorf("fake: owner %s: %w", owner, forge.ErrNotFound)
	}
	out := []forge.Repo{}
	for _, e := range p.repos {
		if strings.ToLower(e.repo.Owner) == ol {
			out = append(out, cloneRepo(e.repo))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// CreateRepo implements forge.Provider.
func (p *Provider) CreateRepo(ctx context.Context, opts forge.CreateOptions) (*forge.Repo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.begin(ctx, "CreateRepo"); err != nil {
		return nil, err
	}
	ol := strings.ToLower(opts.Owner)
	_, known := p.owners[ol]
	isSelf := p.opts.Username != "" && ol == strings.ToLower(p.opts.Username)
	if !known && !isSelf {
		return nil, fmt.Errorf("fake: owner %s: %w", opts.Owner, forge.ErrNotFound)
	}
	k := key(opts.Owner, opts.Name)
	if _, dup := p.repos[k]; dup {
		return nil, fmt.Errorf("fake: repo %s/%s already exists", opts.Owner, opts.Name)
	}
	if p.opts.GitBaseDir != "" {
		if err := initBare(p.bareDir(opts.Owner, opts.Name, false), "main"); err != nil {
			return nil, err
		}
	}
	if !known {
		p.owners[ol] = false
	}
	r := forge.Repo{
		Owner:         opts.Owner,
		Name:          opts.Name,
		Description:   opts.Description,
		Private:       opts.Private,
		DefaultBranch: "main",
		Empty:         true,
	}
	p.repos[k] = &repoEntry{repo: r}
	out := cloneRepo(r)
	return &out, nil
}

// UpdateRepoSettings implements forge.Provider.
func (p *Provider) UpdateRepoSettings(ctx context.Context, owner, name string, s forge.Settings) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.begin(ctx, "UpdateRepoSettings"); err != nil {
		return err
	}
	e, ok := p.repos[key(owner, name)]
	if !ok {
		return p.notFound(owner, name)
	}
	if s.Description != nil {
		e.repo.Description = *s.Description
	}
	if s.Homepage != nil {
		e.repo.Homepage = *s.Homepage
	}
	if s.Topics != nil {
		e.repo.Topics = append([]string{}, (*s.Topics)...)
	}
	if s.DefaultBranch != nil {
		e.repo.DefaultBranch = *s.DefaultBranch
	}
	if s.Private != nil {
		e.repo.Private = *s.Private
	}
	if s.Archived != nil {
		e.repo.Archived = *s.Archived
	}
	return nil
}

// GitURL implements forge.Provider.
func (p *Provider) GitURL(owner, name string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, "GitURL")
	return p.gitURL(owner, name, false)
}

// GitAuth implements forge.Provider.
func (p *Provider) GitAuth() forge.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, "GitAuth")
	return forge.Auth{Username: p.opts.Username, Token: p.opts.Token}
}

// Login implements forge.Identity; it returns the configured Username.
func (p *Provider) Login(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return p.opts.Username, nil
}

// RateLimit implements forge.Provider.
func (p *Provider) RateLimit(ctx context.Context) (forge.Rate, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.begin(ctx, "RateLimit"); err != nil {
		return forge.Rate{}, err
	}
	return p.rate, nil
}

// ---- forge.RepoDeleter ----

// DeleteRepo implements forge.RepoDeleter with RemoveRepo's effect; it
// returns forge.ErrNotFound when the repo does not exist.
func (p *Provider) DeleteRepo(ctx context.Context, owner, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.begin(ctx, "DeleteRepo"); err != nil {
		return err
	}
	if !p.removeLocked(owner, name) {
		return p.notFound(owner, name)
	}
	return nil
}

// ---- forge.WikiProvider ----

// WikiGitURL implements forge.WikiProvider.
func (p *Provider) WikiGitURL(owner, name string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, "WikiGitURL")
	return p.gitURL(owner, name, true)
}

// ---- forge.OrgProvider ----

// EnsureOrg implements forge.OrgProvider.
func (p *Provider) EnsureOrg(ctx context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.begin(ctx, "EnsureOrg"); err != nil {
		return err
	}
	l := strings.ToLower(name)
	if _, ok := p.owners[l]; !ok {
		p.owners[l] = true
	}
	return nil
}

// ---- forge.StarProvider ----

// ListStarred implements forge.StarProvider.
func (p *Provider) ListStarred(ctx context.Context, user string) ([]forge.Repo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.begin(ctx, "ListStarred"); err != nil {
		return nil, err
	}
	out := []forge.Repo{}
	for _, r := range p.stars[strings.ToLower(user)] {
		out = append(out, cloneRepo(r))
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].FullName()) < strings.ToLower(out[j].FullName())
	})
	return out, nil
}

// ---- forge.ReleaseProvider ----

func (e *repoEntry) findRelease(id int64) int {
	for i := range e.releases {
		if e.releases[i].ID == id {
			return i
		}
	}
	return -1
}

// ListReleases implements forge.ReleaseProvider (newest first).
func (p *Provider) ListReleases(ctx context.Context, owner, name string) ([]forge.Release, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.begin(ctx, "ListReleases"); err != nil {
		return nil, err
	}
	e, ok := p.repos[key(owner, name)]
	if !ok {
		return nil, p.notFound(owner, name)
	}
	out := make([]forge.Release, 0, len(e.releases))
	for i := len(e.releases) - 1; i >= 0; i-- {
		out = append(out, cloneRelease(e.releases[i]))
	}
	return out, nil
}

// CreateRelease implements forge.ReleaseProvider. Input assets are ignored.
func (p *Provider) CreateRelease(ctx context.Context, owner, name string, r forge.Release) (*forge.Release, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.begin(ctx, "CreateRelease"); err != nil {
		return nil, err
	}
	e, ok := p.repos[key(owner, name)]
	if !ok {
		return nil, p.notFound(owner, name)
	}
	for _, x := range e.releases {
		if x.Tag == r.Tag {
			return nil, fmt.Errorf("fake: release with tag %q already exists", r.Tag)
		}
	}
	p.nextRelID++
	r.ID = p.nextRelID
	r.Assets = nil
	e.releases = append(e.releases, r)
	out := cloneRelease(r)
	return &out, nil
}

// UpdateRelease implements forge.ReleaseProvider. Assets are preserved.
func (p *Provider) UpdateRelease(ctx context.Context, owner, name string, r forge.Release) (*forge.Release, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.begin(ctx, "UpdateRelease"); err != nil {
		return nil, err
	}
	e, ok := p.repos[key(owner, name)]
	if !ok {
		return nil, p.notFound(owner, name)
	}
	i := e.findRelease(r.ID)
	if i < 0 {
		return nil, fmt.Errorf("fake: release %d: %w", r.ID, forge.ErrNotFound)
	}
	for j, x := range e.releases {
		if j != i && x.Tag == r.Tag {
			return nil, fmt.Errorf("fake: release with tag %q already exists", r.Tag)
		}
	}
	cur := &e.releases[i]
	cur.Tag, cur.Name, cur.Body, cur.Target = r.Tag, r.Name, r.Body, r.Target
	cur.Draft, cur.Prerelease = r.Draft, r.Prerelease
	out := cloneRelease(*cur)
	return &out, nil
}

// DeleteRelease implements forge.ReleaseProvider.
func (p *Provider) DeleteRelease(ctx context.Context, owner, name string, id int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.begin(ctx, "DeleteRelease"); err != nil {
		return err
	}
	e, ok := p.repos[key(owner, name)]
	if !ok {
		return p.notFound(owner, name)
	}
	i := e.findRelease(id)
	if i < 0 {
		return fmt.Errorf("fake: release %d: %w", id, forge.ErrNotFound)
	}
	for _, a := range e.releases[i].Assets {
		delete(p.assetData, a.ID)
	}
	e.releases = append(e.releases[:i], e.releases[i+1:]...)
	return nil
}

// DownloadAsset implements forge.ReleaseProvider.
func (p *Provider) DownloadAsset(ctx context.Context, owner, name string, a forge.Asset) (io.ReadCloser, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.begin(ctx, "DownloadAsset"); err != nil {
		return nil, err
	}
	e, ok := p.repos[key(owner, name)]
	if !ok {
		return nil, p.notFound(owner, name)
	}
	for _, rel := range e.releases {
		for _, x := range rel.Assets {
			if x.ID == a.ID {
				data := append([]byte(nil), p.assetData[a.ID]...)
				return io.NopCloser(strings.NewReader(string(data))), nil
			}
		}
	}
	return nil, fmt.Errorf("fake: asset %d: %w", a.ID, forge.ErrNotFound)
}

// UploadAsset implements forge.ReleaseProvider.
func (p *Provider) UploadAsset(ctx context.Context, owner, name string, releaseID int64, a forge.Asset, body io.Reader) (*forge.Asset, error) {
	p.mu.Lock()
	err := p.begin(ctx, "UploadAsset")
	p.mu.Unlock()
	if err != nil {
		return nil, err
	}
	var data []byte
	if body != nil {
		data, err = io.ReadAll(body)
		if err != nil {
			return nil, fmt.Errorf("fake: reading asset body: %w", err)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.repos[key(owner, name)]
	if !ok {
		return nil, p.notFound(owner, name)
	}
	i := e.findRelease(releaseID)
	if i < 0 {
		return nil, fmt.Errorf("fake: release %d: %w", releaseID, forge.ErrNotFound)
	}
	for _, x := range e.releases[i].Assets {
		if x.Name == a.Name {
			return nil, fmt.Errorf("fake: asset %q already exists on release %d", a.Name, releaseID)
		}
	}
	p.nextAssetID++
	a.ID = p.nextAssetID
	a.Size = int64(len(data))
	p.assetData[a.ID] = data
	e.releases[i].Assets = append(e.releases[i].Assets, a)
	return &a, nil
}

// DeleteAsset implements forge.ReleaseProvider.
func (p *Provider) DeleteAsset(ctx context.Context, owner, name string, releaseID, assetID int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.begin(ctx, "DeleteAsset"); err != nil {
		return err
	}
	e, ok := p.repos[key(owner, name)]
	if !ok {
		return p.notFound(owner, name)
	}
	i := e.findRelease(releaseID)
	if i < 0 {
		return fmt.Errorf("fake: release %d: %w", releaseID, forge.ErrNotFound)
	}
	rel := &e.releases[i]
	for j, x := range rel.Assets {
		if x.ID == assetID {
			rel.Assets = append(rel.Assets[:j], rel.Assets[j+1:]...)
			delete(p.assetData, assetID)
			return nil
		}
	}
	return fmt.Errorf("fake: asset %d: %w", assetID, forge.ErrNotFound)
}

// ---- forge.WebhookProvider ----

// Sign returns the X-Fake-Signature header value for body.
func Sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// ParseWebhook implements forge.WebhookProvider.
func (p *Provider) ParseWebhook(r *http.Request, secret string) (*forge.Event, error) {
	p.mu.Lock()
	err := p.begin(nil, "ParseWebhook")
	p.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if secret == "" {
		return nil, errors.New("fake: webhook secret must not be empty")
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("fake: reading webhook body: %w", err)
	}
	got := r.Header.Get(SignatureHeader)
	if got == "" || !hmac.Equal([]byte(got), []byte(Sign(secret, body))) {
		return nil, fmt.Errorf("fake: webhook: %w", forge.ErrBadSignature)
	}
	var payload struct {
		Kind  string `json:"kind"`
		Owner string `json:"owner"`
		Name  string `json:"name"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("fake: webhook payload: %w", err)
	}
	kind := forge.EventKind(payload.Kind)
	switch kind {
	case forge.EventPush, forge.EventRelease, forge.EventRepo, forge.EventWiki:
	default:
		kind = forge.EventOther
	}
	return &forge.Event{Kind: kind, Owner: payload.Owner, Name: payload.Name}, nil
}
