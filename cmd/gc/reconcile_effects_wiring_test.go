package main

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// The executor's wiring into the planner runtime (CONTRACT v5 P1, P7; D-14).

// Kills a planner runtime whose executor posts nowhere, and a recorder never
// handed to the planner: the executor's settlements reach the planner's
// queue, and bindHost hands over the recorder.
func TestPlannerRuntimeWiresTheExecutor(t *testing.T) {
	rt := newDefaultPlanner(io.Discard)
	rec := events.NewFake()
	rt.bindHost(plannerHost{rec: rec})
	if rt.planner.rec != rec {
		t.Fatal("bindHost did not hand the planner its recorder")
	}
	if err := rt.exec.submit(rowKey{ID: "a"}, sessionEffect{Kind: intentRowHeal, Deadline: time.Now().Add(time.Minute), Run: func(context.Context) settlement {
		return settlement{Outcome: settledLanded}
	}}); err != nil {
		t.Fatal(err)
	}
	rt.exec.stop(time.Now().Add(time.Minute)) // returns once the effect settled
	if got := rt.planner.settlements.drain(); len(got) != 1 || got[0].Key.ID != "a" {
		t.Fatalf("planner queue = %+v, want the effect's settlement", got)
	}
}

// Kills a provider swap that leaves the executor open to a start admitted
// before the pause: until resume, a submitted start settles refused with
// cause swap-pause; after resume, it runs.
func TestBeforeProviderSwapClosesExecutorStarts(t *testing.T) {
	cr := &CityRuntime{v2: newDefaultPlanner(io.Discard)}
	posted := make(chan settlement, 2)
	cr.v2.exec.post = func(s settlement) { posted <- s }
	start := sessionEffect{Kind: intentStart, Deadline: time.Now().Add(time.Minute), Run: func(context.Context) settlement {
		return settlement{Outcome: settledLanded}
	}}
	resume, err := cr.beforeProviderSwap(&config.City{})
	if err != nil {
		t.Fatal(err)
	}
	if err := cr.v2.exec.submit(rowKey{ID: "a"}, start); err != nil {
		t.Fatal(err)
	}
	if s := <-posted; s.Cause != causeSwapPause {
		t.Fatalf("start during the swap: %+v, want refused with cause %q", s, causeSwapPause)
	}
	resume()
	if err := cr.v2.exec.submit(rowKey{ID: "b"}, start); err != nil {
		t.Fatal(err)
	}
	if s := <-posted; s.Outcome != settledLanded {
		t.Fatalf("start after resume: %+v, want it run", s)
	}
}
