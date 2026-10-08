package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/session"
)

// The effect transaction's tests (simplify/EFFECT-STRUCTURE.md §2.1) on the
// effect test kit: one session row on a CachingStore over a stamped,
// revisioned MemStore (the simulator's leg) and the pass that decided on
// it. Effect tests build on it rather than on a fake leaf of their own.

// lockObserver counts, per row, the sections holding its session mutation
// lock, through withRowMutationLock.
type lockObserver struct {
	mu   sync.Mutex
	held map[string]int
}

// observeRowLocks wraps withRowMutationLock in a new observer for the test,
// which must not run in parallel.
func observeRowLocks(t *testing.T) *lockObserver {
	t.Helper()
	o := &lockObserver{held: make(map[string]int)}
	saved := withRowMutationLock
	withRowMutationLock = func(id string, fn func() error) error {
		return saved(id, func() error {
			o.add(id, 1)
			defer o.add(id, -1)
			return fn()
		})
	}
	t.Cleanup(func() { withRowMutationLock = saved })
	return o
}

func (o *lockObserver) add(id string, n int) {
	o.mu.Lock()
	o.held[id] += n
	o.mu.Unlock()
}

func (o *lockObserver) holds(id string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.held[id] > 0
}

// nameLocked reports whether city's runtime name lock on name is held.
func nameLocked(city, name string) bool {
	runtimeNames.mu.Lock()
	defer runtimeNames.mu.Unlock()
	return runtimeNames.held[runtimeNameKey{city, name}]
}

// txKit is the kit: row gc-1, runtime name s-gc-1, the pass, its runtime
// (the simulator's cache-aware provider behind a recording leaf), and the
// outside operations queued per seam, each run once, in order.
type txKit struct {
	t        *testing.T
	backing  *beads.MemStore
	cache    *beads.CachingStore
	p        *effectPass
	it       intent
	locks    *lockObserver
	sp       *simProvider
	leaf     *recordingLeaf
	ops      map[txSeam][]func()
	seen     []txSeam
	sections []int            // each seam's section index, as seen
	attempts []int            // each seam's attempt, as seen
	fail     map[txSeam]error // a seam that fails the effect
}

// on queues op for the next time the transaction reaches seam at.
func (k *txKit) on(at txSeam, op func()) { k.ops[at] = append(k.ops[at], op) }

func newTxKit(t *testing.T) *txKit {
	t.Helper()
	m := beads.NewMemStoreFrom(0, []beads.Bead{poolRow("gc-1", "worker", 1, "asleep")}, nil)
	if err := beads.StampOpenedStore(m, "MemStore", gate.Require, nil, nil); err != nil {
		t.Fatal(err)
	}
	cache := beads.NewCachingStoreForTest(simBacking{m}, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	k := &txKit{t: t, backing: m, cache: cache, locks: observeRowLocks(t), sp: newSimProvider(), ops: make(map[txSeam][]func())}
	k.leaf = &recordingLeaf{simProvider: k.sp}
	w := &World{
		Now: gatherNow, CityPath: t.TempDir(), Env: &reconcileEnv{Gen: 1, Cfg: workerCity(1)},
		Census: readCensus(t, gatherNow, censusLegs(rowLeg, cache)), Mislabelled: map[rowKey]bool{},
		LegStores: map[string]beads.Store{rowLeg: cache}, SessionsStore: cache,
	}
	k.p = newEffectPass(w, &allocDecision{Snapshot: &selectionSnapshot{Entries: map[rowKey]*selectionEntry{}}})
	k.p.Clock, k.p.Runtime = newFakePlannerClock(gatherNow), k.leaf
	k.p.seam = func(_ context.Context, at txSeam, _ intent, section, attempt int) error {
		k.seen, k.sections, k.attempts = append(k.seen, at), append(k.sections, section), append(k.attempts, attempt)
		if ops := k.ops[at]; len(ops) > 0 {
			k.ops[at] = ops[1:]
			ops[0]()
		}
		return k.fail[at]
	}
	key := rowKey{Leg: rowLeg, ID: "gc-1"}
	row := k.p.World.Census.Rows[key]
	tp := TemplateParams{TemplateName: "worker"}
	tp.Hints.ProcessNames = []string{"agent"}
	k.p.World.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(row.Info): {TP: tp}}}
	k.it = intent{Kind: "tx-test", Key: key, Basis: rowBasis{Incarnation: row.Incarnation, InstanceToken: row.InstanceToken}}
	return k
}

