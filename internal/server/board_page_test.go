package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/davidtorcivia/theses/internal/api"
	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/store"
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

// as returns a client signed in as another account. The session is minted
// directly rather than through the invite flow, which its own tests cover.
func (h *harness) as(handle, name, role string) *http.Client {
	h.Helper()
	ctx := context.Background()
	id, err := store.CreateUser(ctx, h.db, &store.User{
		Handle: handle, Email: handle + "@example.com", Name: name,
		Initials: strings.ToUpper(handle[:2]), Colour: Palette[2], Role: role, PasswordHash: "x",
	})
	if err != nil {
		h.Fatal(err)
	}
	user, err := store.UserByID(ctx, h.db, id)
	if err != nil {
		h.Fatal(err)
	}
	rec := httptest.NewRecorder()
	if err := h.srv.auth.StartSession(ctx, rec, httptest.NewRequest("GET", "/", nil), user, 1); err != nil {
		h.Fatal(err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		h.Fatal(err)
	}
	base, err := url.Parse(h.http.URL)
	if err != nil {
		h.Fatal(err)
	}
	jar.SetCookies(base, rec.Result().Cookies())
	return &http.Client{Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (h *harness) payloadAs(client *http.Client, path string) (int, shell) {
	h.Helper()
	res, err := client.Get(h.http.URL + path)
	if err != nil {
		h.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return res.StatusCode, shell{}
	}
	m := payloadRe.FindStringSubmatch(string(body))
	if m == nil {
		h.Fatalf("%s carries no payload", path)
	}
	var state shell
	if err := json.Unmarshal([]byte(m[1]), &state); err != nil {
		h.Fatal(err)
	}
	return res.StatusCode, state
}

// A proposition is readable through membership, for everybody but an owner. An
// editor who is a member of nothing gets an empty workspace rather than a 403
// on somebody else's board, and the rail never names what they cannot read.
func TestThePayloadShowsOnlyWhatMembershipAllows(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.setupOwner()
	owner := h.owner()

	first, err := h.srv.board.CreateProposition(ctx, owner, "Tidal Power")
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.srv.board.CreateProposition(ctx, owner, "Tide Tables")
	if err != nil {
		t.Fatal(err)
	}

	editor := h.as("grace", "Grace Hopper", auth.RoleEditor)
	status, state := h.payloadAs(editor, "/")
	if status != http.StatusOK {
		t.Fatalf("an editor who is a member of nothing got %d on /", status)
	}
	if state.Open != 0 || len(state.Propositions) != 0 {
		t.Errorf("the rail showed %d propositions and opened %d", len(state.Propositions), state.Open)
	}
	// A proposition they are not a member of answers the same as one that is
	// not there, because they are not told it is.
	missing, _ := h.payloadAs(editor, "/p/9999")
	if status, _ := h.payloadAs(editor, "/p/"+strconv.FormatInt(first.EntityID, 10)); status != missing {
		t.Errorf("a proposition they are not a member of gave %d and a missing one gave %d", status, missing)
	}
	if missing != http.StatusNotFound {
		t.Errorf("a missing proposition gave %d", missing)
	}

	var grace int64
	if err := h.db.QueryRowContext(ctx, `SELECT id FROM users WHERE handle = 'grace'`).Scan(&grace); err != nil {
		t.Fatal(err)
	}
	if _, err := h.srv.board.AddMember(ctx, owner, second.EntityID, grace); err != nil {
		t.Fatal(err)
	}

	status, state = h.payloadAs(editor, "/")
	if status != http.StatusOK {
		t.Fatalf("/ gave %d once they were a member", status)
	}
	if state.Open != second.EntityID {
		t.Errorf("/ opened %d, want the one they are a member of, %d", state.Open, second.EntityID)
	}
	if len(state.Propositions) != 1 || state.Propositions[0].ID != second.EntityID {
		t.Errorf("the rail is %+v, want only the proposition they are a member of", state.Propositions)
	}

	// The owner still sees both.
	_, mine := h.payloadAs(h.client, "/")
	if len(mine.Propositions) != 2 {
		t.Errorf("the owner sees %d propositions, want 2", len(mine.Propositions))
	}
}

// The page decides what to draw from the proposition's archived_at, because an
// archived proposition is read only and drawing an add or a tick on one would
// only offer a refusal. The payload has to carry it either way.
func TestThePayloadSaysWhenAPropositionIsArchived(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.setupOwner()
	owner := h.owner()

	e, err := h.srv.board.CreateProposition(ctx, owner, "Tidal Power")
	if err != nil {
		t.Fatal(err)
	}
	path := "/p/" + strconv.FormatInt(e.EntityID, 10)

	state := h.payload(path)
	if state.Propositions[0].ArchivedAt != nil {
		t.Errorf("a live proposition carries archived_at %v", *state.Propositions[0].ArchivedAt)
	}

	if _, err := h.srv.board.ArchiveProposition(ctx, owner, e.EntityID); err != nil {
		t.Fatal(err)
	}
	state = h.payload(path)
	if len(state.Propositions) != 1 || state.Propositions[0].ArchivedAt == nil {
		t.Fatalf("an archived proposition is %+v", state.Propositions)
	}
	if state.Open != e.EntityID || state.Board == nil {
		t.Errorf("an archived proposition no longer opens: %d", state.Open)
	}
	// It is readable, and only restore and delete still work on it.
	if _, err := h.srv.board.SetStatus(ctx, owner, e.EntityID, "recording"); !errors.Is(err, board.ErrArchived) {
		t.Errorf("the board took an edit on an archived proposition: %v", err)
	}
}

// The fallback at the path the plan names is for machines: a bearer token with
// the read scope, no session cookie, and the same membership rule. The browser
// keeps /api/events with the session it already has.
func TestTheEventStreamIsReachableWithAToken(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.setupOwner()
	owner := h.owner()

	e, err := h.srv.board.CreateProposition(ctx, owner, "Tidal Power")
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
	path := "/api/v1/propositions/" + strconv.FormatInt(e.EntityID, 10) + "/events?since=0&wait=0"

	ask := func(token string, at string) (int, string) {
		t.Helper()
		req, err := http.NewRequest("GET", h.http.URL+at, nil)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := h.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		return res.StatusCode, string(body)
	}

	// The owner's session cookie is in the jar and is not a way in here.
	if status, body := ask("", path); status != http.StatusUnauthorized {
		t.Errorf("a session cookie reached the token path: %d %s", status, body)
	}

	read := h.apiToken(auth.ScopeRead)
	status, body := ask(read, path)
	if status != http.StatusOK {
		t.Fatalf("a read token got %d: %s", status, body)
	}
	var got struct {
		Events []struct {
			Seq    int64  `json:"seq"`
			Entity string `json:"entity"`
		} `json:"events"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Events) == 0 || got.Events[len(got.Events)-1].Entity != "card" {
		t.Errorf("the stream is %s", body)
	}

	// The same person, a second token, without the read scope.
	var machine int64
	if err := h.db.QueryRowContext(ctx, `SELECT id FROM users WHERE handle = 'nora'`).Scan(&machine); err != nil {
		t.Fatal(err)
	}
	files, err := h.srv.auth.CreateAPIToken(ctx, machine, "uploader", []string{auth.ScopeFiles})
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := ask(files, path); status != http.StatusForbidden {
		t.Error("a token with no read scope was not refused")
	}

	// A token whose owner is not a member is not told the proposition is
	// there, whatever its scopes say.
	if _, err := h.db.ExecContext(ctx, `UPDATE users SET role = 'editor' WHERE id = ?`, machine); err != nil {
		t.Fatal(err)
	}
	if status, _ := ask(read, path); status != http.StatusNotFound {
		t.Errorf("a token whose owner is not a member got %d", status)
	}
}

// A section is one form and several commands. Every field of it is checked
// before the first command runs, or a refusal halfway down leaves the ones
// above it applied and the page redrawn with unsaved values beside them.
func TestASectionIsCheckedBeforeAnyOfItRuns(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.setupOwner()
	owner := h.owner()

	e, err := h.srv.board.CreateProposition(ctx, owner, "Tidal Power")
	if err != nil {
		t.Fatal(err)
	}
	path := "/p/" + strconv.FormatInt(e.EntityID, 10) + "/settings"
	_, body := h.get(path)
	csrf := csrfRe.FindStringSubmatch(body)[1]

	// The schedule is a status command and then a schedule command. A target
	// date over the cap used to refuse only after the status had been set.
	res, page := h.post(path, url.Values{"csrf": {csrf}, "do": {"schedule"},
		"status": {"recording"}, "episode": {"11"}, "target": {strings.Repeat("x", board.MaxWord+1)}})
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("an oversized target gave %d", res.StatusCode)
	}
	if !strings.Contains(page, "longer than") {
		t.Errorf("the page does not say why: %s", page[:min(len(page), 400)])
	}
	p, err := board.GetProposition(ctx, h.db, e.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != "idea" {
		t.Errorf("the status was set to %q although the section was refused", p.Status)
	}
	if p.Episode != nil {
		t.Errorf("the episode was set to %v although the section was refused", *p.Episode)
	}

	// The columns section is an add, a reorder and a rename each. An oversized
	// new column leaves the renames alone.
	cols, err := board.ListColumns(ctx, h.db, e.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	res, _ = h.post(path, url.Values{"csrf": {csrf}, "do": {"columns"},
		"add": {strings.Repeat("x", board.MaxLine+1)},
		"name-" + strconv.FormatInt(cols[0].ID, 10): {"Renamed"},
	})
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("an oversized column name gave %d", res.StatusCode)
	}
	after, err := board.ListColumns(ctx, h.db, e.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(cols) || after[0].Name != cols[0].Name {
		t.Errorf("the refused section still renamed or added: %+v", after)
	}
}

// An archived proposition is read only on its settings page as well as on its
// board: nothing offers to change it, and restore and delete are what is left.
func TestTheSettingsPageOfAnArchivedPropositionOffersNoEdits(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.setupOwner()
	owner := h.owner()

	e, err := h.srv.board.CreateProposition(ctx, owner, "Tidal Power")
	if err != nil {
		t.Fatal(err)
	}
	path := "/p/" + strconv.FormatInt(e.EntityID, 10) + "/settings"

	_, live := h.get(path)
	if !strings.Contains(live, `class="save"`) || !strings.Contains(live, `name="add"`) {
		t.Fatal("a live proposition has no save or no add column")
	}

	if _, err := h.srv.board.ArchiveProposition(ctx, owner, e.EntityID); err != nil {
		t.Fatal(err)
	}
	_, gone := h.get(path)
	if strings.Contains(gone, `class="save"`) {
		t.Error("an archived proposition still offers a save")
	}
	if strings.Contains(gone, `name="add"`) || strings.Contains(gone, `name="remove"`) {
		t.Error("an archived proposition still offers to add or remove a column")
	}
	if !strings.Contains(gone, "archived") || !strings.Contains(gone, "Restore this proposition") {
		t.Error("the page does not say it is archived or offer a restore")
	}
	if !strings.Contains(gone, "Delete permanently") {
		t.Error("the page does not offer a delete")
	}
}

// A section is one save, so it is one transaction: an add that goes through
// and a remove that does not must leave the board as it was, not half changed
// with a refusal on top of it.
func TestARefusedSectionLeavesNothingBehind(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.setupOwner()
	owner := h.owner()

	e, err := h.srv.board.CreateProposition(ctx, owner, "Tidal Power")
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
	path := "/p/" + strconv.FormatInt(e.EntityID, 10) + "/settings"
	_, body := h.get(path)
	csrf := csrfRe.FindStringSubmatch(body)[1]

	// Add a column and remove one that still has a card, in one submit.
	res, page := h.post(path, url.Values{"csrf": {csrf}, "do": {"columns"},
		"add":    {"Fact check"},
		"remove": {strconv.FormatInt(cols[0].ID, 10)},
	})
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("removing a column with a card gave %d", res.StatusCode)
	}
	if !strings.Contains(page, "move the cards out") {
		t.Errorf("the page does not say why")
	}
	after, err := board.ListColumns(ctx, h.db, e.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(cols) {
		t.Fatalf("%d columns after a refused section, want the %d there were", len(after), len(cols))
	}
	for i, c := range after {
		if c.Name != cols[i].Name {
			t.Errorf("column %d is %q, want %q", i, c.Name, cols[i].Name)
		}
	}

	// The members section is the same shape: a removal that goes through and
	// an addition of somebody who is not there must leave the list alone.
	before, err := board.GetProposition(ctx, h.db, e.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	res, _ = h.post(path, url.Values{"csrf": {csrf}, "do": {"members"}, "member": {"9999"}})
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("adding somebody who is not there gave %d", res.StatusCode)
	}
	now, err := board.GetProposition(ctx, h.db, e.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	if len(now.Members) != len(before.Members) {
		t.Errorf("members are %v, want the %v there were", now.Members, before.Members)
	}

	// And a section that is refused writes no activity either.
	var rows int
	if err := h.db.QueryRowContext(ctx, `SELECT count(*) FROM activity
		WHERE proposition_id = ? AND entity IN ('column', 'member')`, e.EntityID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	// Creating a proposition seeds its columns and its first member with the
	// proposition itself, so neither refused section should have left a row.
	if rows != 0 {
		t.Errorf("%d column and member rows in activity, want none from a refused section", rows)
	}
}

// The stream says what carried a change as well as who made it, so a tab can
// draw an agent's edit differently from the same person's own.
func TestTheEventStreamNamesWhatCarriedTheChange(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.setupOwner()
	owner := h.owner()

	e, err := h.srv.board.CreateProposition(ctx, owner, "Tidal Power")
	if err != nil {
		t.Fatal(err)
	}
	agent := owner
	agent.Via = api.ClientVia("research agent")
	if _, err := h.srv.board.SetStatus(ctx, agent, e.EntityID, "recording"); err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest("GET",
		h.http.URL+"/api/v1/propositions/"+strconv.FormatInt(e.EntityID, 10)+"/events?since=0&wait=0", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.apiToken(auth.ScopeRead))
	res, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()

	var got struct {
		Events []core.Event `json:"events"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Events) == 0 {
		t.Fatalf("the stream is %s", body)
	}
	last := got.Events[len(got.Events)-1]
	if last.Action != "status" || last.Actor.Via != api.ClientVia("research agent") {
		t.Errorf("last event = %+v", last)
	}
	if got.Events[0].Actor.Via != "" {
		t.Errorf("a change made in the browser names a carrier: %q", got.Events[0].Actor.Via)
	}
}

// The payload carries the workspace's time zone, because whether a due date has
// passed is a question about the show's calendar day and not about the one on
// the laptop reading the board.
func TestShellCarriesTheWorkspaceTimezone(t *testing.T) {
	for _, tc := range []struct{ set, want string }{
		{set: "", want: "America/New_York"},
		{set: "Europe/Berlin", want: "Europe/Berlin"},
		{set: "UTC", want: "UTC"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			h := newHarness(t)
			h.setupOwner()
			if tc.set != "" {
				if err := h.srv.settings.Set(context.Background(), "workspace.timezone",
					[]string{tc.set}, h.owner().ID); err != nil {
					t.Fatal(err)
				}
			}
			if got := h.payload("/").Timezone; got != tc.want {
				t.Fatalf("the payload carries the time zone %q", got)
			}
		})
	}
}

// The Members section says what membership does, and offers an owner the row
// that brings somebody into the workspace. Nobody else is offered it: the
// handler behind it is owner only and would refuse them.
func TestPropositionSettingsOffersTheInviteRowToOwnersOnly(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		role  string
		offer bool
	}{
		{auth.RoleOwner, true},
		{auth.RoleEditor, false},
		{auth.RoleResearcher, false},
	} {
		t.Run(tc.role, func(t *testing.T) {
			h := newHarness(t)
			h.setupOwner()
			owner := h.owner()
			e, err := h.srv.board.CreateProposition(ctx, owner, "Tidal Power")
			if err != nil {
				t.Fatal(err)
			}
			h.setRole(t, owner.ID, tc.role)

			_, body := h.get("/p/" + strconv.FormatInt(e.EntityID, 10) + "/settings")
			if !strings.Contains(body, "an account that is on none opens an empty workspace") {
				t.Error("the Members section does not say what membership does")
			}
			if !strings.Contains(body, `id="members"`) {
				t.Error("the Members section has no anchor to land on")
			}
			if got := strings.Contains(body, `action="/settings/team/invite"`); got != tc.offer {
				t.Errorf("the invite row is drawn %v for a %s", got, tc.role)
			}
		})
	}
}
