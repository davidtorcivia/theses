package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/davidtorcivia/theses/internal/store"
)

const (
	InviteValidity = 7 * 24 * time.Hour
	ResetValidity  = time.Hour
	APITokenPrefix = "thes_"
)

var ErrAPITokenInput = errors.New("invalid API token")

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

// PasswordResetUser validates a reset token without spending it. The reset
// form calls this before bcrypt so a made-up token cannot buy expensive work.
func (a *Auth) PasswordResetUser(ctx context.Context, token string) (*store.User, error) {
	r, err := a.passwordReset(ctx, a.db, token)
	if err != nil {
		return nil, err
	}
	return store.UserByID(ctx, a.db, r.UserID)
}

func (a *Auth) passwordReset(ctx context.Context, q store.Querier, token string) (*store.PasswordReset, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, ErrTokenInvalid
	}
	r, err := store.PasswordResetByTokenHash(ctx, q, a.mac(raw))
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
	return r, nil
}

// ResetPassword atomically spends the token, changes the password, records the
// event and invalidates every existing browser session.
func (a *Auth) ResetPassword(ctx context.Context, token, hash string) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := a.passwordReset(ctx, tx, token)
	if err != nil {
		return err
	}
	spent, err := store.UsePasswordReset(ctx, tx, r.ID)
	if err != nil {
		return err
	}
	if !spent {
		return ErrTokenInvalid
	}
	if err := store.SetPasswordHash(ctx, tx, r.UserID, hash); err != nil {
		return err
	}
	id := strconv.FormatInt(r.UserID, 10)
	if err := store.InsertActivity(ctx, tx, "user", id, "", "user", id, "password-reset", "", ""); err != nil {
		return err
	}
	if err := store.BumpSessionEpoch(ctx, tx, r.UserID); err != nil {
		return err
	}
	return tx.Commit()
}

// CreateAPIToken returns the clear token once; only its HMAC is stored.
func (a *Auth) CreateAPIToken(ctx context.Context, userID int64, name string, scopes []string) (string, error) {
	return a.CreateAPITokenWith(ctx, a.db, userID, name, scopes)
}

// CreateAPITokenWith is CreateAPIToken on a caller's transaction, so the
// one-time credential and its activity row can commit together.
func (a *Auth) CreateAPITokenWith(ctx context.Context, q store.Querier, userID int64, name string, scopes []string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("%w: a token needs a name", ErrAPITokenInput)
	}
	if len(scopes) == 0 {
		return "", fmt.Errorf("%w: a token needs at least one scope", ErrAPITokenInput)
	}
	for _, s := range scopes {
		if !validScopes[s] {
			return "", fmt.Errorf("%w: %q is not a scope", ErrAPITokenInput, s)
		}
	}
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	clear := APITokenPrefix + base64.RawURLEncoding.EncodeToString(token)
	if _, err := store.CreateAPIToken(ctx, q, userID, name, a.mac([]byte(clear)), strings.Join(scopes, " ")); err != nil {
		return "", fmt.Errorf("create api token: %w", err)
	}
	return clear, nil
}

// APIToken resolves a bearer token to its user and records the use.
func (a *Auth) APIToken(ctx context.Context, clear string) (*store.APIToken, *store.User, error) {
	t, u, err := a.LookupAPIToken(ctx, clear)
	if err != nil {
		return nil, nil, err
	}
	if err := a.TouchAPIToken(ctx, t.ID); err != nil {
		return nil, nil, err
	}
	return t, u, nil
}

// TouchAPIToken records the use, which TouchAPIToken in store does at most once
// a minute.
func (a *Auth) TouchAPIToken(ctx context.Context, id int64) error {
	return store.TouchAPIToken(ctx, a.db, id)
}

// LookupAPIToken resolves a bearer token without recording the use, so a caller
// that rate limits can refuse a request before it writes anything.
func (a *Auth) LookupAPIToken(ctx context.Context, clear string) (*store.APIToken, *store.User, error) {
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
