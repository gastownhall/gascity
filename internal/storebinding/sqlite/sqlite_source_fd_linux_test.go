//go:build linux

package sqlite

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSQLiteFenceRefusesSourceDescriptorOpenInThisProcess(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), graphFilename)
	if err := os.WriteFile(databasePath, sqliteHeaderForTest(1, 1), 0o600); err != nil {
		t.Fatalf("writing SQLite source: %v", err)
	}
	source, err := os.Open(databasePath)
	if err != nil {
		t.Fatalf("opening SQLite source descriptor: %v", err)
	}
	t.Cleanup(func() { _ = source.Close() })

	err = ensureNoSQLiteSourceDescriptors(databasePath)
	if !errors.Is(err, ErrSQLiteSourceOpenInProcess) {
		t.Fatalf("ensureNoSQLiteSourceDescriptors() error = %v, want ErrSQLiteSourceOpenInProcess", err)
	}
}

func TestSQLiteFenceRefusesHardLinkAliasDescriptorOpenInThisProcess(t *testing.T) {
	directory := t.TempDir()
	databasePath := filepath.Join(directory, graphFilename)
	if err := os.WriteFile(databasePath, sqliteHeaderForTest(1, 1), 0o600); err != nil {
		t.Fatalf("writing SQLite source: %v", err)
	}
	aliasPath := filepath.Join(directory, "source-alias.sqlite")
	if err := os.Link(databasePath, aliasPath); err != nil {
		t.Fatalf("creating source hard-link alias: %v", err)
	}
	source, err := os.Open(aliasPath)
	if err != nil {
		t.Fatalf("opening aliased SQLite source descriptor: %v", err)
	}
	t.Cleanup(func() { _ = source.Close() })

	err = ensureNoSQLiteSourceDescriptors(databasePath)
	if !errors.Is(err, ErrSQLiteSourceOpenInProcess) {
		t.Fatalf("ensureNoSQLiteSourceDescriptors() error = %v, want ErrSQLiteSourceOpenInProcess", err)
	}
}
