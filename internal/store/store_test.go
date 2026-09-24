package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestEmailMigrationRefusesExistingCaseVariantsAtomically(t *testing.T) {
	ctx := context.Background()
	db := openBeforeEmailMigration(t)
	insertOldUser(t, db, 7, "ada", "Ada@example.com")
	insertOldUser(t, db, 9, "grace", "ada@EXAMPLE.com")
	if _, err := db.ExecContext(ctx, `INSERT INTO sessions
		(user_id, hmac, epoch, created_at, expires_at) VALUES (7, x'07', 1, 0, 1)`); err != nil {
		t.Fatal(err)
	}

	const name = "005_email_nocase.sql"
	if err := migrate(ctx, db); err == nil {
		t.Fatal("migration accepted existing case-insensitive duplicate emails")
	}
	var migrations int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM schema_migrations WHERE name = ?`, name).Scan(&migrations); err != nil {
		t.Fatal(err)
	}
	if migrations != 0 {
		t.Fatal("failed migration was recorded as applied")
	}
	var indexes int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = 'users_email_nocase'`).Scan(&indexes); err != nil {
		t.Fatal(err)
	}
	if indexes != 0 {
		t.Fatal("failed migration left its unique index behind")
	}
	var users, sessions int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if users != 2 || sessions != 1 {
		t.Fatalf("failed migration left %d users and %d sessions, want 2 and 1", users, sessions)
	}
	var foreignKeys int
	if err := db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 {
		t.Fatal("failed migration returned its connection with foreign keys disabled")
	}
}

