package main

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

var admitT0 = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func admitRow(i int) rowKey { return rowKey{Leg: "sessions", ID: fmt.Sprintf("gc-%d", i)} }

// startOn is a start for row i on ep, ranked rank after admitT0.
func startOn(i int, ep endpointKey, rank time.Duration) intent {
	return intent{Kind: intentStart, Key: admitRow(i), Endpoint: ep, Rank: admitT0.Add(rank)}
}

func rowIntent(kind string, i int) intent { return intent{Kind: kind, Key: admitRow(i)} }

func poolCreate(template string, ep endpointKey) intent {
	return intent{Kind: intentCreate, Endpoint: ep, CreatePlan: createPlan{Template: template}}
}

func keysOf(its []intent) []string {
	out := make([]string, 0, len(its))
	for _, it := range its {
		out = append(out, it.Kind+":"+it.Key.ID)
	}
	return out
}

// admitIn is a pass at admitT0 with the boot gate open, a full bucket of
// maxWakes tokens and probe_concurrency probes.
func admitIn(maxWakes, probes int) admitInput {
	cfg := &config.City{Daemon: config.DaemonConfig{MaxWakesPerTick: &maxWakes, ProbeConcurrency: &probes}}
	return admitInput{Now: admitT0, Cfg: cfg, Bucket: bucketState{Tokens: maxWakes, LastRefill: admitT0}, BootOpen: true}
}

func causesOf(its []intent) map[string]string {
	out := make(map[string]string)
	for _, it := range its {
		out[it.Kind+":"+it.Key.ID] = it.Cause
	}
	return out
}

// Kills (I1): admission past the city cap or the create cap over random
// worlds: an admitted bring-up not counted, a probe-gated backlog counted
// per row, an ambiguous create dropped. Admission never grows the bring-up
// count past max(its count before, max_wakes_per_tick); running starts stay
// within the cap whenever a start was admitted (a counted row's start takes
// no further slot, P4); and admit is deterministic.
func TestAdmitNeverExceedsCityCap(t *testing.T) {
	eps := []endpointKey{"", "provider:a", "provider:b", "provider:c"}
	for seed := uint64(1); seed <= 500; seed++ {
		rng := rand.New(rand.NewPCG(seed, 11))
		in := admitIn(1+rng.IntN(6), 4)
		in.Bucket.Tokens = rng.IntN(8)
		in.Endpoints = map[endpointKey]endpointView{}
		for _, ep := range eps[1:] {
			in.Endpoints[ep] = endpointView{Gate: endpointGate(rng.IntN(3))}
		}
		var intents []intent
		for i := 0; i < 12; i++ {
			ep, k := eps[rng.IntN(len(eps))], admitRow(i)
			switch rng.IntN(5) {
			case 0:
				in.InFlight.Entries = append(in.InFlight.Entries, inflightEntry{Kind: intentStart, Key: k, Endpoint: ep})
			case 1:
				e := inflightEntry{Kind: intentCreate, Token: fmt.Sprintf("tok-%d", i), Endpoint: ep, Ambiguous: rng.IntN(2) == 0}
				in.InFlight.Entries = append(in.InFlight.Entries, e)
				if rng.IntN(2) == 0 {
					in.BringUp = append(in.BringUp, bringUpRow{Key: k, Token: e.Token, Endpoint: ep, PendingCreate: true})
				}
			case 2:
				in.BringUp = append(in.BringUp, bringUpRow{Key: k, Endpoint: ep, PendingCreate: true})
				intents = append(intents, startOn(i, ep, time.Duration(rng.IntN(100))*time.Second))
			case 3:
				intents = append(intents, startOn(i, ep, time.Duration(rng.IntN(100))*time.Second))
			default:
				intents = append(intents, poolCreate(fmt.Sprintf("t%d", rng.IntN(3)), ep))
			}
		}
		res := admit(in, intents)
		if again := admit(in, intents); !reflect.DeepEqual(again, res) {
			t.Fatalf("seed %d: admit is nondeterministic", seed)
		}
		after := inflightView{Entries: slices.Clone(in.InFlight.Entries)}
		for i, it := range res.Admitted {
			after.Entries = append(after.Entries, inflightEntry{Kind: it.Kind, Key: it.Key, Endpoint: it.Endpoint, Token: fmt.Sprintf("new-%d", i)})
		}
		gate := (&admission{in: in}).gate
		before, _ := cityInFlight(in.InFlight, in.BringUp, gate)
		n, _ := cityInFlight(after, in.BringUp, gate)
		limit := in.Cfg.Daemon.MaxWakesPerTickOrDefault()
		if n > max(before, limit) {
			t.Fatalf("seed %d: %d bring-up rows in flight after admitting %v (%d before), cap %d", seed, n, keysOf(res.Admitted), before, limit)
		}
		running := map[string]int{}
		for _, e := range after.Entries {
			if !e.Ambiguous {
				running[e.Kind]++
			}
		}
		admittedStart := slices.ContainsFunc(res.Admitted, func(it intent) bool { return it.Kind == intentStart })
		if running[intentCreate] > createsInFlightCap || (admittedStart && running[intentStart] > limit) {
			t.Fatalf("seed %d: %v running, cap %d", seed, running, limit)
		}
	}
}

