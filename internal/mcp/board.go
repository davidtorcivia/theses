package mcp

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davidtorcivia/theses/internal/api"
	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/docs"
	"github.com/davidtorcivia/theses/internal/files"
)

// Board adds the propositions, the board, the two writes the links and files
// tools were missing, the activity log and the backup. It is called once at
// startup beside Files, so that mcp.New stays the tools that need nothing but
// the database.
//
// backupNow is the archive, passed as a function rather than the backup service
// so that this package does not depend on it for one call.
func Board(s *Server, b *board.Service, svc *files.Service, backupNow func(context.Context) error) {
	t := &boardTools{Server: s, board: b, files: svc, backupNow: backupNow}

	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        "list_propositions",
		Description: "Lists the propositions this token's owner may read, with their number, status, episode and members.",
		Annotations: reads("List the propositions"),
	}, t.listPropositions)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        "get_proposition",
		Description: "Reads one proposition: its title, statement, blurb, status, episode, target date and members.",
		Annotations: reads("Read a proposition"),
	}, t.getProposition)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        "create_proposition",
		Description: "Starts a proposition with the workspace's default columns and returns its id.",
		Annotations: adds("Start a proposition"),
	}, t.createProposition)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        "set_status",
		Description: "Moves one proposition to another status, which is one of the words the workspace keeps in its settings.",
		Annotations: overwrites("Set a status"),
	}, t.setStatus)

	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        "list_cards",
		Description: "Lists the cards of one proposition with their column, assignees, checklist, notes and version.",
		Annotations: reads("List the cards"),
	}, t.listCards)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        "create_card",
		Description: "Adds a card at the end of one column and returns its id.",
		Annotations: adds("Add a card"),
	}, t.createCard)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        "move_card",
		Description: "Moves one card into a column on the same proposition, after another card or to the head of it.",
		Annotations: overwrites("Move a card"),
	}, t.moveCard)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        "assign_card",
		Description: "Puts somebody on a card, or takes them off it again.",
		Annotations: attaches("Assign a card"),
	}, t.assignCard)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        "complete_card",
		Description: "Marks one card done, or reopens it.",
		Annotations: overwrites("Complete a card"),
	}, t.completeCard)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        "comment",
		Description: "Writes a note on one card, which everybody watching the board sees arrive.",
		Annotations: adds("Write a note"),
	}, t.comment)

	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        "annotate_link",
		Description: "Changes what a saved link says about itself: its note, its kind and the question it belongs under.",
		Annotations: overwrites("Annotate a link"),
	}, t.annotateLink)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        "request_upload",
		Description: "Makes a file row and returns the presigned URLs to put the bytes in the bucket with.",
		Annotations: adds("Ask for an upload"),
	}, t.requestUpload)

	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        "activity",
		Description: "Reads the activity log after a sequence number, saying who made each change and what carried it.",
		Annotations: reads("Read the activity log"),
	}, t.activity)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        "backup_now",
		Description: "Starts one backup in the background and answers as soon as it has begun.",
		Annotations: adds("Back up now"),
	}, t.backup)

	// The three resources the plan names. One handler reads all of them,
	// because a template with a wildcard in the middle is matched by the
	// client's URI and not by the server's pattern, and parsing it here is what
	// keeps the three from disagreeing about what an id is.
	for _, r := range []*sdk.ResourceTemplate{
		{
			URITemplate: "theses://proposition/{id}",
			Name:        "proposition",
			Title:       "One proposition",
			Description: "A proposition, its status and schedule, and what is on its board.",
			MIMEType:    "text/plain; charset=utf-8",
		},
		{
			URITemplate: "theses://proposition/{id}/document/{slug}",
			Name:        "document",
			Title:       "One document",
			Description: "One of a proposition's documents as markdown.",
			MIMEType:    "text/markdown; charset=utf-8",
		},
		{
			URITemplate: "theses://proposition/{id}/links",
			Name:        "links",
			Title:       "The links of one proposition",
			Description: "Every link saved on a proposition with its citation, note and question.",
			MIMEType:    "text/markdown; charset=utf-8",
		},
	} {
		s.srv.AddResourceTemplate(r, t.resource)
	}
}

