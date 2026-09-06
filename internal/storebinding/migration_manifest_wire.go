package storebinding

import (
	"encoding/json"
	"fmt"
)

type wireClassMove struct {
	Class           string `json:"class"`
	Kind            uint8  `json:"kind"`
	PriorBinding    string `json:"prior_binding,omitempty"`
	DesiredBinding  string `json:"desired_binding"`
	SourcePopulated bool   `json:"source_populated"`
}

type wireBindingParticipant struct {
	Name     string   `json:"name"`
	Provider string   `json:"provider"`
	Classes  []string `json:"classes"`
}

type wireParticipantSet struct {
	Bindings []wireBindingParticipant `json:"bindings,omitempty"`
	Work     []wireWorkParticipant    `json:"work,omitempty"`
}

type wireGenesisBinding struct {
	Name       string          `json:"name"`
	Target     wireFenceTarget `json:"target"`
	Descriptor *wireDescriptor `json:"descriptor,omitempty"`
}

type wireIntentSection struct {
	Version              uint16                 `json:"version"`
	Attempt              string                 `json:"attempt"`
	PriorGeneration      uint64                 `json:"prior_generation"`
	DesiredGeneration    uint64                 `json:"desired_generation"`
	Genesis              []wireGenesisBinding   `json:"genesis,omitempty"`
	HasGenesis           bool                   `json:"has_genesis"`
	PriorAssignments     map[string]string      `json:"prior_assignments,omitempty"`
	DesiredAssignments   map[string]string      `json:"desired_assignments"`
	Moves                []wireClassMove        `json:"moves"`
	FenceTargets         []wireNamedFenceTarget `json:"fence_targets"`
	Descriptors          []wireNamedDescriptor  `json:"descriptors,omitempty"`
	RetainedSources      []wireRetainedSource   `json:"retained_sources,omitempty"`
	Participants         wireParticipantSet     `json:"participants"`
	ConfigDigest         string                 `json:"config_digest"`
	BindingConfigDigests map[string]string      `json:"binding_config_digests"`
	WitnessAlgorithm     string                 `json:"witness_algorithm"`
}

func toWireIntentSection(section IntentSection) wireIntentSection {
	out := wireIntentSection{
		Version:              section.Version,
		Attempt:              string(section.Attempt),
		PriorGeneration:      uint64(section.PriorGeneration),
		DesiredGeneration:    uint64(section.DesiredGeneration),
		PriorAssignments:     toWireAssignments(section.PriorAssignments),
		DesiredAssignments:   toWireAssignments(section.DesiredAssignments),
		Descriptors:          toWireNamedDescriptors(section.Descriptors),
		ConfigDigest:         string(section.ConfigDigest),
		BindingConfigDigests: toWireDigests(section.BindingConfigDigests),
		WitnessAlgorithm:     section.WitnessAlgorithm,
	}
	if section.Genesis != nil {
		out.HasGenesis = true
		for _, binding := range section.Genesis.Bindings {
			entry := wireGenesisBinding{Name: string(binding.Name), Target: toWireFenceTarget(binding.Target)}
			if binding.Descriptor != nil {
				descriptor := toWireDescriptor(*binding.Descriptor)
				entry.Descriptor = &descriptor
			}
			out.Genesis = append(out.Genesis, entry)
		}
	}
	for _, move := range section.Moves {
		out.Moves = append(out.Moves, wireClassMove{
			Class:           move.Class.String(),
			Kind:            uint8(move.Kind),
			PriorBinding:    string(move.PriorBinding),
			DesiredBinding:  string(move.DesiredBinding),
			SourcePopulated: move.SourcePopulated,
		})
	}
	for _, target := range section.FenceTargets {
		out.FenceTargets = append(out.FenceTargets, wireNamedFenceTarget{
			Name:   string(target.Name),
			Role:   uint8(target.Role),
			Target: toWireFenceTarget(target.Target),
		})
	}
	for _, source := range section.RetainedSources {
		out.RetainedSources = append(out.RetainedSources, toWireRetainedSource(source))
	}
	for _, binding := range section.Participants.Bindings {
		out.Participants.Bindings = append(out.Participants.Bindings, wireBindingParticipant{
			Name:     string(binding.Name),
			Provider: string(binding.Provider),
			Classes:  toWireClassSet(binding.Classes),
		})
	}
	for _, participant := range section.Participants.Work {
		out.Participants.Work = append(out.Participants.Work, toWireWorkParticipant(participant))
	}
	return out
}

