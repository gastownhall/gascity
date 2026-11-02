package session

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// The scan is bounded, but the terminations after it must respect the start
// context too: a start whose context ends mid-sweep stops between
// terminations instead of grinding through every root a dead start asked for.
func TestKillExistingOrphansStopsBetweenTerminationsWhenContextDone(t *testing.T) {
	sp := newScriptedOrphanScan()
	mgr := newTestManager(t, beads.NewMemStore(), sp)
	city := mgr.cityPath
	sp.setAnswer([]runtime.LiveRuntime{
		{SessionID: "sid", City: city, PID: 100},
		{SessionID: "sid", City: city, PID: 200},
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sp.onTerminate = func(int) { cancel() }

	err := mgr.killExistingOrphans(ctx, "sid")

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("killExistingOrphans = %v, want it to wrap context.Canceled", err)
	}
	if pids := orphanScanTerminatedPIDs(sp); len(pids) != 1 || pids[0] != "100" {
		t.Fatalf("terminated %v, want only the first root: the sweep stops when the start context ends", pids)
	}
}

// Stopping early must not swallow the terminations that already failed: the
// error carries both the failed kills and the cancellation.
func TestKillExistingOrphansKeepsTerminationErrorsWhenCancellingMidway(t *testing.T) {
	sp := newScriptedOrphanScan()
	mgr := newTestManager(t, beads.NewMemStore(), sp)
	city := mgr.cityPath
	sp.setAnswer([]runtime.LiveRuntime{
		{SessionID: "sid", City: city, PID: 100},
		{SessionID: "sid", City: city, PID: 200},
	}, nil)
	killRefused := errors.New("kill refused")
	sp.terminateErrs = map[int]error{100: killRefused}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var attempted []int
	sp.onTerminate = func(pid int) {
		attempted = append(attempted, pid)
		cancel()
	}

	err := mgr.killExistingOrphans(ctx, "sid")

	if !errors.Is(err, killRefused) || !errors.Is(err, context.Canceled) {
		t.Fatalf("killExistingOrphans = %v, want the failed kill and the cancellation both carried", err)
	}
	if len(attempted) != 1 || attempted[0] != 100 {
		t.Fatalf("termination attempts = %v, want only the first root", attempted)
	}
}

// Guard: a scan that completed wrapping ErrRuntimeUnavailable is still an
// answered scan. Its roots are swept as before (fail closed on the scan, not
// on its roots); only an UNANSWERED scan defers the start.
func TestKillExistingOrphansCompletedScanWrappingUnavailableStillTerminates(t *testing.T) {
	sp := newScriptedOrphanScan()
	mgr := newTestManager(t, beads.NewMemStore(), sp)
	city := mgr.cityPath
	sp.setAnswer([]runtime.LiveRuntime{{SessionID: "sid", City: city, PID: 100}},
		fmt.Errorf("%w: listing lost", runtime.ErrRuntimeUnavailable))

	err := mgr.killExistingOrphans(context.Background(), "sid")
	if err != nil {
		t.Fatalf("killExistingOrphans = %v, want nil for a completed scan that reported roots", err)
	}
	if pids := orphanScanTerminatedPIDs(sp); len(pids) != 1 || pids[0] != "100" {
		t.Fatalf("terminated %v, want the answered scan's root", pids)
	}
}
