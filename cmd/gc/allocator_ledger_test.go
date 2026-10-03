package main

import (
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// Ledger tests run the ledger on the synctest bubble's clock (time.Now) and
// move time with advance, so TTLs, veto backoff and the lag bound are exact.
// The pure verdicts take the pass's time explicitly.

var (
	ledgerRowA = rowKey{Leg: "sessions", ID: "gc-a"}
	ledgerRowB = rowKey{Leg: "sessions", ID: "gc-b"}
)

func ledgerGrant(id string, k rowKey, cost int) ledgerEntry {
	return ledgerEntry{ID: id, Kind: kindGrant, Key: k, Endpoint: "provider:p", Cost: cost, ReservedAt: time.Now(), State: ledgerReserved}
}

func ledgerCreate(id, token string) ledgerEntry {
	return ledgerEntry{
		ID: id, Kind: kindCreate, Key: rowKey{Leg: "sessions"}, Endpoint: "provider:p", Cost: 1,
		ReservedAt: time.Now(), State: ledgerReserved, Marker: ledgerMarker{InstanceToken: token},
	}
}

// ledgerEntryOf returns id's entry, or false once it left the ledger.
func ledgerEntryOf(l *intentLedger, id string) (ledgerEntry, bool) {
	for _, e := range l.View() {
		if e.ID == id {
			return e, true
		}
	}
	return ledgerEntry{}, false
}

// ledgerCensusOf builds a ledgerCensus that holds every leg of its rows plus legs.
func ledgerCensusOf(rows map[rowKey]ledgerRow, legs ...string) ledgerCensus {
	c := ledgerCensus{Rows: rows, Legs: make(map[string]bool)}
	for k := range rows {
		c.Legs[k.Leg] = true
	}
	for _, leg := range legs {
		c.Legs[leg] = true
	}
	return c
}

// Kills: any edge outside CONTRACT §5.1 accepted (issued → released loses a
// start's slot mid-flight; committed → issued replays an effect); a terminal
// entry left in the ledger; a lost CAS that still moves the entry.
func TestLedgerStateMachineEveryEdge(t *testing.T) {
	legal := map[[2]ledgerState]bool{
		{ledgerReserved, ledgerReleased}: true,
		{ledgerReserved, ledgerIssued}:   true,
		{ledgerIssued, ledgerCommitted}:  true,
		{ledgerIssued, ledgerFailed}:     true,
		{ledgerCommitted, ledgerCleared}: true,
		{ledgerFailed, ledgerCleared}:    true,
	}
	states := []ledgerState{ledgerReserved, ledgerIssued, ledgerCommitted, ledgerFailed, ledgerReleased, ledgerCleared}
	// into returns a ledger holding grant "g" in from; released and cleared
	// entries have left the ledger.
	into := func(from ledgerState) *intentLedger {
		l := newIntentLedger(time.Now)
		l.Reserve(ledgerGrant("g", ledgerRowA, 1))
		path := map[ledgerState][]ledgerState{
			ledgerIssued:    {ledgerIssued},
			ledgerCommitted: {ledgerIssued, ledgerCommitted},
			ledgerFailed:    {ledgerIssued, ledgerFailed},
			ledgerReleased:  {ledgerReleased},
			ledgerCleared:   {ledgerIssued, ledgerCommitted, ledgerCleared},
		}[from]
		cur := ledgerReserved
		for _, next := range path {
			if !l.Transition("g", cur, next, nil) {
				t.Fatalf("setup %d → %d refused", cur, next)
			}
			cur = next
		}
		return l
	}
	for _, from := range states {
		for _, to := range states {
			l := into(from)
			got := l.Transition("g", from, to, nil)
			if got != legal[[2]ledgerState{from, to}] {
				t.Errorf("Transition(%d → %d) = %v, want %v", from, to, got, !got)
			}
			e, present := ledgerEntryOf(l, "g")
			switch {
			case got && (to == ledgerReleased || to == ledgerCleared):
				if present {
					t.Errorf("%d → %d: terminal entry still in the ledger", from, to)
				}
			case got && e.State != to:
				t.Errorf("%d → %d: state %d after a won CAS", from, to, e.State)
			case !got && present && e.State != from:
				t.Errorf("%d → %d: lost CAS moved the entry to %d", from, to, e.State)
			}
		}
	}
	// A stale from loses even on a legal edge.
	l := into(ledgerIssued)
	if l.Transition("g", ledgerReserved, ledgerReleased, nil) {
		t.Fatal("release of an issued grant through a stale from won")
	}
	if l.Transition("missing", ledgerReserved, ledgerIssued, nil) {
		t.Fatal("transition of a missing entry won")
	}
}

// Kills: a non-CAS transition (both the key's issue and the allocator's
// release win, so a start runs on a refunded token). Run under -race.
func TestLedgerTransitionsAreCompareAndSwap(t *testing.T) {
	issued, released := 0, 0
	for i := 0; i < 500; i++ {
		l := newIntentLedger(time.Now)
		l.Reserve(ledgerGrant("g", ledgerRowA, 1))
		var wg sync.WaitGroup
		start := make(chan struct{})
		var issueOK, releaseOK bool
		var refund int
		wg.Add(2)
		go func() { defer wg.Done(); <-start; issueOK = l.Issue("g", ledgerRowA) }()
		go func() { defer wg.Done(); <-start; refund, releaseOK = l.Release("g") }()
		close(start)
		wg.Wait()
		if issueOK == releaseOK {
			t.Fatalf("round %d: issue=%v release=%v, want exactly one winner", i, issueOK, releaseOK)
		}
		e, present := ledgerEntryOf(l, "g")
		switch {
		case issueOK:
			issued++
			if !present || e.State != ledgerIssued || refund != 0 {
				t.Fatalf("round %d: issue won but entry=%+v present=%v refund=%d", i, e, present, refund)
			}
		default:
			released++
			if present || refund != 1 {
				t.Fatalf("round %d: release won but present=%v refund=%d", i, present, refund)
			}
		}
	}
	t.Logf("issue won %d, release won %d", issued, released)
}

// Kills: releasing an issued grant (a start losing its slot mid-flight);
// releasing a landed effect or a veto; a refund on a lost release; issuing a
// grant that was released or that belongs to another key.
func TestLedgerOnlyReservedEntriesRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newIntentLedger(time.Now)
		for _, id := range []string{"issued", "committed", "failed"} {
			l.Reserve(ledgerGrant(id, rowKey{Leg: "sessions", ID: id}, 1))
			l.Issue(id, rowKey{Leg: "sessions", ID: id})
		}
		l.Commit("committed", ledgerMarker{Incarnation: 2})
		l.Fail("failed", true, ledgerMarker{Incarnation: 2})
		l.Veto(ledgerRowB, time.Time{}, "fresh-read")
		var vetoID string
		for _, e := range l.View() {
			if e.Kind == kindVeto {
				vetoID = e.ID
			}
		}
		for _, id := range []string{"issued", "committed", "failed", vetoID} {
			if refund, ok := l.Release(id); ok || refund != 0 {
				t.Errorf("Release(%s) = (%d, %v), want (0, false)", id, refund, ok)
			}
			if _, present := ledgerEntryOf(l, id); !present {
				t.Errorf("Release(%s) removed the entry", id)
			}
		}

		l.Reserve(ledgerGrant("g", ledgerRowA, 1))
		if l.Issue("g", ledgerRowB) {
			t.Fatal("Issue of another key's grant won")
		}
		l.Reserve(ledgerCreate("c", "tok"))
		if l.Issue("c", rowKey{Leg: "sessions"}) {
			t.Fatal("Issue of a create won; only the create executor issues creates")
		}
		if refund, ok := l.Release("g"); !ok || refund != 1 {
			t.Fatalf("Release(reserved) = (%d, %v), want (1, true)", refund, ok)
		}
		if refund, ok := l.Release("g"); ok || refund != 0 {
			t.Fatalf("second Release = (%d, %v), want (0, false)", refund, ok)
		}
		if l.Issue("g", ledgerRowA) {
			t.Fatal("Issue after release won")
		}
	})
}

