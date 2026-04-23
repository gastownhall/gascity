package supervisor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// fakeDoltOps is a scripted DoltOps for runDoltGC tests. Fields drive
// ExecGC / SmokeCount behavior; call counters verify ordering.
type fakeDoltOps struct {
	execGCErr   error
	execGCDelay time.Duration

	smokeCount int
	smokeErr   error
	smokeDelay time.Duration

	gcCalls    int
	smokeCalls int
	closed     bool
}

func (f *fakeDoltOps) ExecGC(ctx context.Context) error {
	f.gcCalls++
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
	// Override the smoke timeout to something small; smoke takes longer.
	orig := maintenanceSmokeTimeout
	maintenanceSmokeTimeout = 10 * time.Millisecond
	t.Cleanup(func() { maintenanceSmokeTimeout = orig })

	ops := &fakeDoltOps{smokeDelay: 100 * time.Millisecond}
	loop := newGCTestLoop(t, config.DoltMaintenance{Enabled: true, GCTimeout: "1s"}, ops)

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
	// so the existing placeholder path (and SeedLastRunAt tests) keep
	// working unchanged.
	loop := NewStoreMaintenanceLoop(StoreMaintenanceLoopDeps{
		Cfg:      config.DoltMaintenance{Enabled: true, GCTimeout: "1s"},
		CityPath: t.TempDir(),
	})
	if err := loop.runDoltGC(context.Background()); err != nil {
		t.Fatalf("runDoltGC with nil factory = %v; want nil", err)
	}
}
