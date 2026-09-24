package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/docs"
	"github.com/davidtorcivia/theses/internal/store"
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
	c.send(h.execute(ctx, c.user, cmd))
}

// Commands is the session-authenticated fallback, mounted behind the CSRF guard.
func (h *Hub) Commands(w http.ResponseWriter, r *http.Request) {
	user, err := h.auth.SessionUser(r.Context(), r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in first"})
		return
	}
	if !h.auth.Allow(auth.BucketSocket, strconv.FormatInt(user.ID, 10)) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many changes at once; wait a moment"})
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxFrame))
	var cmd command
	if err != nil {
		var large *http.MaxBytesError
		status := http.StatusBadRequest
		if errors.As(err, &large) {
			status = http.StatusRequestEntityTooLarge
		}
		writeJSON(w, status, map[string]string{"error": "that command body could not be read"})
		return
	}
	if json.Unmarshal(raw, &cmd) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "that was not a command"})
		return
	}
	writeJSON(w, http.StatusOK, h.execute(r.Context(), user, cmd))
}

func (h *Hub) execute(ctx context.Context, user *store.User, cmd command) message {
	// Worded so the outbox's drain takes it as worth trying again, which is
	// what the 503 from the write gate says to an HTTP client.
	if h.Frozen != nil && h.Frozen() {
		return message{Type: "error", ID: cmd.ID, Error: "a backup is being restored; wait a moment and try again"}
	}
	if cmd.Key != "" {
		keyed, err := core.WithKey(ctx, cmd.Key)
		if err != nil {
			return message{Type: "error", ID: cmd.ID, Error: reason(err)}
		}
		ctx = keyed
	}
	actor := core.Actor{Kind: core.KindUser, ID: user.ID, Name: user.Name}
	run := h.docsCommand(cmd.Cmd)
	if run == nil {
		board, ok := commands[cmd.Cmd]
		if !ok {
			return message{Type: "error", ID: cmd.ID, Error: "there is no such command"}
		}
		run = func(ctx context.Context, a core.Actor, v args) (core.Event, error) {
			return board(ctx, h.board, a, v)
		}
	}
	e, err := run(ctx, actor, cmd.Args)
	var conflict *core.ConflictError
	switch {
	case errors.As(err, &conflict):
		return message{Type: "conflict", ID: cmd.ID, Conflict: conflict}
	case err != nil:
		h.log.Warn("command refused", "cmd", cmd.Cmd, "user", user.Handle, "err", err)
		return message{Type: "error", ID: cmd.ID, Error: reason(err)}
	default:
		return message{Type: "ack", ID: cmd.ID, Event: &e}
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
		errors.Is(err, board.ErrNotYours), errors.Is(err, board.ErrEmpty), errors.Is(err, board.ErrShow),
		errors.Is(err, board.ErrLegalReleases), errors.Is(err, board.ErrUploading), errors.Is(err, board.ErrArchived), errors.Is(err, board.ErrTooLong),
		errors.Is(err, board.ErrQuestion), errors.Is(err, board.ErrStatus),
		errors.Is(err, board.ErrDueDate),
		errors.Is(err, docs.ErrNameTaken), errors.Is(err, docs.ErrTooManyDocuments),
		errors.Is(err, docs.ErrAfterBoth):
		return err.Error()
	default:
		return "that did not go through"
	}
}
