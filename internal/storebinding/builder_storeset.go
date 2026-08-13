package storebinding

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/gastownhall/gascity/internal/coordclass"
)

var (
	// ErrInvalidStoreSetBuild reports build inputs that do not exactly match the
	// frozen plan.
	ErrInvalidStoreSetBuild = errors.New("invalid store set build")
	// ErrStoreSetAlreadyBuilt reports a second successful build attempt on one
	// builder. Exactly one candidate may exist per frozen plan.
	ErrStoreSetAlreadyBuilt = errors.New("store set candidate is already built")
)

// ActiveBindingHandle is one binding activation opened with OpenModeActive under the
// authority minted from the durable active manifest.
type ActiveBindingHandle struct {
	Binding   BindingName
	Opened    OpenedBinding
	Authority DurableActiveOpenAuthority
}

// BuildInputs carries exactly the planned open participants. The builder owns
// none of them: on any failure every handle stays exactly where activation put it, and
// activation unwinds by executing the plan's close program.
type BuildInputs struct {
	Handles []ActiveBindingHandle
}

// StoreSetBuilder assembles the unpublished immutable StoreSet candidate from
// activation-supplied ACTIVE handles. It opens nothing, closes nothing, and never makes
// a live resource unreachable.
type StoreSetBuilder struct {
	plan     *StoragePlan
	mu       sync.Mutex
	consumed bool
}

// NewStoreSetBuilder freezes one builder over one frozen plan.
func NewStoreSetBuilder(plan *StoragePlan) (*StoreSetBuilder, error) {
	if plan == nil {
		return nil, fmt.Errorf("%w: no frozen plan", ErrInvalidStoreSetBuild)
	}
	if err := plan.validateExact(); err != nil {
		return nil, err
	}
	return &StoreSetBuilder{plan: plan}, nil
}

// Plan returns the frozen plan this builder assembles.
func (b *StoreSetBuilder) Plan() *StoragePlan { return b.plan }

// Build validates every activation-supplied ACTIVE handle before any consuming step and
// then composes the candidate. Consuming steps run in exactly one order: the
// deferred Orders-from-Graph composition, then the deferred Messaging bind last,
// immediately before assembly. Any failure therefore leaves the one-shot
// Messaging binder unconsumed and Build re-invokable; only a fully successful
// build consumes the builder.
func (b *StoreSetBuilder) Build(ctx context.Context, in BuildInputs) (UnpublishedStoreSet, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.consumed {
		return UnpublishedStoreSet{}, ErrStoreSetAlreadyBuilt
	}
	if err := ctx.Err(); err != nil {
		return UnpublishedStoreSet{}, err
	}
	handles, generation, err := b.validateHandles(in)
	if err != nil {
		return UnpublishedStoreSet{}, err
	}
	candidate, err := b.compose(handles, generation)
	if err != nil {
		return UnpublishedStoreSet{}, err
	}
	b.consumed = true
	return candidate, nil
}

