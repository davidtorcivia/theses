package notify

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/settings"
	"github.com/davidtorcivia/theses/internal/store"
)

// collapseWindow is how long a collapsible notification waits for the next one
// like it. Five card moves inside it are one line rather than five messages.
const collapseWindow = time.Minute

// digestPrefix marks a collapse key as one day's worth of one channel.
const digestPrefix = "digest:"

// An item is one line of a queued message. A row that collapsed several
// notifications together carries one of these for each.
type item struct {
	Text string `json:"text"`
	URL  string `json:"url"`
}

// payload is what an outbox row carries. It is the note as far as it can be
// built without the channel, which is everything but the address.
type payload struct {
	Event    string   `json:"event"`
	Title    string   `json:"title"`
	Actor    string   `json:"actor"`
	Entity   string   `json:"entity"`
	EntityID int64    `json:"entity_id"`
	Priority int      `json:"priority"`
	Tags     []string `json:"tags"`
	Items    []item   `json:"items"`
}

// queue writes one outbox row per channel for every match, in one transaction,
// so a command either produces all of its notifications or none.
func (s *Service) queue(ctx context.Context, actor core.Actor, matches []Notice) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := s.Now()
	loc := s.location()
	for _, m := range matches {
		if m.Proposition == 0 && m.Document != 0 {
			if m.Proposition, err = propositionOfDocument(ctx, tx, m.Document); err != nil {
				return err
			}
		}
		if err := s.queueWorkspace(ctx, tx, m, actor, now); err != nil {
			return err
		}
		people, err := s.recipients(ctx, tx, m, actor)
		if err != nil {
			return err
		}
		for _, user := range people {
			if err := s.queueUser(ctx, tx, m, actor, user, now, loc); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.Nudge()
	return nil
}

// queueUser writes the rows one account's channels earn from one match.
func (s *Service) queueUser(ctx context.Context, tx *sql.Tx, m Notice, actor core.Actor,
	user, now int64, loc *time.Location) error {
	channels, err := ListChannels(ctx, tx, s.set, user)
	if err != nil {
		return err
	}
	enabled, err := enabledChannels(ctx, tx, user, m.Event)
	if err != nil {
		return err
	}

	// A mention has to arrive somewhere. The rows are written first and the
	// rule is applied to what they turned out to be, because a mention that
	// went out on a channel straight away needs no second copy.
	var wrote []int64
	immediate := false
	for _, c := range channels {
		if !c.Verified() || !enabled[c.ID] {
			continue
		}
		at, collapse := s.schedule(c, m, now, loc)
		if at <= now {
			immediate = true
		}
		if err := s.write(ctx, tx, c.ID, m, actor, at, collapse, now); err != nil {
			return err
		}
		wrote = append(wrote, c.ID)
	}
	if m.Event != "mentioned" || immediate {
		return nil
	}
	// Quiet hours and the digest are about noise, and being named is not noise.
	// One channel takes it now: the first that was going to get it at all, or
	// failing that the first verified one the account has.
	forced := int64(0)
	if len(wrote) > 0 {
		forced = wrote[0]
	} else {
		for _, c := range channels {
			if c.Verified() {
				forced = c.ID
				break
			}
		}
	}
	if forced == 0 {
		return nil
	}
	return s.write(ctx, tx, forced, m, actor, now, "", now)
}

// schedule is when a notification goes out on one channel and what it may
// merge with while it waits.
func (s *Service) schedule(c Channel, m Notice, now int64, loc *time.Location) (at int64, collapse string) {
	if c.Digest {
		return nextDigest(now, loc, settings.Get[string](s.set, "notify.digest_time")),
			digestPrefix + strconv.FormatInt(c.ID, 10)
	}
	if until := quietUntil(now, loc, c.QuietFrom, c.QuietTo); until > now {
		return until, ""
	}
	if e, ok := LookupEvent(m.Event); ok && e.Collapse {
		return now + int64(collapseWindow.Seconds()),
			fmt.Sprintf("%d:%s:%d", c.ID, m.Event, m.Proposition)
	}
	return now, ""
}

// write puts one notification in the outbox, merging it into a row that is
// already waiting under the same key.
//
// Only a row whose turn has not come merges: one that is due may be inside the
// worker's batch between its re-check and its send, and a line appended there
// would be marked sent without ever going out.
func (s *Service) write(ctx context.Context, tx *sql.Tx, channelID int64, m Notice,
	actor core.Actor, at int64, collapse string, now int64) error {
	line := item{Text: m.Text, URL: s.link(m.Proposition)}
	if collapse != "" {
		var id int64
		var stored string
		err := tx.QueryRowContext(ctx, `SELECT id, payload_json FROM notification_outbox
			WHERE channel_id = ? AND collapse = ? AND sent_at IS NULL AND next_at > ?
			ORDER BY id LIMIT 1`, channelID, collapse, now).Scan(&id, &stored)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("notify: collapse: %w", err)
		}
		if err == nil {
			var p payload
			if err := json.Unmarshal([]byte(stored), &p); err != nil {
				return fmt.Errorf("notify: collapse %d: %w", id, err)
			}
			p.Items = append(p.Items, line)
			body, err := json.Marshal(p)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx,
				`UPDATE notification_outbox SET payload_json = ? WHERE id = ?`, string(body), id)
			return err
		}
	}

	event, _ := LookupEvent(m.Event)
	p := payload{
		Event: m.Event, Title: m.Title, Actor: actorName(actor),
		Entity: m.Entity, EntityID: m.EntityID,
		Priority: event.Priority, Tags: event.Tags, Items: []item{line},
	}
	if strings.HasPrefix(collapse, digestPrefix) {
		// Everything a digest channel is put off until the morning arrives as
		// one message, so it is titled and rendered as one from the first line.
		p.Event, p.Title, p.Priority, p.Tags = digestEvent, "THESES daily digest", -1, nil
	}
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO notification_outbox
		(channel_id, payload_json, created_at, next_at, event, collapse)
		VALUES (?, ?, ?, ?, ?, ?)`, channelID, string(body), now, at, m.Event, collapse)
	if err != nil {
		return fmt.Errorf("notify: enqueue: %w", err)
	}
	return nil
}

// queueWorkspace writes the owner's webhooks, which fire on what happened
// whoever did it: no rules, no quiet hours, no digest and no actor left out.
func (s *Service) queueWorkspace(ctx context.Context, tx *sql.Tx, m Notice, actor core.Actor, now int64) error {
	channels, err := ListChannels(ctx, tx, s.set, 0)
	if err != nil {
		return err
	}
	for _, c := range channels {
		if !c.Verified() || !slices.Contains(c.Config.Events, m.Event) {
			continue
		}
		// A webhook may name the column a card has to reach, which is how "a
		// card reaching Publication" is one message rather than every move.
		if c.Config.Column != "" && m.Event == "moved" {
			name, err := columnOfCard(ctx, tx, m.Card)
			if err != nil {
				return err
			}
			if !strings.EqualFold(name, c.Config.Column) {
				continue
			}
		}
		if err := s.write(ctx, tx, c.ID, m, actor, now, "", now); err != nil {
			return err
		}
	}
	return nil
}

// recipients turns a match into the people it is for, which never includes
// whoever caused it.
func (s *Service) recipients(ctx context.Context, q store.Querier, m Notice, actor core.Actor) ([]int64, error) {
	var found []int64
	var err error
	switch m.Who {
	case WhoUsers:
		found = m.Users
	case WhoHandles:
		found, err = usersByHandle(ctx, q, m.Handles)
	case WhoCardAssignees:
		found, err = ids(ctx, q, `SELECT user_id FROM card_assignees WHERE card_id = ?`, m.Card)
	case WhoMembers:
		found, err = ids(ctx, q, `SELECT user_id FROM proposition_members WHERE proposition_id = ?`, m.Proposition)
	}
	if err != nil {
		return nil, err
	}
	out := make([]int64, 0, len(found))
	for _, id := range found {
		// The markdown watcher is not a person, so nothing is excluded for it.
		if actor.Kind == core.KindUser && id == actor.ID {
			continue
		}
		if slices.Contains(m.Exclude, id) || slices.Contains(out, id) {
			continue
		}
		out = append(out, id)
	}
	return out, nil
}

func usersByHandle(ctx context.Context, q store.Querier, handles []string) ([]int64, error) {
	if len(handles) == 0 {
		return nil, nil
	}
	args := make([]any, len(handles))
	for i, h := range handles {
		args[i] = strings.ToLower(h)
	}
	query := `SELECT id FROM users WHERE handle IN (?` + strings.Repeat(", ?", len(handles)-1) + `)`
	return ids(ctx, q, query, args...)
}

func ids(ctx context.Context, q store.Querier, query string, args ...any) ([]int64, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("notify: recipients: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// enabledChannels is the ticked row of one account's matrix.
func enabledChannels(ctx context.Context, q store.Querier, user int64, event string) (map[int64]bool, error) {
	list, err := ids(ctx, q, `SELECT channel_id FROM notification_rules
		WHERE user_id = ? AND event = ? AND enabled = 1`, user, event)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]bool, len(list))
	for _, id := range list {
		out[id] = true
	}
	return out, nil
}

func propositionOfDocument(ctx context.Context, q store.Querier, document int64) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx,
		`SELECT proposition_id FROM documents WHERE id = ?`, document).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

func columnOfCard(ctx context.Context, q store.Querier, card int64) (string, error) {
	var name string
	err := q.QueryRowContext(ctx,
		`SELECT c.name FROM cards k JOIN columns c ON c.id = k.column_id WHERE k.id = ?`, card).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return name, err
}

// location is the time zone quiet hours and the digest are read in.
//
// ponytail: the workspace's, because nothing stores a per account zone yet.
// When the profile page takes one, read it here and fall back to this.
func (s *Service) location() *time.Location {
	loc, err := time.LoadLocation(settings.Get[string](s.set, "workspace.timezone"))
	if err != nil {
		return time.UTC
	}
	return loc
}

// checkWindow refuses quiet hours that are not two times of day. Both empty is
// no window at all.
func checkWindow(from, to string) error {
	if from == "" && to == "" {
		return nil
	}
	if _, ok := minutes(from); !ok {
		return errors.New("quiet hours start at a time like 23:00")
	}
	if _, ok := minutes(to); !ok {
		return errors.New("quiet hours end at a time like 07:00")
	}
	return nil
}

// minutes parses "HH:MM" into minutes past midnight.
func minutes(hhmm string) (int, bool) {
	h, m, ok := strings.Cut(hhmm, ":")
	if !ok || len(h) != 2 || len(m) != 2 {
		return 0, false
	}
	hours, err := strconv.Atoi(h)
	if err != nil || hours > 23 {
		return 0, false
	}
	mins, err := strconv.Atoi(m)
	if err != nil || mins > 59 {
		return 0, false
	}
	return hours*60 + mins, true
}

// quietUntil is when the quiet window now sits inside ends, or zero when it
// does not. A window whose end is before its start crosses midnight, which is
// what a night is.
func quietUntil(now int64, loc *time.Location, from, to string) int64 {
	start, ok := minutes(from)
	if !ok {
		return 0
	}
	end, ok := minutes(to)
	if !ok {
		return 0
	}
	if start == end {
		return 0 // a window of no length silences nothing
	}
	t := time.Unix(now, 0).In(loc)
	at := t.Hour()*60 + t.Minute()
	midnight := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
	ends := func(minutesPastMidnight, days int) int64 {
		return midnight.AddDate(0, 0, days).Add(time.Duration(minutesPastMidnight) * time.Minute).Unix()
	}
	if start < end {
		if at >= start && at < end {
			return ends(end, 0)
		}
		return 0
	}
	// Crosses midnight: quiet from the start until the end the next morning.
	if at >= start {
		return ends(end, 1)
	}
	if at < end {
		return ends(end, 0)
	}
	return 0
}

// nextDigest is the next time of day the digest goes out, in the workspace's
// zone. A digest time that cannot be read is eight in the morning.
func nextDigest(now int64, loc *time.Location, hhmm string) int64 {
	at, ok := minutes(hhmm)
	if !ok {
		at = 8 * 60
	}
	t := time.Unix(now, 0).In(loc)
	midnight := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
	when := midnight.Add(time.Duration(at) * time.Minute).Unix()
	if when <= now {
		when = midnight.AddDate(0, 0, 1).Add(time.Duration(at) * time.Minute).Unix()
	}
	return when
}

func nowUnix() int64 { return time.Now().Unix() }
