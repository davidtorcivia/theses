package realtime

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/docs"
	"github.com/davidtorcivia/theses/internal/store"
)

type writeSignalConn struct {
	net.Conn
	mu   sync.Mutex
	next chan struct{}
}

func (c *writeSignalConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	if c.next != nil {
		close(c.next)
		c.next = nil
	}
	c.mu.Unlock()
	return c.Conn.Write(p)
}

func (c *writeSignalConn) arm() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next = make(chan struct{})
	return c.next
}

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

// newRig builds a hub and a server in front of it. A tune function runs on the
// hub before anything can reach it, which is where a test shortens a timer: a
// write after the server is listening races the goroutines it spawns.
func newRig(t *testing.T, tune ...func(*Hub)) *rig {
	t.Helper()
	ctx := context.Background()
	db := store.OpenTemp(t)
	a := auth.New(db, []byte("a session key of at least thirty-two bytes"), false, false)
	boards := board.New(core.New(db, core.NewBus()), func() board.Defaults {
		return board.Defaults{Status: "idea", Statuses: []string{"idea", "recording"}, Columns: []string{"Research", "Outline"}}
	})
	hub := New(boards, a, slog.New(slog.NewTextHandler(io.Discard, nil)))
	hub.Docs = docs.New(boards.Service, "", func() string { return "" },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, f := range tune {
		f(hub)
	}

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

// awaitFrame waits for a frame that says something about a command, and passes
// over presence on the way. Presence is not an answer to anything: it is
// broadcast to everybody in the room whenever a tab arrives, leaves or moves,
// so one can land at any moment and a test that fails on whatever turns up next
// fails on somebody else's tab closing. It returns the error the socket ended
// on when nothing else came.
func awaitFrame(t *testing.T, ws *websocket.Conn, within time.Duration) (message, error) {
	t.Helper()
	ws.SetReadDeadline(time.Now().Add(within))
	for {
		var raw string
		if err := websocket.Message.Receive(ws, &raw); err != nil {
			return message{}, err
		}
		var m message
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatal(err)
		}
		if m.Type != "presence" {
			return m, nil
		}
	}
}

// timedOut is the deadline passing rather than the socket closing. For a test
// about a socket being dropped those are not the same thing at all: one says
// the tab was cut off and the other says it was left open with nothing to say.
func timedOut(err error) bool {
	return errors.Is(err, os.ErrDeadlineExceeded)
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

func blockedWebsocket(t *testing.T) (*websocket.Conn, *writeSignalConn) {
	t.Helper()
	raw, peer := net.Pipe()
	conn := &writeSignalConn{Conn: raw}
	deadline := time.Now().Add(time.Second)
	_ = raw.SetDeadline(deadline)
	_ = peer.SetDeadline(deadline)
	t.Cleanup(func() {
		raw.Close()
		peer.Close()
	})

	handshake := make(chan error, 1)
	go func() {
		req, err := http.ReadRequest(bufio.NewReader(peer))
		if err != nil {
			handshake <- err
			return
		}
		sum := sha1.Sum([]byte(req.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		_, err = fmt.Fprintf(peer, "HTTP/1.1 101 Switching Protocols\r\n"+
			"Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n",
			base64.StdEncoding.EncodeToString(sum[:]))
		handshake <- err
	}()
	config, err := websocket.NewConfig("ws://example.test/ws", "http://example.test")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := websocket.NewClient(config, conn)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-handshake; err != nil {
		t.Fatal(err)
	}
	_ = raw.SetDeadline(time.Time{})
	_ = peer.SetDeadline(time.Time{})
	return ws, conn
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

	// A refusal the tab can act on says what it was. A question that is not one
	// of the four used to come back as a fault on this side.
	send(t, ws, command{ID: 4, Cmd: "card.question", Args: args{Card: card.ID, Question: "V"}})
	if refusal := read(t, ws, "error"); refusal.ID != 4 ||
		!strings.Contains(refusal.Error, "four questions") {
		t.Errorf("a question that is not one of the four got %+v", refusal)
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

func TestSocketSendsRemovalFromANonOpenPropositionBeforeRevokingIt(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)

	created, err := r.boards.CreateProposition(ctx, r.actor("ada"), "Wave Power")
	if err != nil {
		t.Fatal(err)
	}
	other := created.EntityID
	if _, err := r.boards.AddMember(ctx, r.actor("ada"), other, r.users["grace"].ID); err != nil {
		t.Fatal(err)
	}

	member := r.mustDial("grace")
	read(t, member, "presence")
	if _, err := r.boards.RemoveMember(ctx, r.actor("ada"), other, r.users["grace"].ID); err != nil {
		t.Fatal(err)
	}
	removed := read(t, member, "event")
	if removed.Event == nil || removed.Event.Entity != "member" || removed.Event.Action != "remove" ||
		removed.Event.Proposition != other || removed.Event.EntityID != r.users["grace"].ID {
		t.Fatalf("removed member got %+v", removed)
	}

	if _, err := r.boards.EditProposition(ctx, r.actor("ada"), other, "Secret rename", "", ""); err != nil {
		t.Fatal(err)
	}
	readNothing(t, member)
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
	removed := read(t, member, "event")
	if removed.Event == nil || removed.Event.Entity != "member" || removed.Event.Action != "remove" {
		t.Fatalf("removed member got %+v", removed)
	}
	if m, err := awaitFrame(t, member, 3*time.Second); err == nil || timedOut(err) {
		t.Fatalf("removed member's socket did not close after its final event: %+v, %v", m, err)
	}

	// Signing out everywhere is a bumped epoch, which the next command sees.
	ws := r.mustDial("ada")
	read(t, ws, "presence")
	if err := store.BumpSessionEpoch(ctx, r.db, r.users["ada"].ID); err != nil {
		t.Fatal(err)
	}
	send(t, ws, command{ID: 1, Cmd: "card.create", Args: args{
		Column: r.cols[0].ID, Title: "Should not land"}})
	if m, err := awaitFrame(t, ws, 3*time.Second); err == nil {
		t.Fatalf("a revoked session got an answer: %+v", m)
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

func TestSocketSendsOpenPropositionDeletionBeforeClosing(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)

	member := r.mustDial("grace")
	read(t, member, "presence")
	if _, err := r.boards.DeleteProposition(ctx, r.actor("ada"), r.prop); err != nil {
		t.Fatal(err)
	}
	deleted := read(t, member, "event")
	if deleted.Event == nil || deleted.Event.Entity != "proposition" || deleted.Event.Action != "delete" {
		t.Fatalf("member got %+v", deleted)
	}
	if m, err := awaitFrame(t, member, 3*time.Second); err == nil || timedOut(err) {
		t.Fatalf("deleted proposition's socket did not close after its final event: %+v, %v", m, err)
	}
}

func TestBlockedWriterClosesAndLeavesWithinWriteWait(t *testing.T) {
	ws, raw := blockedWebsocket(t)
	h := &Hub{pingEvery: time.Hour, writeWait: 100 * time.Millisecond,
		rooms: map[int64]map[*client]struct{}{}}
	c := &client{hub: h, ws: ws, user: &store.User{ID: 2}, proposition: 7,
		member: map[int64]bool{7: true}, out: make(chan outbound, 1), done: make(chan struct{})}
	h.rooms[7] = map[*client]struct{}{c: {}}

	cleanupDone := make(chan struct{})
	go func() {
		var raw string
		_ = websocket.Message.Receive(ws, &raw)
		h.leave(c)
		close(cleanupDone)
	}()
	blocked := raw.arm()
	c.out <- outbound{body: []byte("first")}
	writerDone := make(chan struct{})
	go func() {
		c.write()
		close(writerDone)
	}()
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("writer did not reach the peer that stopped reading")
	}
	c.sendFinal(message{Type: "event"})

	for name, done := range map[string]<-chan struct{}{
		"client close": c.done, "writer return": writerDone, "room cleanup": cleanupDone,
	} {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s did not finish after the write deadline", name)
		}
	}
	h.mu.Lock()
	_, present := h.rooms[7]
	h.mu.Unlock()
	if present {
		t.Fatal("blocked client stayed in its room")
	}
}

func TestDeletedPropositionCannotReuseASocketsCachedMembership(t *testing.T) {
	c := &client{
		user: &store.User{ID: 2}, proposition: 7, member: map[int64]bool{7: true},
		done: make(chan struct{}), out: make(chan outbound, 1),
	}
	deleted := core.Event{Proposition: 7, Entity: "proposition", EntityID: 7, Action: "delete"}
	if !c.wants(deleted) {
		t.Fatal("the member could not see the deletion")
	}
	c.membership(deleted)
	if c.wants(deleted) {
		t.Fatal("the deleted proposition stayed readable")
	}

	reused := core.Event{Proposition: 7, Entity: "proposition", EntityID: 7, Action: "create",
		Actor: core.Actor{Kind: core.KindUser, ID: 3}}
	c.membership(reused)
	if c.wants(reused) {
		t.Fatal("a former member could read a new proposition that reused the deleted id")
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
	m, err := awaitFrame(t, ws, 3*time.Second)
	if err == nil {
		t.Fatalf("an idle socket on a revoked session stayed open: %+v", m)
	}
	// The socket has to have been closed, not merely to have gone quiet: this
	// is the test that the timer drops it.
	if timedOut(err) {
		t.Fatal("an idle socket on a revoked session was never dropped")
	}
}

// A dropped membership event could leave the socket's cached authorization
// wider than the database, so a tab that falls behind reconnects and rebuilds
// that cache before it receives anything else.
func TestForwardDropsATabThatFellBehind(t *testing.T) {
	bus := core.NewBus()
	sub := bus.Subscribe(1)
	defer sub.Close()
	// More than the subscription holds, with nothing reading it yet.
	for i := 0; i < 200; i++ {
		bus.Publish(core.Event{Seq: int64(i + 1), Proposition: 1, Entity: "card", Action: "create"})
	}
	if sub.Dropped() == 0 {
		t.Fatal("the subscription dropped nothing, so there is no hole to report")
	}

	c := &client{
		user: &store.User{ID: 1}, proposition: 1, member: map[int64]bool{1: true},
		out: make(chan outbound, 1024), done: make(chan struct{}),
	}
	go c.forward(sub)

	select {
	case <-c.done:
	case <-time.After(2 * time.Second):
		t.Fatal("a socket with a hole was not closed")
	}
	select {
	case raw := <-c.out:
		t.Fatalf("a socket with a hole was sent %s before it closed", raw.body)
	default:
	}
}

// A websocket request with no Origin header is not a browser on this site, and
// the session cookie would otherwise be enough to open it from anywhere.
func TestSocketRefusesARequestWithNoOrigin(t *testing.T) {
	r := newRig(t)
	wsURL := "ws" + strings.TrimPrefix(r.http.URL, "http") +
		"/ws?proposition=" + strconv.FormatInt(r.prop, 10)
	config, err := websocket.NewConfig(wsURL, r.http.URL)
	if err != nil {
		t.Fatal(err)
	}
	config.Origin = nil
	config.Header.Set("Cookie", r.cookie["ada"])
	if ws, err := websocket.DialConfig(config); err == nil {
		ws.Close()
		t.Error("a request with no origin opened a socket")
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

	// A non member is not told the proposition is there, which is the same
	// answer the page gives.
	refused := ask("stranger", 0)
	defer refused.Body.Close()
	if refused.StatusCode != http.StatusNotFound {
		t.Errorf("a non member got %d from the fallback", refused.StatusCode)
	}
}

// A socket takes short commands and a few of them a second. A frame larger
// than any command could be closes the connection, and a tab in a loop is
// refused rather than allowed to hold the one writer connection.
func TestSocketCapsTheFrameAndTheRate(t *testing.T) {
	r := newRig(t)

	big := r.mustDial("ada")
	read(t, big, "presence")
	if err := websocket.Message.Send(big, strings.Repeat("x", maxFrame+1024)); err != nil {
		t.Fatalf("sending the oversized frame failed before the server saw it: %v", err)
	}
	if m, err := awaitFrame(t, big, 3*time.Second); err == nil {
		t.Fatalf("an oversized frame was answered: %+v", m)
	}

	// An unknown command is refused without touching the database, so the run
	// below measures the limiter and nothing else.
	ws := r.mustDial("grace")
	read(t, ws, "presence")
	limited := false
	for i := 0; i < 400 && !limited; i++ {
		send(t, ws, command{ID: int64(i + 1), Cmd: "no.such.command"})
		answer := read(t, ws, "error")
		if !strings.Contains(answer.Error, "too many") {
			continue
		}
		limited = true
		// The refusal carries the number the tab gave the command, or the tab
		// waits for an answer that never comes.
		if answer.ID != int64(i+1) {
			t.Errorf("the refusal came back under id %d, want %d", answer.ID, i+1)
		}
	}
	if !limited {
		t.Error("a tab sending four hundred commands was never held back")
	}

	// The allowance is counted against the person as well as the tab, so a
	// second tab of the same account is already spent.
	second := r.mustDial("grace")
	read(t, second, "presence")
	send(t, second, command{ID: 1, Cmd: "no.such.command"})
	if answer := read(t, second, "error"); !strings.Contains(answer.Error, "too many") {
		t.Errorf("a second tab of a spent account got %q", answer.Error)
	}
}

// The fallback holds a request open until something happens, and it subscribes
// before it queries, so an event that lands in the window between the two is
// waiting rather than missed.
func TestLongPollHoldsOpenAndMissesNothingInTheWindow(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	if _, err := r.boards.CreateCard(ctx, r.actor("ada"), r.cols[0].ID, "Call the engineer", nil); err != nil {
		t.Fatal(err)
	}
	var seq int64
	if err := r.db.QueryRowContext(ctx,
		`SELECT max(id) FROM activity WHERE proposition_id = ?`, r.prop).Scan(&seq); err != nil {
		t.Fatal(err)
	}

	type answer struct {
		events []core.Event
		err    error
	}
	done := make(chan answer, 1)
	go func() {
		req, err := http.NewRequest("GET", r.http.URL+"/api/events?"+url.Values{
			"proposition": {strconv.FormatInt(r.prop, 10)},
			"since":       {strconv.FormatInt(seq, 10)},
		}.Encode(), nil)
		if err != nil {
			done <- answer{err: err}
			return
		}
		req.Header.Set("Cookie", r.cookie["ada"])
		res, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			done <- answer{err: err}
			return
		}
		defer res.Body.Close()
		var body struct{ Events []core.Event }
		err = json.NewDecoder(res.Body).Decode(&body)
		done <- answer{events: body.Events, err: err}
	}()

	// Long enough that the request is inside its wait, short enough that the
	// test is not waiting on the poll's own timeout.
	time.Sleep(200 * time.Millisecond)
	made, err := r.boards.CreateCard(ctx, r.actor("ada"), r.cols[0].ID, "Draft the opening", nil)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if len(got.events) == 0 {
			t.Fatal("the fallback answered with nothing after a change")
		}
		last := got.events[len(got.events)-1]
		if last.Seq != made.Seq || last.Entity != "card" {
			t.Errorf("the fallback answered with %+v, want the card that was just made", last)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the fallback never answered a change made while it waited")
	}
}

// A proposition somebody makes is a proposition they are on, so their other
// open tab hears about it rather than waiting for a reload. The membership row
// goes in with the proposition, not through a member command, because every
// later command on it is authorized against that row.
func TestCreatorsOtherTabSeesTheNewProposition(t *testing.T) {
	r := newRig(t)
	one := r.mustDial("grace")
	read(t, one, "presence")
	two := r.mustDial("grace")
	read(t, two, "presence")

	send(t, one, command{ID: 1, Cmd: "proposition.create", Args: args{Title: "Harbor Walls"}})
	made := read(t, one, "ack")
	if made.Event == nil {
		t.Fatalf("the create was answered with %+v", made)
	}

	for {
		e := read(t, two, "event")
		if e.Event.Entity != "proposition" {
			continue
		}
		if e.Event.EntityID != made.Event.EntityID {
			t.Fatalf("the other tab saw proposition %d, want %d", e.Event.EntityID, made.Event.EntityID)
		}
		var p board.Proposition
		if err := json.Unmarshal(e.Event.After, &p); err != nil {
			t.Fatal(err)
		}
		if p.Title != "Harbor Walls" || len(p.Members) != 1 || p.Members[0] != r.users["grace"].ID {
			t.Errorf("the other tab saw %+v", p)
		}
		break
	}

	// Somebody else's new proposition still does not reach them.
	stranger, err := r.dialProposition("stranger", 0)
	if err != nil {
		t.Fatalf("a tab with nothing open could not connect: %v", err)
	}
	defer stranger.Close()
	send(t, one, command{ID: 2, Cmd: "proposition.create", Args: args{Title: "Tide Tables"}})
	read(t, one, "ack")
	readNothing(t, stranger)
}

// Presence goes to everybody in the room, so what a tab says it has open takes
// the same cap as any other field. Without one a tab could put sixty four
// kilobytes in front of every other tab on the proposition.
func TestPresenceStringIsCapped(t *testing.T) {
	r := newRig(t)
	ws := r.mustDial("ada")
	read(t, ws, "presence")

	send(t, ws, command{ID: 1, Cmd: "where", Args: args{Where: strings.Repeat("x", board.MaxWord+1)}})
	if answer := read(t, ws, "error"); answer.ID != 1 || !strings.Contains(answer.Error, "longer than") {
		t.Errorf("an oversized presence string got %+v", answer)
	}

	send(t, ws, command{ID: 2, Cmd: "where", Args: args{Where: "card:7"}})
	found := false
	for i := 0; i < 3 && !found; i++ {
		for _, p := range read(t, ws, "presence").People {
			if p.Where == "card:7" {
				found = true
			}
		}
	}
	if !found {
		t.Error("a presence string within the cap never reached the room")
	}
}

// One person in two tabs is one person, shown wherever the tab that moved last
// is. A tab that has gone quiet, or one whose drawer was closed, must not take
// the block their other tab is standing in off the top bar and the margin. The
// list comes back in one order every time, whatever order the room is held in.
func TestPresenceShowsTheTabThatMovedLast(t *testing.T) {
	h := New(nil, nil, nil)
	person := &store.User{ID: 7, Name: "GRACE", Initials: "GH", Colour: "c1"}
	one := &client{hub: h, user: person}
	two := &client{hub: h, user: person}
	other := &client{hub: h, user: &store.User{ID: 2, Name: "ADA", Initials: "AL", Colour: "c2"}}
	h.rooms[1] = map[*client]struct{}{one: {}, two: {}, other: {}}
	other.moveTo("doc:1")

	for _, step := range []struct {
		name  string
		tab   *client
		where string
		want  string
	}{
		{"the first tab to say where it is", one, "doc:3", "doc:3"},
		{"the other tab moving takes it", two, "block:4:9:2:5", "block:4:9:2:5"},
		{"the first moving again takes it back", one, "block:8", "block:8"},
		{"a tab with nothing open leaves the other where it is", two, "", "block:8"},
		{"and the quiet tab can still move again", two, "card:2", "card:2"},
	} {
		step.tab.moveTo(step.where)
		people := h.Presence(1)
		if len(people) != 2 {
			t.Fatalf("%s: three tabs of two people are %d people", step.name, len(people))
		}
		if people[0].ID != 2 || people[1].ID != 7 {
			t.Fatalf("%s: presence came back as %d then %d, want them in order of id",
				step.name, people[0].ID, people[1].ID)
		}
		if people[1].Where != step.want {
			t.Errorf("%s: presence says %q, want %q", step.name, people[1].Where, step.want)
		}
	}
}

// A caret moves far more often than a person edits, so `where` spends an
// allowance of its own. Without one a person moving about a block would run
// the command allowance out and have their next save refused.
func TestCaretsDoNotSpendTheCommandAllowance(t *testing.T) {
	r := newRig(t)
	ws := r.mustDial("grace")
	read(t, ws, "presence")

	// More carets than a command allowance holds. Each is answered with an
	// announce rather than an answer of its own, and the frame is read back so
	// that the socket is not closed for falling behind.
	for i := 0; i < 320; i++ {
		send(t, ws, command{Cmd: "where", Args: args{Where: "block:4:9:2:2"}})
		read(t, ws, "presence")
	}

	// The command allowance is untouched, so the next command is answered on
	// its merits.
	send(t, ws, command{ID: 1, Cmd: "no.such.command"})
	if answer := read(t, ws, "error"); !strings.Contains(answer.Error, "no such command") {
		t.Fatalf("a command after a flood of carets got %q", answer.Error)
	}

	// And the command allowance still bites, on the same socket.
	limited := false
	for i := 0; i < 400 && !limited; i++ {
		send(t, ws, command{ID: int64(i + 2), Cmd: "no.such.command"})
		limited = strings.Contains(read(t, ws, "error").Error, "too many")
	}
	if !limited {
		t.Error("a tab sending four hundred commands was never held back")
	}
}

// quick shortens the heartbeat so a test does not wait a minute for it. The
// wait stays twenty intervals wide, because a runner that stalls for a moment
// under the race detector must not look like a tab that has gone.
func quick(h *Hub) {
	h.pingEvery, h.pongWait = 50*time.Millisecond, time.Second
}

// answering reads frames until one of the type wanted arrives, answering the
// heartbeat on the way. A test that waits out an interval has to keep its own
// socket answering or the server drops that one as well.
func answering(t *testing.T, ws *websocket.Conn, want string, within time.Duration) message {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		ws.SetReadDeadline(deadline)
		var raw string
		if err := websocket.Message.Receive(ws, &raw); err != nil {
			t.Fatalf("waiting for a %s frame: %v", want, err)
		}
		var m message
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatal(err)
		}
		if m.Type == "ping" {
			send(t, ws, command{Cmd: "pong"})
		}
		if m.Type == want {
			return m
		}
	}
}

// A tab that answers the heartbeat keeps its socket however long it sits there
// with nothing else to say. Nothing but the answers holds it open: the read
// deadline is shorter than the run below.
func TestAnsweredHeartbeatsHoldASocketOpen(t *testing.T) {
	r := newRig(t, quick)
	ws := r.mustDial("ada")

	// Forty intervals is two seconds, which is two read deadlines, and the
	// outer bound leaves room for a stall on a busy runner.
	const want = 40
	deadline := time.Now().Add(15 * time.Second)
	beats := 0
	for beats < want {
		ws.SetReadDeadline(deadline)
		var raw string
		if err := websocket.Message.Receive(ws, &raw); err != nil {
			t.Fatalf("the socket went after %d of %d heartbeats: %v", beats, want, err)
		}
		var m message
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatal(err)
		}
		if m.Type != "ping" {
			continue
		}
		beats++
		send(t, ws, command{Cmd: "pong"})
	}

	if people := r.hub.Presence(r.prop); len(people) != 1 || people[0].ID != r.users["ada"].ID {
		t.Fatalf("a tab answering the heartbeat is present as %+v", people)
	}
	send(t, ws, command{ID: 1, Cmd: "no.such.command"})
	if answer := read(t, ws, "error"); answer.ID != 1 {
		t.Errorf("the socket answered a command with %+v", answer)
	}
}

// A tab that has stopped answering is dropped rather than left standing in the
// room, and the others are told on the way out.
func TestSocketDropsATabThatStopsAnsweringTheHeartbeat(t *testing.T) {
	r := newRig(t, quick)
	watcher := r.mustDial("ada")
	answering(t, watcher, "presence", 5*time.Second)

	// This one reads nothing and answers nothing from here on, which is what a
	// closed laptop looks like from the server: the socket is still open and
	// the frames pile up in it.
	quiet := r.mustDial("grace")
	for {
		p := answering(t, watcher, "presence", 5*time.Second)
		if len(p.People) == 1 && p.People[0].ID == r.users["ada"].ID {
			break
		}
	}
	if people := r.hub.Presence(r.prop); len(people) != 1 || people[0].ID != r.users["ada"].ID {
		t.Fatalf("presence still shows %+v", people)
	}

	// The socket was closed, not merely left to go quiet: the frames already in
	// it are read out first, and then the read ends on the close.
	for {
		quiet.SetReadDeadline(time.Now().Add(5 * time.Second))
		var raw string
		err := websocket.Message.Receive(quiet, &raw)
		if err == nil {
			continue
		}
		if timedOut(err) {
			t.Fatal("a tab that stopped answering the heartbeat was left connected")
		}
		break
	}
}

// The answer to the heartbeat is not a command: a tab sitting quietly for hours
// would otherwise have spent its allowance on them and have its next save
// refused.
func TestPongsDoNotSpendTheCommandAllowance(t *testing.T) {
	r := newRig(t)
	ws := r.mustDial("grace")
	read(t, ws, "presence")

	// More answers than a command allowance holds.
	for i := 0; i < 400; i++ {
		send(t, ws, command{Cmd: "pong"})
	}
	// Not one of them is answered, with a refusal for going too fast or with
	// anything else. Without this the flood would come back as four hundred
	// unknown commands and the test below would read the first of those.
	if m, err := awaitFrame(t, ws, 600*time.Millisecond); err == nil {
		t.Fatalf("an answer to the heartbeat was answered with %+v", m)
	} else if !timedOut(err) {
		t.Fatalf("the socket went during the pongs: %v", err)
	}

	send(t, ws, command{ID: 1, Cmd: "no.such.command"})
	if answer := read(t, ws, "error"); !strings.Contains(answer.Error, "no such command") {
		t.Fatalf("a command after four hundred pongs got %q", answer.Error)
	}
}

// The limiter is the cheap check, so a tab sending rubbish as fast as it can
// does not buy a session lookup per frame. The refusal still carries the
// command's own number.
func TestTheLimiterRunsBeforeTheSessionLookup(t *testing.T) {
	r := newRig(t)
	ws := r.mustDial("grace")
	read(t, ws, "presence")

	limited := false
	for i := 0; i < 400 && !limited; i++ {
		send(t, ws, command{ID: int64(i + 1), Cmd: "no.such.command"})
		if strings.Contains(read(t, ws, "error").Error, "too many") {
			limited = true
		}
	}
	if !limited {
		t.Fatal("the limiter never answered")
	}

	// Spent, and now signed out everywhere: the limiter answers, and the
	// socket stays up because the session was never asked about.
	if err := store.BumpSessionEpoch(context.Background(), r.db, r.users["grace"].ID); err != nil {
		t.Fatal(err)
	}
	send(t, ws, command{ID: 999, Cmd: "no.such.command"})
	if answer := read(t, ws, "error"); !strings.Contains(answer.Error, "too many") {
		t.Errorf("a refused frame reached the session lookup: %+v", answer)
	}
}

// The same keyed frame twice is one card. A tab whose socket died with a create
// in the air cannot tell whether the server applied it, so it sends it again
// under the key it used the first time and is answered with what that key
// already did.
func TestAKeyedFrameSentTwiceMakesOneCard(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	ws := r.mustDial("ada")

	for i := int64(1); i <= 2; i++ {
		send(t, ws, command{ID: i, Cmd: "card.create", Key: "a-tab-key",
			Args: args{Column: r.cols[0].ID, Title: "Call the engineer"}})
	}
	first := read(t, ws, "ack")
	second := read(t, ws, "ack")
	if first.Event.Replayed {
		t.Error("the first ack says it was replayed")
	}
	if !second.Event.Replayed {
		t.Error("the second ack does not say it was replayed")
	}
	if second.Event.EntityID != first.Event.EntityID {
		t.Errorf("the second ack is card %d, want the first one, %d",
			second.Event.EntityID, first.Event.EntityID)
	}
	if n := r.cards(ctx); n != 1 {
		t.Fatalf("%d cards after the same frame twice, want 1", n)
	}

	// A key that is not one is the frame being malformed, said out loud rather
	// than dropped: a tab that meant to be safe against a repeat and was not
	// would otherwise never find out.
	send(t, ws, command{ID: 3, Cmd: "card.create", Key: "not a key",
		Args: args{Column: r.cols[0].ID, Title: "Book the studio"}})
	if m := read(t, ws, "error"); !strings.Contains(m.Error, "letters, digits") {
		t.Errorf("a malformed key was answered with %q", m.Error)
	}
	if n := r.cards(ctx); n != 1 {
		t.Errorf("a frame with a malformed key made a card: %d cards", n)
	}
}

// cards counts what is on the board, which is how a test says a command that
// arrived twice was applied once.
func (r *rig) cards(ctx context.Context) int {
	r.Helper()
	var n int
	if err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM cards WHERE proposition_id = ?`, r.prop).Scan(&n); err != nil {
		r.Fatal(err)
	}
	return n
}