// outside writes kv to the row's backing as another process does: the
// cache sees nothing until an event or a read from the backing.
func (k *txKit) outside(kv ...string) {
	k.t.Helper()
	m := make(map[string]string)
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	if err := k.backing.SetMetadataBatch(k.it.Key.ID, m); err != nil {
		k.t.Fatal(err)
	}
}

func (k *txKit) meta(key string) string {
	k.t.Helper()
	b, err := k.backing.Get(k.it.Key.ID)
	if err != nil {
		k.t.Fatal(err)
	}
	return b.Metadata[key]
}

func (k *txKit) run(ctx context.Context, spec effectSpec) settlement {
	return runTx(ctx, k.p, k.it, spec, nil)
}

// mark is a Decide that writes key=1.
func mark(key string) func(txView) txStep {
	return func(txView) txStep { return txStep{Write: session.MetadataPatch{key: "1"}} }
}

// Kills a section outside the row's session mutation lock, a name lock not
// held across the sections or kept after, and a busy name waited on: each
// section decides under both locks; both writes land; neither lock outlives
// the transaction; a busy name refuses.
func TestTxLockScope(t *testing.T) {
	k := newTxKit(t)
	var problems []string
	decide := func(key string) func(txView) txStep {
		return func(v txView) txStep {
			if !k.locks.holds(k.it.Key.ID) || !nameLocked(k.p.World.CityPath, "s-gc-1") {
				problems = append(problems, key)
			}
			return mark(key)(v)
		}
	}
	spec := effectSpec{needs: needs{NameLock: true}, sections: []section{{Decide: decide("a")}, {Decide: decide("b")}}}
	if s := k.run(context.Background(), spec); s.Outcome != settledLanded || len(problems) > 0 || k.meta("a") != "1" || k.meta("b") != "1" {
		t.Fatalf("settlement %+v, unlocked sections %v, a=%q b=%q; want both locks while deciding, both writes", s, problems, k.meta("a"), k.meta("b"))
	}
	if nameLocked(k.p.World.CityPath, "s-gc-1") || k.locks.holds(k.it.Key.ID) {
		t.Fatal("a lock outlived the transaction")
	}
	unlock := runtimeNames.tryLock(k.p.World.CityPath, "s-gc-1")
	defer unlock()
	if s := k.run(context.Background(), spec); s.Outcome != settledRefused || s.Cause != causeNameBusy {
		t.Fatalf("busy name: settlement %+v, want refused with cause %q", s, causeNameBusy)
	}
}

// Kills a single-attempt CAS, a retry that does not decide again, and an
// unbounded one: a write behind the cache that lands after Decide and before
// the CAS loses the first attempt, and the second reads the row again,
// decides again and lands, keeping the outside write; a row that changes
// before every CAS refuses cas after Attempts decisions, writing nothing.
func TestTxRetriesALostCASOnAFreshRead(t *testing.T) {
	for _, c := range []struct {
		attempts, races, decides int
		landed                   bool
	}{{0, 1, 2, true}, {0, 5, 3, false}, {1, 5, 1, false}, {5, 5, 5, false}} {
		k := newTxKit(t)
		decides := 0
		spec := effectSpec{needs: needs{Attempts: c.attempts}, sections: []section{{Decide: func(v txView) txStep {
			if decides++; decides <= c.races {
				k.outside("note", string(rune('a'+decides)))
			}
			return mark("a")(v)
		}}}}
		s := k.run(context.Background(), spec)
		if decides != c.decides || (s.Outcome == settledLanded) != c.landed || (!c.landed && s.Cause != causeCAS) || (k.meta("a") == "1") != c.landed || k.meta("note") == "" {
			t.Fatalf("attempts %d, %d races: settlement %+v after %d decisions, a=%q; want landed %t after %d", c.attempts, c.races, s, decides, k.meta("a"), c.landed, c.decides)
		}
	}
}

