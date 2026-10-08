package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Kills a legacy reader of the stop request's controller half (v5 R1's
// rollback rule: legacy ignores the new keys): no other production file in
// cmd/gc or internal/session spells them.
func TestStopRequestKeysAreNewKeys(t *testing.T) {
	keys := []string{drainIntentReasonKey, drainIntentAtKey, drainIntentIncarnationKey}
	for _, dir := range []string{".", filepath.Join("..", "..", "internal", "session")} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("%s: %d files, %v", dir, len(files), err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") || filepath.Base(f) == "reconcile_stop_keys.go" {
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
