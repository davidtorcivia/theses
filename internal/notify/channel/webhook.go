package channel

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// event names what the payload reports.
// ponytail: one name for every notification, the outbox passes the typed event
// through once core emits it.
const event = "notification"

// Webhook posts a JSON notification to an owner configured URL and signs it
// with Secret when one is set.
type Webhook struct {
	URL    string
	Secret string

	// allowPrivate lets the tests point at an httptest server on loopback.
	allowPrivate bool
}

func (w Webhook) Send(ctx context.Context, n Note) error {
	if err := w.checkURL(); err != nil {
		return err
	}
	body, err := json.Marshal(struct {
		Event  string    `json:"event"`
		Title  string    `json:"title"`
		Body   string    `json:"body"`
		URL    string    `json:"url"`
		SentAt time.Time `json:"sent_at"`
	}{event, n.Title, n.Body, n.URL, time.Now().UTC()})
	if err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if w.Secret != "" {
		mac := hmac.New(sha256.New, []byte(w.Secret))
		mac.Write(body)
		req.Header.Set("X-Theses-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	return check("webhook", resp)
}

// checkURL refuses anything but http and https and anything that resolves into
// a range the workspace should not be able to reach from the inside.
// Redirects are refused by the shared client, so this runs on the only address
// that is dialled.
// ponytail: replace with safehttp.Client once merged, which also closes the gap
// between this lookup and the dial.
func (w Webhook) checkURL() error {
	u, err := url.Parse(w.URL)
	if err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("webhook: refusing scheme %q", u.Scheme)
	}
	if w.allowPrivate {
		return nil
	}
	host := u.Hostname()
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("webhook: resolve %q: %w", host, err)
	}
	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
			ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
			return fmt.Errorf("webhook: %q resolves to %s, which is not a public address", host, ip)
		}
	}
	return nil
}
