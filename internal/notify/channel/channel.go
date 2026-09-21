// Package channel delivers one notification to one destination. Retries,
// backoff and collapsing bursts belong to the notification outbox, not here.
package channel

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/davidtorcivia/theses/internal/safehttp"
)

const timeout = 10 * time.Second

// Note is one notification. Priority is -1 low, 0 normal, 1 high. Event,
// Actor, Entity and EntityID say what happened and to what, which is what a
// webhook carries and what a person reads off the title everywhere else.
type Note struct {
	Title    string
	Body     string
	URL      string
	Priority int
	Event    string
	Actor    string
	Entity   string
	EntityID int64
	Tags     []string
}

// client is used for Pushover's fixed API endpoint.
var client = &http.Client{
	Timeout:       timeout,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

var (
	publicDestinationClient   = guardedClient()
	loopbackDestinationClient = guardedClient(safehttp.AllowLoopback())
)

// destinationClient protects every account-supplied URL with the same address
// checks used by the other outbound fetchers. Redirects stay visible to the
// sender instead of carrying an ntfy token or webhook body to another host.
func destinationClient(allowLoopback bool) *http.Client {
	if allowLoopback {
		return loopbackDestinationClient
	}
	return publicDestinationClient
}

func guardedClient(opts ...safehttp.Option) *http.Client {
	c := safehttp.Client(opts...)
	c.Timeout = timeout
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
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
