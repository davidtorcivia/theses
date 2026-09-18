package settings

import (
	"context"
	"strings"
	"testing"

	"github.com/davidtorcivia/theses/internal/store"
)

var key = []byte("a secret key of at least thirty-two bytes")

func open(t *testing.T, db *store.DB, k []byte) *Settings {
	t.Helper()
	s, err := Open(context.Background(), db, k)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestDefaultsUntilSet(t *testing.T) {
	s := open(t, store.OpenTemp(t), key)
	if got := Get[string](s, "workspace.name"); got != "We All Fall Down" {
		t.Errorf("workspace.name = %q", got)
	}
	if got := Get[int](s, "signin.session_days"); got != 30 {
		t.Errorf("signin.session_days = %d", got)
	}
	if got := Get[[]string](s, "defaults.columns"); len(got) != 6 {
		t.Errorf("defaults.columns = %v", got)
	}
	if s.IsSet("workspace.name") {
		t.Error("an unset key reports as set")
	}
}

func TestSetRoundTripsAndRecordsActivity(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTemp(t)
	s := open(t, db, key)

	if err := s.Set(ctx, "workspace.name", []string{"Debt Machine"}, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "signin.session_days", []string{"7"}, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "defaults.columns", []string{"Research", "", "Edit"}, 0); err != nil {
		t.Fatal(err)
	}
	if got := Get[string](s, "workspace.name"); got != "Debt Machine" {
		t.Errorf("workspace.name = %q", got)
	}
	if got := Get[int](s, "signin.session_days"); got != 7 {
		t.Errorf("signin.session_days = %d", got)
	}
	if got := Get[[]string](s, "defaults.columns"); len(got) != 2 || got[1] != "Edit" {
		t.Errorf("defaults.columns = %v", got)
	}

	// A fresh Settings over the same database sees the same values.
	if got := Get[string](open(t, db, key), "workspace.name"); got != "Debt Machine" {
		t.Errorf("after reload workspace.name = %q", got)
	}

	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM activity WHERE entity = 'setting' AND action = 'set'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("recorded %d activity rows, want 3", n)
	}
}

func TestSecretRoundTripAndNeverStoredPlain(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTemp(t)
	s := open(t, db, key)

	const password = "hunter2-but-longer"
	if err := s.Set(ctx, "mail.password", []string{password}, 0); err != nil {
		t.Fatal(err)
	}
	got, err := s.Secret(ctx, "mail.password")
	if err != nil {
		t.Fatal(err)
	}
	if got != password {
		t.Errorf("secret round trip gave %q", got)
	}
	if !s.IsSet("mail.password") {
		t.Error("a stored secret should report as set")
	}

	var stored, after string
	if err := db.QueryRowContext(ctx, `SELECT value_json FROM settings WHERE key = 'mail.password'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, password) {
		t.Error("the secret is stored in the clear")
	}
	if err := db.QueryRowContext(ctx,
		`SELECT after_json FROM activity WHERE entity_id = 'mail.password'`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != `{"set":true}` || strings.Contains(after, password) {
		t.Errorf("activity recorded %q", after)
	}
}

func TestSecretFailsCleanlyUnderAnotherKey(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTemp(t)
	if err := open(t, db, key).Set(ctx, "mail.password", []string{"hunter2-but-longer"}, 0); err != nil {
		t.Fatal(err)
	}

	other := open(t, db, []byte("a different key of at least thirty-two bytes"))
	got, err := other.Secret(ctx, "mail.password")
	if err == nil {
		t.Fatalf("decrypted %q under the wrong key", got)
	}
	if !strings.Contains(err.Error(), "THESES_SECRET_KEY") {
		t.Errorf("error %q does not say which key is wrong", err)
	}
	if !other.IsSet("mail.password") {
		t.Error("the key should still report as set so the page can offer to replace it")
	}
}

func TestSetValidates(t *testing.T) {
	ctx := context.Background()
	s := open(t, store.OpenTemp(t), key)
	cases := []struct{ name, key, value, want string }{
		{"unknown key", "workspace.colour", "red", "unknown key"},
		{"not a number", "signin.session_days", "a fortnight", "not a number"},
		{"not a choice", "workspace.release_day", "Caturday", "not one of"},
		{"empty list", "defaults.columns", "", "at least one entry"},
		{"blank secret", "mail.password", "", "cannot be blanked"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := s.Set(ctx, tc.key, []string{tc.value}, 0)
			if err == nil {
				t.Fatalf("%s=%q accepted", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestIntegerSettingsAreBounded(t *testing.T) {
	ctx := context.Background()
	s := open(t, store.OpenTemp(t), key)

	// A session length past a year overflows the duration that builds the cookie
	// expiry, which wrote an already expired session on every sign-in.
	if err := s.Set(ctx, "signin.session_days", []string{"200000"}, 0); err == nil {
		t.Fatal("a session length of 200000 days was accepted")
	} else if !strings.Contains(err.Error(), "outside 1 to 365") {
		t.Errorf("error %q does not name the bounds", err)
	}
	if err := s.Set(ctx, "signin.session_days", []string{"0"}, 0); err == nil {
		t.Error("a session length of zero days was accepted")
	}
	if err := s.Set(ctx, "signin.session_days", []string{"365"}, 0); err != nil {
		t.Fatalf("365 days was refused: %v", err)
	}
	if got := Get[int](s, "signin.session_days"); got != 365 {
		t.Errorf("signin.session_days = %d", got)
	}

	for _, c := range []struct{ key, value string }{
		{"mail.port", "0"},
		{"mail.port", "70000"},
		{"signin.handle_min_length", "1"},
		{"signin.handle_min_length", "40"},
		{"workspace.episode_start", "-1"},
	} {
		if err := s.Set(ctx, c.key, []string{c.value}, 0); err == nil {
			t.Errorf("%s=%s was accepted", c.key, c.value)
		}
	}
	if err := s.Set(ctx, "mail.port", []string{"465"}, 0); err != nil {
		t.Errorf("a real port was refused: %v", err)
	}
}

func TestSetAsRecordsTheActorThatIsNotAPerson(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTemp(t)
	s := open(t, db, key)

	if err := s.SetAs(ctx, "workspace.name", []string{"Debt Machine"},
		Actor{Kind: "token", ID: "7", UserID: 0}); err != nil {
		t.Fatal(err)
	}
	var kind, id string
	if err := db.QueryRowContext(ctx,
		`SELECT actor_kind, actor_id FROM activity WHERE entity_id = 'workspace.name'`).
		Scan(&kind, &id); err != nil {
		t.Fatal(err)
	}
	if kind != "token" || id != "7" {
		t.Errorf("activity actor = %s %s, want token 7", kind, id)
	}
}
