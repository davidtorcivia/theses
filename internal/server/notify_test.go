package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/davidtorcivia/theses/internal/notify"
)

// The owner is made through the real flow, so the account starts with the
// channel and the rules StartingChannels writes.
func TestProfileDrawsTheMatrixForTheStartingChannel(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()

	_, body := h.get("/profile")
	if !strings.Contains(body, `id="notifications"`) {
		t.Fatal("the profile page has no notifications section")
	}
	if !strings.Contains(body, "ada@example.com") {
		t.Error("the email channel does not print the account's address")
	}
	for _, e := range notify.Events {
		if !strings.Contains(body, e.Label) {
			t.Errorf("the matrix has no row for %q", e.Key)
		}
	}
	if !strings.Contains(body, `name="rule" value="assigned:1"`) {
		t.Error("the matrix has no cell for assigned on the first channel")
	}
	if !strings.Contains(body, `<dialog id="channel-new-ntfy">`) {
		t.Error("there is no way to add an ntfy channel")
	}
}

func TestMatrixSaveReplacesWhatWasTicked(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.setupOwner()

	res, _ := h.post("/profile/notifications", url.Values{
		"csrf": {h.csrf("/profile")}, "rule": {"moved:1", "done:1"},
	})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("saving the matrix gave %d", res.StatusCode)
	}
	rules, err := notify.Rules(ctx, h.db, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 || rules["moved"] == nil || rules["done"] == nil {
		t.Fatalf("rules = %v, want exactly the two that were ticked", rules)
	}

	// Nothing ticked is nothing ticked, which a form that only sends what is
	// checked has to be able to say.
	if _, _ = h.post("/profile/notifications", url.Values{"csrf": {h.csrf("/profile")}}); true {
		rules, err = notify.Rules(ctx, h.db, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(rules) != 0 {
			t.Errorf("rules = %v, want none", rules)
		}
	}
}

func TestAChannelIsAddedRemovedAndBelongsToItsAccount(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.setupOwner()

	// A webhook inside the deployment is refused by the address check, so its
	// test fails: it is stored, and stays unverified rather than silent.
	// The address is a literal, so nothing here asks a resolver.
	res, body := h.post("/profile/notifications/channel", url.Values{
		"csrf": {h.csrf("/profile")}, "id": {"0"}, "kind": {"webhook"},
		"url": {"https://127.0.0.1:1/hook"}, "secret": {"s3cret"},
		"quiet_from": {"23:00"}, "quiet_to": {"07:00"},
	})
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("adding an unreachable webhook gave %d", res.StatusCode)
	}
	if strings.Contains(body, "s3cret") {
		t.Error("the page printed the webhook secret back")
	}
	channels, err := notify.ListChannels(ctx, h.db, h.srv.settings, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != 2 {
		t.Fatalf("channels = %+v, want the email one and the webhook", channels)
	}
	hook := channels[1]
	if hook.Verified() {
		t.Error("a webhook nothing reached is verified")
	}
	if hook.QuietFrom != "23:00" || hook.Config.Secret != "s3cret" {
		t.Errorf("channel = %+v", hook)
	}

	// Quiet hours that are not times are refused rather than stored.
	res, _ = h.post("/profile/notifications/channel", url.Values{
		"csrf": {h.csrf("/profile")}, "id": {"0"}, "kind": {"ntfy"},
		"topic": {"alerts"}, "quiet_from": {"tonight"}, "quiet_to": {"07:00"},
	})
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("quiet hours that are not times gave %d", res.StatusCode)
	}

	res, _ = h.post("/profile/notifications/channel/"+itoa(hook.ID)+"/delete",
		url.Values{"csrf": {h.csrf("/profile")}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("deleting gave %d", res.StatusCode)
	}
	if channels, _ = notify.ListChannels(ctx, h.db, h.srv.settings, 1); len(channels) != 1 {
		t.Errorf("channels = %+v after the delete", channels)
	}
}

// Editing the quiet hours must not cost a channel its verified state: the
// worker abandons an unverified channel's queued rows for good.
func TestMovingOnlyTheQuietHoursKeepsAChannelProven(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.setupOwner()
	proven, err := notify.SaveChannel(ctx, h.db, h.srv.settings, notify.Channel{
		UserID: 1, Kind: notify.KindNtfy, VerifiedAt: 1,
		Config: notify.Config{Topic: "alerts", Token: "tk"}})
	if err != nil {
		t.Fatal(err)
	}

	// Only the quiet hours move, and the key is left blank as the page prints it.
	res, _ := h.post("/profile/notifications/channel", url.Values{
		"csrf": {h.csrf("/profile")}, "id": {itoa(proven.ID)}, "kind": {"ntfy"},
		"topic": {"alerts"}, "quiet_from": {"23:00"}, "quiet_to": {"07:00"},
	})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("saving the quiet hours gave %d", res.StatusCode)
	}
	back, err := notify.GetChannel(ctx, h.db, h.srv.settings, proven.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Verified() {
		t.Error("a channel that did not move lost its verified state")
	}
	if back.QuietFrom != "23:00" || back.Config.Token != "tk" {
		t.Errorf("channel = %+v", back)
	}

	// Moving the topic does take it away, and the test that follows fails.
	res, _ = h.post("/profile/notifications/channel", url.Values{
		"csrf": {h.csrf("/profile")}, "id": {itoa(proven.ID)}, "kind": {"ntfy"},
		"server": {"https://127.0.0.1:1"}, "topic": {"somewhere-else"},
	})
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("moving the channel gave %d, want the failed test", res.StatusCode)
	}
	if back, _ = notify.GetChannel(ctx, h.db, h.srv.settings, proven.ID); back.Verified() {
		t.Error("a channel that now points somewhere else is still verified")
	}
}

