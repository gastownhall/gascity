package storebinding

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/coordclass"
)

func TestInspectBindingIsMutationFreeAndLeavesIncompleteIdentityIncomplete(t *testing.T) {
	target := testFenceTarget(t)
	provider := &recordingProvider{inspection: Inspection{Target: target}}

	inspection, err := InspectBinding(context.Background(), provider, BindingSpec{Name: BindingName("infra"), Provider: ProviderID("builtin-test")})
	if err != nil {
		t.Fatalf("InspectBinding(): %v", err)
	}
	if inspection.Complete() {
		t.Fatal("incomplete inspection reported a descriptor")
	}
	if provider.inspectCalls != 1 || provider.openCalls != 0 || provider.mutations != 0 {
		t.Fatalf("calls = inspect:%d open:%d mutations:%d, want inspect:1 open:0 mutations:0", provider.inspectCalls, provider.openCalls, provider.mutations)
	}
}

func TestInspectBindingRejectsSecretMaterialWithoutEchoingIt(t *testing.T) {
	secret := "hunter2"
	target := testFenceTarget(t)
	provider := &recordingProvider{inspection: Inspection{Target: target, Descriptor: &Descriptor{
		Version:                 1,
		SemanticContractVersion: ContractVersion("v1"),
		Provider:                ProviderID("builtin-test"),
		ImplementationVersion:   "1",
		ConfigRefDigest:         testConfigDigest("secret-test-config"),
		Components: []ComponentDescriptor{{
			ID:               ComponentID("graph"),
			Locator:          ComponentLocator("db://user:" + secret + "@host/graph"),
			PhysicalIdentity: PhysicalIdentity("graph-file"),
			Classes:          classSet(t, coordclass.ClassGraph),
			Format:           FormatID("builtin-format"),
			SchemaVersion:    "1",
		}},
	}}}

	_, err := InspectBinding(context.Background(), provider, BindingSpec{Name: BindingName("infra"), Provider: ProviderID("builtin-test")})
	if !errors.Is(err, ErrSecretMaterial) {
		t.Fatalf("InspectBinding() error = %v, want ErrSecretMaterial", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("secret leaked in error: %q", err)
	}
}

func TestSecretValidationParsesURLsWithoutRejectingFileAtPaths(t *testing.T) {
	filePath := "file:///city/a@b/store.sqlite"
	if err := (BindingSpec{Name: BindingName("local"), Provider: ProviderID("builtin-test"), Path: filePath}).Validate(); err != nil {
		t.Fatalf("BindingSpec.Validate() rejected file path containing @: %v", err)
	}

	for _, value := range []string{
		"postgres://user@db.example.test/city",
		"postgres://db.example.test/city?access_token=hunter2", // oss-leak-allow:fixture — negative-path input proving the validator rejects credential-shaped material.
	} {
		err := (BindingSpec{Name: BindingName("remote"), Provider: ProviderID("builtin-test"), Path: value}).Validate()
		if !errors.Is(err, ErrSecretMaterial) {
			t.Fatalf("BindingSpec.Validate(%q) error = %v, want ErrSecretMaterial", value, err)
		}
		if strings.Contains(err.Error(), value) || strings.Contains(err.Error(), "hunter2") {
			t.Fatalf("secret validation error leaked value: %q", err)
		}
	}
}

func TestSecretValidationRejectsStructuredCredentialValuesWithoutEchoingThem(t *testing.T) {
	for _, test := range []struct {
		name   string
		value  string
		secret string
	}{
		{
			name:   "URL query key with mixed case",
			value:  "postgres://db.example.test/city?Access_Token=hunter2",
			secret: "hunter2", // oss-leak-allow:fixture — negative-path input proving the validator rejects credential-shaped material.
		},
		{
			name:   "spaced DSN assignment",
			value:  "host=db.example.test password = hunter2", // oss-leak-allow:fixture — negative-path input proving the validator rejects credential-shaped material.
			secret: "hunter2",                                 // oss-leak-allow:fixture — negative-path input proving the validator rejects credential-shaped material.
		},
		{
			name:   "nested JSON credential key",
			value:  `{"connection":{"api-key":"hunter2"}}`,
			secret: "hunter2", // oss-leak-allow:fixture — negative-path input proving the validator rejects credential-shaped material.
		},
		{
			name:   "decoded URL query assignment",
			value:  "postgres://db.example.test/city?options=password%3Dhunter2",
			secret: "hunter2", // oss-leak-allow:fixture — negative-path input proving the validator rejects credential-shaped material.
		},
		{
			name:   "nested encoded URL query assignment",
			value:  "postgres://db.example.test/city?options=password%253Dhunter2",
			secret: "hunter2", // oss-leak-allow:fixture — negative-path input proving the validator rejects credential-shaped material.
		},
		{
			name:   "encoded URL path assignment",
			value:  "file:///tmp/password%3Dhunter2",
			secret: "hunter2", // oss-leak-allow:fixture — negative-path input proving the validator rejects credential-shaped material.
		},
		{
			name:   "raw private key PEM",
			value:  "-----BEGIN PRIVATE KEY-----\nhunter2\n-----END PRIVATE KEY-----", // oss-leak-allow:fixture — negative-path input proving the validator rejects credential-shaped material.
			secret: "hunter2",                                                         // oss-leak-allow:fixture — negative-path input proving the validator rejects credential-shaped material.
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := (BindingSpec{Name: BindingName("remote"), Provider: ProviderID("builtin-test"), ConfigRef: ConfigRef(test.value)}).Validate()
			if !errors.Is(err, ErrSecretMaterial) {
				t.Fatalf("BindingSpec.Validate() error = %v, want ErrSecretMaterial", err)
			}
			if strings.Contains(err.Error(), test.secret) {
				t.Fatalf("secret validation error leaked value: %q", err)
			}
		})
	}
}

