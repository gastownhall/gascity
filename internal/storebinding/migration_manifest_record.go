package storebinding

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/coordclass"
)

// IntentSection is the INTENT_FSYNCED field set. Every fact recovery needs
// before it may touch a fence or a destination lives here, pinned by locator
// and descriptor rather than by mutable binding name.
type IntentSection struct {
	Version              uint16
	Attempt              AttemptID
	PriorGeneration      Generation
	DesiredGeneration    Generation
	Genesis              *GenesisEvidence
	PriorAssignments     map[coordclass.Class]BindingName
	DesiredAssignments   map[coordclass.Class]BindingName
	Moves                []ClassMove
	FenceTargets         []NamedFenceTarget
	Descriptors          []NamedDescriptor
	RetainedSources      []RetainedSourceRef
	Participants         ParticipantSet
	ConfigDigest         CompositeConfigDigest
	BindingConfigDigests map[BindingName]ConfigRefDigest
	WitnessAlgorithm     string
}

// Clone returns a detached intent section.
func (s IntentSection) Clone() IntentSection {
	out := s
	if s.Genesis != nil {
		genesis := s.Genesis.Clone()
		out.Genesis = &genesis
	}
	out.PriorAssignments = cloneAssignments(s.PriorAssignments)
	out.DesiredAssignments = cloneAssignments(s.DesiredAssignments)
	out.Moves = append([]ClassMove(nil), s.Moves...)
	out.FenceTargets = cloneNamedTargets(s.FenceTargets)
	out.Descriptors = cloneNamedDescriptors(s.Descriptors)
	out.RetainedSources = cloneRetainedSources(s.RetainedSources)
	out.Participants = s.Participants.Clone()
	out.BindingConfigDigests = cloneBindingDigests(s.BindingConfigDigests)
	return out
}

// Validate rejects an intent section that cannot support recovery.
func (s IntentSection) Validate() error {
	if s.Version != 1 {
		return fmt.Errorf("%w: intent section has no version", ErrInvalidManifest)
	}
	if strings.TrimSpace(string(s.Attempt)) == "" {
		return fmt.Errorf("%w: intent section has no attempt identity", ErrInvalidManifest)
	}
	if err := validateSecretFree("attempt identity", string(s.Attempt)); err != nil {
		return err
	}
	if !s.DesiredGeneration.Valid() {
		return fmt.Errorf("%w: intent section has no desired generation", ErrInvalidManifest)
	}
	if s.PriorGeneration.Valid() && s.PriorGeneration >= s.DesiredGeneration {
		return fmt.Errorf("%w: desired generation %d does not advance prior generation %d", ErrInvalidManifest, s.DesiredGeneration, s.PriorGeneration)
	}
	if !s.PriorGeneration.Valid() && s.Genesis == nil && !intentSectionAdopts(s.Moves) {
		return fmt.Errorf("%w: intent section has neither a prior generation nor genesis evidence", ErrInvalidManifest)
	}
	if s.Genesis != nil {
		if err := s.Genesis.Validate(); err != nil {
			return err
		}
	}
	if err := validateAssignments("desired", s.DesiredAssignments); err != nil {
		return err
	}
	if s.PriorGeneration.Valid() {
		if err := validateAssignments("prior", s.PriorAssignments); err != nil {
			return err
		}
	}
	if len(s.Moves) != len(coordclass.Classes()) {
		return fmt.Errorf("%w: intent section derives %d of %d classes", ErrInvalidManifest, len(s.Moves), len(coordclass.Classes()))
	}
	moved := make(map[coordclass.Class]struct{}, len(s.Moves))
	for _, move := range s.Moves {
		if err := move.Validate(); err != nil {
			return err
		}
		if _, exists := moved[move.Class]; exists {
			return fmt.Errorf("%w: intent section derives class %s twice", ErrInvalidManifest, move.Class)
		}
		moved[move.Class] = struct{}{}
		if s.DesiredAssignments[move.Class] != move.DesiredBinding {
			return fmt.Errorf("%w: class %s move targets %q but the desired map names %q", ErrInvalidManifest, move.Class, move.DesiredBinding, s.DesiredAssignments[move.Class])
		}
	}
	if len(s.FenceTargets) == 0 {
		return fmt.Errorf("%w: intent section pins no fence target", ErrInvalidManifest)
	}
	seenTargets := make(map[string]struct{}, len(s.FenceTargets))
	for _, target := range s.FenceTargets {
		if err := target.Validate(); err != nil {
			return err
		}
		key := string(target.Name) + "\x00" + target.Role.String()
		if _, exists := seenTargets[key]; exists {
			return fmt.Errorf("%w: binding %q pins the %s target twice", ErrInvalidManifest, target.Name, target.Role)
		}
		seenTargets[key] = struct{}{}
	}
	if err := ValidateNoDescriptorOverlap(s.Descriptors); err != nil {
		return err
	}
	for _, descriptor := range s.Descriptors {
		if err := descriptor.Descriptor.Validate(); err != nil {
			return err
		}
	}
	for _, source := range s.RetainedSources {
		if err := source.Validate(); err != nil {
			return err
		}
	}
	if err := s.Participants.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(string(s.ConfigDigest)) == "" {
		return fmt.Errorf("%w: intent section has no composite config digest", ErrInvalidManifest)
	}
	if err := validateCanonicalSHA256Digest("composite config digest", string(s.ConfigDigest)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidManifest, err)
	}
	if len(s.BindingConfigDigests) == 0 {
		return fmt.Errorf("%w: intent section records no per-binding config digest", ErrInvalidManifest)
	}
	for name, digest := range s.BindingConfigDigests {
		if err := validateIdentifier("binding name", string(name)); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidManifest, err)
		}
		if err := validateCanonicalSHA256Digest("binding config digest", string(digest)); err != nil {
			return fmt.Errorf("%w: binding %q: %w", ErrInvalidManifest, name, err)
		}
	}
	if strings.TrimSpace(s.WitnessAlgorithm) == "" {
		return fmt.Errorf("%w: intent section pins no witness algorithm version", ErrInvalidManifest)
	}
	return validateSecretFree("witness algorithm", s.WitnessAlgorithm)
}

