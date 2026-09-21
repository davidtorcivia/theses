package server

import (
	"net/http"
	"strings"

	agentdocs "github.com/davidtorcivia/theses/docs"
)

var agentDocumentPaths = map[string]bool{
	"/SKILLS.md":      true,
	"/api.md":         true,
	"/connections.md": true,
}

func (s *Server) getAgentDocument(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	http.ServeFileFS(w, r, agentdocs.AgentFiles, strings.TrimPrefix(r.URL.Path, "/"))
}