// The Pushover user key is a secret: it is never printed back into the page,
// and a field left blank keeps it.
func TestThePushoverKeyIsNeverPrintedBack(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.setupOwner()
	saved, err := notify.SaveChannel(ctx, h.db, h.srv.settings, notify.Channel{
		UserID: 1, Kind: notify.KindPushover, VerifiedAt: 1,
		Config: notify.Config{UserKey: "uk_abcdefgh1234"}})
	if err != nil {
		t.Fatal(err)
	}

	_, body := h.get("/profile")
	if strings.Contains(body, "uk_abcdefgh1234") {
		t.Fatal("the profile page printed the user key")
	}
	if !strings.Contains(body, `name="user_key"`) || !strings.Contains(body, `type="password" name="user_key"`) {
		t.Error("the user key field is not a password field")
	}

	res, _ := h.post("/profile/notifications/channel", url.Values{
		"csrf": {h.csrf("/profile")}, "id": {itoa(saved.ID)}, "kind": {"pushover"},
		"quiet_from": {"23:00"}, "quiet_to": {"07:00"},
	})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("saving with the key left blank gave %d", res.StatusCode)
	}
	back, err := notify.GetChannel(ctx, h.db, h.srv.settings, saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if back.Config.UserKey != "uk_abcdefgh1234" {
		t.Errorf("a key nobody retyped was lost: %+v", back.Config)
	}
}

func TestSomebodyElsesChannelIsNotFound(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.setupOwner()

	// A second account with a channel of its own.
	res, err := h.db.ExecContext(ctx, `INSERT INTO users
		(handle, email, name, initials, colour, role, password_hash, created_at)
		VALUES ('grace', 'grace@example.com', 'Grace', 'GH', ?, 'editor', 'x', unixepoch())`, Palette[2])
	if err != nil {
		t.Fatal(err)
	}
	other, _ := res.LastInsertId()
	hers, err := notify.SaveChannel(ctx, h.db, h.srv.settings, notify.Channel{
		UserID: other, Kind: notify.KindNtfy, Config: notify.Config{Topic: "hers"}})
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/test", "/delete"} {
		got, _ := h.post("/profile/notifications/channel/"+itoa(hers.ID)+path,
			url.Values{"csrf": {h.csrf("/profile")}})
		if got.StatusCode != http.StatusNotFound {
			t.Errorf("%s on somebody else's channel gave %d", path, got.StatusCode)
		}
	}
	edit, _ := h.post("/profile/notifications/channel", url.Values{
		"csrf": {h.csrf("/profile")}, "id": {itoa(hers.ID)}, "kind": {"ntfy"}, "topic": {"mine"},
	})
	if edit.StatusCode != http.StatusNotFound {
		t.Errorf("editing somebody else's channel gave %d", edit.StatusCode)
	}
}

func TestSettingsHoldsTheSharedCredentialsAndTheWebhooks(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.setupOwner()

	_, body := h.get("/settings")
	if !strings.Contains(body, `id="integrations"`) || !strings.Contains(body, `id="notifications"`) {
		t.Fatal("the settings page is missing the new sections")
	}
	if !strings.Contains(body, `name="notify.pushover_token"`) {
		t.Error("there is nowhere to put the shared Pushover token")
	}

	res, _ := h.post("/settings", url.Values{
		"csrf": {h.csrf("/settings")}, "notify.ntfy_server": {"https://ntfy.example.com"},
		"notify.digest_time": {"09:30"}, "notify.pushover_token": {"app-token"},
	})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("saving gave %d", res.StatusCode)
	}
	if got, err := h.srv.settings.Secret(ctx, "notify.pushover_token"); err != nil || got != "app-token" {
		t.Errorf("token = %q, %v", got, err)
	}
	if _, body := h.get("/settings"); strings.Contains(body, "app-token") {
		t.Error("the settings page printed the application token back")
	}

	// The defaults for a new account are their own form, so that clearing every
	// box clears them.
	res, _ = h.post("/settings/notifications/defaults", url.Values{
		"csrf": {h.csrf("/settings")}, "event": {"mentioned", "not an event"},
	})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("saving the defaults gave %d", res.StatusCode)
	}
	if got := notify.DefaultEvents(h.srv.settings); len(got) != 1 || got[0] != "mentioned" {
		t.Errorf("defaults = %v", got)
	}

	// A webhook that fires on nothing is refused before it is stored.
	res, _ = h.post("/settings/integrations/webhook", url.Values{
		"csrf": {h.csrf("/settings")}, "id": {"0"}, "url": {"https://example.com/hook"},
	})
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("a webhook with no events gave %d", res.StatusCode)
	}
	hooks, err := notify.ListChannels(ctx, h.db, h.srv.settings, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hooks) != 0 {
		t.Errorf("workspace channels = %+v, want none", hooks)
	}
}

func TestNotificationSectionsAreOwnerOnly(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	h.setRole(t, 1, "editor")
	for _, path := range []string{"/settings/notifications/defaults", "/settings/integrations/webhook"} {
		res, _ := h.post(path, url.Values{"csrf": {h.csrf("/profile")}})
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("%s as an editor gave %d", path, res.StatusCode)
		}
	}
}
