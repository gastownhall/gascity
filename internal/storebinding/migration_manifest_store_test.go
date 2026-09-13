package storebinding

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// migrationMaximalRecord drives one saga through every phase boundary the migration specification
// defines, including a durable return to PREPARING, so the resulting record
// carries a value in every durable field: a multi-entry journal, destination
// residue, a released guard, a staged generation, witnesses, guard receipts, a
// decision, participant receipts, and the pinned active-manifest digest.
func migrationMaximalRecord(t *testing.T) *migrationSaga {
	t.Helper()
	saga := migrationRelocationSaga(t)
	record := saga.record
	if err := record.EnterPreparing(saga.preparingSection(t), PhaseEntryInitial); err != nil {
		t.Fatalf("EnterPreparing: %v", err)
	}
	if err := record.RecordDestinationResidue(DestinationResidue{
		Binding:          "next",
		Component:        saga.destination.Components[0].ID,
		PhysicalIdentity: saga.destination.Components[0].PhysicalIdentity,
		Kind:             ResidueWritten,
	}); err != nil {
		t.Fatalf("RecordDestinationResidue: %v", err)
	}
	staged := saga.preparedSection(t)
	staged.StagedGeneration = &NamedDescriptor{Name: "next", Descriptor: saga.destination}
	if err := record.EnterPrepared(staged); err != nil {
		t.Fatalf("EnterPrepared: %v", err)
	}
	if err := record.EnterGuarding(GuardingSection{Version: 1, Plan: saga.guardPlan}); err != nil {
		t.Fatalf("EnterGuarding: %v", err)
	}
	receipt := migrationGuardReceipt(t, saga.guardPlan[0], saga.source.SemanticContractVersion)
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
	if err := record.EnterPrepared(staged); err != nil {
		t.Fatalf("EnterPrepared after the return: %v", err)
	}
	if err := record.EnterGuarding(GuardingSection{Version: 1, Plan: saga.guardPlan}); err != nil {
		t.Fatalf("EnterGuarding after the return: %v", err)
	}
	if err := record.AppendGuardReceipt(receipt); err != nil {
		t.Fatalf("AppendGuardReceipt after the return: %v", err)
	}
	if err := record.EnterGuardsInstalled(GuardsInstalledSection{Version: 1, Verified: []GuardReceipt{receipt}}); err != nil {
		t.Fatalf("EnterGuardsInstalled: %v", err)
	}
	decision, err := NewCommitDecision(record.Intent.Attempt, record.Intent.DesiredGeneration)
	if err != nil {
		t.Fatalf("NewCommitDecision: %v", err)
	}
	if err := record.EnterCommitDecided(CommitDecisionSection{Version: 1, Decision: decision, Participants: record.Intent.Participants.Keys()}); err != nil {
		t.Fatalf("EnterCommitDecided: %v", err)
	}
	for _, participant := range record.Decision.Participants {
		if err := record.AppendReceipt(migrationBindingReceipt(t, participant, saga.destination, record.Intent.Attempt, record.Intent.DesiredGeneration)); err != nil {
			t.Fatalf("AppendReceipt: %v", err)
		}
	}
	if err := record.EnterReceiptsPersisted(); err != nil {
		t.Fatalf("EnterReceiptsPersisted: %v", err)
	}
	manifest, err := NewActiveManifest(record, saga.activeDescriptors())
	if err != nil {
		t.Fatalf("NewActiveManifest: %v", err)
	}
	saga.manifest = manifest
	digest, err := manifest.Digest()
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if err := record.EnterActiveManifestDurable(digest); err != nil {
		t.Fatalf("EnterActiveManifestDurable: %v", err)
	}
	if err := record.EnterActiveOpenPending(); err != nil {
		t.Fatalf("EnterActiveOpenPending: %v", err)
	}
	if err := record.EnterActive(); err != nil {
		t.Fatalf("EnterActive: %v", err)
	}
	return saga
}

