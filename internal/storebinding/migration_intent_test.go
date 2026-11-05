package storebinding

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
)

func migrationIntentInputs(t *testing.T, plan *StoragePlan, active *ActiveManifest, discovered ...DiscoveredBinding) StartupInputs {
	t.Helper()
	return StartupInputs{
		Plan:             plan,
		Discovered:       discovered,
		Active:           active,
		WitnessAlgorithm: SemanticWitnessAlgorithm,
	}
}

func migrationMoveKinds(intent MigrationIntent) map[coordclass.Class]ClassMoveKind {
	kinds := make(map[coordclass.Class]ClassMoveKind, len(intent.Moves()))
	for _, move := range intent.Moves() {
		kinds[move.Class] = move.Kind
	}
	return kinds
}

func TestDeriveStartupIntentDerivesGenesisWithMutationFreeEvidence(t *testing.T) {
	plan := migrationPlan(t, "infra")
	descriptor := migrationDescriptor(t, "infra", "infra-provider", migrationAllClasses(t))
	discovered := migrationDiscovered(t, "infra", descriptor, migrationDiscoveryOptions{empty: true})

	intent, err := DeriveStartupIntent(migrationIntentInputs(t, plan, nil, discovered))
	if err != nil {
		t.Fatalf("DeriveStartupIntent: %v", err)
	}
	if intent.Outcome() != IntentOutcomeMigrate {
		t.Fatalf("outcome = %s, want migrate", intent.Outcome())
	}
	if intent.PriorGeneration().Valid() {
		t.Fatalf("prior generation = %d, want the invalid sentinel before any durable generation", intent.PriorGeneration())
	}
	if intent.DesiredGeneration() != 1 {
		t.Fatalf("desired generation = %d, want 1", intent.DesiredGeneration())
	}
	genesis := intent.Genesis()
	if genesis == nil {
		t.Fatal("genesis derivation recorded no mutation-free evidence; INTENT_FSYNCED would have neither a prior generation nor evidence")
	}
	if err := genesis.Validate(); err != nil {
		t.Fatalf("genesis evidence invalid: %v", err)
	}
	for class, kind := range migrationMoveKinds(intent) {
		if kind != ClassMoveGenesis {
			t.Fatalf("class %s move = %s, want genesis", class, kind)
		}
	}
	if intent.Attempt() == "" {
		t.Fatal("a migrate intent derived no attempt identity")
	}
}

func TestDeriveStartupIntentAdoptsPopulatedComponentsInPlace(t *testing.T) {
	plan := migrationPlan(t, "infra")
	descriptor := migrationDescriptor(t, "infra", "infra-provider", migrationAllClasses(t))
	discovered := migrationDiscovered(t, "infra", descriptor, migrationDiscoveryOptions{})

	intent, err := DeriveStartupIntent(migrationIntentInputs(t, plan, nil, discovered))
	if err != nil {
		t.Fatalf("DeriveStartupIntent: %v", err)
	}
	if intent.Outcome() != IntentOutcomeMigrate {
		t.Fatalf("outcome = %s, want migrate", intent.Outcome())
	}
	for class, kind := range migrationMoveKinds(intent) {
		if kind != ClassMoveInPlaceAdoption {
			t.Fatalf("class %s move = %s, want in-place adoption", class, kind)
		}
	}
	// Populated components in place are the prior authority, so there is no
	// mutation-free genesis to claim. Recording genesis here would assert that
	// the bindings held nothing, which is how live data gets overwritten.
	if intent.Genesis() != nil {
		t.Fatal("in-place adoption recorded genesis evidence for populated components")
	}
}

