//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package fsys

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadRegularFileSnapshot_MatchesFileIdentityFromInfo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	data := []byte("hello = true\n")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	snapshot, err := (OSFS{}).readRegularFileSnapshot(path)
	if err != nil {
		t.Fatalf("readRegularFileSnapshot: %v", err)
	}
	if !snapshot.hasID {
		t.Fatalf("snapshot missing identity")
	}

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	id, ok := fileIdentityFromInfo(info)
	if !ok {
		t.Fatalf("fileIdentityFromInfo returned ok=false")
	}
	if snapshot.id != id {
		t.Fatalf("snapshot.id = %#v, want %#v", snapshot.id, id)
	}
}
