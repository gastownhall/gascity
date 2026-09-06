package storebinding

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/coordclass"
)

func migrationPhases() []MigrationPhase {
	return []MigrationPhase{
		PhaseIntentFsynced,
		PhasePreparing,
		PhasePrepared,
		PhaseGuarding,
		PhaseGuardsInstalled,
		PhaseCommitDecided,
		PhaseReceiptsPersisted,
		PhaseActiveManifestDurable,
		PhaseActiveOpenPending,
		PhaseActive,
	}
}

func TestAttemptRecordRequiresItsPhaseFieldSetAndRejectsFieldsFromLaterPhases(t *testing.T) {
	for _, phase := range migrationPhases() {
		t.Run(phase.String(), func(t *testing.T) {
			saga := migrationRelocationSaga(t)
			record := saga.advanceTo(t, phase)
			if err := record.Validate(); err != nil {
				t.Fatalf("a record built by the saga is invalid at %s: %v", phase, err)
			}

			for _, mutation := range []struct {
				name  string
				apply func(*AttemptRecord)
			}{
				{"preparing section", func(r *AttemptRecord) { r.Preparing = nil }},
				{"prepared section", func(r *AttemptRecord) { r.Prepared = nil }},
				{"guarding section", func(r *AttemptRecord) { r.Guarding = nil }},
				{"guards-installed section", func(r *AttemptRecord) { r.GuardsInstalled = nil }},
				{"decision section", func(r *AttemptRecord) { r.Decision = nil }},
				{"active manifest digest", func(r *AttemptRecord) { r.ActiveManifestDigest = "" }},
			} {
				damaged := record.Clone()
				before := damaged.Clone()
				mutation.apply(damaged)
				if encodeRecordJSON(t, before) == encodeRecordJSON(t, damaged) {
					continue // the field is legitimately absent at this phase
				}
				if err := damaged.Validate(); err == nil {
					t.Fatalf("a record at %s validated with its %s dropped", phase, mutation.name)
				}
			}
		})
	}
}

func encodeRecordJSON(t *testing.T, record *AttemptRecord) string {
	t.Helper()
	payload, err := encodeAttemptRecord(record)
	if err != nil {
		return "invalid:" + err.Error()
	}
	return string(payload)
}

func TestAttemptRecordRejectsPhaseSkipsAndBackwardTransitions(t *testing.T) {
	saga := migrationRelocationSaga(t)
	record := saga.record

	if err := record.EnterPrepared(saga.preparedSection(t)); !errors.Is(err, ErrInvalidPhaseTransition) {
		t.Fatalf("PREPARED straight from INTENT_FSYNCED = %v, want a rejected transition", err)
	}
	if err := record.EnterGuarding(GuardingSection{Version: 1, Plan: saga.guardPlan}); !errors.Is(err, ErrInvalidPhaseTransition) {
		t.Fatalf("GUARDING straight from INTENT_FSYNCED = %v, want a rejected transition", err)
	}
	if record.Phase != PhaseIntentFsynced {
		t.Fatalf("a rejected transition moved the record to %s", record.Phase)
	}
	if err := record.EnterPreparing(saga.preparingSection(t), PhaseEntryReturnSourceChanged); !errors.Is(err, ErrInvalidPhaseTransition) {
		t.Fatalf("returning to PREPARING from INTENT_FSYNCED = %v, want a rejected transition", err)
	}
}

func TestAttemptRecordSeparatesNotStartedFromStartedAndRolledBack(t *testing.T) {
	saga := migrationRelocationSaga(t)
	record := saga.advanceTo(t, PhasePreparing)

	fresh, err := record.Resume()
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if fresh.PhaseEntries != 1 || fresh.Returned || len(fresh.DirtyDestinations) != 0 {
		t.Fatalf("a first PREPARING entry resumed as %#v, want one entry with no residue", fresh)
	}

	// The attempt reaches PREPARED, having written into the destination, and the
	// source then changes: the PREPARED -> PREPARING edge.
	if err := record.RecordDestinationResidue(DestinationResidue{
		Binding:          "next",
		Component:        saga.destination.Components[0].ID,
		PhysicalIdentity: saga.destination.Components[0].PhysicalIdentity,
		Kind:             ResidueWritten,
	}); err != nil {
		t.Fatalf("RecordDestinationResidue: %v", err)
	}
	if err := record.EnterPrepared(saga.preparedSection(t)); err != nil {
		t.Fatalf("EnterPrepared: %v", err)
	}
	if err := record.EnterPreparing(saga.preparingSection(t), PhaseEntryReturnSourceChanged); err != nil {
		t.Fatalf("return to PREPARING: %v", err)
	}

	returned, err := record.Resume()
	if err != nil {
		t.Fatalf("Resume after return: %v", err)
	}
	if returned.Phase != fresh.Phase {
		t.Fatalf("returned phase = %s, want the same %s the fresh entry reported", returned.Phase, fresh.Phase)
	}
	// Same phase, same action, same authority — so the phase field alone cannot
	// tell recovery that a destination was already written. These three fields
	// are what make the two states distinguishable.
	if returned.PhaseEntries <= fresh.PhaseEntries {
		t.Fatalf("returned entry count = %d, want more than the first entry's %d", returned.PhaseEntries, fresh.PhaseEntries)
	}
	if !returned.Returned {
		t.Fatal("a durable return to PREPARING reported forward progress")
	}
	if len(returned.DirtyDestinations) != 1 || returned.DirtyDestinations[0].Kind != ResidueWritten {
		t.Fatalf("dirty destinations = %#v, want the written destination the earlier entry produced", returned.DirtyDestinations)
	}
	if record.Prepared != nil {
		t.Fatal("a returned record still carries the PREPARED section it invalidated")
	}
}

