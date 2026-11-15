package main

import (
	"encoding/json"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
	"github.com/gastownhall/gascity/internal/rollout"
)

// TestOpSweepDegradeRaisesOpsRecordingDegraded proves the sweeper's default
// degrade hook (OJ-2b) raises one ops.recording_degraded event per ops gate
// that is on, under that gate's mode, latched across passes; and that with
// every gate off it raises none.
func TestOpSweepDegradeRaisesOpsRecordingDegraded(t *testing.T) {
	newSweeper := func(opts ...rollout.ForTestOption) (*opSweeper, *events.Fake) {
		cs := newOpSweepCS(&beads.MemStore{HonorExplicitIDs: true}, opts...)
		rec := events.NewFake()
		cs.eventProv = rec
		s := cs.newOpSweeper()
		s.logf = func(string, ...any) {}
		return s, rec
	}

	off, offRec := newSweeper()
	off.reportDegrade(legacyop.Degrade{Mode: rollout.Off, Surface: legacyop.SurfaceController, Stage: legacyop.StageOpen, Reason: legacyop.DegradeNoNamespace})
	if evs, _ := offRec.List(events.Filter{Type: events.OpsRecordingDegraded}); len(evs) != 0 {
		t.Fatalf("gates off raised %d events", len(evs))
	}

	s, rec := newSweeper(rollout.WithOpsContent(rollout.Auto), rollout.WithOpsLifecycle(rollout.Require))
	for range 2 {
		s.safePass()
	}
	evs, _ := rec.List(events.Filter{Type: events.OpsRecordingDegraded})
	var got []events.OpsRecordingDegradedPayload
	for _, e := range evs {
		var p events.OpsRecordingDegradedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if e.Actor != legacyop.SurfaceController || e.Subject != p.Gate {
			t.Fatalf("event = %+v", e)
		}
		p.Detail = ""
		got = append(got, p)
	}
	want := []events.OpsRecordingDegradedPayload{
		{Gate: rollout.KeyOpsContent, Mode: "auto", Surface: "controller", Stage: "open", Reason: legacyop.DegradeNoNamespace},
		{Gate: rollout.KeyOpsLifecycle, Mode: "require", Surface: "controller", Stage: "open", Reason: legacyop.DegradeNoNamespace},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("events = %+v, want %+v (one per gate on, latched across the two passes)", got, want)
	}
}

// TestOpSweepCleanPassRearmsTheDegradeEvent proves the sweeper's latch is
// "first failure after a healthy period" (DES-O0API §5.5): an outage raises
// one event however long it lasts, and a clean pass re-arms it, so the next
// outage, an interval after the last event, raises another.
func TestOpSweepCleanPassRearmsTheDegradeEvent(t *testing.T) {
	broken := &beads.MemStore{HonorExplicitIDs: true} // no id prefix: no op namespace
	healthy := newOpSweepStore()
	cs := newOpSweepCS(broken, rollout.WithOpsContent(rollout.Auto))
	rec := events.NewFake()
	cs.eventProv = rec
	s := cs.newOpSweeper()
	s.logf = func(string, ...any) {}
	clk := &opSweepClock{now: time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)}
	s.events = api.NewOpsDegradeReporter(func() events.Recorder { return rec }, clk)
	count := func() int {
		evs, _ := rec.List(events.Filter{Type: events.OpsRecordingDegraded})
		return len(evs)
	}
	setStore := func(st beads.Store) {
		cs.mu.Lock()
		cs.cityBeadStore = st
		cs.mu.Unlock()
	}

	s.safePass()
	clk.Advance(3 * legacyop.DegradeLogInterval)
	s.safePass()
	if n := count(); n != 1 {
		t.Fatalf("one outage across two intervals raised %d events, want 1", n)
	}
	setStore(healthy)
	s.safePass()
	setStore(broken)
	clk.Advance(legacyop.DegradeLogInterval)
	s.safePass()
	if n := count(); n != 2 {
		t.Fatalf("an outage after a clean pass raised %d events in total, want 2", n)
	}
}

// opSweepToggleReadStore fails the sweep's read of open ops while failing is
// set.
type opSweepToggleReadStore struct {
	opSweepStore
	failing atomic.Bool
}

func (f *opSweepToggleReadStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Sort == beads.SortCreatedAsc && f.failing.Load() {
		return nil, errors.New("dolt down")
	}
	return f.opSweepStore.List(q)
}

// TestOpSweepCleanSweepRearmsTheSweepDegrade proves a sweep-stage degrade
// (the store opens, the sweep's read fails) re-arms after a pass whose sweep
// is clean, so the next sweep failure, an interval later, raises another
// event (DES-O0API §5.5).
func TestOpSweepCleanSweepRearmsTheSweepDegrade(t *testing.T) {
	store := &opSweepToggleReadStore{opSweepStore: newOpSweepStore()}
	cs := newOpSweepCS(store, rollout.WithOpsContent(rollout.Auto))
	rec := events.NewFake()
	cs.eventProv = rec
	s := cs.newOpSweeper()
	s.logf = func(string, ...any) {}
	clk := &opSweepClock{now: time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)}
	s.events = api.NewOpsDegradeReporter(func() events.Recorder { return rec }, clk)
	sweepEvents := func() int {
		evs, _ := rec.List(events.Filter{Type: events.OpsRecordingDegraded})
		n := 0
		for _, e := range evs {
			var p events.OpsRecordingDegradedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatal(err)
			}
			if p.Stage == legacyop.StageSweep {
				n++
			}
		}
		return n
	}

	store.failing.Store(true)
	s.safePass()
	clk.Advance(legacyop.DegradeLogInterval)
	s.safePass()
	if n := sweepEvents(); n != 1 {
		t.Fatalf("one sweep outage across an interval raised %d sweep events, want 1", n)
	}
	store.failing.Store(false)
	s.safePass()
	store.failing.Store(true)
	clk.Advance(legacyop.DegradeLogInterval)
	s.safePass()
	if n := sweepEvents(); n != 2 {
		t.Fatalf("a sweep outage after a clean sweep raised %d sweep events in total, want 2", n)
	}
}
