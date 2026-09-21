package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/board"
)

func TestConnectedViewsRespectMembershipAndReferences(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	ctx := context.Background()
	owner := h.owner()
	target, err := h.srv.board.CreateProposition(ctx, owner, "Target")
	if err != nil {
		t.Fatal(err)
	}
	private, err := h.srv.board.CreateProposition(ctx, owner, "Private")
	if err != nil {
		t.Fatal(err)
	}
	client := h.as("connected", "Connected", auth.RoleEditor)
	var uid int64
	if err := h.db.QueryRowContext(ctx, `SELECT id FROM users WHERE handle='connected'`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	if _, err := h.srv.board.AddMember(ctx, owner, target.EntityID, uid); err != nil {
		t.Fatal(err)
	}
	cols, err := board.ListColumns(ctx, h.db, private.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	card, err := h.srv.board.CreateCard(ctx, owner, cols[0].ID, fmt.Sprintf("See @[p:%d]", target.EntityID), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.ExecContext(ctx, `INSERT INTO card_assignees(card_id,user_id) VALUES(?,?)`, card.EntityID, uid); err != nil {
		t.Fatal(err)
	}
	read := func(path string) []workItem {
		t.Helper()
		res, err := client.Get(h.http.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d", path, res.StatusCode)
		}
		var out struct {
			Items []workItem `json:"items"`
		}
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out.Items
	}
	path := fmt.Sprintf("/app/backlinks?proposition=%d", target.EntityID)
	if len(read(path)) != 0 || len(read("/app/my-work")) != 0 {
		t.Fatal("private card leaked")
	}
	if _, err := h.srv.board.AddMember(ctx, owner, private.EntityID, uid); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{path, "/app/my-work"} {
		items := read(path)
		if len(items) != 1 || items[0].URL != fmt.Sprintf("/p/%d#card-%d", private.EntityID, card.EntityID) {
			t.Fatalf("items: %+v", items)
		}
	}
	if _, err := h.db.ExecContext(ctx, `UPDATE cards SET title=? WHERE id=?`, fmt.Sprintf("`@[p:%d]`", target.EntityID), card.EntityID); err != nil {
		t.Fatal(err)
	}
	if len(read(path)) != 0 {
		t.Fatal("code token became backlink")
	}
	if _, err := h.db.ExecContext(ctx, `DELETE FROM proposition_members WHERE proposition_id=? AND user_id=?`, private.EntityID, uid); err != nil {
		t.Fatal(err)
	}
	if len(read("/app/my-work")) != 0 {
		t.Fatal("revoked card leaked")
	}
	res, err := client.Get(h.http.URL + fmt.Sprintf("/app/backlinks?proposition=%d", private.EntityID))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatalf("private target: %d", res.StatusCode)
	}
}
