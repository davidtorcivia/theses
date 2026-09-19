package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/backup"
	"github.com/davidtorcivia/theses/internal/notify"
	"github.com/davidtorcivia/theses/internal/store"
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
		// The top bar label is the mockup's class, not the rail
		// row's, which is what gave it a border, a grid and a pointer.
		if !strings.Contains(body, `<span class="show">`) || strings.Contains(body, `<span class="ws">`) {
			t.Errorf("%s: the top bar label is not .show", c.path)
		}
	}
}

// The only route to the workspace settings was typing the address.
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

// Signed in, there is nothing to sign in to, and the one action is
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

// Every form on these two pages answers with a redirect to the
// section it posted from, so the browser never sits on an address that only
// accepts POST and never lands at the top of a page thousands of pixels long.
// Every form on both pages is here, in the order that lets one row make what
// the next row needs.
func TestEveryFormRedirectsToItsSection(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.setupOwner()

	// The rows that act on something need it to exist first.
	mara, err := store.CreateUser(ctx, h.db, &store.User{Handle: "mara", Email: "mara@example.com",
		Name: "Mara Okafor", Initials: "MO", Colour: Palette[2], Role: auth.RoleEditor, PasswordHash: "x"})
	if err != nil {
		t.Fatal(err)
	}
	inviteID, _, err := h.srv.auth.CreateInvitation(ctx, "sam@example.com", auth.RoleEditor, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.srv.auth.CreateAPIToken(ctx, 1, "research agent", []string{"read"}); err != nil {
		t.Fatal(err)
	}
	tokens, err := store.ListAPITokens(ctx, h.db)
	if err != nil || len(tokens) != 1 {
		t.Fatalf("tokens = %v, %v", tokens, err)
	}
	hook, err := notify.SaveChannel(ctx, h.db, h.srv.settings, notify.Channel{
		UserID: 0, Kind: notify.KindWebhook,
		Config: notify.Config{URL: "https://127.0.0.1:1/hook", Events: []string{"mentioned"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	mine, err := notify.SaveChannel(ctx, h.db, h.srv.settings, notify.Channel{
		UserID: 1, Kind: notify.KindNtfy,
		Config: notify.Config{Server: "https://127.0.0.1:1", Topic: "alerts"},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name, path, want string
		form             url.Values
	}{
		// The eight sections of fields, each saving through the one handler.
		{"workspace saves", "/settings", "/settings?saved=workspace#workspace",
			url.Values{"section": {"workspace"}, "workspace.name": {"Workspace"}}},
		{"defaults save", "/settings", "/settings?saved=defaults#defaults",
			url.Values{"section": {"defaults"}, "defaults.columns": {"Research\nOutline"}}},
		{"storage saves", "/settings", "/settings?saved=storage#storage",
			url.Values{"section": {"storage"}, "storage.primary.region": {"us-west-000"}}},
		{"mail saves", "/settings", "/settings?saved=mail#mail",
			url.Values{"section": {"mail"}, "mail.host": {"smtp.example.com"}}},
		{"sign in saves", "/settings", "/settings?saved=signin#signin",
			url.Values{"section": {"signin"}, "signin.session_days": {"30"}}},
		{"backups save", "/settings", "/settings?saved=backups#backups",
			url.Values{"section": {"backups"}, "backups.time": {"03:30"}}},
		{"notifications save", "/settings", "/settings?saved=notifications#notifications",
			url.Values{"section": {"notifications"}, "notify.ntfy_server": {"https://ntfy.example.com"}}},
		{"an integration dialog saves", "/settings", "/settings?saved=integrations#integrations",
			url.Values{"section": {"integrations"}, "integrations.transistor.show_id": {"1"}}},
		{"a refused value stays on its section", "/settings", "/settings#workspace",
			url.Values{"section": {"workspace"}, "workspace.episode_start": {"soon"}}},
		{"a section nobody named", "/settings", "/settings?saved=1",
			url.Values{"workspace.name": {"Workspace"}}},

		// Storage, mail and backups each have their own tests beside the fields.
		{"the primary bucket test", "/settings/test/storage", "/settings#storage",
			url.Values{"prefix": {"storage.primary"}}},
		{"the recordings bucket test", "/settings/test/storage", "/settings#storage",
			url.Values{"prefix": {"storage.recordings"}}},
		{"the CORS rule on the primary bucket", "/settings/cors", "/settings#storage",
			url.Values{"prefix": {"storage.primary"}}},
		{"a test message", "/settings/test/mail", "/settings#mail", url.Values{}},
		{"the outbox retry", "/settings/mail/retry", "/settings#mail", url.Values{}},
		{"the backup key test", "/settings/test/backups", "/settings#backups", url.Values{}},
		{"a backup now", "/settings/backups/now", "/settings#backups", url.Values{}},
		{"a restore", "/settings/backups/restore", "/settings#backups",
			url.Values{"key": {backup.Prefix + "2026-09-18.tar.gz.age"}}},

		// The team.
		{"your own role", "/settings/team/role", "/settings#team",
			url.Values{"user": {"1"}, "role": {auth.RoleEditor}}},
		{"somebody else's role", "/settings/team/role", "/settings?saved=team#team",
			url.Values{"user": {itoa(mara)}, "role": {auth.RoleResearcher}}},
		{"an invitation", "/settings/team/invite", "/settings#team",
			url.Values{"email": {"noor@example.com"}, "role": {auth.RoleEditor}}},
		{"a resend", "/settings/team/invite/" + itoa(inviteID) + "/resend", "/settings#team", url.Values{}},
		{"a revoke", "/settings/team/invite/" + itoa(inviteID) + "/revoke", "/settings?saved=team#team", url.Values{}},

		// The tokens, which are their own anchor inside the team section.
		{"a token", "/settings/tokens", "/settings#tokens",
			url.Values{"name": {"second agent"}, "scopes": {"read"}}},
		{"a token revoke", "/settings/tokens/" + itoa(tokens[0].ID) + "/revoke",
			"/settings?saved=tokens#tokens", url.Values{}},

		// Notifications and the integrations below them.
		{"the notification defaults", "/settings/notifications/defaults",
			"/settings?saved=notifications#notifications", url.Values{"event": {"mentioned"}}},
		{"a webhook with no events", "/settings/integrations/webhook", "/settings#integrations",
			url.Values{"id": {"0"}, "url": {"https://example.com/hook"}}},
		{"a webhook test", "/settings/integrations/webhook/" + itoa(hook.ID) + "/test",
			"/settings#integrations", url.Values{}},
		{"a webhook delete", "/settings/integrations/webhook/" + itoa(hook.ID) + "/delete",
			"/settings?saved=integrations#integrations", url.Values{}},
		{"connecting Drive with no client", "/settings/integrations/drive/connect",
			"/settings#integrations", url.Values{}},
		{"the Drive test", "/settings/test/drive", "/settings#integrations", url.Values{}},
		{"disconnecting Drive", "/settings/integrations/drive/disconnect",
			"/settings?saved=integrations#integrations", url.Values{}},
		{"the Transistor test", "/settings/test/transistor", "/settings#integrations", url.Values{}},
		{"disconnecting Transistor", "/settings/integrations/transistor/disconnect",
			"/settings?saved=integrations#integrations", url.Values{}},

		// The profile.
		{"the profile", "/profile", "/profile?saved=you#you",
			url.Values{"handle": {"ada"}, "name": {"Ada Lovelace"}, "initials": {"AL"},
				"email": {"ada@example.com"}, "colour": {Palette[1]}}},
		{"a password change that is refused", "/profile/password", "/profile#security",
			url.Values{"current": {"not my password"}, "password": {"a brand new password"}}},
		{"the matrix", "/profile/notifications", "/profile?saved=notifications#notifications",
			url.Values{"rule": {"mentioned:1"}}},
		{"a channel that cannot be reached", "/profile/notifications/channel", "/profile#notifications",
			url.Values{"id": {"0"}, "kind": {"ntfy"}, "server": {"https://127.0.0.1:1"}, "topic": {"nope"}}},
		{"a channel test", "/profile/notifications/channel/" + itoa(mine.ID) + "/test",
			"/profile#notifications", url.Values{}},
		{"a channel delete", "/profile/notifications/channel/" + itoa(mine.ID) + "/delete",
			"/profile?saved=notifications#notifications", url.Values{}},
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

// The invitation rate limit used to answer 429 with the page rendered under it.
// It is a redirect like everything else now, so the refusal has to survive the
// flash or it is not said at all.
func TestTheInviteRateLimitStillRefusesAndSaysSo(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()

	var res *http.Response
	var body string
	for i := 0; i < 25; i++ {
		res, body = h.postBack("/settings/team/invite", url.Values{
			"csrf": {h.csrf("/settings")}, "email": {"mara@example.com"}, "role": {auth.RoleEditor},
		})
		if strings.Contains(body, auth.ErrRateLimited.Error()) {
			return
		}
		if res.StatusCode != http.StatusOK {
			t.Fatalf("invitation %d gave %d", i+1, res.StatusCode)
		}
	}
	t.Errorf("twenty five invitations and none was refused:\n%s", firstNotice(body))
}

// The notice belongs in the section the form
// posted from, because the page the browser lands on is scrolled to it and the
// heading at the top of the page is thousands of pixels away.
func TestTheNoticeIsPrintedInsideItsOwnSection(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()

	// Backups is the sixth section down, and Team is the one after it.
	for _, c := range []struct{ name, keep, want string }{
		{"a refusal", "soon", "not a number"},
		{"a save", "30", "Saved."},
	} {
		_, body := h.postBack("/settings", url.Values{
			"csrf": {h.csrf("/settings")}, "section": {"backups"}, "backups.keep": {c.keep},
		})
		start := strings.Index(body, `id="backups"`)
		next := strings.Index(body, `id="team"`)
		at := strings.Index(body, c.want)
		if at < 0 {
			t.Fatalf("%s: it is not on the page at all: %s", c.name, firstNotice(body))
		}
		if start < 0 || next < 0 || at < start || at > next {
			t.Errorf("%s: printed at %d, outside the backups section (%d to %d)", c.name, at, start, next)
		}
	}

	// A form that names no section keeps the notice at the top of the page.
	_, body := h.postBack("/settings", url.Values{
		"csrf": {h.csrf("/settings")}, "workspace.name": {"Workspace"},
	})
	if at, first := strings.Index(body, "Saved."), strings.Index(body, `id="workspace"`); at < 0 || at > first {
		t.Errorf("with no section the notice is not at the top of the page: %d against %d", at, first)
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

	// The invitation says which of the three things happened to the
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

// The flash names who it is for, when it stops being true and which page it
// belongs on, because it carries values that exist nowhere else. Each of the
// three is bent here the way a shared browser or a stale tab would bend it.
func TestTheFlashIsRefusedWhenItIsNotThisOnesToRead(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()

	for _, c := range []struct {
		name    string
		bend    func(*flash)
		path    string
		present bool
	}{
		{"as it was made", func(*flash) {}, "/settings", true},
		{"on the other page", func(*flash) {}, "/profile", false},
		{"made for another account", func(f *flash) { f.UserID = 99 }, "/settings", false},
		{"a minute old", func(f *flash) { f.Expires = time.Now().Add(-time.Hour).Unix() }, "/settings", false},
		{"pointed at another page", func(f *flash) { f.Path = "/profile" }, "/settings", false},
	} {
		res, _ := h.post("/settings/tokens", url.Values{
			"csrf": {h.csrf("/settings")}, "name": {"research agent"}, "scopes": {"read"},
		})
		if res.StatusCode != http.StatusSeeOther {
			t.Fatalf("%s: the token post gave %d", c.name, res.StatusCode)
		}
		h.bendFlash(t, c.bend)
		_, body := h.get(c.path)
		if got := strings.Contains(body, `id="new-token"`); got != c.present {
			t.Errorf("%s: the token on %s = %v, want %v", c.name, c.path, got, c.present)
		}
		if c.name == "on the other page" {
			// It was left where it was, so its own page still prints it once.
			if _, back := h.get("/settings"); !strings.Contains(back, `id="new-token"`) {
				t.Error("a flash for the settings page was eaten by the profile page")
			}
		}
		h.get("/settings") // spend whatever is left before the next round
	}

	// Signing out takes it with the session, so the next person here sees none.
	h.post("/settings/tokens", url.Values{
		"csrf": {h.csrf("/settings")}, "name": {"research agent"}, "scopes": {"read"},
	})
	h.signOut()
	u, _ := url.Parse(h.http.URL)
	for _, c := range h.client.Jar.Cookies(u) {
		if c.Name == flashCookie && c.Value != "" {
			t.Error("signing out left the flash in the browser")
		}
	}
}

// bendFlash opens the cookie the browser holds, changes it and puts it back,
// which is how a flash that is not this one's to read gets here at all.
func (h *harness) bendFlash(t *testing.T, bend func(*flash)) {
	t.Helper()
	u, _ := url.Parse(h.http.URL)
	for _, c := range h.client.Jar.Cookies(u) {
		if c.Name != flashCookie {
			continue
		}
		var f flash
		if !h.srv.pending.unseal(c.Value, &f) {
			t.Fatal("the flash cookie does not open")
		}
		bend(&f)
		value, err := h.srv.pending.seal(&f)
		if err != nil {
			t.Fatal(err)
		}
		h.client.Jar.SetCookies(u, []*http.Cookie{{Name: flashCookie, Value: value, Path: "/"}})
		return
	}
	t.Fatal("there is no flash cookie to bend")
}

// A message longer than the cookie can carry is cut rather than dropped whole,
// because a cookie over about four kilobytes never reaches the browser at all.
func TestALongNoticeIsCutRatherThanLost(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()

	// A bucket name long enough to make the refusal longer than a cookie.
	h.configureBucket("http://127.0.0.1:1", strings.Repeat("a-very-long-bucket-name.", 400))
	res, body := h.postBack("/settings/test/storage", url.Values{
		"csrf": {h.csrf("/settings")}, "prefix": {"storage.primary"},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the probe landed on %d", res.StatusCode)
	}
	if !strings.Contains(body, "The rest is in the log.") {
		t.Errorf("a long refusal was not cut to fit:\n%s", firstNotice(body))
	}
	if !strings.Contains(h.log.String(), "a notice was too long") {
		t.Error("what was cut is not in the log")
	}
}

// A message is cut by byte, and a provider answers in whatever alphabet it
// likes, so the cut has to land between runes and not inside one.
func TestTheCutForTheFlashLandsBetweenRunes(t *testing.T) {
	for _, c := range []struct{ name, text string }{
		{"short enough to keep whole", "the bucket said no"},
		{"exactly the limit", strings.Repeat("a", flashValueMax)},
		{"one byte over", strings.Repeat("a", flashValueMax+1)},
		{"a two byte rune across the cut", strings.Repeat("a", flashValueMax-1) + "ü" + "more"},
		{"a three byte rune across the cut", strings.Repeat("a", flashValueMax-1) + "→" + "more"},
		{"a four byte rune across the cut", strings.Repeat("a", flashValueMax-2) + "𝄞" + "more"},
		{"nothing but multibyte runes", strings.Repeat("→", flashValueMax)},
	} {
		got := cutForFlash(c.text)
		if !utf8.ValidString(got) {
			t.Errorf("%s: the cut left invalid UTF8", c.name)
		}
		if strings.ContainsRune(got, utf8.RuneError) && !strings.ContainsRune(c.text, utf8.RuneError) {
			t.Errorf("%s: the cut left a replacement character", c.name)
		}
		if len(c.text) <= flashValueMax {
			if got != c.text {
				t.Errorf("%s: a value that fits was changed", c.name)
			}
			continue
		}
		if !strings.HasSuffix(got, "… The rest is in the log.") {
			t.Errorf("%s: the cut does not say there is more", c.name)
		}
		if !strings.HasPrefix(c.text, strings.TrimSuffix(got, "… The rest is in the log.")) {
			t.Errorf("%s: what was kept is not the start of what was said", c.name)
		}
	}
}
