package files

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/blob"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
)

// The two windows the upload flow runs on, and why they are different lengths.
//
// A presigned part URL lives an hour, because a URL that leaks is a write into
// the bucket for as long as it lives. A batch is not assumed to fit in that
// hour: the client is told when its URLs stop working and asks for the next
// batch before they do, so a slow connection re-signs rather than fails. The
// upload as a whole lives 48 hours from the last time anybody asked for parts,
// which is what the sweep abandons it after, so the window is 48 hours of
// silence rather than of wall clock.
const (
	uploadTTL = time.Hour
	abandonAt = 48 * time.Hour
)

// maxFileSize is what the multipart limits allow: 10,000 parts of 64 MiB. A
// larger object cannot be assembled by this flow whatever the bucket permits.
const maxFileSize = 10000 * blob.PartSize

// An Upload is what the browser needs to put one object in the bucket. A small
// file gets URL and Headers and PUTs once; anything over a part gets an upload
// id and a batch of part URLs, and asks for the next batch as it goes.
type Upload struct {
	File     File              `json:"file"`
	URL      string            `json:"url,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	UploadID int64             `json:"upload_id,omitempty"`
	PartSize int64             `json:"part_size,omitempty"`
	Parts    []Part            `json:"parts,omitempty"`
	// Done are the part numbers the bucket already holds, which is what a
	// resumed upload skips.
	Done []int `json:"done,omitempty"`
	// ExpiresAt is when the part URLs stop working, as this server counts
	// time. It is for a person reading the answer.
	ExpiresAt int64 `json:"expires_at,omitempty"`
	// TTLSeconds is how long they last from the moment this answer arrives,
	// which is what the client counts from. A browser clock that is minutes
	// fast would otherwise think every batch it is handed has already expired
	// and ask for another, forever, without sending a byte.
	TTLSeconds int64 `json:"ttl_seconds,omitempty"`
}

// A Part is one presigned part URL.
type Part struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
}

// partBatch is how many part URLs one request hands out. At 64 MiB a part that
// is four gigabytes per round trip, which keeps a fast connection busy; a slow
// one asks again before the hour is out and gets the rest of the same batch
// signed afresh.
const partBatch = 64

// Create records a file and hands back the way to upload it. The row is written
// in a transaction of its own and the bucket is talked to afterwards: an S3
// call inside the transaction would hold the database's write lock for as long
// as the endpoint takes to answer.
//
// replace names a file this one is a new version of, which is the answer to the
// drawer's question when a name already exists in this proposition. Zero keeps
// both.
func (s *Service) Create(ctx context.Context, a core.Actor, proposition int64,
	name, folder string, size, replace int64) (Upload, error) {
	name, err := filename(name)
	if err != nil {
		return Upload{}, err
	}
	if size <= 0 || size > maxFileSize {
		return Upload{}, fmt.Errorf("a file has to be between 1 byte and %d bytes", int64(maxFileSize))
	}
	if !known(folder, Folders) {
		return Upload{}, ErrKind
	}
	if err := s.mayWrite(ctx, a, proposition); err != nil {
		return Upload{}, err
	}
	bucket, err := s.bucket(ctx, folder)
	if err != nil {
		return Upload{}, err
	}

	var row File
	if _, err := s.do(ctx, a, proposition, auth.CanEdit, "file", "create", func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
		if replace != 0 {
			var state string
			var of int64
			err := tx.QueryRowContext(ctx,
				`SELECT proposition_id, state FROM files WHERE id = ?`, replace).Scan(&of, &state)
			if errors.Is(err, sql.ErrNoRows) || (err == nil && of != proposition) {
				return core.Change{}, core.ErrNotFound
			}
			if err != nil {
				return core.Change{}, err
			}
			if state != stateReady {
				return core.Change{}, ErrState
			}
		}
		prefix, err := propositionPrefix(ctx, tx, proposition)
		if err != nil {
			return core.Change{}, err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO files
			(proposition_id, name, folder, kind, size, object_key, version_of,
			 uploaded_by, state, created_at)
			VALUES (?, ?, ?, ?, ?, '', ?, ?, ?, ?)`,
			proposition, name, folder, extension(name), size, null(replace), by(a), stateUploading, s.now())
		if err != nil {
			return core.Change{}, err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return core.Change{}, err
		}
		// The key carries the file's id, which only exists once the row does,
		// so it is written back in the same transaction.
		key := objectKey(prefix, id, name)
		if _, err := tx.ExecContext(ctx, `UPDATE files SET object_key = ? WHERE id = ?`, key, id); err != nil {
			return core.Change{}, err
		}
		if row, err = GetFile(ctx, tx, id); err != nil {
			return core.Change{}, err
		}
		return core.Change{Entity: "file", EntityID: id, Action: "create", After: row}, nil
	}); err != nil {
		return Upload{}, err
	}

	out := Upload{File: row, ExpiresAt: s.Now().Add(uploadTTL).Unix(), TTLSeconds: int64(uploadTTL.Seconds())}
	kind := contentType(name)
	if size <= blob.PartSize {
		url, headers, err := bucket.PresignPut(ctx, row.ObjectKey, kind, size, uploadTTL)
		if err != nil {
			return Upload{}, err
		}
		out.URL, out.Headers = url, headers
		return out, nil
	}

	multipart, err := bucket.StartMultipart(ctx, row.ObjectKey, kind)
	if err != nil {
		return Upload{}, err
	}
	// The uploads row is bookkeeping for a transfer in flight, not an entity
	// anything watches, so it is written on its own rather than through a
	// command: no activity row records that a browser asked for part URLs.
	res, err := s.DB.ExecContext(ctx, `INSERT INTO uploads
		(file_id, multipart_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		row.ID, multipart, s.now(), s.Now().Add(abandonAt).Unix())
	if err != nil {
		return Upload{}, err
	}
	if out.UploadID, err = res.LastInsertId(); err != nil {
		return Upload{}, err
	}
	out.PartSize = blob.PartSize
	if out.Parts, out.Done, err = s.presign(ctx, bucket, row, multipart, 0); err != nil {
		return Upload{}, err
	}
	return out, nil
}

// Parts is the resume: the part numbers the bucket already holds and presigned
// URLs for a batch of the ones it does not, starting after the part number the
// client asks from. A client that reloaded knows only its file id.
func (s *Service) Parts(ctx context.Context, a core.Actor, id int64, after int) (Upload, error) {
	row, err := s.readable(ctx, a, id)
	if err != nil {
		return Upload{}, err
	}
	// A part URL is a licence to write into the bucket, so this needs the same
	// standing as the upload it belongs to. Without it a guest, who may read
	// the list, could ask for one.
	if err := s.mayWrite(ctx, a, row.Proposition); err != nil {
		return Upload{}, err
	}
	if row.State != stateUploading {
		return Upload{}, ErrState
	}
	// after is a number off the wire. Negative, it would sign part zero, which
	// no bucket has; past the end it would sign nothing and the client would
	// ask again forever.
	if after < 0 {
		after = 0
	}
	if after >= partCount(row.Size) {
		return Upload{}, ErrPart
	}
	// ponytail: a file small enough for one PUT has no uploads row, so this
	// answers not found and the browser drops its note and waits for the sweep
	// to clear the row. A resume for those is a second presigned PUT, which is
	// a branch here and a branch in the client, for an upload that is by
	// definition under 64 MiB.
	uploadID, multipart, err := s.upload(ctx, row.ID)
	if err != nil {
		return Upload{}, err
	}
	// Asking for the next batch is the sign somebody is still uploading, so
	// the deadline moves. The 48 hours the sweep counts are 48 hours of
	// silence, not of wall clock: at a modest connection a very large object
	// takes longer than that to send and would otherwise be swept mid-flight.
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE uploads SET expires_at = ? WHERE id = ?`,
		s.Now().Add(abandonAt).Unix(), uploadID); err != nil {
		return Upload{}, err
	}
	bucket, err := s.bucket(ctx, row.Folder)
	if err != nil {
		return Upload{}, err
	}
	out := Upload{
		File: row, UploadID: uploadID, PartSize: blob.PartSize,
		ExpiresAt: s.Now().Add(uploadTTL).Unix(), TTLSeconds: int64(uploadTTL.Seconds()),
	}
	out.Parts, out.Done, err = s.presign(ctx, bucket, row, multipart, after)
	return out, err
}

