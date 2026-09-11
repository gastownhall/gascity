package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

func TestEventsRotationSettingsFromConfigDefaults(t *testing.T) {
	got := eventsRotationSettingsFromConfig(config.EventsConfig{}, io.Discard)
	if !got.enabled {
		t.Fatal("enabled = false, want true")
	}
	if got.maxSizeBytes != config.DefaultEventsRotationMaxSizeBytes {
		t.Fatalf("maxSizeBytes = %d, want %d", got.maxSizeBytes, config.DefaultEventsRotationMaxSizeBytes)
	}
	if got.checkIntervalRecords != config.DefaultEventsRotationCheckIntervalRecords {
		t.Fatalf("checkIntervalRecords = %d, want %d", got.checkIntervalRecords, config.DefaultEventsRotationCheckIntervalRecords)
	}
	if got.checkInterval != config.DefaultEventsRotationCheckInterval {
		t.Fatalf("checkInterval = %v, want %v", got.checkInterval, config.DefaultEventsRotationCheckInterval)
	}
	if got.archiveRetainAge != 0 {
		t.Fatalf("archiveRetainAge = %v, want 0", got.archiveRetainAge)
	}
}

func TestEventsRotationSettingsEnvOverridesTOML(t *testing.T) {
	enabled := false
	maxSize := int64(999999)
	checkRecords := 33
	checkSeconds := 44
	cfg := config.EventsConfig{
		Rotation: config.EventsRotationConfig{
			Enabled:              &enabled,
			MaxSizeBytes:         &maxSize,
			CheckIntervalRecords: &checkRecords,
			CheckIntervalSeconds: &checkSeconds,
			ArchiveRetainAge:     "720h",
		},
	}
	t.Setenv("GC_EVENTS_ROTATION_ENABLED", "true")
	t.Setenv("GC_EVENTS_ROTATION_MAX_SIZE_BYTES", "2048")
	t.Setenv("GC_EVENTS_ROTATION_RETAIN_AGE", "24h")

	var stderr strings.Builder
	got := eventsRotationSettingsFromConfig(cfg, &stderr)
	if !got.enabled {
		t.Fatal("enabled = false, want env override true")
	}
	if got.maxSizeBytes != 2048 {
		t.Fatalf("maxSizeBytes = %d, want env override 2048", got.maxSizeBytes)
	}
	if got.checkIntervalRecords != 33 {
		t.Fatalf("checkIntervalRecords = %d, want TOML value 33", got.checkIntervalRecords)
	}
	if got.checkInterval != 44*time.Second {
		t.Fatalf("checkInterval = %v, want TOML value 44s", got.checkInterval)
	}
	if got.archiveRetainAge != 24*time.Hour {
		t.Fatalf("archiveRetainAge = %v, want env override 24h", got.archiveRetainAge)
	}
	wantWarning := "events.rotation: warning: archive_retain_age=24h may delete recent archives\n"
	if stderr.String() != wantWarning {
		t.Fatalf("stderr = %q, want %q", stderr.String(), wantWarning)
	}
}

func TestEventsRotationSettingsWarnsOnInvalidEnvOverrides(t *testing.T) {
	t.Setenv("GC_EVENTS_ROTATION_ENABLED", "maybe")
	t.Setenv("GC_EVENTS_ROTATION_MAX_SIZE_BYTES", "large")
	t.Setenv("GC_EVENTS_ROTATION_RETAIN_AGE", "soon")

	var stderr strings.Builder
	got := eventsRotationSettingsFromConfig(config.EventsConfig{}, &stderr)
	if !got.enabled {
		t.Fatal("enabled = false, want default true after invalid env")
	}
	if got.maxSizeBytes != config.DefaultEventsRotationMaxSizeBytes {
		t.Fatalf("maxSizeBytes = %d, want default %d after invalid env", got.maxSizeBytes, config.DefaultEventsRotationMaxSizeBytes)
	}
	if got.archiveRetainAge != 0 {
		t.Fatalf("archiveRetainAge = %v, want default 0 after invalid env", got.archiveRetainAge)
	}
	for _, want := range []string{
		`events.rotation: warning: ignoring invalid GC_EVENTS_ROTATION_ENABLED="maybe"`,
		`events.rotation: warning: ignoring invalid GC_EVENTS_ROTATION_MAX_SIZE_BYTES="large"`,
		`events.rotation: warning: ignoring invalid GC_EVENTS_ROTATION_RETAIN_AGE="soon"`,
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr missing %q:\n%s", want, stderr.String())
		}
	}
}

