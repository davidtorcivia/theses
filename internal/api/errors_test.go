package api

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/store"
)

// One refusal, one status, whichever resource it came from. These are the cases
// the document routes and the file routes used to answer differently.
func TestOneRefusalIsOneStatus(t *testing.T) {
	h := newFileHarness(t)
	ctx := context.Background()
	proposition := strconv.FormatInt(h.prop, 10)
	owner := h.token(h.owner, auth.ScopeRead, auth.ScopeWrite, auth.ScopeFiles)

	// A researcher may edit and may not delete, and this one is a member, so a
	// delete is the one refusal that is neither the scope nor the membership.
	if _, err := h.db.ExecContext(ctx, `UPDATE users SET role = ? WHERE id = ?`,
		auth.RoleResearcher, h.stranger.ID); err != nil {
		t.Fatal(err)
	}
	researcher, err := store.UserByID(ctx, h.db, h.stranger.ID)
	if err != nil {
		t.Fatal(err)
	}
	who := core.Actor{Kind: core.KindUser, ID: h.owner.ID, Name: h.owner.Name}
	if _, err := h.board.AddMember(ctx, who, h.prop, researcher.ID); err != nil {
		t.Fatal(err)
	}

	w := h.do("POST", "/api/v1/links", owner, `{"proposition":`+proposition+`,"url":"`+h.page+`"}`)
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	link := strconv.FormatInt(int64(decode(t, w)["id"].(float64)), 10)

	for _, tc := range []struct {
		name, method, target, body, token string
		want                              int
	}{
		{
			name:   "a role that may not delete is told nothing is there",
			method: "DELETE", target: "/api/v1/links/" + link,
			token: h.token(researcher, auth.ScopeRead, auth.ScopeWrite),
			want:  http.StatusNotFound,
		},
		{
			name:   "a file with nothing in it is refused rather than logged",
			method: "POST", target: "/api/v1/files",
			body:  `{"proposition":` + proposition + `,"name":"x.md","folder":"Documents","size":0}`,
			token: owner, want: http.StatusUnprocessableEntity,
		},
		{
			name:   "and so is one larger than the bucket takes",
			method: "POST", target: "/api/v1/files",
			body: `{"proposition":` + proposition +
				`,"name":"x.md","folder":"Documents","size":100000000000000}`,
			token: owner, want: http.StatusUnprocessableEntity,
		},
		{
			name:   "a body that is not JSON is the caller's spelling",
			method: "POST", target: "/api/v1/links", body: `not json`,
			token: owner, want: http.StatusBadRequest,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if w := h.do(tc.method, tc.target, tc.token, tc.body); w.Code != tc.want {
				t.Fatalf("%s %s: %d, want %d: %s", tc.method, tc.target, w.Code, tc.want, w.Body)
			}
		})
	}

	// An archived proposition is read only on every resource, and says so with
	// the same status the documents have always used.
	if _, err := h.board.ArchiveProposition(ctx, who, h.prop); err != nil {
		t.Fatal(err)
	}
	w = h.do("POST", "/api/v1/links", owner, `{"proposition":`+proposition+`,"url":"`+h.page+`"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("a link on an archived proposition answered %d: %s", w.Code, w.Body)
	}
}
