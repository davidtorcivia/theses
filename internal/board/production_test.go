package board

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/davidtorcivia/theses/internal/frac"
)

func TestProductionTemplateIsAtomicAndReusable(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	ids := make(chan int64, 4)
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Go(func() { e, err := f.ProductionTemplate(ctx, f.who["editor"], f.prop); ids <- e.EntityID; errs <- err })
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var id int64
	for next := range ids {
		if id != 0 && next != id {
			t.Fatal("template duplicated")
		}
		id = next
	}
	var events int
	if err := f.db.QueryRowContext(ctx, `SELECT count(*) FROM activity WHERE entity='card' AND action='template' AND entity_id=?`, id).Scan(&events); err != nil || events != 1 {
		t.Fatalf("template events %d %v", events, err)
	}
	card, err := GetCard(ctx, f.db, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(card.Checklist) != 4 || len(card.Assignees) != 1 || card.Assignees[0] != f.who["editor"].ID {
		t.Fatalf("template %+v", card)
	}
	for _, role := range []string{"guest", "outsider"} {
		if _, err := f.ProductionTemplate(ctx, f.who[role], f.prop); err == nil {
			t.Fatalf("%s created template", role)
		}
	}
	if _, err := f.DeleteCard(ctx, f.who["owner"], id); err != nil {
		t.Fatal(err)
	}
	replacement, err := f.ProductionTemplate(ctx, f.who["owner"], f.prop)
	if err != nil || replacement.EntityID <= id {
		t.Fatalf("replacement %+v %v", replacement, err)
	}
	if _, err := f.ArchiveProposition(ctx, f.who["owner"], f.prop); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ProductionTemplate(ctx, f.who["owner"], f.prop); err == nil {
		t.Fatal("archived template accepted")
	}
}

func TestProductionPlanValidationAndConflict(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	owner := f.who["editor"].ID
	in := ProductionPlan{Owner: &owner, NextAction: "Review script", RecordDate: "2026-09-25", EditDate: "2026-09-26"}
	e, err := f.SaveProductionPlan(ctx, f.who["editor"], f.prop, in)
	if err != nil {
		t.Fatal(err)
	}
	var p ProductionPlan
	if err := json.Unmarshal(e.After, &p); err != nil {
		t.Fatal(err)
	}
	if p.Version != 1 || p.NextAction != in.NextAction {
		t.Fatalf("%+v", p)
	}
	if _, err = f.SaveProductionPlan(ctx, f.who["editor"], f.prop, in); err == nil {
		t.Fatal("stale plan accepted")
	}
	in.Version = 1
	for _, role := range []string{"guest", "outsider"} {
		if _, err = f.SaveProductionPlan(ctx, f.who[role], f.prop, in); err == nil {
			t.Fatalf("%s wrote plan", role)
		}
	}
	in.RecordDate = "2026-02-30"
	if _, err = f.SaveProductionPlan(ctx, f.who["owner"], f.prop, in); err == nil {
		t.Fatal("invalid date")
	}
	in.RecordDate = ""
	outsider := f.who["outsider"].ID
	in.Owner = &outsider
	if _, err = f.SaveProductionPlan(ctx, f.who["owner"], f.prop, in); err == nil {
		t.Fatal("assigned outsider")
	}
	in.Owner = nil
	if _, err = f.SaveProductionPlan(ctx, f.who["owner"], f.prop, in); err != nil {
		t.Fatal(err)
	}
	if _, err = f.ArchiveProposition(ctx, f.who["owner"], f.prop); err != nil {
		t.Fatal(err)
	}
	in.Version = 2
	if _, err = f.SaveProductionPlan(ctx, f.who["owner"], f.prop, in); err == nil {
		t.Fatal("edited archived plan")
	}
}

// The template once spelled its keys a0 to a3, and frac panics placing after a
// key ending in zero, so an item added under a lone a0 took the command down.
func TestProductionChecklistTakesAnotherItem(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	e, err := f.ProductionTemplate(ctx, f.who["editor"], f.prop)
	if err != nil {
		t.Fatal(err)
	}
	card, err := GetCard(ctx, f.db, e.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range card.Checklist {
		if !frac.Valid(item.Position) {
			t.Errorf("template key %q is not one frac accepts", item.Position)
		}
	}
	for _, item := range card.Checklist[1:] {
		if _, err := f.RemoveChecklistItem(ctx, f.who["editor"], item.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.AddChecklistItem(ctx, f.who["editor"], card.ID, "Share: episode posted"); err != nil {
		t.Fatal(err)
	}
}