// Kills: Reserve accepting a veto, an empty ID or a duplicate ID (two
// entries for one grant would debit twice and clear once); a create with no
// instance token, which an ambiguous outcome could never clear by marker
// (N60).
func TestLedgerReserveRefusesDuplicateEmptyAndVeto(t *testing.T) {
	l := newIntentLedger(time.Now)
	if !l.Reserve(ledgerGrant("g", ledgerRowA, 1)) {
		t.Fatal("Reserve refused a fresh grant")
	}
	for name, e := range map[string]ledgerEntry{
		"duplicate":                        ledgerGrant("g", ledgerRowB, 1),
		"empty ID":                         ledgerGrant("", ledgerRowB, 1),
		"veto":                             {ID: "v", Kind: kindVeto, Key: ledgerRowB},
		"create without an instance token": {ID: "c", Kind: kindCreate, Key: rowKey{Leg: "sessions"}, Cost: 1}, // N60
	} {
		if l.Reserve(e) {
			t.Errorf("Reserve(%s) won", name)
		}
	}
	if got := len(l.View()); got != 1 {
		t.Fatalf("ledger holds %d entries, want 1", got)
	}
	e, _ := ledgerEntryOf(l, "g")
	if e.State != ledgerReserved || e.Key != ledgerRowA {
		t.Fatalf("entry = %+v, want the first grant, reserved", e)
	}
}

// Kills: clearing before the marker is in the census (R18: a pass counts
// the effect zero times); clearing a grant on an older incarnation; clearing
// a grant whose row is missing from a leg the census did not read; never
// clearing on a later incarnation or a closed row; a refund on a landed
// create; a grant with no recorded incarnation clearing on any open row
// (N60).
func TestLedgerClearsCreateOnRowMarkerGrantOnIncarnation(t *testing.T) {
	now := time.Unix(1_000, 0)
	created := ledgerEntry{
		ID: "c", Kind: kindCreate, Key: rowKey{Leg: "sessions", ID: "gc-new"}, Cost: 1, State: ledgerCommitted,
		WroteRow: true, Marker: ledgerMarker{RowID: "gc-new", InstanceToken: "tok"},
	}
	ambiguous := created
	ambiguous.Key.ID, ambiguous.Marker.RowID = "", ""
	granted := ledgerEntry{
		ID: "g", Kind: kindGrant, Key: ledgerRowA, Cost: 1, State: ledgerCommitted, WroteRow: true,
		ProviderCalled: true, Marker: ledgerMarker{Incarnation: 5},
	}
	issued := granted
	issued.State, issued.WroteRow = ledgerIssued, false
	unmarked := granted
	unmarked.Marker.Incarnation = 0
	tests := []struct {
		name   string
		e      ledgerEntry
		c      ledgerCensus
		clears bool
		refund int
	}{
		{"create, row not yet visible", created, ledgerCensusOf(map[rowKey]ledgerRow{ledgerRowA: {}}), false, 0},
		{"create, row ID on its leg", created, ledgerCensusOf(map[rowKey]ledgerRow{{"sessions", "gc-new"}: {}}), true, 0},
		{"create, row ID on another leg", created, ledgerCensusOf(map[rowKey]ledgerRow{{"rig", "gc-new"}: {}}), true, 0},
		{"create, token only (ambiguous outcome)", ambiguous, ledgerCensusOf(map[rowKey]ledgerRow{{"sessions", "gc-x"}: {InstanceToken: "tok"}}), true, 0},
		{"create, other token", ambiguous, ledgerCensusOf(map[rowKey]ledgerRow{{"sessions", "gc-x"}: {InstanceToken: "other"}}), false, 0},
		{"grant, census on the old incarnation", granted, ledgerCensusOf(map[rowKey]ledgerRow{ledgerRowA: {Incarnation: 4}}), false, 0},
		{"grant, census at the PreWake generation", granted, ledgerCensusOf(map[rowKey]ledgerRow{ledgerRowA: {Incarnation: 5}}), true, 0},
		{"grant, census at a later generation", granted, ledgerCensusOf(map[rowKey]ledgerRow{ledgerRowA: {Incarnation: 9}}), true, 0},
		{"grant, row closed (gone from its read leg)", granted, ledgerCensusOf(nil, "sessions"), true, 0},
		{"grant, row's leg not in the census", granted, ledgerCensusOf(map[rowKey]ledgerRow{{"rig", "gc-r"}: {}}), false, 0},
		{"grant, no recorded incarnation (N60)", unmarked, ledgerCensusOf(map[rowKey]ledgerRow{ledgerRowA: {Incarnation: 4}}), false, 0},
		{"grant, no recorded incarnation, row closed", unmarked, ledgerCensusOf(nil, "sessions"), true, 0},
		{"grant, issued with the row visible", issued, ledgerCensusOf(map[rowKey]ledgerRow{ledgerRowA: {Incarnation: 9}}), false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clears, refund := tt.e.clearVerdict(tt.c, now)
			if clears != tt.clears || refund != tt.refund {
				t.Fatalf("clearVerdict = (%v, %d), want (%v, %d)", clears, refund, tt.clears, tt.refund)
			}
		})
	}
}

