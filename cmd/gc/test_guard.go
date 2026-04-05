package main

import (
	"os"
	"strings"
)

// isTestBinary reports whether the current process is a Go test binary.
// Go test binaries are named *.test (e.g., "gc.test"). Used by runtime
// guards to prevent tests from accidentally hitting host infrastructure.
func isTestBinary() bool {
	if len(os.Args) == 0 {
		return false
	}
	return strings.HasSuffix(os.Args[0], ".test") ||
		strings.Contains(os.Args[0], ".test")
}