// validateHandles proves the exact handle set, descriptor identity, capability,
// class-front, and pinned-Work facts before the builder consumes anything.
func (b *StoreSetBuilder) validateHandles(in BuildInputs) (map[BindingName]ActiveBindingHandle, Generation, error) {
	program := b.plan.openProgram
	if len(in.Handles) != len(program) {
		return nil, 0, fmt.Errorf("%w: %d handles for %d planned open participants", ErrInvalidStoreSetBuild, len(in.Handles), len(program))
	}
	handles := make(map[BindingName]ActiveBindingHandle, len(in.Handles))
	for _, handle := range in.Handles {
		if _, duplicate := handles[handle.Binding]; duplicate {
			return nil, 0, fmt.Errorf("%w: duplicate handle for binding %q", ErrInvalidStoreSetBuild, handle.Binding)
		}
		if _, planned := b.plan.openStep(handle.Binding); !planned {
			return nil, 0, fmt.Errorf("%w: unplanned handle for binding %q", ErrInvalidStoreSetBuild, handle.Binding)
		}
		handles[handle.Binding] = handle
	}
	var generation Generation
	for _, step := range program {
		handle, supplied := handles[step.Binding]
		if !supplied {
			return nil, 0, fmt.Errorf("%w: missing handle for binding %q", ErrInvalidStoreSetBuild, step.Binding)
		}
		descriptor, err := validatedActiveDescriptor(handle)
		if err != nil {
			return nil, 0, err
		}
		if generation == 0 {
			generation = handle.Authority.generation
		}
		if handle.Authority.generation != generation {
			return nil, 0, fmt.Errorf("%w: binding %q opened generation %d beside %d", ErrInvalidStoreSetBuild, step.Binding, handle.Authority.generation, generation)
		}
		if err := validateHandleClasses(step, handle, descriptor); err != nil {
			return nil, 0, err
		}
	}
	if err := b.validatePinnedWork(handles); err != nil {
		return nil, 0, err
	}
	return handles, generation, nil
}

// validatedActiveDescriptor re-checks the durable active authority against the
// descriptor the binding actually opened. binding validation checks this at OpenBinding; the
// builder is the last gate before a consumer could ever see the handle.
func validatedActiveDescriptor(handle ActiveBindingHandle) (Descriptor, error) {
	if isNilInterface(handle.Opened) {
		return Descriptor{}, fmt.Errorf("%w: binding %q supplied no opened handle", ErrInvalidStoreSetBuild, handle.Binding)
	}
	if handle.Authority.isZero() || !handle.Authority.generation.Valid() {
		return Descriptor{}, fmt.Errorf("%w: binding %q has no durable active-open authority", ErrInvalidStoreSetBuild, handle.Binding)
	}
	descriptor := handle.Opened.Descriptor()
	if err := descriptor.Validate(); err != nil {
		return Descriptor{}, fmt.Errorf("%w: binding %q: %w", ErrInvalidStoreSetBuild, handle.Binding, err)
	}
	if err := handle.Authority.validate(handle.Authority.generation, descriptor); err != nil {
		return Descriptor{}, fmt.Errorf("%w: binding %q: %w", ErrInvalidStoreSetBuild, handle.Binding, err)
	}
	return descriptor, nil
}

func validateHandleClasses(step PlannedOpen, handle ActiveBindingHandle, descriptor Descriptor) error {
	if !descriptor.Classes().Contains(step.AssignedClasses) {
		return fmt.Errorf("%w: binding %q does not serve every assigned class: %w", ErrInvalidStoreSetBuild, step.Binding, ErrUnsupportedClass)
	}
	capabilities := handle.Opened.Capabilities()
	if !capabilities.Equal(descriptor.Capabilities) {
		return fmt.Errorf("%w: binding %q capabilities differ from its descriptor", ErrInvalidStoreSetBuild, step.Binding)
	}
	if err := validateOpenClassRequirements(capabilities, step.AssignedClasses, step.Requirements); err != nil {
		return fmt.Errorf("%w: binding %q: %w", ErrInvalidStoreSetBuild, step.Binding, err)
	}
	for _, class := range step.AssignedClasses.Classes() {
		if err := validateClassFront(handle.Opened, class); err != nil {
			return fmt.Errorf("%w: binding %q: %w", ErrInvalidStoreSetBuild, step.Binding, err)
		}
	}
	return nil
}

