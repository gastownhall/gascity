package storebinding

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

var (
	// ErrInvalidOpenMode reports an unknown internal provider open mode.
	ErrInvalidOpenMode = errors.New("invalid storage open mode")
	// ErrInvalidOpenAuthority reports missing or mismatched mode-specific open authority.
	ErrInvalidOpenAuthority = errors.New("invalid storage open authority")
	// ErrCommitNotDecided reports activation or commit before a durable decision.
	ErrCommitNotDecided = errors.New("storage commit has not been decided")
	// ErrGuardedActivationUnavailable reports a provider unable to atomically activate a guarded generation.
	ErrGuardedActivationUnavailable = errors.New("guarded storage activation is unavailable")
	// ErrInvalidRetainedSource reports malformed opaque retained-source identity data.
	ErrInvalidRetainedSource = errors.New("invalid retained storage source")
	// ErrInvalidGuard reports malformed retained-source guard values.
	ErrInvalidGuard = errors.New("invalid retained storage guard")
	// ErrInvalidWorkParticipant reports a work participant that does not own one physical workspace.
	ErrInvalidWorkParticipant = errors.New("invalid work workspace participant")
	// ErrDuplicateWorkParticipant reports an attempt to open or migrate one physical workspace twice.
	ErrDuplicateWorkParticipant = errors.New("duplicate work workspace participant")
	// ErrCGOUnavailable reports a provider implementation that requires unavailable CGO support.
	ErrCGOUnavailable = errors.New("storage provider requires unavailable cgo support")
	// ErrCGODisabled is retained as a typed-compatible spelling for disabled CGO builds.
	ErrCGODisabled = ErrCGOUnavailable
)

// OpenMode is the closed startup protocol mode passed to Provider.Open.
type OpenMode uint8

const (
	// OpenModeActive opens the committed active generation.
	OpenModeActive OpenMode = iota + 1
	// OpenModeReadOnlySource opens a source only for migration reads.
	OpenModeReadOnlySource
	// OpenModeAdmittedMigrationDestination opens an admitted migration destination.
	OpenModeAdmittedMigrationDestination
	// OpenModeRetainedSource opens a pinned retained source without provisioning.
	OpenModeRetainedSource
)

// Valid reports whether mode belongs to the closed open-mode set.
func (m OpenMode) Valid() bool {
	return m >= OpenModeActive && m <= OpenModeRetainedSource
}

// String returns a stable diagnostic representation of an open mode.
func (m OpenMode) String() string {
	switch m {
	case OpenModeActive:
		return "active"
	case OpenModeReadOnlySource:
		return "read-only-source"
	case OpenModeAdmittedMigrationDestination:
		return "admitted-migration-destination"
	case OpenModeRetainedSource:
		return "retained-source"
	default:
		return "unknown"
	}
}

// DurableActiveOpenAuthority proves that the controller durably selected one
// exact descriptor and generation as active before opening consumer handles.
// Its fields are deliberately private so providers cannot manufacture or
// rewrite active authority at the open boundary.
type DurableActiveOpenAuthority struct {
	version            uint16
	generation         Generation
	descriptorIdentity BindingIdentity
}

// NewDurableActiveOpenAuthority mints active-open authority for a descriptor
// only after its generation has been made durable by the controller.
func NewDurableActiveOpenAuthority(generation Generation, descriptor Descriptor) (DurableActiveOpenAuthority, error) {
	if !generation.Valid() {
		return DurableActiveOpenAuthority{}, ErrInvalidOpenAuthority
	}
	if err := descriptor.Validate(); err != nil {
		return DurableActiveOpenAuthority{}, fmt.Errorf("%w: %w", ErrInvalidOpenAuthority, err)
	}
	identity, err := descriptor.Identity()
	if err != nil {
		return DurableActiveOpenAuthority{}, fmt.Errorf("%w: %w", ErrInvalidOpenAuthority, err)
	}
	authority := DurableActiveOpenAuthority{version: 1, generation: generation, descriptorIdentity: identity}
	if err := authority.validate(generation, descriptor); err != nil {
		return DurableActiveOpenAuthority{}, err
	}
	return authority, nil
}

// Clone returns a detached durable active-open authority.
func (a DurableActiveOpenAuthority) Clone() DurableActiveOpenAuthority { return a }

func (a DurableActiveOpenAuthority) isZero() bool {
	return a.version == 0 && a.generation == 0 && a.descriptorIdentity == ""
}

func (a DurableActiveOpenAuthority) validate(generation Generation, descriptor Descriptor) error {
	identity, err := descriptor.Identity()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidOpenAuthority, err)
	}
	if a.version != 1 || a.generation != generation || a.descriptorIdentity != identity {
		return ErrInvalidOpenAuthority
	}
	return nil
}

// ComponentCompatibilityRequirement pins the complete physical compatibility
// facts expected for one descriptor component.
type ComponentCompatibilityRequirement struct {
	Component     ComponentID
	Format        FormatID
	SchemaVersion string
	ABIVersion    string
}

// ClassCapabilityRequirement makes per-class transaction and claim needs
// explicit at the open boundary.
type ClassCapabilityRequirement struct {
	Class               coordclass.Class
	RequireTransactions bool
	RequireClaims       bool
}

// OpenRequest contains the complete fenced descriptor and protocol state for a binding open.
type OpenRequest struct {
	Descriptor             Descriptor
	AssignedClasses        ClassSet
	Mode                   OpenMode
	ExpectedGeneration     Generation
	PinnedWork             *WorkTopology
	ExpectedContract       ContractVersion
	ExpectedComponents     []ComponentCompatibilityRequirement
	ClassRequirements      []ClassCapabilityRequirement
	AdmissionFence         WriterFence
	DurableActiveAuthority DurableActiveOpenAuthority
}

// Clone returns a detached request value suitable for provider invocation. The
// caller-owned admission fence remains shared intentionally so its live state
// can be revalidated after the provider call.
func (r OpenRequest) Clone() OpenRequest {
	out := r
	out.Descriptor = r.Descriptor.Clone()
	out.ExpectedComponents = append([]ComponentCompatibilityRequirement(nil), r.ExpectedComponents...)
	out.ClassRequirements = append([]ClassCapabilityRequirement(nil), r.ClassRequirements...)
	out.DurableActiveAuthority = r.DurableActiveAuthority.Clone()
	if r.PinnedWork != nil {
		pinned := *r.PinnedWork
		out.PinnedWork = &pinned
	}
	return out
}

func freezeOpenRequest(request OpenRequest) (OpenRequest, error) {
	frozen := request.Clone()
	if request.PinnedWork == nil {
		return frozen, nil
	}
	pinned, err := cloneWorkTopology(*request.PinnedWork)
	if err != nil {
		return OpenRequest{}, fmt.Errorf("%w: invalid pinned Work topology", ErrInvalidWorkParticipant)
	}
	frozen.PinnedWork = &pinned
	return frozen, nil
}

// Validate checks every fact required before a provider may open a binding.
func (r OpenRequest) Validate() error {
	return r.validate(context.Background())
}

func (r OpenRequest) validate(ctx context.Context) error {
	if err := r.Descriptor.Validate(); err != nil {
		return err
	}
	if !r.ExpectedGeneration.Valid() {
		return fmt.Errorf("%w: invalid expected generation", ErrInvalidOpenMode)
	}
	if r.AssignedClasses.Empty() || !r.Descriptor.Classes().Contains(r.AssignedClasses) {
		return fmt.Errorf("%w: assigned classes", ErrUnsupportedClass)
	}
	if !r.Mode.Valid() {
		return ErrInvalidOpenMode
	}
	if r.ExpectedContract == "" || r.Descriptor.SemanticContractVersion != r.ExpectedContract {
		return fmt.Errorf("%w: expected semantic contract", ErrWrongContract)
	}
	if err := validateOpenComponentCompatibility(r.Descriptor, r.ExpectedComponents); err != nil {
		return err
	}
	if err := r.Descriptor.Capabilities.ValidateRequired(r.AssignedClasses, false, false, r.requiresAdmissionFence()); err != nil {
		return err
	}
	if err := validateOpenClassRequirements(r.Descriptor.Capabilities, r.AssignedClasses, r.ClassRequirements); err != nil {
		return err
	}
	if r.PinnedWork != nil && !r.AssignedClasses.HasWork() {
		return fmt.Errorf("%w: pinned work supplied without work class", ErrUnsupportedClass)
	}
	if r.PinnedWork != nil {
		if _, err := cloneWorkTopology(*r.PinnedWork); err != nil {
			return fmt.Errorf("%w: invalid pinned Work topology", ErrInvalidWorkParticipant)
		}
	}
	return r.validateAuthority(ctx)
}

func (r OpenRequest) requiresAdmissionFence() bool {
	return r.Mode == OpenModeReadOnlySource || r.Mode == OpenModeAdmittedMigrationDestination
}

func (r OpenRequest) validateAuthority(ctx context.Context) error {
	switch r.Mode {
	case OpenModeActive:
		if !isNilInterface(r.AdmissionFence) || r.DurableActiveAuthority.validate(r.ExpectedGeneration, r.Descriptor) != nil {
			return ErrInvalidOpenAuthority
		}
		return nil
	case OpenModeReadOnlySource:
		if !r.DurableActiveAuthority.isZero() {
			return ErrInvalidOpenAuthority
		}
		return validateWorkFence(ctx, r.AdmissionFence, r.Descriptor, r.ExpectedGeneration, FenceRoleSource)
	case OpenModeAdmittedMigrationDestination:
		if !r.DurableActiveAuthority.isZero() {
			return ErrInvalidOpenAuthority
		}
		return validateWorkFence(ctx, r.AdmissionFence, r.Descriptor, r.ExpectedGeneration, FenceRolePopulatedDestination, FenceRoleNewDestinationReservation)
	case OpenModeRetainedSource:
		if !isNilInterface(r.AdmissionFence) || !r.DurableActiveAuthority.isZero() {
			return ErrInvalidOpenAuthority
		}
		return nil
	default:
		return ErrInvalidOpenMode
	}
}

func validateOpenComponentCompatibility(descriptor Descriptor, expected []ComponentCompatibilityRequirement) error {
	if len(expected) != len(descriptor.Components) {
		return fmt.Errorf("%w: complete component compatibility is required", ErrWrongFormat)
	}
	byID := make(map[ComponentID]ComponentCompatibilityRequirement, len(expected))
	for _, requirement := range expected {
		if requirement.Component == "" {
			return fmt.Errorf("%w: missing component", ErrWrongFormat)
		}
		if _, duplicate := byID[requirement.Component]; duplicate {
			return fmt.Errorf("%w: duplicate component compatibility", ErrWrongFormat)
		}
		byID[requirement.Component] = requirement
	}
	for _, component := range descriptor.Components {
		requirement, found := byID[component.ID]
		if !found || requirement.Format != component.Format || requirement.SchemaVersion != component.SchemaVersion || requirement.ABIVersion != component.ABIVersion {
			return fmt.Errorf("%w: component %q", ErrWrongFormat, component.ID)
		}
	}
	return nil
}

func validateOpenClassRequirements(capabilities ClassCapabilities, assigned ClassSet, requirements []ClassCapabilityRequirement) error {
	if len(requirements) != len(assigned.Classes()) {
		return fmt.Errorf("%w: complete per-class requirements are required", ErrMissingCapability)
	}
	seen := make(map[coordclass.Class]struct{}, len(requirements))
	for _, requirement := range requirements {
		if !assigned.Has(requirement.Class) {
			return fmt.Errorf("%w: requirement for unassigned class %s", ErrUnsupportedClass, requirement.Class)
		}
		if _, duplicate := seen[requirement.Class]; duplicate {
			return fmt.Errorf("%w: duplicate requirement for %s", ErrMissingCapability, requirement.Class)
		}
		seen[requirement.Class] = struct{}{}
		capability := capabilities.For(requirement.Class)
		if !capability.Available {
			return fmt.Errorf("%w: %s", ErrMissingCapability, requirement.Class)
		}
		if requirement.RequireTransactions && !capability.Transactions {
			return fmt.Errorf("%w: transactions for %s", ErrMissingCapability, requirement.Class)
		}
		if requirement.RequireClaims && !capability.Claims {
			return fmt.Errorf("%w: claims for %s", ErrMissingCapability, requirement.Class)
		}
	}
	return nil
}

// HasWork reports whether the set includes Work.
func (s ClassSet) HasWork() bool { return s.work }

// OpenedBinding exposes only typed class adapters and closes distinct components once.
type OpenedBinding interface {
	Descriptor() Descriptor
	Capabilities() ClassCapabilities
	Work() (WorkTopology, bool)
	Graph() (GraphStore, bool)
	Sessions() (SessionsStore, bool)
	Messaging() (MessagingFrontDoorBinder, bool)
	Orders() (OrdersStore, bool)
	Nudges() (NudgeFrontDoors, bool)
	Close() error
}

// ComponentHandle is one provider-owned physical handle to close after consumers stop.
type ComponentHandle struct {
	Component ComponentID
	Close     func() error
}

// OpenedBindingParts supplies the typed adapters and physical close handles for NewOpenedBinding.
type OpenedBindingParts struct {
	Descriptor   Descriptor
	Capabilities ClassCapabilities
	Work         *WorkTopology
	Graph        GraphStore
	Sessions     SessionsStore
	Messaging    MessagingFrontDoorBinder
	Orders       OrdersStore
	Nudges       *NudgeFrontDoors
	Handles      []ComponentHandle
}

// NewOpenedBinding validates adapter ownership and creates a close-once binding.
func NewOpenedBinding(parts OpenedBindingParts) (OpenedBinding, error) {
	reject := func(err error) (OpenedBinding, error) {
		return nil, closeRejectedComponentHandles(parts.Handles, err)
	}
	if err := parts.Descriptor.Validate(); err != nil {
		return reject(err)
	}
	if parts.Messaging != nil && isNilInterface(parts.Messaging) {
		return reject(ErrInvalidMessagingBinding)
	}
	if err := validateOpenedBindingParts(parts); err != nil {
		return reject(err)
	}
	if len(parts.Handles) != len(parts.Descriptor.Components) {
		return reject(fmt.Errorf("opened binding has %d handles for %d descriptor components", len(parts.Handles), len(parts.Descriptor.Components)))
	}
	descriptorComponents := make(map[ComponentID]struct{}, len(parts.Descriptor.Components))
	for _, component := range parts.Descriptor.Components {
		descriptorComponents[component.ID] = struct{}{}
	}
	seen := make(map[ComponentID]struct{}, len(parts.Handles))
	handles := make([]openedComponentHandle, len(parts.Handles))
	for index, handle := range parts.Handles {
		if _, exists := descriptorComponents[handle.Component]; !exists || handle.Close == nil {
			return reject(fmt.Errorf("opened binding has invalid component handle"))
		}
		if _, exists := seen[handle.Component]; exists {
			return reject(fmt.Errorf("opened binding has duplicate component handle %q", handle.Component))
		}
		seen[handle.Component] = struct{}{}
		handles[index] = openedComponentHandle{component: handle.Component, close: handle.Close}
	}
	var work *WorkTopology
	if parts.Work != nil {
		frozen, err := cloneWorkTopology(*parts.Work)
		if err != nil {
			return reject(fmt.Errorf("%w: invalid Work topology", ErrInvalidWorkParticipant))
		}
		work = &frozen
	}
	var messaging MessagingFrontDoorBinder
	if !isNilInterface(parts.Messaging) {
		managed, err := newManagedMessagingFrontDoorBinder(parts.Messaging)
		if err != nil {
			return reject(err)
		}
		messaging = managed
	}
	var nudges *NudgeFrontDoors
	if parts.Nudges != nil {
		cloned := *parts.Nudges
		nudges = &cloned
	}
	return &openedBinding{
		descriptor:   parts.Descriptor.Clone(),
		capabilities: parts.Capabilities,
		work:         work,
		graph:        parts.Graph,
		sessions:     parts.Sessions,
		messaging:    messaging,
		orders:       parts.Orders,
		nudges:       nudges,
		handles:      handles,
	}, nil
}

// closeRejectedComponentHandles unwinds each independently supplied physical
// handle in reverse acquisition order. Duplicate component IDs are invalid
// input, but each entry remains independently owned until its Close callback
// has been attempted exactly once.
func closeRejectedComponentHandles(handles []ComponentHandle, rejected error) error {
	cleanup := &openedBinding{handles: make([]openedComponentHandle, len(handles))}
	for index, handle := range handles {
		cleanup.handles[index] = openedComponentHandle{component: handle.Component, close: handle.Close}
	}
	return closeRejectedOpenedBinding(cleanup, rejected)
}

type openedComponentHandle struct {
	component ComponentID
	close     func() error
	closed    bool
}

type openedBinding struct {
	descriptor   Descriptor
	capabilities ClassCapabilities
	work         *WorkTopology
	graph        GraphStore
	sessions     SessionsStore
	messaging    MessagingFrontDoorBinder
	orders       OrdersStore
	nudges       *NudgeFrontDoors
	handles      []openedComponentHandle
	closeMu      sync.Mutex
}

func (b *openedBinding) Descriptor() Descriptor { return b.descriptor.Clone() }

func (b *openedBinding) Capabilities() ClassCapabilities { return b.capabilities }

func (b *openedBinding) Work() (WorkTopology, bool) {
	if b.work == nil {
		return WorkTopology{}, false
	}
	work, err := cloneWorkTopology(*b.work)
	if err != nil {
		return WorkTopology{}, false
	}
	return work, true
}

func (b *openedBinding) Graph() (GraphStore, bool) { return b.graph, b.graph != nil }

func (b *openedBinding) Sessions() (SessionsStore, bool) { return b.sessions, b.sessions != nil }

func (b *openedBinding) Messaging() (MessagingFrontDoorBinder, bool) {
	return b.messaging, !isNilInterface(b.messaging)
}

