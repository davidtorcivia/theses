package mcp

import (
	"context"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/docs"
	"github.com/davidtorcivia/theses/internal/store"
)

// say is the text a refused tool call carries back to the client.
func say(res *sdk.CallToolResult) string {
	var out []string
	for _, c := range res.Content {
		if text, ok := c.(*sdk.TextContent); ok {
			out = append(out, text.Text)
		}
	}
	return strings.Join(out, " ")
}

func (h *harness) owner() core.Actor {
	return core.Actor{Kind: core.KindUser, ID: h.user.ID, Name: h.user.Name}
}

func (h *harness) proposition() int64 {
	h.Helper()
	e, err := h.board.CreateProposition(context.Background(), h.owner(), "Tidal Power")
	if err != nil {
		h.Fatal(err)
	}
	return e.EntityID
}

// An agent with a write token creates a document, fills it and replaces a
// block, and everything it did is recorded as the person whose token it used
// with the client's name in the via column.
func TestDocumentToolsWriteAndAreAttributed(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	cs := h.connect(auth.ScopeRead, auth.ScopeWrite)
	prop := h.proposition()

	var made writeOut
	h.call(cs, "create_document", createDocumentArgs{Proposition: prop, Name: "Research"}, &made)
	if made.ID == 0 {
		t.Fatal("no document id came back")
	}

	var appended writeOut
	h.call(cs, "append_block", appendBlockArgs{Document: made.ID, Text: "The sea is a battery."}, &appended)
	if appended.ID == 0 || appended.Version == 0 {
		t.Fatalf("append_block returned %+v", appended)
	}

	var list listDocumentsOut
	h.call(cs, "list_documents", listDocumentsArgs{Proposition: prop}, &list)
	if len(list.Documents) != 1 || list.Documents[0].Name != "Research" || list.Documents[0].Blocks != 2 {
		t.Fatalf("list_documents returned %+v", list.Documents)
	}

	var read readDocumentOut
	h.call(cs, "read_document", readDocumentArgs{Document: made.ID}, &read)
	if !strings.Contains(read.Markdown, "The sea is a battery.") {
		t.Fatalf("read_document returned %q", read.Markdown)
	}
	if len(read.Blocks) != 2 || read.Blocks[1].ID != appended.ID {
		t.Fatalf("read_document returned %+v", read.Blocks)
	}

	// Replacing with the version read_document gave goes through.
	var replaced writeOut
	h.call(cs, "replace_block", replaceBlockArgs{
		Block: appended.ID, Text: "The sea is a flywheel.", BaseVersion: read.Blocks[1].Version}, &replaced)
	if replaced.Version <= read.Blocks[1].Version {
		t.Fatalf("replace_block returned %+v", replaced)
	}
	// And with no version at all it overwrites rather than refusing.
	h.call(cs, "replace_block", replaceBlockArgs{Block: appended.ID, Text: "The sea is a furnace."}, &replaced)
	block, err := docs.GetBlock(ctx, h.db, appended.ID)
	if err != nil {
		t.Fatal(err)
	}
	if block.Text != "The sea is a furnace." {
		t.Fatalf("the block holds %q", block.Text)
	}

	var kind, actor, via string
	if err := h.db.QueryRowContext(ctx,
		`SELECT actor_kind, actor_id, coalesce(via, '') FROM activity
		 WHERE entity = 'block' ORDER BY id DESC LIMIT 1`).Scan(&kind, &actor, &via); err != nil {
		t.Fatal(err)
	}
	if kind != "user" || via != "mcp:"+clientName {
		t.Fatalf("the row says %q %q %q", kind, actor, via)
	}
}

