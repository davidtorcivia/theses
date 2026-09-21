package workflow

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/store"
)

type CalendarEntry struct {
	ID      int64  `json:"id"`
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	Date    string `json:"date"`
	Notes   string `json:"notes"`
	Version int64  `json:"version"`
	URL     string `json:"url"`
}

func (s *Service) CalendarEntries(ctx context.Context, a core.Actor) ([]CalendarEntry, error) {
	show, err := board.GetShow(ctx, s.DB)
	if err != nil {
		return nil, err
	}
	if err := s.Readable(ctx, a, show.ID); err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id,'event',title,date,notes,version,'/show#calendar' FROM calendar_events
 UNION ALL SELECT c.id,'task',c.title,c.due_date,'',c.version,'/p/'||p.id||'#card-'||c.id FROM cards c JOIN propositions p ON p.id=c.proposition_id JOIN users u ON u.id=? WHERE c.done_at IS NULL AND p.archived_at IS NULL AND coalesce(c.due_date,'')!='' AND (u.role='owner' OR EXISTS(SELECT 1 FROM proposition_members m WHERE m.proposition_id=p.id AND m.user_id=u.id)) ORDER BY 4,1`, a.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CalendarEntry{}
	for rows.Next() {
		var e CalendarEntry
		if err := rows.Scan(&e.ID, &e.Kind, &e.Title, &e.Date, &e.Notes, &e.Version, &e.URL); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func calendarEvent(ctx context.Context, q store.Querier, id int64) (CalendarEntry, error) {
	e := CalendarEntry{Kind: "event", URL: "/show#calendar"}
	err := q.QueryRowContext(ctx, `SELECT id,title,date,notes,version FROM calendar_events WHERE id=?`, id).Scan(&e.ID, &e.Title, &e.Date, &e.Notes, &e.Version)
	if errors.Is(err, sql.ErrNoRows) {
		err = core.ErrNotFound
	}
	return e, err
}

func (s *Service) SaveCalendarEvent(ctx context.Context, a core.Actor, in CalendarEntry) (core.Event, error) {
	title, err := board.Field(in.Title, board.MaxLine)
	if err != nil {
		return core.Event{}, err
	}
	notes, err := board.Field(in.Notes, board.MaxBody)
	if err != nil {
		return core.Event{}, err
	}
	day, err := time.Parse("2006-01-02", in.Date)
	if err != nil || title == "" || day.Year() < 2000 || day.Year() > 2100 || in.ID < 0 {
		return core.Event{}, ErrInvalid
	}
	show, err := board.GetShow(ctx, s.DB)
	if err != nil {
		return core.Event{}, err
	}
	return s.change(ctx, a, show.ID, "calendar_event", func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
		var before *CalendarEntry
		action := "create"
		if in.ID > 0 {
			was, err := calendarEvent(ctx, tx, in.ID)
			if err != nil {
				return core.Change{}, err
			}
			if was.Version != in.Version {
				return core.Change{}, ErrChanged
			}
			before = &was
			action = "edit"
			_, err = tx.ExecContext(ctx, `UPDATE calendar_events SET title=?,date=?,notes=?,version=version+1,updated_at=unixepoch() WHERE id=?`, title, in.Date, notes, in.ID)
			if err != nil {
				return core.Change{}, err
			}
		} else {
			res, err := tx.ExecContext(ctx, `INSERT INTO calendar_events(title,date,notes,created_by,created_at,updated_at) VALUES(?,?,?,?,unixepoch(),unixepoch())`, title, in.Date, notes, a.ID)
			if err != nil {
				return core.Change{}, err
			}
			in.ID, err = res.LastInsertId()
			if err != nil {
				return core.Change{}, err
			}
		}
		after, err := calendarEvent(ctx, tx, in.ID)
		return core.Change{Entity: "calendar_event", EntityID: in.ID, Action: action, Before: before, After: after}, err
	})
}

func (s *Service) DeleteCalendarEvent(ctx context.Context, a core.Actor, id, version int64) (core.Event, error) {
	show, err := board.GetShow(ctx, s.DB)
	if err != nil {
		return core.Event{}, err
	}
	return s.Do(ctx, a, show.ID, auth.CanDelete, func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
		was, err := calendarEvent(ctx, tx, id)
		if err != nil {
			return core.Change{}, err
		}
		if was.Version != version {
			return core.Change{}, ErrChanged
		}
		if err := s.Allow(ctx, tx, show.ID, "calendar_event", "delete"); err != nil {
			return core.Change{}, err
		}
		if err := s.KeepDeleted(ctx, tx, show.ID, "calendar_event", id, was.Title); err != nil {
			return core.Change{}, err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM calendar_events WHERE id=?`, id)
		return core.Change{Entity: "calendar_event", EntityID: id, Action: "delete", Before: was}, err
	})
}
