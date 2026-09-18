package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/search"
	"github.com/davidtorcivia/theses/internal/settings"
)

func TestServiceWorkerIsServedFromTheRootWithTheAssetHash(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()

	res, body := h.get("/sw.js")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("/sw.js gave %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Errorf("/sw.js is %s, which no browser will register", ct)
	}
	// A worker the browser serves itself out of its own cache is a worker that
	// never notices a deploy.
	if cc := res.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("/sw.js is cached as %q", cc)
	}
	if res.Header.Get("Service-Worker-Allowed") != "/" {
		t.Errorf("/sw.js does not claim the root scope")
	}

	version := h.srv.assets.version()
	if version == "" || version == "dev" {
		t.Fatalf("the test server serves assets under %q", version)
	}
	if !strings.HasPrefix(body, "const VERSION = "+strconv.Quote(version)+";\n") {
		t.Fatalf("/sw.js does not open with the asset hash: %.80q", body)
	}
	if !strings.Contains(body, `"app/net.js"`) || !strings.Contains(body, `"app.css"`) {
		t.Error("the precache list does not name the modules and the stylesheet")
	}
	// The worker is served from the root, so its own hashed URL is not
	// something it should be told to keep a copy of.
	if strings.Contains(body, `"sw.js"`) {
		t.Error("the worker precaches itself")
	}

	// Its URL does not move with the hash, which is what makes one registration
	// last across deploys.
	if strings.Contains(res.Request.URL.Path, version) {
		t.Error("the worker is served from a hashed path")
	}
}

func TestTheWorkerIsTheStaticFileWithItsVersionInFront(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()

	// The file lives in the static tree, so it is embedded and hashed with
	// everything else. A worker served from /static/{hash}/ would take that
	// directory as its scope and see no navigation at all, so the copy that is
	// registered is the root one, which is the same script with the version and
	// the precache list written in front of it.
	res, hashed := h.get(h.srv.assets.URL("sw.js"))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the static tree does not hold sw.js: %d", res.StatusCode)
	}
	if strings.Contains(hashed, "const VERSION") {
		t.Error("the file on disk carries a version of its own")
	}
	_, root := h.get("/sw.js")
	if !strings.HasSuffix(root, hashed) {
		t.Error("the root copy is not the same script with its version in front")
	}
}

func TestTheWorkerVersionMovesWithTheAssets(t *testing.T) {
	// This is what invalidates the cache on a deploy: the cache is named after
	// the hash, the hash is over the tree, and the hash is in the bytes of the
	// worker, so a changed file installs a new worker which drops the old
	// cache whole.
	one, err := newAssets(fstest.MapFS{"app.css": &fstest.MapFile{Data: []byte("a{}")}}, false)
	if err != nil {
		t.Fatal(err)
	}
	two, err := newAssets(fstest.MapFS{"app.css": &fstest.MapFile{Data: []byte("b{}")}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if one.version() == two.version() {
		t.Errorf("two different trees are served under %q", one.version())
	}
	if len(one.names) != 1 || one.names[0] != "app.css" {
		t.Errorf("the precache list is %v", one.names)
	}
}

func TestContentSecurityPolicyAllowsTheWorker(t *testing.T) {
	h := newHarness(t)
	res, _ := h.get("/login")
	policy := res.Header.Get("Content-Security-Policy")
	for _, want := range []string{"worker-src 'self'", "script-src 'self'"} {
		if !strings.Contains(policy, want) {
			t.Errorf("the policy has no %q: %s", want, policy)
		}
	}
	if strings.Contains(policy, "unsafe-inline") {
		t.Errorf("the policy went soft: %s", policy)
	}
}

func TestOfflineShellCarriesNoSessionAtAll(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()

	res, body := h.get("/shell")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("/shell gave %d", res.StatusCode)
	}
	// The service worker keeps a copy of this page and hands it to whoever
	// navigates with no network, including the next person at this machine and
	// anyone who simply asks for the URL. It has to say nothing about the person
	// who fetched it, and nothing about the workspace either.
	name := settings.Get[string](h.srv.settings, "workspace.name")
	if name == "" {
		t.Fatal("the workspace has no name to leak, so this test proves nothing")
	}
	for _, leak := range []string{"Ada", "ada@example.com", "AL", name} {
		if strings.Contains(body, leak) {
			t.Errorf("the cached shell names %q", leak)
		}
	}
	m := payloadRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("the shell carries no payload element for the app to read")
	}
	if strings.TrimSpace(m[1]) != "null" {
		t.Errorf("the cached shell carries a payload: %q", m[1])
	}
	if strings.Contains(body, `name="csrf" content=""`) == false {
		t.Error("the cached shell carries a CSRF token")
	}
	if !strings.Contains(body, "app/main.js") {
		t.Error("the cached shell does not load the app")
	}
}

