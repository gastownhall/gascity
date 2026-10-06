package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// Ownership veto tests: a pool worker that holds a live claim must never be
// stopped for a demand-class reason (history-orphan-claim.md, invariant I1).
//
// TestOwnershipVetoAcrossDeciders is the decider table the v1.6 shared
// never-stop-an-owner predicate extends: every demand-class decider that can
// stop a live session gets a row, every ownership variant a column, and the
// negative controls (closed claim, released claim, claim owned elsewhere,
// agent removed from config, agent suspended) prove the veto is not a blanket
// keep-alive. Today the rows are the deciders sessionOwnsLiveClaim is wired
// into; add a row whenever a decider starts consulting it.

const (
	ownershipPoolTemplate = "claude"
	ownershipWorkerName   = "claude-w1"
)

// ownershipFailingGetStore fails Get for one armed bead id, standing in for a
// transient backing-store read failure behind the cache.
type ownershipFailingGetStore struct {
	beads.Store
	mu     sync.Mutex
	failID string
}

func (s *ownershipFailingGetStore) arm(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failID = id
}

func (s *ownershipFailingGetStore) Get(id string) (beads.Bead, error) {
	s.mu.Lock()
	failID := s.failID
	s.mu.Unlock()
	if failID != "" && id == failID {
		return beads.Bead{}, errors.New("transient backing read failure")
	}
	return s.Store.Get(id)
}

// ownershipFixture is one live pool worker behind a primed CachingStore. The
// "out of process" writes the worker's `gc hook --claim` makes go straight to
// the backing store, so the controller's cache (and the session snapshot handed
// to the reconciler) stays stale — the Tier C first-run shape.
type ownershipFixture struct {
	env     *reconcilerTestEnv
	backing *beads.MemStore
	reads   *ownershipFailingGetStore
	session beads.Bead // pre-claim snapshot, as the reconciler tick sees it
	work    beads.Bead
	dops    *fakeDrainOps
}

func newOwnershipFixture(t *testing.T) *ownershipFixture {
	t.Helper()
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: ownershipPoolTemplate, MaxActiveSessions: intPtr(4)}}}
	backing := beads.NewMemStore()
	env.store = backing
	session := env.createSessionBead(ownershipWorkerName, ownershipPoolTemplate)
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{poolManagedMetadataKey: "true"})
	if err := env.sp.Start(context.Background(), ownershipWorkerName, runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("Start(%s): %v", ownershipWorkerName, err)
	}
	if err := env.sp.SetMeta(ownershipWorkerName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}
	work, err := backing.Create(beads.Bead{Title: "do-work", Type: "task"})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	reads := &ownershipFailingGetStore{Store: backing}
	cache := beads.NewCachingStoreForTest(reads, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	env.store = cache
	return &ownershipFixture{env: env, backing: backing, reads: reads, session: session, work: work, dops: newFakeDrainOps()}
}

// claimOutOfProcess is the worker's `gc hook --claim`: the step moves to
// in_progress under the pool route name (an assignee spelling no session
// identity guard matches) and the session bead records current_claim_bead_id.
// Neither write goes through the controller's cache.
func (f *ownershipFixture) claimOutOfProcess(t *testing.T) {
	t.Helper()
	inProgress := "in_progress"
	assignee := ownershipPoolTemplate
	if err := f.backing.Update(f.work.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &assignee}); err != nil {
		t.Fatalf("claim work: %v", err)
	}
	if err := f.backing.SetMetadata(f.work.ID, beadmeta.SessionIDMetadataKey, f.session.ID); err != nil {
		t.Fatalf("stamp gc.session_id: %v", err)
	}
	if err := f.backing.SetMetadata(f.session.ID, beadmeta.CurrentClaimBeadIDMetadataKey, f.work.ID); err != nil {
		t.Fatalf("stamp current_claim_bead_id: %v", err)
	}
}

