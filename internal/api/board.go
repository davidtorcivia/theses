package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
)

// boardRoutes is the board as REST resources: propositions, columns, cards,
// checklist items, notes, and the undo of one activity row. Every one of them
// is the board command the websocket calls under another name, so the rules
// about membership, about an archived proposition and about who may delete are
// the ones in internal/board and are not written twice.
func (a *API) boardRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/propositions", a.scoped(auth.ScopeRead, a.listPropositions))
	mux.HandleFunc("POST /api/v1/propositions", a.scoped(auth.ScopeWrite, a.createProposition))
	mux.HandleFunc("GET /api/v1/propositions/{id}", a.scoped(auth.ScopeRead, a.getProposition))
	mux.HandleFunc("PATCH /api/v1/propositions/{id}", a.scoped(auth.ScopeWrite, a.editProposition))
	mux.HandleFunc("POST /api/v1/propositions/{id}/archive", a.scoped(auth.ScopeWrite, a.archiveProposition))
	mux.HandleFunc("POST /api/v1/propositions/{id}/restore", a.scoped(auth.ScopeWrite, a.restoreProposition))
	mux.HandleFunc("POST /api/v1/propositions/{id}/move", a.scoped(auth.ScopeWrite, a.moveProposition))
	mux.HandleFunc("DELETE /api/v1/propositions/{id}", a.scoped(auth.ScopeWrite, a.deleteProposition))
	mux.HandleFunc("POST /api/v1/propositions/{id}/members/{user}", a.scoped(auth.ScopeWrite, a.addMember))
	mux.HandleFunc("DELETE /api/v1/propositions/{id}/members/{user}", a.scoped(auth.ScopeWrite, a.removeMember))

	mux.HandleFunc("GET /api/v1/propositions/{id}/columns", a.scoped(auth.ScopeRead, a.listColumns))
	mux.HandleFunc("POST /api/v1/propositions/{id}/columns", a.scoped(auth.ScopeWrite, a.createColumn))
	mux.HandleFunc("PATCH /api/v1/columns/{id}", a.scoped(auth.ScopeWrite, a.renameColumn))
	mux.HandleFunc("POST /api/v1/columns/{id}/move", a.scoped(auth.ScopeWrite, a.moveColumn))
	mux.HandleFunc("DELETE /api/v1/columns/{id}", a.scoped(auth.ScopeWrite, a.deleteColumn))

	mux.HandleFunc("GET /api/v1/propositions/{id}/cards", a.scoped(auth.ScopeRead, a.listCards))
	mux.HandleFunc("POST /api/v1/columns/{id}/cards", a.scoped(auth.ScopeWrite, a.createCard))
	mux.HandleFunc("GET /api/v1/cards/{id}", a.scoped(auth.ScopeRead, a.getCard))
	mux.HandleFunc("PATCH /api/v1/cards/{id}", a.scoped(auth.ScopeWrite, a.editCard))
	mux.HandleFunc("POST /api/v1/cards/{id}/move", a.scoped(auth.ScopeWrite, a.moveCard))
	mux.HandleFunc("POST /api/v1/cards/{card}/assignees/{user}", a.scoped(auth.ScopeWrite, a.assignCard))
	mux.HandleFunc("DELETE /api/v1/cards/{card}/assignees/{user}", a.scoped(auth.ScopeWrite, a.unassignCard))
	mux.HandleFunc("POST /api/v1/cards/{id}/done", a.scoped(auth.ScopeWrite, a.doneCard))
	mux.HandleFunc("POST /api/v1/cards/{id}/reopen", a.scoped(auth.ScopeWrite, a.reopenCard))
	mux.HandleFunc("DELETE /api/v1/cards/{id}", a.scoped(auth.ScopeWrite, a.deleteCard))

	mux.HandleFunc("POST /api/v1/cards/{id}/checklist", a.scoped(auth.ScopeWrite, a.addChecklistItem))
	mux.HandleFunc("PATCH /api/v1/checklist/{id}", a.scoped(auth.ScopeWrite, a.toggleChecklistItem))
	mux.HandleFunc("DELETE /api/v1/checklist/{id}", a.scoped(auth.ScopeWrite, a.removeChecklistItem))

	mux.HandleFunc("POST /api/v1/cards/{id}/comments", a.scoped(auth.ScopeWrite, a.postComment))
	mux.HandleFunc("DELETE /api/v1/comments/{id}", a.scoped(auth.ScopeWrite, a.deleteComment))

	mux.HandleFunc("POST /api/v1/activity/{id}/undo", a.scoped(auth.ScopeWrite, a.undo))
}

