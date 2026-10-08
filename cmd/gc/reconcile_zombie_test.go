package main

import (
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/session"
)

// The zombie-pane classifier's and the classified set's tests (CONTRACT v5
// S5, P1, X10).

// Legacy pane fixtures (internal/runtime/dialog_test.go's).
const (
	paneTerminal   = "Error: model gpt-x was not found"
	paneRateLimit  = "Rate limit reached\n1. Keep trying\n2. Stop"
	paneSpendLimit = "What do you want to do?\nUsage credit balance: $573.37\n❯ Adjust monthly spend limit: $1503.19\n  Wait for limit to reset      Resets Jul 12 at 11pm (America/Los_Angeles)\nEnter to confirm · Esc to cancel"
	paneCrash      = "panic: runtime error: index out of range\ngoroutine 1 [running]:"
	paneBilling    = "notes mention Adjust monthly spend limit and Usage credit balance while documenting billing"
)

// crashCandidate is the exit facts of a zombie the exit decider would
// classify: woke an hour ago, no drain, no start in flight.
func crashCandidate() session.ExitFacts {
	return session.ExitFacts{
		LastWokeAt: gatherNow.Add(-time.Hour).Format(time.RFC3339), Now: gatherNow,
		StabilityThreshold: stabilityThreshold, ProductivityThreshold: churnProductivityThreshold,
	}
}

// Kills a classifier that turns terminal or rate-limited agents into restart
// loops (a crash), or a crash into a quarantine: each legacy fixture gets
// legacy's class, the rate-limit lane only where the exit decider quarantines
// (checkRateLimitStability), and a peek error classifies nothing.
func TestZombiePaneClassifierMatchesLegacy(t *testing.T) {
	drain := crashCandidate()
	drain.DrainPending = true
	for _, c := range []struct {
		name   string
		output string
		err    error
		exit   session.ExitFacts
		want   zombieClass
	}{
		{"terminal error", paneTerminal, nil, crashCandidate(), zombieClass{Kind: zombieTerminal, Reason: "model_not_found"}},
		{"terminal error over a rate-limit screen", paneTerminal + "\n" + paneRateLimit, nil, crashCandidate(), zombieClass{Kind: zombieTerminal, Reason: "model_not_found"}},
		{"rate-limit screen", paneRateLimit, nil, crashCandidate(), zombieClass{Kind: zombieRateLimited}},
		{"spend-limit modal", paneSpendLimit, nil, crashCandidate(), zombieClass{Kind: zombieRateLimited}},
		{"rate-limit screen while draining", paneRateLimit, nil, drain, zombieClass{Kind: zombieEmpty}},
		{"rate-limit screen never woken", paneRateLimit, nil, session.ExitFacts{Now: gatherNow}, zombieClass{Kind: zombieEmpty}},
		{"crash output", paneCrash, nil, crashCandidate(), zombieClass{Kind: zombieCrashed}},
		{"spend-limit words in scrollback", paneBilling, nil, crashCandidate(), zombieClass{Kind: zombieCrashed}},
		{"empty", "", nil, crashCandidate(), zombieClass{Kind: zombieEmpty}},
		{"peek error", paneCrash, errors.New("capture-pane: no such session"), crashCandidate(), zombieClass{Kind: zombiePeekError}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyZombiePane(c.output, c.err, c.exit); got != c.want {
				t.Fatalf("classifyZombiePane = %+v, want %+v", got, c.want)
			}
		})
	}
}

// Kills a class whose patch drifts from legacy's: terminal is
// providerTerminalErrorPatch, rate-limited legacy's 30-minute quarantine,
// and every other class writes nothing.
func TestZombieClassPatchIsLegacys(t *testing.T) {
	terminal := zombieClassPatch(zombieClass{Kind: zombieTerminal, Reason: "quota_exceeded"}, gatherNow)
	if !maps.Equal(terminal, session.MetadataPatch(providerTerminalErrorPatch("quota_exceeded", gatherNow))) {
		t.Fatalf("terminal patch %v", terminal)
	}
	quarantine := zombieClassPatch(zombieClass{Kind: zombieRateLimited}, gatherNow)
	if !maps.Equal(quarantine, session.RateLimitQuarantinePatch(gatherNow.Add(30*time.Minute))) {
		t.Fatalf("rate-limit patch %v", quarantine)
	}
	for _, k := range []zombieKind{zombieEmpty, zombieCrashed, zombiePeekError} {
		if p := zombieClassPatch(zombieClass{Kind: k}, gatherNow); p != nil {
			t.Fatalf("class %d writes %v, want nothing", k, p)
		}
	}
}

// Kills a classified set keyed by the row's token, which a re-key or warm
// reuse would confuse: an entry records the runtime's own token, survives a
// row token change and an unread identity, and goes when the runtime under
// the name reads another token or the row closes.
func TestClassifiedSetKeyedByRuntimeToken(t *testing.T) {
	k := rowKey{Leg: rowLeg, ID: "r"}
	census := func(rowToken string) *sessionCensus {
		return &sessionCensus{Rows: map[rowKey]censusRow{k: {Key: k, Info: session.Info{ID: "r", SessionName: "s-r", InstanceToken: rowToken}}}}
	}
	snap := func(token string, enriched bool) *ObservationSnapshot {
		obs := RuntimeObservation{Listed: RuntimeFact{Value: ObsYes, ObservedAt: gatherNow}, Identity: runtimeIdentity{Known: true, SessionID: "r", Token: token}}
		if enriched {
			obs.EnrichedAt = gatherNow
		}
		return &ObservationSnapshot{ByName: map[string]RuntimeObservation{"s-r": obs}, Primed: map[string]bool{"": true}}
	}
	p := settlePlanner(newInflightMap())
	p.settlements.post(settlement{Key: k, Kind: intentZombie, Outcome: settledLanded, Classified: &classifiedRuntime{Token: "rt-1"}})
	p.drainSettlements(gatherNow)
	for _, step := range []struct {
		name   string
		census *sessionCensus
		snap   *ObservationSnapshot
		want   bool
	}{
		{"recorded under a row token the runtime does not carry", census("row-0"), snap("rt-1", true), true},
		{"the row re-keyed to the runtime's token", census("rt-1"), snap("rt-1", true), true},
		{"identity unread", census("rt-1"), snap("rt-2", false), true},
		{"no inventory", census("rt-1"), nil, true},
		{"another runtime under the name", census("rt-1"), snap("rt-2", true), false},
	} {
		got := p.classifiedFor(step.census, step.snap, gatherNow, time.Minute)
		if token, ok := got[k]; ok != step.want || (ok && token != "rt-1") {
			t.Fatalf("%s: classified %v, want present=%v with the runtime token rt-1", step.name, got, step.want)
		}
	}
	p.settlements.post(settlement{Key: k, Kind: intentZombie, Outcome: settledLanded, Classified: &classifiedRuntime{Token: "rt-2"}})
	p.drainSettlements(gatherNow)
	world := p.classifiedFor(census("rt-2"), nil, gatherNow, time.Minute)
	delete(world, k)
	if p.classified[k] != "rt-2" {
		t.Fatalf("classified %v: a pass's copy shares the planner's set", p.classified)
	}
	if got := p.classifiedFor(&sessionCensus{Rows: map[rowKey]censusRow{}}, nil, gatherNow, time.Minute); len(got) != 0 || len(p.classified) != 0 {
		t.Fatalf("classified %v after the row closed, want it pruned", p.classified)
	}
}
