package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/config"
	"github.com/davidtorcivia/theses/internal/settings"
	"github.com/davidtorcivia/theses/internal/store"
)

func TestMain(m *testing.M) {
	auth.BcryptCost = bcrypt.MinCost
	m.Run()
}

type harness struct {
	*testing.T
	srv    *Server
	db     *store.DB
	http   *httptest.Server
	client *http.Client
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db := store.OpenTemp(t)
	set, err := settings.Open(context.Background(), db, []byte("a settings key of at least thirty-two bytes"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Bind: ":0", DataDir: t.TempDir(), BaseURL: "http://localhost:8080",
		SecretKey:  []byte("a secret key of at least thirty-two bytes!"),
		SessionKey: []byte("a session key of at least thirty-two bytes"),
		LogLevel:   slog.LevelError,
	}
	srv, err := New(cfg, db, set, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{
		T: t, srv: srv, db: db, http: ts,
		client: &http.Client{
			Jar:           jar,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (h *harness) get(path string) (*http.Response, string) {
	h.Helper()
	res, err := h.client.Get(h.http.URL + path)
	if err != nil {
		h.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	return res, string(body)
}

func (h *harness) post(path string, form url.Values) (*http.Response, string) {
	h.Helper()
	res, err := h.client.PostForm(h.http.URL+path, form)
	if err != nil {
		h.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	return res, string(body)
}

var (
	csrfRe   = regexp.MustCompile(`name="csrf" value="([^"]+)"`)
	secretRe = regexp.MustCompile(`type the key: ([A-Z2-7]+)`)
	hrefRe   = regexp.MustCompile(`<a [^>]*href="([^"]*)"`)
)

func (h *harness) csrf(path string) string {
	h.Helper()
	_, body := h.get(path)
	m := csrfRe.FindStringSubmatch(body)
	if m == nil {
		h.Fatalf("no csrf token on %s", path)
	}
	return m[1]
}

// setupOwner runs the whole first-run flow and returns the owner's password and
// authenticator secret.
func (h *harness) setupOwner() (password, secret string) {
	h.Helper()
	password = "a long enough password"

	res, _ := h.post("/setup", url.Values{
		"csrf": {h.csrf("/setup")}, "handle": {"dt"}, "name": {"David Torcivia"},
		"initials": {"DT"}, "colour": {Palette[1]}, "email": {"dt@example.fm"},
		"password": {password},
	})
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/setup/authenticator" {
		h.Fatalf("setup details: %d %s", res.StatusCode, res.Header.Get("Location"))
	}

	_, body := h.get("/setup/authenticator")
	m := secretRe.FindStringSubmatch(body)
	if m == nil {
		h.Fatal("the enrolment page did not print the key")
	}
	secret = m[1]
	if !strings.Contains(body, `<img class="qr" src="data:image/png;base64,`) {
		h.Fatal("the enrolment page did not render the QR code as a data URI")
	}

	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		h.Fatal(err)
	}
	res, _ = h.post("/setup/authenticator", url.Values{
		"csrf": {csrfRe.FindStringSubmatch(body)[1]}, "code": {code},
	})
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/" {
		h.Fatalf("setup confirm: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	return password, secret
}

func (h *harness) signOut() {
	h.Helper()
	h.post("/logout", url.Values{"csrf": {h.csrf("/profile")}})
}

func TestSetupGateRedirectsUntilAnOwnerExists(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"/", "/login", "/settings", "/anything"} {
		res, _ := h.get(path)
		if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/setup" {
			t.Errorf("%s gave %d %s, want a redirect to /setup", path, res.StatusCode, res.Header.Get("Location"))
		}
	}
	if res, _ := h.get("/healthz"); res.StatusCode != http.StatusOK {
		t.Errorf("/healthz is behind the setup gate: %d", res.StatusCode)
	}
}

func TestSetupCreatesTheOwnerAndEnrolsTOTP(t *testing.T) {
	h := newHarness(t)
	_, secret := h.setupOwner()

	u, err := store.UserByHandle(context.Background(), h.db, "dt")
	if err != nil {
		t.Fatal(err)
	}
	if u.Role != auth.RoleOwner {
		t.Errorf("role = %q", u.Role)
	}
	if u.Name != "David Torcivia" || u.Initials != "DT" || u.Colour != Palette[1] || u.Email != "dt@example.fm" {
		t.Errorf("owner = %+v", u)
	}
	if u.TOTPSecret != secret {
		t.Error("the confirmed secret was not stored")
	}
	if u.PasswordHash == "" || strings.Contains(u.PasswordHash, "password") {
		t.Error("the password was not hashed")
	}

	// The session from setup works, and setup is closed.
	if res, _ := h.get("/"); res.StatusCode != http.StatusOK {
		t.Errorf("the shell after setup gave %d", res.StatusCode)
	}
	res, _ := h.get("/setup")
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/login" {
		t.Errorf("/setup after setup gave %d %s", res.StatusCode, res.Header.Get("Location"))
	}
}

func TestLoginSucceedsAndSetsTheCookie(t *testing.T) {
	h := newHarness(t)
	password, secret := h.setupOwner()
	h.signOut()

	if res, _ := h.get("/"); res.Header.Get("Location") != "/login" {
		t.Fatalf("still signed in after sign out: %s", res.Header.Get("Location"))
	}

	code, _ := totp.GenerateCode(secret, time.Now())
	res, _ := h.post("/login", url.Values{
		"csrf": {h.csrf("/login")}, "handle": {"dt"}, "password": {password}, "code": {code},
	})
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/" {
		t.Fatalf("login gave %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	var session *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == auth.SessionCookie {
			session = c
		}
	}
	if session == nil {
		t.Fatal("login set no session cookie")
	}
	if !session.HttpOnly || session.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie flags: HttpOnly=%v SameSite=%v", session.HttpOnly, session.SameSite)
	}
	if session.Secure {
		t.Error("an http base URL should turn Secure off")
	}
	if res, _ := h.get("/"); res.StatusCode != http.StatusOK {
		t.Errorf("the shell gave %d after signing in", res.StatusCode)
	}
}

func TestLoginWithAWrongCodeFailsThenIsRateLimited(t *testing.T) {
	h := newHarness(t)
	password, _ := h.setupOwner()
	h.signOut()

	token := h.csrf("/login")
	attempt := func() int {
		res, _ := h.post("/login", url.Values{
			"csrf": {token}, "handle": {"dt"}, "password": {password}, "code": {"000000"},
		})
		return res.StatusCode
	}
	for i := 0; i < 10; i++ {
		if got := attempt(); got != http.StatusUnauthorized {
			t.Fatalf("attempt %d gave %d, want 401", i+1, got)
		}
	}
	if got := attempt(); got != http.StatusTooManyRequests {
		t.Errorf("the eleventh attempt gave %d, want 429", got)
	}
	if _, err := h.srv.auth.SessionUser(context.Background(),
		httptest.NewRequest("GET", "/", nil)); err == nil {
		t.Error("a failed sign-in left a session")
	}
}

func TestCSPOnEveryResponse(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()

	paths := []string{"/", "/login", "/profile", "/settings", "/offline", "/no-such-page",
		h.srv.assets.URL("app.css"), h.srv.assets.URL("art/22.jpg")}
	for _, p := range paths {
		res, _ := h.get(p)
		if got := res.Header.Get("Content-Security-Policy"); got != contentSecurityPolicy {
			t.Errorf("%s: CSP = %q", p, got)
		}
		if got := res.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q", p, got)
		}
	}
}