func TestDeriveStartupIntentIsANoOpWhenEveryIdentityMatches(t *testing.T) {
	plan, factories := migrationPlanWithFactories(t, planAllClassesOn("infra"), map[string]config.StorageBindingConfig{
		"infra": {Provider: "infra-provider", Path: ".gc/infra"},
	})
	descriptor := migrationDescriptor(t, "infra", "infra-provider", migrationAllClasses(t))
	active := migrationActiveManifest(t, plan, 5, descriptor)
	discovered := migrationDiscovered(t, "infra", descriptor, migrationDiscoveryOptions{})

	intent, err := DeriveStartupIntent(migrationIntentInputs(t, plan, active, discovered))
	if err != nil {
		t.Fatalf("DeriveStartupIntent: %v", err)
	}
	if intent.Outcome() != IntentOutcomeNoOp {
		t.Fatalf("outcome = %s, want no-op", intent.Outcome())
	}
	if intent.Attempt() != "" {
		t.Fatalf("no-op derived attempt %q; a no-op starts no saga", intent.Attempt())
	}
	if intent.DesiredGeneration() != active.Generation || intent.PriorGeneration() != active.Generation {
		t.Fatalf("no-op advanced the generation from %d to %d", intent.PriorGeneration(), intent.DesiredGeneration())
	}
	if len(intent.VerificationTargets()) != 0 {
		t.Fatalf("no-op queued verification targets: %v", intent.VerificationTargets())
	}
	for _, factory := range factories {
		if factory.calls() != 0 {
			t.Fatal("intent derivation called a provider operation; intent derivation derives and records, it never inspects or opens")
		}
	}
}

func TestDeriveStartupIntentNeverCountsIncompleteInspectionAsIdentityEquality(t *testing.T) {
	plan := migrationPlan(t, "infra")
	descriptor := migrationDescriptor(t, "infra", "infra-provider", migrationAllClasses(t))
	active := migrationActiveManifest(t, plan, 5, descriptor)
	// The same binding, the same physical identity — but the mutation-free
	// inspection could not prove an aggregate descriptor (a live WAL, per the
	// the specification). Everything else matches, so a derivation that treats absence of
	// evidence as evidence returns the no-op that opens without verifying.
	discovered := migrationDiscovered(t, "infra", descriptor, migrationDiscoveryOptions{incomplete: true})

	intent, err := DeriveStartupIntent(migrationIntentInputs(t, plan, active, discovered))
	if err != nil {
		t.Fatalf("DeriveStartupIntent: %v", err)
	}
	if intent.Outcome() == IntentOutcomeNoOp {
		t.Fatal("an incomplete active inspection derived a no-op; incompleteness never counts as identity equality")
	}
	if intent.Outcome() != IntentOutcomeVerifyActive {
		t.Fatalf("outcome = %s, want verify-active", intent.Outcome())
	}
	targets := intent.VerificationTargets()
	if len(targets) != 1 || targets[0].Name != "infra" || targets[0].Role != TargetRoleDestination {
		t.Fatalf("verification targets = %#v, want the one active target that must be fenced and inspected", targets)
	}
}

func TestDeriveStartupIntentMigratesOnConfigurationChangeAlone(t *testing.T) {
	plan := migrationPlan(t, "infra")
	descriptor := migrationDescriptor(t, "infra", "infra-provider", migrationAllClasses(t))
	active := migrationActiveManifest(t, plan, 5, descriptor)
	// Same binding, same physical identity, different secret-free configuration.
	// The specification authorizes migration from configuration alone, so this must not
	// collapse into the unchanged no-op.
	active.ConfigDigest = CompositeConfigDigest(canonicalDigest([]byte("a-previous-configuration")))
	if err := active.Validate(); err != nil {
		t.Fatalf("reconfigured active manifest invalid: %v", err)
	}
	discovered := migrationDiscovered(t, "infra", descriptor, migrationDiscoveryOptions{})

	intent, err := DeriveStartupIntent(migrationIntentInputs(t, plan, active, discovered))
	if err != nil {
		t.Fatalf("DeriveStartupIntent: %v", err)
	}
	if intent.Outcome() != IntentOutcomeMigrate {
		t.Fatalf("outcome = %s, want migrate on a changed configuration digest", intent.Outcome())
	}
	for class, kind := range migrationMoveKinds(intent) {
		if kind != ClassMoveReconfigure {
			t.Fatalf("class %s move = %s, want reconfigure", class, kind)
		}
	}
	if intent.DesiredGeneration() != active.Generation+1 {
		t.Fatalf("desired generation = %d, want %d", intent.DesiredGeneration(), active.Generation+1)
	}
	if intent.ConfigDigest() != plan.ConfigDigest() {
		t.Fatal("the intent recorded a config digest that is not the frozen plan's")
	}
}

