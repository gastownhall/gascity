package runproj

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestSnapshotForRunMissingRootIsErrRunNotFound proves the missing-root failure
// carries the ErrRunNotFound sentinel, so the dashboard BFF can distinguish a
// truly-unknown run (eligible for its unknown-run warming grace) from every
// other projection failure.
func TestSnapshotForRunMissingRootIsErrRunNotFound(t *testing.T) {
	beadList := loadDetailFixture(t)
	_, err := SnapshotForRun(beadList, "no-such-run", detailGoldenSnapshotVersion, detailGoldenSnapshotEventSeq)
	if err == nil {
		t.Fatal("SnapshotForRun with an absent root returned nil error")
	}
	if !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("err = %v, want errors.Is(err, ErrRunNotFound)", err)
	}
}

// loadDetailFixture reads the shared bead fixture used by the golden tests.
func loadDetailFixture(t *testing.T) []beads.Bead {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "beads_fixture.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var beadList []beads.Bead
	if err := json.Unmarshal(raw, &beadList); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	return beadList
}

// TestFormulaTargetFromSnapshotMatchesRunFormulaTargetForRun proves the
// single-scan formula-target extraction (off a snapshot built with the REAL
// version/seq) resolves the exact same (name, target, scope) tuple the old
// zero-version RunFormulaTargetForRun path did. This is the byte-identity guard
// for Part A's key risk: the formula-target metadata read must not depend on the
// snapshot's version/eventSeq identity fields.
func TestFormulaTargetFromSnapshotMatchesRunFormulaTargetForRun(t *testing.T) {
	beadList := loadDetailFixture(t)

	// The old path builds its snapshot at version=0, eventSeq=0.
	wantName, wantTarget, wantKind, wantRef, wantOK := RunFormulaTargetForRun(beadList, detailGoldenRunID)
	if !wantOK {
		t.Fatalf("RunFormulaTargetForRun(%q) not ok; fixture must carry a formula target", detailGoldenRunID)
	}

	// The new path builds ONE snapshot at the real version/seq and extracts the
	// target from it.
	snap, err := SnapshotForRun(beadList, detailGoldenRunID, detailGoldenSnapshotVersion, detailGoldenSnapshotEventSeq)
	if err != nil {
		t.Fatalf("SnapshotForRun: %v", err)
	}
	gotName, gotTarget, gotKind, gotRef, gotOK := FormulaTargetFromSnapshot(snap)
	if gotOK != wantOK || gotName != wantName || gotTarget != wantTarget || gotKind != wantKind || gotRef != wantRef {
		t.Fatalf("FormulaTargetFromSnapshot = (%q,%q,%q,%q,%v), want (%q,%q,%q,%q,%v) — target must be version/seq-independent",
			gotName, gotTarget, gotKind, gotRef, gotOK, wantName, wantTarget, wantKind, wantRef, wantOK)
	}
}

// TestBuildRunDetailFromSnapshotMatchesGolden proves detail built off the shared
// single snapshot is byte-identical to the two-call golden path.
func TestBuildRunDetailFromSnapshotMatchesGolden(t *testing.T) {
	beadList := loadDetailFixture(t)

	snap, err := SnapshotForRun(beadList, detailGoldenRunID, detailGoldenSnapshotVersion, detailGoldenSnapshotEventSeq)
	if err != nil {
		t.Fatalf("SnapshotForRun: %v", err)
	}
	fromSnap, err := BuildRunDetailFromSnapshot(snap, nil, nil, FormulaDetailUpstreamError)
	if err != nil {
		t.Fatalf("BuildRunDetailFromSnapshot: %v", err)
	}
	gotSnap, err := canonicalJSON(fromSnap)
	if err != nil {
		t.Fatalf("marshal from-snapshot detail: %v", err)
	}

	want, err := os.ReadFile(filepath.Join("testdata", "rundetail_golden.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(gotSnap, want) {
		t.Errorf("BuildRunDetailFromSnapshot does not match golden:\n%s", unifiedDiff(string(want), string(gotSnap)))
	}

	// And it equals the two-call BuildRunDetail byte-for-byte.
	twoCall, err := BuildRunDetail(beadList, detailGoldenRunID, detailGoldenSnapshotVersion, detailGoldenSnapshotEventSeq)
	if err != nil {
		t.Fatalf("BuildRunDetail: %v", err)
	}
	gotTwo, err := canonicalJSON(twoCall)
	if err != nil {
		t.Fatalf("marshal two-call detail: %v", err)
	}
	if !bytes.Equal(gotSnap, gotTwo) {
		t.Errorf("from-snapshot detail differs from two-call detail:\n%s", unifiedDiff(string(gotTwo), string(gotSnap)))
	}
}

// TestBuildRunDetailForRunScansOnce proves the combined single-scan entry point
// runs snapshotForRun exactly once, unlike the old detail() path that scanned
// twice (once for the formula target, once for the build).
func TestBuildRunDetailForRunScansOnce(t *testing.T) {
	beadList := loadDetailFixture(t)

	before := snapshotScanCount.Load()
	detail, name, target, scopeKind, scopeRef, targetOK, err := BuildRunDetailForRun(
		beadList, detailGoldenRunID, detailGoldenSnapshotVersion, detailGoldenSnapshotEventSeq,
		nil, nil, FormulaDetailUpstreamError,
	)
	if err != nil {
		t.Fatalf("BuildRunDetailForRun: %v", err)
	}
	scans := snapshotScanCount.Load() - before
	if scans != 1 {
		t.Fatalf("snapshotForRun ran %d times per BuildRunDetailForRun, want exactly 1", scans)
	}

	// The combined entry returns the same target the standalone resolver does.
	wantName, wantTarget, wantKind, wantRef, wantOK := RunFormulaTargetForRun(beadList, detailGoldenRunID)
	if targetOK != wantOK || name != wantName || target != wantTarget || scopeKind != wantKind || scopeRef != wantRef {
		t.Fatalf("combined target = (%q,%q,%q,%q,%v), want (%q,%q,%q,%q,%v)",
			name, target, scopeKind, scopeRef, targetOK, wantName, wantTarget, wantKind, wantRef, wantOK)
	}

	// And its detail is byte-identical to the golden.
	got, err := canonicalJSON(detail)
	if err != nil {
		t.Fatalf("marshal combined detail: %v", err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "rundetail_golden.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("BuildRunDetailForRun detail does not match golden:\n%s", unifiedDiff(string(want), string(got)))
	}
}