// Kills double counting (P4): a running start for a row holding a
// pending-create claim, and a create whose token a census row carries, each
// count once; an ambiguous create no row carries yet counts; a start for a
// counted row takes no further slot.
func TestAdmitCountsDistinctBringUpRowsOnce(t *testing.T) {
	in := admitIn(4, 4)
	in.InFlight.Entries = []inflightEntry{
		{Kind: intentStart, Key: admitRow(1)},
		{Kind: intentCreate, Token: "tok-2"},
		{Kind: intentCreate, Token: "tok-5", Ambiguous: true},
	}
	in.BringUp = []bringUpRow{
		{Key: admitRow(1), PendingCreate: true},
		{Key: admitRow(2), Token: "tok-2", PendingCreate: true},
		{Key: admitRow(3), PendingCreate: true},
	}
	if n, _ := cityInFlight(in.InFlight, in.BringUp, (&admission{in: in}).gate); n != 4 {
		t.Fatalf("in flight = %d, want 4: rows 1, 2 and 3 once each, and tok-5", n)
	}
	res := admit(in, []intent{startOn(3, "", 0), startOn(4, "", time.Second)})
	if got := keysOf(res.Admitted); fmt.Sprint(got) != "[start:gc-3]" || res.Deferred[0].Cause != causeCityCap {
		t.Fatalf("admitted %v deferred %v: row 3's start continues its counted slot, row 4 has none", got, res.Deferred)
	}
}

// Kills a lease creeping back (v5 P4, SC A5): a creating row that woke a
// second ago, with no pending-create claim and no running start, takes no
// slot.
func TestAdmitNoLeaseInput(t *testing.T) {
	row := censusSession("gc-woke", map[string]string{"state": "creating", "last_woke_at": admitT0.Add(-time.Second).Format(time.RFC3339), "instance_token": "tok"})
	c := readCensus(t, admitT0, censusLegs("sessions", censusStore(row)))
	in := admitIn(1, 4)
	in.BringUp = c.BringUp(&config.City{})
	if res := admit(in, []intent{startOn(1, "", 0)}); len(res.Admitted) != 1 {
		t.Fatalf("deferred %v: a fresh last_woke_at held the only city slot", res.Deferred)
	}
}

