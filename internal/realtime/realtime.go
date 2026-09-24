// Package realtime is the one websocket per tab. It carries presence, the
// events every applied command publishes on the bus, and the commands a tab
// sends, which are dispatched to the same board functions an HTTP handler or an
// MCP tool would call, with the session's actor.
package realtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/websocket"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/docs"
	"github.com/davidtorcivia/theses/internal/store"
)

// outBuffer is how many messages a tab may be behind before it is cut off. A
// tab that far behind is one whose socket has stopped draining.
const outBuffer = 64

// pollWait is how long the fallback holds a request open with nothing to say.
const pollWait = 25 * time.Second

// maxFrame is the largest command a tab may send. Every one of them is a
// short JSON object; a description is the longest field in one and the board
// caps that well below this.
const maxFrame = 64 << 10

// sessionCheck is how often an idle socket re-reads the session behind it. A
// tab that is doing something is checked on every command as well. It is a
// variable so a test does not have to wait a minute for it.
var sessionCheck = time.Minute

// ping is the heartbeat, and the server is what originates it. A protocol ping
// cannot be the heartbeat, because this library answers one and discards a pong
// inside Receive rather than returning either, so a pong can never push out a
// read deadline set around that call. A browser timer cannot be it either: a
// hidden tab's timers are throttled to one a minute. This frame is answered
// from the tab's message handler, which runs when the frame arrives.
const ping = `{"type":"ping"}`

type Hub struct {
	board *board.Service
	auth  *auth.Auth
	log   *slog.Logger
	// Docs is the document service, set by the server after New.
	Docs *docs.Service

	// pingEvery is how often the heartbeat goes out, pongWait how long a socket
	// may go without a frame, and writeWait how long its peer may stop reading.
	// Fields so that a test does not wait a minute for them.
	pingEvery, pongWait, writeWait time.Duration

	// moves numbers the `where` frames of every tab on this server in the order
	// they arrive, so Presence can tell which of a person's tabs moved last
	// without a clock two of them could read the same value from.
	moves atomic.Uint64

	mu     sync.Mutex
	rooms  map[int64]map[*client]struct{}
	tabs   int64
	closed bool
	// stop is closed by Close, which answers every held poll at once.
	stop chan struct{}
}

func New(b *board.Service, a *auth.Auth, log *slog.Logger) *Hub {
	return &Hub{board: b, auth: a, log: log, rooms: map[int64]map[*client]struct{}{}, stop: make(chan struct{}),
		pingEvery: 25 * time.Second, pongWait: time.Minute, writeWait: 10 * time.Second}
}

// Person is one tab's occupant as the other tabs see them.
type Person struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Initials string `json:"initials"`
	Colour   string `json:"colour"`
	Where    string `json:"where"`
}

// message is every frame the server sends. Only one of the payloads is set.
type message struct {
	Type     string              `json:"type"`
	ID       int64               `json:"id,omitempty"`
	Event    *core.Event         `json:"event,omitempty"`
	Conflict *core.ConflictError `json:"conflict,omitempty"`
	Error    string              `json:"error,omitempty"`
	People   []Person            `json:"people,omitempty"`
}

// command is every frame a tab sends. ID is the tab's own request number, which
// comes back on the answer so an optimistic change knows which reply is its own.
//
// Key names the change rather than the attempt: a tab that sends the same
// command again, because its socket went while the command was in the air,
// sends it under the key it used the first time, and the server answers with
// what that key already did rather than doing it twice. It is optional, so a
// frame without one is applied as it always was.
type command struct {
	ID   int64  `json:"id"`
	Cmd  string `json:"cmd"`
	Key  string `json:"key"`
	Args args   `json:"args"`
}

