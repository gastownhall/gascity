package doctor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- measureStageBound: the du stage arithmetic under a runner budget ---

func TestMeasureStageBound(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name        string
		deadline    time.Time
		wantBound   time.Duration
		wantBounded bool
	}{
		{
			name:        "unbounded keeps hard cap",
			deadline:    time.Time{},
			wantBound:   doltDirMeasureTimeout,
			wantBounded: false,
		},
		{
			name:        "generous budget capped at hard du bound",
			deadline:    now.Add(5 * time.Minute),
			wantBound:   doltDirMeasureTimeout,
			wantBounded: true,
		},
		{
			name:        "budget just over cap plus reserve still caps du",
			deadline:    now.Add(doltDirMeasureTimeout + doltDirWalkReserve + time.Second),
			wantBound:   doltDirMeasureTimeout,
			wantBounded: true,
		},
		{
			name:        "tight budget leaves the walk a reserve slice",
			deadline:    now.Add(60 * time.Second),
			wantBound:   30 * time.Second,
			wantBounded: true,
		},
		{
			name:        "budget smaller than reserve goes entirely to du",
			deadline:    now.Add(20 * time.Second),
			wantBound:   20 * time.Second,
			wantBounded: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bound, bounded := measureStageBound(tt.deadline, now)
			if bounded != tt.wantBounded {
				t.Errorf("bounded = %v, want %v", bounded, tt.wantBounded)
			}
			// Tolerate a few milliseconds of clock skew between building
			// the deadline and reading the bound.
			if bound > tt.wantBound+5*time.Millisecond || bound < tt.wantBound-5*time.Millisecond {
				t.Errorf("bound = %s, want ≈%s", bound, tt.wantBound)
			}
		})
	}
}

// --- Doctor.boundedRun stamps the abandonment moment on the check context ---

type deadlineCapturingCheck struct {
	deadline time.Time
	called   bool
}

func (c *deadlineCapturingCheck) Name() string              { return "deadline-capturing" }
func (c *deadlineCapturingCheck) CanFix() bool              { return false }
func (c *deadlineCapturingCheck) Fix(_ *CheckContext) error { return nil }
func (c *deadlineCapturingCheck) WarmupEligible() bool      { return false }
func (c *deadlineCapturingCheck) Run(ctx *CheckContext) *CheckResult {
	c.called = true
	c.deadline = ctx.Deadline
	return &CheckResult{Name: c.Name(), Status: StatusOK}
}

func TestBoundedRunStampsCheckContextDeadline(t *testing.T) {
	d := &Doctor{CheckTimeout: 2 * time.Second}
	capturing := &deadlineCapturingCheck{}
	before := time.Now()
	d.boundedRun(capturing, &CheckContext{})
	if !capturing.called {
		t.Fatal("check did not run")
	}
	if capturing.deadline.IsZero() {
		t.Fatal("boundedRun left CheckContext.Deadline zero; measurement cannot size its bounds")
	}
	if capturing.deadline.Before(before.Add(time.Second)) || capturing.deadline.After(before.Add(3*time.Second)) {
		t.Errorf("Deadline = %s, want ≈ %s + CheckTimeout(2s)", capturing.deadline, before)
	}
}

func TestUnboundedRunLeavesCheckContextDeadlineZero(t *testing.T) {
	d := &Doctor{CheckTimeout: 0}
	capturing := &deadlineCapturingCheck{}
	d.boundedRun(capturing, &CheckContext{})
	if !capturing.called {
		t.Fatal("check did not run")
	}
	if !capturing.deadline.IsZero() {
		t.Errorf("Deadline = %s, want zero for unbounded run (historical behavior)", capturing.deadline)
	}
}

// --- duDirBytes under a runner budget ---

func TestDuDirBytes_BudgetExhaustedBeforeStart(t *testing.T) {
	dir := t.TempDir()
	ctx := &CheckContext{Deadline: time.Now().Add(-time.Second)}
	start := time.Now()
	_, _, err := duDirBytes(ctx, dir)
	if err == nil {
		t.Fatal("duDirBytes with an already-expired budget: err = nil, want immediate budget error")
	}
	if !strings.Contains(err.Error(), "budget") {
		t.Errorf("err = %v, want a budget-exhaustion mention", err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("duDirBytes took %s with an expired budget; want an immediate refusal", time.Since(start))
	}
}

func TestDuDirBytes_AbandonedCheckStopsMeasurement(t *testing.T) {
	dir := t.TempDir()
	done := make(chan struct{})
	close(done) // the runner has already abandoned this check
	ctx := &CheckContext{Done: done}
	_, _, err := duDirBytes(ctx, dir)
	if !errors.Is(err, errDirMeasureAbandoned) {
		t.Errorf("err = %v, want errDirMeasureAbandoned for an abandoned check", err)
	}
}

// duSleepShimPath shadows du with a script that sleeps past any bound, so
// the du stage consumes its whole time budget. Restores PATH via t.Setenv.
func duSleepShimPath(t *testing.T) {
	t.Helper()
	shimDir := t.TempDir()
	shim := filepath.Join(shimDir, "du")
	script := "#!/bin/sh\nexec sleep 300\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := exec.LookPath("du"); err != nil {
		t.Fatalf("du shim not on PATH: %v", err)
	}
}