// zeroFieldExemptions are the durable fields that are zero at every occurrence
// in the maximal fixture. Each is a mutually exclusive alternative that another
// test covers, so exempting it leaves nothing unwitnessed.
var zeroFieldExemptions = map[string]string{
	// Genesis evidence exists only when no prior generation does; the genesis
	// round trip is TestManifestCodecRoundTripsGenesisEvidence.
	"IntentSection.Genesis": "mutually exclusive with a prior generation",
	// Only a guard in the transfer role carries transfer fields and an active
	// proof; a forward deny-write guard must leave them empty (GuardReceipt.Validate).
	"GuardReceipt.TransferState":               "forward deny-write guards carry no transfer state",
	"GuardReceipt.TransferParticipant":         "forward deny-write guards carry no transfer state",
	"GuardReceipt.TransferDestinationIdentity": "forward deny-write guards carry no transfer state",
	"GuardReceipt.TransferReceiptKind":         "forward deny-write guards carry no transfer state",
	"GuardReceipt.ActiveProof":                 "forward deny-write guards carry no transfer state",
	// A binding-activation receipt must reject Work provenance (ParticipantReceipt.Validate).
	"ParticipantReceipt.Preparation":     "binding activation receipts carry no Work provenance",
	"ParticipantReceipt.PreparedReceipt": "binding activation receipts carry no Work provenance",
	// An active descriptor is not itself a retained source; RetainedSourceRef is
	// witnessed through the intent, prepared, and active-manifest lists.
	"Descriptor.RetainedSource": "an active descriptor is not a retained source",
	// The Work class does not move in this saga, so no Work participant, prepare
	// receipt, or proof exists. Those are covered by
	// TestPreparedSectionRequiresAProviderProofForEveryWorkParticipant and the
	// Work scope round trip by TestManifestCodecPreservesClassSetsAndWorkScopes.
	"ParticipantSet.Work":          "Work does not move in this saga",
	"PreparedSection.WorkPrepared": "Work does not move in this saga",
	"PreparedSection.WorkProofs":   "Work does not move in this saga",
	"ActiveManifest.WorkProofs":    "Work does not move in this saga",
	// A non-empty guard plan cannot also claim in-place authority; the empty-plan
	// case is TestGuardsInstalledRejectsAnUnjustifiedEmptyGuardSet.
	"GuardsInstalledSection.InPlace":   "a non-empty guard plan cannot claim in-place authority",
	"GuardsInstalledSection.EmptyPlan": "this saga installs a real guard plan",
	// A recorded duplicate residence is rejected outright by ClassInventory.Validate.
	"ClassInventory.DuplicateResidence": "a duplicate residence blocks startup",
}

// fieldWitness records, for every durable field, whether any occurrence of it
// anywhere in the record carried a non-zero value.
type fieldWitness struct {
	seen      map[string]bool
	witnessed map[string]bool
}

func newFieldWitness() *fieldWitness {
	return &fieldWitness{seen: map[string]bool{}, witnessed: map[string]bool{}}
}

// walk keys by type-qualified field name and aggregates across every occurrence.
// It is per-field rather than per-occurrence because some zero values are the
// real value at their occurrence: coordclass.ClassWork is the zero Class, an HQ
// scope names no rig, and a class set legitimately excludes a class. One
// non-zero occurrence anywhere proves the codec carries the field; none anywhere
// means the round-trip comparison could not have witnessed it.
func (w *fieldWitness) walk(value reflect.Value, key string) {
	if key != "" {
		w.seen[key] = true
		if !value.IsZero() {
			w.witnessed[key] = true
		}
	}
	switch value.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !value.IsNil() {
			w.walk(value.Elem(), "")
		}
	case reflect.Struct:
		// A zero struct's fields say nothing; the struct itself is already
		// reported, so descending would only manufacture unreachable keys.
		if value.IsZero() {
			return
		}
		name := value.Type().Name()
		for index := 0; index < value.NumField(); index++ {
			w.walk(value.Field(index), name+"."+value.Type().Field(index).Name)
		}
	case reflect.Slice:
		// A non-nil empty slice is not a zero value to reflect, but it carries
		// nothing, so it must not count as a witness.
		if value.Len() == 0 {
			delete(w.witnessed, key)
		}
		for index := 0; index < value.Len(); index++ {
			w.walk(value.Index(index), "")
		}
	case reflect.Map:
		if value.Len() == 0 {
			delete(w.witnessed, key)
		}
		for _, mapKey := range value.MapKeys() {
			w.walk(value.MapIndex(mapKey), "")
		}
	}
}

func (w *fieldWitness) report(t *testing.T) {
	t.Helper()
	keys := make([]string, 0, len(w.seen))
	for key := range w.seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if w.witnessed[key] {
			continue
		}
		if _, exempt := zeroFieldExemptions[key]; exempt {
			continue
		}
		t.Errorf("durable field %s is zero at every occurrence; the round-trip comparison cannot witness it", key)
	}
	for key := range zeroFieldExemptions {
		if w.witnessed[key] {
			t.Errorf("durable field %s is exempted as always-zero but the fixture sets it; drop the exemption", key)
		}
	}
}

