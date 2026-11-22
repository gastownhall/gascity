//go:build linux

package proctable

import (
	"errors"
	"io/fs"
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

// ProcessCwd reads pid's cwd link under scanRoot. A process that is gone has
// no working directory: an error, never a directory.
func TestProcessCwdReadsTheCwdLinkUnderScanRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "4242"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/home/user/city", filepath.Join(root, "4242", "cwd")); err != nil {
		t.Fatal(err)
	}
	SetScanRootForTesting(t, root)

	if got, err := ProcessCwd(4242); err != nil || got != "/home/user/city" {
		t.Fatalf("ProcessCwd = %q, %v; want /home/user/city", got, err)
	}
	if got, err := ProcessCwd(4343); !errors.Is(err, fs.ErrNotExist) || got != "" {
		t.Fatalf("ProcessCwd(gone pid) = %q, %v; want empty and a not-exist error", got, err)
	}
}

func TestProcessCwdRefusesTheLiveProcUnderTest(t *testing.T) {
	if _, err := ProcessCwd(os.Getpid()); err == nil {
		t.Fatal("ProcessCwd read the live /proc under go test; want liveScanGuard's refusal")
	}
}