// partCount is how many parts an object of this size is sent in.
func partCount(size int64) int {
	return int((size + blob.PartSize - 1) / blob.PartSize)
}

// atLeastZero is a number a client measured, floored. Zero is how "not known"
// is stored, so a negative one becomes that.
func atLeastZero(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
}

// abandon throws away a file that cannot be finished: the row, through a
// command so that every tab drops it, and then the object and any parts. It
// acts as the file actor, the one with no person behind it, because the person
// who could not finish the upload may not be allowed to delete.
func (s *Service) abandon(ctx context.Context, row File) {
	actor := core.Actor{Kind: core.KindFile, Name: "the upload sweep"}
	_, multipart, err := s.upload(ctx, row.ID)
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		slog.Warn("could not read the upload of a file being abandoned", "file", row.ID, "err", err)
	}
	if _, err := s.file(ctx, actor, row.ID, auth.CanDelete, "delete", func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM files WHERE id = ?`, row.ID)
		return err
	}); err != nil {
		slog.Warn("could not remove a file that could not be finished", "file", row.ID, "err", err)
		return
	}
	s.forget(ctx, row, multipart)
}

// presign returns URLs for the next batch of parts that are not in the bucket
// yet, and the numbers of the ones that are.
func (s *Service) presign(ctx context.Context, bucket *blob.Client, row File, multipart string, after int) ([]Part, []int, error) {
	total := partCount(row.Size)
	held, err := bucket.ListParts(ctx, row.ObjectKey, multipart)
	if err != nil {
		return nil, nil, err
	}
	have := make(map[int]bool, len(held))
	done := []int{}
	for _, p := range held {
		have[p.Number] = true
		done = append(done, p.Number)
	}
	var want []int
	for n := after + 1; n <= total && len(want) < partBatch; n++ {
		if !have[n] {
			want = append(want, n)
		}
	}
	if len(want) == 0 {
		return []Part{}, done, nil
	}
	urls, err := bucket.PresignParts(ctx, row.ObjectKey, multipart, want, uploadTTL)
	if err != nil {
		return nil, nil, err
	}
	parts := make([]Part, len(want))
	for i, n := range want {
		parts[i] = Part{Number: n, URL: urls[i]}
	}
	return parts, done, nil
}

// Complete is what the browser calls once the last byte is in the bucket. The
// server assembles a multipart upload, checks the object is there and is the
// size the upload declared, renders a thumbnail if it is an image, and only
// then marks the file ready. Everything but the last step is outside the
// transaction.
func (s *Service) Complete(ctx context.Context, a core.Actor, id int64, duration, width, height int64) (core.Event, error) {
	row, err := s.readable(ctx, a, id)
	if err != nil {
		return core.Event{}, err
	}
	if row.State != stateUploading {
		return core.Event{}, ErrState
	}
	if err := s.mayWrite(ctx, a, row.Proposition); err != nil {
		return core.Event{}, err
	}
	bucket, err := s.bucket(ctx, row.Folder)
	if err != nil {
		return core.Event{}, err
	}

	uploadID, multipart, err := s.upload(ctx, row.ID)
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		return core.Event{}, err
	}
	if multipart != "" {
		// The sweep and this completion race for the same parts. Claiming the
		// row first settles it: whichever moves expires_at wins, and the loser
		// finds no row to claim and stops. The claim pushes the deadline out
		// rather than deleting the row, so a completion that then fails on the
		// bucket is still swept later.
		claimed, err := s.DB.ExecContext(ctx,
			`UPDATE uploads SET expires_at = ? WHERE id = ? AND expires_at > ?`,
			s.Now().Add(abandonAt).Unix(), uploadID, s.now())
		if err != nil {
			return core.Event{}, err
		}
		if n, err := claimed.RowsAffected(); err != nil || n != 1 {
			return core.Event{}, ErrSwept
		}
		held, err := bucket.ListParts(ctx, row.ObjectKey, multipart)
		if err != nil {
			return core.Event{}, err
		}
		// CompleteMultipart refuses a gap but not a short tail, so a completion
		// sent before the last parts arrived would assemble a truncated object
		// and throw the upload away with it. The count is checked here, while
		// the parts are still there to go on uploading to.
		if len(held) != partCount(row.Size) {
			return core.Event{}, ErrState
		}
		if err := bucket.CompleteMultipart(ctx, row.ObjectKey, multipart, held); err != nil {
			return core.Event{}, err
		}
	}

	stored, _, err := bucket.Head(ctx, row.ObjectKey)
	if errors.Is(err, blob.ErrNotFound) {
		return core.Event{}, ErrState
	}
	if err != nil {
		return core.Event{}, err
	}
	// The presigned PUT signs the length, so a browser cannot write a different
	// number of bytes through it. This is the check for everything else: a
	// multipart upload assembled from short parts, a key written by some other
	// holder of the credentials, a client that lied about the size to get a URL.
	if stored != row.Size {
		// The object is the wrong size and the multipart upload, if there was
		// one, has already been assembled into it: there is nothing left to go
		// on uploading to. Both are cleared so that the same file can be added
		// again, rather than leaving a row stuck at uploading forever.
		s.abandon(ctx, row)
		return core.Event{}, ErrSize
	}
	// Duration and dimensions are what the browser measured, so they are a
	// number a client chose. Nothing downstream divides by them, but a negative
	// duration draws a clock running backwards.
	duration, width, height = atLeastZero(duration), atLeastZero(width), atLeastZero(height)
	if w, h := s.thumbnail(ctx, bucket, row); w > 0 {
		width, height = w, h
	}

	return s.do(ctx, a, row.Proposition, auth.CanEdit, "file", "complete", func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
		was, err := GetFile(ctx, tx, id)
		if err != nil {
			return core.Change{}, err
		}
		if was.State != stateUploading {
			return core.Change{}, ErrState
		}
		if _, err := tx.ExecContext(ctx, `UPDATE files
			SET state = ?, duration_ms = ?, width = ?, height = ? WHERE id = ?`,
			stateReady, null(duration), null(width), null(height), id); err != nil {
			return core.Change{}, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM uploads WHERE file_id = ?`, id); err != nil {
			return core.Change{}, err
		}
		now, err := GetFile(ctx, tx, id)
		if err != nil {
			return core.Change{}, err
		}
		return core.Change{Entity: "file", EntityID: id, Action: "complete", Before: was, After: now}, nil
	})
}