// Kills I8's restart stall: after a restart the in-flight map is empty, and
// k rows PreWaked before it (creating, a fresh token and last_woke_at, no
// claim) do not hold admission for startup_timeout + 7s.
func TestRestartMidStartFreesCapAtOnce(t *testing.T) {
	const k = 4
	var rows []beads.Bead
	var intents []intent
	for i := 0; i < k; i++ {
		rows = append(rows, censusSession(fmt.Sprintf("gc-pw%d", i), map[string]string{
			"state": "creating", "generation": "2", "instance_token": fmt.Sprintf("tok-%d", i), "last_woke_at": admitT0.Format(time.RFC3339),
		}))
		intents = append(intents, startOn(10+i, "", 0))
	}
	in := admitIn(k, 4)
	in.BringUp = readCensus(t, admitT0, censusLegs("sessions", censusStore(rows...))).BringUp(&config.City{})
	if res := admit(in, intents); len(res.Admitted) != k {
		t.Fatalf("admitted %d of %d starts right after the restart; deferred %v", len(res.Admitted), k, res.Deferred)
	}
}

// Kills a half-open endpoint filling the city cap with its backlog: 50
// pending rows behind a probe gate count once, so a healthy endpoint still
// starts; one of them may take the probe without a further slot, and a create
// behind the gate waits for the probe.
func TestAdmitProbeEndpointPendingCreatesCountOnce(t *testing.T) {
	probeIn := func(maxWakes int) admitInput {
		in := admitIn(maxWakes, 4)
		in.Endpoints = map[endpointKey]endpointView{"provider:a": {Gate: gateProbe}, "provider:b": {Gate: gateClosed}}
		for i := 0; i < 50; i++ {
			in.BringUp = append(in.BringUp, bringUpRow{Key: admitRow(i), Endpoint: "provider:a", PendingCreate: true})
		}
		return in
	}
	res := admit(probeIn(4), []intent{startOn(100, "provider:b", 0), startOn(101, "provider:b", 0), poolCreate("w", "provider:a")})
	if got := keysOf(res.Admitted); fmt.Sprint(got) != "[start:gc-100 start:gc-101]" || res.Deferred[0].Cause != causeEndpointGate {
		t.Fatalf("admitted %v deferred %v", got, res.Deferred)
	}
	if res := admit(probeIn(3), []intent{startOn(7, "provider:a", 0), startOn(100, "provider:b", 0), startOn(101, "provider:b", 0)}); len(res.Admitted) != 3 {
		t.Fatalf("deferred %v: the probe start took a second slot for the backlog", res.Deferred)
	}
	in := probeIn(1)
	in.InFlight.Entries = []inflightEntry{{Kind: intentStart, Key: admitRow(7), Endpoint: "provider:a"}}
	if n, _ := cityInFlight(in.InFlight, in.BringUp, (&admission{in: in}).gate); n != 1 {
		t.Fatalf("in flight = %d with the probe running, want 1: it is the backlog's one slot", n)
	}
}

// Kills a refund or a double debit (I9), including a start for an already
// counted row that debits nothing: with demand always above the budget, the
// starts admitted over any run of passes are exactly the capacity plus the
// tokens refilled, never more.
func TestAdmitTokensBound(t *testing.T) {
	const capacity, interval = 3, 30 * time.Second
	rng := rand.New(rand.NewPCG(9, 9))
	in := admitIn(capacity, 4)
	in.Bucket, in.Now = bucketState{}, admitT0
	var demand []intent
	for i := 0; i < 10; i++ {
		demand = append(demand, startOn(i, "", 0))
		if i < 5 {
			in.BringUp = append(in.BringUp, bringUpRow{Key: admitRow(i), PendingCreate: true})
		}
	}
	started := 0
	for pass := 0; pass < 200; pass++ {
		res := admit(in, demand)
		started += len(res.Admitted)
		elapsed := in.Now.Sub(admitT0)
		if want := capacity + int(elapsed/(interval/capacity)); started != want {
			t.Fatalf("pass %d at +%v: %d starts admitted, want %d", pass, elapsed, started, want)
		}
		in.Bucket, in.Now = res.Bucket, in.Now.Add(time.Duration(rng.IntN(20_000))*time.Millisecond)
	}
}

