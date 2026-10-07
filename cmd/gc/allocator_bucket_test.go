package main

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"
)

// Kills: refill per pass instead of per patrol interval (per-event passes
// multiplying the start rate); a bucket that overfills, refills on a clock
// step back, freezes after one until the clock regains the old mark, or drops
// its fractional credit; a capacity cut on reload left unapplied.
func TestBucketRefillContinuousCapped(t *testing.T) {
	const capacity, interval = 5, 15 * time.Second // one token per 3s
	t0 := time.Unix(1_000, 0)
	b := bucketState{}.refill(t0, capacity, interval)
	if b.Tokens != capacity {
		t.Fatalf("first refill: %d tokens, want a full bucket", b.Tokens)
	}
	b.Tokens = 0 // every token spent

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
	if got := half.nextToken(capacity, interval); !got.Equal(t0.Add(3 * time.Second)) {
		t.Fatalf("nextToken = %v, want t0+3s", got)
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
	if full.Tokens != capacity || full.Credit != 0 || !full.nextToken(capacity, interval).IsZero() {
		t.Fatalf("after an hour: %+v, want full", full)
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
		b.Tokens = 0
		for i := 1; i <= 3; i++ {
			if b = b.refill(t0.Add(time.Duration(i)*time.Hour), 5, interval); b.Tokens != 0 {
				t.Fatalf("interval %v, pass %d: refilled to %d tokens, want none", interval, i, b.Tokens)
			}
		}
	}
	b := bucketState{Tokens: 3, LastRefill: t0}.refill(t0.Add(time.Hour), 0, time.Minute)
	if b.Tokens != 0 || b.Credit != 0 || !b.nextToken(0, time.Minute).IsZero() {
		t.Fatalf("capacity 0: %+v, want empty, with no next token", b)
	}
}

// Kills: admission past the bucket or the city cap, including a start
// admitted on an empty bucket; a half-open endpoint admitting a herd; an
// open endpoint admitting anything; admission demanding more than the one
// token a start costs; a refusal reported under the wrong cause.
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
		want        string
	}{
		{"closed, budget and slot", two, 0, gateClosed, 3, ""},
		{"exactly one token", one, 0, gateClosed, 0, ""},
		{"no tokens", empty, 0, gateClosed, 0, causeAwaitingBudget},
		{"city cap reached", two, 5, gateClosed, 0, causeCityCap},
		{"one under the cap", two, 4, gateClosed, 0, ""},
		{"probe, nothing outstanding", two, 0, gateProbe, 0, ""},
		{"probe already outstanding", two, 0, gateProbe, 1, causeEndpointGate},
		{"shut", two, 0, gateShut, 0, causeEndpointGate},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := admitStart(tt.b, tt.inFlight, 5, tt.gate, tt.outstanding); got != tt.want {
				t.Fatalf("admitStart = %q, want %q", got, tt.want)
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
	if admitStart(bucketState{Tokens: 5}, 0, 5, gates["provider:missing"], 0) == "" {
		t.Fatal("an uncaptured gate admitted a start")
	}
	rows := []bringUpRow{{Key: rowKey{"sessions", "gc-p"}, Endpoint: "provider:missing", PendingCreate: true}}
	if got, _ := cityInFlight(inflightView{}, rows, gatesOf(gates)); got != 1 {
		t.Fatalf("pending row behind an uncaptured gate: in flight %d, want 1", got)
	}
}

func gatesOf(m map[endpointKey]endpointGate) func(endpointKey) (endpointGate, bool) {
	return func(k endpointKey) (endpointGate, bool) { g, ok := m[k]; return g, ok }
}

// Kills double counting and dropped bring-ups in the city count (v5 P4): a
// running start for a row holding a claim counts once; a create whose token
// a census row carries counts as that row; a running or ambiguous create no
// row carries counts; a duplicate copy's claim, a row without a claim (a
// fresh last_woke_at is no lease) and a claim behind a shut endpoint count
// nothing; a probe-gated backlog counts once in total, its running probe
// start included; counted names the rows holding a slot.
func TestCityInFlightCountsDistinctBringUpRowsOnce(t *testing.T) {
	r := func(id string) rowKey { return rowKey{"sessions", id} }
	view := inflightView{Entries: []inflightEntry{
		{Kind: inflightStart, Key: r("started")},
		{Kind: inflightStart, Key: r("probe-1"), Endpoint: "provider:probe"},
		{Kind: inflightStart, Key: r("plain")},
		{Kind: inflightCreate, Token: "tok-landed"},
		{Kind: inflightCreate, Token: "tok-running"},
		{Kind: inflightCreate, Token: "tok-ambiguous", Ambiguous: true},
	}}
	rows := []bringUpRow{
		{Key: r("started"), PendingCreate: true},
		{Key: r("landed"), Token: "tok-landed", PendingCreate: true},
		{Key: rowKey{"rig", "landed"}, Token: "tok-landed"},
		{Key: r("woke"), Token: "tok-w"},
		{Key: r("shut"), Endpoint: "provider:shut", PendingCreate: true},
		{Key: r("probe-1"), Endpoint: "provider:probe", PendingCreate: true},
		{Key: r("probe-2"), Endpoint: "provider:probe", PendingCreate: true},
		{Key: r("probe-3"), Endpoint: "provider:probe", PendingCreate: true},
	}
	gates := gatesOf(map[endpointKey]endpointGate{"": gateClosed, "provider:shut": gateShut, "provider:probe": gateProbe})
	n, counted := cityInFlight(view, rows, gates)
	// started, probe-1 (the backlog's slot), plain, landed, tok-running, tok-ambiguous.
	if n != 6 {
		t.Fatalf("in flight = %d, want 6", n)
	}
	for _, id := range []string{"started", "probe-1", "probe-2", "probe-3", "plain", "landed"} {
		if !counted[r(id)] {
			t.Errorf("%s holds no slot, want one", id)
		}
	}
	if counted[r("woke")] || counted[r("shut")] {
		t.Errorf("counted = %v: a row without a claim, or behind a shut endpoint, holds no slot", counted)
	}
	if got, _ := cityInFlight(inflightView{}, rows[5:], gates); got != 1 {
		t.Fatalf("idle probe backlog: in flight %d, want 1", got)
	}
}

// Kills a half-open endpoint admitting a second probe while a start or a
// create for it runs, or an adopt (which takes no ticket) holding it.
func TestEndpointOutstanding(t *testing.T) {
	got := endpointOutstanding(inflightView{Entries: []inflightEntry{
		{Kind: inflightStart, Key: rowKey{"sessions", "a"}, Endpoint: "provider:a"},
		{Kind: inflightCreate, Token: "t1", Endpoint: "provider:a"},
		{Kind: inflightCreate, Token: "t2", Endpoint: "provider:b", Ambiguous: true},
		{Kind: "adopt", Key: rowKey{"sessions", "c"}, Endpoint: "provider:c"},
	}})
	if len(got) != 2 || got["provider:a"] != 2 || got["provider:b"] != 1 {
		t.Fatalf("endpointOutstanding = %v, want provider:a 2, provider:b 1", got)
	}
}

// lagEffect is one effect in the census-lag model: a start on a row, or a
// create under its token, with the passes until it settles. A create lands,
// fails without writing, or errors ambiguously after its row landed (the
// cache declines the install, so the census shows the row only lag passes
// later, perhaps past the hard bound) or before it did.
type lagEffect struct {
	kind    string
	key     rowKey
	token   string
	seq     uint64
	runFor  int
	outcome int // create: 0 landed, 1 failed, 2 ambiguous and landed, 3 ambiguous and unwritten
	lag     int
}

// Kills (I8, P4, P5) a bring-up counted twice or not at all across passes:
// an ambiguous create cleared before its row shows, counted beside the row
// that carries its token, or never cleared; a committed start's row still
// counted. Each pass drains the settlements, applies the census installs due,
// clears what the census shows, and the city count must equal an oracle over
// the model's truth: running effects, ambiguous creates within the hard bound
// whose row the census does not show, and the census's claimed rows, each
// bring-up once by its token or row. At quiescence the map is empty.
func TestCityInFlightCensusLagModel(t *testing.T) {
	closed := func(endpointKey) (endpointGate, bool) { return gateClosed, true }
	for seed := uint64(1); seed <= 300; seed++ {
		rng := rand.New(rand.NewPCG(seed, 3))
		now := time.Unix(10_000, 0)
		m := newInflightMap()
		census := make(map[rowKey]bringUpRow)
		installs := make(map[rowKey]int) // a landed row's census install, in passes
		var running []*lagEffect
		ambiguous := make(map[string]time.Time) // token → settled at, until the census shows it
		next := 0
		identity := func(r bringUpRow) string {
			if r.Token != "" {
				return "tok:" + r.Token
			}
			return "row:" + r.Key.ID
		}
		pass := func() {
			now = now.Add(10 * time.Second)
			var still []*lagEffect
			for _, e := range running {
				if e.runFor--; e.runFor > 0 {
					still = append(still, e)
					continue
				}
				s := settlement{Kind: e.kind, Key: e.key, Token: e.token, Seq: e.seq, At: now}
				switch {
				case e.kind == inflightStart: // the commit lands and drops the claim
					r := census[e.key]
					r.PendingCreate = false
					census[e.key] = r
				case e.outcome == 0:
					census[e.key] = bringUpRow{Key: e.key, Token: e.token, PendingCreate: true}
				case e.outcome >= 2:
					s.Ambiguous, ambiguous[e.token] = true, now
					if e.outcome == 2 {
						installs[e.key] = e.lag
					}
				}
				m.settle(s)
			}
			running = still
			for k, n := range installs {
				if n--; n > 0 {
					installs[k] = n
					continue
				}
				delete(installs, k)
				census[k] = bringUpRow{Key: k, Token: "tok-" + k.ID, PendingCreate: true}
			}
			tokens := make(map[string]bool)
			rows := make([]bringUpRow, 0, len(census))
			for _, r := range census {
				if r.Token != "" {
					tokens[r.Token] = true
				}
				rows = append(rows, r)
			}
			m.clearVisible(inflightCensus{Tokens: tokens}, now)
			want := make(map[string]bool)
			for _, e := range running {
				if e.kind == inflightStart {
					want[identity(census[e.key])] = true
				} else {
					want["tok:"+e.token] = true
				}
			}
			for tok, at := range ambiguous {
				if tokens[tok] || now.Sub(at) >= inflightHardBound {
					delete(ambiguous, tok)
				} else {
					want["tok:"+tok] = true
				}
			}
			for _, r := range rows {
				if r.PendingCreate {
					want[identity(r)] = true
				}
			}
			if got, _ := cityInFlight(m.view(), rows, closed); got != len(want) {
				t.Fatalf("seed %d at %v: in flight %d, want %d (%v)", seed, now, got, len(want), want)
			}
		}
		for step := 0; step < 120; step++ {
			switch rng.IntN(4) {
			case 0:
				next++
				k := rowKey{"sessions", fmt.Sprintf("gc-%d", next)}
				e := &lagEffect{kind: inflightCreate, key: k, token: "tok-" + k.ID, runFor: 1 + rng.IntN(3), outcome: rng.IntN(4), lag: 1 + rng.IntN(25)}
				e.seq = m.add(inflightEntry{Kind: e.kind, Token: e.token})
				running = append(running, e)
			case 1:
				for _, k := range slices.SortedFunc(maps.Keys(census), func(a, b rowKey) int { return strings.Compare(a.ID, b.ID) }) {
					r := census[k]
					if !r.PendingCreate {
						continue
					}
					if seq := m.add(inflightEntry{Kind: inflightStart, Key: r.Key}); seq != 0 {
						running = append(running, &lagEffect{kind: inflightStart, key: r.Key, seq: seq, runFor: 1 + rng.IntN(3)})
					}
					break
				}
			default:
				pass()
			}
		}
		for i := 0; i < 40; i++ {
			pass()
		}
		if left := m.view().Entries; len(running) != 0 || len(left) != 0 {
			t.Fatalf("seed %d: at quiescence %d effects running and %d in-flight entries, want none (I8)", seed, len(running), len(left))
		}
	}
}
