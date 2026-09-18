package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/mcp"
	"github.com/davidtorcivia/theses/internal/store"
)

// apiToken makes a token for a person who exists, since the machine surfaces
// are reached before an owner has been through /setup as well as after.
func (h *harness) apiToken(scopes ...string) string {
	h.Helper()
	ctx := context.Background()
	id, err := store.CreateUser(ctx, h.db, &store.User{
		Handle: "nora", Email: "nora@example.com", Name: "Nora Vance",
		Initials: "NV", Colour: "#b45", Role: "owner", PasswordHash: "x",
	})
	if err != nil {
		h.Fatal(err)
	}
	clear, err := h.srv.auth.CreateAPIToken(ctx, id, "agent", scopes)
	if err != nil {
		h.Fatal(err)
	}
	return clear
}

func TestAPIIsOutsideTheSetupGateAndTheCSRFCheck(t *testing.T) {
	h := newHarness(t)

	// No owner exists yet, so a browser would be sent to /setup.
	res, body := h.get("/api/v1/me")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d: %s", res.StatusCode, body)
	}
	if !strings.Contains(body, `"error"`) {
		t.Errorf("body = %s", body)
	}
	if res.Header.Get("Content-Security-Policy") != contentSecurityPolicy {
		t.Error("no content security policy on an API response")
	}

	req, err := http.NewRequest("GET", h.http.URL+"/api/v1/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.apiToken(auth.ScopeRead))
	res, err = h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("me with a token: %d", res.StatusCode)
	}
	if c := res.Cookies(); len(c) != 0 {
		t.Errorf("the API set cookies: %v", c)
	}
}

func TestMCPSpeaksToATokenAndRefusesWithout(t *testing.T) {
	h := newHarness(t)
	token := h.apiToken(auth.ScopeRead)

	res, err := h.client.Post(h.http.URL+"/mcp", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("mcp without a token: %d", res.StatusCode)
	}

	ctx := context.Background()
	client := sdk.NewClient(&sdk.Implementation{Name: "research agent", Version: "test"}, nil)
	cs, err := client.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint:   h.http.URL + "/mcp",
		HTTPClient: &http.Client{Transport: bearer{token: token}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	out, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "whoami"})
	if err != nil {
		t.Fatal(err)
	}
	if out.IsError {
		t.Fatalf("whoami: %+v", out.Content)
	}
	if got := out.StructuredContent.(map[string]any)["user"].(map[string]any)["handle"]; got != "nora" {
		t.Errorf("whoami returned %v", out.StructuredContent)
	}
}

// bearer is the round tripper that puts the token on every MCP request, the way
// a client configured with one would.
type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// initialize is the first message of an MCP session, sent by hand so the test
// can control the Host header and the size of the body.
const initialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":` +
	`{"protocolVersion":"2025-03-26","capabilities":{},` +
	`"clientInfo":{"name":"research agent","version":"test"}}}`

func (h *harness) postMCP(token, host, body string) *http.Response {
	h.Helper()
	req, err := http.NewRequest("POST", h.http.URL+"/mcp", strings.NewReader(body))
	if err != nil {
		h.Fatal(err)
	}
	req.Host = host
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	res, err := h.client.Do(req)
	if err != nil {
		h.Fatal(err)
	}
	res.Body.Close()
	return res
}

// Caddy runs on the same machine and proxies to loopback, so every real request
// arrives over loopback carrying the public host name.
func TestMCPAnswersLoopbackWithAPublicHost(t *testing.T) {
	h := newHarness(t)
	token := h.apiToken(auth.ScopeRead)

	if res := h.postMCP(token, "theses.example.com", initialize); res.StatusCode != http.StatusOK {
		t.Errorf("initialize behind a proxy: %d", res.StatusCode)
	}
	big := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search","arguments":{"query":"` +
		strings.Repeat("x", mcp.MaxBodyBytes) + `"}}}`
	if res := h.postMCP(token, "theses.example.com", big); res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("an oversized body: %d", res.StatusCode)
	}
}
