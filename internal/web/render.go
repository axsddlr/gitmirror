package web

import (
	"bytes"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
)

// View is what a page handler passes to render.
type View struct {
	// Title is the page heading and the browser tab title.
	Title string
	// Section is the nav item to highlight: "dashboard", "sites",
	// "mirrors", "webhooks", "settings" or "" for none.
	Section string
	// Form is the page's form state (values and errors); may be nil.
	Form *Form
	// Data is anything else the page template needs.
	Data any
	// Status is the HTTP status; 0 means 200.
	Status int
}

// layoutData is the root template data: the View plus what the layout
// needs. Page templates see the View fields directly (.Title, .Form, .Data).
type layoutData struct {
	View
	SignedIn bool
	CSRF     string
	Flashes  []Flash
	NavItems []navItem
}

type navItem struct {
	Path, Label string
	Active      bool
}

// nav is the main navigation, in order.
var nav = []struct{ section, path, label string }{
	{"dashboard", "/", "Dashboard"},
	{"sites", "/sites", "Sites"},
	{"mirrors", "/mirrors", "Mirrors"},
	{"webhooks", "/webhooks", "Webhooks"},
	{"settings", "/settings", "Settings"},
}

// Form holds submitted values and per-field errors so a page can be shown
// again with the user's input and friendly messages.
type Form struct {
	Values url.Values
	// Errors maps a field name to its message.
	Errors map[string]string
	// Message is shown above the form (e.g. "Too many attempts").
	Message string
}

// newForm returns the form of r (its parsed POST body; empty for GET).
func newForm(r *http.Request) *Form {
	f := &Form{Values: url.Values{}, Errors: map[string]string{}}
	if r != nil && r.Method != http.MethodGet && r.Method != http.MethodHead {
		_ = r.ParseForm() // a bad body just yields empty values
		f.Values = r.PostForm
	}
	return f
}

// Get returns the submitted value of name.
func (f *Form) Get(name string) string {
	if f == nil {
		return ""
	}
	return f.Values.Get(name)
}

// SetError records a message for field.
func (f *Form) SetError(field, msg string) { f.Errors[field] = msg }

// Fail sets the message shown above the form.
func (f *Form) Fail(msg string) { f.Message = msg }

// OK reports whether the form has no errors.
func (f *Form) OK() bool { return f.Message == "" && len(f.Errors) == 0 }

// fieldData is the data of the "field" template.
type fieldData struct {
	Name, Label, Type, Value, Error, Autocomplete, Hint string
	// Optional drops the required attribute (see the "optional" func).
	Optional bool
}

// field builds the data of the "field" template:
//
//	{{template "field" (field .Form "name" "Label" "text" "autocomplete" "optional hint")}}
//
// Password fields never echo the submitted value.
func field(f *Form, name, label, typ, autocomplete string, hint ...string) fieldData {
	d := fieldData{Name: name, Label: label, Type: typ, Autocomplete: autocomplete}
	if f != nil {
		d.Error = f.Errors[name]
		if typ != "password" {
			d.Value = f.Get(name)
		}
	}
	if len(hint) > 0 {
		d.Hint = strings.Join(hint, " ")
	}
	return d
}

// optional marks a field as not required:
//
//	{{template "field" (optional (field .Form "name" "Label" "text" "off"))}}
func optional(d fieldData) fieldData {
	d.Optional = true
	return d
}

var funcs = template.FuncMap{
	"field":    field,
	"optional": optional,
	// has reports whether list contains s (checked boxes of multi-value fields).
	"has": func(list []string, s string) bool { return slices.Contains(list, s) },
}

// mustParsePages parses templates/layout.html together with each other
// template file; the page name is the file name without ".html".
func mustParsePages() map[string]*template.Template {
	base := template.Must(template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html"))
	files, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		panic(err)
	}
	pages := map[string]*template.Template{}
	for _, f := range files {
		name := strings.TrimSuffix(path.Base(f), ".html")
		if name == "layout" {
			continue
		}
		pages[name] = template.Must(template.Must(base.Clone()).ParseFS(templateFS, f))
	}
	return pages
}

// render writes page name inside the layout. It adds the sign-in state,
// the CSRF token and any pending flash message. Rendering happens into a
// buffer first, so a template error becomes a clean 500.
func (u *UI) render(w http.ResponseWriter, r *http.Request, name string, v View) {
	t, ok := u.pages[name]
	if !ok {
		u.log().Error("web UI: unknown template", "name", name)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	d := layoutData{View: v, SignedIn: sessionFrom(r) != nil, CSRF: u.csrfToken(w, r), Flashes: u.takeFlashes(w, r)}
	if d.SignedIn {
		for _, n := range nav {
			d.NavItems = append(d.NavItems, navItem{Path: n.path, Label: n.label, Active: n.section == v.Section})
		}
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", d); err != nil {
		u.log().Error("web UI: render", "template", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", fmt.Sprint(buf.Len()))
	status := v.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}
