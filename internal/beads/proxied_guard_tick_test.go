package beads

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
)

// guardFixture is an admitted, long-lived proxied store with a fake ticker.
//
// The pin is a REAL admission pass against the admission fixture's record, not a
// PinForTest: the guard's whole job is to compare the world against a pin, and a
// hand-built pin with no PoolKey would make every generation comparison trivially
// true.
type guardFixture struct {
	t        *testing.T
	admitted *admissionFixture
	store    *ProxiedStore
	native   *NativeDoltStore
	bd       *recordingLeaf
	ticks    chan time.Time
	steps    chan proxiedGuardStep
	guard    *proxiedGuard

	// reopens counts the library opens the reconnect hook performed. The guard
	// must never drive this from its own goroutine.
	reopens chan struct{}
	// cursors is what the injected cursor read answers; drift is how a test moves
	// the database under the handle.
	cursors proxyendpoint.Cursors
	// owner is what the injected socket-owner join answers.
	owner proxiedOwner
	// probes counts the probe sessions admission ran through this fixture.
	probes int
	// probeResult, when set, replaces the served answer the injected probe
	// gives. It is how a test drives a re-admission that learns NOTHING about
	// the endpoint — the ordinary outcome on a loaded box, and the one a tick
	// must not demote on.
	probeResult func() proxyendpoint.ProbeResult
	// recover, when set, is the store's NativeLeafReopener: the recovery path a
	// non-terminally demoted handle has.
	recover func(context.Context) (*NativeDoltStore, Pin, error)
	// recoveries counts the calls the guard made to it.
	recoveries int
	// cursorReads counts the injected cursor sessions. It is the other half of
	// the per-tick budget: probes and cursor reads are both one connection to
	// bd's proxy, and bd's idle watcher counts every one.
	cursorReads int
}

