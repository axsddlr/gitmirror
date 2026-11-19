package web

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"gitmirror/internal/config"
	"gitmirror/internal/state"
)

// Plain-words helpers: everything the pages say about sites, mirrors and
// problems goes through here so the wording stays consistent.

// kindLabel names a forge type for people.
func kindLabel(typ string) string {
	switch typ {
	case "github":
		return "GitHub"
	case "gitea":
		return "Gitea or Forgejo"
	}
	return typ
}

// problem is a plain-words explanation of an error, with the original text
// kept for a "Technical details" reveal.
type problem struct {
	Text   string
	Detail string
}

const allowExistingOption = "“Allow copying into repositories that already exist”"

// explanations map a (lowercase) fragment of a known error to plain words.
// The first match wins, so more specific fragments come first.
var explanations = []struct {
	frags []string
	text  string
}{
	{[]string{"already exists and was not created by gitmirror"},
		"A repository with this name already exists at the destination and gitmirror did not create it, so it was left untouched. " +
			"To copy into it anyway, edit the mirror and tick " + allowExistingOption + ". Its content will then be replaced to match the original."},
	{[]string{"is not the repository gitmirror"},
		"The repository at the destination is not the one gitmirror made (it was deleted and created again, or the site's address changed), " +
			"so gitmirror stopped to be safe. If that is expected, edit the mirror and tick " + allowExistingOption + "."},
	{[]string{"source has no refs but previously had"},
		"The original repository is suddenly empty, although it had branches before. To protect the copy, gitmirror did not change it. " +
			"If the original was emptied on purpose, delete the copy by hand or remove the mirror."},
	{[]string{"refusing to push. if the source really dropped them", "destination refs (guard"},
		"This update would delete many branches or tags from the copy at once, so gitmirror stopped to be safe. " +
			"If they really were removed from the original, an administrator can run `gitmirror sync --allow-prune` once to allow it."},
	{[]string{"source and destination are the same"},
		"The original and the copy would be the same repository. Choose another destination account or site."},
	{[]string{"is already the destination of rule", "is targeted by multiple sources"},
		"Another mirror already copies into the same place. Choose another destination account, or the naming “Prefix with the original owner”."},
	{[]string{"mirror loop"},
		"This would copy two repositories into each other in a loop. Choose another destination."},
	{[]string{"duplicate rule name"},
		"That name is already used by another mirror. Choose another name."},
	{[]string{"invalid rule name"},
		"Use only lowercase letters, digits, - and _ in the mirror name, starting with a letter or digit."},
	{[]string{"to.owner", "to.repo", "to.name", "destination for "},
		"The destination account or repository name is not allowed. Use letters, digits, '.', '-' and '_' only."},
	{[]string{"status 401", " 401 ", "bad credentials", "unauthorized", "401 unauthorized"},
		"The site refused the token. It may have expired or been revoked; update it on the Sites page."},
	{[]string{"status 403", " 403 ", "forbidden", "resource not accessible", "must have admin rights", "permission"},
		"The token is not allowed to do this. Check the token permissions listed on the Sites page and give the token the missing ones."},
	{[]string{"source repository not found"},
		"The original repository was not found. It may have been deleted, renamed or made private, or the token cannot see it."},
	{[]string{"list repos of", "list starred by"},
		"gitmirror could not list the repositories of this account. Check the account name and that the token can see it."},
	{[]string{"rate limit"},
		"The site's limit on requests was reached. gitmirror will try again later by itself."},
	{[]string{"push rejected", "rejected"},
		"The destination refused the update, for example because of a protected branch. Check the branch protection settings there."},
	{[]string{"interrupted", "context canceled"},
		"The copy was interrupted because gitmirror stopped. It runs again on the next schedule."},
	// A git command that ran past gitmirror's own time limit is killed; that
	// is slowness (large files, a slow link), not an unreachable site.
	{[]string{"deadline exceeded (signal: killed)"},
		"A step of the copy took longer than its time limit and was stopped. This happens with very large repositories or large files (LFS) on a slow connection. The copy is tried again on the next schedule and continues where it can; if it keeps failing, untick \"Large files (LFS)\" in the mirror's options."},
	{[]string{"no such host", "connection refused", "i/o timeout", "deadline exceeded", "timeout", "unreachable", "certificate", "tls:"},
		"gitmirror could not reach the site. Check that it is online and that its address is right."},
}

// explain turns an error message from the engine, resolve or config
// validation into plain words. Unknown messages get a generic sentence; the
// original is always kept in Detail.
func explain(msg string) problem {
	l := strings.ToLower(msg)
	p := problem{Detail: truncate(msg, 800)}
	for _, e := range explanations {
		for _, f := range e.frags {
			if strings.Contains(l, f) {
				p.Text = e.text
				return p
			}
		}
	}
	p.Text = "Something went wrong. The technical details below may help."
	return p
}

