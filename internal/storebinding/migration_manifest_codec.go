package storebinding

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/coordclass"
)

// The durable manifest form is explicit. ClassSet and WorkScope keep their
// members unexported so a caller cannot forge one, which also means reflection
// -based encoders read them as empty and write them back as empty — a silent
// loss of exactly the fields that say which classes a fence covers. Every
// record type therefore has a wire twin, and decoding runs the same
// constructors and validation the in-memory value does, so a field this codec
// forgets shows up as a validation failure rather than as a quietly emptier
// record.

func classNamed(name string) (coordclass.Class, error) {
	for _, class := range coordclass.Classes() {
		if class.String() == name {
			return class, nil
		}
	}
	return 0, fmt.Errorf("%w: %q", ErrUnsupportedClass, name)
}

func toWireClassSet(classes ClassSet) []string {
	names := make([]string, 0, len(classes.Classes()))
	for _, class := range classes.Classes() {
		names = append(names, class.String())
	}
	return names
}

func decodeWireClassSet(names []string) (ClassSet, error) {
	classes := make([]coordclass.Class, 0, len(names))
	for _, name := range names {
		class, err := classNamed(name)
		if err != nil {
			return ClassSet{}, err
		}
		classes = append(classes, class)
	}
	return NewClassSet(classes...)
}

type wireWorkScope struct {
	HQ  bool   `json:"hq"`
	Rig string `json:"rig,omitempty"`
}

func toWireWorkScope(scope WorkScope) wireWorkScope {
	rig, _ := scope.Rig()
	return wireWorkScope{HQ: scope.IsHQ(), Rig: rig}
}

func (w wireWorkScope) decode() WorkScope {
	if w.HQ {
		return HQScope()
	}
	return RigScope(w.Rig)
}

type wireRetainedSource struct {
	Version                 uint16   `json:"version"`
	Provider                string   `json:"provider"`
	ImplementationVersion   string   `json:"implementation_version"`
	Component               string   `json:"component"`
	Classes                 []string `json:"classes"`
	SemanticContractVersion string   `json:"semantic_contract_version"`
	Format                  string   `json:"format"`
	SchemaVersion           string   `json:"schema_version"`
	ABIVersion              string   `json:"abi_version"`
	PhysicalIdentity        string   `json:"physical_identity"`
	ConfigRefDigest         string   `json:"config_ref_digest"`
	WitnessVersion          string   `json:"witness_version"`
	WitnessDigest           string   `json:"witness_digest"`
	ReopenData              []byte   `json:"reopen_data,omitempty"`
}

func toWireRetainedSource(source RetainedSourceRef) wireRetainedSource {
	return wireRetainedSource{
		Version:                 source.Version,
		Provider:                string(source.Provider),
		ImplementationVersion:   source.ImplementationVersion,
		Component:               string(source.Component),
		Classes:                 toWireClassSet(source.Classes),
		SemanticContractVersion: string(source.SemanticContractVersion),
		Format:                  string(source.Format),
		SchemaVersion:           source.SchemaVersion,
		ABIVersion:              source.ABIVersion,
		PhysicalIdentity:        string(source.PhysicalIdentity),
		ConfigRefDigest:         string(source.ConfigRefDigest),
		WitnessVersion:          source.WitnessVersion,
		WitnessDigest:           source.WitnessDigest,
		ReopenData:              append([]byte(nil), source.ReopenData...),
	}
}

func (w wireRetainedSource) decode() (RetainedSourceRef, error) {
	classes, err := decodeWireClassSet(w.Classes)
	if err != nil {
		return RetainedSourceRef{}, err
	}
	return RetainedSourceRef{
		Version:                 w.Version,
		Provider:                ProviderID(w.Provider),
		ImplementationVersion:   w.ImplementationVersion,
		Component:               ComponentID(w.Component),
		Classes:                 classes,
		SemanticContractVersion: ContractVersion(w.SemanticContractVersion),
		Format:                  FormatID(w.Format),
		SchemaVersion:           w.SchemaVersion,
		ABIVersion:              w.ABIVersion,
		PhysicalIdentity:        PhysicalIdentity(w.PhysicalIdentity),
		ConfigRefDigest:         ConfigRefDigest(w.ConfigRefDigest),
		WitnessVersion:          w.WitnessVersion,
		WitnessDigest:           w.WitnessDigest,
		ReopenData:              append([]byte(nil), w.ReopenData...),
	}, nil
}

