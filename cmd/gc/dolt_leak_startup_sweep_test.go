package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// doltProcRootedAt builds a dolt sql-server process whose --config lies under
// root, matching the layout a managed dolt provider produces.
func doltProcRootedAt(pid int, root string) DoltProcInfo {
	configPath := filepath.Join(root, ".gc", "runtime", "packs", "dolt", "dolt-config.yaml")
	return DoltProcInfo{PID: pid, Argv: []string{"dolt", "sql-server", "--config=" + configPath}}
}

// sweepStaleForTest runs the startup sweep against a scripted process list and
// returns what it decided to reap, without signalling anything real.
func sweepStaleForTest(g *doltLeakGuardedTestingM, procs []DoltProcInfo) (swept bool, reaped []DoltProcInfo) {
	swept = g.sweepStaleCmdGCTestDoltProcessesWith(
		"startup",
		func() ([]DoltProcInfo, error) { return procs, nil },
		func(leaked []DoltProcInfo) { reaped = leaked },
	)
	return swept, reaped
}

// TestStartupSweepReapsSourceRootRootedServer is the across-invocation leak
// this whole guard exists for. A managed dolt handed a city root of "" or "."
// resolves it to the package directory, so its config lands in the checkout at
// cmd/gc/.gc rather than under tempRoot. Such a server outlives the test binary
// that spawned it, and the in-run snapshot diff can never report it: it is
// already present in the INITIAL snapshot, so diffDoltProcessSnapshots
// correctly excludes it as "not leaked by this run". The startup sweep is the
// only path that can reap it, and before this change it could not classify it —
// staleness was derived solely from an owner PID encoded in a temp-root
// directory name, which a checkout-rooted path does not have.
func TestStartupSweepReapsSourceRootRootedServer(t *testing.T) {
	sourceRoot := t.TempDir()
	g := &doltLeakGuardedTestingM{tempRoot: t.TempDir(), sourceRoot: sourceRoot}

	proc := doltProcRootedAt(4242, sourceRoot)
	swept, reaped := sweepStaleForTest(g, []DoltProcInfo{proc})

	if !swept {
		t.Fatalf("startup sweep reported nothing to do for a server rooted in the checkout at %q", sourceRoot)
	}
	if len(reaped) != 1 || reaped[0].PID != 4242 {
		t.Fatalf("startup sweep reaped %v, want exactly pid 4242", reaped)
	}
}

// TestStartupSweepIgnoresForeignCheckoutServer is the blast-radius guard. Many
// agents run cmd/gc tests concurrently from separate worktrees on one box, so
// the sweep must key on THIS checkout's source root only. Reaping a peer
// checkout's live server would be a worse bug than the leak being fixed.
func TestStartupSweepIgnoresForeignCheckoutServer(t *testing.T) {
	g := &doltLeakGuardedTestingM{tempRoot: t.TempDir(), sourceRoot: t.TempDir()}

	foreignCheckout := t.TempDir()
	swept, reaped := sweepStaleForTest(g, []DoltProcInfo{doltProcRootedAt(4243, foreignCheckout)})

	if swept || len(reaped) != 0 {
		t.Fatalf("startup sweep reaped %v from a foreign checkout %q; must touch this checkout only", reaped, foreignCheckout)
	}
}

// TestStartupSweepWithUnresolvedSourceRootReapsNothing pins that an empty
// source root is ignored rather than treated as match-everything. os.Getwd can
// fail, and newDoltLeakGuardedTestingM deliberately degrades to an empty root
// instead of aborting the run; a bare prefix test against "" would match every
// dolt process on the machine and turn this sweep into a box-wide reaper.
func TestStartupSweepWithUnresolvedSourceRootReapsNothing(t *testing.T) {
	g := &doltLeakGuardedTestingM{tempRoot: t.TempDir(), sourceRoot: ""}

	swept, reaped := sweepStaleForTest(g, []DoltProcInfo{doltProcRootedAt(4244, t.TempDir())})

	if swept || len(reaped) != 0 {
		t.Fatalf("startup sweep with an unresolved source root reaped %v; want nothing", reaped)
	}
}

// TestStartupSweepStillReapsDeadOwnerTempRootServer pins that widening the
// sweep to the source root leaves the pre-existing owner-PID path intact: a
// temp-root server whose owning test binary is gone is still stale.
func TestStartupSweepStillReapsDeadOwnerTempRootServer(t *testing.T) {
	tempParent := os.TempDir()
	g := &doltLeakGuardedTestingM{
		tempRoot:   filepath.Join(tempParent, fmt.Sprintf("%s%d-current", testCmdGCTempRootPrefix, os.Getpid())),
		sourceRoot: t.TempDir(),
	}

	deadOwnerRoot := filepath.Join(tempParent, fmt.Sprintf("%s%d-stale", testCmdGCTempRootPrefix, nonLivePID(t)))
	swept, reaped := sweepStaleForTest(g, []DoltProcInfo{doltProcRootedAt(4245, deadOwnerRoot)})

	if !swept || len(reaped) != 1 || reaped[0].PID != 4245 {
		t.Fatalf("startup sweep reaped %v for a dead-owner temp root; want exactly pid 4245", reaped)
	}
}
