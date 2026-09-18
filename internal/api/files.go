package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/files"
	"github.com/davidtorcivia/theses/internal/store"
)

// The links and files routes are written once and mounted twice: under
// /api/v1 for a bearer token, and under /app for the browser's session cookie.
// They are the same handlers because they are the same commands; only who is
// asking is resolved differently, and the service refuses whatever the actor
// may not do either way.

// A handler is one route, already told who is asking.
type handler func(http.ResponseWriter, *http.Request, core.Actor)

// A wrapper resolves the actor and checks whatever standing the surface asks
// for before the handler runs.
type wrapper func(scope string, h handler) http.HandlerFunc

// FilesHandler is the links and files routes for a token. The caller mounts it
// inside Authenticate, the same as the rest of /api/v1.
func FilesHandler(a *API, svc *files.Service) http.Handler {
	mux := http.NewServeMux()
	mount(mux, "/api/v1", a, svc, func(scope string, h handler) http.HandlerFunc {
		return a.scoped(scope, func(w http.ResponseWriter, r *http.Request, p Principal) {
			h(w, r, core.Actor{
				Kind: core.KindUser, ID: p.User.ID, Name: p.User.Name,
				Via: TokenVia(p.Token.Name),
			})
		})
	})
	return mux
}

// SessionHandler is the same routes under /app for a browser. The caller
// mounts it behind whatever resolves the session, and passes the function that
// reads the signed-in person back out of the request.
func SessionHandler(a *API, svc *files.Service, user func(*http.Request) *store.User) http.Handler {
	mux := http.NewServeMux()
	mount(mux, "/app", a, svc, func(_ string, h handler) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			// A scope is a property of a token, and a browser has none: what
			// the person may do is their role, which every command checks.
			u := user(r)
			if u == nil {
				a.fail(w, http.StatusUnauthorized, "sign in first")
				return
			}
			h(w, r, core.Actor{Kind: core.KindUser, ID: u.ID, Name: u.Name})
		}
	})
	return mux
}

func mount(mux *http.ServeMux, prefix string, a *API, svc *files.Service, wrap wrapper) {
	f := &fileAPI{API: a, svc: svc}

	mux.HandleFunc("GET "+prefix+"/links", wrap(auth.ScopeRead, f.listLinks))
	mux.HandleFunc("POST "+prefix+"/links", wrap(auth.ScopeWrite, f.addLink))
	mux.HandleFunc("GET "+prefix+"/links/{id}", wrap(auth.ScopeRead, f.getLink))
	mux.HandleFunc("PATCH "+prefix+"/links/{id}", wrap(auth.ScopeWrite, f.editLink))
	mux.HandleFunc("POST "+prefix+"/links/{id}/refetch", wrap(auth.ScopeWrite, f.refetchLink))
	mux.HandleFunc("DELETE "+prefix+"/links/{id}", wrap(auth.ScopeWrite, f.deleteLink))

	mux.HandleFunc("GET "+prefix+"/files", wrap(auth.ScopeRead, f.listFiles))
	mux.HandleFunc("POST "+prefix+"/files", wrap(auth.ScopeFiles, f.createFile))
	mux.HandleFunc("GET "+prefix+"/files/{id}/parts", wrap(auth.ScopeFiles, f.parts))
	mux.HandleFunc("POST "+prefix+"/files/{id}/complete", wrap(auth.ScopeFiles, f.complete))
	mux.HandleFunc("GET "+prefix+"/files/{id}/download", wrap(auth.ScopeRead, f.download))
	mux.HandleFunc("GET "+prefix+"/files/{id}/thumb", wrap(auth.ScopeRead, f.thumb))
	mux.HandleFunc("GET "+prefix+"/files/{id}/versions", wrap(auth.ScopeRead, f.versions))
	mux.HandleFunc("PATCH "+prefix+"/files/{id}", wrap(auth.ScopeWrite, f.editFile))
	mux.HandleFunc("DELETE "+prefix+"/files/{id}", wrap(auth.ScopeFiles, f.deleteFile))

	mux.HandleFunc("GET "+prefix+"/attachments", wrap(auth.ScopeRead, f.attachments))
	mux.HandleFunc("POST "+prefix+"/cards/{card}/links/{id}", wrap(auth.ScopeWrite, f.attachLink))
	mux.HandleFunc("DELETE "+prefix+"/cards/{card}/links/{id}", wrap(auth.ScopeWrite, f.detachLink))
	mux.HandleFunc("POST "+prefix+"/cards/{card}/files/{id}", wrap(auth.ScopeWrite, f.attachFile))
	mux.HandleFunc("DELETE "+prefix+"/cards/{card}/files/{id}", wrap(auth.ScopeWrite, f.detachFile))
}

