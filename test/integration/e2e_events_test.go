//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"
)

// TestE2E_EventEmit verifies that gc event emit records a custom event.
func TestE2E_EventEmit(t *testing.T) {
	city := e2eCity{
		Agents: []e2eAgent{
			{Name: "eventer", StartCommand: e2eSleepScript()},
		},
	}
	cityDir := setupE2ECity(t, nil, city)

	out, err := gc(cityDir, "event", "emit", "e2e.test", "--subject", "foo", "--message", "hello from e2e")
	if err != nil {
		t.Fatalf("gc event emit failed: %v\noutput: %s", err, out)
	}

	verifyEvents(t, cityDir, "e2e.test")
}

// TestE2E_EventsQuery verifies that gc events --type filters events correctly.
func TestE2E_EventsQuery(t *testing.T) {
	city := e2eCity{
		Agents: []e2eAgent{
			{Name: "querier", StartCommand: e2eSleepScript()},
		},
	}
	cityDir := setupE2ECity(t, nil, city)

	// Emit two different event types.
	gc(cityDir, "event", "emit", "e2e.alpha", "--message", "alpha event") //nolint:errcheck
	gc(cityDir, "event", "emit", "e2e.beta", "--message", "beta event")   //nolint:errcheck

	// Filter by alpha — should not show beta.
	out, err := gc(cityDir, "events", "--type", "e2e.alpha")
	if err != nil {
		t.Fatalf("gc events --type failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "e2e.alpha") {
		t.Errorf("expected e2e.alpha events:\n%s", out)
	}
	if strings.Contains(out, "e2e.beta") {
		t.Errorf("filtered output should not contain e2e.beta:\n%s", out)
	}

	// Unknown type should produce empty JSONL output.
	out, err = gc(cityDir, "events", "--type", "e2e.nonexistent")
	if err != nil {
		t.Fatalf("gc events for unknown type failed: %v\noutput: %s", err, out)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("expected empty output for unknown type:\n%s", out)
	}
}

// TestE2E_EventsSince verifies that gc events --since filters by time window.
func TestE2E_EventsSince(t *testing.T) {
	city := e2eCity{
		Agents: []e2eAgent{
			{Name: "sincer", StartCommand: e2eSleepScript()},
		},
	}
	cityDir := setupE2ECity(t, nil, city)

	// Emit an event now.
	gc(cityDir, "event", "emit", "e2e.recent", "--message", "just happened") //nolint:errcheck

	// Query with --since=1m should include it.
	out, err := gc(cityDir, "events", "--type", "e2e.recent", "--since", "1m")
	if err != nil {
		t.Fatalf("gc events --since failed: %v\noutput: %s", err, out)
	}
	if strings.TrimSpace(out) == "" {
		t.Errorf("expected recent event to appear with --since=1m:\n%s", out)
	}

	// Query with --since=0s should technically also include it (window includes now).
	// But --since=0s may mean "from now" and return nothing. Use a safe value.
	out, err = gc(cityDir, "events", "--type", "e2e.recent", "--since", "30s")
	if err != nil {
		t.Fatalf("gc events --since=30s failed: %v\noutput: %s", err, out)
	}
	if strings.TrimSpace(out) == "" {
		t.Errorf("expected recent event to appear with --since=30s:\n%s", out)
	}
}

// TestE2E_AgentLifecycleEvents verifies that starting and stopping agents
// produces session.woke and session.stopped events.
func TestE2E_AgentLifecycleEvents(t *testing.T) {
	city := e2eCity{
		Agents: []e2eAgent{
			{Name: "lifecycle", StartCommand: e2eSleepScript()},
		},
	}
	cityDir := setupE2ECity(t, nil, city)

	// Verify session.woke event exists.
	verifyEvents(t, cityDir, "session.woke")

	// Stop the agent.
	out, err := gc("", "stop", cityDir)
	if err != nil {
		t.Fatalf("gc stop failed: %v\noutput: %s", err, out)
	}

	// Give the event log a moment.
	time.Sleep(500 * time.Millisecond)

	// Verify session.stopped event exists.
	verifyEvents(t, cityDir, "session.stopped")
}
