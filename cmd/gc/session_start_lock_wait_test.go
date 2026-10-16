package main

import (
	"context"
	"maps"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// The wire strings the busy skips write. The tests pin them as literals so a
// rename is a deliberate, visible change.
const (
	testReasonSessionLockBusy     = TraceReasonCode("session_lock_busy")
	testReasonRollbackLockBusy    = TraceReasonCode("pending_create_rollback_lock_busy")
	testReasonRecoveryLockBusy    = TraceReasonCode("pending_create_recovery_lock_busy")
	testOutcomeSkippedSessionBusy = "outcome=skipped_session_busy"
	testLogResidualLockBusy       = "skipped: session mutation lock busy"
	testGateWaitBound             = 250 * time.Millisecond
	testPassHangBound             = 10 * time.Second
)

// holdSessionMutationLock takes session id's mutation lock on its own
// goroutine and returns once it is held. The returned func releases it and
// waits for the holder to finish; it is safe to call twice.
func holdSessionMutationLock(t *testing.T, id string) (release func()) {
	t.Helper()
	held := make(chan struct{})
	free := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = sessionpkg.WithSessionMutationLock(id, func() error {
			close(held)
			<-free
			return nil
		})
	}()
	<-held
	var released bool
	release = func() {
		if released {
			return
		}
		released = true
		close(free)
		<-done
	}
	t.Cleanup(release)
	return release
}

// boundedResult is what one pass did inside a synctest bubble, and how long
// it took on the bubble's fake clock.
type boundedResult[T any] struct {
	value    T
	finished bool
	elapsed  time.Duration
}

// runBounded runs fn on its own goroutine inside a synctest bubble. A call that
// has not returned after testPassHangBound of bubble time is blocked on
// something unbounded: it is reported as unfinished and release is invoked so
// the call can drain.
func runBounded[T any](release func(), fn func() T) boundedResult[T] {
	done := make(chan T, 1)
	began := time.Now()
	go func() { done <- fn() }()
	select {
	case v := <-done:
		return boundedResult[T]{value: v, finished: true, elapsed: time.Since(began)}
	case <-time.After(testPassHangBound):
		elapsed := time.Since(began)
		release()
		return boundedResult[T]{value: <-done, finished: false, elapsed: elapsed}
	}
}

// runStartPass runs one executePlannedStartsTraced pass under runBounded.
func runStartPass(e *capacityEnv, release func(), candidates ...startCandidate) boundedResult[int] {
	return runBounded(release, func() int { return e.start(context.Background(), candidates...) })
}

// busyStartEnv is a capacityEnv with the identity breaker on, one named
// session S, and two ordinary pending-create sessions behind it.
type busyStartEnv struct {
	*capacityEnv
	cb        *sessionCircuitBreaker
	s, h1, h2 startCandidate
}

func newBusyStartEnv(t *testing.T) *busyStartEnv {
	t.Helper()
	e := newCapacityEnv(t, true, "template-a", "h1", "h2")
	cb := e.namedIdentityBreaker(t)
	s := e.namedCandidate(t)
	e.desiredState[s.name()] = s.tp
	return &busyStartEnv{
		capacityEnv: e,
		cb:          cb,
		s:           s,
		h1:          e.pendingCreate(t, "h1", "e", nil),
		h2:          e.pendingCreate(t, "h2", "e", nil),
	}
}

func (e *capacityEnv) traceReasons(site TraceSiteCode, reason TraceReasonCode, outcome TraceOutcomeCode, name string) int {
	n := 0
	for _, r := range e.records(site) {
		if r.ReasonCode == reason && r.OutcomeCode == outcome && r.SessionName == name {
			n++
		}
	}
	return n
}

