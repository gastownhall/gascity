package storebinding

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
)

const planTestContract ContractVersion = "gascity.storage.test.v1"

// planInertProvider fails every provider operation and records it. Planning may
// call exactly one provider-facing method — ProviderFactory.New — so any
// recorded call is a planner that touched a provider it must not touch.
type planInertProvider struct {
	id    ProviderID
	spec  BindingSpec
	calls []string
}

func (p *planInertProvider) record(name string) { p.calls = append(p.calls, name) }

func (p *planInertProvider) Inspect(context.Context, BindingSpec) (Inspection, error) {
	p.record("Inspect")
	return Inspection{}, ErrProviderUnavailable
}

func (p *planInertProvider) InspectFenced(context.Context, FencedInspectionRequest) (Descriptor, error) {
	p.record("InspectFenced")
	return Descriptor{}, ErrProviderUnavailable
}

func (p *planInertProvider) AcquireFence(context.Context, MigrationGuardClaim, FenceRequest) (WriterFence, error) {
	p.record("AcquireFence")
	return nil, ErrProviderUnavailable
}

func (p *planInertProvider) RetainedGuards() (RetainedGuardLifecycle, bool) {
	p.record("RetainedGuards")
	return nil, false
}

func (p *planInertProvider) BindingMigration() (BindingMigrationLifecycle, bool) {
	p.record("BindingMigration")
	return nil, false
}

func (p *planInertProvider) WorkMigration() (WorkMigrationLifecycle, bool) {
	p.record("WorkMigration")
	return nil, false
}

func (p *planInertProvider) Open(context.Context, OpenRequest) (OpenedBinding, error) {
	p.record("Open")
	return nil, ErrProviderUnavailable
}

// planCountingFactory records every construction so a test can prove one
// provider construction per distinct binding.
type planCountingFactory struct {
	id        ProviderID
	specs     []BindingSpec
	providers []*planInertProvider
	failWith  error
	returnNil bool
}

func (f *planCountingFactory) ID() ProviderID { return f.id }

func (f *planCountingFactory) New(spec BindingSpec) (Provider, error) {
	f.specs = append(f.specs, spec)
	if f.failWith != nil {
		return nil, f.failWith
	}
	if f.returnNil {
		return nil, nil
	}
	provider := &planInertProvider{id: f.id, spec: spec}
	f.providers = append(f.providers, provider)
	return provider, nil
}

func (f *planCountingFactory) calls() int {
	total := 0
	for _, provider := range f.providers {
		total += len(provider.calls)
	}
	return total
}

// planCountingLookup wraps a real frozen registry so lookup counting observes
// the shipped resolution behavior rather than a substitute for it.
type planCountingLookup struct {
	registry *ProviderRegistry
	lookups  []ProviderID
}

func (l *planCountingLookup) Lookup(id ProviderID) (ProviderFactory, error) {
	l.lookups = append(l.lookups, id)
	return l.registry.Lookup(id)
}

func (l *planCountingLookup) count(id ProviderID) int {
	total := 0
	for _, looked := range l.lookups {
		if looked == id {
			total++
		}
	}
	return total
}

func planRegistry(t *testing.T, factories ...*planCountingFactory) (*ProviderRegistry, *planCountingLookup) {
	t.Helper()
	registry := NewProviderRegistry()
	for _, factory := range factories {
		if err := registry.Register(factory); err != nil {
			t.Fatalf("Register(%q): %v", factory.id, err)
		}
	}
	if err := registry.Freeze(); err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	return registry, &planCountingLookup{registry: registry}
}

func planStorageConfig(classes map[coordclass.Class]string, bindings map[string]config.StorageBindingConfig) config.StorageConfig {
	storage := config.StorageConfig{Bindings: bindings}
	for class, binding := range classes {
		switch class {
		case coordclass.ClassWork:
			storage.Classes.Work = binding
		case coordclass.ClassGraph:
			storage.Classes.Graph = binding
		case coordclass.ClassSessions:
			storage.Classes.Sessions = binding
		case coordclass.ClassMessaging:
			storage.Classes.Messaging = binding
		case coordclass.ClassOrders:
			storage.Classes.Orders = binding
		case coordclass.ClassNudges:
			storage.Classes.Nudges = binding
		}
	}
	return storage
}

