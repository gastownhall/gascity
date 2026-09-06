package storebinding

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// ReservedWorkBinding is the immutable synthesized binding backed by the pinned
// bootstrap Work topology. It is never defined by configuration.
const ReservedWorkBinding BindingName = config.StorageWorkBinding

var (
	// ErrInvalidStoragePlan reports a desired resolution that cannot be frozen
	// into one exact plan.
	ErrInvalidStoragePlan = errors.New("invalid storage plan")
	// ErrMissingClassAssignment reports a semantic class with no binding after
	// config layering.
	ErrMissingClassAssignment = errors.New("missing storage class assignment")
	// ErrUndefinedBinding reports a class assignment naming a binding no layer
	// defines.
	ErrUndefinedBinding = errors.New("undefined storage binding")
	// ErrReservedBindingDefined reports an explicit [storage.bindings.work]
	// definition of the reserved binding.
	ErrReservedBindingDefined = errors.New("reserved storage binding cannot be defined")
	// ErrUnreferencedBinding reports a defined binding no class selects. An
	// unreferenced binding would resolve a provider nobody opens, owns, or
	// closes, so planning rejects it instead of carrying dead weight.
	ErrUnreferencedBinding = errors.New("unreferenced storage binding")
)

// CompositeConfigDigest is the secret-free canonical digest of a complete
// frozen plan's configuration facts.
type CompositeConfigDigest string

// DeferredBindKind is the closed set of front doors that cannot be completed at
// open time.
type DeferredBindKind uint8

const (
	// DeferredBindMessagingSessions binds the opened Messaging persistence edge
	// to the selected Sessions address directory. activation alone executes it.
	DeferredBindMessagingSessions DeferredBindKind = iota + 1
	// DeferredBindOrdersGraph injects the selected GraphStore while composing
	// the OrdersStore, so HasOpenWork never sees a raw bead store.
	DeferredBindOrdersGraph
)

// Valid reports whether kind belongs to the closed deferred-bind set.
func (k DeferredBindKind) Valid() bool {
	return k == DeferredBindMessagingSessions || k == DeferredBindOrdersGraph
}

// String returns a stable diagnostic name.
func (k DeferredBindKind) String() string {
	switch k {
	case DeferredBindMessagingSessions:
		return "messaging-sessions"
	case DeferredBindOrdersGraph:
		return "orders-graph"
	default:
		return "unknown"
	}
}

// DeferredBind is one planned composition step, not a taken handle.
type DeferredBind struct {
	Kind     DeferredBindKind
	Consumer BindingName
	Supplier BindingName
}

// NudgeQueueAuthority names which queue front the builder must find on the
// nudges binding. Moving that authority is a migration, never a plan-time
// choice.
type NudgeQueueAuthority uint8

const (
	// NudgeQueueBindingSupplied means the assigned binding owns the durable queue.
	NudgeQueueBindingSupplied NudgeQueueAuthority = iota + 1
	// NudgeQueueRetainedLegacy means the retained legacy queue authority remains
	// in place behind the reserved work binding, with the Work-backed shadow
	// projection beside it.
	NudgeQueueRetainedLegacy
)

// String returns a stable diagnostic name.
func (a NudgeQueueAuthority) String() string {
	switch a {
	case NudgeQueueBindingSupplied:
		return "binding-supplied"
	case NudgeQueueRetainedLegacy:
		return "retained-legacy"
	default:
		return "unknown"
	}
}

// PlannedBinding is one distinct explicit binding: exactly one registry lookup,
// exactly one provider construction, and every class it serves.
//
// Component compatibility is deliberately absent: format, schema, and ABI facts
// live in a descriptor, and plan resolution opens and inspects nothing. M-series phases
// derive the complete pinned set from their durable descriptor through
// PinnedComponentRequirements.
type PlannedBinding struct {
	Name            BindingName
	Spec            BindingSpec
	ProviderID      ProviderID
	Provider        Provider
	AssignedClasses ClassSet
	Requirements    []ClassCapabilityRequirement
	ConfigDigest    ConfigRefDigest
	OpenRank        int
}