// Kills: a lock wait that is not exactly the tick bound (none at all, or too
// long), a start whose session mutation lock is held by another goroutine
// blocking the tick, or spending a limiter slot, a capacity ticket, a wake
// failure, a breaker restart or a store write before it gives up.
func TestExecutePlannedStarts_BusySessionLockSkipsAtTheGate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newBusyStartEnv(t)
		before := e.bead(t, e.s.info.ID)
		breakerBefore := e.cb.entrySnapshot("rig-a/session-a")
		release := holdSessionMutationLock(t, e.s.info.ID)

		res := runStartPass(e.capacityEnv, release, e.s, e.h1, e.h2)

		if !res.finished || res.elapsed != testGateWaitBound {
			t.Fatalf("finished=%v elapsed=%v, want the pass to give up after exactly %v", res.finished, res.elapsed, testGateWaitBound)
		}
		if res.value != 2 {
			t.Fatalf("woken = %d, want 2: the two sessions behind the busy one start in the same pass", res.value)
		}
		for _, name := range []string{"h1", "h2"} {
			if n := e.sp.CountCalls("Start", name); n != 1 {
				t.Fatalf("Start calls for %s = %d, want 1", name, n)
			}
		}
		if n := e.sp.CountCalls("Start", e.s.name()); n != 0 {
			t.Fatalf("Start calls for the busy session = %d, want 0", n)
		}
		after := e.bead(t, e.s.info.ID)
		if !maps.Equal(after.Metadata, before.Metadata) {
			t.Fatalf("busy session's row changed (last_woke_at, wake failure or persisted breaker metadata):\nbefore %v\nafter  %v", before.Metadata, after.Metadata)
		}
		if got := e.cb.entrySnapshot("rig-a/session-a"); !reflect.DeepEqual(got, breakerBefore) {
			t.Fatalf("in-memory breaker entry changed for a start that never ran: before %+v, after %+v", breakerBefore, got)
		}
		log := e.log.String()
		if !strings.Contains(log, testOutcomeSkippedSessionBusy) || !strings.Contains(log, "session="+e.s.name()) {
			t.Fatalf("stderr = %q, want a %s line for %s", log, testOutcomeSkippedSessionBusy, e.s.name())
		}
		if strings.Contains(log, "outcome=failed") || strings.Contains(log, "pre-wake") {
			t.Fatalf("stderr = %q, want no failed pre-wake for a busy skip", log)
		}
		if n := e.traceReasons(TraceSiteLifecycleStartRun, testReasonSessionLockBusy, TraceOutcomeSkipped, e.s.name()); n != 1 {
			t.Fatalf("skipped %q trace decisions for the busy session = %d, want 1", testReasonSessionLockBusy, n)
		}
		release()
	})
}

// Kills: a busy skip that is remembered, so the session never starts once the
// holder lets go.
func TestExecutePlannedStarts_BusySessionStartsOnTheNextPassAfterRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newBusyStartEnv(t)
		release := holdSessionMutationLock(t, e.s.info.ID)

		first := runStartPass(e.capacityEnv, release, e.s)
		if !first.finished {
			t.Fatalf("the first pass was still blocked on the held session lock after %v", first.elapsed)
		}
		if first.value != 0 || e.sp.CountCalls("Start", e.s.name()) != 0 {
			t.Fatalf("first pass woken = %d, Start calls = %d, want the busy session skipped", first.value, e.sp.CountCalls("Start", e.s.name()))
		}
		release()

		second := runStartPass(e.capacityEnv, func() {}, e.refreshed(e.s))
		if !second.finished {
			t.Fatalf("the retry pass did not finish")
		}
		if second.value != 1 || e.sp.CountCalls("Start", e.s.name()) != 1 {
			t.Fatalf("retry woken = %d, Start calls = %d, want the session started once the lock is free", second.value, e.sp.CountCalls("Start", e.s.name()))
		}
	})
}

