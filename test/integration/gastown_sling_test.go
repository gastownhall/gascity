//go:build integration && unix

package integration

import (
	"strings"
	"testing"
)

// TestGastown_SlingToNonexistent validates that sling to a nonexistent agent
// produces a clear error.
func TestGastown_SlingToNonexistent(t *testing.T) {
	agents := []gasTownAgent{
		{Name: "mayor", StartCommand: "sleep 3600"},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	beadID := createBead(t, cityDir, "test work")
	out, err := gc(cityDir, "sling", "nonexistent", beadID)
	if err == nil {
		t.Fatal("expected sling to nonexistent to fail")
	}
	if !strings.Contains(out, "not found") {
		t.Errorf("expected 'not found' in error output:\n%s", out)
	}
}

// TestGastown_SlingToSuspended validates the warning when slinging to a
// suspended agent.
func TestGastown_SlingToSuspended(t *testing.T) {
	agents := []gasTownAgent{
		{Name: "mayor", StartCommand: "sleep 3600"},
		{Name: "worker", StartCommand: "sleep 3600", Suspended: true},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	beadID := createBead(t, cityDir, "suspended work")
	out, err := gc(cityDir, "sling", "worker", beadID)
	// Sling to suspended should warn but still route (or fail on bd update).
	// We just verify the warning appears.
	_ = err
	if !strings.Contains(out, "suspended") {
		t.Errorf("expected 'suspended' warning in output:\n%s", out)
	}
}