// Kills a decision on the cached row: a write behind the cache (its event
// not yet delivered) is what Decide sees, on its first attempt.
func TestTxDecidesOnTheBackingRow(t *testing.T) {
	k := newTxKit(t)
	k.outside("note", "behind")
	var seen []string
	spec := effectSpec{sections: []section{{Decide: func(v txView) txStep {
		seen = append(seen, v.Meta["note"])
		return mark("a")(v)
	}}}}
	if s := k.run(context.Background(), spec); s.Outcome != settledLanded || !slices.Equal(seen, []string{"behind"}) {
		t.Fatalf("settlement %+v, Decide saw note %q; want the backing's on its one attempt", s, seen)
	}
}

// Kills reads inside the CAS window: a write behind the cache that lands
// while the runtime is read does not cost the attempt its CAS, since the row
// is read after the runtime.
func TestTxReadsTheRuntimeBeforeTheRow(t *testing.T) {
	k := newTxKit(t)
	once := false
	k.leaf.during = func() {
		if !once {
			once = true
			k.outside("note", "during-read")
		}
	}
	decides := 0
	spec := effectSpec{needs: needs{Runtime: true}, sections: []section{{Decide: func(v txView) txStep { decides++; return mark("a")(v) }}}}
	if s := k.run(context.Background(), spec); s.Outcome != settledLanded || decides != 1 {
		t.Fatalf("settlement %+v after %d decisions, want landed on the first", s, decides)
	}
}

// Kills a runtime read stamped once per section, or without the template's
// process names: each attempt reads the runtime since that attempt began,
// with the names of the row's template; an unresolved route refuses.
func TestTxReadsTheRuntimeEachAttempt(t *testing.T) {
	k := newTxKit(t)
	k.leaf.during = func() { k.p.Clock.(*fakePlannerClock).Advance(time.Second) }
	lost := false
	var rt *txRuntime
	spec := effectSpec{needs: needs{Runtime: true}, sections: []section{{Decide: func(v txView) txStep {
		if rt = v.RT; !lost { // lose the first CAS: a second attempt reads again
			lost = true
			k.outside("note", "x")
		}
		return mark("a")(v)
	}}}}
	if s := k.run(context.Background(), spec); s.Outcome != settledLanded || rt == nil || rt.Class != rtAbsent {
		t.Fatalf("settlement %+v, runtime %+v; want landed on a fresh read", s, rt)
	}
	if n := len(k.leaf.sinces); n < 2 || !k.leaf.sinces[n-1].After(k.leaf.sinces[0]) || !slices.Equal(k.leaf.names[0], []string{"agent"}) {
		t.Fatalf("sinces %v, names %v; want a later since for each attempt, the template's names", k.leaf.sinces, k.leaf.names)
	}
	k.p.Runtime = nil
	if s := k.run(context.Background(), spec); s.Cause != causeRouteUnknown {
		t.Fatalf("no provider: settlement %+v, want refused %q", s, causeRouteUnknown)
	}
}

// Kills a premise that checks too little: a fresh row that moved its
// incarnation, any lifecycle fact (a wake_request at the same generation),
// its runtime name, or closed, refuses without Decide; after a section
// lands, the next one expects the row it wrote.
func TestTxPremise(t *testing.T) {
	for name, move := range map[string]func(k *txKit){
		"generation":   func(k *txKit) { k.outside("generation", "2") },
		"wake request": func(k *txKit) { k.outside("wake_request", "api") },
		"runtime name": func(k *txKit) { k.outside("session_name", "s-other") },
		"closed":       func(k *txKit) { _ = k.backing.Close("gc-1") },
	} {
		k := newTxKit(t)
		move(k)
		decided := false
		spec := effectSpec{needs: needs{NameLock: true}, sections: []section{{Decide: func(v txView) txStep { decided = true; return mark("a")(v) }}}}
		if s := k.run(context.Background(), spec); s.Outcome != settledRefused || s.Cause != causePremise || decided {
			t.Fatalf("%s: settlement %+v (decided %t), want refused %q before Decide", name, s, decided, causePremise)
		}
	}
	k := newTxKit(t)
	two := effectSpec{sections: []section{
		{Decide: func(txView) txStep { return txStep{Write: session.MetadataPatch{"state": "awake", "generation": "2"}} }},
		{Decide: mark("b")},
	}}
	if s := k.run(context.Background(), two); s.Outcome != settledLanded || k.meta("b") != "1" {
		t.Fatalf("after a landed section: settlement %+v, b=%q; want the second to expect the first's row", s, k.meta("b"))
	}
}

