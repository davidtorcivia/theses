package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/core"
)

// send is one JSON request to the browser's own links and files routes, with
// the CSRF token in the header a form would have carried in a field.
func (h *harness) send(method, path, token, body string) (*http.Response, string) {
	h.Helper()
	req, err := http.NewRequest(method, h.http.URL+path, strings.NewReader(body))
	if err != nil {
		h.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set(auth.CSRFHeader, token)
	}
	res, err := h.client.Do(req)
	if err != nil {
		h.Fatal(err)
	}
	out, _ := io.ReadAll(res.Body)
	res.Body.Close()
	return res, string(out)
}

// proposition makes one through the command layer, which is the only way there
// is, and returns its id.
func (h *harness) proposition(title string) int64 {
	h.Helper()
	var id int64
	if err := h.db.QueryRowContext(context.Background(),
		`SELECT id FROM users ORDER BY id LIMIT 1`).Scan(&id); err != nil {
		h.Fatal(err)
	}
	e, err := h.srv.board.CreateProposition(context.Background(),
		core.Actor{Kind: core.KindUser, ID: id, Name: "Ada Lovelace"}, title)
	if err != nil {
		h.Fatal(err)
	}
	return e.EntityID
}

func TestAppRoutesNeedASessionAndACSRFToken(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	proposition := strconv.FormatInt(h.proposition("Tidal Power"), 10)
	token := h.csrf("/profile")

	// A GET is safe and needs no token.
	res, body := h.send("GET", "/app/links?proposition="+proposition, "", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /app/links: %d %s", res.StatusCode, body)
	}

	for _, tc := range []struct {
		name, method, path, token, body string
		want                            int
	}{
		{
			name: "a write with no token is refused", method: "POST", path: "/app/links",
			body: `{"proposition":` + proposition + `,"url":"https://example.com"}`,
			want: http.StatusForbidden,
		},
		{
			name: "a delete with no token is refused too", method: "DELETE", path: "/app/links/1",
			want: http.StatusForbidden,
		},
		{
			name: "a patch with no token is refused too", method: "PATCH", path: "/app/files/1",
			body: `{"name":"x.md","folder":"Documents"}`, want: http.StatusForbidden,
		},
		{
			name: "a write with a token gets as far as the command", method: "POST", path: "/app/links",
			token: token, body: `{"proposition":` + proposition + `,"url":"not a url"}`,
			want: http.StatusUnprocessableEntity,
		},
		{
			name: "a proposition that is not there is not found", method: "GET",
			path: "/app/files?proposition=9999", want: http.StatusNotFound,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, body := h.send(tc.method, tc.path, tc.token, tc.body)
			if res.StatusCode != tc.want {
				t.Fatalf("%s %s: %d, want %d: %s", tc.method, tc.path, res.StatusCode, tc.want, body)
			}
			if !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") &&
				tc.want != http.StatusForbidden {
				t.Fatalf("answer is not JSON: %s", res.Header.Get("Content-Type"))
			}
		})
	}
}

// Signed out, the browser's routes send you to sign in rather than answering.
func TestAppRoutesNeedAUser(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	h.signOut()
	res, _ := h.send("GET", "/app/links?proposition=1", "", "")
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/login" {
		t.Fatalf("signed out: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
}

// The browser PUTs to the bucket directly, so the bucket's origin is in the
// three directives it reaches it through and in no others. A workspace with no
// storage configured keeps the policy as it was.
func TestStorageOriginsAreInTheCSP(t *testing.T) {
	h := newHarness(t)
	res, _ := h.get("/healthz")
	if got := res.Header.Get("Content-Security-Policy"); got != contentSecurityPolicy {
		t.Fatalf("with no storage configured the policy is\n%s", got)
	}

	ctx := context.Background()
	for key, value := range map[string]string{
		"storage.primary.endpoint":           "https://s3.example.com",
		"storage.primary.public_base_url":    "https://cdn.example.com/files",
		"storage.recordings.endpoint":        "https://s3.example.com",
		"storage.recordings.public_base_url": "",
	} {
		if err := h.srv.settings.Set(ctx, key, []string{value}, 0); err != nil {
			t.Fatal(err)
		}
	}

	res, _ = h.get("/healthz")
	policy := res.Header.Get("Content-Security-Policy")
	for _, want := range []string{
		"img-src 'self' data: https://s3.example.com https://cdn.example.com;",
		"connect-src 'self' https://s3.example.com https://cdn.example.com;",
		"media-src 'self' https://s3.example.com https://cdn.example.com;",
	} {
		if !strings.Contains(policy, want) {
			t.Errorf("policy has no %q:\n%s", want, policy)
		}
	}
	if !strings.Contains(policy, "script-src 'self';") {
		t.Errorf("the script source was widened:\n%s", policy)
	}
	if strings.Count(policy, "https://s3.example.com") != 3 {
		t.Errorf("the endpoint is named more than three times:\n%s", policy)
	}
}

func TestOriginOf(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://s3.example.com", "https://s3.example.com"},
		{"https://s3.example.com/bucket/path", "https://s3.example.com"},
		{"http://localhost:9000", "http://localhost:9000"},
		{"", ""},
		{"   ", ""},
		{"not a url", ""},
		{"javascript:alert(1)", ""},
		{"file:///etc", ""},
	} {
		if got := originOf(tc.in); got != tc.want {
			t.Errorf("originOf(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The CORS check hands the browser a presigned PUT for a probe object under a
// key derived from who asked, not from anything the browser sent.
func TestCORSCheckIsOwnerOnly(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	token := h.csrf("/settings")

	res, body := h.post("/settings/test/cors", url.Values{
		"csrf": {token}, "prefix": {"storage.other"},
	})
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("an unknown bucket: %d %s", res.StatusCode, body)
	}

	// With no keys saved the bucket cannot be built, and the page is told so
	// rather than being handed a URL that goes nowhere.
	res, body = h.post("/settings/test/cors", url.Values{
		"csrf": {token}, "prefix": {"storage.primary"},
	})
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("with no keys: %d %s", res.StatusCode, body)
	}

	ctx := context.Background()
	for key, value := range map[string]string{
		"storage.primary.provider":   "backblaze",
		"storage.primary.endpoint":   "https://s3.example.com",
		"storage.primary.region":     "us-west-004",
		"storage.primary.bucket":     "theses",
		"storage.primary.access_key": "key",
		"storage.primary.secret_key": "secret",
	} {
		if err := h.srv.settings.Set(ctx, key, []string{value}, 0); err != nil {
			t.Fatal(err)
		}
	}
	res, body = h.post("/settings/test/cors", url.Values{
		"csrf": {token}, "prefix": {"storage.primary"},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("with keys: %d %s", res.StatusCode, body)
	}
	var out struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	if !strings.Contains(out.URL, "/theses/probe/cors-") || !strings.Contains(out.URL, "X-Amz-Signature") {
		t.Fatalf("the probe URL is not a presigned PUT under probe/: %s", out.URL)
	}
	if out.Headers["Content-Type"] != "text/plain" || out.Body == "" {
		t.Fatalf("the headers the browser has to send are missing: %+v", out.Headers)
	}
}
