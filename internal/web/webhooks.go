package web

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"gitmirror/internal/app"
	"gitmirror/internal/config"
	"gitmirror/internal/forge"
	"gitmirror/internal/resolve"
)

// hookTimeout bounds one "Set up webhooks for me" run.
const hookTimeout = 2 * time.Minute

// hookPermissionHint explains a 403 from a webhook API call.
const hookPermissionHint = "The token is not allowed to manage webhooks here. On GitHub it needs the admin:repo_hook scope " +
	"(admin:org_hook for organisations); on Gitea or Forgejo write:repository (write:organization for organisations). " +
	"The account must also be an admin of the repository or organisation."

type hookSite struct {
	Name, Kind, PayloadURL string
	Events                 []string
	Mirrors                []string
	CanSetup               bool
	Why                    string
}

type webhooksData struct {
	Address         string
	AddressFromFile bool
	Secret          string
	SecretFromFile  bool
	Listen          string
	Sites           []hookSite
}

func payloadURL(public, site string) string { return public + "/webhook/" + site }

func (u *UI) webhooksView(ctx context.Context, f *Form, status int) (View, error) {
	if _, err := u.App.EnsureWebhookSecret(ctx); err != nil {
		return View{}, err
	}
	snap := u.App.Snapshot()
	cfg := snap.Config
	addr, addrFile := u.App.PublicAddress()
	sec, secFile := u.App.WebhookSecret()
	d := webhooksData{Address: addr, AddressFromFile: addrFile, SecretFromFile: secFile, Listen: cfg.Server.Listen}
	if !secFile {
		d.Secret = sec
	}
	if f.Get("address") == "" && f.Errors["address"] == "" {
		f.Values.Set("address", addr)
	}
	users := map[string][]string{}
	for _, r := range cfg.Rules {
		users[r.From.Forge] = append(users[r.From.Forge], r.Name)
	}
	for _, name := range sortedNames(users) {
		hs := hookSite{Name: name, Kind: kindLabel(cfg.Forges[name].Type), Mirrors: users[name]}
		hp, canHook := snap.Providers[name].(forge.HookProvider)
		_, canReceive := snap.Providers[name].(forge.WebhookProvider)
		if canHook {
			hs.Events = hp.HookEvents()
		}
		if addr != "" {
			hs.PayloadURL = payloadURL(addr, name)
		}
		switch {
		case !canReceive:
			hs.Why = "gitmirror cannot receive webhooks from this kind of site."
		case addr == "":
			hs.Why = "Add a public address first (above)."
		case sec == "":
			hs.Why = "Create a webhook secret first (above)."
		case !canHook:
			hs.Why = "gitmirror cannot set up webhooks on this kind of site; add them by hand with the details shown."
		default:
			hs.CanSetup = true
		}
		d.Sites = append(d.Sites, hs)
	}
	return View{Title: "Webhooks", Section: "webhooks", Form: f, Data: d, Status: status}, nil
}

func (u *UI) webhooksPage(w http.ResponseWriter, r *http.Request) {
	v, err := u.webhooksView(r.Context(), newForm(r), 0)
	if err != nil {
		u.serverError(w, r, err)
		return
	}
	u.render(w, r, "webhooks", v)
}

func (u *UI) setPublicAddress(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	f := newForm(r)
	if _, fromFile := u.App.PublicAddress(); fromFile {
		u.fileDefined(w, r, "The public address (server.external_url)")
		return
	}
	raw := strings.TrimSpace(f.Get("address"))
	if raw != "" && !strings.Contains(raw, "://") {
		f.SetError("address", "Enter the full address, starting with https:// (or http://), like https://mirror.example.com.")
	} else if err := u.App.SetPublicAddress(ctx, raw); err != nil {
		if !errors.Is(err, app.ErrConfig) {
			u.serverError(w, r, err)
			return
		}
		f.SetError("address", "Enter the full address, starting with https:// (or http://), like https://mirror.example.com. "+
			"It must not contain a user name, password, ? or #.")
	}
	if !f.OK() {
		v, err := u.webhooksView(ctx, f, http.StatusBadRequest)
		if err != nil {
			u.serverError(w, r, err)
			return
		}
		u.render(w, r, "webhooks", v)
		return
	}
	u.log().Info("web UI: public address changed", "address", strings.TrimRight(raw, "/"))
	if raw == "" {
		u.setFlash(w, r, FlashSuccess, "Public address removed.")
	} else {
		u.setFlash(w, r, FlashSuccess, "Public address saved.")
	}
	redirect(w, r, "/webhooks")
}

