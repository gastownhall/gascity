package storebinding

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/coordclass"
)

var (
	// ErrMigrationBlocked reports desired-versus-discovered state that startup
	// must refuse. There is no migration mode, no alternate provider, and no
	// fallback: every block stops startup.
	ErrMigrationBlocked = errors.New("storage startup blocked")
	// ErrInvalidMigrationIntent reports a malformed or underived intent value.
	ErrInvalidMigrationIntent = errors.New("invalid storage migration intent")
	// ErrInvalidBindingDiscovery reports a malformed mutation-free discovery input.
	ErrInvalidBindingDiscovery = errors.New("invalid storage binding discovery")
)

// IntentOutcome is the closed set of startup decisions intent derivation makes. The zero
// value is deliberately not a decision: a caller that reads an unset outcome
// sees IntentOutcomeUnderived rather than a plausible "nothing to do".
type IntentOutcome uint8

const (
	// IntentOutcomeUnderived is the zero value and never a decision.
	IntentOutcomeUnderived IntentOutcome = iota
	// IntentOutcomeNoOp means every desired class assignment and every desired
	// binding identity was proved equal to the durable active generation from
	// complete mutation-free descriptors. activation takes the active-open path.
	IntentOutcomeNoOp
	// IntentOutcomeVerifyActive means every mutation-free fact available matched
	// the active generation but at least one active inspection was incomplete
	// (a live WAL, for example). Incompleteness never counts
	// as identity equality, so this is a separate outcome rather than a no-op
	// with an ignorable field: the fenced inspection step must take a short active-generation
	// verification fence and InspectFenced before activation opens anything.
	IntentOutcomeVerifyActive
	// IntentOutcomeMigrate means a saga must run: genesis, in-place adoption,
	// reconfiguration, relocation, reverse, or the resumption of a recorded
	// nonterminal attempt.
	IntentOutcomeMigrate
	// IntentOutcomeBlocked means startup must stop. A blocked intent is always
	// accompanied by a *MigrationBlockedError, so neither an ignored value nor
	// an ignored error can be mistaken for success.
	IntentOutcomeBlocked
)

// Valid reports whether an outcome is one of the derived decisions.
func (o IntentOutcome) Valid() bool {
	return o >= IntentOutcomeNoOp && o <= IntentOutcomeBlocked
}

// RequiresSaga reports whether the outcome starts or resumes a durable attempt.
func (o IntentOutcome) RequiresSaga() bool { return o == IntentOutcomeMigrate }

// String returns the stable name of an outcome.
func (o IntentOutcome) String() string {
	switch o {
	case IntentOutcomeUnderived:
		return "underived"
	case IntentOutcomeNoOp:
		return "no-op"
	case IntentOutcomeVerifyActive:
		return "verify-active"
	case IntentOutcomeMigrate:
		return "migrate"
	case IntentOutcomeBlocked:
		return "blocked"
	default:
		return "invalid"
	}
}

// BlockReason is the the migration specification blocking taxonomy. None of these degrades
// to a warning and none has a recovery-time guess.
type BlockReason uint8

const (
	// BlockReasonUnspecified is the zero value and never a reason.
	BlockReasonUnspecified BlockReason = iota
	// BlockReasonDamaged reports state that failed a cross-check.
	BlockReasonDamaged
	// BlockReasonIncomplete reports a missing family, field, descriptor, or
	// inspection that a decision would otherwise have to guess at.
	BlockReasonIncomplete
	// BlockReasonDuplicated reports an illegal duplicate or duplicate residence.
	BlockReasonDuplicated
	// BlockReasonConflicting reports state that changed under the protocol, or
	// two records that cannot both be authoritative.
	BlockReasonConflicting
	// BlockReasonStale reports a config-digest or generation mismatch.
	BlockReasonStale
	// BlockReasonUnsupported reports an unknown schema version or unprovable fence.
	BlockReasonUnsupported
	// BlockReasonDangling reports an internally unresolvable reference.
	BlockReasonDangling
	// BlockReasonUnresolvable reports required cross-class state that cannot resolve.
	BlockReasonUnresolvable
)

// Valid reports whether a reason belongs to the closed taxonomy.
func (r BlockReason) Valid() bool {
	return r >= BlockReasonDamaged && r <= BlockReasonUnresolvable
}

// String returns the stable name of a block reason.
func (r BlockReason) String() string {
	switch r {
	case BlockReasonUnspecified:
		return "unspecified"
	case BlockReasonDamaged:
		return "damaged"
	case BlockReasonIncomplete:
		return "incomplete"
	case BlockReasonDuplicated:
		return "duplicated"
	case BlockReasonConflicting:
		return "conflicting"
	case BlockReasonStale:
		return "stale"
	case BlockReasonUnsupported:
		return "unsupported"
	case BlockReasonDangling:
		return "dangling"
	case BlockReasonUnresolvable:
		return "unresolvable"
	default:
		return "invalid"
	}
}

// MigrationBlockedError is the typed, credential-free block report status and
// doctor render. RecordedGeneration is the generation the attempt recorded, so a
// stale-config block can report it without re-resolving through current binding
// names.
type MigrationBlockedError struct {
	Reason             BlockReason
	Subject            string
	Detail             string
	RecordedGeneration Generation
	cause              error
}

// Error implements error.
func (e *MigrationBlockedError) Error() string {
	message := fmt.Sprintf("%s: %s", ErrMigrationBlocked, e.Reason)
	if e.Subject != "" {
		message += fmt.Sprintf(" (%s)", e.Subject)
	}
	if e.Detail != "" {
		message += ": " + e.Detail
	}
	if e.RecordedGeneration.Valid() {
		message += fmt.Sprintf(" [recorded generation %d]", uint64(e.RecordedGeneration))
	}
	return message
}

// Unwrap reports the sentinel and any wrapped cause.
func (e *MigrationBlockedError) Unwrap() []error {
	if e.cause == nil {
		return []error{ErrMigrationBlocked}
	}
	return []error{ErrMigrationBlocked, e.cause}
}

// blockedIncomplete reports the one taxonomy arm derivation itself produces: a
// fact a decision would otherwise have to guess at. Every other arm is either
// carried by a typed error from a validator or built as a literal at the
// cross-check that found it, so each block names the reason at its own site.
func blockedIncomplete(subject, detail string) *MigrationBlockedError {
	return &MigrationBlockedError{Reason: BlockReasonIncomplete, Subject: subject, Detail: detail}
}