// Kills facts a later section drops, a refusal without its facts, and a
// later no-op section overriding a landing: a refusal after a landed section
// keeps both sections' facts; a no-op after one leaves it landed.
func TestTxMergesFacts(t *testing.T) {
	ev := func(typ string) effectFacts { return effectFacts{Events: []events.Event{{Type: typ}}} }
	k := newTxKit(t)
	if s := k.run(context.Background(), effectSpec{sections: []section{{Decide: mark("a")}, {Decide: func(txView) txStep { return txStep{} }}}}); s.Outcome != settledLanded {
		t.Fatalf("a no-op after a landing: settlement %+v, want landed", s)
	}
	k = newTxKit(t)
	s := k.run(context.Background(), effectSpec{sections: []section{
		{Decide: func(txView) txStep { return txStep{Write: session.MetadataPatch{"a": "1"}, Facts: ev("woke")} }},
		{Decide: func(txView) txStep { return txStep{Refuse: "commit-lost", Facts: ev("refused")} }},
	}})
	if s.Outcome != settledRefused || s.Cause != "commit-lost" || len(s.Facts.Events) != 2 {
		t.Fatalf("refused after a landing: settlement %+v, want refused with both sections' events", s)
	}
}

// Kills a context or latch checked before the last seam rather than last, a
// latch begun before the context check, a deadline read as a shutdown (or
// the reverse), and a write after the executor abandoned the effect: a
// context that ends, or a latch the executor closes, immediately before the
// CAS writes nothing, and a context that ended leaves the latch unbegun; a
// landed write leaves it begun.
func TestTxChecksItsContextAndLatchLast(t *testing.T) {
	for _, c := range []struct {
		name  string
		end   func(cancel context.CancelCauseFunc, l *writeLatch)
		cause string
	}{
		{"deadline", func(cancel context.CancelCauseFunc, _ *writeLatch) { cancel(context.DeadlineExceeded) }, causeDeadline},
		{"shutdown", func(cancel context.CancelCauseFunc, _ *writeLatch) { cancel(context.Canceled) }, causeShutdown},
		{"abandoned", func(_ context.CancelCauseFunc, l *writeLatch) { l.abandon() }, causeDeadline},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := newTxKit(t)
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			latch := new(writeLatch)
			k.on(seamBeforeCAS, func() { c.end(cancel, latch) })
			s := runTx(ctx, k.p, k.it, effectSpec{sections: []section{{Decide: mark("a")}}}, latch)
			if s.Outcome != settledFailed || s.Cause != c.cause || k.meta("a") != "" || latch.begun() {
				t.Fatalf("settlement %+v, a=%q, latch begun %t; want failed %q, nothing written or begun", s, k.meta("a"), latch.begun(), c.cause)
			}
		})
	}
	k := newTxKit(t)
	latch := new(writeLatch)
	if s := runTx(context.Background(), k.p, k.it, effectSpec{sections: []section{{Decide: mark("a")}}}, latch); s.Outcome != settledLanded || !latch.begun() {
		t.Fatalf("settlement %+v, want landed with its latch begun", s)
	}
}

// Kills a latch reopened after its abandonment: once the executor closes a
// begun latch, no further write begins.
func TestWriteLatchAbandonIsTerminal(t *testing.T) {
	l := new(writeLatch)
	if !l.begin() || !l.abandon() || l.begin() || !l.begun() {
		t.Fatal("a begun latch, abandoned, began again or forgot it had begun")
	}
	l = new(writeLatch)
	if l.abandon() || l.begin() || l.begun() {
		t.Fatal("an open latch, abandoned, reported a write or began one")
	}
}

// Kills an around that can run its effect twice, or that runs inside the
// locks: it runs outside both, and a second run fails, writing nothing more.
func TestTxAroundRunsTheEffectAtMostOnce(t *testing.T) {
	k := newTxKit(t)
	spec := effectSpec{needs: needs{NameLock: true}, sections: []section{{Decide: mark("a")}}}
	var second settlement
	spec.around = func(_ context.Context, _ aroundCaps, run func() settlement) settlement {
		if k.locks.holds(k.it.Key.ID) || nameLocked(k.p.World.CityPath, "s-gc-1") {
			t.Error("around ran inside a lock")
		}
		first := run()
		second = run()
		return first
	}
	if s := k.run(context.Background(), spec); s.Outcome != settledLanded || second.Outcome != settledFailed || second.Cause != causeAroundRun {
		t.Fatalf("settlement %+v, second run %+v; want landed once and the second refused", s, second)
	}
}

