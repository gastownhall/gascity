package storeref

// The residency resolver: one authority for "which stores answer this
// question?"
//
// The typed-store split shipped a PLACEMENT contract — every class has exactly
// one authoritative accessor for "which store OWNS creates of class C"
// (cmd/gc/class_store.go's resolveClassStore). It never shipped a LOOKUP
// contract, and lookup was answered instead at ~90 call sites, each assembling
// its own store list from two binding-blind base enumerators plus a per-site
// re-derivation of the identity gate, the leg order, the dedupe rule and the
// fail-loud policy. Conventions do not survive 90 call sites: the census counted
// 31 restatements in the query class alone, and each restatement was a chance to
// get one clause wrong.
//
// So the rules live here, once, each with the incident that proved it:
//
//   ByID, id inside a binding's reserved namespace
//     [binding, work, shadows most-specific-first]. The binding leads because
//     it is the sole MINTER of the namespace — but minting is not holding, and
//     a reserved prefix is only ADVISORY on work stores, so the list is PROBED
//     and never routed (#5128).
//
//   ByID, id outside every reserved namespace
//     [binding as a residence probe, work, shadows]. `gc storage migrate`
//     preserves ids, so a relocated bead keeps its work-era prefix; nil from
//     the namespace gate means "cannot decide", never "work owns it"
//     (ga-axin6/#5245, #5132). Retired per ClassBinding.MintsReserved.
//
//   ByID, single-store city
//     [work], returned unprobed. The identity fast-path: a city that relocates
//     nothing pays nothing for a seam it cannot use.
//
//   RoutedWork
//     [work, rigs ascending, binding LAST], first-leg-wins dedupe (#5148).
//     Binding-last is what keeps a co-resident id answering from work, so the
//     demand surface agrees with the claim surface.
//
//   AssignedWork, claim escalation
//     work legs FIRST, binding last, escalating only on a PROVEN all-work-legs
//     not-found. Deliberately the OPPOSITE order to ByID for a co-resident id:
//     the claim must land where the reader serves from (#5148/#5158/#5161,
//     ga-bvdha). The asymmetry is a documented property of the intent PAIR,
//     not a per-site accident.
//
//   AssignedWork, sweeps (gates, releases, re-wake)
//     work + rigs + EVERY binding, as a union. A work-led scan cannot see
//     claim-routed assignees (ga-whzrt, and the sibling sweeps that lacked the
//     leg).
//
//   Session
//     [sessions binding, work, rigs] as a union. Single-leg SUBSTITUTION is the
//     session-verb blindness: graph-run session beads in work stores and
//     pre-relocation residue are both real (#5187 lineage).
//
//   Census
//     work AND rigs AND every binding, each a DISTINCT ref'd leg. The binding
//     must not REPLACE the city work leg — the accidental dual role that let a
//     city-scope routed work bead go invisible to the controller tick.
//
//   Class
//     exactly one leg: the binding serving the class, else work. The placement
//     contract, unchanged.
//
// What the resolver is NOT: an I/O layer (it opens nothing and caches nothing),
// a per-class fan-out engine (Topology can EXPRESS a per-class split and the
// corpus asserts it, but no constructor in this build produces one), and a
// judgment holder (leg order and error policy are transport facts).

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// Intent is a QUESTION about residency. The six are the census's own pattern
// classes; the closed interface is what stops a seventh from being answered by
// a hand-rolled store list somewhere else.
type Intent interface{ intent() }

// ByID is one bead, read or write-after-read.
type ByID struct {
	ID string

	// WorkAxis, when set, supplies the work legs for an id NO binding namespace
	// claims — and is consulted for nothing else.
	//
	// This package models a PROBED work axis: the work ledger, then the work
	// stores whose configured prefix also covers the id. internal/api's by-id
	// surface has a ROUTED one instead — longest configured prefix, then a
	// routes.jsonl lookup on disk, then a full scan of every work store — and
	// that router's answer is a route, not a probe list. Replacing it with this
	// package's rule would make a routes.jsonl-routed id and an id matching no
	// configured prefix unreachable, and would put the city work store AHEAD of
	// the rig store that actually mints the id, so a city-store outage would
	// answer for reads the rig was serving. So the plane keeps its router and
	// declares its answer here, and the resolver still owns the part that was
	// blind: the binding legs.
	//
	// It is an interface rather than a plain []Leg because it must be LAZY, for
	// a reason of correctness before cost. Inside a binding's reserved namespace
	// the binding is the authority and the work axis is a probe list behind it;
	// running the plane's router there would let a shadowing rig prefix capture
	// an id the binding mints. That it also avoids a routes.jsonl disk read on
	// every molecule-id read of a relocated city is the second reason, not the
	// first.
	//
	// A router that returns no legs leaves this package's own rule in place, so
	// the field is safe to set unconditionally.
	WorkAxis WorkAxisRouter
}

