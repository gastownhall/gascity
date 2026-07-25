package storehealth

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestMaintenanceProjectionPath(t *testing.T) {
	got := MaintenanceProjectionPath("/c")
	want := filepath.Join("/c", ".gc", "store-maintenance-latest.json")
	if got != want {
		t.Fatalf("MaintenanceProjectionPath = %q, want %q", got, want)
	}
}

func TestMaintenanceProjectionLatest(t *testing.T) {
	done := time.Date(2026, 4, 8, 3, 0, 0, 0, time.UTC)
	fail := time.Date(2026, 4, 9, 3, 0, 0, 0, time.UTC)

	cases := []struct {
		name       string
		proj       MaintenanceProjection
		wantTs     time.Time
		wantStatus string
	}{
		{"empty", MaintenanceProjection{}, time.Time{}, ""},
		{"done only", MaintenanceProjection{LastDoneAt: done}, done, "success"},
		{"failed only", MaintenanceProjection{LastFailedAt: fail}, fail, "failed"},
		{"failed newer", MaintenanceProjection{LastDoneAt: done, LastFailedAt: fail}, fail, "failed"},
		{"done newer", MaintenanceProjection{LastDoneAt: fail, LastFailedAt: done}, fail, "success"},
		{"tie prefers success", MaintenanceProjection{LastDoneAt: done, LastFailedAt: done}, done, "success"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, status := tc.proj.Latest()
			if !ts.Equal(tc.wantTs) || status != tc.wantStatus {
				t.Fatalf("Latest() = (%v,%q), want (%v,%q)", ts, status, tc.wantTs, tc.wantStatus)
			}
		})
	}
}

func TestLoadMaintenanceProjectionAbsent(t *testing.T) {
	_, ok, err := LoadMaintenanceProjection(fsys.OSFS{}, t.TempDir())
	if err != nil {
		t.Fatalf("err = %v, want nil for absent sidecar", err)
	}
	if ok {
		t.Fatalf("ok = true, want false for absent sidecar")
	}
}

func TestLoadMaintenanceProjectionCorrupt(t *testing.T) {
	city := t.TempDir()
	if err := os.MkdirAll(filepath.Join(city, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(MaintenanceProjectionPath(city), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := LoadMaintenanceProjection(fsys.OSFS{}, city); err == nil || ok {
		t.Fatalf("corrupt sidecar: ok=%v err=%v, want ok=false err!=nil", ok, err)
	}
}

func TestLoadMaintenanceProjectionRejectsInvalidShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
	}{
		{name: "top level", data: "null"},
		{name: "last done", data: `{"last_done_at":null}`},
		{name: "last failed", data: `{"last_failed_at":null}`},
		{name: "empty object", data: `{}`},
		{name: "missing last failed", data: `{"last_done_at":"2026-07-01T04:00:00Z"}`},
		{name: "missing last done", data: `{"last_failed_at":"2026-07-01T04:00:00Z"}`},
		{name: "unknown field", data: `{"last_success_at":"2026-07-01T04:00:00Z"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city := t.TempDir()
			if err := os.MkdirAll(filepath.Join(city, ".gc"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(MaintenanceProjectionPath(city), []byte(tc.data), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, ok, err := LoadMaintenanceProjection(fsys.OSFS{}, city); err == nil || ok {
				t.Fatalf("invalid sidecar: ok=%v err=%v, want ok=false err!=nil", ok, err)
			}
		})
	}
}

func TestRecordMaintenanceEventCreatesAndPersists(t *testing.T) {
	city := t.TempDir()
	ts := time.Date(2026, 7, 1, 4, 0, 0, 0, time.UTC)
	if err := RecordMaintenanceEvent(fsys.OSFS{}, city, ts, "success", nil); err != nil {
		t.Fatalf("RecordMaintenanceEvent: %v", err)
	}
	p, ok, err := LoadMaintenanceProjection(fsys.OSFS{}, city)
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	if !p.LastDoneAt.Equal(ts) {
		t.Fatalf("LastDoneAt = %v, want %v", p.LastDoneAt, ts)
	}
	// No temp file is left behind by the atomic write (temp names are
	// final+".tmp.<pid>").
	leftover, _ := filepath.Glob(MaintenanceProjectionPath(city) + ".tmp.*")
	if len(leftover) != 0 {
		t.Fatalf("temp file(s) survived atomic write: %v", leftover)
	}
}

func TestRecordMaintenanceEventIgnoresUnknownStatusAndZero(t *testing.T) {
	city := t.TempDir()
	if err := RecordMaintenanceEvent(fsys.OSFS{}, city, time.Now(), "bogus", nil); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if err := RecordMaintenanceEvent(fsys.OSFS{}, city, time.Time{}, "success", nil); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if _, ok, _ := LoadMaintenanceProjection(fsys.OSFS{}, city); ok {
		t.Fatalf("projection written for a no-op record, want none")
	}
}
