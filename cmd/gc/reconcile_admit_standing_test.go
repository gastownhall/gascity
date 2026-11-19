package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/session"
)

// standingStartOn is startOn for a standing wake (intent.Standing).
func standingStartOn(i int, ep endpointKey, rank time.Duration) intent {
	it := startOn(i, ep, rank)
	it.Standing = true
	return it
}

// TestAdmitDemandBeforeStandingStarts is AC-2 of ga-z4joq0 (#7680): with two
// tokens, a standing start holding the oldest Rank, a demand start and a pool
// create, the demand start and the pool create are admitted and the standing
// start waits for budget. A standing start is admitted only from the tokens
// the demand starts and the creates leave.
func TestAdmitDemandBeforeStandingStarts(t *testing.T) {
	res := admit(admitIn(2, 4), []intent{
		standingStartOn(1, "", -time.Hour),
		startOn(2, "", 0),
		poolCreate("worker", ""),
	})
	if got := fmt.Sprint(keysOf(res.Admitted)); got != "[start:gc-2 create:]" {
		t.Fatalf("admitted %v, want the demand start then the pool create", got)
	}
	if len(res.Deferred) != 1 || res.Deferred[0].Key != admitRow(1) || res.Deferred[0].Cause != causeAwaitingBudget {
		t.Fatalf("deferred %+v, want the standing start awaiting budget", res.Deferred)
	}

	// With a third token the standing start runs after them.
	res = admit(admitIn(3, 4), []intent{standingStartOn(1, "", -time.Hour), startOn(2, "", 0), poolCreate("worker", "")})
	if got := fmt.Sprint(keysOf(res.Admitted)); got != "[start:gc-2 create: start:gc-1]" || len(res.Deferred) != 0 {
		t.Fatalf("admitted %v deferred %+v, want all three, the standing start last", got, res.Deferred)
	}
}

// TestAdmitStandingStartsRotateProbesWithinTheirTier pins probe rotation
// within the standing tier: on a half-open endpoint, the standing start
// refused least this episode is the one probe its tier offers, and a
// standing start never takes the probe from a demand start.
func TestAdmitStandingStartsRotateProbesWithinTheirTier(t *testing.T) {
	in := admitIn(5, 4)
	in.Endpoints = map[endpointKey]endpointView{"provider:a": {Gate: gateProbe, Refusals: map[string]int{"gc-1": 3}}}
	res := admit(in, []intent{standingStartOn(1, "provider:a", -time.Hour), standingStartOn(2, "provider:a", -time.Minute)})
	if got := fmt.Sprint(keysOf(res.Admitted)); got != "[start:gc-2]" {
		t.Fatalf("admitted %v, want gc-2: gc-1 was refused three times", got)
	}
	in.Endpoints["provider:a"] = endpointView{Gate: gateProbe, Refusals: map[string]int{"gc-3": 3}}
	res = admit(in, []intent{standingStartOn(1, "provider:a", -time.Hour), startOn(3, "provider:a", 0)})
	if got := fmt.Sprint(keysOf(res.Admitted)); got != "[start:gc-3]" {
		t.Fatalf("admitted %v, want the demand start's probe even with more refusals", got)
	}
}

// TestStartIntentCarriesStanding pins the v2 wiring of AwakeDecision.Standing:
// the decide pass copies it onto a Wake entry, and the start arm onto the
// start intent admission orders.
func TestStartIntentCarriesStanding(t *testing.T) {
	standing := newAllocFixture(t, chatCity("always")).sessions(chatRow("gc-1", "1"))
	if e := entryOf(t, standing.decide(), "gc-1"); e.Desired != desireWake || e.Reason != "named-always" || !e.Standing {
		t.Fatalf("idle always-named row = %s/%s standing %v, want a standing named-always wake", e.Desired, e.Reason, e.Standing)
	}
	demand := newAllocFixture(t, chatCity("on_demand")).
		sessions(chatRow("gc-1", "1", "configured_named_mode", "on_demand", "state", "active", "detached_at", ago(time.Hour))).
		alive("s-gc-1", InventoryAttrs{AttachedKnown: true}).fact("s-gc-1", FactPending, ObsYes)
	if e := entryOf(t, demand.decideSelecting("gc-1"), "gc-1"); e.Desired != desireWake || e.Reason != "pending" || e.Standing {
		t.Fatalf("pending row = %s/%s standing %v, want a demand wake", e.Desired, e.Reason, e.Standing)
	}

	k := rowKey{Leg: "sessions", ID: "gc-1"}
	row := censusRow{Key: k, Info: session.Info{ID: "gc-1", SessionName: "s-gc-1", State: "asleep"}}
	for _, want := range []bool{true, false} {
		e := &selectionEntry{Key: k, Desired: desireWake, Liveness: livenessGone, Template: "chat", Standing: want}
		it, ok := (&rowFacts{w: &World{Now: allocNow}, k: k, row: row, found: true, entry: e}).startIntent(decideStart)
		if !ok || it.Kind != intentStart || it.Standing != want {
			t.Fatalf("startIntent = (%+v, %v), want a start with Standing %v", it, ok, want)
		}
	}
}
