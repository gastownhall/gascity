package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/events"
)

func TestBuildDoctorChecksRegistersOrderOutcomeHealthy(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_DOLT", "skip")
	cfg := &config.City{Workspace: config.Workspace{Name: "demo"}}

	names := doctorCheckNames(buildDoctorChecks(cityDir, cfg, nil, buildDoctorChecksOpts{
		ControllerRunning:    false,
		SkipCityDoltCheck:    true,
		SkipManagedDoltCheck: true,
	}))

	firing := doctorCheckIndex(names, "order-firing-current")
	if firing < 0 {
		t.Fatalf("order-firing-current missing: %v", names)
	}
	outcome := doctorCheckIndex(names, "order-outcome-healthy")
	if outcome != firing+1 {
		t.Fatalf("order-outcome-healthy index = %d, want immediately after order-firing-current at %d; names=%v", outcome, firing, names)
	}
}

// TestBuildDoctorChecksOrderOutcomeHealthyUsesOrderRunHistory pins the gc doctor
// call site of ga-rislhg: buildDoctorChecks hands order-outcome-healthy the
// order-run fallback. The event log holds no outcome for the order, so the
// bounded event tail cannot settle its failure streak and only the persisted
// runs can. Built without WithOrderOutcomeRecentRunsFunc the check keeps the
// tail-only verdict ("no completed runs yet") and stays green, so this test
// fails when that option is dropped from buildDoctorChecks.
func TestBuildDoctorChecksOrderOutcomeHealthyUsesOrderRunHistory(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	cityDir := t.TempDir()
	t.Chdir(cityDir)
	mustWriteDoctorOrderFiringTestFile(t, filepath.Join(cityDir, "city.toml"), `[workspace]
name = "test-city"
`)
	for _, dir := range []string{"orders", "formulas"} {
		if err := os.MkdirAll(filepath.Join(cityDir, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWriteDoctorOrderFiringTestFile(t, filepath.Join(cityDir, "orders", "mol-dog-stale-db.toml"), `[order]
exec = "true"
trigger = "cron"
schedule = "0 */4 * * *"
`)
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatalf("ensureScopedFileStoreLayout: %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatalf("ensurePersistedScopeLocalFileStore: %v", err)
	}
	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	// Three failed runs: exactly the failure threshold, all persisted only in the
	// order-run store.
	for i := 0; i < 3; i++ {
		if _, err := store.Create(beads.Bead{
			Title:  "failed run mol-dog-stale-db",
			Type:   "molecule",
			Labels: []string{"order-run:mol-dog-stale-db", "exec-failed"},
		}); err != nil {
			t.Fatalf("create failed order-run bead %d: %v", i, err)
		}
	}

	// A controller start long before the runs, and no order.completed or
	// order.failed event for the order: the event tail has no evidence at all.
	rec, err := events.NewFileRecorder(filepath.Join(cityDir, ".gc", "events.jsonl"), io.Discard)
	if err != nil {
		t.Fatalf("NewFileRecorder: %v", err)
	}
	rec.Record(events.Event{Type: events.ControllerStarted, Ts: time.Now().UTC().Add(-24 * time.Hour)})
	if err := rec.Close(); err != nil {
		t.Fatalf("Close recorder: %v", err)
	}

	cfg := &config.City{FormulaLayers: config.FormulaLayers{City: []string{filepath.Join(cityDir, "formulas")}}}
	var stderr bytes.Buffer
	var check doctor.Check
	for _, candidate := range buildDoctorChecks(cityDir, cfg, nil, buildDoctorChecksOpts{Stderr: &stderr}) {
		if candidate.Name() == "order-outcome-healthy" {
			check = candidate
			break
		}
	}
	if check == nil {
		t.Fatal("order-outcome-healthy check not registered")
	}

	result := check.Run(&doctor.CheckContext{CityPath: cityDir})
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want Warning because the order-run store holds 3 failed runs; msg = %s; details = %v; stderr = %s", result.Status, result.Message, result.Details, stderr.String())
	}
	if result.Severity != doctor.SeverityAdvisory {
		t.Errorf("severity = %v, want advisory: a failing order must not gate gc doctor", result.Severity)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), "mol-dog-stale-db: at least 3 consecutive failures") {
		t.Errorf("details = %v, want the store-derived streak %q", result.Details, "mol-dog-stale-db: at least 3 consecutive failures")
	}
}
