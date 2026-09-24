package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/notify"
	"github.com/davidtorcivia/theses/internal/settings"
	"github.com/davidtorcivia/theses/internal/store"
)

// A ChannelView is one destination as a token may see it. No secret is in it:
// a user key and a webhook secret come back as the last four characters or as
// "set", which is what tells two of them apart and nothing else.
type ChannelView struct {
	ID        int64    `json:"id"`
	Kind      string   `json:"kind"`
	Label     string   `json:"label"`
	Verified  bool     `json:"verified"`
	QuietFrom string   `json:"quiet_from,omitempty"`
	QuietTo   string   `json:"quiet_to,omitempty"`
	Digest    bool     `json:"digest"`
	Server    string   `json:"server,omitempty"`
	Topic     string   `json:"topic,omitempty"`
	URL       string   `json:"url,omitempty"`
	TokenSet  bool     `json:"token_set,omitempty"`
	SecretSet bool     `json:"secret_set,omitempty"`
	Events    []string `json:"events,omitempty"`
	Column    string   `json:"column,omitempty"`
}

// An EventView is one row of the matrix, so a client can draw it without
// knowing the list.
type EventView struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

type notificationsView struct {
	Channels []ChannelView      `json:"channels"`
	Events   []EventView        `json:"events"`
	Rules    map[string][]int64 `json:"rules"`
}

func channelView(c notify.Channel, email string) ChannelView {
	return ChannelView{
		ID: c.ID, Kind: c.Kind, Label: c.Label(email), Verified: c.Verified(),
		QuietFrom: c.QuietFrom, QuietTo: c.QuietTo, Digest: c.Digest,
		Server: c.Config.Server, Topic: c.Config.Topic, URL: c.Config.URL,
		TokenSet: c.Config.Token != "", SecretSet: c.Config.Secret != "",
		Events: c.Config.Events, Column: c.Config.Column,
	}
}

func (a *API) notifications(w http.ResponseWriter, r *http.Request, p Principal) {
	view, err := a.notificationsFor(r, p)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	a.writeJSON(w, http.StatusOK, view)
}

func (a *API) notificationsFor(r *http.Request, p Principal) (notificationsView, error) {
	channels, err := notify.ListChannels(r.Context(), a.db, a.set, p.User.ID)
	if err != nil {
		return notificationsView{}, err
	}
	rules, err := notify.Rules(r.Context(), a.db, p.User.ID)
	if err != nil {
		return notificationsView{}, err
	}
	view := notificationsView{Channels: []ChannelView{}, Rules: rules}
	if view.Rules == nil {
		view.Rules = map[string][]int64{}
	}
	for _, c := range channels {
		view.Channels = append(view.Channels, channelView(c, p.User.Email))
	}
	for _, e := range notify.Events {
		view.Events = append(view.Events, EventView{Key: e.Key, Label: e.Label})
	}
	return view, nil
}

// channelIn is one channel as a client sends it. A secret left out is the
// stored one kept, which is the same rule the settings page follows; a secret
// sent as an empty string clears it.
type channelIn struct {
	ID        int64    `json:"id"`
	Kind      string   `json:"kind"`
	QuietFrom string   `json:"quiet_from"`
	QuietTo   string   `json:"quiet_to"`
	Digest    bool     `json:"digest"`
	UserKey   *string  `json:"user_key"`
	Server    string   `json:"server"`
	Topic     string   `json:"topic"`
	Token     *string  `json:"token"`
	URL       string   `json:"url"`
	Secret    *string  `json:"secret"`
	Events    []string `json:"events"`
	Column    string   `json:"column"`
}

