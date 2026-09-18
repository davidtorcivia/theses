package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/davidtorcivia/theses/internal/auth"
)

// The rail and the search box are the board's, and the two pages that render
// server side carry neither: nothing on them starts the module that would make
// either work. The flag that decides it is on the layout, so one GET of each
// page is the whole check.
func TestTheLayoutFlagDropsTheRailAndTheSearchBox(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()

	for _, c := range []struct {
		path  string
		plain bool
	}{
		{"/", false},
		{"/settings", true},
		{"/profile", true},
	} {
		_, body := h.get(c.path)
		for _, want := range []struct {
			what, mark string
		}{
			{"the rail", `<aside id="rail">`},
			{"the search box", `<button class="search"`},
			{"the rail toggle", `id="railtoggle"`},
		} {
			if got := strings.Contains(body, want.mark); got == c.plain {
				t.Errorf("%s: %s present = %v, want %v", c.path, want.what, got, !c.plain)
			}
		}
		if got := strings.Contains(body, `<body class="dirB plain">`); got != c.plain {
			t.Errorf("%s: plain body = %v, want %v", c.path, got, c.plain)
		}
		// Finding 12: the top bar label is the mockup's class, not the rail
		// row's, which is what gave it a border, a grid and a pointer.
		if !strings.Contains(body, `<span class="show">`) || strings.Contains(body, `<span class="ws">`) {
			t.Errorf("%s: the top bar label is not .show", c.path)
		}
	}
}

// Finding 13: the only route to the workspace settings was typing the address.
func TestTheSettingsLinkIsThereForOwnersOnly(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()

	if _, body := h.get("/profile"); !strings.Contains(body, `<a class="setgs" href="/settings">`) {
		t.Error("an owner has no Settings link in the top bar")
	}
	if _, body := h.get("/profile"); !strings.Contains(body, `<a href="/settings">Workspace settings</a>`) {
		t.Error("the profile page does not link to the workspace settings")
	}
	if _, body := h.get("/settings"); !strings.Contains(body, `<a href="/profile">Your profile</a>`) {
		t.Error("the settings page does not link back to the profile")
	}

	h.setRole(t, h.owner().ID, auth.RoleEditor)
	_, body := h.get("/profile")
	if strings.Contains(body, `class="setgs"`) || strings.Contains(body, `<a href="/settings">Workspace settings</a>`) {
		t.Error("an editor is offered a page they are refused")
	}
}

// Finding 16: signed in, there is nothing to sign in to, and the one action is
// the way back. An editor asking for the owners' page is the 403 with a live
// session; the same two addresses after signing out are the pair without one.
func TestTheErrorPageActionFollowsTheSession(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	h.setRole(t, h.owner().ID, auth.RoleEditor)

	for _, c := range []struct {
		name, path string
		status     int
		want, gone string
	}{
		{"404 with a session", "/no-such-page", http.StatusNotFound,
			`<a href="/">Back to work</a>`, "Sign in"},
		{"403 with a session", "/settings", http.StatusForbidden,
			`<a href="/">Back to work</a>`, "Sign in"},
	} {
		res, body := h.get(c.path)
		if res.StatusCode != c.status {
			t.Fatalf("%s: %d", c.name, res.StatusCode)
		}
		if !strings.Contains(body, c.want) {
			t.Errorf("%s: no %s", c.name, c.want)
		}
		if strings.Contains(body, c.gone) {
			t.Errorf("%s: still says %q", c.name, c.gone)
		}
	}

	h.signOut()
	res, body := h.get("/no-such-page")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("404 with no session: %d", res.StatusCode)
	}
	if !strings.Contains(body, `<a href="/login">Sign in</a>`) {
		t.Error("404 with no session does not offer a sign-in")
	}
}

