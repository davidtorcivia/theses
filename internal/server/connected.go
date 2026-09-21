package server

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/markdown"
)

type workItem struct {
	ID          int64  `json:"id"`
	Proposition int64  `json:"proposition_id"`
	Kind        string `json:"kind"`
	Title       string `json:"title"`
	Text        string `json:"text,omitempty"`
	Due         string `json:"due_date,omitempty"`
	URL         string `json:"url"`
}

func (s *Server) getMyWork(w http.ResponseWriter, r *http.Request) {
	me := userOf(r)
	rows, err := s.db.QueryContext(r.Context(), `SELECT c.id,c.proposition_id,c.title,coalesce(c.due_date,'')
 FROM cards c JOIN propositions p ON p.id=c.proposition_id
 WHERE c.done_at IS NULL AND p.archived_at IS NULL
 AND EXISTS (SELECT 1 FROM card_assignees a WHERE a.card_id=c.id AND a.user_id=?)
 AND (? OR EXISTS (SELECT 1 FROM proposition_members m WHERE m.proposition_id=c.proposition_id AND m.user_id=?))
 ORDER BY c.due_date IS NULL,c.due_date,c.id`, me.ID, me.Role == auth.RoleOwner, me.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer rows.Close()
	out := []workItem{}
	for rows.Next() {
		item := workItem{Kind: "card"}
		if err := rows.Scan(&item.ID, &item.Proposition, &item.Title, &item.Due); err != nil {
			s.fail(w, r, err)
			return
		}
		item.URL = fmt.Sprintf("/p/%d#card-%d", item.Proposition, item.ID)
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"items": out})
}

func (s *Server) getBacklinks(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.URL.Query().Get("proposition"), 10, 64)
	ok, err := board.Readable(r.Context(), s.db, userOf(r), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	me := userOf(r)
	// ponytail: scan token candidates; add a derived index if measured workspace size requires it.
	rows, err := s.db.QueryContext(r.Context(), `SELECT kind,id,proposition_id,title,body FROM (
 SELECT 'card' kind,c.id,c.proposition_id,c.title,c.title||char(10)||c.description_md body FROM cards c
 UNION ALL SELECT 'block',b.id,d.proposition_id,d.name,b.text FROM blocks b JOIN documents d ON d.id=b.document_id WHERE b.deleted_at IS NULL
 ) WHERE instr(body,?)>0 AND (? OR proposition_id IN (SELECT proposition_id FROM proposition_members WHERE user_id=?)) ORDER BY proposition_id,kind,id`, fmt.Sprintf("@[p:%d]", id), me.Role == auth.RoleOwner, me.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer rows.Close()
	out := []workItem{}
	for rows.Next() {
		var item workItem
		if err := rows.Scan(&item.Kind, &item.ID, &item.Proposition, &item.Title, &item.Text); err != nil {
			s.fail(w, r, err)
			return
		}
		if !slices.Contains(markdown.References(item.Text), id) {
			continue
		}
		item.URL = fmt.Sprintf("/p/%d#%s-%d", item.Proposition, item.Kind, item.ID)
		// Send only the visible source passage; the browser resolves other references through its ACL-filtered proposition list.
		if chars := []rune(item.Text); len(chars) > 400 {
			item.Text = string(chars[:400]) + "…"
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"items": out})
}

func (s *Server) getProduction(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.URL.Query().Get("proposition"), 10, 64)
	ok, err := board.Readable(r.Context(), s.db, userOf(r), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	var cardID int64
	if err := s.db.QueryRowContext(r.Context(), `SELECT coalesce(card_id,0) FROM production_templates WHERE proposition_id=?`, id).Scan(&cardID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		s.fail(w, r, err)
		return
	}
	var card *board.Card
	if cardID > 0 {
		row, err := board.GetCard(r.Context(), s.db, cardID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		card = &row
	}
	var recordings int
	if err := s.db.QueryRowContext(r.Context(), `SELECT count(*) FROM files WHERE proposition_id=? AND folder='Recordings' AND state='ready'`, id).Scan(&recordings); err != nil {
		s.fail(w, r, err)
		return
	}
	plan, err := board.GetProductionPlan(r.Context(), s.db, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"card": card, "recordings": recordings, "plan": plan})
}

func (s *Server) getProductionPlans(w http.ResponseWriter, r *http.Request) {
	me := userOf(r)
	rows, err := s.db.QueryContext(r.Context(), `SELECT pp.proposition_id,pp.owner_id,pp.next_action,pp.blocker,pp.record_date,pp.edit_date,pp.version FROM production_plans pp JOIN propositions p ON p.id=pp.proposition_id WHERE p.archived_at IS NULL AND (? OR EXISTS(SELECT 1 FROM proposition_members m WHERE m.proposition_id=p.id AND m.user_id=?))`, me.Role == auth.RoleOwner, me.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer rows.Close()
	out := []board.ProductionPlan{}
	for rows.Next() {
		var p board.ProductionPlan
		if err := rows.Scan(&p.Proposition, &p.Owner, &p.NextAction, &p.Blocker, &p.RecordDate, &p.EditDate, &p.Version); err != nil {
			s.fail(w, r, err)
			return
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"plans": out})
}
