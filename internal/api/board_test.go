package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/store"
)

// boardHarness is one proposition with its column and one card on it, plus a
// person who is a member of nothing.
type boardHarness struct {
	*harness
	prop   int64
	column int64
	card   int64
	// stranger is an editor with a token of every scope and no membership.
	stranger *store.User
}

func newBoardHarness(t *testing.T) *boardHarness {
	t.Helper()
	ctx := context.Background()
	h := newHarness(t)
	owner := core.Actor{Kind: core.KindUser, ID: h.user.ID, Name: h.user.Name}

	e, err := h.board.CreateProposition(ctx, owner, "Tidal Power")
	if err != nil {
		t.Fatal(err)
	}
	cols, err := board.ListColumns(ctx, h.db, e.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	card, err := h.board.CreateCard(ctx, owner, cols[0].ID, "Read the tide tables", nil)
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
	return &boardHarness{harness: h, prop: e.EntityID, column: cols[0].ID,
		card: card.EntityID, stranger: stranger}
}

func TestShowIsAvailableAtItsStableRESTEndpoint(t *testing.T) {
	h := newHarness(t)
	want, err := board.EnsureShow(context.Background(), h.db)
	if err != nil {
		t.Fatal(err)
	}
	w := h.do("GET", "/api/v1/show", h.token(auth.ScopeRead), "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET show gave %d: %s", w.Code, w.Body)
	}
	var got struct {
		Proposition board.Proposition `json:"proposition"`
	}
	into(t, w, &got)
	if got.Proposition.ID != want.ID || got.Proposition.Kind != "show" || got.Proposition.Number != 0 {
		t.Fatalf("show = %+v", got.Proposition)
	}
}

// tokenFor is a token belonging to somebody other than the harness owner.
func (h *boardHarness) tokenFor(user *store.User, scopes ...string) string {
	h.Helper()
	clear, err := h.auth.CreateAPIToken(context.Background(), user.ID, user.Handle, scopes)
	if err != nil {
		h.Fatal(err)
	}
	return clear
}

func (h *boardHarness) event(w *httptest.ResponseRecorder) core.Event {
	h.Helper()
	var body struct {
		Event core.Event `json:"event"`
	}
	into(h.T, w, &body)
	return body.Event
}

