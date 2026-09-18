package server

import (
	"net/http"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/settings"
	"github.com/davidtorcivia/theses/internal/store"
)

// signin.require_totp is enforced here, at the one place a session is started
// from a password. Setup and invitation acceptance enroll before they write the
// account at all, so the only way to reach a sign-in without an authenticator
// is an account that lost one or a role that gained the requirement, and both
// land on the same page: enroll, then in.

// needsAuthenticator reports whether this role has to have an authenticator
// before it may sign in. Owners always do, whatever the setting says, because
// the setting itself is theirs to change.
func (s *Server) needsAuthenticator(role string) bool {
	if role == auth.RoleOwner {
		return true
	}
	return settings.Get[string](s.settings, "signin.require_totp") == "all"
}

// startEnrolment sends someone who has just proved a password, and who has no
// authenticator the workspace requires, to the enrollment page instead of to a
// session. The cookie carries the new secret; nothing is written until a code
// from it comes back.
func (s *Server) startEnrolment(w http.ResponseWriter, r *http.Request, u *store.User) {
	e, err := auth.Enrol(u.Handle)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.pending.put(w, s.cfg.CookieSecure, &pending{
		Kind: "signin", UserID: u.ID, Secret: e.Secret, OTPURL: e.URL,
	}); err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/login/authenticator", http.StatusSeeOther)
}

// finishEnrolment is postEnrol's branch for a sign-in that was held back for
// an authenticator. The code has already matched the secret in the cookie.
func (s *Server) finishEnrolment(w http.ResponseWriter, r *http.Request, p *pending) {
	// The cookie stands in for a session that does not exist yet, so a session
	// that does exist is somebody else's: this page is also reachable at
	// /profile/authenticator, where a stale cookie from a shared browser would
	// otherwise rewrite the signed-in person's secret.
	if userOf(r) != nil {
		s.pending.clear(w, s.cfg.CookieSecure)
		s.errorPage(w, r, http.StatusForbidden)
		return
	}

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer tx.Rollback()
	// The transaction takes the write lock at BEGIN, so the account is read as
	// it will be written. An authenticator enrolled in another tab since this
	// one started is not overwritten by it.
	u, err := store.UserByID(r.Context(), tx, p.UserID)
	if err != nil || u.TOTPSecret != "" {
		s.pending.clear(w, s.cfg.CookieSecure)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if err := store.SetTOTPSecret(r.Context(), tx, u.ID, p.Secret); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := store.InsertActivity(r.Context(), tx, "user", itoa(u.ID), "",
		"user", itoa(u.ID), "totp", "", ""); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := tx.Commit(); err != nil {
		s.fail(w, r, err)
		return
	}

	u.TOTPSecret = p.Secret
	s.pending.clear(w, s.cfg.CookieSecure)
	if err := s.auth.StartSession(r.Context(), w, r, u, s.sessionDays()); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := store.TouchUser(r.Context(), s.db, u.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
