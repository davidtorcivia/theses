package server

import (
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/davidtorcivia/theses/internal/api"
	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/docs"
	"github.com/davidtorcivia/theses/internal/realtime"
	"github.com/davidtorcivia/theses/internal/settings"
	"github.com/davidtorcivia/theses/internal/store"
)

// person is an account as the workspace sees it. The color is the class the
// CSS carries, because a strict CSP refuses the style attribute the mockup used.
type person struct {
	ID       int64  `json:"id"`
	Handle   string `json:"handle"`
	Name     string `json:"name"`
	Initials string `json:"initials"`
	Colour   string `json:"colour"`
	Role     string `json:"role"`
}

// shell is the state the page is rendered with. Everything after it arrives on
// the websocket, and the same shape comes back from a reload.
type shell struct {
	Me             int64               `json:"me"`
	Users          []person            `json:"users"`
	Workspace      string              `json:"workspace"`
	Statuses       []string            `json:"statuses"`
	Questions      []string            `json:"questions"`
	QuestionLabels []string            `json:"question_labels"`
	Propositions   []board.Proposition `json:"propositions"`
	Open           int64               `json:"open"`
	Board          *board.Board        `json:"board"`
	Documents      []docs.Doc          `json:"documents"`
	Presence       []realtime.Person   `json:"presence"`
	Can            map[string]bool     `json:"can"`
}

func (s *Server) getShell(w http.ResponseWriter, r *http.Request) {
	s.workspacePage(w, r, 0)
}

func (s *Server) getProposition(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.errorPage(w, r, http.StatusNotFound)
		return
	}
	s.workspacePage(w, r, id)
}

func (s *Server) workspacePage(w http.ResponseWriter, r *http.Request, open int64) {
	state, err := s.shellState(r, open)
	if errors.Is(err, core.ErrNotFound) {
		s.errorPage(w, r, http.StatusNotFound)
		return
	}
	if errors.Is(err, core.ErrForbidden) {
		s.errorPage(w, r, http.StatusForbidden)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	payload, err := json.Marshal(state)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	title := settings.Get[string](s.settings, "workspace.name")
	for _, p := range state.Propositions {
		if p.ID == state.Open {
			title = strings.TrimSpace(number(p.Number) + " " + p.Title)
		}
	}
	// json.Marshal escapes < > and & into \u00xx, so no payload can close the
	// script element it sits in. The test renders a title that tries to.
	s.render(w, r, http.StatusOK, "shell.html", s.page(r, title, map[string]any{
		"Payload": template.JS(payload),
	}))
}

// shellPayload is the same state, ready to render into a script element. The
// per proposition settings page carries it for the rail and the presence.
func (s *Server) shellPayload(r *http.Request, open int64) (template.JS, error) {
	state, err := s.shellState(r, open)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	return template.JS(payload), nil
}

func (s *Server) shellState(r *http.Request, open int64) (*shell, error) {
	ctx := r.Context()
	me := userOf(r)

	users, err := store.ListUsers(ctx, s.db)
	if err != nil {
		return nil, err
	}
	state := &shell{
		Me:             me.ID,
		Users:          make([]person, 0, len(users)),
		Workspace:      settings.Get[string](s.settings, "workspace.name"),
		Statuses:       settings.Get[[]string](s.settings, "defaults.statuses"),
		Questions:      board.Questions,
		QuestionLabels: settings.Get[[]string](s.settings, "defaults.question_labels"),
		Presence:       []realtime.Person{},
		Can: map[string]bool{
			"edit":     auth.Can(me.Role, auth.CanEdit),
			"delete":   auth.Can(me.Role, auth.CanDelete),
			"settings": auth.Can(me.Role, auth.CanSettings),
		},
	}
	for _, u := range users {
		state.Users = append(state.Users, person{ID: u.ID, Handle: u.Handle, Name: u.Name,
			Initials: u.Initials, Colour: colourClass(u.Colour), Role: u.Role})
	}

	all, err := board.ListPropositions(ctx, s.db)
	if err != nil {
		return nil, err
	}
	member, err := board.Memberships(ctx, s.db, me.ID)
	if err != nil {
		return nil, err
	}
	state.Propositions = visibleTo(me, member, all)

	if open == 0 {
		state.Open = firstOpen(state.Propositions)
	} else {
		state.Open = open
	}
	if state.Open == 0 {
		// Nothing open, either because there is nothing yet or because every
		// proposition is archived. The rail still lists what there is, so an
		// archived one can be restored.
		return state, nil
	}

	ok, err := board.Readable(ctx, s.db, me, state.Open)
	if err != nil {
		return nil, err
	}
	if !ok {
		// A proposition somebody is not a member of and one that does not
		// exist answer the same way, because the rule is that they are not
		// told it is there. An owner reads everything that exists, so for them
		// this is only ever the second case.
		return nil, core.ErrNotFound
	}

	b, err := board.Load(ctx, s.db, state.Open)
	if err != nil {
		return nil, err
	}
	state.Board = &b
	if state.Documents, err = docs.Load(ctx, s.db, state.Open); err != nil {
		return nil, err
	}
	if s.hub != nil {
		state.Presence = s.hub.Presence(state.Open)
	}
	return state, nil
}

// visibleTo is the rail, and it is the same test the socket and the settings
// page use: an owner reads every proposition, everybody else reads the ones
// they are a member of and is not told the others exist.
func visibleTo(me *store.User, member map[int64]bool, all []board.Proposition) []board.Proposition {
	if me.Role == auth.RoleOwner {
		return all
	}
	out := []board.Proposition{}
	for _, p := range all {
		if member[p.ID] {
			out = append(out, p)
		}
	}
	return out
}

func firstOpen(props []board.Proposition) int64 {
	for _, p := range props {
		if p.ArchivedAt == nil {
			return p.ID
		}
	}
	return 0
}

// number is the two digit form the rail and the title use.
func number(n int64) string {
	if n < 10 {
		return "0" + strconv.FormatInt(n, 10)
	}
	return strconv.FormatInt(n, 10)
}

// propositionEvents is the long poll fallback at the path the plan names,
// behind a bearer token with the read scope. The browser uses the socket, and
// /api/events with its session cookie when it cannot hold one; this is the
// same stream for anything holding a token.
func (s *Server) propositionEvents(w http.ResponseWriter, r *http.Request) {
	p, ok := api.PrincipalFrom(r.Context())
	if !ok {
		s.fail(w, r, errors.New("the events route is not behind the API's authentication"))
		return
	}
	if why := p.Deny(auth.ScopeRead); why != "" {
		writeAPIError(w, http.StatusForbidden, why)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "no such proposition")
		return
	}
	s.hub.EventsFor(w, r, p.User, id)
}

// writeAPIError answers in the shape every other /api/v1 refusal uses.
func writeAPIError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