func TestEmailMigrationPreservesUsersAndTheirForeignKeys(t *testing.T) {
	ctx := context.Background()
	db := openBeforeEmailMigration(t)
	insertOldUser(t, db, 7, "ada", "ada@example.com")
	if _, err := db.ExecContext(ctx, `INSERT INTO sessions
		(id, user_id, hmac, epoch, created_at, expires_at) VALUES (11, 7, x'07', 1, 0, 1)`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var handle string
	if err := db.QueryRowContext(ctx, `SELECT handle FROM users WHERE id = 7`).Scan(&handle); err != nil {
		t.Fatal(err)
	}
	if handle != "ada" {
		t.Fatalf("migrated user handle = %q", handle)
	}
	var sessions int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE id = 11 AND user_id = 7`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 1 {
		t.Fatal("migration lost a child session")
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM users WHERE id = 7`); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE id = 11`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 {
		t.Fatal("session no longer cascades after rebuilding users")
	}
}

func openBeforeEmailMigration(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	db, err := open(filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(ctx, `CREATE TABLE schema_migrations (
		name TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"001_initial.sql", "002_activity_entity.sql", "003_client_keys.sql", "004_block_texts.sql"} {
		body, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if err := applyMigration(ctx, db, name, string(body)); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func insertOldUser(t *testing.T, db *sql.DB, id int64, handle, email string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `INSERT INTO users
		(id, handle, email, name, initials, colour, role, password_hash, created_at)
		VALUES (?, ?, ?, ?, 'XX', '#111', 'editor', 'x', 0)`, id, handle, email, handle); err != nil {
		t.Fatal(err)
	}
}

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

	mustExec(t, db, `INSERT INTO propositions (id, number, title, status, position, created_at) VALUES (1, 1, 'Debt', 'idea', 'V', 0)`)
	mustExec(t, db, `INSERT INTO columns (id, proposition_id, name, position) VALUES (1, 1, 'Research', 'V')`)
	mustExec(t, db, `INSERT INTO cards (id, proposition_id, column_id, position, title, description_md, created_at) VALUES (1, 1, 1, 'V', 'Mortgage servicers', 'who forecloses', 0)`)

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
	mustExec(t, db, `INSERT INTO documents (id, proposition_id, name, slug, position, created_at) VALUES (1, 1, 'Research', 'research', 1.0, 0)`)
	mustExec(t, db, `INSERT INTO blocks (id, document_id, position, text, updated_at) VALUES (1, 1, 'a0', 'five consequential facts', 0)`)
	mustExec(t, db, `INSERT INTO links (id, proposition_id, url, title, created_at) VALUES (1, 1, 'https://example.com', 'Foreclosure machine', 0)`)
	mustExec(t, db, `INSERT INTO files (id, proposition_id, name, object_key, state, created_at) VALUES (1, 1, 'interview.wav', '1-debt/1/interview.wav', 'ready', 0)`)
	mustExec(t, db, `INSERT INTO cards (id, proposition_id, column_id, position, title, created_at) VALUES (2, 1, 1, 'W', 'Outline', 0)`)
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

// A restored file is older than the live one, and its activity ids stop
// where it was taken. The next id after the swap has to be past every id the
// live file handed out, or a tab reading from its last seen id misses it.
func TestSwapKeepsIDsFromGoingBackwards(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	restored := filepath.Join(dir, "restored.db")
	other, err := Open(restored)
	if err != nil {
		t.Fatal(err)
	}
	for q, n := range map[Querier]int{db: 5, other: 2} {
		for range n {
			if err := InsertActivity(ctx, q, "user", "1", "", "card", "1", "edit", "", ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	other.Close()

	if err := db.Swap(ctx, restored, filepath.Join(dir, "app.db.aside")); err != nil {
		t.Fatalf("swap: %v", err)
	}
	if err := InsertActivity(ctx, db, "user", "1", "", "card", "1", "edit", "", ""); err != nil {
		t.Fatal(err)
	}
	var last int64
	if err := db.QueryRowContext(ctx, `SELECT max(id) FROM activity`).Scan(&last); err != nil {
		t.Fatal(err)
	}
	if last != 6 {
		t.Fatalf("the first activity id after the swap is %d, want 6", last)
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
// would put "2" above "1A" instead of below it, so the ordered board columns
// are declared TEXT.
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

func TestPermanentLinkIDsAreNeverReused(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	setup := `INSERT INTO propositions(id,number,title,status,position,created_at) VALUES(1,1,'P','Research','a',1);
 INSERT INTO columns(id,proposition_id,name,position) VALUES(1,1,'Inbox','a');
 INSERT INTO documents(id,proposition_id,name,slug,position,created_at) VALUES(1,1,'Notes','notes',1,1);`
	if _, err := db.ExecContext(ctx, setup); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ table, insert string }{
		{"cards", `INSERT INTO cards(proposition_id,column_id,position,title,created_at) VALUES(1,1,'a','searchable',1)`},
		{"blocks", `INSERT INTO blocks(document_id,position,text,updated_at) VALUES(1,'a','searchable',1)`},
		{"links", `INSERT INTO links(proposition_id,url,title,created_at) VALUES(1,'https://example.com','searchable',1)`},
		{"files", `INSERT INTO files(proposition_id,name,folder,object_key,state,created_at) VALUES(1,'searchable','Documents','key','ready',1)`},
		{"documents", `INSERT INTO documents(proposition_id,name,slug,position,created_at) VALUES(1,'Other','other',2,1)`},
	} {
		first, err := db.ExecContext(ctx, tc.insert)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := first.LastInsertId()
		if _, err := db.ExecContext(ctx, "DELETE FROM "+tc.table+" WHERE id=?", id); err != nil {
			t.Fatal(err)
		}
		next, err := db.ExecContext(ctx, tc.insert)
		if err != nil {
			t.Fatal(err)
		}
		newID, _ := next.LastInsertId()
		if newID <= id {
			t.Fatalf("%s reused %d", tc.table, id)
		}
	}
	var bad int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_foreign_key_check`).Scan(&bad); err != nil || bad != 0 {
		t.Fatalf("foreign keys %d %v", bad, err)
	}
	for _, table := range []string{"cards_fts", "blocks_fts", "links_fts", "files_fts"} {
		var hits int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+table+" WHERE "+table+" MATCH 'searchable'").Scan(&hits); err != nil || hits != 1 {
			t.Fatalf("%s: %d %v", table, hits, err)
		}
	}
}

func TestStableLinkMigrationPreservesExistingRowsAndRelations(t *testing.T) {
	ctx := context.Background()
	db := openBeforeEmailMigration(t)
	insertOldUser(t, db, 1, "owner", "owner@example.com")
	seed := `INSERT INTO propositions(id,number,title,status,position,created_at) VALUES(17,1,'P','Research','a',1);
 INSERT INTO proposition_members(proposition_id,user_id) VALUES(17,1);
 INSERT INTO columns(id,proposition_id,name,position) VALUES(18,17,'Inbox','a');
 INSERT INTO cards(id,proposition_id,column_id,position,title,created_at) VALUES(19,17,18,'a','searchable',1);
 INSERT INTO card_assignees(card_id,user_id) VALUES(19,1);
 INSERT INTO comments(id,card_id,user_id,body_md,created_at) VALUES(20,19,1,'searchable',1);
 INSERT INTO documents(id,proposition_id,name,slug,position,created_at) VALUES(21,17,'Notes','notes',1,1);
 INSERT INTO blocks(id,document_id,position,text,updated_at) VALUES(22,21,'a','searchable',1);
 INSERT INTO links(id,proposition_id,url,title,created_at) VALUES(23,17,'https://example.com','searchable',1);
 INSERT INTO files(id,proposition_id,name,folder,object_key,state,created_at) VALUES(24,17,'searchable','Documents','old','ready',1);
 INSERT INTO files(id,proposition_id,name,folder,object_key,state,version_of,created_at) VALUES(25,17,'version','Documents','new','ready',24,1);
 INSERT INTO checklist_items(card_id,text,position) VALUES(19,'Check','a');
 INSERT INTO document_revisions(document_id,markdown,created_at,reason) VALUES(21,'Before',1,'manual');
 INSERT INTO uploads(file_id,multipart_id,created_at,expires_at) VALUES(25,'upload',1,99);
 INSERT INTO card_links(card_id,link_id) VALUES(19,23);
 INSERT INTO card_files(card_id,file_id) VALUES(19,25);`
	if _, err := db.ExecContext(ctx, seed); err != nil {
		t.Fatal(err)
	}
	for _, entity := range []string{"card", "comment", "document", "block", "link", "file"} {
		if _, err := db.ExecContext(ctx, `INSERT INTO activity(proposition_id,actor_kind,actor_id,entity,entity_id,action,created_at) VALUES(17,'user','1',?,'900','delete',1)`, entity); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"cards", "comments", "documents", "blocks", "links", "files"} {
		var seq int64
		if err := db.QueryRowContext(ctx, `SELECT seq FROM sqlite_sequence WHERE name=?`, table).Scan(&seq); err != nil || seq < 900 {
			t.Fatalf("%s sequence: %d %v", table, seq, err)
		}
	}
	for _, query := range []string{
		`SELECT count(*) FROM cards c JOIN card_assignees a ON a.card_id=c.id JOIN comments m ON m.card_id=c.id WHERE c.id=19 AND m.id=20 AND a.user_id=1`,
		`SELECT count(*) FROM blocks b JOIN documents d ON d.id=b.document_id WHERE b.id=22 AND d.id=21 AND d.revision=1`,
		`SELECT count(*) FROM files f JOIN files parent ON parent.id=f.version_of WHERE f.id=25 AND parent.id=24`,
		`SELECT count(*) FROM proposition_members WHERE proposition_id=17 AND user_id=1`,
		`SELECT count(*) FROM checklist_items WHERE card_id=19`,
		`SELECT count(*) FROM document_revisions WHERE document_id=21 AND markdown='Before'`,
		`SELECT count(*) FROM uploads WHERE file_id=25 AND multipart_id='upload'`,
		`SELECT count(*) FROM card_links WHERE card_id=19 AND link_id=23`,
		`SELECT count(*) FROM card_files WHERE card_id=19 AND file_id=25`,
		`SELECT count(*) FROM block_texts WHERE block_id=22 AND version=1 AND text='searchable'`,
	} {
		var count int
		if err := db.QueryRowContext(ctx, query).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s: %d %v", query, count, err)
		}
	}
	for _, table := range []string{"cards_fts", "blocks_fts", "comments_fts", "links_fts", "files_fts"} {
		var hits int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+table+" WHERE "+table+" MATCH 'searchable'").Scan(&hits); err != nil || hits != 1 {
			t.Fatalf("%s: %d %v", table, hits, err)
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE blocks SET text='changed',version=2 WHERE id=22`); err != nil {
		t.Fatal(err)
	}
	var versions int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM block_texts WHERE block_id=22 AND version=2 AND text='changed'`).Scan(&versions); err != nil || versions != 1 {
		t.Fatalf("history trigger: %d %v", versions, err)
	}
	var revision int
	if err := db.QueryRowContext(ctx, `SELECT revision FROM documents WHERE id=21`).Scan(&revision); err != nil || revision != 2 {
		t.Fatalf("revision trigger: %d %v", revision, err)
	}
}

func TestChecklistKeyMigrationRespellsA0(t *testing.T) {
	ctx := context.Background()
	db := OpenTemp(t)
	for _, q := range []string{
		`INSERT INTO propositions(id,number,title,status,position,created_at) VALUES(1,1,'P','idea','V',0)`,
		`INSERT INTO columns(id,proposition_id,name,position) VALUES(1,1,'C','V')`,
		`INSERT INTO cards(id,proposition_id,column_id,position,title,created_at) VALUES(1,1,1,'V','T',0)`,
		`INSERT INTO checklist_items(id,card_id,text,position) VALUES(1,1,'x','a0'),(2,1,'y','a1')`,
		`INSERT INTO activity(proposition_id,actor_kind,entity,entity_id,action,before_json,after_json,created_at)
			VALUES(1,'user','checklist_item','1','move','{"position":"a2"}','{"position":"a0"}',0)`,
		`DELETE FROM schema_migrations WHERE name='021_checklist_keys.sql'`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var first, second, before, after string
	if err := db.QueryRowContext(ctx, `SELECT (SELECT position FROM checklist_items WHERE id=1),
		(SELECT position FROM checklist_items WHERE id=2), json_extract(before_json,'$.position'),
		json_extract(after_json,'$.position') FROM activity`).Scan(&first, &second, &before, &after); err != nil {
		t.Fatal(err)
	}
	if first != "a" || second != "a1" || before != "a2" || after != "a" {
		t.Fatalf("positions %q %q, activity %q to %q", first, second, before, after)
	}
}
