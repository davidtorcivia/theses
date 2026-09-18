package notify

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/markdown"
)

// Who is how the people a match is for are found. The matcher never reads the
// database, so it names the question and the resolver asks it.
type Who int

const (
	// WhoUsers is the ids already in the event.
	WhoUsers Who = iota
	// WhoHandles is the accounts named by @handle.
	WhoHandles
	// WhoCardAssignees is everybody on the card, which is what "a card I am on"
	// means everywhere it appears.
	WhoCardAssignees
	// WhoMembers is every member of the proposition.
	WhoMembers
)

// A Notice is one notification an event produces before anybody has been named.
// Match is pure, so every row of the matrix is a table test over a synthetic
// event rather than a board and a database.
type Notice struct {
	Event string
	Who   Who
	Users []int64
	// Exclude is people the match is not for however they were resolved, which
	// is how an upload finished by the server misses the person who uploaded it.
	Exclude     []int64
	Handles     []string
	Card        int64
	Document    int64
	Proposition int64
	Entity      string
	EntityID    int64
	Title       string
	Text        string
}

// Match returns what one applied command is worth telling people about.
func Match(e core.Event) []Notice {
	who := actorName(e.Actor)
	switch e.Entity {
	case "card":
		return matchCard(e, who)
	case "comment":
		return matchComment(e, who)
	case "block":
		return matchBlock(e, who)
	case "proposition":
		return matchProposition(e, who)
	case "file":
		return matchFile(e, who)
	}
	return nil
}

// actorName is who the message says did it. The markdown watcher is not a
// person, and a message reading "  changed a block" helps nobody.
func actorName(a core.Actor) string {
	if a.Kind == core.KindFile {
		return "The markdown mirror"
	}
	if a.Name == "" {
		return "Somebody"
	}
	return a.Name
}

// The rows, keyed by column name, as core marshals them into an event.

type cardRow struct {
	ID          int64   `json:"id"`
	Proposition int64   `json:"proposition_id"`
	ColumnID    int64   `json:"column_id"`
	Title       string  `json:"title"`
	Description string  `json:"description_md"`
	DueDate     *string `json:"due_date"`
	DoneAt      *int64  `json:"done_at"`
	Assignees   []int64 `json:"assignees"`
}

type commentRow struct {
	ID     int64  `json:"id"`
	CardID int64  `json:"card_id"`
	UserID *int64 `json:"user_id"`
	Body   string `json:"body_md"`
}

type blockRow struct {
	ID        int64  `json:"id"`
	Document  int64  `json:"document_id"`
	Text      string `json:"text"`
	UpdatedBy *int64 `json:"updated_by"`
}

