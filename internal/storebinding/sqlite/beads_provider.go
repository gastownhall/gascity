package sqlite

// The canonical Beads-over-SQLite provider.
//
// This is the first implementation in the tree that satisfies
// storebinding.Provider end to end, so it is the first thing that ever walks
// the whole lifecycle — Inspect, AcquireFence, InspectFenced, Open — against a
// real database instead of a fake. That matters beyond this package: an
// out-of-tree provider that is the FIRST to walk a contract discovers every
// mismatch in the contract on its own, where nobody else can see it.
//
// The scope is one SQLite Beads ledger. NewBeadsAdapters projects that single
// store into all six closed class front doors, so the descriptor describes ONE
// component carrying six classes — not six components, and not a composite of
// several. Nothing here re-derives a physical fact: the mutation-free
// census, the physical identity, the writer reservation, and the fenced
// private-snapshot census all come from the deployed Graph component
// machinery, which observes exactly the same file. This provider differs from
// that component in two declared facts and no observed ones: which provider ID
// owns the binding, and which classes the one component serves.
//
// The narrow/widen pair below is the whole of that difference, which is why it
// is two functions and not a layer.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storebinding"
)

const (
	// BeadsProviderID is the built-in provider identifier for one canonical
	// SQLite Beads scope projected into every storage class.
	BeadsProviderID = storebinding.ProviderID("sqlite-beads")

	// beadsContract is the semantic contract of a single Beads ledger serving
	// all six classes. It is deliberately distinct from graphContract: the
	// physical component is the same file, but a consumer that pins the Graph
	// contract is pinning a one-class binding and must not silently be handed
	// a six-class one.
	beadsContract = storebinding.ContractVersion("gascity-beads-v1")

	beadsFenceProjection = storebinding.FenceProjection("sqlite.beads.inspection")
)

var (
	// ErrInvalidBeadsBinding reports a specification, target, or descriptor
	// that is not this provider's exact single-component Beads scope.
	ErrInvalidBeadsBinding = errors.New("invalid SQLite Beads binding")

	// ErrBeadsLiveWAL is this scope's quiescence sentinel. It is returned when
	// a source open would have to answer from — or recover — a database whose
	// sidecars still hold authoritative bytes.
	ErrBeadsLiveWAL = errors.New("SQLite Beads source has authoritative sidecar content")

	// ErrBeadsCapabilityUndeclared reports an engine that does not have a
	// capability its descriptor declares. It is refused at Open so no consumer
	// ever discovers the loss halfway through a claim or a transaction.
	ErrBeadsCapabilityUndeclared = errors.New("SQLite Beads engine lacks a declared capability")

	_ storebinding.ProviderFactory = BeadsProviderFactory{}
	_ storebinding.Provider        = (*beadsProvider)(nil)
)

// BeadsProviderFactory constructs the resource-free provider facade for one
// canonical SQLite Beads binding.
type BeadsProviderFactory struct{}

// ID returns the exact provider identifier this factory registers.
func (BeadsProviderFactory) ID() storebinding.ProviderID { return BeadsProviderID }

// New binds one validated specification. It opens nothing: every physical
// resource this provider touches is scoped to the call that needs it or is
// transferred to the caller through a fence or an opened binding.
func (BeadsProviderFactory) New(spec storebinding.BindingSpec) (storebinding.Provider, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if spec.Provider != BeadsProviderID {
		return nil, fmt.Errorf("%w: provider %q", ErrInvalidBeadsBinding, spec.Provider)
	}
	path, err := GraphPath(BindingRoot(spec))
	if err != nil {
		return nil, err
	}
	component := spec
	component.Provider = ProviderID
	inspector, err := NewGraphInspector(component)
	if err != nil {
		return nil, err
	}
	bound := spec
	bound.Path = filepath.Dir(filepath.Dir(path))
	component.Path = bound.Path
	return &beadsProvider{spec: bound, component: component, inspector: inspector, path: path}, nil
}