func TestAttemptRecordRecordsGuardReleaseSoDiscoveryIsNotAmbiguous(t *testing.T) {
	saga := migrationRelocationSaga(t)
	record := saga.advanceTo(t, PhaseGuarding)
	request := saga.guardPlan[0]
	receipt := migrationGuardReceipt(t, request, saga.source.SemanticContractVersion)
	if err := record.AppendGuardReceipt(receipt); err != nil {
		t.Fatalf("AppendGuardReceipt: %v", err)
	}

	if err := record.RecordGuardRelease(GuardRelease{
		Provider:         receipt.Provider,
		Component:        receipt.Component,
		PhysicalIdentity: receipt.PhysicalIdentity,
		Role:             receipt.Role,
		ReceiptID:        receipt.ReceiptID,
	}); err != nil {
		t.Fatalf("RecordGuardRelease: %v", err)
	}
	if err := record.EnterPreparing(saga.preparingSection(t), PhaseEntryReturnGuardsReleased); err != nil {
		t.Fatalf("return to PREPARING: %v", err)
	}

	plan, err := record.Resume()
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(plan.ReleasedGuards) != 1 || plan.ReleasedGuards[0].ReceiptID != receipt.ReceiptID {
		t.Fatalf("released guards = %#v, want the guard that was installed and then released", plan.ReleasedGuards)
	}
	if record.Guarding != nil {
		t.Fatal("a returned record still carries its GUARDING section")
	}
}

// migrationGuardReleaseFor builds the release record a caller writes after it
// takes one installed guard off the source.
func migrationGuardReleaseFor(receipt GuardReceipt) GuardRelease {
	return GuardRelease{
		Provider:         receipt.Provider,
		Component:        receipt.Component,
		PhysicalIdentity: receipt.PhysicalIdentity,
		Role:             receipt.Role,
		ReceiptID:        receipt.ReceiptID,
	}
}

