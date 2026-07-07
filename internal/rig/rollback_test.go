package rig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	fs := fsys.OSFS{}
	dir := t.TempDir()
	path := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(path, []byte("original = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	snap, err := SnapshotResolvedFile(fs, path)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if !snap.Exists || string(snap.Data) != "original = true\n" {
		t.Fatalf("snapshot did not capture existing contents: %+v", snap)
	}

	if err := os.WriteFile(path, []byte("mutated = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RestoreSnapshots(fs, []FileSnapshot{snap}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "original = true\n" {
		t.Fatalf("restore did not recover original, got %q", got)
	}
}

func TestSnapshotRestoreRemovesFileCreatedAfterSnapshot(t *testing.T) {
	fs := fsys.OSFS{}
	dir := t.TempDir()
	path := filepath.Join(dir, "new-rig", "routes.jsonl")

	// Snapshot a path that does not exist yet.
	snap, err := SnapshotResolvedFile(fs, path)
	if err != nil {
		t.Fatalf("snapshot missing: %v", err)
	}
	if snap.Exists {
		t.Fatalf("snapshot of a missing file should record Exists=false: %+v", snap)
	}

	// The provisioning step creates it, then fails and rolls back.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RestoreSnapshots(fs, []FileSnapshot{snap}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("restore should have removed the file created after the snapshot, stat err=%v", err)
	}
}

func TestRestoreSnapshotsUsesAtomicWrite(t *testing.T) {
	fs := fsys.NewFake()
	snap := FileSnapshot{Path: "/city/city.toml", Data: []byte("updated = true\n"), Exists: true}
	if err := RestoreSnapshots(fs, []FileSnapshot{snap}); err != nil {
		t.Fatalf("RestoreSnapshots: %v", err)
	}
	var renamed bool
	for _, call := range fs.Calls {
		if call.Method == "Rename" && strings.HasPrefix(call.Path, snap.Path+".tmp.") {
			renamed = true
			break
		}
	}
	if !renamed {
		t.Fatalf("fs calls = %+v, want atomic rename", fs.Calls)
	}
	if got := string(fs.Files[snap.Path]); got != "updated = true\n" {
		t.Fatalf("restored file = %q", got)
	}
}

func TestRestoreSnapshotsAggregatesAcrossFiles(t *testing.T) {
	fs := fsys.OSFS{}
	dir := t.TempDir()
	a := filepath.Join(dir, "a.toml")
	b := filepath.Join(dir, "b.toml")
	if err := os.WriteFile(a, []byte("a=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("b=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snaps := make([]FileSnapshot, 0, 2)
	for _, p := range []string{a, b} {
		s, err := SnapshotResolvedFile(fs, p)
		if err != nil {
			t.Fatal(err)
		}
		snaps = append(snaps, s)
	}
	_ = os.WriteFile(a, []byte("a=2\n"), 0o644)
	_ = os.WriteFile(b, []byte("b=2\n"), 0o644)
	if err := RestoreSnapshots(fs, snaps); err != nil {
		t.Fatalf("restore: %v", err)
	}
	ga, _ := os.ReadFile(a)
	gb, _ := os.ReadFile(b)
	if string(ga) != "a=1\n" || string(gb) != "b=1\n" {
		t.Fatalf("restore did not recover both files: a=%q b=%q", ga, gb)
	}
}
