package main

// Slice 2 of the containment lease (#9237 follow-up, boot-check-design.md
// §4.4-§4.5): the one-shot consumer. storage_containment_lease.go is the
// controller's side — it publishes a lease and serves it over the socket.
// This file is the other end: a one-shot process that asks for one, checks
// it against the facts it reads from files itself, and either trusts it or
// falls back to the full check it exists to avoid. Nothing here ever
// decides a refusal (Q7): acceptContainmentLease rejects anything that is
// not already a serving or degraded-serving verdict, and every rejection —
// for any reason at all — means "run the full check", never "refuse".

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"
)

// containmentLeaseKillSwitchEnv disables the whole containment-lease feature
// when set to "off" (case-insensitive), in every process, not only the
// one-shot consumer this file otherwise belongs to (owner decision):
//
//   - the controller publishes and serves no lease (publishBootContainmentLease,
//     strandRecheck.publishLease);
//   - the controller's recheck falls back to the fixed strandRecheckInterval
//     cadence instead of the adaptive one (strandRecheckNextInterval);
//   - a one-shot process's fetchContainmentLease behaves as though no
//     controller answered, so every caller falls back to the full check,
//     same as today.
//
// It is read once per process (containmentLeaseDisabledOnce), not on every
// call: every site above must agree for the life of the process, rather
// than one seeing the switch flip mid-boot while another does not.
const containmentLeaseKillSwitchEnv = "GC_STORAGE_CONTAINMENT_LEASE"

// containmentLeaseDisabledOnce caches containmentLeaseDisabled's answer for
// the life of the process. A var, not a plain sync.Once wrapper, so a test
// can replace it (resetContainmentLeaseKillSwitchForTest) to observe a
// t.Setenv take effect, the way every other per-process cache in this
// package is made testable.
var containmentLeaseDisabledOnce = sync.OnceValue(readContainmentLeaseKillSwitch)

func readContainmentLeaseKillSwitch() bool {
	return strings.EqualFold(os.Getenv(containmentLeaseKillSwitchEnv), "off")
}

// containmentLeaseDisabled reports whether the kill switch is set, read
// once per process.
func containmentLeaseDisabled() bool {
	return containmentLeaseDisabledOnce()
}

// Slice 2's own socket timeouts (design doc §4.5): short enough that a hung
// controller costs a one-shot process at most a few hundred milliseconds
// before it falls back to the full check (T7) — nowhere near the 15s hook
// timeout F1 measured processes silently dropping work past.
const (
	containmentLeaseDialTimeout  = 100 * time.Millisecond
	containmentLeaseWriteTimeout = 100 * time.Millisecond
	containmentLeaseReadTimeout  = 250 * time.Millisecond
)

// containmentLeaseAcceptCap is the caller's OWN bound on a lease's age,
// independent of the fresh_ms the controller served it with (design doc
// §4.3, carry-over 4): a controller whose adaptive interval has grown very
// large still cannot hand out a lease older than this, no matter what
// fresh_ms says. It equals strandRecheckFreshCap, the controller's own
// bound, but is declared separately: the two sides agreeing is the design
// (Q3's single 10-minute number), not a shared constant one side could
// change without the other noticing.
const containmentLeaseAcceptCap = 10 * time.Minute

// fetchContainmentLease asks cityPath's controller for its live containment
// lease (storageVerdictCommand), with slice 2's short timeouts. ok is false
// whenever this process cannot be sure it got a real, current answer: the
// kill switch, no controller, an old controller, a hung one, or a reply
// that will not parse all look the same here — the caller runs the full
// check either way (design doc §4.6).
func fetchContainmentLease(cityPath string) (wire containmentLeaseWire, ok bool) {
	if containmentLeaseDisabled() {
		return containmentLeaseWire{}, false
	}
	resp, err := sendControllerCommandWithTimeouts(cityPath, storageVerdictCommand,
		containmentLeaseDialTimeout, containmentLeaseWriteTimeout, containmentLeaseReadTimeout)
	if err != nil {
		return containmentLeaseWire{}, false
	}
	if err := json.Unmarshal(resp, &wire); err != nil {
		return containmentLeaseWire{}, false
	}
	return wire, true
}