func TestAttemptRecordRefusesToReturnToPreparingWhileInstalledGuardsAreUnreleased(t *testing.T) {
	// The return discards the GUARDING and GUARDS_INSTALLED sections, which is
	// where every install receipt lives. Both phases and every return reason
	// destroy the same evidence, so all six combinations must refuse.
	for _, phase := range []MigrationPhase{PhaseGuarding, PhaseGuardsInstalled} {
		for _, reason := range []PhaseEntryReason{
			PhaseEntryReturnGuardsReleased,
			PhaseEntryReturnSourceChanged,
			PhaseEntryReturnPreparationAbandoned,
		} {
			t.Run(phase.String()+"/"+reason.String(), func(t *testing.T) {
				saga := migrationRelocationSaga(t)
				record := saga.advanceTo(t, phase)
				receipt := migrationGuardReceipt(t, saga.guardPlan[0], saga.source.SemanticContractVersion)
				if phase == PhaseGuarding {
					if err := record.AppendGuardReceipt(receipt); err != nil {
						t.Fatalf("AppendGuardReceipt: %v", err)
					}
				}

				if err := record.EnterPreparing(saga.preparingSection(t), reason); !errors.Is(err, ErrInvalidPhaseTransition) {
					t.Fatalf("%s with an installed guard still on the source = %v, want a rejected transition", reason, err)
				}
				// The refusal must leave the receipt where recovery reads it.
				if record.Phase != phase {
					t.Fatalf("a refused return moved the record to %s", record.Phase)
				}
				if record.Guarding == nil || len(record.Guarding.Receipts) != 1 || record.Guarding.Receipts[0].ReceiptID != receipt.ReceiptID {
					t.Fatalf("a refused return damaged the GUARDING section: %#v", record.Guarding)
				}
				blocked, err := record.Resume()
				if err != nil {
					t.Fatalf("Resume after a refused return: %v", err)
				}
				if blocked.Phase != phase || blocked.Returned {
					t.Fatalf("a refused return resumed as %#v, want the unchanged %s state", blocked, phase)
				}

				// One release for one receipt, but of a receipt that was never
				// installed: a count check would accept it.
				wrong := migrationGuardReleaseFor(receipt)
				wrong.ReceiptID = receipt.ReceiptID + "-somewhere-else"
				if err := record.RecordGuardRelease(wrong); err != nil {
					t.Fatalf("RecordGuardRelease: %v", err)
				}
				if err := record.EnterPreparing(saga.preparingSection(t), reason); !errors.Is(err, ErrInvalidPhaseTransition) {
					t.Fatalf("%s after releasing a different receipt = %v, want a rejected transition", reason, err)
				}

				if err := record.RecordGuardRelease(migrationGuardReleaseFor(receipt)); err != nil {
					t.Fatalf("RecordGuardRelease: %v", err)
				}
				if err := record.EnterPreparing(saga.preparingSection(t), reason); err != nil {
					t.Fatalf("%s after releasing the installed guard: %v", reason, err)
				}
				if record.Guarding != nil || record.GuardsInstalled != nil {
					t.Fatal("a completed return still carries the guard sections it invalidated")
				}

				returned, err := record.Resume()
				if err != nil {
					t.Fatalf("Resume after the return: %v", err)
				}
				if !returned.Returned || returned.Phase != PhasePreparing {
					t.Fatalf("the return resumed as %#v, want a returned PREPARING", returned)
				}
				var released bool
				for _, release := range returned.ReleasedGuards {
					if release.Component == receipt.Component && release.ReceiptID == receipt.ReceiptID {
						released = true
					}
				}
				if !released {
					t.Fatalf("released guards = %#v, want the receipt the return discarded", returned.ReleasedGuards)
				}
			})
		}
	}
}

func TestAttemptRecordDoesNotAcceptAnEarlierEntrysGuardReleaseForAReinstalledGuard(t *testing.T) {
	// Receipt IDs are provider-derived and recur across attempts at the same
	// guard, so "a release with this receipt ID exists somewhere in the journal"
	// is not evidence that the guard installed under the current GUARDING entry
	// came off.
	saga := migrationRelocationSaga(t)
	record := saga.advanceTo(t, PhaseGuarding)
	receipt := migrationGuardReceipt(t, saga.guardPlan[0], saga.source.SemanticContractVersion)
	if err := record.AppendGuardReceipt(receipt); err != nil {
		t.Fatalf("AppendGuardReceipt: %v", err)
	}
	if err := record.RecordGuardRelease(migrationGuardReleaseFor(receipt)); err != nil {
		t.Fatalf("RecordGuardRelease: %v", err)
	}
	if err := record.EnterPreparing(saga.preparingSection(t), PhaseEntryReturnGuardsReleased); err != nil {
		t.Fatalf("first return to PREPARING: %v", err)
	}

	// The attempt runs again and reinstalls the same guard.
	if err := record.EnterPrepared(saga.preparedSection(t)); err != nil {
		t.Fatalf("EnterPrepared after the return: %v", err)
	}
	if err := record.EnterGuarding(GuardingSection{Version: 1, Plan: saga.guardPlan}); err != nil {
		t.Fatalf("EnterGuarding after the return: %v", err)
	}
	if err := record.AppendGuardReceipt(receipt); err != nil {
		t.Fatalf("AppendGuardReceipt after the return: %v", err)
	}

	if err := record.EnterPreparing(saga.preparingSection(t), PhaseEntryReturnGuardsReleased); !errors.Is(err, ErrInvalidPhaseTransition) {
		t.Fatalf("second return on the first entry's release = %v, want a rejected transition", err)
	}
	if record.Guarding == nil || len(record.Guarding.Receipts) != 1 {
		t.Fatalf("a refused return damaged the reinstalled GUARDING section: %#v", record.Guarding)
	}

	if err := record.RecordGuardRelease(migrationGuardReleaseFor(receipt)); err != nil {
		t.Fatalf("RecordGuardRelease for the reinstalled guard: %v", err)
	}
	if err := record.EnterPreparing(saga.preparingSection(t), PhaseEntryReturnGuardsReleased); err != nil {
		t.Fatalf("second return after releasing the reinstalled guard: %v", err)
	}
	plan, err := record.Resume()
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(plan.ReleasedGuards) != 2 {
		t.Fatalf("released guards = %#v, want one release per installation", plan.ReleasedGuards)
	}
	if plan.ReleasedGuards[0].Entry >= plan.ReleasedGuards[1].Entry {
		t.Fatalf("the two releases share journal entry %d, so neither names which installation it took off", plan.ReleasedGuards[0].Entry)
	}
}

