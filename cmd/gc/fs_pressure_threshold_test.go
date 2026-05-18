package main

import "testing"

func TestFSPressureThreshold_EnvInvalidFallsBackToDefault(t *testing.T) {
	t.Setenv(fsPressureThresholdEnv, "not-a-number")
	if got := fsPressureThreshold(); got != defaultFSPressureThreshold {
		t.Fatalf("expected default %v on invalid env, got %v", defaultFSPressureThreshold, got)
	}
}

func TestFSPressureThreshold_RejectsInvalidNumericValues(t *testing.T) {
	for _, raw := range []string{"-1", "101", "NaN", "+Inf", "-Inf"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv(fsPressureThresholdEnv, raw)
			if got := fsPressureThreshold(); got != defaultFSPressureThreshold {
				t.Fatalf("expected default %v for %q, got %v", defaultFSPressureThreshold, raw, got)
			}
		})
	}
}

func TestFSPressureThreshold_EnvValidOverride(t *testing.T) {
	t.Setenv(fsPressureThresholdEnv, "25.5")
	if got := fsPressureThreshold(); got != 25.5 {
		t.Fatalf("expected 25.5, got %v", got)
	}
}

func TestFSPressureThreshold_EnvUnsetUsesDefault(t *testing.T) {
	t.Setenv(fsPressureThresholdEnv, "")
	if got := fsPressureThreshold(); got != defaultFSPressureThreshold {
		t.Fatalf("expected default %v, got %v", defaultFSPressureThreshold, got)
	}
}