// probe is the injected probe both admission and the tick run through. It
// answers served unless a test says otherwise.
func (f *guardFixture) probe(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult {
	f.probes++
	if f.probeResult != nil {
		return f.probeResult()
	}
	return proxyendpoint.ServedProbeForTest(pinnedCursors(), proxyendpoint.CursorReality{})
}

func newGuardFixture(t *testing.T) *guardFixture {
	t.Helper()
	admitted := newAdmissionFixture(t, "-1")
	f := &guardFixture{
		t:        t,
		admitted: admitted,
		ticks:    make(chan time.Time, 1),
		steps:    make(chan proxiedGuardStep, 8),
		reopens:  make(chan struct{}, 8),
		cursors:  pinnedCursors(),
		owner:    proxiedOwnerUndetermined,
	}

	pin, err := Admit(context.Background(), f.admissionInput(true))
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if !pin.Admitted() {
		t.Fatal("Admit returned an unadmitted pin")
	}

	storage := &nativeDoltMemStorage{store: &MemStore{IDPrefix: "prx", HonorExplicitIDs: true}}
	native := newNativeDoltStoreForTest(storage,
		WithProxiedReadOnly(),
		// The reconnect hook is what a re-pin actually runs, and it runs on the
		// READER's goroutine. Recording it is how the test proves the tick did
		// not open the library itself.
		WithNativeReopen(func(context.Context) (NativeStorage, error) {
			f.reopens <- struct{}{}
			return storage, nil
		}))
	native.idPrefix = "prx"
	writeLeaf := newNativeDoltStoreForTest(storage)
	writeLeaf.idPrefix = "prx"
	f.bd = &recordingLeaf{Store: writeLeaf}

	store, err := NewProxiedStore(native, f.bd, pin, WithNativeLeafReopener(
		func(ctx context.Context) (*NativeDoltStore, Pin, error) {
			f.recoveries++
			if f.recover == nil {
				return nil, Pin{}, errors.New("this fixture installed no recovery")
			}
			return f.recover(ctx)
		}))
	if err != nil {
		t.Fatalf("NewProxiedStore: %v", err)
	}
	f.store = store
	f.native = native
	return f
}

func (f *guardFixture) admissionInput(longLived bool) AdmissionInput {
	return AdmissionInput{
		ScopeRoot:    f.admitted.scopeRoot,
		Database:     "beads",
		ProcessTable: f.admitted.processTable(),
		Probe:        f.probe,
		LongLived:    longLived,
		Observed:     NewGenerationSet(),
		Recovered:    NewGenerationSet(),
		Sleep:        func(context.Context, time.Duration) error { return nil },
		SkipMemo:     true,
	}
}

// start installs the guard with every effect injected. Nothing here touches a
// clock, a socket, /proc or bd.
func (f *guardFixture) start() {
	f.t.Helper()
	f.guard = f.store.startGuard(proxiedGuardOptions{
		ticks:        f.ticks,
		processTable: f.admitted.processTable(),
		probe:        f.probe,
		cursors: func(context.Context, Pin) (proxyendpoint.Cursors, error) {
			f.cursorReads++
			return f.cursors, nil
		},
		owner:      func(context.Context, Pin) proxiedOwner { return f.owner },
		ownerEvery: 10,
		onStep:     func(step proxiedGuardStep) { f.steps <- step },
	})
	f.t.Cleanup(func() { _ = f.store.CloseStore() })
}

// tick delivers one tick and returns what it concluded.
func (f *guardFixture) tick() proxiedGuardStep {
	f.t.Helper()
	f.ticks <- time.Now()
	select {
	case step := <-f.steps:
		return step
	case <-time.After(5 * time.Second):
		f.t.Fatal("the guard did not report a step within 5s")
		return proxiedGuardStopped
	}
}

// TestProxiedGuardTickRepinsOnGenerationChange is design U22 (648-654): a bd
// proxy restart is ordinary operation, and the tick answers it by RE-PINNING, not
// by demoting. A tick that demoted would turn every `bd dolt stop`, every idle
// expiry and every proxy crash into a controller store that forks bd for the rest
// of the process — which is the whole cost this lane exists to remove.
//
// The second half of the test is the constraint that shapes the implementation:
// the tick must not open the library from its own goroutine. So the re-pin is
// "adopt the new pin, mark the pool stale", and the library open happens on the
// next READER's goroutine, through the reconnect hook.
func TestProxiedGuardTickRepinsOnGenerationChange(t *testing.T) {
	f := newGuardFixture(t)
	f.start()

	before := f.store.Pin()
	if step := f.tick(); step != proxiedGuardHeld {
		t.Fatalf("a steady generation reported %s, want held", step)
	}
	if _, owed := f.native.poolStaleOwed(); owed {
		t.Fatal("a steady tick invalidated the pool")
	}

	// bd replaced its proxy: a new pid, a new birth token, the same root.
	f.admitted.writeRecord(6002, "99887766")

	if step := f.tick(); step != proxiedGuardRepinned {
		t.Fatalf("a generation change reported %s, want repinned", step)
	}
	if f.store.Demoted() {
		t.Fatal("the tick DEMOTED on a generation change; design U22 says re-pin")
	}
	if verdict := f.store.Verdict(); verdict != nil {
		t.Fatalf("the re-pin left a verdict on the store: %v", verdict)
	}
	after := f.store.Pin()
	if after.Generation() == before.Generation() {
		t.Fatalf("the pin still names generation %s; the re-admission did not land", after.Generation())
	}
	if after.PoolKey().PID != 6002 {
		t.Fatalf("re-pinned to pid %d, want bd's new proxy 6002", after.PoolKey().PID)
	}
	if _, owed := f.native.poolStaleOwed(); !owed {
		t.Fatal("the re-pin did not invalidate the pool, so the next read would be served from the OLD generation")
	}
	select {
	case <-f.reopens:
		t.Fatal("the guard opened the library from its own goroutine")
	default:
	}

	// And the next read re-points the pool, on the caller's goroutine.
	if _, err := f.store.List(ListQuery{AllowScan: true, TierMode: TierBoth}); err != nil {
		t.Fatalf("List after a re-pin: %v", err)
	}
	select {
	case <-f.reopens:
	default:
		t.Fatal("the first read after a re-pin did not reconnect; it was served from the old pool")
	}
	if _, owed := f.native.poolStaleOwed(); owed {
		t.Fatal("the stale mark survived the reconnect, so every later read would re-pin again")
	}
}

// TestProxiedGuardTickRepinRefusesWhenTheNewGenerationDoesNotAdmit pins the
// other half of step 1: a re-pin is an ADMISSION, not a re-point. A new
// generation whose database has moved to another schema must not be adopted
// merely because bd restarted the proxy.
func TestProxiedGuardTickRepinRefusesWhenTheNewGenerationDoesNotAdmit(t *testing.T) {
	f := newGuardFixture(t)
	f.start()
	// The replacement proxy is not ours at all: a record with a foreign root_id,
	// which admission refuses without ever dialing.
	f.admitted.corrupt(func(rec *proxyendpoint.Record) {
		rec.PID = 6003
		rec.Birth = proxyendpoint.BirthToken("boot-fixture", "12121212")
		rec.RootID = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	})

	if step := f.tick(); step != proxiedGuardStoodDown {
		t.Fatalf("a foreign replacement record reported %s, want stood-down", step)
	}
	verdict := f.store.Verdict()
	if verdict == nil || verdict.Verdict != ProxiedVerdictNotOurs {
		t.Fatalf("verdict = %v, want not_ours", verdict)
	}
	if !f.store.Demoted() {
		t.Fatal("the store kept serving natively against a record that is not ours")
	}
}

// TestProxiedGuardTickDemotesOnCursorDrift is the one demotion the design asks a
// tick to make (563-565).
//
// A cursor pair that moved is a fact about the database: somebody migrated it
// under a handle whose linked library pins the pair admission checked. Re-pinning
// could only re-learn it, so the verdict is terminal and the handle stays on the
// bd leaf for its lifetime.
func TestProxiedGuardTickDemotesOnCursorDrift(t *testing.T) {
	f := newGuardFixture(t)
	f.start()

	if step := f.tick(); step != proxiedGuardHeld {
		t.Fatalf("an unmoved database reported %s, want held", step)
	}

	// Somebody ran a migration against the shared database.
	f.cursors = proxyendpoint.Cursors{Main: SchemaCursorMain + 1, Ignored: SchemaCursorIgnored}
	if step := f.tick(); step != proxiedGuardStoodDown {
		t.Fatalf("cursor drift reported %s, want stood-down", step)
	}
	verdict := f.store.Verdict()
	if verdict == nil || verdict.Verdict != ProxiedVerdictSchemaSkew {
		t.Fatalf("verdict = %v, want schema_skew", verdict)
	}
	if verdict.Lane != ProxiedSkewLaneMain || verdict.Dir != ProxiedSkewDirAhead {
		t.Errorf("verdict lane/dir = %q/%q, want main/ahead", verdict.Lane, verdict.Dir)
	}
	if !verdict.Terminal() {
		t.Error("schema drift must be terminal for the handle: a re-pin could only re-learn it")
	}
	if !f.store.Demoted() {
		t.Fatal("cursor drift left the native leaf serving")
	}
	// The reads keep working, from bd, which is the store a proxied scope has today.
	if _, err := f.store.List(ListQuery{AllowScan: true, TierMode: TierBoth}); err != nil {
		t.Fatalf("the demoted store stopped serving: %v", err)
	}
	// And a terminal stand-down retires the guard rather than re-checking a fact
	// forever.
	if step := f.tick(); step != proxiedGuardStopped {
		t.Fatalf("a tick after a terminal stand-down reported %s, want stopped", step)
	}
	select {
	case <-f.guard.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the guard goroutine outlived a terminal stand-down")
	}

	// The drifted lane and direction, for the ignored lane and the other
	// direction, without re-running the whole fixture.
	if lane, dir := cursorDriftAgainst(pinnedCursors(), proxyendpoint.Cursors{
		Main: SchemaCursorMain, Ignored: SchemaCursorIgnored - 1,
	}); lane != ProxiedSkewLaneIgnored || dir != ProxiedSkewDirBehind {
		t.Errorf("cursorDriftAgainst(ignored behind) = %q/%q, want ignored/behind", lane, dir)
	}
}

// TestProxiedGuardTickUndecidedReadsDecideNothing is the tri-state rule, which is
// what keeps a loaded box from demoting a healthy city.
//
// An unreadable record is bd rewriting it (bd removes the record on an orderly
// stop and writes it again on start), a failed cursor read is our own session's
// clock, and an unreadable /proc is a host that does not permit the join. None of
// the three is evidence about the endpoint, and none may decide.
func TestProxiedGuardTickUndecidedReadsDecideNothing(t *testing.T) {
	t.Run("an unreadable record", func(t *testing.T) {
		f := newGuardFixture(t)
		f.start()
		f.admitted.removeRecord()
		if step := f.tick(); step != proxiedGuardUndecided {
			t.Fatalf("an absent record reported %s, want undecided", step)
		}
		if f.store.Demoted() {
			t.Fatal("an absent record demoted the store; bd removes it on every orderly stop")
		}
	})

	t.Run("a failed cursor read", func(t *testing.T) {
		f := newGuardFixture(t)
		f.guard = f.store.startGuard(proxiedGuardOptions{
			ticks:        f.ticks,
			processTable: f.admitted.processTable(),
			probe:        servedProbe(pinnedCursors(), &f.probes),
			cursors: func(context.Context, Pin) (proxyendpoint.Cursors, error) {
				return proxyendpoint.Cursors{}, errors.New("context deadline exceeded")
			},
			owner:      func(context.Context, Pin) proxiedOwner { return proxiedOwnerUndetermined },
			ownerEvery: 10,
			onStep:     func(step proxiedGuardStep) { f.steps <- step },
		})
		t.Cleanup(func() { _ = f.store.CloseStore() })
		if step := f.tick(); step != proxiedGuardUndecided {
			t.Fatalf("a failed cursor read reported %s, want undecided", step)
		}
		if f.store.Demoted() {
			t.Fatal("a failed cursor read demoted the store; a zero cursor pair is not drift")
		}
	})

	t.Run("an undetermined socket owner never decides", func(t *testing.T) {
		f := newGuardFixture(t)
		f.owner = proxiedOwnerUndetermined
		f.start()
		// Reach the owner rung: it runs on every tenth tick.
		var last proxiedGuardStep
		for range 10 {
			last = f.tick()
		}
		if last != proxiedGuardHeld {
			t.Fatalf("the owner rung reported %s on an undetermined join, want held", last)
		}
		if f.store.Demoted() {
			t.Fatal("an undetermined socket-owner join demoted the store")
		}
	})
}

// TestProxiedGuardTickDemotesOnAForeignSocketOwner is the rung no other check can
// reach: a port taken over by a process that is not bd's proxy still has bd's
// (stale) record beside it and may well serve a database, so neither the record
// check nor the cursor check can see it.
func TestProxiedGuardTickDemotesOnAForeignSocketOwner(t *testing.T) {
	f := newGuardFixture(t)
	f.owner = proxiedOwnerForeign
	f.start()

	var last proxiedGuardStep
	for i := range 10 {
		last = f.tick()
		if i < 9 && last != proxiedGuardHeld {
			t.Fatalf("tick %d reported %s before the owner rung, want held", i+1, last)
		}
	}
	if last != proxiedGuardStoodDown {
		t.Fatalf("a foreign socket owner reported %s, want stood-down", last)
	}
	verdict := f.store.Verdict()
	if verdict == nil || verdict.Verdict != ProxiedVerdictNotOurs {
		t.Fatalf("verdict = %v, want not_ours", verdict)
	}
}

// TestProxiedGuardTickStopsOnCloseStore is the lifecycle contract: the ticker is
// the store's, so closing the store joins the goroutine instead of leaving it
// probing a proxy for a handle nobody holds. A guard that outlived its store
// would keep one probe session per interval alive for the life of the process.
func TestProxiedGuardTickStopsOnCloseStore(t *testing.T) {
	f := newGuardFixture(t)
	f.guard = f.store.startGuard(proxiedGuardOptions{
		ticks:        f.ticks,
		processTable: f.admitted.processTable(),
		probe:        servedProbe(pinnedCursors(), &f.probes),
		cursors:      func(context.Context, Pin) (proxyendpoint.Cursors, error) { return f.cursors, nil },
		owner:        func(context.Context, Pin) proxiedOwner { return f.owner },
		ownerEvery:   10,
		onStep:       func(step proxiedGuardStep) { f.steps <- step },
	})
	if step := f.tick(); step != proxiedGuardHeld {
		t.Fatalf("first tick reported %s, want held", step)
	}

	if err := f.store.CloseStore(); err != nil {
		t.Fatalf("CloseStore: %v", err)
	}
	select {
	case <-f.guard.done:
	case <-time.After(5 * time.Second):
		t.Fatal("CloseStore returned while the guard goroutine was still running")
	}
}

// TestProxiedGuardStartIsIdempotentPerStore pins that a second StartGuard stops
// the first. A re-open path that installed two guards would double every cost the
// tick pays and race two re-pins against one handle.
func TestProxiedGuardStartIsIdempotentPerStore(t *testing.T) {
	f := newGuardFixture(t)
	f.start()
	first := f.guard
	f.start()
	select {
	case <-first.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the first guard survived a second StartGuard")
	}
	if step := f.tick(); step != proxiedGuardHeld {
		t.Fatalf("the second guard reported %s, want held", step)
	}
}

// TestProxiedOpenFiniteIdleLongLivedFallsToBdStore is the idle rule, and the
// deliberate PR2 deviation from design 537-550 (which opens the controller store
// per reconcile pass instead).
//
// bd retires a finite-idle proxy AND its Dolt child after a quiet window, so a
// handle gc held across one would be pinned to a process bd has decided to stop.
// PR2 refuses the long-lived native open and keeps BdStore for such a scope —
// and the refusal is DOCTOR-VISIBLE, which is the condition the deviation was
// accepted under (plan Q1, decision: accept + file a follow-up for PR4). The last
// third of this test is that visibility, asserted through the factory arm that
// builds the payload doctor reads.
func TestProxiedOpenFiniteIdleLongLivedFallsToBdStore(t *testing.T) {
	f := newAdmissionFixture(t, "30")
	probes := 0
	// The memo is LIVE (council C-F2). Production does not set SkipMemo, and
	// with a lane-blind memo key the one-shot admission below would hand its
	// pass to the long-lived open at the end of this test.
	ForgetProxiedPin(f.scopeRoot, "beads")
	t.Cleanup(func() { ForgetProxiedPin(f.scopeRoot, "beads") })
	input := AdmissionInput{
		ScopeRoot:    f.scopeRoot,
		Database:     "beads",
		ProcessTable: f.processTable(),
		Probe:        servedProbe(pinnedCursors(), &probes),
		Observed:     NewGenerationSet(),
		Recovered:    NewGenerationSet(),
		Sleep:        func(context.Context, time.Duration) error { return nil },
	}

	input.LongLived = true
	_, err := Admit(context.Background(), input)
	verdict, ok := ProxiedVerdictOf(err)
	if !ok || verdict.Verdict != ProxiedVerdictIdlePolicyFinite {
		t.Fatalf("Admit(long-lived, finite idle) = %v, want the idle_policy_finite verdict", err)
	}
	if probes != 0 {
		t.Errorf("the idle rule spent %d probe session(s); it is decided from the sidecar and argv alone", probes)
	}

	// The same scope admits fine for a one-shot: the rule is about holding a
	// handle across an idle expiry, not about the scope being unservable.
	input.LongLived = false
	if _, err := Admit(context.Background(), input); err != nil {
		t.Fatalf("Admit(one-shot, finite idle): %v", err)
	}

	// Doctor visibility. The factory's proxied arm turns the verdict into the
	// additive diagnostic field the beads-store payload projects, under the
	// unchanged proxied_provider gate.
	t.Setenv(nativeForceFallbackEnv, "")
	t.Setenv(proxiedNativeEnv, "1")
	fallback := NewMemStore()
	result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:        f.scopeRoot,
		Provider:         "bd",
		PreflightChecker: refusingPreflightChecker(t),
		LongLived:        true,
		OpenBdStore:      func() (Store, error) { return fallback, nil },
		OpenProxiedStore: func(ctx context.Context, longLived bool) (Store, ProxiedOpenReport, error) {
			pin, admitErr := Admit(ctx, func() AdmissionInput {
				in := input
				in.LongLived = longLived
				return in
			}())
			return nil, pin.Report(), admitErr
		},
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity: %v", err)
	}
	if result.Diagnostic.Store != BeadsStoreNameBdStore {
		t.Fatalf("beads_store = %q, want BdStore for a finite-idle long-lived open", result.Diagnostic.Store)
	}
	if result.Diagnostic.PreflightGate != BeadsGateProxiedProvider {
		t.Errorf("preflight_gate = %q, want proxied_provider unchanged", result.Diagnostic.PreflightGate)
	}
	if result.Diagnostic.Proxied == nil || result.Diagnostic.Proxied.Verdict != ProxiedVerdictIdlePolicyFinite {
		t.Fatalf("proxied diagnostic = %+v, want verdict idle_policy_finite; the deviation was accepted only as a VISIBLE one",
			result.Diagnostic.Proxied)
	}
}

