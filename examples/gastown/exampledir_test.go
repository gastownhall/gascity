package gastown_test

import (
	"path/filepath"
	"runtime"

	"github.com/gastownhall/gascity/internal/bazeltest"
)

// exampleDir locates this example's source directory.
//
// It lives in an untagged file because the example's portable tests -- the
// embedded pack contents, operational awareness, and the precompact hook --
// need it on every platform, while the tests that run its POSIX maintenance
// scripts are unix-tagged.
func exampleDir() string {
	if root := bazeltest.OverrideRoot(); root != "" {
		return filepath.Join(root, "examples", "gastown")
	}
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Dir(filename)
}
