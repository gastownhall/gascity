package main

// T7 (boot-check-design.md §4.5): a controller that accepts a connection
// but never replies — hung, deadlocked, or just slow — must not cost a
// one-shot caller anywhere near its own budget. fetchContainmentLease's
// short timeouts (100ms dial/write, 250ms read) exist exactly for this: a
// hook that waited on the full 15s timeout before falling back would have
// made the thing slice 2 exists to fix (F1) worse, not better, on the one
// city where the controller is wedged.

import (
	"bytes"
	"testing"
	"time"
)

// hungControllerSocket accepts every connection and then holds it open
// forever — no read, no write, no close — until the listener itself is
// torn down on test cleanup. This is the one case acceptOnceAndReply
// cannot express: there is deliberately no reply, ever.
func hungControllerSocket(t *testing.T, cityDir string) {
	t.Helper()
	lis := listenControllerSocketForTest(t, cityDir)
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			// Held open deliberately; closed only when the listener
			// (and so the process) tears down at test cleanup.
			_ = conn
		}
	}()
}

// TestFetchContainmentLeaseHungListenerFallsBackFast proves
// fetchContainmentLease itself returns ok=false well inside its own
// 100ms/100ms/250ms budget, not after however long the hang lasts.
func TestFetchContainmentLeaseHungListenerFallsBackFast(t *testing.T) {
	cityDir := t.TempDir()
	hungControllerSocket(t, cityDir)

	start := time.Now()
	_, ok := fetchContainmentLease(cityDir)
	elapsed := time.Since(start)

	if ok {
		t.Fatal("a hung listener that never replies must not be accepted as a lease")
	}
	// Tight enough that a mutant widening the 250ms read timeout (to, say,
	// 1.5s) is caught here rather than passing a bound generous enough to
	// hide it: still comfortably above the ~450ms worst case (dial and
	// write should both succeed against an Accept-ing listener almost
	// instantly), but nowhere near the 15s hook budget this exists to
	// protect.
	if elapsed > time.Second {
		t.Fatalf("fetchContainmentLease took %s against a hung listener, want well under its own ~450ms worst case budget, nowhere near a 15s hook timeout", elapsed)
	}
}

// TestFunnelFallsBackToTheFullCheckOnAHungListener is the integration case:
// the CLI funnel's own leased call shape, against a converged city whose
// controller is hung, must still return the correct Converged verdict —
// from the full work-store check, not the lease — and still do it fast.
func TestFunnelFallsBackToTheFullCheckOnAHungListener(t *testing.T) {
	cityPath, cfg, _, _ := convergedCityWithRecheckLease(t)
	hungControllerSocket(t, cityPath)

	start := time.Now()
	var log bytes.Buffer
	report := checkInfraClassConvergenceWith(cityPath, cfg, "gc start", &log, storageBootGateOpts{lease: true})
	elapsed := time.Since(start)

	if report.Outcome != infraMigrationConverged {
		t.Fatalf("leased re-check behind a hung controller = %s, want converged from the fallback full check: %s", report.Outcome, log.String())
	}
	if elapsed > 2*time.Second {
		t.Fatalf("the funnel took %s to fall back past a hung controller, want nowhere near a 15s hook timeout", elapsed)
	}
}