func intentSectionAdopts(moves []ClassMove) bool {
	for _, move := range moves {
		if move.Kind == ClassMoveInPlaceAdoption {
			return true
		}
	}
	return false
}

func validateAssignments(label string, assignments map[coordclass.Class]BindingName) error {
	for _, class := range coordclass.Classes() {
		name, assigned := assignments[class]
		if !assigned {
			return fmt.Errorf("%w: %s class map assigns no binding to %s", ErrInvalidManifest, label, class)
		}
		if err := validateIdentifier("binding name", string(name)); err != nil {
			return fmt.Errorf("%w: %s class map: %w", ErrInvalidManifest, label, err)
		}
	}
	return nil
}

// intentSection projects a derived migrate intent into the durable
// INTENT_FSYNCED field set. It exists so the record cannot be assembled from
// anything but a derivation.
func (i MigrationIntent) intentSection() (IntentSection, error) {
	if i.outcome != IntentOutcomeMigrate {
		return IntentSection{}, fmt.Errorf("%w: outcome %s does not start a saga", ErrInvalidMigrationIntent, i.outcome)
	}
	section := IntentSection{
		Version:              1,
		Attempt:              i.attempt,
		PriorGeneration:      i.priorGeneration,
		DesiredGeneration:    i.desiredGeneration,
		Genesis:              i.Genesis(),
		PriorAssignments:     i.PriorAssignments(),
		DesiredAssignments:   i.DesiredAssignments(),
		Moves:                i.Moves(),
		FenceTargets:         i.FenceTargets(),
		Descriptors:          i.Descriptors(),
		RetainedSources:      i.RetainedSources(),
		Participants:         i.Participants(),
		ConfigDigest:         i.configDigest,
		BindingConfigDigests: i.BindingConfigDigests(),
		WitnessAlgorithm:     i.witnessAlgorithm,
	}
	if err := section.Validate(); err != nil {
		return IntentSection{}, err
	}
	return section, nil
}

// ComponentCensus is one component's fenced final census. The byte length and
// digest are what make "unchanged source" checkable on recovery instead of
// assumed.
type ComponentCensus struct {
	Binding           BindingName
	Component         ComponentID
	Locator           ComponentLocator
	PhysicalIdentity  PhysicalIdentity
	ByteLength        uint64
	Digest            string
	NamespaceIdentity string
}