func blockedBy(reason BlockReason, subject string, cause error) *MigrationBlockedError {
	detail := ""
	if cause != nil {
		detail = cause.Error()
	}
	return &MigrationBlockedError{Reason: reason, Subject: subject, Detail: detail, cause: cause}
}

// BindingPopulation is the mutation-free census verdict on whether a binding
// already holds authoritative content. The zero value is Unknown and blocks: an
// inspector that does not answer must not be read as "empty", because that
// single default is how a populated legacy store gets silently overwritten.
type BindingPopulation uint8

const (
	// BindingPopulationUnknown is the zero value and always blocks.
	BindingPopulationUnknown BindingPopulation = iota
	// BindingPopulationEmpty proves no component of the binding holds content.
	BindingPopulationEmpty
	// BindingPopulationPopulated proves at least one component holds content.
	BindingPopulationPopulated
)

// Valid reports whether a population verdict was actually made.
func (p BindingPopulation) Valid() bool {
	return p == BindingPopulationEmpty || p == BindingPopulationPopulated
}

// String returns the stable name of a population verdict.
func (p BindingPopulation) String() string {
	switch p {
	case BindingPopulationUnknown:
		return "unknown"
	case BindingPopulationEmpty:
		return "empty"
	case BindingPopulationPopulated:
		return "populated"
	default:
		return "invalid"
	}
}

// DiscoveredBinding is one mutation-free discovery result supplied to intent
// derivation. intent derivation consumes these; it never inspects. The census that produced the
// inspection (the provider's own composite census for the built-in SQLite binding) is the only
// thing that touches a physical component here.
type DiscoveredBinding struct {
	Name       BindingName
	Inspection Inspection
	Population BindingPopulation
}

// Clone returns a detached discovery value.
func (d DiscoveredBinding) Clone() DiscoveredBinding {
	out := d
	out.Inspection.Target = d.Inspection.Target.Clone()
	if d.Inspection.Descriptor != nil {
		cloned := d.Inspection.Descriptor.Clone()
		out.Inspection.Descriptor = &cloned
	}
	return out
}

// Validate rejects a discovery that could not support a decision.
func (d DiscoveredBinding) Validate() error {
	if err := validateIdentifier("binding name", string(d.Name)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidBindingDiscovery, err)
	}
	if !d.Population.Valid() {
		return fmt.Errorf("%w: binding %q has no population verdict", ErrInvalidBindingDiscovery, d.Name)
	}
	if err := d.Inspection.Validate(); err != nil {
		return fmt.Errorf("%w: binding %q: %w", ErrInvalidBindingDiscovery, d.Name, err)
	}
	return nil
}

// TargetRole names why a fence target was pinned at INTENT_FSYNCED. One binding
// may appear under several roles; exact-same-component in-place adoption pins
// the same target as both source and destination.
type TargetRole uint8

const (
	// TargetRoleUnspecified is the zero value and never a pinned role.
	TargetRoleUnspecified TargetRole = iota
	// TargetRoleSource pins a prior-authority target.
	TargetRoleSource
	// TargetRoleDestination pins a desired-authority target.
	TargetRoleDestination
	// TargetRoleRetained pins a target that stays retained and write-denied.
	TargetRoleRetained
)

// Valid reports whether a role belongs to the closed set.
func (r TargetRole) Valid() bool {
	return r >= TargetRoleSource && r <= TargetRoleRetained
}

// String returns the stable name of a target role.
func (r TargetRole) String() string {
	switch r {
	case TargetRoleUnspecified:
		return "unspecified"
	case TargetRoleSource:
		return "source"
	case TargetRoleDestination:
		return "destination"
	case TargetRoleRetained:
		return "retained"
	default:
		return "invalid"
	}
}

// NamedFenceTarget is one pinned target recorded at INTENT_FSYNCED. Recovery
// reacquires from these locators rather than from a mutable binding name.
type NamedFenceTarget struct {
	Name   BindingName
	Role   TargetRole
	Target FenceTarget
}

// Clone returns a detached named target.
func (t NamedFenceTarget) Clone() NamedFenceTarget {
	out := t
	out.Target = t.Target.Clone()
	return out
}

// Validate rejects a target that could not be reacquired from its own record.
func (t NamedFenceTarget) Validate() error {
	if err := validateIdentifier("binding name", string(t.Name)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidFenceTarget, err)
	}
	if !t.Role.Valid() {
		return fmt.Errorf("%w: binding %q has no pinned target role", ErrInvalidFenceTarget, t.Name)
	}
	return t.Target.Validate()
}

// ClassMoveKind is the closed set of per-class transitions a saga may carry.
type ClassMoveKind uint8

const (
	// ClassMoveUnspecified is the zero value and never a derived move.
	ClassMoveUnspecified ClassMoveKind = iota
	// ClassMoveGenesis adopts an empty destination with no prior authority.
	ClassMoveGenesis
	// ClassMoveUnchanged keeps the exact active binding and identity.
	ClassMoveUnchanged
	// ClassMoveReconfigure keeps the exact physical identity while the secret-free
	// configuration that selected it changed. The specification authorizes migration from
	// configuration alone, so this is a saga participant, not a no-op.
	ClassMoveReconfigure
	// ClassMoveInPlaceAdoption adopts populated components already in place, with
	// no copy and an explicit empty guard set.
	ClassMoveInPlaceAdoption
	// ClassMoveRelocate copies a populated class to a physically distinct
	// destination and retains the source.
	ClassMoveRelocate
	// ClassMoveReverse targets a binding retained by an earlier forward saga.
	ClassMoveReverse
)

// Valid reports whether a move kind was actually derived.
func (k ClassMoveKind) Valid() bool {
	return k >= ClassMoveGenesis && k <= ClassMoveReverse
}

// RequiresSaga reports whether a move must run through the durable saga.
func (k ClassMoveKind) RequiresSaga() bool { return k.Valid() && k != ClassMoveUnchanged }

// String returns the stable name of a move kind.
func (k ClassMoveKind) String() string {
	switch k {
	case ClassMoveUnspecified:
		return "unspecified"
	case ClassMoveGenesis:
		return "genesis"
	case ClassMoveUnchanged:
		return "unchanged"
	case ClassMoveReconfigure:
		return "reconfigure"
	case ClassMoveInPlaceAdoption:
		return "in-place-adoption"
	case ClassMoveRelocate:
		return "relocate"
	case ClassMoveReverse:
		return "reverse"
	default:
		return "invalid"
	}
}

