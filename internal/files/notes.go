package files

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
)

var ErrTimestamp = errors.New("timestamp must be within the recording and between 0 and 86400 seconds")
var ErrTags = errors.New("tags cannot contain tabs or line breaks")

func cleanTags(input string) (string, error) {
	var tags []string
	seen := map[string]bool{}
	for _, raw := range strings.Split(input, ",") {
		tag := strings.ToLower(strings.TrimSpace(raw))
		if tag == "" || seen[tag] {
			continue
		}
		if utf8.RuneCountInString(tag) > 32 || len(tags) >= 12 {
			return "", board.ErrTooLong
		}
		if strings.ContainsAny(tag, "\n\r\t") {
			return "", ErrTags
		}
		seen[tag] = true
		tags = append(tags, tag)
	}
	return strings.Join(tags, ", "), nil
}

type FileComment struct {
	ResolvedBy *int64 `json:"resolved_by"`
	ResolvedAt *int64 `json:"resolved_at"`
	Version    int64  `json:"version"`
	ID         int64  `json:"id"`
	File       int64  `json:"file_id"`
	User       *int64 `json:"user_id"`
	Body       string `json:"body_md"`
	Position   int64  `json:"position_ms"`
	Created    int64  `json:"created_at"`
}

func (s *Service) FileComments(ctx context.Context, a core.Actor, id int64) ([]FileComment, error) {
	if _, err := s.readable(ctx, a, id); err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id,file_id,user_id,body_md,position_ms,created_at,resolved_by,resolved_at,version FROM file_comments WHERE file_id=? ORDER BY position_ms,id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FileComment{}
	for rows.Next() {
		var c FileComment
		if err := rows.Scan(&c.ID, &c.File, &c.User, &c.Body, &c.Position, &c.Created, &c.ResolvedBy, &c.ResolvedAt, &c.Version); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Service) AddFileComment(ctx context.Context, a core.Actor, id, position int64, body string) (core.Event, error) {
	body, err := board.Field(body, board.MaxBody)
	if err != nil {
		return core.Event{}, err
	}
	if body == "" {
		return core.Event{}, board.ErrEmpty
	}
	if position < 0 || position > 86400000 {
		return core.Event{}, ErrTimestamp
	}
	return s.comment(ctx, a, id, "comment", func(ctx context.Context, tx *sql.Tx) (FileComment, error) {
		row, err := GetFile(ctx, tx, id)
		if err != nil {
			return FileComment{}, err
		}
		if row.Folder != Recordings || !row.Ready() {
			return FileComment{}, ErrState
		}
		if row.DurationMS != nil && *row.DurationMS > 0 && position > *row.DurationMS {
			return FileComment{}, ErrTimestamp
		}
		created := s.now()
		result, err := tx.ExecContext(ctx, `INSERT INTO file_comments(file_id,user_id,body_md,position_ms,created_at) VALUES(?,?,?,?,?)`, id, by(a), body, position, created)
		if err != nil {
			return FileComment{}, err
		}
		commentID, err := result.LastInsertId()
		if err != nil {
			return FileComment{}, err
		}
		user := a.ID
		return FileComment{ID: commentID, File: id, User: &user, Body: body, Position: position, Created: created, Version: 1}, nil
	})
}

func (s *Service) DeleteFileComment(ctx context.Context, a core.Actor, id, comment int64) (core.Event, error) {
	return s.comment(ctx, a, id, "comment.delete", func(ctx context.Context, tx *sql.Tx) (FileComment, error) {
		var row FileComment
		err := tx.QueryRowContext(ctx, `SELECT id,file_id,user_id,body_md,position_ms,created_at,resolved_by,resolved_at,version FROM file_comments WHERE id=? AND file_id=?`, comment, id).Scan(&row.ID, &row.File, &row.User, &row.Body, &row.Position, &row.Created, &row.ResolvedBy, &row.ResolvedAt, &row.Version)
		if err == sql.ErrNoRows {
			return row, core.ErrNotFound
		}
		if err != nil {
			return row, err
		}
		if row.User == nil || *row.User != a.ID {
			return row, board.ErrNotYours
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM file_comments WHERE id=?`, comment)
		return row, err
	})
}

func (s *Service) comment(ctx context.Context, a core.Actor, id int64, action string, apply func(context.Context, *sql.Tx) (FileComment, error)) (core.Event, error) {
	proposition, err := s.propositionOf(ctx, fileScope, id)
	if err != nil {
		return core.Event{}, err
	}
	return s.do(ctx, a, proposition, auth.CanEdit, "file", action, func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
		was, err := GetFile(ctx, tx, id)
		if err != nil {
			return core.Change{}, err
		}
		row, err := apply(ctx, tx)
		if err != nil {
			return core.Change{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE files SET comment_revision=comment_revision+1 WHERE id=?`, id); err != nil {
			return core.Change{}, err
		}
		now, err := GetFile(ctx, tx, id)
		if err != nil {
			return core.Change{}, err
		}
		type snapshot struct {
			File
			Comment *FileComment `json:"comment,omitempty"`
		}
		before, after := snapshot{File: was}, snapshot{File: now}
		if action == "comment.delete" {
			before.Comment = &row
		} else {
			after.Comment = &row
		}
		return core.Change{Entity: "file", EntityID: id, Action: action, Before: before, After: after}, nil
	})
}

func (s *Service) ResolveComment(ctx context.Context, a core.Actor, file, id, version int64, resolved bool) (core.Event, error) {
	return s.comment(ctx, a, file, "comment.resolve", func(ctx context.Context, tx *sql.Tx) (FileComment, error) {
		var row FileComment
		err := tx.QueryRowContext(ctx, `SELECT id,file_id,user_id,body_md,position_ms,created_at,resolved_by,resolved_at,version FROM file_comments WHERE id=? AND file_id=?`, id, file).Scan(&row.ID, &row.File, &row.User, &row.Body, &row.Position, &row.Created, &row.ResolvedBy, &row.ResolvedAt, &row.Version)
		if errors.Is(err, sql.ErrNoRows) {
			return row, core.ErrNotFound
		}
		if err != nil {
			return row, err
		}
		if row.Version != version {
			return row, &core.ConflictError{Entity: "file_comment", EntityID: id, Field: "version", Version: row.Version, Current: "The comment changed. Reload before resolving it."}
		}
		row.ResolvedAt = nil
		row.ResolvedBy = nil
		if resolved {
			now := s.now()
			user := a.ID
			row.ResolvedAt = &now
			row.ResolvedBy = &user
		}
		row.Version++
		_, err = tx.ExecContext(ctx, `UPDATE file_comments SET resolved_at=?,resolved_by=?,version=? WHERE id=?`, row.ResolvedAt, row.ResolvedBy, row.Version, id)
		return row, err
	})
}
