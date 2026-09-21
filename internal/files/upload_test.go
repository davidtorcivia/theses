package files

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/davidtorcivia/theses/internal/blob"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
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
	if row.ObjectKey == up.File.ObjectKey {
		t.Fatal("ready file still uses the writable upload key")
	}
	put(t, up.URL, up.Headers, bytes.Repeat([]byte("x"), len(body)))
	reader, err := f.bucket.Get(ctx, row.ObjectKey)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("expired workflow changed ready bytes: %q, %v", got, err)
	}
	f.abandon(ctx, up.File)
	if current, err := GetFile(ctx, f.db, row.ID); err != nil || !current.Ready() {
		t.Fatalf("stale cleanup removed the ready file: %+v, %v", current, err)
	}
	if _, err := f.DownloadURL(ctx, f.who["guest"], row.ID); err != nil {
		t.Fatalf("a member may download: %v", err)
	}
	if _, err := f.DownloadURL(ctx, f.who["outsider"], row.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("a stranger downloading: %v, want not found", err)
	}
}

func TestConcurrentSmallCompletionsKeepOnlyTheWinningCopy(t *testing.T) {
	f := setup(t)
	backend := s3mem.New()
	if err := backend.CreateBucket("theses"); err != nil {
		t.Fatal(err)
	}
	fake := gofakes3.New(backend).Server()
	ready := make(chan struct{})
	var copies atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && r.Header.Get("X-Amz-Copy-Source") != "" {
			if copies.Add(1) == 2 {
				close(ready)
			}
			select {
			case <-ready:
			case <-r.Context().Done():
				return
			}
		}
		fake.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	bucket, err := blob.New(blob.Config{
		Provider: "s3", Endpoint: srv.URL, Region: "us-east-1",
		Bucket: "theses", AccessKey: "key", SecretKey: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.bucket = bucket

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	body := []byte("tide tables\n")
	up, err := f.Create(ctx, f.who["editor"], f.prop, "tides.txt", "Documents", int64(len(body)), 0)
	if err != nil {
		t.Fatal(err)
	}
	put(t, up.URL, up.Headers, body)

	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := f.Complete(ctx, f.who["editor"], up.File.ID, 0, 0, 0)
			results <- err
		}()
	}
	var succeeded, refused int
	for range 2 {
		var result error
		select {
		case result = <-results:
		case <-ctx.Done():
			t.Fatal("concurrent completions did not finish")
		}
		switch err := result; {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrState):
			refused++
		default:
			t.Fatalf("Complete: %v", err)
		}
	}
	if succeeded != 1 || refused != 1 || copies.Load() != 2 {
		t.Fatalf("completions: succeeded=%d refused=%d copies=%d", succeeded, refused, copies.Load())
	}
	row, err := GetFile(ctx, f.db, up.File.ID)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := bucket.List(ctx, path.Dir(up.File.ObjectKey)+"/")
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 1 || objects[0].Key != row.ObjectKey {
		t.Fatalf("objects after concurrent completion: %+v; winner %q", objects, row.ObjectKey)
	}
}

