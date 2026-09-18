package server

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/store"
)

// withoutAuthenticator makes an account with a password and no authenticator,
// which is what a role promoted past the requirement, or an account whose
// secret was cleared, looks like. No handler makes one; the enrolment step is
// what this file is about.
func (h *harness) withoutAuthenticator(handle, role, password string) int64 {
	h.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		h.Fatal(err)
	}
	id, err := store.CreateUser(context.Background(), h.db, &store.User{
		Handle: handle, Email: handle + "@example.com", Name: "Mara Okafor",
		Initials: "MO", Colour: Palette[2], Role: role, PasswordHash: hash,
	})
	if err != nil {
		h.Fatal(err)
	}
	return id
}

// signIn posts the sign-in form and returns the redirect it answered with.
func (h *harness) signIn(handle, password, code string) (*http.Response, string) {
	h.Helper()
	res, body := h.post("/login", url.Values{
		"csrf": {h.csrf("/login")}, "handle": {handle}, "password": {password}, "code": {code},
	})
	return res, body
}

func TestSignInEnrolsWhenTheWorkspaceRequiresAnAuthenticator(t *testing.T) {
	const password = "a long enough password"
	cases := []struct {
		name    string
		require string
		role    string
		enrol   bool
	}{
		{"all requires an editor to enrol", "all", auth.RoleEditor, true},
		{"all requires a guest to enrol", "all", auth.RoleGuest, true},
		{"all requires an owner to enrol", "all", auth.RoleOwner, true},
		{"owners lets an editor in without one", "owners", auth.RoleEditor, false},
		{"owners lets a guest in without one", "owners", auth.RoleGuest, false},
		{"owners still requires an owner to enrol", "owners", auth.RoleOwner, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.setupOwner()
			if err := h.srv.settings.Set(context.Background(), "signin.require_totp",
				[]string{c.require}, 1); err != nil {
				t.Fatal(err)
			}
			id := h.withoutAuthenticator("mara", c.role, password)
			h.signOut()

			res, _ := h.signIn("mara", password, "")
			if res.StatusCode != http.StatusSeeOther {
				t.Fatalf("sign in gave %d, want a redirect", res.StatusCode)
			}
			want := "/"
			if c.enrol {
				want = "/login/authenticator"
			}
			if got := res.Header.Get("Location"); got != want {
				t.Fatalf("sign in went to %q, want %q", got, want)
			}

			u, err := store.UserByID(context.Background(), h.db, id)
			if err != nil {
				t.Fatal(err)
			}
			if !c.enrol {
				if u.TOTPSecret != "" {
					t.Fatal("an account that was let straight in was enrolled anyway")
				}
				// A session was started, so the app answers rather than the
				// sign-in page.
				if res, _ := h.get("/"); res.StatusCode != http.StatusOK {
					t.Fatalf("the app gave %d after a sign-in that needed no authenticator", res.StatusCode)
				}
				return
			}
			// Nothing is written until a code from the new secret comes back,
			// and until then there is no session.
			if u.TOTPSecret != "" {
				t.Fatal("the secret was stored before the code was checked")
			}
			if res, _ := h.get("/"); res.StatusCode != http.StatusSeeOther {
				t.Fatal("the enrolment page handed out a session before the code was checked")
			}

			_, page := h.get("/login/authenticator")
			m := secretRe.FindStringSubmatch(page)
			if m == nil {
				t.Fatal("the enrolment page did not print the key")
			}
			code, err := totp.GenerateCode(m[1], time.Now())
			if err != nil {
				t.Fatal(err)
			}
			res, _ = h.post("/login/authenticator", url.Values{
				"csrf": {csrfRe.FindStringSubmatch(page)[1]}, "code": {code},
			})
			if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/" {
				t.Fatalf("enrolment gave %d %s", res.StatusCode, res.Header.Get("Location"))
			}
			u, err = store.UserByID(context.Background(), h.db, id)
			if err != nil {
				t.Fatal(err)
			}
			if u.TOTPSecret != m[1] {
				t.Fatal("the authenticator was not stored")
			}
			if res, _ := h.get("/"); res.StatusCode != http.StatusOK {
				t.Fatalf("the app gave %d after enrolment", res.StatusCode)
			}
			// And the next sign-in asks for a code like everybody else's.
			h.signOut()
			if res, _ := h.signIn("mara", password, ""); res.StatusCode != http.StatusUnauthorized {
				t.Fatalf("a sign-in with no code gave %d, want a refusal", res.StatusCode)
			}
		})
	}
}

