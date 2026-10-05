package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/pendingwatch"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func createPendingLaneSession(t *testing.T, store beads.Store, name, state string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title:    name,
		Type:     sessionpkg.BeadType,
		Labels:   []string{sessionpkg.LabelSession},
		Metadata: map[string]string{"session_name": name, "state": state, "template": "worker"},
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}
	return b
}

// pendingLaneDeps is the lane's wiring with the city runtime's accessors
// replaced by a fixed store, provider and recorder.
func pendingLaneDeps(store beads.Store, sp runtime.Provider, rec events.Recorder) pendingwatch.Deps {
	return pendingwatch.Deps{
		List:     func() (pendingwatch.Listing, error) { return pendingInteractionListing(store) },
		Activity: func(name string) (time.Time, bool) { return pendingInteractionActivity(sp, name) },
		Probe: func(ctx context.Context, name string) pendingwatch.Observation {
			return pendingInteractionObserve(ctx, sp, name)
		},
		Publish: func(tr pendingwatch.Transition) error { return publishPendingTransition(rec, tr) },
	}
}

// The controller alone detects and records session.pending /
// session.pending_cleared: no API server, no event-stream client.
func TestPendingInteractionLaneRecordsTransitionsWithOnlyTheController(t *testing.T) {
	store := beads.NewMemStore()
	active := createPendingLaneSession(t, store, "worker-1", string(sessionpkg.StateActive))
	createPendingLaneSession(t, store, "worker-2", string(sessionpkg.StateAsleep))
	sp := runtime.NewFake()
	for _, name := range []string{"worker-1", "worker-2"} {
		if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatalf("Start %s: %v", name, err)
		}
	}
	sp.SetPendingInteraction("worker-1", &runtime.PendingInteraction{RequestID: "req-1", Kind: "approval", Prompt: "Run rm?", Options: []string{"yes", "no"}})
	sp.SetPendingInteraction("worker-2", &runtime.PendingInteraction{RequestID: "asleep", Kind: "approval"})
	log := events.NewFake()

	w := pendingwatch.New(pendingLaneDeps(store, sp, log), nil)
	w.Pass(context.Background())
	w.Pass(context.Background())

	got := log.Events
	if len(got) != 1 {
		t.Fatalf("events = %+v, want one session.pending", got)
	}
	e := got[0]
	if e.Type != events.SessionPending || e.Subject != active.ID || e.SessionID != active.ID || e.Actor != pendingInteractionActor {
		t.Fatalf("event = %+v, want session.pending for %s by %s", e, active.ID, pendingInteractionActor)
	}
	decoded, _, err := events.DecodePayload(e.Type, e.Payload)
	if err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	payload, ok := decoded.(api.SessionPendingPayload)
	if !ok {
		t.Fatalf("payload type = %T, want api.SessionPendingPayload (registration must stay stable)", decoded)
	}
	if payload.RequestID != "req-1" || payload.Prompt != "Run rm?" || payload.Template != "worker" || len(payload.Options) != 2 {
		t.Fatalf("payload = %+v", payload)
	}

	sp.SetPendingInteraction("worker-1", nil)
	w.Poke(active.ID)
	w.Pass(context.Background())
	if n := len(log.Events); n != 2 {
		t.Fatalf("events after the answer = %d, want 2: %+v", n, log.Events)
	}
	cleared, _, err := events.DecodePayload(log.Events[1].Type, log.Events[1].Payload)
	if err != nil {
		t.Fatalf("DecodePayload cleared: %v", err)
	}
	cp, ok := cleared.(api.SessionPendingClearedPayload)
	if !ok || cp.RequestID != "req-1" || cp.Reason != pendingwatch.ReasonResolved {
		t.Fatalf("cleared payload = %#v", cleared)
	}
}

// A restarted controller rebuilds its announced set from the event log: an
// interaction still pending is not announced again.
func TestPendingInteractionLaneRestartRebuildsFromTheEventLog(t *testing.T) {
	store := beads.NewMemStore()
	createPendingLaneSession(t, store, "worker-1", string(sessionpkg.StateActive))
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker-1", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sp.SetPendingInteraction("worker-1", &runtime.PendingInteraction{RequestID: "req-1", Kind: "approval"})
	log := events.NewFake()
	pendingwatch.New(pendingLaneDeps(store, sp, log), nil).Pass(context.Background())

	published, err := pendingwatch.RebuildFrom(log)
	if err != nil {
		t.Fatalf("RebuildFrom: %v", err)
	}
	pendingwatch.New(pendingLaneDeps(store, sp, log), published).Pass(context.Background())
	if n := len(log.Events); n != 1 {
		t.Fatalf("events after restart = %d, want 1 (no re-announcement): %+v", n, log.Events)
	}
}

// POST .../respond reaches the running lane through the controller state.
func TestControllerStatePokesPendingInteractionLane(t *testing.T) {
	cs := &controllerState{}
	cs.PokePendingInteractions("s-1") // no lane: a no-op
	var got []string
	cs.setPendingInteractionPoke(func(id string) { got = append(got, id) })
	var state api.State = cs
	poker, ok := state.(api.PendingInteractionPoker)
	if !ok {
		t.Fatal("controllerState does not implement api.PendingInteractionPoker")
	}
	poker.PokePendingInteractions("s-1")
	cs.setPendingInteractionPoke(nil)
	poker.PokePendingInteractions("s-2")
	if len(got) != 1 || got[0] != "s-1" {
		t.Fatalf("pokes = %q, want [s-1]", got)
	}
}

// Without a sessions store the lane probes nothing and clears nothing as gone.
func TestPendingInteractionListingWithoutStoreIsPartial(t *testing.T) {
	got, err := pendingInteractionListing(nil)
	if err != nil || !got.Partial || len(got.Sessions) != 0 {
		t.Fatalf("pendingInteractionListing(nil) = %+v, %v; want an empty partial listing", got, err)
	}
}