func TestOfflinePageSaysTheCodeAndNothingElse(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()

	res, body := h.get("/offline")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("/offline gave %d", res.StatusCode)
	}
	if !strings.Contains(body, "kept on this device") {
		t.Error("the offline page does not make the promise the plan makes")
	}
	// The pages outside the app link to nothing internal.
	for _, link := range hrefRe.FindAllStringSubmatch(body, -1) {
		switch link[1] {
		case "/", "/offline", "/login":
		default:
			t.Errorf("the offline page links to %s", link[1])
		}
	}
}

func TestActivityPanelReadsNewestFirstWithUndoWhereCoreAllowsIt(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	id := h.proposition("Tidal Power")
	actor := h.owner()

	column, err := h.srv.board.CreateColumn(h.T.Context(), actor, id, "Reading")
	if err != nil {
		t.Fatal(err)
	}
	card, err := h.srv.board.CreateCard(h.T.Context(), actor, column.EntityID, "Read the tide tables", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.srv.board.SetCardDone(h.T.Context(), actor, card.EntityID, true); err != nil {
		t.Fatal(err)
	}

	res, body := h.get("/app/activity?proposition=" + strconv.FormatInt(id, 10))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("/app/activity gave %d: %s", res.StatusCode, body)
	}
	var out struct {
		Activity []activityRow `json:"activity"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Activity) < 3 {
		t.Fatalf("the panel read %d rows", len(out.Activity))
	}
	for i := 1; i < len(out.Activity); i++ {
		if out.Activity[i-1].Seq <= out.Activity[i].Seq {
			t.Fatalf("the panel is not newest first: %d then %d", out.Activity[i-1].Seq, out.Activity[i].Seq)
		}
	}
	top := out.Activity[0]
	if top.Entity != "card" || top.Action != "done" || !top.Undoable {
		t.Errorf("marking a card done came back as %s %s, undoable %v", top.Entity, top.Action, top.Undoable)
	}
	if top.Actor.Name != actor.Name {
		t.Errorf("the row is attributed to %q", top.Actor.Name)
	}
	// A create cannot be undone: putting the row back would give it a new id
	// and orphan everything that pointed at it.
	for _, row := range out.Activity {
		if row.Action == "create" && row.Undoable {
			t.Errorf("a %s create was offered an undo", row.Entity)
		}
	}
	if core.Undoable("card", "create", top.Before, top.After, false) {
		t.Error("core.Undoable allows a create")
	}
	if core.Undoable(top.Entity, top.Action, top.Before, top.After, true) {
		t.Error("core.Undoable allows a row that has already been undone")
	}
}

func TestActivityPanelIsMembershipAndSessionBound(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	id := strconv.FormatInt(h.proposition("Tidal Power"), 10)

	// An editor who is not on the proposition is not told it exists.
	other := h.as("bob", "Bob Barker", "editor")
	req, err := http.NewRequest("GET", h.http.URL+"/app/activity?proposition="+id, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := other.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("a stranger read the log: %d", res.StatusCode)
	}

	h.signOut()
	res2, _ := h.get("/app/activity?proposition=" + id)
	if res2.StatusCode != http.StatusSeeOther {
		t.Errorf("the panel answered a signed out browser: %d", res2.StatusCode)
	}
}

func TestPaletteSearchReadsGroupedHits(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	id := h.proposition("Tidal Power")
	actor := h.owner()
	column, err := h.srv.board.CreateColumn(h.T.Context(), actor, id, "Reading")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.srv.board.CreateCard(h.T.Context(), actor, column.EntityID, "Read the tide tables", nil); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name, query, kind, title string
	}{
		{name: "a card by a word in its title", query: "tide", kind: "card", title: "Read the tide tables"},
		{name: "a card by the prefix of a word", query: "tid", kind: "card", title: "Read the tide tables"},
		{name: "a proposition by its title", query: "Tidal", kind: "proposition", title: "Tidal Power"},
		{name: "a person by their name", query: "Ada", kind: "user", title: "Ada Lovelace"},
		{name: "nothing matching", query: "kelp"},
		{name: "punctuation alone", query: "**"},
		{name: "an empty query", query: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, body := h.get("/app/search?q=" + url.QueryEscape(tt.query))
			if res.StatusCode != http.StatusOK {
				t.Fatalf("/app/search gave %d: %s", res.StatusCode, body)
			}
			var out struct {
				Query  string         `json:"query"`
				Groups []search.Group `json:"groups"`
			}
			if err := json.Unmarshal([]byte(body), &out); err != nil {
				t.Fatal(err)
			}
			if out.Query != tt.query {
				t.Errorf("the answer carries query %q", out.Query)
			}
			if tt.kind == "" {
				if len(out.Groups) != 0 {
					t.Fatalf("%q found %d groups", tt.query, len(out.Groups))
				}
				return
			}
			for _, g := range out.Groups {
				if g.Kind != tt.kind {
					continue
				}
				for _, hit := range g.Hits {
					if hit.Title == tt.title {
						return
					}
				}
			}
			t.Errorf("%q did not find the %s %q: %s", tt.query, tt.kind, tt.title, body)
		})
	}
}

func TestPaletteSearchIsMembershipAndSessionBound(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	id := h.proposition("Tidal Power")
	actor := h.owner()
	column, err := h.srv.board.CreateColumn(h.T.Context(), actor, id, "Reading")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.srv.board.CreateCard(h.T.Context(), actor, column.EntityID, "Read the tide tables", nil); err != nil {
		t.Fatal(err)
	}

	// The member the proposition belongs to finds the card, which is the control
	// for the reads below: what a stranger does not find has to be membership
	// rather than the route being broken.
	res, body := h.get("/app/search?q=tide")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the owner's search gave %d: %s", res.StatusCode, body)
	}
	if !strings.Contains(body, "Read the tide tables") {
		t.Fatalf("the owner did not find their own card: %s", body)
	}

	// An editor who is not on the proposition finds nothing of it.
	other := h.as("bob", "Bob Barker", "editor")
	kinds := func(query string) []search.Group {
		req, err := http.NewRequest("GET", h.http.URL+"/app/search?q="+url.QueryEscape(query), nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := other.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("a non-member's search gave %d: %s", res.StatusCode, body)
		}
		var out struct {
			Groups []search.Group `json:"groups"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatal(err)
		}
		return out.Groups
	}
	for _, g := range kinds("tide") {
		if g.Kind == "card" || g.Kind == "proposition" {
			t.Errorf("a non-member searched a proposition they are not on: %+v", g)
		}
	}
	// The same non-member's own control: a person belongs to no proposition, so
	// they are found by anyone who may search at all.
	found := false
	for _, g := range kinds("Bob") {
		if g.Kind == "user" {
			found = true
		}
	}
	if !found {
		t.Error("a non-member's search found nobody, so the empty result above says nothing")
	}

	h.signOut()
	res2, _ := h.get("/app/search?q=tide")
	if res2.StatusCode != http.StatusSeeOther {
		t.Errorf("the palette answered a signed out browser: %d", res2.StatusCode)
	}
}
