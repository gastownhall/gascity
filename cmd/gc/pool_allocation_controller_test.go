package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func TestAuthoritativeReadyRoutedWorkByIDReadsBackingStateWithoutFleetScan(t *testing.T) {
	now := time.Now().UTC()

	t.Run("event cache says ready but backing work is closed", func(t *testing.T) {
		backing := &readyRoutedWorkReadAuditStore{Store: beads.NewMemStore()}
		work, err := backing.Create(beads.Bead{
			Title:    "stale cached work",
			Type:     "task",
			Status:   "open",
			Metadata: map[string]string{"gc.routed_to": "worker"},
		})
		if err != nil {
			t.Fatalf("create work: %v", err)
		}
		cache := beads.NewCachingStoreForTest(backing, nil)
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("prime cache: %v", err)
		}
		if _, ready := cache.CachedReadyByID(work.ID, now); !ready {
			t.Fatal("precondition: cache did not retain ready work")
		}
		if err := backing.Close(work.ID); err != nil {
			t.Fatalf("close backing work without cache event: %v", err)
		}
		backing.getCalls.Store(0)
		backing.listCalls.Store(0)
		backing.readyCalls.Store(0)
		backing.depListCalls.Store(0)

		got, ready, err := authoritativeReadyRoutedWorkByID(cache, work.ID, now)
		if err != nil {
			t.Fatalf("authoritative ready read: %v", err)
		}
		if ready || got.ID != "" {
			t.Fatalf("authoritative ready read = (%+v, %t), want not ready", got, ready)
		}
		if got := backing.getCalls.Load(); got != 1 {
			t.Fatalf("backing Get calls = %d, want 1", got)
		}
		if got := backing.listCalls.Load(); got != 0 {
			t.Fatalf("backing List calls = %d, want 0", got)
		}
		if got := backing.readyCalls.Load(); got != 0 {
			t.Fatalf("backing Ready calls = %d, want 0", got)
		}
	})

	t.Run("open blocking dependency fails closed with exact reads", func(t *testing.T) {
		backing := &readyRoutedWorkReadAuditStore{Store: beads.NewMemStore()}
		work, err := backing.Create(beads.Bead{Title: "blocked work", Type: "task", Status: "open"})
		if err != nil {
			t.Fatalf("create work: %v", err)
		}
		blocker, err := backing.Create(beads.Bead{Title: "blocker", Type: "task", Status: "open"})
		if err != nil {
			t.Fatalf("create blocker: %v", err)
		}
		if err := backing.DepAdd(work.ID, blocker.ID, "blocks"); err != nil {
			t.Fatalf("add dependency: %v", err)
		}
		backing.getCalls.Store(0)

		got, ready, err := authoritativeReadyRoutedWorkByID(backing, work.ID, now)
		if err != nil {
			t.Fatalf("authoritative ready read: %v", err)
		}
		if ready || got.ID != "" {
			t.Fatalf("authoritative ready read = (%+v, %t), want blocked", got, ready)
		}
		if got := backing.getCalls.Load(); got != 2 {
			t.Fatalf("backing Get calls = %d, want work plus blocker", got)
		}
		if got := backing.depListCalls.Load(); got != 1 {
			t.Fatalf("backing DepList calls = %d, want 1", got)
		}
		if got := backing.listCalls.Load(); got != 0 {
			t.Fatalf("backing List calls = %d, want 0", got)
		}
		if got := backing.readyCalls.Load(); got != 0 {
			t.Fatalf("backing Ready calls = %d, want 0", got)
		}
	})

	t.Run("dependency read uncertainty is an error", func(t *testing.T) {
		base := beads.NewMemStore()
		work, err := base.Create(beads.Bead{Title: "uncertain work", Type: "task", Status: "open"})
		if err != nil {
			t.Fatalf("create work: %v", err)
		}
		readErr := errors.New("dependency store unavailable")
		store := &poolAllocationDepListErrorStore{Store: base, err: readErr}

		_, ready, err := authoritativeReadyRoutedWorkByID(store, work.ID, now)
		if ready || !errors.Is(err, readErr) {
			t.Fatalf("authoritative ready read = (ready=%t, err=%v), want dependency error", ready, err)
		}
	})
}

func TestRoutedWorkPoolAllocationMaterializesOneDurableSessionAndUsesExactStart(t *testing.T) {
	fixture := newRoutedWorkPoolAllocationFixture(t, beads.NewMemStore())
	priority := 3
	work, err := fixture.store.Create(beads.Bead{
		Title:    "ready routed work",
		Type:     "task",
		Status:   "open",
		Priority: &priority,
		Metadata: map[string]string{
			"gc.routed_to":                    "worker",
			beadmeta.PackMetadataKey:          "review-pack",
			beadmeta.PackWorkspaceMetadataKey: "workspace-a",
		},
	})
	if err != nil {
		t.Fatalf("create ready work: %v", err)
	}
	hint := routedWorkPoolAllocationHint{
		WorkID:      work.ID,
		PoolTarget:  "worker",
		SourceStore: "city:test-city",
		EventAt:     time.Now().UTC().Add(-time.Second),
		EnqueuedAt:  time.Now().UTC(),
	}

	first, err := fixture.cr.reconcileRoutedWorkPoolAllocation(t.Context(), hint)
	if err != nil {
		t.Fatalf("reconcile routed-work allocation: %v", err)
	}
	if !first.Handled || !first.Created || first.Session.ID == "" {
		t.Fatalf("first allocation = %+v, want one created session", first)
	}

	second, err := fixture.cr.reconcileRoutedWorkPoolAllocation(t.Context(), hint)
	if err != nil {
		t.Fatalf("reconcile duplicate routed-work allocation: %v", err)
	}
	if !second.Handled || second.Created || second.Session.ID != first.Session.ID {
		t.Fatalf("duplicate allocation = %+v, want existing session %s", second, first.Session.ID)
	}

	stored, err := fixture.store.Get(first.Session.ID)
	if err != nil {
		t.Fatalf("read created session: %v", err)
	}
	if stored.Metadata[beadmeta.TriggerBeadIDMetadataKey] != work.ID ||
		stored.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey] != hint.SourceStore ||
		stored.Metadata[beadmeta.PackMetadataKey] != "review-pack" ||
		stored.Metadata[beadmeta.PackWorkspaceMetadataKey] != "workspace-a" {
		t.Fatalf("created session trigger metadata = %+v, want authoritative work binding", stored.Metadata)
	}

	infos, err := loadSessionBeadSnapshot(fixture.store)
	if err != nil {
		t.Fatalf("load created sessions: %v", err)
	}
	if got := len(infos.OpenInfos()); got != 1 {
		t.Fatalf("open sessions = %d, want exactly 1", got)
	}
	awaitCond(t, func() bool {
		return fixture.provider.IsRunning(first.Session.SessionName) || fixture.cr.sessionStartController.Pending() == 0
	}, "exact-start controller to finish directly materialized pool session")
	if !fixture.provider.IsRunning(first.Session.SessionName) {
		current := sessionpkg.Info{}
		if currentInfo, _, currentErr := sessionFrontDoor(fixture.store).GetPersistedResponse(first.Session.ID); currentErr == nil {
			current = currentInfo
		}
		snapshot, release, snapshotErr := fixture.cr.cs.acquireSessionStartSnapshot()
		var lease routedWorkPoolStartLease
		var leaseErr error
		var authorized bool
		var authorizeErr error
		if snapshotErr == nil {
			defer release()
			lease, leaseErr = fixture.cr.newRoutedWorkPoolStartLease(snapshot, first.Session, hint)
			if leaseErr == nil {
				authorized, authorizeErr = fixture.cr.authorizeRoutedWorkPoolStart(t.Context(), snapshot, first.Session, lease)
			}
		}
		t.Fatalf("directly materialized pool session %q is not running; current=%+v snapshot=%v lease=%+v lease_err=%v authorized=%t authorize_err=%v membership=%+v controller stderr:\n%s\nruntime calls: %+v", first.Session.SessionName, current, snapshotErr, lease, leaseErr, authorized, authorizeErr, fixture.cr.poolMembershipShadow.observe("worker"), fixture.stderr.String(), fixture.provider.SnapshotCalls())
	}
}

func TestRoutedWorkPoolAllocationReplaysExactActiveBindingAfterRuntimeLoss(t *testing.T) {
	opened, err := beads.OpenStoreAtForCity(t.Context(), beads.StoreOpenOptions{
		Provider:          "file",
		OpenFileStore:     func() (beads.Store, error) { return beads.NewMemStore(), nil },
		ConditionalWrites: gate.Auto,
	})
	if err != nil {
		t.Fatalf("open conditional recovery store: %v", err)
	}
	fixture := newRoutedWorkPoolAllocationFixture(t, opened.Store)
	work, err := fixture.store.Create(beads.Bead{
		Title:  "ready routed work survives pre-claim runtime loss",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.routed_to":                    "worker",
			beadmeta.PackMetadataKey:          "review-pack",
			beadmeta.PackWorkspaceMetadataKey: "workspace-a",
		},
	})
	if err != nil {
		t.Fatalf("create ready routed work: %v", err)
	}
	hint := routedWorkPoolAllocationHint{WorkID: work.ID, PoolTarget: "worker", SourceStore: "city:test-city"}

	first, err := fixture.cr.reconcileRoutedWorkPoolAllocation(t.Context(), hint)
	if err != nil || !first.Handled || !first.Created {
		t.Fatalf("initial allocation = (%+v, %v), want created handled session", first, err)
	}
	awaitCond(t, func() bool {
		return fixture.provider.IsRunning(first.Session.SessionName) && fixture.cr.sessionStartController.Pending() == 0
	}, "initial exact pool allocation to start")
	before, err := sessionFrontDoor(fixture.store).Get(first.Session.ID)
	if err != nil {
		t.Fatalf("read initially active allocation: %v", err)
	}
	if err := fixture.provider.Stop(before.SessionName); err != nil {
		t.Fatalf("remove only fake runtime before claim: %v", err)
	}
	if fixture.provider.IsRunning(before.SessionName) {
		t.Fatal("precondition: fake runtime remains running after removal")
	}
	stopsBeforeReplay := providerCallCount(fixture.provider, "Stop")
	nudgesBeforeReplay := providerNudgeCalls(fixture.provider, before.SessionName)

	replay, err := fixture.cr.reconcileRoutedWorkPoolAllocation(t.Context(), hint)
	if err != nil || !replay.Handled || replay.Created || replay.Session.ID != before.ID {
		t.Fatalf("exact allocation replay = (%+v, %v), want handled existing session %q", replay, err, before.ID)
	}
	awaitCond(t, func() bool {
		return fixture.provider.IsRunning(before.SessionName) && fixture.cr.sessionStartController.Pending() == 0
	}, "exact replay to restart the same active pool allocation")

	after, err := sessionFrontDoor(fixture.store).Get(before.ID)
	if err != nil {
		t.Fatalf("read restarted allocation: %v", err)
	}
	if after.ID != before.ID || after.SessionName != before.SessionName || after.PoolSlot != before.PoolSlot {
		t.Fatalf("restarted allocation identity = (%q, %q, %q), want (%q, %q, %q)", after.ID, after.SessionName, after.PoolSlot, before.ID, before.SessionName, before.PoolSlot)
	}
	if after.InstanceToken == before.InstanceToken {
		t.Fatal("restarted allocation did not rotate its runtime identity")
	}
	if after.TriggerBeadID != before.TriggerBeadID || after.TriggerBeadStoreRef != before.TriggerBeadStoreRef ||
		after.BrainParentSID != before.BrainParentSID || after.Pack != before.Pack || after.PackWorkspace != before.PackWorkspace ||
		after.WorkDirCanonical != before.WorkDirCanonical || after.WorkDir != before.WorkDir {
		t.Fatalf("restarted allocation binding/workdirs changed: before=%+v after=%+v", before, after)
	}
	if got := providerCallCount(fixture.provider, "Start"); got != 2 {
		t.Fatalf("provider Start calls = %d, want exactly initial start plus replay restart", got)
	}
	if got := providerCallCount(fixture.provider, "Stop"); got != stopsBeforeReplay {
		t.Fatalf("provider Stop calls after replay = %d, want no stop beyond pre-claim runtime removal %d", got, stopsBeforeReplay)
	}
	if got := providerNudgeCalls(fixture.provider, before.SessionName); got != nudgesBeforeReplay {
		t.Fatalf("provider nudge calls after replay = %d, want unchanged %d", got, nudgesBeforeReplay)
	}
	infos, err := loadSessionBeadSnapshot(fixture.store)
	if err != nil {
		t.Fatalf("load durable sessions after replay: %v", err)
	}
	if got := len(infos.OpenInfos()); got != 1 {
		t.Fatalf("open session rows after replay = %d, want the original row only", got)
	}

	idempotent, err := fixture.cr.reconcileRoutedWorkPoolAllocation(t.Context(), hint)
	if err != nil || !idempotent.Handled || idempotent.Created || idempotent.Session.ID != before.ID {
		t.Fatalf("live replay = (%+v, %v), want handled idempotent existing session", idempotent, err)
	}
	if got := providerCallCount(fixture.provider, "Start"); got != 2 {
		t.Fatalf("provider Start calls after live replay = %d, want exactly 2", got)
	}
}

func TestRoutedWorkPoolAllocationRecoveryParksOnIncompleteAbsence(t *testing.T) {
	for _, mode := range []rollout.Mode{rollout.Auto, rollout.Require} {
		t.Run(string(mode), func(t *testing.T) {
			fixture := newRoutedWorkPoolAllocationFixture(t, beads.NewMemStore())
			work, err := fixture.store.Create(beads.Bead{
				Title:    "ready routed work with uncertain runtime loss",
				Type:     "task",
				Status:   "open",
				Metadata: map[string]string{"gc.routed_to": "worker"},
			})
			if err != nil {
				t.Fatalf("create ready routed work: %v", err)
			}
			hint := routedWorkPoolAllocationHint{WorkID: work.ID, PoolTarget: "worker", SourceStore: "city:test-city"}
			first, err := fixture.cr.reconcileRoutedWorkPoolAllocation(t.Context(), hint)
			if err != nil || !first.Handled || !first.Created {
				t.Fatalf("initial allocation = (%+v, %v), want created handled session", first, err)
			}
			awaitCond(t, func() bool {
				return fixture.provider.IsRunning(first.Session.SessionName) && fixture.cr.sessionStartController.Pending() == 0
			}, "initial exact pool allocation to start")
			if err := fixture.provider.Stop(first.Session.SessionName); err != nil {
				t.Fatalf("remove runtime before recovery admission: %v", err)
			}

			uncertain := &fixedFreshLivenessProvider{
				Provider:    fixture.cr.sp,
				observation: runtime.Liveness{},
			}
			fixture.cr.sp = uncertain
			fixture.cr.cs.mu.Lock()
			fixture.cr.cs.sp = uncertain
			fixture.cr.cs.mu.Unlock()
			fixture.cr.sessionStartMu.Lock()
			fixture.cr.sessionStartMode = mode
			fixture.cr.sessionStartMu.Unlock()

			result, err := fixture.cr.reconcileRoutedWorkPoolAllocation(t.Context(), hint)
			if err != nil {
				t.Fatalf("reconcile incomplete runtime absence: %v", err)
			}
			if mode == rollout.Auto && result.Handled {
				t.Error("auto mode handled incomplete runtime absence, want legacy fallback")
			}
			if mode == rollout.Require && !result.Handled {
				t.Error("require mode released incomplete runtime absence to legacy, want parked ownership")
			}
			awaitCond(t, func() bool { return fixture.cr.sessionStartController.Pending() == 0 }, "incomplete recovery admission to settle")
			if got := providerCallCount(fixture.provider, "Start"); got != 1 {
				t.Fatalf("provider Start calls = %d, want only the initial start", got)
			}
		})
	}
}

func TestRoutedWorkPoolAllocationGrowsOccupiedUnlimitedPoolForDistinctRoutedWork(t *testing.T) {
	fixture := newRoutedWorkPoolAllocationFixture(t, beads.NewMemStore())

	firstWork, err := fixture.store.Create(beads.Bead{
		Title:    "first ready routed work",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create first routed work: %v", err)
	}
	firstHint := routedWorkPoolAllocationHint{
		WorkID:      firstWork.ID,
		PoolTarget:  "worker",
		SourceStore: "city:test-city",
		EventAt:     time.Now().UTC().Add(-time.Second),
		EnqueuedAt:  time.Now().UTC(),
	}
	first, err := fixture.cr.reconcileRoutedWorkPoolAllocation(t.Context(), firstHint)
	if err != nil {
		t.Fatalf("allocate first routed work: %v", err)
	}
	if !first.Handled || !first.Created || first.Session.PoolSlot != "1" {
		t.Fatalf("first allocation = %+v, want created slot-1 session", first)
	}
	awaitCond(t, func() bool {
		return fixture.provider.IsRunning(first.Session.SessionName) && fixture.cr.sessionStartController.Pending() == 0
	}, "first pool session to become active through keyed exact start")
	fixture.cr.handleRoutedWorkPoolAllocation(t.Context(), firstHint)
	awaitCond(t, func() bool { return fixture.cr.sessionStartController.Pending() == 0 }, "active duplicate to settle without another keyed start")
	if starts := fixture.provider.CountCalls("Start", first.Session.SessionName); starts != 1 {
		t.Fatalf("provider Start calls for active duplicate = %d, want 1", starts)
	}
	if len(fixture.cr.pokeCh) != 0 {
		t.Fatalf("legacy fallback after active duplicate = %d pokes, want none", len(fixture.cr.pokeCh))
	}

	secondWork, err := fixture.store.Create(beads.Bead{
		Title:    "second distinct ready routed work",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create second routed work: %v", err)
	}
	secondHint := routedWorkPoolAllocationHint{
		WorkID:      secondWork.ID,
		PoolTarget:  "worker",
		SourceStore: "city:test-city",
		EventAt:     time.Now().UTC().Add(-time.Second),
		EnqueuedAt:  time.Now().UTC(),
	}
	second, err := fixture.cr.reconcileRoutedWorkPoolAllocation(t.Context(), secondHint)
	if err != nil {
		t.Fatalf("allocate second routed work: %v", err)
	}
	if !second.Handled || !second.Created || second.Session.ID == first.Session.ID || second.Session.PoolSlot != "2" {
		t.Fatalf("second allocation = %+v, want one distinct created slot-2 session", second)
	}
	awaitCond(t, func() bool {
		return fixture.provider.IsRunning(second.Session.SessionName) && fixture.cr.sessionStartController.Pending() == 0
	}, "second pool session to become active through keyed exact start")

	infos, err := loadSessionBeadSnapshot(fixture.store)
	if err != nil {
		t.Fatalf("load pool sessions after second allocation: %v", err)
	}
	if open := infos.OpenInfos(); len(open) != 2 {
		t.Fatalf("open sessions after distinct routed work = %d, want 2: %+v", len(open), open)
	}

	replay, err := fixture.cr.reconcileRoutedWorkPoolAllocation(t.Context(), secondHint)
	if err != nil {
		t.Fatalf("replay second routed work: %v", err)
	}
	if !replay.Handled || replay.Created || replay.Session.ID != second.Session.ID {
		t.Fatalf("replayed second allocation = %+v, want rediscovered session %q without create", replay, second.Session.ID)
	}
	awaitCond(t, func() bool { return fixture.cr.sessionStartController.Pending() == 0 }, "replayed exact start admission to settle")
	infos, err = loadSessionBeadSnapshot(fixture.store)
	if err != nil {
		t.Fatalf("load pool sessions after replay: %v", err)
	}
	if open := infos.OpenInfos(); len(open) != 2 {
		t.Fatalf("open sessions after replay = %d, want 2: %+v", len(open), open)
	}
	if starts := fixture.provider.CountCalls("Start", second.Session.SessionName); starts != 1 {
		t.Fatalf("provider Start calls for replayed slot-2 session = %d, want 1", starts)
	}
}

func TestRoutedWorkPoolAllocationStartsBelowBoundedAgentCapAndFallsBackAtCap(t *testing.T) {
	fixture := newRoutedWorkPoolAllocationFixture(t, beads.NewMemStore())
	maximum := 2
	fixture.cr.cfg.Agents[0].MaxActiveSessions = &maximum

	allocate := func(title string) routedWorkPoolAllocationResult {
		t.Helper()
		work, err := fixture.store.Create(beads.Bead{
			Title:    title,
			Type:     "task",
			Status:   "open",
			Metadata: map[string]string{"gc.routed_to": "worker"},
		})
		if err != nil {
			t.Fatalf("create %s: %v", title, err)
		}
		hint := routedWorkPoolAllocationHint{
			WorkID:      work.ID,
			PoolTarget:  "worker",
			SourceStore: "city:test-city",
			EventAt:     time.Now().UTC().Add(-time.Second),
			EnqueuedAt:  time.Now().UTC(),
		}
		result, err := fixture.cr.reconcileRoutedWorkPoolAllocation(t.Context(), hint)
		if err != nil {
			t.Fatalf("allocate %s: %v", title, err)
		}
		return result
	}

	first := allocate("first bounded-pool work")
	if !first.Handled || !first.Created || first.Session.PoolSlot != "1" {
		t.Fatalf("first bounded allocation = %+v, want created slot-1 session", first)
	}
	awaitCond(t, func() bool {
		return fixture.provider.IsRunning(first.Session.SessionName) && fixture.cr.sessionStartController.Pending() == 0
	}, "first bounded-pool session to start")

	second := allocate("second bounded-pool work")
	if !second.Handled || !second.Created || second.Session.PoolSlot != "2" {
		t.Fatalf("second bounded allocation = %+v, want created slot-2 session", second)
	}
	awaitCond(t, func() bool {
		return fixture.provider.IsRunning(second.Session.SessionName) && fixture.cr.sessionStartController.Pending() == 0
	}, "second bounded-pool session to start")

	thirdWork, err := fixture.store.Create(beads.Bead{
		Title:    "third bounded-pool work",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create third bounded-pool work: %v", err)
	}
	fixture.cr.handleRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{
		WorkID:      thirdWork.ID,
		PoolTarget:  "worker",
		SourceStore: "city:test-city",
	})
	assertRoutedWorkPoolAllocationCensusOwed(t, fixture.cr)

	infos, err := loadSessionBeadSnapshot(fixture.store)
	if err != nil {
		t.Fatalf("load bounded pool sessions: %v", err)
	}
	if open := infos.OpenInfos(); len(open) != maximum {
		t.Fatalf("open bounded pool sessions = %d, want cap %d: %+v", len(open), maximum, open)
	}
	if starts := fixture.provider.CountCalls("Start", first.Session.SessionName) + fixture.provider.CountCalls("Start", second.Session.SessionName); starts != maximum {
		t.Fatalf("bounded pool provider starts = %d, want %d", starts, maximum)
	}
}

func TestRoutedWorkPoolAllocationStartsColdCanonicalSingletonAndFallsBackAtCap(t *testing.T) {
	fixture := newRoutedWorkPoolAllocationFixture(t, beads.NewMemStore())
	maximum := 1
	fixture.cr.cfg.Agents[0].MaxActiveSessions = &maximum

	firstWork, err := fixture.store.Create(beads.Bead{
		Title:    "first singleton work",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create first singleton work: %v", err)
	}
	firstHint := routedWorkPoolAllocationHint{
		WorkID:      firstWork.ID,
		PoolTarget:  "worker",
		SourceStore: "city:test-city",
		EventAt:     time.Now().UTC().Add(-time.Second),
		EnqueuedAt:  time.Now().UTC(),
	}
	first, err := fixture.cr.reconcileRoutedWorkPoolAllocation(t.Context(), firstHint)
	if err != nil {
		t.Fatalf("allocate cold singleton work: %v", err)
	}
	if !first.Handled || !first.Created || first.Session.PoolSlot != "" {
		t.Fatalf("cold singleton allocation = %+v, want one canonical slotless session", first)
	}
	stored, err := fixture.store.Get(first.Session.ID)
	if err != nil {
		t.Fatalf("read canonical singleton session: %v", err)
	}
	if stored.Metadata["agent_name"] != "worker" || stored.Metadata["alias"] != "worker" || stored.Metadata["pool_slot"] != "" {
		t.Fatalf("singleton identity metadata = %+v, want canonical unsuffixed worker", stored.Metadata)
	}
	awaitCond(t, func() bool {
		return fixture.provider.IsRunning(first.Session.SessionName) && fixture.cr.sessionStartController.Pending() == 0
	}, "cold canonical singleton to start")

	secondWork, err := fixture.store.Create(beads.Bead{
		Title:    "second singleton work",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create second singleton work: %v", err)
	}
	fixture.cr.handleRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{
		WorkID:      secondWork.ID,
		PoolTarget:  "worker",
		SourceStore: "city:test-city",
	})
	assertRoutedWorkPoolAllocationCensusOwed(t, fixture.cr)

	infos, err := loadSessionBeadSnapshot(fixture.store)
	if err != nil {
		t.Fatalf("load singleton sessions: %v", err)
	}
	if open := infos.OpenInfos(); len(open) != maximum {
		t.Fatalf("open singleton sessions = %d, want cap %d: %+v", len(open), maximum, open)
	}
	if starts := fixture.provider.CountCalls("Start", first.Session.SessionName); starts != 1 {
		t.Fatalf("singleton provider starts = %d, want 1", starts)
	}
}

func TestRoutedWorkPoolAllocationReusesIdleCanonicalSingletonForNewWork(t *testing.T) {
	opened, err := beads.OpenStoreAtForCity(t.Context(), beads.StoreOpenOptions{
		Provider:          "file",
		OpenFileStore:     func() (beads.Store, error) { return beads.NewMemStore(), nil },
		ConditionalWrites: gate.Auto,
	})
	if err != nil {
		t.Fatalf("open conditional-write singleton store: %v", err)
	}
	fixture := newRoutedWorkPoolAllocationFixture(t, opened.Store)
	maximum := 1
	fixture.cr.cfg.Agents[0].MaxActiveSessions = &maximum
	fixture.cr.cfg.Agents[0].Provider = "claude"
	fixture.cr.cfg.Agents[0].WorkDir = "worker-root"
	fixture.cr.cfg.Agents[0].Nudge = "Run gc hook --claim --json now."

	firstWork, err := fixture.store.Create(beads.Bead{
		Title:    "first singleton work",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create first singleton work: %v", err)
	}
	firstHint := routedWorkPoolAllocationHint{
		WorkID:      firstWork.ID,
		PoolTarget:  "worker",
		SourceStore: "city:test-city",
	}
	first, err := fixture.cr.reconcileRoutedWorkPoolAllocation(t.Context(), firstHint)
	if err != nil || !first.Handled || !first.Created {
		t.Fatalf("allocate cold singleton = (%+v, %v), want one keyed create", first, err)
	}
	awaitCond(t, func() bool {
		return fixture.provider.IsRunning(first.Session.SessionName) && fixture.cr.sessionStartController.Pending() == 0
	}, "cold canonical singleton to start")
	if err := fixture.store.Close(firstWork.ID); err != nil {
		t.Fatalf("close first singleton work: %v", err)
	}
	active, err := sessionFrontDoor(fixture.store).Get(first.Session.ID)
	if err != nil {
		t.Fatalf("read active singleton: %v", err)
	}
	setRoutedWorkPoolRuntimeIdentity(t, fixture, active)
	fixture.provider.WaitForIdleErrors[first.Session.SessionName] = nil
	baselineNudges := providerNudgeCalls(fixture.provider, first.Session.SessionName)

	secondWork, err := fixture.store.Create(beads.Bead{
		Title:  "second singleton work",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.routed_to":                    "worker",
			beadmeta.PackMetadataKey:          "review-pack",
			beadmeta.PackWorkspaceMetadataKey: "workspace-b",
		},
	})
	if err != nil {
		t.Fatalf("create second singleton work: %v", err)
	}
	secondHint := routedWorkPoolAllocationHint{
		WorkID:      secondWork.ID,
		PoolTarget:  "worker",
		SourceStore: "city:test-city",
	}
	fixture.cr.handleRoutedWorkPoolAllocation(t.Context(), secondHint)

	infos, err := loadSessionBeadSnapshot(fixture.store)
	if err != nil {
		t.Fatalf("load singleton sessions after reuse: %v", err)
	}
	open := infos.OpenInfos()
	if len(open) != 1 || open[0].ID != first.Session.ID {
		t.Fatalf("open singleton sessions after reuse = %+v, want only %q", open, first.Session.ID)
	}
	stored, err := fixture.store.Get(first.Session.ID)
	if err != nil {
		t.Fatalf("read rebound singleton session: %v", err)
	}
	wantWorkDir := filepath.Join(fixture.cr.cityPath, "review-pack", "workspace-b")
	if stored.Metadata[beadmeta.TriggerBeadIDMetadataKey] != secondWork.ID ||
		stored.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey] != secondHint.SourceStore ||
		stored.Metadata[beadmeta.PackMetadataKey] != "review-pack" ||
		stored.Metadata[beadmeta.PackWorkspaceMetadataKey] != "workspace-b" ||
		stored.Metadata[beadmeta.WorkDirMetadataKey] != wantWorkDir ||
		stored.Metadata[beadmeta.LegacyWorkDirMetadataKey] != wantWorkDir {
		t.Fatalf("rebound singleton trigger metadata = %+v, want exact second-work provenance; pokes=%d stderr=%q",
			stored.Metadata, len(fixture.cr.pokeCh), fixture.stderr.String())
	}
	if got := fixture.provider.CountCalls("Start", first.Session.SessionName); got != 1 {
		t.Fatalf("provider Start calls after singleton reuse = %d, want 1", got)
	}
	if got := fixture.provider.CountCalls("Stop", first.Session.SessionName); got != 0 {
		t.Fatalf("provider Stop calls after singleton reuse = %d, want 0", got)
	}
	if got := providerNudgeCalls(fixture.provider, first.Session.SessionName); got != baselineNudges+1 {
		t.Fatalf("claim nudge calls after singleton reuse = %d, want %d", got, baselineNudges+1)
	}
	if len(fixture.cr.pokeCh) != 0 {
		t.Fatalf("legacy fallback after singleton reuse = %d pokes, want none", len(fixture.cr.pokeCh))
	}

	reboundRevision := stored.Revision
	fixture.cr.handleRoutedWorkPoolAllocation(t.Context(), secondHint)
	replayed, err := fixture.store.Get(first.Session.ID)
	if err != nil {
		t.Fatalf("read singleton session after replay: %v", err)
	}
	if replayed.Revision != reboundRevision {
		t.Fatalf("singleton replay revision = %d, want unchanged %d", replayed.Revision, reboundRevision)
	}
	if got := providerNudgeCalls(fixture.provider, first.Session.SessionName); got != baselineNudges+1 {
		t.Fatalf("claim nudge calls after replay = %d, want %d", got, baselineNudges+1)
	}
	if len(fixture.cr.pokeCh) != 0 {
		t.Fatalf("legacy fallback after singleton replay = %d pokes, want none", len(fixture.cr.pokeCh))
	}
}

func TestRoutedWorkPoolAllocationReusesSoleIdleGenericMemberForNewWork(t *testing.T) {
	for _, maximum := range []int{2, -1} {
		t.Run(fmt.Sprintf("max_active_sessions=%d", maximum), func(t *testing.T) {
			fixture, firstWork, info := prepareIdleGenericPoolMemberForReuse(t, true, maximum)
			fixture.cr.cfg.Agents[0].WorkDir = ".gc/worktrees/{{.AgentBase}}"
			if err := fixture.store.Close(firstWork.ID); err != nil {
				t.Fatalf("close prior routed work: %v", err)
			}
			baselineNudges := providerNudgeCalls(fixture.provider, info.SessionNameMetadata)
			baselineCalls := len(fixture.provider.SnapshotCalls())
			secondWork, err := fixture.store.Create(beads.Bead{
				Title:  "second generic work",
				Type:   "task",
				Status: "open",
				Metadata: map[string]string{
					"gc.routed_to":                    "worker",
					beadmeta.PackWorkspaceMetadataKey: "workspace-b",
				},
			})
			if err != nil {
				t.Fatalf("create second routed work: %v", err)
			}
			result, err := fixture.cr.reconcileRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{
				WorkID: secondWork.ID, PoolTarget: "worker", SourceStore: "city:test-city",
			})
			if err != nil || !result.Handled || result.Created || result.Session.ID != info.ID || result.Session.PoolSlot != info.PoolSlot {
				t.Fatalf("reuse sole generic member = (%+v, %v), want existing %q without create", result, err, info.ID)
			}
			stored, err := fixture.store.Get(info.ID)
			if err != nil {
				t.Fatalf("read rebound generic member: %v", err)
			}
			wantWorkDir := filepath.Join(fixture.cr.cityPath, ".gc", "worktrees", "worker-1", "workspace-b")
			if stored.Metadata[beadmeta.TriggerBeadIDMetadataKey] != secondWork.ID ||
				stored.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey] != "city:test-city" ||
				stored.Metadata["pool_slot"] != info.PoolSlot ||
				stored.Metadata[beadmeta.PackMetadataKey] != "" ||
				stored.Metadata[beadmeta.PackWorkspaceMetadataKey] != "workspace-b" ||
				stored.Metadata[beadmeta.WorkDirMetadataKey] != wantWorkDir ||
				stored.Metadata[beadmeta.LegacyWorkDirMetadataKey] != wantWorkDir {
				t.Fatalf("rebound generic metadata = %#v, want exact second-work provenance with work dir %q", stored.Metadata, wantWorkDir)
			}
			infos, err := loadSessionBeadSnapshot(fixture.store)
			if err != nil {
				t.Fatalf("load generic sessions: %v", err)
			}
			if open := infos.OpenInfos(); len(open) != 1 || open[0].ID != info.ID {
				t.Fatalf("open generic sessions = %+v, want only %q", open, info.ID)
			}
			if got := providerCallCount(fixture.provider, "Start"); got != 1 {
				t.Fatalf("global provider Start calls after generic reuse = %d, want 1", got)
			}
			if got := providerCallCount(fixture.provider, "Stop"); got != 0 {
				t.Fatalf("global provider Stop calls after generic reuse = %d, want 0", got)
			}
			if got := providerNudgeCalls(fixture.provider, info.SessionNameMetadata); got != baselineNudges+1 {
				t.Fatalf("generic nudge calls = %d, want %d", got, baselineNudges+1)
			}
			assertExactProviderNudgeSince(t, fixture.provider, baselineCalls, info.SessionNameMetadata, "<system-reminder>\nYou have a deferred reminder that was queued until a safe boundary:\n\n- [routed-work-pool-reuse] Run gc hook --claim --json now.\n\nHandle them after this turn.\n</system-reminder>\n")
			if len(fixture.cr.pokeCh) != 0 {
				t.Fatalf("legacy fallback after generic reuse = %d pokes, want none", len(fixture.cr.pokeCh))
			}

			reboundRevision := stored.Revision
			replay, err := fixture.cr.reconcileRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{
				WorkID: secondWork.ID, PoolTarget: "worker", SourceStore: "city:test-city",
			})
			if err != nil || !replay.Handled || replay.Created || replay.Session.ID != info.ID {
				t.Fatalf("replay generic reuse = (%+v, %v), want same existing member", replay, err)
			}
			afterReplay, err := fixture.store.Get(info.ID)
			if err != nil {
				t.Fatalf("read generic member after replay: %v", err)
			}
			if afterReplay.Revision != reboundRevision {
				t.Fatalf("generic replay revision = %d, want unchanged %d", afterReplay.Revision, reboundRevision)
			}
			if got := providerNudgeCalls(fixture.provider, info.SessionNameMetadata); got != baselineNudges+1 {
				t.Fatalf("generic replay nudge calls = %d, want %d", got, baselineNudges+1)
			}
			if len(fixture.cr.pokeCh) != 0 {
				t.Fatalf("legacy fallback after generic replay = %d pokes, want none", len(fixture.cr.pokeCh))
			}
		})
	}
}