// Kills an executor that reads an abandoned effect's kind rather than its
// latch, a shutdown labeled as a deadline, and a panic after a write began
// settled failed: a row write abandoned after its write began settles
// ambiguous, one abandoned before settles failed and can begin no write, a
// shutdown abandonment says so, and a panic once a write began is
// ambiguous.
func TestExecutorSettlesAnAbandonedEffectByItsLatch(t *testing.T) {
	clk := newFakePlannerClock(plannerT0)
	x, posted := fakeClockExecutor(clk)
	release := make(chan struct{})
	defer close(release)
	latches := map[string]*writeLatch{"begun": new(writeLatch), "open": new(writeLatch)}
	latches["begun"].begin()
	for id, l := range latches {
		e := hungEffect(intentRowHeal, 1, plannerT0.Add(30*time.Second), release)
		e.Latch = l
		if err := x.submit(rowKey{Leg: rowLeg, ID: id}, e); err != nil {
			t.Fatal(err)
		}
	}
	waitTimersAt(t, clk, plannerT0.Add(30*time.Second), 4)
	clk.Advance(30 * time.Second)
	for range latches {
		switch s := receive(t, posted); s.Key.ID {
		case "begun":
			if s.Outcome != settledAmbiguous || s.Cause != causeDeadline {
				t.Errorf("begun: settlement %+v, want ambiguous at the deadline", s)
			}
		default:
			if s.Outcome != settledFailed || s.Cause != causeDeadline || latches["open"].begin() {
				t.Errorf("open: settlement %+v, want failed at the deadline and the write refused", s)
			}
		}
	}
	panicky := new(writeLatch)
	panicky.begin()
	if err := x.submit(rowKey{Leg: rowLeg, ID: "panic"}, sessionEffect{Kind: intentRowHeal, Deadline: plannerT0.Add(time.Hour), Latch: panicky, Run: func(context.Context) settlement { panic("mid-write") }}); err != nil {
		t.Fatal(err)
	}
	if s := receive(t, posted); s.Outcome != settledAmbiguous || s.Cause != causePanic {
		t.Fatalf("panic after its write began: settlement %+v, want ambiguous", s)
	}
	if err := x.submit(rowKey{Leg: rowLeg, ID: "shutdown"}, hungEffect(intentRowHeal, 2, plannerT0.Add(time.Hour), release)); err != nil {
		t.Fatal(err)
	}
	x.cancel()
	if s := receive(t, posted); s.Cause != causeShutdown {
		t.Fatalf("abandoned at shutdown: settlement %+v, want cause %q", s, causeShutdown)
	}
}

// Kills a spec table that loses a kind's admission facts or runs an effect
// that has not merged, a body anywhere but the create, and a capability
// reached without its grant: every kind has a cap class, only the merged
// kinds run, the create's body declares no needs and alone holds a
// capability, and caps without capCreate hold no create handle.
func TestEffectSpecsCoverEveryKind(t *testing.T) {
	var running []string
	for kind, spec := range effectSpecs {
		if spec.class == 0 {
			t.Errorf("%s has no cap class", kind)
		}
		if spec.runs() {
			running = append(running, kind)
		}
		if spec.caps != 0 && kind != intentCreate {
			t.Errorf("%s holds capabilities %b", kind, spec.caps)
		}
	}
	slices.Sort(running)
	for kind, spec := range effectSpecs {
		if spec.body != nil && (kind != intentCreate || spec.needs != (needs{}) || len(spec.sections) > 0) {
			t.Errorf("%s: only the create has a body, and a body declares no needs or sections", kind)
		}
	}
	if want := []string{intentCreate, intentDrainCancel, intentDrainVoid, intentRekey, intentRowHeal, intentRowHealFresh}; !slices.Equal(running, want) {
		t.Fatalf("running kinds %v, want %v", running, want)
	}
	p := &effectPass{held: heldCaps{create: &createPass{}, creates: &createEffects{}}}
	if c := p.capsFor(intent{}, 0, nil); c.create != nil || c.creates != nil {
		t.Fatal("caps without capCreate hold the create runner")
	}
	if c := p.capsFor(intent{}, capCreate, nil); c.create == nil || c.creates == nil {
		t.Fatal("capCreate does not reach the create runner")
	}
}

