package dispatch

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// TestProcessWorkflowFinalizeClosesSourceChainPastUnblockedWorkOutcomes pins
// the complement of the blocked-source stop: a closed source whose work
// outcome does not withhold dependents — shipped, no-op, abandoned, or no
// gc.work_outcome at all — still closes its parents as passed, exactly as
// before the stop existed.
func TestProcessWorkflowFinalizeClosesSourceChainPastUnblockedWorkOutcomes(t *testing.T) {
	t.Parallel()

	rows := []struct {
		name        string
		workOutcome string
	}{
		{"no_work_outcome", ""},
		{"shipped", beadmeta.WorkOutcomeShipped},
		{"no-op", beadmeta.WorkOutcomeNoOp},
		{"abandoned", beadmeta.WorkOutcomeAbandoned},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			f := newSourceChainFinalizeFixture(t)
			if err := f.rigStore.Close(f.rigLaunch.ID); err != nil {
				t.Fatalf("closing source: %v", err)
			}
			stamp := map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass}
			if row.workOutcome != "" {
				stamp[beadmeta.WorkOutcomeMetadataKey] = row.workOutcome
			}
			mustStampSourceChainMetadata(t, f.rigStore, f.rigLaunch.ID, stamp)
			mustStampSourceChainMetadata(t, f.rigStore, f.workflow.ID, map[string]string{"pr_review.verdict": "approve"})

			opts, traceBuf := sourceChainTraceOptions(f)
			result, err := ProcessControl(f.rigStore, f.finalizer, opts)
			if err != nil {
				t.Fatalf("ProcessControl(workflow-finalize): %v", err)
			}
			if !result.Processed || result.Action != "workflow-pass" {
				t.Fatalf("result = processed %t action %q, want processed workflow-pass", result.Processed, result.Action)
			}
			parentAfter := mustGetBead(t, f.cityStore, f.citySource.ID)
			if parentAfter.Status != "closed" {
				t.Fatalf("parent status = %q, want closed past an unblocked source", parentAfter.Status)
			}
			if got := parentAfter.Metadata[beadmeta.OutcomeMetadataKey]; got != beadmeta.OutcomePass {
				t.Fatalf("parent gc.outcome = %q, want pass", got)
			}
			if got := parentAfter.Metadata["pr_review.verdict"]; got != "approve" {
				t.Fatalf("parent pr_review.verdict = %q, want the root's propagated approve", got)
			}
			if traced := traceBuf.String(); strings.Contains(traced, "blocked_source") {
				t.Fatalf("trace shows a blocked_source stop for an unblocked source; got:\n%s", traced)
			}
		})
	}
}