// Validate rejects a census entry that cannot be recompared.
func (c ComponentCensus) Validate() error {
	if err := validateIdentifier("binding name", string(c.Binding)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidManifest, err)
	}
	if err := validateIdentifier("component ID", string(c.Component)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidManifest, err)
	}
	if strings.TrimSpace(string(c.Locator)) == "" || strings.TrimSpace(string(c.PhysicalIdentity)) == "" || strings.TrimSpace(c.NamespaceIdentity) == "" {
		return fmt.Errorf("%w: census of %q is missing locator, physical identity, or namespace identity", ErrInvalidManifest, c.Component)
	}
	if err := validateCanonicalSHA256Digest("component census digest", c.Digest); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidManifest, err)
	}
	for field, value := range map[string]string{
		"census component locator":  string(c.Locator),
		"census physical identity":  string(c.PhysicalIdentity),
		"census namespace identity": c.NamespaceIdentity,
	} {
		if err := validateSecretFree(field, value); err != nil {
			return err
		}
	}
	return nil
}

// ClassInventory is one class's source inventory and duplicate-residence result.
type ClassInventory struct {
	Class              coordclass.Class
	Binding            BindingName
	Component          ComponentID
	PhysicalIdentity   PhysicalIdentity
	Population         BindingPopulation
	DuplicateResidence bool
}

// Validate rejects an inventory entry that recorded no verdict. A recorded
// duplicate residence is rejected outright: duplicate residence blocks startup,
// so a manifest that carries one forward would be recording a decision the migration specification
// forbids.
func (i ClassInventory) Validate() error {
	if !isKnownClass(i.Class) {
		return fmt.Errorf("%w: %s", ErrUnsupportedClass, i.Class)
	}
	if err := validateIdentifier("binding name", string(i.Binding)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidManifest, err)
	}
	if err := validateIdentifier("component ID", string(i.Component)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidManifest, err)
	}
	if strings.TrimSpace(string(i.PhysicalIdentity)) == "" {
		return fmt.Errorf("%w: class %s inventory has no physical identity", ErrInvalidManifest, i.Class)
	}
	if err := validateSecretFree("inventory physical identity", string(i.PhysicalIdentity)); err != nil {
		return err
	}
	if !i.Population.Valid() {
		return fmt.Errorf("%w: class %s inventory has no population verdict", ErrInvalidManifest, i.Class)
	}
	if i.DuplicateResidence {
		return fmt.Errorf("%w: class %s inventory records duplicate residence", ErrInvalidManifest, i.Class)
	}
	return nil
}

// ClassPhaseState is one class's phase and resumability inside the attempt.
type ClassPhaseState struct {
	Class     coordclass.Class
	Phase     MigrationPhase
	Resumable bool
}

// Validate rejects a class state that names no phase.
func (s ClassPhaseState) Validate() error {
	if !isKnownClass(s.Class) {
		return fmt.Errorf("%w: %s", ErrUnsupportedClass, s.Class)
	}
	if !s.Phase.Valid() {
		return fmt.Errorf("%w: class %s has no recorded phase", ErrInvalidManifest, s.Class)
	}
	return nil
}

// HoldKind is the closed set of holds an attempt places on a physical component.
type HoldKind uint8

const (
	// HoldUnspecified is the zero value and never a recorded hold.
	HoldUnspecified HoldKind = iota
	// HoldRetention keeps a retained source from being reclaimed.
	HoldRetention
	// HoldMaintenance suspends provider maintenance for the attempt's duration.
	HoldMaintenance
)

// Valid reports whether the hold kind belongs to the closed set.
func (k HoldKind) Valid() bool { return k == HoldRetention || k == HoldMaintenance }

// String returns the stable name of a hold kind.
func (k HoldKind) String() string {
	switch k {
	case HoldUnspecified:
		return "unspecified"
	case HoldRetention:
		return "retention"
	case HoldMaintenance:
		return "maintenance"
	default:
		return "invalid"
	}
}

// RetentionHold is one retention or maintenance hold the attempt placed.
type RetentionHold struct {
	Kind             HoldKind
	Binding          BindingName
	Component        ComponentID
	PhysicalIdentity PhysicalIdentity
	Reason           string
}

