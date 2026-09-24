package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/files"
	"github.com/davidtorcivia/theses/internal/integrations"
)

// Add from Drive, in the files pane. Listing needs somebody who can edit
// anything at all; importing needs the standing to add a file to that one
// proposition, which the files service checks the same way it does for an
// upload, so a guest and a person who is not a member are refused there
// whatever they can reach here.

// Unwrap lets http.ResponseController reach the underlying writer through the
// logging recorder. Without it the import cannot move its write deadline and a
// copy longer than the server's WriteTimeout is cut off mid-stream.
func (rec *recorder) Unwrap() http.ResponseWriter { return rec.ResponseWriter }

// importDeadline is how long one import may take to write its answer. It is
// set on the connection rather than on the context, because the work is a copy
// between two other services and the only thing the server is holding is a
// socket with nothing on it.
const importDeadline = 6 * time.Hour

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.log.Warn("could not write a JSON answer", "err", err)
	}
}

// refuseJSON maps a refusal to a status. It follows internal/api's refuse line
// for line, because these two routes answer the same clients as the links and
// files routes beside them and the same refusal must not have two statuses
// depending on which one was asked. It is a second copy only because that one
// is a method on the API and unexported; when it is reachable from here, this
// goes and the calls move to it.
//
// Not there and not allowed are one answer, as they are there: core says the
// role and the membership with one error, and telling them apart would tell
// somebody who is not a member that the proposition exists. A refusal about
// the state of a thing rather than the request is 409, which is what an
// archived proposition and an integration nobody has connected both are.
func (s *Server) refuseJSON(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, core.ErrNotFound), errors.Is(err, core.ErrForbidden):
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "that is not there"})
	case errors.Is(err, board.ErrArchived):
		s.refusal(w, r, http.StatusConflict, err)
	case errors.Is(err, integrations.ErrNotConnected), errors.Is(err, integrations.ErrReconnect):
		s.refusal(w, r, http.StatusConflict, err)
	case errors.Is(err, files.ErrKind), errors.Is(err, files.ErrBadSize),
		errors.Is(err, files.ErrImportSize), errors.Is(err, files.ErrState),
		errors.Is(err, files.ErrSize), errors.Is(err, board.ErrEmpty),
		errors.Is(err, board.ErrTooLong), errors.Is(err, integrations.ErrProvider):
		s.refusal(w, r, http.StatusUnprocessableEntity, err)
	case errors.Is(err, files.ErrNoBucket):
		s.refusal(w, r, http.StatusServiceUnavailable, err)
	default:
		// Nothing above it, so it is a fault here rather than a refusal
		// anybody chose: a statement that would not run, a secret that will
		// not decrypt, a bucket that broke. It goes in the log and comes back
		// saying nothing, the same as every other route under /app.
		s.log.Error("drive request failed", "method", r.Method, "path", logPath(r), "err", err)
		s.writeJSON(w, http.StatusInternalServerError,
			map[string]string{"error": "something went wrong here"})
	}
}

// refusal writes one, with every stored secret taken out of the message.
func (s *Server) refusal(w http.ResponseWriter, r *http.Request, status int, err error) {
	s.writeJSON(w, status, map[string]string{"error": s.redactSecrets(r.Context(), err.Error())})
}

// getDriveList is the picker: one folder's contents, or a search across the
// account when q is given.
func (s *Server) getDriveList(w http.ResponseWriter, r *http.Request) {
	if !auth.Can(userOf(r).Role, auth.CanEdit) {
		s.refuseJSON(w, r, core.ErrForbidden)
		return
	}
	drive, err := s.loadDrive(r.Context())
	if err != nil {
		s.refuseJSON(w, r, err)
		return
	}
	rows, err := drive.List(r.Context(), r.URL.Query().Get("folder"), r.URL.Query().Get("q"))
	if err != nil {
		s.refuseJSON(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"files": rows, "folders": files.Folders})
}

// postDriveImport copies one Drive file into the bucket as a file on this
// proposition, attributed to the person who asked for it. The bytes go from
// Drive through this process to the bucket without being held, which is the one
// exception to the rule that the app never receives file bytes.
func (s *Server) postDriveImport(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Proposition int64  `json:"proposition"`
		File        string `json:"file"`
		Folder      string `json:"folder"`
		Name        string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxFormBytes)).Decode(&in); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "that request could not be read"})
		return
	}
	// Standing first. The files service checks it again, which is the real
	// gate, but by then Drive has been asked whether the file exists and what
	// it is called, and a refusal that is 403 for one id and 422 for another
	// tells somebody who may not import anything what is in the Drive.
	if !auth.Can(userOf(r).Role, auth.CanEdit) {
		s.refuseJSON(w, r, core.ErrForbidden)
		return
	}
	if _, err := s.writable(r, in.Proposition); err != nil {
		s.refuseJSON(w, r, err)
		return
	}
	if key := r.Header.Get("Idempotency-Key"); key != "" {
		var id int64
		err := s.db.QueryRowContext(r.Context(), `SELECT f.id FROM client_keys k JOIN activity a ON a.id=k.activity_id JOIN files f ON f.id=CAST(a.entity_id AS INTEGER) WHERE k.actor_id=? AND k.key=? AND a.entity='file' AND a.action='create' AND f.proposition_id=? AND f.state='ready'`, userOf(r).ID, key, in.Proposition).Scan(&id)
		if err == nil {
			row, err := s.files.ReadFile(r.Context(), core.Actor{Kind: core.KindUser, ID: userOf(r).ID}, id)
			if err != nil {
				s.refuseJSON(w, r, err)
				return
			}
			s.writeJSON(w, http.StatusOK, map[string]any{"file": row})
			return
		}
		if !errors.Is(err, sql.ErrNoRows) {
			s.refuseJSON(w, r, err)
			return
		}
	}
	drive, err := s.loadDrive(r.Context())
	if err != nil {
		s.refuseJSON(w, r, err)
		return
	}
	// The name, size and type are Drive's, read before anything is opened: the
	// row is written with the size the bucket will be asked to hold, and a file
	// that cannot be copied at all is refused before a row exists.
	found, err := drive.Stat(r.Context(), in.File)
	if err != nil {
		s.refuseJSON(w, r, err)
		return
	}
	name := found.Name
	if in.Name != "" {
		name = in.Name
	}

	// The copy is as long as the file is, and the server's write timeout is
	// meant for a page. The deadline moves out for this one response.
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(importDeadline)); err != nil {
		s.log.Warn("could not extend the write deadline for an import", "err", err)
	}

	me := userOf(r)
	row, err := s.files.Import(r.Context(), core.Actor{Kind: core.KindUser, ID: me.ID, Name: me.Name},
		in.Proposition, name, in.Folder, found.Size, func(ctx context.Context) (io.ReadCloser, error) {
			return drive.Open(ctx, found.ID)
		})
	if err != nil {
		s.refuseJSON(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"file": row})
}