// beadsProvider is one immutable provider facade bound to one binding.
type beadsProvider struct {
	// spec is the caller's binding with its path canonicalized.
	spec storebinding.BindingSpec
	// component is the same binding expressed to the deployed component
	// machinery, which owns provider ID "sqlite". Only the ConfigRef and the
	// canonical root reach a descriptor digest, so both spellings digest alike.
	component storebinding.BindingSpec
	inspector *GraphInspector
	path      string
}

// Inspect observes the single Beads component without writing to it, and
// reports the six-class scope that component serves.
func (p *beadsProvider) Inspect(ctx context.Context, spec storebinding.BindingSpec) (storebinding.Inspection, error) {
	if err := p.boundTo(spec); err != nil {
		return storebinding.Inspection{}, err
	}
	inspection, err := p.inspector.Inspect(ctx)
	if err != nil {
		return storebinding.Inspection{}, err
	}
	target, err := widenBeadsTarget(inspection.Target)
	if err != nil {
		return storebinding.Inspection{}, err
	}
	if inspection.Descriptor == nil {
		return storebinding.NewInspection(target, nil)
	}
	descriptor, err := widenBeadsDescriptor(*inspection.Descriptor)
	if err != nil {
		return storebinding.Inspection{}, err
	}
	return storebinding.NewInspection(target, &descriptor)
}

// AcquireFence reserves the exact inspected component under a claimed city
// migration guard. The reservation is the deployed Graph component's: the same
// file needs the same operating-system exclusion whichever classes it serves.
func (p *beadsProvider) AcquireFence(ctx context.Context, claim storebinding.MigrationGuardClaim, request storebinding.FenceRequest) (storebinding.WriterFence, error) {
	scope := request.Target.Clone()
	component, err := narrowBeadsTarget(scope)
	if err != nil {
		return nil, err
	}
	componentRequest := request.Clone()
	componentRequest.Target = component
	inner, acquireErr := p.inspector.AcquireFence(ctx, claim, componentRequest)
	if inner == nil {
		return nil, acquireErr
	}
	// A failed acquisition can still return a fence that owns pending cleanup.
	// It must be handed back wrapped, so the generic rejection path releases
	// the real reservation rather than dropping it.
	return &beadsFence{inner: inner, target: scope}, acquireErr
}

// InspectFenced completes the descriptor from a private snapshot taken under
// the held fence, then reports it as this provider's six-class scope.
func (p *beadsProvider) InspectFenced(ctx context.Context, request storebinding.FencedInspectionRequest) (storebinding.Descriptor, error) {
	if err := request.Validate(ctx); err != nil {
		return storebinding.Descriptor{}, err
	}
	if request.Target.Provider != BeadsProviderID {
		return storebinding.Descriptor{}, fmt.Errorf("%w: provider %q", ErrInvalidBeadsBinding, request.Target.Provider)
	}
	operation := &beadsFenceInspectionOperation{
		target:             request.Target.Clone(),
		expectedGeneration: request.ExpectedGeneration,
		spec:               p.component,
	}
	if err := storebinding.InspectProviderFence(ctx, request.Fence, operation); err != nil {
		return storebinding.Descriptor{}, err
	}
	return operation.descriptor.Clone(), nil
}

// RetainedGuards reports no retained-guard lifecycle: this provider installs
// and recovers no migration guard.
func (p *beadsProvider) RetainedGuards() (storebinding.RetainedGuardLifecycle, bool) {
	return nil, false
}

// BindingMigration reports no binding-migration lifecycle: this provider
// activates no generation, which is also why Open refuses the migration modes.
func (p *beadsProvider) BindingMigration() (storebinding.BindingMigrationLifecycle, bool) {
	return nil, false
}

// WorkMigration reports no Work-migration lifecycle.
func (p *beadsProvider) WorkMigration() (storebinding.WorkMigrationLifecycle, bool) {
	return nil, false
}