func TestAttemptRecordForbidsReleaseAndReturnAfterTheDecision(t *testing.T) {
	saga := migrationRelocationSaga(t)
	record := saga.advanceTo(t, PhaseCommitDecided)

	if err := record.EnterPreparing(saga.preparingSection(t), PhaseEntryReturnSourceChanged); !errors.Is(err, ErrInvalidPhaseTransition) {
		t.Fatalf("return to PREPARING after the decision = %v, want a rejected transition", err)
	}
	if err := record.RecordGuardRelease(GuardRelease{
		Provider:         saga.source.Provider,
		Component:        saga.source.Components[0].ID,
		PhysicalIdentity: saga.source.Components[0].PhysicalIdentity,
		Role:             GuardRoleDenyWrite,
		ReceiptID:        "guard-release",
	}); !errors.Is(err, ErrInvalidPhaseTransition) {
		t.Fatalf("guard release after the decision = %v, want a rejected transition", err)
	}
	if record.Phase != PhaseCommitDecided {
		t.Fatalf("a rejected post-decision operation moved the record to %s", record.Phase)
	}
}

func TestGuardPlanIsRecordedBeforeAnyInstallAndVerifiedBeforeTheDecision(t *testing.T) {
	saga := migrationRelocationSaga(t)
	record := saga.advanceTo(t, PhasePrepared)
	receipt := migrationGuardReceipt(t, saga.guardPlan[0], saga.source.SemanticContractVersion)

	if err := record.EnterGuarding(GuardingSection{Version: 1, Plan: saga.guardPlan, Receipts: []GuardReceipt{receipt}}); !errors.Is(err, ErrInvalidPhaseTransition) {
		t.Fatalf("GUARDING with a receipt already appended = %v, want a rejected transition", err)
	}
	if err := record.EnterGuarding(GuardingSection{Version: 1, Plan: saga.guardPlan}); err != nil {
		t.Fatalf("EnterGuarding: %v", err)
	}
	// The plan has one entry and no receipt yet: an invisible half-installed
	// guard must never be assumed absent.
	if err := record.EnterGuardsInstalled(GuardsInstalledSection{Version: 1}); !errors.Is(err, ErrInvalidPhaseTransition) {
		t.Fatalf("GUARDS_INSTALLED with no receipts = %v, want a rejected transition", err)
	}
	if err := record.AppendGuardReceipt(receipt); err != nil {
		t.Fatalf("AppendGuardReceipt: %v", err)
	}
	// Re-appending the same receipt is the recovery replay and must be a no-op.
	if err := record.AppendGuardReceipt(receipt); err != nil {
		t.Fatalf("idempotent AppendGuardReceipt: %v", err)
	}
	if len(record.Guarding.Receipts) != 1 {
		t.Fatalf("guard receipts = %d, want one idempotent entry", len(record.Guarding.Receipts))
	}
	conflicting := receipt
	conflicting.ReceiptID = "guard-somewhere-else"
	if err := record.AppendGuardReceipt(conflicting); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("conflicting guard receipt = %v, want a blocked conflict", err)
	}
	if err := record.EnterGuardsInstalled(GuardsInstalledSection{Version: 1, Verified: []GuardReceipt{receipt}}); err != nil {
		t.Fatalf("EnterGuardsInstalled: %v", err)
	}
}

func TestGuardsInstalledRejectsAnUnjustifiedEmptyGuardSet(t *testing.T) {
	saga := migrationRelocationSaga(t)
	guarding := GuardingSection{Version: 1}

	// An empty plan with no in-place authority is the silent path from "no guard
	// was installed" to "no guard was needed".
	unjustified := GuardsInstalledSection{Version: 1, EmptyPlan: true}
	if err := unjustified.Validate(guarding); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("unjustified empty guard set = %v, want a rejection", err)
	}

	identity := migrationIdentity(t, saga.source)
	justified := GuardsInstalledSection{Version: 1, EmptyPlan: true, InPlace: []InPlaceAdoptionRecord{{
		Participant:         "binding:infra",
		SourceIdentity:      identity,
		DestinationIdentity: identity,
	}}}
	if err := justified.Validate(guarding); err != nil {
		t.Fatalf("in-place adoption authority rejected: %v", err)
	}

	// In-place adoption means the exact same aggregate identity on both sides.
	moved := justified
	moved.InPlace = []InPlaceAdoptionRecord{{
		Participant:         "binding:infra",
		SourceIdentity:      identity,
		DestinationIdentity: migrationIdentity(t, saga.destination),
	}}
	if err := moved.Validate(guarding); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("in-place authority across two identities = %v, want a rejection", err)
	}

	// The flag must agree with the recorded plan in both directions.
	mismatch := GuardsInstalledSection{Version: 1, EmptyPlan: false}
	if err := mismatch.Validate(guarding); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("empty plan claimed non-empty = %v, want a rejection", err)
	}
}

