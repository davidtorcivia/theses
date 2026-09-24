package backup

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Sweep removes what a backup, a restore or a verification left in the data
// directory when it was killed between making a temp file and removing it: the
// copy of the database, the archive on its way down and the mirror on its way
// out, any of which is as large as the data itself.
//
// It runs from main before the database is open, which is the one moment
// nothing can be using them, so it needs no age check. It leaves the copies a
// restore moves aside under a timestamp alone: those are the way back.
func Sweep(dir string, log *slog.Logger) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("backup: sweep %s: %w", dir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "backup-") && !strings.HasPrefix(name, "restore-") &&
			!strings.HasPrefix(name, "verify-") {
			continue
		}
		// One that will not go is not a reason to refuse to start; the disk it
		// is filling is a problem either way and the log says which file.
		if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
			log.Error("leftover from an interrupted backup could not be removed", "name", name, "err", err)
			continue
		}
		log.Info("removed a leftover from an interrupted backup", "name", name)
	}
	return nil
}
