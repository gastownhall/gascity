package main

import (
	"fmt"
	"math/rand/v2"
	"sort"
	"testing"
	"time"
)

// Kills: refill per pass instead of per patrol interval (per-event passes
// multiplying the start rate); a bucket that overfills, refills on a clock
// step back, freezes after one until the clock regains the old mark, or drops
// its fractional credit; a debit that goes negative; a refund past capacity;
// a capacity cut on reload left unapplied.
func TestBucketRefillContinuousCappedDebitRefund(t *testing.T) {
	const capacity, interval = 5, 15 * time.Second // one token per 3s
	t0 := time.Unix(1_000, 0)
	b := bucketState{}.refill(t0, capacity, interval)
	if b.Tokens != capacity {
		t.Fatalf("first refill: %d tokens, want a full bucket", b.Tokens)
	}
	var ok bool
	for i := 0; i < capacity; i++ {
		if b, ok = b.debit(1); !ok {
			t.Fatalf("debit %d refused with %d tokens", i, b.Tokens)
		}
	}
	if _, ok = b.debit(1); ok || b.Tokens != 0 {
		t.Fatalf("debit from empty: ok=%v tokens=%d", ok, b.Tokens)
	}

	// 300 passes 10ms apart (3s) accrue exactly one token, like one pass.
	many := b
	for i := 1; i <= 300; i++ {
		many = many.refill(t0.Add(time.Duration(i)*10*time.Millisecond), capacity, interval)
	}
	one := b.refill(t0.Add(3*time.Second), capacity, interval)
	if many.Tokens != 1 || one.Tokens != 1 || many.Credit != 0 || one.Credit != 0 {
		t.Fatalf("3s of refill: many passes %+v, one pass %+v, want 1 token each", many, one)
	}

	// Fractional credit carries across passes.
	half := b.refill(t0.Add(1500*time.Millisecond), capacity, interval)
	if half.Tokens != 0 || half.Credit != 1500*time.Millisecond {
		t.Fatalf("1.5s: %+v, want 0 tokens and 1.5s credit", half)
	}
	if got := half.untilTokens(1, capacity, interval); got != 1500*time.Millisecond {
		t.Fatalf("untilTokens = %v, want 1.5s", got)
	}
	if got := half.refill(t0.Add(3*time.Second), capacity, interval); got.Tokens != 1 {
		t.Fatalf("1.5s + 1.5s: %+v, want 1 token", got)
	}

	// A clock step back accrues nothing and re-anchors the mark: refill
	// resumes from the new clock, not from the old mark 1.5s ahead.
	back := half.refill(t0, capacity, interval)
	if back.Tokens != half.Tokens || back.Credit != half.Credit || !back.LastRefill.Equal(t0) {
		t.Fatalf("step back: %+v, want %+v re-anchored at %v", back, half, t0)
	}
	if got := back.refill(t0.Add(1500*time.Millisecond), capacity, interval); got.Tokens != 1 {
		t.Fatalf("1.5s after a step back: %+v, want 1 token (refill frozen until the old mark)", got)
	}

	// The cap: an hour idle fills to capacity with no stored credit.
	full := b.refill(t0.Add(time.Hour), capacity, interval)
	if full.Tokens != capacity || full.Credit != 0 || full.untilTokens(1, capacity, interval) != 0 {
		t.Fatalf("after an hour: %+v, want full", full)
	}
	if got := full.refund(2, capacity); got.Tokens != capacity {
		t.Fatalf("refund past capacity: %d tokens", got.Tokens)
	}
	if got := b.refund(2, capacity); got.Tokens != 2 {
		t.Fatalf("refund 2 to empty: %d tokens", got.Tokens)
	}
	// A reload lowers max_wakes_per_tick: the next refill clamps.
	if got := full.refill(t0.Add(time.Hour), 2, interval); got.Tokens != 2 {
		t.Fatalf("capacity cut to 2: %d tokens", got.Tokens)
	}
}