// Kills a fact the planner drops: every effectFacts field, set, is consumed
// by applyFacts. A new field fails here until applyFacts and this table
// consume it.
func TestApplyFactsConsumesEveryField(t *testing.T) {
	rec := &memRecorder{}
	p := newPlanner(newFakePlannerClock(plannerT0), func() time.Duration { return time.Minute }, nil, newInflightMap(), nil, io.Discard)
	p.rec = rec
	var transitions []string
	saved := recordDrainTransition
	recordDrainTransition = func(_ context.Context, name, reason, transition string) {
		transitions = append(transitions, name+"/"+reason+"/"+transition)
	}
	t.Cleanup(func() { recordDrainTransition = saved })
	f := effectFacts{
		Events:     []events.Event{{Type: "session.test"}},
		Transition: &drainTransition{Name: "s", Reason: "idle", Transition: "cancel"},
		Work:       &workVerdict{BeadID: "gc-w1", Refused: true},
	}
	p.applyFacts(f, plannerT0)
	consumed := map[string]bool{
		"Events":     len(rec.events) == 1,
		"Transition": slices.Equal(transitions, []string{"s/idle/cancel"}),
		"Work":       p.backoff.Snapshot()[workBackoffKey("gc-w1")].Cause == createStageWorktree,
	}
	ft := reflect.TypeOf(f)
	for i := range ft.NumField() {
		if name := ft.Field(i).Name; !consumed[name] {
			t.Errorf("effectFacts.%s is not consumed by applyFacts", name)
		}
	}
	if len(consumed) != ft.NumField() {
		t.Errorf("the table checks %d fields, effectFacts has %d", len(consumed), ft.NumField())
	}
}