// installRecovery gives the fixture's store the recovery a production
// long-lived handle has: a ONE-session re-admission (ProbeOnce, no Ops) against
// whatever the record names now, and a fresh leaf over it. It is what
// cmd/gc's recoverNativeLeaf does, minus the library open.
func (f *guardFixture) installRecovery() {
	f.recover = func(ctx context.Context) (*NativeDoltStore, Pin, error) {
		in := f.admissionInput(true)
		in.ProbeOnce = true
		pin, err := Admit(ctx, in)
		if err != nil {
			return nil, Pin{}, err
		}
		leaf := newNativeDoltStoreForTest(&nativeDoltMemStorage{store: &MemStore{IDPrefix: "prx", HonorExplicitIDs: true}})
		leaf.idPrefix = "prx"
		return leaf, pin, nil
	}
}

// TestProxiedGuardTickStandsDownNonTerminallyWhenTheNewGenerationDoesNotAdmit
// is council A-F1 as amended by council pr2 E-S1.
//
// bd replacing its proxy is ORDINARY operation — an operator `bd dolt stop`, an
// idle expiry, a crash-restart — and the tick answers it by RE-PINNING (design
// U22, 648-654). The re-pin is an admission, and on a loaded box its one probe
// session routinely learns nothing (budget_exhausted, NON-terminal).
//
// A-F1's point stands: that outcome must never demote the handle for good. What
// changed is how "not for good" is achieved. The tick used to leave the pin and
// the old pool in place and report Undecided; E-S1 showed that serves the
// REPLACED generation for as long as the new one does not admit, which on a
// root move is the moved database. Now the handle stands down NON-terminally —
// reads go to bd, not to the replaced generation — and the next tick's recovery
// re-pins it once the new generation admits.
func TestProxiedGuardTickStandsDownNonTerminallyWhenTheNewGenerationDoesNotAdmit(t *testing.T) {
	f := newGuardFixture(t)
	f.installRecovery()
	f.start()
	before := f.store.Pin()

	// bd replaced its proxy...
	f.admitted.corrupt(func(rec *proxyendpoint.Record) {
		rec.PID = 6004
		rec.Birth = proxyendpoint.BirthToken("boot-fixture", "99887766")
	})
	// ...and the re-admission's probe runs out of its own clock.
	f.probeResult = func() proxyendpoint.ProbeResult {
		return proxyendpoint.ProbeResult{Outcome: proxyendpoint.ProbeUnknown, Err: context.DeadlineExceeded}
	}

	if step := f.tick(); step != proxiedGuardStoodDown {
		t.Fatalf("an indeterminate re-admission of a MOVED generation reported %s, want stood-down: "+
			"the old pool must not keep serving a generation the record says is gone", step)
	}
	if !f.store.Demoted() {
		t.Fatal("the tick kept the native leaf serving the replaced generation")
	}
	verdict := f.store.Verdict()
	if verdict == nil || verdict.Terminal() {
		t.Fatalf("verdict = %v, want a NON-terminal one: the probe's own clock is not evidence about the proxy, "+
			"and a terminal stand-down is the permanent demotion A-F1 forbids", verdict)
	}
	if verdict.Verdict != ProxiedVerdictBudgetExhausted {
		t.Errorf("verdict = %q, want the re-admission's own budget_exhausted kept for doctor", verdict.Verdict)
	}
	if !strings.Contains(verdict.Detail, before.Generation()) {
		t.Errorf("the verdict detail does not name the replaced generation %s: %s", before.Generation(), verdict.Detail)
	}

	// While the box stays loaded, the recovery asks once per tick and the
	// handle stays on bd. Nothing is permanent and nothing is served natively.
	for pass := 0; pass < 2; pass++ {
		if step := f.tick(); step != proxiedGuardUndecided {
			t.Fatalf("pass %d: a recovery that learned nothing reported %s, want undecided", pass, step)
		}
		if !f.store.Demoted() {
			t.Fatalf("pass %d: the store promoted itself on a recovery that did not admit", pass)
		}
	}

	// The box recovers. The very next tick re-pins, against the generation the
	// record has been naming all along.
	f.probeResult = nil
	if step := f.tick(); step != proxiedGuardRepinned {
		t.Fatalf("a healthy recovery after the indeterminate ones reported %s, want repinned", step)
	}
	if f.store.Demoted() {
		t.Fatal("the recovery left the store demoted")
	}
	if f.store.Verdict() != nil {
		t.Fatalf("the recovery left the stand-down verdict in place: %v", f.store.Verdict())
	}
	if after := f.store.Pin(); after.PoolKey().PID != 6004 {
		t.Fatalf("re-pinned to pid %d, want the replacement proxy 6004 (was %d)",
			after.PoolKey().PID, before.PoolKey().PID)
	}
}

