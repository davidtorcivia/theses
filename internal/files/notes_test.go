package files

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/davidtorcivia/theses/internal/core"
)

func TestFileMetadataConflictsAndRecordingComments(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	actor := f.who["editor"]
	up, err := f.Create(ctx, actor, f.prop, "interview.wav", Recordings, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	put(t, up.URL, up.Headers, []byte("test"))
	if _, err := f.Complete(ctx, actor, up.File.ID, 10000, 0, 0); err != nil {
		t.Fatal(err)
	}
	note, tags, version := "A searchable interview", "Research, research, sound", int64(1)
	e, err := f.PatchFileDetails(ctx, actor, up.File.ID, FilePatch{Note: &note, Tags: &tags, Version: &version})
	if err != nil {
		t.Fatal(err)
	}
	if e.Entity != "file" {
		t.Fatalf("event %+v", e)
	}
	row, err := f.ReadFile(ctx, actor, up.File.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Tags != "research, sound" || row.Note != note || row.MetadataVersion != 2 {
		t.Fatalf("metadata %+v", row)
	}
	if _, err := f.PatchFileDetails(ctx, actor, row.ID, FilePatch{Note: &note, Version: &version}); err == nil {
		t.Fatal("stale note accepted")
	}
	name := "renamed.wav"
	rename, err := f.PatchFile(ctx, actor, row.ID, &name, nil)
	if err != nil {
		t.Fatal(err)
	}
	row, _ = f.ReadFile(ctx, actor, row.ID)
	if row.Note != note || row.MetadataVersion != 2 {
		t.Fatal("name edit overwrote metadata")
	}
	if _, err := f.Undo(ctx, actor, rename.Seq); err != nil {
		t.Fatal(err)
	}
	unchanged, _ := f.ReadFile(ctx, actor, row.ID)
	if unchanged.MetadataVersion != row.MetadataVersion {
		t.Fatal("rename undo advanced metadata version")
	}
	var hits int
	if err := f.db.QueryRowContext(ctx, `SELECT count(*) FROM files_fts WHERE files_fts MATCH 'searchable'`).Scan(&hits); err != nil || hits != 1 {
		t.Fatalf("search %d %v", hits, err)
	}
	if _, err := f.Undo(ctx, actor, e.Seq); err != nil {
		t.Fatal(err)
	}
	restored, err := f.ReadFile(ctx, actor, row.ID)
	if err != nil || restored.Note != "" || restored.MetadataVersion <= row.MetadataVersion {
		t.Fatalf("metadata undo: %+v %v", restored, err)
	}
	if _, err := f.AddFileComment(ctx, actor, row.ID, 3000, "Cut this pause"); err != nil {
		t.Fatal(err)
	}
	comments, err := f.FileComments(ctx, actor, row.ID)
	if err != nil || len(comments) != 1 || comments[0].Position != 3000 {
		t.Fatalf("comments %+v %v", comments, err)
	}
	if _, err := f.ResolveComment(ctx, actor, row.ID, comments[0].ID, 1, true); err != nil {
		t.Fatal(err)
	}
	resolved, err := f.FileComments(ctx, actor, row.ID)
	if err != nil || resolved[0].ResolvedAt == nil || resolved[0].Version != 2 {
		t.Fatalf("resolution %+v %v", resolved, err)
	}
	if _, err := f.ResolveComment(ctx, actor, row.ID, comments[0].ID, 1, false); err == nil {
		t.Fatal("stale resolution accepted")
	}
	if _, err := f.ResolveComment(ctx, f.who["guest"], row.ID, comments[0].ID, 2, false); err == nil {
		t.Fatal("guest resolved comment")
	}
	if _, err := f.ResolveComment(ctx, actor, row.ID, comments[0].ID, 2, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.AddFileComment(ctx, actor, row.ID, 10001, "Outside recording"); err == nil {
		t.Fatal("invalid timestamp accepted")
	}
	if _, err := f.FileComments(ctx, f.who["outsider"], row.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("private comments: %v", err)
	}
	if _, err := f.DeleteFileComment(ctx, f.who["owner"], row.ID, comments[0].ID); err == nil {
		t.Fatal("another author's comment deleted")
	}
	deleted, err := f.DeleteFileComment(ctx, actor, row.ID, comments[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(deleted.Before), "Cut this pause") {
		t.Fatal("deleted comment missing from audit")
	}
}
