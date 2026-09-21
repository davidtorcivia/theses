package board

import (
	"context"
	"sync"
	"testing"
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
