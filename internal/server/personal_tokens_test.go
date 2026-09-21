package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/board"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestPersonalKeysPermissionsAndAttribution(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	ctx := context.Background()
	owner := h.owner()
	prop := h.proposition("Member episode")
	private := h.proposition("Private episode")
	ownerKey, err := h.srv.auth.CreateAPIToken(ctx, owner.ID, "Owner secret key", []string{auth.ScopeAdmin})
	if err != nil {
		t.Fatal(err)
	}
	var ownerKeyID int64
	if err := h.db.QueryRowContext(ctx, `SELECT id FROM api_tokens WHERE name='Owner secret key'`).Scan(&ownerKeyID); err != nil {
		t.Fatal(err)
	}
	h.client = h.as("personal", "Personal user", auth.RoleEditor)
	var uid int64
	if err := h.db.QueryRowContext(ctx, `SELECT id FROM users WHERE handle='personal'`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	if _, err := h.srv.board.AddMember(ctx, owner, prop, uid); err != nil {
		t.Fatal(err)
	}
	_, page := h.get("/profile")
	if strings.Contains(page, "Owner secret key") || strings.Contains(page, ownerKey) || strings.Contains(page, `value="read write files admin"`) {
		t.Fatal("another user's key or admin control leaked")
	}
	for _, scopes := range []string{"admin", "read admin", "unknown"} {
		_, page := h.postBack("/profile/tokens", url.Values{"csrf": {h.csrf("/profile")}, "name": {"Forbidden"}, "scopes": {scopes}})
		if tokenRe.MatchString(page) {
			t.Fatalf("minted excess scope %s", scopes)
		}
	}
	res, _ := h.post("/profile/tokens", url.Values{"name": {"No CSRF"}, "scopes": {"read"}})
	if res.StatusCode != 403 {
		t.Fatal("missing CSRF allowed")
	}
	res, page = h.postBack("/profile/tokens", url.Values{"csrf": {h.csrf("/profile")}, "name": {"Personal agent"}, "scopes": {"read write"}})
	token := tokenRe.FindString(page)
	if res.StatusCode != 200 || token == "" {
		t.Fatalf("create: %d %s", res.StatusCode, page)
	}
	_, page = h.get("/profile")
	if strings.Contains(page, token) {
		t.Fatal("key shown again")
	}
	var keyID int64
	if err := h.db.QueryRowContext(ctx, `SELECT id FROM api_tokens WHERE user_id=? AND name='Personal agent'`, uid).Scan(&keyID); err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body string, want int) {
		t.Helper()
		req, _ := http.NewRequest(method, h.http.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != want {
			t.Fatalf("%s %s: %d want %d", method, path, res.StatusCode, want)
		}
	}
	request("GET", "/api/v1/me", "", 200)
	request("GET", fmt.Sprintf("/api/v1/propositions/%d", private), "", 404)
	cols, err := board.ListColumns(ctx, h.db, prop)
	if err != nil {
		t.Fatal(err)
	}
	request("POST", fmt.Sprintf("/api/v1/columns/%d/cards", cols[0].ID), `{"title":"API authored"}`, 200)
	client := sdk.NewClient(&sdk.Implementation{Name: "Personal MCP", Version: "test"}, nil)
	session, err := client.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: h.http.URL + "/mcp", HTTPClient: &http.Client{Transport: &bearer{token: token}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	out, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "create_card", Arguments: map[string]any{"column": cols[0].ID, "title": "MCP authored"}})
	if err != nil || out.IsError {
		t.Fatalf("MCP write: %+v %v", out, err)
	}
	var apiActor, mcpActor int64
	var apiVia, mcpVia string
	if err := h.db.QueryRowContext(ctx, `SELECT actor_id,via FROM activity WHERE entity='card' AND after_json LIKE '%API authored%'`).Scan(&apiActor, &apiVia); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRowContext(ctx, `SELECT actor_id,via FROM activity WHERE entity='card' AND after_json LIKE '%MCP authored%'`).Scan(&mcpActor, &mcpVia); err != nil {
		t.Fatal(err)
	}
	if apiActor != uid || mcpActor != uid || apiVia != "token:Personal agent" || !strings.HasPrefix(mcpVia, "mcp:") {
		t.Fatalf("attribution %d %s / %d %s", apiActor, apiVia, mcpActor, mcpVia)
	}
	res, _ = h.post(fmt.Sprintf("/profile/tokens/%d/revoke", ownerKeyID), url.Values{"csrf": {h.csrf("/profile")}})
	if res.StatusCode != 404 {
		t.Fatalf("cross-user revoke %d", res.StatusCode)
	}
	if _, _, err := h.srv.auth.LookupAPIToken(ctx, ownerKey); err != nil {
		t.Fatal("revoked someone else's key")
	}
	h.setRole(t, uid, auth.RoleGuest)
	request("POST", fmt.Sprintf("/api/v1/columns/%d/cards", cols[0].ID), `{"title":"Denied"}`, 403)
	request("GET", fmt.Sprintf("/api/v1/propositions/%d", prop), "", 200)
	_, page = h.postBack("/profile/tokens", url.Values{"csrf": {h.csrf("/profile")}, "name": {"Guest write"}, "scopes": {"read write"}})
	if tokenRe.MatchString(page) {
		t.Fatal("guest minted write scope")
	}
	if _, err := h.db.ExecContext(ctx, `DELETE FROM proposition_members WHERE proposition_id=? AND user_id=?`, prop, uid); err != nil {
		t.Fatal(err)
	}
	request("GET", fmt.Sprintf("/api/v1/propositions/%d", prop), "", 404)
	res, _ = h.post(fmt.Sprintf("/profile/tokens/%d/revoke", keyID), url.Values{"csrf": {h.csrf("/profile")}})
	if res.StatusCode != 303 {
		t.Fatalf("self revoke %d", res.StatusCode)
	}
	request("GET", "/api/v1/me", "", 401)
	res, _ = h.post(fmt.Sprintf("/profile/tokens/%d/revoke", keyID), url.Values{"csrf": {h.csrf("/profile")}})
	if res.StatusCode != 404 {
		t.Fatal("stale revoke must be 404")
	}
	out, err = session.CallTool(ctx, &sdk.CallToolParams{Name: "whoami"})
	if err == nil && !out.IsError {
		t.Fatal("revoked MCP key allowed")
	}
}

func TestDesktopMCPConfigUsesConfiguredOriginAndPlaceholder(t *testing.T) {
	for _, endpoint := range []string{"http://localhost:8080/mcp", "http://127.0.0.1:8080/mcp", "http://[::1]:8080/mcp"} {
		if !strings.Contains(desktopMCPConfig(endpoint), "--allow-http") {
			t.Fatal("loopback HTTP missing flag")
		}
	}
	if desktopMCPConfig("http://example.com/mcp") != "" {
		t.Fatal("remote plaintext configuration offered")
	}
	raw := desktopMCPConfig(`https://example.com/mcp`)
	var config map[string]any
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		t.Fatal(err)
	}
	server := config["mcpServers"].(map[string]any)["theses"].(map[string]any)
	args := server["args"].([]any)
	if args[2] != "https://example.com/mcp" || args[4] != "http-only" || args[6] != "auto" || args[8] != "Authorization:${THESES_AUTH_HEADER}" {
		t.Fatalf("args: %v", args)
	}
	if server["env"].(map[string]any)["THESES_AUTH_HEADER"] != "Bearer PASTE_YOUR_KEY_HERE" {
		t.Fatal("missing placeholder")
	}
}
