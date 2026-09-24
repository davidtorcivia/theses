package api

import (
	"errors"
	"net/http"

	"github.com/davidtorcivia/theses/internal/backup"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/docs"
	"github.com/davidtorcivia/theses/internal/files"
	"github.com/davidtorcivia/theses/internal/notify"
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
//     cannot be undone, markdown written from blocks the database can no longer
//     produce the text of, a backup or a restore already running;
//   - somebody else's note is 403, because the caller may write here and not to
//     that row, and being told so gives nothing away;
//   - object storage or a backup key nobody has set up yet is 503, because it
//     is this side that is not ready;
//   - a body the rules refuse is 422. A body that is not JSON at all is 400,
//     answered by the handlers where they decode it, and so are an
//     Idempotency-Key that is not a key and an edit that names no field: each
//     is the request malformed rather than anything about the workspace.
func (a *API) refuse(w http.ResponseWriter, r *http.Request, err error) {
	if a.answer(w, err) {
		return
	}
	a.serverError(w, r, err)
}

// answer writes the status this error means and reports whether it had one.
func (a *API) answer(w http.ResponseWriter, err error) bool {
	var clash *core.ConflictError
	var refused notify.Refusal
	switch {
	case errors.As(err, &clash):
		a.writeJSON(w, http.StatusConflict, map[string]any{
			"error": "that changed while you were editing it", "conflict": clash})
	case errors.Is(err, core.ErrKey), errors.Is(err, ErrNoField):
		a.fail(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, core.ErrNotFound), errors.Is(err, core.ErrForbidden):
		a.fail(w, http.StatusNotFound, "that is not there")
	case errors.Is(err, backup.ErrBusy), errors.Is(err, core.ErrRestoreConflict), errors.Is(err, files.ErrMaintenance), errors.Is(err, board.ErrLegalReleases), errors.Is(err, board.ErrUploading), errors.Is(err, board.ErrArchived), errors.Is(err, board.ErrShow), errors.Is(err, board.ErrColumnNotEmpty),
		errors.Is(err, core.ErrNotUndoable), errors.Is(err, docs.ErrSourceBase):
		a.fail(w, http.StatusConflict, err.Error())
	case errors.Is(err, board.ErrNotYours):
		a.fail(w, http.StatusForbidden, err.Error())
	case errors.Is(err, core.ErrRestoreBusy), errors.Is(err, files.ErrNoBucket),
		errors.Is(err, backup.ErrNotConfigured), errors.Is(err, ErrNoBackups):
		a.fail(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, notify.ErrStorage):
		// A failure to read or write is this side's, and its detail says
		// nothing to a client, so it falls through to the log and a 500. The
		// notification package names it so that a channel the rules will not
		// take is not answered the same way.
		return false
	case errors.As(err, &refused):
		a.fail(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, ErrUnconfirmed),
		errors.Is(err, board.ErrEmpty), errors.Is(err, board.ErrTooLong),
		errors.Is(err, board.ErrQuestion), errors.Is(err, board.ErrStatus),
		errors.Is(err, board.ErrDueDate),
		errors.Is(err, docs.ErrNameTaken), errors.Is(err, docs.ErrAfterBoth),
		errors.Is(err, docs.ErrReason), errors.Is(err, docs.ErrSourceSpread),
		errors.Is(err, docs.ErrTooManyDocuments),
		errors.Is(err, files.ErrKind), errors.Is(err, files.ErrQuestion),
		errors.Is(err, files.ErrURL), errors.Is(err, files.ErrUnreachable), errors.Is(err, files.ErrState),
		errors.Is(err, files.ErrTimestamp), errors.Is(err, files.ErrTags), errors.Is(err, files.ErrSize), errors.Is(err, files.ErrBadSize),
		errors.Is(err, files.ErrSwept), errors.Is(err, files.ErrCrossBucket),
		errors.Is(err, files.ErrPart):
		a.fail(w, http.StatusUnprocessableEntity, err.Error())
	default:
		return false
	}
	return true
}