func (b *openedBinding) Orders() (OrdersStore, bool) { return b.orders, b.orders != nil }

func (b *openedBinding) Nudges() (NudgeFrontDoors, bool) {
	if b.nudges == nil {
		return NudgeFrontDoors{}, false
	}
	return *b.nudges, true
}

func (b *openedBinding) Close() error {
	b.closeMu.Lock()
	defer b.closeMu.Unlock()
	errs := make([]error, 0, len(b.handles))
	for index := len(b.handles) - 1; index >= 0; index-- {
		handle := &b.handles[index]
		if handle.closed || handle.close == nil {
			continue
		}
		if err := handle.close(); err != nil {
			errs = append(errs, fmt.Errorf("closing component %q: %w", handle.component, err))
			continue
		}
		handle.closed = true
	}
	return errors.Join(errs...)
}

// resolvedOpenedBinding snapshots a provider result at the resolver boundary
// and owns a close-once wrapper even when the provider returned a direct,
// non-idempotent OpenedBinding implementation.
type resolvedOpenedBinding struct {
	source       OpenedBinding
	descriptor   Descriptor
	capabilities ClassCapabilities
	work         *WorkTopology
	graph        GraphStore
	graphOK      bool
	sessions     SessionsStore
	sessionsOK   bool
	messaging    MessagingFrontDoorBinder
	messagingOK  bool
	orders       OrdersStore
	ordersOK     bool
	nudges       NudgeFrontDoors
	nudgesOK     bool
	closeMu      sync.Mutex
	closed       bool
}

func snapshotOpenedBinding(source OpenedBinding) (*resolvedOpenedBinding, error) {
	resolved := &resolvedOpenedBinding{
		source:       source,
		descriptor:   source.Descriptor().Clone(),
		capabilities: source.Capabilities(),
	}
	if work, ok := source.Work(); ok {
		frozen, err := cloneWorkTopology(work)
		if err != nil {
			return nil, fmt.Errorf("%w: provider returned invalid Work topology", ErrInvalidWorkParticipant)
		}
		resolved.work = &frozen
	}
	resolved.graph, resolved.graphOK = source.Graph()
	resolved.sessions, resolved.sessionsOK = source.Sessions()
	messaging, hasMessaging := source.Messaging()
	if hasMessaging {
		managed, err := newManagedMessagingFrontDoorBinder(messaging)
		if err != nil {
			return nil, err
		}
		resolved.messaging = managed
		resolved.messagingOK = true
	}
	resolved.orders, resolved.ordersOK = source.Orders()
	resolved.nudges, resolved.nudgesOK = source.Nudges()
	return resolved, nil
}

func (b *resolvedOpenedBinding) Descriptor() Descriptor { return b.descriptor.Clone() }

func (b *resolvedOpenedBinding) Capabilities() ClassCapabilities { return b.capabilities }

func (b *resolvedOpenedBinding) Work() (WorkTopology, bool) {
	if b.work == nil {
		return WorkTopology{}, false
	}
	cloned, err := cloneWorkTopology(*b.work)
	if err != nil {
		return WorkTopology{}, false
	}
	return cloned, true
}

func (b *resolvedOpenedBinding) Graph() (GraphStore, bool) { return b.graph, b.graphOK }

func (b *resolvedOpenedBinding) Sessions() (SessionsStore, bool) { return b.sessions, b.sessionsOK }

func (b *resolvedOpenedBinding) Messaging() (MessagingFrontDoorBinder, bool) {
	return b.messaging, b.messagingOK
}

func (b *resolvedOpenedBinding) Orders() (OrdersStore, bool) { return b.orders, b.ordersOK }

func (b *resolvedOpenedBinding) Nudges() (NudgeFrontDoors, bool) { return b.nudges, b.nudgesOK }

func (b *resolvedOpenedBinding) Close() error {
	b.closeMu.Lock()
	defer b.closeMu.Unlock()
	if b.closed {
		return nil
	}
	if err := b.source.Close(); err != nil {
		return err
	}
	b.closed = true
	return nil
}

// OpenBinding validates a request before invoking Provider.Open and validates its result.
func OpenBinding(ctx context.Context, provider Provider, request OpenRequest) (OpenedBinding, error) {
	if err := request.validate(ctx); err != nil {
		return nil, err
	}
	baseline, err := freezeOpenRequest(request)
	if err != nil {
		return nil, err
	}
	providerInput, err := freezeOpenRequest(baseline)
	if err != nil {
		return nil, err
	}
	if isNilInterface(provider) {
		return nil, fmt.Errorf("%w: provider is nil", ErrProviderUnavailable)
	}
	var fenceSnapshots []writerFenceSnapshot
	if baseline.requiresAdmissionFence() {
		fence, err := snapshotWriterFence(ctx, baseline.AdmissionFence)
		if err != nil {
			return nil, err
		}
		fenceSnapshots = append(fenceSnapshots, fence)
	}
	returned, providerErr := provider.Open(ctx, providerInput)
	var rejected error
	if providerErr != nil {
		rejected = fmt.Errorf("opening storage binding: %w", providerErr)
	} else if isNilInterface(returned) {
		rejected = fmt.Errorf("opening storage binding: %w", ErrProviderUnavailable)
	}
	if fenceErr := checkWriterFenceSnapshots(ctx, fenceSnapshots...); fenceErr != nil {
		rejected = errors.Join(rejected, fenceErr)
	}
	if rejected != nil {
		if !isNilInterface(returned) {
			return nil, closeRejectedOpenedBinding(returned, rejected)
		}
		return nil, rejected
	}
	opened, err := snapshotOpenedBinding(returned)
	if err != nil {
		return nil, rejectOpenedBindingAfterFenceCheck(ctx, returned, err, fenceSnapshots)
	}
	actual := opened.Descriptor()
	if err := actual.Validate(); err != nil {
		return nil, rejectOpenedBindingAfterFenceCheck(ctx, opened, err, fenceSnapshots)
	}
	expectedIdentity, err := baseline.Descriptor.Identity()
	if err != nil {
		return nil, rejectOpenedBindingAfterFenceCheck(ctx, opened, err, fenceSnapshots)
	}
	actualIdentity, err := actual.Identity()
	if err != nil {
		return nil, rejectOpenedBindingAfterFenceCheck(ctx, opened, err, fenceSnapshots)
	}
	if expectedIdentity != actualIdentity {
		return nil, rejectOpenedBindingAfterFenceCheck(ctx, opened, fmt.Errorf("%w: opened descriptor identity changed", ErrFenceTargetMoved), fenceSnapshots)
	}
	if !baseline.Descriptor.Equal(actual) {
		return nil, rejectOpenedBindingAfterFenceCheck(ctx, opened, fmt.Errorf("%w: opened descriptor facts changed", ErrFenceTargetMoved), fenceSnapshots)
	}
	capabilities := opened.Capabilities()
	if err := validateOpenedBindingSurface(opened, actual, capabilities); err != nil {
		return nil, rejectOpenedBindingAfterFenceCheck(ctx, opened, err, fenceSnapshots)
	}
	if baseline.PinnedWork != nil {
		actualWork, hasWork := opened.Work()
		if !hasWork || !workTopologiesEqual(*baseline.PinnedWork, actualWork) {
			return nil, rejectOpenedBindingAfterFenceCheck(ctx, opened, fmt.Errorf("%w: opened Work topology differs from pinned topology", ErrInvalidWorkParticipant), fenceSnapshots)
		}
	}
	if err := capabilities.ValidateRequired(baseline.AssignedClasses, false, false, baseline.requiresAdmissionFence()); err != nil {
		return nil, rejectOpenedBindingAfterFenceCheck(ctx, opened, err, fenceSnapshots)
	}
	if err := validateOpenClassRequirements(capabilities, baseline.AssignedClasses, baseline.ClassRequirements); err != nil {
		return nil, rejectOpenedBindingAfterFenceCheck(ctx, opened, err, fenceSnapshots)
	}
	if err := checkWriterFenceSnapshots(ctx, fenceSnapshots...); err != nil {
		return nil, closeRejectedOpenedBinding(opened, err)
	}
	return opened, nil
}

func rejectOpenedBindingAfterFenceCheck(ctx context.Context, opened OpenedBinding, rejected error, snapshots []writerFenceSnapshot) error {
	if fenceErr := checkWriterFenceSnapshots(ctx, snapshots...); fenceErr != nil {
		rejected = errors.Join(rejected, fenceErr)
	}
	return closeRejectedOpenedBinding(opened, rejected)
}

func closeRejectedOpenedBinding(opened OpenedBinding, rejected error) error {
	if err := opened.Close(); err != nil {
		return &RejectedOpenedBindingCleanupError{
			rejected:   rejected,
			cleanupErr: fmt.Errorf("closing rejected storage binding: %w", err),
			binding:    opened,
		}
	}
	return rejected
}

// RejectedOpenedBindingCleanupError retains cleanup ownership when a rejected
// binding cannot close immediately. The binding is never returned as usable;
// callers may only retry cleanup through RetryCleanup.
type RejectedOpenedBindingCleanupError struct {
	rejected   error
	cleanupErr error
	binding    OpenedBinding
	mu         sync.Mutex
	cleaned    bool
}

// Error reports both the rejection and its initial cleanup failure.
func (e *RejectedOpenedBindingCleanupError) Error() string {
	if e == nil {
		return "rejected storage binding cleanup"
	}
	return errors.Join(e.rejected, e.cleanupErr).Error()
}

// Unwrap exposes the rejection and cleanup failure for errors.Is and errors.As.
func (e *RejectedOpenedBindingCleanupError) Unwrap() []error {
	if e == nil {
		return nil
	}
	return []error{e.rejected, e.cleanupErr}
}

// RetryCleanup retries closing the rejected binding. A successful retry is
// remembered; a failed retry keeps the binding cleanup-capable and unreachable
// except through this error.
func (e *RejectedOpenedBindingCleanupError) RetryCleanup() error {
	if e == nil || isNilInterface(e.binding) {
		return ErrProviderUnavailable
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cleaned {
		return nil
	}
	if err := e.binding.Close(); err != nil {
		return fmt.Errorf("retrying rejected storage binding cleanup: %w", err)
	}
	e.cleaned = true
	return nil
}

type openedClassFronts struct {
	work      openedClassFront
	graph     openedClassFront
	sessions  openedClassFront
	messaging openedClassFront
	orders    openedClassFront
	nudges    openedClassFront
}

type openedClassFront struct {
	present bool
	usable  bool
}

func validateOpenedBindingParts(parts OpenedBindingParts) error {
	fronts := openedClassFronts{
		work:      openedClassFront{present: parts.Work != nil, usable: parts.Work != nil},
		graph:     openedClassFront{present: !isNilInterface(parts.Graph), usable: !isNilInterface(parts.Graph)},
		sessions:  openedClassFront{present: !isNilInterface(parts.Sessions), usable: !isNilInterface(parts.Sessions)},
		messaging: openedClassFront{present: !isNilInterface(parts.Messaging), usable: !isNilInterface(parts.Messaging)},
		orders:    openedClassFront{present: !isNilInterface(parts.Orders), usable: !isNilInterface(parts.Orders)},
		nudges:    openedClassFront{present: parts.Nudges != nil, usable: parts.Nudges != nil && nudgeFrontDoorsUsable(*parts.Nudges)},
	}
	return validateOpenedBindingFronts(parts.Descriptor.Classes(), parts.Capabilities, parts.Descriptor.Capabilities, fronts)
}

func validateOpenedBindingSurface(opened OpenedBinding, descriptor Descriptor, capabilities ClassCapabilities) error {
	work, hasWork := opened.Work()
	graph, hasGraph := opened.Graph()
	sessions, hasSessions := opened.Sessions()
	messaging, hasMessaging := opened.Messaging()
	orders, hasOrders := opened.Orders()
	nudges, hasNudges := opened.Nudges()
	fronts := openedClassFronts{
		work:      openedClassFront{present: hasWork, usable: hasWork && workTopologyUsable(work)},
		graph:     openedClassFront{present: hasGraph, usable: hasGraph && !isNilInterface(graph)},
		sessions:  openedClassFront{present: hasSessions, usable: hasSessions && !isNilInterface(sessions)},
		messaging: openedClassFront{present: hasMessaging, usable: hasMessaging && !isNilInterface(messaging)},
		orders:    openedClassFront{present: hasOrders, usable: hasOrders && !isNilInterface(orders)},
		nudges:    openedClassFront{present: hasNudges, usable: hasNudges && nudgeFrontDoorsUsable(nudges)},
	}
	return validateOpenedBindingFronts(descriptor.Classes(), capabilities, descriptor.Capabilities, fronts)
}

func messagingFrontDoorsUsable(fronts MessagingFrontDoors) bool {
	return !isNilInterface(fronts.Mail) && !isNilInterface(fronts.Bindings) && !isNilInterface(fronts.DeliveryContexts) && !isNilInterface(fronts.Groups) && !isNilInterface(fronts.Transcripts)
}

type managedMessagingFrontDoorBinder struct {
	source MessagingFrontDoorBinder
	mu     sync.Mutex
	bound  bool
}

func newManagedMessagingFrontDoorBinder(source MessagingFrontDoorBinder) (MessagingFrontDoorBinder, error) {
	if isNilInterface(source) {
		return nil, ErrInvalidMessagingBinding
	}
	return &managedMessagingFrontDoorBinder{source: source}, nil
}

func (b *managedMessagingFrontDoorBinder) BindSessions(sessions SessionsAddressDirectory) (MessagingFrontDoors, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.bound {
		return MessagingFrontDoors{}, ErrMessagingAlreadyBound
	}
	if isNilInterface(sessions) {
		return MessagingFrontDoors{}, ErrInvalidMessagingBinding
	}
	fronts, err := b.source.BindSessions(sessions)
	if err != nil {
		return MessagingFrontDoors{}, fmt.Errorf("binding opened Messaging persistence to Sessions directory: %w", err)
	}
	if !messagingFrontDoorsUsable(fronts) {
		return MessagingFrontDoors{}, ErrInvalidMessagingBinding
	}
	b.bound = true
	return fronts, nil
}

func nudgeFrontDoorsUsable(fronts NudgeFrontDoors) bool {
	return !isNilInterface(fronts.Queue) && !isNilInterface(fronts.Shadows)
}

func workTopologyUsable(topology WorkTopology) bool {
	_, err := cloneWorkTopology(topology)
	return err == nil
}

func cloneWorkTopology(topology WorkTopology) (WorkTopology, error) {
	hq, err := topology.ForScope(HQScope())
	if err != nil || !workspaceFactsEqual(hq, topology.hq) {
		return WorkTopology{}, ErrInvalidWorkParticipant
	}
	rigs := append([]Workspace(nil), topology.rigs...)
	cloned, err := NewWorkTopology(hq, rigs)
	if err != nil {
		return WorkTopology{}, err
	}
	if !workTopologiesEqual(topology, cloned) {
		return WorkTopology{}, ErrInvalidWorkParticipant
	}
	return cloned, nil
}

func workTopologiesEqual(left, right WorkTopology) bool {
	if !workspaceFactsEqual(left.hq, right.hq) || len(left.rigs) != len(right.rigs) {
		return false
	}
	for index := range left.rigs {
		if !workspaceFactsEqual(left.rigs[index], right.rigs[index]) {
			return false
		}
	}
	return true
}

func workspaceFactsEqual(left, right Workspace) bool {
	return left.Scope == right.Scope && left.Prefix == right.Prefix && left.Suspended == right.Suspended && left.OpenerID == right.OpenerID && left.ComponentID == right.ComponentID && left.PhysicalID == right.PhysicalID
}

func validateOpenedBindingFronts(classes ClassSet, actualCapabilities, descriptorCapabilities ClassCapabilities, fronts openedClassFronts) error {
	if !actualCapabilities.Equal(descriptorCapabilities) {
		return fmt.Errorf("%w: opened capabilities differ from descriptor", ErrInvalidDescriptor)
	}
	for _, front := range []struct {
		class coordclass.Class
		front openedClassFront
	}{
		{class: coordclass.ClassWork, front: fronts.work},
		{class: coordclass.ClassGraph, front: fronts.graph},
		{class: coordclass.ClassSessions, front: fronts.sessions},
		{class: coordclass.ClassMessaging, front: fronts.messaging},
		{class: coordclass.ClassOrders, front: fronts.orders},
		{class: coordclass.ClassNudges, front: fronts.nudges},
	} {
		if classes.Has(front.class) != front.front.present {
			return fmt.Errorf("%w: typed front for class %s does not match descriptor", ErrInvalidDescriptor, front.class)
		}
		if front.front.present && !front.front.usable {
			return fmt.Errorf("%w: typed front for class %s is nil", ErrInvalidDescriptor, front.class)
		}
	}
	return nil
}

// RetainedSourceRef is an opaque provider-issued, secret-free retained-source identity.
type RetainedSourceRef struct {
	Version                 uint16
	Provider                ProviderID
	ImplementationVersion   string
	Component               ComponentID
	Classes                 ClassSet
	SemanticContractVersion ContractVersion
	Format                  FormatID
	SchemaVersion           string
	ABIVersion              string
	PhysicalIdentity        PhysicalIdentity
	ConfigRefDigest         ConfigRefDigest
	WitnessVersion          string
	WitnessDigest           string
	ReopenData              []byte
}

// Clone returns a detached retained-source value.
func (r RetainedSourceRef) Clone() RetainedSourceRef {
	out := r
	out.ReopenData = append([]byte(nil), r.ReopenData...)
	return out
}

// Validate verifies that a retained source remains provider-pinned and credential-free.
func (r RetainedSourceRef) Validate() error {
	if r.Version != 1 || strings.TrimSpace(string(r.Provider)) == "" || strings.TrimSpace(r.ImplementationVersion) == "" || strings.TrimSpace(string(r.Component)) == "" || r.Classes.Empty() || strings.TrimSpace(string(r.SemanticContractVersion)) == "" || strings.TrimSpace(string(r.Format)) == "" || strings.TrimSpace(string(r.PhysicalIdentity)) == "" || strings.TrimSpace(string(r.ConfigRefDigest)) == "" || strings.TrimSpace(r.WitnessVersion) == "" || strings.TrimSpace(r.WitnessDigest) == "" {
		return ErrInvalidRetainedSource
	}
	if strings.TrimSpace(r.SchemaVersion) == "" && strings.TrimSpace(r.ABIVersion) == "" {
		return ErrInvalidRetainedSource
	}
	if err := validateIdentifier("retained source provider ID", string(r.Provider)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRetainedSource, err)
	}
	if err := validateIdentifier("retained source component ID", string(r.Component)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRetainedSource, err)
	}
	if err := validateCanonicalSHA256Digest("retained source config reference digest", string(r.ConfigRefDigest)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRetainedSource, err)
	}
	if err := validateCanonicalSHA256Digest("retained source witness digest", r.WitnessDigest); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRetainedSource, err)
	}
	for field, value := range map[string]string{
		"retained source implementation version": r.ImplementationVersion,
		"retained source semantic contract":      string(r.SemanticContractVersion),
		"retained source format":                 string(r.Format),
		"retained source schema version":         r.SchemaVersion,
		"retained source ABI version":            r.ABIVersion,
		"retained source physical identity":      string(r.PhysicalIdentity),
		"retained source witness version":        r.WitnessVersion,
		"retained source reopen data":            string(r.ReopenData),
	} {
		if err := validateSecretFree(field, value); err != nil {
			return err
		}
	}
	return nil
}

