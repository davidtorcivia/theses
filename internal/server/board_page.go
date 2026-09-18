package server

import (
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/realtime"
	"github.com/davidtorcivia/theses/internal/settings"
	"github.com/davidtorcivia/theses/internal/store"
)

// person is an account as the workspace sees it. The colour is the class the
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
		// A proposition that exists but is not this person's, and one that does
		// not exist, answer the same way to anyone who is not an owner.
		var exists int
		if err := s.db.QueryRowContext(ctx,
			`SELECT count(*) FROM propositions WHERE id = ?`, state.Open).Scan(&exists); err != nil {
			return nil, err
		}
		if exists == 0 {
			return nil, core.ErrNotFound
		}
		return nil, core.ErrForbidden
	}

	b, err := board.Load(ctx, s.db, state.Open)
	if err != nil {
		return nil, err
	}
	state.Board = &b
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
