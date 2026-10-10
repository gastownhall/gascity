package main

import (
	"context"
	"io"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
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

	published, err := pendingwatch.RebuildFrom(context.Background(), log, time.Time{})
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

// blockingRebuildLog is an event log whose pending-interaction rebuild read
// blocks until release is called or the read's ctx ends. Every other read is
// the Fake's.
type blockingRebuildLog struct {
	*events.Fake
	entered     chan struct{}
	enteredOnce sync.Once
	released    chan struct{}
	releaseOnce sync.Once

	mu     sync.Mutex
	filter events.Filter
}

func newBlockingRebuildLog() *blockingRebuildLog {
	return &blockingRebuildLog{Fake: events.NewFake(), entered: make(chan struct{}), released: make(chan struct{})}
}

func isPendingRebuildRead(f events.Filter) bool {
	return f.Type == events.SessionPending || f.Type == events.SessionPendingCleared ||
		slices.Contains(f.Types, events.SessionPending)
}

func (l *blockingRebuildLog) enter(f events.Filter) {
	l.mu.Lock()
	l.filter = f
	l.mu.Unlock()
	l.enteredOnce.Do(func() { close(l.entered) })
}

func (l *blockingRebuildLog) release() { l.releaseOnce.Do(func() { close(l.released) }) }

func (l *blockingRebuildLog) rebuildFilter() events.Filter {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.filter
}

func (l *blockingRebuildLog) List(f events.Filter) ([]events.Event, error) {
	if isPendingRebuildRead(f) {
		l.enter(f)
		<-l.released
	}
	return l.Fake.List(f)
}

func (l *blockingRebuildLog) ListHistoryTail(ctx context.Context, f events.Filter, limit int, _ bool) ([]events.Event, int, error) {
	if isPendingRebuildRead(f) {
		l.enter(f)
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-l.released:
		}
	}
	evs, err := l.Fake.List(f)
	if len(evs) > limit {
		evs = evs[len(evs)-limit:]
	}
	return evs, len(evs), err
}

// startRuntimeWithBlockingRebuild runs a city runtime whose event log blocks
// the pending-interaction rebuild, and returns once that read has begun.
func startRuntimeWithBlockingRebuild(t *testing.T) (*CityRuntime, *controllerState, *blockingRebuildLog, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	appendToFile(t, tomlPath, "\n[daemon]\npatrol_interval = \"10ms\"\n")
	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sp := runtime.NewFake()
	log := newBlockingRebuildLog()
	cr, err := newCityRuntime(CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    log,
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	if err != nil {
		t.Fatalf("building the city runtime: %v", err)
	}
	cs := newControllerState(context.Background(), cfg, sp, log, "test-city", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		cr.run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		log.release()
		awaitClose(t, done, "run to return")
	})
	awaitClose(t, log.entered, "the pending-interaction rebuild read")
	return cr, cs, log, cancel, done
}

// Rebuilding the announced set reads the event log, which on a long-lived
// city takes minutes. It must not hold the controller: run keeps ticking
// while the read blocks, the read takes both transition types in one pass
// bounded by a Since, and the poke is installed only once the rebuilt set is
// in place.
func TestCityRuntimeRunTicksWhilePendingInteractionRebuildBlocks(t *testing.T) {
	cr, cs, log, cancel, done := startRuntimeWithBlockingRebuild(t)

	awaitCond(t, func() bool { return cr.ordersLaneOf().wakePasses.Load() > 0 }, "a controller tick while the rebuild blocks")
	if cs.pendingPoke.Load() != nil {
		t.Fatal("the lane's poke was installed before its announced set was rebuilt")
	}
	f := log.rebuildFilter()
	if f.Type != "" || !slices.Contains(f.Types, events.SessionPending) || !slices.Contains(f.Types, events.SessionPendingCleared) {
		t.Fatalf("rebuild filter = %+v, want one read of both transition types", f)
	}
	if f.Since.IsZero() {
		t.Fatalf("rebuild filter = %+v, want the read bounded by a Since", f)
	}

	log.release()
	awaitCond(t, func() bool { return cs.pendingPoke.Load() != nil }, "the lane's poke after the rebuild")
	cancel()
	awaitClose(t, done, "run to return")
	if cs.pendingPoke.Load() != nil {
		t.Fatal("the lane's poke outlived the lane")
	}
}

// Stopping the city ends a rebuild still reading the event log.
func TestCityRuntimeRunStopsDuringPendingInteractionRebuild(t *testing.T) {
	cr, _, _, cancel, done := startRuntimeWithBlockingRebuild(t)

	awaitCond(t, func() bool { return cr.ordersLaneOf().wakePasses.Load() > 0 }, "a controller tick while the rebuild blocks")
	cancel()
	awaitClose(t, done, "run to return while the rebuild read blocks")
}