// boardBody is every field any of these routes takes. It is one struct for the
// same reason the document routes have one: none of the names is ambiguous, and
// the websocket's argument struct is put together the same way.
//
// The fields a PATCH may change are pointers, because leaving one out and
// sending it empty mean different things: the first keeps what is there and the
// second clears it.
type boardBody struct {
	Title       *string `json:"title"`
	Statement   *string `json:"statement"`
	Blurb       *string `json:"blurb"`
	Status      *string `json:"status"`
	Episode     *string `json:"episode"`
	TargetDate  *string `json:"target_date"`
	Name        *string `json:"name"`
	Text        *string `json:"text"`
	Description *string `json:"description_md"`
	Question    *string `json:"question"`
	DueDate     *string `json:"due_date"`
	Body        *string `json:"body_md"`
	Done        *bool   `json:"done"`
	After       int64   `json:"after"`
	Column      int64   `json:"column"`
	BaseVersion int64   `json:"base_version"`
	Assignees   []int64 `json:"assignees"`
}

// boardBody reads the JSON body, treating an empty one as all defaults so that
// a delete, a done or a move to the head need send nothing.
func (a *API) boardBody(w http.ResponseWriter, r *http.Request) (boardBody, bool) {
	var body boardBody
	return body, a.decode(w, r, &body)
}

// decode reads a JSON body into a value, answering the client itself when it
// cannot, and reports whether the handler should carry on. An empty body is all
// defaults, so a route whose fields are every one optional need send nothing.
func (a *API) decode(w http.ResponseWriter, r *http.Request, into any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(into); err != nil {
		if errors.Is(err, io.EOF) {
			return true
		}
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			a.fail(w, http.StatusRequestEntityTooLarge, "that body is too large")
			return false
		}
		a.fail(w, http.StatusBadRequest, "the body must be JSON")
		return false
	}
	return true
}

// Propositions.

func (a *API) listPropositions(w http.ResponseWriter, r *http.Request, p Principal) {
	out, err := a.Propositions(r.Context(), p)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"propositions": out})
}

// Propositions is the rail as this token may read it, for both surfaces. An
// owner reads every proposition; everybody else reads the ones they are a
// member of, which is the rule the rail, the search and the log all obey. The
// list is filtered rather than refused, because a rail with nothing on it is an
// answer and a refusal here would be about the workspace.
func (a *API) Propositions(ctx context.Context, p Principal) ([]board.Proposition, error) {
	all, err := board.ListPropositions(ctx, a.db)
	if err != nil {
		return nil, err
	}
	if p.User != nil && p.User.Role == auth.RoleOwner {
		return all, nil
	}
	mine, err := a.memberships(ctx, p)
	if err != nil {
		return nil, err
	}
	out := []board.Proposition{}
	for _, prop := range all {
		if mine[prop.ID] {
			out = append(out, prop)
		}
	}
	return out, nil
}

func (a *API) getProposition(w http.ResponseWriter, r *http.Request, p Principal) {
	id, ok := a.pathID(w, r, "proposition")
	if !ok {
		return
	}
	prop, err := a.Proposition(r.Context(), p, id)
	if err != nil {
		a.refuse(w, r, err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"proposition": prop})
}

func (a *API) createProposition(w http.ResponseWriter, r *http.Request, p Principal) {
	body, ok := a.boardBody(w, r)
	if !ok {
		return
	}
	a.applied(w, r, p, func() (core.Event, error) {
		return a.Board.CreateProposition(r.Context(), actorOf(p), some(body.Title))
	})
}