// EditFile renames a file or moves it to another folder. The object keeps the
// key it was written under: a rename that copied and deleted would break every
// download URL already handed out, and the name a person downloads under comes
// from the row, not the key.
func (s *Service) EditFile(ctx context.Context, a core.Actor, id int64, name, folder string) (core.Event, error) {
	name, err := filename(name)
	if err != nil {
		return core.Event{}, err
	}
	if !known(folder, Folders) {
		return core.Event{}, ErrKind
	}
	was, err := s.readable(ctx, a, id)
	if err != nil {
		return core.Event{}, err
	}
	if was.Folder != folder {
		from, err := s.bucket(ctx, was.Folder)
		if err != nil {
			return core.Event{}, err
		}
		to, err := s.bucket(ctx, folder)
		if err != nil {
			return core.Event{}, err
		}
		// The object stays where it is, so a move that crosses buckets would
		// leave the row pointing into the bucket it came from. blob.Copy is one
		// bucket only, so this is a refusal rather than a copy.
		if from.Bucket() != to.Bucket() {
			return core.Event{}, ErrCrossBucket
		}
	}
	return s.file(ctx, a, id, auth.CanEdit, "update", func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE files SET name = ?, folder = ? WHERE id = ?`, name, folder, id)
		return err
	})
}

// Delete removes the row and then the object. That order loses nothing if the
// second step fails: what is left is an object nobody can reach, which costs
// storage until somebody lists the bucket.
//
// ponytail: the orphan is logged and left. A reaper that lists the bucket and
// deletes keys with no row is the upgrade, and it wants the listing to be cheap
// first.
func (s *Service) Delete(ctx context.Context, a core.Actor, id int64) (core.Event, error) {
	was, err := s.readable(ctx, a, id)
	if err != nil {
		return core.Event{}, err
	}
	// The multipart id is read before the row goes: the cascade takes the
	// uploads row with it, and an upload that is never aborted leaves its parts
	// in the bucket, billed and invisible, with nothing left to find them by.
	_, multipart, err := s.upload(ctx, id)
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		return core.Event{}, err
	}
	event, err := s.file(ctx, a, id, auth.CanDelete, "delete", func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM files WHERE id = ?`, id)
		return err
	})
	if err != nil {
		return core.Event{}, err
	}
	s.forget(ctx, was, multipart)
	return event, nil
}

