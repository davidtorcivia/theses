package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/backup"
	"github.com/davidtorcivia/theses/internal/blob"
	"github.com/davidtorcivia/theses/internal/config"
)

// The settings page's backup buttons over REST: owner tokens only, the same
// check of an archive's key the page makes, and a backup key nobody has set
// answered as this side not being ready.
func TestBackupRoutes(t *testing.T) {
	h := newHarness(t)
	admin, write := h.token(auth.ScopeAdmin), h.token(auth.ScopeRead, auth.ScopeWrite, auth.ScopeFiles)
	tests := []struct {
		name, method, path, token, body string
		without, with                   int // with no backup service, and with one that has no key
	}{
		{"list needs admin", "GET", "/api/v1/backups", write, "", http.StatusForbidden, http.StatusForbidden},
		{"now needs admin", "POST", "/api/v1/backups", write, "", http.StatusForbidden, http.StatusForbidden},
		{"restore needs admin", "POST", "/api/v1/backups/restore", write, `{"key":"backups/a.tar.gz.age"}`, http.StatusForbidden, http.StatusForbidden},
		{"list with no key", "GET", "/api/v1/backups", admin, "", http.StatusServiceUnavailable, http.StatusServiceUnavailable},
		{"verify another object", "POST", "/api/v1/backups/verify", admin, `{"key":"uploads/secret.pdf"}`, http.StatusServiceUnavailable, http.StatusNotFound},
		{"restore another object", "POST", "/api/v1/backups/restore", admin, `{"key":"backups/a.json"}`, http.StatusServiceUnavailable, http.StatusNotFound},
		{"restore not JSON", "POST", "/api/v1/backups/restore", admin, `key`, http.StatusBadRequest, http.StatusBadRequest},
	}
	run := func(t *testing.T, with bool) {
		for _, tt := range tests {
			want := tt.without
			if with {
				want = tt.with
			}
			if w := h.do(tt.method, tt.path, tt.token, tt.body); w.Code != want {
				t.Errorf("%s: %d, want %d: %s", tt.name, w.Code, want, w.Body)
			}
		}
	}
	run(t, false)
	h.api.Backup = backup.New(&config.Config{DataDir: t.TempDir()}, h.db, h.set,
		slog.New(slog.NewTextHandler(io.Discard, nil)), "test",
		func(context.Context) (blob.Config, error) { return blob.Config{}, nil })
	t.Cleanup(h.api.Backup.Stop)
	run(t, true)
}
