package api

import (
	"net/http"

	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/files"
)

func (f *fileAPI) listLinks(w http.ResponseWriter, r *http.Request, a core.Actor) {
	rows, err := f.svc.ListLinks(r.Context(), a, id(r, "proposition"))
	if err != nil {
		f.refuse(w, r, err)
		return
	}
	f.writeJSON(w, http.StatusOK, map[string]any{"links": rows, "kinds": files.Kinds})
}

func (f *fileAPI) getLink(w http.ResponseWriter, r *http.Request, a core.Actor) {
	l, err := f.svc.ReadLink(r.Context(), a, path(r, "id"))
	if err != nil {
		f.refuse(w, r, err)
		return
	}
	f.writeJSON(w, http.StatusOK, l)
}

// addLink takes a URL and reads the page before it answers, so the row the
// client gets back already carries the title, the author and the year.
func (f *fileAPI) addLink(w http.ResponseWriter, r *http.Request, a core.Actor) {
	var in struct {
		Proposition int64  `json:"proposition"`
		URL         string `json:"url"`
	}
	if !f.read(w, r, &in) {
		return
	}
	e, err := f.svc.AddLink(r.Context(), a, in.Proposition, in.URL)
	if err != nil {
		f.refuse(w, r, err)
		return
	}
	f.answerLink(w, r, a, e)
}

// editLink changes only the fields the body names.
func (f *fileAPI) editLink(w http.ResponseWriter, r *http.Request, a core.Actor) {
	var in files.LinkPatch
	if !f.read(w, r, &in) {
		return
	}
	e, err := f.svc.PatchLink(r.Context(), a, path(r, "id"), in)
	if err != nil {
		f.refuse(w, r, err)
		return
	}
	f.answerLink(w, r, a, e)
}

// some is a nullable column as the command takes it: the empty string is how
// "no question" is written.
func some(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func (f *fileAPI) refetchLink(w http.ResponseWriter, r *http.Request, a core.Actor) {
	e, err := f.svc.RefetchLink(r.Context(), a, path(r, "id"))
	if err != nil {
		f.refuse(w, r, err)
		return
	}
	f.answerLink(w, r, a, e)
}

func (f *fileAPI) deleteLink(w http.ResponseWriter, r *http.Request, a core.Actor) {
	e, err := f.svc.DeleteLink(r.Context(), a, path(r, "id"))
	if err != nil {
		f.refuse(w, r, err)
		return
	}
	f.writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "event": e})
}

// answerLink returns the row a command left, read back rather than taken from
// the event so that the citation is built the one way.
func (f *fileAPI) answerLink(w http.ResponseWriter, r *http.Request, a core.Actor, e core.Event) {
	l, err := f.svc.ReadLink(r.Context(), a, e.EntityID)
	if err != nil {
		f.refuse(w, r, err)
		return
	}
	f.writeJSON(w, http.StatusOK, struct {
		files.Link
		Event core.Event `json:"event"`
	}{l, e})
}
