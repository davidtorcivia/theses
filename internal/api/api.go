// Package api is the REST surface under /api/v1: bearer tokens with scopes,
// JSON in and JSON out, and the same store and settings the browser uses.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/search"
	"github.com/davidtorcivia/theses/internal/settings"
	"github.com/davidtorcivia/theses/internal/store"
)

type API struct {
	db   *store.DB
	auth *auth.Auth
	set  *settings.Settings
	log  *slog.Logger
}

func New(db *store.DB, a *auth.Auth, set *settings.Settings, log *slog.Logger) *API {
	return &API{db: db, auth: a, set: set, log: log}
}

// A Principal is the token a request arrived with and the person it belongs to.
// MCP shares it: both surfaces authenticate the same way.
type Principal struct {
	Token *store.APIToken
	User  *store.User
}

// TokenVia and ClientVia name what an action was carried by, for the activity
// row's via column. What a token does is done by the person it belongs to, so
// the actor is always that person and via is how they reached in.
func TokenVia(name string) string  { return "token:" + name }
func ClientVia(name string) string { return "mcp:" + name }

// Actor is how a call with this token is recorded in the activity log: the
// person who owns it, and the token's name as via.
func (p Principal) Actor() settings.Actor {
	return settings.Actor{
		Kind:   "user",
		ID:     strconv.FormatInt(p.User.ID, 10),
		Via:    TokenVia(p.Token.Name),
		UserID: p.User.ID,
	}
}

// Scopes returns the token's scopes as a list, for the JSON that reports them.
func (p Principal) Scopes() []string { return strings.Fields(p.Token.Scopes) }

// capabilityFor is the standing a scope also asks of the person the token
// belongs to. A token may not do what its owner may not do, so demoting someone
// takes their tokens down with them on the next request.
var capabilityFor = map[string]string{
	auth.ScopeRead:  auth.CanRead,
	auth.ScopeWrite: auth.CanEdit,
	auth.ScopeFiles: auth.CanEdit,
	auth.ScopeAdmin: auth.CanSettings,
}

// Deny says why a scope is refused, or "" when it is allowed. Both surfaces ask
// it, so the two conditions are written once.
func (p Principal) Deny(scope string) string {
	if !auth.HasScope(p.Token.Scopes, scope) {
		return "this token does not have the " + scope + " scope"
	}
	if !auth.Can(p.User.Role, capabilityFor[scope]) {
		return "the person this token belongs to no longer has that permission"
	}
	return ""
}

// Allowed is Deny asked as a question, for a caller with nothing to say back.
func (p Principal) Allowed(scope string) bool { return p.Deny(scope) == "" }

// A MeView is the answer to who am I: the token and the person it belongs to.
// The REST route and the MCP tool return the same one.
type MeView struct {
	Token TokenView `json:"token"`
	User  OwnerView `json:"user"`
}

type TokenView struct {
	ID     int64    `json:"id"`
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
}

// An OwnerView is the person a token belongs to. It carries the email address
// that UserView leaves out: a token may see whose it is.
type OwnerView struct {
	ID     int64  `json:"id"`
	Handle string `json:"handle"`
	Name   string `json:"name"`
	Email  string `json:"email"`
	Role   string `json:"role"`
}

func (p Principal) Me() MeView {
	return MeView{
		Token: TokenView{ID: p.Token.ID, Name: p.Token.Name, Scopes: p.Scopes()},
		User: OwnerView{
			ID: p.User.ID, Handle: p.User.Handle, Name: p.User.Name,
			Email: p.User.Email, Role: p.User.Role,
		},
	}
}

type principalKey struct{}

// WithPrincipal is exported because the MCP tool handlers read the principal
// from the context their transport was connected with.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// Authenticate resolves the bearer token and puts the principal in the request
// context, or refuses with JSON. The lookup is by HMAC of the presented token,
// so an attacker learns nothing from how long the index took. The MCP handler is
// wrapped in this too, which is why it is a method and not a closure.
func (a *API) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// RFC 9110 says the scheme is matched without regard to case, and some
		// clients send it lowercase.
		scheme, presented, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		presented = strings.TrimSpace(presented)
		if !strings.EqualFold(scheme, "Bearer") || presented == "" {
			a.unauthorized(w, "send an API token as Authorization: Bearer")
			return
		}
		token, user, err := a.auth.LookupAPIToken(r.Context(), presented)
		if errors.Is(err, auth.ErrTokenInvalid) || errors.Is(err, store.ErrNotFound) {
			a.unauthorized(w, "that token is not valid")
			return
		}
		if err != nil {
			a.serverError(w, r, err)
			return
		}
		// The limit is checked before the use is recorded, so a token that
		// floods is refused without costing a write each time.
		if !a.auth.Allow(auth.BucketAPI, "token:"+strconv.FormatInt(token.ID, 10)) {
			a.fail(w, http.StatusTooManyRequests, "too many requests for this token; wait a minute")
			return
		}
		if err := a.auth.TouchAPIToken(r.Context(), token.ID); err != nil {
			a.serverError(w, r, err)
			return
		}
		ctx := WithPrincipal(r.Context(), Principal{Token: token, User: user})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// anyScope is the scope of a route that needs a token but no permission beyond