// Validate rejects a hold that names nothing to hold.
func (h RetentionHold) Validate() error {
	if !h.Kind.Valid() {
		return fmt.Errorf("%w: hold has no kind", ErrInvalidManifest)
	}
	if err := validateIdentifier("binding name", string(h.Binding)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidManifest, err)
	}
	if err := validateIdentifier("component ID", string(h.Component)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidManifest, err)
	}
	if strings.TrimSpace(string(h.PhysicalIdentity)) == "" || strings.TrimSpace(h.Reason) == "" {
		return fmt.Errorf("%w: %s hold on %q has no physical identity or reason", ErrInvalidManifest, h.Kind, h.Component)
	}
	for field, value := range map[string]string{
		"hold physical identity": string(h.PhysicalIdentity),
		"hold reason":            h.Reason,
	} {
		if err := validateSecretFree(field, value); err != nil {
			return err
		}
	}
	return nil
}

// PreparingSection is the PREPARING field set: the fenced source truth every
// later phase compares against.
type PreparingSection struct {
	Version           uint16
	SourceDescriptors []NamedDescriptor
	Census            []ComponentCensus
	Inventory         []ClassInventory
	ClassStates       []ClassPhaseState
	Holds             []RetentionHold
}

// Clone returns a detached preparing section.
func (s PreparingSection) Clone() PreparingSection {
	out := s
	out.SourceDescriptors = cloneNamedDescriptors(s.SourceDescriptors)
	out.Census = append([]ComponentCensus(nil), s.Census...)
	out.Inventory = append([]ClassInventory(nil), s.Inventory...)
	out.ClassStates = append([]ClassPhaseState(nil), s.ClassStates...)
	out.Holds = append([]RetentionHold(nil), s.Holds...)
	return out
}

