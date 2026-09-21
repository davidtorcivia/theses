package files

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/davidtorcivia/theses/internal/core"
)

// stream is a reader that is not a Seeker, which is what a body arriving from
// another service is and what the import has to be able to write.
func stream(body string) io.Reader {
	return io.LimitReader(bytes.NewReader([]byte(body)), int64(len(body)))
}

func TestImportWritesTheObjectAndMarksTheFileReady(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	const body = "twelve bytes"

	row, err := f.Import(ctx, f.who["editor"], f.prop, "Interview.wav", Recordings, int64(len(body)), stream(body))
	if err != nil {
		t.Fatal(err)
	}
	if !row.Ready() {
		t.Fatalf("the file is at %q", row.State)
	}
	if row.Size != int64(len(body)) {
		t.Fatalf("the row says %d bytes", row.Size)
	}
	if row.UploadedBy == nil || *row.UploadedBy != f.who["editor"].ID {
		t.Fatalf("the file is attributed to %v", row.UploadedBy)
	}
	// Recordings goes to the second bucket, like every other file in it.
	read, err := f.second.Get(ctx, row.ObjectKey)
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	got, err := io.ReadAll(read)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("the bucket holds %q", got)
	}
}

func TestImportRefusesWhatTheUploadPathWouldRefuse(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	cases := []struct {
		name   string
		actor  core.Actor
		prop   int64
		file   string
		folder string
		size   int64
		want   string
	}{
		{"a guest", f.who["guest"], f.prop, "a.wav", Recordings, 3, "forbidden"},
		{"somebody who is not a member", f.who["outsider"], f.prop, "a.wav", Recordings, 3, "not found"},
		{"a folder that is not one of ours", f.who["editor"], f.prop, "a.wav", "Elsewhere", 3, "kind"},
		{"a size of zero", f.who["editor"], f.prop, "a.wav", Recordings, 0, "1 byte"},
		{"a size past the single put limit", f.who["editor"], f.prop, "a.wav", Recordings, maxImport + 1, "1 byte"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := f.Import(ctx, c.actor, c.prop, c.file, c.folder, c.size, stream("abc"))
			if err == nil {
				t.Fatal("the import was allowed")
			}
			switch c.want {
			case "forbidden":
				if !errors.Is(err, core.ErrForbidden) {
					t.Fatalf("gave %v", err)
				}
			case "not found":
				if !errors.Is(err, core.ErrNotFound) {
					t.Fatalf("gave %v", err)
				}
			case "kind":
				if !errors.Is(err, ErrKind) {
					t.Fatalf("gave %v", err)
				}
			default:
				if !strings.Contains(err.Error(), c.want) {
					t.Fatalf("gave %v", err)
				}
			}
			// Nothing was recorded for any of them.
			rows, err := f.ListFiles(ctx, f.who["owner"], f.prop)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 0 {
				t.Fatalf("a refused import left %d rows", len(rows))
			}
		})
	}
}

// A source that stops early leaves a row at uploading with no object behind
// it, which is the state nothing can be done with. The import clears both.
func TestImportLeavesNothingBehindWhenTheSourceStopsEarly(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	_, err := f.Import(ctx, f.who["editor"], f.prop, "Interview.wav", Recordings, 64, stream("only ten."))
	if err == nil {
		t.Fatal("a body that stopped early was accepted")
	}
	rows, err := f.ListFiles(ctx, f.who["owner"], f.prop)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("a failed import left %+v", rows)
	}
}

// The likeliest partial failure is the person going away mid copy, which
// cancels the context the import is running on. Clearing up still has to
// happen, or the row sits at uploading with no object behind it.
func TestImportLeavesNothingBehindWhenTheRequestIsCancelled(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := f.Import(ctx, f.who["editor"], f.prop, "Interview.wav", Recordings, 64,
		readerThatCancels(cancel, 64))
	if err == nil {
		t.Fatal("a canceled copy was accepted")
	}
	rows, err := f.ListFiles(context.Background(), f.who["owner"], f.prop)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("a canceled import left %+v", rows)
	}
}

