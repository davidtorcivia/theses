package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/docs"
	"github.com/davidtorcivia/theses/internal/search"
	"github.com/davidtorcivia/theses/internal/settings"
	"github.com/davidtorcivia/theses/internal/store"
)

type harness struct {
	*testing.T
	db      *store.DB
	auth    *auth.Auth
	set     *settings.Settings
	handler http.Handler
	user    *store.User
	board   *board.Service
	docs    *docs.Service
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	db := store.OpenTemp(t)
	set, err := settings.Open(ctx, db, []byte("a secret key of at least thirty-two bytes"))
	if err != nil {
		t.Fatal(err)
	}
	a := auth.New(db, []byte("a session key of at least thirty-two bytes"), false, false)
	id, err := store.CreateUser(ctx, db, &store.User{
		Handle: "nora", Email: "nora@example.com", Name: "Nora Vance",
		Initials: "NV", Colour: "#b45", Role: "owner", PasswordHash: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	user, err := store.UserByID(ctx, db, id)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	api := New(db, a, set, log)
	b := board.New(core.New(db, core.NewBus()), func() board.Defaults {
		return board.Defaults{Status: "idea", Statuses: []string{"idea", "recording"}, Columns: []string{"Research"}}
	})
	api.Docs = docs.New(b.Service, "", func() string { return "" }, log)
	api.Board = b
	return &harness{T: t, db: db, auth: a, set: set, handler: api.Handler(), user: user,
		board: b, docs: api.Docs}
}

// token returns a clear API token with the given scopes.
func (h *harness) token(scopes ...string) string {
	h.Helper()
	clear, err := h.auth.CreateAPIToken(context.Background(), h.user.ID, strings.Join(scopes, "-"), scopes)
	if err != nil {
		h.Fatal(err)
	}
	return clear
}

// do sends one request with a bearer token, or with no header when token is "".
func (h *harness) do(method, target, token, body string) *httptest.ResponseRecorder {
	h.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	return w
}

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("body %q is not JSON: %v", w.Body.String(), err)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
	return out
}

// into decodes a response into a typed value, where decode returns the loose
// map most of these tests read one field out of.
func into(t *testing.T, w *httptest.ResponseRecorder, out any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
		t.Fatalf("body %q is not JSON: %v", w.Body.String(), err)
	}
}

