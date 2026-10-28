package runtime

import (
	"strings"
	"testing"
	"time"
)

func TestFormatBeaconAt_Basic(t *testing.T) {
	ts := time.Date(2026, 2, 26, 15, 30, 0, 0, time.UTC)
	got := FormatBeaconAt("bright-lights", "mayor", false, ts)
	want := "[bright-lights] mayor \u2022 2026-02-26T15:30:00"
	if got != want {
		t.Errorf("FormatBeaconAt() = %q, want %q", got, want)
	}
}

func TestFormatBeaconAt_QualifiedAgent(t *testing.T) {
	ts := time.Date(2026, 2, 26, 15, 30, 0, 0, time.UTC)
	got := FormatBeaconAt("bright-lights", "hello-world/polecat", false, ts)
	want := "[bright-lights] hello-world/polecat \u2022 2026-02-26T15:30:00"
	if got != want {
		t.Errorf("FormatBeaconAt() = %q, want %q", got, want)
	}
}

func TestFormatBeaconAt_WithPrimeInstruction(t *testing.T) {
	ts := time.Date(2026, 2, 26, 15, 30, 0, 0, time.UTC)
	got := FormatBeaconAt("bright-lights", "worker", true, ts)
	if !strings.HasPrefix(got, "[bright-lights] worker \u2022 2026-02-26T15:30:00") {
		t.Errorf("beacon should start with identification, got %q", got)
	}
	if !strings.Contains(got, GCCommandName+" prime`") {
		t.Errorf("beacon should include gc prime instruction, got %q", got)
	}
}

func TestFormatBeaconAt_NoPrimeInstruction(t *testing.T) {
	ts := time.Date(2026, 2, 26, 15, 30, 0, 0, time.UTC)
	got := FormatBeaconAt("bright-lights", "mayor", false, ts)
	if strings.Contains(got, "gc prime") {
		t.Errorf("beacon should NOT include gc prime for hook agents, got %q", got)
	}
}

func TestFormatBeacon_ContainsTimestamp(t *testing.T) {
	got := FormatBeacon("my-city", "worker", false)
	if !strings.HasPrefix(got, "[my-city] worker \u2022 ") {
		t.Errorf("FormatBeacon() = %q, want prefix %q", got, "[my-city] worker \u2022 ")
	}
	parts := strings.SplitN(got, " \u2022 ", 2)
	if len(parts) != 2 {
		t.Fatalf("expected beacon with bullet separator, got %q", got)
	}
	if _, err := time.Parse("2006-01-02T15:04:05", parts[1]); err != nil {
		t.Errorf("timestamp %q not parseable: %v", parts[1], err)
	}
}