// Kills an adopt debiting a token or waiting on the bucket (P4, S1).
func TestAdmitAdoptCostsNoToken(t *testing.T) {
	in := admitIn(2, 4)
	in.Bucket.Tokens = 0
	res := admit(in, []intent{rowIntent(intentAdopt, 1), startOn(2, "", 0)})
	if got := keysOf(res.Admitted); fmt.Sprint(got) != "[adopt:gc-1]" || res.Deferred[0].Cause != causeAwaitingBudget || res.Bucket.Tokens != 0 {
		t.Fatalf("admitted %v deferred %v bucket %+v", got, res.Deferred, res.Bucket)
	}
	// The starved start reports the next token: 30s patrol over 2 tokens.
	if !res.NextToken.Equal(admitT0.Add(15 * time.Second)) {
		t.Fatalf("NextToken = %v, want admitT0+15s", res.NextToken)
	}
	if res := admit(in, []intent{rowIntent(intentAdopt, 1)}); !res.NextToken.IsZero() {
		t.Fatalf("NextToken = %v with no start waiting, want zero", res.NextToken)
	}
}

// Kills a half-open endpoint admitting a herd, or a second probe while a
// start or create for it runs.
func TestAdmitHalfOpenAdmitsOneProbe(t *testing.T) {
	in := admitIn(5, 4)
	in.Endpoints = map[endpointKey]endpointView{"provider:a": {Gate: gateProbe}}
	if res := admit(in, []intent{startOn(1, "provider:a", 0), startOn(2, "provider:a", time.Second)}); len(res.Admitted) != 1 {
		t.Fatalf("admitted %v: a half-open endpoint admits one probe", keysOf(res.Admitted))
	}
	if res := admit(in, []intent{poolCreate("w", "provider:a"), poolCreate("w", "provider:a")}); len(res.Admitted) != 1 {
		t.Fatalf("admitted %d creates behind one half-open endpoint, want one", len(res.Admitted))
	}
	// Probe rotation: the row refused most this episode goes last.
	in.Endpoints["provider:a"] = endpointView{Gate: gateProbe, Refusals: map[string]int{"gc-1": 3}}
	if res := admit(in, []intent{startOn(1, "provider:a", 0), startOn(2, "provider:a", 0)}); fmt.Sprint(keysOf(res.Admitted)) != "[start:gc-2]" {
		t.Fatalf("admitted %v, want gc-2: gc-1 was refused three times", keysOf(res.Admitted))
	}
	in.InFlight.Entries = []inflightEntry{{Kind: intentCreate, Token: "tok", Endpoint: "provider:a"}}
	if res := admit(in, []intent{startOn(1, "provider:a", 0)}); len(res.Admitted) != 0 {
		t.Fatalf("admitted %v while the probe's create runs", keysOf(res.Admitted))
	}
}

// Kills a fair share sized past the create cap, which hands every create
// slot to the first template: 10 creates each for a and b, with tokens and
// city slots for all, split evenly over the eight create slots whatever the
// seed.
func TestAdmitFairShareSurvivesTheCreateCap(t *testing.T) {
	var intents []intent
	for _, template := range []string{"a", "b"} {
		for i := 0; i < 10; i++ {
			intents = append(intents, poolCreate(template, ""))
		}
	}
	for seed := uint64(0); seed < 4; seed++ {
		in := admitIn(20, 4)
		in.FairSeed = seed
		per := map[string]int{}
		for _, it := range admit(in, intents).Admitted {
			per[it.CreatePlan.Template]++
		}
		if per["a"] != 4 || per["b"] != 4 {
			t.Fatalf("seed %d: admitted %v, want 4 each", seed, per)
		}
	}
}

