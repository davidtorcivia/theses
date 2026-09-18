// Package store owns the SQLite database: connection settings, the embedded
// migrations and the queries this step of the build needs.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Querier is satisfied by both *sql.DB and *sql.Tx, so every query helper below
// works inside a transaction as well as outside one.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// DB is the pool and the file it is open on. The pool is behind a lock rather
// than embedded because a restore replaces the file underneath it: Swap closes
// the pool, moves the file and opens it again, and every query here holds the
// read side for as long as it runs so the swap waits for the ones in flight.
type DB struct {
	mu   sync.RWMutex
	db   *sql.DB
	path string
}

// Open connects to the SQLite file at path and applies any pending migrations.
// The pragmas are in the DSN because they must be set on every pooled connection.
func Open(path string) (*DB, error) {
	sqldb, err := open(path)
	if err != nil {
		return nil, err
	}
	if err := migrate(context.Background(), sqldb); err != nil {
		sqldb.Close()
		return nil, err
	}
	return &DB{db: sqldb, path: path}, nil
}

func open(path string) (*sql.DB, error) {
	dsn := "file:" + filepath.ToSlash(path) + "?" + url.Values{
		"_pragma": {"journal_mode(WAL)", "foreign_keys(ON)", "busy_timeout(5000)", "synchronous(NORMAL)"},
		"_txlock": {"immediate"},
	}.Encode()

	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// SQLite in WAL mode takes many readers and one writer. _txlock=immediate makes
	// every transaction take the write lock up front, so writers queue on
	// busy_timeout instead of deadlocking on an upgrade.
	sqldb.SetMaxOpenConns(4)
	sqldb.SetMaxIdleConns(4)

	if err := sqldb.PingContext(context.Background()); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return sqldb, nil
}

// Path is the file the pool is open on.
func (db *DB) Path() string { return db.path }

func (db *DB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.db.ExecContext(ctx, query, args...)
}

func (db *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.db.QueryContext(ctx, query, args...)
}

func (db *DB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.db.QueryRowContext(ctx, query, args...)
}

func (db *DB) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.db.BeginTx(ctx, opts)
}

func (db *DB) PingContext(ctx context.Context) error {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.db.PingContext(ctx)
}

func (db *DB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.db.Close()
}

// drainWait is how long Swap gives connections that were checked out when the
// pool closed. Closing a pool does not wait for them, and Windows refuses to
// rename a file another handle still has open.
//
// It is a variable so that the test for a connection that never comes back does
// not take ten seconds to make its point. While it is one, no test in this
// package may call t.Parallel.
var drainWait = 10 * time.Second

// Swap replaces the database file with the one at from and opens it. The old
// file is moved to aside rather than removed, so a bad restore is one rename
// away from being undone, and every failure puts back what it found: a process
// with no database can do nothing at all.
//
// Nothing can hold a connection across the rename. The write lock keeps any new
// query from starting, because every call in this file takes the read side, and
// the drain below refuses to move anything while a connection checked out
// before that is still in use. A Rows or a Tx that outlives the drain therefore
// fails the swap rather than having the file moved underneath it, which is why
// reads are left alone while a restore runs: they cannot be caught halfway.
func (db *DB) Swap(ctx context.Context, from, aside string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	if err := db.db.Close(); err != nil {
		db.reopen()
		return fmt.Errorf("close the pool: %w", err)
	}
	if err := drain(db.db); err != nil {
		db.reopen()
		return err
	}
	// A clean close checkpoints the write-ahead log and removes it. Anything
	// left goes with the file it belongs to rather than being deleted, because
	// the copy moved aside is the one thing that can undo a bad restore.
	moveSidecars(db.path, aside)
	if err := os.Rename(db.path, aside); err != nil {
		moveSidecars(aside, db.path)
		db.reopen()
		return fmt.Errorf("move the database aside: %w", err)
	}
	if err := os.Rename(from, db.path); err != nil {
		os.Rename(aside, db.path)
		moveSidecars(aside, db.path)
		db.reopen()
		return fmt.Errorf("move the restored database in: %w", err)
	}
	// Whatever the new file brought with it comes too, so that a write-ahead
	// log left by whoever wrote it is replayed rather than orphaned.
	moveSidecars(from, db.path)
	fresh, err := open(db.path)
	if err == nil {
		err = migrate(ctx, fresh)
		if err != nil {
			fresh.Close()
		}
	}
	if err != nil {
		// The restored file and the log it came with both go, or the old
		// database would be reopened beside a stranger's write-ahead log and
		// replay it.
		os.Remove(db.path)
		removeSidecars(db.path)
		os.Rename(aside, db.path)
		moveSidecars(aside, db.path)
		db.reopen()
		return fmt.Errorf("open the restored database: %w", err)
	}
	db.db = fresh
	return nil
}

// moveSidecars takes the write-ahead log and the shared memory file wherever
// the database file they belong to is going. Both are absent after a clean
// close, so both renames usually do nothing.
func moveSidecars(from, to string) {
	for _, suffix := range []string{"-wal", "-shm"} {
		os.Rename(from+suffix, to+suffix)
	}
}

func removeSidecars(of string) {
	for _, suffix := range []string{"-wal", "-shm"} {
		os.Remove(of + suffix)
	}
}

// reopen puts a pool back on the current path after a failed swap. It is best
// effort: if even this fails the pool stays closed and every query says so,
// which is the honest answer.
func (db *DB) reopen() {
	if fresh, err := open(db.path); err == nil {
		db.db = fresh
	}
}

func drain(closed *sql.DB) error {
	for deadline := time.Now().Add(drainWait); ; {
		if closed.Stats().OpenConnections == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d connections were still open %s after the pool closed",
				closed.Stats().OpenConnections, drainWait)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// OpenTemp is the database for a test: a real file in the test's temp directory,
// closed when the test ends.
func OpenTemp(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "theses.db"))
	if err != nil {
		t.Fatalf("open temp database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// Migrate applies every migration not yet recorded, each in its own transaction.
func (db *DB) Migrate(ctx context.Context) error {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return migrate(ctx, db.db)
}

func migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name       TEXT PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied := map[string]bool{}
	rows, err := db.QueryContext(ctx, `SELECT name FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("read schema_migrations: %w", err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		applied[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	names, err := migrationNames()
	if err != nil {
		return err
	}
	for _, name := range names {
		if applied[name] {
			continue
		}
		body, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		if err := applyMigration(ctx, db, name, string(body)); err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, name, body string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, body); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (name, applied_at) VALUES (?, unixepoch())`, name); err != nil {
		return err
	}
	return tx.Commit()
}

func migrationNames() ([]string, error) {
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}