// editProposition changes the fields the body names and leaves the rest as they
// were. The commands take whole sets, because that is what a form posts, so the
// row is read first and the body laid over it: naming one field is not a way to
// clear the others.
func (a *API) editProposition(w http.ResponseWriter, r *http.Request, p Principal) {
	id, ok := a.pathID(w, r, "proposition")
	if !ok {
		return
	}
	body, ok := a.boardBody(w, r)
	if !ok {
		return
	}
	if body.Title == nil && body.Statement == nil && body.Blurb == nil &&
		body.Status == nil && body.Episode == nil && body.TargetDate == nil {
		a.fail(w, http.StatusBadRequest, "name a field to change")
		return
	}
	who := actorOf(p)
	a.together(w, r, func(ctx context.Context) (core.Event, error) {
		was, err := a.Proposition(ctx, p, id)
		if err != nil {
			return core.Event{}, err
		}
		var e core.Event
		if body.Title != nil || body.Statement != nil || body.Blurb != nil {
			if e, err = a.Board.EditProposition(ctx, who, id, or(body.Title, was.Title),
				or(body.Statement, was.Statement), or(body.Blurb, was.Blurb)); err != nil {
				return core.Event{}, err
			}
		}
		if body.Status != nil {
			if e, err = a.Board.SetStatus(ctx, who, id, *body.Status); err != nil {
				return core.Event{}, err
			}
		}
		if body.Episode != nil || body.TargetDate != nil {
			if e, err = a.Board.Schedule(ctx, who, id, or(body.Episode, some(was.Episode)),
				or(body.TargetDate, some(was.TargetDate))); err != nil {
				return core.Event{}, err
			}
		}
		return e, nil
	})
}

func (a *API) archiveProposition(w http.ResponseWriter, r *http.Request, p Principal) {
	a.onProposition(w, r, p, a.Board.ArchiveProposition)
}

func (a *API) restoreProposition(w http.ResponseWriter, r *http.Request, p Principal) {
	a.onProposition(w, r, p, a.Board.RestoreProposition)
}

func (a *API) onProposition(w http.ResponseWriter, r *http.Request, p Principal,
	run func(context.Context, core.Actor, int64) (core.Event, error)) {
	id, ok := a.pathID(w, r, "proposition")
	if !ok {
		return
	}
	a.applied(w, r, p, func() (core.Event, error) { return run(r.Context(), actorOf(p), id) })
}

func (a *API) moveProposition(w http.ResponseWriter, r *http.Request, p Principal) {
	id, ok := a.pathID(w, r, "proposition")
	if !ok {
		return
	}
	body, ok := a.boardBody(w, r)
	if !ok {
		return
	}
	a.applied(w, r, p, func() (core.Event, error) {
		return a.Board.MoveProposition(r.Context(), actorOf(p), id, body.After)
	})
}

// deleteProposition takes the proposition and everything under it. The command
// asks for the standing to delete, which a researcher does not have, so a
// refusal is the same 404 as a proposition that is not there.
func (a *API) deleteProposition(w http.ResponseWriter, r *http.Request, p Principal) {
	a.onProposition(w, r, p, a.Board.DeleteProposition)
}

func (a *API) addMember(w http.ResponseWriter, r *http.Request, p Principal) {
	a.onMember(w, r, p, a.Board.AddMember)
}

func (a *API) removeMember(w http.ResponseWriter, r *http.Request, p Principal) {
	a.onMember(w, r, p, a.Board.RemoveMember)
}

// onMember is the shape both membership routes have. Somebody who is not in the
// workspace is 404, the same as a proposition that is not there.
func (a *API) onMember(w http.ResponseWriter, r *http.Request, p Principal,
	run func(context.Context, core.Actor, int64, int64) (core.Event, error)) {
	id, ok := a.pathID(w, r, "proposition")
	if !ok {
		return
	}
	a.applied(w, r, p, func() (core.Event, error) {
		return run(r.Context(), actorOf(p), id, path(r, "user"))
	})
}

