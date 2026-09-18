// Package channel delivers one notification to one destination. Retries,
// backoff and collapsing bursts belong to the notification outbox, not here.
package channel

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
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

// resolve is the name lookup, replaced in tests.
var resolve = net.LookupIP

// pinKey carries the address checkURL approved down to the dialler.
type pinKey struct{}

// client is shared by every HTTP channel. Redirects are not followed: a
// destination that passed the address check and then redirects would walk
// straight past it, and a redirect off an ntfy server would carry its token.
// The dialler replaces the host with the address the check approved, so a
// second answer to the same name cannot move the request afterwards.
var client = &http.Client{
	Timeout:       timeout,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if ip, ok := ctx.Value(pinKey{}).(net.IP); ok {
				if _, port, err := net.SplitHostPort(addr); err == nil {
					addr = net.JoinHostPort(ip.String(), port)
				}
			}
			return (&net.Dialer{Timeout: timeout}).DialContext(ctx, network, addr)
		},
	},
}

// checkURL refuses any scheme but http and https and, unless a test allows the
// private ranges, any host resolving to an address inside the deployment. It
// returns a context pinning the request to the address it checked, which is
// what closes the gap between this lookup and the dial.
// ponytail: only the first address is dialed, so a dual stack host has no
// fallback if that one is unreachable, and replace with safehttp.Client once
// merged.
func checkURL(ctx context.Context, name, rawURL string, allowPrivate bool) (context.Context, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%s: refusing scheme %q", name, u.Scheme)
	}
	host := u.Hostname()
	ips, err := resolve(host)
	if err != nil {
		return nil, fmt.Errorf("%s: resolve %q: %w", name, host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("%s: %q has no addresses", name, host)
	}
	if !allowPrivate {
		for _, ip := range ips {
			if !public(ip) {
				return nil, fmt.Errorf("%s: %q resolves to %s, which is not a public address", name, host, ip)
			}
		}
	}
	return context.WithValue(ctx, pinKey{}, ips[0]), nil
}

func public(ip net.IP) bool {
	return !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsUnspecified() &&
		!ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsMulticast()
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