// GuardRole is the closed enforcement role for a retained-source guard.
type GuardRole uint8

const (
	// GuardRoleDenyWrite prevents writes to a retained authority.
	GuardRoleDenyWrite GuardRole = iota + 1
	// GuardRoleTransfer holds prior enforcement while a reverse saga takes ownership.
	// It is produced only by ReleaseOrTransfer, never by initial installation.
	GuardRoleTransfer
)

// Valid reports whether role belongs to the closed guard-role set.
func (r GuardRole) Valid() bool { return r == GuardRoleDenyWrite || r == GuardRoleTransfer }

// GuardTransferState records the exact target-saga state bound into a
// transferred retained-source guard.
type GuardTransferState uint8

const (
	// GuardTransferDecided keeps enforcement with a newer saga after its durable
	// COMMIT_DECIDED record, before that saga has become active.
	GuardTransferDecided GuardTransferState = iota + 1
	// GuardTransferActive records the activation proof for the newer saga.
	GuardTransferActive
)

// Valid reports whether state belongs to the closed transfer-state set.
func (s GuardTransferState) Valid() bool {
	return s == GuardTransferDecided || s == GuardTransferActive
}

// GuardInstallRequest identifies one source-owned initial deny-write retained guard.
type GuardInstallRequest struct {
	Attempt          AttemptID
	Generation       Generation
	Source           RetainedSourceRef
	Component        ComponentID
	PhysicalIdentity PhysicalIdentity
	Role             GuardRole
}

// Clone returns a detached retained-guard install request for a provider boundary.
func (r GuardInstallRequest) Clone() GuardInstallRequest {
	out := r
	out.Source = r.Source.Clone()
	return out
}

// Validate verifies that a guard install is pinned by attempt and physical identity.
func (r GuardInstallRequest) Validate() error {
	if strings.TrimSpace(string(r.Attempt)) == "" || !r.Generation.Valid() || r.Role != GuardRoleDenyWrite || strings.TrimSpace(string(r.Component)) == "" || strings.TrimSpace(string(r.PhysicalIdentity)) == "" {
		return ErrInvalidGuard
	}
	if err := validateSecretFree("guard attempt", string(r.Attempt)); err != nil {
		return err
	}
	if err := r.Source.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidGuard, err)
	}
	if r.Source.Component != r.Component || r.Source.PhysicalIdentity != r.PhysicalIdentity {
		return fmt.Errorf("%w: source component or physical identity differs", ErrInvalidGuard)
	}
	return nil
}

// GuardDiscoverRequest finds an install that completed before its receipt was persisted.
type GuardDiscoverRequest struct {
	Attempt          AttemptID
	Generation       Generation
	Source           RetainedSourceRef
	Component        ComponentID
	PhysicalIdentity PhysicalIdentity
	Role             GuardRole
}

// Clone returns a detached retained-guard discovery request for a provider boundary.
func (r GuardDiscoverRequest) Clone() GuardDiscoverRequest {
	out := r
	out.Source = r.Source.Clone()
	return out
}

// Validate verifies discovery uses the same provider-pinned key as install.
func (r GuardDiscoverRequest) Validate() error {
	return GuardInstallRequest(r).Validate()
}

// FencedGuardInstallRequest adds the caller-owned source exclusion required
// while a provider installs a retained-source guard. GuardInstallRequest stays
// serializable; the live fence is deliberately an operation-only envelope.
type FencedGuardInstallRequest struct {
	GuardInstallRequest
	SourceFence WriterFence
}

// Clone returns a detached fenced install request for a provider boundary.
// The caller-owned source fence remains shared intentionally.
func (r FencedGuardInstallRequest) Clone() FencedGuardInstallRequest {
	return FencedGuardInstallRequest{GuardInstallRequest: r.GuardInstallRequest.Clone(), SourceFence: r.SourceFence}
}

// Validate verifies that the source fence exactly excludes the retained
// component during guard installation.
func (r FencedGuardInstallRequest) Validate(ctx context.Context) error {
	if err := r.GuardInstallRequest.Validate(); err != nil {
		return err
	}
	return validateRetainedGuardSourceFence(ctx, r.GuardInstallRequest, r.SourceFence)
}

// FencedGuardDiscoverRequest adds the caller-owned source exclusion required
// while a provider discovers a retained-source guard receipt.
type FencedGuardDiscoverRequest struct {
	GuardDiscoverRequest
	SourceFence WriterFence
}

// Clone returns a detached fenced discovery request for a provider boundary.
// The caller-owned source fence remains shared intentionally.
func (r FencedGuardDiscoverRequest) Clone() FencedGuardDiscoverRequest {
	return FencedGuardDiscoverRequest{GuardDiscoverRequest: r.GuardDiscoverRequest.Clone(), SourceFence: r.SourceFence}
}

// Validate verifies that discovery retains the exact source exclusion used by
// the matching installation request.
func (r FencedGuardDiscoverRequest) Validate(ctx context.Context) error {
	if err := r.GuardDiscoverRequest.Validate(); err != nil {
		return err
	}
	return validateRetainedGuardSourceFence(ctx, GuardInstallRequest{
		Attempt:          r.Attempt,
		Generation:       r.Generation,
		Source:           r.Source,
		Component:        r.Component,
		PhysicalIdentity: r.PhysicalIdentity,
		Role:             r.Role,
	}, r.SourceFence)
}

func (r FencedGuardInstallRequest) discoveryRequest() FencedGuardDiscoverRequest {
	return FencedGuardDiscoverRequest{
		GuardDiscoverRequest: GuardDiscoverRequest{
			Attempt:          r.Attempt,
			Generation:       r.Generation,
			Source:           r.Source.Clone(),
			Component:        r.Component,
			PhysicalIdentity: r.PhysicalIdentity,
			Role:             r.Role,
		},
		SourceFence: r.SourceFence,
	}
}

func validateRetainedGuardSourceFence(ctx context.Context, request GuardInstallRequest, fence WriterFence) error {
	return validateRetainedGuardFence(ctx, request.Source.Provider, request.Component, request.PhysicalIdentity, request.Source.Classes, request.Generation, fence)
}

func validateRetainedGuardFence(ctx context.Context, provider ProviderID, componentID ComponentID, physicalIdentity PhysicalIdentity, classes ClassSet, generation Generation, fence WriterFence) error {
	if isNilInterface(fence) || fence.Role() != FenceRoleSource || fence.Generation() != generation {
		return ErrInvalidFence
	}
	target := fence.Target()
	if err := target.Validate(); err != nil {
		return ErrInvalidFence
	}
	if target.Provider != provider || !target.Classes.Contains(classes) || !sameComponentIDs(fence.CoveredComponents(), fenceTargetComponentIDs(target)) {
		return ErrInvalidFence
	}
	for _, component := range target.Components {
		if component.ID == componentID && component.PhysicalIdentity == physicalIdentity && component.Classes.Equal(classes) {
			return heldFence(ctx, fence)
		}
	}
	return ErrInvalidFence
}

// GuardReceipt is a versioned provider receipt for retained-source enforcement.
type GuardReceipt struct {
	Version                     uint16
	Attempt                     AttemptID
	Generation                  Generation
	Provider                    ProviderID
	Component                   ComponentID
	PhysicalIdentity            PhysicalIdentity
	Classes                     ClassSet
	SemanticContractVersion     ContractVersion
	Role                        GuardRole
	TransferState               GuardTransferState
	TransferParticipant         string
	TransferDestinationIdentity BindingIdentity
	TransferReceiptKind         ParticipantReceiptKind
	ActiveProof                 *ParticipantReceipt
	ReceiptID                   string
	Revalidation                string
}

// Clone returns a detached retained-guard receipt value.
func (r GuardReceipt) Clone() GuardReceipt {
	out := r
	if r.ActiveProof != nil {
		proof := r.ActiveProof.Clone()
		out.ActiveProof = &proof
	}
	return out
}

// IsZero reports whether a guard receipt carries no provider facts.
func (r GuardReceipt) IsZero() bool {
	return r.Version == 0 && r.Attempt == "" && r.Generation == 0 && r.Provider == "" && r.Component == "" && r.PhysicalIdentity == "" && r.Classes.Empty() && r.SemanticContractVersion == "" && r.Role == 0 && r.TransferState == 0 && r.TransferParticipant == "" && r.TransferDestinationIdentity == "" && r.TransferReceiptKind == 0 && r.ActiveProof == nil && r.ReceiptID == "" && r.Revalidation == ""
}

// Validate verifies that a receipt is safe to persist and revalidate.
func (r GuardReceipt) Validate() error {
	if r.Version != 1 || strings.TrimSpace(string(r.Attempt)) == "" || !r.Generation.Valid() || strings.TrimSpace(string(r.Provider)) == "" || strings.TrimSpace(string(r.Component)) == "" || strings.TrimSpace(string(r.PhysicalIdentity)) == "" || r.Classes.Empty() || strings.TrimSpace(string(r.SemanticContractVersion)) == "" || strings.TrimSpace(r.ReceiptID) == "" || strings.TrimSpace(r.Revalidation) == "" || !r.Role.Valid() {
		return ErrInvalidGuard
	}
	if err := validateSecretFree("guard attempt", string(r.Attempt)); err != nil {
		return err
	}
	if err := validateIdentifier("guard provider ID", string(r.Provider)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidGuard, err)
	}
	if err := validateIdentifier("guard component ID", string(r.Component)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidGuard, err)
	}
	if r.Role != GuardRoleTransfer && (r.TransferState != 0 || r.TransferParticipant != "" || r.TransferDestinationIdentity != "" || r.TransferReceiptKind != 0 || r.ActiveProof != nil) {
		return ErrInvalidGuard
	}
	if r.Role == GuardRoleTransfer {
		if !r.TransferState.Valid() || strings.TrimSpace(r.TransferParticipant) == "" || strings.TrimSpace(string(r.TransferDestinationIdentity)) == "" || !r.TransferReceiptKind.Valid() {
			return ErrInvalidGuard
		}
		if err := validateSecretFree("guard transfer participant", r.TransferParticipant); err != nil {
			return err
		}
		if err := validateCanonicalSHA256Digest("guard transfer destination identity", string(r.TransferDestinationIdentity)); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidGuard, err)
		}
		if r.TransferState == GuardTransferDecided && r.ActiveProof != nil {
			return ErrInvalidGuard
		}
		if r.TransferState == GuardTransferActive {
			if r.ActiveProof == nil || r.ActiveProof.Validate() != nil || r.ActiveProof.Attempt != r.Attempt || r.ActiveProof.Generation != r.Generation || r.ActiveProof.Participant != r.TransferParticipant || r.ActiveProof.DescriptorIdentity != r.TransferDestinationIdentity || r.ActiveProof.Kind != r.TransferReceiptKind {
				return ErrInvalidGuard
			}
		}
	}
	for field, value := range map[string]string{
		"guard provider":          string(r.Provider),
		"guard component":         string(r.Component),
		"guard physical identity": string(r.PhysicalIdentity),
		"guard semantic contract": string(r.SemanticContractVersion),
		"guard receipt":           r.ReceiptID,
		"guard revalidation":      r.Revalidation,
	} {
		if err := validateSecretFree(field, value); err != nil {
			return err
		}
	}
	return nil
}

// FencedGuardVerificationRequest supplies a retained-guard receipt together
// with the caller-owned source exclusion that must remain live while the
// provider revalidates it.
type FencedGuardVerificationRequest struct {
	Receipt     GuardReceipt
	SourceFence WriterFence
}

// Clone returns a detached guard verification request for a provider boundary.
// The caller-owned source fence remains shared intentionally.
func (r FencedGuardVerificationRequest) Clone() FencedGuardVerificationRequest {
	return FencedGuardVerificationRequest{Receipt: r.Receipt.Clone(), SourceFence: r.SourceFence}
}

// Validate verifies that the source fence covers the receipt's exact retained
// component while the provider revalidates it.
func (r FencedGuardVerificationRequest) Validate(ctx context.Context) error {
	if err := r.Receipt.Validate(); err != nil {
		return err
	}
	return validateRetainedGuardFence(ctx, r.Receipt.Provider, r.Receipt.Component, r.Receipt.PhysicalIdentity, r.Receipt.Classes, r.Receipt.Generation, r.SourceFence)
}

// GuardDiscovery is the result of a provider-side guard receipt discovery.
type GuardDiscovery struct {
	Found   bool
	Receipt GuardReceipt
}

// Clone returns a detached retained-guard discovery value.
func (d GuardDiscovery) Clone() GuardDiscovery {
	return GuardDiscovery{Found: d.Found, Receipt: d.Receipt.Clone()}
}

// Validate verifies that absent discoveries do not manufacture a receipt.
func (d GuardDiscovery) Validate() error {
	if !d.Found {
		if !d.Receipt.IsZero() {
			return ErrInvalidGuard
		}
		return nil
	}
	return d.Receipt.Validate()
}

// GuardTransferTarget identifies the newer saga that takes over retained-source
// enforcement. A transfer can remain decided until activation, or bind the
// participant receipt proving that the newer saga became active.
type GuardTransferTarget struct {
	Decision            CommitDecision
	Participant         string
	DestinationIdentity BindingIdentity
	ExpectedReceiptKind ParticipantReceiptKind
	State               GuardTransferState
	ActiveProof         *ParticipantReceipt
}

// Clone returns a detached guard-transfer target for a provider boundary.
func (t GuardTransferTarget) Clone() GuardTransferTarget {
	out := t
	if t.ActiveProof != nil {
		proof := t.ActiveProof.Clone()
		out.ActiveProof = &proof
	}
	return out
}

// Validate verifies that a target is a strictly newer decided saga and that an
// active target carries the exact activation receipt for that decision.
func (t GuardTransferTarget) Validate(current GuardReceipt) error {
	if err := t.Decision.Validate(); err != nil || !t.State.Valid() || strings.TrimSpace(t.Participant) == "" || strings.TrimSpace(string(t.DestinationIdentity)) == "" || !t.ExpectedReceiptKind.Valid() || t.Decision.Attempt == current.Attempt || t.Decision.Generation <= current.Generation {
		return ErrInvalidGuard
	}
	if err := validateSecretFree("guard transfer participant", t.Participant); err != nil {
		return err
	}
	if err := validateCanonicalSHA256Digest("guard transfer destination identity", string(t.DestinationIdentity)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidGuard, err)
	}
	if t.State == GuardTransferDecided {
		if t.ActiveProof != nil {
			return ErrInvalidGuard
		}
		return nil
	}
	if t.ActiveProof == nil || t.ActiveProof.Validate() != nil || t.ActiveProof.Attempt != t.Decision.Attempt || t.ActiveProof.Generation != t.Decision.Generation || t.ActiveProof.Participant != t.Participant || t.ActiveProof.DescriptorIdentity != t.DestinationIdentity || t.ActiveProof.Kind != t.ExpectedReceiptKind {
		return ErrInvalidGuard
	}
	return nil
}

// PredecisionAbandonmentAuthority is an opaque controller-minted proof that a
// source guard may be released before a durable commit decision exists. Its
// fields are deliberately unexported: only the durable manifest recovery
// state machine may mint one after its unchanged-source recensus.
type PredecisionAbandonmentAuthority struct {
	version                  uint16
	attempt                  AttemptID
	generation               Generation
	provider                 ProviderID
	component                ComponentID
	physicalIdentity         PhysicalIdentity
	classes                  ClassSet
	semanticContractVersion  ContractVersion
	role                     GuardRole
	receiptID                string
	revalidation             string
	sourceDescriptorIdentity BindingIdentity
	sourceFenceTarget        FenceTarget
}

func (a PredecisionAbandonmentAuthority) clone() PredecisionAbandonmentAuthority {
	out := a
	out.sourceFenceTarget = a.sourceFenceTarget.Clone()
	return out
}