// ClassMove is the derived transition for one semantic class.
type ClassMove struct {
	Class           coordclass.Class
	Kind            ClassMoveKind
	PriorBinding    BindingName
	DesiredBinding  BindingName
	SourcePopulated bool
}

// Validate rejects a move that does not name a real transition.
func (m ClassMove) Validate() error {
	if !isKnownClass(m.Class) {
		return fmt.Errorf("%w: %s", ErrUnsupportedClass, m.Class)
	}
	if !m.Kind.Valid() {
		return fmt.Errorf("%w: class %s has no derived move", ErrInvalidMigrationIntent, m.Class)
	}
	if err := validateIdentifier("desired binding name", string(m.DesiredBinding)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidMigrationIntent, err)
	}
	switch m.Kind {
	case ClassMoveGenesis:
		if m.PriorBinding != "" || m.SourcePopulated {
			return fmt.Errorf("%w: class %s genesis cannot name a populated prior binding", ErrInvalidMigrationIntent, m.Class)
		}
	case ClassMoveInPlaceAdoption:
		if !m.SourcePopulated {
			return fmt.Errorf("%w: class %s in-place adoption requires populated components", ErrInvalidMigrationIntent, m.Class)
		}
	default:
		if err := validateIdentifier("prior binding name", string(m.PriorBinding)); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidMigrationIntent, err)
		}
	}
	if m.Kind == ClassMoveUnchanged && m.PriorBinding != m.DesiredBinding {
		return fmt.Errorf("%w: class %s cannot be unchanged across binding names", ErrInvalidMigrationIntent, m.Class)
	}
	return nil
}

// GenesisBindingEvidence is one mutation-free proof that a desired binding held
// no authoritative content before the first generation existed.
type GenesisBindingEvidence struct {
	Name       BindingName
	Target     FenceTarget
	Descriptor *Descriptor
}

// Clone returns a detached evidence value.
func (e GenesisBindingEvidence) Clone() GenesisBindingEvidence {
	out := e
	out.Target = e.Target.Clone()
	if e.Descriptor != nil {
		cloned := e.Descriptor.Clone()
		out.Descriptor = &cloned
	}
	return out
}

// GenesisEvidence is the mutation-free evidence that stands in for a prior
// generation before any durable generation exists. Generation zero is the
// invalid sentinel and is never genesis authority, so the evidence itself is
// what an INTENT_FSYNCED record with no prior generation must carry.
type GenesisEvidence struct {
	Bindings []GenesisBindingEvidence
}

// Clone returns detached genesis evidence.
func (e GenesisEvidence) Clone() GenesisEvidence {
	out := GenesisEvidence{}
	if e.Bindings != nil {
		out.Bindings = make([]GenesisBindingEvidence, 0, len(e.Bindings))
		for _, binding := range e.Bindings {
			out.Bindings = append(out.Bindings, binding.Clone())
		}
	}
	return out
}

// Validate rejects evidence that does not actually prove the absence of authority.
func (e GenesisEvidence) Validate() error {
	if len(e.Bindings) == 0 {
		return fmt.Errorf("%w: genesis evidence names no binding", ErrInvalidMigrationIntent)
	}
	seen := make(map[BindingName]struct{}, len(e.Bindings))
	for _, binding := range e.Bindings {
		if err := validateIdentifier("binding name", string(binding.Name)); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidMigrationIntent, err)
		}
		if _, exists := seen[binding.Name]; exists {
			return fmt.Errorf("%w: genesis evidence names binding %q twice", ErrInvalidMigrationIntent, binding.Name)
		}
		seen[binding.Name] = struct{}{}
		if err := binding.Target.Validate(); err != nil {
			return err
		}
		if binding.Descriptor != nil {
			if err := binding.Descriptor.Validate(); err != nil {
				return err
			}
		}
	}
	return nil
}

// BindingParticipant is one binding whose activation the saga must commit.
type BindingParticipant struct {
	Name     BindingName
	Provider ProviderID
	Classes  ClassSet
}

// Key returns the participant key used by every receipt for this binding.
func (p BindingParticipant) Key() string { return bindingParticipantPrefix + string(p.Name) }

// bindingParticipantPrefix keeps binding keys disjoint from Work workspace keys,
// which are NUL-joined provider/component/identity triples. A binding name is a
// validated identifier and can contain neither a colon nor a NUL, so no binding
// key can collide with a Work key.
const bindingParticipantPrefix = "binding:"

// Validate rejects a participant that cannot be committed or receipted.
func (p BindingParticipant) Validate() error {
	if err := validateIdentifier("binding name", string(p.Name)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidMigrationIntent, err)
	}
	if err := validateIdentifier("provider ID", string(p.Provider)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidMigrationIntent, err)
	}
	if p.Classes.Empty() {
		return fmt.Errorf("%w: binding participant %q serves no class", ErrInvalidMigrationIntent, p.Name)
	}
	return nil
}

// ParticipantSet is the complete closed participant set an attempt pins before
// any fence or destination mutation. A participant not named here is not in the
// saga, so the set is what the receipt bitmap and COMMIT_DECIDED close over.
type ParticipantSet struct {
	Bindings []BindingParticipant
	Work     []WorkWorkspaceParticipant
}

// Clone returns a detached participant set.
func (s ParticipantSet) Clone() ParticipantSet {
	out := ParticipantSet{}
	if s.Bindings != nil {
		out.Bindings = append([]BindingParticipant(nil), s.Bindings...)
	}
	if s.Work != nil {
		out.Work = make([]WorkWorkspaceParticipant, 0, len(s.Work))
		for _, participant := range s.Work {
			out.Work = append(out.Work, participant.Clone())
		}
	}
	return out
}

// Keys returns every participant key in canonical order: bindings by name, then
// Work workspaces by physical key.
func (s ParticipantSet) Keys() []string {
	keys := make([]string, 0, len(s.Bindings)+len(s.Work))
	for _, binding := range s.Bindings {
		keys = append(keys, binding.Key())
	}
	work := make([]string, 0, len(s.Work))
	for _, participant := range s.Work {
		work = append(work, participant.Key())
	}
	sort.Strings(keys)
	sort.Strings(work)
	return append(keys, work...)
}

// Empty reports whether the set names no participant.
func (s ParticipantSet) Empty() bool { return len(s.Bindings) == 0 && len(s.Work) == 0 }

