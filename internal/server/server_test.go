package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
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
	log    *logBuffer
}

// logBuffer collects what the server logged. The handler writes from whichever
// goroutine is serving, so it is locked.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
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
	logs := &logBuffer{}
	srv, err := New(cfg, db, set, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})), "test")
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
		T: t, srv: srv, db: db, http: ts, log: logs,
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
	csrfRe       = regexp.MustCompile(`name="csrf" value="([^"]+)"`)
	secretRe     = regexp.MustCompile(`type the key: ([A-Z2-7]+)`)
	hrefRe       = regexp.MustCompile(`<a [^>]*href="([^"]*)"`)
	tokenRe      = regexp.MustCompile(`(thes_[A-Za-z0-9_-]+)`)
	inviteLinkRe = regexp.MustCompile(`(http[^"<\s]*/invite/[A-Za-z0-9_-]+)`)
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

// setRole is test setup, not something the app does: the handlers change a role
// only through the guarded statement.
func (h *harness) setRole(t *testing.T, id int64, role string) {
	t.Helper()
	if _, err := h.db.ExecContext(context.Background(),
		`UPDATE users SET role = ? WHERE id = ?`, role, id); err != nil {
		t.Fatal(err)
	}
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
	h := newHarness(t)
	h.setupOwner()
	h.setRole(t, 1, auth.RoleEditor)
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

func TestServerErrorRendersTheErrorPage(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()

	// A handler whose query cannot run is the ordinary way into fail().
	h.db.Close()
	res, body := h.get("/settings")
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if !strings.Contains(body, "500") || !strings.Contains(body, "Something broke.") {
		t.Errorf("the 500 page did not render from the template:\n%s", body)
	}
	var hrefs []string
	for _, m := range hrefRe.FindAllStringSubmatch(body, -1) {
		hrefs = append(hrefs, m[1])
	}
	if len(hrefs) != 1 || hrefs[0] != "/settings" {
		t.Errorf("links = %v, want only the page itself", hrefs)
	}
}

func TestServerErrorFallsBackWhenTemplatesAreBroken(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()

	// Templates from an empty tree: nothing, including the error page, renders.
	h.srv.dev = true
	h.srv.templateFS = fstest.MapFS{}

	res, body := h.get("/")
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if body != brokenPage {
		t.Errorf("fallback body = %q", body)
	}
}

func TestReenrolmentBelongsToTheSignedInPerson(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()

	res, _ := h.post("/profile/totp", url.Values{"csrf": {h.csrf("/profile")}})
	if res.Header.Get("Location") != "/profile/authenticator" {
		t.Fatalf("re-enrol gave %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	_, body := h.get("/profile/authenticator")
	secret := secretRe.FindStringSubmatch(body)[1]
	token := csrfRe.FindStringSubmatch(body)[1]

	// Someone else is now signed in on this browser, with the first person's
	// enrolment cookie still there.
	other := &store.User{Handle: "mara", Email: "mara@example.fm", Name: "Mara Okafor",
		Initials: "MO", Colour: Palette[2], Role: auth.RoleEditor, PasswordHash: "x"}
	id, err := store.CreateUser(context.Background(), h.db, other)
	if err != nil {
		t.Fatal(err)
	}
	other.ID, other.SessionEpoch = id, 1
	w := httptest.NewRecorder()
	if err := h.srv.auth.StartSession(context.Background(), w, httptest.NewRequest("POST", "/login", nil), other, 30); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(h.http.URL)
	h.client.Jar.SetCookies(u, w.Result().Cookies())

	code, _ := totp.GenerateCode(secret, time.Now())
	res, _ = h.post("/profile/authenticator", url.Values{"csrf": {token}, "code": {code}})
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("a stale enrolment cookie gave %d, want 403", res.StatusCode)
	}
	owner, err := store.UserByHandle(context.Background(), h.db, "dt")
	if err != nil {
		t.Fatal(err)
	}
	if owner.TOTPSecret == secret {
		t.Error("the owner's authenticator was replaced from another person's session")
	}
}

func TestTeamInvitationsAndTokens(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.setupOwner()

	res, _ := h.post("/settings/team/invite", url.Values{
		"csrf": {h.csrf("/settings")}, "email": {"mara@example.fm"}, "role": {auth.RoleEditor},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("invite gave %d", res.StatusCode)
	}
	pending, err := store.ListPendingInvitations(ctx, h.db, time.Now().Unix())
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending invitations = %v, %v", pending, err)
	}
	_, body := h.get("/settings")
	if !strings.Contains(body, "mara@example.fm") {
		t.Error("the invitation is not listed on the page")
	}

	var firstHash []byte
	if err := h.db.QueryRowContext(ctx, `SELECT token_hash FROM invitations`).Scan(&firstHash); err != nil {
		t.Fatal(err)
	}
	id := pending[0].ID
	if res, _ := h.post("/settings/team/invite/"+itoa(id)+"/resend",
		url.Values{"csrf": {h.csrf("/settings")}}); res.StatusCode != http.StatusOK {
		t.Fatalf("resend gave %d", res.StatusCode)
	}
	var secondHash []byte
	if err := h.db.QueryRowContext(ctx, `SELECT token_hash FROM invitations`).Scan(&secondHash); err != nil {
		t.Fatal(err)
	}
	if string(firstHash) == string(secondHash) {
		t.Error("resend did not replace the token")
	}

	if res, _ := h.post("/settings/team/invite/"+itoa(id)+"/revoke",
		url.Values{"csrf": {h.csrf("/settings")}}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("revoke gave %d", res.StatusCode)
	}
	if left, _ := store.ListPendingInvitations(ctx, h.db, time.Now().Unix()); len(left) != 0 {
		t.Error("the invitation survived revoking")
	}

	res, body = h.post("/settings/tokens", url.Values{
		"csrf": {h.csrf("/settings")}, "name": {"research agent"}, "scopes": {"read write"},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("token create gave %d", res.StatusCode)
	}
	m := tokenRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("the new token was not shown")
	}
	if _, _, err := h.srv.auth.APIToken(ctx, m[1]); err != nil {
		t.Errorf("the shown token does not work: %v", err)
	}
	if _, after := h.get("/settings"); strings.Contains(after, m[1]) {
		t.Error("the token is shown again on a later load")
	}

	tokens, err := store.ListAPITokens(ctx, h.db)
	if err != nil || len(tokens) != 1 {
		t.Fatalf("tokens = %v, %v", tokens, err)
	}
	if res, _ := h.post("/settings/tokens/"+itoa(tokens[0].ID)+"/revoke",
		url.Values{"csrf": {h.csrf("/settings")}}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("token revoke gave %d", res.StatusCode)
	}
	if _, _, err := h.srv.auth.APIToken(ctx, m[1]); err == nil {
		t.Error("a revoked token still works")
	}
}

func TestRoleChangesAreGuarded(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.setupOwner()
	owner, err := store.UserByHandle(ctx, h.db, "dt")
	if err != nil {
		t.Fatal(err)
	}

	res, body := h.post("/settings/team/role", url.Values{
		"csrf": {h.csrf("/settings")}, "user": {itoa(owner.ID)}, "role": {auth.RoleEditor},
	})
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "your own role") {
		t.Errorf("changing your own role gave %d", res.StatusCode)
	}

	id, err := store.CreateUser(ctx, h.db, &store.User{Handle: "mara", Email: "mara@example.fm",
		Name: "Mara Okafor", Initials: "MO", Colour: Palette[2], Role: auth.RoleGuest, PasswordHash: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if res, _ := h.post("/settings/team/role", url.Values{
		"csrf": {h.csrf("/settings")}, "user": {itoa(id)}, "role": {auth.RoleEditor},
	}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("role change gave %d", res.StatusCode)
	}
	u, _ := store.UserByID(ctx, h.db, id)
	if u.Role != auth.RoleEditor {
		t.Errorf("role = %q", u.Role)
	}

	// A second owner can be demoted; the one doing it is still an owner.
	h.setRole(t, id, auth.RoleOwner)
	if res, _ := h.post("/settings/team/role", url.Values{
		"csrf": {h.csrf("/settings")}, "user": {itoa(id)}, "role": {auth.RoleEditor},
	}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("demoting a second owner gave %d", res.StatusCode)
	}
}

func TestPasswordChangeAndResetLink(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	password, secret := h.setupOwner()

	res, body := h.post("/profile/password", url.Values{
		"csrf": {h.csrf("/profile")}, "current": {"not my password"},
		"password": {"a brand new password"}, "code": {code(t, secret)},
	})
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "current password") {
		t.Errorf("a wrong current password gave %d", res.StatusCode)
	}
	res, body = h.post("/profile/password", url.Values{
		"csrf": {h.csrf("/profile")}, "current": {password},
		"password": {"short"}, "code": {code(t, secret)},
	})
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "twelve") {
		t.Errorf("a short new password gave %d", res.StatusCode)
	}
	if res, _ := h.post("/profile/password", url.Values{
		"csrf": {h.csrf("/profile")}, "current": {password},
		"password": {"a brand new password"}, "code": {code(t, secret)},
	}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("the password change gave %d", res.StatusCode)
	}
	u, _ := store.UserByHandle(ctx, h.db, "dt")
	if !auth.CheckPassword(u.PasswordHash, "a brand new password") {
		t.Error("the new password does not verify")
	}

	// The reset link sets another password and strands every session.
	token, err := h.srv.auth.CreatePasswordReset(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res, _ := h.post("/reset/"+token, url.Values{
		"csrf": {h.csrf("/reset/" + token)}, "password": {"a third long password"},
	}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("the reset gave %d", res.StatusCode)
	}
	u, _ = store.UserByHandle(ctx, h.db, "dt")
	if !auth.CheckPassword(u.PasswordHash, "a third long password") {
		t.Error("the reset password does not verify")
	}
	if res, _ := h.get("/profile"); res.Header.Get("Location") != "/login" {
		t.Error("the session survived a password reset")
	}
	if res, _ := h.post("/reset/"+token, url.Values{
		"csrf": {h.csrf("/reset/" + token)}, "password": {"a fourth long password"},
	}); res.StatusCode != http.StatusNotFound {
		t.Errorf("a spent reset link gave %d", res.StatusCode)
	}
}

func TestDeleteAccountRefusesTheLastOwner(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.setupOwner()

	res, body := h.post("/profile/delete", url.Values{"csrf": {h.csrf("/profile")}})
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "last owner") {
		t.Fatalf("deleting the last owner gave %d", res.StatusCode)
	}
	if n, _ := store.CountUsers(ctx, h.db); n != 1 {
		t.Fatal("the owner was deleted anyway")
	}

	if _, err := store.CreateUser(ctx, h.db, &store.User{Handle: "mara", Email: "mara@example.fm",
		Name: "Mara Okafor", Initials: "MO", Colour: Palette[2], Role: auth.RoleOwner, PasswordHash: "x"}); err != nil {
		t.Fatal(err)
	}
	if res, _ := h.post("/profile/delete", url.Values{"csrf": {h.csrf("/profile")}}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("deleting a second owner gave %d", res.StatusCode)
	}
	if _, err := store.UserByHandle(ctx, h.db, "dt"); err == nil {
		t.Error("the account was not deleted")
	}
}

func TestForbiddenPageLinksOnlyToLogin(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	h.setRole(t, 1, auth.RoleGuest)
	res, body := h.get("/settings")
	if res.StatusCode != http.StatusForbidden || !strings.Contains(body, "No access.") {
		t.Fatalf("status = %d", res.StatusCode)
	}
	var hrefs []string
	for _, m := range hrefRe.FindAllStringSubmatch(body, -1) {
		hrefs = append(hrefs, m[1])
	}
	if len(hrefs) != 1 || hrefs[0] != "/login" {
		t.Errorf("links = %v, want only /login", hrefs)
	}
}

func code(t *testing.T, secret string) string { return codeAt(t, secret, time.Now()) }

func codeAt(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	c, err := totp.GenerateCode(secret, at)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A row written before the registry bounded the key would otherwise build an
// expiry that overflows, so every sign-in wrote an already expired session and
// a cookie with a negative max age, and nobody could get in again.
func TestAnOutOfRangeSessionLengthStillSignsYouIn(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	password, secret := h.setupOwner()
	h.signOut()

	if err := store.PutSetting(ctx, h.db, "signin.session_days", "200000", false, 0); err != nil {
		t.Fatal(err)
	}
	reloaded, err := settings.Open(ctx, h.db, []byte("a settings key of at least thirty-two bytes"))
	if err != nil {
		t.Fatal(err)
	}
	h.srv.settings = reloaded
	if got := h.srv.sessionDays(); got != settings.MaxSessionDays {
		t.Fatalf("sessionDays = %d, want it clamped to %d", got, settings.MaxSessionDays)
	}

	res, _ := h.post("/login", url.Values{
		"csrf": {h.csrf("/login")}, "handle": {"dt"}, "password": {password},
		"code": {code(t, secret)},
	})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("login gave %d", res.StatusCode)
	}
	for _, c := range res.Cookies() {
		if c.Name == auth.SessionCookie && c.MaxAge <= 0 {
			t.Errorf("the session cookie was set with max age %d", c.MaxAge)
		}
	}
	if res, _ := h.get("/"); res.StatusCode != http.StatusOK {
		t.Errorf("the session did not survive the redirect: %d", res.StatusCode)
	}
}

// invited walks an invitation as far as the authenticator page and returns the
// secret and CSRF token waiting there, so a test can interfere in between.
func (h *harness) invited(role string) (token, secret, csrf string, id int64) {
	h.Helper()
	ctx := context.Background()
	owner, err := store.UserByHandle(ctx, h.db, "dt")
	if err != nil {
		h.Fatal(err)
	}
	token, err = h.srv.auth.CreateInvitation(ctx, "mara@example.fm", role, owner.ID)
	if err != nil {
		h.Fatal(err)
	}
	inv, err := h.srv.auth.Invitation(ctx, token)
	if err != nil {
		h.Fatal(err)
	}

	_, body := h.get("/invite/" + token)
	res, _ := h.post("/invite/"+token, url.Values{
		"csrf": {csrfRe.FindStringSubmatch(body)[1]}, "handle": {"mara"}, "name": {"Mara Okafor"},
		"initials": {"MO"}, "colour": {Palette[2]}, "password": {"another long password"},
	})
	if res.Header.Get("Location") != "/invite/"+token+"/authenticator" {
		h.Fatalf("accept gave %d", res.StatusCode)
	}
	_, body = h.get("/invite/" + token + "/authenticator")
	return token, secretRe.FindStringSubmatch(body)[1], csrfRe.FindStringSubmatch(body)[1], inv.ID
}

// An invitation revoked while someone is on the authenticator page used to mint
// the account anyway, because the second step never looked at it again.
func TestAnInvitationRevokedMidwayMakesNoAccount(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.setupOwner()
	token, secret, csrf, id := h.invited(auth.RoleEditor)

	if err := store.DeleteInvitation(ctx, h.db, id); err != nil {
		t.Fatal(err)
	}
	res, body := h.post("/invite/"+token+"/authenticator", url.Values{
		"csrf": {csrf}, "code": {code(t, secret)},
	})
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("a revoked invitation gave %d, want the 404 page", res.StatusCode)
	}
	if !strings.Contains(body, "Not found.") {
		t.Error("the refusal is not the error page")
	}
	if _, err := store.UserByHandle(ctx, h.db, "mara"); !errors.Is(err, store.ErrNotFound) {
		t.Error("the account was created from a revoked invitation")
	}
}

