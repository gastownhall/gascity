package storebinding

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/coordclass"
)

type cancellationAwareFence struct {
	*recordingFence
	canceledErr error
}

func (f *cancellationAwareFence) Held(ctx context.Context) (bool, error) {
	if ctx.Err() != nil {
		return false, f.canceledErr
	}
	return f.recordingFence.Held(ctx)
}

func TestAcquireWriterFencePassesDetachedRequestAndRetainsBaseline(t *testing.T) {
	guard := testMigrationGuard(t, Generation(6))
	t.Cleanup(func() {
		if err := guard.Release(); err != nil {
			t.Errorf("guard.Release(): %v", err)
		}
	})
	target := testFenceTarget(t)
	fence := &recordingFence{target: target, role: FenceRoleSource, generation: Generation(6), held: true}
	provider := &recordingProvider{
		fence: fence,
		mutateFence: func(request *FenceRequest) {
			request.Target.Components[0].PhysicalIdentity = PhysicalIdentity("provider-mutated-target")
			request.Components[0] = ComponentID("provider-mutated-component")
		},
	}
	request := FenceRequest{
		Target:             target,
		GuardScope:         testMigrationGuardScope(t, guard),
		ExpectedGeneration: Generation(6),
		Components:         []ComponentID{ComponentID("graph")},
		Role:               FenceRoleSource,
	}

	acquired, err := AcquireWriterFence(context.Background(), guard, provider, request)
	if err != nil {
		t.Fatalf("AcquireWriterFence(): %v", err)
	}
	if err := acquired.Release(context.Background()); err != nil {
		t.Fatalf("acquired.Release(): %v", err)
	}
	if request.Target.Components[0].PhysicalIdentity != PhysicalIdentity("graph-identity") || request.Components[0] != ComponentID("graph") {
		t.Fatalf("provider mutation escaped request boundary: %#v", request)
	}
}

func TestInspectFencedUsesDetachedRequestAndRejectsPostCallFenceLoss(t *testing.T) {
	t.Run("provider input mutation does not change accepted target", func(t *testing.T) {
		target := testFenceTarget(t)
		fence := &recordingFence{target: target, role: FenceRoleSource, generation: Generation(4), held: true}
		provider := &recordingProvider{
			fencedDescriptor: descriptorForTarget(t, target, PhysicalIdentity("graph-identity")),
			mutateFenced: func(request *FencedInspectionRequest) {
				request.Target.Components[0].PhysicalIdentity = PhysicalIdentity("provider-mutated-target")
			},
		}
		request := FencedInspectionRequest{Target: target, Fence: fence, ExpectedGeneration: Generation(4)}

		if _, err := InspectFenced(context.Background(), provider, request); err != nil {
			t.Fatalf("InspectFenced(): %v", err)
		}
		if request.Target.Components[0].PhysicalIdentity != PhysicalIdentity("graph-identity") {
			t.Fatalf("provider mutation escaped request boundary: %#v", request.Target)
		}
	})

	t.Run("provider cannot release fence before returning descriptor", func(t *testing.T) {
		target := testFenceTarget(t)
		fence := &recordingFence{target: target, role: FenceRoleSource, generation: Generation(4), held: true}
		provider := &recordingProvider{
			fencedDescriptor: descriptorForTarget(t, target, PhysicalIdentity("graph-identity")),
			mutateFenced: func(request *FencedInspectionRequest) {
				if err := request.Fence.Release(context.Background()); err != nil {
					t.Fatalf("provider release fence: %v", err)
				}
			},
		}

		_, err := InspectFenced(context.Background(), provider, FencedInspectionRequest{Target: target, Fence: fence, ExpectedGeneration: Generation(4)})
		if !errors.Is(err, ErrFenceNotHeld) {
			t.Fatalf("InspectFenced() error = %v, want ErrFenceNotHeld", err)
		}
	})
}