// args is one flat struct for every command rather than one struct each: the
// commands share most of their fields and none of them is ambiguous.
type args struct {
	Proposition int64   `json:"proposition"`
	Card        int64   `json:"card"`
	Column      int64   `json:"column"`
	Item        int64   `json:"item"`
	Comment     int64   `json:"comment"`
	User        int64   `json:"user"`
	After       int64   `json:"after"`
	Activity    int64   `json:"activity"`
	Base        int64   `json:"base"`
	Assignees   []int64 `json:"assignees"`
	Text        string  `json:"text"`
	Title       string  `json:"title"`
	Statement   string  `json:"statement"`
	Blurb       string  `json:"blurb"`
	Status      string  `json:"status"`
	Episode     string  `json:"episode"`
	Target      string  `json:"target"`
	Due         string  `json:"due"`
	Question    string  `json:"question"`
	Done        bool    `json:"done"`
	Where       string  `json:"where"`
	Document    int64   `json:"document"`
	Block       int64   `json:"block"`
	// AfterKey is where an insert goes when the block it goes after has no id
	// yet: the key the command making that block was sent under. A tab with no
	// connection draws the block it has just made and queues the command that
	// makes it, so a second block below the first has only that key to name.
	AfterKey string `json:"after_key"`
	// Whole is a save made while somebody is typing: the block takes the text
	// exactly as it was sent rather than trimmed and cut into paragraphs.
	Whole bool `json:"whole"`
}

type client struct {
	hub         *Hub
	ws          *websocket.Conn
	user        *store.User
	proposition int64
	out         chan outbound
	done        chan struct{}
	closeOnce   sync.Once
	// tab names this socket to the rate limiter, so one tab in a loop cannot
	// spend the allowance of the person's other tabs.
	tab string

	mu     sync.Mutex
	where  string
	moved  uint64
	member map[int64]bool
	owner  bool
}

type outbound struct {
	body       []byte
	closeAfter bool
}

// moveTo records what this tab has open and where in the order of everything
// this server has been told. Both fields are written together, because one
// without the other is a tab whose place is known and whose turn is not.
func (c *client) moveTo(where string) {
	n := c.hub.moves.Add(1)
	c.mu.Lock()
	c.where = where
	c.moved = n
	c.mu.Unlock()
}

// Handler is the websocket endpoint. Everything is refused in the handshake
// rather than after it, so a tab that may not be here is told 403 instead of
// being handed a socket that closes on it.
func (h *Hub) Handler() http.Handler {
	return websocket.Server{Handshake: h.handshake, Handler: h.serve}
}

func (h *Hub) handshake(config *websocket.Config, r *http.Request) error {
	// The default handshake only checks that the origin parses. A session
	// cookie is sent on a websocket request from any page, so without
	// comparing the origin to the host, any site could open a socket that
	// writes to the board.
	origin, err := websocket.Origin(config, r)
	if err != nil {
		return err
	}
	config.Origin = origin
	if origin == nil {
		return errors.New("no origin")
	}
	if origin.Host != r.Host {
		return fmt.Errorf("origin %s is not %s", origin.Host, r.Host)
	}
	user, err := h.auth.SessionUser(r.Context(), r)
	if err != nil {
		return errors.New("no session")
	}
	// Proposition zero is the empty workspace: a rail and nothing open yet. The
	// socket still has to exist, because creating the first proposition goes
	// through it. Such a tab is sent only the rail events its role allows.
	proposition, _ := strconv.ParseInt(r.URL.Query().Get("proposition"), 10, 64)
	if proposition == 0 {
		return nil
	}
	ok, err := board.Readable(r.Context(), h.board.DB, user, proposition)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no access to proposition %d", proposition)
	}
	return nil
}

