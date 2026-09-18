package api

import (
	"errors"
	"net/http"

	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/docs"
	"github.com/davidtorcivia/theses/internal/files"
)

// refuse is the one place a command's error becomes a status, for every route
// under /api/v1 and for the same handlers under /app. It is one function rather
// than one per resource so that the same refusal cannot answer two ways
// depending on which file the handler lives in.
//
// The mapping:
//
//   - a stale write is 409 with the conflict beside the message, because the
//     caller's next move is to merge what is there and send it again;
//   - not there and not allowed are both 404, because core answers the role and
//     the membership with one error and saying which would say whether the row
//     is there at all;
//   - a refusal about the state of the thing rather than about the body is 409:
//     an archived proposition, a column with cards still in it, a change that
//     cannot be undone;
//   - somebody else's note is 403, because the caller may write here and not to
//     that row, and being told so gives nothing away;
//   - object storage nobody has set up yet is 503, because it is this side that
//     is not ready;
//   - a body the rules refuse is 422. A body that is not JSON at all is 400,
//     answered by the handlers where they decode it.
func (a *API) refuse(w http.ResponseWriter, r *http.Request, err error) {
	if a.answer(w, err) {
		return
	}
	a.serverError(w, r, err)
}

// refuseInvalid is refuse for a path whose package does not say which of its
// errors the caller caused: one refuse has no status for is answered 422 rather
// than logged as a fault, because the body reached the rules and the rules
// turned it down.
//
// ponytail: the notification package returns a database failure and a channel
// it will not take as the same kind of error, so a failure on that path is
// answered 422 as well. Upgrade path: a sentinel in notify the way settings has
// ErrStorage, after which every caller uses refuse and this goes.
func (a *API) refuseInvalid(w http.ResponseWriter, r *http.Request, err error) {
	if a.answer(w, err) {
		return
	}
	a.fail(w, http.StatusUnprocessableEntity, err.Error())
}

// answer writes the status this error means and reports whether it had one.
func (a *API) answer(w http.ResponseWriter, err error) bool {
	var clash *core.ConflictError
	switch {
	case errors.As(err, &clash):
		a.writeJSON(w, http.StatusConflict, map[string]any{
			"error": "that changed while you were editing it", "conflict": clash})
	case errors.Is(err, core.ErrNotFound), errors.Is(err, core.ErrForbidden):
		a.fail(w, http.StatusNotFound, "that is not there")
	case errors.Is(err, board.ErrArchived), errors.Is(err, board.ErrColumnNotEmpty),
		errors.Is(err, core.ErrNotUndoable):
		a.fail(w, http.StatusConflict, err.Error())
	case errors.Is(err, board.ErrNotYours):
		a.fail(w, http.StatusForbidden, err.Error())
	case errors.Is(err, files.ErrNoBucket):
		a.fail(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, board.ErrEmpty), errors.Is(err, board.ErrTooLong),
		errors.Is(err, board.ErrQuestion), errors.Is(err, docs.ErrNameTaken),
		errors.Is(err, docs.ErrReason), errors.Is(err, docs.ErrTooManyDocuments),
		errors.Is(err, files.ErrKind), errors.Is(err, files.ErrQuestion),
		errors.Is(err, files.ErrURL), errors.Is(err, files.ErrState),
		errors.Is(err, files.ErrSize), errors.Is(err, files.ErrBadSize),
		errors.Is(err, files.ErrSwept), errors.Is(err, files.ErrCrossBucket),
		errors.Is(err, files.ErrPart):
		a.fail(w, http.StatusUnprocessableEntity, err.Error())
	default:
		return false
	}
	return true
}