// WorkAxisRouter answers "which work stores can hold this id, in what order"
// for a plane whose by-id work axis is routed rather than probed.
//
// The first leg returned is the plan's RESIDUAL — returned unprobed by
// ResolveOwner when it is also the last, which is what keeps a single-answer
// route byte-identical to the caller reading its own store. Any further legs
// are read in order, so a multi-leg answer proves absence rather than handing
// back a residual.
//
// DECLARING ONE IS A REVIEWED ACT. An implementation replaces this package's
// by-id work rule with the plane's own, so a second one is a second answer to
// the same question — the bug class the residency resolver exists to end,
// wearing the resolver's API. A plane may declare at most one axis, every
// declaration is named in scripts/residency-boundary-baseline.txt under the
// c:WorkAxisRouter pattern, and the day a SECOND plane declares one the two
// must land with a cross-plane router-agreement pin. The rule is stated in
// full beside the pattern in scripts/residency-boundary-patterns.txt.
type WorkAxisRouter interface {
	// WorkLegsForID returns the work legs for id, or nil to leave the
	// resolver's own [work, covering-shadows] rule in place.
	WorkLegsForID(id string) []Leg
}

// RoutedWork is the claimable/ready federation: `gc ready`, demand tiers.
type RoutedWork struct{}

// AssignedWorkPurpose selects between the two leg shapes the assigned-work
// intent has. They are deliberately different orders, and the difference is the
// documented property of the pair rather than a per-site choice.
type AssignedWorkPurpose int

const (
	// AssignedWorkSweep is the gate/release/re-wake shape: a union over work,
	// the rigs and EVERY binding. It is the zero value because a MISSING leg
	// in a sweep is the failure mode (a claim nothing can see), while an extra
	// leg is merely a read.
	AssignedWorkSweep AssignedWorkPurpose = iota

	// AssignedWorkClaimEscalation is the write shape: work legs first, binding
	// last, escalating only on proof.
	AssignedWorkClaimEscalation
)

// AssignedWork is an identity's claims: gates, releases, re-wake, and claim
// escalation.
type AssignedWork struct {
	// Identifiers are the assignee spellings the caller will query for. The
	// resolver does NOT read them — WHICH stores answer an assigned-work
	// question does not depend on WHOSE claims are being asked about, and a leg
	// set that varied per identity would be a residency decision made from
	// caller data, which is exactly what this package exists to stop.
	//
	// They are carried so the intent is self-describing at the call site and so
	// a future per-identity scope (a rig-pinned agent that provably cannot hold
	// a city claim) can narrow the plan HERE rather than at the consumer. Until
	// then this field is advisory and unused; do not read it as a filter.
	Identifiers []string

	// Purpose selects between the two leg shapes this intent has.
	Purpose AssignedWorkPurpose
}

// Session is the session/wait/identity resolution legs.
type Session struct{}

// Census is a reconciler-frame full sweep. An empty Classes means every class.
type Census struct{ Classes []coordclass.Class }

// Class is single-owner placement: creates and class-pure operations.
type Class struct{ C coordclass.Class }

func (ByID) intent()         {}
func (RoutedWork) intent()   {}
func (AssignedWork) intent() {}
func (Session) intent()      {}
func (Census) intent()       {}
func (Class) intent()        {}

