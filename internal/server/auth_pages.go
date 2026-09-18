package server

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/mail"
	"github.com/davidtorcivia/theses/internal/notify"
	"github.com/davidtorcivia/theses/internal/settings"
	"github.com/davidtorcivia/theses/internal/store"
)

// The frames each page cycles through, by name in web/static/art.
var (
	loginArt  = []string{"13", "22", "10", "04", "30", "17"}
	inviteArt = []string{"30", "01", "08"}
	setupArt  = []string{"08", "13", "30"}
	quietArt  = []string{"17"}
)

// page seeds what every template needs. IsOwner is what puts the Settings link
// in the top bar, and it asks the same question of the role that requireOwner
// asks of the route the link leads to. An auth page has no user at all, so the
// question is asked of the empty role and answered no.
func (s *Server) page(r *http.Request, title string, extra map[string]any) map[string]any {
	role := ""
	if u := userOf(r); u != nil {
		role = u.Role
	}
	d := map[string]any{
		"Title":     title,
		"Workspace": settings.Get[string](s.settings, "workspace.name"),
		"CSRF":      s.auth.CSRFToken(seedOf(r)),
		"User":      userOf(r),
		"IsOwner":   auth.Can(role, auth.CanSettings),
	}
	for k, v := range extra {
		d[k] = v
	}
	return d
}

func swatches(selected string) map[string]any {
	return map[string]any{"Colours": Palette, "Selected": selected}
}

// handleMinLength clamps as well as reads, for the same reason as sessionDays:
// a row written before the registry bounded the key would otherwise ask for an
// account name nobody can type.
func (s *Server) handleMinLength() int {
	n := settings.Get[int](s.settings, "signin.handle_min_length")
	if n < 2 {
		return 2
	}
	if n > 32 {
		return 32
	}
	return n
}

// sessionDays clamps as well as reads. The registry bounds what can be saved,
// but a row written before that bound existed would otherwise build an expiry
// that overflows and signs everyone out for good.
func (s *Server) sessionDays() int {
	days := settings.Get[int](s.settings, "signin.session_days")
	if days < 1 {
		return 1
	}
	if days > settings.MaxSessionDays {
		return settings.MaxSessionDays
	}
	return days
}

// Sign in.

func (s *Server) getLogin(w http.ResponseWriter, r *http.Request) {
	if _, err := s.auth.SessionUser(r.Context(), r); err == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "login.html", s.page(r, "Sign in", map[string]any{
		"ArtFrames": loginArt,
		"Handle":    "",
	}))
}

