//go:build integration

package integration

import (
	"testing"
	"time"
)

// TestGastown_ShutdownDogProcessesWarrant validates that a dog agent
// processes a warrant (work bead) and closes it.
func TestGastown_ShutdownDogProcessesWarrant(t *testing.T) {
	agents := []gasTownAgent{
		{Name: "dog", StartCommand: "bash " + agentScript("dog-warrant.sh"), Pool: &poolConfig{
			Min: 1, Max: 3, Check: "echo 1",
		}},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	// Create a warrant-like bead and assign to dog.
	beadID := createBead(t, cityDir, "Shutdown warrant for stuck-agent")
	claimBead(t, cityDir, sessionAssigneeForTemplate(t, cityDir, "dog"), beadID)

	// Wait for dog to process the warrant.
	waitForBeadStatus(t, cityDir, beadID, "closed", 10*time.Second)
}

// TestGastown_ShutdownGraceful validates that gc stop with multiple
// agents shuts down cleanly.
func TestGastown_ShutdownGraceful(t *testing.T) {
	agents := []gasTownAgent{
		{Name: "mayor", StartCommand: "sleep 3600"},
		{Name: "deacon", StartCommand: "sleep 3600"},
		{Name: "boot", StartCommand: "sleep 3600"},
		{Name: "dog", StartCommand: "sleep 3600", Pool: &poolConfig{
			Min: 0, Max: 3, Check: "echo 1",
		}},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	// Let the city run for a moment.
	time.Sleep(500 * time.Millisecond)

	// Stop should succeed cleanly.
	out, err := gc("", "stop", cityDir)
	if err != nil {
		t.Fatalf("gc stop failed: %v\noutput: %s", err, out)
	}
}
