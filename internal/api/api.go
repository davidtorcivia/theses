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

// Actor is how this token is recorded in the activity log.
func (p Principal) Actor() settings.Actor {
	return settings.Actor{
		Kind:   "token",
		ID:     strconv.FormatInt(p.Token.ID, 10),
		UserID: p.User.ID,
	}
}

// Scopes returns the token's scopes as a list, for the JSON that reports them.
func (p Principal) Scopes() []string { return strings.Fields(p.Token.Scopes) }

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
		presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		presented = strings.TrimSpace(presented)
		if !ok || presented == "" {
			unauthorized(w, "send an API token as Authorization: Bearer")
			return
		}
		token, user, err := a.auth.APIToken(r.Context(), presented)
		if errors.Is(err, auth.ErrTokenInvalid) || errors.Is(err, store.ErrNotFound) {
			unauthorized(w, "that token is not valid")
			return
		}
		if err != nil {
			a.failed(w, r, err)
			return
		}
		if !a.auth.Allow(auth.BucketAPI, "token:"+strconv.FormatInt(token.ID, 10)) {
			fail(w, http.StatusTooManyRequests, "too many requests for this token; wait a minute")
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
			a.failed(w, r, errors.New("route is not behind Authenticate"))
			return
		}
		if scope != anyScope && !auth.HasScope(p.Token.Scopes, scope) {
			fail(w, http.StatusForbidden, "this token does not have the "+scope+" scope")
			return
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
		fail(w, http.StatusNotFound, "no such endpoint")
	})
	return a.Authenticate(mux)
}

func (a *API) me(w http.ResponseWriter, r *http.Request, p Principal) {
	writeJSON(w, http.StatusOK, map[string]any{
		"token": map[string]any{"id": p.Token.ID, "name": p.Token.Name, "scopes": p.Scopes()},
		"user": map[string]any{
			"id": p.User.ID, "handle": p.User.Handle, "name": p.User.Name,
			"email": p.User.Email, "role": p.User.Role,
		},
	})
}

type userJSON struct {
	ID       int64  `json:"id"`
	Handle   string `json:"handle"`
	Name     string `json:"name"`
	Initials string `json:"initials"`
	Colour   string `json:"colour"`
	Role     string `json:"role"`
	LastSeen int64  `json:"last_seen_at,omitempty"`
}

func (a *API) users(w http.ResponseWriter, r *http.Request, _ Principal) {
	users, err := store.ListUsers(r.Context(), a.db)
	if err != nil {
		a.failed(w, r, err)
		return
	}
	out := make([]userJSON, 0, len(users))
	for _, u := range users {
		out = append(out, userJSON{
			ID: u.ID, Handle: u.Handle, Name: u.Name, Initials: u.Initials,
			Colour: u.Colour, Role: u.Role, LastSeen: u.LastSeenAt.Int64,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

func (a *API) search(w http.ResponseWriter, r *http.Request, _ Principal) {
	q := r.URL.Query().Get("q")
	groups, err := search.Search(r.Context(), a.db, q, intParam(r, "limit", 0))
	if err != nil {
		a.failed(w, r, err)
		return
	}
	if groups == nil {
		groups = []search.Group{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"query": q, "groups": groups})
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

// activity is a cursor rather than a feed: it returns the rows after ?since= in
// id order, so following the log is asking again with the last id you were
// given. Ids are used and not timestamps because created_at is whole seconds
// and a busy second holds many rows.
func (a *API) activity(w http.ResponseWriter, r *http.Request, _ Principal) {
	limit := intParam(r, "limit", activityLimit)
	if limit <= 0 {
		limit = activityLimit
	}
	if limit > activityMaxLimit {
		limit = activityMaxLimit
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT id, proposition_id, actor_kind, actor_id,
		entity, entity_id, action, before_json, after_json, created_at, undone_at
		FROM activity WHERE id > ? ORDER BY id LIMIT ?`, intParam(r, "since", 0), limit)
	if err != nil {
		a.failed(w, r, err)
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
			a.failed(w, r, err)
			return
		}
		e.PropositionID, e.UndoneAt = prop.Int64, undone.Int64
		if before.Valid {
			e.Before = json.RawMessage(before.String)
		}
		if after.Valid {
			e.After = json.RawMessage(after.String)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		a.failed(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"activity": out})
}

type settingJSON struct {
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

// describe is one setting as the API reports it. A secret's value is never in
// it: the client is told only that one is stored, which is all it can act on.
func (a *API) describe(def settings.Def) settingJSON {
	s := settingJSON{
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

func (a *API) getSettings(w http.ResponseWriter, r *http.Request, _ Principal) {
	out := make([]settingJSON, 0, len(settings.Registry))
	for _, def := range settings.Registry {
		out = append(out, a.describe(def))
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": out})
}

// maxBodyBytes is what a request body may be. Every write here is one setting;
// files go to object storage from the browser and never through this process.
const maxBodyBytes = 64 << 10

func (a *API) putSetting(w http.ResponseWriter, r *http.Request, p Principal) {
	def, ok := settings.Lookup(r.PathValue("key"))
	if !ok {
		fail(w, http.StatusNotFound, "no such setting")
		return
	}
	var body struct {
		Value json.RawMessage `json:"value"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			fail(w, http.StatusRequestEntityTooLarge, "that body is too large")
			return
		}
		fail(w, http.StatusBadRequest, "the body must be JSON with a value field")
		return
	}
	values, err := formValues(body.Value)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.set.SetAs(r.Context(), def.Key, values, p.Actor()); err != nil {
		// Set validates against the key's definition, so what it complains about
		// is the client's fault and worth repeating word for word.
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.describe(def))
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// fail is every refusal the client caused: one JSON object with one field.
func fail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func unauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="theses"`)
	fail(w, http.StatusUnauthorized, msg)
}

// failed is a fault on this side: logged with the path, answered without the
// detail.
func (a *API) failed(w http.ResponseWriter, r *http.Request, err error) {
	a.log.Error("api request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	fail(w, http.StatusInternalServerError, "something went wrong here")
}
