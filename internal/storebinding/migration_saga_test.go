package storebinding

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// migrationSaga is a complete relocation of the five infrastructure classes from
// "infra" to "next" while Work stays put. It exists so a test can stop the saga
// at any phase boundary and prove what the durable record does and does not say
// at that point.
type migrationSaga struct {
	plan        *StoragePlan
	source      Descriptor
	destination Descriptor
	work        Descriptor
	active      *ActiveManifest
	discovered  []DiscoveredBinding
	intent      MigrationIntent
	record      *AttemptRecord
	retained    RetainedSourceRef
	guardPlan   []GuardInstallRequest
	manifest    *ActiveManifest
}

func migrationRelocationSaga(t *testing.T) *migrationSaga {
	t.Helper()
	infraClasses := migrationInfraClasses(t)
	workClasses := planClassSet(t, coordclass.ClassWork)

	classes := map[coordclass.Class]string{coordclass.ClassWork: "task-beads"}
	for _, class := range infraClasses.Classes() {
		classes[class] = "next"
	}
	plan := migrationPlanFor(t, classes, map[string]config.StorageBindingConfig{
		"task-beads": {Provider: "task-beads-provider", ConfigRef: "work-production"},
		"next":       {Provider: "infra-provider", Path: ".gc/next"},
	})

	saga := &migrationSaga{
		plan:        plan,
		source:      migrationDescriptor(t, "infra", "infra-provider", infraClasses),
		destination: migrationDescriptor(t, "next", "infra-provider", infraClasses),
		work:        migrationDescriptor(t, "task-beads", "task-beads-provider", workClasses),
	}
	saga.retained = migrationRetainedSource(t, saga.source, SemanticWitnessAlgorithm, canonicalDigest([]byte("source-witness")))

	priorAssignments := map[coordclass.Class]BindingName{coordclass.ClassWork: "task-beads"}
	for _, class := range infraClasses.Classes() {
		priorAssignments[class] = "infra"
	}
	// Generation 5 itself retained a source from the generation before it. The
	// locator is carried into INTENT_FSYNCED so recovery can reacquire it
	// without re-inspection.
	legacy := migrationRetainedSource(t, migrationDescriptor(t, "legacy", "infra-provider", infraClasses), SemanticWitnessAlgorithm, canonicalDigest([]byte("legacy-witness")))
	saga.active = migrationActive(t, migrationActiveOptions{
		generation:  5,
		attempt:     "attempt-prior-generation",
		assignments: priorAssignments,
		descriptors: []NamedDescriptor{
			{Name: "infra", Descriptor: saga.source},
			{Name: "task-beads", Descriptor: saga.work},
		},
		retained: []RetainedSourceRef{legacy},
		// The composite configuration is unchanged; only the class assignment
		// moved, so Work stays an unchanged class and the saga has exactly one
		// binding participant.
		configDigest:   plan.ConfigDigest(),
		bindingDigests: plan.BindingConfigDigests(),
	})
	saga.discovered = []DiscoveredBinding{
		migrationDiscovered(t, "infra", saga.source, migrationDiscoveryOptions{}),
		migrationDiscovered(t, "next", saga.destination, migrationDiscoveryOptions{empty: true}),
		migrationDiscovered(t, "task-beads", saga.work, migrationDiscoveryOptions{}),
	}

	intent, err := DeriveStartupIntent(StartupInputs{
		Plan:             plan,
		Discovered:       saga.discovered,
		Active:           saga.active,
		WitnessAlgorithm: SemanticWitnessAlgorithm,
	})
	if err != nil {
		t.Fatalf("migrationRelocationSaga derive: %v", err)
	}
	if intent.Outcome() != IntentOutcomeMigrate {
		t.Fatalf("migrationRelocationSaga outcome = %s, want migrate", intent.Outcome())
	}
	saga.intent = intent

	record, err := NewAttemptRecord(intent)
	if err != nil {
		t.Fatalf("NewAttemptRecord: %v", err)
	}
	saga.record = record
	saga.guardPlan = []GuardInstallRequest{{
		Attempt:          intent.Attempt(),
		Generation:       intent.DesiredGeneration(),
		Source:           saga.retained,
		Component:        saga.source.Components[0].ID,
		PhysicalIdentity: saga.source.Components[0].PhysicalIdentity,
		Role:             GuardRoleDenyWrite,
	}}
	return saga
}

func (s *migrationSaga) preparingSection(t *testing.T) PreparingSection {
	t.Helper()
	return migrationPreparingSection(t, "infra", s.source)
}

