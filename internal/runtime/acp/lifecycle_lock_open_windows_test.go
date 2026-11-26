//go:build windows

package acp

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenLifecycleLockFileRejectsReparsePoint(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	link := filepath.Join(dir, "lock")
	if output, err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Skipf("directory junctions unavailable: %v: %s", err, output)
	}

	f, err := openLifecycleLockFile(link)
	if f != nil {
		_ = f.Close()
	}
	if err == nil {
		t.Fatal("openLifecycleLockFile accepted a reparse point")
	}
	if !strings.Contains(err.Error(), "reparse point") {
		t.Fatalf("error = %v, want explicit reparse-point rejection", err)
	}
}