// Validate rejects an empty, duplicated, or malformed participant set.
func (s ParticipantSet) Validate() error {
	if s.Empty() {
		return fmt.Errorf("%w: participant set is empty", ErrInvalidMigrationIntent)
	}
	seen := make(map[string]struct{}, len(s.Bindings)+len(s.Work))
	for _, binding := range s.Bindings {
		if err := binding.Validate(); err != nil {
			return err
		}
		if _, exists := seen[binding.Key()]; exists {
			return fmt.Errorf("%w: duplicate participant %q", ErrInvalidMigrationIntent, binding.Key())
		}
		seen[binding.Key()] = struct{}{}
	}
	for _, participant := range s.Work {
		if err := participant.Validate(); err != nil {
			return err
		}
		if _, exists := seen[participant.Key()]; exists {
			return fmt.Errorf("%w: duplicate Work participant", ErrInvalidMigrationIntent)
		}
		seen[participant.Key()] = struct{}{}
	}
	return nil
}

// StartupInputs are the facts intent derivation reads. Every one of them is
// produced elsewhere: plan resolution freezes the plan, the provider census produces the
// discoveries, and the manifest store loads the durable records. Derivation
// opens nothing and inspects nothing.
type StartupInputs struct {
	Plan             *StoragePlan
	Discovered       []DiscoveredBinding
	Active           *ActiveManifest
	Attempt          *AttemptRecord
	WitnessAlgorithm string
}

// MigrationIntent is the derived startup decision plus everything an
// INTENT_FSYNCED record must pin. Its fields are unexported so a caller cannot
// assemble one that was never derived.
type MigrationIntent struct {
	outcome            IntentOutcome
	blocked            *MigrationBlockedError
	attempt            AttemptID
	priorGeneration    Generation
	desiredGeneration  Generation
	genesis            *GenesisEvidence
	priorAssignments   map[coordclass.Class]BindingName
	desiredAssignments map[coordclass.Class]BindingName
	moves              []ClassMove
	targets            []NamedFenceTarget
	descriptors        []NamedDescriptor
	retained           []RetainedSourceRef
	verify             []NamedFenceTarget
	participants       ParticipantSet
	configDigest       CompositeConfigDigest
	bindingDigests     map[BindingName]ConfigRefDigest
	witnessAlgorithm   string
	resumedFrom        MigrationPhase
}

// Outcome returns the derived decision. The zero value reports
// IntentOutcomeUnderived rather than a plausible no-op.
func (i MigrationIntent) Outcome() IntentOutcome { return i.outcome }

// Blocked returns the typed block report, or nil when startup may proceed.
func (i MigrationIntent) Blocked() *MigrationBlockedError { return i.blocked }

// Attempt returns the deterministic attempt identity. Two startups that derive
// the same desired identities derive the same attempt, which is what lets a
// restart resume an attempt instead of minting a second one.
func (i MigrationIntent) Attempt() AttemptID { return i.attempt }

// PriorGeneration returns the durable generation that is authoritative until
// COMMIT_DECIDED, or zero when only genesis evidence exists.
func (i MigrationIntent) PriorGeneration() Generation { return i.priorGeneration }

// DesiredGeneration returns the generation that becomes authoritative from the
// fsynced COMMIT_DECIDED record onward.
func (i MigrationIntent) DesiredGeneration() Generation { return i.desiredGeneration }

// Genesis returns the mutation-free genesis evidence, or nil when a prior
// generation exists.
func (i MigrationIntent) Genesis() *GenesisEvidence {
	if i.genesis == nil {
		return nil
	}
	cloned := i.genesis.Clone()
	return &cloned
}

// PriorAssignments returns the class map of the prior generation.
func (i MigrationIntent) PriorAssignments() map[coordclass.Class]BindingName {
	return cloneAssignments(i.priorAssignments)
}

// DesiredAssignments returns the class map configuration selected.
func (i MigrationIntent) DesiredAssignments() map[coordclass.Class]BindingName {
	return cloneAssignments(i.desiredAssignments)
}

// Moves returns the per-class transitions in canonical class order.
func (i MigrationIntent) Moves() []ClassMove { return append([]ClassMove(nil), i.moves...) }

// FenceTargets returns every source, destination, and retained target pinned by
// the intent, in canonical order.
func (i MigrationIntent) FenceTargets() []NamedFenceTarget { return cloneNamedTargets(i.targets) }

// Descriptors returns every complete mutation-free descriptor the intent pinned.
func (i MigrationIntent) Descriptors() []NamedDescriptor { return cloneNamedDescriptors(i.descriptors) }

// RetainedSources returns the retained-source locators the intent pinned.
func (i MigrationIntent) RetainedSources() []RetainedSourceRef {
	return cloneRetainedSources(i.retained)
}

// VerificationTargets returns the active targets whose mutation-free inspection
// was incomplete. It is non-empty exactly when the outcome is
// IntentOutcomeVerifyActive.
func (i MigrationIntent) VerificationTargets() []NamedFenceTarget { return cloneNamedTargets(i.verify) }

// Participants returns the complete closed participant set.
func (i MigrationIntent) Participants() ParticipantSet { return i.participants.Clone() }

// ConfigDigest returns the composite secret-free config digest recovery compares
// against before it trusts any recorded phase field.
func (i MigrationIntent) ConfigDigest() CompositeConfigDigest { return i.configDigest }

// BindingConfigDigests returns the per-binding secret-free config digests.
func (i MigrationIntent) BindingConfigDigests() map[BindingName]ConfigRefDigest {
	return cloneBindingDigests(i.bindingDigests)
}

// WitnessAlgorithm returns the single witness algorithm version the whole
// attempt uses.
func (i MigrationIntent) WitnessAlgorithm() string { return i.witnessAlgorithm }

// ResumedFrom returns the recorded phase a resumed intent continues from, or
// PhaseInvalid for a freshly derived intent.
func (i MigrationIntent) ResumedFrom() MigrationPhase { return i.resumedFrom }

// Resumed reports whether the intent continues a recorded nonterminal attempt
// rather than deriving a new one from current configuration.
func (i MigrationIntent) Resumed() bool { return i.resumedFrom.Valid() }