func (s *migrationSaga) preparedSection(t *testing.T) PreparedSection {
	t.Helper()
	section := PreparedSection{
		Version:                1,
		DestinationDescriptors: []NamedDescriptor{{Name: "next", Descriptor: s.destination}},
		RetainedSources:        []RetainedSourceRef{s.retained},
	}
	for _, class := range migrationInfraClasses(t).Classes() {
		section.Witnesses = append(section.Witnesses, migrationWitnessRecord(t, class, s.destination))
	}
	if err := section.Validate(s.record.Intent); err != nil {
		t.Fatalf("preparedSection: %v", err)
	}
	return section
}

func (s *migrationSaga) activeDescriptors() []NamedDescriptor {
	return []NamedDescriptor{
		{Name: "next", Descriptor: s.destination},
		{Name: "task-beads", Descriptor: s.work},
	}
}

// advanceTo drives the record forward through every phase boundary up to the
// requested phase, exactly as the later saga steps would.
func (s *migrationSaga) advanceTo(t *testing.T, phase MigrationPhase) *AttemptRecord {
	t.Helper()
	if phase < PhaseIntentFsynced || phase > PhaseActive {
		t.Fatalf("advanceTo(%s): not a phase", phase)
	}
	if phase >= PhasePreparing {
		if err := s.record.EnterPreparing(s.preparingSection(t), PhaseEntryInitial); err != nil {
			t.Fatalf("EnterPreparing: %v", err)
		}
	}
	if phase >= PhasePrepared {
		if err := s.record.EnterPrepared(s.preparedSection(t)); err != nil {
			t.Fatalf("EnterPrepared: %v", err)
		}
	}
	if phase >= PhaseGuarding {
		if err := s.record.EnterGuarding(GuardingSection{Version: 1, Plan: s.guardPlan}); err != nil {
			t.Fatalf("EnterGuarding: %v", err)
		}
	}
	if phase >= PhaseGuardsInstalled {
		verified := make([]GuardReceipt, 0, len(s.guardPlan))
		for _, request := range s.guardPlan {
			receipt := migrationGuardReceipt(t, request, s.source.SemanticContractVersion)
			if err := s.record.AppendGuardReceipt(receipt); err != nil {
				t.Fatalf("AppendGuardReceipt: %v", err)
			}
			verified = append(verified, receipt)
		}
		if err := s.record.EnterGuardsInstalled(GuardsInstalledSection{Version: 1, Verified: verified}); err != nil {
			t.Fatalf("EnterGuardsInstalled: %v", err)
		}
	}
	if phase >= PhaseCommitDecided {
		decision, err := NewCommitDecision(s.record.Intent.Attempt, s.record.Intent.DesiredGeneration)
		if err != nil {
			t.Fatalf("NewCommitDecision: %v", err)
		}
		section := CommitDecisionSection{Version: 1, Decision: decision, Participants: s.record.Intent.Participants.Keys()}
		if err := s.record.EnterCommitDecided(section); err != nil {
			t.Fatalf("EnterCommitDecided: %v", err)
		}
	}
	if phase >= PhaseReceiptsPersisted {
		for _, participant := range s.record.Decision.Participants {
			receipt := migrationBindingReceipt(t, participant, s.destination, s.record.Intent.Attempt, s.record.Intent.DesiredGeneration)
			if err := s.record.AppendReceipt(receipt); err != nil {
				t.Fatalf("AppendReceipt(%q): %v", participant, err)
			}
		}
		if err := s.record.EnterReceiptsPersisted(); err != nil {
			t.Fatalf("EnterReceiptsPersisted: %v", err)
		}
	}
	if phase >= PhaseActiveManifestDurable {
		manifest, err := NewActiveManifest(s.record, s.activeDescriptors())
		if err != nil {
			t.Fatalf("NewActiveManifest: %v", err)
		}
		digest, err := manifest.Digest()
		if err != nil {
			t.Fatalf("ActiveManifest.Digest: %v", err)
		}
		s.manifest = manifest
		if err := s.record.EnterActiveManifestDurable(digest); err != nil {
			t.Fatalf("EnterActiveManifestDurable: %v", err)
		}
	}
	if phase >= PhaseActiveOpenPending {
		if err := s.record.EnterActiveOpenPending(); err != nil {
			t.Fatalf("EnterActiveOpenPending: %v", err)
		}
	}
	if phase >= PhaseActive {
		if err := s.record.EnterActive(); err != nil {
			t.Fatalf("EnterActive: %v", err)
		}
	}
	if s.record.Phase != phase {
		t.Fatalf("advanceTo(%s) left the record at %s", phase, s.record.Phase)
	}
	return s.record
}

// migrationCompletedAttempt returns a record that reached ACTIVE.
func migrationCompletedAttempt(t *testing.T) *migrationSaga {
	t.Helper()
	saga := migrationRelocationSaga(t)
	saga.advanceTo(t, PhaseActive)
	return saga
}