func TestOpenBindingUsesImmutableBaselineAfterProviderMutatesInput(t *testing.T) {
	descriptor := testDescriptor(t, ProviderID("builtin-open-copy"), PhysicalIdentity("open-copy-identity"), coordclass.ClassGraph)
	adapters := testBeadsAdapters(t)
	direct := &directOpenedBinding{descriptor: descriptor, capabilities: descriptor.Capabilities, graph: adapters.Graph, graphOK: true}
	provider := &recordingProvider{
		opened: direct,
		mutateOpen: func(request *OpenRequest) {
			request.Descriptor.Components[0].PhysicalIdentity = PhysicalIdentity("provider-mutated-open-input")
			request.ExpectedComponents[0].SchemaVersion = "provider-mutated-schema"
			request.DurableActiveAuthority = DurableActiveOpenAuthority{}
		},
	}
	request := completeOpenRequest(t, descriptor, classSet(t, coordclass.ClassGraph))

	opened, err := OpenBinding(context.Background(), provider, request)
	if err != nil {
		t.Fatalf("OpenBinding(): %v", err)
	}
	t.Cleanup(func() {
		if err := opened.Close(); err != nil {
			t.Errorf("opened.Close(): %v", err)
		}
	})
	if request.Descriptor.Components[0].PhysicalIdentity != PhysicalIdentity("open-copy-identity") || request.ExpectedComponents[0].SchemaVersion != "1" {
		t.Fatalf("provider mutation escaped open request boundary: %#v", request)
	}
}

func TestRecoverRetainedGuardsClonesSourceAcrossDiscoverAndInstall(t *testing.T) {
	source := testRetainedSource(PhysicalIdentity("retained-source-copy"))
	request := GuardInstallRequest{
		Attempt:          AttemptID("retained-copy-attempt"),
		Generation:       Generation(3),
		Source:           source,
		Component:        source.Component,
		PhysicalIdentity: source.PhysicalIdentity,
		Role:             GuardRoleDenyWrite,
	}
	lifecycle := &recordingGuardLifecycle{
		mutateDiscover: func(request *FencedGuardDiscoverRequest) {
			request.Source.ReopenData[0] = 'x'
		},
		mutateInstall: func(request *FencedGuardInstallRequest) {
			request.Source.ReopenData[1] = 'y'
		},
	}

	if _, err := RecoverRetainedGuards(context.Background(), lifecycle, []FencedGuardInstallRequest{fencedGuardInstallRequest(t, request)}, discardGuardReceipt); err != nil {
		t.Fatalf("RecoverRetainedGuards(): %v", err)
	}
	if got := string(request.Source.ReopenData); got != "opaque-reopen" {
		t.Fatalf("provider mutation escaped retained source boundary: %q", got)
	}
}

func TestRecoverRetainedGuardsRequiresAndRechecksSourceFence(t *testing.T) {
	providerErr := errors.New("retained guard provider failed")
	newRequest := func(t *testing.T) FencedGuardInstallRequest {
		t.Helper()
		install := GuardInstallRequest{
			Attempt:          AttemptID("fenced-guard-recovery"),
			Generation:       Generation(3),
			Source:           testRetainedSource(PhysicalIdentity("fenced-guard-source")),
			Component:        ComponentID("component"),
			PhysicalIdentity: PhysicalIdentity("fenced-guard-source"),
			Role:             GuardRoleDenyWrite,
		}
		return fencedGuardInstallRequest(t, install)
	}

	t.Run("lost before discovery prevents provider mutation", func(t *testing.T) {
		request := newRequest(t)
		if err := request.SourceFence.Release(context.Background()); err != nil {
			t.Fatalf("SourceFence.Release(): %v", err)
		}
		lifecycle := &recordingGuardLifecycle{}
		_, err := RecoverRetainedGuards(context.Background(), lifecycle, []FencedGuardInstallRequest{request}, discardGuardReceipt)
		if !errors.Is(err, ErrFenceNotHeld) {
			t.Fatalf("RecoverRetainedGuards() error = %v, want ErrFenceNotHeld", err)
		}
		if lifecycle.discoverCalls != 0 || lifecycle.installCalls != 0 || lifecycle.verifyCalls != 0 {
			t.Fatalf("lost source fence invoked provider: discover:%d install:%d verify:%d", lifecycle.discoverCalls, lifecycle.installCalls, lifecycle.verifyCalls)
		}
	})

	for _, test := range []struct {
		name      string
		configure func(*recordingGuardLifecycle)
		wantCalls func(*recordingGuardLifecycle) bool
	}{
		{
			name: "discover error after source fence release",
			configure: func(lifecycle *recordingGuardLifecycle) {
				lifecycle.discoverErr = providerErr
				lifecycle.mutateDiscover = func(request *FencedGuardDiscoverRequest) {
					if err := request.SourceFence.Release(context.Background()); err != nil {
						t.Fatalf("provider release source fence: %v", err)
					}
				}
			},
			wantCalls: func(lifecycle *recordingGuardLifecycle) bool {
				return lifecycle.discoverCalls == 1 && lifecycle.installCalls == 0
			},
		},
		{
			name: "install error after source fence release",
			configure: func(lifecycle *recordingGuardLifecycle) {
				lifecycle.mutateInstall = func(request *FencedGuardInstallRequest) {
					if err := request.SourceFence.Release(context.Background()); err != nil {
						t.Fatalf("provider release source fence: %v", err)
					}
				}
				lifecycle.installErr = providerErr
			},
			wantCalls: func(lifecycle *recordingGuardLifecycle) bool {
				return lifecycle.discoverCalls == 1 && lifecycle.installCalls == 1
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			lifecycle := &recordingGuardLifecycle{}
			test.configure(lifecycle)
			_, err := RecoverRetainedGuards(context.Background(), lifecycle, []FencedGuardInstallRequest{newRequest(t)}, discardGuardReceipt)
			if !errors.Is(err, providerErr) || !errors.Is(err, ErrFenceNotHeld) {
				t.Fatalf("RecoverRetainedGuards() error = %v, want provider error and ErrFenceNotHeld", err)
			}
			if !test.wantCalls(lifecycle) {
				t.Fatalf("unexpected lifecycle calls: discover:%d install:%d verify:%d", lifecycle.discoverCalls, lifecycle.installCalls, lifecycle.verifyCalls)
			}
		})
	}
}

