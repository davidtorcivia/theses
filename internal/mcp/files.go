package mcp

import (
	"context"
	"errors"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davidtorcivia/theses/internal/api"
	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/files"
)

// Files adds the links and files tools. It is called once at startup, after
// the server is built, so that mcp.New stays the list of tools that need
// nothing but the database.
func Files(s *Server, svc *files.Service) {
	f := &fileTools{Server: s, svc: svc}
	sdk.AddTool(s.srv, &sdk.Tool{Name: "storage_orphans", Description: "Reports old deleted-file objects eligible for conservative cleanup by an owner.", Annotations: storageHints(true)}, f.orphans)
	sdk.AddTool(s.srv, &sdk.Tool{Name: "storage_cleanup", Description: "Rechecks and deletes one reported orphan object as an owner.", Annotations: storageHints(false)}, f.cleanup)
	sdk.AddTool(s.srv, &sdk.Tool{Name: "get_file", Description: "Reads one current or historical file and its metadata.", Annotations: reads("Read a file")}, f.getFile)
	sdk.AddTool(s.srv, &sdk.Tool{Name: "edit_file", Description: "Updates file names, folders, notes or tags using metadata_version for notes and tags.", Annotations: overwrites("Edit a file")}, f.editFile)
	sdk.AddTool(s.srv, &sdk.Tool{Name: "list_file_comments", Description: "Lists timestamped comments on a recording.", Annotations: reads("Read recording comments")}, f.fileComments)
	sdk.AddTool(s.srv, &sdk.Tool{Name: "add_file_comment", Description: "Adds a timestamped recording comment with an optional retry key.", Annotations: adds("Comment on a recording")}, f.addFileComment)
	sdk.AddTool(s.srv, &sdk.Tool{Name: "delete_file_comment", Description: "Deletes your own timestamped recording comment.", Annotations: overwrites("Delete a recording comment")}, f.deleteFileComment)
	sdk.AddTool(s.srv, &sdk.Tool{Name: "production_template", Description: "Creates or returns the assigned production checklist for a proposition.", Annotations: attaches("Create production checklist")}, f.productionTemplate)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        "list_links",
		Description: "Lists the links saved on one proposition, with their kind, author, year, note, question and citation.",
		Annotations: reads("List the links"),
	}, f.listLinks)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        "add_link",
		Description: "Saves a URL on one proposition, reading the page for its title, author, year and kind.",
		Annotations: fetches("Add a link"),
	}, f.addLink)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        "list_files",
		Description: "Lists the files uploaded to one proposition, with their folder, size and state.",
		Annotations: reads("List the files"),
	}, f.listFiles)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        "get_download_url",
		Description: "Returns a download link for one file that works for a few minutes.",
		Annotations: reads("Get a download link"),
	}, f.downloadURL)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        "attach_to_card",
		Description: "Attaches a link or a file to a card on the same proposition, or detaches it again.",
		Annotations: attaches("Attach to a card"),
	}, f.attach)
}

// attaches is the hint for hanging something off a card. It is not read only
// and not destructive, and running it twice leaves the same one thing
// attached, which is what idempotent means here. The documents tools carry an
// adds of their own, for a tool that makes a new thing on every call, which is
// the other answer to the same question.
func attaches(title string) *sdk.ToolAnnotations {
	no := false
	return &sdk.ToolAnnotations{
		Title:           title,
		DestructiveHint: &no,
		IdempotentHint:  true,
		OpenWorldHint:   &no,
	}
}

// fetches is the hint for adding a link: two calls make two links, so it is not
// idempotent, and it reads a page on the open web, which is what the open world
// hint is for.
func fetches(title string) *sdk.ToolAnnotations {
	yes, no := true, false
	return &sdk.ToolAnnotations{
		Title:           title,
		DestructiveHint: &no,
		IdempotentHint:  false,
		OpenWorldHint:   &yes,
	}
}

type fileTools struct {
	*Server
	svc *files.Service
}

// errOneOf is a call that named both a link and a file, or neither.
var errOneOf = errors.New("give either a link or a file, not both and not neither")

// actorFor is who a tool call writes as: the person the token belongs to, with
// the client's name as via, which is the same attribution the REST API records.
func (f *fileTools) actorFor(req *sdk.CallToolRequest, p api.Principal) core.Actor {
	return person(req, p)
}

type propositionArgs struct {
	Proposition int64 `json:"proposition" jsonschema:"the proposition's id, as list_propositions reports it"`
}

type linksOut struct {
	Links []files.Link `json:"links"`
}

func (f *fileTools) listLinks(ctx context.Context, req *sdk.CallToolRequest, in propositionArgs) (*sdk.CallToolResult, linksOut, error) {
	p, err := principal(ctx, auth.ScopeRead)
	if err != nil {
		return nil, linksOut{}, err
	}
	rows, err := f.svc.ListLinks(ctx, f.actorFor(req, p), in.Proposition)
	if err != nil {
		return nil, linksOut{}, f.refusal("list the links", err)
	}
	if rows == nil {
		rows = []files.Link{}
	}
	return nil, linksOut{Links: rows}, nil
}

