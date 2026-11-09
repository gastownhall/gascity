package beads

import "runtime"

// This file is the typed federation leg (typed-stores design §3.6, T7a): a
// read that may miss because its row may live in another store runs through a
// leg layer whose type names its role, rather than inside a marker at the call
// site (FederatedRead, RelicRead). The split-strict gate reads the role off
// the stack (LegRoleOnStack), so the role travels with the leg wherever the
// residency layer hands it.
//
// A leg layer is a class layer: As, SameStore, LiveFor, PhysicalStoreKey,
// IsNilStore and the batch helpers see through it, and it forwards the
// conditional-writes resolution target and the handle providers as the class
// handles do. Its bead reads are its own methods, never inlined, so their
// frames stay on the stack; its writes delegate to the store beneath and carry
// no role. The two layers are distinct non-generic types on purpose: generic
// stenciling would give both one compiled body, and so one frame name.

// LegRole is the role a read's leg declares to the split-strict gate.
type LegRole int

const (
	// NoLegRole is a read through no leg layer: a store, a class handle, a
	// ResidentStore minted over the store itself, or a strict leg
	// (MintStrictLeg). The gate polices it.
	NoLegRole LegRole = iota
	// FederatedLegRole is a read through a federated leg: one leg of a
	// planned multi-store read, or a by-id residence probe. It may miss; the
	// gate filters it and charges nothing, as under FederatedRead.
	FederatedLegRole
	// RelicLegRole is a read through a relic leg: a scope store read for rows
	// of a class that belongs in a binding, kept for rows written there
	// before the split. The gate filters it and counts it as debt, as under
	// RelicRead.
	RelicLegRole
)

// String names the role.
func (r LegRole) String() string {
	switch r {
	case FederatedLegRole:
		return "federated"
	case RelicLegRole:
		return "relic"
	default:
		return "none"
	}
}

// legLayer is a federated or relic leg: a store whose bead reads carry the
// leg's role on the stack.
type legLayer interface {
	Store
	legRef() string
	legRole() LegRole
}

// federatedLeg is the federated leg layer over a store.
type federatedLeg struct {
	Store
	ref string
}

// relicLeg is the relic leg layer over a store.
type relicLeg struct {
	Store
	ref string
}

var (
	_ legLayer = (*federatedLeg)(nil)
	_ legLayer = (*relicLeg)(nil)

	_ ConditionalWritesResolveTargeter      = (*federatedLeg)(nil)
	_ ConditionalWritesResolveTargeter      = (*relicLeg)(nil)
	_ GuardedBatcherHandleProvider          = (*federatedLeg)(nil)
	_ GuardedBatcherHandleProvider          = (*relicLeg)(nil)
	_ interface{ Handles() StoreHandles }   = (*federatedLeg)(nil)
	_ interface{ Handles() StoreHandles }   = (*relicLeg)(nil)
	_ interface{ unwrapClassLayer() Store } = (*federatedLeg)(nil)
	_ interface{ unwrapClassLayer() Store } = (*relicLeg)(nil)
)

func (l *federatedLeg) legRef() string          { return l.ref }
func (l *federatedLeg) legRole() LegRole        { return FederatedLegRole }
func (l *federatedLeg) unwrapClassLayer() Store { return l.Store }

// ConditionalWritesResolveTarget declares the store beneath the leg as the
// conditional-writes resolution target, so a fenced write through a leg
// resolves the store's mode rather than unset.
func (l *federatedLeg) ConditionalWritesResolveTarget() Store { return l.Store }

// GuardedBatcherHandle forwards guarded-batch discovery to the store beneath.
func (l *federatedLeg) GuardedBatcherHandle() (GuardedBatcher, bool) {
	return GuardedBatcherFor(l.Store)
}

// Handles forwards HandlesFor to the store beneath, with the leg as the
// writer (see WorkStore.Handles) and the readers carrying the leg's role
// (leg_reads.go).
func (l *federatedLeg) Handles() StoreHandles {
	return legHandles(FederatedLegRole, forwardHandles(l, l.Store))
}

// Get reads through the federated leg.
//
//go:noinline
func (l *federatedLeg) Get(id string) (Bead, error) { return l.Store.Get(id) }

// List reads through the federated leg.
//
//go:noinline
func (l *federatedLeg) List(query ListQuery) ([]Bead, error) { return l.Store.List(query) }

