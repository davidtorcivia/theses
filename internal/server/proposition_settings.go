package server

import (
	"errors"
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

type columnRow struct {
	ID          int64
	Name        string
	Cards       int
	First, Last bool
}

type propositionMember struct {
	person
	On bool
}

func (s *Server) getPropositionSettings(w http.ResponseWriter, r *http.Request) {
	extra := map[string]any{}
	if r.URL.Query().Get("saved") != "" {
		extra["Notice"] = "Saved."
	}
	s.renderPropositionSettings(w, r, http.StatusOK, extra)
}

func (s *Server) renderPropositionSettings(w http.ResponseWriter, r *http.Request, status int, extra map[string]any) {
	ctx := r.Context()
	me := userOf(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.errorPage(w, r, http.StatusNotFound)
		return
	}
	readable, err := realtime.CanRead(ctx, s.db, me, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !readable {
		s.errorPage(w, r, http.StatusForbidden)
		return
	}
	p, err := board.GetProposition(ctx, s.db, id)
	if errors.Is(err, core.ErrNotFound) {
		s.errorPage(w, r, http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}

	users, err := store.ListUsers(ctx, s.db)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	members := make([]propositionMember, 0, len(users))
	for _, u := range users {
		m := propositionMember{person: person{ID: u.ID, Handle: u.Handle, Name: u.Name,
			Initials: u.Initials, Colour: colourClass(u.Colour), Role: u.Role}}
		for _, id := range p.Members {
			if id == u.ID {
				m.On = true
			}
		}
		members = append(members, m)
	}

	cols, err := board.ListColumns(ctx, s.db, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	b, err := board.Load(ctx, s.db, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	counts := map[int64]int{}
	for _, c := range b.Cards {
		counts[c.ColumnID]++
	}
	rows := make([]columnRow, 0, len(cols))
	for i, c := range cols {
		rows = append(rows, columnRow{ID: c.ID, Name: c.Name, Cards: counts[c.ID],
			First: i == 0, Last: i == len(cols)-1})
	}

	doc, err := board.GetDocumentSettings(ctx, s.db, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	// The page carries the same payload the board does, so the rail beside it,
	// the initials in the top bar and the palette are the same live ones.
	payload, err := s.shellPayload(r, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	statuses := make([]option, 0, 5)
	for _, st := range settings.Get[[]string](s.settings, "defaults.statuses") {
		statuses = append(statuses, option{Value: st, Label: st, On: st == p.Status})
	}

	data := s.page(r, number(p.Number)+" "+p.Title, merge(map[string]any{
		"Payload":  payload,
		"P":        p,
		"Num":      number(p.Number),
		"Episode":  deref(p.Episode),
		"Target":   deref(p.TargetDate),
		"Archived": p.ArchivedAt != nil,
		"Statuses": statuses,
		"Members":  members,
		"Columns":  rows,
		"Doc":      doc,
		"CanEdit":  auth.Can(me.Role, auth.CanEdit),
		"CanDel":   auth.Can(me.Role, auth.CanDelete),
	}, extra))
	s.render(w, r, status, "prop_settings.html", data)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// postPropositionSettings runs one section's save. Every branch is a core
// command, so the board watching this proposition sees the change arrive.
func (s *Server) postPropositionSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := userOf(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.errorPage(w, r, http.StatusNotFound)
		return
	}
	actor := core.Actor{Kind: core.KindUser, ID: me.ID, Name: me.Name}
	form := r.PostForm

	var refused error
	switch form.Get("do") {
	case "proposition":
		_, refused = s.board.EditProposition(ctx, actor, id,
			form.Get("title"), form.Get("statement"), form.Get("blurb"))
	case "schedule":
		if _, refused = s.board.SetStatus(ctx, actor, id, form.Get("status")); refused == nil {
			_, refused = s.board.Schedule(ctx, actor, id, form.Get("episode"), form.Get("target"))
		}
	case "members":
		refused = s.saveMembers(r, actor, id, form["member"])
	case "columns":
		refused = s.saveColumns(r, actor, id)
	case "document":
		_, refused = s.board.SetDocumentSettings(ctx, actor, id, board.DocumentSettings{
			OpenEditing: form.Get("open_editing") != "",
			History:     form.Get("history") != "",
			Publish:     form.Get("publish") != "",
		})
	case "archive":
		_, refused = s.board.ArchiveProposition(ctx, actor, id)
	case "restore":
		_, refused = s.board.RestoreProposition(ctx, actor, id)
	case "delete":
		if _, refused = s.board.DeleteProposition(ctx, actor, id); refused == nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
	default:
		s.errorPage(w, r, http.StatusNotFound)
		return
	}

	switch {
	case errors.Is(refused, core.ErrForbidden):
		s.errorPage(w, r, http.StatusForbidden)
	case errors.Is(refused, core.ErrNotFound):
		s.errorPage(w, r, http.StatusNotFound)
	case refused != nil:
		s.renderPropositionSettings(w, r, http.StatusUnprocessableEntity,
			map[string]any{"Error": refused.Error()})
	default:
		http.Redirect(w, r, "/p/"+strconv.FormatInt(id, 10)+"/settings?saved=1", http.StatusSeeOther)
	}
}

func (s *Server) saveMembers(r *http.Request, actor core.Actor, id int64, wanted []string) error {
	ctx := r.Context()
	p, err := board.GetProposition(ctx, s.db, id)
	if err != nil {
		return err
	}
	on := map[int64]bool{}
	for _, v := range wanted {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			on[n] = true
		}
	}
	was := map[int64]bool{}
	for _, m := range p.Members {
		was[m] = true
		if !on[m] {
			if _, err := s.board.RemoveMember(ctx, actor, id, m); err != nil {
				return err
			}
		}
	}
	for m := range on {
		if !was[m] {
			if _, err := s.board.AddMember(ctx, actor, id, m); err != nil {
				return err
			}
		}
	}
	return nil
}

// saveColumns applies the one control that was pressed, then the renames, so
// that a reorder and a retitle in the same submission both land.
func (s *Server) saveColumns(r *http.Request, actor core.Actor, id int64) error {
	ctx := r.Context()
	cols, err := board.ListColumns(ctx, s.db, id)
	if err != nil {
		return err
	}
	index := map[int64]int{}
	for i, c := range cols {
		index[c.ID] = i
	}
	form := r.PostForm

	if v := strings.TrimSpace(form.Get("add")); v != "" {
		if _, err := s.board.CreateColumn(ctx, actor, id, v); err != nil {
			return err
		}
	}
	if n, err := strconv.ParseInt(form.Get("remove"), 10, 64); err == nil {
		if _, err := s.board.DeleteColumn(ctx, actor, n); err != nil {
			return err
		}
	}
	// Up means after the one two places above; down means after the next one.
	if n, err := strconv.ParseInt(form.Get("up"), 10, 64); err == nil {
		after := int64(0)
		if i := index[n]; i >= 2 {
			after = cols[i-2].ID
		}
		if _, err := s.board.MoveColumn(ctx, actor, n, after); err != nil {
			return err
		}
	}
	if n, err := strconv.ParseInt(form.Get("down"), 10, 64); err == nil {
		if i := index[n]; i+1 < len(cols) {
			if _, err := s.board.MoveColumn(ctx, actor, n, cols[i+1].ID); err != nil {
				return err
			}
		}
	}
	for _, c := range cols {
		name := strings.TrimSpace(form.Get("name-" + strconv.FormatInt(c.ID, 10)))
		if name != "" && name != c.Name {
			if _, err := s.board.RenameColumn(ctx, actor, c.ID, name); err != nil {
				return err
			}
		}
	}
	return nil
}