func TestAnInvitationExpiringMidwayMakesNoAccount(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.setupOwner()
	token, secret, csrf, _ := h.invited(auth.RoleEditor)

	// The clock moves past the week the invitation was good for. The
	// authenticator code has to come from the same clock, or the code is what
	// gets refused and the test proves nothing.
	later := h.srv.auth.Now().Add(auth.InviteValidity + time.Hour)
	h.srv.auth.Now = func() time.Time { return later }

	res, _ := h.post("/invite/"+token+"/authenticator", url.Values{
		"csrf": {csrf}, "code": {codeAt(t, secret, later)},
	})
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("an expired invitation gave %d, want the 404 page", res.StatusCode)
	}
	if _, err := store.UserByHandle(ctx, h.db, "mara"); !errors.Is(err, store.ErrNotFound) {
		t.Error("the account was created from an expired invitation")
	}
}

// The same accept submitted twice used to reach CreateUser a second time and
// die on the email unique constraint with a 500.
func TestAnInvitationAcceptedTwiceIsRefusedNotA500(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.setupOwner()
	token, secret, csrf, _ := h.invited(auth.RoleEditor)

	if res, _ := h.post("/invite/"+token+"/authenticator", url.Values{
		"csrf": {csrf}, "code": {code(t, secret)},
	}); res.Header.Get("Location") != "/" {
		t.Fatalf("the first accept gave %d", res.StatusCode)
	}

	// The pending cookie is gone after a success, so the replay is put back the
	// way a resubmitted form or a copied cookie would put it.
	h.replayPending(t, "mara", secret, auth.RoleEditor, inviteID(t, h))
	res, _ := h.post("/invite/"+token+"/authenticator", url.Values{
		"csrf": {h.csrf("/invite/" + token + "/authenticator")}, "code": {code(t, secret)},
	})
	if res.StatusCode >= 500 {
		t.Fatalf("a replayed accept gave %d", res.StatusCode)
	}
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("a replayed accept gave %d, want the 404 page", res.StatusCode)
	}
	var n int
	if err := h.db.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE handle = 'mara'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d accounts named mara", n)
	}
}