type wireComponentDescriptor struct {
	ID               string      `json:"id"`
	Locator          string      `json:"locator"`
	PhysicalIdentity string      `json:"physical_identity"`
	Classes          []string    `json:"classes"`
	Format           string      `json:"format"`
	SchemaVersion    string      `json:"schema_version"`
	ABIVersion       string      `json:"abi_version"`
	Marker           MarkerState `json:"marker"`
}

type wireDescriptor struct {
	Version                 uint16                    `json:"version"`
	SemanticContractVersion string                    `json:"semantic_contract_version"`
	Provider                string                    `json:"provider"`
	ImplementationVersion   string                    `json:"implementation_version"`
	Components              []wireComponentDescriptor `json:"components"`
	Capabilities            ClassCapabilities         `json:"capabilities"`
	ConfigRefDigest         string                    `json:"config_ref_digest"`
	RetainedSource          *wireRetainedSource       `json:"retained_source,omitempty"`
}

func toWireDescriptor(descriptor Descriptor) wireDescriptor {
	out := wireDescriptor{
		Version:                 descriptor.Version,
		SemanticContractVersion: string(descriptor.SemanticContractVersion),
		Provider:                string(descriptor.Provider),
		ImplementationVersion:   descriptor.ImplementationVersion,
		Capabilities:            descriptor.Capabilities,
		ConfigRefDigest:         string(descriptor.ConfigRefDigest),
	}
	for _, component := range descriptor.Components {
		out.Components = append(out.Components, wireComponentDescriptor{
			ID:               string(component.ID),
			Locator:          string(component.Locator),
			PhysicalIdentity: string(component.PhysicalIdentity),
			Classes:          toWireClassSet(component.Classes),
			Format:           string(component.Format),
			SchemaVersion:    component.SchemaVersion,
			ABIVersion:       component.ABIVersion,
			Marker:           component.Marker,
		})
	}
	if descriptor.RetainedSource != nil {
		retained := toWireRetainedSource(*descriptor.RetainedSource)
		out.RetainedSource = &retained
	}
	return out
}

func (w wireDescriptor) decode() (Descriptor, error) {
	descriptor := Descriptor{
		Version:                 w.Version,
		SemanticContractVersion: ContractVersion(w.SemanticContractVersion),
		Provider:                ProviderID(w.Provider),
		ImplementationVersion:   w.ImplementationVersion,
		Capabilities:            w.Capabilities,
		ConfigRefDigest:         ConfigRefDigest(w.ConfigRefDigest),
	}
	for _, component := range w.Components {
		classes, err := decodeWireClassSet(component.Classes)
		if err != nil {
			return Descriptor{}, err
		}
		descriptor.Components = append(descriptor.Components, ComponentDescriptor{
			ID:               ComponentID(component.ID),
			Locator:          ComponentLocator(component.Locator),
			PhysicalIdentity: PhysicalIdentity(component.PhysicalIdentity),
			Classes:          classes,
			Format:           FormatID(component.Format),
			SchemaVersion:    component.SchemaVersion,
			ABIVersion:       component.ABIVersion,
			Marker:           component.Marker,
		})
	}
	if w.RetainedSource != nil {
		retained, err := w.RetainedSource.decode()
		if err != nil {
			return Descriptor{}, err
		}
		descriptor.RetainedSource = &retained
	}
	return descriptor, nil
}

type wireNamedDescriptor struct {
	Name       string         `json:"name"`
	Descriptor wireDescriptor `json:"descriptor"`
}

func toWireNamedDescriptors(descriptors []NamedDescriptor) []wireNamedDescriptor {
	if descriptors == nil {
		return nil
	}
	out := make([]wireNamedDescriptor, 0, len(descriptors))
	for _, descriptor := range descriptors {
		out = append(out, wireNamedDescriptor{Name: string(descriptor.Name), Descriptor: toWireDescriptor(descriptor.Descriptor)})
	}
	return out
}

func decodeWireNamedDescriptors(wire []wireNamedDescriptor) ([]NamedDescriptor, error) {
	if wire == nil {
		return nil, nil
	}
	out := make([]NamedDescriptor, 0, len(wire))
	for _, entry := range wire {
		descriptor, err := entry.Descriptor.decode()
		if err != nil {
			return nil, err
		}
		out = append(out, NamedDescriptor{Name: BindingName(entry.Name), Descriptor: descriptor})
	}
	return out, nil
}

