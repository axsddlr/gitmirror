// Package web is gitmirror's browser interface (server.ui_listen). It is
// built for non-technical users: plain words ("Sites" for forges, "Mirrors"
// for rules), one admin password, no JavaScript framework, no CDN. Templates
// (templates/*.html, html/template) and static files (static/) are embedded.
//
// # Wiring
//
// serve builds a UI{App, Logger, BaseURL} once the UI listener is bound and
// hands UI.Handler() to server.Server.UIHandler; the server serves it on
// Server.UIListener and shuts it down with the rest.
//
// UI.SyncNow and UI.SyncAll (serve wires them to server.Server.SyncNow /
// SyncAll) queue syncs from the "Sync now" buttons; while nil the buttons
// are hidden.
//
// # Pages
//
//   - / (dashboard.go): mirrors with the stored state of their pairs;
//     /pair?k=<id> shows one pair's recent runs and steps.
//   - /sites (sites.go): forges. Adding or changing one tests the token
//     (App.CheckForge) before saving; tokens are never rendered.
//   - /mirrors (dashboard.go, wizard.go): rules, and the step-by-step
//     mirror form that previews with App.Preview before Save. The preview
//     only plans (forge APIs and the state store, no git fetch, no cache
//     lock), at most app.PreviewLimit repositories, within 45 seconds.
//   - /webhooks (webhooks.go): public address and webhook secret (stored by
//     the App when the config file has none) and "Set up webhooks for me":
//     one organisation webhook for a mirror that copies a whole
//     organisation, one per repository otherwise (picked lists, single
//     repositories, personal accounts). The run is bounded to 45 seconds,
//     below the UI server's 60s write timeout; whatever was not reached is
//     shown as "Not done yet — run Set up again".
//   - Problems are put in plain words by explain (plain.go).
//
// Entries of the config file are read-only: their pages say so and every
// POST that would change one is refused (403), whatever the form says.
//
// The setup code is generated on the first Handler call (and again on any
// later request if no admin exists and no code is pending): while the
// settings key "admin_password" is unset, Handler logs at WARN
// "web UI setup: open <url>/setup and enter setup code XXXX-XXXX-XXXX" and
// every page except /setup and /static/ redirects to /setup. The code lives
// only in memory and is cleared once setup succeeds.
//
// # Security model
//
//   - Password: PBKDF2-SHA256, 600000 iterations, 16-byte random salt,
//     stored as "pbkdf2-sha256$<iter>$<salt>$<key>" (see password.go).
//   - Sessions: random 32-byte token in cookie "gitmirror_session"
//     (HttpOnly, SameSite=Strict, Path=/, Secure on TLS or an https
//     external_url); only its SHA-256 is stored (state table ui_sessions).
//     Idle timeout 24h, absolute 7 days.
//   - CSRF: checkCSRF guards every request that is not GET or HEAD. It
//     rejects a foreign Origin, then requires the field "csrf" (or header
//     X-CSRF-Token) to equal the session's token; signed-out POSTs are only
//     accepted on publicForms (/login, /setup), against the pre-session
//     cookie "gitmirror_csrf" (double submit). Other signed-out POSTs are
//     redirected to /login.
//   - Sign-in, setup and password-change failures are rate limited per
//     RemoteAddr IP (5 per 15 minutes) and globally (100 per 15 minutes).
//     An attempt is reserved in the limiter before the password is checked
//     and counts as a failure until it succeeds, so parallel requests get
//     no more tries; a success clears that IP's failures. Tradeoff of the
//     global cap: it stops guessing spread over many addresses, but about
//     20 addresses failing 5 times each can lock everyone (the admin too)
//     out for 15 minutes. One IP can never use more than its own 5.
//   - Every response carries a strict Content-Security-Policy: no inline
//     scripts or styles anywhere, no style="" attributes. Put CSS in
//     static/app.css.
//
// # Adding a page
//
// A page is a handler plus a template file.
//
//  1. Create templates/<name>.html defining "content". The root data has the
//     View fields (.Title, .Form, .Data) plus .CSRF, .SignedIn, .Flashes.
//  2. Write a handler on *UI that calls u.render(w, r, "<name>", View{...}).
//  3. Register it in UI.routes, wrapped in u.requireAuth unless it must be
//     reachable while signed out (then also list a POST path in
//     publicForms).
//
// Example, a page listing sites with an add form:
//
//	// templates/sites.html
//	{{define "content"}}<h1>{{.Title}}</h1>
//	<form method="post" action="/sites">{{template "csrf" .}}{{template "formerror" .Form}}
//	  {{template "field" (field .Form "name" "Name" "text" "off" "A short name, like work-github.")}}
//	  <button class="btn btn-primary" type="submit">Add site</button></form>{{end}}
//
//	// sites.go
//	func (u *UI) sitesPage(w http.ResponseWriter, r *http.Request) {
//		u.render(w, r, "sites", View{Title: "Sites", Section: "sites", Form: newForm(r)})
//	}
//	func (u *UI) addSite(w http.ResponseWriter, r *http.Request) {
//		f := newForm(r)
//		if f.Get("name") == "" {
//			f.SetError("name", "Please give the site a name.")
//		}
//		if !f.OK() {
//			u.render(w, r, "sites", View{Title: "Sites", Section: "sites", Form: f, Status: http.StatusBadRequest})
//			return
//		}
//		// ... save via u.App.UpdateUI(r.Context(), func(c *app.UIConfig) error { ... })
//		u.setFlash(w, r, FlashSuccess, "Site added.")
//		redirect(w, r, "/sites")
//	}
//
//	// in routes:
//	mux.Handle("GET /sites", u.requireAuth(u.sitesPage))
//	mux.Handle("POST /sites", u.requireAuth(u.addSite))
//
// # Helpers
//
//   - u.render(w, r, name, View): page inside the layout (header, nav,
//     flash, sign-out). View.Section highlights the nav item; View.Status
//     sets the HTTP status (0 = 200). Renders to a buffer first.
//   - u.setFlash(w, r, kind, msg): one-time message on the next page
//     (FlashSuccess, FlashInfo, FlashError). Use before redirect.
//   - redirect(w, r, path): 303 See Other, for after a successful POST.
//   - u.requireAuth(handler): signed-out GETs go to /login?next=<path>.
//   - sessionFrom(r): the current *session (nil when signed out).
//   - newForm(r), Form.Get/SetError/Fail/OK: the form-error pattern. Show
//     the form again with the user's values and messages on error; never
//     echo passwords (the "field" template drops password values).
//   - Templates: {{template "csrf" $}} (hidden CSRF field; pass the root),
//     {{template "formerror" .Form}} (message above the form) and
//     {{template "field" (field .Form name label type autocomplete [hint])}}
//     (label, hint, error with aria-describedby / aria-invalid).
//   - u.serverError(w, r, err): logs err and shows a friendly 500 page.
//   - u.now(): the injectable clock (UI.Clock); use it for anything timed.
//
// Never log passwords, session tokens, CSRF tokens or forge tokens.
package web