func TestSecretValidationURLPathAndOpaqueEquivalenceClasses(t *testing.T) {
	for _, value := range []string{
		"file:///city/token/store.sqlite",
		"file:///city/passwords/store.sqlite",
		"file:///city/source%20%3F%20%23%20%25%20spaces/graph/beads.sqlite",
	} {
		if err := (BindingSpec{Name: BindingName("local"), Provider: ProviderID("builtin-test"), Path: value}).Validate(); err != nil {
			t.Fatalf("BindingSpec.Validate(%q) rejected valid local locator: %v", value, err)
		}
	}

	for _, value := range []string{
		"postgres://db.example.test/city/password=hunter2",         // oss-leak-allow:fixture — negative-path input proving the validator rejects credential-shaped material.
		"postgres://db.example.test/city/segment;password=hunter2", // oss-leak-allow:fixture — negative-path input proving the validator rejects credential-shaped material.
		"postgres:user:hunter2@db.example.test/city",
		"file:/city/password%3Dhunter2%ZZ",
		"postgres://user:hunter2@db.example.test/city", // oss-leak-allow:fixture — negative-path input proving the validator rejects credential-shaped material.
		"file:///city/%ZZ",
	} {
		err := (BindingSpec{Name: BindingName("remote"), Provider: ProviderID("builtin-test"), Path: value}).Validate()
		if !errors.Is(err, ErrSecretMaterial) {
			t.Fatalf("BindingSpec.Validate(%q) error = %v, want ErrSecretMaterial", value, err)
		}
		if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), value) {
			t.Fatalf("secret validation error leaked input: %q", err)
		}
	}
}

func TestSecretValidationRejectsRootlessURLPathCredentialsAcrossBoundaries(t *testing.T) {
	const secret = "hunter2" // oss-leak-allow:fixture — negative-path input proving the validator rejects credential-shaped material.
	boundaries := []struct {
		name     string
		validate func(string) error
	}{
		{
			name: "binding spec path",
			validate: func(locator string) error {
				return (BindingSpec{Name: BindingName("remote"), Provider: ProviderID("builtin-test"), Path: locator}).Validate()
			},
		},
		{
			name: "descriptor component locator",
			validate: func(locator string) error {
				descriptor := testDescriptor(t, ProviderID("builtin-hardening"), PhysicalIdentity("rootless-descriptor"), coordclass.ClassGraph)
				descriptor.Components[0].Locator = ComponentLocator(locator)
				return descriptor.Validate()
			},
		},
		{
			name: "fence target component locator",
			validate: func(locator string) error {
				target := testFenceTarget(t)
				target.Components[0].Locator = ComponentLocator(locator)
				return target.Validate()
			},
		},
	}

	for _, locator := range []string{
		"postgres:city/password=" + secret,
		"postgres:city/password%3D" + secret,
		"postgres:city/password%3D" + secret + "%ZZ",
	} {
		for _, boundary := range boundaries {
			t.Run(boundary.name+"/"+locator, func(t *testing.T) {
				err := boundary.validate(locator)
				if !errors.Is(err, ErrSecretMaterial) {
					t.Fatalf("validation error = %v, want ErrSecretMaterial", err)
				}
				if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), locator) {
					t.Fatalf("secret validation error leaked input: %q", err)
				}
			})
		}
	}
}

func TestSecretValidationPreservesOrdinaryPathsAndBinaryReplayReceipts(t *testing.T) {
	boundaries := []struct {
		name     string
		validate func(string) error
	}{
		{
			name: "binding spec path",
			validate: func(locator string) error {
				return (BindingSpec{Name: BindingName("local"), Provider: ProviderID("builtin-test"), Path: locator}).Validate()
			},
		},
		{
			name: "descriptor component locator",
			validate: func(locator string) error {
				descriptor := testDescriptor(t, ProviderID("builtin-hardening"), PhysicalIdentity("ordinary-path-descriptor"), coordclass.ClassGraph)
				descriptor.Components[0].Locator = ComponentLocator(locator)
				return descriptor.Validate()
			},
		},
		{
			name: "fence target component locator",
			validate: func(locator string) error {
				target := testFenceTarget(t)
				target.Components[0].Locator = ComponentLocator(locator)
				return target.Validate()
			},
		},
	}

	for _, locator := range []string{
		"city/password/store.sqlite",
		"city/token/store.sqlite",
		"file:city/password/store.sqlite",
		"file:///city/token/store.sqlite",
		"file:///city/passwords/store.sqlite",
		"file:///city/source%20%3F%20%23%20%25%20spaces/graph/beads.sqlite",
	} {
		for _, boundary := range boundaries {
			t.Run(boundary.name+"/"+locator, func(t *testing.T) {
				if err := boundary.validate(locator); err != nil {
					t.Fatalf("validation rejected ordinary locator %q: %v", locator, err)
				}
			})
		}
	}

	receipt := ParticipantReceipt{
		Version:            1,
		Kind:               ParticipantReceiptBindingActivation,
		Attempt:            AttemptID("binary-replay"),
		Generation:         Generation(1),
		Participant:        "binding",
		DescriptorIdentity: testBindingIdentity("binary-replay"),
		ReceiptID:          "committed:binary-replay\x001\x001\x00hq\x00sha256:opaque",
	}
	if err := receipt.Validate(); err != nil {
		t.Fatalf("ParticipantReceipt.Validate() rejected binary replay receipt: %v", err)
	}
}

