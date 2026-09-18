package realtime

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/store"
)

type rig struct {
	*testing.T
	hub    *Hub
	boards *board.Service
	db     *store.DB
	http   *httptest.Server
	users  map[string]*store.User
	cookie map[string]string
	prop   int64
	cols   []board.Column
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ctx := context.Background()
	db := store.OpenTemp(t)
	a := auth.New(db, []byte("a session key of at least thirty-two bytes"), false, false)
	boards := board.New(core.New(db, core.NewBus()), func() board.Defaults {
		return board.Defaults{Status: "idea", Columns: []string{"Research", "Outline"}}
	})
	hub := New(boards, a, slog.New(slog.NewTextHandler(io.Discard, nil)))

	r := &rig{T: t, hub: hub, boards: boards, db: db,
		users: map[string]*store.User{}, cookie: map[string]string{}}

	mux := http.NewServeMux()
	mux.Handle("GET /ws", hub.Handler())
	mux.HandleFunc("GET /api/events", hub.Events)
	r.http = httptest.NewServer(mux)
	t.Cleanup(r.http.Close)

	for _, u := range []struct{ handle, role string }{
		{"ada", auth.RoleOwner}, {"grace", auth.RoleEditor}, {"stranger", auth.RoleEditor},
	} {
		id, err := store.CreateUser(ctx, db, &store.User{
			Handle: u.handle, Email: u.handle + "@example.com", Name: strings.ToUpper(u.handle),
			Initials: strings.ToUpper(u.handle), Colour: "#1100ff", Role: u.role, PasswordHash: "x",
		})
		if err != nil {
			t.Fatal(err)
		}
		user, err := store.UserByID(ctx, db, id)
		if err != nil {
			t.Fatal(err)
		}
		r.users[u.handle] = user

		// A real session, so the handshake resolves it the way a browser's does.
		rec := httptest.NewRecorder()
		if err := a.StartSession(ctx, rec, httptest.NewRequest("GET", "/", nil), user, 1); err != nil {
			t.Fatal(err)
		}
		for _, c := range rec.Result().Cookies() {
			if c.Name == auth.SessionCookie {
				r.cookie[u.handle] = c.Name + "=" + c.Value
			}
		}
	}

	e, err := boards.CreateProposition(ctx, r.actor("ada"), "Tidal Power")
	if err != nil {
		t.Fatal(err)
	}
	r.prop = e.EntityID
	if _, err := boards.AddMember(ctx, r.actor("ada"), r.prop, r.users["grace"].ID); err != nil {
		t.Fatal(err)
	}
	if r.cols, err = board.ListColumns(ctx, db, r.prop); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *rig) actor(handle string) core.Actor {
	u := r.users[handle]
	return core.Actor{Kind: core.KindUser, ID: u.ID, Name: u.Name}
}

func (r *rig) dial(handle string) (*websocket.Conn, error) {
	return r.dialProposition(handle, r.prop)
}

func (r *rig) dialProposition(handle string, proposition int64) (*websocket.Conn, error) {
	r.Helper()
	wsURL := "ws" + strings.TrimPrefix(r.http.URL, "http") +
		"/ws?proposition=" + strconv.FormatInt(proposition, 10)
	config, err := websocket.NewConfig(wsURL, r.http.URL)
	if err != nil {
		r.Fatal(err)
	}
	config.Header.Set("Cookie", r.cookie[handle])
	return websocket.DialConfig(config)
}

func (r *rig) mustDial(handle string) *websocket.Conn {
	r.Helper()
	ws, err := r.dial(handle)
	if err != nil {
		r.Fatalf("%s could not connect: %v", handle, err)
	}
	r.Cleanup(func() { ws.Close() })
	return ws
}

// read waits for the next frame of a type, skipping the others.
func read(t *testing.T, ws *websocket.Conn, want string) message {
	t.Helper()
	ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		var raw string
		if err := websocket.Message.Receive(ws, &raw); err != nil {
			t.Fatalf("waiting for a %s frame: %v", want, err)
		}
		var m message
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatal(err)
		}
		if m.Type == want {
			return m
		}
	}
}

// readNothing fails if any applied event reaches this tab before the deadline.
// Presence frames are ignored: they are this tab's own arrival.
func readNothing(t *testing.T, ws *websocket.Conn) {
	t.Helper()
	ws.SetReadDeadline(time.Now().Add(600 * time.Millisecond))
	for {
		var raw string
		if err := websocket.Message.Receive(ws, &raw); err != nil {
			return
		}
		var m message
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatal(err)
		}
		if m.Type == "event" {
			t.Fatalf("an event reached a tab that may not read it: %+v", m.Event)
		}
	}
}

