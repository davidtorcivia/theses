package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/notify"
)

func TestNotificationsNeedTheirScopes(t *testing.T) {
	h := newHarness(t)
	read := h.token(auth.ScopeRead)
	if w := h.do("PUT", "/api/v1/me/notifications", read, `{"rules":{}}`); w.Code != http.StatusForbidden {
		t.Errorf("a read token wrote the matrix: %d", w.Code)
	}
	if w := h.do("POST", "/api/v1/me/notifications/test", read, `{"channel":1}`); w.Code != http.StatusForbidden {
		t.Errorf("a read token tested a channel: %d", w.Code)
	}
	if w := h.do("GET", "/api/v1/me/notifications", h.token(auth.ScopeWrite), ""); w.Code != http.StatusForbidden {
		t.Errorf("a write only token read the matrix: %d", w.Code)
	}
}

func TestNotificationsRoundTripWithoutSecrets(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	token := h.token(auth.ScopeRead, auth.ScopeWrite)

	w := h.do("PUT", "/api/v1/me/notifications", token,
		`{"channels":[{"kind":"ntfy","topic":"alerts","token":"tk_secret","quiet_from":"23:00","quiet_to":"07:00"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT gave %d: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "tk_secret") {
		t.Fatalf("the answer carries the token: %s", w.Body)
	}
	var view notificationsView
	into(t, w, &view)
	if len(view.Channels) != 1 || view.Channels[0].Topic != "alerts" || !view.Channels[0].TokenSet {
		t.Fatalf("channels = %+v", view.Channels)
	}
	if view.Channels[0].Verified {
		t.Error("a channel created through the API is verified before anything reached it")
	}
	if len(view.Events) != len(notify.Events) {
		t.Errorf("events = %d, want the whole matrix", len(view.Events))
	}
	id := view.Channels[0].ID

	// The matrix, and an event nobody can tick.
	w = h.do("PUT", "/api/v1/me/notifications", token,
		`{"rules":{"mentioned":[`+strconv.FormatInt(id, 10)+`]}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("saving rules gave %d: %s", w.Code, w.Body)
	}
	if w := h.do("PUT", "/api/v1/me/notifications", token,
		`{"rules":{"nonsense":[`+strconv.FormatInt(id, 10)+`]}}`); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("an event nobody can tick gave %d", w.Code)
	}

	// A token left out keeps the stored one; an edit that moves the channel
	// takes its verified state away.
	if _, err := notify.SaveChannel(ctx, h.db, h.set, notify.Channel{
		ID: id, UserID: h.user.ID, Kind: notify.KindNtfy, VerifiedAt: 1,
		Config: notify.Config{Topic: "alerts", Token: "tk_secret"}}); err != nil {
		t.Fatal(err)
	}
	w = h.do("PUT", "/api/v1/me/notifications", token,
		`{"channels":[{"id":`+strconv.FormatInt(id, 10)+`,"kind":"ntfy","topic":"alerts"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT gave %d: %s", w.Code, w.Body)
	}
	back, err := notify.GetChannel(ctx, h.db, h.set, id)
	if err != nil {
		t.Fatal(err)
	}
	if back.Config.Token != "tk_secret" {
		t.Errorf("a token nobody sent was cleared: %+v", back.Config)
	}
	if !back.Verified() {
		t.Error("a channel that did not move lost its verified state")
	}

	w = h.do("PUT", "/api/v1/me/notifications", token,
		`{"channels":[{"id":`+strconv.FormatInt(id, 10)+`,"kind":"ntfy","topic":"somewhere-else"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT gave %d: %s", w.Code, w.Body)
	}
	if back, _ = notify.GetChannel(ctx, h.db, h.set, id); back.Verified() {
		t.Error("a channel that now points somewhere else is still verified")
	}

	// A list that leaves a channel out deletes it, which is what PUT means.
	if w := h.do("PUT", "/api/v1/me/notifications", token, `{"channels":[]}`); w.Code != http.StatusOK {
		t.Fatalf("PUT gave %d: %s", w.Code, w.Body)
	}
	channels, err := notify.ListChannels(ctx, h.db, h.set, h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != 0 {
		t.Errorf("channels = %+v, want none", channels)
	}
}

// An email channel goes to the address the account signs in with, so it is
// verified the moment it is saved. The profile page has always done this; a
// channel created through the API used to stay silent until somebody found the
// test button.
func TestAnEmailChannelIsVerifiedWhereverItIsCreated(t *testing.T) {
	h := newHarness(t)
	token := h.token(auth.ScopeRead, auth.ScopeWrite)

	w := h.do("PUT", "/api/v1/me/notifications", token, `{"channels":[{"kind":"email"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT gave %d: %s", w.Code, w.Body)
	}
	var view notificationsView
	into(t, w, &view)
	if len(view.Channels) != 1 || !view.Channels[0].Verified {
		t.Fatalf("channels = %+v, want one verified", view.Channels)
	}
}

func TestNotificationPUTRollsBackTheWholeReplacement(t *testing.T) {
	ctx := context.Background()
	t.Run("invalid later channel", func(t *testing.T) {
		h := newHarness(t)
		w := h.do("PUT", "/api/v1/me/notifications", h.token(auth.ScopeRead, auth.ScopeWrite),
			`{"channels":[{"kind":"email"},{"kind":"ntfy"}]}`)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("PUT gave %d: %s", w.Code, w.Body)
		}
		channels, err := notify.ListChannels(ctx, h.db, h.set, h.user.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(channels) != 0 {
			t.Fatalf("the valid prefix was kept: %+v", channels)
		}
	})

	t.Run("invalid rules after channels", func(t *testing.T) {
		h := newHarness(t)
		kept, err := notify.SaveChannel(ctx, h.db, h.set, notify.Channel{
			UserID: h.user.ID, Kind: notify.KindEmail,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := notify.SetRules(ctx, h.db, h.set, h.user.ID,
			map[string][]int64{"mentioned": {kept.ID}}); err != nil {
			t.Fatal(err)
		}
		w := h.do("PUT", "/api/v1/me/notifications", h.token(auth.ScopeRead, auth.ScopeWrite),
			`{"channels":[],"rules":{"nonsense":[]}}`)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("PUT gave %d: %s", w.Code, w.Body)
		}
		if _, err := notify.GetChannel(ctx, h.db, h.set, kept.ID); err != nil {
			t.Fatalf("the old channel was deleted before the rules were refused: %v", err)
		}
		rules, err := notify.Rules(ctx, h.db, h.user.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(rules["mentioned"]) != 1 || rules["mentioned"][0] != kept.ID {
			t.Fatalf("the old rules changed before the replacement was refused: %v", rules)
		}
	})

	t.Run("storage failure", func(t *testing.T) {
		h := newHarness(t)
		if _, err := h.db.ExecContext(ctx, `CREATE TRIGGER fail_second_channel
			BEFORE INSERT ON notification_channels WHEN NEW.kind = 'ntfy'
			BEGIN SELECT RAISE(ABORT, 'injected channel failure'); END`); err != nil {
			t.Fatal(err)
		}
		w := h.do("PUT", "/api/v1/me/notifications", h.token(auth.ScopeRead, auth.ScopeWrite),
			`{"channels":[{"kind":"email"},{"kind":"ntfy","topic":"alerts"}]}`)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("PUT gave %d: %s", w.Code, w.Body)
		}
		channels, err := notify.ListChannels(ctx, h.db, h.set, h.user.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(channels) != 0 {
			t.Fatalf("a failed replacement kept its valid prefix: %+v", channels)
		}
	})
}

func TestTestingSomebodyElsesChannelIsNotFound(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	hers, err := notify.SaveChannel(ctx, h.db, h.set, notify.Channel{
		Kind: notify.KindWebhook, Config: notify.Config{URL: "https://example.com/h"}})
	if err != nil {
		t.Fatal(err)
	}
	w := h.do("POST", "/api/v1/me/notifications/test", h.token(auth.ScopeWrite),
		`{"channel":`+strconv.FormatInt(hers.ID, 10)+`}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("testing a workspace channel through a personal route gave %d", w.Code)
	}
	if w := h.do("POST", "/api/v1/me/notifications/test", h.token(auth.ScopeWrite),
		`{"channel":999}`); w.Code != http.StatusNotFound {
		t.Errorf("a channel that does not exist gave %d", w.Code)
	}
}

// A channel the rules will not take is the caller's to correct; a failure to
// read or write is this side's, logged rather than repeated back.
func TestANotificationRefusalIsToldApartFromAFailure(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	token := h.token(auth.ScopeRead, auth.ScopeWrite)

	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"a kind that is not one", `{"channels":[{"kind":"pigeon"}]}`, http.StatusUnprocessableEntity},
		{"ntfy with no topic", `{"channels":[{"kind":"ntfy"}]}`, http.StatusUnprocessableEntity},
		{"a webhook that is not a URL", `{"channels":[{"kind":"webhook","url":"nowhere"}]}`,
			http.StatusUnprocessableEntity},
		{"quiet hours that are not a time",
			`{"channels":[{"kind":"email","quiet_from":"bedtime","quiet_to":"07:00"}]}`,
			http.StatusUnprocessableEntity},
		{"a body that is not JSON", `nonsense`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if w := h.do("PUT", "/api/v1/me/notifications", token, tc.body); w.Code != tc.want {
				t.Fatalf("answered %d, want %d: %s", w.Code, tc.want, w.Body)
			}
		})
	}

	// With the table gone, the matrix cannot be written, which is a fault on
	// this side rather than a body to correct.
	if _, err := h.db.ExecContext(ctx, `DROP TABLE notification_rules`); err != nil {
		t.Fatal(err)
	}
	w := h.do("PUT", "/api/v1/me/notifications", token, `{"rules":{}}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("a failure to write answered %d: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "notification_rules") {
		t.Errorf("the answer carries the driver's detail: %s", w.Body)
	}
}
