package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/session"
)

// stopKeyOwners are the only production files that may name the stop
// request's keys: the definitions, the C6a accessor and patch builders, and
// E3's drain-ack CLI (the request half). Other writers clear them through
// session.ClearStopRequestPatch, which lives with the definitions.
var stopKeyOwners = []string{"internal/session/stop_request_keys.go", "cmd/gc/reconcile_stop_request.go", "cmd/gc/cmd_runtime_drain.go"}

// Kills a legacy reader of the stop request's controller half (v5 R1's
// rollback rule: legacy ignores the new keys): no production Go file under
// cmd/, internal/ or pkg/ but stopKeyOwners spells a key of either half or
// names its exported constant.
func TestStopRequestKeysAreNewKeys(t *testing.T) {
	banned := []string{
		session.DrainIntentReasonKey, session.DrainIntentAtKey, session.DrainIntentIncarnationKey,
		session.DrainAckIncarnationKey, session.DrainAckAtKey,
		"DrainIntentReasonKey", "DrainIntentAtKey", "DrainIntentIncarnationKey", "DrainAckIncarnationKey", "DrainAckAtKey",
	}
	root := repoRootForLint(t)
	scanned, owners := 0, 0
	for _, dir := range []string{"cmd", "internal", "pkg"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if slices.Contains(stopKeyOwners, filepath.ToSlash(rel)) {
				owners++
				return nil
			}
			scanned++
			for _, k := range banned {
				if strings.Contains(string(data), k) {
					t.Errorf("%s names the stop-request key %q", rel, k)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", dir, err)
		}
	}
	if owners != len(stopKeyOwners) || scanned < 1000 {
		t.Fatalf("scanned %d files and %d of %d owners; the scan missed the tree", scanned, owners, len(stopKeyOwners))
	}
}
