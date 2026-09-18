package files

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/davidtorcivia/theses/internal/blob"
	"github.com/davidtorcivia/theses/internal/core"
)

// put uploads bytes to a presigned URL the way the browser does, with the
// headers that were signed into it.
func put(t *testing.T, url string, headers map[string]string, body []byte) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(body))
	for k, v := range headers {
		if strings.EqualFold(k, "Host") || strings.EqualFold(k, "Content-Length") {
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		t.Fatalf("PUT %s: %d %s", url, resp.StatusCode, out)
	}
	return resp.Header.Get("ETag")
}

func TestSmallUploadGoesInOnePut(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	body := []byte("tide tables, one page\n")

	up, err := f.Create(ctx, f.who["editor"], f.prop, "tides.pdf", "Documents", int64(len(body)), 0)
	if err != nil {
		t.Fatal(err)
	}
	if up.URL == "" || up.UploadID != 0 {
		t.Fatalf("a small file should get one presigned PUT: %+v", up)
	}
	if up.File.State != stateUploading {
		t.Fatalf("state %q", up.File.State)
	}
	if got := up.Headers["Content-Type"]; got != "application/pdf" {
		t.Fatalf("the signed content type is %q", got)
	}
	put(t, up.URL, up.Headers, body)

	e, err := f.Complete(ctx, f.who["editor"], up.File.ID, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if e.Action != "complete" {
		t.Fatalf("action %q, want complete", e.Action)
	}
	row, err := GetFile(ctx, f.db, up.File.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !row.Ready() {
		t.Fatalf("state %q after complete", row.State)
	}
	if _, err := f.DownloadURL(ctx, f.who["guest"], row.ID); err != nil {
		t.Fatalf("a member may download: %v", err)
	}
	if _, err := f.DownloadURL(ctx, f.who["outsider"], row.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("a stranger downloading: %v, want not found", err)
	}
}

// The size is what the client declared when it asked for the URL. A presigned
// PUT signs the length, so this is the check for everything the browser is not:
// a multipart upload assembled short, a key written by another holder of the
// credentials, a client that asked for a URL under a size it never sent.
func TestCompleteRefusesTheWrongSize(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	up, err := f.Create(ctx, f.who["editor"], f.prop, "tides.pdf", "Documents", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Written past the presigned URL, which is what an attacker with the keys
	// or a bucket with a stale object looks like.
	if err := f.bucket.Put(ctx, up.File.ObjectKey, strings.NewReader("short"), 5, "application/pdf"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Complete(ctx, f.who["editor"], up.File.ID, 0, 0, 0); !errors.Is(err, ErrSize) {
		t.Fatalf("Complete with a short object: %v, want ErrSize", err)
	}
	row, err := GetFile(ctx, f.db, up.File.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != stateUploading {
		t.Fatalf("state %q; a refused completion leaves the file uploading", row.State)
	}
}

func TestCompleteRefusesAnObjectThatIsNotThere(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	up, err := f.Create(ctx, f.who["editor"], f.prop, "tides.pdf", "Documents", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Complete(ctx, f.who["editor"], up.File.ID, 0, 0, 0); !errors.Is(err, ErrState) {
		t.Fatalf("Complete with nothing uploaded: %v", err)
	}
}

func TestLargeUploadResumes(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	// Three parts, the last one short, which is what a real object looks like.
	size := int64(2*blob.PartSize + 16)
	up, err := f.Create(ctx, f.who["editor"], f.prop, "session.wav", Recordings, size, 0)
	if err != nil {
		t.Fatal(err)
	}
	if up.UploadID == 0 || up.PartSize != blob.PartSize {
		t.Fatalf("a large file should open a multipart upload: %+v", up)
	}
	if len(up.Parts) != 3 {
		t.Fatalf("got %d part URLs, want 3", len(up.Parts))
	}
	// The recordings folder goes to the second bucket.
	if _, _, err := f.second.Head(ctx, up.File.ObjectKey); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("the object should be looked for in the recordings bucket: %v", err)
	}

	part := bytes.Repeat([]byte("x"), blob.PartSize)
	put(t, up.Parts[0].URL, nil, part)

	// The tab is closed and opened again: it knows the file id and nothing else.
	again, err := f.Parts(ctx, f.who["editor"], up.File.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Done) != 1 || again.Done[0] != 1 {
		t.Fatalf("resume reports done parts %v, want [1]", again.Done)
	}
	if len(again.Parts) != 2 || again.Parts[0].Number != 2 {
		t.Fatalf("resume offers %+v, want parts 2 and 3", again.Parts)
	}

	put(t, again.Parts[0].URL, nil, part)
	put(t, again.Parts[1].URL, nil, bytes.Repeat([]byte("x"), 16))

	if _, err := f.Complete(ctx, f.who["editor"], up.File.ID, 1000, 0, 0); err != nil {
		t.Fatal(err)
	}
	row, err := GetFile(ctx, f.db, up.File.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !row.Ready() || row.DurationMS == nil || *row.DurationMS != 1000 {
		t.Fatalf("after completing: %+v", row)
	}
	var left int
	if err := f.db.QueryRowContext(ctx, `SELECT count(*) FROM uploads WHERE file_id = ?`, row.ID).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("%d upload rows left behind", left)
	}
}

// The sweep and a completion race for the same parts. Whichever claims the
// uploads row first wins, and the other one stops rather than assembling an
// object out of parts that are being deleted.
func TestSweepAndCompletionDoNotBothWin(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	size := int64(blob.PartSize + 8)
	up, err := f.Create(ctx, f.who["editor"], f.prop, "session.wav", Recordings, size, 0)
	if err != nil {
		t.Fatal(err)
	}
	put(t, up.Parts[0].URL, nil, bytes.Repeat([]byte("x"), blob.PartSize))
	put(t, up.Parts[1].URL, nil, bytes.Repeat([]byte("x"), 8))

	// Two days pass.
	later := time.Now().Add(49 * time.Hour)
	f.Now = func() time.Time { return later }

	if err := f.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := GetFile(ctx, f.db, up.File.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("the swept file is still there: %v", err)
	}
	if _, err := f.Complete(ctx, f.who["editor"], up.File.ID, 0, 0, 0); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("completing a swept upload: %v", err)
	}
}

// The other way round: a completion that got its claim in first pushes the
// deadline out, and the sweep already looking at that row finds nothing to
// claim and leaves the parts where they are.
func TestSweepSkipsAnUploadSomebodyIsFinishing(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	up, err := f.Create(ctx, f.who["editor"], f.prop, "session.wav", Recordings, int64(blob.PartSize+8), 0)
	if err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(49 * time.Hour)
	f.Now = func() time.Time { return later }
	// What Complete does before it talks to the bucket.
	if _, err := f.db.ExecContext(ctx, `UPDATE uploads SET expires_at = ? WHERE file_id = ?`,
		later.Add(abandonAt).Unix(), up.File.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := GetFile(ctx, f.db, up.File.ID); err != nil {
		t.Fatalf("the sweep took an upload that was being finished: %v", err)
	}
	var left int
	if err := f.db.QueryRowContext(ctx,
		`SELECT count(*) FROM uploads WHERE file_id = ?`, up.File.ID).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 1 {
		t.Fatal("the sweep threw away the upload row it could not claim")
	}
}

// A completion that has already claimed the upload wins, and the sweep that
// comes afterwards leaves the finished file alone.
func TestSweepLeavesAFinishedUpload(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	body := []byte("done already")
	up, err := f.Create(ctx, f.who["editor"], f.prop, "tides.pdf", "Documents", int64(len(body)), 0)
	if err != nil {
		t.Fatal(err)
	}
	put(t, up.URL, up.Headers, body)
	if _, err := f.Complete(ctx, f.who["editor"], up.File.ID, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	f.Now = func() time.Time { return time.Now().Add(72 * time.Hour) }
	if err := f.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	row, err := GetFile(ctx, f.db, up.File.ID)
	if err != nil {
		t.Fatalf("the sweep took a finished file: %v", err)
	}
	if !row.Ready() {
		t.Fatalf("state %q", row.State)
	}
}

// A single PUT that never happened has no uploads row of its own, so the sweep
// finds it by its age instead.
func TestSweepTakesAnUploadThatNeverStarted(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	up, err := f.Create(ctx, f.who["editor"], f.prop, "tides.pdf", "Documents", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.Now = func() time.Time { return time.Now().Add(49 * time.Hour) }
	if err := f.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := GetFile(ctx, f.db, up.File.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("an abandoned single PUT is still there: %v", err)
	}
}

// A part URL writes into the bucket, so asking for one needs the standing to
// upload, not the standing to read the list.
func TestPartsNeedTheStandingToUpload(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	up, err := f.Create(ctx, f.who["editor"], f.prop, "session.wav", Recordings, int64(blob.PartSize+8), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, who string
		want      error
	}{
		{"an editor may", "editor", nil},
		{"a researcher may", "researcher", nil},
		{"a guest reads the list but does not upload", "guest", core.ErrForbidden},
		{"a stranger is told nothing is there", "outsider", core.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.Parts(ctx, f.who[tc.who], up.File.ID, 0); !errors.Is(err, tc.want) {
				t.Fatalf("Parts: %v, want %v", err, tc.want)
			}
		})
	}
}

// Asking for the next batch is the sign the upload is still going, so it puts
// the sweep off. Without this a large object on a modest connection is swept
// out from under the browser that is still sending it.
func TestAskingForPartsPutsTheSweepOff(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	up, err := f.Create(ctx, f.who["editor"], f.prop, "session.wav", Recordings, int64(blob.PartSize+8), 0)
	if err != nil {
		t.Fatal(err)
	}
	var before int64
	if err := f.db.QueryRowContext(ctx,
		`SELECT expires_at FROM uploads WHERE file_id = ?`, up.File.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}

	later := time.Now().Add(40 * time.Hour)
	f.Now = func() time.Time { return later }
	if _, err := f.Parts(ctx, f.who["editor"], up.File.ID, 0); err != nil {
		t.Fatal(err)
	}
	var after int64
	if err := f.db.QueryRowContext(ctx,
		`SELECT expires_at FROM uploads WHERE file_id = ?`, up.File.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after <= before {
		t.Fatalf("expires_at is %d, was %d; asking for parts should move it", after, before)
	}

	// Nine hours after that first deadline the upload is still going, so the
	// sweep leaves it where it is.
	f.Now = func() time.Time { return later.Add(time.Hour) }
	if err := f.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := GetFile(ctx, f.db, up.File.ID); err != nil {
		t.Fatalf("the sweep took an upload that had just asked for parts: %v", err)
	}
}

// Deleting a file that is still uploading has to abort its multipart upload as
// well. The uploads row cascades away with it, so nothing would ever find the
// parts again and the bucket would bill for them forever.
func TestDeletingAnUploadAbortsItsParts(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	up, err := f.Create(ctx, f.who["editor"], f.prop, "session.wav", Recordings, int64(blob.PartSize+8), 0)
	if err != nil {
		t.Fatal(err)
	}
	put(t, up.Parts[0].URL, nil, bytes.Repeat([]byte("x"), blob.PartSize))
	if parts, err := f.second.ListParts(ctx, up.File.ObjectKey, multipartOf(t, f, up.File.ID)); err != nil || len(parts) != 1 {
		t.Fatalf("the part did not land: %v %+v", err, parts)
	}
	multipart := multipartOf(t, f, up.File.ID)

	if _, err := f.Delete(ctx, f.who["editor"], up.File.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.second.ListParts(ctx, up.File.ObjectKey, multipart); err == nil {
		t.Fatal("the multipart upload is still open after the file was deleted")
	}
}

func multipartOf(t *testing.T, f *fixture, file int64) string {
	t.Helper()
	var id string
	if err := f.db.QueryRowContext(context.Background(),
		`SELECT multipart_id FROM uploads WHERE file_id = ?`, file).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestVersionsKeepTheOldFile(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	first := f.upload(t, "script.md", "Documents", []byte("draft one"))

	second, err := f.Create(ctx, f.who["editor"], f.prop, "script.md", "Documents", 9, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	// While the replacement is still uploading the old file is what the pane
	// shows, or a paste nobody finished would take it away.
	list, err := f.ListFiles(ctx, f.who["editor"], f.prop)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d files while the replacement uploads, want both", len(list))
	}

	put(t, second.URL, second.Headers, []byte("draft two"))
	if _, err := f.Complete(ctx, f.who["editor"], second.File.ID, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if list, err = f.ListFiles(ctx, f.who["editor"], f.prop); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != second.File.ID {
		t.Fatalf("the pane shows %+v, want only the new version", list)
	}
	older, err := f.Versions(ctx, f.who["editor"], second.File.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(older) != 1 || older[0].ID != first.ID {
		t.Fatalf("versions %+v, want the file it replaced", older)
	}
}

func TestReplacingSomethingElsesFileIsNotFound(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	theirs, err := f.Create(context.Background(), f.who["owner"], f.other, "script.md", "Documents", 9, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Create(ctx, f.who["editor"], f.prop, "script.md", "Documents", 9, theirs.File.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("replacing a file on another proposition: %v", err)
	}
}

func TestMoveBetweenBucketsIsRefused(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	row := f.upload(t, "session.wav", "Documents", []byte("audio"))
	if _, err := f.EditFile(ctx, f.who["editor"], row.ID, "session.wav", Recordings); !errors.Is(err, ErrCrossBucket) {
		t.Fatalf("moving to the recordings bucket: %v, want a refusal", err)
	}
	// With one bucket for everything the same move is fine.
	f.second = f.bucket
	if _, err := f.EditFile(ctx, f.who["editor"], row.ID, "tide session.wav", Recordings); err != nil {
		t.Fatalf("moving inside one bucket: %v", err)
	}
	moved, err := GetFile(ctx, f.db, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if moved.Folder != Recordings || moved.Name != "tide session.wav" {
		t.Fatalf("after the move: %+v", moved)
	}
	if moved.ObjectKey != row.ObjectKey {
		t.Fatalf("the object moved: %q was %q", moved.ObjectKey, row.ObjectKey)
	}
}

func TestDeleteRemovesTheObject(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	row := f.upload(t, "tides.pdf", "Documents", []byte("one page"))

	if _, err := f.Delete(ctx, f.who["researcher"], row.ID); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("a researcher deleting: %v, want forbidden", err)
	}
	e, err := f.Delete(ctx, f.who["editor"], row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.bucket.Head(ctx, row.ObjectKey); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("the object is still in the bucket: %v", err)
	}
	// The object is gone, so there is nothing to put a row back in front of.
	if _, err := f.Undo(ctx, f.who["editor"], e.Seq); !errors.Is(err, core.ErrNotUndoable) {
		t.Fatalf("undoing a file delete: %v, want not undoable", err)
	}
}

func TestThumbnailIsRenderedForAnImage(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	var body bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 900, 600))
	img.Set(3, 3, color.RGBA{R: 255, A: 255})
	if err := png.Encode(&body, img); err != nil {
		t.Fatal(err)
	}
	row := f.upload(t, "cover.png", "Art", body.Bytes())

	if row.Width == nil || *row.Width != 900 || row.Height == nil || *row.Height != 600 {
		t.Fatalf("dimensions %v x %v, want 900 x 600", row.Width, row.Height)
	}
	size, _, err := f.bucket.Head(ctx, thumbKey(row.ObjectKey))
	if err != nil {
		t.Fatalf("no thumbnail beside the original: %v", err)
	}
	if size == 0 {
		t.Fatal("the thumbnail is empty")
	}
	if _, err := f.ThumbURL(ctx, f.who["editor"], row.ID); err != nil {
		t.Fatalf("ThumbURL: %v", err)
	}
	// Nothing is rendered for a file that is not an image, and asking for one
	// says so rather than signing a URL for an object that is not there.
	other := f.upload(t, "notes.md", "Documents", []byte("# notes"))
	if _, err := f.ThumbURL(ctx, f.who["editor"], other.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("ThumbURL for a markdown file: %v", err)
	}
}

func TestFitKeepsTheProportions(t *testing.T) {
	for _, tc := range []struct{ w, h, wantW, wantH int }{
		{900, 600, thumbSide, 320},
		{600, 900, 320, thumbSide},
		{100, 80, 100, 80},
		{10000, 1, thumbSide, 1},
	} {
		w, h := fit(tc.w, tc.h)
		if w != tc.wantW || h != tc.wantH {
			t.Errorf("fit(%d, %d) = %d, %d; want %d, %d", tc.w, tc.h, w, h, tc.wantW, tc.wantH)
		}
	}
}

// upload runs the whole small file flow and returns the ready row, which is
// what most of these tests start from.
func (f *fixture) upload(t *testing.T, name, folder string, body []byte) File {
	t.Helper()
	ctx := context.Background()
	up, err := f.Create(ctx, f.who["editor"], f.prop, name, folder, int64(len(body)), 0)
	if err != nil {
		t.Fatal(err)
	}
	put(t, up.URL, up.Headers, body)
	if _, err := f.Complete(ctx, f.who["editor"], up.File.ID, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	row, err := GetFile(ctx, f.db, up.File.ID)
	if err != nil {
		t.Fatal(err)
	}
	return row
}
