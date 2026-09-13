package storeref

// Relevance: which legs of a plan a caller's question can be answered from.
//
// # Why this is not a leg order
//
// The obvious way to make the controller's hot reads cheap is to put the
// binding first and stop at the first hit. It is also the way that recreates the
// D6 divergence in reverse: Plan(RoutedWork) puts the binding LAST on purpose
// (#5148), because binding-last is what makes a co-resident id answer from the
// work store on the demand side and from the work store on the claim side. A
// consumer that reorders the plan for itself has silently disagreed with every
// other consumer about which copy of a dual-resident bead is the real one.
//
// So the descriptor NARROWS and never reorders. A narrowed plan is a
// subsequence of its parent: same modes, same roles, same per-leg error
// policies, same relative order — with legs this plane may not read removed.
// That is a property, not a convention: TestNarrowKeepsPlanOrderAndOnlyDropsLegs
// asserts the subsequence directly, and the conformance corpus pins the rendered
// rows.
//
// # The plane doctrine
//
// The plane is a property of the CALLER, and the two are deliberately NOT
// complements.
//
//   - The RUNTIME plane is city operations: ticks, hooks, claims, sweeps,
//     census. It is a LATENCY contract, and it narrows to the infra/class
//     binding. Every bd operation on this plane hits the binding only; a
//     work-ledger leg here is a misrouting bug by definition, not a cost to
//     amortize (operator invariant 2026-08-15, bd memory
//     gascity-runtime-infra-store-invariant, ga-l7jdg). It is why a claim needed
//     a 240s window and why one tick leg cost 185s of a 360s tick.
//   - The RECONCILE plane is the rare, separately-scheduled convergence lane. It
//     is a CONVERGENCE contract and narrows NOTHING, because a store it skips is
//     a store nothing converges: the runtime plane over that same binding is
//     delta-only, so one dropped journal event would strand a binding-resident
//     bead permanently.
//
// # Where the descriptor is applied, and why it is not applied in Plan()
//
// Plan() answers "which stores can hold the answer to this question". That is a
// residency fact about the CITY and it does not change when a different process
// asks. The plane is a fact about the CALLER. Keeping the two separable is what
// lets one topology serve both lanes, lets a corpus row show the full plan and
// its narrowing side by side, and lets the convergence lane converge exactly the
// legs the runtime lane refused.
//
// # Why this surface needs no new boundary-guard row
//
// scripts/residency-boundary-patterns.txt ratchets every way a consumer can get
// STORES out of a plan and read them itself — the `[]beads.Store` literal, the
// `.Leg.Store` disassembly, and EachLeg, the sanctioned enumeration seam. Narrow
// hands back a ResolvedPlan, the same type Plan already returns, so a consumer
// still runs it through Union/Walk and the leg order and per-leg error policy are
// still applied by the executor. There is no new way to hold a leg list, so there
// is nothing new to count.
//
// # Per-template relevance is deliberately NOT a field here
//
// The original shape of this surface (ga-4qdfn item 1) was per-TEMPLATE leg
// relevance: the topology knows which stores a given pool's routed work
// materializes into, so a template whose formulas mint into the binding could
// have its legs narrowed to the binding. The operator invariant supersedes it:
// on the runtime plane the answer is the binding for EVERY template, so a
// per-template field would vary nothing this build can produce. It goes in when
// a plane needs two different answers for two templates, and not before.

import (
	"errors"
	"strconv"
)

// Plane is the caller's contract — the descriptor a consumer narrows a plan
// with. It is shared rather than re-derived per site: demand, the tick's delta
// lanes and the readers they must agree with all pass the same value, so a
// disagreement about which legs are relevant is a diff in one place.
type Plane int

const (
	// PlaneRuntime is city operations: ticks, hooks, claims, sweeps, census.
	// Infra/class binding only.
	PlaneRuntime Plane = iota

	// PlaneReconcile is the off-tick convergence lane, which reads every leg
	// because converging them is the whole reason it exists.
	PlaneReconcile
)

// String renders the plane as it appears in a corpus row and a diagnostic.
func (p Plane) String() string {
	switch p {
	case PlaneRuntime:
		return "runtime"
	case PlaneReconcile:
		return "reconcile"
	default:
		return "Plane(" + strconv.Itoa(int(p)) + ")"
	}
}

// Narrow applies the descriptor: it returns the subsequence of p's legs this
// plane may read, with the mode, the roles, the per-leg error policies and the
// relative order untouched.
//
// It errors when the narrowing would leave nothing to read, for the same reason
// Plan does: a legless plan is not an empty answer, and a Union over one reports
// zero rows as a complete one.
//
// # The single-store degradation
//
// A city that relocates no class has no binding, and there its work store IS its
// infra store. So the runtime rule is "the binding where one exists, otherwise
// the plan unchanged" — it degrades to "the only store there is", never to "no
// store at all", which would silently disable every runtime-plane reader on
// every single-store city. The same degradation covers a city whose binding
// resolved back to its work store: the plan's own dedupe already collapsed them,
// so there is one leg and it is both.
func Narrow(p ResolvedPlan, plane Plane) (ResolvedPlan, error) {
	if len(p.Legs) == 0 {
		return ResolvedPlan{}, errors.New("storeref: cannot narrow a plan with no legs — an empty leg list is a reader with nothing to read, not an empty answer")
	}
	if plane != PlaneRuntime || !p.TouchesBinding() {
		return p, nil
	}
	out := p
	out.Legs = make([]PlanLeg, 0, len(p.Legs))
	for _, l := range p.Legs {
		if IsClassRef(string(l.Leg.Ref)) {
			out.Legs = append(out.Legs, l)
		}
	}
	if len(out.Legs) == 0 {
		// Unreachable while TouchesBinding is the gate above — they read the same
		// predicate — but a narrowing that silently empties a plan is the exact
		// shape this package exists to refuse, so it fails loud rather than
		// relying on two predicates staying in step.
		return ResolvedPlan{}, errors.New("storeref: narrowing to the " + plane.String() + " plane left no readable leg")
	}
	return out, nil
}
