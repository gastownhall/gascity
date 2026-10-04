//go:build integration

package integration

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestInstallDriftGCBinary_StampsEachCommit pins that every commit ID the
// start-drift suite fakes resolves to a gc binary stamped with that commit
// and no other (a prebuilt x_defs variant under bazel, a once-per-process
// build under plain go test), so a swapped or mislabeled variant fails here
// rather than as a confusing drift timeout. The drift tests themselves prove
// the stamp end to end through /health build_id.
func TestInstallDriftGCBinary_StampsEachCommit(t *testing.T) {
	ids := driftCommitIDs()
	for _, commitID := range ids {
		t.Run(commitID, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), "gc")
			installDriftGCBinary(t, bin, commitID)

			data, err := os.ReadFile(bin)
			if err != nil {
				t.Fatalf("reading installed binary: %v", err)
			}
			for _, other := range ids {
				stamped := bytes.Contains(data, []byte(other))
				if want := other == commitID; stamped != want {
					t.Errorf("binary for commit %s: contains %q = %v, want %v", commitID, other, stamped, want)
				}
			}
		})
	}
}

// TestInstallDriftGCBinary_ReplacesAtomically pins the on-disk semantics the
// drift detection relies on: installing over a path swaps in a NEW inode (a
// running supervisor keeps the old one, so /proc/<pid>/exe gains " (deleted)"),
// the result is 0o755, and no temp file is left beside it.
func TestInstallDriftGCBinary_ReplacesAtomically(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "gc-drift")

	installDriftGCBinary(t, bin, driftHappyOldCommit)
	before := statInode(t, bin)
	installDriftGCBinary(t, bin, driftHappyNewCommit)
	after := statInode(t, bin)

	if before == after {
		t.Fatalf("inode unchanged (%d) across install; replacement must be a rename, not an in-place write", after)
	}
	info, err := os.Stat(bin)
	if err != nil {
		t.Fatalf("stat installed binary: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("installed binary mode = %o, want 755", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading install dir: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("install dir holds %v, want only %q", names, filepath.Base(bin))
	}
}

func statInode(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skipf("inode not available on this platform")
	}
	return st.Ino
}
