package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The effect transaction's tests (simplify/EFFECT-STRUCTURE.md §2.1) on the
// effect test kit: one session row on a CachingStore over a stamped,
// revisioned MemStore (the simulator's leg) and the pass that decided on
// it. Effect tests build on it rather than on a fake leaf of their own.

// The keys the tx tests write: registered advisory keys no premise
// compares, so a write lands without moving a fact the premise reads. The
// session field registry's guard fails a production write of any other key.
const (
	txKeyA    = "usage_compute_emitted_at"
	txKeyB    = "usage_model_swept_at"
	txKeyNote = "invocation_usage_cursor"
)

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

// txKit is the kit: row gc-1, runtime name s-gc-1, the pass, its runtime
// (the simulator's cache-aware provider behind a recording leaf), and the
// outside operations queued per seam, each run once, in order.
type txKit struct {
	t        *testing.T
	backing  beads.Store
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
	return newTxKitOn(t, m, simBacking{m})
}

// exactBacking is a backing whose reads the cache serves exactly, with its
// conditional writes (and atomic closer) resolved on the store it wraps.
type exactBacking struct{ beads.Store }

func (exactBacking) CachedReadExact() bool                         { return true }
func (b exactBacking) ConditionalWritesResolveTarget() beads.Store { return b.Store }

// newCloseTxKit is the kit over a backing with an atomic conditional closer,
// for close sections.
func newCloseTxKit(t *testing.T) *txKit {
	t.Helper()
	m := beads.NewAtomicCloseMemStore()
	if _, err := m.Create(poolRow("gc-1", "worker", 1, "asleep")); err != nil {
		t.Fatal(err)
	}
	return newTxKitOn(t, m, exactBacking{m})
}

