package notify

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/davidtorcivia/theses/internal/mail"
	"github.com/davidtorcivia/theses/internal/notify/channel"
	"github.com/davidtorcivia/theses/internal/settings"
)

const (
	pollEvery  = 5 * time.Second
	firstRetry = time.Minute
	maxRetry   = time.Hour
	// giveUpAfter is the day the plan gives a webhook, counted from the first
	// attempt rather than from the enqueue, so a row queued before the channel
	// worked still gets its full day once it does. A row waiting on a mail
	// server nobody has set up counts it from the enqueue instead, since it has
	// had no attempt to count from.
	giveUpAfter = 24 * time.Hour
	batchSize   = 20
)

// sendable is the part of the WHERE clause that says a row is still worth
// trying. It takes the give up cutoff.
const sendable = `(tried_at IS NULL OR tried_at > ?)`

// send is deliver, replaced in tests so the worker can be driven without a
// network and made to fail on demand.
var send = func(ctx context.Context, s *Service, c Channel, email string, p payload) error {
	return s.deliverPayload(ctx, c, email, p)
}

// Nudge asks for a batch now. The channel holds one, so a burst of enqueues
// costs one wakeup and nothing blocks on a worker that is already busy.
func (s *Service) Nudge() {
	select {
	case s.nudge <- struct{}{}:
	default:
	}
}

// Run delivers batches and runs the daily tick until ctx is canceled.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	for {
		if err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("notification tick", "err", err)
		}
		if err := s.once(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("notification outbox", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.nudge:
		}
	}
}

type queued struct {
	id        int64
	channelID int64
	attempts  int
	payload   payload
}