// Validate rejects a preparing section that leaves any source fact unproved.
func (s PreparingSection) Validate() error {
	if s.Version != 1 {
		return fmt.Errorf("%w: preparing section has no version", ErrInvalidManifest)
	}
	if err := ValidateNoDescriptorOverlap(s.SourceDescriptors); err != nil {
		return err
	}
	for _, descriptor := range s.SourceDescriptors {
		if err := descriptor.Descriptor.Validate(); err != nil {
			return err
		}
	}
	if len(s.Census) == 0 {
		return fmt.Errorf("%w: preparing section records no fenced census", ErrInvalidManifest)
	}
	censused := make(map[PhysicalIdentity]struct{}, len(s.Census))
	for _, entry := range s.Census {
		if err := entry.Validate(); err != nil {
			return err
		}
		if _, exists := censused[entry.PhysicalIdentity]; exists {
			return fmt.Errorf("%w: census records physical component %q twice", ErrInvalidManifest, entry.PhysicalIdentity)
		}
		censused[entry.PhysicalIdentity] = struct{}{}
	}
	if len(s.Inventory) == 0 {
		return fmt.Errorf("%w: preparing section records no source inventory", ErrInvalidManifest)
	}
	inventoried := make(map[coordclass.Class]struct{}, len(s.Inventory))
	for _, entry := range s.Inventory {
		if err := entry.Validate(); err != nil {
			return err
		}
		if _, exists := inventoried[entry.Class]; exists {
			return fmt.Errorf("%w: inventory records class %s twice", ErrInvalidManifest, entry.Class)
		}
		inventoried[entry.Class] = struct{}{}
	}
	if len(s.ClassStates) != len(coordclass.Classes()) {
		return fmt.Errorf("%w: preparing section records %d of %d class states", ErrInvalidManifest, len(s.ClassStates), len(coordclass.Classes()))
	}
	stated := make(map[coordclass.Class]struct{}, len(s.ClassStates))
	for _, state := range s.ClassStates {
		if err := state.Validate(); err != nil {
			return err
		}
		if _, exists := stated[state.Class]; exists {
			return fmt.Errorf("%w: preparing section records class %s state twice", ErrInvalidManifest, state.Class)
		}
		stated[state.Class] = struct{}{}
	}
	for _, hold := range s.Holds {
		if err := hold.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// PreparedSection is the PREPARED field set. It is the only phase that carries
// witnesses, because it is the only phase whose whole job is proving the copy.
type PreparedSection struct {
	Version                uint16
	DestinationDescriptors []NamedDescriptor
	StagedGeneration       *NamedDescriptor
	Witnesses              []ClassWitnessRecord
	WorkPrepared           []WorkPrepared
	WorkProofs             []WorkProof
	RetainedSources        []RetainedSourceRef
}

// Clone returns a detached prepared section.
func (s PreparedSection) Clone() PreparedSection {
	out := s
	out.DestinationDescriptors = cloneNamedDescriptors(s.DestinationDescriptors)
	if s.StagedGeneration != nil {
		staged := NamedDescriptor{Name: s.StagedGeneration.Name, Descriptor: s.StagedGeneration.Descriptor.Clone()}
		out.StagedGeneration = &staged
	}
	if s.Witnesses != nil {
		out.Witnesses = make([]ClassWitnessRecord, 0, len(s.Witnesses))
		for _, witness := range s.Witnesses {
			out.Witnesses = append(out.Witnesses, witness.Clone())
		}
	}
	if s.WorkPrepared != nil {
		out.WorkPrepared = make([]WorkPrepared, 0, len(s.WorkPrepared))
		for _, prepared := range s.WorkPrepared {
			out.WorkPrepared = append(out.WorkPrepared, prepared.Clone())
		}
	}
	if s.WorkProofs != nil {
		out.WorkProofs = make([]WorkProof, 0, len(s.WorkProofs))
		for _, proof := range s.WorkProofs {
			out.WorkProofs = append(out.WorkProofs, proof.Clone())
		}
	}
	out.RetainedSources = cloneRetainedSources(s.RetainedSources)
	return out
}

// Validate checks the prepared section against the attempt's intent: every
// moving infrastructure class must carry a complete witness record, and every
// Work participant must carry both a provider prepare receipt and a provider
// proof.
func (s PreparedSection) Validate(intent IntentSection) error {
	if s.Version != 1 {
		return fmt.Errorf("%w: prepared section has no version", ErrInvalidManifest)
	}
	if len(s.DestinationDescriptors) == 0 {
		return fmt.Errorf("%w: prepared section records no destination descriptor", ErrInvalidManifest)
	}
	if err := ValidateNoDescriptorOverlap(s.DestinationDescriptors); err != nil {
		return err
	}
	for _, descriptor := range s.DestinationDescriptors {
		if err := descriptor.Descriptor.Validate(); err != nil {
			return err
		}
	}
	if s.StagedGeneration != nil {
		if err := s.StagedGeneration.Descriptor.Validate(); err != nil {
			return err
		}
	}
	witnessed := make(map[coordclass.Class]struct{}, len(s.Witnesses))
	for _, witness := range s.Witnesses {
		if err := witness.Validate(intent.WitnessAlgorithm); err != nil {
			return err
		}
		if _, exists := witnessed[witness.Class]; exists {
			return fmt.Errorf("%w: prepared section witnesses class %s twice", ErrInvalidManifest, witness.Class)
		}
		witnessed[witness.Class] = struct{}{}
	}
	for _, move := range intent.Moves {
		if move.Class == coordclass.ClassWork || !move.Kind.RequiresSaga() {
			continue
		}
		if _, proved := witnessed[move.Class]; !proved {
			return fmt.Errorf("%w: class %s moves by %s but the prepared section carries no witness", ErrInvalidManifest, move.Class, move.Kind)
		}
	}
	if err := s.validateWork(intent); err != nil {
		return err
	}
	for _, source := range s.RetainedSources {
		if err := source.Validate(); err != nil {
			return err
		}
		if source.WitnessVersion != intent.WitnessAlgorithm {
			return fmt.Errorf("%w: retained source %q pins witness algorithm %q, attempt uses %q", ErrInvalidManifest, source.Component, source.WitnessVersion, intent.WitnessAlgorithm)
		}
	}
	return nil
}

func (s PreparedSection) validateWork(intent IntentSection) error {
	prepared := make(map[string]WorkPrepared, len(s.WorkPrepared))
	for _, receipt := range s.WorkPrepared {
		if err := receipt.Validate(); err != nil {
			return err
		}
		if receipt.Attempt != intent.Attempt || receipt.Generation != intent.DesiredGeneration {
			return fmt.Errorf("%w: work prepare receipt is bound to another attempt or generation", ErrInvalidManifest)
		}
		if receipt.Preparation.WitnessVersion != intent.WitnessAlgorithm {
			return fmt.Errorf("%w: work preparation pins witness algorithm %q, attempt uses %q", ErrInvalidManifest, receipt.Preparation.WitnessVersion, intent.WitnessAlgorithm)
		}
		key := receipt.Participant.Key()
		if _, exists := prepared[key]; exists {
			return fmt.Errorf("%w: work participant prepared twice", ErrInvalidManifest)
		}
		prepared[key] = receipt
	}
	proved := make(map[string]struct{}, len(s.WorkProofs))
	for _, proof := range s.WorkProofs {
		if err := proof.Validate(); err != nil {
			return err
		}
		key := proof.Participant.Key()
		receipt, exists := prepared[key]
		if !exists {
			return fmt.Errorf("%w: work proof has no matching prepare receipt", ErrInvalidManifest)
		}
		if !proof.Preparation.Equal(receipt.Preparation) || proof.PreparedReceipt != receipt.Receipt {
			return fmt.Errorf("%w: work proof does not preserve its preparation identity", ErrInvalidManifest)
		}
		if _, duplicate := proved[key]; duplicate {
			return fmt.Errorf("%w: work participant proved twice", ErrInvalidManifest)
		}
		proved[key] = struct{}{}
	}
	for _, participant := range intent.Participants.Work {
		key := participant.Key()
		if _, exists := prepared[key]; !exists {
			return fmt.Errorf("%w: work participant %q has no prepare receipt", ErrInvalidManifest, participant.PhysicalIdentity)
		}
		if _, exists := proved[key]; !exists {
			return fmt.Errorf("%w: work participant %q has no provider proof", ErrInvalidManifest, participant.PhysicalIdentity)
		}
	}
	return nil
}

// GuardingSection is the GUARDING field set. The plan is recorded before the
// first install so recovery can run a closed Discover loop instead of assuming
// an invisible guard is absent.
type GuardingSection struct {
	Version  uint16
	Plan     []GuardInstallRequest
	Receipts []GuardReceipt
}

// Clone returns a detached guarding section.
func (s GuardingSection) Clone() GuardingSection {
	out := s
	if s.Receipts != nil {
		out.Receipts = cloneGuardReceipts(s.Receipts)
	}
	if s.Plan != nil {
		out.Plan = make([]GuardInstallRequest, 0, len(s.Plan))
		for _, request := range s.Plan {
			out.Plan = append(out.Plan, request.Clone())
		}
	}
	return out
}

// Validate checks the guard plan and every appended receipt against the attempt.
func (s GuardingSection) Validate(intent IntentSection) error {
	if s.Version != 1 {
		return fmt.Errorf("%w: guarding section has no version", ErrInvalidManifest)
	}
	planned := make(map[guardPlanKey]struct{}, len(s.Plan))
	for _, request := range s.Plan {
		if err := request.Validate(); err != nil {
			return err
		}
		if request.Attempt != intent.Attempt || request.Generation != intent.DesiredGeneration {
			return fmt.Errorf("%w: guard plan entry is bound to another attempt or generation", ErrInvalidManifest)
		}
		key := newGuardPlanKey(request.Source.Provider, request.Component, request.PhysicalIdentity, request.Role)
		if _, exists := planned[key]; exists {
			return fmt.Errorf("%w: guard plan names component %q twice", ErrInvalidManifest, request.Component)
		}
		planned[key] = struct{}{}
	}
	received := make(map[guardPlanKey]struct{}, len(s.Receipts))
	for _, receipt := range s.Receipts {
		if err := receipt.Validate(); err != nil {
			return err
		}
		if receipt.Attempt != intent.Attempt || receipt.Generation != intent.DesiredGeneration {
			return fmt.Errorf("%w: guard receipt is bound to another attempt or generation", ErrInvalidManifest)
		}
		key := newGuardPlanKey(receipt.Provider, receipt.Component, receipt.PhysicalIdentity, receipt.Role)
		if _, exists := planned[key]; !exists {
			return fmt.Errorf("%w: guard receipt for %q is not in the recorded plan", ErrInvalidManifest, receipt.Component)
		}
		if _, exists := received[key]; exists {
			return fmt.Errorf("%w: guard receipt for %q appended twice", ErrInvalidManifest, receipt.Component)
		}
		received[key] = struct{}{}
	}
	return nil
}

// Complete reports whether every planned guard has a durable receipt.
func (s GuardingSection) Complete() bool {
	if len(s.Plan) != len(s.Receipts) {
		return false
	}
	received := make(map[guardPlanKey]struct{}, len(s.Receipts))
	for _, receipt := range s.Receipts {
		received[newGuardPlanKey(receipt.Provider, receipt.Component, receipt.PhysicalIdentity, receipt.Role)] = struct{}{}
	}
	for _, request := range s.Plan {
		if _, exists := received[newGuardPlanKey(request.Source.Provider, request.Component, request.PhysicalIdentity, request.Role)]; !exists {
			return false
		}
	}
	return true
}

type guardPlanKey struct {
	provider  ProviderID
	component ComponentID
	physical  PhysicalIdentity
	role      GuardRole
}

func newGuardPlanKey(provider ProviderID, component ComponentID, physical PhysicalIdentity, role GuardRole) guardPlanKey {
	return guardPlanKey{provider: provider, component: component, physical: physical, role: role}
}

// guardReleaseKey names one durable install receipt rather than one planned
// guard. A release must name the receipt it took off, so a plan key alone would
// let a release of some other receipt for the same guard stand in for it.
type guardReleaseKey struct {
	guard   guardPlanKey
	receipt string
}

func newGuardReleaseKey(provider ProviderID, component ComponentID, physical PhysicalIdentity, role GuardRole, receiptID string) guardReleaseKey {
	return guardReleaseKey{guard: newGuardPlanKey(provider, component, physical, role), receipt: receiptID}
}

func guardReleaseKeyForReceipt(receipt GuardReceipt) guardReleaseKey {
	return newGuardReleaseKey(receipt.Provider, receipt.Component, receipt.PhysicalIdentity, receipt.Role, receipt.ReceiptID)
}

// InPlaceAdoptionRecord carries the InPlaceGuardedActivationAuthority inputs an
// exact same-component adoption uses instead of a guard. The authority value
// itself is fence-bound and minted at activation; the manifest records what it
// will be minted from.
type InPlaceAdoptionRecord struct {
	Participant         string
	SourceIdentity      BindingIdentity
	DestinationIdentity BindingIdentity
}

// Validate rejects an adoption record that does not identify one participant.
func (r InPlaceAdoptionRecord) Validate() error {
	if strings.TrimSpace(r.Participant) == "" {
		return fmt.Errorf("%w: in-place adoption names no participant", ErrInvalidManifest)
	}
	if err := validateSecretFree("in-place adoption participant", r.Participant); err != nil {
		return err
	}
	for field, value := range map[string]BindingIdentity{
		"in-place adoption source identity":      r.SourceIdentity,
		"in-place adoption destination identity": r.DestinationIdentity,
	} {
		if err := validateCanonicalSHA256Digest(field, string(value)); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidManifest, err)
		}
	}
	if r.SourceIdentity != r.DestinationIdentity {
		return fmt.Errorf("%w: in-place adoption of %q names two different aggregate identities", ErrInvalidManifest, r.Participant)
	}
	return nil
}