// acceptContainmentLease decides whether wire — already fetched for target
// — agrees closely enough with basis, captured by THIS process from its own
// files, to stand in for confirmInfraContainment's own work-store read
// (design doc §4.4). Every condition must hold:
//
//   - the wire version is the one this process speaks (v==1);
//   - the verdict is serving or degraded-serving (Q7: a refusal never comes
//     from the lease — and nor does a lease published for an outcome that
//     is neither, like Retained, which newContainmentLease still allows
//     through on the controller side);
//   - the verdict is complete (no id list was dropped for being over
//     leaseListCap; carry-over 5: complete:false always takes the full
//     check);
//   - the lease names the same binding and database this process resolved;
//   - both bases are non-empty and equal, field for field;
//   - the lease's age is within its own fresh_ms AND within this caller's
//     own 10-minute cap, independent of fresh_ms (carry-over 4);
//   - if the lease is marked recheck_failing (Q3: accepted for up to the
//     10-minute cap), the failure that kept it from refreshing must not
//     itself have been a work-store read (carry-over 3) — a fault there is
//     a fault in the very thing this lease stands in for.
//
// Anything else returns ok=false, and the caller runs the full check; the
// returned report is only meaningful when ok is true.
func acceptContainmentLease(wire containmentLeaseWire, target infraBindingTarget, basis containmentBasis) (report infraMigrationReport, ok bool) {
	if wire.V != containmentLeaseProtocolVersion {
		return infraMigrationReport{}, false
	}
	if !wire.Serving && !wire.ServesDegraded {
		return infraMigrationReport{}, false
	}
	if !wire.Complete {
		return infraMigrationReport{}, false
	}
	if wire.Binding != target.Binding || wire.Database != target.Database {
		return infraMigrationReport{}, false
	}
	if basis.empty() || wire.Basis.empty() || basis != wire.Basis {
		return infraMigrationReport{}, false
	}
	// Compared entirely in int64 milliseconds: converting an attacker- or
	// bug-supplied AgeMS to a time.Duration (nanoseconds) by multiplying by
	// time.Millisecond can overflow int64 for an age nowhere near this
	// process's own clock (18446744073710ms, ~584 years, wraps to a small or
	// negative duration and would otherwise be accepted).
	if wire.AgeMS < 0 || wire.AgeMS > containmentLeaseAcceptCap.Milliseconds() {
		return infraMigrationReport{}, false
	}
	if wire.AgeMS > wire.FreshMS {
		return infraMigrationReport{}, false
	}
	if wire.RecheckFailing && wire.RecheckFailingFromWorkStore {
		return infraMigrationReport{}, false
	}
	var outcome infraMigrationOutcome
	switch wire.Outcome {
	case infraMigrationConverged.String():
		outcome = infraMigrationConverged
	case infraMigrationStranded.String():
		outcome = infraMigrationStranded
	default:
		// Genesis is deliberately not accepted here, even though
		// newContainmentLease publishes a lease for it: it is the FIRST
		// definitive verdict a city can ever carry, and accepting it over
		// the wire would let a one-shot process treat "no infrastructure
		// bead has ever landed" as settled fact from a stale answer instead
		// of its own fresh read, for an outcome whose whole point is a
		// one-time transition this build never gets to repeat.
		// A version mismatch takes care of a wire shape this build does not
		// speak; this takes care of an outcome string it speaks but does
		// not recognize (garbage, or a future outcome this build predates).
		return infraMigrationReport{}, false
	}
	report = infraMigrationReport{
		Outcome:        outcome,
		Target:         target,
		ProvenBeads:    wire.ProvenBeads,
		LostCrossEdges: wire.LostCrossEdges,
		Stranded:       wire.Stranded,
		PresumedReused: wire.PresumedReused,
		Declassifiable: wire.Declassifiable,
	}
	// A defensive cross-check, not a condition the wire can fail on its
	// own terms: Serving and ServesDegraded are precomputed by the
	// publisher (design review MUST 1), and report.serving() /
	// .servesDegraded() recompute the same answer from Outcome and the
	// fields just above. The two must agree; if they do not, something
	// upstream is inconsistent in a way the rest of this file cannot
	// reason about, and the only safe answer is the full check.
	if report.serving() != wire.Serving || report.servesDegraded() != wire.ServesDegraded {
		return infraMigrationReport{}, false
	}
	return report, true
}

// leasedContainmentReport is confirmInfraConvergenceWith's lease path: fetch
// a lease, capture this process's own basis, and accept or reject it. ok is
// false for anything that sends the caller to the full check, including a
// basis-capture failure — the same failure confirmInfraContainment would go
// on to hit anyway, by a different read.
func leasedContainmentReport(cityPath string, target infraBindingTarget) (infraMigrationReport, bool) {
	wire, ok := fetchContainmentLease(cityPath)
	if !ok {
		return infraMigrationReport{}, false
	}
	basis, err := captureContainmentBasis(cityPath, target)
	if err != nil {
		return infraMigrationReport{}, false
	}
	return acceptContainmentLease(wire, target, basis)
}