type boardTools struct {
	*Server
	board     *board.Service
	files     *files.Service
	backupNow func(context.Context) error
}

// person is who a tool call writes as: the person the token belongs to, with
// the client's name as via. Every write on every surface is attributed this
// way, so an agent's edit is that person's edit with the agent named beside it.
func person(req *sdk.CallToolRequest, p api.Principal) core.Actor {
	return core.Actor{Kind: core.KindUser, ID: p.User.ID, Name: p.User.Name, Via: actor(req, p).Via}
}

// reader is who a read is made as, which carries no via: nothing is recorded,
// and the service asks the same membership of it either way.
func reader(p api.Principal) core.Actor {
	return core.Actor{Kind: core.KindUser, ID: p.User.ID, Name: p.User.Name}
}

// questionOf is a link's question as the command takes it, where the empty
// string is how "no question" is written.
func questionOf(l files.Link) string {
	if l.Question == nil {
		return ""
	}
	return *l.Question
}

// writing is the principal and the actor for a tool that changes something,
// refused unless the token carries the scope.
func (t *boardTools) writing(ctx context.Context, req *sdk.CallToolRequest, scope string) (api.Principal, core.Actor, error) {
	p, err := principal(ctx, scope)
	if err != nil {
		return api.Principal{}, core.Actor{}, err
	}
	return p, person(req, p), nil
}

// wrote is the line the log keeps beside the activity row, for reading a write
// next to the connection that made it.
func (t *boardTools) wrote(req *sdk.CallToolRequest, tool string, id int64, p api.Principal, a core.Actor) {
	t.log.Info("mcp write", "tool", tool, "entity", id,
		"user", p.User.ID, "via", a.Via, "protocol", req.ProtocolVersion())
}

// Propositions.

type propositionsOut struct {
	Propositions []board.Proposition `json:"propositions"`
}

func (t *boardTools) listPropositions(ctx context.Context, req *sdk.CallToolRequest, _ noArgs) (*sdk.CallToolResult, propositionsOut, error) {
	p, err := principal(ctx, auth.ScopeRead)
	if err != nil {
		return nil, propositionsOut{}, err
	}
	list, err := t.api.Propositions(ctx, p)
	if err != nil {
		return nil, propositionsOut{}, t.refusal("list the propositions", err)
	}
	return nil, propositionsOut{Propositions: list}, nil
}

type propositionOut struct {
	Proposition board.Proposition `json:"proposition"`
}

func (t *boardTools) getProposition(ctx context.Context, req *sdk.CallToolRequest, in propositionArgs) (*sdk.CallToolResult, propositionOut, error) {
	p, err := principal(ctx, auth.ScopeRead)
	if err != nil {
		return nil, propositionOut{}, err
	}
	one, err := t.api.Proposition(ctx, p, in.Proposition)
	if err != nil {
		return nil, propositionOut{}, t.refusal("read the proposition", err)
	}
	return nil, propositionOut{Proposition: one}, nil
}

type createPropositionArgs struct {
	Title string `json:"title" jsonschema:"what the proposition is called"`
	Key   string `json:"key,omitempty" jsonschema:"an optional name for this change; calling again with the same key answers with what the first call did rather than making a second"`
}

func (t *boardTools) createProposition(ctx context.Context, req *sdk.CallToolRequest, in createPropositionArgs) (*sdk.CallToolResult, writeOut, error) {
	p, who, err := t.writing(ctx, req, auth.ScopeWrite)
	if err != nil {
		return nil, writeOut{}, err
	}
	ctx, err = keyed(ctx, in.Key)
	if err != nil {
		return nil, writeOut{}, err
	}
	e, err := t.board.CreateProposition(ctx, who, in.Title)
	if err != nil {
		return nil, writeOut{}, t.refusal("start the proposition", err)
	}
	t.wrote(req, "create_proposition", e.EntityID, p, who)
	return nil, writeOut{ID: e.EntityID}, nil
}

type setStatusArgs struct {
	Proposition int64  `json:"proposition" jsonschema:"the proposition's id"`
	Status      string `json:"status" jsonschema:"one of the workspace's statuses, as get_settings reports defaults.statuses"`
}

