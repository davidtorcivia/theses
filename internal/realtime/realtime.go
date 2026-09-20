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
	"strconv"
	"sync"
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

type Hub struct {
	board *board.Service
	auth  *auth.Auth
	log   *slog.Logger
	// Docs is the document service, set by the server after New.
	Docs *docs.Service

	mu    sync.Mutex
	rooms map[int64]map[*client]struct{}
	tabs  int64
}

func New(b *board.Service, a *auth.Auth, log *slog.Logger) *Hub {
	return &Hub{board: b, auth: a, log: log, rooms: map[int64]map[*client]struct{}{}}
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
type command struct {
	ID   int64  `json:"id"`
	Cmd  string `json:"cmd"`
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
	// Whole is a save made while somebody is typing: the block takes the text
	// exactly as it was sent rather than trimmed and cut into paragraphs.
	Whole bool `json:"whole"`
}

type client struct {
	hub         *Hub
	ws          *websocket.Conn
	user        *store.User
	proposition int64
	out         chan []byte
	done        chan struct{}
	closeOnce   sync.Once
	// tab names this socket to the rate limiter, so one tab in a loop cannot
	// spend the allowance of the person's other tabs.
	tab string

	mu     sync.Mutex
	where  string
	member map[int64]bool
	owner  bool
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
		out: make(chan []byte, outBuffer), done: make(chan struct{}),
		member: member, owner: user.Role == auth.RoleOwner, tab: tab}
	defer c.close()
	go c.write()

	go c.forward(sub)

	h.join(c)
	defer h.leave(c)
	go h.watchSession(c, r)

	for {
		var raw string
		if err := websocket.Message.Receive(ws, &raw); err != nil {
			return
		}
		// The limiter first, because it is the cheap check and a flood of
		// rubbish should not buy a session query per frame. The frame was
		// capped before it was read, so parsing one to answer under its own
		// number costs nothing either.
		allowed := h.auth.Allow(auth.BucketSocket, c.tab, strconv.FormatInt(c.user.ID, 10))
		var cmd command
		if err := json.Unmarshal([]byte(raw), &cmd); err != nil {
			c.send(message{Type: "error", Error: "that was not a command"})
			continue
		}
		if !allowed {
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

// forward carries the bus to one tab. A drop means this tab's view has a hole
// in it, and it is told so there and then: at teardown the socket is already
// gone and the frame goes nowhere.
func (c *client) forward(sub *core.Subscription) {
	var dropped int64
	for e := range sub.C {
		if d := sub.Dropped(); d > dropped {
			dropped = d
			c.send(message{Type: "gap"})
		}
		c.membership(e)
		if c.wants(e) {
			c.send(message{Type: "event", Event: &e})
		}
	}
}

// membership keeps the set of propositions this tab may read in step with the
// member commands as they happen, and closes the socket when this person is
// removed from the one they have open, so a removed member stops receiving on
// the same command that removed them.
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
	owner := c.owner
	c.mu.Unlock()
	if !added && !owner && e.Proposition == c.proposition {
		c.close()
	}
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
	for {
		select {
		case b := <-c.out:
			if err := websocket.Message.Send(c.ws, string(b)); err != nil {
				c.close()
				return
			}
		case <-c.done:
			return
		}
	}
}

func (c *client) send(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	select {
	case <-c.done:
	case c.out <- b:
	default:
		// The tab has stopped draining its socket, so there is nothing to wait
		// for. Closing it makes the page reconnect and reload the board.
		c.close()
	}
}

func (c *client) close() {
	c.closeOnce.Do(func() {
		close(c.done)
		c.ws.Close()
	})
}

func (h *Hub) join(c *client) {
	h.mu.Lock()
	if h.rooms[c.proposition] == nil {
		h.rooms[c.proposition] = map[*client]struct{}{}
	}
	h.rooms[c.proposition][c] = struct{}{}
	h.mu.Unlock()
	h.announce(c.proposition)
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
// person in two tabs is one person, showing whichever of them moved last.
func (h *Hub) Presence(proposition int64) []Person {
	h.mu.Lock()
	defer h.mu.Unlock()
	byUser := map[int64]Person{}
	order := []int64{}
	for c := range h.rooms[proposition] {
		c.mu.Lock()
		where := c.where
		c.mu.Unlock()
		if _, seen := byUser[c.user.ID]; !seen {
			order = append(order, c.user.ID)
		}
		p := Person{ID: c.user.ID, Name: c.user.Name, Initials: c.user.Initials, Colour: c.user.Colour}
		if where != "" {
			p.Where = where
		} else if seen, ok := byUser[c.user.ID]; ok {
			p.Where = seen.Where
		}
		byUser[c.user.ID] = p
	}
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
