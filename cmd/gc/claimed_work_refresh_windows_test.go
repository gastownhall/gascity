//go:build windows

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestBuildDesiredStateDrainAckRefreshesClosedClaimBeforePoolDemand(t *testing.T) {
	backing := beads.NewMemStore()
	var events []string
	store := beads.NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})
	sessionBead := seedDrainRefreshSession(t, backing, "session-worker", "worker-1")
	workBead := seedDrainRefreshWork(t, backing, "gc-claim", "worker-1")
	if err := store.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if err := backing.Close(workBead.ID); err != nil {
		t.Fatalf("foreign close: %v", err)
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker-1", runtime.Config{}); err != nil {
		t.Fatalf("Start fake session: %v", err)
	}
	if err := sp.SetMeta("worker-1", "GC_DRAIN_ACK", "1"); err != nil {
		t.Fatalf("SetMeta drain ack: %v", err)
	}

	cfg := drainRefreshConfig()
	var stderr strings.Builder
	result := buildDesiredStateWithSessionBeads(
		"test-city",
		t.TempDir(),
		time.Now().UTC(),
		cfg,
		sp,
		store,
		nil,
		newSessionBeadSnapshot([]beads.Bead{sessionBead}),
		nil,
		&stderr,
	)

	if containsBeadID(result.AssignedWorkBeads, workBead.ID) {
		t.Fatalf("AssignedWorkBeads still contains closed claim %s; stale claim would feed pool demand: %#v stderr=%s", workBead.ID, result.AssignedWorkBeads, stderr.String())
	}
	poolDesired := PoolDesiredCounts(ComputePoolDesiredStates(cfg, result.AssignedWorkBeads, newSessionBeadSnapshot([]beads.Bead{sessionBead}).OpenInfos(), result.ScaleCheckCounts))
	if got := poolDesired["worker"]; got != 0 {
		t.Fatalf("poolDesired[worker] = %d, want 0 after drain-ack claim refresh; assigned=%#v stderr=%s", got, result.AssignedWorkBeads, stderr.String())
	}
	wantEvents := []string{"bead.closed:" + workBead.ID}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("events = %v, want %v", events, wantEvents)
	}
	if _, err := store.List(beads.ListQuery{Status: "open", Live: true, AllowScan: true}); err != nil {
		t.Fatalf("post-refresh live list: %v", err)
	}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("events after later live refresh = %v, want no duplicate beyond %v", events, wantEvents)
	}
}

func TestBuildDesiredStateDrainAckRefreshPreservesStillOpenClaimDemand(t *testing.T) {
	backing := beads.NewMemStore()
	store := beads.NewCachingStoreForTest(backing, nil)
	sessionBead := seedDrainRefreshSession(t, backing, "session-worker", "worker-1")
	workBead := seedDrainRefreshWork(t, backing, "gc-claim", "worker-1")
	if err := store.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker-1", runtime.Config{}); err != nil {
		t.Fatalf("Start fake session: %v", err)
	}
	if err := sp.SetMeta("worker-1", "GC_DRAIN_ACK", "1"); err != nil {
		t.Fatalf("SetMeta drain ack: %v", err)
	}

	cfg := drainRefreshConfig()
	result := buildDesiredStateWithSessionBeads(
		"test-city",
		t.TempDir(),
		time.Now().UTC(),
		cfg,
		sp,
		store,
		nil,
		newSessionBeadSnapshot([]beads.Bead{sessionBead}),
		nil,
		io.Discard,
	)

	if !containsBeadID(result.AssignedWorkBeads, workBead.ID) {
		t.Fatalf("AssignedWorkBeads = %#v, want still-open claim %s preserved", result.AssignedWorkBeads, workBead.ID)
	}
	poolDesired := PoolDesiredCounts(ComputePoolDesiredStates(cfg, result.AssignedWorkBeads, newSessionBeadSnapshot([]beads.Bead{sessionBead}).OpenInfos(), result.ScaleCheckCounts))
	if got := poolDesired["worker"]; got != 1 {
		t.Fatalf("poolDesired[worker] = %d, want 1 while backing still shows the claim open", got)
	}
}