// validateClassFront rejects an absent or typed-nil front for an assigned class.
func validateClassFront(opened OpenedBinding, class coordclass.Class) error {
	present, usable := false, false
	switch class {
	case coordclass.ClassWork:
		work, ok := opened.Work()
		present, usable = ok, ok && workTopologyUsable(work)
	case coordclass.ClassGraph:
		graph, ok := opened.Graph()
		present, usable = ok, ok && !isNilInterface(graph)
	case coordclass.ClassSessions:
		sessions, ok := opened.Sessions()
		present, usable = ok, ok && !isNilInterface(sessions)
	case coordclass.ClassMessaging:
		binder, ok := opened.Messaging()
		present, usable = ok, ok && !isNilInterface(binder)
	case coordclass.ClassOrders:
		orders, ok := opened.Orders()
		present, usable = ok, ok && !isNilInterface(orders)
	case coordclass.ClassNudges:
		nudges, ok := opened.Nudges()
		present, usable = ok, ok && nudgeFrontDoorsUsable(nudges)
	default:
		return fmt.Errorf("%w: %s", ErrUnsupportedClass, class)
	}
	if !present {
		return fmt.Errorf("class %s front is absent", class)
	}
	if !usable {
		return fmt.Errorf("class %s front is nil", class)
	}
	return nil
}

// validatePinnedWork fact-checks the opened Work topology against the frozen
// pins. Semantic facts are always compared; the pinned identity triples and
// physical grouping are compared when Work is still served by the reserved
// binding, whose pins name the bootstrap generation. The builder never adopts
// an observed topology.
func (b *StoreSetBuilder) validatePinnedWork(handles map[BindingName]ActiveBindingHandle) error {
	binding, assigned := b.plan.BindingFor(coordclass.ClassWork)
	if !assigned {
		return fmt.Errorf("%w: %w: %s", ErrInvalidStoreSetBuild, ErrMissingClassAssignment, coordclass.ClassWork)
	}
	handle, supplied := handles[binding]
	if !supplied {
		return fmt.Errorf("%w: missing handle for Work binding %q", ErrInvalidStoreSetBuild, binding)
	}
	topology, ok := handle.Opened.Work()
	if !ok {
		return fmt.Errorf("%w: Work binding %q exposes no topology", ErrInvalidStoreSetBuild, binding)
	}
	if err := b.plan.workPlan.checkTopology(topology, binding == ReservedWorkBinding); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidStoreSetBuild, err)
	}
	return nil
}

func (b *StoreSetBuilder) compose(handles map[BindingName]ActiveBindingHandle, generation Generation) (UnpublishedStoreSet, error) {
	work, err := workFront(b.plan, handles)
	if err != nil {
		return UnpublishedStoreSet{}, err
	}
	graph, err := frontFor(b.plan, handles, coordclass.ClassGraph, func(opened OpenedBinding) (GraphStore, bool) { return opened.Graph() })
	if err != nil {
		return UnpublishedStoreSet{}, err
	}
	sessions, err := frontFor(b.plan, handles, coordclass.ClassSessions, func(opened OpenedBinding) (SessionsStore, bool) { return opened.Sessions() })
	if err != nil {
		return UnpublishedStoreSet{}, err
	}
	nudges, err := frontFor(b.plan, handles, coordclass.ClassNudges, func(opened OpenedBinding) (NudgeFrontDoors, bool) { return opened.Nudges() })
	if err != nil {
		return UnpublishedStoreSet{}, err
	}
	orders, err := b.composeOrders(handles, graph)
	if err != nil {
		return UnpublishedStoreSet{}, err
	}
	messaging, err := b.composeMessaging(handles, sessions)
	if err != nil {
		return UnpublishedStoreSet{}, err
	}
	identities, err := descriptorIdentities(b.plan, handles)
	if err != nil {
		return UnpublishedStoreSet{}, err
	}
	fronts := storeSetFronts{work: work, graph: graph, sessions: sessions, messaging: messaging, orders: orders, nudges: nudges}
	candidate, err := newUnpublishedStoreSet(fronts, generation, identities)
	if err != nil {
		return UnpublishedStoreSet{}, fmt.Errorf("%w: %w", ErrInvalidStoreSetBuild, err)
	}
	return candidate, nil
}

