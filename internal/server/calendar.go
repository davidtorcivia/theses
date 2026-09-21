package server

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/davidtorcivia/theses/internal/store"
)

func (s *Server) postCalendar(w http.ResponseWriter, r *http.Request) {
	action := r.PostFormValue("action")
	if action != "create" && action != "revoke" {
		http.Error(w, "Invalid action", http.StatusBadRequest)
		return
	}
	token := ""
	if action == "create" {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			s.fail(w, r, err)
			return
		}
		token = hex.EncodeToString(raw)
	}
	hash := sha256.Sum256([]byte(token))
	if err := s.write(r, "user", itoa(userOf(r).ID), "calendar-"+action, "", "", func(q store.Querier) error {
		if action == "revoke" {
			_, err := q.ExecContext(r.Context(), `DELETE FROM calendar_subscriptions WHERE user_id=?`, userOf(r).ID)
			return err
		}
		_, err := q.ExecContext(r.Context(), `INSERT INTO calendar_subscriptions(user_id,token_hash) VALUES(?,?) ON CONFLICT(user_id) DO UPDATE SET token_hash=excluded.token_hash`, userOf(r).ID, hash[:])
		return err
	}); err != nil {
		s.fail(w, r, err)
		return
	}
	if action == "create" {
		s.back(w, r, "/profile#calendar", map[string]any{"CalendarURL": s.cfg.BaseURL + "/calendar/" + token + "/production.ics"})
		return
	}
	http.Redirect(w, r, profileTo("calendar", true), http.StatusSeeOther)
}

func (s *Server) getCalendar(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	token := r.PathValue("token")
	raw, err := hex.DecodeString(token)
	if err != nil || len(raw) != 32 {
		http.NotFound(w, r)
		return
	}
	hash := sha256.Sum256([]byte(token))
	// Resolve the token and current membership in the same database snapshot.
	tx, err := s.db.BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer tx.Rollback()
	var uid int64
	var role string
	err = tx.QueryRowContext(r.Context(), `SELECT u.id,u.role FROM calendar_subscriptions c JOIN users u ON u.id=c.user_id WHERE c.token_hash=?`, hash[:]).Scan(&uid, &role)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rows, err := tx.QueryContext(r.Context(), `SELECT p.id,p.title,p.created_at,p.calendar_revision,p.calendar_updated_at,coalesce(p.target_date,''),coalesce(pp.record_date,''),coalesce(pp.edit_date,'') FROM propositions p LEFT JOIN production_plans pp ON pp.proposition_id=p.id WHERE p.kind!='show' AND p.archived_at IS NULL AND (?='owner' OR EXISTS(SELECT 1 FROM proposition_members m WHERE m.proposition_id=p.id AND m.user_id=?)) ORDER BY p.id`, role, uid)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer rows.Close()
	var out strings.Builder
	for _, line := range []string{"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//Theses//Production calendar//EN", "CALSCALE:GREGORIAN", "X-WR-CALNAME:Theses production"} {
		calendarLine(&out, line)
	}
	workspace := sha256.Sum256([]byte(s.cfg.BaseURL))
	for rows.Next() {
		var id, created, revision, updated int64
		var title, release, record, edit string
		if err := rows.Scan(&id, &title, &created, &revision, &updated, &release, &record, &edit); err != nil {
			s.fail(w, r, err)
			return
		}
		for _, event := range []struct{ kind, date string }{{"Record", record}, {"Edit", edit}, {"Release", release}} {
			appendCalendarEvent(&out, fmt.Sprintf("%x-%x-%d-%d-%s", workspace[:8], hash[:12], id, created, event.kind), event.date, event.kind+": "+title, "", s.cfg.BaseURL+"/p/"+itoa(id), revision, updated)
		}
	}
	if err := rows.Err(); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := rows.Close(); err != nil {
		s.fail(w, r, err)
		return
	}
	extra, err := tx.QueryContext(r.Context(), `SELECT 'card',c.id,c.created_at,c.calendar_revision,c.calendar_updated_at,c.due_date,c.title,'','/p/'||p.id||'#card-'||c.id FROM cards c JOIN propositions p ON p.id=c.proposition_id WHERE c.done_at IS NULL AND p.archived_at IS NULL AND coalesce(c.due_date,'')!='' AND (?='owner' OR EXISTS(SELECT 1 FROM proposition_members m WHERE m.proposition_id=p.id AND m.user_id=?))
 UNION ALL SELECT 'event',e.id,e.created_at,e.version,e.updated_at,e.date,e.title,e.notes,'/show#calendar' FROM calendar_events e JOIN propositions p ON p.kind='show' WHERE (?='owner' OR EXISTS(SELECT 1 FROM proposition_members m WHERE m.proposition_id=p.id AND m.user_id=?)) ORDER BY 1,2`, role, uid, role, uid)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer extra.Close()
	for extra.Next() {
		var kind, date, title, notes, path string
		var id, created, revision, updated int64
		if err := extra.Scan(&kind, &id, &created, &revision, &updated, &date, &title, &notes, &path); err != nil {
			s.fail(w, r, err)
			return
		}
		label := "Event: "
		if kind == "card" {
			label = "Task: "
		}
		appendCalendarEvent(&out, fmt.Sprintf("%x-%x-%s-%d-%d", workspace[:8], hash[:12], kind, id, created), date, label+title, notes, s.cfg.BaseURL+path, revision, updated)
	}
	if err := extra.Err(); err != nil {
		s.fail(w, r, err)
		return
	}
	calendarLine(&out, "END:VCALENDAR")
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("Content-Disposition", `inline; filename="production.ics"`)
	fmt.Fprint(w, out.String())
}

func appendCalendarEvent(out *strings.Builder, uid, date, title, notes, url string, revision, updated int64) {
	day, err := time.Parse("2006-01-02", date)
	if err != nil || day.Year() < 1 || day.Year() > 9998 {
		return
	}
	for _, line := range []string{"BEGIN:VEVENT", "UID:" + uid + "@theses", "DTSTAMP:" + time.Unix(updated, 0).UTC().Format("20060102T150405Z"), fmt.Sprintf("SEQUENCE:%d", revision), "DTSTART;VALUE=DATE:" + day.Format("20060102"), "DTEND;VALUE=DATE:" + day.AddDate(0, 0, 1).Format("20060102"), "SUMMARY:" + calendarText(title), "DESCRIPTION:" + calendarText(notes), "URL:" + url, "CLASS:PRIVATE", "TRANSP:TRANSPARENT", "END:VEVENT"} {
		calendarLine(out, line)
	}
}

func calendarText(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\t' && r != '\n' {
			return -1
		}
		return r
	}, s)
	return strings.NewReplacer("\\", "\\\\", "\n", "\\n", ";", "\\;", ",", "\\,").Replace(s)
}

func calendarLine(out *strings.Builder, line string) {
	line = strings.ToValidUTF8(line, "�")
	// RFC 5545 folds at 75 octets without splitting a UTF-8 code point.
	for len(line) > 75 {
		n := 75
		for !utf8.RuneStart(line[n]) {
			n--
		}
		out.WriteString(line[:n])
		out.WriteString("\r\n")
		line = " " + line[n:]
	}
	out.WriteString(line)
	out.WriteString("\r\n")
}
