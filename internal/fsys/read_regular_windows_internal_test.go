//go:build windows

package fsys

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadRegularFileSnapshot_MatchesPathFileIdentity checks the two halves of
// WriteFileIfChangedAtomic's stability check agree on Windows: the identity
// read through the snapshot's handle equals the identity read by path, and an
// atomic replacement of the file changes the latter.
func TestReadRegularFileSnapshot_MatchesPathFileIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	data := []byte("hello = true\n")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	snapshot, err := (OSFS{}).readRegularFileSnapshot(path)
	if err != nil {
		t.Fatalf("readRegularFileSnapshot: %v", err)
	}
	if !snapshot.hasID {
		t.Fatal("snapshot missing identity")
	}
	if string(snapshot.data) != string(data) {
		t.Fatalf("snapshot data = %q, want %q", snapshot.data, data)
	}
	id, ok := (OSFS{}).pathFileIdentity(path)
	if !ok {
		t.Fatal("pathFileIdentity returned ok=false")
	}
	if snapshot.id != id {
		t.Fatalf("snapshot.id = %#v, want the path identity %#v", snapshot.id, id)
	}

	if err := WriteFileAtomic(OSFS{}, path, []byte("hello = false\n"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	replaced, ok := (OSFS{}).pathFileIdentity(path)
	if !ok {
		t.Fatal("pathFileIdentity after replace returned ok=false")
	}
	if replaced == snapshot.id {
		t.Fatal("identity unchanged after an atomic replace")
	}
}

func TestReadRegularFileSnapshot_RejectsDirectory(t *testing.T) {
	if _, err := (OSFS{}).readRegularFileSnapshot(t.TempDir()); err == nil {
		t.Fatal("readRegularFileSnapshot of a directory succeeded, want an error")
	}
}
