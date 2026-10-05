package main

import (
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// Ledger tests run the ledger on the synctest bubble's clock (time.Now) and
// move time with advance, so the TTL and the hard bound are exact.
// The pure verdicts take the pass's time explicitly.

var (
	ledgerRowA = rowKey{Leg: "sessions", ID: "gc-a"}
	ledgerRowB = rowKey{Leg: "sessions", ID: "gc-b"}
)

func ledgerGrant(id string, k rowKey) ledgerEntry {
	return ledgerEntry{ID: id, Kind: kindGrant, Key: k, Endpoint: "provider:p", ReservedAt: time.Now(), State: ledgerReserved}
}

func ledgerCreate(id, token string) ledgerEntry {
	return ledgerEntry{
		ID: id, Kind: kindCreate, Key: rowKey{Leg: "sessions"}, Endpoint: "provider:p",
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

// ledgerCensusOf builds a ledgerCensus that holds every leg of its rows plus
// legs, as complete exact legs: no read start.
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
		l.Reserve(ledgerGrant("g", ledgerRowA))
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
		l.Reserve(ledgerGrant("g", ledgerRowA))
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
// releasing a landed effect; a refund on a lost release; a refund for a
// create (C5.7: creates debit nothing); issuing a grant that was released or
// that belongs to another key.
func TestLedgerOnlyReservedEntriesRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newIntentLedger(time.Now)
		for _, id := range []string{"issued", "committed", "failed"} {
			l.Reserve(ledgerGrant(id, rowKey{Leg: "sessions", ID: id}))
			l.Issue(id, rowKey{Leg: "sessions", ID: id})
		}
		l.Commit("committed", ledgerMarker{Incarnation: 2})
		l.Fail("failed", true, ledgerMarker{Incarnation: 2})
		for _, id := range []string{"issued", "committed", "failed"} {
			if refund, ok := l.Release(id); ok || refund != 0 {
				t.Errorf("Release(%s) = (%d, %v), want (0, false)", id, refund, ok)
			}
			if _, present := ledgerEntryOf(l, id); !present {
				t.Errorf("Release(%s) removed the entry", id)
			}
		}

		l.Reserve(ledgerGrant("g", ledgerRowA))
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
		if refund, ok := l.Release("c"); !ok || refund != 0 {
			t.Fatalf("Release(reserved create) = (%d, %v), want (0, true)", refund, ok)
		}
	})
}

