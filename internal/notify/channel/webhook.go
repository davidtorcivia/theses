package channel

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
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
	ctx, err := checkURL(ctx, "webhook", w.URL, w.allowPrivate)
	if err != nil {
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