func (h *Hub) serve(ws *websocket.Conn) {
	defer ws.Close()
	r := ws.Request()
	ctx := r.Context()

	// The handshake has already refused anyone who may not be here; this reads
	// the person back, because a handshake cannot hand anything to the handler.
	user, err := h.auth.SessionUser(ctx, r)
	if err != nil {
		return
	}
	proposition, _ := strconv.ParseInt(r.URL.Query().Get("proposition"), 10, 64)

	// The subscription takes every proposition so that the rail stays live, and
	// it is opened before the memberships are read: a member command in
	// between is then waiting on the subscription rather than missed by both.
	sub := h.board.Bus.Subscribe(0)
	defer sub.Close()

	member, err := board.Memberships(ctx, h.board.DB, user.ID)
	if err != nil {
		return
	}
	ws.MaxPayloadBytes = maxFrame
	h.mu.Lock()
	h.tabs++
	tab := strconv.FormatInt(h.tabs, 10)
	h.mu.Unlock()
	c := &client{hub: h, ws: ws, user: user, proposition: proposition,
		out: make(chan outbound, outBuffer), done: make(chan struct{}),
		member: member, owner: user.Role == auth.RoleOwner, tab: tab}
	defer c.close()
	go c.write()

	go c.forward(sub)

	h.join(c)
	defer h.leave(c)
	go h.watchSession(c, r)

	for {
		var raw string
		// Every frame a tab sends, its answer to the heartbeat included, pushes
		// the deadline out. A tab that has stopped answering, which is a closed
		// laptop or a phone off the network, ends the loop here instead of
		// holding its place in the room until TCP gives up on it.
		ws.SetReadDeadline(time.Now().Add(h.pongWait))
		if err := websocket.Message.Receive(ws, &raw); err != nil {
			return
		}
		// The frame is parsed first, because which allowance it spends depends
		// on what it is, and the frame was capped before it was read, so
		// parsing one to answer under its own number costs nothing. A frame
		// that is not a command at all spends the command allowance, so that a
		// flood of rubbish is still held back. Both come before the session
		// query, which is the expensive check.
		var cmd command
		bad := json.Unmarshal([]byte(raw), &cmd) != nil
		if !bad && cmd.Cmd == "pong" {
			// The answer to the heartbeat did its work by arriving: the
			// deadline above is already pushed out. It spends no allowance,
			// because the server is what asked for it, and it is answered with
			// nothing.
			continue
		}
		bucket := auth.BucketSocket
		keys := []string{c.tab, strconv.FormatInt(c.user.ID, 10)}
		if !bad && cmd.Cmd == "where" {
			bucket, keys = auth.BucketPresence, []string{c.tab}
		}
		allowed := h.auth.Allow(bucket, keys...)
		if bad {
			c.send(message{Type: "error", Error: "that was not a command"})
			continue
		}
		if !allowed {
			// A caret over its allowance is dropped in silence: nothing on the
			// page changed, the tab has nothing to do about it, and the next
			// one it sends says the same thing a moment later.
			if bucket == auth.BucketPresence {
				continue
			}
			c.send(message{Type: "error", ID: cmd.ID, Error: "too many changes at once; wait a moment"})
			continue
		}
		// The handshake is not enough on a connection that stays open for
		// hours: a sign out, a sign out everywhere or a deleted account has to
		// stop the writes it was authorizing.
		if !h.stillSignedIn(c, r) {
			return
		}
		h.dispatch(ctx, c, cmd)
	}
}

// wants is what this tab may see: everything on the proposition it has open,
// plus the rail entries for the other propositions it may read. A person who
// is not a member of a proposition is not told it exists, let alone edited.
func (c *client) wants(e core.Event) bool {
	if e.Entity != "proposition" && e.Entity != "member" && e.Proposition != c.proposition {
		return false
	}
	return c.canRead(e.Proposition)
}

// forward carries the bus to one tab. Reconnecting after a drop reloads both
// missed rows and the memberships that decide which rows it may receive.
func (c *client) forward(sub *core.Subscription) {
	for e := range sub.C {
		if sub.Dropped() > 0 {
			// A dropped membership event can leave cached authorization too wide;
			// reconnecting rebuilds it before another event is considered.
			c.close()
			return
		}
		terminal := e.Entity == "proposition" && e.Action == "delete" ||
			e.Entity == "member" && e.EntityID == c.user.ID && len(e.After) == 0
		if terminal {
			allowed := c.wants(e)
			closeAfter := c.revokesOpen(e.Proposition)
			c.membership(e)
			if allowed {
				m := message{Type: "event", Event: &e}
				if closeAfter {
					c.sendFinal(m)
					return
				}
				c.send(m)
			} else if closeAfter {
				c.close()
				return
			}
			continue
		}
		c.membership(e)
		if c.wants(e) {
			c.send(message{Type: "event", Event: &e})
		}
	}
}

