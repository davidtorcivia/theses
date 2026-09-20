package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/docs"
	"github.com/davidtorcivia/theses/internal/files"
)

// addDocumentTools registers the document tools. It is one call from New so
// that the list there grows by a line rather than by a screen.
func (s *Server) addDocumentTools() {
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        "list_documents",
		Description: "Lists the documents of one proposition with their names and how many blocks each holds.",
		Annotations: reads("List the documents"),
	}, s.listDocuments)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        "read_document",
		Description: "Reads one document as markdown, with each block's id and version so it can be replaced.",
		Annotations: reads("Read a document"),
	}, s.readDocument)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        "create_document",
		Description: "Creates a document in a proposition and returns its id.",
		Annotations: adds("Create a document"),
	}, s.createDocument)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        "append_block",
		Description: "Adds a paragraph at the end of a document.",
		Annotations: adds("Add a paragraph"),
	}, s.appendBlock)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        "insert_after_heading",
		Description: "Adds a paragraph at the end of the section under a heading, so that several calls read in the order they were made.",
		Annotations: adds("Add a paragraph under a heading"),
	}, s.insertAfterHeading)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        "write_document",
		Description: "Replaces a document, or the part of it the base names, with markdown, keeping the blocks whose paragraphs did not change and answering with the block each paragraph now stands in.",
		Annotations: overwrites("Write a document"),
	}, s.writeDocument)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        "replace_block",
		Description: "Replaces the text of one block, merging in a change somebody else made when the version read_document reported is sent with it and overwriting when it is not.",
		Annotations: overwrites("Replace a block"),
	}, s.replaceBlock)
}

// adds is the hint for a tool that puts something new there. It takes nothing
// away, and calling it twice makes two of them.
func adds(title string) *sdk.ToolAnnotations {
	no := false
	return &sdk.ToolAnnotations{
		Title:           title,
		DestructiveHint: &no,
		IdempotentHint:  false,
		OpenWorldHint:   &no,
	}
}

// documents is the service, refused politely rather than by a panic if a build
// ever leaves it out.
func (s *Server) documents() (*docs.Service, error) {
	if s.api.Docs == nil {
		return nil, errors.New("this endpoint has no documents")
	}
	return s.api.Docs, nil
}

// refusal is what a tool says about a command that did not go through, for
// every tool on this server. A conflict carries the text the row holds now,
// because the caller's next move is to merge it in and send that. A refusal the
// caller can act on comes back word for word; anything else is a fault on this
// side, logged with its detail and answered without it.
func (s *Server) refusal(what string, err error) error {
	var clash *core.ConflictError
	switch {
	case errors.As(err, &clash):
		return fmt.Errorf("that %s changed while you were writing; it is now at version %d and holds: %s",
			clash.Entity, clash.Version, clash.Current)
	case errors.Is(err, core.ErrNotFound), errors.Is(err, core.ErrForbidden):
		// Not there and not allowed answer alike, because the commands answer
		// the role and the membership with one refusal and saying which would
		// say whether the row is there.
		return errors.New("that is not there, or this token's owner may not touch it")
	case errors.Is(err, board.ErrArchived), errors.Is(err, board.ErrEmpty),
		errors.Is(err, board.ErrTooLong), errors.Is(err, board.ErrQuestion),
		errors.Is(err, board.ErrStatus),
		errors.Is(err, board.ErrColumnNotEmpty), errors.Is(err, board.ErrNotYours),
		errors.Is(err, core.ErrNotUndoable),
		errors.Is(err, docs.ErrNameTaken), errors.Is(err, docs.ErrReason),
		errors.Is(err, docs.ErrSourceBase), errors.Is(err, docs.ErrSourceSpread),
		errors.Is(err, docs.ErrTooManyDocuments),
		errors.Is(err, files.ErrKind), errors.Is(err, files.ErrQuestion),
		errors.Is(err, files.ErrURL), errors.Is(err, files.ErrState),
		errors.Is(err, files.ErrSize), errors.Is(err, files.ErrBadSize),
		errors.Is(err, files.ErrSwept), errors.Is(err, files.ErrCrossBucket),
		errors.Is(err, files.ErrNoBucket), errors.Is(err, files.ErrPart):
		return err
	}
	return s.failed(what, err)
}

type listDocumentsArgs struct {
	Proposition int64 `json:"proposition" jsonschema:"the proposition to list the documents of"`
}

type documentSummary struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Slug     string `json:"slug"`
	Revision int64  `json:"revision"`
	Blocks   int    `json:"blocks"`
}

