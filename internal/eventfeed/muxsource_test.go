package eventfeed

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/testutil"
	"github.com/gastownhall/gascity/pkg/eventexport"
)

type watchSignalProvider struct {
	*events.Fake
	floors  chan uint64
	watches chan uint64
}

func newWatchSignalProvider() *watchSignalProvider {
	return &watchSignalProvider{
		Fake:    events.NewFake(),
		floors:  make(chan uint64, 4),
		watches: make(chan uint64, 4),
	}
}

func (p *watchSignalProvider) LatestSeq() (uint64, error) {
	seq, err := p.Fake.LatestSeq()
	if err != nil {
		return 0, err
	}
	select {
	case p.floors <- seq:
	default:
	}
	return seq, nil
}

func (p *watchSignalProvider) Watch(ctx context.Context, afterSeq uint64) (events.Watcher, error) {
	watcher, err := p.Fake.Watch(ctx, afterSeq)
	if err != nil {
		return nil, err
	}
	select {
	case p.watches <- afterSeq:
	default:
	}
	return watcher, nil
}

func requireFloorAt(t *testing.T, provider *watchSignalProvider, want uint64) {
	t.Helper()
	timer := time.NewTimer(testutil.GoroutineRaceTimeout)
	defer timer.Stop()
	select {
	case got := <-provider.floors:
		if got != want {
			t.Fatalf("provider floored at sequence %d, want %d", got, want)
		}
	case <-timer.C:
		t.Fatalf("provider was not floored within %s", testutil.GoroutineRaceTimeout)
	}
}

func requireNoFloorSample(t *testing.T, provider *watchSignalProvider) {
	t.Helper()
	select {
	case got := <-provider.floors:
		t.Fatalf("initialized provider head was sampled again at sequence %d", got)
	default:
	}
}

func requireWatchAfter(t *testing.T, provider *watchSignalProvider, want uint64) {
	t.Helper()
	timer := time.NewTimer(testutil.GoroutineRaceTimeout)
	defer timer.Stop()
	select {
	case got := <-provider.watches:
		if got != want {
			t.Fatalf("watch started after sequence %d, want %d", got, want)
		}
	case <-timer.C:
		t.Fatalf("watch did not start within %s", testutil.GoroutineRaceTimeout)
	}
}

func requireTaggedEvent(t *testing.T, received <-chan eventexport.TaggedEvent, city string, seq uint64) {
	t.Helper()
	timer := time.NewTimer(testutil.GoroutineRaceTimeout)
	defer timer.Stop()
	select {
	case got := <-received:
		if got.City != city || got.Seq != seq {
			t.Fatalf("received event %s:%d, want %s:%d", got.City, got.Seq, city, seq)
		}
	case <-timer.C:
		t.Fatalf("event %s:%d not received within %s", city, seq, testutil.GoroutineRaceTimeout)
	}
}

func TestMuxSource_PreservesInitializedZeroFloorAcrossRebuild(t *testing.T) {
	provider := newWatchSignalProvider()
	var acknowledged uint64
	src := NewMuxSource(
		func() map[string]events.Provider { return map[string]events.Provider{"c1": provider} },
		func() map[string]uint64 {
			if acknowledged == 0 {
				return nil
			}
			return map[string]uint64{"c1": acknowledged}
		},
		time.Hour,
		nil,
	)
	ctx, cancel := context.WithTimeout(context.Background(), testutil.GoroutineRaceTimeout)
	defer cancel()
	defer src.closeWatcher()

	if err := src.rebuild(ctx); err != nil {
		t.Fatalf("initial rebuild: %v", err)
	}
	requireFloorAt(t, provider, 0)
	requireWatchAfter(t, provider, 0)

	// Cross a rebuild boundary with no acknowledged event. Recording after the
	// first empty-city floor must not let the next rebuild advance that floor.
	src.closeWatcher()
	provider.Record(events.Event{Type: "bead.closed", Subject: "mc-1"})
	if err := src.rebuild(ctx); err != nil {
		t.Fatalf("second rebuild: %v", err)
	}
	requireNoFloorSample(t, provider)
	requireWatchAfter(t, provider, 0)

	got, err := src.Next(ctx)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if got.City != "c1" || got.Seq != 1 {
		t.Fatalf("Next returned %s:%d, want c1:1", got.City, got.Seq)
	}

	// Once the caller acknowledges an event, that durable cursor takes
	// precedence over the initial floor on every later rebuild.
	acknowledged = got.Seq
	src.closeWatcher()
	provider.Record(events.Event{Type: "bead.closed", Subject: "mc-2"})
	if err := src.rebuild(ctx); err != nil {
		t.Fatalf("acknowledged rebuild: %v", err)
	}
	requireNoFloorSample(t, provider)
	requireWatchAfter(t, provider, acknowledged)

	got, err = src.Next(ctx)
	if err != nil {
		t.Fatalf("Next after acknowledgement: %v", err)
	}
	if got.City != "c1" || got.Seq != 2 {
		t.Fatalf("Next after acknowledgement returned %s:%d, want c1:2", got.City, got.Seq)
	}
}