// DeriveStartupIntent compares the desired state configuration selected against
// the discovered physical identity and the durable records, and returns one
// typed decision. It never inspects, opens, fences, recovers, or publishes.
//
// A blocked derivation returns both an IntentOutcomeBlocked intent and a
// non-nil *MigrationBlockedError, so a caller that reads only one of the two
// still stops.
func DeriveStartupIntent(inputs StartupInputs) (MigrationIntent, error) {
	if inputs.Plan == nil {
		return MigrationIntent{}, fmt.Errorf("%w: no frozen storage plan", ErrInvalidMigrationIntent)
	}
	if strings.TrimSpace(inputs.WitnessAlgorithm) == "" {
		return MigrationIntent{}, fmt.Errorf("%w: no witness algorithm version", ErrInvalidMigrationIntent)
	}
	if err := validateSecretFree("witness algorithm", inputs.WitnessAlgorithm); err != nil {
		return MigrationIntent{}, err
	}
	discovered, err := indexDiscoveries(inputs.Discovered)
	if err != nil {
		return MigrationIntent{}, err
	}
	if inputs.Active != nil {
		if err := inputs.Active.Validate(); err != nil {
			return MigrationIntent{}, err
		}
	}
	if inputs.Attempt != nil {
		if err := inputs.Attempt.Validate(); err != nil {
			return MigrationIntent{}, err
		}
	}
	if inputs.Attempt != nil && !inputs.Attempt.Phase.Terminal() {
		return resumeRecordedAttempt(inputs.Attempt, inputs.Active, inputs.Plan)
	}
	if inputs.Attempt != nil {
		if blocked := terminalAttemptDisagreesWithActive(inputs.Attempt, inputs.Active); blocked != nil {
			return blockedIntent(blocked)
		}
	}
	return deriveFreshIntent(inputs, discovered)
}

// terminalAttemptDisagreesWithActive rejects the state where a saga recorded
// ACTIVE but the active manifest does not agree. Reading past it would treat a
// completed generation's bindings as undiscovered and derive genesis over live
// data, which is the exact shape of a silent overwrite.
func terminalAttemptDisagreesWithActive(record *AttemptRecord, active *ActiveManifest) *MigrationBlockedError {
	recorded := record.Intent.DesiredGeneration
	if active == nil {
		return &MigrationBlockedError{
			Reason:             BlockReasonConflicting,
			Subject:            string(record.Intent.Attempt),
			Detail:             "attempt records ACTIVE but no active manifest exists",
			RecordedGeneration: recorded,
		}
	}
	if active.Generation != recorded || active.Attempt != record.Intent.Attempt {
		return &MigrationBlockedError{
			Reason:             BlockReasonConflicting,
			Subject:            string(record.Intent.Attempt),
			Detail:             fmt.Sprintf("active manifest records attempt %q at generation %d", active.Attempt, active.Generation),
			RecordedGeneration: recorded,
		}
	}
	return nil
}

func indexDiscoveries(discoveries []DiscoveredBinding) (map[BindingName]DiscoveredBinding, error) {
	indexed := make(map[BindingName]DiscoveredBinding, len(discoveries))
	named := make([]NamedDescriptor, 0, len(discoveries))
	for _, discovery := range discoveries {
		if err := discovery.Validate(); err != nil {
			return nil, err
		}
		if _, exists := indexed[discovery.Name]; exists {
			return nil, fmt.Errorf("%w: binding %q discovered twice", ErrInvalidBindingDiscovery, discovery.Name)
		}
		indexed[discovery.Name] = discovery.Clone()
		if discovery.Inspection.Descriptor != nil {
			named = append(named, NamedDescriptor{Name: discovery.Name, Descriptor: *discovery.Inspection.Descriptor})
		}
	}
	if err := ValidateNoDescriptorOverlap(named); err != nil {
		return nil, err
	}
	return indexed, nil
}

// resumeRecordedAttempt continues a recorded nonterminal attempt. Every fact
// comes from the record, never from current configuration: current config is
// consulted only to prove the recorded digests still describe it.
func resumeRecordedAttempt(record *AttemptRecord, active *ActiveManifest, plan *StoragePlan) (MigrationIntent, error) {
	recorded := record.Intent.DesiredGeneration
	if blocked := recordedAttemptDisagreesWithActive(record, active); blocked != nil {
		return blockedIntent(blocked)
	}
	if record.Intent.ConfigDigest != plan.ConfigDigest() {
		return blockedIntent(&MigrationBlockedError{
			Reason:             BlockReasonStale,
			Subject:            "composite config digest",
			Detail:             "recorded attempt does not describe the current configuration",
			RecordedGeneration: recorded,
		})
	}
	current := plan.BindingConfigDigests()
	for _, name := range sortedBindingNames(record.Intent.BindingConfigDigests) {
		digest, exists := current[name]
		if !exists || digest != record.Intent.BindingConfigDigests[name] {
			return blockedIntent(&MigrationBlockedError{
				Reason:             BlockReasonStale,
				Subject:            string(name),
				Detail:             "recorded binding config digest does not describe the current configuration",
				RecordedGeneration: recorded,
			})
		}
	}
	section := record.Intent.Clone()
	return MigrationIntent{
		outcome:            IntentOutcomeMigrate,
		attempt:            section.Attempt,
		priorGeneration:    section.PriorGeneration,
		desiredGeneration:  section.DesiredGeneration,
		genesis:            section.Genesis,
		priorAssignments:   section.PriorAssignments,
		desiredAssignments: section.DesiredAssignments,
		moves:              section.Moves,
		targets:            section.FenceTargets,
		descriptors:        section.Descriptors,
		retained:           section.RetainedSources,
		participants:       section.Participants,
		configDigest:       section.ConfigDigest,
		bindingDigests:     section.BindingConfigDigests,
		witnessAlgorithm:   section.WitnessAlgorithm,
		resumedFrom:        record.Phase,
	}, nil
}

