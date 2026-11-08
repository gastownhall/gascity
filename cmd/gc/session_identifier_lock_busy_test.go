package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/poolplan"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// shortLockWaitBudget shrinks the tick-side identifier-lock wait so a busy
// lock fails in test time, and restores the production budget afterwards.
func shortLockWaitBudget(t *testing.T) {
	t.Helper()
	prev := sessionIdentifierLockWaitBudget
	sessionIdentifierLockWaitBudget = 30 * time.Millisecond
	t.Cleanup(func() { sessionIdentifierLockWaitBudget = prev })
}

// holdSessionIdentifierLock holds the city-scoped lock for identifier, as a
// slow creator in another process would, until the test ends.
func holdSessionIdentifierLock(t *testing.T, cityPath, identifier string) {
	t.Helper()
	held := make(chan struct{})
	releaseCh := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- session.WithCitySessionNameLock(cityPath, identifier, func() error {
			close(held)
			<-releaseCh
			return nil
		})
	}()
	select {
	case <-held:
	case err := <-done:
		t.Fatalf("holder of %q never acquired its lock: %v", identifier, err)
	}
	t.Cleanup(func() {
		close(releaseCh)
		if err := <-done; err != nil {
			t.Errorf("holder of %q: %v", identifier, err)
		}
	})
}

// runBounded runs fn and fails the test, instead of hanging the suite, when it
// does not return in time: an unbounded identifier-lock wait is exactly the
// regression these tests exist to catch.
func runBounded(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatalf("%s did not return within 15s although the identifier-lock wait budget is %v: the wait is not bounded", what, sessionIdentifierLockWaitBudget)
	}
}

func busyLockWorkerCity() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "gc"},
		Agents: []config.Agent{
			{Name: "worker", StartCommand: "true", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)},
		},
	}
}

func TestSessionIdentifierLockWaitBudgetDefaultIsFiveSeconds(t *testing.T) {
	if sessionIdentifierLockWaitBudget != 5*time.Second {
		t.Fatalf("sessionIdentifierLockWaitBudget = %v, want 5s per acquisition", sessionIdentifierLockWaitBudget)
	}
}

