package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"gitmirror/internal/app"
	"gitmirror/internal/config"
	"gitmirror/internal/state"
)

// pairView is one original -> copy pair of a mirror.
type pairView struct {
	ID                       string
	Src, SrcURL, Dst, DstURL string
	Status                   statusView
	Last, LastAbs            string
	Problem                  *problem
}

// mirrorView is one mirror (rule) with the state of its pairs.
type mirrorView struct {
	Name           string
	FromFile       bool
	From, To       string
	Every          string
	Status         statusView
	LastRun        string
	LastRunAbs     string
	Pairs          []pairView
	Problems, OKs  int
	NotCopied, Run int
}

// pairID encodes a pair key for a URL.
func pairID(k state.PairKey) string { return base64.RawURLEncoding.EncodeToString([]byte(k.String())) }

func parsePairID(id string) (state.PairKey, bool) {
	b, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return state.PairKey{}, false
	}
	var parts []string
	if json.Unmarshal(b, &parts) != nil || len(parts) != 5 {
		return state.PairKey{}, false
	}
	return state.PairKey{Rule: parts[0], SrcForge: parts[1], SrcRepo: parts[2], DstForge: parts[3], DstRepo: parts[4]}, true
}

// repoURL is the web page of repo on site, when its address is known.
func repoURL(snap *app.Snapshot, site, repo string) string {
	base := snap.ForgeURLs[site]
	if !strings.HasPrefix(base, "https://") && !strings.HasPrefix(base, "http://") {
		return ""
	}
	return base + "/" + repo
}

// mirrorViews returns every mirror of the current configuration with the
// stored state of its pairs.
func (u *UI) mirrorViews(ctx context.Context) ([]mirrorView, error) {
	snap := u.App.Snapshot()
	cfg := snap.Config
	pairs, err := u.App.Store.Pairs(ctx)
	if err != nil {
		return nil, err
	}
	byRule := map[string][]state.PairState{}
	for _, p := range pairs {
		byRule[p.Key.Rule] = append(byRule[p.Key.Rule], p)
	}
	now := u.now()
	out := make([]mirrorView, 0, len(cfg.Rules))
	for _, r := range cfg.Rules {
		m := mirrorView{Name: r.Name, FromFile: cfg.RuleOrigin(r.Name) == config.OriginFile,
			From: fromLabel(r), To: toLabel(r), Every: everyLabel(r.Effective(cfg.Defaults).Interval), Status: stNever}
		if len(byRule[r.Name]) > 0 {
			m.Status = stOK
		}
		var lastRun time.Time
		for _, ps := range byRule[r.Name] {
			pv, run, err := u.pairView(ctx, snap, ps, now)
			if err != nil {
				return nil, err
			}
			m.Status = worse(m.Status, pv.Status)
			switch pv.Status {
			case stOK:
				m.OKs++
			case stNever:
				m.NotCopied++
			case stRunning:
				m.Run++
			default:
				m.Problems++
			}
			if run.Started.After(lastRun) {
				lastRun = run.Started
			}
			m.Pairs = append(m.Pairs, pv)
		}
		m.LastRun, m.LastRunAbs = relTime(now, lastRun), absTime(lastRun)
		out = append(out, m)
	}
	return out, nil
}

// pairView describes one stored pair; run is its latest run (zero if none).
func (u *UI) pairView(ctx context.Context, snap *app.Snapshot, ps state.PairState, now time.Time) (pairView, state.Run, error) {
	k := ps.Key
	pv := pairView{ID: pairID(k), Src: k.SrcForge + ": " + k.SrcRepo, Dst: k.DstForge + ": " + k.DstRepo,
		SrcURL: repoURL(snap, k.SrcForge, k.SrcRepo), DstURL: repoURL(snap, k.DstForge, k.DstRepo), Status: stNever}
	runs, err := u.App.Store.Runs(ctx, k, 1)
	if err != nil {
		return pv, state.Run{}, err
	}
	var run state.Run
	if len(runs) > 0 {
		run = runs[0]
		pv.Status = runStatus(run)
		if run.Status != state.RunOK && run.Status != state.RunRunning && run.Error != "" {
			p := explain(run.Error)
			pv.Problem = &p
		}
	}
	pv.Last, pv.LastAbs = relTime(now, ps.LastSuccess), absTime(ps.LastSuccess)
	return pv, run, nil
}

type dashboardData struct {
	Sites, Mirrors int
	Pairs, OK      int
	Problems       int
	NotCopied      int
	Running        int
	List           []mirrorView
	CanSync        bool
}

func (u *UI) dashboard(w http.ResponseWriter, r *http.Request) {
	ms, err := u.mirrorViews(r.Context())
	if err != nil {
		u.serverError(w, r, err)
		return
	}
	d := dashboardData{Sites: len(u.App.Config().Forges), Mirrors: len(ms), List: ms, CanSync: u.SyncAll != nil}
	for _, m := range ms {
		d.Pairs += len(m.Pairs)
		d.OK += m.OKs
		d.Problems += m.Problems
		d.Running += m.Run
		d.NotCopied += m.NotCopied
		if len(m.Pairs) == 0 {
			d.NotCopied++
		}
	}
	u.render(w, r, "dashboard", View{Title: "Dashboard", Section: "dashboard", Data: d, Live: u.live(d.Running > 0)})
}

type runView struct {
	Started, StartedAbs string
	Trigger             string
	Status              statusView
	Problem             *problem
	Steps               []stepView
}