func (f *ownershipFixture) assertCacheStale(t *testing.T) {
	t.Helper()
	cached, err := f.env.store.Get(f.session.ID)
	if err != nil {
		t.Fatalf("cache Get(session): %v", err)
	}
	if cached.Metadata[beadmeta.CurrentClaimBeadIDMetadataKey] != "" {
		t.Fatal("test premise broken: the cache already sees the out-of-process claim")
	}
}

// beginInFlightOrphanedDrain simulates an orphaned drain begun on an earlier
// tick, before the claim was visible. acked=true also publishes the reconciler
// drain ack (the state the next tick would stop the worker from).
func (f *ownershipFixture) beginInFlightOrphanedDrain(t *testing.T, reason string, acked bool) {
	t.Helper()
	ds := &drainState{
		startedAt:  f.env.clk.Now(),
		deadline:   f.env.clk.Now().Add(time.Hour),
		reason:     reason,
		generation: 1,
	}
	if acked {
		if err := setReconcilerDrainAckMetadata(f.env.sp, ownershipWorkerName, ds); err != nil {
			t.Fatalf("setReconcilerDrainAckMetadata: %v", err)
		}
		if err := f.dops.setDrainAck(ownershipWorkerName); err != nil {
			t.Fatalf("setDrainAck: %v", err)
		}
		ds.ackSet = true
	}
	f.env.dt.set(f.session.ID, ds)
}

// stopObservation reports whether the tick stopped or is stopping the worker,
// and why.
func (f *ownershipFixture) stopObservation(t *testing.T) (bool, string) {
	t.Helper()
	if ds := f.env.dt.get(f.session.ID); ds != nil {
		return true, "drain:" + ds.reason
	}
	b, err := f.backing.Get(f.session.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if b.Metadata["state_reason"] == sessionpkg.DrainAckStopPendingReason {
		return true, "stop-pending"
	}
	if b.Status == "closed" {
		return true, "closed"
	}
	if !f.env.sp.IsRunning(ownershipWorkerName) {
		return true, "stopped"
	}
	if ack, _ := f.env.sp.GetMeta(ownershipWorkerName, "GC_DRAIN_ACK"); ack != "" {
		return true, "drain-ack-pending"
	}
	return false, ""
}

// Ownership variants. Each mutates the fixture after the cache was primed.
type ownershipVariant struct {
	name  string
	apply func(t *testing.T, f *ownershipFixture)
	// kept reports whether the worker must survive every demand-class decider.
	kept bool
}

func ownershipVariants() []ownershipVariant {
	return []ownershipVariant{
		{name: "no claim", kept: false, apply: func(*testing.T, *ownershipFixture) {}},
		{name: "live claim, stale cache", kept: true, apply: func(t *testing.T, f *ownershipFixture) {
			f.claimOutOfProcess(t)
			f.assertCacheStale(t)
		}},
		{name: "claim read error", kept: true, apply: func(t *testing.T, f *ownershipFixture) {
			f.claimOutOfProcess(t)
			f.reads.arm(f.work.ID)
		}},
		{name: "claimed bead closed", kept: false, apply: func(t *testing.T, f *ownershipFixture) {
			f.claimOutOfProcess(t)
			if err := f.backing.Close(f.work.ID); err != nil {
				t.Fatalf("Close(work): %v", err)
			}
		}},
		{name: "claim released to queue", kept: false, apply: func(t *testing.T, f *ownershipFixture) {
			f.claimOutOfProcess(t)
			open, none := "open", ""
			if err := f.backing.Update(f.work.ID, beads.UpdateOpts{Status: &open, Assignee: &none}); err != nil {
				t.Fatalf("release work: %v", err)
			}
		}},
		{name: "claim now owned by another session", kept: false, apply: func(t *testing.T, f *ownershipFixture) {
			f.claimOutOfProcess(t)
			if err := f.backing.SetMetadata(f.work.ID, beadmeta.SessionIDMetadataKey, "gc-other-session"); err != nil {
				t.Fatalf("restamp gc.session_id: %v", err)
			}
		}},
		{name: "agent removed from config", kept: false, apply: func(t *testing.T, f *ownershipFixture) {
			f.claimOutOfProcess(t)
			f.env.cfg = &config.City{Agents: []config.Agent{{Name: "other"}}}
		}},
		{name: "agent suspended", kept: false, apply: func(t *testing.T, f *ownershipFixture) {
			f.claimOutOfProcess(t)
			f.env.cfg.Agents[0].Suspended = true
		}},
	}
}

// Demand-class deciders that consult sessionOwnsLiveClaim.
type ownershipDecider struct {
	name  string
	drive func(t *testing.T, f *ownershipFixture)
	// negativeStop is the stop reason expected when the worker is not kept.
	negativeStop string
}

func ownershipDeciders() []ownershipDecider {
	notDesired := func(f *ownershipFixture) {
		f.env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{f.session}, map[string]int{}, f.dops)
	}
	return []ownershipDecider{
		{
			name:         "orphaned drain begin (not-desired live arm)",
			negativeStop: "drain:orphaned",
			drive:        func(_ *testing.T, f *ownershipFixture) { notDesired(f) },
		},
		{
			name:         "orphaned drain in flight (tracked, not yet acked)",
			negativeStop: "drain:orphaned",
			drive: func(t *testing.T, f *ownershipFixture) {
				f.beginInFlightOrphanedDrain(t, "orphaned", false)
				notDesired(f)
			},
		},
		{
			name:         "orphaned drain in flight (reconciler ack published)",
			negativeStop: "stop-pending",
			drive: func(t *testing.T, f *ownershipFixture) {
				f.beginInFlightOrphanedDrain(t, "orphaned", true)
				notDesired(f)
			},
		},
		{
			name:         "no-wake-reason drain begin (desired, no wake reason)",
			negativeStop: "drain:no-wake-reason",
			drive: func(_ *testing.T, f *ownershipFixture) {
				f.env.addDesired(ownershipWorkerName, ownershipPoolTemplate, false)
				f.env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{f.session}, map[string]int{}, f.dops)
			},
		},
		{
			name:         "idle drain begin (idle probe authorized idle-stop-pending)",
			negativeStop: "drain:idle",
			drive: func(_ *testing.T, f *ownershipFixture) {
				f.env.setSessionMetadata(&f.session, map[string]string{"sleep_intent": "idle-stop-pending"})
				f.env.addDesired(ownershipWorkerName, ownershipPoolTemplate, false)
				f.env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{f.session}, map[string]int{}, f.dops)
			},
		},
	}
}