func (w wireIntentSection) decode() (IntentSection, error) {
	priorAssignments, err := decodeWireAssignments(w.PriorAssignments)
	if err != nil {
		return IntentSection{}, err
	}
	desiredAssignments, err := decodeWireAssignments(w.DesiredAssignments)
	if err != nil {
		return IntentSection{}, err
	}
	descriptors, err := decodeWireNamedDescriptors(w.Descriptors)
	if err != nil {
		return IntentSection{}, err
	}
	section := IntentSection{
		Version:              w.Version,
		Attempt:              AttemptID(w.Attempt),
		PriorGeneration:      Generation(w.PriorGeneration),
		DesiredGeneration:    Generation(w.DesiredGeneration),
		PriorAssignments:     priorAssignments,
		DesiredAssignments:   desiredAssignments,
		Descriptors:          descriptors,
		ConfigDigest:         CompositeConfigDigest(w.ConfigDigest),
		BindingConfigDigests: decodeWireDigests(w.BindingConfigDigests),
		WitnessAlgorithm:     w.WitnessAlgorithm,
	}
	if w.HasGenesis {
		genesis := &GenesisEvidence{}
		for _, binding := range w.Genesis {
			target, err := binding.Target.decode()
			if err != nil {
				return IntentSection{}, err
			}
			entry := GenesisBindingEvidence{Name: BindingName(binding.Name), Target: target}
			if binding.Descriptor != nil {
				descriptor, err := binding.Descriptor.decode()
				if err != nil {
					return IntentSection{}, err
				}
				entry.Descriptor = &descriptor
			}
			genesis.Bindings = append(genesis.Bindings, entry)
		}
		section.Genesis = genesis
	}
	for _, move := range w.Moves {
		class, err := classNamed(move.Class)
		if err != nil {
			return IntentSection{}, err
		}
		section.Moves = append(section.Moves, ClassMove{
			Class:           class,
			Kind:            ClassMoveKind(move.Kind),
			PriorBinding:    BindingName(move.PriorBinding),
			DesiredBinding:  BindingName(move.DesiredBinding),
			SourcePopulated: move.SourcePopulated,
		})
	}
	for _, target := range w.FenceTargets {
		decoded, err := target.Target.decode()
		if err != nil {
			return IntentSection{}, err
		}
		section.FenceTargets = append(section.FenceTargets, NamedFenceTarget{
			Name:   BindingName(target.Name),
			Role:   TargetRole(target.Role),
			Target: decoded,
		})
	}
	for _, source := range w.RetainedSources {
		decoded, err := source.decode()
		if err != nil {
			return IntentSection{}, err
		}
		section.RetainedSources = append(section.RetainedSources, decoded)
	}
	for _, binding := range w.Participants.Bindings {
		classes, err := decodeWireClassSet(binding.Classes)
		if err != nil {
			return IntentSection{}, err
		}
		section.Participants.Bindings = append(section.Participants.Bindings, BindingParticipant{
			Name:     BindingName(binding.Name),
			Provider: ProviderID(binding.Provider),
			Classes:  classes,
		})
	}
	for _, participant := range w.Participants.Work {
		section.Participants.Work = append(section.Participants.Work, participant.decode())
	}
	return section, nil
}

type wireClassInventory struct {
	Class              string `json:"class"`
	Binding            string `json:"binding"`
	Component          string `json:"component"`
	PhysicalIdentity   string `json:"physical_identity"`
	Population         uint8  `json:"population"`
	DuplicateResidence bool   `json:"duplicate_residence"`
}

type wireClassPhaseState struct {
	Class     string `json:"class"`
	Phase     uint8  `json:"phase"`
	Resumable bool   `json:"resumable"`
}

type wirePreparingSection struct {
	Version           uint16                `json:"version"`
	SourceDescriptors []wireNamedDescriptor `json:"source_descriptors,omitempty"`
	Census            []ComponentCensus     `json:"census"`
	Inventory         []wireClassInventory  `json:"inventory"`
	ClassStates       []wireClassPhaseState `json:"class_states"`
	Holds             []RetentionHold       `json:"holds,omitempty"`
}