// having one.
const anyScope = ""

// scoped checks one scope and hands the handler the principal. Every route goes
// through it, so a route that names no scope is a compile error, not a hole.
func (a *API) scoped(scope string, h func(http.ResponseWriter, *http.Request, Principal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := PrincipalFrom(r.Context())
		if !ok {
			a.serverError(w, r, errors.New("route is not behind Authenticate"))
			return
		}
		if scope != anyScope {
			if why := p.Deny(scope); why != "" {
				a.fail(w, http.StatusForbidden, why)
				return
			}
		}
		h(w, r, p)
	}
}

// Handler is everything under /api/v1, authentication included. The server
// mounts it inside its own chain, so the security headers and the logging are
// the same ones the pages get.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/me", a.scoped(anyScope, a.me))
	mux.HandleFunc("GET /api/v1/users", a.scoped(auth.ScopeRead, a.users))
	mux.HandleFunc("GET /api/v1/search", a.scoped(auth.ScopeRead, a.search))
	mux.HandleFunc("GET /api/v1/activity", a.scoped(auth.ScopeRead, a.activity))
	mux.HandleFunc("GET /api/v1/settings", a.scoped(auth.ScopeAdmin, a.getSettings))
	mux.HandleFunc("PUT /api/v1/settings/{key}", a.scoped(auth.ScopeAdmin, a.putSetting))
	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) {
		a.fail(w, http.StatusNotFound, "no such endpoint")
	})
	return a.Authenticate(mux)
}

func (a *API) me(w http.ResponseWriter, r *http.Request, p Principal) {
	a.writeJSON(w, http.StatusOK, p.Me())
}

// A UserView is one member of the workspace. Email addresses are not in it: a
// read token is what an agent is given, and it has no use for them.
type UserView struct {
	ID       int64  `json:"id"`
	Handle   string `json:"handle"`
	Name     string `json:"name"`
	Initials string `json:"initials"`
	Colour   string `json:"colour"`
	Role     string `json:"role"`
	LastSeen int64  `json:"last_seen_at,omitempty"`
}

// UserViews is the workspace, for both surfaces.
func (a *API) UserViews(ctx context.Context) ([]UserView, error) {
	users, err := store.ListUsers(ctx, a.db)
	if err != nil {
		return nil, err
	}
	out := make([]UserView, 0, len(users))
	for _, u := range users {
		out = append(out, UserView{
			ID: u.ID, Handle: u.Handle, Name: u.Name, Initials: u.Initials,
			Colour: u.Colour, Role: u.Role, LastSeen: u.LastSeenAt.Int64,
		})
	}
	return out, nil
}

func (a *API) users(w http.ResponseWriter, r *http.Request, _ Principal) {
	out, err := a.UserViews(r.Context())
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

func (a *API) search(w http.ResponseWriter, r *http.Request, p Principal) {
	q := r.URL.Query().Get("q")
	visible, err := a.Visible(r.Context(), p)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	groups, err := search.Search(r.Context(), a.db, q, intParam(r, "limit", 0), visible)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	if groups == nil {
		groups = []search.Group{}
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"query": q, "groups": groups})
}

// Visible is the workspace as this token's owner may read it: every
// proposition for an owner, and the ones they are a member of for everybody
// else. A token is never a way to read past what its owner may read.
func (a *API) Visible(ctx context.Context, p Principal) (search.Visible, error) {
	if p.User != nil && p.User.Role == auth.RoleOwner {
		return search.Everything, nil
	}
	if p.User == nil {
		return func(int64) bool { return false }, nil
	}
	member, err := board.Memberships(ctx, a.db, p.User.ID)
	if err != nil {
		return nil, err
	}
	return func(proposition int64) bool { return member[proposition] }, nil
}