func TestFailedSmallCompletionRemovesOnlyItsCopy(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	body := []byte("tide tables\n")
	up, err := f.Create(ctx, f.who["editor"], f.prop, "tides.txt", "Documents", int64(len(body)), 0)
	if err != nil {
		t.Fatal(err)
	}
	put(t, up.URL, up.Headers, body)
	if _, err := f.db.ExecContext(ctx, `CREATE TRIGGER fail_file_complete
		BEFORE INSERT ON activity WHEN NEW.entity = 'file' AND NEW.action = 'complete'
		BEGIN SELECT RAISE(FAIL, 'fail completion'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Complete(ctx, f.who["editor"], up.File.ID, 0, 0, 0); err == nil {
		t.Fatal("Complete succeeded despite the failing activity write")
	}
	row, err := GetFile(ctx, f.db, up.File.ID)
	if err != nil || row.State != stateUploading || row.ObjectKey != up.File.ObjectKey {
		t.Fatalf("row after failed completion: %+v, %v", row, err)
	}
	objects, err := f.bucket.List(ctx, path.Dir(up.File.ObjectKey)+"/")
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 1 || objects[0].Key != up.File.ObjectKey {
		t.Fatalf("objects after failed completion: %+v", objects)
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
	// There is nothing left to go on uploading to, so the row goes with the
	// object rather than sitting at uploading until the sweep reaches it.
	if _, err := GetFile(ctx, f.db, up.File.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("the file row survived a size that could not be right: %v", err)
	}
	if _, _, err := f.bucket.Head(ctx, up.File.ObjectKey); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("the object survived: %v", err)
	}
	// The same file can now be added again, which is the point of clearing it.
	if _, err := f.Create(ctx, f.who["editor"], f.prop, "tides.pdf", "Documents", 100, 0); err != nil {
		t.Fatalf("adding the file again: %v", err)
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

// An archived proposition is exactly where an upload nobody finished is most
// likely to be left, and the archived rule is about what a person may still do
// to one. The sweep is not a person, so it gets through.
func TestSweepClearsAnArchivedProposition(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	small, err := f.Create(ctx, f.who["editor"], f.prop, "tides.pdf", "Documents", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	large, err := f.Create(ctx, f.who["editor"], f.prop, "session.wav", Recordings, int64(blob.PartSize+8), 0)
	if err != nil {
		t.Fatal(err)
	}
	multipart := multipartOf(t, f, large.File.ID)
	put(t, large.Parts[0].URL, nil, bytes.Repeat([]byte("x"), blob.PartSize))
	if _, err := f.board.ArchiveProposition(ctx, f.who["owner"], f.prop); err != nil {
		t.Fatal(err)
	}

	f.Now = func() time.Time { return time.Now().Add(49 * time.Hour) }
	if err := f.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	for _, id := range []int64{small.File.ID, large.File.ID} {
		if _, err := GetFile(ctx, f.db, id); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("file %d survived the sweep: %v", id, err)
		}
	}
	if _, err := f.second.ListParts(ctx, large.File.ObjectKey, multipart); err == nil {
		t.Fatal("the multipart upload was not aborted")
	}
}

// A completion sent before the last part arrived would assemble a short object
// and take the parts with it. It is refused while there is still something to
// upload to.
func TestCompleteRefusesAnUploadThatIsNotFinished(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	up, err := f.Create(ctx, f.who["editor"], f.prop, "session.wav", Recordings, int64(2*blob.PartSize+16), 0)
	if err != nil {
		t.Fatal(err)
	}
	put(t, up.Parts[0].URL, nil, bytes.Repeat([]byte("x"), blob.PartSize))

	if _, err := f.Complete(ctx, f.who["editor"], up.File.ID, 0, 0, 0); !errors.Is(err, ErrState) {
		t.Fatalf("completing one part of three: %v, want ErrState", err)
	}
	row, err := GetFile(ctx, f.db, up.File.ID)
	if err != nil {
		t.Fatalf("the refusal took the file with it: %v", err)
	}
	if row.State != stateUploading {
		t.Fatalf("state %q", row.State)
	}
	// The parts are still there, so the upload carries on from where it was.
	again, err := f.Parts(ctx, f.who["editor"], up.File.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Done) != 1 {
		t.Fatalf("the parts already uploaded are gone: %v", again.Done)
	}
	put(t, again.Parts[0].URL, nil, bytes.Repeat([]byte("x"), blob.PartSize))
	put(t, again.Parts[1].URL, nil, bytes.Repeat([]byte("x"), 16))
	if _, err := f.Complete(ctx, f.who["editor"], up.File.ID, 0, 0, 0); err != nil {
		t.Fatalf("completing once every part is in: %v", err)
	}
}

// after is a number off the wire.
func TestPartsBoundsTheNumberAskedFrom(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	up, err := f.Create(ctx, f.who["editor"], f.prop, "session.wav", Recordings, int64(blob.PartSize+8), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		after int
		want  error
	}{
		{"a negative number is the beginning", -5, nil},
		{"the middle of the upload", 1, nil},
		{"one past the last part", 2, ErrPart},
		{"a number nothing could have", 1 << 40, ErrPart},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.Parts(ctx, f.who["editor"], up.File.ID, tc.after); !errors.Is(err, tc.want) {
				t.Fatalf("Parts(after=%d): %v, want %v", tc.after, err, tc.want)
			}
		})
	}
}

// A duration or a size is what the browser measured, which is a number a client
// chose.
func TestCompleteFloorsWhatTheClientMeasured(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	body := []byte("tide tables")
	up, err := f.Create(ctx, f.who["editor"], f.prop, "tides.pdf", "Documents", int64(len(body)), 0)
	if err != nil {
		t.Fatal(err)
	}
	put(t, up.URL, up.Headers, body)
	if _, err := f.Complete(ctx, f.who["editor"], up.File.ID, -1000, -4, -4); err != nil {
		t.Fatal(err)
	}
	row, err := GetFile(ctx, f.db, up.File.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.DurationMS != nil || row.Width != nil || row.Height != nil {
		t.Fatalf("a negative measurement was stored: %+v", row)
	}
}

// A header can promise far more pixels than the file has bytes, so the size is
// read before anything is decoded.
func TestThumbnailRefusesAnImageTooLargeToDecode(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	row := f.upload(t, "bomb.png", "Art", hugePNG(100000, 100000))

	if row.Width != nil || row.Height != nil {
		t.Fatalf("the header was believed: %v x %v", row.Width, row.Height)
	}
	if _, _, err := f.bucket.Head(ctx, thumbKey(row.ObjectKey)); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("a thumbnail was rendered for it: %v", err)
	}
}

// hugePNG is a PNG header that declares an enormous image and carries none of
// it. DecodeConfig reads the header and stops, which is the point: the size is
// known before a pixel is allocated.
func hugePNG(width, height uint32) []byte {
	var b bytes.Buffer
	b.Write([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	head := make([]byte, 13)
	binary.BigEndian.PutUint32(head[0:], width)
	binary.BigEndian.PutUint32(head[4:], height)
	head[8], head[9] = 8, 6 // eight bits a channel, color with alpha
	chunk(&b, "IHDR", head)
	chunk(&b, "IDAT", []byte{0})
	chunk(&b, "IEND", nil)
	return b.Bytes()
}

func chunk(b *bytes.Buffer, kind string, data []byte) {
	binary.Write(b, binary.BigEndian, uint32(len(data)))
	sum := crc32.NewIEEE()
	b.WriteString(kind)
	sum.Write([]byte(kind))
	b.Write(data)
	sum.Write(data)
	binary.Write(b, binary.BigEndian, sum.Sum32())
}

// Rendering a thumbnail is best effort, so having dimensions is not having a
// thumbnail: the drawer asks the bucket rather than signing a URL for an object
// that may never have been written.
func TestThumbURLRefusesAThumbnailThatIsNotThere(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	var body bytes.Buffer
	if err := png.Encode(&body, image.NewRGBA(image.Rect(0, 0, 40, 30))); err != nil {
		t.Fatal(err)
	}
	row := f.upload(t, "cover.png", "Art", body.Bytes())
	if _, err := f.ThumbURL(ctx, f.who["editor"], row.ID); err != nil {
		t.Fatalf("ThumbURL: %v", err)
	}
	// What a failed write to the bucket leaves behind: a row that says the
	// image had dimensions and no object under the thumbnail's key.
	if err := f.bucket.Delete(ctx, thumbKey(row.ObjectKey)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ThumbURL(ctx, f.who["editor"], row.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("ThumbURL for a thumbnail that is not there: %v, want not found", err)
	}
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

// Nothing a completion or a delete writes is on the list of columns undo may
// put back, so neither of them offers to be taken back: the object in the
// bucket is not something an activity row can restore.
func TestUploadsAreNotUndoable(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	body := []byte("tide tables")
	up, err := f.Create(ctx, f.who["editor"], f.prop, "tides.pdf", "Documents", int64(len(body)), 0)
	if err != nil {
		t.Fatal(err)
	}
	put(t, up.URL, up.Headers, body)
	done, err := f.Complete(ctx, f.who["editor"], up.File.ID, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Undo(ctx, f.who["editor"], done.Seq); !errors.Is(err, core.ErrNotUndoable) {
		t.Fatalf("undoing a completion: %v, want not undoable", err)
	}

	gone, err := f.Delete(ctx, f.who["editor"], up.File.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Undo(ctx, f.who["editor"], gone.Seq); !errors.Is(err, core.ErrNotUndoable) {
		t.Fatalf("undoing a delete: %v, want not undoable", err)
	}
}

// A rename is the one thing about a file that can be taken back.
func TestUndoPutsAFileNameBack(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	row := f.upload(t, "tides.pdf", "Documents", []byte("one page"))
	edit, err := f.EditFile(ctx, f.who["editor"], row.ID, "wrong.pdf", "Reading")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Undo(ctx, f.who["editor"], edit.Seq); err != nil {
		t.Fatalf("Undo: %v", err)
	}
	back, err := GetFile(ctx, f.db, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if back.Name != "tides.pdf" || back.Folder != "Documents" {
		t.Fatalf("after undo: %+v", back)
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

// A create that arrives again under a client key already spent is the resume,
// not a second upload. Before this, the second call started a second multipart
// against the same object and wrote a second uploads row; the client then put
// its parts to the URLs the second call signed, and the completion, which reads
// the upload by file and takes whichever row it finds, looked for them under
// the first and refused for ever.
func TestAKeyedCreateSentTwiceResumesOneMultipartUpload(t *testing.T) {
	f := setup(t)
	size := int64(2*blob.PartSize + 16)
	create := func() Upload {
		t.Helper()
		ctx, err := core.WithKey(context.Background(), "agent-key")
		if err != nil {
			t.Fatal(err)
		}
		up, err := f.Create(ctx, f.who["editor"], f.prop, "session.wav", Recordings, size, 0)
		if err != nil {
			t.Fatal(err)
		}
		return up
	}

	first := create()
	again := create()
	if again.File.ID != first.File.ID {
		t.Fatalf("the second create made file %d, want the first one, %d", again.File.ID, first.File.ID)
	}
	ctx := context.Background()
	var rows int
	if err := f.db.QueryRowContext(ctx,
		`SELECT count(*) FROM uploads WHERE file_id = ?`, first.File.ID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("%d upload rows for one file, want 1", rows)
	}
	if again.UploadID != first.UploadID {
		t.Errorf("the second create answers upload %d, want the one in flight, %d",
			again.UploadID, first.UploadID)
	}
	if len(again.Parts) != 3 || again.PartSize != blob.PartSize {
		t.Fatalf("the second create offers %+v", again.Parts)
	}

	// The URLs the second answer carried are the ones that finish the file.
	part := bytes.Repeat([]byte("x"), blob.PartSize)
	put(t, again.Parts[0].URL, nil, part)
	put(t, again.Parts[1].URL, nil, part)
	put(t, again.Parts[2].URL, nil, bytes.Repeat([]byte("x"), 16))
	if _, err := f.Complete(ctx, f.who["editor"], first.File.ID, 0, 0, 0); err != nil {
		t.Fatalf("completing the upload the second create answered: %v", err)
	}
	row, err := GetFile(ctx, f.db, first.File.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !row.Ready() {
		t.Fatalf("after completing: %+v", row)
	}

	// Completing reads the state before it reaches the command, so a repeat is
	// refused there and the key is never looked at: the caller is told the file
	// is not in a state for this rather than answered with what the first
	// completion did. The change did happen, which is what it wanted to know.
	keyed, err := core.WithKey(ctx, "agent-key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Complete(keyed, f.who["editor"], first.File.ID, 0, 0, 0); !errors.Is(err, ErrState) {
		t.Errorf("completing a ready file again gave %v, want ErrState", err)
	}
}

// The same again for a file small enough to go in one PUT, which has no
// multipart to resume: the second answer signs the same object key again and
// the bytes land once.
func TestAKeyedCreateSentTwiceSignsOneSmallFile(t *testing.T) {
	f := setup(t)
	body := []byte("the tide tables")
	create := func() Upload {
		t.Helper()
		ctx, err := core.WithKey(context.Background(), "agent-key")
		if err != nil {
			t.Fatal(err)
		}
		up, err := f.Create(ctx, f.who["editor"], f.prop, "notes.md", "Documents", int64(len(body)), 0)
		if err != nil {
			t.Fatal(err)
		}
		return up
	}
	first := create()
	again := create()
	if again.File.ID != first.File.ID || again.File.ObjectKey != first.File.ObjectKey {
		t.Fatalf("the second create answers %+v, want the first one's row %+v", again.File, first.File)
	}
	ctx := context.Background()
	put(t, again.URL, again.Headers, body)
	if _, err := f.Complete(ctx, f.who["editor"], first.File.ID, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	var files int
	if err := f.db.QueryRowContext(ctx,
		`SELECT count(*) FROM files WHERE proposition_id = ?`, f.prop).Scan(&files); err != nil {
		t.Fatal(err)
	}
	if files != 1 {
		t.Errorf("%d file rows, want 1", files)
	}
}

func TestUploadReplayUsesCurrentFile(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	keyed := func() context.Context {
		ctx, err := core.WithKey(ctx, "upload-retry")
		if err != nil {
			t.Fatal(err)
		}
		return ctx
	}
	first, err := f.Create(keyed(), f.who["editor"], f.prop, "notes.txt", "Documents", 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.Create(keyed(), f.who["editor"], f.prop, "changed.wav", Recordings, blob.PartSize+1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if again.URL == "" || again.File.ID != first.File.ID || again.Headers["Content-Length"] != "3" {
		t.Fatalf("replay changed upload parameters: %+v", again)
	}
	put(t, again.URL, again.Headers, []byte("abc"))
	if _, err := f.Complete(ctx, f.who["editor"], first.File.ID, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Create(keyed(), f.who["editor"], f.prop, "notes.txt", "Documents", 3, 0); !errors.Is(err, ErrState) {
		t.Fatalf("ready file can be overwritten through replay: %v", err)
	}
	if _, err := f.Delete(ctx, f.who["editor"], first.File.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Create(keyed(), f.who["editor"], f.prop, "notes.txt", "Documents", 3, 0); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("deleted file can be recreated in storage: %v", err)
	}
}

func TestUploadReplayChecksOriginalMembership(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	keyed := func() context.Context {
		ctx, err := core.WithKey(ctx, "upload-access")
		if err != nil {
			t.Fatal(err)
		}
		return ctx
	}
	if _, err := f.Create(keyed(), f.who["editor"], f.prop, "notes.txt", "Documents", 3, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.board.AddMember(ctx, f.who["owner"], f.other, f.who["editor"].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.board.RemoveMember(ctx, f.who["owner"], f.prop, f.who["editor"].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Create(keyed(), f.who["editor"], f.other, "notes.txt", "Documents", 3, 0); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("replay signs a file after membership was removed: %v", err)
	}
}

func TestCompleteResumesAfterMultipartWasAssembled(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	up, err := f.Create(ctx, f.who["editor"], f.prop, "session.wav", Recordings, blob.PartSize+1, 0)
	if err != nil {
		t.Fatal(err)
	}
	put(t, up.Parts[0].URL, nil, bytes.Repeat([]byte("a"), blob.PartSize))
	put(t, up.Parts[1].URL, nil, []byte("b"))
	_, multipart, err := f.Service.upload(ctx, up.File.ID)
	if err != nil {
		t.Fatal(err)
	}
	parts, err := f.second.ListParts(ctx, up.File.ObjectKey, multipart)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.second.CompleteMultipart(ctx, up.File.ObjectKey, multipart, parts); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Complete(ctx, f.who["editor"], up.File.ID, 0, 0, 0); err != nil {
		t.Fatalf("retry after successful assembly: %v", err)
	}
}

func TestFolderMoveDistinguishesStorageEndpoints(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	row := f.upload(t, "notes.txt", "Documents", []byte("abc"))
	// A different S3 service can have a bucket with the same name.
	f.second, _ = buckets(t)
	if _, err := f.EditFile(ctx, f.who["editor"], row.ID, row.Name, Recordings); !errors.Is(err, ErrCrossBucket) {
		t.Fatalf("move to the same bucket name on another endpoint: %v", err)
	}
	f.second = f.bucket
	if _, err := f.EditFile(ctx, f.who["editor"], row.ID, row.Name, Recordings); err != nil {
		t.Fatalf("move between folders sharing a bucket: %v", err)
	}
}

func TestSmallUploadCanResumeAndPatchOnlyNamedFields(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	up, err := f.Create(ctx, f.who["editor"], f.prop, "original.txt", "Documents", 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := f.Parts(ctx, f.who["editor"], up.File.ID, 0)
	if err != nil || resumed.URL == "" {
		t.Fatalf("resume: %+v, %v", resumed, err)
	}
	put(t, resumed.URL, resumed.Headers, []byte("abc"))
	if _, err := f.Complete(ctx, f.who["editor"], up.File.ID, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	name, folder := "renamed.txt", "Reading"
	if _, err := f.PatchFile(ctx, f.who["editor"], up.File.ID, &name, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.PatchFile(ctx, f.who["editor"], up.File.ID, nil, &folder); err != nil {
		t.Fatal(err)
	}
	row, err := GetFile(ctx, f.db, up.File.ID)
	if err != nil || row.Name != name || row.Folder != folder {
		t.Fatalf("patch: %+v, %v", row, err)
	}
}

func TestConcurrentUploadRetriesStartOneMultipart(t *testing.T) {
	f := setup(t)
	const attempts = 12
	start := make(chan struct{})
	answers := make(chan error, attempts)
	for range attempts {
		go func() {
			<-start
			ctx, err := core.WithKey(context.Background(), "same-upload")
			if err == nil {
				_, err = f.Create(ctx, f.who["editor"], f.prop, "session.wav", Recordings, blob.PartSize+1, 0)
			}
			answers <- err
		}()
	}
	close(start)
	for range attempts {
		select {
		case err := <-answers:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("concurrent upload retries did not finish")
		}
	}
	var count int
	if err := f.db.QueryRowContext(context.Background(), `SELECT count(*) FROM uploads`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("%d multipart uploads started for one command", count)
	}
}
