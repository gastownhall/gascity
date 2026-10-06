package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
)

// TestNewBeadsPreflightCheckerWiresDatabaseSchemaCursors is the G4 re-review's
// M8 regression test: newBeadsPreflightChecker, the production
// contract.PreflightChecker constructor, must wire a non-nil
// DatabaseSchemaCursors reader. A mutation that un-wires that field (deletes
// its line from the constructor, or wires it to nil) must fail this test — the
// resulting checker would silently skip the schema gate for every city it
// composes, exactly as if the database could never answer.
func TestNewBeadsPreflightCheckerWiresDatabaseSchemaCursors(t *testing.T) {
	checker := newBeadsPreflightChecker("/city", "")

	if checker.DatabaseSchemaCursors == nil {
		t.Fatal("newBeadsPreflightChecker left DatabaseSchemaCursors nil: the schema gate would never run")
	}
	if checker.DatabaseProjectID == nil {
		t.Fatal("newBeadsPreflightChecker left DatabaseProjectID nil")
	}
	if checker.DeferIdentityToNativeOpen == nil {
		t.Fatal("newBeadsPreflightChecker left DeferIdentityToNativeOpen nil")
	}
	if checker.AllowSchemaBehindMigrate == nil {
		t.Fatal("newBeadsPreflightChecker left AllowSchemaBehindMigrate nil")
	}
	if checker.SchemaLatestIgnoredVersion != beads.SchemaCursorIgnored {
		t.Fatalf("SchemaLatestIgnoredVersion = %d, want beads.SchemaCursorIgnored (%d)", checker.SchemaLatestIgnoredVersion, beads.SchemaCursorIgnored)
	}
	if checker.BDContext == nil {
		t.Fatal("newBeadsPreflightChecker left BDContext nil")
	}
}

// TestPreflightSchemaCursorsFromReportUsesTheClampedIgnoredValue is the G4
// re-review's M9 regression test: preflightSchemaCursorsFromReport (and so
// preflightDatabaseSchemaCursorsReader, which delegates to it) must report the
// CLAMPED ignored-lane cursor (CursorReality.EffectiveIgnored), not the raw
// on-disk counter, whenever the reality is Limited and its Floor sits below
// the raw value. A mutation that compares/returns the raw
// report.Cursors.Ignored instead must fail this test.
func TestPreflightSchemaCursorsFromReportUsesTheClampedIgnoredValue(t *testing.T) {
	const rawIgnored = 42
	const floor = 7

	report := proxyendpoint.CursorReportForTest(
		proxyendpoint.Cursors{Main: 5, Ignored: rawIgnored},
		proxyendpoint.CursorReality{Limited: true, Floor: floor, Missing: "dolt_ignore"},
	)

	got := preflightSchemaCursorsFromReport(report)

	if got.Main != 5 {
		t.Errorf("Main = %d, want 5", got.Main)
	}
	if got.Ignored != floor {
		t.Errorf("Ignored = %d, want the clamped floor %d (raw was %d): a schema gate that reads the raw counter trusts a cursor the live schema does not corroborate", got.Ignored, floor, rawIgnored)
	}
	if !got.IgnoredChecked {
		t.Error("IgnoredChecked = false, want true: CursorReportForTest marks the reality checked")
	}
}

// TestPreflightSchemaCursorsFromReportTrustsAnUnlimitedRealityAsIs guards the
// other half of EffectiveIgnored's contract: when nothing was missing
// (Limited=false), the raw counter IS the believed one, so clamping must be a
// no-op rather than always substituting Floor.
func TestPreflightSchemaCursorsFromReportTrustsAnUnlimitedRealityAsIs(t *testing.T) {
	const rawIgnored = 42

	report := proxyendpoint.CursorReportForTest(
		proxyendpoint.Cursors{Main: 5, Ignored: rawIgnored},
		proxyendpoint.CursorReality{},
	)

	got := preflightSchemaCursorsFromReport(report)

	if got.Ignored != rawIgnored {
		t.Errorf("Ignored = %d, want the raw value %d unchanged: nothing was missing", got.Ignored, rawIgnored)
	}
}