// Finding 8: every form on these two pages answers with a redirect to the
// section it posted from, so the browser never sits on an address that only
// accepts POST and never lands at the top of a page thousands of pixels long.
func TestEveryFormRedirectsToItsSection(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()

	for _, c := range []struct {
		name, path, want string
		form             url.Values
	}{
		{"a settings section saves", "/settings", "/settings?saved=1#storage",
			url.Values{"section": {"storage"}, "storage.primary.region": {"us-west-000"}}},
		{"a settings section refuses a value", "/settings", "/settings#workspace",
			url.Values{"section": {"workspace"}, "workspace.episode_start": {"soon"}}},
		{"a mail test", "/settings/test/mail", "/settings#mail", url.Values{}},
		{"the outbox retry", "/settings/mail/retry", "/settings#mail", url.Values{}},
		{"a storage probe", "/settings/test/storage", "/settings#storage",
			url.Values{"prefix": {"storage.primary"}}},
		{"a backup now", "/settings/backups/now", "/settings#backups", url.Values{}},
		{"an invitation", "/settings/team/invite", "/settings#team",
			url.Values{"email": {"mara@example.com"}, "role": {auth.RoleEditor}}},
		{"a role change", "/settings/team/role", "/settings#team",
			url.Values{"user": {"1"}, "role": {auth.RoleEditor}}},
		{"a token", "/settings/tokens", "/settings#tokens",
			url.Values{"name": {"research agent"}, "scopes": {"read"}}},
		{"the notification defaults", "/settings/notifications/defaults",
			"/settings?saved=1#notifications", url.Values{"event": {"mentioned"}}},
		{"a webhook with no events", "/settings/integrations/webhook", "/settings#integrations",
			url.Values{"id": {"0"}, "url": {"https://example.com/hook"}}},
		{"the profile", "/profile", "/profile?saved=1#you",
			url.Values{"handle": {"ada"}, "name": {"Ada Lovelace"}, "initials": {"AL"},
				"email": {"ada@example.com"}, "colour": {Palette[1]}}},
		{"a password change that is refused", "/profile/password", "/profile#security",
			url.Values{"current": {"not my password"}, "password": {"a brand new password"}}},
		{"deleting the last owner", "/profile/delete", "/profile#danger", url.Values{}},
	} {
		form := url.Values{"csrf": {h.csrf("/settings")}}
		for k, v := range c.form {
			form[k] = v
		}
		res, _ := h.post(c.path, form)
		if res.StatusCode != http.StatusSeeOther {
			t.Errorf("%s: %s gave %d, want a redirect", c.name, c.path, res.StatusCode)
			continue
		}
		if got := res.Header.Get("Location"); got != c.want {
			t.Errorf("%s: landed on %s, want %s", c.name, got, c.want)
		}
	}
}

// The flash is what carries a value the page shows once across that redirect.
func TestTheOneTimeValuesSurviveTheRedirectOnlyOnce(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()

	res, body := h.postBack("/settings/tokens", url.Values{
		"csrf": {h.csrf("/settings")}, "name": {"research agent"}, "scopes": {"read"},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the token page gave %d", res.StatusCode)
	}
	token := tokenRe.FindStringSubmatch(body)
	if token == nil {
		t.Fatalf("the new token is not on the page it landed on:\n%s", body)
	}
	if !strings.Contains(body, `id="new-token"`) || !strings.Contains(body, `data-copy="new-token"`) {
		t.Error("the token has no copyable value beside a Copy button")
	}
	if _, again := h.get("/settings"); strings.Contains(again, token[1]) {
		t.Error("the flash was not spent: the token is on the next load too")
	}

	// Finding 20: the invitation says which of the three things happened to the
	// mail, and no mail server is configured here.
	_, body = h.postBack("/settings/team/invite", url.Values{
		"csrf": {h.csrf("/settings")}, "email": {"mara@example.com"}, "role": {auth.RoleEditor},
	})
	if link := inviteLinkRe.FindStringSubmatch(body); link == nil {
		t.Fatalf("the invitation link is not on the page it landed on:\n%s", body)
	}
	if !strings.Contains(body, "Mail is not configured") {
		t.Errorf("the notice does not say the mail is only queued:\n%s", firstNotice(body))
	}
	if !strings.Contains(body, `data-copy="new-invite"`) {
		t.Error("the invitation link has no Copy button")
	}
}
