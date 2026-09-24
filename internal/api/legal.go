package api

import (
	"errors"
	"net/http"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/legal"
	"github.com/davidtorcivia/theses/internal/mail"
)

func (a *API) legalAnswer(w http.ResponseWriter, r *http.Request, value any, err error) {
	if errors.Is(err, legal.ErrInvalid) {
		a.fail(w, 422, err.Error())
		return
	}
	if errors.Is(err, legal.ErrChanged) || errors.Is(err, legal.ErrClosed) || errors.Is(err, legal.ErrRecipientsChanged) {
		a.fail(w, 409, err.Error())
		return
	}
	if errors.Is(err, mail.ErrNotConfigured) {
		a.fail(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	a.workflowAnswer(w, r, value, err)
}

func previewed(hash string) []string {
	if hash == "" {
		return nil
	}
	return []string{hash}
}

type LegalNotification struct {
	Version int64  `json:"version"`
	URL     string `json:"url"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
	// PreviewHash is the preview_hash a preview returned. Notify refuses when
	// the recipients or messages no longer match it; empty skips the check.
	PreviewHash string `json:"preview_hash,omitempty"`
}

func (a *API) legalRoutes(m *http.ServeMux, p string, wrap wrapper) {
	m.HandleFunc("GET "+p+"/legal/releases", wrap(auth.ScopeRead, func(w http.ResponseWriter, r *http.Request, actor core.Actor) {
		out, err := a.Legal.List(r.Context(), actor, id(r, "proposition"))
		a.legalAnswer(w, r, map[string]any{"releases": out}, err)
	}))
	m.HandleFunc("GET "+p+"/legal/releases/{id}", wrap(auth.ScopeRead, func(w http.ResponseWriter, r *http.Request, actor core.Actor) {
		out, err := a.Legal.Get(r.Context(), actor, path(r, "id"))
		a.legalAnswer(w, r, map[string]any{"release": out}, err)
	}))
	m.HandleFunc("POST "+p+"/legal/releases", wrap(auth.ScopeWrite, func(w http.ResponseWriter, r *http.Request, actor core.Actor) {
		var in legal.Release
		if !(&fileAPI{API: a}).read(w, r, &in) {
			return
		}
		out, err := a.Legal.Save(r.Context(), actor, in)
		a.legalAnswer(w, r, map[string]any{"event": out}, err)
	}))
	m.HandleFunc("GET "+p+"/legal/releases/{id}/submissions", wrap(auth.ScopeRead, func(w http.ResponseWriter, r *http.Request, actor core.Actor) {
		out, err := a.Legal.Submissions(r.Context(), actor, path(r, "id"))
		a.legalAnswer(w, r, map[string]any{"submissions": out}, err)
	}))
	m.HandleFunc("POST "+p+"/legal/releases/{id}/preview", wrap(auth.ScopeWrite, func(w http.ResponseWriter, r *http.Request, actor core.Actor) {
		var in LegalNotification
		if !(&fileAPI{API: a}).read(w, r, &in) {
			return
		}
		out, err := a.Legal.Preview(r.Context(), actor, path(r, "id"), in.URL, in.Subject, in.Body)
		a.legalAnswer(w, r, map[string]any{"messages": out, "preview_hash": legal.MessageDigest(out)}, err)
	}))
	m.HandleFunc("POST "+p+"/legal/releases/{id}/notify", wrap(auth.ScopeWrite, func(w http.ResponseWriter, r *http.Request, actor core.Actor) {
		var in LegalNotification
		if !(&fileAPI{API: a}).read(w, r, &in) {
			return
		}
		out, err := a.Legal.Notify(r.Context(), actor, path(r, "id"), in.Version, in.URL, in.Subject, in.Body, previewed(in.PreviewHash)...)
		a.legalAnswer(w, r, map[string]any{"event": out}, err)
	}))
}