func (t *boardTools) setStatus(ctx context.Context, req *sdk.CallToolRequest, in setStatusArgs) (*sdk.CallToolResult, writeOut, error) {
	p, who, err := t.writing(ctx, req, auth.ScopeWrite)
	if err != nil {
		return nil, writeOut{}, err
	}
	e, err := t.board.SetStatus(ctx, who, in.Proposition, in.Status)
	if err != nil {
		return nil, writeOut{}, t.refusal("set the status", err)
	}
	t.wrote(req, "set_status", e.EntityID, p, who)
	return nil, writeOut{ID: e.EntityID}, nil
}

// The board.

type cardsOut struct {
	Columns []board.Column `json:"columns"`
	Cards   []board.Card   `json:"cards"`
	// Seq is the sequence number this reading is of, which the event stream
	// carries on from.
	Seq int64 `json:"seq"`
}

func (t *boardTools) listCards(ctx context.Context, req *sdk.CallToolRequest, in propositionArgs) (*sdk.CallToolResult, cardsOut, error) {
	p, err := principal(ctx, auth.ScopeRead)
	if err != nil {
		return nil, cardsOut{}, err
	}
	if err := t.api.Readable(ctx, p, in.Proposition); err != nil {
		return nil, cardsOut{}, t.refusal("read the board", err)
	}
	loaded, err := board.Load(ctx, t.board.DB, in.Proposition)
	if err != nil {
		return nil, cardsOut{}, t.refusal("read the board", err)
	}
	return nil, cardsOut{Columns: loaded.Columns, Cards: loaded.Cards, Seq: loaded.Seq}, nil
}

type createCardArgs struct {
	Column    int64   `json:"column" jsonschema:"the column to add it to, as list_cards reports the columns"`
	Title     string  `json:"title" jsonschema:"one line saying what the card is"`
	Assignees []int64 `json:"assignees,omitempty" jsonschema:"the ids of the people to put on it, as list_users reports them"`
	Key       string  `json:"key,omitempty" jsonschema:"an optional name for this change; calling again with the same key answers with what the first call did rather than making a second"`
}

func (t *boardTools) createCard(ctx context.Context, req *sdk.CallToolRequest, in createCardArgs) (*sdk.CallToolResult, writeOut, error) {
	p, who, err := t.writing(ctx, req, auth.ScopeWrite)
	if err != nil {
		return nil, writeOut{}, err
	}
	ctx, err = keyed(ctx, in.Key)
	if err != nil {
		return nil, writeOut{}, err
	}
	e, err := t.board.CreateCard(ctx, who, in.Column, in.Title, in.Assignees)
	if err != nil {
		return nil, writeOut{}, t.refusal("add the card", err)
	}
	t.wrote(req, "create_card", e.EntityID, p, who)
	return nil, writeOut{ID: e.EntityID}, nil
}

type moveCardArgs struct {
	Card   int64 `json:"card" jsonschema:"the card's id"`
	Column int64 `json:"column" jsonschema:"the column to move it into, on the same proposition"`
	After  int64 `json:"after,omitempty" jsonschema:"the card to put it after, or leave it out for the head of the column"`
}

func (t *boardTools) moveCard(ctx context.Context, req *sdk.CallToolRequest, in moveCardArgs) (*sdk.CallToolResult, writeOut, error) {
	p, who, err := t.writing(ctx, req, auth.ScopeWrite)
	if err != nil {
		return nil, writeOut{}, err
	}
	e, err := t.board.MoveCard(ctx, who, in.Card, in.Column, in.After)
	if err != nil {
		return nil, writeOut{}, t.refusal("move the card", err)
	}
	t.wrote(req, "move_card", e.EntityID, p, who)
	return nil, writeOut{ID: e.EntityID}, nil
}

type assignCardArgs struct {
	Card     int64 `json:"card" jsonschema:"the card's id"`
	User     int64 `json:"user" jsonschema:"the person's id, as list_users reports it"`
	Unassign bool  `json:"unassign,omitempty" jsonschema:"true to take them off the card instead"`
}

