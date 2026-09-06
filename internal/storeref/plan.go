package storeref

// The plan: which stores answer this question, in what order, and what a
// per-leg failure means.
//
// A ResolvedPlan is DATA. It performs no I/O and it prints as the row format
// the conformance corpus pins. That is what lets a migration slice assert "the
// store list this site builds today == the resolver's plan" before it swaps in
// the executor, and what makes a leg-order change a diff in a golden table
// rather than a paragraph in a review.
//
// It is NOT comparable — it carries a leg slice, so `==` on two plans does not
// compile. Compare them the way the corpus does, through String(); comparing
// their leg SETS is refSet's job in the tests, and it exists precisely because
// the two comparisons have to fail differently.

import (
	"strconv"
	"strings"
)

// Mode selects the executor a plan is run through. It is a property of the
// INTENT, not of the caller: "one bead lives in one store" and "this question
// spans every store" are different questions, and running one through the
// other's executor is how a federated answer silently becomes a single-store
// one.
type Mode int

const (
	// ModeFirstOwner probes legs in order and pins the first store that
	// answers. One bead lives in exactly one store.
	ModeFirstOwner Mode = iota
	// ModeUnion reads every leg and merges, first-leg-wins on id.
	ModeUnion
	// ModeSingleOwner names exactly one store: the placement contract's
	// answer, with nothing to probe.
	ModeSingleOwner
)

// String renders the mode as it appears in a corpus row.
func (m Mode) String() string {
	switch m {
	case ModeFirstOwner:
		return "FirstOwner"
	case ModeUnion:
		return "Union(first-leg-wins)"
	case ModeSingleOwner:
		return "SingleOwner"
	default:
		return "Mode(" + strconv.Itoa(int(m)) + ")"
	}
}

// Role says WHY a leg is in the plan. It carries no policy of its own — that is
// ErrPolicy — but it is what a consumer reads to decide whether a leg is one it
// was already reading.
type Role int

const (
	// RoleAuthority is the leg that owns this question's answer: the binding
	// for an id inside its reserved namespace and for the session verbs, the
	// work ledger for routed/assigned/census questions.
	RoleAuthority Role = iota

	// RoleWorkFallback is the work ledger as the RESIDUAL answer of a by-id
	// plan — or, on a plane with a routed work axis (ByID.WorkAxis), the store
	// that plane's router pinned. When it is the last leg it is returned
	// UNPROBED: it is the caller's own store, and letting the caller's own read
	// produce its own error message is what keeps a single-store city
	// byte-identical.
	RoleWorkFallback

	// RoleShadow is another work store that can hold the id, read BEHIND the
	// residual: one whose CONFIGURED prefix also covers it — a reserved class
	// prefix is only advisory on work stores, so a work store can legitimately
	// hold an id inside a relocated class's namespace — or one a routed work
	// axis listed behind the store it pinned.
	//
	// A consumer whose pre-resolver list did not include the rig stores must
	// not gain them silently: it either resolves over a rig-less Topology or
	// drops Shadow legs, and its migration slice pins the choice.
	RoleShadow

	// RoleResidenceProbe is a binding asked about an id OUTSIDE its reserved
	// namespace. `gc storage migrate` preserves ids, so a relocated bead keeps
	// its work-era prefix and the namespace rule cannot decide — nil from the
	// namespace gate means "cannot decide", never "work owns it" (ga-axin6).
	RoleResidenceProbe

	// RoleFederationTail is an additional leg read for completeness: the rig
	// legs of a union, and the binding placed LAST on the federation intents so
	// a co-resident id answers from work and agrees with claim.
	RoleFederationTail
)

// String renders the role as it appears in a corpus row.
func (r Role) String() string {
	switch r {
	case RoleAuthority:
		return "Authority"
	case RoleWorkFallback:
		return "WorkFallback"
	case RoleShadow:
		return "Shadow"
	case RoleResidenceProbe:
		return "ResidenceProbe"
	case RoleFederationTail:
		return "FederationTail"
	default:
		return "Role(" + strconv.Itoa(int(r)) + ")"
	}
}

