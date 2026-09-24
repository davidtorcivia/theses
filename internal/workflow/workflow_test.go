package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/docs"
	"github.com/davidtorcivia/theses/internal/store"
)

func TestWorkflowVersionsAndBoundaries(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTemp(t)
	c := core.New(db, core.NewBus())
	b := board.New(c, func() board.Defaults { return board.Defaults{Status: "idea", Columns: []string{"Research"}} })
	d := docs.New(c, "", func() string { return "# Script\n\nFirst claim." }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s := New(c)
	actors := map[string]core.Actor{}
	for _, role := range []string{"owner", "editor", "guest"} {
		id, err := store.CreateUser(ctx, db, &store.User{Handle: role, Email: role + "@example.com", Name: role, Role: role, PasswordHash: "x"})
		if err != nil {
			t.Fatal(err)
		}
		actors[role] = core.Actor{Kind: core.KindUser, ID: id}
	}
	p, err := b.CreateProposition(ctx, actors["owner"], "Episode")
	if err != nil {
		t.Fatal(err)
	}
	prop := p.EntityID
	doc, err := d.CreateDocument(ctx, actors["owner"], prop, "Script")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.RequestReview(ctx, actors["editor"], doc.EntityID, 0, actors["owner"].ID); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("outsider review: %v", err)
	}
	if _, err = s.Pin(ctx, actors["guest"], doc.EntityID, ""); err == nil {
		t.Fatal("guest pinned")
	}
	requested, err := s.RequestReview(ctx, actors["owner"], doc.EntityID, 0, actors["owner"].ID)
	if err != nil {
		t.Fatal(err)
	}
	var review Review
	if err = json.Unmarshal(requested.After, &review); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Decide(ctx, actors["owner"], review.ID, 1, "approved", "Ready"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Decide(ctx, actors["owner"], review.ID, 1, "changes_requested", ""); !errors.Is(err, ErrChanged) {
		t.Fatalf("stale decision %v", err)
	}
	snapshot, err := s.Snapshot(ctx, actors["owner"], *review.Snapshot)
	if err != nil || !strings.Contains(snapshot.Markdown, "First claim") {
		t.Fatalf("snapshot %+v %v", snapshot, err)
	}
	// Moving a block away and back leaves the contents unchanged but advances document revision.
	if _, err = db.ExecContext(ctx, `UPDATE blocks SET position=position WHERE document_id=?`, doc.EntityID); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Reviews(ctx, actors["owner"], prop)
	if err != nil || !rows[0].Stale {
		t.Fatalf("revision did not invalidate %+v %v", rows, err)
	}
	if _, err = s.Decide(ctx, actors["owner"], review.ID, 2, "approved", ""); !errors.Is(err, ErrChanged) {
		t.Fatalf("changed script %v", err)
	}
	evidence, err := s.SaveEvidence(ctx, actors["owner"], Evidence{Proposition: prop, Title: "Source", URL: "https://example.com/source", Quotation: "Exact quotation", Locator: "p. 42", Claim: "First claim", Verified: true})
	if err != nil {
		t.Fatal(err)
	}
	var e Evidence
	json.Unmarshal(evidence.After, &e)
	if e.VerifiedBy == nil || *e.VerifiedBy != actors["owner"].ID {
		t.Fatal("verification attribution")
	}
	e.Version = 0
	if _, err = s.SaveEvidence(ctx, actors["owner"], e); !errors.Is(err, ErrChanged) {
		t.Fatalf("stale evidence %v", err)
	}
	if _, err = s.SaveEvidence(ctx, actors["owner"], Evidence{Proposition: prop, Title: "Bad URL", URL: "javascript:alert(1)"}); err == nil {
		t.Fatal("unsafe URL")
	}
	if _, err = s.Evidence(ctx, actors["editor"], prop); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("private evidence %v", err)
	}
	exported, err := ExportEvidence([]Evidence{{Title: "Title\nTY  - BAD", Author: "Someone", Quotation: "quote"}}, "ris")
	if err != nil || strings.Count(exported, "\nTY  -") > 0 {
		t.Fatalf("RIS injection %q %v", exported, err)
	}
	if _, err = s.DeleteEvidence(ctx, actors["owner"], e.ID, 0); !errors.Is(err, ErrChanged) {
		t.Fatalf("stale delete %v", err)
	}
	if _, err = s.DeleteEvidence(ctx, actors["owner"], e.ID, 1); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM evidence_fts WHERE evidence_fts MATCH 'quotation'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("stale search row %d %v", count, err)
	}
	if _, err = s.Pin(ctx, actors["owner"], doc.EntityID, "Say this slowly"); err != nil {
		t.Fatal(err)
	}
	if _, err = d.DeleteDocument(ctx, actors["owner"], doc.EntityID); err != nil {
		t.Fatal(err)
	}
	if pinned, err := s.Snapshot(ctx, actors["owner"], *review.Snapshot); err != nil || pinned.Markdown != snapshot.Markdown {
		t.Fatalf("lost pinned history %+v %v", pinned, err)
	}
	if _, err := s.Snapshot(ctx, actors["editor"], *review.Snapshot); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("private historical snapshot %v", err)
	}
	history, err := s.Reviews(ctx, actors["owner"], prop)
	if err != nil || len(history) != 1 || !history[0].Stale {
		t.Fatalf("lost review history %+v %v", history, err)
	}
	doc, err = d.CreateDocument(ctx, actors["owner"], prop, "Replacement script")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.ArchiveProposition(ctx, actors["owner"], prop); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Pin(ctx, actors["owner"], doc.EntityID, ""); !errors.Is(err, board.ErrArchived) {
		t.Fatalf("archived pin %v", err)
	}
}
func TestTranscriptFormats(t *testing.T) {
	good := []struct {
		format, body string
		start        int64
		speaker      string
	}{{"srt", "1\n00:00:01,250 --> 00:00:02,500\nA &amp; B\n", 1250, ""}, {"vtt", "WEBVTT\n\n00:01.250 --> 00:02.500 align:start\n<v Speaker 1>A &amp; B</v>\n", 1250, "Speaker 1"}}
	for _, tc := range good {
		segments, err := ParseTranscript(tc.body, tc.format)
		if err != nil || len(segments) != 1 || *segments[0].Start != tc.start || segments[0].Speaker != tc.speaker || segments[0].Text != "A & B" {
			t.Fatalf("%s %+v %v", tc.format, segments, err)
		}
		body, err := ExportTranscript(Transcript{Segments: segments}, "vtt")
		if err != nil {
			t.Fatal(err)
		}
		again, err := ParseTranscript(body, "vtt")
		if err != nil || again[0].Text != "A & B" || again[0].Speaker != tc.speaker {
			t.Fatalf("roundtrip %+v %v", again, err)
		}
	}
	for _, body := range []string{"", "not a cue", "1\n00:00:05,000 --> 00:00:01,000\nreverse", "1\n00:61:00,000 --> 00:62:00,000\nbad"} {
		if _, err := ParseTranscript(body, "srt"); err == nil {
			t.Fatalf("accepted %q", body)
		}
	}
	plain, err := ParseTranscript("An untimed paragraph.\n\nAnother.", "txt")
	if err != nil || len(plain) != 2 || plain[0].Start != nil {
		t.Fatalf("plain %+v %v", plain, err)
	}
	if _, err := ExportTranscript(Transcript{Segments: plain}, "vtt"); err == nil {
		t.Fatal("invented timestamps")
	}
}