func send(t *testing.T, ws *websocket.Conn, cmd command) {
	t.Helper()
	b, err := json.Marshal(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := websocket.Message.Send(ws, string(b)); err != nil {
		t.Fatal(err)
	}
}

// The round trip: two tabs connect, both see the presence, one moves a card and
// gets the applied event back, and the other is told about it unasked.
func TestSocketCarriesPresenceCommandsAndEvents(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	card, err := r.boards.CreateCard(ctx, r.actor("ada"), r.cols[0].ID, "Call the engineer", nil)
	if err != nil {
		t.Fatal(err)
	}

	mine := r.mustDial("ada")
	if p := read(t, mine, "presence"); len(p.People) != 1 || p.People[0].ID != r.users["ada"].ID {
		t.Fatalf("presence is %+v", p.People)
	}
	theirs := r.mustDial("grace")
	if p := read(t, mine, "presence"); len(p.People) != 2 {
		t.Fatalf("the second tab did not show up: %+v", p.People)
	}

	send(t, theirs, command{ID: 1, Cmd: "where", Args: args{Where: "card:" + strconv.FormatInt(card.EntityID, 10)}})
	found := false
	for i := 0; i < 3 && !found; i++ {
		for _, p := range read(t, mine, "presence").People {
			if p.ID == r.users["grace"].ID && strings.HasPrefix(p.Where, "card:") {
				found = true
			}
		}
	}
	if !found {
		t.Error("presence never carried what the other tab had open")
	}

	send(t, mine, command{ID: 7, Cmd: "card.move", Args: args{
		Card: card.EntityID, Column: r.cols[1].ID}})
	ack := read(t, mine, "ack")
	if ack.ID != 7 || ack.Event == nil || ack.Event.Action != "move" {
		t.Fatalf("the answer is %+v", ack)
	}
	var moved board.Card
	if err := json.Unmarshal(ack.Event.After, &moved); err != nil {
		t.Fatal(err)
	}
	if moved.ColumnID != r.cols[1].ID {
		t.Errorf("the card landed in column %d, want %d", moved.ColumnID, r.cols[1].ID)
	}

	echo := read(t, theirs, "event")
	if echo.Event.Seq != ack.Event.Seq {
		t.Errorf("the other tab saw seq %d, want %d", echo.Event.Seq, ack.Event.Seq)
	}
	if echo.Event.Actor.ID != r.users["ada"].ID {
		t.Errorf("the event names actor %+v", echo.Event.Actor)
	}
}

func TestSocketAnswersAConflictAndARefusal(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	e, err := r.boards.CreateCard(ctx, r.actor("ada"), r.cols[0].ID, "Call the engineer", nil)
	if err != nil {
		t.Fatal(err)
	}
	card, err := board.GetCard(ctx, r.db, e.EntityID)
	if err != nil {
		t.Fatal(err)
	}

	ws := r.mustDial("ada")
	send(t, ws, command{ID: 1, Cmd: "card.title", Args: args{
		Card: card.ID, Base: card.Version, Title: "Call the surveyor"}})
	read(t, ws, "ack")

	send(t, ws, command{ID: 2, Cmd: "card.title", Args: args{
		Card: card.ID, Base: card.Version, Title: "Call the hydrologist"}})
	answer := read(t, ws, "conflict")
	if answer.ID != 2 || answer.Conflict == nil {
		t.Fatalf("the stale edit got %+v", answer)
	}
	if answer.Conflict.Current != "Call the surveyor" {
		t.Errorf("the conflict carries %q", answer.Conflict.Current)
	}

	send(t, ws, command{ID: 3, Cmd: "card.nonsense"})
	if refusal := read(t, ws, "error"); refusal.ID != 3 {
		t.Errorf("an unknown command got %+v", refusal)
	}
}

// A socket is only as open as the person behind it. Someone who is not a member
// of the proposition gets no socket at all.
func TestSocketRefusesANonMemberAndAStrangeOrigin(t *testing.T) {
	r := newRig(t)
	if ws, err := r.dial("stranger"); err == nil {
		ws.Close()
		t.Error("a non member opened a socket on the proposition")
	}

	wsURL := "ws" + strings.TrimPrefix(r.http.URL, "http") +
		"/ws?proposition=" + strconv.FormatInt(r.prop, 10)
	config, err := websocket.NewConfig(wsURL, "https://elsewhere.example")
	if err != nil {
		t.Fatal(err)
	}
	config.Header.Set("Cookie", r.cookie["ada"])
	if ws, err := websocket.DialConfig(config); err == nil {
		ws.Close()
		t.Error("a page on another origin opened a socket with the session cookie")
	}
}

// An empty workspace has nothing open, but it still needs a socket: creating
// the first proposition goes through it.
func TestSocketOnAnEmptyWorkspaceCanCreateTheFirstProposition(t *testing.T) {
	r := newRig(t)
	ws, err := r.dialProposition("ada", 0)
	if err != nil {
		t.Fatalf("an empty workspace could not open a socket: %v", err)
	}
	defer ws.Close()

	send(t, ws, command{ID: 1, Cmd: "proposition.create", Args: args{Title: "Tide Tables"}})
	ack := read(t, ws, "ack")
	if ack.ID != 1 || ack.Event == nil || ack.Event.Action != "create" {
		t.Fatalf("creating the first proposition got %+v", ack)
	}
	if ack.Event.Proposition != ack.Event.EntityID {
		t.Errorf("the event files itself under proposition %d, want %d",
			ack.Event.Proposition, ack.Event.EntityID)
	}
	p, err := board.GetProposition(context.Background(), r.db, ack.Event.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != "Tide Tables" {
		t.Errorf("the proposition is %+v", p)
	}
}

// A person who is not a member is not told a proposition exists, so no edit to
// it reaches their socket, not even the rail entry.
func TestSocketSendsNoEventsForAPropositionTheTabCannotRead(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)

	stranger, err := r.dialProposition("stranger", 0)
	if err != nil {
		t.Fatalf("a tab with nothing open could not connect: %v", err)
	}
	defer stranger.Close()

	if _, err := r.boards.EditProposition(ctx, r.actor("ada"), r.prop, "Renamed", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.boards.CreateCard(ctx, r.actor("ada"), r.cols[0].ID, "Call the engineer", nil); err != nil {
		t.Fatal(err)
	}
	readNothing(t, stranger)

	// Once they are a member, the rail entry reaches them.
	if _, err := r.boards.AddMember(ctx, r.actor("ada"), r.prop, r.users["stranger"].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.boards.EditProposition(ctx, r.actor("ada"), r.prop, "Renamed again", "", ""); err != nil {
		t.Fatal(err)
	}
	for {
		e := read(t, stranger, "event")
		if e.Event.Entity != "proposition" {
			continue
		}
		var p board.Proposition
		if err := json.Unmarshal(e.Event.After, &p); err != nil {
			t.Fatal(err)
		}
		if p.Title != "Renamed again" {
			t.Errorf("the rail entry a new member saw is %+v", p)
		}
		break
	}
}

// A socket is only as good as the session behind it. Signing out everywhere
// stops the writes, and being removed from the proposition stops the reading.
func TestSocketDropsARevokedSessionAndARemovedMember(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)

	member := r.mustDial("grace")
	read(t, member, "presence")
	if _, err := r.boards.RemoveMember(ctx, r.actor("ada"), r.prop, r.users["grace"].ID); err != nil {
		t.Fatal(err)
	}
	member.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		var raw string
		if err := websocket.Message.Receive(member, &raw); err != nil {
			break // the socket was closed, which is the point
		}
		var m message
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatal(err)
		}
		if m.Type == "event" && m.Event.Entity == "card" {
			t.Fatal("a removed member was still sent the board")
		}
	}

	// Signing out everywhere is a bumped epoch, which the next command sees.
	ws := r.mustDial("ada")
	read(t, ws, "presence")
	if err := store.BumpSessionEpoch(ctx, r.db, r.users["ada"].ID); err != nil {
		t.Fatal(err)
	}
	send(t, ws, command{ID: 1, Cmd: "card.create", Args: args{
		Column: r.cols[0].ID, Title: "Should not land"}})
	ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	var raw string
	if err := websocket.Message.Receive(ws, &raw); err == nil {
		t.Fatalf("a revoked session got an answer: %s", raw)
	}
	var n int
	if err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM cards WHERE title = 'Should not land'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("a revoked session wrote a card")
	}
}