// ErrPolicy is what a failure of THIS leg means. The resolver decides it per
// (intent, leg role); a caller never does, which is the point — every per-site
// re-derivation of the rule was a chance to get one clause wrong.
type ErrPolicy int

const (
	// PolicyFatal means a read error is never absence. The leg's failure fails the
	// whole plan, with the store named.
	PolicyFatal ErrPolicy = iota

	// PolicyPartialDegrade means the leg's failure is recorded, the result is
	// MARKED Partial, and the remaining legs still answer. A consumer must
	// retain rather than reap on a partial answer.
	PolicyPartialDegrade

	// PolicyRefusalTolerated means a STANDING STORAGE REFUSAL from this leg is not
	// a fault of this read and the leg is skipped; any other error is fatal.
	// The single place this applies is a residence probe on an id no relocated
	// class could own — a refused city still serves WORK from its work ledger,
	// so the refusal establishes nothing about that id.
	PolicyRefusalTolerated
)

// String renders the policy as it appears in a corpus row.
func (p ErrPolicy) String() string {
	switch p {
	case PolicyFatal:
		return "Fatal"
	case PolicyPartialDegrade:
		return "PartialDegrade"
	case PolicyRefusalTolerated:
		return "RefusalTolerated"
	default:
		return "ErrPolicy(" + strconv.Itoa(int(p)) + ")"
	}
}

// PlanLeg is one store to read, with the reason it is here and the meaning of
// its failure.
type PlanLeg struct {
	Leg     Leg
	Role    Role
	OnError ErrPolicy
}

// String renders one leg of a corpus row: `<ref>[<Role>,<Policy>]`, with the
// work ref rendered as `""` so an empty ref is visible rather than blank.
func (l PlanLeg) String() string {
	ref := string(l.Leg.Ref)
	if ref == "" {
		ref = `""`
	}
	return ref + "[" + l.Role.String() + "," + l.OnError.String() + "]"
}

// ResolvedPlan is the ordered leg list one intent resolves to over one
// topology.
type ResolvedPlan struct {
	Mode Mode
	Legs []PlanLeg

	// ID is the bead id a ByID plan was built for, and empty for every other
	// intent. The executor refuses to run a ByID plan against a different id:
	// the leg list is id-SPECIFIC (namespace gate, shadows, probe retirement),
	// so reusing one is a wrong-store read waiting to happen.
	ID string
}

// String renders the plan as a conformance-corpus row.
func (p ResolvedPlan) String() string {
	if len(p.Legs) == 0 {
		return p.Mode.String() + ": <no legs>"
	}
	parts := make([]string, 0, len(p.Legs))
	for _, l := range p.Legs {
		parts = append(parts, l.String())
	}
	return p.Mode.String() + ": " + strings.Join(parts, " > ")
}

// TouchesBinding reports whether any leg of this plan is a relocated class
// binding.
//
// It is the one bit a consumer that cannot execute a plan can still act on: the
// generated work_query's legs are bd SUBPROCESSES in a workspace, and a binding
// is not a bd workspace, so "does the answer live anywhere a bd workspace cannot
// reach" decides which reader the query is built around. Answering it from the
// plan rather than by re-asking the routes is what keeps a generated query and
// the reader it drives from disagreeing.
func (p ResolvedPlan) TouchesBinding() bool {
	for _, l := range p.Legs {
		if IsClassRef(string(l.Leg.Ref)) {
			return true
		}
	}
	return false
}

// Refs returns the plan's leg refs in order — the census/wake vocabulary a
// consumer records alongside a claim.
func (p ResolvedPlan) Refs() []StoreRef {
	out := make([]StoreRef, 0, len(p.Legs))
	for _, l := range p.Legs {
		out = append(out, l.Leg.Ref)
	}
	return out
}
