package convergence

import "strings"

// GateInfraExitCode is the exit status a gate command uses to say "I could not
// reach the infrastructure I need" rather than "the thing I check is not true".
// It is EX_TEMPFAIL from sysexits.h, the same convention the repo's own scripts
// already use to separate a temporary-unavailable outcome from a real failure
// (see scripts/push-gate-lock-lib.sh and TESTING.md).
//
// Gates are shell commands, so this is the protocol a gate author opts into:
// exit 75 and the dispatcher treats the run as an infra outcome instead of a
// verdict. No Go code inspects what the gate was checking.
const GateInfraExitCode = 75

// gateInfraMarkers are the failure strings Gas City's own tools print when a
// read cannot see its infrastructure. They are a defense-in-depth layer under
// GateInfraExitCode, for gate commands that simply propagate `gc`/`bd` output
// and their bare nonzero exit status.
//
// Matching is case-insensitive against stderr only, and a match costs a full
// attempt-free gate re-run (up to the dispatcher's infra budget), so an entry
// has to satisfy two conditions, not one:
//
//  1. it is a typed error our own stack emits, not a heuristic about arbitrary
//     gate output; and
//  2. it only appears when the read actually failed — never on a recovered
//     path, and never as remediation advice attached to some other failure.
//
// Condition 2 is what keeps a genuine gate verdict from being re-run 20 times.
// Two strings that satisfy (1) and fail (2) are deliberately absent:
//
//   - "native_store_unavailable" (internal/beads/factory.go) is logged on a
//     *recovered* path: every logNativeUnavailable call is immediately followed
//     by openBdFallback, so in any native-ineligible deployment it prints on
//     every store-open while the read succeeds. Treating it as blindness makes
//     the first real gate failure in such a city burn the whole infra budget.
//   - "gc import install" is a remediation substring, not an error: it is the
//     generic RepairHint in internal/config/pack_include.go,
//     internal/packman/check.go and cmd/gc/suggest.go, and it is also
//     cmd/gc/cmd_import.go's own error prefix, so a genuine `gc import install`
//     failure matches it. The typed pack-cache blindness it was meant to cover
//     is already covered by "locked but not cached" below.
var gateInfraMarkers = []string{
	// internal/config/pack_include.go: a locked remote import has no cache,
	// so the pack (and every formula in it) is invisible to this process.
	"locked but not cached",
	// internal/config/implicit.go: no GC_HOME, so the repo cache root is
	// unresolvable.
	"no gc_home available",
	// Dolt/MySQL error 1045: the store refused the connection because no
	// usable credential reached it. This is the shape ga-pqlgh produces when
	// the credentials file is invisible under the gate sandbox. Matching a
	// typed data-plane error code mirrors what the doctor store preflight
	// already does for error 1040 (too many connections).
	"error 1045",
	"access denied for user",
}

// ClassifyInfraBlind reports whether a gate command that ran to completion
// failed because it could not see its infrastructure — a blind read — rather
// than because the condition it checks is not yet true.
//
// A blind read is an infra outcome: the gate never produced a verdict, so it
// must be re-run without consuming a semantic attempt. Callers normalize a
// blind result to GateError, which already has a bounded attempt-free re-run
// path.
//
// The returned reason names the protocol signal that matched ("exit_75" or the
// marker string) so traces record why the reclassification happened. Results
// that already carry an infra outcome (GateError/GateTimeout) and passing
// results are never blind.
func ClassifyInfraBlind(result GateResult) (string, bool) {
	if result.Outcome != GateFail {
		return "", false
	}
	if result.ExitCode != nil && *result.ExitCode == GateInfraExitCode {
		return "exit_75", true
	}
	stderr := strings.ToLower(result.Stderr)
	for _, marker := range gateInfraMarkers {
		if strings.Contains(stderr, marker) {
			return marker, true
		}
	}
	return "", false
}
