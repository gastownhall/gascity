package main

import (
	"bytes"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
)

// passRecord runs one trace-only pass of f at gatherNow and returns its
// record, failing on a pass that ended early.
func (f *gatherFixture) passRecord(t *testing.T) *passTrace {
	t.Helper()
	f.p.tracePass(f.env, gatherNow)
	rec := f.p.out.record.Load()
	if rec == nil || rec.Err != "" {
		t.Fatalf("pass record = %+v, want a completed pass", rec)
	}
	return rec
}

// intentKeys lists its' kinds and row IDs, a create's as "create".
func intentKeys(its []intent) []string {
	var out []string
	for _, it := range its {
		out = append(out, it.Kind+":"+it.Key.ID)
	}
	return out
}

var expiredHold = []string{"held_until", rowAt(-time.Minute)}

// Kills effects leaking out before C9: a pass whose admission let a row
// heal and two creates through submits none of them. No in-flight entry is
// recorded, no settlement is posted, and the store is unchanged.
func TestPassTraceOnlySubmitsNothing(t *testing.T) {
	f := newGatherFixture(t, poolRow("gc-1", "worker", 1, "asleep", expiredHold...), routedDemandBead("gc-r1"), routedDemandBead("gc-r2"))
	rec := f.passRecord(t)
	if got := intentKeys(rec.Admitted); !slices.Equal(got, []string{"create:", "create:", "row-heal:gc-1"}) {
		t.Fatalf("admitted = %v, want two creates and gc-1's heal", got)
	}
	if v := f.inflight.view(); len(v.Entries) != 0 {
		t.Fatalf("in flight after a trace-only pass = %+v, want none", v.Entries)
	}
	if s := f.p.settlements.drain(); len(s) != 0 {
		t.Fatalf("settlements after a trace-only pass = %+v, want none", s)
	}
	rows, err := f.backing.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range rows {
		if b.ID == "gc-1" && b.Metadata["held_until"] == "" {
			t.Fatal("the pass wrote gc-1's heal")
		}
	}
	if len(rows) != 3 {
		t.Fatalf("store holds %d beads after the pass, want the 3 seeded (a create landed?)", len(rows))
	}
}

// Kills the only enforcement of one intent per row (v5 R5): a row with an
// effect in flight is not decided, proposed or traced; once its settlement
// drains, the next pass proposes for it again.
func TestPassSkipsRowsWithEffectInFlight(t *testing.T) {
	f := newGatherFixture(t, poolRow("gc-1", "worker", 1, "asleep", expiredHold...), poolRow("gc-2", "worker", 2, "asleep", expiredHold...))
	k := rowKey{Leg: rowLeg, ID: "gc-1"}
	seq := f.inflight.add(inflightEntry{Kind: intentRowHeal, Key: k})
	rec := f.passRecord(t)
	if got := intentKeys(append(rec.Admitted, rec.Deferred...)); !slices.Equal(got, []string{"row-heal:gc-2"}) {
		t.Fatalf("intents with gc-1's heal in flight = %v, want gc-2's only", got)
	}
	for _, r := range rec.Rows {
		if r.Key == k {
			t.Fatalf("gc-1 traced while its effect is in flight: %+v", r)
		}
	}
	f.p.settlements.post(settlement{Key: k, Kind: intentRowHeal, Seq: seq})
	if got := intentKeys(f.passRecord(t).Admitted); !slices.Equal(got, []string{"row-heal:gc-1", "row-heal:gc-2"}) {
		t.Fatalf("intents after the settlement = %v, want both heals", got)
	}
}

