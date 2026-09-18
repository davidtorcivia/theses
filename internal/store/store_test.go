package store

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestMigrateFreshAndIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "theses.db")

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var applied int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied == 0 {
		t.Fatal("no migrations recorded")
	}
	for _, table := range []string{"users", "sessions", "invitations", "password_resets", "settings",
		"api_tokens", "propositions", "proposition_members", "columns", "cards", "card_assignees",
		"checklist_items", "comments", "documents", "blocks", "document_revisions", "links", "files",
		"uploads", "card_links", "card_files", "activity", "notification_channels", "notification_rules",
		"notification_outbox", "mail_outbox", "cards_fts", "blocks_fts", "links_fts", "files_fts", "comments_fts"} {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM sqlite_master WHERE name = ?`, table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Errorf("table %s missing", table)
		}
	}
	var mode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Errorf("journal mode %q, want wal", mode)
	}
	var fk int
	if err := db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Error("foreign keys are off")
	}
	db.Close()

	// Reopening must not re-apply anything.
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var again int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&again); err != nil {
		t.Fatal(err)
	}
	if again != applied {
		t.Errorf("reopen applied %d migrations, want %d", again-applied, 0)
	}
}

func TestForeignKeysEnforced(t *testing.T) {
	db := OpenTemp(t)
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO sessions (user_id, hmac, epoch, created_at, expires_at) VALUES (99, x'00', 1, 0, 0)`)
	if err == nil {
		t.Fatal("session for a missing user was accepted")
	}
}

func TestFTSTriggers(t *testing.T) {
	ctx := context.Background()
	db := OpenTemp(t)

	mustExec(t, db, `INSERT INTO propositions (id, number, title, status, position, created_at) VALUES (1, 1, 'Debt', 'idea', 1, 0)`)
	mustExec(t, db, `INSERT INTO columns (id, proposition_id, name, position) VALUES (1, 1, 'Research', 1)`)
	mustExec(t, db, `INSERT INTO cards (id, proposition_id, column_id, position, title, description_md, created_at) VALUES (1, 1, 1, 1, 'Mortgage servicers', 'who forecloses', 0)`)

	count := func(q string) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM cards_fts WHERE cards_fts MATCH ?`, q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count("servicers") != 1 {
		t.Error("insert did not reach cards_fts")
	}

	mustExec(t, db, `UPDATE cards SET title = 'Rating agencies' WHERE id = 1`)
	if count("servicers") != 0 {
		t.Error("update left the old title in cards_fts")
	}
	if count("agencies") != 1 {
		t.Error("update did not index the new title")
	}

	mustExec(t, db, `DELETE FROM cards WHERE id = 1`)
	if count("agencies") != 0 {
		t.Error("delete left the row in cards_fts")
	}

	// The other four indexes exist and index their own tables.
	mustExec(t, db, `INSERT INTO documents (id, proposition_id, name, slug, position, created_at) VALUES (1, 1, 'Research', 'research', 1, 0)`)
	mustExec(t, db, `INSERT INTO blocks (id, document_id, position, text, updated_at) VALUES (1, 1, 'a0', 'five consequential facts', 0)`)
	mustExec(t, db, `INSERT INTO links (id, proposition_id, url, title, created_at) VALUES (1, 1, 'https://example.com', 'Foreclosure machine', 0)`)
	mustExec(t, db, `INSERT INTO files (id, proposition_id, name, object_key, state, created_at) VALUES (1, 1, 'interview.wav', '1-debt/1/interview.wav', 'ready', 0)`)
	mustExec(t, db, `INSERT INTO cards (id, proposition_id, column_id, position, title, created_at) VALUES (2, 1, 1, 2, 'Outline', 0)`)
	mustExec(t, db, `INSERT INTO comments (id, card_id, body_md, created_at) VALUES (1, 2, 'needs a street question', 0)`)

	for _, c := range []struct{ table, match string }{
		{"blocks_fts", "consequential"},
		{"links_fts", "foreclosure"},
		{"files_fts", "interview"},
		{"comments_fts", "street"},
	} {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM `+c.table+` WHERE `+c.table+` MATCH ?`, c.match).Scan(&n); err != nil {
			t.Fatalf("%s: %v", c.table, err)
		}
		if n != 1 {
			t.Errorf("%s did not index %q", c.table, c.match)
		}
	}
}