// takeLockOnFirstGet is a store that, on the first read of one row after it is
// armed, has another goroutine take that session's mutation lock and returns
// only once it holds it. The lease record the start takes between the
// gate and the prepare reads the row, which lands the take in the window the
// gate cannot cover.
type takeLockOnFirstGet struct {
	beads.Store
	t       *testing.T
	id      string
	armed   bool
	release func()
}

func (s *takeLockOnFirstGet) Get(id string) (beads.Bead, error) {
	if s.armed && id == s.id && s.release == nil {
		s.release = holdSessionMutationLock(s.t, id)
	}
	return s.Store.Get(id)
}

// Kills: prepare's own acquire of the session lock waiting without bound, and
// a busy loss there being handled as a failed pre-wake (in-flight lease
// cleared, wake failure recorded, last_woke_at written).
func TestExecutePlannedStarts_LockTakenBetweenGateAndPrepareIsASkippedStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newBusyStartEnv(t)
		hook := &takeLockOnFirstGet{Store: e.store, t: t, id: e.s.info.ID}
		e.store = hook
		before := e.bead(t, e.s.info.ID)
		hook.armed = true

		res := runStartPass(e.capacityEnv, func() {
			if hook.release != nil {
				hook.release()
			}
		}, e.s, e.h1)

		if !res.finished || res.elapsed != testGateWaitBound {
			t.Fatalf("finished=%v elapsed=%v, want prepare's acquire to give up after exactly %v", res.finished, res.elapsed, testGateWaitBound)
		}
		if hook.release == nil {
			t.Fatal("the test hook never took the lock: the seam moved")
		}
		if res.value != 1 || e.sp.CountCalls("Start", "h1") != 1 || e.sp.CountCalls("Start", e.s.name()) != 0 {
			t.Fatalf("woken = %d, Start calls: h1=%d busy=%d; want only h1 started", res.value, e.sp.CountCalls("Start", "h1"), e.sp.CountCalls("Start", e.s.name()))
		}
		log := e.log.String()
		if !strings.Contains(log, testLogResidualLockBusy) {
			t.Fatalf("stderr = %q, want a %q line", log, testLogResidualLockBusy)
		}
		if strings.Contains(log, "outcome=failed") {
			t.Fatalf("stderr = %q, want no failed outcome for a busy skip", log)
		}
		after := e.bead(t, e.s.info.ID)
		if got, want := after.Metadata["last_woke_at"], before.Metadata["last_woke_at"]; got != want {
			t.Fatalf("last_woke_at = %q, want unchanged %q", got, want)
		}
		if n := e.traceReasons(TraceSiteLifecycleStartRun, testReasonSessionLockBusy, TraceOutcomeSkipped, e.s.name()); n != 1 {
			t.Fatalf("skipped %q trace decisions for the busy session = %d, want 1", testReasonSessionLockBusy, n)
		}
		// The ticket was handed back: the endpoint's admission is not left
		// holding the busy session's slot.
		ticket, ok := e.guard.Admit(capacityTestEndpoint, "outside", "outside")
		if !ok {
			t.Fatal("endpoint admission denied after a busy skip, want the ticket released")
		}
		ticket.Resolve(verdictSuccess)
	})
}