func TestRoutedWorkPoolAllocationReusesLaterIdleMemberOfMultiMemberPool(t *testing.T) {
	for _, maximum := range []int{3, -1} {
		t.Run(fmt.Sprintf("max_active_sessions=%d", maximum), func(t *testing.T) {
			fixture, firstWork, older, secondWork, newer := prepareTwoMemberGenericPoolForReuse(t, maximum)
			if err := fixture.store.Close(firstWork.ID); err != nil {
				t.Fatalf("close older trigger work: %v", err)
			}
			if err := fixture.store.Close(secondWork.ID); err != nil {
				t.Fatalf("close newer trigger work: %v", err)
			}
			if _, err := fixture.store.Create(beads.Bead{Title: "older assigned work", Type: "task", Status: "open", Assignee: older.ID}); err != nil {
				t.Fatalf("create older assigned work: %v", err)
			}
			beforeOlder, err := fixture.store.Get(older.ID)
			if err != nil {
				t.Fatalf("read older member before reuse: %v", err)
			}
			baselineCalls := len(fixture.provider.SnapshotCalls())
			thirdWork, err := fixture.store.Create(beads.Bead{Title: "third routed work", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "worker"}})
			if err != nil {
				t.Fatalf("create third routed work: %v", err)
			}

			result, err := fixture.cr.reconcileRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{WorkID: thirdWork.ID, PoolTarget: "worker", SourceStore: "city:test-city"})
			if err != nil || !result.Handled || result.Created || result.Session.ID != newer.ID || result.Session.PoolSlot != newer.PoolSlot {
				t.Fatalf("reuse later idle member = (%+v, %v), want existing newer slot-2 member %q", result, err, newer.ID)
			}
			afterOlder, err := fixture.store.Get(older.ID)
			if err != nil {
				t.Fatalf("read older member after reuse: %v", err)
			}
			if !reflect.DeepEqual(afterOlder, beforeOlder) {
				t.Fatalf("reuse changed older busy member\n before=%+v\n  after=%+v", beforeOlder, afterOlder)
			}
			stored, err := fixture.store.Get(newer.ID)
			if err != nil {
				t.Fatalf("read rebound newer member: %v", err)
			}
			if stored.Metadata[beadmeta.TriggerBeadIDMetadataKey] != thirdWork.ID || stored.Metadata["pool_slot"] != newer.PoolSlot {
				t.Fatalf("rebound newer member = %#v, want third-work trigger and unchanged slot %q", stored.Metadata, newer.PoolSlot)
			}
			if got := providerCallCount(fixture.provider, "Start"); got != 2 {
				t.Fatalf("global provider Start calls after multi-member reuse = %d, want 2", got)
			}
			if got := providerCallCount(fixture.provider, "Stop"); got != 0 {
				t.Fatalf("global provider Stop calls after multi-member reuse = %d, want 0", got)
			}
			assertExactProviderNudgeSince(t, fixture.provider, baselineCalls, newer.SessionNameMetadata, "<system-reminder>\nYou have a deferred reminder that was queued until a safe boundary:\n\n- [routed-work-pool-reuse] Run gc hook --claim --json now.\n\nHandle them after this turn.\n</system-reminder>\n")
			if len(fixture.cr.pokeCh) != 0 {
				t.Fatalf("legacy fallback after multi-member reuse = %d pokes, want none", len(fixture.cr.pokeCh))
			}
		})
	}
}

func TestRoutedWorkPoolAllocationReusesOldestIdleMemberOfMultiMemberPool(t *testing.T) {
	fixture, firstWork, older, secondWork, newer := prepareTwoMemberGenericPoolForReuse(t, 3)
	if err := fixture.store.Close(firstWork.ID); err != nil {
		t.Fatalf("close older trigger work: %v", err)
	}
	if err := fixture.store.Close(secondWork.ID); err != nil {
		t.Fatalf("close newer trigger work: %v", err)
	}
	beforeNewer, err := fixture.store.Get(newer.ID)
	if err != nil {
		t.Fatalf("read newer member before reuse: %v", err)
	}
	baselineCalls := len(fixture.provider.SnapshotCalls())
	thirdWork, err := fixture.store.Create(beads.Bead{Title: "third routed work", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "worker"}})
	if err != nil {
		t.Fatalf("create third routed work: %v", err)
	}

	result, err := fixture.cr.reconcileRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{WorkID: thirdWork.ID, PoolTarget: "worker", SourceStore: "city:test-city"})
	if err != nil || !result.Handled || result.Created || result.Session.ID != older.ID || result.Session.PoolSlot != older.PoolSlot {
		t.Fatalf("reuse oldest idle member = (%+v, %v), want existing older slot-1 member %q", result, err, older.ID)
	}
	storedOlder, err := fixture.store.Get(older.ID)
	if err != nil {
		t.Fatalf("read rebound older member: %v", err)
	}
	if storedOlder.Metadata[beadmeta.TriggerBeadIDMetadataKey] != thirdWork.ID || storedOlder.Metadata["pool_slot"] != older.PoolSlot {
		t.Fatalf("rebound older member = %#v, want third-work trigger and unchanged slot %q", storedOlder.Metadata, older.PoolSlot)
	}
	afterNewer, err := fixture.store.Get(newer.ID)
	if err != nil {
		t.Fatalf("read newer member after reuse: %v", err)
	}
	if !reflect.DeepEqual(afterNewer, beforeNewer) {
		t.Fatalf("reuse changed newer idle member\n before=%+v\n  after=%+v", beforeNewer, afterNewer)
	}
	if got := providerCallCount(fixture.provider, "Start"); got != 2 {
		t.Fatalf("global provider Start calls after deterministic multi-member reuse = %d, want 2", got)
	}
	if got := providerCallCount(fixture.provider, "Stop"); got != 0 {
		t.Fatalf("global provider Stop calls after deterministic multi-member reuse = %d, want 0", got)
	}
	assertExactProviderNudgeSince(t, fixture.provider, baselineCalls, older.SessionNameMetadata, "<system-reminder>\nYou have a deferred reminder that was queued until a safe boundary:\n\n- [routed-work-pool-reuse] Run gc hook --claim --json now.\n\nHandle them after this turn.\n</system-reminder>\n")
	if len(fixture.cr.pokeCh) != 0 {
		t.Fatalf("legacy fallback after deterministic multi-member reuse = %d pokes, want none", len(fixture.cr.pokeCh))
	}
	infos, err := loadSessionBeadSnapshot(fixture.store)
	if err != nil {
		t.Fatalf("load sessions after deterministic multi-member reuse: %v", err)
	}
	if open := infos.OpenInfos(); len(open) != 2 {
		t.Fatalf("open sessions after deterministic multi-member reuse = %+v, want original two", open)
	}
	reboundRevision := storedOlder.Revision
	reboundNudges := providerNudgeCalls(fixture.provider, older.SessionNameMetadata)
	replay, err := fixture.cr.reconcileRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{WorkID: thirdWork.ID, PoolTarget: "worker", SourceStore: "city:test-city"})
	if err != nil || !replay.Handled || replay.Created || replay.Session.ID != older.ID {
		t.Fatalf("replay deterministic multi-member reuse = (%+v, %v), want existing older member", replay, err)
	}
	afterReplay, err := fixture.store.Get(older.ID)
	if err != nil {
		t.Fatalf("read older member after replay: %v", err)
	}
	if afterReplay.Revision != reboundRevision {
		t.Fatalf("older member revision after replay = %d, want unchanged %d", afterReplay.Revision, reboundRevision)
	}
	if got := providerNudgeCalls(fixture.provider, older.SessionNameMetadata); got != reboundNudges {
		t.Fatalf("older member nudges after replay = %d, want unchanged %d", got, reboundNudges)
	}
}

func TestRoutedWorkPoolAllocationRebindEventDoesNotInvalidateItsLease(t *testing.T) {
	opened, err := beads.OpenStoreAtForCity(t.Context(), beads.StoreOpenOptions{
		Provider:          "file",
		OpenFileStore:     func() (beads.Store, error) { return beads.NewMemStore(), nil },
		ConditionalWrites: gate.Auto,
	})
	if err != nil {
		t.Fatalf("open conditional-write store: %v", err)
	}
	var cr *CityRuntime
	var reboundSessionID string
	var refreshes atomic.Int32
	store := beads.NewCachingStoreForTest(opened.Store, func(eventType, beadID string, _ json.RawMessage) {
		if cr == nil || eventType != "bead.updated" || beadID != reboundSessionID {
			return
		}
		refreshes.Add(1)
		cr.refreshPoolMembershipSession(beadID)
	})
	fixture := newRoutedWorkPoolAllocationFixture(t, store)
	cr = fixture.cr
	maximum := 1
	fixture.cr.cfg.Agents[0].MaxActiveSessions = &maximum
	fixture.cr.cfg.Agents[0].Provider = "claude"
	fixture.cr.cfg.Agents[0].Nudge = "Run gc hook --claim --json now."

	firstWork, err := store.Create(beads.Bead{
		Title: "first work", Type: "task", Status: "open",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create first work: %v", err)
	}
	first, err := fixture.cr.reconcileRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{
		WorkID: firstWork.ID, PoolTarget: "worker", SourceStore: "city:test-city",
	})
	if err != nil || !first.Handled || !first.Created {
		t.Fatalf("allocate first member = (%+v, %v), want created", first, err)
	}
	awaitCond(t, func() bool {
		return fixture.provider.IsRunning(first.Session.SessionName) && fixture.cr.sessionStartController.Pending() == 0
	}, "first member to start")
	info, err := sessionFrontDoor(store).Get(first.Session.ID)
	if err != nil {
		t.Fatalf("read first member: %v", err)
	}
	setRoutedWorkPoolRuntimeIdentity(t, fixture, info)
	fixture.cr.refreshPoolMembershipSession(info.ID)
	fixture.provider.WaitForIdleErrors[info.SessionNameMetadata] = nil
	if err := store.Close(firstWork.ID); err != nil {
		t.Fatalf("close first work: %v", err)
	}
	secondWork, err := store.Create(beads.Bead{
		Title: "second work", Type: "task", Status: "open",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create second work: %v", err)
	}
	reboundSessionID = info.ID
	baselineNudges := providerAllNudgeCalls(fixture.provider)

	result, err := fixture.cr.reconcileRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{
		WorkID: secondWork.ID, PoolTarget: "worker", SourceStore: "city:test-city",
	})
	if err != nil || !result.Handled || result.Created || result.Session.ID != info.ID {
		t.Fatalf("reuse with synchronous rebind event = (%+v, %v), want existing member %q", result, err, info.ID)
	}
	if refreshes.Load() != 1 {
		t.Fatalf("synchronous membership refreshes = %d, want exactly 1 rebind event", refreshes.Load())
	}
	if got := providerAllNudgeCalls(fixture.provider); got != baselineNudges+1 {
		t.Fatalf("nudges after rebind event = %d, want %d", got, baselineNudges+1)
	}
	if len(fixture.cr.pokeCh) != 0 {
		t.Fatalf("legacy fallback after rebind event = %d pokes, want none", len(fixture.cr.pokeCh))
	}
}

func TestRoutedWorkPoolAllocationCanonicalReuseRequiresExactSoleMember(t *testing.T) {
	fixture, firstWork, canonical := prepareIdleCanonicalSingletonForReuse(t, true)
	if err := fixture.store.Close(firstWork.ID); err != nil {
		t.Fatalf("close canonical trigger work: %v", err)
	}
	extra, err := createPoolSessionBeadWithAlias(fixture.store, "worker", fixture.cr.cfg, nil, time.Now().UTC(), poolSessionCreateIdentity{
		AgentName: "worker-2",
		Slot:      2,
	}, "")
	if err != nil {
		t.Fatalf("create conflicting certified member: %v", err)
	}
	if err := fixture.store.SetMetadataBatch(extra.ID, map[string]string{
		"state":                     string(sessionpkg.StateActive),
		"pending_create_claim":      "",
		"pending_create_started_at": "",
	}); err != nil {
		t.Fatalf("mark conflicting member active: %v", err)
	}
	extra, err = sessionFrontDoor(fixture.store).Get(extra.ID)
	if err != nil {
		t.Fatalf("read conflicting member: %v", err)
	}
	if err := fixture.cr.poolMembershipShadow.replace(fixture.cr.cfg, extra); err != nil {
		t.Fatalf("publish conflicting certified member: %v", err)
	}
	before, err := fixture.store.Get(canonical.ID)
	if err != nil {
		t.Fatalf("read canonical member before refusal: %v", err)
	}
	baselineNudges := providerNudgeCalls(fixture.provider, canonical.SessionNameMetadata)
	work, err := fixture.store.Create(beads.Bead{Title: "new canonical work", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "worker"}})
	if err != nil {
		t.Fatalf("create new canonical work: %v", err)
	}

	fixture.cr.handleRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{WorkID: work.ID, PoolTarget: "worker", SourceStore: "city:test-city"})

	after, err := fixture.store.Get(canonical.ID)
	if err != nil {
		t.Fatalf("read canonical member after refusal: %v", err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("non-sole canonical reuse changed the durable member\n before=%+v\n  after=%+v", before, after)
	}
	if got := providerNudgeCalls(fixture.provider, canonical.SessionNameMetadata); got != baselineNudges {
		t.Fatalf("canonical nudges with conflicting member = %d, want unchanged %d", got, baselineNudges)
	}
	assertRoutedWorkPoolAllocationCensusOwed(t, fixture.cr)
}

func TestRoutedWorkPoolAllocationRefusesMemberSetAdditionDuringSelection(t *testing.T) {
	fixture, firstWork, older, secondWork, newer := prepareTwoMemberGenericPoolForReuse(t, 4)
	if err := fixture.store.Close(firstWork.ID); err != nil {
		t.Fatalf("close older trigger work: %v", err)
	}
	if err := fixture.store.Close(secondWork.ID); err != nil {
		t.Fatalf("close newer trigger work: %v", err)
	}
	extra, err := createPoolSessionBeadWithAlias(fixture.store, "worker", fixture.cr.cfg, nil, time.Now().UTC(), poolSessionCreateIdentity{
		AgentName: "worker-3",
		Slot:      3,
	}, "")
	if err != nil {
		t.Fatalf("create unobserved third member: %v", err)
	}
	if err := fixture.store.SetMetadataBatch(extra.ID, map[string]string{
		"state":                     string(sessionpkg.StateActive),
		"pending_create_claim":      "",
		"pending_create_started_at": "",
	}); err != nil {
		t.Fatalf("mark unobserved third member active: %v", err)
	}
	extra, err = sessionFrontDoor(fixture.store).Get(extra.ID)
	if err != nil {
		t.Fatalf("read unobserved third member: %v", err)
	}
	provider := &poolReuseGetMetaHookProvider{
		sequenceGetMetaProvider: fixture.cr.cs.sp.(*sequenceGetMetaProvider),
		after: func() {
			if replaceErr := fixture.cr.poolMembershipShadow.replace(fixture.cr.cfg, extra); replaceErr != nil {
				t.Errorf("publish third member during selection: %v", replaceErr)
			}
		},
	}
	fixture.cr.sp = provider
	fixture.cr.cs.mu.Lock()
	fixture.cr.cs.sp = provider
	fixture.cr.cs.mu.Unlock()
	beforeOlder, err := fixture.store.Get(older.ID)
	if err != nil {
		t.Fatalf("read older member before set drift: %v", err)
	}
	beforeNewer, err := fixture.store.Get(newer.ID)
	if err != nil {
		t.Fatalf("read newer member before set drift: %v", err)
	}
	baselineNudges := providerAllNudgeCalls(fixture.provider)
	work, err := fixture.store.Create(beads.Bead{Title: "work racing membership growth", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "worker"}})
	if err != nil {
		t.Fatalf("create work racing membership growth: %v", err)
	}

	fixture.cr.handleRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{WorkID: work.ID, PoolTarget: "worker", SourceStore: "city:test-city"})

	for id, before := range map[string]beads.Bead{older.ID: beforeOlder, newer.ID: beforeNewer} {
		after, getErr := fixture.store.Get(id)
		if getErr != nil {
			t.Fatalf("read member %q after set drift: %v", id, getErr)
		}
		if !reflect.DeepEqual(after, before) {
			t.Fatalf("member-set drift changed candidate %q\n before=%+v\n  after=%+v", id, before, after)
		}
	}
	if got := providerAllNudgeCalls(fixture.provider); got != baselineNudges {
		t.Fatalf("nudges after member-set drift = %d, want unchanged %d", got, baselineNudges)
	}
	assertRoutedWorkPoolAllocationCensusOwed(t, fixture.cr)
}

func TestRoutedWorkPoolAllocationRefusesLaterReuseAfterBusyCandidateDrifts(t *testing.T) {
	fixture, firstWork, older, secondWork, newer := prepareTwoMemberGenericPoolForReuse(t, 3)
	if err := fixture.store.Close(firstWork.ID); err != nil {
		t.Fatalf("close older trigger work: %v", err)
	}
	if err := fixture.store.Close(secondWork.ID); err != nil {
		t.Fatalf("close newer trigger work: %v", err)
	}
	if _, err := fixture.store.Create(beads.Bead{Title: "older assigned work", Type: "task", Status: "open", Assignee: older.ID}); err != nil {
		t.Fatalf("create older assigned work: %v", err)
	}
	underlying := fixture.store
	hooked := &poolReuseAssignedListHookStore{
		Store: underlying,
		after: func() {
			if err := underlying.SetMetadata(older.ID, "state_reason", "busy-candidate-drift"); err != nil {
				t.Errorf("mutate busy candidate after classification: %v", err)
			}
		},
	}
	fixture.store = hooked
	fixture.cr.cs.mu.Lock()
	fixture.cr.cs.cityBeadStore = hooked
	fixture.cr.cs.mu.Unlock()
	beforeNewer, err := underlying.Get(newer.ID)
	if err != nil {
		t.Fatalf("read later member before busy drift: %v", err)
	}
	baselineNudges := providerNudgeCalls(fixture.provider, newer.SessionNameMetadata)
	work, err := underlying.Create(beads.Bead{Title: "work after drifting busy member", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "worker"}})
	if err != nil {
		t.Fatalf("create later routed work: %v", err)
	}

	fixture.cr.handleRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{WorkID: work.ID, PoolTarget: "worker", SourceStore: "city:test-city"})

	afterNewer, err := underlying.Get(newer.ID)
	if err != nil {
		t.Fatalf("read later member after busy drift: %v", err)
	}
	if !reflect.DeepEqual(afterNewer, beforeNewer) {
		t.Fatalf("busy-candidate drift changed later member\n before=%+v\n  after=%+v", beforeNewer, afterNewer)
	}
	if got := providerNudgeCalls(fixture.provider, newer.SessionNameMetadata); got != baselineNudges {
		t.Fatalf("later-member nudges after busy drift = %d, want unchanged %d", got, baselineNudges)
	}
	assertRoutedWorkPoolAllocationCensusOwed(t, fixture.cr)
}

