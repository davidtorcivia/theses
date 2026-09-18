package api

import (
	"net/http"

	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/files"
)

// A linkView is a link with the line the drawer copies. The citation is built
// from the fields rather than stored, so a corrected author appears in it
// without the row being touched twice.
type linkView struct {
	files.Link
	Citation string `json:"citation"`
}

func view(l files.Link) linkView {
	return linkView{Link: l, Citation: files.Citation(l)}
}

func (f *fileAPI) listLinks(w http.ResponseWriter, r *http.Request, a core.Actor) {
	rows, err := f.svc.ListLinks(r.Context(), a, id(r, "proposition"))
	if err != nil {
		f.refuse(w, r, err)
		return
	}
	out := make([]linkView, 0, len(rows))
	for _, l := range rows {
		out = append(out, view(l))
	}
	f.writeJSON(w, http.StatusOK, map[string]any{"links": out, "kinds": files.Kinds})
}

func (f *fileAPI) getLink(w http.ResponseWriter, r *http.Request, a core.Actor) {
	l, err := f.svc.ReadLink(r.Context(), a, path(r, "id"))
	if err != nil {
		f.refuse(w, r, err)
		return
	}
	f.writeJSON(w, http.StatusOK, view(l))
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

func (f *fileAPI) editLink(w http.ResponseWriter, r *http.Request, a core.Actor) {
	var in struct {
		Title    string `json:"title"`
		Author   string `json:"author"`
		Year     string `json:"year"`
		Kind     string `json:"kind"`
		Note     string `json:"note_md"`
		Question string `json:"question"`
	}
	if !f.read(w, r, &in) {
		return
	}
	e, err := f.svc.EditLink(r.Context(), a, path(r, "id"), files.Edit{
		Title: in.Title, Author: in.Author, Year: in.Year,
		Kind: in.Kind, Note: in.Note, Question: in.Question,
	})
	if err != nil {
		f.refuse(w, r, err)
		return
	}
	f.answerLink(w, r, a, e)
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
	if _, err := f.svc.DeleteLink(r.Context(), a, path(r, "id")); err != nil {
		f.refuse(w, r, err)
		return
	}
	f.writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// answerLink returns the row a command left, read back rather than taken from
// the event so that the citation is built the one way.
func (f *fileAPI) answerLink(w http.ResponseWriter, r *http.Request, a core.Actor, e core.Event) {
	l, err := f.svc.ReadLink(r.Context(), a, e.EntityID)
	if err != nil {
		f.refuse(w, r, err)
		return
	}
	f.writeJSON(w, http.StatusOK, view(l))
}