func (u *UI) newSecretPage(w http.ResponseWriter, r *http.Request) {
	if _, fromFile := u.App.WebhookSecret(); fromFile {
		u.fileDefined(w, r, "The webhook secret (server.webhook_secret)")
		return
	}
	u.render(w, r, "confirm", View{Title: "Create a new webhook secret", Section: "webhooks", Data: confirmData{
		Lines: []string{"The current secret stops working at once. Every webhook already set up with it must be updated " +
			"with the new secret on its site, or its deliveries are refused.",
			"“Set up webhooks for me” skips webhooks that already exist, so change their secret on the site by hand " +
				"(or delete them there and run “Set up webhooks for me” again)."},
		Action: "/webhooks/secret/new", Button: "Create a new secret", Back: "/webhooks"}})
}

func (u *UI) newSecret(w http.ResponseWriter, r *http.Request) {
	err := u.App.NewWebhookSecret(r.Context())
	switch {
	case errors.Is(err, config.ErrFileDefined):
		u.fileDefined(w, r, "The webhook secret (server.webhook_secret)")
		return
	case err != nil:
		u.serverError(w, r, err)
		return
	}
	u.log().Info("web UI: new webhook secret created")
	u.setFlash(w, r, FlashSuccess, "A new webhook secret was created. Update it in the webhooks you set up before.")
	redirect(w, r, "/webhooks")
}

// hookResult is the outcome of one webhook of "Set up webhooks for me".
type hookResult struct {
	Target  string // organisation or owner/repo
	Org     bool
	Status  statusView
	Outcome string
	Detail  string
}

type hookSetupData struct {
	Site       string
	PayloadURL string
	Results    []hookResult
	Problems   []string
}

func (u *UI) setupHooks(w http.ResponseWriter, r *http.Request) {
	site := r.PathValue("site")
	snap := u.App.Snapshot()
	cfg := snap.Config
	var rules []string
	for _, rl := range cfg.Rules {
		if rl.From.Forge == site {
			rules = append(rules, rl.Name)
		}
	}
	if len(rules) == 0 {
		u.notFound(w, r)
		return
	}
	addr, _ := u.App.PublicAddress()
	sec, _ := u.App.WebhookSecret()
	hp, ok := snap.Providers[site].(forge.HookProvider)
	if addr == "" || sec == "" || !ok {
		u.render(w, r, "message", View{Title: "Webhooks cannot be set up yet", Section: "webhooks", Status: http.StatusBadRequest,
			Data: "Add a public address and a webhook secret on the Webhooks page first. Some kinds of site cannot have webhooks set up by gitmirror."})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), hookTimeout)
	defer cancel()
	d := hookSetupData{Site: site, PayloadURL: payloadURL(addr, site)}
	d.Results, d.Problems = u.ensureHooks(ctx, snap, hp, rules, d.PayloadURL, sec)
	u.log().Info("web UI: set up webhooks", "site", site, "targets", len(d.Results))
	u.render(w, r, "webhook_setup", View{Title: "Webhooks for " + site, Section: "webhooks", Data: d})
}

func hasHook(hs []forge.Hook, url string) bool {
	for _, h := range hs {
		if strings.EqualFold(strings.TrimRight(h.URL, "/"), strings.TrimRight(url, "/")) {
			return true
		}
	}
	return false
}

