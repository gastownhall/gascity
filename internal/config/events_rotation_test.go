package config

import (
	"strings"
	"testing"
	"time"
)

func TestEventsRotationConfigRoundTrip(t *testing.T) {
	enabled := false
	maxSize := int64(123456)
	checkRecords := 17
	checkSeconds := 23
	cfg := &City{
		Workspace: Workspace{Name: "test-city"},
		Events: EventsConfig{
			Provider: "file",
			Rotation: EventsRotationConfig{
				Enabled:              &enabled,
				MaxSizeBytes:         &maxSize,
				CheckIntervalRecords: &checkRecords,
				CheckIntervalSeconds: &checkSeconds,
				ArchiveRetainAge:     "720h",
			},
		},
	}

	data, err := cfg.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		"[events.rotation]",
		"enabled = false",
		"max_size_bytes = 123456",
		"check_interval_records = 17",
		"check_interval_seconds = 23",
		`archive_retain_age = "720h"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("marshaled config missing %q:\n%s", want, text)
		}
	}

	decoded, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	rot := decoded.Events.Rotation
	if rot.Enabled == nil || *rot.Enabled != false {
		t.Fatalf("Enabled = %v, want explicit false", rot.Enabled)
	}
	if rot.MaxSizeBytes == nil || *rot.MaxSizeBytes != 123456 {
		t.Fatalf("MaxSizeBytes = %v, want 123456", rot.MaxSizeBytes)
	}
	if rot.CheckIntervalRecords == nil || *rot.CheckIntervalRecords != 17 {
		t.Fatalf("CheckIntervalRecords = %v, want 17", rot.CheckIntervalRecords)
	}
	if rot.CheckIntervalSeconds == nil || *rot.CheckIntervalSeconds != 23 {
		t.Fatalf("CheckIntervalSeconds = %v, want 23", rot.CheckIntervalSeconds)
	}
	if rot.ArchiveRetainAge != "720h" {
		t.Fatalf("ArchiveRetainAge = %q, want 720h", rot.ArchiveRetainAge)
	}
}

func TestEventsRotationConfigDefaults(t *testing.T) {
	var rot EventsRotationConfig
	if !rot.EnabledOrDefault() {
		t.Fatal("EnabledOrDefault() = false, want true")
	}
	if got := rot.MaxSizeBytesOrDefault(); got != DefaultEventsRotationMaxSizeBytes {
		t.Fatalf("MaxSizeBytesOrDefault() = %d, want %d", got, DefaultEventsRotationMaxSizeBytes)
	}
	if got := rot.CheckIntervalRecordsOrDefault(); got != DefaultEventsRotationCheckIntervalRecords {
		t.Fatalf("CheckIntervalRecordsOrDefault() = %d, want %d", got, DefaultEventsRotationCheckIntervalRecords)
	}
	if got := rot.CheckIntervalDurationOrDefault(); got != DefaultEventsRotationCheckInterval {
		t.Fatalf("CheckIntervalDurationOrDefault() = %v, want %v", got, DefaultEventsRotationCheckInterval)
	}
	if got := rot.ArchiveRetainAgeDuration(); got != 0 {
		t.Fatalf("ArchiveRetainAgeDuration() = %v, want 0", got)
	}
}

func TestValidateEventsRotationWarnsOnShortRetainAge(t *testing.T) {
	cfg := &City{Events: EventsConfig{Rotation: EventsRotationConfig{ArchiveRetainAge: "24h"}}}
	warnings := ValidateEventsRotation(cfg)
	if len(warnings) != 1 {
		t.Fatalf("ValidateEventsRotation warnings = %d, want 1: %v", len(warnings), warnings)
	}
	want := "events.rotation: warning: archive_retain_age=24h may delete recent archives"
	if warnings[0] != want {
		t.Fatalf("warning = %q, want %q", warnings[0], want)
	}
}

func TestValidateEventsRotationDoesNotWarnForSevenDaysOrUnset(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *City
	}{
		{name: "unset", cfg: &City{}},
		{name: "zero", cfg: &City{Events: EventsConfig{Rotation: EventsRotationConfig{ArchiveRetainAge: "0s"}}}},
		{name: "seven days", cfg: &City{Events: EventsConfig{Rotation: EventsRotationConfig{ArchiveRetainAge: (168 * time.Hour).String()}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if warnings := ValidateEventsRotation(tc.cfg); len(warnings) != 0 {
				t.Fatalf("ValidateEventsRotation() warnings = %v, want none", warnings)
			}
		})
	}
}
