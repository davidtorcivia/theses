package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/notify"
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
		Events: c.Config.Events,
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
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			a.fail(w, http.StatusRequestEntityTooLarge, "that body is too large")
			return
		}
		a.fail(w, http.StatusBadRequest, "the body must be JSON with channels, rules, or both")
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
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		a.fail(w, http.StatusBadRequest, "the body must be JSON with a channel field")
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

// notificationRoutes is registered by Handler.
func (a *API) notificationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/me/notifications", a.scoped(auth.ScopeRead, a.notifications))
	mux.HandleFunc("PUT /api/v1/me/notifications", a.scoped(auth.ScopeWrite, a.putNotifications))
	mux.HandleFunc("POST /api/v1/me/notifications/test", a.scoped(auth.ScopeWrite, a.testNotification))
}
