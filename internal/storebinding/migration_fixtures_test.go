package storebinding

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
)

const migrationContract ContractVersion = "gascity.storage.migration.test.v1"

func migrationAllClasses(t *testing.T) ClassSet {
	t.Helper()
	return planClassSet(t, coordclass.Classes()...)
}

func migrationInfraClasses(t *testing.T) ClassSet {
	t.Helper()
	return planClassSet(t, coordclass.ClassGraph, coordclass.ClassMessaging, coordclass.ClassSessions, coordclass.ClassOrders, coordclass.ClassNudges)
}

// migrationDescriptor builds a complete descriptor whose physical identities are
// derived from the binding name, so two bindings are never accidentally the same
// physical thing.
func migrationDescriptor(t *testing.T, binding BindingName, provider ProviderID, classes ClassSet) Descriptor {
	t.Helper()
	descriptor, err := NewDescriptor(Descriptor{
		Version:                 1,
		SemanticContractVersion: migrationContract,
		Provider:                provider,
		ImplementationVersion:   "1.0.0",
		Components: []ComponentDescriptor{{
			ID:               ComponentID(string(binding) + "-main"),
			Locator:          ComponentLocator("/var/lib/gascity/" + string(binding) + "/main"),
			PhysicalIdentity: PhysicalIdentity(string(binding) + "-main-physical"),
			Classes:          classes,
			Format:           "test-format",
			SchemaVersion:    "1",
			ABIVersion:       "1",
			Marker:           MarkerState{Name: string(binding) + ".migrated", Present: true},
		}},
		Capabilities:    planCapabilities(classes),
		ConfigRefDigest: ConfigRefDigest(canonicalDigest([]byte("config:" + binding))),
	})
	if err != nil {
		t.Fatalf("migrationDescriptor(%q): %v", binding, err)
	}
	return descriptor
}

func migrationFenceTarget(t *testing.T, descriptor Descriptor) FenceTarget {
	t.Helper()
	components := make([]FenceComponentTarget, 0, len(descriptor.Components))
	for _, component := range descriptor.Components {
		components = append(components, FenceComponentTarget{
			ID:               component.ID,
			Locator:          component.Locator,
			PhysicalIdentity: component.PhysicalIdentity,
			Classes:          component.Classes,
		})
	}
	target, err := NewFenceTarget(descriptor.Provider, descriptor.Classes(), components)
	if err != nil {
		t.Fatalf("migrationFenceTarget: %v", err)
	}
	return target
}

type migrationDiscoveryOptions struct {
	incomplete bool
	empty      bool
}

func migrationDiscovered(t *testing.T, name BindingName, descriptor Descriptor, options migrationDiscoveryOptions) DiscoveredBinding {
	t.Helper()
	population := BindingPopulationPopulated
	if options.empty {
		population = BindingPopulationEmpty
	}
	pinned := &descriptor
	if options.incomplete {
		pinned = nil
	}
	inspection, err := NewInspection(migrationFenceTarget(t, descriptor), pinned)
	if err != nil {
		t.Fatalf("migrationDiscovered(%q): %v", name, err)
	}
	discovery := DiscoveredBinding{Name: name, Inspection: inspection, Population: population}
	if err := discovery.Validate(); err != nil {
		t.Fatalf("migrationDiscovered(%q) invalid: %v", name, err)
	}
	return discovery
}

// migrationPlan freezes a plan that puts every class on one explicit binding.
func migrationPlan(t *testing.T, binding string) *StoragePlan {
	t.Helper()
	return migrationPlanFor(t, planAllClassesOn(binding), map[string]config.StorageBindingConfig{
		binding: {Provider: "infra-provider", Path: ".gc/" + binding},
	})
}

func migrationPlanFor(t *testing.T, classes map[coordclass.Class]string, bindings map[string]config.StorageBindingConfig) *StoragePlan {
	t.Helper()
	plan, _ := migrationPlanWithFactories(t, classes, bindings)
	return plan
}

// migrationPlanWithFactories also returns the inert provider factories so a test
// can prove that intent derivation never called a provider operation.
func migrationPlanWithFactories(t *testing.T, classes map[coordclass.Class]string, bindings map[string]config.StorageBindingConfig) (*StoragePlan, []*planCountingFactory) {
	t.Helper()
	factories := []*planCountingFactory{{id: "infra-provider"}, {id: "task-beads-provider"}}
	registry, _ := planRegistry(t, factories...)
	plan, err := ResolveStoragePlan(registry, planStorageConfig(classes, bindings), planWorkPins(), "")
	if err != nil {
		t.Fatalf("ResolveStoragePlan: %v", err)
	}
	return plan, factories
}

