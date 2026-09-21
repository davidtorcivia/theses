package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"filippo.io/age"

	"github.com/davidtorcivia/theses/internal/blob"
	"github.com/davidtorcivia/theses/internal/store"
)

// maxEntry is the largest file a restore will write out of an archive. The
// archive is authenticated, so this is a bound on a bug rather than on an
// attacker, and it is far above any database this app will hold.
const maxEntry = 64 << 30

// Restore puts the database and the markdown mirror back from one archive.
//
// Everything that can fail happens before anything changes: the archive is
// downloaded, decrypted, checked against the hash in its manifest, unpacked
// beside the live files and opened as a database. Only then do writes stop and
// the files change places, and the ones being replaced are moved aside under a
// timestamp rather than removed.
func (b *Backup) Restore(ctx context.Context, key string, actorID int64) error {
	if !b.busy.CompareAndSwap(false, true) {
		return ErrBusy
	}
	defer b.busy.Store(false)

	client, err := b.client(ctx)
	if err != nil {
		return err
	}
	id, err := identity(b.cfg.SecretKey)
	if err != nil {
		return err
	}
	m, err := manifestOf(ctx, client, key)
	if err != nil {
		return err
	}

	stamp := b.now().UTC().Format("20060102T150405Z")
	archivePath := filepath.Join(b.cfg.DataDir, "restore-"+stamp+".tar.gz")
	defer os.Remove(archivePath)
	sum, err := fetch(ctx, client, key, id, archivePath)
	if err != nil {
		return err
	}
	if sum != m.SHA256 {
		return fmt.Errorf("%s is not the archive its manifest describes: the contents hash to %s, the manifest says %s",
			path.Base(key), sum, m.SHA256)
	}

	// An archive from before the owner account existed would leave a running
	// app that nobody can sign in to and that never shows the setup page again,
	// because the gate in front of it has already latched.
	if m.Rows["users"] == 0 {
		return fmt.Errorf("%s was taken before there was an account to sign in with, so restoring it would lock everyone out",
			path.Base(key))
	}

	dbPath := filepath.Join(b.cfg.DataDir, "restore-"+stamp+".db")
	docsPath := filepath.Join(b.cfg.DataDir, "restore-"+stamp+".docs")
	defer os.Remove(dbPath)
	defer os.RemoveAll(docsPath)
	if err := unpack(archivePath, dbPath, docsPath); err != nil {
		return err
	}
	// The archive is only a backup if what came out of it is a database this
	// binary can open and migrate. Finding that out now costs one open and
	// leaves the live file untouched if the answer is no.
	check, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("the archive does not hold a database this version can open: %w", err)
	}
	if err := validateRestore(ctx, check); err != nil {
		check.Close()
		return err
	}
	if err := check.Close(); err != nil {
		return err
	}

	// From here the app answers writes with 503 until the files are in place.
	b.Freeze(true)
	defer b.Freeze(false)

	aside := b.db.Path() + "." + stamp + ".aside"
	if err := b.db.Swap(ctx, dbPath, aside); err != nil {
		return err
	}
	// The database is the backup's from here. Anything that fails after this is
	// a restore that half happened, and saying "nothing was changed" about it
	// would send whoever reads it looking in the wrong place.
	if err := b.afterSwap(ctx, docsPath, stamp, key, m, actorID); err != nil {
		return fmt.Errorf("%w: %w", ErrPartial, err)
	}
	b.log.Warn("restored from a backup", "key", key, "aside", aside)
	return nil
}

