package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/davidtorcivia/theses/internal/docs"
)

// The page is rendered with the documents and their blocks, so the tabs and the
// document under the board are there before the socket says anything. A new
// proposition is seeded with the three every episode has, so there is never a
// document area with nothing in it.
func TestShellCarriesTheDocuments(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.setupOwner()
	owner := h.owner()

	if _, err := h.srv.board.CreateProposition(ctx, owner, "Tidal Power"); err != nil {
		t.Fatal(err)
	}

	state := h.payload("/")
	want := []string{"Research", "Script", "Show notes"}
	if len(state.Documents) != len(want) {
		t.Fatalf("the payload carries %+v", state.Documents)
	}
	for i, name := range want {
		if state.Documents[i].Name != name || len(state.Documents[i].Blocks) == 0 {
			t.Fatalf("document %d is %+v, wanted %s with blocks", i, state.Documents[i], name)
		}
	}
	// Research starts from the workspace template. A proposition made a moment
	// ago has no statement, so the template's placeholder takes its title.
	if first := state.Documents[0].Blocks[0].Text; first != "# Tidal Power" {
		t.Fatalf("Research starts with %q", first)
	}
	document := state.Documents[0]

	// The history the page asks for is the same list the API serves, through
	// the same membership test.
	if _, err := h.srv.docs.CreateRevision(ctx, owner, document.ID, docs.ReasonManual); err != nil {
		t.Fatal(err)
	}
	res, body := h.get("/documents/" + strconv.FormatInt(document.ID, 10) + "/revisions")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the history gave %d: %s", res.StatusCode, body)
	}
	var out struct {
		Revisions []docs.Revision `json:"revisions"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("the history is not JSON: %v", err)
	}
	if len(out.Revisions) != 1 || out.Revisions[0].Reason != docs.ReasonManual {
		t.Fatalf("the history is %+v", out.Revisions)
	}

	// A document that is not there answers the same way one somebody may not
	// read does, and neither is a page.
	if res, _ := h.get("/documents/9999/revisions"); res.StatusCode != http.StatusNotFound {
		t.Fatalf("an unknown document gave %d", res.StatusCode)
	}
}