func newTxKitOn(t *testing.T, m, backing beads.Store) *txKit {
	t.Helper()
	if err := beads.StampOpenedStore(m, "MemStore", gate.Require, nil, nil); err != nil {
		t.Fatal(err)
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
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
	w.WorkLegs = workLegsFromCensus(w.CityPath, w.Env.Cfg, cityWorkStore{store: cache}, nil)
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

// Kills a section outside the row's session mutation lock or released
// before its CAS, a provider Call under it, a name lock not held across the
// sections and the Call or kept after, a busy name waited on, and a Call
// input or result that does not cross: inside each section both locks are
// held at every seam; during and after the Call only the name lock is; the
// Call takes the step's Pass and the next section reads its result and
// error; both writes land; the last section's Call error fails the effect.
func TestTxLockScope(t *testing.T) {
	k := newTxKit(t)
	var problems []string
	expect := func(where string, row bool) {
		if k.locks.holds(k.it.Key.ID) != row || !nameHeld(k.t, k.p.World.CityPath, "s-gc-1") {
			problems = append(problems, where)
		}
	}
	k.p.seam = func(_ context.Context, at txSeam, _ intent, _, _ int) error {
		k.seen = append(k.seen, at)
		expect(fmt.Sprintf("seam %d", at), at != seamBeforeCall && at != seamAfterCall)
		return nil
	}
	callErr := errors.New("start failed")
	first := called(callStart, section{Decide: func(v txView) txStep {
		s := mark(txKeyA)(v)
		s.Write["state"] = "creating"
		s.Pass = "prepared"
		return s
	}},
		func(_ context.Context, _ txCaps, in string) (int, error) {
			expect("during the call", false)
			if in != "prepared" {
				problems = append(problems, "the call's input")
			}
			return 7, callErr
		})
	second := section{Premise: premiseOwnToken, Decide: func(v txView) txStep {
		if out, err := callResult[int](v); out != 7 || !errors.Is(err, callErr) {
			problems = append(problems, "the second section's view")
		}
		return mark(txKeyB)(v)
	}}
	spec := effectSpec{sections: []section{first, second}}
	if s := k.run(context.Background(), spec); s.Outcome != settledLanded {
		t.Fatalf("settlement %+v, want landed", s)
	}
	want := []txSeam{seamAfterReads, seamAfterRowRead, seamBeforeCAS, seamAfterWrite, seamBeforeCall, seamAfterCall, seamAfterReads, seamAfterRowRead, seamBeforeCAS, seamAfterWrite}
	if len(problems) > 0 || !slices.Equal(k.seen, want) || k.meta(txKeyA) != "1" || k.meta(txKeyB) != "1" {
		t.Fatalf("problems %v, seams %v, a=%q b=%q; want both locks in each section, the name lock alone over the call, both writes", problems, k.seen, k.meta(txKeyA), k.meta(txKeyB))
	}
	if nameHeld(k.t, k.p.World.CityPath, "s-gc-1") || k.locks.holds(k.it.Key.ID) {
		t.Fatal("a lock outlived the transaction")
	}
	k = newTxKit(t) // the first run moved the row to creating
	spec.sections = spec.sections[:1]
	if s := k.run(context.Background(), spec); s.Outcome != settledFailed || s.Cause != causeCallError || !errors.Is(s.Err, callErr) {
		t.Fatalf("last call failed: settlement %+v, want failed with cause %q", s, causeCallError)
	}
	holdName(t, k.p.World.CityPath, "s-gc-1")
	if s := k.run(context.Background(), spec); s.Outcome != settledRefused || s.Cause != causeNameBusy {
		t.Fatalf("busy name: settlement %+v, want refused with cause %q", s, causeNameBusy)
	}
}

// Kills a provider Call that runs ungated (review must-fix 1, T19): one the
// executor abandoned before any write began never calls, nor one whose
// context ended, and one abandoned during its call settles ambiguous.
func TestTxCallIsGatedLikeAWrite(t *testing.T) {
	k := newTxKit(t)
	latch := new(writeLatch)
	k.on(seamAfterRowRead, func() { latch.abandon() }) // the executor gives up here
	ran := false
	call := called(callStop, section{Decide: func(txView) txStep { return txStep{} }}, func(context.Context, txCaps, any) (any, error) { ran = true; return nil, nil })
	if s := runTx(context.Background(), k.p, k.it, effectSpec{sections: []section{call}}, latch); ran || s.Outcome != settledFailed {
		t.Fatalf("settlement %+v (called %t), want no call after the abandonment", s, ran)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	k.on(seamAfterRowRead, func() { cancel(context.DeadlineExceeded) }) // the section itself writes nothing
	if s := runTx(ctx, k.p, k.it, effectSpec{sections: []section{call}}, new(writeLatch)); ran || s.Cause != causeDeadline {
		t.Fatalf("settlement %+v (called %t), want no call past the deadline", s, ran)
	}
	clk := newFakePlannerClock(plannerT0)
	x, posted := fakeClockExecutor(clk)
	inCall, release := make(chan struct{}), make(chan struct{})
	slow := called(callStop, section{Decide: func(txView) txStep { return txStep{} }}, func(context.Context, txCaps, any) (any, error) {
		close(inCall)
		<-release
		return nil, nil
	})
	latch = new(writeLatch)
	e := sessionEffect{
		Kind: "tx-test", Seq: 1, Deadline: plannerT0.Add(time.Minute), Latch: latch,
		Run: func(ctx context.Context) settlement {
			return runTx(ctx, k.p, k.it, effectSpec{sections: []section{slow}}, latch)
		},
	}
	if err := x.submit(k.it.Key, e); err != nil {
		t.Fatal(err)
	}
	<-inCall
	waitTimersAt(t, clk, plannerT0.Add(time.Minute), 2)
	clk.Advance(time.Minute)
	s := receive(t, posted)
	close(release) // what it returns carries no facts, so nothing is posted late
	if s.Outcome != settledAmbiguous {
		t.Fatalf("abandoned during its provider call: settlement %+v, want ambiguous", s)
	}
}

// Kills a landed section's facts lost to a later Call error (review pin),
// and a section reading the runtime from before an earlier Call (review pin
// T21): the effect fails with the call's error and keeps the landing's
// event; the section after a Call reads since that section began.
func TestTxCallKeepsFactsAndTheNextSectionReadsAfterIt(t *testing.T) {
	k := newTxKit(t)
	sec := called(callStop, section{Decide: func(v txView) txStep {
		s := mark(txKeyA)(v)
		s.Facts.Events = []events.Event{{Type: "landed"}}
		return s
	}}, func(context.Context, txCaps, any) (any, error) { return nil, errors.New("provider failed") })
	if s := k.run(context.Background(), effectSpec{sections: []section{sec}}); s.Cause != causeCallError || len(s.Facts.Events) != 1 {
		t.Fatalf("settlement %+v, want the call's failure with the landed write's event", s)
	}
	k = newTxKit(t)
	clk := k.p.Clock.(*fakePlannerClock)
	t0 := clk.Now()
	spec := effectSpec{needs: needs{Runtime: true}, sections: []section{
		called(callStop, section{Decide: func(txView) txStep { return txStep{} }}, func(context.Context, txCaps, any) (any, error) { clk.Advance(time.Minute); return nil, nil }),
		{Decide: func(txView) txStep { return txStep{} }},
	}}
	k.run(context.Background(), spec)
	if len(k.leaf.sinces) < 2 || !k.leaf.sinces[len(k.leaf.sinces)-1].Equal(t0.Add(time.Minute)) {
		t.Fatalf("sinces %v, want the second section's read since %v", k.leaf.sinces, t0.Add(time.Minute))
	}
}

// Kills verdicts the transaction cannot express: a failure settles failed
// with its cause and error; Done ends the effect, landed after a write and a
// no-op with its cause without one, and no later section runs.
func TestTxVerdicts(t *testing.T) {
	boom := errors.New("boom")
	ran := false
	later := section{Premise: premiseOwnToken, Decide: func(txView) txStep { ran = true; return txStep{} }}
	for _, c := range []struct {
		name    string
		first   txStep
		outcome settleOutcome
		cause   string
	}{
		{"fail", txStep{Fail: "prepare", Err: boom}, settledFailed, "prepare"},
		{"done without a write", txStep{Done: true, Cause: "already-running"}, settledNoop, "already-running"},
		{"done after a write", txStep{Write: session.MetadataPatch{txKeyA: "1"}, Done: true}, settledLanded, ""},
	} {
		k := newTxKit(t)
		ran = false
		s := k.run(context.Background(), effectSpec{sections: []section{{Decide: func(txView) txStep { return c.first }}, later}})
		if s.Outcome != c.outcome || s.Cause != c.cause || ran || (c.name == "fail") != errors.Is(s.Err, boom) {
			t.Fatalf("%s: settlement %+v (later ran %t), want outcome %d cause %q", c.name, s, ran, c.outcome, c.cause)
		}
	}
}

// Kills S2's own-token rule checking too much or too little (review pins,
// the F2 ruling: the registry's FactsCommit site): after the effect's own
// write and its start Call it ignores an agent's heartbeat hold and the keys
// the started runtime writes, and refuses an operator suspend, another
// token, a closed row, and `gc session kill`'s fence (asleep, the same
// token) landed during the Call; with no own write before it, it refuses
// even a row in S2's states (another writer's wake since the pass).
func TestTxPremiseOwnToken(t *testing.T) {
	kill := session.KillPendingPatch(gatherNow)
	for _, c := range []struct {
		name    string
		outside func(k *txKit)
		lands   bool
	}{
		{"an agent's heartbeat hold", func(k *txKit) { k.outside("held_until", rowAt(time.Hour)) }, true},
		{"the started runtime's session key", func(k *txKit) { k.outside("session_key", "k-2") }, true},
		{"an operator suspend", func(k *txKit) { k.outside("state", "suspended", "suspended_at", rowAt(0)) }, false},
		{"another token", func(k *txKit) { k.outside("instance_token", "tok-other") }, false},
		{"closed", func(k *txKit) {
			if err := k.backing.Close("gc-1"); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"a kill fence since the pass", func(k *txKit) {
			if err := k.backing.SetMetadataBatch("gc-1", kill); err != nil {
				t.Fatal(err)
			}
		}, false},
	} {
		k := newTxKit(t)
		k.on(seamAfterCall, func() { c.outside(k) }) // out of process, during the Start
		preWake := called(callStart, section{Decide: func(txView) txStep { return txStep{Write: session.MetadataPatch{"state": "creating"}} }},
			func(context.Context, txCaps, any) (any, error) { return nil, nil })
		commit := section{Premise: premiseOwnToken, Decide: mark(txKeyA)}
		s := k.run(context.Background(), effectSpec{needs: needs{NameLock: true}, sections: []section{preWake, commit}})
		if landed := k.meta(txKeyA) == "1"; landed != c.lands || !c.lands && s.Cause != causePremise {
			t.Fatalf("%s: settlement %+v (commit landed %t), want landed %t", c.name, s, landed, c.lands)
		}
	}
	k := newTxKit(t)
	k.outside("state", "active", "held_until", rowAt(time.Hour))
	if s := k.run(context.Background(), effectSpec{sections: []section{{Premise: premiseOwnToken, Decide: mark(txKeyA)}}}); s.Cause != causePremise || k.meta(txKeyA) != "" {
		t.Fatalf("no own write: settlement %+v, want refused %q", s, causePremise)
	}
}

// legLeaf is the kit's leaf answering the fence legs: attachment reported,
// a pending interaction (runtime.Fake's), and the idle wait and activity.
type legLeaf struct {
	*recordingLeaf
	attached           bool
	attachErr, idleErr error
	last               time.Time
}

func (l legLeaf) Capabilities() runtime.ProviderCapabilities {
	return runtime.ProviderCapabilities{CanReportAttachment: true}
}
func (l legLeaf) IsAttachedWithError(string) (bool, error) { return l.attached, l.attachErr }
func (l legLeaf) WaitForIdle(context.Context, string, time.Duration) error {
	return l.idleErr
}
func (l legLeaf) GetLastActivity(string) (time.Time, error) { return l.last, nil }

// failingList is a store whose list reads fail.
type failingList struct{ beads.Store }

func (failingList) List(beads.ListQuery) ([]beads.Bead, error) { return nil, errors.New("store down") }

// Kills legs read elsewhere than the routed leaf, read for an intent whose
// needs omit them, and a failed or missing work read counted as no work
// (review pin T16): attach, pending, idle and L5 each answer for the kit's
// row, per intent as needsFor says.
func TestTxReadsTheFenceLegs(t *testing.T) {
	k := newTxKit(t)
	leaf := legLeaf{recordingLeaf: k.leaf, attached: true, idleErr: nil, last: gatherNow.Add(-time.Hour)}
	k.p.Runtime = leaf
	var f txFence
	spec := effectSpec{
		needs: needs{Legs: legAttach | legPending | legWork, Idle: true},
		needsFor: func(_ *World, it intent) needs {
			n := needs{Legs: legAttach | legPending, Idle: true}
			if it.Reason == "orphaned" { // drain begin's L5, by reason
				n.Legs |= legWork
			}
			return n
		},
		sections: []section{{Decide: func(v txView) txStep { f = v.Fence; return txStep{} }}},
	}
	k.sp.SetPendingInteraction("s-gc-1", &runtime.PendingInteraction{RequestID: "r-1"})
	k.it.Reason = "orphaned"
	k.run(context.Background(), spec)
	if f.Attach != fenceAttached || f.Pending != fencePending || !f.Idle || f.Work == nil || !f.Work.Free || f.Work.Err != nil {
		t.Fatalf("fence %+v (work %+v), want attached, pending, idle, no work", f, f.Work)
	}
	if _, err := k.backing.Create(beads.Bead{Title: "task", Type: "task", Status: "open", Assignee: "gc-1"}); err != nil {
		t.Fatal(err)
	}
	k.p.Runtime = legLeaf{recordingLeaf: k.leaf, last: gatherNow.Add(time.Minute)}
	k.sp.SetPendingInteraction("s-gc-1", nil)
	k.run(context.Background(), spec)
	if f.Attach != "" || f.Pending != "" || f.Idle || f.Work.Free {
		t.Fatalf("fence %+v (work %+v), want detached, nothing pending, active since the pass, work found", f, f.Work)
	}
	k.p.reads.legs = workLegsFromCensus("", nil, cityWorkStore{store: blindWriteRefusingStore{inner: failingList{k.cache}}}, nil)
	if k.run(context.Background(), spec); f.Work.Free || f.Work.Err == nil {
		t.Fatalf("failed work read: %+v, want work assumed, with its error", f.Work)
	}
	k.p.reads.legs = WorkLegs{}
	if k.run(context.Background(), spec); f.Work.Free || !errors.Is(f.Work.Err, errNoReadStore) {
		t.Fatalf("no store: %+v, want work assumed", f.Work)
	}
	k.it.Reason = "idle"
	if k.run(context.Background(), spec); f.Work != nil {
		t.Fatalf("an intent whose needs omit L5: work %+v, want unread", f.Work)
	}
}

// Kills a Probe outside the locks, or with an untyped or lost result: its
// typed result and error reach Decide; it runs under both locks and sees the
// expected row. (Its bound is boundedProbe's, tested with the fence.)
func TestTxProbe(t *testing.T) {
	k := newTxKit(t)
	var got string
	var gotErr error
	sec := probed(func(_ context.Context, _ effectReads, v txView) (string, error) {
		if !k.locks.holds(k.it.Key.ID) || !nameHeld(k.t, k.p.World.CityPath, "s-gc-1") {
			t.Error("the probe ran outside a lock")
		}
		return v.Row.SessionName, nil
	}, func(_ txView, p string, err error) txStep { got, gotErr = p, err; return txStep{} })
	k.run(context.Background(), effectSpec{needs: needs{NameLock: true}, sections: []section{sec}})
	if got != "s-gc-1" || gotErr != nil {
		t.Fatalf("probe result %q, %v; want the expected row's name", got, gotErr)
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
				k.outside(txKeyNote, string(rune('a'+decides)))
			}
			return mark(txKeyA)(v)
		}}}}
		s := k.run(context.Background(), spec)
		if decides != c.decides || (s.Outcome == settledLanded) != c.landed || (!c.landed && s.Cause != causeCAS) || (k.meta(txKeyA) == "1") != c.landed || k.meta(txKeyNote) == "" {
			t.Fatalf("attempts %d, %d races: settlement %+v after %d decisions, a=%q; want landed %t after %d", c.attempts, c.races, s, decides, k.meta(txKeyA), c.landed, c.decides)
		}
	}
}

