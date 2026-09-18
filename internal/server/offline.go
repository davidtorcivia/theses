package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strconv"
	"strings"

	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/store"
)

// swName is the service worker inside the static tree. It is served from the
// site root instead, because a worker's scope is the directory it is served
// from and this one has to see every navigation.
const swName = "sw.js"

// serviceWorker serves sw.js from the root with the version and the asset list
// written in front of it. The URL carries no hash, so the browser can check one
// unchanging address for a new worker; the bytes carry the hash, so a deploy
// changes them, the new worker installs itself and the old cache is dropped.
func (s *Server) serviceWorker(w http.ResponseWriter, r *http.Request) {
	body, err := fs.ReadFile(s.assets.fsys, swName)
	if err != nil {
		s.fail(w, r, fmt.Errorf("read %s: %w", swName, err))
		return
	}
	list, err := json.Marshal(s.assets.names)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	// The worker script is the one thing a browser must not serve itself out of
	// its own cache: an update is found by fetching this URL and comparing it.
	w.Header().Set("Cache-Control", "no-cache")
	// Root scope is the default for a script served from the root, so this only
	// says out loud what the path already allows.
	w.Header().Set("Service-Worker-Allowed", "/")
	fmt.Fprintf(w, "const VERSION = %q;\nconst ASSETS = %s;\n", s.assets.version(), list)
	w.Write(body)
}

// version is the hash the static tree is served under, which is what the cache
// is named after.
func (a *assets) version() string {
	return strings.Trim(strings.TrimPrefix(a.prefix, "/static/"), "/")
}

// assetNames is every asset there is to precache, relative to the hashed
// prefix. The worker itself is left out: it is served from the root and is not
// fetched through that prefix. It is walked once, at startup, because the tree
// it walks is embedded and cannot change under a running process.
//
// ponytail: this is the whole tree, the error pages' artwork included, which is
// about a megabyte and a half fetched once per deploy. A list of what the app
// and the offline page actually need would save most of it, at the cost of a
// list that goes stale the first time somebody adds a module.
func assetNames(fsys fs.FS) ([]string, error) {
	out := []string{}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && p != swName {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// offlineShell is the app with nothing of anybody's in it: no payload, no CSRF
// token, no account, and not even the name of the workspace. The service worker
// keeps a copy and hands it to a navigation the network refused, and the page
// fills itself, the name in the top bar included, from the snapshot in
// IndexedDB. Nothing here is worth a session, which is the point: this page is
// served to anyone who asks for it, signed in or not.
func (s *Server) offlineShell(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "shell.html", map[string]any{
		"Title":     "",
		"Workspace": "",
		"CSRF":      "",
		"User":      &store.User{},
		"Payload":   template.JS("null"),
	})
}

// activityRow is one line of the panel: an applied command as the stream
// carries it, plus whether it has been undone and whether undoing it would be
// refused out of hand.
type activityRow struct {
	Seq      int64           `json:"seq"`
	Entity   string          `json:"entity"`
	EntityID int64           `json:"entity_id"`
	Action   string          `json:"action"`
	Actor    core.Actor      `json:"actor"`
	Before   json.RawMessage `json:"before,omitempty"`
	After    json.RawMessage `json:"after,omitempty"`
	At       int64           `json:"at"`
	Undone   bool            `json:"undone,omitempty"`
	Undoable bool            `json:"undoable,omitempty"`
}

// activityPage is how many rows the panel asks for at once.
const activityPage = 50

// getActivity is the panel's read: the newest rows of one proposition, newest
// first. The API's own /api/v1/activity is a forward cursor for a token
// following the log; a panel wants the other end of it, and a browser has a
// session rather than a bearer token. The membership rule is the same one the
// socket and the event stream use.
func (s *Server) getActivity(w http.ResponseWriter, r *http.Request) {
	proposition, _ := strconv.ParseInt(r.URL.Query().Get("proposition"), 10, 64)
	ok, err := board.Readable(r.Context(), s.db, userOf(r), proposition)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		writeJSON(w, map[string]string{"error": "no such proposition"})
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `SELECT a.id, a.actor_kind, a.actor_id,
		coalesce(u.name, ''), coalesce(a.via, ''), a.entity, a.entity_id, a.action,
		a.before_json, a.after_json, a.created_at, a.undone_at
		FROM activity a LEFT JOIN users u ON a.actor_kind = 'user' AND u.id = a.actor_id
		WHERE a.proposition_id = ? ORDER BY a.id DESC LIMIT ?`, proposition, activityPage)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer rows.Close()

	out := []activityRow{}
	for rows.Next() {
		var e activityRow
		var actorID, entityID string
		var before, after sql.NullString
		var undone sql.NullInt64
		if err := rows.Scan(&e.Seq, &e.Actor.Kind, &actorID, &e.Actor.Name, &e.Actor.Via,
			&e.Entity, &entityID, &e.Action, &before, &after, &e.At, &undone); err != nil {
			s.fail(w, r, err)
			return
		}
		e.Actor.ID, _ = strconv.ParseInt(actorID, 10, 64)
		e.EntityID, _ = strconv.ParseInt(entityID, 10, 64)
		if before.Valid {
			e.Before = json.RawMessage(before.String)
		}
		if after.Valid {
			e.After = json.RawMessage(after.String)
		}
		e.Undone = undone.Valid
		e.Undoable = core.Undoable(e.Entity, e.Action, e.Before, e.After, e.Undone)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"activity": out})
}