// Columns.

func (a *API) listColumns(w http.ResponseWriter, r *http.Request, p Principal) {
	id, ok := a.pathID(w, r, "proposition")
	if !ok {
		return
	}
	if err := a.Readable(r.Context(), p, id); err != nil {
		a.refuse(w, r, err)
		return
	}
	cols, err := board.ListColumns(r.Context(), a.db, id)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"columns": cols})
}

func (a *API) createColumn(w http.ResponseWriter, r *http.Request, p Principal) {
	id, ok := a.pathID(w, r, "proposition")
	if !ok {
		return
	}
	body, ok := a.boardBody(w, r)
	if !ok {
		return
	}
	a.applied(w, r, p, func() (core.Event, error) {
		return a.Board.CreateColumn(r.Context(), actorOf(p), id, some(body.Name))
	})
}

func (a *API) renameColumn(w http.ResponseWriter, r *http.Request, p Principal) {
	id, ok := a.pathID(w, r, "column")
	if !ok {
		return
	}
	body, ok := a.boardBody(w, r)
	if !ok {
		return
	}
	a.applied(w, r, p, func() (core.Event, error) {
		return a.Board.RenameColumn(r.Context(), actorOf(p), id, some(body.Name))
	})
}

func (a *API) moveColumn(w http.ResponseWriter, r *http.Request, p Principal) {
	id, ok := a.pathID(w, r, "column")
	if !ok {
		return
	}
	body, ok := a.boardBody(w, r)
	if !ok {
		return
	}
	a.applied(w, r, p, func() (core.Event, error) {
		return a.Board.MoveColumn(r.Context(), actorOf(p), id, body.After)
	})
}

func (a *API) deleteColumn(w http.ResponseWriter, r *http.Request, p Principal) {
	id, ok := a.pathID(w, r, "column")
	if !ok {
		return
	}
	a.applied(w, r, p, func() (core.Event, error) {
		return a.Board.DeleteColumn(r.Context(), actorOf(p), id)
	})
}

// Cards.

func (a *API) listCards(w http.ResponseWriter, r *http.Request, p Principal) {
	id, ok := a.pathID(w, r, "proposition")
	if !ok {
		return
	}
	if err := a.Readable(r.Context(), p, id); err != nil {
		a.refuse(w, r, err)
		return
	}
	loaded, err := board.Load(r.Context(), a.db, id)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	// The sequence number goes with the cards, so a client that reads the board
	// and then follows the event stream knows where to start.
	a.writeJSON(w, http.StatusOK, map[string]any{"cards": loaded.Cards, "seq": loaded.Seq})
}

func (a *API) getCard(w http.ResponseWriter, r *http.Request, p Principal) {
	id, ok := a.pathID(w, r, "card")
	if !ok {
		return
	}
	card, err := board.GetCard(r.Context(), a.db, id)
	if err != nil {
		a.refuse(w, r, err)
		return
	}
	// The card is read before the membership, and a card on a proposition this
	// token's owner may not read answers the same way as one that is not there.
	if err := a.Readable(r.Context(), p, card.Proposition); err != nil {
		a.refuse(w, r, err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"card": card})
}

func (a *API) createCard(w http.ResponseWriter, r *http.Request, p Principal) {
	id, ok := a.pathID(w, r, "column")
	if !ok {
		return
	}
	body, ok := a.boardBody(w, r)
	if !ok {
		return
	}
	a.applied(w, r, p, func() (core.Event, error) {
		return a.Board.CreateCard(r.Context(), actorOf(p), id, some(body.Title), body.Assignees)
	})
}