func migrationIdentity(t *testing.T, descriptor Descriptor) BindingIdentity {
	t.Helper()
	identity, err := descriptor.Identity()
	if err != nil {
		t.Fatalf("Descriptor.Identity: %v", err)
	}
	return identity
}

func migrationBindingReceipt(t *testing.T, participant string, descriptor Descriptor, attempt AttemptID, generation Generation) ParticipantReceipt {
	t.Helper()
	receipt := ParticipantReceipt{
		Version:            1,
		Kind:               ParticipantReceiptBindingActivation,
		Attempt:            attempt,
		Generation:         generation,
		Participant:        participant,
		DescriptorIdentity: migrationIdentity(t, descriptor),
		ReceiptID:          "receipt-" + participant,
	}
	if err := receipt.Validate(); err != nil {
		t.Fatalf("migrationBindingReceipt(%q): %v", participant, err)
	}
	return receipt
}

func migrationRetainedSource(t *testing.T, descriptor Descriptor, algorithm, witnessDigest string) RetainedSourceRef {
	t.Helper()
	component := descriptor.Components[0]
	source := RetainedSourceRef{
		Version:                 1,
		Provider:                descriptor.Provider,
		ImplementationVersion:   descriptor.ImplementationVersion,
		Component:               component.ID,
		Classes:                 component.Classes,
		SemanticContractVersion: descriptor.SemanticContractVersion,
		Format:                  component.Format,
		SchemaVersion:           component.SchemaVersion,
		ABIVersion:              component.ABIVersion,
		PhysicalIdentity:        component.PhysicalIdentity,
		ConfigRefDigest:         descriptor.ConfigRefDigest,
		WitnessVersion:          algorithm,
		WitnessDigest:           witnessDigest,
		ReopenData:              []byte("reopen:" + component.ID),
	}
	if err := source.Validate(); err != nil {
		t.Fatalf("migrationRetainedSource: %v", err)
	}
	return source
}

type migrationActiveOptions struct {
	generation     Generation
	attempt        AttemptID
	assignments    map[coordclass.Class]BindingName
	descriptors    []NamedDescriptor
	configDigest   CompositeConfigDigest
	bindingDigests map[BindingName]ConfigRefDigest
	retained       []RetainedSourceRef
}

func migrationActive(t *testing.T, options migrationActiveOptions) *ActiveManifest {
	t.Helper()
	manifest := &ActiveManifest{
		Version:              1,
		Generation:           options.generation,
		Attempt:              options.attempt,
		Assignments:          options.assignments,
		Descriptors:          options.descriptors,
		RetainedSources:      options.retained,
		ConfigDigest:         options.configDigest,
		BindingConfigDigests: options.bindingDigests,
		WitnessAlgorithm:     SemanticWitnessAlgorithm,
		CutoverGeneration:    options.generation,
	}
	for _, descriptor := range options.descriptors {
		participant := BindingParticipant{Name: descriptor.Name}.Key()
		manifest.Receipts = append(manifest.Receipts, migrationBindingReceipt(t, participant, descriptor.Descriptor, options.attempt, options.generation))
	}
	if err := manifest.Validate(); err != nil {
		t.Fatalf("migrationActive: %v", err)
	}
	return manifest
}

// migrationActiveManifest builds a durable active generation for one binding.
func migrationActiveManifest(t *testing.T, plan *StoragePlan, generation Generation, descriptor Descriptor, retained ...RetainedSourceRef) *ActiveManifest {
	t.Helper()
	binding := plan.Assignments()[coordclass.ClassGraph]
	return migrationActive(t, migrationActiveOptions{
		generation:     generation,
		attempt:        AttemptID("attempt-generation-" + descriptor.ImplementationVersion),
		assignments:    plan.Assignments(),
		descriptors:    []NamedDescriptor{{Name: binding, Descriptor: descriptor}},
		configDigest:   plan.ConfigDigest(),
		bindingDigests: plan.BindingConfigDigests(),
		retained:       retained,
	})
}

func migrationSemanticWitness(t *testing.T, class coordclass.Class, digest string) SemanticWitness {
	t.Helper()
	witness := SemanticWitness{
		Version:   1,
		Class:     class,
		Contract:  migrationContract,
		Algorithm: SemanticWitnessAlgorithm,
		Digest:    digest,
		Families:  []WitnessFamilyCount{{Name: class.String() + ".primary", Count: 3}, {Name: class.String() + ".empty", Count: 0}},
	}
	if err := validateRecordedWitness(witness); err != nil {
		t.Fatalf("migrationSemanticWitness(%s): %v", class, err)
	}
	return witness
}

