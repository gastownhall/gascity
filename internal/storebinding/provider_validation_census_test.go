package storebinding

import (
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/coordclass"
)

type persistedFieldValidationFamily string

const (
	persistedFieldIdentifier persistedFieldValidationFamily = "identifier"
	persistedFieldDigest     persistedFieldValidationFamily = "canonical-digest"
	persistedFieldSecretFree persistedFieldValidationFamily = "secret-free"
	persistedFieldNested     persistedFieldValidationFamily = "nested"
	persistedFieldScalar     persistedFieldValidationFamily = "scalar"
	persistedFieldEnum       persistedFieldValidationFamily = "closed-enum"
	persistedFieldOperation  persistedFieldValidationFamily = "operation-only"
)

type persistedStructValidationCensus struct {
	typ    reflect.Type
	fields map[string]persistedFieldValidationFamily
}

// TestPersistedProviderBoundaryValidationCensus prevents an exported
// string-like field from joining a provider-plan, manifest, receipt, or
// retained-reference value without an explicit validation classification.
// Nested values must themselves have a census entry; only typed scalars and
// live operation-only handles may avoid a string validation family.
func TestPersistedProviderBoundaryValidationCensus(t *testing.T) {
	census := persistedProviderValidationCensus()
	known := make(map[reflect.Type]struct{}, len(census))
	for _, entry := range census {
		if entry.typ.Kind() != reflect.Struct {
			t.Fatalf("census type %v is not a struct", entry.typ)
		}
		if _, duplicate := known[entry.typ]; duplicate {
			t.Fatalf("duplicate validation census entry for %v", entry.typ)
		}
		known[entry.typ] = struct{}{}
	}

	for _, entry := range census {
		t.Run(entry.typ.Name(), func(t *testing.T) {
			seen := make(map[string]struct{}, entry.typ.NumField())
			for index := 0; index < entry.typ.NumField(); index++ {
				field := entry.typ.Field(index)
				if !field.IsExported() {
					continue
				}
				family, found := entry.fields[field.Name]
				if !found {
					t.Fatalf("exported field %s.%s has no validation classification", entry.typ, field.Name)
				}
				seen[field.Name] = struct{}{}
				assertPersistedFieldFamily(t, field, family, known)
			}
			for field := range entry.fields {
				if _, found := seen[field]; !found {
					t.Fatalf("validation census contains removed or unexported field %s.%s", entry.typ, field)
				}
			}
		})
	}
}

func assertPersistedFieldFamily(t *testing.T, field reflect.StructField, family persistedFieldValidationFamily, known map[reflect.Type]struct{}) {
	t.Helper()
	switch family {
	case persistedFieldIdentifier, persistedFieldDigest, persistedFieldSecretFree:
		if !isPersistedStringLike(field.Type) {
			t.Fatalf("%s is classified as %s but has non-string-like type %s", field.Name, family, field.Type)
		}
	case persistedFieldNested:
		nested := indirectPersistedType(field.Type)
		if nested.Kind() != reflect.Struct {
			t.Fatalf("%s is classified as nested but has non-struct type %s", field.Name, field.Type)
		}
		if _, found := known[nested]; !found {
			t.Fatalf("%s nests %s without its own validation census entry", field.Name, nested)
		}
	case persistedFieldScalar:
		switch field.Type.Kind() {
		case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		default:
			t.Fatalf("%s is classified as scalar but has non-scalar type %s", field.Name, field.Type)
		}
	case persistedFieldEnum:
		if field.Type != reflect.TypeOf(coordclass.Class(0)) {
			t.Fatalf("%s is classified as a closed enum but has type %s", field.Name, field.Type)
		}
	case persistedFieldOperation:
		switch field.Type.Kind() {
		case reflect.Interface, reflect.Func:
		default:
			t.Fatalf("%s is classified as operation-only but has non-operation type %s", field.Name, field.Type)
		}
	default:
		t.Fatalf("%s has unknown validation family %q", field.Name, family)
	}
}