type wireFenceComponentTarget struct {
	ID               string   `json:"id"`
	Locator          string   `json:"locator"`
	PhysicalIdentity string   `json:"physical_identity"`
	Classes          []string `json:"classes"`
}

type wireFenceTarget struct {
	Version    uint16                     `json:"version"`
	Provider   string                     `json:"provider"`
	Classes    []string                   `json:"classes"`
	Components []wireFenceComponentTarget `json:"components"`
}

func toWireFenceTarget(target FenceTarget) wireFenceTarget {
	out := wireFenceTarget{Version: target.Version, Provider: string(target.Provider), Classes: toWireClassSet(target.Classes)}
	for _, component := range target.Components {
		out.Components = append(out.Components, wireFenceComponentTarget{
			ID:               string(component.ID),
			Locator:          string(component.Locator),
			PhysicalIdentity: string(component.PhysicalIdentity),
			Classes:          toWireClassSet(component.Classes),
		})
	}
	return out
}

func (w wireFenceTarget) decode() (FenceTarget, error) {
	classes, err := decodeWireClassSet(w.Classes)
	if err != nil {
		return FenceTarget{}, err
	}
	target := FenceTarget{Version: w.Version, Provider: ProviderID(w.Provider), Classes: classes}
	for _, component := range w.Components {
		componentClasses, err := decodeWireClassSet(component.Classes)
		if err != nil {
			return FenceTarget{}, err
		}
		target.Components = append(target.Components, FenceComponentTarget{
			ID:               ComponentID(component.ID),
			Locator:          ComponentLocator(component.Locator),
			PhysicalIdentity: PhysicalIdentity(component.PhysicalIdentity),
			Classes:          componentClasses,
		})
	}
	return target, nil
}

type wireNamedFenceTarget struct {
	Name   string          `json:"name"`
	Role   uint8           `json:"role"`
	Target wireFenceTarget `json:"target"`
}

type wireWorkWorkspaceMember struct {
	Scope            wireWorkScope `json:"scope"`
	Prefix           string        `json:"prefix"`
	ConfigContext    string        `json:"config_context"`
	Suspended        bool          `json:"suspended"`
	ConfigOrder      int           `json:"config_order"`
	Provider         string        `json:"provider"`
	Component        string        `json:"component"`
	PhysicalIdentity string        `json:"physical_identity"`
}

type wireWorkParticipant struct {
	Provider         string                    `json:"provider"`
	Component        string                    `json:"component"`
	PhysicalIdentity string                    `json:"physical_identity"`
	Members          []wireWorkWorkspaceMember `json:"members"`
}

func toWireWorkParticipant(participant WorkWorkspaceParticipant) wireWorkParticipant {
	out := wireWorkParticipant{
		Provider:         string(participant.Provider),
		Component:        string(participant.Component),
		PhysicalIdentity: string(participant.PhysicalIdentity),
	}
	for _, member := range participant.Members {
		out.Members = append(out.Members, wireWorkWorkspaceMember{
			Scope:            toWireWorkScope(member.Scope),
			Prefix:           member.Prefix,
			ConfigContext:    string(member.ConfigContext),
			Suspended:        member.Suspended,
			ConfigOrder:      member.ConfigOrder,
			Provider:         string(member.Provider),
			Component:        string(member.Component),
			PhysicalIdentity: string(member.PhysicalIdentity),
		})
	}
	return out
}

func (w wireWorkParticipant) decode() WorkWorkspaceParticipant {
	participant := WorkWorkspaceParticipant{
		Provider:         ProviderID(w.Provider),
		Component:        ComponentID(w.Component),
		PhysicalIdentity: PhysicalIdentity(w.PhysicalIdentity),
	}
	for _, member := range w.Members {
		participant.Members = append(participant.Members, WorkWorkspaceMember{
			Scope:            member.Scope.decode(),
			Prefix:           member.Prefix,
			ConfigContext:    ConfigRefDigest(member.ConfigContext),
			Suspended:        member.Suspended,
			ConfigOrder:      member.ConfigOrder,
			Provider:         ProviderID(member.Provider),
			Component:        ComponentID(member.Component),
			PhysicalIdentity: PhysicalIdentity(member.PhysicalIdentity),
		})
	}
	return participant
}

