package server

import (
	"errors"
	"net/http"
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
	UserKey            string
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
			UserKey: c.Config.UserKey, Server: c.Config.Server, Topic: c.Config.Topic,
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
				On: contains(rules[e.Key], c.ID)})
		}
		rows = append(rows, row)
	}
	return map[string]any{"Channels": views, "ChannelForms": forms, "Matrix": rows}, nil
}

func contains(ids []int64, id int64) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
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
			On: containsString(chosen, e.Key)})
	}

	hooks := make([]hookView, 0, len(channels))
	for _, c := range channels {
		h := hookView{ID: c.ID, URL: c.Config.URL, SecretSet: c.Config.Secret != "",
			Verified: c.Verified(), Column: c.Config.Column}
		for _, e := range notify.Events {
			h.Events = append(h.Events, hookEvent{Key: e.Key, Label: e.Label,
				On: containsString(c.Config.Events, e.Key)})
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

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
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
		s.renderProfile(w, r, http.StatusUnprocessableEntity, map[string]any{"Error": err.Error()})
		return
	}
	http.Redirect(w, r, "/profile?saved=1#notifications", http.StatusSeeOther)
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
	if c.Config.Token == "" {
		c.Config.Token = was.Config.Token
	}
	if c.Config.Secret == "" {
		c.Config.Secret = was.Config.Secret
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
	s.saveAndTest(w, r, c, u.Email, s.renderProfile, "/profile?saved=1#notifications")
}

// saveAndTest is the one path a channel is written by, whoever it belongs to.
func (s *Server) saveAndTest(w http.ResponseWriter, r *http.Request, c notify.Channel, email string,
	refuse func(http.ResponseWriter, *http.Request, int, map[string]any), back string) {
	// Email is verified by existing: the address is the account's own. Anything
	// else has to prove it works, and saying so is the save's job.
	if c.Kind == notify.KindEmail {
		c.VerifiedAt = s.auth.Now().Unix()
	} else {
		c.VerifiedAt = 0
	}
	saved, err := notify.SaveChannel(r.Context(), s.db, s.settings, c)
	if errors.Is(err, store.ErrNotFound) {
		s.errorPage(w, r, http.StatusNotFound)
		return
	}
	if err != nil {
		refuse(w, r, http.StatusUnprocessableEntity, map[string]any{"Error": err.Error()})
		return
	}
	if saved.Kind != notify.KindEmail {
		if err := s.notify.Test(r.Context(), saved, email); err != nil {
			refuse(w, r, http.StatusUnprocessableEntity, map[string]any{
				"NotifyResult": "Saved, but nothing reached it: " + err.Error(),
				"NotifyFailed": true,
			})
			return
		}
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

func (s *Server) postChannelTest(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	c, ok := s.channelOf(w, r, u.ID)
	if !ok {
		return
	}
	if err := s.notify.Test(r.Context(), c, u.Email); err != nil {
		s.renderProfile(w, r, http.StatusUnprocessableEntity, map[string]any{
			"NotifyResult": err.Error(), "NotifyFailed": true,
		})
		return
	}
	s.renderProfile(w, r, http.StatusOK, map[string]any{
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
	http.Redirect(w, r, "/profile?saved=1#notifications", http.StatusSeeOther)
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
		s.renderSettings(w, r, http.StatusUnprocessableEntity, map[string]any{"Error": err.Error()})
		return
	}
	http.Redirect(w, r, "/settings?saved=1#notifications", http.StatusSeeOther)
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
		s.renderSettings(w, r, http.StatusUnprocessableEntity, map[string]any{
			"Error": "A webhook that fires on nothing is a webhook nobody needs. Tick at least one event.",
		})
		return
	}
	s.saveAndTest(w, r, c, "", s.renderSettings, "/settings?saved=1#integrations")
}

func (s *Server) postWorkspaceWebhookTest(w http.ResponseWriter, r *http.Request) {
	c, ok := s.channelOf(w, r, 0)
	if !ok {
		return
	}
	if err := s.notify.Test(r.Context(), c, ""); err != nil {
		s.renderSettings(w, r, http.StatusUnprocessableEntity, map[string]any{
			"NotifyResult": err.Error(), "NotifyFailed": true,
		})
		return
	}
	s.renderSettings(w, r, http.StatusOK, map[string]any{"NotifyResult": "The webhook took it."})
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
	http.Redirect(w, r, "/settings?saved=1#integrations", http.StatusSeeOther)
}
