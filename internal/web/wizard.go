package web

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"gitmirror/internal/app"
	"gitmirror/internal/config"
	"gitmirror/internal/forge"
	"gitmirror/internal/state"
)

// The mirror wizard is one form shown in steps. Every step posts the whole
// state (the fields of the other steps travel as hidden inputs) to
// /mirrors/wizard with a "go" button value; nothing is stored until Save,
// and every field is validated again on Preview and Save. Hidden fields are
// never trusted for anything that matters: whether the edited mirror may be
// changed, the sites, names and the resulting rule are all checked again
// against the current configuration.

const (
	stepFrom    = "from"
	stepRepos   = "repos"
	stepTo      = "to"
	stepOptions = "options"
	stepPreview = "preview"
)

var wizardSteps = []struct{ id, label string }{
	{stepFrom, "Copy from"},
	{stepRepos, "Repositories"},
	{stepTo, "Copy to"},
	{stepOptions, "Options"},
	{stepPreview, "Check and save"},
}

const (
	modePick    = "pick"
	modeOwner   = "owner"
	modeStarred = "starred"

	namingSame   = "same"
	namingPrefix = "prefix"
	namingCustom = "custom"

	tmplSame   = "{{.Name}}"
	tmplPrefix = "{{.Owner}}-{{.Name}}"

	// listTimeout bounds the repository listing of the wizard.
	listTimeout = 30 * time.Second
	// maxListed is the most repositories the pick list shows at once.
	maxListed = 500
)

// featureChoices are the optional parts of a copy, in config order. Code
// (git) is always copied.
var featureChoices = []struct{ Value, Label, Hint string }{
	{config.FeatureLFS, "Large files (LFS)", "Big files stored with Git LFS, such as images or data sets."},
	{config.FeatureSettings, "Description and settings", "The description, website, topics, default branch and archived state."},
	{config.FeatureReleases, "Releases", "Releases with their downloadable files."},
	{config.FeatureWiki, "Wiki", "The wiki pages, when the repository has a wiki."},
}