// GuardsInstalledSection is the GUARDS_INSTALLED field set: the verification
// proof AttestGuardedActivation will re-check at activation time.
type GuardsInstalledSection struct {
	Version   uint16
	Verified  []GuardReceipt
	EmptyPlan bool
	InPlace   []InPlaceAdoptionRecord
}

// Clone returns a detached guards-installed section.
func (s GuardsInstalledSection) Clone() GuardsInstalledSection {
	out := s
	if s.Verified != nil {
		out.Verified = cloneGuardReceipts(s.Verified)
	}
	out.InPlace = append([]InPlaceAdoptionRecord(nil), s.InPlace...)
	return out
}

// Validate proves the recorded plan is complete and verified, or that the empty
// plan is justified by in-place authority. An unjustified empty set is the
// silent path from "no guard was installed" to "no guard was needed", so it is
// rejected rather than defaulted.
func (s GuardsInstalledSection) Validate(guarding GuardingSection) error {
	if s.Version != 1 {
		return fmt.Errorf("%w: guards-installed section has no version", ErrInvalidManifest)
	}
	if s.EmptyPlan != (len(guarding.Plan) == 0) {
		return fmt.Errorf("%w: empty-plan flag disagrees with the recorded guard plan", ErrInvalidManifest)
	}
	if s.EmptyPlan {
		if len(s.Verified) != 0 {
			return fmt.Errorf("%w: empty guard plan carries verified receipts", ErrInvalidManifest)
		}
		if len(s.InPlace) == 0 {
			return fmt.Errorf("%w: empty guard plan carries no in-place adoption authority", ErrInvalidManifest)
		}
		participants := make(map[string]struct{}, len(s.InPlace))
		for _, record := range s.InPlace {
			if err := record.Validate(); err != nil {
				return err
			}
			if _, exists := participants[record.Participant]; exists {
				return fmt.Errorf("%w: in-place adoption names participant %q twice", ErrInvalidManifest, record.Participant)
			}
			participants[record.Participant] = struct{}{}
		}
		return nil
	}
	if len(s.InPlace) != 0 {
		return fmt.Errorf("%w: a non-empty guard plan cannot also claim in-place authority", ErrInvalidManifest)
	}
	verified := make(map[guardPlanKey]GuardReceipt, len(s.Verified))
	for _, receipt := range s.Verified {
		if err := receipt.Validate(); err != nil {
			return err
		}
		verified[newGuardPlanKey(receipt.Provider, receipt.Component, receipt.PhysicalIdentity, receipt.Role)] = receipt
	}
	for _, request := range guarding.Plan {
		key := newGuardPlanKey(request.Source.Provider, request.Component, request.PhysicalIdentity, request.Role)
		receipt, exists := verified[key]
		if !exists {
			return fmt.Errorf("%w: planned guard on %q was never verified", ErrInvalidManifest, request.Component)
		}
		if receipt.Attempt != request.Attempt || receipt.Generation != request.Generation {
			return fmt.Errorf("%w: verified guard on %q is bound to another attempt or generation", ErrInvalidManifest, request.Component)
		}
	}
	if len(verified) != len(guarding.Plan) {
		return fmt.Errorf("%w: verified guard set has %d entries for a %d entry plan", ErrInvalidManifest, len(verified), len(guarding.Plan))
	}
	return nil
}

