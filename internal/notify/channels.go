package notify

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/davidtorcivia/theses/internal/mail"
	"github.com/davidtorcivia/theses/internal/notify/channel"
	"github.com/davidtorcivia/theses/internal/settings"
	"github.com/davidtorcivia/theses/internal/store"
)

// The four kinds of destination. Email is the account's own address through the
// workspace SMTP server, so it carries no configuration at all.
const (
	KindEmail    = "email"
	KindPushover = "pushover"
	KindNtfy     = "ntfy"
	KindWebhook  = "webhook"
)

// Kinds is the order the profile page offers them in.
var Kinds = []string{KindEmail, KindPushover, KindNtfy, KindWebhook}

// Config is every field any channel takes. One struct rather than four,
// because a channel is a row in one table and the kind says which fields mean
// anything.
type Config struct {
	UserKey string `json:"user_key,omitempty"` // pushover
	Server  string `json:"server,omitempty"`   // ntfy
	Topic   string `json:"topic,omitempty"`    // ntfy
	Token   string `json:"token,omitempty"`    // ntfy
	URL     string `json:"url,omitempty"`      // webhook
	Secret  string `json:"secret,omitempty"`   // webhook
	// Events and Column belong to a workspace webhook, which fires on what
	// happened rather than on who it happened to and so has no rules row.
	Events []string `json:"events,omitempty"`
	Column string   `json:"column,omitempty"`
}

// A Channel is one destination. UserID is zero for a workspace webhook.
type Channel struct {
	ID         int64
	UserID     int64
	Kind       string
	Config     Config
	QuietFrom  string
	QuietTo    string
	Digest     bool
	CreatedAt  int64
	VerifiedAt int64
}

// SameDestination reports whether two configs point at the same place. The
// quiet hours and the digest flag are not part of it: moving those does not
// make a channel unproven, and taking its verified state away would have the
// worker abandon the rows already queued for it.
func (c Config) SameDestination(other Config) bool {
	return c.UserKey == other.UserKey && c.Server == other.Server &&
		c.Topic == other.Topic && c.Token == other.Token &&
		c.URL == other.URL && c.Secret == other.Secret
}

// Verified reports whether the channel has ever delivered a test. Nothing is
// sent to one that has not: a mistyped topic would otherwise swallow every
// notification in silence.
func (c Channel) Verified() bool { return c.VerifiedAt > 0 }

// Secrets is what must never appear in a log, a stored error or a page.
func (c Channel) Secrets() []string {
	return []string{c.Config.UserKey, c.Config.Token, c.Config.Secret}
}

// Label is the one line the channel list prints. Nothing in it is a secret:
// the last four characters of a key are what tells two of them apart.
func (c Channel) Label(email string) string {
	switch c.Kind {
	case KindEmail:
		return email
	case KindPushover:
		return "user key " + tail(c.Config.UserKey)
	case KindNtfy:
		server := c.Config.Server
		if server == "" {
			server = channel.DefaultNtfyServer
		}
		return strings.TrimPrefix(strings.TrimPrefix(strings.TrimSuffix(server, "/"),
			"https://"), "http://") + "/" + c.Config.Topic
	case KindWebhook:
		if u, err := url.Parse(c.Config.URL); err == nil && u.Host != "" {
			return u.Scheme + "://" + u.Host + u.EscapedPath()
		}
		return c.Config.URL
	}
	return c.Kind
}

// tail is how a key is shown once it is stored: four characters and dots.
func tail(secret string) string {
	if secret == "" {
		return "not set"
	}
	if len(secret) <= 4 {
		return "••••"
	}
	return "•••• " + secret[len(secret)-4:]
}

// Validate refuses a channel that cannot work before it is stored, so the
// account finds out here rather than from silence.
func (c Channel) Validate() error {
	switch c.Kind {
	case KindEmail:
		// The address is the account's own, so there is nothing else to check.
	case KindPushover:
		if c.Config.UserKey == "" {
			return errors.New("Pushover needs your user key")
		}
	case KindNtfy:
		if c.Config.Topic == "" {
			return errors.New("ntfy needs a topic")
		}
		if strings.ContainsAny(c.Config.Topic, "/ ?#") {
			return errors.New("an ntfy topic is one word, without a slash")
		}
	case KindWebhook:
		u, err := url.Parse(c.Config.URL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return errors.New("a webhook needs an http or https URL")
		}
	default:
		return fmt.Errorf("%q is not a kind of channel", c.Kind)
	}
	return checkWindow(c.QuietFrom, c.QuietTo)
}