func TestTokenIsRequiredAndScoped(t *testing.T) {
	h := newHarness(t)
	read := h.token(auth.ScopeRead)

	revoked := h.token(auth.ScopeRead, auth.ScopeWrite)
	var id int64
	if err := h.db.QueryRowContext(context.Background(),
		`SELECT id FROM api_tokens WHERE name = 'read-write'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeAPIToken(context.Background(), h.db, id); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, token string
		want        int
	}{
		{"no header", "", http.StatusUnauthorized},
		{"not a token", "thes_nonsense", http.StatusUnauthorized},
		{"not one of ours", "nonsense", http.StatusUnauthorized},
		{"revoked", revoked, http.StatusUnauthorized},
		{"valid", read, http.StatusOK},
	}
	for _, c := range cases {
		w := h.do("GET", "/api/v1/users", c.token, "")
		if w.Code != c.want {
			t.Errorf("%s: status %d, want %d (%s)", c.name, w.Code, c.want, w.Body.String())
		}
		if c.want != http.StatusOK {
			if body := decode(t, w); body["error"] == "" || body["error"] == nil {
				t.Errorf("%s: no error field in %s", c.name, w.Body.String())
			}
			if a := w.Header().Get("WWW-Authenticate"); a == "" {
				t.Errorf("%s: no WWW-Authenticate header", c.name)
			}
		}
	}

	// A scope is not a role: a read token is refused the admin routes.
	if w := h.do("GET", "/api/v1/settings", read, ""); w.Code != http.StatusForbidden {
		t.Errorf("settings with a read token: %d (%s)", w.Code, w.Body.String())
	}
	// Every token may ask who it is.
	if w := h.do("GET", "/api/v1/me", read, ""); w.Code != http.StatusOK {
		t.Errorf("me with a read token: %d (%s)", w.Code, w.Body.String())
	}
}

func TestValidTokenIsTouched(t *testing.T) {
	h := newHarness(t)
	if w := h.do("GET", "/api/v1/me", h.token(auth.ScopeRead), ""); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	var used *int64
	if err := h.db.QueryRowContext(context.Background(),
		`SELECT last_used_at FROM api_tokens`).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if used == nil {
		t.Error("last_used_at was not recorded")
	}
}

func TestMeReportsTheTokenAndItsUser(t *testing.T) {
	h := newHarness(t)
	w := h.do("GET", "/api/v1/me", h.token(auth.ScopeRead, auth.ScopeFiles), "")
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	body := decode(t, w)
	token := body["token"].(map[string]any)
	if token["name"] != "read-files" {
		t.Errorf("token = %v", token)
	}
	scopes := token["scopes"].([]any)
	if len(scopes) != 2 || scopes[0] != "read" || scopes[1] != "files" {
		t.Errorf("scopes = %v", scopes)
	}
	if user := body["user"].(map[string]any); user["handle"] != "nora" || user["role"] != "owner" {
		t.Errorf("user = %v", user)
	}
}

func TestUsersListsTheTeamWithoutEmails(t *testing.T) {
	h := newHarness(t)
	w := h.do("GET", "/api/v1/users", h.token(auth.ScopeRead), "")
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	users := decode(t, w)["users"].([]any)
	if len(users) != 1 {
		t.Fatalf("users = %v", users)
	}
	u := users[0].(map[string]any)
	if u["handle"] != "nora" || u["initials"] != "NV" {
		t.Errorf("user = %v", u)
	}
	if _, ok := u["email"]; ok {
		t.Error("the user list carries email addresses")
	}
}

func TestSettingsHideSecretsAndWriteAsTheToken(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	if err := h.set.Set(ctx, "mail.password", []string{"hunter2-but-longer"}, h.user.ID); err != nil {
		t.Fatal(err)
	}
	admin := h.token(auth.ScopeAdmin)

	w := h.do("GET", "/api/v1/settings", admin, "")
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	if strings.Contains(w.Body.String(), "hunter2") {
		t.Error("a secret was returned")
	}
	var secretSeen bool
	for _, s := range decode(t, w)["settings"].([]any) {
		s := s.(map[string]any)
		if s["key"] == "backups.last_ok_at" || s["key"] == "notify.last_tick" {
			t.Errorf("internal state is in the settings listing: %v", s)
		}
		if s["key"] == "mail.password" {
			secretSeen = true
			if s["secret"] != true || s["set"] != true {
				t.Errorf("mail.password = %v", s)
			}
			if _, ok := s["value"]; ok {
				t.Errorf("mail.password carries a value: %v", s)
			}
		}
		if s["key"] == "workspace.name" && s["value"] != "Workspace" {
			t.Errorf("workspace.name = %v", s)
		}
	}
	if !secretSeen {
		t.Error("mail.password is missing from the listing")
	}

	w = h.do("PUT", "/api/v1/settings/workspace.name", admin, `{"value":"Renamed workspace"}`)
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	if got := decode(t, w)["value"]; got != "Renamed workspace" {
		t.Errorf("value = %v", got)
	}
	if got := settings.Get[string](h.set, "workspace.name"); got != "Renamed workspace" {
		t.Errorf("stored workspace.name = %q", got)
	}

	var kind, actorID string
	if err := h.db.QueryRowContext(ctx, `SELECT actor_kind, actor_id FROM activity
		WHERE entity_id = 'workspace.name'`).Scan(&kind, &actorID); err != nil {
		t.Fatal(err)
	}
	if want := strconv.FormatInt(h.user.ID, 10); kind != "user" || actorID != want {
		t.Errorf("activity actor = %s %s, want user %s", kind, actorID, want)
	}
}

func TestActorIsTheOwnerByWayOfTheToken(t *testing.T) {
	h := newHarness(t)
	token, user, err := h.auth.APIToken(context.Background(), h.token(auth.ScopeAdmin))
	if err != nil {
		t.Fatal(err)
	}
	actor := Principal{Token: token, User: user}.Actor()
	want := settings.Actor{
		Kind: "user", ID: strconv.FormatInt(h.user.ID, 10),
		Via: "token:admin", UserID: h.user.ID,
	}
	if actor != want {
		t.Errorf("actor = %+v, want %+v", actor, want)
	}
}

func TestSettingsWriteRefusesWhatSettingsRefuses(t *testing.T) {
	h := newHarness(t)
	admin := h.token(auth.ScopeAdmin)

	cases := []struct {
		name, target, body string
		want               int
	}{
		{"unknown key", "/api/v1/settings/not.a.key", `{"value":"x"}`, http.StatusNotFound},
		{"not a number", "/api/v1/settings/signin.session_days", `{"value":"soon"}`, http.StatusUnprocessableEntity},
		{"out of range", "/api/v1/settings/signin.session_days", `{"value":4000}`, http.StatusUnprocessableEntity},
		{"not a choice", "/api/v1/settings/workspace.release_day", `{"value":"Caturday"}`, http.StatusUnprocessableEntity},
		{"internal state", "/api/v1/settings/backups.last_ok_at", `{"value":2000000000}`, http.StatusUnprocessableEntity},
		{"not JSON", "/api/v1/settings/workspace.name", `hello`, http.StatusBadRequest},
		{"no value", "/api/v1/settings/workspace.name", `{}`, http.StatusBadRequest},
		{"list of numbers", "/api/v1/settings/defaults.columns", `{"value":[1,2]}`, http.StatusBadRequest},
		{"a list", "/api/v1/settings/defaults.columns", `{"value":["Research","Edit"]}`, http.StatusOK},
		{"a number", "/api/v1/settings/signin.session_days", `{"value":7}`, http.StatusOK},
	}
	for _, c := range cases {
		w := h.do("PUT", c.target, admin, c.body)
		if w.Code != c.want {
			t.Errorf("%s: status %d, want %d (%s)", c.name, w.Code, c.want, w.Body.String())
		}
	}
	if got := settings.Get[[]string](h.set, "defaults.columns"); len(got) != 2 {
		t.Errorf("defaults.columns = %v", got)
	}
	if got := settings.Get[int](h.set, "signin.session_days"); got != 7 {
		t.Errorf("signin.session_days = %d", got)
	}
}

func TestSettingsWriteRefusesALargeBody(t *testing.T) {
	h := newHarness(t)
	body := `{"value":"` + strings.Repeat("x", maxBodyBytes) + `"}`
	w := h.do("PUT", "/api/v1/settings/workspace.name", h.token(auth.ScopeAdmin), body)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status %d (%s)", w.Code, w.Body.String())
	}
}

func TestActivityIsACursor(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	for _, name := range []string{"one", "two", "three"} {
		if err := store.InsertActivity(ctx, h.db, "user", "1", "", "card", name, "create", "", `{"a":1}`); err != nil {
			t.Fatal(err)
		}
	}
	read := h.token(auth.ScopeRead)

	w := h.do("GET", "/api/v1/activity?limit=2", read, "")
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	rows := decode(t, w)["activity"].([]any)
	if len(rows) != 2 {
		t.Fatalf("rows = %v", rows)
	}
	first := rows[0].(map[string]any)
	if first["entity_id"] != "one" || first["actor_kind"] != "user" {
		t.Errorf("first row = %v", first)
	}
	if after := first["after"].(map[string]any); after["a"] != float64(1) {
		t.Errorf("after = %v", after)
	}
	if _, ok := first["before"]; ok {
		t.Errorf("an empty before was returned: %v", first)
	}

	last := int64(rows[1].(map[string]any)["id"].(float64))
	w = h.do("GET", "/api/v1/activity?since="+strconv.FormatInt(last, 10), read, "")
	rows = decode(t, w)["activity"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["entity_id"] != "three" {
		t.Errorf("after the cursor: %v", rows)
	}
}

func TestSearchNeedsReadAndFindsARow(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	if _, err := h.db.ExecContext(ctx, `INSERT INTO propositions
		(id, number, title, statement, status, position, created_at)
		VALUES (1, 10, 'Student debt is a policy choice', '', 'idea', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if w := h.do("GET", "/api/v1/search?q=debt", h.token(auth.ScopeFiles), ""); w.Code != http.StatusForbidden {
		t.Errorf("search without read: %d", w.Code)
	}
	w := h.do("GET", "/api/v1/search?q=debt", h.token(auth.ScopeRead), "")
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	body := decode(t, w)
	if body["query"] != "debt" {
		t.Errorf("query = %v", body["query"])
	}
	groups := body["groups"].([]any)
	if len(groups) != 1 {
		t.Fatalf("groups = %v", groups)
	}
	g := groups[0].(map[string]any)
	if g["kind"] != "proposition" {
		t.Errorf("group = %v", g)
	}

	// A query of pure punctuation is an empty list, not an error.
	w = h.do("GET", "/api/v1/search?q=%2A%2A", h.token(auth.ScopeRead), "")
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	if groups := decode(t, w)["groups"].([]any); len(groups) != 0 {
		t.Errorf("groups = %v", groups)
	}
}

func TestUnknownEndpointIsJSON(t *testing.T) {
	h := newHarness(t)
	w := h.do("GET", "/api/v1/cards", h.token(auth.ScopeRead), "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d", w.Code)
	}
	if decode(t, w)["error"] == nil {
		t.Errorf("body = %s", w.Body.String())
	}
}

func TestATokenIsRateLimited(t *testing.T) {
	h := newHarness(t)
	token := h.token(auth.ScopeRead)
	var last *httptest.ResponseRecorder
	for range 301 {
		last = h.do("GET", "/api/v1/me", token, "")
	}
	if last.Code != http.StatusTooManyRequests {
		t.Errorf("status %d after 301 requests, want 429", last.Code)
	}
	if decode(t, last)["error"] == nil {
		t.Errorf("body = %s", last.Body.String())
	}
}

func TestActivityCarriesRowsThatAreNotJSON(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	// A role change stores the role either side, which is a bare word and not
	// the JSON the column usually holds.
	if err := store.InsertActivity(ctx, h.db, "user", "1", "", "user", "2", "role", "owner", "guest"); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertActivity(ctx, h.db, "user", "1", "", "card", "3", "create", "", `{"t":"x"}`); err != nil {
		t.Fatal(err)
	}

	w := h.do("GET", "/api/v1/activity", h.token(auth.ScopeRead), "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %q", w.Code, w.Body.String())
	}
	rows := decode(t, w)["activity"].([]any)
	if len(rows) != 2 {
		t.Fatalf("rows = %v", rows)
	}
	first := rows[0].(map[string]any)
	if first["before"] != "owner" || first["after"] != "guest" {
		t.Errorf("bare values came back as %v and %v", first["before"], first["after"])
	}
	if after := rows[1].(map[string]any)["after"].(map[string]any); after["t"] != "x" {
		t.Errorf("JSON value came back as %v", after)
	}
}

