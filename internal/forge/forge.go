// Package forge defines the contract every hosting platform implements.
//
// Provider is the required surface. The optional interfaces below are
// discovered with type assertions, so a platform implements only what it
// supports and the engine skips the steps a provider lacks.
package forge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"
)

// ErrNotFound is returned (wrapped) when a repo, release or owner is absent.
var ErrNotFound = errors.New("forge: not found")

// ErrUnsupported is returned (wrapped) when a provider cannot do an operation.
var ErrUnsupported = errors.New("forge: unsupported")

// Repo is the platform-neutral view of a repository.
type Repo struct {
	// ID is the forge's numeric repository id; it changes when a repository
	// is deleted and re-created under the same name. 0 means unknown.
	ID            int64
	Owner         string
	Name          string
	Description   string
	Homepage      string
	Topics        []string
	DefaultBranch string
	Private       bool
	Archived      bool
	Fork          bool
	Empty         bool
	HasWiki       bool
}

// FullName returns "owner/name".
func (r Repo) FullName() string { return r.Owner + "/" + r.Name }

// CreateOptions describes a repository to create.
type CreateOptions struct {
	Owner       string
	Name        string
	Description string
	Private     bool
}

// Settings is a partial update; nil fields are left untouched.
type Settings struct {
	Description   *string
	Homepage      *string
	Topics        *[]string
	DefaultBranch *string
	Private       *bool
	Archived      *bool
	// Actions says whether the repository's workflows may run. false keeps
	// the files but stops the site from running them; it needs admin rights
	// on the destination. nil leaves the setting alone.
	Actions *bool
}

// Auth carries HTTP basic credentials for git transport.
type Auth struct {
	Username string
	Token    string
}

// Rate is the API quota snapshot. A zero Limit means "unknown/unlimited".
type Rate struct {
	Limit     int
	Remaining int
	Reset     time.Time
}

// Provider is the required surface of a hosting platform.
type Provider interface {
	// Type is the registry name, e.g. "github".
	Type() string
	GetRepo(ctx context.Context, owner, name string) (*Repo, error)
	// ListRepos lists every repo owned by a user or organisation.
	ListRepos(ctx context.Context, owner string) ([]Repo, error)
	CreateRepo(ctx context.Context, opts CreateOptions) (*Repo, error)
	UpdateRepoSettings(ctx context.Context, owner, name string, s Settings) error
	// GitURL is the HTTPS clone URL, without credentials.
	GitURL(owner, name string) string
	GitAuth() Auth
	RateLimit(ctx context.Context) (Rate, error)
}

// Release is a tagged release and its downloadable assets.
type Release struct {
	ID         int64
	Tag        string
	Name       string
	Body       string
	Target     string
	Draft      bool
	Prerelease bool
	Assets     []Asset
}

// Asset is one file attached to a release.
type Asset struct {
	ID          int64
	Name        string
	Size        int64
	ContentType string
}

// ReleaseProvider is implemented by platforms with releases.
type ReleaseProvider interface {
	ListReleases(ctx context.Context, owner, name string) ([]Release, error)
	CreateRelease(ctx context.Context, owner, name string, r Release) (*Release, error)
	UpdateRelease(ctx context.Context, owner, name string, r Release) (*Release, error)
	DeleteRelease(ctx context.Context, owner, name string, id int64) error
	DownloadAsset(ctx context.Context, owner, name string, a Asset) (io.ReadCloser, error)
	UploadAsset(ctx context.Context, owner, name string, releaseID int64, a Asset, body io.Reader) (*Asset, error)
	DeleteAsset(ctx context.Context, owner, name string, releaseID, assetID int64) error
}

// WikiProvider is implemented by platforms whose wiki is a git repository.
type WikiProvider interface {
	// WikiGitURL is the HTTPS clone URL of the wiki, without credentials.
	WikiGitURL(owner, name string) string
}

