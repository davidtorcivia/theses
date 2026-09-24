package mcp

import (
	"context"

	"github.com/davidtorcivia/theses/internal/api"
	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/docs"
	"github.com/davidtorcivia/theses/internal/files"
)

// The rest of what the browser can do, each a tool over the command or the API
// method its REST route calls, so a rule is written once whichever surface
// asks. The tools that make something take a key, like their siblings.

type editPropositionArgs struct {
	Proposition int64 `json:"proposition" jsonschema:"the proposition's id"`
	api.PropositionPatch
}
type movePropositionArgs struct {
	Proposition int64 `json:"proposition" jsonschema:"the proposition's id"`
	After       int64 `json:"after,omitempty" jsonschema:"the proposition to put it after on the rail, or leave it out for the top"`
}
type archiveArgs struct {
	Proposition int64 `json:"proposition" jsonschema:"the proposition's id"`
	Restore     bool  `json:"restore,omitempty" jsonschema:"true to restore an archived proposition instead"`
}
type memberArgs struct {
	Proposition int64 `json:"proposition" jsonschema:"the proposition's id"`
	User        int64 `json:"user" jsonschema:"the person's id, as list_users reports it"`
	Remove      bool  `json:"remove,omitempty" jsonschema:"true to take them off the proposition instead"`
}
type createColumnArgs struct {
	workflowKey
	Proposition int64  `json:"proposition" jsonschema:"the proposition to add the column to"`
	Name        string `json:"name" jsonschema:"what the column is called"`
}
type renameColumnArgs struct {
	Column int64  `json:"column" jsonschema:"the column's id, as list_cards reports the columns"`
	Name   string `json:"name" jsonschema:"what the column is called now"`
}
type moveColumnArgs struct {
	Column int64 `json:"column" jsonschema:"the column's id"`
	After  int64 `json:"after,omitempty" jsonschema:"the column to put it after, or leave it out for the first place"`
}
type columnArgs struct {
	Column int64 `json:"column" jsonschema:"the column's id"`
}
type editCardArgs struct {
	Card int64 `json:"card" jsonschema:"the card's id"`
	api.CardPatch
}
type cardArgs struct {
	Card int64 `json:"card" jsonschema:"the card's id"`
}
type addItemArgs struct {
	workflowKey
	Card int64  `json:"card" jsonschema:"the card to add the item to"`
	Text string `json:"text" jsonschema:"what the item says"`
}
type checkItemArgs struct {
	Item int64 `json:"item" jsonschema:"the checklist item's id, as list_cards reports it"`
	Done bool  `json:"done" jsonschema:"true to tick it and false to untick it"`
}
type itemArgs struct {
	Item int64 `json:"item" jsonschema:"the checklist item's id"`
}
type noteArgs struct {
	Comment int64 `json:"comment" jsonschema:"the note's id"`
}
type undoArgs struct {
	Activity int64 `json:"activity" jsonschema:"the activity row's id, as activity reports it"`
}
type renameDocumentArgs struct {
	Document int64  `json:"document" jsonschema:"the document's id"`
	Name     string `json:"name" jsonschema:"what the tab above the document says now"`
}
type revisionArgs struct {
	workflowKey
	Document int64 `json:"document" jsonschema:"the document to keep a revision of"`
}
type linkArgs struct {
	Link int64 `json:"link" jsonschema:"the link's id, as list_links reports it"`
}
type partsArgs struct {
	File  int64 `json:"file" jsonschema:"the file's id, as request_upload reported it"`
	After int   `json:"after,omitempty" jsonschema:"the last part number already signed, to sign the batch after it"`
}
type completeArgs struct {
	File       int64 `json:"file" jsonschema:"the file's id, as request_upload reported it"`
	DurationMS int64 `json:"duration_ms,omitempty" jsonschema:"a recording's length in milliseconds, when it has one"`
	Width      int64 `json:"width,omitempty" jsonschema:"an image or video's width in pixels, when it has one"`
	Height     int64 `json:"height,omitempty" jsonschema:"an image or video's height in pixels, when it has one"`
}
type webhookArgs struct {
	ID int64 `json:"id,omitempty" jsonschema:"the webhook to change, or leave it out to add one"`
	api.WebhookPatch
}
type archiveKeyArgs struct {
	Archive string `json:"archive" jsonschema:"the archive's key, as list_backups reports it"`
}
type restoreBackupArgs struct {
	archiveKeyArgs
	Confirm string `json:"confirm" jsonschema:"the same archive key again, sent only once the person has said to restore it"`
}