func TestCommitDecisionClosesExactlyThePinnedParticipantSet(t *testing.T) {
	saga := migrationRelocationSaga(t)
	record := saga.advanceTo(t, PhaseGuardsInstalled)
	decision, err := NewCommitDecision(record.Intent.Attempt, record.Intent.DesiredGeneration)
	if err != nil {
		t.Fatalf("NewCommitDecision: %v", err)
	}

	for _, testCase := range []struct {
		name         string
		participants []string
	}{
		{"missing participant", nil},
		{"extra participant", append(record.Intent.Participants.Keys(), "binding:somewhere-else")},
		{"renamed participant", []string{"binding:somewhere-else"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			section := CommitDecisionSection{Version: 1, Decision: decision, Participants: testCase.participants}
			if err := section.Validate(record.Intent); !errors.Is(err, ErrInvalidManifest) {
				t.Fatalf("decision over %v = %v, want a rejection", testCase.participants, err)
			}
		})
	}

	wrongGeneration, err := NewCommitDecision(record.Intent.Attempt, record.Intent.DesiredGeneration+1)
	if err != nil {
		t.Fatalf("NewCommitDecision: %v", err)
	}
	section := CommitDecisionSection{Version: 1, Decision: wrongGeneration, Participants: record.Intent.Participants.Keys()}
	if err := section.Validate(record.Intent); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("decision for another generation = %v, want a rejection", err)
	}
}

func TestReceiptsAreIdempotentAndBoundToTheClosedParticipantSet(t *testing.T) {
	saga := migrationRelocationSaga(t)
	record := saga.advanceTo(t, PhaseCommitDecided)
	participant := record.Decision.Participants[0]
	receipt := migrationBindingReceipt(t, participant, saga.destination, record.Intent.Attempt, record.Intent.DesiredGeneration)

	if err := record.EnterReceiptsPersisted(); !errors.Is(err, ErrInvalidPhaseTransition) {
		t.Fatalf("RECEIPTS_PERSISTED with a pending participant = %v, want a rejected transition", err)
	}
	if err := record.AppendReceipt(receipt); err != nil {
		t.Fatalf("AppendReceipt: %v", err)
	}
	if err := record.AppendReceipt(receipt); err != nil {
		t.Fatalf("idempotent AppendReceipt: %v", err)
	}
	if len(record.Receipts) != 1 {
		t.Fatalf("receipts = %d, want one idempotent entry", len(record.Receipts))
	}

	conflicting := receipt
	conflicting.ReceiptID = "receipt-from-another-run"
	if err := record.AppendReceipt(conflicting); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("conflicting receipt = %v, want a blocked conflict", err)
	}
	stranger := migrationBindingReceipt(t, "binding:somewhere-else", saga.destination, record.Intent.Attempt, record.Intent.DesiredGeneration)
	if err := record.AppendReceipt(stranger); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("receipt for an unclosed participant = %v, want a rejection", err)
	}
	if err := record.EnterReceiptsPersisted(); err != nil {
		t.Fatalf("EnterReceiptsPersisted: %v", err)
	}
}