func TestManifestCodecPreservesEveryDurableField(t *testing.T) {
	saga := migrationMaximalRecord(t)

	witness := newFieldWitness()
	witness.walk(reflect.ValueOf(saga.record).Elem(), "")
	witness.walk(reflect.ValueOf(saga.manifest).Elem(), "")
	witness.report(t)
	if t.Failed() {
		t.Fatalf("the maximal fixture is not maximal; fix it before trusting the round trip")
	}

	payload, err := encodeAttemptRecord(saga.record)
	if err != nil {
		t.Fatalf("encodeAttemptRecord: %v", err)
	}
	decoded, err := decodeAttemptRecord(payload)
	if err != nil {
		t.Fatalf("decodeAttemptRecord: %v", err)
	}
	if !reflect.DeepEqual(saga.record, decoded) {
		t.Fatalf("the decoded attempt record is not the encoded one:\n durable: %#v\n decoded: %#v", saga.record, decoded)
	}

	manifestPayload, err := encodeActiveManifest(saga.manifest)
	if err != nil {
		t.Fatalf("encodeActiveManifest: %v", err)
	}
	decodedManifest, err := decodeActiveManifest(manifestPayload)
	if err != nil {
		t.Fatalf("decodeActiveManifest: %v", err)
	}
	if !reflect.DeepEqual(saga.manifest, decodedManifest) {
		t.Fatalf("the decoded active manifest is not the encoded one:\n durable: %#v\n decoded: %#v", saga.manifest, decodedManifest)
	}
}

func TestManifestCodecPreservesClassSetsAndWorkScopes(t *testing.T) {
	saga := migrationMaximalRecord(t)
	payload, err := encodeAttemptRecord(saga.record)
	if err != nil {
		t.Fatalf("encodeAttemptRecord: %v", err)
	}
	decoded, err := decodeAttemptRecord(payload)
	if err != nil {
		t.Fatalf("decodeAttemptRecord: %v", err)
	}

	// ClassSet members are unexported. A reflective encoder would write them as
	// an empty object and read them back empty, and every affected value would
	// still be a well-formed record — just one that fences nothing.
	for index, target := range decoded.Intent.FenceTargets {
		want := saga.record.Intent.FenceTargets[index].Target.Classes
		if target.Target.Classes.Empty() || !target.Target.Classes.Equal(want) {
			t.Fatalf("decoded fence target %q covers %v, want %v", target.Name, target.Target.Classes.Classes(), want.Classes())
		}
	}
	for index, participant := range decoded.Intent.Participants.Bindings {
		want := saga.record.Intent.Participants.Bindings[index].Classes
		if participant.Classes.Empty() || !participant.Classes.Equal(want) {
			t.Fatalf("decoded participant %q serves %v, want %v", participant.Name, participant.Classes.Classes(), want.Classes())
		}
	}
	for index, guard := range decoded.Guarding.Receipts {
		want := saga.record.Guarding.Receipts[index].Classes
		if guard.Classes.Empty() || !guard.Classes.Equal(want) {
			t.Fatalf("decoded guard receipt protects %v, want %v", guard.Classes.Classes(), want.Classes())
		}
	}

	// WorkScope is likewise unexported, so a Work member round trip is the only
	// thing that proves HQ did not decode as an unnamed rig.
	participant, err := NewWorkWorkspaceParticipant("task-beads-provider", "work", "hq-physical", []WorkWorkspaceMember{
		{Scope: HQScope(), Prefix: "hq", ConfigContext: ConfigRefDigest(canonicalDigest([]byte("ctx"))), Provider: "task-beads-provider", Component: "work", PhysicalIdentity: "hq-physical"},
		{Scope: RigScope("alpha"), Prefix: "alpha", ConfigContext: ConfigRefDigest(canonicalDigest([]byte("ctx"))), ConfigOrder: 1, Provider: "task-beads-provider", Component: "work", PhysicalIdentity: "hq-physical"},
	})
	if err != nil {
		t.Fatalf("NewWorkWorkspaceParticipant: %v", err)
	}
	restored := toWireWorkParticipant(participant).decode()
	if !restored.Equal(participant) {
		t.Fatalf("decoded Work participant = %#v, want %#v", restored, participant)
	}
	if !restored.Members[0].Scope.IsHQ() {
		t.Fatal("the HQ scope decoded as something else")
	}
	if rig, ok := restored.Members[1].Scope.Rig(); !ok || rig != "alpha" {
		t.Fatalf("the rig scope decoded as %q (rig=%v)", rig, ok)
	}
}

