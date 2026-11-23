package main

// T4 (boot-check-design.md §4.4, §4.5): fetchContainmentLease is reached
// only through confirmInfraConvergenceWith's opts.lease branch, which only
// cli_storage_routes.go's two funnel call sites ever set. Every other
// production entry point into the convergence machinery passes the zero
// storageBootGateOpts, so it can never fetch — this file proves that
// dynamically, over a real socket, rather than resting on the static
// single-caller argument alone.

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestFetchContainmentLeaseNeverDialsOutsideTheFunnel runs the production
// entry points that do NOT go through the CLI funnel against a converged
// city whose controller socket is live and counting, and requires every one
// of them to leave the counter at zero.
func TestFetchContainmentLeaseNeverDialsOutsideTheFunnel(t *testing.T) {
	// Each case gets its OWN converged city and its own socket: these
	// entry points open real bindings and write real served-binding notes
	// as a side effect of a normal boot, so sharing one city across
	// repeated boots here would risk tripping an unrelated hold
	// (servedBindingNoteHold) that has nothing to do with what this test
	// is checking.
	t.Run("controller boot", func(t *testing.T) {
		cityPath, cfg, _, _ := convergedCityWithRecheckLease(t)
		count := countingControllerSocket(t, cityPath)
		var diag bytes.Buffer
		routes, err := storageBootGate(cityPath, cfg, "gc start", nil, &diag)
		if err != nil {
			t.Fatalf("boot refused: %v\n%s", err, diag.String())
		}
		_ = routes.close()
		if got := count.Load(); got != 0 {
			t.Fatalf("controller boot dialed the controller socket %d time(s)", got)
		}
	})

	t.Run("v2 boot probe", func(t *testing.T) {
		cityPath, cfg, _, _ := convergedCityWithRecheckLease(t)
		count := countingControllerSocket(t, cityPath)
		_ = v2DefaultBootRefusal(cityPath, "city", cfg, nil)
		if got := count.Load(); got != 0 {
			t.Fatalf("the v2 boot probe dialed the controller socket %d time(s)", got)
		}
	})

	t.Run("storage status", func(t *testing.T) {
		cityPath, cfg, _, _ := convergedCityWithRecheckLease(t)
		count := countingControllerSocket(t, cityPath)
		var stdout, stderr bytes.Buffer
		doStorageStatus(storageOperatorRequest{CityPath: cityPath, Cfg: cfg}, &stdout, &stderr)
		if got := count.Load(); got != 0 {
			t.Fatalf("storage status dialed the controller socket %d time(s): %s%s", got, stdout.String(), stderr.String())
		}
	})

	t.Run("storage preflight", func(t *testing.T) {
		cityPath, cfg, _, _ := convergedCityWithRecheckLease(t)
		count := countingControllerSocket(t, cityPath)
		var stdout, stderr bytes.Buffer
		doStoragePreflight(storageOperatorRequest{CityPath: cityPath, Cfg: cfg, FleetStopped: true}, &stdout, &stderr)
		if got := count.Load(); got != 0 {
			t.Fatalf("storage preflight dialed the controller socket %d time(s): %s%s", got, stdout.String(), stderr.String())
		}
	})

	t.Run("migrate recheck", func(t *testing.T) {
		cityPath, _, target, _ := convergedCityWithRecheckLease(t)
		count := countingControllerSocket(t, cityPath)
		_ = confirmInfraConvergence(cityPath, target, "gc storage migrate", io.Discard)
		if got := count.Load(); got != 0 {
			t.Fatalf("the operator command's own re-check dialed the controller socket %d time(s)", got)
		}
	})

	t.Run("strand recheck tick", func(t *testing.T) {
		cityPath, _, target, _ := convergedCityWithRecheckLease(t)
		count := countingControllerSocket(t, cityPath)
		recheck := &strandRecheck{cityPath: cityPath, target: target, stderr: io.Discard, logPrefix: "gc start"}
		recheck.tick()
		if got := count.Load(); got != 0 {
			t.Fatalf("the strand recheck's own tick dialed the controller socket %d time(s)", got)
		}
	})
}

// TestFetchContainmentLeaseNeverDialsOnReverse is the reverse command's own
// fixture (newReverseEngineCity), checked against the same counter: a
// reverse never consults a lease either way, and this fixture's forward
// migration + reverse attempt is a heavier setup than the others share, so
// it gets its own city and its own socket.
func TestFetchContainmentLeaseNeverDialsOnReverse(t *testing.T) {
	c := newReverseEngineCity(t, 3, "")
	count := countingControllerSocket(t, c.request.CityPath)
	c.reverse(t, reverseOptions{Mode: reverseToWork})
	if got := count.Load(); got != 0 {
		t.Fatalf("storage reverse dialed the controller socket %d time(s)", got)
	}
}

// TestFetchContainmentLeaseNeverDialsOnRecoverStranded is the recover
// command's own fixture, same reasoning as the reverse test above.
func TestFetchContainmentLeaseNeverDialsOnRecoverStranded(t *testing.T) {
	cityPath := recoveryCityPath(t)
	source := stubStrandedRecoverySource(t)
	mustCreateInfraBead(t, source, beads.Bead{Title: "live session", Type: "session", Labels: []string{"gc:session"}})
	cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
	var log bytes.Buffer
	if got := migrateInfraClassesRetainingSource(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("cutover = %s: %s", got.Outcome, log.String())
	}
	count := countingControllerSocket(t, cityPath)
	var stdout, stderr bytes.Buffer
	request := storageOperatorRequest{CityPath: cityPath, Cfg: cfg, FleetStopped: true}
	doStorageRecoverStranded(context.Background(), request, strandedRecoveryOptions{DryRun: true}, &stdout, &stderr)
	if got := count.Load(); got != 0 {
		t.Fatalf("storage recover-stranded dialed the controller socket %d time(s)", got)
	}
}

// TestFetchContainmentLeaseDialsOnlyThroughTheFunnel is the control for all
// of the above: the one call shape that DOES set opts.lease must actually
// reach the socket, so a wiring break that silently stopped BOTH sides (the
// funnel and everything else) could not pass the zero-counter tests above
// by accident.
func TestFetchContainmentLeaseDialsOnlyThroughTheFunnel(t *testing.T) {
	cityPath, cfg, _, _ := convergedCityWithRecheckLease(t)
	count := countingControllerSocket(t, cityPath)
	var diag bytes.Buffer
	_ = checkInfraClassConvergenceWith(cityPath, cfg, "gc start", &diag, storageBootGateOpts{lease: true})
	if got := count.Load(); got == 0 {
		t.Fatal("the leased re-check never dialed the controller socket; this test's counter cannot be trusted by the zero-count tests above either")
	}
}