func TestResumePlanMatchesTheCrashTable(t *testing.T) {
	wanted := map[MigrationPhase]struct {
		action    ResumeAction
		authority ResumeAuthority
	}{
		PhaseIntentFsynced:         {ResumeActionReacquirePins, ResumeAuthorityPrior},
		PhasePreparing:             {ResumeActionResumePreparation, ResumeAuthorityPrior},
		PhasePrepared:              {ResumeActionRecensusSource, ResumeAuthorityPrior},
		PhaseGuarding:              {ResumeActionDiscoverGuards, ResumeAuthorityPrior},
		PhaseGuardsInstalled:       {ResumeActionVerifyGuards, ResumeAuthorityPrior},
		PhaseCommitDecided:         {ResumeActionRollForwardReceipts, ResumeAuthorityDesired},
		PhaseReceiptsPersisted:     {ResumeActionRebuildActiveManifest, ResumeAuthorityDesired},
		PhaseActiveManifestDurable: {ResumeActionReopenActive, ResumeAuthorityDesired},
		PhaseActiveOpenPending:     {ResumeActionReopenActive, ResumeAuthorityDesired},
		PhaseActive:                {ResumeActionNormalRestart, ResumeAuthorityDesired},
	}
	for _, phase := range migrationPhases() {
		t.Run(phase.String(), func(t *testing.T) {
			saga := migrationRelocationSaga(t)
			record := saga.advanceTo(t, phase)
			plan, err := record.Resume()
			if err != nil {
				t.Fatalf("Resume: %v", err)
			}
			want := wanted[phase]
			if plan.Action != want.action {
				t.Fatalf("resume action at %s = %s, want %s", phase, plan.Action, want.action)
			}
			if plan.Authority != want.authority {
				t.Fatalf("authority at %s = %s, want %s", phase, plan.Authority, want.authority)
			}
			if plan.Attempt != record.Intent.Attempt || plan.ConfigDigest != record.Intent.ConfigDigest {
				t.Fatal("the resume plan lost the attempt identity or config digest recovery compares against")
			}
			if (plan.Authority == ResumeAuthorityPrior) != phase.SourceAuthoritative() {
				t.Fatalf("authority at %s disagrees with the phase's source authority", phase)
			}
		})
	}
}

func TestResumePlanReportsOnlyMissingReceipts(t *testing.T) {
	saga := migrationRelocationSaga(t)
	record := saga.advanceTo(t, PhaseCommitDecided)

	plan, err := record.Resume()
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(plan.PendingReceipts) != len(record.Decision.Participants) {
		t.Fatalf("pending receipts = %v, want every closed participant", plan.PendingReceipts)
	}
	for _, participant := range record.Decision.Participants {
		receipt := migrationBindingReceipt(t, participant, saga.destination, record.Intent.Attempt, record.Intent.DesiredGeneration)
		if err := record.AppendReceipt(receipt); err != nil {
			t.Fatalf("AppendReceipt: %v", err)
		}
	}
	plan, err = record.Resume()
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(plan.PendingReceipts) != 0 {
		t.Fatalf("pending receipts after every commit = %v, want none", plan.PendingReceipts)
	}
}

func TestClassWitnessRecordRejectsEveryComparisonFailure(t *testing.T) {
	saga := migrationRelocationSaga(t)
	valid := migrationWitnessRecord(t, coordclass.ClassGraph, saga.destination)
	other := canonicalDigest([]byte("a-different-logical-set"))

	for _, testCase := range []struct {
		name  string
		apply func(*ClassWitnessRecord)
	}{
		{"source and destination differ", func(r *ClassWitnessRecord) { r.Source.Digest = other }},
		{"fresh reopen differs", func(r *ClassWitnessRecord) { r.FreshReopen.Digest = other }},
		{"fresh reopen envelope differs", func(r *ClassWitnessRecord) {
			r.FreshReopenEnvelope = migrationEnvelope(t, saga.source, r.Destination.Digest)
		}},
		{"envelope is not bound to the semantic digest", func(r *ClassWitnessRecord) {
			r.AdmittedEnvelope = migrationEnvelope(t, saga.destination, other)
			r.FreshReopenEnvelope = r.AdmittedEnvelope
		}},
		{"contract versions disagree", func(r *ClassWitnessRecord) { r.Source.Contract = "another.contract.v9" }},
		{"class disagrees with the record", func(r *ClassWitnessRecord) { r.Destination.Class = coordclass.ClassOrders }},
		{"a witness names no record family", func(r *ClassWitnessRecord) { r.Destination.Families = nil }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			record := valid.Clone()
			testCase.apply(&record)
			if err := record.Validate(SemanticWitnessAlgorithm); err == nil {
				t.Fatalf("a witness record with %s validated", testCase.name)
			}
		})
	}

	if err := valid.Validate("gascity.storage-semantic-witness.v2"); err == nil {
		t.Fatal("witnesses from one algorithm validated against another; digests across versions never compare")
	}
}

func TestSemanticWitnessesAreNeverRecordedForWork(t *testing.T) {
	witness := SemanticWitness{
		Version:   1,
		Class:     coordclass.ClassWork,
		Contract:  migrationContract,
		Algorithm: SemanticWitnessAlgorithm,
		Digest:    canonicalDigest([]byte("work")),
		Families:  []WitnessFamilyCount{{Name: "beads", Count: 1}},
	}
	if err := validateRecordedWitness(witness); err == nil {
		t.Fatal("a Work semantic witness validated; Gas City never decodes a Work row")
	}
}

