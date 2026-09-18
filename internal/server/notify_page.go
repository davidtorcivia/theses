package server

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/davidtorcivia/theses/internal/notify"
	"github.com/davidtorcivia/theses/internal/store"
)

// kindLabels is what each kind of channel is called on the page.
var kindLabels = map[string]string{
	notify.KindEmail:    "Email",
	notify.KindPushover: "Pushover",
	notify.KindNtfy:     "ntfy",
	notify.KindWebhook:  "Webhook",
}

type channelView struct {
	ID                 int64
	Kind, KindLabel    string
	Label              string
	Verified           bool
	QuietFrom, QuietTo string
	Digest             bool
	UserKeySet         bool
	Server, Topic      string
	URL                string
	TokenSet           bool
	SecretSet          bool
	// DialogID is what the Add or Edit button opens. A channel that does not
	// exist yet is named by its kind, because it has no id to be named by.
	DialogID string
}

type matrixCell struct {
	ChannelID int64
	KindLabel string
	On        bool
}

type matrixRow struct {
	Key, Label string
	Cells      []matrixCell
}

// hookView is one workspace webhook under Integrations. It has no rules row:
// the events it fires on are in its own config.
type hookView struct {
	ID        int64
	URL       string
	SecretSet bool
	Verified  bool
	Column    string
	Events    []hookEvent
}

type hookEvent struct {
	Key, Label string
	On         bool
}

// notifyProfile is the Notifications part of the profile page: the channels,
// the matrix and what each channel does outside its hours.
func (s *Server) notifyProfile(r *http.Request) (map[string]any, error) {
	u := userOf(r)
	channels, err := notify.ListChannels(r.Context(), s.db, s.settings, u.ID)
	if err != nil {
		return nil, err
	}
	rules, err := notify.Rules(r.Context(), s.db, u.ID)
	if err != nil {
		return nil, err
	}

	views := make([]channelView, 0, len(channels))
	have := map[string]bool{}
	for _, c := range channels {
		have[c.Kind] = true
		views = append(views, channelView{
			ID: c.ID, Kind: c.Kind, KindLabel: kindLabels[c.Kind], Label: c.Label(u.Email),
			Verified: c.Verified(), QuietFrom: c.QuietFrom, QuietTo: c.QuietTo, Digest: c.Digest,
			UserKeySet: c.Config.UserKey != "", Server: c.Config.Server, Topic: c.Config.Topic,
			URL: c.Config.URL, TokenSet: c.Config.Token != "", SecretSet: c.Config.Secret != "",
			DialogID: "channel-" + strconv.FormatInt(c.ID, 10),
		})
	}
	// The forms are the channels that exist and one blank per kind that can be
	// added, so the page draws them all from one loop. Email is the account's
	// own address, so there is only ever one of it.
	forms := append([]channelView{}, views...)
	for _, kind := range notify.Kinds {
		if kind == notify.KindEmail && have[kind] {
			continue
		}
		forms = append(forms, channelView{Kind: kind, KindLabel: kindLabels[kind],
			DialogID: "channel-new-" + kind})
	}

	rows := make([]matrixRow, 0, len(notify.Events))
	for _, e := range notify.Events {
		row := matrixRow{Key: e.Key, Label: e.Label}
		for _, c := range views {
			row.Cells = append(row.Cells, matrixCell{ChannelID: c.ID, KindLabel: c.KindLabel,
				On: slices.Contains(rules[e.Key], c.ID)})
		}
		rows = append(rows, row)
	}
	return map[string]any{"Channels": views, "ChannelForms": forms, "Matrix": rows}, nil
}

// notifySettings is the Notifications and Integrations sections of the settings
// page: the workspace defaults, the shared credentials and the webhooks.
func (s *Server) notifySettings(r *http.Request) (map[string]any, error) {
	channels, err := notify.ListChannels(r.Context(), s.db, s.settings, 0)
	if err != nil {
		return nil, err
	}
	chosen := notify.DefaultEvents(s.settings)
	defaults := make([]hookEvent, 0, len(notify.Events))
	for _, e := range notify.Events {
		defaults = append(defaults, hookEvent{Key: e.Key, Label: e.Label,
			On: slices.Contains(chosen, e.Key)})
	}

	hooks := make([]hookView, 0, len(channels))
	for _, c := range channels {
		h := hookView{ID: c.ID, URL: c.Config.URL, SecretSet: c.Config.Secret != "",
			Verified: c.Verified(), Column: c.Config.Column}
		for _, e := range notify.Events {
			h.Events = append(h.Events, hookEvent{Key: e.Key, Label: e.Label,
				On: slices.Contains(c.Config.Events, e.Key)})
		}
		hooks = append(hooks, h)
	}
	blank := hookView{}
	for _, e := range notify.Events {
		blank.Events = append(blank.Events, hookEvent{Key: e.Key, Label: e.Label})
	}
	hooks = append(hooks, blank)
	state, err := s.notify.State(r.Context())
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"NotifyDefaults": defaults,
		"Hooks":          hooks,
		"NotifyOutbox":   state,
	}, nil
}

