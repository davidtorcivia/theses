package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/davidtorcivia/theses/internal/auth"
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
	api := New(db, a, set, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return &harness{T: t, db: db, auth: a, set: set, handler: api.Handler(), user: user}
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
		if s["key"] == "mail.password" {
			secretSeen = true
			if s["secret"] != true || s["set"] != true {
				t.Errorf("mail.password = %v", s)
			}
			if _, ok := s["value"]; ok {
				t.Errorf("mail.password carries a value: %v", s)
			}
		}
		if s["key"] == "workspace.name" && s["value"] != "We All Fall Down" {
			t.Errorf("workspace.name = %v", s)
		}
	}
	if !secretSeen {
		t.Error("mail.password is missing from the listing")
	}

	w = h.do("PUT", "/api/v1/settings/workspace.name", admin, `{"value":"Debt Machine"}`)
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	if got := decode(t, w)["value"]; got != "Debt Machine" {
		t.Errorf("value = %v", got)
	}
	if got := settings.Get[string](h.set, "workspace.name"); got != "Debt Machine" {
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
		{"not a number", "/api/v1/settings/signin.session_days", `{"value":"soon"}`, http.StatusBadRequest},
		{"out of range", "/api/v1/settings/signin.session_days", `{"value":4000}`, http.StatusBadRequest},
		{"not a choice", "/api/v1/settings/workspace.release_day", `{"value":"Caturday"}`, http.StatusBadRequest},
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
		if err := store.InsertActivity(ctx, h.db, "user", "1", "card", name, "create", "", `{"a":1}`); err != nil {
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