// addEditTools registers them. It is called from Board, beside the tools whose
// services these share.
func addEditTools(s *Server, b *board.Service, svc *files.Service) {
	workflowTool(s, "edit_proposition", "Changes the title, statement, blurb, status, episode or release date a call names and keeps the rest.", auth.ScopeWrite, overwrites("Edit a proposition"), func(ctx context.Context, a core.Actor, in editPropositionArgs) (core.Event, error) {
		p, _ := api.PrincipalFrom(ctx)
		return s.api.EditProposition(ctx, p, a, in.Proposition, in.PropositionPatch)
	})
	workflowTool(s, "move_proposition", "Moves one proposition on the rail, after another or to the top.", auth.ScopeWrite, overwrites("Move a proposition"), func(ctx context.Context, a core.Actor, in movePropositionArgs) (core.Event, error) {
		return b.MoveProposition(ctx, a, in.Proposition, in.After)
	})
	workflowTool(s, "archive_proposition", "Archives one proposition so it is read only, or restores it.", auth.ScopeWrite, overwrites("Archive a proposition"), func(ctx context.Context, a core.Actor, in archiveArgs) (core.Event, error) {
		if in.Restore {
			return b.RestoreProposition(ctx, a, in.Proposition)
		}
		return b.ArchiveProposition(ctx, a, in.Proposition)
	})
	workflowTool(s, "delete_proposition", "Deletes one proposition and everything on it, which a researcher may not do.", auth.ScopeWrite, overwrites("Delete a proposition"), func(ctx context.Context, a core.Actor, in propositionArgs) (core.Event, error) {
		return b.DeleteProposition(ctx, a, in.Proposition)
	})
	workflowTool(s, "set_member", "Makes somebody a member of one proposition, or takes them off it.", auth.ScopeWrite, attaches("Change the members"), func(ctx context.Context, a core.Actor, in memberArgs) (core.Event, error) {
		if in.Remove {
			return b.RemoveMember(ctx, a, in.Proposition, in.User)
		}
		return b.AddMember(ctx, a, in.Proposition, in.User)
	})

	workflowTool(s, "create_column", "Adds a column at the end of one proposition's board.", auth.ScopeWrite, adds("Add a column"), func(ctx context.Context, a core.Actor, in createColumnArgs) (core.Event, error) {
		return b.CreateColumn(ctx, a, in.Proposition, in.Name)
	})
	workflowTool(s, "rename_column", "Renames one column.", auth.ScopeWrite, overwrites("Rename a column"), func(ctx context.Context, a core.Actor, in renameColumnArgs) (core.Event, error) {
		return b.RenameColumn(ctx, a, in.Column, in.Name)
	})
	workflowTool(s, "move_column", "Moves one column after another or to the first place.", auth.ScopeWrite, overwrites("Move a column"), func(ctx context.Context, a core.Actor, in moveColumnArgs) (core.Event, error) {
		return b.MoveColumn(ctx, a, in.Column, in.After)
	})
	workflowTool(s, "delete_column", "Deletes one column that has no cards left in it.", auth.ScopeWrite, overwrites("Delete a column"), func(ctx context.Context, a core.Actor, in columnArgs) (core.Event, error) {
		return b.DeleteColumn(ctx, a, in.Column)
	})

	workflowTool(s, "edit_card", "Changes the title, description, question or due date a call names, merging nothing: a stale base_version is refused with the text the card holds now.", auth.ScopeWrite, overwrites("Edit a card"), func(ctx context.Context, a core.Actor, in editCardArgs) (core.Event, error) {
		return s.api.EditCard(ctx, a, in.Card, in.CardPatch)
	})
	workflowTool(s, "delete_card", "Deletes one card; list_trash keeps it for a week.", auth.ScopeWrite, overwrites("Delete a card"), func(ctx context.Context, a core.Actor, in cardArgs) (core.Event, error) {
		return b.DeleteCard(ctx, a, in.Card)
	})
	workflowTool(s, "add_checklist_item", "Adds an item at the end of one card's checklist.", auth.ScopeWrite, adds("Add a checklist item"), func(ctx context.Context, a core.Actor, in addItemArgs) (core.Event, error) {
		return b.AddChecklistItem(ctx, a, in.Card, in.Text)
	})
	workflowTool(s, "check_item", "Ticks or unticks one checklist item.", auth.ScopeWrite, overwrites("Tick a checklist item"), func(ctx context.Context, a core.Actor, in checkItemArgs) (core.Event, error) {
		return b.ToggleChecklistItem(ctx, a, in.Item, in.Done)
	})
	workflowTool(s, "remove_checklist_item", "Removes one checklist item.", auth.ScopeWrite, overwrites("Remove a checklist item"), func(ctx context.Context, a core.Actor, in itemArgs) (core.Event, error) {
		return b.RemoveChecklistItem(ctx, a, in.Item)
	})
	workflowTool(s, "delete_comment", "Deletes one of your own notes on a card.", auth.ScopeWrite, overwrites("Delete a note"), func(ctx context.Context, a core.Actor, in noteArgs) (core.Event, error) {
		return b.DeleteComment(ctx, a, in.Comment)
	})
	workflowTool(s, "undo", "Puts back what one activity row changed, unless it was a create, is already undone or has moved on since.", auth.ScopeWrite, changes("Undo a change"), func(ctx context.Context, a core.Actor, in undoArgs) (core.Event, error) {
		return b.Undo(ctx, a, in.Activity)
	})

	workflowTool(s, "rename_document", "Renames one document.", auth.ScopeWrite, overwrites("Rename a document"), func(ctx context.Context, a core.Actor, in renameDocumentArgs) (core.Event, error) {
		service, err := s.documents()
		if err != nil {
			return core.Event{}, err
		}
		return service.RenameDocument(ctx, a, in.Document, in.Name)
	})
	workflowTool(s, "delete_document", "Deletes one document and its blocks.", auth.ScopeWrite, overwrites("Delete a document"), func(ctx context.Context, a core.Actor, in documentArgs) (core.Event, error) {
		service, err := s.documents()
		if err != nil {
			return core.Event{}, err
		}
		return service.DeleteDocument(ctx, a, in.Document)
	})
	workflowTool(s, "list_revisions", "Lists one document's revisions, newest first, each with the markdown it holds.", auth.ScopeRead, reads("List the revisions"), func(ctx context.Context, a core.Actor, in documentArgs) ([]docs.Revision, error) {
		service, err := s.documents()
		if err != nil {
			return nil, err
		}
		p, _ := api.PrincipalFrom(ctx)
		return service.History(ctx, p.User, in.Document)
	})
	workflowTool(s, "create_revision", "Keeps a revision of one document as it stands now.", auth.ScopeWrite, adds("Keep a revision"), func(ctx context.Context, a core.Actor, in revisionArgs) (core.Event, error) {
		service, err := s.documents()
		if err != nil {
			return core.Event{}, err
		}
		return service.CreateRevision(ctx, a, in.Document, docs.ReasonManual)
	})

	refetch := overwrites("Read a link again")
	yes := true
	refetch.OpenWorldHint = &yes
	workflowTool(s, "refetch_link", "Reads a saved link's page again for its title, author, year and kind.", auth.ScopeWrite, refetch, func(ctx context.Context, a core.Actor, in linkArgs) (files.Link, error) {
		e, err := svc.RefetchLink(ctx, a, in.Link)
		if err != nil {
			return files.Link{}, err
		}
		return svc.ReadLink(ctx, a, e.EntityID)
	})
	workflowTool(s, "delete_link", "Deletes one saved link.", auth.ScopeWrite, overwrites("Delete a link"), func(ctx context.Context, a core.Actor, in linkArgs) (core.Event, error) {
		return svc.DeleteLink(ctx, a, in.Link)
	})

	workflowTool(s, "upload_parts", "Reports which parts of a multipart upload the bucket holds and signs URLs for the next batch.", auth.ScopeFiles, reads("Sign upload parts"), func(ctx context.Context, a core.Actor, in partsArgs) (files.Upload, error) {
		return svc.Parts(ctx, a, in.File, in.After)
	})
	workflowTool(s, "complete_upload", "Checks the bucket holds the whole object request_upload asked for and makes the file available.", auth.ScopeFiles, changes("Complete an upload"), func(ctx context.Context, a core.Actor, in completeArgs) (core.Event, error) {
		return svc.Complete(ctx, a, in.File, in.DurationMS, in.Width, in.Height)
	})
	workflowTool(s, "delete_file", "Deletes one file; a finished one stays in list_trash for a week.", auth.ScopeFiles, overwrites("Delete a file"), func(ctx context.Context, a core.Actor, in fileArgs) (core.Event, error) {
		return svc.Delete(ctx, a, in.File)
	})
	workflowTool(s, "list_file_versions", "Lists the older versions one file replaced, newest first.", auth.ScopeRead, reads("List file versions"), func(ctx context.Context, a core.Actor, in fileArgs) ([]files.File, error) {
		return svc.Versions(ctx, a, in.File)
	})

	workflowTool(s, "list_webhooks", "Lists the workspace's webhooks with the events they fire on.", auth.ScopeAdmin, reads("List the webhooks"), func(ctx context.Context, _ core.Actor, _ noArgs) ([]api.ChannelView, error) {
		return s.api.Webhooks(ctx)
	})
	workflowTool(s, "save_webhook", "Adds a workspace webhook, or changes the fields a call names on one, keeping a secret it leaves out.", auth.ScopeAdmin, changes("Save a webhook"), func(ctx context.Context, a core.Actor, in webhookArgs) (api.ChannelView, error) {
		return s.api.SaveWebhook(ctx, a, in.ID, in.WebhookPatch)
	})
	workflowTool(s, "delete_webhook", "Deletes one of the workspace's webhooks.", auth.ScopeAdmin, overwrites("Delete a webhook"), func(ctx context.Context, a core.Actor, in workflowID) (bool, error) {
		return true, s.api.DeleteWebhook(ctx, a, in.ID)
	})
	workflowTool(s, "test_webhook", "Sends the test message to one webhook and marks it verified when it arrives.", auth.ScopeAdmin, fetches("Test a webhook"), func(ctx context.Context, _ core.Actor, in workflowID) (api.WebhookTest, error) {
		return s.api.TestWebhook(ctx, in.ID)
	})

	workflowTool(s, "list_backups", "Lists the backup archives, newest first, and whether each can be restored.", auth.ScopeAdmin, reads("List the backups"), func(ctx context.Context, _ core.Actor, _ noArgs) ([]api.BackupView, error) {
		return s.api.Backups(ctx)
	})
	workflowTool(s, "verify_backup", "Starts restoring one archive into an isolated workspace to prove it can be, with the result on the settings page.", auth.ScopeAdmin, adds("Verify a backup"), func(ctx context.Context, _ core.Actor, in archiveKeyArgs) (bool, error) {
		return true, s.api.VerifyBackup(ctx, in.Archive)
	})
	workflowTool(s, "restore_backup", "Starts replacing the database and documents with one archive, after which everyone signs in again and every token stops working.", auth.ScopeAdmin, changes("Restore a backup"), func(ctx context.Context, a core.Actor, in restoreBackupArgs) (bool, error) {
		return true, s.api.RestoreBackup(ctx, a, in.Archive, in.Confirm)
	})
}