type activityJSON struct {
	ID            int64           `json:"id"`
	PropositionID int64           `json:"proposition_id,omitempty"`
	ActorKind     string          `json:"actor_kind"`
	ActorID       string          `json:"actor_id"`
	Entity        string          `json:"entity"`
	EntityID      string          `json:"entity_id"`
	Action        string          `json:"action"`
	Before        json.RawMessage `json:"before,omitempty"`
	After         json.RawMessage `json:"after,omitempty"`
	CreatedAt     int64           `json:"created_at"`
	UndoneAt      int64           `json:"undone_at,omitempty"`
}

// The default page of activity and the most one request may ask for.
const (
	activityLimit    = 50
	activityMaxLimit = 200
)

// administration is what the log says about running the workspace rather than
// about its work: a setting's value either side of a change, the address an
// invitation went to, the name and scopes of a token. A token without admin
// reads the log without these.
const administration = `'setting', 'invitation', 'api_token'`

// workspaceWide is every entity whose rows belong to the workspace rather than
// to one proposition, and so are readable by anyone the scope allows. It is a
// list of what to show rather than of what to hide: a proposition's own delete
// is filed with no proposition so that it survives the cascade, and an entity
// added later would otherwise be readable by everybody the day it appeared.
const workspaceWide = `'setting', 'user', 'invitation', 'api_token', 'backup'`

// activity is a cursor rather than a feed: it returns the rows after ?since= in
// id order, so following the log is asking again with the last id you were
// given. Ids are used and not timestamps because created_at is whole seconds
// and a busy second holds many rows.
func (a *API) activity(w http.ResponseWriter, r *http.Request, p Principal) {
	limit := intParam(r, "limit", activityLimit)
	if limit <= 0 {
		limit = activityLimit
	}
	if limit > activityMaxLimit {
		limit = activityMaxLimit
	}
	query := `SELECT id, proposition_id, actor_kind, actor_id,
		entity, entity_id, action, before_json, after_json, created_at, undone_at
		FROM activity WHERE id > ?`
	if !p.Allowed(auth.ScopeAdmin) {
		query += ` AND entity NOT IN (` + administration + `)`
	}
	args := []any{intParam(r, "since", 0)}
	// A row about a proposition is readable the way the proposition is. Rows
	// about the workspace itself are readable by anyone, but carrying no
	// proposition is not enough to be one: a deleted proposition's row carries
	// none either, and it holds the title, statement and members it took away.
	if p.User == nil || p.User.Role != auth.RoleOwner {
		query += ` AND ((proposition_id IS NULL AND entity IN (` + workspaceWide + `))
			OR proposition_id IN
			(SELECT proposition_id FROM proposition_members WHERE user_id = ?))`
		var id int64
		if p.User != nil {
			id = p.User.ID
		}
		args = append(args, id)
	}
	rows, err := a.db.QueryContext(r.Context(), query+` ORDER BY id LIMIT ?`,
		append(args, limit)...)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	defer rows.Close()

	out := []activityJSON{}
	for rows.Next() {
		var e activityJSON
		var prop, undone sql.NullInt64
		var before, after sql.NullString
		if err := rows.Scan(&e.ID, &prop, &e.ActorKind, &e.ActorID, &e.Entity, &e.EntityID,
			&e.Action, &before, &after, &e.CreatedAt, &undone); err != nil {
			a.serverError(w, r, err)
			return
		}
		e.PropositionID, e.UndoneAt = prop.Int64, undone.Int64
		e.Before, e.After = jsonOrString(before), jsonOrString(after)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		a.serverError(w, r, err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"activity": out})
}

// jsonOrString is an activity column as JSON. Most hold the JSON of an entity,
// but a few hold a bare value such as a role name, and quoting those is what
// keeps one of them from making the whole page unencodable.
func jsonOrString(v sql.NullString) json.RawMessage {
	if !v.Valid {
		return nil
	}
	if json.Valid([]byte(v.String)) {
		return json.RawMessage(v.String)
	}
	quoted, err := json.Marshal(v.String)
	if err != nil {
		return nil
	}
	return quoted
}

// A SettingView is one setting as both surfaces report it.
type SettingView struct {
	Key     string   `json:"key"`
	Kind    string   `json:"kind"`
	Label   string   `json:"label"`
	Hint    string   `json:"hint,omitempty"`
	Choices []string `json:"choices,omitempty"`
	Secret  bool     `json:"secret,omitempty"`
	Set     bool     `json:"set"`
	Value   any      `json:"value,omitempty"`
}

