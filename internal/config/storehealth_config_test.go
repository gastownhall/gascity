package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseStoreHealthSection(t *testing.T) {
	data := []byte(`
[workspace]
name = "test-city"

[storehealth]
enabled = false
interval = "45s"
consecutive_fails = 5
reap_cooldown = "20m"
write_probe_interval = "1h"

[[agent]]
name = "mayor"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.StoreHealth.Enabled == nil || *cfg.StoreHealth.Enabled {
		t.Errorf("Enabled = %v, want explicit false", cfg.StoreHealth.Enabled)
	}
	if cfg.StoreHealth.Interval != "45s" {
		t.Errorf("Interval = %q, want %q", cfg.StoreHealth.Interval, "45s")
	}
	if cfg.StoreHealth.ConsecutiveFails != 5 {
		t.Errorf("ConsecutiveFails = %d, want 5", cfg.StoreHealth.ConsecutiveFails)
	}
	if cfg.StoreHealth.ReapCooldown != "20m" {
		t.Errorf("ReapCooldown = %q, want %q", cfg.StoreHealth.ReapCooldown, "20m")
	}
	if cfg.StoreHealth.WriteProbeInterval != "1h" {
		t.Errorf("WriteProbeInterval = %q, want %q", cfg.StoreHealth.WriteProbeInterval, "1h")
	}

	// Configured values flow through the accessors unchanged.
	if got := cfg.StoreHealth.IntervalOrDefault(); got != 45*time.Second {
		t.Errorf("IntervalOrDefault() = %v, want 45s", got)
	}
	if got := cfg.StoreHealth.ConsecutiveFailsOrDefault(); got != 5 {
		t.Errorf("ConsecutiveFailsOrDefault() = %d, want 5", got)
	}
	if got := cfg.StoreHealth.ReapCooldownOrDefault(); got != 20*time.Minute {
		t.Errorf("ReapCooldownOrDefault() = %v, want 20m", got)
	}
	if got := cfg.StoreHealth.WriteProbeIntervalOrDefault(); got != time.Hour {
		t.Errorf("WriteProbeIntervalOrDefault() = %v, want 1h", got)
	}
	if cfg.StoreHealth.StoreHealthEnabledOrDefault() {
		t.Error("StoreHealthEnabledOrDefault() = true, want false when explicitly disabled")
	}
}

func TestParseNoStoreHealthSection(t *testing.T) {
	data := []byte(`
[workspace]
name = "test-city"

[[agent]]
name = "mayor"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.StoreHealth != (StoreHealthConfig{}) {
		t.Errorf("StoreHealth should be zero-valued when absent; got %+v", cfg.StoreHealth)
	}
	// An absent section must still yield the documented defaults, since the
	// patrol reads thresholds exclusively through the accessors.
	if !cfg.StoreHealth.StoreHealthEnabledOrDefault() {
		t.Error("StoreHealthEnabledOrDefault() = false, want true by default")
	}
	if got := cfg.StoreHealth.IntervalOrDefault(); got != 30*time.Second {
		t.Errorf("IntervalOrDefault() = %v, want 30s", got)
	}
	if got := cfg.StoreHealth.ConsecutiveFailsOrDefault(); got != 3 {
		t.Errorf("ConsecutiveFailsOrDefault() = %d, want 3", got)
	}
	if got := cfg.StoreHealth.ReapCooldownOrDefault(); got != 10*time.Minute {
		t.Errorf("ReapCooldownOrDefault() = %v, want 10m", got)
	}
	if got := cfg.StoreHealth.WriteProbeIntervalOrDefault(); got != 10*time.Minute {
		t.Errorf("WriteProbeIntervalOrDefault() = %v, want 10m", got)
	}
}

func TestMarshalOmitsEmptyStoreHealthSection(t *testing.T) {
	c := DefaultCity("test")
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "[storehealth]") {
		t.Errorf("Marshal output should not contain '[storehealth]' when empty:\n%s", data)
	}
}

func TestStoreHealthAccessorsFallBackOnUnusableValues(t *testing.T) {
	tests := []struct {
		name string
		cfg  StoreHealthConfig
	}{
		{"unparseable durations", StoreHealthConfig{
			Interval:           "30 secs",
			ReapCooldown:       "ten minutes",
			WriteProbeInterval: "1hour",
		}},
		{"zero durations", StoreHealthConfig{
			Interval:           "0s",
			ReapCooldown:       "0",
			WriteProbeInterval: "0m",
		}},
		{"negative durations", StoreHealthConfig{
			Interval:           "-30s",
			ReapCooldown:       "-10m",
			WriteProbeInterval: "-10m",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.IntervalOrDefault(); got != 30*time.Second {
				t.Errorf("IntervalOrDefault() = %v, want the 30s default", got)
			}
			if got := tt.cfg.ReapCooldownOrDefault(); got != 10*time.Minute {
				t.Errorf("ReapCooldownOrDefault() = %v, want the 10m default", got)
			}
			if got := tt.cfg.WriteProbeIntervalOrDefault(); got != 10*time.Minute {
				t.Errorf("WriteProbeIntervalOrDefault() = %v, want the 10m default", got)
			}
		})
	}

	// A non-positive count is unset, not a threshold of zero: a zero
	// threshold would confirm a poison on the very first failed cycle.
	for _, n := range []int{0, -1} {
		cfg := StoreHealthConfig{ConsecutiveFails: n}
		if got := cfg.ConsecutiveFailsOrDefault(); got != 3 {
			t.Errorf("ConsecutiveFailsOrDefault() with %d = %d, want the 3 default", n, got)
		}
	}
}

func TestStoreHealthEnabledDefaultsToTrue(t *testing.T) {
	enabled := true
	tests := []struct {
		name string
		cfg  StoreHealthConfig
		want bool
	}{
		{"unset defaults to enabled", StoreHealthConfig{}, true},
		{"explicit true", StoreHealthConfig{Enabled: &enabled}, true},
		{"explicit false", StoreHealthConfig{Enabled: new(bool)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.StoreHealthEnabledOrDefault(); got != tt.want {
				t.Errorf("StoreHealthEnabledOrDefault() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestValidateDurationsBadStoreHealthFields(t *testing.T) {
	cfg := &City{
		StoreHealth: StoreHealthConfig{
			Interval:           "30 secs",
			ReapCooldown:       "ten minutes",
			WriteProbeInterval: "1hour",
		},
	}
	warnings := ValidateDurations(cfg, "city.toml")
	if len(warnings) != 3 {
		t.Fatalf("expected 3 warnings, got %d: %v", len(warnings), warnings)
	}
	joined := strings.Join(warnings, "|")
	if !strings.Contains(joined, "[storehealth]") {
		t.Errorf("warnings should mention section [storehealth]: %v", warnings)
	}
	for _, field := range []string{"interval", "reap_cooldown", "write_probe_interval"} {
		if !strings.Contains(joined, field) {
			t.Errorf("warnings should mention %s field: %v", field, warnings)
		}
	}
}

func TestValidateDurationsStoreHealthValidOK(t *testing.T) {
	cfg := &City{
		StoreHealth: StoreHealthConfig{
			Interval:           "30s",
			ConsecutiveFails:   3,
			ReapCooldown:       "10m",
			WriteProbeInterval: "10m",
		},
	}
	warnings := ValidateDurations(cfg, "city.toml")
	if len(warnings) != 0 {
		t.Errorf("expected no warnings for valid [storehealth], got: %v", warnings)
	}
}