func TestAttestGuardedActivationRetainsVerificationErrorAndSourceFenceLoss(t *testing.T) {
	decision, err := NewCommitDecision(AttemptID("attest-fence-loss"), Generation(12))
	if err != nil {
		t.Fatalf("NewCommitDecision(): %v", err)
	}
	install := GuardInstallRequest{
		Attempt:          decision.Attempt,
		Generation:       decision.Generation,
		Source:           testRetainedSource(PhysicalIdentity("attest-fence-source")),
		Component:        ComponentID("component"),
		PhysicalIdentity: PhysicalIdentity("attest-fence-source"),
		Role:             GuardRoleDenyWrite,
	}
	plan := fencedGuardPlan(t, install)
	receipts := []GuardReceipt{matchingGuardReceipt(install)}
	originalReceipt := receipts[0].Clone()
	providerErr := errors.New("guard verification failed")
	lifecycle := &recordingGuardLifecycle{
		verifyErr: providerErr,
		mutateVerify: func(request *FencedGuardVerificationRequest) {
			request.Receipt.Revalidation = "provider-mutated-revalidation"
			if err := request.SourceFence.Release(context.Background()); err != nil {
				t.Fatalf("SourceFence.Release(): %v", err)
			}
		},
	}

	_, err = AttestGuardedActivation(context.Background(), lifecycle, decision, "binding", testBindingIdentity("attest-fence-destination"), plan, receipts)
	if !errors.Is(err, providerErr) || !errors.Is(err, ErrFenceNotHeld) {
		t.Fatalf("AttestGuardedActivation() error = %v, want provider error and ErrFenceNotHeld", err)
	}
	if !guardReceiptsEqual(receipts[0], originalReceipt) {
		t.Fatalf("provider mutation escaped guarded-attestation receipt boundary: %#v", receipts[0])
	}
}