func (t *boardTools) assignCard(ctx context.Context, req *sdk.CallToolRequest, in assignCardArgs) (*sdk.CallToolResult, writeOut, error) {
	p, who, err := t.writing(ctx, req, auth.ScopeWrite)
	if err != nil {
		return nil, writeOut{}, err
	}
	run := t.board.AssignCard
	if in.Unassign {
		run = t.board.UnassignCard
	}
	e, err := run(ctx, who, in.Card, in.User)
	if err != nil {
		return nil, writeOut{}, t.refusal("change who is on the card", err)
	}
	t.wrote(req, "assign_card", e.EntityID, p, who)
	return nil, writeOut{ID: e.EntityID}, nil
}

type completeCardArgs struct {
	Card   int64 `json:"card" jsonschema:"the card's id"`
	Reopen bool  `json:"reopen,omitempty" jsonschema:"true to reopen it instead of marking it done"`
}

func (t *boardTools) completeCard(ctx context.Context, req *sdk.CallToolRequest, in completeCardArgs) (*sdk.CallToolResult, writeOut, error) {
	p, who, err := t.writing(ctx, req, auth.ScopeWrite)
	if err != nil {
		return nil, writeOut{}, err
	}
	e, err := t.board.SetCardDone(ctx, who, in.Card, !in.Reopen)
	if err != nil {
		return nil, writeOut{}, t.refusal("mark the card", err)
	}
	t.wrote(req, "complete_card", e.EntityID, p, who)
	return nil, writeOut{ID: e.EntityID}, nil
}

type commentArgs struct {
	Card int64  `json:"card" jsonschema:"the card to write on"`
	Text string `json:"text" jsonschema:"the markdown of the note; an @handle in it mentions that person"`
	Key  string `json:"key,omitempty" jsonschema:"an optional name for this change; calling again with the same key answers with what the first call did rather than making a second"`
}

func (t *boardTools) comment(ctx context.Context, req *sdk.CallToolRequest, in commentArgs) (*sdk.CallToolResult, writeOut, error) {
	p, who, err := t.writing(ctx, req, auth.ScopeWrite)
	if err != nil {
		return nil, writeOut{}, err
	}
	ctx, err = keyed(ctx, in.Key)
	if err != nil {
		return nil, writeOut{}, err
	}
	e, err := t.board.PostComment(ctx, who, in.Card, in.Text)
	if err != nil {
		return nil, writeOut{}, t.refusal("write the note", err)
	}
	t.wrote(req, "comment", e.EntityID, p, who)
	return nil, writeOut{ID: e.EntityID}, nil
}

// Links and files.

type annotateLinkArgs struct {
	Link     int64   `json:"link" jsonschema:"the link's id, as list_links reports it"`
	Note     *string `json:"note,omitempty" jsonschema:"why this matters for the episode; leave it out to keep what is there and send an empty string to clear it"`
	Kind     *string `json:"kind,omitempty" jsonschema:"one of the kinds list_links reports; leave it out to keep what is there"`
	Question *string `json:"question,omitempty" jsonschema:"one of I, II, III or IV, an empty string for none, or leave it out to keep what is there"`
}

func (t *boardTools) annotateLink(ctx context.Context, req *sdk.CallToolRequest, in annotateLinkArgs) (*sdk.CallToolResult, files.Link, error) {
	p, who, err := t.writing(ctx, req, auth.ScopeWrite)
	if err != nil {
		return nil, files.Link{}, err
	}
	was, err := t.files.ReadLink(ctx, who, in.Link)
	if err != nil {
		return nil, files.Link{}, t.refusal("read the link", err)
	}
	// The command takes the whole set, because that is what a form posts, so
	// the row is read first and the call laid over it: naming the note is not a
	// way to clear the title.
	edit := files.Edit{
		Title: was.Title, Author: was.Author, Year: was.Year,
		Kind: was.Kind, Note: was.Note, Question: questionOf(was),
	}
	for _, field := range []struct{ into, from *string }{
		{&edit.Note, in.Note}, {&edit.Kind, in.Kind}, {&edit.Question, in.Question},
	} {
		if field.from != nil {
			*field.into = *field.from
		}
	}
	if _, err := t.files.EditLink(ctx, who, was.ID, edit); err != nil {
		return nil, files.Link{}, t.refusal("annotate the link", err)
	}
	t.wrote(req, "annotate_link", was.ID, p, who)
	now, err := t.files.ReadLink(ctx, who, was.ID)
	if err != nil {
		return nil, files.Link{}, t.refusal("read the link back", err)
	}
	return nil, now, nil
}