// TestProxiedGuardTickDoesNotServeAMovedRootWhileTheNewProxyRefuses is council
// pr2 E-S1, on the shape the tick header calls "the hazard no read can see".
//
// The scope root was moved and recreated at the same path. bd's new proxy
// wrote its record and has not bound its listener yet, so the one probe session
// the tick may spend is REFUSED. The old proxy is alive and still serving the
// MOVED database on the socket this handle's pool holds, so every read on it
// succeeds and none of them reaches the reopen hook.
//
// Before E-S1 the tick reported Undecided and left the pool alone: the next
// read was served from the moved database, and so was every read until a tick
// found the new proxy serving. The rows assert what a READER sees after the
// tick, and that the recovery re-pins once the new proxy binds — each tick on
// one probe session.
func TestProxiedGuardTickDoesNotServeAMovedRootWhileTheNewProxyRefuses(t *testing.T) {
	f := newGuardFixture(t)
	f.installRecovery()
	f.start()

	// The moved root's replacement proxy: a new pid, a new birth, the same
	// path-derived root identity.
	f.admitted.writeRecord(6020, "20202020")
	refusing := true
	f.probeResult = func() proxyendpoint.ProbeResult {
		if refusing {
			return proxyendpoint.ProbeResult{Outcome: proxyendpoint.ProbeRefused}
		}
		return proxyendpoint.ServedProbeForTest(pinnedCursors(), proxyendpoint.CursorReality{})
	}

	probes := f.probes
	if step := f.tick(); step != proxiedGuardStoodDown {
		t.Fatalf("a replaced generation whose new proxy refuses reported %s, want stood-down", step)
	}
	if got := f.probes - probes; got != 1 {
		t.Fatalf("the tick spent %d probe session(s), want exactly 1", got)
	}
	verdict := f.store.Verdict()
	if verdict == nil || verdict.Verdict != ProxiedVerdictDraining || verdict.Terminal() {
		t.Fatalf("verdict = %v, want a non-terminal draining", verdict)
	}

	// A reader right after the tick. The old pool is the moved database's, so
	// the only acceptable server of this read is the bd leaf.
	f.bd.took()
	if _, err := f.store.List(ListQuery{AllowScan: true, TierMode: TierBoth}); err != nil {
		t.Fatalf("List after the stand-down: %v", err)
	}
	if calls := f.bd.took(); len(calls) != 1 || calls[0] != "List" {
		t.Fatalf("the read after the tick reached the bd leaf as %v, want [List]: "+
			"anything else is a read served from the replaced generation's pool", calls)
	}
	select {
	case <-f.reopens:
		t.Fatal("the stood-down handle reconnected its old pool instead of leaving the read to bd")
	default:
	}

	// The new proxy binds. The next tick's recovery re-pins onto it.
	refusing = false
	probes = f.probes
	if step := f.tick(); step != proxiedGuardRepinned {
		t.Fatalf("the tick after the new proxy bound reported %s, want repinned", step)
	}
	if got := f.probes - probes; got != 1 {
		t.Fatalf("the recovery tick spent %d probe session(s), want exactly 1", got)
	}
	if f.store.Demoted() {
		t.Fatal("the recovery left the store on the bd leaf")
	}
	if pid := f.store.Pin().PoolKey().PID; pid != 6020 {
		t.Fatalf("re-pinned to pid %d, want the moved root's new proxy 6020", pid)
	}
}