func toWirePreparingSection(section PreparingSection) wirePreparingSection {
	out := wirePreparingSection{
		Version:           section.Version,
		SourceDescriptors: toWireNamedDescriptors(section.SourceDescriptors),
		Census:            append([]ComponentCensus(nil), section.Census...),
		Holds:             append([]RetentionHold(nil), section.Holds...),
	}
	for _, entry := range section.Inventory {
		out.Inventory = append(out.Inventory, wireClassInventory{
			Class:              entry.Class.String(),
			Binding:            string(entry.Binding),
			Component:          string(entry.Component),
			PhysicalIdentity:   string(entry.PhysicalIdentity),
			Population:         uint8(entry.Population),
			DuplicateResidence: entry.DuplicateResidence,
		})
	}
	for _, state := range section.ClassStates {
		out.ClassStates = append(out.ClassStates, wireClassPhaseState{
			Class:     state.Class.String(),
			Phase:     uint8(state.Phase),
			Resumable: state.Resumable,
		})
	}
	return out
}

func (w wirePreparingSection) decode() (PreparingSection, error) {
	descriptors, err := decodeWireNamedDescriptors(w.SourceDescriptors)
	if err != nil {
		return PreparingSection{}, err
	}
	section := PreparingSection{
		Version:           w.Version,
		SourceDescriptors: descriptors,
		Census:            append([]ComponentCensus(nil), w.Census...),
		Holds:             append([]RetentionHold(nil), w.Holds...),
	}
	for _, entry := range w.Inventory {
		class, err := classNamed(entry.Class)
		if err != nil {
			return PreparingSection{}, err
		}
		section.Inventory = append(section.Inventory, ClassInventory{
			Class:              class,
			Binding:            BindingName(entry.Binding),
			Component:          ComponentID(entry.Component),
			PhysicalIdentity:   PhysicalIdentity(entry.PhysicalIdentity),
			Population:         BindingPopulation(entry.Population),
			DuplicateResidence: entry.DuplicateResidence,
		})
	}
	for _, state := range w.ClassStates {
		class, err := classNamed(state.Class)
		if err != nil {
			return PreparingSection{}, err
		}
		section.ClassStates = append(section.ClassStates, ClassPhaseState{
			Class:     class,
			Phase:     MigrationPhase(state.Phase),
			Resumable: state.Resumable,
		})
	}
	return section, nil
}

type wireWorkPrepared struct {
	Version            uint16              `json:"version"`
	Attempt            string              `json:"attempt"`
	Generation         uint64              `json:"generation"`
	Participant        wireWorkParticipant `json:"participant"`
	DescriptorIdentity string              `json:"descriptor_identity"`
	Preparation        wireWorkPreparation `json:"preparation"`
	Receipt            string              `json:"receipt"`
}

type wireWorkProof struct {
	Version            uint16              `json:"version"`
	Attempt            string              `json:"attempt"`
	Generation         uint64              `json:"generation"`
	Participant        wireWorkParticipant `json:"participant"`
	DescriptorIdentity string              `json:"descriptor_identity"`
	Preparation        wireWorkPreparation `json:"preparation"`
	PreparedReceipt    string              `json:"prepared_receipt"`
	Witness            string              `json:"witness"`
}

type wirePreparedSection struct {
	Version                uint16                   `json:"version"`
	DestinationDescriptors []wireNamedDescriptor    `json:"destination_descriptors"`
	StagedGeneration       *wireNamedDescriptor     `json:"staged_generation,omitempty"`
	Witnesses              []wireClassWitnessRecord `json:"witnesses,omitempty"`
	WorkPrepared           []wireWorkPrepared       `json:"work_prepared,omitempty"`
	WorkProofs             []wireWorkProof          `json:"work_proofs,omitempty"`
	RetainedSources        []wireRetainedSource     `json:"retained_sources,omitempty"`
}

