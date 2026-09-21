// Package mcp is the MCP server at /mcp: the same bearer tokens and scopes as
// the REST API, over streamable HTTP, with the tools an agent needs to see the
// workspace and change what its token allows.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davidtorcivia/theses/internal/api"
	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/search"
	"github.com/davidtorcivia/theses/internal/settings"
	"github.com/davidtorcivia/theses/internal/store"
)

// WorkspaceURI is the one resource: what this workspace is and what is in it.
const WorkspaceURI = "theses://workspace"

type Server struct {
	api *api.API
	db  *store.DB
	set *settings.Settings
	log *slog.Logger
	srv *sdk.Server
}

// New builds the server and registers its tools. Descriptions are one sentence
// each: they are read by a model, not by a person with the manual open.
func New(a *api.API, db *store.DB, set *settings.Settings, log *slog.Logger, version string) *Server {
	s := &Server{api: a, db: db, set: set, log: log}
	s.srv = sdk.NewServer(&sdk.Implementation{
		Name:    "theses",
		Title:   "THESES",
		Version: version,
	}, nil)

	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        "whoami",
		Description: "Reports the API token this connection is using and the person it belongs to.",
		Annotations: reads("Who am I"),
	}, s.whoami)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        "search",
		Description: "Searches cards, document blocks, links, files, comments, propositions and people for a phrase.",
		Annotations: reads("Search the workspace"),
	}, s.search)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        "list_users",
		Description: "Lists everyone in the workspace with their handle, role and color.",
		Annotations: reads("List the people"),
	}, s.listUsers)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        "get_settings",
		Description: "Lists the workspace settings and their values, with stored secrets reported as set rather than returned.",
		Annotations: reads("Read the settings"),
	}, s.getSettings)
	sdk.AddTool(s.srv, &sdk.Tool{
		Name:        "set_setting",
		Description: "Changes one workspace setting, taking one line per entry for a setting that holds a list.",
		Annotations: overwrites("Change a setting"),
	}, s.setSetting)
	s.addDocumentTools()

	s.srv.AddResource(&sdk.Resource{
		URI:         WorkspaceURI,
		Name:        "workspace",
		Title:       "This workspace",
		Description: "What this THESES workspace is, how much is in it, and what this endpoint can do.",
		MIMEType:    "text/plain; charset=utf-8",
	}, s.workspace)

	return s
}

// MaxBodyBytes is what a POST to /mcp may be, the same as the REST API allows,
// since a tool call is a few hundred bytes of JSON.
const MaxBodyBytes = 64 << 10

// keyed puts a tool call's key in the context. The tools that make something
// take one, because an agent that never saw the answer to a call cannot tell a
// request that was lost from one that was applied, and calling it again under
// the same key makes one card rather than two. The tools that set a field take
// none: setting it twice sets it to what it already holds.
//
// The SDK has no per call place of its own for this, so it is an argument like
// any other, which is also what puts it in the tool's schema for a model to
// read about and use.
func keyed(ctx context.Context, key string) (context.Context, error) {
	if key == "" {
		return ctx, nil
	}
	return core.WithKey(ctx, key)
}

// reads and overwrites are the hints a client shows before it runs a tool. Four
// of these tools only look; the fifth replaces a value that was there, which is
// destructive in the sense the annotation means, and repeating it with the same
// arguments changes nothing further.
func reads(title string) *sdk.ToolAnnotations {
	no := false
	return &sdk.ToolAnnotations{Title: title, ReadOnlyHint: true, OpenWorldHint: &no}
}

func overwrites(title string) *sdk.ToolAnnotations {
	yes, no := true, false
	return &sdk.ToolAnnotations{
		Title:           title,
		DestructiveHint: &yes,
		IdempotentHint:  true,
		OpenWorldHint:   &no,
	}
}

// Handler is /mcp, behind the same bearer tokens as the REST API. The server is
// stateless: every POST carries its own Authorization header and is
// authenticated on its own, rather than trusting the session an initialize
// request opened.
//
// The SDK's rebinding protection is off because it refuses a request that
// arrives over loopback with a public Host header, which is every request in
// this deployment: a reverse proxy on the same host proxies to 127.0.0.1. The
// protection it offers is against a browser reaching a local server that
// answers whoever asks, and this one answers a bearer token it checks per
// request.
func (s *Server) Handler() http.Handler {
	return s.api.Authenticate(sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return s.srv },
		&sdk.StreamableHTTPOptions{
			Stateless:                  true,
			Logger:                     s.log,
			DisableLocalhostProtection: true,
			MaxRequestBodyBytes:        MaxBodyBytes,
		},
	))
}

// principal is the token this call arrived with, refused unless it has the
// scope the tool needs. The context is the one the transport was connected
// with, which for a stateless HTTP session is the request Authenticate handled.
func principal(ctx context.Context, scope string) (api.Principal, error) {
	p, ok := api.PrincipalFrom(ctx)
	if !ok {
		return api.Principal{}, errors.New("this connection is not authenticated")
	}
	if scope != "" {
		if why := p.Deny(scope); why != "" {
			return api.Principal{}, errors.New(why)
		}
	}
	return p, nil
}

// actor is how a write through MCP is recorded: the person whose token it is
// using, with the client's name as via.
//
// ponytail: a stateless session has no initialize parameters of its own, so a
// client on a protocol older than 2026-07-28 does not name itself on each call
// and the token's name stands in for it. Upgrade path: take the name from the
// session once /mcp keeps sessions.
func actor(req *sdk.CallToolRequest, p api.Principal) settings.Actor {
	name := ""
	if info := req.ClientInfo(); info != nil {
		name = info.Name
	}
	a := p.Actor()
	a.Via = via(name, p)
	return a
}