// membership keeps the set of propositions this tab may read in step with the
// member commands as they happen.
func (c *client) membership(e core.Event) {
	// A proposition somebody makes is a proposition they are on. The row that
	// says so is written with the proposition rather than by a member command,
	// so this is where another tab of theirs learns about it, and it has to
	// happen before the event is filtered or the tab would never see the
	// proposition it just made.
	if e.Entity == "proposition" && e.Action == "create" && e.Actor.ID == c.user.ID {
		c.mu.Lock()
		c.member[e.Proposition] = true
		c.mu.Unlock()
		return
	}
	if e.Entity == "proposition" && e.Action == "delete" {
		c.mu.Lock()
		delete(c.member, e.Proposition)
		c.mu.Unlock()
		return
	}
	if e.Entity != "member" || e.EntityID != c.user.ID {
		return
	}
	added := len(e.After) > 0
	c.mu.Lock()
	if added {
		c.member[e.Proposition] = true
	} else {
		delete(c.member, e.Proposition)
	}
	c.mu.Unlock()
}

func (c *client) revokesOpen(proposition int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.owner && proposition == c.proposition
}

func (c *client) canRead(proposition int64) bool {
	if proposition == 0 {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.owner || c.member[proposition]
}

// watchSession closes an idle socket once the session behind it is gone, so a
// tab left open on a signed out account stops receiving as well as writing.
func (h *Hub) watchSession(c *client, r *http.Request) {
	ticker := time.NewTicker(sessionCheck)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			if !h.stillSignedIn(c, r) {
				return
			}
		}
	}
}

// stillSignedIn re-reads the session and keeps the client's standing current,
// so a demotion takes away the rail the old role reached.
func (h *Hub) stillSignedIn(c *client, r *http.Request) bool {
	user, err := h.auth.SessionUser(r.Context(), r)
	if err != nil || user.ID != c.user.ID {
		c.close()
		return false
	}
	c.mu.Lock()
	c.owner = user.Role == auth.RoleOwner
	c.mu.Unlock()
	return true
}

func (c *client) write() {
	beat := time.NewTicker(c.hub.pingEvery)
	defer beat.Stop()
	for {
		var out outbound
		select {
		case out = <-c.out:
		case <-beat.C:
			// The heartbeat goes out from here rather than through send,
			// because this goroutine is the socket's only writer and because a
			// frame the server owes itself must not be what pushes a tab that
			// is already behind over the buffer and closes it.
			out.body = []byte(ping)
		case <-c.done:
			return
		}
		if err := c.ws.SetWriteDeadline(time.Now().Add(c.hub.writeWait)); err != nil {
			c.close()
			return
		}
		if err := websocket.Message.Send(c.ws, string(out.body)); err != nil {
			c.close()
			return
		}
		if out.closeAfter {
			c.close()
			return
		}
	}
}

func (c *client) send(v any) {
	c.queue(v, false)
}

func (c *client) sendFinal(v any) {
	c.queue(v, true)
}

func (c *client) queue(v any, closeAfter bool) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	select {
	case <-c.done:
	case c.out <- outbound{body: b, closeAfter: closeAfter}:
	default:
		// The tab has stopped draining its socket, so there is nothing to wait
		// for. Closing it makes the page reconnect and reload the board.
		c.close()
	}
}

func (c *client) close() {
	c.closeOnce.Do(func() {
		close(c.done)
		if c.ws != nil {
			c.ws.Close()
		}
	})
}

func (h *Hub) join(c *client) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		c.close()
		return
	}
	if h.rooms[c.proposition] == nil {
		h.rooms[c.proposition] = map[*client]struct{}{}
	}
	h.rooms[c.proposition][c] = struct{}{}
	h.mu.Unlock()
	h.announce(c.proposition)
}

// Close ends every socket, refuses new ones and answers every held poll.
// http.Server.Shutdown does not wait for hijacked connections, and a socket
// left open would go on applying commands after the watchers that match them
// to notifications have stopped; a poll held for pollWait would outlast the
// shutdown's own deadline.
func (h *Hub) Close() {
	h.mu.Lock()
	if !h.closed {
		close(h.stop)
	}
	h.closed = true
	var all []*client
	for _, room := range h.rooms {
		for c := range room {
			all = append(all, c)
		}
	}
	h.mu.Unlock()
	for _, c := range all {
		c.close()
	}
}

