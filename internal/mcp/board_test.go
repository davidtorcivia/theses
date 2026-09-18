package mcp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davidtorcivia/theses/internal/api"
	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/blob"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/files"
	"github.com/davidtorcivia/theses/internal/safehttp"
	"github.com/davidtorcivia/theses/internal/store"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

// boardFixture is the workspace the board tools are asked about: one
// proposition with its column and a card, a links and files service over an in
// process bucket, a backup that only records that it was asked, and somebody
// who is a member of nothing.
type boardFixture struct {
	prop     int64
	column   int64
	card     int64
	backedUp *bool
	stranger *store.User
}

func (h *harness) withBoard(t *testing.T) boardFixture {
	t.Helper()
	ctx := context.Background()

	backend := s3mem.New()
	fake := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(fake.Close)
	if err := backend.CreateBucket("theses"); err != nil {
		t.Fatal(err)
	}
	bucket, err := blob.New(blob.Config{
		Provider: "s3", Endpoint: fake.URL, Region: "us-east-1",
		Bucket: "theses", AccessKey: "key", SecretKey: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := files.New(h.board.Service, func(context.Context, string) (*blob.Client, error) { return bucket, nil },
		safehttp.Client(safehttp.AllowLoopback()))
	Files(h.srv, svc)

	asked := false
	Board(h.srv, h.board, svc, func(context.Context) error {
		asked = true
		return nil
	})

	who := core.Actor{Kind: core.KindUser, ID: h.user.ID, Name: h.user.Name}
	e, err := h.board.CreateProposition(ctx, who, "Tidal Power")
	if err != nil {
		t.Fatal(err)
	}
	cols, err := board.ListColumns(ctx, h.db, e.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	card, err := h.board.CreateCard(ctx, who, cols[0].ID, "Read the tide tables", nil)
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.CreateUser(ctx, h.db, &store.User{
		Handle: "ada", Email: "ada@example.com", Name: "Ada Lovelace",
		Initials: "AL", Colour: "#1100ff", Role: auth.RoleEditor, PasswordHash: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := store.UserByID(ctx, h.db, id)
	if err != nil {
		t.Fatal(err)
	}
	return boardFixture{prop: e.EntityID, column: cols[0].ID, card: card.EntityID,
		backedUp: &asked, stranger: stranger}
}

// connectAs opens a session for somebody other than the harness owner.
func (h *harness) connectAs(user *store.User, scopes ...string) *sdk.ClientSession {
	h.Helper()
	ctx := context.Background()
	clear, err := h.auth.CreateAPIToken(ctx, user.ID, user.Handle, scopes)
	if err != nil {
		h.Fatal(err)
	}
	token, owner, err := h.auth.APIToken(ctx, clear)
	if err != nil {
		h.Fatal(err)
	}
	clientTransport, serverTransport := sdk.NewInMemoryTransports()
	ss, err := h.srv.srv.Connect(api.WithPrincipal(ctx, api.Principal{Token: token, User: owner}),
		serverTransport, nil)
	if err != nil {
		h.Fatal(err)
	}
	h.Cleanup(func() { ss.Close() })
	client := sdk.NewClient(&sdk.Implementation{Name: clientName, Version: "test"}, nil)
	cs, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		h.Fatal(err)
	}
	h.Cleanup(func() { cs.Close() })
	return cs
}

func TestBoardToolsAreListedWithTheirHints(t *testing.T) {
	h := newHarness(t)
	h.withBoard(t)
	res, err := h.connect(auth.ScopeRead).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	reading := map[string]bool{"list_propositions": true, "get_proposition": true,
		"list_cards": true, "activity": true}
	want := map[string]bool{"create_proposition": true, "set_status": true,
		"create_card": true, "move_card": true, "assign_card": true,
		"complete_card": true, "comment": true, "annotate_link": true,
		"request_upload": true, "backup_now": true}
	for k := range reading {
		want[k] = true
	}
	for _, tool := range res.Tools {
		if !want[tool.Name] {
			continue
		}
		delete(want, tool.Name)
		if !strings.HasSuffix(tool.Description, ".") || strings.Count(tool.Description, ".") != 1 {
			t.Errorf("%s: description is not one sentence: %q", tool.Name, tool.Description)
		}
		if tool.Annotations == nil || tool.Annotations.Title == "" {
			t.Errorf("%s: no annotations", tool.Name)
			continue
		}
		if tool.Annotations.ReadOnlyHint != reading[tool.Name] {
			t.Errorf("%s: read only hint is %v", tool.Name, tool.Annotations.ReadOnlyHint)
		}
		if tool.OutputSchema == nil {
			t.Errorf("%s: no output schema", tool.Name)
		}
	}
	if len(want) != 0 {
		t.Errorf("missing tools: %v", want)
	}
}

// The board over MCP is the board over the socket, and every write is recorded
// as the person whose token it is with the client named beside it.
func TestTheBoardToolsWriteAsThePersonAndTheClient(t *testing.T) {
	h := newHarness(t)
	f := h.withBoard(t)
	cs := h.connect(auth.ScopeRead, auth.ScopeWrite)

	var made writeOut
	h.call(cs, "create_proposition", map[string]any{"title": "Deep Water"}, &made)
	if made.ID == 0 {
		t.Fatal("no proposition came back")
	}
	h.call(cs, "set_status", map[string]any{"proposition": made.ID, "status": "recording"}, nil)

	var list propositionsOut
	h.call(cs, "list_propositions", map[string]any{}, &list)
	if len(list.Propositions) != 2 {
		t.Fatalf("propositions = %+v", list.Propositions)
	}
	var one propositionOut
	h.call(cs, "get_proposition", map[string]any{"proposition": made.ID}, &one)
	if one.Proposition.Status != "recording" {
		t.Fatalf("proposition = %+v", one.Proposition)
	}

	var card writeOut
	h.call(cs, "create_card", map[string]any{"column": f.column, "title": "Call the harbor"}, &card)
	h.call(cs, "assign_card", map[string]any{"card": card.ID, "user": h.user.ID}, nil)
	h.call(cs, "complete_card", map[string]any{"card": card.ID}, nil)
	h.call(cs, "comment", map[string]any{"card": card.ID, "text": "Done before lunch."}, nil)
	h.call(cs, "move_card", map[string]any{"card": card.ID, "column": f.column, "after": f.card}, nil)

	// And the other way on both of the tools that take a direction.
	h.call(cs, "complete_card", map[string]any{"card": card.ID, "reopen": true}, nil)
	h.call(cs, "assign_card", map[string]any{"card": card.ID, "user": h.user.ID, "unassign": true}, nil)
	var reopened cardsOut
	h.call(cs, "list_cards", map[string]any{"proposition": f.prop}, &reopened)
	for _, c := range reopened.Cards {
		if c.ID == card.ID && (c.DoneAt != nil || len(c.Assignees) != 0) {
			t.Fatalf("reopening left %+v", c)
		}
	}
	h.call(cs, "complete_card", map[string]any{"card": card.ID}, nil)
	h.call(cs, "assign_card", map[string]any{"card": card.ID, "user": h.user.ID}, nil)

	var cards cardsOut
	h.call(cs, "list_cards", map[string]any{"proposition": f.prop}, &cards)
	if len(cards.Cards) != 2 || cards.Seq == 0 {
		t.Fatalf("cards = %+v seq = %d", cards.Cards, cards.Seq)
	}
	for _, c := range cards.Cards {
		if c.ID != card.ID {
			continue
		}
		if c.DoneAt == nil || len(c.Assignees) != 1 || len(c.Comments) != 1 {
			t.Fatalf("card = %+v", c)
		}
	}

	// Every one of those is in the log as the token's owner, carried by the
	// client that called it.
	var rows activityOut
	h.call(cs, "activity", map[string]any{"limit": 200}, &rows)
	writes := 0
	for _, row := range rows.Activity {
		if row.Entity == "card" || row.Entity == "comment" || row.Entity == "proposition" {
			if row.Via == "" {
				continue
			}
			writes++
			if row.Via != api.ClientVia(clientName) {
				t.Errorf("%s %s was carried by %q", row.Entity, row.Action, row.Via)
			}
			if row.ActorID != strconv.FormatInt(h.user.ID, 10) {
				t.Errorf("%s %s was made by %q", row.Entity, row.Action, row.ActorID)
			}
		}
	}
	if writes == 0 {
		t.Error("the log names nothing this session did")
	}
}

// A token without the scope, and a person without the membership, are refused
// the same way on every tool.
func TestBoardToolsAreScopedAndAuthorised(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	f := h.withBoard(t)

	read := h.connect(auth.ScopeRead)
	if res := h.call(read, "create_card", map[string]any{"column": f.column, "title": "No"}, nil); !res.IsError {
		t.Error("a read token added a card")
	}
	if res := h.call(read, "backup_now", map[string]any{}, nil); !res.IsError {
		t.Error("a read token started a backup")
	}
	if *f.backedUp {
		t.Error("the backup ran anyway")
	}
	write := h.connect(auth.ScopeWrite)
	if res := h.call(write, "list_cards", map[string]any{"proposition": f.prop}, nil); !res.IsError {
		t.Error("a write only token read the board")
	}
	if res := h.call(h.connect(auth.ScopeAdmin), "backup_now", map[string]any{}, nil); res.IsError {
		t.Errorf("an admin token could not start a backup: %+v", res.Content)
	}
	if !*f.backedUp {
		t.Error("the backup was not started")
	}

	// Somebody who is a member of nothing reads an empty rail and is told the
	// proposition is not there rather than that they may not read it.
	stranger := h.connectAs(f.stranger, auth.ScopeRead, auth.ScopeWrite)
	var list propositionsOut
	h.call(stranger, "list_propositions", map[string]any{}, &list)
	if len(list.Propositions) != 0 {
		t.Errorf("a member of nothing read %d propositions", len(list.Propositions))
	}
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"get_proposition", map[string]any{"proposition": f.prop}},
		{"list_cards", map[string]any{"proposition": f.prop}},
		{"comment", map[string]any{"card": f.card, "text": "Hello"}},
		{"complete_card", map[string]any{"card": f.card}},
	} {
		res := h.call(stranger, tc.name, tc.args, nil)
		if !res.IsError {
			t.Errorf("%s answered a member of nothing", tc.name)
			continue
		}
		if said := say(res); !strings.Contains(said, "not there") {
			t.Errorf("%s said %q", tc.name, said)
		}
	}

	// A status the workspace does not have is refused, because the rail would
	// have nowhere to draw it.
	if res := h.call(h.connect(auth.ScopeRead, auth.ScopeWrite), "set_status",
		map[string]any{"proposition": f.prop, "status": "shipped"}, nil); !res.IsError ||
		!strings.Contains(say(res), "statuses") {
		t.Errorf("a status the workspace does not have said %q", say(res))
	}

	// A card cannot move to a column on another proposition.
	other, err := h.board.CreateProposition(ctx,
		core.Actor{Kind: core.KindUser, ID: h.user.ID, Name: h.user.Name}, "Deep Water")
	if err != nil {
		t.Fatal(err)
	}
	cols, err := board.ListColumns(ctx, h.db, other.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	both := h.connect(auth.ScopeRead, auth.ScopeWrite)
	if res := h.call(both, "move_card",
		map[string]any{"card": f.card, "column": cols[0].ID}, nil); !res.IsError {
		t.Error("a card moved to another proposition's column")
	}

	// An archived proposition is read only.
	if _, err := h.board.ArchiveProposition(ctx,
		core.Actor{Kind: core.KindUser, ID: h.user.ID, Name: h.user.Name}, f.prop); err != nil {
		t.Fatal(err)
	}
	res := h.call(both, "comment", map[string]any{"card": f.card, "text": "Too late"}, nil)
	if !res.IsError || !strings.Contains(say(res), "archived") {
		t.Errorf("a note on an archived proposition said %q", say(res))
	}
	if res := h.call(both, "list_cards", map[string]any{"proposition": f.prop}, nil); res.IsError {
		t.Error("an archived board could not be read")
	}
}

// annotate_link writes the agent's own words onto a link somebody saved, and
// leaves the fields it does not name alone.
func TestAnnotateLinkKeepsWhatItDoesNotName(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	f := h.withBoard(t)
	page := httptest.NewServer(httpPage())
	t.Cleanup(page.Close)

	cs := h.connect(auth.ScopeRead, auth.ScopeWrite)
	var link files.Link
	h.call(cs, "add_link", map[string]any{"proposition": f.prop, "url": page.URL}, &link)
	if link.Title == "" {
		t.Fatalf("link = %+v", link)
	}

	var back files.Link
	h.call(cs, "annotate_link", map[string]any{
		"link": link.ID, "note": "Chapter three.", "question": "II"}, &back)
	if back.Note != "Chapter three." || back.Question == nil || *back.Question != "II" {
		t.Fatalf("link = %+v", back)
	}
	if back.Title != link.Title || back.Kind != link.Kind {
		t.Errorf("annotating changed what it was not given: %+v", back)
	}

	// The question clears with an empty string, and the note stays.
	h.call(cs, "annotate_link", map[string]any{"link": link.ID, "question": ""}, &back)
	if back.Question != nil && *back.Question != "" {
		t.Errorf("question = %v", back.Question)
	}
	if back.Note != "Chapter three." {
		t.Errorf("note = %q", back.Note)
	}

	// And the write is the token owner's, carried by the client.
	var via string
	if err := h.db.QueryRowContext(ctx,
		`SELECT coalesce(via, '') FROM activity WHERE entity = 'link' AND action = 'update'
		 ORDER BY id DESC LIMIT 1`).Scan(&via); err != nil {
		t.Fatal(err)
	}
	if via != api.ClientVia(clientName) {
		t.Errorf("via = %q", via)
	}
}

// request_upload is the presigned PUT the REST route hands out, with the file
// row beside it.
func TestRequestUploadNeedsTheFilesScope(t *testing.T) {
	h := newHarness(t)
	f := h.withBoard(t)

	if res := h.call(h.connect(auth.ScopeWrite), "request_upload", map[string]any{
		"proposition": f.prop, "name": "tide.md", "folder": "Documents", "size": 4}, nil); !res.IsError {
		t.Error("a write token asked for an upload")
	}
	var up files.Upload
	h.call(h.connect(auth.ScopeFiles), "request_upload", map[string]any{
		"proposition": f.prop, "name": "tide.md", "folder": "Documents", "size": 4}, &up)
	if up.File.ID == 0 || up.URL == "" {
		t.Fatalf("upload = %+v", up)
	}
	// A size no object may be is refused rather than logged as a fault.
	res := h.call(h.connect(auth.ScopeFiles), "request_upload", map[string]any{
		"proposition": f.prop, "name": "tide.md", "folder": "Documents", "size": 0}, nil)
	if !res.IsError || !strings.Contains(say(res), "1 byte") {
		t.Errorf("a file of no bytes said %q", say(res))
	}
}

// The three resources the plan names, each refused to somebody who may not read
// the proposition they are about.
func TestPropositionResources(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	f := h.withBoard(t)
	page := httptest.NewServer(httpPage())
	t.Cleanup(page.Close)

	cs := h.connect(auth.ScopeRead, auth.ScopeWrite)
	var link files.Link
	h.call(cs, "add_link", map[string]any{"proposition": f.prop, "url": page.URL,
		"note": "Chapter three."}, &link)
	who := core.Actor{Kind: core.KindUser, ID: h.user.ID, Name: h.user.Name}
	if _, err := h.srv.api.Docs.CreateDocument(ctx, who, f.prop, "Research"); err != nil {
		t.Fatal(err)
	}

	id := strconv.FormatInt(f.prop, 10)
	for _, tc := range []struct{ name, uri, holds string }{
		{"the proposition", "theses://proposition/" + id, "Tidal Power"},
		{"its links", "theses://proposition/" + id + "/links", "Chapter three."},
		{"one of its documents", "theses://proposition/" + id + "/document/research", "Research"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: tc.uri})
			if err != nil {
				t.Fatalf("%s: %v", tc.uri, err)
			}
			if len(res.Contents) != 1 || !strings.Contains(res.Contents[0].Text, tc.holds) {
				t.Fatalf("%s = %+v", tc.uri, res.Contents)
			}
			if res.Contents[0].URI != tc.uri {
				t.Errorf("uri = %q", res.Contents[0].URI)
			}
		})
	}

	// A member of nothing is told it is not there, and a document that is not
	// there says so rather than answering empty.
	stranger := h.connectAs(f.stranger, auth.ScopeRead)
	for _, uri := range []string{
		"theses://proposition/" + id,
		"theses://proposition/" + id + "/links",
		"theses://proposition/" + id + "/document/research",
	} {
		if _, err := stranger.ReadResource(ctx, &sdk.ReadResourceParams{URI: uri}); err == nil {
			t.Errorf("a member of nothing read %s", uri)
		}
	}
	if _, err := cs.ReadResource(ctx,
		&sdk.ReadResourceParams{URI: "theses://proposition/" + id + "/document/nowhere"}); err == nil {
		t.Error("a document that is not there was read")
	}
	if _, err := cs.ReadResource(ctx,
		&sdk.ReadResourceParams{URI: "theses://proposition/x"}); err == nil {
		t.Error("a proposition id that is not a number was read")
	}
}

// A proposition that is not there answers the same as one nobody may read.
func TestGetPropositionThatIsNotThere(t *testing.T) {
	h := newHarness(t)
	h.withBoard(t)
	res := h.call(h.connect(auth.ScopeRead), "get_proposition", map[string]any{"proposition": 9999}, nil)
	if !res.IsError || !strings.Contains(say(res), "not there") {
		t.Fatalf("a proposition that is not there said %q", say(res))
	}
}

// httpPage is a page on loopback for the link fetch to read.
func httpPage() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<html><head><meta property="og:title" content="The tide tables">
			<meta name="citation_author" content="Ada Lovelace"></head><body>Twice a day.</body></html>`)
	})
}
