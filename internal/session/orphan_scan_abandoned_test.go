package session

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// A scan abandoned at its budget may answer later, after its caller has
// deferred the start and gone. Its late roots must be discarded, not acted
// on: the next start scans fresh and terminates what that scan found, and
// nothing else. Terminating the abandoned scan's roots anyway would kill
// processes the fresh scan knows nothing about (they may belong to the
// replacement start itself by then).
func TestKillExistingOrphansDiscardsTheAbandonedScansLateRoots(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newScriptedOrphanScan()
		sp.blockLeading.Store(1)
		defer sp.unblock()
		mgr := newTestManager(t, beads.NewMemStore(), sp)
		city := mgr.cityPath
		rootA := runtime.LiveRuntime{SessionID: "sid-late", City: city, PID: 100}
		rootB := runtime.LiveRuntime{SessionID: "sid-late", City: city, PID: 200}

		if err := orphanWatchdog(t, func() error { return mgr.killExistingOrphans(context.Background(), "sid-late") }); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("first cleanup = %v, want unavailable at the scan budget", err)
		}

		// The abandoned scan finally answers root A. Its caller is long gone.
		sp.setAnswer([]runtime.LiveRuntime{rootA}, nil)
		sp.unblock()
		synctest.Wait() // the abandoned scan goroutine has returned and freed the key

		sp.setAnswer([]runtime.LiveRuntime{rootB}, nil)
		if err := mgr.killExistingOrphans(context.Background(), "sid-late"); err != nil {
			t.Fatalf("second cleanup = %v, want nil", err)
		}
		if got := sp.calls.Load(); got != 2 {
			t.Fatalf("scans = %d, want a fresh second scan after the first answered", got)
		}
		pids := orphanScanTerminatedPIDs(sp)
		if len(pids) != 1 || pids[0] != "200" {
			t.Fatalf("terminated %v, want only the fresh scan's root 200, never the abandoned scan's 100", pids)
		}
	})
}
