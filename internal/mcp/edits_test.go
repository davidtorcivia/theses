package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davidtorcivia/theses/internal/api"
	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/docs"
	"github.com/davidtorcivia/theses/internal/files"
	"github.com/davidtorcivia/theses/internal/notify"
)

// result runs a tool registered through workflowTool and decodes what it
// answered, failing the test on a refusal.
func (h *harness) result(cs *sdk.ClientSession, name string, args, out any) {
	h.Helper()
	var wrapped struct {
		Result json.RawMessage `json:"result"`
	}
	if res := h.call(cs, name, args, &wrapped); res.IsError {
		h.Fatalf("%s refused: %s", name, say(res))
	}
	if out != nil {
		if err := json.Unmarshal(wrapped.Result, out); err != nil {
			h.Fatalf("%s returned %s: %v", name, wrapped.Result, err)
		}
	}
}

// refused runs a tool that should not go through and says what it answered.
func (h *harness) refused(cs *sdk.ClientSession, name string, args any) string {
	h.Helper()
	res := h.call(cs, name, args, nil)
	if !res.IsError {
		h.Fatalf("%s went through", name)
	}
	return say(res)
}

// Each tool is one sentence and carries its hints, and a token without the
// scope the matching REST route asks for is refused before anything runs.
func TestEditToolsAreListedAndScoped(t *testing.T) {
	h := newHarness(t)
	f := h.withBoard(t)
	h.srv.api.Board = h.board
	res, err := h.connect(auth.ScopeRead).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]*sdk.Tool{}
	for _, tool := range res.Tools {
		listed[tool.Name] = tool
	}
	tests := []struct {
		tool  string
		scope string // the one scope a token needs, so every other is refused
		args  map[string]any
	}{
		{"edit_proposition", auth.ScopeWrite, map[string]any{"proposition": f.prop, "title": "x"}},
		{"move_proposition", auth.ScopeWrite, map[string]any{"proposition": f.prop}},
		{"archive_proposition", auth.ScopeWrite, map[string]any{"proposition": f.prop}},
		{"delete_proposition", auth.ScopeWrite, map[string]any{"proposition": f.prop}},
		{"set_member", auth.ScopeWrite, map[string]any{"proposition": f.prop, "user": f.stranger.ID}},
		{"create_column", auth.ScopeWrite, map[string]any{"proposition": f.prop, "name": "x"}},
		{"rename_column", auth.ScopeWrite, map[string]any{"column": f.column, "name": "x"}},
		{"move_column", auth.ScopeWrite, map[string]any{"column": f.column}},
		{"delete_column", auth.ScopeWrite, map[string]any{"column": f.column}},
		{"edit_card", auth.ScopeWrite, map[string]any{"card": f.card, "question": "I"}},
		{"delete_card", auth.ScopeWrite, map[string]any{"card": f.card}},
		{"add_checklist_item", auth.ScopeWrite, map[string]any{"card": f.card, "text": "x"}},
		{"check_item", auth.ScopeWrite, map[string]any{"item": 1, "done": true}},
		{"remove_checklist_item", auth.ScopeWrite, map[string]any{"item": 1}},
		{"delete_comment", auth.ScopeWrite, map[string]any{"comment": 1}},
		{"undo", auth.ScopeWrite, map[string]any{"activity": 1}},
		{"rename_document", auth.ScopeWrite, map[string]any{"document": 1, "name": "x"}},
		{"delete_document", auth.ScopeWrite, map[string]any{"document": 1}},
		{"list_revisions", auth.ScopeRead, map[string]any{"document": 1}},
		{"create_revision", auth.ScopeWrite, map[string]any{"document": 1}},
		{"refetch_link", auth.ScopeWrite, map[string]any{"link": 1}},
		{"delete_link", auth.ScopeWrite, map[string]any{"link": 1}},
		{"upload_parts", auth.ScopeFiles, map[string]any{"file": 1}},
		{"complete_upload", auth.ScopeFiles, map[string]any{"file": 1}},
		{"delete_file", auth.ScopeFiles, map[string]any{"file": 1}},
		{"list_file_versions", auth.ScopeRead, map[string]any{"file": 1}},
		{"list_webhooks", auth.ScopeAdmin, map[string]any{}},
		{"save_webhook", auth.ScopeAdmin, map[string]any{"url": "https://example.com/h"}},
		{"delete_webhook", auth.ScopeAdmin, map[string]any{"id": 1}},
		{"test_webhook", auth.ScopeAdmin, map[string]any{"id": 1}},
		{"list_backups", auth.ScopeAdmin, map[string]any{}},
		{"verify_backup", auth.ScopeAdmin, map[string]any{"archive": "x"}},
		{"restore_backup", auth.ScopeAdmin, map[string]any{"archive": "x", "confirm": "x"}},
	}
	for _, tt := range tests {
		t.Run(tt.tool, func(t *testing.T) {
			tool := listed[tt.tool]
			if tool == nil {
				t.Fatal("not listed")
			}
			if !strings.HasSuffix(tool.Description, ".") || strings.Count(tool.Description, ".") != 1 {
				t.Errorf("description is not one sentence: %q", tool.Description)
			}
			if tool.Annotations == nil || tool.Annotations.Title == "" || tool.OutputSchema == nil {
				t.Fatalf("no annotations or no output schema")
			}
			// A client asks before it runs a tool that changes something, so
			// only the ones that look may say they only look.
			looks := strings.HasPrefix(tt.tool, "list_") || tt.tool == "upload_parts"
			if a := tool.Annotations; a.ReadOnlyHint != looks || (!looks && a.DestructiveHint == nil) {
				t.Errorf("hints = %+v", a)
			}
			// Admin carries every scope, so it is refused only what needs more.
			for _, scope := range []string{auth.ScopeRead, auth.ScopeWrite, auth.ScopeFiles} {
				if scope == tt.scope {
					continue
				}
				res := h.call(h.connect(scope), tt.tool, tt.args, nil)
				if why := say(res); !res.IsError || !strings.Contains(why, tt.scope+" scope") {
					t.Errorf("a %s token was told %q", scope, why)
				}
			}
		})
	}
}