// recordedAttemptDisagreesWithActive cross-checks a nonterminal attempt against
// the durable active manifest. Before the decision the prior generation is
// authoritative, so the active manifest must be exactly the generation the
// attempt recorded as prior; from ACTIVE_MANIFEST_DURABLE onward the attempt
// pinned the manifest's digest, so a manifest that no longer hashes to it is a
// different manifest and blocks rather than being adopted.
func recordedAttemptDisagreesWithActive(record *AttemptRecord, active *ActiveManifest) *MigrationBlockedError {
	recorded := record.Intent.DesiredGeneration
	if record.ActiveManifestDigest != "" {
		if active == nil {
			return &MigrationBlockedError{
				Reason:             BlockReasonConflicting,
				Subject:            string(record.Intent.Attempt),
				Detail:             "attempt pins an active manifest that does not exist",
				RecordedGeneration: recorded,
			}
		}
		digest, err := active.Digest()
		if err != nil {
			return blockedBy(BlockReasonDamaged, string(record.Intent.Attempt), err)
		}
		if digest != record.ActiveManifestDigest {
			return &MigrationBlockedError{
				Reason:             BlockReasonConflicting,
				Subject:            string(record.Intent.Attempt),
				Detail:             "the durable active manifest is not the one the attempt pinned",
				RecordedGeneration: recorded,
			}
		}
		return nil
	}
	if active == nil {
		if record.Intent.PriorGeneration.Valid() {
			return &MigrationBlockedError{
				Reason:             BlockReasonConflicting,
				Subject:            string(record.Intent.Attempt),
				Detail:             fmt.Sprintf("attempt names prior generation %d but no active manifest exists", record.Intent.PriorGeneration),
				RecordedGeneration: recorded,
			}
		}
		return nil
	}
	if active.Generation != record.Intent.PriorGeneration {
		return &MigrationBlockedError{
			Reason:             BlockReasonConflicting,
			Subject:            string(record.Intent.Attempt),
			Detail:             fmt.Sprintf("attempt names prior generation %d, the active manifest is generation %d", record.Intent.PriorGeneration, active.Generation),
			RecordedGeneration: recorded,
		}
	}
	return nil
}

func deriveFreshIntent(inputs StartupInputs, discovered map[BindingName]DiscoveredBinding) (MigrationIntent, error) {
	plan := inputs.Plan
	desired := plan.Assignments()
	for _, class := range coordclass.Classes() {
		if _, assigned := desired[class]; !assigned {
			return blockedIntent(blockedIncomplete(class.String(), "frozen plan assigns no binding to the class"))
		}
	}
	for _, name := range sortedAssignmentNames(desired) {
		if _, found := discovered[name]; !found {
			return blockedIntent(blockedIncomplete(string(name), "desired binding was not inspected"))
		}
	}

	intent := MigrationIntent{
		desiredAssignments: cloneAssignments(desired),
		configDigest:       plan.ConfigDigest(),
		bindingDigests:     plan.BindingConfigDigests(),
		witnessAlgorithm:   inputs.WitnessAlgorithm,
	}

	var derived []ClassMove
	var err error
	if inputs.Active == nil {
		derived, intent.genesis, err = deriveGenesisMoves(desired, discovered)
	} else {
		intent.priorAssignments = inputs.Active.AssignmentMap()
		derived, intent.verify, err = deriveActiveMoves(desired, discovered, inputs.Active, plan.ConfigDigest())
	}
	if err != nil {
		return blockedIntentFrom(err)
	}
	intent.moves = derived

	if inputs.Active == nil {
		intent.priorGeneration = 0
		intent.desiredGeneration = 1
	} else {
		intent.priorGeneration = inputs.Active.Generation
		intent.desiredGeneration = inputs.Active.Generation + 1
	}

	saga := false
	for _, move := range derived {
		if move.Kind.RequiresSaga() {
			saga = true
			break
		}
	}
	if !saga {
		intent.desiredGeneration = intent.priorGeneration
		intent.genesis = nil
		intent.attempt = ""
		if len(intent.verify) > 0 {
			intent.outcome = IntentOutcomeVerifyActive
		} else {
			intent.outcome = IntentOutcomeNoOp
		}
		return intent, nil
	}

	// A saga is derived, so incompleteness can no longer be resolved by a short
	// verification fence: every pinned fact must be recorded before mutation.
	intent.verify = nil
	intent.outcome = IntentOutcomeMigrate
	intent.targets, intent.descriptors = pinTargets(desired, discovered, inputs.Active, derived)
	if inputs.Active != nil {
		intent.retained = cloneRetainedSources(inputs.Active.RetainedSources)
	}
	participants, err := deriveParticipants(plan, desired, discovered, derived)
	if err != nil {
		return blockedIntentFrom(err)
	}
	intent.participants = participants
	intent.attempt = deriveAttemptID(intent)
	if err := intent.validateDerived(); err != nil {
		return blockedIntentFrom(err)
	}
	return intent, nil
}

func deriveGenesisMoves(desired map[coordclass.Class]BindingName, discovered map[BindingName]DiscoveredBinding) ([]ClassMove, *GenesisEvidence, error) {
	populated := false
	for _, name := range sortedAssignmentNames(desired) {
		if discovered[name].Population == BindingPopulationPopulated {
			populated = true
			break
		}
	}
	moves := make([]ClassMove, 0, len(coordclass.Classes()))
	for _, class := range coordclass.Classes() {
		name := desired[class]
		kind := ClassMoveGenesis
		sourcePopulated := false
		if discovered[name].Population == BindingPopulationPopulated {
			kind = ClassMoveInPlaceAdoption
			sourcePopulated = true
		}
		moves = append(moves, ClassMove{Class: class, Kind: kind, DesiredBinding: name, SourcePopulated: sourcePopulated})
	}
	if populated {
		// the legacy combined layout adoption: populated components already in place are the prior
		// authority, so there is no genesis evidence to record.
		return moves, nil, nil
	}
	evidence := &GenesisEvidence{}
	for _, name := range sortedAssignmentNames(desired) {
		discovery := discovered[name]
		entry := GenesisBindingEvidence{Name: name, Target: discovery.Inspection.Target.Clone()}
		if discovery.Inspection.Descriptor != nil {
			cloned := discovery.Inspection.Descriptor.Clone()
			entry.Descriptor = &cloned
		}
		evidence.Bindings = append(evidence.Bindings, entry)
	}
	if err := evidence.Validate(); err != nil {
		return nil, nil, err
	}
	return moves, evidence, nil
}