// TestProxiedGuardTickRecoversANonTerminallyDemotedHandle is the other half of
// council A-F1: the recovery path that (*ProxiedStore).repin did not have.
//
// A non-terminal stand-down can arrive from three places that are not the tick —
// the mutation bracket's generation change, a read whose budget ran out, a
// markPoolStale that could not be honored. Before this, all three were
// permanent: tickSteps returned Held on the theory that "the re-pin belongs to
// the read path (the reopen hook)", and with the native leaf nil that hook is
// unreachable by construction.
func TestProxiedGuardTickRecoversANonTerminallyDemotedHandle(t *testing.T) {
	t.Run("a fresh leaf is installed and the store is native again", func(t *testing.T) {
		f := newGuardFixture(t)
		f.start()
		replacement := newNativeDoltStoreForTest(&nativeDoltMemStorage{
			store: &MemStore{IDPrefix: "prx", HonorExplicitIDs: true},
		})
		replacement.idPrefix = "prx"
		f.recover = func(context.Context) (*NativeDoltStore, Pin, error) {
			return replacement, f.store.Pin(), nil
		}

		// A write found the generation moved: H6's bracket, non-terminal.
		f.store.standDown(NewNonTerminalProxiedVerdictError(ProxiedVerdictProxyGone,
			"the proxy generation changed across create", nil))
		if !f.store.Demoted() {
			t.Fatal("standDown left the store native")
		}

		if step := f.tick(); step != proxiedGuardRepinned {
			t.Fatalf("the recovery tick reported %s, want repinned", step)
		}
		if f.store.Demoted() {
			t.Fatal("the tick reported a re-pin and left the store on the bd leaf")
		}
		if f.store.Verdict() != nil {
			t.Fatalf("the re-pin left the stale verdict in place: %v", f.store.Verdict())
		}
		if f.recoveries != 1 {
			t.Fatalf("the guard called the recovery %d time(s), want exactly 1", f.recoveries)
		}
	})

	t.Run("a non-terminal refusal leaves the handle demoted and asks again", func(t *testing.T) {
		f := newGuardFixture(t)
		f.start()
		f.recover = func(context.Context) (*NativeDoltStore, Pin, error) {
			return nil, Pin{}, NewNonTerminalProxiedVerdictError(ProxiedVerdictDraining,
				"the proxy is still refusing after the drain ceiling", nil)
		}
		f.store.standDown(NewNonTerminalProxiedVerdictError(ProxiedVerdictProxyGone, "bd restarted its proxy", nil))

		for pass := 0; pass < 2; pass++ {
			if step := f.tick(); step != proxiedGuardUndecided {
				t.Fatalf("pass %d: a non-terminal recovery refusal reported %s, want undecided", pass, step)
			}
			if !f.store.Demoted() {
				t.Fatalf("pass %d: the store promoted itself on a refusal", pass)
			}
		}
		if f.recoveries != 2 {
			t.Fatalf("the guard asked %d time(s) across two ticks, want 2: a non-terminal refusal is retried", f.recoveries)
		}
	})

	t.Run("a terminal refusal latches and the tick stops", func(t *testing.T) {
		f := newGuardFixture(t)
		f.start()
		f.recover = func(context.Context) (*NativeDoltStore, Pin, error) {
			return nil, Pin{}, NewSchemaSkewVerdictError(ProxiedSkewLaneIgnored, ProxiedSkewDirBehind,
				"the database moved while this handle was demoted")
		}
		f.store.standDown(NewNonTerminalProxiedVerdictError(ProxiedVerdictProxyGone, "bd restarted its proxy", nil))

		if step := f.tick(); step != proxiedGuardStoodDown {
			t.Fatalf("a terminal recovery refusal reported %s, want stood-down", step)
		}
		verdict := f.store.Verdict()
		if verdict == nil || verdict.Verdict != ProxiedVerdictSchemaSkew || !verdict.Terminal() {
			t.Fatalf("verdict = %v, want a terminal schema_skew", verdict)
		}
		// The latch holds: the next tick has nothing left to guard, and the
		// recovery is never asked again.
		if step := f.tick(); step != proxiedGuardStopped {
			t.Fatalf("the tick after a terminal latch reported %s, want stopped", step)
		}
		if f.recoveries != 1 {
			t.Fatalf("the guard asked %d time(s) after a TERMINAL refusal, want exactly 1", f.recoveries)
		}
	})
}

