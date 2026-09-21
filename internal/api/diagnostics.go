package api

import (
	"context"
	"net/http"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/store"
)

func (a *API) ReadDiagnostics(ctx context.Context, actor core.Actor) (map[string]any, error) {
	if actor.Kind != core.KindUser {
		return nil, core.ErrForbidden
	}
	user, err := store.UserByID(ctx, a.db, actor.ID)
	if err != nil {
		return nil, err
	}
	if user.Role != auth.RoleOwner {
		return nil, core.ErrForbidden
	}
	if a.Diagnostics == nil {
		return nil, core.ErrNotFound
	}
	return a.Diagnostics(ctx)
}
func (f *fileAPI) diagnostics(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	report, err := f.ReadDiagnostics(r.Context(), actor)
	if err != nil {
		f.refuse(w, r, err)
		return
	}
	f.writeJSON(w, http.StatusOK, report)
}
