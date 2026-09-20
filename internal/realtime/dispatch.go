package realtime

import (
	"context"
	"errors"

	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/docs"
)

// commands is the whole client surface. Every one of them is a board command
// called with the session's actor, so a tab can do exactly what the API and
// MCP can do and no more.
var commands = map[string]func(context.Context, *board.Service, core.Actor, args) (core.Event, error){
	"proposition.create": func(ctx context.Context, b *board.Service, a core.Actor, v args) (core.Event, error) {
		return b.CreateProposition(ctx, a, v.Title)
	},
	"proposition.edit": func(ctx context.Context, b *board.Service, a core.Actor, v args) (core.Event, error) {
		return b.EditProposition(ctx, a, v.Proposition, v.Title, v.Statement, v.Blurb)
	},
	"proposition.status": func(ctx context.Context, b *board.Service, a core.Actor, v args) (core.Event, error) {
		return b.SetStatus(ctx, a, v.Proposition, v.Status)
	},
	"proposition.schedule": func(ctx context.Context, b *board.Service, a core.Actor, v args) (core.Event, error) {
		return b.Schedule(ctx, a, v.Proposition, v.Episode, v.Target)
	},
	"proposition.move": func(ctx context.Context, b *board.Service, a core.Actor, v args) (core.Event, error) {
		return b.MoveProposition(ctx, a, v.Proposition, v.After)
	},
	"proposition.archive": func(ctx context.Context, b *board.Service, a core.Actor, v args) (core.Event, error) {
		return b.ArchiveProposition(ctx, a, v.Proposition)
	},
	"proposition.restore": func(ctx context.Context, b *board.Service, a core.Actor, v args) (core.Event, error) {
		return b.RestoreProposition(ctx, a, v.Proposition)
	},
	"proposition.delete": func(ctx context.Context, b *board.Service, a core.Actor, v args) (core.Event, error) {
		return b.DeleteProposition(ctx, a, v.Proposition)
	},
	"member.add": func(ctx context.Context, b *board.Service, a core.Actor, v args) (core.Event, error) {
		return b.AddMember(ctx, a, v.Proposition, v.User)
	},
	"member.remove": func(ctx context.Context, b *board.Service, a core.Actor, v args) (core.Event, error) {
		return b.RemoveMember(ctx, a, v.Proposition, v.User)
	},
	"column.create": func(ctx context.Context, b *board.Service, a core.Actor, v args) (core.Event, error) {
		return b.CreateColumn(ctx, a, v.Proposition, v.Title)
	},
	"column.rename": func(ctx context.Context, b *board.Service, a core.Actor, v args) (core.Event, error) {
		return b.RenameColumn(ctx, a, v.Column, v.Title)
	},
	"column.move": func(ctx context.Context, b *board.Service, a core.Actor, v args) (core.Event, error) {
		return b.MoveColumn(ctx, a, v.Column, v.After)
	},
	"column.delete": func(ctx context.Context, b *board.Service, a core.Actor, v args) (core.Event, error) {
		return b.DeleteColumn(ctx, a, v.Column)
	},
	"card.create": func(ctx context.Context, b *board.Service, a core.Actor, v args) (core.Event, error) {
		return b.CreateCard(ctx, a, v.Column, v.Title, v.Assignees)
	},
	"card.title": func(ctx context.Context, b *board.Service, a core.Actor, v args) (core.Event, error) {
		return b.EditCardTitle(ctx, a, v.Card, v.Base, v.Title)
	},
	"card.description": func(ctx context.Context, b *board.Service, a core.Actor, v args) (core.Event, error) {
		return b.EditCardDescription(ctx, a, v.Card, v.Base, v.Text)
	},
	"card.move": func(ctx context.Context, b *board.Service, a core.Actor, v args) (core.Event, error) {
		return b.MoveCard(ctx, a, v.Card, v.Column, v.After)
	},
	"card.assign": func(ctx context.Context, b *board.Service, a core.Actor, v args) (core.Event, error) {
		return b.AssignCard(ctx, a, v.Card, v.User)
	},
	"card.unassign": func(ctx context.Context, b *board.Service, a core.Actor, v args) (core.Event, error) {
		return b.UnassignCard(ctx, a, v.Card, v.User)
	},
	"card.due": func(ctx context.Context, b *board.Service, a core.Actor, v args) (core.Event, error) {
		return b.SetCardDue(ctx, a, v.Card, v.Due)
	},
	"card.question": func(ctx context.Context, b *board.Service, a core.Actor, v args) (core.Event, error) {
		return b.SetCardQuestion(ctx, a, v.Card, v.Question)
	},
	"card.done": func(ctx context.Context, b *board.Service, a core.Actor, v args) (core.Event, error) {
		return b.SetCardDone(ctx, a, v.Card, v.Done)
	},
	"card.delete": func(ctx context.Context, b *board.Service, a core.Actor, v args) (core.Event, error) {
		return b.DeleteCard(ctx, a, v.Card)
	},
	"checklist.add": func(ctx context.Context, b *board.Service, a core.Actor, v args) (core.Event, error) {
		return b.AddChecklistItem(ctx, a, v.Card, v.Text)
	},
	"checklist.toggle": func(ctx context.Context, b *board.Service, a core.Actor, v args) (core.Event, error) {
		return b.ToggleChecklistItem(ctx, a, v.Item, v.Done)
	},
	"checklist.remove": func(ctx context.Context, b *board.Service, a core.Actor, v args) (core.Event, error) {
		return b.RemoveChecklistItem(ctx, a, v.Item)
	},
	"comment.post": func(ctx context.Context, b *board.Service, a core.Actor, v args) (core.Event, error) {
		return b.PostComment(ctx, a, v.Card, v.Text)
	},
	"comment.delete": func(ctx context.Context, b *board.Service, a core.Actor, v args) (core.Event, error) {
		return b.DeleteComment(ctx, a, v.Comment)
	},
	"undo": func(ctx context.Context, b *board.Service, a core.Actor, v args) (core.Event, error) {
		return b.Undo(ctx, a, v.Activity)
	},
}

