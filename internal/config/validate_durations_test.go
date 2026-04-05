package config

import (
	"strings"
	"testing"
)

func TestValidateDurationsAllValid(t *testing.T) {
	cfg := &City{
		Session: SessionConfig{
			SetupTimeout:       "10s",
			NudgeReadyTimeout:  "10s",
			NudgeRetryInterval: "500ms",
			NudgeLockTimeout:   "30s",
			StartupTimeout:     "60s",
		},
		Daemon: DaemonConfig{
			PatrolInterval:    "30s",
			RestartWindow:     "1h",
			ShutdownTimeout:   "5s",
			DriftDrainTimeout: "2m",
		},
		Agents: []Agent{
			{Name: "mayor", IdleTimeout: "15m"},
			{Name: "worker", DrainTimeout: "5m"},
		},
	}
	warnings := ValidateDurations(cfg, "city.toml")
	if len(warnings) != 0 {
		t.Errorf("expected no warnings for valid config, got: %v", warnings)
	}
}

func TestValidateDurationsEmptyFieldsOK(t *testing.T) {
	cfg := &City{}
	warnings := ValidateDurations(cfg, "city.toml")
	if len(warnings) != 0 {
		t.Errorf("expected no warnings for empty config, got: %v", warnings)
	}
}

func TestValidateDurationsBadAgentIdleTimeout(t *testing.T) {
	cfg := &City{
		Agents: []Agent{
			{Name: "mayor", IdleTimeout: "5mins"},
		},
	}
	warnings := ValidateDurations(cfg, "city.toml")
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "mayor") {
		t.Errorf("warning should mention agent name: %s", warnings[0])
	}
	if !strings.Contains(warnings[0], "idle_timeout") {
		t.Errorf("warning should mention field name: %s", warnings[0])
	}
	if !strings.Contains(warnings[0], "5mins") {
		t.Errorf("warning should mention bad value: %s", warnings[0])
	}
}

func TestValidateDurationsBadSessionTimeout(t *testing.T) {
	cfg := &City{
		Session: SessionConfig{SetupTimeout: "ten seconds"},
	}
	warnings := ValidateDurations(cfg, "city.toml")
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "[session]") {
		t.Errorf("warning should mention section: %s", warnings[0])
	}
	if !strings.Contains(warnings[0], "setup_timeout") {
		t.Errorf("warning should mention field: %s", warnings[0])
	}
}

func TestValidateDurationsBadDaemonFields(t *testing.T) {
	cfg := &City{
		Daemon: DaemonConfig{
			PatrolInterval:  "30sec",
			RestartWindow:   "one hour",
			ShutdownTimeout: "5 seconds",
		},
	}
	warnings := ValidateDurations(cfg, "city.toml")
	if len(warnings) != 3 {
		t.Fatalf("expected 3 warnings, got %d: %v", len(warnings), warnings)
	}
}

func TestValidateDurationsBadPoolDrainTimeout(t *testing.T) {
	cfg := &City{
		Agents: []Agent{
			{Name: "worker", Dir: "hw", DrainTimeout: "5min"},
		},
	}
	warnings := ValidateDurations(cfg, "city.toml")
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "hw/worker") {
		t.Errorf("warning should mention qualified name: %s", warnings[0])
	}
	if !strings.Contains(warnings[0], "drain_timeout") {
		t.Errorf("warning should mention field: %s", warnings[0])
	}
}

func TestValidateDurationsMultipleIssues(t *testing.T) {
	cfg := &City{
		Session: SessionConfig{NudgeReadyTimeout: "bad1"},
		Daemon:  DaemonConfig{WispGCInterval: "bad2", WispTTL: "bad3"},
		Orders:  OrdersConfig{MaxTimeout: "bad4"},
		Agents: []Agent{
			{Name: "a1", IdleTimeout: "bad5"},
		},
	}
	warnings := ValidateDurations(cfg, "test.toml")
	if len(warnings) != 5 {
		t.Fatalf("expected 5 warnings, got %d: %v", len(warnings), warnings)
	}
}

func TestValidateDurationsIncludesSource(t *testing.T) {
	cfg := &City{
		Session: SessionConfig{SetupTimeout: "invalid"},
	}
	warnings := ValidateDurations(cfg, "/path/to/city.toml")
	if len(warnings) == 0 {
		t.Fatal("expected warning")
	}
	if !strings.Contains(warnings[0], "/path/to/city.toml") {
		t.Errorf("warning should include source path: %s", warnings[0])
	}
}
