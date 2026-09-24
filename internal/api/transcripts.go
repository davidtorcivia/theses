package api

import (
	"net/http"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/workflow"
)

func (a *API) transcriptRoutes(m *http.ServeMux, p string, wrap wrapper) {
	m.HandleFunc("GET "+p+"/files/{id}/transcript", wrap(auth.ScopeRead, func(w http.ResponseWriter, r *http.Request, actor core.Actor) {
		t, err := a.Workflow.Transcript(r.Context(), actor, path(r, "id"))
		a.workflowAnswer(w, r, map[string]any{"transcript": t, "local_transcription": a.Workflow.WhisperURL != ""}, err)
	}))
	m.HandleFunc("PUT "+p+"/files/{id}/transcript", wrap(auth.ScopeWrite, func(w http.ResponseWriter, r *http.Request, actor core.Actor) {
		var in struct {
			Text     string             `json:"text"`
			Format   string             `json:"format"`
			Version  int64              `json:"version"`
			Segments []workflow.Segment `json:"segments"`
		}
		if !a.decode(w, r, workflow.MaxTranscript+65536, false, &in) {
			return
		}
		var err error
		if in.Format != "" {
			in.Segments, err = workflow.ParseTranscript(in.Text, in.Format)
			if err != nil {
				a.workflowAnswer(w, r, nil, err)
				return
			}
		}
		event, err := a.Workflow.SaveTranscript(r.Context(), actor, workflow.Transcript{File: path(r, "id"), Segments: in.Segments, Version: in.Version})
		a.workflowAnswer(w, r, map[string]any{"event": event}, err)
	}))
	m.HandleFunc("GET "+p+"/files/{id}/transcript/export", wrap(auth.ScopeRead, func(w http.ResponseWriter, r *http.Request, actor core.Actor) {
		t, err := a.Workflow.Transcript(r.Context(), actor, path(r, "id"))
		if err != nil {
			a.workflowAnswer(w, r, nil, err)
			return
		}
		format := r.URL.Query().Get("format")
		body, err := workflow.ExportTranscript(t, format)
		if err != nil {
			a.workflowAnswer(w, r, nil, err)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="transcript.`+format+`"`)
		w.Write([]byte(body))
	}))
	m.HandleFunc("GET "+p+"/files/{id}/transcription-jobs", wrap(auth.ScopeRead, func(w http.ResponseWriter, r *http.Request, actor core.Actor) {
		jobs, err := a.Workflow.Jobs(r.Context(), actor, path(r, "id"))
		a.workflowAnswer(w, r, map[string]any{"jobs": jobs}, err)
	}))
	m.HandleFunc("POST "+p+"/files/{id}/transcription-jobs", wrap(auth.ScopeWrite, func(w http.ResponseWriter, r *http.Request, actor core.Actor) {
		var in struct {
			Stereo bool `json:"stereo"`
		}
		if !a.decode(w, r, maxBodyBytes, false, &in) {
			return
		}
		event, err := a.Workflow.QueueTranscription(r.Context(), actor, path(r, "id"), in.Stereo)
		a.workflowAnswer(w, r, map[string]any{"job": event.After, "event": event}, err)
	}))
}