func planAllClassesOn(binding string) map[coordclass.Class]string {
	classes := make(map[coordclass.Class]string, len(planClassOrder()))
	for _, class := range planClassOrder() {
		classes[class] = binding
	}
	return classes
}

func planWorkPins() WorkPinInputs {
	return WorkPinInputs{
		Recorded:      true,
		ConfigContext: ConfigRefDigest(canonicalDigest([]byte("work-config-context"))),
		HQ:            WorkScopePin{Scope: HQScope(), Prefix: "hq", OpenerID: "beads", ComponentID: "work", PhysicalID: "hq-physical"},
		Rigs: []WorkScopePin{
			{Scope: RigScope("beta"), Prefix: "beta", Suspended: true, OpenerID: "beads", ComponentID: "work", PhysicalID: "beta-physical"},
			{Scope: RigScope("alpha"), Prefix: "alpha", OpenerID: "beads", ComponentID: "work", PhysicalID: "alpha-physical"},
		},
	}
}

// planUnifiedWorkPins shares one physical workspace between HQ and both rigs
// while keeping three semantic scopes and three distinct prefixes.
func planUnifiedWorkPins() WorkPinInputs {
	pins := planWorkPins()
	pins.HQ.PhysicalID = "city-physical"
	for index := range pins.Rigs {
		pins.Rigs[index].PhysicalID = "city-physical"
	}
	return pins
}

func planWorkTopology(t *testing.T, in WorkPinInputs) WorkTopology {
	t.Helper()
	stores := map[string]beads.Store{}
	store := func(physical string) beads.Store {
		if existing, ok := stores[physical]; ok {
			return existing
		}
		created := beads.NewMemStore()
		stores[physical] = created
		return created
	}
	toWorkspace := func(pin WorkScopePin) Workspace {
		return Workspace{
			Scope:       pin.Scope,
			Store:       store(pin.PhysicalID),
			Prefix:      pin.Prefix,
			Suspended:   pin.Suspended,
			OpenerID:    pin.OpenerID,
			ComponentID: pin.ComponentID,
			PhysicalID:  pin.PhysicalID,
		}
	}
	rigs := make([]Workspace, 0, len(in.Rigs))
	for _, rig := range in.Rigs {
		rigs = append(rigs, toWorkspace(rig))
	}
	topology, err := NewWorkTopology(toWorkspace(in.HQ), rigs)
	if err != nil {
		t.Fatalf("NewWorkTopology: %v", err)
	}
	return topology
}

func planClassSet(t *testing.T, classes ...coordclass.Class) ClassSet {
	t.Helper()
	set, err := NewClassSet(classes...)
	if err != nil {
		t.Fatalf("NewClassSet(%v): %v", classes, err)
	}
	return set
}

func planCapabilities(classes ClassSet) ClassCapabilities {
	capabilities := ClassCapabilities{WriterFencing: true, GuardedActivation: true}
	for _, class := range classes.Classes() {
		capabilities = planWithCapability(capabilities, class, ClassCapability{Available: true, Transactions: true, Claims: true})
	}
	return capabilities
}

func planWithCapability(capabilities ClassCapabilities, class coordclass.Class, capability ClassCapability) ClassCapabilities {
	switch class {
	case coordclass.ClassWork:
		capabilities.Work = capability
	case coordclass.ClassGraph:
		capabilities.Graph = capability
	case coordclass.ClassSessions:
		capabilities.Sessions = capability
	case coordclass.ClassMessaging:
		capabilities.Messaging = capability
	case coordclass.ClassOrders:
		capabilities.Orders = capability
	case coordclass.ClassNudges:
		capabilities.Nudges = capability
	}
	return capabilities
}

