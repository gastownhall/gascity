//go:build linux

package proctable

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestOpenFilesReadsTheFDLinksUnderScanRoot(t *testing.T) {
	root := t.TempDir()
	fd := filepath.Join(root, "4242", "fd")
	if err := os.MkdirAll(fd, 0o755); err != nil {
		t.Fatal(err)
	}
	const rollout = "/home/user/.codex/sessions/2026/10/15/rollout-2026-10-15T14-36-37-01a13ffe-60c1-7c61-bc2c-87801bfee88f.jsonl"
	for name, target := range map[string]string{
		"0":  "/dev/pts/3",
		"3":  rollout,
		"7":  "pipe:[123]",
		"12": "socket:[456]",
	} {
		if err := os.Symlink(target, filepath.Join(fd, name)); err != nil {
			t.Fatal(err)
		}
	}
	SetScanRootForTesting(t, root)

	got, err := OpenFiles(4242)
	if err != nil {
		t.Fatalf("OpenFiles = %v", err)
	}
	slices.Sort(got)
	if want := []string{"/dev/pts/3", rollout}; !slices.Equal(got, want) {
		t.Fatalf("OpenFiles = %q, want %q (paths only, no pipes or sockets)", got, want)
	}
	if got, err := OpenFiles(4343); err != nil || got != nil {
		t.Fatalf("OpenFiles(gone pid) = %q, %v; want nothing, nil", got, err)
	}
}

func TestOpenFilesRefusesTheLiveProcUnderTest(t *testing.T) {
	if _, err := OpenFiles(os.Getpid()); err == nil {
		t.Fatal("OpenFiles read the live /proc under go test; want liveScanGuard's refusal")
	}
}
