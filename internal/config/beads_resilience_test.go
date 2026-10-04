package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
)

// loadBeadsResilienceConfig writes content to a temp city.toml and loads it.
func loadBeadsResilienceConfig(t *testing.T, content string) *City {
	t.Helper()
	path := filepath.Join(t.TempDir(), "city.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(fsys.OSFS{}, path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func TestBeadsResilienceDefaults(t *testing.T) {
	var r BeadsResilienceConfig
	if !r.EnabledOrDefault() {
		t.Error("EnabledOrDefault() = false for zero config, want true")
	}
	if got := r.ConsecutiveFailuresOrDefault(); got != 3 {
		t.Errorf("ConsecutiveFailuresOrDefault() = %d, want 3", got)
	}
	if got := r.OpenBaseOrDefault(); got != time.Second {
		t.Errorf("OpenBaseOrDefault() = %v, want 1s", got)
	}
	if got := r.OpenMaxOrDefault(); got != 60*time.Second {
		t.Errorf("OpenMaxOrDefault() = %v, want 60s", got)
	}
	if got := r.HalfOpenIntervalOrDefault(); got != 15*time.Second {
		t.Errorf("HalfOpenIntervalOrDefault() = %v, want 15s", got)
	}
	if got := r.MaxInflightPerScopeOrDefault(); got != 4 {
		t.Errorf("MaxInflightPerScopeOrDefault() = %d, want 4", got)
	}
	if got := r.MaxInflightGlobalOrDefault(); got != 16 {
		t.Errorf("MaxInflightGlobalOrDefault() = %d, want 16", got)
	}
	if got := r.MaxAdmissionWaitOrDefault(); got != 30*time.Second {
		t.Errorf("MaxAdmissionWaitOrDefault() = %v, want 30s", got)
	}
}

func TestBeadsResilienceMaxAdmissionWait(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{"unset uses default", "", 30 * time.Second},
		{"explicit positive", "45s", 45 * time.Second},
		{"unparseable uses default", "not-a-duration", 30 * time.Second},
		{"zero blocks forever", "0s", 0},
		{"negative blocks forever", "-1s", -time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := BeadsResilienceConfig{MaxAdmissionWait: tc.value}
			if got := r.MaxAdmissionWaitOrDefault(); got != tc.want {
				t.Errorf("MaxAdmissionWaitOrDefault() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBeadsResilienceMaxAdmissionWaitValidated(t *testing.T) {
	cfg := &City{}
	cfg.Beads.Resilience.MaxAdmissionWait = "5mins"
	warnings := ValidateDurations(cfg, "city.toml")
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "[beads.resilience]") && strings.Contains(w, "max_admission_wait") {
			found = true
		}
	}
	if !found {
		t.Fatalf("ValidateDurations warnings = %v, want a [beads.resilience] max_admission_wait warning", warnings)
	}
}

func TestBeadsResilienceInflightCaps(t *testing.T) {
	tests := []struct {
		name             string
		perScope, global int
		wantPer, wantGlb int
	}{
		{"zero uses defaults", 0, 0, 4, 16},
		{"explicit positive", 8, 32, 8, 32},
		{"negative disables", -1, -1, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := BeadsResilienceConfig{MaxInflightPerScope: tc.perScope, MaxInflightGlobal: tc.global}
			if got := r.MaxInflightPerScopeOrDefault(); got != tc.wantPer {
				t.Errorf("MaxInflightPerScopeOrDefault() = %d, want %d", got, tc.wantPer)
			}
			if got := r.MaxInflightGlobalOrDefault(); got != tc.wantGlb {
				t.Errorf("MaxInflightGlobalOrDefault() = %d, want %d", got, tc.wantGlb)
			}
		})
	}
}

func TestBeadsResilienceParsesFromTOML(t *testing.T) {
	cfg := loadBeadsResilienceConfig(t, `
[workspace]
name = "test"

[beads.resilience]
enabled = false
consecutive_failures = 5
open_base = "2s"
open_max = "30s"
half_open_interval = "10s"
`)
	r := cfg.Beads.Resilience
	if r.EnabledOrDefault() {
		t.Error("EnabledOrDefault() = true, want false (explicit enabled = false)")
	}
	if got := r.ConsecutiveFailuresOrDefault(); got != 5 {
		t.Errorf("ConsecutiveFailuresOrDefault() = %d, want 5", got)
	}
	if got := r.OpenBaseOrDefault(); got != 2*time.Second {
		t.Errorf("OpenBaseOrDefault() = %v, want 2s", got)
	}
	if got := r.OpenMaxOrDefault(); got != 30*time.Second {
		t.Errorf("OpenMaxOrDefault() = %v, want 30s", got)
	}
	if got := r.HalfOpenIntervalOrDefault(); got != 10*time.Second {
		t.Errorf("HalfOpenIntervalOrDefault() = %v, want 10s", got)
	}
}

func TestBeadsResilienceInvalidValuesFallBackToDefaults(t *testing.T) {
	r := BeadsResilienceConfig{
		ConsecutiveFailures: -2,
		OpenBase:            "not-a-duration",
		OpenMax:             "-5s",
		HalfOpenInterval:    "0s",
	}
	if got := r.ConsecutiveFailuresOrDefault(); got != 3 {
		t.Errorf("ConsecutiveFailuresOrDefault() = %d, want 3", got)
	}
	if got := r.OpenBaseOrDefault(); got != time.Second {
		t.Errorf("OpenBaseOrDefault() = %v, want 1s", got)
	}
	if got := r.OpenMaxOrDefault(); got != 60*time.Second {
		t.Errorf("OpenMaxOrDefault() = %v, want 60s", got)
	}
	if got := r.HalfOpenIntervalOrDefault(); got != 15*time.Second {
		t.Errorf("HalfOpenIntervalOrDefault() = %v, want 15s", got)
	}
}

func TestBeadsResilienceDurationsValidated(t *testing.T) {
	cfg := &City{}
	cfg.Beads.Resilience.OpenBase = "5mins"
	warnings := ValidateDurations(cfg, "city.toml")
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "[beads.resilience]") && strings.Contains(w, "open_base") {
			found = true
		}
	}
	if !found {
		t.Fatalf("ValidateDurations warnings = %v, want a [beads.resilience] open_base warning", warnings)
	}
}
