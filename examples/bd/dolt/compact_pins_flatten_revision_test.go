package dolt_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCompactScriptPinsFlattenRevisionAgainstConcurrentCommitBetweenVerificationReads
// asserts that post-flatten verification pinned to flatten_head is immune to a
// writer commit that lands between sequential verification reads. Unpinned
// reads in this mode return drifted values, so dropping the pin fails the test.
func TestCompactScriptPinsFlattenRevisionAgainstConcurrentCommitBetweenVerificationReads(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "pin_no_false_quarantine_concurrent_writer", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("compact should succeed when post-flatten reads are pinned to flatten_head: %v\n%s", err, out)
	}
	if strings.Contains(out, "quarantine") || strings.Contains(out, "INTEGRITY check failed") {
		t.Fatalf("pinned flatten revision must not quarantine a concurrent writer between verification reads:\n%s", out)
	}
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("pinned flatten revision must not write a quarantine marker; stat=%v", err)
	}
}

// TestCompactScriptStillQuarantinesRealDriftAtPinnedFlattenRevision asserts that
// pinning to flatten_head does not mask genuine same-row-count table-hash drift
// that exists in the flatten's own committed revision.
func TestCompactScriptStillQuarantinesRealDriftAtPinnedFlattenRevision(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "pin_real_drift_at_flatten_head_quarantines", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded despite genuine table-hash drift at flatten_head:\n%s", out)
	}
	if !strings.Contains(out, "post-flatten INTEGRITY check failed") {
		t.Fatalf("output missing integrity-check failure:\n%s", out)
	}
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	if reason := compactMarkerValue(t, marker, "reason"); reason != "post-flatten table value hash changed without row-count increase" {
		t.Fatalf("quarantine reason should identify same-count hash drift at flatten_head, got %q", reason)
	}
}