func TestManifestCodecRoundTripsGenesisEvidence(t *testing.T) {
	plan := migrationPlan(t, "infra")
	descriptor := migrationDescriptor(t, "infra", "infra-provider", migrationAllClasses(t))
	intent, err := DeriveStartupIntent(StartupInputs{
		Plan:             plan,
		Discovered:       []DiscoveredBinding{migrationDiscovered(t, "infra", descriptor, migrationDiscoveryOptions{empty: true})},
		WitnessAlgorithm: SemanticWitnessAlgorithm,
	})
	if err != nil {
		t.Fatalf("DeriveStartupIntent: %v", err)
	}
	record, err := NewAttemptRecord(intent)
	if err != nil {
		t.Fatalf("NewAttemptRecord: %v", err)
	}
	if record.Intent.Genesis == nil {
		t.Fatal("the genesis attempt recorded no evidence")
	}

	payload, err := encodeAttemptRecord(record)
	if err != nil {
		t.Fatalf("encodeAttemptRecord: %v", err)
	}
	decoded, err := decodeAttemptRecord(payload)
	if err != nil {
		t.Fatalf("decodeAttemptRecord: %v", err)
	}
	if decoded.Intent.Genesis == nil {
		t.Fatal("genesis evidence was lost in the round trip; INTENT_FSYNCED would have neither a prior generation nor evidence")
	}
	if !reflect.DeepEqual(record, decoded) {
		t.Fatal("the decoded genesis record is not the encoded one")
	}
}

func TestManifestStoreResumesFromEveryPhaseBoundary(t *testing.T) {
	for _, phase := range migrationPhases() {
		t.Run(phase.String(), func(t *testing.T) {
			directory := t.TempDir()
			saga := migrationRelocationSaga(t)
			record := saga.advanceTo(t, phase)

			store, err := OpenManifestStore(directory)
			if err != nil {
				t.Fatalf("OpenManifestStore: %v", err)
			}
			if err := store.SaveAttempt(record); err != nil {
				t.Fatalf("SaveAttempt at %s: %v", phase, err)
			}
			if saga.manifest != nil {
				if err := store.SaveActive(saga.manifest); err != nil {
					t.Fatalf("SaveActive at %s: %v", phase, err)
				}
			}

			// A different process opens the same city directory and reads only
			// what is durable.
			reopened, err := OpenManifestStore(directory)
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			loaded, found, err := reopened.LatestAttempt()
			if err != nil || !found {
				t.Fatalf("LatestAttempt = (%v, %v)", found, err)
			}
			if loaded.Phase != phase {
				t.Fatalf("reloaded phase = %s, want %s", loaded.Phase, phase)
			}
			if !reflect.DeepEqual(record, loaded) {
				t.Fatal("the reloaded record is not the durable one")
			}
			want, err := record.Resume()
			if err != nil {
				t.Fatalf("Resume: %v", err)
			}
			got, err := loaded.Resume()
			if err != nil {
				t.Fatalf("Resume after reload: %v", err)
			}
			if !reflect.DeepEqual(want, got) {
				t.Fatalf("recovery route after reload = %#v, want %#v", got, want)
			}
		})
	}
}

func TestManifestStoreKeepsRollbackEvidenceAcrossARestart(t *testing.T) {
	directory := t.TempDir()
	store, err := OpenManifestStore(directory)
	if err != nil {
		t.Fatalf("OpenManifestStore: %v", err)
	}
	saga := migrationRelocationSaga(t)
	record := saga.advanceTo(t, PhasePreparing)
	if err := store.SaveAttempt(record); err != nil {
		t.Fatalf("SaveAttempt: %v", err)
	}
	beforeAnyWork, found, err := store.LatestAttempt()
	if err != nil || !found {
		t.Fatalf("LatestAttempt = (%v, %v)", found, err)
	}

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
	if err := store.SaveAttempt(record); err != nil {
		t.Fatalf("SaveAttempt after the return: %v", err)
	}
	afterRollback, found, err := store.LatestAttempt()
	if err != nil || !found {
		t.Fatalf("LatestAttempt = (%v, %v)", found, err)
	}

	first, err := beforeAnyWork.Resume()
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	second, err := afterRollback.Resume()
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if first.Phase != second.Phase || first.Action != second.Action {
		t.Fatalf("the two durable states differ by phase (%s/%s) rather than by evidence", first.Phase, second.Phase)
	}
	if second.PhaseEntries <= first.PhaseEntries || !second.Returned || len(second.DirtyDestinations) == 0 {
		t.Fatalf("the durable record lost the difference between not started and started-and-rolled-back: %#v", second)
	}
}

