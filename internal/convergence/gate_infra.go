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
// Every entry is a typed error our own stack emits — not a heuristic about
// arbitrary gate output. Matching is case-insensitive against stderr only.
var gateInfraMarkers = []string{
	// internal/beads/factory.go: the native store could not be opened.
	"native_store_unavailable",
	// internal/config/pack_include.go: a locked remote import has no cache,
	// so the pack (and every formula in it) is invisible to this process.
	"locked but not cached",
	// internal/config/pack_include.go + internal/packman/check.go: the
	// remediation every uncached/stale pack-cache error carries.
	"gc import install",
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