// WikiInitializer is implemented by platforms that refuse git access to a
// wiki until it has been created through the web UI or API (Gitea, Forgejo).
// EnsureWiki is a no-op when the wiki already exists.
type WikiInitializer interface {
	EnsureWiki(ctx context.Context, owner, name string) error
}

// StarProvider lists the repositories a user has starred.
type StarProvider interface {
	ListStarred(ctx context.Context, user string) ([]Repo, error)
}

// OrgProvider can create an organisation/namespace on demand.
type OrgProvider interface {
	EnsureOrg(ctx context.Context, name string) error
}

// Identity reports the account the provider's token authenticates as. The
// validate command uses it to check credentials.
type Identity interface {
	Login(ctx context.Context) (string, error)
}

// RepoDeleter can permanently delete a repository. The engine only calls it
// for destinations it created, and only when a rule opts into deletion.
type RepoDeleter interface {
	DeleteRepo(ctx context.Context, owner, name string) error
}

// Hook is a webhook registered on a forge, as listed by HookProvider.
type Hook struct {
	ID          int64
	URL         string
	Events      []string
	Active      bool
	ContentType string
}

// HookOptions describes a webhook to create. The payload content type is
// always JSON. The secret is never returned by the forge when listing.
type HookOptions struct {
	URL    string
	Secret string
	Events []string
}

// HookProvider manages webhooks on the forge (the opposite direction from
// WebhookProvider, which receives them). Hooks can be set on one repository
// or on an organisation; a user account has no account-wide hooks on either
// GitHub or Gitea/Forgejo, so callers must hook each repository of a user.
// Failures never include HookOptions.Secret in their text.
type HookProvider interface {
	RepoHooks(ctx context.Context, owner, repo string) ([]Hook, error)
	CreateRepoHook(ctx context.Context, owner, repo string, o HookOptions) (Hook, error)
	OrgHooks(ctx context.Context, org string) ([]Hook, error)
	CreateOrgHook(ctx context.Context, org string, o HookOptions) (Hook, error)
	// HookEvents lists the event names gitmirror needs on this forge, in the
	// forge's own vocabulary; pass them as HookOptions.Events.
	HookEvents() []string
}

// EventKind classifies a webhook delivery.
type EventKind string

const (
	EventPush    EventKind = "push"
	EventRelease EventKind = "release"
	EventRepo    EventKind = "repository"
	EventWiki    EventKind = "wiki"
	EventOther   EventKind = "other"
)

// Event is a verified, parsed webhook delivery.
type Event struct {
	Kind  EventKind
	Owner string
	Name  string
}

// ErrBadSignature is returned when a webhook signature does not verify.
var ErrBadSignature = errors.New("forge: bad webhook signature")

// WebhookProvider verifies and parses incoming webhook requests.
type WebhookProvider interface {
	// ParseWebhook reads the body (already size-limited by the caller),
	// verifies it against secret and returns the event. An empty secret
	// must be rejected.
	ParseWebhook(r *http.Request, secret string) (*Event, error)
}

// Options configures a provider instance.
type Options struct {
	// URL is the base URL; empty means the platform default where one exists.
	URL string
	// Username is used for git transport; providers supply a default.
	Username string
	Token    string
	// HTTPClient is optional; nil means a sensible default with timeouts.
	HTTPClient *http.Client
}

// Factory builds a provider from options.
type Factory func(Options) (Provider, error)

var (
	regMu    sync.RWMutex
	registry = map[string]Factory{}
)

// Register adds a provider type. It panics on a duplicate, like database/sql.
func Register(typ string, f Factory) {
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := registry[typ]; dup {
		panic("forge: Register called twice for " + typ)
	}
	registry[typ] = f
}

// New builds a provider of the given registered type.
func New(typ string, o Options) (Provider, error) {
	regMu.RLock()
	f, ok := registry[typ]
	regMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("forge: unknown type %q (known: %v)", typ, Types())
	}
	return f(o)
}

// Types lists registered provider types, sorted.
func Types() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]string, 0, len(registry))
	for t := range registry {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