func TestManifestStoreRefusesToOverwriteWithAStaleRecord(t *testing.T) {
	directory := t.TempDir()
	store, err := OpenManifestStore(directory)
	if err != nil {
		t.Fatalf("OpenManifestStore: %v", err)
	}
	saga := migrationRelocationSaga(t)
	stale := saga.record.Clone()
	advanced := saga.advanceTo(t, PhaseGuarding)
	if err := store.SaveAttempt(advanced); err != nil {
		t.Fatalf("SaveAttempt: %v", err)
	}

	// A caller holding a record from before the advance must not be able to
	// rewind the durable saga by writing it back. Losing a phase transition and
	// reporting success is exactly how a saga silently forgets that it already
	// installed a guard or wrote into a destination.
	if err := store.SaveAttempt(stale); !errors.Is(err, ErrManifestConflict) {
		t.Fatalf("saving a stale record over an advanced one = %v, want a conflict", err)
	}
	loaded, _, err := store.LoadAttempt(advanced.Intent.DesiredGeneration)
	if err != nil {
		t.Fatalf("LoadAttempt: %v", err)
	}
	if loaded.Phase != PhaseGuarding {
		t.Fatalf("durable phase = %s, want the advanced %s", loaded.Phase, PhaseGuarding)
	}
}

// TestManifestStoreRefusesToOverwriteDurableEvidence covers the writes that
// advance the record without advancing the journal. Destination residue, guard
// receipts, guard releases, and participant receipts are all durable proof that
// something irreversible happened to a store, and every one of them is appended
// under the journal entry that was already current — so a stale clone taken
// before the append carries an identical journal and would otherwise be accepted
// as a continuation, silently erasing the proof.
func TestManifestStoreRefusesToOverwriteDurableEvidence(t *testing.T) {
	t.Run("destination residue", func(t *testing.T) {
		store, saga := migrationStoreAt(t, PhasePreparing)
		stale := saga.record.Clone()
		if err := saga.record.RecordDestinationResidue(DestinationResidue{
			Binding:          "next",
			Component:        saga.destination.Components[0].ID,
			PhysicalIdentity: saga.destination.Components[0].PhysicalIdentity,
			Kind:             ResidueWritten,
		}); err != nil {
			t.Fatalf("RecordDestinationResidue: %v", err)
		}
		if err := store.SaveAttempt(saga.record); err != nil {
			t.Fatalf("SaveAttempt: %v", err)
		}
		requireStaleWriteRejected(t, store, saga.record, stale, func(loaded *AttemptRecord) error {
			if len(loaded.Residue) != 1 {
				return fmt.Errorf("durable residue = %d entries, want the recorded one", len(loaded.Residue))
			}
			return nil
		})
	})

	t.Run("residue downgrade", func(t *testing.T) {
		store, saga := migrationStoreAt(t, PhasePreparing)
		residue := DestinationResidue{
			Binding:          "next",
			Component:        saga.destination.Components[0].ID,
			PhysicalIdentity: saga.destination.Components[0].PhysicalIdentity,
			Kind:             ResidueWritten,
		}
		if err := saga.record.RecordDestinationResidue(residue); err != nil {
			t.Fatalf("RecordDestinationResidue: %v", err)
		}
		if err := store.SaveAttempt(saga.record); err != nil {
			t.Fatalf("SaveAttempt: %v", err)
		}
		// A clone that only ever saw the reservation must not be able to
		// downgrade a written destination back to an untouched one: recovery
		// discards a reserved destination and reconciles a written one.
		downgraded := saga.record.Clone()
		downgraded.Residue[0].Kind = ResidueReserved
		if err := store.SaveAttempt(downgraded); !errors.Is(err, ErrManifestConflict) {
			t.Fatalf("downgrading durable residue = %v, want a conflict", err)
		}
	})

	t.Run("guard receipt", func(t *testing.T) {
		store, saga := migrationStoreAt(t, PhaseGuarding)
		stale := saga.record.Clone()
		receipt := migrationGuardReceipt(t, saga.guardPlan[0], saga.source.SemanticContractVersion)
		if err := saga.record.AppendGuardReceipt(receipt); err != nil {
			t.Fatalf("AppendGuardReceipt: %v", err)
		}
		if err := store.SaveAttempt(saga.record); err != nil {
			t.Fatalf("SaveAttempt: %v", err)
		}
		requireStaleWriteRejected(t, store, saga.record, stale, func(loaded *AttemptRecord) error {
			if loaded.Guarding == nil || len(loaded.Guarding.Receipts) != 1 {
				return fmt.Errorf("durable guard receipts = %#v, want the installed one", loaded.Guarding)
			}
			return nil
		})
	})

	t.Run("guard release", func(t *testing.T) {
		store, saga := migrationStoreAt(t, PhaseGuarding)
		receipt := migrationGuardReceipt(t, saga.guardPlan[0], saga.source.SemanticContractVersion)
		if err := saga.record.AppendGuardReceipt(receipt); err != nil {
			t.Fatalf("AppendGuardReceipt: %v", err)
		}
		if err := store.SaveAttempt(saga.record); err != nil {
			t.Fatalf("SaveAttempt: %v", err)
		}
		stale := saga.record.Clone()
		if err := saga.record.RecordGuardRelease(migrationGuardReleaseFor(receipt)); err != nil {
			t.Fatalf("RecordGuardRelease: %v", err)
		}
		if err := store.SaveAttempt(saga.record); err != nil {
			t.Fatalf("SaveAttempt after the release: %v", err)
		}
		requireStaleWriteRejected(t, store, saga.record, stale, func(loaded *AttemptRecord) error {
			if len(loaded.ReleasedGuards) != 1 {
				return fmt.Errorf("durable releases = %d, want the recorded one", len(loaded.ReleasedGuards))
			}
			return nil
		})
	})

	t.Run("participant receipt", func(t *testing.T) {
		store, saga := migrationStoreAt(t, PhaseCommitDecided)
		stale := saga.record.Clone()
		for _, participant := range saga.record.Decision.Participants {
			if err := saga.record.AppendReceipt(migrationBindingReceipt(t, participant, saga.destination, saga.record.Intent.Attempt, saga.record.Intent.DesiredGeneration)); err != nil {
				t.Fatalf("AppendReceipt(%q): %v", participant, err)
			}
		}
		if err := store.SaveAttempt(saga.record); err != nil {
			t.Fatalf("SaveAttempt: %v", err)
		}
		requireStaleWriteRejected(t, store, saga.record, stale, func(loaded *AttemptRecord) error {
			if !loaded.ReceiptsComplete() {
				return fmt.Errorf("durable receipts = %d, want the complete set", len(loaded.Receipts))
			}
			return nil
		})
	})
}

