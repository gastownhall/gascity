//go:build integration

package integration

import (
	"testing"
	"time"
)

// TestGastown_PolecatHappyPath validates the polecat work lifecycle:
// claim work → process → close bead → exit.
func TestGastown_PolecatHappyPath(t *testing.T) {
	agents := []gasTownAgent{
		{Name: "polecat", StartCommand: "bash " + agentScript("one-shot.sh"), Pool: &poolConfig{
			Min: 1, Max: 3, Check: "echo 1",
		}},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	// Create work and claim it for the polecat.
	beadID := createBead(t, cityDir, "Implement feature X")
	claimBead(t, cityDir, sessionAssigneeForTemplate(t, cityDir, "polecat"), beadID)

	// Wait for the polecat to process and close the bead.
	waitForBeadStatus(t, cityDir, beadID, "closed", 10*time.Second)
}

// TestGastown_PolecatPoolProcessing validates multiple polecats processing
// multiple beads from the ready queue.
func TestGastown_PolecatPoolProcessing(t *testing.T) {
	agents := []gasTownAgent{
		{Name: "polecat", StartCommand: "bash " + agentScript("loop.sh"), Pool: &poolConfig{
			Min: 1, Max: 3, Check: "echo 1",
		}},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	// Create 3 beads — the loop agent will drain them.
	var beadIDs []string
	for i := 0; i < 3; i++ {
		id := createBead(t, cityDir, "Batch work item")
		beadIDs = append(beadIDs, id)
	}

	// Wait for all beads to close.
	for _, id := range beadIDs {
		waitForBeadStatus(t, cityDir, id, "closed", 15*time.Second)
	}
}
