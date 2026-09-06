package storebinding

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/coordclass"
)

// WitnessEnvelopeDomainV1 is the domain separator for the pinned physical half
// of a witness (the witness contract). It extends the descriptor's canonical identity
// payload rather than replacing it, so the envelope and the descriptor cannot
// drift apart.
const WitnessEnvelopeDomainV1 = "gascity.storage-witness-envelope.v1"

var (
	// ErrInvalidManifest reports a durable record that cannot support a decision.
	ErrInvalidManifest = errors.New("invalid storage migration manifest")
	// ErrInvalidPhaseTransition reports a phase advance the saga does not allow.
	ErrInvalidPhaseTransition = errors.New("invalid storage migration phase transition")
	// ErrInvalidWitness reports a malformed or internally inconsistent witness.
	ErrInvalidWitness = errors.New("invalid storage migration witness")
)

// MigrationPhase is the phase-typed saga state. The zero value is not a
// recorded phase: an attempt record exists only once INTENT_FSYNCED is durable,
// so a record that reads back as PhaseNone is a damaged record, never a
// not-yet-started one.
type MigrationPhase uint8

const (
	// PhaseNone is the zero value. It means no attempt record exists.
	PhaseNone MigrationPhase = iota
	// PhaseIntentFsynced pins every identity before any fence or mutation.
	PhaseIntentFsynced
	// PhasePreparing holds fenced source descriptors, the final census, and any
	// destination mutation the attempt has begun.
	PhasePreparing
	// PhasePrepared holds destination descriptors and matched witnesses.
	PhasePrepared
	// PhaseGuarding holds the complete guard plan and each install receipt.
	PhaseGuarding
	// PhaseGuardsInstalled proves the recorded plan is complete and verified.
	PhaseGuardsInstalled
	// PhaseCommitDecided is the single global authority transition.
	PhaseCommitDecided
	// PhaseReceiptsPersisted means every participant receipt is durable.
	PhaseReceiptsPersisted
	// PhaseActiveManifestDurable means the active manifest replacement is durable.
	PhaseActiveManifestDurable
	// PhaseActiveOpenPending records that the exact active descriptors must be opened.
	PhaseActiveOpenPending
	// PhaseActive is written only after one StoreSet has been published.
	PhaseActive
)

// Valid reports whether the phase is one a durable record may carry.
func (p MigrationPhase) Valid() bool {
	return p >= PhaseIntentFsynced && p <= PhaseActive
}

// Terminal reports whether the saga is finished. Only PhaseActive is terminal;
// every other phase means recovery must run before consumers start.
func (p MigrationPhase) Terminal() bool { return p == PhaseActive }

// SourceAuthoritative reports whether the prior generation (or mutation-free
// genesis evidence) is still the authority. The cut is the fsynced
// COMMIT_DECIDED record and nothing else.
func (p MigrationPhase) SourceAuthoritative() bool {
	return p.Valid() && p < PhaseCommitDecided
}

// String returns the stable phase name.
func (p MigrationPhase) String() string {
	switch p {
	case PhaseNone:
		return "NONE"
	case PhaseIntentFsynced:
		return "INTENT_FSYNCED"
	case PhasePreparing:
		return "PREPARING"
	case PhasePrepared:
		return "PREPARED"
	case PhaseGuarding:
		return "GUARDING"
	case PhaseGuardsInstalled:
		return "GUARDS_INSTALLED"
	case PhaseCommitDecided:
		return "COMMIT_DECIDED"
	case PhaseReceiptsPersisted:
		return "RECEIPTS_PERSISTED"
	case PhaseActiveManifestDurable:
		return "ACTIVE_MANIFEST_DURABLE"
	case PhaseActiveOpenPending:
		return "ACTIVE_OPEN_PENDING"
	case PhaseActive:
		return "ACTIVE"
	default:
		return "invalid"
	}
}

// PhaseEntryReason names why the saga entered a phase. It is the field that
// separates "never started" from "started and rolled back": a record sitting at
// PREPARING with one initial entry has touched nothing, while the same record
// with a later entry whose reason is a return has already mutated destinations
// or installed guards under an earlier entry.
type PhaseEntryReason uint8