// TestProxiedGuardTickSpendsOneSessionPerTick is the tick's stated budget, made
// an assertion.
//
// proxied_guard_tick.go's header promises what one tick may spend: "two
// 200-byte file reads (the record, and the root identity behind Validate), one
// probe session, and -- every proxiedGuardOwnerEvery-th tick -- a /proc read."
// The fixture counted its probe sessions and read the counter nowhere (council
// C-F9), so a tick that dialed bd's proxy twice, or on every tick instead of on
// a change, would have passed every existing guard test. A session is an
// accepted TCP connection bd's idle watcher counts and cannot arm while one is
// open, so the budget is the whole reason the tick is affordable at all.
func TestProxiedGuardTickSpendsOneSessionPerTick(t *testing.T) {
	f := newGuardFixture(t)
	f.start()
	// The pin was minted by the fixture's own Admit, which spent one.
	admissionProbes := f.probes

	t.Run("a held tick reads the cursors once and probes nothing", func(t *testing.T) {
		for pass := 0; pass < 3; pass++ {
			if step := f.tick(); step != proxiedGuardHeld {
				t.Fatalf("pass %d reported %s, want held", pass, step)
			}
		}
		if got := f.probes - admissionProbes; got != 0 {
			t.Errorf("three held ticks ran %d admission probe session(s), want 0: "+
				"an unchanged record is decided from two file reads", got)
		}
		if f.cursorReads != 3 {
			t.Errorf("three held ticks ran %d cursor session(s), want exactly 3 (one per tick)", f.cursorReads)
		}
	})

	t.Run("a re-pin costs exactly one more session, and not a second one", func(t *testing.T) {
		before := f.probes
		cursorsBefore := f.cursorReads
		f.admitted.corrupt(func(rec *proxyendpoint.Record) {
			rec.PID = 6007
			rec.Birth = proxyendpoint.BirthToken("boot-fixture", "abcdabcd")
		})

		if step := f.tick(); step != proxiedGuardRepinned {
			t.Fatalf("the generation change reported %s, want repinned", step)
		}
		if got := f.probes - before; got != 1 {
			t.Fatalf("the re-pin ran %d probe session(s), want exactly 1: the re-admission is one session", got)
		}
		if got := f.cursorReads - cursorsBefore; got != 0 {
			t.Fatalf("the re-pin ran %d cursor session(s) as well, want 0: the re-admission already "+
				"gated the new generation's cursors, so a second session buys the same answer", got)
		}
	})

	// Council pr2 D-F5, re-pin arm. The re-admission inside a generation change
	// was the long-lived ladder on the real clock: a new generation that
	// refused ran the 60s drain on the tick goroutine (this fixture's 5s step
	// timeout fires first), and a silent one walked three probes with 1s
	// sleeps. Either way the tick spent more than the one session its header
	// promises. Since council pr2 E-S1 the handle also stands down
	// NON-terminally on that one session, so each row gets its own fixture:
	// a stood-down store's next tick is a recovery, not a re-pin.
	for i, tc := range []struct {
		name    string
		outcome proxyendpoint.ProbeOutcome
	}{
		{name: "a re-pin against a new generation that refuses costs one session and waits out nothing", outcome: proxyendpoint.ProbeRefused},
		{name: "a re-pin against a new generation that never greets costs one session", outcome: proxyendpoint.ProbeAcceptedNoGreeting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGuardFixture(t)
			f.start()
			f.probeResult = func() proxyendpoint.ProbeResult { return proxyendpoint.ProbeResult{Outcome: tc.outcome} }
			before := f.probes
			f.admitted.corrupt(func(rec *proxyendpoint.Record) {
				rec.PID = 6010 + i
				rec.Birth = proxyendpoint.BirthToken("boot-fixture", fmt.Sprintf("feed%04d", i))
			})

			if step := f.tick(); step != proxiedGuardStoodDown {
				t.Fatalf("the re-pin reported %s, want stood-down: the replaced generation must not keep serving", step)
			}
			if verdict := f.store.Verdict(); verdict == nil || verdict.Terminal() {
				t.Fatalf("verdict = %v, want a non-terminal one: a proxy in motion is not a fact about the database", verdict)
			}
			if got := f.probes - before; got != 1 {
				t.Fatalf("the re-pin ran %d probe session(s), want exactly 1: the tick's budget is one "+
					"session, and the next tick is the retry", got)
			}
		})
	}
}

