// Package backup writes the database and the markdown mirror to object storage
// as one encrypted archive, and puts them back from it.
//
// The archive goes under a prefix of its own, written with a second application
// key that may write and list but not delete. Retention is the bucket's job: a
// lifecycle rule expires old archives and Object Lock keeps them until then, so
// nothing here, and nothing holding this key, can shorten the history.
package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"filippo.io/age"

	"github.com/davidtorcivia/theses/internal/blob"
	"github.com/davidtorcivia/theses/internal/config"
	"github.com/davidtorcivia/theses/internal/settings"
	"github.com/davidtorcivia/theses/internal/store"
)

const (
	// Prefix is where the archives live and the only place the backup key may
	// write. Nothing else in the bucket is under it.
	Prefix = "backups/"

	// probePrefix is where "Test backup key" writes. Object Lock holds those
	// objects until the lifecycle rule expires them like any other, so they are
	// kept apart from the archives and the listing skips them.
	probePrefix = Prefix + "probe/"

	// databaseEntry and docsEntry are the two things inside the archive.
	databaseEntry = "database.db"
	docsEntry     = "docs"

	// staleAfter is when readyz stops believing in the newest backup. It is
	// longer than a day, so one missed night is a warning in the log rather than
	// an unready container, and shorter than two, so two are not.
	staleAfter = 36 * time.Hour

	// storeEvery is how often the object store check actually talks to the
	// bucket. A readiness probe runs every few seconds; a bucket does not need
	// to hear about it.
	storeEvery = 5 * time.Minute

	// tickEvery is how often the scheduler looks at the clock.
	tickEvery = time.Minute

	// runTimeout bounds a backup started in the background, so a bucket that
	// stops answering cannot leave the flag set until the process ends.
	runTimeout = 2 * time.Hour
)

var (
	// ErrNotConfigured is what every call says while the backup key is missing.
	ErrNotConfigured = errors.New("backups are not configured: fill in the backup key and test it")

	// ErrBusy keeps a scheduled run, a manual run and a restore off each other.
	ErrBusy = errors.New("a backup or a restore is already running")
)

// Bucket hands back the primary bucket as the settings page has it. The backups
// prefix borrows its provider, endpoint and region and brings its own key.
type Bucket func(context.Context) (blob.Config, error)

type Backup struct {
	cfg     *config.Config
	db      *store.DB
	set     *settings.Settings
	log     *slog.Logger
	version string
	primary Bucket

	// now is time.Now outside the tests.
	now func() time.Time

	busy   atomic.Bool // one archive or restore at a time
	frozen atomic.Bool // writes are refused while a restore swaps the file

	// restored is what the last restore said, for the settings page to print.
	restored atomic.Pointer[string]

	mu       sync.Mutex
	storeAt  time.Time // when the object store was last asked
	storeErr error     // and what it said
}

func New(cfg *config.Config, db *store.DB, set *settings.Settings, log *slog.Logger, version string, primary Bucket) *Backup {
	return &Backup{cfg: cfg, db: db, set: set, log: log, version: version, primary: primary, now: time.Now}
}

// Manifest is the JSON written beside each archive. It says what is inside
// without anyone having to download and decrypt the archive to find out.
type Manifest struct {
	Name      string         `json:"name"`
	CreatedAt time.Time      `json:"created_at"`
	Schema    string         `json:"schema_version"`
	Rows      map[string]int `json:"rows"`
	Database  int64          `json:"database_bytes"`
	Docs      int64          `json:"docs_bytes"`
	Archive   int64          `json:"archive_bytes"`
	Encrypted int64          `json:"encrypted_bytes"`
	SHA256    string         `json:"sha256"`
	Version   string         `json:"app_version"`
}

// Run writes one archive and records what happened where the settings page and
// readyz can read it.
func (b *Backup) Run(ctx context.Context) (Manifest, error) {
	if !b.busy.CompareAndSwap(false, true) {
		return Manifest{}, ErrBusy
	}
	defer b.busy.Store(false)
	m, err := b.archive(ctx)
	b.record(ctx, m, err)
	if err != nil {
		return Manifest{}, err
	}
	b.log.Info("backup written", "key", m.Name, "bytes", m.Encrypted)
	return m, nil
}