// readerThatCancels hands out one byte and then pulls the context out from
// under whoever is reading it.
func readerThatCancels(cancel func(), n int) io.Reader {
	first := true
	return readerFunc(func(p []byte) (int, error) {
		if first {
			first = false
			p[0] = 'x'
			return 1, nil
		}
		cancel()
		return 0, context.Canceled
	})
}

type readerFunc func([]byte) (int, error)

func (r readerFunc) Read(p []byte) (int, error) { return r(p) }

func TestImportReplayKeepsCompletedBytes(t *testing.T) {
	f := setup(t)
	keyed := func() context.Context {
		ctx, err := core.WithKey(context.Background(), "import-retry")
		if err != nil {
			t.Fatal(err)
		}
		return ctx
	}
	first, err := f.Import(keyed(), f.who["editor"], f.prop, "notes.txt", "Documents", 3, stream("abc"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.Import(keyed(), f.who["editor"], f.prop, "notes.txt", "Documents", 3, stream("xyz"))
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID || !again.Ready() {
		t.Fatalf("replayed file: %+v", again)
	}
	r, err := f.bucket.Get(context.Background(), first.ObjectKey)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	body, err := io.ReadAll(r)
	if err != nil || string(body) != "abc" {
		t.Fatalf("replay changed contents: %q, %v", body, err)
	}
}

// And a source that is longer than it said is cut to the declared length, so
// the object is always the size the row promises.
func TestImportWritesNoMoreThanTheDeclaredSize(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	row, err := f.Import(ctx, f.who["editor"], f.prop, "Interview.wav", Recordings, 5, stream("far more than five"))
	if err != nil {
		t.Fatal(err)
	}
	stored, _, err := f.second.Head(ctx, row.ObjectKey)
	if err != nil {
		t.Fatal(err)
	}
	if stored != 5 {
		t.Fatalf("the object is %d bytes", stored)
	}
}

// The same reader through the blob client on its own, which is the thing that
// cannot be done with Put: it signs the payload and so has to rewind.
func TestPutStreamTakesAReaderThatCannotRewind(t *testing.T) {
	primary, _ := buckets(t)
	ctx := context.Background()
	const body = "not seekable"

	if err := primary.Put(ctx, "a/seekable.bin", stream(body), int64(len(body)), ""); err == nil {
		t.Fatal("Put took a reader it cannot rewind")
	}
	if err := primary.PutStream(ctx, "a/streamed.bin", stream(body), int64(len(body)), ""); err != nil {
		t.Fatal(err)
	}
	n, _, err := primary.Head(ctx, "a/streamed.bin")
	if err != nil || n != int64(len(body)) {
		t.Fatalf("the object is %d bytes: %v", n, err)
	}
	if err := primary.PutStream(ctx, "a/empty.bin", stream(""), 0, ""); err == nil {
		t.Fatal("an empty object was written through PutStream")
	}
}

// The presigned PUT signs a content type, and the browser has to send exactly
// it. A name ending in a dot or a space has no extension until filename has
// trimmed it, so the type has to be read off the row rather than off what came
// in, or the bucket is told octet-stream for a file the list calls markdown.
func TestCreateSignsTheTypeOfTheStoredName(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	cases := []struct{ name, stored, want string }{
		{"notes.md", "notes.md", "text/markdown; charset=utf-8"},
		{"notes.md.", "notes.md", "text/markdown; charset=utf-8"},
		{"notes.md ", "notes.md", "text/markdown; charset=utf-8"},
		{"take one.wav", "take one.wav", "audio/wav"},
		{"unlabeled", "unlabeled", "application/octet-stream"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up, err := f.Create(ctx, f.who["editor"], f.prop, c.name, "Documents", 10, 0)
			if err != nil {
				t.Fatal(err)
			}
			if up.File.Name != c.stored {
				t.Fatalf("the row is called %q, want %q", up.File.Name, c.stored)
			}
			if got := up.Headers["Content-Type"]; got != c.want {
				t.Fatalf("the PUT signs %q, want %q", got, c.want)
			}
		})
	}
}