// Plan resolves an intent over a topology.
//
// It returns an error only when the topology cannot answer the question at all:
// a standing storage refusal on an intent that must touch a binding, or a
// malformed intent. A refused topology never degrades to a work-only plan —
// that is the answer that looks like success and reads the ledger the relocated
// class was moved off.
func Plan(i Intent, t Topology) (ResolvedPlan, error) {
	if t.Refused != nil && len(t.Bindings) == 0 {
		// A real constructor attaches the refusal to the refusing binding it
		// resolved, so this shape only reaches here from a hand-built Topology.
		// Refusing it wholesale is the point: ById and Class would otherwise
		// find no binding to touch, take the "work owns it" branch, and hand a
		// refused city a work-only plan — the exact answer that looks like
		// success while reading the ledger the relocated class was moved off.
		return ResolvedPlan{}, fmt.Errorf("storeref: topology carries a standing storage refusal but no binding to attach it to; a refused city's classes must be routed at the refusing store: %w", t.Refused)
	}
	plan, err := planFor(i, t)
	if err != nil {
		return ResolvedPlan{}, err
	}
	if len(plan.Legs) == 0 {
		// A legless plan is not an empty answer; it is a topology with nothing
		// to read (a nil work store). Returning it would let a Union report
		// zero rows as a complete answer, which is the silent-shrink shape the
		// whole contract exists to close.
		return ResolvedPlan{}, fmt.Errorf("storeref: %T resolved to no readable leg — the topology has no work store", i)
	}
	return plan, nil
}

func planFor(i Intent, t Topology) (ResolvedPlan, error) {
	switch in := i.(type) {
	case ByID:
		return planByID(in, t)
	case RoutedWork:
		return planRoutedWork(t)
	case AssignedWork:
		return planAssignedWork(in, t)
	case Session:
		return planSession(t)
	case Census:
		return planCensus(in, t)
	case Class:
		return planClass(in, t)
	default:
		return ResolvedPlan{}, fmt.Errorf("storeref: unknown residency intent %T", i)
	}
}

// planByID builds the by-id probe list.
func planByID(in ByID, t Topology) (ResolvedPlan, error) {
	id := strings.TrimSpace(in.ID)
	if id == "" {
		return ResolvedPlan{}, errors.New("storeref: ByID requires a bead id")
	}
	plan := ResolvedPlan{Mode: ModeFirstOwner, ID: id}

	// The reserved namespaces are disjoint, so an id inside ONE binding's
	// namespace is never resident in another: that binding alone leads. An id
	// inside NONE of them is a migrate-preserved relic candidate, and every
	// binding that has not retired its probe must be asked.
	bindings := t.orderedBindings()
	owner := -1
	for i, b := range bindings {
		if b.coversID(id) {
			owner = i
			break
		}
	}
	switch {
	case owner >= 0:
		plan.Legs = append(plan.Legs, PlanLeg{Leg: bindings[owner].Leg, Role: RoleAuthority, OnError: PolicyFatal})
	default:
		for _, b := range bindings {
			if b.probeRetired() {
				continue
			}
			plan.Legs = append(plan.Legs, PlanLeg{Leg: b.Leg, Role: RoleResidenceProbe, OnError: PolicyRefusalTolerated})
		}
		// Outside every reserved namespace a plane with its own by-id router
		// owns the work axis, and its answer REPLACES the probed tail below
		// rather than joining it. Consulted only here: inside a namespace the
		// binding is the authority and the tail is this package's probe list.
		if routed := routedWorkLegs(in, id); len(routed) > 0 {
			plan.Legs = append(plan.Legs, routed...)
			return dedupeLegs(plan), nil
		}
	}

	plan.Legs = append(plan.Legs, PlanLeg{Leg: t.Work, Role: RoleWorkFallback, OnError: PolicyFatal})
	for _, shadow := range shadowLegsCovering(id, t.orderedRigs()) {
		plan.Legs = append(plan.Legs, PlanLeg{Leg: shadow, Role: RoleShadow, OnError: PolicyFatal})
	}
	return dedupeLegs(plan), nil
}

// planRoutedWork federates the claimable set: work, the rigs, then the binding
// LAST.
func planRoutedWork(t Topology) (ResolvedPlan, error) { return planWorkFederation(t) }