func TestBuildDesiredStateExitedSessionRefreshesClosedClaimBeforePoolDemand(t *testing.T) {
	backing := beads.NewMemStore()
	store := beads.NewCachingStoreForTest(backing, nil)
	sessionBead := seedDrainRefreshSession(t, backing, "session-worker", "worker-1")
	workBead := seedDrainRefreshWork(t, backing, "gc-claim", "worker-1")
	if err := store.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if err := backing.Close(workBead.ID); err != nil {
		t.Fatalf("foreign close: %v", err)
	}
	sp := runtime.NewFake()

	result := buildDesiredStateWithSessionBeads(
		"test-city",
		t.TempDir(),
		time.Now().UTC(),
		drainRefreshConfig(),
		sp,
		store,
		nil,
		newSessionBeadSnapshot([]beads.Bead{sessionBead}),
		nil,
		io.Discard,
	)

	if containsBeadID(result.AssignedWorkBeads, workBead.ID) {
		t.Fatalf("AssignedWorkBeads still contains closed claim %s for exited session: %#v", workBead.ID, result.AssignedWorkBeads)
	}
}

func TestBuildDesiredStateClaimRefreshFailureFallsBackToCachedDemand(t *testing.T) {
	backing := &failingClaimRefreshStore{Store: beads.NewMemStore(), err: errors.New("store down")}
	store := beads.NewCachingStoreForTest(backing, nil)
	sessionBead := seedDrainRefreshSession(t, backing.Store, "session-worker", "worker-1")
	workBead := seedDrainRefreshWork(t, backing.Store, "gc-claim", "worker-1")
	if err := store.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if err := backing.Store.Close(workBead.ID); err != nil {
		t.Fatalf("foreign close: %v", err)
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker-1", runtime.Config{}); err != nil {
		t.Fatalf("Start fake session: %v", err)
	}
	if err := sp.SetMeta("worker-1", "GC_DRAIN_ACK", "1"); err != nil {
		t.Fatalf("SetMeta drain ack: %v", err)
	}

	var stderr strings.Builder
	result := buildDesiredStateWithSessionBeads(
		"test-city",
		t.TempDir(),
		time.Now().UTC(),
		drainRefreshConfig(),
		sp,
		store,
		nil,
		newSessionBeadSnapshot([]beads.Bead{sessionBead}),
		nil,
		&stderr,
	)

	if !containsBeadID(result.AssignedWorkBeads, workBead.ID) {
		t.Fatalf("AssignedWorkBeads = %#v, want cached claim retained on refresh failure", result.AssignedWorkBeads)
	}
	if !strings.Contains(stderr.String(), "claimed-work refresh: refreshing "+workBead.ID) || !strings.Contains(stderr.String(), "store down") {
		t.Fatalf("stderr %q does not report the refresh failure for %s", stderr.String(), workBead.ID)
	}
}

func TestBuildDesiredStateClaimRefreshTimeoutFallsBackPromptly(t *testing.T) {
	backing := &failingClaimRefreshStore{Store: beads.NewMemStore(), delay: 5 * time.Second}
	store := beads.NewCachingStoreForTest(backing, nil)
	sessionBead := seedDrainRefreshSession(t, backing.Store, "session-worker", "worker-1")
	workBead := seedDrainRefreshWork(t, backing.Store, "gc-claim", "worker-1")
	if err := store.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if err := backing.Store.Close(workBead.ID); err != nil {
		t.Fatalf("foreign close: %v", err)
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker-1", runtime.Config{}); err != nil {
		t.Fatalf("Start fake session: %v", err)
	}
	if err := sp.SetMeta("worker-1", "GC_DRAIN_ACK", "1"); err != nil {
		t.Fatalf("SetMeta drain ack: %v", err)
	}

	var stderr strings.Builder
	start := time.Now()
	result := buildDesiredStateWithSessionBeads(
		"test-city",
		t.TempDir(),
		time.Now().UTC(),
		drainRefreshConfig(),
		sp,
		store,
		nil,
		newSessionBeadSnapshot([]beads.Bead{sessionBead}),
		nil,
		&stderr,
	)
	elapsed := time.Since(start)

	if !containsBeadID(result.AssignedWorkBeads, workBead.ID) {
		t.Fatalf("AssignedWorkBeads = %#v, want cached claim retained on refresh timeout", result.AssignedWorkBeads)
	}
	if elapsed >= 3*time.Second {
		t.Fatalf("refresh took %s, want bounded below 3s", elapsed)
	}
	if !strings.Contains(stderr.String(), "claimed-work refresh timed out") {
		t.Fatalf("stderr %q does not report timeout", stderr.String())
	}
}