// Kills starts for already-counted rows wedged by the bring-up count (P4: a
// start for a counted row takes no further slot): five claimed rows exceed a
// cap of 2, as after a reload lowered it; their starts are limited by the
// running starts instead, while a new row still waits on the bring-up count.
func TestAdmitCountedRowStartsLimitedByRunningStarts(t *testing.T) {
	in := admitIn(2, 4)
	for i := 0; i < 5; i++ {
		in.BringUp = append(in.BringUp, bringUpRow{Key: admitRow(i), PendingCreate: true})
	}
	in.InFlight.Entries = []inflightEntry{{Kind: intentStart, Key: admitRow(4)}}
	res := admit(in, []intent{startOn(0, "", 0), startOn(1, "", 0), startOn(9, "", 0)})
	causes := causesOf(res.Deferred)
	if fmt.Sprint(keysOf(res.Admitted)) != "[start:gc-0]" || causes["start:gc-1"] != causeCityCap || causes["start:gc-9"] != causeCityCap {
		t.Fatalf("admitted %v deferred %v", keysOf(res.Admitted), causes)
	}
}

// Kills admission failing open: an unknown kind is deferred, a running kind
// the table does not know counts against the probe cap, and a second intent
// for one row in a pass is deferred (R5).
func TestAdmitFailsClosedOnUnknownKindsAndRepeatedRows(t *testing.T) {
	in := admitIn(5, 1)
	in.InFlight.Entries = []inflightEntry{{Kind: "kind-from-a-later-pr", Key: admitRow(99)}}
	res := admit(in, []intent{rowIntent("bogus", 1), rowIntent(intentClose, 2), rowIntent(intentRowHeal, 3), rowIntent(intentRowMetadata, 3)})
	causes := causesOf(res.Deferred)
	if causes["bogus:gc-1"] != causeUnknownKind || causes["close:gc-2"] != causeProbeCap || causes["row-metadata:gc-3"] != causeRowRepeated {
		t.Fatalf("admitted %v deferred %v", keysOf(res.Admitted), causes)
	}
}

// Kills an open endpoint admitting a start or a create.
func TestAdmitOpenEndpointAdmitsNothing(t *testing.T) {
	in := admitIn(5, 4)
	in.Endpoints = map[endpointKey]endpointView{"provider:a": {Gate: gateShut}}
	res := admit(in, []intent{startOn(1, "provider:a", 0), poolCreate("w", "provider:a")})
	if len(res.Admitted) != 0 || causesOf(res.Deferred)["start:gc-1"] != causeEndpointGate {
		t.Fatalf("admitted %v deferred %v", keysOf(res.Admitted), res.Deferred)
	}
}

// Kills rank inversion (START-009): starts go oldest wakeFairnessTime first,
// and a never-woken row ranks by its CreatedAt.
func TestAdmitLRUOrderUsesWakeFairnessTime(t *testing.T) {
	rank := func(info session.Info) time.Time { return wakeFairnessTime(startCandidate{info: info}) }
	woke := func(d time.Duration) string { return admitT0.Add(-d).Format(time.RFC3339) }
	a := intent{Kind: intentStart, Key: admitRow(1), Rank: rank(session.Info{LastWokeAt: woke(time.Hour)})}
	never := intent{Kind: intentStart, Key: admitRow(2), Rank: rank(session.Info{CreatedAt: admitT0.Add(-2 * time.Hour)})}
	recent := intent{Kind: intentStart, Key: admitRow(3), Rank: rank(session.Info{LastWokeAt: woke(time.Minute)})}
	res := admit(admitIn(2, 4), []intent{recent, a, never})
	if got := keysOf(res.Admitted); fmt.Sprint(got) != "[start:gc-2 start:gc-1]" {
		t.Fatalf("admitted %v, want the never-woken row (created 2h ago) then the row woken 1h ago", got)
	}
}