type requestUploadArgs struct {
	Proposition int64  `json:"proposition" jsonschema:"the proposition to put the file on"`
	Name        string `json:"name" jsonschema:"the file name, with its extension"`
	Folder      string `json:"folder" jsonschema:"one of the folders list_files reports"`
	Size        int64  `json:"size" jsonschema:"how many bytes the object will be, which the bucket is checked against on completion"`
	Replace     int64  `json:"replace,omitempty" jsonschema:"the id of a file this is a new version of, or leave it out to keep both"`
}

func (t *boardTools) requestUpload(ctx context.Context, req *sdk.CallToolRequest, in requestUploadArgs) (*sdk.CallToolResult, files.Upload, error) {
	p, who, err := t.writing(ctx, req, auth.ScopeFiles)
	if err != nil {
		return nil, files.Upload{}, err
	}
	up, err := t.files.Create(ctx, who, in.Proposition, in.Name, in.Folder, in.Size, in.Replace)
	if err != nil {
		return nil, files.Upload{}, t.refusal("ask for the upload", err)
	}
	t.wrote(req, "request_upload", up.File.ID, p, who)
	return nil, up, nil
}

// Ops.

type activityArgs struct {
	Since int64 `json:"since,omitempty" jsonschema:"the id of the last row you have seen, or leave it out for the beginning"`
	Limit int   `json:"limit,omitempty" jsonschema:"how many rows, 50 by default and 200 at most"`
}

type activityOut struct {
	Activity []api.ActivityView `json:"activity"`
}

func (t *boardTools) activity(ctx context.Context, req *sdk.CallToolRequest, in activityArgs) (*sdk.CallToolResult, activityOut, error) {
	p, err := principal(ctx, auth.ScopeRead)
	if err != nil {
		return nil, activityOut{}, err
	}
	rows, err := t.api.Activity(ctx, p, in.Since, in.Limit)
	if err != nil {
		return nil, activityOut{}, t.failed("read the activity log", err)
	}
	return nil, activityOut{Activity: rows}, nil
}

type backupOut struct {
	Started bool `json:"started"`
}

// backup starts an archive and says so. The archive outlives the call, so what
// it did is read from the settings page or from the log rather than from here.
//
// ponytail: the activity row a backup writes is the scheduler's, so a backup
// started from here is not attributed to the person whose token asked for it;
// the log line below is. Upgrade path: an actor argument on backup.Now.
func (t *boardTools) backup(ctx context.Context, req *sdk.CallToolRequest, _ noArgs) (*sdk.CallToolResult, backupOut, error) {
	p, who, err := t.writing(ctx, req, auth.ScopeAdmin)
	if err != nil {
		return nil, backupOut{}, err
	}
	if t.backupNow == nil {
		return nil, backupOut{}, errors.New("this endpoint has no backups")
	}
	if err := t.backupNow(ctx); err != nil {
		// Whatever the backup says about itself is the caller's to read: it is
		// either busy or not configured, and both are acted on.
		return nil, backupOut{}, err
	}
	t.wrote(req, "backup_now", 0, p, who)
	return nil, backupOut{Started: true}, nil
}

// Resources.

const propositionURI = "theses://proposition/"

// resource reads the three proposition resources. The URI is parsed here rather
// than by the template, because a wildcard followed by more path is matched by
// what the client asked for and not by what the server registered.
func (t *boardTools) resource(ctx context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
	p, err := principal(ctx, auth.ScopeRead)
	if err != nil {
		return nil, err
	}
	rest, found := strings.CutPrefix(req.Params.URI, propositionURI)
	if !found {
		return nil, fmt.Errorf("there is no resource at %s", req.Params.URI)
	}
	head, tail, _ := strings.Cut(rest, "/")
	id, err := strconv.ParseInt(head, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%q is not a proposition id", head)
	}

	var text, kind string
	switch slug, document := strings.CutPrefix(tail, "document/"); {
	case tail == "":
		text, kind, err = t.propositionText(ctx, p, id)
	case tail == "links":
		text, kind, err = t.linksText(ctx, p, id)
	case document:
		text, kind, err = t.documentText(ctx, p, id, slug)
	default:
		return nil, fmt.Errorf("there is no resource at %s", req.Params.URI)
	}
	if err != nil {
		return nil, err
	}
	return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{
		URI: req.Params.URI, MIMEType: kind, Text: text,
	}}}, nil
}