func TestOwnershipVetoAcrossDeciders(t *testing.T) {
	for _, decider := range ownershipDeciders() {
		for _, variant := range ownershipVariants() {
			t.Run(decider.name+"/"+variant.name, func(t *testing.T) {
				f := newOwnershipFixture(t)
				variant.apply(t, f)
				decider.drive(t, f)
				stopped, why := f.stopObservation(t)
				if variant.kept && stopped {
					t.Fatalf("worker holding a live claim was stopped (%s); stdout:\n%s\nstderr:\n%s", why, f.env.stdout.String(), f.env.stderr.String())
				}
				if !variant.kept && why != decider.negativeStop {
					t.Fatalf("stop observation = %q (stopped=%v), want %q: the veto must not keep this worker; stdout:\n%s\nstderr:\n%s", why, stopped, decider.negativeStop, f.env.stdout.String(), f.env.stderr.String())
				}
			})
		}
	}
}

// TestOwnershipVeto_LogsOnce: the vetoed decider re-runs every tick; the skip
// is logged once per (session, claim).
func TestOwnershipVeto_LogsOnce(t *testing.T) {
	f := newOwnershipFixture(t)
	f.claimOutOfProcess(t)
	for range 3 {
		f.env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{f.session}, map[string]int{}, f.dops)
	}
	if stopped, why := f.stopObservation(t); stopped {
		t.Fatalf("worker stopped (%s)", why)
	}
	if n := strings.Count(f.env.stdout.String(), "session holds live claim"); n != 1 {
		t.Fatalf("live-claim veto logged %d times over 3 ticks, want 1; stdout:\n%s", n, f.env.stdout.String())
	}
}