// Kills: a pending-create rollback whose session mutation lock is held waiting
// on the holder for the whole tick, or reporting the busy skip as a store
// failure ("session beads: ...") or a superseded snapshot, or running the
// retired-session cleanup for a bead that was never closed.
func TestReconcileSessionBeads_BusySessionLockDefersPendingCreateRollback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newCapacityEnv(t, false, "helper")
		c := e.session(t, "helper", "", map[string]string{
			"session_name":              "helper",
			"session_name_explicit":     "true",
			"pending_create_claim":      "true",
			"pending_create_started_at": pendingCreateStartedAtNow(e.clk.Now().Add(-(pendingCreateNeverStartedTimeout + time.Second))),
			"template":                  "helper",
			"state":                     "creating",
			"generation":                "1",
			"continuation_epoch":        "1",
			"instance_token":            "test-token",
		})
		before := e.bead(t, c.info.ID)
		release := holdSessionMutationLock(t, c.info.ID)

		res := runBounded(release, func() int { return e.reconcileTraced(before) })

		if !res.finished || res.elapsed != testGateWaitBound {
			t.Fatalf("finished=%v elapsed=%v, want the rollback to give up after exactly %v", res.finished, res.elapsed, testGateWaitBound)
		}
		after := e.bead(t, c.info.ID)
		if !maps.Equal(after.Metadata, before.Metadata) || after.Status != before.Status {
			t.Fatalf("a busy rollback changed the store:\nbefore %s %v\nafter  %s %v", before.Status, before.Metadata, after.Status, after.Metadata)
		}
		log := e.log.String()
		if !strings.Contains(log, "session mutation lock busy") {
			t.Fatalf("stderr = %q, want a rollback-deferred line naming the busy session lock", log)
		}
		if strings.Contains(log, "session beads:") || strings.Contains(log, "snapshot superseded") {
			t.Fatalf("stderr = %q, want the busy skip kept apart from the store-failure and superseded reports", log)
		}
		if n := e.traceReasons(TraceSiteReconcilerPendingCreate, testReasonRollbackLockBusy, TraceOutcomeSkipped, "helper"); n != 1 {
			t.Fatalf("skipped %q trace decisions = %d, want 1", testReasonRollbackLockBusy, n)
		}
		release()

		if res := runBounded(func() {}, func() int { return e.reconcileTraced(e.bead(t, c.info.ID)) }); !res.finished {
			t.Fatal("the retry tick did not finish")
		}
		if got := e.bead(t, c.info.ID); got.Status != "closed" {
			t.Fatalf("status after the lock was free = %q, want the rollback applied on the next tick", got.Status)
		}
	})
}

// Kills: recovery of an already-running pending create waiting on the held
// session lock for the whole tick, or folding a half-applied batch for a commit
// that never ran.
func TestRecoverRunningPendingCreate_BusySessionLockIsAnUnappliedRecovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		testCity := t.TempDir()
		store := beads.NewMemStore()
		bead, err := store.Create(beads.Bead{
			Title:  "helper",
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel},
			Metadata: map[string]string{
				"session_name":         "sky",
				"pending_create_claim": "true",
				"state":                "active",
				"state_reason":         "creation_complete",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		cfg := &config.City{Agents: []config.Agent{{Name: "helper"}}}
		tp := TemplateParams{SessionName: "sky", TemplateName: "helper"}
		clk := &clock.Fake{Time: time.Date(2026, 3, 18, 12, 0, 1, 0, time.UTC)}
		trace := newPoolDesiredStateTestTrace("helper")
		var log syncBuffer
		recoverOnce := func() (bool, map[string]string) {
			return recoverRunningPendingCreate(testCity, sessiontest.SeedBead(t, mustGetBead(t, store, bead.ID)), tp, cfg, store, nil, clk, &log, trace)
		}
		release := holdSessionMutationLock(t, bead.ID)

		type outcome struct {
			ok    bool
			batch map[string]string
		}
		res := runBounded(release, func() outcome {
			ok, batch := recoverOnce()
			return outcome{ok, batch}
		})

		if !res.finished || res.elapsed != testGateWaitBound {
			t.Fatalf("finished=%v elapsed=%v, want recovery to give up after exactly %v", res.finished, res.elapsed, testGateWaitBound)
		}
		if res.value.ok || res.value.batch != nil {
			t.Fatalf("recovery = (%v, %v), want (false, nil) when the commit could not be attempted", res.value.ok, res.value.batch)
		}
		got := mustGetBead(t, store, bead.ID)
		if got.Metadata["pending_create_claim"] != "true" || got.Metadata["creation_complete_at"] != "" {
			t.Fatalf("a busy recovery committed: pending_create_claim=%q creation_complete_at=%q", got.Metadata["pending_create_claim"], got.Metadata["creation_complete_at"])
		}
		var skipped bool
		for _, r := range trace.records {
			if r.SiteCode == TraceSiteReconcilerPendingCreate && r.ReasonCode == testReasonRecoveryLockBusy && r.OutcomeCode == TraceOutcomeSkipped && r.SessionName == "sky" {
				skipped = true
			}
		}
		if !skipped {
			t.Fatalf("no skipped %q trace decision for sky", testReasonRecoveryLockBusy)
		}
		if !strings.Contains(log.String(), "session mutation lock busy") {
			t.Fatalf("stderr = %q, want a recovery line naming the busy session lock", log.String())
		}
		release()

		if ok, _ := recoverOnce(); !ok {
			t.Fatal("the retry returned false, want the recovery applied once the lock is free")
		}
		if got := mustGetBead(t, store, bead.ID); got.Metadata["pending_create_claim"] != "" {
			t.Fatalf("pending_create_claim = %q after the retry, want cleared", got.Metadata["pending_create_claim"])
		}
	})
}

