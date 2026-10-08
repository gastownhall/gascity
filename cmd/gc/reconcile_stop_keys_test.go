package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Kills a legacy reader of the stop request's controller half (v5 R1's
// rollback rule: legacy ignores the new keys): no production file in cmd/gc
// or internal/session but the two that define them spells or names them.
// internal/session's resume CAS (D8) clears them through
// session.ClearStopRequestPatch.
func TestStopRequestKeysAreNewKeys(t *testing.T) {
	keys := []string{
		drainIntentReasonKey, drainIntentAtKey, drainIntentIncarnationKey,
		"DrainIntentReasonKey", "DrainIntentAtKey", "DrainIntentIncarnationKey",
	}
	owners := []string{"reconcile_stop_keys.go", "stop_request_keys.go"}
	for _, dir := range []string{".", filepath.Join("..", "..", "internal", "session")} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("%s: %d files, %v", dir, len(files), err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") || slices.Contains(owners, filepath.Base(f)) {
				continue
			}
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for _, k := range keys {
				if strings.Contains(string(data), k) {
					t.Errorf("%s spells the stop-request key %q", f, k)
				}
			}
		}
	}
}
