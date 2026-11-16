package web

import (
	"context"
	"crypto/rand"
	"embed"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gitmirror/internal/app"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// contentSecurityPolicy is sent with every response. It forbids inline
// scripts and styles, so templates must not use them.
const contentSecurityPolicy = "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; " +
	"frame-ancestors 'none'; form-action 'self'; base-uri 'none'"

// maxFormBody bounds the body of every state-changing request.
const maxFormBody = 1 << 20

// UI is the browser interface of gitmirror. Set the exported fields before
// calling Handler; they must not change afterwards.
type UI struct {
	App    *app.App
	Logger *slog.Logger
	// Clock returns the current time; nil means time.Now.
	Clock func() time.Time
	// BaseURL is the address printed in the first-run setup message, e.g.
	// "http://127.0.0.1:8090". server.external_url, when set, wins.
	BaseURL string
	// SyncNow, when set, queues a sync of one mirror (rule) right away. The
	// "Sync now" buttons are hidden while it is nil (no daemon).
	SyncNow func(rule string) error
	// SyncAll, when set, queues a sync of every mirror right away.
	SyncAll func() error

	once     sync.Once
	handler  http.Handler
	pages    map[string]*template.Template
	flashKey []byte
	login    *limiter
	setup    *limiter
	hashSem  chan struct{} // bounds concurrent password hashing (CPU heavy)

	hasAdmin  atomic.Bool
	setupMu   sync.Mutex // guards setupCode and serialises setup
	setupCode string     // normalised; "" when none is pending
	lastPrune atomic.Int64
}

func (u *UI) now() time.Time {
	if u.Clock == nil {
		return time.Now()
	}
	return u.Clock()
}

func (u *UI) log() *slog.Logger {
	if u.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return u.Logger
}

// Handler returns the web UI. The first call loads the templates and, while
// no admin password exists, generates the one-time setup code and logs it
// at WARN ("web UI setup: open <url>/setup and enter setup code ...").
// Call it once the UI listener is bound so the message is accurate.
func (u *UI) Handler() http.Handler {
	u.once.Do(u.init)
	return u.handler
}

func (u *UI) init() {
	u.pages = mustParsePages()
	u.flashKey = make([]byte, 32)
	if _, err := rand.Read(u.flashKey); err != nil {
		panic(err)
	}
	u.login = newLimiter(u.now)
	u.setup = newLimiter(u.now)
	u.hashSem = make(chan struct{}, 2)
	if _, err := u.adminExists(context.Background()); err != nil {
		u.log().Error("web UI: read admin password", "err", err)
	}

	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	files := http.StripPrefix("/static/", http.FileServerFS(static))

	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=300")
		files.ServeHTTP(w, r)
	}))
	u.routes(mux)
	u.handler = u.secure(u.withSession(u.setupGate(u.checkCSRF(mux))))
}

