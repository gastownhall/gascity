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

	// MoleculeRoots enumerates molecule roots for the wisp GC: the graph
	// placement at Scope (Authority, Fatal), then the leg each of Work names,
	// in Work's order (RoleV1, PartialDegrade), where v1 poured roots and the
	// attachment holders live. The work legs are the caller's census, by ref,
	// not the epoch's: the GC reads the legs every seat-work question reads
	// (cmd/gc's WorkLegs), and only when the routes relocate graph, so with
	// no refs it is the graph placement alone (tickWispGC,
	// wisp_gc_work_lane.go). A ref names a leg whatever its suspension: the
	// census decides which rigs serve.
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

	// Work is MoleculeRoots' work legs, by ref (WorkRef, a rig ref, a binding
	// ref), in order. A ref this epoch holds no store for is a plan error.
	Work []StoreRef
}

// ConvoyKind is which convoy federation a Convoy intent plans. Each kind is
// one hand-rolled federation cmd/gc's convoy autoclose and wisp autoclose
// read today, with its legs, order and gate roles pinned here (t7g-design.md
// §4).
type ConvoyKind int

const (
	// ConvoyParent finds a row's legacy parent (a work convoy): Near, the
	// row's own store, then the work home ID's prefix names (the open rig
	// whose configured prefix is the longest that ID carries, suspended or
	// not, else the city work store). The home is planned only when Near is
	// no work store of the epoch (a work store's rows keep their parent
	// beside them) and ID is in no reserved class namespace (a binding-minted
	// parent has no work-ledger home). First owner; only a not-found moves the
	// walk on; both legs federated (cmd/gc doConvoyAutocloseWith).
	ConvoyParent ConvoyKind = iota

	// ConvoyTracking lists the tracking convoys of an item: Near, the item's
	// own store, read strict (the work class's own read), then the graph
	// placement at Scope, federated, for a convoy minted single-store-era on
	// the graph side. A union; any leg's failure fails the whole read, which
	// the reader takes as "decline" (cmd/gc collectInputConvoyWorkflowRoots).
	ConvoyTracking

	// ConvoyChildren lists a convoy's legacy children: Near, the convoy's own
	// store, then Elsewhere, the store its children may also sit in, when it
	// is another store. Near is strict alone and federated when paired; a
	// union walked lazily (the reader stops at the first open child), every
	// leg Fatal (cmd/gc closeCompletedConvoyAcross).
	ConvoyChildren
)

// String renders the kind.
func (k ConvoyKind) String() string {
	switch k {
	case ConvoyParent:
		return "Parent"
	case ConvoyTracking:
		return "Tracking"
	case ConvoyChildren:
		return "Children"
	default:
		return "ConvoyKind(?)"
	}
}

// Convoy is a convoy's federation: where its parent, its tracking convoys or
// its children may live. Near is the caller's store value as it holds it
// today (the closed row's, the item's, the convoy's); the shape is the kind's.
type Convoy struct {
	Kind ConvoyKind

	// Scope is the scope whose graph placement ConvoyTracking reads.
	Scope StoreRef

	// ID is the parent ConvoyParent looks for; its plan is id-specific.
	ID string

	Near    beads.Store
	NearRef StoreRef

	// Elsewhere is ConvoyChildren's second store, nil for none.
	Elsewhere beads.Store
}

// CloseOwner is the store a close event's row is confirmed in (R4): the
// event's route, the refs of the stores its id routes to (the prefix route's
// one store, or the unconfigured fallback's every rig then the city), then
// Relic, the binding the residence probe proved holds the id, unless the
// route already holds it. First owner, every leg Fatal and federated: the
// reader (ResolveOwnerPreferring) takes the first row that reads closed, and
// any failed read before one leaves the close unconfirmed. An empty route
// with a relic is the relic alone (the event's prefix names a rig this epoch
// did not open); neither is a plan error. Route refs name a
// rig whatever its suspension (the event routed there); a ref this epoch
// holds no store for is a plan error.
type CloseOwner struct {
	ID    string
	Route []StoreRef
	Relic StoreRef
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

func (Convoy) intent()     {}
func (CloseOwner) intent() {}

// WorkKind is which work federation a WorkFederation intent plans. Each kind
// is the leg list one family of API readers federates over today, its order
// pinned here (t7b3-design.md §2).
type WorkKind int

const (
	// WorkList is the work ledger as GET /v0/beads lists it and /status
	// counts it: the city and every rig the epoch opened, in name order, the
	// city listed under CityName (a rig of that name replaces it, as
	// State.BeadStores' alias does), then the graph placement
	// (RoleFederationTail) when it is relocated by handle identity.
	WorkList WorkKind = iota

	// WorkSweep is every store that can hold work assigned to one session (the
	// API's retired-session hand-off). On a city whose graph is relocated by
	// handle identity: the city, the rigs ascending, the graph placement, then
	// the sessions placement. Otherwise the sessions placement alone: the one
	// store the sweep always read there (OD-T7e-3 keeps this leg set; no mail,
	// orders or nudges binding holds assigned work).
	WorkSweep

	// WorkScope is Scope's work store alone: a rig-scoped GET /v0/beads, the
	// city ledger store_health measures, status's city ready leg.
	WorkScope
)

// String renders the kind.
func (k WorkKind) String() string {
	switch k {
	case WorkList:
		return "List"
	case WorkSweep:
		return "Sweep"
	case WorkScope:
		return "Scope"
	default:
		return "WorkKind(?)"
	}
}

// WorkFederation is the work ledger's federation for the API's list, count
// and sweep readers. The city and rig legs are RoleAuthority (each is its
// scope's work authority) and the graph and sessions legs RoleFederationTail,
// so a reader tells the tail by its role. Every leg is PolicyFatal and
// federated: each leg holds only its own rows by design, and whether a leg's
// failure is tolerated is each reader's rule, kept in its visitor. No leg is
// ever a relic leg: these readers ask for work, which a scope store
// legitimately holds.
type WorkFederation struct {
	Kind WorkKind

	// Scope is WorkScope's scope: WorkRef or a rig ref.
	Scope StoreRef

	// CityName is the name WorkList lists the city under, among the rigs.
	CityName string

	// SuspendedRigs is the reader's choice about suspended rigs, for WorkList
	// and WorkSweep. It has no default: the zero value is a plan error.
	SuspendedRigs WorkSuspendedRigs

	// Skip is the rigs WorkSkipsRigs leaves unread, by ref: the caller's own
	// census of suspended rigs (the API reads effective suspension from disk
	// per request; the epoch's Leg.Suspended is the controller's per-tick
	// publication, and the two can disagree). A skipped rig is dropped before
	// the legs are deduped, so a store it shares with a rig that is read is
	// still read through that rig. A ref the epoch did not open is ignored;
	// WorkRef is a plan error, since the city is never skipped.
	Skip []StoreRef
}

// WorkSuspendedRigs is a WorkFederation reader's choice about suspended rigs.
type WorkSuspendedRigs int

const (
	// WorkSuspendedRigsUnset is no choice; Stores.Plan refuses it.
	WorkSuspendedRigsUnset WorkSuspendedRigs = iota
	// WorkReadsSuspendedRigs reads every rig the epoch opened.
	WorkReadsSuspendedRigs
	// WorkSkipsRigs leaves the rigs WorkFederation.Skip names unread.
	WorkSkipsRigs
)

func (WorkFederation) intent() {}
