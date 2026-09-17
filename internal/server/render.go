package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"sort"
	"strings"
)

// Palette is the eight colours a person can be. The mockup fixed this list, and
// fixing it here is what lets the CSS carry the colours instead of a style
// attribute, which a strict CSP would refuse.
var Palette = []string{"#1100ff", "#d0021b", "#0a8a3a", "#b35c00", "#7b2cbf", "#008b8b", "#c2185b", "#111"}

// layoutFor says which layout each page is wrapped in.
var layoutFor = map[string]string{
	"login.html":     "auth.html",
	"setup.html":     "auth.html",
	"enrol.html":     "auth.html",
	"invite.html":    "auth.html",
	"reset.html":     "auth.html",
	"reset_new.html": "auth.html",
	"error.html":     "auth.html",
	"shell.html":     "app.html",
	"settings.html":  "app.html",
	"profile.html":   "app.html",
}

// assets serves web/static under one content-hashed prefix. One hash for the
// whole tree keeps relative URLs inside the CSS working and makes any change
// bust every cached URL, which at this size costs nothing.
type assets struct {
	prefix  string
	handler http.Handler
	dev     bool
}

func newAssets(fsys fs.FS, dev bool) (*assets, error) {
	mime.AddExtensionType(".woff2", "font/woff2") // not in Go's built-in table

	hash := "dev"
	if !dev {
		h, err := treeHash(fsys)
		if err != nil {
			return nil, err
		}
		hash = h
	}
	a := &assets{prefix: "/static/" + hash + "/", dev: dev}
	a.handler = http.StripPrefix(a.prefix, http.FileServerFS(fsys))
	return a, nil
}

// URL is the {{asset}} template function.
func (a *assets) URL(name string) string { return a.prefix + name }

func (a *assets) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, a.prefix) {
		// An older page asking for a hash that is no longer current.
		http.NotFound(w, r)
		return
	}
	if a.dev {
		w.Header().Set("Cache-Control", "no-store")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	a.handler.ServeHTTP(w, r)
}

func treeHash(fsys fs.FS) (string, error) {
	var names []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			names = append(names, p)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(names)

	sum := sha256.New()
	for _, n := range names {
		fmt.Fprintf(sum, "%s\x00", n)
		f, err := fsys.Open(n)
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(sum, f); err != nil {
			f.Close()
			return "", err
		}
		f.Close()
	}
	return hex.EncodeToString(sum.Sum(nil))[:12], nil
}

// templates parses each page together with the layout it names and the shared
// partials, so every page can define "content" under its own layout.
func parseTemplates(fsys fs.FS, funcs template.FuncMap) (map[string]*template.Template, error) {
	out := map[string]*template.Template{}
	for page, layout := range layoutFor {
		t, err := template.New(page).Funcs(funcs).ParseFS(fsys,
			"layouts/"+layout, "partials/*.html", "pages/"+page)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", page, err)
		}
		out[page] = t
	}
	return out, nil
}

func (s *Server) funcs() template.FuncMap {
	return template.FuncMap{
		"asset":  s.assets.URL,
		"join":   strings.Join,
		"colour": colourClass,
		"firstName": func(name string) string {
			first, _, _ := strings.Cut(name, " ")
			return first
		},
	}
}

// colourClass maps a stored colour to its class, because the CSP forbids the
// style attribute the mockup used.
func colourClass(hex string) string {
	for i, c := range Palette {
		if strings.EqualFold(c, hex) {
			return fmt.Sprintf("c%d", i+1)
		}
	}
	return "c8"
}

// render writes a page. In dev the templates are reparsed first, so editing one
// needs no restart.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page string, data map[string]any) {
	set := s.templates
	if s.dev {
		fresh, err := parseTemplates(s.templateFS, s.funcs())
		if err != nil {
			s.fail(w, r, fmt.Errorf("reparse templates: %w", err))
			return
		}
		set = fresh
	}
	t, ok := set[page]
	if !ok {
		s.fail(w, r, fmt.Errorf("no template %s", page))
		return
	}

	// Render to memory so a template error does not land half a page with a 200.
	var buf strings.Builder
	if err := t.ExecuteTemplate(&buf, "layout", data); err != nil {
		s.fail(w, r, fmt.Errorf("render %s: %w", page, err))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	io.WriteString(w, buf.String())
}

// Error pages, as the plan wants them: the code, one line, one action, and no
// link to anything internal.
var errorPages = map[int]struct{ code, headline, lead, actText, actHref string }{
	http.StatusNotFound:            {"404", "Not found.", "", "Sign in", "/login"},
	http.StatusForbidden:           {"403", "No access.", "", "Sign in as someone else", "/login"},
	http.StatusInternalServerError: {"500", "Something broke.", "", "Try again", "/"},
}

var errorArt = map[int][]string{
	http.StatusNotFound:            {"22"},
	http.StatusForbidden:           {"04"},
	http.StatusInternalServerError: {"10"},
}

func (s *Server) errorPage(w http.ResponseWriter, r *http.Request, status int) {
	p, ok := errorPages[status]
	if !ok {
		p = errorPages[http.StatusInternalServerError]
	}
	s.render(w, r, status, "error.html", map[string]any{
		"Title":      p.code,
		"Code":       p.code,
		"Headline":   p.headline,
		"Lead":       p.lead,
		"ActionText": p.actText,
		"ActionHref": p.actHref,
		"ArtFrames":  errorArt[status],
	})
}

func (s *Server) offlinePage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "error.html", map[string]any{
		"Title":      "Offline",
		"Headline":   "You’re offline.",
		"Lead":       "Your changes are kept on this device and sync when the connection is back.",
		"ActionText": "Retry",
		"ActionHref": "/",
		"ArtFrames":  []string{"19"},
	})
}

// fail is the one place a handler's error becomes a response. The fallback is
// plain text, so a broken template still gets an answer out.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusInternalServerError)
	io.WriteString(w, brokenPage)
}

// brokenPage is what 500 falls back to when even the error template will not
// render. It carries no markup the CSP would refuse and links to nothing.
const brokenPage = `<!doctype html><html lang="en"><head><meta charset="utf-8">` +
	`<title>500 · THESES</title></head><body><h1>500</h1><p>Something broke.</p></body></html>`