func deriveActiveMoves(desired map[coordclass.Class]BindingName, discovered map[BindingName]DiscoveredBinding, active *ActiveManifest, configDigest CompositeConfigDigest) ([]ClassMove, []NamedFenceTarget, error) {
	prior := active.AssignmentMap()
	reconfigured := active.ConfigDigest != configDigest
	moves := make([]ClassMove, 0, len(coordclass.Classes()))
	var verify []NamedFenceTarget
	for _, class := range coordclass.Classes() {
		desiredName := desired[class]
		priorName, assigned := prior[class]
		if !assigned {
			return nil, nil, blockedIncomplete(class.String(), "active manifest assigns no binding to the class")
		}
		discovery := discovered[desiredName]
		move := ClassMove{Class: class, Kind: ClassMoveRelocate, PriorBinding: priorName, DesiredBinding: desiredName}
		if priorDiscovery, found := discovered[priorName]; found {
			move.SourcePopulated = priorDiscovery.Population == BindingPopulationPopulated
		} else {
			move.SourcePopulated = true
		}
		switch {
		case priorName != desiredName:
			if active.retains(discovery.Inspection) {
				move.Kind = ClassMoveReverse
			}
		default:
			activeDescriptor, hasActive := active.descriptorFor(desiredName)
			if !hasActive {
				return nil, nil, blockedIncomplete(string(desiredName), "active manifest records no descriptor for its own binding")
			}
			if !discovery.Inspection.Complete() {
				// Incompleteness never counts as identity equality. The class is
				// provisionally unchanged and the target is queued for a short
				// fenced verification; if any other class forces a saga, the
				// provisional verdict is discarded with the verification list.
				move.Kind = ClassMoveUnchanged
				verify = appendVerification(verify, NamedFenceTarget{Name: desiredName, Role: TargetRoleDestination, Target: discovery.Inspection.Target.Clone()})
				break
			}
			if !discovery.Inspection.Descriptor.Equal(activeDescriptor) {
				break
			}
			move.Kind = ClassMoveUnchanged
			if reconfigured {
				move.Kind = ClassMoveReconfigure
			}
		}
		if err := move.Validate(); err != nil {
			return nil, nil, err
		}
		moves = append(moves, move)
	}
	return moves, verify, nil
}

func appendVerification(targets []NamedFenceTarget, target NamedFenceTarget) []NamedFenceTarget {
	for _, existing := range targets {
		if existing.Name == target.Name && existing.Role == target.Role {
			return targets
		}
	}
	return append(targets, target)
}

func pinTargets(desired map[coordclass.Class]BindingName, discovered map[BindingName]DiscoveredBinding, active *ActiveManifest, moves []ClassMove) ([]NamedFenceTarget, []NamedDescriptor) {
	roles := make(map[BindingName]map[TargetRole]struct{})
	addRole := func(name BindingName, role TargetRole) {
		if name == "" {
			return
		}
		if _, found := discovered[name]; !found {
			return
		}
		if roles[name] == nil {
			roles[name] = make(map[TargetRole]struct{}, 3)
		}
		roles[name][role] = struct{}{}
	}
	for _, move := range moves {
		addRole(move.DesiredBinding, TargetRoleDestination)
		if move.PriorBinding == "" {
			continue
		}
		addRole(move.PriorBinding, TargetRoleSource)
		if move.PriorBinding != move.DesiredBinding && move.Kind == ClassMoveRelocate {
			addRole(move.PriorBinding, TargetRoleRetained)
		}
	}
	if active != nil {
		for _, source := range active.RetainedSources {
			for name, discovery := range discovered {
				if targetCoversComponent(discovery.Inspection.Target, source.Component, source.PhysicalIdentity) {
					addRole(name, TargetRoleRetained)
				}
			}
		}
	}

	var targets []NamedFenceTarget
	var descriptors []NamedDescriptor
	for _, name := range sortedRoleNames(roles) {
		discovery := discovered[name]
		for _, role := range []TargetRole{TargetRoleSource, TargetRoleDestination, TargetRoleRetained} {
			if _, pinned := roles[name][role]; !pinned {
				continue
			}
			targets = append(targets, NamedFenceTarget{Name: name, Role: role, Target: discovery.Inspection.Target.Clone()})
		}
		if discovery.Inspection.Descriptor != nil {
			descriptors = append(descriptors, NamedDescriptor{Name: name, Descriptor: discovery.Inspection.Descriptor.Clone()})
		}
	}
	// Desired bindings that are only unchanged still need their descriptor
	// recorded, because the active manifest the saga writes names every class.
	for _, name := range sortedAssignmentNames(desired) {
		if _, pinned := roles[name]; pinned {
			continue
		}
		discovery := discovered[name]
		targets = append(targets, NamedFenceTarget{Name: name, Role: TargetRoleDestination, Target: discovery.Inspection.Target.Clone()})
		if discovery.Inspection.Descriptor != nil {
			descriptors = append(descriptors, NamedDescriptor{Name: name, Descriptor: discovery.Inspection.Descriptor.Clone()})
		}
	}
	sort.SliceStable(targets, func(i, j int) bool {
		if targets[i].Name != targets[j].Name {
			return targets[i].Name < targets[j].Name
		}
		return targets[i].Role < targets[j].Role
	})
	sort.SliceStable(descriptors, func(i, j int) bool { return descriptors[i].Name < descriptors[j].Name })
	return targets, descriptors
}

func targetCoversComponent(target FenceTarget, component ComponentID, identity PhysicalIdentity) bool {
	for _, part := range target.Components {
		if part.ID == component && part.PhysicalIdentity == identity {
			return true
		}
	}
	return false
}

func deriveParticipants(plan *StoragePlan, desired map[coordclass.Class]BindingName, discovered map[BindingName]DiscoveredBinding, moves []ClassMove) (ParticipantSet, error) {
	providers := make(map[BindingName]ProviderID, len(plan.Bindings()))
	for _, binding := range plan.Bindings() {
		providers[binding.Name] = binding.ProviderID
	}
	classes := make(map[BindingName]ClassSet, len(desired))
	workMoved := false
	for _, move := range moves {
		if move.Class == coordclass.ClassWork {
			workMoved = move.Kind.RequiresSaga()
			continue
		}
		classes[move.DesiredBinding] = classes[move.DesiredBinding].with(move.Class)
	}
	set := ParticipantSet{}
	for _, name := range sortedClassSetNames(classes) {
		provider, known := providers[name]
		if !known {
			if descriptor := discovered[name].Inspection.Descriptor; descriptor != nil {
				provider = descriptor.Provider
			}
		}
		if provider == "" {
			return ParticipantSet{}, blockedIncomplete(string(name), "no frozen provider resolved for the participant binding")
		}
		set.Bindings = append(set.Bindings, BindingParticipant{Name: name, Provider: provider, Classes: classes[name]})
	}
	if workMoved {
		participants, err := plan.WorkParticipants()
		if err != nil {
			return ParticipantSet{}, blockedBy(BlockReasonIncomplete, string(ReservedWorkBinding), err)
		}
		set.Work = participants
	}
	if err := set.Validate(); err != nil {
		return ParticipantSet{}, err
	}
	return set, nil
}

