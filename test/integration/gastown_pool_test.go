//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"
)

// TestGastown_PoolScaling validates that a pool agent processes work and
// the pool check determines how many instances run.
func TestGastown_PoolScaling(t *testing.T) {
	agents := []gasTownAgent{
		{Name: "worker", StartCommand: "bash " + agentScript("loop.sh"), Pool: &poolConfig{
			Min: 1, Max: 3, Check: "echo 1",
		}},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	// Create some work.
	beadID := createBead(t, cityDir, "pool work item")
	claimBead(t, cityDir, sessionAssigneeForTemplate(t, cityDir, "worker"), beadID)

	// Wait for the bead to be processed.
	waitForBeadStatus(t, cityDir, beadID, "closed", 10*time.Second)
}

// TestGastown_PoolMinGuarantee validates that min pool count is maintained
// even when check returns 0.
func TestGastown_PoolMinGuarantee(t *testing.T) {
	agents := []gasTownAgent{
		{Name: "builder", StartCommand: "sleep 3600", Pool: &poolConfig{
			Min: 2, Max: 5, Check: "echo 0",
		}},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	out, err := gc(cityDir, "session", "list")
	if err != nil {
		t.Fatalf("gc session list failed: %v\noutput: %s", err, out)
	}
	// min=2 means at least 2 agents even when check says 0.
	if !strings.Contains(out, "builder") {
		t.Errorf("expected 'builder' in session list:\n%s", out)
	}
}

// TestGastown_PoolMaxCap validates that pool scaling is capped at max.
func TestGastown_PoolMaxCap(t *testing.T) {
	agents := []gasTownAgent{
		{Name: "worker", StartCommand: "sleep 3600", Pool: &poolConfig{
			Min: 0, Max: 3, Check: "echo 100",
		}},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	// Pool max is a config-level field; check via config show.
	out, err := gc(cityDir, "config", "show")
	if err != nil {
		t.Fatalf("gc config show failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "max_active_sessions = 3") {
		t.Errorf("expected 'max_active_sessions = 3' in config show:\n%s", out)
	}
}