// boundTo refuses a specification other than the one this facade was built
// for. A provider facade is bound to exactly one binding; answering for a
// second would silently inspect a database the caller did not name.
func (p *beadsProvider) boundTo(spec storebinding.BindingSpec) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	if spec.Provider != BeadsProviderID {
		return fmt.Errorf("%w: provider %q", ErrInvalidBeadsBinding, spec.Provider)
	}
	path, err := GraphPath(BindingRoot(spec))
	if err != nil {
		return err
	}
	if spec.Name != p.spec.Name || spec.ConfigRef != p.spec.ConfigRef || path != p.path {
		return fmt.Errorf("%w: specification does not match the bound binding %q", ErrInvalidBeadsBinding, p.spec.Name)
	}
	return nil
}

// beadsClasses is the complete set of classes one Beads ledger serves.
func beadsClasses() (storebinding.ClassSet, error) {
	return storebinding.NewClassSet(
		coordclass.ClassWork,
		coordclass.ClassGraph,
		coordclass.ClassSessions,
		coordclass.ClassMessaging,
		coordclass.ClassOrders,
		coordclass.ClassNudges,
	)
}

// beadsCapabilities is the honest capability declaration for the deployed
// SQLite engine behind every class front door.
//
// Transactions and Claims are engine facts, and one engine serves all six
// classes, so they are declared uniformly and Open proves both against the
// opened store before any front door escapes. Nothing here declares a
// capability whose closed class contract has no operation for it, because
// ValidateRequired would then admit a caller that asked for something the
// front door cannot express.
func beadsCapabilities(writerFencing bool) storebinding.ClassCapabilities {
	engine := storebinding.ClassCapability{Available: true, Transactions: true, Claims: true}
	return storebinding.ClassCapabilities{
		Work:          engine,
		Graph:         engine,
		Sessions:      engine,
		Messaging:     engine,
		Orders:        engine,
		Nudges:        engine,
		WriterFencing: writerFencing,
	}
}

// widenBeadsTarget re-expresses the component's mutation-free fence target as
// this provider's six-class scope. Every physical fact — locator, identity,
// component ID — is carried through untouched.
func widenBeadsTarget(component storebinding.FenceTarget) (storebinding.FenceTarget, error) {
	classes, err := beadsClasses()
	if err != nil {
		return storebinding.FenceTarget{}, err
	}
	if component.Provider != ProviderID || len(component.Components) != 1 {
		return storebinding.FenceTarget{}, fmt.Errorf("%w: component target is not one SQLite component", ErrInvalidBeadsBinding)
	}
	widened := component.Components[0]
	widened.Classes = classes
	return storebinding.NewFenceTarget(BeadsProviderID, classes, []storebinding.FenceComponentTarget{widened})
}

// narrowBeadsTarget is the exact inverse of widenBeadsTarget.
func narrowBeadsTarget(scope storebinding.FenceTarget) (storebinding.FenceTarget, error) {
	classes, err := beadsClasses()
	if err != nil {
		return storebinding.FenceTarget{}, err
	}
	componentClasses, err := graphClasses()
	if err != nil {
		return storebinding.FenceTarget{}, err
	}
	if scope.Provider != BeadsProviderID || !scope.Classes.Equal(classes) || len(scope.Components) != 1 {
		return storebinding.FenceTarget{}, fmt.Errorf("%w: target is not one six-class Beads scope", ErrInvalidBeadsBinding)
	}
	narrowed := scope.Components[0]
	if narrowed.ID != GraphComponentID || !narrowed.Classes.Equal(classes) {
		return storebinding.FenceTarget{}, fmt.Errorf("%w: component %q", ErrInvalidBeadsBinding, narrowed.ID)
	}
	narrowed.Classes = componentClasses
	return storebinding.NewFenceTarget(ProviderID, componentClasses, []storebinding.FenceComponentTarget{narrowed})
}

