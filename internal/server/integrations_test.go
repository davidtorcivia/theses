package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/safehttp"
)

// fakeGoogle is the token endpoint and the one Drive call the test button
// makes. It records the code exchange so the assertions can read it back.
func fakeGoogle(t *testing.T) (*httptest.Server, *url.Values) {
	t.Helper()
	var last url.Values
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		last = r.PostForm
		if r.PostFormValue("code") == "bad" {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":"invalid_grant","error_description":"Bad Request"}`)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "access-1", "refresh_token": "refresh-1", "expires_in": 3600,
		})
	})
	mux.HandleFunc("GET /drive/v3/files", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer access-1" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":{"message":"Invalid Credentials"}}`)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"files": []map[string]string{
			{"id": "d1", "name": "Recordings", "mimeType": "application/vnd.google-apps.folder"},
		}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &last
}

// pointAtFakes sends both integrations at test servers, through the same
// safehttp client the real ones use with loopback allowed.
func (h *harness) pointAtFakes(google, transistor string) {
	c := safehttp.Client(safehttp.AllowLoopback())
	c.Timeout = 0
	h.srv.drive.HTTP, h.srv.transistor.HTTP = c, c
	h.srv.drive.Auth = google + "/auth"
	h.srv.drive.TokenURL = google + "/token"
	h.srv.drive.API = google + "/drive/v3"
	h.srv.transistor.API = transistor
}

func (h *harness) saveSecret(key, value string) {
	h.Helper()
	if err := h.srv.settings.Set(context.Background(), key, []string{value}, 1); err != nil {
		h.Fatal(err)
	}
}

func TestDriveConnectNeedsAClientFirst(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	res, body := h.post("/settings/integrations/drive/connect", url.Values{"csrf": {h.csrf("/settings")}})
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("connect with nothing configured gave %d", res.StatusCode)
	}
	if !strings.Contains(body, "client id and secret first") {
		t.Fatal("the page did not say what is missing")
	}
}

func TestDriveOAuthFlow(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	google, exchange := fakeGoogle(t)
	h.pointAtFakes(google.URL, "")
	h.saveSecret("integrations.drive.client_id", "the-client")
	h.saveSecret("integrations.drive.client_secret", "the-secret")

	res, _ := h.post("/settings/integrations/drive/connect", url.Values{"csrf": {h.csrf("/settings")}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("connect gave %d", res.StatusCode)
	}
	sent, err := url.Parse(res.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if sent.Host != google.Listener.Addr().String() {
		t.Fatalf("connect went to %s", sent.Host)
	}
	state := sent.Query().Get("state")
	if state == "" {
		t.Fatal("no state was sent")
	}
	var cookie string
	for _, c := range res.Cookies() {
		if c.Name == oauthCookie {
			cookie = c.Value
		}
	}
	if cookie != state {
		t.Fatalf("the state cookie is %q and the state parameter %q", cookie, state)
	}
	if sent.Query().Get("redirect_uri") != h.srv.cfg.BaseURL+driveCallback {
		t.Fatalf("the redirect was %q", sent.Query().Get("redirect_uri"))
	}

	// A code that comes back with the wrong state is not exchanged at all.
	res, body := h.get(driveCallback + "?code=the-code&state=somebody-elses")
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("a wrong state gave %d", res.StatusCode)
	}
	if !strings.Contains(body, "press Connect again") {
		t.Fatal("the page did not say what happened")
	}
	if h.srv.settings.IsSet("integrations.drive.token") {
		t.Fatal("a token was stored for a callback with the wrong state")
	}
	if *exchange != nil {
		t.Fatal("the code was exchanged although the state did not match")
	}

	// The cookie is spent by that attempt, so the real one has to start again.
	res, _ = h.post("/settings/integrations/drive/connect", url.Values{"csrf": {h.csrf("/settings")}})
	sent, _ = url.Parse(res.Header.Get("Location"))
	state = sent.Query().Get("state")

	res, _ = h.get(driveCallback + "?code=the-code&state=" + url.QueryEscape(state))
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("the callback gave %d", res.StatusCode)
	}
	if got := exchange.Get("grant_type"); got != "authorization_code" {
		t.Fatalf("the exchange used %q", got)
	}
	if got := exchange.Get("redirect_uri"); got != h.srv.cfg.BaseURL+driveCallback {
		t.Fatalf("the exchange sent redirect_uri %q", got)
	}

	// The token is stored as a secret, so it is sealed and the page never
	// shows it.
	stored, err := h.srv.settings.Secret(context.Background(), "integrations.drive.token")
	if err != nil {
		t.Fatal(err)
	}
	var token struct {
		Refresh string `json:"refresh_token"`
	}
	if err := json.Unmarshal([]byte(stored), &token); err != nil {
		t.Fatal(err)
	}
	if token.Refresh != "refresh-1" {
		t.Fatalf("the stored token is %q", stored)
	}
	_, page := h.get("/settings")
	for _, secret := range []string{"refresh-1", "access-1", "the-secret", "the-client"} {
		if strings.Contains(page, secret) {
			t.Fatalf("the settings page shows %q", secret)
		}
	}
	if !strings.Contains(page, "connected") {
		t.Fatal("the settings page does not say Drive is connected")
	}

	// The test button lists the root folder.
	res, page = h.post("/settings/test/drive", url.Values{"csrf": {h.csrf("/settings")}})
	if res.StatusCode != http.StatusOK || !strings.Contains(page, "1 of them folders") {
		t.Fatalf("the test button gave %d: %s", res.StatusCode, firstNotice(page))
	}

	// Disconnecting throws the token away and keeps the client.
	res, _ = h.post("/settings/integrations/drive/disconnect", url.Values{"csrf": {h.csrf("/settings")}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("disconnect gave %d", res.StatusCode)
	}
	if h.srv.settings.IsSet("integrations.drive.token") {
		t.Fatal("the token survived a disconnect")
	}
	if !h.srv.settings.IsSet("integrations.drive.client_id") {
		t.Fatal("disconnecting threw the client id away too")
	}
}

func TestDriveCallbackReportsWhatGoogleRefused(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	google, _ := fakeGoogle(t)
	h.pointAtFakes(google.URL, "")
	h.saveSecret("integrations.drive.client_id", "the-client")
	h.saveSecret("integrations.drive.client_secret", "the-secret")

	res, _ := h.post("/settings/integrations/drive/connect", url.Values{"csrf": {h.csrf("/settings")}})
	sent, _ := url.Parse(res.Header.Get("Location"))
	res, body := h.get(driveCallback + "?code=bad&state=" + url.QueryEscape(sent.Query().Get("state")))
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("a refused exchange gave %d", res.StatusCode)
	}
	if !strings.Contains(body, "connect it again") {
		t.Fatalf("the page said %q", firstNotice(body))
	}
	if strings.Contains(body, "the-secret") {
		t.Fatal("the page printed the client secret")
	}
}

func TestTransistorTestButton(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "the-key" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"errors":[{"title":"Unauthorized"}]}`)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"id": "1", "attributes": map[string]string{"title": "Workspace"}}})
	}))
	defer fake.Close()

	h := newHarness(t)
	h.setupOwner()
	h.pointAtFakes("http://127.0.0.1:1", fake.URL)
	h.saveSecret("integrations.transistor.api_key", "the-key")
	h.saveSecret("integrations.transistor.show_id", "1")

	res, body := h.post("/settings/test/transistor", url.Values{"csrf": {h.csrf("/settings")}})
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "Connected to Workspace.") {
		t.Fatalf("the test button gave %d: %s", res.StatusCode, firstNotice(body))
	}

	res, _ = h.post("/settings/integrations/transistor/disconnect", url.Values{"csrf": {h.csrf("/settings")}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("disconnect gave %d", res.StatusCode)
	}
	if h.srv.settings.IsSet("integrations.transistor.api_key") {
		t.Fatal("the key survived a disconnect")
	}
}

