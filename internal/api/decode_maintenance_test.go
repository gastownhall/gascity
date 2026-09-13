package api

import (
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/api/genclient"
)

func TestMaintenanceRunViewFromGen_FullFields(t *testing.T) {
	t.Parallel()
	errMsg := "dolt error: lock failed"
	snap := "/var/city/.beads/dolt-backups/failed/2026-04-22T03-00-00Z"
	g := genclient.MaintenanceRunBody{
		StartedAt:    "2026-04-22T03:00:00Z",
		FinishedAt:   "2026-04-22T03:00:42Z",
		Stage:        "gc",
		Err:          &errMsg,
		BeforeBytes:  11_000_000_000,
		AfterBytes:   2_500_000_000,
		SnapshotPath: &snap,
		DurationS:    42.0,
	}
	got := maintenanceRunViewFromGen(g)
	want := MaintenanceRunView{
		StartedAt:       "2026-04-22T03:00:00Z",
		FinishedAt:      "2026-04-22T03:00:42Z",
		Stage:           "gc",
		Err:             errMsg,
		BeforeBytes:     11_000_000_000,
		AfterBytes:      2_500_000_000,
		SnapshotPath:    snap,
		DurationSeconds: 42.0,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MaintenanceRunView mismatch:\n got: %+v\nwant: %+v", got, want)
	}
}

func TestMaintenanceRunViewFromGen_OmitsMissingOptionals(t *testing.T) {
	t.Parallel()
	g := genclient.MaintenanceRunBody{
		StartedAt:  "2026-04-22T03:00:00Z",
		FinishedAt: "2026-04-22T03:00:05Z",
		Stage:      "done",
		DurationS:  5.0,
	}
	got := maintenanceRunViewFromGen(g)
	if got.Err != "" {
		t.Errorf("Err = %q; want empty when not provided", got.Err)
	}
	if got.SnapshotPath != "" {
		t.Errorf("SnapshotPath = %q; want empty when not provided", got.SnapshotPath)
	}
}

func TestMaintenanceStatusViewFromGen_NilReturnsEmptyHistory(t *testing.T) {
	t.Parallel()
	got := maintenanceStatusViewFromGen(nil)
	if got.History == nil {
		t.Fatal("History nil after decoding nil body; want empty slice")
	}
	if len(got.History) != 0 {
		t.Fatalf("History len = %d; want 0", len(got.History))
	}
}

func TestMaintenanceStatusViewFromGen_PopulatesFields(t *testing.T) {
	t.Parallel()
	inFlight := "2026-04-22T03:00:10Z"
	next := "2026-04-29T03:00:10Z"
	entries := []genclient.MaintenanceRunBody{
		{StartedAt: "2026-04-15T03:00:00Z", FinishedAt: "2026-04-15T03:00:05Z", Stage: "done", DurationS: 5},
		{StartedAt: "2026-04-22T03:00:00Z", FinishedAt: "2026-04-22T03:00:05Z", Stage: "done", DurationS: 5},
	}
	g := &genclient.MaintenanceStatusBody{
		Enabled:         true,
		IntervalSeconds: 604800,
		InFlight:        true,
		InFlightStart:   &inFlight,
		NextScheduled:   &next,
		LastRun:         &entries[1],
		History:         &entries,
	}
	got := maintenanceStatusViewFromGen(g)
	if !got.Enabled {
		t.Errorf("Enabled = false; want true")
	}
	if got.IntervalSec != 604800 {
		t.Errorf("IntervalSec = %d; want 604800", got.IntervalSec)
	}
	if !got.InFlight || got.InFlightStart != inFlight {
		t.Errorf("InFlight/Start = %v/%q; want true/%q", got.InFlight, got.InFlightStart, inFlight)
	}
	if got.NextScheduled != next {
		t.Errorf("NextScheduled = %q; want %q", got.NextScheduled, next)
	}
	if got.LastRun == nil || got.LastRun.StartedAt != "2026-04-22T03:00:00Z" {
		t.Errorf("LastRun not populated: %+v", got.LastRun)
	}
	if len(got.History) != 2 {
		t.Errorf("History len = %d; want 2", len(got.History))
	}
}

func TestMaintenanceStatusViewFromGen_EmptyHistoryList(t *testing.T) {
	t.Parallel()
	empty := []genclient.MaintenanceRunBody{}
	got := maintenanceStatusViewFromGen(&genclient.MaintenanceStatusBody{History: &empty})
	if got.History == nil {
		t.Fatal("History nil for empty list; want empty slice")
	}
	if len(got.History) != 0 {
		t.Fatalf("History len = %d; want 0", len(got.History))
	}
	if got.LastRun != nil {
		t.Errorf("LastRun = %+v; want nil when not provided", got.LastRun)
	}
}

func TestMaintenanceTriggerViewFromGen_NilBody(t *testing.T) {
	t.Parallel()
	got := maintenanceTriggerViewFromGen(nil)
	if got.Accepted || got.Run != nil || got.StartedAt != "" {
		t.Fatalf("nil body = %+v; want zero MaintenanceTriggerView", got)
	}
}

func TestMaintenanceTriggerViewFromGen_AsyncOnlyStartedAt(t *testing.T) {
	t.Parallel()
	s := "2026-04-22T03:00:00Z"
	g := &genclient.MaintenanceTriggerBody{Accepted: true, StartedAt: &s}
	got := maintenanceTriggerViewFromGen(g)
	if !got.Accepted {
		t.Error("Accepted = false; want true")
	}
	if got.StartedAt != s {
		t.Errorf("StartedAt = %q; want %q", got.StartedAt, s)
	}
	if got.Run != nil {
		t.Errorf("Run = %+v; want nil for async response", got.Run)
	}
}

func TestMaintenanceTriggerViewFromGen_SyncIncludesRun(t *testing.T) {
	t.Parallel()
	s := "2026-04-22T03:00:00Z"
	run := genclient.MaintenanceRunBody{StartedAt: s, FinishedAt: "2026-04-22T03:00:05Z", Stage: "done", DurationS: 5}
	g := &genclient.MaintenanceTriggerBody{Accepted: true, StartedAt: &s, Run: &run}
	got := maintenanceTriggerViewFromGen(g)
	if got.Run == nil {
		t.Fatal("Run = nil; want populated")
	}
	if got.Run.Stage != "done" {
		t.Errorf("Run.Stage = %q; want %q", got.Run.Stage, "done")
	}
	if got.Run.StartedAt != s {
		t.Errorf("Run.StartedAt = %q; want %q", got.Run.StartedAt, s)
	}
}
