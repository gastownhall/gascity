package eventfeed

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/pkg/eventexport"
)

func waitFor(t *testing.T, d time.Duration, cond func() bool) { //nolint:unparam // helper kept general
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", d)
}

func TestMuxSource_YieldsAndPicksUpNewCity(t *testing.T) {
	var pmu sync.Mutex
	provs := map[string]events.Provider{"c1": events.NewFake()}
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
	defer cancel()

	var gotMu sync.Mutex
	got := map[string][]uint64{}
	go func() {
		for {
			te, err := src.Next(ctx)
			if err != nil {
				return
			}
			gotMu.Lock()
			got[te.City] = append(got[te.City], te.Seq)
			gotMu.Unlock()
			cmu.Lock()
			if te.Seq > consumed[te.City] {
				consumed[te.City] = te.Seq
			}
			cmu.Unlock()
		}
	}()

	// c1 is present + empty at first build (floor 0): live records are delivered.
	time.Sleep(40 * time.Millisecond)
	f1 := provs["c1"].(*events.Fake)
	f1.Record(events.Event{Seq: 1, Type: "bead.closed", Ts: time.Now(), Actor: "a", Subject: "mc-1"})
	f1.Record(events.Event{Seq: 2, Type: "order.fired", Ts: time.Now(), Actor: "a", Subject: "sweep"})

	has := func(city string, seq uint64) bool {
		gotMu.Lock()
		defer gotMu.Unlock()
		for _, s := range got[city] {
			if s == seq {
				return true
			}
		}
		return false
	}
	waitFor(t, 2*time.Second, func() bool { return has("c1", 1) && has("c1", 2) })

	// add a second city after launch; it must be picked up on a rebuild.
	f2 := events.NewFake()
	pmu.Lock()
	provs["c2"] = f2
	pmu.Unlock()
	time.Sleep(40 * time.Millisecond) // let a rebuild floor c2 at 0
	f2.Record(events.Event{Seq: 1, Type: "bead.created", Ts: time.Now(), Actor: "b", Subject: "mc-9"})
	waitFor(t, 2*time.Second, func() bool { return has("c2", 1) })
}

// TestAdapter_NoLeakFromPayload proves the events.Event -> primitive conversion
// (toExport) forwards ONLY the envelope-safe fields and never copies Payload or
// Message — even when the payload carries bead metadata (where a run-root id
// would live). This is the leak proof that pkg/eventexport cannot provide (it
// never sees an events.Event). It also confirms #3654 does NOT resolve run_id by
// decoding the payload: the run-root id buried in metadata must NOT appear.
func TestAdapter_NoLeakFromPayload(t *testing.T) {
	opt := eventexport.Options{Salt: []byte("s"), ExportRef: true}
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
	batch.CityID = "c"
	batch.SchemaVersion = eventexport.SchemaVersion
	for _, e := range corpus {
		te := events.TaggedEvent{Event: e, City: "c"}
		ex := toExport(te)
		if env, ok := eventexport.ProjectFields(ex.Seq, ex.Type, ex.Ts, ex.Actor, ex.Subject, ex.RunID, ex.SessionID, opt); ok {
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
	// run_id/session_id must be ABSENT (the adapter does not resolve them in #3654).
	for _, en := range batch.Events {
		if en.RunID != "" || en.SessionID != "" {
			t.Fatalf("adapter must not populate run/session yet, got %+v", en)
		}
	}
	if len(batch.Events) < 3 {
		t.Fatalf("expected allowlisted events to survive, got %d", len(batch.Events))
	}
}
