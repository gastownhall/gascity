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
	"time"
)

// containmentLeaseKillSwitchEnv disables slice 2's lease consumer entirely
// when set to "off" (case-insensitive): fetchContainmentLease then behaves
// as though no controller answered, and every caller falls back to the
// full check, same as today. It changes nothing on the controller side —
// storage_containment_lease.go keeps publishing and serving leases — so
// turning it back on needs no restart.
const containmentLeaseKillSwitchEnv = "GC_STORAGE_CONTAINMENT_LEASE"

// containmentLeaseDisabled reports whether the kill switch is set.
func containmentLeaseDisabled() bool {
	return strings.EqualFold(os.Getenv(containmentLeaseKillSwitchEnv), "off")
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
	age := time.Duration(wire.AgeMS) * time.Millisecond
	if age < 0 || age > containmentLeaseAcceptCap {
		return infraMigrationReport{}, false
	}
	if age > time.Duration(wire.FreshMS)*time.Millisecond {
		return infraMigrationReport{}, false
	}
	if wire.RecheckFailing && wire.RecheckFailingFromWorkStore {
		return infraMigrationReport{}, false
	}
	var outcome infraMigrationOutcome
	switch wire.Outcome {
	case infraMigrationConverged.String():
		outcome = infraMigrationConverged
	case infraMigrationGenesis.String():
		outcome = infraMigrationGenesis
	case infraMigrationStranded.String():
		outcome = infraMigrationStranded
	default:
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
