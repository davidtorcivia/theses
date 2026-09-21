package workflow

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/docs"
	"github.com/davidtorcivia/theses/internal/files"
	"github.com/davidtorcivia/theses/internal/store"
)

var ErrInvalid = errors.New("invalid workflow input")
var ErrChanged = errors.New("this version has changed; load the current version before continuing")

type Service struct {
	*core.Service
	Files        *files.Service
	WhisperURL   string
	workerMu     sync.Mutex
	controlMu    sync.Mutex
	workerCancel context.CancelFunc
	paused       bool
}

func (s *Service) Readable(ctx context.Context, a core.Actor, prop int64) error {
	u, err := store.UserByID(ctx, s.DB, a.ID)
	if err != nil {
		return core.ErrNotFound
	}
	ok, err := board.Readable(ctx, s.DB, u, prop)
	if err != nil {
		return err
	}
	if !ok {
		return core.ErrNotFound
	}
	return nil
}
func (s *Service) change(ctx context.Context, a core.Actor, prop int64, entity string, fn func(context.Context, *sql.Tx) (core.Change, error)) (core.Event, error) {
	return s.Do(ctx, a, prop, auth.CanEdit, func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
		if err := s.Allow(ctx, tx, prop, entity, "edit"); err != nil {
			return core.Change{}, err
		}
		return fn(ctx, tx)
	})
}
func target(ctx context.Context, q store.Querier, document, file int64) (prop int64, fingerprint, markdown string, err error) {
	if (document > 0) == (file > 0) {
		err = ErrInvalid
		return
	}
	if document > 0 {
		var d docs.Document
		d, err = docs.GetDocument(ctx, q, document)
		if err != nil {
			return
		}
		prop = d.Proposition
		var blocks []docs.Block
		blocks, err = docs.Blocks(ctx, q, document)
		if err != nil {
			return
		}
		// Block identities and versions make edit-and-revert invalidate an old approval.
		raw, _ := json.Marshal(struct {
			Revision int64
			Blocks   []docs.Block
		}{d.Revision, blocks})
		fingerprint = fmt.Sprintf("%x", sha256.Sum256(raw))
		markdown = docs.Markdown(blocks)
		return
	}
	var f files.File
	f, err = files.GetFile(ctx, q, file)
	if err != nil {
		return
	}
	prop = f.Proposition
	if !f.Ready() || f.Folder != files.Recordings {
		err = ErrInvalid
		return
	}
	fingerprint = fmt.Sprintf("file:%d:%s", f.ID, f.ObjectKey)
	var newer int
	err = q.QueryRowContext(ctx, `SELECT count(*) FROM files WHERE version_of=? AND state='ready'`, file).Scan(&newer)
	if newer > 0 {
		err = ErrChanged
	}
	return
}

type Snapshot struct {
	Proposition int64  `json:"proposition_id"`
	ID          int64  `json:"id"`
	Document    int64  `json:"document_id"`
	Fingerprint string `json:"fingerprint"`
	Markdown    string `json:"markdown"`
	Cues        string `json:"cues"`
	Created     int64  `json:"created_at"`
}