// The board over REST is the board over the socket: a proposition, a column, a
// card, a checklist item and a note, each read back the way the event left it.
func TestTheBoardRoundTripsOverREST(t *testing.T) {
	h := newBoardHarness(t)
	token := h.token(auth.ScopeRead, auth.ScopeWrite)
	prop := strconv.FormatInt(h.prop, 10)
	card := strconv.FormatInt(h.card, 10)

	w := h.do("POST", "/api/v1/propositions", token, `{"title":"Deep Water"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("creating a proposition gave %d: %s", w.Code, w.Body)
	}
	second := h.event(w).EntityID

	if w = h.do("GET", "/api/v1/propositions", token, ""); w.Code != http.StatusOK {
		t.Fatalf("listing gave %d: %s", w.Code, w.Body)
	}
	var list struct {
		Propositions []board.Proposition `json:"propositions"`
	}
	into(t, w, &list)
	if len(list.Propositions) != 2 {
		t.Fatalf("propositions = %+v", list.Propositions)
	}

	// A patch names what it changes and leaves the rest as it was.
	if w = h.do("PATCH", "/api/v1/propositions/"+prop, token,
		`{"statement":"The tide is a battery.","status":"recording","episode":"12"}`); w.Code != http.StatusOK {
		t.Fatalf("patching gave %d: %s", w.Code, w.Body)
	}
	w = h.do("GET", "/api/v1/propositions/"+prop, token, "")
	var one struct {
		Proposition board.Proposition `json:"proposition"`
	}
	into(t, w, &one)
	if one.Proposition.Title != "Tidal Power" || one.Proposition.Statement != "The tide is a battery." ||
		one.Proposition.Status != "recording" || one.Proposition.Episode == nil || *one.Proposition.Episode != "12" {
		t.Fatalf("proposition = %+v", one.Proposition)
	}

	// The rail order, and a column of the caller's own.
	if w = h.do("POST", "/api/v1/propositions/"+prop+"/move", token,
		`{"after":`+strconv.FormatInt(second, 10)+`}`); w.Code != http.StatusOK {
		t.Fatalf("moving gave %d: %s", w.Code, w.Body)
	}
	if w = h.do("POST", "/api/v1/propositions/"+prop+"/columns", token, `{"name":"Cutting"}`); w.Code != http.StatusOK {
		t.Fatalf("creating a column gave %d: %s", w.Code, w.Body)
	}
	column := strconv.FormatInt(h.event(w).EntityID, 10)
	if w = h.do("PATCH", "/api/v1/columns/"+column, token, `{"name":"Cut"}`); w.Code != http.StatusOK {
		t.Fatalf("renaming gave %d: %s", w.Code, w.Body)
	}
	if w = h.do("GET", "/api/v1/propositions/"+prop+"/columns", token, ""); w.Code != http.StatusOK {
		t.Fatalf("listing columns gave %d: %s", w.Code, w.Body)
	}
	var columns struct {
		Columns []board.Column `json:"columns"`
	}
	into(t, w, &columns)
	if len(columns.Columns) != 2 || columns.Columns[1].Name != "Cut" {
		t.Fatalf("columns = %+v", columns.Columns)
	}

	// A card, moved into it, assigned, checked off and noted on.
	if w = h.do("POST", "/api/v1/cards/"+card+"/move", token,
		`{"column":`+column+`}`); w.Code != http.StatusOK {
		t.Fatalf("moving a card gave %d: %s", w.Code, w.Body)
	}
	if w = h.do("POST", "/api/v1/cards/"+card+"/assignees/"+strconv.FormatInt(h.user.ID, 10),
		token, ""); w.Code != http.StatusOK {
		t.Fatalf("assigning gave %d: %s", w.Code, w.Body)
	}
	if w = h.do("POST", "/api/v1/cards/"+card+"/done", token, ""); w.Code != http.StatusOK {
		t.Fatalf("done gave %d: %s", w.Code, w.Body)
	}
	if w = h.do("POST", "/api/v1/cards/"+card+"/checklist", token, `{"text":"Ring the harbor"}`); w.Code != http.StatusOK {
		t.Fatalf("a checklist item gave %d: %s", w.Code, w.Body)
	}
	item := strconv.FormatInt(h.event(w).EntityID, 10)
	if w = h.do("PATCH", "/api/v1/checklist/"+item, token, `{"done":true}`); w.Code != http.StatusOK {
		t.Fatalf("ticking gave %d: %s", w.Code, w.Body)
	}
	if w = h.do("PATCH", "/api/v1/cards/"+card, token,
		`{"due_date":"2026-10-01","question":"II"}`); w.Code != http.StatusOK {
		t.Fatalf("a due date and a question gave %d: %s", w.Code, w.Body)
	}
	if w = h.do("POST", "/api/v1/cards/"+card+"/comments", token, `{"body_md":"They answered."}`); w.Code != http.StatusOK {
		t.Fatalf("a note gave %d: %s", w.Code, w.Body)
	}
	note := strconv.FormatInt(h.event(w).EntityID, 10)

	w = h.do("GET", "/api/v1/cards/"+card, token, "")
	var got struct {
		Card board.Card `json:"card"`
	}
	into(t, w, &got)
	if strconv.FormatInt(got.Card.ColumnID, 10) != column || got.Card.DoneAt == nil {
		t.Fatalf("card = %+v", got.Card)
	}
	if len(got.Card.Assignees) != 1 || len(got.Card.Comments) != 1 ||
		len(got.Card.Checklist) != 1 || !got.Card.Checklist[0].Done {
		t.Fatalf("card = %+v", got.Card)
	}
	if got.Card.DueDate == nil || *got.Card.DueDate != "2026-10-01" ||
		got.Card.Question == nil || *got.Card.Question != "II" {
		t.Fatalf("card = %+v", got.Card)
	}

	// The list by proposition carries the sequence number the stream continues
	// from, so a client can read the board and then follow along.
	w = h.do("GET", "/api/v1/propositions/"+prop+"/cards", token, "")
	var cards struct {
		Cards []board.Card `json:"cards"`
		Seq   int64        `json:"seq"`
	}
	into(t, w, &cards)
	if len(cards.Cards) != 1 || cards.Seq == 0 {
		t.Fatalf("cards = %+v seq = %d", cards.Cards, cards.Seq)
	}

	// And back out again.
	if w = h.do("POST", "/api/v1/cards/"+card+"/reopen", token, ""); w.Code != http.StatusOK {
		t.Fatalf("reopen gave %d: %s", w.Code, w.Body)
	}
	for _, target := range []string{
		"/api/v1/cards/" + card + "/assignees/" + strconv.FormatInt(h.user.ID, 10),
		"/api/v1/checklist/" + item,
		"/api/v1/comments/" + note,
		"/api/v1/cards/" + card,
		"/api/v1/columns/" + column,
	} {
		if w = h.do("DELETE", target, token, ""); w.Code != http.StatusOK {
			t.Fatalf("DELETE %s gave %d: %s", target, w.Code, w.Body)
		}
	}
}

// A card has one version across its title and its description, so an edit that
// began before somebody else's is refused with the text that is there now, and
// an edit naming both fields sends the second command the version the first one
// left.
func TestAStaleCardEditIsRefusedWithWhatIsThere(t *testing.T) {
	h := newBoardHarness(t)
	token := h.token(auth.ScopeRead, auth.ScopeWrite)
	card := strconv.FormatInt(h.card, 10)

	var got struct {
		Card board.Card `json:"card"`
	}
	into(t, h.do("GET", "/api/v1/cards/"+card, token, ""), &got)
	base := strconv.FormatInt(got.Card.Version, 10)

	w := h.do("PATCH", "/api/v1/cards/"+card, token,
		`{"base_version":`+base+`,"title":"Read the tables","description_md":"Both of them."}`)
	if w.Code != http.StatusOK {
		t.Fatalf("an edit naming both fields gave %d: %s", w.Code, w.Body)
	}
	after := h.do("GET", "/api/v1/cards/"+card, token, "")
	into(t, after, &got)
	if got.Card.Title != "Read the tables" || got.Card.Description != "Both of them." {
		t.Fatalf("card = %+v", got.Card)
	}

	w = h.do("PATCH", "/api/v1/cards/"+card, token, `{"base_version":`+base+`,"title":"Too late"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("a stale edit gave %d: %s", w.Code, w.Body)
	}
	var refused struct {
		Error    string              `json:"error"`
		Conflict *core.ConflictError `json:"conflict"`
	}
	into(t, w, &refused)
	if refused.Conflict == nil || refused.Conflict.Field != "title" ||
		refused.Conflict.Current != "Read the tables" || refused.Conflict.Version != got.Card.Version {
		t.Fatalf("conflict = %+v", refused.Conflict)
	}

	// Nothing of the refused body landed, because the two commands are one
	// transaction.
	after = h.do("GET", "/api/v1/cards/"+card, token, "")
	into(t, after, &got)
	if got.Card.Title != "Read the tables" {
		t.Fatalf("a refused edit landed: %+v", got.Card)
	}
}

// Undo over the API is the undo the socket offers, refused by the same rules.
func TestUndoOverTheAPIIsRefusedTheSameWay(t *testing.T) {
	ctx := context.Background()
	h := newBoardHarness(t)
	token := h.token(auth.ScopeRead, auth.ScopeWrite)
	owner := core.Actor{Kind: core.KindUser, ID: h.user.ID, Name: h.user.Name}

	created, err := h.board.CreateCard(ctx, owner, h.column, "Call the harbor", nil)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := board.GetCard(ctx, h.db, created.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	edited, err := h.board.EditCardTitle(ctx, owner, created.EntityID, fresh.Version, "Call the harbor master")
	if err != nil {
		t.Fatal(err)
	}
	undo := func(seq int64) *httptest.ResponseRecorder {
		return h.do("POST", fmt.Sprintf("/api/v1/activity/%d/undo", seq), token, "")
	}

	if w := undo(created.Seq); w.Code != http.StatusConflict {
		t.Errorf("undoing a create gave %d: %s", w.Code, w.Body)
	}
	w := undo(edited.Seq)
	if w.Code != http.StatusOK {
		t.Fatalf("undoing an edit gave %d: %s", w.Code, w.Body)
	}
	back, err := board.GetCard(ctx, h.db, created.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	if back.Title != "Call the harbor" {
		t.Errorf("title = %q", back.Title)
	}
	if w := undo(edited.Seq); w.Code != http.StatusConflict {
		t.Errorf("undoing the same row twice gave %d: %s", w.Code, w.Body)
	}
	if w := undo(9999); w.Code != http.StatusNotFound {
		t.Errorf("undoing a row that is not there gave %d: %s", w.Code, w.Body)
	}

	// An archived proposition is read only, and undo is a write.
	again, err := h.board.EditCardTitle(ctx, owner, created.EntityID, back.Version, "Call the pilot")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.board.ArchiveProposition(ctx, owner, h.prop); err != nil {
		t.Fatal(err)
	}
	if w := undo(again.Seq); w.Code != http.StatusConflict {
		t.Errorf("undoing on an archived proposition gave %d: %s", w.Code, w.Body)
	}

	// Somebody who is not a member is told the row is not there rather than
	// that they may not undo it, and a row that cannot be undone answers the
	// same way: 409 for one of those and 404 for the rest would say which
	// activity ids are real and what they are about.
	if _, err := h.board.RestoreProposition(ctx, owner, h.prop); err != nil {
		t.Fatal(err)
	}
	stranger := h.tokenFor(h.stranger, auth.ScopeRead, auth.ScopeWrite)
	for _, seq := range []int64{again.Seq, created.Seq, 9999} {
		if w := h.do("POST", fmt.Sprintf("/api/v1/activity/%d/undo", seq), stranger, ""); w.Code != http.StatusNotFound {
			t.Errorf("a member of nothing was told about activity %d: %d %s", seq, w.Code, w.Body)
		}
	}
	// And the refused undo wrote no activity row of its own.
	var rows int
	if err := h.db.QueryRowContext(ctx,
		`SELECT count(*) FROM activity WHERE action = 'undo' AND actor_id = ?`,
		strconv.FormatInt(h.stranger.ID, 10)).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("a refused undo left %d rows behind", rows)
	}
}

// Every board route asks the same three questions: does this token have the
// scope, is its owner a member, and is the proposition still open.
func TestBoardRoutesAreScopedAndAuthorised(t *testing.T) {
	ctx := context.Background()
	h := newBoardHarness(t)
	prop := strconv.FormatInt(h.prop, 10)
	card := strconv.FormatInt(h.card, 10)
	column := strconv.FormatInt(h.column, 10)
	both := h.token(auth.ScopeRead, auth.ScopeWrite)
	stranger := h.tokenFor(h.stranger, auth.ScopeRead, auth.ScopeWrite)

	// A second proposition the first one's ids do not belong to.
	other, err := h.board.CreateProposition(ctx,
		core.Actor{Kind: core.KindUser, ID: h.user.ID, Name: h.user.Name}, "Deep Water")
	if err != nil {
		t.Fatal(err)
	}
	otherColumns, err := board.ListColumns(ctx, h.db, other.EntityID)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, method, target, body, token string
		want                              int
	}{
		{
			name: "a read token cannot create a proposition", method: "POST",
			target: "/api/v1/propositions", body: `{"title":"Deep Water"}`,
			token: h.token(auth.ScopeRead), want: http.StatusForbidden,
		},
		{
			name: "a write token cannot read the board", method: "GET",
			target: "/api/v1/propositions/" + prop + "/cards",
			token:  h.token(auth.ScopeWrite), want: http.StatusForbidden,
		},
		{
			name: "a member of nothing is told the proposition is not there", method: "GET",
			target: "/api/v1/propositions/" + prop, token: stranger, want: http.StatusNotFound,
		},
		{
			name: "nor the card on it", method: "GET",
			target: "/api/v1/cards/" + card, token: stranger, want: http.StatusNotFound,
		},
		{
			name: "nor its columns", method: "GET",
			target: "/api/v1/propositions/" + prop + "/columns", token: stranger, want: http.StatusNotFound,
		},
		{
			name: "and cannot write to it either", method: "POST",
			target: "/api/v1/cards/" + card + "/comments", body: `{"body_md":"Hello"}`,
			token: stranger, want: http.StatusNotFound,
		},
		{
			name: "a card cannot move to a column on another proposition", method: "POST",
			target: "/api/v1/cards/" + card + "/move",
			body:   `{"column":` + strconv.FormatInt(otherColumns[0].ID, 10) + `}`,
			token:  both, want: http.StatusNotFound,
		},
		{
			name: "a card cannot be created in a column that is not there", method: "POST",
			target: "/api/v1/columns/9999/cards", body: `{"title":"Nowhere"}`,
			token: both, want: http.StatusNotFound,
		},
		{
			name: "a title with nothing in it is refused", method: "POST",
			target: "/api/v1/columns/" + column + "/cards", body: `{"title":"   "}`,
			token: both, want: http.StatusUnprocessableEntity,
		},
		{
			name: "so is a question that is not one of the four", method: "PATCH",
			target: "/api/v1/cards/" + card, body: `{"question":"V"}`,
			token: both, want: http.StatusUnprocessableEntity,
		},
		{
			name: "a patch that names nothing is the caller's mistake", method: "PATCH",
			target: "/api/v1/cards/" + card, body: `{}`,
			token: both, want: http.StatusBadRequest,
		},
		{
			name: "a checklist item cannot be ticked without saying which way", method: "PATCH",
			target: "/api/v1/checklist/1", body: `{}`,
			token: both, want: http.StatusBadRequest,
		},
		{
			name: "a column with cards in it is not deleted by surprise", method: "DELETE",
			target: "/api/v1/columns/" + column, token: both, want: http.StatusConflict,
		},
		{
			name: "an id that is not a number is not found", method: "GET",
			target: "/api/v1/cards/x", token: both, want: http.StatusNotFound,
		},
		{
			name: "a move that names no column is the caller's mistake", method: "POST",
			target: "/api/v1/cards/" + card + "/move", body: `{"after":0}`,
			token: both, want: http.StatusBadRequest,
		},
		{
			name: "a status the workspace does not have is refused", method: "PATCH",
			target: "/api/v1/propositions/" + prop, body: `{"status":"shipped"}`,
			token: both, want: http.StatusUnprocessableEntity,
		},
		{
			name: "a member of nothing cannot add somebody to a proposition", method: "POST",
			target: "/api/v1/propositions/" + prop + "/members/" +
				strconv.FormatInt(h.user.ID, 10),
			token: stranger, want: http.StatusNotFound,
		},
		{
			name: "nor delete it", method: "DELETE",
			target: "/api/v1/propositions/" + prop, token: stranger, want: http.StatusNotFound,
		},
		{
			name: "somebody who is not in the workspace cannot be added", method: "POST",
			target: "/api/v1/propositions/" + prop + "/members/9999",
			token:  both, want: http.StatusNotFound,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if w := h.do(tc.method, tc.target, tc.token, tc.body); w.Code != tc.want {
				t.Fatalf("%s %s: %d, want %d: %s", tc.method, tc.target, w.Code, tc.want, w.Body)
			}
		})
	}

	// The rail is filtered rather than refused: a member of nothing sees an
	// empty one.
	w := h.do("GET", "/api/v1/propositions", stranger, "")
	var list struct {
		Propositions []board.Proposition `json:"propositions"`
	}
	into(t, w, &list)
	if w.Code != http.StatusOK || len(list.Propositions) != 0 {
		t.Fatalf("a member of nothing read %d propositions: %s", len(list.Propositions), w.Body)
	}
}