func mustExec(t *testing.T, db *DB, q string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func TestSwapReplacesTheFileAndKeepsTheOldOne(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `INSERT INTO settings (key, value_json, updated_at)
		VALUES ('workspace.name', '"before"', 0)`); err != nil {
		t.Fatal(err)
	}

	// The file swapped in is a second database, as a restored archive would be.
	restored := filepath.Join(dir, "restored.db")
	other, err := Open(restored)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.ExecContext(ctx, `INSERT INTO settings (key, value_json, updated_at)
		VALUES ('workspace.name', '"after"', 0)`); err != nil {
		t.Fatal(err)
	}
	other.Close()

	aside := filepath.Join(dir, "app.db.aside")
	if err := db.Swap(ctx, restored, aside); err != nil {
		t.Fatalf("swap: %v", err)
	}
	var name string
	if err := db.QueryRowContext(ctx, `SELECT value_json FROM settings WHERE key = 'workspace.name'`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != `"after"` {
		t.Errorf("the pool still reads the old file: %s", name)
	}
	if _, err := os.Stat(aside); err != nil {
		t.Errorf("the old database was not kept: %v", err)
	}
}

func TestSwapPutsTheOldFileBackWhenTheNewOneWillNotOpen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "app.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	junk := filepath.Join(dir, "junk.db")
	if err := os.WriteFile(junk, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := db.Swap(ctx, junk, filepath.Join(dir, "app.db.aside")); err == nil {
		t.Fatal("a file that is not a database was swapped in")
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("the original database is not back: %v", err)
	}
}

func TestSwapLeavesAWorkingPoolWhenAConnectionNeverComesBack(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "app.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	restored := filepath.Join(dir, "restored.db")
	other, err := Open(restored)
	if err != nil {
		t.Fatal(err)
	}
	other.Close()

	// Rows hold the connection they were read on until they are closed, and a
	// closed pool does not take it back by itself.
	rows, err := db.QueryContext(ctx, `SELECT name FROM schema_migrations`)
	if err != nil {
		t.Fatal(err)
	}
	drainWait = 50 * time.Millisecond
	t.Cleanup(func() { drainWait = 10 * time.Second })

	if err := db.Swap(ctx, restored, filepath.Join(dir, "app.db.aside")); err == nil {
		t.Fatal("the file was moved while a connection was still checked out")
	}
	rows.Close()

	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("the pool was left closed after a failed swap: %v", err)
	}
	if _, err := os.Stat(restored); err != nil {
		t.Errorf("the file to swap in was moved anyway: %v", err)
	}
}

func TestSwapTakesNoForeignWriteAheadLogWithIt(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "app.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `INSERT INTO settings (key, value_json, updated_at)
		VALUES ('workspace.name', '"before"', 0)`); err != nil {
		t.Fatal(err)
	}

	// A file that will not open, with the log and shared memory files a real
	// one would have beside it.
	junk := filepath.Join(dir, "junk.db")
	for _, name := range []string{junk, junk + "-wal", junk + "-shm"} {
		if err := os.WriteFile(name, []byte("foreign"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	aside := filepath.Join(dir, "app.db.aside")
	if err := db.Swap(ctx, junk, aside); err == nil {
		t.Fatal("a file that is not a database was swapped in")
	}

	// The reopened database makes its own log and shared memory files again;
	// what must not be there is the ones that came in with the other file.
	for _, suffix := range []string{"-wal", "-shm"} {
		if body, err := os.ReadFile(path + suffix); err == nil && strings.Contains(string(body), "foreign") {
			t.Errorf("%s was left beside the database it does not belong to", suffix)
		}
	}
	if _, err := os.Stat(aside); err == nil {
		t.Error("the database was left aside as well as back in place")
	}
	var name string
	if err := db.QueryRowContext(ctx,
		`SELECT value_json FROM settings WHERE key = 'workspace.name'`).Scan(&name); err != nil {
		t.Fatalf("the original database is not back: %v", err)
	}
	if name != `"before"` {
		t.Errorf("the original database reads %s", name)
	}
}

func TestSidecarsMoveWithTheirDatabaseAndAreRemovedWithIt(t *testing.T) {
	dir := t.TempDir()
	from, to := filepath.Join(dir, "a.db"), filepath.Join(dir, "b.db")
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.WriteFile(from+suffix, []byte(suffix), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	moveSidecars(from, to)
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(from + suffix); err == nil {
			t.Errorf("%s stayed behind", suffix)
		}
		body, err := os.ReadFile(to + suffix)
		if err != nil || string(body) != suffix {
			t.Errorf("%s did not arrive: %q, %v", suffix, body, err)
		}
	}
	removeSidecars(to)
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(to + suffix); err == nil {
			t.Errorf("%s survived", suffix)
		}
	}
}

// Ordering keys are base-62 strings, and a column with REAL affinity would turn
// the ones that spell a number into floats and leave the rest as text, which
// would put "2" above "1A" instead of below it.
func TestOrderingKeysStayText(t *testing.T) {
	ctx := context.Background()
	db := OpenTemp(t)

	mustExec(t, db, `INSERT INTO propositions (id, number, title, status, position, created_at) VALUES (1, 1, 'Debt', 'idea', 'V', 0)`)
	mustExec(t, db, `INSERT INTO columns (id, proposition_id, name, position) VALUES (1, 1, 'Research', 'V')`)
	for i, pos := range []string{"2", "1A", "1"} {
		mustExec(t, db, `INSERT INTO cards (id, proposition_id, column_id, position, title, created_at)
			VALUES (?, 1, 1, ?, 'card', 0)`, i+1, pos)
	}

	rows, err := db.QueryContext(ctx, `SELECT position FROM cards WHERE column_id = 1 ORDER BY position`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		got = append(got, p)
	}
	if want := []string{"1", "1A", "2"}; !slices.Equal(got, want) {
		t.Errorf("ordered %v, want %v", got, want)
	}
}