const (
	asText     = "text/plain; charset=utf-8"
	asMarkdown = "text/markdown; charset=utf-8"
)

func (t *boardTools) propositionText(ctx context.Context, p api.Principal, id int64) (string, string, error) {
	one, err := t.api.Proposition(ctx, p, id)
	if err != nil {
		return "", "", t.refusal("read the proposition", err)
	}
	loaded, err := board.Load(ctx, t.board.DB, id)
	if err != nil {
		return "", "", t.failed("read the board", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d. %s\nStatus: %s\n", one.Number, one.Title, one.Status)
	if one.Episode != nil {
		fmt.Fprintf(&b, "Episode: %s\n", *one.Episode)
	}
	if one.TargetDate != nil {
		fmt.Fprintf(&b, "Target: %s\n", *one.TargetDate)
	}
	if one.ArchivedAt != nil {
		b.WriteString("Archived, and read only until it is restored.\n")
	}
	if one.Statement != "" {
		fmt.Fprintf(&b, "\n%s\n", one.Statement)
	}
	if one.Blurb != "" {
		fmt.Fprintf(&b, "\n%s\n", one.Blurb)
	}
	byColumn := map[int64][]board.Card{}
	for _, c := range loaded.Cards {
		byColumn[c.ColumnID] = append(byColumn[c.ColumnID], c)
	}
	for _, col := range loaded.Columns {
		fmt.Fprintf(&b, "\n%s\n", col.Name)
		if len(byColumn[col.ID]) == 0 {
			b.WriteString("  nothing yet\n")
		}
		for _, c := range byColumn[col.ID] {
			done := " "
			if c.DoneAt != nil {
				done = "x"
			}
			fmt.Fprintf(&b, "  [%s] %d %s\n", done, c.ID, c.Title)
		}
	}
	return b.String(), asText, nil
}

func (t *boardTools) documentText(ctx context.Context, p api.Principal, id int64, slug string) (string, string, error) {
	if t.api.Docs == nil {
		return "", "", errors.New("this endpoint has no documents")
	}
	// Documents asks the membership itself, so a proposition this token's owner
	// may not read answers before the slug is looked at.
	list, err := t.api.Docs.Documents(ctx, p.User, id)
	if err != nil {
		return "", "", t.refusal("read the documents", err)
	}
	for _, d := range list {
		if d.Slug == slug {
			return docs.Markdown(d.Blocks), asMarkdown, nil
		}
	}
	return "", "", fmt.Errorf("that proposition has no document called %q", slug)
}

func (t *boardTools) linksText(ctx context.Context, p api.Principal, id int64) (string, string, error) {
	rows, err := t.files.ListLinks(ctx, reader(p), id)
	if err != nil {
		return "", "", t.refusal("read the links", err)
	}
	var b strings.Builder
	if len(rows) == 0 {
		b.WriteString("No links on this proposition yet.\n")
	}
	for _, l := range rows {
		fmt.Fprintf(&b, "- [%s](%s)\n", title(l), l.URL)
		if l.Citation != "" {
			fmt.Fprintf(&b, "  %s\n", l.Citation)
		}
		if q := questionOf(l); q != "" {
			fmt.Fprintf(&b, "  Question %s\n", q)
		}
		if l.Note != "" {
			fmt.Fprintf(&b, "  %s\n", l.Note)
		}
	}
	return b.String(), asMarkdown, nil
}

// title is what a link is called, falling back to its address when the page
// gave nothing to call it.
func title(l files.Link) string {
	if l.Title != "" {
		return l.Title
	}
	return l.URL
}
