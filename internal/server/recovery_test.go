package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/docs"
	"github.com/davidtorcivia/theses/internal/store"
	"github.com/davidtorcivia/theses/internal/workflow"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDeletedContentRecovery(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	ctx := context.Background()
	a := h.owner()
	prop := h.proposition("Recovery fixture")
	doc, err := h.srv.docs.CreateDocument(ctx, a, prop, "Recovery script")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.srv.docs.InsertBlock(ctx, a, doc.EntityID, 0, "", "Words to recover", true); err != nil {
		t.Fatal(err)
	}
	owner, err := store.UserByID(ctx, h.db, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := h.srv.docs.Document(ctx, owner, doc.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.srv.docs.DeleteDocument(ctx, a, doc.EntityID); err != nil {
		t.Fatal(err)
	}
	items, err := h.srv.board.Trash(ctx, a, prop)
	if err != nil || len(items) != 1 {
		t.Fatalf("trash %+v %v", items, err)
	}
	duplicate, err := h.srv.docs.CreateDocument(ctx, a, prop, "Recovery script")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.srv.board.RestoreDeleted(ctx, a, items[0].ID); !errors.Is(err, core.ErrRestoreConflict) {
		t.Fatalf("name conflict %v", err)
	}
	if _, err = h.srv.docs.Document(ctx, owner, doc.EntityID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("partial restore %v", err)
	}
	if _, err = h.srv.docs.DeleteDocument(ctx, a, duplicate.EntityID); err != nil {
		t.Fatal(err)
	}
	event, err := h.srv.board.RestoreDeleted(ctx, a, items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	var restored docs.Doc
	if err = json.Unmarshal(event.After, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.ID != before.ID || restored.CreatedAt != before.CreatedAt || len(restored.Blocks) != len(before.Blocks) || restored.Revision <= before.Revision {
		t.Fatalf("restored %+v", restored)
	}
	for i, b := range before.Blocks {
		if restored.Blocks[i].ID != b.ID || restored.Blocks[i].Text != b.Text || restored.Blocks[i].Version <= b.Version {
			t.Fatal("block identity/content/version not preserved")
		}
	}
	if _, err = h.srv.board.RestoreDeleted(ctx, a, items[0].ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("double restore %v", err)
	}
	cols, err := board.ListColumns(ctx, h.db, prop)
	if err != nil {
		t.Fatal(err)
	}
	card, err := h.srv.board.CreateCard(ctx, a, cols[0].ID, "Deleted task", []int64{a.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.srv.board.AddChecklistItem(ctx, a, card.EntityID, "Keep this checklist"); err != nil {
		t.Fatal(err)
	}
	if _, err = h.srv.board.PostComment(ctx, a, card.EntityID, "Keep this comment"); err != nil {
		t.Fatal(err)
	}
	if _, err = h.srv.board.DeleteCard(ctx, a, card.EntityID); err != nil {
		t.Fatal(err)
	}
	items, err = h.srv.board.Trash(ctx, a, prop)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Entity == "card" {
			if _, err = h.srv.board.RestoreDeleted(ctx, a, item.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	var comments, checklist, assignees int
	if err = h.db.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM comments WHERE card_id=?),(SELECT count(*) FROM checklist_items WHERE card_id=?),(SELECT count(*) FROM card_assignees WHERE card_id=?)", card.EntityID, card.EntityID, card.EntityID).Scan(&comments, &checklist, &assignees); err != nil || comments != 1 || checklist != 1 || assignees != 1 {
		t.Fatalf("card children %d %d %d %v", comments, checklist, assignees, err)
	}
	e, err := h.srv.api.Workflow.SaveEvidence(ctx, a, workflow.Evidence{Proposition: prop, Title: "Recover reference"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.srv.api.Workflow.DeleteEvidence(ctx, a, e.EntityID, 1); err != nil {
		t.Fatal(err)
	}
	items, err = h.srv.board.Trash(ctx, a, prop)
	if err != nil {
		t.Fatal(err)
	}
	var evidenceTrash int64
	for _, item := range items {
		if item.Entity == "evidence" {
			evidenceTrash = item.ID
		}
	}
	if evidenceTrash == 0 {
		t.Fatal("missing evidence trash")
	}
	other := h.as("outsider", "Outsider", "editor")
	h.client = other
	response, _ := h.get(fmt.Sprintf("/app/trash?proposition=%d", prop))
	if response.StatusCode != 404 {
		t.Fatalf("private trash: %d", response.StatusCode)
	}
	response, _ = h.send("POST", fmt.Sprintf("/app/trash/%d/restore", evidenceTrash), h.csrf("/profile"), "{}")
	if response.StatusCode != 404 {
		t.Fatalf("private restore %d", response.StatusCode)
	}
	if _, err = h.db.ExecContext(ctx, "UPDATE trash SET expires_at=unixepoch() WHERE id=?", evidenceTrash); err != nil {
		t.Fatal(err)
	}
	if _, err = h.srv.board.RestoreDeleted(ctx, a, evidenceTrash); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("expired restore %v", err)
	}
	var foreignKeys int
	rows, err := h.db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		foreignKeys++
	}
	if foreignKeys != 0 {
		t.Fatal("foreign key corruption")
	}
}

func TestRestoreAfterAccountDeletion(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	ctx := context.Background()
	a := h.owner()
	prop := h.proposition("Recovery authors")
	h.as("former", "Former member", "editor")
	var former int64
	if err := h.db.QueryRowContext(ctx, "SELECT id FROM users WHERE handle='former'").Scan(&former); err != nil {
		t.Fatal(err)
	}
	cols, err := board.ListColumns(ctx, h.db, prop)
	if err != nil {
		t.Fatal(err)
	}
	card, err := h.srv.board.CreateCard(ctx, a, cols[0].ID, "Keep task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.srv.board.PostComment(ctx, a, card.EntityID, "Keep comment"); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"UPDATE cards SET created_by=? WHERE id=?", "UPDATE comments SET user_id=? WHERE card_id=?", "INSERT INTO card_assignees(user_id,card_id) VALUES(?,?)"} {
		if _, err = h.db.ExecContext(ctx, query, former, card.EntityID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = h.srv.board.DeleteCard(ctx, a, card.EntityID); err != nil {
		t.Fatal(err)
	}
	if _, err = h.db.ExecContext(ctx, "DELETE FROM users WHERE id=?", former); err != nil {
		t.Fatal(err)
	}
	items, err := h.srv.board.Trash(ctx, a, prop)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.srv.board.RestoreDeleted(ctx, a, items[0].ID); err != nil {
		t.Fatal(err)
	}
	var preserved bool
	if err = h.db.QueryRowContext(ctx, `SELECT created_by IS NULL AND (SELECT user_id IS NULL FROM comments WHERE card_id=cards.id) AND NOT EXISTS(SELECT 1 FROM card_assignees WHERE card_id=cards.id) FROM cards WHERE id=?`, card.EntityID).Scan(&preserved); err != nil || !preserved {
		t.Fatalf("account references: %v %v", preserved, err)
	}
}

func TestRestoredWorkflowActivity(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	ctx := context.Background()
	a := h.owner()
	prop := h.proposition("Workflow recovery")
	evidence, err := h.srv.api.Workflow.SaveEvidence(ctx, a, workflow.Evidence{Proposition: prop, Title: "Recovered source", Quotation: "Preserved quotation"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.srv.api.Workflow.DeleteEvidence(ctx, a, evidence.EntityID, 1); err != nil {
		t.Fatal(err)
	}
	items, err := h.srv.board.Trash(ctx, a, prop)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := h.srv.board.RestoreDeleted(ctx, a, items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	var e workflow.Evidence
	if err = json.Unmarshal(restored.After, &e); err != nil || e.Title != "Recovered source" || e.Quotation != "Preserved quotation" || e.Version != 2 {
		t.Fatalf("evidence activity %+v %v", e, err)
	}
	event, err := h.srv.api.Workflow.SaveCalendarEvent(ctx, a, workflow.CalendarEntry{Title: "Recovered event", Date: "2026-09-23", Notes: "Keep these notes"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.srv.api.Workflow.DeleteCalendarEvent(ctx, a, event.EntityID, 1); err != nil {
		t.Fatal(err)
	}
	show, err := board.GetShow(ctx, h.db)
	if err != nil {
		t.Fatal(err)
	}
	items, err = h.srv.board.Trash(ctx, a, show.ID)
	if err != nil {
		t.Fatal(err)
	}
	restored, err = h.srv.board.RestoreDeleted(ctx, a, items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	var c workflow.CalendarEntry
	if err = json.Unmarshal(restored.After, &c); err != nil || c.Title != "Recovered event" || c.Date != "2026-09-23" || c.Notes != "Keep these notes" || c.Version != 2 {
		t.Fatalf("calendar activity %+v %v", c, err)
	}
}

func TestPersonalKeyExpiry(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	ctx := context.Background()
	_, page := h.postBack("/profile/tokens", url.Values{"csrf": {h.csrf("/profile")}, "name": {"Expiring automation"}, "scopes": {"read write"}, "expiry_days": {"7"}})
	token := tokenRe.FindString(page)
	if token == "" {
		t.Fatal("no key")
	}
	key, _, err := h.srv.auth.LookupAPIToken(ctx, token)
	if err != nil || !key.ExpiresAt.Valid || key.ExpiresAt.Int64 < time.Now().Add(6*24*time.Hour).Unix() {
		t.Fatalf("expiry %+v %v", key, err)
	}
	request := func(path string) int {
		t.Helper()
		req, _ := http.NewRequest("GET", h.http.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	prop := h.proposition("File scope")
	if _, err := h.db.ExecContext(ctx, `INSERT INTO files(id,proposition_id,name,folder,object_key,state,created_at) VALUES(88,?,'take.wav','Recordings','scope-fixture','ready',1)`, prop); err != nil {
		t.Fatal(err)
	}
	if _, err := h.srv.files.Delete(ctx, h.owner(), 88); err != nil {
		t.Fatal(err)
	}
	items, err := h.srv.board.Trash(ctx, h.owner(), prop)
	if err != nil || len(items) != 1 {
		t.Fatalf("file trash %+v %v", items, err)
	}
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/v1/trash/%d/restore", h.http.URL, items[0].ID), strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	result, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	result.Body.Close()
	if result.StatusCode != 403 {
		t.Fatalf("write-only file restore: %d", result.StatusCode)
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "Scoped agent", Version: "test"}, nil)
	session, err := client.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: h.http.URL + "/mcp", HTTPClient: &http.Client{Transport: &bearer{token: token}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	reply, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "restore_deleted", Arguments: map[string]any{"id": items[0].ID}})
	if err != nil || !reply.IsError {
		t.Fatalf("write-only MCP restore: %+v %v", reply, err)
	}
	if request("/api/v1/me") != 200 {
		t.Fatal("valid token refused")
	}
	if _, err = h.db.ExecContext(ctx, "UPDATE api_tokens SET expires_at=unixepoch() WHERE id=?", key.ID); err != nil {
		t.Fatal(err)
	}
	reply, err = session.CallTool(ctx, &sdk.CallToolParams{Name: "whoami"})
	if err == nil && !reply.IsError {
		t.Fatal("expired MCP session remained usable")
	}
	if request("/api/v1/me") != 401 || request("/mcp") != 401 {
		t.Fatal("expired credential accepted")
	}
	for _, days := range []string{"-1", "8", "99999999999", "invalid"} {
		_, page = h.postBack("/profile/tokens", url.Values{"csrf": {h.csrf("/profile")}, "name": {"Bad expiry"}, "scopes": {"read"}, "expiry_days": {days}})
		if tokenRe.MatchString(page) || !strings.Contains(page, "Choose a valid key expiry") {
			t.Fatalf("invalid expiry %s accepted", days)
		}
	}
}
