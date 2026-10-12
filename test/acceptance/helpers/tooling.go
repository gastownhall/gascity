package acceptancehelpers

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

const (
	// EnvRequireTooling makes a missing bd or dolt fatal instead of a skip, and
	// so does a row precondition the bd under test cannot produce (see
	// MissingPrecondition): a job that sets it is one whose rows must run.
	EnvRequireTooling = "GC_REQUIRE_ACCEPTANCE_TOOLING"
	// EnvRequireLegacyGC makes a missing pre-journal gc fatal instead of a skip.
	EnvRequireLegacyGC = "GC_REQUIRE_ACCEPTANCE_LEGACY_GC"
	// EnvTopologyMatrix opts a run in to the full bd/dolt topology matrix.
	EnvTopologyMatrix = "GC_ACCEPTANCE_TOPOLOGY_MATRIX"
	// EnvProxiedNative is PR2's rollout flag: native reads over bd's proxy,
	// every mutation still through the bd CLI. It lives here rather than in the
	// test package because the shared harness has to be able to REMOVE it — a
	// flag-off lane that an ambient export can turn on is not a lane.
	EnvProxiedNative = "GC_BEADS_PROXIED_NATIVE"
)

// RequireTopologyMatrix skips unless the run opted in to the topology matrix.
//
// Every shape stands up real Dolt servers and a real bd proxy, and the matrix
// runs them back to back: on the Mac Tier A runner it reached 14 minutes and
// blew `make test-acceptance`'s 20-minute budget with M2-direct-local alone
// taking 11 of them. Tier A is a smoke tier, so the matrix is not its job.
// The Beads / topology acceptance workflow sets this and is where the shapes
// actually run, under GC_REQUIRE_ACCEPTANCE_TOOLING so it cannot pass by
// skipping them.
func RequireTopologyMatrix(t *testing.T) {
	t.Helper()
	if requireSwitchOn(EnvTopologyMatrix) {
		return
	}
	t.Skipf("the topology matrix stands up real Dolt servers per shape and does not fit the Tier A smoke budget; "+
		"set %s=1 (the Beads / topology acceptance job does) to run it", EnvTopologyMatrix)
}

// requireSwitchOn reports whether a GC_REQUIRE_* switch is on. Unset, empty and
// "0" are off; anything else is on.
func requireSwitchOn(name string) bool {
	v := strings.TrimSpace(os.Getenv(name))
	return v != "" && v != "0"
}

// MissingTooling reports a dependency the beads acceptance shapes cannot run
// without. It skips by default so a developer without bd or dolt on PATH still
// gets a useful local run, and fails under EnvRequireTooling.
//
// The switch exists because the skip is indistinguishable from a pass in a job
// summary. Every one of these tests skipped in every CI job for want of a bd,
// so a suite that had never executed read as green for as long as it took
// someone to check. A job whose whole purpose is to run them sets the switch,
// and a runner that loses its bd fails loudly instead of quietly running
// nothing.
func MissingTooling(t *testing.T, format string, args ...any) {
	t.Helper()
	skipOrFail(t, EnvRequireTooling, fmt.Sprintf(format, args...))
}

// MissingLegacyGC is the same contract for the pre-journal gc binary, on its
// own switch. The migration tests and the M5 shape need a gc from before the
// ownership journal, which no job builds yet; a separate switch lets CI demand
// bd and dolt without also demanding a binary nothing produces.
func MissingLegacyGC(t *testing.T, format string, args ...any) {
	t.Helper()
	skipOrFail(t, EnvRequireLegacyGC, fmt.Sprintf(format, args...))
}

// MissingPrecondition reports a row whose precondition this host, or the bd
// under test, did not produce — a database with no row for the row to remove,
// a record bd cleaned up that the row needs to find. It is MissingTooling's
// contract on the same switch: a skip for a developer's local run, and a
// failure under EnvRequireTooling.
//
// The switch is the right one because the jobs that set it are the ones whose
// rows are the evidence (the Beads / proxied-native acceptance job is
// required). An in-row t.Skip there let the job report success without
// running the row, visible only in a step summary nobody gates on; a bd that
// changed the behavior the row depends on must turn that job red, so somebody
// decides what the row should now measure.
func MissingPrecondition(t *testing.T, format string, args ...any) {
	t.Helper()
	missingPrecondition(t, format, args...)
}

func missingPrecondition(t skipOrFailer, format string, args ...any) {
	t.Helper()
	skipOrFail(t, EnvRequireTooling, fmt.Sprintf(format, args...))
}

// skipOrFailer is the part of *testing.T skipOrFail uses, so the choice it
// makes has a test that does not have to fail one.
type skipOrFailer interface {
	Helper()
	Skip(args ...any)
	Fatalf(format string, args ...any)
}

func skipOrFail(t skipOrFailer, env, reason string) {
	t.Helper()
	if requireSwitchOn(env) {
		t.Fatalf("%s is set, so this test must run, but %s", env, reason)
		return
	}
	t.Skip(reason)
}
