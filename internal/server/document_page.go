package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/davidtorcivia/theses/internal/core"
)

// getDocumentRevisions is the history list behind the document's History link.
// The REST API serves the same rows to a bearer token; a browser has a session
// cookie and no token, so it asks here, and the read goes through the same
// membership test either way.
func (s *Server) getDocumentRevisions(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "no such document")
		return
	}
	list, err := s.docs.History(r.Context(), userOf(r), id)
	if errors.Is(err, core.ErrNotFound) || errors.Is(err, core.ErrForbidden) {
		writeAPIError(w, http.StatusNotFound, "no such document")
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]any{"revisions": list})
}
