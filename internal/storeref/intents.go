package storeref

// The federation intents T7 adds (typed-stores T7b, plan §4.3). Each names a
// hand-rolled federation the tree reads today, with its leg order and per-leg
// error policy pinned here rather than restated at the site. They are planned
// over a city's minted handles (Stores.Plan, OD-T7-5): whether a class is
// relocated is decided by handle identity, so the split-strict gate's
// projection, which relocates nothing but hands the class handles a view of
// their own, keeps the split shape its fixtures exercise.

import "github.com/gastownhall/gascity/internal/beads"

// MoleculeKind is which molecule federation a Molecule intent plans. The
// kinds are named rather than parameterized (OD-T7-4): each pins the leg order
// and policy of the sites it serves.
type MoleculeKind int

const (
	// MoleculeRootNear finds a molecule root: the graph placement at Scope
	// first (Authority), then Near (RoleNear). First owner; only a not-found
	// moves the walk on (B2's root lookup, cmd/gc/molecule_autoclose.go).
	MoleculeRootNear MoleculeKind = iota

	// MoleculeParentNear finds a legacy parent: Near first (RoleNear), then
	// the graph placement at Scope. First owner; only a not-found moves the
	// walk on (B2's parent lookup).
	MoleculeParentNear

	// MoleculeAttachment finds a bead's attached molecule: the graph placement
	// at Scope (Authority), then Near, the parent bead's own store (RoleV1, a
	// v1 poured molecule lives beside its parent). First owner under
	// PolicyFatalUnlessResolved, so a graph leg that refuses does not hide a
	// v1 molecule Near holds (R2, internal/sling/sling_attachment.go). The
	// attachment's children are attributed to the Near leg, as getFromStores'
	// callers attribute them to the last store.
	MoleculeAttachment

	// MoleculeRoots enumerates molecule roots for the wisp GC. On a city that
	// relocates the graph class (by handle identity) it is the graph placement
	// (Authority, Fatal), then every serving work leg, the city's and each
	// rig's ascending (RoleV1, PartialDegrade), where v1 poured roots and the
	// attachment holders live. On a city that relocates nothing it is the
	// graph placement alone: the GC reads no work leg there
	// (cmd/gc/city_runtime.go tickWispGC, wisp_gc_work_lane.go).
	MoleculeRoots

	// MoleculeActive is the step reminder's molecule readers for one agent:
	// the graph placement at Scope (Authority), then Near, the agent's own
	// work store (RoleV1), only when Near is not the graph placement's store.
	// A union, Fatal on any leg (cmd/gc/wisp_step_inject.go activeMoleculeIn).
	MoleculeActive
)

// String renders the kind.
func (k MoleculeKind) String() string {
	switch k {
	case MoleculeRootNear:
		return "RootNear"
	case MoleculeParentNear:
		return "ParentNear"
	case MoleculeAttachment:
		return "Attachment"
	case MoleculeRoots:
		return "Roots"
	case MoleculeActive:
		return "Active"
	default:
		return "MoleculeKind(?)"
	}
}

// Molecule is a molecule's federation: where its root, parent, attachment or
// roots may live.
type Molecule struct {
	Kind MoleculeKind

	// Scope is the scope whose graph placement the plan reads: WorkRef or a
	// RigRef.
	Scope StoreRef

	// Near is the store the plan reads beside the graph placement, and NearRef
	// its name: the closed bead's own store (RootNear, ParentNear), the parent
	// bead's own store (Attachment), the agent's own work store (Active). A
	// Near that is the graph placement's store is one leg, not two; a nil
	// Near is no leg. Roots reads no Near.
	Near    beads.Store
	NearRef StoreRef
}

// OrderRuns is an order's run evidence, as the order dispatcher's open-work
// gate, cooldown clock and event cursor read it (cmd/gc/order_dispatch.go
// gateStoresFor), in its order, because HasOpenWork stops at the first leg
// that holds open work or fails. Every leg is Fatal; LastRun keeps the rows
// it has when a later leg fails, in its visitor.
//
//   - Where the graph or orders placement at Scope is not one of the work legs
//     (a split city): the graph placement, then the orders placement, then
//     the scope's work store only when PoursWork. The work ledger is
//     otherwise not read.
//   - Else (a single-store city): the scope's work store, then the city work
//     store when LegacyCity.
type OrderRuns struct {
	Scope StoreRef

	// PoursWork reports whether the order's formula pours a v1 work-class
	// molecule, whose root and run label land in the scope's work store. It
	// is asked only on a split city, as gateStoresFor asks it, so a
	// single-store city never compiles a formula to plan its gate; nil means
	// no.
	PoursWork func() bool

	// LegacyCity reports that a rig order's tracking may still sit in the
	// city work store (the dispatcher's legacy city target).
	LegacyCity bool
}

// Workflow is the workflow-root federation: the relocated graph binding
// (Authority), then the city work store, then each serving rig ascending.
// On a relocated city the scope legs are RoleRelic, read for roots written
// there before the split; the city's is never retired (OD-T7-11), and a rig
// proven relic-free (Leg.RelicFree) is dropped. On a single-store city every
// leg is RoleFederationTail.
//
// RoleAuthority marks exactly one leg: the relocated graph binding, the store
// that structurally holds every live root. A single-store city has none.
//
// Every leg is PolicyFatal: whether a leg's failure is tolerated is each
// reader's own rule, kept in its visitor. The sling's source guard refuses on
// the Authority leg and on the source store (by ref), and tolerates the rest
// only with a warning sink; dispatch's live-root scan, the API's run cancel
// and `gc workflow delete` fail on any.
type Workflow struct {
	// IncludeRetiredRigs keeps a relic-free rig, as a federated tail, for a
	// reader that may be looking for rig WORK (an event subject), which such a
	// rig still holds.
	IncludeRetiredRigs bool

	// SuspendedRigs is the reader's choice about the rigs the holder reports
	// suspended (Leg.Suspended). It has no default: the zero value is a plan
	// error, because every reader converted so far reads them today and a
	// silent skip would drop their roots, while a suspended rig's store is
	// routinely dark and a read can wake its retired proxy.
	SuspendedRigs WorkflowSuspendedRigs
}

// WorkflowSuspendedRigs is a Workflow reader's choice about suspended rigs.
type WorkflowSuspendedRigs int

const (
	// WorkflowSuspendedRigsUnset is no choice; Stores.Plan refuses it.
	WorkflowSuspendedRigsUnset WorkflowSuspendedRigs = iota
	// WorkflowReadsSuspendedRigs reads a suspended rig as any other.
	WorkflowReadsSuspendedRigs
	// WorkflowSkipsSuspendedRigs leaves a suspended rig unread.
	WorkflowSkipsSuspendedRigs
)
