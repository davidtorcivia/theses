package api

import (
	"errors"
	"net/http"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/workflow"
)

func (a *API) workflowAnswer(w http.ResponseWriter, r *http.Request, value any, err error) {
	if errors.Is(err, workflow.ErrTranscriptionUnavailable) {
		a.fail(w, 503, err.Error())
		return
	}
	if errors.Is(err, workflow.ErrInvalid) {
		a.fail(w, 422, err.Error())
		return
	}
	if errors.Is(err, workflow.ErrChanged) {
		a.fail(w, 409, err.Error())
		return
	}
	if err != nil {
		(&fileAPI{API: a}).refuse(w, r, err)
		return
	}
	a.writeJSON(w, 200, value)
}
func (a *API) workflowRoutes(m *http.ServeMux, p string, wrap wrapper) {
	m.HandleFunc("GET "+p+"/trash", wrap(auth.ScopeRead, func(w http.ResponseWriter, r *http.Request, actor core.Actor) {
		rows, err := a.Board.Trash(r.Context(), actor, id(r, "proposition"))
		a.workflowAnswer(w, r, map[string]any{"items": rows}, err)
	}))
	m.HandleFunc("POST "+p+"/trash/{id}/restore", wrap(auth.ScopeWrite, func(w http.ResponseWriter, r *http.Request, actor core.Actor) {
		item, err := a.Board.DeletedItem(r.Context(), actor, path(r, "id"))
		if err != nil {
			a.workflowAnswer(w, r, nil, err)
			return
		}
		if p, ok := PrincipalFrom(r.Context()); ok && item.Entity == "file" {
			if why := p.Deny(auth.ScopeFiles); why != "" {
				a.fail(w, 403, why)
				return
			}
		}
		event, err := a.Board.RestoreDeleted(r.Context(), actor, path(r, "id"))
		a.workflowAnswer(w, r, map[string]any{"event": event}, err)
	}))

	m.HandleFunc("GET "+p+"/calendar-entries", wrap(auth.ScopeRead, func(w http.ResponseWriter, r *http.Request, actor core.Actor) {
		entries, err := a.Workflow.CalendarEntries(r.Context(), actor)
		a.workflowAnswer(w, r, map[string]any{"entries": entries}, err)
	}))
	m.HandleFunc("POST "+p+"/calendar-events", wrap(auth.ScopeWrite, func(w http.ResponseWriter, r *http.Request, actor core.Actor) {
		var in workflow.CalendarEntry
		if !(&fileAPI{API: a}).read(w, r, &in) {
			return
		}
		event, err := a.Workflow.SaveCalendarEvent(r.Context(), actor, in)
		a.workflowAnswer(w, r, map[string]any{"event": event}, err)
	}))
	m.HandleFunc("DELETE "+p+"/calendar-events/{id}", wrap(auth.ScopeWrite, func(w http.ResponseWriter, r *http.Request, actor core.Actor) {
		event, err := a.Workflow.DeleteCalendarEvent(r.Context(), actor, path(r, "id"), id(r, "version"))
		a.workflowAnswer(w, r, map[string]any{"event": event}, err)
	}))
	m.HandleFunc("POST "+p+"/calendar-tasks", wrap(auth.ScopeWrite, func(w http.ResponseWriter, r *http.Request, actor core.Actor) {
		var in struct {
			Title  string `json:"title"`
			Date   string `json:"date"`
			Column int64  `json:"column"`
		}
		if !(&fileAPI{API: a}).read(w, r, &in) {
			return
		}
		event, err := a.Board.CreateCalendarTask(r.Context(), actor, in.Column, in.Title, in.Date)
		a.workflowAnswer(w, r, map[string]any{"event": event}, err)
	}))

	m.HandleFunc("DELETE "+p+"/evidence/{id}", wrap(auth.ScopeWrite, func(w http.ResponseWriter, r *http.Request, actor core.Actor) {
		event, err := a.Workflow.DeleteEvidence(r.Context(), actor, path(r, "id"), id(r, "version"))
		a.workflowAnswer(w, r, map[string]any{"event": event}, err)
	}))

	m.HandleFunc("GET "+p+"/snapshots/{id}", wrap(auth.ScopeRead, func(w http.ResponseWriter, r *http.Request, actor core.Actor) {
		snapshot, err := a.Workflow.Snapshot(r.Context(), actor, path(r, "id"))
		a.workflowAnswer(w, r, map[string]any{"snapshot": snapshot}, err)
	}))

	m.HandleFunc("PATCH "+p+"/files/{id}/comments/{comment}", wrap(auth.ScopeWrite, func(w http.ResponseWriter, r *http.Request, actor core.Actor) {
		var in struct {
			Resolved bool  `json:"resolved"`
			Version  int64 `json:"version"`
		}
		if !(&fileAPI{API: a}).read(w, r, &in) {
			return
		}
		event, err := a.Files.ResolveComment(r.Context(), actor, path(r, "id"), path(r, "comment"), in.Version, in.Resolved)
		a.workflowAnswer(w, r, map[string]any{"file": event.After, "event": event}, err)
	}))

	m.HandleFunc("GET "+p+"/propositions/{id}/production-plan", wrap(auth.ScopeRead, func(w http.ResponseWriter, r *http.Request, actor core.Actor) {
		if a.Workflow == nil {
			http.NotFound(w, r)
			return
		}
		if err := a.Workflow.Readable(r.Context(), actor, path(r, "id")); err != nil {
			a.workflowAnswer(w, r, nil, err)
			return
		}
		plan, err := board.GetProductionPlan(r.Context(), a.db, path(r, "id"))
		a.workflowAnswer(w, r, map[string]any{"plan": plan}, err)
	}))
	m.HandleFunc("GET "+p+"/documents/{id}/snapshots", wrap(auth.ScopeRead, func(w http.ResponseWriter, r *http.Request, actor core.Actor) {
		rows, err := a.Workflow.Snapshots(r.Context(), actor, path(r, "id"))
		a.workflowAnswer(w, r, map[string]any{"snapshots": rows}, err)
	}))
	m.HandleFunc("POST "+p+"/documents/{id}/snapshots", wrap(auth.ScopeWrite, func(w http.ResponseWriter, r *http.Request, actor core.Actor) {
		var in struct {
			Cues string `json:"cues"`
		}
		if !(&fileAPI{API: a}).read(w, r, &in) {
			return
		}
		event, err := a.Workflow.Pin(r.Context(), actor, path(r, "id"), in.Cues)
		a.workflowAnswer(w, r, map[string]any{"snapshot": event.After, "event": event}, err)
	}))
	m.HandleFunc("GET "+p+"/reviews", wrap(auth.ScopeRead, func(w http.ResponseWriter, r *http.Request, actor core.Actor) {
		rows, err := a.Workflow.Reviews(r.Context(), actor, id(r, "proposition"))
		a.workflowAnswer(w, r, map[string]any{"reviews": rows}, err)
	}))
	m.HandleFunc("POST "+p+"/reviews", wrap(auth.ScopeWrite, func(w http.ResponseWriter, r *http.Request, actor core.Actor) {
		var in struct {
			Document int64 `json:"document_id"`
			File     int64 `json:"file_id"`
			Reviewer int64 `json:"reviewer_id"`
		}
		if !(&fileAPI{API: a}).read(w, r, &in) {
			return
		}
		event, err := a.Workflow.RequestReview(r.Context(), actor, in.Document, in.File, in.Reviewer)
		a.workflowAnswer(w, r, map[string]any{"review": event.After, "event": event}, err)
	}))
	m.HandleFunc("PATCH "+p+"/reviews/{id}", wrap(auth.ScopeWrite, func(w http.ResponseWriter, r *http.Request, actor core.Actor) {
		var in struct {
			State   string `json:"state"`
			Note    string `json:"note"`
			Version int64  `json:"version"`
		}
		if !(&fileAPI{API: a}).read(w, r, &in) {
			return
		}
		event, err := a.Workflow.Decide(r.Context(), actor, path(r, "id"), in.Version, in.State, in.Note)
		a.workflowAnswer(w, r, map[string]any{"review": event.After, "event": event}, err)
	}))
	m.HandleFunc("GET "+p+"/evidence", wrap(auth.ScopeRead, func(w http.ResponseWriter, r *http.Request, actor core.Actor) {
		rows, err := a.Workflow.Evidence(r.Context(), actor, id(r, "proposition"))
		a.workflowAnswer(w, r, map[string]any{"evidence": rows}, err)
	}))
	m.HandleFunc("PUT "+p+"/evidence", wrap(auth.ScopeWrite, func(w http.ResponseWriter, r *http.Request, actor core.Actor) {
		var in workflow.Evidence
		if !(&fileAPI{API: a}).read(w, r, &in) {
			return
		}
		event, err := a.Workflow.SaveEvidence(r.Context(), actor, in)
		a.workflowAnswer(w, r, map[string]any{"evidence": event.After, "event": event}, err)
	}))
	m.HandleFunc("GET "+p+"/evidence/export", wrap(auth.ScopeRead, func(w http.ResponseWriter, r *http.Request, actor core.Actor) {
		rows, err := a.Workflow.Evidence(r.Context(), actor, id(r, "proposition"))
		if err != nil {
			a.workflowAnswer(w, r, nil, err)
			return
		}
		format := r.URL.Query().Get("format")
		body, err := workflow.ExportEvidence(rows, format)
		if err != nil {
			a.workflowAnswer(w, r, nil, err)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="references.`+format+`"`)
		w.Write([]byte(body))
	}))
}