func TestWebVTTCueIdentifiers(t *testing.T) {
	for _, name := range []string{"NOTEWORTHY", "WEBVTT-quote", "WEBVTT"} {
		segments, err := ParseTranscript("WEBVTT\n\n"+name+"\n00:00:01.000 --> 00:00:02.000\nKeep this cue.\n", "vtt")
		if err != nil || len(segments) != 1 {
			t.Fatalf("%s %+v %v", name, segments, err)
		}
	}
}

func TestEvidenceVerificationAndOrphanReview(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTemp(t)
	c := core.New(db, core.NewBus())
	b := board.New(c, func() board.Defaults { return board.Defaults{Status: "idea", Columns: []string{"Research"}} })
	d := docs.New(c, "", func() string { return "# Script\n\nFirst claim." }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s := New(c)
	actors := map[string]core.Actor{}
	for _, name := range []string{"ada", "grace", "reviewer"} {
		id, err := store.CreateUser(ctx, db, &store.User{Handle: name, Email: name + "@example.com", Name: name, Role: "owner", PasswordHash: "x"})
		if err != nil {
			t.Fatal(err)
		}
		actors[name] = core.Actor{Kind: core.KindUser, ID: id}
	}
	p, err := b.CreateProposition(ctx, actors["ada"], "Episode")
	if err != nil {
		t.Fatal(err)
	}
	created, err := s.SaveEvidence(ctx, actors["ada"], Evidence{Proposition: p.EntityID, Title: "Source", Quotation: "Exact", Verified: true})
	if err != nil || created.Action != "create" {
		t.Fatalf("create %v %v", created.Action, err)
	}
	var e Evidence
	json.Unmarshal(created.After, &e)
	for _, tc := range []struct {
		name, saver, quotation string
		verified               bool
		verifier               string
	}{
		{"unchanged save keeps the verifier", "grace", "Exact", true, "ada"},
		{"changed content with verified names the saver", "grace", "Edited", true, "grace"},
		{"unchanged save by another keeps the verifier", "ada", "Edited", true, "grace"},
		{"changed content without verified clears it", "ada", "Edited again", false, ""},
	} {
		e.Quotation, e.Verified = tc.quotation, tc.verified
		ev, err := s.SaveEvidence(ctx, actors[tc.saver], e)
		if err != nil || ev.Action != "edit" {
			t.Fatalf("%s: %v %v", tc.name, ev.Action, err)
		}
		json.Unmarshal(ev.After, &e)
		if tc.verifier == "" && (e.Verified || e.VerifiedBy != nil) || tc.verifier != "" && (e.VerifiedBy == nil || *e.VerifiedBy != actors[tc.verifier].ID) {
			t.Fatalf("%s: verified %v by %v", tc.name, e.Verified, e.VerifiedBy)
		}
	}
	doc, err := d.CreateDocument(ctx, actors["ada"], p.EntityID, "Script")
	if err != nil {
		t.Fatal(err)
	}
	requested, err := s.RequestReview(ctx, actors["ada"], doc.EntityID, 0, actors["reviewer"].ID)
	if err != nil {
		t.Fatal(err)
	}
	var review Review
	json.Unmarshal(requested.After, &review)
	if _, err = s.Decide(ctx, actors["ada"], review.ID, 1, "approved", ""); !errors.Is(err, board.ErrNotYours) {
		t.Fatalf("decided for a present reviewer: %v", err)
	}
	if _, err = db.ExecContext(ctx, `DELETE FROM users WHERE id=?`, actors["reviewer"].ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Decide(ctx, actors["ada"], review.ID, 1, "approved", ""); err != nil {
		t.Fatalf("review of a deleted reviewer stuck: %v", err)
	}
}