// widenBeadsDescriptor re-expresses the component descriptor as this
// provider's. The provider ID, the semantic contract, the component's class
// set, and the capability declaration are the only changed values; the
// observed physical facts and the configuration digest are carried through, so
// both inspection routes produce byte-identical descriptor identities.
func widenBeadsDescriptor(component storebinding.Descriptor) (storebinding.Descriptor, error) {
	classes, err := beadsClasses()
	if err != nil {
		return storebinding.Descriptor{}, err
	}
	if component.Provider != ProviderID || len(component.Components) != 1 {
		return storebinding.Descriptor{}, fmt.Errorf("%w: component descriptor is not one SQLite component", ErrInvalidBeadsBinding)
	}
	if component.RetainedSource != nil {
		return storebinding.Descriptor{}, fmt.Errorf("%w: a retained component source cannot be re-scoped", ErrInvalidBeadsBinding)
	}
	widened := component.Clone()
	widened.Provider = BeadsProviderID
	widened.SemanticContractVersion = beadsContract
	widened.Components[0].Classes = classes
	widened.Capabilities = beadsCapabilities(component.Capabilities.WriterFencing)
	return storebinding.NewDescriptor(widened)
}

// beadsFence re-expresses the component's held reservation as this provider's
// scope. It owns no reservation of its own and adds no cleanup authority: the
// inner fence remains the sole owner of the operating-system exclusion and of
// the migration-guard claim.
type beadsFence struct {
	inner  storebinding.WriterFence
	target storebinding.FenceTarget
}

// Target returns this provider's six-class view of the reserved component.
func (f *beadsFence) Target() storebinding.FenceTarget { return f.target.Clone() }

// Role returns the closed protocol role of the underlying reservation.
func (f *beadsFence) Role() storebinding.FenceRole { return f.inner.Role() }

// Generation returns the durable generation the reservation was taken under.
func (f *beadsFence) Generation() storebinding.Generation { return f.inner.Generation() }

// CoveredComponents returns the components the underlying reservation covers.
func (f *beadsFence) CoveredComponents() []storebinding.ComponentID {
	return f.inner.CoveredComponents()
}

// Held reports whether the underlying reservation still excludes writers.
func (f *beadsFence) Held(ctx context.Context) (bool, error) { return f.inner.Held(ctx) }

// Release releases the underlying reservation. It is idempotent because the
// inner fence is.
func (f *beadsFence) Release(ctx context.Context) error { return f.inner.Release(ctx) }

// ExecuteProviderFenceOperation runs this provider's fenced census by handing
// the component its own inert operation. The component fence performs every
// held check, takes the private snapshot, and censuses it; this method only
// translates the target in and the descriptor out.
func (f *beadsFence) ExecuteProviderFenceOperation(ctx context.Context, projection storebinding.FenceProjection, operation storebinding.ProviderFenceOperation) error {
	if f == nil || projection != beadsFenceProjection {
		return storebinding.ErrInvalidFence
	}
	inspection, ok := operation.(*beadsFenceInspectionOperation)
	if !ok || inspection == nil {
		return storebinding.ErrInvalidFence
	}
	if !inspection.target.Equal(f.target) || inspection.expectedGeneration != f.inner.Generation() {
		return storebinding.ErrInvalidFence
	}
	target, err := narrowBeadsTarget(inspection.target)
	if err != nil {
		return err
	}
	executor, ok := f.inner.(interface {
		ExecuteProviderFenceOperation(context.Context, storebinding.FenceProjection, storebinding.ProviderFenceOperation) error
	})
	if !ok {
		return storebinding.ErrInvalidFence
	}
	component := &graphFenceInspectionOperation{
		target:             target,
		expectedGeneration: inspection.expectedGeneration,
		spec:               inspection.spec,
	}
	if err := executor.ExecuteProviderFenceOperation(ctx, graphFenceProjection, component); err != nil {
		return err
	}
	descriptor, err := widenBeadsDescriptor(component.descriptor)
	if err != nil {
		return err
	}
	inspection.descriptor = descriptor
	return nil
}

// beadsFenceInspectionOperation is inert fenced-inspection input. It carries
// no fence and no cleanup authority.
type beadsFenceInspectionOperation struct {
	target             storebinding.FenceTarget
	expectedGeneration storebinding.Generation
	spec               storebinding.BindingSpec
	descriptor         storebinding.Descriptor
}

