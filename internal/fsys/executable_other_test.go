//go:build !windows

package fsys

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestIsExecutableReadsTheExecuteBits(t *testing.T) {
	dir := t.TempDir()
	for mode, want := range map[os.FileMode]bool{
		0o755: true,
		0o700: true,
		0o710: true,
		0o701: true,
		0o644: false,
		0o600: false,
		0o666: false,
	} {
		// The name ends in .exe so the Windows rule, were it applied here,
		// would answer differently for every non-executable mode.
		path := filepath.Join(dir, fmt.Sprintf("gc-%o.exe", mode))
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := IsExecutable(info); got != want {
			t.Errorf("IsExecutable(mode %o) = %t, want %t", mode, got, want)
		}
	}
}
