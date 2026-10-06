package storebinding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

func TestOpenBindingRejectsDescriptorContractFormatAndCapabilityBeforeOpen(t *testing.T) {
	descriptor := testDescriptor(t, ProviderID("builtin-open"), PhysicalIdentity("open-identity"), coordclass.ClassGraph)
	classes := classSet(t, coordclass.ClassGraph)
	base := completeOpenRequest(t, descriptor, classes)

	tests := []struct {
		name    string
		request OpenRequest
		want    error
	}{
		{
			name: "missing capability",
			request: func() OpenRequest {
				request := base.Clone()
				request.Descriptor = descriptorWithoutCapabilities(descriptor)
				return request
			}(),
			want: ErrMissingCapability,
		},
		{
			name: "wrong contract",
			request: func() OpenRequest {
				request := base.Clone()
				request.ExpectedContract = ContractVersion("other-contract")
				return request
			}(),
			want: ErrWrongContract,
		},
		{
			name: "wrong format",
			request: func() OpenRequest {
				request := base.Clone()
				request.ExpectedComponents[0].Format = FormatID("other-format")
				return request
			}(),
			want: ErrWrongFormat,
		},
		{
			name: "unknown open mode",
			request: func() OpenRequest {
				request := base.Clone()
				request.Mode = OpenMode(99)
				return request
			}(),
			want: ErrInvalidOpenMode,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := &recordingProvider{}
			_, err := OpenBinding(context.Background(), provider, test.request)
			if !errors.Is(err, test.want) {
				t.Fatalf("OpenBinding() error = %v, want %v", err, test.want)
			}
			if provider.openCalls != 0 {
				t.Fatalf("OpenBinding() called provider.Open %d times after validation failure", provider.openCalls)
			}
		})
	}
}

