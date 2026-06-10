package beadmeta

import "testing"

// TestPinnedKindValues pins the gc.kind value vocabulary. These exact strings
// are written into persistent bead metadata and branched on across dispatch,
// formula compilation, and the API projection — renaming a constant's value
// silently orphans every persisted bead carrying the old string, so any change
// here must come with a data-migration story.
func TestPinnedKindValues(t *testing.T) {
	pinned := map[string]string{
		KindRetry:            "retry",
		KindRalph:            "ralph",
		KindCheck:            "check",
		KindRetryEval:        "retry-eval",
		KindFanout:           "fanout",
		KindTally:            "tally",
		KindDrain:            "drain",
		KindScopeCheck:       "scope-check",
		KindWorkflowFinalize: "workflow-finalize",
		KindScope:            "scope",
		KindCleanup:          "cleanup",
		KindRun:              "run",
		KindRetryRun:         "retry-run",
		KindWorkflow:         "workflow",
	}
	for got, want := range pinned {
		if got != want {
			t.Errorf("pinned kind value drift: got %q, want %q", got, want)
		}
	}
}

// TestPinnedOutcomeAndFailureClassValues pins the outcome and failure-class
// value vocabularies for the same persisted-data reason as the kind values.
func TestPinnedOutcomeAndFailureClassValues(t *testing.T) {
	pinned := map[string]string{
		OutcomePass:           "pass",
		OutcomeFail:           "fail",
		OutcomeSkipped:        "skipped",
		FailureClassTransient: "transient",
		FailureClassHard:      "hard",
	}
	for got, want := range pinned {
		if got != want {
			t.Errorf("pinned value drift: got %q, want %q", got, want)
		}
	}
}
