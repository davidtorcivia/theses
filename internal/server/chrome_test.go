package server

import (
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