func TestDeriveStartupIntentRelocatesAndPinsBothSides(t *testing.T) {
	previous := migrationPlan(t, "infra")
	plan := migrationPlan(t, "next")
	source := migrationDescriptor(t, "infra", "infra-provider", migrationAllClasses(t))
	destination := migrationDescriptor(t, "next", "infra-provider", migrationAllClasses(t))
	active := migrationActiveManifest(t, previous, 5, source)

	intent, err := DeriveStartupIntent(migrationIntentInputs(t, plan, active,
		migrationDiscovered(t, "infra", source, migrationDiscoveryOptions{}),
		migrationDiscovered(t, "next", destination, migrationDiscoveryOptions{empty: true}),
	))
	if err != nil {
		t.Fatalf("DeriveStartupIntent: %v", err)
	}
	if intent.Outcome() != IntentOutcomeMigrate {
		t.Fatalf("outcome = %s, want migrate", intent.Outcome())
	}
	for class, kind := range migrationMoveKinds(intent) {
		if kind != ClassMoveRelocate {
			t.Fatalf("class %s move = %s, want relocate", class, kind)
		}
	}
	roles := make(map[BindingName]map[TargetRole]bool)
	for _, target := range intent.FenceTargets() {
		if roles[target.Name] == nil {
			roles[target.Name] = map[TargetRole]bool{}
		}
		roles[target.Name][target.Role] = true
	}
	if !roles["infra"][TargetRoleSource] || !roles["infra"][TargetRoleRetained] {
		t.Fatalf("source roles = %v, want the prior binding pinned as both source and retained", roles["infra"])
	}
	if !roles["next"][TargetRoleDestination] {
		t.Fatalf("destination roles = %v, want the desired binding pinned as destination", roles["next"])
	}
}

func TestDeriveStartupIntentDerivesReverseAgainstARetainedSource(t *testing.T) {
	previous := migrationPlan(t, "next")
	plan := migrationPlan(t, "infra")
	retainedDescriptor := migrationDescriptor(t, "infra", "infra-provider", migrationAllClasses(t))
	current := migrationDescriptor(t, "next", "infra-provider", migrationAllClasses(t))
	retained := migrationRetainedSource(t, retainedDescriptor, SemanticWitnessAlgorithm, canonicalDigest([]byte("retained-witness")))
	active := migrationActiveManifest(t, previous, 6, current, retained)

	intent, err := DeriveStartupIntent(migrationIntentInputs(t, plan, active,
		migrationDiscovered(t, "infra", retainedDescriptor, migrationDiscoveryOptions{}),
		migrationDiscovered(t, "next", current, migrationDiscoveryOptions{}),
	))
	if err != nil {
		t.Fatalf("DeriveStartupIntent: %v", err)
	}
	for class, kind := range migrationMoveKinds(intent) {
		if kind != ClassMoveReverse {
			t.Fatalf("class %s move = %s, want reverse against the retained source", class, kind)
		}
	}
}

func TestDeriveStartupIntentBlocksWhenADesiredBindingWasNotInspected(t *testing.T) {
	plan := migrationPlan(t, "infra")

	intent, err := DeriveStartupIntent(migrationIntentInputs(t, plan, nil))
	if !errors.Is(err, ErrMigrationBlocked) {
		t.Fatalf("error = %v, want a startup block", err)
	}
	if intent.Outcome() != IntentOutcomeBlocked {
		t.Fatalf("outcome = %s, want blocked", intent.Outcome())
	}
	report := intent.Blocked()
	if report == nil || report.Reason != BlockReasonIncomplete {
		t.Fatalf("block report = %#v, want an incomplete-state block", report)
	}
}

func TestDeriveStartupIntentReportsBlockThroughBothChannels(t *testing.T) {
	plan := migrationPlan(t, "infra")

	intent, err := DeriveStartupIntent(migrationIntentInputs(t, plan, nil))
	// A caller that reads only the error must stop, and so must one that reads
	// only the outcome. Either channel alone has to be sufficient.
	if err == nil {
		t.Fatal("a blocked derivation returned no error")
	}
	if intent.Outcome() != IntentOutcomeBlocked || intent.Blocked() == nil {
		t.Fatalf("blocked intent = %#v, want an outcome and a typed report", intent)
	}
	if intent.Outcome().RequiresSaga() {
		t.Fatal("a blocked outcome claims to start a saga")
	}
}

