package channel

import (
	"context"

	"github.com/davidtorcivia/theses/internal/mail"
)

// Email delivers a Note as mail through the workspace sender. Priority has no
// counterpart in mail and is dropped.
type Email struct {
	Sender mail.Sender
	To     string
}

func (e Email) Send(ctx context.Context, n Note) error {
	text := n.Body
	if n.URL != "" {
		text += "\n\n" + n.URL
	}
	return e.Sender.Send(ctx, mail.Message{To: []string{e.To}, Subject: n.Title, Text: text})
}