// TestManifestStoreRefusesAReturnThatForgetsAnInstalledGuard is the same defect
// on the one path that legitimately discards guard evidence. A clone taken
// before the install passes its own EnterPreparing check — it carries no receipt
// to release — and its return then discards the durable receipt, after which the
// record reads exactly like a guard that was never installed while the guard is
// still enforced on the source.
func TestManifestStoreRefusesAReturnThatForgetsAnInstalledGuard(t *testing.T) {
	store, saga := migrationStoreAt(t, PhaseGuarding)
	if err := store.SaveAttempt(saga.record); err != nil {
		t.Fatalf("SaveAttempt: %v", err)
	}
	stale := saga.record.Clone()

	receipt := migrationGuardReceipt(t, saga.guardPlan[0], saga.source.SemanticContractVersion)
	if err := saga.record.AppendGuardReceipt(receipt); err != nil {
		t.Fatalf("AppendGuardReceipt: %v", err)
	}
	if err := store.SaveAttempt(saga.record); err != nil {
		t.Fatalf("SaveAttempt for the receipt: %v", err)
	}

	if err := stale.EnterPreparing(saga.preparingSection(t), PhaseEntryReturnGuardsReleased); err != nil {
		t.Fatalf("return to PREPARING: %v", err)
	}
	if err := store.SaveAttempt(stale); !errors.Is(err, ErrManifestConflict) {
		t.Fatalf("a return that forgets an installed guard = %v, want a conflict", err)
	}
	loaded, _, err := store.LoadAttempt(saga.record.Intent.DesiredGeneration)
	if err != nil {
		t.Fatalf("LoadAttempt: %v", err)
	}
	if loaded.Guarding == nil || len(loaded.Guarding.Receipts) != 1 {
		t.Fatalf("the forgetful return erased the durable install receipt: %#v", loaded.Guarding)
	}
}

