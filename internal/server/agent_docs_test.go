package server

import (
	"io"
	"net/http"
	"strings"
	"testing"

	agentdocs "github.com/davidtorcivia/theses/docs"
)

func TestPublicAgentDocuments(t *testing.T) {
	h := newHarness(t)
	for _, setup := range []bool{false, true} {
		if setup {
			h.setupOwner()
		}
		for path := range agentDocumentPaths {
			want, err := agentdocs.AgentFiles.ReadFile(strings.TrimPrefix(path, "/"))
			if err != nil {
				t.Fatal(err)
			}
			for _, method := range []string{"GET", "HEAD"} {
				req, err := http.NewRequest(method, h.http.URL+path, nil)
				if err != nil {
					t.Fatal(err)
				}
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				raw, err := io.ReadAll(res.Body)
				res.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				if res.StatusCode != 200 || res.Request.URL.Path != path || res.Header.Get("Content-Type") != "text/markdown; charset=utf-8" || res.Header.Get("Set-Cookie") != "" || res.Header.Get("X-Content-Type-Options") != "nosniff" {
					t.Fatalf("%s %s setup=%v: %d %v", method, path, setup, res.StatusCode, res.Header)
				}
				if method == "GET" && string(raw) != string(want) || method == "HEAD" && len(raw) != 0 {
					t.Fatalf("unexpected %s body for %s", method, path)
				}
			}
		}
	}
	_, profile := h.get("/profile")
	for _, part := range []string{`href="/SKILLS.md"`, `href="/api.md"`, `id="agent-guide-url"`, h.srv.cfg.BaseURL + "/SKILLS.md"} {
		if !strings.Contains(profile, part) {
			t.Fatalf("profile missing %s", part)
		}
	}
	for _, path := range []string{"/SKILLS.md/private", "/embed.go", "/agent_docs.go"} {
		res, err := http.Get(h.http.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 404 {
			t.Fatalf("unexpected public path %s: %d", path, res.StatusCode)
		}
	}
}
