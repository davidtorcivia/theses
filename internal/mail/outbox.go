package mail

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/davidtorcivia/theses/internal/settings"
	"github.com/davidtorcivia/theses/internal/store"
)

// ErrNotConfigured is what Sender returns while the mail settings are still
// incomplete. Queued rows wait rather than burning their attempts on a server
// that has not been named yet.
var ErrNotConfigured = errors.New("mail is not configured: fill in the SMTP host and the from address")

const (
	pollEvery   = 5 * time.Second
	firstRetry  = time.Minute
	maxRetry    = time.Hour
	giveUpAfter = 24 * time.Hour
	batchSize   = 20
)

// Enqueue writes one row per recipient through q, which may be a transaction,
// so that a message and whatever caused it commit together or not at all.
func Enqueue(ctx context.Context, q store.Querier, m Message) error {
	if len(m.To) == 0 {
		return errors.New("mail: no recipients")
	}
	for _, to := range m.To {
		if _, err := q.ExecContext(ctx, `INSERT INTO mail_outbox
			(to_addr, subject, body_text, body_html, created_at, next_at)
			VALUES (?, ?, ?, ?, unixepoch(), unixepoch())`,
			to, m.Subject, m.Text, m.HTML); err != nil {
			return fmt.Errorf("mail: enqueue: %w", err)
		}
	}
	return nil
}

// Outbox is the worker over mail_outbox. It polls, and Nudge wakes it early.
type Outbox struct {
	db    *store.DB
	set   *settings.Settings
	log   *slog.Logger
	nudge chan struct{}
}

func NewOutbox(db *store.DB, set *settings.Settings, log *slog.Logger) *Outbox {
	return &Outbox{db: db, set: set, log: log, nudge: make(chan struct{}, 1)}
}

// Nudge asks for a batch now. The channel holds one, so a burst of enqueues
// costs one wakeup and nothing blocks on a worker that is already busy.
func (o *Outbox) Nudge() {
	select {
	case o.nudge <- struct{}{}:
	default:
	}
}

// Run sends batches until ctx is cancelled.
func (o *Outbox) Run(ctx context.Context) {
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	for {
		if err := o.once(ctx); err != nil && ctx.Err() == nil && !errors.Is(err, ErrNotConfigured) {
			o.log.Error("mail outbox", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-o.nudge:
		}
	}
}

type queued struct {
	id             int64
	attempts       int
	to             string
	subject        string
	text, htmlBody string
}

// once sends one batch. A configuration that is missing or unreadable stops the
// batch before any row is touched, so nothing runs out its day while the owner
// is still typing the fields in.
func (o *Outbox) once(ctx context.Context) error {
	now := time.Now()
	rows, err := o.db.QueryContext(ctx, `SELECT id, to_addr, subject, body_text, body_html, attempts
		FROM mail_outbox
		WHERE sent_at IS NULL AND next_at <= ? AND created_at > ?
		ORDER BY next_at LIMIT ?`, now.Unix(), now.Add(-giveUpAfter).Unix(), batchSize)
	if err != nil {
		return fmt.Errorf("mail: read outbox: %w", err)
	}
	var batch []queued
	for rows.Next() {
		var q queued
		if err := rows.Scan(&q.id, &q.to, &q.subject, &q.text, &q.htmlBody, &q.attempts); err != nil {
			rows.Close()
			return err
		}
		batch = append(batch, q)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(batch) == 0 {
		return nil
	}

	sender, err := o.Sender(ctx)
	if err != nil {
		return err
	}
	for _, q := range batch {
		err := sender.Send(ctx, Message{To: []string{q.to}, Subject: q.subject, Text: q.text, HTML: q.htmlBody})
		if err == nil {
			if _, err := o.db.ExecContext(ctx,
				`UPDATE mail_outbox SET sent_at = unixepoch(), last_error = '' WHERE id = ?`, q.id); err != nil {
				return err
			}
			continue
		}
		// A cancelled send is the shutdown, not the server refusing. The row is
		// left exactly as it was and goes out on the next start.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		next := time.Now().Add(backoff(q.attempts + 1)).Unix()
		if _, err := o.db.ExecContext(ctx,
			`UPDATE mail_outbox SET attempts = attempts + 1, last_error = ?, next_at = ? WHERE id = ?`,
			Redact(err.Error(), sender.Password), next, q.id); err != nil {
			return err
		}
		o.log.Warn("mail not sent", "id", q.id, "attempts", q.attempts+1,
			"err", Redact(err.Error(), sender.Password))
	}
	return nil
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

// Sender builds the SMTP client from the settings as they stand, so a change on
// the settings page reaches the next batch without a restart.
func (o *Outbox) Sender(ctx context.Context) (SMTP, error) {
	host := settings.Get[string](o.set, "mail.host")
	from := settings.Get[string](o.set, "mail.from")
	if host == "" || from == "" {
		return SMTP{}, ErrNotConfigured
	}
	password, err := o.set.Secret(ctx, "mail.password")
	if err != nil {
		return SMTP{}, err
	}
	return SMTP{
		Host:     host,
		Port:     settings.Get[int](o.set, "mail.port"),
		TLS:      settings.Get[string](o.set, "mail.tls"),
		User:     settings.Get[string](o.set, "mail.user"),
		Password: password,
		From:     from,
	}, nil
}

// State is what the settings page prints about the queue.
type State struct {
	Pending   int // unsent and still being retried
	GivenUp   int // unsent, past the day, kept for inspection
	LastError string
}

func (o *Outbox) State(ctx context.Context) (State, error) {
	cutoff := time.Now().Add(-giveUpAfter).Unix()
	var st State
	err := o.db.QueryRowContext(ctx, `SELECT
		coalesce(sum(CASE WHEN created_at >  ? THEN 1 ELSE 0 END), 0),
		coalesce(sum(CASE WHEN created_at <= ? THEN 1 ELSE 0 END), 0),
		coalesce((SELECT last_error FROM mail_outbox
			WHERE sent_at IS NULL AND last_error <> '' ORDER BY id DESC LIMIT 1), '')
		FROM mail_outbox WHERE sent_at IS NULL`, cutoff, cutoff).
		Scan(&st.Pending, &st.GivenUp, &st.LastError)
	if err != nil {
		return State{}, fmt.Errorf("mail: outbox state: %w", err)
	}
	return st, nil
}

// RetryNow puts every unsent row back at the front of the queue. It restarts the
// day as well, because a row that has run out of it is picked up by nothing.
//
// ponytail: restarting the day loses when the row was first queued, which is
// half of what keeping it for inspection was for; a gave_up_at column would
// hold both, and belongs in the next migration this schema opens anyway.
func (o *Outbox) RetryNow(ctx context.Context) error {
	if _, err := o.db.ExecContext(ctx, `UPDATE mail_outbox
		SET attempts = 0, next_at = unixepoch(), created_at = unixepoch()
		WHERE sent_at IS NULL`); err != nil {
		return fmt.Errorf("mail: retry: %w", err)
	}
	o.Nudge()
	return nil
}

// Redact takes a secret out of text on its way to a page, a log or a stored
// error. An empty secret is left alone, because replacing the empty string
// would spray the marker between every byte.
func Redact(text, secret string) string {
	if secret == "" {
		return text
	}
	return strings.ReplaceAll(text, secret, "[redacted]")
}