// planAssignedWork answers both halves of the intent pair.
func planAssignedWork(in AssignedWork, t Topology) (ResolvedPlan, error) {
	if in.Purpose != AssignedWorkClaimEscalation {
		return planWorkFederation(t)
	}
	if err := t.refusalFor(true); err != nil {
		return ResolvedPlan{}, err
	}
	// Claim escalation reverses the federation's order — work legs FIRST,
	// binding last — and every work leg is Fatal: a rig read error is not
	// proof of not-found, and escalation may only fire on proof.
	plan := ResolvedPlan{Mode: ModeFirstOwner}
	plan.Legs = append(plan.Legs, PlanLeg{Leg: t.Work, Role: RoleAuthority, OnError: PolicyFatal})
	for _, rig := range t.orderedRigs() {
		plan.Legs = append(plan.Legs, PlanLeg{Leg: rig, Role: RoleAuthority, OnError: PolicyFatal})
	}
	plan.Legs = append(plan.Legs, bindingTailLegs(t.orderedBindings())...)
	return dedupeLegs(plan), nil
}

// planWorkFederation is the leg set BOTH the demand surface (RoutedWork) and
// the assigned-work sweep read: work, the rigs ascending, then every binding.
//
// One function rather than two identical ones on purpose. "The surface that
// counts work and the surface that sweeps its claims read the same stores" is
// the reader-agreement property, and sharing the builder makes it true by
// construction instead of by two lists that have to be kept in step.
func planWorkFederation(t Topology) (ResolvedPlan, error) {
	if err := t.refusalFor(true); err != nil {
		return ResolvedPlan{}, err
	}
	plan := ResolvedPlan{Mode: ModeUnion}
	plan.Legs = append(plan.Legs, PlanLeg{Leg: t.Work, Role: RoleAuthority, OnError: PolicyFatal})
	plan.Legs = append(plan.Legs, rigTailLegs(t)...)
	plan.Legs = append(plan.Legs, bindingTailLegs(t.orderedBindings())...)
	return dedupeLegs(plan), nil
}

// planSession leads with the sessions binding, which is the AUTHORITY, and
// keeps work and the rigs behind it: substituting the binding for them is the
// session-verb blindness.
func planSession(t Topology) (ResolvedPlan, error) {
	if err := t.refusalFor(true); err != nil {
		return ResolvedPlan{}, err
	}
	plan := ResolvedPlan{Mode: ModeUnion}
	workRole := RoleAuthority
	if b, ok := t.BindingFor(coordclass.ClassSessions); ok {
		plan.Legs = append(plan.Legs, PlanLeg{Leg: b.Leg, Role: RoleAuthority, OnError: PolicyFatal})
		workRole = RoleFederationTail
	}
	plan.Legs = append(plan.Legs, PlanLeg{Leg: t.Work, Role: workRole, OnError: PolicyFatal})
	plan.Legs = append(plan.Legs, rigTailLegs(t)...)
	return dedupeLegs(plan), nil
}

// planCensus is the full sweep, and the city WORK store is a DISTINCT leg
// beside the binding. A binding that merely REPLACED the work leg is how a
// city-scope routed work bead became invisible to the controller tick.
func planCensus(in Census, t Topology) (ResolvedPlan, error) {
	classes := in.Classes
	if len(classes) == 0 {
		classes = coordclass.Classes()
	}
	bindings := bindingsServing(t, classes)
	if err := t.refusalFor(len(bindings) > 0); err != nil {
		return ResolvedPlan{}, err
	}
	plan := ResolvedPlan{Mode: ModeUnion}
	plan.Legs = append(plan.Legs, PlanLeg{Leg: t.Work, Role: RoleAuthority, OnError: PolicyFatal})
	plan.Legs = append(plan.Legs, rigTailLegs(t)...)
	plan.Legs = append(plan.Legs, bindingTailLegs(bindings)...)
	return dedupeLegs(plan), nil
}

// planClass is the placement contract, unchanged: the binding serving the
// class, else the work store.
func planClass(in Class, t Topology) (ResolvedPlan, error) {
	if b, ok := t.BindingFor(in.C); ok {
		if err := t.refusalFor(true); err != nil {
			return ResolvedPlan{}, err
		}
		return dedupeLegs(ResolvedPlan{Mode: ModeSingleOwner, Legs: []PlanLeg{{Leg: b.Leg, Role: RoleAuthority, OnError: PolicyFatal}}}), nil
	}
	return dedupeLegs(ResolvedPlan{Mode: ModeSingleOwner, Legs: []PlanLeg{{Leg: t.Work, Role: RoleAuthority, OnError: PolicyFatal}}}), nil
}