func (a PredecisionAbandonmentAuthority) validate(current GuardReceipt, descriptor Descriptor, fence WriterFence) error {
	if current.Role != GuardRoleDenyWrite || a.version != 1 || a.attempt != current.Attempt || a.generation != current.Generation || a.provider != current.Provider || a.component != current.Component || a.physicalIdentity != current.PhysicalIdentity || !a.classes.Equal(current.Classes) || a.semanticContractVersion != current.SemanticContractVersion || a.role != current.Role || a.receiptID != current.ReceiptID || a.revalidation != current.Revalidation {
		return ErrInvalidGuard
	}
	if err := validateCanonicalSHA256Digest("predecision abandonment source descriptor identity", string(a.sourceDescriptorIdentity)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidGuard, err)
	}
	if err := a.sourceFenceTarget.Validate(); err != nil {
		return ErrInvalidGuard
	}
	descriptorIdentity, err := descriptor.Identity()
	if err != nil || descriptorIdentity != a.sourceDescriptorIdentity || !a.sourceFenceTarget.Equal(fence.Target()) {
		return ErrInvalidGuard
	}
	return nil
}

// GuardTransitionRequest asks a provider to release existing enforcement or
// transfer it to one explicit newer saga.
type GuardTransitionRequest struct {
	Current          GuardReceipt
	Release          bool
	Abandonment      *PredecisionAbandonmentAuthority
	Target           *GuardTransferTarget
	SourceDescriptor Descriptor
	SourceFence      WriterFence
}

// Clone returns a detached retained-guard transition request for a provider boundary.
// The held source fence remains caller-owned and is intentionally shared.
func (r GuardTransitionRequest) Clone() GuardTransitionRequest {
	out := r
	out.Current = r.Current.Clone()
	out.SourceDescriptor = r.SourceDescriptor.Clone()
	if r.Abandonment != nil {
		authority := r.Abandonment.clone()
		out.Abandonment = &authority
	}
	if r.Target != nil {
		target := r.Target.Clone()
		out.Target = &target
	}
	return out
}

// Validate verifies a release or transfer uses the persisted physical guard receipt.
func (r GuardTransitionRequest) Validate(ctx context.Context) error {
	if err := r.Current.Validate(); err != nil {
		return err
	}
	if r.Release {
		if r.Target != nil || r.Abandonment == nil || isNilInterface(r.SourceFence) {
			return ErrInvalidGuard
		}
		if err := sourceGuardStillMatches(ctx, r.Current, r.SourceDescriptor, r.SourceFence); err != nil {
			return err
		}
		return r.Abandonment.validate(r.Current, r.SourceDescriptor, r.SourceFence)
	}
	if r.Abandonment != nil || r.Target == nil {
		return ErrInvalidGuard
	}
	return r.Target.Validate(r.Current)
}

func sourceGuardStillMatches(ctx context.Context, receipt GuardReceipt, descriptor Descriptor, fence WriterFence) error {
	if err := descriptor.Validate(); err != nil {
		return ErrInvalidGuard
	}
	if descriptor.Provider != receipt.Provider || descriptor.SemanticContractVersion != receipt.SemanticContractVersion || isNilInterface(fence) {
		return ErrInvalidGuard
	}
	if fence.Role() != FenceRoleSource || fence.Generation() != receipt.Generation {
		return ErrInvalidGuard
	}
	target := fence.Target()
	if err := target.Validate(); err != nil {
		return ErrInvalidGuard
	}
	if err := validateDescriptorForTarget(descriptor, target); err != nil {
		return ErrInvalidGuard
	}
	if !sameComponentIDs(fence.CoveredComponents(), fenceTargetComponentIDs(target)) {
		return ErrInvalidGuard
	}
	componentFound := false
	for _, component := range descriptor.Components {
		if component.ID == receipt.Component && component.PhysicalIdentity == receipt.PhysicalIdentity && component.Classes.Equal(receipt.Classes) {
			componentFound = true
			break
		}
	}
	if !componentFound || target.Provider != receipt.Provider {
		return ErrInvalidGuard
	}
	fenceComponentFound := false
	for _, component := range target.Components {
		if component.ID == receipt.Component && component.PhysicalIdentity == receipt.PhysicalIdentity && component.Classes.Equal(receipt.Classes) {
			fenceComponentFound = true
			break
		}
	}
	if !fenceComponentFound {
		return ErrInvalidGuard
	}
	held, err := fence.Held(ctx)
	if err != nil {
		return err
	}
	if !held {
		return ErrFenceNotHeld
	}
	return nil
}

// RetainedGuardLifecycle is implemented by the provider that owns a retained component.
type RetainedGuardLifecycle interface {
	Install(context.Context, FencedGuardInstallRequest) (GuardReceipt, error)
	Discover(context.Context, FencedGuardDiscoverRequest) (GuardDiscovery, error)
	Verify(context.Context, FencedGuardVerificationRequest) error
	ReleaseOrTransfer(context.Context, GuardTransitionRequest) (GuardReceipt, error)
}

// RetainedGuardLifecycleResolver resolves the exact source-provider lifecycle
// for retained guard recovery and activation attestation.
type RetainedGuardLifecycleResolver interface {
	ResolveRetainedGuardLifecycle(context.Context, ProviderID) (RetainedGuardLifecycle, error)
}

// RetainedGuardLifecycleResolverFunc adapts a function into a retained guard
// lifecycle resolver.
type RetainedGuardLifecycleResolverFunc func(context.Context, ProviderID) (RetainedGuardLifecycle, error)

// ResolveRetainedGuardLifecycle resolves one exact provider lifecycle.
func (f RetainedGuardLifecycleResolverFunc) ResolveRetainedGuardLifecycle(ctx context.Context, provider ProviderID) (RetainedGuardLifecycle, error) {
	return f(ctx, provider)
}

// GuardReceiptPersistence durably records one verified retained-guard receipt.
// It is called once per receipt after provider verification and before the next
// request is recovered, so a crash cannot leave later provider work ahead of
// earlier attempt-record state.
type GuardReceiptPersistence func(context.Context, GuardReceipt) error

// RecoverRetainedGuards discovers an install that survived a crash before its
// receipt fsync, verifies each guard, and durably persists each receipt.
func RecoverRetainedGuards(ctx context.Context, resolver RetainedGuardLifecycleResolver, requests []FencedGuardInstallRequest, persist GuardReceiptPersistence) ([]GuardReceipt, error) {
	if isNilInterface(resolver) {
		return nil, fmt.Errorf("%w: retained guard lifecycle resolver is nil", ErrMissingCapability)
	}
	if isNilInterface(persist) {
		return nil, fmt.Errorf("%w: retained guard receipt persistence is nil", ErrMissingCapability)
	}
	baselines := cloneFencedGuardInstallRequests(requests)
	if err := validateRetainedGuardPlan(ctx, baselines); err != nil {
		return nil, err
	}
	lifecycles, err := resolveRetainedGuardLifecycles(ctx, resolver, baselines)
	if err != nil {
		return nil, err
	}
	receipts := make([]GuardReceipt, 0, len(baselines))
	for _, request := range baselines {
		lifecycle := lifecycles[request.Source.Provider]
		discoverFence, err := snapshotWriterFence(ctx, request.SourceFence)
		if err != nil {
			return nil, err
		}
		discovery, providerErr := lifecycle.Discover(ctx, request.discoveryRequest().Clone())
		if err := providerFenceResult(ctx, "discovering retained guard", providerErr, discoverFence); err != nil {
			return nil, err
		}
		discovery = discovery.Clone()
		if err := discovery.Validate(); err != nil {
			return nil, err
		}
		receipt := discovery.Receipt.Clone()
		if !discovery.Found {
			installFence, err := snapshotWriterFence(ctx, request.SourceFence)
			if err != nil {
				return nil, err
			}
			receipt, providerErr = lifecycle.Install(ctx, request.Clone())
			if err := providerFenceResult(ctx, "installing retained guard", providerErr, installFence); err != nil {
				return nil, err
			}
			receipt = receipt.Clone()
		}
		if err := receipt.Validate(); err != nil {
			return nil, err
		}
		if receipt.Attempt != request.Attempt || receipt.Generation != request.Generation || receipt.Component != request.Component || receipt.PhysicalIdentity != request.PhysicalIdentity || !receipt.Classes.Equal(request.Source.Classes) || receipt.SemanticContractVersion != request.Source.SemanticContractVersion || receipt.Role != request.Role || receipt.Provider != request.Source.Provider {
			return nil, fmt.Errorf("%w: guard receipt does not match install request", ErrInvalidGuard)
		}
		verifyRequest := FencedGuardVerificationRequest{Receipt: receipt.Clone(), SourceFence: request.SourceFence}
		if err := verifyRequest.Validate(ctx); err != nil {
			return nil, err
		}
		verifyFence, err := snapshotWriterFence(ctx, verifyRequest.SourceFence)
		if err != nil {
			return nil, err
		}
		providerErr = lifecycle.Verify(ctx, verifyRequest.Clone())
		if err := providerFenceResult(ctx, "verifying retained guard", providerErr, verifyFence); err != nil {
			return nil, err
		}
		if err := persist(ctx, receipt.Clone()); err != nil {
			return nil, fmt.Errorf("persisting retained guard receipt: %w", err)
		}
		receipts = append(receipts, receipt.Clone())
	}
	return receipts, nil
}

func resolveRetainedGuardLifecycles(ctx context.Context, resolver RetainedGuardLifecycleResolver, requests []FencedGuardInstallRequest) (map[ProviderID]RetainedGuardLifecycle, error) {
	lifecycles := make(map[ProviderID]RetainedGuardLifecycle)
	for _, request := range requests {
		provider := request.Source.Provider
		if _, resolved := lifecycles[provider]; resolved {
			continue
		}
		lifecycle, err := resolver.ResolveRetainedGuardLifecycle(ctx, provider)
		if err != nil {
			return nil, fmt.Errorf("resolving retained guard lifecycle for provider %q: %w", provider, err)
		}
		if isNilInterface(lifecycle) {
			return nil, fmt.Errorf("%w: retained guard lifecycle for provider %q is nil", ErrMissingCapability, provider)
		}
		lifecycles[provider] = lifecycle
	}
	return lifecycles, nil
}

func cloneFencedGuardInstallRequests(requests []FencedGuardInstallRequest) []FencedGuardInstallRequest {
	cloned := make([]FencedGuardInstallRequest, len(requests))
	for index, request := range requests {
		cloned[index] = request.Clone()
	}
	return cloned
}

type retainedGuardPlanKey struct {
	attempt    AttemptID
	generation Generation
	provider   ProviderID
	component  ComponentID
	physical   PhysicalIdentity
	classes    ClassSet
	contract   ContractVersion
}

func validateRetainedGuardPlan(ctx context.Context, requests []FencedGuardInstallRequest) error {
	seen := make(map[retainedGuardPlanKey]struct{}, len(requests))
	for _, request := range requests {
		if err := request.Validate(ctx); err != nil {
			return err
		}
		key := retainedGuardPlanKey{
			attempt:    request.Attempt,
			generation: request.Generation,
			provider:   request.Source.Provider,
			component:  request.Component,
			physical:   request.PhysicalIdentity,
			classes:    request.Source.Classes,
			contract:   request.Source.SemanticContractVersion,
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("%w: duplicate retained guard install", ErrInvalidGuard)
		}
		seen[key] = struct{}{}
	}
	return nil
}

// TransitionRetainedGuard validates and performs a source-owned release or transfer.
func TransitionRetainedGuard(ctx context.Context, lifecycle RetainedGuardLifecycle, request GuardTransitionRequest) (GuardReceipt, error) {
	if isNilInterface(lifecycle) {
		return GuardReceipt{}, fmt.Errorf("%w: retained guard lifecycle is nil", ErrMissingCapability)
	}
	baseline := request.Clone()
	if err := baseline.Validate(ctx); err != nil {
		return GuardReceipt{}, err
	}
	var fence writerFenceSnapshot
	var hasFence bool
	if !isNilInterface(baseline.SourceFence) {
		var err error
		fence, err = snapshotWriterFence(ctx, baseline.SourceFence)
		if err != nil {
			return GuardReceipt{}, err
		}
		hasFence = true
	}
	receipt, providerErr := lifecycle.ReleaseOrTransfer(ctx, baseline.Clone())
	if hasFence {
		if err := providerFenceResult(ctx, "transitioning retained guard", providerErr, fence); err != nil {
			return GuardReceipt{}, err
		}
	} else if providerErr != nil {
		return GuardReceipt{}, fmt.Errorf("transitioning retained guard: %w", providerErr)
	}
	receipt = receipt.Clone()
	if err := receipt.Validate(); err != nil {
		return GuardReceipt{}, err
	}
	if !guardTransitionReceiptMatches(receipt, baseline) {
		return GuardReceipt{}, ErrInvalidGuard
	}
	return receipt.Clone(), nil
}

func guardTransitionReceiptMatches(receipt GuardReceipt, request GuardTransitionRequest) bool {
	current := request.Current
	if receipt.Provider != current.Provider || receipt.Component != current.Component || receipt.PhysicalIdentity != current.PhysicalIdentity || !receipt.Classes.Equal(current.Classes) || receipt.SemanticContractVersion != current.SemanticContractVersion {
		return false
	}
	if request.Release {
		return receipt.Attempt == current.Attempt && receipt.Generation == current.Generation && receipt.Role == current.Role && receipt.TransferState == current.TransferState && receipt.TransferParticipant == current.TransferParticipant && receipt.TransferDestinationIdentity == current.TransferDestinationIdentity && receipt.TransferReceiptKind == current.TransferReceiptKind && participantReceiptsEqual(receipt.ActiveProof, current.ActiveProof)
	}
	target := request.Target
	return target != nil && receipt.Attempt == target.Decision.Attempt && receipt.Generation == target.Decision.Generation && receipt.Role == GuardRoleTransfer && receipt.TransferState == target.State && receipt.TransferParticipant == target.Participant && receipt.TransferDestinationIdentity == target.DestinationIdentity && receipt.TransferReceiptKind == target.ExpectedReceiptKind && participantReceiptsEqual(receipt.ActiveProof, target.ActiveProof)
}

func participantReceiptsEqual(left, right *ParticipantReceipt) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.Equal(*right)
}

// CommitDecision is the fsynced global authority transition required for activation and commit.
type CommitDecision struct {
	Attempt    AttemptID
	Generation Generation
	Decided    bool
}

// NewCommitDecision creates a decided authority token for a durable attempt.
func NewCommitDecision(attempt AttemptID, generation Generation) (CommitDecision, error) {
	decision := CommitDecision{Attempt: attempt, Generation: generation, Decided: true}
	if err := decision.Validate(); err != nil {
		return CommitDecision{}, err
	}
	return decision, nil
}

// Validate verifies that a decision is durable and tied to an attempt.
func (d CommitDecision) Validate() error {
	if strings.TrimSpace(string(d.Attempt)) == "" || !d.Generation.Valid() || !d.Decided {
		return ErrCommitNotDecided
	}
	if err := validateSecretFree("commit decision attempt", string(d.Attempt)); err != nil {
		return err
	}
	return nil
}

// ParticipantReceiptKind is the closed purpose of a provider receipt.
type ParticipantReceiptKind uint8

const (
	// ParticipantReceiptBindingActivation proves one binding activation.
	ParticipantReceiptBindingActivation ParticipantReceiptKind = iota + 1
	// ParticipantReceiptWorkMigration proves one Work participant migration.
	ParticipantReceiptWorkMigration
)

// Valid reports whether kind belongs to the closed receipt-purpose set.
func (k ParticipantReceiptKind) Valid() bool {
	return k == ParticipantReceiptBindingActivation || k == ParticipantReceiptWorkMigration
}

// ParticipantReceipt is a versioned provider receipt for one binding activation
// or Work participant migration. Work receipts carry their complete preparation;
// activation receipts reject Work-only fields.
type ParticipantReceipt struct {
	Version            uint16
	Kind               ParticipantReceiptKind
	Attempt            AttemptID
	Generation         Generation
	Participant        string
	DescriptorIdentity BindingIdentity
	ReceiptID          string
	Preparation        WorkPreparationIdentity
	PreparedReceipt    string
}

// Clone returns a detached receipt value.
func (r ParticipantReceipt) Clone() ParticipantReceipt {
	out := r
	out.Preparation = r.Preparation.Clone()
	return out
}

// Equal reports whether every receipt fact, including Work provenance, matches.
func (r ParticipantReceipt) Equal(other ParticipantReceipt) bool {
	return r.Version == other.Version && r.Kind == other.Kind && r.Attempt == other.Attempt && r.Generation == other.Generation && r.Participant == other.Participant && r.DescriptorIdentity == other.DescriptorIdentity && r.ReceiptID == other.ReceiptID && r.Preparation.Equal(other.Preparation) && r.PreparedReceipt == other.PreparedReceipt
}

// Validate verifies receipt identity without exposing opaque provider state.
func (r ParticipantReceipt) Validate() error {
	if r.Version != 1 || !r.Kind.Valid() || strings.TrimSpace(string(r.Attempt)) == "" || !r.Generation.Valid() || strings.TrimSpace(r.Participant) == "" || strings.TrimSpace(string(r.DescriptorIdentity)) == "" || strings.TrimSpace(r.ReceiptID) == "" {
		return errors.New("invalid storage participant receipt")
	}
	if err := validateSecretFree("participant receipt attempt", string(r.Attempt)); err != nil {
		return err
	}
	if err := validateCanonicalSHA256Digest("participant receipt descriptor identity", string(r.DescriptorIdentity)); err != nil {
		return err
	}
	switch r.Kind {
	case ParticipantReceiptBindingActivation:
		if !r.Preparation.IsZero() || r.PreparedReceipt != "" {
			return errors.New("binding activation receipt carries work provenance")
		}
	case ParticipantReceiptWorkMigration:
		if err := r.Preparation.Validate(); err != nil {
			return err
		}
		if strings.TrimSpace(r.PreparedReceipt) == "" || r.Attempt != r.Preparation.Attempt || r.Generation != r.Preparation.Generation || r.Participant != r.Preparation.Participant.Key() || r.DescriptorIdentity != r.Preparation.DestinationIdentity {
			return ErrInvalidWorkParticipant
		}
	}
	for field, value := range map[string]string{
		"participant receipt participant":         r.Participant,
		"participant receipt descriptor identity": string(r.DescriptorIdentity),
		"participant receipt":                     r.ReceiptID,
	} {
		if err := validateSecretFree(field, value); err != nil {
			return err
		}
	}
	if r.Kind == ParticipantReceiptWorkMigration {
		if err := validateSecretFree("work prepared receipt", r.PreparedReceipt); err != nil {
			return err
		}
	}
	return nil
}

