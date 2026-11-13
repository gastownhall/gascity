package supervisor

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// fakeDoltOps is a scripted DoltOps for runDoltGC tests. Fields drive
// LiveSessions / ExecGC / SmokeCount behavior; call counters verify ordering.
type fakeDoltOps struct {
	liveSessions int
	liveErr      error
	liveDelay    time.Duration

	execGCErr   error
	execGCDelay time.Duration

	smokeCount int
	smokeErr   error
	smokeDelay time.Duration

	probeCalls int
	gcCalls    int
	smokeCalls int
	closed     bool
	calls      *[]string
}

func (f *fakeDoltOps) LiveSessions(ctx context.Context) (int, error) {
	f.probeCalls++
	if f.calls != nil {
		*f.calls = append(*f.calls, "gc.probe")
	}
	if f.liveDelay > 0 {
		select {
		case <-time.After(f.liveDelay):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return f.liveSessions, f.liveErr
}

func (f *fakeDoltOps) ExecGC(ctx context.Context) error {
	f.gcCalls++
	if f.calls != nil {
		*f.calls = append(*f.calls, "gc.exec")
	}
	if f.execGCDelay > 0 {
		select {
		case <-time.After(f.execGCDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return f.execGCErr
}

func (f *fakeDoltOps) SmokeCount(ctx context.Context) (int, error) {
	f.smokeCalls++
	if f.calls != nil {
		*f.calls = append(*f.calls, "gc.smoke")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if f.smokeDelay > 0 {
		select {
		case <-time.After(f.smokeDelay):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return f.smokeCount, f.smokeErr
}

func (f *fakeDoltOps) Close() error {
	f.closed = true
	return nil
}

func newGCTestLoop(t *testing.T, cfg config.DoltMaintenance, ops *fakeDoltOps) *StoreMaintenanceLoop {
	t.Helper()
	return NewStoreMaintenanceLoop(StoreMaintenanceLoopDeps{
		Cfg:      cfg,
		CityPath: t.TempDir(),
		OpenDoltOps: func(context.Context) (DoltOps, error) {
			return ops, nil
		},
	})
}

func TestRunDoltGC_HappyPathReturnsNilAndClosesConn(t *testing.T) {
	t.Parallel()
	ops := &fakeDoltOps{smokeCount: 5}
	loop := newGCTestLoop(t, config.DoltMaintenance{Enabled: true, GCTimeout: "1s"}, ops)

	if err := loop.runDoltGC(context.Background()); err != nil {
		t.Fatalf("runDoltGC = %v; want nil on happy path", err)
	}
	if ops.gcCalls != 1 {
		t.Errorf("ExecGC called %d times; want 1", ops.gcCalls)
	}
	if ops.smokeCalls != 1 {
		t.Errorf("SmokeCount called %d times; want 1", ops.smokeCalls)
	}
	if !ops.closed {
		t.Errorf("ops.Close not called — connection would leak in production")
	}
}

func TestRunDoltGC_SQLErrorAtGC_ReturnsStageGC(t *testing.T) {
	t.Parallel()
	ops := &fakeDoltOps{execGCErr: errors.New("out of disk")}
	loop := newGCTestLoop(t, config.DoltMaintenance{Enabled: true, GCTimeout: "1s"}, ops)

	err := loop.runDoltGC(context.Background())
	var me *MaintenanceError
	if !errors.As(err, &me) {
		t.Fatalf("runDoltGC = %v; want *MaintenanceError", err)
	}
	if me.Stage != "gc" {
		t.Errorf("Stage = %q; want %q", me.Stage, "gc")
	}
	if ops.smokeCalls != 0 {
		t.Errorf("SmokeCount ran after gc failure (%d calls); want 0", ops.smokeCalls)
	}
	if !ops.closed {
		t.Errorf("ops.Close not called on failure path — connection would leak")
	}
}

func TestRunDoltGC_GCDeadlineExceeded_ReturnsStageGC(t *testing.T) {
	t.Parallel()
	// GC takes 100ms but cfg.GCTimeout caps it at 10ms → context deadline.
	ops := &fakeDoltOps{execGCDelay: 100 * time.Millisecond}
	loop := newGCTestLoop(t, config.DoltMaintenance{Enabled: true, GCTimeout: "10ms"}, ops)

	err := loop.runDoltGC(context.Background())
	var me *MaintenanceError
	if !errors.As(err, &me) {
		t.Fatalf("runDoltGC = %v; want *MaintenanceError", err)
	}
	if me.Stage != "gc" {
		t.Errorf("Stage = %q; want %q", me.Stage, "gc")
	}
	if !errors.Is(me.Err, context.DeadlineExceeded) {
		t.Errorf("wrapped err = %v; want context.DeadlineExceeded", me.Err)
	}
}

func TestRunDoltGC_SmokeCountZero_ReturnsStageSmokeTest(t *testing.T) {
	t.Parallel()
	ops := &fakeDoltOps{smokeCount: 0}
	loop := newGCTestLoop(t, config.DoltMaintenance{Enabled: true, GCTimeout: "1s"}, ops)

	err := loop.runDoltGC(context.Background())
	var me *MaintenanceError
	if !errors.As(err, &me) {
		t.Fatalf("runDoltGC = %v; want *MaintenanceError", err)
	}
	if me.Stage != "smoke-test" {
		t.Errorf("Stage = %q; want %q", me.Stage, "smoke-test")
	}
	if ops.gcCalls != 1 {
		t.Errorf("ExecGC should still run when smoke fails (got %d calls); want 1", ops.gcCalls)
	}
}

func TestRunDoltGC_SmokeSQLError_ReturnsStageSmokeTest(t *testing.T) {
	t.Parallel()
	ops := &fakeDoltOps{smokeErr: errors.New("table not found")}
	loop := newGCTestLoop(t, config.DoltMaintenance{Enabled: true, GCTimeout: "1s"}, ops)

	err := loop.runDoltGC(context.Background())
	var me *MaintenanceError
	if !errors.As(err, &me) {
		t.Fatalf("runDoltGC = %v; want *MaintenanceError", err)
	}
	if me.Stage != "smoke-test" {
		t.Errorf("Stage = %q; want %q", me.Stage, "smoke-test")
	}
}

func TestRunDoltGC_SmokeDeadlineExceeded_ReturnsStageSmokeTest(t *testing.T) {
	t.Parallel()
	ops := &fakeDoltOps{smokeDelay: 100 * time.Millisecond}
	loop := newGCTestLoop(t, config.DoltMaintenance{Enabled: true, GCTimeout: "1s"}, ops)
	// Shorten this loop's smoke timeout to less than the fake's delay. On the
	// loop, not the package var: the probe reads it from whichever loop is
	// running, so a package-level write races every parallel test in here.
	loop.smokeTimeout = 10 * time.Millisecond

	err := loop.runDoltGC(context.Background())
	var me *MaintenanceError
	if !errors.As(err, &me) {
		t.Fatalf("runDoltGC = %v; want *MaintenanceError", err)
	}
	if me.Stage != "smoke-test" {
		t.Errorf("Stage = %q; want %q", me.Stage, "smoke-test")
	}
	if !errors.Is(me.Err, context.DeadlineExceeded) {
		t.Errorf("wrapped err = %v; want context.DeadlineExceeded", me.Err)
	}
}

func TestRunDoltGC_OpenError_ReturnsStageGC(t *testing.T) {
	t.Parallel()
	loop := NewStoreMaintenanceLoop(StoreMaintenanceLoopDeps{
		Cfg:      config.DoltMaintenance{Enabled: true, GCTimeout: "1s"},
		CityPath: t.TempDir(),
		OpenDoltOps: func(context.Context) (DoltOps, error) {
			return nil, errors.New("connection refused")
		},
	})

	err := loop.runDoltGC(context.Background())
	var me *MaintenanceError
	if !errors.As(err, &me) {
		t.Fatalf("runDoltGC = %v; want *MaintenanceError", err)
	}
	if me.Stage != "gc" {
		t.Errorf("Stage = %q; want %q", me.Stage, "gc")
	}
}

func TestRunDoltGC_NilFactoryReturnsNil(t *testing.T) {
	t.Parallel()
	// No OpenDoltOps injected — loop should treat runDoltGC as a no-op
	// so deployments can wire maintenance dependencies incrementally.
	loop := NewStoreMaintenanceLoop(StoreMaintenanceLoopDeps{
		Cfg:      config.DoltMaintenance{Enabled: true, GCTimeout: "1s"},
		CityPath: t.TempDir(),
	})
	if err := loop.runDoltGC(context.Background()); err != nil {
		t.Fatalf("runDoltGC with nil factory = %v; want nil", err)
	}
}

func TestRunDoltGC_ProbesLiveSessionsBeforeGC(t *testing.T) {
	t.Parallel()
	var calls []string
	ops := &fakeDoltOps{smokeCount: 5, calls: &calls}
	loop := newGCTestLoop(t, config.DoltMaintenance{Enabled: true, GCTimeout: "1s"}, ops)

	if err := loop.runDoltGC(context.Background()); err != nil {
		t.Fatalf("runDoltGC = %v; want nil with no live sessions", err)
	}
	want := []string{"gc.probe", "gc.exec", "gc.smoke"}
	if strings.Join(calls, ",") != strings.Join(want, ",") {
		t.Errorf("call order = %v; want %v (probe must precede CALL DOLT_GC)", calls, want)
	}
}

func TestRunDoltGC_LiveSessionsSkipGCAndLogReason(t *testing.T) {
	t.Parallel()
	ops := &fakeDoltOps{liveSessions: 3, smokeCount: 5}
	var stderr bytes.Buffer
	loop := NewStoreMaintenanceLoop(StoreMaintenanceLoopDeps{
		Cfg:      config.DoltMaintenance{Enabled: true, GCTimeout: "1s"},
		CityPath: t.TempDir(),
		Stderr:   &stderr,
		OpenDoltOps: func(context.Context) (DoltOps, error) {
			return ops, nil
		},
	})

	if err := loop.runDoltGC(context.Background()); !errors.Is(err, errGCSkippedLiveSessions) {
		t.Fatalf("runDoltGC = %v; want errGCSkippedLiveSessions (a skip is not a failure)", err)
	}
	if ops.gcCalls != 0 {
		t.Errorf("ExecGC called %d times with live sessions; want 0", ops.gcCalls)
	}
	if ops.smokeCalls != 0 {
		t.Errorf("SmokeCount called %d times after a skipped GC; want 0", ops.smokeCalls)
	}
	if !ops.closed {
		t.Errorf("ops.Close not called on skip path — connection would leak")
	}
	got := stderr.String()
	for _, want := range []string{"live session", "3", "skipping CALL DOLT_GC"} {
		if !strings.Contains(got, want) {
			t.Errorf("skip log = %q; want it to contain %q", got, want)
		}
	}
}

func TestRunDoltGC_ProbeError_FailsClosedWithStageGC(t *testing.T) {
	t.Parallel()
	probeErr := errors.New("processlist unavailable")
	ops := &fakeDoltOps{liveErr: probeErr, smokeCount: 5}
	loop := newGCTestLoop(t, config.DoltMaintenance{Enabled: true, GCTimeout: "1s"}, ops)

	err := loop.runDoltGC(context.Background())
	var me *MaintenanceError
	if !errors.As(err, &me) {
		t.Fatalf("runDoltGC = %v; want *MaintenanceError (fail-closed on probe error)", err)
	}
	if me.Stage != "gc" {
		t.Errorf("Stage = %q; want %q", me.Stage, "gc")
	}
	if !errors.Is(err, probeErr) {
		t.Errorf("err = %v; want it to wrap the probe error", err)
	}
	if ops.gcCalls != 0 {
		t.Errorf("ExecGC called %d times after a failed probe; want 0", ops.gcCalls)
	}
	if !ops.closed {
		t.Errorf("ops.Close not called on probe-error path — connection would leak")
	}
}

func TestRunDoltGC_ProbeDeadline_FailsClosedWithStageGC(t *testing.T) {
	t.Parallel()
	ops := &fakeDoltOps{liveDelay: 100 * time.Millisecond, smokeCount: 5}
	loop := newGCTestLoop(t, config.DoltMaintenance{Enabled: true, GCTimeout: "1s"}, ops)
	loop.smokeTimeout = 10 * time.Millisecond

	err := loop.runDoltGC(context.Background())
	var me *MaintenanceError
	if !errors.As(err, &me) {
		t.Fatalf("runDoltGC = %v; want *MaintenanceError (fail-closed on probe deadline)", err)
	}
	if me.Stage != "gc" {
		t.Errorf("Stage = %q; want %q", me.Stage, "gc")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v; want it to wrap context.DeadlineExceeded", err)
	}
	if ops.gcCalls != 0 {
		t.Errorf("ExecGC called %d times after a probe deadline overrun; want 0", ops.gcCalls)
	}
	if !ops.closed {
		t.Errorf("ops.Close not called on probe-deadline path — connection would leak")
	}
}
