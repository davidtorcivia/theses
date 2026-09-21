package mcp

import (
	"context"
	"encoding/json"
	"reflect"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/workflow"
	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type workflowKey struct {
	Key string `json:"key,omitempty" jsonschema:"retry key; reuse with the identical payload after an uncertain response"`
}

func (k workflowKey) retryKey() string { return k.Key }

type documentArgs struct {
	Document int64 `json:"document"`
}

type restoreArgs struct {
	workflowKey
	ID int64 `json:"id"`
}

type workflowID struct {
	ID int64 `json:"id" jsonschema:"resource ID"`
}
type workflowVersion struct {
	workflowKey
	ID      int64 `json:"id"`
	Version int64 `json:"version" jsonschema:"current version read before editing"`
}
type calendarSave struct {
	workflowKey
	ID      int64  `json:"id,omitempty"`
	Version int64  `json:"version,omitempty"`
	Title   string `json:"title"`
	Date    string `json:"date" jsonschema:"all-day date YYYY-MM-DD between 2000 and 2100"`
	Notes   string `json:"notes,omitempty"`
}
type calendarTask struct {
	workflowKey
	Column int64  `json:"column" jsonschema:"Show board column ID"`
	Title  string `json:"title"`
	Date   string `json:"date"`
}
type evidenceSave struct {
	workflowKey
	Evidence workflow.Evidence `json:"evidence" jsonschema:"reference with proposition_id and current version; id zero creates a reference"`
}
type evidenceExport struct {
	Proposition int64  `json:"proposition"`
	Format      string `json:"format" jsonschema:"md or ris"`
}
type snapshotPin struct {
	workflowKey
	Document int64  `json:"document"`
	Cues     string `json:"cues,omitempty"`
}
type reviewRequest struct {
	workflowKey
	Document int64 `json:"document,omitempty"`
	File     int64 `json:"file,omitempty"`
	Reviewer int64 `json:"reviewer"`
}
type reviewDecision struct {
	workflowVersion
	State string `json:"state" jsonschema:"approved or changes_requested"`
	Note  string `json:"note,omitempty"`
}
type transcriptSave struct {
	workflowKey
	File     int64              `json:"file"`
	Version  int64              `json:"version"`
	Format   string             `json:"format,omitempty" jsonschema:"txt, srt or vtt when importing text; otherwise provide segments"`
	Text     string             `json:"text,omitempty"`
	Segments []workflow.Segment `json:"segments,omitempty"`
}
type transcriptExport struct {
	File   int64  `json:"file"`
	Format string `json:"format" jsonschema:"txt or vtt"`
}
type transcriptionQueue struct {
	workflowKey
	File   int64 `json:"file"`
	Stereo bool  `json:"stereo,omitempty" jsonschema:"label separate left and right channels; not mixed-audio diarization"`
}
type commentResolve struct {
	workflowKey
	File     int64 `json:"file"`
	Comment  int64 `json:"comment"`
	Version  int64 `json:"version"`
	Resolved bool  `json:"resolved"`
}
type planSave struct {
	workflowKey
	Plan board.ProductionPlan `json:"plan" jsonschema:"production plan with proposition_id and current version"`
}

type workflowOutput[O any] struct {
	Result O `json:"result"`
}

func workflowTool[I any, O any](s *Server, name, description, scope string, hints *sdk.ToolAnnotations, run func(context.Context, core.Actor, I) (O, error)) {
	schema, err := jsonschema.For[workflowOutput[O]](&jsonschema.ForOptions{TypeSchemas: map[reflect.Type]*jsonschema.Schema{reflect.TypeFor[json.RawMessage](): {}}})
	if err != nil {
		panic(err)
	}
	sdk.AddTool(s.srv, &sdk.Tool{Name: name, Description: description, Annotations: hints, OutputSchema: schema}, func(ctx context.Context, req *sdk.CallToolRequest, in I) (*sdk.CallToolResult, workflowOutput[O], error) {
		p, err := principal(ctx, scope)
		if err != nil {
			return nil, workflowOutput[O]{}, err
		}
		if input, ok := any(in).(interface{ retryKey() string }); ok {
			ctx, err = keyed(ctx, input.retryKey())
			if err != nil {
				return nil, workflowOutput[O]{}, err
			}
		}
		out, err := run(ctx, person(req, p), in)
		if err != nil {
			return nil, workflowOutput[O]{}, s.refusal(name, err)
		}
		return nil, workflowOutput[O]{Result: out}, nil
	})
}

// Workflow exposes the same commands and permission checks used by the REST routes.
func Workflow(s *Server) {
	w := s.api.Workflow
	workflowTool(s, "list_trash", "Lists recoverable deleted items in an accessible proposition.", auth.ScopeRead, reads("List recently deleted items"), func(ctx context.Context, a core.Actor, in propositionArgs) ([]core.TrashItem, error) {
		return s.api.Board.Trash(ctx, a, in.Proposition)
	})
	workflowTool(s, "restore_deleted", "Restores one recently deleted item without overwriting existing content.", auth.ScopeWrite, adds("Restore deleted content"), func(ctx context.Context, a core.Actor, in restoreArgs) (core.Event, error) {
		item, err := s.api.Board.DeletedItem(ctx, a, in.ID)
		if err != nil {
			return core.Event{}, err
		}
		if item.Entity == "file" {
			if _, err := principal(ctx, auth.ScopeFiles); err != nil {
				return core.Event{}, err
			}
		}
		return s.api.Board.RestoreDeleted(ctx, a, in.ID)
	})
	workflowTool(s, "list_calendar_entries", "Lists accessible dated tasks and Show calendar events.", auth.ScopeRead, reads("list calendar entries"), func(ctx context.Context, a core.Actor, in noArgs) ([]workflow.CalendarEntry, error) {
		return w.CalendarEntries(ctx, a)
	})
	workflowTool(s, "save_calendar_event", "Creates or updates an all-day Show event using its current version.", auth.ScopeWrite, changes("save calendar event"), func(ctx context.Context, a core.Actor, in calendarSave) (core.Event, error) {
		return w.SaveCalendarEvent(ctx, a, workflow.CalendarEntry{ID: in.ID, Version: in.Version, Title: in.Title, Date: in.Date, Notes: in.Notes})
	})
	workflowTool(s, "delete_calendar_event", "Deletes a Show event only if its version still matches.", auth.ScopeWrite, overwrites("delete calendar event"), func(ctx context.Context, a core.Actor, in workflowVersion) (core.Event, error) {
		return w.DeleteCalendarEvent(ctx, a, in.ID, in.Version)
	})
	workflowTool(s, "create_calendar_task", "Creates a dated Show task assigned to your account.", auth.ScopeWrite, adds("create calendar task"), func(ctx context.Context, a core.Actor, in calendarTask) (core.Event, error) {
		return s.api.Board.CreateCalendarTask(ctx, a, in.Column, in.Title, in.Date)
	})
	workflowTool(s, "get_production_plan", "Reads the owner, next action, blocker and production dates.", auth.ScopeRead, reads("get production plan"), func(ctx context.Context, a core.Actor, in propositionArgs) (board.ProductionPlan, error) {
		if err := w.Readable(ctx, a, in.Proposition); err != nil {
			return board.ProductionPlan{}, err
		}
		return board.GetProductionPlan(ctx, s.db, in.Proposition)
	})
	workflowTool(s, "save_production_plan", "Updates a production plan using its current version.", auth.ScopeWrite, overwrites("save production plan"), func(ctx context.Context, a core.Actor, in planSave) (core.Event, error) {
		return s.api.Board.SaveProductionPlan(ctx, a, in.Plan.Proposition, in.Plan)
	})
	workflowTool(s, "list_evidence", "Lists research references and the claims they support.", auth.ScopeRead, reads("list evidence"), func(ctx context.Context, a core.Actor, in propositionArgs) ([]workflow.Evidence, error) {
		return w.Evidence(ctx, a, in.Proposition)
	})
	workflowTool(s, "save_evidence", "Creates or updates a reference using its current version.", auth.ScopeWrite, changes("save evidence"), func(ctx context.Context, a core.Actor, in evidenceSave) (core.Event, error) {
		return w.SaveEvidence(ctx, a, in.Evidence)
	})
	workflowTool(s, "delete_evidence", "Deletes a reference only if its version still matches.", auth.ScopeWrite, overwrites("delete evidence"), func(ctx context.Context, a core.Actor, in workflowVersion) (core.Event, error) {
		return w.DeleteEvidence(ctx, a, in.ID, in.Version)
	})
	workflowTool(s, "export_evidence", "Exports accessible references as Markdown or RIS.", auth.ScopeRead, reads("export evidence"), func(ctx context.Context, a core.Actor, in evidenceExport) (string, error) {
		rows, err := w.Evidence(ctx, a, in.Proposition)
		if err != nil {
			return "", err
		}
		return workflow.ExportEvidence(rows, in.Format)
	})
	workflowTool(s, "list_snapshots", "Lists the pinned scripts for a document.", auth.ScopeRead, reads("list snapshots"), func(ctx context.Context, a core.Actor, in documentArgs) ([]workflow.Snapshot, error) {
		return w.Snapshots(ctx, a, in.Document)
	})
	workflowTool(s, "get_snapshot", "Reads an immutable pinned script and its recording cues.", auth.ScopeRead, reads("get snapshot"), func(ctx context.Context, a core.Actor, in workflowID) (workflow.Snapshot, error) {
		return w.Snapshot(ctx, a, in.ID)
	})
	workflowTool(s, "pin_script", "Pins the saved document with optional recording cues.", auth.ScopeWrite, adds("pin script"), func(ctx context.Context, a core.Actor, in snapshotPin) (core.Event, error) {
		return w.Pin(ctx, a, in.Document, in.Cues)
	})
	workflowTool(s, "list_reviews", "Lists version-specific review requests and decisions for a proposition.", auth.ScopeRead, reads("list reviews"), func(ctx context.Context, a core.Actor, in propositionArgs) ([]workflow.Review, error) {
		return w.Reviews(ctx, a, in.Proposition)
	})
	workflowTool(s, "request_review", "Requests a named reviewer for exactly one document or audio file.", auth.ScopeWrite, adds("request review"), func(ctx context.Context, a core.Actor, in reviewRequest) (core.Event, error) {
		return w.RequestReview(ctx, a, in.Document, in.File, in.Reviewer)
	})
	workflowTool(s, "decide_review", "Records a decision as the assigned reviewer using the current review version.", auth.ScopeWrite, overwrites("decide review"), func(ctx context.Context, a core.Actor, in reviewDecision) (core.Event, error) {
		return w.Decide(ctx, a, in.ID, in.Version, in.State, in.Note)
	})
	workflowTool(s, "get_transcript", "Reads a recording transcript with timestamps and speaker labels.", auth.ScopeRead, reads("get transcript"), func(ctx context.Context, a core.Actor, in fileArgs) (workflow.Transcript, error) {
		return w.Transcript(ctx, a, in.File)
	})
	workflowTool(s, "save_transcript", "Imports transcript text or replaces segments using the current transcript version.", auth.ScopeWrite, overwrites("save transcript"), func(ctx context.Context, a core.Actor, in transcriptSave) (core.Event, error) {
		if in.Format != "" {
			var err error
			in.Segments, err = workflow.ParseTranscript(in.Text, in.Format)
			if err != nil {
				return core.Event{}, err
			}
		}
		return w.SaveTranscript(ctx, a, workflow.Transcript{File: in.File, Version: in.Version, Segments: in.Segments})
	})
	workflowTool(s, "export_transcript", "Exports a recording transcript as TXT or VTT.", auth.ScopeRead, reads("export transcript"), func(ctx context.Context, a core.Actor, in transcriptExport) (string, error) {
		t, err := w.Transcript(ctx, a, in.File)
		if err != nil {
			return "", err
		}
		return workflow.ExportTranscript(t, in.Format)
	})
	workflowTool(s, "list_transcription_jobs", "Lists local transcription jobs for an accessible recording.", auth.ScopeRead, reads("list transcription jobs"), func(ctx context.Context, a core.Actor, in fileArgs) ([]workflow.Job, error) {
		return w.Jobs(ctx, a, in.File)
	})
	workflowTool(s, "queue_transcription", "Queues local Whisper transcription with optional separate-channel speaker labels.", auth.ScopeWrite, adds("queue transcription"), func(ctx context.Context, a core.Actor, in transcriptionQueue) (core.Event, error) {
		return w.QueueTranscription(ctx, a, in.File, in.Stereo)
	})
	workflowTool(s, "resolve_file_comment", "Resolves or reopens an audio comment using its current version.", auth.ScopeWrite, overwrites("resolve file comment"), func(ctx context.Context, a core.Actor, in commentResolve) (core.Event, error) {
		return s.api.Files.ResolveComment(ctx, a, in.File, in.Comment, in.Version, in.Resolved)
	})
}

func changes(title string) *sdk.ToolAnnotations {
	hints := overwrites(title)
	hints.IdempotentHint = false
	return hints
}