const (
	// PhaseEntryUnspecified is the zero value and never a recorded reason.
	PhaseEntryUnspecified PhaseEntryReason = iota
	// PhaseEntryInitial is the first entry into a phase on the forward path.
	PhaseEntryInitial
	// PhaseEntryRollForward is a post-decision re-entry during recovery.
	PhaseEntryRollForward
	// PhaseEntryReturnSourceChanged records the
	// "PREPARED -- source changed before decision --> PREPARING" edge.
	PhaseEntryReturnSourceChanged
	// PhaseEntryReturnGuardsReleased records a durable return to PREPARING after
	// discovered guards were released under the unchanged-source fence.
	PhaseEntryReturnGuardsReleased
	// PhaseEntryReturnPreparationAbandoned records a durable return to PREPARING
	// after preparation was safely abandoned before any decision.
	PhaseEntryReturnPreparationAbandoned
)

// Valid reports whether the reason belongs to the closed set.
func (r PhaseEntryReason) Valid() bool {
	return r >= PhaseEntryInitial && r <= PhaseEntryReturnPreparationAbandoned
}

// Return reports whether the reason records a durable rollback rather than
// forward progress.
func (r PhaseEntryReason) Return() bool {
	return r >= PhaseEntryReturnSourceChanged && r <= PhaseEntryReturnPreparationAbandoned
}

// String returns the stable name of an entry reason.
func (r PhaseEntryReason) String() string {
	switch r {
	case PhaseEntryUnspecified:
		return "unspecified"
	case PhaseEntryInitial:
		return "initial"
	case PhaseEntryRollForward:
		return "roll-forward"
	case PhaseEntryReturnSourceChanged:
		return "return-source-changed"
	case PhaseEntryReturnGuardsReleased:
		return "return-guards-released"
	case PhaseEntryReturnPreparationAbandoned:
		return "return-preparation-abandoned"
	default:
		return "invalid"
	}
}

// PhaseEntry is one append-only journal record. The journal is what makes phase
// re-entry observable; the phase field alone cannot express it.
type PhaseEntry struct {
	Sequence uint64
	Phase    MigrationPhase
	Reason   PhaseEntryReason
}

// Validate rejects a journal entry that names no transition.
func (e PhaseEntry) Validate() error {
	if e.Sequence == 0 {
		return fmt.Errorf("%w: journal entry has no sequence", ErrInvalidManifest)
	}
	if !e.Phase.Valid() {
		return fmt.Errorf("%w: journal entry %d has no phase", ErrInvalidManifest, e.Sequence)
	}
	if !e.Reason.Valid() {
		return fmt.Errorf("%w: journal entry %d has no reason", ErrInvalidManifest, e.Sequence)
	}
	return nil
}

// ResidueKind is how far a destination mutation got before the attempt left the
// phase. Recovery needs the distinction: a reserved-but-untouched destination
// may be discarded, while a written one must be reconciled or rebuilt.
type ResidueKind uint8

const (
	// ResidueUnspecified is the zero value and never a recorded residue.
	ResidueUnspecified ResidueKind = iota
	// ResidueReserved records a new-destination reservation with no bytes written.
	ResidueReserved
	// ResidueInitialized records an initialized but unpopulated destination.
	ResidueInitialized
	// ResidueWritten records a destination that received logical records.
	ResidueWritten
)

// Valid reports whether the residue kind belongs to the closed set.
func (k ResidueKind) Valid() bool { return k >= ResidueReserved && k <= ResidueWritten }

// String returns the stable name of a residue kind.
func (k ResidueKind) String() string {
	switch k {
	case ResidueUnspecified:
		return "unspecified"
	case ResidueReserved:
		return "reserved"
	case ResidueInitialized:
		return "initialized"
	case ResidueWritten:
		return "written"
	default:
		return "invalid"
	}
}

// DestinationResidue is durable evidence that a destination was mutated under a
// named journal entry. Without it a restart cannot tell a pristine destination
// from one an earlier entry half-populated, and "no residue recorded" would be
// indistinguishable from "residue recorded and lost".
type DestinationResidue struct {
	Entry            uint64
	Binding          BindingName
	Component        ComponentID
	PhysicalIdentity PhysicalIdentity
	Kind             ResidueKind
}

// Validate rejects residue that does not name a physical destination.
func (r DestinationResidue) Validate() error {
	if r.Entry == 0 {
		return fmt.Errorf("%w: destination residue names no journal entry", ErrInvalidManifest)
	}
	if err := validateIdentifier("binding name", string(r.Binding)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidManifest, err)
	}
	if err := validateIdentifier("component ID", string(r.Component)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidManifest, err)
	}
	if strings.TrimSpace(string(r.PhysicalIdentity)) == "" {
		return fmt.Errorf("%w: destination residue has no physical identity", ErrInvalidManifest)
	}
	if err := validateSecretFree("destination residue physical identity", string(r.PhysicalIdentity)); err != nil {
		return err
	}
	if !r.Kind.Valid() {
		return fmt.Errorf("%w: destination residue has no kind", ErrInvalidManifest)
	}
	return nil
}