func TestRoutedWorkPoolAllocationRefusesReuseDriftAfterIdleWait(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(testing.TB, routedWorkPoolAllocationFixture, sessionpkg.Info, sessionpkg.Info, beads.Bead)
	}{
		{
			name: "routed work drift",
			mutate: func(t testing.TB, fixture routedWorkPoolAllocationFixture, _, _ sessionpkg.Info, work beads.Bead) {
				t.Helper()
				if err := fixture.store.SetMetadata(work.ID, "gc.routed_to", "other"); err != nil {
					t.Fatalf("change routed work after idle wait: %v", err)
				}
			},
		},
		{
			name: "membership set drift",
			mutate: func(_ testing.TB, fixture routedWorkPoolAllocationFixture, _, other sessionpkg.Info, _ beads.Bead) {
				fixture.cr.poolMembershipShadow.remove(other.ID)
			},
		},
		{
			name: "runtime token replacement",
			mutate: func(t testing.TB, fixture routedWorkPoolAllocationFixture, selected, _ sessionpkg.Info, _ beads.Bead) {
				t.Helper()
				if err := fixture.provider.SetMeta(selected.SessionNameMetadata, "GC_INSTANCE_TOKEN", "replacement-token"); err != nil {
					t.Fatalf("replace runtime token after idle wait: %v", err)
				}
			},
		},
		{
			name: "durable session revision",
			mutate: func(t testing.TB, fixture routedWorkPoolAllocationFixture, selected, _ sessionpkg.Info, _ beads.Bead) {
				t.Helper()
				if err := fixture.store.SetMetadata(selected.ID, "state_reason", "concurrent-revision-only-write"); err != nil {
					t.Fatalf("advance rebound session revision after idle wait: %v", err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture, firstWork, selected, secondWork, other := prepareTwoMemberGenericPoolForReuse(t, 3)
			if err := fixture.store.Close(firstWork.ID); err != nil {
				t.Fatalf("close selected member's prior work: %v", err)
			}
			if err := fixture.store.Close(secondWork.ID); err != nil {
				t.Fatalf("close other member's prior work: %v", err)
			}
			work, err := fixture.store.Create(beads.Bead{Title: "work racing idle reuse", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "worker"}})
			if err != nil {
				t.Fatalf("create routed work: %v", err)
			}
			idleStarted := make(chan struct{})
			idleGate := make(chan struct{})
			fixture.provider.WaitForIdleStarted[selected.SessionNameMetadata] = idleStarted
			fixture.provider.WaitForIdleGates[selected.SessionNameMetadata] = idleGate
			baselineNudges := providerAllNudgeCalls(fixture.provider)
			baselineStarts := providerCallCount(fixture.provider, "Start")
			done := make(chan struct{})
			go func() {
				defer close(done)
				fixture.cr.handleRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{WorkID: work.ID, PoolTarget: "worker", SourceStore: "city:test-city"})
			}()
			awaitClose(t, idleStarted, "pool reuse idle wait")
			test.mutate(t, fixture, selected, other, work)
			close(idleGate)
			awaitClose(t, done, "pool reuse after post-wait drift")

			if got := providerAllNudgeCalls(fixture.provider); got != baselineNudges {
				t.Fatalf("nudges after %s = %d, want unchanged %d", test.name, got, baselineNudges)
			}
			if got := providerCallCount(fixture.provider, "Start"); got != baselineStarts {
				t.Fatalf("starts after %s = %d, want unchanged %d", test.name, got, baselineStarts)
			}
			if got := providerCallCount(fixture.provider, "Stop"); got != 0 {
				t.Fatalf("stops after %s = %d, want 0", test.name, got)
			}
			assertRoutedWorkPoolAllocationCensusOwed(t, fixture.cr)
		})
	}
}

func TestRoutedWorkPoolAllocationRefusesReboundRevisionDriftBeforeIdleWait(t *testing.T) {
	fixture, firstWork, info := prepareIdleGenericPoolMemberForReuse(t, true, 2)
	if err := fixture.store.Close(firstWork.ID); err != nil {
		t.Fatalf("close previous trigger work: %v", err)
	}
	work, err := fixture.store.Create(beads.Bead{
		Title: "work racing rebound authorization", Type: "task", Status: "open",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create routed work: %v", err)
	}
	underlying := fixture.store
	hooked := &poolReusePersistedReadHookStore{
		Store: underlying, sessionID: info.ID, workID: work.ID,
		beforeReturn: func() {
			if err := underlying.SetMetadata(info.ID, "state_reason", "concurrent-revision-only-write"); err != nil {
				t.Errorf("advance rebound session revision: %v", err)
			}
		},
	}
	fixture.store = hooked
	fixture.cr.cs.mu.Lock()
	fixture.cr.cs.cityBeadStore = hooked
	fixture.cr.cs.mu.Unlock()
	baselineNudges := providerAllNudgeCalls(fixture.provider)
	baselineStarts := providerCallCount(fixture.provider, "Start")

	fixture.cr.handleRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{
		WorkID: work.ID, PoolTarget: "worker", SourceStore: "city:test-city",
	})

	stored, err := underlying.Get(info.ID)
	if err != nil {
		t.Fatalf("read session after revision drift: %v", err)
	}
	if stored.Metadata[beadmeta.TriggerBeadIDMetadataKey] != work.ID || stored.Metadata["state_reason"] != "concurrent-revision-only-write" {
		t.Fatalf("durable session after revision drift = %#v, want committed binding plus concurrent write", stored.Metadata)
	}
	if got := providerAllNudgeCalls(fixture.provider); got != baselineNudges {
		t.Fatalf("nudges after rebound revision drift = %d, want unchanged %d", got, baselineNudges)
	}
	if got := providerCallCount(fixture.provider, "Start"); got != baselineStarts {
		t.Fatalf("starts after rebound revision drift = %d, want unchanged %d", got, baselineStarts)
	}
	assertRoutedWorkPoolAllocationCensusOwed(t, fixture.cr)
}

func TestRoutedWorkPoolAllocationRefusesUnavailableReboundRevision(t *testing.T) {
	for _, test := range []struct {
		name      string
		afterIdle bool
	}{
		{name: "immediate reread"},
		{name: "post-idle reread", afterIdle: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture, firstWork, info := prepareIdleGenericPoolMemberForReuse(t, true, 2)
			if err := fixture.store.Close(firstWork.ID); err != nil {
				t.Fatalf("close previous trigger work: %v", err)
			}
			work, err := fixture.store.Create(beads.Bead{
				Title: "work with unavailable rebound revision", Type: "task", Status: "open",
				Metadata: map[string]string{"gc.routed_to": "worker"},
			})
			if err != nil {
				t.Fatalf("create routed work: %v", err)
			}
			underlying := fixture.store
			zeroRevision := &atomic.Bool{}
			hooked := &poolReusePersistedReadHookStore{
				Store: underlying, sessionID: info.ID, workID: work.ID, zeroRevision: zeroRevision,
			}
			fixture.store = hooked
			fixture.cr.cs.mu.Lock()
			fixture.cr.cs.cityBeadStore = hooked
			fixture.cr.cs.mu.Unlock()
			baselineNudges := providerAllNudgeCalls(fixture.provider)
			if !test.afterIdle {
				zeroRevision.Store(true)
				fixture.cr.handleRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{
					WorkID: work.ID, PoolTarget: "worker", SourceStore: "city:test-city",
				})
			} else {
				idleStarted := make(chan struct{})
				idleGate := make(chan struct{})
				fixture.provider.WaitForIdleStarted[info.SessionNameMetadata] = idleStarted
				fixture.provider.WaitForIdleGates[info.SessionNameMetadata] = idleGate
				done := make(chan struct{})
				go func() {
					defer close(done)
					fixture.cr.handleRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{
						WorkID: work.ID, PoolTarget: "worker", SourceStore: "city:test-city",
					})
				}()
				awaitClose(t, idleStarted, "pool reuse idle wait before zero revision")
				zeroRevision.Store(true)
				close(idleGate)
				awaitClose(t, done, "pool reuse after zero revision")
			}

			if got := providerAllNudgeCalls(fixture.provider); got != baselineNudges {
				t.Fatalf("nudges with unavailable %s = %d, want unchanged %d", test.name, got, baselineNudges)
			}
			stored, err := underlying.Get(info.ID)
			if err != nil {
				t.Fatalf("read durable session after unavailable revision: %v", err)
			}
			if stored.Revision <= 0 {
				t.Fatalf("underlying durable revision = %d, want test seam only to hide it", stored.Revision)
			}
			assertRoutedWorkPoolAllocationCensusOwed(t, fixture.cr)
		})
	}
}

func TestRoutedWorkPoolAllocationBoundsAssignedWorkReadsToExactMemberKeys(t *testing.T) {
	for _, members := range []int{2, 5} {
		t.Run(fmt.Sprintf("members=%d", members), func(t *testing.T) {
			fixture, _, _ := prepareIdleGenericPoolMemberForReuse(t, true, members)
			for slot := 2; slot <= members; slot++ {
				work, err := fixture.store.Create(beads.Bead{Title: fmt.Sprintf("busy pool work %d", slot), Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "worker"}})
				if err != nil {
					t.Fatalf("create pool work %d: %v", slot, err)
				}
				result, err := fixture.cr.reconcileRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{WorkID: work.ID, PoolTarget: "worker", SourceStore: "city:test-city"})
				if err != nil || !result.Handled || !result.Created || result.Session.PoolSlot != fmt.Sprint(slot) {
					t.Fatalf("grow pool slot %d = (%+v, %v), want created", slot, result, err)
				}
				awaitCond(t, func() bool {
					return fixture.provider.IsRunning(result.Session.SessionName) && fixture.cr.sessionStartController.Pending() == 0
				}, fmt.Sprintf("pool slot %d to start", slot))
				info, err := sessionFrontDoor(fixture.store).Get(result.Session.ID)
				if err != nil {
					t.Fatalf("read pool slot %d: %v", slot, err)
				}
				setRoutedWorkPoolRuntimeIdentity(t, fixture, info)
				fixture.provider.WaitForIdleErrors[info.SessionNameMetadata] = nil
			}
			underlying := fixture.store
			counted := &poolReuseListCountingStore{Store: underlying}
			fixture.store = counted
			fixture.cr.cs.mu.Lock()
			fixture.cr.cs.cityBeadStore = counted
			fixture.cr.cs.mu.Unlock()
			work, err := underlying.Create(beads.Bead{Title: "work at full busy pool", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "worker"}})
			if err != nil {
				t.Fatalf("create work at full pool: %v", err)
			}

			fixture.cr.handleRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{WorkID: work.ID, PoolTarget: "worker", SourceStore: "city:test-city"})

			assertRoutedWorkPoolAllocationCensusOwed(t, fixture.cr)
			queries := counted.snapshot()
			if len(queries) != 4*members {
				t.Fatalf("assigned-work List calls for %d members = %d, want one exact query per canonical and compatibility identity for classification and revalidation: %+v", members, len(queries), queries)
			}
			for _, query := range queries {
				if query.Assignee == "" || len(query.Assignees) != 0 || query.Status != "" || !query.Live || query.TierMode != beads.TierBoth {
					t.Fatalf("assigned-work query = %+v, want one exact assignee, all live nonclosed statuses, TierBoth", query)
				}
			}
		})
	}
}

func TestRoutedWorkPoolAllocationAllBusyMultiMemberPoolKeepsExistingGrowthAndFallback(t *testing.T) {
	for _, test := range []struct {
		name          string
		maximum       int
		wantCreated   bool
		wantFallback  bool
		wantPoolCount int
	}{
		{name: "below cap grows", maximum: 3, wantCreated: true, wantPoolCount: 3},
		{name: "at cap falls back", maximum: 2, wantFallback: true, wantPoolCount: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture, _, _, _, _ := prepareTwoMemberGenericPoolForReuse(t, test.maximum)
			baselineNudges := providerAllNudgeCalls(fixture.provider)
			thirdWork, err := fixture.store.Create(beads.Bead{Title: "third routed work", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "worker"}})
			if err != nil {
				t.Fatalf("create third routed work: %v", err)
			}

			if test.wantFallback {
				fixture.cr.handleRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{WorkID: thirdWork.ID, PoolTarget: "worker", SourceStore: "city:test-city"})
				assertRoutedWorkPoolAllocationCensusOwed(t, fixture.cr)
				if got := providerCallCount(fixture.provider, "Start"); got != 2 {
					t.Fatalf("global provider Start calls after all-busy cap fallback = %d, want 2", got)
				}
				if got := providerCallCount(fixture.provider, "Stop"); got != 0 {
					t.Fatalf("global provider Stop calls after all-busy cap fallback = %d, want 0", got)
				}
				if got := providerAllNudgeCalls(fixture.provider); got != baselineNudges {
					t.Fatalf("global provider nudge calls after all-busy cap fallback = %d, want unchanged %d", got, baselineNudges)
				}
			} else {
				result, err := fixture.cr.reconcileRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{WorkID: thirdWork.ID, PoolTarget: "worker", SourceStore: "city:test-city"})
				if err != nil || !result.Handled || result.Created != test.wantCreated || result.Session.PoolSlot != "3" {
					t.Fatalf("all-busy multi-member allocation = (%+v, %v), want created slot 3", result, err)
				}
			}
			infos, err := loadSessionBeadSnapshot(fixture.store)
			if err != nil {
				t.Fatalf("load pool sessions: %v", err)
			}
			if open := len(infos.OpenInfos()); open != test.wantPoolCount {
				t.Fatalf("open pool sessions after all-busy allocation = %d, want %d", open, test.wantPoolCount)
			}
		})
	}
}

func TestRoutedWorkPoolAllocationUncertainEarlierMemberDoesNotRouteAroundToLaterIdle(t *testing.T) {
	fixture, firstWork, older, secondWork, newer := prepareTwoMemberGenericPoolForReuse(t, 3)
	if err := fixture.store.Close(firstWork.ID); err != nil {
		t.Fatalf("close older trigger work: %v", err)
	}
	if err := fixture.store.Close(secondWork.ID); err != nil {
		t.Fatalf("close newer trigger work: %v", err)
	}
	if err := fixture.provider.SetMeta(older.SessionNameMetadata, "GC_INSTANCE_TOKEN", "replacement-token"); err != nil {
		t.Fatalf("replace older runtime token: %v", err)
	}
	beforeOlder, err := fixture.store.Get(older.ID)
	if err != nil {
		t.Fatalf("read uncertain older member: %v", err)
	}
	beforeNewer, err := fixture.store.Get(newer.ID)
	if err != nil {
		t.Fatalf("read idle newer member: %v", err)
	}
	baselineNudges := providerAllNudgeCalls(fixture.provider)
	thirdWork, err := fixture.store.Create(beads.Bead{Title: "third routed work", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "worker"}})
	if err != nil {
		t.Fatalf("create third routed work: %v", err)
	}

	fixture.cr.handleRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{WorkID: thirdWork.ID, PoolTarget: "worker", SourceStore: "city:test-city"})

	assertRoutedWorkPoolAllocationCensusOwed(t, fixture.cr)
	for _, before := range []beads.Bead{beforeOlder, beforeNewer} {
		after, err := fixture.store.Get(before.ID)
		if err != nil {
			t.Fatalf("read protected member %q: %v", before.ID, err)
		}
		if !reflect.DeepEqual(after, before) {
			t.Fatalf("uncertain selection changed protected member %q\n before=%+v\n  after=%+v", before.ID, before, after)
		}
	}
	if got := providerCallCount(fixture.provider, "Start"); got != 2 {
		t.Fatalf("global provider Start calls after uncertain selection = %d, want 2", got)
	}
	if got := providerCallCount(fixture.provider, "Stop"); got != 0 {
		t.Fatalf("global provider Stop calls after uncertain selection = %d, want 0", got)
	}
	if got := providerAllNudgeCalls(fixture.provider); got != baselineNudges {
		t.Fatalf("global provider nudge calls after uncertain selection = %d, want unchanged %d", got, baselineNudges)
	}
	infos, err := loadSessionBeadSnapshot(fixture.store)
	if err != nil {
		t.Fatalf("load sessions after uncertain selection: %v", err)
	}
	if open := infos.OpenInfos(); len(open) != 2 {
		t.Fatalf("open sessions after uncertain selection = %+v, want original two", open)
	}
}

func TestRoutedWorkPoolAllocationBusyGenericReuseGrowsWithoutRebinding(t *testing.T) {
	tests := []struct {
		name       string
		closePrior bool
		mutate     func(testing.TB, routedWorkPoolAllocationFixture, sessionpkg.Info)
	}{
		{name: "open prior trigger"},
		{
			name:       "assigned work",
			closePrior: true,
			mutate: func(t testing.TB, fixture routedWorkPoolAllocationFixture, info sessionpkg.Info) {
				t.Helper()
				if _, err := fixture.store.Create(beads.Bead{Title: "assigned", Type: "task", Status: "open", Assignee: info.ID}); err != nil {
					t.Fatalf("create assigned work: %v", err)
				}
			},
		},
		{
			name:       "blocked assigned work",
			closePrior: true,
			mutate: func(t testing.TB, fixture routedWorkPoolAllocationFixture, info sessionpkg.Info) {
				t.Helper()
				assigned, err := fixture.store.Create(beads.Bead{Title: "blocked assigned", Type: "task", Status: "open", Assignee: info.ID})
				if err != nil {
					t.Fatalf("create blocked assigned work: %v", err)
				}
				blocker, err := fixture.store.Create(beads.Bead{Title: "blocker", Type: "task", Status: "open"})
				if err != nil {
					t.Fatalf("create assigned-work blocker: %v", err)
				}
				if err := fixture.store.DepAdd(assigned.ID, blocker.ID, "blocks"); err != nil {
					t.Fatalf("block assigned work: %v", err)
				}
			},
		},
		{
			name:       "future-deferred assigned work",
			closePrior: true,
			mutate: func(t testing.TB, fixture routedWorkPoolAllocationFixture, info sessionpkg.Info) {
				t.Helper()
				future := time.Now().UTC().Add(time.Hour)
				if _, err := fixture.store.Create(beads.Bead{Title: "deferred assigned", Type: "task", Status: "open", Assignee: info.ID, DeferUntil: &future}); err != nil {
					t.Fatalf("create future-deferred assigned work: %v", err)
				}
			},
		},
		{
			name:       "human attachment",
			closePrior: true,
			mutate: func(_ testing.TB, fixture routedWorkPoolAllocationFixture, info sessionpkg.Info) {
				fixture.provider.SetAttached(info.SessionNameMetadata, true)
			},
		},
		{
			name:       "pending interaction",
			closePrior: true,
			mutate: func(_ testing.TB, fixture routedWorkPoolAllocationFixture, info sessionpkg.Info) {
				fixture.provider.SetPendingInteraction(info.SessionNameMetadata, &runtime.PendingInteraction{RequestID: "approval-1"})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture, firstWork, info := prepareIdleGenericPoolMemberForReuse(t, true, 2)
			if test.closePrior {
				if err := fixture.store.Close(firstWork.ID); err != nil {
					t.Fatalf("close prior routed work: %v", err)
				}
			}
			if test.mutate != nil {
				test.mutate(t, fixture, info)
			}
			before, err := fixture.store.Get(info.ID)
			if err != nil {
				t.Fatalf("read original generic member: %v", err)
			}
			secondWork, err := fixture.store.Create(beads.Bead{Title: "second generic work", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "worker"}})
			if err != nil {
				t.Fatalf("create second routed work: %v", err)
			}
			result, err := fixture.cr.reconcileRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{WorkID: secondWork.ID, PoolTarget: "worker", SourceStore: "city:test-city"})
			if err != nil || !result.Handled || !result.Created || result.Session.ID == info.ID || result.Session.PoolSlot != "2" {
				t.Fatalf("busy generic allocation = (%+v, %v), want new slot-2 member below capacity", result, err)
			}
			awaitCond(t, func() bool {
				return fixture.provider.IsRunning(result.Session.SessionName) && fixture.cr.sessionStartController.Pending() == 0
			}, "busy generic allocation to start its second member")
			after, err := fixture.store.Get(info.ID)
			if err != nil {
				t.Fatalf("read original generic member after growth: %v", err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("busy generic allocation rebound original member\n before=%+v\n  after=%+v", before, after)
			}
			infos, err := loadSessionBeadSnapshot(fixture.store)
			if err != nil {
				t.Fatalf("load grown generic pool: %v", err)
			}
			if open := infos.OpenInfos(); len(open) != 2 {
				t.Fatalf("open generic sessions after busy growth = %+v, want two", open)
			}
			if len(fixture.cr.pokeCh) != 0 {
				t.Fatalf("legacy fallback after proven-busy growth = %d pokes, want none", len(fixture.cr.pokeCh))
			}
			if got := providerCallCount(fixture.provider, "Start"); got != 2 {
				t.Fatalf("global provider Start calls after busy growth = %d, want 2", got)
			}
			if got := providerCallCount(fixture.provider, "Stop"); got != 0 {
				t.Fatalf("global provider Stop calls after busy growth = %d, want 0", got)
			}
		})
	}
}

func TestRoutedWorkPoolAllocationRefusesManualAndDependencyOnlySoleMembers(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(testing.TB, routedWorkPoolAllocationFixture, sessionpkg.Info)
	}{
		{
			name: "legacy manual row",
			mutate: func(t testing.TB, fixture routedWorkPoolAllocationFixture, info sessionpkg.Info) {
				t.Helper()
				if err := fixture.store.SetMetadata(info.ID, poolManagedMetadataKey, ""); err != nil {
					t.Fatalf("clear legacy manual pool marker: %v", err)
				}
				if err := fixture.store.SetMetadata(info.ID, "pool_slot", ""); err != nil {
					t.Fatalf("clear legacy manual pool slot: %v", err)
				}
			},
		},
		{
			name: "dependency-only row",
			mutate: func(t testing.TB, fixture routedWorkPoolAllocationFixture, info sessionpkg.Info) {
				t.Helper()
				if err := fixture.store.SetMetadata(info.ID, "dependency_only", "true"); err != nil {
					t.Fatalf("mark member dependency-only: %v", err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture, firstWork, info := prepareIdleGenericPoolMemberForReuse(t, true, 2)
			if err := fixture.store.Close(firstWork.ID); err != nil {
				t.Fatalf("close prior routed work: %v", err)
			}
			test.mutate(t, fixture, info)
			before, err := fixture.store.Get(info.ID)
			if err != nil {
				t.Fatalf("read protected member before allocation: %v", err)
			}
			baselineNudges := providerNudgeCalls(fixture.provider, info.SessionNameMetadata)
			secondWork, err := fixture.store.Create(beads.Bead{Title: "second routed work", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "worker"}})
			if err != nil {
				t.Fatalf("create second routed work: %v", err)
			}

			fixture.cr.handleRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{WorkID: secondWork.ID, PoolTarget: "worker", SourceStore: "city:test-city"})

			after, err := fixture.store.Get(info.ID)
			if err != nil {
				t.Fatalf("read protected member after allocation: %v", err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("refused reuse changed protected durable member\n before=%+v\n  after=%+v", before, after)
			}
			if got := providerCallCount(fixture.provider, "Start"); got != 1 {
				t.Fatalf("global provider Start calls after refused reuse = %d, want 1", got)
			}
			if got := providerCallCount(fixture.provider, "Stop"); got != 0 {
				t.Fatalf("global provider Stop calls after refused reuse = %d, want 0", got)
			}
			if got := providerNudgeCalls(fixture.provider, info.SessionNameMetadata); got != baselineNudges {
				t.Fatalf("protected member nudge calls = %d, want unchanged %d", got, baselineNudges)
			}
			assertRoutedWorkPoolAllocationCensusOwed(t, fixture.cr)
		})
	}
}

func TestRoutedWorkPoolAllocationRefusedGenericReuseFallsBackWithoutGrowth(t *testing.T) {
	tests := []struct {
		name        string
		conditional bool
		mutate      func(testing.TB, routedWorkPoolAllocationFixture, sessionpkg.Info)
	}{
		{name: "conditional writer unavailable"},
		{
			name:        "runtime instance token mismatch",
			conditional: true,
			mutate: func(t testing.TB, fixture routedWorkPoolAllocationFixture, info sessionpkg.Info) {
				t.Helper()
				if err := fixture.provider.SetMeta(info.SessionNameMetadata, "GC_INSTANCE_TOKEN", "replacement-token"); err != nil {
					t.Fatalf("replace runtime token: %v", err)
				}
			},
		},
		{
			name:        "identity uncertainty overrides attachment",
			conditional: true,
			mutate: func(t testing.TB, fixture routedWorkPoolAllocationFixture, info sessionpkg.Info) {
				t.Helper()
				fixture.provider.SetAttached(info.SessionNameMetadata, true)
				if err := fixture.provider.SetMeta(info.SessionNameMetadata, "GC_INSTANCE_TOKEN", "replacement-token"); err != nil {
					t.Fatalf("replace attached runtime token: %v", err)
				}
			},
		},
		{
			name:        "uncertified membership",
			conditional: true,
			mutate: func(_ testing.TB, fixture routedWorkPoolAllocationFixture, _ sessionpkg.Info) {
				fixture.cr.poolMembershipShadow.invalidate(poolMembershipUncertifiedSnapshotGap)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture, firstWork, info := prepareIdleGenericPoolMemberForReuse(t, test.conditional, 2)
			if err := fixture.store.Close(firstWork.ID); err != nil {
				t.Fatalf("close prior routed work: %v", err)
			}
			if test.mutate != nil {
				test.mutate(t, fixture, info)
			}
			secondWork, err := fixture.store.Create(beads.Bead{Title: "second generic work", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "worker"}})
			if err != nil {
				t.Fatalf("create second routed work: %v", err)
			}
			before, err := fixture.store.Get(info.ID)
			if err != nil {
				t.Fatalf("read generic member before refusal: %v", err)
			}
			baselineNudges := providerNudgeCalls(fixture.provider, info.SessionNameMetadata)

			fixture.cr.handleRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{WorkID: secondWork.ID, PoolTarget: "worker", SourceStore: "city:test-city"})

			after, err := fixture.store.Get(info.ID)
			if err != nil {
				t.Fatalf("read generic member after refusal: %v", err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("refused generic reuse changed durable member\n before=%+v\n  after=%+v", before, after)
			}
			infos, err := loadSessionBeadSnapshot(fixture.store)
			if err != nil {
				t.Fatalf("load generic sessions after refusal: %v", err)
			}
			if open := infos.OpenInfos(); len(open) != 1 || open[0].ID != info.ID {
				t.Fatalf("refused generic reuse open sessions = %+v, want only %q", open, info.ID)
			}
			if got := fixture.provider.CountCalls("Start", info.SessionNameMetadata); got != 1 {
				t.Fatalf("refused generic reuse Start calls = %d, want 1", got)
			}
			if got := providerNudgeCalls(fixture.provider, info.SessionNameMetadata); got != baselineNudges {
				t.Fatalf("refused generic reuse nudge calls = %d, want %d", got, baselineNudges)
			}
			assertRoutedWorkPoolAllocationCensusOwed(t, fixture.cr)
		})
	}
}

