package channel

import (
	"context"
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"strings"
)

// DefaultNtfyServer is where a topic lives when the workspace names no server
// of its own.
const DefaultNtfyServer = "https://ntfy.sh"

// Ntfy posts to a topic on an ntfy server. Server defaults to ntfy.sh, Token
// is the optional access token.
type Ntfy struct {
	Server string
	Topic  string
	Token  string

	// allowPrivate lets the tests point at an httptest server on loopback.
	allowPrivate bool
}

func (t Ntfy) Send(ctx context.Context, n Note) error {
	server := t.Server
	if server == "" {
		server = DefaultNtfyServer
	}
	// An account types this server, so it goes through the same check as a
	// webhook: nothing else stops it naming an admin port on the box.
	endpoint := strings.TrimSuffix(server, "/") + "/" + t.Topic
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(n.Body))
	if err != nil {
		return fmt.Errorf("ntfy: %w", err)
	}
	// Headers carry no raw UTF-8, and ntfy decodes RFC 2047.
	req.Header.Set("Title", mime.QEncoding.Encode("utf-8", n.Title))
	req.Header.Set("Priority", strconv.Itoa(n.Priority+3))
	if n.URL != "" {
		req.Header.Set("Click", n.URL)
	}
	if len(n.Tags) > 0 {
		req.Header.Set("Tags", strings.Join(n.Tags, ","))
	}
	if t.Token != "" {
		req.Header.Set("Authorization", "Bearer "+t.Token)
	}
	resp, err := destinationClient(t.allowPrivate).Do(req)
	if err != nil {
		return fmt.Errorf("ntfy: %w", err)
	}
	return check("ntfy", resp)
}
