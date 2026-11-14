package api

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/testutil"
)

// The workflow projection on an event frame is optional enrichment; delivery is
// the stream's contract. These tests pin that contract for the supervisor-scope
// stream (GET /v0/events/stream) the way event_stream_delivery_test.go pins it
// for the city-scope stream, and pin the bound on the one case slack alone does
// not cover: a subscriber that is caught up (backlog == 0) while a bead store
// is wedged.
//
// beads.Store.Get takes no context, so a projection computed inline on the send
// loop parks the loop for as long as the store does — the event is never sent
// and the SSE keepalive stops with it. The projection is therefore raced
// against a deadline, and an event whose projection does not resolve in time is
// sent without one.

// openSupervisorEventStream opens GET /v0/events/stream replaying every city
// from zero against a single-city resolver.
func openSupervisorEventStream(t *testing.T, state *fakeState, gate *storeGate) *liveGlobalStream {
	t.Helper()
	return openStateEventStream(t, state, gate, "/v0/events/stream?after_cursor=0")
}

// openStateEventStream serves GET path in-process against a single-city
// resolver. The gate is released before the stream is torn down: a handler
// parked in a wedged Store.Get does not observe request cancellation, so
// tearing down first would turn a failure into a hang. A test that asserts on
// delivery must therefore decide before cleanup runs, never after the gate has
// let a parked handler finish.
func openStateEventStream(t *testing.T, state *fakeState, gate *storeGate, path string) *liveGlobalStream {
	t.Helper()
	sm := NewSupervisorMux(&singleStateResolver{state: state}, nil, false, "test", "", time.Now())
	stream := openSSEStream(t, sm, path, "")
	if gate != nil {
		// Registered after the stream's own cleanup, so it runs before it.
		t.Cleanup(gate.releaseAll)
	}
	return &liveGlobalStream{frames: stream.frames}
}

// firstEventFrameWithin returns the first event frame (one carrying a seq;
// heartbeats do not) or fails when none arrives within the race deadline.
func firstEventFrameWithin(t *testing.T, stream *liveGlobalStream, gate *storeGate) map[string]any {
	t.Helper()
	deadline := time.After(testutil.GoroutineRaceTimeout)
	for {
		select {
		case frame, ok := <-stream.frames:
			if !ok {
				t.Fatal("event stream ended before delivering the event")
			}
			data := decodeSSETestData(t, frame)
			if _, hasSeq := data["seq"]; !hasSeq {
				continue
			}
			return data
		case <-deadline:
			t.Fatalf("no event delivered after %d bead-store reads: the send loop is parked on a wedged store", gate.count())
			return nil
		}
	}
}

// collectTaggedEventSeqs reads tagged_event frames until want have arrived or
// the race deadline passes, and returns the seq of each frame it saw.
func collectTaggedEventSeqs(t *testing.T, stream *liveGlobalStream, want int) []uint64 {
	t.Helper()
	var seqs []uint64
	deadline := time.After(testutil.GoroutineRaceTimeout)
	for len(seqs) < want {
		select {
		case frame, ok := <-stream.frames:
			if !ok {
				return seqs
			}
			if frame.Event != "tagged_event" {
				continue
			}
			data := decodeSSETestData(t, frame)
			seq, _ := data["seq"].(float64)
			seqs = append(seqs, uint64(seq))
		case <-deadline:
			return seqs
		}
	}
	return seqs
}

// wedgedCityState is a city whose city and rig bead stores admit freeReads
// reads and then block until the gate is released.
func wedgedCityState(t *testing.T, name string, freeReads int) (*fakeState, *storeGate) {
	t.Helper()
	state := newFakeState(t)
	state.cityName = name
	gate := newStoreGate(t, freeReads)
	state.cityBeadStore = &gatedGetStore{Store: beads.NewMemStore(), gate: gate}
	state.stores = map[string]beads.Store{"alpha": &gatedGetStore{Store: beads.NewMemStore(), gate: gate}}
	return state, gate
}

// recordUnresolvableBeadEvent records a bead.updated whose subject no store
// resolves, so projecting it scans every store and produces nothing.
func recordUnresolvableBeadEvent(t *testing.T, recorder *events.FileRecorder, id string) {
	t.Helper()
	recorder.Record(events.Event{
		Type:    events.BeadUpdated,
		Actor:   "cache-reconcile",
		Subject: id,
		Payload: wrappedBeadPayload(t, id),
	})
}