func TestTransitionRetainedGuardUsesDetachedTargetAndRechecksSourceFence(t *testing.T) {
	t.Run("provider input mutation does not alter transfer baseline", func(t *testing.T) {
		source := testRetainedSource(PhysicalIdentity("retained-transfer-copy"))
		install := GuardInstallRequest{Attempt: AttemptID("old-transfer-copy"), Generation: Generation(4), Source: source, Component: source.Component, PhysicalIdentity: source.PhysicalIdentity, Role: GuardRoleDenyWrite}
		current := matchingGuardReceipt(install)
		decision, err := NewCommitDecision(AttemptID("new-transfer-copy"), Generation(5))
		if err != nil {
			t.Fatalf("NewCommitDecision(): %v", err)
		}
		proof := ParticipantReceipt{Version: 1, Kind: ParticipantReceiptBindingActivation, Attempt: decision.Attempt, Generation: decision.Generation, Participant: "transfer-binding", DescriptorIdentity: testBindingIdentity("transfer-copy-destination"), ReceiptID: "transfer-copy-receipt"}
		target := GuardTransferTarget{Decision: decision, Participant: proof.Participant, DestinationIdentity: proof.DescriptorIdentity, ExpectedReceiptKind: proof.Kind, State: GuardTransferActive, ActiveProof: &proof}
		expected := current
		expected.Attempt = decision.Attempt
		expected.Generation = decision.Generation
		expected.Role = GuardRoleTransfer
		expected.TransferState = GuardTransferActive
		expected.TransferParticipant = proof.Participant
		expected.TransferDestinationIdentity = proof.DescriptorIdentity
		expected.TransferReceiptKind = proof.Kind
		expected.ActiveProof = &proof
		lifecycle := &recordingGuardLifecycle{
			transitionReceipt: expected,
			mutateTransition: func(_ context.Context, request *GuardTransitionRequest) {
				request.Target.ActiveProof.ReceiptID = "provider-mutated-receipt"
			},
		}

		if _, err := TransitionRetainedGuard(context.Background(), lifecycle, GuardTransitionRequest{Current: current, Target: &target}); err != nil {
			t.Fatalf("TransitionRetainedGuard(): %v", err)
		}
		if target.ActiveProof.ReceiptID != "transfer-copy-receipt" {
			t.Fatalf("provider mutation escaped transfer target boundary: %#v", target)
		}
	})

	t.Run("provider cannot release source fence before returning release receipt", func(t *testing.T) {
		source := testRetainedSource(PhysicalIdentity("retained-release-fence"))
		install := GuardInstallRequest{Attempt: AttemptID("release-fence-attempt"), Generation: Generation(4), Source: source, Component: source.Component, PhysicalIdentity: source.PhysicalIdentity, Role: GuardRoleDenyWrite}
		current := matchingGuardReceipt(install)
		descriptor := testDescriptor(t, source.Provider, source.PhysicalIdentity, coordclass.ClassWork)
		fence := fenceForDescriptor(t, descriptor, FenceRoleSource, current.Generation)
		authority := testPredecisionAbandonmentAuthority(t, current, descriptor, fence)
		lifecycle := &recordingGuardLifecycle{
			transitionReceipt: current,
			mutateTransition: func(_ context.Context, request *GuardTransitionRequest) {
				if err := request.SourceFence.Release(context.Background()); err != nil {
					t.Fatalf("provider release source fence: %v", err)
				}
			},
		}

		_, err := TransitionRetainedGuard(context.Background(), lifecycle, GuardTransitionRequest{Current: current, Release: true, Abandonment: &authority, SourceDescriptor: descriptor, SourceFence: fence})
		if !errors.Is(err, ErrFenceNotHeld) {
			t.Fatalf("TransitionRetainedGuard() error = %v, want ErrFenceNotHeld", err)
		}
	})
}

func TestWorkLifecycleRejectsPostCallFenceLoss(t *testing.T) {
	t.Run("prepare", func(t *testing.T) {
		fixture := newWorkMigrationFixture(t)
		lifecycle := &recordingWorkMigration{afterPrepare: func(_ context.Context, request WorkPrepareRequest) {
			if err := request.SourceFence.Release(context.Background()); err != nil {
				t.Fatalf("provider release source fence: %v", err)
			}
		}}
		_, err := PrepareWork(context.Background(), lifecycle, fixture.prepare)
		if !errors.Is(err, ErrFenceNotHeld) {
			t.Fatalf("PrepareWork() error = %v, want ErrFenceNotHeld", err)
		}
	})

	t.Run("verify", func(t *testing.T) {
		fixture := newWorkMigrationFixture(t)
		lifecycle := &recordingWorkMigration{afterVerify: func(_ context.Context, request WorkVerifyRequest) {
			if err := request.Prepare.SourceFence.Release(context.Background()); err != nil {
				t.Fatalf("provider release source fence: %v", err)
			}
		}}
		_, err := VerifyWork(context.Background(), lifecycle, WorkVerifyRequest{Prepare: fixture.prepare, Prepared: fixture.prepared})
		if !errors.Is(err, ErrFenceNotHeld) {
			t.Fatalf("VerifyWork() error = %v, want ErrFenceNotHeld", err)
		}
	})

	t.Run("resume", func(t *testing.T) {
		fixture := newWorkMigrationFixture(t)
		lifecycle := &recordingWorkMigration{afterResume: func(_ context.Context, request WorkResumeRequest) {
			if err := request.Prepare.SourceFence.Release(context.Background()); err != nil {
				t.Fatalf("provider release source fence: %v", err)
			}
		}}
		_, err := ResumeWork(context.Background(), lifecycle, WorkResumeRequest{Prepare: fixture.prepare})
		if !errors.Is(err, ErrFenceNotHeld) {
			t.Fatalf("ResumeWork() error = %v, want ErrFenceNotHeld", err)
		}
	})
}

