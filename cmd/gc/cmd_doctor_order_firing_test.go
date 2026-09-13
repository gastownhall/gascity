package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/events"
)

func TestBuildDoctorChecksOrderFiringCurrentUsesOrderRunHistory(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	cityDir := t.TempDir()
	t.Chdir(cityDir)
	mustWriteDoctorOrderFiringTestFile(t, filepath.Join(cityDir, "city.toml"), `[workspace]
name = "test-city"
`)
	if err := os.MkdirAll(filepath.Join(cityDir, "orders"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, "formulas"), 0o755); err != nil {
		t.Fatal(err)
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
	if _, err := store.Create(beads.Bead{
		Title:  "manual run mol-dog-stale-db",
		Type:   "molecule",
		Labels: []string{"order-run:mol-dog-stale-db"},
	}); err != nil {
		t.Fatalf("create recent order-run bead: %v", err)
	}

	now := time.Now().UTC()
	rec, err := events.NewFileRecorder(filepath.Join(cityDir, ".gc", "events.jsonl"), io.Discard)
	if err != nil {
		t.Fatalf("NewFileRecorder: %v", err)
	}
	rec.Record(events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)})
	rec.Record(events.Event{Type: events.OrderFired, Subject: "mol-dog-stale-db", Ts: now.Add(-13 * time.Hour)})
	if err := rec.Close(); err != nil {
		t.Fatalf("Close recorder: %v", err)
	}

	cfg := &config.City{FormulaLayers: config.FormulaLayers{City: []string{filepath.Join(cityDir, "formulas")}}}
	var stderr bytes.Buffer
	var check doctor.Check
	for _, candidate := range buildDoctorChecks(cityDir, cfg, nil, buildDoctorChecksOpts{Stderr: &stderr}) {
		if candidate.Name() == "order-firing-current" {
			check = candidate
			break
		}
	}
	if check == nil {
		t.Fatal("order-firing-current check not registered")
	}
	result := check.Run(&doctor.CheckContext{CityPath: cityDir})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK because order-run history is fresh; msg = %s; details = %v; stderr = %s", result.Status, result.Message, result.Details, stderr.String())
	}
}

func mustWriteDoctorOrderFiringTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}