// TestOwnershipVeto_InFlightOrphanedDrainCanceledWhenClaimAppears drives the
// exact sequence: tick 1 begins an orphaned drain (claim not yet made), the
// worker claims out of process, tick 2 must cancel the drain instead of
// publishing the ack that stops the worker.
func TestOwnershipVeto_InFlightOrphanedDrainCanceledWhenClaimAppears(t *testing.T) {
	f := newOwnershipFixture(t)
	f.env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{f.session}, map[string]int{}, f.dops)
	if ds := f.env.dt.get(f.session.ID); ds == nil || ds.reason != "orphaned" {
		t.Fatalf("tick 1: drain = %+v, want an orphaned drain (test premise)", ds)
	}
	f.claimOutOfProcess(t)
	f.assertCacheStale(t)
	f.env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{f.session}, map[string]int{}, f.dops)
	if stopped, why := f.stopObservation(t); stopped {
		t.Fatalf("tick 2: worker still being stopped (%s) after its claim became live; stdout:\n%s", why, f.env.stdout.String())
	}
}

// TestOwnershipVeto_DeliberateInFlightDrainsNotCanceled: the live-claim lens
// cancels only "orphaned". Intent-class drains already in flight keep going
// even when the session holds a live claim.
func TestOwnershipVeto_DeliberateInFlightDrainsNotCanceled(t *testing.T) {
	for _, reason := range []string{"config-drift", "suspended", executionStalledDrainReason, "user-hold"} {
		t.Run(reason, func(t *testing.T) {
			f := newOwnershipFixture(t)
			f.claimOutOfProcess(t)
			f.beginInFlightOrphanedDrain(t, reason, false)
			f.env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{f.session}, map[string]int{}, f.dops)
			if ds := f.env.dt.get(f.session.ID); ds == nil || ds.reason != reason {
				t.Fatalf("in-flight %q drain = %+v, want it untouched by the live-claim veto", reason, ds)
			}
		})
	}
}

func TestLiveClaimDrainReasonCancelable(t *testing.T) {
	for reason, want := range map[string]bool{
		"orphaned":                  true,
		"suspended":                 false,
		"config-drift":              false,
		"no-wake-reason":            false,
		"idle":                      false,
		"user-hold":                 false,
		executionStalledDrainReason: false,
		idleRespawnDrainReason:      false,
	} {
		if got := liveClaimDrainReasonCancelable(reason); got != want {
			t.Errorf("liveClaimDrainReasonCancelable(%q) = %v, want %v", reason, got, want)
		}
	}
}

// TestOwnershipVeto_UserHoldDrainUnaffected: a user suspend (sleep_intent
// user-hold) of a pool worker holding a live claim still drains.
func TestOwnershipVeto_UserHoldDrainUnaffected(t *testing.T) {
	f := newOwnershipFixture(t)
	f.claimOutOfProcess(t)
	heldUntil := f.env.clk.Now().Add(100 * time.Hour).UTC().Format(time.RFC3339)
	f.env.setSessionMetadata(&f.session, map[string]string{
		"held_until":   heldUntil,
		"sleep_intent": "user-hold",
		"state":        "suspended",
	})
	f.env.addDesired(ownershipWorkerName, ownershipPoolTemplate, false)
	f.env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{f.session}, map[string]int{}, f.dops)
	if ds := f.env.dt.get(f.session.ID); ds == nil || ds.reason != "user-hold" {
		t.Fatalf("drain = %+v, want the user-hold drain to proceed despite the live claim", ds)
	}
}

