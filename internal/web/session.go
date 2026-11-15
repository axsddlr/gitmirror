package web

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gitmirror/internal/state"
)

const (
	// adminPasswordKey is the settings key of the admin password hash.
	adminPasswordKey = "admin_password"

	sessionCookie = "gitmirror_session"
	csrfCookie    = "gitmirror_csrf" // pre-session (double-submit) token for /login and /setup
	flashCookie   = "gitmirror_flash"

	sessionIdle   = 24 * time.Hour
	sessionMaxAge = 7 * 24 * time.Hour
	touchEvery    = time.Minute // last_seen is written at most this often
	pruneEvery    = time.Hour   // expired sessions are deleted at most this often
	tokenBytes    = 32
)

// publicForms are the paths that accept POST without a session; they are
// protected by the pre-session CSRF cookie instead.
var publicForms = map[string]bool{"/login": true, "/setup": true}

var b64url = base64.RawURLEncoding

// session is the signed-in admin session of a request.
type session struct {
	idHash  []byte
	csrf    string
	created time.Time
}

type ctxKey int

const sessionKey ctxKey = 0

// sessionFrom returns the request's valid session, or nil when signed out.
func sessionFrom(r *http.Request) *session {
	s, _ := r.Context().Value(sessionKey).(*session)
	return s
}

func randomToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return b64url.EncodeToString(b), nil
}

func hashToken(tok string) []byte {
	h := sha256.Sum256([]byte(tok))
	return h[:]
}

func (u *UI) cookie(r *http.Request, name, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   u.secureCookies(r),
		SameSite: http.SameSiteStrictMode,
	}
}

// withSession looks up the session cookie and stores a valid session in the
// request context. Expired sessions are deleted.
func (u *UI) withSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.pruneSessions(r.Context())
		if s := u.lookupSession(r); s != nil {
			r = r.WithContext(context.WithValue(r.Context(), sessionKey, s))
		}
		next.ServeHTTP(w, r)
	})
}

func (u *UI) lookupSession(r *http.Request) *session {
	c, err := r.Cookie(sessionCookie)
	if err != nil || len(c.Value) != b64url.EncodedLen(tokenBytes) {
		return nil
	}
	ctx := r.Context()
	id := hashToken(c.Value)
	row, ok, err := u.App.Store.UISession(ctx, id)
	if err != nil {
		u.log().Error("web UI: read session", "err", err)
		return nil
	}
	if !ok {
		return nil
	}
	now := u.now()
	if now.Sub(row.LastSeen) > sessionIdle || now.Sub(row.Created) > sessionMaxAge {
		if err := u.App.Store.DeleteUISession(ctx, id); err != nil {
			u.log().Error("web UI: delete expired session", "err", err)
		}
		return nil
	}
	if now.Sub(row.LastSeen) >= touchEvery {
		if err := u.App.Store.TouchUISession(ctx, id, now); err != nil {
			u.log().Error("web UI: update session", "err", err)
		}
	}
	return &session{idHash: id, csrf: row.CSRF, created: row.Created}
}

// startSession creates a new session and sets its cookie.
func (u *UI) startSession(w http.ResponseWriter, r *http.Request) error {
	tok, err := randomToken()
	if err != nil {
		return err
	}
	csrf, err := randomToken()
	if err != nil {
		return err
	}
	now := u.now()
	if err := u.App.Store.CreateUISession(r.Context(), state.UISession{
		IDHash: hashToken(tok), CSRF: csrf, Created: now, LastSeen: now,
	}); err != nil {
		return err
	}
	http.SetCookie(w, u.cookie(r, sessionCookie, tok, int(sessionMaxAge/time.Second)))
	return nil
}

// endSession deletes the request's session (if any) and clears its cookie.
func (u *UI) endSession(w http.ResponseWriter, r *http.Request) error {
	http.SetCookie(w, u.cookie(r, sessionCookie, "", -1))
	if s := sessionFrom(r); s != nil {
		return u.App.Store.DeleteUISession(r.Context(), s.idHash)
	}
	return nil
}

func (u *UI) pruneSessions(ctx context.Context) {
	now := u.now()
	last := u.lastPrune.Load()
	if last != 0 && now.Sub(time.Unix(0, last)) < pruneEvery {
		return
	}
	if !u.lastPrune.CompareAndSwap(last, now.UnixNano()) {
		return
	}
	if n, err := u.App.Store.DeleteExpiredUISessions(ctx, now.Add(-sessionIdle), now.Add(-sessionMaxAge)); err != nil {
		u.log().Error("web UI: delete expired sessions", "err", err)
	} else if n > 0 {
		u.log().Debug("web UI: deleted expired sessions", "count", n)
	}
}