// Kills a create admitted when its row's first start could not be: no token
// left after the starts, the city cap reached, or its endpoint shut.
func TestAdmitCreateOnlyIfStartAdmissible(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*admitInput)
		cause  string
	}{
		"no token left": {func(in *admitInput) { in.Bucket.Tokens = 1 }, causeAwaitingBudget},
		"city cap":      {func(in *admitInput) { in.BringUp = []bringUpRow{{Key: admitRow(9), PendingCreate: true}} }, causeCityCap},
		"endpoint shut": {func(in *admitInput) { in.Endpoints = map[endpointKey]endpointView{"provider:a": {}} }, causeEndpointGate},
	} {
		in := admitIn(2, 4)
		tc.mutate(&in)
		res := admit(in, []intent{startOn(1, "", 0), poolCreate("w", "provider:a")})
		if len(res.Deferred) != 1 || res.Deferred[0].Kind != intentCreate || res.Deferred[0].Cause != tc.cause || res.FairSeed != 0 {
			t.Errorf("%s: admitted %v deferred %+v, want the create deferred %s", name, keysOf(res.Admitted), res.Deferred, tc.cause)
		}
	}
	in := admitIn(2, 4)
	in.Endpoints = map[endpointKey]endpointView{"provider:a": {Gate: gateClosed}}
	if res := admit(in, []intent{startOn(1, "", 0), poolCreate("w", "provider:a")}); len(res.Admitted) != 2 || res.Bucket.Tokens != 1 {
		t.Fatalf("admitted %v bucket %+v: a create holds its row's token but debits none", keysOf(res.Admitted), res.Bucket)
	}
	in.Bucket.Tokens = 1
	if res := admit(in, []intent{poolCreate("w", ""), poolCreate("w", "")}); len(res.Admitted) != 1 || res.Deferred[0].Cause != causeAwaitingBudget || res.FairSeed != 1 {
		t.Fatalf("admitted %d deferred %v seed %d: two creates shared one token, or the seed stood still", len(res.Admitted), res.Deferred, res.FairSeed)
	}
	named := intent{Kind: intentCreate, CreatePlan: createPlan{Named: &namedCreatePlan{Identity: "boss"}}}
	if res := admit(in, []intent{poolCreate("w", ""), named}); len(res.Admitted) != 1 || !res.Admitted[0].named() || res.FairSeed != 0 {
		t.Fatalf("admitted %+v seed %d: a named create goes first, and no pool create advanced the seed", res.Admitted, res.FairSeed)
	}
}

// Kills creates past eight running; an ambiguous create is not running.
func TestAdmitCreatesCappedAtEight(t *testing.T) {
	in := admitIn(100, 4)
	for i := 0; i < 8; i++ {
		in.InFlight.Entries = append(in.InFlight.Entries, inflightEntry{Kind: intentCreate, Token: fmt.Sprint(i), Ambiguous: i == 0})
	}
	res := admit(in, []intent{poolCreate("w", ""), poolCreate("w", ""), poolCreate("w", "")})
	if len(res.Admitted) != 1 || res.Deferred[0].Cause != causeCreateCap {
		t.Fatalf("admitted %d deferred %v: 7 running plus 1, want the rest create-capped", len(res.Admitted), res.Deferred)
	}
}

// Kills a start or create admitted during a provider swap (P7), and the
// pause holding anything else.
func TestAdmitPausedDefersStartsAndCreates(t *testing.T) {
	in := admitIn(5, 4)
	in.Paused = true
	res := admit(in, []intent{startOn(1, "", 0), poolCreate("w", ""), rowIntent(intentAdopt, 2), rowIntent(intentRowHeal, 3)})
	causes := causesOf(res.Deferred)
	if len(res.Admitted) != 2 || causes["start:gc-1"] != causeSwapPause || causes["create:"] != causeSwapPause {
		t.Fatalf("admitted %v deferred %v", keysOf(res.Admitted), causes)
	}
}