type listDocumentsOut struct {
	Documents []documentSummary `json:"documents"`
}

func (s *Server) listDocuments(ctx context.Context, req *sdk.CallToolRequest, in listDocumentsArgs) (*sdk.CallToolResult, listDocumentsOut, error) {
	p, err := principal(ctx, auth.ScopeRead)
	if err != nil {
		return nil, listDocumentsOut{}, err
	}
	service, err := s.documents()
	if err != nil {
		return nil, listDocumentsOut{}, err
	}
	list, err := service.Documents(ctx, p.User, in.Proposition)
	if err != nil {
		return nil, listDocumentsOut{}, s.refusal("list the documents", err)
	}
	out := listDocumentsOut{Documents: []documentSummary{}}
	for _, d := range list {
		out.Documents = append(out.Documents, documentSummary{
			ID: d.ID, Name: d.Name, Slug: d.Slug, Revision: d.Revision, Blocks: len(d.Blocks)})
	}
	return nil, out, nil
}

type readDocumentArgs struct {
	Document int64 `json:"document" jsonschema:"the document to read, as list_documents reports its id"`
}

type blockOut struct {
	ID      int64  `json:"id"`
	Version int64  `json:"version"`
	Text    string `json:"text"`
}

type readDocumentOut struct {
	ID       int64      `json:"id"`
	Name     string     `json:"name"`
	Revision int64      `json:"revision"`
	Markdown string     `json:"markdown"`
	Blocks   []blockOut `json:"blocks"`
}

func (s *Server) readDocument(ctx context.Context, req *sdk.CallToolRequest, in readDocumentArgs) (*sdk.CallToolResult, readDocumentOut, error) {
	p, err := principal(ctx, auth.ScopeRead)
	if err != nil {
		return nil, readDocumentOut{}, err
	}
	service, err := s.documents()
	if err != nil {
		return nil, readDocumentOut{}, err
	}
	doc, err := service.Document(ctx, p.User, in.Document)
	if err != nil {
		return nil, readDocumentOut{}, s.refusal("read the document", err)
	}
	out := readDocumentOut{ID: doc.ID, Name: doc.Name, Revision: doc.Revision,
		Markdown: docs.Markdown(doc.Blocks), Blocks: []blockOut{}}
	for _, b := range doc.Blocks {
		out.Blocks = append(out.Blocks, blockOut{ID: b.ID, Version: b.Version, Text: b.Text})
	}
	return nil, out, nil
}

type createDocumentArgs struct {
	Proposition int64  `json:"proposition" jsonschema:"the proposition to put the document in"`
	Name        string `json:"name" jsonschema:"what the tab above the document says"`
	Key         string `json:"key,omitempty" jsonschema:"an optional name for this change; calling again with the same key answers with what the first call did rather than making a second"`
}

type writeOut struct {
	ID int64 `json:"id"`
	// Version is the block's version after the write, which the next write on
	// it sends back as base_version.
	Version int64 `json:"version,omitempty"`
}

func (s *Server) createDocument(ctx context.Context, req *sdk.CallToolRequest, in createDocumentArgs) (*sdk.CallToolResult, writeOut, error) {
	service, who, err := s.writer(ctx, req)
	if err != nil {
		return nil, writeOut{}, err
	}
	ctx, err = keyed(ctx, in.Key)
	if err != nil {
		return nil, writeOut{}, err
	}
	e, err := service.CreateDocument(ctx, who, in.Proposition, in.Name)
	if err != nil {
		return nil, writeOut{}, s.refusal("create the document", err)
	}
	return nil, writeOut{ID: e.EntityID}, nil
}

type appendBlockArgs struct {
	Document int64  `json:"document" jsonschema:"the document to add to"`
	Text     string `json:"text" jsonschema:"the markdown of the paragraph; text holding a blank line becomes one block per paragraph, and a fenced code block stays one block whatever is in it"`
	Key      string `json:"key,omitempty" jsonschema:"an optional name for this change; calling again with the same key answers with what the first call did rather than making a second"`
}

func (s *Server) appendBlock(ctx context.Context, req *sdk.CallToolRequest, in appendBlockArgs) (*sdk.CallToolResult, writeOut, error) {
	service, who, err := s.writer(ctx, req)
	if err != nil {
		return nil, writeOut{}, err
	}
	last, err := docs.LastBlock(ctx, service.DB, in.Document)
	if err != nil {
		return nil, writeOut{}, s.refusal("read the document", err)
	}
	ctx, err = keyed(ctx, in.Key)
	if err != nil {
		return nil, writeOut{}, err
	}
	return s.inserted(ctx, service, who, in.Document, last, in.Text)
}

