package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/runtime"
)

// handWrittenCitySuspension is the brake an operator writes from any shell.
const handWrittenCitySuspension = `{"city":{"suspended":true}}`

// writeSuspensionFile writes raw as the city's suspension file, as an
// operator would by hand with no gc process running.
func writeSuspensionFile(t *testing.T, cityPath, raw string) {
	t.Helper()
	p := citylayout.SuspensionStateFile(cityPath)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
}

// AC-1: a suspension file hand-written as exactly {"city":{"suspended":true}}
// holds every wake path the controller has: the wake read both reconcilers
// and the orders lane use, v2's gather, legacy's desired-state build (which
// then wants no session at all), and the order pass.
func TestHandWrittenCitySuspensionHoldsEveryControllerWakePath(t *testing.T) {
	t.Setenv("GC_SUSPENDED", "")
	cr, _ := newPhaseFixtureRuntime(t, false, false)
	writeSuspensionFile(t, cr.cityPath, handWrittenCitySuspension)

	st, err := readSuspensionForWake(cr.cityPath)
	if err != nil {
		t.Fatalf("readSuspensionForWake: %v", err)
	}
	if !effectiveCitySuspended(cr.cfg, st) {
		t.Fatal("the wake read does not see the hand-written city suspension")
	}

	host := cr.newPlannerHost()
	gst, gerr := host.gather.Suspension()
	if gerr != nil || !effectiveCitySuspended(cr.cfg, gst) {
		t.Fatalf("v2 gather's suspension read = (%+v, %v), want the city suspended", gst, gerr)
	}

	got := buildDesiredStateWithSessionBeadsAtOpts(cr.cityName, cr.cityPath, time.Now(), time.Now(), cr.cfg, cr.sp, nil, nil, nil, nil, io.Discard, desiredStateOptions{readOnly: true})
	if len(got.State) != 0 {
		t.Fatalf("legacy desired state under a hand-written suspension = %v, want none", got.State)
	}

	od := &countingOrderDispatcher{}
	cr.od = od
	cr.wispIndexMigrationApplied = true
	cr.dispatchOrdersLocked(context.Background(), cr.cityPath, 0, cr.cfg)
	if od.dispatches != 0 {
		t.Fatalf("dispatched %d order pass(es) under a hand-written suspension", od.dispatches)
	}
}

// AC-1, legacy: with the city suspended on disk, buildDesiredState (the
// source the legacy start wave plans from) materializes nothing. The
// unsuspended control proves the same city otherwise demands one session,
// so the brake is what empties the result.
func TestSuspendedCityBrakesLegacyDesiredState(t *testing.T) {
	t.Setenv("GC_SUSPENDED", "")
	cfg := workerCity(1)
	cfg.Agents[0].ScaleCheck = printCountCommand(1)

	t.Run("control: unsuspended city demands its pool", func(t *testing.T) {
		ds := buildDesiredState("test-city", t.TempDir(), time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
		if len(ds.State) != 1 {
			t.Fatalf("unsuspended desired state size = %d, want 1", len(ds.State))
		}
	})
	t.Run("suspended city demands nothing", func(t *testing.T) {
		cityPath := t.TempDir()
		writeSuspensionFile(t, cityPath, handWrittenCitySuspension)
		ds := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
		if len(ds.State) != 0 {
			t.Fatalf("suspended desired state size = %d, want 0: %#v", len(ds.State), ds.State)
		}
	})
}