// deriveAttemptID derives a stable attempt identity from the desired identities
// alone. Restarting with the same desired state derives the same attempt and
// therefore resumes it; changing any pinned identity derives a different attempt
// and cannot silently adopt a record prepared for other identities.
func deriveAttemptID(intent MigrationIntent) AttemptID {
	var encoded canonicalDescriptorEncoding
	encoded.string("gascity.storage-migration-attempt.v1")
	encoded.uint64(uint64(intent.priorGeneration))
	encoded.uint64(uint64(intent.desiredGeneration))
	encoded.string(string(intent.configDigest))
	encoded.string(intent.witnessAlgorithm)
	for _, class := range coordclass.Classes() {
		encoded.string(class.String())
		encoded.string(string(intent.priorAssignments[class]))
		encoded.string(string(intent.desiredAssignments[class]))
	}
	encoded.uint64(uint64(len(intent.moves)))
	for _, move := range intent.moves {
		encoded.string(move.Class.String())
		encoded.uint16(uint16(move.Kind))
		encoded.bool(move.SourcePopulated)
	}
	keys := intent.participants.Keys()
	encoded.uint64(uint64(len(keys)))
	for _, key := range keys {
		encoded.string(key)
	}
	encoded.uint64(uint64(len(intent.targets)))
	for _, target := range intent.targets {
		encoded.string(string(target.Name))
		encoded.uint16(uint16(target.Role))
		encoded.string(string(target.Target.Provider))
		encoded.classSet(target.Target.Classes)
		encoded.uint64(uint64(len(target.Target.Components)))
		for _, component := range target.Target.Components {
			encoded.string(string(component.ID))
			encoded.string(string(component.Locator))
			encoded.string(string(component.PhysicalIdentity))
		}
	}
	for _, name := range sortedAssignmentNames(intent.desiredAssignments) {
		encoded.string(string(name))
		encoded.string(string(intent.bindingDigests[name]))
	}
	return AttemptID("attempt-" + strings.TrimPrefix(canonicalDigest(encoded.bytes), "sha256:"))
}

func (i MigrationIntent) validateDerived() error {
	if i.outcome != IntentOutcomeMigrate {
		return nil
	}
	if strings.TrimSpace(string(i.attempt)) == "" {
		return fmt.Errorf("%w: migrate intent has no attempt identity", ErrInvalidMigrationIntent)
	}
	if !i.desiredGeneration.Valid() {
		return fmt.Errorf("%w: migrate intent has no desired generation", ErrInvalidMigrationIntent)
	}
	if !i.priorGeneration.Valid() && i.genesis == nil && !i.hasAdoptionMove() {
		return fmt.Errorf("%w: migrate intent has neither a prior generation nor genesis evidence", ErrInvalidMigrationIntent)
	}
	if i.priorGeneration.Valid() && i.priorGeneration >= i.desiredGeneration {
		return fmt.Errorf("%w: desired generation does not advance the prior generation", ErrInvalidMigrationIntent)
	}
	if len(i.moves) != len(coordclass.Classes()) {
		return fmt.Errorf("%w: migrate intent does not derive every class", ErrInvalidMigrationIntent)
	}
	if len(i.targets) == 0 {
		return fmt.Errorf("%w: migrate intent pins no fence target", ErrInvalidMigrationIntent)
	}
	for _, target := range i.targets {
		if err := target.Validate(); err != nil {
			return err
		}
	}
	for _, descriptor := range i.descriptors {
		if err := descriptor.Descriptor.Validate(); err != nil {
			return err
		}
	}
	for _, source := range i.retained {
		if err := source.Validate(); err != nil {
			return err
		}
	}
	if err := i.participants.Validate(); err != nil {
		return err
	}
	if i.genesis != nil {
		if err := i.genesis.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (i MigrationIntent) hasAdoptionMove() bool {
	for _, move := range i.moves {
		if move.Kind == ClassMoveInPlaceAdoption {
			return true
		}
	}
	return false
}

func blockedIntent(report *MigrationBlockedError) (MigrationIntent, error) {
	return MigrationIntent{outcome: IntentOutcomeBlocked, blocked: report}, report
}

func blockedIntentFrom(err error) (MigrationIntent, error) {
	var report *MigrationBlockedError
	if errors.As(err, &report) {
		return MigrationIntent{outcome: IntentOutcomeBlocked, blocked: report}, report
	}
	return blockedIntent(blockedBy(BlockReasonDamaged, "", err))
}

func cloneAssignments(assignments map[coordclass.Class]BindingName) map[coordclass.Class]BindingName {
	if assignments == nil {
		return nil
	}
	out := make(map[coordclass.Class]BindingName, len(assignments))
	for class, name := range assignments {
		out[class] = name
	}
	return out
}

func cloneBindingDigests(digests map[BindingName]ConfigRefDigest) map[BindingName]ConfigRefDigest {
	if digests == nil {
		return nil
	}
	out := make(map[BindingName]ConfigRefDigest, len(digests))
	for name, digest := range digests {
		out[name] = digest
	}
	return out
}

func cloneNamedTargets(targets []NamedFenceTarget) []NamedFenceTarget {
	if targets == nil {
		return nil
	}
	out := make([]NamedFenceTarget, 0, len(targets))
	for _, target := range targets {
		out = append(out, target.Clone())
	}
	return out
}

func cloneNamedDescriptors(descriptors []NamedDescriptor) []NamedDescriptor {
	if descriptors == nil {
		return nil
	}
	out := make([]NamedDescriptor, 0, len(descriptors))
	for _, descriptor := range descriptors {
		out = append(out, NamedDescriptor{Name: descriptor.Name, Descriptor: descriptor.Descriptor.Clone()})
	}
	return out
}

func cloneRetainedSources(sources []RetainedSourceRef) []RetainedSourceRef {
	if sources == nil {
		return nil
	}
	out := make([]RetainedSourceRef, 0, len(sources))
	for _, source := range sources {
		out = append(out, source.Clone())
	}
	return out
}

func sortedBindingNames[V any](values map[BindingName]V) []BindingName {
	names := make([]BindingName, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	return names
}

func sortedRoleNames(roles map[BindingName]map[TargetRole]struct{}) []BindingName {
	return sortedBindingNames(roles)
}

func sortedClassSetNames(classes map[BindingName]ClassSet) []BindingName {
	return sortedBindingNames(classes)
}

func sortedAssignmentNames(assignments map[coordclass.Class]BindingName) []BindingName {
	seen := make(map[BindingName]struct{}, len(assignments))
	for _, name := range assignments {
		seen[name] = struct{}{}
	}
	return sortedBindingNames(seen)
}
