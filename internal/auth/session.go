package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/davidtorcivia/theses/internal/store"
)

const (
	SessionCookie = "theses_session"
	CSRFCookie    = "theses_csrf"
)

// Authenticate checks an account name, password and authenticator code together,
// so a failure never says which of the three was wrong.
func (a *Auth) Authenticate(ctx context.Context, handle, password, code string) (*store.User, error) {
	u, err := store.UserByHandle(ctx, a.db, handle)
	if errors.Is(err, store.ErrNotFound) {
		burnPasswordTime(password)
		return nil, ErrBadCredentials
	}
	if err != nil {
		return nil, err
	}
	if !CheckPassword(u.PasswordHash, password) {
		return nil, ErrBadCredentials
	}
	if u.TOTPSecret != "" {
		if err := a.CheckTOTP(ctx, u, code); err != nil {
			return nil, ErrBadCredentials
		}
	}
	return u, nil
}

// StartSession writes a new session row and sets the cookie. The cookie holds a
// random token; the database holds its HMAC, so a copy of the database yields no
// working cookies.
func (a *Auth) StartSession(ctx context.Context, w http.ResponseWriter, r *http.Request, u *store.User, days int) error {
	token, err := randomToken()
	if err != nil {
		return err
	}
	expires := a.Now().Add(time.Duration(days) * 24 * time.Hour)
	if err := store.CreateSession(ctx, a.db, u.ID, a.mac(token), u.SessionEpoch,
		expires.Unix(), truncate(r.UserAgent(), 200)); err != nil {
		return fmt.Errorf("start session: %w", err)
	}
	http.SetCookie(w, a.cookie(SessionCookie, token, expires))
	return nil
}

// SessionUser resolves the session cookie. It returns store.ErrNotFound when
// there is no usable session, including an expired or revoked one.
func (a *Auth) SessionUser(ctx context.Context, r *http.Request) (*store.User, error) {
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		return nil, store.ErrNotFound
	}
	token, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return nil, store.ErrNotFound
	}
	_, u, err := store.SessionByHMAC(ctx, a.db, a.mac(token), a.Now().Unix())
	return u, err
}

// EndSession deletes this browser's session and clears the cookie.
func (a *Auth) EndSession(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	if c, err := r.Cookie(SessionCookie); err == nil {
		if token, err := base64.RawURLEncoding.DecodeString(c.Value); err == nil {
			if err := store.DeleteSession(ctx, a.db, a.mac(token)); err != nil {
				return err
			}
		}
	}
	http.SetCookie(w, a.cookie(SessionCookie, nil, time.Unix(0, 0)))
	return nil
}

// SignOutEverywhere bumps the user's session epoch, which strands every session
// row including this browser's.
func (a *Auth) SignOutEverywhere(ctx context.Context, userID int64) error {
	return store.BumpSessionEpoch(ctx, a.db, userID)
}

// CSRFToken is bound to seed: the session cookie value once signed in, and the
// CSRF cookie value before that.
func (a *Auth) CSRFToken(seed string) string {
	m := hmac.New(sha256.New, a.sessionKey)
	m.Write([]byte("csrf\x00" + seed))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (a *Auth) CheckCSRF(seed, token string) bool {
	if seed == "" || token == "" {
		return false
	}
	return hmac.Equal([]byte(a.CSRFToken(seed)), []byte(token))
}

func (a *Auth) cookie(name string, value []byte, expires time.Time) *http.Cookie {
	c := &http.Cookie{
		Name:     name,
		Value:    base64.RawURLEncoding.EncodeToString(value),
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   a.cookieSecure,
		SameSite: http.SameSiteLaxMode,
	}
	if value == nil {
		c.MaxAge = -1
	} else {
		c.MaxAge = int(time.Until(expires).Seconds())
	}
	return c
}

// NewBrowserCookie is the cookie a CSRF seed is carried in for people who have
// no session yet.
func (a *Auth) NewBrowserCookie(name string) (*http.Cookie, error) {
	token, err := randomToken()
	if err != nil {
		return nil, err
	}
	return a.cookie(name, token, a.Now().Add(24*time.Hour)), nil
}

func (a *Auth) mac(b []byte) []byte {
	m := hmac.New(sha256.New, a.sessionKey)
	m.Write(b)
	return m.Sum(nil)
}

func randomToken() ([]byte, error) {
	b := make([]byte, 32)
	_, err := rand.Read(b)
	return b, err
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