// An archived proposition is read only. Restoring it is the one write that
// still means something on one.
func TestAnArchivedPropositionIsReadOnlyOverREST(t *testing.T) {
	ctx := context.Background()
	h := newBoardHarness(t)
	token := h.token(auth.ScopeRead, auth.ScopeWrite)
	prop := strconv.FormatInt(h.prop, 10)
	card := strconv.FormatInt(h.card, 10)

	if w := h.do("POST", "/api/v1/propositions/"+prop+"/archive", token, ""); w.Code != http.StatusOK {
		t.Fatalf("archiving gave %d: %s", w.Code, w.Body)
	}
	for _, tc := range []struct{ method, target, body string }{
		{"PATCH", "/api/v1/cards/" + card, `{"title":"Anything"}`},
		{"POST", "/api/v1/cards/" + card + "/comments", `{"body_md":"Anything"}`},
		{"POST", "/api/v1/propositions/" + prop + "/columns", `{"name":"Anything"}`},
		{"POST", "/api/v1/cards/" + card + "/done", ""},
	} {
		w := h.do(tc.method, tc.target, token, tc.body)
		if w.Code != http.StatusConflict {
			t.Errorf("%s %s gave %d: %s", tc.method, tc.target, w.Code, w.Body)
		}
	}
	// Reading still works, and so does restoring.
	if w := h.do("GET", "/api/v1/propositions/"+prop+"/cards", token, ""); w.Code != http.StatusOK {
		t.Errorf("reading an archived board gave %d: %s", w.Code, w.Body)
	}
	if w := h.do("POST", "/api/v1/propositions/"+prop+"/restore", token, ""); w.Code != http.StatusOK {
		t.Errorf("restoring gave %d: %s", w.Code, w.Body)
	}
	if _, err := board.GetProposition(ctx, h.db, h.prop); err != nil {
		t.Fatal(err)
	}
}

