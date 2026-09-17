// Package auth owns account names, passwords, TOTP, sessions, roles, the
// one-time tokens for invitations, password resets and the API, and the rate
// limits in front of all of them.
package auth

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/davidtorcivia/theses/internal/store"
)

// BcryptCost is a variable so tests can drop it; production never changes it.
var BcryptCost = 12

// MinPasswordLen matches what the invite page asks for.
const MinPasswordLen = 12

var (
	ErrBadCredentials = errors.New("that account name, password or code is wrong")
	ErrRateLimited    = errors.New("too many attempts; wait a minute and try again")
	ErrTokenInvalid   = errors.New("that link is not valid or has already been used")
	ErrTokenExpired   = errors.New("that link has expired")
)

type Auth struct {
	db           *store.DB
	sessionKey   []byte
	trustProxy   bool
	cookieSecure bool

	// Now is the clock, replaced in tests.
	Now func() time.Time

	limits *limiters
}

func New(db *store.DB, sessionKey []byte, trustProxy, cookieSecure bool) *Auth {
	return &Auth{
		db:           db,
		sessionKey:   sessionKey,
		trustProxy:   trustProxy,
		cookieSecure: cookieSecure,
		Now:          time.Now,
		limits:       newLimiters(),
	}
}

var handlePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$`)

// NormaliseHandle lowercases and checks an account name. Account names are
// lowercase letters, digits and hyphens, 2 to 32 characters, and unique.
func NormaliseHandle(h string) (string, error) {
	h = strings.ToLower(strings.TrimSpace(h))
	if len(h) < 2 || len(h) > 32 {
		return "", fmt.Errorf("an account name is between 2 and 32 characters")
	}
	if !handlePattern.MatchString(h) {
		return "", fmt.Errorf("an account name holds only lowercase letters, digits and hyphens, and does not start or end with a hyphen")
	}
	return h, nil
}

func HashPassword(password string) (string, error) {
	if len([]rune(password)) < MinPasswordLen {
		return "", fmt.Errorf("a password is at least %d characters", MinPasswordLen)
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), BcryptCost)
	return string(h), err
}

func CheckPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// burnPasswordTime does bcrypt's work and throws it away, so that an unknown
// account name takes as long to refuse as a wrong password.
func burnPasswordTime(password string) {
	bcrypt.GenerateFromPassword([]byte(password), BcryptCost) //nolint:errcheck // discarded on purpose
}

// Roles and what they may do. Per-proposition membership sits on top of this.
const (
	RoleOwner      = "owner"
	RoleEditor     = "editor"
	RoleResearcher = "researcher"
	RoleGuest      = "guest"
)

// Actions a role may or may not take.
const (
	CanRead     = "read"
	CanEdit     = "edit"
	CanDelete   = "delete"
	CanSettings = "settings"
)

var permissions = map[string]map[string]bool{
	RoleOwner:      {CanRead: true, CanEdit: true, CanDelete: true, CanSettings: true},
	RoleEditor:     {CanRead: true, CanEdit: true, CanDelete: true},
	RoleResearcher: {CanRead: true, CanEdit: true},
	RoleGuest:      {CanRead: true},
}

// Can reports whether a role may take an action.
func Can(role, action string) bool { return permissions[role][action] }

func ValidRole(role string) bool { _, ok := permissions[role]; return ok }

// ClientIP is the address rate limits and the activity log use.
// X-Forwarded-For and CF-Connecting-IP are believed only with THESES_TRUST_PROXY.
func (a *Auth) ClientIP(r *http.Request) string {
	if a.trustProxy {
		if v := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); v != "" {
			return v
		}
		if v := r.Header.Get("X-Forwarded-For"); v != "" {
			if first, _, _ := strings.Cut(v, ","); strings.TrimSpace(first) != "" {
				return strings.TrimSpace(first)
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