// TestSupervisorEventStreamDeliversEveryEventWhenBeadStoresAreWedged is the
// supervisor-scope twin of
// TestCityEventStreamDeliversEveryEventWhenBeadStoresAreWedged: a replaying
// subscriber receives every event in seq order however slow the stores behind
// the optional projection are.
func TestSupervisorEventStreamDeliversEveryEventWhenBeadStoresAreWedged(t *testing.T) {
	const (
		eventCount = 40
		freeReads  = 24
	)
	state, gate := wedgedCityState(t, "wedged-city", freeReads)
	recorder := newTestEventRecorder(t, state)
	for i := range eventCount {
		recordUnresolvableBeadEvent(t, recorder, fmt.Sprintf("gcg-%04d", i))
	}

	stream := openSupervisorEventStream(t, state, gate)
	delivered := collectTaggedEventSeqs(t, stream, eventCount)
	if len(delivered) != eventCount {
		t.Fatalf("delivered %d of %d events after %d bead-store reads; the supervisor stream must keep up with the log regardless of bead-store latency",
			len(delivered), eventCount, gate.count())
	}
	for i, seq := range delivered {
		if want := uint64(i + 1); seq != want {
			t.Fatalf("delivered[%d] seq = %d, want %d (gap or reorder in %v)", i, seq, want, delivered)
		}
	}
}

// TestSupervisorEventStreamProjectsWorkflowEventsWhenKeepingUp is the
// supervisor-scope twin of TestCityEventStreamProjectsWorkflowEventsWhenKeepingUp:
// bounding the projection must not turn into dropping it. A caught-up stream
// still enriches a workflow bead event.
func TestSupervisorEventStreamProjectsWorkflowEventsWhenKeepingUp(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "wf-city"
	state.cityBeadStore = beads.NewMemStore()
	rigStore := beads.NewMemStore()
	state.stores = map[string]beads.Store{"alpha": rigStore}

	root, err := rigStore.Create(beads.Bead{
		Title: "Workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":           "workflow",
			"gc.workflow_id":    "wf_stream",
			"gc.scope_kind":     "rig",
			"gc.scope_ref":      "alpha",
			"gc.root_store_ref": "rig:alpha",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	child, err := rigStore.Create(beads.Bead{
		Title: "Step",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id":    root.ID,
			"gc.root_store_ref":  "rig:alpha",
			"gc.logical_bead_id": "node-1",
		},
	})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}
	payload, err := json.Marshal(child)
	if err != nil {
		t.Fatalf("Marshal(child): %v", err)
	}
	recorder := newTestEventRecorder(t, state)
	recorder.Record(events.Event{
		Type:    events.BeadUpdated,
		Actor:   "controller",
		Subject: child.ID,
		Payload: payload,
	})

	stream := openSupervisorEventStream(t, state, nil)
	_, data := stream.nextTaggedEvent(t)
	workflow, ok := data["workflow"].(map[string]any)
	if !ok {
		t.Fatalf("tagged_event has no workflow projection: %v", data)
	}
	if got := workflow["workflow_id"]; got != "wf_stream" {
		t.Fatalf("workflow.workflow_id = %v, want wf_stream", got)
	}
}

// TestCityEventStreamDeliversWhenCaughtUpAndBeadStoresWedge: the stream is
// caught up (backlog == 0), so the projection is attempted, and the bead stores
// are wedged from the first read. The event must still be delivered, without a
// workflow projection, within the projection deadline rather than when the
// store eventually answers.
func TestCityEventStreamDeliversWhenCaughtUpAndBeadStoresWedge(t *testing.T) {
	state, gate := wedgedCityState(t, "caught-up-city", 0)
	recorder := newTestEventRecorder(t, state)
	recordUnresolvableBeadEvent(t, recorder, "gcg-0001")

	stream := openStateEventStream(t, state, gate, "/v0/city/caught-up-city/events/stream?after_seq=0")
	if frame := firstEventFrameWithin(t, stream, gate); frame["workflow"] != nil {
		t.Fatalf("event frame carries a workflow projection from a wedged store: %v", frame)
	}
}

// TestSupervisorEventStreamDeliversWhenCaughtUpAndBeadStoresWedge is the
// supervisor-scope twin of the city test above.
func TestSupervisorEventStreamDeliversWhenCaughtUpAndBeadStoresWedge(t *testing.T) {
	state, gate := wedgedCityState(t, "caught-up-city", 0)
	recorder := newTestEventRecorder(t, state)
	recordUnresolvableBeadEvent(t, recorder, "gcg-0001")

	stream := openSupervisorEventStream(t, state, gate)
	if frame := firstEventFrameWithin(t, stream, gate); frame["workflow"] != nil {
		t.Fatalf("tagged_event carries a workflow projection from a wedged store: %v", frame)
	}
}