// ListOpen reads through the federated leg.
//
//go:noinline
func (l *federatedLeg) ListOpen(status ...string) ([]Bead, error) {
	return l.Store.ListOpen(status...)
}

// Ready reads through the federated leg.
//
//go:noinline
func (l *federatedLeg) Ready(query ...ReadyQuery) ([]Bead, error) { return l.Store.Ready(query...) }

// Children reads through the federated leg.
//
//go:noinline
func (l *federatedLeg) Children(parentID string, opts ...QueryOpt) ([]Bead, error) {
	return l.Store.Children(parentID, opts...)
}

// ListByLabel reads through the federated leg.
//
//go:noinline
func (l *federatedLeg) ListByLabel(label string, limit int, opts ...QueryOpt) ([]Bead, error) {
	return l.Store.ListByLabel(label, limit, opts...)
}

// ListByAssignee reads through the federated leg.
//
//go:noinline
func (l *federatedLeg) ListByAssignee(assignee, status string, limit int) ([]Bead, error) {
	return l.Store.ListByAssignee(assignee, status, limit)
}

// ListByMetadata reads through the federated leg.
//
//go:noinline
func (l *federatedLeg) ListByMetadata(filters map[string]string, limit int, opts ...QueryOpt) ([]Bead, error) {
	return l.Store.ListByMetadata(filters, limit, opts...)
}

// DepList reads through the federated leg.
//
//go:noinline
func (l *federatedLeg) DepList(id, direction string) ([]Dep, error) {
	return l.Store.DepList(id, direction)
}

func (l *relicLeg) legRef() string          { return l.ref }
func (l *relicLeg) legRole() LegRole        { return RelicLegRole }
func (l *relicLeg) unwrapClassLayer() Store { return l.Store }

// ConditionalWritesResolveTarget declares the store beneath the leg as the
// conditional-writes resolution target; see federatedLeg's.
func (l *relicLeg) ConditionalWritesResolveTarget() Store { return l.Store }

// GuardedBatcherHandle forwards guarded-batch discovery to the store beneath.
func (l *relicLeg) GuardedBatcherHandle() (GuardedBatcher, bool) {
	return GuardedBatcherFor(l.Store)
}

// Handles forwards HandlesFor to the store beneath, with the leg as the
// writer and the readers carrying the leg's role; see federatedLeg's.
func (l *relicLeg) Handles() StoreHandles {
	return legHandles(RelicLegRole, forwardHandles(l, l.Store))
}

// Get reads through the relic leg.
//
//go:noinline
func (l *relicLeg) Get(id string) (Bead, error) { return l.Store.Get(id) }

// List reads through the relic leg.
//
//go:noinline
func (l *relicLeg) List(query ListQuery) ([]Bead, error) { return l.Store.List(query) }

// ListOpen reads through the relic leg.
//
//go:noinline
func (l *relicLeg) ListOpen(status ...string) ([]Bead, error) { return l.Store.ListOpen(status...) }

// Ready reads through the relic leg.
//
//go:noinline
func (l *relicLeg) Ready(query ...ReadyQuery) ([]Bead, error) { return l.Store.Ready(query...) }

// Children reads through the relic leg.
//
//go:noinline
func (l *relicLeg) Children(parentID string, opts ...QueryOpt) ([]Bead, error) {
	return l.Store.Children(parentID, opts...)
}

// ListByLabel reads through the relic leg.
//
//go:noinline
func (l *relicLeg) ListByLabel(label string, limit int, opts ...QueryOpt) ([]Bead, error) {
	return l.Store.ListByLabel(label, limit, opts...)
}

// ListByAssignee reads through the relic leg.
//
//go:noinline
func (l *relicLeg) ListByAssignee(assignee, status string, limit int) ([]Bead, error) {
	return l.Store.ListByAssignee(assignee, status, limit)
}

// ListByMetadata reads through the relic leg.
//
//go:noinline
func (l *relicLeg) ListByMetadata(filters map[string]string, limit int, opts ...QueryOpt) ([]Bead, error) {
	return l.Store.ListByMetadata(filters, limit, opts...)
}

// DepList reads through the relic leg.
//
//go:noinline
func (l *relicLeg) DepList(id, direction string) ([]Dep, error) {
	return l.Store.DepList(id, direction)
}