func TestClaimRefreshScopesSameIDByStoreWhenOneRefreshFails(t *testing.T) {
	sessionStore := beads.NewMemStore()
	sessionBead := seedDrainRefreshSession(t, sessionStore, "session-worker", "worker-1")

	closedStore := beads.NewMemStore()
	closedStore.HonorExplicitIDs = true
	openBacking := beads.NewMemStore()
	openBacking.HonorExplicitIDs = true
	openStore := &failingClaimRefreshStore{Store: openBacking, err: errors.New("rig store down")}

	closedClaim := seedDrainRefreshWork(t, closedStore, "gc-same", "worker-1")
	openClaim := seedDrainRefreshWork(t, openBacking, "gc-same", "worker-1")
	if err := closedStore.Close(closedClaim.ID); err != nil {
		t.Fatalf("foreign close: %v", err)
	}

	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker-1", runtime.Config{}); err != nil {
		t.Fatalf("Start fake session: %v", err)
	}
	if err := sp.SetMeta("worker-1", "GC_DRAIN_ACK", "1"); err != nil {
		t.Fatalf("SetMeta drain ack: %v", err)
	}

	work, stores, refs, ready := refreshDrainAckedOrExitedClaimedWorkForDemand(
		drainRefreshConfig(),
		sp,
		[]beads.Bead{closedClaim, openClaim},
		[]beads.Store{closedStore, openStore},
		[]string{"city", "rig:other"},
		map[storeScopedBeadKey]bool{
			{StoreRef: "city", ID: closedClaim.ID}:    true,
			{StoreRef: "rig:other", ID: openClaim.ID}: true,
		},
		newSessionBeadSnapshot([]beads.Bead{sessionBead}),
		io.Discard,
	)

	if len(work) != 1 || work[0].ID != openClaim.ID || work[0].Status != "in_progress" {
		t.Fatalf("refreshed work = %#v, want only the still-open rig claim", work)
	}
	if len(stores) != 1 || stores[0] != openStore {
		t.Fatalf("stores = %#v, want only the rig store", stores)
	}
	if len(refs) != 1 || refs[0] != "rig:other" {
		t.Fatalf("refs = %#v, want only rig:other", refs)
	}
	if !ready[storeScopedBeadKey{StoreRef: "rig:other", ID: openClaim.ID}] {
		t.Fatalf("ready = %#v, want rig claim readiness preserved", ready)
	}
	if ready[storeScopedBeadKey{StoreRef: "city", ID: closedClaim.ID}] {
		t.Fatalf("ready = %#v, want closed city claim removed", ready)
	}
}

func TestClaimRefreshKeepsUnassignedInProgressPoolWork(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead := seedDrainRefreshSession(t, beads.NewMemStore(), "session-worker", "worker-1")
	claim := seedDrainRefreshWork(t, store, "gc-claim", "worker-1")
	stranded := seedDrainRefreshWork(t, store, "gc-stranded", "")
	if !isRecoverableUnassignedInProgressPoolWork(drainRefreshConfig(), stranded) {
		t.Fatalf("fixture stranded work is not recoverable pool work: %#v", stranded)
	}

	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker-1", runtime.Config{}); err != nil {
		t.Fatalf("Start fake session: %v", err)
	}
	if err := sp.SetMeta("worker-1", "GC_DRAIN_ACK", "1"); err != nil {
		t.Fatalf("SetMeta drain ack: %v", err)
	}

	work, _, _, ready := refreshDrainAckedOrExitedClaimedWorkForDemand(
		drainRefreshConfig(),
		sp,
		[]beads.Bead{claim, stranded},
		[]beads.Store{store, store},
		[]string{"", ""},
		map[storeScopedBeadKey]bool{
			{ID: claim.ID}:    true,
			{ID: stranded.ID}: true,
		},
		newSessionBeadSnapshot([]beads.Bead{sessionBead}),
		io.Discard,
	)

	if !containsBeadID(work, stranded.ID) {
		t.Fatalf("refreshed work = %#v, want unrelated stranded work %s preserved", work, stranded.ID)
	}
	if !ready[storeScopedBeadKey{ID: stranded.ID}] {
		t.Fatalf("ready = %#v, want stranded ready flag preserved", ready)
	}
}