func inviteID(t *testing.T, h *harness) int64 {
	t.Helper()
	var id int64
	if err := h.db.QueryRowContext(context.Background(), `SELECT id FROM invitations`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// replayPending puts an enrolment cookie back in the jar, which is what a
// resubmitted form or a copied cookie amounts to.
func (h *harness) replayPending(t *testing.T, handle, secret, role string, invitation int64) {
	t.Helper()
	hash, err := auth.HashPassword("another long password")
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	if err := h.srv.pending.put(w, false, &pending{
		Kind: "invite", InvitationID: invitation, Handle: handle, Name: "Mara Okafor",
		Initials: "MO", Colour: Palette[2], Email: "mara@example.fm", PasswordHash: hash,
		Role: role, Secret: secret, OTPURL: "otpauth://totp/THESES:" + handle + "?secret=" + secret + "&issuer=THESES",
	}); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(h.http.URL)
	h.client.Jar.SetCookies(u, w.Result().Cookies())
}

// A reset link or an accept link in the process log is a way in for anything
// that can read the log, and logs travel further than the database does.
func TestNoResetOrInviteLinkReachesTheLog(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()

	if res, _ := h.post("/settings/team/invite", url.Values{
		"csrf": {h.csrf("/settings")}, "email": {"mara@example.fm"}, "role": {auth.RoleEditor},
	}); res.StatusCode != http.StatusOK {
		t.Fatalf("invite gave %d", res.StatusCode)
	}
	h.signOut()
	if res, _ := h.post("/reset", url.Values{
		"csrf": {h.csrf("/reset")}, "who": {"dt"},
	}); res.StatusCode != http.StatusOK {
		t.Fatalf("reset gave %d", res.StatusCode)
	}

	logged := h.log.String()
	// The events are still recorded, so an operator can see them happen.
	for _, want := range []string{"invitation issued", "password reset requested"} {
		if !strings.Contains(logged, want) {
			t.Errorf("the log does not record %q", want)
		}
	}
	for _, leak := range []string{"/reset/", "/invite/"} {
		for _, line := range strings.Split(logged, "\n") {
			// Request lines name the path that was asked for, which is not a token.
			if strings.Contains(line, "msg=request") {
				continue
			}
			if strings.Contains(line, leak) {
				t.Errorf("the log carries a %s link: %s", leak, line)
			}
		}
	}
}

// Hashing before checking the token let anyone spend a full bcrypt on the
// server with a URL they made up.
func TestAWrongResetTokenDoesNoHashing(t *testing.T) {
	was := auth.BcryptCost
	auth.BcryptCost = 12
	defer func() { auth.BcryptCost = was }()

	h := newHarness(t)
	h.setupOwner()
	u, err := store.UserByHandle(context.Background(), h.db, "dt")
	if err != nil {
		t.Fatal(err)
	}
	good, err := h.srv.auth.CreatePasswordReset(context.Background(), u.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Both tokens are fetched before either region is timed, so the ratio is the
	// POST and nothing else.
	wrongCSRF := h.csrf("/reset/not-a-real-token")
	rightCSRF := h.csrf("/reset/" + good)

	start := time.Now()
	res, _ := h.post("/reset/not-a-real-token", url.Values{
		"csrf": {wrongCSRF}, "password": {"a long enough password"},
	})
	wrong := time.Since(start)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("a wrong token gave %d", res.StatusCode)
	}

	start = time.Now()
	if res, _ := h.post("/reset/"+good, url.Values{
		"csrf": {rightCSRF}, "password": {"a long enough password"},
	}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("a real token gave %d", res.StatusCode)
	}
	right := time.Since(start)

	if wrong*4 > right {
		t.Errorf("a wrong token took %v against %v for a real one, so it hashed first", wrong, right)
	}
}

func TestTheResetFormIsRateLimited(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	h.signOut()

	token := h.csrf("/reset/whatever")
	limited := 0
	for i := 0; i < 20; i++ {
		res, _ := h.post("/reset/whatever", url.Values{"csrf": {token}, "password": {"a long enough password"}})
		switch res.StatusCode {
		case http.StatusNotFound:
		case http.StatusTooManyRequests:
			limited = i + 1
		default:
			t.Fatalf("attempt %d gave %d", i+1, res.StatusCode)
		}
		if limited != 0 {
			break
		}
	}
	if limited == 0 {
		t.Error("twenty guesses at a reset token were never rate limited")
	}
}

// ParseForm reads the whole body into memory, so without a cap one request
// could ask the process to hold as much as the sender cared to send.
func TestAnOversizedFormIsRefused(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()

	big := url.Values{
		"csrf":           {h.csrf("/settings")},
		"workspace.name": {strings.Repeat("x", 100<<10)},
	}
	res, _ := h.post("/settings", big)
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("a 100 KB form gave %d, want 413", res.StatusCode)
	}
	if got := settings.Get[string](h.srv.settings, "workspace.name"); len(got) > 100 {
		t.Error("the oversized value was saved")
	}

	// A form of the size the app actually sends still goes through.
	if res, _ := h.post("/settings", url.Values{
		"csrf": {h.csrf("/settings")}, "workspace.name": {"Debt Machine"},
	}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("an ordinary form gave %d", res.StatusCode)
	}
}

// A refusal is not a mutation, so it should leave no trace in the activity log.
// The guard ran inside the same transaction as the activity row but only the
// statement was conditional, so the row was committed either way.
func TestARefusedDeleteLeavesNoActivityRow(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.setupOwner()
	before := activityCount(t, h, "delete")

	res, body := h.post("/profile/delete", url.Values{"csrf": {h.csrf("/profile")}})
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "last owner") {
		t.Fatalf("deleting the last owner gave %d", res.StatusCode)
	}
	if got := activityCount(t, h, "delete"); got != before {
		t.Errorf("%d delete rows in the activity log, want %d", got, before)
	}
	if n, _ := store.CountUsers(ctx, h.db); n != 1 {
		t.Error("the owner was deleted after all")
	}
}

