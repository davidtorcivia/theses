// Package channel delivers one notification to one destination. Retries,
// backoff and collapsing bursts belong to the notification outbox, not here.
package channel

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Note is one notification. Priority is -1 low, 0 normal, 1 high.
type Note struct {
	Title    string
	Body     string
	URL      string
	Priority int
}

// client is shared by every HTTP channel. Redirects are not followed: a
// webhook that is checked once and then redirected would walk straight past
// the address check, and a redirect off an ntfy server would carry its token.
var client = &http.Client{
	Timeout:       10 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// check consumes the response and turns anything outside 2xx into an error
// carrying the status and the first 200 bytes of the body.
func check(name string, resp *http.Response) error {
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
	return fmt.Errorf("%s: %s: %s", name, resp.Status, strings.TrimSpace(string(b)))
}
