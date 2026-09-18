package backup

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestSweepTakesOnlyWhatAnInterruptedRunLeft(t *testing.T) {
	dir := t.TempDir()
	write := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// What a killed run leaves.
	write("restore-20260917T033000Z.tar.gz")
	write("restore-20260917T033000Z.db")
	write("backup-20260917T033000Z.db")
	if err := os.MkdirAll(filepath.Join(dir, "restore-20260917T033000Z.docs", "10-proposition"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join("restore-20260917T033000Z.docs", "10-proposition", "research.md"))
	// What it must not touch: the database, the mirror, and the copies a
	// restore moved aside to be the way back.
	write("theses.db")
	write("theses.db.20260917T033000Z.aside")
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join("docs", "research.md"))

	if err := Sweep(dir, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}

	for _, gone := range []string{
		"restore-20260917T033000Z.tar.gz", "restore-20260917T033000Z.db",
		"backup-20260917T033000Z.db", "restore-20260917T033000Z.docs",
	} {
		if _, err := os.Stat(filepath.Join(dir, gone)); err == nil {
			t.Errorf("%s was left behind", gone)
		}
	}
	for _, kept := range []string{
		"theses.db", "theses.db.20260917T033000Z.aside", filepath.Join("docs", "research.md"),
	} {
		if _, err := os.Stat(filepath.Join(dir, kept)); err != nil {
			t.Errorf("%s was swept away: %v", kept, err)
		}
	}
}