type propositionRow struct {
	ID     int64  `json:"id"`
	Number int64  `json:"number"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

type fileRow struct {
	ID          int64  `json:"id"`
	Proposition int64  `json:"proposition_id"`
	Name        string `json:"name"`
	UploadedBy  *int64 `json:"uploaded_by"`
}

// decode is every read of an event payload. A payload that is not the row it
// should be produces no notification rather than a panic: the matcher runs on
// whatever the bus carries.
func decode[T any](raw json.RawMessage) (T, bool) {
	var v T
	if len(raw) == 0 {
		return v, false
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, false
	}
	return v, true
}

func matchCard(e core.Event, who string) []Notice {
	after, ok := decode[cardRow](e.After)
	before, hadBefore := decode[cardRow](e.Before)
	if !ok {
		// A delete carries only the before, and nothing in the matrix fires on
		// a card that no longer exists.
		return nil
	}
	base := Notice{Card: after.ID, Proposition: after.Proposition, Entity: "card", EntityID: after.ID}
	quoted := strconv.Quote(after.Title)

	switch e.Action {
	case "create":
		if len(after.Assignees) == 0 {
			return nil
		}
		m := base
		m.Event, m.Who, m.Users = "assigned", WhoUsers, after.Assignees
		m.Title = "Assigned to you: " + after.Title
		m.Text = fmt.Sprintf("%s assigned you %s.", who, quoted)
		return append([]Notice{m}, mentions(base, "mentioned", who,
			"", after.Description, fmt.Sprintf("%s mentioned you on %s.", who, quoted))...)

	case "assign", "unassign":
		// The command names the card, not the person, so who joined or left is
		// the difference between the two lists.
		var joined, left []int64
		if hadBefore {
			joined, left = difference(after.Assignees, before.Assignees), difference(before.Assignees, after.Assignees)
		}
		var out []Notice
		if len(joined) > 0 {
			m := base
			m.Event, m.Who, m.Users = "assigned", WhoUsers, joined
			m.Title = "Assigned to you: " + after.Title
			m.Text = fmt.Sprintf("%s assigned you %s.", who, quoted)
			out = append(out, m)
		}
		if len(left) > 0 {
			m := base
			m.Event, m.Who, m.Users = "unassigned", WhoUsers, left
			m.Title = "No longer yours: " + after.Title
			m.Text = fmt.Sprintf("%s took you off %s.", who, quoted)
			out = append(out, m)
		}
		return out

	case "edit":
		// One action covers the title and the description, so what fires is
		// whichever handles the description gained.
		was := ""
		if hadBefore {
			was = before.Description
		}
		return mentions(base, "mentioned", who, was, after.Description,
			fmt.Sprintf("%s mentioned you on %s.", who, quoted))

	case "move":
		m := base
		m.Event, m.Who = "moved", WhoCardAssignees
		m.Title = "Moved: " + after.Title
		m.Text = fmt.Sprintf("%s moved %s.", who, quoted)
		return []Notice{m}

	case "done":
		m := base
		m.Event, m.Who = "done", WhoCardAssignees
		m.Title = "Done: " + after.Title
		m.Text = fmt.Sprintf("%s marked %s done.", who, quoted)
		return []Notice{m}
	}
	// reopen, due, question and the rest are on nobody's matrix.
	return nil
}

func matchComment(e core.Event, who string) []Notice {
	if e.Action != "create" {
		return nil
	}
	after, ok := decode[commentRow](e.After)
	if !ok {
		return nil
	}
	base := Notice{Card: after.CardID, Proposition: e.Proposition, Entity: "comment", EntityID: after.ID}
	note := base
	note.Event, note.Who = "note", WhoCardAssignees
	note.Title = "A note on your card"
	note.Text = fmt.Sprintf("%s wrote: %s", who, excerpt(after.Body))
	return append([]Notice{note}, mentions(base, "mentioned", who, "", after.Body,
		fmt.Sprintf("%s mentioned you: %s", who, excerpt(after.Body)))...)
}

func matchBlock(e core.Event, who string) []Notice {
	after, hasAfter := decode[blockRow](e.After)
	before, hasBefore := decode[blockRow](e.Before)
	row := after
	if !hasAfter {
		row = before
	}
	if !hasAfter && !hasBefore {
		return nil
	}
	base := Notice{Document: row.Document, Entity: "block", EntityID: row.ID, Proposition: e.Proposition}

	var out []Notice
	// The person being told is whoever wrote what was there, which is in the
	// before, and only when somebody else is the one who changed it.
	if (e.Action == "set" || e.Action == "delete") && hasBefore && before.UpdatedBy != nil &&
		!(e.Actor.Kind == core.KindUser && e.Actor.ID == *before.UpdatedBy) {
		m := base
		m.Event, m.Who, m.Users = "block", WhoUsers, []int64{*before.UpdatedBy}
		m.Title = "A block you wrote changed"
		m.Text = fmt.Sprintf("%s changed a block you wrote: %s", who, excerpt(before.Text))
		out = append(out, m)
	}
	if e.Action == "set" || e.Action == "insert" {
		was := ""
		if hasBefore {
			was = before.Text
		}
		out = append(out, mentions(base, "mentioned", who, was, after.Text,
			fmt.Sprintf("%s mentioned you in a document: %s", who, excerpt(after.Text)))...)
	}
	return out
}

func matchProposition(e core.Event, who string) []Notice {
	if e.Action != "status" {
		return nil
	}
	after, ok := decode[propositionRow](e.After)
	if !ok {
		return nil
	}
	before, hadBefore := decode[propositionRow](e.Before)
	if hadBefore && before.Status == after.Status {
		return nil
	}
	return []Notice{{
		Event: "status", Who: WhoMembers, Proposition: after.ID,
		Entity: "proposition", EntityID: after.ID,
		Title: after.Title + " is " + after.Status,
		Text:  fmt.Sprintf("%s moved %s to %s.", who, strconv.Quote(after.Title), after.Status),
	}}
}

func matchFile(e core.Event, who string) []Notice {
	if e.Action != "complete" {
		return nil
	}
	after, ok := decode[fileRow](e.After)
	if !ok {
		return nil
	}
	m := Notice{
		Event: "file", Who: WhoMembers, Proposition: after.Proposition,
		Entity: "file", EntityID: after.ID,
		Title: "New file: " + after.Name,
		Text:  fmt.Sprintf("%s uploaded %s.", who, strconv.Quote(after.Name)),
	}
	// Excluded as well as the actor, because an upload the server finishes is
	// not the uploader's own action and would otherwise reach them.
	if after.UploadedBy != nil {
		m.Exclude = []int64{*after.UploadedBy}
	}
	return []Notice{m}
}

// mentions is the one rule that reads text: the handles the new version names
// and the old one did not. Editing a line that already mentioned somebody does
// not mention them again.
func mentions(base Notice, event, who, was, now, text string) []Notice {
	fresh := markdown.Mentions(now)
	if len(fresh) == 0 {
		return nil
	}
	if was != "" {
		old := markdown.Mentions(was)
		fresh = slices.DeleteFunc(fresh, func(h string) bool {
			return slices.ContainsFunc(old, func(o string) bool { return strings.EqualFold(o, h) })
		})
	}
	if len(fresh) == 0 {
		return nil
	}
	m := base
	m.Event, m.Who, m.Handles = event, WhoHandles, fresh
	m.Title = who + " mentioned you"
	m.Text = text
	return []Notice{m}
}

// difference is everything in a that is not in b.
func difference(a, b []int64) []int64 {
	var out []int64
	for _, v := range a {
		if !slices.Contains(b, v) {
			out = append(out, v)
		}
	}
	return out
}

// excerpt is as much of a body as a push notification shows.
func excerpt(body string) string {
	plain := strings.Join(strings.Fields(markdown.Plain(body)), " ")
	if len([]rune(plain)) > 140 {
		return string([]rune(plain)[:139]) + "…"
	}
	return plain
}
