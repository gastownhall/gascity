//go:build !windows

package main

import (
	"fmt"
	"os"
)

// createHookRunSpool creates the file gc hook run captures child stdout in.
// It is unlinked at once: this process's handle and the copies the child and
// its descendants inherit keep it alive, and it disappears with the last of
// them, even if this process dies first.
//
// Reading at an offset (pread) leaves the file offset the child writes at
// alone, so one open file serves as both the child's stdout and the reader.
func createHookRunSpool() (*hookRunSpool, error) {
	f, err := os.CreateTemp("", "gc-hook-run-stdout-*")
	if err != nil {
		return nil, err
	}
	if err := os.Remove(f.Name()); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("unlinking output spool: %w", err)
	}
	return &hookRunSpool{out: f, in: f}, nil
}