// TestProxiedGuardTickForgetsTheMemoOnCursorDrift is the other half of council
// A-F3.
//
// The pin memo's stamp fingerprints proxy.pid and the proxied sidecar, and a
// migration writes neither — so nothing a file can tell invalidates a memoized
// pass when somebody runs `bd migrate` inside the TTL. The guard tick's cursor
// re-read is the ONE thing in the process that sees it, and it used to keep
// that knowledge to itself: it stood this handle down and left every other open
// in the process reading a pass it had just contradicted.
func TestProxiedGuardTickForgetsTheMemoOnCursorDrift(t *testing.T) {
	f := newGuardFixture(t)
	f.start()

	pin := f.store.Pin()
	root, err := proxyendpoint.ProviderRoot(pin.ScopeRoot())
	if err != nil {
		t.Fatalf("ProviderRoot: %v", err)
	}
	beadsDir := filepath.Join(pin.ScopeRoot(), ".beads")
	now := time.Now()

	// The state a one-shot open in the same process would have left behind.
	// (The guard's own admission runs with SkipMemo, so the tick cannot have
	// written this itself — which is the point: it belongs to another open.)
	storeProxiedPin(pin.ScopeRoot(), pin.Database(), false, root, beadsDir, pin, now)
	if _, ok := lookupProxiedPin(pin.ScopeRoot(), pin.Database(), false, root, beadsDir, now); !ok {
		t.Fatal("the memo entry was not installed; this test would pass vacuously")
	}

	// Somebody migrated the shared database.
	f.cursors = proxyendpoint.Cursors{Main: SchemaCursorMain, Ignored: SchemaCursorIgnored + 1}
	if step := f.tick(); step != proxiedGuardStoodDown {
		t.Fatalf("cursor drift reported %s, want stood-down", step)
	}
	if _, ok := lookupProxiedPin(pin.ScopeRoot(), pin.Database(), false, root, beadsDir, now); ok {
		t.Fatal("the tick stood this handle down for schema drift and left the memoized pass in place, " +
			"so the next open in this process opens the library against the moved database without the gate")
	}
}