// hookFailure explains a failed webhook call.
func hookFailure(err error) (string, string) {
	msg := err.Error()
	l := strings.ToLower(msg)
	if strings.Contains(l, "403") || strings.Contains(l, "forbidden") || strings.Contains(l, "admin rights") {
		return hookPermissionHint, msg
	}
	if errors.Is(err, forge.ErrNotFound) {
		return "Not found, or the token cannot see it. On GitHub, a token without webhook permission also gets this answer.", msg
	}
	return explain(msg).Text, msg
}

// ensureHooks makes sure every source of rules on the site has a webhook
// to url: one organisation webhook for an owner that is an organisation
// (OrgHooks answers for it), one per repository otherwise.
func (u *UI) ensureHooks(ctx context.Context, snap *app.Snapshot, hp forge.HookProvider, rules []string, url, secret string) ([]hookResult, []string) {
	cfg := snap.Config
	res := resolve.Resolve(ctx, cfg, snap.Providers, rules...)
	var problems []string
	for _, e := range res.Errors {
		problems = append(problems, e.Rule+": "+explain(e.Err.Error()).Text)
	}
	opts := forge.HookOptions{URL: url, Secret: secret, Events: hp.HookEvents()}
	var out []hookResult

	// Owners of owner rules: is it an organisation?
	orgs := map[string]bool{} // lowercase org -> handled
	var owners []string
	for _, rl := range cfg.Rules {
		if slices.Contains(rules, rl.Name) && rl.From.Owner != "" && !slices.Contains(owners, strings.ToLower(rl.From.Owner)) {
			owners = append(owners, strings.ToLower(rl.From.Owner))
		}
	}
	slices.Sort(owners)
	for _, owner := range owners {
		hooks, err := hp.OrgHooks(ctx, owner)
		if err != nil {
			// Not an organisation (or not allowed to tell): hook each repository.
			continue
		}
		orgs[owner] = true
		hr := hookResult{Target: owner, Org: true}
		if hasHook(hooks, url) {
			hr.Status, hr.Outcome = stOK, "Already there."
			hr.Status.Label = "Already there"
		} else if _, err := hp.CreateOrgHook(ctx, owner, opts); err != nil {
			hr.Status = stFailed
			hr.Outcome, hr.Detail = hookFailure(err)
		} else {
			hr.Status, hr.Outcome = stOK, "Added: one webhook for every repository of the organisation, including new ones."
			hr.Status.Label = "Added"
		}
		out = append(out, hr)
	}

	var repos []string
	for _, p := range res.Pairs {
		full := p.Src.FullName()
		if orgs[strings.ToLower(p.Src.Owner)] {
			continue // covered by the organisation webhook
		}
		if !slices.ContainsFunc(repos, func(s string) bool { return strings.EqualFold(s, full) }) {
			repos = append(repos, full)
		}
	}
	slices.SortFunc(repos, func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
	for _, full := range repos {
		owner, name, _ := strings.Cut(full, "/")
		hr := hookResult{Target: full}
		hooks, err := hp.RepoHooks(ctx, owner, name)
		switch {
		case err != nil:
			hr.Status = stFailed
			hr.Outcome, hr.Detail = hookFailure(err)
		case hasHook(hooks, url):
			hr.Status, hr.Outcome = stOK, "Already there."
			hr.Status.Label = "Already there"
		default:
			if _, err := hp.CreateRepoHook(ctx, owner, name, opts); err != nil {
				hr.Status = stFailed
				hr.Outcome, hr.Detail = hookFailure(err)
			} else {
				hr.Status, hr.Outcome = stOK, "Added."
				hr.Status.Label = "Added"
			}
		}
		out = append(out, hr)
	}
	for i := range out {
		out[i].Detail = strings.ReplaceAll(out[i].Detail, secret, "[hidden]")
	}
	return out, problems
}