func isPersistedStringLike(typ reflect.Type) bool {
	if typ.Kind() == reflect.String {
		return true
	}
	return typ.Kind() == reflect.Slice && (typ.Elem().Kind() == reflect.String || typ.Elem().Kind() == reflect.Uint8)
}

func indirectPersistedType(typ reflect.Type) reflect.Type {
	for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array {
		typ = typ.Elem()
	}
	return typ
}

func persistedProviderValidationCensus() []persistedStructValidationCensus {
	return []persistedStructValidationCensus{
		{typ: reflect.TypeOf(BindingSpec{}), fields: map[string]persistedFieldValidationFamily{
			"Name": persistedFieldIdentifier, "Provider": persistedFieldIdentifier, "Path": persistedFieldSecretFree, "ConfigRef": persistedFieldIdentifier,
		}},
		{typ: reflect.TypeOf(ClassSet{}), fields: map[string]persistedFieldValidationFamily{}},
		{typ: reflect.TypeOf(ClassCapability{}), fields: map[string]persistedFieldValidationFamily{
			"Available": persistedFieldScalar, "Transactions": persistedFieldScalar, "Claims": persistedFieldScalar,
		}},
		{typ: reflect.TypeOf(ClassCapabilities{}), fields: map[string]persistedFieldValidationFamily{
			"Work": persistedFieldNested, "Graph": persistedFieldNested, "Sessions": persistedFieldNested, "Messaging": persistedFieldNested, "Orders": persistedFieldNested, "Nudges": persistedFieldNested, "WriterFencing": persistedFieldScalar, "GuardedActivation": persistedFieldScalar,
		}},
		{typ: reflect.TypeOf(MarkerState{}), fields: map[string]persistedFieldValidationFamily{
			"Name": persistedFieldSecretFree, "Present": persistedFieldScalar,
		}},
		{typ: reflect.TypeOf(ComponentDescriptor{}), fields: map[string]persistedFieldValidationFamily{
			"ID": persistedFieldIdentifier, "Locator": persistedFieldSecretFree, "PhysicalIdentity": persistedFieldSecretFree, "Classes": persistedFieldNested, "Format": persistedFieldSecretFree, "SchemaVersion": persistedFieldSecretFree, "ABIVersion": persistedFieldSecretFree, "Marker": persistedFieldNested,
		}},
		{typ: reflect.TypeOf(Descriptor{}), fields: map[string]persistedFieldValidationFamily{
			"Version": persistedFieldScalar, "SemanticContractVersion": persistedFieldSecretFree, "Provider": persistedFieldIdentifier, "ImplementationVersion": persistedFieldSecretFree, "Components": persistedFieldNested, "Capabilities": persistedFieldNested, "ConfigRefDigest": persistedFieldDigest, "RetainedSource": persistedFieldNested,
		}},
		{typ: reflect.TypeOf(NamedDescriptor{}), fields: map[string]persistedFieldValidationFamily{
			"Name": persistedFieldIdentifier, "Descriptor": persistedFieldNested,
		}},
		{typ: reflect.TypeOf(FenceComponentTarget{}), fields: map[string]persistedFieldValidationFamily{
			"ID": persistedFieldIdentifier, "Locator": persistedFieldSecretFree, "PhysicalIdentity": persistedFieldSecretFree, "Classes": persistedFieldNested,
		}},
		{typ: reflect.TypeOf(FenceTarget{}), fields: map[string]persistedFieldValidationFamily{
			"Version": persistedFieldScalar, "Provider": persistedFieldIdentifier, "Classes": persistedFieldNested, "Components": persistedFieldNested,
		}},
		{typ: reflect.TypeOf(Inspection{}), fields: map[string]persistedFieldValidationFamily{
			"Target": persistedFieldNested, "Descriptor": persistedFieldNested,
		}},
		{typ: reflect.TypeOf(MigrationGuardScope{}), fields: map[string]persistedFieldValidationFamily{}},
		{typ: reflect.TypeOf(FenceRequest{}), fields: map[string]persistedFieldValidationFamily{
			"Target": persistedFieldNested, "GuardScope": persistedFieldNested, "ExpectedGeneration": persistedFieldScalar, "Components": persistedFieldIdentifier, "Role": persistedFieldScalar,
		}},
		{typ: reflect.TypeOf(FencedInspectionRequest{}), fields: map[string]persistedFieldValidationFamily{
			"Target": persistedFieldNested, "Fence": persistedFieldOperation, "ExpectedGeneration": persistedFieldScalar,
		}},
		{typ: reflect.TypeOf(ComponentCompatibilityRequirement{}), fields: map[string]persistedFieldValidationFamily{
			"Component": persistedFieldIdentifier, "Format": persistedFieldSecretFree, "SchemaVersion": persistedFieldSecretFree, "ABIVersion": persistedFieldSecretFree,
		}},
		{typ: reflect.TypeOf(ClassCapabilityRequirement{}), fields: map[string]persistedFieldValidationFamily{
			"Class": persistedFieldEnum, "RequireTransactions": persistedFieldScalar, "RequireClaims": persistedFieldScalar,
		}},
		{typ: reflect.TypeOf(WorkScope{}), fields: map[string]persistedFieldValidationFamily{}},
		{typ: reflect.TypeOf(WorkTopology{}), fields: map[string]persistedFieldValidationFamily{}},
		{typ: reflect.TypeOf(OpenRequest{}), fields: map[string]persistedFieldValidationFamily{
			"Descriptor": persistedFieldNested, "AssignedClasses": persistedFieldNested, "Mode": persistedFieldScalar, "ExpectedGeneration": persistedFieldScalar, "PinnedWork": persistedFieldNested, "ExpectedContract": persistedFieldSecretFree, "ExpectedComponents": persistedFieldNested, "ClassRequirements": persistedFieldNested, "AdmissionFence": persistedFieldOperation, "DurableActiveAuthority": persistedFieldNested,
		}},
		{typ: reflect.TypeOf(DurableActiveOpenAuthority{}), fields: map[string]persistedFieldValidationFamily{}},
		{typ: reflect.TypeOf(RetainedSourceRef{}), fields: map[string]persistedFieldValidationFamily{
			"Version": persistedFieldScalar, "Provider": persistedFieldIdentifier, "ImplementationVersion": persistedFieldSecretFree, "Component": persistedFieldIdentifier, "Classes": persistedFieldNested, "SemanticContractVersion": persistedFieldSecretFree, "Format": persistedFieldSecretFree, "SchemaVersion": persistedFieldSecretFree, "ABIVersion": persistedFieldSecretFree, "PhysicalIdentity": persistedFieldSecretFree, "ConfigRefDigest": persistedFieldDigest, "WitnessVersion": persistedFieldSecretFree, "WitnessDigest": persistedFieldDigest, "ReopenData": persistedFieldSecretFree,
		}},
		{typ: reflect.TypeOf(GuardInstallRequest{}), fields: map[string]persistedFieldValidationFamily{
			"Attempt": persistedFieldSecretFree, "Generation": persistedFieldScalar, "Source": persistedFieldNested, "Component": persistedFieldIdentifier, "PhysicalIdentity": persistedFieldSecretFree, "Role": persistedFieldScalar,
		}},
		{typ: reflect.TypeOf(GuardDiscoverRequest{}), fields: map[string]persistedFieldValidationFamily{
			"Attempt": persistedFieldSecretFree, "Generation": persistedFieldScalar, "Source": persistedFieldNested, "Component": persistedFieldIdentifier, "PhysicalIdentity": persistedFieldSecretFree, "Role": persistedFieldScalar,
		}},
		{typ: reflect.TypeOf(FencedGuardInstallRequest{}), fields: map[string]persistedFieldValidationFamily{
			"GuardInstallRequest": persistedFieldNested, "SourceFence": persistedFieldOperation,
		}},
		{typ: reflect.TypeOf(FencedGuardDiscoverRequest{}), fields: map[string]persistedFieldValidationFamily{
			"GuardDiscoverRequest": persistedFieldNested, "SourceFence": persistedFieldOperation,
		}},
		{typ: reflect.TypeOf(GuardReceipt{}), fields: map[string]persistedFieldValidationFamily{
			"Version": persistedFieldScalar, "Attempt": persistedFieldSecretFree, "Generation": persistedFieldScalar, "Provider": persistedFieldIdentifier, "Component": persistedFieldIdentifier, "PhysicalIdentity": persistedFieldSecretFree, "Classes": persistedFieldNested, "SemanticContractVersion": persistedFieldSecretFree, "Role": persistedFieldScalar, "TransferState": persistedFieldScalar, "TransferParticipant": persistedFieldSecretFree, "TransferDestinationIdentity": persistedFieldDigest, "TransferReceiptKind": persistedFieldScalar, "ActiveProof": persistedFieldNested, "ReceiptID": persistedFieldSecretFree, "Revalidation": persistedFieldSecretFree,
		}},
		{typ: reflect.TypeOf(FencedGuardVerificationRequest{}), fields: map[string]persistedFieldValidationFamily{
			"Receipt": persistedFieldNested, "SourceFence": persistedFieldOperation,
		}},
		{typ: reflect.TypeOf(GuardDiscovery{}), fields: map[string]persistedFieldValidationFamily{
			"Found": persistedFieldScalar, "Receipt": persistedFieldNested,
		}},
		{typ: reflect.TypeOf(GuardTransferTarget{}), fields: map[string]persistedFieldValidationFamily{
			"Decision": persistedFieldNested, "Participant": persistedFieldSecretFree, "DestinationIdentity": persistedFieldDigest, "ExpectedReceiptKind": persistedFieldScalar, "State": persistedFieldScalar, "ActiveProof": persistedFieldNested,
		}},
		{typ: reflect.TypeOf(PredecisionAbandonmentAuthority{}), fields: map[string]persistedFieldValidationFamily{}},
		{typ: reflect.TypeOf(GuardTransitionRequest{}), fields: map[string]persistedFieldValidationFamily{
			"Current": persistedFieldNested, "Release": persistedFieldScalar, "Abandonment": persistedFieldNested, "Target": persistedFieldNested, "SourceDescriptor": persistedFieldNested, "SourceFence": persistedFieldOperation,
		}},
		{typ: reflect.TypeOf(CommitDecision{}), fields: map[string]persistedFieldValidationFamily{
			"Attempt": persistedFieldSecretFree, "Generation": persistedFieldScalar, "Decided": persistedFieldScalar,
		}},
		{typ: reflect.TypeOf(ParticipantReceipt{}), fields: map[string]persistedFieldValidationFamily{
			"Version": persistedFieldScalar, "Kind": persistedFieldScalar, "Attempt": persistedFieldSecretFree, "Generation": persistedFieldScalar, "Participant": persistedFieldSecretFree, "DescriptorIdentity": persistedFieldDigest, "ReceiptID": persistedFieldSecretFree, "Preparation": persistedFieldNested, "PreparedReceipt": persistedFieldSecretFree,
		}},
		{typ: reflect.TypeOf(GuardedActivationAttestation{}), fields: map[string]persistedFieldValidationFamily{}},
		{typ: reflect.TypeOf(InPlaceGuardedActivationAuthority{}), fields: map[string]persistedFieldValidationFamily{}},
		{typ: reflect.TypeOf(BindingActivationRequest{}), fields: map[string]persistedFieldValidationFamily{
			"Decision": persistedFieldNested, "Participant": persistedFieldSecretFree, "Destination": persistedFieldNested, "DesiredIdentity": persistedFieldDigest, "GuardAttestation": persistedFieldNested, "DestinationFence": persistedFieldOperation,
		}},
		{typ: reflect.TypeOf(BindingActivationResumeRequest{}), fields: map[string]persistedFieldValidationFamily{
			"Decision": persistedFieldNested, "Participant": persistedFieldSecretFree, "Destination": persistedFieldNested, "DesiredIdentity": persistedFieldDigest, "GuardAttestation": persistedFieldNested, "DestinationFence": persistedFieldOperation,
		}},
		{typ: reflect.TypeOf(WorkWorkspaceMember{}), fields: map[string]persistedFieldValidationFamily{
			"Scope": persistedFieldNested, "Prefix": persistedFieldIdentifier, "ConfigContext": persistedFieldDigest, "Suspended": persistedFieldScalar, "ConfigOrder": persistedFieldScalar, "Provider": persistedFieldIdentifier, "Component": persistedFieldIdentifier, "PhysicalIdentity": persistedFieldSecretFree,
		}},
		{typ: reflect.TypeOf(WorkWorkspaceParticipant{}), fields: map[string]persistedFieldValidationFamily{
			"Provider": persistedFieldIdentifier, "Component": persistedFieldIdentifier, "PhysicalIdentity": persistedFieldSecretFree, "Members": persistedFieldNested,
		}},
		{typ: reflect.TypeOf(WorkPrepareRequest{}), fields: map[string]persistedFieldValidationFamily{
			"Attempt": persistedFieldSecretFree, "Generation": persistedFieldScalar, "Direction": persistedFieldScalar, "Participant": persistedFieldNested, "Source": persistedFieldNested, "Destination": persistedFieldNested, "SourceFence": persistedFieldOperation, "DestinationFence": persistedFieldOperation, "WitnessVersion": persistedFieldSecretFree, "ConfigDigest": persistedFieldDigest, "PriorReceipt": persistedFieldNested,
		}},
		{typ: reflect.TypeOf(WorkPreparationIdentity{}), fields: map[string]persistedFieldValidationFamily{
			"Version": persistedFieldScalar, "Attempt": persistedFieldSecretFree, "Generation": persistedFieldScalar, "Direction": persistedFieldScalar, "Participant": persistedFieldNested, "SourceIdentity": persistedFieldDigest, "DestinationIdentity": persistedFieldDigest, "WitnessVersion": persistedFieldSecretFree, "ConfigDigest": persistedFieldDigest,
		}},
		{typ: reflect.TypeOf(WorkPrepared{}), fields: map[string]persistedFieldValidationFamily{
			"Version": persistedFieldScalar, "Attempt": persistedFieldSecretFree, "Generation": persistedFieldScalar, "Participant": persistedFieldNested, "DescriptorIdentity": persistedFieldDigest, "Preparation": persistedFieldNested, "Receipt": persistedFieldSecretFree,
		}},
		{typ: reflect.TypeOf(WorkVerifyRequest{}), fields: map[string]persistedFieldValidationFamily{
			"Prepare": persistedFieldNested, "Prepared": persistedFieldNested,
		}},
		{typ: reflect.TypeOf(WorkProof{}), fields: map[string]persistedFieldValidationFamily{
			"Version": persistedFieldScalar, "Attempt": persistedFieldSecretFree, "Generation": persistedFieldScalar, "Participant": persistedFieldNested, "DescriptorIdentity": persistedFieldDigest, "Preparation": persistedFieldNested, "PreparedReceipt": persistedFieldSecretFree, "Witness": persistedFieldSecretFree,
		}},
		{typ: reflect.TypeOf(WorkCommitRequest{}), fields: map[string]persistedFieldValidationFamily{
			"Decision": persistedFieldNested, "Participant": persistedFieldNested, "Prepare": persistedFieldNested, "Prepared": persistedFieldNested, "Proof": persistedFieldNested,
		}},
		{typ: reflect.TypeOf(WorkResumeRequest{}), fields: map[string]persistedFieldValidationFamily{
			"Prepare": persistedFieldNested,
		}},
		{typ: reflect.TypeOf(WorkProgress{}), fields: map[string]persistedFieldValidationFamily{
			"Version": persistedFieldScalar, "Attempt": persistedFieldSecretFree, "Generation": persistedFieldScalar, "Participant": persistedFieldNested, "Preparation": persistedFieldNested, "Complete": persistedFieldScalar, "Receipt": persistedFieldNested,
		}},
		{typ: reflect.TypeOf(RetainedWorkRequest{}), fields: map[string]persistedFieldValidationFamily{
			"Attempt": persistedFieldSecretFree, "Generation": persistedFieldScalar, "Participant": persistedFieldNested, "Source": persistedFieldNested, "ExpectedContract": persistedFieldSecretFree,
		}},

		// These values cross the provider-open or retained-work handoff as live
		// carriers. They are deliberately not durable manifests: store/service
		// interfaces and close callbacks are operation-only, while their exposed
		// identity strings remain classified so a future persistence path cannot
		// silently serialize an unchecked field.
		{typ: reflect.TypeOf(BeadsAdapterIdentity{}), fields: map[string]persistedFieldValidationFamily{
			"OpenerID": persistedFieldIdentifier, "ComponentID": persistedFieldIdentifier, "PhysicalID": persistedFieldSecretFree,
		}},
		{typ: reflect.TypeOf(MessagingFrontDoors{}), fields: map[string]persistedFieldValidationFamily{
			"Mail": persistedFieldOperation, "Bindings": persistedFieldOperation, "DeliveryContexts": persistedFieldOperation, "Groups": persistedFieldOperation, "Transcripts": persistedFieldOperation,
		}},
		{typ: reflect.TypeOf(NudgeFrontDoors{}), fields: map[string]persistedFieldValidationFamily{
			"Queue": persistedFieldOperation, "Shadows": persistedFieldOperation,
		}},
		{typ: reflect.TypeOf(BeadsAdapters{}), fields: map[string]persistedFieldValidationFamily{
			"Identity": persistedFieldNested, "Work": persistedFieldOperation, "Graph": persistedFieldOperation, "Sessions": persistedFieldOperation, "Messaging": persistedFieldNested, "Orders": persistedFieldOperation, "Nudges": persistedFieldNested,
		}},
		{typ: reflect.TypeOf(Workspace{}), fields: map[string]persistedFieldValidationFamily{
			"Scope": persistedFieldNested, "Store": persistedFieldOperation, "Prefix": persistedFieldIdentifier, "Suspended": persistedFieldScalar, "OpenerID": persistedFieldIdentifier, "ComponentID": persistedFieldIdentifier, "PhysicalID": persistedFieldSecretFree,
		}},
		{typ: reflect.TypeOf(PhysicalWorkspace{}), fields: map[string]persistedFieldValidationFamily{
			"Workspace": persistedFieldNested, "Scopes": persistedFieldNested,
		}},
		{typ: reflect.TypeOf(StoreSet{}), fields: map[string]persistedFieldValidationFamily{}},
		{typ: reflect.TypeOf(ComponentHandle{}), fields: map[string]persistedFieldValidationFamily{
			"Component": persistedFieldIdentifier, "Close": persistedFieldOperation,
		}},
		{typ: reflect.TypeOf(OpenedBindingParts{}), fields: map[string]persistedFieldValidationFamily{
			"Descriptor": persistedFieldNested, "Capabilities": persistedFieldNested, "Work": persistedFieldNested, "Graph": persistedFieldOperation, "Sessions": persistedFieldOperation, "Messaging": persistedFieldOperation, "Orders": persistedFieldOperation, "Nudges": persistedFieldNested, "Handles": persistedFieldNested,
		}},
		{typ: reflect.TypeOf(RetainedWorkMemberView{}), fields: map[string]persistedFieldValidationFamily{
			"Scope": persistedFieldNested, "Prefix": persistedFieldIdentifier, "Store": persistedFieldOperation,
		}},
		{typ: reflect.TypeOf(RetainedWorkWorkspace{}), fields: map[string]persistedFieldValidationFamily{
			"Participant": persistedFieldNested,
		}},
	}
}