func TestWitnessEnvelopeDigestCoversEveryPinnedFact(t *testing.T) {
	saga := migrationRelocationSaga(t)
	digest := canonicalDigest([]byte("semantic"))
	envelope := migrationEnvelope(t, saga.destination, digest)

	for _, testCase := range []struct {
		name  string
		apply func(*WitnessEnvelope)
	}{
		{"a fact is dropped", func(e *WitnessEnvelope) { e.PhysicalFacts = e.PhysicalFacts[:1] }},
		{"a fact value is edited", func(e *WitnessEnvelope) { e.PhysicalFacts[0].Value = "9" }},
		{"a fact is added", func(e *WitnessEnvelope) {
			e.PhysicalFacts = append(e.PhysicalFacts, ComponentPhysicalFact{
				Component: saga.destination.Components[0].ID,
				Kind:      PhysicalFactGraphSeqFloor,
				Value:     "12",
			})
		}},
		{"the semantic digest is swapped", func(e *WitnessEnvelope) { e.SemanticDigest = canonicalDigest([]byte("other")) }},
		{"the identity is swapped", func(e *WitnessEnvelope) { e.Identity = migrationIdentity(t, saga.source) }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			damaged := envelope.Clone()
			testCase.apply(&damaged)
			if err := damaged.Validate(); !errors.Is(err, ErrInvalidWitness) {
				t.Fatalf("an envelope where %s validated: %v", testCase.name, err)
			}
			if envelope.Equal(damaged) {
				t.Fatalf("an envelope where %s compared equal to the admitted one", testCase.name)
			}
		})
	}
}

func TestPreparedSectionRequiresAWitnessForEveryMovingInfrastructureClass(t *testing.T) {
	saga := migrationRelocationSaga(t)
	record := saga.advanceTo(t, PhasePreparing)
	section := saga.preparedSection(t)

	for index := range section.Witnesses {
		short := section
		short.Witnesses = append(append([]ClassWitnessRecord(nil), section.Witnesses[:index]...), section.Witnesses[index+1:]...)
		if err := short.Validate(record.Intent); !errors.Is(err, ErrInvalidManifest) {
			t.Fatalf("PREPARED without a witness for %s = %v, want a rejection", section.Witnesses[index].Class, err)
		}
	}
	if err := section.Validate(record.Intent); err != nil {
		t.Fatalf("the complete prepared section was rejected: %v", err)
	}
}

func TestPreparedSectionRequiresAProviderProofForEveryWorkParticipant(t *testing.T) {
	saga := migrationRelocationSaga(t)
	intent := saga.record.Intent
	participant, err := NewWorkWorkspaceParticipant("task-beads-provider", "work", "hq-physical", []WorkWorkspaceMember{{
		Scope:            HQScope(),
		Prefix:           "hq",
		ConfigContext:    ConfigRefDigest(canonicalDigest([]byte("work-config-context"))),
		Provider:         "task-beads-provider",
		Component:        "work",
		PhysicalIdentity: "hq-physical",
	}})
	if err != nil {
		t.Fatalf("NewWorkWorkspaceParticipant: %v", err)
	}
	intent.Participants.Work = []WorkWorkspaceParticipant{participant}

	section := saga.preparedSection(t)
	if err := section.Validate(intent); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("PREPARED with an unprepared Work participant = %v, want a rejection", err)
	}

	preparation := WorkPreparationIdentity{
		Version:             1,
		Attempt:             intent.Attempt,
		Generation:          intent.DesiredGeneration,
		Direction:           WorkMigrationForward,
		Participant:         participant,
		SourceIdentity:      migrationIdentity(t, saga.source),
		DestinationIdentity: migrationIdentity(t, saga.destination),
		WitnessVersion:      intent.WitnessAlgorithm,
		ConfigDigest:        ConfigRefDigest(canonicalDigest([]byte("work-config-context"))),
	}
	prepared := WorkPrepared{
		Version:            1,
		Attempt:            intent.Attempt,
		Generation:         intent.DesiredGeneration,
		Participant:        participant,
		DescriptorIdentity: preparation.DestinationIdentity,
		Preparation:        preparation,
		Receipt:            "work-prepare-receipt",
	}
	section.WorkPrepared = []WorkPrepared{prepared}
	if err := section.Validate(intent); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("PREPARED with a prepare receipt but no proof = %v, want a rejection", err)
	}

	section.WorkProofs = []WorkProof{{
		Version:            1,
		Attempt:            intent.Attempt,
		Generation:         intent.DesiredGeneration,
		Participant:        participant,
		DescriptorIdentity: preparation.DestinationIdentity,
		Preparation:        preparation,
		PreparedReceipt:    prepared.Receipt,
		Witness:            "opaque-provider-work-witness",
	}}
	if err := section.Validate(intent); err != nil {
		t.Fatalf("a complete Work prepare-and-proof pair was rejected: %v", err)
	}
}

