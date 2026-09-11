package runproj

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// detailGoldenRunID is the graph.v2 run captured as the FormulaRunDetail golden
// (rundetail_golden.json), the Go-owned reference fixture.
const detailGoldenRunID = "dt-adopt1"

// detailGoldenSnapshotVersion / detailGoldenSnapshotEventSeq are the
// snapshot_version=1 / snapshot_event_seq=100 constants that appear verbatim in
// the golden's snapshotVersion/snapshotEventSeq.
const (
	detailGoldenSnapshotVersion  = 1
	detailGoldenSnapshotEventSeq = 100
)

// TestBuildRunDetailGolden pins the Go port of the detail pipeline to the
// TypeScript oracle: it loads the shared bead fixture, builds the detail for the
// captured run, and asserts the canonical JSON matches rundetail_golden.json
// byte-for-byte (same JSON.stringify(value, null, 2)+newline canonicalization the
// generator used).
func TestBuildRunDetailGolden(t *testing.T) {
	fixturePath := filepath.Join("testdata", "beads_fixture.json")
	goldenPath := filepath.Join("testdata", "rundetail_golden.json")

	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var beadList []beads.Bead
	if err := json.Unmarshal(raw, &beadList); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}

	detail, err := BuildRunDetail(beadList, detailGoldenRunID, detailGoldenSnapshotVersion, detailGoldenSnapshotEventSeq)
	if err != nil {
		t.Fatalf("BuildRunDetail: %v", err)
	}
	got, err := canonicalJSON(detail)
	if err != nil {
		t.Fatalf("marshal detail: %v", err)
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}

	if !bytes.Equal(got, want) {
		t.Errorf("run detail does not match golden:\n%s", unifiedDiff(string(want), string(got)))
	}
}
