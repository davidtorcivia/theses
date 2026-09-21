package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestPublishedWorkflowRoutes(t *testing.T) {
	raw, err := os.ReadFile("../../docs/api.md")
	if err != nil {
		t.Fatal(err)
	}
	routes := regexp.MustCompile("(?m)^\\| `[a-z_]+` \\| `(read|write)` \\| `(GET|POST|PUT|PATCH|DELETE) (/api/v1/[^`]+)` \\|").FindAllStringSubmatch(string(raw), -1)
	mux := http.NewServeMux()
	a := &API{}
	mount(mux, "/api/v1", a, nil, func(scope string, _ handler) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { w.Header().Set("Scope", scope) }
	})
	if len(routes) != 24 {
		t.Fatalf("expected 24 workflow mappings, got %d", len(routes))
	}
	for _, row := range routes {
		path := regexp.MustCompile(`\{[^}]+\}`).ReplaceAllString(row[3], "1")
		request := httptest.NewRequest(row[2], path, strings.NewReader("{}"))
		reply := httptest.NewRecorder()
		mux.ServeHTTP(reply, request)
		if reply.Code != 200 || reply.Header().Get("Scope") != row[1] {
			t.Fatalf("documented route %s %s: %d scope %q", row[2], path, reply.Code, reply.Header().Get("Scope"))
		}
	}
}
