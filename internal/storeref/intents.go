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
	// MoleculeRootNear finds a molecule root: the graph placement first
	// (Authority), then the store the walk started from (RoleNear). First
	// owner; only a not-found moves the walk on (B2's root lookup,
	// cmd/gc/molecule_autoclose.go).
	MoleculeRootNear MoleculeKind = iota

	// MoleculeParentNear finds a legacy parent: the store the walk started
	// from first (RoleNear), then the graph placement. First owner; only a
	// not-found moves the walk on (B2's parent lookup).
	MoleculeParentNear

	// MoleculeAttachment finds a bead's attached molecule: the graph placement
	// (Authority), then the scope's work store (RoleV1, a v1 poured molecule).
	// First owner under PolicyFatalUnlessResolved, so a graph leg that refuses
	// does not hide a v1 molecule the work leg holds (R2,
	// internal/sling/sling_attachment.go).
	MoleculeAttachment

	// MoleculeRoots enumerates molecule roots: the graph placement
	// (Authority), then every work leg, the city's and each rig's ascending
	// (RoleV1). A union; the visitor asks each role its own query (wisp GC,
	// the step reminder). A failed work leg degrades the answer; a failed
	// graph leg fails it.
	MoleculeRoots
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
	default:
		return "MoleculeKind(?)"
	}
}

// Molecule is a molecule's federation: where its root, parent, attachment or
// roots may live.
type Molecule struct {
	Kind MoleculeKind

	// Scope is the scope whose graph placement the plan reads (WorkRef or a
	// RigRef) and, for MoleculeAttachment, whose work store backs it.
	Scope StoreRef

	// Near is the store the walk started from (the closed bead's own) and
	// NearRef its name, for MoleculeRootNear and MoleculeParentNear. A Near
	// that is the graph placement's store is one leg, not two.
	Near    beads.Store
	NearRef StoreRef
}

// OrderRuns is an order's run evidence: the orders placement, the graph
// placement at Scope, then the scope's work store, where a v1 poured
// formula's root lives (RoleV1). A union; the visitor keeps the rows it has
// when a later leg fails (orders.Store.LastRun, cmd/gc's bd cursor).
type OrderRuns struct {
	Scope StoreRef
}

// WorkflowDoor is which reader a Workflow plan serves: it picks each leg's
// error policy.
type WorkflowDoor int

const (
	// WorkflowDoorRead is a workflow reader that keeps scanning past a leg it
	// could not read and reports the leg (the API's workflow, run and convoy
	// snapshots, cmd/gc's workflow scans): every leg degrades.
	WorkflowDoorRead WorkflowDoor = iota

	// WorkflowDoorSourceGuard is the source-bead singleton guard (sling, the
	// CLI and dispatch's live-root scan): the relocated graph leg is Fatal,
	// because a binding fault is an error and never "no conflict"; every scope
	// leg degrades (the tolerated non-source-store warning).
	WorkflowDoorSourceGuard
)

// Workflow is the workflow-root federation: the relocated graph binding
// (Authority), then the city work store, then each rig ascending. On a
// relocated city the scope legs are RoleRelic, read for roots written there
// before the split, and a scope proven relic-free (Leg.RelicFree) is dropped;
// on a single-store city they are Authority. IncludeRetiredRigs keeps a
// relic-free rig, for a reader that may be looking for rig WORK (an event
// subject), which such a rig still holds.
type Workflow struct {
	IncludeRetiredRigs bool
	Door               WorkflowDoor
}
