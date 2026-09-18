package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davidtorcivia/theses/internal/api"
	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/settings"
	"github.com/davidtorcivia/theses/internal/store"
)

// clientName is what the test client calls itself, and so what a write it makes
// should be recorded as.
const clientName = "research agent"

type harness struct {
	*testing.T
	db   *store.DB
	set  *settings.Settings
	auth *auth.Auth
	srv  *Server
	user *store.User
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	db := store.OpenTemp(t)
	set, err := settings.Open(ctx, db, []byte("a secret key of at least thirty-two bytes"))
	if err != nil {
		t.Fatal(err)
	}
	a := auth.New(db, []byte("a session key of at least thirty-two bytes"), false, false)
	id, err := store.CreateUser(ctx, db, &store.User{
		Handle: "nora", Email: "nora@example.com", Name: "Nora Vance",
		Initials: "NV", Colour: "#b45", Role: "owner", PasswordHash: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	user, err := store.UserByID(ctx, db, id)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	restAPI := api.New(db, a, set, log)
	return &harness{T: t, db: db, set: set, auth: a, user: user,
		srv: New(restAPI, db, set, log, "test")}
}

// connect opens a session whose token has the given scopes, the way the HTTP
// handler does: the principal is in the context the transport is connected with.
func (h *harness) clearToken(scopes ...string) string {
	h.Helper()
	clear, err := h.auth.CreateAPIToken(context.Background(), h.user.ID, strings.Join(scopes, "-"), scopes)
	if err != nil {
		h.Fatal(err)
	}
	return clear
}

func (h *harness) connect(scopes ...string) *sdk.ClientSession {
	h.Helper()
	ctx := context.Background()
	token, user, err := h.auth.APIToken(ctx, h.clearToken(scopes...))
	if err != nil {
		h.Fatal(err)
	}

	clientTransport, serverTransport := sdk.NewInMemoryTransports()
	serverCtx := api.WithPrincipal(ctx, api.Principal{Token: token, User: user})
	ss, err := h.srv.srv.Connect(serverCtx, serverTransport, nil)
	if err != nil {
		h.Fatal(err)
	}
	h.Cleanup(func() { ss.Close() })

	client := sdk.NewClient(&sdk.Implementation{Name: clientName, Version: "test"}, nil)
	cs, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		h.Fatal(err)
	}
	h.Cleanup(func() { cs.Close() })
	return cs
}

// call runs one tool and decodes its structured output into out.
func (h *harness) call(cs *sdk.ClientSession, name string, args any, out any) *sdk.CallToolResult {
	h.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		h.Fatalf("%s: %v", name, err)
	}
	if res.IsError || out == nil {
		return res
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		h.Fatal(err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		h.Fatalf("%s returned %s: %v", name, b, err)
	}
	return res
}

func TestToolsAreListedWithOneSentenceEach(t *testing.T) {
	h := newHarness(t)
	cs := h.connect(auth.ScopeRead)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"whoami": true, "search": true, "list_users": true,
		"get_settings": true, "set_setting": true}
	for _, tool := range res.Tools {
		if !want[tool.Name] {
			t.Errorf("unexpected tool %q", tool.Name)
		}
		delete(want, tool.Name)
		if !strings.HasSuffix(tool.Description, ".") || strings.Count(tool.Description, ".") != 1 {
			t.Errorf("%s: description is not one sentence: %q", tool.Name, tool.Description)
		}
	}
	if len(want) != 0 {
		t.Errorf("missing tools: %v", want)
	}
}

func TestWhoamiReportsTheToken(t *testing.T) {
	h := newHarness(t)
	var me api.MeView
	h.call(h.connect(auth.ScopeRead, auth.ScopeWrite), "whoami", noArgs{}, &me)
	if me.Token.Name != "read-write" || len(me.Token.Scopes) != 2 {
		t.Errorf("token = %+v", me.Token)
	}
	if me.User.Handle != "nora" || me.User.Role != "owner" {
		t.Errorf("user = %+v", me.User)
	}
}