// Kills a row panic that fails the pass, or that proposes for the row: the
// row holds and is traced with reason panic, and the other rows are decided.
func TestDecideRowPanicHoldsRow(t *testing.T) {
	saved := rowArms
	t.Cleanup(func() { rowArms = saved })
	rowArms = append([]rowArm{{"A0", func(r *rowFacts) (intent, bool) {
		if r.k.ID == "gc-1" {
			panic("boom")
		}
		return intent{}, false
	}}}, saved...)
	f := newGatherFixture(t, poolRow("gc-1", "worker", 1, "asleep", expiredHold...), poolRow("gc-2", "worker", 2, "asleep", expiredHold...))
	var stderr bytes.Buffer
	f.p.stderr = &stderr
	rec := f.passRecord(t)
	if got := intentKeys(rec.Admitted); !slices.Equal(got, []string{"row-heal:gc-2"}) {
		t.Fatalf("admitted = %v, want gc-2's heal only", got)
	}
	if !slices.Contains(rec.Rows, rowTrace{Key: rowKey{Leg: rowLeg, ID: "gc-1"}, Reason: decidePanic, Outcome: outcomeNone}) {
		t.Fatalf("rows traced = %+v, want gc-1 held with reason panic", rec.Rows)
	}
	if !strings.Contains(stderr.String(), "gc-1") || !strings.Contains(stderr.String(), "boom") {
		t.Fatalf("stderr = %q, want the row and its panic", stderr.String())
	}
}

// Kills C8 reading a summary that the next pass mutates (S-14): each pass
// publishes a fresh summary and relevant set, and the last pass's stay as
// they were when published.
func TestPassPublishesImmutableAllocationSummary(t *testing.T) {
	work := func(id string) beads.Bead {
		return beads.Bead{ID: id, Title: id, Type: "task", Status: "open", Assignee: "gc-1"}
	}
	f := newGatherFixture(t, poolRow("gc-1", "worker", 1, "active"), work("gc-w1"), routedDemandBead("gc-r1"))
	f.passRecord(t)
	s1, r1 := f.p.out.summary.Load(), f.p.out.relevant.Load()
	want := *s1
	want.AssignedWork, want.AssignedStores, want.AssignedStoreRefs = slices.Clone(s1.AssignedWork), slices.Clone(s1.AssignedStores), slices.Clone(s1.AssignedStoreRefs)
	if len(s1.AssignedWork) != 1 || len(s1.AssignedStores) != 1 || len(s1.ReadyRouted) != 1 || !(*r1)["gc-w1"] || !(*r1)["gc-r1"] {
		t.Fatalf("first summary = %+v, relevant %v: want gc-w1 assigned with its store, gc-r1 ready routed, both relevant", s1, *r1)
	}
	if _, err := f.cache.Create(work("gc-w2")); err != nil {
		t.Fatal(err)
	}
	f.passRecord(t)
	s2, r2 := f.p.out.summary.Load(), f.p.out.relevant.Load()
	if s2 == s1 || r2 == r1 || len(s2.AssignedWork) != 2 {
		t.Fatalf("second pass published %d assigned rows (same summary %v, same set %v), want 2 in fresh values", len(s2.AssignedWork), s2 == s1, r2 == r1)
	}
	if !reflect.DeepEqual(*s1, want) || len(*r1) != 2 {
		t.Fatalf("the first summary changed after the next pass: %+v, relevant %v", *s1, *r1)
	}
}