func TestInspectProviderFenceDoesNotExposeProviderFenceOrExportProjectionBypass(t *testing.T) {
	target := testFenceTarget(t)
	guard := testMigrationGuard(t, Generation(6))
	inner := &projectableFence{
		recordingFence: &recordingFence{target: target, role: FenceRoleSource, generation: Generation(6), held: true},
		projection:     FenceProjection("test-provider"),
	}
	outer, err := AcquireWriterFence(context.Background(), guard, projectableFenceAcquirer{fence: inner}, FenceRequest{
		Target:             target,
		GuardScope:         testMigrationGuardScope(t, guard),
		ExpectedGeneration: Generation(6),
		Components:         []ComponentID{"graph"},
		Role:               FenceRoleSource,
	})
	if err != nil {
		t.Fatalf("AcquireWriterFence(): %v", err)
	}

	if _, ok := outer.(interface {
		ExecuteProviderFenceOperation(context.Context, FenceProjection, ProviderFenceOperation) error
	}); ok {
		t.Fatal("managed outer fence exposes provider operation executor")
	}
	wrong := &recordingProviderFenceOperation{projection: FenceProjection("wrong-provider")}
	if err := InspectProviderFence(context.Background(), outer, wrong); !errors.Is(err, ErrInvalidFence) {
		t.Fatalf("InspectProviderFence(wrong provider) error = %v, want ErrInvalidFence", err)
	}
	if wrong.executed {
		t.Fatal("wrong provider operation executed")
	}

	operation := &recordingProviderFenceOperation{projection: inner.projection}
	if err := InspectProviderFence(context.Background(), outer, operation); err != nil {
		t.Fatalf("InspectProviderFence(): %v", err)
	}
	if !operation.executed {
		t.Fatal("provider operation did not execute")
	}
	if _, releasable := any(operation).(interface{ Release(context.Context) error }); releasable {
		t.Fatal("retained provider operation exposes fence release")
	}
	if operation.retained != nil {
		t.Fatalf("provider operation retained unexpected value of type %T", operation.retained)
	}

	if err := outer.Release(context.Background()); err != nil {
		t.Fatalf("managed Release(): %v", err)
	}
	if err := outer.Release(context.Background()); err != nil {
		t.Fatalf("idempotent managed Release(): %v", err)
	}
	if inner.releaseCalls != 1 {
		t.Fatalf("inner Release() calls = %d, want managed cleanup to call it once", inner.releaseCalls)
	}
	if err := guard.Release(); err != nil {
		t.Fatalf("guard.Release() after managed cleanup: %v", err)
	}
	if err := InspectProviderFence(context.Background(), outer, operation); !errors.Is(err, ErrFenceNotHeld) {
		t.Fatalf("InspectProviderFence(released fence) error = %v, want ErrFenceNotHeld", err)
	}
}

func TestInspectProviderFenceRevalidatesFenceLoss(t *testing.T) {
	target := testFenceTarget(t)
	guard := testMigrationGuard(t, Generation(6))
	inner := &projectableFence{
		recordingFence: &recordingFence{target: target, role: FenceRoleSource, generation: Generation(6), held: true},
		projection:     FenceProjection("test-provider"),
	}
	outer, err := AcquireWriterFence(context.Background(), guard, projectableFenceAcquirer{fence: inner}, FenceRequest{
		Target:             target,
		GuardScope:         testMigrationGuardScope(t, guard),
		ExpectedGeneration: Generation(6),
		Components:         []ComponentID{"graph"},
		Role:               FenceRoleSource,
	})
	if err != nil {
		t.Fatalf("AcquireWriterFence(): %v", err)
	}
	t.Cleanup(func() {
		_ = outer.Release(context.Background())
		_ = guard.Release()
	})
	inner.afterOperation = func() { inner.held = false }
	if err := InspectProviderFence(context.Background(), outer, &recordingProviderFenceOperation{projection: inner.projection}); !errors.Is(err, ErrFenceNotHeld) {
		t.Fatalf("InspectProviderFence(fence lost during operation) error = %v, want ErrFenceNotHeld", err)
	}
}

func TestInspectProviderFenceEvaluatesOperationOutsideManagedHeldScope(t *testing.T) {
	target := testFenceTarget(t)
	guard := testMigrationGuard(t, Generation(6))
	inner := &projectableFence{
		recordingFence: &recordingFence{target: target, role: FenceRoleSource, generation: Generation(6), held: true},
		projection:     FenceProjection("test-provider"),
	}
	outer, err := AcquireWriterFence(context.Background(), guard, projectableFenceAcquirer{fence: inner}, FenceRequest{
		Target:             target,
		GuardScope:         testMigrationGuardScope(t, guard),
		ExpectedGeneration: Generation(6),
		Components:         []ComponentID{"graph"},
		Role:               FenceRoleSource,
	})
	if err != nil {
		t.Fatalf("AcquireWriterFence(): %v", err)
	}
	operation := &releaseOnProjectionOperation{
		projection: inner.projection,
		fence:      outer,
	}
	if err := InspectProviderFence(context.Background(), outer, operation); !errors.Is(err, ErrFenceNotHeld) {
		t.Fatalf("InspectProviderFence(operation releasing outer) error = %v, want ErrFenceNotHeld", err)
	}
	if operation.releaseErr != nil {
		t.Fatalf("synchronous managed Release(): %v", operation.releaseErr)
	}
	if inner.releaseCalls != 1 {
		t.Fatalf("inner Release() calls = %d, want 1", inner.releaseCalls)
	}
	if err := guard.Release(); err != nil {
		t.Fatalf("guard.Release(): %v", err)
	}
}

