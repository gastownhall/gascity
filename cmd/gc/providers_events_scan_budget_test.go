package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
)

// TestOpenCityEventsProviderAppliesScanBudgetConfig proves the [events.scan_budget]
// TOML table actually reaches FileRecorder.ListNewestBounded's per-call archive
// budget, end to end through openCityEventsProvider — not just that the config
// struct decodes (that's covered in internal/config).
func TestOpenCityEventsProviderAppliesScanBudgetConfig(t *testing.T) {
	cityDir := t.TempDir()
	clearGCEnv(t)
	// Resolve via the --city flag rather than GC_CITY: flags win at the
	// highest priority tier (resolveContextFromFlags, step 1), ahead of every
	// env var, so this is immune to whatever GC_CITY/GC_RIG/GC_DIR the host
	// gc-rig session has ambiently set (see resolveContextAllowRemote's
	// documented priority chain). It's also census-free — a plain package
	// variable assignment isn't a tracked os/testing env or cwd mutation,
	// unlike t.Setenv/t.Chdir, so it doesn't grow the zero-headroom
	// ResourceEnvironment/ResourceCWD ledgers in internal/testpolicy/resourcecensus.
	oldCityFlag, oldRigFlag := cityFlag, rigFlag
	cityFlag, rigFlag = cityDir, ""
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

[events.scan_budget]
max_archive_bytes_per_request = 1
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

	// max_size_bytes=1 with check_interval_records=1 rotates on nearly every
	// record, so a handful of records produces several small archives.
	for i := 0; i < 5; i++ {
		rec.Record(events.Event{Type: events.BeadCreated, Actor: "test", Subject: strings.Repeat("x", 80)})
	}
	rec.WaitForRotations()

	if got := countEventArchives(t, filepath.Join(cityDir, ".gc")); got < 2 {
		t.Fatalf("archives = %d, want >= 2 (need multiple archives for a tiny budget to demonstrate truncation)", got)
	}

	got, truncated, reachedSeq, err := rec.ListNewestBounded(context.Background(), events.Filter{}, 100)
	if err != nil {
		t.Fatalf("ListNewestBounded: %v", err)
	}
	if !truncated {
		t.Fatalf("truncated = false, want true (configured max_archive_bytes_per_request=1 cannot cover every archive)")
	}
	if reachedSeq == 0 {
		t.Fatal("reachedSeq = 0, want a positive resume boundary when truncated")
	}
	if len(got) == 0 {
		t.Fatal("got 0 events, want at least the newest archive's events (forward-progress guarantee)")
	}
}