// Now starts one archive in the background. An archive outlives the request
// that asked for it, so the page reports what it did the next time it is loaded.
func (b *Backup) Now(ctx context.Context) error {
	if b.busy.Load() {
		return ErrBusy
	}
	run, cancel := context.WithTimeout(context.WithoutCancel(ctx), runTimeout)
	go func() {
		defer cancel()
		if _, err := b.Run(run); err != nil {
			b.log.Error("backup", "err", err)
		}
	}()
	return nil
}

// Running reports whether an archive or a restore is in progress.
func (b *Backup) Running() bool { return b.busy.Load() }

// LastRestore is what the last restore said, success or failure.
func (b *Backup) LastRestore() string {
	if p := b.restored.Load(); p != nil {
		return *p
	}
	return ""
}

// RestoreNow starts a restore in the background. Like an archive it outlives
// the request that asked for it: the archive has to come down, be decrypted,
// be checked and be unpacked before any file changes place.
func (b *Backup) RestoreNow(ctx context.Context, key string, actorID int64) error {
	if b.busy.Load() {
		return ErrBusy
	}
	run, cancel := context.WithTimeout(context.WithoutCancel(ctx), runTimeout)
	go func() {
		defer cancel()
		msg := "Restored " + path.Base(key) + "."
		if err := b.Restore(run, key, actorID); err != nil {
			msg = "The restore failed and nothing was changed: " + err.Error()
			b.log.Error("restore", "key", key, "err", err)
		}
		b.restored.Store(&msg)
	}()
	return nil
}

// Schedule runs the backup at the configured time of day until ctx is cancelled.
func (b *Backup) Schedule(ctx context.Context) {
	t := time.NewTicker(tickEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.tick(ctx, b.now())
		}
	}
}

// tick is one look at the clock.
func (b *Backup) tick(ctx context.Context, now time.Time) {
	due, err := b.due(now)
	if err != nil {
		// A time or a zone that will not parse is a settings mistake. Recording
		// it puts it on the page; the same message every minute writes nothing,
		// because storing a setting that has not changed is a no-op.
		_ = b.set.Set(context.WithoutCancel(ctx), "backups.last_error", []string{err.Error()}, 0)
		return
	}
	if !due {
		return
	}
	if _, err := b.Run(ctx); err != nil && !errors.Is(err, ErrBusy) {
		b.log.Error("scheduled backup", "err", err)
	}
}

// due reports whether the configured moment has passed and no run has started
// since it. It compares against the moment rather than against the minute on
// the clock, so a restart, a slow tick or a zone that jumps an hour forward
// cannot lose a night or run twice.
func (b *Backup) due(now time.Time) (bool, error) {
	if !b.Enabled() || !b.configured() {
		return false, nil
	}
	zone := settings.Get[string](b.set, "workspace.timezone")
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return false, fmt.Errorf("the time zone %q is not an IANA name, so there is no nightly backup", zone)
	}
	clock := settings.Get[string](b.set, "backups.time")
	hm, err := time.Parse("15:04", clock)
	if err != nil {
		return false, fmt.Errorf("the backup time %q is not a time of day such as 03:30", clock)
	}
	local := now.In(loc)
	at := time.Date(local.Year(), local.Month(), local.Day(), hm.Hour(), hm.Minute(), 0, 0, loc)
	if local.Before(at) {
		return false, nil
	}
	last := int64(settings.Get[int](b.set, "backups.last_at"))
	return time.Unix(last, 0).Before(at), nil
}

// Enabled is the toggle on the settings page.
func (b *Backup) Enabled() bool { return settings.Get[string](b.set, "backups.enabled") == "on" }

func (b *Backup) configured() bool {
	return b.set.IsSet("backups.access_key") && b.set.IsSet("backups.secret_key")
}

// Frozen is what the write middleware checks: true while a restore is replacing
// the database.
func (b *Backup) Frozen() bool { return b.frozen.Load() }

// Freeze stops and restarts writes. Restore holds it over the swap; the only
// other caller is the test that checks the middleware refuses a write.
func (b *Backup) Freeze(on bool) { b.frozen.Store(on) }