// A note is the one row a role cannot delete on somebody else's behalf: the
// activity panel is a record, not a wall to moderate.
func TestANoteIsOnlyItsAuthorsToDelete(t *testing.T) {
	ctx := context.Background()
	h := newBoardHarness(t)
	owner := core.Actor{Kind: core.KindUser, ID: h.user.ID, Name: h.user.Name}
	if _, err := h.board.AddMember(ctx, owner, h.prop, h.stranger.ID); err != nil {
		t.Fatal(err)
	}
	e, err := h.board.PostComment(ctx, owner, h.card, "Mine.")
	if err != nil {
		t.Fatal(err)
	}
	w := h.do("DELETE", fmt.Sprintf("/api/v1/comments/%d", e.EntityID),
		h.tokenFor(h.stranger, auth.ScopeRead, auth.ScopeWrite), "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("somebody deleted a note that is not theirs: %d %s", w.Code, w.Body)
	}
}

// Every write through a token is the token owner's, carried by the token, and
// the row it left says so.
func TestABoardWriteThroughATokenIsAttributed(t *testing.T) {
	h := newBoardHarness(t)
	w := h.do("POST", "/api/v1/cards/"+strconv.FormatInt(h.card, 10)+"/comments",
		h.token(auth.ScopeRead, auth.ScopeWrite), `{"body_md":"From a script."}`)
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	e := h.event(w)
	if e.Actor.ID != h.user.ID || e.Actor.Via != TokenVia("read-write") {
		t.Fatalf("actor = %+v", e.Actor)
	}
	var note board.Comment
	if err := json.Unmarshal(e.After, &note); err != nil || note.Body != "From a script." {
		t.Fatalf("after = %s: %v", e.After, err)
	}
}