// TestOwnershipVeto_MaxSessionAgeUnaffected: max-session-age is intent-class
// and still stops a pool worker holding a live claim.
func TestOwnershipVeto_MaxSessionAgeUnaffected(t *testing.T) {
	f := newOwnershipFixture(t)
	f.env.cfg.Agents[0].MaxSessionAge = "5h"
	f.claimOutOfProcess(t)
	f.env.setSessionMetadata(&f.session, map[string]string{
		"creation_complete_at": f.env.clk.Now().Add(-6 * time.Hour).UTC().Format(time.RFC3339),
	})
	f.env.addDesired(ownershipWorkerName, ownershipPoolTemplate, false)
	tr := newMaxSessionAgeTracker()
	tr.setConfig(ownershipWorkerName, 5*time.Hour, 0)
	f.env.maxAgeReconcile([]beads.Bead{f.session}, tr)
	if f.env.sp.IsRunning(ownershipWorkerName) {
		t.Fatalf("max-session-age must still stop a pool worker holding a live claim; stdout:\n%s\nstderr:\n%s", f.env.stdout.String(), f.env.stderr.String())
	}
}

func TestSessionOwnsLiveClaim(t *testing.T) {
	t.Run("nothing stamped", func(t *testing.T) {
		f := newOwnershipFixture(t)
		owns, claimID, err := sessionOwnsLiveClaim("", f.env.cfg, f.env.store, nil, f.env.sessionInfo(f.session.ID))
		if owns || claimID != "" || err != nil {
			t.Fatalf("sessionOwnsLiveClaim = (%v, %q, %v), want (false, \"\", nil)", owns, claimID, err)
		}
	})
	t.Run("stamped claim invisible to cache is read live", func(t *testing.T) {
		f := newOwnershipFixture(t)
		f.claimOutOfProcess(t)
		f.assertCacheStale(t)
		owns, claimID, err := sessionOwnsLiveClaim("", f.env.cfg, f.env.store, nil, f.env.sessionInfo(f.session.ID))
		if !owns || claimID != f.work.ID || err != nil {
			t.Fatalf("sessionOwnsLiveClaim = (%v, %q, %v), want (true, %q, nil)", owns, claimID, err, f.work.ID)
		}
	})
	t.Run("session bead read error fails safe", func(t *testing.T) {
		f := newOwnershipFixture(t)
		f.reads.arm(f.session.ID)
		owns, _, err := sessionOwnsLiveClaim("", f.env.cfg, f.env.store, nil, f.env.sessionInfo(f.session.ID))
		if !owns || err == nil {
			t.Fatalf("sessionOwnsLiveClaim = (%v, %v), want claim held with the read error", owns, err)
		}
	})
	t.Run("claimed bead missing", func(t *testing.T) {
		f := newOwnershipFixture(t)
		if err := f.backing.SetMetadata(f.session.ID, beadmeta.CurrentClaimBeadIDMetadataKey, "gc-missing"); err != nil {
			t.Fatal(err)
		}
		owns, claimID, err := sessionOwnsLiveClaim("", f.env.cfg, f.env.store, nil, f.env.sessionInfo(f.session.ID))
		if owns || claimID != "gc-missing" || err != nil {
			t.Fatalf("sessionOwnsLiveClaim = (%v, %q, %v), want (false, gc-missing, nil)", owns, claimID, err)
		}
	})
}

// ownershipCountingStore counts Get calls, to pin how many store reads one
// live-claim check costs.
type ownershipCountingStore struct {
	beads.Store
	mu   sync.Mutex
	gets int
}

func (s *ownershipCountingStore) Get(id string) (beads.Bead, error) {
	s.mu.Lock()
	s.gets++
	s.mu.Unlock()
	return s.Store.Get(id)
}

func (s *ownershipCountingStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets
}

