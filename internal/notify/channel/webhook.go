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

// Webhook posts a JSON notification to an owner configured URL and signs it
// with Secret when one is set.
type Webhook struct {
	URL    string
	Secret string
}

func (w Webhook) Send(ctx context.Context, n Note) error {
	event := n.Event
	if event == "" {
		event = "notification"
	}
	body, err := json.Marshal(struct {
		Event    string    `json:"event"`
		Title    string    `json:"title"`
		Body     string    `json:"body"`
		URL      string    `json:"url"`
		Actor    string    `json:"actor,omitempty"`
		Entity   string    `json:"entity,omitempty"`
		EntityID int64     `json:"entity_id,omitempty"`
		SentAt   time.Time `json:"sent_at"`
	}{event, n.Title, n.Body, n.URL, n.Actor, n.Entity, n.EntityID, time.Now().UTC()})
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
	resp, err := destination.Do(req)
	if err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	return check("webhook", resp)
}