// Every route in this section is the owner's.
func TestIntegrationRoutesAreOwnerOnly(t *testing.T) {
	const password = "a long enough password"
	h := newHarness(t)
	h.setupOwner()
	if err := h.srv.settings.Set(context.Background(), "signin.require_totp",
		[]string{"owners"}, 1); err != nil {
		t.Fatal(err)
	}
	h.withoutAuthenticator("mara", auth.RoleEditor, password)
	token := h.csrf("/settings")
	h.signOut()
	if res, _ := h.signIn("mara", password, ""); res.Header.Get("Location") != "/" {
		t.Fatalf("the editor could not sign in: %s", res.Header.Get("Location"))
	}

	for _, path := range []string{
		"/settings/integrations/drive/connect",
		"/settings/integrations/drive/disconnect",
		"/settings/integrations/transistor/disconnect",
		"/settings/test/drive",
		"/settings/test/transistor",
	} {
		if res, _ := h.post(path, url.Values{"csrf": {token}}); res.StatusCode != http.StatusForbidden {
			t.Errorf("%s gave an editor %d, want 403", path, res.StatusCode)
		}
	}
	if res, _ := h.get(driveCallback + "?code=x&state=y"); res.StatusCode != http.StatusForbidden {
		t.Errorf("the callback gave an editor %d, want 403", res.StatusCode)
	}
}

