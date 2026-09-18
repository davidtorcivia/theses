package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/docs"
)

// documentRoutes is every document endpoint. It is one call from Handler so
// that the route table there stays one line longer than it was.
func (a *API) documentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/propositions/{id}/documents", a.scoped(auth.ScopeRead, a.listDocuments))
	mux.HandleFunc("POST /api/v1/propositions/{id}/documents", a.scoped(auth.ScopeWrite, a.createDocument))
	mux.HandleFunc("GET /api/v1/documents/{id}", a.scoped(auth.ScopeRead, a.readDocument))
	mux.HandleFunc("PATCH /api/v1/documents/{id}", a.scoped(auth.ScopeWrite, a.renameDocument))
	mux.HandleFunc("DELETE /api/v1/documents/{id}", a.scoped(auth.ScopeWrite, a.deleteDocument))
	mux.HandleFunc("GET /api/v1/documents/{id}/revisions", a.scoped(auth.ScopeRead, a.listRevisions))
	mux.HandleFunc("POST /api/v1/documents/{id}/revisions", a.scoped(auth.ScopeWrite, a.createRevision))
	mux.HandleFunc("POST /api/v1/documents/{id}/blocks", a.scoped(auth.ScopeWrite, a.insertBlock))
	mux.HandleFunc("PUT /api/v1/blocks/{id}", a.scoped(auth.ScopeWrite, a.setBlock))
	mux.HandleFunc("POST /api/v1/blocks/{id}/move", a.scoped(auth.ScopeWrite, a.moveBlock))
	mux.HandleFunc("DELETE /api/v1/blocks/{id}", a.scoped(auth.ScopeWrite, a.deleteBlock))
}

// documentBody is every field any of these endpoints takes. They share most of
// them and none of them is ambiguous, which is the same choice the websocket's
// argument struct makes.
type documentBody struct {
	Name        string `json:"name"`
	Text        string `json:"text"`
	After       int64  `json:"after"`
	BaseVersion int64  `json:"base_version"`
	Reason      string `json:"reason"`
}

// documentView is one document with its blocks and, on the read of a single
// document, the markdown rendered the way the page renders it.
type documentView struct {
	docs.Doc
	Rendered string `json:"rendered,omitempty"`
}

func (a *API) listDocuments(w http.ResponseWriter, r *http.Request, p Principal) {
	id, ok := a.pathID(w, r, "proposition")
	if !ok {
		return
	}
	list, err := a.Docs.Documents(r.Context(), p.User, id)
	if err != nil {
		a.documentError(w, r, err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"documents": list})
}

