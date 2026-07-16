package api

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/processgroup/processgrouptest"
	"github.com/gastownhall/gascity/internal/session"
)

type contextBlindReadyStore struct {
	beads.Store
	entered chan struct{}
	release chan struct{}
}

type legacyReadyStore struct {
	beads.Store
}

func (s *contextBlindReadyStore) Ready(...beads.ReadyQuery) ([]beads.Bead, error) {
	close(s.entered)
	<-s.release
	return nil, nil
}

type countingReadyStore struct {
	*beads.MemStore
	readyCalls int
}

func (s *countingReadyStore) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	s.readyCalls++
	return s.MemStore.Ready(query...)
}

func newSessionBead(sessionName string) beads.Bead {
	return beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"state":        string(session.StateActive),
			"template":     "myrig/worker",
			"session_name": sessionName,
		},
	}
}

// TestStatusSessionSnapshotUsesScopedStoreWhenAvailable proves
// statusSessionSnapshot reads through state.ScopedStoreLike's result
// instead of the shared CityBeadStore when one is available — the shared
// store here carries a bead that must NOT show up in the snapshot.
func TestStatusSessionSnapshotUsesScopedStoreWhenAvailable(t *testing.T) {
	shared := beads.NewMemStore()
	if _, err := shared.Create(newSessionBead("shared-session")); err != nil {
		t.Fatalf("Create shared session bead: %v", err)
	}
	scoped := beads.NewMemStore()
	if _, err := scoped.Create(newSessionBead("scoped-session")); err != nil {
		t.Fatalf("Create scoped session bead: %v", err)
	}

	state := newFakeState(t)
	state.cityBeadStore = shared
	state.scopedStoreFn = func(context.Context, beads.Store) (beads.Store, error) {
		return scoped, nil
	}
	s := &Server{state: state}

	snapshot := s.statusSessionSnapshot(context.Background())

	if _, ok := snapshot.bySessionName["scoped-session"]; !ok {
		t.Errorf("snapshot missing scoped-session; bySessionName = %+v, want the scoped store's read used", snapshot.bySessionName)
	}
	if _, ok := snapshot.bySessionName["shared-session"]; ok {
		t.Error("snapshot contains shared-session, want the shared store bypassed in favor of the scoped one")
	}
}

// TestStatusSessionSnapshotFallsBackWhenScopedStoreUnavailable pins the
// pre-ga-cdmx6x behavior for non-bd-CLI-backed stores (native, file, mem):
// when ScopedStoreLike answers (nil, nil), the read goes through the
// shared store exactly as before.
func TestStatusSessionSnapshotFallsBackWhenScopedStoreUnavailable(t *testing.T) {
	shared := beads.NewMemStore()
	if _, err := shared.Create(newSessionBead("shared-session")); err != nil {
		t.Fatalf("Create shared session bead: %v", err)
	}

	state := newFakeState(t)
	state.cityBeadStore = shared
	// scopedStoreFn left nil: defaults to (nil, nil), matching the real
	// ScopedStoreLike's answer for a MemStore-backed shared store.
	s := &Server{state: state}

	snapshot := s.statusSessionSnapshot(context.Background())

	if _, ok := snapshot.bySessionName["shared-session"]; !ok {
		t.Errorf("snapshot missing shared-session; bySessionName = %+v, want fallback to the shared store", snapshot.bySessionName)
	}
}

// TestStatusSessionSnapshotSurfacesScopedStoreResolutionError proves a
// ScopedStoreLike failure (e.g. Dolt credential resolution) surfaces as a
// "sessions:" partial error instead of silently degrading or panicking.
func TestStatusSessionSnapshotSurfacesScopedStoreResolutionError(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	state.scopedStoreFn = func(context.Context, beads.Store) (beads.Store, error) {
		return nil, errors.New("resolving dolt credentials: boom")
	}
	s := &Server{state: state}

	snapshot := s.statusSessionSnapshot(context.Background())

	if len(snapshot.partialErrors) == 0 {
		t.Fatal("partialErrors empty, want the scoped-store resolution error surfaced")
	}
	joined := strings.Join(snapshot.partialErrors, "; ")
	if !strings.Contains(joined, "sessions:") || !strings.Contains(joined, "boom") {
		t.Fatalf("partialErrors = %v, want a sessions: ...boom entry", snapshot.partialErrors)
	}
}