// Kills a decision on the cached row: a write behind the cache (its event
// not yet delivered) is what Decide sees, on its first attempt.
func TestTxDecidesOnTheBackingRow(t *testing.T) {
	k := newTxKit(t)
	k.outside(txKeyNote, "behind")
	var seen []string
	spec := effectSpec{sections: []section{{Decide: func(v txView) txStep {
		seen = append(seen, v.Meta[txKeyNote])
		return mark(txKeyA)(v)
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
			k.outside(txKeyNote, "during-read")
		}
	}
	decides := 0
	spec := effectSpec{needs: needs{Runtime: true}, sections: []section{{Decide: func(v txView) txStep { decides++; return mark(txKeyA)(v) }}}}
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
			k.outside(txKeyNote, "x")
		}
		return mark(txKeyA)(v)
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
		spec := effectSpec{needs: needs{NameLock: true}, sections: []section{{Decide: func(v txView) txStep { decided = true; return mark(txKeyA)(v) }}}}
		if s := k.run(context.Background(), spec); s.Outcome != settledRefused || s.Cause != causePremise || decided {
			t.Fatalf("%s: settlement %+v (decided %t), want refused %q before Decide", name, s, decided, causePremise)
		}
	}
	k := newTxKit(t)
	two := effectSpec{sections: []section{
		{Decide: func(txView) txStep { return txStep{Write: session.MetadataPatch{"state": "awake", "generation": "2"}} }},
		{Decide: mark(txKeyB)},
	}}
	if s := k.run(context.Background(), two); s.Outcome != settledLanded || k.meta(txKeyB) != "1" {
		t.Fatalf("after a landed section: settlement %+v, b=%q; want the second to expect the first's row", s, k.meta(txKeyB))
	}
}

