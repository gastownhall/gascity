//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// installFakeBdOnPath puts a fake bd first on PATH for the rest of the test,
// and returns the file its runs are recorded in, or "" where nothing records
// them. Here the fake is posix, a POSIX script written as an executable bd;
// Windows runs the recording native fake instead, answering with the replies.
func installFakeBdOnPath(t *testing.T, posix string, _ map[string]nativeFakeBdReply) string {
	t.Helper()
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "bd"), []byte(posix), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("PATH", tmp+string(os.PathListSeparator)+os.Getenv("PATH"))
	return ""
}