type stepView struct {
	// Plain is Detail in plain words; "" shows Detail as it is.
	Label, Plain, Detail string
	Status               statusView
}

type pairData struct {
	Mirror string
	Pair   pairView
	Runs   []runView
}

func triggerLabel(t string) string {
	switch t {
	case "schedule":
		return "On schedule or Sync now"
	case "webhook":
		return "Webhook"
	case "cli":
		return "Command line"
	}
	return t
}

func (u *UI) pairPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	k, ok := parsePairID(r.URL.Query().Get("k"))
	if !ok {
		u.notFound(w, r)
		return
	}
	ps, ok, err := u.App.Store.Pair(ctx, k)
	if err != nil {
		u.serverError(w, r, err)
		return
	}
	if !ok {
		u.notFound(w, r)
		return
	}
	now := u.now()
	pv, _, err := u.pairView(ctx, u.App.Snapshot(), ps, now)
	if err != nil {
		u.serverError(w, r, err)
		return
	}
	runs, err := u.App.Store.Runs(ctx, k, 20)
	if err != nil {
		u.serverError(w, r, err)
		return
	}
	d := pairData{Mirror: k.Rule, Pair: pv}
	for _, run := range runs {
		rv := runView{Started: relTime(now, run.Started), StartedAbs: absTime(run.Started),
			Trigger: triggerLabel(run.Trigger), Status: runStatus(run)}
		if run.Error != "" && run.Status != state.RunOK {
			p := explain(run.Error)
			rv.Problem = &p
		}
		for _, s := range run.Steps {
			rv.Steps = append(rv.Steps, stepView{Label: stepLabel(s.Step), Plain: stepDetail(s.Step, s.Detail),
				Detail: s.Detail, Status: stepStatus(s.Status)})
		}
		d.Runs = append(d.Runs, rv)
	}
	u.render(w, r, "pair", View{Title: k.SrcRepo + " → " + k.DstRepo, Section: "dashboard", Data: d,
		Live: u.live(pv.Status == stRunning)})
}

// ---- mirrors list, delete, sync ----

type mirrorsData struct {
	List    []mirrorView
	Sites   int
	CanSync bool
}

func (u *UI) mirrorsPage(w http.ResponseWriter, r *http.Request) {
	ms, err := u.mirrorViews(r.Context())
	if err != nil {
		u.serverError(w, r, err)
		return
	}
	running := false
	for _, m := range ms {
		running = running || m.Run > 0
	}
	u.render(w, r, "mirrors", View{Title: "Mirrors", Section: "mirrors", Live: u.live(running),
		Data: mirrorsData{List: ms, Sites: len(u.App.Config().Forges), CanSync: u.SyncNow != nil}})
}

func (u *UI) deleteMirrorPage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !u.editableMirror(w, r, name) {
		return
	}
	u.render(w, r, "confirm", View{Title: "Delete mirror " + name, Section: "mirrors", Data: confirmData{
		Lines: []string{"The copies already made are kept; gitmirror just stops updating them.",
			"You can delete the copies on their site by hand if you no longer need them."},
		Action: "/mirrors/" + name + "/delete", Button: "Delete mirror", Back: "/mirrors"}})
}

func (u *UI) deleteMirror(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !u.editableMirror(w, r, name) {
		return
	}
	err := u.App.UpdateUI(r.Context(), func(c *app.UIConfig) error { return c.DeleteRule(name) })
	switch {
	case errors.Is(err, config.ErrFileDefined):
		u.fileDefined(w, r, "The mirror "+name)
		return
	case errors.Is(err, app.ErrUnknownEntry):
		u.notFound(w, r)
		return
	case err != nil:
		u.serverError(w, r, err)
		return
	}
	u.log().Info("web UI: mirror deleted", "mirror", name)
	u.setFlash(w, r, FlashSuccess, "Mirror "+name+" deleted. The copies already made are kept.")
	redirect(w, r, "/mirrors")
}

// back returns the form's "back" path when it is a local page, else def.
func back(r *http.Request, def string) string {
	if b := r.PostFormValue("back"); b != "" && safeNext(b) == b {
		return b
	}
	return def
}

func (u *UI) syncMirror(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if u.SyncNow == nil || u.App.Config().RuleOrigin(name) == "" {
		u.notFound(w, r)
		return
	}
	if err := u.SyncNow(name); err != nil {
		u.log().Warn("web UI: sync now", "mirror", name, "err", err)
		u.setFlash(w, r, FlashError, "gitmirror could not start copying "+name+". See the gitmirror log.")
	} else {
		u.markSyncAsked()
		u.setFlash(w, r, FlashInfo, "Copying "+name+" now. This page updates by itself until it is done.")
	}
	redirect(w, r, back(r, "/mirrors"))
}

func (u *UI) syncAll(w http.ResponseWriter, r *http.Request) {
	if u.SyncAll == nil {
		u.notFound(w, r)
		return
	}
	if err := u.SyncAll(); err != nil {
		u.log().Warn("web UI: sync all", "err", err)
		u.setFlash(w, r, FlashError, "gitmirror could not start copying. See the gitmirror log.")
	} else {
		u.markSyncAsked()
		u.setFlash(w, r, FlashInfo, "Copying every mirror now. This page updates by itself until they are done.")
	}
	redirect(w, r, back(r, "/"))
}

// sortedNames returns the keys of m, sorted.
func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
