package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
)

var payloadRe = regexp.MustCompile(`(?s)<script type="application/json" id="payload">(.*?)</script>`)

func (h *harness) payload(path string) shell {
	h.Helper()
	res, body := h.get(path)
	if res.StatusCode != http.StatusOK {
		h.Fatalf("%s gave %d", path, res.StatusCode)
	}
	m := payloadRe.FindStringSubmatch(body)
	if m == nil {
		h.Fatalf("%s carries no payload", path)
	}
	var state shell
	if err := json.Unmarshal([]byte(m[1]), &state); err != nil {
		h.Fatalf("the payload on %s is not JSON: %v", path, err)
	}
	return state
}

// owner returns the actor the owner acts as, once setupOwner has run.
func (h *harness) owner() core.Actor {
	h.Helper()
	var id int64
	var name string
	if err := h.db.QueryRowContext(context.Background(),
		`SELECT id, name FROM users WHERE role = 'owner'`).Scan(&id, &name); err != nil {
		h.Fatal(err)
	}
	return core.Actor{Kind: core.KindUser, ID: id, Name: name}
}

func TestShellOffersNewPropositionWhenThereAreNone(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()

	state := h.payload("/")
	if state.Open != 0 || state.Board != nil {
		t.Errorf("an empty workspace opened proposition %d", state.Open)
	}
	if len(state.Propositions) != 0 {
		t.Errorf("propositions are %+v", state.Propositions)
	}
	if !state.Can["edit"] || !state.Can["settings"] {
		t.Errorf("the owner cannot edit or reach settings: %v", state.Can)
	}
	if len(state.Statuses) == 0 || len(state.QuestionLabels) != 4 {
		t.Errorf("statuses %v, question labels %v", state.Statuses, state.QuestionLabels)
	}
}

func TestShellCarriesTheOpenBoardAndEscapesItSafely(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.setupOwner()
	owner := h.owner()

	// A title that would close the script element it is rendered into.
	e, err := h.srv.board.CreateProposition(ctx, owner, `Tide </script><script>alert(1)</script>`)
	if err != nil {
		t.Fatal(err)
	}
	cols, err := board.ListColumns(ctx, h.db, e.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.srv.board.CreateCard(ctx, owner, cols[0].ID, "Call the engineer", nil); err != nil {
		t.Fatal(err)
	}

	_, body := h.get("/")
	if strings.Contains(body, "</script><script>alert(1)") {
		t.Fatal("the payload closed its own script element")
	}
	state := h.payload("/")
	if state.Open != e.EntityID || state.Board == nil {
		t.Fatalf("the shell opened %d with board %v", state.Open, state.Board)
	}
	if len(state.Board.Columns) == 0 || len(state.Board.Cards) != 1 {
		t.Fatalf("board is %+v", state.Board)
	}
	if state.Board.Cards[0].Title != "Call the engineer" {
		t.Errorf("card is %+v", state.Board.Cards[0])
	}
	if state.Board.Seq == 0 {
		t.Error("the board carries no sequence number to continue the stream from")
	}
	if len(state.Users) != 1 || state.Users[0].Colour != colourClass(Palette[1]) {
		t.Errorf("users are %+v", state.Users)
	}

	// The same proposition by its own address.
	direct := h.payload("/p/" + strconv.FormatInt(e.EntityID, 10))
	if direct.Open != e.EntityID {
		t.Errorf("/p/%d opened %d", e.EntityID, direct.Open)
	}
	if res, _ := h.get("/p/9999"); res.StatusCode != http.StatusNotFound {
		t.Errorf("a proposition that does not exist gave %d", res.StatusCode)
	}
}

func TestPropositionSettingsPageSavesThroughCommands(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.setupOwner()
	owner := h.owner()

	e, err := h.srv.board.CreateProposition(ctx, owner, "Tidal Power")
	if err != nil {
		t.Fatal(err)
	}
	path := "/p/" + strconv.FormatInt(e.EntityID, 10) + "/settings"

	res, body := h.get(path)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("%s gave %d", path, res.StatusCode)
	}
	for _, want := range []string{"Proposition", "Schedule", "Members", "Board columns", "Document", "Danger", "Research"} {
		if !strings.Contains(body, want) {
			t.Errorf("the settings page has no %s section", want)
		}
	}
	csrf := csrfRe.FindStringSubmatch(body)[1]

	res, _ = h.post(path, url.Values{"csrf": {csrf}, "do": {"proposition"},
		"title": {"Tidal Power"}, "statement": {"The tide is a battery."},
		"blurb": {"On the estuary."}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("saving the proposition gave %d", res.StatusCode)
	}
	res, _ = h.post(path, url.Values{"csrf": {csrf}, "do": {"schedule"},
		"status": {"researching"}, "episode": {"11"}, "target": {"24 Sep"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("saving the schedule gave %d", res.StatusCode)
	}

	p, err := board.GetProposition(ctx, h.db, e.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Statement != "The tide is a battery." || p.Status != "researching" {
		t.Errorf("proposition is %+v", p)
	}
	if p.Episode == nil || *p.Episode != "11" {
		t.Errorf("episode is %v", p.Episode)
	}

	// Adding a column and then refusing to delete one that has a card.
	res, _ = h.post(path, url.Values{"csrf": {csrf}, "do": {"columns"}, "add": {"Fact check"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("adding a column gave %d", res.StatusCode)
	}
	cols, err := board.ListColumns(ctx, h.db, e.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	if cols[len(cols)-1].Name != "Fact check" {
		t.Errorf("columns are %+v", cols)
	}
	if _, err := h.srv.board.CreateCard(ctx, owner, cols[0].ID, "Call the engineer", nil); err != nil {
		t.Fatal(err)
	}
	res, body = h.post(path, url.Values{"csrf": {csrf}, "do": {"columns"},
		"remove": {strconv.FormatInt(cols[0].ID, 10)}})
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "move the cards out") {
		t.Errorf("removing a column with a card gave %d", res.StatusCode)
	}

	// Every save is a command, so every one of them is in the activity log.
	var n int
	if err := h.db.QueryRowContext(ctx, `SELECT count(*) FROM activity
		WHERE proposition_id = ? AND entity IN ('proposition', 'column')`, e.EntityID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n < 5 {
		t.Errorf("%d activity rows for the saves, want one each", n)
	}

	// The document switches are stored settings, read back on the next render.
	res, _ = h.post(path, url.Values{"csrf": {csrf}, "do": {"document"}, "publish": {"1"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("saving the document section gave %d", res.StatusCode)
	}
	doc, err := board.GetDocumentSettings(ctx, h.db, e.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	if doc.OpenEditing || doc.History || !doc.Publish {
		t.Errorf("document settings are %+v", doc)
	}
}