func migrationPhysicalFacts(t *testing.T, descriptor Descriptor) []ComponentPhysicalFact {
	t.Helper()
	component := descriptor.Components[0]
	return []ComponentPhysicalFact{
		{Component: component.ID, Kind: PhysicalFactUserVersion, Value: "7"},
		{Component: component.ID, Kind: PhysicalFactComponentCensus, Value: canonicalDigest([]byte("census:" + component.PhysicalIdentity))},
	}
}

func migrationEnvelope(t *testing.T, descriptor Descriptor, semanticDigest string) WitnessEnvelope {
	t.Helper()
	envelope, err := NewWitnessEnvelope(descriptor, semanticDigest, migrationPhysicalFacts(t, descriptor))
	if err != nil {
		t.Fatalf("NewWitnessEnvelope: %v", err)
	}
	return envelope
}

// migrationWitnessRecord builds a complete, matching witness set for one class.
func migrationWitnessRecord(t *testing.T, class coordclass.Class, destination Descriptor) ClassWitnessRecord {
	t.Helper()
	digest := canonicalDigest([]byte("semantic:" + class.String()))
	witness := migrationSemanticWitness(t, class, digest)
	envelope := migrationEnvelope(t, destination, digest)
	record := ClassWitnessRecord{
		Class:               class,
		Source:              witness,
		Destination:         witness,
		FreshReopen:         witness,
		AdmittedEnvelope:    envelope,
		FreshReopenEnvelope: envelope,
	}
	if err := record.Validate(SemanticWitnessAlgorithm); err != nil {
		t.Fatalf("migrationWitnessRecord(%s): %v", class, err)
	}
	return record
}

func migrationCensus(t *testing.T, binding BindingName, descriptor Descriptor) []ComponentCensus {
	t.Helper()
	entries := make([]ComponentCensus, 0, len(descriptor.Components))
	for _, component := range descriptor.Components {
		entries = append(entries, ComponentCensus{
			Binding:           binding,
			Component:         component.ID,
			Locator:           component.Locator,
			PhysicalIdentity:  component.PhysicalIdentity,
			ByteLength:        4096,
			Digest:            canonicalDigest([]byte("census:" + component.PhysicalIdentity)),
			NamespaceIdentity: "dev=1,ino=2",
		})
	}
	return entries
}

func migrationPreparingSection(t *testing.T, binding BindingName, source Descriptor) PreparingSection {
	t.Helper()
	section := PreparingSection{
		Version:           1,
		SourceDescriptors: []NamedDescriptor{{Name: binding, Descriptor: source}},
		Census:            migrationCensus(t, binding, source),
		Holds: []RetentionHold{{
			Kind:             HoldRetention,
			Binding:          binding,
			Component:        source.Components[0].ID,
			PhysicalIdentity: source.Components[0].PhysicalIdentity,
			Reason:           "retained source for the prior generation",
		}},
	}
	for _, class := range coordclass.Classes() {
		section.Inventory = append(section.Inventory, ClassInventory{
			Class:            class,
			Binding:          binding,
			Component:        source.Components[0].ID,
			PhysicalIdentity: source.Components[0].PhysicalIdentity,
			Population:       BindingPopulationPopulated,
		})
		section.ClassStates = append(section.ClassStates, ClassPhaseState{Class: class, Phase: PhasePreparing, Resumable: true})
	}
	if err := section.Validate(); err != nil {
		t.Fatalf("migrationPreparingSection: %v", err)
	}
	return section
}

func migrationGuardReceipt(t *testing.T, request GuardInstallRequest, contract ContractVersion) GuardReceipt {
	t.Helper()
	receipt := GuardReceipt{
		Version:                 1,
		Attempt:                 request.Attempt,
		Generation:              request.Generation,
		Provider:                request.Source.Provider,
		Component:               request.Component,
		PhysicalIdentity:        request.PhysicalIdentity,
		Classes:                 request.Source.Classes,
		SemanticContractVersion: contract,
		Role:                    request.Role,
		ReceiptID:               "guard-" + string(request.Component),
		Revalidation:            "identity=" + string(request.PhysicalIdentity),
	}
	if err := receipt.Validate(); err != nil {
		t.Fatalf("migrationGuardReceipt: %v", err)
	}
	return receipt
}