func (s *Service) Pin(ctx context.Context, a core.Actor, document int64, cues string) (core.Event, error) {
	cues, err := board.Field(cues, board.MaxBody)
	if err != nil {
		return core.Event{}, err
	}
	d, err := docs.GetDocument(ctx, s.DB, document)
	if err != nil {
		return core.Event{}, err
	}
	return s.change(ctx, a, d.Proposition, "script_snapshot", func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
		_, fp, md, err := target(ctx, tx, document, 0)
		if err != nil {
			return core.Change{}, err
		}
		row := Snapshot{Proposition: d.Proposition, Document: document, Fingerprint: fp, Markdown: md, Cues: cues, Created: s.Now().Unix()}
		result, err := tx.ExecContext(ctx, `INSERT INTO script_snapshots(proposition_id,document_id,fingerprint,markdown,cues,created_by,created_at) VALUES(?,?,?,?,?,?,?)`, d.Proposition, document, fp, md, cues, a.ID, row.Created)
		if err != nil {
			return core.Change{}, err
		}
		row.ID, err = result.LastInsertId()
		return core.Change{Entity: "script_snapshot", EntityID: row.ID, Action: "create", After: map[string]any{"id": row.ID, "document_id": document, "created_at": row.Created}}, err
	})
}
func (s *Service) Snapshots(ctx context.Context, a core.Actor, document int64) ([]Snapshot, error) {
	d, err := docs.GetDocument(ctx, s.DB, document)
	if err != nil {
		return nil, err
	}
	if err = s.Readable(ctx, a, d.Proposition); err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id,proposition_id,document_id,fingerprint,markdown,cues,created_at FROM script_snapshots WHERE document_id=? ORDER BY id DESC LIMIT 50`, document)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Snapshot{}
	for rows.Next() {
		var row Snapshot
		if err = rows.Scan(&row.ID, &row.Proposition, &row.Document, &row.Fingerprint, &row.Markdown, &row.Cues, &row.Created); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

type Review struct {
	ID          int64  `json:"id"`
	Proposition int64  `json:"proposition_id"`
	Document    *int64 `json:"document_id"`
	File        *int64 `json:"file_id"`
	Snapshot    *int64 `json:"snapshot_id"`
	Fingerprint string `json:"fingerprint"`
	Reviewer    *int64 `json:"reviewer_id"`
	State       string `json:"state"`
	Note        string `json:"note"`
	Version     int64  `json:"version"`
	Created     int64  `json:"created_at"`
	Decided     *int64 `json:"decided_at"`
	Stale       bool   `json:"stale"`
}

const reviewColumns = `id,proposition_id,document_id,file_id,snapshot_id,fingerprint,reviewer_id,state,note,version,created_at,decided_at`

func scanReview(row interface{ Scan(...any) error }) (r Review, err error) {
	err = row.Scan(&r.ID, &r.Proposition, &r.Document, &r.File, &r.Snapshot, &r.Fingerprint, &r.Reviewer, &r.State, &r.Note, &r.Version, &r.Created, &r.Decided)
	if errors.Is(err, sql.ErrNoRows) {
		err = core.ErrNotFound
	}
	return
}
func value(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}
func (s *Service) Reviews(ctx context.Context, a core.Actor, prop int64) ([]Review, error) {
	if err := s.Readable(ctx, a, prop); err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT `+reviewColumns+` FROM reviews WHERE proposition_id=? ORDER BY id DESC LIMIT 100`, prop)
	if err != nil {
		return nil, err
	}
	out := []Review{}
	for rows.Next() {
		r, e := scanReview(rows)
		if e != nil {
			rows.Close()
			return nil, e
		}
		out = append(out, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	cache := map[[2]int64]string{}
	for i := range out {
		key := [2]int64{value(out[i].Document), value(out[i].File)}
		fp, ok := cache[key]
		if !ok {
			var e error
			_, fp, _, e = target(ctx, s.DB, key[0], key[1])
			if e != nil {
				if !errors.Is(e, ErrChanged) && !errors.Is(e, ErrInvalid) && !errors.Is(e, core.ErrNotFound) {
					return nil, e
				}
				fp = ""
			}
			cache[key] = fp
		}
		out[i].Stale = fp == "" || fp != out[i].Fingerprint
	}
	return out, nil
}
func (s *Service) RequestReview(ctx context.Context, a core.Actor, document, file, reviewer int64) (core.Event, error) {
	var prop int64
	var err error
	if (document > 0) == (file > 0) {
		return core.Event{}, ErrInvalid
	}
	if document > 0 {
		var d docs.Document
		d, err = docs.GetDocument(ctx, s.DB, document)
		prop = d.Proposition
	} else {
		var f files.File
		f, err = files.GetFile(ctx, s.DB, file)
		prop = f.Proposition
	}
	if err != nil {
		return core.Event{}, err
	}
	return s.change(ctx, a, prop, "review", func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
		u, err := store.UserByID(ctx, tx, reviewer)
		if err != nil {
			return core.Change{}, core.ErrNotFound
		}
		ok, err := board.Readable(ctx, tx, u, prop)
		if err != nil {
			return core.Change{}, err
		}
		if !ok || !auth.Can(u.Role, auth.CanEdit) {
			return core.Change{}, core.ErrNotFound
		}
		_, fp, md, err := target(ctx, tx, document, file)
		if err != nil {
			return core.Change{}, err
		}
		row := Review{Proposition: prop, Fingerprint: fp, Reviewer: &reviewer, State: "needs_review", Version: 1, Created: s.Now().Unix()}
		if document > 0 {
			row.Document = &document
			res, err := tx.ExecContext(ctx, `INSERT INTO script_snapshots(proposition_id,document_id,fingerprint,markdown,created_by,created_at) VALUES(?,?,?,?,?,?)`, prop, document, fp, md, a.ID, row.Created)
			if err != nil {
				return core.Change{}, err
			}
			id, err := res.LastInsertId()
			if err != nil {
				return core.Change{}, err
			}
			row.Snapshot = &id
		} else {
			row.File = &file
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO reviews(proposition_id,document_id,file_id,snapshot_id,fingerprint,reviewer_id,state,created_at) VALUES(?,?,?,?,?,?,'needs_review',?)`, prop, row.Document, row.File, row.Snapshot, fp, reviewer, row.Created)
		if err != nil {
			return core.Change{}, err
		}
		row.ID, err = res.LastInsertId()
		return core.Change{Entity: "review", EntityID: row.ID, Action: "create", After: row}, err
	})
}
func (s *Service) Decide(ctx context.Context, a core.Actor, id, version int64, state, note string) (core.Event, error) {
	if state != "approved" && state != "changes_requested" {
		return core.Event{}, ErrInvalid
	}
	note, err := board.Field(note, board.MaxBody)
	if err != nil {
		return core.Event{}, err
	}
	before, err := scanReview(s.DB.QueryRowContext(ctx, `SELECT `+reviewColumns+` FROM reviews WHERE id=?`, id))
	if err != nil {
		return core.Event{}, err
	}
	return s.change(ctx, a, before.Proposition, "review", func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
		before, err := scanReview(tx.QueryRowContext(ctx, `SELECT `+reviewColumns+` FROM reviews WHERE id=?`, id))
		if err != nil {
			return core.Change{}, err
		}
		if value(before.Reviewer) != a.ID {
			return core.Change{}, board.ErrNotYours
		}
		_, fp, _, err := target(ctx, tx, value(before.Document), value(before.File))
		if err != nil {
			if errors.Is(err, ErrInvalid) || errors.Is(err, core.ErrNotFound) {
				err = ErrChanged
			}
			return core.Change{}, err
		}
		if before.Version != version || fp != before.Fingerprint {
			return core.Change{}, ErrChanged
		}
		after := before
		now := s.Now().Unix()
		after.State = state
		after.Note = note
		after.Version++
		after.Decided = &now
		_, err = tx.ExecContext(ctx, `UPDATE reviews SET state=?,note=?,version=version+1,decided_at=? WHERE id=?`, state, note, now, id)
		return core.Change{Entity: "review", EntityID: id, Action: "decide", Before: before, After: after}, err
	})
}

func (s *Service) Snapshot(ctx context.Context, a core.Actor, id int64) (Snapshot, error) {
	var row Snapshot
	err := s.DB.QueryRowContext(ctx, `SELECT id,proposition_id,document_id,fingerprint,markdown,cues,created_at FROM script_snapshots WHERE id=?`, id).Scan(&row.ID, &row.Proposition, &row.Document, &row.Fingerprint, &row.Markdown, &row.Cues, &row.Created)
	if errors.Is(err, sql.ErrNoRows) {
		return row, core.ErrNotFound
	}
	if err != nil {
		return row, err
	}
	if err = s.Readable(ctx, a, row.Proposition); err != nil {
		return Snapshot{}, err
	}
	return row, nil
}