// A stale replace_block comes back with the text the block holds now, which is
// what an agent needs to write its next attempt.
func TestReplaceBlockReportsWhatTheBlockHoldsNow(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	cs := h.connect(auth.ScopeRead, auth.ScopeWrite)
	prop := h.proposition()

	var made writeOut
	h.call(cs, "create_document", createDocumentArgs{Proposition: prop, Name: "Research"}, &made)
	var appended writeOut
	h.call(cs, "append_block", appendBlockArgs{Document: made.ID, Text: "The tide is high."}, &appended)

	if _, err := h.srv.api.Docs.SetBlock(ctx, h.owner(), appended.ID, appended.Version,
		"The tide is low.", false); err != nil {
		t.Fatal(err)
	}
	res := h.call(cs, "replace_block", replaceBlockArgs{
		Block: appended.ID, Text: "The tide is slack.", BaseVersion: appended.Version}, nil)
	if !res.IsError {
		t.Fatal("a stale replace went through")
	}
	said := say(res)
	if !strings.Contains(said, "The tide is low.") {
		t.Fatalf("the refusal says %q", said)
	}
}

// insert_after_heading puts each paragraph at the end of its section, so an
// agent adding three facts leaves them in the order it wrote them and does not
// spill into the next section.
func TestInsertAfterHeadingFillsTheSectionInOrder(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	cs := h.connect(auth.ScopeRead, auth.ScopeWrite)
	prop := h.proposition()

	var made writeOut
	h.call(cs, "create_document", createDocumentArgs{Proposition: prop, Name: "Research"}, &made)
	for _, heading := range []string{"## Five consequential facts", "## Street question"} {
		var out writeOut
		h.call(cs, "append_block", appendBlockArgs{Document: made.ID, Text: heading}, &out)
	}
	for _, fact := range []string{"One.", "Two.", "Three."} {
		var out writeOut
		h.call(cs, "insert_after_heading", insertAfterHeadingArgs{
			Document: made.ID, Heading: "five consequential facts", Text: fact}, &out)
		if out.ID == 0 {
			t.Fatalf("insert_after_heading returned %+v", out)
		}
	}
	blocks, err := docs.Blocks(ctx, h.db, made.ID)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, b := range blocks {
		got = append(got, b.Text)
	}
	want := []string{"# Research", "## Five consequential facts", "One.", "Two.", "Three.", "## Street question"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("the document reads %v", got)
	}

	res := h.call(cs, "insert_after_heading", insertAfterHeadingArgs{
		Document: made.ID, Heading: "a heading nobody wrote", Text: "Nowhere."}, nil)
	if !res.IsError {
		t.Fatal("a heading that is not there took a paragraph")
	}
}