func TestNewEventsProviderForNameLegacyWrapper(t *testing.T) {
	ep, err := newEventsProviderForName("fake", "", io.Discard)
	if err != nil {
		t.Fatalf("newEventsProviderForName: %v", err)
	}
	defer ep.Close() //nolint:errcheck // test cleanup
	if _, err := ep.List(events.Filter{}); err != nil {
		t.Fatalf("legacy wrapper did not create fake provider: %v", err)
	}
}

func TestOpenCityEventsProviderAppliesRotationConfig(t *testing.T) {
	cityDir := t.TempDir()
	t.Setenv("GC_EVENTS", "")
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY_ROOT", "")
	t.Setenv("GC_RIG", "")
	oldCityFlag, oldRigFlag := cityFlag, rigFlag
	cityFlag, rigFlag = "", ""
	t.Cleanup(func() {
		cityFlag = oldCityFlag
		rigFlag = oldRigFlag
	})

	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`
[workspace]
name = "test-city"

[events.rotation]
max_size_bytes = 512
check_interval_records = 1
check_interval_seconds = 3600
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	var stderr strings.Builder
	ep, code := openCityEventsProvider(&stderr, "test")
	if code != 0 || ep == nil {
		t.Fatalf("openCityEventsProvider() code = %d, provider nil = %t, stderr = %q", code, ep == nil, stderr.String())
	}
	rec, ok := ep.(*events.FileRecorder)
	if !ok {
		t.Fatalf("provider = %T, want *events.FileRecorder", ep)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	for i := 0; i < 40; i++ {
		rec.Record(events.Event{Type: events.BeadCreated, Actor: "test", Subject: strings.Repeat("x", 80)})
	}
	rec.WaitForRotations()

	if got := countEventArchives(t, filepath.Join(cityDir, ".gc")); got == 0 {
		t.Fatal("expected configured small max_size_bytes to produce at least one archive")
	}
}

func TestOpenCityEventsProviderEnvCanDisableRotation(t *testing.T) {
	cityDir := t.TempDir()
	t.Setenv("GC_EVENTS", "")
	t.Setenv("GC_EVENTS_ROTATION_ENABLED", "false")
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY_ROOT", "")
	t.Setenv("GC_RIG", "")
	oldCityFlag, oldRigFlag := cityFlag, rigFlag
	cityFlag, rigFlag = "", ""
	t.Cleanup(func() {
		cityFlag = oldCityFlag
		rigFlag = oldRigFlag
	})

	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`
[workspace]
name = "test-city"

[events.rotation]
max_size_bytes = 1
check_interval_records = 1
check_interval_seconds = 1
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	var stderr strings.Builder
	ep, code := openCityEventsProvider(&stderr, "test")
	if code != 0 || ep == nil {
		t.Fatalf("openCityEventsProvider() code = %d, provider nil = %t, stderr = %q", code, ep == nil, stderr.String())
	}
	rec, ok := ep.(*events.FileRecorder)
	if !ok {
		t.Fatalf("provider = %T, want *events.FileRecorder", ep)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	for i := 0; i < 40; i++ {
		rec.Record(events.Event{Type: events.BeadCreated, Actor: "test", Subject: strings.Repeat("x", 80)})
	}
	rec.WaitForRotations()

	if got := countEventArchives(t, filepath.Join(cityDir, ".gc")); got != 0 {
		t.Fatalf("archives = %d, want 0 when env disables rotation", got)
	}
}

func TestOpenCityEventsProviderEmitsShortRetainAgeWarningFromConfig(t *testing.T) {
	cityDir := t.TempDir()
	t.Setenv("GC_EVENTS", "")
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY_ROOT", "")
	t.Setenv("GC_RIG", "")
	oldCityFlag, oldRigFlag := cityFlag, rigFlag
	cityFlag, rigFlag = "", ""
	t.Cleanup(func() {
		cityFlag = oldCityFlag
		rigFlag = oldRigFlag
	})

	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`
[workspace]

[events.rotation]
archive_retain_age = "24h"
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	writeBuiltinImportsFixture(t, cityDir, "core", "bd")

	var stderr strings.Builder
	ep, code := openCityEventsProvider(&stderr, "test")
	if code != 0 || ep == nil {
		t.Fatalf("openCityEventsProvider() code = %d, provider nil = %t, stderr = %q", code, ep == nil, stderr.String())
	}
	t.Cleanup(func() { _ = ep.Close() })

	want := "events.rotation: warning: archive_retain_age=24h may delete recent archives\n"
	if stderr.String() != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
}

func countEventArchives(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	count := 0
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "events.jsonl.archive-") && strings.HasSuffix(name, ".gz") {
			count++
		}
	}
	return count
}