func TestRoutedWorkPoolAllocationReuseUncertaintyRespectsRolloutMode(t *testing.T) {
	for _, failure := range []struct {
		name      string
		wantBound bool
		// wantErr marks the refusals that surface an error. Only those carry an
		// auto-mode diagnostic: a clean non-handled refusal is silent and simply
		// waits for the next patrol census.
		wantErr bool
	}{
		{name: "authorization refusal"},
		{name: "assignment read error", wantErr: true},
		{name: "unsupported fenced provider", wantBound: true, wantErr: true},
		{name: "post-idle authorization drift", wantBound: true, wantErr: true},
	} {
		for _, mode := range []rollout.Mode{rollout.Auto, rollout.Require} {
			t.Run(failure.name+"/"+string(mode), func(t *testing.T) {
				fixture, firstWork, info := prepareIdleGenericPoolMemberForReuse(t, true, 2)
				fixture.cr.sessionStartMode = mode
				if err := fixture.store.Close(firstWork.ID); err != nil {
					t.Fatalf("close previous trigger work: %v", err)
				}
				work, err := fixture.store.Create(beads.Bead{
					Title: "work under reuse uncertainty", Type: "task", Status: "open",
					Metadata: map[string]string{"gc.routed_to": "worker"},
				})
				if err != nil {
					t.Fatalf("create routed work: %v", err)
				}
				underlying := fixture.store
				baselineNudges := providerAllNudgeCalls(fixture.provider)
				baselineStarts := providerCallCount(fixture.provider, "Start")

				switch failure.name {
				case "authorization refusal":
					if err := fixture.provider.SetMeta(info.SessionNameMetadata, "GC_INSTANCE_TOKEN", "replacement-token"); err != nil {
						t.Fatalf("replace runtime token: %v", err)
					}
				case "assignment read error":
					hooked := &poolReuseAssignedListErrorStore{Store: underlying, err: errors.New("assignment store unavailable")}
					fixture.store = hooked
					fixture.cr.cs.mu.Lock()
					fixture.cr.cs.cityBeadStore = hooked
					fixture.cr.cs.mu.Unlock()
				case "unsupported fenced provider":
					provider := &poolReuseNoFencedProvider{Provider: fixture.cr.cs.sp, fake: fixture.provider}
					fixture.cr.sp = provider
					fixture.cr.cs.mu.Lock()
					fixture.cr.cs.sp = provider
					fixture.cr.cs.mu.Unlock()
				case "post-idle authorization drift":
					idleStarted := make(chan struct{})
					idleGate := make(chan struct{})
					fixture.provider.WaitForIdleStarted[info.SessionNameMetadata] = idleStarted
					fixture.provider.WaitForIdleGates[info.SessionNameMetadata] = idleGate
					done := make(chan struct{})
					go func() {
						defer close(done)
						fixture.cr.handleRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{
							WorkID: work.ID, PoolTarget: "worker", SourceStore: "city:test-city",
						})
					}()
					awaitClose(t, idleStarted, "pool reuse idle wait before authorization drift")
					if err := underlying.SetMetadata(info.ID, "state_reason", "post-idle-authorization-drift"); err != nil {
						t.Fatalf("advance rebound row during idle wait: %v", err)
					}
					close(idleGate)
					awaitClose(t, done, "pool reuse after authorization drift")
				}
				if failure.name != "post-idle authorization drift" {
					fixture.cr.handleRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{
						WorkID: work.ID, PoolTarget: "worker", SourceStore: "city:test-city",
					})
				}

				stored, err := underlying.Get(info.ID)
				if err != nil {
					t.Fatalf("read session after reuse uncertainty: %v", err)
				}
				if got := stored.Metadata[beadmeta.TriggerBeadIDMetadataKey]; (got == work.ID) != failure.wantBound {
					t.Fatalf("trigger after %s = %q, want rebound=%t", failure.name, got, failure.wantBound)
				}
				if got := providerAllNudgeCalls(fixture.provider); got != baselineNudges {
					t.Fatalf("nudges after %s = %d, want unchanged %d", failure.name, got, baselineNudges)
				}
				if got := providerCallCount(fixture.provider, "Start"); got != baselineStarts {
					t.Fatalf("starts after %s = %d, want unchanged %d", failure.name, got, baselineStarts)
				}
				if got := providerCallCount(fixture.provider, "Stop"); got != 0 {
					t.Fatalf("stops after %s = %d, want 0", failure.name, got)
				}
				if mode == rollout.Auto {
					assertRoutedWorkPoolAllocationCensusOwed(t, fixture.cr)
					if failure.wantErr && !strings.Contains(fixture.stderr.String(), "re-detection owed to the next patrol census") {
						t.Fatalf("auto diagnostic = %q, want a visible census-owed re-detection", fixture.stderr.String())
					}
				} else {
					if len(fixture.cr.pokeCh) != 0 {
						t.Fatalf("require fallback after %s = %d pokes, want parked", failure.name, len(fixture.cr.pokeCh))
					}
					if fixture.cr.sessionStartOwnershipState() != sessionStartOwnershipKeyed ||
						!strings.Contains(fixture.stderr.String(), "parked in required keyed reconciliation") {
						t.Fatalf("require disposition after %s = (ownership=%v, stderr=%q), want visible keyed park", failure.name, fixture.cr.sessionStartOwnershipState(), fixture.stderr.String())
					}
					if failure.wantBound {
						fixture.cr.handleRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{
							WorkID: work.ID, PoolTarget: "worker", SourceStore: "city:test-city",
						})
						if got := providerAllNudgeCalls(fixture.provider); got != baselineNudges {
							t.Fatalf("parked binding replay nudges = %d, want unchanged %d", got, baselineNudges)
						}
						if len(fixture.cr.pokeCh) != 0 {
							t.Fatal("parked binding replay escaped to legacy")
						}
					}
				}
			})
		}
	}
}

func TestRoutedWorkPoolAllocationStaleGenericReuseRevisionFallsBackWithoutGrowth(t *testing.T) {
	fixture, firstWork, info := prepareIdleGenericPoolMemberForReuse(t, true, 2)
	if err := fixture.store.Close(firstWork.ID); err != nil {
		t.Fatalf("close prior routed work: %v", err)
	}
	secondWork, err := fixture.store.Create(beads.Bead{
		Title:    "second generic work",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create second generic work: %v", err)
	}
	underlying := fixture.store
	hooked := &triggerMatchingReadHookStore{
		Store:     underlying,
		sessionID: info.ID,
		workID:    firstWork.ID,
		after: func() {
			if err := underlying.SetMetadata(info.ID, "test_revision_race", "1"); err != nil {
				t.Fatalf("advance reusable member revision: %v", err)
			}
		},
	}
	fixture.store = hooked
	fixture.cr.cs.mu.Lock()
	fixture.cr.cs.cityBeadStore = hooked
	fixture.cr.cs.mu.Unlock()
	baselineNudges := providerNudgeCalls(fixture.provider, info.SessionNameMetadata)

	fixture.cr.handleRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{
		WorkID: secondWork.ID, PoolTarget: "worker", SourceStore: "city:test-city",
	})

	after, err := underlying.Get(info.ID)
	if err != nil {
		t.Fatalf("read generic member after revision race: %v", err)
	}
	if after.Metadata[beadmeta.TriggerBeadIDMetadataKey] != firstWork.ID ||
		after.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey] != "city:test-city" {
		t.Fatalf("stale generic reuse changed trigger binding: %#v", after.Metadata)
	}
	infos, err := loadSessionBeadSnapshot(underlying)
	if err != nil {
		t.Fatalf("load generic sessions after revision race: %v", err)
	}
	if open := infos.OpenInfos(); len(open) != 1 || open[0].ID != info.ID {
		t.Fatalf("stale generic reuse open sessions = %+v, want only %q", open, info.ID)
	}
	if got := providerNudgeCalls(fixture.provider, info.SessionNameMetadata); got != baselineNudges {
		t.Fatalf("stale generic reuse nudge calls = %d, want unchanged %d", got, baselineNudges)
	}
	assertRoutedWorkPoolAllocationCensusOwed(t, fixture.cr)
}

func TestRoutedWorkPoolSingletonReuseLeavesAmbiguousStatesLegacyOwned(t *testing.T) {
	tests := []struct {
		name             string
		conditionalStore bool
		keepPreviousOpen bool
		mutate           func(testing.TB, routedWorkPoolAllocationFixture, sessionpkg.Info, beads.Bead)
	}{
		{
			name:             "human attached",
			conditionalStore: true,
			mutate: func(_ testing.TB, fixture routedWorkPoolAllocationFixture, info sessionpkg.Info, _ beads.Bead) {
				fixture.provider.SetAttached(info.SessionNameMetadata, true)
			},
		},
		{
			name:             "pending interaction",
			conditionalStore: true,
			mutate: func(_ testing.TB, fixture routedWorkPoolAllocationFixture, info sessionpkg.Info, _ beads.Bead) {
				fixture.provider.SetPendingInteraction(info.SessionNameMetadata, &runtime.PendingInteraction{RequestID: "approval-1"})
			},
		},
		{
			name:             "actionable assigned work",
			conditionalStore: true,
			mutate: func(t testing.TB, fixture routedWorkPoolAllocationFixture, info sessionpkg.Info, _ beads.Bead) {
				t.Helper()
				if _, err := fixture.store.Create(beads.Bead{Title: "already assigned", Type: "task", Status: "open", Assignee: info.ID}); err != nil {
					t.Fatalf("create assigned work: %v", err)
				}
			},
		},
		{
			name:             "previous trigger still open",
			conditionalStore: true,
			keepPreviousOpen: true,
		},
		{
			name:             "membership uncertified",
			conditionalStore: true,
			mutate: func(_ testing.TB, fixture routedWorkPoolAllocationFixture, _ sessionpkg.Info, _ beads.Bead) {
				fixture.cr.poolMembershipShadow.invalidate(poolMembershipUncertifiedSnapshotGap)
			},
		},
		{
			name:             "membership no longer sole",
			conditionalStore: true,
			mutate: func(t testing.TB, fixture routedWorkPoolAllocationFixture, info sessionpkg.Info, _ beads.Bead) {
				t.Helper()
				duplicate := info
				duplicate.ID = info.ID + "-duplicate"
				duplicate.InstanceToken = info.InstanceToken + "-duplicate"
				duplicate.SessionName = info.SessionName + "-duplicate"
				duplicate.SessionNameMetadata = duplicate.SessionName
				if err := fixture.cr.poolMembershipShadow.replace(fixture.cr.cfg, duplicate); err != nil {
					t.Fatalf("publish duplicate membership: %v", err)
				}
			},
		},
		{
			name:             "runtime instance token drift",
			conditionalStore: true,
			mutate: func(t testing.TB, fixture routedWorkPoolAllocationFixture, info sessionpkg.Info, _ beads.Bead) {
				t.Helper()
				if err := fixture.provider.SetMeta(info.SessionNameMetadata, "GC_INSTANCE_TOKEN", "replacement-token"); err != nil {
					t.Fatalf("replace runtime instance token: %v", err)
				}
			},
		},
		{
			name:             "new work route changed",
			conditionalStore: true,
			mutate: func(t testing.TB, fixture routedWorkPoolAllocationFixture, _ sessionpkg.Info, work beads.Bead) {
				t.Helper()
				if err := fixture.store.SetMetadata(work.ID, "gc.routed_to", "other"); err != nil {
					t.Fatalf("change new work route: %v", err)
				}
			},
		},
		{
			name: "conditional writes unavailable",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture, firstWork, info := prepareIdleCanonicalSingletonForReuse(t, test.conditionalStore)
			if !test.keepPreviousOpen {
				if err := fixture.store.Close(firstWork.ID); err != nil {
					t.Fatalf("close previous trigger work: %v", err)
				}
			}
			secondWork, err := fixture.store.Create(beads.Bead{
				Title:    "second singleton work",
				Type:     "task",
				Status:   "open",
				Metadata: map[string]string{"gc.routed_to": "worker"},
			})
			if err != nil {
				t.Fatalf("create second singleton work: %v", err)
			}
			if test.mutate != nil {
				test.mutate(t, fixture, info, secondWork)
			}
			before, err := fixture.store.Get(info.ID)
			if err != nil {
				t.Fatalf("read singleton before refused reuse: %v", err)
			}
			baselineNudges := providerNudgeCalls(fixture.provider, info.SessionNameMetadata)

			fixture.cr.handleRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{
				WorkID: secondWork.ID, PoolTarget: "worker", SourceStore: "city:test-city",
			})

			after, err := fixture.store.Get(info.ID)
			if err != nil {
				t.Fatalf("read singleton after refused reuse: %v", err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("refused reuse changed durable session\n before=%+v\n  after=%+v", before, after)
			}
			if got := providerNudgeCalls(fixture.provider, info.SessionNameMetadata); got != baselineNudges {
				t.Fatalf("refused reuse nudge calls = %d, want unchanged %d", got, baselineNudges)
			}
			if got := fixture.provider.CountCalls("Start", info.SessionNameMetadata); got != 1 {
				t.Fatalf("refused reuse Start calls = %d, want 1", got)
			}
			if got := fixture.provider.CountCalls("Stop", info.SessionNameMetadata); got != 0 {
				t.Fatalf("refused reuse Stop calls = %d, want 0", got)
			}
			assertRoutedWorkPoolAllocationCensusOwed(t, fixture.cr)
		})
	}
}

func TestRoutedWorkPoolSingletonReuseFallsBackAfterUnconfirmedIdleDelivery(t *testing.T) {
	fixture, firstWork, info := prepareIdleCanonicalSingletonForReuse(t, true)
	if err := fixture.store.Close(firstWork.ID); err != nil {
		t.Fatalf("close previous trigger work: %v", err)
	}
	fixture.provider.WaitForIdleErrors[info.SessionNameMetadata] = errors.New("not idle")
	secondWork, err := fixture.store.Create(beads.Bead{
		Title:    "second singleton work",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create second singleton work: %v", err)
	}
	baselineNudges := providerNudgeCalls(fixture.provider, info.SessionNameMetadata)

	fixture.cr.handleRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{
		WorkID: secondWork.ID, PoolTarget: "worker", SourceStore: "city:test-city",
	})

	stored, err := fixture.store.Get(info.ID)
	if err != nil {
		t.Fatalf("read rebound singleton: %v", err)
	}
	if stored.Metadata[beadmeta.TriggerBeadIDMetadataKey] != secondWork.ID {
		t.Fatalf("rebound trigger = %q, want durable %q despite unconfirmed delivery", stored.Metadata[beadmeta.TriggerBeadIDMetadataKey], secondWork.ID)
	}
	if got := providerNudgeCalls(fixture.provider, info.SessionNameMetadata); got != baselineNudges {
		t.Fatalf("unconfirmed delivery nudge calls = %d, want unchanged %d", got, baselineNudges)
	}
	assertRoutedWorkPoolAllocationCensusOwed(t, fixture.cr)
}

func TestRoutedWorkPoolGenericReuseDoesNotStartAfterCommittedBindingLosesAuthorization(t *testing.T) {
	fixture, firstWork, info := prepareIdleGenericPoolMemberForReuse(t, true, 2)
	if err := fixture.store.Close(firstWork.ID); err != nil {
		t.Fatalf("close previous trigger work: %v", err)
	}
	secondWork, err := fixture.store.Create(beads.Bead{
		Title:    "second generic work",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create second generic work: %v", err)
	}
	underlying := fixture.store
	hooked := &triggerMatchingReadHookStore{
		Store:     underlying,
		sessionID: info.ID,
		workID:    secondWork.ID,
		after: func() {
			if err := underlying.Close(info.ID); err != nil {
				t.Fatalf("retire singleton after committed rebind: %v", err)
			}
			fixture.cr.poolMembershipShadow.remove(info.ID)
		},
	}
	fixture.store = hooked
	fixture.cr.cs.mu.Lock()
	fixture.cr.cs.cityBeadStore = hooked
	fixture.cr.cs.mu.Unlock()
	baselineNudges := providerNudgeCalls(fixture.provider, info.SessionNameMetadata)

	fixture.cr.handleRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{
		WorkID: secondWork.ID, PoolTarget: "worker", SourceStore: "city:test-city",
	})

	snapshot, err := loadSessionBeadSnapshot(fixture.store)
	if err != nil {
		t.Fatalf("load sessions after lost reuse authorization: %v", err)
	}
	if open := snapshot.OpenInfos(); len(open) != 0 {
		t.Fatalf("open sessions after lost reuse authorization = %+v, want no replacement after %q retired", open, info.ID)
	}
	if got := providerNudgeCalls(fixture.provider, info.SessionNameMetadata); got != baselineNudges {
		t.Fatalf("nudges after lost reuse authorization = %d, want unchanged %d", got, baselineNudges)
	}
	assertRoutedWorkPoolAllocationCensusOwed(t, fixture.cr)
}

func TestRoutedWorkPoolAllocationCanonicalSingletonRetiresByExactDrainAck(t *testing.T) {
	opened, err := beads.OpenStoreAtForCity(t.Context(), beads.StoreOpenOptions{
		Provider:          "file",
		OpenFileStore:     func() (beads.Store, error) { return beads.NewAtomicCloseMemStore(), nil },
		ConditionalWrites: gate.Auto,
	})
	if err != nil {
		t.Fatalf("open conditional-write singleton store: %v", err)
	}
	fixture := newRoutedWorkPoolAllocationFixture(t, opened.Store)
	maximum := 1
	fixture.cr.cfg.Agents[0].MaxActiveSessions = &maximum
	work, err := fixture.store.Create(beads.Bead{
		Title:    "singleton lifecycle work",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create singleton lifecycle work: %v", err)
	}
	hint := routedWorkPoolAllocationHint{WorkID: work.ID, PoolTarget: "worker", SourceStore: "city:test-city"}
	allocated, err := fixture.cr.reconcileRoutedWorkPoolAllocation(t.Context(), hint)
	if err != nil || !allocated.Handled || !allocated.Created {
		t.Fatalf("allocate canonical singleton = (%+v, %v), want one keyed create", allocated, err)
	}
	awaitCond(t, func() bool {
		return fixture.provider.IsRunning(allocated.Session.SessionName) && fixture.cr.sessionStartController.Pending() == 0
	}, "canonical singleton to start before drain acknowledgement")
	if err := fixture.store.Close(work.ID); err != nil {
		t.Fatalf("close singleton trigger work: %v", err)
	}
	info, err := sessionFrontDoor(fixture.store).Get(allocated.Session.ID)
	if err != nil {
		t.Fatalf("read active singleton session: %v", err)
	}
	for key, value := range map[string]string{
		"GC_SESSION_ID":                   info.ID,
		"GC_INSTANCE_TOKEN":               info.InstanceToken,
		reconcilerDrainAckSourceKey:       drainAckSourceAgentValue,
		drainAckRequesterSessionIDKey:     info.ID,
		drainAckRequesterInstanceTokenKey: info.InstanceToken,
		"GC_DRAIN_ACK":                    "1",
	} {
		if err := fixture.provider.SetMeta(info.SessionName, key, value); err != nil {
			t.Fatalf("set singleton runtime metadata %s: %v", key, err)
		}
	}

	snapshot, release, err := fixture.cr.cs.acquireSessionStartSnapshot()
	if err != nil {
		t.Fatalf("acquire singleton stop snapshot: %v", err)
	}
	lease, agentAck, _, leaseErr := fixture.cr.newRoutedWorkPoolDrainAckLease(snapshot, info)
	if leaseErr != nil || !agentAck {
		release()
		t.Fatalf("create singleton drain-ack lease = (%+v, %t, %v), want exact agent lease", lease, agentAck, leaseErr)
	}
	authorized, _, authorizeErr := fixture.cr.authorizeRoutedWorkPoolDrainAck(snapshot, info, lease)
	release()
	if authorizeErr != nil || !authorized {
		t.Fatalf("authorize canonical singleton drain acknowledgement = (%t, %v), want true", authorized, authorizeErr)
	}
	if reply := fixture.cr.admitSessionStartSocketKey(info.ID); reply != sessionStartSocketReplyOK {
		t.Fatalf("singleton drain-ack socket reply = %q, want %q", reply, sessionStartSocketReplyOK)
	}
	awaitCond(t, func() bool {
		row, getErr := fixture.store.Get(info.ID)
		return getErr == nil && row.Status == "closed" && row.Metadata["state"] == string(sessionpkg.StateDrained) &&
			row.Metadata["state_reason"] == "" && row.Metadata["close_reason"] == sessionpkg.CanonicalCloseReason("drained") &&
			row.Metadata["closed_at"] != "" && !fixture.provider.IsRunning(info.SessionName)
	}, "canonical singleton exact durable retirement")
	if got := fixture.provider.CountCalls("Stop", info.SessionName); got != 1 {
		t.Fatalf("canonical singleton provider Stop calls = %d, want 1", got)
	}
	if len(fixture.cr.pokeCh) != 0 {
		t.Fatalf("legacy fallback after canonical singleton retirement = %d pokes, want none", len(fixture.cr.pokeCh))
	}
}

func TestRoutedWorkPoolAllocationRediscoverPendingBindingFromStaleEmptyIndex(t *testing.T) {
	fixture := newRoutedWorkPoolAllocationFixture(t, beads.NewMemStore())
	work, err := fixture.store.Create(beads.Bead{
		Title:    "ready routed work with preexisting pending binding",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create routed work: %v", err)
	}
	hint := routedWorkPoolAllocationHint{WorkID: work.ID, PoolTarget: "worker", SourceStore: "city:test-city"}
	pending := createRoutedWorkPoolBinding(t, fixture.store, fixture.cr.cfg, hint, 1)

	result, err := fixture.cr.reconcileRoutedWorkPoolAllocation(t.Context(), hint)
	if err != nil {
		t.Fatalf("reconcile stale-index pending binding: %v", err)
	}
	if !result.Handled || result.Created || result.Session.ID != pending.ID {
		t.Fatalf("stale-index allocation = %+v, want rediscovered pending binding %q without create", result, pending.ID)
	}
	awaitCond(t, func() bool {
		return fixture.provider.IsRunning(pending.SessionName) && fixture.cr.sessionStartController.Pending() == 0
	}, "rediscovered pending binding to start through its exact lease")
	infos, err := loadSessionBeadSnapshot(fixture.store)
	if err != nil {
		t.Fatalf("load sessions after stale-index recovery: %v", err)
	}
	if open := infos.OpenInfos(); len(open) != 1 || open[0].ID != pending.ID {
		t.Fatalf("open sessions after stale-index recovery = %+v, want only %q", open, pending.ID)
	}
}

func TestRoutedWorkPoolAllocationFailsClosedOnAmbiguousBinding(t *testing.T) {
	fixture := newRoutedWorkPoolAllocationFixture(t, beads.NewMemStore())
	work, err := fixture.store.Create(beads.Bead{
		Title:    "ready routed work with ambiguous bindings",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create routed work: %v", err)
	}
	hint := routedWorkPoolAllocationHint{WorkID: work.ID, PoolTarget: "worker", SourceStore: "city:test-city"}
	first := createRoutedWorkPoolBinding(t, fixture.store, fixture.cr.cfg, hint, 1)
	second := createRoutedWorkPoolBinding(t, fixture.store, fixture.cr.cfg, hint, 2)

	if got, found, err := findRoutedWorkPoolSession(fixture.store, fixture.cr.cfg, hint); err == nil || found || got.ID != "" {
		t.Fatalf("find ambiguous routed-work bindings = (%+v, %t, %v), want ambiguity error", got, found, err)
	}
	fixture.cr.handleRoutedWorkPoolAllocation(t.Context(), hint)
	assertRoutedWorkPoolAllocationCensusOwed(t, fixture.cr)
	if got := fixture.provider.CountCalls("Start", first.SessionName) + fixture.provider.CountCalls("Start", second.SessionName); got != 0 {
		t.Fatalf("provider starts for ambiguous bindings = %d, want 0", got)
	}
	infos, err := loadSessionBeadSnapshot(fixture.store)
	if err != nil {
		t.Fatalf("load ambiguous bindings: %v", err)
	}
	if open := infos.OpenInfos(); len(open) != 2 {
		t.Fatalf("open sessions after ambiguous binding = %d, want 2", len(open))
	}
}

func TestRoutedWorkPoolAllocationLeavesUnsafeExistingBindingsLegacyOwned(t *testing.T) {
	newActiveBinding := func(t *testing.T) (routedWorkPoolAllocationFixture, routedWorkPoolAllocationHint, sessionpkg.Info) {
		t.Helper()
		fixture := newRoutedWorkPoolAllocationFixture(t, beads.NewMemStore())
		work, err := fixture.store.Create(beads.Bead{
			Title:    "ready routed work",
			Type:     "task",
			Status:   "open",
			Metadata: map[string]string{"gc.routed_to": "worker"},
		})
		if err != nil {
			t.Fatalf("create routed work: %v", err)
		}
		hint := routedWorkPoolAllocationHint{WorkID: work.ID, PoolTarget: "worker", SourceStore: "city:test-city"}
		result, err := fixture.cr.reconcileRoutedWorkPoolAllocation(t.Context(), hint)
		if err != nil || !result.Created {
			t.Fatalf("create active binding = (%+v, %v), want created", result, err)
		}
		awaitCond(t, func() bool {
			return fixture.provider.IsRunning(result.Session.SessionName) && fixture.cr.sessionStartController.Pending() == 0
		}, "initial keyed pool start to settle")
		return fixture, hint, result.Session
	}

	t.Run("unsupported policy", func(t *testing.T) {
		fixture, hint, _ := newActiveBinding(t)
		fixture.cr.cfg.Agents[0].DependsOn = []string{"another-template"}
		fixture.cr.handleRoutedWorkPoolAllocation(t.Context(), hint)
		assertRoutedWorkPoolAllocationCensusOwed(t, fixture.cr)
	})

	t.Run("asleep existing binding", func(t *testing.T) {
		fixture, hint, session := newActiveBinding(t)
		if err := fixture.store.SetMetadata(session.ID, "state", string(sessionpkg.StateAsleep)); err != nil {
			t.Fatalf("mark existing binding asleep: %v", err)
		}
		fixture.cr.handleRoutedWorkPoolAllocation(t.Context(), hint)
		assertRoutedWorkPoolAllocationCensusOwed(t, fixture.cr)
	})
}

func TestRoutedWorkPoolAllocationEventCoalescingDoesNotActivateLegacyFallback(t *testing.T) {
	genericEntered := make(chan struct{})
	genericSuperseded := make(chan struct{}, 1)
	releaseGeneric := make(chan struct{})
	var enterOnce sync.Once
	var releaseOnce sync.Once

	originalNewController := newCitySessionStartController
	newCitySessionStartController = func(opts sessionStartControllerOptions) (*sessionStartController, error) {
		reconcile := opts.Reconcile
		observe := opts.Observer
		opts.Reconcile = func(ctx context.Context, admission sessionStartAdmission) error {
			if admission.PoolAllocation == nil {
				enterOnce.Do(func() { close(genericEntered) })
				select {
				case <-releaseGeneric:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return reconcile(ctx, admission)
		}
		opts.Observer = func(result sessionStartReconcileResult) {
			if result.Admission.PoolAllocation == nil && result.Outcome == sessionStartReconcileSuperseded {
				select {
				case genericSuperseded <- struct{}{}:
				default:
				}
			}
			if observe != nil {
				observe(result)
			}
		}
		return originalNewController(opts)
	}
	t.Cleanup(func() { newCitySessionStartController = originalNewController })

	var controllerState *controllerState
	store := beads.NewCachingStoreForTest(beads.NewMemStore(), func(eventType, beadID string, payload json.RawMessage) {
		if controllerState == nil {
			return
		}
		controllerState.admitSessionStartEvent(events.Event{
			Type:    eventType,
			Subject: beadID,
			Payload: payload,
		})
		if eventType == events.BeadCreated {
			var bead beads.Bead
			if err := json.Unmarshal(payload, &bead); err == nil && bead.Type == sessionpkg.BeadType {
				awaitClose(t, genericEntered, "generic session-create event reconciliation")
			}
		}
	})
	fixture := newRoutedWorkPoolAllocationFixture(t, store)
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseGeneric) }) })
	controllerState = fixture.cr.cs

	work, err := fixture.store.Create(beads.Bead{
		Title:    "ready routed work with emitted session events",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create ready work: %v", err)
	}
	hint := routedWorkPoolAllocationHint{
		WorkID:      work.ID,
		PoolTarget:  "worker",
		SourceStore: "city:test-city",
		EventAt:     time.Now().UTC().Add(-time.Second),
		EnqueuedAt:  time.Now().UTC(),
	}

	fixture.cr.handleRoutedWorkPoolAllocation(t.Context(), hint)
	awaitCond(t, func() bool {
		controller := fixture.cr.sessionStartController
		controller.mu.Lock()
		defer controller.mu.Unlock()
		for _, admission := range controller.admissions {
			if admission.PoolAllocation != nil {
				return true
			}
		}
		return false
	}, "pool-allocation lease to supersede generic create event")
	releaseOnce.Do(func() { close(releaseGeneric) })
	awaitClose(t, genericSuperseded, "generic create event to resolve as superseded")
	awaitCond(t, func() bool {
		return fixture.cr.sessionStartController.Pending() == 0
	}, "leased exact start and emitted update events to settle")

	infos, err := loadSessionBeadSnapshot(fixture.store)
	if err != nil {
		t.Fatalf("load materialized session: %v", err)
	}
	open := infos.OpenInfos()
	if len(open) != 1 {
		t.Fatalf("open sessions = %d, want exactly 1", len(open))
	}
	if !fixture.provider.IsRunning(open[0].SessionName) {
		t.Fatalf("materialized session %q is not running", open[0].SessionName)
	}
	if got := fixture.provider.CountCalls("Start", open[0].SessionName); got != 1 {
		t.Fatalf("provider Start calls for %q = %d, want exactly 1", open[0].SessionName, got)
	}
	if len(fixture.cr.pokeCh) != 0 {
		t.Fatalf("legacy fallback after allocator-owned events = %d pokes, want none", len(fixture.cr.pokeCh))
	}
}

func TestRoutedWorkPoolAllocationLeaseExcludesLegacyStartWhileKeyedStartIsInFlight(t *testing.T) {
	fixture := newRoutedWorkPoolAuthorizationFixture(t)
	opened, err := beads.OpenStoreAtForCity(t.Context(), beads.StoreOpenOptions{
		Provider:          "file",
		OpenFileStore:     func() (beads.Store, error) { return fixture.store, nil },
		ConditionalWrites: gate.Auto,
	})
	if err != nil {
		t.Fatalf("enable conditional writes on collision fixture: %v", err)
	}
	if opened.Store != fixture.store {
		t.Fatalf("conditional-writes fixture store = %T, want original %T", opened.Store, fixture.store)
	}
	commitEntered := make(chan struct{})
	releaseCommit := make(chan struct{})
	var releaseOnce sync.Once
	barrierStore := &poolAllocationStartCommitBarrierStore{
		Store:       fixture.snapshot.Store,
		provider:    fixture.provider,
		sessionID:   fixture.info.ID,
		sessionName: fixture.info.SessionName,
		entered:     commitEntered,
		release:     releaseCommit,
	}

	controller, err := newSessionStartController(sessionStartControllerOptions{
		Workers:     1,
		MaxDistinct: 4,
		MaxRetries:  0,
		Reconcile: func(ctx context.Context, admission sessionStartAdmission) error {
			owner, reconcileErr := reconcileExactSessionStartWithOwner(ctx, admission, exactSessionStartParams{
				Generation:   fixture.snapshot.Generation,
				CityPath:     fixture.snapshot.CityPath,
				CityName:     fixture.snapshot.CityName,
				Config:       fixture.snapshot.Config,
				Provider:     fixture.snapshot.Provider,
				Store:        barrierStore,
				Recorder:     events.Discard,
				Stdout:       io.Discard,
				Stderr:       io.Discard,
				StartOptions: []startExecutionOption{withStartStabilityWaiter(immediateStartStabilityWaiter)},
				AuthorizePoolStart: func(authorizeCtx context.Context, info sessionpkg.Info, lease routedWorkPoolStartLease) (bool, error) {
					return fixture.cr.authorizeRoutedWorkPoolStart(authorizeCtx, fixture.snapshot, info, lease)
				},
			})
			if reconcileErr == nil && owner == exactSessionStartLegacyOwner {
				return errSessionStartLegacyFallbackRequired
			}
			return reconcileErr
		},
	})
	if err != nil {
		t.Fatalf("create blocked exact-start controller: %v", err)
	}
	if err := controller.Start(t.Context()); err != nil {
		t.Fatalf("start blocked exact-start controller: %v", err)
	}
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseCommit) })
		controller.Stop()
	})
	fixture.cr.sessionStartMu.Lock()
	fixture.cr.sessionStartController = controller
	fixture.cr.sessionStartOwnership = sessionStartOwnershipKeyed
	fixture.cr.sessionStartMode = rollout.Auto
	fixture.cr.sessionStartMu.Unlock()

	if outcome, err := controller.AdmitPoolAllocation(fixture.lease); err != nil || outcome != sessionStartAdmissionAccepted {
		t.Fatalf("admit pool allocation = (%q, %v), want accepted", outcome, err)
	}
	awaitClose(t, commitEntered, "keyed provider start to reach the durable commit boundary")
	if !fixture.provider.IsRunning(fixture.info.SessionName) {
		t.Fatal("keyed provider is not live at the pre-commit barrier")
	}
	if got := fixture.provider.CountCalls("Start", fixture.info.SessionName); got != 1 {
		t.Fatalf("provider Start calls at the pre-commit barrier = %d, want exactly 1", got)
	}
	beforeLegacy, err := fixture.store.Get(fixture.info.ID)
	if err != nil {
		t.Fatalf("read allocator-owned row at pre-commit barrier: %v", err)
	}
	preCommitInfo, err := sessionFrontDoor(fixture.store).Get(fixture.info.ID)
	if err != nil {
		t.Fatalf("read allocator-owned session info at pre-commit barrier: %v", err)
	}
	if preCommitInfo.InstanceToken == "" || preCommitInfo.InstanceToken == fixture.lease.InstanceToken {
		t.Fatalf("pre-commit instance token = %q, want a nonempty rotation from lease token %q", preCommitInfo.InstanceToken, fixture.lease.InstanceToken)
	}
	legacyExclusion := fixture.cr.sessionStartLegacyExclusionOption()
	if legacyExclusion == nil {
		t.Fatal("keyed pool allocation did not install legacy exclusions")
	}
	legacyOptions := startExecutionOptions{}
	legacyExclusion(&legacyOptions)
	if legacyOptions.legacyStartExcluded == nil || !legacyOptions.legacyStartExcluded(preCommitInfo) {
		t.Fatal("keyed pool allocation did not exclude legacy start after pre-wake token rotation")
	}
	if legacyOptions.legacyStatusHealExcluded == nil || !legacyOptions.legacyStatusHealExcluded(preCommitInfo) {
		t.Fatal("keyed pool allocation did not exclude legacy status heal after pre-wake token rotation")
	}

	legacy := newReconcilerTestEnv()
	legacy.store = fixture.store
	legacy.sp = fixture.provider
	legacy.cfg = fixture.snapshot.Config
	legacy.clk.Time = time.Now().UTC()
	legacy.addDesired(fixture.info.SessionName, fixture.lease.PoolTarget, false)
	legacy.startOptions = append(legacy.startOptions, legacyExclusion)
	if starts := legacy.reconcile([]beads.Bead{beforeLegacy}); starts != 0 {
		t.Fatalf("legacy wake attempts while keyed lease is in flight = %d, want 0", starts)
	}
	afterLegacy, err := fixture.store.Get(fixture.info.ID)
	if err != nil {
		t.Fatalf("read allocator-owned row after legacy pass: %v", err)
	}
	if !reflect.DeepEqual(afterLegacy, beforeLegacy) {
		t.Fatalf("legacy pass mutated live keyed row before durable commit:\nbefore=%+v\nafter=%+v", beforeLegacy, afterLegacy)
	}
	if got := fixture.provider.CountCalls("Start", fixture.info.SessionName); got != 1 {
		t.Fatalf("provider Start calls before keyed commit release = %d, want exactly 1", got)
	}

	releaseOnce.Do(func() { close(releaseCommit) })
	awaitCond(t, func() bool { return controller.Pending() == 0 }, "keyed pool-allocation start to settle")
	if got := fixture.provider.CountCalls("Start", fixture.info.SessionName); got != 1 {
		t.Fatalf("provider Start calls after keyed commit = %d, want exactly 1", got)
	}
	committed, err := fixture.store.Get(fixture.info.ID)
	if err != nil {
		t.Fatalf("read allocator-owned row after keyed commit: %v", err)
	}
	if committed.Revision <= beforeLegacy.Revision || committed.Metadata["state"] != string(sessionpkg.StateActive) || committed.Metadata["pending_create_claim"] != "" {
		t.Fatalf("keyed commit row = %+v, want newer active row with cleared pending-create claim", committed)
	}
}