// Kills (R18): a start counted twice or not at all while the census read,
// the cache apply and the commit race in either order; a late marker clearing
// early; a duplicate commit or clear landing twice.
func TestLedgerMarkerRaceCountsStartOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stale := ledgerCensusOf(map[rowKey]ledgerRow{ledgerRowA: {Incarnation: 4}})
		fresh := ledgerCensusOf(map[rowKey]ledgerRow{ledgerRowA: {Incarnation: 5, StartLease: true}})
		for _, cacheFirst := range []bool{false, true} {
			l := newIntentLedger(time.Now)
			l.Reserve(ledgerGrant("g", ledgerRowA, 1))
			l.Issue("g", ledgerRowA)
			// The pass sees whichever order the race produced.
			seen := stale
			if cacheFirst {
				seen = fresh // PreWake's write is in the cache before Commit returns
			}
			assertPass := func(step string, c ledgerCensus, wantClear bool) {
				t.Helper()
				view := l.View()
				if got := cityInFlight(view, c, nil); got != 1 {
					t.Fatalf("cacheFirst=%v %s: in flight %d, want 1", cacheFirst, step, got)
				}
				e := view[0]
				clears, _ := e.clearVerdict(c, time.Now())
				if clears != wantClear {
					t.Fatalf("cacheFirst=%v %s: clear=%v, want %v", cacheFirst, step, clears, wantClear)
				}
				if clears && !l.Transition(e.ID, e.State, ledgerCleared, nil) {
					t.Fatalf("cacheFirst=%v %s: clear lost", cacheFirst, step)
				}
			}
			assertPass("issued", seen, false)
			if !l.Commit("g", ledgerMarker{Incarnation: 5}) {
				t.Fatal("Commit lost")
			}
			if l.Commit("g", ledgerMarker{Incarnation: 7}) || l.Fail("g", false, ledgerMarker{}) {
				t.Fatal("a duplicate settle won")
			}
			if !cacheFirst {
				advance(time.Second)
				assertPass("committed, cache lagging", stale, false)
			}
			assertPass("committed, marker visible", fresh, true)
			if got := cityInFlight(l.View(), fresh, nil); got != 1 {
				t.Fatalf("cacheFirst=%v after clear: in flight %d, want 1 (the row's lease)", cacheFirst, got)
			}
			if l.Transition("g", ledgerCommitted, ledgerCleared, nil) || l.Commit("g", ledgerMarker{Incarnation: 5}) {
				t.Fatalf("cacheFirst=%v: a duplicate clear or late commit won", cacheFirst)
			}
		}
	})
}

// Kills: a refund on a provider-called failure (#45); no refund on a
// prepare failure (START-013); a failure that wrote nothing waiting for a
// marker that never comes; a refund for a prepaid grant.
func TestLedgerNoWriteFailuresClearImmediatelyWithRefund(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newIntentLedger(time.Now)
		mk := func(e ledgerEntry, providerCalled, wroteRow bool) {
			l.Reserve(e)
			l.Transition(e.ID, ledgerReserved, ledgerIssued, nil)
			if providerCalled {
				l.MarkProviderCalled(e.ID)
			}
			l.Fail(e.ID, wroteRow, ledgerMarker{Incarnation: 5})
		}
		mk(ledgerGrant("prepare-failure", rowKey{"sessions", "a"}, 1), false, false)
		mk(ledgerGrant("prepaid-prepare-failure", rowKey{"sessions", "b"}, 0), false, false)
		mk(ledgerGrant("start-failed", rowKey{"sessions", "c"}, 1), true, true)
		mk(ledgerGrant("finalizer-after-start", rowKey{"sessions", "d"}, 1), true, false)
		mk(ledgerCreate("create-failed", "tok"), false, false)
		// The census shows none of their markers.
		c := ledgerCensusOf(map[rowKey]ledgerRow{{"sessions", "a"}: {}, {"sessions", "b"}: {}, {"sessions", "c"}: {}, {"sessions", "d"}: {}})
		want := map[string]struct {
			clears bool
			refund int
		}{
			"prepare-failure":         {true, 1},
			"prepaid-prepare-failure": {true, 0},
			"start-failed":            {false, 0}, // wrote a row: waits for its marker
			"finalizer-after-start":   {true, 0},
			"create-failed":           {true, 1},
		}
		for _, e := range l.View() {
			clears, refund := e.clearVerdict(c, time.Now())
			if w := want[e.ID]; clears != w.clears || refund != w.refund {
				t.Errorf("%s: clearVerdict = (%v, %d), want (%v, %d)", e.ID, clears, refund, w.clears, w.refund)
			}
		}
		// MarkProviderCalled only marks an issued grant.
		l.Reserve(ledgerGrant("reserved", ledgerRowB, 1))
		l.MarkProviderCalled("reserved")
		if e, _ := ledgerEntryOf(l, "reserved"); e.ProviderCalled {
			t.Fatal("MarkProviderCalled marked a reserved grant")
		}
	})
}