func (s *Server) postLogin(w http.ResponseWriter, r *http.Request) {
	handle := strings.ToLower(strings.TrimSpace(r.PostFormValue("handle")))
	refuse := func(status int, msg string) {
		s.render(w, r, status, "login.html", s.page(r, "Sign in", map[string]any{
			"ArtFrames": loginArt, "Handle": handle, "Error": msg,
		}))
	}

	if !s.auth.Allow(auth.BucketLogin, s.auth.ClientIP(r), handle) {
		refuse(http.StatusTooManyRequests, auth.ErrRateLimited.Error())
		return
	}
	u, err := s.auth.Authenticate(r.Context(), handle, r.PostFormValue("password"), r.PostFormValue("code"))
	if errors.Is(err, auth.ErrBadCredentials) {
		refuse(http.StatusUnauthorized, auth.ErrBadCredentials.Error())
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.auth.ResetLimits(auth.BucketLogin, handle)
	// The password, and the code where there was a secret to check it against,
	// have both been accepted. An account this workspace requires an
	// authenticator of and has none gets the enrollment page rather than a
	// session, and signs in at the end of it.
	if u.TOTPSecret == "" && s.needsAuthenticator(u.Role) {
		s.startEnrolment(w, r, u)
		return
	}
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

func (s *Server) postLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.auth.EndSession(r.Context(), w, r); err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// First run.

func (s *Server) getSetup(w http.ResponseWriter, r *http.Request) {
	if s.hasUsers.Load() {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "setup.html", s.page(r, "Set up", map[string]any{
		"ArtFrames": setupArt,
		"Form":      map[string]string{},
		"Swatches":  swatches(Palette[0]),
	}))
}

func (s *Server) postSetup(w http.ResponseWriter, r *http.Request) {
	if s.hasUsers.Load() {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	form, p, err := s.accountFromForm(r)
	if err != nil {
		s.render(w, r, http.StatusUnprocessableEntity, "setup.html", s.page(r, "Set up", map[string]any{
			"ArtFrames": setupArt, "Form": form, "Swatches": swatches(form["colour"]), "Error": err.Error(),
		}))
		return
	}
	p.Kind = "setup"
	p.Role = auth.RoleOwner
	if err := s.pending.put(w, s.cfg.CookieSecure, p); err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/setup/authenticator", http.StatusSeeOther)
}

// Invitations.

func (s *Server) getInvite(w http.ResponseWriter, r *http.Request) {
	inv, err := s.auth.Invitation(r.Context(), r.PathValue("token"))
	if err != nil {
		s.inviteRefused(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "invite.html", s.page(r, "Join", map[string]any{
		"ArtFrames":  inviteArt,
		"Invitation": inv,
		"Action":     "/invite/" + r.PathValue("token"),
		"Form":       map[string]string{},
		"Swatches":   swatches(Palette[0]),
	}))
}

func (s *Server) postInvite(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	inv, err := s.auth.Invitation(r.Context(), token)
	if err != nil {
		s.inviteRefused(w, r, err)
		return
	}
	form, p, err := s.accountFromForm(r)
	if err != nil {
		s.render(w, r, http.StatusUnprocessableEntity, "invite.html", s.page(r, "Join", map[string]any{
			"ArtFrames": inviteArt, "Invitation": inv, "Action": "/invite/" + token,
			"Form": form, "Swatches": swatches(form["colour"]), "Error": err.Error(),
		}))
		return
	}
	// The address comes from the invitation, not the form, so it is checked here
	// rather than in accountFromForm.
	if _, err := store.UserByEmail(r.Context(), s.db, inv.Email); err == nil {
		s.render(w, r, http.StatusUnprocessableEntity, "invite.html", s.page(r, "Join", map[string]any{
			"ArtFrames": inviteArt, "Invitation": inv, "Action": "/invite/" + token,
			"Form": form, "Swatches": swatches(form["colour"]),
			"Error": "An account already uses that email address. Sign in instead.",
		}))
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		s.fail(w, r, err)
		return
	}
	p.Kind = "invite"
	p.Role = inv.Role
	p.Email = inv.Email
	p.InvitationID = inv.ID
	if err := s.pending.put(w, s.cfg.CookieSecure, p); err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/invite/"+token+"/authenticator", http.StatusSeeOther)
}

// invitationGone is what an accept form finds when the invitation stopped being
// valid between the two steps: revoked, expired, or already used.
func (s *Server) invitationGone(w http.ResponseWriter, r *http.Request) {
	s.pending.clear(w, s.cfg.CookieSecure)
	s.errorPage(w, r, http.StatusNotFound)
}

func (s *Server) inviteRefused(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, auth.ErrTokenInvalid) || errors.Is(err, auth.ErrTokenExpired) {
		s.errorPage(w, r, http.StatusNotFound)
		return
	}
	s.fail(w, r, err)
}

// accountFromForm validates the details step of setup and invitation acceptance.
// The password is hashed here so that the clear text never reaches the cookie.
func (s *Server) accountFromForm(r *http.Request) (map[string]string, *pending, error) {
	form := map[string]string{
		"handle":   r.PostFormValue("handle"),
		"name":     strings.TrimSpace(r.PostFormValue("name")),
		"initials": strings.ToUpper(strings.TrimSpace(r.PostFormValue("initials"))),
		"colour":   r.PostFormValue("colour"),
		"email":    strings.TrimSpace(r.PostFormValue("email")),
	}
	if !validColour(form["colour"]) {
		form["colour"] = Palette[0]
	}

	handle, err := auth.NormaliseHandle(form["handle"])
	if err != nil {
		return form, nil, err
	}
	form["handle"] = handle
	if min := s.handleMinLength(); len(handle) < min {
		return form, nil, errShort(min)
	}
	if form["name"] == "" {
		return form, nil, errors.New("a name is needed; it is what appears on cards")
	}
	if n := len([]rune(form["initials"])); n < 1 || n > 2 {
		return form, nil, errors.New("initials are one or two characters")
	}
	if _, err := store.UserByHandle(r.Context(), s.db, handle); err == nil {
		return form, nil, errors.New("that account name is taken")
	} else if !errors.Is(err, store.ErrNotFound) {
		return form, nil, err
	}
	if form["email"] != "" {
		if _, err := store.UserByEmail(r.Context(), s.db, form["email"]); err == nil {
			return form, nil, errors.New("an account already uses that email address")
		} else if !errors.Is(err, store.ErrNotFound) {
			return form, nil, err
		}
	}

	hash, err := auth.HashPassword(r.PostFormValue("password"))
	if err != nil {
		return form, nil, err
	}
	e, err := auth.Enrol(handle)
	if err != nil {
		return form, nil, err
	}
	return form, &pending{
		Handle: handle, Name: form["name"], Initials: form["initials"],
		Colour: form["colour"], Email: form["email"], PasswordHash: hash,
		Secret: e.Secret, OTPURL: e.URL,
	}, nil
}

func errShort(min int) error {
	return fmt.Errorf("this workspace asks for account names of at least %d characters", min)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func validColour(c string) bool {
	for _, p := range Palette {
		if p == c {
			return true
		}
	}
	return false
}

// Enrollment, shared by setup, invitation acceptance and re-enrollment from the
// profile page. Which one it is comes from the cookie, not the URL.

func (s *Server) getEnrol(w http.ResponseWriter, r *http.Request) {
	p, err := s.pending.get(r)
	if err != nil {
		http.Redirect(w, r, s.enrolRestart(r), http.StatusSeeOther)
		return
	}
	qr, err := auth.QR(p.OTPURL)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "enrol.html", s.page(r, "Authenticator", map[string]any{
		"ArtFrames": quietArt,
		"Action":    r.URL.Path,
		"QR":        template.URL(qr),
		"Secret":    p.Secret,
	}))
}

func (s *Server) postEnrol(w http.ResponseWriter, r *http.Request) {
	p, err := s.pending.get(r)
	if err != nil {
		http.Redirect(w, r, s.enrolRestart(r), http.StatusSeeOther)
		return
	}
	if _, ok := auth.CheckCode(p.Secret, r.PostFormValue("code"), s.auth.Now()); !ok {
		qr, err := auth.QR(p.OTPURL)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		s.render(w, r, http.StatusUnprocessableEntity, "enrol.html", s.page(r, "Authenticator", map[string]any{
			"ArtFrames": quietArt, "Action": r.URL.Path, "QR": template.URL(qr), "Secret": p.Secret,
			"Error": "That code did not match. Check the clock on your phone and try the next one.",
		}))
		return
	}

	if p.Kind == "signin" {
		s.finishEnrolment(w, r, p)
		return
	}

	if p.Kind == "reenrol" {
		// The cookie says whose secret this replaces; the session has to agree,
		// or a stale cookie in a shared browser would rewrite someone else's.
		if u := userOf(r); u == nil || u.ID != p.UserID {
			s.pending.clear(w, s.cfg.CookieSecure)
			s.errorPage(w, r, http.StatusForbidden)
			return
		}
		if err := s.activity(r.Context(), p.UserID, "user", itoa(p.UserID), "totp", "", ""); err != nil {
			s.fail(w, r, err)
			return
		}
		if err := store.SetTOTPSecret(r.Context(), s.db, p.UserID, p.Secret); err != nil {
			s.fail(w, r, err)
			return
		}
		s.pending.clear(w, s.cfg.CookieSecure)
		http.Redirect(w, r, "/profile", http.StatusSeeOther)
		return
	}

	u := &store.User{
		Handle: p.Handle, Email: p.Email, Name: p.Name, Initials: p.Initials,
		Colour: p.Colour, Role: p.Role, PasswordHash: p.PasswordHash, TOTPSecret: p.Secret,
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer tx.Rollback()
	// The transaction takes the write lock at BEGIN, so this settles a race
	// between two first-run tabs and refuses a stale setup cookie besides.
	if p.Kind == "setup" {
		n, err := store.CountUsers(r.Context(), tx)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if n > 0 {
			s.pending.clear(w, s.cfg.CookieSecure)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
	}
	// The invitation was checked before the authenticator step, and could have
	// been revoked, run out or already been used since. It is read again inside
	// the transaction so a replayed cookie is refused here rather than dying on
	// a unique constraint further down.
	if p.InvitationID != 0 {
		inv, err := store.InvitationByID(r.Context(), tx, p.InvitationID)
		if errors.Is(err, store.ErrNotFound) {
			s.invitationGone(w, r)
			return
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if inv.AcceptedAt.Valid || inv.ExpiresAt <= s.auth.Now().Unix() {
			s.invitationGone(w, r)
			return
		}
	}
	id, err := store.CreateUser(r.Context(), tx, u)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// The account starts with its own address as a channel and the events the
	// owner chose, so that being mentioned reaches somebody from the first day
	// rather than from the first visit to the profile page.
	if err := notify.StartingChannels(r.Context(), tx, s.settings, id); err != nil {
		s.fail(w, r, err)
		return
	}
	if p.InvitationID != 0 {
		accepted, err := store.AcceptInvitation(r.Context(), tx, p.InvitationID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if !accepted {
			s.invitationGone(w, r)
			return
		}
		// The invitation is spent, so its mail must not go out later with a
		// link that now opens nothing.
		if err := mail.Abandon(r.Context(), tx, inviteRef(p.InvitationID), mail.Accepted); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	if err := store.InsertActivity(r.Context(), tx, "user", itoa(id), "", "user", itoa(id), "create", "", ""); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := tx.Commit(); err != nil {
		s.fail(w, r, err)
		return
	}

	u.ID = id
	u.SessionEpoch = 1
	s.hasUsers.Store(true)
	s.pending.clear(w, s.cfg.CookieSecure)
	if err := s.auth.StartSession(r.Context(), w, r, u, s.sessionDays()); err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// enrolRestart sends someone whose enrollment cookie is gone back to the start of
// whichever flow they were in.
func (s *Server) enrolRestart(r *http.Request) string {
	switch {
	case strings.HasPrefix(r.URL.Path, "/setup"):
		return "/setup"
	case strings.HasPrefix(r.URL.Path, "/profile"):
		return "/profile"
	default:
		return "/login"
	}
}

// Password reset.

func (s *Server) getReset(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "reset.html", s.page(r, "Reset", map[string]any{"ArtFrames": quietArt}))
}

func (s *Server) postReset(w http.ResponseWriter, r *http.Request) {
	const said = "If that account exists, mail is on its way."
	who := strings.TrimSpace(r.PostFormValue("who"))

	if s.auth.Allow(auth.BucketReset, s.auth.ClientIP(r), strings.ToLower(who)) {
		u, err := store.UserByHandleOrEmail(r.Context(), s.db, who)
		switch {
		case errors.Is(err, store.ErrNotFound):
			// Say the same thing either way.
		case err != nil:
			s.fail(w, r, err)
			return
		case u.Email == "":
			// An account can be made without an address, and there is nowhere to
			// send the link. The answer is the same either way.
		default:
			// The link is deliberately not logged, because anything that can
			// read the log could then use it. Only the mail carries it.
			// ponytail: the token row and the mail row are two commits, because
			// CreatePasswordReset holds its own handle; give it a store.Querier
			// and enqueue inside the same transaction when auth is next opened.
			token, err := s.auth.CreatePasswordReset(r.Context(), u.ID)
			if err != nil {
				s.fail(w, r, err)
				return
			}
			// The message expires with the link it carries: an hour of retries
			// is all a reset is worth, and a day of them would deliver a URL
			// that had died long before it arrived. The ref abandons any reset
			// mail for this account that has not gone out yet, so asking twice
			// delivers one link rather than two. The two statements share a
			// transaction so the older mail is never dropped without the newer
			// one taking its place.
			tx, err := s.db.BeginTx(r.Context(), nil)
			if err != nil {
				s.fail(w, r, err)
				return
			}
			defer tx.Rollback()
			if err := mail.Enqueue(r.Context(), tx, mail.Reset{
				To: u.Email, URL: s.cfg.BaseURL + "/reset/" + token, Expires: auth.ResetValidity,
			}.Message(), s.auth.Now().Add(auth.ResetValidity), "reset:"+itoa(u.ID)); err != nil {
				s.fail(w, r, err)
				return
			}
			if err := tx.Commit(); err != nil {
				s.fail(w, r, err)
				return
			}
			s.mail.Nudge()
			s.log.Info("password reset requested", "handle", u.Handle)
		}
	}
	s.render(w, r, http.StatusOK, "reset.html", s.page(r, "Reset", map[string]any{
		"ArtFrames": quietArt, "Notice": said,
	}))
}

func (s *Server) getResetToken(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "reset_new.html", s.page(r, "Reset", map[string]any{
		"ArtFrames": quietArt, "Action": r.URL.Path,
	}))
}

func (s *Server) postResetToken(w http.ResponseWriter, r *http.Request) {
	refuse := func(status int, msg string) {
		s.render(w, r, status, "reset_new.html", s.page(r, "Reset", map[string]any{
			"ArtFrames": quietArt, "Action": r.URL.Path, "Error": msg,
		}))
	}
	if !s.auth.Allow(auth.BucketReset, s.auth.ClientIP(r)) {
		refuse(http.StatusTooManyRequests, auth.ErrRateLimited.Error())
		return
	}

	// Only the length is checked before the token, because hashing is the
	// expensive part and a wrong token should not pay for it. The token is spent
	// before the hash for the same reason, and the length check first means a
	// password the rules refuse still does not burn the link.
	password := r.PostFormValue("password")
	if len([]rune(password)) < auth.MinPasswordLen {
		refuse(http.StatusUnprocessableEntity, fmt.Sprintf("a password is at least %d characters", auth.MinPasswordLen))
		return
	}
	u, err := s.auth.UsePasswordReset(r.Context(), r.PathValue("token"))
	if errors.Is(err, auth.ErrTokenInvalid) || errors.Is(err, auth.ErrTokenExpired) {
		s.errorPage(w, r, http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		refuse(http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := store.SetPasswordHash(r.Context(), s.db, u.ID, hash); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.activity(r.Context(), u.ID, "user", itoa(u.ID), "password-reset", "", ""); err != nil {
		s.fail(w, r, err)
		return
	}
	// A reset is also a way of throwing off anyone already signed in as them.
	if err := s.auth.SignOutEverywhere(r.Context(), u.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