// Clone returns a detached planned binding. The resolved provider facade owns
// nothing and is shared deliberately so no later phase re-resolves a binding
// name.
func (b PlannedBinding) Clone() PlannedBinding {
	out := b
	out.Requirements = append([]ClassCapabilityRequirement(nil), b.Requirements...)
	return out
}

// PlannedOpen is one participant of the frozen open program.
type PlannedOpen struct {
	Binding         BindingName
	Reserved        bool
	Provider        Provider
	Spec            BindingSpec
	AssignedClasses ClassSet
	Requirements    []ClassCapabilityRequirement
	PinnedWork      bool
	Rank            int
}

// Clone returns a detached planned open step.
func (o PlannedOpen) Clone() PlannedOpen {
	out := o
	out.Requirements = append([]ClassCapabilityRequirement(nil), o.Requirements...)
	return out
}

// OpenRequestTemplate completes the frozen open step with the durable facts activation
// holds: the active descriptor, its generation, and the durable active-open
// authority minted from the active manifest. Mode is always OpenModeActive:
// plan resolution plans no source, destination, or retained open.
func (o PlannedOpen) OpenRequestTemplate(descriptor Descriptor, generation Generation, authority DurableActiveOpenAuthority, pinnedWork *WorkTopology) OpenRequest {
	return OpenRequest{
		Descriptor:             descriptor.Clone(),
		AssignedClasses:        o.AssignedClasses,
		Mode:                   OpenModeActive,
		ExpectedGeneration:     generation,
		PinnedWork:             pinnedWork,
		ExpectedContract:       descriptor.SemanticContractVersion,
		ExpectedComponents:     PinnedComponentRequirements(descriptor),
		ClassRequirements:      append([]ClassCapabilityRequirement(nil), o.Requirements...),
		DurableActiveAuthority: authority,
	}
}

// PinnedComponentRequirements pins the complete physical compatibility facts of
// one durable descriptor, in canonical component order.
func PinnedComponentRequirements(descriptor Descriptor) []ComponentCompatibilityRequirement {
	components := append([]ComponentDescriptor(nil), descriptor.Components...)
	sort.Slice(components, func(i, j int) bool { return components[i].ID < components[j].ID })
	requirements := make([]ComponentCompatibilityRequirement, 0, len(components))
	for _, component := range components {
		requirements = append(requirements, ComponentCompatibilityRequirement{
			Component:     component.ID,
			Format:        component.Format,
			SchemaVersion: component.SchemaVersion,
			ABIVersion:    component.ABIVersion,
		})
	}
	return requirements
}

// PlannedClose is one participant of the frozen close program, executed in
// exact reverse open order by the activation owner after all front doors
// stop.
type PlannedClose struct {
	Binding  BindingName
	Reserved bool
	Rank     int
	// PinnedWork annotates the reserved binding with the pinned physical
	// workspaces it owns. Explicit bindings own descriptor-declared components,
	// which binding validation already forbids two binding names from sharing.
	PinnedWork []PinnedPhysicalGroup
}

// Clone returns a detached planned close step.
func (c PlannedClose) Clone() PlannedClose {
	out := c
	out.PinnedWork = make([]PinnedPhysicalGroup, len(c.PinnedWork))
	for index, group := range c.PinnedWork {
		out.PinnedWork[index] = group.Clone()
	}
	return out
}

// PlannedInspection is one participant of the mutation-free inspection program
// the fenced inspection step executes. Fence targets are discovered by that inspection, never planned.
type PlannedInspection struct {
	Binding         BindingName
	Provider        Provider
	Spec            BindingSpec
	AssignedClasses ClassSet
	Rank            int
}

// StoragePlan is the frozen desired resolution and open plan. It is formed once
// per process from a frozen registry and layered config, before any provider
// mutation, and it holds no handle: the only provider method planning may call
// is the acquisition-free ProviderFactory.New.
type StoragePlan struct {
	assignments    map[coordclass.Class]BindingName
	bindings       []PlannedBinding
	workPlan       WorkTopologyPlan
	deferred       []DeferredBind
	inspections    []PlannedInspection
	openProgram    []PlannedOpen
	closeProgram   []PlannedClose
	nudgeAuthority NudgeQueueAuthority
	configDigest   CompositeConfigDigest
}