// GuardedActivationAttestation is an opaque, participant-bound proof that every
// planned retained guard was verified for one decided binding activation.
// It can only be created by AttestGuardedActivation.
type GuardedActivationAttestation struct {
	version             uint16
	attempt             AttemptID
	generation          Generation
	participant         string
	destinationIdentity BindingIdentity
	receiptIDs          []string
}

// Clone returns a detached guarded-activation attestation.
func (a GuardedActivationAttestation) Clone() GuardedActivationAttestation {
	out := a
	out.receiptIDs = append([]string(nil), a.receiptIDs...)
	return out
}

func (a GuardedActivationAttestation) validate(decision CommitDecision, participant string, destinationIdentity BindingIdentity) error {
	if err := decision.Validate(); err != nil {
		return err
	}
	if err := validateCanonicalSHA256Digest("guarded activation destination identity", string(destinationIdentity)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidGuard, err)
	}
	if a.version != 1 || a.attempt != decision.Attempt || a.generation != decision.Generation || a.participant != participant || a.destinationIdentity != destinationIdentity {
		return ErrInvalidGuard
	}
	seen := make(map[string]struct{}, len(a.receiptIDs))
	for _, receiptID := range a.receiptIDs {
		if strings.TrimSpace(receiptID) == "" {
			return ErrInvalidGuard
		}
		if err := validateSecretFree("guarded activation receipt", receiptID); err != nil {
			return err
		}
		if _, duplicate := seen[receiptID]; duplicate {
			return ErrInvalidGuard
		}
		seen[receiptID] = struct{}{}
	}
	return nil
}

// InPlaceGuardedActivationAuthority proves that an empty retained-guard plan
// is safe because the source and destination retain the same fenced physical
// components through a no-copy adoption. Its fields are deliberately private
// so only the validating constructor can mint it.
type InPlaceGuardedActivationAuthority struct {
	version             uint16
	attempt             AttemptID
	generation          Generation
	participant         string
	sourceIdentity      BindingIdentity
	destinationIdentity BindingIdentity
	fence               writerFenceSnapshot
}

// NewInPlaceGuardedActivationAuthority proves a same-component adoption keeps
// one populated binding fenced through activation without retained sources.
func NewInPlaceGuardedActivationAuthority(ctx context.Context, decision CommitDecision, participant string, source, destination Descriptor, fence WriterFence) (InPlaceGuardedActivationAuthority, error) {
	if err := decision.Validate(); err != nil {
		return InPlaceGuardedActivationAuthority{}, err
	}
	if strings.TrimSpace(participant) == "" {
		return InPlaceGuardedActivationAuthority{}, ErrInvalidGuard
	}
	if err := validateSecretFree("in-place activation participant", participant); err != nil {
		return InPlaceGuardedActivationAuthority{}, err
	}
	if err := source.Validate(); err != nil {
		return InPlaceGuardedActivationAuthority{}, fmt.Errorf("%w: %w", ErrInvalidGuard, err)
	}
	if err := destination.Validate(); err != nil {
		return InPlaceGuardedActivationAuthority{}, fmt.Errorf("%w: %w", ErrInvalidGuard, err)
	}
	sourceIdentity, err := source.Identity()
	if err != nil {
		return InPlaceGuardedActivationAuthority{}, fmt.Errorf("%w: %w", ErrInvalidGuard, err)
	}
	destinationIdentity, err := destination.Identity()
	if err != nil {
		return InPlaceGuardedActivationAuthority{}, fmt.Errorf("%w: %w", ErrInvalidGuard, err)
	}
	if !sameNoCopyAdoptionDescriptors(source, destination) {
		return InPlaceGuardedActivationAuthority{}, ErrInvalidGuard
	}
	if err := validateWorkFence(ctx, fence, source, decision.Generation, FenceRolePopulatedDestination); err != nil {
		return InPlaceGuardedActivationAuthority{}, err
	}
	if err := validateWorkFence(ctx, fence, destination, decision.Generation, FenceRolePopulatedDestination); err != nil {
		return InPlaceGuardedActivationAuthority{}, err
	}
	snapshot, err := snapshotWriterFence(ctx, fence)
	if err != nil {
		return InPlaceGuardedActivationAuthority{}, err
	}
	return InPlaceGuardedActivationAuthority{
		version:             1,
		attempt:             decision.Attempt,
		generation:          decision.Generation,
		participant:         participant,
		sourceIdentity:      sourceIdentity,
		destinationIdentity: destinationIdentity,
		fence:               snapshot,
	}, nil
}

func (a InPlaceGuardedActivationAuthority) validate(ctx context.Context, decision CommitDecision, participant string, destinationIdentity BindingIdentity) error {
	if a.version != 1 || a.attempt != decision.Attempt || a.generation != decision.Generation || a.participant != participant || a.sourceIdentity == "" || a.destinationIdentity != destinationIdentity {
		return ErrInvalidGuard
	}
	if err := validateSecretFree("in-place activation participant", a.participant); err != nil {
		return err
	}
	if err := validateCanonicalSHA256Digest("in-place activation source identity", string(a.sourceIdentity)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidGuard, err)
	}
	if err := validateCanonicalSHA256Digest("in-place activation destination identity", string(a.destinationIdentity)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidGuard, err)
	}
	if err := checkWriterFenceSnapshots(ctx, a.fence); err != nil {
		return err
	}
	return nil
}

func sameNoCopyAdoptionDescriptors(source, destination Descriptor) bool {
	if source.Version != destination.Version || source.SemanticContractVersion != destination.SemanticContractVersion || source.Provider != destination.Provider || source.ImplementationVersion != destination.ImplementationVersion || !source.Capabilities.Equal(destination.Capabilities) || source.ConfigRefDigest != destination.ConfigRefDigest || source.RetainedSource != nil || destination.RetainedSource != nil || len(source.Components) != len(destination.Components) {
		return false
	}
	byID := make(map[ComponentID]ComponentDescriptor, len(destination.Components))
	for _, component := range destination.Components {
		byID[component.ID] = component
	}
	for _, component := range source.Components {
		other, found := byID[component.ID]
		if !found || component.Locator != other.Locator || component.PhysicalIdentity != other.PhysicalIdentity || !component.Classes.Equal(other.Classes) || component.Format != other.Format || component.SchemaVersion != other.SchemaVersion || component.ABIVersion != other.ABIVersion || component.Marker.Name != other.Marker.Name || (component.Marker.Present && !other.Marker.Present) {
			return false
		}
	}
	return true
}

// AttestGuardedActivation verifies every planned retained guard and returns an
// opaque attestation bound to exactly one activation participant and decision.
// An exact empty plan and receipt set additionally requires one in-place
// authority proving the source and destination are the same no-copy fenced
// components.
func AttestGuardedActivation(ctx context.Context, resolver RetainedGuardLifecycleResolver, decision CommitDecision, participant string, destinationIdentity BindingIdentity, plan []FencedGuardInstallRequest, receipts []GuardReceipt, inPlaceAuthorities ...InPlaceGuardedActivationAuthority) (GuardedActivationAttestation, error) {
	if err := decision.Validate(); err != nil {
		return GuardedActivationAttestation{}, err
	}
	if strings.TrimSpace(participant) == "" || strings.TrimSpace(string(destinationIdentity)) == "" {
		return GuardedActivationAttestation{}, ErrInvalidGuard
	}
	if err := validateSecretFree("activation participant", participant); err != nil {
		return GuardedActivationAttestation{}, err
	}
	if err := validateCanonicalSHA256Digest("activation destination identity", string(destinationIdentity)); err != nil {
		return GuardedActivationAttestation{}, fmt.Errorf("%w: %w", ErrInvalidGuard, err)
	}
	if len(plan) == 0 && len(receipts) == 0 {
		if len(inPlaceAuthorities) != 1 {
			return GuardedActivationAttestation{}, ErrInvalidGuard
		}
		if err := inPlaceAuthorities[0].validate(ctx, decision, participant, destinationIdentity); err != nil {
			return GuardedActivationAttestation{}, err
		}
		attestation := GuardedActivationAttestation{
			version:             1,
			attempt:             decision.Attempt,
			generation:          decision.Generation,
			participant:         participant,
			destinationIdentity: destinationIdentity,
		}
		if err := attestation.validate(decision, participant, destinationIdentity); err != nil {
			return GuardedActivationAttestation{}, err
		}
		return attestation, nil
	}
	if len(inPlaceAuthorities) != 0 {
		return GuardedActivationAttestation{}, ErrInvalidGuard
	}
	if len(plan) == 0 || len(receipts) == 0 {
		return GuardedActivationAttestation{}, ErrInvalidGuard
	}
	if isNilInterface(resolver) {
		return GuardedActivationAttestation{}, fmt.Errorf("%w: retained guard lifecycle resolver is nil", ErrMissingCapability)
	}
	baselines := cloneFencedGuardInstallRequests(plan)
	receiptBaselines := cloneGuardReceipts(receipts)
	if err := validateActivationGuards(ctx, decision, baselines, receiptBaselines); err != nil {
		return GuardedActivationAttestation{}, err
	}
	lifecycles, err := resolveRetainedGuardLifecycles(ctx, resolver, baselines)
	if err != nil {
		return GuardedActivationAttestation{}, err
	}
	byReceiptKey := make(map[guardReceiptKey]FencedGuardInstallRequest, len(baselines))
	for _, request := range baselines {
		byReceiptKey[newGuardReceiptKey(request.Source.Provider, request.Component, request.PhysicalIdentity, request.Source.Classes, request.Source.SemanticContractVersion, request.Role)] = request.Clone()
	}
	receiptIDs := make([]string, 0, len(receiptBaselines))
	seenReceiptIDs := make(map[string]struct{}, len(receiptBaselines))
	for _, receipt := range receiptBaselines {
		if receipt.Role != GuardRoleDenyWrite {
			return GuardedActivationAttestation{}, ErrInvalidGuard
		}
		request, found := byReceiptKey[newGuardReceiptKey(receipt.Provider, receipt.Component, receipt.PhysicalIdentity, receipt.Classes, receipt.SemanticContractVersion, receipt.Role)]
		if !found {
			return GuardedActivationAttestation{}, ErrInvalidGuard
		}
		verifyRequest := FencedGuardVerificationRequest{Receipt: receipt.Clone(), SourceFence: request.SourceFence}
		if err := verifyRequest.Validate(ctx); err != nil {
			return GuardedActivationAttestation{}, err
		}
		fence, err := snapshotWriterFence(ctx, verifyRequest.SourceFence)
		if err != nil {
			return GuardedActivationAttestation{}, err
		}
		lifecycle := lifecycles[receipt.Provider]
		providerErr := lifecycle.Verify(ctx, verifyRequest.Clone())
		if err := providerFenceResult(ctx, "verifying guarded activation receipt", providerErr, fence); err != nil {
			return GuardedActivationAttestation{}, err
		}
		if _, duplicate := seenReceiptIDs[receipt.ReceiptID]; duplicate {
			return GuardedActivationAttestation{}, ErrInvalidGuard
		}
		seenReceiptIDs[receipt.ReceiptID] = struct{}{}
		receiptIDs = append(receiptIDs, receipt.ReceiptID)
	}
	sort.Strings(receiptIDs)
	attestation := GuardedActivationAttestation{
		version:             1,
		attempt:             decision.Attempt,
		generation:          decision.Generation,
		participant:         participant,
		destinationIdentity: destinationIdentity,
		receiptIDs:          receiptIDs,
	}
	if err := attestation.validate(decision, participant, destinationIdentity); err != nil {
		return GuardedActivationAttestation{}, err
	}
	return attestation, nil
}

// BindingActivationRequest activates an already witnessed binding only after COMMIT_DECIDED.
type BindingActivationRequest struct {
	Decision         CommitDecision
	Participant      string
	Destination      Descriptor
	DesiredIdentity  BindingIdentity
	GuardAttestation GuardedActivationAttestation
	DestinationFence WriterFence
}

// Clone returns a detached binding activation request for a provider boundary.
func (r BindingActivationRequest) Clone() BindingActivationRequest {
	out := r
	out.Destination = r.Destination.Clone()
	out.GuardAttestation = r.GuardAttestation.Clone()
	return out
}

// Validate verifies activation is legal only after the global decision.
func (r BindingActivationRequest) Validate() error {
	return r.validate(context.Background())
}

func (r BindingActivationRequest) validate(ctx context.Context) error {
	if err := r.Decision.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(r.Participant) == "" {
		return errors.New("activation participant is required")
	}
	if err := validateSecretFree("activation participant", r.Participant); err != nil {
		return err
	}
	if err := r.Destination.Validate(); err != nil {
		return err
	}
	if !r.Destination.Capabilities.GuardedActivation {
		return ErrGuardedActivationUnavailable
	}
	identity, err := r.Destination.Identity()
	if err != nil {
		return err
	}
	if r.DesiredIdentity == "" || r.DesiredIdentity != identity {
		return fmt.Errorf("activation desired identity differs from destination descriptor")
	}
	if err := validateCanonicalSHA256Digest("activation desired identity", string(r.DesiredIdentity)); err != nil {
		return err
	}
	if err := r.GuardAttestation.validate(r.Decision, r.Participant, identity); err != nil {
		return err
	}
	return validateWorkFence(ctx, r.DestinationFence, r.Destination, r.Decision.Generation, FenceRolePopulatedDestination, FenceRoleNewDestinationReservation)
}

// BindingActivationResumeRequest discovers an activation completed before its receipt fsync.
type BindingActivationResumeRequest struct {
	Decision         CommitDecision
	Participant      string
	Destination      Descriptor
	DesiredIdentity  BindingIdentity
	GuardAttestation GuardedActivationAttestation
	DestinationFence WriterFence
}

// Clone returns a detached binding activation recovery request for a provider boundary.
func (r BindingActivationResumeRequest) Clone() BindingActivationResumeRequest {
	out := r
	out.Destination = r.Destination.Clone()
	out.GuardAttestation = r.GuardAttestation.Clone()
	return out
}

// Validate verifies resume is pinned to the same decided generation as activation.
func (r BindingActivationResumeRequest) Validate() error {
	return r.validate(context.Background())
}

func (r BindingActivationResumeRequest) validate(ctx context.Context) error {
	return BindingActivationRequest(r).validate(ctx)
}

func cloneGuardReceipts(receipts []GuardReceipt) []GuardReceipt {
	cloned := make([]GuardReceipt, len(receipts))
	for index, receipt := range receipts {
		cloned[index] = receipt.Clone()
	}
	return cloned
}

func validateActivationGuards(ctx context.Context, decision CommitDecision, plan []FencedGuardInstallRequest, receipts []GuardReceipt) error {
	if len(plan) == 0 && len(receipts) == 0 {
		return nil
	}
	if len(plan) != len(receipts) {
		return ErrInvalidGuard
	}
	expected := make(map[guardReceiptKey]struct{}, len(plan))
	for _, request := range plan {
		if err := request.Validate(ctx); err != nil || request.Attempt != decision.Attempt || request.Generation != decision.Generation {
			return ErrInvalidGuard
		}
		key := newGuardReceiptKey(request.Source.Provider, request.Component, request.PhysicalIdentity, request.Source.Classes, request.Source.SemanticContractVersion, request.Role)
		if _, exists := expected[key]; exists {
			return ErrInvalidGuard
		}
		expected[key] = struct{}{}
	}
	for _, receipt := range receipts {
		if err := receipt.Validate(); err != nil || receipt.Attempt != decision.Attempt || receipt.Generation != decision.Generation {
			return ErrInvalidGuard
		}
		key := newGuardReceiptKey(receipt.Provider, receipt.Component, receipt.PhysicalIdentity, receipt.Classes, receipt.SemanticContractVersion, receipt.Role)
		if _, exists := expected[key]; !exists {
			return ErrInvalidGuard
		}
		delete(expected, key)
	}
	if len(expected) != 0 {
		return ErrInvalidGuard
	}
	return nil
}

type guardReceiptKey struct {
	provider  ProviderID
	component ComponentID
	identity  PhysicalIdentity
	classes   ClassSet
	contract  ContractVersion
	role      GuardRole
}

func newGuardReceiptKey(provider ProviderID, component ComponentID, identity PhysicalIdentity, classes ClassSet, contract ContractVersion, role GuardRole) guardReceiptKey {
	return guardReceiptKey{
		provider:  provider,
		component: component,
		identity:  identity,
		classes:   classes,
		contract:  contract,
		role:      role,
	}
}

// BindingMigrationLifecycle owns provider-specific activation of a witnessed binding generation.
type BindingMigrationLifecycle interface {
	Activate(context.Context, BindingActivationRequest) (ParticipantReceipt, error)
	ResumeActivation(context.Context, BindingActivationResumeRequest) (ParticipantReceipt, error)
}