// refusalFor returns the standing refusal when the intent would read a binding
// on a city this build must not serve. Work is untouched by a refusal: a
// refused city still serves its work beads from its work ledger.
func (t Topology) refusalFor(touchesBinding bool) error {
	if t.Refused != nil && touchesBinding {
		return t.Refused
	}
	return nil
}

func bindingsServing(t Topology, classes []coordclass.Class) []ClassBinding {
	want := make(map[coordclass.Class]bool, len(classes))
	for _, c := range classes {
		want[c] = true
	}
	var out []ClassBinding
	for _, b := range t.orderedBindings() {
		for _, have := range b.Classes {
			if want[have] {
				out = append(out, b)
				break
			}
		}
	}
	return out
}

func rigTailLegs(t Topology) []PlanLeg {
	rigs := t.orderedRigs()
	out := make([]PlanLeg, 0, len(rigs))
	for _, rig := range rigs {
		// A rig going dark is one scope reporting a hole, and the result says
		// so honestly rather than presenting a smaller answer as authoritative.
		out = append(out, PlanLeg{Leg: rig, Role: RoleFederationTail, OnError: PolicyPartialDegrade})
	}
	return out
}

func bindingTailLegs(bindings []ClassBinding) []PlanLeg {
	out := make([]PlanLeg, 0, len(bindings))
	for _, b := range bindings {
		// The binding is FATAL: the execution DAG going dark is not a partial
		// degradation, and a work-only answer is indistinguishable from "the
		// DAG finished".
		out = append(out, PlanLeg{Leg: b.Leg, Role: RoleFederationTail, OnError: PolicyFatal})
	}
	return out
}

// routedWorkLegs asks the intent's router for the work axis of an id no binding
// namespace claims, and gives its answer the roles the executor reads.
//
// The FIRST leg is the residual, so a route that named exactly one store is
// handed back unprobed and the caller's own read produces the caller's own
// error message. The rest are Shadow legs — other work stores that can hold the
// id, read behind it — so a multi-leg answer that misses everywhere is proven
// absence. Every leg is Fatal: a work store that cannot be read has told the
// resolution nothing, and reporting that as a missing bead is the shape this
// package exists to prevent.
func routedWorkLegs(in ByID, id string) []PlanLeg {
	if in.WorkAxis == nil {
		return nil
	}
	legs := in.WorkAxis.WorkLegsForID(id)
	out := make([]PlanLeg, 0, len(legs))
	for i, leg := range legs {
		role := RoleShadow
		if i == 0 {
			role = RoleWorkFallback
		}
		out = append(out, PlanLeg{Leg: leg, Role: role, OnError: PolicyFatal})
	}
	return out
}

// shadowLegsCovering returns the work legs whose CONFIGURED prefix also covers
// id, most specific (longest prefix) first so the narrowest declared owner is
// probed before a broader one.
func shadowLegsCovering(id string, legs []Leg) []Leg {
	matched := make([]Leg, 0, len(legs))
	for _, leg := range legs {
		if leg.Store == nil || !IDInNamespace(id, strings.TrimSpace(leg.Prefix)) {
			continue
		}
		matched = append(matched, leg)
	}
	sort.SliceStable(matched, func(i, j int) bool {
		return len(strings.TrimSpace(matched[i].Prefix)) > len(strings.TrimSpace(matched[j].Prefix))
	})
	return matched
}

// dedupeLegs drops nil stores and repeats of a store already in the plan. A
// city whose rig store IS its city store, or whose binding resolved back to the
// work store, must not read the same store twice — the first entry keeps its
// role, because that is the reason the leg is there.
func dedupeLegs(p ResolvedPlan) ResolvedPlan {
	out := p.Legs[:0]
	var seen storeSet
	for _, l := range p.Legs {
		if !seen.addIfNew(l.Leg.Store) {
			continue
		}
		out = append(out, l)
	}
	p.Legs = out
	return p
}