// Kills a premise that compares other keys than the registry's (F2): a key
// of each premise class moved since the pass refuses, one outside the
// session codec (a stop request) included; an advisory, a baseline or a
// lease key does not; an assignment identity key refuses only an effect
// that reads the row's work; after a start Call the keys the started
// runtime writes (session.FactsAfterStart: its session key and claim) do
// not, while a lifecycle key, detached_at or the current bead still do;
// after a stop Call, a start's included, the full premise holds. A refusal
// names the key that moved.
func TestTxPremiseComparesTheRegistryFacts(t *testing.T) {
	for _, c := range []struct {
		name    string
		kv      []string
		work    bool
		calls   []callKind // the Calls before the section, if any
		refuses bool
	}{
		{"lifecycle", []string{"state_reason", "x"}, false, nil, true},
		{"operator intent", []string{"held_until", rowAt(time.Hour)}, false, nil, true},
		{"request", []string{"restart_requested", "true"}, false, nil, true},
		{"stop request", []string{"drain_intent_reason", "idle"}, false, nil, true},
		{"counter", []string{"wake_attempts", "3"}, false, nil, true},
		{"marker", []string{"detached_at", rowAt(0)}, false, nil, true},
		{"advisory", []string{"synced_at", rowAt(0)}, false, nil, false},
		{"baseline", []string{"started_provision_hash", "h"}, false, nil, false},
		{"lease", []string{session.RuntimeLeaseEpochKey, "7", session.RuntimeLeaseHolderKey, "other"}, false, nil, false},
		{"assignment identity, no work read", []string{"alias", "a2"}, false, nil, false},
		{"assignment identity, work read", []string{"alias", "a2"}, true, nil, true},
		{"after a start: its session key", []string{"session_key", "k2"}, false, []callKind{callStart}, false},
		{"after a start: detached_at", []string{"detached_at", rowAt(0)}, false, []callKind{callStart}, true},
		{"after a start: the current bead", []string{session.CurrentBeadIDKey, "b-1"}, false, []callKind{callStart}, true},
		{"after a start: the current claim", []string{beadmeta.CurrentClaimBeadIDMetadataKey, "b-1"}, false, []callKind{callStart}, false},
		{"after a start: lifecycle", []string{"state_reason", "x"}, false, []callKind{callStart}, true},
		{"after a stop: a session key", []string{"session_key", "k2"}, false, []callKind{callStop}, true},
		{"after a stop: detached_at", []string{"detached_at", rowAt(0)}, false, []callKind{callStop}, true},
		{"after a start, then a stop: a session key", []string{"session_key", "k2"}, false, []callKind{callStart, callStop}, true},
	} {
		k := newTxKit(t)
		spec := effectSpec{sections: []section{{Decide: mark(txKeyA)}}}
		if c.work {
			spec.needs.Legs = legWork
		}
		if len(c.calls) > 0 {
			spec = effectSpec{needs: needs{NameLock: true}}
			for i, kind := range c.calls {
				spec.sections = append(spec.sections, called(kind, section{Decide: func(txView) txStep { return txStep{} }}, func(context.Context, txCaps, any) (any, error) { return nil, nil }))
				if i < len(c.calls)-1 {
					k.on(seamAfterCall, func() {})
				}
			}
			spec.sections = append(spec.sections, section{Decide: mark(txKeyA)})
			k.on(seamAfterCall, func() { k.outside(c.kv...) })
		} else {
			k.outside(c.kv...)
		}
		s := k.run(context.Background(), spec)
		if refused := s.Cause == causePremise && k.meta(txKeyA) == ""; refused != c.refuses {
			t.Errorf("%s: settlement %+v, %s=%q; want refused on the premise %t", c.name, s, txKeyA, k.meta(txKeyA), c.refuses)
		}
		if c.refuses && (!errors.Is(s.Err, errPremiseMoved) || !strings.Contains(s.Err.Error(), c.kv[0])) {
			t.Errorf("%s: refusal error %v, want it to name %s", c.name, s.Err, c.kv[0])
		}
	}
}

