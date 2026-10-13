package main

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// The planner's settlement drain (CONTRACT v5 P1, P4, P5).

func settlePlanner(inflight plannerInflight) *planner {
	return newPlanner(newFakePlannerClock(plannerT0), func() time.Duration { return time.Minute }, nil, inflight, nil, io.Discard)
}

// Kills a backoff table that loses or rewrites what settlements carry: a
// row refusal backs its row off under its cause verbatim (a finalize's own
// refusal keeps causeFinalizePrefix, so admit holds the finalize back); a
// swap pause records nothing; a landing resets the row; a create carries its
// identity, stage and ConfigRev through to its record, and its worktree
// verdict to the work item's; an ambiguous create backs nothing off.
func TestDrainSettlementsBackOffByCause(t *testing.T) {
	p := settlePlanner(newInflightMap())
	row := func(id string) rowKey { return rowKey{Leg: rowLeg, ID: id} }
	finalizeCause := causeFinalizePrefix + "cas"
	for _, s := range []settlement{
		{Key: row("cas"), Kind: intentRowHeal, Outcome: settledRefused, Cause: "cas"},
		{Key: row("fin"), Kind: intentStop, Outcome: settledRefused, Cause: finalizeCause},
		{Key: row("swap"), Kind: intentStart, Outcome: settledRefused, Cause: causeSwapPause},
		{Key: row("ok"), Kind: intentRowHeal, Outcome: settledRefused, Cause: "cas"},
		createSettlement{Identity: "worker/w-1", ConfigRev: "rev-1", Stage: createStageFence, Err: errors.New("taken")}.settlement(),
		createSettlement{Identity: "worker/w-2", ConfigRev: "rev-1", Work: &workVerdict{BeadID: "b-1", Fingerprint: "fp-1", Refused: true}, Err: errors.New("bad evidence")}.settlement(),
		createSettlement{Identity: "worker/w-3", ConfigRev: "rev-1", Token: "tok-3", Ambiguous: true, Err: errors.New("timeout")}.settlement(),
	} {
		p.settlements.post(s)
	}
	p.drainSettlements(plannerT0)
	p.settlements.post(settlement{Key: row("ok"), Kind: intentRowHeal, Outcome: settledLanded})
	p.drainSettlements(plannerT0)

	recs := p.backoff.Snapshot()
	if r := recs[rowBackoffKey(row("cas"))]; r.Cause != "cas" || !r.live(plannerT0) {
		t.Errorf("row refusal: %+v, want a live record with cause %q", r, "cas")
	}
	fin := recs[rowBackoffKey(row("fin"))]
	if fin.Cause != finalizeCause || (intent{Kind: intentStop, Finalize: true}).finalizesOnly(fin) {
		t.Errorf("finalize refusal: %+v, want its cause verbatim, holding the finalize back", fin)
	}
	for _, k := range []string{rowBackoffKey(row("swap")), rowBackoffKey(row("ok")), createBackoffKey("worker/w-2"), createBackoffKey("worker/w-3")} {
		if r, ok := recs[k]; ok {
			t.Errorf("%s: %+v, want no record", k, r)
		}
	}
	if r := recs[createBackoffKey("worker/w-1")]; r.Cause != createStageFence || r.Fingerprint != "rev-1" {
		t.Errorf("create refused at the fence: %+v, want cause %q under rev-1", r, createStageFence)
	}
	if r := recs[workBackoffKey("b-1")]; r.Cause != createStageWorktree || r.Fingerprint != "fp-1" {
		t.Errorf("worktree refusal: %+v, want cause %q under fp-1", r, createStageWorktree)
	}

	p.settlements.post(createSettlement{Identity: "worker/w-1", Landed: true, Work: &workVerdict{BeadID: "b-1"}}.settlement())
	p.drainSettlements(plannerT0)
	recs = p.backoff.Snapshot()
	if _, ok := recs[createBackoffKey("worker/w-1")]; ok {
		t.Error("a landed create kept its identity's record")
	}
	if _, ok := recs[workBackoffKey("b-1")]; ok {
		t.Error("verified evidence kept the work item's record")
	}
}

// panicRecorder panics on every event.
type panicRecorder struct{ seen int }

func (r *panicRecorder) Record(events.Event) {
	r.seen++
	panic("recorder exploded")
}

// Kills a drain that interleaves the in-flight clears with steps that can
// panic, or lets one panicking event cost the others: with a recorder that
// panics on every event, every drained settlement has still left the
// in-flight map and reached the backoff table, every event was attempted,
// and the pass did not panic.
func TestDrainClearsInflightBeforeAnyStepThatCanPanic(t *testing.T) {
	m := newInflightMap()
	p := settlePlanner(m)
	rec := &panicRecorder{}
	p.rec = rec
	ev := events.Event{Type: "session.test"}
	for _, id := range []string{"a", "b"} {
		k := rowKey{Leg: rowLeg, ID: id}
		seq := m.add(inflightEntry{Kind: intentRowHeal, Key: k})
		p.settlements.post(settlement{Key: k, Kind: intentRowHeal, Seq: seq, Outcome: settledRefused, Cause: "cas", Facts: effectFacts{Events: []events.Event{ev}}})
	}
	var stderr strings.Builder
	p.stderr = &stderr
	p.pass = func(time.Time) passResult { return passResult{} }
	p.runPass(plannerT0)
	if !strings.Contains(stderr.String(), "recorder exploded") || rec.seen != 2 || p.last.Panicked {
		t.Fatalf("stderr %q after %d records (pass panicked %v), want each event's panic recovered alone", stderr.String(), rec.seen, p.last.Panicked)
	}
	if n := len(m.view().Entries); n != 0 {
		t.Fatalf("%d entries still in flight, want both settled ones cleared", n)
	}
	if n := len(p.backoff.Snapshot()); n != 2 {
		t.Fatalf("%d backoff records, want both refusals recorded", n)
	}
}