// CommitDecisionSection is the one fsynced decision record. Its durability is
// the sole authority transition, so it names the closed participant set and
// nothing else may be inferred about membership.
type CommitDecisionSection struct {
	Version      uint16
	Decision     CommitDecision
	Participants []string
}

// Clone returns a detached decision section.
func (s CommitDecisionSection) Clone() CommitDecisionSection {
	out := s
	out.Participants = append([]string(nil), s.Participants...)
	return out
}

// Validate proves the decision closes exactly the participant set the intent
// pinned. A participant not named here is not in the saga.
func (s CommitDecisionSection) Validate(intent IntentSection) error {
	if s.Version != 1 {
		return fmt.Errorf("%w: decision section has no version", ErrInvalidManifest)
	}
	if err := s.Decision.Validate(); err != nil {
		return err
	}
	if s.Decision.Attempt != intent.Attempt {
		return fmt.Errorf("%w: decision names attempt %q, record is attempt %q", ErrInvalidManifest, s.Decision.Attempt, intent.Attempt)
	}
	if s.Decision.Generation != intent.DesiredGeneration {
		return fmt.Errorf("%w: decision names generation %d, attempt desires %d", ErrInvalidManifest, s.Decision.Generation, intent.DesiredGeneration)
	}
	expected := intent.Participants.Keys()
	if len(s.Participants) != len(expected) {
		return fmt.Errorf("%w: decision closes %d participants, the attempt pinned %d", ErrInvalidManifest, len(s.Participants), len(expected))
	}
	recorded := append([]string(nil), s.Participants...)
	sort.Strings(recorded)
	closed := append([]string(nil), expected...)
	sort.Strings(closed)
	for index := range closed {
		if recorded[index] != closed[index] {
			return fmt.Errorf("%w: decision participant set does not equal the pinned participant set", ErrInvalidManifest)
		}
	}
	return nil
}
