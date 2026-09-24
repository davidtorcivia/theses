package legal

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/store"
)

func TestReleaseRules(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTemp(t)
	c := core.New(db, core.NewBus())
	b := board.New(c, func() board.Defaults { return board.Defaults{Status: "idea", Columns: []string{"Research"}} })
	s := New(c, nil)
	actors := map[string]core.Actor{}
	for _, role := range []string{"owner", "editor"} {
		id, err := store.CreateUser(ctx, db, &store.User{Handle: role, Email: role + "@example.com", Name: role, Role: role, PasswordHash: "x"})
		if err != nil {
			t.Fatal(err)
		}
		actors[role] = core.Actor{Kind: core.KindUser, ID: id}
	}
	props := map[string]int64{}
	for _, name := range []string{"shared", "private", "archived"} {
		e, err := b.CreateProposition(ctx, actors["owner"], name)
		if err != nil {
			t.Fatal(err)
		}
		props[name] = e.EntityID
	}
	if _, err := b.AddMember(ctx, actors["owner"], props["shared"], actors["editor"].ID); err != nil {
		t.Fatal(err)
	}
	release := func(prop int64) Release {
		t.Helper()
		e, err := s.Save(ctx, actors["owner"], Release{Proposition: prop, State: "NY", Title: "Voices", Kind: "street", Brand: "Workspace", RightsHolder: "Example LLC", EmailSubject: "{title}", EmailBody: "{url}"})
		if err != nil {
			t.Fatal(err)
		}
		r, err := s.Get(ctx, actors["owner"], e.EntityID)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	people := []Person{{Name: "Ada Lovelace", Date: time.Now().Format("2006-01-02"), Consent: true}}

	moved := release(props["shared"])
	moved.Proposition = props["private"]
	if _, err := s.Save(ctx, actors["editor"], moved); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("editor moved a release out of reach: %v", err)
	}

	r := release(props["shared"])
	receipt := Token()
	first, err := s.Sign(ctx, r.Token, receipt, r.Version, people, r.Agreement().Digest())
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.Sign(ctx, r.Token, receipt, r.Version, people, r.Agreement().Digest())
	if err != nil || again.ID != first.ID {
		t.Fatalf("retry signed again: %d then %d, %v", first.ID, again.ID, err)
	}
	stale := r.Agreement().Digest()
	r.Body = "New wording for {rights_holder}"
	if _, err = s.Save(ctx, actors["owner"], r); err != nil {
		t.Fatal(err)
	}
	var before, after string
	if err = db.QueryRowContext(ctx, `SELECT before_json,after_json FROM activity WHERE entity='legal_release' AND action='edit'`).Scan(&before, &after); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(before, `"agreement_digest":"`+stale) || !strings.Contains(after, `"proposition_id"`) || strings.Contains(before+after, "Ada") {
		t.Fatalf("release activity %s -> %s", before, after)
	}
	if _, err = s.Sign(ctx, r.Token, Token(), r.Version, people, stale); !errors.Is(err, ErrChanged) {
		t.Fatalf("stale agreement signed: %v", err)
	}

	archived := release(props["archived"])
	if _, err = b.ArchiveProposition(ctx, actors["owner"], props["archived"]); err != nil {
		t.Fatal(err)
	}
	if public, err := s.Public(ctx, archived.Token); err != nil || !public.Closed {
		t.Fatalf("archived release reads open: %v %v", public.Closed, err)
	}
	if _, err = s.Sign(ctx, archived.Token, Token(), archived.Version, people, archived.Agreement().Digest()); !errors.Is(err, ErrClosed) {
		t.Fatalf("archived sign: %v", err)
	}
}
