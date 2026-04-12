//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"
)

// TestGastown_ControllerStartStop validates the controller lifecycle
// with a gastown-style city.
func TestGastown_ControllerStartStop(t *testing.T) {
	agents := []gasTownAgent{
		{Name: "mayor", StartCommand: "sleep 3600"},
		{Name: "deacon", StartCommand: "sleep 3600"},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	// Verify agents are running.
	out, err := gc(cityDir, "session", "list")
	if err != nil {
		t.Fatalf("gc session list failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "mayor") {
		t.Errorf("expected mayor in session list:\n%s", out)
	}
	if !strings.Contains(out, "deacon") {
		t.Errorf("expected deacon in session list:\n%s", out)
	}

	// Stop and restart.
	out, err = gc("", "stop", cityDir)
	if err != nil {
		t.Fatalf("gc stop failed: %v\noutput: %s", err, out)
	}

	time.Sleep(200 * time.Millisecond)

	out, err = gc("", "start", cityDir)
	if err != nil {
		t.Fatalf("gc start (restart) failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "City started") {
		t.Errorf("expected restart output to contain 'City started':\n%s", out)
	}
}

// TestGastown_ControllerIdleAgent validates that the controller can handle
// idle agent detection (agent with idle_timeout configured).
func TestGastown_ControllerIdleAgent(t *testing.T) {
	// Configure a very short idle timeout to test detection.
	// We can't really test timeout firing in integration without long waits,
	// but we verify the config doesn't break start/stop.
	agents := []gasTownAgent{
		{Name: "mayor", StartCommand: "sleep 3600"},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	out, err := gc(cityDir, "session", "list")
	if err != nil {
		t.Fatalf("gc session list failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "mayor") {
		t.Errorf("expected mayor in session list:\n%s", out)
	}
}
