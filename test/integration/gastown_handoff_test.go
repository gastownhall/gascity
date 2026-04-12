//go:build integration

package integration

import (
	"strings"
	"testing"
)

// TestGastown_HandoffRemote validates that gc handoff --target sends
// mail to the target agent.
func TestGastown_HandoffRemote(t *testing.T) {
	agents := []gasTownAgent{
		{Name: "mayor", StartCommand: "sleep 3600"},
		{Name: "deacon", StartCommand: "sleep 3600"},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	// Remote handoff from human to mayor.
	out, err := gc(cityDir, "handoff", "--target", "mayor", "Context refresh", "Check latest status")
	if err != nil {
		t.Fatalf("gc handoff failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "Handoff") {
		t.Errorf("expected 'Handoff' in output:\n%s", out)
	}

	// Verify mail was delivered.
	out, err = gc(cityDir, "mail", "inbox", "mayor")
	if err != nil {
		t.Fatalf("gc mail inbox failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "Context refresh") {
		t.Errorf("expected handoff mail in mayor inbox:\n%s", out)
	}
}

// TestGastown_HandoffSelfRequiresContext validates that self-handoff
// fails without agent context.
func TestGastown_HandoffSelfRequiresContext(t *testing.T) {
	agents := []gasTownAgent{
		{Name: "mayor", StartCommand: "sleep 3600"},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	out, err := gc(cityDir, "handoff", "Context refresh")
	if err == nil {
		t.Fatal("expected self-handoff to fail without agent context")
	}
	if !strings.Contains(out, "not in session context") {
		t.Errorf("expected 'not in session context' error:\n%s", out)
	}
}

// TestGastown_HandoffToNonexistent validates error on handoff to
// unknown agent.
func TestGastown_HandoffToNonexistent(t *testing.T) {
	agents := []gasTownAgent{
		{Name: "mayor", StartCommand: "sleep 3600"},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	out, err := gc(cityDir, "handoff", "--target", "nonexistent", "hello")
	if err == nil {
		t.Fatal("expected handoff to nonexistent to fail")
	}
	if !strings.Contains(out, "not found") {
		t.Errorf("expected 'not found' in error:\n%s", out)
	}
}
