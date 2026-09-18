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
// document under the board are there before the socket says anything.
func TestShellCarriesTheDocuments(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.setupOwner()
	owner := h.owner()

	e, err := h.srv.board.CreateProposition(ctx, owner, "Tidal Power")
	if err != nil {
		t.Fatal(err)
	}
	document, err := h.srv.docs.CreateDocument(ctx, owner, e.EntityID, "Research")
	if err != nil {
		t.Fatal(err)
	}

	state := h.payload("/")
	if len(state.Documents) != 1 {
		t.Fatalf("the payload carries %+v", state.Documents)
	}
	if state.Documents[0].Name != "Research" || len(state.Documents[0].Blocks) == 0 {
		t.Fatalf("the document is %+v", state.Documents[0])
	}

	// The history the page asks for is the same list the API serves, through
	// the same membership test.
	if _, err := h.srv.docs.CreateRevision(ctx, owner, document.EntityID, docs.ReasonManual); err != nil {
		t.Fatal(err)
	}
	res, body := h.get("/documents/" + strconv.FormatInt(document.EntityID, 10) + "/revisions")
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