// Kills: a non-positive patrol interval or capacity refilling the bucket to
// full on every pass (an unbounded start rate from a bad config).
func TestBucketRefillGuardsNonPositiveIntervalAndCapacity(t *testing.T) {
	t0 := time.Unix(1_000, 0)
	for _, interval := range []time.Duration{0, -time.Second} {
		b := bucketState{}.refill(t0, 5, interval)
		if b.Tokens != 5 {
			t.Fatalf("interval %v: first refill %d tokens, want a full bucket", interval, b.Tokens)
		}
		b, _ = b.debit(5)
		for i := 1; i <= 3; i++ {
			if b = b.refill(t0.Add(time.Duration(i)*time.Hour), 5, interval); b.Tokens != 0 {
				t.Fatalf("interval %v, pass %d: refilled to %d tokens, want none", interval, i, b.Tokens)
			}
		}
	}
	b := bucketState{Tokens: 3, LastRefill: t0}.refill(t0.Add(time.Hour), 0, time.Minute)
	if b.Tokens != 0 || b.Credit != 0 {
		t.Fatalf("capacity 0: %+v, want empty", b)
	}
}

// Kills: a token leaked or counted twice anywhere in the ledger's life (a
// release refund applied when the key won the race, a refund on any clear, a
// refund for a failed issued grant, a debit for a create). The property is
// I-tokens restated (C5.7): once every entry has cleared, debits minus
// refunds equal the number of grants ever issued.
func TestBucketTokenConservation(t *testing.T) {
	const capacity = 1 << 20 // never caps a refund, so the level is exact
	for seed := uint64(1); seed <= 200; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(seed, 7))
			now := time.Unix(1_000, 0)
			l := newIntentLedger(func() time.Time { return now })
			b := bucketState{}.refill(now, capacity, time.Minute)
			rows := make(map[rowKey]ledgerRow) // the census
			kinds := make(map[string]ledgerKind)
			var ids []string
			issued := 0
			pass := func() {
				c := ledgerCensusOf(rows, "sessions")
				for _, e := range l.View() {
					if e.clearVerdict(c, now) != clearKeep && !l.Transition(e.ID, e.State, ledgerCleared, nil) {
						t.Fatalf("clear of %s lost", e.ID)
					}
				}
			}
			for step := 0; step < 60; step++ {
				now = now.Add(time.Second)
				var id string
				if len(ids) > 0 {
					id = ids[rng.IntN(len(ids))]
				}
				switch op := rng.IntN(7); {
				case op == 0 || id == "":
					id = fmt.Sprintf("e%02d", step)
					e := ledgerCreate(id, "tok-"+id)
					cost := 0
					if rng.IntN(2) == 0 {
						e, cost = ledgerGrant(id, rowKey{"sessions", "row-" + id}), 1
						rows[e.Key] = ledgerRow{Incarnation: 1}
					}
					var ok bool
					if b, ok = b.debit(cost); ok && l.Reserve(e) {
						kinds[id] = e.Kind
						ids = append(ids, id)
					}
				case op == 1:
					if kinds[id] == kindGrant {
						if l.Issue(id, rowKey{"sessions", "row-" + id}) {
							issued++
						}
					} else {
						l.IssueCreate(id)
					}
				case op == 2:
					l.Commit(id, ledgerMarker{RowID: "row-" + id, Incarnation: 2})
				case op == 3:
					l.Fail(id, rng.IntN(2) == 0, ledgerMarker{RowID: "row-" + id, Incarnation: 2})
				case op == 4:
					if refund, ok := l.Release(id); ok {
						b = b.refund(refund, capacity)
					}
				case op == 5: // the effect's write reaches the census
					rows[rowKey{"sessions", "row-" + id}] = ledgerRow{Incarnation: 2}
				default:
					pass()
				}
			}
			// Quiesce: release what is reserved, finalize what is issued, let
			// every marker reach the census, clear.
			for _, e := range l.View() {
				switch e.State {
				case ledgerReserved:
					if refund, ok := l.Release(e.ID); ok {
						b = b.refund(refund, capacity)
					}
				case ledgerIssued:
					l.Fail(e.ID, false, ledgerMarker{})
				}
			}
			for _, e := range l.View() {
				rows[rowKey{"sessions", "row-" + e.ID}] = ledgerRow{Incarnation: 2}
			}
			pass()
			if left := l.View(); len(left) != 0 {
				t.Fatalf("entries left at quiescence: %+v", left)
			}
			if spent := capacity - b.Tokens; spent != issued {
				t.Fatalf("tokens spent %d, want %d issued grants (I-tokens)", spent, issued)
			}
		})
	}
}