func toWirePreparedSection(section PreparedSection) wirePreparedSection {
	out := wirePreparedSection{
		Version:                section.Version,
		DestinationDescriptors: toWireNamedDescriptors(section.DestinationDescriptors),
	}
	if section.StagedGeneration != nil {
		staged := wireNamedDescriptor{Name: string(section.StagedGeneration.Name), Descriptor: toWireDescriptor(section.StagedGeneration.Descriptor)}
		out.StagedGeneration = &staged
	}
	for _, witness := range section.Witnesses {
		out.Witnesses = append(out.Witnesses, toWireClassWitnessRecord(witness))
	}
	for _, prepared := range section.WorkPrepared {
		out.WorkPrepared = append(out.WorkPrepared, wireWorkPrepared{
			Version:            prepared.Version,
			Attempt:            string(prepared.Attempt),
			Generation:         uint64(prepared.Generation),
			Participant:        toWireWorkParticipant(prepared.Participant),
			DescriptorIdentity: string(prepared.DescriptorIdentity),
			Preparation:        toWireWorkPreparation(prepared.Preparation),
			Receipt:            prepared.Receipt,
		})
	}
	for _, proof := range section.WorkProofs {
		out.WorkProofs = append(out.WorkProofs, wireWorkProof{
			Version:            proof.Version,
			Attempt:            string(proof.Attempt),
			Generation:         uint64(proof.Generation),
			Participant:        toWireWorkParticipant(proof.Participant),
			DescriptorIdentity: string(proof.DescriptorIdentity),
			Preparation:        toWireWorkPreparation(proof.Preparation),
			PreparedReceipt:    proof.PreparedReceipt,
			Witness:            proof.Witness,
		})
	}
	for _, source := range section.RetainedSources {
		out.RetainedSources = append(out.RetainedSources, toWireRetainedSource(source))
	}
	return out
}

func (w wirePreparedSection) decode() (PreparedSection, error) {
	descriptors, err := decodeWireNamedDescriptors(w.DestinationDescriptors)
	if err != nil {
		return PreparedSection{}, err
	}
	section := PreparedSection{Version: w.Version, DestinationDescriptors: descriptors}
	if w.StagedGeneration != nil {
		descriptor, err := w.StagedGeneration.Descriptor.decode()
		if err != nil {
			return PreparedSection{}, err
		}
		section.StagedGeneration = &NamedDescriptor{Name: BindingName(w.StagedGeneration.Name), Descriptor: descriptor}
	}
	for _, witness := range w.Witnesses {
		decoded, err := witness.decode()
		if err != nil {
			return PreparedSection{}, err
		}
		section.Witnesses = append(section.Witnesses, decoded)
	}
	for _, prepared := range w.WorkPrepared {
		section.WorkPrepared = append(section.WorkPrepared, WorkPrepared{
			Version:            prepared.Version,
			Attempt:            AttemptID(prepared.Attempt),
			Generation:         Generation(prepared.Generation),
			Participant:        prepared.Participant.decode(),
			DescriptorIdentity: BindingIdentity(prepared.DescriptorIdentity),
			Preparation:        prepared.Preparation.decode(),
			Receipt:            prepared.Receipt,
		})
	}
	for _, proof := range w.WorkProofs {
		section.WorkProofs = append(section.WorkProofs, WorkProof{
			Version:            proof.Version,
			Attempt:            AttemptID(proof.Attempt),
			Generation:         Generation(proof.Generation),
			Participant:        proof.Participant.decode(),
			DescriptorIdentity: BindingIdentity(proof.DescriptorIdentity),
			Preparation:        proof.Preparation.decode(),
			PreparedReceipt:    proof.PreparedReceipt,
			Witness:            proof.Witness,
		})
	}
	for _, source := range w.RetainedSources {
		decoded, err := source.decode()
		if err != nil {
			return PreparedSection{}, err
		}
		section.RetainedSources = append(section.RetainedSources, decoded)
	}
	return section, nil
}

type wireGuardInstallRequest struct {
	Attempt          string             `json:"attempt"`
	Generation       uint64             `json:"generation"`
	Source           wireRetainedSource `json:"source"`
	Component        string             `json:"component"`
	PhysicalIdentity string             `json:"physical_identity"`
	Role             uint8              `json:"role"`
}

type wireGuardingSection struct {
	Version  uint16                    `json:"version"`
	Plan     []wireGuardInstallRequest `json:"plan"`
	Receipts []wireGuardReceipt        `json:"receipts,omitempty"`
}