// routes registers every page. Add new pages here.
func (u *UI) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /setup", u.setupPage)
	mux.HandleFunc("POST /setup", u.setupSubmit)
	mux.HandleFunc("GET /login", u.loginPage)
	mux.HandleFunc("POST /login", u.loginSubmit)
	mux.HandleFunc("POST /logout", u.logout)

	mux.Handle("GET /{$}", u.requireAuth(u.dashboard))
	mux.Handle("GET /pair", u.requireAuth(u.pairPage))
	mux.Handle("POST /sync", u.requireAuth(u.syncAll))

	mux.Handle("GET /sites", u.requireAuth(u.sitesPage))
	mux.Handle("GET /sites/new", u.requireAuth(u.newSitePage))
	mux.Handle("POST /sites/new", u.requireAuth(u.addSite))
	mux.Handle("GET /sites/{name}/edit", u.requireAuth(u.editSitePage))
	mux.Handle("POST /sites/{name}/edit", u.requireAuth(u.editSite))
	mux.Handle("GET /sites/{name}/delete", u.requireAuth(u.deleteSitePage))
	mux.Handle("POST /sites/{name}/delete", u.requireAuth(u.deleteSite))

	mux.Handle("GET /mirrors", u.requireAuth(u.mirrorsPage))
	mux.Handle("GET /mirrors/new", u.requireAuth(u.newMirrorPage))
	mux.Handle("POST /mirrors/wizard", u.requireAuth(u.wizardSubmit))
	mux.Handle("GET /mirrors/{name}/edit", u.requireAuth(u.editMirrorPage))
	mux.Handle("GET /mirrors/{name}/delete", u.requireAuth(u.deleteMirrorPage))
	mux.Handle("POST /mirrors/{name}/delete", u.requireAuth(u.deleteMirror))
	mux.Handle("POST /mirrors/{name}/sync", u.requireAuth(u.syncMirror))

	mux.Handle("GET /webhooks", u.requireAuth(u.webhooksPage))
	mux.Handle("POST /webhooks/address", u.requireAuth(u.setPublicAddress))
	mux.Handle("GET /webhooks/secret/new", u.requireAuth(u.newSecretPage))
	mux.Handle("POST /webhooks/secret/new", u.requireAuth(u.newSecret))
	mux.Handle("POST /webhooks/{site}/setup", u.requireAuth(u.setupHooks))

	mux.Handle("GET /settings", u.requireAuth(u.settingsPage))
	mux.Handle("POST /settings/password", u.requireAuth(u.changePassword))

	mux.Handle("/", u.requireAuth(u.notFound))
}

// secure sets the security headers on every response.
func (u *UI) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// setupGate sends every request to /setup while no admin password exists.
func (u *UI) setupGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/setup" || strings.HasPrefix(r.URL.Path, "/static/") {
			next.ServeHTTP(w, r)
			return
		}
		ok, err := u.adminExists(r.Context())
		if err != nil {
			u.serverError(w, r, err)
			return
		}
		if !ok {
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// adminExists reports whether the admin password is set. While it is not,
// it makes sure a setup code is pending (and logged).
func (u *UI) adminExists(ctx context.Context) (bool, error) {
	if u.hasAdmin.Load() {
		return true, nil
	}
	_, ok, err := u.App.Store.Setting(ctx, adminPasswordKey)
	if err != nil {
		return false, err
	}
	if ok {
		u.hasAdmin.Store(true)
		return true, nil
	}
	u.setupMu.Lock()
	defer u.setupMu.Unlock()
	if u.hasAdmin.Load() { // setup finished meanwhile
		return true, nil
	}
	if u.setupCode == "" {
		code, err := newSetupCode()
		if err != nil {
			return false, err
		}
		u.setupCode = normaliseSetupCode(code)
		u.log().Warn("web UI setup: open " + u.baseURL() + "/setup and enter setup code " + code)
	}
	return false, nil
}

// baseURL is external_url, else BaseURL, without a trailing slash.
func (u *UI) baseURL() string {
	if eu := u.externalURL(); eu != "" {
		return eu
	}
	if u.BaseURL != "" {
		return strings.TrimRight(u.BaseURL, "/")
	}
	return "http://localhost"
}

func (u *UI) externalURL() string {
	if u.App == nil {
		return ""
	}
	return u.App.Config().Server.ExternalURL
}

// secureCookies reports whether cookies must be marked Secure.
func (u *UI) secureCookies(r *http.Request) bool {
	return r.TLS != nil || strings.HasPrefix(strings.ToLower(u.externalURL()), "https://")
}

// serverError logs err and shows a generic error page.
func (u *UI) serverError(w http.ResponseWriter, r *http.Request, err error) {
	u.log().Error("web UI: request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	u.render(w, r, "message", View{
		Title:  "Something went wrong",
		Status: http.StatusInternalServerError,
		Data:   "gitmirror could not finish this request. Please try again. If it keeps happening, check the gitmirror log.",
	})
}

// redirect answers a form submission with 303 See Other.
func redirect(w http.ResponseWriter, r *http.Request, path string) {
	http.Redirect(w, r, path, http.StatusSeeOther)
}