// storeSet tracks store identity for a dedupe pass without assuming a store can
// be a map key.
//
// beads.Store is an INTERFACE, and an implementation whose dynamic type is a
// struct carrying a slice or a map is not comparable: putting one in a map
// panics with "hash of unhashable type", and so does ==. Plans are built over
// whatever stores the caller opened, so the resolver cannot make that
// assumption about them.
//
// An uncomparable store is therefore treated as distinct from everything. That
// is the safe direction of the two: a duplicated leg is read twice, while a
// wrongly-dropped one is a leg silently missing from a federated answer, which
// is the whole failure class this package exists to close.
//
// Identity is ==, so two DIFFERENT backends behind a value-typed store that
// happens to compare equal — a zero-size store double, say — read as one leg.
// Every production store is reference-identified (a pointer, or a wrapper
// carrying one), which is what makes == the right rule here.
type storeSet struct{ seen []beads.Store }

// addIfNew reports whether store is a leg worth keeping — non-nil, and not one
// already added.
func (s *storeSet) addIfNew(store beads.Store) bool {
	if store == nil {
		return false
	}
	if !comparableStore(store) {
		return true
	}
	for _, have := range s.seen {
		if comparableStore(have) && have == store {
			return false
		}
	}
	s.seen = append(s.seen, store)
	return true
}

func comparableStore(store beads.Store) bool {
	t := reflect.TypeOf(store)
	return t != nil && t.Comparable()
}

// ResolveOwner probes a FirstOwner plan and pins the store that holds id.
//
// A read error is NEVER absence: any error a leg's policy does not tolerate
// aborts with the store NAMED, because reading "the binding could not answer"
// as "the bead is not there" is the root-loss shape this whole lane exists to
// prevent.
//
// # A miss has TWO shapes, and a consumer must handle both
//
//   - The plan ENDS in the work residual (the common by-id case). The last leg
//     is returned UNPROBED: (work store, its ref, nil). This is the identity
//     fast-path — on a single-store city nothing is read here at all, so the
//     caller's own read produces its own error message byte-identically, which
//     is what keeps such a city unchanged. A consumer gets a store back and
//     discovers the miss from its OWN Get.
//   - The plan ends in a SHADOW leg — an id covered by a rig's configured
//     prefix, or a further store a routed work axis named — and every leg
//     cleanly missed. There is no residual to hand back, so this returns
//     (nil, "", beads.ErrNotFound).
//
// So `err == nil` does not mean "the bead exists", and ErrNotFound is reachable
// only for the second shape. A consumer that treats a nil store as impossible,
// or that skips its own read because ResolveOwner "succeeded", is wrong on one
// of the two.
//
// A consumer that is about to read the bead anyway should call ResolveOwnerRow,
// which hands back the row a winning probe already read instead of making it
// pay for the same read twice.
func ResolveOwner(p ResolvedPlan, id string) (beads.Store, StoreRef, error) {
	owner, err := ResolveOwnerRow(p, id)
	return owner.Store, owner.Ref, err
}

// Owner is a by-id resolution: the store that holds the bead, and the row
// itself when a probe already read it.
type Owner struct {
	// Store is the store to read and write the bead through.
	Store beads.Store

	// Ref names that store for a diagnostic or a persisted census row.
	Ref StoreRef

	// Bead is the row a probed leg returned. It is meaningful only when Read
	// is true.
	Bead beads.Bead

	// Read reports whether Bead holds the row. It is FALSE for the work
	// residual, which is handed back unprobed: the caller must perform its own
	// read there, and letting the caller's own read produce the caller's own
	// error message is what keeps a single-store city byte-identical.
	Read bool
}