func toWireGuardingSection(section GuardingSection) wireGuardingSection {
	out := wireGuardingSection{Version: section.Version}
	for _, request := range section.Plan {
		out.Plan = append(out.Plan, wireGuardInstallRequest{
			Attempt:          string(request.Attempt),
			Generation:       uint64(request.Generation),
			Source:           toWireRetainedSource(request.Source),
			Component:        string(request.Component),
			PhysicalIdentity: string(request.PhysicalIdentity),
			Role:             uint8(request.Role),
		})
	}
	for _, receipt := range section.Receipts {
		out.Receipts = append(out.Receipts, toWireGuardReceipt(receipt))
	}
	return out
}

func (w wireGuardingSection) decode() (GuardingSection, error) {
	section := GuardingSection{Version: w.Version}
	for _, request := range w.Plan {
		source, err := request.Source.decode()
		if err != nil {
			return GuardingSection{}, err
		}
		section.Plan = append(section.Plan, GuardInstallRequest{
			Attempt:          AttemptID(request.Attempt),
			Generation:       Generation(request.Generation),
			Source:           source,
			Component:        ComponentID(request.Component),
			PhysicalIdentity: PhysicalIdentity(request.PhysicalIdentity),
			Role:             GuardRole(request.Role),
		})
	}
	for _, receipt := range w.Receipts {
		decoded, err := receipt.decode()
		if err != nil {
			return GuardingSection{}, err
		}
		section.Receipts = append(section.Receipts, decoded)
	}
	return section, nil
}

type wireGuardsInstalledSection struct {
	Version   uint16                  `json:"version"`
	Verified  []wireGuardReceipt      `json:"verified,omitempty"`
	EmptyPlan bool                    `json:"empty_plan"`
	InPlace   []InPlaceAdoptionRecord `json:"in_place,omitempty"`
}

func toWireGuardsInstalledSection(section GuardsInstalledSection) wireGuardsInstalledSection {
	out := wireGuardsInstalledSection{
		Version:   section.Version,
		EmptyPlan: section.EmptyPlan,
		InPlace:   append([]InPlaceAdoptionRecord(nil), section.InPlace...),
	}
	for _, receipt := range section.Verified {
		out.Verified = append(out.Verified, toWireGuardReceipt(receipt))
	}
	return out
}

func (w wireGuardsInstalledSection) decode() (GuardsInstalledSection, error) {
	section := GuardsInstalledSection{
		Version:   w.Version,
		EmptyPlan: w.EmptyPlan,
		InPlace:   append([]InPlaceAdoptionRecord(nil), w.InPlace...),
	}
	for _, receipt := range w.Verified {
		decoded, err := receipt.decode()
		if err != nil {
			return GuardsInstalledSection{}, err
		}
		section.Verified = append(section.Verified, decoded)
	}
	return section, nil
}

type wireCommitDecisionSection struct {
	Version      uint16   `json:"version"`
	Attempt      string   `json:"attempt"`
	Generation   uint64   `json:"generation"`
	Decided      bool     `json:"decided"`
	Participants []string `json:"participants"`
}

type wireAttemptRecord struct {
	Version              uint16                      `json:"version"`
	Phase                uint8                       `json:"phase"`
	Journal              []PhaseEntry                `json:"journal"`
	Intent               wireIntentSection           `json:"intent"`
	Preparing            *wirePreparingSection       `json:"preparing,omitempty"`
	Prepared             *wirePreparedSection        `json:"prepared,omitempty"`
	Guarding             *wireGuardingSection        `json:"guarding,omitempty"`
	GuardsInstalled      *wireGuardsInstalledSection `json:"guards_installed,omitempty"`
	Decision             *wireCommitDecisionSection  `json:"decision,omitempty"`
	Receipts             []wireParticipantReceipt    `json:"receipts,omitempty"`
	Residue              []DestinationResidue        `json:"residue,omitempty"`
	ReleasedGuards       []GuardRelease              `json:"released_guards,omitempty"`
	ActiveManifestDigest string                      `json:"active_manifest_digest,omitempty"`
}

