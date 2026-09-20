package realtime

import (
	"context"

	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/docs"
)

// docCommands is the document surface, the same shape as the board's: every one
// of them is a docs command called with the session's actor, so a tab can do
// exactly what the API and MCP can do and no more.
var docCommands = map[string]func(context.Context, *docs.Service, core.Actor, args) (core.Event, error){
	"document.create": func(ctx context.Context, d *docs.Service, a core.Actor, v args) (core.Event, error) {
		return d.CreateDocument(ctx, a, v.Proposition, v.Title)
	},
	"document.rename": func(ctx context.Context, d *docs.Service, a core.Actor, v args) (core.Event, error) {
		return d.RenameDocument(ctx, a, v.Document, v.Title)
	},
	"document.delete": func(ctx context.Context, d *docs.Service, a core.Actor, v args) (core.Event, error) {
		return d.DeleteDocument(ctx, a, v.Document)
	},
	"block.set": func(ctx context.Context, d *docs.Service, a core.Actor, v args) (core.Event, error) {
		return d.SetBlock(ctx, a, v.Block, v.Base, v.Text, v.Whole)
	},
	"block.insert": func(ctx context.Context, d *docs.Service, a core.Actor, v args) (core.Event, error) {
		return d.InsertBlock(ctx, a, v.Document, v.After, v.AfterKey, v.Text, v.Whole)
	},
	"block.move": func(ctx context.Context, d *docs.Service, a core.Actor, v args) (core.Event, error) {
		return d.MoveBlock(ctx, a, v.Block, v.After)
	},
	"block.delete": func(ctx context.Context, d *docs.Service, a core.Actor, v args) (core.Event, error) {
		return d.DeleteBlock(ctx, a, v.Block)
	},
	// Only the button makes a revision from here. The timer and the importer
	// make their own, and neither of them is a tab.
	"revision.create": func(ctx context.Context, d *docs.Service, a core.Actor, v args) (core.Event, error) {
		return d.CreateRevision(ctx, a, v.Document, docs.ReasonManual)
	},
}

// docsCommand is the document command of that name bound to this hub's
// service, or nil when there is no such command or no service behind it.
func (h *Hub) docsCommand(name string) func(context.Context, core.Actor, args) (core.Event, error) {
	run, ok := docCommands[name]
	if !ok || h.Docs == nil {
		return nil
	}
	return func(ctx context.Context, a core.Actor, v args) (core.Event, error) {
		return run(ctx, h.Docs, a, v)
	}
}