// demote changes the role in place, past the guard that keeps one owner, since
// what is being tested is what happens to a token afterwards.
func (h *harness) demote(role string) {
	h.Helper()
	if _, err := h.db.ExecContext(context.Background(),
		`UPDATE users SET role = ? WHERE id = ?`, role, h.user.ID); err != nil {
		h.Fatal(err)
	}
}

func TestATokenCannotOutrankItsOwner(t *testing.T) {
	h := newHarness(t)
	admin := h.token(auth.ScopeAdmin)
	if w := h.do("GET", "/api/v1/settings", admin, ""); w.Code != http.StatusOK {
		t.Fatalf("before the demotion: %d (%s)", w.Code, w.Body.String())
	}

	h.demote("guest")

	for _, target := range []string{"/api/v1/settings", "/api/v1/users"} {
		w := h.do("GET", target, admin, "")
		if target == "/api/v1/users" {
			// A guest may still read, and admin implies read.
			if w.Code != http.StatusOK {
				t.Errorf("%s: %d (%s)", target, w.Code, w.Body.String())
			}
			continue
		}
		if w.Code != http.StatusForbidden {
			t.Errorf("%s: %d (%s)", target, w.Code, w.Body.String())
		}
		if got := decode(t, w)["error"]; got != "the person this token belongs to no longer has that permission" {
			t.Errorf("%s: error = %v", target, got)
		}
	}
	w := h.do("PUT", "/api/v1/settings/workspace.name", admin, `{"value":"Renamed workspace"}`)
	if w.Code != http.StatusForbidden {
		t.Errorf("a demoted owner's token wrote a setting: %d", w.Code)
	}
}

