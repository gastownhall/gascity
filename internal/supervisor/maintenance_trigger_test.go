package supervisor

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// TestTriggerNow_Success runs one synchronous maintenance cycle and verifies
// the run is appended to history and lastRunAt is updated. Covers the happy
// path the CLI `gc maintenance dolt-gc --wait` takes through the API
// handler.
func TestTriggerNow_Success(t *testing.T) {
	t.Parallel()
	cfg := config.DoltMaintenance{Enabled: true, Interval: "1h"}
	now := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	loop := newTestLoop(t, cfg, now, 0.5, time.Time{})

	run, err := loop.TriggerNow(context.Background())
	if err != nil {
		t.Fatalf("TriggerNow = %v; want nil", err)
	}
	if !run.StartedAt.Equal(now) {
		t.Fatalf("run.StartedAt = %v; want %v", run.StartedAt, now)
	}
	if run.Stage != "done" {
		t.Fatalf("run.Stage = %q; want %q", run.Stage, "done")
	}
	if got := loop.LastRunAt(); !got.Equal(now) {
		t.Fatalf("LastRunAt after TriggerNow = %v; want %v", got, now)
	}
	if hist := loop.History(); len(hist) != 1 {
		t.Fatalf("History length = %d; want 1", len(hist))
	}
}

func TestTriggerNow_RunsSnapshotThenGC(t *testing.T) {
	cfg := config.DoltMaintenance{Enabled: true, Interval: "1h", GCTimeout: "1s"}
	now := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	var calls []string
	runner := &fakeDoltBackupRunner{writeOnSync: true, calls: &calls}
	ops := &fakeDoltOps{smokeCount: 5, calls: &calls}

	loop := NewStoreMaintenanceLoop(StoreMaintenanceLoopDeps{
		Cfg:      cfg,
		CityPath: t.TempDir(),
		Clock:    func() time.Time { return now },
		Rand:     func() float64 { return 0.5 },
		OpenDoltBackup: func(context.Context) (DoltBackupRunner, error) {
			return runner, nil
		},
		OpenDoltOps: func(context.Context) (DoltOps, error) {
			return ops, nil
		},
	})

	run, err := loop.TriggerNow(context.Background())
	if err != nil {
		t.Fatalf("TriggerNow = %v; want nil", err)
	}
	if run.Stage != "done" || run.Err != "" {
		t.Fatalf("run Stage=%q Err=%q; want done with no error", run.Stage, run.Err)
	}
	if run.SnapshotPath == "" {
		t.Fatal("run.SnapshotPath is empty; want snapshot path recorded")
	}
	wantCalls := []string{"backup.add", "backup.sync", "gc.exec", "gc.smoke"}
	if !slices.Equal(calls, wantCalls) {
		t.Fatalf("calls = %v; want %v", calls, wantCalls)
	}
	if !ops.closed {
		t.Fatal("DoltOps.Close not called")
	}
}

// TestTriggerNow_LeaseContention verifies that a second caller receives
// ErrMaintenanceInProgress with the started_at of the in-flight run rather
// than blocking or double-running. This is what POST
// /v0/city/{city}/maintenance/dolt-gc returns as a 409 Conflict when the
// supervisor scheduler or an earlier manual trigger is active.
func TestTriggerNow_LeaseContention(t *testing.T) {
	t.Parallel()
	cfg := config.DoltMaintenance{Enabled: true, Interval: "1h"}
	now := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	loop := newTestLoop(t, cfg, now, 0.5, time.Time{})

	// Simulate an in-flight run by holding the lease manually and setting the
	// in-flight reporter (matches what TriggerNow / runOnce do internally).
	loop.mu.Lock()
	inFlight := now.Add(-10 * time.Second)
	loop.runStartedAt.Store(&inFlight)
	defer func() {
		loop.runStartedAt.Store(nil)
		loop.mu.Unlock()
	}()

	_, err := loop.TriggerNow(context.Background())
	if err == nil {
		t.Fatal("TriggerNow under contention = nil; want ErrMaintenanceInProgress")
	}
	var inProg *MaintenanceInProgressError
	if !errors.As(err, &inProg) {
		t.Fatalf("TriggerNow error = %T %v; want *MaintenanceInProgressError", err, err)
	}
	if !inProg.StartedAt.Equal(inFlight) {
		t.Fatalf("MaintenanceInProgressError.StartedAt = %v; want %v", inProg.StartedAt, inFlight)
	}
}

// TestTriggerNow_ReleasesLease verifies that a successful TriggerNow
// releases m.mu so a subsequent trigger can proceed. Without this, a single
// manual run would brick the scheduler goroutine for the life of the
// supervisor.
func TestTriggerNow_ReleasesLease(t *testing.T) {
	t.Parallel()
	cfg := config.DoltMaintenance{Enabled: true, Interval: "1h"}
	now := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	loop := newTestLoop(t, cfg, now, 0.5, time.Time{})

	if _, err := loop.TriggerNow(context.Background()); err != nil {
		t.Fatalf("first TriggerNow = %v; want nil", err)
	}
	// Second call should proceed, not return in-progress.
	if _, err := loop.TriggerNow(context.Background()); err != nil {
		t.Fatalf("second TriggerNow = %v; want nil (lease released)", err)
	}
	if hist := loop.History(); len(hist) != 2 {
		t.Fatalf("History length after two triggers = %d; want 2", len(hist))
	}
}

// TestInFlightStart_TracksRunOnce verifies that scheduled runs (not just
// manual triggers) set runStartedAt so the 409 body still reports a
// timestamp when contention comes from the scheduler. Uses blocking DoltOps
// to stall inside runOnce just long enough to
// observe InFlightStart() from a second goroutine.
func TestInFlightStart_TracksRunOnce(t *testing.T) {
	t.Parallel()
	cfg := config.DoltMaintenance{Enabled: true, Interval: "1h"}
	now := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	gate := &gatedDoltOps{opened: make(chan struct{}), proceed: make(chan struct{})}
	loop := NewStoreMaintenanceLoop(StoreMaintenanceLoopDeps{
		Cfg:      cfg,
		CityPath: t.TempDir(),
		Clock:    func() time.Time { return now },
		Rand:     func() float64 { return 0.5 },
		OpenDoltOps: func(context.Context) (DoltOps, error) {
			return gate, nil
		},
	})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		loop.runOnce(context.Background())
	}()
	<-gate.opened // runOnce has set runStartedAt and is about to Write

	started, ok := loop.InFlightStart()
	if !ok {
		t.Fatal("InFlightStart() ok = false during runOnce; want true")
	}
	if !started.Equal(now) {
		t.Fatalf("InFlightStart() = %v; want %v", started, now)
	}

	close(gate.proceed)
	wg.Wait()

	if _, ok := loop.InFlightStart(); ok {
		t.Fatal("InFlightStart() ok = true after runOnce returned; want false")
	}
}

type gatedDoltOps struct {
	once    sync.Once
	opened  chan struct{}
	proceed chan struct{}
}

func (g *gatedDoltOps) ExecGC(ctx context.Context) error {
	g.once.Do(func() { close(g.opened) })
	select {
	case <-g.proceed:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *gatedDoltOps) SmokeCount(context.Context) (int, error) {
	return 1, nil
}

func (g *gatedDoltOps) Close() error {
	return nil
}