// TestSessionOwnsLiveClaim_StopsAtFoundBead: the first residency leg that
// holds the claimed bead answers for it. A finished worker whose stamp still
// names a closed (or released, or re-owned) bead must not fan the read out to
// every other leg on every tick — that was 1+2R bd subprocesses per check on a
// city with R rigs. Only a NotFound moves on to the next leg.
func TestSessionOwnsLiveClaim_StopsAtFoundBead(t *testing.T) {
	city := &ownershipCountingStore{Store: beads.NewMemStore()}
	rigA := &ownershipCountingStore{Store: beads.NewMemStore()}
	rigB := &ownershipCountingStore{Store: beads.NewMemStore()}
	rigStores := map[string]beads.Store{"alpha": rigA, "beta": rigB}
	cfg := &config.City{
		Agents: []config.Agent{{Name: ownershipPoolTemplate, Scope: "city", MaxActiveSessions: intPtr(4)}},
		Rigs:   []config.Rig{{Name: "alpha", Path: t.TempDir()}, {Name: "beta", Path: t.TempDir()}},
	}

	session, err := city.Create(beads.Bead{
		Title:  ownershipWorkerName,
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":         ownershipWorkerName,
			"template":             ownershipPoolTemplate,
			"state":                "active",
			poolManagedMetadataKey: "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	info, err := sessionFrontDoor(city).Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name     string
		mutate   func(t *testing.T, work beads.Bead)
		wantHeld bool
	}{
		{name: "closed", mutate: func(t *testing.T, work beads.Bead) {
			if err := city.Close(work.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "released", mutate: func(t *testing.T, work beads.Bead) {
			open, none := "open", ""
			if err := city.Update(work.ID, beads.UpdateOpts{Status: &open, Assignee: &none}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "owned by another session", mutate: func(t *testing.T, work beads.Bead) {
			if err := city.SetMetadata(work.ID, beadmeta.SessionIDMetadataKey, "gc-other"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "held", wantHeld: true, mutate: func(*testing.T, beads.Bead) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inProgress, assignee := "in_progress", ownershipPoolTemplate
			work, err := city.Create(beads.Bead{Title: "do-work", Type: "task"})
			if err != nil {
				t.Fatal(err)
			}
			if err := city.Update(work.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &assignee}); err != nil {
				t.Fatal(err)
			}
			if err := city.SetMetadata(work.ID, beadmeta.SessionIDMetadataKey, session.ID); err != nil {
				t.Fatal(err)
			}
			if err := city.SetMetadata(session.ID, beadmeta.CurrentClaimBeadIDMetadataKey, work.ID); err != nil {
				t.Fatal(err)
			}
			tc.mutate(t, work)

			cityBefore, aBefore, bBefore := city.count(), rigA.count(), rigB.count()
			held, claimID, err := sessionOwnsLiveClaim("", cfg, city, rigStores, info)
			if err != nil || claimID != work.ID || held != tc.wantHeld {
				t.Fatalf("sessionOwnsLiveClaim = (%v, %q, %v), want (%v, %q, nil)", held, claimID, err, tc.wantHeld, work.ID)
			}
			if got := city.count() - cityBefore; got != 2 {
				t.Errorf("city store Gets = %d, want 2 (session bead + claimed bead)", got)
			}
			if got := rigA.count() - aBefore + rigB.count() - bBefore; got != 0 {
				t.Errorf("rig store Gets = %d, want 0: the claimed bead was already found in the city leg", got)
			}
		})
	}

	t.Run("not found in city leg moves on to the rigs", func(t *testing.T) {
		if err := city.SetMetadata(session.ID, beadmeta.CurrentClaimBeadIDMetadataKey, "gc-elsewhere"); err != nil {
			t.Fatal(err)
		}
		aBefore, bBefore := rigA.count(), rigB.count()
		held, _, err := sessionOwnsLiveClaim("", cfg, city, rigStores, info)
		if held || err != nil {
			t.Fatalf("sessionOwnsLiveClaim = (%v, %v), want not held", held, err)
		}
		if rigA.count() == aBefore || rigB.count() == bBefore {
			t.Fatalf("rig legs not searched for a claim missing from the city leg (alpha +%d, beta +%d)", rigA.count()-aBefore, rigB.count()-bBefore)
		}
	})
}
