package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"

	"github.com/davidtorcivia/theses/internal/store"
)

func TestMain(m *testing.M) {
	BcryptCost = bcrypt.MinCost // cost 12 makes the rate limit tests take minutes
	m.Run()
}

var sessionKey = []byte("a session key of at least thirty-two bytes")

type fixture struct {
	*Auth
	db  *store.DB
	now time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db := store.OpenTemp(t)
	f := &fixture{db: db, now: time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)}
	f.Auth = New(db, sessionKey, false, true)
	f.Auth.Now = func() time.Time { return f.now }
	return f
}

func (f *fixture) user(t *testing.T, handle, password, secret string) *store.User {
	t.Helper()
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.CreateUser(context.Background(), f.db, &store.User{
		Handle: handle, Email: handle + "@example.com", Name: handle, Initials: "XX",
		Colour: "#1100ff", Role: RoleOwner, PasswordHash: hash, TOTPSecret: secret,
	})
	if err != nil {
		t.Fatal(err)
	}
	u, err := store.UserByID(context.Background(), f.db, id)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func code(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	c, err := totp.GenerateCode(secret, at)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNormaliseHandle(t *testing.T) {
	ok := map[string]string{"AL": "al", " Mara ": "mara", "a-b-9": "a-b-9",
		strings.Repeat("x", 32): strings.Repeat("x", 32)}
	for in, want := range ok {
		got, err := NormaliseHandle(in)
		if err != nil || got != want {
			t.Errorf("NormaliseHandle(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "d", "-ada", "ada-", "a da", "ada!", "Ada_L", strings.Repeat("x", 33)} {
		if got, err := NormaliseHandle(in); err == nil {
			t.Errorf("NormaliseHandle(%q) accepted as %q", in, got)
		}
	}
}

func TestPasswordHashing(t *testing.T) {
	if _, err := HashPassword("short"); err == nil {
		t.Error("a password under twelve characters was accepted")
	}
	hash, err := HashPassword("a long enough password")
	if err != nil {
		t.Fatal(err)
	}
	if !CheckPassword(hash, "a long enough password") {
		t.Error("the right password did not verify")
	}
	if CheckPassword(hash, "a long enough passwore") {
		t.Error("a wrong password verified")
	}
}

func TestRoles(t *testing.T) {
	for _, c := range []struct {
		role, action string
		want         bool
	}{
		{RoleOwner, CanSettings, true},
		{RoleEditor, CanSettings, false},
		{RoleEditor, CanDelete, true},
		{RoleResearcher, CanDelete, false},
		{RoleResearcher, CanEdit, true},
		{RoleGuest, CanEdit, false},
		{RoleGuest, CanRead, true},
		{"nobody", CanRead, false},
	} {
		if got := Can(c.role, c.action); got != c.want {
			t.Errorf("Can(%q, %q) = %v", c.role, c.action, got)
		}
	}
}

func TestTOTPEnrolAndReplay(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	e, err := Enrol("ada")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(e.QR, "data:image/png;base64,") || len(e.QR) < 500 {
		t.Errorf("QR is not a png data URI: %.40q", e.QR)
	}
	if !strings.HasPrefix(e.URL, "otpauth://totp/THESES:ada") {
		t.Errorf("URL = %q", e.URL)
	}

	u := f.user(t, "ada", "a long enough password", e.Secret)
	c := code(t, e.Secret, f.now)
	if err := f.CheckTOTP(ctx, u, c); err != nil {
		t.Fatalf("valid code refused: %v", err)
	}
	if err := f.CheckTOTP(ctx, u, c); err == nil {
		t.Error("the same code was accepted twice")
	}
	if err := f.CheckTOTP(ctx, u, "000000"); err == nil {
		t.Error("a wrong code was accepted")
	}

	// One step of skew either way, but not two.
	f.now = f.now.Add(90 * time.Second)
	u, _ = store.UserByID(ctx, f.db, u.ID)
	if err := f.CheckTOTP(ctx, u, code(t, e.Secret, f.now.Add(-30*time.Second))); err != nil {
		t.Errorf("a code one step old was refused: %v", err)
	}
	f.now = f.now.Add(5 * time.Minute)
	u, _ = store.UserByID(ctx, f.db, u.ID)
	if err := f.CheckTOTP(ctx, u, code(t, e.Secret, f.now.Add(-90*time.Second))); err == nil {
		t.Error("a code three steps old was accepted")
	}
}

func TestAuthenticate(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	e, _ := Enrol("ada")
	f.user(t, "ada", "a long enough password", e.Secret)

	if _, err := f.Authenticate(ctx, "ada", "a long enough password", code(t, e.Secret, f.now)); err != nil {
		t.Fatalf("correct credentials refused: %v", err)
	}
	f.now = f.now.Add(time.Minute)
	for _, c := range []struct{ name, handle, password, code string }{
		{"wrong password", "ada", "a long enough passwore", code(t, e.Secret, f.now)},
		{"wrong code", "ada", "a long enough password", "000000"},
		{"unknown account", "nobody", "a long enough password", code(t, e.Secret, f.now)},
	} {
		if _, err := f.Authenticate(ctx, c.handle, c.password, c.code); !errors.Is(err, ErrBadCredentials) {
			t.Errorf("%s returned %v, want ErrBadCredentials", c.name, err)
		}
	}
}

func TestSessionCookieLifecycle(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	u := f.user(t, "ada", "a long enough password", "")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/login", nil)
	if err := f.StartSession(ctx, w, r, u, 30); err != nil {
		t.Fatal(err)
	}
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie was set")
	}
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie flags: HttpOnly=%v Secure=%v SameSite=%v", cookie.HttpOnly, cookie.Secure, cookie.SameSite)
	}
	if got := cookie.Expires.Sub(f.now); got < 29*24*time.Hour || got > 31*24*time.Hour {
		t.Errorf("cookie lasts %v, want about 30 days", got)
	}

	signed := httptest.NewRequest("GET", "/", nil)
	signed.AddCookie(cookie)
	got, err := f.SessionUser(ctx, signed)
	if err != nil || got.ID != u.ID {
		t.Fatalf("SessionUser = %v, %v", got, err)
	}

	// The database stores the HMAC, not the cookie value.
	var stored []byte
	if err := f.db.QueryRowContext(ctx, `SELECT hmac FROM sessions`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if string(stored) == cookie.Value {
		t.Error("the session cookie value is stored as it stands")
	}

	if err := f.SignOutEverywhere(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.SessionUser(ctx, signed); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("session survived sign out everywhere: %v", err)
	}
}

func TestEndSession(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	u := f.user(t, "ada", "a long enough password", "")

	w := httptest.NewRecorder()
	if err := f.StartSession(ctx, w, httptest.NewRequest("POST", "/login", nil), u, 30); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(w.Result().Cookies()[0])

	out := httptest.NewRecorder()
	if err := f.EndSession(ctx, out, r); err != nil {
		t.Fatal(err)
	}
	if _, err := f.SessionUser(ctx, r); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("session survived sign out: %v", err)
	}
	if out.Result().Cookies()[0].MaxAge >= 0 {
		t.Error("the session cookie was not cleared")
	}
}

func TestCSRFTokenIsBoundToSeed(t *testing.T) {
	f := newFixture(t)
	a := f.CSRFToken("seed-a")
	if !f.CheckCSRF("seed-a", a) {
		t.Error("a token did not verify against its own seed")
	}
	if f.CheckCSRF("seed-b", a) {
		t.Error("a token verified against another seed")
	}
	if f.CheckCSRF("seed-a", "") || f.CheckCSRF("", a) {
		t.Error("an empty seed or token verified")
	}
}

func TestInvitationLifecycle(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	owner := f.user(t, "ada", "a long enough password", "")

	_, token, err := f.CreateInvitation(ctx, "mara@example.com", RoleEditor, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := f.Invitation(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Email != "mara@example.com" || inv.Role != RoleEditor || inv.InviterName != "ada" {
		t.Errorf("invitation = %+v", inv)
	}
	if _, err := f.Invitation(ctx, "not-a-token"); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("a nonsense token returned %v", err)
	}

	if accepted, err := store.AcceptInvitation(ctx, f.db, inv.ID); err != nil || !accepted {
		t.Fatalf("accepting: %v %v", accepted, err)
	}
	if accepted, err := store.AcceptInvitation(ctx, f.db, inv.ID); err != nil || accepted {
		t.Fatalf("accepting twice: %v %v", accepted, err)
	}
	if _, err := f.Invitation(ctx, token); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("an accepted invitation returned %v", err)
	}

	// Resending replaces the token, and the week runs out.
	_, again, err := f.CreateInvitation(ctx, "mara@example.com", RoleEditor, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	inv2, _ := f.Invitation(ctx, again)
	fresh, err := f.ReissueInvitation(ctx, inv2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Invitation(ctx, again); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("the replaced token still worked: %v", err)
	}
	f.now = f.now.Add(InviteValidity + time.Minute)
	if _, err := f.Invitation(ctx, fresh); !errors.Is(err, ErrTokenExpired) {
		t.Errorf("an eight day old invitation returned %v", err)
	}

	if _, _, err := f.CreateInvitation(ctx, "mara@example.com", "admiral", owner.ID); err == nil {
		t.Error("an unknown role was accepted")
	}
	if _, _, err := f.CreateInvitation(ctx, "not-an-address", RoleEditor, owner.ID); err == nil {
		t.Error("an address without an @ was accepted")
	}
}

func TestPasswordResetLifecycle(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	u := f.user(t, "ada", "a long enough password", "")

	token, err := f.CreatePasswordReset(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.PasswordResetUser(ctx, token)
	if err != nil || got.ID != u.ID {
		t.Fatalf("PasswordResetUser = %v, %v", got, err)
	}
	hash, err := HashPassword("a different long password")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.ResetPassword(ctx, token, hash); err != nil {
		t.Fatal(err)
	}
	after, err := store.UserByID(ctx, f.db, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !CheckPassword(after.PasswordHash, "a different long password") {
		t.Error("the reset did not change the password")
	}
	if after.SessionEpoch != u.SessionEpoch+1 {
		t.Errorf("session epoch = %d, want %d", after.SessionEpoch, u.SessionEpoch+1)
	}
	var actions int
	if err := f.db.QueryRowContext(ctx,
		`SELECT count(*) FROM activity WHERE entity = 'user' AND entity_id = ? AND action = 'password-reset'`, u.ID).
		Scan(&actions); err != nil {
		t.Fatal(err)
	}
	if actions != 1 {
		t.Errorf("password reset activity rows = %d, want 1", actions)
	}
	if _, err := f.PasswordResetUser(ctx, token); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("a spent token returned %v", err)
	}

	token, _ = f.CreatePasswordReset(ctx, u.ID)
	f.now = f.now.Add(ResetValidity + time.Minute)
	if _, err := f.PasswordResetUser(ctx, token); !errors.Is(err, ErrTokenExpired) {
		t.Errorf("an hour old token returned %v", err)
	}
}

func TestAPITokens(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	u := f.user(t, "ada", "a long enough password", "")

	clear, err := f.CreateAPIToken(ctx, u.ID, "research agent", []string{ScopeRead, ScopeWrite})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(clear, APITokenPrefix) {
		t.Errorf("token = %q", clear)
	}
	var stored []byte
	if err := f.db.QueryRowContext(ctx, `SELECT hash FROM api_tokens`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), clear) {
		t.Error("the token is stored in the clear")
	}

	tok, got, err := f.APIToken(ctx, clear)
	if err != nil || got.ID != u.ID {
		t.Fatalf("APIToken = %v, %v, %v", tok, got, err)
	}
	if !HasScope(tok.Scopes, ScopeWrite) || HasScope(tok.Scopes, ScopeAdmin) {
		t.Errorf("scopes = %q", tok.Scopes)
	}
	if !HasScope("admin", ScopeFiles) {
		t.Error("admin should imply every scope")
	}
	after, err := store.APITokenByHash(ctx, f.db, f.mac([]byte(clear)))
	if err != nil || !after.LastUsedAt.Valid {
		t.Error("last_used_at was not recorded")
	}

	if err := store.RevokeAPIToken(ctx, f.db, tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.APIToken(ctx, clear); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("a revoked token returned %v", err)
	}
	if _, err := f.CreateAPIToken(ctx, u.ID, "bad", []string{"root"}); err == nil {
		t.Error("an unknown scope was accepted")
	}
	if _, err := f.CreateAPIToken(ctx, u.ID, "  ", []string{ScopeRead}); err == nil {
		t.Error("a nameless token was accepted")
	}
}

func TestRateLimits(t *testing.T) {
	f := newFixture(t)
	n := limitsByBucket[BucketLogin].n
	for i := 0; i < n; i++ {
		if !f.Allow(BucketLogin, "10.0.0.1", "ada") {
			t.Fatalf("attempt %d was refused before the limit", i+1)
		}
	}
	if f.Allow(BucketLogin, "10.0.0.1", "ada") {
		t.Error("the limit did not hold")
	}
	// Another address for the same account is still held back by the handle key.
	if f.Allow(BucketLogin, "10.0.0.2", "ada") {
		t.Error("the handle key did not hold across addresses")
	}
	// A different account from a fresh address is unaffected.
	if !f.Allow(BucketLogin, "10.0.0.3", "mara") {
		t.Error("an unrelated account was held back")
	}
	// The window passes.
	f.now = f.now.Add(limitsByBucket[BucketLogin].window + time.Minute)
	if !f.Allow(BucketLogin, "10.0.0.1", "ada") {
		t.Error("the window did not expire")
	}

	f.ResetLimits(BucketReset, "ada")
	for i := 0; i < limitsByBucket[BucketReset].n; i++ {
		f.Allow(BucketReset, "ada")
	}
	if f.Allow(BucketReset, "ada") {
		t.Error("the reset bucket did not hold")
	}
	f.ResetLimits(BucketReset, "ada")
	if !f.Allow(BucketReset, "ada") {
		t.Error("ResetLimits did not clear the counter")
	}
}

func TestClientIP(t *testing.T) {
	f := newFixture(t)
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "192.0.2.9:4444"
	// The client sent the first entry itself; the proxy appended the last.
	r.Header.Set("X-Forwarded-For", "203.0.113.7, 198.51.100.1")
	r.Header.Set("CF-Connecting-IP", "203.0.113.8")

	if got := f.ClientIP(r); got != "192.0.2.9" {
		t.Errorf("without THESES_TRUST_PROXY, ClientIP = %q", got)
	}
	trusting := New(f.db, sessionKey, true, true)
	if got := trusting.ClientIP(r); got != "198.51.100.1" {
		t.Errorf("the rightmost X-Forwarded-For entry expected: %q", got)
	}
	r.Header.Del("X-Forwarded-For")
	if got := trusting.ClientIP(r); got != "192.0.2.9" {
		t.Errorf("CF-Connecting-IP must not be believed: %q", got)
	}
	// Two header lines are one list, so the last entry of the last line wins.
	r.Header.Add("X-Forwarded-For", "203.0.113.7")
	r.Header.Add("X-Forwarded-For", "198.51.100.2")
	if got := trusting.ClientIP(r); got != "198.51.100.2" {
		t.Errorf("with two header lines, ClientIP = %q", got)
	}
}

func TestUnknownHandleTakesAsLongAsAWrongPassword(t *testing.T) {
	// bcrypt refuses a password over 72 bytes outright but still hashes one when
	// comparing, so without a cap the two branches were 0ms and 180ms apart.
	ctx := context.Background()
	was := BcryptCost
	BcryptCost = 10
	defer func() { BcryptCost = was }()

	f := newFixture(t)
	f.user(t, "ada", "a long enough password", "")
	long := strings.Repeat("x", 100)

	median := func(handle string) time.Duration {
		var runs []time.Duration
		for i := 0; i < 3; i++ {
			start := time.Now()
			if _, err := f.Authenticate(ctx, handle, long, ""); err == nil {
				t.Fatalf("%s with a wrong password was accepted", handle)
			}
			runs = append(runs, time.Since(start))
		}
		sort.Slice(runs, func(i, j int) bool { return runs[i] < runs[j] })
		return runs[1]
	}
	known := median("ada")
	unknown := median("nobody")
	if unknown*2 < known {
		t.Errorf("an unknown account answered in %v against %v for a known one, which says which is which", unknown, known)
	}
}

func TestRateLimiterRefusalRecordsNothing(t *testing.T) {
	f := newFixture(t)
	key := BucketLogin + "\x00" + "10.0.0.1"
	for i := 0; i < limitsByBucket[BucketLogin].n; i++ {
		f.Allow(BucketLogin, "10.0.0.1")
	}
	before := len(f.limits.hits[key])
	if f.Allow(BucketLogin, "10.0.0.1") {
		t.Fatal("the limit did not hold")
	}
	if got := len(f.limits.hits[key]); got != before {
		t.Errorf("a refused attempt was still counted: %d entries, was %d", got, before)
	}
}

func TestRateLimiterForgetsStaleKeys(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 300; i++ {
		f.Allow(BucketLogin, fmt.Sprintf("10.0.1.%d", i))
	}
	if len(f.limits.hits) < 300 {
		t.Fatalf("only %d keys were recorded", len(f.limits.hits))
	}

	f.now = f.now.Add(longestWindow + time.Minute)
	for i := 0; i < sweepEvery; i++ {
		f.Allow(BucketLogin, "10.0.0.1")
	}
	if got := len(f.limits.hits); got > 1 {
		t.Errorf("%d keys left after the window passed, want only the live one", got)
	}
}
