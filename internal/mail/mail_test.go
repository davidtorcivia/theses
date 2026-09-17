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
)

// fakeSMTP speaks the smallest dialogue net/smtp needs, on loopback, with no
// TLS and no AUTH, and keeps what the client sent.
type fakeSMTP struct {
	port int
	from string
	to   []string
	data string
	done chan struct{}
}

func startFake(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	f := &fakeSMTP{done: make(chan struct{})}
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
		"From":                      "THESES <theses@example.org>",
		"To":                        "alice@example.com",
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

	if len(f.to) != 2 || f.to[1] != "bob@example.com" {
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
