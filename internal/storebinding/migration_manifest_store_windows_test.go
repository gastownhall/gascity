//go:build windows

package storebinding

import (
	"os"
	"path/filepath"
	"testing"
)

func TestManifestStoreUsesWindowsDirectorySyncHandle(t *testing.T) {
	t.Run("open store", func(t *testing.T) {
		if _, err := OpenManifestStore(t.TempDir()); err != nil {
			t.Fatalf("OpenManifestStore: %v", err)
		}
	})

	t.Run("write manifest", func(t *testing.T) {
		directory := filepath.Join(t.TempDir(), manifestDirectoryName)
		if err := os.MkdirAll(directory, manifestDirMode); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		store := &ManifestStore{directory: directory}
		saga := migrationRelocationSaga(t)
		saga.advanceTo(t, PhaseActiveManifestDurable)
		if err := store.SaveActive(saga.manifest); err != nil {
			t.Fatalf("SaveActive: %v", err)
		}
	})
}