// GuardRelease records a guard that was installed and then deliberately
// released before any decision. GUARDING recovery discovers against the
// recorded plan; without the release record a discovered-absent guard is
// ambiguous between "never installed" and "installed then released".
type GuardRelease struct {
	Entry            uint64
	Provider         ProviderID
	Component        ComponentID
	PhysicalIdentity PhysicalIdentity
	Role             GuardRole
	ReceiptID        string
}

// Validate rejects a release record that cannot be matched to a guard plan entry.
func (r GuardRelease) Validate() error {
	if r.Entry == 0 {
		return fmt.Errorf("%w: guard release names no journal entry", ErrInvalidManifest)
	}
	if err := validateIdentifier("provider ID", string(r.Provider)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidManifest, err)
	}
	if err := validateIdentifier("component ID", string(r.Component)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidManifest, err)
	}
	if strings.TrimSpace(string(r.PhysicalIdentity)) == "" || strings.TrimSpace(r.ReceiptID) == "" {
		return fmt.Errorf("%w: guard release is missing physical identity or receipt", ErrInvalidManifest)
	}
	if !r.Role.Valid() {
		return fmt.Errorf("%w: guard release has no role", ErrInvalidManifest)
	}
	for field, value := range map[string]string{
		"guard release physical identity": string(r.PhysicalIdentity),
		"guard release receipt":           r.ReceiptID,
	} {
		if err := validateSecretFree(field, value); err != nil {
			return err
		}
	}
	return nil
}

// PhysicalFactKind is the closed set of per-component preservation facts the
// witness envelope pins (the witness contract). They are compatibility contract, not
// identity, and never appear in a semantic digest.
type PhysicalFactKind uint8

const (
	// PhysicalFactUnspecified is the zero value and never a recorded fact.
	PhysicalFactUnspecified PhysicalFactKind = iota
	// PhysicalFactUserVersion pins PRAGMA user_version.
	PhysicalFactUserVersion
	// PhysicalFactDependencyKeyLayout pins the Graph dependency-key layout in effect.
	PhysicalFactDependencyKeyLayout
	// PhysicalFactGraphSeqFloor pins the graph.seqfloor path, presence, identity, and bytes.
	PhysicalFactGraphSeqFloor
	// PhysicalFactSessionsSidecar pins the Sessions local-sidecar path, identity, and disposition.
	PhysicalFactSessionsSidecar
	// PhysicalFactComponentCensus pins the component byte length and SHA-256 at the fenced census.
	PhysicalFactComponentCensus
)

// Valid reports whether the fact kind belongs to the closed set.
func (k PhysicalFactKind) Valid() bool {
	return k >= PhysicalFactUserVersion && k <= PhysicalFactComponentCensus
}

// String returns the stable name of a physical fact kind.
func (k PhysicalFactKind) String() string {
	switch k {
	case PhysicalFactUnspecified:
		return "unspecified"
	case PhysicalFactUserVersion:
		return "user-version"
	case PhysicalFactDependencyKeyLayout:
		return "dependency-key-layout"
	case PhysicalFactGraphSeqFloor:
		return "graph-seqfloor"
	case PhysicalFactSessionsSidecar:
		return "sessions-sidecar"
	case PhysicalFactComponentCensus:
		return "component-census"
	default:
		return "invalid"
	}
}

// ComponentPhysicalFact is one pinned preservation fact for one component.
type ComponentPhysicalFact struct {
	Component ComponentID
	Kind      PhysicalFactKind
	Value     string
}

// Validate rejects a fact that pins nothing.
func (f ComponentPhysicalFact) Validate() error {
	if err := validateIdentifier("component ID", string(f.Component)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidWitness, err)
	}
	if !f.Kind.Valid() {
		return fmt.Errorf("%w: component %q has an unknown physical fact", ErrInvalidWitness, f.Component)
	}
	if strings.TrimSpace(f.Value) == "" {
		return fmt.Errorf("%w: component %q %s fact has no value", ErrInvalidWitness, f.Component, f.Kind)
	}
	return validateSecretFree("component physical fact", f.Value)
}

// cloneSemanticWitness returns a detached copy of a shipped witness value.
// SemanticWitness, WitnessFamilyCount, and SemanticWitnessAlgorithm ship in
// storebinding.go; the manifest records them and never redefines them.
func cloneSemanticWitness(witness SemanticWitness) SemanticWitness {
	out := witness
	out.Families = append([]WitnessFamilyCount(nil), witness.Families...)
	return out
}