// postNotificationRules saves the matrix. Every cell is a checkbox named
// "rule" carrying "<event>:<channel>", so a box that was cleared arrives as
// nothing at all and the whole matrix is replaced by what came back.
func (s *Server) postNotificationRules(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	ticked := map[string][]int64{}
	for _, cell := range r.PostForm["rule"] {
		event, id, ok := strings.Cut(cell, ":")
		if !ok {
			continue
		}
		channel, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			continue
		}
		ticked[event] = append(ticked[event], channel)
	}
	if err := notify.SetRules(r.Context(), s.db, s.settings, u.ID, ticked); err != nil {
		// A failure to read or write is this side's: logged, and answered with
		// the error page rather than printed to the person as a matrix they
		// could have posted differently.
		if errors.Is(err, notify.ErrStorage) {
			s.fail(w, r, err)
			return
		}
		s.back(w, r, "/profile#notifications", map[string]any{"Error": err.Error()})
		return
	}
	http.Redirect(w, r, profileTo("notifications", true), http.StatusSeeOther)
}

// channelFrom reads a channel out of a posted form, keeping the secrets that
// were left blank, exactly as the settings page keeps a secret nobody retyped.
func (s *Server) channelFrom(r *http.Request, owner int64) (notify.Channel, error) {
	id, _ := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	c := notify.Channel{
		ID:     id,
		UserID: owner,
		Kind:   r.PostFormValue("kind"),
		Config: notify.Config{
			UserKey: strings.TrimSpace(r.PostFormValue("user_key")),
			Server:  strings.TrimSpace(r.PostFormValue("server")),
			Topic:   strings.TrimSpace(r.PostFormValue("topic")),
			Token:   strings.TrimSpace(r.PostFormValue("token")),
			URL:     strings.TrimSpace(r.PostFormValue("url")),
			Secret:  strings.TrimSpace(r.PostFormValue("secret")),
			Column:  strings.TrimSpace(r.PostFormValue("column")),
		},
		QuietFrom: strings.TrimSpace(r.PostFormValue("quiet_from")),
		QuietTo:   strings.TrimSpace(r.PostFormValue("quiet_to")),
		Digest:    r.PostFormValue("digest") != "",
	}
	for _, e := range r.PostForm["event"] {
		if _, ok := notify.LookupEvent(e); ok {
			c.Config.Events = append(c.Config.Events, e)
		}
	}
	if id == 0 {
		return c, nil
	}
	was, err := notify.GetChannel(r.Context(), s.db, s.settings, id)
	if err != nil {
		return notify.Channel{}, err
	}
	if was.UserID != owner {
		return notify.Channel{}, store.ErrNotFound
	}
	// The kind of a channel never changes; what it points at does.
	c.Kind = was.Kind
	// A secret the page never printed cannot come back from it, so a field
	// left blank keeps what is stored.
	if c.Config.UserKey == "" {
		c.Config.UserKey = was.Config.UserKey
	}
	if c.Config.Token == "" {
		c.Config.Token = was.Config.Token
	}
	if c.Config.Secret == "" {
		c.Config.Secret = was.Config.Secret
	}
	// A channel that still points at the same place is still proven. An edit
	// that only moved the quiet hours or the digest flag must not cost it, or
	// the worker abandons everything already queued for it.
	if c.Config.SameDestination(was.Config) {
		c.VerifiedAt = was.VerifiedAt
	}
	return c, nil
}

