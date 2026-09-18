// Package safehttp builds the http.Client used for outbound fetches, such as
// link metadata. Anything a person can paste is a URL an attacker can choose,
// so the client refuses private, loopback, link-local and cloud metadata
// addresses, checks every hop of a redirect, and caps what it will read.
package safehttp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"time"
)

var (
	// ErrBlocked means the host resolved to an address the fetcher is not
	// allowed to reach.
	ErrBlocked = errors.New("safehttp: address not allowed")
	// ErrScheme means the URL was not http or https.
	ErrScheme = errors.New("safehttp: scheme not allowed")
	// ErrTooLarge means the response body went past the configured cap.
	ErrTooLarge = errors.New("safehttp: response body too large")
)

const (
	defaultMaxBytes = 4 << 20
	maxRedirects    = 5
	userAgent       = "theses-linkbot/1.0 (+https://github.com/davidtorcivia/theses)"
)

// lookupIP is a variable so tests can resolve names without a network. It is
// never replaced outside tests.
var lookupIP = func(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

type config struct {
	maxBytes      int64
	allowLoopback bool
}

// An Option changes one setting of the client returned by Client.
type Option func(*config)

// MaxBytes caps how much of a response body the client will hand back before
// it returns ErrTooLarge. The default is 4 MB.
func MaxBytes(n int64) Option { return func(c *config) { c.maxBytes = n } }

// AllowLoopback permits 127.0.0.0/8 and ::1. Tests need it to reach an
// httptest server; production must never set it.
func AllowLoopback() Option { return func(c *config) { c.allowLoopback = true } }

// Client returns an http.Client that only reaches public addresses.
func Client(opts ...Option) *http.Client {
	cfg := config{maxBytes: defaultMaxBytes}
	for _, opt := range opts {
		opt(&cfg)
	}
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &guard{
			cfg: cfg,
			base: &http.Transport{
				// An environment proxy would carry requests to private
				// addresses without ever passing through dial.
				Proxy:                 nil,
				DialContext:           cfg.dial,
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          10,
				IdleConnTimeout:       30 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 15 * time.Second,
				ExpectContinueTimeout: time.Second,
			},
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("safehttp: stopped after %d redirects", maxRedirects)
			}
			return checkScheme(req.URL.Scheme)
		},
	}
}

type guard struct {
	cfg  config
	base http.RoundTripper
}

func (g *guard) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := checkScheme(req.URL.Scheme); err != nil {
		return nil, err
	}
	out := req.Clone(req.Context())
	out.Header.Set("User-Agent", userAgent)
	resp, err := g.base.RoundTrip(out)
	if err != nil {
		return nil, err
	}
	resp.Body = &cappedBody{rc: resp.Body, max: g.cfg.maxBytes}
	return resp, nil
}

func checkScheme(scheme string) error {
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("%w: %q", ErrScheme, scheme)
	}
	return nil
}

// dial resolves the host itself and checks every address it gets, then
// connects to one of those addresses rather than to the name, so a second
// answer from the resolver cannot take the place of the one that was checked.
func (c config) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var addrs []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{ip}
	} else if addrs, err = lookupIP(ctx, host); err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("safehttp: no addresses for %q", host)
	}
	for _, ip := range addrs {
		if err := c.allowed(ip); err != nil {
			return nil, err
		}
	}
	dialer := net.Dialer{Timeout: 10 * time.Second}
	var firstErr error
	for _, ip := range addrs {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return nil, firstErr
}

// blocked holds the ranges the netip.Addr predicates do not already cover:
// "this network", carrier grade NAT, IETF protocol assignments, benchmarking,
// reserved and broadcast, NAT64 and 6to4, the last two because they embed an
// IPv4 address that may itself be private.
var blocked = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("255.255.255.255/32"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("100::/64"),
}

func (c config) allowed(ip netip.Addr) error {
	ip = ip.Unmap()
	if !ip.IsValid() {
		return fmt.Errorf("%w: invalid address", ErrBlocked)
	}
	if ip.IsLoopback() {
		if c.allowLoopback {
			return nil
		}
		return fmt.Errorf("%w: %s is loopback", ErrBlocked, ip)
	}
	if ip.IsUnspecified() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return fmt.Errorf("%w: %s", ErrBlocked, ip)
	}
	for _, p := range blocked {
		if p.Contains(ip) {
			return fmt.Errorf("%w: %s", ErrBlocked, ip)
		}
	}
	return nil
}

type cappedBody struct {
	rc   io.ReadCloser
	max  int64
	read int64
}

func (b *cappedBody) Read(p []byte) (int, error) {
	if b.read > b.max {
		return 0, ErrTooLarge
	}
	// One byte of room past the cap, so a body of exactly max bytes still
	// ends in EOF rather than an error.
	if room := b.max - b.read + 1; int64(len(p)) > room {
		p = p[:room]
	}
	n, err := b.rc.Read(p)
	b.read += int64(n)
	if b.read > b.max {
		return n - int(b.read-b.max), ErrTooLarge
	}
	return n, err
}

func (b *cappedBody) Close() error { return b.rc.Close() }