// Kills (R24): a landed effect whose marker never reaches the census stuck
// forever; repair before the bound; repair rereading every pass after a read
// installed the row; a refund for a provider-called grant proven absent; a
// second outcome overwriting a clearing one.
func TestLedgerLagRepairAfterBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newIntentLedger(time.Now)
		l.Reserve(ledgerCreate("c", "tok"))
		l.Transition("c", ledgerReserved, ledgerIssued, nil)
		l.Commit("c", ledgerMarker{RowID: "gc-new"})
		l.Reserve(ledgerGrant("g", ledgerRowA, 1))
		l.Issue("g", ledgerRowA)
		l.MarkProviderCalled("g")
		l.Commit("g", ledgerMarker{Incarnation: 5})
		lagged := ledgerCensusOf(map[rowKey]ledgerRow{ledgerRowA: {Incarnation: 4}})
		lagging := func() map[string]bool {
			out := make(map[string]bool)
			for _, e := range l.View() {
				if e.lagging(lagged, time.Now()) {
					out[e.ID] = true
				}
			}
			return out
		}

		advance(ledgerCacheLagBound - time.Millisecond)
		if got := lagging(); len(got) != 0 {
			t.Fatalf("lagging before the bound: %v", got)
		}
		advance(time.Millisecond)
		if got := lagging(); !got["c"] || !got["g"] {
			t.Fatalf("lagging at the bound = %v, want c and g", got)
		}
		if e, _ := ledgerEntryOf(l, "c"); e.Key.ID != "gc-new" {
			t.Fatalf("committed create key = %+v, want the new row", e.Key)
		}

		// The read installed the create's row: no rereads for another bound.
		if !l.ResolveLag("c", lagInstalled) {
			t.Fatal("ResolveLag(c, installed) lost")
		}
		if got := lagging(); got["c"] {
			t.Fatal("a create whose row the read installed is lagging again at once")
		}
		if e, _ := ledgerEntryOf(l, "c"); func() bool { ok, _ := e.clearVerdict(lagged, time.Now()); return ok }() {
			t.Fatal("an installed create cleared before its marker reached the census")
		}
		advance(ledgerCacheLagBound)
		if got := lagging(); !got["c"] {
			t.Fatal("a create still missing a bound after the read is not lagging")
		}

		// The reads found nothing: both clear at the next pass.
		l.ResolveLag("c", lagNotFound)
		l.ResolveLag("g", lagNotFound)
		if l.ResolveLag("c", lagInstalled) || l.ResolveLag("g", lagClosed) {
			t.Fatal("a second outcome overwrote a clearing one")
		}
		for _, e := range l.View() {
			clears, refund := e.clearVerdict(lagged, time.Now())
			wantRefund := map[string]int{"c": 1, "g": 0}[e.ID]
			if !clears || refund != wantRefund || e.lagging(lagged, time.Now()) {
				t.Errorf("%s not found: clear=%v refund=%d lagging=%v, want clear, refund %d",
					e.ID, clears, refund, e.lagging(lagged, time.Now()), wantRefund)
			}
		}

		l.Reserve(ledgerGrant("r", ledgerRowB, 1))
		if l.ResolveLag("r", lagNotFound) || l.ResolveLag("missing", lagNotFound) {
			t.Fatal("ResolveLag accepted an entry that has not landed")
		}
		l.Issue("r", ledgerRowB)
		l.Commit("r", ledgerMarker{Incarnation: 2})
		if l.ResolveLag("r", 0) || l.ResolveLag("r", lagNotFound+1) {
			t.Fatal("ResolveLag accepted an unknown outcome")
		}
	})
}