// encodeAttemptRecord serializes a validated attempt record. Encoding validates
// first so a record that could never be read back is never written.
func encodeAttemptRecord(record *AttemptRecord) ([]byte, error) {
	if err := record.Validate(); err != nil {
		return nil, err
	}
	wire := wireAttemptRecord{
		Version:              record.Version,
		Phase:                uint8(record.Phase),
		Journal:              append([]PhaseEntry(nil), record.Journal...),
		Intent:               toWireIntentSection(record.Intent),
		Residue:              append([]DestinationResidue(nil), record.Residue...),
		ReleasedGuards:       append([]GuardRelease(nil), record.ReleasedGuards...),
		ActiveManifestDigest: record.ActiveManifestDigest,
	}
	if record.Preparing != nil {
		section := toWirePreparingSection(*record.Preparing)
		wire.Preparing = &section
	}
	if record.Prepared != nil {
		section := toWirePreparedSection(*record.Prepared)
		wire.Prepared = &section
	}
	if record.Guarding != nil {
		section := toWireGuardingSection(*record.Guarding)
		wire.Guarding = &section
	}
	if record.GuardsInstalled != nil {
		section := toWireGuardsInstalledSection(*record.GuardsInstalled)
		wire.GuardsInstalled = &section
	}
	if record.Decision != nil {
		wire.Decision = &wireCommitDecisionSection{
			Version:      record.Decision.Version,
			Attempt:      string(record.Decision.Decision.Attempt),
			Generation:   uint64(record.Decision.Decision.Generation),
			Decided:      record.Decision.Decision.Decided,
			Participants: append([]string(nil), record.Decision.Participants...),
		}
	}
	for _, receipt := range record.Receipts {
		wire.Receipts = append(wire.Receipts, toWireParticipantReceipt(receipt))
	}
	return json.Marshal(wire)
}

// decodeAttemptRecord parses and revalidates a durable attempt record. A record
// that decodes into something the phase rules reject is a damaged record and
// blocks; it is never repaired into a plausible one.
func decodeAttemptRecord(payload []byte) (*AttemptRecord, error) {
	var wire wireAttemptRecord
	if err := json.Unmarshal(payload, &wire); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidManifest, err)
	}
	intent, err := wire.Intent.decode()
	if err != nil {
		return nil, err
	}
	record := &AttemptRecord{
		Version:              wire.Version,
		Phase:                MigrationPhase(wire.Phase),
		Journal:              append([]PhaseEntry(nil), wire.Journal...),
		Intent:               intent,
		Residue:              append([]DestinationResidue(nil), wire.Residue...),
		ReleasedGuards:       append([]GuardRelease(nil), wire.ReleasedGuards...),
		ActiveManifestDigest: wire.ActiveManifestDigest,
	}
	if wire.Preparing != nil {
		section, err := wire.Preparing.decode()
		if err != nil {
			return nil, err
		}
		record.Preparing = &section
	}
	if wire.Prepared != nil {
		section, err := wire.Prepared.decode()
		if err != nil {
			return nil, err
		}
		record.Prepared = &section
	}
	if wire.Guarding != nil {
		section, err := wire.Guarding.decode()
		if err != nil {
			return nil, err
		}
		record.Guarding = &section
	}
	if wire.GuardsInstalled != nil {
		section, err := wire.GuardsInstalled.decode()
		if err != nil {
			return nil, err
		}
		record.GuardsInstalled = &section
	}
	if wire.Decision != nil {
		record.Decision = &CommitDecisionSection{
			Version: wire.Decision.Version,
			Decision: CommitDecision{
				Attempt:    AttemptID(wire.Decision.Attempt),
				Generation: Generation(wire.Decision.Generation),
				Decided:    wire.Decision.Decided,
			},
			Participants: append([]string(nil), wire.Decision.Participants...),
		}
	}
	for _, receipt := range wire.Receipts {
		record.Receipts = append(record.Receipts, receipt.decode())
	}
	if err := record.Validate(); err != nil {
		return nil, err
	}
	return record, nil
}

type wireActiveManifest struct {
	Version              uint16                   `json:"version"`
	Generation           uint64                   `json:"generation"`
	Attempt              string                   `json:"attempt"`
	Assignments          map[string]string        `json:"assignments"`
	Descriptors          []wireNamedDescriptor    `json:"descriptors"`
	Receipts             []wireParticipantReceipt `json:"receipts"`
	Guards               []wireGuardReceipt       `json:"guards,omitempty"`
	RetainedSources      []wireRetainedSource     `json:"retained_sources,omitempty"`
	WorkProofs           []wireWorkProof          `json:"work_proofs,omitempty"`
	ConfigDigest         string                   `json:"config_digest"`
	BindingConfigDigests map[string]string        `json:"binding_config_digests"`
	WitnessAlgorithm     string                   `json:"witness_algorithm"`
	CutoverGeneration    uint64                   `json:"cutover_generation"`
	RollbackGeneration   uint64                   `json:"rollback_generation"`
}