// requireAuth wraps a page that needs a signed-in admin. Signed-out
// visitors are sent to /login, which returns them to the page afterwards.
func (u *UI) requireAuth(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sessionFrom(r) == nil {
			target := "/login"
			if r.Method == http.MethodGet && r.URL.Path != "/" {
				target += "?next=" + url.QueryEscape(r.URL.RequestURI())
			}
			redirect(w, r, target)
			return
		}
		h(w, r)
	})
}

// safeNext returns next if it is a local path, else "/".
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	if u, err := url.Parse(next); err != nil || u.Host != "" || u.Scheme != "" {
		return "/"
	}
	return next
}

// checkCSRF rejects state-changing requests (anything but GET and HEAD)
// that come from another origin or lack the right CSRF token: the
// session's token when signed in, else (only on publicForms) the
// pre-session cookie. Other signed-out requests are sent to /login.
func (u *UI) checkCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		if o := r.Header.Get("Origin"); o != "" {
			ou, err := url.Parse(o)
			if err != nil || ou.Host == "" || !strings.EqualFold(ou.Host, r.Host) {
				u.forbidden(w, r)
				return
			}
		}
		var want string
		if s := sessionFrom(r); s != nil {
			want = s.csrf
		} else if publicForms[r.URL.Path] {
			if c, err := r.Cookie(csrfCookie); err == nil {
				want = c.Value
			}
		} else {
			redirect(w, r, "/login")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxFormBody)
		got := r.Header.Get("X-CSRF-Token")
		if got == "" {
			got = r.PostFormValue("csrf")
		}
		if want == "" || got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			u.forbidden(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (u *UI) forbidden(w http.ResponseWriter, r *http.Request) {
	u.log().Warn("web UI: rejected a form without a valid security token", "method", r.Method, "path", r.URL.Path, "ip", clientIP(r))
	u.render(w, r, "message", View{
		Title:  "This form has expired",
		Status: http.StatusForbidden,
		Data: "For your safety gitmirror did not accept this form. Go back, reload the page and try again. " +
			"If this keeps happening, make sure cookies are allowed for this site.",
	})
}

// csrfToken returns the token to embed in forms: the session's token, or
// the pre-session cookie (set here if missing) when signed out.
func (u *UI) csrfToken(w http.ResponseWriter, r *http.Request) string {
	if s := sessionFrom(r); s != nil {
		return s.csrf
	}
	if c, err := r.Cookie(csrfCookie); err == nil && len(c.Value) == b64url.EncodedLen(tokenBytes) {
		return c.Value
	}
	tok, err := randomToken()
	if err != nil {
		u.log().Error("web UI: random token", "err", err)
		return ""
	}
	http.SetCookie(w, u.cookie(r, csrfCookie, tok, 0))
	return tok
}

// Flash kinds.
const (
	FlashSuccess = "success"
	FlashInfo    = "info"
	FlashError   = "error"
)

// Flash is a one-time message shown on the next page.
type Flash struct {
	Kind    string
	Message string
}

// setFlash stores a one-time message for the next page the browser loads.
// It is kept in a cookie signed with a per-process key.
func (u *UI) setFlash(w http.ResponseWriter, r *http.Request, kind, msg string) {
	payload := b64url.EncodeToString([]byte(kind + "\x00" + msg))
	http.SetCookie(w, u.cookie(r, flashCookie, payload+"."+u.flashMAC(payload), 0))
}

func (u *UI) flashMAC(payload string) string {
	m := hmac.New(sha256.New, u.flashKey)
	m.Write([]byte(payload))
	return b64url.EncodeToString(m.Sum(nil))
}

// takeFlashes returns the pending flash message (if any) and clears it.
func (u *UI) takeFlashes(w http.ResponseWriter, r *http.Request) []Flash {
	c, err := r.Cookie(flashCookie)
	if err != nil {
		return nil
	}
	http.SetCookie(w, u.cookie(r, flashCookie, "", -1))
	payload, mac, ok := strings.Cut(c.Value, ".")
	if !ok || !hmac.Equal([]byte(mac), []byte(u.flashMAC(payload))) {
		return nil
	}
	b, err := b64url.DecodeString(payload)
	if err != nil {
		return nil
	}
	kind, msg, ok := strings.Cut(string(b), "\x00")
	if !ok {
		return nil
	}
	switch kind {
	case FlashSuccess, FlashInfo, FlashError:
	default:
		kind = FlashInfo
	}
	return []Flash{{Kind: kind, Message: msg}}
}