// Membership and the delete a proposition has are routes of their own, so the
// API can do what the socket and the page can.
func TestMembershipAndDeleteOverREST(t *testing.T) {
	ctx := context.Background()
	h := newBoardHarness(t)
	token := h.token(auth.ScopeRead, auth.ScopeWrite)
	prop := strconv.FormatInt(h.prop, 10)
	who := strconv.FormatInt(h.stranger.ID, 10)

	if w := h.do("POST", "/api/v1/propositions/"+prop+"/members/"+who, token, ""); w.Code != http.StatusOK {
		t.Fatalf("adding a member gave %d: %s", w.Code, w.Body)
	}
	one, err := board.GetProposition(ctx, h.db, h.prop)
	if err != nil {
		t.Fatal(err)
	}
	if len(one.Members) != 2 {
		t.Fatalf("members = %v", one.Members)
	}
	// Now a member, they read it.
	theirs := h.tokenFor(h.stranger, auth.ScopeRead, auth.ScopeWrite)
	if w := h.do("GET", "/api/v1/propositions/"+prop, theirs, ""); w.Code != http.StatusOK {
		t.Fatalf("a member could not read the proposition: %d %s", w.Code, w.Body)
	}

	if w := h.do("DELETE", "/api/v1/propositions/"+prop+"/members/"+who, token, ""); w.Code != http.StatusOK {
		t.Fatalf("removing a member gave %d: %s", w.Code, w.Body)
	}
	if w := h.do("GET", "/api/v1/propositions/"+prop, theirs, ""); w.Code != http.StatusNotFound {
		t.Fatalf("somebody taken off still reads it: %d %s", w.Code, w.Body)
	}

	// Taking off somebody who is not on is nothing happening, and it says so
	// rather than writing a row and telling every board watching that it did.
	var before int
	if err := h.db.QueryRowContext(ctx,
		`SELECT count(*) FROM activity WHERE entity = 'member'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{who, "9999"} {
		if w := h.do("DELETE", "/api/v1/propositions/"+prop+"/members/"+id, token, ""); w.Code != http.StatusNotFound {
			t.Errorf("removing %s again gave %d: %s", id, w.Code, w.Body)
		}
	}
	var after int
	if err := h.db.QueryRowContext(ctx,
		`SELECT count(*) FROM activity WHERE entity = 'member'`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("%d rows were written for removals that did nothing", after-before)
	}

	// A researcher may edit and not delete, and is told the proposition is not
	// there rather than that the role is wrong.
	if _, err := h.db.ExecContext(ctx, `UPDATE users SET role = ? WHERE id = ?`,
		auth.RoleResearcher, h.stranger.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.board.AddMember(ctx,
		core.Actor{Kind: core.KindUser, ID: h.user.ID, Name: h.user.Name}, h.prop, h.stranger.ID); err != nil {
		t.Fatal(err)
	}
	if w := h.do("DELETE", "/api/v1/propositions/"+prop,
		h.tokenFor(h.stranger, auth.ScopeRead, auth.ScopeWrite), ""); w.Code != http.StatusNotFound {
		t.Fatalf("a researcher deleted a proposition: %d %s", w.Code, w.Body)
	}

	if w := h.do("DELETE", "/api/v1/propositions/"+prop, token, ""); w.Code != http.StatusOK {
		t.Fatalf("deleting gave %d: %s", w.Code, w.Body)
	}
	if w := h.do("GET", "/api/v1/propositions/"+prop, token, ""); w.Code != http.StatusNotFound {
		t.Fatalf("the proposition is still there: %d %s", w.Code, w.Body)
	}
}

// An Idempotency-Key makes a request that arrives twice one change. An agent
// whose connection dropped before the answer came back cannot tell a request
// that was lost from one that was applied; the header is how it says the second
// attempt is the same change as the first.
func TestIdempotencyKeyMakesARepeatedRequestOneChange(t *testing.T) {
	h := newBoardHarness(t)
	token := h.token(auth.ScopeRead, auth.ScopeWrite)
	body := `{"title":"Read the almanac"}`
	path := "/api/v1/columns/" + strconv.FormatInt(h.column, 10) + "/cards"

	var events []core.Event
	for i := 0; i < 2; i++ {
		w := h.keyed("POST", path, token, body, "a-caller-key")
		if w.Code != http.StatusOK {
			t.Fatalf("attempt %d gave %d: %s", i+1, w.Code, w.Body)
		}
		events = append(events, h.event(w))
	}
	if events[0].Replayed {
		t.Error("the first answer says it was replayed")
	}
	if !events[1].Replayed {
		t.Error("the second answer does not say it was replayed")
	}
	if events[1].EntityID != events[0].EntityID {
		t.Errorf("the second answer is card %d, want the first one, %d",
			events[1].EntityID, events[0].EntityID)
	}
	var n int
	if err := h.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM cards WHERE column_id = ?`, h.column).Scan(&n); err != nil {
		t.Fatal(err)
	}
	// The card the harness starts with, and the one this test made.
	if n != 2 {
		t.Errorf("%d cards in the column, want 2", n)
	}

	// A different key is a different change, and a key that is not one is the
	// request malformed rather than something quietly ignored.
	if w := h.keyed("POST", path, token, body, "another-key"); w.Code != http.StatusOK {
		t.Fatalf("a second key gave %d: %s", w.Code, w.Body)
	}
	if w := h.keyed("POST", path, token, body, "not a key"); w.Code != http.StatusBadRequest {
		t.Errorf("a malformed key gave %d: %s", w.Code, w.Body)
	}
	// A read carries no change to remember, so the header is nothing to it.
	if w := h.keyed("GET", "/api/v1/propositions", token, "", "not a key"); w.Code != http.StatusOK {
		t.Errorf("a read with a malformed key gave %d: %s", w.Code, w.Body)
	}
}

// keyed is do with the header a caller names its change with.
func (h *harness) keyed(method, target, token, body, key string) *httptest.ResponseRecorder {
	h.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set(IdempotencyKey, key)
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	return w
}
