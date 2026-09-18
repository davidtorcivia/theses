package channel

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/davidtorcivia/theses/internal/mail"
)

var note = Note{
	Title:    "Assigned to you: Cut the cold open",
	Body:     "Ada assigned you a card in Episode 12.",
	URL:      "https://theses.example/c/7",
	Priority: 1,
}

// record serves one request and keeps it.
type record struct {
	method string
	path   string
	header http.Header
	body   string
}

func serve(t *testing.T, status int, reply string) (*httptest.Server, *record) {
	t.Helper()
	got := &record{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.method, got.path, got.header, got.body = r.Method, r.URL.Path, r.Header.Clone(), string(b)
		w.WriteHeader(status)
		io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func TestPushover(t *testing.T) {
	srv, got := serve(t, 200, `{"status":1}`)
	old := pushoverURL
	pushoverURL = srv.URL + "/1/messages.json"
	t.Cleanup(func() { pushoverURL = old })

	if err := (Pushover{Token: "apptok", UserKey: "userkey"}).Send(context.Background(), note); err != nil {
		t.Fatal(err)
	}
	if got.method != http.MethodPost || got.path != "/1/messages.json" {
		t.Errorf("%s %s", got.method, got.path)
	}
	if ct := got.header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
		t.Errorf("content type = %q", ct)
	}
	form, err := url.ParseQuery(got.body)
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"token":     "apptok",
		"user":      "userkey",
		"title":     note.Title,
		"message":   note.Body,
		"url":       note.URL,
		"url_title": "Open",
		"priority":  "1",
	} {
		if form.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, form.Get(k), want)
		}
	}
}

func TestNtfy(t *testing.T) {
	srv, got := serve(t, 200, "{}")
	if err := (Ntfy{Server: srv.URL + "/", Topic: "theses", Token: "tok", allowPrivate: true}).Send(context.Background(), note); err != nil {
		t.Fatal(err)
	}
	if got.method != http.MethodPost || got.path != "/theses" {
		t.Errorf("%s %s", got.method, got.path)
	}
	if got.body != note.Body {
		t.Errorf("body = %q", got.body)
	}
	for k, want := range map[string]string{
		"Title":         note.Title,
		"Click":         note.URL,
		"Priority":      "4",
		"Authorization": "Bearer tok",
	} {
		if got.header.Get(k) != want {
			t.Errorf("header %s = %q, want %q", k, got.header.Get(k), want)
		}
	}
}

func TestNtfyPriorities(t *testing.T) {
	for p, want := range map[int]string{-1: "2", 0: "3", 1: "4"} {
		srv, got := serve(t, 200, "{}")
		if err := (Ntfy{Server: srv.URL, Topic: "t", allowPrivate: true}).Send(context.Background(), Note{Priority: p}); err != nil {
			t.Fatal(err)
		}
		if gotp := got.header.Get("Priority"); gotp != want {
			t.Errorf("priority = %q, want %q", gotp, want)
		}
		if got.header.Get("Authorization") != "" {
			t.Error("sent an Authorization header without a token")
		}
	}
}

func TestWebhook(t *testing.T) {
	srv, got := serve(t, 202, "")
	w := Webhook{URL: srv.URL + "/hook", Secret: "s3cret", allowPrivate: true}
	if err := w.Send(context.Background(), note); err != nil {
		t.Fatal(err)
	}
	if got.method != http.MethodPost || got.path != "/hook" {
		t.Errorf("%s %s", got.method, got.path)
	}
	if ct := got.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type = %q", ct)
	}
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write([]byte(got.body))
	if want := "sha256=" + hex.EncodeToString(mac.Sum(nil)); got.header.Get("X-Theses-Signature") != want {
		t.Errorf("signature = %q, want %q", got.header.Get("X-Theses-Signature"), want)
	}
	// Unmarshalling sent_at into a time.Time also checks it is RFC 3339.
	var payload struct {
		Event  string    `json:"event"`
		Title  string    `json:"title"`
		Body   string    `json:"body"`
		URL    string    `json:"url"`
		SentAt time.Time `json:"sent_at"`
	}
	if err := json.Unmarshal([]byte(got.body), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Event != "notification" || payload.Title != note.Title || payload.Body != note.Body || payload.URL != note.URL {
		t.Errorf("payload = %+v", payload)
	}
	if time.Since(payload.SentAt) > time.Minute {
		t.Errorf("sent_at = %s", payload.SentAt)
	}
}