// providerLookup is the exact frozen-registry surface planning uses. Resolution
// performs one lookup per referenced provider ID and constructs each binding
// from that resolved factory, so no binding costs a second lookup.
type providerLookup interface {
	Lookup(ProviderID) (ProviderFactory, error)
}

// ResolveStoragePlan freezes one exact class-to-binding resolution and open
// plan. It opens nothing, mutates nothing, and fails whole: any unknown class,
// missing assignment, undefined or unreferenced binding, reserved-binding
// definition, unknown provider, invalid specification, unsupported class
// capability, or aliased Work pin is rejected before a single provider
// mutation, with nothing to unwind beyond discarding pure facade values.
//
// cityRoot is the absolute directory of the city the plan is resolved for, and
// it is stamped into every binding specification. It is a parameter rather
// than something read off the Work pins because the pins deliberately carry a
// DIGEST of each scope's root, not the root: a digest identifies a workspace
// and cannot be resolved back into a path. An empty cityRoot is legal and
// leaves the specifications unstamped; a provider that needs one then refuses.
func ResolveStoragePlan(registry *ProviderRegistry, storage config.StorageConfig, work WorkPinInputs, cityRoot string) (*StoragePlan, error) {
	if registry == nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidStoragePlan, ErrProviderUnavailable)
	}
	if !registryFrozen(registry) {
		return nil, ErrProviderRegistryNotFrozen
	}
	return resolveStoragePlan(registry, storage, work, cityRoot)
}

func registryFrozen(registry *ProviderRegistry) bool {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	return registry.frozen
}

func resolveStoragePlan(lookup providerLookup, storage config.StorageConfig, work WorkPinInputs, cityRoot string) (*StoragePlan, error) {
	if lookup == nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidStoragePlan, ErrProviderUnavailable)
	}
	assignments, err := resolveAssignments(storage)
	if err != nil {
		return nil, err
	}
	if _, defined := storage.Bindings[string(ReservedWorkBinding)]; defined {
		return nil, fmt.Errorf("%w: %w", ErrInvalidStoragePlan, ErrReservedBindingDefined)
	}
	specs, err := resolveBindingSpecs(storage, assignments, cityRoot)
	if err != nil {
		return nil, err
	}
	providers, err := resolveProviders(lookup, specs)
	if err != nil {
		return nil, err
	}
	workPlan, err := newWorkTopologyPlan(work)
	if err != nil {
		return nil, err
	}
	plan := &StoragePlan{assignments: assignments, workPlan: workPlan}
	for _, spec := range specs {
		classes := assignedClassSet(assignments, spec.Name)
		provider := providers[spec.Name]
		capabilities, err := plannedRequirements(classes)
		if err != nil {
			return nil, err
		}
		plan.bindings = append(plan.bindings, PlannedBinding{
			Name:            spec.Name,
			Spec:            spec,
			ProviderID:      spec.Provider,
			Provider:        provider,
			AssignedClasses: classes,
			Requirements:    capabilities,
			ConfigDigest:    bindingConfigDigest(spec),
			OpenRank:        openRank(classes),
		})
	}
	sort.SliceStable(plan.bindings, func(i, j int) bool {
		if plan.bindings[i].OpenRank != plan.bindings[j].OpenRank {
			return plan.bindings[i].OpenRank < plan.bindings[j].OpenRank
		}
		return plan.bindings[i].Name < plan.bindings[j].Name
	})
	if err := plan.buildPrograms(); err != nil {
		return nil, err
	}
	plan.nudgeAuthority = NudgeQueueBindingSupplied
	if assignments[coordclass.ClassNudges] == ReservedWorkBinding {
		plan.nudgeAuthority = NudgeQueueRetainedLegacy
	}
	plan.configDigest = compositeConfigDigest(plan)
	if err := plan.validateExact(); err != nil {
		return nil, err
	}
	return plan, nil
}