// A token whose owner is a member of nothing is told the document is not there,
// however wide its scopes.
func TestDocumentToolsRefuseSomebodyWhoIsNotAMember(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	prop := h.proposition()
	document, err := h.srv.api.Docs.CreateDocument(ctx, h.owner(), prop, "Research")
	if err != nil {
		t.Fatal(err)
	}
	blocks, err := docs.Blocks(ctx, h.db, document.EntityID)
	if err != nil {
		t.Fatal(err)
	}

	// The harness's owner reads everything, so this needs somebody else.
	id, err := store.CreateUser(ctx, h.db, &store.User{
		Handle: "ada", Email: "ada@example.com", Name: "Ada Lovelace",
		Initials: "AL", Colour: "#1100ff", Role: auth.RoleEditor, PasswordHash: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	h.user, err = store.UserByID(ctx, h.db, id)
	if err != nil {
		t.Fatal(err)
	}
	cs := h.connect(auth.ScopeRead, auth.ScopeWrite)

	for _, tc := range []struct {
		name string
		args any
	}{
		{"list_documents", listDocumentsArgs{Proposition: prop}},
		{"read_document", readDocumentArgs{Document: document.EntityID}},
		{"append_block", appendBlockArgs{Document: document.EntityID, Text: "Mine."}},
		{"replace_block", replaceBlockArgs{Block: blocks[0].ID, Text: "Mine.", BaseVersion: blocks[0].Version}},
		{"create_document", createDocumentArgs{Proposition: prop, Name: "Mine"}},
		{"write_document", writeDocumentArgs{Document: document.EntityID, Text: "Mine."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if res := h.call(cs, tc.name, tc.args, nil); !res.IsError {
				t.Fatalf("%s went through for somebody who is not a member", tc.name)
			}
		})
	}
	after, err := docs.GetBlock(ctx, h.db, blocks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Text == "Mine." {
		t.Fatal("a non-member wrote a block")
	}
	if list, err := docs.ListDocuments(ctx, h.db, prop); err != nil || len(list) != 1 {
		t.Fatalf("a non-member created a document: %v %v", list, err)
	}
}

// write_document replaces a document with markdown, keeping the blocks whose
// paragraphs did not change, and reports the ones somebody else has written in
// since rather than writing over them.
func TestWriteDocumentTool(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	cs := h.connect(auth.ScopeRead, auth.ScopeWrite)
	prop := h.proposition()

	var made writeOut
	h.call(cs, "create_document", createDocumentArgs{Proposition: prop, Name: "Research"}, &made)
	h.call(cs, "append_block", appendBlockArgs{Document: made.ID, Text: "The sea is a battery."}, &writeOut{})

	var read readDocumentOut
	h.call(cs, "read_document", readDocumentArgs{Document: made.ID}, &read)
	base := make([]sourceBlock, 0, len(read.Blocks))
	for _, b := range read.Blocks {
		base = append(base, sourceBlock{ID: b.ID, Version: b.Version})
	}
	last := read.Blocks[len(read.Blocks)-1]

	// Somebody else writes in the last block while the markdown is being
	// written, and this text changes it too, in a way that cannot be merged.
	if _, err := h.srv.api.Docs.SetBlock(ctx, h.owner(), last.ID, last.Version, "The sea is a furnace.", false); err != nil {
		t.Fatal(err)
	}
	text := read.Markdown[:len(read.Markdown)-len(last.Text)] + "The sea is a flywheel.\n\nAnd a battery."

	var wrote writeDocumentOut
	h.call(cs, "write_document", writeDocumentArgs{Document: made.ID, Text: text, Base: base}, &wrote)
	if len(wrote.Conflicts) != 1 || wrote.Conflicts[0].Block != last.ID ||
		wrote.Conflicts[0].Current != "The sea is a furnace." {
		t.Fatalf("write_document reported %+v", wrote.Conflicts)
	}
	blocks, err := docs.Blocks(ctx, h.db, made.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The block in conflict still holds what they wrote, and the paragraph
	// added after it went in.
	if blocks[len(blocks)-2].Text != "The sea is a furnace." || blocks[len(blocks)-1].Text != "And a battery." {
		t.Fatalf("the document reads %+v", blocks)
	}

	// The same call again changes nothing and reports the same conflict, which
	// is what an agent retrying a call it never saw the answer to does.
	var twice writeDocumentOut
	h.call(cs, "write_document", writeDocumentArgs{Document: made.ID, Text: text, Base: base}, &twice)
	if len(twice.Conflicts) != 1 || twice.Conflicts[0].Block != last.ID {
		t.Fatalf("the second call reported %+v", twice.Conflicts)
	}
	after, err := docs.Blocks(ctx, h.db, made.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(blocks) {
		t.Fatalf("the document has %d blocks after the second call, want %d", len(after), len(blocks))
	}
	for i, b := range after {
		if b.ID != blocks[i].ID || b.Version != blocks[i].Version {
			t.Fatalf("block %d is %d v%d, want %d v%d", i, b.ID, b.Version, blocks[i].ID, blocks[i].Version)
		}
	}

	// A base naming a version the database cannot produce the text of is
	// refused outright, with a reason the caller can act on.
	res := h.call(cs, "write_document", writeDocumentArgs{Document: made.ID, Text: "Nothing.",
		Base: []sourceBlock{{ID: last.ID, Version: 999}}}, nil)
	if !res.IsError || !strings.Contains(say(res), "no longer on record") {
		t.Fatalf("an unrecoverable base answered %v: %s", res.IsError, say(res))
	}
}