func TestNoInlineScriptsOrStyles(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()

	for _, p := range []string{"/", "/login", "/profile", "/settings", "/no-such-page"} {
		_, body := h.get(p)
		if strings.Contains(body, " style=") {
			t.Errorf("%s carries a style attribute, which the CSP refuses", p)
		}
		if strings.Contains(body, " on") && regexp.MustCompile(`\son(click|submit|load|change)=`).MatchString(body) {
			t.Errorf("%s carries an inline event handler", p)
		}
		for _, tag := range regexp.MustCompile(`(?s)<script[^>]*>(.*?)</script>`).FindAllStringSubmatch(body, -1) {
			if strings.TrimSpace(tag[1]) != "" {
				t.Errorf("%s has a script with a body", p)
			}
		}
	}
}

func TestNotFoundPageLinksOnlyToLogin(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()

	res, body := h.get("/no-such-page")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if !strings.Contains(body, "404") || !strings.Contains(body, "Not found.") {
		t.Error("the page does not say 404")
	}
	var hrefs []string
	for _, m := range hrefRe.FindAllStringSubmatch(body, -1) {
		hrefs = append(hrefs, m[1])
	}
	if len(hrefs) != 1 || hrefs[0] != "/login" {
		t.Errorf("links = %v, want only /login", hrefs)
	}
}