// FenceProjection names this provider's private fenced operation.
func (*beadsFenceInspectionOperation) FenceProjection() storebinding.FenceProjection {
	return beadsFenceProjection
}

// Open opens the single Beads ledger the descriptor identifies and hands back
// all six class front doors projected from it.
//
// Three things are proved before the database is opened and one immediately
// after, and the order is the point. The descriptor is proved to be this
// provider's own; the component is proved not to have moved since the census,
// because nothing holds a fence during an active open; and a source open is
// proved to be answerable without recovering the artifact it is supposed to
// leave byte-intact. Only then is the engine opened, and only then are the
// declared capabilities proved against it — before a single front door
// escapes.
func (p *beadsProvider) Open(ctx context.Context, request storebinding.OpenRequest) (storebinding.OpenedBinding, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	descriptor := request.Descriptor.Clone()
	component, path, err := p.componentOf(descriptor)
	if err != nil {
		return nil, err
	}
	readOnly, err := beadsOpenIsReadOnly(request.Mode)
	if err != nil {
		return nil, err
	}
	if err := p.componentStillThere(ctx, path, component); err != nil {
		return nil, err
	}
	if readOnly {
		// A source open can neither recover a hot journal nor checkpoint a
		// WAL. If either still holds authoritative bytes, no read of this file
		// is answerable, and without this gate the open degrades into a
		// multi-second SQLITE_BUSY rather than saying so. The caller's route
		// is the fenced private snapshot it already has.
		if err := requireQuiescentSQLiteSource(path, "Beads", ErrBeadsLiveWAL); err != nil {
			return nil, err
		}
	}

	// This open is NOT fenced, and componentOf has just proved it is the same
	// database the serving seam fences: it refuses any descriptor resolving to a
	// path other than p.path, which is what OpenEngine (beads_engine.go) opens
	// with storebinding.EngineReservedPrefixes(classes). Only the mint prefix is
	// configured here, so a pinned id from any namespace is admitted through
	// this lifecycle door. That is tolerable only because it has no production
	// caller today; the first one has to thread the fence in too, or invariant
	// 16 (engdocs/architecture/beads.md) holds for the engine and not for the
	// database underneath it.
	options := []beads.SQLiteStoreOption{beads.WithSQLiteStoreIDPrefix(graphIDPrefix)}
	if readOnly {
		options = append(options, beads.WithSQLiteStoreReadOnly())
	}
	store, err := beads.OpenSQLiteStore(filepath.Dir(path), options...)
	if err != nil {
		return nil, fmt.Errorf("opening the SQLite Beads component: %w", err)
	}
	closer, isCloser := store.(interface{ CloseStore() error })
	if !isCloser {
		return nil, fmt.Errorf("%w: engine %T cannot release its physical handle", ErrInvalidBeadsBinding, store)
	}
	reject := func(cause error) (storebinding.OpenedBinding, error) {
		if err := closer.CloseStore(); err != nil {
			return nil, errors.Join(cause, fmt.Errorf("closing the refused SQLite Beads component: %w", err))
		}
		return nil, cause
	}

	if err := verifyBeadsCapabilities(store, descriptor.Capabilities); err != nil {
		return reject(err)
	}
	parts, err := p.frontDoors(store, descriptor, component)
	if err != nil {
		return reject(err)
	}
	if err := beadsWorkMatchesPin(request.PinnedWork, *parts.Work); err != nil {
		return reject(err)
	}
	parts.Handles = []storebinding.ComponentHandle{{Component: component.ID, Close: closer.CloseStore}}
	// NewOpenedBinding owns the handles from here: it closes them itself if it
	// rejects the parts, so this path must not close them again.
	return storebinding.NewOpenedBinding(parts)
}