// strictLeg is a leg layer that declares no role: one leg of a planned read
// whose reads the gate polices as it does an unmarked read (the union legs no
// marker covered before T7, OD-T7-1). Its reads are the store's own.
type strictLeg struct {
	Store
	ref string
}

var (
	_ legLayer                         = (*strictLeg)(nil)
	_ ConditionalWritesResolveTargeter = (*strictLeg)(nil)
	_ GuardedBatcherHandleProvider     = (*strictLeg)(nil)
)

func (l *strictLeg) legRef() string          { return l.ref }
func (l *strictLeg) legRole() LegRole        { return NoLegRole }
func (l *strictLeg) unwrapClassLayer() Store { return l.Store }

// ConditionalWritesResolveTarget declares the store beneath the leg as the
// conditional-writes resolution target; see federatedLeg's.
func (l *strictLeg) ConditionalWritesResolveTarget() Store { return l.Store }

// GuardedBatcherHandle forwards guarded-batch discovery to the store beneath.
func (l *strictLeg) GuardedBatcherHandle() (GuardedBatcher, bool) {
	return GuardedBatcherFor(l.Store)
}

// Handles forwards HandlesFor to the store beneath, with the leg as the
// writer; see WorkStore.Handles.
func (l *strictLeg) Handles() StoreHandles { return forwardHandles(l, l.Store) }

// FederatedReader is one read-only leg of a federation: the reads a planned
// multi-store read or residence probe makes of one store, through a leg layer
// that declares the leg's role (or, for a strict leg, none). It is not a
// Store, so a leg cannot be written or handed to a write path; ResidentStore
// is the writable handle. Reach a read capability with ReaderAs. Only the
// residency layer mints one (MintFederatedLeg, MintRelicLeg, MintStrictLeg;
// beadstest in tests). Like a zero class handle, a zero reader's reads panic:
// IsNilStore reports it.
type FederatedReader struct {
	leg legLayer
}

// MintFederatedLeg is a federated leg over s, named ref (the residency
// layer's store-ref vocabulary). It is a mint: the storeclass lint reports any
// caller but internal/storeref, and beadstest.FederatedLeg in tests.
func MintFederatedLeg(s Store, ref string) FederatedReader {
	return FederatedReader{leg: &federatedLeg{Store: s, ref: ref}}
}

// MintRelicLeg is a relic leg over s, named ref; a mint, as MintFederatedLeg.
func MintRelicLeg(s Store, ref string) FederatedReader {
	return FederatedReader{leg: &relicLeg{Store: s, ref: ref}}
}

// MintStrictLeg is a leg over s, named ref, that declares no role: the gate
// polices its reads as it does a read through the store (OD-T7-1: a union leg
// no marker covered stays strict). A mint, as MintFederatedLeg.
func MintStrictLeg(s Store, ref string) FederatedReader {
	return FederatedReader{leg: &strictLeg{Store: s, ref: ref}}
}

// Ref names the store the leg reads, or "" for a zero reader.
func (r FederatedReader) Ref() string {
	if r.leg == nil {
		return ""
	}
	return r.leg.legRef()
}

// Role is the leg's role, or NoLegRole for a zero reader.
func (r FederatedReader) Role() LegRole {
	if r.leg == nil {
		return NoLegRole
	}
	return r.leg.legRole()
}

func (r FederatedReader) unwrapClassLayer() Store {
	if r.leg == nil {
		return nil
	}
	return r.leg
}

// Get reads id through the leg.
func (r FederatedReader) Get(id string) (Bead, error) { return r.leg.Get(id) }

// List reads through the leg.
func (r FederatedReader) List(query ListQuery) ([]Bead, error) { return r.leg.List(query) }

// Ready reads through the leg.
func (r FederatedReader) Ready(query ...ReadyQuery) ([]Bead, error) { return r.leg.Ready(query...) }

// Children reads through the leg.
func (r FederatedReader) Children(parentID string, opts ...QueryOpt) ([]Bead, error) {
	return r.leg.Children(parentID, opts...)
}

// ListByLabel reads through the leg.
func (r FederatedReader) ListByLabel(label string, limit int, opts ...QueryOpt) ([]Bead, error) {
	return r.leg.ListByLabel(label, limit, opts...)
}

// ListByAssignee reads through the leg.
func (r FederatedReader) ListByAssignee(assignee, status string, limit int) ([]Bead, error) {
	return r.leg.ListByAssignee(assignee, status, limit)
}

