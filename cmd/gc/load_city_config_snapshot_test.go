package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestCityConfigLoadersDeclineTheRevisionSnapshot pins a file-level invariant:
// every city-config loader in cmd_agent.go discards the Provenance, so every one
// of them must decline the load-time revision snapshot.
//
// The snapshot content-hashes every pack directory so a later config.Revision()
// can compare against the tree as it was loaded. These loaders return only
// *config.City — they use the Provenance to emit warnings and then drop it — so
// nothing they load can observe the snapshot, and building it is pure cost on a
// one-shot command.
//
// This is a source-level guard rather than a behavioral one because the cost is
// invisible by construction: a loader that reverts to the default returns
// exactly the same config and passes every functional test, it just re-reads
// every pack file. Nothing fails; it only gets slower, which is precisely the
// regression a test suite does not otherwise catch.
//
// It follows the file-scanning idiom of TestGCNonTestFilesStayOnWorkerBoundary
// and TestGCNonTestFilesStayOnRigProvisionBoundary: read the guarded file from a
// runtime.Caller-derived directory rather than the process working directory,
// and match with substring needles.
//
// Scope is deliberately narrow. Only cmd_agent.go's loaders are known to discard
// the Provenance; roughly a dozen other call sites elsewhere in the tree do the
// same thing and are out of scope for this change and this guard.
//
// If a loader here is ever changed to RETURN the Provenance it should keep the
// default instead, and this test should be updated to exempt it by name rather
// than deleted.
func TestCityConfigLoadersDeclineTheRevisionSnapshot(t *testing.T) {
	const guarded = "cmd_agent.go"
	const capturingCall = "config.LoadWithIncludes("
	const decliningCall = "config.LoadWithIncludesOptions("
	const option = "skipRevisionSnapshot"

	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	src, err := os.ReadFile(filepath.Join(filepath.Dir(currentFile), guarded))
	if err != nil {
		t.Fatalf("reading %s: %v", guarded, err)
	}
	text := string(src)

	// capturingCall ends in '(' so it does not also match decliningCall, whose
	// next character is 'O'.
	if strings.Contains(text, capturingCall) {
		t.Errorf("%s calls %s — that form always captures the revision snapshot; use %s..., %s)",
			guarded, capturingCall, decliningCall, option)
	}

	// Scan line by line rather than with a bracket-matching regex: a pattern
	// like `\([^)]*\)` truncates at the first ')', so a future call containing a
	// nested call expression would fail spuriously with a misleading message.
	found := 0
	for i, line := range strings.Split(text, "\n") {
		if !strings.Contains(line, decliningCall) {
			continue
		}
		found++
		if !strings.Contains(line, option) {
			t.Errorf("%s:%d does not decline the revision snapshot: %s",
				guarded, i+1, strings.TrimSpace(line))
		}
	}
	if found == 0 {
		t.Fatalf("no %s call in %s; this guard is no longer watching anything", decliningCall, guarded)
	}
}