// The role change has the same shape but its refusal cannot be reached over
// HTTP: only an owner may post to it, changing your own role is refused before
// the guard, and any other owner being demoted means there are at least two. So
// the rollback is checked on s.write itself, which is what both handlers use.
func TestARefusedWriteCommitsNoActivityRow(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	owner, err := store.UserByHandle(context.Background(), h.db, "dt")
	if err != nil {
		t.Fatal(err)
	}
	before := activityCount(t, h, "role")

	r := httptest.NewRequest("POST", "/settings/team/role", nil)
	r = r.WithContext(context.WithValue(r.Context(), userKey, owner))
	err = h.srv.write(r, "user", itoa(owner.ID), "role", auth.RoleOwner, auth.RoleEditor,
		func(q store.Querier) error { return errRefused })
	if !errors.Is(err, errRefused) {
		t.Fatalf("write returned %v, want errRefused", err)
	}
	if got := activityCount(t, h, "role"); got != before {
		t.Errorf("%d role rows in the activity log, want %d", got, before)
	}

	// The same write that succeeds does record one.
	err = h.srv.write(r, "user", itoa(owner.ID), "role", auth.RoleOwner, auth.RoleEditor,
		func(q store.Querier) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if got := activityCount(t, h, "role"); got != before+1 {
		t.Errorf("a write that went through recorded %d rows, want %d", got, before+1)
	}
}

func activityCount(t *testing.T, h *harness, action string) int {
	t.Helper()
	var n int
	if err := h.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM activity WHERE action = ?`, action).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The link is not in the log and mail does not exist yet, so the page that made
// the invitation is the one place it can be read from, once.
func TestANewInvitationShowsItsLinkOnce(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.setupOwner()

	res, body := h.post("/settings/team/invite", url.Values{
		"csrf": {h.csrf("/settings")}, "email": {"mara@example.fm"}, "role": {auth.RoleEditor},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("invite gave %d", res.StatusCode)
	}
	m := inviteLinkRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("the accept link is not on the page:\n%s", body)
	}
	link := m[1]

	// It is a link that works.
	if _, err := h.srv.auth.Invitation(ctx, strings.TrimPrefix(link, h.srv.cfg.BaseURL+"/invite/")); err != nil {
		t.Errorf("the link on the page does not resolve: %v", err)
	}

	// And it is not on the page the next time it is loaded.
	if _, again := h.get("/settings"); inviteLinkRe.MatchString(again) {
		t.Error("the accept link is shown again on a later load")
	}

	// Resending shows the new link once, and it is a different one.
	pending, err := store.ListPendingInvitations(ctx, h.db, time.Now().Unix())
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending invitations = %v, %v", pending, err)
	}
	res, body = h.post("/settings/team/invite/"+itoa(pending[0].ID)+"/resend",
		url.Values{"csrf": {h.csrf("/settings")}})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("resend gave %d", res.StatusCode)
	}
	again := inviteLinkRe.FindStringSubmatch(body)
	if again == nil {
		t.Fatal("resending showed no link")
	}
	if again[1] == link {
		t.Error("resending showed the link it had just replaced")
	}
	if _, body := h.get("/settings"); inviteLinkRe.MatchString(body) {
		t.Error("the resent link is shown again on a later load")
	}
}

// Back after signing out used to redisplay whatever the browser had cached,
// which for /settings is member addresses, pending invitations and the
// environment section, and for the panels shown once is the whole point of them.
func TestRenderedPagesAreNotCached(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()

	for _, p := range []string{"/", "/settings", "/profile", "/offline", "/no-such-page"} {
		res, _ := h.get(p)
		if got := res.Header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control = %q", p, got)
		}
	}

	// A page rendered as the answer to a POST, which is where the once-only
	// panels live.
	res, _ := h.post("/settings/tokens", url.Values{
		"csrf": {h.csrf("/settings")}, "name": {"research agent"}, "scopes": {"read"},
	})
	if got := res.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("a new token page: Cache-Control = %q", got)
	}

	h.signOut()
	if res, _ := h.get("/login"); res.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("/login: Cache-Control = %q", res.Header.Get("Cache-Control"))
	}

	// Static files are content hashed and keep their own year.
	res, _ = h.get(h.srv.assets.URL("app.css"))
	if got := res.Header.Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("app.css: Cache-Control = %q", got)
	}
}