func TestMuxSource_YieldsAndPicksUpNewCity(t *testing.T) {
	var pmu sync.Mutex
	f1 := newWatchSignalProvider()
	provs := map[string]events.Provider{"c1": f1}
	providers := func() map[string]events.Provider {
		pmu.Lock()
		defer pmu.Unlock()
		out := make(map[string]events.Provider, len(provs))
		for k, v := range provs {
			out[k] = v
		}
		return out
	}

	// cursors() advances as the collector consumes, so resume moves forward.
	var cmu sync.Mutex
	consumed := map[string]uint64{}
	cursors := func() map[string]uint64 {
		cmu.Lock()
		defer cmu.Unlock()
		out := make(map[string]uint64, len(consumed))
		for k, v := range consumed {
			out[k] = v
		}
		return out
	}

	src := NewMuxSource(providers, cursors, 15*time.Millisecond, nil)
	ctx, cancel := context.WithCancel(context.Background())

	received := make(chan eventexport.TaggedEvent, 4)
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		for {
			te, err := src.Next(ctx)
			if err != nil {
				return
			}
			cmu.Lock()
			if te.Seq > consumed[te.City] {
				consumed[te.City] = te.Seq
			}
			cmu.Unlock()
			select {
			case received <- te:
			case <-ctx.Done():
				return
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		src.closeWatcher()
		timer := time.NewTimer(testutil.GoroutineRaceTimeout)
		defer timer.Stop()
		select {
		case <-consumerDone:
		case <-timer.C:
			t.Errorf("MuxSource consumer did not stop within %s", testutil.GoroutineRaceTimeout)
		}
	})

	// c1 is present + empty at first build (floor 0): live records are delivered.
	requireFloorAt(t, f1, 0)
	requireWatchAfter(t, f1, 0)
	f1.Record(events.Event{Seq: 1, Type: "bead.closed", Ts: time.Now(), Actor: "a", Subject: "mc-1"})
	f1.Record(events.Event{Seq: 2, Type: "order.fired", Ts: time.Now(), Actor: "a", Subject: "sweep"})
	requireTaggedEvent(t, received, "c1", 1)
	requireTaggedEvent(t, received, "c1", 2)

	// add a second city after launch; it must be picked up on a rebuild.
	f2 := newWatchSignalProvider()
	pmu.Lock()
	provs["c2"] = f2
	pmu.Unlock()
	requireFloorAt(t, f2, 0)
	requireWatchAfter(t, f2, 0)
	f2.Record(events.Event{Seq: 1, Type: "bead.created", Ts: time.Now(), Actor: "b", Subject: "mc-9"})
	requireTaggedEvent(t, received, "c2", 1)
}

