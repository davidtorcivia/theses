package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/davidtorcivia/theses/internal/store"
)

const (
	InviteValidity = 7 * 24 * time.Hour
	ResetValidity  = time.Hour
	APITokenPrefix = "thes_"
)

// API token scopes.
const (
	ScopeRead  = "read"
	ScopeWrite = "write"
	ScopeFiles = "files"
	ScopeAdmin = "admin"
)

var validScopes = map[string]bool{ScopeRead: true, ScopeWrite: true, ScopeFiles: true, ScopeAdmin: true}

// CreateInvitation stores a hashed one-time token and returns the new
// invitation's id and the clear token for the accept link. The clear token is
// never stored and cannot be recovered. The id is what the mail queued for this
// invitation is filed under, so a later resend can find it.
func (a *Auth) CreateInvitation(ctx context.Context, email, role string, invitedBy int64) (int64, string, error) {
	if !ValidRole(role) {
		return 0, "", fmt.Errorf("%q is not a role", role)
	}
	if !strings.Contains(email, "@") {
		return 0, "", fmt.Errorf("that does not look like an email address")
	}
	token, err := randomToken()
	if err != nil {
		return 0, "", err
	}
	expires := a.Now().Add(InviteValidity).Unix()
	id, err := store.CreateInvitation(ctx, a.db, email, role, invitedBy, a.mac(token), expires)
	if err != nil {
		return 0, "", fmt.Errorf("create invitation: %w", err)
	}
	return id, base64.RawURLEncoding.EncodeToString(token), nil
}

// ReissueInvitation is "resend": a new token and expiry, the old link dead.
func (a *Auth) ReissueInvitation(ctx context.Context, id int64) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	expires := a.Now().Add(InviteValidity).Unix()
	if err := store.ReissueInvitation(ctx, a.db, id, a.mac(token), expires); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(token), nil
}

// Invitation resolves an accept token. It returns ErrTokenInvalid for an unknown
// or already accepted token and ErrTokenExpired for one past its week.
func (a *Auth) Invitation(ctx context.Context, token string) (*store.Invitation, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, ErrTokenInvalid
	}
	inv, err := store.InvitationByTokenHash(ctx, a.db, a.mac(raw))
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrTokenInvalid
	}
	if err != nil {
		return nil, err
	}
	if inv.AcceptedAt.Valid {
		return nil, ErrTokenInvalid
	}
	if inv.ExpiresAt <= a.Now().Unix() {
		return nil, ErrTokenExpired
	}
	return inv, nil
}

// CreatePasswordReset stores a hashed one-time token good for an hour.
func (a *Auth) CreatePasswordReset(ctx context.Context, userID int64) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	expires := a.Now().Add(ResetValidity).Unix()
	if err := store.CreatePasswordReset(ctx, a.db, userID, a.mac(token), expires); err != nil {
		return "", fmt.Errorf("create password reset: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(token), nil
}

// UsePasswordReset spends a reset token and returns the user it belongs to.
func (a *Auth) UsePasswordReset(ctx context.Context, token string) (*store.User, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, ErrTokenInvalid
	}
	r, err := store.PasswordResetByTokenHash(ctx, a.db, a.mac(raw))
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrTokenInvalid
	}
	if err != nil {
		return nil, err
	}
	if r.UsedAt.Valid {
		return nil, ErrTokenInvalid
	}
	if r.ExpiresAt <= a.Now().Unix() {
		return nil, ErrTokenExpired
	}
	spent, err := store.UsePasswordReset(ctx, a.db, r.ID)
	if err != nil {
		return nil, err
	}
	if !spent {
		return nil, ErrTokenInvalid
	}
	return store.UserByID(ctx, a.db, r.UserID)
}

// CreateAPIToken returns the clear token once; only its HMAC is stored.
func (a *Auth) CreateAPIToken(ctx context.Context, userID int64, name string, scopes []string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("a token needs a name")
	}
	if len(scopes) == 0 {
		return "", fmt.Errorf("a token needs at least one scope")
	}
	for _, s := range scopes {
		if !validScopes[s] {
			return "", fmt.Errorf("%q is not a scope", s)
		}
	}
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	clear := APITokenPrefix + base64.RawURLEncoding.EncodeToString(token)
	if _, err := store.CreateAPIToken(ctx, a.db, userID, name, a.mac([]byte(clear)), strings.Join(scopes, " ")); err != nil {
		return "", fmt.Errorf("create api token: %w", err)
	}
	return clear, nil
}

// APIToken resolves a bearer token to its user and records the use.
func (a *Auth) APIToken(ctx context.Context, clear string) (*store.APIToken, *store.User, error) {
	if !strings.HasPrefix(clear, APITokenPrefix) {
		return nil, nil, ErrTokenInvalid
	}
	t, err := store.APITokenByHash(ctx, a.db, a.mac([]byte(clear)))
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, ErrTokenInvalid
	}
	if err != nil {
		return nil, nil, err
	}
	u, err := store.UserByID(ctx, a.db, t.UserID)
	if err != nil {
		return nil, nil, err
	}
	if err := store.TouchAPIToken(ctx, a.db, t.ID); err != nil {
		return nil, nil, err
	}
	return t, u, nil
}

// HasScope reports whether a token's scope string contains scope. admin implies
// every other scope.
func HasScope(scopes, scope string) bool {
	for _, s := range strings.Fields(scopes) {
		if s == scope || s == ScopeAdmin {
			return true
		}
	}
	return false
}
