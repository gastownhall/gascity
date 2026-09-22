package beads

import (
	"context"
	"errors"
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

	store, err := NewProxiedStore(native, f.bd, pin)
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
		Probe:        servedProbe(pinnedCursors(), &f.probes),
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
		probe:        servedProbe(pinnedCursors(), &f.probes),
		cursors: func(context.Context, Pin) (proxyendpoint.Cursors, error) {
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
	if f.native.poolStale.Load() {
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
	if !f.native.poolStale.Load() {
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
	if f.native.poolStale.Load() {
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
	input := AdmissionInput{
		ScopeRoot:    f.scopeRoot,
		Database:     "beads",
		ProcessTable: f.processTable(),
		Probe:        servedProbe(pinnedCursors(), &probes),
		Observed:     NewGenerationSet(),
		Recovered:    NewGenerationSet(),
		Sleep:        func(context.Context, time.Duration) error { return nil },
		SkipMemo:     true,
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
