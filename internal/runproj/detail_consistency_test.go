package runproj

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestSummaryDetailPhaseStageConsistency proves the ADR invariant structurally:
// the same run resolves to the same phase and the same stage ladder through both
// BuildRunSummary and BuildRunDetail, because both call the shared
// mapRunPhase/stageProgress classifier (detail stages == summary stages by
// construction, not by two call sites that happen to agree).
func TestSummaryDetailPhaseStageConsistency(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "beads_fixture.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var beadList []beads.Bead
	if err := json.Unmarshal(raw, &beadList); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}

	const runID = "dt-adopt1"

	summary := BuildRunSummary(beadList)
	lane, ok := findLane(summary, runID)
	if !ok {
		t.Fatalf("run %q not found in summary lanes", runID)
	}

	detail, err := BuildRunDetail(beadList, runID, detailGoldenSnapshotVersion, detailGoldenSnapshotEventSeq)
	if err != nil {
		t.Fatalf("BuildRunDetail: %v", err)
	}

	if lane.Phase != detail.Phase {
		t.Errorf("phase mismatch: summary=%q detail=%q", lane.Phase, detail.Phase)
	}
	if !reflect.DeepEqual(lane.Stages, detail.Stages) {
		t.Errorf("stage ladder mismatch:\nsummary=%+v\ndetail =%+v", lane.Stages, detail.Stages)
	}
}

// findLane locates a lane by id across the summary's active/blocked/historical
// partitions.
func findLane(summary RunSummary, id string) (RunLane, bool) {
	for _, group := range [][]RunLane{summary.Lanes, summary.BlockedLanes, summary.HistoricalLanes} {
		for _, lane := range group {
			if lane.ID == id {
				return lane, true
			}
		}
	}
	return RunLane{}, false
}