// Kills: Reserve accepting another kind, an empty ID or a duplicate ID (two
// entries for one grant would debit twice and clear once); a create with no
// instance token, which an ambiguous outcome could never clear by marker
// (N60), or no sessions leg, which no read could ever clear by absence.
func TestLedgerReserveRefusesDuplicateEmptyAndOtherKinds(t *testing.T) {
	l := newIntentLedger(time.Now)
	if !l.Reserve(ledgerGrant("g", ledgerRowA)) {
		t.Fatal("Reserve refused a fresh grant")
	}
	for name, e := range map[string]ledgerEntry{
		"duplicate":                        ledgerGrant("g", ledgerRowB),
		"empty ID":                         ledgerGrant("", ledgerRowB),
		"another kind":                     {ID: "v", Kind: kindGrant + 1, Key: ledgerRowB},
		"create without an instance token": {ID: "c", Kind: kindCreate, Key: rowKey{Leg: "sessions"}}, // N60
		"create without its sessions leg":  {ID: "c", Kind: kindCreate, Marker: ledgerMarker{InstanceToken: "tok"}},
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
// clearing on a later incarnation or a closed row; a grant with no recorded
// incarnation clearing on any open row (N60).
func TestLedgerClearsCreateOnRowMarkerGrantOnIncarnation(t *testing.T) {
	now := time.Unix(1_000, 0)
	created := ledgerEntry{
		ID: "c", Kind: kindCreate, Key: rowKey{Leg: "sessions", ID: "gc-new"}, State: ledgerCommitted, SettledAt: now,
		WroteRow: true, Marker: ledgerMarker{RowID: "gc-new", InstanceToken: "tok"},
	}
	ambiguous := created
	ambiguous.Key.ID, ambiguous.Marker.RowID, ambiguous.Ambiguous = "", "", true
	granted := ledgerEntry{
		ID: "g", Kind: kindGrant, Key: ledgerRowA, State: ledgerCommitted, WroteRow: true, SettledAt: now,
		Marker: ledgerMarker{Incarnation: 5},
	}
	issued := granted
	issued.State, issued.WroteRow = ledgerIssued, false
	unmarked := granted
	unmarked.Marker.Incarnation = 0
	tests := []struct {
		name string
		e    ledgerEntry
		c    ledgerCensus
		want ledgerClear
	}{
		{"create, row not yet visible", created, ledgerCensusOf(map[rowKey]ledgerRow{ledgerRowA: {}}), clearKeep},
		{"create, row ID on its leg", created, ledgerCensusOf(map[rowKey]ledgerRow{{"sessions", "gc-new"}: {}}), clearWritten},
		{"create, row ID on another leg", created, ledgerCensusOf(map[rowKey]ledgerRow{{"rig", "gc-new"}: {}}), clearWritten},
		{"create, token only (ambiguous outcome)", ambiguous, ledgerCensusOf(map[rowKey]ledgerRow{{"sessions", "gc-x"}: {InstanceToken: "tok"}}), clearWritten},
		{"create, other token", ambiguous, ledgerCensusOf(map[rowKey]ledgerRow{{"sessions", "gc-x"}: {InstanceToken: "other"}}), clearKeep},
		{"grant, census on the old incarnation", granted, ledgerCensusOf(map[rowKey]ledgerRow{ledgerRowA: {Incarnation: 4}}), clearKeep},
		{"grant, census at the PreWake generation", granted, ledgerCensusOf(map[rowKey]ledgerRow{ledgerRowA: {Incarnation: 5}}), clearWritten},
		{"grant, census at a later generation", granted, ledgerCensusOf(map[rowKey]ledgerRow{ledgerRowA: {Incarnation: 9}}), clearWritten},
		{"grant, row closed (gone from its read leg)", granted, ledgerCensusOf(nil, "sessions"), clearWritten},
		{"grant, row's leg not in the census", granted, ledgerCensusOf(map[rowKey]ledgerRow{{"rig", "gc-r"}: {}}), clearKeep},
		{"grant, no recorded incarnation (N60)", unmarked, ledgerCensusOf(map[rowKey]ledgerRow{ledgerRowA: {Incarnation: 4}}), clearKeep},
		{"grant, no recorded incarnation, row closed", unmarked, ledgerCensusOf(nil, "sessions"), clearWritten},
		{"grant, issued with the row visible", issued, ledgerCensusOf(map[rowKey]ledgerRow{ledgerRowA: {Incarnation: 9}}), clearKeep},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.e.clearVerdict(tt.c, now); got != tt.want {
				t.Fatalf("clearVerdict = %d, want %d", got, tt.want)
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
			l.Reserve(ledgerGrant("g", ledgerRowA))
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
				clears := e.clearVerdict(c, time.Now()) != clearKeep
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

// Kills: a failure that wrote nothing waiting for a marker that never comes;
// a failure that wrote a row clearing before its marker; a refund for an
// issued grant (C5.7: once issued, its token is spent whatever the outcome,
// so the only refund, Release, loses).
func TestLedgerNoWriteFailuresClearImmediately(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newIntentLedger(time.Now)
		mk := func(e ledgerEntry, wroteRow bool) {
			l.Reserve(e)
			l.Transition(e.ID, ledgerReserved, ledgerIssued, nil)
			l.Fail(e.ID, wroteRow, ledgerMarker{Incarnation: 5})
		}
		mk(ledgerGrant("prepare-failure", rowKey{"sessions", "a"}), false)
		mk(ledgerGrant("start-failed", rowKey{"sessions", "c"}), true)
		mk(ledgerCreate("create-failed", "tok"), false)
		// The census shows none of their markers.
		c := ledgerCensusOf(map[rowKey]ledgerRow{{"sessions", "a"}: {}, {"sessions", "c"}: {}})
		want := map[string]ledgerClear{
			"prepare-failure": clearUnwritten,
			"start-failed":    clearKeep, // wrote a row: waits for its marker
			"create-failed":   clearUnwritten,
		}
		for _, e := range l.View() {
			if got := e.clearVerdict(c, time.Now()); got != want[e.ID] {
				t.Errorf("%s: clearVerdict = %d, want %d", e.ID, got, want[e.ID])
			}
			if refund, ok := l.Release(e.ID); ok || refund != 0 {
				t.Errorf("%s: Release = (%d, %v), want (0, false): an issued grant's token is spent", e.ID, refund, ok)
			}
		}
	})
}

// committedCreate reserves, issues and commits a create on the sessions leg
// at the bubble's now, ambiguous or proven, and returns the settled entry.
func committedCreate(t *testing.T, l *intentLedger, id string, ambiguous bool, m ledgerMarker) ledgerEntry {
	t.Helper()
	l.Reserve(ledgerCreate(id, "tok-"+id))
	if _, _, ok := l.IssueCreate(id); !ok {
		t.Fatalf("issue %s lost", id)
	}
	if !l.CommitCreate(id, ambiguous, m) {
		t.Fatalf("commit %s lost", id)
	}
	e, _ := ledgerEntryOf(l, id)
	return e
}

// startedCensus is a census whose complete non-exact sessions leg was read
// from started, holding rows.
func startedCensus(rows map[rowKey]ledgerRow, started time.Time) ledgerCensus {
	c := ledgerCensusOf(rows, "sessions")
	c.ReadStarted = map[string]time.Time{"sessions": started}
	return c
}

// Kills (C2 rule 1, C5.4(3)): an ambiguous create resolved by a read that
// started before or at its settle (a read that may predate the write
// proving it absent), or never resolved by one that started after; an
// ambiguous create on an exact leg (no read start) resolved by absence
// instead of only by marker or the hard bound; the marker losing to an
// absence; a proven create cleared by absence; a reopen resolved by its
// reserved token instead of its row ID.
func TestLedgerAmbiguousCreateResolvedByReadStartedAfterSettle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newIntentLedger(time.Now)
		e := committedCreate(t, l, "c", true, ledgerMarker{})
		settled := e.SettledAt
		advance(time.Second)
		token := map[rowKey]ledgerRow{{"sessions", "gc-x"}: {InstanceToken: "tok-c"}}
		other := map[rowKey]ledgerRow{{"sessions", "gc-y"}: {InstanceToken: "other"}}
		for _, tt := range []struct {
			name string
			c    ledgerCensus
			want ledgerClear
		}{
			{"read started before the settle, token absent", startedCensus(other, settled.Add(-time.Nanosecond)), clearKeep},
			{"read started at the settle, token absent", startedCensus(other, settled), clearKeep},
			{"read started after the settle, token absent", startedCensus(other, settled.Add(time.Nanosecond)), clearUnwritten},
			{"read started after the settle, token present", startedCensus(token, settled.Add(time.Nanosecond)), clearWritten},
			{"read started before the settle, token present", startedCensus(token, settled.Add(-time.Second)), clearWritten},
			{"exact leg, token absent", ledgerCensusOf(other, "sessions"), clearKeep},
			{"another leg read after the settle", func() ledgerCensus {
				c := startedCensus(other, time.Time{})
				c.ReadStarted = map[string]time.Time{"rig": settled.Add(time.Second)}
				return c
			}(), clearKeep},
		} {
			if got := e.clearVerdict(tt.c, time.Now()); got != tt.want {
				t.Errorf("%s: clearVerdict = %d, want %d", tt.name, got, tt.want)
			}
		}
		// On an exact leg only the hard bound resolves it.
		if got := e.clearVerdict(ledgerCensusOf(other, "sessions"), settled.Add(ledgerHardBound)); got != clearHardBound {
			t.Errorf("exact leg at the hard bound: clearVerdict = %d, want clearHardBound", got)
		}

		proven := committedCreate(t, l, "p", false, ledgerMarker{RowID: "gc-p"})
		if got := proven.clearVerdict(startedCensus(other, proven.SettledAt.Add(time.Second)), time.Now()); got != clearKeep {
			t.Errorf("proven create absent from a later read: clearVerdict = %d, want clearKeep (marker or hard bound only)", got)
		}

		l.Reserve(ledgerCreate("r", "tok-r"))
		l.IssueCreate("r")
		l.Retarget("r", "gc-closed")
		l.CommitCreate("r", true, ledgerMarker{RowID: "gc-closed", InstanceToken: "tok-r"})
		reopen, _ := ledgerEntryOf(l, "r")
		later := reopen.SettledAt.Add(time.Second)
		if got := reopen.clearVerdict(startedCensus(map[rowKey]ledgerRow{{"sessions", "gc-closed"}: {}}, later), time.Now()); got != clearWritten {
			t.Errorf("reopened row open after the settle: clearVerdict = %d, want clearWritten", got)
		}
		if got := reopen.clearVerdict(startedCensus(other, later), time.Now()); got != clearUnwritten {
			t.Errorf("reopen target still closed after the settle: clearVerdict = %d, want clearUnwritten", got)
		}
	})
}

