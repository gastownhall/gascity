package bazeltest

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// DataPath resolves a file a go_test declares as data and passes to the test
// binary by its $(rootpath) in the environment variable name, e.g.
//
//	data = ["//cmd/tool"],
//	env = {"GC_TEST_TOOL_BIN": "$(rootpath //cmd/tool)"},
//
// It returns the file's absolute path in the runfiles tree, or "" when the
// variable is unset or empty: plain `go test` passes nothing and the caller
// falls back to its own setup (typically `go build`). A variable that is set
// but does not resolve to a runfile fails the test, so a broken declaration
// can never silently degrade into the fallback.
func DataPath(t testing.TB, name string) string {
	t.Helper()
	rel := os.Getenv(name)
	if rel == "" {
		return ""
	}
	path, err := dataPath(rel, os.Getenv("TEST_SRCDIR"), os.Getenv("TEST_WORKSPACE"))
	if err != nil {
		t.Fatalf("%s=%s: %v", name, rel, err)
	}
	return path
}

// dataPath joins a $(rootpath) (relative to the main repository's runfiles
// directory; external repositories appear as ../<repo>/...) under
// srcdir/workspace and checks that the file exists.
func dataPath(rel, srcdir, workspace string) (string, error) {
	if srcdir == "" || workspace == "" {
		return "", fmt.Errorf("not under bazel test (TEST_SRCDIR=%q, TEST_WORKSPACE=%q)", srcdir, workspace)
	}
	path := filepath.Join(srcdir, workspace, filepath.FromSlash(rel))
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("declared data file: %w", err)
	}
	return path, nil
}
