package main

import (
	"io"
	"os"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestDrainWorkflowServeWorkCountsQuarantineAsProgress is the AC-5 guard for
// issue #6024's fix: a control dispatch that ends in a quarantine CLOSE is a
// write, so the drain must keep counting it as progress — processedAny set and
// the dispatch-wake file written — even though handleControlDispatchError
// returns nil without a Processed result.
func TestDrainWorkflowServeWorkCountsQuarantineAsProgress(t *testing.T) {
	cityDir, store := setUpControlReadyFileStoreCity(t)
	noBDOnPathForTest(t)

	// An unsupported control kind fails inside ProcessControl and classifies
	// TierNone, so handleControlDispatchError quarantines: it closes the bead
	// and returns nil.
	quarantined, err := store.Create(beads.Bead{
		Type: "task",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey: freshnessControlTarget,
			beadmeta.KindMetadataKey:     "bogus-kind",
		},
	})
	if err != nil {
		t.Fatalf("create control bead: %v", err)
	}

	prevInterval := workflowServeIdlePollInterval
	prevAttempts := workflowServeIdlePollAttempts
	workflowServeIdlePollInterval = 0
	workflowServeIdlePollAttempts = 0
	t.Cleanup(func() {
		workflowServeIdlePollInterval = prevInterval
		workflowServeIdlePollAttempts = prevAttempts
	})

	// Production ready scan and production serve seam throughout: the
	// quarantined control must reach the drain as a processed dispatch.
	result, drainErr := drainWorkflowServeWork(freshnessControlAgent(), cityDir, cityDir, workflowServeControlReadyQuery(freshnessControlAgent()), nil, io.Discard)
	if drainErr != nil {
		t.Fatalf("drainWorkflowServeWork: %v", drainErr)
	}
	if !result.processedAny {
		t.Error("processedAny = false, want the quarantined control counted as processed: a quarantine close is a write")
	}
	if _, statErr := os.Stat(dispatchWakeFile(cityDir)); statErr != nil {
		t.Errorf("dispatch wake file not written for a quarantined control: %v", statErr)
	}
	b, getErr := store.Get(quarantined.ID)
	if getErr != nil || b.Status != "closed" {
		t.Fatalf("quarantined control status = %q (err %v), want closed", b.Status, getErr)
	}
}