// Every board edit the browser can make, made through MCP by an agent in the
// order an agent would, each one checked against what the board then holds.
func TestBoardEditTools(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	f := h.withBoard(t)
	h.srv.api.Board = h.board
	cs := h.connect(auth.ScopeRead, auth.ScopeWrite)

	one := func() board.Proposition {
		t.Helper()
		p, err := board.GetProposition(ctx, h.db, f.prop)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	cardOf := func(id int64) board.Card {
		t.Helper()
		c, err := board.GetCard(ctx, h.db, id)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	var column, item, note, rename int64
	var e core.Event
	steps := []struct {
		name  string
		tool  string
		args  func() map[string]any
		check func(t *testing.T)
	}{
		{"edit and schedule", "edit_proposition", func() map[string]any {
			return map[string]any{"proposition": f.prop, "title": "Tidal Energy", "episode": "12", "target_date": "2026-11-02"}
		}, func(t *testing.T) {
			if p := one(); p.Title != "Tidal Energy" || p.Episode == nil || *p.Episode != "12" || p.TargetDate == nil {
				t.Errorf("proposition = %+v", p)
			}
		}},
		{"a member", "set_member", func() map[string]any {
			return map[string]any{"proposition": f.prop, "user": f.stranger.ID}
		}, func(t *testing.T) {
			if mine, _ := board.Memberships(ctx, h.db, f.stranger.ID); !mine[f.prop] {
				t.Error("the stranger is not a member")
			}
		}},
		{"archive", "archive_proposition", func() map[string]any { return map[string]any{"proposition": f.prop} },
			func(t *testing.T) {
				if one().ArchivedAt == nil {
					t.Error("not archived")
				}
				if why := h.refused(cs, "create_column", map[string]any{"proposition": f.prop, "name": "Late"}); !strings.Contains(why, "archived") {
					t.Errorf("an archived proposition took a column: %q", why)
				}
			}},
		{"restore", "archive_proposition", func() map[string]any { return map[string]any{"proposition": f.prop, "restore": true} },
			func(t *testing.T) {
				if one().ArchivedAt != nil {
					t.Error("still archived")
				}
			}},
		{"move", "move_proposition", func() map[string]any { return map[string]any{"proposition": f.prop} }, nil},
		{"a column", "create_column", func() map[string]any {
			return map[string]any{"proposition": f.prop, "name": "Drafts", "key": "column-1"}
		}, func(t *testing.T) {
			column = e.EntityID
			var again core.Event
			h.result(cs, "create_column", map[string]any{"proposition": f.prop, "name": "Drafts", "key": "column-1"}, &again)
			if again.EntityID != column {
				t.Errorf("the same key made column %d after %d", again.EntityID, column)
			}
		}},
		{"rename it", "rename_column", func() map[string]any { return map[string]any{"column": column, "name": "Final"} },
			func(t *testing.T) { rename = e.Seq }},
		{"undo the rename", "undo", func() map[string]any { return map[string]any{"activity": rename} },
			func(t *testing.T) {
				cols, _ := board.ListColumns(ctx, h.db, f.prop)
				for _, c := range cols {
					if c.ID == column && c.Name != "Drafts" {
						t.Errorf("undo left %q", c.Name)
					}
				}
			}},
		{"move it", "move_column", func() map[string]any { return map[string]any{"column": column} }, nil},
		{"edit a card", "edit_card", func() map[string]any {
			c := cardOf(f.card)
			return map[string]any{"card": f.card, "title": "Read the tables", "description_md": "Both harbors.",
				"question": "II", "base_version": c.Version}
		}, func(t *testing.T) {
			if c := cardOf(f.card); c.Title != "Read the tables" || c.Description != "Both harbors." {
				t.Errorf("card = %+v", c)
			}
			if why := h.refused(cs, "edit_card", map[string]any{"card": f.card, "title": "Stale", "base_version": 1}); !strings.Contains(why, "Read the tables") {
				t.Errorf("a stale edit said %q", why)
			}
		}},
		{"an item", "add_checklist_item", func() map[string]any { return map[string]any{"card": f.card, "text": "Low tide"} },
			func(t *testing.T) { item = e.EntityID }},
		{"tick it", "check_item", func() map[string]any { return map[string]any{"item": item, "done": true} }, nil},
		{"remove it", "remove_checklist_item", func() map[string]any { return map[string]any{"item": item} },
			func(t *testing.T) {
				if c := cardOf(f.card); len(c.Checklist) != 0 {
					t.Errorf("checklist = %+v", c.Checklist)
				}
			}},
		{"delete a note", "delete_comment", func() map[string]any {
			var made writeOut
			h.call(cs, "comment", map[string]any{"card": f.card, "text": "Wrong harbor."}, &made)
			note = made.ID
			return map[string]any{"comment": note}
		}, func(t *testing.T) {
			if c := cardOf(f.card); len(c.Comments) != 0 {
				t.Errorf("comments = %+v", c.Comments)
			}
		}},
		{"delete the card", "delete_card", func() map[string]any { return map[string]any{"card": f.card} }, nil},
		{"delete the column", "delete_column", func() map[string]any { return map[string]any{"column": column} }, nil},
		{"delete the proposition", "delete_proposition", func() map[string]any { return map[string]any{"proposition": f.prop} },
			func(t *testing.T) {
				if _, err := board.GetProposition(ctx, h.db, f.prop); err == nil {
					t.Error("the proposition is still there")
				}
			}},
	}
	// One after another rather than as subtests: each step reads what the one
	// before it made.
	for _, step := range steps {
		e = core.Event{}
		h.result(cs, step.tool, step.args(), &e)
		if e.Actor.Via != api.ClientVia(clientName) {
			t.Errorf("%s was carried by %q", step.name, e.Actor.Via)
		}
		if step.check != nil {
			step.check(t)
		}
	}
}

// A document renamed, kept as a revision, read back from its history and
// deleted, the way the history panel and the tab menu do it.
func TestDocumentEditTools(t *testing.T) {
	h := newHarness(t)
	h.withBoard(t)
	cs := h.connect(auth.ScopeRead, auth.ScopeWrite)
	var doc writeOut
	h.call(cs, "create_document", map[string]any{"proposition": h.proposition(), "name": "Script"}, &doc)
	h.call(cs, "append_block", map[string]any{"document": doc.ID, "text": "Cold open."}, nil)

	var e core.Event
	h.result(cs, "rename_document", map[string]any{"document": doc.ID, "name": "Final script"}, &e)
	h.result(cs, "create_revision", map[string]any{"document": doc.ID, "key": "rev-1"}, &e)
	var revisions []docs.Revision
	h.result(cs, "list_revisions", map[string]any{"document": doc.ID}, &revisions)
	if len(revisions) != 1 || !strings.Contains(revisions[0].Markdown, "Cold open.") {
		t.Fatalf("revisions = %+v", revisions)
	}
	h.result(cs, "delete_document", map[string]any{"document": doc.ID}, &e)
	h.refused(cs, "read_document", map[string]any{"document": doc.ID})
}

// An upload finished through MCP alone: asked for under a key, put in the
// bucket, completed, listed among its versions and deleted. A link is corrected
// by hand, read again from its page and deleted.
func TestFileAndLinkEditTools(t *testing.T) {
	h := newHarness(t)
	f := h.withBoard(t)
	cs := h.connect(auth.ScopeRead, auth.ScopeWrite, auth.ScopeFiles)
	body := "tide tables\n"

	ask := map[string]any{"proposition": f.prop, "name": "tides.md", "folder": "Documents",
		"size": len(body), "key": "upload-1"}
	var up, again files.Upload
	h.call(cs, "request_upload", ask, &up)
	h.call(cs, "request_upload", ask, &again)
	if up.File.ID == 0 || again.File.ID != up.File.ID {
		t.Fatalf("the same key made file %d after %d", again.File.ID, up.File.ID)
	}
	if why := h.refused(cs, "complete_upload", map[string]any{"file": up.File.ID}); why == "" {
		t.Error("an empty upload completed")
	}
	req, err := http.NewRequest(http.MethodPut, up.URL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", up.Headers["Content-Type"])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var e core.Event
	h.result(cs, "complete_upload", map[string]any{"file": up.File.ID}, &e)
	var next files.Upload
	h.call(cs, "request_upload", map[string]any{"proposition": f.prop, "name": "tides.md", "folder": "Documents",
		"size": len(body), "replace": up.File.ID}, &next)
	var versions []files.File
	h.result(cs, "list_file_versions", map[string]any{"file": next.File.ID}, &versions)
	if len(versions) != 1 || versions[0].ID != up.File.ID || !versions[0].Ready() {
		t.Fatalf("versions = %+v", versions)
	}
	h.result(cs, "delete_file", map[string]any{"file": next.File.ID}, &e)
	if e.Entity != "file" || e.Action != "delete" {
		t.Errorf("delete = %+v", e)
	}

	page := httptest.NewServer(httpPage())
	t.Cleanup(page.Close)
	var link files.Link
	h.call(cs, "add_link", map[string]any{"proposition": f.prop, "url": page.URL}, &link)
	h.call(cs, "annotate_link", map[string]any{"link": link.ID, "title": "Tables", "author": "", "year": "1843"}, &link)
	if link.Title != "Tables" || link.Author != "" || link.Year != "1843" {
		t.Fatalf("annotated = %+v", link)
	}
	h.result(cs, "refetch_link", map[string]any{"link": link.ID}, &link)
	if link.Title != "The tide tables" || link.Author != "Ada Lovelace" {
		t.Errorf("read again = %+v", link)
	}
	h.result(cs, "delete_link", map[string]any{"link": link.ID}, &e)
	if e.Entity != "link" || e.Action != "delete" {
		t.Errorf("delete = %+v", e)
	}
}

// The workspace's webhooks through MCP: the same rules as the settings page, a
// secret that is never sent back and is kept when left out, and a log row that
// says who changed it without saying where it posts.
func TestWebhookTools(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.withBoard(t)
	cs := h.connect(auth.ScopeAdmin)
	url := "https://hooks.example.com/services/T0/B1?token=abc"

	if why := h.refused(cs, "save_webhook", map[string]any{"url": url}); !strings.Contains(why, "fires on nothing") {
		t.Errorf("a webhook with no events said %q", why)
	}
	var hook api.ChannelView
	h.result(cs, "save_webhook", map[string]any{"url": url, "secret": "shh_signing", "events": []string{"moved"}}, &hook)
	if hook.ID == 0 || !hook.SecretSet || hook.Verified {
		t.Fatalf("saved = %+v", hook)
	}
	h.result(cs, "save_webhook", map[string]any{"id": hook.ID, "column": "Publication"}, &hook)
	stored, err := notify.GetChannel(ctx, h.db, h.set, hook.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Config.Secret != "shh_signing" || stored.Config.Column != "Publication" || stored.Config.URL != url {
		t.Errorf("stored = %+v", stored.Config)
	}
	var hooks []api.ChannelView
	h.result(cs, "list_webhooks", map[string]any{}, &hooks)
	if len(hooks) != 1 || hooks[0].ID != hook.ID {
		t.Fatalf("hooks = %+v", hooks)
	}
	h.result(cs, "delete_webhook", map[string]any{"id": hook.ID}, nil)
	h.refused(cs, "delete_webhook", map[string]any{"id": hook.ID})

	var rows activityOut
	h.call(h.connect(auth.ScopeRead, auth.ScopeAdmin), "activity", map[string]any{"limit": 200}, &rows)
	var actions []string
	for _, row := range rows.Activity {
		if row.Entity != "notification_channel" {
			continue
		}
		actions = append(actions, row.Action)
		raw, _ := json.Marshal(row)
		if strings.Contains(string(raw), "shh_signing") || strings.Contains(string(raw), "token=abc") ||
			strings.Contains(string(raw), "/services/") {
			t.Errorf("the log carries a secret: %s", raw)
		}
		if row.Via != api.ClientVia(clientName) || row.ActorID != strconv.FormatInt(h.user.ID, 10) {
			t.Errorf("row = %+v", row)
		}
	}
	if got := strings.Join(actions, " "); got != "create update delete" {
		t.Errorf("actions = %q", got)
	}
	// A token without admin reads the log without them.
	h.call(h.connect(auth.ScopeRead), "activity", map[string]any{"limit": 200}, &rows)
	for _, row := range rows.Activity {
		if row.Entity == "notification_channel" {
			t.Errorf("a read token read %+v", row)
		}
	}
}

// A build without backups says so, rather than failing as a fault.
func TestBackupToolsWithoutBackups(t *testing.T) {
	h := newHarness(t)
	h.withBoard(t)
	cs := h.connect(auth.ScopeAdmin)
	for tool, args := range map[string]map[string]any{
		"list_backups":   {},
		"verify_backup":  {"archive": "backups/x.tar.gz.age"},
		"restore_backup": {"archive": "backups/x.tar.gz.age", "confirm": "backups/x.tar.gz.age"},
	} {
		if why := h.refused(cs, tool, args); why != api.ErrNoBackups.Error() {
			t.Errorf("%s said %q", tool, why)
		}
	}
	// A restore that does not repeat its key starts nothing, whatever else.
	for _, confirm := range []string{"", "backups/y.tar.gz.age"} {
		if why := h.refused(cs, "restore_backup", map[string]any{"archive": "backups/x.tar.gz.age", "confirm": confirm}); why != api.ErrUnconfirmed.Error() {
			t.Errorf("confirm %q said %q", confirm, why)
		}
	}
}