func TestInspectProviderFenceSerializesManagedRelease(t *testing.T) {
	target := testFenceTarget(t)
	guard := testMigrationGuard(t, Generation(6))
	inner := &projectableFence{
		recordingFence: &recordingFence{target: target, role: FenceRoleSource, generation: Generation(6), held: true},
		projection:     FenceProjection("test-provider"),
	}
	outer, err := AcquireWriterFence(context.Background(), guard, projectableFenceAcquirer{fence: inner}, FenceRequest{
		Target:             target,
		GuardScope:         testMigrationGuardScope(t, guard),
		ExpectedGeneration: Generation(6),
		Components:         []ComponentID{"graph"},
		Role:               FenceRoleSource,
	})
	if err != nil {
		t.Fatalf("AcquireWriterFence(): %v", err)
	}
	operation := &recordingProviderFenceOperation{
		projection: inner.projection,
		started:    make(chan struct{}),
		finish:     make(chan struct{}),
	}
	inspectionDone := make(chan error, 1)
	go func() { inspectionDone <- InspectProviderFence(context.Background(), outer, operation) }()
	<-operation.started
	releaseStarted := make(chan struct{})
	releaseDone := make(chan error, 1)
	go func() {
		close(releaseStarted)
		releaseDone <- outer.Release(context.Background())
	}()
	<-releaseStarted
	close(operation.finish)
	if err := <-inspectionDone; err != nil && !errors.Is(err, ErrFenceNotHeld) {
		t.Fatalf("InspectProviderFence() during concurrent release: %v", err)
	}
	if err := <-releaseDone; err != nil {
		t.Fatalf("managed Release(): %v", err)
	}
	if inner.releaseCalls != 1 {
		t.Fatalf("inner Release() calls = %d, want managed cleanup to call it once", inner.releaseCalls)
	}
	if err := guard.Release(); err != nil {
		t.Fatalf("guard.Release() after managed cleanup: %v", err)
	}
	inner.orderMu.Lock()
	defer inner.orderMu.Unlock()
	if got := strings.Join(inner.order, ","); got != "operation-start,operation-end,release" {
		t.Fatalf("provider operation/release order = %q, want operation-start,operation-end,release", got)
	}
}

func TestInspectFencedRejectsAComponentThatMovedAfterInspection(t *testing.T) {
	target := testFenceTarget(t)
	fence := &recordingFence{target: target, role: FenceRoleSource, generation: Generation(4), held: true}
	provider := &recordingProvider{fencedDescriptor: descriptorForTarget(t, target, PhysicalIdentity("moved-identity"))}

	_, err := InspectFenced(context.Background(), provider, FencedInspectionRequest{
		Target:             target,
		Fence:              fence,
		ExpectedGeneration: Generation(4),
	})
	if !errors.Is(err, ErrFenceTargetMoved) {
		t.Fatalf("InspectFenced() error = %v, want ErrFenceTargetMoved", err)
	}
	if provider.openCalls != 0 {
		t.Fatalf("InspectFenced() opened binding %d times", provider.openCalls)
	}
}

func TestInspectFencedRejectsDescriptorWithUninspectedComponent(t *testing.T) {
	target := testFenceTarget(t)
	fence := &recordingFence{target: target, role: FenceRoleSource, generation: Generation(4), held: true}
	descriptor := descriptorForTarget(t, target, PhysicalIdentity("graph-identity"))
	descriptor.Capabilities.Sessions = ClassCapability{Available: true}
	descriptor.Components = append(descriptor.Components, ComponentDescriptor{
		ID:               ComponentID("sessions"),
		Locator:          ComponentLocator("file:/city/.gc/store/sessions.db"),
		PhysicalIdentity: PhysicalIdentity("sessions-identity"),
		Classes:          classSet(t, coordclass.ClassSessions),
		Format:           FormatID("builtin-format"),
		SchemaVersion:    "1",
		Marker:           MarkerState{Name: "none", Present: false},
	})
	if err := descriptor.Validate(); err != nil {
		t.Fatalf("extra descriptor Validate(): %v", err)
	}

	_, err := InspectFenced(context.Background(), &recordingProvider{fencedDescriptor: descriptor}, FencedInspectionRequest{
		Target:             target,
		Fence:              fence,
		ExpectedGeneration: Generation(4),
	})
	if !errors.Is(err, ErrFenceTargetMoved) {
		t.Fatalf("InspectFenced() error = %v, want ErrFenceTargetMoved", err)
	}
}

func TestAcquireWriterFenceRejectsFenceThatIsNotHeld(t *testing.T) {
	target := testFenceTarget(t)
	fence := &recordingFence{target: target, role: FenceRoleSource, generation: Generation(6), held: false}
	provider := &recordingProvider{fence: fence}
	guard := testMigrationGuard(t, Generation(6))

	_, err := AcquireWriterFence(context.Background(), guard, provider, FenceRequest{
		Target:             target,
		GuardScope:         testMigrationGuardScope(t, guard),
		ExpectedGeneration: Generation(6),
		Components:         []ComponentID{ComponentID("graph")},
		Role:               FenceRoleSource,
	})
	if !errors.Is(err, ErrFenceNotHeld) {
		t.Fatalf("AcquireWriterFence() error = %v, want ErrFenceNotHeld", err)
	}
	if fence.releaseCalls != 1 {
		t.Fatalf("AcquireWriterFence() released unheld fence %d times, want 1", fence.releaseCalls)
	}
	if err := guard.Release(); err != nil {
		t.Fatalf("guard Release() after rejected fence: %v", err)
	}
}

func TestAcquireWriterFenceRejectsMismatchedCoverageAndReleases(t *testing.T) {
	target := testFenceTarget(t)
	fence := &recordingFence{
		target:     target,
		role:       FenceRoleSource,
		generation: Generation(6),
		held:       true,
		covered:    []ComponentID{"other"},
	}
	guard := testMigrationGuard(t, Generation(6))
	_, err := AcquireWriterFence(context.Background(), guard, &recordingProvider{fence: fence}, FenceRequest{
		Target:             target,
		GuardScope:         testMigrationGuardScope(t, guard),
		ExpectedGeneration: Generation(6),
		Components:         []ComponentID{"graph"},
		Role:               FenceRoleSource,
	})
	if !errors.Is(err, ErrInvalidFence) {
		t.Fatalf("AcquireWriterFence() error = %v, want ErrInvalidFence", err)
	}
	if fence.releaseCalls != 1 {
		t.Fatalf("AcquireWriterFence() released mismatched fence %d times, want 1", fence.releaseCalls)
	}
	if err := guard.Release(); err != nil {
		t.Fatalf("guard Release() after rejected fence: %v", err)
	}
}