type wireWorkPreparation struct {
	Version             uint16              `json:"version"`
	Attempt             string              `json:"attempt"`
	Generation          uint64              `json:"generation"`
	Direction           uint8               `json:"direction"`
	Participant         wireWorkParticipant `json:"participant"`
	SourceIdentity      string              `json:"source_identity"`
	DestinationIdentity string              `json:"destination_identity"`
	WitnessVersion      string              `json:"witness_version"`
	ConfigDigest        string              `json:"config_digest"`
}

func toWireWorkPreparation(identity WorkPreparationIdentity) wireWorkPreparation {
	return wireWorkPreparation{
		Version:             identity.Version,
		Attempt:             string(identity.Attempt),
		Generation:          uint64(identity.Generation),
		Direction:           uint8(identity.Direction),
		Participant:         toWireWorkParticipant(identity.Participant),
		SourceIdentity:      string(identity.SourceIdentity),
		DestinationIdentity: string(identity.DestinationIdentity),
		WitnessVersion:      identity.WitnessVersion,
		ConfigDigest:        string(identity.ConfigDigest),
	}
}

func (w wireWorkPreparation) decode() WorkPreparationIdentity {
	return WorkPreparationIdentity{
		Version:             w.Version,
		Attempt:             AttemptID(w.Attempt),
		Generation:          Generation(w.Generation),
		Direction:           WorkMigrationDirection(w.Direction),
		Participant:         w.Participant.decode(),
		SourceIdentity:      BindingIdentity(w.SourceIdentity),
		DestinationIdentity: BindingIdentity(w.DestinationIdentity),
		WitnessVersion:      w.WitnessVersion,
		ConfigDigest:        ConfigRefDigest(w.ConfigDigest),
	}
}

type wireParticipantReceipt struct {
	Version            uint16               `json:"version"`
	Kind               uint8                `json:"kind"`
	Attempt            string               `json:"attempt"`
	Generation         uint64               `json:"generation"`
	Participant        string               `json:"participant"`
	DescriptorIdentity string               `json:"descriptor_identity"`
	ReceiptID          string               `json:"receipt_id"`
	Preparation        *wireWorkPreparation `json:"preparation,omitempty"`
	PreparedReceipt    string               `json:"prepared_receipt,omitempty"`
}

func toWireParticipantReceipt(receipt ParticipantReceipt) wireParticipantReceipt {
	out := wireParticipantReceipt{
		Version:            receipt.Version,
		Kind:               uint8(receipt.Kind),
		Attempt:            string(receipt.Attempt),
		Generation:         uint64(receipt.Generation),
		Participant:        receipt.Participant,
		DescriptorIdentity: string(receipt.DescriptorIdentity),
		ReceiptID:          receipt.ReceiptID,
		PreparedReceipt:    receipt.PreparedReceipt,
	}
	if !receipt.Preparation.IsZero() {
		preparation := toWireWorkPreparation(receipt.Preparation)
		out.Preparation = &preparation
	}
	return out
}

func (w wireParticipantReceipt) decode() ParticipantReceipt {
	receipt := ParticipantReceipt{
		Version:            w.Version,
		Kind:               ParticipantReceiptKind(w.Kind),
		Attempt:            AttemptID(w.Attempt),
		Generation:         Generation(w.Generation),
		Participant:        w.Participant,
		DescriptorIdentity: BindingIdentity(w.DescriptorIdentity),
		ReceiptID:          w.ReceiptID,
		PreparedReceipt:    w.PreparedReceipt,
	}
	if w.Preparation != nil {
		receipt.Preparation = w.Preparation.decode()
	}
	return receipt
}

type wireGuardReceipt struct {
	Version                     uint16                  `json:"version"`
	Attempt                     string                  `json:"attempt"`
	Generation                  uint64                  `json:"generation"`
	Provider                    string                  `json:"provider"`
	Component                   string                  `json:"component"`
	PhysicalIdentity            string                  `json:"physical_identity"`
	Classes                     []string                `json:"classes"`
	SemanticContractVersion     string                  `json:"semantic_contract_version"`
	Role                        uint8                   `json:"role"`
	TransferState               uint8                   `json:"transfer_state,omitempty"`
	TransferParticipant         string                  `json:"transfer_participant,omitempty"`
	TransferDestinationIdentity string                  `json:"transfer_destination_identity,omitempty"`
	TransferReceiptKind         uint8                   `json:"transfer_receipt_kind,omitempty"`
	ActiveProof                 *wireParticipantReceipt `json:"active_proof,omitempty"`
	ReceiptID                   string                  `json:"receipt_id"`
	Revalidation                string                  `json:"revalidation"`
}