// postChannel saves one of the account's channels and tests it, because a
// channel nobody has tested is one that silently receives nothing.
func (s *Server) postChannel(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	c, err := s.channelFrom(r, u.ID)
	if errors.Is(err, store.ErrNotFound) {
		s.errorPage(w, r, http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.saveAndTest(w, r, c, u.Email, "/profile#notifications", profileTo("notifications", true))
}

// saveAndTest is the one path a channel is written by, whoever it belongs to.
func (s *Server) saveAndTest(w http.ResponseWriter, r *http.Request, c notify.Channel, email, section, saved string) {
	// Anything that is new, or that now points somewhere else, has to prove it
	// works before anything is sent to it, and saying so is the save's job.
	// Email is the exception, verified by SaveChannel because the address is
	// the account's own.
	channel, err := notify.SaveChannel(r.Context(), s.db, s.settings, c)
	if errors.Is(err, store.ErrNotFound) {
		s.errorPage(w, r, http.StatusNotFound)
		return
	}
	if errors.Is(err, notify.ErrStorage) {
		s.fail(w, r, err)
		return
	}
	if err != nil {
		s.back(w, r, section, map[string]any{"Error": err.Error()})
		return
	}
	if channel.Kind != notify.KindEmail && !channel.Verified() {
		if err := s.notify.Test(r.Context(), channel, email); err != nil {
			s.back(w, r, section, map[string]any{
				"NotifyResult": "Saved, but nothing reached it: " + err.Error(),
				"NotifyFailed": true,
			})
			return
		}
	}
	http.Redirect(w, r, saved, http.StatusSeeOther)
}

func (s *Server) postChannelTest(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	c, ok := s.channelOf(w, r, u.ID)
	if !ok {
		return
	}
	if err := s.notify.Test(r.Context(), c, u.Email); err != nil {
		s.back(w, r, "/profile#notifications", map[string]any{
			"NotifyResult": err.Error(), "NotifyFailed": true,
		})
		return
	}
	s.back(w, r, "/profile#notifications", map[string]any{
		"NotifyResult": "Sent. If it did not arrive, the destination took it and dropped it.",
	})
}

func (s *Server) postChannelDelete(w http.ResponseWriter, r *http.Request) {
	if err := notify.DeleteChannel(r.Context(), s.db, pathID(r), userOf(r).ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.errorPage(w, r, http.StatusNotFound)
			return
		}
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, profileTo("notifications", true), http.StatusSeeOther)
}

// channelOf reads the channel a path names and refuses one that is not the
// owner's, so an id from another account is a 404 rather than a test send.
func (s *Server) channelOf(w http.ResponseWriter, r *http.Request, owner int64) (notify.Channel, bool) {
	c, err := notify.GetChannel(r.Context(), s.db, s.settings, pathID(r))
	if errors.Is(err, store.ErrNotFound) || (err == nil && c.UserID != owner) {
		s.errorPage(w, r, http.StatusNotFound)
		return notify.Channel{}, false
	}
	if err != nil {
		s.fail(w, r, err)
		return notify.Channel{}, false
	}
	return c, true
}

func pathID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

// postNotifyDefaults saves the events a new account starts with. It is its own
// route because a list of ticked boxes that is empty arrives as no field at
// all, which the settings form reads as "leave it alone".
func (s *Server) postNotifyDefaults(w http.ResponseWriter, r *http.Request) {
	var chosen []string
	for _, e := range r.PostForm["event"] {
		if _, ok := notify.LookupEvent(e); ok {
			chosen = append(chosen, e)
		}
	}
	if err := s.settings.Set(r.Context(), "notify.defaults",
		[]string{strings.Join(chosen, "\n")}, userOf(r).ID); err != nil {
		s.back(w, r, "/settings#notifications", map[string]any{"Error": err.Error()})
		return
	}
	http.Redirect(w, r, settingsTo("notifications", true), http.StatusSeeOther)
}

// postWorkspaceWebhook saves one of the workspace's webhooks, which belong to
// nobody and fire on what happened rather than on who it happened to.
func (s *Server) postWorkspaceWebhook(w http.ResponseWriter, r *http.Request) {
	c, err := s.channelFrom(r, 0)
	if errors.Is(err, store.ErrNotFound) {
		s.errorPage(w, r, http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	c.Kind = notify.KindWebhook
	if len(c.Config.Events) == 0 {
		s.back(w, r, "/settings#integrations", map[string]any{
			"Error": "A webhook that fires on nothing is a webhook nobody needs. Tick at least one event.",
		})
		return
	}
	s.saveAndTest(w, r, c, "", "/settings#integrations", settingsTo("integrations", true))
}

func (s *Server) postWorkspaceWebhookTest(w http.ResponseWriter, r *http.Request) {
	c, ok := s.channelOf(w, r, 0)
	if !ok {
		return
	}
	if err := s.notify.Test(r.Context(), c, ""); err != nil {
		s.back(w, r, "/settings#integrations", map[string]any{
			"NotifyResult": err.Error(), "NotifyFailed": true,
		})
		return
	}
	s.back(w, r, "/settings#integrations", map[string]any{"NotifyResult": "The webhook took it."})
}

func (s *Server) postWorkspaceWebhookDelete(w http.ResponseWriter, r *http.Request) {
	if err := notify.DeleteChannel(r.Context(), s.db, pathID(r), 0); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.errorPage(w, r, http.StatusNotFound)
			return
		}
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, settingsTo("integrations", true), http.StatusSeeOther)
}