// encodeActiveManifest serializes a validated active manifest.
func encodeActiveManifest(manifest *ActiveManifest) ([]byte, error) {
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	wire := wireActiveManifest{
		Version:              manifest.Version,
		Generation:           uint64(manifest.Generation),
		Attempt:              string(manifest.Attempt),
		Assignments:          toWireAssignments(manifest.Assignments),
		Descriptors:          toWireNamedDescriptors(manifest.Descriptors),
		ConfigDigest:         string(manifest.ConfigDigest),
		BindingConfigDigests: toWireDigests(manifest.BindingConfigDigests),
		WitnessAlgorithm:     manifest.WitnessAlgorithm,
		CutoverGeneration:    uint64(manifest.CutoverGeneration),
		RollbackGeneration:   uint64(manifest.RollbackGeneration),
	}
	for _, receipt := range manifest.Receipts {
		wire.Receipts = append(wire.Receipts, toWireParticipantReceipt(receipt))
	}
	for _, guard := range manifest.Guards {
		wire.Guards = append(wire.Guards, toWireGuardReceipt(guard))
	}
	for _, source := range manifest.RetainedSources {
		wire.RetainedSources = append(wire.RetainedSources, toWireRetainedSource(source))
	}
	for _, proof := range manifest.WorkProofs {
		wire.WorkProofs = append(wire.WorkProofs, wireWorkProof{
			Version:            proof.Version,
			Attempt:            string(proof.Attempt),
			Generation:         uint64(proof.Generation),
			Participant:        toWireWorkParticipant(proof.Participant),
			DescriptorIdentity: string(proof.DescriptorIdentity),
			Preparation:        toWireWorkPreparation(proof.Preparation),
			PreparedReceipt:    proof.PreparedReceipt,
			Witness:            proof.Witness,
		})
	}
	return json.Marshal(wire)
}

// decodeActiveManifest parses and revalidates a durable active manifest.
func decodeActiveManifest(payload []byte) (*ActiveManifest, error) {
	var wire wireActiveManifest
	if err := json.Unmarshal(payload, &wire); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidManifest, err)
	}
	assignments, err := decodeWireAssignments(wire.Assignments)
	if err != nil {
		return nil, err
	}
	descriptors, err := decodeWireNamedDescriptors(wire.Descriptors)
	if err != nil {
		return nil, err
	}
	manifest := &ActiveManifest{
		Version:              wire.Version,
		Generation:           Generation(wire.Generation),
		Attempt:              AttemptID(wire.Attempt),
		Assignments:          assignments,
		Descriptors:          descriptors,
		ConfigDigest:         CompositeConfigDigest(wire.ConfigDigest),
		BindingConfigDigests: decodeWireDigests(wire.BindingConfigDigests),
		WitnessAlgorithm:     wire.WitnessAlgorithm,
		CutoverGeneration:    Generation(wire.CutoverGeneration),
		RollbackGeneration:   Generation(wire.RollbackGeneration),
	}
	for _, receipt := range wire.Receipts {
		manifest.Receipts = append(manifest.Receipts, receipt.decode())
	}
	for _, guard := range wire.Guards {
		decoded, err := guard.decode()
		if err != nil {
			return nil, err
		}
		manifest.Guards = append(manifest.Guards, decoded)
	}
	for _, source := range wire.RetainedSources {
		decoded, err := source.decode()
		if err != nil {
			return nil, err
		}
		manifest.RetainedSources = append(manifest.RetainedSources, decoded)
	}
	for _, proof := range wire.WorkProofs {
		manifest.WorkProofs = append(manifest.WorkProofs, WorkProof{
			Version:            proof.Version,
			Attempt:            AttemptID(proof.Attempt),
			Generation:         Generation(proof.Generation),
			Participant:        proof.Participant.decode(),
			DescriptorIdentity: BindingIdentity(proof.DescriptorIdentity),
			Preparation:        proof.Preparation.decode(),
			PreparedReceipt:    proof.PreparedReceipt,
			Witness:            proof.Witness,
		})
	}
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	return manifest, nil
}
