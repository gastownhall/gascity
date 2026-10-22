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

func TestTreeSnapshotEqual(t *testing.T) {
	seed := func() *Fake {
		fs := NewFake()
		fs.Dirs["/city/agents"] = true
		fs.Dirs["/city/agents/worker"] = true
		fs.Modes["/city/agents/worker"] = 0o700
		fs.Files["/city/agents/worker/agent.toml"] = []byte("provider = \"codex\"\n")
		fs.Modes["/city/agents/worker/agent.toml"] = 0o640
		fs.Symlinks["/city/agents/worker/skills"] = "/shared/skills"
		return fs
	}
	snapshot := func(t *testing.T, fs *Fake, root string) *TreeSnapshot {
		t.Helper()
		s, err := SnapshotTree(fs, root)
		if err != nil {
			t.Fatalf("SnapshotTree: %v", err)
		}
		return s
	}
	want := snapshot(t, seed(), "/city/agents")
	for _, tc := range []struct {
		name string
		edit func(*Fake)
		root string
		want bool
	}{
		{name: "the same tree", want: true},
		{name: "a file's contents differ", edit: func(fs *Fake) { fs.Files["/city/agents/worker/agent.toml"] = []byte("provider = \"claude\"\n") }},
		{name: "a file's mode differs", edit: func(fs *Fake) { fs.Modes["/city/agents/worker/agent.toml"] = 0o600 }},
		{name: "a file was added", edit: func(fs *Fake) { fs.Files["/city/agents/worker/prompt.md"] = nil }},
		{name: "a directory's mode differs", edit: func(fs *Fake) { fs.Modes["/city/agents/worker"] = 0o750 }},
		{name: "a directory was added", edit: func(fs *Fake) { fs.Dirs["/city/agents/reviewer"] = true }},
		{name: "a symlink's target differs", edit: func(fs *Fake) { fs.Symlinks["/city/agents/worker/skills"] = "/other/skills" }},
		{name: "a file replaced a symlink", edit: func(fs *Fake) {
			delete(fs.Symlinks, "/city/agents/worker/skills")
			fs.Files["/city/agents/worker/skills"] = nil
		}},
		{name: "the root is gone", edit: func(fs *Fake) {
			if err := RemoveAll(fs, "/city/agents"); err != nil {
				t.Fatalf("RemoveAll: %v", err)
			}
		}},
		{name: "another root", root: "/city/agents/worker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := seed()
			if tc.edit != nil {
				tc.edit(fs)
			}
			root := "/city/agents"
			if tc.root != "" {
				root = tc.root
			}
			got := snapshot(t, fs, root)
			if want.Equal(got) != tc.want || got.Equal(want) != tc.want {
				t.Fatalf("Equal = %v/%v, want %v", want.Equal(got), got.Equal(want), tc.want)
			}
		})
	}
	t.Run("two missing roots", func(t *testing.T) {
		if !snapshot(t, NewFake(), "/city/agents").Equal(snapshot(t, NewFake(), "/city/agents")) {
			t.Fatal("snapshots of the same missing root differ")
		}
	})
	t.Run("nil", func(t *testing.T) {
		var none *TreeSnapshot
		if !none.Equal(nil) {
			t.Fatal("a nil snapshot differs from nil")
		}
		if none.Equal(want) || want.Equal(nil) {
			t.Fatal("a nil snapshot equals a captured one")
		}
	})
}