func TestAcquireWriterFenceTransfersGuardClaimToProviderFence(t *testing.T) {
	target := testFenceTarget(t)
	fence := &recordingFence{target: target, role: FenceRoleSource, generation: Generation(6), held: true}
	guard := testMigrationGuard(t, Generation(6))
	got, err := AcquireWriterFence(context.Background(), guard, &recordingProvider{fence: fence}, FenceRequest{
		Target:             target,
		GuardScope:         testMigrationGuardScope(t, guard),
		ExpectedGeneration: Generation(6),
		Components:         []ComponentID{"graph"},
		Role:               FenceRoleSource,
	})
	if err != nil {
		t.Fatalf("AcquireWriterFence(): %v", err)
	}
	if !fence.hasClaim || !fence.claim.Held() {
		t.Fatal("provider fence did not retain the live migration guard claim")
	}
	if err := guard.Release(); !errors.Is(err, ErrMigrationGuardClaimsHeld) {
		t.Fatalf("guard Release() before fence release error = %v, want ErrMigrationGuardClaimsHeld", err)
	}
	if err := got.Release(context.Background()); err != nil {
		t.Fatalf("fence Release(): %v", err)
	}
	if err := guard.Release(); err != nil {
		t.Fatalf("guard Release() after fence release: %v", err)
	}
}

func TestAcquireWriterFenceManagedReleaseRecoversClaimFromBrokenProvider(t *testing.T) {
	target := testFenceTarget(t)
	guard := testMigrationGuard(t, Generation(6))
	fence := &claimIgnoringFence{recordingFence: &recordingFence{target: target, role: FenceRoleSource, generation: Generation(6), held: true}}
	got, err := AcquireWriterFence(context.Background(), guard, &claimIgnoringFenceAcquirer{fence: fence}, FenceRequest{
		Target:             target,
		GuardScope:         testMigrationGuardScope(t, guard),
		ExpectedGeneration: Generation(6),
		Components:         []ComponentID{"graph"},
		Role:               FenceRoleSource,
	})
	if err != nil {
		t.Fatalf("AcquireWriterFence(): %v", err)
	}
	if err := guard.Release(); !errors.Is(err, ErrMigrationGuardClaimsHeld) {
		t.Fatalf("guard.Release() before managed fence release error = %v, want ErrMigrationGuardClaimsHeld", err)
	}
	if err := got.Release(context.Background()); err != nil {
		t.Fatalf("managed fence Release(): %v", err)
	}
	if fence.releaseCalls != 1 {
		t.Fatalf("provider fence release calls = %d, want 1", fence.releaseCalls)
	}
	if err := guard.Release(); err != nil {
		t.Fatalf("guard.Release() after managed fence release: %v", err)
	}
}

func TestAcquireWriterFenceManagedReleaseRetriesOnlyIncompleteStage(t *testing.T) {
	target := testFenceTarget(t)
	requestFor := func(t *testing.T, guard MigrationGuard) FenceRequest {
		t.Helper()
		return FenceRequest{
			Target:             target,
			GuardScope:         testMigrationGuardScope(t, guard),
			ExpectedGeneration: Generation(6),
			Components:         []ComponentID{"graph"},
			Role:               FenceRoleSource,
		}
	}

	t.Run("inner release failure retries inner before claim", func(t *testing.T) {
		guard := testMigrationGuard(t, Generation(6))
		innerErr := errors.New("inner writer fence release failed")
		fence := &claimIgnoringFence{
			recordingFence: &recordingFence{target: target, role: FenceRoleSource, generation: Generation(6), held: true},
			releaseErrors:  []error{innerErr, nil},
		}
		got, err := AcquireWriterFence(context.Background(), guard, &claimIgnoringFenceAcquirer{fence: fence}, requestFor(t, guard))
		if err != nil {
			t.Fatalf("AcquireWriterFence(): %v", err)
		}
		if err := got.Release(context.Background()); !errors.Is(err, innerErr) {
			t.Fatalf("first managed Release() error = %v, want inner error", err)
		}
		if err := guard.Release(); !errors.Is(err, ErrMigrationGuardClaimsHeld) {
			t.Fatalf("guard.Release() after failed inner stage error = %v, want ErrMigrationGuardClaimsHeld", err)
		}
		if err := got.Release(context.Background()); err != nil {
			t.Fatalf("second managed Release(): %v", err)
		}
		if fence.releaseCalls != 2 {
			t.Fatalf("provider fence release calls = %d, want 2", fence.releaseCalls)
		}
		if err := got.Release(context.Background()); err != nil {
			t.Fatalf("idempotent managed Release(): %v", err)
		}
		if fence.releaseCalls != 2 {
			t.Fatalf("provider fence release calls after success = %d, want 2", fence.releaseCalls)
		}
		if err := guard.Release(); err != nil {
			t.Fatalf("guard.Release() after successful retry: %v", err)
		}
	})

	t.Run("claim release failure does not repeat completed inner release", func(t *testing.T) {
		guard := testMigrationGuard(t, Generation(6))
		fence := &claimIgnoringFence{recordingFence: &recordingFence{target: target, role: FenceRoleSource, generation: Generation(6), held: true}}
		got, err := AcquireWriterFence(context.Background(), guard, &claimIgnoringFenceAcquirer{fence: fence}, requestFor(t, guard))
		if err != nil {
			t.Fatalf("AcquireWriterFence(): %v", err)
		}

		guard.state.mu.Lock()
		guard.state.claims = 0
		guard.state.mu.Unlock()
		if err := got.Release(context.Background()); !errors.Is(err, ErrInvalidMigrationGuardClaim) {
			t.Fatalf("first managed Release() error = %v, want ErrInvalidMigrationGuardClaim", err)
		}
		if fence.releaseCalls != 1 {
			t.Fatalf("provider fence release calls after claim failure = %d, want 1", fence.releaseCalls)
		}

		guard.state.mu.Lock()
		guard.state.claims = 1
		guard.state.mu.Unlock()
		if err := got.Release(context.Background()); err != nil {
			t.Fatalf("managed Release() claim retry: %v", err)
		}
		if fence.releaseCalls != 1 {
			t.Fatalf("provider fence release calls after claim retry = %d, want completed inner stage skipped", fence.releaseCalls)
		}
		if err := guard.Release(); err != nil {
			t.Fatalf("guard.Release() after claim retry: %v", err)
		}
	})
}