func TestInvitationAcceptCreatesAUser(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.setupOwner()

	owner, err := store.UserByHandle(ctx, h.db, "dt")
	if err != nil {
		t.Fatal(err)
	}
	token, err := h.srv.auth.CreateInvitation(ctx, "mara@example.fm", auth.RoleEditor, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	h.signOut()

	_, body := h.get("/invite/" + token)
	if !strings.Contains(body, "David Torcivia invited you as editor") {
		t.Errorf("the accept page does not name the inviter and role:\n%s", body)
	}
	res, _ := h.post("/invite/"+token, url.Values{
		"csrf": {csrfRe.FindStringSubmatch(body)[1]}, "handle": {"mara"}, "name": {"Mara Okafor"},
		"initials": {"MO"}, "colour": {Palette[2]}, "password": {"another long password"},
	})
	if res.Header.Get("Location") != "/invite/"+token+"/authenticator" {
		t.Fatalf("accept gave %d %s", res.StatusCode, res.Header.Get("Location"))
	}

	_, body = h.get("/invite/" + token + "/authenticator")
	secret := secretRe.FindStringSubmatch(body)[1]
	code, _ := totp.GenerateCode(secret, time.Now())
	res, _ = h.post("/invite/"+token+"/authenticator", url.Values{
		"csrf": {csrfRe.FindStringSubmatch(body)[1]}, "code": {code},
	})
	if res.Header.Get("Location") != "/" {
		t.Fatalf("enrolment gave %d %s", res.StatusCode, res.Header.Get("Location"))
	}

	u, err := store.UserByHandle(ctx, h.db, "mara")
	if err != nil {
		t.Fatal(err)
	}
	if u.Role != auth.RoleEditor || u.Email != "mara@example.fm" || u.Initials != "MO" {
		t.Errorf("invited user = %+v", u)
	}
	// The invitation is spent.
	if _, err := h.srv.auth.Invitation(ctx, token); err == nil {
		t.Error("the invitation still resolves after being accepted")
	}
	if res, _ := h.get("/invite/" + token); res.StatusCode != http.StatusNotFound {
		t.Error("a spent invitation should show the 404 page")
	}
}

func TestSettingsSaveRoundTrips(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()

	res, _ := h.post("/settings", url.Values{
		"csrf":                    {h.csrf("/settings")},
		"workspace.name":          {"Debt Machine"},
		"workspace.episode_start": {"7"},
		"workspace.release_day":   {"Thursday"},
		"workspace.release_time":  {"06:00"},
		"workspace.timezone":      {"America/New_York"},
	})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("save gave %d", res.StatusCode)
	}
	if got := settings.Get[string](h.srv.settings, "workspace.name"); got != "Debt Machine" {
		t.Errorf("workspace.name = %q", got)
	}
	if got := settings.Get[int](h.srv.settings, "workspace.episode_start"); got != 7 {
		t.Errorf("workspace.episode_start = %d", got)
	}
	_, body := h.get("/settings")
	if !strings.Contains(body, `value="Debt Machine"`) {
		t.Error("the saved name is not on the page")
	}

	// A secret is stored, shown as set, and never printed back.
	res, _ = h.post("/settings", url.Values{
		"csrf": {h.csrf("/settings")}, "mail.host": {"smtp.example.com"},
		"mail.port": {"587"}, "mail.tls": {"starttls"}, "mail.password": {"a mail password"},
	})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("mail save gave %d", res.StatusCode)
	}
	_, body = h.get("/settings")
	if strings.Contains(body, "a mail password") {
		t.Error("the stored mail password was sent back to the browser")
	}
	if !strings.Contains(body, "set, leave empty to keep") {
		t.Error("the page does not show the password as set")
	}
	got, err := h.srv.settings.Secret(context.Background(), "mail.password")
	if err != nil || got != "a mail password" {
		t.Errorf("secret round trip gave %q, %v", got, err)
	}

	// A value the registry refuses does not save and says why.
	res, body = h.post("/settings", url.Values{
		"csrf": {h.csrf("/settings")}, "workspace.episode_start": {"soon"},
	})
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "not a number") {
		t.Errorf("a bad value gave %d", res.StatusCode)
	}
}