func TestFenceBearingOperationsJoinProviderErrorsWithPostCallFenceLoss(t *testing.T) {
	providerErr := errors.New("provider operation failed")
	assertJoinedFenceFailure := func(t *testing.T, err error) {
		t.Helper()
		if !errors.Is(err, providerErr) || !errors.Is(err, ErrFenceNotHeld) {
			t.Fatalf("operation error = %v, want provider error and ErrFenceNotHeld", err)
		}
	}

	t.Run("fenced inspection", func(t *testing.T) {
		target := testFenceTarget(t)
		fence := &recordingFence{target: target, role: FenceRoleSource, generation: Generation(4), held: true}
		provider := &recordingProvider{
			fencedErr: providerErr,
			mutateFenced: func(request *FencedInspectionRequest) {
				if err := request.Fence.Release(context.Background()); err != nil {
					t.Fatalf("provider release fence: %v", err)
				}
			},
		}
		_, err := InspectFenced(context.Background(), provider, FencedInspectionRequest{Target: target, Fence: fence, ExpectedGeneration: Generation(4)})
		assertJoinedFenceFailure(t, err)
	})

	t.Run("retained guard transition", func(t *testing.T) {
		source := testRetainedSource(PhysicalIdentity("transition-error-fence"))
		install := GuardInstallRequest{Attempt: AttemptID("transition-error"), Generation: Generation(4), Source: source, Component: source.Component, PhysicalIdentity: source.PhysicalIdentity, Role: GuardRoleDenyWrite}
		current := matchingGuardReceipt(install)
		descriptor := testDescriptor(t, source.Provider, source.PhysicalIdentity, coordclass.ClassWork)
		fence := fenceForDescriptor(t, descriptor, FenceRoleSource, current.Generation)
		authority := testPredecisionAbandonmentAuthority(t, current, descriptor, fence)
		lifecycle := &recordingGuardLifecycle{
			transitionReceipt: current,
			transitionErr:     providerErr,
			mutateTransition: func(_ context.Context, request *GuardTransitionRequest) {
				if err := request.SourceFence.Release(context.Background()); err != nil {
					t.Fatalf("provider release source fence: %v", err)
				}
			},
		}
		_, err := TransitionRetainedGuard(context.Background(), lifecycle, GuardTransitionRequest{Current: current, Release: true, Abandonment: &authority, SourceDescriptor: descriptor, SourceFence: fence})
		assertJoinedFenceFailure(t, err)
	})

	for _, test := range []struct {
		name string
		call func(*recordingWorkMigration, workMigrationFixture) error
	}{
		{
			name: "prepare",
			call: func(lifecycle *recordingWorkMigration, fixture workMigrationFixture) error {
				lifecycle.afterPrepare = func(_ context.Context, request WorkPrepareRequest) {
					if err := request.SourceFence.Release(context.Background()); err != nil {
						t.Fatalf("provider release source fence: %v", err)
					}
				}
				lifecycle.prepareErr = providerErr
				_, err := PrepareWork(context.Background(), lifecycle, fixture.prepare)
				return err
			},
		},
		{
			name: "verify",
			call: func(lifecycle *recordingWorkMigration, fixture workMigrationFixture) error {
				lifecycle.afterVerify = func(_ context.Context, request WorkVerifyRequest) {
					if err := request.Prepare.SourceFence.Release(context.Background()); err != nil {
						t.Fatalf("provider release source fence: %v", err)
					}
				}
				lifecycle.verifyErr = providerErr
				_, err := VerifyWork(context.Background(), lifecycle, WorkVerifyRequest{Prepare: fixture.prepare, Prepared: fixture.prepared})
				return err
			},
		},
		{
			name: "resume",
			call: func(lifecycle *recordingWorkMigration, fixture workMigrationFixture) error {
				lifecycle.afterResume = func(_ context.Context, request WorkResumeRequest) {
					if err := request.Prepare.SourceFence.Release(context.Background()); err != nil {
						t.Fatalf("provider release source fence: %v", err)
					}
				}
				lifecycle.resumeErr = providerErr
				_, err := ResumeWork(context.Background(), lifecycle, WorkResumeRequest{Prepare: fixture.prepare})
				return err
			},
		},
	} {
		t.Run("work "+test.name, func(t *testing.T) {
			assertJoinedFenceFailure(t, test.call(&recordingWorkMigration{}, newWorkMigrationFixture(t)))
		})
	}
}