func TestClaimRefreshRestrictsBackingRead(t *testing.T) {
	var calls [][]string
	store := beads.NewBdStore(t.TempDir(), func(_, _ string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		return []byte("[]"), nil
	})

	if _, err := refreshClaimedWorkIDs(store, []string{"gc-one"}, time.Second); err != nil {
		t.Fatalf("refreshClaimedWorkIDs: %v", err)
	}
	if len(calls) == 0 {
		t.Fatal("no backing read")
	}
	for _, args := range calls {
		if !strings.Contains(strings.Join(args, " "), "gc-one") {
			t.Fatalf("backing command is not ID-bounded: %v", args)
		}
	}
}

func TestRefreshClaimedWorkIDsTimeoutDoesNotAccumulateReaders(t *testing.T) {
	store := newBlockingClaimRefreshStore(beads.NewMemStore())
	t.Cleanup(store.unblock)

	for i := 0; i < 3; i++ {
		_, err := refreshClaimedWorkIDs(store, []string{"gc-claim"}, time.Millisecond)
		if !errors.Is(err, errClaimedWorkRefreshTimeout) {
			t.Fatalf("refresh %d error = %v, want %v", i, err, errClaimedWorkRefreshTimeout)
		}
	}
	select {
	case <-store.started:
	case <-time.After(time.Second):
		t.Fatal("backing read did not start")
	}
	if got := store.calls.Load(); got != 1 {
		t.Fatalf("backing reads = %d, want 1", got)
	}
	if got := store.inFlight.Load(); got != 1 {
		t.Fatalf("in-flight reads = %d, want 1", got)
	}

	store.unblock()
	deadline := time.Now().Add(time.Second)
	for store.inFlight.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := store.inFlight.Load(); got != 0 {
		t.Fatalf("in-flight reads after unblock = %d, want 0", got)
	}
}

func TestClaimRefreshSameDrainAckObservationRunsOnce(t *testing.T) {
	backing := beads.NewMemStore()
	store := &countingClaimRefreshStore{Store: backing}
	sessionBead := seedDrainRefreshSession(t, beads.NewMemStore(), "session-worker-once", "worker-once")
	claim := seedDrainRefreshWork(t, backing, "gc-once", "worker-once")

	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker-once", runtime.Config{}); err != nil {
		t.Fatalf("Start fake session: %v", err)
	}
	if err := sp.SetMeta("worker-once", "GC_DRAIN_ACK", "1"); err != nil {
		t.Fatalf("SetMeta drain ack: %v", err)
	}

	for i := 0; i < 2; i++ {
		refreshDrainAckedOrExitedClaimedWorkForDemand(
			drainRefreshConfig(),
			sp,
			[]beads.Bead{claim},
			[]beads.Store{store},
			[]string{""},
			map[storeScopedBeadKey]bool{{ID: claim.ID}: true},
			newSessionBeadSnapshot([]beads.Bead{sessionBead}),
			io.Discard,
		)
	}
	if got := store.reads.Load(); got != 1 {
		t.Fatalf("backing reads after repeated identical observation = %d, want 1", got)
	}
}