func (h *Hub) leave(c *client) {
	h.mu.Lock()
	if room := h.rooms[c.proposition]; room != nil {
		delete(room, c)
		if len(room) == 0 {
			delete(h.rooms, c.proposition)
		}
	}
	h.mu.Unlock()
	h.announce(c.proposition)
}

// Presence is who is on a proposition and what they have open, which is what
// the initials in the top bar and the marker beside a card are drawn from. One
// person in two tabs is one person, showing whichever of them moved last, and
// the answer is in one order however often it is asked for.
func (h *Hub) Presence(proposition int64) []Person {
	h.mu.Lock()
	defer h.mu.Unlock()
	byUser := map[int64]Person{}
	// moved is the turn the place each person is shown at was taken from, so a
	// tab that has not moved since cannot take it back off the one that did.
	moved := map[int64]uint64{}
	order := []int64{}
	for c := range h.rooms[proposition] {
		c.mu.Lock()
		where, at := c.where, c.moved
		c.mu.Unlock()
		p, seen := byUser[c.user.ID]
		if !seen {
			order = append(order, c.user.ID)
			p = Person{ID: c.user.ID, Name: c.user.Name, Initials: c.user.Initials, Colour: c.user.Colour}
		}
		// A tab with nothing open says nothing about where its person is: the
		// drawer they closed must not take the block their other tab is in.
		if where != "" && at > moved[c.user.ID] {
			p.Where = where
			moved[c.user.ID] = at
		}
		byUser[c.user.ID] = p
	}
	// By id, because the order a Go map ranges in is fresh every time: the
	// initials in the top bar would reshuffle on every frame, and a block two
	// people are in would take first one colour and then the other.
	slices.Sort(order)
	out := make([]Person, 0, len(order))
	for _, id := range order {
		out = append(out, byUser[id])
	}
	return out
}

func (h *Hub) announce(proposition int64) {
	people := h.Presence(proposition)
	h.mu.Lock()
	room := make([]*client, 0, len(h.rooms[proposition]))
	for c := range h.rooms[proposition] {
		room = append(room, c)
	}
	h.mu.Unlock()
	for _, c := range room {
		c.send(message{Type: "presence", People: people})
	}
}

// Events is the fallback for a network that will not hold a socket, for the
// browser: the same stream, read out of the activity table, one request at a
// time, with the session cookie the page already has. A token reaches the same
// stream through the API, at the path the plan names, and lands in EventsFor.
func (h *Hub) Events(w http.ResponseWriter, r *http.Request) {
	user, err := h.auth.SessionUser(r.Context(), r)
	if err != nil {
		http.Error(w, "sign in", http.StatusUnauthorized)
		return
	}
	proposition, _ := strconv.ParseInt(r.URL.Query().Get("proposition"), 10, 64)
	h.EventsFor(w, r, user, proposition)
}

// EventsFor is the same fallback for a caller whose identity came from
// somewhere other than a cookie, which is the API's bearer token. Whichever
// way in, the proposition is readable only through membership.
func (h *Hub) EventsFor(w http.ResponseWriter, r *http.Request, user *store.User, proposition int64) {
	ctx := r.Context()
	if ok, err := board.Readable(ctx, h.board.DB, user, proposition); err != nil || !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such proposition"})
		return
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)

	// Subscribing before the query, not after it, so an event that lands while
	// the query runs is waiting rather than missed.
	sub := h.board.Bus.Subscribe(proposition)
	defer sub.Close()

	events, err := h.board.Since(ctx, proposition, since, 200)
	if err != nil {
		http.Error(w, "unavailable", http.StatusInternalServerError)
		return
	}
	if len(events) == 0 && r.URL.Query().Get("wait") != "0" {
		// Nothing yet, so hold the request open until something happens or the
		// browser would give up on it anyway. A tab catching up after a
		// reconnect asks with wait=0 and takes the empty answer.
		timer := time.NewTimer(pollWait)
		defer timer.Stop()
		select {
		case e := <-sub.C:
			events = append(events, e)
		case <-timer.C:
		case <-h.stop:
		case <-ctx.Done():
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}