func TestSearchFindsAnInsertedRow(t *testing.T) {
	h := newHarness(t)
	if _, err := h.db.ExecContext(context.Background(), `INSERT INTO propositions
		(id, number, title, statement, status, position, created_at)
		VALUES (1, 10, 'Student debt is a policy choice', 'It was designed', 'idea', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	var out searchOut
	h.call(h.connect(auth.ScopeRead), "search", searchArgs{Query: "debt"}, &out)
	if len(out.Groups) != 1 || len(out.Groups[0].Hits) != 1 {
		t.Fatalf("groups = %+v", out.Groups)
	}
	if hit := out.Groups[0].Hits[0]; hit.Title != "Student debt is a policy choice" {
		t.Errorf("hit = %+v", hit)
	}
}

func TestListUsersNeedsRead(t *testing.T) {
	h := newHarness(t)
	if res := h.call(h.connect(auth.ScopeFiles), "list_users", noArgs{}, nil); !res.IsError {
		t.Error("a files token listed the workspace")
	}
	var out usersOut
	h.call(h.connect(auth.ScopeRead), "list_users", noArgs{}, &out)
	if len(out.Users) != 1 || out.Users[0].Handle != "nora" {
		t.Errorf("users = %+v", out.Users)
	}
}

func TestSetSettingNeedsAdminAndIsRecordedAsTheClient(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	res := h.call(h.connect(auth.ScopeRead, auth.ScopeWrite), "set_setting",
		setSettingArgs{Key: "workspace.name", Value: "Debt Machine"}, nil)
	if !res.IsError {
		t.Fatal("a write token changed a setting")
	}
	if got := settings.Get[string](h.set, "workspace.name"); got != "We All Fall Down" {
		t.Errorf("the setting changed anyway: %q", got)
	}

	var view api.SettingView
	h.call(h.connect(auth.ScopeAdmin), "set_setting",
		setSettingArgs{Key: "workspace.name", Value: "Debt Machine"}, &view)
	if view.Value != "Debt Machine" {
		t.Errorf("view = %+v", view)
	}
	if got := settings.Get[string](h.set, "workspace.name"); got != "Debt Machine" {
		t.Errorf("stored workspace.name = %q", got)
	}

	var kind, id string
	if err := h.db.QueryRowContext(ctx, `SELECT actor_kind, actor_id FROM activity
		WHERE entity_id = 'workspace.name'`).Scan(&kind, &id); err != nil {
		t.Fatal(err)
	}
	if want := strconv.FormatInt(h.user.ID, 10); kind != "user" || id != want {
		t.Errorf("activity actor = %s %s, want user %s", kind, id, want)
	}
}

// The client names itself, and that name is the via the activity row carries
// once the schema has a column for it. A client that gives no name, which is what a
// stateless session sees on the older protocol, is named by its token instead.
func TestViaNamesTheClientOrElseTheToken(t *testing.T) {
	h := newHarness(t)
	token, user, err := h.auth.APIToken(context.Background(), h.clearToken(auth.ScopeAdmin))
	if err != nil {
		t.Fatal(err)
	}
	p := api.Principal{Token: token, User: user}
	if got := via(clientName, p); got != "mcp:research agent" {
		t.Errorf("via = %q", got)
	}
	if got := via("  ", p); got != "mcp:admin" {
		t.Errorf("via without a client name = %q", got)
	}
}

func TestGetSettingsReturnsNoSecret(t *testing.T) {
	h := newHarness(t)
	if err := h.set.Set(context.Background(), "mail.password",
		[]string{"hunter2-but-longer"}, h.user.ID); err != nil {
		t.Fatal(err)
	}
	if res := h.call(h.connect(auth.ScopeRead), "get_settings", noArgs{}, nil); !res.IsError {
		t.Error("a read token read the settings")
	}

	var out settingsOut
	res := h.call(h.connect(auth.ScopeAdmin), "get_settings", noArgs{}, &out)
	for _, c := range res.Content {
		if text, ok := c.(*sdk.TextContent); ok && strings.Contains(text.Text, "hunter2") {
			t.Fatal("a secret was returned")
		}
	}
	var seen bool
	for _, s := range out.Settings {
		if s.Key == "mail.password" {
			seen = true
			if !s.Secret || !s.Set || s.Value != nil {
				t.Errorf("mail.password = %+v", s)
			}
		}
	}
	if !seen {
		t.Error("mail.password is missing from the listing")
	}
}

func TestUnknownSettingIsAToolError(t *testing.T) {
	h := newHarness(t)
	res := h.call(h.connect(auth.ScopeAdmin), "set_setting",
		setSettingArgs{Key: "not.a.key", Value: "x"}, nil)
	if !res.IsError {
		t.Error("an unknown key was accepted")
	}
}

func TestWorkspaceResourceDescribesTheWorkspace(t *testing.T) {
	h := newHarness(t)
	cs := h.connect(auth.ScopeRead)
	res, err := cs.ReadResource(context.Background(), &sdk.ReadResourceParams{URI: WorkspaceURI})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Contents) != 1 {
		t.Fatalf("contents = %+v", res.Contents)
	}
	text := res.Contents[0].Text
	if !strings.Contains(text, "We All Fall Down") || !strings.Contains(text, "1 person") {
		t.Errorf("resource text = %q", text)
	}

	if _, err := h.connect(auth.ScopeFiles).ReadResource(context.Background(),
		&sdk.ReadResourceParams{URI: WorkspaceURI}); err == nil {
		t.Error("a files token read the workspace resource")
	}
}

// A token may not do what the person it belongs to may not do. The principal is
// resolved when the transport is connected, which over stateless HTTP is once
// per request, so the demotion here is made before connecting.
func TestAToolCannotOutrankTheTokenOwner(t *testing.T) {
	h := newHarness(t)
	if _, err := h.db.ExecContext(context.Background(),
		`UPDATE users SET role = 'guest' WHERE id = ?`, h.user.ID); err != nil {
		t.Fatal(err)
	}
	cs := h.connect(auth.ScopeAdmin)
	if res := h.call(cs, "set_setting",
		setSettingArgs{Key: "workspace.name", Value: "Debt Machine"}, nil); !res.IsError {
		t.Error("a demoted owner's token changed a setting")
	}
	if got := settings.Get[string](h.set, "workspace.name"); got != "We All Fall Down" {
		t.Errorf("workspace.name = %q", got)
	}
	// A guest may still read, so the read tools keep working.
	var out usersOut
	h.call(cs, "list_users", noArgs{}, &out)
	if len(out.Users) != 1 {
		t.Errorf("users = %+v", out.Users)
	}
}

// A client shows these hints before it runs a tool, so the four that only look
// must say so and the one that replaces a value must not.
func TestToolsCarryTheirHints(t *testing.T) {
	h := newHarness(t)
	res, err := h.connect(auth.ScopeRead).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		a := tool.Annotations
		if a == nil || a.Title == "" {
			t.Errorf("%s has no annotations", tool.Name)
			continue
		}
		if a.OpenWorldHint == nil || *a.OpenWorldHint {
			t.Errorf("%s reaches outside the workspace", tool.Name)
		}
		if tool.Name == "set_setting" {
			if a.ReadOnlyHint || a.DestructiveHint == nil || !*a.DestructiveHint || !a.IdempotentHint {
				t.Errorf("set_setting = %+v", a)
			}
			continue
		}
		if !a.ReadOnlyHint {
			t.Errorf("%s is not marked read only", tool.Name)
		}
		if tool.OutputSchema == nil {
			t.Errorf("%s has no output schema", tool.Name)
		}
	}
}
