package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"gitmirror/internal/app"
	"gitmirror/internal/config"
	"gitmirror/internal/forge"
)

// Sites are config forges. The web UI manages its own (stored in the state
// database, tokens sealed); those of the config file are shown read-only.

// siteKinds are the choices of the "kind" field and the forge type each
// one stores. Forgejo speaks the Gitea API.
var siteKinds = map[string]string{"github": "github", "gitea": "gitea", "forgejo": "gitea"}

// accountTimeout bounds the live account lookups of the Sites page.
const accountTimeout = 5 * time.Second

type siteRow struct {
	Name, Kind, Address, Account string
	FromFile                     bool
	Mirrors                      []string
}

// siteAddress is the address shown for a forge.
func siteAddress(f config.Forge) string {
	if f.URL == "" && f.Type == "github" {
		return "https://github.com"
	}
	return f.URL
}

// mirrorsUsing lists the rules of cfg that read from or write to site.
func mirrorsUsing(cfg *config.Config, site string) []string {
	var out []string
	for _, r := range cfg.Rules {
		if r.From.Forge == site || r.To.Forge == site {
			out = append(out, r.Name)
		}
	}
	return out
}

// account returns the account a site's token signs in as: the stored
// username, else the provider's identity (cached by the provider).
func account(ctx context.Context, snap *app.Snapshot, site string) string {
	if f := snap.Config.Forges[site]; f.Username != "" {
		return f.Username
	}
	id, ok := snap.Providers[site].(forge.Identity)
	if !ok {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, accountTimeout)
	defer cancel()
	l, err := id.Login(ctx)
	if err != nil {
		return ""
	}
	return l
}

func (u *UI) siteRows(ctx context.Context) []siteRow {
	snap := u.App.Snapshot()
	cfg := snap.Config
	names := make([]string, 0, len(cfg.Forges))
	for n := range cfg.Forges {
		names = append(names, n)
	}
	slices.Sort(names)
	rows := make([]siteRow, len(names))
	var wg sync.WaitGroup
	for i, n := range names {
		f := cfg.Forges[n]
		rows[i] = siteRow{Name: n, Kind: kindLabel(f.Type), Address: siteAddress(f),
			FromFile: cfg.ForgeOrigin(n) == config.OriginFile, Mirrors: mirrorsUsing(cfg, n)}
		wg.Go(func() { rows[i].Account = account(ctx, snap, n) })
	}
	wg.Wait()
	return rows
}

func (u *UI) sitesPage(w http.ResponseWriter, r *http.Request) {
	u.render(w, r, "sites", View{Title: "Sites", Section: "sites", Data: u.siteRows(r.Context())})
}

// siteFormData is the data of the site form.
type siteFormData struct {
	// Edit is the name of the site being edited; "" when adding.
	Edit string
	// GitHubTokens and GiteaTokens are the token pages to link to; "" when
	// the address is not known yet.
	GitHubTokens, GiteaTokens string
}

func tokenPages(f *Form) siteFormData {
	d := siteFormData{GitHubTokens: "https://github.com/settings/tokens"}
	addr, err := normaliseAddress(f.Get("url"))
	if err != nil || addr == "" {
		return d
	}
	if siteKinds[f.Get("kind")] == "github" && !isGitHubCom(addr) {
		d.GitHubTokens = addr + "/settings/tokens"
	}
	d.GiteaTokens = addr + "/user/settings/applications"
	return d
}

func (u *UI) siteForm(w http.ResponseWriter, r *http.Request, f *Form, edit string, status int) {
	title := "Add a site"
	if edit != "" {
		title = "Change site " + edit
	}
	d := tokenPages(f)
	d.Edit = edit
	u.render(w, r, "site_form", View{Title: title, Section: "sites", Form: f, Data: d, Status: status})
}

func (u *UI) newSitePage(w http.ResponseWriter, r *http.Request) {
	f := newForm(r)
	f.Values.Set("kind", "github")
	u.siteForm(w, r, f, "", 0)
}

