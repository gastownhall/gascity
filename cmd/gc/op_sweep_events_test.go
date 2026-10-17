package main

import (
	"encoding/json"
	"slices"
	"testing"

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