func TestClaimRefreshDoesNotProbeIsRunningPerClaimHolder(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	var sessions []beads.Bead
	var work []beads.Bead
	var stores []beads.Store
	var refs []string
	ready := make(map[storeScopedBeadKey]bool)
	for i := 1; i <= 3; i++ {
		sessionName := "worker-running-" + string(rune('0'+i))
		sessions = append(sessions, seedDrainRefreshSession(t, beads.NewMemStore(), "session-"+sessionName, sessionName))
		claim := seedDrainRefreshWork(t, store, "gc-running-"+string(rune('0'+i)), sessionName)
		work = append(work, claim)
		stores = append(stores, store)
		refs = append(refs, "")
		ready[storeScopedBeadKey{ID: claim.ID}] = true
		if err := sp.Start(context.Background(), sessionName, runtime.Config{}); err != nil {
			t.Fatalf("Start fake session %s: %v", sessionName, err)
		}
	}

	refreshDrainAckedOrExitedClaimedWorkForDemand(
		drainRefreshConfig(),
		sp,
		work,
		stores,
		refs,
		ready,
		newSessionBeadSnapshot(sessions),
		io.Discard,
	)

	for _, call := range sp.SnapshotCalls() {
		if call.Method == "IsRunning" {
			t.Fatalf("unexpected per-session IsRunning call: %#v", call)
		}
	}
}

func seedDrainRefreshSession(t *testing.T, store beads.Store, id, sessionName string) beads.Bead {
	t.Helper()
	bead, err := store.Create(beads.Bead{
		ID:     id,
		Title:  "worker",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "agent:worker", "template:worker"},
		Metadata: map[string]string{
			"template":     "worker",
			"agent_name":   "worker",
			"session_name": sessionName,
			"state":        "active",
			"pool_managed": "true",
			"pool_slot":    "1",
		},
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}
	return bead
}

func seedDrainRefreshWork(t *testing.T, store beads.Store, id, assignee string) beads.Bead {
	t.Helper()
	status := "in_progress"
	bead, err := store.Create(beads.Bead{
		ID:       id,
		Title:    "claimed work",
		Type:     "task",
		Status:   status,
		Assignee: assignee,
		Metadata: map[string]string{
			"gc.routed_to": "worker",
		},
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	if err := store.Update(bead.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("claim work bead: %v", err)
	}
	bead, err = store.Get(bead.ID)
	if err != nil {
		t.Fatalf("reload claimed work bead: %v", err)
	}
	if bead.Status != status {
		t.Fatalf("created work status = %q, want %q", bead.Status, status)
	}
	return bead
}

func drainRefreshConfig() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(2),
		}},
	}
}

type failingClaimRefreshStore struct {
	beads.Store
	delay       time.Duration
	err         error
	getAttempts atomic.Int32
}

func (s *failingClaimRefreshStore) Get(id string) (beads.Bead, error) {
	if s.getAttempts.Add(1) == 1 {
		if s.delay > 0 {
			time.Sleep(s.delay)
		}
		if s.err != nil {
			return beads.Bead{}, s.err
		}
	}
	return s.Store.Get(id)
}

func (s *failingClaimRefreshStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Live && len(query.IDs) > 0 {
		if s.delay > 0 {
			time.Sleep(s.delay)
		}
		if s.err != nil {
			return nil, s.err
		}
	}
	return s.Store.List(query)
}

type countingClaimRefreshStore struct {
	beads.Store
	reads atomic.Int32
}

func (s *countingClaimRefreshStore) Get(id string) (beads.Bead, error) {
	s.reads.Add(1)
	return s.Store.Get(id)
}

func (s *countingClaimRefreshStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Live && len(query.IDs) > 0 {
		s.reads.Add(1)
	}
	return s.Store.List(query)
}

type blockingClaimRefreshStore struct {
	beads.Store
	started     chan struct{}
	release     chan struct{}
	startedOnce sync.Once
	releaseOnce sync.Once
	calls       atomic.Int32
	inFlight    atomic.Int32
}

func newBlockingClaimRefreshStore(store beads.Store) *blockingClaimRefreshStore {
	return &blockingClaimRefreshStore{
		Store:   store,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (s *blockingClaimRefreshStore) Get(id string) (beads.Bead, error) {
	s.block()
	return s.Store.Get(id)
}

func (s *blockingClaimRefreshStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.block()
	return s.Store.List(query)
}

func (s *blockingClaimRefreshStore) block() {
	s.calls.Add(1)
	s.inFlight.Add(1)
	s.startedOnce.Do(func() {
		close(s.started)
	})
	<-s.release
	s.inFlight.Add(-1)
}

func (s *blockingClaimRefreshStore) unblock() {
	s.releaseOnce.Do(func() {
		close(s.release)
	})
}