func TestRoutedWorkPoolAllocationExhaustionReleasesLeaseForLegacyFallback(t *testing.T) {
	originalNewController := newCitySessionStartController
	newCitySessionStartController = func(opts sessionStartControllerOptions) (*sessionStartController, error) {
		reconcile := opts.Reconcile
		opts.MaxRetries = 0
		opts.Reconcile = func(ctx context.Context, admission sessionStartAdmission) error {
			if admission.PoolAllocation != nil {
				return errors.New("authoritative store unavailable")
			}
			return reconcile(ctx, admission)
		}
		return originalNewController(opts)
	}
	t.Cleanup(func() { newCitySessionStartController = originalNewController })

	fixture := newRoutedWorkPoolAllocationFixture(t, beads.NewMemStore())
	work, err := fixture.store.Create(beads.Bead{
		Title:    "ready routed work for exhausted exact start",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create ready work: %v", err)
	}
	fixture.cr.handleRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{
		WorkID: work.ID, PoolTarget: "worker", SourceStore: "city:test-city",
	})
	awaitCond(t, func() bool {
		return fixture.cr.sessionStartController.Pending() == 0
	}, "production observer to drain the exhausted allocation")
	assertRoutedWorkPoolAllocationCensusOwed(t, fixture.cr)

	snapshot, err := loadSessionBeadSnapshot(fixture.store)
	if err != nil {
		t.Fatalf("load exhausted allocator-owned pool row: %v", err)
	}
	open := snapshot.OpenInfos()
	if len(open) != 1 {
		t.Fatalf("open sessions after keyed exhaustion = %d, want 1", len(open))
	}
	info := open[0]
	if fixture.cr.sessionStartController.ownsPoolAllocationStart(info.ID, info.InstanceToken) {
		t.Fatal("exhausted pool-allocation lease still excludes legacy start")
	}

	legacy := newReconcilerTestEnv()
	legacy.store = fixture.store
	legacy.sp = fixture.provider
	legacy.cfg = fixture.cr.cfg
	legacy.clk.Time = time.Now().UTC()
	legacy.addDesired(info.SessionName, "worker", false)
	legacy.startOptions = append(legacy.startOptions, fixture.cr.sessionStartLegacyExclusionOption())
	row, err := fixture.store.Get(info.ID)
	if err != nil {
		t.Fatalf("read exhausted allocator-owned pool row: %v", err)
	}
	if starts := legacy.reconcile([]beads.Bead{row}); starts != 1 {
		t.Fatalf("legacy fallback wake attempts after keyed exhaustion = %d, want 1", starts)
	}
	if got := fixture.provider.CountCalls("Start", info.SessionName); got != 1 {
		t.Fatalf("provider Start calls after legacy fallback = %d, want exactly 1", got)
	}
}

func TestRoutedWorkPoolAllocationFallsBackWithoutCreatingOnUncertainty(t *testing.T) {
	t.Run("work became closed", func(t *testing.T) {
		fixture := newRoutedWorkPoolAllocationFixture(t, beads.NewMemStore())
		work, err := fixture.store.Create(beads.Bead{
			Title:    "stale routed work",
			Type:     "task",
			Status:   "open",
			Metadata: map[string]string{"gc.routed_to": "worker"},
		})
		if err != nil {
			t.Fatalf("create work: %v", err)
		}
		if err := fixture.store.Close(work.ID); err != nil {
			t.Fatalf("close work: %v", err)
		}

		fixture.cr.handleRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{
			WorkID: work.ID, PoolTarget: "worker", SourceStore: "city:test-city",
		})

		assertRoutedWorkPoolAllocationCensusOwed(t, fixture.cr)
		assertNoPoolAllocationSession(t, fixture.store)
	})

	t.Run("session create fails", func(t *testing.T) {
		store := &poolAllocationFailSessionCreateStore{Store: beads.NewMemStore()}
		fixture := newRoutedWorkPoolAllocationFixture(t, store)
		work, err := fixture.store.Create(beads.Bead{
			Title:    "ready routed work",
			Type:     "task",
			Status:   "open",
			Metadata: map[string]string{"gc.routed_to": "worker"},
		})
		if err != nil {
			t.Fatalf("create work: %v", err)
		}
		store.fail.Store(true)

		fixture.cr.handleRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{
			WorkID: work.ID, PoolTarget: "worker", SourceStore: "city:test-city",
		})

		assertRoutedWorkPoolAllocationCensusOwed(t, fixture.cr)
		assertNoPoolAllocationSession(t, fixture.store)
	})

	t.Run("exact admission fails after durable create", func(t *testing.T) {
		fixture := newRoutedWorkPoolAllocationFixture(t, beads.NewMemStore())
		work, err := fixture.store.Create(beads.Bead{
			Title:    "ready routed work",
			Type:     "task",
			Status:   "open",
			Metadata: map[string]string{"gc.routed_to": "worker"},
		})
		if err != nil {
			t.Fatalf("create work: %v", err)
		}
		fixture.cr.sessionStartController.Stop()

		fixture.cr.handleRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{
			WorkID: work.ID, PoolTarget: "worker", SourceStore: "city:test-city",
		})

		assertRoutedWorkPoolAllocationCensusOwed(t, fixture.cr)
		infos, err := loadSessionBeadSnapshot(fixture.store)
		if err != nil {
			t.Fatalf("load durable session after admission failure: %v", err)
		}
		if got := len(infos.OpenInfos()); got != 1 {
			t.Fatalf("open sessions after admission failure = %d, want durable binding retained", got)
		}
	})
}

func TestAdmitRoutedWorkPoolSessionReportsQueueOverflow(t *testing.T) {
	fixture := newRoutedWorkPoolAllocationFixture(t, beads.NewMemStore())
	controller := fixture.cr.sessionStartController
	controller.mu.Lock()
	controller.maxDistinct = 0
	controller.mu.Unlock()

	err := fixture.cr.admitRoutedWorkPoolSession(routedWorkPoolStartLease{
		SessionID:            "gcs-created",
		InstanceToken:        "instance-token",
		ControllerGeneration: 1,
		PoolTarget:           "worker",
		WorkID:               "ga-work",
		SourceStore:          "city:test-city",
		MembershipRevision:   1,
	})
	if err == nil || !strings.Contains(err.Error(), "queue overflow") {
		t.Fatalf("admit saturated exact-start controller error = %v, want queue overflow", err)
	}
	if !controller.TakeAuditRequest() {
		t.Fatal("queue overflow did not request an authoritative audit")
	}
}

func TestSessionStartAdmissionPreservesPoolAllocationLeaseAcrossGenericCoalescing(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	controller, err := newSessionStartController(sessionStartControllerOptions{
		Workers:     1,
		MaxDistinct: 4,
		MaxRetries:  0,
		Reconcile: func(context.Context, sessionStartAdmission) error {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
			return nil
		},
	})
	if err != nil {
		t.Fatalf("new exact-start controller: %v", err)
	}
	if err := controller.Start(t.Context()); err != nil {
		t.Fatalf("start exact-start controller: %v", err)
	}
	t.Cleanup(controller.Stop)
	defer close(release)

	lease := routedWorkPoolStartLease{
		SessionID:            "gcs-pool1",
		InstanceToken:        "instance-token",
		ControllerGeneration: 7,
		PoolTarget:           "worker",
		WorkID:               "ga-work",
		SourceStore:          "city:test-city",
		MembershipRevision:   11,
	}
	if outcome, err := controller.AdmitPoolAllocation(lease); err != nil || outcome != sessionStartAdmissionAccepted {
		t.Fatalf("admit pool allocation = (%q, %v), want accepted", outcome, err)
	}
	awaitClose(t, entered, "pool allocation reconcile to enter")
	if outcome, err := controller.Admit(lease.SessionID, sessionStartAdmissionInProcess); err != nil || outcome != sessionStartAdmissionCoalesced {
		t.Fatalf("coalesce generic event = (%q, %v), want coalesced", outcome, err)
	}

	admission, ok := controller.readAdmission(lease.SessionID)
	if !ok || admission.PoolAllocation == nil || *admission.PoolAllocation != lease {
		t.Fatalf("coalesced admission lease = %+v, want %+v", admission.PoolAllocation, lease)
	}
	if !admission.PoolStartEntered {
		t.Fatal("generic coalescing cleared entered pool-allocation ownership")
	}
}

func TestAuthorizeRoutedWorkPoolStartRejectsStaleLeaseAuthority(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*routedWorkPoolAuthorizationFixture)
	}{
		{
			name: "instance changed",
			mutate: func(f *routedWorkPoolAuthorizationFixture) {
				f.info.InstanceToken = "replacement-token"
			},
		},
		{
			name: "controller generation changed",
			mutate: func(f *routedWorkPoolAuthorizationFixture) {
				f.lease.ControllerGeneration++
			},
		},
		{
			name: "trigger work changed",
			mutate: func(f *routedWorkPoolAuthorizationFixture) {
				f.lease.WorkID = "gc-other-work"
			},
		},
		{
			name: "source store changed",
			mutate: func(f *routedWorkPoolAuthorizationFixture) {
				f.lease.SourceStore = "city:other"
			},
		},
		{
			name: "pool target changed",
			mutate: func(f *routedWorkPoolAuthorizationFixture) {
				f.lease.PoolTarget = "other"
			},
		},
		{
			name: "membership became uncertified",
			mutate: func(f *routedWorkPoolAuthorizationFixture) {
				f.cr.poolMembershipShadow.invalidate(poolMembershipUncertifiedSnapshotGap)
			},
		},
		{
			name: "allocated member disappeared",
			mutate: func(f *routedWorkPoolAuthorizationFixture) {
				f.cr.poolMembershipShadow.remove(f.info.ID)
			},
		},
		{
			name: "membership revision not observed",
			mutate: func(f *routedWorkPoolAuthorizationFixture) {
				f.lease.MembershipRevision++
			},
		},
		{
			name: "work no longer ready",
			mutate: func(f *routedWorkPoolAuthorizationFixture) {
				if err := f.store.Close(f.work.ID); err != nil {
					f.t.Fatalf("close routed work: %v", err)
				}
			},
		},
		{
			name: "config reloaded",
			mutate: func(f *routedWorkPoolAuthorizationFixture) {
				next := *f.snapshot.Config
				f.cr.cfg = &next
			},
		},
		{
			name: "canonical singleton rejects numbered allocation",
			mutate: func(f *routedWorkPoolAuthorizationFixture) {
				maximum := 1
				f.snapshot.Config.Agents[0].MaxActiveSessions = &maximum
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRoutedWorkPoolAuthorizationFixture(t)
			test.mutate(&fixture)

			authorized, err := fixture.cr.authorizeRoutedWorkPoolStart(t.Context(), fixture.snapshot, fixture.info, fixture.lease)
			if err != nil {
				t.Fatalf("authorize stale lease: %v", err)
			}
			if authorized {
				t.Fatal("stale pool-allocation lease retained start authority")
			}
		})
	}
}

func TestAuthorizeRoutedWorkPoolStartActiveRecoveryRequiresExactAbsentRuntime(t *testing.T) {
	fixture := newRoutedWorkPoolAuthorizationFixture(t)
	fresh := &fixedFreshLivenessProvider{
		Provider:    fixture.snapshot.Provider,
		observation: runtime.Liveness{Complete: true},
	}
	fixture.snapshot.Provider = fresh
	if err := fixture.store.SetMetadataBatch(fixture.info.ID, map[string]string{
		"state":                     string(sessionpkg.StateActive),
		"pending_create_claim":      "",
		"pending_create_started_at": "",
	}); err != nil {
		t.Fatalf("mark exact pool allocation active: %v", err)
	}
	info, err := sessionFrontDoor(fixture.store).Get(fixture.info.ID)
	if err != nil {
		t.Fatalf("read active exact pool allocation: %v", err)
	}
	if err := fixture.cr.poolMembershipShadow.replace(fixture.snapshot.Config, info); err != nil {
		t.Fatalf("publish active exact pool allocation: %v", err)
	}
	fixture.lease.InstanceToken = info.InstanceToken
	fixture.lease.RecoverActive = true
	_, persisted, err := getAuthoritativeSessionStartPersistedRecord(fixture.store, info.ID)
	if err != nil {
		t.Fatalf("read exact active recovery revision: %v", err)
	}
	fixture.lease.SessionRevision = persisted.Revision

	authorized, err := fixture.cr.authorizeRoutedWorkPoolStart(t.Context(), fixture.snapshot, info, fixture.lease)
	if err != nil || !authorized {
		t.Fatalf("authorize exact active recovery with absent runtime = (%t, %v), want true", authorized, err)
	}

	ordinary := fixture.lease
	ordinary.RecoverActive = false
	authorized, err = fixture.cr.authorizeRoutedWorkPoolStart(t.Context(), fixture.snapshot, info, ordinary)
	if err != nil || authorized {
		t.Fatalf("authorize ordinary active pool row = (%t, %v), want false without recovery lease", authorized, err)
	}

	fresh.observation = runtime.Liveness{}
	authorized, err = fixture.cr.authorizeRoutedWorkPoolStart(t.Context(), fixture.snapshot, info, fixture.lease)
	if err != nil || authorized {
		t.Fatalf("authorize active recovery with incomplete absence = (%t, %v), want false", authorized, err)
	}

	if err := fixture.provider.Start(t.Context(), info.SessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start live replacement runtime: %v", err)
	}
	fresh.observation = runtime.Liveness{Running: true, Alive: true, Complete: true}
	authorized, err = fixture.cr.authorizeRoutedWorkPoolStart(t.Context(), fixture.snapshot, info, fixture.lease)
	if err != nil || authorized {
		t.Fatalf("authorize active recovery with live replacement = (%t, %v), want false", authorized, err)
	}
}

