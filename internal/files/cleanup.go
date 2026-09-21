package files

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path"
	"regexp"
	"time"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/store"
)

var ownedObject = regexp.MustCompile(`^[0-9]+-[^/]+/[0-9]+/[A-Z2-7]{26}/(?:[A-Z2-7]{26}/)?[^/]+$`)

var ErrMaintenance = errors.New("backup, restore, or storage maintenance is running; retry when it finishes")

const cleanupGrace = 7 * 24 * time.Hour

type Orphan struct {
	Event  int64  `json:"deletion"`
	File   int64  `json:"file"`
	Folder string `json:"folder"`
	Key    string `json:"key"`
	Size   int64  `json:"size"`
}

func (s *Service) cleanupOwner(ctx context.Context, a core.Actor) error {
	if a.Kind != core.KindUser {
		return core.ErrForbidden
	}
	u, err := store.UserByID(ctx, s.DB, a.ID)
	if err != nil {
		return err
	}
	if u.Role != auth.RoleOwner {
		return core.ErrForbidden
	}
	return nil
}

// ponytail: deletion records bound discovery; unknown objects require manual inspection.
type OrphanReport struct {
	Objects []Orphan `json:"objects"`
	Before  int64    `json:"before"`
	More    bool     `json:"more"`
}

func (s *Service) Orphans(ctx context.Context, a core.Actor) ([]Orphan, error) {
	report, err := s.OrphanPage(ctx, a, 0)
	return report.Objects, err
}
func (s *Service) OrphanPage(ctx context.Context, a core.Actor, before int64) (OrphanReport, error) {
	return s.orphanPage(ctx, a, before, 0)
}
func (s *Service) orphanPage(ctx context.Context, a core.Actor, before, only int64) (OrphanReport, error) {
	if err := s.cleanupOwner(ctx, a); err != nil {
		return OrphanReport{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rows, err := s.DB.QueryContext(ctx, `SELECT id,json_object('id',file_id,'folder',folder,'object_key',object_key) FROM file_cleanup WHERE deleted_at<? AND NOT EXISTS(SELECT 1 FROM trash t WHERE t.entity='file' AND t.entity_id=file_cleanup.file_id AND t.restored_at IS NULL AND t.expires_at>unixepoch()) AND (?=0 OR id<?) AND (?=0 OR id=?) ORDER BY id DESC LIMIT 101`, s.Now().Add(-cleanupGrace).Unix(), before, before, only, only)
	if err != nil {
		return OrphanReport{}, err
	}
	type deletedFile struct {
		File
		Event int64
	}
	var deleted []deletedFile
	report := OrphanReport{Objects: []Orphan{}}
	examined := 0
	for rows.Next() {
		var raw string
		var event int64
		if err := rows.Scan(&event, &raw); err != nil {
			rows.Close()
			return OrphanReport{}, err
		}
		if examined == 100 {
			report.More = true
			break
		}
		examined++
		report.Before = event
		var f File
		if json.Unmarshal([]byte(raw), &f) == nil {
			deleted = append(deleted, deletedFile{File: f, Event: event})
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return OrphanReport{}, err
	}
	out := []Orphan{}
	seen := map[string]bool{}
	for _, f := range deleted {
		if !ownedObject.MatchString(f.ObjectKey) {
			continue
		}
		var live int
		if err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM files WHERE object_key=?`, f.ObjectKey).Scan(&live); err != nil {
			return OrphanReport{}, err
		}
		if live != 0 {
			continue
		}
		bucket, err := s.bucket(ctx, f.Folder)
		if err != nil {
			return OrphanReport{}, err
		}
		for _, key := range []string{f.ObjectKey, thumbKey(f.ObjectKey)} {
			if key == thumbKey(f.ObjectKey) {
				var protected int
				if err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM files WHERE substr(object_key,1,length(?))=?`, path.Dir(f.ObjectKey)+"/", path.Dir(f.ObjectKey)+"/").Scan(&protected); err != nil {
					return OrphanReport{}, err
				}
				if protected > 0 {
					continue
				}
			}
			if key == "" || seen[f.Folder+"\x00"+key] {
				continue
			}
			objects, err := bucket.List(ctx, key)
			if err != nil {
				return OrphanReport{}, err
			}
			for _, object := range objects {
				if object.Key != key || object.Modified.IsZero() || !object.Modified.Before(s.Now().Add(-cleanupGrace)) {
					continue
				}
				seen[f.Folder+"\x00"+key] = true
				out = append(out, Orphan{Event: f.Event, File: f.ID, Folder: f.Folder, Key: key, Size: object.Size})
			}
		}
	}
	report.Objects = out
	return report, nil
}

func (s *Service) CleanupObject(ctx context.Context, a core.Actor, in Orphan) (core.Event, error) {
	if in.Event <= 0 {
		return core.Event{}, core.ErrNotFound
	}
	if s.ReserveMaintenance != nil {
		release, err := s.ReserveMaintenance()
		if err != nil {
			return core.Event{}, ErrMaintenance
		}
		defer release()
	}
	report, err := s.orphanPage(ctx, a, 0, in.Event)
	if err != nil {
		return core.Event{}, err
	}
	found := false
	for _, candidate := range report.Objects {
		if candidate.File == in.File && candidate.Folder == in.Folder && candidate.Key == in.Key {
			in = candidate
			found = true
			break
		}
	}
	if !found {
		return core.Event{}, core.ErrNotFound
	}
	bucket, err := s.bucket(ctx, in.Folder)
	if err != nil {
		return core.Event{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var live int
	if err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM files WHERE object_key=? OR (? LIKE '%/.thumb.jpg' AND substr(object_key,1,length(?))=?)`, in.Key, in.Key, path.Dir(in.Key)+"/", path.Dir(in.Key)+"/").Scan(&live); err != nil {
		return core.Event{}, err
	}
	if live != 0 {
		return core.Event{}, ErrState
	}
	// Generated keys are immutable; the maintenance reservation excludes restored references.
	event, err := s.Do(ctx, a, 0, auth.CanSettings, func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
		return core.Change{Entity: "storage", EntityID: in.File, Action: "cleanup.request", After: in}, nil
	})
	if err != nil {
		return core.Event{}, err
	}
	if err := bucket.Delete(ctx, in.Key); err != nil {
		return core.Event{}, err
	}
	return event, nil
}