// Kills: a lock wait too short to ride out a holder that lets go inside the
// bound (a pure try-lock): the session loses its start for the pass, and the
// pass ends at the release instead of at the bound.
func TestExecutePlannedStarts_HolderReleasingWithinTheBoundStartsTheSessionInThePass(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newBusyStartEnv(t)
		release := holdSessionMutationLock(t, e.s.info.ID)
		time.AfterFunc(testGateWaitBound/2, release)

		res := runStartPass(e.capacityEnv, func() {}, e.s, e.h1, e.h2)

		if !res.finished || res.elapsed != testGateWaitBound/2 {
			t.Fatalf("finished=%v elapsed=%v, want the pass to proceed once the holder let go at %v", res.finished, res.elapsed, testGateWaitBound/2)
		}
		if res.value != 3 || e.sp.CountCalls("Start", e.s.name()) != 1 {
			t.Fatalf("woken = %d, Start calls for the once-busy session = %d; want all three started in this pass", res.value, e.sp.CountCalls("Start", e.s.name()))
		}
		if strings.Contains(e.log.String(), testOutcomeSkippedSessionBusy) {
			t.Fatalf("stderr = %q, want no busy skip when the lock came free inside the bound", e.log.String())
		}
	})
}

// Kills: a busy loss at prepare that leaves the capacity ticket held (the next
// session in the pass is denied the half-open probe the busy session was
// holding) or that clears the session's in-flight marker, as a failed pre-wake
// would. Needs a half-open endpoint, because a closed breaker admits whether or
// not the ticket was released, and a seeded last_woke_at, because an absent one
// compares equal before and after.
func TestExecutePlannedStarts_LockTakenBetweenGateAndPrepareReleasesTheProbeAndKeepsTheMarker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newBusyStartEnv(t)
		const seeded = "2026-10-13T00:00:00Z"
		if err := e.store.SetMetadata(e.s.info.ID, "last_woke_at", seeded); err != nil {
			t.Fatal(err)
		}
		hook := &takeLockOnFirstGet{Store: e.store, t: t, id: e.s.info.ID}
		e.store = hook
		e.openEndpoint(t)
		e.makeProbeDue()
		hook.armed = true

		res := runStartPass(e.capacityEnv, func() {
			if hook.release != nil {
				hook.release()
			}
		}, e.s, e.h1)

		if !res.finished || hook.release == nil {
			t.Fatalf("finished=%v hookFired=%v", res.finished, hook.release != nil)
		}
		if n := e.sp.CountCalls("Start", e.s.name()); n != 0 {
			t.Fatalf("Start calls for the busy session = %d, want 0", n)
		}
		if n := e.sp.CountCalls("Start", "h1"); n != 1 {
			t.Fatalf("Start calls for h1 = %d, want it to take the probe the busy skip released, in the same pass", n)
		}
		if got := e.bead(t, e.s.info.ID).Metadata["last_woke_at"]; got != seeded {
			t.Fatalf("last_woke_at = %q, want the seeded %q left alone by a busy skip", got, seeded)
		}
	})
}

