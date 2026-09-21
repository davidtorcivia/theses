package mcp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/blob"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/files"
	"github.com/davidtorcivia/theses/internal/safehttp"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

// withFiles adds the links and files tools to a harness and returns the
// proposition, the card and a page the fetch can read over loopback.
func (h *harness) withFiles(t *testing.T) (int64, int64, string) {
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

	c := core.New(h.db, core.NewBus())
	b := board.New(c, func() board.Defaults {
		return board.Defaults{Status: "idea", Statuses: []string{"idea", "recording"}, Columns: []string{"Research"}}
	})
	svc := files.New(c, func(context.Context, string) (*blob.Client, error) { return bucket, nil },
		safehttp.Client(safehttp.AllowLoopback()))
	Files(h.srv, svc)

	who := core.Actor{Kind: core.KindUser, ID: h.user.ID, Name: h.user.Name}
	e, err := b.CreateProposition(ctx, who, "Tidal Power")
	if err != nil {
		t.Fatal(err)
	}
	cols, err := board.ListColumns(ctx, h.db, e.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	card, err := b.CreateCard(ctx, who, cols[0].ID, "Read the tide tables", nil)
	if err != nil {
		t.Fatal(err)
	}

	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<html><head><meta property="og:title" content="The tide tables">
			<meta name="citation_author" content="Ada Lovelace"></head><body>Twice a day.</body></html>`)
	}))
	t.Cleanup(page.Close)
	return e.EntityID, card.EntityID, page.URL
}

func TestLinkAndFileToolsAreListed(t *testing.T) {
	h := newHarness(t)
	h.withFiles(t)
	res, err := h.connect(auth.ScopeRead).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"list_links": true, "add_link": true, "list_files": true,
		"get_download_url": true, "attach_to_card": true}
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
		}
	}
	if len(want) != 0 {
		t.Errorf("missing tools: %v", want)
	}
}

// A write through MCP is the person who owns the token, with the client's name
// in via, and it shows in the activity log like any other edit.
func TestAddLinkIsAttributedToTheTokenOwner(t *testing.T) {
	h := newHarness(t)
	proposition, card, page := h.withFiles(t)
	cs := h.connect(auth.ScopeRead, auth.ScopeWrite)

	var added files.Link
	if res := h.call(cs, "add_link", map[string]any{
		"proposition": proposition, "url": page, "note": "Chapter 3.", "question": "II", "key": "annotated-link",
	}, &added); res.IsError {
		t.Fatalf("add_link: %v", res.Content)
	}
	if added.Title != "The tide tables" || added.Author != "Ada Lovelace" {
		t.Fatalf("the page was not read: %+v", added)
	}
	if added.Note != "Chapter 3." || added.Question == nil || *added.Question != "II" {
		t.Fatalf("the agent's own words were not saved: %+v", added)
	}
	if !strings.Contains(added.Citation, "Ada Lovelace") {
		t.Fatalf("citation %q", added.Citation)
	}
	var replayed files.Link
	if res := h.call(cs, "add_link", map[string]any{
		"proposition": proposition, "url": page, "note": "Changed.", "question": "III", "key": "annotated-link",
	}, &replayed); res.IsError {
		t.Fatalf("replay: %v", res.Content)
	}
	if replayed.ID != added.ID || replayed.Note != added.Note || replayed.Question == nil || *replayed.Question != "II" {
		t.Fatalf("annotations changed on replay: %+v", replayed)
	}
	var events int
	if err := h.db.QueryRowContext(context.Background(), `SELECT count(*) FROM activity WHERE entity = 'link' AND entity_id = ?`, added.ID).Scan(&events); err != nil || events != 1 {
		t.Fatalf("link command events = %d, err %v", events, err)
	}

	var kind, actorID, via string
	if err := h.db.QueryRowContext(context.Background(),
		`SELECT actor_kind, actor_id, coalesce(via, '') FROM activity
		 WHERE entity = 'link' AND action = 'create' ORDER BY id DESC LIMIT 1`).
		Scan(&kind, &actorID, &via); err != nil {
		t.Fatal(err)
	}
	if kind != "user" || actorID != strconv.FormatInt(h.user.ID, 10) {
		t.Fatalf("the link was filed as %s %s, want the token's owner", kind, actorID)
	}
	if via != "mcp:"+clientName {
		t.Fatalf("via = %q, want the client's name", via)
	}

	var attached attachOut
	if res := h.call(cs, "attach_to_card", map[string]any{
		"card": card, "link": added.ID,
	}, &attached); res.IsError {
		t.Fatalf("attach_to_card: %v", res.Content)
	}
	if attached.Action != "attach" {
		t.Fatalf("action %q", attached.Action)
	}
}

func TestInvalidLinkAnnotationsDoNotCreateALink(t *testing.T) {
	h := newHarness(t)
	proposition, _, page := h.withFiles(t)
	cs := h.connect(auth.ScopeRead, auth.ScopeWrite)
	for _, fields := range []map[string]any{
		{"question": "V"},
		{"note": strings.Repeat("x", board.MaxBody+1)},
	} {
		fields["proposition"], fields["url"] = proposition, page
		if res := h.call(cs, "add_link", fields, nil); !res.IsError {
			t.Fatal("invalid annotations accepted")
		}
	}
	var count int
	if err := h.db.QueryRowContext(context.Background(), `SELECT count(*) FROM links`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial links = %d, err %v", count, err)
	}
}

func TestFileToolsRefuseWhatTheTokenMayNotDo(t *testing.T) {
	h := newHarness(t)
	proposition, card, page := h.withFiles(t)

	for _, tc := range []struct {
		name, tool string
		scopes     []string
		args       map[string]any
	}{
		{
			name: "adding a link needs write", tool: "add_link",
			scopes: []string{auth.ScopeRead},
			args:   map[string]any{"proposition": proposition, "url": page},
		},
		{
			name: "listing needs read", tool: "list_links",
			scopes: []string{auth.ScopeWrite},
			args:   map[string]any{"proposition": proposition},
		},
		{
			name: "a proposition that is not there", tool: "list_files",
			scopes: []string{auth.ScopeRead},
			args:   map[string]any{"proposition": 9999},
		},
		{
			name: "a file that is not there", tool: "get_download_url",
			scopes: []string{auth.ScopeRead},
			args:   map[string]any{"file": 9999},
		},
		{
			name: "attaching neither a link nor a file", tool: "attach_to_card",
			scopes: []string{auth.ScopeWrite},
			args:   map[string]any{"card": card},
		},
		{
			name: "attaching both at once", tool: "attach_to_card",
			scopes: []string{auth.ScopeWrite},
			args:   map[string]any{"card": card, "link": 1, "file": 1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := h.connect(tc.scopes...)
			res := h.call(cs, tc.tool, tc.args, nil)
			if !res.IsError {
				t.Fatalf("%s went through: %v", tc.tool, res.StructuredContent)
			}
		})
	}
}
