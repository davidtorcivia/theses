package mail

import (
	"strings"
	"testing"
	"time"
)

func TestTemplates(t *testing.T) {
	for _, tt := range []struct {
		name    string
		msg     Message
		subject string
		url     string
	}{
		{
			"invite",
			Invite{To: "a@example.com", Inviter: "David", Role: "editor", URL: "https://theses.example/invite/abc", Expires: 72 * time.Hour}.Message(),
			"David invited you to THESES",
			"https://theses.example/invite/abc",
		},
		{
			"reset",
			Reset{To: "a@example.com", URL: "https://theses.example/reset/xyz", Expires: time.Hour}.Message(),
			"Reset your THESES password",
			"https://theses.example/reset/xyz",
		},
		{
			"mention",
			Mention{To: "a@example.com", Who: "David", Where: "Cold open", Excerpt: "ask @ana about the tape", URL: "https://theses.example/p/1#c2"}.Message(),
			"David mentioned you in Cold open",
			"https://theses.example/p/1#c2",
		},
		{
			"assigned",
			Assigned{To: "a@example.com", Card: "Cut the cold open", Proposition: "Episode 12", URL: "https://theses.example/c/7"}.Message(),
			"Assigned to you: Cut the cold open",
			"https://theses.example/c/7",
		},
		{
			"digest",
			Digest{To: "a@example.com", Items: []DigestItem{{"Ana moved a card", "https://theses.example/c/7"}, {"A file landed", "https://theses.example/f/3"}}}.Message(),
			"THESES daily digest",
			"https://theses.example/f/3",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.msg.Subject != tt.subject {
				t.Errorf("subject = %q, want %q", tt.msg.Subject, tt.subject)
			}
			if len(tt.msg.To) != 1 || tt.msg.To[0] != "a@example.com" {
				t.Errorf("to = %v", tt.msg.To)
			}
			if !strings.Contains(tt.msg.Text, tt.url) {
				t.Errorf("text is missing %q:\n%s", tt.url, tt.msg.Text)
			}
			if !strings.Contains(tt.msg.HTML, `<a href="`+tt.url+`">`) {
				t.Errorf("html is missing an anchor for %q:\n%s", tt.url, tt.msg.HTML)
			}
		})
	}
}

func TestHTMLBodyEscapes(t *testing.T) {
	got := Mention{Who: "<script>", Where: "a doc", Excerpt: "a & b", URL: "https://theses.example/p/1"}.Message().HTML
	if strings.Contains(got, "<script>") || !strings.Contains(got, "a &amp; b") {
		t.Errorf("html = %q", got)
	}
}

func TestExpiry(t *testing.T) {
	for d, want := range map[time.Duration]string{
		time.Hour:      "1 hour",
		24 * time.Hour: "24 hours",
		72 * time.Hour: "3 days",
	} {
		if got := expiry(d); got != want {
			t.Errorf("expiry(%s) = %q, want %q", d, got, want)
		}
	}
}