// Kills: a busy pending-create rollback over a store with an atomic close (the
// production arm) that does not recognize the busy result, or that still
// cancels the session's pending waits for a bead it never closed. The plain
// MemStore the other rollback test uses answers ErrConditionalWriteUnsupported
// before the lock, so it never reaches this arm.
func TestReconcileSessionBeads_BusySessionLockOverAtomicCloseStoreLeavesRowAndWaitsUntouched(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newCapacityEnv(t, false, "helper")
		e.store = beads.NewAtomicCloseMemStore()
		c := e.session(t, "helper", "", map[string]string{
			"session_name":              "helper",
			"session_name_explicit":     "true",
			"pending_create_claim":      "true",
			"pending_create_started_at": pendingCreateStartedAtNow(e.clk.Now().Add(-(pendingCreateNeverStartedTimeout + time.Second))),
			"template":                  "helper",
			"state":                     "creating",
			"generation":                "1",
			"continuation_epoch":        "1",
			"instance_token":            "test-token",
		})
		wait, err := e.store.Create(beads.Bead{
			Title:    "wait",
			Type:     waitBeadType,
			Labels:   []string{waitBeadLabel, "session:" + c.info.ID},
			Metadata: map[string]string{"session_id": c.info.ID, "state": waitStatePending},
		})
		if err != nil {
			t.Fatal(err)
		}
		before := e.bead(t, c.info.ID)
		release := holdSessionMutationLock(t, c.info.ID)

		res := runBounded(release, func() int { return e.reconcileTraced(before) })

		if !res.finished || res.elapsed != testGateWaitBound {
			t.Fatalf("finished=%v elapsed=%v, want the rollback to give up after exactly %v", res.finished, res.elapsed, testGateWaitBound)
		}
		after := e.bead(t, c.info.ID)
		if !maps.Equal(after.Metadata, before.Metadata) || after.Status != before.Status {
			t.Fatalf("a busy rollback changed the store:\nbefore %s %v\nafter  %s %v", before.Status, before.Metadata, after.Status, after.Metadata)
		}
		if w := e.bead(t, wait.ID); w.Status == "closed" || w.Metadata["state"] != waitStatePending {
			t.Fatalf("wait after a busy rollback = %s/%s, want it left open and pending", w.Status, w.Metadata["state"])
		}
		log := e.log.String()
		if !strings.Contains(log, "session mutation lock busy") || strings.Contains(log, "session beads:") || strings.Contains(log, "snapshot superseded") {
			t.Fatalf("stderr = %q, want the busy line and no store-failure or superseded report", log)
		}
		skipped := 0
		for _, r := range e.records(TraceSiteReconcilerPendingCreate) {
			if r.ReasonCode == testReasonRollbackLockBusy && r.OutcomeCode == TraceOutcomeSkipped && r.SessionName == "helper" {
				skipped++
			}
		}
		if skipped != 1 {
			t.Fatalf("skipped %q trace decisions = %d, want 1", testReasonRollbackLockBusy, skipped)
		}
		release()

		if res := runBounded(func() {}, func() int { return e.reconcileTraced(e.bead(t, c.info.ID)) }); !res.finished {
			t.Fatal("the retry tick did not finish")
		}
		if got := e.bead(t, c.info.ID); got.Status != "closed" {
			t.Fatalf("status after the lock was free = %q, want the rollback applied", got.Status)
		}
		if w := e.bead(t, wait.ID); w.Status != "closed" || w.Metadata["state"] != waitStateCanceled {
			t.Fatalf("wait after the applied rollback = %s/%s, want closed/canceled (positive control)", w.Status, w.Metadata["state"])
		}
	})
}
