package beads

import "reflect"

// This file is the capability side of the typed class handles (typed-stores
// design §3.3, phase T1): one way to reach an optional capability through a
// handle, one way to ask whether two handles are one physical store, and the
// seam a class guard (T8) uses to vet every capability that is not
// class-neutral. It changes no existing lookup: the XFor helpers answer
// through a handle exactly as they did (TestXForThroughAHandleToday pins
// that), and As is new.
//
// Outside internal/beads the storeclass lint (tools/nogo/analyzers/storeclass)
// reports a type assertion on a store and an unwrap of a handle; As and the
// XFor helpers are what replace them.

// classLayer is a layer the class machinery puts above the store the router
// chose: a typed class handle, and from T8 the class guard. As and SameStore
// walk class layers and nothing else; they never peel a CachingStore, an
// emitter or a policy store.
type classLayer interface {
	unwrapClassLayer() Store
}

// classGateLayer is a class layer that vets capabilities: the class guard
// (T8). For every T but a class-neutral one (passesClassGate), As asks the
// outermost gate among a store's class layers instead of looking at any
// layer, above the gate or below it: a typed handle over a gate is itself a
// Store and a GuardedBatcherHandleProvider, and would otherwise answer those
// before the gate is reached. gateCapability stores the guarded adapter for
// *T in target and reports true, or reports false and the capability is
// hidden. A class-neutral capability passes the gate as the store below it,
// so a caller that asserts that value onward to a writer launders past the
// gate, like the assertions the storeclass lint leaves out of scope: the
// guard (T8) must re-vet that path.
type classGateLayer interface {
	classLayer
	gateCapability(target any) bool
}

func (s WorkStore) unwrapClassLayer() Store    { return s.Store }
func (s GraphStore) unwrapClassLayer() Store   { return s.Store }
func (s SessionStore) unwrapClassLayer() Store { return s.Store }
func (s MailStore) unwrapClassLayer() Store    { return s.Store }
func (s OrdersStore) unwrapClassLayer() Store  { return s.Store }
func (s NudgesStore) unwrapClassLayer() Store  { return s.Store }

// Raw returns the store the handle wraps. It is the unwrap the storeclass
// lint baselines: prefer the handle, or As for a capability. A zero handle
// returns nil.
func (s WorkStore) Raw() Store { return s.Store }

// Raw returns the store the handle wraps; see WorkStore.Raw.
func (s GraphStore) Raw() Store { return s.Store }

// Raw returns the store the handle wraps; see WorkStore.Raw.
func (s SessionStore) Raw() Store { return s.Store }

// Raw returns the store the handle wraps; see WorkStore.Raw.
func (s MailStore) Raw() Store { return s.Store }

// Raw returns the store the handle wraps; see WorkStore.Raw.
func (s OrdersStore) Raw() Store { return s.Store }

// Raw returns the store the handle wraps; see WorkStore.Raw.
func (s NudgesStore) Raw() Store { return s.Store }

// classLayerMaxDepth bounds the walk, so a self-wrapping layer ends it.
const classLayerMaxDepth = 8

// As returns s's capability T, seeing through typed class handles. At each
// class layer, outermost first, it answers with the layer itself when the
// layer is a T (As[Store] keeps the handle); then it steps inside. Below the
// class layers it answers what the capability's XFor helper answers on that
// store (ConditionalWriterFor, GuardedBatcherFor, GraphApplyFor,
// MetadataCASWriterFor, AtomicConditionalCloserFor, NamespaceCensusFor,
// DepListBatchFor, GraphOnlyReadyFor, AssignmentGuardedUpdaterFor,
// ReadyBlockedByAbsentFor), or a plain assertion for any other T, including
// *CachingStore. So for every T, As[T](h) is exactly what the lookup answers
// on h's store; TestAsRoutesEveryCapabilityHelper fails when a helper lands
// without its case in resolveCapability.
//
// As[ConditionalWriter] is the capability, not the rollout policy: a write
// that obeys the conditional-writes mode still goes through
// ResolveConditionalWriter.
//
// A class guard among s's class layers answers every capability outside the
// class-neutral allowlist (passesClassGate): its adapter or nothing, even for
// a T a handle above it implements, so As[Store] on a handle over a guard is
// the guard's answer, not the handle. As is nil-safe: nil, a zero handle (or
// a handle over one), or a walk deeper than classLayerMaxDepth answers
// false, before any layer is asked, so As[Store] never hands out an absent
// handle.
func As[T any](s Store) (T, bool) {
	var zero T
	if physicalStore(s) == nil {
		return zero, false // nil, a zero handle, or a handle over one, is no store
	}
	if gate := classGate(s); gate != nil && !passesClassGate[T]() {
		var v T
		if gate.gateCapability(&v) {
			return v, true
		}
		return zero, false
	}
	for range classLayerMaxDepth {
		layer, isLayer := s.(classLayer)
		if !isLayer {
			return resolveCapability[T](s)
		}
		if v, ok := s.(T); ok {
			return v, true
		}
		s = layer.unwrapClassLayer()
	}
	return zero, false
}