// once delivers one batch.
func (s *Service) once(ctx context.Context) error {
	now := time.Now()
	cutoff := now.Add(-giveUpAfter).Unix()
	rows, err := s.db.QueryContext(ctx, `SELECT id, channel_id, payload_json, attempts
		FROM notification_outbox
		WHERE sent_at IS NULL AND next_at <= ? AND `+sendable+`
		ORDER BY next_at LIMIT ?`, now.Unix(), cutoff, batchSize)
	if err != nil {
		return fmt.Errorf("notify: read outbox: %w", err)
	}
	var batch []queued
	for rows.Next() {
		var q queued
		var body string
		if err := rows.Scan(&q.id, &q.channelID, &body, &q.attempts); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal([]byte(body), &q.payload); err != nil {
			rows.Close()
			return fmt.Errorf("notify: row %d: %w", q.id, err)
		}
		batch = append(batch, q)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, q := range batch {
		// The batch was read before the first send, and the channel may have
		// been edited, unverified or deleted since. Everything the send needs is
		// read again here, one row at a time.
		c, email, ok, err := s.stillSendable(ctx, q.id, q.channelID)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		err = send(ctx, s, c, email, q.payload)
		if err == nil {
			if err := s.markSent(ctx, q.id); err != nil {
				return err
			}
			continue
		}
		// A canceled send is the shutdown, not the destination refusing. The
		// row is left exactly as it was and goes out on the next start.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Mail that has not been configured yet is not a failed attempt: the
		// row waits, with its day of retries still ahead of it, exactly as a
		// queued invitation does. It is pushed past the next poll rather than
		// left due, because a batch is twenty rows and every account has an
		// email channel: twenty of these sitting at the head of the queue would
		// otherwise stop everything behind them from ever going out. The wait
		// has the same day as a retry, counted from the enqueue: without one,
		// a workspace with no mail server piles up every notification it ever
		// raised and sends them all the day one is set.
		if errors.Is(err, mail.ErrNotConfigured) {
			res, err := s.db.ExecContext(ctx,
				`UPDATE notification_outbox SET next_at = ? WHERE id = ? AND created_at > ?`,
				time.Now().Add(pollEvery).Unix(), q.id, cutoff)
			if err != nil {
				return err
			}
			if n, err := res.RowsAffected(); err != nil {
				return err
			} else if n == 0 {
				if err := s.abandon(ctx, q.id, "mail was not set up within a day of this being queued"); err != nil {
					return err
				}
			}
			continue
		}
		next := time.Now().Add(backoff(q.attempts + 1)).Unix()
		if _, err := s.db.ExecContext(ctx, `UPDATE notification_outbox
			SET attempts = attempts + 1, last_error = ?, next_at = ?,
				tried_at = coalesce(tried_at, unixepoch()) WHERE id = ?`,
			err.Error(), next, q.id); err != nil {
			return err
		}
		s.log.Warn("notification not sent", "id", q.id, "kind", c.Kind,
			"attempts", q.attempts+1, "err", err)
	}
	return nil
}

// stillSendable re-reads a row claimed earlier in the batch along with the
// channel it is for. A channel that has been unverified since stops the row for
// good rather than leaving it to be retried until the end of time.
func (s *Service) stillSendable(ctx context.Context, id, channelID int64) (Channel, string, bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM notification_outbox
		WHERE id = ? AND sent_at IS NULL AND `+sendable,
		id, time.Now().Add(-giveUpAfter).Unix()).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return Channel{}, "", false, nil
	}
	if err != nil {
		return Channel{}, "", false, fmt.Errorf("notify: recheck %d: %w", id, err)
	}
	c, err := GetChannel(ctx, s.db, s.set, channelID)
	if err != nil {
		// The row would have cascaded with the channel, so this is a config
		// that cannot be read rather than a channel that is gone.
		// ponytail: a database that is momentarily unreadable abandons the row
		// too. Distinguish the two once there is a deployment to see it happen.
		return Channel{}, "", false, s.abandon(ctx, id, err.Error())
	}
	if !c.Verified() {
		return Channel{}, "", false, s.abandon(ctx, id,
			"the channel was not verified when this was due to go out")
	}
	email := ""
	if c.Kind == KindEmail {
		if err := s.db.QueryRowContext(ctx,
			`SELECT email FROM users WHERE id = ?`, c.UserID).Scan(&email); err != nil {
			return Channel{}, "", false, s.abandon(ctx, id, "that account has no email address")
		}
	}
	return c, email, true, nil
}

// abandon stops a row being tried again by putting its first attempt a day in
// the past, which is what the give up clause already reads, and records why the
// settings page and the log can say so.
func (s *Service) abandon(ctx context.Context, id int64, why string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE notification_outbox
		SET tried_at = ?, last_error = ? WHERE id = ?`,
		time.Now().Add(-giveUpAfter-time.Second).Unix(), why, id)
	return err
}

// markSent records the delivery. The write drops the cancellation, because the
// destination has already taken the message: a shutdown landing between the two
// would otherwise deliver it twice on the next start.
func (s *Service) markSent(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(context.WithoutCancel(ctx),
		`UPDATE notification_outbox SET sent_at = unixepoch(), last_error = '' WHERE id = ?`, id)
	return err
}

// backoff is a minute, doubled for each attempt already made, capped at an hour.
func backoff(attempts int) time.Duration {
	d := firstRetry
	for i := 1; i < attempts && d < maxRetry; i++ {
		d *= 2
	}
	if d > maxRetry {
		return maxRetry
	}
	return d
}

// note renders a queued payload. A row that collapsed several notifications
// says so in one line each rather than arriving as several messages.
func note(p payload) channel.Note {
	n := channel.Note{
		Title: p.Title, Priority: p.Priority, Event: p.Event,
		Actor: p.Actor, Entity: p.Entity, EntityID: p.EntityID, Tags: p.Tags,
	}
	if len(p.Items) == 0 {
		return n
	}
	n.URL = p.Items[0].URL
	if len(p.Items) == 1 {
		n.Body = p.Items[0].Text
		return n
	}
	lines := make([]string, 0, len(p.Items))
	for _, it := range p.Items {
		lines = append(lines, it.Text)
	}
	n.Body = strings.Join(lines, "\n")
	if p.Event != digestEvent {
		n.Title = fmt.Sprintf("%s, and %d more", p.Title, len(p.Items)-1)
	}
	return n
}