func TestNewActiveManifestRefusesAnythingBeforeCompleteReceipts(t *testing.T) {
	saga := migrationRelocationSaga(t)
	record := saga.advanceTo(t, PhaseCommitDecided)

	if _, err := NewActiveManifest(record, saga.activeDescriptors()); !errors.Is(err, ErrInvalidPhaseTransition) {
		t.Fatalf("active manifest built with a pending receipt = %v, want a rejection", err)
	}

	guarded := migrationRelocationSaga(t)
	guarded.advanceTo(t, PhaseGuardsInstalled)
	if _, err := NewActiveManifest(guarded.record, guarded.activeDescriptors()); !errors.Is(err, ErrInvalidPhaseTransition) {
		t.Fatalf("active manifest built before the decision = %v, want a rejection", err)
	}
}

func TestActiveManifestDescribesEveryAssignedBinding(t *testing.T) {
	saga := migrationRelocationSaga(t)
	saga.advanceTo(t, PhaseReceiptsPersisted)

	if _, err := NewActiveManifest(saga.record, saga.activeDescriptors()[:1]); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("active manifest missing a descriptor for an assigned binding = %v, want a rejection", err)
	}
	manifest, err := NewActiveManifest(saga.record, saga.activeDescriptors())
	if err != nil {
		t.Fatalf("NewActiveManifest: %v", err)
	}
	if manifest.Generation != saga.record.Intent.DesiredGeneration || manifest.CutoverGeneration != manifest.Generation {
		t.Fatalf("active manifest generation = %d/%d, want the desired generation", manifest.Generation, manifest.CutoverGeneration)
	}
	if manifest.RollbackGeneration != saga.record.Intent.PriorGeneration {
		t.Fatalf("rollback generation = %d, want the prior generation %d", manifest.RollbackGeneration, saga.record.Intent.PriorGeneration)
	}
	if len(manifest.Guards) != len(saga.guardPlan) {
		t.Fatalf("active manifest retained %d guard records, want the %d the attempt installed", len(manifest.Guards), len(saga.guardPlan))
	}
	if len(manifest.RetainedSources) != 1 {
		t.Fatalf("active manifest retained %d sources, want the pinned retained source", len(manifest.RetainedSources))
	}
}

func TestActiveManifestDigestCoversItsPinnedIdentities(t *testing.T) {
	saga := migrationRelocationSaga(t)
	saga.advanceTo(t, PhaseActiveManifestDurable)
	baseline, err := saga.manifest.Digest()
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}

	for _, testCase := range []struct {
		name  string
		apply func(*ActiveManifest)
	}{
		{"a class is reassigned", func(m *ActiveManifest) { m.Assignments[coordclass.ClassGraph] = "task-beads" }},
		{"an active descriptor changes", func(m *ActiveManifest) { m.Descriptors[0].Descriptor = saga.source }},
		{"a receipt changes", func(m *ActiveManifest) { m.Receipts[0].ReceiptID = "receipt-from-another-run" }},
		{"a guard record is dropped", func(m *ActiveManifest) { m.Guards = nil }},
		{"the rollback generation changes", func(m *ActiveManifest) { m.RollbackGeneration = 0 }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			mutated := saga.manifest.Clone()
			testCase.apply(mutated)
			digest, err := mutated.Digest()
			if err != nil {
				t.Fatalf("Digest: %v", err)
			}
			if digest == baseline {
				t.Fatalf("the active manifest digest did not change when %s", testCase.name)
			}
		})
	}
}

func TestMigrationPhaseZeroValueIsNotAPhase(t *testing.T) {
	var phase MigrationPhase
	if phase.Valid() || phase.Terminal() || phase.SourceAuthoritative() {
		t.Fatalf("the zero phase claims to be %s", phase)
	}
	var reason PhaseEntryReason
	if reason.Valid() || reason.Return() {
		t.Fatal("the zero entry reason claims to be a recorded transition")
	}
	var residue ResidueKind
	if residue.Valid() {
		t.Fatal("the zero residue kind claims to be recorded evidence")
	}
	var fact PhysicalFactKind
	if fact.Valid() {
		t.Fatal("the zero physical fact kind claims to pin something")
	}
	var action ResumeAction
	if action.Valid() {
		t.Fatal("the zero resume action claims to be a derived route")
	}
}