func TestCreatePoolSessionBeadWithGuardedAlias_BusyIdentifierLockFailsTypedAndCreatesNothing(t *testing.T) {
	shortLockWaitBudget(t)
	store := beads.NewMemStore()
	cityPath := t.TempDir()
	cfg := busyLockWorkerCity()
	var stderr bytes.Buffer
	bp := newAgentBuildParams("gc", cityPath, cfg, runtime.NewFake(), time.Now().UTC(), store, &stderr)
	bp.sessionBeads = newSessionBeadSnapshot(nil)
	holdSessionIdentifierLock(t, cityPath, "worker-1")

	start := time.Now()
	var (
		info session.Info
		err  error
	)
	runBounded(t, "createPoolSessionBeadWithGuardedAlias", func() {
		info, err = createPoolSessionBeadWithGuardedAlias(bp, &cfg.Agents[0], "worker", "worker-1", 1, nil)
	})
	if !errors.Is(err, session.ErrSessionIdentifierBusy) {
		t.Fatalf("error = %v, want errors.Is session.ErrSessionIdentifierBusy", err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("busy create took %v, want it bounded by the wait budget", elapsed)
	}
	if info.ID != "" {
		t.Fatalf("busy create returned session %#v", info)
	}
	created, listErr := store.ListByLabel(sessionBeadLabel, 0)
	if listErr != nil {
		t.Fatalf("listing session beads: %v", listErr)
	}
	if len(created) != 0 {
		t.Fatalf("session beads = %#v, want none after a busy lock", created)
	}
}

func TestExecutePlannedPoolSessionBeadCreate_BusyLockReleasesCreateBudgetExactlyOnce(t *testing.T) {
	shortLockWaitBudget(t)
	cityPath := t.TempDir()
	cfg := busyLockWorkerCity()
	budget := poolplan.NewCreateBudget(1)
	bp := newAgentBuildParams("gc", cityPath, cfg, runtime.NewFake(), time.Now().UTC(), beads.NewMemStore(), io.Discard)
	bp.sessionBeads = newSessionBeadSnapshot(nil)
	bp.poolSessionCreateBudget = budget
	holdSessionIdentifierLock(t, cityPath, "worker-1")

	if !bp.tryClaimPoolSessionCreate("worker") {
		t.Fatal("the single create token was not claimable")
	}
	var err error
	runBounded(t, "executePlannedPoolSessionBeadCreate", func() {
		_, err = executePlannedPoolSessionBeadCreate(bp, &cfg.Agents[0], "worker", poolSessionCreatePlan{
			qualifiedInstance: "worker-1", slot: 1, poolSlot: 1,
		})
	})
	if !errors.Is(err, session.ErrSessionIdentifierBusy) {
		t.Fatalf("error = %v, want errors.Is session.ErrSessionIdentifierBusy", err)
	}
	if !budget.TryClaim("worker") {
		t.Fatal("the create-budget claim was not released by the busy create (zero releases)")
	}
	if budget.TryClaim("worker") {
		t.Fatal("the create-budget claim was released twice by the busy create (token minted)")
	}
}

func TestSelectOrCreateDependencyPoolSessionBeadWithSlot_BusyIdentifierLockFailsTypedAndCreatesNothing(t *testing.T) {
	shortLockWaitBudget(t)
	store := beads.NewMemStore()
	cityPath := t.TempDir()
	cfgAgent := config.Agent{Name: "claude", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}
	bp := &agentBuildParams{
		cityPath:     cityPath,
		beadStore:    store,
		sessionBeads: newSessionBeadSnapshot(nil),
		agents:       []config.Agent{cfgAgent},
	}
	// Plan first, to learn which identifier the floor create will lock.
	_, _, plan, err := selectOrPlanDependencyPoolSessionBead(bp, &cfgAgent, "claude", time.Now())
	if err != nil || plan == nil {
		t.Fatalf("selectOrPlanDependencyPoolSessionBead plan = %#v, err = %v, want a fresh-row plan", plan, err)
	}
	holdSessionIdentifierLock(t, cityPath, plan.qualifiedInstance)

	var info session.Info
	runBounded(t, "selectOrCreateDependencyPoolSessionBeadWithSlot", func() {
		info, _, err = selectOrCreateDependencyPoolSessionBeadWithSlot(bp, &cfgAgent, "claude")
	})
	if !errors.Is(err, session.ErrSessionIdentifierBusy) {
		t.Fatalf("error = %v, want errors.Is session.ErrSessionIdentifierBusy", err)
	}
	if info.ID != "" {
		t.Fatalf("busy floor create returned session %#v", info)
	}
	created, listErr := store.ListByLabel(sessionBeadLabel, 0)
	if listErr != nil {
		t.Fatalf("listing session beads: %v", listErr)
	}
	if len(created) != 0 {
		t.Fatalf("session beads = %#v, want none after a busy lock", created)
	}
}

func TestNormalizeNonExpandingPoolSessionInfo_BusyAliasLockLeavesRowUntouchedAndSucceeds(t *testing.T) {
	shortLockWaitBudget(t)
	seed := wpoolSessionBead("gm-1", "open", "mayor-1", []string{"agent:mayor-1"}, map[string]string{
		"template": "mayor", "agent_name": "mayor-1", "alias": "mayor-1",
		"pool_slot": "1", "session_name": "s-mayor-1", "pool_managed": "true",
	})
	cfgAgent := &config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	cfg := &config.City{Agents: []config.Agent{*cfgAgent}}
	cityPath := t.TempDir()
	store := beads.NewMemStoreFrom(1, []beads.Bead{seed}, nil)
	var stderr bytes.Buffer
	bp := &agentBuildParams{cityPath: cityPath, beadStore: store, city: cfg, stderr: &stderr}
	in := sessiontest.SeedBead(t, seed)
	holdSessionIdentifierLock(t, cityPath, "mayor")

	var (
		got session.Info
		err error
	)
	runBounded(t, "normalizeNonExpandingPoolSessionInfo", func() {
		got, err = normalizeNonExpandingPoolSessionInfo(bp, cfgAgent, in)
	})
	if err != nil {
		t.Fatalf("normalize under a busy alias lock: %v, want nil (the collapse is retried next tick)", err)
	}
	if got.Alias != in.Alias || got.AgentName != in.AgentName || got.PoolSlot != in.PoolSlot {
		t.Fatalf("normalize returned %+v, want the input row unchanged", got)
	}
	persisted, getErr := session.NewStore(beads.SessionStore{Store: store}).Get("gm-1")
	if getErr != nil {
		t.Fatalf("store Get: %v", getErr)
	}
	if persisted.Alias != "mayor-1" || persisted.PoolSlot != "1" {
		t.Fatalf("persisted row = %+v, want it untouched while the alias lock is busy", persisted)
	}
	if !strings.Contains(stderr.String(), "busy") {
		t.Fatalf("stderr = %q, want a log line saying the alias lock was busy", stderr.String())
	}
	if strings.Contains(stderr.String(), "collapsing phantom pool identity") {
		t.Fatalf("stderr = %q, must not claim a collapse that did not happen", stderr.String())
	}
}

// TestBuildDesiredStateBusyPoolSlotStallsOnlyThatSlot builds a pool with one
// reusable seat and one slot whose identifier lock another creator holds: the
// build must finish, keep the reused seat, skip the stalled slot with the
// named log line, count one create error, and report the wait in lock_wait_ms.
func TestBuildDesiredStateBusyPoolSlotStallsOnlyThatSlot(t *testing.T) {
	shortLockWaitBudget(t)
	cityDir := t.TempDir()
	cfg := busyLockWorkerCity()
	mem := beads.NewMemStore()
	for i := 0; i < 2; i++ {
		if _, err := mem.Create(beads.Bead{
			Title:    fmt.Sprintf("routed %d", i),
			Type:     "task",
			Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := mem.Create(beads.Bead{
		Title:  "worker seat",
		Type:   sessionBeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "worker-1", "template": "worker", "agent_name": "worker-1",
			"pool_slot": "1", "state": "active", "pool_managed": "true",
		},
	}); err != nil {
		t.Fatal(err)
	}
	clk := newLockedClock(time.Millisecond)
	tracer, cycle := demandTraceCycle(t, cityDir, clk)
	snapshot, err := loadSessionBeadSnapshot(mem)
	if err != nil {
		t.Fatal(err)
	}
	holdSessionIdentifierLock(t, cityDir, "worker-2")

	var stderr bytes.Buffer
	var result DesiredStateResult
	runBounded(t, "buildDesiredStateWithSessionBeads", func() {
		result = buildDesiredStateWithSessionBeads("gc", cityDir, time.Now().UTC(), cfg, runtime.NewFake(),
			mem, nil, snapshot, cycle, &stderr)
	})
	if len(result.State) != 1 {
		t.Fatalf("desired sessions = %d, want 1 (the reused seat; the stalled slot is skipped)\nstderr:\n%s", len(result.State), stderr.String())
	}
	const want = "slot stalled on a busy session identifier lock; retrying next tick"
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr = %q, want it to contain %q", stderr.String(), want)
	}
	records := demandTraceRecords(t, cityDir, tracer, cycle)["demand_snapshot.realize_pools"]
	if len(records) != 1 {
		t.Fatalf("realize_pools records = %d, want 1", len(records))
	}
	if got := demandTraceInt(t, records[0], "create_errors"); got != 1 {
		t.Errorf("realize_pools create_errors = %d, want 1", got)
	}
	if got := demandTraceInt(t, records[0], "lock_wait_ms"); got < 1 {
		t.Errorf("realize_pools lock_wait_ms = %d, want >= 1 for a create that waited out its budget", got)
	}
}

func TestPoolRealizeProbeLockWaitAccumulatesAcrossBoundedAcquisitions(t *testing.T) {
	shortLockWaitBudget(t)
	cityPath := t.TempDir()
	cfg := busyLockWorkerCity()
	bp := newAgentBuildParams("gc", cityPath, cfg, runtime.NewFake(), time.Now().UTC(), beads.NewMemStore(), io.Discard)
	bp.sessionBeads = newSessionBeadSnapshot(nil)
	bp.realizeProbe = &poolRealizeProbe{}
	holdSessionIdentifierLock(t, cityPath, "worker-1")

	var err error
	runBounded(t, "createPoolSessionBeadWithGuardedAlias", func() {
		_, err = createPoolSessionBeadWithGuardedAlias(bp, &cfg.Agents[0], "worker", "worker-1", 1, nil)
	})
	if !errors.Is(err, session.ErrSessionIdentifierBusy) {
		t.Fatalf("error = %v, want busy", err)
	}
	if got := bp.realizeProbe.lockWait.Load(); got < int64(20*time.Millisecond) {
		t.Fatalf("probe lockWait = %v, want at least most of the 30ms budget it waited out", time.Duration(got))
	}
}
