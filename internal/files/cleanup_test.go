package files

import (
	"context"
	"errors"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/davidtorcivia/theses/internal/blob"
	"github.com/davidtorcivia/theses/internal/core"
)

func TestCleanupProtectsLiveFilesAndUnknownObjects(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	owner := f.who["owner"]
	up, err := f.Create(ctx, owner, f.prop, "audio.wav", "Documents", 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	put(t, up.URL, up.Headers, []byte("test"))

	old := time.Now().Add(-8 * 24 * time.Hour).Unix()
	if _, err := f.db.ExecContext(ctx, `INSERT INTO file_cleanup(file_id,folder,object_key,deleted_at) VALUES(?,?,?,?)`, up.File.ID, up.File.Folder, up.File.ObjectKey, old); err != nil {
		t.Fatal(err)
	}
	orphans := func(a core.Actor) ([]Orphan, error) {
		report, err := f.OrphanPage(ctx, a, 0)
		return report.Objects, err
	}
	f.Now = func() time.Time { return time.Now().Add(8 * 24 * time.Hour) }
	if _, err := orphans(f.who["editor"]); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("owner guard: %v", err)
	}
	rows, err := orphans(owner)
	if err != nil || len(rows) != 0 {
		t.Fatalf("live upload exposed: %v %v", rows, err)
	}
	if _, err := f.db.ExecContext(ctx, `DELETE FROM files WHERE id=?`, up.File.ID); err != nil {
		t.Fatal(err)
	}
	f.Now = time.Now
	rows, err = orphans(owner)
	if err != nil || len(rows) != 0 {
		t.Fatalf("recent object exposed: %v %v", rows, err)
	}
	f.Now = func() time.Time { return time.Now().Add(8 * 24 * time.Hour) }
	rows, err = orphans(owner)
	if err != nil || len(rows) < 1 {
		t.Fatalf("orphan absent: %v %v key=%s", rows, err, up.File.ObjectKey)
	}
	unknown := rows[0]
	unknown.Key = "backups/archive.enc"
	if _, err := f.CleanupObject(ctx, owner, unknown); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("unknown accepted: %v", err)
	}
	if _, err := f.CleanupObject(ctx, owner, rows[0]); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.bucket.Head(ctx, up.File.ObjectKey); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("not deleted: %v", err)
	}
	if ownedObject.MatchString("backups/1/" + strings.Repeat("A", 26) + "/archive.enc") {
		t.Fatal("backup key accepted")
	}
}

// A thumbnail sits beside every key in its folder, so it is an orphan only
// once no live file has a key under that folder.
func TestCleanupKeepsAThumbnailALiveFileShares(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	owner := f.who["owner"]
	up, err := f.Create(ctx, owner, f.prop, "live.txt", "Documents", 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	put(t, up.URL, up.Headers, []byte("live"))
	dir := path.Dir(up.File.ObjectKey)
	gone, thumb := dir+"/gone.txt", dir+"/.thumb.jpg"
	for _, key := range []string{gone, thumb} {
		if err := f.bucket.Put(ctx, key, strings.NewReader("x"), 1, ""); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-8 * 24 * time.Hour).Unix()
	if _, err := f.db.ExecContext(ctx, `INSERT INTO file_cleanup(file_id,folder,object_key,deleted_at) VALUES(?,?,?,?)`, up.File.ID+1000, up.File.Folder, gone, old); err != nil {
		t.Fatal(err)
	}
	f.Now = func() time.Time { return time.Now().Add(8 * 24 * time.Hour) }
	keys := func() []string {
		report, err := f.OrphanPage(ctx, owner, 0)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, o := range report.Objects {
			out = append(out, o.Key)
		}
		return out
	}
	if got := keys(); len(got) != 1 || got[0] != gone {
		t.Fatalf("with a live file in the folder: %v, want only %s", got, gone)
	}
	if _, err := f.db.ExecContext(ctx, `DELETE FROM files WHERE id=?`, up.File.ID); err != nil {
		t.Fatal(err)
	}
	// The delete's own trigger records live.txt too, so the thumbnail is
	// looked for among the rest.
	if got := keys(); !slices.Contains(got, thumb) || !slices.Contains(got, gone) {
		t.Fatalf("with the folder empty: %v, want %s and %s among them", got, gone, thumb)
	}
}

func TestCleanupFindsSupersededUploadKeysAndCascadeDeletes(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	owner := f.who["owner"]
	up, err := f.Create(ctx, owner, f.prop, "source.txt", "Documents", 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	put(t, up.URL, up.Headers, []byte("test"))
	if _, err := f.Complete(ctx, owner, up.File.ID, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	final, err := f.ReadFile(ctx, owner, up.File.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.bucket.Put(ctx, up.File.ObjectKey, strings.NewReader("test"), 4, "text/plain"); err != nil {
		t.Fatal(err)
	}
	f.Now = func() time.Time { return time.Now().Add(8 * 24 * time.Hour) }
	report, err := f.OrphanPage(ctx, owner, 0)
	if err != nil || len(report.Objects) != 1 || report.Objects[0].Key != up.File.ObjectKey {
		t.Fatalf("source orphan: %+v %v", report, err)
	}
	if _, err := f.CleanupObject(ctx, owner, report.Objects[0]); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.bucket.Head(ctx, final.ObjectKey); err != nil {
		t.Fatalf("final object deleted: %v", err)
	}
	if _, err := f.board.ArchiveProposition(ctx, owner, f.prop); err != nil {
		t.Fatal(err)
	}
	if _, err := f.board.DeleteProposition(ctx, owner, f.prop); err != nil {
		t.Fatal(err)
	}
	report, err = f.OrphanPage(ctx, owner, 0)
	if err != nil || len(report.Objects) != 1 || report.Objects[0].Key != final.ObjectKey {
		t.Fatalf("cascade orphan: %+v %v", report, err)
	}
	for i := 0; i < 101; i++ {
		if _, err := f.db.ExecContext(ctx, `INSERT INTO file_cleanup(file_id,folder,object_key,deleted_at) VALUES(999,'Documents','unknown',?)`, time.Now().Unix()); err != nil {
			t.Fatal(err)
		}
	}
	first, err := f.OrphanPage(ctx, owner, 0)
	if err != nil || !first.More {
		t.Fatalf("pagination: %+v %v", first, err)
	}
	second, err := f.OrphanPage(ctx, owner, first.Before)
	if err != nil || len(second.Objects) != 1 {
		t.Fatalf("older orphan hidden: %+v %v", second, err)
	}
}
