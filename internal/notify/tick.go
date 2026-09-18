package notify

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/settings"
)

// tickActor is who the daily pass is. It is not a person, so nobody is left out
// of what it produces the way the actor of a command is.
var tickActor = core.Actor{Kind: "system", Name: "THESES"}

// tickLead is how long before the digest the daily pass runs, in minutes.
const tickLead = 5

// Tick produces the notifications no command does: a card due tomorrow, a card
// that has just gone overdue, and a release day tomorrow. It runs once on the
// day, at the digest time, and records the day it ran so that a restart an hour
// later does not send everything again.
func (s *Service) Tick(ctx context.Context) error {
	loc := s.location()
	now := time.Unix(s.Now(), 0).In(loc)
	at, ok := minutes(settings.Get[string](s.set, "notify.digest_time"))
	if !ok {
		at = 8 * 60
	}
	// A few minutes before the hour, so that what the pass produces for a
	// digest channel catches today's digest rather than tomorrow's.
	if now.Hour()*60+now.Minute() < at-tickLead {
		return nil
	}
	today := day(now)
	if settings.Get[int](s.set, "notify.last_tick") >= today {
		return nil
	}
	// Recorded before the work rather than after it, so a failure part way
	// through does not put the whole pass through again on the next poll.
	if err := s.set.SetAs(ctx, "notify.last_tick", []string{strconv.Itoa(today)}, settings.System()); err != nil {
		return err
	}

	matches, err := s.dated(ctx, now, loc)
	if err != nil {
		return err
	}
	if len(matches) == 0 {
		return nil
	}
	return s.queue(ctx, tickActor, matches)
}

// day is a date as the number the last run is remembered by.
func day(t time.Time) int {
	y, m, d := t.Date()
	return y*10000 + int(m)*100 + d
}

// dated is every card and proposition whose date falls due now.
func (s *Service) dated(ctx context.Context, now time.Time, loc *time.Location) ([]Notice, error) {
	tomorrow := day(now.AddDate(0, 0, 1))
	yesterday := day(now.AddDate(0, 0, -1))

	var out []Notice
	rows, err := s.db.QueryContext(ctx, `SELECT c.id, c.proposition_id, c.title, c.due_date
		FROM cards c JOIN propositions p ON p.id = c.proposition_id
		WHERE c.done_at IS NULL AND c.due_date IS NOT NULL AND c.due_date <> ''
			AND p.archived_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("notify: due cards: %w", err)
	}
	for rows.Next() {
		var id, proposition int64
		var title, due string
		if err := rows.Scan(&id, &proposition, &title, &due); err != nil {
			rows.Close()
			return nil, err
		}
		when, ok := parseDate(due, now, loc)
		if !ok {
			continue
		}
		m := Notice{
			Who: WhoCardAssignees, Card: id, Proposition: proposition,
			Entity: "card", EntityID: id,
		}
		switch day(when) {
		case tomorrow:
			m.Event, m.Title = "due", "Due tomorrow: "+title
			m.Text = strconv.Quote(title) + " is due tomorrow."
		case yesterday:
			// Once, on the morning after. A card that has been late for a week
			// is not news every day.
			m.Event, m.Title = "overdue", "Overdue: "+title
			m.Text = strconv.Quote(title) + " was due yesterday and is not done."
		default:
			continue
		}
		out = append(out, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	props, err := s.db.QueryContext(ctx, `SELECT id, title, target_date FROM propositions
		WHERE archived_at IS NULL AND target_date IS NOT NULL AND target_date <> ''`)
	if err != nil {
		return nil, fmt.Errorf("notify: release dates: %w", err)
	}
	defer props.Close()
	for props.Next() {
		var id int64
		var title, target string
		if err := props.Scan(&id, &title, &target); err != nil {
			return nil, err
		}
		when, ok := parseDate(target, now, loc)
		if !ok || day(when) != tomorrow {
			continue
		}
		out = append(out, Notice{
			Event: "release", Who: WhoMembers, Proposition: id,
			Entity: "proposition", EntityID: id,
			Title: "Release tomorrow: " + title,
			Text:  strconv.Quote(title) + " is out tomorrow.",
		})
	}
	return out, props.Err()
}

// dateLayouts are what a date on a card may have been typed as. The board takes
// free text, so anything else is left alone rather than guessed at.
var dateLayouts = []string{"2006-01-02", "2 Jan 2006", "2 January 2006", "Jan 2 2006"}

// parseDate reads a due date. A date with no year is this year, which is what
// the board already assumes when it draws one as late.
func parseDate(value string, now time.Time, loc *time.Location) (time.Time, bool) {
	for _, layout := range dateLayouts {
		if t, err := time.ParseInLocation(layout, value, loc); err == nil {
			return t, true
		}
	}
	year := " " + strconv.Itoa(now.Year())
	for _, layout := range []string{"2 Jan 2006", "2 January 2006", "Jan 2 2006"} {
		if t, err := time.ParseInLocation(layout, value+year, loc); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
