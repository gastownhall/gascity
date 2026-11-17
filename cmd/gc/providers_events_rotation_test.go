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

// TestNewEventsProviderForNameFileFailureReturnsNilProvider locks the contract
// the caller-side nil guards depend on: a file-backed provider that could not
// be opened comes back as a nil interface. Returning the concrete
// *events.FileRecorder instead would box a typed nil into events.Provider, so
// every `provider == nil` guard would read non-nil for a recorder that was
// never opened.
func TestNewEventsProviderForNameFileFailureReturnsNilProvider(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	provider, err := newEventsProviderForName("", filepath.Join(blocker, "events.jsonl"), io.Discard)
	if err == nil {
		if provider != nil {
			provider.Close() //nolint:errcheck // test cleanup
		}
		t.Fatal("newEventsProviderForName with an unopenable events path: err = nil, want an error")
	}
	if provider != nil {
		t.Fatalf("provider = %#v, want nil alongside the error", provider)
	}
}

// TestOpenCityEventsProviderNeverRotates pins the CLI provider as a secondary
// writer: even with a tiny configured threshold it must not rotate, because a
// CLI that opened its handle before the controller rotated would rotate the
// controller's fresh log (mc-zndi7.58).
func TestOpenCityEventsProviderNeverRotates(t *testing.T) {
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
	writeSmallRotationCityToml(t, cityDir)

	var stderr strings.Builder
	ep, code := openCityEventsProvider(&stderr, "test")
	if code != 0 || ep == nil {
		t.Fatalf("openCityEventsProvider() code = %d, provider nil = %t, stderr = %q", code, ep == nil, stderr.String())
	}
	rec, ok := ep.(*events.FileRecorder)
	if !ok {
		t.Fatalf("provider = %T, want *events.FileRecorder", ep)
	}
	recordAndAssertNoRotation(t, rec, cityDir)
}

// TestOpenCityRecorderAtNeverRotates is the same promise for the recorder
// behind every CLI emit (gc convoy control, gc sling, gc mail, ...).
func TestOpenCityRecorderAtNeverRotates(t *testing.T) {
	cityDir := t.TempDir()
	writeSmallRotationCityToml(t, cityDir)
	rec, ok := openCityRecorderAt(cityDir, io.Discard).(*events.FileRecorder)
	if !ok {
		t.Fatal("openCityRecorderAt did not return a *events.FileRecorder")
	}
	recordAndAssertNoRotation(t, rec, cityDir)
}

// TestNewEventsReaderForNameHoldsNoWriteHandle covers the provider read-only
// watchers (gc convoy control --serve --follow, gc status) open: it creates no
// file and refuses to record.
func TestNewEventsReaderForNameHoldsNoWriteHandle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".gc")
	ep, err := newEventsReaderForName("", filepath.Join(dir, "events.jsonl"), io.Discard)
	if err != nil {
		t.Fatalf("newEventsReaderForName: %v", err)
	}
	defer ep.Close() //nolint:errcheck // test cleanup
	ack, ok := ep.(events.AckRecorder)
	if !ok {
		t.Fatalf("provider = %T, want an events.AckRecorder", ep)
	}
	if err := ack.RecordAck(events.Event{Type: events.BeadCreated, Actor: "test"}); err == nil {
		t.Fatal("RecordAck on the read-only provider succeeded")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("read-only provider created %s (stat err %v)", dir, err)
	}
}

func TestNewFileEventsRecorderAppliesRotationConfig(t *testing.T) {
	dir := t.TempDir()
	maxSize, checkRecords := int64(512), 1
	rec, err := newFileEventsRecorder(filepath.Join(dir, "events.jsonl"), config.EventsConfig{
		Rotation: config.EventsRotationConfig{MaxSizeBytes: &maxSize, CheckIntervalRecords: &checkRecords},
	}, io.Discard)
	if err != nil {
		t.Fatalf("newFileEventsRecorder: %v", err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	for i := 0; i < 40; i++ {
		rec.Record(events.Event{Type: events.BeadCreated, Actor: "test", Subject: strings.Repeat("x", 80)})
	}
	rec.WaitForRotations()

	if got := countEventArchives(t, dir); got == 0 {
		t.Fatal("expected configured small max_size_bytes to produce at least one archive")
	}
}

func TestNewFileEventsRecorderEnvCanDisableRotation(t *testing.T) {
	t.Setenv("GC_EVENTS_ROTATION_ENABLED", "false")
	dir := t.TempDir()
	maxSize, checkRecords := int64(1), 1
	rec, err := newFileEventsRecorder(filepath.Join(dir, "events.jsonl"), config.EventsConfig{
		Rotation: config.EventsRotationConfig{MaxSizeBytes: &maxSize, CheckIntervalRecords: &checkRecords},
	}, io.Discard)
	if err != nil {
		t.Fatalf("newFileEventsRecorder: %v", err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	for i := 0; i < 40; i++ {
		rec.Record(events.Event{Type: events.BeadCreated, Actor: "test", Subject: strings.Repeat("x", 80)})
	}
	rec.WaitForRotations()

	if got := countEventArchives(t, dir); got != 0 {
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

// writeSmallRotationCityToml writes a city.toml whose [events.rotation] would
// rotate after a few records, for proving that a recorder ignores it.
func writeSmallRotationCityToml(t *testing.T, cityDir string) {
	t.Helper()
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
}

// recordAndAssertNoRotation writes well past the city.toml threshold through rec,
// closes it, and asserts that the log was never rotated.
func recordAndAssertNoRotation(t *testing.T, rec *events.FileRecorder, cityDir string) {
	t.Helper()
	for i := 0; i < 40; i++ {
		rec.Record(events.Event{Type: events.BeadCreated, Actor: "test", Subject: strings.Repeat("x", 80)})
	}
	if err := rec.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := countEventArchives(t, filepath.Join(cityDir, ".gc")); got != 0 {
		t.Fatalf("archives = %d, want 0: a secondary writer must never rotate", got)
	}
	all, err := events.ReadAll(filepath.Join(cityDir, ".gc", "events.jsonl"))
	if err != nil || len(all) != 40 {
		t.Fatalf("active log holds %d events (err %v), want all 40 unrotated", len(all), err)
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