// flightRow is one session row in the in-flight model: the store's truth,
// or the cache's lagging copy of it.
type flightRow struct {
	ledgerRow
	Open  bool
	Owner string // the effect whose start lease or pending create the row carries
}

// flightEffect is the model's truth about one admitted effect.
type flightEffect struct {
	kind    ledgerKind
	key     rowKey // grant: its row; create: the row it writes
	token   string
	written bool  // its write landed in the store
	inc     int64 // grant: the incarnation its write sets
}

// Kills (I-ledger, C5.13): an effect counted zero times or twice by
// cityInFlight on the view a pass counts, before or after the pass clears
// entries. Random interleavings cover census lag (writes reach the cache
// late, singly), rows closing and leases ending under the ledger, whole-leg
// recordings that resolve ambiguous creates (C5.4(3)), the hard bound,
// reserve TTL, release racing issue in both orders, and settles that land
// before or after the census shows the write. Every effect truly in flight
// must be represented, by an entry that counts or by a census row that
// carries its lease or pending create, and cityInFlight must equal the
// number of effects represented. The one accepted exception is an entry the
// hard bound cleared (C5.4(5)). A write never lands after a recording that
// started after its settle: the effect returned before that read began.
func TestLedgerInFlightExactlyOnce(t *testing.T) {
	seeds, steps := 3000, 80
	if testing.Short() {
		seeds = 300
	}
	for seed := uint64(1); seed <= uint64(seeds); seed++ {
		runInFlightModel(t, seed, steps)
		if t.Failed() {
			return
		}
	}
}

