package dolt_test

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
)

// repoRoot locates this pack's source directory.
//
// It lives in an untagged file because the pack's portable tests -- agent
// discovery and prompt-fragment comparison -- need it on every platform, while
// the tests that drive the pack's POSIX shell commands are unix-tagged.
func repoRoot(t *testing.T) string {
	t.Helper()
	if root := bazeltest.OverrideRoot(); root != "" {
		return filepath.Join(root, "examples", "bd", "dolt")
	}
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Dir(filename)
}