// composeOrders executes the planned Orders-from-Graph deferred bind. When
// Orders and Graph share a binding the provider already returned a bound store
// and no step is planned, so nothing is composed.
func (b *StoreSetBuilder) composeOrders(handles map[BindingName]ActiveBindingHandle, graph GraphStore) (OrdersStore, error) {
	orders, err := frontFor(b.plan, handles, coordclass.ClassOrders, func(opened OpenedBinding) (OrdersStore, bool) { return opened.Orders() })
	if err != nil {
		return nil, err
	}
	if !b.plan.hasDeferredBind(DeferredBindOrdersGraph) {
		return orders, nil
	}
	binder, ok := orders.(OrdersGraphBinder)
	if !ok || isNilInterface(binder) {
		return nil, fmt.Errorf("%w: Orders binding cannot accept the selected Graph store", ErrInvalidStoreSetBuild)
	}
	bound := binder.BindGraph(graph)
	if isNilInterface(bound) {
		return nil, fmt.Errorf("%w: Orders binding returned no bound store", ErrInvalidStoreSetBuild)
	}
	return bound, nil
}

// composeMessaging executes the Messaging-from-Sessions deferred bind with the
// selected Sessions directory. It runs last, so any earlier failure leaves the
// one-shot binder unconsumed.
func (b *StoreSetBuilder) composeMessaging(handles map[BindingName]ActiveBindingHandle, sessions SessionsStore) (MessagingFrontDoors, error) {
	binder, err := frontFor(b.plan, handles, coordclass.ClassMessaging, func(opened OpenedBinding) (MessagingFrontDoorBinder, bool) {
		return opened.Messaging()
	})
	if err != nil {
		return MessagingFrontDoors{}, err
	}
	if !b.plan.hasDeferredBind(DeferredBindMessagingSessions) {
		return MessagingFrontDoors{}, fmt.Errorf("%w: no planned Messaging bind", ErrInvalidStoreSetBuild)
	}
	fronts, err := binder.BindSessions(sessions)
	if err != nil {
		return MessagingFrontDoors{}, fmt.Errorf("%w: %w", ErrInvalidStoreSetBuild, err)
	}
	return fronts, nil
}

func workFront(plan *StoragePlan, handles map[BindingName]ActiveBindingHandle) (WorkTopology, error) {
	return frontFor(plan, handles, coordclass.ClassWork, func(opened OpenedBinding) (WorkTopology, bool) { return opened.Work() })
}

// frontFor selects the typed front of the binding the plan assigned to a class.
// Selection is a plan lookup, never a provider route.
func frontFor[T any](plan *StoragePlan, handles map[BindingName]ActiveBindingHandle, class coordclass.Class, front func(OpenedBinding) (T, bool)) (T, error) {
	var zero T
	binding, assigned := plan.BindingFor(class)
	if !assigned {
		return zero, fmt.Errorf("%w: %w: %s", ErrInvalidStoreSetBuild, ErrMissingClassAssignment, class)
	}
	handle, supplied := handles[binding]
	if !supplied {
		return zero, fmt.Errorf("%w: missing handle for binding %q", ErrInvalidStoreSetBuild, binding)
	}
	value, ok := front(handle.Opened)
	if !ok {
		return zero, fmt.Errorf("%w: binding %q exposes no %s front", ErrInvalidStoreSetBuild, binding, class)
	}
	return value, nil
}

func descriptorIdentities(plan *StoragePlan, handles map[BindingName]ActiveBindingHandle) (map[BindingName]BindingIdentity, error) {
	identities := make(map[BindingName]BindingIdentity, len(plan.openProgram))
	for _, step := range plan.openProgram {
		identity, err := handles[step.Binding].Opened.Descriptor().Identity()
		if err != nil {
			return nil, fmt.Errorf("%w: binding %q: %w", ErrInvalidStoreSetBuild, step.Binding, err)
		}
		identities[step.Binding] = identity
	}
	return identities, nil
}

func (p *StoragePlan) hasDeferredBind(kind DeferredBindKind) bool {
	for _, deferred := range p.deferred {
		if deferred.Kind == kind {
			return true
		}
	}
	return false
}