// editCard is the one route that takes a base version. A card has one version
// across its title and its description, so an edit that names both sends the
// second command the version the first one left rather than the one the body
// carried, and an edit that began before somebody else's is refused with 409
// and the text that is in the card now.
func (a *API) editCard(w http.ResponseWriter, r *http.Request, p Principal) {
	id, ok := a.pathID(w, r, "card")
	if !ok {
		return
	}
	body, ok := a.boardBody(w, r)
	if !ok {
		return
	}
	if body.Title == nil && body.Description == nil && body.Question == nil && body.DueDate == nil {
		a.fail(w, http.StatusBadRequest, "name a field to change")
		return
	}
	who := actorOf(p)
	a.together(w, r, func(ctx context.Context) (core.Event, error) {
		base := body.BaseVersion
		var e core.Event
		var err error
		if body.Title != nil {
			if e, err = a.Board.EditCardTitle(ctx, who, id, base, *body.Title); err != nil {
				return core.Event{}, err
			}
			base = versionOf(e, base)
		}
		if body.Description != nil {
			if e, err = a.Board.EditCardDescription(ctx, who, id, base, *body.Description); err != nil {
				return core.Event{}, err
			}
			base = versionOf(e, base)
		}
		if body.Question != nil {
			if e, err = a.Board.SetCardQuestion(ctx, who, id, *body.Question); err != nil {
				return core.Event{}, err
			}
		}
		if body.DueDate != nil {
			if e, err = a.Board.SetCardDue(ctx, who, id, *body.DueDate); err != nil {
				return core.Event{}, err
			}
		}
		return e, nil
	})
}

// versionOf is the version a card is at after a command, read out of the
// event's payload rather than guessed at, with the version the caller sent as
// the answer for an event that carries no card.
func versionOf(e core.Event, fallback int64) int64 {
	var card board.Card
	if err := json.Unmarshal(e.After, &card); err != nil || card.Version == 0 {
		return fallback
	}
	return card.Version
}

func (a *API) moveCard(w http.ResponseWriter, r *http.Request, p Principal) {
	id, ok := a.pathID(w, r, "card")
	if !ok {
		return
	}
	body, ok := a.boardBody(w, r)
	if !ok {
		return
	}
	// A card always lands in a column, and a body that names none would reach
	// the command as column zero and come back as a column that is not there.
	if body.Column == 0 {
		a.fail(w, http.StatusBadRequest, "name the column to move it into")
		return
	}
	a.applied(w, r, p, func() (core.Event, error) {
		return a.Board.MoveCard(r.Context(), actorOf(p), id, body.Column, body.After)
	})
}

func (a *API) assignCard(w http.ResponseWriter, r *http.Request, p Principal) {
	a.applied(w, r, p, func() (core.Event, error) {
		return a.Board.AssignCard(r.Context(), actorOf(p), path(r, "card"), path(r, "user"))
	})
}

func (a *API) unassignCard(w http.ResponseWriter, r *http.Request, p Principal) {
	a.applied(w, r, p, func() (core.Event, error) {
		return a.Board.UnassignCard(r.Context(), actorOf(p), path(r, "card"), path(r, "user"))
	})
}

func (a *API) doneCard(w http.ResponseWriter, r *http.Request, p Principal) {
	a.setDone(w, r, p, true)
}

func (a *API) reopenCard(w http.ResponseWriter, r *http.Request, p Principal) {
	a.setDone(w, r, p, false)
}

func (a *API) setDone(w http.ResponseWriter, r *http.Request, p Principal, done bool) {
	id, ok := a.pathID(w, r, "card")
	if !ok {
		return
	}
	a.applied(w, r, p, func() (core.Event, error) {
		return a.Board.SetCardDone(r.Context(), actorOf(p), id, done)
	})
}

func (a *API) deleteCard(w http.ResponseWriter, r *http.Request, p Principal) {
	id, ok := a.pathID(w, r, "card")
	if !ok {
		return
	}
	a.applied(w, r, p, func() (core.Event, error) {
		return a.Board.DeleteCard(r.Context(), actorOf(p), id)
	})
}

// Checklist items and notes.

func (a *API) addChecklistItem(w http.ResponseWriter, r *http.Request, p Principal) {
	id, ok := a.pathID(w, r, "card")
	if !ok {
		return
	}
	body, ok := a.boardBody(w, r)
	if !ok {
		return
	}
	a.applied(w, r, p, func() (core.Event, error) {
		return a.Board.AddChecklistItem(r.Context(), actorOf(p), id, some(body.Text))
	})
}