// Kills (C2 rule 1's input): a read start published for a leg whose rows
// prove nothing (missing, stale, or an exact leg's cache, which installs
// nothing for a failed write), or a last-good leg stamped with this pass's
// time instead of its own read's.
func TestCensusLedgerReadStartsOnlyForCompleteNonExactLegs(t *testing.T) {
	t0 := time.Unix(1_000, 0)
	c := &sessionCensus{Legs: []censusLeg{
		{Ref: "read", State: legRead, StartedAt: t0},
		{Ref: "last-good", State: legLastGood, StartedAt: t0.Add(-time.Minute)},
		{Ref: "exact", Exact: true, State: legRead},
		{Ref: "stale", State: legStale, StartedAt: t0},
		{Ref: "missing", State: legMissing},
	}}
	got := c.Ledger(&config.City{}).ReadStarted
	want := map[string]time.Time{"read": t0, "last-good": t0.Add(-time.Minute)}
	if len(got) != len(want) || !got["read"].Equal(want["read"]) || !got["last-good"].Equal(want["last-good"]) {
		t.Fatalf("ReadStarted = %v, want %v", got, want)
	}

	start, end := t0, t0.Add(3*time.Second)
	rec := censusRecording{StartedAt: start, At: end, Expires: end.Add(time.Minute)}
	r := newCensusReader(censusLegFeed{
		exact:    func(beads.Store) bool { return false },
		recorded: func(beads.Store) (censusRecording, bool) { return rec, true },
	})
	leg, _ := r.readLeg(end, classStoreCandidate{ref: "sessions"})
	if !leg.StartedAt.Equal(start) {
		t.Fatalf("read leg StartedAt = %v, want the recording's %v", leg.StartedAt, start)
	}
	rec = censusRecording{Err: errors.New("down")}
	if leg, _ = r.readLeg(end.Add(time.Second), classStoreCandidate{ref: "sessions"}); leg.State != legLastGood || !leg.StartedAt.Equal(start) {
		t.Fatalf("last-good leg = %+v, want StartedAt %v from its own read", leg, start)
	}
}