// ActivateBinding validates a decided activation and validates the returned receipt.
func ActivateBinding(ctx context.Context, lifecycle BindingMigrationLifecycle, request BindingActivationRequest) (ParticipantReceipt, error) {
	if isNilInterface(lifecycle) {
		return ParticipantReceipt{}, fmt.Errorf("%w: binding migration lifecycle is nil", ErrMissingCapability)
	}
	baseline := request.Clone()
	if err := baseline.validate(ctx); err != nil {
		return ParticipantReceipt{}, err
	}
	fence, err := snapshotWriterFence(ctx, baseline.DestinationFence)
	if err != nil {
		return ParticipantReceipt{}, err
	}
	receipt, providerErr := lifecycle.Activate(ctx, baseline.Clone())
	if err := providerFenceResult(ctx, "activating storage binding", providerErr, fence); err != nil {
		return ParticipantReceipt{}, err
	}
	receipt = receipt.Clone()
	if err := receipt.Validate(); err != nil {
		return ParticipantReceipt{}, err
	}
	if receipt.Kind != ParticipantReceiptBindingActivation || receipt.Attempt != baseline.Decision.Attempt || receipt.Generation != baseline.Decision.Generation || receipt.Participant != baseline.Participant || receipt.DescriptorIdentity != baseline.DesiredIdentity {
		return ParticipantReceipt{}, errors.New("activation receipt does not match decided generation")
	}
	return receipt, nil
}

// RecoverBindingActivation discovers an already-active generation after receipt loss.
func RecoverBindingActivation(ctx context.Context, lifecycle BindingMigrationLifecycle, request BindingActivationResumeRequest) (ParticipantReceipt, error) {
	if isNilInterface(lifecycle) {
		return ParticipantReceipt{}, fmt.Errorf("%w: binding migration lifecycle is nil", ErrMissingCapability)
	}
	baseline := request.Clone()
	if err := baseline.validate(ctx); err != nil {
		return ParticipantReceipt{}, err
	}
	fence, err := snapshotWriterFence(ctx, baseline.DestinationFence)
	if err != nil {
		return ParticipantReceipt{}, err
	}
	receipt, providerErr := lifecycle.ResumeActivation(ctx, baseline.Clone())
	if err := providerFenceResult(ctx, "resuming storage binding activation", providerErr, fence); err != nil {
		return ParticipantReceipt{}, err
	}
	receipt = receipt.Clone()
	if err := receipt.Validate(); err != nil {
		return ParticipantReceipt{}, err
	}
	if receipt.Kind != ParticipantReceiptBindingActivation || receipt.Attempt != baseline.Decision.Attempt || receipt.Generation != baseline.Decision.Generation || receipt.Participant != baseline.Participant || receipt.DescriptorIdentity != baseline.DesiredIdentity {
		return ParticipantReceipt{}, errors.New("activation recovery receipt does not match decided generation")
	}
	return receipt, nil
}

// WorkMigrationDirection is the closed direction of a Work migration.
type WorkMigrationDirection uint8

const (
	// WorkMigrationForward moves the pinned source into the desired destination.
	WorkMigrationForward WorkMigrationDirection = iota + 1
	// WorkMigrationReverse reconciles the current authority back into a retained destination.
	WorkMigrationReverse
)

// Valid reports whether direction belongs to the closed work-migration direction set.
func (d WorkMigrationDirection) Valid() bool {
	return d == WorkMigrationForward || d == WorkMigrationReverse
}

// WorkWorkspaceMember is one semantic scope sharing a physical Work workspace.
type WorkWorkspaceMember struct {
	Scope            WorkScope
	Prefix           string
	ConfigContext    ConfigRefDigest
	Suspended        bool
	ConfigOrder      int
	Provider         ProviderID
	Component        ComponentID
	PhysicalIdentity PhysicalIdentity
}

// Validate verifies that a semantic member can be grouped by stable physical identity.
func (m WorkWorkspaceMember) Validate() error {
	if (!m.Scope.IsHQ() && m.Scope.String() == "rig:") || !validWorkPrefix(m.Prefix) || strings.TrimSpace(string(m.ConfigContext)) == "" || strings.TrimSpace(string(m.Provider)) == "" || strings.TrimSpace(string(m.Component)) == "" || strings.TrimSpace(string(m.PhysicalIdentity)) == "" {
		return ErrInvalidWorkParticipant
	}
	if err := validateIdentifier("work provider ID", string(m.Provider)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidWorkParticipant, err)
	}
	if err := validateIdentifier("work component ID", string(m.Component)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidWorkParticipant, err)
	}
	if err := validateCanonicalSHA256Digest("work config context", string(m.ConfigContext)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidWorkParticipant, err)
	}
	if err := validateSecretFree("work physical identity", string(m.PhysicalIdentity)); err != nil {
		return err
	}
	return nil
}

// WorkWorkspaceParticipant owns one physical Work workspace and every semantic member scope sharing it.
type WorkWorkspaceParticipant struct {
	Provider         ProviderID
	Component        ComponentID
	PhysicalIdentity PhysicalIdentity
	Members          []WorkWorkspaceMember
}

type workParticipantIdentity struct {
	provider  ProviderID
	component ComponentID
	physical  PhysicalIdentity
}

func (p WorkWorkspaceParticipant) identity() workParticipantIdentity {
	return workParticipantIdentity{provider: p.Provider, component: p.Component, physical: p.PhysicalIdentity}
}

// NewWorkWorkspaceParticipant validates and defensively copies one grouped physical workspace.
func NewWorkWorkspaceParticipant(provider ProviderID, component ComponentID, identity PhysicalIdentity, members []WorkWorkspaceMember) (WorkWorkspaceParticipant, error) {
	participant := WorkWorkspaceParticipant{Provider: provider, Component: component, PhysicalIdentity: identity, Members: append([]WorkWorkspaceMember(nil), members...)}
	if err := participant.Validate(); err != nil {
		return WorkWorkspaceParticipant{}, err
	}
	return participant, nil
}

// Clone returns a detached participant value.
func (p WorkWorkspaceParticipant) Clone() WorkWorkspaceParticipant {
	out := p
	out.Members = append([]WorkWorkspaceMember(nil), p.Members...)
	return out
}

// Key returns the stable physical identity key used to deduplicate a participant.
func (p WorkWorkspaceParticipant) Key() string {
	return string(p.Provider) + "\x00" + string(p.Component) + "\x00" + string(p.PhysicalIdentity)
}

// Equal reports whether two participants preserve the same physical workspace and every member scope fact.
func (p WorkWorkspaceParticipant) Equal(other WorkWorkspaceParticipant) bool {
	if p.Provider != other.Provider || p.Component != other.Component || p.PhysicalIdentity != other.PhysicalIdentity || len(p.Members) != len(other.Members) {
		return false
	}
	byScope := make(map[WorkScope]WorkWorkspaceMember, len(other.Members))
	for _, member := range other.Members {
		if _, exists := byScope[member.Scope]; exists {
			return false
		}
		byScope[member.Scope] = member
	}
	for _, member := range p.Members {
		otherMember, found := byScope[member.Scope]
		if !found || member != otherMember {
			return false
		}
	}
	return true
}

// Validate verifies that every member belongs to exactly the participant's physical workspace.
func (p WorkWorkspaceParticipant) Validate() error {
	if strings.TrimSpace(string(p.Provider)) == "" || strings.TrimSpace(string(p.Component)) == "" || strings.TrimSpace(string(p.PhysicalIdentity)) == "" || len(p.Members) == 0 {
		return ErrInvalidWorkParticipant
	}
	if err := validateIdentifier("work participant provider ID", string(p.Provider)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidWorkParticipant, err)
	}
	if err := validateIdentifier("work participant component ID", string(p.Component)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidWorkParticipant, err)
	}
	if err := validateSecretFree("work participant physical identity", string(p.PhysicalIdentity)); err != nil {
		return err
	}
	seenScopes := make(map[WorkScope]struct{}, len(p.Members))
	seenOrder := make(map[int]struct{}, len(p.Members))
	for _, member := range p.Members {
		if err := member.Validate(); err != nil {
			return err
		}
		if member.Provider != p.Provider || member.Component != p.Component || member.PhysicalIdentity != p.PhysicalIdentity {
			return fmt.Errorf("%w: member physical identity differs", ErrInvalidWorkParticipant)
		}
		if _, exists := seenScopes[member.Scope]; exists {
			return fmt.Errorf("%w: duplicate member scope %s", ErrInvalidWorkParticipant, member.Scope)
		}
		if _, exists := seenOrder[member.ConfigOrder]; exists {
			return fmt.Errorf("%w: duplicate member config order", ErrInvalidWorkParticipant)
		}
		seenScopes[member.Scope] = struct{}{}
		seenOrder[member.ConfigOrder] = struct{}{}
	}
	return nil
}

// GroupWorkParticipants groups scoped Work members by one physical workspace identity.
func GroupWorkParticipants(members []WorkWorkspaceMember) ([]WorkWorkspaceParticipant, error) {
	if err := validateWorkMembersTopology(members); err != nil {
		return nil, err
	}
	groups := make(map[workParticipantIdentity][]WorkWorkspaceMember)
	order := make([]workParticipantIdentity, 0, len(members))
	for _, member := range members {
		key := workParticipantIdentity{provider: member.Provider, component: member.Component, physical: member.PhysicalIdentity}
		if _, exists := groups[key]; !exists {
			order = append(order, key)
		}
		groups[key] = append(groups[key], member)
	}
	participants := make([]WorkWorkspaceParticipant, 0, len(order))
	for _, key := range order {
		membersForWorkspace := groups[key]
		sort.SliceStable(membersForWorkspace, func(i, j int) bool { return membersForWorkspace[i].ConfigOrder < membersForWorkspace[j].ConfigOrder })
		first := membersForWorkspace[0]
		participant, err := NewWorkWorkspaceParticipant(first.Provider, first.Component, first.PhysicalIdentity, membersForWorkspace)
		if err != nil {
			return nil, err
		}
		participants = append(participants, participant)
	}
	sort.SliceStable(participants, func(i, j int) bool { return participants[i].Key() < participants[j].Key() })
	return participants, nil
}

func validateWorkMembersTopology(members []WorkWorkspaceMember) error {
	seenScopes := make(map[WorkScope]struct{}, len(members))
	seenConfigOrders := make(map[int]struct{}, len(members))
	hqCount := 0
	for _, member := range members {
		if err := member.Validate(); err != nil {
			return err
		}
		if _, duplicate := seenScopes[member.Scope]; duplicate {
			return fmt.Errorf("%w: duplicate scope %s", ErrInvalidWorkParticipant, member.Scope)
		}
		seenScopes[member.Scope] = struct{}{}
		if _, duplicate := seenConfigOrders[member.ConfigOrder]; duplicate {
			return fmt.Errorf("%w: duplicate config order", ErrInvalidWorkParticipant)
		}
		seenConfigOrders[member.ConfigOrder] = struct{}{}
		if member.Scope.IsHQ() {
			hqCount++
		}
	}
	if hqCount != 1 {
		return fmt.Errorf("%w: expected exactly one HQ scope", ErrInvalidWorkParticipant)
	}
	return nil
}

// WorkPrepareRequest supplies one grouped physical Work workspace to a provider migration lifecycle.
type WorkPrepareRequest struct {
	Attempt          AttemptID
	Generation       Generation
	Direction        WorkMigrationDirection
	Participant      WorkWorkspaceParticipant
	Source           Descriptor
	Destination      Descriptor
	SourceFence      WriterFence
	DestinationFence WriterFence
	WitnessVersion   string
	ConfigDigest     ConfigRefDigest
	PriorReceipt     *ParticipantReceipt
}

// Clone returns a detached prepare request for a provider boundary.
func (r WorkPrepareRequest) Clone() WorkPrepareRequest {
	out := r
	out.Participant = r.Participant.Clone()
	out.Source = r.Source.Clone()
	out.Destination = r.Destination.Clone()
	if r.PriorReceipt != nil {
		receipt := r.PriorReceipt.Clone()
		out.PriorReceipt = &receipt
	}
	return out
}

// WorkPreparationIdentity is the canonical, immutable identity of one Work
// preparation. It binds source and destination descriptors, direction,
// participant, witness contract, and configuration context through every later
// prepare/verify/commit/resume value.
type WorkPreparationIdentity struct {
	Version             uint16
	Attempt             AttemptID
	Generation          Generation
	Direction           WorkMigrationDirection
	Participant         WorkWorkspaceParticipant
	SourceIdentity      BindingIdentity
	DestinationIdentity BindingIdentity
	WitnessVersion      string
	ConfigDigest        ConfigRefDigest
}

// Clone returns a detached preparation identity.
func (p WorkPreparationIdentity) Clone() WorkPreparationIdentity {
	out := p
	out.Participant = p.Participant.Clone()
	return out
}

// Equal reports whether all canonical preparation facts match.
func (p WorkPreparationIdentity) Equal(other WorkPreparationIdentity) bool {
	return p.Version == other.Version && p.Attempt == other.Attempt && p.Generation == other.Generation && p.Direction == other.Direction && p.Participant.Equal(other.Participant) && p.SourceIdentity == other.SourceIdentity && p.DestinationIdentity == other.DestinationIdentity && p.WitnessVersion == other.WitnessVersion && p.ConfigDigest == other.ConfigDigest
}

// IsZero reports whether no Work preparation facts are present.
func (p WorkPreparationIdentity) IsZero() bool {
	return p.Equal(WorkPreparationIdentity{})
}

// Validate verifies that a preparation identity is complete and participant-scoped.
func (p WorkPreparationIdentity) Validate() error {
	if p.Version != 1 || strings.TrimSpace(string(p.Attempt)) == "" || !p.Generation.Valid() || !p.Direction.Valid() || strings.TrimSpace(string(p.SourceIdentity)) == "" || strings.TrimSpace(string(p.DestinationIdentity)) == "" || strings.TrimSpace(p.WitnessVersion) == "" || strings.TrimSpace(string(p.ConfigDigest)) == "" {
		return ErrInvalidWorkParticipant
	}
	if err := validateSecretFree("work preparation attempt", string(p.Attempt)); err != nil {
		return err
	}
	if err := validateSecretFree("work preparation witness version", p.WitnessVersion); err != nil {
		return err
	}
	for field, value := range map[string]string{
		"work preparation source identity":      string(p.SourceIdentity),
		"work preparation destination identity": string(p.DestinationIdentity),
		"work preparation config digest":        string(p.ConfigDigest),
	} {
		if err := validateCanonicalSHA256Digest(field, value); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidWorkParticipant, err)
		}
	}
	return p.Participant.Validate()
}

// PreparationIdentity returns the canonical identity that later provider values
// must preserve for this preparation request.
func (r WorkPrepareRequest) PreparationIdentity() (WorkPreparationIdentity, error) {
	if strings.TrimSpace(string(r.Attempt)) == "" || !r.Generation.Valid() || !r.Direction.Valid() || strings.TrimSpace(r.WitnessVersion) == "" || strings.TrimSpace(string(r.ConfigDigest)) == "" {
		return WorkPreparationIdentity{}, ErrInvalidWorkParticipant
	}
	if err := validateSecretFree("work prepare attempt", string(r.Attempt)); err != nil {
		return WorkPreparationIdentity{}, err
	}
	if err := validateSecretFree("work prepare witness version", r.WitnessVersion); err != nil {
		return WorkPreparationIdentity{}, err
	}
	if err := r.Participant.Validate(); err != nil {
		return WorkPreparationIdentity{}, err
	}
	if err := r.Source.Validate(); err != nil {
		return WorkPreparationIdentity{}, err
	}
	if err := r.Destination.Validate(); err != nil {
		return WorkPreparationIdentity{}, err
	}
	sourceIdentity, err := r.Source.Identity()
	if err != nil {
		return WorkPreparationIdentity{}, err
	}
	destinationIdentity, err := r.Destination.Identity()
	if err != nil {
		return WorkPreparationIdentity{}, err
	}
	identity := WorkPreparationIdentity{
		Version:             1,
		Attempt:             r.Attempt,
		Generation:          r.Generation,
		Direction:           r.Direction,
		Participant:         r.Participant.Clone(),
		SourceIdentity:      sourceIdentity,
		DestinationIdentity: destinationIdentity,
		WitnessVersion:      r.WitnessVersion,
		ConfigDigest:        r.ConfigDigest,
	}
	if err := identity.Validate(); err != nil {
		return WorkPreparationIdentity{}, err
	}
	return identity, nil
}

// Validate verifies that preparation is pinned to descriptors, fences, and one physical participant.
func (r WorkPrepareRequest) Validate(ctx context.Context) error {
	if strings.TrimSpace(string(r.Attempt)) == "" || !r.Generation.Valid() || !r.Direction.Valid() || strings.TrimSpace(r.WitnessVersion) == "" || strings.TrimSpace(string(r.ConfigDigest)) == "" {
		return ErrInvalidWorkParticipant
	}
	if err := validateSecretFree("work prepare attempt", string(r.Attempt)); err != nil {
		return err
	}
	if err := validateSecretFree("work prepare witness version", r.WitnessVersion); err != nil {
		return err
	}
	if err := r.Participant.Validate(); err != nil {
		return err
	}
	if err := r.Source.Validate(); err != nil {
		return err
	}
	if err := r.Destination.Validate(); err != nil {
		return err
	}
	preparation, err := r.PreparationIdentity()
	if err != nil {
		return err
	}
	if err := participantMatchesSourceDescriptor(r.Participant, r.Source); err != nil {
		return err
	}
	if err := validateWorkFence(ctx, r.SourceFence, r.Source, r.Generation, FenceRoleSource); err != nil {
		return err
	}
	if err := validateWorkFence(ctx, r.DestinationFence, r.Destination, r.Generation, FenceRolePopulatedDestination, FenceRoleNewDestinationReservation); err != nil {
		return err
	}
	if r.PriorReceipt != nil {
		if err := validateWorkParticipantReceipt(*r.PriorReceipt, preparation, ""); err != nil {
			return err
		}
	}
	return nil
}

