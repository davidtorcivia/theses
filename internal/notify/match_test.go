package notify

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/davidtorcivia/theses/internal/core"
)

func raw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// card is a board card as core marshals it into an event.
func card(t *testing.T, fields map[string]any) json.RawMessage {
	t.Helper()
	row := map[string]any{
		"id": 7, "proposition_id": 3, "column_id": 2, "title": "Fix the intro",
		"description_md": "", "assignees": []int64{},
	}
	for k, v := range fields {
		row[k] = v
	}
	return raw(t, row)
}

// One table per row of the matrix. Match is pure, so every rule is checked
// against a synthetic event rather than against a board.
func TestMatchRules(t *testing.T) {
	ada := core.Actor{Kind: core.KindUser, ID: 1, Name: "Ada Lovelace"}
	tests := []struct {
		name     string
		event    core.Event
		want     []string // the rule keys, in order
		wantWho  []Who
		wantUser []int64 // the ids of the first match, when it names them
	}{
		{
			name: "assign names the person who joined, not the card",
			event: core.Event{Entity: "card", Action: "assign", Actor: ada,
				Before: card(t, map[string]any{"assignees": []int64{4}}),
				After:  card(t, map[string]any{"assignees": []int64{4, 5}})},
			want: []string{"assigned"}, wantWho: []Who{WhoUsers}, wantUser: []int64{5},
		},
		{
			name: "unassign names the person who left",
			event: core.Event{Entity: "card", Action: "unassign", Actor: ada,
				Before: card(t, map[string]any{"assignees": []int64{4, 5}}),
				After:  card(t, map[string]any{"assignees": []int64{4}})},
			want: []string{"unassigned"}, wantWho: []Who{WhoUsers}, wantUser: []int64{5},
		},
		{
			name: "assign that changed nobody notifies nobody",
			event: core.Event{Entity: "card", Action: "assign", Actor: ada,
				Before: card(t, map[string]any{"assignees": []int64{4}}),
				After:  card(t, map[string]any{"assignees": []int64{4}})},
			want: nil,
		},
		{
			name: "creating a card with assignees is an assignment",
			event: core.Event{Entity: "card", Action: "create", Actor: ada,
				After: card(t, map[string]any{"assignees": []int64{5}})},
			want: []string{"assigned"}, wantWho: []Who{WhoUsers}, wantUser: []int64{5},
		},
		{
			name: "creating a card with nobody on it notifies nobody",
			event: core.Event{Entity: "card", Action: "create", Actor: ada,
				After: card(t, nil)},
			want: nil,
		},
		{
			name:  "a move goes to everybody on the card",
			event: core.Event{Entity: "card", Action: "move", Actor: ada, After: card(t, nil)},
			want:  []string{"moved"}, wantWho: []Who{WhoCardAssignees},
		},
		{
			name:  "done fires and reopen does not",
			event: core.Event{Entity: "card", Action: "done", Actor: ada, After: card(t, nil)},
			want:  []string{"done"}, wantWho: []Who{WhoCardAssignees},
		},
		{
			name:  "reopening a card is nobody's business",
			event: core.Event{Entity: "card", Action: "reopen", Actor: ada, After: card(t, nil)},
			want:  nil,
		},
		{
			name: "a mention in a description fires once for the new handle",
			event: core.Event{Entity: "card", Action: "edit", Actor: ada,
				Before: card(t, map[string]any{"description_md": "hello @grace"}),
				After:  card(t, map[string]any{"description_md": "hello @grace and @alan"})},
			want: []string{"mentioned"}, wantWho: []Who{WhoHandles},
		},
		{
			name: "editing a description that already named somebody names them again to nobody",
			event: core.Event{Entity: "card", Action: "edit", Actor: ada,
				Before: card(t, map[string]any{"description_md": "hello @grace"}),
				After:  card(t, map[string]any{"description_md": "hello @grace, again"})},
			want: nil,
		},
		{
			name: "a note reaches the card and the handles in it",
			event: core.Event{Entity: "comment", Action: "create", Actor: ada, Proposition: 3,
				After: raw(t, map[string]any{"id": 9, "card_id": 7, "user_id": 1, "body_md": "look at this @grace"})},
			want: []string{"note", "mentioned"}, wantWho: []Who{WhoCardAssignees, WhoHandles},
		},
		{
			name: "a block set tells whoever wrote what was there",
			event: core.Event{Entity: "block", Action: "set", Actor: ada, Proposition: 3,
				Before: raw(t, map[string]any{"id": 2, "document_id": 4, "text": "was", "updated_by": 5}),
				After:  raw(t, map[string]any{"id": 2, "document_id": 4, "text": "now", "updated_by": 1})},
			want: []string{"block"}, wantWho: []Who{WhoUsers}, wantUser: []int64{5},
		},
		{
			name: "changing your own block tells you nothing",
			event: core.Event{Entity: "block", Action: "set", Actor: ada, Proposition: 3,
				Before: raw(t, map[string]any{"id": 2, "document_id": 4, "text": "was", "updated_by": 1}),
				After:  raw(t, map[string]any{"id": 2, "document_id": 4, "text": "now", "updated_by": 1})},
			want: nil,
		},
		{
			name: "the markdown watcher changing a block still tells its writer",
			event: core.Event{Entity: "block", Action: "set", Actor: core.Actor{Kind: core.KindFile},
				Before: raw(t, map[string]any{"id": 2, "document_id": 4, "text": "was", "updated_by": 1}),
				After:  raw(t, map[string]any{"id": 2, "document_id": 4, "text": "now", "updated_by": 1})},
			want: []string{"block"}, wantWho: []Who{WhoUsers}, wantUser: []int64{1},
		},
		{
			name: "a mention inside a block reaches the handle",
			event: core.Event{Entity: "block", Action: "insert", Actor: ada,
				After: raw(t, map[string]any{"id": 2, "document_id": 4, "text": "ask @grace"})},
			want: []string{"mentioned"}, wantWho: []Who{WhoHandles},
		},
		{
			name: "a status change reaches the members",
			event: core.Event{Entity: "proposition", Action: "status", Actor: ada,
				Before: raw(t, map[string]any{"id": 3, "title": "Ten", "status": "idea"}),
				After:  raw(t, map[string]any{"id": 3, "title": "Ten", "status": "recording"})},
			want: []string{"status"}, wantWho: []Who{WhoMembers},
		},
		{
			name: "a status that did not change is not a change",
			event: core.Event{Entity: "proposition", Action: "status", Actor: ada,
				Before: raw(t, map[string]any{"id": 3, "title": "Ten", "status": "idea"}),
				After:  raw(t, map[string]any{"id": 3, "title": "Ten", "status": "idea"})},
			want: nil,
		},
		{
			name: "a finished upload reaches the members",
			event: core.Event{Entity: "file", Action: "complete", Actor: ada,
				After: raw(t, map[string]any{"id": 11, "proposition_id": 3, "name": "take one.wav", "uploaded_by": 5})},
			want: []string{"file"}, wantWho: []Who{WhoMembers},
		},
		{
			name: "an upload that only started reaches nobody",
			event: core.Event{Entity: "file", Action: "create", Actor: ada,
				After: raw(t, map[string]any{"id": 11, "proposition_id": 3, "name": "take one.wav"})},
			want: nil,
		},
		{
			name: "a link is on nobody's matrix",
			event: core.Event{Entity: "link", Action: "create", Actor: ada,
				After: raw(t, map[string]any{"id": 1, "proposition_id": 3})},
			want: nil,
		},
		{
			name:  "a payload that is not a row produces nothing rather than panicking",
			event: core.Event{Entity: "card", Action: "move", Actor: ada, After: json.RawMessage(`"not a row"`)},
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Match(tt.event)
			keys := make([]string, 0, len(got))
			for _, m := range got {
				keys = append(keys, m.Event)
			}
			if !slices.Equal(keys, tt.want) {
				t.Fatalf("events = %v, want %v", keys, tt.want)
			}
			for i, who := range tt.wantWho {
				if got[i].Who != who {
					t.Errorf("match %d who = %d, want %d", i, got[i].Who, who)
				}
			}
			if tt.wantUser != nil && !slices.Equal(got[0].Users, tt.wantUser) {
				t.Errorf("users = %v, want %v", got[0].Users, tt.wantUser)
			}
			for _, m := range got {
				if m.Title == "" || m.Text == "" {
					t.Errorf("%s has no message: %+v", m.Event, m)
				}
			}
		})
	}
}