// forget removes an object, its thumbnail and any multipart upload that was
// still going, reporting nothing: the row is already gone and the person who
// pressed delete has nothing to do about a bucket that refused.
func (s *Service) forget(ctx context.Context, row File, multipart string) {
	bucket, err := s.bucket(ctx, row.Folder)
	if err != nil {
		slog.Warn("file deleted but its object was left behind", "file", row.ID, "err", err)
		return
	}
	if multipart != "" {
		if err := bucket.AbortMultipart(ctx, row.ObjectKey, multipart); err != nil {
			slog.Warn("file deleted but its parts were left behind", "key", row.ObjectKey, "err", err)
		}
	}
	if err := bucket.Delete(ctx, row.ObjectKey); err != nil {
		slog.Warn("file deleted but its object was left behind", "key", row.ObjectKey, "err", err)
	}
	if key := thumbKey(row.ObjectKey); key != "" {
		if err := bucket.Delete(ctx, key); err != nil {
			slog.Warn("file deleted but its thumbnail was left behind", "key", key, "err", err)
		}
	}
}

// file is the shape every file command has.
func (s *Service) file(ctx context.Context, a core.Actor, id int64, need, action string,
	apply func(context.Context, *sql.Tx) error) (core.Event, error) {
	proposition, err := s.propositionOf(ctx, fileScope, id)
	if err != nil {
		return core.Event{}, err
	}
	return s.do(ctx, a, proposition, need, "file", action, func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
		was, err := GetFile(ctx, tx, id)
		if err != nil {
			return core.Change{}, err
		}
		if err := apply(ctx, tx); err != nil {
			return core.Change{}, err
		}
		change := core.Change{Entity: "file", EntityID: id, Action: action, Before: was}
		if action == "delete" {
			return change, nil
		}
		if change.After, err = GetFile(ctx, tx, id); err != nil {
			return core.Change{}, err
		}
		return change, nil
	})
}

