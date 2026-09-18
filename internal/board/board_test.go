package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/store"
)

type fixture struct {
	*Service
	db   *store.DB
	who  map[string]core.Actor
	prop int64
	cols []Column
}

// setup is one owner, one editor, one researcher, one guest and one outsider,
// with a proposition the first four are members of.
func setup(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	db := store.OpenTemp(t)
	svc := New(core.New(db, core.NewBus()), func() Defaults {
		return Defaults{Status: "idea", Statuses: []string{"idea", "recording"},
			Columns: []string{"Research", "Outline", "Script"}}
	})

	f := &fixture{Service: svc, db: db, who: map[string]core.Actor{}}
	for _, u := range []struct{ handle, role string }{
		{"owner", auth.RoleOwner},
		{"editor", auth.RoleEditor},
		{"researcher", auth.RoleResearcher},
		{"guest", auth.RoleGuest},
		{"outsider", auth.RoleEditor},
	} {
		id, err := store.CreateUser(ctx, db, &store.User{
			Handle: u.handle, Email: u.handle + "@example.com", Name: u.handle,
			Initials: "XX", Colour: "#1100ff", Role: u.role, PasswordHash: "x",
		})
		if err != nil {
			t.Fatal(err)
		}
		f.who[u.handle] = core.Actor{Kind: core.KindUser, ID: id, Name: u.handle}
	}

	e, err := svc.CreateProposition(ctx, f.who["owner"], "Tidal Power")
	if err != nil {
		t.Fatal(err)
	}
	f.prop = e.EntityID
	for _, handle := range []string{"editor", "researcher", "guest"} {
		if _, err := svc.AddMember(ctx, f.who["owner"], f.prop, f.who[handle].ID); err != nil {
			t.Fatal(err)
		}
	}
	if f.cols, err = ListColumns(ctx, db, f.prop); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) mustCard(t *testing.T, column int64, title string) Card {
	t.Helper()
	e, err := f.CreateCard(context.Background(), f.who["editor"], column, title, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := GetCard(context.Background(), f.db, e.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCreatePropositionSeedsColumnsAndRecordsIt(t *testing.T) {
	ctx := context.Background()
	f := setup(t)

	if len(f.cols) != 3 || f.cols[0].Name != "Research" || f.cols[2].Name != "Script" {
		t.Fatalf("columns are %+v", f.cols)
	}
	p, err := GetProposition(ctx, f.db, f.prop)
	if err != nil {
		t.Fatal(err)
	}
	if p.Number != 1 || p.Status != "idea" {
		t.Errorf("proposition is %+v", p)
	}
	if len(p.Members) != 4 {
		t.Errorf("members are %v, want the owner and the three added", p.Members)
	}

	var entity, action string
	var after string
	if err := f.db.QueryRowContext(ctx, `SELECT entity, action, after_json FROM activity
		WHERE proposition_id = ? AND entity = 'proposition' ORDER BY id LIMIT 1`, f.prop).
		Scan(&entity, &action, &after); err != nil {
		t.Fatal(err)
	}
	if action != "create" {
		t.Errorf("the activity row says %q, want create", action)
	}
	var recorded Proposition
	if err := json.Unmarshal([]byte(after), &recorded); err != nil {
		t.Fatal(err)
	}
	if recorded.Title != "Tidal Power" {
		t.Errorf("the activity row recorded %q", recorded.Title)
	}
}

func TestAuthorisationRefusals(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	card := f.mustCard(t, f.cols[0].ID, "Call the engineer")

	// A guest reads and nothing more.
	if _, err := f.SetCardDone(ctx, f.who["guest"], card.ID, true); !errors.Is(err, core.ErrForbidden) {
		t.Errorf("a guest marked a card done: %v", err)
	}
	// A researcher edits but does not delete.
	if _, err := f.SetCardDone(ctx, f.who["researcher"], card.ID, true); err != nil {
		t.Errorf("a researcher could not mark a card done: %v", err)
	}
	if _, err := f.DeleteCard(ctx, f.who["researcher"], card.ID); !errors.Is(err, core.ErrForbidden) {
		t.Errorf("a researcher deleted a card: %v", err)
	}
	// An editor who is not a member of this proposition gets nothing.
	if _, err := f.SetCardDone(ctx, f.who["outsider"], card.ID, false); !errors.Is(err, core.ErrForbidden) {
		t.Errorf("a non member edited the board: %v", err)
	}
	// The owner is a member of everything.
	if _, err := f.DeleteCard(ctx, f.who["owner"], card.ID); err != nil {
		t.Errorf("the owner could not delete a card: %v", err)
	}

	// A refusal leaves no activity row behind.
	var n int
	if err := f.db.QueryRowContext(ctx,
		`SELECT count(*) FROM activity WHERE action = 'delete' AND entity = 'card'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d card deletions recorded, want 1", n)
	}
}

func TestStaleTitleEditIsRefusedWithTheCurrentValue(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	card := f.mustCard(t, f.cols[0].ID, "Draft the opening")

	if _, err := f.EditCardTitle(ctx, f.who["editor"], card.ID, card.Version, "Draft the open line"); err != nil {
		t.Fatal(err)
	}
	_, err := f.EditCardDescription(ctx, f.who["owner"], card.ID, card.Version, "from the tide tables")
	var conflict *core.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("a stale edit went through: %v", err)
	}
	if conflict.Field != "description_md" || conflict.Version != card.Version+1 {
		t.Errorf("conflict is %+v", conflict)
	}
	if conflict.Current != "" {
		t.Errorf("conflict carries %q as the current description, want the empty one", conflict.Current)
	}
	// Taking theirs means editing again from the version the refusal named.
	if _, err := f.EditCardDescription(ctx, f.who["owner"], card.ID, conflict.Version, "from the tide tables"); err != nil {
		t.Fatalf("the retry at the current version failed: %v", err)
	}
}

func TestUndoPutsTheTitleBackAndOnlyOnce(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	card := f.mustCard(t, f.cols[0].ID, "Call the engineer")

	edit, err := f.EditCardTitle(ctx, f.who["editor"], card.ID, card.Version, "Call the surveyor")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Undo(ctx, f.who["editor"], edit.Seq); err != nil {
		t.Fatal(err)
	}
	back, err := GetCard(ctx, f.db, card.ID)
	if err != nil {
		t.Fatal(err)
	}
	if back.Title != "Call the engineer" {
		t.Errorf("undo left the title as %q", back.Title)
	}
	if back.Version <= card.Version+1 {
		t.Errorf("undo left the version at %d, which would let a stale edit through", back.Version)
	}
	if _, err := f.Undo(ctx, f.who["editor"], edit.Seq); !errors.Is(err, core.ErrNotUndoable) {
		t.Errorf("the same change was undone twice: %v", err)
	}

	// Creating a card is not undoable, because putting the row back would give
	// it a new id and orphan everything that pointed at it.
	var create int64
	if err := f.db.QueryRowContext(ctx,
		`SELECT id FROM activity WHERE entity = 'card' AND action = 'create'`).Scan(&create); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Undo(ctx, f.who["editor"], create); !errors.Is(err, core.ErrNotUndoable) {
		t.Errorf("a card creation was undone: %v", err)
	}

	// Nor is deleting one: the row is gone, and putting it back would give it a
	// new id. Only a delete that leaves the row behind as a tombstone, which is
	// what a document block does, can be undone.
	deleted, err := f.DeleteCard(ctx, f.who["editor"], card.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Undo(ctx, f.who["editor"], deleted.Seq); !errors.Is(err, core.ErrNotUndoable) {
		t.Errorf("a card deletion was undone: %v", err)
	}
}

// Moving a card anywhere in the board leaves every column in a strict order,
// which is the whole promise of a fractional index.
func TestReorderKeepsStrictOrder(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	var cards []Card
	for _, title := range []string{"one", "two", "three", "four"} {
		cards = append(cards, f.mustCard(t, f.cols[0].ID, title))
	}

	order := func(t *testing.T) map[int64][]string {
		t.Helper()
		b, err := Load(ctx, f.db, f.prop)
		if err != nil {
			t.Fatal(err)
		}
		out := map[int64][]string{}
		seen := map[int64]string{}
		for _, c := range b.Cards {
			if prev, ok := seen[c.ColumnID]; ok && prev >= c.Position {
				t.Fatalf("column %d has %q at or above %q", c.ColumnID, prev, c.Position)
			}
			seen[c.ColumnID] = c.Position
			out[c.ColumnID] = append(out[c.ColumnID], c.Title)
		}
		return out
	}

	if got := order(t)[f.cols[0].ID]; len(got) != 4 || got[0] != "one" || got[3] != "four" {
		t.Fatalf("fresh cards are %v", got)
	}
	// four to the head, one after two, three into the next column at its head.
	if _, err := f.MoveCard(ctx, f.who["editor"], cards[3].ID, f.cols[0].ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.MoveCard(ctx, f.who["editor"], cards[0].ID, f.cols[0].ID, cards[1].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.MoveCard(ctx, f.who["editor"], cards[2].ID, f.cols[1].ID, 0); err != nil {
		t.Fatal(err)
	}

	got := order(t)
	if want := []string{"four", "two", "one"}; !equal(got[f.cols[0].ID], want) {
		t.Errorf("first column is %v, want %v", got[f.cols[0].ID], want)
	}
	if want := []string{"three"}; !equal(got[f.cols[1].ID], want) {
		t.Errorf("second column is %v, want %v", got[f.cols[1].ID], want)
	}
}

func TestColumnsRenameReorderAndRefuseToTakeCardsWithThem(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	f.mustCard(t, f.cols[0].ID, "Call the engineer")

	if _, err := f.DeleteColumn(ctx, f.who["owner"], f.cols[0].ID); !errors.Is(err, ErrColumnNotEmpty) {
		t.Errorf("a column with a card in it was deleted: %v", err)
	}
	if _, err := f.DeleteColumn(ctx, f.who["owner"], f.cols[2].ID); err != nil {
		t.Errorf("an empty column would not go: %v", err)
	}
	if _, err := f.RenameColumn(ctx, f.who["editor"], f.cols[1].ID, "Structure"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.MoveColumn(ctx, f.who["editor"], f.cols[1].ID, 0); err != nil {
		t.Fatal(err)
	}
	cols, err := ListColumns(ctx, f.db, f.prop)
	if err != nil {
		t.Fatal(err)
	}
	if len(cols) != 2 || cols[0].Name != "Structure" || cols[1].Name != "Research" {
		t.Errorf("columns are %+v", cols)
	}
}

func TestChecklistAssigneesAndNotes(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	card := f.mustCard(t, f.cols[0].ID, "Call the engineer")
	editor, researcher := f.who["editor"], f.who["researcher"]

	item, err := f.AddChecklistItem(ctx, editor, card.ID, "find her number")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.ToggleChecklistItem(ctx, editor, item.EntityID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.AssignCard(ctx, editor, card.ID, researcher.ID); err != nil {
		t.Fatal(err)
	}
	note, err := f.PostComment(ctx, researcher, card.ID, "she is away until Friday")
	if err != nil {
		t.Fatal(err)
	}

	got, err := GetCard(ctx, f.db, card.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Checklist) != 1 || !got.Checklist[0].Done {
		t.Errorf("checklist is %+v", got.Checklist)
	}
	if len(got.Assignees) != 1 || got.Assignees[0] != researcher.ID {
		t.Errorf("assignees are %v", got.Assignees)
	}
	if len(got.Comments) != 1 {
		t.Fatalf("notes are %+v", got.Comments)
	}

	if _, err := f.DeleteComment(ctx, editor, note.EntityID); !errors.Is(err, ErrNotYours) {
		t.Errorf("somebody else's note was deleted: %v", err)
	}
	if _, err := f.DeleteComment(ctx, researcher, note.EntityID); err != nil {
		t.Errorf("an author could not delete their own note: %v", err)
	}
}

// Deleting a proposition files its activity row with no proposition, because a
// row pointing at the proposition would cascade away with it.
func TestDeletingAPropositionKeepsTheRecordOfIt(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	if _, err := f.DeleteProposition(ctx, f.who["owner"], f.prop); err != nil {
		t.Fatal(err)
	}
	var proposition sql.NullInt64
	var before string
	if err := f.db.QueryRowContext(ctx, `SELECT proposition_id, before_json FROM activity
		WHERE entity = 'proposition' AND action = 'delete'`).Scan(&proposition, &before); err != nil {
		t.Fatal(err)
	}
	if proposition.Valid {
		t.Errorf("the deletion row still points at proposition %d", proposition.Int64)
	}
	var was Proposition
	if err := json.Unmarshal([]byte(before), &was); err != nil {
		t.Fatal(err)
	}
	if was.ID != f.prop {
		t.Errorf("the deletion row records proposition %d, want %d", was.ID, f.prop)
	}
}

func TestArchiveAndRestoreAndVia(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	through := f.who["owner"]
	through.Via = "mcp:research-agent"

	if _, err := f.ArchiveProposition(ctx, through, f.prop); err != nil {
		t.Fatal(err)
	}
	p, err := GetProposition(ctx, f.db, f.prop)
	if err != nil {
		t.Fatal(err)
	}
	if p.ArchivedAt == nil {
		t.Fatal("archive left archived_at null")
	}
	if _, err := f.RestoreProposition(ctx, through, f.prop); err != nil {
		t.Fatal(err)
	}
	if p, err = GetProposition(ctx, f.db, f.prop); err != nil {
		t.Fatal(err)
	}
	if p.ArchivedAt != nil {
		t.Errorf("restore left archived_at at %v", *p.ArchivedAt)
	}

	events, err := f.Since(ctx, f.prop, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1]
	if last.Action != "restore" || last.Actor.ID != through.ID || last.Actor.Via != "mcp:research-agent" {
		t.Errorf("the stream ends with %+v", last)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// An archived proposition is read only. Restoring it and deleting it are the
// two things still allowed, so the rail's archived list cannot be edited by
// accident from a tab that still has it open.
func TestArchivedPropositionTakesOnlyRestoreAndDelete(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	card := f.mustCard(t, f.cols[0].ID, "Call the engineer")
	owner := f.who["owner"]

	edit, err := f.EditCardTitle(ctx, owner, card.ID, card.Version, "Call the surveyor")
	if err != nil {
		t.Fatal(err)
	}
	added, err := f.AddChecklistItem(ctx, owner, card.ID, "find her number")
	if err != nil {
		t.Fatal(err)
	}
	item := added.EntityID
	if _, err := f.ArchiveProposition(ctx, owner, f.prop); err != nil {
		t.Fatal(err)
	}
	for name, run := range map[string]func() error{
		"edit the proposition": func() error {
			_, err := f.EditProposition(ctx, owner, f.prop, "Renamed", "", "")
			return err
		},
		"set the status":  func() error { _, err := f.SetStatus(ctx, owner, f.prop, "recording"); return err },
		"archive again":   func() error { _, err := f.ArchiveProposition(ctx, owner, f.prop); return err },
		"move a card":     func() error { _, err := f.MoveCard(ctx, owner, card.ID, f.cols[1].ID, 0); return err },
		"tick a card":     func() error { _, err := f.SetCardDone(ctx, owner, card.ID, true); return err },
		"add a card":      func() error { _, err := f.CreateCard(ctx, owner, f.cols[0].ID, "New", nil); return err },
		"rename a column": func() error { _, err := f.RenameColumn(ctx, owner, f.cols[0].ID, "Other"); return err },
		"post a note":     func() error { _, err := f.PostComment(ctx, owner, card.ID, "hello"); return err },
		"add a member":    func() error { _, err := f.AddMember(ctx, owner, f.prop, f.who["outsider"].ID); return err },
		// These three name their action "delete", which is what the guard used
		// to exempt, so each of them wrote to an archived proposition.
		"delete a card":           func() error { _, err := f.DeleteCard(ctx, owner, card.ID); return err },
		"delete a column":         func() error { _, err := f.DeleteColumn(ctx, owner, f.cols[2].ID); return err },
		"remove a checklist item": func() error { _, err := f.RemoveChecklistItem(ctx, owner, item); return err },
		// An undo is a write like any other, and it does not go through the
		// command shape, so it carries the rule itself.
		"undo an edit": func() error { _, err := f.Undo(ctx, owner, edit.Seq); return err },
	} {
		if err := run(); !errors.Is(err, ErrArchived) {
			t.Errorf("%s on an archived proposition gave %v, want ErrArchived", name, err)
		}
	}

	// Nothing got through: the card, the column and the checklist item are all
	// still there.
	if _, err := GetCard(ctx, f.db, card.ID); err != nil {
		t.Errorf("the card went anyway: %v", err)
	}
	cols, err := ListColumns(ctx, f.db, f.prop)
	if err != nil {
		t.Fatal(err)
	}
	if len(cols) != 3 {
		t.Errorf("%d columns left, want 3", len(cols))
	}

	if _, err := f.RestoreProposition(ctx, owner, f.prop); err != nil {
		t.Fatalf("restore was refused: %v", err)
	}
	if _, err := f.SetCardDone(ctx, owner, card.ID, true); err != nil {
		t.Errorf("the board is still read only after a restore: %v", err)
	}
	if _, err := f.ArchiveProposition(ctx, owner, f.prop); err != nil {
		t.Fatal(err)
	}
	if _, err := f.DeleteProposition(ctx, owner, f.prop); err != nil {
		t.Errorf("deleting an archived proposition was refused: %v", err)
	}
}

// A tab replaces the card it holds with the payload of an event, so an undo
// has to carry the whole card and not only the columns it wrote. Before this,
// undoing a title edit took the card's assignees, checklist and notes with it.
func TestUndoPublishesTheWholeCard(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	card := f.mustCard(t, f.cols[0].ID, "Call the engineer")
	editor, researcher := f.who["editor"], f.who["researcher"]

	if _, err := f.AssignCard(ctx, editor, card.ID, researcher.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.AddChecklistItem(ctx, editor, card.ID, "find her number"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.PostComment(ctx, editor, card.ID, "she is away until Friday"); err != nil {
		t.Fatal(err)
	}
	now, err := GetCard(ctx, f.db, card.ID)
	if err != nil {
		t.Fatal(err)
	}

	edit, err := f.EditCardTitle(ctx, editor, card.ID, now.Version, "Call the surveyor")
	if err != nil {
		t.Fatal(err)
	}
	undone, err := f.Undo(ctx, editor, edit.Seq)
	if err != nil {
		t.Fatal(err)
	}

	var published Card
	if err := json.Unmarshal(undone.After, &published); err != nil {
		t.Fatal(err)
	}
	if published.Title != "Call the engineer" {
		t.Errorf("the undo published the title %q", published.Title)
	}
	if len(published.Assignees) != 1 || len(published.Checklist) != 1 || len(published.Comments) != 1 {
		t.Errorf("the undo published a card with %d assignees, %d checklist items and %d notes",
			len(published.Assignees), len(published.Checklist), len(published.Comments))
	}
	if published.ColumnID != card.ColumnID || published.Version <= now.Version {
		t.Errorf("the undo published %+v", published)
	}

	// A proposition comes back whole too, members and all.
	e, err := f.SetStatus(ctx, f.who["owner"], f.prop, "recording")
	if err != nil {
		t.Fatal(err)
	}
	back, err := f.Undo(ctx, f.who["owner"], e.Seq)
	if err != nil {
		t.Fatal(err)
	}
	var p Proposition
	if err := json.Unmarshal(back.After, &p); err != nil {
		t.Fatal(err)
	}
	if p.Status != "idea" || p.Title == "" || len(p.Members) != 4 {
		t.Errorf("the undo published %+v", p)
	}
}

// Undo puts a row back to what it was before a change, which is only safe
// while the row still holds what that change left. A newer edit, or a newer
// move, turns the undo into a conflict rather than a silent loss.
func TestUndoRefusesARowThatHasMovedOn(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	card := f.mustCard(t, f.cols[0].ID, "Call the engineer")
	editor, owner := f.who["editor"], f.who["owner"]

	first, err := f.EditCardTitle(ctx, editor, card.ID, card.Version, "Call the surveyor")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.EditCardTitle(ctx, owner, card.ID, card.Version+1, "Call the hydrologist"); err != nil {
		t.Fatal(err)
	}

	_, err = f.Undo(ctx, editor, first.Seq)
	var conflict *core.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("undoing over a newer edit gave %v, want a conflict", err)
	}
	if conflict.Field != "title" || conflict.Current != "Call the hydrologist" {
		t.Errorf("the conflict is %+v", conflict)
	}
	now, err := GetCard(ctx, f.db, card.ID)
	if err != nil {
		t.Fatal(err)
	}
	if now.Title != "Call the hydrologist" {
		t.Errorf("the refused undo wrote anyway: %q", now.Title)
	}
	var undone sql.NullInt64
	if err := f.db.QueryRowContext(ctx,
		`SELECT undone_at FROM activity WHERE id = ?`, first.Seq).Scan(&undone); err != nil {
		t.Fatal(err)
	}
	if undone.Valid {
		t.Error("the refused undo marked the activity row undone")
	}

	// A move that somebody else has moved past is the same refusal, which is
	// what keeps two cards off one ordering key.
	other := f.mustCard(t, f.cols[0].ID, "Draft the opening")
	move, err := f.MoveCard(ctx, editor, other.ID, f.cols[1].ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.MoveCard(ctx, owner, other.ID, f.cols[2].ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Undo(ctx, editor, move.Seq); !errors.As(err, &conflict) {
		t.Errorf("undoing a move that was moved past gave %v, want a conflict", err)
	}

	// A change undo cannot put back at all is refused rather than marked done.
	assign, err := f.AssignCard(ctx, editor, card.ID, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Undo(ctx, editor, assign.Seq); !errors.Is(err, core.ErrNotUndoable) {
		t.Errorf("undoing an assignment gave %v, want ErrNotUndoable", err)
	}
}

// The sequence number a board is handed with has to stand for the rows that
// came with it, not for a later moment, or a change that lands between the two
// reads is never replayed by the catch up.
func TestLoadReadsTheSequenceNumberBeforeTheRows(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	first := f.mustCard(t, f.cols[0].ID, "Call the engineer")

	before, err := Load(ctx, f.db, f.prop)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.CreateCard(ctx, f.who["editor"], f.cols[0].ID, "Draft the opening", nil)
	if err != nil {
		t.Fatal(err)
	}
	after, err := Load(ctx, f.db, f.prop)
	if err != nil {
		t.Fatal(err)
	}

	// Whatever a board holds, the stream from its sequence number carries
	// everything the board does not already have.
	for _, b := range []Board{before, after} {
		events, err := f.Since(ctx, f.prop, b.Seq, 100)
		if err != nil {
			t.Fatal(err)
		}
		have := map[int64]bool{}
		for _, c := range b.Cards {
			have[c.ID] = true
		}
		for _, e := range events {
			if e.Entity == "card" {
				have[e.EntityID] = true
			}
		}
		if !have[first.ID] || !have[second.EntityID] {
			t.Errorf("a board at seq %d plus its stream is missing a card: %v", b.Seq, have)
		}
	}
}

// A field on the board holds what the field is for. Past that the command
// refuses rather than storing something no column was meant to carry and every
// board that draws it has to render.
func TestCommandsRefuseAFieldLongerThanTheFieldTakes(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	card := f.mustCard(t, f.cols[0].ID, "Call the engineer")
	owner := f.who["owner"]
	long := strings.Repeat("x", MaxLine+1)
	huge := strings.Repeat("x", MaxBody+1)

	for name, run := range map[string]func() error{
		"a proposition title": func() error { _, err := f.CreateProposition(ctx, owner, long); return err },
		"a statement":         func() error { _, err := f.EditProposition(ctx, owner, f.prop, "ok", long, ""); return err },
		"a blurb":             func() error { _, err := f.EditProposition(ctx, owner, f.prop, "ok", "", huge); return err },
		"a status":            func() error { _, err := f.SetStatus(ctx, owner, f.prop, long); return err },
		"an episode":          func() error { _, err := f.Schedule(ctx, owner, f.prop, long, ""); return err },
		"a column name":       func() error { _, err := f.CreateColumn(ctx, owner, f.prop, long); return err },
		"a card title":        func() error { _, err := f.CreateCard(ctx, owner, f.cols[0].ID, long, nil); return err },
		"a card description": func() error {
			_, err := f.EditCardDescription(ctx, owner, card.ID, card.Version, huge)
			return err
		},
		"a due date":       func() error { _, err := f.SetCardDue(ctx, owner, card.ID, long); return err },
		"a checklist item": func() error { _, err := f.AddChecklistItem(ctx, owner, card.ID, long); return err },
		"a note":           func() error { _, err := f.PostComment(ctx, owner, card.ID, huge); return err },
		"a list of assignees": func() error {
			_, err := f.CreateCard(ctx, owner, f.cols[0].ID, "ok", make([]int64, maxAssignees+1))
			return err
		},
	} {
		if err := run(); !errors.Is(err, ErrTooLong) {
			t.Errorf("%s over the limit gave %v, want ErrTooLong", name, err)
		}
	}

	// The limit is in runes, not bytes, so a field of accented text holds as
	// much of it as a field of plain text does.
	if _, err := f.CreateProposition(ctx, owner, strings.Repeat("é", MaxLine)); err != nil {
		t.Errorf("a title of %d accented characters was refused: %v", MaxLine, err)
	}
}

// An ordering key is unique within its column. A card that leaves one frees
// the key it held, and undoing that move has to notice when something else has
// since been given it, or two cards sit on one key and their order is whatever
// the database returns first.
func TestUndoRefusesWhenTheOrderingKeyHasBeenTaken(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	editor := f.who["editor"]
	moving := f.mustCard(t, f.cols[0].ID, "Call the engineer")

	// The first card in any column takes the same first key, so moving this
	// one out of its column and making another frees a key and gives it away.
	move, err := f.MoveCard(ctx, editor, moving.ID, f.cols[1].ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	made, err := f.CreateCard(ctx, editor, f.cols[0].ID, "Draft the opening", nil)
	if err != nil {
		t.Fatal(err)
	}
	taken, err := GetCard(ctx, f.db, made.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	if taken.Position != moving.Position {
		t.Fatalf("the new card took %q, not the freed %q", taken.Position, moving.Position)
	}

	_, err = f.Undo(ctx, editor, move.Seq)
	var conflict *core.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("undoing onto a taken key gave %v, want a conflict", err)
	}
	if conflict.Field != "position" {
		t.Errorf("the conflict is %+v", conflict)
	}

	// Nothing moved, and the two cards are still one to a key.
	b, err := Load(ctx, f.db, f.prop)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, c := range b.Cards {
		key := fmt.Sprintf("%d/%s", c.ColumnID, c.Position)
		if other, ok := seen[key]; ok {
			t.Errorf("%q and %q are both at %s", other, c.Title, key)
		}
		seen[key] = c.Title
	}
	if len(b.Cards) != 2 {
		t.Fatalf("cards are %+v", b.Cards)
	}

	// Once the key is free again the same undo goes through.
	if _, err := f.MoveCard(ctx, editor, taken.ID, f.cols[2].ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Undo(ctx, editor, move.Seq); err != nil {
		t.Errorf("the undo was still refused with the key free: %v", err)
	}
	back, err := GetCard(ctx, f.db, moving.ID)
	if err != nil {
		t.Fatal(err)
	}
	if back.ColumnID != f.cols[0].ID || back.Position != moving.Position {
		t.Errorf("the card came back to column %d at %q", back.ColumnID, back.Position)
	}
}

// The rail groups by status, so a proposition cannot be moved to a word the
// workspace does not have: the rail would have nowhere to draw it.
func TestSetStatusTakesOnlyTheWorkspacesStatuses(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	owner := f.who["owner"]

	for _, tc := range []struct {
		name, status string
		want         error
	}{
		{"one the workspace has", "recording", nil},
		{"the one it starts on", "idea", nil},
		{"a word it does not have", "shipped", ErrStatus},
		{"the same word in another case", "Recording", ErrStatus},
		{"nothing at all", "   ", ErrEmpty},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.SetStatus(ctx, owner, f.prop, tc.status)
			if !errors.Is(err, tc.want) {
				t.Fatalf("%q gave %v, want %v", tc.status, err, tc.want)
			}
		})
	}

	// A workspace that names no statuses has nothing to check against, so any
	// word is taken rather than every word refused.
	was := f.Defaults
	f.Defaults = func() Defaults { return Defaults{Status: "idea", Columns: []string{"Research"}} }
	defer func() { f.Defaults = was }()
	if _, err := f.SetStatus(ctx, owner, f.prop, "shipped"); err != nil {
		t.Fatalf("a workspace with no statuses refused one: %v", err)
	}
}

// Seed fills a proposition the moment it is made, in the transaction that made
// it: what it writes is there with the first render, and a seed that refuses
// takes the proposition down with it rather than leaving a half made one.
func TestCreatePropositionRunsTheSeedInTheSameTransaction(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		who  string
		seed func(*fixture) func(context.Context, core.Actor, int64) error
		kept bool
	}{
		{name: "no seed", seed: func(*fixture) func(context.Context, core.Actor, int64) error { return nil }, kept: true},
		{
			name: "a seed that writes",
			seed: func(f *fixture) func(context.Context, core.Actor, int64) error {
				return func(ctx context.Context, a core.Actor, id int64) error {
					_, err := f.CreateColumn(ctx, a, id, "Seeded")
					return err
				}
			},
			kept: true,
		},
		{
			// The creator's membership is written in the same transaction, so
			// a seed command authorized against the new proposition finds it.
			// An owner would pass that test whatever happened, so this row is
			// an editor, who would not.
			name: "an editor's seed that writes",
			who:  "editor",
			seed: func(f *fixture) func(context.Context, core.Actor, int64) error {
				return func(ctx context.Context, a core.Actor, id int64) error {
					_, err := f.CreateColumn(ctx, a, id, "Seeded")
					return err
				}
			},
			kept: true,
		},
		{
			name: "a seed that refuses",
			seed: func(*fixture) func(context.Context, core.Actor, int64) error {
				return func(context.Context, core.Actor, int64) error { return errors.New("no") }
			},
			kept: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t)
			f.Seed = tc.seed(f)
			who := tc.who
			if who == "" {
				who = "owner"
			}
			e, err := f.CreateProposition(ctx, f.who[who], "Wind")
			if (err == nil) != tc.kept {
				t.Fatalf("create gave %v", err)
			}
			var made int
			if err := f.db.QueryRowContext(ctx,
				`SELECT count(*) FROM propositions WHERE title = 'Wind'`).Scan(&made); err != nil {
				t.Fatal(err)
			}
			if (made == 1) != tc.kept {
				t.Fatalf("%d propositions named Wind", made)
			}
			if !tc.kept {
				return
			}
			cols, err := ListColumns(ctx, f.db, e.EntityID)
			if err != nil {
				t.Fatal(err)
			}
			if seeded := len(cols) == 4; seeded != strings.Contains(tc.name, "seed that writes") {
				t.Fatalf("the columns are %+v", cols)
			}
		})
	}
}

// A due date is the one field the board asks a question of: has that day gone.
// Anything the calendar does not have could never answer it, so it is refused
// at the command rather than stored and left saying nothing for ever.
func TestSetCardDueTakesOnlyACalendarDay(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	e, err := f.CreateCard(ctx, f.who["owner"], f.cols[0].ID, "Read the tide tables", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		due   string
		kept  string
		wrong bool
	}{
		{due: "2026-09-24", kept: "2026-09-24"},
		{due: "  2026-09-24  ", kept: "2026-09-24"},
		{due: "", kept: ""},
		{due: "24 Sep", wrong: true},
		{due: "2026-02-31", wrong: true},
		{due: "2026-9-4", wrong: true},
		{due: "soon", wrong: true},
	} {
		t.Run(tc.due, func(t *testing.T) {
			_, err := f.SetCardDue(ctx, f.who["owner"], e.EntityID, tc.due)
			if tc.wrong {
				if !errors.Is(err, ErrDueDate) {
					t.Fatalf("%q gave %v", tc.due, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("%q gave %v", tc.due, err)
			}
			card, err := GetCard(ctx, f.db, e.EntityID)
			if err != nil {
				t.Fatal(err)
			}
			got := ""
			if card.DueDate != nil {
				got = *card.DueDate
			}
			if got != tc.kept {
				t.Fatalf("%q was kept as %q", tc.due, got)
			}
		})
	}
}
