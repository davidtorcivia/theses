package search

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/davidtorcivia/theses/internal/store"
)

func BenchmarkWorkspace(b *testing.B) {
	db, err := store.Open(filepath.Join(b.TempDir(), "benchmark.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	for _, query := range []string{
		`INSERT INTO propositions(id,number,title,status,position,created_at) VALUES(1,1,'Workspace benchmark','idea','a0',1)`,
		`INSERT INTO columns(id,proposition_id,name,position) VALUES(1,1,'Research','a0')`,
		`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<10000) INSERT INTO cards(proposition_id,column_id,position,title,description_md,created_at) SELECT 1,1,printf('%06d',x),'Research task '||x,CASE WHEN x%100=0 THEN 'Tidal evidence' ELSE 'General research notes' END,1 FROM n`,
		`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<50000) INSERT INTO activity(proposition_id,actor_kind,actor_id,entity,entity_id,action,created_at) SELECT 1,'user','1','card',x%10000+1,'update',1 FROM n`,
	} {
		if _, err := db.ExecContext(ctx, query); err != nil {
			b.Fatal(err)
		}
	}
	b.Run("Search10000Cards", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			rows, err := Search(ctx, db, "Tidal", 20, Everything())
			if err != nil || len(rows) == 0 {
				b.Fatalf("search: %v %v", rows, err)
			}
		}
	})
	b.Run("Activity50000RowsDeepCursor", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			rows, err := db.QueryContext(ctx, `SELECT id FROM activity WHERE proposition_id=1 AND entity='card' AND id<25000 ORDER BY id DESC LIMIT 51`)
			if err != nil {
				b.Fatal(err)
			}
			n := 0
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					b.Fatal(err)
				}
				n++
			}
			err = rows.Err()
			rows.Close()
			if err != nil || n != 51 {
				b.Fatalf("activity: %d %v", n, err)
			}
		}
	})
}
