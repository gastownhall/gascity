package main

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime/proctable"
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

// hostGuardedProcess reports whether the host-protection guards (those that
// panic rather than touch the operator's real supervisor socket or systemctl)
// must treat this process as a test. It adds the marker the integration
// harness stamps on the gc binaries it builds and execs, which carry no
// ".test" in argv[0]. isTestBinary stays argv-based on purpose: it also gates
// behavior (ambient city discovery, GC_BIN resolution, managed-dolt test mode)
// that those spawned binaries must keep.
func hostGuardedProcess() bool {
	return isTestBinary() || proctable.TestSpawned()
}
