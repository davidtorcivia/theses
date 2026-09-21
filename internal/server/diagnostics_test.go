package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/core"
)

func TestDiagnosticsAreOwnerOnlyAndContainCounts(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	owner := h.owner()
	ctx := context.Background()
	if _, err := h.srv.board.Do(ctx, core.Actor{Kind: core.KindUser, ID: 99999}, 0, auth.CanEdit, func(context.Context, *sql.Tx) (core.Change, error) {
		t.Fatal("unauthorized callback ran")
		return core.Change{}, nil
	}); err == nil {
		t.Fatal("invalid actor accepted")
	}
	if _, err := h.srv.files.Complete(ctx, owner, -1, 0, 0, 0); err == nil {
		t.Fatal("invalid upload completed")
	}
	if err := h.srv.docs.Mirror(ctx, -1, nil, false); err == nil {
		t.Fatal("missing document mirrored")
	}
	prop := h.proposition("Upload diagnostic")
	if _, err := h.db.ExecContext(ctx, `INSERT INTO files(proposition_id,name,object_key,state,created_at) VALUES(?,'old.txt','old','uploading',unixepoch()-172801)`, prop); err != nil {
		t.Fatal(err)
	}
	report, err := h.srv.api.ReadDiagnostics(ctx, core.Actor{Kind: core.KindUser, ID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	if report["core_refusals_since_restart"] != uint64(1) || report["upload_completion_failures_since_restart"] != uint64(1) || report["mirror_write_failures_since_restart"] != uint64(0) {
		t.Fatalf("counters %v", report)
	}
	if report["expired_uploads"] != int64(1) {
		t.Fatal("expired single PUT upload omitted")
	}
	raw, _ := json.Marshal(report)
	for _, private := range []string{"ada@example.com", owner.Name, "object_key", "password", "body_md"} {
		if private != "" && strings.Contains(string(raw), private) {
			t.Fatalf("private detail in diagnostics: %s", private)
		}
	}
	res, body := h.get("/app/diagnostics")
	if res.StatusCode != 200 || !strings.Contains(body, "notification_unmatched") {
		t.Fatalf("diagnostics: %d %s", res.StatusCode, body)
	}
	for _, tc := range []struct {
		scope  string
		status int
	}{{auth.ScopeRead, 403}, {auth.ScopeAdmin, 200}} {
		token, err := h.srv.auth.CreateAPIToken(ctx, owner.ID, "diagnostics-"+tc.scope, []string{tc.scope})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest("GET", "/api/v1/diagnostics", nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		h.srv.api.Handler().ServeHTTP(response, request)
		if response.Code != tc.status {
			t.Fatalf("%s scope: %d %s", tc.scope, response.Code, response.Body.String())
		}
	}
	h.setRole(t, owner.ID, auth.RoleEditor)
	if _, err := h.srv.api.ReadDiagnostics(ctx, core.Actor{Kind: core.KindUser, ID: owner.ID}); err == nil {
		t.Fatal("editor read diagnostics")
	}
	res, _ = h.get("/app/diagnostics")
	if res.StatusCode != 404 {
		t.Fatalf("editor status %d", res.StatusCode)
	}
}