func TestDeriveStartupIntentRejectsADiscoveryWithNoPopulationVerdict(t *testing.T) {
	plan := migrationPlan(t, "infra")
	descriptor := migrationDescriptor(t, "infra", "infra-provider", migrationAllClasses(t))
	discovered := migrationDiscovered(t, "infra", descriptor, migrationDiscoveryOptions{})
	// An inspector that did not answer must not read as "empty": that single
	// default is how a populated legacy store gets silently overwritten.
	discovered.Population = BindingPopulationUnknown

	intent, err := DeriveStartupIntent(migrationIntentInputs(t, plan, nil, discovered))
	if !errors.Is(err, ErrInvalidBindingDiscovery) {
		t.Fatalf("error = %v, want an invalid-discovery rejection", err)
	}
	if intent.Outcome().Valid() {
		t.Fatalf("outcome = %s, want the underived zero value", intent.Outcome())
	}
}

func TestDeriveStartupIntentResumesARecordedAttemptFromItsOwnFields(t *testing.T) {
	plan := migrationPlan(t, "infra")
	descriptor := migrationDescriptor(t, "infra", "infra-provider", migrationAllClasses(t))
	discovered := migrationDiscovered(t, "infra", descriptor, migrationDiscoveryOptions{})
	derived, err := DeriveStartupIntent(migrationIntentInputs(t, plan, nil, discovered))
	if err != nil {
		t.Fatalf("DeriveStartupIntent: %v", err)
	}
	record, err := NewAttemptRecord(derived)
	if err != nil {
		t.Fatalf("NewAttemptRecord: %v", err)
	}

	// A restart discovers nothing at all; the record alone must carry the saga.
	resumed, err := DeriveStartupIntent(StartupInputs{Plan: plan, Attempt: record, WitnessAlgorithm: SemanticWitnessAlgorithm})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.Outcome() != IntentOutcomeMigrate {
		t.Fatalf("resumed outcome = %s, want migrate", resumed.Outcome())
	}
	if !resumed.Resumed() || resumed.ResumedFrom() != PhaseIntentFsynced {
		t.Fatalf("resumed from %s (resumed=%v), want INTENT_FSYNCED", resumed.ResumedFrom(), resumed.Resumed())
	}
	if resumed.Attempt() != derived.Attempt() {
		t.Fatalf("resume minted attempt %q, want the recorded %q", resumed.Attempt(), derived.Attempt())
	}
	if len(resumed.FenceTargets()) != len(derived.FenceTargets()) {
		t.Fatalf("resumed %d pinned targets, recorded %d", len(resumed.FenceTargets()), len(derived.FenceTargets()))
	}
}

func TestDeriveStartupIntentBlocksResumeOnAStaleConfigDigest(t *testing.T) {
	plan := migrationPlan(t, "infra")
	descriptor := migrationDescriptor(t, "infra", "infra-provider", migrationAllClasses(t))
	derived, err := DeriveStartupIntent(migrationIntentInputs(t, plan, nil, migrationDiscovered(t, "infra", descriptor, migrationDiscoveryOptions{})))
	if err != nil {
		t.Fatalf("DeriveStartupIntent: %v", err)
	}
	record, err := NewAttemptRecord(derived)
	if err != nil {
		t.Fatalf("NewAttemptRecord: %v", err)
	}

	// Configuration moved on while the saga was unfinished. Recovery must block
	// and report the recorded generation, not re-resolve through binding names.
	other := migrationPlan(t, "next")
	intent, err := DeriveStartupIntent(StartupInputs{Plan: other, Attempt: record, WitnessAlgorithm: SemanticWitnessAlgorithm})
	if !errors.Is(err, ErrMigrationBlocked) {
		t.Fatalf("error = %v, want a startup block", err)
	}
	report := intent.Blocked()
	if report == nil || report.Reason != BlockReasonStale {
		t.Fatalf("block report = %#v, want a stale-config block", report)
	}
	if report.RecordedGeneration != record.Intent.DesiredGeneration {
		t.Fatalf("reported generation = %d, want the recorded %d", report.RecordedGeneration, record.Intent.DesiredGeneration)
	}
}