// Kills (C5.4(5), R24): a landed entry whose marker never appears counting
// forever, or clearing before the bound; the bound measured from reserve
// instead of the settle.
func TestLedgerHardBoundClearsAsUnwritten(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newIntentLedger(time.Now)
		l.Reserve(ledgerGrant("g", ledgerRowA))
		l.Issue("g", ledgerRowA)
		advance(time.Minute)
		l.Commit("g", ledgerMarker{Incarnation: 5})
		e, _ := ledgerEntryOf(l, "g")
		lagging := ledgerCensusOf(map[rowKey]ledgerRow{ledgerRowA: {Incarnation: 4}})
		if got := e.clearVerdict(lagging, e.SettledAt.Add(ledgerHardBound-time.Nanosecond)); got != clearKeep {
			t.Fatalf("before the bound: clearVerdict = %d, want clearKeep", got)
		}
		if got := e.clearVerdict(lagging, e.SettledAt.Add(ledgerHardBound)); got != clearHardBound {
			t.Fatalf("at the bound: clearVerdict = %d, want clearHardBound", got)
		}
		if got := e.clearVerdict(ledgerCensusOf(map[rowKey]ledgerRow{ledgerRowA: {Incarnation: 5}}), e.SettledAt.Add(ledgerHardBound)); got != clearWritten {
			t.Fatalf("marker visible at the bound: clearVerdict = %d, want clearWritten", got)
		}
		if !l.Transition("g", ledgerCommitted, ledgerCleared, nil) {
			t.Fatal("hard-bound clear lost")
		}
	})
}