// Kills nondeterminism: the same World gives the same intents and traces
// whatever the census row order, with duplicate notifications posted, and a
// repeated pass proposes the same intents while tracing no unchanged row
// (R6), its mislabelled and unknown-state rows included.
func TestPassIsDeterministic(t *testing.T) {
	rows := []beads.Bead{
		poolRow("gc-1", "worker", 1, "asleep", expiredHold...),
		poolRow("gc-2", "worker", 2, "asleep", "quarantined_until", rowAt(-time.Minute)),
		poolRow("gc-3", "worker", 3, "hibernating"),
		sessionRow("gc-4", "state", "asleep"),
		routedDemandBead("gc-r1"), routedDemandBead("gc-r2"),
	}
	dir := t.TempDir()
	run := func(rows []beads.Bead, dupes int) (*gatherFixture, *passTrace) {
		f := newGatherFixture(t, rows...)
		f.env.CityPath = dir
		f.cur.Load().Cfg.Agents[0].MaxActiveSessions = intPtr(8) // room for creates beside the asleep rows
		for range dupes {
			f.p.settlements.post(settlement{Key: rowKey{Leg: rowLeg, ID: "gc-1"}, Kind: intentRowHeal, Seq: 9})
			f.p.markDirty("duplicate")
		}
		return f, f.passRecord(t)
	}
	f, first := run(rows, 0)
	reversed := slices.Clone(rows)
	slices.Reverse(reversed)
	_, other := run(reversed, 3)
	for _, cmp := range []struct {
		name      string
		got, want any
	}{
		{"admitted", other.Admitted, first.Admitted},
		{"deferred", other.Deferred, first.Deferred},
		{"rows", other.Rows, first.Rows},
	} {
		if !reflect.DeepEqual(cmp.got, cmp.want) {
			t.Fatalf("%s differ by row order:\n got  %+v\n want %+v", cmp.name, cmp.got, cmp.want)
		}
	}
	traced := make(map[string]string)
	for _, r := range first.Rows {
		traced[r.Key.ID] = r.Reason
	}
	if traced["gc-3"] != decideUnknownState || traced["gc-4"] != decideMislabelled || len(first.Admitted) != 4 {
		t.Fatalf("first pass traced %v and admitted %v, want gc-3 unknown-state, gc-4 mislabelled, two heals and two creates", traced, intentKeys(first.Admitted))
	}
	again := f.passRecord(t)
	if !reflect.DeepEqual(again.Admitted, first.Admitted) || len(again.Rows) != 0 {
		t.Fatalf("repeated pass admitted %v and traced %+v, want the same intents and no row", intentKeys(again.Admitted), again.Rows)
	}
}

// Kills admission's planner state dropped or a row deadline lost: the pass
// stores the bucket and the fair seed admission returned (an admitted pool
// create advances the seed), and reports the earliest row deadline as the
// next pass.
func TestPassCarriesAdmissionStateAndDeadlines(t *testing.T) {
	f := newGatherFixture(t, poolRow("gc-1", "worker", 1, "asleep", "held_until", rowAt(time.Minute)), routedDemandBead("gc-r1"), routedDemandBead("gc-r2"))
	res := f.p.tracePass(f.env, gatherNow)
	if f.p.fairSeed != 1 || f.p.bucket.LastRefill.IsZero() {
		t.Fatalf("planner state after a pass with pool creates = seed %d, bucket %+v; want seed 1 and a refilled bucket", f.p.fairSeed, f.p.bucket)
	}
	if want := gatherNow.Add(time.Minute + time.Second); !res.Next.Equal(want) {
		t.Fatalf("next pass = %v, want gc-1's hold expiry %v", res.Next, want)
	}
	if res.Counts.Admitted[intentCreate] != 2 {
		t.Fatalf("counts = %+v, want two admitted creates", res.Counts)
	}
}

// Kills the boot gate left out of the pass (P2): with no inventory pass yet,
// admission defers a destructive intent with cause boot-gate. No C2c1 arm
// proposes one, so a test arm proposes a stop.
func TestPassAppliesBootGate(t *testing.T) {
	saved := rowArms
	t.Cleanup(func() { rowArms = saved })
	rowArms = append([]rowArm{{"A0", func(*rowFacts) (intent, bool) { return intent{Kind: intentStop, Reason: "test"}, true }}}, saved...)
	f := newGatherFixture(t, poolRow("gc-1", "worker", 1, "active"))
	empty := NewObservationCache(&clock.Fake{Time: gatherNow}, time.Minute, "e1")
	f.env.Observations = func() *ObservationCache { return empty }
	rec := f.passRecord(t)
	if len(rec.Admitted) != 0 || len(rec.Deferred) != 1 || rec.Deferred[0].Cause != causeBootGate {
		t.Fatalf("admitted %v, deferred %+v; want the stop deferred by the boot gate", intentKeys(rec.Admitted), rec.Deferred)
	}
}
