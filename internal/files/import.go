package files

import (
	"context"
	"fmt"
	"io"

	"github.com/davidtorcivia/theses/internal/core"
)

// maxImport is the largest object an import may write. A browser upload larger
// than this goes to the bucket in parts; this one is a single PutObject, which
// is what S3 allows at most.
//
// ponytail: the ceiling is 5 GiB. The upgrade is UploadPart on the blob client
// and a loop here, which is worth writing the first time somebody has a
// recording in Drive that will not fit.
const maxImport = 5 << 30

// ErrImportSize is a file too large to come in this way, or one with nothing
// in it. It is a sentinel rather than a sentence so that a caller mapping
// errors to statuses answers it as a refusal instead of a fault, which is what
// ErrBadSize does for the browser's own uploads.
var ErrImportSize = fmt.Errorf("a file imported this way has to be between 1 byte and %d GB", int64(maxImport)>>30)

// Import is the one place the app receives file bytes. Everything else goes
// browser to bucket over a presigned URL; a file that lives in somebody else's
// service cannot, because the browser has no credentials for it, so the bytes
// come through here on their way from one to the other.
//
// They are never held: the reader is passed to the bucket and copied straight
// out of it, so a six gigabyte recording costs a buffer, not a disk. The row
// exists at uploading before the first byte moves and is marked ready only
// after the object has been read back and found to be the size it was declared,
// which is the same check every browser upload passes. A copy that fails partway
// leaves nothing behind: the row goes and the object with it, so the same file
// can be imported again rather than sitting at uploading forever.
//
// name, folder and size are the caller's to validate as far as they can; the
// checks the browser path runs are run here too, on the same function.
func (s *Service) Import(ctx context.Context, a core.Actor, proposition int64,
	name, folder string, size int64, body io.Reader) (File, error) {
	if size <= 0 || size > maxImport {
		return File{}, ErrImportSize
	}
	row, bucket, err := s.record(ctx, a, proposition, name, folder, size, 0)
	if err != nil {
		return File{}, err
	}
	// Exactly the declared length is sent, whatever the other end goes on
	// offering. A short body fails on the way out, and a long one is cut here
	// rather than becoming an object that is not the size the row says.
	if err := bucket.PutStream(ctx, row.ObjectKey, io.LimitReader(body, size), size, contentType(row.Name)); err != nil {
		// The likeliest way to get here is the browser going away mid copy,
		// which cancels this context. Clearing up needs a live one, or the row
		// stays at uploading with nothing behind it until the sweep.
		s.abandon(context.WithoutCancel(ctx), row)
		return File{}, fmt.Errorf("the file could not be written to the bucket: %w", err)
	}
	// The same completion a browser upload runs: the object is read back, its
	// size checked against the row, a thumbnail rendered if it is an image, and
	// the row marked ready. A size that does not match abandons the file in
	// there and comes back as ErrSize.
	if _, err := s.Complete(ctx, a, row.ID, 0, 0, 0); err != nil {
		return File{}, err
	}
	return GetFile(ctx, s.DB, row.ID)
}