func TestAuthorizeRoutedWorkPoolStartActiveRecoveryRejectsDriftWithoutEffects(t *testing.T) {
	newActiveRecovery := func(t *testing.T) (routedWorkPoolAuthorizationFixture, routedWorkPoolStartLease) {
		t.Helper()
		fixture := newRoutedWorkPoolAuthorizationFixture(t)
		fixture.snapshot.Provider = &fixedFreshLivenessProvider{
			Provider:    fixture.snapshot.Provider,
			observation: runtime.Liveness{Complete: true},
		}
		if err := fixture.store.SetMetadataBatch(fixture.info.ID, map[string]string{
			"state":                     string(sessionpkg.StateActive),
			"pending_create_claim":      "",
			"pending_create_started_at": "",
		}); err != nil {
			t.Fatalf("mark exact pool allocation active: %v", err)
		}
		info, persisted, err := getAuthoritativeSessionStartPersistedRecord(fixture.store, fixture.info.ID)
		if err != nil {
			t.Fatalf("read active exact pool allocation: %v", err)
		}
		if err := fixture.cr.poolMembershipShadow.replace(fixture.snapshot.Config, info); err != nil {
			t.Fatalf("publish active exact pool allocation: %v", err)
		}
		lease := fixture.lease
		lease.InstanceToken = info.InstanceToken
		lease.RecoverActive = true
		lease.SessionRevision = persisted.Revision
		return fixture, lease
	}

	tests := []struct {
		name   string
		mutate func(*routedWorkPoolAuthorizationFixture, *routedWorkPoolStartLease)
	}{
		{
			name: "work claimed",
			mutate: func(f *routedWorkPoolAuthorizationFixture, _ *routedWorkPoolStartLease) {
				assignee := "another-session"
				if err := f.store.Update(f.work.ID, beads.UpdateOpts{Assignee: &assignee}); err != nil {
					f.t.Fatalf("claim routed work: %v", err)
				}
			},
		},
		{
			name: "work closed",
			mutate: func(f *routedWorkPoolAuthorizationFixture, _ *routedWorkPoolStartLease) {
				if err := f.store.Close(f.work.ID); err != nil {
					f.t.Fatalf("close routed work: %v", err)
				}
			},
		},
		{
			name: "work rerouted",
			mutate: func(f *routedWorkPoolAuthorizationFixture, _ *routedWorkPoolStartLease) {
				if err := f.store.SetMetadata(f.work.ID, "gc.routed_to", "other"); err != nil {
					f.t.Fatalf("reroute work: %v", err)
				}
			},
		},
		{
			name: "binding changed",
			mutate: func(f *routedWorkPoolAuthorizationFixture, _ *routedWorkPoolStartLease) {
				if err := f.store.SetMetadata(f.info.ID, beadmeta.TriggerBeadIDMetadataKey, "gc-other-work"); err != nil {
					f.t.Fatalf("change exact binding: %v", err)
				}
			},
		},
		{
			name: "same semantics durable revision changed",
			mutate: func(f *routedWorkPoolAuthorizationFixture, _ *routedWorkPoolStartLease) {
				if err := f.store.SetMetadata(f.info.ID, "diagnostic_note", "concurrent writer"); err != nil {
					f.t.Fatalf("change unrelated durable metadata: %v", err)
				}
			},
		},
		{
			name: "config generation changed",
			mutate: func(f *routedWorkPoolAuthorizationFixture, _ *routedWorkPoolStartLease) {
				next := *f.snapshot.Config
				f.cr.cfg = &next
			},
		},
		{
			name: "membership changed",
			mutate: func(f *routedWorkPoolAuthorizationFixture, _ *routedWorkPoolStartLease) {
				f.cr.poolMembershipShadow.remove(f.info.ID)
			},
		},
		{
			name: "agent suspended",
			mutate: func(f *routedWorkPoolAuthorizationFixture, _ *routedWorkPoolStartLease) {
				f.snapshot.Config.Agents[0].Suspended = true
			},
		},
		{
			name: "dependency policy changed",
			mutate: func(f *routedWorkPoolAuthorizationFixture, _ *routedWorkPoolStartLease) {
				f.snapshot.Config.Agents[0].DependsOn = []string{"other"}
			},
		},
		{
			name: "provider unhealthy",
			mutate: func(f *routedWorkPoolAuthorizationFixture, _ *routedWorkPoolStartLease) {
				f.snapshot.Config.Agents[0].Provider = "claude"
				path := filepath.Join(f.snapshot.CityPath, providerHealthCacheRelPath)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					f.t.Fatalf("create provider health directory: %v", err)
				}
				body, err := json.Marshal(providerHealthFileFormat{Providers: []providerHealthRecord{{Provider: "claude", Status: "unhealthy", ProbedAt: float64(time.Now().UnixNano()) / float64(time.Second)}}})
				if err != nil {
					f.t.Fatalf("marshal provider health: %v", err)
				}
				if err := os.WriteFile(path, body, 0o600); err != nil {
					f.t.Fatalf("write provider health: %v", err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture, lease := newActiveRecovery(t)
			test.mutate(&fixture, &lease)
			info, err := sessionFrontDoor(fixture.store).Get(fixture.info.ID)
			if err != nil {
				t.Fatalf("read mutated exact pool allocation: %v", err)
			}
			authorized, err := fixture.cr.authorizeRoutedWorkPoolStart(t.Context(), fixture.snapshot, info, lease)
			if err != nil {
				t.Fatalf("authorize drifted active recovery: %v", err)
			}
			if authorized {
				t.Fatal("drifted active recovery retained start authority")
			}
			if starts, stops, nudges := providerCallCount(fixture.provider, "Start"), providerCallCount(fixture.provider, "Stop"), providerNudgeCalls(fixture.provider, info.SessionName); starts != 0 || stops != 0 || nudges != 0 {
				t.Fatalf("drift authorization runtime effects = (starts=%d stops=%d nudges=%d), want none", starts, stops, nudges)
			}
		})
	}
}

func TestReconcileExactPoolRecoveryCASRefusalHasZeroRuntimeEffect(t *testing.T) {
	for _, mode := range []rollout.Mode{rollout.Auto, rollout.Require} {
		t.Run(string(mode), func(t *testing.T) {
			fixture, admission, params, before := newExactPoolRecoveryReconcileFixture(t, mode)
			conditional, ok := beads.ConditionalWriterFor(fixture.store)
			if !ok {
				t.Fatal("test store lacks conditional writer")
			}
			writer := &recordingExactStatusWriter{
				ConditionalWriter: conditional,
				err: &beads.PreconditionFailedError{
					ID:       admission.SessionID,
					Expected: before.Revision,
					Current:  before.Revision + 1,
				},
				forward: true,
			}
			params.StatusWriter = writer

			owner, reconcileErr := reconcileExactSessionStartWithOwner(t.Context(), admission, params)
			switch mode {
			case rollout.Auto:
				if owner != exactSessionStartLegacyOwner || !errors.Is(reconcileErr, errSessionStartLegacyFallbackRequired) {
					t.Fatalf("Auto owner/error = %v/%v, want visible legacy fallback", owner, reconcileErr)
				}
			case rollout.Require:
				if owner != exactSessionStartKeyedOwner || reconcileErr == nil || errors.Is(reconcileErr, errSessionStartLegacyFallbackRequired) {
					t.Fatalf("Require owner/error = %v/%v, want keyed park", owner, reconcileErr)
				}
			}
			if len(writer.expected) != 1 || writer.expected[0] != before.Revision {
				t.Fatalf("conditional revisions = %v, want only exact recovery revision %d", writer.expected, before.Revision)
			}
			if starts, stops, nudges := providerCallCount(fixture.provider, "Start"), providerCallCount(fixture.provider, "Stop"), providerNudgeCalls(fixture.provider, fixture.info.SessionName); starts != 0 || stops != 0 || nudges != 0 {
				t.Fatalf("CAS-refused runtime effects = (starts=%d stops=%d nudges=%d), want none", starts, stops, nudges)
			}
			after, err := fixture.store.Get(admission.SessionID)
			if err != nil {
				t.Fatalf("read CAS-refused recovery row: %v", err)
			}
			if after.Metadata["instance_token"] != before.Metadata["instance_token"] ||
				after.Metadata["generation"] != before.Metadata["generation"] ||
				after.Metadata["last_woke_at"] != before.Metadata["last_woke_at"] {
				t.Fatalf("CAS-refused incarnation changed: before=%+v after=%+v", before.Metadata, after.Metadata)
			}
		})
	}
}

func TestReconcileExactPoolRecoveryReplacementAfterPreWakeRestoresIncarnationWithoutEffects(t *testing.T) {
	fixture, admission, params, before := newExactPoolRecoveryReconcileFixture(t, rollout.Auto)
	hooked := &exactPoolRecoveryPostPreWakeHookStore{
		Store:         fixture.store,
		sessionID:     admission.SessionID,
		originalToken: before.Metadata["instance_token"],
		after: func(beads.Bead) {
			if err := fixture.provider.Start(t.Context(), fixture.info.SessionName, runtime.Config{Command: "replacement"}); err != nil {
				t.Fatalf("start same-name replacement: %v", err)
			}
		},
	}
	fixture.snapshot.Store = hooked
	params.Store = hooked
	params.AuthorizePoolStart = func(ctx context.Context, current sessionpkg.Info, candidate routedWorkPoolStartLease) (bool, error) {
		return fixture.cr.authorizeRoutedWorkPoolStart(ctx, fixture.snapshot, current, candidate)
	}
	conditional, ok := beads.ConditionalWriterFor(fixture.store)
	if !ok {
		t.Fatal("test store lacks conditional writer")
	}
	writer := &recordingExactStatusWriter{ConditionalWriter: conditional, forward: true}
	params.StatusWriter = writer

	owner, reconcileErr := reconcileExactSessionStartWithOwner(t.Context(), admission, params)
	if owner != exactSessionStartLegacyOwner || !errors.Is(reconcileErr, errSessionStartLegacyFallbackRequired) {
		t.Fatalf("owner/error = %v/%v, want visible fallback after replacement", owner, reconcileErr)
	}
	if len(writer.expected) != 2 || writer.expected[0] != before.Revision || writer.expected[1] == writer.expected[0] {
		t.Fatalf("conditional revisions = %v, want pre-wake CAS then fenced restore", writer.expected)
	}
	if starts, stops, nudges := providerCallCount(fixture.provider, "Start"), providerCallCount(fixture.provider, "Stop"), providerNudgeCalls(fixture.provider, fixture.info.SessionName); starts != 1 || stops != 0 || nudges != 0 {
		t.Fatalf("replacement race runtime effects = (starts=%d stops=%d nudges=%d), want only the injected replacement START", starts, stops, nudges)
	}
	after, err := fixture.store.Get(admission.SessionID)
	if err != nil {
		t.Fatalf("read replacement-race recovery row: %v", err)
	}
	if after.Metadata["instance_token"] != before.Metadata["instance_token"] ||
		after.Metadata["generation"] != before.Metadata["generation"] ||
		after.Metadata["last_woke_at"] != before.Metadata["last_woke_at"] {
		t.Fatalf("replacement race did not restore the prior incarnation: before=%+v after=%+v", before.Metadata, after.Metadata)
	}
}

func TestReconcileExactPoolRecoveryRevisionDriftBeforeStartDoesNotOverwriteConcurrentMutation(t *testing.T) {
	fixture, admission, params, before := newExactPoolRecoveryReconcileFixture(t, rollout.Auto)
	hooked := &exactPoolRecoveryPostPreWakeHookStore{
		Store:         fixture.store,
		sessionID:     admission.SessionID,
		originalToken: before.Metadata["instance_token"],
		after: func(bead beads.Bead) {
			if err := fixture.store.SetMetadataBatch(bead.ID, map[string]string{
				"diagnostic_note": "concurrent writer",
				"generation":      "999",
			}); err != nil {
				t.Fatalf("apply concurrent recovery mutation: %v", err)
			}
		},
	}
	fixture.snapshot.Store = hooked
	params.Store = hooked
	params.AuthorizePoolStart = func(ctx context.Context, current sessionpkg.Info, candidate routedWorkPoolStartLease) (bool, error) {
		return fixture.cr.authorizeRoutedWorkPoolStart(ctx, fixture.snapshot, current, candidate)
	}
	conditional, ok := beads.ConditionalWriterFor(fixture.store)
	if !ok {
		t.Fatal("test store lacks conditional writer")
	}
	writer := &recordingExactStatusWriter{ConditionalWriter: conditional, forward: true}
	params.StatusWriter = writer

	owner, reconcileErr := reconcileExactSessionStartWithOwner(t.Context(), admission, params)
	if owner != exactSessionStartLegacyOwner || !errors.Is(reconcileErr, errSessionStartLegacyFallbackRequired) {
		t.Fatalf("owner/error = %v/%v, want visible fallback after revision drift", owner, reconcileErr)
	}
	if len(writer.expected) != 2 || writer.expected[0] != before.Revision || writer.expected[1] == writer.expected[0] {
		t.Fatalf("conditional revisions = %v, want pre-wake CAS then stale fenced restore", writer.expected)
	}
	if starts, stops, nudges := providerCallCount(fixture.provider, "Start"), providerCallCount(fixture.provider, "Stop"), providerNudgeCalls(fixture.provider, fixture.info.SessionName); starts != 0 || stops != 0 || nudges != 0 {
		t.Fatalf("revision-drift runtime effects = (starts=%d stops=%d nudges=%d), want none", starts, stops, nudges)
	}
	after, err := fixture.store.Get(admission.SessionID)
	if err != nil {
		t.Fatalf("read revision-drift recovery row: %v", err)
	}
	if after.Metadata["diagnostic_note"] != "concurrent writer" || after.Metadata["generation"] != "999" {
		t.Fatalf("fenced rollback overwrote concurrent mutation: %+v", after.Metadata)
	}
}

func TestReconcileExactPoolRecoveryStartsThroughFinalAuthorizationFence(t *testing.T) {
	fixture, admission, params, before := newExactPoolRecoveryReconcileFixture(t, rollout.Auto)
	conditional, ok := beads.ConditionalWriterFor(fixture.store)
	if !ok {
		t.Fatal("test store lacks conditional writer")
	}
	writer := &recordingExactStatusWriter{ConditionalWriter: conditional, forward: true}
	params.StatusWriter = writer
	delegate := params.AuthorizePoolStart
	var authorizations atomic.Int32
	params.AuthorizePoolStart = func(ctx context.Context, current sessionpkg.Info, candidate routedWorkPoolStartLease) (bool, error) {
		authorizations.Add(1)
		return delegate(ctx, current, candidate)
	}

	owner, reconcileErr := reconcileExactSessionStartWithOwner(t.Context(), admission, params)
	if owner != exactSessionStartKeyedOwner || reconcileErr != nil {
		t.Fatalf("owner/error = %v/%v, want committed keyed recovery", owner, reconcileErr)
	}
	if got := authorizations.Load(); got != 3 {
		t.Fatalf("authorization calls = %d, want admission, pre-CAS, and provider-boundary checks", got)
	}
	if len(writer.expected) != 1 || writer.expected[0] != before.Revision {
		t.Fatalf("conditional revisions = %v, want one exact pre-wake CAS at %d", writer.expected, before.Revision)
	}
	if starts, stops, nudges := providerCallCount(fixture.provider, "Start"), providerCallCount(fixture.provider, "Stop"), providerNudgeCalls(fixture.provider, fixture.info.SessionName); starts != 1 || stops != 0 || nudges != 0 {
		t.Fatalf("successful recovery runtime effects = (starts=%d stops=%d nudges=%d), want one START only", starts, stops, nudges)
	}
	after, err := sessionFrontDoor(fixture.store).Get(admission.SessionID)
	if err != nil {
		t.Fatalf("read recovered exact pool row: %v", err)
	}
	if after.InstanceToken == before.Metadata["instance_token"] || after.MetadataState != string(sessionpkg.StateActive) || after.ID != fixture.info.ID || after.SessionName != fixture.info.SessionName {
		t.Fatalf("recovered row = %+v, want same active row with rotated token", after)
	}
}

func TestNewRoutedWorkPoolRecoveryLeaseCapturesExactDurableRevision(t *testing.T) {
	fixture := newRoutedWorkPoolAuthorizationFixture(t)
	info, persisted, err := getAuthoritativeSessionStartPersistedRecord(fixture.store, fixture.info.ID)
	if err != nil {
		t.Fatalf("read exact pool row: %v", err)
	}
	hint := routedWorkPoolAllocationHint{
		WorkID:      fixture.work.ID,
		PoolTarget:  "worker",
		SourceStore: "city:test-city",
	}
	lease, err := fixture.cr.newRoutedWorkPoolRecoveryLease(fixture.snapshot, info, persisted, hint)
	if err != nil {
		t.Fatalf("create exact recovery lease: %v", err)
	}
	if !lease.RecoverActive || lease.SessionRevision <= 0 || lease.SessionRevision != persisted.Revision {
		t.Fatalf("recovery lease = %+v, want active recovery at exact revision %d", lease, persisted.Revision)
	}

	persisted.Revision = 0
	if _, err := fixture.cr.newRoutedWorkPoolRecoveryLease(fixture.snapshot, info, persisted, hint); err == nil {
		t.Fatal("zero-revision recovery lease was accepted")
	}
}

func TestAuthorizeRoutedWorkPoolStartRetainsExactMemberAuthorityAfterPoolGrowth(t *testing.T) {
	fixture := newRoutedWorkPoolAuthorizationFixture(t)
	other, err := createPoolSessionBeadWithAlias(fixture.store, "worker", fixture.snapshot.Config, nil, time.Now().UTC(), poolSessionCreateIdentity{
		AgentName: "worker-2",
		Slot:      2,
		Metadata: map[string]string{
			beadmeta.TriggerBeadIDMetadataKey:       "gc-other-work",
			beadmeta.TriggerBeadStoreRefMetadataKey: fixture.lease.SourceStore,
		},
	}, "")
	if err != nil {
		t.Fatalf("create second occupied pool session: %v", err)
	}
	if err := fixture.store.SetMetadata(other.ID, "state", string(sessionpkg.StateActive)); err != nil {
		t.Fatalf("make second pool session active: %v", err)
	}
	other, err = sessionFrontDoor(fixture.store).Get(other.ID)
	if err != nil {
		t.Fatalf("read second occupied pool session: %v", err)
	}
	if err := fixture.cr.poolMembershipShadow.replace(fixture.snapshot.Config, other); err != nil {
		t.Fatalf("publish second occupied pool session: %v", err)
	}

	observation := fixture.cr.poolMembershipShadow.observe("worker")
	if !observation.certified || observation.members != 2 || observation.occupied != 2 {
		t.Fatalf("grown pool membership = %+v, want two certified occupied members", observation)
	}
	authorized, err := fixture.cr.authorizeRoutedWorkPoolStart(t.Context(), fixture.snapshot, fixture.info, fixture.lease)
	if err != nil || !authorized {
		t.Fatalf("authorize original exact member after pool growth = (%t, %v), want true", authorized, err)
	}
}

func TestAuthorizeRoutedWorkPoolStartRejectsBoundedPoolGrowthPastCap(t *testing.T) {
	tests := []struct {
		name      string
		maximum   int
		occupancy int
	}{
		{name: "multi-session pool", maximum: 2, occupancy: 3},
		{name: "canonical singleton", maximum: 1, occupancy: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRoutedWorkPoolAuthorizationFixture(t)
			for slot := 2; slot <= test.occupancy; slot++ {
				other, err := createPoolSessionBeadWithAlias(fixture.store, "worker", fixture.snapshot.Config, nil, time.Now().UTC(), poolSessionCreateIdentity{
					AgentName: fmt.Sprintf("worker-%d", slot),
					Slot:      slot,
					Metadata: map[string]string{
						beadmeta.TriggerBeadIDMetadataKey:       fmt.Sprintf("gc-other-work-%d", slot),
						beadmeta.TriggerBeadStoreRefMetadataKey: fixture.lease.SourceStore,
					},
				}, "")
				if err != nil {
					t.Fatalf("create occupied pool session %d: %v", slot, err)
				}
				if err := fixture.store.SetMetadata(other.ID, "state", string(sessionpkg.StateActive)); err != nil {
					t.Fatalf("make pool session %d active: %v", slot, err)
				}
				other, err = sessionFrontDoor(fixture.store).Get(other.ID)
				if err != nil {
					t.Fatalf("read occupied pool session %d: %v", slot, err)
				}
				if err := fixture.cr.poolMembershipShadow.replace(fixture.snapshot.Config, other); err != nil {
					t.Fatalf("publish occupied pool session %d: %v", slot, err)
				}
			}

			observation := fixture.cr.poolMembershipShadow.observe("worker")
			if !observation.certified || observation.occupied != test.occupancy {
				t.Fatalf("grown bounded membership = %+v, want %d certified occupied members", observation, test.occupancy)
			}
			fixture.snapshot.Config.Agents[0].MaxActiveSessions = &test.maximum
			policy := newPoolAllocationShadowPolicy(fixture.snapshot.Config, &fixture.snapshot.Config.Agents[0], nil)
			if !policy.supported() {
				t.Fatalf("bounded policy = %+v, want supported start policy", policy)
			}
			authorized, err := fixture.cr.authorizeRoutedWorkPoolStart(t.Context(), fixture.snapshot, fixture.info, fixture.lease)
			if err != nil {
				t.Fatalf("authorize over-cap pool start: %v", err)
			}
			if authorized {
				t.Fatal("over-cap bounded pool retained exact start authority")
			}
		})
	}
}

func TestAuthorizeRoutedWorkPoolDrainAckRequiresExactLiveEvidence(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*routedWorkPoolDrainAckAuthorizationFixture)
		wantError bool
	}{
		{
			name: "config generation changed",
			mutate: func(f *routedWorkPoolDrainAckAuthorizationFixture) {
				f.lease.ControllerGeneration++
			},
		},
		{
			name: "config instance changed",
			mutate: func(f *routedWorkPoolDrainAckAuthorizationFixture) {
				next := *f.snapshot.Config
				f.cr.cfg = &next
			},
		},
		{
			name: "durable instance token changed",
			mutate: func(f *routedWorkPoolDrainAckAuthorizationFixture) {
				f.info.InstanceToken = "replacement-token"
			},
		},
		{
			// In stamp mode the fence is the acknowledgement's own trigger, so a
			// live stamp that no longer matches the lease is the refusal — the
			// ROW moving is not, because legacy re-pointing a member that already
			// acknowledged its drain is exactly what the stamp exists to survive
			// (ga-f7v2ft.131).
			name: "acknowledged trigger stamp changed",
			mutate: func(f *routedWorkPoolDrainAckAuthorizationFixture) {
				restampAckTrigger(f, "ga-other-work", f.sourceStore)
			},
		},
		{
			name: "acknowledged store ref stamp changed",
			mutate: func(f *routedWorkPoolDrainAckAuthorizationFixture) {
				restampAckTrigger(f, f.work.ID, "rig:elsewhere")
			},
		},
		{
			// An unstamped acknowledgement (older agent CLI) keeps the row as its
			// only evidence, so the row binding stays its fence.
			name: "durable trigger changed on an unstamped acknowledgement",
			mutate: func(f *routedWorkPoolDrainAckAuthorizationFixture) {
				clearAckTriggerStamp(f)
				f.lease.TriggerFromAck = false
				f.info.TriggerBeadID = "ga-other-work"
			},
		},
		{
			name: "requester session changed",
			mutate: func(f *routedWorkPoolDrainAckAuthorizationFixture) {
				f.lease.RequesterSessionID = "gc-other-session"
			},
		},
		{
			name: "requester runtime token changed",
			mutate: func(f *routedWorkPoolDrainAckAuthorizationFixture) {
				if err := f.provider.SetMeta(f.info.SessionName, drainAckRequesterInstanceTokenKey, "replacement-token"); err != nil {
					f.t.Fatalf("change requester runtime token: %v", err)
				}
			},
		},
		{
			// An unconfigured source ref refuses cleanly at the release gate —
			// an acknowledgement the keyed lane cannot service is legacy's to
			// handle. It no longer walks into the store resolution and errors
			// as unavailable, which retried a permanently-unresolvable ref
			// forever (the old agent-scope proxy waved any "city:" prefix
			// through for a city-scoped agent).
			name: "source store unconfigured",
			mutate: func(f *routedWorkPoolDrainAckAuthorizationFixture) {
				f.info.TriggerBeadStoreRef = "city:missing"
				f.lease.SourceStore = "city:missing"
				restampAckTrigger(f, f.work.ID, "city:missing")
			},
		},
		{
			name: "runtime session id changed",
			mutate: func(f *routedWorkPoolDrainAckAuthorizationFixture) {
				if err := f.provider.SetMeta(f.info.SessionName, "GC_SESSION_ID", "gcs-other"); err != nil {
					f.t.Fatalf("change runtime session id: %v", err)
				}
			},
		},
		{
			name: "runtime token changed",
			mutate: func(f *routedWorkPoolDrainAckAuthorizationFixture) {
				if err := f.provider.SetMeta(f.info.SessionName, "GC_INSTANCE_TOKEN", "replacement-token"); err != nil {
					f.t.Fatalf("change runtime token: %v", err)
				}
			},
		},
		{
			name: "ack source is not agent",
			mutate: func(f *routedWorkPoolDrainAckAuthorizationFixture) {
				if err := f.provider.SetMeta(f.info.SessionName, reconcilerDrainAckSourceKey, reconcilerDrainAckSourceValue); err != nil {
					f.t.Fatalf("change acknowledgement source: %v", err)
				}
			},
		},
		{
			name: "ack bit cleared",
			mutate: func(f *routedWorkPoolDrainAckAuthorizationFixture) {
				if err := f.provider.RemoveMeta(f.info.SessionName, "GC_DRAIN_ACK"); err != nil {
					f.t.Fatalf("clear acknowledgement: %v", err)
				}
			},
		},
		{
			name: "pending interaction",
			mutate: func(f *routedWorkPoolDrainAckAuthorizationFixture) {
				f.provider.SetPendingInteraction(f.info.SessionName, &runtime.PendingInteraction{RequestID: "approval-1"})
			},
		},
		{
			name:      "provider cannot prove pending interaction",
			wantError: true,
			mutate: func(f *routedWorkPoolDrainAckAuthorizationFixture) {
				f.snapshot.Provider = poolDrainAckProviderWithoutInteraction{Provider: f.provider}
			},
		},
		{
			name:      "runtime metadata read failed",
			wantError: true,
			mutate: func(f *routedWorkPoolDrainAckAuthorizationFixture) {
				f.snapshot.Provider = runtime.NewFailFake()
			},
		},
		{
			name: "trigger work reopened",
			mutate: func(f *routedWorkPoolDrainAckAuthorizationFixture) {
				status := "open"
				if err := f.store.Update(f.work.ID, beads.UpdateOpts{Status: &status}); err != nil {
					f.t.Fatalf("reopen trigger work: %v", err)
				}
			},
		},
		{
			name:      "trigger work disappeared",
			wantError: true,
			mutate: func(f *routedWorkPoolDrainAckAuthorizationFixture) {
				f.info.TriggerBeadID = "ga-missing-work"
				f.lease.WorkID = "ga-missing-work"
				restampAckTrigger(f, "ga-missing-work", f.sourceStore)
			},
		},
		{
			name: "other awake assigned work",
			mutate: func(f *routedWorkPoolDrainAckAuthorizationFixture) {
				if _, err := f.store.Create(beads.Bead{
					Title:    "new assigned work",
					Type:     "task",
					Status:   "in_progress",
					Assignee: f.info.SessionName,
				}); err != nil {
					f.t.Fatalf("create assigned work: %v", err)
				}
			},
		},
		{
			name: "unsupported pool policy",
			mutate: func(f *routedWorkPoolDrainAckAuthorizationFixture) {
				f.snapshot.Config.Agents[0].DependsOn = []string{"database"}
			},
		},
		{
			name: "numbered singleton stop remains legacy owned",
			mutate: func(f *routedWorkPoolDrainAckAuthorizationFixture) {
				maximum := 1
				f.snapshot.Config.Agents[0].MaxActiveSessions = &maximum
			},
		},
		{
			name: "membership lost exact member",
			mutate: func(f *routedWorkPoolDrainAckAuthorizationFixture) {
				f.cr.poolMembershipShadow.remove(f.info.ID)
			},
		},
		{
			name: "membership revision not observed",
			mutate: func(f *routedWorkPoolDrainAckAuthorizationFixture) {
				f.lease.MembershipRevision++
			},
		},
		{
			name: "session already closed",
			mutate: func(f *routedWorkPoolDrainAckAuthorizationFixture) {
				f.info.Closed = true
			},
		},
		{
			name: "unsupported pre-CAS lifecycle shape",
			mutate: func(f *routedWorkPoolDrainAckAuthorizationFixture) {
				f.info.MetadataState = string(sessionpkg.StateAsleep)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
			test.mutate(&fixture)
			before, err := fixture.store.Get(fixture.info.ID)
			if err != nil {
				t.Fatalf("read row before authorization: %v", err)
			}

			authorized, _, authorizeErr := fixture.cr.authorizeRoutedWorkPoolDrainAck(fixture.snapshot, fixture.info, fixture.lease)
			if authorized {
				t.Fatal("stale or unsafe drain acknowledgement retained stop authority")
			}
			if (authorizeErr != nil) != test.wantError {
				t.Fatalf("authorization error = %v, wantError=%t", authorizeErr, test.wantError)
			}
			after, err := fixture.store.Get(fixture.info.ID)
			if err != nil {
				t.Fatalf("read row after authorization: %v", err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("authorization mutated durable row:\nbefore=%+v\nafter=%+v", before, after)
			}
			if got := fixture.provider.CountCalls("Stop", fixture.info.SessionName); got != 0 {
				t.Fatalf("provider Stop calls = %d, want 0", got)
			}
		})
	}
}

func TestAuthorizeRoutedWorkPoolDrainAckAcceptsExactStopPendingRow(t *testing.T) {
	fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
	if err := fixture.store.SetMetadataBatch(fixture.info.ID, sessionpkg.DrainAckStopPendingPatch(time.Now().UTC())); err != nil {
		t.Fatalf("mark drain acknowledgement stop-pending: %v", err)
	}
	info, err := sessionFrontDoor(fixture.store).Get(fixture.info.ID)
	if err != nil {
		t.Fatalf("read stop-pending pool session: %v", err)
	}
	if !isDrainAckStopPendingInfo(info) {
		t.Fatal("fixture did not enter drain-ack stop-pending")
	}

	authorized, _, err := fixture.cr.authorizeRoutedWorkPoolDrainAck(fixture.snapshot, info, fixture.lease)
	if err != nil || !authorized {
		t.Fatalf("authorize exact stop-pending drain acknowledgement = (%t, %v), want true", authorized, err)
	}
}