func runInFlightModel(t *testing.T, seed uint64, steps int) {
	t.Helper()
	rng := rand.New(rand.NewPCG(seed, 11))
	now := time.Unix(1_000, 0)
	l := newIntentLedger(func() time.Time { return now })
	store := make(map[rowKey]flightRow)
	cache := make(map[rowKey]flightRow) // open rows only, as delivered
	for i := 0; i < 4; i++ {
		k := rowKey{"sessions", fmt.Sprintf("gc-%d", i)}
		store[k] = flightRow{ledgerRow: ledgerRow{Incarnation: 1}, Open: true}
		cache[k] = store[k]
	}
	effects := make(map[string]*flightEffect)
	bound := make(map[string]bool) // cleared at the hard bound
	var ids []string
	var recorded time.Time // when the last whole-leg recording started
	step := 0
	census := func() ledgerCensus {
		rows := make(map[rowKey]ledgerRow, len(cache))
		for k, r := range cache {
			rows[k] = r.ledgerRow
		}
		c := ledgerCensusOf(rows, "sessions")
		c.ReadStarted = map[string]time.Time{"sessions": recorded}
		return c
	}
	deliver := func(k rowKey) {
		if r, ok := store[k]; ok && r.Open {
			cache[k] = r
		} else {
			delete(cache, k)
		}
	}
	entryOf := func(id string) (ledgerEntry, bool) { return ledgerEntryOf(l, id) }
	check := func(phase string) {
		t.Helper()
		view, c := l.View(), census()
		live := make(map[string]ledgerEntry, len(view))
		represented := make(map[string]bool)
		for _, e := range view {
			live[e.ID] = e
			if ledgerCounts(e) {
				represented[e.ID] = true
			}
		}
		for _, r := range cache {
			if r.StartLease || r.PendingCreate {
				represented[r.Owner] = true
			}
		}
		for id, f := range effects {
			e, ok := live[id]
			inFlight := ok && (e.State == ledgerReserved || e.State == ledgerIssued)
			if r := store[f.key]; f.written && r.Open && r.Owner == id && (r.StartLease || r.PendingCreate) {
				inFlight = true
			}
			if inFlight && !represented[id] && !bound[id] {
				t.Fatalf("seed %d step %d %s: %s in flight but unrepresented (entry %+v present=%v)", seed, step, phase, id, e, ok)
			}
		}
		if got := cityInFlight(view, c, nil); got != len(represented) {
			t.Fatalf("seed %d step %d %s: cityInFlight %d, represented %d: %v\nview %+v\ncache %+v", seed, step, phase, got, len(represented), represented, view, cache)
		}
	}
	pass := func() {
		check("pre-clear")
		c := census()
		for _, e := range l.View() {
			v := e.clearVerdict(c, now)
			if v != clearKeep && !l.Transition(e.ID, e.State, ledgerCleared, nil) {
				t.Fatalf("seed %d step %d: clear of %s lost", seed, step, e.ID)
			}
			bound[e.ID] = v == clearHardBound
		}
		check("post-clear")
		for _, e := range l.View() {
			if e.reserveExpired(now) {
				l.Release(e.ID)
			}
		}
	}
	pick := func() (string, *flightEffect) {
		if len(ids) == 0 {
			return "", nil
		}
		id := ids[rng.IntN(len(ids))]
		return id, effects[id]
	}
	pickRow := func(m map[rowKey]flightRow) (rowKey, bool) {
		keys := make([]rowKey, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		if len(keys) == 0 {
			return rowKey{}, false
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i].ID < keys[j].ID })
		return keys[rng.IntN(len(keys))], true
	}
	for ; step < steps; step++ {
		now = now.Add(time.Duration(rng.IntN(16)) * time.Second)
		id, f := pick()
		switch op := rng.IntN(12); op {
		case 0: // reserve a grant for an idle row the census shows
			k, ok := pickRow(cache)
			if !ok || cache[k].StartLease || cache[k].PendingCreate {
				continue
			}
			// C5.5: no other uncleared entry for the row, matched by key or,
			// for a create the census shows before it settles, by token.
			busy := false
			for _, e := range l.View() {
				busy = busy || e.Key == k ||
					(e.Kind == kindCreate && e.Marker.InstanceToken == cache[k].InstanceToken)
			}
			gid := fmt.Sprintf("g%02d", step)
			if !busy && l.Reserve(ledgerGrant(gid, k)) {
				effects[gid] = &flightEffect{kind: kindGrant, key: k}
				ids = append(ids, gid)
			}
		case 1: // reserve a create
			cid := fmt.Sprintf("c%02d", step)
			e := ledgerCreate(cid, "tok-"+cid)
			if l.Reserve(e) {
				effects[cid] = &flightEffect{kind: kindCreate, key: rowKey{"sessions", "row-" + cid}, token: "tok-" + cid}
				ids = append(ids, cid)
			}
		case 2: // the key or executor issues
			if f == nil {
				continue
			}
			if f.kind == kindGrant {
				l.Issue(id, f.key)
			} else {
				l.Transition(id, ledgerReserved, ledgerIssued, nil)
			}
		case 3: // the allocator releases
			if f != nil {
				l.Release(id)
			}
		case 4: // the effect's write lands, before or after its settle
			e, ok := entryOf(id)
			// Issued, or committed without its write (ambiguous), and no
			// recording started since the settle.
			if !ok || f.written || (e.State != ledgerIssued && e.State != ledgerCommitted) ||
				(e.State == ledgerCommitted && recorded.After(e.SettledAt)) {
				continue
			}
			if f.kind == kindGrant {
				r := store[f.key]
				if !r.Open || (e.State == ledgerCommitted && r.Incarnation+1 != e.Marker.Incarnation) {
					continue
				}
				r.Incarnation++
				r.StartLease, r.PendingCreate, r.Owner = true, false, id
				store[f.key], f.inc, f.written = r, r.Incarnation, true
			} else {
				store[f.key] = flightRow{ledgerRow: ledgerRow{Incarnation: 1, InstanceToken: f.token, PendingCreate: true}, Open: true, Owner: id}
				f.written = true
			}
		case 5: // the effect settles
			e, ok := entryOf(id)
			if !ok || e.State != ledgerIssued {
				continue
			}
			var m ledgerMarker
			switch {
			case f.kind == kindGrant && f.written:
				m.Incarnation = f.inc
			case f.kind == kindGrant:
				m.Incarnation = store[f.key].Incarnation + 1 // what PreWake would write
			case f.written:
				m.RowID = f.key.ID
			}
			switch {
			case f.written && rng.IntN(2) == 0:
				l.Fail(id, true, m)
			case f.written && f.kind == kindCreate:
				l.CommitCreate(id, rng.IntN(2) == 0, m) // proven, or ambiguous after it landed
			case f.written:
				l.Commit(id, m)
			case rng.IntN(2) == 0:
				l.Fail(id, false, ledgerMarker{})
			case f.kind == kindCreate:
				l.CommitCreate(id, true, m) // ambiguous: the write may yet land
			default:
				l.Commit(id, m)
			}
		case 6, 7: // one row's latest state reaches the cache
			if k, ok := pickRow(store); ok {
				deliver(k)
			}
		case 8: // a row closes
			if k, ok := pickRow(store); ok {
				r := store[k]
				r.Open = false
				store[k] = r
			}
		case 9: // a start lease or pending create ends
			if k, ok := pickRow(store); ok {
				r := store[k]
				r.StartLease, r.PendingCreate = false, false
				store[k] = r
			}
		case 10: // the lane records the whole leg: every open row, from a read started now
			recorded = now
			clear(cache)
			for k, r := range store {
				if r.Open {
					cache[k] = r
				}
			}
		default:
			pass()
		}
	}
	pass()
}

