package web

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
)

// hash and check run the CPU-heavy password functions, at most two at a time.
func (u *UI) hash(ctx context.Context, pw string) (string, error) {
	if err := u.acquireHash(ctx); err != nil {
		return "", err
	}
	defer func() { <-u.hashSem }()
	return hashPassword(pw)
}

func (u *UI) check(ctx context.Context, pw string) (bool, error) {
	enc, ok, err := u.App.Store.Setting(ctx, adminPasswordKey)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, errors.New("no admin password is set")
	}
	if err := u.acquireHash(ctx); err != nil {
		return false, err
	}
	defer func() { <-u.hashSem }()
	return checkPassword(string(enc), pw)
}

func (u *UI) acquireHash(ctx context.Context) error {
	select {
	case u.hashSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// tooMany answers a rate-limited attempt with 429 on the given page.
func (u *UI) tooMany(w http.ResponseWriter, r *http.Request, page string, v View) {
	u.log().Warn("web UI: too many failed attempts", "path", r.URL.Path, "ip", clientIP(r))
	w.Header().Set("Retry-After", fmt.Sprint(int(failureWindow.Seconds())))
	v.Form.Fail(fmt.Sprintf("Too many wrong attempts. For your safety, please wait %d minutes and try again.", int(failureWindow.Minutes())))
	v.Status = http.StatusTooManyRequests
	u.render(w, r, page, v)
}

// ---- first-run setup ----

func setupView(f *Form) View { return View{Title: "Welcome to gitmirror", Form: f} }

func (u *UI) setupPage(w http.ResponseWriter, r *http.Request) {
	ok, err := u.adminExists(r.Context())
	if err != nil {
		u.serverError(w, r, err)
		return
	}
	if ok {
		redirect(w, r, "/login")
		return
	}
	u.render(w, r, "setup", setupView(newForm(r)))
}

func (u *UI) setupSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ok, err := u.adminExists(ctx)
	if err != nil {
		u.serverError(w, r, err)
		return
	}
	if ok {
		redirect(w, r, "/login")
		return
	}
	f := newForm(r)
	ip := clientIP(r)
	if !u.setup.allow(ip) {
		u.tooMany(w, r, "setup", setupView(f))
		return
	}
	u.setupMu.Lock()
	defer u.setupMu.Unlock()
	if u.hasAdmin.Load() {
		redirect(w, r, "/login")
		return
	}
	code := normaliseSetupCode(f.Get("code"))
	if u.setupCode == "" || subtle.ConstantTimeCompare([]byte(code), []byte(u.setupCode)) != 1 {
		u.setup.fail(ip)
		u.log().Warn("web UI: wrong setup code", "ip", ip)
		f.SetError("code", "That setup code is not right. Look for the line starting with \"web UI setup\" in the gitmirror log and type the code again.")
		v := setupView(f)
		v.Status = http.StatusUnauthorized
		u.render(w, r, "setup", v)
		return
	}
	if name, msg := checkPasswordRules(f.Get("password"), f.Get("confirm")); msg != "" {
		f.SetError(name, msg)
		v := setupView(f)
		v.Status = http.StatusBadRequest
		u.render(w, r, "setup", v)
		return
	}
	enc, err := u.hash(ctx, f.Get("password"))
	if err != nil {
		u.serverError(w, r, err)
		return
	}
	if err := u.App.Store.PutSetting(ctx, adminPasswordKey, []byte(enc)); err != nil {
		u.serverError(w, r, err)
		return
	}
	u.hasAdmin.Store(true)
	u.setupCode = ""
	u.setup.succeed(ip)
	u.log().Info("web UI: admin password created", "ip", ip)
	if err := u.startSession(w, r); err != nil {
		u.serverError(w, r, err)
		return
	}
	u.setFlash(w, r, FlashSuccess, "Welcome! Your admin password is set and you are signed in.")
	redirect(w, r, "/")
}

// ---- sign in and out ----

func loginView(f *Form, next string) View { return View{Title: "Sign in", Form: f, Data: next} }

func (u *UI) loginPage(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.URL.Query().Get("next"))
	if sessionFrom(r) != nil {
		redirect(w, r, next)
		return
	}
	if next == "/" {
		next = ""
	}
	u.render(w, r, "login", loginView(newForm(r), next))
}

