package channel

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// pushoverURL is the message API endpoint, replaced in tests.
var pushoverURL = "https://api.pushover.net/1/messages.json"

// Pushover posts to the Pushover message API. Token is the application token,
// which the workspace may hold for everyone, UserKey is the account's own.
type Pushover struct {
	Token   string
	UserKey string
}

func (p Pushover) Send(ctx context.Context, n Note) error {
	form := url.Values{
		"token":    {p.Token},
		"user":     {p.UserKey},
		"title":    {n.Title},
		"message":  {n.Body},
		"priority": {strconv.Itoa(n.Priority)},
	}
	if n.URL != "" {
		form.Set("url", n.URL)
		form.Set("url_title", "Open")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, pushoverURL, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("pushover: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("pushover: %w", err)
	}
	return check("pushover", resp)
}