// Kills (C5.15 as amended 2026-10-03 per P3-4 review): a closed row read as
// unwritten (a create refunded though it wrote its row); a closed, unmarked
// or missing row leaving the entry stuck; an installed read clearing before
// the census shows the marker; a provider-called grant refunded on any
// outcome; a prepare-only grant charged.
func TestLedgerLagRepairOutcomesClearAndRefund(t *testing.T) {
	lagged := ledgerCensusOf(map[rowKey]ledgerRow{ledgerRowA: {Incarnation: 4}})
	create := ledgerEntry{
		ID: "c", Kind: kindCreate, Key: rowKey{Leg: "sessions", ID: "gc-new"}, Cost: 1, State: ledgerCommitted,
		WroteRow: true, Marker: ledgerMarker{RowID: "gc-new", InstanceToken: "tok"},
	}
	called := ledgerEntry{
		ID: "g", Kind: kindGrant, Key: ledgerRowA, Cost: 1, State: ledgerCommitted, WroteRow: true,
		ProviderCalled: true, Marker: ledgerMarker{Incarnation: 5},
	}
	uncalled := called
	uncalled.ProviderCalled = false
	type verdict struct {
		clears bool
		refund int
	}
	for _, tt := range []struct {
		name string
		e    ledgerEntry
		want map[lagOutcome]verdict
	}{
		{"create", create, map[lagOutcome]verdict{
			lagInstalled: {false, 0}, lagNoMarker: {true, 1}, lagClosed: {true, 0}, lagNotFound: {true, 1},
		}},
		{"grant, provider called", called, map[lagOutcome]verdict{
			lagInstalled: {false, 0}, lagNoMarker: {true, 0}, lagClosed: {true, 0}, lagNotFound: {true, 0},
		}},
		{"grant, provider never called", uncalled, map[lagOutcome]verdict{
			lagInstalled: {false, 0}, lagNoMarker: {true, 1}, lagClosed: {true, 1}, lagNotFound: {true, 1},
		}},
	} {
		for o, w := range tt.want {
			synctest.Test(t, func(t *testing.T) {
				l := newIntentLedger(time.Now)
				e := tt.e
				e.State = ledgerReserved
				l.Reserve(e)
				l.Transition(e.ID, ledgerReserved, ledgerIssued, func(x *ledgerEntry) { x.ProviderCalled = tt.e.ProviderCalled })
				l.Commit(e.ID, tt.e.Marker)
				advance(ledgerCacheLagBound)
				if !l.ResolveLag(e.ID, o) {
					t.Fatalf("%s: ResolveLag(%d) lost", tt.name, o)
				}
				got, _ := ledgerEntryOf(l, e.ID)
				clears, refund := got.clearVerdict(lagged, time.Now())
				if clears != w.clears || refund != w.refund {
					t.Fatalf("%s, outcome %d: clearVerdict = (%v, %d), want (%v, %d)", tt.name, o, clears, refund, w.clears, w.refund)
				}
				if got.lagging(lagged, time.Now()) {
					t.Fatalf("%s, outcome %d: lagging right after the read", tt.name, o)
				}
			})
		}
	}
}

// Kills: a grant row read below the incarnation PreWake would have written
// counted as found, so lag repair rereads it every bound forever and its leg
// stays flagged (review scenario 1); a create whose row closed before the
// census showed it read as found or as unwritten (review scenario 2); a
// marked row not installed; a grant with no recorded incarnation read as
// marked.
func TestLedgerLagOutcomeOfLiveRead(t *testing.T) {
	create := ledgerEntry{Kind: kindCreate, Marker: ledgerMarker{RowID: "gc-new", InstanceToken: "tok"}}
	ambiguous := ledgerEntry{Kind: kindCreate, Marker: ledgerMarker{InstanceToken: "tok"}}
	grant := ledgerEntry{Kind: kindGrant, Key: ledgerRowA, Marker: ledgerMarker{Incarnation: 5}}
	unmarked := ledgerEntry{Kind: kindGrant, Key: ledgerRowA}
	newRow := rowKey{Leg: "sessions", ID: "gc-new"}
	for _, tt := range []struct {
		name        string
		e           ledgerEntry
		k           rowKey
		r           ledgerRow
		found, open bool
		want        lagOutcome
	}{
		{"create, row by ID", create, newRow, ledgerRow{InstanceToken: "tok"}, true, true, lagInstalled},
		{"create, row by token only", ambiguous, rowKey{Leg: "sessions", ID: "gc-x"}, ledgerRow{InstanceToken: "tok"}, true, true, lagInstalled},
		{"create, row closed before the census saw it", create, newRow, ledgerRow{InstanceToken: "tok"}, true, false, lagClosed},
		{"create, nothing", ambiguous, rowKey{}, ledgerRow{}, false, false, lagNotFound},
		{"grant, row at the marker", grant, ledgerRowA, ledgerRow{Incarnation: 5}, true, true, lagInstalled},
		{"grant, row past the marker", grant, ledgerRowA, ledgerRow{Incarnation: 6}, true, true, lagInstalled},
		{"grant, row below the marker", grant, ledgerRowA, ledgerRow{Incarnation: 4}, true, true, lagNoMarker},
		{"grant, row closed", grant, ledgerRowA, ledgerRow{Incarnation: 5}, true, false, lagClosed},
		{"grant, row gone", grant, ledgerRowA, ledgerRow{}, false, false, lagNotFound},
		{"grant, no recorded incarnation", unmarked, ledgerRowA, ledgerRow{Incarnation: 4}, true, true, lagNoMarker},
	} {
		if got := tt.e.lagOutcomeOf(tt.k, tt.r, tt.found, tt.open); got != tt.want {
			t.Errorf("%s: lagOutcomeOf = %d, want %d", tt.name, got, tt.want)
		}
	}

	// Both review scenarios, end to end: the read resolves the entry at the
	// next pass instead of rereading every bound.
	synctest.Test(t, func(t *testing.T) {
		l := newIntentLedger(time.Now)
		l.Reserve(ledgerGrant("g", ledgerRowA, 1))
		l.Issue("g", ledgerRowA)
		l.MarkProviderCalled("g")
		l.Commit("g", ledgerMarker{Incarnation: 5}) // ambiguous: PreWake never landed
		l.Reserve(ledgerCreate("c", "tok"))
		l.Transition("c", ledgerReserved, ledgerIssued, nil)
		l.Commit("c", ledgerMarker{RowID: "gc-new"})
		// The census holds A below the marker and never saw gc-new, which
		// closed before its open row was ever delivered.
		c := ledgerCensusOf(map[rowKey]ledgerRow{ledgerRowA: {Incarnation: 4}})
		live := map[string]struct {
			k           rowKey
			r           ledgerRow
			found, open bool
		}{
			"g": {ledgerRowA, ledgerRow{Incarnation: 4}, true, true},
			"c": {newRow, ledgerRow{InstanceToken: "tok"}, true, false},
		}
		want := map[string]int{"g": 0, "c": 0}
		advance(ledgerCacheLagBound)
		for _, e := range l.View() {
			if !e.lagging(c, time.Now()) {
				t.Fatalf("%s not lagging at the bound", e.ID)
			}
			rd := live[e.ID]
			l.ResolveLag(e.ID, e.lagOutcomeOf(rd.k, rd.r, rd.found, rd.open))
		}
		for _, e := range l.View() {
			clears, refund := e.clearVerdict(c, time.Now())
			if !clears || refund != want[e.ID] {
				t.Errorf("%s after the read: clearVerdict = (%v, %d), want (true, %d)", e.ID, clears, refund, want[e.ID])
			}
		}
	})
}

