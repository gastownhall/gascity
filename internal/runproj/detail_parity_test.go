package runproj

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// The golden tests pin one graph.v2 adopt-pr run byte-for-byte. These parity
// tests broaden the oracle across the shapes the golden fixture does not cover —
// every supported formula's stage ladder, multi-iteration loop instancing, and
// the unsupported-run classification — so a porting regression in that
// most-bug-fixed logic fails CI instead of silently diverging from the deleted TS.

// TestStagesForFormulaCoversEverySupportedFormula pins each supported formula's
// stagesForFormula ladder (keys and order). A dropped, reordered, or renamed
// stage — the kind of error the narrow golden cannot see — fails here.
func TestStagesForFormulaCoversEverySupportedFormula(t *testing.T) {
	want := map[string][]string{
		"mol-adopt-pr-v2":                  {"preflight", "rebase", "pre-review-ci", "review", "ci", "approval", "finalize", "cleanup"},
		"mol-design-review-v2":             {"setup", "personas", "fanout", "synthesis", "apply", "finalize"},
		"mol-bug-report-flow-v2":           {"intake", "repro", "audit", "classify", "approval", "publish", "dispatch"},
		"mol-bug-report-implementation-v2": {"plan", "design", "implement", "review", "pr", "ci", "merge"},
	}
	for formula, keys := range want {
		stages := stagesForFormula(formula, true)
		if len(stages) != len(keys) {
			t.Errorf("%s: got %d stages, want %d", formula, len(stages), len(keys))
			continue
		}
		for i, stage := range stages {
			if stage.key != keys[i] {
				t.Errorf("%s: stage[%d].key = %q, want %q", formula, i, stage.key, keys[i])
			}
			if stage.label == "" {
				t.Errorf("%s: stage %q has an empty label", formula, stage.key)
			}
			if len(stage.steps) == 0 {
				t.Errorf("%s: stage %q has no steps", formula, stage.key)
			}
		}
	}
	if got := stagesForFormula("mol-adopt-pr-v2", false); got != nil {
		t.Errorf("hasFormula=false must yield nil stages, got %v", got)
	}
	if got := stagesForFormula("mol-not-a-real-formula", true); got != nil {
		t.Errorf("unknown formula must yield nil stages, got %v", got)
	}
}

// TestFormulaStageProgressMarksCompleteActivePending drives the stage-status
// classifier for a mid-flight adopt-pr run: two closed early steps, one
// in-progress step. Stages before the active one are complete, the owning stage
// active, and the rest pending.
func TestFormulaStageProgressMarksCompleteActivePending(t *testing.T) {
	stages := stagesForFormula("mol-adopt-pr-v2", true)
	issues := []runIssue{
		stepIssue("preflight", "closed"),
		stepIssue("rebase-check", "closed"),
		stepIssue("review-loop", "in_progress"),
	}

	got := formulaStageProgress(stages, issues)
	// pre-review-ci sits before the active review stage, so it reads complete
	// even with no issues of its own (stage status is positional).
	want := []string{"complete", "complete", "complete", "active", "pending", "pending", "pending", "pending"}
	if len(got) != len(want) {
		t.Fatalf("got %d stages, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Status != want[i] {
			t.Errorf("stage %q status = %q, want %q", got[i].Key, got[i].Status, want[i])
		}
	}
}

// TestReviewRoundDetectionAcrossIterations exercises loop instancing: the review
// round is read from the highest iteration/attempt marker across a run's beads,
// in each of the encodings reviewRoundForIssue accepts (digit-suffixed key,
// digit-suffixed value, and a bare iteration/attempt key whose value holds the
// number).
func TestReviewRoundDetectionAcrossIterations(t *testing.T) {
	cases := []struct {
		name  string
		issue runIssue
		want  int
	}{
		{"digit-suffixed key", metaIssue(map[string]string{"review.iteration.2": "in_progress"}), 2},
		{"digit-suffixed value", metaIssue(map[string]string{beadmeta.ScopeRefMetadataKey: "review-loop.iteration.3"}), 3},
		{"bare attempt key holds the number", metaIssue(map[string]string{"attempt": "4"}), 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := reviewRoundForIssue(tc.issue)
			if !ok || got != tc.want {
				t.Fatalf("reviewRoundForIssue = (%d,%v), want (%d,true)", got, ok, tc.want)
			}
		})
	}

	// Across a multi-iteration convoy the run resolves to the furthest round.
	issues := []runIssue{
		metaIssue(map[string]string{"review.iteration.1": "closed"}),
		metaIssue(map[string]string{"review.iteration.3": "in_progress"}),
		metaIssue(map[string]string{beadmeta.StepIDMetadataKey: "preflight"}),
	}
	round, ok := reviewRoundForIssues(issues)
	if !ok || round != 3 {
		t.Fatalf("reviewRoundForIssues = (%d,%v), want (3,true)", round, ok)
	}
}

// TestBuildRunDetailUnsupportedRunReasons pins the run-classification bug fix
// (gascity-dashboard-9w3k): a non-graph.v2 run reports not_run_view (an honest
// list-only run), while a graph.v2 run missing its snapshot identity reports
// invalid_snapshot (a genuine load failure) — the SPA renders these differently.
func TestBuildRunDetailUnsupportedRunReasons(t *testing.T) {
	notRunView := beads.Bead{
		ID: "run-legacy", Type: "molecule", Status: "open",
		Metadata: map[string]string{beadmeta.KindMetadataKey: "run"},
	}
	assertUnsupported(t, []beads.Bead{notRunView}, "run-legacy", ReasonNotRunView)

	invalidSnapshot := beads.Bead{
		ID: "run-x", Type: "molecule", Status: "open",
		Metadata: map[string]string{
			beadmeta.FormulaContractMetadataKey: "graph.v2",
			beadmeta.KindMetadataKey:            "run",
			beadmeta.ScopeKindMetadataKey:       "rig",
			beadmeta.ScopeRefMetadataKey:        "demo",
			// No gc.root_store_ref: the snapshot identity is incomplete.
		},
	}
	assertUnsupported(t, []beads.Bead{invalidSnapshot}, "run-x", ReasonInvalidSnapshot)
}

func stepIssue(stepID, status string) runIssue {
	return runIssue{
		status: status,
		metadata: map[string]string{
			beadmeta.StepIDMetadataKey: stepID,
			beadmeta.KindMetadataKey:   "step",
		},
	}
}

func metaIssue(metadata map[string]string) runIssue {
	return runIssue{metadata: metadata}
}

func assertUnsupported(t *testing.T, beadList []beads.Bead, runID string, wantReason UnsupportedRunReason) {
	t.Helper()
	_, err := BuildRunDetail(beadList, runID, 1, 1)
	if err == nil {
		t.Fatalf("run %q: expected UnsupportedRunError %q, got nil", runID, wantReason)
	}
	var unsupported *UnsupportedRunError
	if !errors.As(err, &unsupported) {
		t.Fatalf("run %q: error %v is not an UnsupportedRunError", runID, err)
	}
	if unsupported.Reason != wantReason {
		t.Errorf("run %q: reason = %q, want %q", runID, unsupported.Reason, wantReason)
	}
}