func planDescriptor(t *testing.T, binding BindingName, provider ProviderID, classes ClassSet, capabilities ClassCapabilities) Descriptor {
	t.Helper()
	descriptor, err := NewDescriptor(Descriptor{
		Version:                 1,
		SemanticContractVersion: planTestContract,
		Provider:                provider,
		ImplementationVersion:   "1.0.0",
		Components: []ComponentDescriptor{{
			ID:               ComponentID(string(binding) + "-main"),
			Locator:          ComponentLocator("/var/lib/gascity/" + string(binding)),
			PhysicalIdentity: PhysicalIdentity(string(binding) + "-physical"),
			Classes:          classes,
			Format:           "test-format",
			SchemaVersion:    "1",
			Marker:           MarkerState{Name: string(binding) + ".migrated", Present: true},
		}},
		Capabilities:    capabilities,
		ConfigRefDigest: ConfigRefDigest(canonicalDigest([]byte(binding))),
	})
	if err != nil {
		t.Fatalf("NewDescriptor(%q): %v", binding, err)
	}
	return descriptor
}

// planOpenedBinding is one ACTIVE handle fixture: a real OpenedBinding over
// real Beads class adapters, with a close counter so a test can observe that
// the builder never closes what it does not own.
type planOpenedBinding struct {
	binding    BindingName
	descriptor Descriptor
	opened     OpenedBinding
	closes     int
	topology   WorkTopology
}

type planOpenOptions struct {
	provider     ProviderID
	classes      ClassSet
	pins         *WorkPinInputs
	topology     *WorkTopology
	capabilities *ClassCapabilities
	descriptor   *Descriptor
}

func planOpen(t *testing.T, binding BindingName, options planOpenOptions) *planOpenedBinding {
	t.Helper()
	if options.provider == "" {
		options.provider = "test-provider"
	}
	capabilities := planCapabilities(options.classes)
	if options.capabilities != nil {
		capabilities = *options.capabilities
	}
	descriptor := planDescriptor(t, binding, options.provider, options.classes, capabilities)
	if options.descriptor != nil {
		descriptor = *options.descriptor
	}
	fixture := &planOpenedBinding{binding: binding, descriptor: descriptor}
	store := beads.NewMemStore()
	adapters, err := NewBeadsAdapters(store, BeadsAdapterIdentity{OpenerID: "beads", ComponentID: "component", PhysicalID: string(binding)}, nudgeQueueFake{})
	if err != nil {
		t.Fatalf("NewBeadsAdapters: %v", err)
	}
	parts := OpenedBindingParts{
		Descriptor:   descriptor,
		Capabilities: capabilities,
		Handles: []ComponentHandle{{
			Component: descriptor.Components[0].ID,
			Close:     func() error { fixture.closes++; return nil },
		}},
	}
	if options.classes.HasWork() {
		var topology WorkTopology
		switch {
		case options.topology != nil:
			topology = *options.topology
		case options.pins != nil:
			topology = planWorkTopology(t, *options.pins)
		default:
			topology = planWorkTopology(t, planWorkPins())
		}
		fixture.topology = topology
		parts.Work = &topology
	}
	if options.classes.Has(coordclass.ClassGraph) {
		parts.Graph = adapters.Graph
	}
	if options.classes.Has(coordclass.ClassSessions) {
		parts.Sessions = adapters.Sessions
	}
	if options.classes.Has(coordclass.ClassMessaging) {
		binder, err := BindBeadsMessaging(store)
		if err != nil {
			t.Fatalf("BindBeadsMessaging: %v", err)
		}
		parts.Messaging = binder
	}
	if options.classes.Has(coordclass.ClassOrders) {
		parts.Orders = adapters.Orders
	}
	if options.classes.Has(coordclass.ClassNudges) {
		nudges := adapters.Nudges
		parts.Nudges = &nudges
	}
	opened, err := NewOpenedBinding(parts)
	if err != nil {
		t.Fatalf("NewOpenedBinding(%q): %v", binding, err)
	}
	fixture.opened = opened
	return fixture
}

func planHandle(t *testing.T, fixture *planOpenedBinding, generation Generation) ActiveBindingHandle {
	t.Helper()
	authority, err := NewDurableActiveOpenAuthority(generation, fixture.descriptor)
	if err != nil {
		t.Fatalf("NewDurableActiveOpenAuthority(%q): %v", fixture.binding, err)
	}
	return ActiveBindingHandle{Binding: fixture.binding, Opened: fixture.opened, Authority: authority}
}