func TestDuDirBytes_BoundedWalkFallsBackWithinRemainder(t *testing.T) {
	// A ~32s runner budget: du gets 32s−30s reserve = 2s, the walk gets the
	// ~30s remainder. The du shim sleeps past its bound, so the measurement
	// must complete through the fallback walk — the bug this file covers
	// (a hard 60s cap ignoring the runner budget) turned this into a
	// measure failure on artifact-heavy trees.
	duSleepShimPath(t)
	dir := t.TempDir()
	data := make([]byte, 64*1024)
	for i := range data {
		data[i] = 'x'
	}
	if err := os.WriteFile(filepath.Join(dir, "chunk"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := &CheckContext{Deadline: time.Now().Add(32 * time.Second)}
	total, exists, err := duDirBytes(ctx, dir)
	if err != nil {
		t.Fatalf("duDirBytes via fallback walk: %v", err)
	}
	if !exists {
		t.Fatal("duDirBytes exists = false, want true")
	}
	if total < int64(len(data)) {
		t.Fatalf("duDirBytes total = %d, want at least %d", total, len(data))
	}
}

func TestDuDirBytes_DuMissingWalksUnderRunnerBudget(t *testing.T) {
	// PATH holds only an empty dir: du resolves nowhere (exec.ErrNotFound)
	// and the fallback walk must run inside the runner budget, not a fresh
	// 60s. Nothing else in this test spawns subprocesses.
	shimDir := t.TempDir()
	t.Setenv("PATH", shimDir)
	if _, err := exec.LookPath("du"); err == nil {
		t.Skip("du still resolvable despite empty shim dir; cannot simulate missing du")
	}
	dir := t.TempDir()
	data := make([]byte, 32*1024)
	for i := range data {
		data[i] = 'y'
	}
	if err := os.WriteFile(filepath.Join(dir, "chunk"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := &CheckContext{Deadline: time.Now().Add(15 * time.Second)}
	total, exists, err := duDirBytes(ctx, dir)
	if err != nil {
		t.Fatalf("duDirBytes with du absent: %v", err)
	}
	if !exists || total < int64(len(data)) {
		t.Errorf("duDirBytes = (%d, %v), want ≥%d, true", total, exists, len(data))
	}
}

// --- walkDirBytesRemainder: the fallback walk's slice of the budget ---

func TestWalkDirBytesRemainder_BudgetExhausted(t *testing.T) {
	ctx := &CheckContext{Deadline: time.Now().Add(-time.Second)}
	_, _, err := walkDirBytesRemainder(ctx, t.TempDir(), time.Now(), true)
	if err == nil || !strings.Contains(err.Error(), "budget") {
		t.Errorf("err = %v, want a budget-exhaustion error", err)
	}
}

func TestWalkDirBytesRemainder_UnboundedKeepsHardCap(t *testing.T) {
	// Unbounded runs keep the historical behavior: the walk runs under the
	// hard cap and measures a small tree successfully.
	total, exists, err := walkDirBytesRemainder(nil, t.TempDir(), time.Now(), false)
	if err != nil {
		t.Fatalf("walkDirBytesRemainder unbounded: %v", err)
	}
	if !exists {
		t.Error("exists = false, want true for a TempDir")
	}
	if total != 0 {
		t.Errorf("total = %d, want 0 for an empty tree", total)
	}
}

// --- sumDirBytesWithContext honors its context mid-walk ---

func TestSumDirBytesWithContext_StopsOnCanceledContext(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := sumDirBytesWithContext(ctx, dir)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled (the walk must stop, not report success)", err)
	}
}

// --- watchCheckDone: CheckContext.Done propagates into measurement contexts ---

func TestWatchCheckDoneCancelsOnDone(t *testing.T) {
	done := make(chan struct{})
	walkCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := watchCheckDone(&CheckContext{Done: done}, cancel)
	close(done)
	select {
	case <-walkCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not cancel the measurement context after Done closed")
	}
	stop()
}

func TestWatchCheckDoneStopReleasesWatcher(t *testing.T) {
	done := make(chan struct{}) // never closed
	walkCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := watchCheckDone(&CheckContext{Done: done}, cancel)
	stop() // watcher exits without canceling
	select {
	case <-walkCtx.Done():
		t.Fatal("stop canceled the measurement context; want a clean release")
	default:
	}
}

func TestWatchCheckDoneNilContextIsNoop(t *testing.T) {
	cancel := func() {}
	stop := watchCheckDone(nil, cancel)
	stop() // must not panic
}