func toWireGuardReceipt(receipt GuardReceipt) wireGuardReceipt {
	out := wireGuardReceipt{
		Version:                     receipt.Version,
		Attempt:                     string(receipt.Attempt),
		Generation:                  uint64(receipt.Generation),
		Provider:                    string(receipt.Provider),
		Component:                   string(receipt.Component),
		PhysicalIdentity:            string(receipt.PhysicalIdentity),
		Classes:                     toWireClassSet(receipt.Classes),
		SemanticContractVersion:     string(receipt.SemanticContractVersion),
		Role:                        uint8(receipt.Role),
		TransferState:               uint8(receipt.TransferState),
		TransferParticipant:         receipt.TransferParticipant,
		TransferDestinationIdentity: string(receipt.TransferDestinationIdentity),
		TransferReceiptKind:         uint8(receipt.TransferReceiptKind),
		ReceiptID:                   receipt.ReceiptID,
		Revalidation:                receipt.Revalidation,
	}
	if receipt.ActiveProof != nil {
		proof := toWireParticipantReceipt(*receipt.ActiveProof)
		out.ActiveProof = &proof
	}
	return out
}

func (w wireGuardReceipt) decode() (GuardReceipt, error) {
	classes, err := decodeWireClassSet(w.Classes)
	if err != nil {
		return GuardReceipt{}, err
	}
	receipt := GuardReceipt{
		Version:                     w.Version,
		Attempt:                     AttemptID(w.Attempt),
		Generation:                  Generation(w.Generation),
		Provider:                    ProviderID(w.Provider),
		Component:                   ComponentID(w.Component),
		PhysicalIdentity:            PhysicalIdentity(w.PhysicalIdentity),
		Classes:                     classes,
		SemanticContractVersion:     ContractVersion(w.SemanticContractVersion),
		Role:                        GuardRole(w.Role),
		TransferState:               GuardTransferState(w.TransferState),
		TransferParticipant:         w.TransferParticipant,
		TransferDestinationIdentity: BindingIdentity(w.TransferDestinationIdentity),
		TransferReceiptKind:         ParticipantReceiptKind(w.TransferReceiptKind),
		ReceiptID:                   w.ReceiptID,
		Revalidation:                w.Revalidation,
	}
	if w.ActiveProof != nil {
		proof := w.ActiveProof.decode()
		receipt.ActiveProof = &proof
	}
	return receipt, nil
}

type wireSemanticWitness struct {
	Version   uint16               `json:"version"`
	Class     string               `json:"class"`
	Contract  string               `json:"contract"`
	Algorithm string               `json:"algorithm"`
	Digest    string               `json:"digest"`
	Families  []WitnessFamilyCount `json:"families"`
}

func toWireSemanticWitness(witness SemanticWitness) wireSemanticWitness {
	return wireSemanticWitness{
		Version:   witness.Version,
		Class:     witness.Class.String(),
		Contract:  string(witness.Contract),
		Algorithm: witness.Algorithm,
		Digest:    witness.Digest,
		Families:  append([]WitnessFamilyCount(nil), witness.Families...),
	}
}

func (w wireSemanticWitness) decode() (SemanticWitness, error) {
	class, err := classNamed(w.Class)
	if err != nil {
		return SemanticWitness{}, err
	}
	return SemanticWitness{
		Version:   w.Version,
		Class:     class,
		Contract:  ContractVersion(w.Contract),
		Algorithm: w.Algorithm,
		Digest:    w.Digest,
		Families:  append([]WitnessFamilyCount(nil), w.Families...),
	}, nil
}

type wirePhysicalFact struct {
	Component string `json:"component"`
	Kind      uint8  `json:"kind"`
	Value     string `json:"value"`
}

type wireWitnessEnvelope struct {
	Version        uint16             `json:"version"`
	Descriptor     wireDescriptor     `json:"descriptor"`
	Identity       string             `json:"identity"`
	SemanticDigest string             `json:"semantic_digest"`
	PhysicalFacts  []wirePhysicalFact `json:"physical_facts"`
	Digest         string             `json:"digest"`
}

func toWireWitnessEnvelope(envelope WitnessEnvelope) wireWitnessEnvelope {
	out := wireWitnessEnvelope{
		Version:        envelope.Version,
		Descriptor:     toWireDescriptor(envelope.Descriptor),
		Identity:       string(envelope.Identity),
		SemanticDigest: envelope.SemanticDigest,
		Digest:         envelope.Digest,
	}
	for _, fact := range envelope.PhysicalFacts {
		out.PhysicalFacts = append(out.PhysicalFacts, wirePhysicalFact{Component: string(fact.Component), Kind: uint8(fact.Kind), Value: fact.Value})
	}
	return out
}