// ListChannels returns one account's channels, or the workspace's when user is
// zero.
func ListChannels(ctx context.Context, q store.Querier, set *settings.Settings, user int64) ([]Channel, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, coalesce(user_id, 0), kind, config_json,
		quiet_from, quiet_to, digest, created_at, coalesce(verified_at, 0)
		FROM notification_channels WHERE coalesce(user_id, 0) = ? ORDER BY id`, user)
	if err != nil {
		return nil, fmt.Errorf("notify: list channels: %w", err)
	}
	defer rows.Close()
	out := []Channel{}
	for rows.Next() {
		c, err := scanChannel(rows, set)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetChannel reads one channel by id.
func GetChannel(ctx context.Context, q store.Querier, set *settings.Settings, id int64) (Channel, error) {
	row := q.QueryRowContext(ctx, `SELECT id, coalesce(user_id, 0), kind, config_json,
		quiet_from, quiet_to, digest, created_at, coalesce(verified_at, 0)
		FROM notification_channels WHERE id = ?`, id)
	c, err := scanChannel(row, set)
	if errors.Is(err, sql.ErrNoRows) {
		return Channel{}, store.ErrNotFound
	}
	return c, err
}

type scanner interface{ Scan(...any) error }

func scanChannel(r scanner, set *settings.Settings) (Channel, error) {
	var c Channel
	var stored string
	if err := r.Scan(&c.ID, &c.UserID, &c.Kind, &stored, &c.QuietFrom, &c.QuietTo,
		&c.Digest, &c.CreatedAt, &c.VerifiedAt); err != nil {
		return Channel{}, err
	}
	plain, err := set.Unseal(configContext, stored)
	if err != nil {
		return Channel{}, fmt.Errorf("notify: channel %d config %w", c.ID, err)
	}
	if err := json.Unmarshal([]byte(plain), &c.Config); err != nil {
		return Channel{}, fmt.Errorf("notify: channel %d config: %w", c.ID, err)
	}
	return c, nil
}

// configContext binds a sealed config to this column, so a value cannot be
// moved out of a settings row and into a channel.
const configContext = "notification_channels.config_json"

// SaveChannel writes a channel and returns it with its id. A channel with an
// id replaces the row it names, which must belong to the same account.
//
// The config is encrypted at rest with the settings key: a user key, an ntfy
// token and a webhook secret are secrets, and the plan says so.
func SaveChannel(ctx context.Context, db *store.DB, set *settings.Settings, c Channel) (Channel, error) {
	if err := c.Validate(); err != nil {
		return Channel{}, err
	}
	body, err := json.Marshal(c.Config)
	if err != nil {
		return Channel{}, err
	}
	sealed, err := set.Seal(configContext, string(body))
	if err != nil {
		return Channel{}, err
	}
	var owner any
	if c.UserID != 0 {
		owner = c.UserID
	}
	var verified any
	if c.VerifiedAt != 0 {
		verified = c.VerifiedAt
	}
	if c.ID == 0 {
		res, err := db.ExecContext(ctx, `INSERT INTO notification_channels
			(user_id, kind, config_json, quiet_from, quiet_to, digest, created_at, verified_at)
			VALUES (?, ?, ?, ?, ?, ?, unixepoch(), ?)`,
			owner, c.Kind, sealed, c.QuietFrom, c.QuietTo, c.Digest, verified)
		if err != nil {
			return Channel{}, fmt.Errorf("notify: save channel: %w", err)
		}
		if c.ID, err = res.LastInsertId(); err != nil {
			return Channel{}, err
		}
		return c, nil
	}
	res, err := db.ExecContext(ctx, `UPDATE notification_channels
		SET kind = ?, config_json = ?, quiet_from = ?, quiet_to = ?, digest = ?, verified_at = ?
		WHERE id = ? AND coalesce(user_id, 0) = ?`,
		c.Kind, sealed, c.QuietFrom, c.QuietTo, c.Digest, verified, c.ID, c.UserID)
	if err != nil {
		return Channel{}, fmt.Errorf("notify: save channel: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return Channel{}, store.ErrNotFound
	}
	return c, nil
}

// DeleteChannel removes one channel, and with it every rule and every queued
// message pointing at it.
func DeleteChannel(ctx context.Context, db *store.DB, id, user int64) error {
	res, err := db.ExecContext(ctx,
		`DELETE FROM notification_channels WHERE id = ? AND coalesce(user_id, 0) = ?`, id, user)
	if err != nil {
		return fmt.Errorf("notify: delete channel: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// markVerified records that something reached the channel.
func markVerified(ctx context.Context, db *store.DB, id int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE notification_channels SET verified_at = unixepoch() WHERE id = ?`, id)
	return err
}

// Test sends one message to a channel and records the channel as verified when
// it arrives. It is what the button on the profile page does and what the API
// route does, so a channel is verified by the same path either way.
func (s *Service) Test(ctx context.Context, c Channel, email string) error {
	// No link: a test proves the destination takes a message, and a deep link
	// to nothing in particular is the one line of it nobody wants.
	note := channel.Note{
		Title:  "THESES test",
		Body:   "This is the test message from the notification settings. Nothing is wrong.",
		Event:  "test",
		Entity: "channel",
	}
	if err := s.deliver(ctx, c, email, note); err != nil {
		return err
	}
	return markVerified(ctx, s.db, c.ID)
}

// sender is what a kind of channel turns into. Four implementations, one line
// each, so that deliver has one shape whatever the destination is.
type sender interface {
	Send(context.Context, channel.Note) error
}