// TestStatusSessionSnapshotKillsBdChildOnTimeout is the ga-cdmx6x
// regression test for the API-side call site: statusSessionSnapshot must
// bind whatever ScopedStoreLike hands back to a request-scoped ctx tightly
// enough that a slow bd child is killed instead of surviving past the
// caller's budget. ScopedStoreLike here builds a real ctx-bound BdStore
// (mirroring what cmd/gc's scopedStoreLike does in production) pointed at
// a fake `bd` that backgrounds a long sleep and records its PID, proving
// the ctx statusSessionSnapshot constructs actually propagates end to end.
func TestStatusSessionSnapshotKillsBdChildOnTimeout(t *testing.T) {
	processgrouptest.RequireRealProcessSignals(t)
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}

	oldTimeout := statusStoreReadTimeout
	statusStoreReadTimeout = 200 * time.Millisecond
	t.Cleanup(func() { statusStoreReadTimeout = oldTimeout })

	binDir := t.TempDir()
	pidFile := filepath.Join(binDir, "bd-child.pid")
	writeExecutableScopedTest(t, filepath.Join(binDir, "bd"), "#!/bin/sh\n"+
		"sleep 30 &\n"+
		"echo \"$!\" > "+pidFile+"\n"+
		"wait\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	state.scopedStoreFn = func(ctx context.Context, _ beads.Store) (beads.Store, error) {
		return beads.NewBdStore(t.TempDir(), beads.ExecCommandRunnerWithEnvContext(ctx, nil)), nil
	}
	s := &Server{state: state}

	start := time.Now()
	_ = s.statusSessionSnapshot(context.Background())
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("statusSessionSnapshot blocked %s; want bounded by statusStoreReadTimeout", elapsed)
	}

	childPid := waitForNonEmptyFileScopedTest(t, pidFile, 5*time.Second)
	for range 50 {
		if err := exec.Command("kill", "-0", childPid).Run(); err != nil {
			return // child is gone
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = exec.Command("kill", "-KILL", childPid).Run()
	t.Fatalf("bd child process %s survived statusSessionSnapshot's timeout", childPid)
}

// TestStatusListStoreWithTimeoutUsesScopedStoreWhenAvailable proves the
// per-rig work-count fallback (statusListStoreWithTimeout, reached when a
// store isn't Counter-capable) reads through state.ScopedStoreLike's
// result instead of the store it was handed, when one is available.
func TestStatusListStoreWithTimeoutUsesScopedStoreWhenAvailable(t *testing.T) {
	shared := beads.NewMemStore()
	if _, err := shared.Create(beads.Bead{Type: "task", Title: "shared work", Status: "open"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	scoped := beads.NewMemStore()
	if _, err := scoped.Create(beads.Bead{Type: "task", Title: "scoped work 1", Status: "open"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := scoped.Create(beads.Bead{Type: "task", Title: "scoped work 2", Status: "open"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	state := newFakeState(t)
	state.scopedStoreFn = func(context.Context, beads.Store) (beads.Store, error) {
		return scoped, nil
	}

	rows, err := statusListStoreWithTimeout(context.Background(), state, shared, beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("statusListStoreWithTimeout: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2 from the scoped store (the shared store has only 1)", len(rows))
	}
}

// TestStatusListStoreWithTimeoutFallsBackWhenScopedStoreUnavailable pins
// the pre-ga-cdmx6x behavior: when ScopedStoreLike answers (nil, nil), the
// read goes through the store it was handed, exactly as before.
func TestStatusListStoreWithTimeoutFallsBackWhenScopedStoreUnavailable(t *testing.T) {
	shared := beads.NewMemStore()
	if _, err := shared.Create(beads.Bead{Type: "task", Title: "shared work", Status: "open"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state := newFakeState(t)

	rows, err := statusListStoreWithTimeout(context.Background(), state, shared, beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("statusListStoreWithTimeout: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1 from the fallback store", len(rows))
	}
}

// TestStatusListStoreWithTimeoutSurfacesScopedStoreResolutionError proves
// a ScopedStoreLike failure surfaces as an error instead of silently
// falling back or panicking — statusStoreWorkCounts already formats
// whatever error this returns into a "rig %s work: ..." partial error.
func TestStatusListStoreWithTimeoutSurfacesScopedStoreResolutionError(t *testing.T) {
	state := newFakeState(t)
	state.scopedStoreFn = func(context.Context, beads.Store) (beads.Store, error) {
		return nil, errors.New("resolving dolt credentials: boom")
	}

	_, err := statusListStoreWithTimeout(context.Background(), state, beads.NewMemStore(), beads.ListQuery{AllowScan: true})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("statusListStoreWithTimeout error = %v, want it to contain the resolution error", err)
	}
}

func TestStatusReadyStoreWithTimeoutUsesScopedStoreWhenAvailable(t *testing.T) {
	shared := beads.NewMemStore()
	if _, err := shared.Create(beads.Bead{Type: "task", Title: "shared ready work"}); err != nil {
		t.Fatalf("Create shared ready work: %v", err)
	}
	scoped := beads.NewMemStore()
	for _, title := range []string{"scoped ready work 1", "scoped ready work 2"} {
		if _, err := scoped.Create(beads.Bead{Type: "task", Title: title}); err != nil {
			t.Fatalf("Create %s: %v", title, err)
		}
	}

	state := newFakeState(t)
	state.scopedStoreFn = func(context.Context, beads.Store) (beads.Store, error) {
		return scoped, nil
	}

	rows, err := statusReadyStoreWithTimeout(context.Background(), state, &legacyReadyStore{Store: shared})
	if err != nil {
		t.Fatalf("statusReadyStoreWithTimeout: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2 from the scoped store", len(rows))
	}
}

func TestStatusReadyStoreWithTimeoutSurfacesScopedStoreResolutionError(t *testing.T) {
	state := newFakeState(t)
	state.scopedStoreFn = func(context.Context, beads.Store) (beads.Store, error) {
		return nil, errors.New("resolving dolt credentials: boom")
	}

	_, err := statusReadyStoreWithTimeout(context.Background(), state, &legacyReadyStore{Store: beads.NewMemStore()})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("statusReadyStoreWithTimeout error = %v, want it to contain the resolution error", err)
	}
}

func TestStatusReadyStoreWithTimeoutHonorsCanceledRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := statusReadyStoreWithTimeout(ctx, newFakeState(t), beads.NewMemStore())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("statusReadyStoreWithTimeout error = %v, want context.Canceled", err)
	}
}

func TestStatusReadyStoreWithTimeoutRejectsContextBlindReady(t *testing.T) {
	oldTimeout := statusStoreReadTimeout
	statusStoreReadTimeout = 50 * time.Millisecond
	t.Cleanup(func() { statusStoreReadTimeout = oldTimeout })

	store := &contextBlindReadyStore{
		Store:   beads.NewMemStore(),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	t.Cleanup(func() { close(store.release) })

	_, err := statusReadyStoreWithTimeout(context.Background(), newFakeState(t), store)
	if err == nil || !strings.Contains(err.Error(), "context-aware ready") {
		t.Fatalf("statusReadyStoreWithTimeout error = %v, want unsupported context-aware ready error", err)
	}
	select {
	case <-store.entered:
		t.Fatal("context-blind Ready was launched and abandoned")
	default:
	}
}

func TestStatusReadyStoreWithTimeoutUsesCachingStoreProjection(t *testing.T) {
	backing := &countingReadyStore{MemStore: beads.NewMemStore()}
	if _, err := backing.Create(beads.Bead{Type: "task", Title: "cached ready work"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	backing.readyCalls = 0
	state := newFakeState(t)
	scopedCalls := 0
	state.scopedStoreFn = func(context.Context, beads.Store) (beads.Store, error) {
		scopedCalls++
		return beads.NewMemStore(), nil
	}

	rows, err := statusReadyStoreWithTimeout(context.Background(), state, cache)
	if err != nil {
		t.Fatalf("statusReadyStoreWithTimeout: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1 cached Ready row", len(rows))
	}
	if backing.readyCalls != 0 {
		t.Fatalf("backing Ready calls = %d, want cache-only projection", backing.readyCalls)
	}
	if scopedCalls != 0 {
		t.Fatalf("ScopedStoreLike calls = %d, want context-ready cache projection tried first", scopedCalls)
	}
}

func TestStatusReadyStoreWithTimeoutDoesNotFallbackWhenCacheUnavailable(t *testing.T) {
	cache := beads.NewCachingStoreForTest(beads.NewMemStore(), nil)
	state := newFakeState(t)
	scopedCalls := 0
	state.scopedStoreFn = func(context.Context, beads.Store) (beads.Store, error) {
		scopedCalls++
		return beads.NewMemStore(), nil
	}

	rows, err := statusReadyStoreWithTimeout(context.Background(), state, cache)
	if !errors.Is(err, beads.ErrCacheUnavailable) {
		t.Fatalf("statusReadyStoreWithTimeout error = %v, want ErrCacheUnavailable", err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %+v, want no rows from an unavailable cache", rows)
	}
	if scopedCalls != 0 {
		t.Fatalf("ScopedStoreLike calls = %d, want none for final cache-unavailable result", scopedCalls)
	}
}

func TestStatusReadyStoreWithTimeoutWaitsForScopedResolutionCleanup(t *testing.T) {
	oldTimeout := statusStoreReadTimeout
	statusStoreReadTimeout = 50 * time.Millisecond
	t.Cleanup(func() { statusStoreReadTimeout = oldTimeout })

	for attempt := 1; attempt <= 3; attempt++ {
		cleanupStarted := make(chan struct{})
		releaseCleanup := make(chan struct{})
		cleanupDone := make(chan struct{})
		state := newFakeState(t)
		state.scopedStoreFn = func(ctx context.Context, _ beads.Store) (beads.Store, error) {
			<-ctx.Done()
			close(cleanupStarted)
			<-releaseCleanup
			close(cleanupDone)
			return nil, ctx.Err()
		}

		resultDone := make(chan error, 1)
		go func() {
			_, err := statusReadyStoreWithTimeout(context.Background(), state, &legacyReadyStore{Store: beads.NewMemStore()})
			resultDone <- err
		}()

		select {
		case <-cleanupStarted:
		case <-time.After(10 * time.Second):
			t.Fatalf("attempt %d: scoped resolution did not observe cancellation", attempt)
		}
		select {
		case err := <-resultDone:
			close(releaseCleanup)
			<-cleanupDone
			t.Fatalf("attempt %d: status returned %v before scoped resolution cleanup completed", attempt, err)
		case <-time.After(100 * time.Millisecond):
		}

		close(releaseCleanup)
		select {
		case <-cleanupDone:
		case <-time.After(10 * time.Second):
			t.Fatalf("attempt %d: scoped resolution cleanup did not finish", attempt)
		}
		select {
		case err := <-resultDone:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("attempt %d: error = %v, want context deadline exceeded", attempt, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("attempt %d: status did not return after scoped resolution cleanup", attempt)
		}
	}
}

func TestStatusStoreWorkCountsWaitsForReadyResolutionCleanup(t *testing.T) {
	oldTimeout := statusStoreReadTimeout
	statusStoreReadTimeout = 50 * time.Millisecond
	t.Cleanup(func() { statusStoreReadTimeout = oldTimeout })

	cleanupStarted := make(chan struct{})
	releaseCleanup := make(chan struct{})
	cleanupDone := make(chan struct{})
	state := newFakeState(t)
	state.scopedStoreFn = func(ctx context.Context, _ beads.Store) (beads.Store, error) {
		<-ctx.Done()
		close(cleanupStarted)
		<-releaseCleanup
		close(cleanupDone)
		return nil, ctx.Err()
	}

	resultDone := make(chan statusWorkResult, 1)
	go func() {
		resultDone <- statusStoreWorkCountsFor(
			context.Background(),
			state,
			"rig test",
			&legacyReadyStore{Store: beads.NewMemStore()},
			false,
			true,
		)
	}()

	select {
	case <-cleanupStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("ready resolution did not observe cancellation")
	}
	select {
	case result := <-resultDone:
		close(releaseCleanup)
		<-cleanupDone
		t.Fatalf("status work count returned %+v before ready resolution cleanup completed", result)
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseCleanup)
	select {
	case <-cleanupDone:
	case <-time.After(10 * time.Second):
		t.Fatal("ready resolution cleanup did not finish")
	}
	select {
	case result := <-resultDone:
		if len(result.errs) == 0 || !strings.Contains(result.errs[0], "timed out") {
			t.Fatalf("status work count result = %+v, want ready timeout", result)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("status work count did not return after ready resolution cleanup")
	}
}

func TestStatusReadyStoreWithTimeoutBoundsSlowScopedStoreResolution(t *testing.T) {
	oldTimeout := statusStoreReadTimeout
	statusStoreReadTimeout = 200 * time.Millisecond
	t.Cleanup(func() { statusStoreReadTimeout = oldTimeout })

	state := newFakeState(t)
	state.scopedStoreFn = cancelableStatusWorkScopedStoreResolution

	start := time.Now()
	_, err := statusReadyStoreWithTimeout(context.Background(), state, &legacyReadyStore{Store: beads.NewMemStore()})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("statusReadyStoreWithTimeout blocked %s on scoped-store resolution; want bounded by statusStoreReadTimeout", elapsed)
	}
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("statusReadyStoreWithTimeout error = %v, want a timed-out error", err)
	}
}

// TestStatusListStoreWithTimeoutKillsBdChildOnTimeout is the ga-cdmx6x
// regression test for the per-rig work-count call site: mirrors
// TestStatusSessionSnapshotKillsBdChildOnTimeout, but through
// statusListStoreWithTimeout directly.
func TestStatusListStoreWithTimeoutKillsBdChildOnTimeout(t *testing.T) {
	processgrouptest.RequireRealProcessSignals(t)
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}

	oldTimeout := statusStoreReadTimeout
	statusStoreReadTimeout = 200 * time.Millisecond
	t.Cleanup(func() { statusStoreReadTimeout = oldTimeout })

	binDir := t.TempDir()
	pidFile := filepath.Join(binDir, "bd-child.pid")
	writeExecutableScopedTest(t, filepath.Join(binDir, "bd"), "#!/bin/sh\n"+
		"sleep 30 &\n"+
		"echo \"$!\" > "+pidFile+"\n"+
		"wait\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	state := newFakeState(t)
	state.scopedStoreFn = func(ctx context.Context, _ beads.Store) (beads.Store, error) {
		return beads.NewBdStore(t.TempDir(), beads.ExecCommandRunnerWithEnvContext(ctx, nil)), nil
	}

	start := time.Now()
	_, _ = statusListStoreWithTimeout(context.Background(), state, beads.NewMemStore(), beads.ListQuery{AllowScan: true})
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("statusListStoreWithTimeout blocked %s; want bounded by statusStoreReadTimeout", elapsed)
	}

	childPid := waitForNonEmptyFileScopedTest(t, pidFile, 5*time.Second)
	assertStatusWorkReadChildStopped(t, childPid, "statusListStoreWithTimeout")
}

// TestStatusReadyStoreWithTimeoutKillsBdChildOnTimeout proves canonical
// readiness uses the request-scoped store too. A Ready read through the shared
// background-bound store would leave the bd child alive after the status
// deadline, defeating the scoped-store mitigation.
func TestStatusReadyStoreWithTimeoutKillsBdChildOnTimeout(t *testing.T) {
	processgrouptest.RequireRealProcessSignals(t)
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}

	oldTimeout := statusStoreReadTimeout
	statusStoreReadTimeout = 200 * time.Millisecond
	t.Cleanup(func() { statusStoreReadTimeout = oldTimeout })

	binDir := t.TempDir()
	pidFile := filepath.Join(binDir, "bd-child.pid")
	writeExecutableScopedTest(t, filepath.Join(binDir, "bd"), "#!/bin/sh\n"+
		"sleep 30 &\n"+
		"echo \"$!\" > "+pidFile+"\n"+
		"wait\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	state := newFakeState(t)
	state.scopedStoreFn = func(ctx context.Context, _ beads.Store) (beads.Store, error) {
		return beads.NewBdStore(t.TempDir(), beads.ExecCommandRunnerWithEnvContext(ctx, nil)), nil
	}

	start := time.Now()
	_, _ = statusReadyStoreWithTimeout(context.Background(), state, &legacyReadyStore{Store: beads.NewMemStore()})
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("statusReadyStoreWithTimeout blocked %s; want bounded by statusStoreReadTimeout", elapsed)
	}

	childPid := waitForNonEmptyFileScopedTest(t, pidFile, 5*time.Second)
	assertStatusWorkReadChildStopped(t, childPid, "statusReadyStoreWithTimeout")
}

// TestStatusSessionSnapshotBoundsSlowScopedStoreResolution proves the
// scoped-store *resolution* itself (not just the read through it) is bounded
// by statusStoreReadTimeout. ScopedStoreLike resolves the bd env / managed-
// dolt connection state synchronously, and that work can block on a mutex the
// reconcile loop holds without honoring the request ctx (gc-08qgn: /status
// hung ~20s-2min dragging the supervisor loop). A resolution that ignores ctx
// must still not hang the handler past its own read budget.
func TestStatusSessionSnapshotBoundsSlowScopedStoreResolution(t *testing.T) {
	oldTimeout := statusStoreReadTimeout
	statusStoreReadTimeout = 200 * time.Millisecond
	t.Cleanup(func() { statusStoreReadTimeout = oldTimeout })

	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	// Block for far longer than statusStoreReadTimeout WITHOUT honoring ctx,
	// mirroring a ctx-blind mutex acquire in the real env/store resolution.
	state.scopedStoreFn = func(context.Context, beads.Store) (beads.Store, error) {
		time.Sleep(3 * time.Second)
		return nil, nil
	}
	s := &Server{state: state}

	start := time.Now()
	snapshot := s.statusSessionSnapshot(context.Background())
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("statusSessionSnapshot blocked %s on scoped-store resolution; want bounded by statusStoreReadTimeout", elapsed)
	}
	joined := strings.Join(snapshot.partialErrors, "; ")
	if !strings.Contains(joined, "timed out") {
		t.Fatalf("partialErrors = %v, want a timed-out entry when resolution exceeds the budget", snapshot.partialErrors)
	}
}

// TestStatusListStoreWithTimeoutBoundsSlowScopedStoreResolution is the
// per-rig work-count analog of the above: a slow, ctx-blind ScopedStoreLike
// resolution must not hang statusListStoreWithTimeout past its read budget.
func TestStatusListStoreWithTimeoutBoundsSlowScopedStoreResolution(t *testing.T) {
	oldTimeout := statusStoreReadTimeout
	statusStoreReadTimeout = 200 * time.Millisecond
	t.Cleanup(func() { statusStoreReadTimeout = oldTimeout })

	state := newFakeState(t)
	state.scopedStoreFn = slowStatusWorkScopedStoreResolution

	start := time.Now()
	_, err := statusListStoreWithTimeout(context.Background(), state, beads.NewMemStore(), beads.ListQuery{AllowScan: true})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("statusListStoreWithTimeout blocked %s on scoped-store resolution; want bounded by statusStoreReadTimeout", elapsed)
	}
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("statusListStoreWithTimeout error = %v, want a timed-out error when resolution exceeds the budget", err)
	}
}

func writeExecutableScopedTest(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func slowStatusWorkScopedStoreResolution(context.Context, beads.Store) (beads.Store, error) {
	time.Sleep(3 * time.Second)
	return nil, nil
}

func cancelableStatusWorkScopedStoreResolution(ctx context.Context, _ beads.Store) (beads.Store, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(3 * time.Second):
		return nil, nil
	}
}

func assertStatusWorkReadChildStopped(t *testing.T, childPID, caller string) {
	t.Helper()
	for range 50 {
		if err := exec.Command("kill", "-0", childPID).Run(); err != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = exec.Command("kill", "-KILL", childPID).Run()
	t.Fatalf("bd child process %s survived %s's timeout", childPID, caller)
}

func waitForNonEmptyFileScopedTest(t *testing.T, path string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		data, err := os.ReadFile(path)
		if err == nil && len(strings.TrimSpace(string(data))) > 0 {
			return strings.TrimSpace(string(data))
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s to be written", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