// configProblems explains every distinct problem of a validation error.
func configProblems(err error) []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range splitErr(err) {
		t := explain(l).Text
		if strings.HasPrefix(t, "Something went wrong") {
			t = "gitmirror cannot save this: " + stripLoc(l)
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// stripLoc removes a leading "rules[3] (name): " location from a message.
func stripLoc(s string) string {
	if strings.HasPrefix(s, "rules[") {
		if i := strings.Index(s, "): "); i >= 0 {
			return s[i+3:]
		}
	}
	return s
}

func splitErr(err error) []string {
	var out []string
	var walk func(error)
	walk = func(e error) {
		if j, ok := e.(interface{ Unwrap() []error }); ok {
			for _, s := range j.Unwrap() {
				walk(s)
			}
			return
		}
		for _, l := range strings.Split(e.Error(), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				out = append(out, l)
			}
		}
	}
	walk(err)
	return out
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// connectionProblem explains a failed connection test of a site at addr
// ("" for github.com). token is scrubbed from any detail shown.
func connectionProblem(err error, addr, token string) string {
	if addr == "" {
		addr = "https://github.com"
	}
	l := strings.ToLower(err.Error())
	var ne net.Error
	var ue *url.Error
	var oe *net.OpError
	var de *net.DNSError
	switch {
	case strings.Contains(l, "401") || strings.Contains(l, "bad credentials") || strings.Contains(l, "unauthorized"):
		return "The token was refused. Check that you copied all of it."
	case strings.Contains(l, "403") || strings.Contains(l, "forbidden"):
		return "The token was accepted but may not read your account. Give it the permissions listed below and try again."
	case errors.As(err, &de) || errors.As(err, &oe) || errors.As(err, &ne) || errors.As(err, &ue) ||
		strings.Contains(l, "no such host") || strings.Contains(l, "connection refused") || strings.Contains(l, "timeout") ||
		strings.Contains(l, "deadline exceeded") || strings.Contains(l, "certificate"):
		return "Could not reach " + addr + " — check the address."
	case strings.Contains(l, "404") || strings.Contains(l, "not found"):
		return "No site of this kind answered at " + addr + ". Check the address and the kind of site."
	}
	d := err.Error()
	if token != "" {
		d = strings.ReplaceAll(d, token, "[hidden]")
	}
	return "The connection test failed: " + truncate(d, 300)
}

// statusView is a status in words and colour (the class); the icon and the
// words carry the meaning, the colour only repeats it.
type statusView struct {
	Class, Icon, Label string
	rank               int // higher is worse
}

var (
	stOK      = statusView{"ok", "✓", "OK", 0}
	stNever   = statusView{"muted", "○", "Not copied yet", 1}
	stRunning = statusView{"info", "↻", "Copying now", 2}
	stPartial = statusView{"warn", "!", "Some parts failed", 3}
	stStopped = statusView{"warn", "■", "Stopped for safety", 4}
	stIntr    = statusView{"warn", "■", "Interrupted", 4}
	stFailed  = statusView{"bad", "✕", "Failed", 5}
)

// runStatus is the status of a run.
func runStatus(r state.Run) statusView {
	switch r.Status {
	case state.RunRunning:
		return stRunning
	case state.RunOK:
		return stOK
	case state.RunPartial:
		return stPartial
	case state.RunFailed:
		return stFailed
	case state.RunAborted:
		if l := strings.ToLower(r.Error); strings.Contains(l, "interrupted") || strings.Contains(l, "context canceled") {
			return stIntr
		}
		return stStopped
	}
	return stNever
}

func worse(a, b statusView) statusView {
	if b.rank > a.rank {
		return b
	}
	return a
}

// stepStatus names a step outcome.
func stepStatus(s state.StepStatus) statusView {
	switch s {
	case state.StepOK:
		return statusView{"ok", "✓", "Done", 0}
	case state.StepUnchanged:
		return statusView{"ok", "✓", "Already up to date", 0}
	case state.StepSkipped:
		return statusView{"muted", "–", "Skipped", 0}
	case state.StepFailed:
		return statusView{"bad", "✕", "Failed", 0}
	}
	return statusView{"muted", "?", string(s), 0}
}

// stepLabel names an engine step.
func stepLabel(step string) string {
	switch step {
	case "prepare":
		return "Checks before copying"
	case "git":
		return "Code (branches and tags)"
	case "lfs":
		return "Large files (LFS)"
	case "settings":
		return "Description and settings"
	case "releases":
		return "Releases"
	case "wiki":
		return "Wiki"
	case "finalize":
		return "Archive state"
	}
	return step
}

var (
	refCountsRe     = regexp.MustCompile(`^(\d+) created, (\d+) updated, (\d+) deleted`)
	releaseCountsRe = regexp.MustCompile(`^(\d+) created, (\d+) updated, (\d+) deleted releases; (\d+) assets uploaded \((\d+) replaced\), (\d+) assets deleted`)
	upToDateRe      = regexp.MustCompile(`^\d+ refs up to date$`)
	lfsCountRe      = regexp.MustCompile(`^(\d+) objects in cache$`)
)

// counted joins the non-zero counts as "2 releases added, 1 updated": the
// first one carries the noun (one: singular, else plural). "" when all are
// zero.
func counted(one, plural string, counts []string, verbs ...string) string {
	var parts []string
	for i, c := range counts {
		if c == "0" {
			continue
		}
		if len(parts) == 0 {
			noun := plural
			if c == "1" {
				noun = one
			}
			parts = append(parts, c+" "+noun+" "+verbs[i])
		} else {
			parts = append(parts, c+" "+verbs[i])
		}
	}
	return strings.Join(parts, ", ")
}

// stepDetail puts the detail the engine recorded for a step in plain
// words; "" when there is no plain version (show the detail as it is).
func stepDetail(step, detail string) string {
	switch {
	case detail == "":
		return ""
	case upToDateRe.MatchString(detail):
		return "No changes"
	case detail == "source is empty; nothing to push":
		return "The original is empty, so there is nothing to copy"
	case step == "prepare" && strings.HasPrefix(detail, "destination created"):
		return "The copy was created"
	case step == "prepare" && strings.HasPrefix(detail, "destination adopted"):
		return "The repository that already existed is now kept up to date"
	case step == "prepare" && strings.HasPrefix(detail, "destination exists"):
		return "The copy is there"
	}
	if m := releaseCountsRe.FindStringSubmatch(detail); m != nil {
		s := counted("release", "releases", m[1:4], "added", "updated", "removed")
		if f := counted("file", "files", m[4:5], "uploaded"); f != "" {
			s = strings.TrimPrefix(s+", "+f, ", ")
		}
		if s == "" {
			return "No changes"
		}
		return s
	}
	if m := refCountsRe.FindStringSubmatch(detail); m != nil && (step == "git" || step == "wiki") {
		s := counted("branch or tag", "branches or tags", m[1:4], "added", "updated", "removed")
		switch {
		case s == "":
			return "No changes"
		case step == "wiki":
			return "The wiki was updated"
		}
		return s
	}
	if m := lfsCountRe.FindStringSubmatch(detail); m != nil {
		if m[1] == "1" {
			return "1 large file kept"
		}
		return m[1] + " large files kept"
	}
	return ""
}

// relTime says how long ago t was, relative to now.
func relTime(now, t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := now.Sub(t)
	plural := func(n int, unit string) string {
		if n == 1 {
			return "1 " + unit + " ago"
		}
		return fmt.Sprintf("%d %ss ago", n, unit)
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d/time.Minute), "minute")
	case d < 24*time.Hour:
		return plural(int(d/time.Hour), "hour")
	case d < 30*24*time.Hour:
		return plural(int(d/(24*time.Hour)), "day")
	}
	return t.Local().Format("2 Jan 2006")
}