// deliver sends one note to one channel. Mail takes the templates where there
// is one for the event and the plain note where there is not.
func (s *Service) deliver(ctx context.Context, c Channel, email string, n channel.Note) error {
	return s.deliverNote(ctx, c, email, n, nil)
}

func (s *Service) deliverPayload(ctx context.Context, c Channel, email string, p payload) error {
	return s.deliverNote(ctx, c, email, note(p), p.Items)
}

func (s *Service) deliverNote(ctx context.Context, c Channel, email string, n channel.Note, items []item) error {
	snd, err := s.senderFor(ctx, c, email)
	if err != nil {
		return err
	}
	if e, ok := snd.(channel.Email); ok {
		// The SMTP password ends up in what a server says back, which is why the
		// mail outbox takes it out of every error it stores.
		password := ""
		if smtp, ok := e.Sender.(mail.SMTP); ok {
			password = smtp.Password
		}
		if msg, ok := template(e.To, n, items, settings.Get[string](s.set, "workspace.name")); ok {
			return s.redacted(ctx, e.Sender.Send(ctx, msg), c, password)
		}
		return s.redacted(ctx, snd.Send(ctx, n), c, password)
	}
	return s.redacted(ctx, snd.Send(ctx, n), c)
}

// template is the rendered mail for the three events that have one, and false
// for everything else, which goes out as the note.
func template(to string, n channel.Note, items []item, workspace string) (mail.Message, bool) {
	switch n.Event {
	case "mentioned":
		return mail.Mention{To: to, Who: n.Actor, Where: where(n.Entity), Excerpt: n.Body, URL: n.URL}.Message(), true
	case "assigned":
		return mail.Assigned{To: to, Card: strings.TrimPrefix(n.Title, "Assigned to you: "),
			Proposition: workspace, URL: n.URL}.Message(), true
	case digestEvent:
		digest := make([]mail.DigestItem, 0, len(items))
		for _, it := range items {
			digest = append(digest, mail.DigestItem{Text: it.Text, URL: it.URL})
		}
		return mail.Digest{To: to, Items: digest}.Message(), true
	}
	return mail.Message{}, false
}

// where is what the mention mail says somebody was named in.
func where(entity string) string {
	switch entity {
	case "comment":
		return "a note"
	case "card":
		return "a card"
	case "block":
		return "a document"
	}
	return "THESES"
}

// redacted takes every secret out of a failure before it is stored or logged: a
// Pushover key, an ntfy token and a webhook secret all end up in the text a
// server or a transport puts in an error.
func (s *Service) redacted(ctx context.Context, err error, c Channel, more ...string) error {
	if err == nil {
		return nil
	}
	text := redact(err.Error(), c)
	for _, secret := range more {
		text = mail.Redact(text, secret)
	}
	if c.Kind == KindPushover {
		if token, e := s.set.Secret(ctx, "notify.pushover_token"); e == nil {
			text = mail.Redact(text, token)
		}
	}
	if text == err.Error() {
		return err
	}
	return errors.New(text)
}

// State is what the settings page prints about the queue.
type State struct {
	Pending   int
	GivenUp   int
	LastError string
}

func (s *Service) State(ctx context.Context) (State, error) {
	cutoff := time.Now().Add(-giveUpAfter).Unix()
	var st State
	err := s.db.QueryRowContext(ctx, `SELECT
		coalesce(sum(CASE WHEN `+sendable+` THEN 1 ELSE 0 END), 0),
		coalesce(sum(CASE WHEN `+sendable+` THEN 0 ELSE 1 END), 0),
		coalesce((SELECT last_error FROM notification_outbox
			WHERE sent_at IS NULL AND last_error <> '' ORDER BY id DESC LIMIT 1), '')
		FROM notification_outbox WHERE sent_at IS NULL`, cutoff, cutoff).
		Scan(&st.Pending, &st.GivenUp, &st.LastError)
	if err != nil {
		return State{}, fmt.Errorf("notify: outbox state: %w", err)
	}
	return st, nil
}
