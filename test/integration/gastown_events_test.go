//go:build integration

package integration

import (
	"strings"
	"testing"
)

// TestGastown_EventsBeadLifecycle validates that bead create/close
// operations emit events visible via gc events.
func TestGastown_EventsBeadLifecycle(t *testing.T) {
	agents := []gasTownAgent{
		{Name: "mayor", StartCommand: "sleep 3600"},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	beadID := createBead(t, cityDir, "event test bead")

	// Close the bead.
	out, err := bd(cityDir, "close", beadID)
	if err != nil {
		t.Fatalf("bd close failed: %v\noutput: %s", err, out)
	}

	// Verify events were recorded.
	verifyEvents(t, cityDir, "bead.created")
	verifyEvents(t, cityDir, "bead.closed")
}

// TestGastown_EventsMailLifecycle validates that mail operations emit events.
func TestGastown_EventsMailLifecycle(t *testing.T) {
	agents := []gasTownAgent{
		{Name: "mayor", StartCommand: "sleep 3600"},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	sendMail(t, cityDir, "mayor", "test message")
	verifyEvents(t, cityDir, "mail.sent")
}

// TestGastown_EventsCustom validates gc event emit for custom events.
func TestGastown_EventsCustom(t *testing.T) {
	agents := []gasTownAgent{
		{Name: "mayor", StartCommand: "sleep 3600"},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	out, err := gc(cityDir, "event", "emit", "test.custom", "--message", "hello from test")
	if err != nil {
		t.Fatalf("gc event emit failed: %v\noutput: %s", err, out)
	}

	verifyEvents(t, cityDir, "test.custom")
}

// TestGastown_EventsFiltering validates event type filtering.
func TestGastown_EventsFiltering(t *testing.T) {
	agents := []gasTownAgent{
		{Name: "mayor", StartCommand: "sleep 3600"},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	// Create some events.
	createBead(t, cityDir, "bead for events")
	sendMail(t, cityDir, "mayor", "mail for events")

	// Filter by type — should only show matching events.
	out, err := gc(cityDir, "events", "--type", "bead.created")
	if err != nil {
		t.Fatalf("gc events --type failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "bead.created") {
		t.Errorf("expected bead.created events:\n%s", out)
	}
	if strings.Contains(out, "mail.sent") {
		t.Errorf("filtered output should not contain mail.sent:\n%s", out)
	}

	// Unknown type should produce empty JSONL output.
	out, err = gc(cityDir, "events", "--type", "does.not.exist")
	if err != nil {
		t.Fatalf("gc events for unknown type failed: %v\noutput: %s", err, out)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("expected empty output for unknown type:\n%s", out)
	}
}
