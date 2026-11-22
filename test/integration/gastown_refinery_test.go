//go:build integration && unix

package integration

import (
	"testing"
	"time"
)

// TestGastown_RefineryProcessing validates the refinery merge flow:
// claim merge request → process → close bead.
func TestGastown_RefineryProcessing(t *testing.T) {
	// Use a simple one-shot agent as refinery stand-in.
	agents := []gasTownAgent{
		{Name: "refinery", StartCommand: "bash " + agentScript("one-shot.sh")},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	beadID := createBead(t, cityDir, "Merge PR #42")
	claimBead(t, cityDir, "refinery", beadID)

	waitForBeadStatus(t, cityDir, beadID, "closed", 10*time.Second)
}

// TestGastown_RefinerySequentialQueue validates that the refinery processes
// multiple merge requests sequentially.
func TestGastown_RefinerySequentialQueue(t *testing.T) {
	agents := []gasTownAgent{
		{Name: "refinery", StartCommand: "bash " + agentScript("loop.sh")},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	// Create 3 merge requests.
	var beadIDs []string
	for i := 0; i < 3; i++ {
		id := createBead(t, cityDir, "Merge request")
		beadIDs = append(beadIDs, id)
	}

	// Wait for all to be processed.
	for _, id := range beadIDs {
		waitForBeadStatus(t, cityDir, id, "closed", 15*time.Second)
	}
}