func TestEnrolmentOnTheWayInRefusesAWrongCode(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	id := h.withoutAuthenticator("mara", auth.RoleEditor, "a long enough password")
	h.signOut()

	if res, _ := h.signIn("mara", "a long enough password", ""); res.Header.Get("Location") != "/login/authenticator" {
		t.Fatalf("sign in went to %q", res.Header.Get("Location"))
	}
	_, page := h.get("/login/authenticator")
	res, _ := h.post("/login/authenticator", url.Values{
		"csrf": {csrfRe.FindStringSubmatch(page)[1]}, "code": {"000000"},
	})
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("a wrong code gave %d", res.StatusCode)
	}
	u, err := store.UserByID(context.Background(), h.db, id)
	if err != nil {
		t.Fatal(err)
	}
	if u.TOTPSecret != "" {
		t.Fatal("a wrong code enrolled the account anyway")
	}
}

// A sign-in enrolment cookie names whose secret it is, and the enrolment page
// is also reachable while signed in. Completing one there would rewrite the
// signed-in person's authenticator with a secret somebody else has.
func TestASignInEnrolmentCannotBeFinishedFromSomebodyElsesSession(t *testing.T) {
	h := newHarness(t)
	password, _ := h.setupOwner()
	victim := h.withoutAuthenticator("mara", auth.RoleEditor, "a long enough password")
	h.signOut()

	if res, _ := h.signIn("mara", "a long enough password", ""); res.Header.Get("Location") != "/login/authenticator" {
		t.Fatalf("sign in went to %q", res.Header.Get("Location"))
	}
	_, page := h.get("/login/authenticator")
	m := secretRe.FindStringSubmatch(page)
	if m == nil {
		t.Fatal("the enrolment page did not print the key")
	}
	code, err := totp.GenerateCode(m[1], time.Now())
	if err != nil {
		t.Fatal(err)
	}

	// The same browser now signs in as the owner, cookie and all.
	signInAsOwner(t, h, password)
	res, _ := h.post("/profile/authenticator", url.Values{
		"csrf": {h.csrf("/profile")}, "code": {code},
	})
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("the enrolment was accepted with %d", res.StatusCode)
	}
	u, err := store.UserByID(context.Background(), h.db, victim)
	if err != nil {
		t.Fatal(err)
	}
	if u.TOTPSecret != "" {
		t.Fatal("the other account was enrolled from this session")
	}
}

// signInAsOwner signs the owner in again, which the test above needs a session for.
func signInAsOwner(t *testing.T, h *harness, password string) {
	t.Helper()
	u, err := store.UserByHandle(context.Background(), h.db, "ada")
	if err != nil {
		t.Fatal(err)
	}
	code, err := totp.GenerateCode(u.TOTPSecret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	res, _ := h.signIn("ada", password, code)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/" {
		t.Fatalf("the owner could not sign in: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
}

// An invitation enrols before the account exists, whatever the setting says,
// so there is never an invited account without an authenticator to enforce
// the rule against.
func TestInvitationAcceptanceAlwaysEnrols(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	if err := h.srv.settings.Set(context.Background(), "signin.require_totp",
		[]string{"owners"}, 1); err != nil {
		t.Fatal(err)
	}
	res, body := h.post("/settings/team/invite", url.Values{
		"csrf": {h.csrf("/settings")}, "email": {"mara@example.com"}, "role": {auth.RoleGuest},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("invite gave %d", res.StatusCode)
	}
	link := inviteLinkRe.FindStringSubmatch(body)
	if link == nil {
		t.Fatal("the page did not show the invitation link")
	}
	// The link carries the configured base URL, not the test server's.
	at, err := url.Parse(link[1])
	if err != nil {
		t.Fatal(err)
	}
	path := at.Path
	h.signOut()

	_, page := h.get(path)
	res, _ = h.post(path, url.Values{
		"csrf": {csrfRe.FindStringSubmatch(page)[1]}, "handle": {"mara"}, "name": {"Mara Okafor"},
		"initials": {"MO"}, "colour": {Palette[2]}, "password": {"a long enough password"},
	})
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != path+"/authenticator" {
		t.Fatalf("accepting an invitation gave %d %s, want the enrolment page",
			res.StatusCode, res.Header.Get("Location"))
	}
}