func TestAcquireWriterFenceRetainsClaimForRetryablePartialFenceCleanup(t *testing.T) {
	target := testFenceTarget(t)
	guard := testMigrationGuard(t, Generation(6))
	acquireErr := errors.New("partial fence acquisition")
	firstReleaseErr := errors.New("first inner fence release")
	secondReleaseErr := errors.New("second inner fence release")
	fence := &recordingFence{
		target:        target,
		role:          FenceRoleSource,
		generation:    Generation(6),
		held:          true,
		releaseErrors: []error{firstReleaseErr, secondReleaseErr, nil},
	}
	acquirer := &partialFenceAcquirer{fence: fence, err: acquireErr}

	_, err := AcquireWriterFence(context.Background(), guard, acquirer, FenceRequest{
		Target:             target,
		GuardScope:         testMigrationGuardScope(t, guard),
		ExpectedGeneration: Generation(6),
		Components:         []ComponentID{"graph"},
		Role:               FenceRoleSource,
	})
	if !errors.Is(err, acquireErr) || !errors.Is(err, firstReleaseErr) {
		t.Fatalf("AcquireWriterFence() error = %v, want acquisition and first-release causes", err)
	}
	var cleanup *RejectedWriterFenceCleanupError
	if !errors.As(err, &cleanup) {
		t.Fatalf("AcquireWriterFence() error = %T, want *RejectedWriterFenceCleanupError", err)
	}
	if held, heldErr := fence.Held(context.Background()); heldErr != nil || held {
		t.Fatalf("partial fence Held() = %t, %v; want false, nil", held, heldErr)
	}
	if err := guard.Release(); !errors.Is(err, ErrMigrationGuardClaimsHeld) {
		t.Fatalf("guard.Release() while cleanup pending error = %v, want ErrMigrationGuardClaimsHeld", err)
	}
	if err := cleanup.RetryCleanup(context.Background()); !errors.Is(err, secondReleaseErr) {
		t.Fatalf("RetryCleanup() first retry error = %v, want second inner-release error", err)
	}
	if err := guard.Release(); !errors.Is(err, ErrMigrationGuardClaimsHeld) {
		t.Fatalf("guard.Release() after failed retry error = %v, want ErrMigrationGuardClaimsHeld", err)
	}
	if err := cleanup.RetryCleanup(context.Background()); err != nil {
		t.Fatalf("RetryCleanup() successful retry: %v", err)
	}
	if fence.releaseCalls != 3 {
		t.Fatalf("fence release calls = %d, want initial attempt plus two retries", fence.releaseCalls)
	}
	if err := cleanup.RetryCleanup(context.Background()); err != nil {
		t.Fatalf("RetryCleanup() after success: %v", err)
	}
	if fence.releaseCalls != 3 {
		t.Fatalf("fence release calls after successful retry = %d, want 3", fence.releaseCalls)
	}
	if err := guard.Release(); err != nil {
		t.Fatalf("guard.Release() after inner cleanup: %v", err)
	}
}

func TestAcquireWriterFenceRejectsWrongGuardGenerationBeforeProviderMutation(t *testing.T) {
	target := testFenceTarget(t)
	provider := &recordingProvider{fence: &recordingFence{target: target, role: FenceRoleSource, generation: Generation(6), held: true}}
	guard := testMigrationGuard(t, Generation(5))
	_, err := AcquireWriterFence(context.Background(), guard, provider, FenceRequest{
		Target:             target,
		GuardScope:         testMigrationGuardScope(t, guard),
		ExpectedGeneration: Generation(6),
		Components:         []ComponentID{"graph"},
		Role:               FenceRoleSource,
	})
	if !errors.Is(err, ErrInvalidFence) {
		t.Fatalf("AcquireWriterFence() error = %v, want ErrInvalidFence", err)
	}
	if provider.mutations != 0 {
		t.Fatalf("AcquireWriterFence() called provider %d times with wrong guard generation", provider.mutations)
	}
	if err := guard.Release(); err != nil {
		t.Fatalf("guard Release() after rejected generation: %v", err)
	}
}

func TestAcquireWriterFenceUsesNarrowAcquirerBoundary(t *testing.T) {
	target := testFenceTarget(t)
	guard := testMigrationGuard(t, Generation(6))
	acquirer := &fenceOnlyAcquirer{fence: &recordingFence{target: target, role: FenceRoleSource, generation: Generation(6), held: true}}

	got, err := AcquireWriterFence(context.Background(), guard, acquirer, FenceRequest{
		Target:             target,
		GuardScope:         testMigrationGuardScope(t, guard),
		ExpectedGeneration: Generation(6),
		Components:         []ComponentID{"graph"},
		Role:               FenceRoleSource,
	})
	if err != nil {
		t.Fatalf("AcquireWriterFence(): %v", err)
	}
	if acquirer.calls != 1 {
		t.Fatalf("fence-only acquirer calls = %d, want 1", acquirer.calls)
	}
	if err := got.Release(context.Background()); err != nil {
		t.Fatalf("fence Release(): %v", err)
	}
	if err := guard.Release(); err != nil {
		t.Fatalf("guard Release(): %v", err)
	}
}

func TestAcquireWriterFenceRejectsNilNarrowAcquirer(t *testing.T) {
	target := testFenceTarget(t)
	guard := testMigrationGuard(t, Generation(6))
	var acquirer *fenceOnlyAcquirer

	_, err := AcquireWriterFence(context.Background(), guard, acquirer, FenceRequest{
		Target:             target,
		GuardScope:         testMigrationGuardScope(t, guard),
		ExpectedGeneration: Generation(6),
		Components:         []ComponentID{"graph"},
		Role:               FenceRoleSource,
	})
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("AcquireWriterFence() error = %v, want ErrProviderUnavailable", err)
	}
	if err := guard.Release(); err != nil {
		t.Fatalf("guard Release(): %v", err)
	}
}