// client builds the client for the backups prefix: the primary bucket's
// provider, endpoint and region, the bucket named in the Backups section when
// there is one, and the second application key.
func (b *Backup) client(ctx context.Context) (*blob.Client, error) {
	if !b.configured() {
		return nil, ErrNotConfigured
	}
	cfg, err := b.primary(ctx)
	if err != nil {
		return nil, err
	}
	if cfg.AccessKey, err = b.set.Secret(ctx, "backups.access_key"); err != nil {
		return nil, err
	}
	if cfg.SecretKey, err = b.set.Secret(ctx, "backups.secret_key"); err != nil {
		return nil, err
	}
	if bucket := settings.Get[string](b.set, "backups.bucket"); bucket != "" {
		cfg.Bucket = bucket
	}
	if cfg.Bucket == "" {
		return nil, ErrNotConfigured
	}
	return blob.New(cfg)
}

// archive is one run: a consistent copy of the database, a tar of it and the
// markdown mirror, gzipped, encrypted, uploaded, and a manifest beside it.
func (b *Backup) archive(ctx context.Context) (Manifest, error) {
	client, err := b.client(ctx)
	if err != nil {
		return Manifest{}, err
	}
	id, err := identity(b.cfg.SecretKey)
	if err != nil {
		return Manifest{}, err
	}

	started := b.now().UTC()
	stamp := started.Format("20060102T150405Z")
	m := Manifest{
		Name:      Prefix + "theses-" + stamp + ".tar.gz.age",
		CreatedAt: started,
		Version:   b.version,
	}
	if m.Schema, m.Rows, err = b.inventory(ctx); err != nil {
		return Manifest{}, err
	}

	// VACUUM INTO writes a copy of the database as one transaction sees it, so
	// the archive is consistent even though the app is still writing and the
	// write-ahead log is still catching up. It is inside the data directory
	// because that is the one place the container is guaranteed to be able to
	// write, and it is removed on every path out of here.
	copyPath := filepath.Join(b.cfg.DataDir, "backup-"+stamp+".db")
	defer os.Remove(copyPath)
	if _, err := b.db.ExecContext(ctx, `VACUUM INTO ?`, filepath.ToSlash(copyPath)); err != nil {
		return Manifest{}, fmt.Errorf("backup: copy the database: %w", err)
	}

	archivePath := copyPath + ".tar.gz.age"
	defer os.Remove(archivePath)
	if err := b.pack(archivePath, copyPath, id.Recipient(), &m); err != nil {
		return Manifest{}, err
	}

	f, err := os.Open(archivePath)
	if err != nil {
		return Manifest{}, fmt.Errorf("backup: %w", err)
	}
	defer f.Close()
	if err := client.Put(ctx, m.Name, f, m.Encrypted, "application/octet-stream"); err != nil {
		return Manifest{}, err
	}

	// The manifest goes up second, so an archive with one beside it is an
	// archive that finished uploading.
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return Manifest{}, err
	}
	if err := client.Put(ctx, manifestKey(m.Name), bytes.NewReader(body), int64(len(body)), "application/json"); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// counter is the plaintext length, which is neither the size of the file on
// disk nor anything the hash can be asked for.
type counter int64

func (c *counter) Write(p []byte) (int, error) {
	*c += counter(len(p))
	return len(p), nil
}

// pack writes the encrypted archive at dst and fills in the sizes and the hash.
// The bytes go through the hash on their way into the encryption, so the sum is
// of the archive as a restore sees it after decrypting.
func (b *Backup) pack(dst, dbCopy string, to age.Recipient, m *Manifest) (err error) {
	f, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()

	enc, err := age.Encrypt(f, to)
	if err != nil {
		return fmt.Errorf("backup: encrypt: %w", err)
	}
	sum := sha256.New()
	plain := new(counter)
	gz := gzip.NewWriter(io.MultiWriter(enc, sum, plain))
	tw := tar.NewWriter(gz)

	if m.Database, err = addFile(tw, databaseEntry, dbCopy); err != nil {
		return err
	}
	if m.Docs, err = addTree(tw, docsEntry, filepath.Join(b.cfg.DataDir, docsEntry)); err != nil {
		return err
	}

	for _, c := range []io.Closer{tw, gz, enc} {
		if err := c.Close(); err != nil {
			return fmt.Errorf("backup: finish the archive: %w", err)
		}
	}
	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	m.Archive, m.Encrypted = int64(*plain), st.Size()
	m.SHA256 = hex.EncodeToString(sum.Sum(nil))
	return nil
}