// Every event the matcher can produce has to be a row of the matrix, or a rule
// nobody can tick would be the only thing standing between it and everybody.
func TestMatchOnlyProducesKnownEvents(t *testing.T) {
	ada := core.Actor{Kind: core.KindUser, ID: 1, Name: "Ada Lovelace"}
	events := []core.Event{
		{Entity: "card", Action: "assign", Actor: ada,
			Before: card(t, nil), After: card(t, map[string]any{"assignees": []int64{5}})},
		{Entity: "card", Action: "move", Actor: ada, After: card(t, nil)},
		{Entity: "card", Action: "done", Actor: ada, After: card(t, nil)},
		{Entity: "comment", Action: "create", Actor: ada,
			After: raw(t, map[string]any{"id": 1, "card_id": 7, "body_md": "@grace"})},
		{Entity: "block", Action: "set", Actor: ada,
			Before: raw(t, map[string]any{"id": 1, "document_id": 2, "updated_by": 9}),
			After:  raw(t, map[string]any{"id": 1, "document_id": 2, "text": "@grace"})},
		{Entity: "proposition", Action: "status", Actor: ada,
			After: raw(t, map[string]any{"id": 3, "title": "Ten", "status": "editing"})},
		{Entity: "file", Action: "complete", Actor: ada,
			After: raw(t, map[string]any{"id": 4, "proposition_id": 3, "name": "a.wav"})},
	}
	for _, e := range events {
		for _, m := range Match(e) {
			if _, ok := LookupEvent(m.Event); !ok {
				t.Errorf("%s/%s produced %q, which is not a row of the matrix", e.Entity, e.Action, m.Event)
			}
		}
	}
}

func TestExcerptIsShortAndPlain(t *testing.T) {
	long := ""
	for range 40 {
		long += "word "
	}
	if got := excerpt(long); len([]rune(got)) != 140 {
		t.Errorf("excerpt is %d runes, want 140", len([]rune(got)))
	}
	if got := excerpt("**bold** and @grace"); got != "bold and @grace" {
		t.Errorf("excerpt = %q", got)
	}
}