// putNotifications replaces the account's channels, its matrix, or both.
//
// Channels are a whole list: one this account has that the list leaves out is
// deleted, which is what PUT means. A channel created here arrives unverified
// and stays silent until the test route proves it works.
func (a *API) putNotifications(w http.ResponseWriter, r *http.Request, p Principal) {
	var body struct {
		Channels *[]channelIn        `json:"channels"`
		Rules    *map[string][]int64 `json:"rules"`
	}
	if !a.decode(w, r, maxBodyBytes, false, &body) {
		return
	}

	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	defer tx.Rollback()
	if body.Channels != nil {
		if err := a.saveChannels(r, p, tx, *body.Channels); err != nil {
			a.refuse(w, r, err)
			return
		}
	}
	if body.Rules != nil {
		if err := notify.SetRulesTx(r.Context(), tx, a.set, p.User.ID, *body.Rules); err != nil {
			a.refuse(w, r, err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		a.serverError(w, r, err)
		return
	}
	view, err := a.notificationsFor(r, p)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	a.writeJSON(w, http.StatusOK, view)
}

func (a *API) saveChannels(r *http.Request, p Principal, tx *sql.Tx, list []channelIn) error {
	existing, err := notify.ListChannels(r.Context(), tx, a.set, p.User.ID)
	if err != nil {
		return err
	}
	was := map[int64]notify.Channel{}
	for _, c := range existing {
		was[c.ID] = c
	}
	kept := map[int64]bool{}
	for _, in := range list {
		c := notify.Channel{
			ID: in.ID, UserID: p.User.ID, Kind: in.Kind,
			QuietFrom: in.QuietFrom, QuietTo: in.QuietTo, Digest: in.Digest,
			Config: notify.Config{
				Server: in.Server, Topic: in.Topic, URL: in.URL,
				Events: in.Events, Column: in.Column,
			},
		}
		if in.ID != 0 {
			old, ok := was[in.ID]
			if !ok {
				return store.ErrNotFound
			}
			// The kind never changes, and a secret left out keeps the stored
			// one, so the comparison below sees what the channel will hold.
			c.Kind = old.Kind
			c.Config.UserKey, c.Config.Token, c.Config.Secret = old.Config.UserKey, old.Config.Token, old.Config.Secret
			c.VerifiedAt = old.VerifiedAt
			kept[in.ID] = true
		}
		if in.UserKey != nil {
			c.Config.UserKey = *in.UserKey
		}
		if in.Token != nil {
			c.Config.Token = *in.Token
		}
		if in.Secret != nil {
			c.Config.Secret = *in.Secret
		}
		// A channel that now points somewhere else has to be tested again
		// before anything is sent to it.
		if in.ID != 0 && !c.Config.SameDestination(was[in.ID].Config) {
			c.VerifiedAt = 0
		}
		if _, err := notify.SaveChannelTx(r.Context(), tx, a.set, c, p.Actor()); err != nil {
			return err
		}
	}
	for _, c := range existing {
		if !kept[c.ID] {
			if err := notify.DeleteChannelTx(r.Context(), tx, a.set, c.ID, p.User.ID, p.Actor()); err != nil {
				return err
			}
		}
	}
	return nil
}

// testNotification sends one message to one of the caller's channels and marks
// it verified when it arrives.
func (a *API) testNotification(w http.ResponseWriter, r *http.Request, p Principal) {
	var body struct {
		Channel int64 `json:"channel"`
	}
	if !a.decode(w, r, maxBodyBytes, false, &body) {
		return
	}
	c, err := notify.GetChannel(r.Context(), a.db, a.set, body.Channel)
	if errors.Is(err, store.ErrNotFound) || (err == nil && c.UserID != p.User.ID) {
		a.fail(w, http.StatusNotFound, "no such channel")
		return
	}
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	// The notifier here sends and nothing else: the worker that empties the
	// outbox is the one main runs.
	if err := notify.New(a.db, a.set, a.log, "").Test(r.Context(), c, p.User.Email); err != nil {
		a.fail(w, http.StatusBadGateway, err.Error())
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"sent": true, "channel": c.ID})
}

// notificationRoutes is registered by Handler. The workspace's webhooks are
// the settings page's, so they take admin, which only an owner's token has.
func (a *API) notificationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/me/notifications", a.scoped(auth.ScopeRead, a.notifications))
	mux.HandleFunc("PUT /api/v1/me/notifications", a.scoped(auth.ScopeWrite, a.putNotifications))
	mux.HandleFunc("POST /api/v1/me/notifications/test", a.scoped(auth.ScopeWrite, a.testNotification))
	mux.HandleFunc("GET /api/v1/webhooks", a.scoped(auth.ScopeAdmin, a.listWebhooks))
	mux.HandleFunc("POST /api/v1/webhooks", a.scoped(auth.ScopeAdmin, a.saveWebhook))
	mux.HandleFunc("PATCH /api/v1/webhooks/{id}", a.scoped(auth.ScopeAdmin, a.saveWebhook))
	mux.HandleFunc("DELETE /api/v1/webhooks/{id}", a.scoped(auth.ScopeAdmin, a.deleteWebhook))
	mux.HandleFunc("POST /api/v1/webhooks/{id}/test", a.scoped(auth.ScopeAdmin, a.testWebhook))
}

// A WebhookPatch is the fields of a workspace webhook a call names. Every one
// left out keeps what is stored, so a secret the caller never saw is not
// cleared by leaving it out, the rule the settings page follows.
type WebhookPatch struct {
	URL    *string  `json:"url,omitempty" jsonschema:"the http or https address the workspace posts to"`
	Secret *string  `json:"secret,omitempty" jsonschema:"what signs the body as X-Theses-Signature; an empty string clears it"`
	Events []string `json:"events,omitempty" jsonschema:"the events it fires on, as the notification matrix names them"`
	Column *string  `json:"column,omitempty" jsonschema:"fire a card move only when it lands in this column; empty for any"`
}

// Webhooks is the workspace's webhooks, for both surfaces.
func (a *API) Webhooks(ctx context.Context) ([]ChannelView, error) {
	hooks, err := notify.ListChannels(ctx, a.db, a.set, 0)
	if err != nil {
		return nil, err
	}
	out := []ChannelView{}
	for _, c := range hooks {
		out = append(out, channelView(c, ""))
	}
	return out, nil
}