// Kills: admission past the bucket or the city cap, including a start
// admitted on an empty bucket (C5.8: no prepaid grants); a half-open
// endpoint admitting a herd; an open endpoint admitting anything; admission
// demanding more than the one token a grant costs (C6).
func TestAdmitStartTokensCapAndBreaker(t *testing.T) {
	two := bucketState{Tokens: 2}
	one := bucketState{Tokens: 1}
	empty := bucketState{}
	tests := []struct {
		name        string
		b           bucketState
		inFlight    int
		gate        endpointGate
		outstanding int
		want        bool
	}{
		{"closed, budget and slot", two, 0, gateClosed, 3, true},
		{"exactly one token", one, 0, gateClosed, 0, true},
		{"no tokens", empty, 0, gateClosed, 0, false},
		{"city cap reached", two, 5, gateClosed, 0, false},
		{"one under the cap", two, 4, gateClosed, 0, true},
		{"probe, nothing outstanding", two, 0, gateProbe, 0, true},
		{"probe already outstanding", two, 0, gateProbe, 1, false},
		{"shut", two, 0, gateShut, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := admitStart(tt.b, tt.inFlight, 5, tt.gate, tt.outstanding); got != tt.want {
				t.Fatalf("admitStart = %v, want %v", got, tt.want)
			}
		})
	}
}

// Kills: the gate misreading the guard (an open endpoint admitting, a due
// probe read as closed and admitting a herd, a probe in flight admitting a
// second probe); a nil guard refusing.
func TestEndpointGateOfReadsGuard(t *testing.T) {
	now := time.Unix(1_000, 0)
	g := newEndpointCapacityGuard(func() time.Time { return now })
	const k = endpointKey("provider:p")
	if got := endpointGateOf(g, k); got != gateClosed {
		t.Fatalf("fresh endpoint: gate %d, want closed", got)
	}
	ticket, ok := g.Admit(k, "gc-a", "worker")
	if !ok {
		t.Fatal("closed endpoint refused Admit")
	}
	ticket.Resolve(verdictCapacity)
	if got := endpointGateOf(g, k); got != gateShut {
		t.Fatalf("after a refusal: gate %d, want shut", got)
	}
	now = now.Add(capacityBreakerSettings.OpenMax + time.Second)
	if got := endpointGateOf(g, k); got != gateProbe {
		t.Fatalf("probe due: gate %d, want probe", got)
	}
	probe, ok := g.Admit(k, "gc-b", "worker")
	if !ok {
		t.Fatal("due probe refused Admit")
	}
	if got := endpointGateOf(g, k); got != gateShut {
		t.Fatalf("probe in flight: gate %d, want shut", got)
	}
	probe.Resolve(verdictSuccess)
	if got := endpointGateOf(g, k); got != gateClosed {
		t.Fatalf("after the probe succeeded: gate %d, want closed", got)
	}
	if got := endpointGateOf(nil, k); got != gateClosed {
		t.Fatalf("nil guard: gate %d, want closed", got)
	}
	if got := endpointGateOf(g, ""); got != gateClosed {
		t.Fatalf("unguarded (empty) key: gate %d, want closed", got)
	}
}

