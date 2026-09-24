package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/notify"
	"github.com/davidtorcivia/theses/internal/settings"
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
		Config: notify.Config{Topic: "alerts", Token: "tk_secret"}}, settings.System()); err != nil {
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
		}, settings.System())
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
		Kind: notify.KindWebhook, Config: notify.Config{URL: "https://example.com/h", Events: []string{"moved"}}},
		settings.System())
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

// The workspace's webhooks over REST, in the order an owner's script would
// make the calls: refused without admin, refused without an event, never
// answering with the secret, keeping it when a change leaves it out, and not
// reaching somebody's own channel by its id.
func TestWorkspaceWebhookRoutes(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	admin := h.token(auth.ScopeAdmin)
	mine, err := notify.SaveChannel(ctx, h.db, h.set, notify.Channel{UserID: h.user.ID, Kind: notify.KindNtfy,
		Config: notify.Config{Topic: "alerts"}}, settings.System())
	if err != nil {
		t.Fatal(err)
	}
	url := "https://hooks.example.com/services/T0?token=abc"
	var id string
	steps := []struct {
		name, method, path, token, body string
		want                            int
		then                            func(t *testing.T, body string)
	}{
		{"a write token", "GET", "/api/v1/webhooks", h.token(auth.ScopeRead, auth.ScopeWrite), "", http.StatusForbidden, nil},
		{"no event", "POST", "/api/v1/webhooks", admin, `{"url":"` + url + `"}`, http.StatusUnprocessableEntity, nil},
		{"an event nobody fires", "POST", "/api/v1/webhooks", admin, `{"url":"` + url + `","events":["lunch"]}`, http.StatusUnprocessableEntity, nil},
		{"add one", "POST", "/api/v1/webhooks", admin, `{"url":"` + url + `","secret":"shh_signing","events":["moved"]}`, http.StatusOK,
			func(t *testing.T, body string) {
				var out struct{ Webhook ChannelView }
				if err := json.Unmarshal([]byte(body), &out); err != nil || out.Webhook.ID == 0 || !out.Webhook.SecretSet {
					t.Fatalf("added %s: %v", body, err)
				}
				id = strconv.FormatInt(out.Webhook.ID, 10)
			}},
		{"change its column", "PATCH", "/api/v1/webhooks/{id}", admin, `{"column":"Publication"}`, http.StatusOK,
			func(t *testing.T, body string) {
				n, _ := strconv.ParseInt(id, 10, 64)
				c, err := notify.GetChannel(ctx, h.db, h.set, n)
				if err != nil || c.Config.Secret != "shh_signing" || c.Config.Column != "Publication" {
					t.Errorf("stored %+v, %v", c.Config, err)
				}
			}},
		{"list", "GET", "/api/v1/webhooks", admin, "", http.StatusOK, func(t *testing.T, body string) {
			if !strings.Contains(body, `"column":"Publication"`) {
				t.Errorf("list = %s", body)
			}
		}},
		{"somebody's own channel", "PATCH", "/api/v1/webhooks/" + strconv.FormatInt(mine.ID, 10), admin, `{"column":"x"}`, http.StatusNotFound, nil},
		{"delete somebody's own", "DELETE", "/api/v1/webhooks/" + strconv.FormatInt(mine.ID, 10), admin, "", http.StatusNotFound, nil},
		{"delete it", "DELETE", "/api/v1/webhooks/{id}", admin, "", http.StatusOK, nil},
		{"delete it again", "DELETE", "/api/v1/webhooks/{id}", admin, "", http.StatusNotFound, nil},
	}
	for _, step := range steps {
		w := h.do(step.method, strings.Replace(step.path, "{id}", id, 1), step.token, step.body)
		if w.Code != step.want {
			t.Fatalf("%s: %d, want %d: %s", step.name, w.Code, step.want, w.Body)
		}
		if strings.Contains(w.Body.String(), "shh_signing") {
			t.Errorf("%s answered with the secret: %s", step.name, w.Body)
		}
		if step.then != nil {
			step.then(t, w.Body.String())
		}
	}
}

// A patch is laid over the webhook as committed, not as some earlier read of
// it: two callers changing different fields at once both keep their change,
// where a read outside the write would let the later one put back what the
// earlier one replaced.
func TestWebhookPatchesLandOnTheCommittedRow(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	who := core.Actor{Kind: core.KindUser, ID: h.user.ID, Name: h.user.Name}
	url := "https://example.com/hook"
	hook, err := h.api.SaveWebhook(ctx, who, 0, WebhookPatch{URL: &url, Events: []string{"moved"}})
	if err != nil {
		t.Fatal(err)
	}
	const rounds = 20
	var wg sync.WaitGroup
	errs := make(chan error, 2*rounds)
	for _, field := range []string{"column", "secret"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range rounds {
				value := field + strconv.Itoa(i)
				patch := WebhookPatch{Column: &value}
				if field == "secret" {
					patch = WebhookPatch{Secret: &value}
				}
				if _, err := h.api.SaveWebhook(ctx, who, hook.ID, patch); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	c, err := notify.GetChannel(ctx, h.db, h.set, hook.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := strconv.Itoa(rounds - 1)
	if c.Config.Column != "column"+last || c.Config.Secret != "secret"+last || c.Config.URL != url {
		t.Errorf("stored %+v, want both writers' last values", c.Config)
	}
}