type addLinkArgs struct {
	Proposition int64  `json:"proposition" jsonschema:"the proposition's id"`
	URL         string `json:"url" jsonschema:"the http or https address to save"`
	Note        string `json:"note,omitempty" jsonschema:"why this matters for the episode"`
	Question    string `json:"question,omitempty" jsonschema:"one of I, II, III or IV, or empty"`
	Key         string `json:"key,omitempty" jsonschema:"an optional name for this change; calling again with the same key answers with what the first call did rather than making a second"`
}

func (f *fileTools) addLink(ctx context.Context, req *sdk.CallToolRequest, in addLinkArgs) (*sdk.CallToolResult, files.Link, error) {
	p, err := principal(ctx, auth.ScopeWrite)
	if err != nil {
		return nil, files.Link{}, err
	}
	a := f.actorFor(req, p)
	ctx, err = keyed(ctx, in.Key)
	if err != nil {
		return nil, files.Link{}, err
	}
	e, err := f.svc.AddLinkWithNote(ctx, a, in.Proposition, in.URL, in.Note, in.Question)
	if err != nil {
		return nil, files.Link{}, f.refusal("add the link", err)
	}
	f.log.Info("mcp write", "tool", "add_link", "proposition", in.Proposition,
		"user", p.User.ID, "via", a.Via, "protocol", req.ProtocolVersion())

	link, err := f.svc.ReadLink(ctx, a, e.EntityID)
	if err != nil {
		return nil, files.Link{}, f.refusal("read the link back", err)
	}
	return nil, link, nil
}

type filesOut struct {
	Files []files.File `json:"files"`
}

func (f *fileTools) listFiles(ctx context.Context, req *sdk.CallToolRequest, in propositionArgs) (*sdk.CallToolResult, filesOut, error) {
	p, err := principal(ctx, auth.ScopeRead)
	if err != nil {
		return nil, filesOut{}, err
	}
	rows, err := f.svc.ListFiles(ctx, f.actorFor(req, p), in.Proposition)
	if err != nil {
		return nil, filesOut{}, f.refusal("list the files", err)
	}
	return nil, filesOut{Files: rows}, nil
}

type fileArgs struct {
	File int64 `json:"file" jsonschema:"the file's id, as list_files reports it"`
}

type downloadOut struct {
	URL string `json:"url"`
}

func (f *fileTools) downloadURL(ctx context.Context, req *sdk.CallToolRequest, in fileArgs) (*sdk.CallToolResult, downloadOut, error) {
	p, err := principal(ctx, auth.ScopeRead)
	if err != nil {
		return nil, downloadOut{}, err
	}
	url, err := f.svc.DownloadURL(ctx, f.actorFor(req, p), in.File)
	if err != nil {
		return nil, downloadOut{}, f.refusal("make a download link", err)
	}
	return nil, downloadOut{URL: url}, nil
}

type attachArgs struct {
	Card   int64 `json:"card" jsonschema:"the card's id"`
	Link   int64 `json:"link,omitempty" jsonschema:"the link to attach, or leave empty and give a file"`
	File   int64 `json:"file,omitempty" jsonschema:"the file to attach, or leave empty and give a link"`
	Detach bool  `json:"detach,omitempty" jsonschema:"true to take it off the card instead"`
}

type attachOut struct {
	Card   int64  `json:"card"`
	Action string `json:"action"`
}

func (f *fileTools) attach(ctx context.Context, req *sdk.CallToolRequest, in attachArgs) (*sdk.CallToolResult, attachOut, error) {
	p, err := principal(ctx, auth.ScopeWrite)
	if err != nil {
		return nil, attachOut{}, err
	}
	if (in.Link == 0) == (in.File == 0) {
		return nil, attachOut{}, errOneOf
	}
	a := f.actorFor(req, p)
	var e core.Event
	switch {
	case in.Link != 0 && in.Detach:
		e, err = f.svc.DetachLink(ctx, a, in.Card, in.Link)
	case in.Link != 0:
		e, err = f.svc.AttachLink(ctx, a, in.Card, in.Link)
	case in.Detach:
		e, err = f.svc.DetachFile(ctx, a, in.Card, in.File)
	default:
		e, err = f.svc.AttachFile(ctx, a, in.Card, in.File)
	}
	if err != nil {
		return nil, attachOut{}, f.refusal("change the card's attachments", err)
	}
	f.log.Info("mcp write", "tool", "attach_to_card", "card", in.Card,
		"user", p.User.ID, "via", a.Via, "protocol", req.ProtocolVersion())
	return nil, attachOut{Card: e.EntityID, Action: e.Action}, nil
}