// TestManifestStoreAcceptsAReturnThatReleasedEveryInstalledGuard pins the other
// side: the release-then-return path is legal and must not be caught by
// the evidence check.
func TestManifestStoreAcceptsAReturnThatReleasedEveryInstalledGuard(t *testing.T) {
	store, saga := migrationStoreAt(t, PhaseGuarding)
	receipt := migrationGuardReceipt(t, saga.guardPlan[0], saga.source.SemanticContractVersion)
	if err := saga.record.AppendGuardReceipt(receipt); err != nil {
		t.Fatalf("AppendGuardReceipt: %v", err)
	}
	if err := store.SaveAttempt(saga.record); err != nil {
		t.Fatalf("SaveAttempt: %v", err)
	}
	if err := saga.record.RecordGuardRelease(migrationGuardReleaseFor(receipt)); err != nil {
		t.Fatalf("RecordGuardRelease: %v", err)
	}
	if err := saga.record.EnterPreparing(saga.preparingSection(t), PhaseEntryReturnGuardsReleased); err != nil {
		t.Fatalf("return to PREPARING: %v", err)
	}
	if err := store.SaveAttempt(saga.record); err != nil {
		t.Fatalf("SaveAttempt for a released return: %v", err)
	}
	loaded, _, err := store.LoadAttempt(saga.record.Intent.DesiredGeneration)
	if err != nil {
		t.Fatalf("LoadAttempt: %v", err)
	}
	if loaded.Phase != PhasePreparing || loaded.Guarding != nil || len(loaded.ReleasedGuards) != 1 {
		t.Fatalf("durable record after a released return = %s with %#v", loaded.Phase, loaded.Guarding)
	}
}

// migrationStoreAt opens a manifest store on a fresh directory and returns a
// saga advanced to one phase, without saving it.
func migrationStoreAt(t *testing.T, phase MigrationPhase) (*ManifestStore, *migrationSaga) {
	t.Helper()
	store, err := OpenManifestStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenManifestStore: %v", err)
	}
	saga := migrationRelocationSaga(t)
	saga.advanceTo(t, phase)
	return store, saga
}

// requireStaleWriteRejected proves that writing back a record taken before a
// durable evidence append conflicts, and that the durable record still carries
// the evidence afterwards.
func requireStaleWriteRejected(t *testing.T, store *ManifestStore, durable, stale *AttemptRecord, check func(*AttemptRecord) error) {
	t.Helper()
	if err := store.SaveAttempt(stale); !errors.Is(err, ErrManifestConflict) {
		t.Fatalf("saving a record from before the evidence append = %v, want a conflict", err)
	}
	loaded, found, err := store.LoadAttempt(durable.Intent.DesiredGeneration)
	if err != nil || !found {
		t.Fatalf("LoadAttempt = (%v, %v)", found, err)
	}
	if err := check(loaded); err != nil {
		t.Fatalf("the stale write erased durable evidence: %v", err)
	}
}

func TestManifestStoreRejectsARewrittenJournalAndAForeignAttempt(t *testing.T) {
	directory := t.TempDir()
	store, err := OpenManifestStore(directory)
	if err != nil {
		t.Fatalf("OpenManifestStore: %v", err)
	}
	saga := migrationRelocationSaga(t)
	record := saga.advanceTo(t, PhasePrepared)
	if err := store.SaveAttempt(record); err != nil {
		t.Fatalf("SaveAttempt: %v", err)
	}

	rewritten := record.Clone()
	rewritten.Journal[1].Reason = PhaseEntryReturnSourceChanged
	if err := store.SaveAttempt(rewritten); !errors.Is(err, ErrManifestConflict) {
		t.Fatalf("saving a rewritten journal = %v, want a conflict", err)
	}

	foreign := record.Clone()
	foreign.Intent.Attempt = "attempt-from-another-derivation"
	foreign.Decision = nil
	if err := store.SaveAttempt(foreign); !errors.Is(err, ErrManifestConflict) {
		t.Fatalf("saving a foreign attempt for the same generation = %v, want a conflict", err)
	}
}

