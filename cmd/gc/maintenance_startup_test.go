package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/storehealth"
)

// TestMaintenanceStartupLine verifies the always-on startup banner reports
// the interval and distinguishes the active (GC wired) and observe-only
// (no-op) modes, so operators can confirm the loop initialized from the
// supervisor log. (gascity ga-tp7)
func TestMaintenanceStartupLine(t *testing.T) {
	t.Run("observe-only-when-not-active", func(t *testing.T) {
		got := maintenanceStartupLine(168*time.Hour, false)
		for _, want := range []string{"store-maintenance: loop started", "interval=168h", "observe-only"} {
			if !strings.Contains(got, want) {
				t.Errorf("startup line missing %q\ngot: %q", want, got)
			}
		}
		if strings.Contains(got, "mode=active") {
			t.Errorf("observe-only line must not claim active mode; got: %q", got)
		}
	})

	t.Run("active-when-wired", func(t *testing.T) {
		got := maintenanceStartupLine(24*time.Hour, true)
		for _, want := range []string{"store-maintenance: loop started", "interval=24h", "mode=active"} {
			if !strings.Contains(got, want) {
				t.Errorf("startup line missing %q\ngot: %q", want, got)
			}
		}
		if strings.Contains(got, "observe-only") {
			t.Errorf("active line must not claim observe-only; got: %q", got)
		}
	})
}

func TestStartMaintenanceLoopSeedsProjectionWhenSchedulingDisabled(t *testing.T) {
	cityPath := t.TempDir()
	want := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	provider := events.NewFake()
	provider.Record(events.Event{Type: events.StoreMaintenanceDone, Ts: want})
	cs := &controllerState{
		cfg:       &config.City{},
		cityPath:  cityPath,
		eventProv: provider,
	}

	cs.startMaintenanceLoop(context.Background())

	projection, ok, err := storehealth.LoadMaintenanceProjection(fsys.OSFS{}, cityPath)
	if err != nil || !ok {
		t.Fatalf("LoadMaintenanceProjection = (%+v, %v, %v), want startup seed", projection, ok, err)
	}
	if !projection.LastDoneAt.Equal(want) {
		t.Fatalf("LastDoneAt = %v, want %v", projection.LastDoneAt, want)
	}
}