// componentOf proves a descriptor is this provider's exact single-component
// Beads scope and returns its component and deployed database path.
func (p *beadsProvider) componentOf(descriptor storebinding.Descriptor) (storebinding.ComponentDescriptor, string, error) {
	classes, err := beadsClasses()
	if err != nil {
		return storebinding.ComponentDescriptor{}, "", err
	}
	if descriptor.Provider != BeadsProviderID || descriptor.SemanticContractVersion != beadsContract {
		return storebinding.ComponentDescriptor{}, "", fmt.Errorf("%w: descriptor is not this provider's", ErrInvalidBeadsBinding)
	}
	if len(descriptor.Components) != 1 || !descriptor.Components[0].Classes.Equal(classes) {
		return storebinding.ComponentDescriptor{}, "", fmt.Errorf("%w: descriptor is not one six-class component", ErrInvalidBeadsBinding)
	}
	// The capability declaration is this provider's to make, not the caller's
	// to supply. Admitting a hand-shaped one would let an under-declaration
	// through, and an under-declared binding silently refuses callers that ask
	// for a capability the engine actually has.
	if !descriptor.Capabilities.Equal(beadsCapabilities(descriptor.Capabilities.WriterFencing)) {
		return storebinding.ComponentDescriptor{}, "", fmt.Errorf("%w: descriptor capabilities are not this provider's declaration", ErrInvalidBeadsBinding)
	}
	component := descriptor.Components[0]
	target, err := narrowBeadsTarget(storebinding.FenceTarget{
		Version:  1,
		Provider: BeadsProviderID,
		Classes:  classes,
		Components: []storebinding.FenceComponentTarget{{
			ID:               component.ID,
			Locator:          component.Locator,
			PhysicalIdentity: component.PhysicalIdentity,
			Classes:          classes,
		}},
	})
	if err != nil {
		return storebinding.ComponentDescriptor{}, "", err
	}
	path, _, err := graphTargetPath(target)
	if err != nil {
		return storebinding.ComponentDescriptor{}, "", err
	}
	if path != p.path {
		return storebinding.ComponentDescriptor{}, "", fmt.Errorf("%w: descriptor names a component outside binding %q", ErrInvalidBeadsBinding, p.spec.Name)
	}
	return component, path, nil
}

// componentStillThere re-observes the component and refuses an open whose
// descriptor no longer describes what is on disk.
//
// The census this runs is the same mutation-free one that produced the
// descriptor, and it necessarily runs BEFORE the engine opens: a WAL-mode open
// creates sidecars in the component directory, and the directory is part of
// the identity, so a post-open re-observation would report a move that the
// open itself caused.
func (p *beadsProvider) componentStillThere(ctx context.Context, path string, component storebinding.ComponentDescriptor) error {
	state, err := captureGraphSourceContext(ctx, path)
	if err != nil {
		return graphFenceSourceCaptureError(err)
	}
	if !state.database.Present || graphComponentIdentity(path, state) != component.PhysicalIdentity {
		return &storebinding.FenceTargetMovedError{Component: component.ID}
	}
	return nil
}

// frontDoors projects the opened store into every class front door the
// descriptor declares.
func (p *beadsProvider) frontDoors(store beads.Store, descriptor storebinding.Descriptor, component storebinding.ComponentDescriptor) (storebinding.OpenedBindingParts, error) {
	identity := storebinding.BeadsAdapterIdentity{
		OpenerID:    string(BeadsProviderID),
		ComponentID: string(component.ID),
		PhysicalID:  string(component.PhysicalIdentity),
	}
	queue, err := storebinding.NewBeadsNudgeQueue(beads.NudgesStore{Store: store})
	if err != nil {
		return storebinding.OpenedBindingParts{}, fmt.Errorf("binding the SQLite Beads nudge queue: %w", err)
	}
	adapters, err := storebinding.NewBeadsAdapters(store, identity, queue)
	if err != nil {
		return storebinding.OpenedBindingParts{}, fmt.Errorf("projecting the SQLite Beads class front doors: %w", err)
	}
	// Messaging is handed over unbound: only the composition layer knows which
	// Sessions address directory this city's mail resolves against.
	messaging, err := storebinding.BindBeadsMessaging(store)
	if err != nil {
		return storebinding.OpenedBindingParts{}, err
	}
	work, err := storebinding.NewWorkTopology(storebinding.Workspace{
		Scope:       storebinding.HQScope(),
		Store:       adapters.Work,
		Prefix:      graphIDPrefix,
		OpenerID:    identity.OpenerID,
		ComponentID: identity.ComponentID,
		PhysicalID:  identity.PhysicalID,
	}, nil)
	if err != nil {
		return storebinding.OpenedBindingParts{}, fmt.Errorf("binding the SQLite Beads Work topology: %w", err)
	}
	nudges := adapters.Nudges
	return storebinding.OpenedBindingParts{
		Descriptor:   descriptor,
		Capabilities: descriptor.Capabilities,
		Work:         &work,
		Graph:        adapters.Graph,
		Sessions:     adapters.Sessions,
		Messaging:    messaging,
		Orders:       adapters.Orders,
		Nudges:       &nudges,
	}, nil
}

