package mail

import (
	"fmt"
	"html"
	"strings"
	"time"
)

// Invite asks someone to join the workspace.
type Invite struct {
	To      string
	Inviter string
	Role    string
	URL     string
	Expires time.Duration
}

func (i Invite) Message() Message {
	return build(i.To, fmt.Sprintf("%s invited you to THESES", i.Inviter),
		fmt.Sprintf("%s invited you to THESES as %s.", i.Inviter, i.Role),
		"",
		"Accept the invitation:",
		i.URL,
		"",
		"The link expires in "+expiry(i.Expires)+".",
	)
}

// Reset carries a one time password reset link.
type Reset struct {
	To      string
	URL     string
	Expires time.Duration
}

func (r Reset) Message() Message {
	return build(r.To, "Reset your THESES password",
		"Somebody asked to reset your THESES password.",
		"",
		"Choose a new one:",
		r.URL,
		"",
		"The link expires in "+expiry(r.Expires)+". If this was not you, ignore this mail.",
	)
}

// Mention tells someone they were named somewhere.
type Mention struct {
	To      string
	Who     string
	Where   string
	Excerpt string
	URL     string
}

func (m Mention) Message() Message {
	return build(m.To, fmt.Sprintf("%s mentioned you in %s", m.Who, m.Where),
		fmt.Sprintf("%s mentioned you in %s.", m.Who, m.Where),
		"",
		m.Excerpt,
		"",
		m.URL,
	)
}

// Assigned tells someone a card is theirs.
type Assigned struct {
	To          string
	Card        string
	Proposition string
	URL         string
}

func (a Assigned) Message() Message {
	return build(a.To, "Assigned to you: "+a.Card,
		fmt.Sprintf("You were assigned %q in %s.", a.Card, a.Proposition),
		"",
		a.URL,
	)
}

// DigestItem is one line of a Digest.
type DigestItem struct {
	Text string
	URL  string
}

// Digest is the daily roundup of everything not sent individually.
type Digest struct {
	To    string
	Items []DigestItem
}

func (d Digest) Message() Message {
	lines := []string{plural(len(d.Items), "thing") + " happened in THESES today.", ""}
	for _, it := range d.Items {
		lines = append(lines, it.Text, it.URL, "")
	}
	return build(d.To, "THESES daily digest", lines...)
}

// build assembles a Message from the subject and the plain text lines. The
// HTML alternative is the same lines as paragraphs with the URLs as anchors.
//
// Every line is flattened first. The line is what htmlBody turns into an
// anchor and what the subject header carries, so a newline inside an
// interpolated name, excerpt or card title would otherwise buy a line of the
// template's own.
func build(to, subject string, lines ...string) Message {
	for i, line := range lines {
		lines[i] = flatten.Replace(line)
	}
	text := strings.Join(lines, "\n")
	return Message{To: []string{to}, Subject: flatten.Replace(subject), Text: text, HTML: htmlBody(text)}
}

var flatten = strings.NewReplacer("\r", " ", "\n", " ")

func htmlBody(text string) string {
	var b strings.Builder
	b.WriteString("<html><body>\n")
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			continue
		}
		if isURL(line) {
			e := html.EscapeString(line)
			fmt.Fprintf(&b, "<p><a href=\"%s\">%s</a></p>\n", e, e)
			continue
		}
		fmt.Fprintf(&b, "<p>%s</p>\n", html.EscapeString(line))
	}
	b.WriteString("</body></html>\n")
	return b.String()
}

// isURL is true for the lines the templates put a bare link on, which are the
// only lines that become anchors.
func isURL(line string) bool {
	return strings.HasPrefix(line, "https://") || strings.HasPrefix(line, "http://")
}

func expiry(d time.Duration) string {
	h := int(d.Hours())
	if h >= 48 {
		return plural(h/24, "day")
	}
	return plural(h, "hour")
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}