// Kills a destructive effect before the inputs are primed, and gating a
// start, adopt or rekey on the boot gate (P2): exactly the six destructive
// families defer, with cause boot-gate.
func TestAdmitBootGateClosedDefersExactlyTheSixKinds(t *testing.T) {
	gated := []string{intentClose, intentDrainBegin, intentDrainBeginFresh, intentRollback, intentSignal, intentSignalFresh, intentStop, intentZombie}
	in := admitIn(50, 50)
	in.BootOpen = false
	var intents []intent
	for i, kind := range slices.Sorted(maps.Keys(intentKinds)) {
		it := rowIntent(kind, i)
		if kind == intentCreate {
			it = poolCreate("w", "")
		}
		intents = append(intents, it)
	}
	res := admit(in, intents)
	var got []string
	for _, it := range res.Deferred {
		if it.Cause != causeBootGate {
			t.Errorf("%s deferred %s with the gate closed", it.Kind, it.Cause)
		}
		got = append(got, it.Kind)
	}
	if slices.Sort(got); !slices.Equal(got, gated) {
		t.Fatalf("boot-gated kinds = %v, want %v", got, gated)
	}
	finalize := rowIntent(intentStop, 1)
	finalize.Finalize = true
	if res := admit(in, []intent{finalize}); len(res.Deferred) != 1 || res.Deferred[0].Cause != causeBootGate {
		t.Fatalf("a finalize stop passed the closed boot gate: admitted %v", keysOf(res.Admitted))
	}
}

// capCheck proposes five intents of kind with probe_concurrency 3, one of
// its class running and one of the other class: two more are admitted, the
// rest deferred with cause.
func capCheck(t *testing.T, kind, running, other, cause string) {
	t.Helper()
	in := admitIn(5, 3)
	in.InFlight.Entries = []inflightEntry{{Kind: running, Key: admitRow(99)}, {Kind: other, Key: admitRow(98)}}
	var intents []intent
	for i := 0; i < 5; i++ {
		intents = append(intents, rowIntent(kind, i))
	}
	res := admit(in, intents)
	if len(res.Admitted) != 2 || len(res.Deferred) != 3 || res.Deferred[0].Cause != cause {
		t.Fatalf("%s: admitted %v deferred %v", kind, keysOf(res.Admitted), res.Deferred)
	}
}

// Kills a goroutine storm on a mass heal: row writes cap at
// probe_concurrency, counted apart from probing effects.
func TestAdmitRowWritesCappedAtProbeConcurrency(t *testing.T) {
	capCheck(t, intentRowHeal, intentBaseline, intentClose, causeRowWriteCap)
}

// Kills a tmux storm on a city-wide suspend: probing effects cap at
// probe_concurrency, counted apart from row writes.
func TestAdmitProbeEffectsCappedAtProbeConcurrency(t *testing.T) {
	capCheck(t, intentStop, intentAdopt, intentRowMetadata, causeProbeCap)
}

// Kills a reintroduced special deadline (P3): every kind's deadline is its
// class's, a start startup_timeout + 10s, a row write 30s, the rest 60s.
func TestIntentDeadlinesAreFlat(t *testing.T) {
	rowWrites := []string{intentBaseline, intentDrainBegin, intentDrainCancel, intentDrainVoid, intentRowHeal, intentRowMetadata, intentSignal}
	in := admitIn(50, 50)
	in.Cfg.Session.StartupTimeout = "2m"
	for kind := range intentKinds {
		it := rowIntent(kind, 1)
		if kind == intentCreate {
			it = poolCreate("w", "")
		}
		res := admit(in, []intent{it})
		want := providerEffectDeadline
		switch {
		case kind == intentStart:
			want = 2*time.Minute + 10*time.Second
		case slices.Contains(rowWrites, kind):
			want = 30 * time.Second
		}
		if len(res.Admitted) != 1 || res.Admitted[0].Deadline.Sub(admitT0) != want {
			t.Errorf("%s: admitted %+v, want deadline +%v", kind, res.Admitted, want)
		}
	}
	// A startup_timeout that is not positive reads as the 60s default.
	for _, timeout := range []string{"0s", "-5s"} {
		in.Cfg.Session.StartupTimeout = timeout
		if res := admit(in, []intent{startOn(1, "", 0)}); res.Admitted[0].Deadline.Sub(admitT0) != 70*time.Second {
			t.Errorf("startup_timeout %s: deadline +%v, want +70s", timeout, res.Admitted[0].Deadline.Sub(admitT0))
		}
	}
}