func TestFencedInspectionRejectsPartialFenceCoverage(t *testing.T) {
	target, err := NewFenceTarget(ProviderID("builtin-test"), classSet(t, coordclass.ClassGraph, coordclass.ClassSessions), []FenceComponentTarget{
		{ID: "graph", Locator: "file:/city/graph", PhysicalIdentity: "graph-identity", Classes: classSet(t, coordclass.ClassGraph)},
		{ID: "sessions", Locator: "file:/city/sessions", PhysicalIdentity: "sessions-identity", Classes: classSet(t, coordclass.ClassSessions)},
	})
	if err != nil {
		t.Fatalf("NewFenceTarget(): %v", err)
	}
	fence := &recordingFence{target: target, role: FenceRoleSource, generation: Generation(3), held: true, covered: []ComponentID{"graph"}}
	err = (FencedInspectionRequest{Target: target, Fence: fence, ExpectedGeneration: Generation(3)}).Validate(context.Background())
	if !errors.Is(err, ErrInvalidFence) {
		t.Fatalf("FencedInspectionRequest.Validate() error = %v, want ErrInvalidFence", err)
	}
}

func TestAcquireWriterFenceRejectsDifferentCityGuardScopeBeforeProviderMutation(t *testing.T) {
	target := testFenceTarget(t)
	guard := testMigrationGuard(t, Generation(6))
	otherScope, err := NewMigrationGuardScope(testMigrationGuardDirectory(t))
	if err != nil {
		t.Fatalf("NewMigrationGuardScope(): %v", err)
	}
	provider := &recordingProvider{fence: &recordingFence{target: target, role: FenceRoleSource, generation: Generation(6), held: true}}

	_, err = AcquireWriterFence(context.Background(), guard, provider, FenceRequest{
		Target:             target,
		GuardScope:         otherScope,
		ExpectedGeneration: Generation(6),
		Components:         []ComponentID{"graph"},
		Role:               FenceRoleSource,
	})
	if !errors.Is(err, ErrMigrationGuardScopeMismatch) {
		t.Fatalf("AcquireWriterFence() error = %v, want ErrMigrationGuardScopeMismatch", err)
	}
	if provider.mutations != 0 {
		t.Fatalf("AcquireWriterFence() called provider %d times for a wrong city guard scope", provider.mutations)
	}
}

func testFenceTarget(t *testing.T) FenceTarget {
	t.Helper()
	target, err := NewFenceTarget(ProviderID("builtin-test"), classSet(t, coordclass.ClassGraph), []FenceComponentTarget{{
		ID:               ComponentID("graph"),
		Locator:          ComponentLocator("file:/city/.gc/store/graph/beads.sqlite"),
		PhysicalIdentity: PhysicalIdentity("graph-identity"),
		Classes:          classSet(t, coordclass.ClassGraph),
	}})
	if err != nil {
		t.Fatalf("NewFenceTarget(): %v", err)
	}
	return target
}

func testMigrationGuard(t *testing.T, generation Generation) MigrationGuard {
	t.Helper()
	guard, err := AcquireMigrationGuard(context.Background(), testMigrationGuardDirectory(t), generation)
	if err != nil {
		t.Fatalf("AcquireMigrationGuard(): %v", err)
	}
	return guard
}

func testMigrationGuardScope(t *testing.T, guard MigrationGuard) MigrationGuardScope {
	t.Helper()
	claim, err := guard.claim(context.Background())
	if err != nil {
		t.Fatalf("guard.Claim(): %v", err)
	}
	identity, err := claim.Identity()
	if err != nil {
		t.Fatalf("claim.Identity(): %v", err)
	}
	if err := claim.Release(); err != nil {
		t.Fatalf("claim.Release(): %v", err)
	}
	scope, err := NewMigrationGuardScope(identity.Directory())
	if err != nil {
		t.Fatalf("NewMigrationGuardScope(): %v", err)
	}
	return scope
}

func descriptorForTarget(t *testing.T, target FenceTarget, identity PhysicalIdentity) Descriptor {
	t.Helper()
	descriptor := Descriptor{
		Version:                 1,
		SemanticContractVersion: ContractVersion("v1"),
		Provider:                target.Provider,
		ImplementationVersion:   "1",
		ConfigRefDigest:         testConfigDigest("target-test-config"),
		Capabilities:            ClassCapabilities{Graph: ClassCapability{Available: true}},
		Components: []ComponentDescriptor{{
			ID:               ComponentID("graph"),
			Locator:          ComponentLocator("file:/city/.gc/store/graph/beads.sqlite"),
			PhysicalIdentity: identity,
			Classes:          classSet(t, coordclass.ClassGraph),
			Format:           FormatID("builtin-format"),
			SchemaVersion:    "1",
			Marker:           MarkerState{Name: "none", Present: false},
		}},
	}
	if err := descriptor.Validate(); err != nil {
		t.Fatalf("descriptor.Validate(): %v", err)
	}
	return descriptor
}

func classSet(t *testing.T, classes ...coordclass.Class) ClassSet {
	t.Helper()
	set, err := NewClassSet(classes...)
	if err != nil {
		t.Fatalf("NewClassSet(): %v", err)
	}
	return set
}

type recordingProvider struct {
	inspection       Inspection
	fencedDescriptor Descriptor
	fencedErr        error
	fence            WriterFence
	opened           OpenedBinding
	openErr          error
	mutateFence      func(*FenceRequest)
	mutateFenced     func(*FencedInspectionRequest)
	mutateOpen       func(*OpenRequest)
	inspectCalls     int
	openCalls        int
	mutations        int
}

type fenceOnlyAcquirer struct {
	fence WriterFence
	calls int
}

type partialFenceAcquirer struct {
	fence WriterFence
	err   error
	calls int
}

type claimIgnoringFenceAcquirer struct {
	fence *claimIgnoringFence
}

func (a *claimIgnoringFenceAcquirer) AcquireFence(context.Context, MigrationGuardClaim, FenceRequest) (WriterFence, error) {
	return a.fence, nil
}