func (w wireWitnessEnvelope) decode() (WitnessEnvelope, error) {
	descriptor, err := w.Descriptor.decode()
	if err != nil {
		return WitnessEnvelope{}, err
	}
	envelope := WitnessEnvelope{
		Version:        w.Version,
		Descriptor:     descriptor,
		Identity:       BindingIdentity(w.Identity),
		SemanticDigest: w.SemanticDigest,
		Digest:         w.Digest,
	}
	for _, fact := range w.PhysicalFacts {
		envelope.PhysicalFacts = append(envelope.PhysicalFacts, ComponentPhysicalFact{
			Component: ComponentID(fact.Component),
			Kind:      PhysicalFactKind(fact.Kind),
			Value:     fact.Value,
		})
	}
	return envelope, nil
}

type wireClassWitnessRecord struct {
	Class               string              `json:"class"`
	Source              wireSemanticWitness `json:"source"`
	Destination         wireSemanticWitness `json:"destination"`
	FreshReopen         wireSemanticWitness `json:"fresh_reopen"`
	AdmittedEnvelope    wireWitnessEnvelope `json:"admitted_envelope"`
	FreshReopenEnvelope wireWitnessEnvelope `json:"fresh_reopen_envelope"`
}

func toWireClassWitnessRecord(record ClassWitnessRecord) wireClassWitnessRecord {
	return wireClassWitnessRecord{
		Class:               record.Class.String(),
		Source:              toWireSemanticWitness(record.Source),
		Destination:         toWireSemanticWitness(record.Destination),
		FreshReopen:         toWireSemanticWitness(record.FreshReopen),
		AdmittedEnvelope:    toWireWitnessEnvelope(record.AdmittedEnvelope),
		FreshReopenEnvelope: toWireWitnessEnvelope(record.FreshReopenEnvelope),
	}
}

func (w wireClassWitnessRecord) decode() (ClassWitnessRecord, error) {
	class, err := classNamed(w.Class)
	if err != nil {
		return ClassWitnessRecord{}, err
	}
	record := ClassWitnessRecord{Class: class}
	for _, pair := range []struct {
		wire   wireSemanticWitness
		target *SemanticWitness
	}{
		{w.Source, &record.Source},
		{w.Destination, &record.Destination},
		{w.FreshReopen, &record.FreshReopen},
	} {
		witness, err := pair.wire.decode()
		if err != nil {
			return ClassWitnessRecord{}, err
		}
		*pair.target = witness
	}
	for _, pair := range []struct {
		wire   wireWitnessEnvelope
		target *WitnessEnvelope
	}{
		{w.AdmittedEnvelope, &record.AdmittedEnvelope},
		{w.FreshReopenEnvelope, &record.FreshReopenEnvelope},
	} {
		envelope, err := pair.wire.decode()
		if err != nil {
			return ClassWitnessRecord{}, err
		}
		*pair.target = envelope
	}
	return record, nil
}

func toWireAssignments(assignments map[coordclass.Class]BindingName) map[string]string {
	if assignments == nil {
		return nil
	}
	out := make(map[string]string, len(assignments))
	for class, name := range assignments {
		out[class.String()] = string(name)
	}
	return out
}

func decodeWireAssignments(wire map[string]string) (map[coordclass.Class]BindingName, error) {
	if wire == nil {
		return nil, nil
	}
	out := make(map[coordclass.Class]BindingName, len(wire))
	for name, binding := range wire {
		class, err := classNamed(name)
		if err != nil {
			return nil, err
		}
		out[class] = BindingName(binding)
	}
	return out, nil
}

func toWireDigests(digests map[BindingName]ConfigRefDigest) map[string]string {
	if digests == nil {
		return nil
	}
	out := make(map[string]string, len(digests))
	for name, digest := range digests {
		out[string(name)] = string(digest)
	}
	return out
}

func decodeWireDigests(wire map[string]string) map[BindingName]ConfigRefDigest {
	if wire == nil {
		return nil
	}
	out := make(map[BindingName]ConfigRefDigest, len(wire))
	for name, digest := range wire {
		out[BindingName(name)] = ConfigRefDigest(digest)
	}
	return out
}