func TestSettingsAreOwnerOnly(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.setupOwner()
	if err := store.SetUserRole(ctx, h.db, 1, auth.RoleEditor); err != nil {
		t.Fatal(err)
	}
	if res, _ := h.get("/settings"); res.StatusCode != http.StatusForbidden {
		t.Errorf("an editor got %d from /settings", res.StatusCode)
	}
}

func TestPostWithoutACSRFTokenIsRefused(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	res, _ := h.post("/settings", url.Values{"workspace.name": {"Nope"}})
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("a form with no token gave %d", res.StatusCode)
	}
	res, _ = h.post("/settings", url.Values{"csrf": {"not-a-token"}, "workspace.name": {"Nope"}})
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("a form with a wrong token gave %d", res.StatusCode)
	}
	if got := settings.Get[string](h.srv.settings, "workspace.name"); got == "Nope" {
		t.Error("the refused form saved anyway")
	}
}

func TestReadyzReportsTheDatabase(t *testing.T) {
	h := newHarness(t)
	res, body := h.get("/readyz")
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "database: ok") {
		t.Fatalf("/readyz gave %d %q", res.StatusCode, body)
	}

	h.srv.AddCheck(Check{Name: "object store", Run: func(context.Context) error {
		return context.DeadlineExceeded
	}})
	res, body = h.get("/readyz")
	if res.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "object store:") {
		t.Errorf("a failing check gave %d %q", res.StatusCode, body)
	}
}

func TestStaticAssetsAreHashedAndCached(t *testing.T) {
	h := newHarness(t)
	res, body := h.get(h.srv.assets.URL("app.css"))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("app.css gave %d", res.StatusCode)
	}
	if !strings.Contains(body, "--railw") {
		t.Error("app.css is not the mockup stylesheet")
	}
	if got := res.Header.Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("Cache-Control = %q", got)
	}
	if res, _ := h.get("/static/0000deadbeef/app.css"); res.StatusCode != http.StatusNotFound {
		t.Errorf("a stale hash gave %d", res.StatusCode)
	}
	if res, _ := h.get(h.srv.assets.URL("fonts/archivo-latin.woff2")); res.Header.Get("Content-Type") != "font/woff2" {
		t.Errorf("woff2 content type = %q", res.Header.Get("Content-Type"))
	}
}

func TestProfileChangesAndSignOutEverywhere(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.setupOwner()

	res, _ := h.post("/profile", url.Values{
		"csrf": {h.csrf("/profile")}, "handle": {"dtorcivia"}, "name": {"David Torcivia"},
		"initials": {"DT"}, "colour": {Palette[4]}, "email": {"dt@example.fm"},
	})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("profile save gave %d", res.StatusCode)
	}
	u, err := store.UserByHandle(ctx, h.db, "dtorcivia")
	if err != nil || u.Colour != Palette[4] {
		t.Fatalf("profile did not save: %v %+v", err, u)
	}

	res, _ = h.post("/profile/signout-everywhere", url.Values{"csrf": {h.csrf("/profile")}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign out everywhere gave %d", res.StatusCode)
	}
	if res, _ := h.get("/"); res.Header.Get("Location") != "/login" {
		t.Error("the session survived sign out everywhere")
	}
}

func TestPasswordResetWritesATokenAndSaysNothing(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.setupOwner()
	h.signOut()

	const said = "If that account exists, mail is on its way."
	for _, who := range []string{"dt", "nobody"} {
		res, body := h.post("/reset", url.Values{"csrf": {h.csrf("/reset")}, "who": {who}})
		if res.StatusCode != http.StatusOK || !strings.Contains(body, said) {
			t.Errorf("reset for %q gave %d without the standard line", who, res.StatusCode)
		}
	}
	var n int
	if err := h.db.QueryRowContext(ctx, `SELECT count(*) FROM password_resets`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d reset rows, want one for the account that exists", n)
	}
}