// Kills (N39): a proof of absence beating the marker. An ambiguous create's
// read found no row; its write lands after the read and reaches the census
// before the pass. The entry must clear as written, with no refund: the
// token was spent on a row that exists.
func TestLedgerMarkerBeatsLateAbsenceProof(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newIntentLedger(time.Now)
		l.Reserve(ledgerCreate("c", "tok"))
		l.Transition("c", ledgerReserved, ledgerIssued, nil)
		l.Commit("c", ledgerMarker{}) // ambiguous: token only
		advance(ledgerCacheLagBound)
		if !l.ResolveLag("c", lagNotFound) {
			t.Fatal("ResolveLag lost")
		}
		late := ledgerCensusOf(map[rowKey]ledgerRow{{"sessions", "gc-late"}: {InstanceToken: "tok", PendingCreate: true}})
		e, _ := ledgerEntryOf(l, "c")
		if clears, refund := e.clearVerdict(late, time.Now()); !clears || refund != 0 {
			t.Fatalf("late write visible: clearVerdict = (%v, %d), want (true, 0)", clears, refund)
		}
	})
}

// vetoesOf returns l's veto entries for k.
func vetoesOf(l *intentLedger, k rowKey) []ledgerEntry {
	var got []ledgerEntry
	for _, e := range l.View() {
		if e.Kind == kindVeto && e.Key == k {
			got = append(got, e)
		}
	}
	return got
}

// Kills: linear or uncapped veto backoff; a backoff that never resets after a
// start; a requested Until shortened by the backoff; two vetoes for one key;
// a veto that never clears or clears early.
func TestLedgerVetoBackoffDoublesCapsAtFiveMinutesResetsOnIssue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newIntentLedger(time.Now)
		veto := func(k rowKey, until time.Time) ledgerEntry {
			t.Helper()
			l.Veto(k, until, "fresh-read")
			got := vetoesOf(l, k)
			if len(got) != 1 {
				t.Fatalf("%d vetoes for %v, want 1", len(got), k)
			}
			return got[0]
		}
		// Each veto comes after the last one expired: a consecutive refusal.
		for i, want := range []time.Duration{10, 20, 40, 80, 160, 300, 300, 300} {
			e := veto(ledgerRowA, time.Time{})
			if got := time.Until(e.Until); got != want*time.Second || e.Consecutive != i+1 {
				t.Fatalf("veto %d: backoff %v consecutive %d, want %v and %d", i+1, got, e.Consecutive, want*time.Second, i+1)
			}
			advance(time.Until(e.Until))
		}
		if e := veto(ledgerRowB, time.Time{}); time.Until(e.Until) != 10*time.Second {
			t.Fatalf("another key's first veto backs off %v, want 10s", time.Until(e.Until))
		}
		if e := veto(ledgerRowB, time.Now().Add(time.Minute)); time.Until(e.Until) != time.Minute {
			t.Fatalf("requeueAfter floor lost: backoff %v, want 1m", time.Until(e.Until))
		}

		// The veto clears at Until, not before.
		e := veto(ledgerRowA, time.Time{})
		if clears, _ := e.clearVerdict(ledgerCensus{}, e.Until.Add(-time.Nanosecond)); clears {
			t.Fatal("veto cleared before Until")
		}
		if clears, refund := e.clearVerdict(ledgerCensus{}, e.Until); !clears || refund != 0 {
			t.Fatalf("veto at Until: clear=%v refund=%d, want clear, no refund", clears, refund)
		}
		advance(time.Until(e.Until))

		// An issued start resets the backoff.
		l.Reserve(ledgerGrant("g", ledgerRowA, 1))
		l.Issue("g", ledgerRowA)
		if e := veto(ledgerRowA, time.Time{}); time.Until(e.Until) != 10*time.Second || e.Consecutive != 1 {
			t.Fatalf("after issue: backoff %v consecutive %d, want 10s and 1", time.Until(e.Until), e.Consecutive)
		}
	})
}

// Kills: a held grant's session key, re-deciding on every event, escalating
// its own backoff to minutes within one refusal. Four vetoes 100ms apart on
// one held grant are one refusal: the count stays 1 and the live veto
// extends to the later Until, never shortens.
func TestLedgerVetoWhileLiveDoesNotEscalate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newIntentLedger(time.Now)
		l.Reserve(ledgerGrant("g", ledgerRowA, 1)) // held, never issued
		start := time.Now()
		for i := 0; i < 4; i++ {
			l.Veto(ledgerRowA, time.Time{}, "fresh-read")
			got := vetoesOf(l, ledgerRowA)
			if len(got) != 1 || got[0].Consecutive != 1 || !got[0].Until.Equal(time.Now().Add(ledgerVetoBase)) {
				t.Fatalf("veto %d at +%v: %+v, want one veto, consecutive 1, until now+10s", i+1, time.Since(start), got)
			}
			advance(100 * time.Millisecond)
		}
		// A shorter request does not shorten the live veto.
		l.Veto(ledgerRowA, time.Now().Add(time.Minute), "commit-rule")
		l.Veto(ledgerRowA, time.Time{}, "fresh-read")
		if got := vetoesOf(l, ledgerRowA)[0]; time.Until(got.Until) != time.Minute || got.Consecutive != 1 {
			t.Fatalf("re-veto shortened or escalated: %+v, want until now+1m, consecutive 1", got)
		}
		// Once it expires, the next veto is a consecutive refusal.
		advance(time.Until(vetoesOf(l, ledgerRowA)[0].Until))
		l.Veto(ledgerRowA, time.Time{}, "fresh-read")
		if got := vetoesOf(l, ledgerRowA); len(got) != 1 || got[0].Consecutive != 2 || time.Until(got[0].Until) != 2*ledgerVetoBase {
			t.Fatalf("veto after expiry: %+v, want one veto, consecutive 2, 20s", got)
		}
	})
}