func addFile(tw *tar.Writer, name, from string) (int64, error) {
	f, err := os.Open(from)
	if err != nil {
		return 0, fmt.Errorf("backup: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("backup: %w", err)
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: name, Mode: 0o600, Size: st.Size(), ModTime: st.ModTime(), Typeflag: tar.TypeReg,
	}); err != nil {
		return 0, fmt.Errorf("backup: %w", err)
	}
	if _, err := io.Copy(tw, f); err != nil {
		return 0, fmt.Errorf("backup: %w", err)
	}
	return st.Size(), nil
}

// addTree adds a directory under name, if it is there. The markdown mirror does
// not exist until the first document is written, and a backup taken before that
// is still a backup.
func addTree(tw *tar.Writer, name, dir string) (int64, error) {
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	var total int64
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		n, err := addFile(tw, path.Join(name, filepath.ToSlash(rel)), p)
		total += n
		return err
	})
	return total, err
}

// inventory is the schema version and a count per table, which is what makes a
// manifest worth reading: it says how much was in the database at the time.
func (b *Backup) inventory(ctx context.Context) (string, map[string]int, error) {
	var schema string
	if err := b.db.QueryRowContext(ctx,
		`SELECT coalesce(max(name), '') FROM schema_migrations`).Scan(&schema); err != nil {
		return "", nil, fmt.Errorf("backup: read the schema version: %w", err)
	}
	// The full text index keeps its own shadow tables; counting them would say
	// nothing about the data they index.
	rows, err := b.db.QueryContext(ctx, `SELECT name FROM sqlite_schema
		WHERE type = 'table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\'
		AND name NOT LIKE '%\_fts%' ESCAPE '\' ORDER BY name`)
	if err != nil {
		return "", nil, fmt.Errorf("backup: list the tables: %w", err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return "", nil, err
		}
		names = append(names, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", nil, err
	}

	counts := make(map[string]int, len(names))
	for _, name := range names {
		var n int
		if err := b.db.QueryRowContext(ctx, `SELECT count(*) FROM "`+name+`"`).Scan(&n); err != nil {
			return "", nil, fmt.Errorf("backup: count %s: %w", name, err)
		}
		counts[name] = n
	}
	return schema, counts, nil
}

func (b *Backup) record(ctx context.Context, m Manifest, runErr error) {
	// The run is over, so a cancelled context is the shutdown rather than a
	// reason not to write down what happened.
	ctx = context.WithoutCancel(ctx)
	set := func(key, value string) {
		if err := b.set.Set(ctx, key, []string{value}, 0); err != nil {
			b.log.Error("recording the backup", "key", key, "err", err)
		}
	}
	now := strconv.FormatInt(b.now().Unix(), 10)
	set("backups.last_at", now)
	if runErr != nil {
		set("backups.last_error", runErr.Error())
		return
	}
	set("backups.last_error", "")
	set("backups.last_ok_at", now)
	set("backups.last_size", strconv.FormatInt(m.Encrypted, 10))
}

// Entry is one backup as the settings page lists it.
type Entry struct {
	Key      string
	Manifest string // empty when the archive is there but its manifest is not
	When     time.Time
	Size     int64
}

// List reads the prefix. It is the only way to see the history with a key that
// may write and list but not delete, and it is what the restore picks from.
func (b *Backup) List(ctx context.Context) ([]Entry, error) {
	client, err := b.client(ctx)
	if err != nil {
		return nil, err
	}
	objects, err := client.List(ctx, Prefix)
	if err != nil {
		return nil, err
	}
	manifests := map[string]bool{}
	for _, o := range objects {
		if strings.HasSuffix(o.Key, ".json") {
			manifests[o.Key] = true
		}
	}
	var out []Entry
	for _, o := range objects {
		if !strings.HasSuffix(o.Key, ".tar.gz.age") || strings.HasPrefix(o.Key, probePrefix) {
			continue
		}
		e := Entry{Key: o.Key, Size: o.Size, When: o.Modified}
		if when, err := whenOf(o.Key); err == nil {
			e.When = when
		}
		if manifests[manifestKey(o.Key)] {
			e.Manifest = manifestKey(o.Key)
		}
		out = append(out, e)
	}
	// Newest first, which is the order the page wants and the reverse of the
	// order the keys sort in.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func manifestKey(archive string) string { return strings.TrimSuffix(archive, ".tar.gz.age") + ".json" }

// whenOf reads the timestamp out of a key, so the page can date a backup
// without downloading its manifest.
func whenOf(key string) (time.Time, error) {
	name := strings.TrimSuffix(path.Base(key), ".tar.gz.age")
	_, stamp, ok := strings.Cut(name, "-")
	if !ok {
		return time.Time{}, fmt.Errorf("backup: %q has no timestamp", key)
	}
	return time.Parse("20060102T150405Z", stamp)
}

// Probe is "Test backup key". It writes and lists under the prefix and then
// tries to delete what it wrote: a refused delete is the pass, because the
// whole point of the second key is that it cannot take the history with it if
// the app, or this settings page, is ever in the wrong hands.
func (b *Backup) Probe(ctx context.Context) (string, error) {
	client, err := b.client(ctx)
	if err != nil {
		return "", err
	}
	var r [8]byte
	if _, err := rand.Read(r[:]); err != nil {
		return "", err
	}
	key := probePrefix + hex.EncodeToString(r[:])
	const body = "probe\n"
	if err := client.Put(ctx, key, strings.NewReader(body), int64(len(body)), "text/plain"); err != nil {
		return "", fmt.Errorf("the key cannot write under %s: %w", Prefix, err)
	}
	objects, err := client.List(ctx, Prefix)
	if err != nil {
		return "", fmt.Errorf("the key cannot list %s, so the page could never show the history: %w", Prefix, err)
	}
	found := false
	for _, o := range objects {
		found = found || o.Key == key
	}
	if !found {
		return "", fmt.Errorf("the key wrote %s and then did not see it in the listing", key)
	}
	if err := client.Delete(ctx, key); err == nil {
		return "", fmt.Errorf("the key deleted its own probe object, so it can delete backups too. "+
			"Make an application key for the %s prefix with write and list but no delete, and turn Object Lock on.", Prefix)
	}
	return fmt.Sprintf("Wrote and listed %s, and the delete was refused. That refusal is the test: "+
		"this key can add to the history and read it back, and cannot remove any of it.", key), nil
}

// CheckStore is the readyz probe for the object store. It heads one key in the
// primary bucket and keeps the answer for five minutes, because a readiness
// probe runs every few seconds and a bucket charges for every call.
func (b *Backup) CheckStore(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.storeAt.IsZero() && b.now().Sub(b.storeAt) < storeEvery {
		return b.storeErr
	}
	b.storeAt, b.storeErr = b.now(), b.headPrimary(ctx)
	return b.storeErr
}

func (b *Backup) headPrimary(ctx context.Context) error {
	cfg, err := b.primary(ctx)
	if err != nil {
		return err
	}
	if cfg.Bucket == "" || cfg.AccessKey == "" {
		return nil // nothing is configured yet, which is the setup page's problem
	}
	client, err := blob.New(cfg)
	if err != nil {
		return err
	}
	// The object is not expected to be there. A bucket that answers "no such
	// key" is a bucket that is reachable, exists and took the signature, which
	// is everything this check is for.
	if _, _, err := client.Head(ctx, "probe/readyz"); err != nil && !errors.Is(err, blob.ErrNotFound) {
		return err
	}
	return nil
}

// CheckAge is the readyz probe for the history itself.
func (b *Backup) CheckAge(context.Context) error {
	if !b.Enabled() {
		return nil
	}
	at := int64(settings.Get[int](b.set, "backups.last_ok_at"))
	if at == 0 {
		return errors.New("no backup has succeeded yet")
	}
	if since := b.now().Sub(time.Unix(at, 0)); since > staleAfter {
		return fmt.Errorf("the newest backup is %s old", since.Round(time.Minute))
	}
	return nil
}