// via is the client's name, or the token's when the client did not give one.
func via(name string, p api.Principal) string {
	if name = strings.TrimSpace(name); name == "" {
		name = p.Token.Name
	}
	return api.ClientVia(name)
}

// noArgs is a tool that takes nothing. The SDK builds the schema from it.
type noArgs struct{}

func (s *Server) whoami(ctx context.Context, req *sdk.CallToolRequest, _ noArgs) (*sdk.CallToolResult, api.MeView, error) {
	p, err := principal(ctx, "")
	if err != nil {
		return nil, api.MeView{}, err
	}
	return nil, p.Me(), nil
}

type searchArgs struct {
	Query string `json:"query" jsonschema:"what to search for"`
	Limit int    `json:"limit,omitempty" jsonschema:"how many hits per kind, 10 by default and 50 at most"`
}

type searchOut struct {
	Query  string         `json:"query"`
	Groups []search.Group `json:"groups"`
}

func (s *Server) search(ctx context.Context, req *sdk.CallToolRequest, in searchArgs) (*sdk.CallToolResult, searchOut, error) {
	p, err := principal(ctx, auth.ScopeRead)
	if err != nil {
		return nil, searchOut{}, err
	}
	groups, err := search.Search(ctx, s.db, in.Query, in.Limit, s.api.Reader(p))
	if err != nil {
		return nil, searchOut{}, s.failed("search", err)
	}
	if groups == nil {
		groups = []search.Group{}
	}
	return nil, searchOut{Query: in.Query, Groups: groups}, nil
}

type usersOut struct {
	Users []api.UserView `json:"users"`
}

func (s *Server) listUsers(ctx context.Context, req *sdk.CallToolRequest, _ noArgs) (*sdk.CallToolResult, usersOut, error) {
	if _, err := principal(ctx, auth.ScopeRead); err != nil {
		return nil, usersOut{}, err
	}
	users, err := s.api.UserViews(ctx)
	if err != nil {
		return nil, usersOut{}, s.failed("list users", err)
	}
	return nil, usersOut{Users: users}, nil
}

type settingsOut struct {
	Settings []api.SettingView `json:"settings"`
}

func (s *Server) getSettings(ctx context.Context, req *sdk.CallToolRequest, _ noArgs) (*sdk.CallToolResult, settingsOut, error) {
	if _, err := principal(ctx, auth.ScopeAdmin); err != nil {
		return nil, settingsOut{}, err
	}
	return nil, settingsOut{Settings: s.api.SettingViews()}, nil
}

type setSettingArgs struct {
	Key   string `json:"key" jsonschema:"the settings key, as get_settings reports it"`
	Value string `json:"value" jsonschema:"the new value, one line per entry for a setting that holds a list"`
}

func (s *Server) setSetting(ctx context.Context, req *sdk.CallToolRequest, in setSettingArgs) (*sdk.CallToolResult, api.SettingView, error) {
	p, err := principal(ctx, auth.ScopeAdmin)
	if err != nil {
		return nil, api.SettingView{}, err
	}
	def, ok := settings.Lookup(in.Key)
	if !ok {
		return nil, api.SettingView{}, fmt.Errorf("there is no setting called %q", in.Key)
	}
	who := actor(req, p)
	// The activity row carries the via as well now; this is the same line in
	// the log, for reading a write next to the connection that made it.
	s.log.Info("mcp write", "tool", "set_setting", "key", def.Key,
		"user", p.User.ID, "via", who.Via, "protocol", req.ProtocolVersion())
	if err := s.set.SetAs(ctx, def.Key, []string{in.Value}, who); err != nil {
		if errors.Is(err, settings.ErrStorage) {
			return nil, api.SettingView{}, s.failed("save the setting", err)
		}
		return nil, api.SettingView{}, err
	}
	return nil, s.api.Describe(def), nil
}

// workspace is the resource: enough for a client to know where it has connected
// and what it may ask for next.
func (s *Server) workspace(ctx context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
	p, err := principal(ctx, auth.ScopeRead)
	if err != nil {
		return nil, err
	}
	var people int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&people); err != nil {
		return nil, s.failed("read the workspace", err)
	}
	visible, err := s.api.Propositions(ctx, p)
	if err != nil {
		return nil, s.failed("read the workspace", err)
	}
	propositions := 0
	for _, proposition := range visible {
		if proposition.Kind != "show" && proposition.ArchivedAt == nil {
			propositions++
		}
	}
	text := fmt.Sprintf(`%s is a THESES workspace: one board, one document set,
one link list and one file list per proposition, and an activity log of every
change. It holds %s and %s that are not archived, and its time zone is %s.

This endpoint reads and writes it as the token you connected with allows: read
to search and to list, admin to read and change settings. The same workspace is
served over REST at /api/v1.`,
		settings.Get[string](s.set, "workspace.name"),
		plural(people, "person", "people"),
		plural(propositions, "proposition", "propositions"),
		settings.Get[string](s.set, "workspace.timezone"))

	return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{
		URI:      WorkspaceURI,
		MIMEType: "text/plain; charset=utf-8",
		Text:     text,
	}}}, nil
}

// plural counts a thing in words a reader expects: one person, two people.
func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// failed logs a fault on this side and tells the client only that it happened,
// the same way the REST API answers a 500.
func (s *Server) failed(what string, err error) error {
	s.log.Error("mcp call failed", "what", what, "err", err)
	return fmt.Errorf("could not %s", what)
}
