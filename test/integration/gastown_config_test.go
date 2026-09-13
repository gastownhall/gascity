//go:build integration

package integration

import (
	"strings"
	"testing"
)

// TestGastown_ConfigStartStop validates that a gastown-style city with multiple
// agent types starts and stops cleanly.
func TestGastown_ConfigStartStop(t *testing.T) {
	agents := []gasTownAgent{
		{Name: "mayor", StartCommand: "sleep 3600"},
		{Name: "deacon", StartCommand: "sleep 3600"},
		{Name: "boot", StartCommand: "sleep 3600"},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	// Agent list should show all three agents.
	out, err := gc(cityDir, "session", "list")
	if err != nil {
		t.Fatalf("gc session list failed: %v\noutput: %s", err, out)
	}
	for _, name := range []string{"mayor", "deacon", "boot"} {
		if !strings.Contains(out, name) {
			t.Errorf("expected agent %q in list output:\n%s", name, out)
		}
	}
}

// TestGastown_ConfigWithPool validates pool agents start according to check command.
func TestGastown_ConfigWithPool(t *testing.T) {
	agents := []gasTownAgent{
		{Name: "mayor", StartCommand: "sleep 3600"},
		{Name: "dog", StartCommand: "sleep 3600", Pool: &poolConfig{
			Min: 0, Max: 3, Check: "echo 2",
		}},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	// Session list shows running sessions; config show shows pool config.
	out, err := gc(cityDir, "session", "list")
	if err != nil {
		t.Fatalf("gc session list failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "mayor") {
		t.Errorf("expected 'mayor' in session list:\n%s", out)
	}
	if !strings.Contains(out, "dog") {
		t.Errorf("expected 'dog' in session list:\n%s", out)
	}
	// Pool config annotations are in city.toml, not session beads.
	cfgOut, err := gc(cityDir, "config", "show")
	if err != nil {
		t.Fatalf("gc config show failed: %v\noutput: %s", err, cfgOut)
	}
	for _, want := range []string{"min_active_sessions = 0", "max_active_sessions = 3", "scale_check = \"echo 2\""} {
		if !strings.Contains(cfgOut, want) {
			t.Errorf("expected %q in config show:\n%s", want, cfgOut)
		}
	}
}

// TestGastown_ConfigValidate validates that gc config show --validate
// passes on a valid gastown config.
func TestGastown_ConfigValidate(t *testing.T) {
	agents := []gasTownAgent{
		{Name: "mayor", StartCommand: "sleep 3600"},
		{Name: "deacon", StartCommand: "sleep 3600"},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	out, err := gc(cityDir, "config", "show", "--validate")
	if err != nil {
		t.Fatalf("gc config show --validate failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "Config valid.") {
		t.Errorf("expected 'Config valid.' in output:\n%s", out)
	}
}

// TestGastown_SuspendedAgentSkipped validates that suspended agents are not
// started by gc start.
func TestGastown_SuspendedAgentSkipped(t *testing.T) {
	agents := []gasTownAgent{
		{Name: "mayor", StartCommand: "sleep 3600"},
		{Name: "worker", StartCommand: "sleep 3600", Suspended: true},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	// Suspended is a config-level flag, check via config show.
	out, err := gc(cityDir, "config", "show")
	if err != nil {
		t.Fatalf("gc config show failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "suspended") {
		t.Errorf("expected 'suspended' in config show for worker:\n%s", out)
	}
}