// Kills a backed-off row re-stopped every pass, a finalize that waits on
// another refusal's backoff, and one that ignores its own (P4): a live row
// backoff defers every intent for its row except the stop verb proposed for
// a gone runtime, unless the finalize recorded it; an expired one defers
// nothing.
func TestAdmitRowBackoffDefersAllButGoneFinalize(t *testing.T) {
	in := admitIn(50, 50)
	in.Backoff = map[string]backoffRecord{rowBackoffKey(admitRow(1)): {Until: admitT0.Add(time.Second), Cause: "fence-l3"}}
	for kind := range intentKinds {
		if kind == intentCreate {
			continue
		}
		if res := admit(in, []intent{rowIntent(kind, 1)}); len(res.Deferred) != 1 || res.Deferred[0].Cause != causeRowBackoff {
			t.Errorf("%s: admitted under a live row backoff", kind)
		}
	}
	finalize := rowIntent(intentStop, 1)
	finalize.Finalize = true
	if res := admit(in, []intent{finalize}); len(res.Admitted) != 1 {
		t.Fatalf("the gone row's finalize waited on the backoff: %v", res.Deferred)
	}
	own := in
	own.Backoff = map[string]backoffRecord{rowBackoffKey(admitRow(1)): {Until: admitT0.Add(time.Second), Cause: causeFinalizePrefix + "c8.8"}}
	if res := admit(own, []intent{finalize}); len(res.Deferred) != 1 || res.Deferred[0].Cause != causeRowBackoff {
		t.Fatalf("the finalize ignored its own backoff: admitted %v", keysOf(res.Admitted))
	}
	in.Now = admitT0.Add(time.Second)
	if res := admit(in, []intent{rowIntent(intentStop, 1)}); len(res.Admitted) != 1 {
		t.Fatalf("an expired backoff deferred the stop: %v", res.Deferred)
	}
}

// Kills a re-probe storm (mc-zndi7.29): an attribution-unknown refusal's row
// backoff defers the row's adopt, start and rollback until it expires or a
// readable verdict drops it.
func TestAdmitDefersAttributionReprobeUnderBackoff(t *testing.T) {
	table := newBackoffTable()
	table.Refuse(rowBackoffKey(admitRow(1)), admitT0, time.Time{}, "attribution-unknown", "")
	in := admitIn(5, 5)
	probes := []intent{rowIntent(intentAdopt, 1), startOn(1, "", 0), rowIntent(intentRollback, 1)}
	admitted := func(in admitInput) (n int) { // one intent per row per pass (R5)
		for _, it := range probes {
			n += len(admit(in, []intent{it}).Admitted)
		}
		return n
	}
	in.Backoff = table.Snapshot()
	if n := admitted(in); n != 0 {
		t.Fatalf("%d re-probes under the attribution backoff", n)
	}
	in.Now = admitT0.Add(backoffBase)
	if n := admitted(in); n != 3 {
		t.Fatalf("%d of 3 admitted after the backoff", n)
	}
	table.Succeed(rowBackoffKey(admitRow(1)))
	in.Now, in.Backoff = admitT0, table.Snapshot()
	if n := admitted(in); n != 3 {
		t.Fatalf("%d of 3 admitted after a readable verdict reset it", n)
	}
}
