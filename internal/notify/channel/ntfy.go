package channel

import (
	"context"
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"strings"
)

// Ntfy posts to a topic on an ntfy server. Server defaults to ntfy.sh, Token
// is the optional access token.
type Ntfy struct {
	Server string
	Topic  string
	Token  string
}

func (t Ntfy) Send(ctx context.Context, n Note) error {
	server := t.Server
	if server == "" {
		server = "https://ntfy.sh"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(server, "/")+"/"+t.Topic, strings.NewReader(n.Body))
	if err != nil {
		return fmt.Errorf("ntfy: %w", err)
	}
	// Headers carry no raw UTF-8, and ntfy decodes RFC 2047.
	req.Header.Set("Title", mime.QEncoding.Encode("utf-8", n.Title))
	req.Header.Set("Priority", strconv.Itoa(n.Priority+3))
	if n.URL != "" {
		req.Header.Set("Click", n.URL)
	}
	if t.Token != "" {
		req.Header.Set("Authorization", "Bearer "+t.Token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("ntfy: %w", err)
	}
	return check("ntfy", resp)
}
