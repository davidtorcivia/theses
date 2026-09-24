package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/backup"
	"github.com/davidtorcivia/theses/internal/core"
)

// ErrNoBackups is a build that was given no backup service, refused politely
// rather than by a panic.
var ErrNoBackups = errors.New("this endpoint has no backups")

// backupRoutes is the settings page's backup buttons. They take admin, which
// only an owner's token has, like the page.
func (a *API) backupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/backups", a.scoped(auth.ScopeAdmin, a.listBackups))
	mux.HandleFunc("POST /api/v1/backups", a.scoped(auth.ScopeAdmin, a.backupNow))
	mux.HandleFunc("POST /api/v1/backups/verify", a.scoped(auth.ScopeAdmin, a.verifyBackup))
	mux.HandleFunc("POST /api/v1/backups/restore", a.scoped(auth.ScopeAdmin, a.restoreBackup))
}

// A BackupView is one archive under the backups prefix. Complete is false for
// an archive with no manifest beside it, which cannot be verified or restored.
type BackupView struct {
	Key      string `json:"key"`
	When     int64  `json:"when"`
	Size     int64  `json:"size"`
	Complete bool   `json:"complete"`
}

// Backups is the archives, newest first, for both surfaces.
func (a *API) Backups(ctx context.Context) ([]BackupView, error) {
	if a.Backup == nil {
		return nil, ErrNoBackups
	}
	entries, err := a.Backup.List(ctx)
	if err != nil {
		return nil, err
	}
	out := []BackupView{}
	for _, e := range entries {
		out = append(out, BackupView{Key: e.Key, When: e.When.Unix(), Size: e.Size, Complete: e.Manifest != ""})
	}
	return out, nil
}

// BackupNow starts one archive in the background, as the page's button does.
func (a *API) BackupNow(ctx context.Context) error {
	if a.Backup == nil {
		return ErrNoBackups
	}
	return a.Backup.Now(ctx)
}

// VerifyBackup starts restoring one archive into an isolated workspace to
// prove it can be; the result is on the settings page and in the log.
func (a *API) VerifyBackup(ctx context.Context, key string) error {
	if a.Backup == nil {
		return ErrNoBackups
	}
	if !archive(key) {
		return core.ErrNotFound
	}
	return a.Backup.VerifyNow(ctx, key)
}

// RestoreBackup starts replacing the database and the markdown mirror with one
// archive. What is here now is moved aside rather than removed, writes are
// refused until it is done, and every session and token ends with it, this
// one included.
func (a *API) RestoreBackup(ctx context.Context, who core.Actor, key string) error {
	if a.Backup == nil {
		return ErrNoBackups
	}
	if !archive(key) {
		return core.ErrNotFound
	}
	return a.Backup.RestoreNow(ctx, key, who.ID)
}

// archive is the check the settings page makes of a key before it acts on it:
// one of this workspace's archives, not any object in the bucket.
func archive(key string) bool {
	return strings.HasPrefix(key, backup.Prefix) && strings.HasSuffix(key, ".tar.gz.age")
}

func (a *API) listBackups(w http.ResponseWriter, r *http.Request, _ Principal) {
	list, err := a.Backups(r.Context())
	if err != nil {
		a.refuse(w, r, err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"backups": list})
}

func (a *API) backupNow(w http.ResponseWriter, r *http.Request, _ Principal) {
	a.started(w, r, a.BackupNow(r.Context()))
}

func (a *API) verifyBackup(w http.ResponseWriter, r *http.Request, _ Principal) {
	var in struct {
		Key string `json:"key"`
	}
	if a.decode(w, r, maxBodyBytes, true, &in) {
		a.started(w, r, a.VerifyBackup(r.Context(), in.Key))
	}
}

func (a *API) restoreBackup(w http.ResponseWriter, r *http.Request, p Principal) {
	var in struct {
		Key string `json:"key"`
	}
	if a.decode(w, r, maxBodyBytes, true, &in) {
		a.started(w, r, a.RestoreBackup(r.Context(), actorOf(p), in.Key))
	}
}

// started answers a job that outlives the request: accepted, or why not.
func (a *API) started(w http.ResponseWriter, r *http.Request, err error) {
	if err != nil {
		a.refuse(w, r, err)
		return
	}
	a.writeJSON(w, http.StatusAccepted, map[string]any{"started": true})
}