func TestFencePostvalidationUsesBoundedContextAfterProviderCancellation(t *testing.T) {
	providerErr := context.Canceled
	canceledCheckErr := errors.New("writer fence rechecked with canceled operation context")

	for _, test := range []struct {
		name          string
		releaseFence  bool
		wantFenceLoss bool
	}{
		{name: "held fence keeps provider cancellation distinct"},
		{name: "released fence joins provider cancellation and fence loss", releaseFence: true, wantFenceLoss: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			target := testFenceTarget(t)
			fence := &cancellationAwareFence{
				recordingFence: &recordingFence{target: target, role: FenceRoleSource, generation: Generation(4), held: true},
				canceledErr:    canceledCheckErr,
			}
			provider := &recordingProvider{
				fencedErr: providerErr,
				mutateFenced: func(request *FencedInspectionRequest) {
					cancel()
					if test.releaseFence {
						if err := request.Fence.Release(context.Background()); err != nil {
							t.Fatalf("provider release fence: %v", err)
						}
					}
				},
			}

			_, err := InspectFenced(ctx, provider, FencedInspectionRequest{Target: target, Fence: fence, ExpectedGeneration: Generation(4)})
			if !errors.Is(err, providerErr) {
				t.Fatalf("InspectFenced() error = %v, want provider cancellation", err)
			}
			if errors.Is(err, canceledCheckErr) {
				t.Fatalf("InspectFenced() postvalidated with canceled operation context: %v", err)
			}
			if errors.Is(err, ErrFenceNotHeld) != test.wantFenceLoss {
				t.Fatalf("InspectFenced() error = %v, fence loss = %t, want %t", err, errors.Is(err, ErrFenceNotHeld), test.wantFenceLoss)
			}
		})
	}
}

func TestCommitWorkRequiresLivePreparationFencesThroughProviderCommit(t *testing.T) {
	newRequest := func(t *testing.T) (workMigrationFixture, WorkCommitRequest) {
		t.Helper()
		fixture := newWorkMigrationFixture(t)
		return fixture, WorkCommitRequest{
			Decision:    fixture.decision,
			Participant: fixture.participant,
			Prepare:     fixture.prepare,
			Prepared:    fixture.prepared,
			Proof:       fixture.proof,
		}
	}

	t.Run("released preparation fence prevents commit mutation", func(t *testing.T) {
		fixture, request := newRequest(t)
		if err := fixture.prepare.SourceFence.Release(context.Background()); err != nil {
			t.Fatalf("SourceFence.Release(): %v", err)
		}
		called := false
		lifecycle := &recordingWorkMigration{afterCommit: func(context.Context, WorkCommitRequest) { called = true }}

		_, err := CommitWork(context.Background(), lifecycle, request)
		if !errors.Is(err, ErrFenceNotHeld) {
			t.Fatalf("CommitWork() error = %v, want ErrFenceNotHeld", err)
		}
		if called {
			t.Fatal("CommitWork() called provider after preparation fence loss")
		}
	})

	t.Run("provider error and post-call fence loss are both retained", func(t *testing.T) {
		_, request := newRequest(t)
		providerErr := errors.New("work commit failed")
		lifecycle := &recordingWorkMigration{
			commitErr: providerErr,
			afterCommit: func(_ context.Context, request WorkCommitRequest) {
				if err := request.Prepare.DestinationFence.Release(context.Background()); err != nil {
					t.Fatalf("DestinationFence.Release(): %v", err)
				}
			},
		}

		_, err := CommitWork(context.Background(), lifecycle, request)
		if !errors.Is(err, providerErr) || !errors.Is(err, ErrFenceNotHeld) {
			t.Fatalf("CommitWork() error = %v, want provider error and ErrFenceNotHeld", err)
		}
	})
}