// senderFor builds the client for one channel from the channel's own config and
// whatever the workspace holds for it. The Pushover application token and the
// default ntfy server are the workspace's, so that a member pastes only their
// own key.
func (s *Service) senderFor(ctx context.Context, c Channel, email string) (sender, error) {
	switch c.Kind {
	case KindEmail:
		smtp, err := s.mail.Sender(ctx)
		if err != nil {
			return nil, err
		}
		if email == "" {
			return nil, errors.New("this account has no email address")
		}
		return channel.Email{Sender: smtp, To: email}, nil
	case KindPushover:
		token, err := s.set.Secret(ctx, "notify.pushover_token")
		if err != nil {
			return nil, err
		}
		if token == "" {
			return nil, errors.New("the workspace has no Pushover application token; the owner sets one in Settings")
		}
		return channel.Pushover{Token: token, UserKey: c.Config.UserKey}, nil
	case KindNtfy:
		server := c.Config.Server
		if server == "" {
			server = settings.Get[string](s.set, "notify.ntfy_server")
		}
		return channel.Ntfy{Server: server, Topic: c.Config.Topic, Token: c.Config.Token}, nil
	case KindWebhook:
		return channel.Webhook{URL: c.Config.URL, Secret: c.Config.Secret}, nil
	}
	return nil, fmt.Errorf("%q is not a kind of channel", c.Kind)
}

// redact takes every secret a channel holds out of a message on its way to a
// log, a stored error or a page.
func redact(text string, c Channel) string {
	for _, secret := range c.Secrets() {
		text = mail.Redact(text, secret)
	}
	return text
}

// StartingChannels gives a new account the email channel on its own address
// and the rules the owner chose for a new account, so that being mentioned
// reaches somebody from the first day rather than from the first visit to the
// profile page. It runs in the transaction that creates the account.
//
// Email is verified on creation: the address is the account's own and was typed
// by whoever invited them, so there is nothing for a test to prove.
func StartingChannels(ctx context.Context, q store.Querier, set *settings.Settings, user int64) error {
	sealed, err := set.Seal(configContext, "{}")
	if err != nil {
		return err
	}
	res, err := q.ExecContext(ctx, `INSERT INTO notification_channels
		(user_id, kind, config_json, created_at, verified_at)
		VALUES (?, 'email', ?, unixepoch(), unixepoch())`, user, sealed)
	if err != nil {
		return fmt.Errorf("notify: starting channel: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	for _, key := range DefaultEvents(set) {
		if _, ok := LookupEvent(key); !ok {
			continue
		}
		if _, err := q.ExecContext(ctx, `INSERT INTO notification_rules
			(user_id, event, channel_id, enabled) VALUES (?, ?, ?, 1)`, user, key, id); err != nil {
			return fmt.Errorf("notify: starting rules: %w", err)
		}
	}
	return nil
}

// DefaultEvents is what the owner set a new account to start with, one key per
// line. An unknown key is ignored rather than refused: the list outlives any
// one spelling of the matrix.
func DefaultEvents(set *settings.Settings) []string {
	var out []string
	for _, line := range strings.Split(settings.Get[string](set, "notify.defaults"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// Rules is one account's matrix: the channels ticked for each event.
func Rules(ctx context.Context, q store.Querier, user int64) (map[string][]int64, error) {
	rows, err := q.QueryContext(ctx, `SELECT event, channel_id FROM notification_rules
		WHERE user_id = ? AND enabled = 1`, user)
	if err != nil {
		return nil, fmt.Errorf("notify: rules: %w", err)
	}
	defer rows.Close()
	out := map[string][]int64{}
	for rows.Next() {
		var event string
		var id int64
		if err := rows.Scan(&event, &id); err != nil {
			return nil, err
		}
		out[event] = append(out[event], id)
	}
	return out, rows.Err()
}

// SetRules replaces one account's matrix with the cells that are ticked. Rows
// for an event that is not in the table, or a channel that is not this
// account's, are refused rather than stored: the matrix is the contract.
func SetRules(ctx context.Context, db *store.DB, set *settings.Settings, user int64, ticked map[string][]int64) error {
	channels, err := ListChannels(ctx, db, set, user)
	if err != nil {
		return err
	}
	mine := map[int64]bool{}
	for _, c := range channels {
		mine[c.ID] = true
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM notification_rules WHERE user_id = ?`, user); err != nil {
		return fmt.Errorf("notify: clear rules: %w", err)
	}
	for event, ids := range ticked {
		if _, ok := LookupEvent(event); !ok {
			return fmt.Errorf("%q is not an event", event)
		}
		for _, id := range ids {
			if !mine[id] {
				return errors.New("that is not one of your channels")
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO notification_rules
				(user_id, event, channel_id, enabled) VALUES (?, ?, ?, 1)`, user, event, id); err != nil {
				return fmt.Errorf("notify: save rules: %w", err)
			}
		}
	}
	return tx.Commit()
}