// Kills: a gate the gather phase forgot to capture admitting a start, or
// dropping its endpoint's pending rows from the city count.
func TestEndpointGateZeroValueAdmitsNothing(t *testing.T) {
	var forgotten endpointGate
	if forgotten != gateShut {
		t.Fatalf("zero gate = %d, want shut", forgotten)
	}
	gates := map[endpointKey]endpointGate{"provider:a": gateClosed}
	if admitStart(bucketState{Tokens: 5}, 0, 5, gates["provider:missing"], 0) {
		t.Fatal("an uncaptured gate admitted a start")
	}
	c := ledgerCensusOf(map[rowKey]ledgerRow{{"sessions", "gc-p"}: {Endpoint: "provider:missing", PendingCreate: true}})
	if got := cityInFlight(nil, c, gates); got != 1 {
		t.Fatalf("pending row behind an uncaptured gate: in flight %d, want 1", got)
	}
}

// Kills: an effect counted twice (entry and row, create and its row's
// grant), or not at all (a landed grant whose marker the census lags); a
// failure that wrote nothing hiding the row's own lease, or counted itself.
func TestCityInFlightCountsOncePerEffect(t *testing.T) {
	grant := func(id string, k rowKey, st ledgerState, wrote bool) ledgerEntry {
		e := ledgerGrant(id, k)
		e.State, e.WroteRow = st, wrote
		return e
	}
	created := ledgerCreate("c-landed", "tok-landed")
	created.State, created.WroteRow, created.Key.ID = ledgerCommitted, true, "gc-new"
	pending := ledgerCreate("c-pending", "tok-pending")
	rows := map[rowKey]ledgerRow{
		ledgerRowA:               {StartLease: true},                                  // with a reserved grant
		ledgerRowB:               {Incarnation: 4},                                    // landed grant, census lagging
		{"sessions", "gc-c"}:     {StartLease: true},                                  // failed before writing
		{"sessions", "gc-new"}:   {PendingCreate: true},                               // the landed create's row
		{"sessions", "gc-tok"}:   {InstanceToken: "tok-pending", PendingCreate: true}, // the pending create's row, by token
		{"sessions", "gc-lease"}: {StartLease: true},                                  // a start from before the restart
		{"sessions", "gc-idle"}:  {},                                                  // nothing in flight
	}
	view := []ledgerEntry{
		grant("g-a", ledgerRowA, ledgerReserved, false),
		grant("g-b", ledgerRowB, ledgerCommitted, true),
		grant("g-c", rowKey{"sessions", "gc-c"}, ledgerFailed, false),
		grant("g-new", rowKey{"sessions", "gc-new"}, ledgerReserved, false), // the landed create's first grant
		created, pending,
		grant("g-idle", rowKey{"sessions", "gc-idle"}, ledgerFailed, false), // failed without writing: represents nothing
	}
	// g-a, g-b, gc-c's lease, gc-new (create + grant + row), pending (entry + row), gc-lease.
	if got := cityInFlight(view, ledgerCensusOf(rows), nil); got != 6 {
		t.Fatalf("cityInFlight = %d, want 6", got)
	}
}

