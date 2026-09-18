package safehttp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestAllowed(t *testing.T) {
	tests := []struct {
		ip          string
		wantBlocked bool
	}{
		{"93.184.216.34", false},
		{"2606:2800:220:1:248:1893:25c8:1946", false},
		{"127.0.0.1", true},
		{"::1", true},
		{"::ffff:127.0.0.1", true},
		{"10.0.0.5", true},
		{"172.16.3.1", true},
		{"192.168.1.1", true},
		{"169.254.169.254", true},
		{"::ffff:169.254.169.254", true},
		{"fd00:ec2::254", true},
		{"fe80::1", true},
		{"100.64.0.1", true},
		{"0.0.0.0", true},
		{"255.255.255.255", true},
		{"224.0.0.1", true},
		{"240.0.0.1", true},
		{"64:ff9b::a00:1", true},
		{"2002:0a00:0001::1", true},
	}
	var c config
	for _, tt := range tests {
		ip := netip.MustParseAddr(tt.ip)
		err := c.allowed(ip)
		if got := err != nil; got != tt.wantBlocked {
			t.Errorf("allowed(%s) error = %v, want blocked = %v", tt.ip, err, tt.wantBlocked)
		}
		if err != nil && !errors.Is(err, ErrBlocked) {
			t.Errorf("allowed(%s) error = %v, want ErrBlocked", tt.ip, err)
		}
	}
	loop := config{allowLoopback: true}
	for _, ip := range []string{"127.0.0.1", "::1"} {
		if err := loop.allowed(netip.MustParseAddr(ip)); err != nil {
			t.Errorf("with AllowLoopback, allowed(%s) = %v, want nil", ip, err)
		}
	}
	if err := loop.allowed(netip.MustParseAddr("10.0.0.5")); err == nil {
		t.Error("AllowLoopback also allowed a private address")
	}
}

func TestLoopbackRefusedUnlessAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello")
	}))
	defer srv.Close()

	if _, err := Client().Get(srv.URL); !errors.Is(err, ErrBlocked) {
		t.Errorf("Get(%s) error = %v, want ErrBlocked", srv.URL, err)
	}

	resp, err := Client(AllowLoopback()).Get(srv.URL)
	if err != nil {
		t.Fatalf("with AllowLoopback, Get(%s) = %v", srv.URL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "hello" {
		t.Errorf("body = %q, %v, want %q", body, err, "hello")
	}
}

// The host is checked after it is resolved, and the connection is made to the
// address that was checked, so a name pointing into a private range is refused
// whatever the name looks like.
func TestResolvedAddressIsChecked(t *testing.T) {
	tests := []struct {
		name  string
		addrs []string
	}{
		{"private only", []string{"10.0.0.5"}},
		{"public first, private second", []string{"93.184.216.34", "192.168.0.9"}},
		{"metadata", []string{"169.254.169.254"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withLookup(t, tt.addrs)
			_, err := Client().Get("http://intranet.example/")
			if !errors.Is(err, ErrBlocked) {
				t.Errorf("Get = %v, want ErrBlocked", err)
			}
		})
	}
}

func TestRedirectToPrivateRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://10.0.0.1/secret", http.StatusFound)
	}))
	defer srv.Close()
	if _, err := Client(AllowLoopback()).Get(srv.URL); !errors.Is(err, ErrBlocked) {
		t.Errorf("Get = %v, want ErrBlocked", err)
	}
}

func TestRedirectToOtherSchemeRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "ftp://example.com/file", http.StatusFound)
	}))
	defer srv.Close()
	if _, err := Client(AllowLoopback()).Get(srv.URL); !errors.Is(err, ErrScheme) {
		t.Errorf("Get = %v, want ErrScheme", err)
	}
}

func TestRedirectsCapped(t *testing.T) {
	hops := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops++
		http.Redirect(w, r, "/next", http.StatusFound)
	}))
	defer srv.Close()
	_, err := Client(AllowLoopback()).Get(srv.URL)
	if err == nil || !strings.Contains(err.Error(), "stopped after 5 redirects") {
		t.Fatalf("Get = %v, want the redirect cap", err)
	}
	if hops != maxRedirects {
		t.Errorf("server saw %d requests, want %d", hops, maxRedirects)
	}
}

func TestBodyCap(t *testing.T) {
	tests := []struct {
		name     string
		size     int
		max      int64
		wantErr  bool
		wantRead int
	}{
		{"under the cap", 100, 1024, false, 100},
		{"exactly the cap", 1024, 1024, false, 1024},
		{"over the cap", 2048, 1024, true, 1024},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(strings.Repeat("x", tt.size)))
			}))
			defer srv.Close()
			resp, err := Client(AllowLoopback(), MaxBytes(tt.max)).Get(srv.URL)
			if err != nil {
				t.Fatalf("Get = %v", err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if gotErr := errors.Is(err, ErrTooLarge); gotErr != tt.wantErr {
				t.Errorf("ReadAll error = %v, want ErrTooLarge = %v", err, tt.wantErr)
			}
			if len(body) != tt.wantRead {
				t.Errorf("read %d bytes, want %d", len(body), tt.wantRead)
			}
		})
	}
}

func TestSchemeRejected(t *testing.T) {
	for _, url := range []string{"ftp://example.com/x", "file:///etc/passwd", "gopher://example.com"} {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			t.Fatalf("NewRequest(%s) = %v", url, err)
		}
		if _, err := Client().Do(req); !errors.Is(err, ErrScheme) {
			t.Errorf("Do(%s) error = %v, want ErrScheme", url, err)
		}
	}
}

func TestUserAgentIsFixed(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.UserAgent()
	}))
	defer srv.Close()
	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", "someone else")
	resp, err := Client(AllowLoopback()).Do(req)
	if err != nil {
		t.Fatalf("Do = %v", err)
	}
	resp.Body.Close()
	if got != userAgent {
		t.Errorf("server saw User-Agent %q, want %q", got, userAgent)
	}
	if req.Header.Get("User-Agent") != "someone else" {
		t.Error("the client changed the caller's request")
	}
}

func withLookup(t *testing.T, addrs []string) {
	t.Helper()
	parsed := make([]netip.Addr, len(addrs))
	for i, a := range addrs {
		parsed[i] = netip.MustParseAddr(a)
	}
	old := lookupIP
	lookupIP = func(context.Context, string) ([]netip.Addr, error) { return parsed, nil }
	t.Cleanup(func() { lookupIP = old })
}
