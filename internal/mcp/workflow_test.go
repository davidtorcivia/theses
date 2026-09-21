package mcp

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/files"
	"github.com/davidtorcivia/theses/internal/workflow"
)

func TestWorkflowToolsAndPublishedContract(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a := core.Actor{Kind: core.KindUser, ID: h.user.ID}
	show, err := board.EnsureShow(ctx, h.db)
	if err != nil {
		t.Fatal(err)
	}
	cols, err := board.ListColumns(ctx, h.db, show.ID)
	if err != nil {
		t.Fatal(err)
	}
	h.srv.api.Board = h.board
	h.srv.api.Files = files.New(h.board.Service, nil, nil)
	h.srv.api.Workflow = workflow.New(h.board.Service)
	h.srv.api.Workflow.Files = h.srv.api.Files
	h.srv.api.Workflow.WhisperURL = "http://whisper.invalid"
	Workflow(h.srv)
	p, err := h.board.CreateProposition(ctx, a, "Episode")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := h.srv.api.Docs.CreateDocument(ctx, a, p.EntityID, "Script")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.db.ExecContext(ctx, `INSERT INTO files(id,proposition_id,name,folder,object_key,state,created_at) VALUES(44,?,'take.wav','Recordings','fixture','ready',unixepoch())`, p.EntityID); err != nil {
		t.Fatal(err)
	}
	cs := h.connect(auth.ScopeRead, auth.ScopeWrite)
	list, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../docs/api.md")
	if err != nil {
		t.Fatal(err)
	}
	documented := map[string]string{}
	for _, row := range regexp.MustCompile("(?m)^\\| `([a-z_]+)` \\| `(read|write)` \\| `(GET|POST|PUT|PATCH|DELETE) /api/v1/[^`]+` \\|").FindAllStringSubmatch(string(raw), -1) {
		documented[row[1]] = row[2]
	}
	reads := h.connect(auth.ScopeRead)
	var workflowCount int
	for _, tool := range list.Tools {
		scope, ok := documented[tool.Name]
		schemaRaw, err := json.Marshal(tool.OutputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err = json.Unmarshal(schemaRaw, &schema); err != nil {
			t.Fatal(err)
		}
		if !ok {
			if len(schema.Properties["result"]) > 0 {
				t.Fatalf("undocumented workflow tool %s", tool.Name)
			}
			continue
		}
		workflowCount++
		if len(schema.Properties["result"]) == 0 || string(schema.Properties["result"]) == "{}" {
			t.Fatalf("missing typed output for %s", tool.Name)
		}
		if tool.Annotations.ReadOnlyHint != (scope == auth.ScopeRead) {
			t.Fatalf("scope annotation %s", tool.Name)
		}

	}
	if workflowCount != 24 || len(documented) != 24 {
		t.Fatalf("workflow documentation drift: runtime %d documented %d", workflowCount, len(documented))
	}
	call := func(name string, args any) map[string]json.RawMessage {
		t.Helper()
		var out map[string]json.RawMessage
		result := h.call(cs, name, args, &out)
		if result.IsError {
			t.Fatalf("%s: %+v", name, result.Content)
		}
		return out
	}
	event := func(name string, args any) core.Event {
		t.Helper()
		if !h.call(reads, name, args, nil).IsError {
			t.Fatalf("read-only key invoked %s", name)
		}
		out := call(name, args)
		var ev core.Event
		if err := json.Unmarshal(out["result"], &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Actor.ID != a.ID || !strings.HasPrefix(ev.Actor.Via, "mcp:") {
			t.Fatalf("attribution %+v", ev)
		}
		return ev
	}
	cal := map[string]any{"title": "Planning", "date": "2026-10-15", "key": "calendar-retry"}
	created := event("save_calendar_event", cal)
	replay := event("save_calendar_event", cal)
	if created.Seq != replay.Seq || !replay.Replayed {
		t.Fatal("calendar replay duplicated")
	}
	event("create_calendar_task", map[string]any{"column": cols[0].ID, "title": "Check citations", "date": "2026-10-15"})
	call("list_calendar_entries", map[string]any{})
	plan := board.ProductionPlan{Proposition: p.EntityID, NextAction: "Research"}
	event("save_production_plan", map[string]any{"plan": plan})
	call("get_production_plan", map[string]any{"proposition": p.EntityID})
	evidence := workflow.Evidence{Proposition: p.EntityID, Title: "Source", Quotation: "Verified text"}
	ev := event("save_evidence", map[string]any{"evidence": evidence, "key": "evidence-retry"})
	json.Unmarshal(ev.After, &evidence)
	call("list_evidence", map[string]any{"proposition": p.EntityID})
	call("export_evidence", map[string]any{"proposition": p.EntityID, "format": "ris"})
	stale := evidence
	stale.Version = 0
	if !h.call(cs, "save_evidence", map[string]any{"evidence": stale}, nil).IsError {
		t.Fatal("stale evidence accepted")
	}
	pinned := event("pin_script", map[string]any{"document": doc.EntityID, "cues": "Pause"})
	call("get_snapshot", map[string]any{"id": pinned.EntityID})
	call("list_snapshots", map[string]any{"document": doc.EntityID})
	review := event("request_review", map[string]any{"document": doc.EntityID, "reviewer": a.ID})
	call("list_reviews", map[string]any{"proposition": p.EntityID})
	event("decide_review", map[string]any{"id": review.EntityID, "version": 1, "state": "approved"})
	event("save_transcript", map[string]any{"file": 44, "version": 0, "format": "vtt", "text": "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\n<v Host>Passage</v>\n"})
	call("get_transcript", map[string]any{"file": 44})
	call("export_transcript", map[string]any{"file": 44, "format": "vtt"})
	event("queue_transcription", map[string]any{"file": 44})
	call("list_transcription_jobs", map[string]any{"file": 44})
	_, err = h.srv.api.Files.AddFileComment(ctx, a, 44, 1000, "Listen")
	if err != nil {
		t.Fatal(err)
	}
	var comment int64
	if err := h.db.QueryRowContext(ctx, "SELECT id FROM file_comments WHERE file_id=44").Scan(&comment); err != nil {
		t.Fatal(err)
	}
	event("resolve_file_comment", map[string]any{"file": 44, "comment": comment, "version": 1, "resolved": true})
	event("delete_evidence", map[string]any{"id": evidence.ID, "version": evidence.Version})
	event("delete_calendar_event", map[string]any{"id": created.EntityID, "version": 1})
	out := call("list_trash", map[string]any{"proposition": p.EntityID})
	var trash []core.TrashItem
	json.Unmarshal(out["result"], &trash)
	if len(trash) != 1 {
		t.Fatalf("trash %+v", trash)
	}
	restored := event("restore_deleted", map[string]any{"id": trash[0].ID, "key": "restore-once"})
	restoredAgain := event("restore_deleted", map[string]any{"id": trash[0].ID, "key": "restore-once"})
	if restored.Seq != restoredAgain.Seq || !restoredAgain.Replayed {
		t.Fatal("restore retry did not replay")
	}
	// Live role and membership are still enforced by the shared services.
	if _, err := h.db.ExecContext(ctx, `UPDATE users SET role='editor' WHERE id=?`, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.ExecContext(ctx, `DELETE FROM proposition_members WHERE proposition_id=? AND user_id=?`, p.EntityID, a.ID); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args map[string]any
	}{{"list_evidence", map[string]any{"proposition": p.EntityID}}, {"get_snapshot", map[string]any{"id": pinned.EntityID}}, {"get_transcript", map[string]any{"file": 44}}, {"list_reviews", map[string]any{"proposition": p.EntityID}}, {"list_trash", map[string]any{"proposition": p.EntityID}}} {
		if !h.call(cs, tc.name, tc.args, nil).IsError {
			t.Fatalf("private %s leaked", tc.name)
		}
	}
}