func TestCommitWorkDetachesProviderReturnedParticipantReceipt(t *testing.T) {
	fixture := newWorkMigrationFixture(t)
	request := WorkCommitRequest{
		Decision:    fixture.decision,
		Participant: fixture.participant,
		Prepare:     fixture.prepare,
		Prepared:    fixture.prepared,
		Proof:       fixture.proof,
	}
	providerReceipt := ParticipantReceipt{
		Version:            1,
		Kind:               ParticipantReceiptWorkMigration,
		Attempt:            fixture.decision.Attempt,
		Generation:         fixture.decision.Generation,
		Participant:        fixture.participant.Key(),
		DescriptorIdentity: fixture.destinationIdentity,
		ReceiptID:          "provider-receipt",
		Preparation:        fixture.prepared.Preparation.Clone(),
		PreparedReceipt:    fixture.prepared.Receipt,
	}

	receipt, err := CommitWork(context.Background(), &scriptedWorkMigration{receipt: providerReceipt}, request)
	if err != nil {
		t.Fatalf("CommitWork(): %v", err)
	}
	providerReceipt.Preparation.Participant.Members[0].Prefix = "provider-mutated-prefix"
	if got := receipt.Preparation.Participant.Members[0].Prefix; got != "hq" {
		t.Fatalf("provider mutation escaped returned participant receipt: prefix = %q, want hq", got)
	}
}