// TestStartGuardArmsARealTicker is council C-F8.
//
// `if longLived { store.StartGuard() }` in cmd/gc is the one line that arms the
// guard in production, and StartGuard itself had no test: every guard test
// called the unexported s.startGuard(opts) with an injected ticker channel and
// scripted effects. So all 486 lines of this file passed with StartGuard's body
// emptied, and a controller store would hold a native leaf with no generation
// or cursor watch for the whole process — including the moved-root hazard this
// file's header says no read can detect.
//
// This drives the EXPORTED entry point with the real time.Ticker, the real
// defaults and the knob an operator actually sets, and asserts an effect only a
// live tick can produce. The effect is chosen so the test needs no proxy and no
// socket: a record that fails Validate for this root is refused from the record
// alone — "no dial is ever spent on it" is the checkGeneration contract — so
// the whole path is two file reads and the ticker this test exists to prove
// exists.
func TestStartGuardArmsARealTicker(t *testing.T) {
	t.Setenv(proxiedGuardIntervalEnv, "1s")
	if got := proxiedGuardInterval(); got != time.Second {
		t.Fatalf("proxiedGuardInterval() = %s, want 1s: this test is not driving the knob", got)
	}

	f := newGuardFixture(t)

	// The production call, not the test seam.
	f.store.StartGuard()

	// Somebody put a foreign proxy record at the pinned root. Nothing else in
	// this test touches the store.
	f.admitted.corrupt(func(rec *proxyendpoint.Record) {
		rec.RootID = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	})

	deadline := time.After(20 * time.Second)
	for {
		if verdict := f.store.Verdict(); verdict != nil {
			if verdict.Verdict != ProxiedVerdictNotOurs {
				t.Fatalf("verdict = %v, want not_ours", verdict)
			}
			if !f.store.Demoted() {
				t.Fatal("the guard recorded a terminal verdict and kept serving natively")
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("StartGuard armed no ticker: 20s after a foreign record appeared at the pinned " +
				"root the store is still native, so a controller would hold an unwatched native leaf " +
				"for the process lifetime")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// TestStartGuardIsIdempotentPerStore pins the other half of its contract: a
// second call stops the first guard before installing its own, so a re-open
// path cannot accumulate tickers — each of which is a goroutine and a probe
// session per interval against bd's proxy.
//
// It asserts the join itself, deterministically, rather than a goroutine count
// after a wait. The count was process-wide, needed a baseline taken before the
// first StartGuard (council pr2 D-F14) and a polling sleep to let a joined
// goroutine finish exiting — a fixed sleep the resource census counts. What the
// contract actually promises is that each replaced guard's run has RETURNED by
// the time the next StartGuard returns (done is closed only by run's return,
// and stop waits for it), and that CloseStore does the same for the last one.
// Both are channel facts with no timing in them: a guard whose stop was skipped
// never closes done, so the non-blocking check below fails at once instead of
// after a deadline.
func TestStartGuardIsIdempotentPerStore(t *testing.T) {
	t.Setenv(proxiedGuardIntervalEnv, "1s")
	f := newGuardFixture(t)
	installed := func() *proxiedGuard {
		f.store.mu.RLock()
		defer f.store.mu.RUnlock()
		return f.store.guard
	}
	joined := func(g *proxiedGuard) bool {
		select {
		case <-g.done:
			return true
		default:
			return false
		}
	}

	// The production call, three times.
	var guards []*proxiedGuard
	for i := 0; i < 3; i++ {
		f.store.StartGuard()
		g := installed()
		if g == nil {
			t.Fatalf("StartGuard #%d installed no guard", i+1)
		}
		for _, earlier := range guards {
			if earlier == g {
				t.Fatalf("StartGuard #%d reinstalled an earlier guard instead of a fresh one", i+1)
			}
		}
		guards = append(guards, g)
	}
	for i, g := range guards[:2] {
		if !joined(g) {
			t.Fatalf("guard #%d was still running after StartGuard #%d returned: a later StartGuard "+
				"leaked an earlier guard's ticker, and every leaked guard is a probe session per "+
				"interval against bd's proxy", i+1, i+2)
		}
	}
	if joined(guards[2]) {
		t.Fatal("the installed guard exited before anything stopped it; this test would pass vacuously")
	}

	// CloseStore joins the installed guard.
	if err := f.store.CloseStore(); err != nil {
		t.Fatalf("CloseStore: %v", err)
	}
	if !joined(guards[2]) {
		t.Fatal("CloseStore returned with the installed guard still running")
	}
	if installed() != nil {
		t.Fatal("CloseStore left a guard installed")
	}
}

// closeSignalStorage is a library handle whose Close is observable.
type closeSignalStorage struct {
	*nativeDoltMemStorage
	closed chan struct{}
}

func (s *closeSignalStorage) Close() error {
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
	return nil
}

// TestCloseStoreDuringAGuardRecoveryClosesTheRecoveredLeaf is round3 review
// (safety).
//
// Stopping the guard only cancels its context. A recovery that has already
// finished its admission, library open and post-open read when the cancel
// lands goes on to repin — and repin checked only the terminal latch, which
// CloseStore never sets. So CloseStore captured native=nil, joined the guard,
// and returned; the guard then installed the fresh leaf behind it: a live
// library pool on bd's proxy that nothing closed for the rest of the process,
// and a CLOSED store serving reads natively. E-S1 made the state this starts
// from — non-terminally stood down, with a recovery each tick — the ordinary
// outcome of every re-pin that does not admit.
func TestCloseStoreDuringAGuardRecoveryClosesTheRecoveredLeaf(t *testing.T) {
	f := newGuardFixture(t)
	f.store.standDown(NewNonTerminalProxiedVerdictError(ProxiedVerdictProxyGone, "stood down by a refused re-pin", nil))
	if !f.store.Demoted() {
		t.Fatal("setup: the store did not stand down")
	}
	entered, release := make(chan struct{}), make(chan struct{})
	storage := &closeSignalStorage{
		nativeDoltMemStorage: &nativeDoltMemStorage{store: &MemStore{IDPrefix: "prx", HonorExplicitIDs: true}},
		closed:               make(chan struct{}),
	}
	f.recover = func(ctx context.Context) (*NativeDoltStore, Pin, error) {
		in := f.admissionInput(true)
		in.ProbeOnce = true
		pin, err := Admit(ctx, in)
		if err != nil {
			return nil, Pin{}, err
		}
		fresh := newNativeDoltStoreForTest(storage)
		fresh.idPrefix = "prx"
		// The admission, the library open and the post-open read are done:
		// nothing left in the recovery looks at ctx. CloseStore lands here.
		close(entered)
		<-release
		return fresh, pin, nil
	}
	f.start()
	f.ticks <- time.Now()
	<-entered

	closed := make(chan error, 1)
	go func() { closed <- f.store.CloseStore() }()
	// CloseStore has passed its locked section once it has taken the guard
	// out of the store; it is then joining the guard, which is parked above.
	for deadline := time.Now().Add(10 * time.Second); ; runtime.Gosched() {
		f.store.mu.RLock()
		taken := f.store.guard == nil
		f.store.mu.RUnlock()
		if taken {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("CloseStore never took the guard out of the store")
		}
	}
	close(release)
	if err := <-closed; err != nil {
		t.Fatalf("CloseStore: %v", err)
	}

	if step := <-f.steps; step != proxiedGuardStopped {
		t.Errorf("the recovery that finished after CloseStore reported %s, want stopped", step)
	}
	if leaked := f.store.nativeLeaf(); leaked != nil {
		t.Fatal("CloseStore returned and a recovered native leaf was installed behind it: the closed store serves reads " +
			"natively from a pool nothing will close")
	}
	select {
	case <-storage.closed:
	case <-time.After(10 * time.Second):
		t.Fatal("the leaf the recovery opened was never closed: a live library pool on bd's proxy for the rest of the process")
	}
}

// TestProxiedGuardTickStandsDownALeafTheReadPathAlreadyRepinned pins a cost of
// E-S1's stand-down, as it stands, so it cannot change silently in either
// direction (round3 review, safety).
//
// Row 1, a bd proxy restart, seen by the READ path first: a read fails on the
// dead socket and the reopen hook re-points the pool at the new generation.
// The hook never updates the wrapper's pin, so the next tick still sees the
// generation "move", re-admits on one probe session, and — on a loaded box,
// where that session is indeterminate — stands down a leaf that is serving the
// right generation. The one-session recovery then keeps it on the bd front
// door for as long as the load lasts. It fails safe (non-terminal, nothing
// read from a wrong generation) and it is an availability cost; checkGeneration
// says so. If the reopen hook ever adopts the pin it admitted, the first
// assertion fails: update that comment with this test.
func TestProxiedGuardTickStandsDownALeafTheReadPathAlreadyRepinned(t *testing.T) {
	f := newGuardFixture(t)
	f.installRecovery()
	f.start()
	before := f.store.Pin()

	// bd restarted its proxy, and a read met the dead socket first: the read
	// path's reconnect, through the reopen hook.
	f.admitted.writeRecord(6002, "99887766")
	_, gen, release, err := f.native.acquireStorageGen()
	if err != nil {
		t.Fatalf("acquire the leaf's storage: %v", err)
	}
	release()
	if err := f.native.reconnect(context.Background(), gen); err != nil {
		t.Fatalf("the read path's reconnect: %v", err)
	}
	<-f.reopens
	if f.store.Pin().Generation() != before.Generation() {
		t.Fatalf("the read path's reopen moved the wrapper's pin to %s: the cost this test documents is gone, "+
			"so update checkGeneration's comment (and this test) to say what the tick does now", f.store.Pin().Generation())
	}

	// A loaded box: the tick's one re-admission session learns nothing.
	f.probeResult = func() proxyendpoint.ProbeResult {
		return proxyendpoint.ProbeResult{Outcome: proxyendpoint.ProbeUnknown, Err: context.DeadlineExceeded}
	}
	if step := f.tick(); step != proxiedGuardStoodDown || !f.store.Demoted() {
		t.Fatalf("tick = %s, demoted = %v: want the documented stand-down of the already-repinned leaf", step, f.store.Demoted())
	}
	if verdict := f.store.Verdict(); verdict == nil || verdict.Terminal() {
		t.Fatalf("verdict = %v, want a NON-terminal stand-down: the leaf was never wrong", verdict)
	}
	for pass := 0; pass < 2; pass++ {
		if step := f.tick(); step != proxiedGuardUndecided || !f.store.Demoted() {
			t.Fatalf("loaded tick %d = %s, demoted = %v: want undecided and still on bd while the one-session recovery learns nothing",
				pass, step, f.store.Demoted())
		}
	}

	// The load lifts: the next recovery re-pins onto the generation the leaf
	// was serving all along.
	f.probeResult = nil
	if step := f.tick(); step != proxiedGuardRepinned || f.store.Demoted() {
		t.Fatalf("tick after the load = %s, demoted = %v: want the recovery to re-pin", step, f.store.Demoted())
	}
	if got := f.store.Pin().PoolKey().PID; got != 6002 {
		t.Fatalf("re-pinned to pid %d, want the restarted proxy (6002)", got)
	}
}