func TestManifestStoreDistinguishesAbsentFromUnreadable(t *testing.T) {
	directory := t.TempDir()
	store, err := OpenManifestStore(directory)
	if err != nil {
		t.Fatalf("OpenManifestStore: %v", err)
	}

	manifest, found, err := store.LoadActive()
	if err != nil || found || manifest != nil {
		t.Fatalf("LoadActive on a fresh city = (%v, %v, %v), want an absent manifest and no error", manifest, found, err)
	}
	if _, found, err := store.LatestAttempt(); err != nil || found {
		t.Fatalf("LatestAttempt on a fresh city = (%v, %v), want no attempt and no error", found, err)
	}

	// A truncated or corrupt record is not an absent one: reading it must fail
	// loudly, because "absent" is the genesis path.
	if err := os.WriteFile(filepath.Join(store.Directory(), activeManifestName), []byte("{\"version\":1,"), manifestFileMode); err != nil {
		t.Fatalf("writing a corrupt manifest: %v", err)
	}
	manifest, found, err = store.LoadActive()
	if err == nil {
		t.Fatal("a corrupt active manifest read as valid")
	}
	if !found {
		t.Fatal("a corrupt active manifest reported itself absent, which is the genesis path")
	}
	if manifest != nil {
		t.Fatal("a corrupt active manifest returned a usable value")
	}
}

func TestManifestStoreReplacementLeavesThePreviousRecordIntact(t *testing.T) {
	directory := t.TempDir()
	store, err := OpenManifestStore(directory)
	if err != nil {
		t.Fatalf("OpenManifestStore: %v", err)
	}
	saga := migrationRelocationSaga(t)
	record := saga.advanceTo(t, PhasePreparing)
	if err := store.SaveAttempt(record); err != nil {
		t.Fatalf("SaveAttempt: %v", err)
	}
	durable, _, err := store.LoadAttempt(record.Intent.DesiredGeneration)
	if err != nil {
		t.Fatalf("LoadAttempt: %v", err)
	}

	// A record that cannot be encoded must never reach the file: the previous
	// durable record stays exactly as it was.
	damaged := record.Clone()
	damaged.Intent.WitnessAlgorithm = ""
	if err := store.SaveAttempt(damaged); err == nil {
		t.Fatal("an invalid record was written")
	}
	reloaded, _, err := store.LoadAttempt(record.Intent.DesiredGeneration)
	if err != nil {
		t.Fatalf("LoadAttempt after the rejected write: %v", err)
	}
	if !reflect.DeepEqual(durable, reloaded) {
		t.Fatal("a rejected write changed the durable record")
	}

	entries, err := os.ReadDir(store.Directory())
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp-") {
			t.Fatalf("a temporary manifest file survived: %s", entry.Name())
		}
	}
}

func TestManifestStoreDoesNotDowngradeTheActiveGeneration(t *testing.T) {
	directory := t.TempDir()
	store, err := OpenManifestStore(directory)
	if err != nil {
		t.Fatalf("OpenManifestStore: %v", err)
	}
	saga := migrationMaximalRecord(t)
	if err := store.SaveActive(saga.manifest); err != nil {
		t.Fatalf("SaveActive: %v", err)
	}

	older := saga.manifest.Clone()
	older.Generation = saga.manifest.Generation - 1
	older.CutoverGeneration = older.Generation
	older.RollbackGeneration = 0
	for index := range older.Receipts {
		older.Receipts[index].Generation = older.Generation
	}
	for index := range older.Guards {
		older.Guards[index].Generation = older.Generation
	}
	if err := store.SaveActive(older); !errors.Is(err, ErrManifestConflict) {
		t.Fatalf("saving an older active generation = %v, want a conflict", err)
	}
}

func TestManifestStoreNamesRecordsByGeneration(t *testing.T) {
	directory := t.TempDir()
	store, err := OpenManifestStore(directory)
	if err != nil {
		t.Fatalf("OpenManifestStore: %v", err)
	}
	saga := migrationRelocationSaga(t)
	if err := store.SaveAttempt(saga.record); err != nil {
		t.Fatalf("SaveAttempt: %v", err)
	}

	name := fmt.Sprintf("attempt-%d.json", saga.record.Intent.DesiredGeneration)
	if _, err := os.Stat(filepath.Join(store.Directory(), name)); err != nil {
		t.Fatalf("expected a durable %s: %v", name, err)
	}
	if _, found, err := store.LoadAttempt(saga.record.Intent.DesiredGeneration + 1); err != nil || found {
		t.Fatalf("LoadAttempt for an unrecorded generation = (%v, %v), want absent", found, err)
	}
	if _, _, err := store.LoadAttempt(0); err == nil {
		t.Fatal("generation zero read as an attempt; it is the invalid sentinel")
	}
}