func TestBindingActivationRequiresLiveDestinationFenceThroughProviderCall(t *testing.T) {
	newActivation := func(t *testing.T) (BindingActivationRequest, BindingActivationResumeRequest) {
		t.Helper()
		destination := testDescriptor(t, ProviderID("builtin-binding-fence"), PhysicalIdentity("binding-fence-destination"), coordclass.ClassGraph)
		destination.Capabilities.GuardedActivation = true
		identity, err := destination.Identity()
		if err != nil {
			t.Fatalf("Destination.Identity(): %v", err)
		}
		decision, err := NewCommitDecision(AttemptID("binding-fence-attempt"), Generation(17))
		if err != nil {
			t.Fatalf("NewCommitDecision(): %v", err)
		}
		install := GuardInstallRequest{
			Attempt:          decision.Attempt,
			Generation:       decision.Generation,
			Source:           testRetainedSource(PhysicalIdentity("binding-fence-source")),
			Component:        ComponentID("component"),
			PhysicalIdentity: PhysicalIdentity("binding-fence-source"),
			Role:             GuardRoleDenyWrite,
		}
		attestation, err := AttestGuardedActivation(context.Background(), &recordingGuardLifecycle{}, decision, "binding", identity, fencedGuardPlan(t, install), []GuardReceipt{matchingGuardReceipt(install)})
		if err != nil {
			t.Fatalf("AttestGuardedActivation(): %v", err)
		}
		fence := fenceForDescriptor(t, destination, FenceRolePopulatedDestination, decision.Generation)
		activate := BindingActivationRequest{
			Decision:         decision,
			Participant:      "binding",
			Destination:      destination,
			DesiredIdentity:  identity,
			GuardAttestation: attestation,
			DestinationFence: fence,
		}
		return activate, BindingActivationResumeRequest{
			Decision:         decision,
			Participant:      "binding",
			Destination:      destination,
			DesiredIdentity:  identity,
			GuardAttestation: attestation,
			DestinationFence: fence,
		}
	}

	t.Run("lost before activation prevents provider mutation", func(t *testing.T) {
		activate, _ := newActivation(t)
		if err := activate.DestinationFence.Release(context.Background()); err != nil {
			t.Fatalf("DestinationFence.Release(): %v", err)
		}
		called := false
		lifecycle := &recordingBindingMigration{afterActivate: func(context.Context, BindingActivationRequest) { called = true }}
		_, err := ActivateBinding(context.Background(), lifecycle, activate)
		if !errors.Is(err, ErrFenceNotHeld) {
			t.Fatalf("ActivateBinding() error = %v, want ErrFenceNotHeld", err)
		}
		if called {
			t.Fatal("ActivateBinding() called provider after destination fence loss")
		}
	})

	for _, test := range []struct {
		name string
		call func(context.Context, *recordingBindingMigration, BindingActivationRequest, BindingActivationResumeRequest, error) error
	}{
		{
			name: "activate joins provider error and post-call fence loss",
			call: func(ctx context.Context, lifecycle *recordingBindingMigration, activate BindingActivationRequest, _ BindingActivationResumeRequest, providerErr error) error {
				lifecycle.activateErr = providerErr
				lifecycle.afterActivate = func(_ context.Context, request BindingActivationRequest) {
					if err := request.DestinationFence.Release(context.Background()); err != nil {
						t.Fatalf("DestinationFence.Release(): %v", err)
					}
				}
				_, err := ActivateBinding(ctx, lifecycle, activate)
				return err
			},
		},
		{
			name: "resume joins provider error and post-call fence loss",
			call: func(ctx context.Context, lifecycle *recordingBindingMigration, _ BindingActivationRequest, resume BindingActivationResumeRequest, providerErr error) error {
				lifecycle.resumeErr = providerErr
				lifecycle.afterResume = func(_ context.Context, request BindingActivationResumeRequest) {
					if err := request.DestinationFence.Release(context.Background()); err != nil {
						t.Fatalf("DestinationFence.Release(): %v", err)
					}
				}
				_, err := RecoverBindingActivation(ctx, lifecycle, resume)
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			activate, resume := newActivation(t)
			lifecycle := &recordingBindingMigration{}
			providerErr := errors.New(test.name)
			err := test.call(context.Background(), lifecycle, activate, resume, providerErr)
			if !errors.Is(err, providerErr) || !errors.Is(err, ErrFenceNotHeld) {
				t.Fatalf("activation operation error = %v, want provider error and ErrFenceNotHeld", err)
			}
		})
	}
}

func TestOpenRetainedWorkTopologyUsesDetachedRequests(t *testing.T) {
	member := workMember(HQScope(), "hq", 0, false, "retained-copy-workspace")
	member.Provider = ProviderID("builtin-retained-copy")
	participant, err := NewWorkWorkspaceParticipant(ProviderID("builtin-retained-copy"), ComponentID("work"), PhysicalIdentity("retained-copy-workspace"), []WorkWorkspaceMember{member})
	if err != nil {
		t.Fatalf("NewWorkWorkspaceParticipant(): %v", err)
	}
	workspace, err := NewRetainedWorkWorkspaceWithViews(participant, retainedPrefixViews(participant), func() error { return nil })
	if err != nil {
		t.Fatalf("NewRetainedWorkWorkspaceWithViews(): %v", err)
	}
	request := RetainedWorkRequest{Attempt: AttemptID("retained-open-copy"), Generation: Generation(5), Participant: participant, Source: testRetainedWorkSource(participant), ExpectedContract: ContractVersion("storage-v1")}
	lifecycle := &recordingWorkMigration{
		retainedOverride: &workspace,
		mutateOpenRetained: func(request *RetainedWorkRequest) {
			request.Participant.Members[0].Prefix = "provider-mutated-prefix"
			request.Source.ReopenData[0] = 'x'
		},
	}

	_, handles, err := OpenRetainedWorkTopology(context.Background(), lifecycle, []RetainedWorkRequest{request})
	if err != nil {
		t.Fatalf("OpenRetainedWorkTopology(): %v", err)
	}
	for _, handle := range handles {
		if err := handle.Close(); err != nil {
			t.Errorf("handle.Close(): %v", err)
		}
	}
	if request.Participant.Members[0].Prefix != "hq" || string(request.Source.ReopenData) != "opaque-reopen" {
		t.Fatalf("provider mutation escaped retained work request boundary: %#v", request)
	}
}