var kindNames = map[settings.Kind]string{
	settings.KindString: "string",
	settings.KindInt:    "int",
	settings.KindList:   "list",
	settings.KindChoice: "choice",
	settings.KindText:   "text",
}

// Describe is one setting as the API reports it. A secret's value is never in
// it: the client is told only that one is stored, which is all it can act on.
func (a *API) Describe(def settings.Def) SettingView {
	s := SettingView{
		Key: def.Key, Kind: kindNames[def.Kind], Label: def.Label, Hint: def.Hint,
		Choices: def.Choices, Secret: def.Secret, Set: a.set.IsSet(def.Key),
	}
	if def.Secret {
		return s
	}
	switch def.Kind {
	case settings.KindInt:
		s.Value = settings.Get[int](a.set, def.Key)
	case settings.KindList:
		s.Value = settings.Get[[]string](a.set, def.Key)
	default:
		s.Value = settings.Get[string](a.set, def.Key)
	}
	return s
}

// SettingViews is every known setting, for both surfaces.
func (a *API) SettingViews() []SettingView {
	out := make([]SettingView, 0, len(settings.Registry))
	for _, def := range settings.Registry {
		out = append(out, a.Describe(def))
	}
	return out
}

func (a *API) getSettings(w http.ResponseWriter, r *http.Request, _ Principal) {
	a.writeJSON(w, http.StatusOK, map[string]any{"settings": a.SettingViews()})
}

// maxBodyBytes is what a request body may be. Every write here is one setting;
// files go to object storage from the browser and never through this process.
const maxBodyBytes = 64 << 10

func (a *API) putSetting(w http.ResponseWriter, r *http.Request, p Principal) {
	def, ok := settings.Lookup(r.PathValue("key"))
	if !ok {
		a.fail(w, http.StatusNotFound, "no such setting")
		return
	}
	var body struct {
		Value json.RawMessage `json:"value"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			a.fail(w, http.StatusRequestEntityTooLarge, "that body is too large")
			return
		}
		a.fail(w, http.StatusBadRequest, "the body must be JSON with a value field")
		return
	}
	values, err := formValues(body.Value)
	if err != nil {
		a.fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.set.SetAs(r.Context(), def.Key, values, p.Actor()); err != nil {
		// Set validates against the key's definition, so what it complains about
		// is the client's fault and worth repeating word for word. A failure to
		// store is not, and carries driver detail that says nothing to a client.
		if errors.Is(err, settings.ErrStorage) {
			a.serverError(w, r, err)
			return
		}
		a.fail(w, http.StatusBadRequest, err.Error())
		return
	}
	a.writeJSON(w, http.StatusOK, a.Describe(def))
}

// formValues turns the JSON value into what settings parses: the strings a form
// would have posted. A list setting takes an array, everything else takes a
// string, a number or a boolean.
func formValues(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, errors.New("the body must be JSON with a value field")
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, errors.New("value is not JSON")
	}
	switch v := v.(type) {
	case string:
		return []string{v}, nil
	case float64:
		return []string{strconv.FormatFloat(v, 'f', -1, 64)}, nil
	case bool:
		return []string{strconv.FormatBool(v)}, nil
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			s, ok := e.(string)
			if !ok {
				return nil, errors.New("a list value must be a list of strings")
			}
			out = append(out, s)
		}
		return out, nil
	}
	return nil, errors.New("value must be a string, a number or a list of strings")
}

func intParam(r *http.Request, name string, fallback int) int {
	n, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil {
		return fallback
	}
	return n
}

// writeJSON marshals the whole response before writing the status, so a value
// that cannot be encoded is a logged 500 rather than a 200 with an empty body.
func (a *API) writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		a.log.Error("api response could not be encoded", "err", err)
		status, body = http.StatusInternalServerError, []byte(`{"error":"something went wrong here"}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	w.Write(append(body, '\n'))
}

// fail is every refusal the client caused: one JSON object with one field.
func (a *API) fail(w http.ResponseWriter, status int, msg string) {
	a.writeJSON(w, status, map[string]string{"error": msg})
}

func (a *API) unauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="theses"`)
	a.fail(w, http.StatusUnauthorized, msg)
}

// serverError is a fault on this side: logged with the path, answered without
// the detail.
func (a *API) serverError(w http.ResponseWriter, r *http.Request, err error) {
	a.log.Error("api request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	a.fail(w, http.StatusInternalServerError, "something went wrong here")
}