// validateRecordedWitness adds the two manifest-side obligations the shipped
// witness validation does not carry: a witness must name at least one record
// family, because a witness that hashed nothing it can name cannot show that a
// destination is a strict subset of its source (the comparison matrix); and every
// family name must be secret-free like every other durable field.
func validateRecordedWitness(witness SemanticWitness) error {
	if err := witness.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidWitness, err)
	}
	if len(witness.Families) == 0 {
		return fmt.Errorf("%w: class %s witness names no record family", ErrInvalidWitness, witness.Class)
	}
	for _, family := range witness.Families {
		if err := validateSecretFree("witness record family", family.Name); err != nil {
			return err
		}
	}
	return nil
}

// WitnessEnvelope is the pinned physical half of a witness. It is never
// compared across providers; its only equality obligation is that a fresh
// reopen of an admitted destination reproduces it exactly.
type WitnessEnvelope struct {
	Version        uint16
	Descriptor     Descriptor
	Identity       BindingIdentity
	SemanticDigest string
	PhysicalFacts  []ComponentPhysicalFact
	Digest         string
}

// NewWitnessEnvelope pins one side of a witness and computes its digest from
// the descriptor's canonical identity payload, the semantic digest, and the
// preservation facts.
func NewWitnessEnvelope(descriptor Descriptor, semanticDigest string, facts []ComponentPhysicalFact) (WitnessEnvelope, error) {
	identity, err := descriptor.Identity()
	if err != nil {
		return WitnessEnvelope{}, err
	}
	envelope := WitnessEnvelope{
		Version:        1,
		Descriptor:     descriptor.Clone(),
		Identity:       identity,
		SemanticDigest: semanticDigest,
		PhysicalFacts:  sortedPhysicalFacts(facts),
	}
	digest, err := envelope.computeDigest()
	if err != nil {
		return WitnessEnvelope{}, err
	}
	envelope.Digest = digest
	if err := envelope.Validate(); err != nil {
		return WitnessEnvelope{}, err
	}
	return envelope, nil
}

// Clone returns a detached envelope value.
func (e WitnessEnvelope) Clone() WitnessEnvelope {
	out := e
	out.Descriptor = e.Descriptor.Clone()
	out.PhysicalFacts = append([]ComponentPhysicalFact(nil), e.PhysicalFacts...)
	return out
}

// Equal reports byte equality of two envelopes. The digest covers every pinned
// field, so envelope equality is digest equality plus the raw comparison that
// proves the digest was not carried over from another envelope.
func (e WitnessEnvelope) Equal(other WitnessEnvelope) bool {
	if e.Version != other.Version || e.Identity != other.Identity || e.SemanticDigest != other.SemanticDigest || e.Digest != other.Digest {
		return false
	}
	if !e.Descriptor.Equal(other.Descriptor) || len(e.PhysicalFacts) != len(other.PhysicalFacts) {
		return false
	}
	left := sortedPhysicalFacts(e.PhysicalFacts)
	right := sortedPhysicalFacts(other.PhysicalFacts)
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// Validate recomputes the envelope digest and rejects any mismatch, so a
// dropped, added, or edited preservation fact cannot pass as the admitted
// envelope.
func (e WitnessEnvelope) Validate() error {
	if e.Version != 1 {
		return fmt.Errorf("%w: envelope has no version", ErrInvalidWitness)
	}
	if err := e.Descriptor.Validate(); err != nil {
		return err
	}
	identity, err := e.Descriptor.Identity()
	if err != nil {
		return err
	}
	if e.Identity != identity {
		return fmt.Errorf("%w: envelope identity does not match its descriptor", ErrInvalidWitness)
	}
	if err := validateCanonicalSHA256Digest("envelope semantic digest", e.SemanticDigest); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidWitness, err)
	}
	if len(e.PhysicalFacts) == 0 {
		return fmt.Errorf("%w: envelope pins no physical preservation fact", ErrInvalidWitness)
	}
	seen := make(map[ComponentPhysicalFact]struct{}, len(e.PhysicalFacts))
	for _, fact := range e.PhysicalFacts {
		if err := fact.Validate(); err != nil {
			return err
		}
		key := ComponentPhysicalFact{Component: fact.Component, Kind: fact.Kind}
		if _, exists := seen[key]; exists {
			return fmt.Errorf("%w: component %q pins %s twice", ErrInvalidWitness, fact.Component, fact.Kind)
		}
		seen[key] = struct{}{}
	}
	digest, err := e.computeDigest()
	if err != nil {
		return err
	}
	if e.Digest != digest {
		return fmt.Errorf("%w: envelope digest does not cover its pinned facts", ErrInvalidWitness)
	}
	return nil
}