// ResolveOwnerRow is ResolveOwner, plus the row the winning probe already read.
//
// Every by-id surface reads the bead immediately after resolving it, and a
// probe that answered has already paid for that read. Discarding it would
// double the reads of every by-id operation against a relocated city's
// binding — the hot path, and exactly the funnel cost the identity fast-path
// exists to avoid.
func ResolveOwnerRow(p ResolvedPlan, id string) (Owner, error) {
	if p.Mode != ModeFirstOwner {
		return Owner{}, fmt.Errorf("storeref: ResolveOwner needs a %s plan, got %s", ModeFirstOwner, p.Mode)
	}
	id = strings.TrimSpace(id)
	if p.ID != "" && p.ID != id {
		return Owner{}, fmt.Errorf("storeref: plan was resolved for %q, executed for %q — a by-id plan is id-specific", p.ID, id)
	}
	if len(p.Legs) == 0 {
		return Owner{}, errors.New("storeref: plan has no legs")
	}
	for i, leg := range p.Legs {
		if leg.Role == RoleWorkFallback && i == len(p.Legs)-1 {
			return Owner{Store: leg.Leg.Store, Ref: leg.Leg.Ref}, nil
		}
		b, err := leg.Leg.Store.Get(id)
		switch {
		case err == nil:
			return Owner{Store: leg.Leg.Store, Ref: leg.Leg.Ref, Bead: b, Read: true}, nil
		case errors.Is(err, beads.ErrNotFound):
			continue
		case leg.OnError == PolicyRefusalTolerated && IsStandingRefusal(err):
			// A refused city still serves WORK, and this leg was only ever a
			// residence probe for an id no relocated class could own.
			continue
		default:
			return Owner{Ref: leg.Leg.Ref}, fmt.Errorf("reading %q from %s: %w", id, legName(leg.Leg.Ref), err)
		}
	}
	return Owner{}, beads.ErrNotFound
}

// ResolvePlacement is the ModeSingleOwner executor: the one store a
// class-pure operation — above all a CREATE — acts on.
//
// It performs no I/O, because placement is not a search. A by-id plan asks
// "where does this bead live" and probes until something answers; a placement
// plan asks "where does this class BELONG", and the topology already knows.
// That difference is why the modes need separate executors rather than one
// leg-walking loop: running a FirstOwner plan through this function would
// return the leg that merely LEADS a probe order and place the bead there, and
// running a placement plan through ResolveOwner would probe the owner and
// report a not-yet-created bead's absence as a miss.
//
// A refused city never reaches here for a relocated class — Plan returns the
// standing refusal as an error, which is what keeps an infrastructure-class
// create off the work ledger the class was moved away from.
func ResolvePlacement(p ResolvedPlan) (Leg, error) {
	if p.Mode != ModeSingleOwner {
		return Leg{}, fmt.Errorf("storeref: ResolvePlacement needs a %s plan, got %s", ModeSingleOwner, p.Mode)
	}
	if len(p.Legs) != 1 {
		return Leg{}, fmt.Errorf("storeref: a %s plan names exactly one store, got %d (%s)", ModeSingleOwner, len(p.Legs), p)
	}
	owner := p.Legs[0].Leg
	if owner.Store == nil {
		return Leg{}, fmt.Errorf("storeref: %s plan leg %s has no store", ModeSingleOwner, legName(owner.Ref))
	}
	return owner, nil
}

// LegError names a leg whose read failed and was tolerated as a partial
// degradation.
type LegError struct {
	Ref StoreRef
	Err error
}

func (e LegError) Error() string { return legName(e.Ref) + ": " + e.Err.Error() }
func (e LegError) Unwrap() error { return e.Err }

// UnionResult is a federated answer plus the honest statement of what it is
// missing. Emptiness is a RESULT; failure is an EVENT, and Partial is how the
// two stay distinguishable at the consumer.
type UnionResult[T any] struct {
	Items     []T
	Partial   bool
	LegErrors []LegError
}

// Union reads every leg of a Union plan and merges the rows, first-leg-wins on
// the id fn returns (pass nil to skip dedupe).
//
// A leg whose policy is Fatal fails the whole union with the store named — the
// no-silent-downgrade rule. A leg whose policy is PartialDegrade is recorded,
// the result is MARKED Partial, and the remaining legs still answer.
//
// On a fatal failure the degraded legs collected BEFORE it still come back with
// the error. Without them "the graph plane is down" and "every store in the
// city is down" are the same message, and the caller loses a diagnosis it had
// already paid for (internal/api's graphPlaneUnavailable carries exactly these).
func Union[T any](p ResolvedPlan, id func(T) string, fn func(Leg) ([]T, error)) (UnionResult[T], error) {
	var out UnionResult[T]
	seen := make(map[string]bool)
	walked, err := Walk(p, func(leg Leg) (bool, error) {
		items, ferr := fn(leg)
		if ferr != nil {
			return false, ferr
		}
		for _, item := range items {
			if id != nil {
				if key := id(item); key != "" {
					if seen[key] {
						continue
					}
					seen[key] = true
				}
			}
			out.Items = append(out.Items, item)
		}
		return false, nil
	})
	out.Partial = walked.Partial
	out.LegErrors = walked.LegErrors
	if err != nil {
		out.Items = nil
		out.Partial = true
		return out, err
	}
	return out, nil
}