func (f *fileTools) getFile(ctx context.Context, req *sdk.CallToolRequest, in fileArgs) (*sdk.CallToolResult, files.File, error) {
	p, err := principal(ctx, auth.ScopeRead)
	if err != nil {
		return nil, files.File{}, err
	}
	row, err := f.svc.ReadFile(ctx, person(req, p), in.File)
	if err != nil {
		err = f.refusal("read the file", err)
	}
	return nil, row, err
}

type editFileArgs struct {
	File    int64   `json:"file"`
	Name    *string `json:"name,omitempty"`
	Folder  *string `json:"folder,omitempty"`
	Note    *string `json:"note_md,omitempty"`
	Tags    *string `json:"tags,omitempty"`
	Version *int64  `json:"metadata_version,omitempty"`
}

func (f *fileTools) editFile(ctx context.Context, req *sdk.CallToolRequest, in editFileArgs) (*sdk.CallToolResult, core.Event, error) {
	p, err := principal(ctx, auth.ScopeWrite)
	if err != nil {
		return nil, core.Event{}, err
	}
	e, err := f.svc.PatchFileDetails(ctx, person(req, p), in.File, files.FilePatch{Name: in.Name, Folder: in.Folder, Note: in.Note, Tags: in.Tags, Version: in.Version})
	if err != nil {
		err = f.refusal("change file details", err)
	}
	return nil, e, err
}
func (f *fileTools) fileComments(ctx context.Context, req *sdk.CallToolRequest, in fileArgs) (*sdk.CallToolResult, []files.FileComment, error) {
	p, err := principal(ctx, auth.ScopeRead)
	if err != nil {
		return nil, nil, err
	}
	rows, err := f.svc.FileComments(ctx, person(req, p), in.File)
	if err != nil {
		err = f.refusal("read file details", err)
	}
	return nil, rows, err
}

type addFileCommentArgs struct {
	File     int64  `json:"file"`
	Position int64  `json:"position_ms"`
	Body     string `json:"body_md"`
	Key      string `json:"key,omitempty"`
}

func (f *fileTools) addFileComment(ctx context.Context, req *sdk.CallToolRequest, in addFileCommentArgs) (*sdk.CallToolResult, core.Event, error) {
	p, err := principal(ctx, auth.ScopeWrite)
	if err != nil {
		return nil, core.Event{}, err
	}
	ctx, err = keyed(ctx, in.Key)
	if err != nil {
		return nil, core.Event{}, err
	}
	e, err := f.svc.AddFileComment(ctx, person(req, p), in.File, in.Position, in.Body)
	if err != nil {
		err = f.refusal("change file details", err)
	}
	return nil, e, err
}

type deleteFileCommentArgs struct {
	File    int64 `json:"file"`
	Comment int64 `json:"comment"`
}

func (f *fileTools) deleteFileComment(ctx context.Context, req *sdk.CallToolRequest, in deleteFileCommentArgs) (*sdk.CallToolResult, core.Event, error) {
	p, err := principal(ctx, auth.ScopeWrite)
	if err != nil {
		return nil, core.Event{}, err
	}
	e, err := f.svc.DeleteFileComment(ctx, person(req, p), in.File, in.Comment)
	if err != nil {
		err = f.refusal("change file details", err)
	}
	return nil, e, err
}
func (f *fileTools) productionTemplate(ctx context.Context, req *sdk.CallToolRequest, in propositionArgs) (*sdk.CallToolResult, core.Event, error) {
	p, err := principal(ctx, auth.ScopeWrite)
	if err != nil {
		return nil, core.Event{}, err
	}
	e, err := f.api.Board.ProductionTemplate(ctx, person(req, p), in.Proposition)
	if err != nil {
		err = f.refusal("change file details", err)
	}
	return nil, e, err
}

func (f *fileTools) orphans(ctx context.Context, req *sdk.CallToolRequest, in struct {
	Before int64 `json:"before,omitempty"`
}) (*sdk.CallToolResult, files.OrphanReport, error) {
	p, err := principal(ctx, auth.ScopeAdmin)
	if err != nil {
		return nil, files.OrphanReport{}, err
	}
	rows, err := f.svc.OrphanPage(ctx, person(req, p), in.Before)
	if err != nil {
		err = f.refusal("read file details", err)
	}
	return nil, rows, err
}
func (f *fileTools) cleanup(ctx context.Context, req *sdk.CallToolRequest, in files.Orphan) (*sdk.CallToolResult, core.Event, error) {
	p, err := principal(ctx, auth.ScopeAdmin)
	if err != nil {
		return nil, core.Event{}, err
	}
	e, err := f.svc.CleanupObject(ctx, person(req, p), in)
	if err != nil {
		err = f.refusal("change file details", err)
	}
	return nil, e, err
}

func storageHints(readOnly bool) *sdk.ToolAnnotations {
	yes := true
	destructive := !readOnly
	return &sdk.ToolAnnotations{Title: "Inspect or clean up storage", ReadOnlyHint: readOnly, DestructiveHint: &destructive, OpenWorldHint: &yes}
}