func (a *API) readDocument(w http.ResponseWriter, r *http.Request, p Principal) {
	id, ok := a.pathID(w, r, "document")
	if !ok {
		return
	}
	doc, err := a.Docs.Document(r.Context(), p.User, id)
	if err != nil {
		a.documentError(w, r, err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{
		"document": documentView{Doc: doc, Rendered: string(docs.RenderDocument(doc.Blocks))},
	})
}

func (a *API) createDocument(w http.ResponseWriter, r *http.Request, p Principal) {
	id, ok := a.pathID(w, r, "proposition")
	if !ok {
		return
	}
	body, ok := a.documentBody(w, r)
	if !ok {
		return
	}
	a.applied(w, r, p, func() (core.Event, error) {
		return a.Docs.CreateDocument(r.Context(), actorOf(p), id, body.Name)
	})
}

func (a *API) renameDocument(w http.ResponseWriter, r *http.Request, p Principal) {
	id, ok := a.pathID(w, r, "document")
	if !ok {
		return
	}
	body, ok := a.documentBody(w, r)
	if !ok {
		return
	}
	a.applied(w, r, p, func() (core.Event, error) {
		return a.Docs.RenameDocument(r.Context(), actorOf(p), id, body.Name)
	})
}

func (a *API) deleteDocument(w http.ResponseWriter, r *http.Request, p Principal) {
	id, ok := a.pathID(w, r, "document")
	if !ok {
		return
	}
	a.applied(w, r, p, func() (core.Event, error) {
		return a.Docs.DeleteDocument(r.Context(), actorOf(p), id)
	})
}

func (a *API) listRevisions(w http.ResponseWriter, r *http.Request, p Principal) {
	id, ok := a.pathID(w, r, "document")
	if !ok {
		return
	}
	list, err := a.Docs.History(r.Context(), p.User, id)
	if err != nil {
		a.documentError(w, r, err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"revisions": list})
}

func (a *API) createRevision(w http.ResponseWriter, r *http.Request, p Principal) {
	id, ok := a.pathID(w, r, "document")
	if !ok {
		return
	}
	body, ok := a.documentBody(w, r)
	if !ok {
		return
	}
	// A revision asked for over the API is one somebody asked for, whatever
	// they called it; the timer and the importer keep their own.
	if body.Reason == "" {
		body.Reason = docs.ReasonManual
	}
	a.applied(w, r, p, func() (core.Event, error) {
		return a.Docs.CreateRevision(r.Context(), actorOf(p), id, body.Reason)
	})
}

func (a *API) insertBlock(w http.ResponseWriter, r *http.Request, p Principal) {
	id, ok := a.pathID(w, r, "document")
	if !ok {
		return
	}
	body, ok := a.documentBody(w, r)
	if !ok {
		return
	}
	a.applied(w, r, p, func() (core.Event, error) {
		return a.Docs.InsertBlock(r.Context(), actorOf(p), id, body.After, body.Text)
	})
}

func (a *API) setBlock(w http.ResponseWriter, r *http.Request, p Principal) {
	id, ok := a.pathID(w, r, "block")
	if !ok {
		return
	}
	body, ok := a.documentBody(w, r)
	if !ok {
		return
	}
	a.applied(w, r, p, func() (core.Event, error) {
		return a.Docs.SetBlock(r.Context(), actorOf(p), id, body.BaseVersion, body.Text)
	})
}

func (a *API) moveBlock(w http.ResponseWriter, r *http.Request, p Principal) {
	id, ok := a.pathID(w, r, "block")
	if !ok {
		return
	}
	body, ok := a.documentBody(w, r)
	if !ok {
		return
	}
	a.applied(w, r, p, func() (core.Event, error) {
		return a.Docs.MoveBlock(r.Context(), actorOf(p), id, body.After)
	})
}

func (a *API) deleteBlock(w http.ResponseWriter, r *http.Request, p Principal) {
	id, ok := a.pathID(w, r, "block")
	if !ok {
		return
	}
	a.applied(w, r, p, func() (core.Event, error) {
		return a.Docs.DeleteBlock(r.Context(), actorOf(p), id)
	})
}

// actorOf is how a call with this token is recorded: the person who owns it,
// with the token's name as via.
func actorOf(p Principal) core.Actor {
	return core.Actor{Kind: core.KindUser, ID: p.User.ID, Name: p.User.Name, Via: TokenVia(p.Token.Name)}
}

// applied runs one command and answers with the event, which carries the whole
// row so a client need not read it back.
func (a *API) applied(w http.ResponseWriter, r *http.Request, _ Principal, run func() (core.Event, error)) {
	e, err := run()
	if err != nil {
		a.documentError(w, r, err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"event": e})
}

func (a *API) pathID(w http.ResponseWriter, r *http.Request, what string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		a.fail(w, http.StatusNotFound, "no such "+what)
		return 0, false
	}
	return id, true
}

// documentBody reads the JSON body, treating an empty one as all defaults so
// that a delete or a move to the head need send nothing.
func (a *API) documentBody(w http.ResponseWriter, r *http.Request) (documentBody, bool) {
	var body documentBody
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		if errors.Is(err, io.EOF) {
			return body, true
		}
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			a.fail(w, http.StatusRequestEntityTooLarge, "that body is too large")
			return body, false
		}
		a.fail(w, http.StatusBadRequest, "the body must be JSON")
		return body, false
	}
	return body, true
}

// documentError maps one refusal to one status. A caller who may not read the
// proposition and one asking about a document that is not there are told the
// same thing, because core answers the role and the membership with one error
// and saying which would say whether the row is there.
func (a *API) documentError(w http.ResponseWriter, r *http.Request, err error) {
	var clash *core.ConflictError
	switch {
	case errors.As(err, &clash):
		a.writeJSON(w, http.StatusConflict, map[string]any{
			"error": "that changed while you were editing it", "conflict": clash})
	case errors.Is(err, core.ErrNotFound), errors.Is(err, core.ErrForbidden):
		a.fail(w, http.StatusNotFound, "that is not there")
	case errors.Is(err, board.ErrArchived):
		a.fail(w, http.StatusConflict, err.Error())
	case errors.Is(err, board.ErrEmpty), errors.Is(err, board.ErrTooLong),
		errors.Is(err, docs.ErrNameTaken), errors.Is(err, docs.ErrReason),
		errors.Is(err, docs.ErrTooManyDocuments):
		a.fail(w, http.StatusBadRequest, err.Error())
	default:
		a.serverError(w, r, err)
	}
}
