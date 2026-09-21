package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/workflow"
)

func TestCalendarEventsAndTasks(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	ctx := context.Background()
	owner := h.owner()
	csrf := h.csrf("/profile")
	show, err := board.GetShow(ctx, h.db)
	if err != nil {
		t.Fatal(err)
	}
	column, err := h.srv.board.CreateColumn(ctx, owner, show.ID, "Calendar tasks")
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body string, want int) string {
		t.Helper()
		res, raw := h.send(method, path, csrf, body)
		if res.StatusCode != want {
			t.Fatalf("%s %s: %d %s", method, path, res.StatusCode, raw)
		}
		return raw
	}
	raw := request("POST", "/app/calendar-events", `{"title":"Planning call","date":"2028-02-29","notes":"Guests, agenda; prep"}`, 200)
	var result struct {
		Event struct {
			After workflow.CalendarEntry `json:"after"`
		} `json:"event"`
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	event := result.Event.After
	if event.ID == 0 || event.Version != 1 {
		t.Fatalf("event: %+v", event)
	}
	taskBody := fmt.Sprintf(`{"title":"Prepare episode","date":"2028-02-28","column":%d}`, column.EntityID)
	request("POST", "/app/calendar-tasks", taskBody, 200)
	var taskID int64
	if err := h.db.QueryRowContext(ctx, `SELECT id FROM cards WHERE title='Prepare episode' AND proposition_id=? AND due_date='2028-02-28'`, show.ID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	var assigned int
	if err := h.db.QueryRowContext(ctx, `SELECT count(*) FROM card_assignees WHERE card_id=? AND user_id=?`, taskID, owner.ID).Scan(&assigned); err != nil || assigned != 1 {
		t.Fatal("task assignment missing")
	}
	entries := request("GET", "/app/calendar-entries", "", 200)
	if !strings.Contains(entries, "Planning call") || !strings.Contains(entries, "Prepare episode") {
		t.Fatal(entries)
	}
	_, profile := h.postBack("/profile/calendar", url.Values{"csrf": {csrf}, "action": {"create"}})
	feedPath := regexp.MustCompile(`/calendar/[a-f0-9]{64}/production.ics`).FindString(profile)
	if feedPath == "" {
		t.Fatal("no subscription")
	}
	feed := func() string {
		t.Helper()
		res, err := http.Get(h.http.URL + feedPath)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		if res.StatusCode != 200 {
			t.Fatal(res.StatusCode)
		}
		return strings.ReplaceAll(string(b), "\r\n ", "")
	}
	ics := feed()
	if !strings.Contains(ics, "SUMMARY:Event: Planning call") || !strings.Contains(ics, "SUMMARY:Task: Prepare episode") || !strings.Contains(ics, "DTEND;VALUE=DATE:20280301") {
		t.Fatal(ics)
	}
	request("POST", "/app/calendar-events", fmt.Sprintf(`{"id":%d,"version":1,"title":"Moved call","date":"2028-03-01"}`, event.ID), 200)
	request("POST", "/app/calendar-events", fmt.Sprintf(`{"id":%d,"version":1,"title":"Stale call","date":"2028-03-02"}`, event.ID), 409)
	if !strings.Contains(feed(), "SUMMARY:Event: Moved call") {
		t.Fatal("feed did not update")
	}
	request("POST", "/app/calendar-events", `{"title":"Invalid","date":"2026-02-30"}`, 422)
	request("POST", "/app/calendar-tasks", fmt.Sprintf(`{"title":"Bad task","date":"not-a-date","column":%d}`, column.EntityID), 422)
	for _, date := range []string{"0000-01-01", "1999-12-31", "2101-01-01", "9999-01-01"} {
		request("POST", "/app/calendar-tasks", fmt.Sprintf(`{"title":"Bad task","date":%q,"column":%d}`, date, column.EntityID), 422)
	}
	var bad int
	if err := h.db.QueryRowContext(ctx, `SELECT count(*) FROM cards WHERE title='Bad task'`).Scan(&bad); err != nil || bad != 0 {
		t.Fatal("invalid task partially created")
	}
	// A private episode's tasks never enter another member's calendar.
	private := h.proposition("Private schedule")
	cols, err := board.ListColumns(ctx, h.db, private)
	if err != nil {
		t.Fatal(err)
	}
	privateTask, err := h.srv.board.CreateCard(ctx, owner, cols[0].ID, "Private task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.srv.board.SetCardDue(ctx, owner, privateTask.EntityID, "2028-02-28"); err != nil {
		t.Fatal(err)
	}
	request("POST", "/app/calendar-tasks", fmt.Sprintf(`{"title":"Bad task","date":"2028-02-28","column":%d}`, cols[0].ID), 404)
	h.client = h.as("calendar_guest", "Calendar guest", auth.RoleGuest)
	csrf = h.csrf("/profile")
	entries = request("GET", "/app/calendar-entries", "", 200)
	if strings.Contains(entries, "Private task") || !strings.Contains(entries, "Moved call") {
		t.Fatal(entries)
	}
	request("POST", "/app/calendar-events", `{"title":"No permission","date":"2028-02-28"}`, 404)
	request("POST", "/app/calendar-tasks", taskBody, 404)
	request("DELETE", fmt.Sprintf("/app/calendar-events/%d?version=2", event.ID), "", 404)
	h.client = h.as("calendar_editor", "Calendar editor", auth.RoleEditor)
	csrf = h.csrf("/profile")
	request("DELETE", fmt.Sprintf("/app/calendar-events/%d?version=1", event.ID), "", 409)
	request("DELETE", fmt.Sprintf("/app/calendar-events/%d?version=2", event.ID), "", 200)
	if strings.Contains(feed(), "Moved call") {
		t.Fatal("deleted event remains")
	}
	if _, err := h.srv.board.SetCardDone(ctx, owner, taskID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.ExecContext(ctx, `INSERT INTO cards_fts(cards_fts,rank) VALUES('integrity-check',1)`); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(feed(), "Prepare episode") {
		t.Fatal("completed task remains")
	}
}