// TestCanonicalizeLegacyWorkflowStoreRef pins the definite legacy→canonical
// mapping. It is deliberately not a wildcard: an unknown bare ref stays
// unchanged so downstream validation still refuses it (ga-2oboq).
func TestCanonicalizeLegacyWorkflowStoreRef(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city", Prefix: "hq"},
		Rigs:      []config.Rig{{Name: "packs", Path: filepath.Join(cityPath, "rigs", "packs")}},
	}
	for _, test := range []struct {
		name string
		ref  string
		want string
	}{
		{name: "legacy bare city", ref: "city", want: "city:test-city"},
		{name: "legacy bare rig", ref: "packs", want: "rig:packs"},
		{name: "canonical city untouched", ref: "city:other", want: "city:other"},
		{name: "canonical rig untouched", ref: "rig:packs", want: "rig:packs"},
		{name: "unknown bare ref untouched", ref: "not-a-store", want: "not-a-store"},
		{name: "empty ref untouched", ref: "", want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := canonicalizeLegacyWorkflowStoreRef(cfg, cityPath, test.ref); got != test.want {
				t.Fatalf("canonicalizeLegacyWorkflowStoreRef(%q) = %q, want %q", test.ref, got, test.want)
			}
		})
	}
}

// TestRoutedWorkPoolDrainAckCanonicalizesLegacyBareStoreRefs covers the
// permanent park ga-2oboq filed. The legacy demand collector speaks a bare
// storeKey vocabulary ("city", a bare rig name) and stamps it verbatim into the
// member row; the keyed drain-ack seam rebuilds its lease FROM that row, so the
// bare spelling failed both AgentReachesWorkflowStore (rig-less agents require a
// "city:" prefix) and routedWorkStore's canonical equality. Under
// first-creator-wins, legacy-created members are the norm, so that refusal made
// keyed drain of the normal population impossible.
func TestRoutedWorkPoolDrainAckCanonicalizesLegacyBareStoreRefs(t *testing.T) {
	t.Run("bare city ref reaches the provider-meta checks", func(t *testing.T) {
		fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
		info := stampLegacyBareTriggerStoreRef(t, fixture, "city")
		lease, agentAck, _, err := fixture.cr.newRoutedWorkPoolDrainAckLease(fixture.snapshot, info)
		if err != nil || !agentAck {
			t.Fatalf("legacy-stamped drain acknowledgement lease = (%+v, %t, %v), want an admitted lease", lease, agentAck, err)
		}
		if lease.SourceStore != "city:test-city" {
			t.Fatalf("lease source store = %q, want the canonical HQ spelling %q", lease.SourceStore, "city:test-city")
		}
		authorized, _, err := fixture.cr.authorizeRoutedWorkPoolDrainAck(fixture.snapshot, info, lease)
		if err != nil || !authorized {
			t.Fatalf("legacy bare-city drain acknowledgement authorization = (%t, %v), want true", authorized, err)
		}
	})

	t.Run("bare rig ref reaches the provider-meta checks", func(t *testing.T) {
		fixture := newRoutedWorkPoolDrainAckAuthorizationFixtureWithOptions(t, routedWorkPoolAuthorizationFixtureOptions{rigName: "packs"})
		info := stampLegacyBareTriggerStoreRef(t, fixture, "packs")
		lease, agentAck, _, err := fixture.cr.newRoutedWorkPoolDrainAckLease(fixture.snapshot, info)
		if err != nil || !agentAck {
			t.Fatalf("legacy-stamped drain acknowledgement lease = (%+v, %t, %v), want an admitted lease", lease, agentAck, err)
		}
		if lease.SourceStore != "rig:packs" {
			t.Fatalf("lease source store = %q, want the canonical rig spelling %q", lease.SourceStore, "rig:packs")
		}
		authorized, _, err := fixture.cr.authorizeRoutedWorkPoolDrainAck(fixture.snapshot, info, lease)
		if err != nil || !authorized {
			t.Fatalf("legacy bare-rig drain acknowledgement authorization = (%t, %v), want true", authorized, err)
		}
	})

	t.Run("unknown bare ref still refuses", func(t *testing.T) {
		fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
		info := stampLegacyBareTriggerStoreRef(t, fixture, "not-a-configured-store")
		lease, agentAck, _, err := fixture.cr.newRoutedWorkPoolDrainAckLease(fixture.snapshot, info)
		if err != nil || !agentAck {
			t.Fatalf("unknown-ref drain acknowledgement lease = (%+v, %t, %v), want an admitted lease", lease, agentAck, err)
		}
		if lease.SourceStore != "not-a-configured-store" {
			t.Fatalf("lease source store = %q, want the unknown ref left verbatim", lease.SourceStore)
		}
		authorized, _, err := fixture.cr.authorizeRoutedWorkPoolDrainAck(fixture.snapshot, info, lease)
		if authorized || err != nil {
			t.Fatalf("unknown bare-ref drain acknowledgement authorization = (%t, %v), want a clean refusal", authorized, err)
		}
	})

	// Sibling audit: the pool start seam builds its lease from the canonical
	// allocation hint but re-checks it against the raw row, so a legacy-created
	// member could never be recovered either.
	t.Run("pool start recovery accepts a legacy bare-city row", func(t *testing.T) {
		fixture := newRoutedWorkPoolAuthorizationFixture(t)
		if err := fixture.store.SetMetadata(fixture.info.ID, beadmeta.TriggerBeadStoreRefMetadataKey, "city"); err != nil {
			t.Fatalf("stamp legacy bare trigger store ref: %v", err)
		}
		info, err := sessionFrontDoor(fixture.store).Get(fixture.info.ID)
		if err != nil {
			t.Fatalf("read legacy-stamped pool session: %v", err)
		}
		authorized, err := fixture.cr.authorizeRoutedWorkPoolStart(t.Context(), fixture.snapshot, info, fixture.lease)
		if err != nil || !authorized {
			t.Fatalf("legacy bare-city pool start authorization = (%t, %v), want true", authorized, err)
		}
	})
}

// restampAckTrigger rewrites the acknowledgement's own trigger stamp. A test
// that fabricates a lease has to move the ack evidence with it: in stamp mode
// the effect boundary fences on the LIVE stamp, not on the member row.
func restampAckTrigger(f *routedWorkPoolDrainAckAuthorizationFixture, beadID, storeRef string) {
	f.t.Helper()
	if err := f.provider.SetMeta(f.info.SessionName, reconcilerDrainAckTriggerBeadIDKey, beadID); err != nil {
		f.t.Fatalf("restamp acknowledged trigger bead: %v", err)
	}
	if err := f.provider.SetMeta(f.info.SessionName, reconcilerDrainAckTriggerStoreRefKey, storeRef); err != nil {
		f.t.Fatalf("restamp acknowledged trigger store ref: %v", err)
	}
}

// clearAckTriggerStamp reproduces an acknowledgement written by an agent CLI
// from before the trigger stamp existed.
func clearAckTriggerStamp(f *routedWorkPoolDrainAckAuthorizationFixture) {
	f.t.Helper()
	if err := f.provider.RemoveMeta(f.info.SessionName, reconcilerDrainAckTriggerBeadIDKey); err != nil {
		f.t.Fatalf("clear acknowledged trigger bead stamp: %v", err)
	}
	if err := f.provider.RemoveMeta(f.info.SessionName, reconcilerDrainAckTriggerStoreRefKey); err != nil {
		f.t.Fatalf("clear acknowledged trigger store ref stamp: %v", err)
	}
}

func stampLegacyBareTriggerStoreRef(
	t *testing.T,
	fixture routedWorkPoolDrainAckAuthorizationFixture,
	bareRef string,
) sessionpkg.Info {
	t.Helper()
	if err := fixture.store.SetMetadata(fixture.info.ID, beadmeta.TriggerBeadStoreRefMetadataKey, bareRef); err != nil {
		t.Fatalf("stamp legacy bare trigger store ref: %v", err)
	}
	// The ack stamps the row VERBATIM, so a bare row yields a bare stamp: the
	// canonicalizer stays the single read-side point either way (ga-2oboq).
	if err := fixture.provider.SetMeta(fixture.info.SessionName, reconcilerDrainAckTriggerStoreRefKey, bareRef); err != nil {
		t.Fatalf("stamp legacy bare acknowledged store ref: %v", err)
	}
	info, err := sessionFrontDoor(fixture.store).Get(fixture.info.ID)
	if err != nil {
		t.Fatalf("read legacy-stamped pool session: %v", err)
	}
	if strings.TrimSpace(info.TriggerBeadStoreRef) != bareRef {
		t.Fatalf("legacy-stamped trigger store ref = %q, want %q", info.TriggerBeadStoreRef, bareRef)
	}
	if err := fixture.cr.poolMembershipShadow.replace(fixture.snapshot.Config, info); err != nil {
		t.Fatalf("publish legacy-stamped pool membership: %v", err)
	}
	return info
}

func TestRecoverRoutedWorkPoolDrainAckLeaseDistinguishesLegacyFromUnknownProvenance(t *testing.T) {
	fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
	if err := fixture.provider.SetMeta(fixture.info.SessionName, reconcilerDrainAckSourceKey, reconcilerDrainAckSourceValue); err != nil {
		t.Fatalf("set legacy drain acknowledgement source: %v", err)
	}
	_, agent, legacy, err := fixture.cr.recoverRoutedWorkPoolDrainAckLease(fixture.snapshot, fixture.info)
	if err != nil || agent || !legacy {
		t.Fatalf("recover legacy provenance = (agent=%t, legacy=%t, err=%v), want false/true/nil", agent, legacy, err)
	}
	if err := fixture.provider.RemoveMeta(fixture.info.SessionName, reconcilerDrainAckSourceKey); err != nil {
		t.Fatalf("remove drain acknowledgement source: %v", err)
	}
	// Re-pointed by ga-f7v2ft.173: unrecognized provenance is evidence, not an
	// error — the caller re-validates it against a fresh COMPLETE liveness
	// observation before any effect. The pin's teeth are unchanged: recovery
	// must never CLAIM an agent acknowledgement or a legacy marker it cannot
	// read.
	_, agent, legacy, err = fixture.cr.recoverRoutedWorkPoolDrainAckLease(fixture.snapshot, fixture.info)
	if err != nil || agent || legacy {
		t.Fatalf("recover unknown provenance = (agent=%t, legacy=%t, err=%v), want false/false/nil unrecognized", agent, legacy, err)
	}
}

// TestRecoverRoutedWorkPoolDrainAckLeaseHoldsMemberOutsideKeyedMembership is
// the recovery half of council R1. Keyed pool membership is allocation
// lineage, and the member shape the whole fleet has at cutover is one LEGACY
// created — so a member the keyed index never held used to fail recovery
// outright, handing a stamp-provable acknowledgement back to the legacy
// reconciler, which then applied the very drain effect the routed-work drain
// leg asserts nobody but the keyed owner applies.
func TestRecoverRoutedWorkPoolDrainAckLeaseHoldsMemberOutsideKeyedMembership(t *testing.T) {
	fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
	fixture.cr.poolMembershipShadow.remove(fixture.info.ID)
	lease, agent, legacy, err := fixture.cr.recoverRoutedWorkPoolDrainAckLease(fixture.snapshot, fixture.info)
	if err != nil || !agent || legacy {
		t.Fatalf("recover legacy-created member = (agent=%t, legacy=%t, err=%v), want an agent lease", agent, legacy, err)
	}
	if lease.SessionID != fixture.info.ID || lease.MembershipOccupied ||
		lease.RequesterSessionID != fixture.info.ID || lease.RequesterInstanceToken != fixture.info.InstanceToken {
		t.Fatalf("recovered lease = %+v, want the row's own ack stamps with MembershipOccupied=false", lease)
	}
	// The stamps are still the fence: strip the agent source and recovery must
	// stop claiming an agent acknowledgement. Re-pointed by ga-f7v2ft.173: the
	// unstamped answer is the unrecognized disposition (false/false/nil), never
	// a claimed agent lease or legacy marker.
	if err := fixture.provider.RemoveMeta(fixture.info.SessionName, reconcilerDrainAckSourceKey); err != nil {
		t.Fatalf("remove drain acknowledgement source: %v", err)
	}
	if _, agent, legacy, err = fixture.cr.recoverRoutedWorkPoolDrainAckLease(fixture.snapshot, fixture.info); err != nil || agent || legacy {
		t.Fatalf("recover unstamped member = (agent=%t, legacy=%t, err=%v), want false/false/nil unrecognized", agent, legacy, err)
	}
}

func TestNewRoutedWorkPoolDrainAckLeaseDistinguishesAgentAckFromOrdinaryStart(t *testing.T) {
	t.Run("certified agent acknowledgement", func(t *testing.T) {
		fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
		lease, agentAck, _, err := fixture.cr.newRoutedWorkPoolDrainAckLease(fixture.snapshot, fixture.info)
		if err != nil || !agentAck || lease != fixture.lease {
			t.Fatalf("new drain acknowledgement lease = (%+v, %t, %v), want %+v", lease, agentAck, err, fixture.lease)
		}
	})

	t.Run("admission does not reread work stores", func(t *testing.T) {
		fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
		store := &poolDrainAckAdmissionReadRejectStore{Store: fixture.store}
		fixture.snapshot.Store = store
		fixture.cr.cs.cityBeadStore = store
		lease, agentAck, _, err := fixture.cr.newRoutedWorkPoolDrainAckLease(fixture.snapshot, fixture.info)
		if err != nil || !agentAck || lease != fixture.lease {
			t.Fatalf("new drain acknowledgement lease = (%+v, %t, %v), want cheap lease %+v", lease, agentAck, err, fixture.lease)
		}
		if store.reads != 0 {
			t.Fatalf("admission store reads = %d, want 0 before effect-time authorization", store.reads)
		}
	})

	t.Run("ordinary live session", func(t *testing.T) {
		fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
		if err := fixture.provider.RemoveMeta(fixture.info.SessionName, reconcilerDrainAckSourceKey); err != nil {
			t.Fatalf("clear acknowledgement source: %v", err)
		}
		lease, agentAck, _, err := fixture.cr.newRoutedWorkPoolDrainAckLease(fixture.snapshot, fixture.info)
		if err != nil || agentAck || lease != (routedWorkPoolDrainAckLease{}) {
			t.Fatalf("ordinary session drain lease = (%+v, %t, %v), want no acknowledgement", lease, agentAck, err)
		}
	})

	t.Run("ordinary live session without membership index", func(t *testing.T) {
		fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
		if err := fixture.provider.RemoveMeta(fixture.info.SessionName, reconcilerDrainAckSourceKey); err != nil {
			t.Fatalf("clear acknowledgement source: %v", err)
		}
		fixture.cr.poolMembershipShadow = nil
		lease, agentAck, _, err := fixture.cr.newRoutedWorkPoolDrainAckLease(fixture.snapshot, fixture.info)
		if err != nil || agentAck || lease != (routedWorkPoolDrainAckLease{}) {
			t.Fatalf("ordinary session drain lease = (%+v, %t, %v), want no acknowledgement", lease, agentAck, err)
		}
	})

	t.Run("agent acknowledgement without membership index", func(t *testing.T) {
		fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
		fixture.cr.poolMembershipShadow = nil
		lease, agentAck, _, err := fixture.cr.newRoutedWorkPoolDrainAckLease(fixture.snapshot, fixture.info)
		if err == nil || !agentAck || lease != (routedWorkPoolDrainAckLease{}) || !strings.Contains(err.Error(), "keyed state is unavailable") {
			t.Fatalf("uncertain drain lease = (%+v, %t, %v), want visible acknowledged uncertainty", lease, agentAck, err)
		}
	})

	t.Run("agent acknowledgement without requester binding", func(t *testing.T) {
		fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
		if err := fixture.provider.RemoveMeta(fixture.info.SessionName, drainAckRequesterInstanceTokenKey); err != nil {
			t.Fatalf("clear acknowledgement requester token: %v", err)
		}
		lease, agentAck, _, err := fixture.cr.newRoutedWorkPoolDrainAckLease(fixture.snapshot, fixture.info)
		if err != nil || !agentAck || lease != (routedWorkPoolDrainAckLease{}) {
			t.Fatalf("unbound drain lease = (%+v, %t, %v), want acknowledged but unadmitted", lease, agentAck, err)
		}
	})

	// Council R1: a member outside keyed pool membership — every legacy-created
	// member, which is the whole fleet at cutover — still gets a lease. The
	// acknowledgement stamps are the fence, not the allocation lineage, and the
	// lease records the lineage it did not find so the fence it dropped is
	// visible in the trace rather than silent.
	t.Run("member outside keyed membership", func(t *testing.T) {
		fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
		fixture.cr.poolMembershipShadow.remove(fixture.info.ID)
		lease, agentAck, refusal, err := fixture.cr.newRoutedWorkPoolDrainAckLease(fixture.snapshot, fixture.info)
		if err != nil || !agentAck || refusal != drainAckRefusalNone {
			t.Fatalf("legacy-created member drain lease = (%t, %q, %v), want an admitted agent acknowledgement", agentAck, refusal, err)
		}
		if lease.SessionID != fixture.info.ID || lease.MembershipOccupied || !lease.TriggerFromAck {
			t.Fatalf("legacy-created member drain lease = %+v, want the stamped lease with MembershipOccupied=false", lease)
		}
	})

	t.Run("running provider metadata uncertainty", func(t *testing.T) {
		fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
		fixture.snapshot.Provider = poolDrainAckGetMetaErrorProvider{
			Provider: fixture.provider,
			err:      errors.New("runtime metadata unavailable"),
		}
		lease, agentAck, _, err := fixture.cr.newRoutedWorkPoolDrainAckLease(fixture.snapshot, fixture.info)
		if err == nil || !agentAck || lease != (routedWorkPoolDrainAckLease{}) || !strings.Contains(err.Error(), "runtime metadata unavailable") {
			t.Fatalf("uncertain drain lease = (%+v, %t, %v), want visible acknowledged uncertainty", lease, agentAck, err)
		}
	})
}

func TestCityRuntimeSocketReportsDrainAckAdmissionUncertainty(t *testing.T) {
	fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
	readErr := errors.New("runtime metadata unavailable")
	fixture.cr.cs.mu.Lock()
	fixture.cr.cs.sp = poolDrainAckGetMetaErrorProvider{Provider: fixture.provider, err: readErr}
	fixture.cr.cs.mu.Unlock()
	controller, err := newSessionStartController(sessionStartControllerOptions{
		Workers:     1,
		MaxDistinct: 1,
		MaxRetries:  0,
		Reconcile:   func(context.Context, sessionStartAdmission) error { return nil },
	})
	if err != nil {
		t.Fatalf("create exact controller: %v", err)
	}
	t.Cleanup(controller.Stop)
	var stderr bytes.Buffer
	fixture.cr.stderr = &stderr
	fixture.cr.sessionStartController = controller
	fixture.cr.sessionStartOwnership = sessionStartOwnershipKeyed

	if reply := fixture.cr.admitSessionStartSocketKey(fixture.info.ID); reply != sessionStartSocketReplyFallback {
		t.Fatalf("socket reply = %q, want fallback", reply)
	}
	if !strings.Contains(stderr.String(), readErr.Error()) {
		t.Fatalf("socket fallback diagnostic = %q, want %q", stderr.String(), readErr)
	}
}

func TestCityRuntimeSocketRequireRefusesDrainAckAdmissionUncertainty(t *testing.T) {
	fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
	readErr := errors.New("runtime metadata unavailable")
	fixture.cr.cs.mu.Lock()
	fixture.cr.cs.sp = poolDrainAckGetMetaErrorProvider{Provider: fixture.provider, err: readErr}
	fixture.cr.cs.mu.Unlock()
	controller, err := newSessionStartController(sessionStartControllerOptions{
		Workers:     1,
		MaxDistinct: 1,
		MaxRetries:  0,
		Reconcile:   func(context.Context, sessionStartAdmission) error { return nil },
	})
	if err != nil {
		t.Fatalf("create exact controller: %v", err)
	}
	t.Cleanup(controller.Stop)
	var stderr bytes.Buffer
	fixture.cr.stderr = &stderr
	fixture.cr.sessionStartController = controller
	fixture.cr.sessionStartOwnership = sessionStartOwnershipKeyed
	fixture.cr.sessionStartMode = rollout.Require

	if reply := fixture.cr.admitSessionStartSocketKey(fixture.info.ID); reply != sessionStartSocketReplyBlocked {
		t.Fatalf("socket reply = %q, want blocked", reply)
	}
	if !strings.Contains(stderr.String(), readErr.Error()) {
		t.Fatalf("socket refusal diagnostic = %q, want %q", stderr.String(), readErr)
	}
}

func TestSessionStartLegacyExclusionRequireRetainsAgentDrainAckAfterAdmissionEnds(t *testing.T) {
	fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
	fixture.cr.sessionStartOwnership = sessionStartOwnershipKeyed
	fixture.cr.sessionStartMode = rollout.Require
	fixture.cr.sessionStartController = nil
	excluded := fixture.cr.sessionStartLegacyExclusionPredicate()
	if excluded == nil || !excluded(fixture.info) {
		t.Fatal("require mode allowed legacy drain-ack entry after exact admission ended")
	}
}

func TestSessionStartLegacyExclusionLeavesConfirmedLegacyDrainAckOwned(t *testing.T) {
	fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
	if err := fixture.store.SetMetadataBatch(fixture.info.ID, sessionpkg.DrainAckStopPendingPatch(time.Now().UTC())); err != nil {
		t.Fatalf("mark legacy drain acknowledgement stop-pending: %v", err)
	}
	if err := fixture.provider.SetMeta(fixture.info.SessionName, reconcilerDrainAckSourceKey, reconcilerDrainAckSourceValue); err != nil {
		t.Fatalf("mark reconciler-authored acknowledgement: %v", err)
	}
	info, err := sessionFrontDoor(fixture.store).Get(fixture.info.ID)
	if err != nil {
		t.Fatalf("read legacy stop-pending session: %v", err)
	}
	fixture.cr.sessionStartOwnership = sessionStartOwnershipKeyed
	fixture.cr.sessionStartController = nil
	for _, mode := range []rollout.Mode{rollout.Auto, rollout.Require} {
		fixture.cr.sessionStartMode = mode
		excluded := fixture.cr.sessionStartLegacyExclusionPredicate()
		if excluded == nil || excluded(info) {
			t.Fatalf("%s mode excluded a confirmed reconciler-authored stop-pending row", mode)
		}
	}
}

func TestReconcileExactSessionStartKeepsOrdinaryPoolRowsLegacyOwned(t *testing.T) {
	fixture := newRoutedWorkPoolAuthorizationFixture(t)

	owner, err := reconcileExactSessionStartWithOwner(t.Context(), sessionStartAdmission{
		SessionID: fixture.info.ID,
		Source:    sessionStartAdmissionInProcess,
	}, exactSessionStartParams{
		Generation: fixture.snapshot.Generation,
		CityPath:   fixture.snapshot.CityPath,
		CityName:   fixture.snapshot.CityName,
		Config:     fixture.snapshot.Config,
		Provider:   fixture.snapshot.Provider,
		Store:      fixture.snapshot.Store,
		Recorder:   events.Discard,
		Stdout:     io.Discard,
		Stderr:     io.Discard,
	})
	if err != nil {
		t.Fatalf("reconcile ordinary pool row: %v", err)
	}
	if owner != exactSessionStartLegacyOwner {
		t.Fatalf("ordinary pool row owner = %v, want legacy", owner)
	}
	if fixture.provider.IsRunning(fixture.info.SessionName) {
		t.Fatal("ordinary pool row started without allocation authority")
	}
	current, _, err := sessionFrontDoor(fixture.store).GetPersistedResponse(fixture.info.ID)
	if err != nil {
		t.Fatalf("read ordinary pool row: %v", err)
	}
	if current.LastWokeAt != "" || current.InstanceToken != fixture.info.InstanceToken {
		t.Fatalf("ordinary pool row mutated without authority: before=%+v after=%+v", fixture.info, current)
	}
}

type routedWorkPoolAuthorizationFixture struct {
	t        *testing.T
	cr       *CityRuntime
	store    beads.Store
	provider *runtime.Fake
	snapshot controllerSessionStartSnapshot
	work     beads.Bead
	info     sessionpkg.Info
	lease    routedWorkPoolStartLease
	// workStore holds the trigger work. It is store for a city-scoped pool and
	// the rig's own store when the fixture is rig-scoped.
	workStore   beads.Store
	template    string
	sourceStore string
}

type exactPoolRecoveryPostPreWakeHookStore struct {
	beads.Store
	sessionID     string
	originalToken string
	after         func(beads.Bead)
	once          sync.Once
}

func (s *exactPoolRecoveryPostPreWakeHookStore) Get(id string) (beads.Bead, error) {
	bead, err := s.Store.Get(id)
	if err == nil && id == s.sessionID && bead.Metadata["instance_token"] != "" &&
		bead.Metadata["instance_token"] != s.originalToken && bead.Metadata["last_woke_at"] != "" && s.after != nil {
		s.once.Do(func() { s.after(bead) })
	}
	return bead, err
}

func (s *exactPoolRecoveryPostPreWakeHookStore) ConditionalWritesResolveTarget() beads.Store {
	return s.Store
}

func newExactPoolRecoveryReconcileFixture(
	t *testing.T,
	mode rollout.Mode,
) (routedWorkPoolAuthorizationFixture, sessionStartAdmission, exactSessionStartParams, sessionpkg.PersistedResponse) {
	t.Helper()
	fixture := newRoutedWorkPoolAuthorizationFixture(t)
	fixture.snapshot.Config.Agents[0].WorkDir = fixture.snapshot.CityPath
	if err := fixture.store.SetMetadataBatch(fixture.info.ID, map[string]string{
		"state":                     string(sessionpkg.StateActive),
		"pending_create_claim":      "",
		"pending_create_started_at": "",
	}); err != nil {
		t.Fatalf("mark exact pool allocation active: %v", err)
	}
	info, persisted, err := getAuthoritativeSessionStartPersistedRecord(fixture.store, fixture.info.ID)
	if err != nil {
		t.Fatalf("read exact active recovery row: %v", err)
	}
	fixture.info = info
	if err := fixture.cr.poolMembershipShadow.replace(fixture.snapshot.Config, info); err != nil {
		t.Fatalf("publish exact active recovery membership: %v", err)
	}
	provider := &sequenceGetMetaProvider{Fake: fixture.provider}
	fixture.snapshot.Provider = provider
	lease, err := fixture.cr.newRoutedWorkPoolRecoveryLease(fixture.snapshot, info, persisted, routedWorkPoolAllocationHint{
		WorkID:      fixture.work.ID,
		PoolTarget:  "worker",
		SourceStore: "city:test-city",
	})
	if err != nil {
		t.Fatalf("create exact pool recovery lease: %v", err)
	}
	writer, ok := beads.ConditionalWriterFor(fixture.store)
	if !ok {
		t.Fatal("test store lacks conditional writer")
	}
	params := exactSessionStartParams{
		Generation:   fixture.snapshot.Generation,
		CityPath:     fixture.snapshot.CityPath,
		CityName:     fixture.snapshot.CityName,
		Config:       fixture.snapshot.Config,
		Provider:     provider,
		Store:        fixture.snapshot.Store,
		StatusWriter: writer,
		Recorder:     events.Discard,
		Stdout:       io.Discard,
		Stderr:       io.Discard,
		RolloutMode:  mode,
		AuthorizePoolStart: func(ctx context.Context, current sessionpkg.Info, candidate routedWorkPoolStartLease) (bool, error) {
			return fixture.cr.authorizeRoutedWorkPoolStart(ctx, fixture.snapshot, current, candidate)
		},
		StartOptions: []startExecutionOption{
			withStartStabilityWaiter(immediateStartStabilityWaiter),
			withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
		},
	}
	return fixture, sessionStartAdmission{
		SessionID:      info.ID,
		Source:         sessionStartAdmissionInProcess,
		PoolAllocation: &lease,
	}, params, persisted
}

