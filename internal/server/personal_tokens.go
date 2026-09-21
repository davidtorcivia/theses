package server

import (
	"encoding/json"
	"net"
	"net/url"

	"github.com/davidtorcivia/theses/internal/store"
)

func tokenViews(tokens []*store.APIToken) []tokenView {
	out := make([]tokenView, 0, len(tokens))
	for _, t := range tokens {
		used := "never used"
		if t.LastUsedAt.Valid {
			used = "last used " + on(t.LastUsedAt.Int64)
		}
		expiry := "no expiry"
		if t.ExpiresAt.Valid {
			expiry = "expires " + on(t.ExpiresAt.Int64)
		}
		out = append(out, tokenView{Expires: expiry, ID: t.ID, Name: t.Name, Scopes: t.Scopes, Created: on(t.CreatedAt), Used: used})
	}
	return out
}

func desktopMCPConfig(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	args := []string{"-y", "mcp-remote@0.1.38", endpoint, "--transport", "http-only", "--protocol", "auto", "--header", "Authorization:${THESES_AUTH_HEADER}"}
	if u.Scheme == "http" {
		ip := net.ParseIP(u.Hostname())
		if u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return ""
		}
		args = append(args, "--allow-http")
	}
	config := map[string]any{"mcpServers": map[string]any{"theses": map[string]any{
		"command": "npx",
		"args":    args,
		"env":     map[string]string{"THESES_AUTH_HEADER": "Bearer PASTE_YOUR_KEY_HERE"},
	}}}
	raw, _ := json.MarshalIndent(config, "", "  ")
	return string(raw)
}
