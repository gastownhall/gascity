package main

import (
	"os"
	"path/filepath"
	"strings"
)

// isTestBinary reports whether the current process is a Go test binary.
// Go test binaries are named *.test (e.g., "gc.test"). Used by runtime
// guards to prevent tests from accidentally hitting host infrastructure.
func isTestBinary() bool {
	if len(os.Args) == 0 {
		return false
	}
	// Bazel test binaries drop the .test suffix but export TEST_SRCDIR and
	// name the binary <target>_test. The TEST_SRCDIR marker alone is not
	// enough: a gc binary exec'd by a bazel test inherits TEST_SRCDIR from
	// the runner env but is not itself a test binary, and refusing it breaks
	// ambient city discovery for e2e helpers that rely on cwd resolution
	// (matching internal/supervisor, internal/config, internal/testenv).
	if os.Getenv("TEST_SRCDIR") != "" && strings.HasSuffix(filepath.Base(os.Args[0]), "_test") {
		return true
	}
	return strings.HasSuffix(os.Args[0], ".test") ||
		strings.Contains(os.Args[0], ".test")
}