// Kills (N48): a veto on one key removing or extending another key's veto,
// or sharing its backoff count.
func TestLedgerVetoesAreIndependentPerKey(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newIntentLedger(time.Now)
		l.Veto(ledgerRowA, time.Time{}, "fresh-read")
		advance(time.Second)
		l.Veto(ledgerRowB, time.Time{}, "commit-rule")
		a, b := vetoesOf(l, ledgerRowA), vetoesOf(l, ledgerRowB)
		if len(a) != 1 || len(b) != 1 {
			t.Fatalf("vetoes: A %d, B %d, want one each", len(a), len(b))
		}
		if time.Until(a[0].Until) != 9*time.Second || a[0].Reason != "fresh-read" || a[0].Consecutive != 1 {
			t.Fatalf("B's veto moved A's: %+v", a[0])
		}
		if time.Until(b[0].Until) != 10*time.Second || b[0].Consecutive != 1 {
			t.Fatalf("B's veto shares A's backoff: %+v", b[0])
		}
	})
}

// Kills: veto backoff memory growing with every row ever vetoed; forgetting
// an open row's backoff, or a row on a leg the census did not hold.
func TestLedgerForgetClosedBoundsVetoBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newIntentLedger(time.Now)
		rig := rowKey{Leg: "rig", ID: "gc-r"}
		for _, k := range []rowKey{ledgerRowA, ledgerRowB, rig} {
			l.Veto(k, time.Time{}, "fresh-read")
		}
		// B closed; the rig leg was not read this pass.
		l.ForgetClosed(ledgerCensusOf(map[rowKey]ledgerRow{ledgerRowA: {}}))
		advance(ledgerVetoBase) // every veto expired
		for k, want := range map[rowKey]time.Duration{ledgerRowA: 20 * time.Second, ledgerRowB: 10 * time.Second, rig: 20 * time.Second} {
			l.Veto(k, time.Time{}, "fresh-read")
			for _, e := range vetoesOf(l, k) {
				if time.Until(e.Until) != want {
					t.Errorf("%v: second veto backs off %v, want %v", k, time.Until(e.Until), want)
				}
			}
		}
	})
}

// Kills: a reserved grant or create held forever, so a plan no key or
// executor picks up keeps its token, its city slot and its endpoint's probe;
// a release before the TTL; TTL release reaching an issued entry.
func TestLedgerReserveTTL(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newIntentLedger(time.Now)
		l.Reserve(ledgerGrant("grant", ledgerRowA, 1))
		l.Reserve(ledgerGrant("issued", ledgerRowB, 1))
		l.Issue("issued", ledgerRowB)
		create := ledgerCreate("create", "tok")
		create.Endpoint = "provider:half-open"
		l.Reserve(create)
		expired := func() map[string]bool {
			out := make(map[string]bool)
			for _, e := range l.View() {
				if e.reserveExpired(time.Now()) {
					out[e.ID] = true
				}
			}
			return out
		}
		advance(ledgerReserveTTL - time.Millisecond)
		if got := expired(); len(got) != 0 {
			t.Fatalf("expired before the TTL: %v", got)
		}
		if got := endpointOutstanding(l.View())["provider:half-open"]; got != 1 {
			t.Fatalf("reserved create holds %d probe slots, want 1", got)
		}
		advance(time.Millisecond)
		if got := expired(); len(got) != 2 || !got["grant"] || !got["create"] {
			t.Fatalf("expired at the TTL = %v, want the reserved grant and create", got)
		}
		refunds := 0
		for id := range expired() {
			refund, ok := l.Release(id)
			if !ok {
				t.Fatalf("Release(%s) lost", id)
			}
			refunds += refund
		}
		if refunds != 2 {
			t.Fatalf("TTL releases refunded %d, want 2", refunds)
		}
		view := l.View()
		if got := cityInFlight(view, ledgerCensus{}, nil); got != 1 {
			t.Fatalf("in flight after TTL release = %d, want 1 (the issued grant)", got)
		}
		if got := endpointOutstanding(view)["provider:half-open"]; got != 0 {
			t.Fatalf("released create still holds the probe: %d outstanding", got)
		}
	})
}

// Kills: View exposing the ledger's own entries, or an unstable order.
func TestLedgerViewIsSortedCopy(t *testing.T) {
	l := newIntentLedger(time.Now)
	l.Reserve(ledgerGrant("b", ledgerRowB, 1))
	l.Reserve(ledgerGrant("a", ledgerRowA, 1))
	view := l.View()
	if len(view) != 2 || view[0].ID != "a" || view[1].ID != "b" {
		t.Fatalf("View order = %v", view)
	}
	view[0].State = ledgerCommitted
	if e, _ := ledgerEntryOf(l, "a"); e.State != ledgerReserved {
		t.Fatal("editing the view edited the ledger")
	}
}