func (e WitnessEnvelope) computeDigest() (string, error) {
	payload, err := e.Descriptor.canonicalIdentityPayload()
	if err != nil {
		return "", err
	}
	var encoded canonicalDescriptorEncoding
	encoded.string(WitnessEnvelopeDomainV1)
	encoded.uint16(e.Version)
	encoded.raw(payload)
	encoded.string(string(e.Identity))
	encoded.string(e.SemanticDigest)
	facts := sortedPhysicalFacts(e.PhysicalFacts)
	encoded.uint64(uint64(len(facts)))
	for _, fact := range facts {
		encoded.string(string(fact.Component))
		encoded.uint16(uint16(fact.Kind))
		encoded.string(fact.Value)
	}
	return canonicalDigest(encoded.bytes), nil
}

func sortedPhysicalFacts(facts []ComponentPhysicalFact) []ComponentPhysicalFact {
	if facts == nil {
		return nil
	}
	out := append([]ComponentPhysicalFact(nil), facts...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Component != out[j].Component {
			return out[i].Component < out[j].Component
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Value < out[j].Value
	})
	return out
}

// ClassWitnessRecord is the complete PREPARED witness set for one class: the
// three semantic digests that must compare equal and the two envelopes that
// must reproduce each other.
type ClassWitnessRecord struct {
	Class               coordclass.Class
	Source              SemanticWitness
	Destination         SemanticWitness
	FreshReopen         SemanticWitness
	AdmittedEnvelope    WitnessEnvelope
	FreshReopenEnvelope WitnessEnvelope
}

// Clone returns a detached witness record.
func (r ClassWitnessRecord) Clone() ClassWitnessRecord {
	return ClassWitnessRecord{
		Class:               r.Class,
		Source:              cloneSemanticWitness(r.Source),
		Destination:         cloneSemanticWitness(r.Destination),
		FreshReopen:         cloneSemanticWitness(r.FreshReopen),
		AdmittedEnvelope:    r.AdmittedEnvelope.Clone(),
		FreshReopenEnvelope: r.FreshReopenEnvelope.Clone(),
	}
}

// Validate enforces the comparison matrix. Every row is a separate blocking
// check, because each excludes a different failure: unequal source and
// destination digests mean the copy is lossy, an unreproduced envelope means
// the durable bytes are not the ones admitted, and a mismatched algorithm
// version means two incomparable streams were compared as if they were one.
func (r ClassWitnessRecord) Validate(algorithm string) error {
	for _, witness := range []SemanticWitness{r.Source, r.Destination, r.FreshReopen} {
		if err := validateRecordedWitness(witness); err != nil {
			return err
		}
		if witness.Class != r.Class {
			return fmt.Errorf("%w: class %s record carries a %s witness", ErrInvalidWitness, r.Class, witness.Class)
		}
		if witness.Algorithm != algorithm {
			return fmt.Errorf("%w: class %s witness uses algorithm %q, attempt uses %q", ErrInvalidWitness, r.Class, witness.Algorithm, algorithm)
		}
	}
	if r.Source.Contract != r.Destination.Contract || r.Source.Contract != r.FreshReopen.Contract {
		return fmt.Errorf("%w: class %s witnesses disagree on the semantic contract version", ErrInvalidWitness, r.Class)
	}
	if r.Source.Digest != r.Destination.Digest {
		return fmt.Errorf("%w: class %s source and destination semantic digests differ", ErrInvalidWitness, r.Class)
	}
	if r.Destination.Digest != r.FreshReopen.Digest {
		return fmt.Errorf("%w: class %s fresh reopen did not reproduce the destination semantic digest", ErrInvalidWitness, r.Class)
	}
	for _, envelope := range []WitnessEnvelope{r.AdmittedEnvelope, r.FreshReopenEnvelope} {
		if err := envelope.Validate(); err != nil {
			return err
		}
		if envelope.SemanticDigest != r.Destination.Digest {
			return fmt.Errorf("%w: class %s envelope is not bound to its semantic digest", ErrInvalidWitness, r.Class)
		}
	}
	if !r.AdmittedEnvelope.Equal(r.FreshReopenEnvelope) {
		return fmt.Errorf("%w: class %s fresh reopen did not reproduce the admitted envelope", ErrInvalidWitness, r.Class)
	}
	return nil
}
