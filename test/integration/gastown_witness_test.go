//go:build integration && unix

package integration

import (
	"testing"
	"time"
)

// TestGastown_WitnessOrphanDetection validates that the witness agent
// detects orphaned beads and sends mail to the mayor about them.
func TestGastown_WitnessOrphanDetection(t *testing.T) {
	agents := []gasTownAgent{
		{Name: "mayor", StartCommand: "sleep 3600"},
		{Name: "witness", StartCommand: "bash " + agentScript("witness-patrol.sh")},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	// Create an orphaned bead (open, no assignee).
	createBead(t, cityDir, "Orphaned work item")

	// Wait for witness to detect and report via mail to mayor.
	waitForMail(t, cityDir, "mayor", "Orphaned bead", 10*time.Second)
}

// TestGastown_WitnessWithNoOrphans validates that the witness patrols
// without error when no orphaned beads exist.
func TestGastown_WitnessWithNoOrphans(t *testing.T) {
	agents := []gasTownAgent{
		{Name: "mayor", StartCommand: "sleep 3600"},
		{Name: "witness", StartCommand: "bash " + agentScript("witness-patrol.sh")},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	// No beads → witness should patrol without issues.
	// Just verify the city runs for a moment without errors.
	time.Sleep(1 * time.Second)

	// City should still be operational.
	_, err := gc(cityDir, "session", "list")
	if err != nil {
		t.Fatalf("gc session list failed after witness patrol: %v", err)
	}
}