// classGate is the outermost class guard among s's class layers, or nil.
func classGate(s Store) classGateLayer {
	for range classLayerMaxDepth {
		layer, ok := s.(classLayer)
		if !ok {
			return nil
		}
		if gate, ok := layer.(classGateLayer); ok {
			return gate
		}
		s = layer.unwrapClassLayer()
	}
	return nil
}

// resolveCapability is T on a store below the class layers: its XFor helper
// when it has one, else a plain assertion.
func resolveCapability[T any](s Store) (T, bool) {
	var v T
	var ok bool
	switch p := any(&v).(type) {
	case *ConditionalWriter:
		*p, ok = ConditionalWriterFor(s)
	case *GuardedBatcher:
		*p, ok = GuardedBatcherFor(s)
	case *GraphApplyStore:
		*p, ok = GraphApplyFor(s)
	case *MetadataCASWriter:
		*p, ok = MetadataCASWriterFor(s)
	case *AtomicConditionalCloser:
		*p, ok = AtomicConditionalCloserFor(s)
	case *NamespaceCensus:
		*p, ok = NamespaceCensusFor(s)
	case *DependencyBatchLister:
		*p, ok = DepListBatchFor(s)
	case *GraphOnlyReadyStore:
		*p, ok = GraphOnlyReadyFor(s)
	case *AssignmentGuardedUpdater:
		*p, ok = AssignmentGuardedUpdaterFor(s)
	case *AbsentBlockedReader:
		*p, ok = ReadyBlockedByAbsentFor(s)
	default:
		v, ok = s.(T)
	}
	return v, ok
}

// passesClassGate reports whether T is class-neutral: it can neither create
// a bead nor change a bead's class (design §3.3.4, §3.7), so As looks past a
// class guard for it. The list is exact types, the readers and the
// assignee-only writers, and it fails closed: a guard vets everything else,
// Store, every concrete store, UpdateReporter, the provider interfaces, and
// any interface not named here, even one with a listed type's or a writer's
// method set. A capability added later is vetted until it is listed.
func passesClassGate[T any]() bool {
	switch any((*T)(nil)).(type) {
	case *Counter, *NamespaceCensus, *DependencyBatchLister, *DepMetadataReader,
		*GraphOnlyReadyStore, *ContextReadyReader, *OrderedReadyReader, *CachePreferredReadyReader,
		*ChildrenBatchLister, *ExactBatchGetter, *CachedReadExact, *ReadOnlyReporter,
		*RowWitness, *ParentProjectionWaiter, *AbsentBlockedReader,
		*ConditionalAssignmentReleaser, *ConditionalAssigneeTransferer:
		return true
	}
	return false
}

// SameStore reports whether a and b are one physical store: the store below
// their class layers, so two class handles over one store are the same, and
// a handle is the same as the store it wraps. A nil or zero handle is the
// same as nothing, and a store whose dynamic type is not comparable (== on
// it would panic) is the same as nothing either.
func SameStore(a, b Store) (same bool) {
	a, b = physicalStore(a), physicalStore(b)
	if a == nil || b == nil {
		return false
	}
	ta := reflect.TypeOf(a)
	if ta != reflect.TypeOf(b) || !ta.Comparable() {
		return false
	}
	defer func() {
		if recover() != nil {
			same = false // a comparable type holding a non-comparable interface value
		}
	}()
	return a == b
}

// physicalStore is s below its class layers, or nil.
func physicalStore(s Store) Store {
	for range classLayerMaxDepth {
		layer, ok := s.(classLayer)
		if !ok {
			return s
		}
		s = layer.unwrapClassLayer()
	}
	return nil
}