// beadsOpenIsReadOnly maps an open mode onto this provider's two supported
// physical opens and refuses the modes it has no lifecycle for.
func beadsOpenIsReadOnly(mode storebinding.OpenMode) (bool, error) {
	switch mode {
	case storebinding.OpenModeActive:
		return false, nil
	case storebinding.OpenModeReadOnlySource:
		return true, nil
	default:
		// Both refused modes belong to a migration this provider declares no
		// lifecycle for (BindingMigration and WorkMigration are both absent).
		// Opening for one anyway would let a migration begin against a
		// participant that can never activate or hand back a receipt.
		return false, fmt.Errorf("%w: this provider declares no migration lifecycle", storebinding.ErrInvalidOpenMode)
	}
}

// verifyBeadsCapabilities proves the opened engine has every capability the
// descriptor declares, before any front door escapes.
//
// One engine serves all six classes, so a declared transaction or claim is one
// engine fact asserted six times, and every assertion is checked. A store that
// declares a capability it does not have must be refused here: the alternative
// is a caller that learns mid-claim that its single-winner guarantee was never
// there.
func verifyBeadsCapabilities(store beads.Store, declared storebinding.ClassCapabilities) error {
	if declared.WriterFencing && !sqliteWriterFencingSupported() {
		return fmt.Errorf("%w: writer fencing", ErrBeadsCapabilityUndeclared)
	}
	transactional := beads.StoreSupportsAtomicTx(store)
	_, claimable := store.(interface {
		Claim(id, assignee string) (beads.Bead, bool, error)
	})
	for _, class := range coordclass.Classes() {
		capability := declared.For(class)
		if !capability.Available {
			continue
		}
		if capability.Transactions && !transactional {
			return fmt.Errorf("%w: atomic transactions for %s", ErrBeadsCapabilityUndeclared, class)
		}
		if capability.Claims && !claimable {
			return fmt.Errorf("%w: compare-and-swap claims for %s", ErrBeadsCapabilityUndeclared, class)
		}
	}
	return nil
}

// beadsWorkMatchesPin refuses a pinned Work topology this provider cannot
// serve. One Beads ledger is one physical workspace, so the only topology it
// can honor is the single HQ scope it opens.
func beadsWorkMatchesPin(pinned *storebinding.WorkTopology, opened storebinding.WorkTopology) error {
	if pinned == nil {
		return nil
	}
	pinnedWorkspaces := pinned.All()
	openedWorkspaces := opened.All()
	if len(pinnedWorkspaces) != len(openedWorkspaces) {
		return fmt.Errorf("%w: this provider opens exactly one Work scope", storebinding.ErrInvalidWorkParticipant)
	}
	for index, workspace := range pinnedWorkspaces {
		actual := openedWorkspaces[index]
		if workspace.Scope != actual.Scope || workspace.Prefix != actual.Prefix || workspace.Suspended != actual.Suspended ||
			workspace.OpenerID != actual.OpenerID || workspace.ComponentID != actual.ComponentID || workspace.PhysicalID != actual.PhysicalID {
			return fmt.Errorf("%w: pinned Work scope %s is not the scope this binding opens", storebinding.ErrInvalidWorkParticipant, workspace.Scope)
		}
	}
	return nil
}