type routedWorkPoolDrainAckAuthorizationFixture struct {
	*routedWorkPoolAuthorizationFixture
	lease routedWorkPoolDrainAckLease
}

type poolDrainAckProviderWithoutInteraction struct {
	runtime.Provider
}

type poolDrainAckGetMetaErrorProvider struct {
	runtime.Provider
	err error
}

type poolDrainAckAdmissionReadRejectStore struct {
	beads.Store
	reads int
}

func (s *poolDrainAckAdmissionReadRejectStore) Get(string) (beads.Bead, error) {
	s.reads++
	return beads.Bead{}, errors.New("work-store read attempted during admission")
}

func (s *poolDrainAckAdmissionReadRejectStore) List(beads.ListQuery) ([]beads.Bead, error) {
	s.reads++
	return nil, errors.New("work-store list attempted during admission")
}

func (s *poolDrainAckAdmissionReadRejectStore) Ready(...beads.ReadyQuery) ([]beads.Bead, error) {
	s.reads++
	return nil, errors.New("work-store ready scan attempted during admission")
}

func (s *poolDrainAckAdmissionReadRejectStore) DepList(string, string) ([]beads.Dep, error) {
	s.reads++
	return nil, errors.New("work-store dependency scan attempted during admission")
}

func (s *poolDrainAckAdmissionReadRejectStore) Handles() beads.StoreHandles {
	return beads.StoreHandles{Cached: s, Live: s, Writer: s.Store}
}

func (p poolDrainAckGetMetaErrorProvider) GetMeta(string, string) (string, error) {
	return "", p.err
}

func newRoutedWorkPoolDrainAckAuthorizationFixture(t *testing.T) routedWorkPoolDrainAckAuthorizationFixture {
	t.Helper()
	return newRoutedWorkPoolDrainAckAuthorizationFixtureWithOptions(t, routedWorkPoolAuthorizationFixtureOptions{})
}

func newRoutedWorkPoolDrainAckAuthorizationFixtureWithOptions(
	t *testing.T,
	options routedWorkPoolAuthorizationFixtureOptions,
) routedWorkPoolDrainAckAuthorizationFixture {
	t.Helper()
	base := newRoutedWorkPoolAuthorizationFixtureWithOptions(t, beads.NewAtomicCloseMemStore(), options)
	if err := base.provider.Start(t.Context(), base.info.SessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start pool runtime: %v", err)
	}
	// Exactly what `gc runtime drain-ack` stamps on a pool member today: the
	// four ack keys, GC_DRAIN_ACK, and the ack-time trigger pair read verbatim
	// off the member row (providerDrainOps.setDrainAck / stampDrainAckTrigger).
	for key, value := range map[string]string{
		"GC_SESSION_ID":                      base.info.ID,
		"GC_INSTANCE_TOKEN":                  base.info.InstanceToken,
		reconcilerDrainAckSourceKey:          drainAckSourceAgentValue,
		drainAckRequesterSessionIDKey:        base.info.ID,
		drainAckRequesterInstanceTokenKey:    base.info.InstanceToken,
		reconcilerDrainAckTriggerBeadIDKey:   base.work.ID,
		reconcilerDrainAckTriggerStoreRefKey: base.sourceStore,
		"GC_DRAIN_ACK":                       "1",
	} {
		if err := base.provider.SetMeta(base.info.SessionName, key, value); err != nil {
			t.Fatalf("set runtime metadata %s: %v", key, err)
		}
	}
	if err := base.store.SetMetadataBatch(base.info.ID, map[string]string{
		"state":                     string(sessionpkg.StateActive),
		"pending_create_claim":      "",
		"pending_create_started_at": "",
	}); err != nil {
		t.Fatalf("mark pool session active: %v", err)
	}
	if err := base.workStore.Close(base.work.ID); err != nil {
		t.Fatalf("close trigger work: %v", err)
	}
	info, err := sessionFrontDoor(base.store).Get(base.info.ID)
	if err != nil {
		t.Fatalf("read active pool session: %v", err)
	}
	base.info = info
	if err := base.cr.poolMembershipShadow.replace(base.snapshot.Config, info); err != nil {
		t.Fatalf("publish active pool membership: %v", err)
	}
	observation, occupied := base.cr.poolMembershipShadow.observeOccupiedMember(base.template, info.ID)
	if !occupied {
		t.Fatal("active pool session is not an occupied member")
	}
	lease := routedWorkPoolDrainAckLease{
		SessionID:              info.ID,
		InstanceToken:          info.InstanceToken,
		RequesterSessionID:     info.ID,
		RequesterInstanceToken: info.InstanceToken,
		ControllerGeneration:   base.snapshot.Generation,
		PoolTarget:             base.template,
		WorkID:                 base.work.ID,
		SourceStore:            base.sourceStore,
		MembershipRevision:     observation.revision,
		// This fixture's member IS keyed-occupied (asserted just above), so its
		// lease says so and keeps the membership monotonicity fence live. A
		// legacy-created member carries MembershipOccupied=false and is fenced
		// by its ack stamps and row binding instead — see
		// TestAuthorizeRoutedWorkPoolDrainAckHoldsLegacyCreatedMember.
		MembershipOccupied: true,
		TriggerFromAck:     true,
	}
	authorized, _, err := base.cr.authorizeRoutedWorkPoolDrainAck(base.snapshot, info, lease)
	if err != nil || !authorized {
		t.Fatalf("baseline drain acknowledgement authorization = (%t, %v), want true", authorized, err)
	}
	enableDrainAckAtomicCloseForFixture(&base)
	return routedWorkPoolDrainAckAuthorizationFixture{
		routedWorkPoolAuthorizationFixture: &base,
		lease:                              lease,
	}
}

func newRoutedWorkPoolAuthorizationFixture(t *testing.T) routedWorkPoolAuthorizationFixture {
	return newRoutedWorkPoolAuthorizationFixtureWithStore(t, beads.NewMemStore())
}

func newRoutedWorkPoolAuthorizationFixtureWithStore(t *testing.T, store beads.Store) routedWorkPoolAuthorizationFixture {
	t.Helper()
	return newRoutedWorkPoolAuthorizationFixtureWithOptions(t, store, routedWorkPoolAuthorizationFixtureOptions{})
}

// routedWorkPoolAuthorizationFixtureOptions scopes the fixture's pool agent to a
// configured rig. The session row stays in the city store (where session beads
// live) while the trigger work moves to the rig's own store, which is the real
// cross-store shape a rig-scoped pool member has.
type routedWorkPoolAuthorizationFixtureOptions struct {
	rigName string
}

func newRoutedWorkPoolAuthorizationFixtureWithOptions(
	t *testing.T,
	store beads.Store,
	options routedWorkPoolAuthorizationFixtureOptions,
) routedWorkPoolAuthorizationFixture {
	t.Helper()
	unlimited := -1
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city", Prefix: "hq"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "true",
			MaxActiveSessions: &unlimited,
		}},
	}
	workStore := store
	sourceStore := "city:test-city"
	rigStores := map[string]beads.Store{}
	if options.rigName != "" {
		cfg.Rigs = []config.Rig{{Name: options.rigName, Path: filepath.Join(cityPath, "rigs", options.rigName)}}
		cfg.Agents[0].Dir = options.rigName
		workStore = beads.NewMemStore()
		rigStores[options.rigName] = workStore
		sourceStore = "rig:" + options.rigName
	}
	template := cfg.Agents[0].QualifiedName()
	provider := runtime.NewFake()
	cs := coherentSessionStartControllerStateForTest(cfg, provider, store, rollout.Auto)
	cs.cityPath = cityPath
	cs.cityName = "test-city"
	cs.beadStores = rigStores
	cr := &CityRuntime{
		cityPath:             cityPath,
		cityName:             "test-city",
		cfg:                  cfg,
		sp:                   provider,
		cs:                   cs,
		rec:                  events.Discard,
		poolMembershipShadow: newPoolMembershipIndex(),
		stdout:               io.Discard,
		stderr:               io.Discard,
	}
	if !cr.poolMembershipShadow.publishRebuild(0, newPoolMembershipState()) {
		t.Fatal("publish empty pool membership")
	}
	work, err := workStore.Create(beads.Bead{
		Title:  "ready routed work",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.routed_to": template,
		},
	})
	if err != nil {
		t.Fatalf("create routed work: %v", err)
	}
	hint := routedWorkPoolAllocationHint{
		WorkID:      work.ID,
		PoolTarget:  template,
		SourceStore: sourceStore,
	}
	info, err := createPoolSessionBeadWithAlias(store, template, cfg, nil, time.Now().UTC(), poolSessionCreateIdentity{
		AgentName: cfg.Agents[0].QualifiedInstanceName("worker-1"),
		Slot:      1,
		Metadata: map[string]string{
			beadmeta.TriggerBeadIDMetadataKey:       work.ID,
			beadmeta.TriggerBeadStoreRefMetadataKey: hint.SourceStore,
		},
	}, "")
	if err != nil {
		t.Fatalf("create pool session: %v", err)
	}
	if err := cr.poolMembershipShadow.replace(cfg, info); err != nil {
		t.Fatalf("publish pool session membership: %v", err)
	}
	snapshot, release, err := cs.acquireSessionStartSnapshot()
	if err != nil {
		t.Fatalf("acquire start snapshot: %v", err)
	}
	t.Cleanup(release)
	lease, err := cr.newRoutedWorkPoolStartLease(snapshot, info, hint)
	if err != nil {
		t.Fatalf("create pool start lease: %v", err)
	}
	authorized, err := cr.authorizeRoutedWorkPoolStart(t.Context(), snapshot, info, lease)
	if err != nil || !authorized {
		t.Fatalf("baseline pool start authorization = (%t, %v), want true", authorized, err)
	}
	return routedWorkPoolAuthorizationFixture{
		t: t, cr: cr, store: store, workStore: workStore, provider: provider, snapshot: snapshot,
		work: work, info: info, lease: lease, template: template, sourceStore: sourceStore,
	}
}

type routedWorkPoolAllocationFixture struct {
	cr       *CityRuntime
	store    beads.Store
	provider *runtime.Fake
	stderr   *bytes.Buffer
}

type fixedFreshLivenessProvider struct {
	runtime.Provider
	observation runtime.Liveness
}

func (p *fixedFreshLivenessProvider) ObserveFreshLiveness(runtime.LivenessTarget) runtime.Liveness {
	return p.observation
}

type triggerMatchingReadHookStore struct {
	beads.Store
	sessionID string
	workID    string
	after     func()
	once      sync.Once
}

func (s *triggerMatchingReadHookStore) Get(id string) (beads.Bead, error) {
	row, err := s.Store.Get(id)
	if err == nil && id == s.sessionID && row.Metadata[beadmeta.TriggerBeadIDMetadataKey] == s.workID {
		s.once.Do(s.after)
	}
	return row, err
}

func (s *triggerMatchingReadHookStore) ConditionalWritesResolveTarget() beads.Store {
	return s.Store
}

type poolReusePersistedReadHookStore struct {
	beads.Store
	sessionID    string
	workID       string
	beforeReturn func()
	zeroRevision *atomic.Bool
	once         sync.Once
}

func (s *poolReusePersistedReadHookStore) Get(id string) (beads.Bead, error) {
	row, err := s.Store.Get(id)
	if err != nil || id != s.sessionID || row.Metadata[beadmeta.TriggerBeadIDMetadataKey] != s.workID {
		return row, err
	}
	if s.beforeReturn != nil {
		s.once.Do(s.beforeReturn)
		row, err = s.Store.Get(id)
		if err != nil {
			return row, err
		}
	}
	if s.zeroRevision != nil && s.zeroRevision.Load() {
		row.Revision = 0
	}
	return row, nil
}

func (s *poolReusePersistedReadHookStore) ConditionalWritesResolveTarget() beads.Store {
	return s.Store
}

type poolReuseGetMetaHookProvider struct {
	*sequenceGetMetaProvider
	after func()
	once  sync.Once
}

func (p *poolReuseGetMetaHookProvider) GetMeta(name, key string) (string, error) {
	value, err := p.sequenceGetMetaProvider.GetMeta(name, key)
	if err == nil && key == "GC_INSTANCE_TOKEN" && p.after != nil {
		p.once.Do(p.after)
	}
	return value, err
}

type poolReuseAssignedListHookStore struct {
	beads.Store
	after func()
	once  sync.Once
}

func (s *poolReuseAssignedListHookStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.Store.List(query)
	if err == nil && len(rows) > 0 && (query.Assignee != "" || len(query.Assignees) > 0) && s.after != nil {
		s.once.Do(s.after)
	}
	return rows, err
}

func (s *poolReuseAssignedListHookStore) ConditionalWritesResolveTarget() beads.Store {
	return s.Store
}

type poolReuseAssignedListErrorStore struct {
	beads.Store
	err error
}

func (s *poolReuseAssignedListErrorStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Assignee != "" || len(query.Assignees) > 0 {
		return nil, s.err
	}
	return s.Store.List(query)
}

func (s *poolReuseAssignedListErrorStore) ConditionalWritesResolveTarget() beads.Store {
	return s.Store
}

type poolReuseNoFencedProvider struct {
	runtime.Provider
	fake *runtime.Fake
}

func (p *poolReuseNoFencedProvider) Pending(name string) (*runtime.PendingInteraction, error) {
	return p.fake.Pending(name)
}

func (p *poolReuseNoFencedProvider) Respond(name string, response runtime.InteractionResponse) error {
	return p.fake.Respond(name, response)
}

func (p *poolReuseNoFencedProvider) WaitForIdle(ctx context.Context, name string, timeout time.Duration) error {
	return p.fake.WaitForIdle(ctx, name, timeout)
}

type poolReuseListCountingStore struct {
	beads.Store
	mu      sync.Mutex
	queries []beads.ListQuery
}

func (s *poolReuseListCountingStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Assignee != "" || len(query.Assignees) > 0 {
		s.mu.Lock()
		s.queries = append(s.queries, query)
		s.mu.Unlock()
	}
	return s.Store.List(query)
}

func (s *poolReuseListCountingStore) ConditionalWritesResolveTarget() beads.Store {
	return s.Store
}

func (s *poolReuseListCountingStore) snapshot() []beads.ListQuery {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]beads.ListQuery(nil), s.queries...)
}

func newRoutedWorkPoolAllocationFixture(t *testing.T, store beads.Store) routedWorkPoolAllocationFixture {
	t.Helper()
	unlimited := -1
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city", Prefix: "hq"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "true",
			MaxActiveSessions: &unlimited,
		}},
	}
	provider := runtime.NewFake()
	sessionProvider := &sequenceGetMetaProvider{Fake: provider}
	stderr := &bytes.Buffer{}
	cs := coherentSessionStartControllerStateForTest(cfg, sessionProvider, store, rollout.Auto)
	cs.cityPath = cityPath
	cs.cityName = "test-city"
	cr := &CityRuntime{
		cityPath:             cityPath,
		cityName:             "test-city",
		cfg:                  cfg,
		sp:                   sessionProvider,
		cs:                   cs,
		rec:                  events.Discard,
		poolMembershipShadow: newPoolMembershipIndex(),
		pokeCh:               make(chan struct{}, 1),
		sessionStartOptions: []startExecutionOption{
			withStartStabilityWaiter(immediateStartStabilityWaiter),
		},
		stdout: io.Discard,
		stderr: stderr,
	}
	if !cr.poolMembershipShadow.publishRebuild(0, newPoolMembershipState()) {
		t.Fatal("publish empty pool membership")
	}
	if err := cr.ensureSessionStartController(context.Background(), newSessionBeadSnapshotFromInfos(nil)); err != nil {
		t.Fatalf("ensure exact-start controller: %v", err)
	}
	t.Cleanup(cr.stopSessionStartController)
	return routedWorkPoolAllocationFixture{cr: cr, store: store, provider: provider, stderr: stderr}
}

func prepareIdleCanonicalSingletonForReuse(t *testing.T, conditional bool) (routedWorkPoolAllocationFixture, beads.Bead, sessionpkg.Info) {
	return prepareIdleGenericPoolMemberForReuse(t, conditional, 1)
}

func prepareIdleGenericPoolMemberForReuse(t *testing.T, conditional bool, maximum int) (routedWorkPoolAllocationFixture, beads.Bead, sessionpkg.Info) {
	t.Helper()
	var store beads.Store = beads.NewMemStore()
	if conditional {
		opened, err := beads.OpenStoreAtForCity(t.Context(), beads.StoreOpenOptions{
			Provider:          "file",
			OpenFileStore:     func() (beads.Store, error) { return beads.NewMemStore(), nil },
			ConditionalWrites: gate.Auto,
		})
		if err != nil {
			t.Fatalf("open conditional-write singleton store: %v", err)
		}
		store = opened.Store
	}
	return prepareIdleGenericPoolMemberForReuseWithStore(t, store, maximum)
}

// prepareIdleGenericPoolMemberForReuseWithStore is prepareIdleGenericPoolMember
// ForReuse over a caller-supplied store, so a fixture can vary the backend's
// revision minting without duplicating the cold-start choreography.
func prepareIdleGenericPoolMemberForReuseWithStore(t *testing.T, store beads.Store, maximum int) (routedWorkPoolAllocationFixture, beads.Bead, sessionpkg.Info) {
	t.Helper()
	fixture := newRoutedWorkPoolAllocationFixture(t, store)
	fixture.cr.cfg.Agents[0].MaxActiveSessions = &maximum
	fixture.cr.cfg.Agents[0].Provider = "claude"
	fixture.cr.cfg.Agents[0].Nudge = "Run gc hook --claim --json now."
	firstWork, err := fixture.store.Create(beads.Bead{
		Title:    "first singleton work",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create first singleton work: %v", err)
	}
	first, err := fixture.cr.reconcileRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{
		WorkID: firstWork.ID, PoolTarget: "worker", SourceStore: "city:test-city",
	})
	if err != nil || !first.Handled || !first.Created {
		t.Fatalf("allocate cold singleton = (%+v, %v), want one create", first, err)
	}
	awaitCond(t, func() bool {
		return fixture.provider.IsRunning(first.Session.SessionName) && fixture.cr.sessionStartController.Pending() == 0
	}, "cold singleton to start before reuse refusal")
	info, err := sessionFrontDoor(fixture.store).Get(first.Session.ID)
	if err != nil {
		t.Fatalf("read active singleton: %v", err)
	}
	setRoutedWorkPoolRuntimeIdentity(t, fixture, info)
	fixture.provider.WaitForIdleErrors[info.SessionNameMetadata] = nil
	return fixture, firstWork, info
}

func prepareTwoMemberGenericPoolForReuse(t *testing.T, maximum int) (routedWorkPoolAllocationFixture, beads.Bead, sessionpkg.Info, beads.Bead, sessionpkg.Info) {
	t.Helper()
	fixture, firstWork, first := prepareIdleGenericPoolMemberForReuse(t, true, maximum)
	secondWork, err := fixture.store.Create(beads.Bead{Title: "second pool work", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "worker"}})
	if err != nil {
		t.Fatalf("create second pool work: %v", err)
	}
	second, err := fixture.cr.reconcileRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{WorkID: secondWork.ID, PoolTarget: "worker", SourceStore: "city:test-city"})
	if err != nil || !second.Handled || !second.Created || second.Session.PoolSlot != "2" {
		t.Fatalf("grow second pool member = (%+v, %v), want created slot 2", second, err)
	}
	awaitCond(t, func() bool {
		return fixture.provider.IsRunning(second.Session.SessionName) && fixture.cr.sessionStartController.Pending() == 0
	}, "second pool member to start")
	secondInfo, err := sessionFrontDoor(fixture.store).Get(second.Session.ID)
	if err != nil {
		t.Fatalf("read second pool member: %v", err)
	}
	setRoutedWorkPoolRuntimeIdentity(t, fixture, secondInfo)
	fixture.provider.WaitForIdleErrors[secondInfo.SessionNameMetadata] = nil
	return fixture, firstWork, first, secondWork, secondInfo
}

func setRoutedWorkPoolRuntimeIdentity(t testing.TB, fixture routedWorkPoolAllocationFixture, info sessionpkg.Info) {
	t.Helper()
	// The wait-idle worker path resolves its provider family from the durable
	// session record. Production rows have this after provider resolution; the
	// direct keyed-start fixture bypasses that legacy metadata-refresh pass.
	if err := fixture.store.SetMetadata(info.ID, "provider_kind", "claude"); err != nil {
		t.Fatalf("stamp singleton provider family: %v", err)
	}
	for key, value := range map[string]string{
		"GC_SESSION_ID":     info.ID,
		"GC_INSTANCE_TOKEN": info.InstanceToken,
	} {
		if err := fixture.provider.SetMeta(info.SessionNameMetadata, key, value); err != nil {
			t.Fatalf("set singleton runtime metadata %s: %v", key, err)
		}
	}
}

// assertRoutedWorkPoolAllocationCensusOwed pins Q2's resolution: a routed-work
// key the keyed allocation could not handle is NOT converted into a legacy poke.
// Recovery is re-detection by the next patrol's declared routed-work view, so the
// legacy pool builder never sees the key and only discovery latency is lost.
func assertRoutedWorkPoolAllocationCensusOwed(t *testing.T, cr *CityRuntime) {
	t.Helper()
	if len(cr.pokeCh) != 0 {
		t.Fatalf("legacy fallback pokes after an unhandled routed-work allocation = %d, want none: recovery is census-owed re-detection", len(cr.pokeCh))
	}
}

func providerCallCount(provider *runtime.Fake, method string) int {
	count := 0
	for _, call := range provider.SnapshotCalls() {
		if call.Method == method {
			count++
		}
	}
	return count
}

func providerNudgeCalls(provider *runtime.Fake, name string) int {
	return provider.CountCalls("Nudge", name) + provider.CountCalls("NudgeNow", name) + provider.CountCalls("NudgeFenced", name)
}

func providerAllNudgeCalls(provider *runtime.Fake) int {
	return providerCallCount(provider, "Nudge") + providerCallCount(provider, "NudgeNow") + providerCallCount(provider, "NudgeFenced")
}

func assertExactProviderNudgeSince(t *testing.T, provider *runtime.Fake, baseline int, name, message string) {
	t.Helper()
	calls := provider.SnapshotCalls()
	if baseline > len(calls) {
		t.Fatalf("provider call baseline = %d, want at most %d", baseline, len(calls))
	}
	var nudges []runtime.Call
	for _, call := range calls[baseline:] {
		if call.Method == "Nudge" || call.Method == "NudgeNow" || call.Method == "NudgeFenced" {
			nudges = append(nudges, call)
		}
	}
	if len(nudges) != 1 || nudges[0].Name != name || nudges[0].Message != message {
		t.Fatalf("provider nudges since reuse = %+v, want exactly one nudge to %q with %q", nudges, name, message)
	}
}

func assertNoPoolAllocationSession(t *testing.T, store beads.Store) {
	t.Helper()
	snapshot, err := loadSessionBeadSnapshot(store)
	if err != nil {
		t.Fatalf("load sessions: %v", err)
	}
	if got := len(snapshot.OpenInfos()); got != 0 {
		t.Fatalf("open sessions = %d, want 0", got)
	}
}

func createRoutedWorkPoolBinding(t *testing.T, store beads.Store, cfg *config.City, hint routedWorkPoolAllocationHint, slot int) sessionpkg.Info {
	t.Helper()
	info, err := createPoolSessionBeadWithAlias(store, hint.PoolTarget, cfg, nil, time.Now().UTC(), poolSessionCreateIdentity{
		AgentName: fmt.Sprintf("%s-%d", hint.PoolTarget, slot),
		Slot:      slot,
		Metadata: map[string]string{
			beadmeta.TriggerBeadIDMetadataKey:       hint.WorkID,
			beadmeta.TriggerBeadStoreRefMetadataKey: hint.SourceStore,
		},
	}, "")
	if err != nil {
		t.Fatalf("create slot-%d routed-work binding: %v", slot, err)
	}
	return info
}

type poolAllocationDepListErrorStore struct {
	beads.Store
	err error
}

func (s *poolAllocationDepListErrorStore) DepList(string, string) ([]beads.Dep, error) {
	return nil, s.err
}

type poolAllocationFailSessionCreateStore struct {
	beads.Store
	fail atomic.Bool
}

type poolAllocationStartCommitBarrierStore struct {
	beads.Store
	provider    *runtime.Fake
	sessionID   string
	sessionName string
	entered     chan struct{}
	release     chan struct{}
	once        sync.Once
}

func (s *poolAllocationStartCommitBarrierStore) Get(id string) (beads.Bead, error) {
	if id == s.sessionID && s.provider.IsRunning(s.sessionName) {
		s.once.Do(func() {
			close(s.entered)
			<-s.release
		})
	}
	return s.Store.Get(id)
}

func (s *poolAllocationFailSessionCreateStore) Create(bead beads.Bead) (beads.Bead, error) {
	if s.fail.Load() && bead.Type == sessionpkg.BeadType {
		return beads.Bead{}, errors.New("session create unavailable")
	}
	return s.Store.Create(bead)
}

// TestValidateRoutedWorkPoolStartLeaseAcceptsASignedRecoveryRevision is the
// ga-f7v2ft.141 red for the recovery lease. SessionRevision is copied verbatim
// off persisted.Revision and is only ever tested for equality against a later
// re-read, so it must be admitted whenever it is KNOWN. Gating it on `> 0`
// refused every recovery admission on the negative half of every city's bd rows
// — the active-runtime recovery start could not be certified at all there.
func TestValidateRoutedWorkPoolStartLeaseAcceptsASignedRecoveryRevision(t *testing.T) {
	base := routedWorkPoolStartLease{
		SessionID:            "gcs-pool1",
		InstanceToken:        "instance-token",
		ControllerGeneration: 7,
		PoolTarget:           "worker",
		WorkID:               "ga-work",
		SourceStore:          "city:test-city",
		MembershipRevision:   11,
		RecoverActive:        true,
	}
	for _, tc := range []struct {
		name     string
		revision int64
		wantErr  bool
	}{
		{"positive", 5434260017027113294, false},
		{"negative", -1655629893108404930, false},
		{"unavailable", 0, true},
	} {
		lease := base
		lease.SessionRevision = tc.revision
		err := validateRoutedWorkPoolStartLease(lease)
		if gotErr := err != nil; gotErr != tc.wantErr {
			t.Errorf("validateRoutedWorkPoolStartLease(revision %d) [%s] = %v, wantErr %v", tc.revision, tc.name, err, tc.wantErr)
		}
	}
}