type insertAfterHeadingArgs struct {
	Document int64  `json:"document" jsonschema:"the document to add to"`
	Heading  string `json:"heading" jsonschema:"the heading to add under, with or without its hashes"`
	Text     string `json:"text" jsonschema:"the markdown of the paragraph"`
	Key      string `json:"key,omitempty" jsonschema:"an optional name for this change; calling again with the same key answers with what the first call did rather than making a second"`
}

func (s *Server) insertAfterHeading(ctx context.Context, req *sdk.CallToolRequest, in insertAfterHeadingArgs) (*sdk.CallToolResult, writeOut, error) {
	service, who, err := s.writer(ctx, req)
	if err != nil {
		return nil, writeOut{}, err
	}
	p, err := principal(ctx, auth.ScopeWrite)
	if err != nil {
		return nil, writeOut{}, err
	}
	doc, err := service.Document(ctx, p.User, in.Document)
	if err != nil {
		return nil, writeOut{}, s.refusal("read the document", err)
	}
	after, found := endOfSection(doc.Blocks, in.Heading)
	if !found {
		return nil, writeOut{}, fmt.Errorf("that document has no heading %q", in.Heading)
	}
	ctx, err = keyed(ctx, in.Key)
	if err != nil {
		return nil, writeOut{}, err
	}
	return s.inserted(ctx, service, who, in.Document, after, in.Text)
}

// endOfSection is the block a paragraph added under a heading goes after: the
// last block before the next heading at the same level or above. Adding two
// paragraphs under one heading therefore leaves them in the order they were
// added rather than reversed.
func endOfSection(blocks []docs.Block, heading string) (after int64, found bool) {
	want := headingText(heading)
	level := 0
	for _, b := range blocks {
		text, depth := headingOf(b.Text)
		if !found {
			if depth > 0 && text == want {
				found, level, after = true, depth, b.ID
			}
			continue
		}
		if depth > 0 && depth <= level {
			return after, true
		}
		after = b.ID
	}
	return after, found
}

// headingOf is a block's heading text and how deep it is, or depth zero when the
// block is not a heading.
func headingOf(text string) (string, int) {
	depth := 0
	for depth < len(text) && text[depth] == '#' {
		depth++
	}
	if depth == 0 || depth >= len(text) || text[depth] != ' ' {
		return "", 0
	}
	return headingText(text), depth
}

// headingText is a heading compared the way a person names it: without its
// hashes, without surrounding space, and without regard to case.
func headingText(s string) string {
	return strings.ToLower(strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(s), "#")))
}

func (s *Server) inserted(ctx context.Context, service *docs.Service, who core.Actor,
	document, after int64, text string) (*sdk.CallToolResult, writeOut, error) {
	e, err := service.InsertBlock(ctx, who, document, after, text, false)
	if err != nil {
		return nil, writeOut{}, s.refusal("add the paragraph", err)
	}
	block, err := docs.GetBlock(ctx, service.DB, e.EntityID)
	if err != nil {
		return nil, writeOut{}, s.failed("read the paragraph back", err)
	}
	return nil, writeOut{ID: block.ID, Version: block.Version}, nil
}

// A sourceBlock is one block a write_document call says its markdown was
// written from. It is this package's own shape rather than the command's so
// that the schema an agent reads describes it in the words the tool uses.
type sourceBlock struct {
	ID      int64 `json:"id" jsonschema:"the block's id, as read_document reports it"`
	Version int64 `json:"version" jsonschema:"the version read_document reported for that block"`
}

type writeDocumentArgs struct {
	Document int64         `json:"document" jsonschema:"the document to write, as list_documents reports its id"`
	Text     string        `json:"text" jsonschema:"the whole document as markdown; it is cut into blocks at blank lines and headings, except inside a fenced code block"`
	Base     []sourceBlock `json:"base,omitempty" jsonschema:"the blocks read_document reported, in order, so a paragraph nobody touched keeps its block and a block somebody else changed is merged or reported; leave it out for the document as it stands now"`
	Key      string        `json:"key,omitempty" jsonschema:"an optional name for this change; calling again with the same key answers with what the first call did rather than writing a second time"`
}

// A conflictOut is a paragraph the write did not make: somebody else changed
// that block while the markdown was being written, and the two cannot be put
// together. The block is left holding what they wrote.
type conflictOut struct {
	Block   int64  `json:"block"`
	Version int64  `json:"version"`
	Current string `json:"current"`
}

