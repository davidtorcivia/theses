package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/store"
)

// profileSections are the anchors a form on the profile page may send the
// browser back to.
var profileSections = map[string]bool{"you": true, "security": true, "notifications": true, "danger": true}

// profileTo is the profile page's half of settingsTo.
func profileTo(section string, saved bool) string {
	return pageTo("/profile", section, profileSections[section], saved)
}

func (s *Server) getProfile(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	notifications, err := s.notifyProfile(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	data := s.page(r, "Profile", merge(map[string]any{
		"Plain":    true,
		"Section":  "",
		"Swatches": swatches(u.Colour),
	}, notifications))
	s.render(w, r, http.StatusOK, "profile.html", s.said(w, r, data, profileSections))
}

func (s *Server) postProfile(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	handle, err := auth.NormaliseHandle(r.PostFormValue("handle"))
	if err != nil {
		s.back(w, r, "/profile#you", map[string]any{"Error": err.Error()})
		return
	}
	if min := s.handleMinLength(); len(handle) < min {
		s.back(w, r, "/profile#you", map[string]any{"Error": errShort(min).Error()})
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	initials := strings.ToUpper(strings.TrimSpace(r.PostFormValue("initials")))
	email := strings.TrimSpace(r.PostFormValue("email"))
	colour := r.PostFormValue("colour")
	if !validColour(colour) {
		colour = u.Colour
	}
	if name == "" || initials == "" || len([]rune(initials)) > 2 {
		s.back(w, r, "/profile#you", map[string]any{
			"Error": "A name, and one or two initials, are what appear on cards.",
		})
		return
	}
	if handle != u.Handle {
		if _, err := store.UserByHandle(r.Context(), s.db, handle); err == nil {
			s.back(w, r, "/profile#you", map[string]any{"Error": "That account name is taken."})
			return
		} else if !errors.Is(err, store.ErrNotFound) {
			s.fail(w, r, err)
			return
		}
	}
	if other, err := store.UserByEmail(r.Context(), s.db, email); err == nil && other.ID != u.ID {
		s.back(w, r, "/profile#you", map[string]any{"Error": "Another account already uses that email address."})
		return
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, r, err)
		return
	}
	if err := s.write(r, "user", itoa(u.ID), "update", u.Handle, handle, func(q store.Querier) error {
		return store.UpdateProfile(r.Context(), q, u.ID, handle, name, initials, colour, email)
	}); err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, profileTo("you", true), http.StatusSeeOther)
}

func (s *Server) postPassword(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	if !auth.CheckPassword(u.PasswordHash, r.PostFormValue("current")) {
		s.back(w, r, "/profile#security", map[string]any{"Error": "That is not your current password."})
		return
	}
	hash, err := auth.HashPassword(r.PostFormValue("password"))
	if err != nil {
		s.back(w, r, "/profile#security", map[string]any{"Error": err.Error()})
		return
	}
	// The code is claimed last, so a password the rules refuse does not use one up.
	if u.TOTPSecret != "" {
		if err := s.auth.CheckTOTP(r.Context(), u, r.PostFormValue("code")); err != nil {
			s.back(w, r, "/profile#security", map[string]any{"Error": "That authenticator code did not match."})
			return
		}
	}
	if err := s.write(r, "user", itoa(u.ID), "password", "", "", func(q store.Querier) error {
		return store.SetPasswordHash(r.Context(), q, u.ID, hash)
	}); err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, profileTo("security", true), http.StatusSeeOther)
}

// postReenrol starts enrollment for someone already signed in. The new secret
// does not replace the old one until a code from it comes back.
func (s *Server) postReenrol(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	e, err := auth.Enrol(u.Handle)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.pending.put(w, s.cfg.CookieSecure, &pending{
		Kind: "reenrol", UserID: u.ID, Secret: e.Secret, OTPURL: e.URL,
	}); err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/profile/authenticator", http.StatusSeeOther)
}

func (s *Server) postSignOutEverywhere(w http.ResponseWriter, r *http.Request) {
	if err := s.write(r, "user", itoa(userOf(r).ID), "signout-everywhere", "", "", func(q store.Querier) error {
		return store.BumpSessionEpoch(r.Context(), q, userOf(r).ID)
	}); err != nil {
		s.fail(w, r, err)
		return
	}
	s.pending.clearFlash(w, s.cfg.CookieSecure)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) postDeleteAccount(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	// Counting first and deleting after let two owners delete themselves at the
	// same moment and leave nobody, so the count is part of the delete.
	if err := s.write(r, "user", itoa(u.ID), "delete", u.Handle, "", func(q store.Querier) error {
		deleted, err := store.DeleteUserKeepingAnOwner(r.Context(), q, u.ID)
		if err != nil {
			return err
		}
		if !deleted {
			return errRefused
		}
		return nil
	}); err != nil {
		if errors.Is(err, errRefused) {
			s.back(w, r, "/profile#danger", map[string]any{
				"Error": "You are the last owner. Make someone else an owner first.",
			})
			return
		}
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