// Kills facts a later section drops, a refusal without its facts, and a
// later no-op section overriding a landing: a refusal after a landed section
// keeps both sections' facts; a no-op after one leaves it landed.
func TestTxMergesFacts(t *testing.T) {
	ev := func(typ string) effectFacts { return effectFacts{Events: []events.Event{{Type: typ}}} }
	k := newTxKit(t)
	if s := k.run(context.Background(), effectSpec{sections: []section{{Decide: mark(txKeyA)}, {Decide: func(txView) txStep { return txStep{} }}}}); s.Outcome != settledLanded {
		t.Fatalf("a no-op after a landing: settlement %+v, want landed", s)
	}
	k = newTxKit(t)
	s := k.run(context.Background(), effectSpec{sections: []section{
		{Decide: func(txView) txStep { return txStep{Write: session.MetadataPatch{txKeyA: "1"}, Facts: ev("woke")} }},
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
			s := runTx(ctx, k.p, k.it, effectSpec{sections: []section{{Decide: mark(txKeyA)}}}, latch)
			if s.Outcome != settledFailed || s.Cause != c.cause || k.meta(txKeyA) != "" || latch.begun() {
				t.Fatalf("settlement %+v, a=%q, latch begun %t; want failed %q, nothing written or begun", s, k.meta(txKeyA), latch.begun(), c.cause)
			}
		})
	}
	k := newTxKit(t)
	latch := new(writeLatch)
	if s := runTx(context.Background(), k.p, k.it, effectSpec{sections: []section{{Decide: mark(txKeyA)}}}, latch); s.Outcome != settledLanded || !latch.begun() {
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
	spec := effectSpec{needs: needs{NameLock: true}, sections: []section{{Decide: mark(txKeyA)}}}
	var second settlement
	spec.around = func(_ context.Context, _ aroundCaps, run func() settlement) settlement {
		if k.locks.holds(k.it.Key.ID) || nameHeld(k.t, k.p.World.CityPath, "s-gc-1") {
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
	spec := effectSpec{needs: needs{Runtime: true}, sections: []section{{Decide: func(v txView) txStep { decided = true; return mark(txKeyA)(v) }}}}
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
		_ = (*b.cache).SetMetadata(id, txKeyNote, "racing")
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
	s := k.run(context.Background(), effectSpec{sections: []section{{Decide: func(v txView) txStep { decides++; return mark(txKeyA)(v) }}}})
	b, _ := m.Get("gc-1")
	if armed || s.Outcome != settledLanded || b.Metadata[txKeyA] != "1" || b.Metadata[txKeyNote] != "racing" {
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
		if n := casAttempts(kind, spec); n < 3 && attemptsWaivers[kind] == "" {
			t.Errorf("%s retries its CAS %d times: add a waiver with its reason to attemptsWaivers", kind, n)
		}
	}
	seeded := effectSpec{needsFor: func(_ *World, it intent) needs { return needs{Attempts: map[bool]int{true: 1}[it.Reason == "idle"]} }}
	if n := casAttempts("seeded", seeded); n != 1 {
		t.Fatalf("a needsFor giving one reason one attempt counts %d, want 1", n)
	}
}

// casAttempts is the fewest CAS attempts spec gives kind, over needsFor's
// reasons too.
func casAttempts(kind string, spec effectSpec) int {
	n := cmp.Or(spec.needs.Attempts, 3)
	if spec.needsFor != nil {
		for _, reason := range []string{"", "idle", "no-wake-reason", "orphaned", "suspended", "idle-respawn"} {
			n = min(n, cmp.Or(spec.needsFor(&World{}, intent{Kind: kind, Reason: reason}).Attempts, 3))
		}
	}
	return n
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
		s := k.run(context.Background(), effectSpec{sections: []section{{Decide: mark(txKeyA)}}})
		want := settledFailed
		if at == seamAfterWrite {
			want = settledAmbiguous
		}
		if s.Outcome != want || s.Cause != causeInjected || !errors.Is(s.Err, boom) || (k.meta(txKeyA) == "1") != (at == seamAfterWrite) {
			t.Fatalf("seam %d: settlement %+v, a=%q; want outcome %d %q, written only past the write", at, s, k.meta(txKeyA), want, causeInjected)
		}
	}
	k := newTxKit(t)
	k.on(seamBeforeCAS, func() { k.outside(txKeyNote, "x") }) // loses the first CAS
	k.run(context.Background(), effectSpec{sections: []section{{Decide: mark(txKeyA)}, {Decide: mark(txKeyB)}}})
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
// lock, holds only these fields, and a Probe only the read-only stores; a
// Call's handles (provider start, routing, release, kill) go on txCaps
// alone.
func TestAroundCapsAreScoped(t *testing.T) {
	if got := reflect.TypeOf(section{}.probe).In(1); got != reflect.TypeFor[effectReads]() {
		t.Errorf("a Probe holds %v, want effectReads alone", got)
	}
	allowed := map[string]bool{"it": true, "reads": true}
	for i, ty := 0, reflect.TypeFor[aroundCaps](); i < ty.NumField(); i++ {
		if f := ty.Field(i).Name; !allowed[f] {
			t.Errorf("aroundCaps.%s: around runs outside every lock; a write or provider handle belongs to a Call", f)
		}
	}
}

// Kills a finalizer that some exit skips (C5a1's endpoint ticket, SC N1): a
// Call's onExit runs once with the final settlement whether the effect
// lands, refuses after the Call, or panics.
func TestTxOnExitResolvesEveryExit(t *testing.T) {
	admit := func(got *[]settlement) section {
		pre := section{Decide: func(txView) txStep { return txStep{Write: session.MetadataPatch{"state": "creating"}} }}
		return called(callStart, pre, func(_ context.Context, c txCaps, _ any) (any, error) {
			c.onExit(func(final settlement) { *got = append(*got, final) })
			return nil, nil
		})
	}
	for _, c := range []struct {
		name   string
		next   func(txView) txStep
		want   settleOutcome
		panics bool
	}{
		{"lands", mark(txKeyA), settledLanded, false},
		{"refused after the call", func(txView) txStep { return txStep{Refuse: "commit-lost"} }, settledRefused, false},
		{"panics", func(txView) txStep { panic("boom") }, settledFailed, true},
	} {
		k := newTxKit(t)
		var got []settlement
		spec := effectSpec{sections: []section{admit(&got), {Premise: premiseOwnToken, Decide: c.next}}}
		func() {
			defer func() {
				if r := recover(); (r != nil) != c.panics {
					t.Errorf("%s: recovered %v", c.name, r)
				}
			}()
			k.run(context.Background(), spec)
		}()
		if len(got) != 1 || got[0].Outcome != c.want {
			t.Fatalf("%s: finalizer saw %+v, want one call with outcome %d", c.name, got, c.want)
		}
	}
}

// Kills read-only stores reaching a Probe without the grant, or writable
// with it, a needsFor that cannot see the World, an L5-only fence refused
// for want of a route, a Probe result leaking into the next section, and a
// seam before a Call that cannot fail it.
func TestTxReadStoresNeedsForProbeResetAndCallSeam(t *testing.T) {
	k := newTxKit(t)
	var reads effectReads
	probe := probed(func(_ context.Context, r effectReads, _ txView) (int, error) { reads = r; return 7, nil }, func(txView, int, error) txStep { return txStep{} })
	k.run(context.Background(), effectSpec{sections: []section{probe}})
	if reads.legs.work != nil {
		t.Fatal("a Probe reached the stores without capReadStores")
	}
	var leaked any
	next := section{Decide: func(v txView) txStep { leaked = v.probe; return txStep{} }}
	k.run(context.Background(), effectSpec{caps: capReadStores, sections: []section{probe, next}})
	if reads.legs.work == nil || leaked != nil {
		t.Fatalf("stores %+v, next section's probe %v; want the stores granted and the result not carried", reads, leaked)
	}
	if _, err := reads.legs.work.Create(beads.Bead{}); !errors.Is(err, errBlindWriteRefused) {
		t.Fatalf("the granted city store wrote: %v", err)
	}
	var sawWorld *World
	var work *txWork
	k.p.Runtime = nil // L5 alone needs no route
	spec := effectSpec{
		needsFor: func(w *World, _ intent) needs { sawWorld = w; return needs{Legs: legWork} },
		sections: []section{{Decide: func(v txView) txStep { work = v.Fence.Work; return txStep{} }}},
	}
	if s := k.run(context.Background(), spec); sawWorld != k.p.World || work == nil || s.Cause == causeRouteUnknown {
		t.Fatalf("settlement %+v, world %p, work %+v; want needsFor given the pass's World and L5 read without a route", s, sawWorld, work)
	}
	k = newTxKit(t)
	k.fail = map[txSeam]error{seamBeforeCall: errors.New("staging fault")}
	ran := false
	if s := k.run(context.Background(), effectSpec{sections: []section{callOnly(func() { ran = true })}}); s.Cause != causeInjected || ran {
		t.Fatalf("settlement %+v (called %t), want the seam to fail it before the call", s, ran)
	}
}

// callOnly is a section that writes nothing and calls ran.
func callOnly(ran func()) section {
	return called(callStop, section{Decide: func(txView) txStep { return txStep{} }}, func(context.Context, txCaps, any) (any, error) { ran(); return nil, nil })
}

// panickingList is a store whose list reads panic.
type panickingList struct{ beads.Store }

func (panickingList) List(beads.ListQuery) ([]beads.Bead, error) { panic("review: store exploded") }

// Kills a bounded read that does not recover (review must-fix 3), an
// expired or failed L5 read counted as no work (review mutant FL2), and a
// Probe given the effect's context rather than its bounded one: a
// panicking Probe reaches Decide as an error, a panicking L5 read counts as
// work, and the Probe's context carries its bound.
func TestTxBoundedReadsFailClosed(t *testing.T) {
	k := newTxKit(t)
	var probeErr error
	bounded := false
	sec := probed(func(ctx context.Context, _ effectReads, _ txView) (int, error) {
		_, bounded = ctx.Deadline()
		panic("review: probe exploded")
	}, func(_ txView, _ int, err error) txStep { probeErr = err; return txStep{} })
	if s := k.run(context.Background(), effectSpec{sections: []section{sec}}); s.Outcome != settledNoop || probeErr == nil || !bounded {
		t.Fatalf("settlement %+v, probe error %v, bounded %t; want the panic read as a failed, bounded Probe", s, probeErr, bounded)
	}
	k.p.reads.legs = workLegsFromCensus("", nil, cityWorkStore{store: blindWriteRefusingStore{inner: panickingList{k.cache}}}, nil)
	var w *txWork
	k.run(context.Background(), effectSpec{needs: needs{Legs: legWork}, sections: []section{{Decide: func(v txView) txStep { w = v.Fence.Work; return txStep{} }}}})
	if w == nil || w.Free || w.Err == nil {
		t.Fatalf("work %+v, want a panicking read counted as work", w)
	}
}

// Kills a Probe shown the pass's cached row rather than the expected one
// (review mutant F5): after a section's write lands, the next section's
// Probe sees the row that write left.
func TestTxProbeSeesTheExpectedRow(t *testing.T) {
	k := newTxKit(t)
	var state string
	first := section{Decide: func(txView) txStep { return txStep{Write: session.MetadataPatch{"state": "creating"}} }}
	next := probed(func(_ context.Context, _ effectReads, v txView) (string, error) { return v.Row.MetadataState, nil },
		func(_ txView, p string, _ error) txStep { state = p; return txStep{} })
	k.run(context.Background(), effectSpec{sections: []section{first, next}})
	if state != "creating" {
		t.Fatalf("the Probe saw state %q, want the landed write's creating", state)
	}
}

// routedTo is a composite routing every name to leaf, answering the legs
// itself otherwise.
type routedTo struct {
	runtime.Provider
	leaf runtime.Provider
}

func (r routedTo) RouteFor(string) runtime.Route {
	return runtime.Route{Backend: runtime.Backend{Label: "leaf", Provider: r.leaf}, Known: true}
}

// Kills legs read on the composite rather than the routed leaf (review
// mutant FL5): with the composite detached and the leaf attached, L3 holds,
// whether or not the runtime was read first.
func TestTxReadsTheLegsOnTheRoutedLeaf(t *testing.T) {
	for _, rt := range []bool{false, true} {
		k := newTxKit(t)
		k.p.Runtime = routedTo{Provider: legLeaf{recordingLeaf: k.leaf}, leaf: legLeaf{recordingLeaf: k.leaf, attached: true}}
		var f txFence
		k.run(context.Background(), effectSpec{needs: needs{Runtime: rt, Legs: legAttach}, sections: []section{{Decide: func(v txView) txStep { f = v.Fence; return txStep{} }}}})
		if f.Attach != fenceAttached {
			t.Fatalf("runtime read %t: fence %+v, want attached on the routed leaf", rt, f)
		}
	}
}

// Kills a premise that does not pin what L5 read (review pin, should-fix
// 6): an alias moved since the pass, with work assigned to the new one,
// refuses rather than reading the old alias's work.
func TestTxWorkReadMatchesTheRowThePremiseHolds(t *testing.T) {
	m := beads.NewMemStoreFrom(0, []beads.Bead{sessionRow("gc-1", "template", "worker", "state", "asleep", "session_name", "s-gc-1", "alias", "old-alias", "generation", "1")}, nil)
	k := newTxKitOn(t, m, simBacking{m})
	k.outside("alias", "new-alias")
	if _, err := k.backing.Create(beads.Bead{Title: "task", Type: "task", Status: "open", Assignee: "new-alias"}); err != nil {
		t.Fatal(err)
	}
	var w *txWork
	s := k.run(context.Background(), effectSpec{needs: needs{Legs: legWork}, sections: []section{{Decide: func(v txView) txStep { w = v.Fence.Work; return txStep{} }}}})
	if s.Cause != causePremise {
		t.Fatalf("settlement %+v, work %+v; want refused %q: L5 read the old alias", s, w, causePremise)
	}
}

// Kills read-only stores that resolve a conditional writer (review pin N7):
// a Probe holding them finds no writer, so it cannot write the row.
func TestTxReadStoresAreReadOnly(t *testing.T) {
	k := newTxKit(t)
	var w beads.ConditionalWriter
	var err error
	sec := probed(func(_ context.Context, r effectReads, _ txView) (int, error) {
		w, _, err = beads.ResolveConditionalWriter(r.legs.work)
		return 0, nil
	}, func(txView, int, error) txStep { return txStep{} })
	var around effectReads
	k.run(context.Background(), effectSpec{caps: capReadStores, sections: []section{sec}, around: func(_ context.Context, a aroundCaps, run func() settlement) settlement {
		around = a.reads
		return run()
	}})
	if w != nil || around.legs.work == nil {
		t.Fatalf("the read-only city store resolved a conditional writer (%v); around got stores %t", err, around.legs.work != nil)
	}
}

// Kills a finalizer's panic skipping the others, and an order other than
// LIFO (review pins N8, OE3): every finalizer runs, last registered first,
// and the first panic, the effect's own before a finalizer's, is raised
// again.
func TestTxOnExitRunsEveryFinalizer(t *testing.T) {
	k := newTxKit(t)
	var order []int
	sec := called(callStart, section{Decide: func(txView) txStep { return txStep{} }}, func(_ context.Context, c txCaps, _ any) (any, error) {
		c.onExit(func(settlement) { order = append(order, 1) }) // the ticket, registered first
		c.onExit(func(settlement) { order = append(order, 2); panic("a later finalizer") })
		c.onExit(func(settlement) { order = append(order, 3) })
		return nil, nil
	})
	var r any
	func() {
		defer func() { r = recover() }()
		k.run(context.Background(), effectSpec{sections: []section{sec}})
	}()
	if !slices.Equal(order, []int{3, 2, 1}) || r != "a later finalizer" {
		t.Fatalf("finalizers ran %v, recovered %v; want [3 2 1] and the finalizer's panic raised again", order, r)
	}
	boom := section{Decide: func(txView) txStep { panic("the effect") }}
	func() {
		defer func() { r = recover() }()
		k.run(context.Background(), effectSpec{sections: []section{sec, boom}})
	}()
	if r != "the effect" {
		t.Fatalf("recovered %v, want the effect's own panic before any finalizer's", r)
	}
}

// closeSec is a close section writing close_reason, then (when later is set)
// a section that must never run after a landed close.
func closeSec() section {
	return section{Premise: premiseClose, Decide: func(txView) txStep {
		return txStep{Terminal: session.MetadataPatch{"close_reason": "orphaned"}, Facts: effectFacts{Events: []events.Event{{Type: "closed"}}}}
	}}
}

func (k *txKit) status() string {
	k.t.Helper()
	b, err := k.backing.Get(k.it.Key.ID)
	if err != nil {
		k.t.Fatal(err)
	}
	return b.Status
}

// Kills a close verb that closes over a wake or a kill fence, at a stale
// revision, past its deadline or the executor's abandonment, through the
// two-write fallback, or lets a later section write; and one that fails a
// row already closed: each case below, against the conformance re-check's
// §3.
func TestTxCloseVerb(t *testing.T) {
	ran := false
	later := section{Premise: premiseOwnToken, Decide: func(v txView) txStep { ran = true; return mark(txKeyB)(v) }}
	k := newCloseTxKit(t)
	if s := k.run(context.Background(), effectSpec{sections: []section{closeSec(), later}}); s.Outcome != settledLanded || len(s.Facts.Events) != 1 || ran || k.status() != "closed" || k.meta("close_reason") != "orphaned" {
		t.Fatalf("close: settlement %+v (later ran %t), row %s; want landed and closed, nothing after", s, ran, k.status())
	}
	if s := k.run(context.Background(), effectSpec{sections: []section{closeSec(), later}}); s.Outcome != settledNoop || ran {
		t.Fatalf("already closed: settlement %+v (later ran %t), want a no-op that ends the effect", s, ran)
	}
	for _, c := range []struct {
		name  string
		setup func(k *txKit, cancel context.CancelCauseFunc, l *writeLatch)
		cause string
	}{
		{"a wake after the pass", func(k *txKit, _ context.CancelCauseFunc, _ *writeLatch) { k.outside("wake_request", "api") }, causePremise},
		{"a wake at the close's revision", func(k *txKit, _ context.CancelCauseFunc, _ *writeLatch) {
			k.on(seamBeforeCAS, func() { k.outside("wake_request", "api") }) // loses the fence; the re-read sees it
		}, causePremise},
		{"a kill fence since the pass", func(k *txKit, _ context.CancelCauseFunc, _ *writeLatch) {
			k.on(seamAfterReads, func() { _ = k.backing.SetMetadataBatch(k.it.Key.ID, session.KillPendingPatch(gatherNow)) })
		}, causePremise},
		{"past the deadline", func(k *txKit, cancel context.CancelCauseFunc, _ *writeLatch) {
			k.on(seamBeforeCAS, func() { cancel(context.DeadlineExceeded) })
		}, causeDeadline},
		{"after the abandonment", func(k *txKit, _ context.CancelCauseFunc, l *writeLatch) { k.on(seamBeforeCAS, func() { l.abandon() }) }, causeDeadline},
	} {
		k := newCloseTxKit(t)
		ctx, cancel := context.WithCancelCause(context.Background())
		latch := new(writeLatch)
		c.setup(k, cancel, latch)
		if s := runTx(ctx, k.p, k.it, effectSpec{sections: []section{closeSec()}}, latch); s.Cause != c.cause || k.status() != "open" || k.meta("close_reason") != "" {
			t.Fatalf("%s: settlement %+v, row %s; want refused or failed %q, the row open and untouched", c.name, s, k.status(), c.cause)
		}
		cancel(nil)
	}
	m := beads.NewAtomicCloseMemStore() // a kill fence the pass already saw: only the kill check refuses
	fenced := poolRow("gc-1", "worker", 1, "asleep")
	maps.Copy(fenced.Metadata, session.KillPendingPatch(gatherNow))
	if _, err := m.Create(fenced); err != nil {
		t.Fatal(err)
	}
	if k = newTxKitOn(t, m, exactBacking{m}); k.run(context.Background(), effectSpec{sections: []section{closeSec()}}).Cause != causePremise || k.status() != "open" {
		t.Fatalf("a kill fence at the pass: row %s, want refused %q and the row open", k.status(), causePremise)
	}
	k = newTxKit(t) // a plain MemStore: no atomic closer
	if s := k.run(context.Background(), effectSpec{sections: []section{closeSec()}}); s.Cause != causeNoWriter || k.status() != "open" || k.meta("close_reason") != "" {
		t.Fatalf("no atomic closer: settlement %+v, row %s; want refused %q with no fallback write", s, k.status(), causeNoWriter)
	}
	k = newCloseTxKit(t)
	misuse := section{Decide: func(txView) txStep { return txStep{Terminal: session.MetadataPatch{"close_reason": "x"}} }}
	if s := k.run(context.Background(), effectSpec{sections: []section{misuse}}); s.Cause != causeStep || k.status() != "open" {
		t.Fatalf("a close outside a close section: settlement %+v, want failed %q", s, causeStep)
	}
}

// terminal is a close section closing with close_reason=orphaned.
var terminal = section{Premise: premiseClose, Decide: func(txView) txStep {
	return txStep{Terminal: session.MetadataPatch{"close_reason": "orphaned"}}
}}

// Kills a Write dropped in a close section (review pin N9): it fails step,
// writing nothing, with no write begun.
func TestTxCloseSectionFailsAWrite(t *testing.T) {
	k := newCloseTxKit(t)
	sec := section{Premise: premiseClose, Decide: func(txView) txStep { return txStep{Write: session.MetadataPatch{txKeyNote: "x"}} }}
	latch := new(writeLatch)
	if s := runTx(context.Background(), k.p, k.it, effectSpec{sections: []section{sec}}, latch); s.Cause != causeStep || latch.begun() || k.meta(txKeyNote) != "" {
		t.Fatalf("settlement %+v (latch begun %t), want failed %q", s, latch.begun(), causeStep)
	}
}

// Kills the close's kill check read on the pass's row (review pin CV7): a
// repeat kill on a row asleep and killed moves only fields the lifecycle
// facts leave out, so only the fresh row's kill check refuses.
func TestTxCloseRefusesARepeatKill(t *testing.T) {
	m := beads.NewAtomicCloseMemStore()
	if _, err := m.Create(poolRow("gc-1", "worker", 1, "asleep", "sleep_reason", "killed")); err != nil {
		t.Fatal(err)
	}
	k := newTxKitOn(t, m, exactBacking{m})
	k.on(seamAfterReads, func() { _ = m.SetMetadataBatch("gc-1", session.KillPendingPatch(gatherNow)) })
	if s := k.run(context.Background(), effectSpec{sections: []section{terminal}}); s.Cause != causePremise || k.status() != "open" {
		t.Fatalf("settlement %+v, row %s: closed over a repeat kill fence", s, k.status())
	}
}

// racingCloseBacking lands a write through the cache while the close's
// backing read is in flight, so RefreshRow answers ErrRowRefreshFenced once.
type racingCloseBacking struct {
	exactBacking
	cache **beads.CachingStore
	fired *bool
}

func (b racingCloseBacking) Get(id string) (beads.Bead, error) {
	got, err := b.exactBacking.Get(id)
	if id == "gc-1" && *b.cache != nil && !*b.fired {
		*b.fired = true
		_ = (*b.cache).SetMetadata(id, txKeyNote, "racing")
	}
	return got, err
}

// Kills a fenced refresh failing the close (review pin CV8), and a close
// section reporting the wrong attempt: the close reads again and lands;
// after a lost fence its seams report attempt 2.
func TestTxCloseRetries(t *testing.T) {
	m := beads.NewAtomicCloseMemStore()
	if _, err := m.Create(poolRow("gc-1", "worker", 1, "asleep")); err != nil {
		t.Fatal(err)
	}
	k := newTxKitOn(t, m, exactBacking{m})
	var cache *beads.CachingStore
	fired := false
	cache = beads.NewCachingStoreForTest(racingCloseBacking{exactBacking: exactBacking{m}, cache: &cache, fired: &fired}, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	fired = false
	k.p.Writers[rowLeg] = fencedWriter{store: cache}
	if s := k.run(context.Background(), effectSpec{sections: []section{terminal}}); !fired || k.status() != "closed" || s.Outcome != settledLanded {
		t.Fatalf("raced %t: settlement %+v, row %s; want the retry to close", fired, s, k.status())
	}
	k = newCloseTxKit(t)
	k.on(seamBeforeCAS, func() { k.outside(txKeyNote, "x") }) // loses the first close's fence
	if s := k.run(context.Background(), effectSpec{sections: []section{terminal}}); s.Outcome != settledLanded || k.attempts[len(k.attempts)-1] != 2 {
		t.Fatalf("settlement %+v, attempts %v at seams %v; want the close landed on attempt 2", s, k.attempts, k.seen)
	}
}

// Kills a close that around cannot tell apart (the conformance re-check),
// and a landed close without its after-write seam (review pin SE5): Closed
// marks only a close that landed, not an earlier write before a row already
// closed; a fault injected after a landed close settles ambiguous, closed.
func TestTxCloseLandedSignal(t *testing.T) {
	stamp := section{Decide: mark(txKeyA)}
	for _, c := range []struct {
		name     string
		closed   bool // another writer closed the row first
		sections []section
		want     bool
	}{
		{"close", false, []section{terminal}, true},
		{"stamp, then close", false, []section{stamp, terminal}, true},
		{"stamp, then already closed", true, []section{stamp, terminal}, false},
	} {
		k := newCloseTxKit(t)
		if c.closed {
			k.on(seamAfterWrite, func() { _ = k.backing.Close("gc-1") })
		}
		if s := k.run(context.Background(), effectSpec{sections: c.sections}); s.Closed != c.want || s.Outcome != settledLanded {
			t.Fatalf("%s: settlement %+v, want landed with Closed %t", c.name, s, c.want)
		}
	}
	k := newCloseTxKit(t)
	k.fail = map[txSeam]error{seamAfterWrite: errors.New("staging fault")}
	if s := k.run(context.Background(), effectSpec{sections: []section{terminal}}); s.Cause != causeInjected || s.Outcome != settledAmbiguous || !s.Closed || k.status() != "closed" {
		t.Fatalf("settlement %+v, row %s; want ambiguous and closed, the fault injected", s, k.status())
	}
}