// normaliseAddress turns what was typed into a base URL: https:// is added
// when no scheme is given; spaces and trailing slashes are removed. "" stays
// "". The result is an absolute http(s) URL without user, query or fragment.
func normaliseAddress(raw string) (string, error) {
	s := strings.TrimRight(strings.TrimSpace(raw), "/")
	if s == "" {
		return "", nil
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	p, err := url.Parse(s)
	if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Hostname() == "" || p.User != nil ||
		p.RawQuery != "" || p.ForceQuery || p.Fragment != "" || strings.Contains(s, "#") {
		return "", errors.New("invalid address")
	}
	return s, nil
}

func isGitHubCom(addr string) bool {
	p, err := url.Parse(addr)
	if err != nil {
		return false
	}
	h := strings.ToLower(p.Hostname())
	return (h == "github.com" || h == "www.github.com") && strings.Trim(p.Path, "/") == ""
}

var nonName = regexp.MustCompile(`[^a-z0-9]+`)

// suggestName derives a site name from its address ("github" for
// github.com, "git-example-com" for https://git.example.com), made unique
// against cfg.
func suggestName(cfg *config.Config, typ, addr string) string {
	base := "github"
	if addr != "" {
		host := "site"
		if p, err := url.Parse(addr); err == nil && p.Hostname() != "" {
			host = strings.TrimPrefix(strings.ToLower(p.Hostname()), "www.")
			if port := p.Port(); port != "" {
				host += "-" + port
			}
		}
		base = strings.Trim(nonName.ReplaceAllString(host, "-"), "-")
		if base == "github-com" {
			base = "github"
		}
	} else if typ != "github" {
		base = "site"
	}
	if base == "" || !config.ValidName(base) {
		base = "site"
	}
	name := base
	for i := 2; cfg.ForgeOrigin(name) != ""; i++ {
		name = base + "-" + strconv.Itoa(i)
	}
	return name
}

// readSite validates the site form. old is the site being edited (its
// token is kept when the token field is blank); name is "" when adding.
func (u *UI) readSite(f *Form, name string, old *config.Forge) (string, config.Forge, bool) {
	cfg := u.App.Config()
	var fg config.Forge
	typ, ok := siteKinds[f.Get("kind")]
	if !ok {
		f.SetError("kind", "Choose the kind of site.")
	}
	fg.Type = typ
	addr, err := normaliseAddress(f.Get("url"))
	switch {
	case err != nil && typ == "github":
		f.SetError("url", "Enter a full address like https://github.example.com, or leave it empty for github.com.")
	case err != nil || (addr == "" && typ == "gitea"):
		f.SetError("url", "Enter the address of your server, like https://git.example.com.")
	case typ == "github" && isGitHubCom(addr):
		addr = ""
	}
	fg.URL = addr
	fg.Token = strings.TrimSpace(f.Get("token"))
	if fg.Token == "" {
		if old != nil {
			fg.Token = old.Token
		} else {
			f.SetError("token", "Paste the token you created on the site.")
		}
	}
	if name == "" {
		name = strings.TrimSpace(f.Get("name"))
		switch {
		case name == "":
			name = suggestName(cfg, typ, addr)
		case !config.ValidName(name):
			f.SetError("name", "Use only lowercase letters, digits, - and _, starting with a letter or digit, like work-github.")
		case cfg.ForgeOrigin(name) == config.OriginFile:
			f.SetError("name", "A site named "+name+" already exists in the config file. Choose another name.")
		case cfg.ForgeOrigin(name) != "":
			f.SetError("name", "A site named "+name+" already exists. Choose another name.")
		}
	}
	return name, fg, f.OK()
}

func (u *UI) addSite(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	f := newForm(r)
	name, fg, ok := u.readSite(f, "", nil)
	if !ok {
		u.siteForm(w, r, f, "", http.StatusBadRequest)
		return
	}
	login, err := u.App.CheckForge(ctx, name, fg)
	if err != nil {
		u.log().Info("web UI: site connection test failed", "site", name, "type", fg.Type)
		f.Fail(connectionProblem(err, fg.URL, fg.Token))
		u.siteForm(w, r, f, "", http.StatusBadRequest)
		return
	}
	fg.Username = login
	err = u.App.UpdateUI(ctx, func(c *app.UIConfig) error {
		if _, dup := c.Forges[name]; dup {
			return errSiteExists
		}
		return c.PutForge(name, fg)
	})
	if err != nil {
		if !u.saveFailed(w, r, f, err) {
			return
		}
		u.siteForm(w, r, f, "", http.StatusBadRequest)
		return
	}
	u.log().Info("web UI: site added", "site", name, "type", fg.Type, "account", login)
	u.setFlash(w, r, FlashSuccess, "Site "+name+" saved. Connected as "+login+".")
	redirect(w, r, "/sites")
}

var errSiteExists = errors.New("a site with this name already exists")

// saveFailed puts a failed UpdateUI's error on f in plain words and
// reports true; an unexpected error is answered with the error page
// (false).
func (u *UI) saveFailed(w http.ResponseWriter, r *http.Request, f *Form, err error) bool {
	switch {
	case errors.Is(err, errSiteExists):
		f.SetError("name", "A site with this name already exists. Choose another name.")
	case errors.Is(err, config.ErrFileDefined):
		f.Fail("This is defined in the config file, so it cannot be changed here.")
	case errors.Is(err, app.ErrConfig):
		f.Fail(strings.Join(configProblems(err), " "))
	default:
		u.serverError(w, r, err)
		return false
	}
	return true
}

// fileDefined answers a request to change an entry of the config file.
func (u *UI) fileDefined(w http.ResponseWriter, r *http.Request, what string) {
	u.render(w, r, "message", View{
		Title:  "Defined in the config file",
		Status: http.StatusForbidden,
		Data:   what + " is defined in gitmirror's config file, so it cannot be changed here. Edit the config file instead.",
	})
}

// editableSite checks that path's site exists and is managed in the UI; it
// answers the request otherwise.
func (u *UI) editableSite(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("name")
	switch u.App.Config().ForgeOrigin(name) {
	case "":
		u.notFound(w, r)
		return "", false
	case config.OriginFile:
		u.fileDefined(w, r, "The site "+name)
		return "", false
	}
	return name, true
}

func (u *UI) editSitePage(w http.ResponseWriter, r *http.Request) {
	name, ok := u.editableSite(w, r)
	if !ok {
		return
	}
	fg := u.App.Config().Forges[name]
	f := newForm(r)
	f.Values.Set("kind", fg.Type)
	f.Values.Set("url", fg.URL)
	u.siteForm(w, r, f, name, 0)
}

func (u *UI) editSite(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name, ok := u.editableSite(w, r)
	if !ok {
		return
	}
	cur, err := u.App.UI(ctx)
	if err != nil {
		u.serverError(w, r, err)
		return
	}
	old, ok := cur.Forges[name]
	if !ok {
		u.notFound(w, r)
		return
	}
	f := newForm(r)
	_, fg, ok := u.readSite(f, name, &old)
	if !ok {
		u.siteForm(w, r, f, name, http.StatusBadRequest)
		return
	}
	msg := "Site " + name + " saved."
	fg.Username = old.Username
	if fg.Type != old.Type || fg.URL != old.URL || fg.Token != old.Token {
		login, err := u.App.CheckForge(ctx, name, fg)
		if err != nil {
			u.log().Info("web UI: site connection test failed", "site", name, "type", fg.Type)
			f.Fail(connectionProblem(err, fg.URL, fg.Token))
			u.siteForm(w, r, f, name, http.StatusBadRequest)
			return
		}
		fg.Username = login
		msg += " Connected as " + login + "."
	}
	if err := u.App.UpdateUI(ctx, func(c *app.UIConfig) error { return c.PutForge(name, fg) }); err != nil {
		if u.saveFailed(w, r, f, err) {
			u.siteForm(w, r, f, name, http.StatusBadRequest)
		}
		return
	}
	u.log().Info("web UI: site changed", "site", name, "type", fg.Type)
	u.setFlash(w, r, FlashSuccess, msg)
	redirect(w, r, "/sites")
}

// confirmData is the data of the confirm page: a destructive action on its
// own page. Without an Action the page only explains why it is not possible.
type confirmData struct {
	Lines  []string
	Items  []string
	Action string
	Button string
	Back   string
}

func (u *UI) siteDeleteView(name string, used []string) View {
	if len(used) > 0 {
		return View{Title: "Delete site " + name, Section: "sites", Data: confirmData{
			Lines: []string{"This site cannot be deleted because these mirrors use it. Delete them or change them to use another site first:"},
			Items: used, Back: "/sites"}}
	}
	return View{Title: "Delete site " + name, Section: "sites", Data: confirmData{
		Lines: []string{"gitmirror will forget this site and its token. Repositories on the site itself are not touched.",
			"You can add the site again later."},
		Action: "/sites/" + name + "/delete", Button: "Delete site", Back: "/sites"}}
}

func (u *UI) deleteSitePage(w http.ResponseWriter, r *http.Request) {
	name, ok := u.editableSite(w, r)
	if !ok {
		return
	}
	u.render(w, r, "confirm", u.siteDeleteView(name, mirrorsUsing(u.App.Config(), name)))
}

func (u *UI) deleteSite(w http.ResponseWriter, r *http.Request) {
	name, ok := u.editableSite(w, r)
	if !ok {
		return
	}
	var used []string
	err := u.App.UpdateUI(r.Context(), func(c *app.UIConfig) error {
		// Checked again on the current set: rules may have changed since.
		used = mirrorsUsing(u.App.FileConfig(), name)
		for _, rl := range c.Rules {
			if rl.From.Forge == name || rl.To.Forge == name {
				used = append(used, rl.Name)
			}
		}
		if len(used) > 0 {
			return errInUse
		}
		return c.DeleteForge(name)
	})
	switch {
	case errors.Is(err, errInUse):
		slices.Sort(used)
		v := u.siteDeleteView(name, slices.Compact(used))
		v.Status = http.StatusConflict
		u.render(w, r, "confirm", v)
		return
	case errors.Is(err, config.ErrFileDefined):
		u.fileDefined(w, r, "The site "+name)
		return
	case errors.Is(err, app.ErrUnknownEntry):
		u.notFound(w, r)
		return
	case err != nil:
		u.serverError(w, r, err)
		return
	}
	u.log().Info("web UI: site deleted", "site", name)
	u.setFlash(w, r, FlashSuccess, "Site "+name+" deleted.")
	redirect(w, r, "/sites")
}

var errInUse = errors.New("site is in use")