func TestActivityHidesAdministrationFromAReadToken(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	rows := []struct{ entity, entityID, before, after string }{
		{"card", "1", "", `{"t":"x"}`},
		{"setting", "mail.host", `"old.example.com"`, `"smtp.example.com"`},
		{"invitation", "invitee@example.com", "", "editor"},
		{"api_token", "deploy", "", "admin"},
	}
	for _, e := range rows {
		if err := store.InsertActivity(ctx, h.db, "user", "1", "",
			e.entity, e.entityID, "set", e.before, e.after); err != nil {
			t.Fatal(err)
		}
	}

	w := h.do("GET", "/api/v1/activity", h.token(auth.ScopeRead), "")
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	if body := w.Body.String(); strings.Contains(body, "invitee@example.com") ||
		strings.Contains(body, "smtp.example.com") || strings.Contains(body, "deploy") {
		t.Errorf("a read token saw administration: %s", body)
	}
	if got := decode(t, w)["activity"].([]any); len(got) != 1 {
		t.Errorf("rows = %v", got)
	}

	w = h.do("GET", "/api/v1/activity", h.token(auth.ScopeAdmin), "")
	if got := decode(t, w)["activity"].([]any); len(got) != 4 {
		t.Errorf("an admin token saw %d of 4 rows", len(got))
	}
}

