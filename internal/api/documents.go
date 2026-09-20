package api

import (
	"net/http"
	"strconv"

	"github.com/davidtorcivia/theses/internal/auth"
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
	// Whole asks for the text to be stored exactly as it was sent, which is
	// what an editor saving while somebody types needs and nothing else does.
	Whole bool `json:"whole"`
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
		a.refuse(w, r, err)
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
		a.refuse(w, r, err)
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
		a.refuse(w, r, err)
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
	// A revision asked for over the API is one somebody asked for. The timer's
	// reason and the importer's belong to the timer and the importer, so naming
	// one here is refused rather than quietly filed under it.
	if body.Reason != "" && body.Reason != docs.ReasonManual {
		a.refuse(w, r, docs.ErrReason)
		return
	}
	a.applied(w, r, p, func() (core.Event, error) {
		return a.Docs.CreateRevision(r.Context(), actorOf(p), id, docs.ReasonManual)
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
		return a.Docs.InsertBlock(r.Context(), actorOf(p), id, body.After, body.Text, body.Whole)
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
		return a.Docs.SetBlock(r.Context(), actorOf(p), id, body.BaseVersion, body.Text, body.Whole)
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

// sourceBody is a whole document as markdown, with the blocks it was written
// from. A base left out altogether means the document as it stands, which is
// what an agent replacing a document it has just read sends; an empty list is a
// document that had no blocks, so the two are told apart rather than folded
// together. A base naming some of the blocks is that much of the document: the
// text stands for those blocks and every other one is left where it is.
type sourceBody struct {
	Text string          `json:"text"`
	Base []docs.BlockRef `json:"base"`
}

// maxSourceBytes is as large as a document written back as markdown may be.
const maxSourceBytes = 1 << 20

// writeSource replaces a document from its markdown. It answers with the blocks
// the save could not take rather than with an event, because it is many
// commands in one transaction and the caller's next move is about the ones that
// did not go in. It is mounted under both prefixes, so this is the browser's
// source view as well as the API's.
func (a *API) writeSource(w http.ResponseWriter, r *http.Request, who core.Actor) {
	id, ok := a.pathID(w, r, "document")
	if !ok {
		return
	}
	var body sourceBody
	// A whole document is more than the sixty four kilobytes a body that names
	// one field of one row is held to, and a megabyte is a long document with
	// room to spare. It also bounds the base list, which is a block to look up
	// each, and so bounds the work one request can ask of the write lock.
	if !a.decodeUpTo(w, r, maxSourceBytes, &body) {
		return
	}
	if a.Docs == nil {
		a.fail(w, http.StatusNotFound, "no such document")
		return
	}
	save, err := a.Docs.WriteSource(r.Context(), who, id, body.Base, body.Text)
	if err != nil {
		a.refuse(w, r, err)
		return
	}
	a.writeJSON(w, http.StatusOK, save)
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
		a.refuse(w, r, err)
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
	return body, a.decode(w, r, &body)
}