// DownloadURL is a presigned GET that expires shortly, with the file's current
// name on it so the browser saves it under that rather than under its key.
func (s *Service) DownloadURL(ctx context.Context, a core.Actor, id int64) (string, error) {
	row, err := s.readable(ctx, a, id)
	if err != nil {
		return "", err
	}
	if !row.Ready() {
		return "", ErrState
	}
	bucket, err := s.bucket(ctx, row.Folder)
	if err != nil {
		return "", err
	}
	return bucket.PresignGet(ctx, row.ObjectKey, row.Name, downloadTTL)
}

// ThumbURL is the same for the thumbnail the binary rendered on completion, or
// ErrNotFound for a file that has none.
func (s *Service) ThumbURL(ctx context.Context, a core.Actor, id int64) (string, error) {
	row, err := s.readable(ctx, a, id)
	if err != nil {
		return "", err
	}
	key := thumbKey(row.ObjectKey)
	if !row.Ready() || key == "" || row.Width == nil {
		return "", core.ErrNotFound
	}
	bucket, err := s.bucket(ctx, row.Folder)
	if err != nil {
		return "", err
	}
	// Rendering a thumbnail is best effort and its failures are logged rather
	// than raised, so knowing the image had dimensions is not knowing the
	// thumbnail was written. Asking the bucket is, and it is one call on a
	// drawer that is already open.
	if _, _, err := bucket.Head(ctx, key); errors.Is(err, blob.ErrNotFound) {
		return "", core.ErrNotFound
	} else if err != nil {
		return "", err
	}
	return bucket.PresignGet(ctx, key, "", downloadTTL)
}

