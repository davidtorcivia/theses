// Package mail renders and sends transactional messages.
package mail

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	netmail "net/mail"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"
)

// timeout covers the dial and the whole SMTP conversation.
const timeout = 20 * time.Second

// Message is one outbound mail. HTML is optional; when it is set the message is
// sent as multipart/alternative with Text first.
type Message struct {
	To      []string
	Subject string
	Text    string
	HTML    string
}

// Sender delivers a Message. SMTP is the implementation and the mail outbox
// calls this.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// SMTP sends through an SMTP server. TLS is "starttls", "tls" (implicit) or
// "none". User and Password are optional; when User is empty no AUTH is tried.
type SMTP struct {
	Host     string
	Port     int
	TLS      string
	User     string
	Password string
	From     string
}

func (s SMTP) Send(ctx context.Context, m Message) error {
	switch s.TLS {
	case "starttls", "tls", "none":
	default:
		// A typo must not quietly become an unencrypted session.
		return fmt.Errorf("mail: unknown tls mode %q", s.TLS)
	}
	from, err := netmail.ParseAddress(s.From)
	if err != nil {
		return fmt.Errorf("mail: from address %q: %w", s.From, err)
	}
	if len(m.To) == 0 {
		return errors.New("mail: no recipients")
	}
	// Only the parsed forms reach the envelope and the headers. ParseAddress
	// accepts a comment holding raw CR and LF and returns it in the display
	// name, and Address.String encodes that rather than writing it out.
	rcpt := make([]string, len(m.To))
	header := make([]string, len(m.To))
	for i, to := range m.To {
		a, err := netmail.ParseAddress(to)
		if err != nil {
			return fmt.Errorf("mail: recipient %q: %w", to, err)
		}
		rcpt[i], header[i] = a.Address, a.String()
	}
	body, err := render(m, from, header)
	if err != nil {
		return err
	}

	addr := net.JoinHostPort(s.Host, fmt.Sprint(s.Port))
	raw, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("mail: dial %s: %w", addr, err)
	}
	defer raw.Close()
	// The cancellation closes raw, which is never reassigned, so it reaches the
	// connection under the TLS wrapper as well.
	stop := context.AfterFunc(ctx, func() { raw.Close() })
	defer stop()
	if err := raw.SetDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("mail: deadline: %w", err)
	}
	var conn net.Conn = raw
	if s.TLS == "tls" {
		tc := tls.Client(raw, &tls.Config{ServerName: s.Host})
		if err := tc.HandshakeContext(ctx); err != nil {
			return fmt.Errorf("mail: tls handshake: %w", err)
		}
		conn = tc
	}

	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		return fmt.Errorf("mail: greeting: %w", err)
	}
	defer c.Close()
	if err := c.Hello(domain(from.Address)); err != nil {
		return fmt.Errorf("mail: ehlo: %w", err)
	}
	if s.TLS == "starttls" {
		if err := c.StartTLS(&tls.Config{ServerName: s.Host}); err != nil {
			return fmt.Errorf("mail: starttls: %w", err)
		}
	}
	if s.User != "" {
		if err := c.Auth(smtp.PlainAuth("", s.User, s.Password, s.Host)); err != nil {
			return fmt.Errorf("mail: auth: %w", err)
		}
	}
	if err := c.Mail(from.Address); err != nil {
		return fmt.Errorf("mail: mail from: %w", err)
	}
	for _, to := range rcpt {
		if err := c.Rcpt(to); err != nil {
			return fmt.Errorf("mail: rcpt to %s: %w", to, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("mail: data: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("mail: write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("mail: write: %w", err)
	}
	return c.Quit()
}

// render builds the RFC 5322 message. The DotWriter returned by Data supplies
// the carriage returns and the dot stuffing, so plain newlines are used here.
func render(m Message, from *netmail.Address, to []string) ([]byte, error) {
	var b strings.Builder
	h := textproto.MIMEHeader{}
	h.Set("From", from.String())
	h.Set("To", strings.Join(to, ", "))
	h.Set("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	h.Set("Date", time.Now().Format(time.RFC1123Z))
	h.Set("Message-ID", messageID(from.Address))
	h.Set("MIME-Version", "1.0")

	if m.HTML == "" {
		text, cte := encode(m.Text)
		h.Set("Content-Type", "text/plain; charset=utf-8")
		h.Set("Content-Transfer-Encoding", cte)
		writeHeader(&b, h)
		b.WriteString("\n")
		b.WriteString(text)
		return []byte(b.String()), nil
	}

	var parts strings.Builder
	mw := multipart.NewWriter(&parts)
	h.Set("Content-Type", "multipart/alternative; boundary="+mw.Boundary())
	for _, p := range []struct{ ctype, body string }{
		{"text/plain; charset=utf-8", m.Text},
		{"text/html; charset=utf-8", m.HTML},
	} {
		body, cte := encode(p.body)
		pw, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {p.ctype},
			"Content-Transfer-Encoding": {cte},
		})
		if err != nil {
			return nil, fmt.Errorf("mail: part: %w", err)
		}
		if _, err := pw.Write([]byte(body)); err != nil {
			return nil, fmt.Errorf("mail: part: %w", err)
		}
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("mail: parts: %w", err)
	}
	writeHeader(&b, h)
	b.WriteString("\n")
	b.WriteString(parts.String())
	return []byte(b.String()), nil
}

func writeHeader(b *strings.Builder, h textproto.MIMEHeader) {
	for _, k := range []string{"From", "To", "Subject", "Date", "Message-ID", "MIME-Version", "Content-Type", "Content-Transfer-Encoding"} {
		if v := h.Get(k); v != "" {
			b.WriteString(fold(k, v))
		}
	}
}

// fold writes one header, breaking it at a space once the line would pass 78
// bytes and continuing with CRLF and a space, because nothing else breaks a
// long value: QEncoding.Encode joins its encoded words with a plain space and
// a long ASCII subject is not encoded at all. Unfolding puts the single space
// back, so the value parses to what went in. A single word longer than the
// limit stays where it is; RFC 5322 has nowhere to break it.
func fold(name, value string) string {
	const limit = 78
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte(':')
	line := len(name) + 1
	for i, w := range strings.Split(value, " ") {
		if i > 0 && w != "" && line+1+len(w) > limit {
			b.WriteString("\r\n ")
			line = 1
		} else {
			b.WriteByte(' ')
			line++
		}
		b.WriteString(w)
		line += len(w)
	}
	b.WriteByte('\n')
	return b.String()
}

// encode returns the body and its transfer encoding. 8BITMIME is not
// negotiated, so anything that is not short line ASCII is quoted printable.
// ponytail: quoted printable always works, check c.Extension("8BITMIME") and
// emit 8bit if a server ever complains about the size.
func encode(s string) (string, string) {
	plain := true
	for _, line := range strings.Split(s, "\n") {
		if len(line) > 998 {
			plain = false
			break
		}
		for i := 0; i < len(line); i++ {
			if c := line[i]; c > 126 || (c < 32 && c != '\t') {
				plain = false
				break
			}
		}
		if !plain {
			break
		}
	}
	if plain {
		return s, "7bit"
	}
	var b strings.Builder
	w := quotedprintable.NewWriter(&b)
	w.Write([]byte(s))
	w.Close()
	return b.String(), "quoted-printable"
}

func messageID(from string) string {
	var buf [16]byte
	rand.Read(buf[:])
	return "<" + hex.EncodeToString(buf[:]) + "@" + domain(from) + ">"
}

// domain takes the host from a bare address, the form Address.Address holds.
func domain(addr string) string {
	if i := strings.LastIndex(addr, "@"); i >= 0 {
		return addr[i+1:]
	}
	return "localhost"
}
