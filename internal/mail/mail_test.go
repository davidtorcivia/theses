package mail

import (
	"context"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	netmail "net/mail"
	"net/textproto"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeSMTP speaks the smallest dialogue net/smtp needs, on loopback, with no
// TLS and no AUTH, and keeps what the client sent.
type fakeSMTP struct {
	port int
	from string
	to   []string
	data string
	done chan struct{}

	// stall holds the dialogue after DATA, stalled is closed when it does and
	// release ends the test.
	stall    bool
	stalled  chan struct{}
	released chan struct{}
}

func startFake(t *testing.T) *fakeSMTP { return newFake(t, false) }

func startStallingFake(t *testing.T) *fakeSMTP { return newFake(t, true) }

func newFake(t *testing.T, stall bool) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	f := &fakeSMTP{
		done:     make(chan struct{}),
		stall:    stall,
		stalled:  make(chan struct{}),
		released: make(chan struct{}),
	}
	t.Cleanup(func() { close(f.released) })
	f.port, _ = strconv.Atoi(port)
	go func() {
		defer close(f.done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		tp := textproto.NewConn(conn)
		tp.PrintfLine("220 fake ESMTP")
		for {
			line, err := tp.ReadLine()
			if err != nil {
				return
			}
			cmd := strings.ToUpper(line)
			switch {
			case strings.HasPrefix(cmd, "EHLO"):
				tp.PrintfLine("250-fake")
				tp.PrintfLine("250 HELP")
			case strings.HasPrefix(cmd, "MAIL FROM:"):
				f.from = angled(line)
				tp.PrintfLine("250 OK")
			case strings.HasPrefix(cmd, "RCPT TO:"):
				f.to = append(f.to, angled(line))
				tp.PrintfLine("250 OK")
			case cmd == "DATA":
				tp.PrintfLine("354 go ahead")
				b, err := tp.ReadDotBytes()
				if err != nil {
					return
				}
				f.data = string(b)
				if f.stall {
					close(f.stalled)
					<-f.released
					return
				}
				tp.PrintfLine("250 OK")
			case cmd == "QUIT":
				tp.PrintfLine("221 bye")
				return
			default:
				tp.PrintfLine("250 OK")
			}
		}
	}()
	return f
}

func angled(line string) string {
	i, j := strings.Index(line, "<"), strings.LastIndex(line, ">")
	if i < 0 || j < i {
		return ""
	}
	return line[i+1 : j]
}

func (f *fakeSMTP) sender() SMTP {
	return SMTP{Host: "127.0.0.1", Port: f.port, TLS: "none", From: "THESES <theses@example.org>"}
}

func TestSMTPSendPlain(t *testing.T) {
	f := startFake(t)
	m := Message{To: []string{"alice@example.com"}, Subject: "Hello", Text: "Line one\nLine two\n"}
	if err := f.sender().Send(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	<-f.done

	if f.from != "theses@example.org" {
		t.Errorf("envelope from = %q", f.from)
	}
	if len(f.to) != 1 || f.to[0] != "alice@example.com" {
		t.Errorf("envelope to = %v", f.to)
	}
	msg, err := netmail.ReadMessage(strings.NewReader(f.data))
	if err != nil {
		t.Fatalf("parse: %v (%q)", err, f.data)
	}
	for k, want := range map[string]string{
		"From":                      `"THESES" <theses@example.org>`,
		"To":                        "<alice@example.com>",
		"Subject":                   "Hello",
		"MIME-Version":              "1.0",
		"Content-Type":              "text/plain; charset=utf-8",
		"Content-Transfer-Encoding": "7bit",
	} {
		if got := msg.Header.Get(k); got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}
	if _, err := msg.Header.Date(); err != nil {
		t.Errorf("Date: %v", err)
	}
	if id := msg.Header.Get("Message-ID"); !strings.HasSuffix(id, "@example.org>") || len(id) != 32+len("<@example.org>") {
		t.Errorf("Message-ID = %q", id)
	}
	// ReadDotBytes has already turned the wire CRLF back into LF.
	body, _ := io.ReadAll(msg.Body)
	if string(body) != "Line one\nLine two\n" {
		t.Errorf("body = %q", body)
	}
}

func TestSMTPSendMultipart(t *testing.T) {
	f := startFake(t)
	m := Message{
		To:      []string{"alice@example.com", "Bob <bob@example.com>"},
		Subject: "Grüße",
		Text:    "plain part",
		HTML:    "<html><body><p>html part</p></body></html>",
	}
	if err := f.sender().Send(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	<-f.done

	if len(f.to) != 2 || f.to[0] != "alice@example.com" || f.to[1] != "bob@example.com" {
		t.Errorf("envelope to = %v", f.to)
	}
	msg, err := netmail.ReadMessage(strings.NewReader(f.data))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	subject, err := (&mime.WordDecoder{}).DecodeHeader(msg.Header.Get("Subject"))
	if err != nil || subject != "Grüße" {
		t.Errorf("Subject = %q (%v)", subject, err)
	}
	ctype, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || ctype != "multipart/alternative" {
		t.Fatalf("Content-Type = %q (%v)", ctype, err)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	want := []string{"text/plain; charset=utf-8", "text/html; charset=utf-8"}
	for i, wantType := range want {
		p, err := mr.NextPart()
		if err != nil {
			t.Fatalf("part %d: %v", i, err)
		}
		if got := p.Header.Get("Content-Type"); got != wantType {
			t.Errorf("part %d type = %q, want %q", i, got, wantType)
		}
		b, _ := io.ReadAll(p)
		if !strings.Contains(string(b), []string{"plain part", "html part"}[i]) {
			t.Errorf("part %d body = %q", i, b)
		}
	}
	if _, err := mr.NextPart(); err != io.EOF {
		t.Errorf("want two parts, got more")
	}
}

func TestSMTPRejectsBadAddresses(t *testing.T) {
	s := SMTP{Host: "127.0.0.1", Port: 1, TLS: "none", From: "theses@example.org"}
	bad := s
	bad.TLS = "ssl"
	if err := bad.Send(context.Background(), Message{To: []string{"a@example.com"}, Text: "x"}); err == nil {
		t.Error("unknown tls mode: want error")
	}
	for _, m := range []Message{
		{Subject: "no recipients", Text: "x"},
		{To: []string{"alice@example.com\r\nRCPT TO:<eve@example.com>"}, Subject: "injection", Text: "x"},
	} {
		if err := s.Send(context.Background(), m); err == nil {
			t.Errorf("%s: want error", m.Subject)
		}
	}
}

// TestAddressCommentCannotInjectHeaders uses the RFC 5322 comment that
// netmail.ParseAddress accepts with raw CR and LF inside it and returns in the
// display name.
func TestAddressCommentCannotInjectHeaders(t *testing.T) {
	f := startFake(t)
	s := f.sender()
	s.From = "theses@example.org (y\r\nReply-To: eve@example.com)"
	m := Message{
		To:      []string{"alice@example.com (x\r\nBcc: eve@example.com\r\n\r\nbody)"},
		Subject: "Hello\r\nBcc: eve@example.com",
		Text:    "the real body\n",
	}
	if err := s.Send(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	<-f.done

	if len(f.to) != 1 || f.to[0] != "alice@example.com" {
		t.Errorf("envelope to = %v", f.to)
	}
	for _, line := range strings.Split(f.data, "\n") {
		if strings.HasPrefix(line, "Bcc:") || strings.HasPrefix(line, "Reply-To:") {
			t.Errorf("injected header survived:\n%s", f.data)
		}
	}
	msg, err := netmail.ReadMessage(strings.NewReader(f.data))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if b, r := msg.Header.Get("Bcc"), msg.Header.Get("Reply-To"); b != "" || r != "" {
		t.Errorf("Bcc = %q, Reply-To = %q", b, r)
	}
	// The comment is inert inside the encoded word it was folded into.
	if got, _ := (&mime.WordDecoder{}).DecodeHeader(msg.Header.Get("Subject")); got != m.Subject {
		t.Errorf("subject = %q, want %q", got, m.Subject)
	}
	body, _ := io.ReadAll(msg.Body)
	if string(body) != "the real body\n" {
		t.Errorf("body = %q", body)
	}
	if to, err := netmail.ParseAddress(msg.Header.Get("To")); err != nil || to.Address != "alice@example.com" {
		t.Errorf("To header = %q (%v)", msg.Header.Get("To"), err)
	}
}

func TestLongSubjectIsFolded(t *testing.T) {
	for _, subject := range []string{
		strings.Repeat("Пример ", 40),
		strings.Repeat("a long ascii subject ", 40),
	} {
		subject = strings.TrimSuffix(subject, " ")
		f := startFake(t)
		m := Message{To: []string{"alice@example.com"}, Subject: subject, Text: "x\n"}
		if err := f.sender().Send(context.Background(), m); err != nil {
			t.Fatal(err)
		}
		<-f.done

		for _, line := range strings.Split(f.data, "\n") {
			if len(line) > 998 {
				t.Errorf("line of %d bytes: %q", len(line), line[:60])
			}
		}
		msg, err := netmail.ReadMessage(strings.NewReader(f.data))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		got, err := (&mime.WordDecoder{}).DecodeHeader(msg.Header.Get("Subject"))
		if err != nil || got != subject {
			t.Errorf("subject round trip = %q (%v)", got, err)
		}
	}
}

func TestSendReturnsOnCancel(t *testing.T) {
	f := startStallingFake(t)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		errc <- f.sender().Send(ctx, Message{To: []string{"alice@example.com"}, Subject: "s", Text: "x\n"})
	}()
	<-f.stalled
	cancel()
	select {
	case err := <-errc:
		if err == nil {
			t.Error("want an error after the cancel")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Send did not return after the context was cancelled")
	}
}

func TestEncode(t *testing.T) {
	if got, cte := encode("plain ascii\n"); cte != "7bit" || got != "plain ascii\n" {
		t.Errorf("ascii = %q %q", got, cte)
	}
	got, cte := encode("Grüße\n")
	if cte != "quoted-printable" {
		t.Fatalf("cte = %q", cte)
	}
	back, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(got)))
	if err != nil || string(back) != "Grüße\r\n" {
		t.Errorf("round trip = %q (%v)", back, err)
	}
	if _, cte := encode(strings.Repeat("a", 999)); cte != "quoted-printable" {
		t.Errorf("long line cte = %q", cte)
	}
}
