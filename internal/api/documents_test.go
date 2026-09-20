package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/docs"
	"github.com/davidtorcivia/theses/internal/store"
)

// proposition is one proposition the harness's owner may read.
func (h *harness) proposition() int64 {
	h.Helper()
	e, err := h.board.CreateProposition(context.Background(),
		core.Actor{Kind: core.KindUser, ID: h.user.ID, Name: h.user.Name}, "Tidal Power")
	if err != nil {
		h.Fatal(err)
	}
	return e.EntityID
}

// eventID is the entity the answer's event is about.
func eventID(t *testing.T, body map[string]any) int64 {
	t.Helper()
	event, ok := body["event"].(map[string]any)
	if !ok {
		t.Fatalf("no event in %v", body)
	}
	id, ok := event["entity_id"].(float64)
	if !ok {
		t.Fatalf("no entity id in %v", event)
	}
	return int64(id)
}

func TestDocumentRoundTrip(t *testing.T) {
	h := newHarness(t)
	write := h.token(auth.ScopeRead, auth.ScopeWrite)
	prop := h.proposition()

	created := h.do("POST", fmt.Sprintf("/api/v1/propositions/%d/documents", prop), write,
		`{"name":"Research"}`)
	if created.Code != http.StatusOK {
		t.Fatalf("create answered %d: %s", created.Code, created.Body)
	}
	document := eventID(t, decode(t, created))

	list := decode(t, h.do("GET", fmt.Sprintf("/api/v1/propositions/%d/documents", prop), write, ""))
	if len(list["documents"].([]any)) != 1 {
		t.Fatalf("the list is %v", list["documents"])
	}

	inserted := h.do("POST", fmt.Sprintf("/api/v1/documents/%d/blocks", document), write,
		`{"text":"The sea is a battery."}`)
	if inserted.Code != http.StatusOK {
		t.Fatalf("insert answered %d: %s", inserted.Code, inserted.Body)
	}
	block := eventID(t, decode(t, inserted))

	read := decode(t, h.do("GET", fmt.Sprintf("/api/v1/documents/%d", document), write, ""))
	doc := read["document"].(map[string]any)
	if len(doc["blocks"].([]any)) != 2 {
		t.Fatalf("the document has %v", doc["blocks"])
	}
	rendered, _ := doc["rendered"].(string)
	if !strings.Contains(rendered, "<p>The sea is a battery.</p>") {
		t.Fatalf("the rendered form is %q", rendered)
	}

	// A set with the version the caller holds goes through; the same one again
	// is a conflict carrying what the block holds now.
	current, err := docs.GetBlock(context.Background(), h.db, block)
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"base_version":%d,"text":"The sea is a flywheel."}`, current.Version)
	if w := h.do("PUT", fmt.Sprintf("/api/v1/blocks/%d", block), write, body); w.Code != http.StatusOK {
		t.Fatalf("set answered %d: %s", w.Code, w.Body)
	}
	clash := h.do("PUT", fmt.Sprintf("/api/v1/blocks/%d", block), write,
		fmt.Sprintf(`{"base_version":%d,"text":"The sea is a furnace."}`, current.Version))
	if clash.Code != http.StatusConflict {
		t.Fatalf("a stale set answered %d: %s", clash.Code, clash.Body)
	}
	var conflict struct {
		Conflict core.ConflictError `json:"conflict"`
	}
	if err := json.Unmarshal(clash.Body.Bytes(), &conflict); err != nil {
		t.Fatal(err)
	}
	if conflict.Conflict.Current != "The sea is a flywheel." {
		t.Fatalf("the conflict carries %q", conflict.Conflict.Current)
	}

	if w := h.do("POST", fmt.Sprintf("/api/v1/blocks/%d/move", block), write, `{"after":0}`); w.Code != http.StatusOK {
		t.Fatalf("move answered %d: %s", w.Code, w.Body)
	}
	if w := h.do("POST", fmt.Sprintf("/api/v1/documents/%d/revisions", document), write, ""); w.Code != http.StatusOK {
		t.Fatalf("revision answered %d: %s", w.Code, w.Body)
	}
	revisions := decode(t, h.do("GET", fmt.Sprintf("/api/v1/documents/%d/revisions", document), write, ""))
	if len(revisions["revisions"].([]any)) != 1 {
		t.Fatalf("revisions are %v", revisions["revisions"])
	}
	if w := h.do("DELETE", fmt.Sprintf("/api/v1/blocks/%d", block), write, ""); w.Code != http.StatusOK {
		t.Fatalf("delete answered %d: %s", w.Code, w.Body)
	}
	if w := h.do("PATCH", fmt.Sprintf("/api/v1/documents/%d", document), write, `{"name":"Show notes"}`); w.Code != http.StatusOK {
		t.Fatalf("rename answered %d: %s", w.Code, w.Body)
	}
	if w := h.do("DELETE", fmt.Sprintf("/api/v1/documents/%d", document), write, ""); w.Code != http.StatusOK {
		t.Fatalf("document delete answered %d: %s", w.Code, w.Body)
	}
}

// Every write is attributed to the person whose token it is, with the token's
// name in the via column.
func TestDocumentWritesAreAttributedToTheTokensOwner(t *testing.T) {
	h := newHarness(t)
	write := h.token(auth.ScopeWrite)
	prop := h.proposition()

	h.do("POST", fmt.Sprintf("/api/v1/propositions/%d/documents", prop), write, `{"name":"Research"}`)
	var kind, actor, via string
	if err := h.db.QueryRowContext(context.Background(),
		`SELECT actor_kind, actor_id, coalesce(via, '') FROM activity
		 WHERE entity = 'document' ORDER BY id DESC LIMIT 1`).Scan(&kind, &actor, &via); err != nil {
		t.Fatal(err)
	}
	if kind != "user" || actor != fmt.Sprint(h.user.ID) || via != "token:write" {
		t.Fatalf("the row says %q %q %q", kind, actor, via)
	}
}

// A token whose owner is not a member of the proposition is told the document
// is not there, which is the same answer a document that does not exist gets.
func TestDocumentRoutesRefuseSomebodyWhoIsNotAMember(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	prop := h.proposition()
	document, err := h.docs.CreateDocument(ctx,
		core.Actor{Kind: core.KindUser, ID: h.user.ID, Name: h.user.Name}, prop, "Research")
	if err != nil {
		t.Fatal(err)
	}
	blocks, err := docs.Blocks(ctx, h.db, document.EntityID)
	if err != nil {
		t.Fatal(err)
	}

	// A second person, an editor, who was never added to the proposition.
	id, err := store.CreateUser(ctx, h.db, &store.User{
		Handle: "ada", Email: "ada@example.com", Name: "Ada Lovelace",
		Initials: "AL", Colour: "#1100ff", Role: auth.RoleEditor, PasswordHash: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	clear, err := h.auth.CreateAPIToken(ctx, id, "hers", []string{auth.ScopeRead, auth.ScopeWrite})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, method, target, body string }{
		{"list", "GET", fmt.Sprintf("/api/v1/propositions/%d/documents", prop), ""},
		{"read", "GET", fmt.Sprintf("/api/v1/documents/%d", document.EntityID), ""},
		{"revisions", "GET", fmt.Sprintf("/api/v1/documents/%d/revisions", document.EntityID), ""},
		{"insert", "POST", fmt.Sprintf("/api/v1/documents/%d/blocks", document.EntityID), `{"text":"Mine."}`},
		{"set", "PUT", fmt.Sprintf("/api/v1/blocks/%d", blocks[0].ID), `{"base_version":1,"text":"Mine."}`},
		{"move", "POST", fmt.Sprintf("/api/v1/blocks/%d/move", blocks[0].ID), `{"after":0}`},
		{"delete", "DELETE", fmt.Sprintf("/api/v1/blocks/%d", blocks[0].ID), ""},
		{"rename", "PATCH", fmt.Sprintf("/api/v1/documents/%d", document.EntityID), `{"name":"Mine"}`},
		{"source", "PUT", fmt.Sprintf("/api/v1/documents/%d/source", document.EntityID), `{"text":"Mine."}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := h.do(tc.method, tc.target, clear, tc.body)
			if w.Code != http.StatusNotFound {
				t.Fatalf("answered %d: %s", w.Code, w.Body)
			}
		})
	}
	after, err := docs.GetBlock(ctx, h.db, blocks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Text == "Mine." {
		t.Fatal("a non-member wrote a block")
	}
}