func (a *API) toggleChecklistItem(w http.ResponseWriter, r *http.Request, p Principal) {
	id, ok := a.pathID(w, r, "checklist item")
	if !ok {
		return
	}
	body, ok := a.boardBody(w, r)
	if !ok {
		return
	}
	if body.Done == nil {
		a.fail(w, http.StatusBadRequest, "the body must be JSON with a done field")
		return
	}
	a.applied(w, r, p, func() (core.Event, error) {
		return a.Board.ToggleChecklistItem(r.Context(), actorOf(p), id, *body.Done)
	})
}

func (a *API) removeChecklistItem(w http.ResponseWriter, r *http.Request, p Principal) {
	id, ok := a.pathID(w, r, "checklist item")
	if !ok {
		return
	}
	a.applied(w, r, p, func() (core.Event, error) {
		return a.Board.RemoveChecklistItem(r.Context(), actorOf(p), id)
	})
}

func (a *API) postComment(w http.ResponseWriter, r *http.Request, p Principal) {
	id, ok := a.pathID(w, r, "card")
	if !ok {
		return
	}
	body, ok := a.boardBody(w, r)
	if !ok {
		return
	}
	a.applied(w, r, p, func() (core.Event, error) {
		return a.Board.PostComment(r.Context(), actorOf(p), id, some(body.Body))
	})
}

func (a *API) deleteComment(w http.ResponseWriter, r *http.Request, p Principal) {
	id, ok := a.pathID(w, r, "note")
	if !ok {
		return
	}
	a.applied(w, r, p, func() (core.Event, error) {
		return a.Board.DeleteComment(r.Context(), actorOf(p), id)
	})
}

// undo puts back one activity row, refused by the same rules the websocket's
// undo is refused by: a create, a delete that took the row away, a row already
// undone, and a row whose entity has moved on since.
func (a *API) undo(w http.ResponseWriter, r *http.Request, p Principal) {
	id, ok := a.pathID(w, r, "activity row")
	if !ok {
		return
	}
	a.applied(w, r, p, func() (core.Event, error) {
		return a.Board.Undo(r.Context(), actorOf(p), id)
	})
}

// together runs several commands as one transaction, so a body that names four
// fields either lands whole or not at all, and answers with the last event.
func (a *API) together(w http.ResponseWriter, r *http.Request, run func(context.Context) (core.Event, error)) {
	var e core.Event
	err := a.Board.Together(r.Context(), func(ctx context.Context) error {
		var err error
		e, err = run(ctx)
		return err
	})
	if err != nil {
		a.refuse(w, r, err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"event": e})
}

// Readable is the one visibility test, the same one the board, the documents
// and the search ask: an owner reads every proposition, everybody else reads
// the ones they are a member of. A proposition somebody is not a member of and
// one that is not there answer alike, because the rule is that they are not
// told it is there.
func (a *API) Readable(ctx context.Context, p Principal, proposition int64) error {
	ok, err := board.Readable(ctx, a.db, p.User, proposition)
	if err != nil {
		return err
	}
	if !ok {
		return core.ErrNotFound
	}
	return nil
}

// Proposition is one row this token may read.
func (a *API) Proposition(ctx context.Context, p Principal, id int64) (board.Proposition, error) {
	if err := a.Readable(ctx, p, id); err != nil {
		return board.Proposition{}, err
	}
	return board.GetProposition(ctx, a.db, id)
}

// memberships is every proposition this token's owner is a member of.
func (a *API) memberships(ctx context.Context, p Principal) (map[int64]bool, error) {
	if p.User == nil {
		return map[int64]bool{}, nil
	}
	return board.Memberships(ctx, a.db, p.User.ID)
}

// or is a field a PATCH may leave out, which keeps what the row holds.
func or(v *string, was string) string {
	if v == nil {
		return was
	}
	return *v
}
