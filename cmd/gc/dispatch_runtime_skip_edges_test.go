package main

import (
	"fmt"
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/dispatch"
)

// Edge-case pins for the bead_not_open skip classification (issue #6024).

// TestDrainWorkflowServeWorkSkipBesidePendingReturnsThroughPendingQueue pins
// that pending wins over skips: a pass holding one skipped row and one
// pending row returns through serve pending-queue with pendingAny set, so
// the --follow loop still wakes for the blocked work rather than treating the
// whole pass as idle.
func TestDrainWorkflowServeWorkSkipBesidePendingReturnsThroughPendingQueue(t *testing.T) {
	clearGCEnv(t)

	prevList := workflowServeList
	prevControl := controlDispatcherServe
	t.Cleanup(func() {
		workflowServeList = prevList
		controlDispatcherServe = prevControl
	})

	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		return []hookBead{
			{ID: "gc-skip", Metadata: hookBeadMetadata{beadmeta.KindMetadataKey: beadmeta.KindWorkflowFinalize}},
			{ID: "gc-pending", Metadata: hookBeadMetadata{beadmeta.KindMetadataKey: beadmeta.KindRetryEval}},
		}, nil
	}
	// A stubbed seam stands in for any no-op shape ProcessControl reports
	// with a nil error and Processed=false — the non-open skip, or a spawned
	// fanout whose finalize is still pending.
	controlDispatcherServe = func(_, _, beadID string, _, _ io.Writer, _ *executionEmitDeferral) error {
		if beadID == "gc-skip" {
			return errControlNotProcessed
		}
		return fmt.Errorf("%w: subject still open", dispatch.ErrControlPending)
	}

	result, err := drainWorkflowServeWork(config.Agent{Name: config.ControlDispatcherAgentName}, t.TempDir(), t.TempDir(), "bd ready", nil, io.Discard)
	if err != nil {
		t.Fatalf("drainWorkflowServeWork: %v", err)
	}
	if result.processedAny {
		t.Error("processedAny = true for a skip-plus-pending pass, want false")
	}
	if !result.pendingAny {
		t.Error("pendingAny = false, want true: pending wins over skips")
	}
}