func participantMatchesSourceDescriptor(participant WorkWorkspaceParticipant, descriptor Descriptor) error {
	if descriptor.Provider != participant.Provider {
		return ErrInvalidWorkParticipant
	}
	for _, component := range descriptor.Components {
		if component.ID == participant.Component && component.PhysicalIdentity == participant.PhysicalIdentity && component.Classes.Has(coordclass.ClassWork) {
			return nil
		}
	}
	return ErrInvalidWorkParticipant
}

func validateWorkFence(ctx context.Context, fence WriterFence, descriptor Descriptor, generation Generation, allowedRoles ...FenceRole) error {
	if isNilInterface(fence) {
		return ErrInvalidFence
	}
	roleAllowed := false
	for _, role := range allowedRoles {
		if fence.Role() == role {
			roleAllowed = true
			break
		}
	}
	if !roleAllowed || fence.Generation() != generation {
		return ErrInvalidFence
	}
	if err := fence.Target().Validate(); err != nil {
		return ErrInvalidFence
	}
	if !sameComponentIDs(fence.CoveredComponents(), fenceTargetComponentIDs(fence.Target())) {
		return ErrInvalidFence
	}
	if err := validateDescriptorForTarget(descriptor, fence.Target()); err != nil {
		return err
	}
	return heldFence(ctx, fence)
}

// WorkPrepared is a provider-owned prepare receipt for one physical workspace.
type WorkPrepared struct {
	Version            uint16
	Attempt            AttemptID
	Generation         Generation
	Participant        WorkWorkspaceParticipant
	DescriptorIdentity BindingIdentity
	Preparation        WorkPreparationIdentity
	Receipt            string
}

// Clone returns a detached prepared value.
func (p WorkPrepared) Clone() WorkPrepared {
	out := p
	out.Participant = p.Participant.Clone()
	out.Preparation = p.Preparation.Clone()
	return out
}

// Validate verifies a prepared value is bound to one workspace participant.
func (p WorkPrepared) Validate() error {
	if p.Version != 1 || strings.TrimSpace(string(p.Attempt)) == "" || !p.Generation.Valid() || strings.TrimSpace(string(p.DescriptorIdentity)) == "" || strings.TrimSpace(p.Receipt) == "" {
		return ErrInvalidWorkParticipant
	}
	if err := validateSecretFree("work prepared attempt", string(p.Attempt)); err != nil {
		return err
	}
	if err := validateCanonicalSHA256Digest("work prepared descriptor identity", string(p.DescriptorIdentity)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidWorkParticipant, err)
	}
	if err := p.Participant.Validate(); err != nil {
		return err
	}
	if err := p.Preparation.Validate(); err != nil {
		return err
	}
	if !workOutputMatchesPreparation(p.Attempt, p.Generation, p.Participant, p.DescriptorIdentity, p.Preparation) {
		return ErrInvalidWorkParticipant
	}
	for field, value := range map[string]string{
		"work prepared descriptor identity": string(p.DescriptorIdentity),
		"work prepare receipt":              p.Receipt,
	} {
		if err := validateSecretFree(field, value); err != nil {
			return err
		}
	}
	return nil
}

// WorkVerifyRequest supplies the prepared participant for a semantic witness check.
type WorkVerifyRequest struct {
	Prepare  WorkPrepareRequest
	Prepared WorkPrepared
}

// Clone returns a detached verify request for a provider boundary.
func (r WorkVerifyRequest) Clone() WorkVerifyRequest {
	return WorkVerifyRequest{Prepare: r.Prepare.Clone(), Prepared: r.Prepared.Clone()}
}

// Validate checks that verify remains pinned to the prepared workspace.
func (r WorkVerifyRequest) Validate(ctx context.Context) error {
	if err := r.Prepare.Validate(ctx); err != nil {
		return err
	}
	if err := r.Prepared.Validate(); err != nil {
		return err
	}
	preparation, err := r.Prepare.PreparationIdentity()
	if err != nil {
		return err
	}
	if !r.Prepared.Preparation.Equal(preparation) {
		return ErrInvalidWorkParticipant
	}
	return nil
}

// WorkProof is a provider-owned semantic witness proof for one physical workspace.
type WorkProof struct {
	Version            uint16
	Attempt            AttemptID
	Generation         Generation
	Participant        WorkWorkspaceParticipant
	DescriptorIdentity BindingIdentity
	Preparation        WorkPreparationIdentity
	PreparedReceipt    string
	Witness            string
}

// Clone returns a detached proof value.
func (p WorkProof) Clone() WorkProof {
	out := p
	out.Participant = p.Participant.Clone()
	out.Preparation = p.Preparation.Clone()
	return out
}

// Validate verifies a work proof remains participant-scoped.
func (p WorkProof) Validate() error {
	if p.Version != 1 || strings.TrimSpace(string(p.Attempt)) == "" || !p.Generation.Valid() || strings.TrimSpace(string(p.DescriptorIdentity)) == "" || strings.TrimSpace(p.PreparedReceipt) == "" || strings.TrimSpace(p.Witness) == "" {
		return ErrInvalidWorkParticipant
	}
	if err := validateSecretFree("work proof attempt", string(p.Attempt)); err != nil {
		return err
	}
	if err := validateCanonicalSHA256Digest("work proof descriptor identity", string(p.DescriptorIdentity)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidWorkParticipant, err)
	}
	if err := p.Participant.Validate(); err != nil {
		return err
	}
	if err := p.Preparation.Validate(); err != nil {
		return err
	}
	if !workOutputMatchesPreparation(p.Attempt, p.Generation, p.Participant, p.DescriptorIdentity, p.Preparation) {
		return ErrInvalidWorkParticipant
	}
	for field, value := range map[string]string{
		"work proof descriptor identity": string(p.DescriptorIdentity),
		"work proof prepared receipt":    p.PreparedReceipt,
		"work proof witness":             p.Witness,
	} {
		if err := validateSecretFree(field, value); err != nil {
			return err
		}
	}
	return nil
}

// WorkCommitRequest commits a prepared and proven physical workspace after COMMIT_DECIDED.
type WorkCommitRequest struct {
	Decision    CommitDecision
	Participant WorkWorkspaceParticipant
	Prepare     WorkPrepareRequest
	Prepared    WorkPrepared
	Proof       WorkProof
}

// Clone returns a detached commit request for a provider boundary.
func (r WorkCommitRequest) Clone() WorkCommitRequest {
	return WorkCommitRequest{
		Decision:    r.Decision,
		Participant: r.Participant.Clone(),
		Prepare:     r.Prepare.Clone(),
		Prepared:    r.Prepared.Clone(),
		Proof:       r.Proof.Clone(),
	}
}

// Validate verifies that Work commit is legal only after the global decision.
func (r WorkCommitRequest) Validate() error {
	return r.validate(context.Background())
}

func (r WorkCommitRequest) validate(ctx context.Context) error {
	if err := r.Decision.Validate(); err != nil {
		return err
	}
	if err := r.Participant.Validate(); err != nil {
		return err
	}
	if err := r.Prepare.Validate(ctx); err != nil {
		return err
	}
	if err := r.Prepared.Validate(); err != nil {
		return err
	}
	if err := r.Proof.Validate(); err != nil {
		return err
	}
	preparation, err := r.Prepare.PreparationIdentity()
	if err != nil {
		return err
	}
	if r.Prepare.Attempt != r.Decision.Attempt || r.Prepare.Generation != r.Decision.Generation || !r.Prepare.Participant.Equal(r.Participant) || r.Prepared.Attempt != r.Decision.Attempt || r.Prepared.Generation != r.Decision.Generation || r.Proof.Attempt != r.Decision.Attempt || r.Proof.Generation != r.Decision.Generation || !r.Prepared.Participant.Equal(r.Participant) || !r.Proof.Participant.Equal(r.Participant) || r.Prepared.DescriptorIdentity != r.Proof.DescriptorIdentity || !r.Prepared.Preparation.Equal(r.Proof.Preparation) || !r.Prepared.Preparation.Equal(preparation) || r.Proof.PreparedReceipt != r.Prepared.Receipt {
		return ErrInvalidWorkParticipant
	}
	return nil
}

// WorkResumeRequest resumes one direction-tagged physical workspace migration idempotently.
type WorkResumeRequest struct {
	Prepare WorkPrepareRequest
}

// Clone returns a detached resume request for a provider boundary.
func (r WorkResumeRequest) Clone() WorkResumeRequest {
	return WorkResumeRequest{Prepare: r.Prepare.Clone()}
}

// Validate verifies that resume uses the original pinned prepare request.
func (r WorkResumeRequest) Validate(ctx context.Context) error { return r.Prepare.Validate(ctx) }

// WorkProgress describes idempotent provider-owned Work migration progress.
type WorkProgress struct {
	Version     uint16
	Attempt     AttemptID
	Generation  Generation
	Participant WorkWorkspaceParticipant
	Preparation WorkPreparationIdentity
	Complete    bool
	Receipt     *ParticipantReceipt
}

// Clone returns a detached progress value.
func (p WorkProgress) Clone() WorkProgress {
	out := p
	out.Participant = p.Participant.Clone()
	out.Preparation = p.Preparation.Clone()
	if p.Receipt != nil {
		receipt := p.Receipt.Clone()
		out.Receipt = &receipt
	}
	return out
}

// Validate verifies progress and its durable receipt when migration is complete.
func (p WorkProgress) Validate() error {
	if p.Version != 1 || strings.TrimSpace(string(p.Attempt)) == "" || !p.Generation.Valid() {
		return ErrInvalidWorkParticipant
	}
	if err := validateSecretFree("work progress attempt", string(p.Attempt)); err != nil {
		return err
	}
	if err := p.Participant.Validate(); err != nil {
		return err
	}
	if err := p.Preparation.Validate(); err != nil {
		return err
	}
	if p.Attempt != p.Preparation.Attempt || p.Generation != p.Preparation.Generation || !p.Participant.Equal(p.Preparation.Participant) || p.Complete != (p.Receipt != nil) {
		return ErrInvalidWorkParticipant
	}
	if p.Receipt != nil {
		if err := validateWorkParticipantReceipt(*p.Receipt, p.Preparation, ""); err != nil {
			return err
		}
	}
	return nil
}

// RetainedWorkRequest asks a provider to reopen exactly one pinned retained physical workspace.
type RetainedWorkRequest struct {
	Attempt          AttemptID
	Generation       Generation
	Participant      WorkWorkspaceParticipant
	Source           RetainedSourceRef
	ExpectedContract ContractVersion
}

// Clone returns a detached retained Work request for a provider boundary.
func (r RetainedWorkRequest) Clone() RetainedWorkRequest {
	out := r
	out.Participant = r.Participant.Clone()
	out.Source = r.Source.Clone()
	return out
}

// Validate verifies that retained Work reopening never follows a mutable binding name.
func (r RetainedWorkRequest) Validate() error {
	if strings.TrimSpace(string(r.Attempt)) == "" || !r.Generation.Valid() || strings.TrimSpace(string(r.ExpectedContract)) == "" {
		return ErrInvalidWorkParticipant
	}
	if err := validateSecretFree("retained work attempt", string(r.Attempt)); err != nil {
		return err
	}
	if err := r.Participant.Validate(); err != nil {
		return err
	}
	if err := r.Source.Validate(); err != nil {
		return err
	}
	if err := validateSecretFree("retained work expected contract", string(r.ExpectedContract)); err != nil {
		return err
	}
	if r.Source.Provider != r.Participant.Provider || r.Source.Component != r.Participant.Component || r.Source.PhysicalIdentity != r.Participant.PhysicalIdentity || !r.Source.Classes.Has(coordclass.ClassWork) || r.Source.SemanticContractVersion != r.ExpectedContract {
		return fmt.Errorf("%w: retained source identity differs from participant", ErrInvalidWorkParticipant)
	}
	return nil
}

// FrozenWorkPrefixStore is a provider-created Work view whose prefix is frozen
// at retained-open time. It deliberately does not expose a mutable IDPrefix
// lookup to lifecycle validation.
type FrozenWorkPrefixStore interface {
	beads.Store
	FrozenWorkPrefix() string
}

// RetainedWorkMemberView is one frozen prefix-aware semantic scope view over a
// retained physical workspace. Multiple views may share one physical handle.
type RetainedWorkMemberView struct {
	Scope  WorkScope
	Prefix string
	Store  FrozenWorkPrefixStore
}

// RetainedWorkWorkspace is one opened retained physical workspace with every prefix-aware semantic scope view.
type RetainedWorkWorkspace struct {
	Participant WorkWorkspaceParticipant
	views       []RetainedWorkMemberView
	closer      *retainedWorkspaceCloser
}

type retainedWorkspaceCloser struct {
	mu     sync.Mutex
	fn     func() error
	closed bool
}

// NewRetainedWorkWorkspace creates frozen prefix-aware member views over one shared physical store.
func NewRetainedWorkWorkspace(participant WorkWorkspaceParticipant, store FrozenWorkPrefixStore, closeFn func() error) (RetainedWorkWorkspace, error) {
	reject := func(err error) (RetainedWorkWorkspace, error) {
		return closeRejectedRetainedWorkspace(closeFn, err)
	}
	if err := participant.Validate(); err != nil {
		return reject(err)
	}
	if isNilInterface(store) || closeFn == nil {
		return reject(ErrInvalidWorkParticipant)
	}
	views := make([]RetainedWorkMemberView, len(participant.Members))
	for index, member := range participant.Members {
		if store.FrozenWorkPrefix() != member.Prefix {
			return reject(ErrInvalidWorkParticipant)
		}
		views[index] = RetainedWorkMemberView{Scope: member.Scope, Prefix: member.Prefix, Store: store}
	}
	return NewRetainedWorkWorkspaceWithViews(participant, views, closeFn)
}

// NewRetainedWorkWorkspaceWithViews creates a close-once physical workspace with provider-supplied member views.
func NewRetainedWorkWorkspaceWithViews(participant WorkWorkspaceParticipant, views []RetainedWorkMemberView, closeFn func() error) (RetainedWorkWorkspace, error) {
	reject := func(err error) (RetainedWorkWorkspace, error) {
		return closeRejectedRetainedWorkspace(closeFn, err)
	}
	if err := participant.Validate(); err != nil {
		return reject(err)
	}
	if closeFn == nil || len(views) != len(participant.Members) {
		return reject(ErrInvalidWorkParticipant)
	}
	byScope := make(map[WorkScope]RetainedWorkMemberView, len(views))
	for _, view := range views {
		if isNilInterface(view.Store) || view.Store.FrozenWorkPrefix() != view.Prefix {
			return reject(ErrInvalidWorkParticipant)
		}
		if _, exists := byScope[view.Scope]; exists {
			return reject(ErrInvalidWorkParticipant)
		}
		byScope[view.Scope] = view
	}
	canonical := make([]RetainedWorkMemberView, len(participant.Members))
	for index, member := range participant.Members {
		view, found := byScope[member.Scope]
		if !found || view.Prefix != member.Prefix {
			return reject(ErrInvalidWorkParticipant)
		}
		canonical[index] = RetainedWorkMemberView{Scope: member.Scope, Prefix: member.Prefix, Store: view.Store}
	}
	return RetainedWorkWorkspace{Participant: participant.Clone(), views: canonical, closer: &retainedWorkspaceCloser{fn: closeFn}}, nil
}

func closeRejectedRetainedWorkspace(closeFn func() error, rejected error) (RetainedWorkWorkspace, error) {
	if closeFn == nil {
		return RetainedWorkWorkspace{}, rejected
	}
	workspace := RetainedWorkWorkspace{closer: &retainedWorkspaceCloser{fn: closeFn}}
	if err := workspace.Close(); err != nil {
		return RetainedWorkWorkspace{}, &RejectedRetainedWorkWorkspaceCleanupError{
			rejected:   rejected,
			cleanupErr: fmt.Errorf("closing rejected retained work workspace: %w", err),
			workspace:  workspace,
		}
	}
	return RetainedWorkWorkspace{}, rejected
}

// RejectedRetainedWorkWorkspaceCleanupError retains cleanup ownership when a
// rejected retained workspace cannot close immediately. The workspace is never
// returned as usable; callers may only retry cleanup through RetryCleanup.
type RejectedRetainedWorkWorkspaceCleanupError struct {
	rejected   error
	cleanupErr error
	workspace  RetainedWorkWorkspace
	mu         sync.Mutex
	cleaned    bool
}

// Error reports the primary rejection and the failed physical cleanup.
func (e *RejectedRetainedWorkWorkspaceCleanupError) Error() string {
	if e == nil {
		return "rejected retained work workspace cleanup"
	}
	return errors.Join(e.rejected, e.cleanupErr).Error()
}

// Unwrap exposes both the primary rejection and failed cleanup for errors.Is
// and errors.As.
func (e *RejectedRetainedWorkWorkspaceCleanupError) Unwrap() []error {
	if e == nil {
		return nil
	}
	return []error{e.rejected, e.cleanupErr}
}