// absTime is t as a full date and time, for title attributes.
func absTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("2 Jan 2006, 15:04:05 MST")
}

// intervals are the schedule choices of the mirror form.
var intervals = []struct{ Value, Label string }{
	{"15m", "Every 15 minutes"},
	{"1h", "Every hour"},
	{"6h", "Every 6 hours"},
	{"24h", "Every day"},
}

// everyLabel says how often an interval runs.
func everyLabel(d time.Duration) string {
	switch d {
	case 15 * time.Minute:
		return "every 15 minutes"
	case time.Hour:
		return "every hour"
	case 6 * time.Hour:
		return "every 6 hours"
	case 24 * time.Hour:
		return "every day"
	}
	s := strings.TrimSuffix(d.String(), "0s") // "2h0m0s" -> "2h0m", "30m0s" -> "30m"
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return "every " + s
}

// fromLabel says what a rule copies, e.g. "From github: alice/tool".
func fromLabel(r config.Rule) string {
	s := "From " + r.From.Forge + ": "
	switch {
	case r.From.Repo != "":
		return s + r.From.Repo
	case r.From.StarredBy != "":
		return s + "everything starred by " + r.From.StarredBy
	}
	if names, ok := pickedNames(r); ok {
		if len(names) == 1 {
			return s + r.From.Owner + "/" + names[0]
		}
		return fmt.Sprintf("%s%d chosen repositories of %s", s, len(names), r.From.Owner)
	}
	return s + "everything from " + r.From.Owner
}

// toLabel says where a rule copies to, e.g. "To gitea: carol".
func toLabel(r config.Rule) string {
	s := "To " + r.To.Forge + ": "
	if r.To.Repo != "" {
		return s + r.To.Repo
	}
	s += r.To.Owner
	switch r.To.Name {
	case "", "{{.Name}}":
	case "{{.Owner}}-{{.Name}}":
		s += " (names prefixed with the original owner)"
	default:
		s += " (named " + r.To.Name + ")"
	}
	return s
}