func TestDeriveStartupIntentBlocksACompletedAttemptWithNoActiveManifest(t *testing.T) {
	saga := migrationCompletedAttempt(t)

	// The attempt says the desired generation is authoritative and published.
	// With no active manifest, deriving fresh intent would classify the live
	// bindings as adoption and migrate over a completed generation.
	intent, err := DeriveStartupIntent(StartupInputs{
		Plan:             saga.plan,
		Discovered:       saga.discovered,
		Attempt:          saga.record,
		WitnessAlgorithm: SemanticWitnessAlgorithm,
	})
	if !errors.Is(err, ErrMigrationBlocked) {
		t.Fatalf("error = %v, want a startup block", err)
	}
	if report := intent.Blocked(); report == nil || report.Reason != BlockReasonConflicting {
		t.Fatalf("block report = %#v, want a conflicting-state block", report)
	}
}

func TestDeriveStartupIntentBlocksAnAttemptWhosePriorGenerationIsNotActive(t *testing.T) {
	plan := migrationPlan(t, "infra")
	descriptor := migrationDescriptor(t, "infra", "infra-provider", migrationAllClasses(t))
	active := migrationActiveManifest(t, plan, 5, descriptor)
	active.ConfigDigest = CompositeConfigDigest(canonicalDigest([]byte("a-previous-configuration")))
	derived, err := DeriveStartupIntent(migrationIntentInputs(t, plan, active, migrationDiscovered(t, "infra", descriptor, migrationDiscoveryOptions{})))
	if err != nil {
		t.Fatalf("DeriveStartupIntent: %v", err)
	}
	record, err := NewAttemptRecord(derived)
	if err != nil {
		t.Fatalf("NewAttemptRecord: %v", err)
	}

	// The durable active manifest is a different generation than the one the
	// attempt recorded as prior. Resuming would prepare against a source that is
	// no longer the authority.
	moved := migrationActiveManifest(t, plan, 9, descriptor)
	intent, err := DeriveStartupIntent(StartupInputs{Plan: plan, Active: moved, Attempt: record, WitnessAlgorithm: SemanticWitnessAlgorithm})
	if !errors.Is(err, ErrMigrationBlocked) {
		t.Fatalf("error = %v, want a startup block", err)
	}
	if report := intent.Blocked(); report == nil || report.Reason != BlockReasonConflicting {
		t.Fatalf("block report = %#v, want a conflicting-state block", report)
	}
}

func TestDeriveStartupIntentRejectsInputsWithNoWitnessAlgorithm(t *testing.T) {
	plan := migrationPlan(t, "infra")
	descriptor := migrationDescriptor(t, "infra", "infra-provider", migrationAllClasses(t))

	intent, err := DeriveStartupIntent(StartupInputs{
		Plan:       plan,
		Discovered: []DiscoveredBinding{migrationDiscovered(t, "infra", descriptor, migrationDiscoveryOptions{})},
	})
	if !errors.Is(err, ErrInvalidMigrationIntent) {
		t.Fatalf("error = %v, want an invalid-intent rejection", err)
	}
	if intent.Outcome().Valid() {
		t.Fatalf("outcome = %s, want the underived zero value", intent.Outcome())
	}
}

func TestIntentOutcomeZeroValueIsNotADecision(t *testing.T) {
	var outcome IntentOutcome
	if outcome.Valid() || outcome.RequiresSaga() || outcome.String() != "underived" {
		t.Fatalf("zero outcome = %s (valid=%v), want an underived non-decision", outcome, outcome.Valid())
	}
	var population BindingPopulation
	if population.Valid() || population.String() != "unknown" {
		t.Fatalf("zero population = %s, want an unknown verdict that blocks", population)
	}
	var kind ClassMoveKind
	if kind.Valid() || kind.RequiresSaga() {
		t.Fatal("the zero move kind claims to be a derived move")
	}
	var reason BlockReason
	if reason.Valid() {
		t.Fatal("the zero block reason claims to belong to the taxonomy")
	}
}