// A read token may not write and a write token may not read, which is what the
// scope is for.
func TestDocumentRoutesAskForTheRightScope(t *testing.T) {
	h := newHarness(t)
	prop := h.proposition()

	w := h.do("POST", fmt.Sprintf("/api/v1/propositions/%d/documents", prop), h.token(auth.ScopeRead),
		`{"name":"Research"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("a read token created a document: %d %s", w.Code, w.Body)
	}
	w = h.do("GET", fmt.Sprintf("/api/v1/propositions/%d/documents", prop), h.token(auth.ScopeWrite), "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("a write token listed the documents: %d %s", w.Code, w.Body)
	}
}

func TestDocumentRoutesRefuseNonsense(t *testing.T) {
	h := newHarness(t)
	write := h.token(auth.ScopeRead, auth.ScopeWrite)
	prop := h.proposition()

	for _, tc := range []struct {
		name, method, target, body string
		want                       int
	}{
		{"a document with no name", "POST", fmt.Sprintf("/api/v1/propositions/%d/documents", prop),
			`{"name":"  "}`, http.StatusUnprocessableEntity},
		{"a body that is not JSON", "POST", fmt.Sprintf("/api/v1/propositions/%d/documents", prop),
			`not json`, http.StatusBadRequest},
		{"an id that is not a number", "GET", "/api/v1/documents/x", "", http.StatusNotFound},
		{"a document that is not there", "GET", "/api/v1/documents/99", "", http.StatusNotFound},
		{"a revision reason the column will not take", "POST",
			fmt.Sprintf("/api/v1/documents/%d/revisions", 99), `{"reason":"because"}`,
			http.StatusUnprocessableEntity},
		{"a reason that belongs to the timer", "POST",
			fmt.Sprintf("/api/v1/documents/%d/revisions", 99), `{"reason":"periodic"}`,
			http.StatusUnprocessableEntity},
		{"and one that belongs to the importer", "POST",
			fmt.Sprintf("/api/v1/documents/%d/revisions", 99), `{"reason":"pre-import"}`,
			http.StatusUnprocessableEntity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if w := h.do(tc.method, tc.target, write, tc.body); w.Code != tc.want {
				t.Fatalf("answered %d, want %d: %s", w.Code, tc.want, w.Body)
			}
		})
	}
}

// Writing a document from its markdown: the paragraphs nobody changed keep
// their blocks, the new one is a new block, and a base naming a version the
// database cannot produce the text of is refused with 409.
func TestWriteDocumentSource(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	write := h.token(auth.ScopeWrite)
	who := core.Actor{Kind: core.KindUser, ID: h.user.ID, Name: h.user.Name}
	prop := h.proposition()
	document, err := h.docs.CreateDocument(ctx, who, prop, "Research")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.docs.InsertBlock(ctx, who, document.EntityID, 0, "The sea is a battery.", false); err != nil {
		t.Fatal(err)
	}
	blocks, err := docs.Blocks(ctx, h.db, document.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	base, err := json.Marshal(blocks)
	if err != nil {
		t.Fatal(err)
	}
	// The blocks marshal with more fields than base takes, and the extra ones
	// are ignored, which is what a client sending the rows back does.
	body := fmt.Sprintf(`{"base":%s,"text":%q}`, base,
		"The sea is a battery.\n\n"+strings.Join(textsAfter(blocks[1:]), "\n\n")+"\n\nAnd a flywheel.")

	w := h.do("PUT", fmt.Sprintf("/api/v1/documents/%d/source", document.EntityID), write, body)
	if w.Code != http.StatusOK {
		t.Fatalf("the save answered %d: %s", w.Code, w.Body)
	}
	if conflicts := decode(t, w)["conflicts"].([]any); len(conflicts) != 0 {
		t.Fatalf("the save reported %v", conflicts)
	}
	after, err := docs.Blocks(ctx, h.db, document.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(blocks)+1 || after[0].ID != blocks[0].ID {
		t.Fatalf("the document is %+v", after)
	}
	if after[len(after)-1].Text != "And a flywheel." {
		t.Fatalf("the last paragraph is %q", after[len(after)-1].Text)
	}

	// The same base again, now that the document has moved on, still lines up:
	// the versions it names are on record. A base naming a version that is not
	// is refused outright.
	stale := fmt.Sprintf(`{"base":[{"id":%d,"version":999}],"text":"Nothing."}`, blocks[0].ID)
	if w := h.do("PUT", fmt.Sprintf("/api/v1/documents/%d/source", document.EntityID), write, stale); w.Code != http.StatusConflict {
		t.Fatalf("an unrecoverable base answered %d: %s", w.Code, w.Body)
	}
}

func textsAfter(blocks []docs.Block) []string {
	out := make([]string, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, b.Text)
	}
	return out
}