// firstNotice is the message a settings page printed, for a failure line that
// says what went wrong rather than dumping the page.
func firstNotice(page string) string {
	i := strings.Index(page, `class="notice`)
	if i < 0 {
		return "no notice on the page"
	}
	rest := page[i:]
	start := strings.Index(rest, ">")
	end := strings.Index(rest, "</p>")
	if start < 0 || end < 0 || end < start {
		return "no notice on the page"
	}
	return rest[start+1 : end]
}

// Google sends the owner back with an error rather than a code when they say
// no on the consent screen. It is still their browser and their state, so it
// is a line on the page rather than a refusal of the request.
func TestDriveCallbackWhenTheOwnerSaysNo(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	google, exchange := fakeGoogle(t)
	h.pointAtFakes(google.URL, "")
	h.saveSecret("integrations.drive.client_id", "the-client")
	h.saveSecret("integrations.drive.client_secret", "the-secret")

	res, _ := h.post("/settings/integrations/drive/connect", url.Values{"csrf": {h.csrf("/settings")}})
	sent, _ := url.Parse(res.Header.Get("Location"))
	state := url.QueryEscape(sent.Query().Get("state"))

	res, body := h.get(driveCallback + "?error=access_denied&state=" + state)
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("a refused consent gave %d", res.StatusCode)
	}
	if !strings.Contains(body, "Google refused the connection: access_denied") {
		t.Fatalf("the page said %q", firstNotice(body))
	}
	if *exchange != nil {
		t.Fatal("something was exchanged although there was no code")
	}
	if h.srv.settings.IsSet("integrations.drive.token") {
		t.Fatal("a token was stored")
	}
	// The cookie was spent, so pressing Connect again is what is left.
	res, body = h.get(driveCallback + "?code=the-code&state=" + state)
	if res.StatusCode != http.StatusUnprocessableEntity ||
		!strings.Contains(body, "press Connect again") {
		t.Fatalf("the spent state gave %d: %q", res.StatusCode, firstNotice(body))
	}
}

// And a callback with no code and no error is not a connection either.
func TestDriveCallbackWithNoCode(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	google, _ := fakeGoogle(t)
	h.pointAtFakes(google.URL, "")
	h.saveSecret("integrations.drive.client_id", "the-client")
	h.saveSecret("integrations.drive.client_secret", "the-secret")

	res, _ := h.post("/settings/integrations/drive/connect", url.Values{"csrf": {h.csrf("/settings")}})
	sent, _ := url.Parse(res.Header.Get("Location"))
	res, body := h.get(driveCallback + "?state=" + url.QueryEscape(sent.Query().Get("state")))
	if res.StatusCode != http.StatusUnprocessableEntity ||
		!strings.Contains(body, "no authorization code") {
		t.Fatalf("gave %d: %q", res.StatusCode, firstNotice(body))
	}
}
