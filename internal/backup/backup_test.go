package backup

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/davidtorcivia/theses/internal/blob"
	"github.com/davidtorcivia/theses/internal/config"
	"github.com/davidtorcivia/theses/internal/settings"
	"github.com/davidtorcivia/theses/internal/store"
)

const envKey = "an environment key of at least thirty-two bytes"

type fake struct {
	*testing.T
	b      *Backup
	db     *store.DB
	set    *settings.Settings
	dir    string
	bucket *blob.Client

	mu         sync.Mutex
	methods    []string
	deny       string // the method the bucket refuses
	denyStatus int    // and how: 403 is how a key without a capability reads
}

// newFake is the whole package against an in-process bucket: a real database in
// a real data directory, and a settings table with the backup key filled in.
func newFake(t *testing.T) *fake {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	db, err := store.Open(filepath.Join(dir, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	set, err := settings.Open(ctx, db, []byte(envKey))
	if err != nil {
		t.Fatal(err)
	}

	f := &fake{T: t, db: db, set: set, dir: dir, denyStatus: http.StatusForbidden}
	backend := s3mem.New()
	if err := backend.CreateBucket("example-bucket"); err != nil {
		t.Fatal(err)
	}
	inner := gofakes3.New(backend).Server()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.methods = append(f.methods, r.Method)
		deny, status := f.deny, f.denyStatus
		f.mu.Unlock()
		if deny != "" && r.Method == deny {
			w.WriteHeader(status)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	primary := blob.Config{
		Provider: "s3", Endpoint: srv.URL, Region: "us-east-1", Bucket: "example-bucket",
		AccessKey: "primary-key", SecretKey: "primary-secret",
	}
	if f.bucket, err = blob.New(primary); err != nil {
		t.Fatal(err)
	}
	f.b = New(&config.Config{DataDir: dir, SecretKey: []byte(envKey)}, db, set,
		slog.New(slog.NewTextHandler(io.Discard, nil)), "test",
		func(context.Context) (blob.Config, error) { return primary, nil })

	// An account, because a restore refuses an archive taken before there was
	// one to sign in with.
	if _, err := db.ExecContext(ctx, `INSERT INTO users
		(handle, email, name, initials, colour, role, password_hash, created_at)
		VALUES ('ada', 'ada@example.com', 'Ada Lovelace', 'AL', '#1100ff', 'owner', 'x', unixepoch())`); err != nil {
		t.Fatal(err)
	}

	f.save("backups.access_key", "backup-key")
	f.save("backups.secret_key", "backup-secret")
	return f
}

func (f *fake) save(key, value string) {
	f.Helper()
	if err := f.set.SetAs(context.Background(), key, []string{value}, settings.System()); err != nil {
		f.Fatalf("set %s: %v", key, err)
	}
}

// seen is how many requests of one method the bucket has had.
func (f *fake) seen(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, m := range f.methods {
		if m == method {
			n++
		}
	}
	return n
}

func (f *fake) writeDoc(name, body string) {
	f.Helper()
	p := filepath.Join(f.dir, docsEntry, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		f.Fatal(err)
	}
}

func (f *fake) readDoc(name string) string {
	f.Helper()
	body, err := os.ReadFile(filepath.Join(f.dir, docsEntry, name))
	if err != nil {
		return ""
	}
	return string(body)
}

func (f *fake) workspaceName() string {
	f.Helper()
	var v string
	err := f.db.QueryRowContext(context.Background(),
		`SELECT value_json FROM settings WHERE key = 'workspace.name'`).Scan(&v)
	if err != nil {
		f.Fatalf("read workspace.name: %v", err)
	}
	return v
}

func TestBackupAndRestoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	f.save("workspace.name", "before")
	f.writeDoc("10-proposition/research.md", "before")

	m, err := f.b.Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// Everything the archive holds changes after it was taken.
	f.save("workspace.name", "after")
	f.writeDoc("10-proposition/research.md", "after")
	f.writeDoc("10-proposition/later.md", "written after the backup")

	list, err := f.b.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].Key != m.Name || list[0].Manifest != manifestKey(m.Name) {
		t.Fatalf("the listing does not show the archive and its manifest: %+v", list)
	}
	if time.Since(list[0].When) > time.Hour {
		t.Errorf("the date read out of the key is %s", list[0].When)
	}

	if err := f.b.Restore(ctx, m.Name, 0); err != nil {
		t.Fatalf("restore: %v", err)
	}

	if got := f.workspaceName(); got != `"before"` {
		t.Errorf("the database was not put back: workspace.name is %s", got)
	}
	if got := settings.Get[string](f.set, "workspace.name"); got != "before" {
		t.Errorf("the settings in memory still read the old file: %q", got)
	}
	if got := f.readDoc("10-proposition/research.md"); got != "before" {
		t.Errorf("the markdown mirror was not put back: %q", got)
	}
	if got := f.readDoc("10-proposition/later.md"); got != "" {
		t.Errorf("a file written after the backup survived the restore: %q", got)
	}

	// The restored database is the one being read, migrates to the same schema
	// and carries the row that says it was restored.
	if err := f.db.Migrate(ctx); err != nil {
		t.Fatalf("the restored database does not migrate: %v", err)
	}
	var schema string
	if err := f.db.QueryRowContext(ctx, `SELECT max(name) FROM schema_migrations`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if schema != m.Schema {
		t.Errorf("the restored schema is %s, the manifest says %s", schema, m.Schema)
	}
	var n int
	if err := f.db.QueryRowContext(ctx,
		`SELECT count(*) FROM activity WHERE entity = 'backup' AND action = 'restore'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("the restore left %d activity rows", n)
	}
}

func TestManifestSaysWhatIsInside(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	f.writeDoc("notes.md", "a document")

	m, err := f.b.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The newest migration applied, read from the database rather than written
	// out here, because every migration after this one would fail a literal.
	var schema string
	if err := f.db.QueryRowContext(ctx, `SELECT max(name) FROM schema_migrations`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if m.Schema != schema {
		t.Errorf("schema version is %q, want %q", m.Schema, schema)
	}
	if m.Rows["users"] != 1 {
		t.Errorf("users counted as %d", m.Rows["users"])
	}
	if m.Rows["settings"] != 2 { // the two halves of the backup key
		t.Errorf("settings rows counted as %d", m.Rows["settings"])
	}
	if _, ok := m.Rows["cards"]; !ok {
		t.Error("an empty table is missing from the counts")
	}
	for name := range m.Rows {
		if strings.Contains(name, "_fts") {
			t.Errorf("the full text index is in the counts: %s", name)
		}
	}
	if m.Docs != int64(len("a document")) {
		t.Errorf("the mirror is %d bytes in the manifest", m.Docs)
	}
	if m.Database == 0 || m.Archive == 0 || m.Encrypted == 0 || len(m.SHA256) != 64 {
		t.Errorf("the sizes or the hash are missing: %+v", m)
	}
	if m.Version != "test" {
		t.Errorf("app version is %q", m.Version)
	}

	// The manifest beside the archive is the one that was returned.
	var stored Manifest
	client, err := f.b.client(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stored, err = manifestOf(ctx, client, m.Name); err != nil {
		t.Fatal(err)
	}
	if stored.SHA256 != m.SHA256 || stored.Name != m.Name {
		t.Errorf("the stored manifest is not the one that was returned: %+v", stored)
	}
}

func TestTheSchedulerFiresAtTheConfiguredMinuteAndOnlyOnce(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	f.save("workspace.timezone", "UTC")
	f.save("backups.time", "03:30")

	day := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	at := func(h, m int) time.Time { return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }
	count := func() int {
		list, err := f.b.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return len(list)
	}

	// Switched off, the hour does not matter.
	f.b.now = func() time.Time { return at(4, 0) }
	f.b.tick(ctx, at(4, 0))
	if count() != 0 {
		t.Fatal("a backup ran while backups were off")
	}

	f.save("backups.enabled", "on")
	f.b.now = func() time.Time { return at(3, 29) }
	f.b.tick(ctx, at(3, 29))
	if count() != 0 {
		t.Fatal("a backup ran a minute early")
	}

	f.b.now = func() time.Time { return at(3, 30) }
	f.b.tick(ctx, at(3, 30))
	if count() != 1 {
		t.Fatalf("the backup did not run at the configured minute: %d archives", count())
	}
	if settings.Get[int](f.set, "backups.last_ok_at") != int(at(3, 30).Unix()) {
		t.Error("the run was not recorded")
	}
	// Nobody pressed anything, so the row says the app did it rather than
	// naming a person who does not exist.
	var kind string
	if err := f.db.QueryRowContext(ctx, `SELECT actor_kind FROM activity
		WHERE entity = 'setting' AND entity_id = 'backups.last_ok_at'`).Scan(&kind); err != nil {
		t.Fatal(err)
	}
	if kind != "system" {
		t.Errorf("the scheduled run was recorded as %q", kind)
	}

	// Every tick for the rest of the day sees a run that has already happened.
	for _, m := range []int{31, 45} {
		f.b.now = func() time.Time { return at(3, m) }
		f.b.tick(ctx, at(3, m))
	}
	f.b.now = func() time.Time { return at(23, 59) }
	f.b.tick(ctx, at(23, 59))
	if count() != 1 {
		t.Fatalf("the backup ran %d times in one night", count())
	}

	// The next night is a new one.
	next := at(27, 30)
	f.b.now = func() time.Time { return next }
	f.b.tick(ctx, next)
	if count() != 2 {
		t.Fatalf("the backup did not run the next night: %d archives", count())
	}
}

func TestTheSchedulerSaysSoWhenTheTimeWillNotParse(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	f.save("backups.enabled", "on")
	f.save("backups.time", "half three")
	f.b.tick(ctx, time.Now())

	if got := settings.Get[string](f.set, "backups.last_error"); !strings.Contains(got, "half three") {
		t.Errorf("the settings mistake is not on the page: %q", got)
	}
	if list, _ := f.b.List(ctx); len(list) != 0 {
		t.Error("a backup ran on a time that will not parse")
	}
}

func TestTheProbePassesOnlyWhenTheDeleteIsRefused(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)

	// gofakes3 deletes happily, which is the bucket misconfigured: a key that
	// can delete under the prefix can delete the history.
	if _, err := f.b.Probe(ctx); err == nil || !strings.Contains(err.Error(), "deleted its own probe object") {
		t.Fatalf("a key that can delete passed the probe: %v", err)
	}

	// A bucket that fails the delete for any other reason has shown nothing
	// about the key, and saying otherwise would be a security claim nothing
	// supports.
	f.mu.Lock()
	f.deny, f.denyStatus = http.MethodDelete, http.StatusInternalServerError
	f.mu.Unlock()
	if _, err := f.b.Probe(ctx); err == nil || !strings.Contains(err.Error(), "neither refused nor allowed") {
		t.Fatalf("a bucket that broke on the delete gave %v", err)
	}
	f.mu.Lock()
	f.denyStatus = http.StatusForbidden
	f.mu.Unlock()

	// With the capability withheld, as B2 withholds it from a key created
	// without deleteFiles, the refusal is the pass.
	f.mu.Lock()
	f.deny = http.MethodDelete
	f.mu.Unlock()
	msg, err := f.b.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !strings.Contains(msg, "refused") {
		t.Errorf("the result does not say the delete was refused: %q", msg)
	}
	if !strings.Contains(msg, probePrefix) {
		t.Errorf("the result does not name what it wrote: %q", msg)
	}
}

func TestRestoreRefusesATamperedArchiveBeforeAnythingChanges(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	f.save("workspace.name", "before")
	m, err := f.b.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.save("workspace.name", "after")

	r, err := f.bucket.Get(ctx, m.Name)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatal(err)
	}
	archive[len(archive)/2] ^= 0xff
	if err := f.bucket.Put(ctx, m.Name, strings.NewReader(string(archive)), int64(len(archive)), "application/octet-stream"); err != nil {
		t.Fatal(err)
	}

	if err := f.b.Restore(ctx, m.Name, 0); err == nil {
		t.Fatal("a tampered archive was restored")
	}
	if got := f.workspaceName(); got != `"after"` {
		t.Errorf("the database changed anyway: workspace.name is %s", got)
	}
	if f.b.Frozen() {
		t.Error("writes are still stopped after a failed restore")
	}
}

func TestRestoreValidatesTheDatabaseBeforeReplacingLiveData(t *testing.T) {
	for _, tc := range []struct {
		name, damage, repair, want string
	}{
		{"no owner", `UPDATE users SET role = 'editor'`, `UPDATE users SET role = 'owner'`, "no owner"},
		{"broken reference", `PRAGMA foreign_keys = OFF;
			INSERT INTO proposition_members (proposition_id, user_id) VALUES (999, 999);
			PRAGMA foreign_keys = ON`, `DELETE FROM proposition_members`, "foreign key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newFake(t)
			f.save("workspace.name", "before")
			if _, err := f.db.ExecContext(ctx, tc.damage); err != nil {
				t.Fatal(err)
			}
			m, err := f.b.Run(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.ExecContext(ctx, tc.repair); err != nil {
				t.Fatal(err)
			}
			f.save("workspace.name", "after")
			if err := f.b.Restore(ctx, m.Name, 0); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("restore invalid database: %v", err)
			}
			if got := f.workspaceName(); got != `"after"` || f.b.Frozen() {
				t.Fatalf("live data changed or writes remained frozen: %s", got)
			}
		})
	}
}

func TestRestoreRefusesAnArchiveThatDoesNotMatchItsManifest(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	f.save("workspace.name", "before")
	m, err := f.b.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.save("workspace.name", "after")

	// The archive is untouched and decrypts; the hash it is measured against is
	// not the one it was written with.
	wrong := `{"name":"` + m.Name + `","sha256":"` + strings.Repeat("0", 64) + `"}`
	if err := f.bucket.Put(ctx, manifestKey(m.Name), strings.NewReader(wrong), int64(len(wrong)), "application/json"); err != nil {
		t.Fatal(err)
	}
	err = f.b.Restore(ctx, m.Name, 0)
	if err == nil || !strings.Contains(err.Error(), "hash") {
		t.Fatalf("the hash was not checked: %v", err)
	}
	if got := f.workspaceName(); got != `"after"` {
		t.Errorf("the database changed anyway: workspace.name is %s", got)
	}
}

func TestReadyzReportsAStaleBackup(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	now := time.Now()
	f.b.now = func() time.Time { return now }

	if err := f.b.CheckAge(ctx); err != nil {
		t.Errorf("a workspace with backups off is unready: %v", err)
	}

	f.save("backups.enabled", "on")
	if err := f.b.CheckAge(ctx); err == nil || !strings.Contains(err.Error(), "no backup has succeeded") {
		t.Errorf("backups on and never run gave %v", err)
	}

	f.save("backups.last_ok_at", itoa(now.Add(-40*time.Hour).Unix()))
	err := f.b.CheckAge(ctx)
	if err == nil || !strings.Contains(err.Error(), "old") {
		t.Errorf("a backup from forty hours ago gave %v", err)
	}

	f.save("backups.last_ok_at", itoa(now.Add(-time.Hour).Unix()))
	if err := f.b.CheckAge(ctx); err != nil {
		t.Errorf("a backup from an hour ago gave %v", err)
	}
}

func TestTheObjectStoreCheckIsCached(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	now := time.Now()
	f.b.now = func() time.Time { return now }

	for range 3 {
		if err := f.b.CheckStore(ctx); err != nil {
			t.Fatalf("check: %v", err)
		}
	}
	if n := f.seen(http.MethodHead); n != 1 {
		t.Errorf("three readiness checks made %d requests to the bucket", n)
	}

	now = now.Add(storeEvery + time.Minute)
	if err := f.b.CheckStore(ctx); err != nil {
		t.Fatal(err)
	}
	if n := f.seen(http.MethodHead); n != 2 {
		t.Errorf("the check was not repeated after five minutes: %d requests", n)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestAFailureAfterTheSwapIsReportedAsAPartialRestore(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	f.save("workspace.name", "before")
	f.writeDoc("notes.md", "before")
	m, err := f.b.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.save("workspace.name", "after")
	f.writeDoc("notes.md", "after")

	// The mirror is moved aside under a stamp taken from the clock, so
	// something already sitting there stops the move, which is the first thing
	// that can fail once the database has already been replaced.
	when := time.Date(2026, 9, 17, 3, 30, 0, 0, time.UTC)
	f.b.now = func() time.Time { return when }
	blocked := filepath.Join(f.dir, docsEntry+"."+when.Format("20060102T150405Z")+".aside")
	if err := os.MkdirAll(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "in the way"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := f.b.RestoreNow(ctx, m.Name, 0); err != nil {
		t.Fatal(err)
	}
	msg := ""
	for range 100 {
		time.Sleep(20 * time.Millisecond)
		if msg = f.b.LastRestore(); msg != "" {
			break
		}
	}
	if !strings.HasPrefix(msg, "Partly restored") {
		t.Fatalf("a failure after the swap was reported as %q", msg)
	}
	if !strings.Contains(msg, "markdown mirror") {
		t.Errorf("the message does not say what did not happen: %q", msg)
	}
	if strings.Contains(msg, "nothing was changed") {
		t.Errorf("the message claims nothing was changed: %q", msg)
	}

	// The half that did happen stayed happened.
	if got := f.workspaceName(); got != `"before"` {
		t.Errorf("the database is not the restored one: %s", got)
	}
	if got := settings.Get[string](f.set, "workspace.name"); got != "before" {
		t.Errorf("the settings in memory were not reloaded: %q", got)
	}
	if got := f.readDoc("notes.md"); got != "after" {
		t.Errorf("the mirror was moved after all: %q", got)
	}
}

func TestStopWaitsForARestore(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	f.save("workspace.name", "before")
	m, err := f.b.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.save("workspace.name", "after")

	if err := f.b.RestoreNow(ctx, m.Name, 0); err != nil {
		t.Fatal(err)
	}
	f.b.Stop()

	// Everything the goroutine does is done: the message it ends with is there,
	// nothing is still running, and the database is the restored one. A
	// shutdown at this point closes a database that is whole.
	if msg := f.b.LastRestore(); !strings.Contains(msg, "Restored") {
		t.Fatalf("Stop returned before the restore finished: %q", msg)
	}
	if f.b.Running() {
		t.Error("Stop returned with a restore still running")
	}
	if got := f.workspaceName(); got != `"before"` {
		t.Errorf("the database is %s", got)
	}
}