type writeDocumentOut struct {
	// Base is the block each paragraph of the text just written now stands in,
	// in the order of the text, and is what the next write of this text sends
	// back as its own base. It is empty on a replayed call.
	Base      []sourceBlock `json:"base"`
	Conflicts []conflictOut `json:"conflicts"`
	// Merged is the blocks that took somebody else's words in on the way, so
	// what is stored there is neither this text nor theirs but both. The text
	// this call sent is out of date for those paragraphs, and sending it again
	// would write it back over the merge; read the document again to keep them.
	Merged []int64 `json:"merged"`
	// Replayed is a call answered out of the key it was sent under, having
	// written nothing because the first one did. Read the document again before
	// writing more: nothing remembers what the first answer said.
	Replayed bool `json:"replayed,omitempty"`
}

func (s *Server) writeDocument(ctx context.Context, req *sdk.CallToolRequest, in writeDocumentArgs) (*sdk.CallToolResult, writeDocumentOut, error) {
	service, who, err := s.writer(ctx, req)
	if err != nil {
		return nil, writeDocumentOut{}, err
	}
	ctx, err = keyed(ctx, in.Key)
	if err != nil {
		return nil, writeDocumentOut{}, err
	}
	// A base left out is the document as it stands, which the command reads
	// for itself inside the transaction that writes, so no agent has to send
	// one back to replace a document wholesale.
	var base []docs.BlockRef
	if in.Base != nil {
		base = make([]docs.BlockRef, 0, len(in.Base))
		for _, b := range in.Base {
			base = append(base, docs.BlockRef{ID: b.ID, Version: b.Version})
		}
	}
	save, err := service.WriteSource(ctx, who, in.Document, base, in.Text)
	if err != nil {
		return nil, writeDocumentOut{}, s.refusal("write the document", err)
	}
	out := writeDocumentOut{Base: []sourceBlock{}, Conflicts: []conflictOut{},
		Merged: save.Merged, Replayed: save.Replayed}
	for _, b := range save.Base {
		out.Base = append(out.Base, sourceBlock{ID: b.ID, Version: b.Version})
	}
	for _, c := range save.Conflicts {
		out.Conflicts = append(out.Conflicts, conflictOut{Block: c.Block, Version: c.Version, Current: c.Current})
	}
	return nil, out, nil
}

type replaceBlockArgs struct {
	Block       int64  `json:"block" jsonschema:"the block to replace, as read_document reports its id"`
	Text        string `json:"text" jsonschema:"the markdown to put in it"`
	BaseVersion int64  `json:"base_version,omitempty" jsonschema:"the version read_document reported, so a change somebody else made is merged in; leave it out to overwrite"`
}

func (s *Server) replaceBlock(ctx context.Context, req *sdk.CallToolRequest, in replaceBlockArgs) (*sdk.CallToolResult, writeOut, error) {
	service, who, err := s.writer(ctx, req)
	if err != nil {
		return nil, writeOut{}, err
	}
	base := in.BaseVersion
	if base == 0 {
		// No version means overwrite: the caller is told what is there and
		// writes over it rather than merging with it.
		current, err := docs.GetBlock(ctx, service.DB, in.Block)
		if err != nil {
			return nil, writeOut{}, s.refusal("read the block", err)
		}
		base = current.Version
	}
	e, err := service.SetBlock(ctx, who, in.Block, base, in.Text, false)
	if err != nil {
		return nil, writeOut{}, s.refusal("replace the block", err)
	}
	block, err := docs.GetBlock(ctx, service.DB, e.EntityID)
	if err != nil {
		return nil, writeOut{}, s.failed("read the block back", err)
	}
	return nil, writeOut{ID: block.ID, Version: block.Version}, nil
}

// writer is the service and the actor a write is recorded as: the person whose
// token it is, with the client's name as via.
func (s *Server) writer(ctx context.Context, req *sdk.CallToolRequest) (*docs.Service, core.Actor, error) {
	p, err := principal(ctx, auth.ScopeWrite)
	if err != nil {
		return nil, core.Actor{}, err
	}
	service, err := s.documents()
	if err != nil {
		return nil, core.Actor{}, err
	}
	name := ""
	if info := req.ClientInfo(); info != nil {
		name = info.Name
	}
	return service, core.Actor{Kind: core.KindUser, ID: p.User.ID, Name: p.User.Name,
		Via: via(name, p)}, nil
}