// TestAdapter_NoLeakFromPayload proves the events.Event -> primitive conversion
// (toExport) forwards ONLY the envelope-safe fields and never copies Payload or
// Message — even when the payload carries bead metadata (where a run-root id
// would live). This is the leak proof that pkg/eventexport cannot provide (it
// never sees an events.Event). It also confirms #3654 does NOT resolve run_id by
// decoding the payload: the run-root id buried in metadata must NOT appear.
func TestAdapter_NoLeakFromPayload(t *testing.T) {
	// EmitCorrelation:true so emission is ON — proving the gc.root_bead_id buried
	// in Payload metadata is STILL not promoted to run_id (toExport reads only the
	// typed Event fields, which this corpus leaves empty).
	opt := eventexport.Options{Salt: []byte("sixteen-byte-salt-xx"), ExportRef: true, EmitCorrelation: true}
	ts := time.Date(2026, 6, 21, 10, 3, 27, 0, time.UTC)
	corpus := []events.Event{
		{
			Seq: 2, Type: "bead.closed", Ts: ts, Actor: "cache-reconcile", Subject: "mc-wisp-i6vz0e",
			Payload: json.RawMessage(`{"bead":{"title":"some private title","metadata":{"gc.root_bead_id":"wf-secret-root"}}}`),
		},
		{
			Seq: 3, Type: "order.failed", Ts: ts, Actor: "controller", Subject: "orphan-sweep",
			Message: "some failure detail that must not leak",
		},
		{
			Seq: 5, Type: "mail.sent", Ts: ts, Actor: "gascity/codex-mini-1", Subject: "mc-wisp-wcvwm2",
			Message: "private body", Payload: json.RawMessage(`{"to":"someone@example.com"}`),
		},
		{Seq: 7, Type: "convoy.closed", Ts: ts, Actor: "human", Subject: "gcg-4216"},
	}
	var batch eventexport.Batch
	batch.CityHash = eventexport.CityHash([]byte("salt"), "c")
	batch.SchemaVersion = eventexport.SchemaVersion
	for _, e := range corpus {
		te := events.TaggedEvent{Event: e, City: "c"}
		ex := toExport(te)
		if env, ok := eventexport.ProjectEvent(ex, opt); ok {
			batch.Events = append(batch.Events, env)
		}
	}
	out, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	blob := string(out)
	forbidden := []string{
		"private title", "metadata", "gc.root_bead_id", "wf-secret-root",
		"failure detail", "private body", "someone", "example.com",
		"payload", "Message", "Subject", "gascity/",
	}
	for _, f := range forbidden {
		if strings.Contains(blob, f) {
			t.Fatalf("LEAK: adapter batch contains %q\n%s", f, blob)
		}
	}
	// run_id/session_id must be ABSENT: the corpus carries them only inside Payload
	// metadata, and toExport never decodes Payload — it forwards only the typed
	// Event fields, which are empty here.
	for _, en := range batch.Events {
		if en.RunID != "" || en.SessionID != "" {
			t.Fatalf("adapter must not extract run/session from payload, got %+v", en)
		}
	}
	if len(batch.Events) < 3 {
		t.Fatalf("expected allowlisted events to survive, got %d", len(batch.Events))
	}
}

// TestToExport_ForwardsTypedRunSession proves the adapter forwards the typed
// Event.RunID/SessionID (stamped at the record site) through to the projected
// envelope when EmitCorrelation is on.
func TestToExport_ForwardsTypedRunSession(t *testing.T) {
	te := events.TaggedEvent{
		Event: events.Event{
			Seq: 1, Type: "bead.closed", Ts: time.Date(2026, 6, 21, 10, 3, 27, 0, time.UTC),
			Actor: "cache-reconcile", Subject: "mc-1", RunID: "wf-root-abc", SessionID: "sess-9f2a",
		},
		City: "c",
	}
	ex := toExport(te)
	if ex.RunID != "wf-root-abc" || ex.SessionID != "sess-9f2a" {
		t.Fatalf("toExport must forward typed run/session, got run=%q session=%q", ex.RunID, ex.SessionID)
	}
	env, ok := eventexport.ProjectEvent(ex, eventexport.Options{Salt: []byte("sixteen-byte-salt-xx"), ExportRef: true, EmitCorrelation: true})
	if !ok || env.RunID != "wf-root-abc" || env.SessionID != "sess-9f2a" {
		t.Fatalf("projected envelope must carry forwarded run/session, got %+v", env)
	}
}
