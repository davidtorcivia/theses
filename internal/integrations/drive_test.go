package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/davidtorcivia/theses/internal/safehttp"
)

// loopback is the outbound client the tests use: safehttp's, with the one
// exception that lets it reach an httptest server. Production never sets it,
// so the address check is the same one in both.
func loopback() *http.Client {
	c := safehttp.Client(safehttp.AllowLoopback(), safehttp.MaxBytes(maxImport))
	c.Timeout = 0
	return c
}

// fakeGoogle is Google's token endpoint and Drive, as much of them as this
// package uses. The recorded fields are what the assertions read back.
type fakeGoogle struct {
	*httptest.Server

	mu        sync.Mutex
	grants    []url.Values // every token request, in order
	token     string       // the access token the Drive half will accept
	refresh   string       // what a refresh answers with, empty for the default
	tokenCode int
	tokenBody string
	files     map[string]string // id to contents
	query     string            // the q of the last listing
	bearer    string            // the Authorization of the last Drive call
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	t.Helper()
	g := &fakeGoogle{token: "access-1", files: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		g.mu.Lock()
		g.grants = append(g.grants, r.PostForm)
		code, body := g.tokenCode, g.tokenBody
		refresh := g.refresh
		g.mu.Unlock()
		if code != 0 {
			w.WriteHeader(code)
			io.WriteString(w, body)
			return
		}
		out := map[string]any{"access_token": "access-1", "expires_in": 3600, "token_type": "Bearer"}
		if r.PostFormValue("grant_type") == "authorization_code" {
			out["refresh_token"] = "refresh-1"
		} else if refresh != "" {
			out["refresh_token"] = refresh
		}
		json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("GET /drive/v3/files", func(w http.ResponseWriter, r *http.Request) {
		if !g.authorised(w, r) {
			return
		}
		g.mu.Lock()
		g.query = r.URL.Query().Get("q")
		g.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"files": []map[string]string{
			{"id": "f1", "name": "Interview.wav", "mimeType": "audio/wav", "size": "12"},
			{"id": "d1", "name": "Recordings", "mimeType": folderMime},
			{"id": "n1", "name": "Notes", "mimeType": nativePrefix + "document"},
		}})
	})
	mux.HandleFunc("GET /drive/v3/files/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !g.authorised(w, r) {
			return
		}
		id := r.PathValue("id")
		g.mu.Lock()
		body, ok := g.files[id]
		g.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":{"message":"File not found."}}`)
			return
		}
		if r.URL.Query().Get("alt") == "media" {
			io.WriteString(w, body)
			return
		}
		mime := "audio/wav"
		if id == "n1" {
			mime = nativePrefix + "document"
		}
		json.NewEncoder(w).Encode(map[string]string{
			"id": id, "name": id + ".wav", "mimeType": mime,
			"size": strconv.Itoa(len(body)),
		})
	})
	g.Server = httptest.NewServer(mux)
	t.Cleanup(g.Close)
	return g
}

