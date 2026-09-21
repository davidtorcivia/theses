package backup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/docs"
	"github.com/davidtorcivia/theses/internal/settings"
	"github.com/davidtorcivia/theses/internal/store"
)

type Verification struct {
	Documents int           `json:"documents"`
	Objects   int           `json:"objects"`
	Duration  time.Duration `json:"duration"`
}

// Verify runs restore preparation and mirror reconciliation in an isolated directory.
func (b *Backup) Verify(ctx context.Context, key string) (Verification, error) {
	if !b.busy.CompareAndSwap(false, true) {
		return Verification{}, ErrBusy
	}
	defer b.busy.Store(false)
	return b.verify(ctx, key)
}

func (b *Backup) verify(ctx context.Context, key string) (Verification, error) {
	start := time.Now()
	dir, err := os.MkdirTemp(b.cfg.DataDir, "verify-")
	if err != nil {
		return Verification{}, err
	}
	defer os.RemoveAll(dir)
	if _, err := b.prepare(ctx, key, dir); err != nil {
		return Verification{}, err
	}
	db, err := store.Open(filepath.Join(dir, databaseEntry))
	if err != nil {
		return Verification{}, err
	}
	defer db.Close()
	mirror := docs.New(core.New(db, core.NewBus()), filepath.Join(dir, docsEntry), func() string { return "" }, b.log)
	defer mirror.Stop()
	if err := mirror.CheckMirror(ctx); err != nil {
		return Verification{}, err
	}
	if err := validateRestore(ctx, db); err != nil {
		return Verification{}, err
	}
	set, err := settings.Open(ctx, db, b.cfg.SecretKey)
	if err != nil {
		return Verification{}, err
	}
	var result Verification
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM documents`).Scan(&result.Documents); err != nil {
		return result, err
	}
	rows, err := db.QueryContext(ctx, `SELECT folder, object_key, size FROM files WHERE state = 'ready' ORDER BY id LIMIT 10`)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var folder, key string
		var size int64
		if err := rows.Scan(&folder, &key, &size); err != nil {
			return result, err
		}
		if b.CheckObject == nil {
			return result, fmt.Errorf("object verification is unavailable")
		}
		if err := b.CheckObject(ctx, set, folder, key, size); err != nil {
			return result, fmt.Errorf("referenced object check: %w", err)
		}
		result.Objects++
	}
	result.Duration = time.Since(start)
	return result, rows.Err()
}

func (b *Backup) VerifyNow(ctx context.Context, key string) error {
	if !b.busy.CompareAndSwap(false, true) {
		return ErrBusy
	}
	run, cancel := context.WithTimeout(context.WithoutCancel(ctx), runTimeout)
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer b.busy.Store(false)
		defer cancel()
		result, err := b.verify(run, key)
		b.recordVerification(run, result, err)
	}()
	return nil
}

func (b *Backup) recordVerification(ctx context.Context, result Verification, err error) {
	record, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	msg := fmt.Sprintf("Verified %d documents and %d sampled objects in %s at %s.", result.Documents, result.Objects, result.Duration.Round(time.Millisecond), b.now().UTC().Format(time.RFC3339))
	if err != nil {
		msg = "Backup verification failed: " + err.Error()
		b.log.Error("backup verification", "err", err)
	}
	if err := b.set.SetAs(record, "backups.last_verify", []string{msg}, settings.System()); err != nil {
		b.log.Error("record backup verification", "err", err)
	}
}
