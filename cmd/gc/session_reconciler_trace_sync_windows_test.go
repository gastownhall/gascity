//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSyncTraceAncestorsUsesWindowsDirectorySyncHandle(t *testing.T) {
	root := t.TempDir()
	stop := filepath.Join(root, ".gc", "runtime", "session-reconciler-trace", "segments")
	dir := filepath.Join(stop, "2026", "10", "04")
	if err := os.MkdirAll(dir, sessionReconcilerTraceOwnerDirPerm); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	if err := syncTraceAncestors(dir, stop); err != nil {
		t.Fatalf("syncTraceAncestors: %v", err)
	}
}