func (u *UI) loginSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	next := safeNext(r.URL.Query().Get("next"))
	if sessionFrom(r) != nil {
		redirect(w, r, next)
		return
	}
	f := newForm(r)
	formNext := next
	if formNext == "/" {
		formNext = ""
	}
	ip := clientIP(r)
	if !u.login.allow(ip) {
		u.tooMany(w, r, "login", loginView(f, formNext))
		return
	}
	match, err := u.check(ctx, f.Get("password"))
	if err != nil {
		u.serverError(w, r, err)
		return
	}
	if !match {
		u.login.fail(ip)
		u.log().Warn("web UI: failed sign-in", "ip", ip)
		f.SetError("password", "That password is not right. Please try again.")
		v := loginView(f, formNext)
		v.Status = http.StatusUnauthorized
		u.render(w, r, "login", v)
		return
	}
	u.login.succeed(ip)
	if err := u.startSession(w, r); err != nil {
		u.serverError(w, r, err)
		return
	}
	u.log().Info("web UI: signed in", "ip", ip)
	redirect(w, r, next)
}

func (u *UI) logout(w http.ResponseWriter, r *http.Request) {
	if err := u.endSession(w, r); err != nil {
		u.serverError(w, r, err)
		return
	}
	u.setFlash(w, r, FlashInfo, "You are signed out.")
	redirect(w, r, "/login")
}

// ---- pages ----

func (u *UI) dashboard(w http.ResponseWriter, r *http.Request) {
	u.render(w, r, "dashboard", View{Title: "Dashboard", Section: "dashboard", Data: len(u.App.Config().Rules)})
}

// comingSoon is the placeholder of a page that a later version fills in.
func (u *UI) comingSoon(section, title, text string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u.render(w, r, "soon", View{Title: title, Section: section, Data: text})
	}
}

func (u *UI) notFound(w http.ResponseWriter, r *http.Request) {
	u.render(w, r, "message", View{
		Title:  "Page not found",
		Status: http.StatusNotFound,
		Data:   "There is no page at this address. Use the menu above to find your way.",
	})
}

func settingsView(f *Form) View { return View{Title: "Settings", Section: "settings", Form: f} }

func (u *UI) settingsPage(w http.ResponseWriter, r *http.Request) {
	u.render(w, r, "settings", settingsView(newForm(r)))
}

// changePassword sets a new admin password and signs out every other
// session.
func (u *UI) changePassword(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	f := newForm(r)
	ip := clientIP(r)
	if !u.login.allow(ip) {
		u.tooMany(w, r, "settings", settingsView(f))
		return
	}
	match, err := u.check(ctx, f.Get("current"))
	if err != nil {
		u.serverError(w, r, err)
		return
	}
	if !match {
		u.login.fail(ip)
		u.log().Warn("web UI: wrong current password on password change", "ip", ip)
		f.SetError("current", "That is not your current password.")
		v := settingsView(f)
		v.Status = http.StatusUnauthorized
		u.render(w, r, "settings", v)
		return
	}
	if name, msg := checkPasswordRules(f.Get("password"), f.Get("confirm")); msg != "" {
		f.SetError(name, msg)
		v := settingsView(f)
		v.Status = http.StatusBadRequest
		u.render(w, r, "settings", v)
		return
	}
	enc, err := u.hash(ctx, f.Get("password"))
	if err != nil {
		u.serverError(w, r, err)
		return
	}
	if err := u.App.Store.PutSetting(ctx, adminPasswordKey, []byte(enc)); err != nil {
		u.serverError(w, r, err)
		return
	}
	if err := u.App.Store.DeleteOtherUISessions(ctx, sessionFrom(r).idHash); err != nil {
		u.serverError(w, r, err)
		return
	}
	u.login.succeed(ip)
	u.log().Info("web UI: admin password changed; other sessions signed out", "ip", ip)
	u.setFlash(w, r, FlashSuccess, "Password changed. Any other browsers that were signed in have been signed out.")
	redirect(w, r, "/settings")
}