var (
	createIdentityA = createIdentity{Template: "worker", QualifiedInstance: "worker-1", Slot: 1}
	createIdentityB = createIdentity{Template: "worker", QualifiedInstance: "worker-2", Slot: 2}
)

// failCreate reserves and issues create id under rev, then fails it for c.
func failCreate(t *testing.T, l *intentLedger, id, rev string, c createIdentity) createVeto {
	t.Helper()
	e := ledgerCreate(id, "tok-"+id)
	e.ConfigRev = rev
	if !l.Reserve(e) {
		t.Fatalf("reserve %s refused", id)
	}
	if token, ok := l.IssueCreate(id); !ok || token != "tok-"+id {
		t.Fatalf("issue %s = (%q, %v)", id, token, ok)
	}
	if !l.FailCreate(id, c, "fence") {
		t.Fatalf("fail %s refused", id)
	}
	entries, vetoes := l.Snapshot()
	for _, e := range entries {
		if e.ID == id && (e.State != ledgerFailed || e.WroteRow) {
			t.Fatalf("entry %+v, want failed without a row", e)
		}
	}
	return vetoes[c.key()]
}

// Kills (AM-N8): create vetoes kept with the row vetoes, so a row's issued
// start or close resets a create identity's backoff; a create veto shown to
// the pass as a ledger entry; one identity's veto moving another's.
func TestLedgerCreateVetoIsItsOwnKeySpace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newIntentLedger(time.Now)
		failCreate(t, l, "c1", "r", createIdentityA)
		advance(ledgerVetoBase)
		before := failCreate(t, l, "c2", "r", createIdentityA)
		l.Reserve(ledgerGrant("g", ledgerRowA, 1))
		l.Issue("g", ledgerRowA)
		l.ForgetClosed(ledgerCensusOf(nil, "sessions"))
		_, vetoes := l.Snapshot()
		if vetoes[createIdentityA.key()] != before || before.Consecutive != 2 {
			t.Fatalf("create veto %+v after Issue and ForgetClosed, want %+v (consecutive 2)", vetoes[createIdentityA.key()], before)
		}
		if _, ok := vetoes[createIdentityB.key()]; ok || len(vetoesOf(l, ledgerRowA)) != 0 {
			t.Fatal("a create veto leaked into another identity or into the row vetoes")
		}
		for _, e := range l.View() {
			if e.Kind == kindVeto {
				t.Fatalf("create veto shown as ledger entry %+v", e)
			}
		}
	})
}

// Kills: a create veto that escalates within one refusal, survives a
// commit or a ConfigRev change, or outlives its identity in config.
func TestLedgerCreateVetoResetsOnCommitAndConfigRevAndPrunes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newIntentLedger(time.Now)
		failCreate(t, l, "c1", "r1", createIdentityA)
		advance(time.Second)
		if v := failCreate(t, l, "c2", "r1", createIdentityA); v.Consecutive != 1 || time.Until(v.Until) != ledgerVetoBase {
			t.Fatalf("refusal while live: %+v, want consecutive 1 extended to now+10s", v)
		}
		advance(ledgerVetoBase)
		if v := failCreate(t, l, "c3", "r1", createIdentityA); v.Consecutive != 2 {
			t.Fatalf("refusal after expiry: %+v, want consecutive 2", v)
		}
		advance(2 * ledgerVetoBase)
		if v := failCreate(t, l, "c4", "r2", createIdentityA); v.Consecutive != 1 || v.ConfigRev != "r2" || time.Until(v.Until) != ledgerVetoBase {
			t.Fatalf("refusal under a new ConfigRev: %+v, want a fresh count", v)
		}

		e := ledgerCreate("ok", "tok-ok")
		l.Reserve(e)
		l.IssueCreate("ok")
		if !l.CommitCreate("ok", createIdentityA, ledgerMarker{RowID: "gc-1"}) {
			t.Fatal("commit refused")
		}
		if _, vetoes := l.Snapshot(); len(vetoes) != 0 {
			t.Fatalf("vetoes after a commit = %+v, want none", vetoes)
		}

		failCreate(t, l, "a", "r2", createIdentityA)
		failCreate(t, l, "b", "r2", createIdentityB)
		l.PruneCreateVetoes("r2", func(c createIdentity) bool { return c != createIdentityB })
		if _, vetoes := l.Snapshot(); len(vetoes) != 1 || vetoes[createIdentityA.key()].Consecutive != 1 {
			t.Fatalf("after pruning B: %+v, want only A", vetoes)
		}
		l.PruneCreateVetoes("r3", func(createIdentity) bool { return true })
		if _, vetoes := l.Snapshot(); len(vetoes) != 0 {
			t.Fatalf("after a ConfigRev change: %+v, want none", vetoes)
		}
	})
}

// Kills: the create executor issuing or settling a grant, or vetoing on an
// entry it never issued.
func TestLedgerCreateMovesRefuseOtherKindsAndStates(t *testing.T) {
	l := newIntentLedger(time.Now)
	l.Reserve(ledgerGrant("g", ledgerRowA, 1))
	if _, ok := l.IssueCreate("g"); ok {
		t.Fatal("IssueCreate issued a grant")
	}
	l.Issue("g", ledgerRowA)
	if l.FailCreate("g", createIdentityA, "fence") || l.CommitCreate("g", createIdentityA, ledgerMarker{}) {
		t.Fatal("a create move settled a grant")
	}
	l.Reserve(ledgerCreate("c", "tok"))
	if l.FailCreate("c", createIdentityA, "fence") {
		t.Fatal("FailCreate settled a reserved create")
	}
	if _, vetoes := l.Snapshot(); len(vetoes) != 0 {
		t.Fatalf("vetoes = %+v, want none from refused moves", vetoes)
	}
}