// upload reads the multipart upload in flight for a file, or ErrNotFound when
// the file was small enough to go in one PUT.
func (s *Service) upload(ctx context.Context, file int64) (int64, string, error) {
	var id int64
	var multipart string
	err := s.DB.QueryRowContext(ctx,
		`SELECT id, multipart_id FROM uploads WHERE file_id = ?`, file).Scan(&id, &multipart)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", core.ErrNotFound
	}
	return id, multipart, err
}

// Sweep abandons uploads nobody finished. It runs as the file actor, the one
// with no person behind it, so the delete is published and every tab drops the
// row that had been sitting at ninety percent since the day before.
func (s *Service) Sweep(ctx context.Context) error {
	cutoff := s.now()
	rows, err := s.DB.QueryContext(ctx, `SELECT f.id, u.multipart_id FROM files f
		LEFT JOIN uploads u ON u.file_id = f.id
		WHERE f.state = ? AND ((u.id IS NOT NULL AND u.expires_at < ?)
			OR (u.id IS NULL AND f.created_at < ?))`,
		stateUploading, cutoff, s.Now().Add(-abandonAt).Unix())
	if err != nil {
		return err
	}
	type abandoned struct {
		id        int64
		multipart string
	}
	var found []abandoned
	for rows.Next() {
		var one abandoned
		var multipart sql.NullString
		if err := rows.Scan(&one.id, &multipart); err != nil {
			rows.Close()
			return err
		}
		one.multipart = multipart.String
		found = append(found, one)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	actor := core.Actor{Kind: core.KindFile, Name: "the upload sweep"}
	for _, one := range found {
		row, err := GetFile(ctx, s.DB, one.id)
		if errors.Is(err, core.ErrNotFound) {
			continue
		}
		if err != nil {
			slog.Error("could not read an abandoned upload", "file", one.id, "err", err)
			continue
		}
		// The claim and the delete are one transaction. Claiming first in a
		// transaction of its own would commit the loss of the multipart id
		// before the row it belongs to went, and a failure in between would
		// leave parts in the bucket with nothing left to find them by.
		if _, err := s.file(ctx, actor, one.id, auth.CanDelete, "delete", func(ctx context.Context, tx *sql.Tx) error {
			if one.multipart != "" {
				// A completion that started a moment ago has already moved the
				// deadline, so there is nothing here to claim and this sweep
				// leaves the upload to it.
				claimed, err := tx.ExecContext(ctx,
					`DELETE FROM uploads WHERE file_id = ? AND expires_at < ?`, one.id, cutoff)
				if err != nil {
					return err
				}
				if n, err := claimed.RowsAffected(); err != nil || n == 0 {
					return ErrState
				}
			}
			// The state is checked again inside the transaction: a completion
			// that got through between the listing and here leaves a ready
			// file, which is not abandoned at all.
			res, err := tx.ExecContext(ctx,
				`DELETE FROM files WHERE id = ? AND state = ?`, one.id, stateUploading)
			if err != nil {
				return err
			}
			if n, err := res.RowsAffected(); err != nil || n == 0 {
				return ErrState
			}
			return nil
		}); err != nil {
			// One upload that will not go is not a reason to leave the rest
			// where they are, and the sweep runs again in an hour: it is logged
			// and the run carries on.
			if !errors.Is(err, ErrState) {
				slog.Error("could not abandon an upload", "file", one.id, "err", err)
			}
			continue
		}
		s.forget(ctx, row, one.multipart)
	}
	return nil
}

// bucket is the client for a folder: the recordings bucket when one is
// configured and the folder is Recordings, the primary one otherwise.
func (s *Service) bucket(ctx context.Context, folder string) (*blob.Client, error) {
	if s.Bucket == nil {
		return nil, ErrNoBucket
	}
	return s.Bucket(ctx, folder)
}

// propositionPrefix is the first segment of an object key: the number and a
// slug of the title, so that a bucket listing reads like the rail.
func propositionPrefix(ctx context.Context, tx *sql.Tx, proposition int64) (string, error) {
	var number int64
	var title string
	err := tx.QueryRowContext(ctx,
		`SELECT number, title FROM propositions WHERE id = ?`, proposition).Scan(&number, &title)
	if errors.Is(err, sql.ErrNoRows) {
		return "", core.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%02d-%s", number, slug(title)), nil
}

func objectKey(prefix string, id int64, name string) string {
	return fmt.Sprintf("%s/%d/%s", prefix, id, name)
}

// thumbKey is where a file's thumbnail sits: beside the original, under a name
// no upload can take, because filename refuses a leading dot.
func thumbKey(objectKey string) string {
	dir := path.Dir(objectKey)
	if dir == "." || dir == "/" {
		return ""
	}
	return dir + "/.thumb.jpg"
}

// slug is a title as a path segment: lowercase words joined by dashes, ASCII
// only, because a bucket listing is read in a terminal.
func slug(title string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(title) {
		switch {
		case r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			b.WriteRune(r)
			dash = false
		case b.Len() > 0 && !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "untitled"
	}
	return clip(out, 60)
}

// maxName is what a file name may be once sanitised. Every provider allows far
// more; this is what a person types and what a listing shows on one line.
const maxName = 120

// filename is the gate on the one part of an object key a person chooses. The
// key is built from it, so it may not carry a separator, a dot segment, a
// control character or a leading dot, and blob validates the whole key again
// before it signs anything.
func filename(name string) (string, error) {
	// One spelling. A name typed on one platform and a name typed on another
	// can be the same characters in two encodings, and without this they are
	// two keys in the bucket and two files that never offer to replace each
	// other.
	name = norm.NFC.String(strings.TrimSpace(name))
	// A browser sends the base name, but a form, the API and MCP send whatever
	// they were given, and "../../backups/x" is a path.
	name = path.Base(strings.ReplaceAll(name, `\`, "/"))
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7f:
			// Dropped: a control character in a key is refused by blob and by
			// most providers, and it is never what was meant.
		case r == '/' || r == '"' || r == '\\':
			b.WriteByte('-')
		default:
			b.WriteRune(r)
		}
	}
	name = strings.Trim(strings.TrimSpace(b.String()), ".")
	if name == "" {
		return "", board.ErrEmpty
	}
	if utf8.RuneCountInString(name) > maxName {
		// The extension is what an application opens the file by, so the name
		// is cut in the middle rather than at the end.
		ext := extension(name)
		keep := maxName - len(ext) - 1
		if keep < 1 {
			return "", board.ErrTooLong
		}
		name = strings.TrimRight(string([]rune(name)[:keep]), ".")
		if ext != "" {
			name += "." + ext
		}
		if name == "" {
			return "", board.ErrEmpty
		}
	}
	return name, nil
}

// extension is the kind column: the suffix without its dot, lowercase, empty
// for a name that has none.
func extension(name string) string {
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(name), "."))
	if len(ext) > 16 {
		return ""
	}
	return ext
}

// contentType is what the presigned PUT signs, so the browser has to send
// exactly this header. It is derived here rather than taken from the client,
// because a type the client chose is a type it could set to text/html on a
// public bucket.
func contentType(name string) string {
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		return t
	}
	switch extension(name) {
	case "md":
		return "text/markdown; charset=utf-8"
	case "wav":
		return "audio/wav"
	case "m4a":
		return "audio/mp4"
	}
	return "application/octet-stream"
}

// null turns a zero into a NULL, which is what an unknown duration, size or
// version is. A file of zero bytes is refused before it gets here.
func null(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}