func TestWebhookWithoutSecretIsUnsigned(t *testing.T) {
	srv, got := serve(t, 200, "")
	if err := (Webhook{URL: srv.URL, allowPrivate: true}).Send(context.Background(), note); err != nil {
		t.Fatal(err)
	}
	if s := got.header.Get("X-Theses-Signature"); s != "" {
		t.Errorf("signature = %q, want none", s)
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	err := (Webhook{URL: srv.URL, allowPrivate: true}).Send(context.Background(), note)
	if err == nil || !strings.Contains(err.Error(), "302") {
		t.Errorf("error = %v, want a 302", err)
	}
}

// TestRequestGoesToTheCheckedAddress points a channel at a name nothing
// resolves and hands the check the loopback address of the test server, so the
// request can only arrive if the address that was checked is the address that
// was dialed.
func TestRequestGoesToTheCheckedAddress(t *testing.T) {
	srv, got := serve(t, 200, "")
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	var asked []string
	old := resolve
	resolve = func(host string) ([]net.IP, error) {
		asked = append(asked, host)
		return []net.IP{net.IPv4(127, 0, 0, 1)}, nil
	}
	t.Cleanup(func() { resolve = old })

	w := Webhook{URL: "http://pinned.invalid:" + port + "/hook", allowPrivate: true}
	if err := w.Send(context.Background(), note); err != nil {
		t.Fatal(err)
	}
	if got.path != "/hook" {
		t.Errorf("path = %q", got.path)
	}
	if len(asked) != 1 || asked[0] != "pinned.invalid" {
		t.Errorf("resolved %v, want pinned.invalid once", asked)
	}
}

func TestNtfyRefusesPrivateAddresses(t *testing.T) {
	// The admin port of a service on the same host.
	err := (Ntfy{Server: "http://127.0.0.1:9000", Topic: "load"}).Send(context.Background(), note)
	if err == nil {
		t.Fatal("want a refusal")
	}
	if !strings.HasPrefix(err.Error(), "ntfy: ") {
		t.Errorf("error = %q", err)
	}
}

func TestWebhookRefusesPrivateAddresses(t *testing.T) {
	for _, u := range []string{
		"http://127.0.0.1:9000/hook",
		"http://[::1]/hook",
		"http://10.1.2.3/hook",
		"http://192.168.1.10/hook",
		"http://169.254.169.254/latest/meta-data/",
		"http://0.0.0.0/hook",
		"file:///etc/passwd",
		"gopher://example.com/",
	} {
		if err := (Webhook{URL: u}).Send(context.Background(), note); err == nil {
			t.Errorf("%s: want a refusal", u)
		}
	}
}

func TestNon2xxIsAnError(t *testing.T) {
	long := strings.Repeat("a", 150) + strings.Repeat("b", 150)
	srv, _ := serve(t, 503, long)
	old := pushoverURL
	pushoverURL = srv.URL
	t.Cleanup(func() { pushoverURL = old })

	for name, s := range map[string]interface {
		Send(context.Context, Note) error
	}{
		"pushover": Pushover{},
		"ntfy":     Ntfy{Server: srv.URL, Topic: "t", allowPrivate: true},
		"webhook":  Webhook{URL: srv.URL, allowPrivate: true},
	} {
		err := s.Send(context.Background(), note)
		if err == nil {
			t.Fatalf("%s: want an error", name)
		}
		msg := err.Error()
		if !strings.HasPrefix(msg, name+": ") || !strings.Contains(msg, "503") {
			t.Errorf("%s: error = %q", name, msg)
		}
		if !strings.Contains(msg, strings.Repeat("a", 150)) || strings.Contains(msg, strings.Repeat("b", 51)) {
			t.Errorf("%s: want the first 200 bytes of the body, got %q", name, msg)
		}
	}
}

// fakeSender captures what the Email channel hands to mail.
type fakeSender struct{ got mail.Message }

func (f *fakeSender) Send(_ context.Context, m mail.Message) error {
	f.got = m
	return nil
}

func TestEmail(t *testing.T) {
	f := &fakeSender{}
	if err := (Email{Sender: f, To: "ana@example.com"}).Send(context.Background(), note); err != nil {
		t.Fatal(err)
	}
	if len(f.got.To) != 1 || f.got.To[0] != "ana@example.com" {
		t.Errorf("to = %v", f.got.To)
	}
	if f.got.Subject != note.Title {
		t.Errorf("subject = %q", f.got.Subject)
	}
	if f.got.Text != note.Body+"\n\n"+note.URL {
		t.Errorf("text = %q", f.got.Text)
	}
}