// Kills (C2 rule 2): a grant whose row closed (a rolled-back start) holding
// its in-flight slot until the hard bound; or one clearing on an absence
// from a leg whose read is stale or missing, which proves nothing.
func TestLedgerGrantClearsWhenRowGoneFromCompleteLeg(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newIntentLedger(time.Now)
		for _, id := range []string{"committed", "failed-after-prewake"} {
			k := rowKey{Leg: "sessions", ID: id}
			l.Reserve(ledgerGrant(id, k))
			l.Issue(id, k)
		}
		l.Commit("committed", ledgerMarker{Incarnation: 5})
		l.Fail("failed-after-prewake", true, ledgerMarker{Incarnation: 5})
		for _, tt := range []struct {
			state censusLegState
			want  ledgerClear
		}{{legRead, clearWritten}, {legLastGood, clearWritten}, {legStale, clearKeep}, {legMissing, clearKeep}} {
			c := (&sessionCensus{Legs: []censusLeg{{Ref: "sessions", Exact: true, State: tt.state}}}).Ledger(&config.City{})
			for _, e := range l.View() {
				if got := e.clearVerdict(c, time.Now()); got != tt.want {
					t.Errorf("%s, row gone from a %s leg: clearVerdict = %d, want %d", e.ID, tt.state, got, tt.want)
				}
			}
		}
	})
}

// Kills: the create executor issuing or settling a grant, or committing a
// create it never issued; an ambiguous flag lost or set on a proven commit.
func TestLedgerCreateMovesRefuseOtherKindsAndStates(t *testing.T) {
	l := newIntentLedger(time.Now)
	l.Reserve(ledgerGrant("g", ledgerRowA))
	if _, _, ok := l.IssueCreate("g"); ok {
		t.Fatal("IssueCreate issued a grant")
	}
	l.Issue("g", ledgerRowA)
	if l.CommitCreate("g", false, ledgerMarker{}) {
		t.Fatal("CommitCreate settled a grant")
	}
	l.Reserve(ledgerCreate("c", "tok"))
	if l.CommitCreate("c", true, ledgerMarker{}) {
		t.Fatal("CommitCreate settled a reserved create")
	}
	for id, ambiguous := range map[string]bool{"proven": false, "ambiguous": true} {
		l.Reserve(ledgerCreate(id, "tok-"+id))
		l.IssueCreate(id)
		if !l.CommitCreate(id, ambiguous, ledgerMarker{RowID: "gc-" + id}) {
			t.Fatalf("commit %s lost", id)
		}
		if e, _ := ledgerEntryOf(l, id); e.Ambiguous != ambiguous || !e.WroteRow || e.Key.ID != "gc-"+id {
			t.Fatalf("%s entry = %+v", id, e)
		}
	}
}

// Kills: a reserved grant or create held forever, so a plan no key or
// executor picks up keeps its city slot, its endpoint's probe and, for a
// grant, its token;
// a release before the TTL; TTL release reaching an issued entry.
func TestLedgerReserveTTL(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newIntentLedger(time.Now)
		l.Reserve(ledgerGrant("grant", ledgerRowA))
		l.Reserve(ledgerGrant("issued", ledgerRowB))
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
		if refunds != 1 {
			t.Fatalf("TTL releases refunded %d, want 1 (the grant; a create debited none)", refunds)
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
	l.Reserve(ledgerGrant("b", ledgerRowB))
	l.Reserve(ledgerGrant("a", ledgerRowA))
	view := l.View()
	if len(view) != 2 || view[0].ID != "a" || view[1].ID != "b" {
		t.Fatalf("View order = %v", view)
	}
	view[0].State = ledgerCommitted
	if e, _ := ledgerEntryOf(l, "a"); e.State != ledgerReserved {
		t.Fatal("editing the view edited the ledger")
	}
}