var (
	accountRe  = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	repoNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// wizard is the state of the mirror form.
type wizard struct {
	Edit     string // name of the mirror being edited; "" for a new one
	Step     string
	Src      string
	Mode     string
	Account  string
	Picked   []string
	PickFor  string // src/account/mode the picks and filters belong to
	Query    string
	Forks    bool
	Archived bool
	// FilterSet and OptsSet record that the repositories / options step
	// was shown, so its defaults are not applied again.
	FilterSet  bool
	Dst        string
	DstOwner   string
	Naming     string
	DstName    string
	Features   []string
	Visibility string
	Interval   string
	Adopt      bool
	Name       string
	OptsSet    bool
}

func (w *wizard) pickKey() string { return w.Src + "/" + w.Account + "/" + w.Mode }

func readWizard(f *Form) *wizard {
	v := f.Values
	get := func(k string) string { return strings.TrimSpace(v.Get(k)) }
	w := &wizard{
		Edit: get("edit"), Step: get("step"), Src: get("src"), Mode: get("mode"), Account: get("account"),
		PickFor: v.Get("pickfor"), Query: get("q"), Forks: v.Get("forks") == "1", Archived: v.Get("archived") == "1",
		FilterSet: v.Get("filterset") == "1", Dst: get("dst"), DstOwner: get("dstowner"), Naming: get("naming"),
		DstName: get("dstname"), Visibility: get("visibility"), Interval: get("interval"), Adopt: v.Get("adopt") == "1",
		Name: get("name"), OptsSet: v.Get("optsset") == "1",
	}
	for _, p := range v["repo"] {
		if p = strings.TrimSpace(p); p != "" && !slices.Contains(w.Picked, p) {
			w.Picked = append(w.Picked, p)
		}
	}
	slices.Sort(w.Picked)
	for _, fc := range featureChoices {
		if slices.Contains(v["feat"], fc.Value) {
			w.Features = append(w.Features, fc.Value)
		}
	}
	if w.PickFor != "" && w.PickFor != w.pickKey() {
		// The source changed: earlier picks and filters no longer apply.
		w.Picked, w.FilterSet, w.Forks, w.Archived, w.PickFor = nil, false, false, false, ""
	}
	return w
}

type kv struct{ Name, Value string }

// hidden returns the fields that are not edited on step, as hidden inputs.
func (w *wizard) hidden(step string, shownRepos []string) []kv {
	var out []kv
	add := func(name, value string) { out = append(out, kv{name, value}) }
	flag := func(name string, b bool) {
		if b {
			add(name, "1")
		}
	}
	add("step", step)
	if w.Edit != "" {
		add("edit", w.Edit)
	}
	add("pickfor", w.PickFor)
	flag("filterset", w.FilterSet)
	flag("optsset", w.OptsSet)
	if step != stepFrom {
		add("src", w.Src)
		add("mode", w.Mode)
		add("account", w.Account)
	}
	for _, p := range w.Picked {
		if step != stepRepos || !slices.Contains(shownRepos, p) {
			add("repo", p)
		}
	}
	if step != stepRepos {
		if w.Query != "" {
			add("q", w.Query)
		}
		flag("forks", w.Forks)
		flag("archived", w.Archived)
	}
	if step != stepTo {
		add("dst", w.Dst)
		add("dstowner", w.DstOwner)
		add("naming", w.Naming)
		add("dstname", w.DstName)
	}
	if step != stepOptions {
		for _, f := range w.Features {
			add("feat", f)
		}
		add("visibility", w.Visibility)
		add("interval", w.Interval)
		flag("adopt", w.Adopt)
		add("name", w.Name)
	}
	return out
}

// ---- validation ----

// checkFrom validates the source and fills the default account.
func (u *UI) checkFrom(ctx context.Context, snap *app.Snapshot, w *wizard, f *Form) {
	if _, ok := snap.Config.Forges[w.Src]; !ok {
		f.SetError("src", "Choose the site to copy from.")
		return
	}
	switch w.Mode {
	case modePick, modeOwner:
	case modeStarred:
		if _, ok := snap.Providers[w.Src].(forge.StarProvider); !ok {
			f.SetError("mode", "This site cannot list starred repositories.")
		}
	default:
		f.SetError("mode", "Choose what to copy.")
	}
	if w.Account == "" {
		w.Account = account(ctx, snap, w.Src)
	}
	switch {
	case w.Account == "":
		f.SetError("account", "Enter the account or organisation to copy from.")
	case !accountRe.MatchString(w.Account):
		f.SetError("account", "Enter an account name like alice or my-org: letters, digits, '.', '-' and '_' only.")
	}
}

func (u *UI) checkRepos(w *wizard, f *Form) {
	if w.Mode != modePick {
		return
	}
	if len(w.Picked) == 0 {
		f.SetError("repo", "Tick at least one repository.")
	}
	for _, p := range w.Picked {
		if strings.ContainsAny(p, "/ \t") {
			f.SetError("repo", "One of the ticked repositories has an invalid name.")
		}
	}
}

func (u *UI) checkTo(ctx context.Context, snap *app.Snapshot, w *wizard, f *Form) {
	if _, ok := snap.Config.Forges[w.Dst]; !ok {
		f.SetError("dst", "Choose the site to copy to.")
		return
	}
	if w.DstOwner == "" {
		w.DstOwner = account(ctx, snap, w.Dst)
	}
	switch {
	case w.DstOwner == "":
		f.SetError("dstowner", "Enter the account or organisation to copy into.")
	case !accountRe.MatchString(w.DstOwner):
		f.SetError("dstowner", "Enter an account name like carol or my-org: letters, digits, '.', '-' and '_' only.")
	}
	switch w.Naming {
	case namingSame, namingPrefix:
	case namingCustom:
		switch {
		case w.Mode != modePick || len(w.Picked) != 1:
			f.SetError("naming", "A name of your own works only when you copy one repository.")
		case !repoNameRe.MatchString(w.DstName) || strings.HasSuffix(w.DstName, ".git") || w.DstName == "." || w.DstName == "..":
			f.SetError("dstname", "Enter a repository name: letters, digits, '.', '-' and '_' only.")
		}
	default:
		f.SetError("naming", "Choose how the copies are named.")
	}
}

func (u *UI) checkOptions(cfg *config.Config, w *wizard, f *Form) {
	if w.Visibility != "private" && w.Visibility != "match-source" {
		f.SetError("visibility", "Choose who can see the copies.")
	}
	if d, err := time.ParseDuration(w.Interval); err != nil || d < time.Minute {
		f.SetError("interval", "Choose how often to copy.")
	}
	if w.Name == "" {
		w.Name = suggestMirrorName(cfg, w)
	}
	switch {
	case !config.ValidName(w.Name):
		f.SetError("name", "Use only lowercase letters, digits, - and _ in the name, starting with a letter or digit, like alice-to-gitea.")
	case w.Name != w.Edit && cfg.RuleOrigin(w.Name) != "":
		f.SetError("name", "That name is already used by another mirror. Choose another name.")
	}
}

// checkAll validates every step (Preview and Save).
func (u *UI) checkAll(ctx context.Context, snap *app.Snapshot, w *wizard, f *Form) {
	u.checkFrom(ctx, snap, w, f)
	u.checkRepos(w, f)
	u.checkTo(ctx, snap, w, f)
	u.checkOptions(snap.Config, w, f)
}

// firstBadStep is the first step whose fields have errors.
func firstBadStep(f *Form) string {
	owner := map[string]string{"src": stepFrom, "mode": stepFrom, "account": stepFrom, "repo": stepRepos,
		"dst": stepTo, "dstowner": stepTo, "naming": stepTo, "dstname": stepTo}
	best := stepOptions
	for k := range f.Errors {
		if s, ok := owner[k]; ok && stepIndex(s) < stepIndex(best) {
			best = s
		}
	}
	return best
}

func stepIndex(s string) int {
	for i, st := range wizardSteps {
		if st.id == s {
			return i
		}
	}
	return 0
}

func suggestMirrorName(cfg *config.Config, w *wizard) string {
	base, suffix := w.Account, "-repos"
	switch {
	case w.Mode == modePick && len(w.Picked) == 1:
		base, suffix = w.Picked[0], "-copy"
	case w.Mode == modeStarred:
		suffix = "-stars"
	}
	base = strings.Trim(nonName.ReplaceAllString(strings.ToLower(base), "-"), "-")
	if base == "" {
		base = "mirror"
	} else {
		base += suffix
	}
	if !config.ValidName(base) {
		base = "mirror"
	}
	name := base
	for i := 2; name != w.Edit && cfg.RuleOrigin(name) != ""; i++ {
		name = base + "-" + strconv.Itoa(i)
	}
	return name
}

// ---- rule mapping ----

// escapePattern makes a repository name a path.Match pattern that matches
// only itself.
func escapePattern(name string) string {
	var b strings.Builder
	for _, r := range name {
		if strings.ContainsRune(`*?[\`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// literalPattern returns the name a pattern made by escapePattern matches;
// ok is false for a pattern with wildcards.
func literalPattern(p string) (string, bool) {
	var b strings.Builder
	esc := false
	for _, r := range p {
		switch {
		case esc:
			b.WriteRune(r)
			esc = false
		case r == '\\':
			esc = true
		case strings.ContainsRune("*?[", r):
			return "", false
		default:
			b.WriteRune(r)
		}
	}
	return b.String(), !esc && b.Len() > 0
}

// pickedNames returns the exact repository names of an owner rule made from
// a multi-repository pick: every include pattern is a literal name and
// forks, archived and private repositories are all allowed.
func pickedNames(r config.Rule) ([]string, bool) {
	f := r.From.Filter
	if r.From.Owner == "" || f == nil || len(f.Include) == 0 || len(f.Exclude) > 0 ||
		f.Forks == nil || !*f.Forks || f.Archived == nil || !*f.Archived || f.Private == nil || !*f.Private {
		return nil, false
	}
	var out []string
	for _, p := range f.Include {
		n, ok := literalPattern(p)
		if !ok {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

func ptr[T any](v T) *T { return &v }

// rule builds the mirror rule of a validated wizard.
func (w *wizard) rule() config.Rule {
	r := config.Rule{Name: w.Name, Interval: w.Interval, Visibility: w.Visibility, Adopt: w.Adopt,
		Include: append([]string{config.FeatureGit}, w.Features...)}
	r.From.Forge, r.To.Forge = w.Src, w.Dst
	tmpl := tmplSame
	if w.Naming == namingPrefix {
		tmpl = tmplPrefix
	}
	switch {
	case w.Mode == modePick && len(w.Picked) == 1:
		name := w.Picked[0]
		r.From.Repo = w.Account + "/" + name
		switch w.Naming {
		case namingPrefix:
			name = w.Account + "-" + name
		case namingCustom:
			name = w.DstName
		}
		r.To.Repo = w.DstOwner + "/" + name
		return r
	case w.Mode == modePick:
		r.From.Owner = w.Account
		f := &config.Filter{Forks: ptr(true), Archived: ptr(true), Private: ptr(true)}
		for _, p := range w.Picked {
			f.Include = append(f.Include, escapePattern(p))
		}
		r.From.Filter = f
	case w.Mode == modeStarred:
		r.From.StarredBy = w.Account
		r.From.Filter = &config.Filter{Forks: ptr(w.Forks), Archived: ptr(w.Archived)}
	default:
		r.From.Owner = w.Account
		r.From.Filter = &config.Filter{Forks: ptr(w.Forks), Archived: ptr(w.Archived)}
	}
	r.To.Owner, r.To.Name = w.DstOwner, tmpl
	return r
}

// wizardFromRule fills the wizard from an existing rule (edit).
func wizardFromRule(cfg *config.Config, r config.Rule) *wizard {
	eff := r.Effective(cfg.Defaults)
	w := &wizard{Edit: r.Name, Step: stepFrom, Src: r.From.Forge, Dst: r.To.Forge, Name: r.Name,
		Visibility: eff.Visibility, Interval: r.Interval, Adopt: r.Adopt, FilterSet: true, OptsSet: true, Naming: namingSame}
	if w.Interval == "" {
		w.Interval = cfg.Defaults.Interval
	}
	for _, fc := range featureChoices {
		if eff.Includes(fc.Value) {
			w.Features = append(w.Features, fc.Value)
		}
	}
	srcName := ""
	switch {
	case r.From.Repo != "":
		owner, name, _ := strings.Cut(r.From.Repo, "/")
		w.Mode, w.Account, w.Picked, srcName = modePick, owner, []string{name}, name
	case r.From.StarredBy != "":
		w.Mode, w.Account = modeStarred, r.From.StarredBy
	default:
		w.Account = r.From.Owner
		if names, ok := pickedNames(r); ok {
			w.Mode, w.Picked = modePick, names
		} else {
			w.Mode = modeOwner
		}
	}
	if w.Mode != modePick && r.From.Filter != nil {
		w.Forks = r.From.Filter.Forks != nil && *r.From.Filter.Forks
		w.Archived = r.From.Filter.Archived == nil || *r.From.Filter.Archived
	}
	if r.To.Repo != "" {
		owner, name, _ := strings.Cut(r.To.Repo, "/")
		w.DstOwner = owner
		switch name {
		case srcName:
		case w.Account + "-" + srcName:
			w.Naming = namingPrefix
		default:
			w.Naming, w.DstName = namingCustom, name
		}
	} else {
		w.DstOwner = r.To.Owner
		if r.To.Name == tmplPrefix {
			w.Naming = namingPrefix
		}
	}
	w.PickFor = w.pickKey()
	return w
}

// ---- pages ----

type choice struct {
	Value, Label, Hint string
	Checked            bool
}

type repoChoice struct {
	Name, Description       string
	Private, Fork, Archived bool
	Checked                 bool
}

type previewRow struct {
	Src, Dst, Outcome, Detail string
	Status                    statusView
}

type previewView struct {
	Rows     []previewRow
	Problems []string
	// More counts the repositories beyond app.PreviewLimit, not listed.
	More int
}

type wizardView struct {
	W         *wizard
	Step      string
	StepNo    int
	Steps     []choice // Value = step id, Label, Checked = current
	Hidden    []kv
	Sites     []siteRow
	Modes     []choice
	Namings   []choice
	Features  []choice
	Intervals []choice
	Repos     []repoChoice
	Listed    int // repositories on the account before filtering
	Picked    int
	Preview   *previewView
	ToLabel   string
	FromLabel string
	Every     string
}

func (u *UI) renderWizard(w http.ResponseWriter, r *http.Request, f *Form, wz *wizard, step string, status int) {
	u.renderWizardWith(w, r, f, wz, step, status, nil)
}

// renderWizardWith shows step of the wizard; pv is the dry run shown on the
// preview step.
func (u *UI) renderWizardWith(w http.ResponseWriter, r *http.Request, f *Form, wz *wizard, step string, status int, pv *previewView) {
	ctx := r.Context()
	snap := u.App.Snapshot()
	v := &wizardView{W: wz, Step: step, StepNo: stepIndex(step) + 1, Preview: pv}
	for _, s := range wizardSteps {
		v.Steps = append(v.Steps, choice{Value: s.id, Label: s.label, Checked: s.id == step})
	}
	var shown []string
	switch step {
	case stepFrom, stepTo:
		v.Sites = u.siteRows(ctx)
		v.Modes = []choice{
			{modePick, "Choose repositories", "Pick one or more repositories from a list.", wz.Mode == modePick},
			{modeOwner, "Everything from an account or organisation", "Also copies repositories added there later.", wz.Mode == modeOwner},
			{modeStarred, "Everything starred by an account", "Copies the repositories the account has starred.", wz.Mode == modeStarred},
		}
		v.Namings = []choice{
			{namingSame, "Keep the same names", "For example, alice/tool is copied as tool.", wz.Naming == namingSame},
			{namingPrefix, "Prefix with the original owner", "For example, alice/tool is copied as alice-tool. Avoids clashes when copying from several owners.", wz.Naming == namingPrefix},
		}
		if wz.Mode == modePick && len(wz.Picked) == 1 {
			v.Namings = append(v.Namings, choice{namingCustom, "A name of my own", "Type the name below.", wz.Naming == namingCustom})
		}
	case stepRepos:
		if wz.Mode == modePick {
			shown = u.listRepos(ctx, snap, wz, f, v)
		}
		v.Picked = len(wz.Picked)
	case stepOptions:
		for _, fc := range featureChoices {
			v.Features = append(v.Features, choice{fc.Value, fc.Label, fc.Hint, slices.Contains(wz.Features, fc.Value)})
		}
		known := false
		for _, iv := range intervals {
			known = known || iv.Value == wz.Interval
			v.Intervals = append(v.Intervals, choice{Value: iv.Value, Label: iv.Label, Checked: iv.Value == wz.Interval})
		}
		if d, err := time.ParseDuration(wz.Interval); !known && err == nil {
			v.Intervals = append([]choice{{Value: wz.Interval, Label: "E" + everyLabel(d)[1:], Checked: true}}, v.Intervals...)
		}
	case stepPreview:
		rl := wz.rule()
		v.FromLabel, v.ToLabel = fromLabel(rl), toLabel(rl)
		if d, err := time.ParseDuration(wz.Interval); err == nil {
			v.Every = everyLabel(d)
		}
	}
	v.Hidden = wz.hidden(step, shown)
	title := "New mirror"
	if wz.Edit != "" {
		title = "Change mirror " + wz.Edit
	}
	if v.Preview == nil && step == stepPreview {
		v.Preview = &previewView{}
	}
	u.render(w, r, "wizard", View{Title: title, Section: "mirrors", Form: f, Data: v, Status: status})
}

// listRepos fills v.Repos with the source account's repositories that match
// the search and toggles; it returns the names shown.
func (u *UI) listRepos(ctx context.Context, snap *app.Snapshot, wz *wizard, f *Form, v *wizardView) []string {
	p, ok := snap.Providers[wz.Src]
	if !ok {
		f.Fail("The site to copy from is gone. Go back and choose another one.")
		return nil
	}
	lctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	repos, err := p.ListRepos(lctx, wz.Account)
	if err != nil {
		u.log().Info("web UI: list repositories failed", "site", wz.Src, "account", wz.Account, "err", err)
		if errors.Is(err, forge.ErrNotFound) {
			f.Fail("There is no account or organisation named " + wz.Account + " on " + wz.Src + ". Go back and check the name.")
		} else {
			f.Fail("gitmirror could not list the repositories of " + wz.Account + ". " + explain(err.Error()).Text)
		}
		return nil
	}
	v.Listed = len(repos)
	q := strings.ToLower(wz.Query)
	var shown []string
	for _, rp := range repos {
		picked := slices.Contains(wz.Picked, rp.Name)
		if !picked && ((rp.Fork && !wz.Forks) || (rp.Archived && !wz.Archived)) {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(rp.Name), q) && !strings.Contains(strings.ToLower(rp.Description), q) {
			continue
		}
		if len(v.Repos) >= maxListed {
			break
		}
		v.Repos = append(v.Repos, repoChoice{Name: rp.Name, Description: truncate(rp.Description, 140),
			Private: rp.Private, Fork: rp.Fork, Archived: rp.Archived, Checked: picked})
		shown = append(shown, rp.Name)
	}
	return shown
}

// enter applies the defaults of step the first time it is shown.
func (u *UI) enter(cfg *config.Config, wz *wizard, step string) {
	switch step {
	case stepRepos:
		if !wz.FilterSet {
			wz.Forks, wz.Archived = false, wz.Mode != modePick
			wz.FilterSet = true
		}
		wz.PickFor = wz.pickKey()
	case stepTo:
		if wz.Dst == "" && len(cfg.Forges) == 1 {
			for n := range cfg.Forges {
				wz.Dst = n
			}
		}
		if wz.Naming == "" || wz.Naming == namingCustom && (wz.Mode != modePick || len(wz.Picked) != 1) {
			wz.Naming = namingSame
		}
	case stepOptions:
		if !wz.OptsSet {
			wz.Features = nil
			for _, fc := range featureChoices {
				wz.Features = append(wz.Features, fc.Value)
			}
			wz.Visibility, wz.Interval, wz.Adopt = "private", "1h", false
			wz.OptsSet = true
		}
		if wz.Name == "" {
			wz.Name = suggestMirrorName(cfg, wz)
		}
	}
}

func (u *UI) newMirrorPage(w http.ResponseWriter, r *http.Request) {
	cfg := u.App.Config()
	if len(cfg.Forges) == 0 {
		u.setFlash(w, r, FlashInfo, "Add a site first: the place to copy from and the place to copy to.")
		redirect(w, r, "/sites/new")
		return
	}
	wz := &wizard{Mode: modePick}
	if len(cfg.Forges) == 1 {
		for n := range cfg.Forges {
			wz.Src = n
		}
	}
	u.renderWizard(w, r, newForm(r), wz, stepFrom, 0)
}

// editableMirror checks that path's mirror exists and is managed in the
// UI; it answers the request otherwise.
func (u *UI) editableMirror(w http.ResponseWriter, r *http.Request, name string) bool {
	switch u.App.Config().RuleOrigin(name) {
	case "":
		u.notFound(w, r)
		return false
	case config.OriginFile:
		u.fileDefined(w, r, "The mirror "+name)
		return false
	}
	return true
}

func (u *UI) editMirrorPage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !u.editableMirror(w, r, name) {
		return
	}
	cfg := u.App.Config()
	for _, rl := range cfg.Rules {
		if rl.Name == name {
			u.renderWizard(w, r, newForm(r), wizardFromRule(cfg, rl), stepFrom, 0)
			return
		}
	}
	u.notFound(w, r)
}

func (u *UI) wizardSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	f := newForm(r)
	wz := readWizard(f)
	if wz.Edit != "" && !u.editableMirror(w, r, wz.Edit) {
		return
	}
	snap := u.App.Snapshot()
	cfg := snap.Config
	step := wz.Step
	if stepIndex(step) == 0 {
		step = stepFrom
	}
	show := func(next string, status int) {
		u.enter(cfg, wz, next)
		u.renderWizard(w, r, f, wz, next, status)
	}
	bad := func() { u.renderWizard(w, r, f, wz, firstBadStep(f), http.StatusBadRequest) }
	switch f.Get("go") {
	case "back":
		f.Errors = map[string]string{}
		show(wizardSteps[max(stepIndex(step)-1, 0)].id, 0)
	case "search":
		show(stepRepos, 0)
	case "next":
		switch step {
		case stepFrom:
			u.checkFrom(ctx, snap, wz, f)
		case stepRepos:
			u.checkRepos(wz, f)
		case stepTo:
			u.checkTo(ctx, snap, wz, f)
		case stepOptions:
			u.checkOptions(cfg, wz, f)
		}
		if !f.OK() {
			u.renderWizard(w, r, f, wz, step, http.StatusBadRequest)
			return
		}
		next := min(stepIndex(step)+1, len(wizardSteps)-1)
		if wizardSteps[next].id == stepPreview {
			u.checkAll(ctx, snap, wz, f)
			if !f.OK() {
				bad()
				return
			}
			u.preview(w, r, f, wz)
			return
		}
		show(wizardSteps[next].id, 0)
	case "preview":
		u.checkAll(ctx, snap, wz, f)
		if !f.OK() {
			bad()
			return
		}
		u.preview(w, r, f, wz)
	case "save":
		u.checkAll(ctx, snap, wz, f)
		if !f.OK() {
			bad()
			return
		}
		u.saveMirror(w, r, f, wz)
	default:
		show(step, 0)
	}
}

// change is the UI change that saves the wizard's rule.
func (wz *wizard) change(rl config.Rule) func(*app.UIConfig) error {
	return func(c *app.UIConfig) error {
		if wz.Edit != "" && wz.Edit != rl.Name {
			if err := c.DeleteRule(wz.Edit); err != nil {
				return err
			}
		}
		return c.PutRule(rl)
	}
}

// changeFailed shows a failed preview or save on the options step.
func (u *UI) changeFailed(w http.ResponseWriter, r *http.Request, f *Form, wz *wizard, err error) {
	switch {
	case errors.Is(err, config.ErrFileDefined):
		u.fileDefined(w, r, "The mirror "+wz.Name)
		return
	case errors.Is(err, app.ErrUnknownEntry):
		f.Fail("The mirror you were changing no longer exists. It may have been deleted meanwhile.")
	case errors.Is(err, app.ErrConfig) && destinationProblem(err):
		// Caused by the choices of step 3: show it there, on the field.
		f.SetError("dstowner", strings.Join(configProblems(err), " "))
		u.renderWizard(w, r, f, wz, stepTo, http.StatusBadRequest)
		return
	case errors.Is(err, app.ErrConfig):
		f.Fail(strings.Join(configProblems(err), " "))
	default:
		u.serverError(w, r, err)
		return
	}
	u.renderWizard(w, r, f, wz, stepOptions, http.StatusBadRequest)
}

// destinationFrags mark validation errors about where the copies go (step
// 3 of the wizard).
var destinationFrags = []string{"source and destination are the same", "is already the destination of rule",
	"is targeted by multiple sources", "mirror loop", "to.owner", "to.repo", "to.name", "destination for "}

// destinationProblem reports whether every problem of the validation
// error err is about the destination.
func destinationProblem(err error) bool {
	lines := splitErr(err)
	for _, l := range lines {
		l = strings.ToLower(l)
		if !slices.ContainsFunc(destinationFrags, func(f string) bool { return strings.Contains(l, f) }) {
			return false
		}
	}
	return len(lines) > 0
}

func (u *UI) preview(w http.ResponseWriter, r *http.Request, f *Form, wz *wizard) {
	rl := wz.rule()
	res, err := u.App.Preview(r.Context(), wz.change(rl), rl.Name)
	if err != nil {
		u.changeFailed(w, r, f, wz, err)
		return
	}
	pv := &previewView{}
	used := map[int]bool{}
	for _, rep := range res.Reports {
		row := previewRow{Src: rep.Key.SrcForge + ": " + rep.Key.SrcRepo, Dst: rep.Key.DstForge + ": " + rep.Key.DstRepo}
		acts := strings.Join(rep.Actions, "\n")
		switch {
		case rep.Status == state.RunFailed || rep.Status == state.RunAborted:
			msg := ""
			if rep.Err != nil {
				msg = rep.Err.Error()
			}
			p := explain(msg)
			row.Status, row.Outcome, row.Detail = stFailed, "Will not be copied. "+p.Text, p.Detail
			row.Status.Label = "Conflict"
		case strings.Contains(acts, "adopt existing repository"):
			row.Status, row.Outcome = stPartial, "The repository already exists there; its content will be replaced to match the original."
			row.Status.Label = "Replaces existing"
		case strings.Contains(acts, "create repository"):
			row.Status = stOK
			row.Status.Label = "Will be created"
		default:
			// The preview fetches no code, so it cannot tell whether
			// anything changed since the last copy.
			row.Status, row.Outcome = stRunning, "Only what changed since the last copy is copied."
			row.Status.Label, row.Status.Icon = "Will be updated", "↑"
		}
		if rep.Status == state.RunPartial && rep.Err != nil {
			row.Outcome = strings.TrimSpace(row.Outcome + " Some parts may fail: " + explain(rep.Err.Error()).Text)
			row.Detail = rep.Err.Error()
		}
		pv.Rows = append(pv.Rows, row)
	}
	for _, p := range res.Resolve.Dropped {
		row := previewRow{Src: p.SrcForge + ": " + p.Src.FullName(), Dst: p.DstForge + ": " + p.DstFullName(), Status: stStopped}
		row.Status.Label = "Will be skipped"
		for i, e := range res.Resolve.Errors {
			if strings.Contains(e.Err.Error(), p.Src.FullName()) {
				used[i] = true
				ex := explain(e.Err.Error())
				row.Outcome, row.Detail = ex.Text, ex.Detail
				break
			}
		}
		pv.Rows = append(pv.Rows, row)
	}
	for i, e := range res.Resolve.Errors {
		if !used[i] {
			pv.Problems = append(pv.Problems, explain(e.Err.Error()).Text)
		}
	}
	pv.More = max(len(res.Resolve.Pairs)-len(res.Reports), 0)
	u.renderWizardWith(w, r, f, wz, stepPreview, 0, pv)
}

func (u *UI) saveMirror(w http.ResponseWriter, r *http.Request, f *Form, wz *wizard) {
	rl := wz.rule()
	if err := u.App.UpdateUI(r.Context(), wz.change(rl)); err != nil {
		u.changeFailed(w, r, f, wz, err)
		return
	}
	u.log().Info("web UI: mirror saved", "mirror", rl.Name, "edited", wz.Edit)
	msg := "Mirror saved."
	if u.SyncNow != nil {
		if err := u.SyncNow(rl.Name); err != nil {
			u.log().Warn("web UI: sync now", "mirror", rl.Name, "err", err)
		}
		if wz.Edit == "" {
			msg += " The first copy starts in a moment."
		} else {
			msg += " A new copy starts in a moment."
		}
	} else {
		msg += " It is copied on its schedule while gitmirror serve runs."
	}
	u.setFlash(w, r, FlashSuccess, msg)
	redirect(w, r, "/")
}