func TestOpenRequestRequiresCompleteCompatibilityAndPerClassRequirements(t *testing.T) {
	descriptor := testDescriptor(t, ProviderID("builtin-open-requirements"), PhysicalIdentity("open-requirements-identity"), coordclass.ClassGraph)
	assigned := classSet(t, coordclass.ClassGraph)
	base := completeOpenRequest(t, descriptor, assigned)
	if err := base.Validate(); err != nil {
		t.Fatalf("complete OpenRequest.Validate(): %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*OpenRequest)
		want   error
	}{
		{
			name: "missing exact contract",
			mutate: func(request *OpenRequest) {
				request.ExpectedContract = ""
			},
			want: ErrWrongContract,
		},
		{
			name: "partial component compatibility",
			mutate: func(request *OpenRequest) {
				request.ExpectedComponents = nil
			},
			want: ErrWrongFormat,
		},
		{
			name: "schema compatibility drift",
			mutate: func(request *OpenRequest) {
				request.ExpectedComponents[0].SchemaVersion = "other-schema"
			},
			want: ErrWrongFormat,
		},
		{
			name: "missing per-class requirement",
			mutate: func(request *OpenRequest) {
				request.ClassRequirements = nil
			},
			want: ErrMissingCapability,
		},
		{
			name: "required class transaction missing",
			mutate: func(request *OpenRequest) {
				request.Descriptor.Capabilities.Graph.Transactions = false
			},
			want: ErrMissingCapability,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := base.Clone()
			test.mutate(&request)
			if err := request.Validate(); !errors.Is(err, test.want) {
				t.Fatalf("OpenRequest.Validate() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestOpenRequestRequiresExactModeSpecificOpenAuthority(t *testing.T) {
	descriptor := testDescriptor(t, ProviderID("builtin-open-authority"), PhysicalIdentity("open-authority-identity"), coordclass.ClassGraph)
	descriptor.Capabilities.WriterFencing = true
	active := completeOpenRequest(t, descriptor, descriptor.Classes())

	t.Run("active requires exact durable authority", func(t *testing.T) {
		missing := active.Clone()
		missing.DurableActiveAuthority = DurableActiveOpenAuthority{}
		if err := missing.Validate(); !errors.Is(err, ErrInvalidOpenAuthority) {
			t.Fatalf("active OpenRequest.Validate() error = %v, want ErrInvalidOpenAuthority", err)
		}

		wrongGeneration, err := NewDurableActiveOpenAuthority(active.ExpectedGeneration+1, descriptor)
		if err != nil {
			t.Fatalf("NewDurableActiveOpenAuthority(): %v", err)
		}
		mismatched := active.Clone()
		mismatched.DurableActiveAuthority = wrongGeneration
		if err := mismatched.Validate(); !errors.Is(err, ErrInvalidOpenAuthority) {
			t.Fatalf("generation-mismatched active OpenRequest.Validate() error = %v, want ErrInvalidOpenAuthority", err)
		}

		withFence := active.Clone()
		withFence.AdmissionFence = fenceForDescriptor(t, descriptor, FenceRoleActiveVerification, active.ExpectedGeneration)
		if err := withFence.Validate(); !errors.Is(err, ErrInvalidOpenAuthority) {
			t.Fatalf("active OpenRequest.Validate() with fence error = %v, want ErrInvalidOpenAuthority", err)
		}
	})

	t.Run("read-only source requires exact source fence", func(t *testing.T) {
		request := active.Clone()
		request.Mode = OpenModeReadOnlySource
		request.DurableActiveAuthority = DurableActiveOpenAuthority{}
		request.AdmissionFence = fenceForDescriptor(t, descriptor, FenceRoleSource, request.ExpectedGeneration)
		if err := request.Validate(); err != nil {
			t.Fatalf("read-only OpenRequest.Validate(): %v", err)
		}

		missing := request.Clone()
		missing.AdmissionFence = nil
		if err := missing.Validate(); !errors.Is(err, ErrInvalidFence) {
			t.Fatalf("read-only OpenRequest.Validate() without fence error = %v, want ErrInvalidFence", err)
		}

		wrongRole := request.Clone()
		wrongRole.AdmissionFence = fenceForDescriptor(t, descriptor, FenceRolePopulatedDestination, request.ExpectedGeneration)
		if err := wrongRole.Validate(); !errors.Is(err, ErrInvalidFence) {
			t.Fatalf("read-only OpenRequest.Validate() wrong-role error = %v, want ErrInvalidFence", err)
		}

		wrongGeneration := request.Clone()
		wrongGeneration.AdmissionFence = fenceForDescriptor(t, descriptor, FenceRoleSource, request.ExpectedGeneration+1)
		if err := wrongGeneration.Validate(); !errors.Is(err, ErrInvalidFence) {
			t.Fatalf("read-only OpenRequest.Validate() wrong-generation error = %v, want ErrInvalidFence", err)
		}

		movedDescriptor := descriptor.Clone()
		movedDescriptor.Components[0].Locator = ComponentLocator("file:/city/open-authority-moved")
		wrongTarget := request.Clone()
		wrongTarget.AdmissionFence = fenceForDescriptor(t, movedDescriptor, FenceRoleSource, request.ExpectedGeneration)
		if err := wrongTarget.Validate(); !errors.Is(err, ErrFenceTargetMoved) {
			t.Fatalf("read-only OpenRequest.Validate() wrong-target error = %v, want ErrFenceTargetMoved", err)
		}

		released := request.Clone()
		if err := released.AdmissionFence.Release(context.Background()); err != nil {
			t.Fatalf("AdmissionFence.Release(): %v", err)
		}
		if err := released.Validate(); !errors.Is(err, ErrFenceNotHeld) {
			t.Fatalf("read-only OpenRequest.Validate() released-fence error = %v, want ErrFenceNotHeld", err)
		}
	})

	t.Run("admitted destination requires an exact destination fence", func(t *testing.T) {
		for _, role := range []FenceRole{FenceRolePopulatedDestination, FenceRoleNewDestinationReservation} {
			request := active.Clone()
			request.Mode = OpenModeAdmittedMigrationDestination
			request.DurableActiveAuthority = DurableActiveOpenAuthority{}
			request.AdmissionFence = fenceForDescriptor(t, descriptor, role, request.ExpectedGeneration)
			if err := request.Validate(); err != nil {
				t.Fatalf("admitted destination role %s Validate(): %v", role, err)
			}
		}

		wrongRole := active.Clone()
		wrongRole.Mode = OpenModeAdmittedMigrationDestination
		wrongRole.DurableActiveAuthority = DurableActiveOpenAuthority{}
		wrongRole.AdmissionFence = fenceForDescriptor(t, descriptor, FenceRoleSource, wrongRole.ExpectedGeneration)
		if err := wrongRole.Validate(); !errors.Is(err, ErrInvalidFence) {
			t.Fatalf("admitted destination wrong-role error = %v, want ErrInvalidFence", err)
		}
	})
}

func TestOpenBindingRevalidatesAdmissionFenceAndClosesRejectedProviderResult(t *testing.T) {
	descriptor := testDescriptor(t, ProviderID("builtin-open-live-fence"), PhysicalIdentity("open-live-fence-identity"), coordclass.ClassGraph)
	descriptor.Capabilities.WriterFencing = true
	adapters := testBeadsAdapters(t)

	tests := []struct {
		name   string
		mutate func(*recordingFence)
		want   error
	}{
		{
			name: "released",
			mutate: func(fence *recordingFence) {
				fence.held = false
			},
			want: ErrFenceNotHeld,
		},
		{
			name: "role changed",
			mutate: func(fence *recordingFence) {
				fence.role = FenceRolePopulatedDestination
			},
			want: ErrInvalidFence,
		},
		{
			name: "generation changed",
			mutate: func(fence *recordingFence) {
				fence.generation++
			},
			want: ErrInvalidFence,
		},
		{
			name: "target changed",
			mutate: func(fence *recordingFence) {
				fence.target.Components[0].Locator = ComponentLocator("file:/city/open-live-fence-moved")
			},
			want: ErrInvalidFence,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := completeOpenRequest(t, descriptor, descriptor.Classes())
			request.Mode = OpenModeReadOnlySource
			request.DurableActiveAuthority = DurableActiveOpenAuthority{}
			request.AdmissionFence = fenceForDescriptor(t, descriptor, FenceRoleSource, request.ExpectedGeneration)
			opened := &directOpenedBinding{descriptor: descriptor, capabilities: descriptor.Capabilities, graph: adapters.Graph, graphOK: true}
			provider := &recordingProvider{
				opened: opened,
				mutateOpen: func(request *OpenRequest) {
					test.mutate(request.AdmissionFence.(*recordingFence))
				},
			}

			_, err := OpenBinding(context.Background(), provider, request)
			if !errors.Is(err, test.want) {
				t.Fatalf("OpenBinding() error = %v, want %v", err, test.want)
			}
			if opened.closeCalls != 1 {
				t.Fatalf("OpenBinding() rejected result close calls = %d, want 1", opened.closeCalls)
			}
		})
	}
}

func TestOpenBindingPreservesProviderFenceAndCleanupErrorsForNonNilErrorResult(t *testing.T) {
	descriptor := testDescriptor(t, ProviderID("builtin-open-error-fence"), PhysicalIdentity("open-error-fence-identity"), coordclass.ClassGraph)
	descriptor.Capabilities.WriterFencing = true
	request := completeOpenRequest(t, descriptor, descriptor.Classes())
	request.Mode = OpenModeReadOnlySource
	request.DurableActiveAuthority = DurableActiveOpenAuthority{}
	request.AdmissionFence = fenceForDescriptor(t, descriptor, FenceRoleSource, request.ExpectedGeneration)
	adapters := testBeadsAdapters(t)
	providerErr := errors.New("provider open failed")
	closeErr := errors.New("returned binding close failed")
	opened := &directOpenedBinding{descriptor: descriptor, capabilities: descriptor.Capabilities, graph: adapters.Graph, graphOK: true, closeErr: closeErr}
	provider := &recordingProvider{
		opened:  opened,
		openErr: providerErr,
		mutateOpen: func(request *OpenRequest) {
			request.AdmissionFence.(*recordingFence).held = false
		},
	}

	_, err := OpenBinding(context.Background(), provider, request)
	if !errors.Is(err, providerErr) || !errors.Is(err, ErrFenceNotHeld) || !errors.Is(err, closeErr) {
		t.Fatalf("OpenBinding() error = %v, want provider, fence, and cleanup errors", err)
	}
	if opened.closeCalls != 1 {
		t.Fatalf("OpenBinding() returned binding close calls = %d, want 1", opened.closeCalls)
	}
	var cleanup *RejectedOpenedBindingCleanupError
	if !errors.As(err, &cleanup) {
		t.Fatalf("OpenBinding() error = %T, want *RejectedOpenedBindingCleanupError", err)
	}
}

func TestOpenRequestDoesNotDuplicateActivationGuardRequirement(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "provider_lifecycle.go", nil, 0)
	if err != nil {
		t.Fatalf("parse provider lifecycle API: %v", err)
	}
	for _, declaration := range file.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.TYPE {
			continue
		}
		for _, specification := range general.Specs {
			typeSpec, ok := specification.(*ast.TypeSpec)
			if !ok || typeSpec.Name.Name != "OpenRequest" {
				continue
			}
			structure, ok := typeSpec.Type.(*ast.StructType)
			if !ok {
				t.Fatal("OpenRequest is no longer a struct")
			}
			for _, field := range structure.Fields.List {
				for _, name := range field.Names {
					if name.Name == "RequireGuardedActivation" {
						t.Fatal("OpenRequest must not duplicate activation guard policy")
					}
				}
			}
			return
		}
	}
	t.Fatal("OpenRequest declaration not found")
}

func TestValidateNoDescriptorOverlapRejectsPhysicalAliases(t *testing.T) {
	left := testDescriptor(t, ProviderID("builtin-alias"), PhysicalIdentity("same-physical-component"), coordclass.ClassGraph)
	right := testDescriptor(t, ProviderID("builtin-alias"), PhysicalIdentity("same-physical-component"), coordclass.ClassSessions)
	right.Components[0].Locator = ComponentLocator("file:/another/canonical-path")

	err := ValidateNoDescriptorOverlap([]NamedDescriptor{
		{Name: BindingName("left"), Descriptor: left},
		{Name: BindingName("right"), Descriptor: right},
	})
	if !errors.Is(err, ErrDescriptorOverlap) {
		t.Fatalf("ValidateNoDescriptorOverlap() error = %v, want ErrDescriptorOverlap", err)
	}
}

func TestDescriptorRequiresExplicitMarkerAndClassCapabilities(t *testing.T) {
	descriptor := testDescriptor(t, ProviderID("builtin-descriptor"), PhysicalIdentity("descriptor-identity"), coordclass.ClassGraph)

	missingMarker := descriptor.Clone()
	missingMarker.Components[0].Marker = MarkerState{}
	if err := missingMarker.Validate(); !errors.Is(err, ErrInvalidDescriptor) {
		t.Fatalf("descriptor without marker error = %v, want ErrInvalidDescriptor", err)
	}

	missingCapability := descriptorWithoutCapabilities(descriptor)
	if err := missingCapability.Validate(); !errors.Is(err, ErrMissingCapability) {
		t.Fatalf("descriptor without class capability error = %v, want ErrMissingCapability", err)
	}

	extraCapability := descriptor.Clone()
	extraCapability.Capabilities.Sessions.Available = true
	if err := extraCapability.Validate(); !errors.Is(err, ErrInvalidDescriptor) {
		t.Fatalf("descriptor with unowned available class error = %v, want ErrInvalidDescriptor", err)
	}

	invalidUnavailableCapability := descriptor.Clone()
	invalidUnavailableCapability.Capabilities.Sessions.Transactions = true
	if err := invalidUnavailableCapability.Validate(); !errors.Is(err, ErrInvalidDescriptor) {
		t.Fatalf("descriptor with unavailable transactions error = %v, want ErrInvalidDescriptor", err)
	}
}

func TestProviderBoundaryValuesRequireV1AndCanonicalSHA256Digests(t *testing.T) {
	descriptor := testDescriptor(t, ProviderID("builtin-validation"), PhysicalIdentity("validation-identity"), coordclass.ClassGraph)
	target := fenceForDescriptor(t, descriptor, FenceRoleSource, Generation(1)).Target()
	retained := testRetainedSource(PhysicalIdentity("retained-validation"))
	guardRequest := GuardInstallRequest{
		Attempt:          AttemptID("guard-validation"),
		Generation:       Generation(1),
		Source:           retained,
		Component:        retained.Component,
		PhysicalIdentity: retained.PhysicalIdentity,
		Role:             GuardRoleDenyWrite,
	}
	guardReceipt := matchingGuardReceipt(guardRequest)
	fixture := newWorkMigrationFixture(t)

	tests := []struct {
		name     string
		validate func() error
		want     error
	}{
		{
			name: "descriptor version two",
			validate: func() error {
				value := descriptor.Clone()
				value.Version = 2
				return value.Validate()
			},
			want: ErrInvalidDescriptor,
		},
		{
			name: "fence target version two",
			validate: func() error {
				value := target.Clone()
				value.Version = 2
				return value.Validate()
			},
			want: ErrInvalidFenceTarget,
		},
		{
			name: "retained source version two",
			validate: func() error {
				value := retained.Clone()
				value.Version = 2
				return value.Validate()
			},
			want: ErrInvalidRetainedSource,
		},
		{
			name: "guard receipt version two",
			validate: func() error {
				value := guardReceipt
				value.Version = 2
				return value.Validate()
			},
			want: ErrInvalidGuard,
		},
		{
			name: "descriptor abbreviated config digest",
			validate: func() error {
				value := descriptor.Clone()
				value.ConfigRefDigest = ConfigRefDigest("sha256:abc")
				return value.Validate()
			},
			want: ErrInvalidDescriptor,
		},
		{
			name: "retained source malformed witness digest",
			validate: func() error {
				value := retained.Clone()
				value.WitnessDigest = "sha256:ABC"
				return value.Validate()
			},
			want: ErrInvalidRetainedSource,
		},
		{
			name: "work preparation abbreviated descriptor identity",
			validate: func() error {
				value := fixture.preparation.Clone()
				value.SourceIdentity = BindingIdentity("sha256:abc")
				return value.Validate()
			},
			want: ErrInvalidWorkParticipant,
		},
		{
			name: "work preparation abbreviated config digest",
			validate: func() error {
				value := fixture.preparation.Clone()
				value.ConfigDigest = ConfigRefDigest("sha256:abc")
				return value.Validate()
			},
			want: ErrInvalidWorkParticipant,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.validate(); !errors.Is(err, test.want) {
				t.Fatalf("validation error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestProviderBoundaryValuesRejectZeroGeneration(t *testing.T) {
	descriptor := testDescriptor(t, ProviderID("builtin-generation"), PhysicalIdentity("generation-identity"), coordclass.ClassGraph)
	target := fenceForDescriptor(t, descriptor, FenceRoleSource, Generation(1)).Target()
	guard := testMigrationGuard(t, Generation(1))
	t.Cleanup(func() {
		if err := guard.Release(); err != nil {
			t.Errorf("guard.Release(): %v", err)
		}
	})
	scope := testMigrationGuardScope(t, guard)
	retained := testRetainedSource(PhysicalIdentity("generation-retained"))
	guardRequest := GuardInstallRequest{
		Attempt:          AttemptID("generation-guard"),
		Generation:       Generation(1),
		Source:           retained,
		Component:        retained.Component,
		PhysicalIdentity: retained.PhysicalIdentity,
		Role:             GuardRoleDenyWrite,
	}
	guardReceipt := matchingGuardReceipt(guardRequest)
	fixture := newWorkMigrationFixture(t)
	activationReceipt := ParticipantReceipt{
		Version:            1,
		Kind:               ParticipantReceiptBindingActivation,
		Attempt:            AttemptID("generation-activation"),
		Generation:         Generation(1),
		Participant:        "binding",
		DescriptorIdentity: fixture.destinationIdentity,
		ReceiptID:          "activation-receipt",
	}

	tests := []struct {
		name         string
		validate     func() error
		want         error
		wantAnyError bool
	}{
		{
			name: "migration guard identity",
			validate: func() error {
				_, err := newMigrationGuardIdentity(testMigrationGuardDirectory(t), 1, 1, 0)
				return err
			},
			want: ErrInvalidMigrationGuard,
		},
		{
			name: "migration guard acquisition",
			validate: func() error {
				_, err := AcquireMigrationGuard(context.Background(), testMigrationGuardDirectory(t), 0)
				return err
			},
			want: ErrInvalidMigrationGuard,
		},
		{
			name: "fence request",
			validate: func() error {
				return FenceRequest{Target: target, GuardScope: scope, ExpectedGeneration: 0, Components: []ComponentID{ComponentID("component")}, Role: FenceRoleSource}.Validate()
			},
			want: ErrInvalidFence,
		},
		{
			name: "fenced inspection request",
			validate: func() error {
				return FencedInspectionRequest{Target: target, Fence: fenceForDescriptor(t, descriptor, FenceRoleSource, 0), ExpectedGeneration: 0}.Validate(context.Background())
			},
			want: ErrInvalidFence,
		},
		{
			name: "open request",
			validate: func() error {
				request := completeOpenRequest(t, descriptor, descriptor.Classes())
				request.ExpectedGeneration = 0
				return request.Validate()
			},
			wantAnyError: true,
		},
		{
			name: "commit decision",
			validate: func() error {
				return CommitDecision{Attempt: AttemptID("generation-decision"), Generation: 0, Decided: true}.Validate()
			},
			want: ErrCommitNotDecided,
		},
		{
			name: "guard install",
			validate: func() error {
				request := guardRequest
				request.Generation = 0
				return request.Validate()
			},
			want: ErrInvalidGuard,
		},
		{
			name: "guard discovery",
			validate: func() error {
				return GuardDiscoverRequest{Attempt: guardRequest.Attempt, Generation: 0, Source: guardRequest.Source, Component: guardRequest.Component, PhysicalIdentity: guardRequest.PhysicalIdentity, Role: guardRequest.Role}.Validate()
			},
			want: ErrInvalidGuard,
		},
		{
			name: "guard receipt",
			validate: func() error {
				receipt := guardReceipt
				receipt.Generation = 0
				return receipt.Validate()
			},
			want: ErrInvalidGuard,
		},
		{
			name: "participant receipt",
			validate: func() error {
				receipt := activationReceipt
				receipt.Generation = 0
				return receipt.Validate()
			},
			wantAnyError: true,
		},
		{
			name: "work prepare request",
			validate: func() error {
				request := fixture.prepare.Clone()
				request.Generation = 0
				request.SourceFence = fenceForDescriptor(t, request.Source, FenceRoleSource, 0)
				request.DestinationFence = fenceForDescriptor(t, request.Destination, FenceRolePopulatedDestination, 0)
				return request.Validate(context.Background())
			},
			want: ErrInvalidWorkParticipant,
		},
		{
			name: "work preparation identity",
			validate: func() error {
				identity := fixture.preparation.Clone()
				identity.Generation = 0
				return identity.Validate()
			},
			want: ErrInvalidWorkParticipant,
		},
		{
			name: "work prepared",
			validate: func() error {
				prepared := fixture.prepared.Clone()
				prepared.Generation = 0
				prepared.Preparation.Generation = 0
				return prepared.Validate()
			},
			want: ErrInvalidWorkParticipant,
		},
		{
			name: "work proof",
			validate: func() error {
				proof := fixture.proof.Clone()
				proof.Generation = 0
				proof.Preparation.Generation = 0
				return proof.Validate()
			},
			want: ErrInvalidWorkParticipant,
		},
		{
			name: "work progress",
			validate: func() error {
				preparation := fixture.preparation.Clone()
				preparation.Generation = 0
				return WorkProgress{Version: 1, Attempt: fixture.prepare.Attempt, Generation: 0, Participant: fixture.participant, Preparation: preparation}.Validate()
			},
			want: ErrInvalidWorkParticipant,
		},
		{
			name: "retained work request",
			validate: func() error {
				return RetainedWorkRequest{Attempt: AttemptID("generation-retained"), Generation: 0, Participant: fixture.participant, Source: testRetainedWorkSource(fixture.participant), ExpectedContract: ContractVersion("storage-v1")}.Validate()
			},
			want: ErrInvalidWorkParticipant,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.validate()
			if test.wantAnyError {
				if err == nil {
					t.Fatal("validation error = nil, want rejection")
				}
				return
			}
			if !errors.Is(err, test.want) {
				t.Fatalf("validation error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestDescriptorIdentityIncludesTypedCapabilities(t *testing.T) {
	descriptor := testDescriptor(t, ProviderID("builtin-identity"), PhysicalIdentity("identity-component"), coordclass.ClassGraph)
	withCapabilities, err := descriptor.Identity()
	if err != nil {
		t.Fatalf("Descriptor.Identity(): %v", err)
	}
	changed := descriptor.Clone()
	changed.Capabilities.Graph.Transactions = false
	withoutTransactions, err := changed.Identity()
	if err != nil {
		t.Fatalf("Descriptor.Identity(changed): %v", err)
	}
	if withCapabilities == withoutTransactions {
		t.Fatalf("descriptor identity did not change after capability change: %q", withCapabilities)
	}
}

func TestDescriptorIdentitySeparatesDelimitedFieldsAndRetainedOpenerData(t *testing.T) {
	left := testDescriptor(t, ProviderID("builtin-identity"), PhysicalIdentity("identity-component"), coordclass.ClassGraph)
	right := left.Clone()

	// These two values produce the same bytes with a NUL-joined encoder:
	// schema-v1\x00abi-v1\x00abi-v2. They must remain distinct descriptor
	// identities because field boundaries are part of the contract.
	left.Components[0].SchemaVersion = "schema-v1\x00abi-v1"
	left.Components[0].ABIVersion = "abi-v2"
	right.Components[0].SchemaVersion = "schema-v1"
	right.Components[0].ABIVersion = "abi-v1\x00abi-v2"

	leftIdentity, err := left.Identity()
	if err != nil {
		t.Fatalf("left Descriptor.Identity(): %v", err)
	}
	rightIdentity, err := right.Identity()
	if err != nil {
		t.Fatalf("right Descriptor.Identity(): %v", err)
	}
	if leftIdentity == rightIdentity {
		t.Fatalf("descriptor identities collided across encoded field boundaries: %q", leftIdentity)
	}

	retained := testRetainedSource(PhysicalIdentity("identity-component"))
	retained.Provider = right.Provider
	retained.Component = right.Components[0].ID
	retained.Classes = right.Components[0].Classes
	retained.SemanticContractVersion = right.SemanticContractVersion
	retained.Format = right.Components[0].Format
	retained.SchemaVersion = right.Components[0].SchemaVersion
	retained.ABIVersion = right.Components[0].ABIVersion
	retained.ConfigRefDigest = right.ConfigRefDigest
	right.RetainedSource = &retained
	withRetained, err := right.Identity()
	if err != nil {
		t.Fatalf("Descriptor.Identity() with retained opener: %v", err)
	}
	retained.ReopenData = []byte("different opaque retained opener")
	right.RetainedSource = &retained
	withChangedRetained, err := right.Identity()
	if err != nil {
		t.Fatalf("Descriptor.Identity() with changed retained opener: %v", err)
	}
	if withRetained == withChangedRetained {
		t.Fatal("descriptor identity did not bind retained opener data")
	}
}

func TestRecoverRetainedGuardsDiscoversReceiptLostBeforeFsync(t *testing.T) {
	source := testRetainedSource(PhysicalIdentity("retained-work"))
	request := GuardInstallRequest{
		Attempt:          AttemptID("attempt-1"),
		Generation:       Generation(3),
		Source:           source,
		Component:        ComponentID("component"),
		PhysicalIdentity: source.PhysicalIdentity,
		Role:             GuardRoleDenyWrite,
	}
	receipt := matchingGuardReceipt(request)
	lifecycle := &recordingGuardLifecycle{discovery: GuardDiscovery{Found: true, Receipt: receipt}}

	receipts, err := RecoverRetainedGuards(context.Background(), lifecycle, []FencedGuardInstallRequest{fencedGuardInstallRequest(t, request)}, discardGuardReceipt)
	if err != nil {
		t.Fatalf("RecoverRetainedGuards(): %v", err)
	}
	if len(receipts) != 1 || receipts[0].ReceiptID != receipt.ReceiptID {
		t.Fatalf("receipts = %#v, want discovered receipt", receipts)
	}
	if lifecycle.discoverCalls != 1 || lifecycle.installCalls != 0 || lifecycle.verifyCalls != 1 {
		t.Fatalf("guard lifecycle calls = discover:%d install:%d verify:%d, want 1:0:1", lifecycle.discoverCalls, lifecycle.installCalls, lifecycle.verifyCalls)
	}
}

func TestGuardDiscoveryRejectsReceiptWhenNotFound(t *testing.T) {
	request := GuardInstallRequest{
		Attempt:          AttemptID("guard-discovery-attempt"),
		Generation:       Generation(3),
		Source:           testRetainedSource(PhysicalIdentity("guard-discovery-source")),
		Component:        ComponentID("component"),
		PhysicalIdentity: PhysicalIdentity("guard-discovery-source"),
		Role:             GuardRoleDenyWrite,
	}
	discovery := GuardDiscovery{Found: false, Receipt: matchingGuardReceipt(request)}
	if err := discovery.Validate(); !errors.Is(err, ErrInvalidGuard) {
		t.Fatalf("GuardDiscovery.Validate() error = %v, want ErrInvalidGuard", err)
	}
}

func TestRecoverRetainedGuardsInstallsAfterExplicitAbsentDiscovery(t *testing.T) {
	request := GuardInstallRequest{
		Attempt:          AttemptID("guard-discovery-absent-attempt"),
		Generation:       Generation(3),
		Source:           testRetainedSource(PhysicalIdentity("guard-discovery-absent-source")),
		Component:        ComponentID("component"),
		PhysicalIdentity: PhysicalIdentity("guard-discovery-absent-source"),
		Role:             GuardRoleDenyWrite,
	}
	lifecycle := &recordingGuardLifecycle{discovery: GuardDiscovery{Found: false, Receipt: GuardReceipt{}}}

	receipts, err := RecoverRetainedGuards(context.Background(), lifecycle, []FencedGuardInstallRequest{fencedGuardInstallRequest(t, request)}, discardGuardReceipt)
	if err != nil {
		t.Fatalf("RecoverRetainedGuards(): %v", err)
	}
	if len(receipts) != 1 || receipts[0].ReceiptID == "" {
		t.Fatalf("recovered receipts = %#v, want one installed receipt", receipts)
	}
	if lifecycle.discoverCalls != 1 || lifecycle.installCalls != 1 || lifecycle.verifyCalls != 1 {
		t.Fatalf("guard lifecycle calls = discover:%d install:%d verify:%d, want 1:1:1", lifecycle.discoverCalls, lifecycle.installCalls, lifecycle.verifyCalls)
	}
}

func TestRecoverRetainedGuardsPermitsMultiClassSourceOwnership(t *testing.T) {
	source := testRetainedSource(PhysicalIdentity("retained-shared-source"))
	source.Classes = classSet(t, coordclass.ClassWork, coordclass.ClassGraph)
	request := GuardInstallRequest{
		Attempt:          AttemptID("attempt-shared-source"),
		Generation:       Generation(3),
		Source:           source,
		Component:        source.Component,
		PhysicalIdentity: source.PhysicalIdentity,
		Role:             GuardRoleDenyWrite,
	}
	lifecycle := &recordingGuardLifecycle{}

	receipts, err := RecoverRetainedGuards(context.Background(), lifecycle, []FencedGuardInstallRequest{fencedGuardInstallRequest(t, request)}, discardGuardReceipt)
	if err != nil {
		t.Fatalf("RecoverRetainedGuards(): %v", err)
	}
	if len(receipts) != 1 || !receipts[0].Classes.Equal(source.Classes) {
		t.Fatalf("receipts = %#v, want one guard receipt retaining the complete source class set", receipts)
	}
}

func TestRecoverRetainedGuardsAcceptsSourceCoveredByAggregateFenceTarget(t *testing.T) {
	source := testRetainedSource(PhysicalIdentity("aggregate-guard-source"))
	install := GuardInstallRequest{
		Attempt:          AttemptID("aggregate-guard-attempt"),
		Generation:       Generation(3),
		Source:           source,
		Component:        source.Component,
		PhysicalIdentity: source.PhysicalIdentity,
		Role:             GuardRoleDenyWrite,
	}
	classes := classSet(t, coordclass.ClassWork, coordclass.ClassGraph)
	target, err := NewFenceTarget(source.Provider, classes, []FenceComponentTarget{
		{
			ID:               source.Component,
			Locator:          ComponentLocator("retained:" + string(source.PhysicalIdentity)),
			PhysicalIdentity: source.PhysicalIdentity,
			Classes:          source.Classes,
		},
		{
			ID:               ComponentID("graph"),
			Locator:          ComponentLocator("file:/city/aggregate-guard-graph"),
			PhysicalIdentity: PhysicalIdentity("aggregate-guard-graph"),
			Classes:          classSet(t, coordclass.ClassGraph),
		},
	})
	if err != nil {
		t.Fatalf("NewFenceTarget(): %v", err)
	}
	request := FencedGuardInstallRequest{
		GuardInstallRequest: install,
		SourceFence:         &recordingFence{target: target, role: FenceRoleSource, generation: install.Generation, held: true},
	}
	lifecycle := &recordingGuardLifecycle{}
	if _, err := RecoverRetainedGuards(context.Background(), lifecycle, []FencedGuardInstallRequest{request}, discardGuardReceipt); err != nil {
		t.Fatalf("RecoverRetainedGuards(): %v", err)
	}
	if lifecycle.discoverCalls != 1 || lifecycle.installCalls != 1 || lifecycle.verifyCalls != 1 {
		t.Fatalf("aggregate source lifecycle calls = discover:%d install:%d verify:%d, want 1:1:1", lifecycle.discoverCalls, lifecycle.installCalls, lifecycle.verifyCalls)
	}
}

func TestRecoverRetainedGuardsPrevalidatesWholePlanAndPersistsVerifiedReceipts(t *testing.T) {
	first := GuardInstallRequest{
		Attempt:          AttemptID("attempt-prevalidate"),
		Generation:       Generation(3),
		Source:           testRetainedSource(PhysicalIdentity("retained-first")),
		Component:        ComponentID("component"),
		PhysicalIdentity: PhysicalIdentity("retained-first"),
		Role:             GuardRoleDenyWrite,
	}
	second := first
	second.Source = second.Source.Clone()
	second.Source.PhysicalIdentity = PhysicalIdentity("retained-second")
	second.PhysicalIdentity = second.Source.PhysicalIdentity

	t.Run("invalid later request cannot mutate earlier request", func(t *testing.T) {
		invalid := second
		invalid.Role = GuardRole(99)
		lifecycle := &recordingGuardLifecycle{}
		_, err := RecoverRetainedGuards(context.Background(), lifecycle, []FencedGuardInstallRequest{fencedGuardInstallRequest(t, first), fencedGuardInstallRequest(t, invalid)}, func(context.Context, GuardReceipt) error { return nil })
		if !errors.Is(err, ErrInvalidGuard) {
			t.Fatalf("RecoverRetainedGuards() error = %v, want ErrInvalidGuard", err)
		}
		if lifecycle.discoverCalls != 0 || lifecycle.installCalls != 0 || lifecycle.verifyCalls != 0 {
			t.Fatalf("invalid full plan mutated provider: discover:%d install:%d verify:%d", lifecycle.discoverCalls, lifecycle.installCalls, lifecycle.verifyCalls)
		}
	})

	t.Run("transfer role cannot install alongside deny-write authority", func(t *testing.T) {
		transfer := first
		transfer.Source = transfer.Source.Clone()
		transfer.Role = GuardRoleTransfer
		lifecycle := &recordingGuardLifecycle{}
		_, err := RecoverRetainedGuards(context.Background(), lifecycle, []FencedGuardInstallRequest{fencedGuardInstallRequest(t, first), fencedGuardInstallRequest(t, transfer)}, discardGuardReceipt)
		if !errors.Is(err, ErrInvalidGuard) {
			t.Fatalf("RecoverRetainedGuards() error = %v, want ErrInvalidGuard", err)
		}
		if lifecycle.discoverCalls != 0 || lifecycle.installCalls != 0 || lifecycle.verifyCalls != 0 {
			t.Fatalf("two-role plan mutated provider: discover:%d install:%d verify:%d", lifecycle.discoverCalls, lifecycle.installCalls, lifecycle.verifyCalls)
		}
	})

	t.Run("duplicate full plan cannot mutate provider", func(t *testing.T) {
		lifecycle := &recordingGuardLifecycle{}
		_, err := RecoverRetainedGuards(context.Background(), lifecycle, []FencedGuardInstallRequest{fencedGuardInstallRequest(t, first), fencedGuardInstallRequest(t, first)}, func(context.Context, GuardReceipt) error { return nil })
		if !errors.Is(err, ErrInvalidGuard) {
			t.Fatalf("RecoverRetainedGuards() error = %v, want ErrInvalidGuard", err)
		}
		if lifecycle.discoverCalls != 0 || lifecycle.installCalls != 0 || lifecycle.verifyCalls != 0 {
			t.Fatalf("duplicate full plan mutated provider: discover:%d install:%d verify:%d", lifecycle.discoverCalls, lifecycle.installCalls, lifecycle.verifyCalls)
		}
	})

	t.Run("verified receipts are persisted one at a time", func(t *testing.T) {
		lifecycle := &recordingGuardLifecycle{}
		var persisted []GuardReceipt
		receipts, err := RecoverRetainedGuards(context.Background(), lifecycle, []FencedGuardInstallRequest{fencedGuardInstallRequest(t, first), fencedGuardInstallRequest(t, second)}, func(_ context.Context, receipt GuardReceipt) error {
			persisted = append(persisted, receipt)
			return nil
		})
		if err != nil {
			t.Fatalf("RecoverRetainedGuards(): %v", err)
		}
		if len(receipts) != 2 || len(persisted) != 2 || persisted[0].PhysicalIdentity != first.PhysicalIdentity || persisted[1].PhysicalIdentity != second.PhysicalIdentity {
			t.Fatalf("receipts/persisted = %#v/%#v, want each verified component persisted in plan order", receipts, persisted)
		}
		if lifecycle.verifyCalls != 2 || lifecycle.installCalls != 2 {
			t.Fatalf("successful lifecycle calls = install:%d verify:%d, want 2:2", lifecycle.installCalls, lifecycle.verifyCalls)
		}
	})
}

func TestRecoverRetainedGuardsUsesExactProviderLifecycleAndPreResolvesEveryGroup(t *testing.T) {
	newInstall := func(provider ProviderID, identity PhysicalIdentity) GuardInstallRequest {
		source := testRetainedSource(identity)
		source.Provider = provider
		return GuardInstallRequest{
			Attempt:          AttemptID("mixed-provider-recovery"),
			Generation:       Generation(3),
			Source:           source,
			Component:        source.Component,
			PhysicalIdentity: identity,
			Role:             GuardRoleDenyWrite,
		}
	}
	first := newInstall(ProviderID("guard-provider-one"), PhysicalIdentity("guard-source-one"))
	second := newInstall(ProviderID("guard-provider-two"), PhysicalIdentity("guard-source-two"))
	requests := []FencedGuardInstallRequest{fencedGuardInstallRequest(t, first), fencedGuardInstallRequest(t, second)}

	t.Run("each provider receives only its subgroup", func(t *testing.T) {
		firstLifecycle := &recordingGuardLifecycle{provider: first.Source.Provider}
		secondLifecycle := &recordingGuardLifecycle{provider: second.Source.Provider}
		resolver := &recordingGuardLifecycleResolver{lifecycles: map[ProviderID]RetainedGuardLifecycle{
			first.Source.Provider:  firstLifecycle,
			second.Source.Provider: secondLifecycle,
		}}

		receipts, err := RecoverRetainedGuards(context.Background(), resolver, requests, discardGuardReceipt)
		if err != nil {
			t.Fatalf("RecoverRetainedGuards(): %v", err)
		}
		if len(receipts) != 2 || receipts[0].Provider != first.Source.Provider || receipts[1].Provider != second.Source.Provider {
			t.Fatalf("mixed-provider receipts = %#v, want one receipt from each exact provider", receipts)
		}
		if firstLifecycle.discoverCalls != 1 || firstLifecycle.installCalls != 1 || firstLifecycle.verifyCalls != 1 || secondLifecycle.discoverCalls != 1 || secondLifecycle.installCalls != 1 || secondLifecycle.verifyCalls != 1 {
			t.Fatalf("mixed-provider calls = first:%d/%d/%d second:%d/%d/%d, want 1/1/1 for each", firstLifecycle.discoverCalls, firstLifecycle.installCalls, firstLifecycle.verifyCalls, secondLifecycle.discoverCalls, secondLifecycle.installCalls, secondLifecycle.verifyCalls)
		}
		if firstLifecycle.wrongProviderCalls != 0 || secondLifecycle.wrongProviderCalls != 0 {
			t.Fatalf("wrong-provider calls = first:%d second:%d, want zero", firstLifecycle.wrongProviderCalls, secondLifecycle.wrongProviderCalls)
		}
	})

	t.Run("missing later provider resolves before any provider mutation", func(t *testing.T) {
		firstLifecycle := &recordingGuardLifecycle{provider: first.Source.Provider}
		resolver := &recordingGuardLifecycleResolver{lifecycles: map[ProviderID]RetainedGuardLifecycle{
			first.Source.Provider: firstLifecycle,
		}}

		_, err := RecoverRetainedGuards(context.Background(), resolver, requests, discardGuardReceipt)
		if !errors.Is(err, ErrMissingCapability) {
			t.Fatalf("RecoverRetainedGuards() error = %v, want ErrMissingCapability", err)
		}
		if firstLifecycle.discoverCalls != 0 || firstLifecycle.installCalls != 0 || firstLifecycle.verifyCalls != 0 {
			t.Fatalf("unresolved plan mutated first provider: discover:%d install:%d verify:%d", firstLifecycle.discoverCalls, firstLifecycle.installCalls, firstLifecycle.verifyCalls)
		}
	})
}

func TestTransitionRetainedGuardRequiresPredecisionAuthorityAndMatchingSourceFence(t *testing.T) {
	source := testRetainedSource(PhysicalIdentity("retained-transition"))
	install := GuardInstallRequest{
		Attempt:          AttemptID("attempt-transition"),
		Generation:       Generation(4),
		Source:           source,
		Component:        ComponentID("component"),
		PhysicalIdentity: source.PhysicalIdentity,
		Role:             GuardRoleDenyWrite,
	}
	current := matchingGuardReceipt(install)
	matchingDescriptor := testDescriptor(t, source.Provider, source.PhysicalIdentity, coordclass.ClassWork)
	target, err := NewFenceTarget(source.Provider, classSet(t, coordclass.ClassWork), []FenceComponentTarget{{
		ID:               ComponentID("component"),
		Locator:          matchingDescriptor.Components[0].Locator,
		PhysicalIdentity: source.PhysicalIdentity,
		Classes:          classSet(t, coordclass.ClassWork),
	}})
	if err != nil {
		t.Fatalf("NewFenceTarget(): %v", err)
	}
	matchingFence := &recordingFence{target: target, role: FenceRoleSource, generation: Generation(4), held: true}
	release := GuardTransitionRequest{
		Current:          current,
		Release:          true,
		SourceDescriptor: matchingDescriptor,
		SourceFence:      matchingFence,
	}
	lifecycle := &recordingGuardLifecycle{transitionReceipt: current}

	_, err = TransitionRetainedGuard(context.Background(), lifecycle, release)
	if !errors.Is(err, ErrInvalidGuard) {
		t.Fatalf("release without predecision authority error = %v, want ErrInvalidGuard", err)
	}
	if lifecycle.transitionCalls != 0 {
		t.Fatalf("TransitionRetainedGuard() called provider %d times without predecision authority", lifecycle.transitionCalls)
	}

	authority := testPredecisionAbandonmentAuthority(t, current, matchingDescriptor, matchingFence)
	release.Abandonment = &authority

	wrongDescriptor := matchingDescriptor.Clone()
	wrongDescriptor.Components[0].PhysicalIdentity = PhysicalIdentity("different-source")
	release.SourceDescriptor = wrongDescriptor
	_, err = TransitionRetainedGuard(context.Background(), lifecycle, release)
	if !errors.Is(err, ErrInvalidGuard) {
		t.Fatalf("release with changed source descriptor error = %v, want ErrInvalidGuard", err)
	}
	if lifecycle.transitionCalls != 0 {
		t.Fatalf("TransitionRetainedGuard() called provider %d times with a changed source descriptor", lifecycle.transitionCalls)
	}

	release.SourceDescriptor = matchingDescriptor
	got, err := TransitionRetainedGuard(context.Background(), lifecycle, release)
	if err != nil {
		t.Fatalf("TransitionRetainedGuard() with predecision authority: %v", err)
	}
	if !guardReceiptsEqual(got, current) || lifecycle.transitionCalls != 1 {
		t.Fatalf("released guard/calls = %#v/%d, want exact current receipt and one call", got, lifecycle.transitionCalls)
	}
}

func TestTransitionRetainedGuardRejectsForeignPredecisionAuthorityAndPostdecisionRelease(t *testing.T) {
	source := testRetainedSource(PhysicalIdentity("retained-authority"))
	install := GuardInstallRequest{
		Attempt:          AttemptID("attempt-authority"),
		Generation:       Generation(4),
		Source:           source,
		Component:        source.Component,
		PhysicalIdentity: source.PhysicalIdentity,
		Role:             GuardRoleDenyWrite,
	}
	current := matchingGuardReceipt(install)
	descriptor := testDescriptor(t, source.Provider, source.PhysicalIdentity, coordclass.ClassWork)
	fence := fenceForDescriptor(t, descriptor, FenceRoleSource, current.Generation).(*recordingFence)
	authority := testPredecisionAbandonmentAuthority(t, current, descriptor, fence)

	for _, test := range []struct {
		name   string
		mutate func(*PredecisionAbandonmentAuthority, *GuardReceipt)
	}{
		{
			name: "zero authority",
			mutate: func(authority *PredecisionAbandonmentAuthority, _ *GuardReceipt) {
				*authority = PredecisionAbandonmentAuthority{}
			},
		},
		{
			name: "other receipt",
			mutate: func(authority *PredecisionAbandonmentAuthority, _ *GuardReceipt) {
				authority.receiptID = "other-guard-receipt"
			},
		},
		{
			name: "other descriptor",
			mutate: func(authority *PredecisionAbandonmentAuthority, _ *GuardReceipt) {
				authority.sourceDescriptorIdentity = testBindingIdentity("other-source-descriptor")
			},
		},
		{
			name: "other generation",
			mutate: func(authority *PredecisionAbandonmentAuthority, _ *GuardReceipt) {
				authority.generation++
			},
		},
		{
			name: "postdecision transfer",
			mutate: func(_ *PredecisionAbandonmentAuthority, current *GuardReceipt) {
				current.Role = GuardRoleTransfer
				current.TransferState = GuardTransferDecided
				current.TransferParticipant = "newer-binding"
				current.TransferDestinationIdentity = testBindingIdentity("newer-destination")
				current.TransferReceiptKind = ParticipantReceiptBindingActivation
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidateAuthority := authority
			candidateCurrent := current
			test.mutate(&candidateAuthority, &candidateCurrent)
			lifecycle := &recordingGuardLifecycle{transitionReceipt: candidateCurrent}
			_, err := TransitionRetainedGuard(context.Background(), lifecycle, GuardTransitionRequest{
				Current:          candidateCurrent,
				Release:          true,
				Abandonment:      &candidateAuthority,
				SourceDescriptor: descriptor,
				SourceFence:      fence,
			})
			if !errors.Is(err, ErrInvalidGuard) {
				t.Fatalf("TransitionRetainedGuard() error = %v, want ErrInvalidGuard", err)
			}
			if lifecycle.transitionCalls != 0 {
				t.Fatalf("TransitionRetainedGuard() called provider %d times for invalid authority", lifecycle.transitionCalls)
			}
		})
	}
}

func TestTransitionRetainedGuardMatchesItsExactComponentInMultiComponentDescriptor(t *testing.T) {
	source := testRetainedSource(PhysicalIdentity("retained-graph-component"))
	source.Provider = ProviderID("builtin-multi-component")
	source.Component = ComponentID("graph")
	source.Classes = classSet(t, coordclass.ClassGraph)
	install := GuardInstallRequest{
		Attempt:          AttemptID("attempt-multi-component-transition"),
		Generation:       Generation(4),
		Source:           source,
		Component:        source.Component,
		PhysicalIdentity: source.PhysicalIdentity,
		Role:             GuardRoleDenyWrite,
	}
	current := matchingGuardReceipt(install)
	descriptor := testDescriptor(t, source.Provider, source.PhysicalIdentity, coordclass.ClassGraph)
	descriptor.Components[0].ID = source.Component
	descriptor.Components = append(descriptor.Components, ComponentDescriptor{
		ID:               ComponentID("sessions"),
		Locator:          ComponentLocator("file:/city/retained-sessions"),
		PhysicalIdentity: PhysicalIdentity("retained-sessions-component"),
		Classes:          classSet(t, coordclass.ClassSessions),
		Format:           FormatID("builtin-format"),
		SchemaVersion:    "1",
		Marker:           MarkerState{Name: "marker", Present: true},
	})
	descriptor.Capabilities = capabilitiesFor(coordclass.ClassGraph, coordclass.ClassSessions)
	if err := descriptor.Validate(); err != nil {
		t.Fatalf("multi-component descriptor Validate(): %v", err)
	}
	lifecycle := &recordingGuardLifecycle{transitionReceipt: current}
	fence := fenceForDescriptor(t, descriptor, FenceRoleSource, install.Generation).(*recordingFence)
	authority := testPredecisionAbandonmentAuthority(t, current, descriptor, fence)

	got, err := TransitionRetainedGuard(context.Background(), lifecycle, GuardTransitionRequest{
		Current:          current,
		Release:          true,
		Abandonment:      &authority,
		SourceDescriptor: descriptor,
		SourceFence:      fence,
	})
	if err != nil {
		t.Fatalf("TransitionRetainedGuard(): %v", err)
	}
	if got.Component != source.Component || !got.Classes.Equal(source.Classes) || lifecycle.transitionCalls != 1 {
		t.Fatalf("transition receipt/calls = %#v/%d, want exact component receipt and one provider transition", got, lifecycle.transitionCalls)
	}
}

func TestTransitionRetainedGuardBindsNewReverseSagaDecisionAndActiveProof(t *testing.T) {
	source := testRetainedSource(PhysicalIdentity("retained-reverse-source"))
	install := GuardInstallRequest{
		Attempt:          AttemptID("forward-attempt"),
		Generation:       Generation(4),
		Source:           source,
		Component:        source.Component,
		PhysicalIdentity: source.PhysicalIdentity,
		Role:             GuardRoleDenyWrite,
	}
	current := matchingGuardReceipt(install)
	decision, err := NewCommitDecision(AttemptID("reverse-attempt"), Generation(5))
	if err != nil {
		t.Fatalf("NewCommitDecision(): %v", err)
	}
	activeProof := ParticipantReceipt{
		Version:            1,
		Kind:               ParticipantReceiptBindingActivation,
		Attempt:            decision.Attempt,
		Generation:         decision.Generation,
		Participant:        "binding",
		DescriptorIdentity: testBindingIdentity("reverse-active"),
		ReceiptID:          "reverse-active-receipt",
	}
	target := GuardTransferTarget{
		Decision:            decision,
		Participant:         activeProof.Participant,
		DestinationIdentity: activeProof.DescriptorIdentity,
		ExpectedReceiptKind: ParticipantReceiptBindingActivation,
		State:               GuardTransferActive,
		ActiveProof:         &activeProof,
	}
	expected := current
	expected.Attempt = decision.Attempt
	expected.Generation = decision.Generation
	expected.Role = GuardRoleTransfer
	expected.TransferState = GuardTransferActive
	expected.TransferParticipant = target.Participant
	expected.TransferDestinationIdentity = target.DestinationIdentity
	expected.TransferReceiptKind = target.ExpectedReceiptKind
	expected.ActiveProof = &activeProof
	lifecycle := &recordingGuardLifecycle{transitionReceipt: expected}

	got, err := TransitionRetainedGuard(context.Background(), lifecycle, GuardTransitionRequest{
		Current: current,
		Target:  &target,
	})
	if err != nil {
		t.Fatalf("TransitionRetainedGuard(): %v", err)
	}
	if got.Attempt != decision.Attempt || got.Generation != decision.Generation || got.TransferState != GuardTransferActive || got.ActiveProof == nil || !got.ActiveProof.Equal(activeProof) {
		t.Fatalf("transferred guard receipt = %#v, want bound reverse target", got)
	}
}

func TestTransitionRetainedGuardRejectsDriftedTransferReceipt(t *testing.T) {
	source := testRetainedSource(PhysicalIdentity("retained-transition"))
	install := GuardInstallRequest{
		Attempt:          AttemptID("attempt-transition"),
		Generation:       Generation(4),
		Source:           source,
		Component:        ComponentID("component"),
		PhysicalIdentity: source.PhysicalIdentity,
		Role:             GuardRoleDenyWrite,
	}
	current := matchingGuardReceipt(install)
	decision, err := NewCommitDecision(AttemptID("newer-transition"), Generation(5))
	if err != nil {
		t.Fatalf("NewCommitDecision(): %v", err)
	}
	target := GuardTransferTarget{
		Decision:            decision,
		Participant:         "newer-binding",
		DestinationIdentity: testBindingIdentity("newer-transition-destination"),
		ExpectedReceiptKind: ParticipantReceiptBindingActivation,
		State:               GuardTransferDecided,
	}
	expected := current
	expected.Attempt = decision.Attempt
	expected.Generation = decision.Generation
	expected.Role = GuardRoleTransfer
	expected.TransferState = GuardTransferDecided
	expected.TransferParticipant = target.Participant
	expected.TransferDestinationIdentity = target.DestinationIdentity
	expected.TransferReceiptKind = target.ExpectedReceiptKind

	validRequest := func() GuardTransitionRequest {
		return GuardTransitionRequest{
			Current: current,
			Target:  &target,
		}
	}

	t.Run("returned receipt identifies another component", func(t *testing.T) {
		drifted := expected
		drifted.PhysicalIdentity = PhysicalIdentity("other-physical-component")
		lifecycle := &recordingGuardLifecycle{transitionReceipt: drifted}
		_, err := TransitionRetainedGuard(context.Background(), lifecycle, validRequest())
		if !errors.Is(err, ErrInvalidGuard) {
			t.Fatalf("TransitionRetainedGuard() error = %v, want ErrInvalidGuard", err)
		}
	})

	t.Run("returned receipt changes class ownership", func(t *testing.T) {
		drifted := expected
		drifted.Classes = classSet(t, coordclass.ClassWork, coordclass.ClassGraph)
		lifecycle := &recordingGuardLifecycle{transitionReceipt: drifted}
		_, err := TransitionRetainedGuard(context.Background(), lifecycle, validRequest())
		if !errors.Is(err, ErrInvalidGuard) {
			t.Fatalf("TransitionRetainedGuard() error = %v, want ErrInvalidGuard", err)
		}
	})

	t.Run("returned receipt changes semantic contract", func(t *testing.T) {
		drifted := expected
		drifted.SemanticContractVersion = ContractVersion("storage-v2")
		lifecycle := &recordingGuardLifecycle{transitionReceipt: drifted}
		_, err := TransitionRetainedGuard(context.Background(), lifecycle, validRequest())
		if !errors.Is(err, ErrInvalidGuard) {
			t.Fatalf("TransitionRetainedGuard() error = %v, want ErrInvalidGuard", err)
		}
	})
}

func TestGuardTransferTargetRejectsSameDecisionProofSubstitution(t *testing.T) {
	source := testRetainedSource(PhysicalIdentity("retained-transfer-source"))
	current := matchingGuardReceipt(GuardInstallRequest{
		Attempt:          AttemptID("older-transfer"),
		Generation:       Generation(4),
		Source:           source,
		Component:        source.Component,
		PhysicalIdentity: source.PhysicalIdentity,
		Role:             GuardRoleDenyWrite,
	})
	decision, err := NewCommitDecision(AttemptID("newer-transfer"), Generation(5))
	if err != nil {
		t.Fatalf("NewCommitDecision(): %v", err)
	}
	proof := ParticipantReceipt{
		Version:            1,
		Kind:               ParticipantReceiptBindingActivation,
		Attempt:            decision.Attempt,
		Generation:         decision.Generation,
		Participant:        "target-binding",
		DescriptorIdentity: testBindingIdentity("target-descriptor"),
		ReceiptID:          "target-active-receipt",
	}
	base := GuardTransferTarget{
		Decision:            decision,
		Participant:         proof.Participant,
		DestinationIdentity: proof.DescriptorIdentity,
		ExpectedReceiptKind: proof.Kind,
		State:               GuardTransferActive,
		ActiveProof:         &proof,
	}
	if err := base.Validate(current); err != nil {
		t.Fatalf("base GuardTransferTarget.Validate(): %v", err)
	}

	for _, test := range []struct {
		name   string
		mutate func(*GuardTransferTarget)
	}{
		{
			name: "wrong participant",
			mutate: func(target *GuardTransferTarget) {
				target.ActiveProof.Participant = "other-binding"
			},
		},
		{
			name: "wrong descriptor",
			mutate: func(target *GuardTransferTarget) {
				target.ActiveProof.DescriptorIdentity = testBindingIdentity("other-descriptor")
			},
		},
		{
			name: "wrong expected kind",
			mutate: func(target *GuardTransferTarget) {
				target.ExpectedReceiptKind = ParticipantReceiptWorkMigration
			},
		},
		{
			name: "cross binding receipt",
			mutate: func(target *GuardTransferTarget) {
				target.ActiveProof.Participant = "other-binding"
				target.ActiveProof.DescriptorIdentity = testBindingIdentity("other-binding-descriptor")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := base
			proofCopy := proof.Clone()
			target.ActiveProof = &proofCopy
			test.mutate(&target)
			if err := target.Validate(current); !errors.Is(err, ErrInvalidGuard) {
				t.Fatalf("GuardTransferTarget.Validate() error = %v, want ErrInvalidGuard", err)
			}
		})
	}
}

func TestPredecisionAbandonmentAuthorityHasNoPublicMintSurface(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "provider_lifecycle.go", nil, 0)
	if err != nil {
		t.Fatalf("parse provider lifecycle API: %v", err)
	}
	for _, declaration := range file.Decls {
		switch declaration := declaration.(type) {
		case *ast.GenDecl:
			for _, specification := range declaration.Specs {
				typeSpec, ok := specification.(*ast.TypeSpec)
				if !ok || typeSpec.Name.Name != "PredecisionAbandonmentAuthority" {
					continue
				}
				structure, ok := typeSpec.Type.(*ast.StructType)
				if !ok {
					t.Fatal("PredecisionAbandonmentAuthority must remain an opaque struct")
				}
				for _, field := range structure.Fields.List {
					for _, name := range field.Names {
						if name.IsExported() {
							t.Fatalf("PredecisionAbandonmentAuthority exposes mintable field %q", name.Name)
						}
					}
				}
			}
		case *ast.FuncDecl:
			if !declaration.Name.IsExported() || declaration.Type.Results == nil {
				continue
			}
			for _, result := range declaration.Type.Results.List {
				identifier, ok := result.Type.(*ast.Ident)
				if ok && identifier.Name == "PredecisionAbandonmentAuthority" {
					t.Fatalf("exported function %q mints PredecisionAbandonmentAuthority", declaration.Name.Name)
				}
			}
		}
	}
}

func TestGuardReceiptRequiresSecretFreeRevalidationEvidence(t *testing.T) {
	request := GuardInstallRequest{
		Attempt:          AttemptID("guard-revalidation"),
		Generation:       Generation(3),
		Source:           testRetainedSource(PhysicalIdentity("guard-revalidation-source")),
		Component:        ComponentID("component"),
		PhysicalIdentity: PhysicalIdentity("guard-revalidation-source"),
		Role:             GuardRoleDenyWrite,
	}
	base := matchingGuardReceipt(request)
	for _, test := range []struct {
		name  string
		value string
		want  error
	}{
		{name: "missing", value: "", want: ErrInvalidGuard},
		{name: "whitespace", value: " \t", want: ErrInvalidGuard},
		{name: "secret", value: "token=guard-secret", want: ErrSecretMaterial}, // oss-leak-allow:fixture — negative-path input proving the validator rejects credential-shaped material.
	} {
		t.Run(test.name, func(t *testing.T) {
			receipt := base
			receipt.Revalidation = test.value
			if err := receipt.Validate(); !errors.Is(err, test.want) {
				t.Fatalf("GuardReceipt.Validate() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestGuardReceiptKeyDoesNotAliasEmbeddedNULFields(t *testing.T) {
	classes := classSet(t, coordclass.ClassWork)
	first := newGuardReceiptKey(
		ProviderID("builtin-key"),
		ComponentID("component"),
		PhysicalIdentity("left\x00work\x00right"),
		classes,
		ContractVersion("tail"),
		GuardRoleDenyWrite,
	)
	second := newGuardReceiptKey(
		ProviderID("builtin-key"),
		ComponentID("component"),
		PhysicalIdentity("left"),
		classes,
		ContractVersion("right\x00work\x00tail"),
		GuardRoleDenyWrite,
	)
	if first == second {
		t.Fatal("different receipt tuples aliased through embedded NUL fields")
	}
}

func TestRecoverBindingActivationFindsReceiptLostAfterDecision(t *testing.T) {
	descriptor := testDescriptor(t, ProviderID("builtin-activation"), PhysicalIdentity("activation-destination"), coordclass.ClassGraph)
	descriptor.Capabilities.GuardedActivation = true
	identity, err := descriptor.Identity()
	if err != nil {
		t.Fatalf("Descriptor.Identity(): %v", err)
	}
	decision, err := NewCommitDecision(AttemptID("attempt-activation"), Generation(8))
	if err != nil {
		t.Fatalf("NewCommitDecision(): %v", err)
	}
	install := GuardInstallRequest{
		Attempt:          decision.Attempt,
		Generation:       decision.Generation,
		Source:           testRetainedSource(PhysicalIdentity("activation-guard-source")),
		Component:        ComponentID("component"),
		PhysicalIdentity: PhysicalIdentity("activation-guard-source"),
		Role:             GuardRoleDenyWrite,
	}
	guardLifecycle := &recordingGuardLifecycle{}
	attestation, err := AttestGuardedActivation(context.Background(), guardLifecycle, decision, "binding", identity, fencedGuardPlan(t, install), []GuardReceipt{matchingGuardReceipt(install)})
	if err != nil {
		t.Fatalf("AttestGuardedActivation(): %v", err)
	}
	resume := BindingActivationResumeRequest{Decision: decision, Participant: "binding", Destination: descriptor, DesiredIdentity: identity, GuardAttestation: attestation, DestinationFence: fenceForDescriptor(t, descriptor, FenceRolePopulatedDestination, decision.Generation)}
	receipt := ParticipantReceipt{Version: 1, Kind: ParticipantReceiptBindingActivation, Attempt: decision.Attempt, Generation: decision.Generation, Participant: "binding", DescriptorIdentity: identity, ReceiptID: "activation-receipt"}
	lifecycle := &recordingBindingMigration{resumeReceipt: receipt}

	got, err := RecoverBindingActivation(context.Background(), lifecycle, resume)
	if err != nil {
		t.Fatalf("RecoverBindingActivation(): %v", err)
	}
	if got.ReceiptID != receipt.ReceiptID || lifecycle.activateCalls != 0 || lifecycle.resumeCalls != 1 {
		t.Fatalf("activation recovery = %#v, calls activate:%d resume:%d", got, lifecycle.activateCalls, lifecycle.resumeCalls)
	}
}

func TestBindingActivationRejectsUnrelatedOrDuplicateGuardReceipts(t *testing.T) {
	descriptor := testDescriptor(t, ProviderID("builtin-activation"), PhysicalIdentity("activation-destination"), coordclass.ClassGraph)
	descriptor.Capabilities.GuardedActivation = true
	identity, err := descriptor.Identity()
	if err != nil {
		t.Fatalf("Descriptor.Identity(): %v", err)
	}
	decision, err := NewCommitDecision(AttemptID("attempt-activation-guards"), Generation(9))
	if err != nil {
		t.Fatalf("NewCommitDecision(): %v", err)
	}
	install := GuardInstallRequest{
		Attempt:          decision.Attempt,
		Generation:       decision.Generation,
		Source:           testRetainedSource(PhysicalIdentity("retained-guard-source")),
		Component:        ComponentID("component"),
		PhysicalIdentity: PhysicalIdentity("retained-guard-source"),
		Role:             GuardRoleDenyWrite,
	}
	receipt := matchingGuardReceipt(install)
	lifecycle := &recordingGuardLifecycle{}
	attestation, err := AttestGuardedActivation(context.Background(), lifecycle, decision, "binding", identity, fencedGuardPlan(t, install), []GuardReceipt{receipt})
	if err != nil {
		t.Fatalf("AttestGuardedActivation(): %v", err)
	}
	if lifecycle.verifyCalls != 1 {
		t.Fatalf("AttestGuardedActivation() verify calls = %d, want 1", lifecycle.verifyCalls)
	}
	base := BindingActivationRequest{
		Decision:         decision,
		Participant:      "binding",
		Destination:      descriptor,
		DesiredIdentity:  identity,
		GuardAttestation: attestation,
		DestinationFence: fenceForDescriptor(t, descriptor, FenceRolePopulatedDestination, decision.Generation),
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("base BindingActivationRequest.Validate(): %v", err)
	}

	missing := base
	missing.GuardAttestation = GuardedActivationAttestation{}
	if err := missing.Validate(); !errors.Is(err, ErrInvalidGuard) {
		t.Fatalf("missing guard attestation error = %v, want ErrInvalidGuard", err)
	}

	wrongParticipant := base
	wrongParticipant.Participant = "other-binding"
	if err := wrongParticipant.Validate(); !errors.Is(err, ErrInvalidGuard) {
		t.Fatalf("cross-participant attestation error = %v, want ErrInvalidGuard", err)
	}

	wrongDecision := base
	wrongDecision.Decision, err = NewCommitDecision(AttemptID("other-attempt"), decision.Generation+1)
	if err != nil {
		t.Fatalf("NewCommitDecision(): %v", err)
	}
	if err := wrongDecision.Validate(); !errors.Is(err, ErrInvalidGuard) {
		t.Fatalf("cross-attempt attestation error = %v, want ErrInvalidGuard", err)
	}

	unguardedDestination := base
	unguardedDestination.Destination = descriptor.Clone()
	unguardedDestination.Destination.Capabilities.GuardedActivation = false
	if err := unguardedDestination.Validate(); !errors.Is(err, ErrGuardedActivationUnavailable) {
		t.Fatalf("unguarded destination error = %v, want ErrGuardedActivationUnavailable", err)
	}

	wrongReceiptParticipant := ParticipantReceipt{
		Version:            1,
		Kind:               ParticipantReceiptBindingActivation,
		Attempt:            decision.Attempt,
		Generation:         decision.Generation,
		Participant:        "other-binding",
		DescriptorIdentity: identity,
		ReceiptID:          "activation-receipt",
	}
	if _, err := ActivateBinding(context.Background(), &recordingBindingMigration{activateReceipt: wrongReceiptParticipant}, base); err == nil {
		t.Fatal("ActivateBinding() succeeded with a cross-participant activation receipt")
	}

	duplicate := append([]GuardReceipt{receipt}, receipt)
	if _, err := AttestGuardedActivation(context.Background(), lifecycle, decision, "binding", identity, fencedGuardPlan(t, install), duplicate); !errors.Is(err, ErrInvalidGuard) {
		t.Fatalf("duplicate guard receipts error = %v, want ErrInvalidGuard", err)
	}

	unrelated := receipt
	unrelated.Provider = ProviderID("other-provider")
	if _, err := AttestGuardedActivation(context.Background(), lifecycle, decision, "binding", identity, fencedGuardPlan(t, install), []GuardReceipt{unrelated}); !errors.Is(err, ErrInvalidGuard) {
		t.Fatalf("unrelated guard receipt error = %v, want ErrInvalidGuard", err)
	}

	wrongAttempt := receipt
	wrongAttempt.Attempt = AttemptID("other-attempt")
	if _, err := AttestGuardedActivation(context.Background(), lifecycle, decision, "binding", identity, fencedGuardPlan(t, install), []GuardReceipt{wrongAttempt}); !errors.Is(err, ErrInvalidGuard) {
		t.Fatalf("wrong-attempt guard receipt error = %v, want ErrInvalidGuard", err)
	}
}

func TestAttestGuardedActivationRejectsGuardVerificationFailure(t *testing.T) {
	descriptor := testDescriptor(t, ProviderID("builtin-activation"), PhysicalIdentity("activation-destination"), coordclass.ClassGraph)
	descriptor.Capabilities.GuardedActivation = true
	identity, err := descriptor.Identity()
	if err != nil {
		t.Fatalf("Descriptor.Identity(): %v", err)
	}
	decision, err := NewCommitDecision(AttemptID("attempt-activation-verify"), Generation(10))
	if err != nil {
		t.Fatalf("NewCommitDecision(): %v", err)
	}
	install := GuardInstallRequest{
		Attempt:          decision.Attempt,
		Generation:       decision.Generation,
		Source:           testRetainedSource(PhysicalIdentity("activation-guard-source")),
		Component:        ComponentID("component"),
		PhysicalIdentity: PhysicalIdentity("activation-guard-source"),
		Role:             GuardRoleDenyWrite,
	}
	verifyErr := errors.New("guard verification failed")
	attestation, err := AttestGuardedActivation(context.Background(), &recordingGuardLifecycle{verifyErr: verifyErr}, decision, "binding", identity, fencedGuardPlan(t, install), []GuardReceipt{matchingGuardReceipt(install)})
	if !errors.Is(err, verifyErr) {
		t.Fatalf("AttestGuardedActivation() error = %v, want guard verification failure", err)
	}
	if len(attestation.receiptIDs) != 0 {
		t.Fatalf("AttestGuardedActivation() returned receipt IDs %#v after verification failure", attestation.receiptIDs)
	}
}

func TestAttestGuardedActivationUsesExactProviderLifecycleAfterGlobalCompletenessChecks(t *testing.T) {
	decision, err := NewCommitDecision(AttemptID("mixed-provider-attestation"), Generation(11))
	if err != nil {
		t.Fatalf("NewCommitDecision(): %v", err)
	}
	newInstall := func(provider ProviderID, identity PhysicalIdentity) GuardInstallRequest {
		source := testRetainedSource(identity)
		source.Provider = provider
		return GuardInstallRequest{
			Attempt:          decision.Attempt,
			Generation:       decision.Generation,
			Source:           source,
			Component:        source.Component,
			PhysicalIdentity: identity,
			Role:             GuardRoleDenyWrite,
		}
	}
	first := newInstall(ProviderID("attest-provider-one"), PhysicalIdentity("attest-source-one"))
	second := newInstall(ProviderID("attest-provider-two"), PhysicalIdentity("attest-source-two"))
	plan := fencedGuardPlan(t, first, second)
	receipts := []GuardReceipt{matchingGuardReceipt(first), matchingGuardReceipt(second)}
	receipts[0].ReceiptID = "attest-receipt-one"
	receipts[1].ReceiptID = "attest-receipt-two"
	destinationIdentity := testBindingIdentity("mixed-provider-attestation-destination")

	t.Run("each provider verifies only its subgroup", func(t *testing.T) {
		firstLifecycle := &recordingGuardLifecycle{provider: first.Source.Provider}
		secondLifecycle := &recordingGuardLifecycle{provider: second.Source.Provider}
		resolver := &recordingGuardLifecycleResolver{lifecycles: map[ProviderID]RetainedGuardLifecycle{
			first.Source.Provider:  firstLifecycle,
			second.Source.Provider: secondLifecycle,
		}}

		attestation, err := AttestGuardedActivation(context.Background(), resolver, decision, "binding", destinationIdentity, plan, receipts)
		if err != nil {
			t.Fatalf("AttestGuardedActivation(): %v", err)
		}
		if firstLifecycle.verifyCalls != 1 || secondLifecycle.verifyCalls != 1 || firstLifecycle.wrongProviderCalls != 0 || secondLifecycle.wrongProviderCalls != 0 {
			t.Fatalf("mixed-provider verification calls = first:%d/%d second:%d/%d, want exact 1/0 for each", firstLifecycle.verifyCalls, firstLifecycle.wrongProviderCalls, secondLifecycle.verifyCalls, secondLifecycle.wrongProviderCalls)
		}
		if len(attestation.receiptIDs) != 2 || attestation.receiptIDs[0] != "attest-receipt-one" || attestation.receiptIDs[1] != "attest-receipt-two" {
			t.Fatalf("attestation receipt IDs = %#v, want aggregate sorted receipt set", attestation.receiptIDs)
		}
	})

	t.Run("missing receipt fails before lifecycle resolution", func(t *testing.T) {
		resolver := &recordingGuardLifecycleResolver{lifecycles: map[ProviderID]RetainedGuardLifecycle{}}
		_, err := AttestGuardedActivation(context.Background(), resolver, decision, "binding", destinationIdentity, plan, receipts[:1])
		if !errors.Is(err, ErrInvalidGuard) {
			t.Fatalf("AttestGuardedActivation() error = %v, want ErrInvalidGuard", err)
		}
		if len(resolver.resolveCalls) != 0 {
			t.Fatalf("incomplete receipt set resolved providers %#v", resolver.resolveCalls)
		}
	})

	t.Run("missing later lifecycle fails before any verification", func(t *testing.T) {
		firstLifecycle := &recordingGuardLifecycle{provider: first.Source.Provider}
		resolver := &recordingGuardLifecycleResolver{lifecycles: map[ProviderID]RetainedGuardLifecycle{
			first.Source.Provider: firstLifecycle,
		}}
		_, err := AttestGuardedActivation(context.Background(), resolver, decision, "binding", destinationIdentity, plan, receipts)
		if !errors.Is(err, ErrMissingCapability) {
			t.Fatalf("AttestGuardedActivation() error = %v, want ErrMissingCapability", err)
		}
		if firstLifecycle.verifyCalls != 0 {
			t.Fatalf("unresolved attestation verified first provider %d times", firstLifecycle.verifyCalls)
		}
	})
}

func TestInPlaceGuardedActivationAuthorityAllowsOnlyEquivalentNoCopyDescriptors(t *testing.T) {
	destination := testDescriptor(t, ProviderID("builtin-in-place-authority"), PhysicalIdentity("in-place-authority"), coordclass.ClassGraph)
	decision, err := NewCommitDecision(AttemptID("in-place-authority"), Generation(16))
	if err != nil {
		t.Fatalf("NewCommitDecision(): %v", err)
	}

	unmarked := destination.Clone()
	unmarked.Components[0].Marker.Present = false
	formatDrift := destination.Clone()
	formatDrift.Components[0].Format = FormatID("other-format")
	schemaDrift := destination.Clone()
	schemaDrift.Components[0].SchemaVersion = "2"
	abiDrift := destination.Clone()
	abiDrift.Components[0].ABIVersion = "2"
	componentDrift := destination.Clone()
	componentDrift.Components[0].ID = ComponentID("other-component")

	for _, test := range []struct {
		name   string
		source Descriptor
		want   error
	}{
		{name: "marked to marked", source: destination},
		{name: "unmarked to marked", source: unmarked},
		{name: "format drift", source: formatDrift, want: ErrInvalidGuard},
		{name: "schema drift", source: schemaDrift, want: ErrInvalidGuard},
		{name: "ABI drift", source: abiDrift, want: ErrInvalidGuard},
		{name: "component drift", source: componentDrift, want: ErrInvalidGuard},
	} {
		t.Run(test.name, func(t *testing.T) {
			fence := fenceForDescriptor(t, destination, FenceRolePopulatedDestination, decision.Generation)
			_, err := NewInPlaceGuardedActivationAuthority(context.Background(), decision, "binding", test.source, destination, fence)
			if !errors.Is(err, test.want) {
				t.Fatalf("NewInPlaceGuardedActivationAuthority() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestAttestGuardedActivationPermitsOnlyAnExactEmptyGuardPlan(t *testing.T) {
	descriptor := testDescriptor(t, ProviderID("builtin-empty-guard-activation"), PhysicalIdentity("empty-guard-destination"), coordclass.ClassGraph)
	descriptor.Capabilities.GuardedActivation = true
	identity, err := descriptor.Identity()
	if err != nil {
		t.Fatalf("Descriptor.Identity(): %v", err)
	}
	decision, err := NewCommitDecision(AttemptID("empty-guard-activation"), Generation(16))
	if err != nil {
		t.Fatalf("NewCommitDecision(): %v", err)
	}

	if _, err := AttestGuardedActivation(context.Background(), nil, decision, "binding", identity, nil, nil); !errors.Is(err, ErrInvalidGuard) {
		t.Fatalf("AttestGuardedActivation(empty without adoption authority) error = %v, want ErrInvalidGuard", err)
	}
	fence := fenceForDescriptor(t, descriptor, FenceRolePopulatedDestination, decision.Generation)
	authority, err := NewInPlaceGuardedActivationAuthority(context.Background(), decision, "binding", descriptor, descriptor, fence)
	if err != nil {
		t.Fatalf("NewInPlaceGuardedActivationAuthority(): %v", err)
	}
	attestation, err := AttestGuardedActivation(context.Background(), nil, decision, "binding", identity, nil, nil, authority)
	if err != nil {
		t.Fatalf("AttestGuardedActivation(empty): %v", err)
	}
	request := BindingActivationRequest{
		Decision:         decision,
		Participant:      "binding",
		Destination:      descriptor,
		DesiredIdentity:  identity,
		GuardAttestation: attestation,
		DestinationFence: fence,
	}
	receipt := ParticipantReceipt{Version: 1, Kind: ParticipantReceiptBindingActivation, Attempt: decision.Attempt, Generation: decision.Generation, Participant: request.Participant, DescriptorIdentity: identity, ReceiptID: "empty-guard-activation-receipt"}
	if _, err := ActivateBinding(context.Background(), &recordingBindingMigration{activateReceipt: receipt}, request); err != nil {
		t.Fatalf("ActivateBinding(empty guarded plan): %v", err)
	}

	install := GuardInstallRequest{
		Attempt:          decision.Attempt,
		Generation:       decision.Generation,
		Source:           testRetainedSource(PhysicalIdentity("empty-guard-source")),
		Component:        ComponentID("component"),
		PhysicalIdentity: PhysicalIdentity("empty-guard-source"),
		Role:             GuardRoleDenyWrite,
	}
	for _, test := range []struct {
		name     string
		plan     []FencedGuardInstallRequest
		receipts []GuardReceipt
	}{
		{name: "plan without receipts", plan: fencedGuardPlan(t, install)},
		{name: "receipt without plan", receipts: []GuardReceipt{matchingGuardReceipt(install)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := AttestGuardedActivation(context.Background(), nil, decision, "binding", identity, test.plan, test.receipts)
			if !errors.Is(err, ErrInvalidGuard) {
				t.Fatalf("AttestGuardedActivation() error = %v, want ErrInvalidGuard", err)
			}
		})
	}
}

func TestOpenedBindingClosesEveryPhysicalHandleExactlyOnce(t *testing.T) {
	descriptor := testDescriptor(t, ProviderID("builtin-close"), PhysicalIdentity("close-identity"), coordclass.ClassGraph)
	adapters := testBeadsAdapters(t)
	var closeCalls atomic.Int32
	opened, err := NewOpenedBinding(OpenedBindingParts{
		Descriptor:   descriptor,
		Capabilities: descriptor.Capabilities,
		Graph:        adapters.Graph,
		Handles: []ComponentHandle{{
			Component: ComponentID("component"),
			Close: func() error {
				closeCalls.Add(1)
				return nil
			},
		}},
	})
	if err != nil {
		t.Fatalf("NewOpenedBinding(): %v", err)
	}

	var wait sync.WaitGroup
	for range 32 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := opened.Close(); err != nil {
				t.Errorf("Close(): %v", err)
			}
		}()
	}
	wait.Wait()
	if got := closeCalls.Load(); got != 1 {
		t.Fatalf("physical handle close calls = %d, want 1", got)
	}
}

func TestOpenedBindingCloseRetriesOnlyFailedHandlesInReverseAcquisitionOrder(t *testing.T) {
	adapters := testBeadsAdapters(t)
	descriptor := testDescriptor(t, ProviderID("builtin-close-retry"), PhysicalIdentity("close-retry-graph"), coordclass.ClassGraph)
	descriptor.Components = append(descriptor.Components, ComponentDescriptor{
		ID:               ComponentID("sessions"),
		Locator:          ComponentLocator("file:/city/close-retry-sessions"),
		PhysicalIdentity: PhysicalIdentity("close-retry-sessions"),
		Classes:          classSet(t, coordclass.ClassSessions),
		Format:           FormatID("builtin-format"),
		SchemaVersion:    "1",
		Marker:           MarkerState{Name: "marker", Present: true},
	})
	descriptor.Capabilities = capabilitiesFor(coordclass.ClassGraph, coordclass.ClassSessions)
	if err := descriptor.Validate(); err != nil {
		t.Fatalf("Descriptor.Validate(): %v", err)
	}
	closeErr := errors.New("second handle close failed")
	var order []string
	secondAttempts := 0
	opened, err := NewOpenedBinding(OpenedBindingParts{
		Descriptor:   descriptor,
		Capabilities: descriptor.Capabilities,
		Graph:        adapters.Graph,
		Sessions:     adapters.Sessions,
		Handles: []ComponentHandle{
			{Component: ComponentID("component"), Close: func() error { order = append(order, "first"); return nil }},
			{Component: ComponentID("sessions"), Close: func() error {
				order = append(order, "second")
				secondAttempts++
				if secondAttempts == 1 {
					return closeErr
				}
				return nil
			}},
		},
	})
	if err != nil {
		t.Fatalf("NewOpenedBinding(): %v", err)
	}
	if err := opened.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("first Close() error = %v, want close error", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("retry Close(): %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("idempotent Close(): %v", err)
	}
	if got, want := fmt.Sprint(order), "[second first second]"; got != want {
		t.Fatalf("close order = %s, want %s", got, want)
	}
}

func TestResolvedOpenedBindingCloseRetriesFailedSourceClose(t *testing.T) {
	descriptor := testDescriptor(t, ProviderID("builtin-resolved-close-retry"), PhysicalIdentity("resolved-close-retry"), coordclass.ClassGraph)
	closeErr := errors.New("source close failed")
	source := &directOpenedBinding{
		descriptor:   descriptor,
		capabilities: descriptor.Capabilities,
		graph:        testBeadsAdapters(t).Graph,
		graphOK:      true,
		closeErr:     closeErr,
	}
	opened, err := OpenBinding(context.Background(), &recordingProvider{opened: source}, completeOpenRequest(t, descriptor, descriptor.Classes()))
	if err != nil {
		t.Fatalf("OpenBinding(): %v", err)
	}
	if err := opened.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("first Close() error = %v, want source close error", err)
	}
	source.closeErr = nil
	if err := opened.Close(); err != nil {
		t.Fatalf("retry Close(): %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("idempotent Close(): %v", err)
	}
	if source.closeCalls != 2 {
		t.Fatalf("source Close() calls = %d, want failed then successful retries only", source.closeCalls)
	}
}

func TestNewOpenedBindingRejectedConstructorRetainsRetryCleanup(t *testing.T) {
	closeErr := errors.New("rejected handle close failed")
	attempts := 0
	descriptor := testDescriptor(t, ProviderID("builtin-rejected-retry"), PhysicalIdentity("rejected-retry"), coordclass.ClassGraph)
	descriptor.Version = 0
	_, err := NewOpenedBinding(OpenedBindingParts{
		Descriptor: descriptor,
		Handles: []ComponentHandle{{
			Component: ComponentID("component"),
			Close: func() error {
				attempts++
				if attempts == 1 {
					return closeErr
				}
				return nil
			},
		}},
	})
	if !errors.Is(err, closeErr) {
		t.Fatalf("NewOpenedBinding() error = %v, want close error", err)
	}
	var pending *RejectedOpenedBindingCleanupError
	if !errors.As(err, &pending) {
		t.Fatalf("NewOpenedBinding() error = %T, want *RejectedOpenedBindingCleanupError", err)
	}
	if err := pending.RetryCleanup(); err != nil {
		t.Fatalf("RetryCleanup(): %v", err)
	}
	if err := pending.RetryCleanup(); err != nil {
		t.Fatalf("idempotent RetryCleanup(): %v", err)
	}
	if attempts != 2 {
		t.Fatalf("rejected handle close attempts = %d, want failed then successful retry", attempts)
	}
}

func TestNewOpenedBindingRejectsPartsByUnwindingEverySuppliedHandleInReverse(t *testing.T) {
	adapters := testBeadsAdapters(t)
	one := testDescriptor(t, ProviderID("builtin-rejection-close"), PhysicalIdentity("rejection-close-identity"), coordclass.ClassGraph)
	two := one.Clone()
	two.Components = append(two.Components, ComponentDescriptor{
		ID:               ComponentID("sessions"),
		Locator:          ComponentLocator("file:/city/rejection-sessions"),
		PhysicalIdentity: PhysicalIdentity("rejection-sessions-identity"),
		Classes:          classSet(t, coordclass.ClassSessions),
		Format:           FormatID("builtin-format"),
		SchemaVersion:    "1",
		Marker:           MarkerState{Name: "marker", Present: true},
	})
	two.Capabilities = capabilitiesFor(coordclass.ClassGraph, coordclass.ClassSessions)
	if err := two.Validate(); err != nil {
		t.Fatalf("two-component Descriptor.Validate(): %v", err)
	}

	for _, test := range []struct {
		name  string
		parts func([]ComponentHandle) OpenedBindingParts
		names []string
		want  []string
	}{
		{
			name: "descriptor validation",
			parts: func(handles []ComponentHandle) OpenedBindingParts {
				invalid := one.Clone()
				invalid.Version = 0
				return OpenedBindingParts{Descriptor: invalid, Capabilities: one.Capabilities, Graph: adapters.Graph, Handles: handles}
			},
			names: []string{"first", "second"},
			want:  []string{"second", "first"},
		},
		{
			name: "typed nil front",
			parts: func(handles []ComponentHandle) OpenedBindingParts {
				var graph *beadsGraphAdapter
				return OpenedBindingParts{Descriptor: one, Capabilities: one.Capabilities, Graph: graph, Handles: handles}
			},
			names: []string{"first"},
			want:  []string{"first"},
		},
		{
			name: "handle cardinality",
			parts: func(handles []ComponentHandle) OpenedBindingParts {
				return OpenedBindingParts{Descriptor: one, Capabilities: one.Capabilities, Graph: adapters.Graph, Handles: handles}
			},
			names: []string{"first", "second"},
			want:  []string{"second", "first"},
		},
		{
			name: "invalid handle closure",
			parts: func(handles []ComponentHandle) OpenedBindingParts {
				handles = append(handles, ComponentHandle{Component: ComponentID("component")})
				return OpenedBindingParts{Descriptor: one, Capabilities: one.Capabilities, Graph: adapters.Graph, Handles: handles}
			},
			names: []string{"first"},
			want:  []string{"first"},
		},
		{
			name: "duplicate component handle",
			parts: func(handles []ComponentHandle) OpenedBindingParts {
				handles[0].Component = ComponentID("component")
				handles[1].Component = ComponentID("component")
				return OpenedBindingParts{Descriptor: two, Capabilities: two.Capabilities, Graph: adapters.Graph, Sessions: adapters.Sessions, Handles: handles}
			},
			names: []string{"first", "second"},
			want:  []string{"second", "first"},
		},
		{
			name: "work topology clone",
			parts: func(handles []ComponentHandle) OpenedBindingParts {
				workDescriptor := testDescriptor(t, ProviderID("builtin-rejection-work"), PhysicalIdentity("rejection-work-identity"), coordclass.ClassWork)
				return OpenedBindingParts{Descriptor: workDescriptor, Capabilities: workDescriptor.Capabilities, Work: &WorkTopology{}, Handles: handles}
			},
			names: []string{"first"},
			want:  []string{"first"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var order []string
			handles := make([]ComponentHandle, len(test.names))
			for index, name := range test.names {
				handles[index] = ComponentHandle{
					Component: ComponentID("component"),
					Close: func() error {
						order = append(order, name)
						return nil
					},
				}
			}
			if _, err := NewOpenedBinding(test.parts(handles)); err == nil {
				t.Fatal("NewOpenedBinding() succeeded for rejected parts")
			}
			if len(order) != len(test.want) {
				t.Fatalf("close order = %#v, want %#v", order, test.want)
			}
			for index := range test.want {
				if order[index] != test.want[index] {
					t.Fatalf("close order = %#v, want %#v", order, test.want)
				}
			}
		})
	}

	closeErr := errors.New("component close failed")
	invalid := one.Clone()
	invalid.Version = 0
	_, err := NewOpenedBinding(OpenedBindingParts{
		Descriptor: invalid,
		Handles:    []ComponentHandle{{Component: ComponentID("component"), Close: func() error { return closeErr }}},
	})
	if !errors.Is(err, closeErr) {
		t.Fatalf("NewOpenedBinding() error = %v, want joined close error", err)
	}
}

func TestNewOpenedBindingChecksHandleCardinalityBeforePerHandleMembership(t *testing.T) {
	descriptor := testDescriptor(t, ProviderID("builtin-handle-cardinality"), PhysicalIdentity("handle-cardinality"), coordclass.ClassGraph)
	adapters := testBeadsAdapters(t)
	var closeOrder []string
	closeHandle := func(name string) func() error {
		return func() error {
			closeOrder = append(closeOrder, name)
			return nil
		}
	}

	_, err := NewOpenedBinding(OpenedBindingParts{
		Descriptor:   descriptor,
		Capabilities: descriptor.Capabilities,
		Graph:        adapters.Graph,
		Handles: []ComponentHandle{
			{Component: ComponentID("component"), Close: closeHandle("matching")},
			{Component: ComponentID("unexpected"), Close: closeHandle("extra")},
		},
	})
	if err == nil {
		t.Fatal("NewOpenedBinding() succeeded with an extra handle")
	}
	if got, want := err.Error(), "opened binding has 2 handles for 1 descriptor components"; got != want {
		t.Fatalf("NewOpenedBinding() error = %q, want cardinality error %q", got, want)
	}
	if got, want := fmt.Sprint(closeOrder), "[extra matching]"; got != want {
		t.Fatalf("rejected handle close order = %s, want %s", got, want)
	}
}

func TestNewOpenedBindingRejectsMissingExtraAndMismatchedTypedFronts(t *testing.T) {
	descriptor := testDescriptor(t, ProviderID("builtin-opened"), PhysicalIdentity("opened-identity"), coordclass.ClassGraph)
	adapters := testBeadsAdapters(t)
	base := OpenedBindingParts{
		Descriptor:   descriptor,
		Capabilities: descriptor.Capabilities,
		Graph:        adapters.Graph,
		Handles: []ComponentHandle{{
			Component: ComponentID("component"),
			Close:     func() error { return nil },
		}},
	}

	tests := []struct {
		name   string
		mutate func(*OpenedBindingParts)
	}{
		{
			name: "missing advertised graph front",
			mutate: func(parts *OpenedBindingParts) {
				parts.Graph = nil
			},
		},
		{
			name: "extra unadvertised sessions front",
			mutate: func(parts *OpenedBindingParts) {
				parts.Sessions = adapters.Sessions
			},
		},
		{
			name: "capability differs from descriptor",
			mutate: func(parts *OpenedBindingParts) {
				parts.Capabilities.Graph.Transactions = false
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parts := base
			test.mutate(&parts)
			_, err := NewOpenedBinding(parts)
			if !errors.Is(err, ErrInvalidDescriptor) {
				t.Fatalf("NewOpenedBinding() error = %v, want ErrInvalidDescriptor", err)
			}
		})
	}
}

func TestOpenBindingRejectsMissingExtraAndMismatchedReturnedFronts(t *testing.T) {
	descriptor := testDescriptor(t, ProviderID("builtin-opened"), PhysicalIdentity("opened-result-identity"), coordclass.ClassGraph)
	adapters := testBeadsAdapters(t)
	request := completeOpenRequest(t, descriptor, classSet(t, coordclass.ClassGraph))

	tests := []struct {
		name   string
		opened *directOpenedBinding
	}{
		{
			name:   "missing advertised graph front",
			opened: &directOpenedBinding{descriptor: descriptor, capabilities: descriptor.Capabilities},
		},
		{
			name: "extra unadvertised sessions front",
			opened: &directOpenedBinding{
				descriptor:   descriptor,
				capabilities: descriptor.Capabilities,
				graph:        adapters.Graph,
				graphOK:      true,
				sessions:     adapters.Sessions,
				sessionsOK:   true,
			},
		},
		{
			name: "capability differs from descriptor",
			opened: &directOpenedBinding{
				descriptor: descriptor,
				capabilities: ClassCapabilities{
					Graph: ClassCapability{Available: true, Claims: true},
				},
				graph:   adapters.Graph,
				graphOK: true,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := &recordingProvider{opened: test.opened}
			_, err := OpenBinding(context.Background(), provider, request)
			if !errors.Is(err, ErrInvalidDescriptor) {
				t.Fatalf("OpenBinding() error = %v, want ErrInvalidDescriptor", err)
			}
			if test.opened.closeCalls != 1 {
				t.Fatalf("OpenBinding() close calls = %d, want 1 after rejecting provider output", test.opened.closeCalls)
			}
		})
	}
}

func TestOpenBindingRejectsZeroAndPartialAggregateFrontDoors(t *testing.T) {
	adapters := testBeadsAdapters(t)
	tests := []struct {
		name       string
		descriptor Descriptor
		opened     *directOpenedBinding
		wantErr    error
	}{
		{
			name:       "zero messaging front",
			descriptor: testDescriptor(t, ProviderID("builtin-messaging"), PhysicalIdentity("messaging-identity"), coordclass.ClassMessaging),
			opened: &directOpenedBinding{
				descriptor:   testDescriptor(t, ProviderID("builtin-messaging"), PhysicalIdentity("messaging-identity"), coordclass.ClassMessaging),
				capabilities: capabilitiesFor(coordclass.ClassMessaging),
				messagingOK:  true,
			},
			wantErr: ErrInvalidMessagingBinding,
		},
		{
			name:       "zero nudges front",
			descriptor: testDescriptor(t, ProviderID("builtin-nudges"), PhysicalIdentity("nudges-identity"), coordclass.ClassNudges),
			opened: &directOpenedBinding{
				descriptor:   testDescriptor(t, ProviderID("builtin-nudges"), PhysicalIdentity("nudges-identity"), coordclass.ClassNudges),
				capabilities: capabilitiesFor(coordclass.ClassNudges),
				nudgesOK:     true,
			},
			wantErr: ErrInvalidDescriptor,
		},
		{
			name:       "partial nudges front",
			descriptor: testDescriptor(t, ProviderID("builtin-nudges"), PhysicalIdentity("nudges-partial-identity"), coordclass.ClassNudges),
			opened: &directOpenedBinding{
				descriptor:   testDescriptor(t, ProviderID("builtin-nudges"), PhysicalIdentity("nudges-partial-identity"), coordclass.ClassNudges),
				capabilities: capabilitiesFor(coordclass.ClassNudges),
				nudges:       NudgeFrontDoors{Queue: nudgeQueueFake{}},
				nudgesOK:     true,
			},
			wantErr: ErrInvalidDescriptor,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := completeOpenRequest(t, test.descriptor, test.descriptor.Classes())
			_, err := OpenBinding(context.Background(), &recordingProvider{opened: test.opened}, request)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("OpenBinding() error = %v, want %v", err, test.wantErr)
			}
			if test.opened.closeCalls != 1 {
				t.Fatalf("OpenBinding() close calls = %d, want 1", test.opened.closeCalls)
			}
		})
	}

	t.Run("partial messaging front is rejected at deferred bind", func(t *testing.T) {
		descriptor := testDescriptor(t, ProviderID("builtin-messaging"), PhysicalIdentity("messaging-partial-identity"), coordclass.ClassMessaging)
		direct := &directOpenedBinding{
			descriptor:   descriptor,
			capabilities: capabilitiesFor(coordclass.ClassMessaging),
			messaging: &recordingMessagingBinder{
				bind: func(SessionsAddressDirectory) (MessagingFrontDoors, error) {
					return MessagingFrontDoors{Mail: adapters.Messaging.Mail}, nil
				},
			},
			messagingOK: true,
		}
		request := completeOpenRequest(t, descriptor, descriptor.Classes())
		opened, err := OpenBinding(context.Background(), &recordingProvider{opened: direct}, request)
		if err != nil {
			t.Fatalf("OpenBinding() error = %v", err)
		}
		binder, ok := opened.Messaging()
		if !ok {
			t.Fatal("Messaging() did not return deferred binder")
		}
		if _, err := binder.BindSessions(adapters.Sessions); !errors.Is(err, ErrInvalidMessagingBinding) {
			t.Fatalf("BindSessions() error = %v, want ErrInvalidMessagingBinding", err)
		}
		if err := opened.Close(); err != nil {
			t.Fatalf("Close(): %v", err)
		}
		if direct.closeCalls != 1 {
			t.Fatalf("Close() calls = %d, want 1", direct.closeCalls)
		}
	})
}

func TestOpenBindingJoinsRejectedBindingCloseError(t *testing.T) {
	descriptor := testDescriptor(t, ProviderID("builtin-close-error"), PhysicalIdentity("close-error-identity"), coordclass.ClassGraph)
	actual := descriptor.Clone()
	actual.ImplementationVersion = "other-implementation"
	closeErr := errors.New("close rejected binding")
	direct := &directOpenedBinding{
		descriptor:   actual,
		capabilities: actual.Capabilities,
		graph:        testBeadsAdapters(t).Graph,
		graphOK:      true,
		closeErr:     closeErr,
	}

	_, err := OpenBinding(context.Background(), &recordingProvider{opened: direct}, completeOpenRequest(t, descriptor, descriptor.Classes()))
	if !errors.Is(err, ErrFenceTargetMoved) {
		t.Fatalf("OpenBinding() error = %v, want ErrFenceTargetMoved", err)
	}
	if !errors.Is(err, closeErr) {
		t.Fatalf("OpenBinding() error = %v, want rejected binding close error", err)
	}
	if direct.closeCalls != 1 {
		t.Fatalf("rejected provider binding close calls = %d, want 1", direct.closeCalls)
	}
}

func TestOpenBindingClosesReturnedBindingWhenProviderAlsoReturnsError(t *testing.T) {
	descriptor := testDescriptor(t, ProviderID("builtin-open-error"), PhysicalIdentity("open-error-identity"), coordclass.ClassGraph)
	primary := errors.New("provider open failed after allocating")
	cleanup := errors.New("provider open cleanup failed")
	direct := &directOpenedBinding{
		descriptor:   descriptor,
		capabilities: descriptor.Capabilities,
		graph:        testBeadsAdapters(t).Graph,
		graphOK:      true,
		closeErr:     cleanup,
	}
	_, err := OpenBinding(context.Background(), &recordingProvider{opened: direct, openErr: primary}, completeOpenRequest(t, descriptor, descriptor.Classes()))
	if !errors.Is(err, primary) || !errors.Is(err, cleanup) {
		t.Fatalf("OpenBinding() error = %v, want joined provider and cleanup causes", err)
	}
	if direct.closeCalls != 1 {
		t.Fatalf("OpenBinding() close calls = %d, want 1 for returned binding with error", direct.closeCalls)
	}
	var pending *RejectedOpenedBindingCleanupError
	if !errors.As(err, &pending) {
		t.Fatalf("OpenBinding() error = %T, want *RejectedOpenedBindingCleanupError", err)
	}
}

func TestOpenBindingWrapsAcceptedProviderBindingCloseOnce(t *testing.T) {
	descriptor := testDescriptor(t, ProviderID("builtin-wrap"), PhysicalIdentity("wrap-identity"), coordclass.ClassGraph)
	adapters := testBeadsAdapters(t)
	direct := &directOpenedBinding{
		descriptor:   descriptor,
		capabilities: descriptor.Capabilities,
		graph:        adapters.Graph,
		graphOK:      true,
	}
	opened, err := OpenBinding(context.Background(), &recordingProvider{opened: direct}, completeOpenRequest(t, descriptor, descriptor.Classes()))
	if err != nil {
		t.Fatalf("OpenBinding(): %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("first OpenedBinding.Close(): %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("second OpenedBinding.Close(): %v", err)
	}
	if direct.closeCalls != 1 {
		t.Fatalf("provider binding Close() calls = %d, want 1", direct.closeCalls)
	}
}

func TestOpenBindingRequiresAndClonesExactPinnedWorkTopology(t *testing.T) {
	descriptor := testDescriptor(t, ProviderID("builtin-pinned-work"), PhysicalIdentity("pinned-work-identity"), coordclass.ClassWork)
	hqStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	pinned, err := NewWorkTopology(
		workspace(HQScope(), hqStore, "hq", false),
		[]Workspace{workspace(RigScope("rig"), rigStore, "rig", true)},
	)
	if err != nil {
		t.Fatalf("NewWorkTopology(): %v", err)
	}
	request := completeOpenRequest(t, descriptor, descriptor.Classes())
	request.PinnedWork = &pinned

	t.Run("every frozen scope fact must match", func(t *testing.T) {
		for _, test := range []struct {
			name   string
			mutate func(*Workspace)
		}{
			{name: "prefix", mutate: func(workspace *Workspace) { workspace.Prefix = "other" }},
			{name: "suspension", mutate: func(workspace *Workspace) { workspace.Suspended = false }},
			{name: "opener identity", mutate: func(workspace *Workspace) { workspace.OpenerID = "other-opener" }},
			{name: "component identity", mutate: func(workspace *Workspace) { workspace.ComponentID = "other-component" }},
			{name: "physical identity", mutate: func(workspace *Workspace) { workspace.PhysicalID = "other-physical" }},
		} {
			t.Run(test.name, func(t *testing.T) {
				rig, err := pinned.ForScope(RigScope("rig"))
				if err != nil {
					t.Fatal(err)
				}
				test.mutate(&rig)
				drifted, err := NewWorkTopology(pinned.hq, []Workspace{rig})
				if err != nil {
					t.Fatal(err)
				}
				direct := &directOpenedBinding{descriptor: descriptor, capabilities: descriptor.Capabilities, work: drifted, workOK: true}
				_, err = OpenBinding(context.Background(), &recordingProvider{opened: direct}, request)
				if !errors.Is(err, ErrInvalidWorkParticipant) {
					t.Fatalf("OpenBinding() error = %v, want ErrInvalidWorkParticipant", err)
				}
			})
		}
	})

	t.Run("provider output cannot borrow caller topology", func(t *testing.T) {
		direct := &directOpenedBinding{descriptor: descriptor, capabilities: descriptor.Capabilities, work: pinned, workOK: true}
		opened, err := OpenBinding(context.Background(), &recordingProvider{opened: direct}, request)
		if err != nil {
			t.Fatalf("OpenBinding(): %v", err)
		}
		pinned.rigs[0].Prefix = "mutated-after-open"
		got, ok := opened.Work()
		if !ok {
			t.Fatal("opened binding omitted Work")
		}
		rig, err := got.ForScope(RigScope("rig"))
		if err != nil || rig.Prefix != "rig" {
			t.Fatalf("opened pinned topology = %#v, %v; want original frozen rig prefix", rig, err)
		}
	})
}

func TestGroupAndOpenRetainedWorkTopologyUsesScopedUnifiedAndMixedParticipantsOnce(t *testing.T) {
	members := []WorkWorkspaceMember{
		workMember(HQScope(), "hq", 0, false, "hq-workspace"),
		workMember(RigScope("alpha"), "alpha", 1, false, "alpha-workspace"),
		workMember(RigScope("bravo"), "bravo", 2, false, "shared-workspace"),
		workMember(RigScope("charlie"), "charlie", 3, true, "shared-workspace"),
	}
	participants, err := GroupWorkParticipants(members)
	if err != nil {
		t.Fatalf("GroupWorkParticipants(): %v", err)
	}
	if len(participants) != 3 {
		t.Fatalf("participants = %d, want 3 scoped/unified physical workspaces", len(participants))
	}
	var unifiedMembers int
	for _, participant := range participants {
		if participant.PhysicalIdentity == PhysicalIdentity("shared-workspace") {
			unifiedMembers = len(participant.Members)
		}
	}
	if unifiedMembers != 2 {
		t.Fatalf("unified participant members = %d, want 2", unifiedMembers)
	}

	requests := make([]RetainedWorkRequest, 0, len(participants))
	for _, participant := range participants {
		requests = append(requests, RetainedWorkRequest{
			Attempt:          AttemptID("retained-attempt"),
			Generation:       Generation(5),
			Participant:      participant,
			Source:           testRetainedWorkSource(participant),
			ExpectedContract: ContractVersion("storage-v1"),
		})
	}
	lifecycle := &recordingWorkMigration{}
	topology, handles, err := OpenRetainedWorkTopology(context.Background(), lifecycle, requests)
	if err != nil {
		t.Fatalf("OpenRetainedWorkTopology(): %v", err)
	}
	if len(handles) != 3 || len(topology.PhysicalWorkspaces()) != 3 || lifecycle.openCalls != 3 {
		t.Fatalf("opened handles:%d physical:%d provider opens:%d, want all 3", len(handles), len(topology.PhysicalWorkspaces()), lifecycle.openCalls)
	}
	for _, member := range members {
		if _, err := topology.ForScope(member.Scope); err != nil {
			t.Fatalf("topology missing %s: %v", member.Scope, err)
		}
	}

	_, _, err = OpenRetainedWorkTopology(context.Background(), lifecycle, []RetainedWorkRequest{requests[0], requests[0]})
	if !errors.Is(err, ErrDuplicateWorkParticipant) {
		t.Fatalf("duplicate retained requests error = %v, want ErrDuplicateWorkParticipant", err)
	}
	if lifecycle.openCalls != 3 {
		t.Fatalf("duplicate requests invoked provider.OpenRetained; calls = %d", lifecycle.openCalls)
	}
}

func TestOpenRetainedWorkTopologyJoinsOpenedWorkspaceCloseErrorAfterLaterOpenFailure(t *testing.T) {
	first, second, requests := retainedTopologyCloseRequests(t)
	closeErr := errors.New("close first retained workspace")
	firstWorkspace, err := NewRetainedWorkWorkspaceWithViews(first, retainedPrefixViews(first), func() error { return closeErr })
	if err != nil {
		t.Fatalf("NewRetainedWorkWorkspaceWithViews(first): %v", err)
	}
	openErr := errors.New("open second retained workspace")
	lifecycle := &recordingWorkMigration{retainedResults: []retainedOpenResult{
		{workspace: firstWorkspace},
		{err: openErr},
	}}

	_, handles, err := OpenRetainedWorkTopology(context.Background(), lifecycle, requests)
	if !errors.Is(err, openErr) {
		t.Fatalf("OpenRetainedWorkTopology() error = %v, want later open error", err)
	}
	if !errors.Is(err, closeErr) {
		t.Fatalf("OpenRetainedWorkTopology() error = %v, want first workspace close error", err)
	}
	if len(handles) != 1 || !handles[0].Participant.Equal(first) {
		t.Fatalf("OpenRetainedWorkTopology() handles = %#v, want the retryable first workspace", handles)
	}
	if lifecycle.openCalls != 2 || second.PhysicalIdentity == first.PhysicalIdentity {
		t.Fatalf("retained open calls/participants = %d/%q/%q, want 2 distinct opens", lifecycle.openCalls, first.PhysicalIdentity, second.PhysicalIdentity)
	}
}

func TestOpenRetainedWorkTopologyClosesReturnedWorkspaceBeforeEarlierHandlesOnOpenError(t *testing.T) {
	first, second, requests := retainedTopologyCloseRequests(t)
	openErr := errors.New("open returned retained workspace failed")
	firstCloseErr := errors.New("close earlier retained workspace")
	returnedCloseErr := errors.New("close returned retained workspace")
	var closeOrder []string
	firstAttempts := 0
	firstWorkspace, err := NewRetainedWorkWorkspaceWithViews(first, retainedPrefixViews(first), func() error {
		closeOrder = append(closeOrder, "first")
		firstAttempts++
		if firstAttempts == 1 {
			return firstCloseErr
		}
		return nil
	})
	if err != nil {
		t.Fatalf("NewRetainedWorkWorkspaceWithViews(first): %v", err)
	}
	returnedAttempts := 0
	returnedWorkspace, err := NewRetainedWorkWorkspaceWithViews(second, retainedPrefixViews(second), func() error {
		closeOrder = append(closeOrder, "returned")
		returnedAttempts++
		if returnedAttempts == 1 {
			return returnedCloseErr
		}
		return nil
	})
	if err != nil {
		t.Fatalf("NewRetainedWorkWorkspaceWithViews(returned): %v", err)
	}
	lifecycle := &recordingWorkMigration{retainedResults: []retainedOpenResult{
		{workspace: firstWorkspace},
		{workspace: returnedWorkspace, err: openErr},
	}}

	_, handles, err := OpenRetainedWorkTopology(context.Background(), lifecycle, requests)
	if !errors.Is(err, openErr) || !errors.Is(err, returnedCloseErr) || !errors.Is(err, firstCloseErr) {
		t.Fatalf("OpenRetainedWorkTopology() error = %v, want open and both cleanup errors", err)
	}
	if len(handles) != 2 || !handles[0].Participant.Equal(first) || !handles[1].Participant.Equal(second) {
		t.Fatalf("OpenRetainedWorkTopology() handles = %#v, want earlier then returned retryable handles", handles)
	}
	if len(closeOrder) != 2 || closeOrder[0] != "returned" || closeOrder[1] != "first" {
		t.Fatalf("cleanup close order = %#v, want returned then first", closeOrder)
	}
	if err := handles[1].Close(); err != nil {
		t.Fatalf("returned retry Close(): %v", err)
	}
	if err := handles[0].Close(); err != nil {
		t.Fatalf("first retry Close(): %v", err)
	}
}

func TestOpenRetainedWorkTopologyJoinsCloseErrorsWhenRejectingReturnedWorkspace(t *testing.T) {
	first, second, requests := retainedTopologyCloseRequests(t)
	firstCloseErr := errors.New("close first retained workspace")
	firstWorkspace, err := NewRetainedWorkWorkspaceWithViews(first, retainedPrefixViews(first), func() error { return firstCloseErr })
	if err != nil {
		t.Fatalf("NewRetainedWorkWorkspaceWithViews(first): %v", err)
	}
	unexpected, err := NewWorkWorkspaceParticipant(second.Provider, second.Component, PhysicalIdentity("unexpected-retained-workspace"), []WorkWorkspaceMember{
		workMember(RigScope("second"), "second", 2, false, "unexpected-retained-workspace"),
	})
	if err != nil {
		t.Fatalf("NewWorkWorkspaceParticipant(unexpected): %v", err)
	}
	returnedCloseErr := errors.New("close rejected retained workspace")
	returned, err := NewRetainedWorkWorkspaceWithViews(unexpected, retainedPrefixViews(unexpected), func() error { return returnedCloseErr })
	if err != nil {
		t.Fatalf("NewRetainedWorkWorkspaceWithViews(unexpected): %v", err)
	}
	lifecycle := &recordingWorkMigration{retainedResults: []retainedOpenResult{
		{workspace: firstWorkspace},
		{workspace: returned},
	}}

	_, handles, err := OpenRetainedWorkTopology(context.Background(), lifecycle, requests)
	if !errors.Is(err, ErrInvalidWorkParticipant) {
		t.Fatalf("OpenRetainedWorkTopology() error = %v, want ErrInvalidWorkParticipant", err)
	}
	if !errors.Is(err, firstCloseErr) || !errors.Is(err, returnedCloseErr) {
		t.Fatalf("OpenRetainedWorkTopology() error = %v, want both cleanup errors", err)
	}
	if len(handles) != 2 || !handles[0].Participant.Equal(first) || !handles[1].Participant.Equal(unexpected) {
		t.Fatalf("OpenRetainedWorkTopology() handles = %#v, want both retryable workspaces", handles)
	}
}

func retainedTopologyCloseRequests(t *testing.T) (WorkWorkspaceParticipant, WorkWorkspaceParticipant, []RetainedWorkRequest) {
	t.Helper()
	first, err := NewWorkWorkspaceParticipant(ProviderID("builtin-work"), ComponentID("work"), PhysicalIdentity("first-retained-workspace"), []WorkWorkspaceMember{
		workMember(HQScope(), "hq", 0, false, "first-retained-workspace"),
		workMember(RigScope("first"), "first", 1, false, "first-retained-workspace"),
	})
	if err != nil {
		t.Fatalf("NewWorkWorkspaceParticipant(first): %v", err)
	}
	second, err := NewWorkWorkspaceParticipant(ProviderID("builtin-work"), ComponentID("work"), PhysicalIdentity("second-retained-workspace"), []WorkWorkspaceMember{
		workMember(RigScope("second"), "second", 2, false, "second-retained-workspace"),
	})
	if err != nil {
		t.Fatalf("NewWorkWorkspaceParticipant(second): %v", err)
	}
	requests := []RetainedWorkRequest{
		{Attempt: AttemptID("retained-close"), Generation: Generation(5), Participant: first, Source: testRetainedWorkSource(first), ExpectedContract: ContractVersion("storage-v1")},
		{Attempt: AttemptID("retained-close"), Generation: Generation(5), Participant: second, Source: testRetainedWorkSource(second), ExpectedContract: ContractVersion("storage-v1")},
	}
	return first, second, requests
}

func TestOpenRetainedWorkTopologyPreResolvesMixedProviderParticipantsBeforeAnyOpen(t *testing.T) {
	first, err := NewWorkWorkspaceParticipant(ProviderID("provider-one"), ComponentID("work-one"), PhysicalIdentity("work-one"), []WorkWorkspaceMember{{
		Scope:            HQScope(),
		Prefix:           "hq",
		ConfigContext:    testConfigDigest("work-one-config"),
		ConfigOrder:      0,
		Provider:         ProviderID("provider-one"),
		Component:        ComponentID("work-one"),
		PhysicalIdentity: PhysicalIdentity("work-one"),
	}})
	if err != nil {
		t.Fatalf("NewWorkWorkspaceParticipant(first): %v", err)
	}
	second, err := NewWorkWorkspaceParticipant(ProviderID("provider-two"), ComponentID("work-two"), PhysicalIdentity("work-two"), []WorkWorkspaceMember{{
		Scope:            RigScope("rig"),
		Prefix:           "rig",
		ConfigContext:    testConfigDigest("work-two-config"),
		ConfigOrder:      1,
		Provider:         ProviderID("provider-two"),
		Component:        ComponentID("work-two"),
		PhysicalIdentity: PhysicalIdentity("work-two"),
	}})
	if err != nil {
		t.Fatalf("NewWorkWorkspaceParticipant(second): %v", err)
	}
	requests := []RetainedWorkRequest{
		{Attempt: AttemptID("mixed-retained"), Generation: Generation(5), Participant: first, Source: testRetainedWorkSource(first), ExpectedContract: ContractVersion("storage-v1")},
		{Attempt: AttemptID("mixed-retained"), Generation: Generation(5), Participant: second, Source: testRetainedWorkSource(second), ExpectedContract: ContractVersion("storage-v1")},
	}
	firstLifecycle := &recordingWorkMigration{}
	secondLifecycle := &recordingWorkMigration{}
	resolver := RetainedWorkLifecycleResolverFunc(func(_ context.Context, provider ProviderID) (WorkMigrationLifecycle, error) {
		switch provider {
		case first.Provider:
			return firstLifecycle, nil
		case second.Provider:
			return secondLifecycle, nil
		default:
			return nil, errors.New("unexpected provider")
		}
	})

	topology, handles, err := OpenRetainedWorkTopology(context.Background(), resolver, requests)
	if err != nil {
		t.Fatalf("OpenRetainedWorkTopology(): %v", err)
	}
	if len(handles) != 2 || firstLifecycle.openCalls != 1 || secondLifecycle.openCalls != 1 {
		t.Fatalf("mixed retained opens = handles:%d provider-one:%d provider-two:%d, want 2:1:1", len(handles), firstLifecycle.openCalls, secondLifecycle.openCalls)
	}
	if _, err := topology.ForScope(HQScope()); err != nil {
		t.Fatalf("mixed topology missing HQ: %v", err)
	}
	if _, err := topology.ForScope(RigScope("rig")); err != nil {
		t.Fatalf("mixed topology missing rig: %v", err)
	}

	failingResolver := RetainedWorkLifecycleResolverFunc(func(_ context.Context, provider ProviderID) (WorkMigrationLifecycle, error) {
		if provider == first.Provider {
			return firstLifecycle, nil
		}
		return nil, errors.New("second provider unavailable")
	})
	before := firstLifecycle.openCalls
	_, _, err = OpenRetainedWorkTopology(context.Background(), failingResolver, requests)
	if err == nil {
		t.Fatal("OpenRetainedWorkTopology() error = nil for an unresolved later provider")
	}
	if firstLifecycle.openCalls != before {
		t.Fatalf("later resolution failure opened earlier provider %d additional times", firstLifecycle.openCalls-before)
	}
}

func TestGroupWorkParticipantsPrevalidatesGlobalHQScopeAndConfigOrder(t *testing.T) {
	base := []WorkWorkspaceMember{
		workMember(HQScope(), "hq", 0, false, "workspace-one"),
		workMember(RigScope("rig"), "rig", 1, false, "workspace-two"),
	}
	tests := []struct {
		name    string
		members []WorkWorkspaceMember
	}{
		{
			name:    "duplicate HQ",
			members: append(append([]WorkWorkspaceMember(nil), base...), workMember(HQScope(), "other-hq", 2, false, "workspace-three")),
		},
		{
			name:    "duplicate scope",
			members: append(append([]WorkWorkspaceMember(nil), base...), workMember(RigScope("rig"), "other-rig", 2, false, "workspace-three")),
		},
		{
			name:    "duplicate config order across physical participants",
			members: append(append([]WorkWorkspaceMember(nil), base...), workMember(RigScope("other"), "other", 1, false, "workspace-three")),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := GroupWorkParticipants(test.members); !errors.Is(err, ErrInvalidWorkParticipant) {
				t.Fatalf("GroupWorkParticipants() error = %v, want ErrInvalidWorkParticipant", err)
			}
		})
	}
}

func TestOpenRetainedWorkTopologyRejectsFrozenParticipantDrift(t *testing.T) {
	participant, err := NewWorkWorkspaceParticipant(ProviderID("builtin-work"), ComponentID("work"), PhysicalIdentity("retained-workspace"), []WorkWorkspaceMember{
		workMember(HQScope(), "hq", 0, false, "retained-workspace"),
		workMember(RigScope("rig"), "rig", 1, false, "retained-workspace"),
	})
	if err != nil {
		t.Fatalf("NewWorkWorkspaceParticipant(): %v", err)
	}
	drifted := participant.Clone()
	drifted.Members[1].Prefix = "other-rig"
	workspace, err := NewRetainedWorkWorkspaceWithViews(drifted, retainedPrefixViews(drifted), func() error { return nil })
	if err != nil {
		t.Fatalf("NewRetainedWorkWorkspaceWithViews(): %v", err)
	}
	lifecycle := &recordingWorkMigration{retainedOverride: &workspace}
	_, _, err = OpenRetainedWorkTopology(context.Background(), lifecycle, []RetainedWorkRequest{{
		Attempt:          AttemptID("retained-attempt"),
		Generation:       Generation(5),
		Participant:      participant,
		Source:           testRetainedWorkSource(participant),
		ExpectedContract: ContractVersion("storage-v1"),
	}})
	if !errors.Is(err, ErrInvalidWorkParticipant) {
		t.Fatalf("OpenRetainedWorkTopology() error = %v, want ErrInvalidWorkParticipant", err)
	}
}

func TestOpenRetainedWorkTopologyRejectsFrozenPrefixDriftAndClosesWorkspace(t *testing.T) {
	participant, err := NewWorkWorkspaceParticipant(ProviderID("builtin-work"), ComponentID("work"), PhysicalIdentity("retained-prefix-workspace"), []WorkWorkspaceMember{
		workMember(HQScope(), "hq", 0, false, "retained-prefix-workspace"),
	})
	if err != nil {
		t.Fatalf("NewWorkWorkspaceParticipant(): %v", err)
	}
	store := &mutablePrefixProbe{Store: beads.NewMemStore(), frozen: "hq"}
	var closeCalls atomic.Int32
	workspace, err := NewRetainedWorkWorkspace(participant, store, func() error {
		closeCalls.Add(1)
		return nil
	})
	if err != nil {
		t.Fatalf("NewRetainedWorkWorkspace(): %v", err)
	}
	store.frozen = "changed-after-open"
	lifecycle := &recordingWorkMigration{retainedOverride: &workspace}

	_, handles, err := OpenRetainedWorkTopology(context.Background(), lifecycle, []RetainedWorkRequest{{
		Attempt:          AttemptID("retained-prefix-attempt"),
		Generation:       Generation(5),
		Participant:      participant,
		Source:           testRetainedWorkSource(participant),
		ExpectedContract: ContractVersion("storage-v1"),
	}})
	if !errors.Is(err, ErrInvalidWorkParticipant) {
		t.Fatalf("OpenRetainedWorkTopology() error = %v, want ErrInvalidWorkParticipant", err)
	}
	if handles != nil {
		t.Fatalf("OpenRetainedWorkTopology() handles = %#v, want nil after successful cleanup", handles)
	}
	if got := closeCalls.Load(); got != 1 {
		t.Fatalf("rejected workspace close calls = %d, want 1", got)
	}
}

func TestOpenRetainedWorkTopologyRejectsOwnershipOrContractDriftBeforeProviderOpen(t *testing.T) {
	participant, err := NewWorkWorkspaceParticipant(ProviderID("builtin-work"), ComponentID("work"), PhysicalIdentity("retained-workspace"), []WorkWorkspaceMember{
		workMember(HQScope(), "hq", 0, false, "retained-workspace"),
	})
	if err != nil {
		t.Fatalf("NewWorkWorkspaceParticipant(): %v", err)
	}
	base := RetainedWorkRequest{
		Attempt:          AttemptID("retained-attempt"),
		Generation:       Generation(5),
		Participant:      participant,
		Source:           testRetainedWorkSource(participant),
		ExpectedContract: ContractVersion("storage-v1"),
	}

	for _, test := range []struct {
		name   string
		mutate func(*RetainedWorkRequest)
	}{
		{
			name: "component differs",
			mutate: func(request *RetainedWorkRequest) {
				request.Source.Component = ComponentID("other-work")
			},
		},
		{
			name: "source does not own work only",
			mutate: func(request *RetainedWorkRequest) {
				request.Source.Classes = classSet(t, coordclass.ClassGraph)
			},
		},
		{
			name: "expected semantic contract differs",
			mutate: func(request *RetainedWorkRequest) {
				request.ExpectedContract = ContractVersion("storage-v2")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := base
			request.Source = request.Source.Clone()
			test.mutate(&request)
			lifecycle := &recordingWorkMigration{}

			_, _, err := OpenRetainedWorkTopology(context.Background(), lifecycle, []RetainedWorkRequest{request})
			if !errors.Is(err, ErrInvalidWorkParticipant) {
				t.Fatalf("OpenRetainedWorkTopology() error = %v, want ErrInvalidWorkParticipant", err)
			}
			if lifecycle.openCalls != 0 {
				t.Fatalf("OpenRetainedWorkTopology() called provider %d times after invalid retained ownership", lifecycle.openCalls)
			}
		})
	}
}

func TestOpenRetainedWorkTopologyAllowsSharedWorkAndInfrastructureComponent(t *testing.T) {
	participant, err := NewWorkWorkspaceParticipant(ProviderID("builtin-work"), ComponentID("work"), PhysicalIdentity("retained-shared-workspace"), []WorkWorkspaceMember{
		workMember(HQScope(), "hq", 0, false, "retained-shared-workspace"),
	})
	if err != nil {
		t.Fatalf("NewWorkWorkspaceParticipant(): %v", err)
	}
	source := testRetainedWorkSource(participant)
	source.Classes = classSet(t, coordclass.ClassWork, coordclass.ClassGraph)
	lifecycle := &recordingWorkMigration{}

	_, handles, err := OpenRetainedWorkTopology(context.Background(), lifecycle, []RetainedWorkRequest{{
		Attempt:          AttemptID("retained-shared-attempt"),
		Generation:       Generation(5),
		Participant:      participant,
		Source:           source,
		ExpectedContract: ContractVersion("storage-v1"),
	}})
	if err != nil {
		t.Fatalf("OpenRetainedWorkTopology(): %v", err)
	}
	if len(handles) != 1 || lifecycle.openCalls != 1 {
		t.Fatalf("retained shared component opens = handles:%d calls:%d, want 1:1", len(handles), lifecycle.openCalls)
	}
}

func TestWorkMigrationContractsPreserveForwardAndReverseGroupedParticipant(t *testing.T) {
	fixture := newWorkMigrationFixture(t)
	lifecycle := &recordingWorkMigration{}

	for _, direction := range []WorkMigrationDirection{WorkMigrationForward, WorkMigrationReverse} {
		prepareRequest := fixture.prepare
		prepareRequest.Direction = direction
		prepared, err := PrepareWork(context.Background(), lifecycle, prepareRequest)
		if err != nil {
			t.Fatalf("PrepareWork(%v): %v", direction, err)
		}
		proof, err := VerifyWork(context.Background(), lifecycle, WorkVerifyRequest{Prepare: prepareRequest, Prepared: prepared})
		if err != nil {
			t.Fatalf("VerifyWork(%v): %v", direction, err)
		}
		decision, err := NewCommitDecision(prepareRequest.Attempt, prepareRequest.Generation)
		if err != nil {
			t.Fatalf("NewCommitDecision(): %v", err)
		}
		if _, err := CommitWork(context.Background(), lifecycle, WorkCommitRequest{Decision: decision, Participant: fixture.participant, Prepare: prepareRequest, Prepared: prepared, Proof: proof}); err != nil {
			t.Fatalf("CommitWork(%v): %v", direction, err)
		}
		if _, err := ResumeWork(context.Background(), lifecycle, WorkResumeRequest{Prepare: prepareRequest}); err != nil {
			t.Fatalf("ResumeWork(%v): %v", direction, err)
		}
	}
	if len(lifecycle.prepareDirections) != 2 || lifecycle.prepareDirections[0] != WorkMigrationForward || lifecycle.prepareDirections[1] != WorkMigrationReverse {
		t.Fatalf("prepare directions = %#v, want forward then reverse", lifecycle.prepareDirections)
	}
}

func TestWorkMigrationReverseReconcilesCreateUpdateDeleteAcrossCrashReplayAndSecondForward(t *testing.T) {
	lifecycle := newReplayWorkMigration()
	forwardSource := testDescriptor(t, ProviderID("builtin-replay"), PhysicalIdentity("forward-source"), coordclass.ClassWork)
	forwardDestination := testDescriptor(t, ProviderID("builtin-replay"), PhysicalIdentity("forward-destination"), coordclass.ClassWork)
	lifecycle.setRecords(t, forwardSource, map[string]string{
		"kept":    "before",
		"updated": "before",
		"deleted": "before",
	})
	lifecycle.setRecords(t, forwardDestination, map[string]string{"stale": "remove"})

	forward := replayWorkRequest(t, forwardSource, forwardDestination, Generation(21), WorkMigrationForward)
	forwardPrepared, err := PrepareWork(context.Background(), lifecycle, forward)
	if err != nil {
		t.Fatalf("PrepareWork(forward): %v", err)
	}
	forwardProof, err := VerifyWork(context.Background(), lifecycle, WorkVerifyRequest{Prepare: forward, Prepared: forwardPrepared})
	if err != nil {
		t.Fatalf("VerifyWork(forward): %v", err)
	}
	forwardDecision, err := NewCommitDecision(forward.Attempt, forward.Generation)
	if err != nil {
		t.Fatalf("NewCommitDecision(forward): %v", err)
	}
	if _, err := CommitWork(context.Background(), lifecycle, WorkCommitRequest{Decision: forwardDecision, Participant: forward.Participant, Prepare: forward, Prepared: forwardPrepared, Proof: forwardProof}); err != nil {
		t.Fatalf("CommitWork(forward): %v", err)
	}
	if !logicalRecordsEqual(lifecycle.recordsFor(t, forwardSource), lifecycle.recordsFor(t, forwardDestination)) {
		t.Fatalf("forward destination records = %#v, want %#v", lifecycle.recordsFor(t, forwardDestination), lifecycle.recordsFor(t, forwardSource))
	}

	active := lifecycle.recordsFor(t, forwardDestination)
	active["created"] = "after"
	active["updated"] = "after"
	delete(active, "deleted")
	lifecycle.setRecords(t, forwardDestination, active)

	reverseDestination := testDescriptor(t, ProviderID("builtin-replay"), PhysicalIdentity("reverse-destination"), coordclass.ClassWork)
	lifecycle.setRecords(t, reverseDestination, map[string]string{
		"deleted": "resurrected-baseline",
		"stale":   "remove",
	})
	reverse := replayWorkRequest(t, forwardDestination, reverseDestination, Generation(22), WorkMigrationReverse)
	preparedBeforeCrash, err := PrepareWork(context.Background(), lifecycle, reverse)
	if err != nil {
		t.Fatalf("PrepareWork(reverse): %v", err)
	}
	if !logicalRecordsEqual(lifecycle.recordsFor(t, reverseDestination), lifecycle.recordsFor(t, forwardDestination)) {
		t.Fatalf("reverse destination records after prepare = %#v, want exact active authority %#v", lifecycle.recordsFor(t, reverseDestination), lifecycle.recordsFor(t, forwardDestination))
	}
	if _, found := lifecycle.recordsFor(t, reverseDestination)["deleted"]; found {
		t.Fatal("reverse reconciliation resurrected a post-forward deletion")
	}
	progressBeforeCommit, err := ResumeWork(context.Background(), lifecycle, WorkResumeRequest{Prepare: reverse})
	if err != nil {
		t.Fatalf("ResumeWork(before reverse commit): %v", err)
	}
	if progressBeforeCommit.Complete || progressBeforeCommit.Receipt != nil {
		t.Fatalf("reverse progress before commit = %#v, want incomplete replay state", progressBeforeCommit)
	}
	preparedAfterCrash, err := PrepareWork(context.Background(), lifecycle, reverse)
	if err != nil {
		t.Fatalf("PrepareWork(reverse replay): %v", err)
	}
	if !workPreparedEqual(preparedBeforeCrash, preparedAfterCrash) {
		t.Fatalf("reverse prepare replay = %#v, want %#v", preparedAfterCrash, preparedBeforeCrash)
	}
	if lifecycle.prepareCalls[replayPreparationKey(preparedBeforeCrash.Preparation)] != 2 {
		t.Fatalf("reverse prepare calls = %d, want 2 after crash replay", lifecycle.prepareCalls[replayPreparationKey(preparedBeforeCrash.Preparation)])
	}

	reverseProof, err := VerifyWork(context.Background(), lifecycle, WorkVerifyRequest{Prepare: reverse, Prepared: preparedAfterCrash})
	if err != nil {
		t.Fatalf("VerifyWork(reverse): %v", err)
	}
	reverseDecision, err := NewCommitDecision(reverse.Attempt, reverse.Generation)
	if err != nil {
		t.Fatalf("NewCommitDecision(reverse): %v", err)
	}
	reverseCommit := WorkCommitRequest{Decision: reverseDecision, Participant: reverse.Participant, Prepare: reverse, Prepared: preparedAfterCrash, Proof: reverseProof}
	firstReceipt, err := CommitWork(context.Background(), lifecycle, reverseCommit)
	if err != nil {
		t.Fatalf("CommitWork(reverse): %v", err)
	}
	secondReceipt, err := CommitWork(context.Background(), lifecycle, reverseCommit)
	if err != nil {
		t.Fatalf("CommitWork(reverse receipt replay): %v", err)
	}
	if !firstReceipt.Equal(secondReceipt) {
		t.Fatalf("reverse receipt replay = %#v, want %#v", secondReceipt, firstReceipt)
	}
	if lifecycle.commitCalls[replayPreparationKey(preparedAfterCrash.Preparation)] != 2 {
		t.Fatalf("reverse commit calls = %d, want 2 after receipt replay", lifecycle.commitCalls[replayPreparationKey(preparedAfterCrash.Preparation)])
	}
	progressAfterCommit, err := ResumeWork(context.Background(), lifecycle, WorkResumeRequest{Prepare: reverse})
	if err != nil {
		t.Fatalf("ResumeWork(after reverse commit): %v", err)
	}
	if !progressAfterCommit.Complete || progressAfterCommit.Receipt == nil || !progressAfterCommit.Receipt.Equal(firstReceipt) {
		t.Fatalf("reverse progress after receipt replay = %#v, want completed receipt %#v", progressAfterCommit, firstReceipt)
	}

	secondForwardDestination := testDescriptor(t, ProviderID("builtin-replay"), PhysicalIdentity("second-forward-destination"), coordclass.ClassWork)
	lifecycle.setRecords(t, secondForwardDestination, map[string]string{"stale": "remove"})
	secondForward := replayWorkRequest(t, reverseDestination, secondForwardDestination, Generation(23), WorkMigrationForward)
	secondPrepared, err := PrepareWork(context.Background(), lifecycle, secondForward)
	if err != nil {
		t.Fatalf("PrepareWork(second forward): %v", err)
	}
	secondProof, err := VerifyWork(context.Background(), lifecycle, WorkVerifyRequest{Prepare: secondForward, Prepared: secondPrepared})
	if err != nil {
		t.Fatalf("VerifyWork(second forward): %v", err)
	}
	secondDecision, err := NewCommitDecision(secondForward.Attempt, secondForward.Generation)
	if err != nil {
		t.Fatalf("NewCommitDecision(second forward): %v", err)
	}
	if _, err := CommitWork(context.Background(), lifecycle, WorkCommitRequest{Decision: secondDecision, Participant: secondForward.Participant, Prepare: secondForward, Prepared: secondPrepared, Proof: secondProof}); err != nil {
		t.Fatalf("CommitWork(second forward): %v", err)
	}
	if !logicalRecordsEqual(lifecycle.recordsFor(t, reverseDestination), lifecycle.recordsFor(t, secondForwardDestination)) {
		t.Fatalf("second forward destination records = %#v, want retained reverse authority %#v", lifecycle.recordsFor(t, secondForwardDestination), lifecycle.recordsFor(t, reverseDestination))
	}
}

func TestWorkVerificationRejectsPreparedOutputReusedWithChangedPreparationFacts(t *testing.T) {
	fixture := newWorkMigrationFixture(t)
	prepared, err := PrepareWork(context.Background(), &recordingWorkMigration{}, fixture.prepare)
	if err != nil {
		t.Fatalf("PrepareWork(): %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*WorkPrepareRequest)
	}{
		{
			name: "direction",
			mutate: func(request *WorkPrepareRequest) {
				request.Direction = WorkMigrationReverse
			},
		},
		{
			name: "witness version",
			mutate: func(request *WorkPrepareRequest) {
				request.WitnessVersion = "semantic-v2"
			},
		},
		{
			name: "config digest",
			mutate: func(request *WorkPrepareRequest) {
				request.ConfigDigest = testConfigDigest("other-work-config")
			},
		},
		{
			name: "source descriptor",
			mutate: func(request *WorkPrepareRequest) {
				request.Source = request.Source.Clone()
				request.Source.ImplementationVersion = "different-source-implementation"
				request.SourceFence = fenceForDescriptor(t, request.Source, FenceRoleSource, request.Generation)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := fixture.prepare
			test.mutate(&request)
			_, err := VerifyWork(context.Background(), &recordingWorkMigration{}, WorkVerifyRequest{Prepare: request, Prepared: prepared})
			if !errors.Is(err, ErrInvalidWorkParticipant) {
				t.Fatalf("VerifyWork() error = %v, want ErrInvalidWorkParticipant", err)
			}
		})
	}
}

func TestWorkProgressRequiresCompleteAndReceiptToAgree(t *testing.T) {
	fixture := newWorkMigrationFixture(t)
	for _, progress := range []WorkProgress{
		{Version: 1, Attempt: fixture.prepare.Attempt, Generation: fixture.prepare.Generation, Participant: fixture.participant, Preparation: fixture.preparation.Clone(), Complete: true},
		{
			Version:     1,
			Attempt:     fixture.prepare.Attempt,
			Generation:  fixture.prepare.Generation,
			Participant: fixture.participant,
			Preparation: fixture.preparation.Clone(),
			Complete:    false,
			Receipt: &ParticipantReceipt{
				Version:            1,
				Kind:               ParticipantReceiptWorkMigration,
				Attempt:            fixture.prepare.Attempt,
				Generation:         fixture.prepare.Generation,
				Participant:        fixture.participant.Key(),
				DescriptorIdentity: fixture.destinationIdentity,
				ReceiptID:          "contradictory-progress",
				Preparation:        fixture.preparation.Clone(),
				PreparedReceipt:    "prepared",
			},
		},
	} {
		if err := progress.Validate(); !errors.Is(err, ErrInvalidWorkParticipant) {
			t.Fatalf("WorkProgress.Validate() error = %v, want ErrInvalidWorkParticipant for %#v", err, progress)
		}
	}
}

func TestWorkMigrationRejectsPreparedReceiptAndReceiptKindDrift(t *testing.T) {
	fixture := newWorkMigrationFixture(t)

	t.Run("verify proof references another prepare receipt", func(t *testing.T) {
		proof := fixture.proof.Clone()
		proof.PreparedReceipt = "other-prepared-receipt"
		_, err := VerifyWork(context.Background(), &scriptedWorkMigration{proof: proof}, WorkVerifyRequest{Prepare: fixture.prepare, Prepared: fixture.prepared})
		if !errors.Is(err, ErrInvalidWorkParticipant) {
			t.Fatalf("VerifyWork() error = %v, want ErrInvalidWorkParticipant", err)
		}
	})

	t.Run("commit rejects a proof referencing another prepare receipt", func(t *testing.T) {
		proof := fixture.proof.Clone()
		proof.PreparedReceipt = "other-prepared-receipt"
		err := (WorkCommitRequest{Decision: fixture.decision, Participant: fixture.participant, Prepare: fixture.prepare, Prepared: fixture.prepared, Proof: proof}).Validate()
		if !errors.Is(err, ErrInvalidWorkParticipant) {
			t.Fatalf("WorkCommitRequest.Validate() error = %v, want ErrInvalidWorkParticipant", err)
		}
	})

	t.Run("commit rejects an activation receipt", func(t *testing.T) {
		lifecycle := &scriptedWorkMigration{receipt: ParticipantReceipt{
			Version:            1,
			Kind:               ParticipantReceiptBindingActivation,
			Attempt:            fixture.decision.Attempt,
			Generation:         fixture.decision.Generation,
			Participant:        fixture.participant.Key(),
			DescriptorIdentity: fixture.destinationIdentity,
			ReceiptID:          "activation-receipt",
		}}
		_, err := CommitWork(context.Background(), lifecycle, WorkCommitRequest{Decision: fixture.decision, Participant: fixture.participant, Prepare: fixture.prepare, Prepared: fixture.prepared, Proof: fixture.proof})
		if !errors.Is(err, ErrInvalidWorkParticipant) {
			t.Fatalf("CommitWork() error = %v, want ErrInvalidWorkParticipant", err)
		}
	})

	t.Run("completed resume rejects an activation receipt", func(t *testing.T) {
		lifecycle := &scriptedWorkMigration{progress: WorkProgress{
			Version:     1,
			Attempt:     fixture.prepare.Attempt,
			Generation:  fixture.prepare.Generation,
			Participant: fixture.participant.Clone(),
			Preparation: fixture.preparation.Clone(),
			Complete:    true,
			Receipt: &ParticipantReceipt{
				Version:            1,
				Kind:               ParticipantReceiptBindingActivation,
				Attempt:            fixture.prepare.Attempt,
				Generation:         fixture.prepare.Generation,
				Participant:        fixture.participant.Key(),
				DescriptorIdentity: fixture.destinationIdentity,
				ReceiptID:          "activation-receipt",
			},
		}}
		_, err := ResumeWork(context.Background(), lifecycle, WorkResumeRequest{Prepare: fixture.prepare})
		if !errors.Is(err, ErrInvalidWorkParticipant) {
			t.Fatalf("ResumeWork() error = %v, want ErrInvalidWorkParticipant", err)
		}
	})

	t.Run("activation receipt rejects work provenance", func(t *testing.T) {
		receipt := ParticipantReceipt{
			Version:            1,
			Kind:               ParticipantReceiptBindingActivation,
			Attempt:            fixture.decision.Attempt,
			Generation:         fixture.decision.Generation,
			Participant:        "binding",
			DescriptorIdentity: fixture.destinationIdentity,
			ReceiptID:          "activation-receipt",
			Preparation:        fixture.preparation.Clone(),
		}
		if err := receipt.Validate(); err == nil {
			t.Fatal("ParticipantReceipt.Validate() succeeded for activation receipt with work provenance")
		}
	})
}

func TestPrepareWorkClonesParticipantProvenanceAtProviderBoundary(t *testing.T) {
	fixture := newWorkMigrationFixture(t)
	lifecycle := &mutatingPrepareWorkMigration{scriptedWorkMigration: scriptedWorkMigration{prepared: fixture.prepared}}

	prepared, err := PrepareWork(context.Background(), lifecycle, fixture.prepare)
	if err != nil {
		t.Fatalf("PrepareWork(): %v", err)
	}
	if fixture.prepare.Participant.Members[0].Prefix != "hq" {
		t.Fatalf("provider mutation changed caller request prefix to %q", fixture.prepare.Participant.Members[0].Prefix)
	}
	prepared.Preparation.Participant.Members[0].Prefix = "caller-mutated"
	if lifecycle.prepared.Preparation.Participant.Members[0].Prefix != "hq" {
		t.Fatalf("caller mutation changed provider-owned preparation prefix to %q", lifecycle.prepared.Preparation.Participant.Members[0].Prefix)
	}
}

func TestWorkMigrationRejectsParticipantAndReceiptIdentityDrift(t *testing.T) {
	fixture := newWorkMigrationFixture(t)
	drifted := fixture.participant.Clone()
	drifted.Members[1].Suspended = !drifted.Members[1].Suspended

	t.Run("prepare returned participant member drift", func(t *testing.T) {
		lifecycle := &scriptedWorkMigration{
			prepared: WorkPrepared{
				Version:            1,
				Attempt:            fixture.prepare.Attempt,
				Generation:         fixture.prepare.Generation,
				Participant:        drifted,
				DescriptorIdentity: fixture.destinationIdentity,
				Preparation:        fixture.preparation.Clone(),
				Receipt:            "prepared",
			},
		}
		_, err := PrepareWork(context.Background(), lifecycle, fixture.prepare)
		if !errors.Is(err, ErrInvalidWorkParticipant) {
			t.Fatalf("PrepareWork() error = %v, want ErrInvalidWorkParticipant", err)
		}
	})

	t.Run("verify returned participant member drift", func(t *testing.T) {
		lifecycle := &scriptedWorkMigration{
			proof: WorkProof{
				Version:            1,
				Attempt:            fixture.prepare.Attempt,
				Generation:         fixture.prepare.Generation,
				Participant:        drifted,
				DescriptorIdentity: fixture.destinationIdentity,
				Preparation:        fixture.preparation.Clone(),
				PreparedReceipt:    fixture.prepared.Receipt,
				Witness:            "witness",
			},
		}
		_, err := VerifyWork(context.Background(), lifecycle, WorkVerifyRequest{Prepare: fixture.prepare, Prepared: fixture.prepared})
		if !errors.Is(err, ErrInvalidWorkParticipant) {
			t.Fatalf("VerifyWork() error = %v, want ErrInvalidWorkParticipant", err)
		}
	})

	t.Run("commit receipt physical participant drift", func(t *testing.T) {
		lifecycle := &scriptedWorkMigration{
			receipt: ParticipantReceipt{
				Version:            1,
				Kind:               ParticipantReceiptWorkMigration,
				Attempt:            fixture.decision.Attempt,
				Generation:         fixture.decision.Generation,
				Participant:        "other-physical-participant",
				DescriptorIdentity: fixture.destinationIdentity,
				ReceiptID:          "committed",
				Preparation:        fixture.preparation.Clone(),
				PreparedReceipt:    fixture.prepared.Receipt,
			},
		}
		_, err := CommitWork(context.Background(), lifecycle, WorkCommitRequest{Decision: fixture.decision, Participant: fixture.participant, Prepare: fixture.prepare, Prepared: fixture.prepared, Proof: fixture.proof})
		if !errors.Is(err, ErrInvalidWorkParticipant) {
			t.Fatalf("CommitWork() error = %v, want ErrInvalidWorkParticipant", err)
		}
	})

	t.Run("commit receipt destination identity drift", func(t *testing.T) {
		lifecycle := &scriptedWorkMigration{
			receipt: ParticipantReceipt{
				Version:            1,
				Kind:               ParticipantReceiptWorkMigration,
				Attempt:            fixture.decision.Attempt,
				Generation:         fixture.decision.Generation,
				Participant:        fixture.participant.Key(),
				DescriptorIdentity: testBindingIdentity("other-destination"),
				ReceiptID:          "committed",
				Preparation:        fixture.preparation.Clone(),
				PreparedReceipt:    fixture.prepared.Receipt,
			},
		}
		_, err := CommitWork(context.Background(), lifecycle, WorkCommitRequest{Decision: fixture.decision, Participant: fixture.participant, Prepare: fixture.prepare, Prepared: fixture.prepared, Proof: fixture.proof})
		if !errors.Is(err, ErrInvalidWorkParticipant) {
			t.Fatalf("CommitWork() error = %v, want ErrInvalidWorkParticipant", err)
		}
	})

	t.Run("resume returned participant member drift", func(t *testing.T) {
		lifecycle := &scriptedWorkMigration{
			progress: WorkProgress{
				Version:     1,
				Attempt:     fixture.prepare.Attempt,
				Generation:  fixture.prepare.Generation,
				Participant: drifted,
				Preparation: fixture.preparation.Clone(),
			},
		}
		_, err := ResumeWork(context.Background(), lifecycle, WorkResumeRequest{Prepare: fixture.prepare})
		if !errors.Is(err, ErrInvalidWorkParticipant) {
			t.Fatalf("ResumeWork() error = %v, want ErrInvalidWorkParticipant", err)
		}
	})

	t.Run("prepare prior receipt identity drift", func(t *testing.T) {
		request := fixture.prepare
		request.PriorReceipt = &ParticipantReceipt{
			Version:            1,
			Kind:               ParticipantReceiptWorkMigration,
			Attempt:            request.Attempt,
			Generation:         request.Generation,
			Participant:        "other-physical-participant",
			DescriptorIdentity: fixture.destinationIdentity,
			ReceiptID:          "prior",
			Preparation:        fixture.preparation.Clone(),
			PreparedReceipt:    fixture.prepared.Receipt,
		}
		_, err := PrepareWork(context.Background(), &recordingWorkMigration{}, request)
		if !errors.Is(err, ErrInvalidWorkParticipant) {
			t.Fatalf("PrepareWork() error = %v, want ErrInvalidWorkParticipant", err)
		}
	})

	t.Run("resume progress receipt identity drift", func(t *testing.T) {
		lifecycle := &scriptedWorkMigration{
			progress: WorkProgress{
				Version:     1,
				Attempt:     fixture.prepare.Attempt,
				Generation:  fixture.prepare.Generation,
				Participant: fixture.participant,
				Preparation: fixture.preparation.Clone(),
				Complete:    true,
				Receipt: &ParticipantReceipt{
					Version:            1,
					Kind:               ParticipantReceiptWorkMigration,
					Attempt:            fixture.prepare.Attempt,
					Generation:         fixture.prepare.Generation,
					Participant:        fixture.participant.Key(),
					DescriptorIdentity: testBindingIdentity("other-destination"),
					ReceiptID:          "resumed",
					Preparation:        fixture.preparation.Clone(),
					PreparedReceipt:    fixture.prepared.Receipt,
				},
			},
		}
		_, err := ResumeWork(context.Background(), lifecycle, WorkResumeRequest{Prepare: fixture.prepare})
		if !errors.Is(err, ErrInvalidWorkParticipant) {
			t.Fatalf("ResumeWork() error = %v, want ErrInvalidWorkParticipant", err)
		}
	})
}

func TestPrepareWorkRejectsHeldFencesForWrongRoleGenerationOrPhysicalTarget(t *testing.T) {
	participant, err := NewWorkWorkspaceParticipant(ProviderID("builtin-work"), ComponentID("component"), PhysicalIdentity("source-physical"), []WorkWorkspaceMember{
		{
			Scope:            HQScope(),
			Prefix:           "hq",
			ConfigContext:    testConfigDigest("work-config"),
			ConfigOrder:      0,
			Provider:         ProviderID("builtin-work"),
			Component:        ComponentID("component"),
			PhysicalIdentity: PhysicalIdentity("source-physical"),
		},
	})
	if err != nil {
		t.Fatalf("NewWorkWorkspaceParticipant(): %v", err)
	}
	source := testDescriptor(t, participant.Provider, participant.PhysicalIdentity, coordclass.ClassWork)
	destination := testDescriptor(t, participant.Provider, PhysicalIdentity("destination-physical"), coordclass.ClassWork)
	validSourceFence := fenceForDescriptor(t, source, FenceRoleSource, Generation(12))
	validDestinationFence := fenceForDescriptor(t, destination, FenceRolePopulatedDestination, Generation(12))
	base := WorkPrepareRequest{
		Attempt:          AttemptID("fence-attempt"),
		Generation:       Generation(12),
		Direction:        WorkMigrationForward,
		Participant:      participant,
		Source:           source,
		Destination:      destination,
		SourceFence:      validSourceFence,
		DestinationFence: validDestinationFence,
		WitnessVersion:   "semantic-v1",
		ConfigDigest:     testConfigDigest("work-config"),
	}

	tests := []struct {
		name   string
		mutate func(*WorkPrepareRequest)
		want   error
	}{
		{
			name: "source role",
			mutate: func(request *WorkPrepareRequest) {
				request.SourceFence = fenceForDescriptor(t, source, FenceRoleActiveVerification, request.Generation)
			},
			want: ErrInvalidFence,
		},
		{
			name: "destination generation",
			mutate: func(request *WorkPrepareRequest) {
				request.DestinationFence = fenceForDescriptor(t, destination, FenceRolePopulatedDestination, request.Generation+1)
			},
			want: ErrInvalidFence,
		},
		{
			name: "moved source component",
			mutate: func(request *WorkPrepareRequest) {
				moved := source.Clone()
				moved.Components[0].PhysicalIdentity = PhysicalIdentity("moved-source")
				request.SourceFence = fenceForDescriptor(t, moved, FenceRoleSource, request.Generation)
			},
			want: ErrFenceTargetMoved,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := base
			test.mutate(&request)
			lifecycle := &recordingWorkMigration{}
			_, err := PrepareWork(context.Background(), lifecycle, request)
			if !errors.Is(err, test.want) {
				t.Fatalf("PrepareWork() error = %v, want %v", err, test.want)
			}
			if len(lifecycle.prepareDirections) != 0 {
				t.Fatalf("PrepareWork called provider despite invalid fence: %#v", lifecycle.prepareDirections)
			}
		})
	}
}

func TestCGOUnavailableErrorIsTypedAndProviderUnavailable(t *testing.T) {
	err := NewCGOUnavailableError(ProviderID("builtin-cgo"), "open")
	if !errors.Is(err, ErrCGOUnavailable) || !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("CGOUnavailableError = %v, want both typed causes", err)
	}
	var typed *CGOUnavailableError
	if !errors.As(err, &typed) {
		t.Fatalf("errors.As() = %#v, want typed cgo error", typed)
	}
}

type workMigrationFixture struct {
	participant         WorkWorkspaceParticipant
	prepare             WorkPrepareRequest
	preparation         WorkPreparationIdentity
	prepared            WorkPrepared
	proof               WorkProof
	decision            CommitDecision
	destinationIdentity BindingIdentity
}

func newWorkMigrationFixture(t *testing.T) workMigrationFixture {
	t.Helper()
	const (
		provider    = ProviderID("builtin-work")
		component   = ComponentID("component")
		sourceID    = PhysicalIdentity("source-work")
		destination = PhysicalIdentity("destination-work")
		generation  = Generation(11)
	)
	participant, err := NewWorkWorkspaceParticipant(provider, component, sourceID, []WorkWorkspaceMember{
		{
			Scope:            HQScope(),
			Prefix:           "hq",
			ConfigContext:    testConfigDigest("work-config"),
			ConfigOrder:      0,
			Provider:         provider,
			Component:        component,
			PhysicalIdentity: sourceID,
		},
		{
			Scope:            RigScope("rig"),
			Prefix:           "rig",
			ConfigContext:    testConfigDigest("work-config"),
			ConfigOrder:      1,
			Provider:         provider,
			Component:        component,
			PhysicalIdentity: sourceID,
		},
	})
	if err != nil {
		t.Fatalf("NewWorkWorkspaceParticipant(): %v", err)
	}
	source := testDescriptor(t, provider, sourceID, coordclass.ClassWork)
	destinationDescriptor := testDescriptor(t, provider, destination, coordclass.ClassWork)
	destinationIdentity, err := destinationDescriptor.Identity()
	if err != nil {
		t.Fatalf("Destination.Identity(): %v", err)
	}
	prepare := WorkPrepareRequest{
		Attempt:          AttemptID("work-attempt"),
		Generation:       generation,
		Direction:        WorkMigrationForward,
		Participant:      participant,
		Source:           source,
		Destination:      destinationDescriptor,
		SourceFence:      fenceForDescriptor(t, source, FenceRoleSource, generation),
		DestinationFence: fenceForDescriptor(t, destinationDescriptor, FenceRolePopulatedDestination, generation),
		WitnessVersion:   "semantic-v1",
		ConfigDigest:     testConfigDigest("work-config"),
	}
	preparation, err := prepare.PreparationIdentity()
	if err != nil {
		t.Fatalf("WorkPrepareRequest.PreparationIdentity(): %v", err)
	}
	decision, err := NewCommitDecision(prepare.Attempt, prepare.Generation)
	if err != nil {
		t.Fatalf("NewCommitDecision(): %v", err)
	}
	return workMigrationFixture{
		participant: participant,
		prepare:     prepare,
		preparation: preparation,
		prepared: WorkPrepared{
			Version:            1,
			Attempt:            prepare.Attempt,
			Generation:         prepare.Generation,
			Participant:        participant,
			DescriptorIdentity: destinationIdentity,
			Preparation:        preparation.Clone(),
			Receipt:            "prepared",
		},
		proof: WorkProof{
			Version:            1,
			Attempt:            prepare.Attempt,
			Generation:         prepare.Generation,
			Participant:        participant,
			DescriptorIdentity: destinationIdentity,
			Preparation:        preparation.Clone(),
			PreparedReceipt:    "prepared",
			Witness:            "witness",
		},
		decision:            decision,
		destinationIdentity: destinationIdentity,
	}
}

func testBeadsAdapters(t *testing.T) BeadsAdapters {
	t.Helper()
	adapters, err := NewBeadsAdapters(beads.NewMemStore(), BeadsAdapterIdentity{
		OpenerID:    "test-opener",
		ComponentID: "test-component",
		PhysicalID:  "test-physical",
	})
	if err != nil {
		t.Fatalf("NewBeadsAdapters(): %v", err)
	}
	return adapters
}

func descriptorWithoutCapabilities(descriptor Descriptor) Descriptor {
	descriptor.Capabilities = ClassCapabilities{}
	return descriptor
}

func testSHA256Digest(label string) string {
	sum := sha256.Sum256([]byte(label))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func testConfigDigest(label string) ConfigRefDigest {
	return ConfigRefDigest(testSHA256Digest(label))
}

func testBindingIdentity(label string) BindingIdentity {
	return BindingIdentity(testSHA256Digest(label))
}

func testDescriptor(t *testing.T, provider ProviderID, identity PhysicalIdentity, class coordclass.Class) Descriptor {
	t.Helper()
	classes := classSet(t, class)
	descriptor := Descriptor{
		Version:                 1,
		SemanticContractVersion: ContractVersion("storage-v1"),
		Provider:                provider,
		ImplementationVersion:   "implementation-v1",
		ConfigRefDigest:         testConfigDigest("descriptor-config"),
		Capabilities:            capabilitiesFor(class),
		Components: []ComponentDescriptor{{
			ID:               ComponentID("component"),
			Locator:          ComponentLocator("file:/city/" + string(identity)),
			PhysicalIdentity: identity,
			Classes:          classes,
			Format:           FormatID("builtin-format"),
			SchemaVersion:    "1",
			Marker:           MarkerState{Name: "marker", Present: true},
		}},
	}
	if err := descriptor.Validate(); err != nil {
		t.Fatalf("Descriptor.Validate(): %v", err)
	}
	return descriptor
}

func completeOpenRequest(t *testing.T, descriptor Descriptor, assigned ClassSet) OpenRequest {
	t.Helper()
	expected := make([]ComponentCompatibilityRequirement, len(descriptor.Components))
	for index, component := range descriptor.Components {
		expected[index] = ComponentCompatibilityRequirement{
			Component:     component.ID,
			Format:        component.Format,
			SchemaVersion: component.SchemaVersion,
			ABIVersion:    component.ABIVersion,
		}
	}
	requirements := make([]ClassCapabilityRequirement, 0, len(assigned.Classes()))
	for _, class := range assigned.Classes() {
		requirements = append(requirements, ClassCapabilityRequirement{
			Class:               class,
			RequireTransactions: true,
			RequireClaims:       true,
		})
	}
	authority, err := NewDurableActiveOpenAuthority(Generation(1), descriptor)
	if err != nil {
		t.Fatalf("NewDurableActiveOpenAuthority(): %v", err)
	}
	return OpenRequest{
		Descriptor:             descriptor,
		AssignedClasses:        assigned,
		Mode:                   OpenModeActive,
		ExpectedGeneration:     Generation(1),
		ExpectedContract:       descriptor.SemanticContractVersion,
		ExpectedComponents:     expected,
		ClassRequirements:      requirements,
		DurableActiveAuthority: authority,
	}
}

func capabilitiesFor(classes ...coordclass.Class) ClassCapabilities {
	var capabilities ClassCapabilities
	for _, class := range classes {
		capability := ClassCapability{Available: true, Transactions: true, Claims: true}
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
	}
	return capabilities
}

func testRetainedSource(identity PhysicalIdentity) RetainedSourceRef {
	return RetainedSourceRef{
		Version:                 1,
		Provider:                ProviderID("builtin-work"),
		ImplementationVersion:   "implementation-v1",
		Component:               ComponentID("component"),
		Classes:                 workOnlyClasses(),
		SemanticContractVersion: ContractVersion("storage-v1"),
		Format:                  FormatID("builtin-format"),
		SchemaVersion:           "1",
		PhysicalIdentity:        identity,
		ConfigRefDigest:         testConfigDigest("retained-config"),
		WitnessVersion:          "semantic-v1",
		WitnessDigest:           testSHA256Digest("retained-witness"),
		ReopenData:              []byte("opaque-reopen"),
	}
}

func fencedGuardInstallRequest(t *testing.T, install GuardInstallRequest) FencedGuardInstallRequest {
	t.Helper()
	target, err := NewFenceTarget(install.Source.Provider, install.Source.Classes, []FenceComponentTarget{{
		ID:               install.Component,
		Locator:          ComponentLocator("retained:" + string(install.PhysicalIdentity)),
		PhysicalIdentity: install.PhysicalIdentity,
		Classes:          install.Source.Classes,
	}})
	if err != nil {
		t.Fatalf("NewFenceTarget(): %v", err)
	}
	return FencedGuardInstallRequest{
		GuardInstallRequest: install.Clone(),
		SourceFence:         &recordingFence{target: target, role: FenceRoleSource, generation: install.Generation, held: true},
	}
}

func fencedGuardPlan(t *testing.T, installs ...GuardInstallRequest) []FencedGuardInstallRequest {
	t.Helper()
	requests := make([]FencedGuardInstallRequest, len(installs))
	for index, install := range installs {
		requests[index] = fencedGuardInstallRequest(t, install)
	}
	return requests
}

func testRetainedWorkSource(participant WorkWorkspaceParticipant) RetainedSourceRef {
	source := testRetainedSource(participant.PhysicalIdentity)
	source.Provider = participant.Provider
	source.Component = participant.Component
	return source
}

func fenceForDescriptor(t *testing.T, descriptor Descriptor, role FenceRole, generation Generation) WriterFence {
	t.Helper()
	components := make([]FenceComponentTarget, len(descriptor.Components))
	for index, component := range descriptor.Components {
		components[index] = FenceComponentTarget{ID: component.ID, Locator: component.Locator, PhysicalIdentity: component.PhysicalIdentity, Classes: component.Classes}
	}
	target, err := NewFenceTarget(descriptor.Provider, descriptor.Classes(), components)
	if err != nil {
		t.Fatalf("NewFenceTarget(): %v", err)
	}
	return &recordingFence{target: target, role: role, generation: generation, held: true}
}

func matchingGuardReceipt(request GuardInstallRequest) GuardReceipt {
	return GuardReceipt{
		Version:                 1,
		Attempt:                 request.Attempt,
		Generation:              request.Generation,
		Provider:                request.Source.Provider,
		Component:               request.Component,
		PhysicalIdentity:        request.PhysicalIdentity,
		Classes:                 request.Source.Classes,
		SemanticContractVersion: request.Source.SemanticContractVersion,
		Role:                    request.Role,
		ReceiptID:               "guard-receipt",
		Revalidation:            "guard-proof",
	}
}

func testPredecisionAbandonmentAuthority(t *testing.T, current GuardReceipt, descriptor Descriptor, fence WriterFence) PredecisionAbandonmentAuthority {
	t.Helper()
	identity, err := descriptor.Identity()
	if err != nil {
		t.Fatalf("Descriptor.Identity(): %v", err)
	}
	return PredecisionAbandonmentAuthority{
		version:                  1,
		attempt:                  current.Attempt,
		generation:               current.Generation,
		provider:                 current.Provider,
		component:                current.Component,
		physicalIdentity:         current.PhysicalIdentity,
		classes:                  current.Classes,
		semanticContractVersion:  current.SemanticContractVersion,
		role:                     current.Role,
		receiptID:                current.ReceiptID,
		revalidation:             current.Revalidation,
		sourceDescriptorIdentity: identity,
		sourceFenceTarget:        fence.Target().Clone(),
	}
}

func guardReceiptsEqual(left, right GuardReceipt) bool {
	return left.Version == right.Version && left.Attempt == right.Attempt && left.Generation == right.Generation && left.Provider == right.Provider && left.Component == right.Component && left.PhysicalIdentity == right.PhysicalIdentity && left.Classes.Equal(right.Classes) && left.SemanticContractVersion == right.SemanticContractVersion && left.Role == right.Role && left.TransferState == right.TransferState && left.TransferParticipant == right.TransferParticipant && left.TransferDestinationIdentity == right.TransferDestinationIdentity && left.TransferReceiptKind == right.TransferReceiptKind && participantReceiptsEqual(left.ActiveProof, right.ActiveProof) && left.ReceiptID == right.ReceiptID && left.Revalidation == right.Revalidation
}

func discardGuardReceipt(context.Context, GuardReceipt) error { return nil }

type recordingGuardLifecycle struct {
	provider           ProviderID
	discovery          GuardDiscovery
	transitionReceipt  GuardReceipt
	verifyErr          error
	discoverErr        error
	installErr         error
	transitionErr      error
	mutateInstall      func(*FencedGuardInstallRequest)
	mutateDiscover     func(*FencedGuardDiscoverRequest)
	mutateVerify       func(*FencedGuardVerificationRequest)
	mutateTransition   func(context.Context, *GuardTransitionRequest)
	discoverCalls      int
	installCalls       int
	verifyCalls        int
	transitionCalls    int
	wrongProviderCalls int
}

func (l *recordingGuardLifecycle) Install(_ context.Context, request FencedGuardInstallRequest) (GuardReceipt, error) {
	if l.provider != "" && request.Source.Provider != l.provider {
		l.wrongProviderCalls++
		return GuardReceipt{}, ErrInvalidGuard
	}
	l.installCalls++
	if l.mutateInstall != nil {
		l.mutateInstall(&request)
	}
	return matchingGuardReceipt(request.GuardInstallRequest), l.installErr
}

func (l *recordingGuardLifecycle) Discover(_ context.Context, request FencedGuardDiscoverRequest) (GuardDiscovery, error) {
	if l.provider != "" && request.Source.Provider != l.provider {
		l.wrongProviderCalls++
		return GuardDiscovery{}, ErrInvalidGuard
	}
	l.discoverCalls++
	if l.mutateDiscover != nil {
		l.mutateDiscover(&request)
	}
	return l.discovery, l.discoverErr
}

func (l *recordingGuardLifecycle) Verify(_ context.Context, request FencedGuardVerificationRequest) error {
	if l.provider != "" && request.Receipt.Provider != l.provider {
		l.wrongProviderCalls++
		return ErrInvalidGuard
	}
	l.verifyCalls++
	if l.mutateVerify != nil {
		l.mutateVerify(&request)
	}
	return l.verifyErr
}

func (l *recordingGuardLifecycle) ReleaseOrTransfer(ctx context.Context, request GuardTransitionRequest) (GuardReceipt, error) {
	if l.provider != "" && request.Current.Provider != l.provider {
		l.wrongProviderCalls++
		return GuardReceipt{}, ErrInvalidGuard
	}
	l.transitionCalls++
	if l.mutateTransition != nil {
		l.mutateTransition(ctx, &request)
	}
	if l.transitionReceipt.Version == 0 {
		return GuardReceipt{}, errors.New("not used")
	}
	return l.transitionReceipt, l.transitionErr
}

func (l *recordingGuardLifecycle) ResolveRetainedGuardLifecycle(_ context.Context, provider ProviderID) (RetainedGuardLifecycle, error) {
	if l.provider != "" && provider != l.provider {
		return nil, fmt.Errorf("%w: retained guard lifecycle for provider %q", ErrMissingCapability, provider)
	}
	return l, nil
}

type recordingGuardLifecycleResolver struct {
	lifecycles   map[ProviderID]RetainedGuardLifecycle
	resolveCalls []ProviderID
}

func (r *recordingGuardLifecycleResolver) ResolveRetainedGuardLifecycle(_ context.Context, provider ProviderID) (RetainedGuardLifecycle, error) {
	r.resolveCalls = append(r.resolveCalls, provider)
	lifecycle := r.lifecycles[provider]
	if isNilInterface(lifecycle) {
		return nil, fmt.Errorf("%w: retained guard lifecycle for provider %q", ErrMissingCapability, provider)
	}
	return lifecycle, nil
}

type recordingBindingMigration struct {
	activateCalls   int
	resumeCalls     int
	activateReceipt ParticipantReceipt
	resumeReceipt   ParticipantReceipt
	activateErr     error
	resumeErr       error
	afterActivate   func(context.Context, BindingActivationRequest)
	afterResume     func(context.Context, BindingActivationResumeRequest)
}

func (l *recordingBindingMigration) Activate(ctx context.Context, request BindingActivationRequest) (ParticipantReceipt, error) {
	l.activateCalls++
	if l.afterActivate != nil {
		l.afterActivate(ctx, request)
	}
	if l.activateReceipt.Version != 0 {
		return l.activateReceipt, l.activateErr
	}
	return ParticipantReceipt{}, l.activateErr
}

func (l *recordingBindingMigration) ResumeActivation(ctx context.Context, request BindingActivationResumeRequest) (ParticipantReceipt, error) {
	l.resumeCalls++
	if l.afterResume != nil {
		l.afterResume(ctx, request)
	}
	return l.resumeReceipt, l.resumeErr
}

type recordingWorkMigration struct {
	prepareDirections  []WorkMigrationDirection
	openCalls          int
	retainedOverride   *RetainedWorkWorkspace
	retainedResults    []retainedOpenResult
	afterPrepare       func(context.Context, WorkPrepareRequest)
	afterVerify        func(context.Context, WorkVerifyRequest)
	afterCommit        func(context.Context, WorkCommitRequest)
	afterResume        func(context.Context, WorkResumeRequest)
	prepareErr         error
	verifyErr          error
	commitErr          error
	resumeErr          error
	mutateOpenRetained func(*RetainedWorkRequest)
}

// replayWorkMigration is a stateful provider-neutral substitute for the Work
// migration lifecycle. It models the only facts this contract test owns:
// exact logical-record reconciliation and stable replay receipts.
type replayWorkMigration struct {
	records      map[BindingIdentity]map[string]string
	prepared     map[string]WorkPrepared
	receipts     map[string]ParticipantReceipt
	prepareCalls map[string]int
	commitCalls  map[string]int
}

func newReplayWorkMigration() *replayWorkMigration {
	return &replayWorkMigration{
		records:      make(map[BindingIdentity]map[string]string),
		prepared:     make(map[string]WorkPrepared),
		receipts:     make(map[string]ParticipantReceipt),
		prepareCalls: make(map[string]int),
		commitCalls:  make(map[string]int),
	}
}

func (l *replayWorkMigration) setRecords(t *testing.T, descriptor Descriptor, records map[string]string) {
	t.Helper()
	identity, err := descriptor.Identity()
	if err != nil {
		t.Fatalf("Descriptor.Identity(): %v", err)
	}
	l.records[identity] = cloneLogicalRecords(records)
}

func (l *replayWorkMigration) recordsFor(t *testing.T, descriptor Descriptor) map[string]string {
	t.Helper()
	identity, err := descriptor.Identity()
	if err != nil {
		t.Fatalf("Descriptor.Identity(): %v", err)
	}
	return cloneLogicalRecords(l.records[identity])
}

func (l *replayWorkMigration) Prepare(_ context.Context, request WorkPrepareRequest) (WorkPrepared, error) {
	preparation, err := request.PreparationIdentity()
	if err != nil {
		return WorkPrepared{}, err
	}
	key := replayPreparationKey(preparation)
	l.prepareCalls[key]++
	if prepared, found := l.prepared[key]; found {
		return prepared.Clone(), nil
	}
	sourceIdentity, err := request.Source.Identity()
	if err != nil {
		return WorkPrepared{}, err
	}
	destinationIdentity, err := request.Destination.Identity()
	if err != nil {
		return WorkPrepared{}, err
	}
	l.records[destinationIdentity] = cloneLogicalRecords(l.records[sourceIdentity])
	prepared := WorkPrepared{
		Version:            1,
		Attempt:            request.Attempt,
		Generation:         request.Generation,
		Participant:        request.Participant.Clone(),
		DescriptorIdentity: destinationIdentity,
		Preparation:        preparation.Clone(),
		Receipt:            "prepared:" + key,
	}
	l.prepared[key] = prepared.Clone()
	return prepared, nil
}

func (l *replayWorkMigration) Verify(_ context.Context, request WorkVerifyRequest) (WorkProof, error) {
	return WorkProof{
		Version:            1,
		Attempt:            request.Prepared.Attempt,
		Generation:         request.Prepared.Generation,
		Participant:        request.Prepared.Participant.Clone(),
		DescriptorIdentity: request.Prepared.DescriptorIdentity,
		Preparation:        request.Prepared.Preparation.Clone(),
		PreparedReceipt:    request.Prepared.Receipt,
		Witness:            "replay-witness",
	}, nil
}

func (l *replayWorkMigration) Commit(_ context.Context, request WorkCommitRequest) (ParticipantReceipt, error) {
	key := replayPreparationKey(request.Prepared.Preparation)
	l.commitCalls[key]++
	if receipt, found := l.receipts[key]; found {
		return receipt.Clone(), nil
	}
	receipt := ParticipantReceipt{
		Version:            1,
		Kind:               ParticipantReceiptWorkMigration,
		Attempt:            request.Decision.Attempt,
		Generation:         request.Decision.Generation,
		Participant:        request.Participant.Key(),
		DescriptorIdentity: request.Prepared.DescriptorIdentity,
		ReceiptID:          "committed:" + key,
		Preparation:        request.Prepared.Preparation.Clone(),
		PreparedReceipt:    request.Prepared.Receipt,
	}
	l.receipts[key] = receipt.Clone()
	return receipt, nil
}

func (l *replayWorkMigration) Resume(_ context.Context, request WorkResumeRequest) (WorkProgress, error) {
	preparation, err := request.Prepare.PreparationIdentity()
	if err != nil {
		return WorkProgress{}, err
	}
	progress := WorkProgress{
		Version:     1,
		Attempt:     request.Prepare.Attempt,
		Generation:  request.Prepare.Generation,
		Participant: request.Prepare.Participant.Clone(),
		Preparation: preparation.Clone(),
	}
	if receipt, found := l.receipts[replayPreparationKey(preparation)]; found {
		cloned := receipt.Clone()
		progress.Complete = true
		progress.Receipt = &cloned
	}
	return progress, nil
}

func (l *replayWorkMigration) OpenRetained(context.Context, RetainedWorkRequest) (RetainedWorkWorkspace, error) {
	return RetainedWorkWorkspace{}, errors.New("not used")
}

func replayWorkRequest(t *testing.T, source, destination Descriptor, generation Generation, direction WorkMigrationDirection) WorkPrepareRequest {
	t.Helper()
	participant, err := NewWorkWorkspaceParticipant(source.Provider, source.Components[0].ID, source.Components[0].PhysicalIdentity, []WorkWorkspaceMember{{
		Scope:            HQScope(),
		Prefix:           "hq",
		ConfigContext:    testConfigDigest("replay-work-config"),
		ConfigOrder:      0,
		Provider:         source.Provider,
		Component:        source.Components[0].ID,
		PhysicalIdentity: source.Components[0].PhysicalIdentity,
	}})
	if err != nil {
		t.Fatalf("NewWorkWorkspaceParticipant(): %v", err)
	}
	return WorkPrepareRequest{
		Attempt:          AttemptID(fmt.Sprintf("replay-%d-%d", generation, direction)),
		Generation:       generation,
		Direction:        direction,
		Participant:      participant,
		Source:           source.Clone(),
		Destination:      destination.Clone(),
		SourceFence:      fenceForDescriptor(t, source, FenceRoleSource, generation),
		DestinationFence: fenceForDescriptor(t, destination, FenceRolePopulatedDestination, generation),
		WitnessVersion:   "semantic-v1",
		ConfigDigest:     testConfigDigest("replay-work-config"),
	}
}

func replayPreparationKey(preparation WorkPreparationIdentity) string {
	return fmt.Sprintf("%s\x00%d\x00%d\x00%s\x00%s\x00%s\x00%s\x00%s", preparation.Attempt, preparation.Generation, preparation.Direction, preparation.Participant.Key(), preparation.SourceIdentity, preparation.DestinationIdentity, preparation.WitnessVersion, preparation.ConfigDigest)
}

func workPreparedEqual(left, right WorkPrepared) bool {
	return left.Version == right.Version && left.Attempt == right.Attempt && left.Generation == right.Generation && left.Participant.Equal(right.Participant) && left.DescriptorIdentity == right.DescriptorIdentity && left.Preparation.Equal(right.Preparation) && left.Receipt == right.Receipt
}

func logicalRecordsEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, leftValue := range left {
		if rightValue, found := right[key]; !found || leftValue != rightValue {
			return false
		}
	}
	return true
}

func cloneLogicalRecords(records map[string]string) map[string]string {
	cloned := make(map[string]string, len(records))
	for key, value := range records {
		cloned[key] = value
	}
	return cloned
}

type retainedOpenResult struct {
	workspace RetainedWorkWorkspace
	err       error
}

type scriptedWorkMigration struct {
	prepared WorkPrepared
	proof    WorkProof
	receipt  ParticipantReceipt
	progress WorkProgress
}

type mutatingPrepareWorkMigration struct {
	scriptedWorkMigration
}

func (l *mutatingPrepareWorkMigration) Prepare(_ context.Context, request WorkPrepareRequest) (WorkPrepared, error) {
	request.Participant.Members[0].Prefix = "provider-mutated"
	return l.prepared, nil
}

type directOpenedBinding struct {
	descriptor   Descriptor
	capabilities ClassCapabilities
	work         WorkTopology
	workOK       bool
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
	closeCalls   int
	closeErr     error
}

func (b *directOpenedBinding) Descriptor() Descriptor { return b.descriptor.Clone() }

func (b *directOpenedBinding) Capabilities() ClassCapabilities { return b.capabilities }

func (b *directOpenedBinding) Work() (WorkTopology, bool) { return b.work, b.workOK }

func (b *directOpenedBinding) Graph() (GraphStore, bool) { return b.graph, b.graphOK }

func (b *directOpenedBinding) Sessions() (SessionsStore, bool) { return b.sessions, b.sessionsOK }

func (b *directOpenedBinding) Messaging() (MessagingFrontDoorBinder, bool) {
	return b.messaging, b.messagingOK
}

func (b *directOpenedBinding) Orders() (OrdersStore, bool) { return b.orders, b.ordersOK }

func (b *directOpenedBinding) Nudges() (NudgeFrontDoors, bool) { return b.nudges, b.nudgesOK }

func (b *directOpenedBinding) Close() error {
	b.closeCalls++
	return b.closeErr
}

func (l *scriptedWorkMigration) Prepare(context.Context, WorkPrepareRequest) (WorkPrepared, error) {
	return l.prepared, nil
}

func (l *scriptedWorkMigration) Verify(context.Context, WorkVerifyRequest) (WorkProof, error) {
	return l.proof, nil
}

func (l *scriptedWorkMigration) Commit(context.Context, WorkCommitRequest) (ParticipantReceipt, error) {
	return l.receipt, nil
}

func (l *scriptedWorkMigration) Resume(context.Context, WorkResumeRequest) (WorkProgress, error) {
	return l.progress, nil
}

func (l *scriptedWorkMigration) OpenRetained(context.Context, RetainedWorkRequest) (RetainedWorkWorkspace, error) {
	return RetainedWorkWorkspace{}, errors.New("not used")
}

func (l *recordingWorkMigration) Prepare(_ context.Context, request WorkPrepareRequest) (WorkPrepared, error) {
	l.prepareDirections = append(l.prepareDirections, request.Direction)
	destinationIdentity, err := request.Destination.Identity()
	if err != nil {
		return WorkPrepared{}, err
	}
	preparation, err := request.PreparationIdentity()
	if err != nil {
		return WorkPrepared{}, err
	}
	prepared := WorkPrepared{Version: 1, Attempt: request.Attempt, Generation: request.Generation, Participant: request.Participant.Clone(), DescriptorIdentity: destinationIdentity, Preparation: preparation, Receipt: "prepared"}
	if l.afterPrepare != nil {
		l.afterPrepare(context.Background(), request)
	}
	return prepared, l.prepareErr
}

func (l *recordingWorkMigration) Verify(_ context.Context, request WorkVerifyRequest) (WorkProof, error) {
	proof := WorkProof{Version: 1, Attempt: request.Prepare.Attempt, Generation: request.Prepare.Generation, Participant: request.Prepare.Participant.Clone(), DescriptorIdentity: request.Prepared.DescriptorIdentity, Preparation: request.Prepared.Preparation.Clone(), PreparedReceipt: request.Prepared.Receipt, Witness: "witness"}
	if l.afterVerify != nil {
		l.afterVerify(context.Background(), request)
	}
	return proof, l.verifyErr
}

func (l *recordingWorkMigration) Commit(ctx context.Context, request WorkCommitRequest) (ParticipantReceipt, error) {
	if l.afterCommit != nil {
		l.afterCommit(ctx, request)
	}
	return ParticipantReceipt{Version: 1, Kind: ParticipantReceiptWorkMigration, Attempt: request.Decision.Attempt, Generation: request.Decision.Generation, Participant: request.Participant.Key(), DescriptorIdentity: request.Proof.DescriptorIdentity, ReceiptID: "committed", Preparation: request.Prepared.Preparation.Clone(), PreparedReceipt: request.Prepared.Receipt}, l.commitErr
}

func (l *recordingWorkMigration) Resume(_ context.Context, request WorkResumeRequest) (WorkProgress, error) {
	preparation, err := request.Prepare.PreparationIdentity()
	if err != nil {
		return WorkProgress{}, err
	}
	progress := WorkProgress{Version: 1, Attempt: request.Prepare.Attempt, Generation: request.Prepare.Generation, Participant: request.Prepare.Participant.Clone(), Preparation: preparation}
	if l.afterResume != nil {
		l.afterResume(context.Background(), request)
	}
	return progress, l.resumeErr
}

func (l *recordingWorkMigration) OpenRetained(_ context.Context, request RetainedWorkRequest) (RetainedWorkWorkspace, error) {
	resultIndex := l.openCalls
	l.openCalls++
	if l.mutateOpenRetained != nil {
		l.mutateOpenRetained(&request)
	}
	if resultIndex < len(l.retainedResults) {
		result := l.retainedResults[resultIndex]
		return result.workspace, result.err
	}
	if l.retainedOverride != nil {
		return *l.retainedOverride, nil
	}
	return NewRetainedWorkWorkspaceWithViews(request.Participant, retainedPrefixViews(request.Participant), func() error { return nil })
}

func (l *recordingWorkMigration) ResolveRetainedWorkLifecycle(context.Context, ProviderID) (WorkMigrationLifecycle, error) {
	return l, nil
}
