package beads

import (
	"runtime"
	"strings"
)

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
	// NoLegRole is a read through no leg layer: a store, a class handle, or a
	// ResidentStore minted over the store itself. The gate polices it.
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
// writer; see WorkStore.Handles.
func (l *federatedLeg) Handles() StoreHandles { return forwardHandles(l, l.Store) }

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
// writer; see WorkStore.Handles.
func (l *relicLeg) Handles() StoreHandles { return forwardHandles(l, l.Store) }

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

// FederatedReader is one read-only leg of a federation: the reads a planned
// multi-store read or residence probe makes of one store, through a leg layer
// that declares the leg's role. It is not a Store, so a leg cannot be written
// or handed to a write path; ResidentStore is the writable handle. Reach a
// read capability with ReaderAs. Only the residency layer mints one
// (MintFederatedLeg, MintRelicLeg; beadstest in tests). Like a zero class
// handle, a zero reader's reads panic: IsNilStore reports it.
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

// ReaderAs is As for a leg: r's capability T, for a class-neutral T only (the
// readers and assignee-only writers passesClassGate lists), so a read-only
// leg never hands out a writer or its store. A capability read runs below the
// leg layer, so it carries no role on the stack: the gate polices it.
func ReaderAs[T any](r FederatedReader) (T, bool) {
	var zero T
	if r.leg == nil || !passesClassGate[T]() {
		return zero, false
	}
	return As[T](r.leg)
}

// storeReader is a Store or a FederatedReader: what the read helpers
// (SameStore, LiveFor, PhysicalStoreKey, IsNilStore, GetMany,
// DepListBatchFor, ReadyBlockedByAbsentFor) take, so a leg passes to them as
// its store does.
type storeReader interface {
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
	_ storeReader = Store(nil)
	_ storeReader = FederatedReader{}
)

// legReadFrames are the frames a leg layer's bead reads leave, by role.
var legReadFrames = func() map[string]LegRole {
	const pkg = "github.com/gastownhall/gascity/internal/beads."
	frames := map[string]LegRole{}
	for _, read := range []string{"Get", "List", "ListOpen", "Ready", "Children", "ListByLabel", "ListByAssignee", "ListByMetadata", "DepList"} {
		frames[pkg+"(*federatedLeg)."+read] = FederatedLegRole
		frames[pkg+"(*relicLeg)."+read] = RelicLegRole
	}
	return frames
}()

// LegRoleOnStack reports the role of the innermost leg-layer read pcs (from
// runtime.Callers) pass through, or NoLegRole. It sits beside
// ReadMarkerOnStack: the split-strict gate honors both until the markers go
// (T7j). A frame is matched by its function with method-value ("-fm") and
// closure decorations dropped, so a leg read taken as a method value, or a
// closure inside one, still counts.
//
// A role is visible only on the goroutine that made the read: a leg read must
// run on the calling goroutine, and a read a store hands to another goroutine
// carries no role.
func LegRoleOnStack(pcs []uintptr) LegRole {
	if len(pcs) == 0 {
		return NoLegRole
	}
	frames := runtime.CallersFrames(pcs)
	for {
		frame, more := frames.Next()
		if role, ok := legReadFrames[undecoratedFunc(frame.Function)]; ok {
			return role
		}
		if !more {
			return NoLegRole
		}
	}
}

// undecoratedFunc drops a method value's "-fm" suffix and the compiler's
// closure segments ("f.func1", "f.func2.3", "f.gowrap1", "f.deferwrap1") from
// a frame's function name.
func undecoratedFunc(name string) string {
	name = strings.TrimSuffix(name, "-fm")
	for {
		dot := strings.LastIndexByte(name, '.')
		if dot < 0 || !isClosureSegment(name[dot+1:]) {
			return name
		}
		name = name[:dot]
	}
}

// isClosureSegment reports a closure segment of a function name: funcN,
// gowrapN, deferwrapN, or a bare number (a closure nested in one).
func isClosureSegment(seg string) bool {
	for _, prefix := range []string{"func", "gowrap", "deferwrap"} {
		if rest, ok := strings.CutPrefix(seg, prefix); ok {
			seg = rest
			break
		}
	}
	if seg == "" {
		return false
	}
	for _, c := range seg {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
