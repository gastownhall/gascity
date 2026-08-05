package supervisor

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// fakeDoltOps is a scripted DoltOps for runDoltGC tests. Fields drive
// ListDatabases / ExecGCFor / SmokeCountFor behavior; call counters and
// gcDBs verify the per-database sweep order. Errors apply uniformly to
// every database.
type fakeDoltOps struct {
	databases []string // nil ⇒ default single target "hq"
	listErr   error

	execGCErr   error
	execGCDelay time.Duration

	smokeCount int
	smokeErr   error
	smokeDelay time.Duration

	listCalls  int
	gcCalls    int
	smokeCalls int
	gcDBs      []string // databases passed to ExecGCFor, in order
	closed     bool
	calls      *[]string
}

func (f *fakeDoltOps) ListDatabases(ctx context.Context) ([]string, error) {
	f.listCalls++
	if f.calls != nil {
		*f.calls = append(*f.calls, "gc.list")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.databases == nil {
		return []string{"hq"}, nil
	}
	return f.databases, nil
}

func (f *fakeDoltOps) ExecGCFor(ctx context.Context, db string) error {
	f.gcCalls++
	f.gcDBs = append(f.gcDBs, db)
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

func (f *fakeDoltOps) SmokeCountFor(ctx context.Context, _ string) (int, error) {
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

func TestRunDoltGC_HappyPathSweepsAllTargetsAndClosesConn(t *testing.T) {
	t.Parallel()
	ops := &fakeDoltOps{databases: []string{"hq", "gascity"}, smokeCount: 5}
	loop := newGCTestLoop(t, config.DoltMaintenance{Enabled: true, GCTimeout: "1s"}, ops)

	res, err := loop.runDoltGC(context.Background())
	if err != nil {
		t.Fatalf("runDoltGC = %v; want nil on happy path", err)
	}
	if res.Skipped {
		t.Errorf("res.Skipped = true; want false when GC runs")
	}
	if ops.gcCalls != 2 {
		t.Errorf("ExecGCFor called %d times; want 2 (one per target DB)", ops.gcCalls)
	}
	if ops.smokeCalls != 2 {
		t.Errorf("SmokeCountFor called %d times; want 2", ops.smokeCalls)
	}
	if want := []string{"hq", "gascity"}; !slices.Equal(ops.gcDBs, want) {
		t.Errorf("gcDBs = %v; want %v", ops.gcDBs, want)
	}
	if !ops.closed {
		t.Errorf("ops.Close not called — connection would leak in production")
	}
}

func TestRunDoltGC_FiltersSystemDatabases(t *testing.T) {
	t.Parallel()
	ops := &fakeDoltOps{
		databases:  []string{"information_schema", "mysql", "hq", "performance_schema", "gascity", "dolt", "__gc_probe"},
		smokeCount: 1,
	}
	loop := newGCTestLoop(t, config.DoltMaintenance{Enabled: true, GCTimeout: "1s"}, ops)

	if _, err := loop.runDoltGC(context.Background()); err != nil {
		t.Fatalf("runDoltGC = %v; want nil", err)
	}
	if want := []string{"hq", "gascity"}; !slices.Equal(ops.gcDBs, want) {
		t.Errorf("gcDBs = %v; want %v (system schemas must be filtered)", ops.gcDBs, want)
	}
}

func TestRunDoltGC_NoTargetDatabasesIsNoop(t *testing.T) {
	t.Parallel()
	ops := &fakeDoltOps{databases: []string{"information_schema", "mysql", "dolt"}}
	loop := newGCTestLoop(t, config.DoltMaintenance{Enabled: true, GCTimeout: "1s"}, ops)

	res, err := loop.runDoltGC(context.Background())
	if err != nil {
		t.Fatalf("runDoltGC = %v; want nil when only system DBs present", err)
	}
	if ops.gcCalls != 0 {
		t.Errorf("ExecGCFor called %d times; want 0 when no user databases", ops.gcCalls)
	}
	if res.Skipped {
		t.Errorf("res.Skipped = true; want false (no-target is not a size-gate skip)")
	}
}

func TestRunDoltGC_ListError_ReturnsStageGC(t *testing.T) {
	t.Parallel()
	ops := &fakeDoltOps{listErr: errors.New("connection reset")}
	loop := newGCTestLoop(t, config.DoltMaintenance{Enabled: true, GCTimeout: "1s"}, ops)

	_, err := loop.runDoltGC(context.Background())
	var me *MaintenanceError
	if !errors.As(err, &me) {
		t.Fatalf("runDoltGC = %v; want *MaintenanceError", err)
	}
	if me.Stage != "gc" {
		t.Errorf("Stage = %q; want %q", me.Stage, "gc")
	}
	if ops.gcCalls != 0 {
		t.Errorf("ExecGCFor ran despite list failure (%d calls); want 0", ops.gcCalls)
	}
}

func TestRunDoltGC_SQLErrorAtGC_ReturnsStageGC(t *testing.T) {
	t.Parallel()
	ops := &fakeDoltOps{execGCErr: errors.New("out of disk")}
	loop := newGCTestLoop(t, config.DoltMaintenance{Enabled: true, GCTimeout: "1s"}, ops)

	_, err := loop.runDoltGC(context.Background())
	var me *MaintenanceError
	if !errors.As(err, &me) {
		t.Fatalf("runDoltGC = %v; want *MaintenanceError", err)
	}
	if me.Stage != "gc" {
		t.Errorf("Stage = %q; want %q", me.Stage, "gc")
	}
	if ops.smokeCalls != 0 {
		t.Errorf("SmokeCountFor ran after gc failure (%d calls); want 0", ops.smokeCalls)
	}
	if !ops.closed {
		t.Errorf("ops.Close not called on failure path — connection would leak")
	}
}

func TestRunDoltGC_AggregatesPerDatabaseFailures(t *testing.T) {
	t.Parallel()
	ops := &fakeDoltOps{databases: []string{"hq", "gascity"}, execGCErr: errors.New("boom")}
	loop := newGCTestLoop(t, config.DoltMaintenance{Enabled: true, GCTimeout: "1s"}, ops)

	_, err := loop.runDoltGC(context.Background())
	if err == nil {
		t.Fatal("runDoltGC = nil; want aggregated error")
	}
	var me *MaintenanceError
	if !errors.As(err, &me) || me.Stage != "gc" {
		t.Fatalf("errors.As stage = %v; want a *MaintenanceError with Stage=gc", err)
	}
	// errors.Join concatenates both failures; both DB names must appear so
	// operators can see every database that failed, not just the first.
	msg := err.Error()
	if !strings.Contains(msg, `"hq"`) || !strings.Contains(msg, `"gascity"`) {
		t.Errorf("aggregated error %q; want it to name both hq and gascity", msg)
	}
	if ops.gcCalls != 2 {
		t.Errorf("ExecGCFor called %d times; want 2 (sweep continues past a failure)", ops.gcCalls)
	}
}

func TestRunDoltGC_GCDeadlineExceeded_ReturnsStageGC(t *testing.T) {
	t.Parallel()
	// GC takes 100ms but cfg.GCTimeout caps it at 10ms → context deadline.
	ops := &fakeDoltOps{execGCDelay: 100 * time.Millisecond}
	loop := newGCTestLoop(t, config.DoltMaintenance{Enabled: true, GCTimeout: "10ms"}, ops)

	_, err := loop.runDoltGC(context.Background())
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

func TestRunDoltGC_SmokeZeroCountIsHealthy(t *testing.T) {
	t.Parallel()
	// A per-database zero count is a healthy post-gc state (an empty rig),
	// not a smoke-test failure — the multi-DB sweep cannot assume every
	// database is non-empty the way the old single-store check did.
	ops := &fakeDoltOps{smokeCount: 0}
	loop := newGCTestLoop(t, config.DoltMaintenance{Enabled: true, GCTimeout: "1s"}, ops)

	if _, err := loop.runDoltGC(context.Background()); err != nil {
		t.Fatalf("runDoltGC with zero smoke count = %v; want nil (empty DB is healthy)", err)
	}
	if ops.gcCalls != 1 {
		t.Errorf("ExecGCFor called %d times; want 1", ops.gcCalls)
	}
}

func TestRunDoltGC_SmokeSQLError_ReturnsStageSmokeTest(t *testing.T) {
	t.Parallel()
	ops := &fakeDoltOps{smokeErr: errors.New("table not found")}
	loop := newGCTestLoop(t, config.DoltMaintenance{Enabled: true, GCTimeout: "1s"}, ops)

	_, err := loop.runDoltGC(context.Background())
	var me *MaintenanceError
	if !errors.As(err, &me) {
		t.Fatalf("runDoltGC = %v; want *MaintenanceError", err)
	}
	if me.Stage != "smoke-test" {
		t.Errorf("Stage = %q; want %q", me.Stage, "smoke-test")
	}
	if ops.gcCalls != 1 {
		t.Errorf("ExecGCFor should still run when smoke fails (got %d calls); want 1", ops.gcCalls)
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

	_, err := loop.runDoltGC(context.Background())
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

	_, err := loop.runDoltGC(context.Background())
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
	res, err := loop.runDoltGC(context.Background())
	if err != nil {
		t.Fatalf("runDoltGC with nil factory = %v; want nil", err)
	}
	if res != (gcSweepResult{}) {
		t.Errorf("res = %+v; want zero value when factory is nil", res)
	}
}

// --- store-size gate ---------------------------------------------------

func minStoreMB(v int) *int { return &v }

func newGateTestLoop(t *testing.T, cfg config.DoltMaintenance, ops *fakeDoltOps, size int64) *StoreMaintenanceLoop {
	t.Helper()
	return NewStoreMaintenanceLoop(StoreMaintenanceLoopDeps{
		Cfg:            cfg,
		CityPath:       t.TempDir(),
		StoreSizeBytes: func() int64 { return size },
		OpenDoltOps: func(context.Context) (DoltOps, error) {
			return ops, nil
		},
	})
}

func TestRunDoltGC_SizeGateSkipsSmallStore(t *testing.T) {
	t.Parallel()
	const mib = int64(1) << 20
	ops := &fakeDoltOps{smokeCount: 1}
	// Default MinStoreMB (nil) ⇒ 1 GiB floor; a 100 MiB store is below it.
	loop := newGateTestLoop(t, config.DoltMaintenance{Enabled: true, GCTimeout: "1s"}, ops, 100*mib)

	res, err := loop.runDoltGC(context.Background())
	if err != nil {
		t.Fatalf("runDoltGC = %v; want nil when gated", err)
	}
	if !res.Skipped {
		t.Errorf("res.Skipped = false; want true when store below floor")
	}
	if res.BeforeBytes != 100*mib || res.AfterBytes != 100*mib {
		t.Errorf("res before/after = %d/%d; want %d/%d (no GC, no change)", res.BeforeBytes, res.AfterBytes, 100*mib, 100*mib)
	}
	if ops.gcCalls != 0 || ops.listCalls != 0 {
		t.Errorf("gcCalls=%d listCalls=%d; want 0/0 — gate must skip before opening a connection", ops.gcCalls, ops.listCalls)
	}
}

func TestRunDoltGC_SizeGateRunsAboveFloor(t *testing.T) {
	t.Parallel()
	const mib = int64(1) << 20
	ops := &fakeDoltOps{smokeCount: 1}
	loop := newGateTestLoop(t, config.DoltMaintenance{Enabled: true, GCTimeout: "1s"}, ops, 2048*mib)

	res, err := loop.runDoltGC(context.Background())
	if err != nil {
		t.Fatalf("runDoltGC = %v; want nil", err)
	}
	if res.Skipped {
		t.Errorf("res.Skipped = true; want false for a 2 GiB store above the 1 GiB floor")
	}
	if ops.gcCalls != 1 {
		t.Errorf("ExecGCFor called %d times; want 1", ops.gcCalls)
	}
	if res.BeforeBytes != 2048*mib || res.AfterBytes != 2048*mib {
		t.Errorf("res before/after = %d/%d; want both %d", res.BeforeBytes, res.AfterBytes, 2048*mib)
	}
}

func TestRunDoltGC_SizeGateDisabledWhenMinStoreZero(t *testing.T) {
	t.Parallel()
	const mib = int64(1) << 20
	ops := &fakeDoltOps{smokeCount: 1}
	// MinStoreMB=0 disables the gate ⇒ GC runs even on a tiny store.
	cfg := config.DoltMaintenance{Enabled: true, GCTimeout: "1s", MinStoreMB: minStoreMB(0)}
	loop := newGateTestLoop(t, cfg, ops, 1*mib)

	res, err := loop.runDoltGC(context.Background())
	if err != nil {
		t.Fatalf("runDoltGC = %v; want nil", err)
	}
	if res.Skipped {
		t.Errorf("res.Skipped = true; want false when the size gate is disabled")
	}
	if ops.gcCalls != 1 {
		t.Errorf("ExecGCFor called %d times; want 1 (gate disabled)", ops.gcCalls)
	}
}

func TestGCTargetDatabases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"empty", nil, []string{}},
		{"all system", []string{"information_schema", "mysql", "dolt", "sys", "performance_schema", "dolt_cluster", "__gc_probe"}, []string{}},
		{"user dbs preserved in order", []string{"hq", "information_schema", "gascity", "mysql"}, []string{"hq", "gascity"}},
		{"blank entries dropped", []string{"", "  ", "hq"}, []string{"hq"}},
		{"system match is case-insensitive", []string{"INFORMATION_SCHEMA", "MySQL", "Rig1"}, []string{"Rig1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := gcTargetDatabases(tc.in)
			if !slices.Equal(got, tc.want) {
				t.Errorf("gcTargetDatabases(%v) = %v; want %v", tc.in, got, tc.want)
			}
		})
	}
}