// fileAPI is the API with the service these routes need. It is a type of its
// own rather than a field on API so that both surfaces mount the same routes
// without the handler itself knowing which one it is on.
type fileAPI struct {
	*API
	svc *files.Service
}

func (f *fileAPI) listFiles(w http.ResponseWriter, r *http.Request, a core.Actor) {
	rows, err := f.svc.ListFiles(r.Context(), a, id(r, "proposition"))
	if err != nil {
		f.refuse(w, r, err)
		return
	}
	f.writeJSON(w, http.StatusOK, map[string]any{"files": rows, "folders": files.Folders})
}

// createFile answers with the file row and the way to put the object in the
// bucket: one presigned PUT for a small file, an upload id and a batch of part
// URLs for anything larger.
func (f *fileAPI) createFile(w http.ResponseWriter, r *http.Request, a core.Actor) {
	var in struct {
		Proposition int64  `json:"proposition"`
		Name        string `json:"name"`
		Folder      string `json:"folder"`
		Size        int64  `json:"size"`
		Replace     int64  `json:"replace"`
	}
	if !f.read(w, r, &in) {
		return
	}
	up, err := f.svc.Create(r.Context(), a, in.Proposition, in.Name, in.Folder, in.Size, in.Replace)
	if err != nil {
		f.refuse(w, r, err)
		return
	}
	f.writeJSON(w, http.StatusOK, up)
}

// parts is the resume: which parts the bucket already holds and signed URLs for
// a batch of the rest, starting after ?after=.
func (f *fileAPI) parts(w http.ResponseWriter, r *http.Request, a core.Actor) {
	up, err := f.svc.Parts(r.Context(), a, path(r, "id"), intParam(r, "after", 0))
	if err != nil {
		f.refuse(w, r, err)
		return
	}
	f.writeJSON(w, http.StatusOK, up)
}

func (f *fileAPI) complete(w http.ResponseWriter, r *http.Request, a core.Actor) {
	var in struct {
		DurationMS int64 `json:"duration_ms"`
		Width      int64 `json:"width"`
		Height     int64 `json:"height"`
	}
	if !f.read(w, r, &in) {
		return
	}
	e, err := f.svc.Complete(r.Context(), a, path(r, "id"), in.DurationMS, in.Width, in.Height)
	if err != nil {
		f.refuse(w, r, err)
		return
	}
	f.writeJSON(w, http.StatusOK, map[string]any{"file": e.After})
}

func (f *fileAPI) download(w http.ResponseWriter, r *http.Request, a core.Actor) {
	url, err := f.svc.DownloadURL(r.Context(), a, path(r, "id"))
	if err != nil {
		f.refuse(w, r, err)
		return
	}
	f.writeJSON(w, http.StatusOK, map[string]any{"url": url})
}

func (f *fileAPI) thumb(w http.ResponseWriter, r *http.Request, a core.Actor) {
	url, err := f.svc.ThumbURL(r.Context(), a, path(r, "id"))
	if err != nil {
		f.refuse(w, r, err)
		return
	}
	f.writeJSON(w, http.StatusOK, map[string]any{"url": url})
}

func (f *fileAPI) versions(w http.ResponseWriter, r *http.Request, a core.Actor) {
	rows, err := f.svc.Versions(r.Context(), a, path(r, "id"))
	if err != nil {
		f.refuse(w, r, err)
		return
	}
	f.writeJSON(w, http.StatusOK, map[string]any{"versions": rows})
}

// editFile renames or moves, and leaves the one the body does not name alone:
// the command takes both, so a body with only a folder in it would otherwise
// arrive as a rename to nothing.
func (f *fileAPI) editFile(w http.ResponseWriter, r *http.Request, a core.Actor) {
	var in struct {
		Name   *string `json:"name"`
		Folder *string `json:"folder"`
	}
	if !f.read(w, r, &in) {
		return
	}
	was, err := f.svc.ReadFile(r.Context(), a, path(r, "id"))
	if err != nil {
		f.refuse(w, r, err)
		return
	}
	name, folder := was.Name, was.Folder
	if in.Name != nil {
		name = *in.Name
	}
	if in.Folder != nil {
		folder = *in.Folder
	}
	e, err := f.svc.EditFile(r.Context(), a, was.ID, name, folder)
	if err != nil {
		f.refuse(w, r, err)
		return
	}
	f.writeJSON(w, http.StatusOK, map[string]any{"file": e.After})
}