func validateRestore(ctx context.Context, db *store.DB) error {
	var result string
	if err := db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&result); err != nil {
		return fmt.Errorf("check restored database: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("the restored database failed its integrity check: %s", result)
	}
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return errors.New("the restored database has broken foreign key references")
	}
	if err := rows.Err(); err != nil {
		return err
	}
	var owners int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE role = 'owner'`).Scan(&owners); err != nil {
		return err
	}
	if owners == 0 {
		return errors.New("the restored database has no owner account")
	}
	return nil
}

// ErrPartial marks a failure after the database had already been replaced: the
// restore happened and the rest of it did not.
var ErrPartial = errors.New("the database was restored and the rest of the restore was not")

// afterSwap is everything that follows the database changing places: the
// settings in memory, the markdown mirror and the record of what was done.
func (b *Backup) afterSwap(ctx context.Context, docsPath, stamp, key string, m Manifest, actorID int64) error {
	// The values in memory were read from the file that has just been moved
	// aside.
	if err := b.set.Reload(ctx); err != nil {
		return fmt.Errorf("the settings could not be read from it: %w", err)
	}
	docs := filepath.Join(b.cfg.DataDir, docsEntry)
	if _, err := os.Stat(docs); err == nil {
		if err := os.Rename(docs, docs+"."+stamp+".aside"); err != nil {
			return fmt.Errorf("the markdown mirror could not be moved aside: %w", err)
		}
	}
	if _, err := os.Stat(docsPath); err == nil {
		if err := os.Rename(docsPath, docs); err != nil {
			return fmt.Errorf("the markdown mirror from the archive could not be put in place: %w", err)
		}
	}

	after, err := json.Marshal(m)
	if err != nil {
		return err
	}
	// The row goes into the restored database, which is the one that will be
	// read from now on, so the restore is in the history it created.
	if err := store.InsertActivity(ctx, b.db, "user", strconv.FormatInt(actorID, 10), "",
		"backup", key, "restore", "", string(after)); err != nil {
		return fmt.Errorf("the restore could not be recorded in activity: %w", err)
	}
	return nil
}

func manifestOf(ctx context.Context, client *blob.Client, key string) (Manifest, error) {
	r, err := client.Get(ctx, manifestKey(key))
	if errors.Is(err, blob.ErrNotFound) {
		return Manifest{}, fmt.Errorf("%s has no manifest beside it, so there is nothing to check it against", path.Base(key))
	}
	if err != nil {
		return Manifest{}, err
	}
	defer r.Close()
	var m Manifest
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("the manifest of %s cannot be read: %w", path.Base(key), err)
	}
	return m, nil
}

// fetch downloads and decrypts the archive to dst and returns its hash. age
// authenticates every chunk as it goes, so a changed byte fails here rather
// than surfacing as a broken database later.
func fetch(ctx context.Context, client *blob.Client, key string, id *age.X25519Identity, dst string) (string, error) {
	r, err := client.Get(ctx, key)
	if err != nil {
		return "", err
	}
	defer r.Close()
	plain, err := age.Decrypt(r, id)
	if err != nil {
		return "", fmt.Errorf("%s cannot be decrypted with this THESES_SECRET_KEY: %w", path.Base(key), err)
	}
	f, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, sum), plain); err != nil {
		return "", fmt.Errorf("%s did not come down whole: %w", path.Base(key), err)
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// unpack writes the two things an archive holds: the database at dbPath and the
// markdown mirror under docsPath. Names are checked even though the archive is
// authenticated, because the one thing a restore must never do is write outside
// the data directory.
func unpack(archivePath, dbPath, docsPath string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("the archive is not gzipped: %w", err)
	}
	defer gz.Close()

	seen := false
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("the archive is not readable: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		name := path.Clean(h.Name)
		if path.IsAbs(name) || strings.HasPrefix(name, "..") || h.Size > maxEntry {
			return fmt.Errorf("the archive holds an entry called %q", h.Name)
		}
		out := ""
		switch {
		case name == databaseEntry:
			out, seen = dbPath, true
		case strings.HasPrefix(name, docsEntry+"/"):
			out = filepath.Join(docsPath, filepath.FromSlash(strings.TrimPrefix(name, docsEntry+"/")))
		default:
			continue
		}
		if err := write(out, tr, h.Size); err != nil {
			return err
		}
	}
	if !seen {
		return fmt.Errorf("the archive holds no %s", databaseEntry)
	}
	return nil
}

func write(dst string, r io.Reader, size int64) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, io.LimitReader(r, size)); err != nil {
		return err
	}
	return f.Close()
}