// lastUsed is the token's last_used_at, or nil when it has never been recorded.
func (h *harness) lastUsed() *int64 {
	h.Helper()
	var at *int64
	if err := h.db.QueryRowContext(context.Background(),
		`SELECT last_used_at FROM api_tokens`).Scan(&at); err != nil {
		h.Fatal(err)
	}
	return at
}

func (h *harness) setLastUsed(sql string) {
	h.Helper()
	if _, err := h.db.ExecContext(context.Background(),
		`UPDATE api_tokens SET last_used_at = `+sql); err != nil {
		h.Fatal(err)
	}
}

func TestARefusedRequestRecordsNothing(t *testing.T) {
	h := newHarness(t)
	token := h.token(auth.ScopeRead)
	for range 300 {
		h.do("GET", "/api/v1/me", token, "")
	}
	h.setLastUsed("NULL")

	w := h.do("GET", "/api/v1/me", token, "")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429", w.Code)
	}
	if at := h.lastUsed(); at != nil {
		t.Errorf("a refused request wrote last_used_at = %d", *at)
	}
}

func TestTheUseIsRecordedAtMostOnceAMinute(t *testing.T) {
	h := newHarness(t)
	token := h.token(auth.ScopeRead)

	if w := h.do("GET", "/api/v1/me", token, ""); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	if h.lastUsed() == nil {
		t.Fatal("the first use was not recorded")
	}

	h.setLastUsed("unixepoch() - 5")
	recent := *h.lastUsed()
	if w := h.do("GET", "/api/v1/me", token, ""); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	if at := *h.lastUsed(); at != recent {
		t.Errorf("a use five seconds after the last one rewrote %d as %d", recent, at)
	}

	h.setLastUsed("unixepoch() - 120")
	stale := *h.lastUsed()
	if w := h.do("GET", "/api/v1/me", token, ""); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	if at := *h.lastUsed(); at == stale {
		t.Errorf("a use two minutes after the last one did not record %d", at)
	}
}