func (f *fileAPI) deleteFile(w http.ResponseWriter, r *http.Request, a core.Actor) {
	if _, err := f.svc.Delete(r.Context(), a, path(r, "id")); err != nil {
		f.refuse(w, r, err)
		return
	}
	f.writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (f *fileAPI) attachments(w http.ResponseWriter, r *http.Request, a core.Actor) {
	got, err := f.svc.Attachments(r.Context(), a, id(r, "proposition"))
	if err != nil {
		f.refuse(w, r, err)
		return
	}
	f.writeJSON(w, http.StatusOK, got)
}

func (f *fileAPI) attachLink(w http.ResponseWriter, r *http.Request, a core.Actor) {
	e, err := f.svc.AttachLink(r.Context(), a, path(r, "card"), path(r, "id"))
	f.joined(w, r, e, err)
}

func (f *fileAPI) detachLink(w http.ResponseWriter, r *http.Request, a core.Actor) {
	e, err := f.svc.DetachLink(r.Context(), a, path(r, "card"), path(r, "id"))
	f.joined(w, r, e, err)
}

func (f *fileAPI) attachFile(w http.ResponseWriter, r *http.Request, a core.Actor) {
	e, err := f.svc.AttachFile(r.Context(), a, path(r, "card"), path(r, "id"))
	f.joined(w, r, e, err)
}

func (f *fileAPI) detachFile(w http.ResponseWriter, r *http.Request, a core.Actor) {
	e, err := f.svc.DetachFile(r.Context(), a, path(r, "card"), path(r, "id"))
	f.joined(w, r, e, err)
}

func (f *fileAPI) joined(w http.ResponseWriter, r *http.Request, e core.Event, err error) {
	if err != nil {
		f.refuse(w, r, err)
		return
	}
	f.writeJSON(w, http.StatusOK, map[string]any{"card": e.EntityID, "action": e.Action})
}

// read decodes a JSON body, answering the client itself when it cannot. It
// reports whether the handler should carry on.
func (f *fileAPI) read(w http.ResponseWriter, r *http.Request, into any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(into); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			f.fail(w, http.StatusRequestEntityTooLarge, "that body is too large")
			return false
		}
		f.fail(w, http.StatusBadRequest, "the body must be JSON")
		return false
	}
	return true
}

// refuse maps a command's error to a status. Anything not named here is a fault
// on this side: it is logged with the path and answered without its detail.
func (f *fileAPI) refuse(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, core.ErrNotFound):
		// A proposition somebody is not a member of answers the same way as one
		// that does not exist, because the rule is that they are not told.
		f.fail(w, http.StatusNotFound, "that is not here")
	case errors.Is(err, core.ErrForbidden):
		f.fail(w, http.StatusForbidden, "you cannot do that here")
	case errors.Is(err, files.ErrNoBucket):
		f.fail(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, board.ErrArchived), errors.Is(err, board.ErrEmpty),
		errors.Is(err, board.ErrTooLong), errors.Is(err, files.ErrKind),
		errors.Is(err, files.ErrQuestion), errors.Is(err, files.ErrURL),
		errors.Is(err, files.ErrState), errors.Is(err, files.ErrSize),
		errors.Is(err, files.ErrSwept), errors.Is(err, files.ErrCrossBucket),
		errors.Is(err, files.ErrPart):
		f.fail(w, http.StatusUnprocessableEntity, err.Error())
	default:
		f.serverError(w, r, err)
	}
}

// path is a numeric path value, zero when it is not one. Zero reaches the
// service and comes back as ErrNotFound, so a bad id needs no branch here.
func path(r *http.Request, name string) int64 {
	n, _ := strconv.ParseInt(r.PathValue(name), 10, 64)
	return n
}

// id is the same for a query parameter.
func id(r *http.Request, name string) int64 {
	n, _ := strconv.ParseInt(r.URL.Query().Get(name), 10, 64)
	return n
}