// An idle socket is checked on a timer as well, so a tab nobody is touching
// does not keep receiving after the account behind it is signed out.
func TestIdleSocketIsDroppedWhenTheSessionGoes(t *testing.T) {
	was := sessionCheck
	sessionCheck = 50 * time.Millisecond
	t.Cleanup(func() { sessionCheck = was })

	ctx := context.Background()
	r := newRig(t)
	ws := r.mustDial("grace")
	read(t, ws, "presence")

	if err := store.BumpSessionEpoch(ctx, r.db, r.users["grace"].ID); err != nil {
		t.Fatal(err)
	}
	ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	var raw string
	if err := websocket.Message.Receive(ws, &raw); err == nil {
		t.Fatalf("an idle socket on a revoked session stayed open: %s", raw)
	}
}

// The fallback reads the same stream out of the activity table.
func TestLongPollServesTheSameStream(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	if _, err := r.boards.CreateCard(ctx, r.actor("ada"), r.cols[0].ID, "Call the engineer", nil); err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	ask := func(handle string, since int64) *http.Response {
		t.Helper()
		req, err := http.NewRequest("GET", r.http.URL+"/api/events?"+url.Values{
			"proposition": {strconv.FormatInt(r.prop, 10)},
			"since":       {strconv.FormatInt(since, 10)},
		}.Encode(), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Cookie", r.cookie[handle])
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	res := ask("ada", 0)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the fallback gave %d", res.StatusCode)
	}
	var got struct{ Events []core.Event }
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Events) == 0 || got.Events[len(got.Events)-1].Entity != "card" {
		t.Fatalf("the fallback returned %+v", got.Events)
	}

	refused := ask("stranger", 0)
	defer refused.Body.Close()
	if refused.StatusCode != http.StatusForbidden {
		t.Errorf("a non member got %d from the fallback", refused.StatusCode)
	}
}
