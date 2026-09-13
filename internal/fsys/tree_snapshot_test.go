package fsys

import (
	"errors"
	"testing"
)

func TestTreeSnapshotRestoreRestoresNestedTree(t *testing.T) {
	fs := NewFake()
	fs.Dirs["/city/agents"] = true
	fs.Dirs["/city/agents/worker"] = true
	fs.Modes["/city/agents"] = 0o750
	fs.Modes["/city/agents/worker"] = 0o700
	fs.Files["/city/agents/worker/agent.toml"] = []byte("provider = \"codex\"\n")
	fs.Modes["/city/agents/worker/agent.toml"] = 0o640

	snapshot, err := SnapshotTree(fs, "/city/agents")
	if err != nil {
		t.Fatalf("SnapshotTree: %v", err)
	}

	if err := RemoveAll(fs, "/city/agents"); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	fs.Dirs["/city/agents"] = true
	fs.Files["/city/agents/new.toml"] = []byte("stale\n")

	if err := snapshot.Restore(fs); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if _, ok := fs.Files["/city/agents/new.toml"]; ok {
		t.Fatalf("stale file still exists after restore")
	}
	if got := string(fs.Files["/city/agents/worker/agent.toml"]); got != "provider = \"codex\"\n" {
		t.Fatalf("restored agent.toml = %q", got)
	}
	if got := fs.Modes["/city/agents/worker/agent.toml"]; got != 0o640 {
		t.Fatalf("agent.toml mode = %#o, want 0640", got)
	}
	if got := fs.Modes["/city/agents/worker"]; got != 0o700 {
		t.Fatalf("worker dir mode = %#o, want 0700", got)
	}
}

func TestTreeSnapshotRestoreMissingRootRemovesCurrentRoot(t *testing.T) {
	fs := NewFake()

	snapshot, err := SnapshotTree(fs, "/city/agents")
	if err != nil {
		t.Fatalf("SnapshotTree: %v", err)
	}

	fs.Dirs["/city/agents"] = true
	fs.Files["/city/agents/new.toml"] = []byte("new\n")

	if err := snapshot.Restore(fs); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if fs.Dirs["/city/agents"] {
		t.Fatalf("root dir still exists after restoring missing snapshot")
	}
	if _, ok := fs.Files["/city/agents/new.toml"]; ok {
		t.Fatalf("file still exists after restoring missing snapshot")
	}
}

func TestTreeSnapshotRestorePreservesSymlinks(t *testing.T) {
	fs := NewFake()
	fs.Dirs["/city/agents"] = true
	fs.Dirs["/city/agents/worker"] = true
	fs.Symlinks["/city/agents/worker/skills"] = "/shared/skills"

	snapshot, err := SnapshotTree(fs, "/city/agents")
	if err != nil {
		t.Fatalf("SnapshotTree: %v", err)
	}

	if err := RemoveAll(fs, "/city/agents"); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	fs.Dirs["/city/agents"] = true
	fs.Symlinks["/city/agents/stale"] = "/stale"

	if err := snapshot.Restore(fs); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if _, ok := fs.Symlinks["/city/agents/stale"]; ok {
		t.Fatal("stale symlink still exists after restore")
	}
	if got := fs.Symlinks["/city/agents/worker/skills"]; got != "/shared/skills" {
		t.Fatalf("restored skills symlink = %q, want /shared/skills", got)
	}
}

func TestTreeSnapshotRestoreReportsRemoveError(t *testing.T) {
	fs := NewFake()
	fs.Dirs["/city/agents"] = true
	snapshot, err := SnapshotTree(fs, "/city/agents")
	if err != nil {
		t.Fatalf("SnapshotTree: %v", err)
	}

	removeErr := errors.New("busy")
	fs.Errors["/city/agents"] = removeErr

	err = snapshot.Restore(fs)
	if !errors.Is(err, removeErr) {
		t.Fatalf("Restore error = %v, want remove error", err)
	}
}