func TestTheBearerSchemeIsNotCaseSensitive(t *testing.T) {
	h := newHarness(t)
	token := h.token(auth.ScopeRead)
	cases := []struct {
		header string
		want   int
	}{
		{"Bearer " + token, http.StatusOK},
		{"bearer " + token, http.StatusOK},
		{"BEARER " + token, http.StatusOK},
		{"Basic " + token, http.StatusUnauthorized},
		{token, http.StatusUnauthorized},
		{"Bearer", http.StatusUnauthorized},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/api/v1/me", nil)
		r.Header.Set("Authorization", c.header)
		w := httptest.NewRecorder()
		h.handler.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%q: status %d, want %d", c.header, w.Code, c.want)
		}
	}
}

// A storage failure is the server's fault and must not come back as a 400 with
// driver text in it. The settings table is dropped rather than the database
// closed, so that the token still resolves and the write is the only thing that
// fails.
func TestASettingThatCannotBeStoredIsAServerError(t *testing.T) {
	h := newHarness(t)
	admin := h.token(auth.ScopeAdmin)
	if _, err := h.db.ExecContext(context.Background(), `DROP TABLE settings`); err != nil {
		t.Fatal(err)
	}
	w := h.do("PUT", "/api/v1/settings/workspace.name", admin, `{"value":"Renamed workspace"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500 (%s)", w.Code, w.Body.String())
	}
	if got := decode(t, w)["error"]; got != "something went wrong here" {
		t.Errorf("error = %v", got)
	}
}

// A token reads what its owner reads. Somebody who is a member of nothing sees
// no proposition's cards in search and no proposition's rows in the log,
// whatever the token's scopes say. Rows about the workspace itself carry no
// proposition and are unaffected.
func TestSearchAndActivityShowOnlyWhatMembershipAllows(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	boards := board.New(core.New(h.db, core.NewBus()), func() board.Defaults {
		return board.Defaults{Status: "idea", Statuses: []string{"idea", "recording"}, Columns: []string{"Research"}}
	})
	owner := core.Actor{Kind: core.KindUser, ID: h.user.ID, Name: h.user.Name}
	e, err := boards.CreateProposition(ctx, owner, "Tidal Power")
	if err != nil {
		t.Fatal(err)
	}
	cols, err := board.ListColumns(ctx, h.db, e.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := boards.CreateCard(ctx, owner, cols[0].ID, "Tidal survey notes", nil); err != nil {
		t.Fatal(err)
	}
	// A row about the workspace and not about a proposition. Not a setting,
	// because a token without the admin scope does not read those either and
	// the two filters would be indistinguishable.
	if err := store.InsertActivity(ctx, h.db, "user", "1", "", "user", "1", "create", "", `{}`); err != nil {
		t.Fatal(err)
	}

	read := h.token(auth.ScopeRead)
	hits := func() []search.Hit {
		t.Helper()
		var body struct {
			Groups []search.Group `json:"groups"`
		}
		into(t, h.do("GET", "/api/v1/search?q=tidal", read, ""), &body)
		var all []search.Hit
		for _, g := range body.Groups {
			all = append(all, g.Hits...)
		}
		return all
	}
	rows := func() []ActivityView {
		t.Helper()
		var body struct {
			Activity []ActivityView `json:"activity"`
		}
		into(t, h.do("GET", "/api/v1/activity", read, ""), &body)
		return body.Activity
	}

	if len(hits()) == 0 {
		t.Fatal("the owner found nothing")
	}
	propositionRows := 0
	for _, row := range rows() {
		if row.PropositionID != 0 {
			propositionRows++
		}
	}
	if propositionRows == 0 {
		t.Fatal("the owner's log holds no proposition rows")
	}

	// The same token, once its owner reads only through membership and has
	// none.
	if _, err := h.db.ExecContext(ctx, `UPDATE users SET role = 'guest' WHERE id = ?`, h.user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.ExecContext(ctx, `DELETE FROM proposition_members WHERE user_id = ?`, h.user.ID); err != nil {
		t.Fatal(err)
	}

	for _, hit := range hits() {
		if hit.PropositionID != 0 {
			t.Errorf("a member of nothing was shown %s %q of proposition %d",
				hit.Kind, hit.Title, hit.PropositionID)
		}
	}
	workspaceRows := 0
	for _, row := range rows() {
		if row.PropositionID != 0 {
			t.Errorf("a member of nothing was shown activity on proposition %d", row.PropositionID)
		} else {
			workspaceRows++
		}
	}
	if workspaceRows == 0 {
		t.Error("the rows that belong to no proposition went away too")
	}
}

// Deleting a proposition files its record with no proposition, so that it
// survives the cascade that takes the proposition away. That record holds the
// title, statement, blurb and members it took with it, so carrying no
// proposition cannot be what makes a row readable by everybody.
func TestADeletedPropositionIsNotReadableByEverybody(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	boards := board.New(core.New(h.db, core.NewBus()), func() board.Defaults {
		return board.Defaults{Status: "idea", Statuses: []string{"idea", "recording"}, Columns: []string{"Research"}}
	})
	owner := core.Actor{Kind: core.KindUser, ID: h.user.ID, Name: h.user.Name}
	e, err := boards.CreateProposition(ctx, owner, "Tidal Power")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := boards.EditProposition(ctx, owner, e.EntityID,
		"Tidal Power", "The tide is a battery.", "On the estuary."); err != nil {
		t.Fatal(err)
	}
	if _, err := boards.DeleteProposition(ctx, owner, e.EntityID); err != nil {
		t.Fatal(err)
	}

	// The token's owner is now a guest who was never a member of anything.
	if _, err := h.db.ExecContext(ctx, `UPDATE users SET role = 'guest' WHERE id = ?`, h.user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.ExecContext(ctx, `DELETE FROM proposition_members WHERE user_id = ?`, h.user.ID); err != nil {
		t.Fatal(err)
	}

	w := h.do("GET", "/api/v1/activity?limit=200", h.token(auth.ScopeRead), "")
	var body struct {
		Activity []ActivityView `json:"activity"`
	}
	into(t, w, &body)
	for _, row := range body.Activity {
		if row.Entity == "proposition" || row.Entity == "card" || row.Entity == "member" {
			t.Errorf("a member of nothing was shown a %s row: %s %s", row.Entity, row.Action, row.Before)
		}
	}
	if strings.Contains(w.Body.String(), "The tide is a battery") {
		t.Error("the deleted proposition's statement is in the log for anyone to read")
	}

	// The owner still reads it, because the record is the point of keeping it.
	if _, err := h.db.ExecContext(ctx, `UPDATE users SET role = 'owner' WHERE id = ?`, h.user.ID); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.do("GET", "/api/v1/activity?limit=200", h.token(auth.ScopeRead), "").Body.String(),
		"The tide is a battery") {
		t.Error("an owner cannot read what the deletion took away")
	}
}

// A change made with a token is the token owner's, carried by the token. The
// log says which, so the activity panel can tell an agent's edit from a
// person's without the two being different people.
func TestActivityNamesWhatCarriedTheChange(t *testing.T) {
	h := newHarness(t)
	prop := h.proposition()
	token := h.token(auth.ScopeRead, auth.ScopeWrite)

	w := h.do("POST", fmt.Sprintf("/api/v1/propositions/%d/documents", prop), token, `{"name":"Research"}`)
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}

	w = h.do("GET", "/api/v1/activity?limit=200", token, "")
	var body struct {
		Activity []ActivityView `json:"activity"`
	}
	into(t, w, &body)
	var carried string
	for _, row := range body.Activity {
		if row.Entity == "document" && row.Action == "create" {
			carried = row.Via
		}
		if row.Entity == "proposition" && row.Via != "" {
			t.Errorf("a change made in the browser names a carrier: %q", row.Via)
		}
	}
	if want := TokenVia("read-write"); carried != want {
		t.Errorf("via = %q, want %q", carried, want)
	}
}