func resolveAssignments(storage config.StorageConfig) (map[coordclass.Class]BindingName, error) {
	assignments := make(map[coordclass.Class]BindingName, len(planClassOrder()))
	for _, class := range planClassOrder() {
		binding := storage.Classes.BindingFor(storageClassFor(class))
		if binding == "" {
			return nil, fmt.Errorf("%w: %w: %s", ErrInvalidStoragePlan, ErrMissingClassAssignment, class)
		}
		if binding != string(ReservedWorkBinding) {
			if _, defined := storage.Bindings[binding]; !defined {
				return nil, fmt.Errorf("%w: %w: class %s names %q", ErrInvalidStoragePlan, ErrUndefinedBinding, class, binding)
			}
		}
		assignments[class] = BindingName(binding)
	}
	return assignments, nil
}

// resolveBindingSpecs validates every defined binding and rejects one no class
// selects, in lexical binding-name order so failures are deterministic.
func resolveBindingSpecs(storage config.StorageConfig, assignments map[coordclass.Class]BindingName, cityRoot string) ([]BindingSpec, error) {
	names := make([]string, 0, len(storage.Bindings))
	for name := range storage.Bindings {
		names = append(names, name)
	}
	sort.Strings(names)
	specs := make([]BindingSpec, 0, len(names))
	for _, name := range names {
		binding := storage.Bindings[name]
		spec := BindingSpec{
			Name:      BindingName(name),
			Provider:  ProviderID(binding.Provider),
			Path:      binding.Path,
			ConfigRef: ConfigRef(binding.ConfigRef),
			CityRoot:  cityRoot,
			URL:       binding.URL,
			Auth:      binding.Auth,
		}
		if err := spec.Validate(); err != nil {
			return nil, fmt.Errorf("%w: binding %q: %w", ErrInvalidStoragePlan, name, err)
		}
		if assignedClassSet(assignments, spec.Name).Empty() {
			return nil, fmt.Errorf("%w: %w: %q", ErrInvalidStoragePlan, ErrUnreferencedBinding, name)
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

// resolveProviders performs exactly one registry lookup per referenced provider
// ID, in first-referenced order, and stops at the first failure. Every distinct
// binding then constructs its own provider from its own specification: two
// bindings may share a provider ID while owning different provider-owned
// configuration, so one facade must never be reused across binding names.
func resolveProviders(lookup providerLookup, specs []BindingSpec) (map[BindingName]Provider, error) {
	factories := make(map[ProviderID]ProviderFactory, len(specs))
	for _, spec := range specs {
		if _, resolved := factories[spec.Provider]; resolved {
			continue
		}
		factory, err := lookup.Lookup(spec.Provider)
		if err != nil {
			return nil, fmt.Errorf("resolving storage provider %q: %w", spec.Provider, err)
		}
		if isNilInterface(factory) {
			return nil, fmt.Errorf("%w: provider %q resolved to no factory", ErrProviderUnavailable, spec.Provider)
		}
		if factory.ID() != spec.Provider {
			return nil, fmt.Errorf("%w: provider %q resolved to factory %q", ErrUnknownProvider, spec.Provider, factory.ID())
		}
		factories[spec.Provider] = factory
	}
	providers := make(map[BindingName]Provider, len(specs))
	for _, spec := range specs {
		provider, err := newPlannedProvider(factories[spec.Provider], spec)
		if err != nil {
			return nil, err
		}
		providers[spec.Name] = provider
	}
	return providers, nil
}

// newPlannedProvider mirrors ProviderRegistry.New's factory contract checks
// without paying a second registry lookup for an already resolved factory.
func newPlannedProvider(factory ProviderFactory, spec BindingSpec) (Provider, error) {
	provider, err := factory.New(spec)
	if err != nil {
		if !isNilInterface(provider) {
			return nil, fmt.Errorf("%w: factory returned a provider with an error", ErrProviderFactoryContract)
		}
		return nil, fmt.Errorf("constructing storage provider %q: %w", spec.Provider, err)
	}
	if isNilInterface(provider) {
		return nil, fmt.Errorf("%w: provider %q returned nil", ErrProviderUnavailable, spec.Provider)
	}
	return provider, nil
}

func (p *StoragePlan) buildPrograms() error {
	reservedClasses := assignedClassSet(p.assignments, ReservedWorkBinding)
	if !reservedClasses.Empty() {
		requirements, err := plannedRequirements(reservedClasses)
		if err != nil {
			return err
		}
		p.openProgram = append(p.openProgram, PlannedOpen{
			Binding:         ReservedWorkBinding,
			Reserved:        true,
			AssignedClasses: reservedClasses,
			Requirements:    requirements,
			PinnedWork:      reservedClasses.HasWork(),
			Rank:            0,
		})
	}
	for _, binding := range p.bindings {
		p.openProgram = append(p.openProgram, PlannedOpen{
			Binding:         binding.Name,
			Provider:        binding.Provider,
			Spec:            binding.Spec,
			AssignedClasses: binding.AssignedClasses,
			Requirements:    append([]ClassCapabilityRequirement(nil), binding.Requirements...),
			Rank:            len(p.openProgram),
		})
	}
	p.closeProgram = make([]PlannedClose, 0, len(p.openProgram))
	for index := len(p.openProgram) - 1; index >= 0; index-- {
		step := p.openProgram[index]
		closeStep := PlannedClose{
			Binding:  step.Binding,
			Reserved: step.Reserved,
			Rank:     len(p.closeProgram),
		}
		if step.Reserved {
			closeStep.PinnedWork = p.workPlan.Physical()
		}
		p.closeProgram = append(p.closeProgram, closeStep)
	}
	for _, binding := range sortedByInspectionRank(p.bindings) {
		p.inspections = append(p.inspections, PlannedInspection{
			Binding:         binding.Name,
			Provider:        binding.Provider,
			Spec:            binding.Spec,
			AssignedClasses: binding.AssignedClasses,
			Rank:            len(p.inspections),
		})
	}
	p.deferred = append(p.deferred, DeferredBind{
		Kind:     DeferredBindMessagingSessions,
		Consumer: p.assignments[coordclass.ClassMessaging],
		Supplier: p.assignments[coordclass.ClassSessions],
	})
	if p.assignments[coordclass.ClassOrders] != p.assignments[coordclass.ClassGraph] {
		p.deferred = append(p.deferred, DeferredBind{
			Kind:     DeferredBindOrdersGraph,
			Consumer: p.assignments[coordclass.ClassOrders],
			Supplier: p.assignments[coordclass.ClassGraph],
		})
	}
	return nil
}

func sortedByInspectionRank(bindings []PlannedBinding) []PlannedBinding {
	out := append([]PlannedBinding(nil), bindings...)
	sort.SliceStable(out, func(i, j int) bool {
		left, right := inspectionRank(out[i].AssignedClasses), inspectionRank(out[j].AssignedClasses)
		if left != right {
			return left < right
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// validateExact proves the plan names an exact route for every class: no class
// without a planned binding, no binding without a class, no participant that is
// not both opened and closed, and no deferred step naming an unplanned binding.
func (p *StoragePlan) validateExact() error {
	planned := make(map[BindingName]struct{}, len(p.bindings)+1)
	for _, binding := range p.bindings {
		if _, duplicate := planned[binding.Name]; duplicate {
			return fmt.Errorf("%w: duplicate binding %q", ErrInvalidStoragePlan, binding.Name)
		}
		planned[binding.Name] = struct{}{}
	}
	referenced := make(map[BindingName]struct{}, len(planned)+1)
	for _, class := range planClassOrder() {
		binding, assigned := p.assignments[class]
		if !assigned || binding == "" {
			return fmt.Errorf("%w: %w: %s", ErrInvalidStoragePlan, ErrMissingClassAssignment, class)
		}
		if binding != ReservedWorkBinding {
			if _, exists := planned[binding]; !exists {
				return fmt.Errorf("%w: %w: class %s names %q", ErrInvalidStoragePlan, ErrUndefinedBinding, class, binding)
			}
		}
		referenced[binding] = struct{}{}
	}
	if len(p.assignments) != len(planClassOrder()) {
		return fmt.Errorf("%w: %d class assignments", ErrInvalidStoragePlan, len(p.assignments))
	}
	for _, binding := range p.bindings {
		if _, used := referenced[binding.Name]; !used {
			return fmt.Errorf("%w: %w: %q", ErrInvalidStoragePlan, ErrUnreferencedBinding, binding.Name)
		}
	}
	if err := p.validatePrograms(referenced); err != nil {
		return err
	}
	if err := p.validateWorkParticipants(); err != nil {
		return err
	}
	for _, deferred := range p.deferred {
		if !deferred.Kind.Valid() {
			return fmt.Errorf("%w: unknown deferred bind", ErrInvalidStoragePlan)
		}
		for _, name := range []BindingName{deferred.Consumer, deferred.Supplier} {
			if _, exists := planned[name]; !exists && name != ReservedWorkBinding {
				return fmt.Errorf("%w: deferred %s names unplanned binding %q", ErrInvalidStoragePlan, deferred.Kind, name)
			}
		}
	}
	return nil
}

func (p *StoragePlan) validatePrograms(referenced map[BindingName]struct{}) error {
	opened := make(map[BindingName]int, len(p.openProgram))
	for index, step := range p.openProgram {
		if step.Rank != index {
			return fmt.Errorf("%w: open program rank %d at position %d", ErrInvalidStoragePlan, step.Rank, index)
		}
		if _, duplicate := opened[step.Binding]; duplicate {
			return fmt.Errorf("%w: binding %q opens twice", ErrInvalidStoragePlan, step.Binding)
		}
		if step.AssignedClasses.Empty() {
			return fmt.Errorf("%w: binding %q opens for no class", ErrInvalidStoragePlan, step.Binding)
		}
		hasProvider := !isNilInterface(step.Provider)
		if step.Reserved == hasProvider {
			return fmt.Errorf("%w: binding %q has no exact provider", ErrInvalidStoragePlan, step.Binding)
		}
		opened[step.Binding] = index
	}
	if len(opened) != len(referenced) {
		return fmt.Errorf("%w: %d open participants for %d referenced bindings", ErrInvalidStoragePlan, len(opened), len(referenced))
	}
	for name := range referenced {
		if _, exists := opened[name]; !exists {
			return fmt.Errorf("%w: referenced binding %q never opens", ErrInvalidStoragePlan, name)
		}
	}
	if len(p.closeProgram) != len(p.openProgram) {
		return fmt.Errorf("%w: %d close steps for %d open steps", ErrInvalidStoragePlan, len(p.closeProgram), len(p.openProgram))
	}
	for index, step := range p.closeProgram {
		if step.Rank != index {
			return fmt.Errorf("%w: close program rank %d at position %d", ErrInvalidStoragePlan, step.Rank, index)
		}
		expected := p.openProgram[len(p.openProgram)-1-index]
		if step.Binding != expected.Binding || step.Reserved != expected.Reserved {
			return fmt.Errorf("%w: close program is not the reverse of the open program", ErrInvalidStoragePlan)
		}
	}
	if len(p.inspections) != len(p.bindings) {
		return fmt.Errorf("%w: %d inspection steps for %d explicit bindings", ErrInvalidStoragePlan, len(p.inspections), len(p.bindings))
	}
	return nil
}

// validateWorkParticipants proves every distinct pinned physical Work workspace
// appears exactly once, so one physical workspace migrates and closes once.
func (p *StoragePlan) validateWorkParticipants() error {
	participants, err := p.workPlan.Participants()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidStoragePlan, err)
	}
	if len(participants) != len(p.workPlan.physical) {
		return fmt.Errorf("%w: %d work participants for %d pinned physical workspaces", ErrInvalidStoragePlan, len(participants), len(p.workPlan.physical))
	}
	seen := make(map[string]struct{}, len(participants))
	members := 0
	for _, participant := range participants {
		if _, duplicate := seen[participant.Key()]; duplicate {
			return fmt.Errorf("%w: %w", ErrInvalidStoragePlan, ErrDuplicateWorkParticipant)
		}
		seen[participant.Key()] = struct{}{}
		members += len(participant.Members)
	}
	if members != len(p.workPlan.allInConfigOrder()) {
		return fmt.Errorf("%w: %d participant members for %d pinned scopes", ErrInvalidStoragePlan, members, len(p.workPlan.allInConfigOrder()))
	}
	return nil
}

// Assignments returns the complete class-to-binding assignment.
func (p *StoragePlan) Assignments() map[coordclass.Class]BindingName {
	out := make(map[coordclass.Class]BindingName, len(p.assignments))
	for class, binding := range p.assignments {
		out[class] = binding
	}
	return out
}

// BindingFor returns the binding assigned to one semantic class.
func (p *StoragePlan) BindingFor(class coordclass.Class) (BindingName, bool) {
	binding, assigned := p.assignments[class]
	return binding, assigned
}

// Bindings returns every distinct explicit binding in open order.
func (p *StoragePlan) Bindings() []PlannedBinding {
	out := make([]PlannedBinding, len(p.bindings))
	for index, binding := range p.bindings {
		out[index] = binding.Clone()
	}
	return out
}

// WorkPlan returns the pinned Work topology plan for the reserved binding.
func (p *StoragePlan) WorkPlan() WorkTopologyPlan { return p.workPlan }

// WorkParticipants returns one grouped migration participant per pinned
// physical Work workspace, the set intent derivation records at INTENT_FSYNCED.
func (p *StoragePlan) WorkParticipants() ([]WorkWorkspaceParticipant, error) {
	return p.workPlan.Participants()
}

// DeferredBinds returns the planned composition steps activation executes through the
// builder.
func (p *StoragePlan) DeferredBinds() []DeferredBind {
	return append([]DeferredBind(nil), p.deferred...)
}

// InspectionProgram returns the mutation-free inspection order the fenced inspection step executes.
func (p *StoragePlan) InspectionProgram() []PlannedInspection {
	return append([]PlannedInspection(nil), p.inspections...)
}

// OpenProgram returns the frozen open order activation executes: the reserved work
// binding first, so the bootstrap generation is addressable before anything
// that may retain or reference it.
func (p *StoragePlan) OpenProgram() []PlannedOpen {
	out := make([]PlannedOpen, len(p.openProgram))
	for index, step := range p.openProgram {
		out[index] = step.Clone()
	}
	return out
}

// CloseProgram returns the exact reverse of the open order.
func (p *StoragePlan) CloseProgram() []PlannedClose {
	out := make([]PlannedClose, len(p.closeProgram))
	for index, step := range p.closeProgram {
		out[index] = step.Clone()
	}
	return out
}

// NudgeQueueAuthority reports which queue front the nudges binding must supply.
func (p *StoragePlan) NudgeQueueAuthority() NudgeQueueAuthority { return p.nudgeAuthority }

// ConfigDigest returns the secret-free composite configuration digest.
func (p *StoragePlan) ConfigDigest() CompositeConfigDigest { return p.configDigest }

// BindingConfigDigests returns the secret-free per-binding configuration
// digests, including the reserved binding's pinned config context.
func (p *StoragePlan) BindingConfigDigests() map[BindingName]ConfigRefDigest {
	digests := make(map[BindingName]ConfigRefDigest, len(p.bindings)+1)
	for _, binding := range p.bindings {
		digests[binding.Name] = binding.ConfigDigest
	}
	if !assignedClassSet(p.assignments, ReservedWorkBinding).Empty() {
		digests[ReservedWorkBinding] = p.workPlan.ConfigContext()
	}
	return digests
}

func (p *StoragePlan) openStep(name BindingName) (PlannedOpen, bool) {
	for _, step := range p.openProgram {
		if step.Binding == name {
			return step, true
		}
	}
	return PlannedOpen{}, false
}

func assignedClassSet(assignments map[coordclass.Class]BindingName, name BindingName) ClassSet {
	var classes ClassSet
	for _, class := range planClassOrder() {
		if assignments[class] == name {
			classes = classes.with(class)
		}
	}
	return classes
}

// plannedRequirements states, per assigned class, the capabilities the closed
// front-door contract cannot be served without: Graph and Work expose
// transactions and claim/release, and the Nudge queue claims due items.
func plannedRequirements(classes ClassSet) ([]ClassCapabilityRequirement, error) {
	if classes.Empty() {
		return nil, fmt.Errorf("%w: no assigned classes", ErrInvalidStoragePlan)
	}
	requirements := make([]ClassCapabilityRequirement, 0, len(classes.Classes()))
	for _, class := range classes.Classes() {
		switch class {
		case coordclass.ClassWork, coordclass.ClassGraph:
			requirements = append(requirements, ClassCapabilityRequirement{Class: class, RequireTransactions: true, RequireClaims: true})
		case coordclass.ClassNudges:
			requirements = append(requirements, ClassCapabilityRequirement{Class: class, RequireClaims: true})
		default:
			requirements = append(requirements, ClassCapabilityRequirement{Class: class})
		}
	}
	return requirements, nil
}

// planClassOrder is the fixed class order this package pins for configuration and
// open programs.
func planClassOrder() []coordclass.Class {
	return []coordclass.Class{
		coordclass.ClassWork,
		coordclass.ClassGraph,
		coordclass.ClassSessions,
		coordclass.ClassMessaging,
		coordclass.ClassOrders,
		coordclass.ClassNudges,
	}
}

// inspectionClassOrder is the fixed fencing and inspection order: the five
// infrastructure classes first, then Work.
func inspectionClassOrder() []coordclass.Class {
	return []coordclass.Class{
		coordclass.ClassGraph,
		coordclass.ClassSessions,
		coordclass.ClassMessaging,
		coordclass.ClassOrders,
		coordclass.ClassNudges,
		coordclass.ClassWork,
	}
}

func openRank(classes ClassSet) int { return firstClassRank(planClassOrder(), classes) }

func inspectionRank(classes ClassSet) int { return firstClassRank(inspectionClassOrder(), classes) }

func firstClassRank(order []coordclass.Class, classes ClassSet) int {
	for rank, class := range order {
		if classes.Has(class) {
			return rank
		}
	}
	return len(order)
}

func storageClassFor(class coordclass.Class) config.StorageClass {
	switch class {
	case coordclass.ClassWork:
		return config.StorageClassWork
	case coordclass.ClassGraph:
		return config.StorageClassGraph
	case coordclass.ClassSessions:
		return config.StorageClassSessions
	case coordclass.ClassMessaging:
		return config.StorageClassMessaging
	case coordclass.ClassOrders:
		return config.StorageClassOrders
	case coordclass.ClassNudges:
		return config.StorageClassNudges
	default:
		return ""
	}
}

// bindingConfigDigest digests the CONFIGURED facts of one binding: what
// city.toml says, not where the city is. The city root is deliberately absent.
// This digest is persisted on the migration manifest wire, so folding a new
// input into it would change the identity of records already written — and the
// city is not missing from the plan's identity anyway: the composite digest
// folds the Work pins' config context, which already hashes the city path.
func bindingConfigDigest(spec BindingSpec) ConfigRefDigest {
	var encoded canonicalDescriptorEncoding
	encoded.string("gascity.storage-binding-config.v1")
	encoded.string(string(spec.Name))
	encoded.string(string(spec.Provider))
	encoded.string(spec.Path)
	encoded.string(string(spec.ConfigRef))
	return ConfigRefDigest(canonicalDigest(encoded.bytes))
}

func compositeConfigDigest(plan *StoragePlan) CompositeConfigDigest {
	var encoded canonicalDescriptorEncoding
	encoded.string("gascity.storage-composite-config.v1")
	for _, class := range planClassOrder() {
		encoded.string(class.String())
		encoded.string(string(plan.assignments[class]))
	}
	encoded.uint64(uint64(len(plan.bindings)))
	for _, binding := range plan.bindings {
		encoded.string(string(binding.Name))
		encoded.string(string(binding.ProviderID))
		encoded.string(string(binding.ConfigDigest))
	}
	encoded.string(string(plan.workPlan.ConfigContext()))
	return CompositeConfigDigest(canonicalDigest(encoded.bytes))
}

func canonicalDigest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}