// Kills a decision on reads its context overtook: a context that ends while
// the runtime is read settles with its cause, and Decide never runs.
func TestTxChecksItsContextAfterTheReads(t *testing.T) {
	k := newTxKit(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	k.leaf.during = func() { cancel(context.DeadlineExceeded) }
	decided := false
	spec := effectSpec{needs: needs{Runtime: true}, sections: []section{{Decide: func(v txView) txStep { decided = true; return mark("a")(v) }}}}
	if s := k.run(ctx, spec); s.Outcome != settledFailed || s.Cause != causeDeadline || decided {
		t.Fatalf("settlement %+v (decided %t), want failed at the deadline before Decide", s, decided)
	}
}

// racingBacking is a backing whose first Get of id after arm lands a local
// write through the cache while RefreshRow's backing read is in flight, so
// RefreshRow answers ErrRowRefreshFenced.
type racingBacking struct {
	simBacking
	id    string
	cache **beads.CachingStore
	armed *bool
}

func (b racingBacking) Get(id string) (beads.Bead, error) {
	got, err := b.simBacking.Get(id)
	if id == b.id && *b.armed {
		*b.armed = false
		_ = (*b.cache).SetMetadata(id, "note", "racing")
	}
	return got, err
}

// Kills a fenced row read ended as a write error (review pin T5b): a write
// racing the transaction's backing read fences the read, and the next
// attempt reads again, decides again and lands beside the racing write.
func TestTxRetriesAFencedRefresh(t *testing.T) {
	k := newTxKit(t)
	m := beads.NewMemStoreFrom(0, []beads.Bead{poolRow("gc-1", "worker", 1, "asleep")}, nil)
	if err := beads.StampOpenedStore(m, "MemStore", gate.Require, nil, nil); err != nil {
		t.Fatal(err)
	}
	var cache *beads.CachingStore
	armed := false
	cache = beads.NewCachingStoreForTest(racingBacking{simBacking: simBacking{m}, id: "gc-1", cache: &cache, armed: &armed}, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	armed = true
	k.p.Writers[rowLeg] = fencedWriter{store: cache}
	decides := 0
	s := k.run(context.Background(), effectSpec{sections: []section{{Decide: func(v txView) txStep { decides++; return mark("a")(v) }}}})
	b, _ := m.Get("gc-1")
	if armed || s.Outcome != settledLanded || b.Metadata["a"] != "1" || b.Metadata["note"] != "racing" {
		t.Fatalf("settlement %+v after %d decisions, row %v; want the retry to land beside the racing write", s, decides, b.Metadata)
	}
}

// attemptsWaivers names each kind whose spec retries its CAS fewer than the
// default 3 times, with the reason. A single attempt turns every unrelated
// write on a busy row into a backoff (EFFECT-STRUCTURE class 4).
var attemptsWaivers = map[string]string{}

// Kills a single-attempt CAS slipped into the table: a kind setting
// needs.Attempts below the default needs a waiver with its reason.
func TestEffectSpecsRetryTheirCAS(t *testing.T) {
	for kind, spec := range effectSpecs {
		if n := spec.needs.Attempts; n != 0 && n < 3 && attemptsWaivers[kind] == "" {
			t.Errorf("%s retries its CAS %d times: add a waiver with its reason to attemptsWaivers", kind, n)
		}
	}
}

// Kills a seam that cannot fail the effect, fails it as something else, or
// reports the wrong section or attempt (H1's fail action): each seam's error
// ends the effect with cause injected, failed before the write and
// ambiguous after it, the write landed; seams report the section and the
// attempt they fire in; a bound planner is armed with the staging seam for
// its city.
func TestTxSeamsInjectFailures(t *testing.T) {
	boom := errors.New("staging fault")
	for _, at := range []txSeam{seamAfterReads, seamAfterRowRead, seamBeforeCAS, seamAfterWrite} {
		k := newTxKit(t)
		k.fail = map[txSeam]error{at: boom}
		s := k.run(context.Background(), effectSpec{sections: []section{{Decide: mark("a")}}})
		want := settledFailed
		if at == seamAfterWrite {
			want = settledAmbiguous
		}
		if s.Outcome != want || s.Cause != causeInjected || !errors.Is(s.Err, boom) || (k.meta("a") == "1") != (at == seamAfterWrite) {
			t.Fatalf("seam %d: settlement %+v, a=%q; want outcome %d %q, written only past the write", at, s, k.meta("a"), want, causeInjected)
		}
	}
	k := newTxKit(t)
	k.on(seamBeforeCAS, func() { k.outside("note", "x") }) // loses the first CAS
	k.run(context.Background(), effectSpec{sections: []section{{Decide: mark("a")}, {Decide: mark("b")}}})
	if !slices.Contains(k.attempts, 2) || k.attempts[0] != 1 || k.sections[0] != 0 || k.sections[len(k.sections)-1] != 1 {
		t.Fatalf("sections %v, attempts %v at seams %v, want attempts 1 then 2 in section 0, then section 1", k.sections, k.attempts, k.seen)
	}
	saved, city := stagingSeam, ""
	stagingSeam = func(c string) txSeamFunc {
		city = c
		return func(context.Context, txSeam, intent, int, int) error { return nil }
	}
	t.Cleanup(func() { stagingSeam = saved })
	rt := newDefaultPlanner(io.Discard)
	rt.bindHost(plannerHost{gather: gatherEnv{CityPath: "/city"}})
	if rt.planner.seam == nil || city != "/city" {
		t.Fatalf("seam set %t for city %q; want the bound planner armed for /city", rt.planner.seam != nil, city)
	}
}

// Kills a production path that arms the staging seam: only gcstaging-tagged
// code assigns stagingSeam (H1).
func TestOnlyStagingCodeAssignsTheStagingSeam(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	assign := regexp.MustCompile(`(?m)^\s*stagingSeam\s*=`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if assign.Match(src) && !regexp.MustCompile(`(?m)^//go:build .*\bgcstaging\b`).Match(src) {
			t.Errorf("%s assigns stagingSeam without the gcstaging build tag", f)
		}
	}
}

// Kills a hook reaching handles its place forbids: around, outside every
// lock, holds only these fields; a Call's handles (provider start,
// routing, release, kill) go on txCaps alone.
func TestAroundCapsAreScoped(t *testing.T) {
	allowed := map[string]bool{"it": true}
	for i, ty := 0, reflect.TypeFor[aroundCaps](); i < ty.NumField(); i++ {
		if f := ty.Field(i).Name; !allowed[f] {
			t.Errorf("aroundCaps.%s: around runs outside every lock; a write or provider handle belongs to a Call", f)
		}
	}
}