// ListByMetadata reads through the leg.
func (r FederatedReader) ListByMetadata(filters map[string]string, limit int, opts ...QueryOpt) ([]Bead, error) {
	return r.leg.ListByMetadata(filters, limit, opts...)
}

// DepList reads through the leg.
func (r FederatedReader) DepList(id, direction string) ([]Dep, error) {
	return r.leg.DepList(id, direction)
}

// ReaderAs is As for a leg: r's capability T, for a read capability only
// (passesReaderGate), so a read-only leg never hands out a writer or its
// store. A capability read runs below the leg layer, so it carries the leg's
// role only through an adapter (leg_reads.go: CachedLister and Counter); any
// other capability's reads the gate polices.
func ReaderAs[T any](r FederatedReader) (T, bool) {
	var zero T
	if r.leg == nil || !passesReaderGate[T]() {
		return zero, false
	}
	return As[T](r.leg)
}

// passesReaderGate reports whether a leg reader may hand out T: a
// class-neutral capability (passesClassGate) that writes nothing. The class
// gate lets the assignee-only writers through, because they cannot change a
// bead's class; a read-only leg refuses them, because they write.
func passesReaderGate[T any]() bool {
	switch any((*T)(nil)).(type) {
	case *ConditionalAssignmentReleaser, *ConditionalAssigneeTransferer:
		return false
	}
	return passesClassGate[T]()
}

// StoreReader is the read side a Store and a FederatedReader share: what the
// read helpers (SameStore, LiveFor, PhysicalStoreKey, IsNilStore,
// HasExplicitHandles, GetMany, DepListBatchFor, ReadyBlockedByAbsentFor)
// take, so a leg passes to them as its store does. It is not a capability to
// hold: outside internal/beads, take a typed handle or a FederatedReader, and
// the storeclass lint reports a type assertion on a StoreReader as it does
// one on a store.
type StoreReader interface {
	Get(id string) (Bead, error)
	List(query ListQuery) ([]Bead, error)
	Ready(query ...ReadyQuery) ([]Bead, error)
	Children(parentID string, opts ...QueryOpt) ([]Bead, error)
	ListByLabel(label string, limit int, opts ...QueryOpt) ([]Bead, error)
	ListByAssignee(assignee, status string, limit int) ([]Bead, error)
	ListByMetadata(filters map[string]string, limit int, opts ...QueryOpt) ([]Bead, error)
	DepList(id, direction string) ([]Dep, error)
}

var (
	_ StoreReader = Store(nil)
	_ StoreReader = FederatedReader{}
)

// legReadFrames are the frames a leg layer's bead reads leave, by role.
var legReadFrames = func() map[string]LegRole {
	const pkg = "github.com/gastownhall/gascity/internal/beads."
	frames := map[string]LegRole{}
	for _, read := range []string{"Get", "List", "ListOpen", "Ready", "Children", "ListByLabel", "ListByAssignee", "ListByMetadata", "DepList"} {
		frames[pkg+"(*federatedLeg)."+read] = FederatedLegRole
		frames[pkg+"(*relicLeg)."+read] = RelicLegRole
	}
	legAdapterFrames(pkg, frames)
	return frames
}()

// LegRoleOnStack reports the role of the innermost leg-layer read pcs (from
// runtime.Callers) pass through, or NoLegRole. It sits beside
// ReadMarkerOnStack: the split-strict gate honors both until the markers go
// (T7j). A frame is matched by its exact function name. A leg read taken as
// a method value, or made from a closure, still counts with no name
// decoration stripped: the leg's own read method is never inlined, so its
// undecorated frame is on the stack whatever wrapper or closure called it
// (TestLegRoleOnStackShapes), and the leg methods make no closures of their
// own.
//
// A role is visible only on the goroutine that made the read: a leg read must
// run on the calling goroutine, and a read a store hands to another goroutine
// carries no role. GetMany's fan-out keeps it because each of its goroutines
// reads through the leg's live reader adapter (leg_reads.go), whose frame is
// on that goroutine's stack.
func LegRoleOnStack(pcs []uintptr) LegRole {
	if len(pcs) == 0 {
		return NoLegRole
	}
	frames := runtime.CallersFrames(pcs)
	for {
		frame, more := frames.Next()
		if role, ok := legReadFrames[frame.Function]; ok {
			return role
		}
		if !more {
			return NoLegRole
		}
	}
}