// RetryCleanup retries closing the unreachable rejected workspace. Once its
// close succeeds, subsequent retries are no-ops.
func (e *RejectedRetainedWorkWorkspaceCleanupError) RetryCleanup() error {
	if e == nil || e.workspace.closer == nil {
		return ErrInvalidWorkParticipant
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cleaned {
		return nil
	}
	if err := e.workspace.Close(); err != nil {
		return fmt.Errorf("retrying rejected retained work workspace cleanup: %w", err)
	}
	e.cleaned = true
	return nil
}

// Close closes the physical retained workspace exactly once.
func (w RetainedWorkWorkspace) Close() error {
	if w.closer == nil {
		return ErrInvalidWorkParticipant
	}
	w.closer.mu.Lock()
	defer w.closer.mu.Unlock()
	if w.closer.closed {
		return nil
	}
	if err := w.closer.fn(); err != nil {
		return err
	}
	w.closer.closed = true
	return nil
}

// Validate verifies that a returned retained workspace preserves its requested participant.
func (w RetainedWorkWorkspace) Validate() error {
	if err := w.Participant.Validate(); err != nil {
		return err
	}
	if w.closer == nil || len(w.views) != len(w.Participant.Members) {
		return ErrInvalidWorkParticipant
	}
	byScope := make(map[WorkScope]RetainedWorkMemberView, len(w.views))
	for _, view := range w.views {
		if isNilInterface(view.Store) || view.Store.FrozenWorkPrefix() != view.Prefix {
			return ErrInvalidWorkParticipant
		}
		if _, exists := byScope[view.Scope]; exists {
			return ErrInvalidWorkParticipant
		}
		byScope[view.Scope] = view
	}
	for _, member := range w.Participant.Members {
		view, found := byScope[member.Scope]
		if !found || view.Prefix != member.Prefix {
			return ErrInvalidWorkParticipant
		}
	}
	return nil
}

// View returns the prefix-aware store view for one semantic Work scope.
func (w RetainedWorkWorkspace) View(scope WorkScope) (RetainedWorkMemberView, error) {
	if err := w.Validate(); err != nil {
		return RetainedWorkMemberView{}, err
	}
	for _, view := range w.views {
		if view.Scope == scope {
			return view, nil
		}
	}
	return RetainedWorkMemberView{}, &WorkScopeNotFoundError{Scope: scope}
}

// WorkMigrationLifecycle is the sole provider seam for grouped populated Work moves.
type WorkMigrationLifecycle interface {
	Prepare(context.Context, WorkPrepareRequest) (WorkPrepared, error)
	Verify(context.Context, WorkVerifyRequest) (WorkProof, error)
	Commit(context.Context, WorkCommitRequest) (ParticipantReceipt, error)
	Resume(context.Context, WorkResumeRequest) (WorkProgress, error)
	OpenRetained(context.Context, RetainedWorkRequest) (RetainedWorkWorkspace, error)
}

// RetainedWorkLifecycleResolver resolves the exact source-provider lifecycle
// for retained Work participants before any provider open occurs.
type RetainedWorkLifecycleResolver interface {
	ResolveRetainedWorkLifecycle(context.Context, ProviderID) (WorkMigrationLifecycle, error)
}

// RetainedWorkLifecycleResolverFunc adapts a function into a retained Work
// lifecycle resolver.
type RetainedWorkLifecycleResolverFunc func(context.Context, ProviderID) (WorkMigrationLifecycle, error)

// ResolveRetainedWorkLifecycle resolves one exact provider lifecycle.
func (f RetainedWorkLifecycleResolverFunc) ResolveRetainedWorkLifecycle(ctx context.Context, provider ProviderID) (WorkMigrationLifecycle, error) {
	return f(ctx, provider)
}

// PrepareWork validates and delegates one direction-tagged physical participant preparation.
func PrepareWork(ctx context.Context, lifecycle WorkMigrationLifecycle, request WorkPrepareRequest) (WorkPrepared, error) {
	if isNilInterface(lifecycle) {
		return WorkPrepared{}, fmt.Errorf("%w: work migration lifecycle is nil", ErrMissingCapability)
	}
	baseline := request.Clone()
	if err := baseline.Validate(ctx); err != nil {
		return WorkPrepared{}, err
	}
	sourceFence, err := snapshotWriterFence(ctx, baseline.SourceFence)
	if err != nil {
		return WorkPrepared{}, err
	}
	destinationFence, err := snapshotWriterFence(ctx, baseline.DestinationFence)
	if err != nil {
		return WorkPrepared{}, err
	}
	prepared, providerErr := lifecycle.Prepare(ctx, baseline.Clone())
	if err := providerFenceResult(ctx, "preparing work participant", providerErr, sourceFence, destinationFence); err != nil {
		return WorkPrepared{}, err
	}
	prepared = prepared.Clone()
	if err := prepared.Validate(); err != nil {
		return WorkPrepared{}, err
	}
	preparation, err := baseline.PreparationIdentity()
	if err != nil {
		return WorkPrepared{}, err
	}
	if !prepared.Preparation.Equal(preparation) {
		return WorkPrepared{}, ErrInvalidWorkParticipant
	}
	return prepared.Clone(), nil
}

// VerifyWork validates and delegates a provider-neutral witness proof for one participant.
func VerifyWork(ctx context.Context, lifecycle WorkMigrationLifecycle, request WorkVerifyRequest) (WorkProof, error) {
	if isNilInterface(lifecycle) {
		return WorkProof{}, fmt.Errorf("%w: work migration lifecycle is nil", ErrMissingCapability)
	}
	baseline := request.Clone()
	if err := baseline.Validate(ctx); err != nil {
		return WorkProof{}, err
	}
	sourceFence, err := snapshotWriterFence(ctx, baseline.Prepare.SourceFence)
	if err != nil {
		return WorkProof{}, err
	}
	destinationFence, err := snapshotWriterFence(ctx, baseline.Prepare.DestinationFence)
	if err != nil {
		return WorkProof{}, err
	}
	proof, providerErr := lifecycle.Verify(ctx, baseline.Clone())
	if err := providerFenceResult(ctx, "verifying work participant", providerErr, sourceFence, destinationFence); err != nil {
		return WorkProof{}, err
	}
	proof = proof.Clone()
	if err := proof.Validate(); err != nil {
		return WorkProof{}, err
	}
	preparation, err := baseline.Prepare.PreparationIdentity()
	if err != nil {
		return WorkProof{}, err
	}
	if !proof.Preparation.Equal(preparation) || proof.PreparedReceipt != baseline.Prepared.Receipt {
		return WorkProof{}, ErrInvalidWorkParticipant
	}
	return proof.Clone(), nil
}

// CommitWork validates and delegates an idempotent post-decision participant commit.
func CommitWork(ctx context.Context, lifecycle WorkMigrationLifecycle, request WorkCommitRequest) (ParticipantReceipt, error) {
	if isNilInterface(lifecycle) {
		return ParticipantReceipt{}, fmt.Errorf("%w: work migration lifecycle is nil", ErrMissingCapability)
	}
	baseline := request.Clone()
	if err := baseline.validate(ctx); err != nil {
		return ParticipantReceipt{}, err
	}
	sourceFence, err := snapshotWriterFence(ctx, baseline.Prepare.SourceFence)
	if err != nil {
		return ParticipantReceipt{}, err
	}
	destinationFence, err := snapshotWriterFence(ctx, baseline.Prepare.DestinationFence)
	if err != nil {
		return ParticipantReceipt{}, err
	}
	receipt, providerErr := lifecycle.Commit(ctx, baseline.Clone())
	if err := providerFenceResult(ctx, "committing work participant", providerErr, sourceFence, destinationFence); err != nil {
		return ParticipantReceipt{}, err
	}
	receipt = receipt.Clone()
	if err := receipt.Validate(); err != nil {
		return ParticipantReceipt{}, err
	}
	if err := validateWorkParticipantReceipt(receipt, baseline.Prepared.Preparation, baseline.Prepared.Receipt); err != nil {
		return ParticipantReceipt{}, ErrInvalidWorkParticipant
	}
	return receipt.Clone(), nil
}

// ResumeWork validates and delegates idempotent participant recovery.
func ResumeWork(ctx context.Context, lifecycle WorkMigrationLifecycle, request WorkResumeRequest) (WorkProgress, error) {
	if isNilInterface(lifecycle) {
		return WorkProgress{}, fmt.Errorf("%w: work migration lifecycle is nil", ErrMissingCapability)
	}
	baseline := request.Clone()
	if err := baseline.Validate(ctx); err != nil {
		return WorkProgress{}, err
	}
	sourceFence, err := snapshotWriterFence(ctx, baseline.Prepare.SourceFence)
	if err != nil {
		return WorkProgress{}, err
	}
	destinationFence, err := snapshotWriterFence(ctx, baseline.Prepare.DestinationFence)
	if err != nil {
		return WorkProgress{}, err
	}
	progress, providerErr := lifecycle.Resume(ctx, baseline.Clone())
	if err := providerFenceResult(ctx, "resuming work participant", providerErr, sourceFence, destinationFence); err != nil {
		return WorkProgress{}, err
	}
	progress = progress.Clone()
	if err := progress.Validate(); err != nil {
		return WorkProgress{}, err
	}
	preparation, err := baseline.Prepare.PreparationIdentity()
	if err != nil {
		return WorkProgress{}, err
	}
	if !progress.Preparation.Equal(preparation) {
		return WorkProgress{}, ErrInvalidWorkParticipant
	}
	return progress.Clone(), nil
}

func workOutputMatchesPreparation(attempt AttemptID, generation Generation, participant WorkWorkspaceParticipant, descriptorIdentity BindingIdentity, preparation WorkPreparationIdentity) bool {
	return attempt == preparation.Attempt && generation == preparation.Generation && participant.Equal(preparation.Participant) && descriptorIdentity == preparation.DestinationIdentity
}

func validateWorkParticipantReceipt(receipt ParticipantReceipt, preparation WorkPreparationIdentity, preparedReceipt string) error {
	if err := receipt.Validate(); err != nil {
		return err
	}
	if receipt.Kind != ParticipantReceiptWorkMigration || !receipt.Preparation.Equal(preparation) || (preparedReceipt != "" && receipt.PreparedReceipt != preparedReceipt) {
		return ErrInvalidWorkParticipant
	}
	return nil
}

// OpenRetainedWorkTopology pre-resolves each exact retained source lifecycle,
// opens every distinct physical workspace once, and composes one complete
// topology only after all global topology conflicts are ruled out.
func OpenRetainedWorkTopology(ctx context.Context, resolver RetainedWorkLifecycleResolver, requests []RetainedWorkRequest) (WorkTopology, []RetainedWorkWorkspace, error) {
	if isNilInterface(resolver) {
		return WorkTopology{}, nil, fmt.Errorf("%w: retained work lifecycle resolver is nil", ErrMissingCapability)
	}
	baselines := cloneRetainedWorkRequests(requests)
	if err := validateRetainedWorkTopologyRequests(baselines); err != nil {
		return WorkTopology{}, nil, err
	}
	lifecycles := make(map[ProviderID]WorkMigrationLifecycle)
	for _, request := range baselines {
		if _, resolved := lifecycles[request.Participant.Provider]; resolved {
			continue
		}
		lifecycle, err := resolver.ResolveRetainedWorkLifecycle(ctx, request.Participant.Provider)
		if err != nil {
			return WorkTopology{}, nil, fmt.Errorf("resolving retained work lifecycle for provider %q: %w", request.Participant.Provider, err)
		}
		if isNilInterface(lifecycle) {
			return WorkTopology{}, nil, fmt.Errorf("%w: retained work lifecycle for provider %q is nil", ErrMissingCapability, request.Participant.Provider)
		}
		lifecycles[request.Participant.Provider] = lifecycle
	}
	opened := make([]RetainedWorkWorkspace, 0, len(baselines))
	for _, request := range baselines {
		lifecycle := lifecycles[request.Participant.Provider]
		workspace, err := lifecycle.OpenRetained(ctx, request.Clone())
		if err != nil {
			if workspace.closer != nil {
				opened = append(opened, workspace)
			}
			return rejectOpenedRetainedWorkspaces(fmt.Errorf("opening retained work participant: %w", err), opened)
		}
		if err := workspace.Validate(); err != nil || !workspace.Participant.Equal(request.Participant) {
			if workspace.closer != nil {
				opened = append(opened, workspace)
			}
			primary := ErrInvalidWorkParticipant
			if err != nil {
				primary = err
			}
			return rejectOpenedRetainedWorkspaces(primary, opened)
		}
		opened = append(opened, workspace)
	}
	topology, err := ComposeRetainedWorkTopology(opened)
	if err != nil {
		return rejectOpenedRetainedWorkspaces(err, opened)
	}
	return topology, opened, nil
}

func cloneRetainedWorkRequests(requests []RetainedWorkRequest) []RetainedWorkRequest {
	cloned := make([]RetainedWorkRequest, len(requests))
	for index, request := range requests {
		cloned[index] = request.Clone()
	}
	return cloned
}

func rejectOpenedRetainedWorkspaces(rejected error, opened []RetainedWorkWorkspace) (WorkTopology, []RetainedWorkWorkspace, error) {
	if cleanupErr := closeRetainedWorkspaces(opened); cleanupErr != nil {
		return WorkTopology{}, opened, errors.Join(rejected, cleanupErr)
	}
	return WorkTopology{}, nil, rejected
}

func validateRetainedWorkTopologyRequests(requests []RetainedWorkRequest) error {
	seenParticipants := make(map[workParticipantIdentity]struct{}, len(requests))
	members := make([]WorkWorkspaceMember, 0, len(requests))
	for _, request := range requests {
		if err := request.Validate(); err != nil {
			return err
		}
		if _, duplicate := seenParticipants[request.Participant.identity()]; duplicate {
			return fmt.Errorf("%w: %s", ErrDuplicateWorkParticipant, request.Participant.Key())
		}
		seenParticipants[request.Participant.identity()] = struct{}{}
		members = append(members, request.Participant.Members...)
	}
	return validateWorkMembersTopology(members)
}

func workOnlyClasses() ClassSet { return ClassSet{work: true} }

// ComposeRetainedWorkTopology constructs the work topology from once-opened retained physical workspaces.
func ComposeRetainedWorkTopology(handles []RetainedWorkWorkspace) (WorkTopology, error) {
	seenParticipants := make(map[workParticipantIdentity]struct{}, len(handles))
	seenScopes := make(map[WorkScope]struct{})
	var hq Workspace
	var haveHQ bool
	type orderedRig struct {
		workspace Workspace
		order     int
	}
	rigs := make([]orderedRig, 0)
	seenRigOrders := make(map[int]struct{})
	for _, handle := range handles {
		if err := handle.Validate(); err != nil {
			return WorkTopology{}, err
		}
		if _, exists := seenParticipants[handle.Participant.identity()]; exists {
			return WorkTopology{}, fmt.Errorf("%w: %s", ErrDuplicateWorkParticipant, handle.Participant.Key())
		}
		seenParticipants[handle.Participant.identity()] = struct{}{}
		for _, member := range handle.Participant.Members {
			if _, exists := seenScopes[member.Scope]; exists {
				return WorkTopology{}, fmt.Errorf("%w: duplicate scope %s", ErrInvalidWorkParticipant, member.Scope)
			}
			seenScopes[member.Scope] = struct{}{}
			view, err := handle.View(member.Scope)
			if err != nil {
				return WorkTopology{}, err
			}
			workspace := Workspace{Scope: member.Scope, Store: view.Store, Prefix: member.Prefix, Suspended: member.Suspended, OpenerID: string(handle.Participant.Provider), ComponentID: string(handle.Participant.Component), PhysicalID: string(handle.Participant.PhysicalIdentity)}
			if member.Scope.IsHQ() {
				if haveHQ {
					return WorkTopology{}, fmt.Errorf("%w: duplicate HQ", ErrInvalidWorkParticipant)
				}
				hq = workspace
				haveHQ = true
				continue
			}
			if _, exists := seenRigOrders[member.ConfigOrder]; exists {
				return WorkTopology{}, fmt.Errorf("%w: duplicate rig config order", ErrInvalidWorkParticipant)
			}
			seenRigOrders[member.ConfigOrder] = struct{}{}
			rigs = append(rigs, orderedRig{workspace: workspace, order: member.ConfigOrder})
		}
	}
	if !haveHQ {
		return WorkTopology{}, fmt.Errorf("%w: missing HQ", ErrInvalidWorkParticipant)
	}
	sort.SliceStable(rigs, func(i, j int) bool {
		if rigs[i].order == rigs[j].order {
			return rigs[i].workspace.Scope.String() < rigs[j].workspace.Scope.String()
		}
		return rigs[i].order < rigs[j].order
	})
	orderedWorkspaces := make([]Workspace, len(rigs))
	for index, rig := range rigs {
		orderedWorkspaces[index] = rig.workspace
	}
	return NewWorkTopology(hq, orderedWorkspaces)
}

func closeRetainedWorkspaces(handles []RetainedWorkWorkspace) error {
	var closeErrors []error
	for index := len(handles) - 1; index >= 0; index-- {
		handle := handles[index]
		if err := closeRetainedWorkspace(handle); err != nil {
			closeErrors = append(closeErrors, err)
		}
	}
	return errors.Join(closeErrors...)
}

func closeRetainedWorkspace(handle RetainedWorkWorkspace) error {
	if handle.closer == nil {
		return nil
	}
	if err := handle.Close(); err != nil {
		return fmt.Errorf("closing retained work workspace: %w", err)
	}
	return nil
}

func heldFence(ctx context.Context, fence WriterFence) error {
	if isNilInterface(fence) {
		return ErrInvalidFence
	}
	held, err := fence.Held(ctx)
	if err != nil {
		return err
	}
	if !held {
		return ErrFenceNotHeld
	}
	return nil
}

// CGOUnavailableError is a typed unavailable-implementation error for CGO-dependent providers.
// It deliberately does not retain provider or operation text because the value may cross
// untrusted provider boundaries before being logged.
type CGOUnavailableError struct{}

// NewCGOUnavailableError creates a redacted typed unavailable-implementation error.
// Provider and operation are accepted only to keep the assembly boundary explicit; they are
// intentionally discarded and never become observable error data.
func NewCGOUnavailableError(ProviderID, string) *CGOUnavailableError { return &CGOUnavailableError{} }

// Error implements error without embedding environment or credential detail.
func (e *CGOUnavailableError) Error() string {
	return ErrCGOUnavailable.Error()
}

// Unwrap supports errors.Is for both generic availability and the typed CGO cause.
func (e *CGOUnavailableError) Unwrap() error {
	return errors.Join(ErrProviderUnavailable, ErrCGOUnavailable)
}