// dispatch answers every command with the applied event, the conflict or the
// refusal, carrying the tab's own request number so an optimistic change knows
// which answer is its own.
func (h *Hub) dispatch(ctx context.Context, c *client, cmd command) {
	if cmd.Cmd == "where" {
		// Presence is echoed to everybody in the room, so it takes the same
		// cap as any other field rather than whatever fits in a frame.
		where, err := board.Field(cmd.Args.Where, board.MaxWord)
		if err != nil {
			c.send(message{Type: "error", ID: cmd.ID, Error: reason(err)})
			return
		}
		c.moveTo(where)
		h.announce(c.proposition)
		return
	}
	if cmd.Key != "" {
		keyed, err := core.WithKey(ctx, cmd.Key)
		if err != nil {
			c.send(message{Type: "error", ID: cmd.ID, Error: reason(err)})
			return
		}
		ctx = keyed
	}
	actor := core.Actor{Kind: core.KindUser, ID: c.user.ID, Name: c.user.Name}
	run := h.docsCommand(cmd.Cmd)
	if run == nil {
		board, ok := commands[cmd.Cmd]
		if !ok {
			c.send(message{Type: "error", ID: cmd.ID, Error: "there is no such command"})
			return
		}
		run = func(ctx context.Context, a core.Actor, v args) (core.Event, error) {
			return board(ctx, h.board, a, v)
		}
	}
	e, err := run(ctx, actor, cmd.Args)
	var conflict *core.ConflictError
	switch {
	case errors.As(err, &conflict):
		c.send(message{Type: "conflict", ID: cmd.ID, Conflict: conflict})
	case err != nil:
		h.log.Warn("command refused", "cmd", cmd.Cmd, "user", c.user.Handle, "err", err)
		c.send(message{Type: "error", ID: cmd.ID, Error: reason(err)})
	default:
		c.send(message{Type: "ack", ID: cmd.ID, Event: &e})
	}
}

// reason is what a refusal says out loud. Anything unrecognized is a fault on
// this side, and the log has the detail the tab has no business seeing.
func reason(err error) string {
	switch {
	case errors.Is(err, core.ErrForbidden):
		return "you cannot do that here"
	case errors.Is(err, core.ErrNotFound):
		return "that is no longer there"
	case errors.Is(err, core.ErrKey), errors.Is(err, core.ErrNotUndoable),
		errors.Is(err, board.ErrColumnNotEmpty),
		errors.Is(err, board.ErrNotYours), errors.Is(err, board.ErrEmpty),
		errors.Is(err, board.ErrArchived), errors.Is(err, board.ErrTooLong),
		errors.Is(err, board.ErrQuestion), errors.Is(err, board.ErrStatus),
		errors.Is(err, board.ErrDueDate),
		errors.Is(err, docs.ErrNameTaken), errors.Is(err, docs.ErrTooManyDocuments):
		return err.Error()
	default:
		return "that did not go through"
	}
}
