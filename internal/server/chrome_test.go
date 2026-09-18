package server

import (
	"net/http"
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
