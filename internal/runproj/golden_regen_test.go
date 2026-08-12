package runproj

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRegenerateGoldens rewrites the golden fixtures from the current pipeline
// when RUNPROJ_UPDATE_GOLDENS=1 is set. It exists so an intentional semantic
// change regenerates all three goldens through the exact build calls their
// tests use; audit the resulting git diff before committing.
func TestRegenerateGoldens(t *testing.T) {
	if os.Getenv("RUNPROJ_UPDATE_GOLDENS") == "" {
		t.Skip("set RUNPROJ_UPDATE_GOLDENS=1 to rewrite testdata goldens")
	}

	beadList := loadFixtureBeads(t)

	detail, err := BuildRunDetail(beadList, detailGoldenRunID, detailGoldenSnapshotVersion, detailGoldenSnapshotEventSeq)
	if err != nil {
		t.Fatalf("BuildRunDetail: %v", err)
	}
	writeGolden(t, "rundetail_golden.json", detail)

	summary := BuildRunSummary(beadList)
	writeGolden(t, "runsummary_golden.json", summary)

	sessions := loadFixtureSessions(t)
	inFlight := make([]RunLane, 0, len(summary.Lanes)+len(summary.BlockedLanes))
	inFlight = append(inFlight, summary.Lanes...)
	inFlight = append(inFlight, summary.BlockedLanes...)
	marks := AdvanceProgressMarks(nil, inFlight)
	enriched := EnrichRunSummary(summary, sessions, true, mustMillis(t, "2026-06-09T00:00:00Z"), marks)
	writeGolden(t, "runsummary_enriched_golden.json", enriched)
}

func writeGolden(t *testing.T, name string, v any) {
	t.Helper()
	data, err := canonicalJSON(v)
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	if err := os.WriteFile(filepath.Join("testdata", name), data, 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