type claimIgnoringFence struct {
	*recordingFence
	releaseErrors []error
}

func (f *claimIgnoringFence) Release(context.Context) error {
	f.releaseCalls++
	if len(f.releaseErrors) != 0 {
		err := f.releaseErrors[0]
		f.releaseErrors = f.releaseErrors[1:]
		if err != nil {
			return err
		}
	}
	f.held = false
	return nil
}

func (a *fenceOnlyAcquirer) AcquireFence(_ context.Context, claim MigrationGuardClaim, _ FenceRequest) (WriterFence, error) {
	a.calls++
	if fence, ok := a.fence.(*recordingFence); ok {
		fence.claim = claim
		fence.hasClaim = true
	}
	return a.fence, nil
}

func (a *partialFenceAcquirer) AcquireFence(_ context.Context, claim MigrationGuardClaim, _ FenceRequest) (WriterFence, error) {
	a.calls++
	if fence, ok := a.fence.(*recordingFence); ok {
		fence.claim = claim
		fence.hasClaim = true
	}
	return a.fence, a.err
}

func (p *recordingProvider) Inspect(context.Context, BindingSpec) (Inspection, error) {
	p.inspectCalls++
	return p.inspection, nil
}

func (p *recordingProvider) AcquireFence(_ context.Context, claim MigrationGuardClaim, request FenceRequest) (WriterFence, error) {
	p.mutations++
	if p.mutateFence != nil {
		p.mutateFence(&request)
	}
	if p.fence != nil {
		if fence, ok := p.fence.(*recordingFence); ok {
			fence.claim = claim
			fence.hasClaim = true
		}
		return p.fence, nil
	}
	return nil, errors.New("not implemented")
}

func (p *recordingProvider) InspectFenced(_ context.Context, request FencedInspectionRequest) (Descriptor, error) {
	if p.mutateFenced != nil {
		p.mutateFenced(&request)
	}
	return p.fencedDescriptor, p.fencedErr
}

func (p *recordingProvider) RetainedGuards() (RetainedGuardLifecycle, bool) { return nil, false }

func (p *recordingProvider) BindingMigration() (BindingMigrationLifecycle, bool) { return nil, false }

func (p *recordingProvider) WorkMigration() (WorkMigrationLifecycle, bool) { return nil, false }

func (p *recordingProvider) Open(_ context.Context, request OpenRequest) (OpenedBinding, error) {
	p.openCalls++
	if p.mutateOpen != nil {
		p.mutateOpen(&request)
	}
	if !isNilInterface(p.opened) {
		return p.opened, p.openErr
	}
	if p.openErr != nil {
		return nil, p.openErr
	}
	return nil, errors.New("not implemented")
}

type recordingFence struct {
	target        FenceTarget
	role          FenceRole
	generation    Generation
	held          bool
	covered       []ComponentID
	releaseCalls  int
	releaseErrors []error
	claim         MigrationGuardClaim
	hasClaim      bool
}

func (f *recordingFence) Target() FenceTarget    { return f.target }
func (f *recordingFence) Role() FenceRole        { return f.role }
func (f *recordingFence) Generation() Generation { return f.generation }
func (f *recordingFence) CoveredComponents() []ComponentID {
	if f.covered != nil {
		return append([]ComponentID(nil), f.covered...)
	}
	components := make([]ComponentID, len(f.target.Components))
	for index, component := range f.target.Components {
		components[index] = component.ID
	}
	return components
}

func (f *recordingFence) Held(context.Context) (bool, error) {
	return f.held && (!f.hasClaim || f.claim.Held()), nil
}

func (f *recordingFence) Release(context.Context) error {
	f.releaseCalls++
	f.held = false
	if len(f.releaseErrors) != 0 {
		err := f.releaseErrors[0]
		f.releaseErrors = f.releaseErrors[1:]
		if err != nil {
			return err
		}
	}
	if f.hasClaim {
		return f.claim.Release()
	}
	return nil
}

type projectableFenceAcquirer struct{ fence *projectableFence }

func (a projectableFenceAcquirer) AcquireFence(_ context.Context, claim MigrationGuardClaim, _ FenceRequest) (WriterFence, error) {
	a.fence.claim = claim
	a.fence.hasClaim = true
	return a.fence, nil
}

type projectableFence struct {
	*recordingFence
	projection     FenceProjection
	afterOperation func()
	mu             sync.Mutex
	orderMu        sync.Mutex
	order          []string
}

func (f *projectableFence) ExecuteProviderFenceOperation(_ context.Context, projection FenceProjection, operation ProviderFenceOperation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if projection != f.projection {
		return ErrInvalidFence
	}
	held, err := f.Held(context.Background())
	if err != nil {
		return err
	}
	if !held {
		return ErrFenceNotHeld
	}
	recording, ok := operation.(*recordingProviderFenceOperation)
	if !ok {
		return ErrInvalidFence
	}
	if recording.started != nil {
		f.recordOrder("operation-start")
		close(recording.started)
		<-recording.finish
		f.recordOrder("operation-end")
	}
	recording.executed = true
	if f.afterOperation != nil {
		f.afterOperation()
	}
	return nil
}

func (f *projectableFence) Release(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordOrder("release")
	return f.recordingFence.Release(ctx)
}

func (f *projectableFence) recordOrder(value string) {
	f.orderMu.Lock()
	defer f.orderMu.Unlock()
	f.order = append(f.order, value)
}

type recordingProviderFenceOperation struct {
	projection FenceProjection
	executed   bool
	retained   any
	started    chan struct{}
	finish     chan struct{}
}

func (o *recordingProviderFenceOperation) FenceProjection() FenceProjection {
	if o == nil {
		return ""
	}
	return o.projection
}

type releaseOnProjectionOperation struct {
	projection FenceProjection
	fence      WriterFence
	releaseErr error
}

func (o *releaseOnProjectionOperation) FenceProjection() FenceProjection {
	o.releaseErr = o.fence.Release(context.Background())
	return o.projection
}