func (g *fakeGoogle) authorised(w http.ResponseWriter, r *http.Request) bool {
	g.mu.Lock()
	g.bearer = r.Header.Get("Authorization")
	want := "Bearer " + g.token
	g.mu.Unlock()
	if r.Header.Get("Authorization") != want {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"Invalid Credentials"}}`)
		return false
	}
	return true
}

func (g *fakeGoogle) drive(t *testing.T, s Settings) *Drive {
	t.Helper()
	d := &Drive{
		HTTP:     loopback(),
		Auth:     g.URL + "/auth",
		TokenURL: g.URL + "/token",
		API:      g.URL + "/drive/v3",
		Now:      time.Now,
	}
	if err := d.Configure(s); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDriveAuthURLAsksForARefreshToken(t *testing.T) {
	g := newFakeGoogle(t)
	d := g.drive(t, Settings{"client_id": "id", "client_secret": "secret"})
	u, err := url.Parse(d.AuthURL("state-1", "https://example.com/settings/integrations/drive/callback"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"client_id":     "id",
		"response_type": "code",
		"access_type":   "offline",
		"prompt":        "consent",
		"state":         "state-1",
		"scope":         DriveScope,
		"redirect_uri":  "https://example.com/settings/integrations/drive/callback",
	}
	for k, v := range want {
		if got := u.Query().Get(k); got != v {
			t.Errorf("%s is %q, want %q", k, got, v)
		}
	}
	if d.Connected() {
		t.Error("Drive is connected before the code has been exchanged")
	}
	if !d.Configured() {
		t.Error("Drive with a client id and secret is not configured")
	}
}

func TestDriveExchangeAndRefresh(t *testing.T) {
	g := newFakeGoogle(t)
	d := g.drive(t, Settings{"client_id": "id", "client_secret": "secret"})

	tok, err := d.Exchange(context.Background(), "the-code", "https://example.com/back")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Refresh != "refresh-1" || tok.Access != "access-1" {
		t.Fatalf("exchange returned %+v", tok)
	}
	if !d.Connected() {
		t.Fatal("Drive is not connected after the exchange")
	}
	if got := g.grants[0].Get("grant_type"); got != "authorization_code" {
		t.Fatalf("grant_type is %q", got)
	}
	if got := g.grants[0].Get("code"); got != "the-code" {
		t.Fatalf("code is %q", got)
	}

	// A token that has not run out is reused rather than refreshed.
	if _, err := d.List(context.Background(), "", ""); err != nil {
		t.Fatal(err)
	}
	if len(g.grants) != 1 {
		t.Fatalf("a live token was refreshed anyway: %d grants", len(g.grants))
	}

	// One that has runs the refresh, keeps the refresh token Google left out
	// of the answer, and saves what it got.
	var saved []Token
	d.Save = func(_ context.Context, tok Token) error { saved = append(saved, tok); return nil }
	d.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if _, err := d.List(context.Background(), "", ""); err != nil {
		t.Fatal(err)
	}
	if len(g.grants) != 2 || g.grants[1].Get("grant_type") != "refresh_token" {
		t.Fatalf("the expired token was not refreshed: %v", g.grants)
	}
	if g.grants[1].Get("refresh_token") != "refresh-1" {
		t.Fatalf("the refresh used %q", g.grants[1].Get("refresh_token"))
	}
	if len(saved) != 1 || saved[0].Refresh != "refresh-1" {
		t.Fatalf("the refreshed token was stored as %+v", saved)
	}
}

func TestDriveRefreshRaceRunsOnceAtATime(t *testing.T) {
	g := newFakeGoogle(t)
	d := g.drive(t, Settings{"client_id": "id", "client_secret": "secret",
		"token": `{"access_token":"stale","refresh_token":"refresh-1","expires_at":1}`})

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := d.List(context.Background(), "", ""); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	// Whichever refresh went first leaves a live token behind, so the ones
	// waiting on the lock find it and do not exchange again.
	if len(g.grants) != 1 {
		t.Fatalf("four concurrent calls made %d token requests, want 1", len(g.grants))
	}
}

func TestDriveRevokedRefreshTokenAsksForAReconnection(t *testing.T) {
	g := newFakeGoogle(t)
	g.tokenCode, g.tokenBody = http.StatusBadRequest,
		`{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`
	d := g.drive(t, Settings{"client_id": "id", "client_secret": "secret",
		"token": `{"refresh_token":"refresh-1"}`})

	_, err := d.List(context.Background(), "", "")
	if !errors.Is(err, ErrReconnect) {
		t.Fatalf("a revoked refresh token gave %v", err)
	}
	if strings.Contains(err.Error(), "refresh-1") {
		t.Fatal("the refusal carries the refresh token")
	}
}

func TestDriveListQuotesTheSearchTerm(t *testing.T) {
	g := newFakeGoogle(t)
	d := g.drive(t, Settings{"client_id": "id", "client_secret": "secret",
		"token": `{"access_token":"access-1","refresh_token":"refresh-1","expires_at":99999999999}`})

	cases := []struct {
		name, folder, query, want string
	}{
		{"the root folder", "", "", "'root' in parents and trashed = false"},
		{"a named folder", "d1", "", "'d1' in parents and trashed = false"},
		{"a search", "", "wav", "name contains 'wav' and trashed = false"},
		{"a term that would close the string", "", `a' or name contains 'b`,
			`name contains 'a\' or name contains \'b' and trashed = false`},
		{"a term ending in a backslash", "", `a\`, `name contains 'a\\' and trashed = false`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := d.List(context.Background(), c.folder, c.query); err != nil {
				t.Fatal(err)
			}
			if g.query != c.want {
				t.Errorf("q was %q, want %q", g.query, c.want)
			}
		})
	}
}

func TestDriveListLeavesOutWhatCannotBeImported(t *testing.T) {
	g := newFakeGoogle(t)
	d := g.drive(t, Settings{"client_id": "id", "client_secret": "secret",
		"token": `{"access_token":"access-1","refresh_token":"refresh-1","expires_at":99999999999}`})
	rows, err := d.List(context.Background(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("listed %+v, want the file and the folder", rows)
	}
	for _, r := range rows {
		if strings.HasPrefix(r.Mime, nativePrefix) && !r.Folder {
			t.Errorf("a native Google file was listed: %+v", r)
		}
	}
	if !rows[1].Folder || rows[1].Name != "Recordings" {
		t.Errorf("the folder came back as %+v", rows[1])
	}
}

func TestDriveStatRefusesWhatCannotBeImported(t *testing.T) {
	g := newFakeGoogle(t)
	g.files["n1"] = "x"
	g.files["gone"] = ""
	d := g.drive(t, Settings{"client_id": "id", "client_secret": "secret",
		"token": `{"access_token":"access-1","refresh_token":"refresh-1","expires_at":99999999999}`})

	if _, err := d.Stat(context.Background(), "n1"); err == nil ||
		!strings.Contains(err.Error(), "export it") {
		t.Fatalf("a native Google file gave %v", err)
	}
	if _, err := d.Stat(context.Background(), "gone"); err == nil ||
		!strings.Contains(err.Error(), "empty") {
		t.Fatalf("an empty file gave %v", err)
	}
	if _, err := d.Stat(context.Background(), "missing"); err == nil ||
		!strings.Contains(err.Error(), "File not found") {
		t.Fatalf("a missing file gave %v", err)
	}
}

func TestDriveOpenReadsTheBytes(t *testing.T) {
	g := newFakeGoogle(t)
	g.files["f1"] = "twelve bytes"
	d := g.drive(t, Settings{"client_id": "id", "client_secret": "secret",
		"token": `{"access_token":"access-1","refresh_token":"refresh-1","expires_at":99999999999}`})

	f, err := d.Stat(context.Background(), "f1")
	if err != nil {
		t.Fatal(err)
	}
	if f.Size != 12 {
		t.Fatalf("size is %d", f.Size)
	}
	body, err := d.Open(context.Background(), "f1")
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	read, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(read) != "twelve bytes" {
		t.Fatalf("read %q", read)
	}
	if !strings.HasPrefix(g.bearer, "Bearer ") || strings.Contains(g.bearer, "refresh") {
		t.Fatalf("the download was authorised with %q", g.bearer)
	}
}

func TestDriveWithoutATokenSaysSo(t *testing.T) {
	g := newFakeGoogle(t)
	d := g.drive(t, Settings{"client_id": "id", "client_secret": "secret"})
	if _, err := d.List(context.Background(), "", ""); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("a listing with no token gave %v", err)
	}
	if err := d.Configure(Settings{"client_id": "id", "token": "not json"}); err == nil {
		t.Fatal("a token that will not parse was accepted")
	}
}