// EachLeg visits every leg of a plan in order, with the reason it is there and
// the meaning of its failure. It performs NO I/O and cannot fail.
//
// It is the enumeration seam for a consumer that genuinely cannot run its reads
// inside Walk: one that fans the legs out CONCURRENTLY (the controller's census
// arms, which read every leg in parallel and fold per-arm partials), or one that
// resolves the leg set once and reads it repeatedly under different queries (the
// `gc ready` reader, whose three arms share one leg set).
//
// Those consumers still must not hand-roll a store list, because the two things
// a hand-rolled list loses are the two things that were wrong at 31 call sites:
// the leg ORDER and the per-leg error POLICY. EachLeg hands over both, so what a
// consumer supplies is only its own fan-out shape. Walk remains the executor for
// reads — the place a policy is APPLIED — and a consumer that can use it should.
func EachLeg(p ResolvedPlan, visit func(leg Leg, role Role, onError ErrPolicy)) {
	for _, l := range p.Legs {
		visit(l.Leg, l.Role, l.OnError)
	}
}

// WalkResult is what a leg-by-leg pass over a Union plan reports beyond the
// caller's own side effects: whether it stopped early and where, and the honest
// statement of which legs went dark.
type WalkResult struct {
	// Stopped reports that visit asked to stop, and StoppedAt names the leg it
	// stopped on. A pass that read every leg leaves both zero.
	Stopped   bool
	StoppedAt StoreRef

	// Visited counts the legs visit was called for, degraded legs included.
	Visited int

	// Partial marks a pass that could not read every leg it planned to. A
	// consumer answering an EXISTENCE question must treat it as "cannot prove
	// absence" and retain rather than reap — a partial pass reported as a clean
	// "this session holds nothing" is the drain-a-live-holder shape.
	Partial   bool
	LegErrors []LegError
}

// Walk visits every leg of a Union plan in order, honoring per-leg ErrPolicy,
// and stops early when visit returns stop.
//
// It is the executor for the union questions whose answer is not a merged row
// set: an existence probe that must stop at the first leg that answers, a
// release sweep that mutates as it goes, a pass bounded by a time budget. Those
// are the sites that used to build a []beads.Store and read it themselves — and
// reading it themselves is how the leg order, the dedupe rule and the fail-loud
// policy came to be restated once per site.
//
// Union is implemented on top of this, so the two executors cannot disagree
// about what a leg failure means.
func Walk(p ResolvedPlan, visit func(Leg) (stop bool, err error)) (WalkResult, error) {
	var out WalkResult
	if p.Mode != ModeUnion {
		return out, fmt.Errorf("storeref: Walk needs a %s plan, got %s", ModeUnion, p.Mode)
	}
	for _, leg := range p.Legs {
		out.Visited++
		stop, err := visit(leg.Leg)
		if err != nil {
			if leg.OnError == PolicyRefusalTolerated && IsStandingRefusal(err) {
				// A refused city still serves WORK, and a leg carrying this
				// policy was only ever consulted in case the binding held the
				// answer. The city is refused, not degraded.
				continue
			}
			if leg.OnError != PolicyPartialDegrade {
				out.Partial = true
				return out, fmt.Errorf("reading %s: %w", legName(leg.Leg.Ref), err)
			}
			out.Partial = true
			out.LegErrors = append(out.LegErrors, LegError{Ref: leg.Leg.Ref, Err: err})
			continue
		}
		if stop {
			out.Stopped = true
			out.StoppedAt = leg.Leg.Ref
			return out, nil
		}
	}
	return out, nil
}

// legName renders a ref for an error message, where an empty string would read
// as a missing word.
func legName(ref StoreRef) string {
	if ref == WorkRef {
		return "the city work store"
	}
	return string(ref)
}