// Kills (N32, N33): a create counted only once its row is in the census
// (a reserved create, or a committed create the census lags, would let the
// pass admit past the city cap); (N36) a running start's lease dropped
// because its endpoint's breaker shut after the start was admitted.
func TestCityInFlightCountsCreatesWithoutRowsAndLeasesBehindShutEndpoints(t *testing.T) {
	reserved := ledgerCreate("c-reserved", "tok-r")
	committed := ledgerCreate("c-committed", "tok-c")
	committed.State, committed.WroteRow, committed.Key.ID = ledgerCommitted, true, "gc-unseen"
	committed.Marker.RowID, committed.SettledAt = "gc-unseen", time.Unix(1_000, 0)
	empty := ledgerCensusOf(nil, "sessions")
	for _, tt := range []struct {
		name string
		e    ledgerEntry
	}{{"reserved create (N32)", reserved}, {"committed create, no census row (N33)", committed}} {
		if got := cityInFlight([]ledgerEntry{tt.e}, empty, nil); got != 1 {
			t.Errorf("%s: in flight %d, want 1", tt.name, got)
		}
		if tt.e.clearVerdict(empty, time.Unix(1_000, 0)) != clearKeep {
			t.Errorf("%s: cleared with no census row", tt.name)
		}
	}
	shut := map[endpointKey]endpointGate{"provider:a": gateShut}
	c := ledgerCensusOf(map[rowKey]ledgerRow{{"sessions", "gc-run"}: {Endpoint: "provider:a", StartLease: true}})
	if got := cityInFlight(nil, c, shut); got != 1 {
		t.Fatalf("start lease behind a shut endpoint (N36): in flight %d, want 1", got)
	}
}

// Kills (F5, R-park): rows parked behind a shut endpoint filling the city
// cap and starving a healthy endpoint; parked rows not counted once their
// endpoint can admit again.
func TestCityInFlightParkedPendingCountsOnlyWhileEndpointEligible(t *testing.T) {
	const capacity = 50
	rows := make(map[rowKey]ledgerRow)
	for i := 0; i < capacity; i++ {
		rows[rowKey{"sessions", fmt.Sprintf("a-%02d", i)}] = ledgerRow{Endpoint: "provider:a", PendingCreate: true}
	}
	rows[rowKey{"sessions", "b-live"}] = ledgerRow{Endpoint: "provider:b", StartLease: true}
	c := ledgerCensusOf(rows)
	b := bucketState{Tokens: capacity}

	shut := map[endpointKey]endpointGate{"provider:a": gateShut}
	inFlight := cityInFlight(nil, c, shut)
	if inFlight != 1 {
		t.Fatalf("A shut: in flight %d, want 1 (B's lease only)", inFlight)
	}
	if !admitStart(b, inFlight, capacity, gateClosed, endpointOutstanding(nil)["provider:b"]) {
		t.Fatal("B starved behind A's parked rows")
	}
	for _, gate := range []endpointGate{gateProbe, gateClosed} {
		gates := map[endpointKey]endpointGate{"provider:a": gate}
		if got := cityInFlight(nil, c, gates); got != capacity+1 {
			t.Fatalf("A gate %d: in flight %d, want %d", gate, got, capacity+1)
		}
	}
}

// Kills: a half-open endpoint admitting a second probe while a grant or a
// create for it is outstanding; a create and its row's grant counted as two
// probes; a committed start still blocking the probe.
func TestEndpointOutstanding(t *testing.T) {
	g := func(id string, k rowKey, ep endpointKey, st ledgerState) ledgerEntry {
		e := ledgerGrant(id, k)
		e.Endpoint, e.State = ep, st
		return e
	}
	created := ledgerCreate("c", "tok")
	created.Endpoint, created.State, created.WroteRow, created.Key.ID = "provider:b", ledgerCommitted, true, "gc-new"
	reserved := ledgerCreate("c2", "tok2")
	reserved.Endpoint = "provider:c"
	view := []ledgerEntry{
		g("g1", ledgerRowA, "provider:a", ledgerReserved),
		g("g2", ledgerRowB, "provider:a", ledgerIssued),
		g("g3", rowKey{"sessions", "gc-3"}, "provider:d", ledgerCommitted),
		g("g4", rowKey{"sessions", "gc-new"}, "provider:b", ledgerReserved),
		created, reserved,
	}
	got := endpointOutstanding(view)
	want := map[endpointKey]int{"provider:a": 2, "provider:b": 1, "provider:c": 1}
	if len(got) != len(want) {
		t.Fatalf("endpointOutstanding = %v, want %v", got, want)
	}
	for k, n := range want {
		if got[k] != n {
			t.Fatalf("endpointOutstanding = %v, want %v", got, want)
		}
	}
}
