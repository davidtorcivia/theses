package realtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/davidtorcivia/theses/internal/docs"
)

// The document commands go over the same socket as the board's, answer with the
// same ack, refuse a stale write with the same conflict frame, and reach the
// other tab as an event.
func TestSocketCarriesTheDocumentCommands(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)

	mine := r.mustDial("ada")
	read(t, mine, "presence")
	theirs := r.mustDial("grace")
	read(t, mine, "presence")

	send(t, mine, command{ID: 1, Cmd: "document.create", Args: args{
		Proposition: r.prop, Title: "Research"}})
	created := read(t, mine, "ack")
	if created.Event == nil || created.Event.Entity != "document" || created.Event.Action != "create" {
		t.Fatalf("the answer is %+v", created)
	}
	document := created.Event.EntityID

	send(t, mine, command{ID: 2, Cmd: "block.insert", Args: args{
		Document: document, Text: "The sea is a battery."}})
	inserted := read(t, mine, "ack")
	if inserted.Event == nil || inserted.Event.Entity != "block" || inserted.Event.Action != "insert" {
		t.Fatalf("the answer is %+v", inserted)
	}
	var block docs.Block
	if err := json.Unmarshal(inserted.Event.After, &block); err != nil {
		t.Fatal(err)
	}
	if block.Text != "The sea is a battery." || block.Document != document {
		t.Fatalf("the block is %+v", block)
	}

	// The other tab is told about both without asking.
	if e := read(t, theirs, "event"); e.Event.Entity != "document" {
		t.Fatalf("the other tab saw %+v", e.Event)
	}
	if e := read(t, theirs, "event"); e.Event.Entity != "block" {
		t.Fatalf("the other tab saw %+v", e.Event)
	}

	// Somebody else writes the block, and this tab's stale set comes back as a
	// conflict carrying what the block holds now.
	if _, err := r.hub.Docs.SetBlock(ctx, r.actor("grace"), block.ID, block.Version,
		"The sea is a flywheel.", false); err != nil {
		t.Fatal(err)
	}
	send(t, mine, command{ID: 3, Cmd: "block.set", Args: args{
		Block: block.ID, Base: block.Version, Text: "The sea is a furnace."}})
	clash := read(t, mine, "conflict")
	if clash.ID != 3 || clash.Conflict == nil {
		t.Fatalf("the answer is %+v", clash)
	}
	if clash.Conflict.Current != "The sea is a flywheel." {
		t.Fatalf("the conflict carries %q", clash.Conflict.Current)
	}
}

// A tab whose person is not a member of the proposition cannot reach its
// documents, even by asking for a block by id.
func TestSocketRefusesADocumentCommandFromANonMember(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	created, err := r.hub.Docs.CreateDocument(ctx, r.actor("ada"), r.prop, "Research")
	if err != nil {
		t.Fatal(err)
	}
	blocks, err := docs.Blocks(ctx, r.db, created.EntityID)
	if err != nil {
		t.Fatal(err)
	}

	// The stranger is not a member, so their only way in is a socket on the
	// empty workspace; the command names the block by id.
	ws, err := r.dialProposition("stranger", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	send(t, ws, command{ID: 1, Cmd: "block.set", Args: args{
		Block: blocks[0].ID, Base: blocks[0].Version, Text: "Mine now."}})
	if m := read(t, ws, "error"); m.Error != "you cannot do that here" {
		t.Fatalf("a non-member got %q", m.Error)
	}
	after, err := docs.GetBlock(ctx, r.db, blocks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Text == "Mine now." {
		t.Fatal("a non-member wrote a block")
	}
}

// Two blocks made with no connection are two frames in an outbox, and the
// second names the first by the key the first went up under, because the tab
// drew that block before the server had given it an id.
func TestSocketInsertsAfterAKey(t *testing.T) {
	r := newRig(t)
	ws := r.mustDial("ada")
	read(t, ws, "presence")

	send(t, ws, command{ID: 1, Cmd: "document.create", Args: args{
		Proposition: r.prop, Title: "Research"}})
	document := read(t, ws, "ack").Event.EntityID

	send(t, ws, command{ID: 2, Cmd: "block.insert", Key: "first",
		Args: args{Document: document, Text: "One.", Whole: true}})
	read(t, ws, "ack")
	send(t, ws, command{ID: 3, Cmd: "block.insert", Key: "second",
		Args: args{Document: document, AfterKey: "first", Text: "Two.", Whole: true}})
	read(t, ws, "ack")

	blocks, err := docs.Blocks(context.Background(), r.db, document)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, b := range blocks {
		texts = append(texts, b.Text)
	}
	// Both went in at the head, which is what an absent after means, so the
	// template block the document started from is under them. What this is
	// about is that the second landed under the first rather than above it.
	if got := strings.Join(texts, "|"); got != "One.|Two.|# Research" {
		t.Fatalf("the document reads %q", got)
	}

	// A key this person never spent names no block, and the insert is refused
	// rather than landing at the head of the document.
	send(t, ws, command{ID: 4, Cmd: "block.insert", Key: "third",
		Args: args{Document: document, AfterKey: "nobodys", Text: "Three.", Whole: true}})
	if refused := read(t, ws, "error"); refused.Error != "that is no longer there" {
		t.Fatalf("the answer is %+v", refused)
	}
}