// webhook is one of the workspace's webhooks, and not there when the id names
// somebody's own channel.
func (a *API) webhook(ctx context.Context, q store.Querier, id int64) (notify.Channel, error) {
	c, err := notify.GetChannel(ctx, q, a.set, id)
	if err == nil && c.UserID != 0 {
		return notify.Channel{}, store.ErrNotFound
	}
	return c, err
}

// SaveWebhook adds a workspace webhook when id is zero and changes the one it
// names otherwise. One that now points somewhere else is unproven again, and
// nothing is sent to it until TestWebhook reaches it.
//
// The row is read inside the transaction that writes it, which holds the
// write lock from the start, so a patch is laid over the row as committed and
// two patches of different fields cannot undo each other.
func (a *API) SaveWebhook(ctx context.Context, who core.Actor, id int64, in WebhookPatch) (ChannelView, error) {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return ChannelView{}, fmt.Errorf("%w: save webhook: %w", notify.ErrStorage, err)
	}
	defer tx.Rollback()
	was := notify.Channel{Kind: notify.KindWebhook}
	if id != 0 {
		if was, err = a.webhook(ctx, tx, id); err != nil {
			return ChannelView{}, err
		}
	}
	c := was
	if in.URL != nil {
		c.Config.URL = *in.URL
	}
	if in.Secret != nil {
		c.Config.Secret = *in.Secret
	}
	if in.Events != nil {
		c.Config.Events = in.Events
	}
	if in.Column != nil {
		c.Config.Column = *in.Column
	}
	if !c.Config.SameDestination(was.Config) {
		c.VerifiedAt = 0
	}
	saved, err := notify.SaveChannelTx(ctx, tx, a.set, c, settingsActor(who))
	if err != nil {
		return ChannelView{}, err
	}
	if err := tx.Commit(); err != nil {
		return ChannelView{}, fmt.Errorf("%w: save webhook: %w", notify.ErrStorage, err)
	}
	return channelView(saved, ""), nil
}

// DeleteWebhook removes one of the workspace's webhooks.
func (a *API) DeleteWebhook(ctx context.Context, who core.Actor, id int64) error {
	return notify.DeleteChannel(ctx, a.db, a.set, id, 0, settingsActor(who))
}

// A WebhookTest is what the test message met: sent, or what the destination
// said, with the webhook's secrets already taken out of it.
type WebhookTest struct {
	Sent  bool   `json:"sent"`
	Error string `json:"error,omitempty"`
}

// TestWebhook sends the test message to one of the workspace's webhooks and
// marks it verified when it arrives. Only a webhook that is not there is an
// error; one that did not answer is the result.
func (a *API) TestWebhook(ctx context.Context, id int64) (WebhookTest, error) {
	c, err := a.webhook(ctx, a.db, id)
	if err != nil {
		return WebhookTest{}, err
	}
	if err := notify.New(a.db, a.set, a.log, "").Test(ctx, c, ""); err != nil {
		return WebhookTest{Error: err.Error()}, nil
	}
	return WebhookTest{Sent: true}, nil
}

// settingsActor is a command's actor as the tables outside core record one.
func settingsActor(a core.Actor) settings.Actor {
	return settings.Actor{Kind: a.Kind, ID: strconv.FormatInt(a.ID, 10), Via: a.Via, UserID: a.ID}
}

func (a *API) listWebhooks(w http.ResponseWriter, r *http.Request, _ Principal) {
	hooks, err := a.Webhooks(r.Context())
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"webhooks": hooks})
}

// saveWebhook is both the POST that adds one and the PATCH that changes one:
// the path's id is all that tells them apart.
func (a *API) saveWebhook(w http.ResponseWriter, r *http.Request, p Principal) {
	var id int64
	if r.PathValue("id") != "" {
		var ok bool
		if id, ok = a.pathID(w, r, "webhook"); !ok {
			return
		}
	}
	var in WebhookPatch
	if !a.decode(w, r, maxBodyBytes, true, &in) {
		return
	}
	hook, err := a.SaveWebhook(r.Context(), actorOf(p), id, in)
	if err != nil {
		a.refuse(w, r, err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"webhook": hook})
}

func (a *API) deleteWebhook(w http.ResponseWriter, r *http.Request, p Principal) {
	id, ok := a.pathID(w, r, "webhook")
	if !ok {
		return
	}
	if err := a.DeleteWebhook(r.Context(), actorOf(p), id); err != nil {
		a.refuse(w, r, err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (a *API) testWebhook(w http.ResponseWriter, r *http.Request, _ Principal) {
	id, ok := a.pathID(w, r, "webhook")
	if !ok {
		return
	}
	got, err := a.TestWebhook(r.Context(), id)
	if err != nil {
		a.refuse(w, r, err)
		return
	}
	if !got.Sent {
		a.fail(w, http.StatusBadGateway, got.Error)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"sent": true, "webhook": id})
}
