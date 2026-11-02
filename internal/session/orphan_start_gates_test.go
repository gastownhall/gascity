package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// gatedStartWatchdog is orphanWatchdog on wall time: the gated starts here
// take real runtime leases, outside a synctest bubble, so their watchdog is
// seconds, not minutes.
func gatedStartWatchdog(t *testing.T, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("the gated start still blocked 5s after its context ended: the pre-start orphan scan is unbounded")
		return nil
	}
}

// Every start-shaped entry that sweeps for orphans before starting defers on
// an unanswered scan instead of blocking forever: the caller gets an error
// wrapping runtime.ErrRuntimeUnavailable and no runtime is started.
func TestPreStartOrphanScanDefersEveryGatedStart(t *testing.T) {
	t.Run("start", func(t *testing.T) {
		store := openLeaseStore(t, t.TempDir())
		b := mintRow(t, store)
		sp := newScriptedOrphanScan()
		sp.blockLeading.Store(1)
		defer sp.unblock()
		mgr := newTestManager(t, store, sp)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		err := gatedStartWatchdog(t, func() error {
			return mgr.Start(ctx, testActor(mgr, ActorOperator), b.ID, "claude --resume tok-3", runtime.Config{})
		})

		if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("Start = %v, want it to defer with an error wrapping runtime.ErrRuntimeUnavailable", err)
		}
		if got := sp.starts.Load(); got != 0 {
			t.Fatalf("provider Starts = %d, want the deferral to start nothing", got)
		}
	})

	t.Run("start runtime only", func(t *testing.T) {
		store := openLeaseStore(t, t.TempDir())
		b := mintRow(t, store)
		sp := newScriptedOrphanScan()
		sp.blockLeading.Store(1)
		defer sp.unblock()
		mgr := newTestManager(t, store, sp)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		err := gatedStartWatchdog(t, func() error {
			by := runtimeOnlyActor(t, mgr, b.ID)
			return mgr.StartRuntimeOnly(ctx, by, b.ID, mintUnder(t, mgr, by.Lease, b.ID), "claude --resume tok-3", runtime.Config{})
		})

		if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("StartRuntimeOnly = %v, want it to defer with an error wrapping runtime.ErrRuntimeUnavailable", err)
		}
		if got := sp.starts.Load(); got != 0 {
			t.Fatalf("provider Starts = %d, want the deferral to start nothing", got)
		}
	})

	t.Run("stale-key fresh restart", func(t *testing.T) {
		store := openLeaseStore(t, t.TempDir())
		b := mintRow(t, store, "session_key", "k-stale", "resume_flag", "--resume")
		sp := newScriptedOrphanScan()
		sp.startFailures.Store(1) // the resume under the stale key dies
		sp.blockedFrom.Store(2)   // the fresh restart's pre-start scan wedges
		defer sp.unblock()
		mgr := newTestManager(t, store, sp, WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter))
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		err := gatedStartWatchdog(t, func() error {
			return mgr.Start(ctx, testActor(mgr, ActorOperator), b.ID, "claude --resume k-stale", runtime.Config{})
		})

		if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("Start = %v, want the fresh restart to defer with an error wrapping runtime.ErrRuntimeUnavailable", err)
		}
		if got := sp.starts.Load(); got != 1 {
			t.Fatalf("provider Starts = %d, want only the failed resume, never the fresh relaunch", got)
		}
	})

	t.Run("create rolls back its row and its explicit name", func(t *testing.T) {
		store := openLeaseStore(t, t.TempDir())
		sp := newScriptedOrphanScan()
		sp.blockLeading.Store(1)
		defer sp.unblock()
		mgr := newTestManager(t, store, sp)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		err := gatedStartWatchdog(t, func() error {
			_, err := mgr.CreateSession(ctx, CreateOptions{
				ExplicitName: "want-gone", Template: "helper", Title: "orphan-gate",
				Command: "claude", WorkDir: "/tmp", Provider: "claude",
			})
			return err
		})

		if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("CreateSession = %v, want it to defer with an error wrapping runtime.ErrRuntimeUnavailable", err)
		}
		if got := sp.starts.Load(); got != 0 {
			t.Fatalf("provider Starts = %d, want the deferral to start nothing", got)
		}
		rows, listErr := store.List(beads.ListQuery{Type: BeadType, IncludeClosed: true})
		if listErr != nil {
			t.Fatalf("List: %v", listErr)
		}
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want the one rolled-back create", len(rows))
		}
		row := rows[0]
		if row.Status != "closed" {
			t.Fatalf("row status = %q, want the create rolled back to closed", row.Status)
		}
		if row.Metadata["session_name"] != "" || row.Metadata["session_name_explicit"] != "" {
			t.Fatalf("row names = %q/%q, want the explicit name cleared on rollback", row.Metadata["session_name"], row.Metadata["session_name_explicit"])
		}
	})
}

// noScanProvider hides the Fake's process-table scanner, as a provider
// without one presents.
type noScanProvider struct{ runtime.Provider }

// Guard: providers without scanner support and empty session IDs take the
// same fast paths as before the scan was bounded.
func TestPreStartOrphanScanFastPathsRemain(t *testing.T) {
	t.Run("provider without a process-table scanner", func(t *testing.T) {
		sp := newScriptedOrphanScan()
		mgr := newTestManager(t, beads.NewMemStore(), noScanProvider{sp})
		if err := mgr.killExistingOrphans(context.Background(), "sid"); err != nil {
			t.Fatalf("killExistingOrphans = %v, want nil without scanner support", err)
		}
		if got := sp.calls.Load(); got != 0 {
			t.Fatalf("scans = %d, want 0 without scanner support", got)
		}
	})
	t.Run("empty session id", func(t *testing.T) {
		sp := newScriptedOrphanScan()
		mgr := newTestManager(t, beads.NewMemStore(), sp)
		if err := mgr.killExistingOrphans(context.Background(), ""); err != nil {
			t.Fatalf("killExistingOrphans = %v, want nil for an empty session id", err)
		}
		if got := sp.calls.Load(); got != 0 {
			t.Fatalf("scans = %d, want 0 for an empty session id", got)
		}
	})
}
