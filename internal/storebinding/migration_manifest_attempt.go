package storebinding

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/coordclass"
)

// AttemptRecord is the phase-typed durable saga record. intent derivation owns its shape and
// its transition rules; the later saga steps drive the transitions. Every field a phase adds
// is mandatory at that phase, and later phases only add — the one exception is
// the explicit PREPARED-to-PREPARING return, which is journaled rather
// than silent.
type AttemptRecord struct {
	Version              uint16
	Phase                MigrationPhase
	Journal              []PhaseEntry
	Intent               IntentSection
	Preparing            *PreparingSection
	Prepared             *PreparedSection
	Guarding             *GuardingSection
	GuardsInstalled      *GuardsInstalledSection
	Decision             *CommitDecisionSection
	Receipts             []ParticipantReceipt
	Residue              []DestinationResidue
	ReleasedGuards       []GuardRelease
	ActiveManifestDigest string
}

// NewAttemptRecord opens a saga at INTENT_FSYNCED from a derived intent. It is
// the only way to create an attempt record, so an attempt can never pin
// identities that were not derived by comparing desired configuration against
// discovered physical state.
func NewAttemptRecord(intent MigrationIntent) (*AttemptRecord, error) {
	if intent.Resumed() {
		return nil, fmt.Errorf("%w: a resumed intent already has a durable record", ErrInvalidMigrationIntent)
	}
	section, err := intent.intentSection()
	if err != nil {
		return nil, err
	}
	record := &AttemptRecord{
		Version: 1,
		Phase:   PhaseIntentFsynced,
		Journal: []PhaseEntry{{Sequence: 1, Phase: PhaseIntentFsynced, Reason: PhaseEntryInitial}},
		Intent:  section,
	}
	if err := record.Validate(); err != nil {
		return nil, err
	}
	return record, nil
}

// Clone returns a detached attempt record.
func (r *AttemptRecord) Clone() *AttemptRecord {
	if r == nil {
		return nil
	}
	out := &AttemptRecord{
		Version:              r.Version,
		Phase:                r.Phase,
		Journal:              append([]PhaseEntry(nil), r.Journal...),
		Intent:               r.Intent.Clone(),
		Residue:              append([]DestinationResidue(nil), r.Residue...),
		ReleasedGuards:       append([]GuardRelease(nil), r.ReleasedGuards...),
		ActiveManifestDigest: r.ActiveManifestDigest,
	}
	if r.Preparing != nil {
		section := r.Preparing.Clone()
		out.Preparing = &section
	}
	if r.Prepared != nil {
		section := r.Prepared.Clone()
		out.Prepared = &section
	}
	if r.Guarding != nil {
		section := r.Guarding.Clone()
		out.Guarding = &section
	}
	if r.GuardsInstalled != nil {
		section := r.GuardsInstalled.Clone()
		out.GuardsInstalled = &section
	}
	if r.Decision != nil {
		section := r.Decision.Clone()
		out.Decision = &section
	}
	if r.Receipts != nil {
		out.Receipts = make([]ParticipantReceipt, 0, len(r.Receipts))
		for _, receipt := range r.Receipts {
			out.Receipts = append(out.Receipts, receipt.Clone())
		}
	}
	return out
}

// Attempt returns the attempt identity the record pins.
func (r *AttemptRecord) Attempt() AttemptID { return r.Intent.Attempt }

// Generation returns the generation the attempt makes authoritative at COMMIT_DECIDED.
func (r *AttemptRecord) Generation() Generation { return r.Intent.DesiredGeneration }

// Entries returns how many times the saga has entered its current phase. One
// entry means the phase has never been left; more means an earlier entry ran
// and was durably rolled back.
func (r *AttemptRecord) Entries(phase MigrationPhase) uint64 {
	var entries uint64
	for _, entry := range r.Journal {
		if entry.Phase == phase {
			entries++
		}
	}
	return entries
}

// LastEntry returns the most recent journal entry.
func (r *AttemptRecord) LastEntry() PhaseEntry {
	if len(r.Journal) == 0 {
		return PhaseEntry{}
	}
	return r.Journal[len(r.Journal)-1]
}

// lastEntrySequence returns the sequence of the most recent entry into a phase,
// or zero if the journal never entered it. It is the epoch boundary evidence is
// matched against: a receipt or release from an earlier entry into the same
// phase belongs to a run that was already rolled back.
func (r *AttemptRecord) lastEntrySequence(phase MigrationPhase) uint64 {
	var sequence uint64
	for _, entry := range r.Journal {
		if entry.Phase == phase {
			sequence = entry.Sequence
		}
	}
	return sequence
}

// PendingParticipants returns the participants the decision closed over that
// have no durable receipt yet, in canonical order.
func (r *AttemptRecord) PendingParticipants() []string {
	if r.Decision == nil {
		return nil
	}
	received := make(map[string]struct{}, len(r.Receipts))
	for _, receipt := range r.Receipts {
		received[receipt.Participant] = struct{}{}
	}
	pending := make([]string, 0, len(r.Decision.Participants))
	for _, participant := range r.Decision.Participants {
		if _, exists := received[participant]; !exists {
			pending = append(pending, participant)
		}
	}
	sort.Strings(pending)
	return pending
}

// ReceiptsComplete reports whether every closed participant has a receipt.
func (r *AttemptRecord) ReceiptsComplete() bool {
	return r.Decision != nil && len(r.PendingParticipants()) == 0
}

// EnterPreparing records the PREPARING field set. The reason distinguishes the
// first entry from a durable return; a return preserves the residue and
// guard-release journals and discards the later sections that are no longer
// true, so recovery can never read a stale PREPARED as current. Discarding the
// GUARDING and GUARDS_INSTALLED sections destroys every durable install
// receipt, so a return that would do that is refused until each of those
// receipts has a matching release: the saga releases the guards first and
// returns second, and a return recorded without the release evidence would read
// afterwards exactly like a guard that was never installed.
func (r *AttemptRecord) EnterPreparing(section PreparingSection, reason PhaseEntryReason) error {
	if !reason.Valid() {
		return fmt.Errorf("%w: entering PREPARING requires a reason", ErrInvalidPhaseTransition)
	}
	switch {
	case reason == PhaseEntryInitial:
		if r.Phase != PhaseIntentFsynced {
			return fmt.Errorf("%w: initial PREPARING entry from %s", ErrInvalidPhaseTransition, r.Phase)
		}
	case reason.Return():
		if !r.Phase.SourceAuthoritative() {
			return fmt.Errorf("%w: %s from %s; the decision is immutable and recovery only rolls forward", ErrInvalidPhaseTransition, reason, r.Phase)
		}
		if r.Phase < PhasePrepared {
			return fmt.Errorf("%w: %s from %s, which has nothing to return from", ErrInvalidPhaseTransition, reason, r.Phase)
		}
		if err := r.requireInstalledGuardsReleased(reason); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%w: %s does not enter PREPARING", ErrInvalidPhaseTransition, reason)
	}
	if err := section.Validate(); err != nil {
		return err
	}
	next := r.Clone()
	next.Phase = PhasePreparing
	prepared := section.Clone()
	next.Preparing = &prepared
	next.Prepared = nil
	next.Guarding = nil
	next.GuardsInstalled = nil
	next.appendEntry(PhasePreparing, reason)
	return r.commit(next)
}

// requireInstalledGuardsReleased proves that every durable install receipt a
// return would discard has a matching release. Matching is by guard identity
// and receipt ID rather than by count, so a release of a different guard — or
// of an earlier entry's receipt that reused the same receipt ID — is not
// accepted as evidence that this guard came off. Only releases recorded under
// the current GUARDING entry count, because a release journaled before the
// record last re-entered GUARDING describes a guard installation that was
// already rolled back.
func (r *AttemptRecord) requireInstalledGuardsReleased(reason PhaseEntryReason) error {
	outstanding := r.installedGuardReceipts()
	if len(outstanding) == 0 {
		return nil
	}
	entry := r.lastEntrySequence(PhaseGuarding)
	if entry == 0 {
		return fmt.Errorf("%w: the record carries installed-guard receipts with no GUARDING journal entry to release them under", ErrInvalidManifest)
	}
	for _, release := range r.ReleasedGuards {
		if release.Entry < entry {
			continue
		}
		delete(outstanding, newGuardReleaseKey(release.Provider, release.Component, release.PhysicalIdentity, release.Role, release.ReceiptID))
	}
	if len(outstanding) == 0 {
		return nil
	}
	unreleased := make([]string, 0, len(outstanding))
	for key := range outstanding {
		unreleased = append(unreleased, fmt.Sprintf("%s receipt %s", key.guard.component, key.receipt))
	}
	sort.Strings(unreleased)
	return fmt.Errorf("%w: %s from %s would discard %d installed-guard receipts that were never released: %v", ErrInvalidPhaseTransition, reason, r.Phase, len(unreleased), unreleased)
}

// installedGuardReceipts returns every durable install receipt the record
// carries, keyed by guard identity and receipt ID. A verified receipt counts in
// its own right: when verification recorded a different receipt ID than the
// install did, both name a guard that is still enforced on the source.
func (r *AttemptRecord) installedGuardReceipts() map[guardReleaseKey]struct{} {
	if r.Guarding == nil {
		return nil
	}
	receipts := make(map[guardReleaseKey]struct{}, len(r.Guarding.Receipts))
	for _, receipt := range r.Guarding.Receipts {
		receipts[guardReleaseKeyForReceipt(receipt)] = struct{}{}
	}
	if r.GuardsInstalled != nil {
		for _, receipt := range r.GuardsInstalled.Verified {
			receipts[guardReleaseKeyForReceipt(receipt)] = struct{}{}
		}
	}
	return receipts
}

// EnterPrepared records the PREPARED field set.
func (r *AttemptRecord) EnterPrepared(section PreparedSection) error {
	if r.Phase != PhasePreparing {
		return fmt.Errorf("%w: PREPARED from %s", ErrInvalidPhaseTransition, r.Phase)
	}
	if err := section.Validate(r.Intent); err != nil {
		return err
	}
	next := r.Clone()
	next.Phase = PhasePrepared
	prepared := section.Clone()
	next.Prepared = &prepared
	next.appendEntry(PhasePrepared, PhaseEntryInitial)
	return r.commit(next)
}

// EnterGuarding records the complete guard plan before the first install.
func (r *AttemptRecord) EnterGuarding(section GuardingSection) error {
	if r.Phase != PhasePrepared {
		return fmt.Errorf("%w: GUARDING from %s", ErrInvalidPhaseTransition, r.Phase)
	}
	if len(section.Receipts) != 0 {
		return fmt.Errorf("%w: the guard plan is recorded before the first install, so it carries no receipt", ErrInvalidPhaseTransition)
	}
	if err := section.Validate(r.Intent); err != nil {
		return err
	}
	next := r.Clone()
	next.Phase = PhaseGuarding
	guarding := section.Clone()
	next.Guarding = &guarding
	next.appendEntry(PhaseGuarding, PhaseEntryInitial)
	return r.commit(next)
}

// AppendGuardReceipt appends one installed or discovered guard receipt
// idempotently. Re-appending an identical receipt is a no-op, which is what
// makes GUARDING recovery replayable; appending a different receipt for the
// same guard is a conflict and blocks.
func (r *AttemptRecord) AppendGuardReceipt(receipt GuardReceipt) error {
	if r.Phase != PhaseGuarding {
		return fmt.Errorf("%w: guard receipts append in GUARDING, not %s", ErrInvalidPhaseTransition, r.Phase)
	}
	if r.Guarding == nil {
		return fmt.Errorf("%w: GUARDING has no recorded plan", ErrInvalidManifest)
	}
	key := newGuardPlanKey(receipt.Provider, receipt.Component, receipt.PhysicalIdentity, receipt.Role)
	for _, existing := range r.Guarding.Receipts {
		if newGuardPlanKey(existing.Provider, existing.Component, existing.PhysicalIdentity, existing.Role) != key {
			continue
		}
		if existing.ReceiptID == receipt.ReceiptID && existing.Revalidation == receipt.Revalidation {
			return nil
		}
		return fmt.Errorf("%w: guard on %q already has a different durable receipt", ErrInvalidManifest, receipt.Component)
	}
	next := r.Clone()
	next.Guarding.Receipts = append(next.Guarding.Receipts, receipt.Clone())
	if err := next.Guarding.Validate(next.Intent); err != nil {
		return err
	}
	return r.commit(next)
}

// RecordGuardRelease records a pre-decision guard release under the current
// journal entry. Recovery reads it to tell a guard that was never installed
// from one that was installed and deliberately released.
func (r *AttemptRecord) RecordGuardRelease(release GuardRelease) error {
	if !r.Phase.SourceAuthoritative() {
		return fmt.Errorf("%w: releasing a retained guard is forbidden from %s", ErrInvalidPhaseTransition, r.Phase)
	}
	release.Entry = r.LastEntry().Sequence
	if err := release.Validate(); err != nil {
		return err
	}
	next := r.Clone()
	next.ReleasedGuards = append(next.ReleasedGuards, release)
	return r.commit(next)
}

// RecordDestinationResidue records that a destination component was mutated
// under the current journal entry.
func (r *AttemptRecord) RecordDestinationResidue(residue DestinationResidue) error {
	if r.Phase != PhasePreparing {
		return fmt.Errorf("%w: destinations are mutated in PREPARING, not %s", ErrInvalidPhaseTransition, r.Phase)
	}
	residue.Entry = r.LastEntry().Sequence
	if err := residue.Validate(); err != nil {
		return err
	}
	next := r.Clone()
	for index, existing := range next.Residue {
		if existing.Entry == residue.Entry && existing.PhysicalIdentity == residue.PhysicalIdentity {
			if existing.Kind > residue.Kind {
				return nil
			}
			next.Residue[index] = residue
			return r.commit(next)
		}
	}
	next.Residue = append(next.Residue, residue)
	return r.commit(next)
}

// EnterGuardsInstalled records the verification proof.
func (r *AttemptRecord) EnterGuardsInstalled(section GuardsInstalledSection) error {
	if r.Phase != PhaseGuarding {
		return fmt.Errorf("%w: GUARDS_INSTALLED from %s", ErrInvalidPhaseTransition, r.Phase)
	}
	if r.Guarding == nil {
		return fmt.Errorf("%w: GUARDING has no recorded plan", ErrInvalidManifest)
	}
	if !r.Guarding.Complete() {
		return fmt.Errorf("%w: %d of %d planned guards have durable receipts", ErrInvalidPhaseTransition, len(r.Guarding.Receipts), len(r.Guarding.Plan))
	}
	if err := section.Validate(*r.Guarding); err != nil {
		return err
	}
	next := r.Clone()
	next.Phase = PhaseGuardsInstalled
	installed := section.Clone()
	next.GuardsInstalled = &installed
	next.appendEntry(PhaseGuardsInstalled, PhaseEntryInitial)
	return r.commit(next)
}

// EnterCommitDecided writes the single authority transition. It is legal only
// from GUARDS_INSTALLED, and once written it is immutable.
func (r *AttemptRecord) EnterCommitDecided(section CommitDecisionSection) error {
	if r.Phase != PhaseGuardsInstalled {
		return fmt.Errorf("%w: COMMIT_DECIDED from %s", ErrInvalidPhaseTransition, r.Phase)
	}
	if err := section.Validate(r.Intent); err != nil {
		return err
	}
	next := r.Clone()
	next.Phase = PhaseCommitDecided
	decision := section.Clone()
	next.Decision = &decision
	next.appendEntry(PhaseCommitDecided, PhaseEntryInitial)
	return r.commit(next)
}

// AppendReceipt appends one participant receipt idempotently. An identical
// receipt for a participant that already has one is a no-op so post-decision
// roll-forward replays only what is missing; a different receipt for the same
// participant is a conflict and blocks.
func (r *AttemptRecord) AppendReceipt(receipt ParticipantReceipt) error {
	if r.Phase < PhaseCommitDecided {
		return fmt.Errorf("%w: participant receipts are post-decision, not %s", ErrInvalidPhaseTransition, r.Phase)
	}
	if r.Decision == nil {
		return fmt.Errorf("%w: no decision closed the participant set", ErrInvalidManifest)
	}
	if err := receipt.Validate(); err != nil {
		return err
	}
	if receipt.Attempt != r.Intent.Attempt || receipt.Generation != r.Intent.DesiredGeneration {
		return fmt.Errorf("%w: receipt is bound to another attempt or generation", ErrInvalidManifest)
	}
	closed := false
	for _, participant := range r.Decision.Participants {
		if participant == receipt.Participant {
			closed = true
			break
		}
	}
	if !closed {
		return fmt.Errorf("%w: %q is not in the decided participant set", ErrInvalidManifest, receipt.Participant)
	}
	for _, existing := range r.Receipts {
		if existing.Participant != receipt.Participant {
			continue
		}
		if existing.Equal(receipt) {
			return nil
		}
		return fmt.Errorf("%w: participant %q already has a different durable receipt", ErrInvalidManifest, receipt.Participant)
	}
	next := r.Clone()
	next.Receipts = append(next.Receipts, receipt.Clone())
	return r.commit(next)
}

// EnterReceiptsPersisted advances once every closed participant has a receipt.
func (r *AttemptRecord) EnterReceiptsPersisted() error {
	if r.Phase != PhaseCommitDecided {
		return fmt.Errorf("%w: RECEIPTS_PERSISTED from %s", ErrInvalidPhaseTransition, r.Phase)
	}
	if pending := r.PendingParticipants(); len(pending) != 0 {
		return fmt.Errorf("%w: %d participants still have no receipt: %v", ErrInvalidPhaseTransition, len(pending), pending)
	}
	next := r.Clone()
	next.Phase = PhaseReceiptsPersisted
	next.appendEntry(PhaseReceiptsPersisted, PhaseEntryInitial)
	return r.commit(next)
}

// EnterActiveManifestDurable records that the active manifest replacement is
// durable, pinned by its digest so a later phase cannot be satisfied by a
// different manifest.
func (r *AttemptRecord) EnterActiveManifestDurable(manifestDigest string) error {
	if r.Phase != PhaseReceiptsPersisted {
		return fmt.Errorf("%w: ACTIVE_MANIFEST_DURABLE from %s", ErrInvalidPhaseTransition, r.Phase)
	}
	if err := validateCanonicalSHA256Digest("active manifest digest", manifestDigest); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidManifest, err)
	}
	next := r.Clone()
	next.Phase = PhaseActiveManifestDurable
	next.ActiveManifestDigest = manifestDigest
	next.appendEntry(PhaseActiveManifestDurable, PhaseEntryInitial)
	return r.commit(next)
}

// EnterActiveOpenPending records that the exact active descriptors must be opened.
func (r *AttemptRecord) EnterActiveOpenPending() error {
	if r.Phase != PhaseActiveManifestDurable {
		return fmt.Errorf("%w: ACTIVE_OPEN_PENDING from %s", ErrInvalidPhaseTransition, r.Phase)
	}
	next := r.Clone()
	next.Phase = PhaseActiveOpenPending
	next.appendEntry(PhaseActiveOpenPending, PhaseEntryInitial)
	return r.commit(next)
}

// EnterActive closes the saga. activation calls it only after one StoreSet has been
// published; the record never infers liveness from the phase.
func (r *AttemptRecord) EnterActive() error {
	if r.Phase != PhaseActiveOpenPending {
		return fmt.Errorf("%w: ACTIVE from %s", ErrInvalidPhaseTransition, r.Phase)
	}
	next := r.Clone()
	next.Phase = PhaseActive
	next.appendEntry(PhaseActive, PhaseEntryInitial)
	return r.commit(next)
}

func (r *AttemptRecord) appendEntry(phase MigrationPhase, reason PhaseEntryReason) {
	r.Journal = append(r.Journal, PhaseEntry{Sequence: uint64(len(r.Journal)) + 1, Phase: phase, Reason: reason})
}

// commit validates a candidate record before it replaces the receiver, so a
// transition that would produce an unreadable record fails with the record
// unchanged rather than leaving a half-applied one in memory to be fsynced.
func (r *AttemptRecord) commit(next *AttemptRecord) error {
	if err := next.Validate(); err != nil {
		return err
	}
	*r = *next
	return nil
}

// Validate rejects any record whose phase and field sets disagree.
func (r *AttemptRecord) Validate() error {
	if r == nil {
		return fmt.Errorf("%w: nil attempt record", ErrInvalidManifest)
	}
	if r.Version != 1 {
		return fmt.Errorf("%w: attempt record has no version", ErrInvalidManifest)
	}
	if !r.Phase.Valid() {
		return fmt.Errorf("%w: attempt record has no phase", ErrInvalidManifest)
	}
	if err := r.Intent.Validate(); err != nil {
		return err
	}
	if err := r.validateJournal(); err != nil {
		return err
	}
	if err := r.validateSectionPresence(); err != nil {
		return err
	}
	if r.Preparing != nil {
		if err := r.Preparing.Validate(); err != nil {
			return err
		}
	}
	if r.Prepared != nil {
		if err := r.Prepared.Validate(r.Intent); err != nil {
			return err
		}
	}
	if r.Guarding != nil {
		if err := r.Guarding.Validate(r.Intent); err != nil {
			return err
		}
	}
	if r.GuardsInstalled != nil {
		if err := r.GuardsInstalled.Validate(*r.Guarding); err != nil {
			return err
		}
	}
	if r.Decision != nil {
		if err := r.Decision.Validate(r.Intent); err != nil {
			return err
		}
	}
	if err := r.validateReceipts(); err != nil {
		return err
	}
	return r.validateEvidence()
}

func (r *AttemptRecord) validateJournal() error {
	if len(r.Journal) == 0 {
		return fmt.Errorf("%w: attempt record has an empty phase journal", ErrInvalidManifest)
	}
	first := r.Journal[0]
	if first.Phase != PhaseIntentFsynced || first.Reason != PhaseEntryInitial {
		return fmt.Errorf("%w: the journal does not open at INTENT_FSYNCED", ErrInvalidManifest)
	}
	for index, entry := range r.Journal {
		if err := entry.Validate(); err != nil {
			return err
		}
		if entry.Sequence != uint64(index)+1 {
			return fmt.Errorf("%w: journal entry %d is out of sequence", ErrInvalidManifest, entry.Sequence)
		}
		if index > 0 && entry.Reason.Return() && entry.Phase != PhasePreparing {
			return fmt.Errorf("%w: journal entry %d returns into %s; only PREPARING is returnable", ErrInvalidManifest, entry.Sequence, entry.Phase)
		}
	}
	if last := r.LastEntry(); last.Phase != r.Phase {
		return fmt.Errorf("%w: record phase %s does not match the last journal entry %s", ErrInvalidManifest, r.Phase, last.Phase)
	}
	return nil
}

func (r *AttemptRecord) validateSectionPresence() error {
	required := map[MigrationPhase]bool{
		PhasePreparing:       r.Preparing != nil,
		PhasePrepared:        r.Prepared != nil,
		PhaseGuarding:        r.Guarding != nil,
		PhaseGuardsInstalled: r.GuardsInstalled != nil,
		PhaseCommitDecided:   r.Decision != nil,
	}
	for phase, present := range required {
		if r.Phase >= phase && !present {
			return fmt.Errorf("%w: record at %s has no %s section", ErrInvalidManifest, r.Phase, phase)
		}
		if r.Phase < phase && present {
			return fmt.Errorf("%w: record at %s already carries a %s section", ErrInvalidManifest, r.Phase, phase)
		}
	}
	if (r.Phase >= PhaseActiveManifestDurable) != (strings.TrimSpace(r.ActiveManifestDigest) != "") {
		return fmt.Errorf("%w: record at %s disagrees with its active-manifest digest", ErrInvalidManifest, r.Phase)
	}
	if r.ActiveManifestDigest != "" {
		if err := validateCanonicalSHA256Digest("active manifest digest", r.ActiveManifestDigest); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidManifest, err)
		}
	}
	return nil
}

func (r *AttemptRecord) validateReceipts() error {
	if len(r.Receipts) != 0 && r.Decision == nil {
		return fmt.Errorf("%w: receipts exist with no decision", ErrInvalidManifest)
	}
	seen := make(map[string]struct{}, len(r.Receipts))
	for _, receipt := range r.Receipts {
		if err := receipt.Validate(); err != nil {
			return err
		}
		if receipt.Attempt != r.Intent.Attempt || receipt.Generation != r.Intent.DesiredGeneration {
			return fmt.Errorf("%w: receipt for %q is bound to another attempt or generation", ErrInvalidManifest, receipt.Participant)
		}
		if _, exists := seen[receipt.Participant]; exists {
			return fmt.Errorf("%w: participant %q has two receipts", ErrInvalidManifest, receipt.Participant)
		}
		seen[receipt.Participant] = struct{}{}
	}
	if r.Phase >= PhaseReceiptsPersisted && !r.ReceiptsComplete() {
		return fmt.Errorf("%w: record at %s is missing participant receipts: %v", ErrInvalidManifest, r.Phase, r.PendingParticipants())
	}
	return nil
}

func (r *AttemptRecord) validateEvidence() error {
	entries := uint64(len(r.Journal))
	for _, residue := range r.Residue {
		if err := residue.Validate(); err != nil {
			return err
		}
		if residue.Entry > entries {
			return fmt.Errorf("%w: destination residue names journal entry %d of %d", ErrInvalidManifest, residue.Entry, entries)
		}
	}
	for _, release := range r.ReleasedGuards {
		if err := release.Validate(); err != nil {
			return err
		}
		if release.Entry > entries {
			return fmt.Errorf("%w: guard release names journal entry %d of %d", ErrInvalidManifest, release.Entry, entries)
		}
	}
	return nil
}

// ResumeAction is the closed set of recovery routes the crash table
// defines. Each phase maps to exactly one; there is no third state and no
// "probably fine" route.
type ResumeAction uint8

const (
	// ResumeActionUnspecified is the zero value and never a derived route.
	ResumeActionUnspecified ResumeAction = iota
	// ResumeActionReacquirePins reacquires from the pinned FenceTarget locators.
	ResumeActionReacquirePins
	// ResumeActionResumePreparation reacquires fences and resumes or safely
	// abandons preparation.
	ResumeActionResumePreparation
	// ResumeActionRecensusSource recensuses the source; an unchanged digest
	// revalidates, a changed one durably returns to PREPARING.
	ResumeActionRecensusSource
	// ResumeActionDiscoverGuards discovers every planned install before deciding
	// whether to complete or release the set.
	ResumeActionDiscoverGuards
	// ResumeActionVerifyGuards recensuses the unchanged source and verifies every guard.
	ResumeActionVerifyGuards
	// ResumeActionRollForwardReceipts replays only the missing idempotent commits.
	ResumeActionRollForwardReceipts
	// ResumeActionRebuildActiveManifest rebuilds the manifest from decision and receipts.
	ResumeActionRebuildActiveManifest
	// ResumeActionReopenActive verifies guards and reopens the exact active descriptors.
	ResumeActionReopenActive
	// ResumeActionNormalRestart is the terminal route after descriptor and guard verification.
	ResumeActionNormalRestart
)

// Valid reports whether the action is one of the derived routes.
func (a ResumeAction) Valid() bool {
	return a >= ResumeActionReacquirePins && a <= ResumeActionNormalRestart
}

// String returns the stable name of a resume action.
func (a ResumeAction) String() string {
	switch a {
	case ResumeActionUnspecified:
		return "unspecified"
	case ResumeActionReacquirePins:
		return "reacquire-pins"
	case ResumeActionResumePreparation:
		return "resume-preparation"
	case ResumeActionRecensusSource:
		return "recensus-source"
	case ResumeActionDiscoverGuards:
		return "discover-guards"
	case ResumeActionVerifyGuards:
		return "verify-guards"
	case ResumeActionRollForwardReceipts:
		return "roll-forward-receipts"
	case ResumeActionRebuildActiveManifest:
		return "rebuild-active-manifest"
	case ResumeActionReopenActive:
		return "reopen-active"
	case ResumeActionNormalRestart:
		return "normal-restart"
	default:
		return "invalid"
	}
}

// ResumeAuthority names which generation is authoritative on restart.
type ResumeAuthority uint8

const (
	// ResumeAuthorityUnspecified is the zero value and never a derived authority.
	ResumeAuthorityUnspecified ResumeAuthority = iota
	// ResumeAuthorityPrior means the prior generation or genesis evidence rules
	// and every destination is disposable.
	ResumeAuthorityPrior
	// ResumeAuthorityDesired means the desired generation rules and recovery may
	// only roll forward.
	ResumeAuthorityDesired
)

// String returns the stable name of a resume authority.
func (a ResumeAuthority) String() string {
	switch a {
	case ResumeAuthorityUnspecified:
		return "unspecified"
	case ResumeAuthorityPrior:
		return "prior"
	case ResumeAuthorityDesired:
		return "desired"
	default:
		return "invalid"
	}
}

// ResumePlan is what a durable record tells recovery. PhaseEntries,
// DirtyDestinations, and ReleasedGuards are the fields that answer "was this
// phase entered before?" — the question a bare phase field cannot answer.
type ResumePlan struct {
	Action               ResumeAction
	Authority            ResumeAuthority
	Attempt              AttemptID
	PriorGeneration      Generation
	DesiredGeneration    Generation
	Phase                MigrationPhase
	PhaseEntries         uint64
	Returned             bool
	DirtyDestinations    []DestinationResidue
	ReleasedGuards       []GuardRelease
	PendingReceipts      []string
	WitnessAlgorithm     string
	ConfigDigest         CompositeConfigDigest
	ActiveManifestPinned bool
}

// Resume derives the recovery route from the durable record alone. It reads no
// configuration: the caller compares the returned ConfigDigest against current
// configuration and blocks on mismatch before acting on the plan.
func (r *AttemptRecord) Resume() (ResumePlan, error) {
	if err := r.Validate(); err != nil {
		return ResumePlan{}, err
	}
	plan := ResumePlan{
		Attempt:              r.Intent.Attempt,
		PriorGeneration:      r.Intent.PriorGeneration,
		DesiredGeneration:    r.Intent.DesiredGeneration,
		Phase:                r.Phase,
		PhaseEntries:         r.Entries(r.Phase),
		Returned:             r.LastEntry().Reason.Return(),
		DirtyDestinations:    append([]DestinationResidue(nil), r.Residue...),
		ReleasedGuards:       append([]GuardRelease(nil), r.ReleasedGuards...),
		PendingReceipts:      r.PendingParticipants(),
		WitnessAlgorithm:     r.Intent.WitnessAlgorithm,
		ConfigDigest:         r.Intent.ConfigDigest,
		ActiveManifestPinned: r.ActiveManifestDigest != "",
	}
	plan.Authority = ResumeAuthorityDesired
	if r.Phase.SourceAuthoritative() {
		plan.Authority = ResumeAuthorityPrior
	}
	switch r.Phase {
	case PhaseIntentFsynced:
		plan.Action = ResumeActionReacquirePins
	case PhasePreparing:
		plan.Action = ResumeActionResumePreparation
	case PhasePrepared:
		plan.Action = ResumeActionRecensusSource
	case PhaseGuarding:
		plan.Action = ResumeActionDiscoverGuards
	case PhaseGuardsInstalled:
		plan.Action = ResumeActionVerifyGuards
	case PhaseCommitDecided:
		plan.Action = ResumeActionRollForwardReceipts
	case PhaseReceiptsPersisted:
		plan.Action = ResumeActionRebuildActiveManifest
	case PhaseActiveManifestDurable, PhaseActiveOpenPending:
		plan.Action = ResumeActionReopenActive
	case PhaseActive:
		plan.Action = ResumeActionNormalRestart
	default:
		return ResumePlan{}, fmt.Errorf("%w: no recovery route for %s", ErrInvalidManifest, r.Phase)
	}
	return plan, nil
}

// ActiveManifest is the durable record of one active generation. It is a
// separate atomic replacement from the attempt record and outlives it: the
// retained-guard records it carries are revalidated on every startup, status,
// and doctor pass for the whole generation.
type ActiveManifest struct {
	Version              uint16
	Generation           Generation
	Attempt              AttemptID
	Assignments          map[coordclass.Class]BindingName
	Descriptors          []NamedDescriptor
	Receipts             []ParticipantReceipt
	Guards               []GuardReceipt
	RetainedSources      []RetainedSourceRef
	WorkProofs           []WorkProof
	ConfigDigest         CompositeConfigDigest
	BindingConfigDigests map[BindingName]ConfigRefDigest
	WitnessAlgorithm     string
	CutoverGeneration    Generation
	RollbackGeneration   Generation
}

// NewActiveManifest builds the active manifest from a decided attempt and the
// exact active descriptors. Building it from the decision and receipts is what
// makes the "rebuild the manifest from decision and receipts" recovery a
// replay rather than a reconstruction from memory.
func NewActiveManifest(record *AttemptRecord, descriptors []NamedDescriptor) (*ActiveManifest, error) {
	if record == nil {
		return nil, fmt.Errorf("%w: nil attempt record", ErrInvalidManifest)
	}
	if err := record.Validate(); err != nil {
		return nil, err
	}
	if record.Phase < PhaseCommitDecided {
		return nil, fmt.Errorf("%w: the active manifest is post-decision, not %s", ErrInvalidPhaseTransition, record.Phase)
	}
	if !record.ReceiptsComplete() {
		return nil, fmt.Errorf("%w: %d participants still have no receipt", ErrInvalidPhaseTransition, len(record.PendingParticipants()))
	}
	manifest := &ActiveManifest{
		Version:              1,
		Generation:           record.Intent.DesiredGeneration,
		Attempt:              record.Intent.Attempt,
		Assignments:          cloneAssignments(record.Intent.DesiredAssignments),
		Descriptors:          cloneNamedDescriptors(descriptors),
		ConfigDigest:         record.Intent.ConfigDigest,
		BindingConfigDigests: cloneBindingDigests(record.Intent.BindingConfigDigests),
		WitnessAlgorithm:     record.Intent.WitnessAlgorithm,
		CutoverGeneration:    record.Intent.DesiredGeneration,
		RollbackGeneration:   record.Intent.PriorGeneration,
	}
	for _, receipt := range record.Receipts {
		manifest.Receipts = append(manifest.Receipts, receipt.Clone())
	}
	if record.Guarding != nil && record.Guarding.Receipts != nil {
		manifest.Guards = cloneGuardReceipts(record.Guarding.Receipts)
	}
	if record.Prepared != nil {
		manifest.RetainedSources = cloneRetainedSources(record.Prepared.RetainedSources)
		for _, proof := range record.Prepared.WorkProofs {
			manifest.WorkProofs = append(manifest.WorkProofs, proof.Clone())
		}
	}
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	return manifest, nil
}

// Clone returns a detached active manifest.
func (m *ActiveManifest) Clone() *ActiveManifest {
	if m == nil {
		return nil
	}
	out := &ActiveManifest{
		Version:              m.Version,
		Generation:           m.Generation,
		Attempt:              m.Attempt,
		Assignments:          cloneAssignments(m.Assignments),
		Descriptors:          cloneNamedDescriptors(m.Descriptors),
		RetainedSources:      cloneRetainedSources(m.RetainedSources),
		ConfigDigest:         m.ConfigDigest,
		BindingConfigDigests: cloneBindingDigests(m.BindingConfigDigests),
		WitnessAlgorithm:     m.WitnessAlgorithm,
		CutoverGeneration:    m.CutoverGeneration,
		RollbackGeneration:   m.RollbackGeneration,
	}
	if m.Guards != nil {
		out.Guards = cloneGuardReceipts(m.Guards)
	}
	if m.Receipts != nil {
		out.Receipts = make([]ParticipantReceipt, 0, len(m.Receipts))
		for _, receipt := range m.Receipts {
			out.Receipts = append(out.Receipts, receipt.Clone())
		}
	}
	if m.WorkProofs != nil {
		out.WorkProofs = make([]WorkProof, 0, len(m.WorkProofs))
		for _, proof := range m.WorkProofs {
			out.WorkProofs = append(out.WorkProofs, proof.Clone())
		}
	}
	return out
}

// AssignmentMap returns the active class-to-binding map.
func (m *ActiveManifest) AssignmentMap() map[coordclass.Class]BindingName {
	return cloneAssignments(m.Assignments)
}

// Digest returns the canonical digest of the manifest's pinned identities. The
// attempt record pins it at ACTIVE_MANIFEST_DURABLE so a later phase cannot be
// satisfied by a different manifest.
func (m *ActiveManifest) Digest() (string, error) {
	var encoded canonicalDescriptorEncoding
	encoded.string("gascity.storage-active-manifest.v1")
	encoded.uint16(m.Version)
	encoded.uint64(uint64(m.Generation))
	encoded.string(string(m.Attempt))
	for _, class := range coordclass.Classes() {
		encoded.string(class.String())
		encoded.string(string(m.Assignments[class]))
	}
	descriptors := cloneNamedDescriptors(m.Descriptors)
	sort.SliceStable(descriptors, func(i, j int) bool { return descriptors[i].Name < descriptors[j].Name })
	encoded.uint64(uint64(len(descriptors)))
	for _, descriptor := range descriptors {
		identity, err := descriptor.Descriptor.Identity()
		if err != nil {
			return "", err
		}
		encoded.string(string(descriptor.Name))
		encoded.string(string(identity))
	}
	receipts := make([]string, 0, len(m.Receipts))
	for _, receipt := range m.Receipts {
		receipts = append(receipts, receipt.Participant+"\x00"+receipt.ReceiptID)
	}
	sort.Strings(receipts)
	encoded.uint64(uint64(len(receipts)))
	for _, receipt := range receipts {
		encoded.string(receipt)
	}
	guards := make([]string, 0, len(m.Guards))
	for _, guard := range m.Guards {
		guards = append(guards, string(guard.Component)+"\x00"+string(guard.PhysicalIdentity)+"\x00"+guard.ReceiptID)
	}
	sort.Strings(guards)
	encoded.uint64(uint64(len(guards)))
	for _, guard := range guards {
		encoded.string(guard)
	}
	encoded.string(string(m.ConfigDigest))
	encoded.string(m.WitnessAlgorithm)
	encoded.uint64(uint64(m.CutoverGeneration))
	encoded.uint64(uint64(m.RollbackGeneration))
	return canonicalDigest(encoded.bytes), nil
}

// Validate rejects an active manifest that cannot authorize an active open.
func (m *ActiveManifest) Validate() error {
	if m == nil {
		return fmt.Errorf("%w: nil active manifest", ErrInvalidManifest)
	}
	if m.Version != 1 {
		return fmt.Errorf("%w: active manifest has no version", ErrInvalidManifest)
	}
	if !m.Generation.Valid() {
		return fmt.Errorf("%w: active manifest has no generation", ErrInvalidManifest)
	}
	if strings.TrimSpace(string(m.Attempt)) == "" {
		return fmt.Errorf("%w: active manifest has no attempt identity", ErrInvalidManifest)
	}
	if err := validateSecretFree("attempt identity", string(m.Attempt)); err != nil {
		return err
	}
	if err := validateAssignments("active", m.Assignments); err != nil {
		return err
	}
	if err := ValidateNoDescriptorOverlap(m.Descriptors); err != nil {
		return err
	}
	byName := make(map[BindingName]struct{}, len(m.Descriptors))
	for _, descriptor := range m.Descriptors {
		if err := descriptor.Descriptor.Validate(); err != nil {
			return err
		}
		if _, exists := byName[descriptor.Name]; exists {
			return fmt.Errorf("%w: active manifest describes binding %q twice", ErrInvalidManifest, descriptor.Name)
		}
		byName[descriptor.Name] = struct{}{}
	}
	for _, class := range coordclass.Classes() {
		if _, described := byName[m.Assignments[class]]; !described {
			return fmt.Errorf("%w: active manifest assigns %s to %q with no active descriptor", ErrInvalidManifest, class, m.Assignments[class])
		}
	}
	if len(m.Receipts) == 0 {
		return fmt.Errorf("%w: active manifest carries no participant receipt", ErrInvalidManifest)
	}
	participants := make(map[string]struct{}, len(m.Receipts))
	for _, receipt := range m.Receipts {
		if err := receipt.Validate(); err != nil {
			return err
		}
		if receipt.Attempt != m.Attempt || receipt.Generation != m.Generation {
			return fmt.Errorf("%w: receipt for %q is bound to another attempt or generation", ErrInvalidManifest, receipt.Participant)
		}
		if _, exists := participants[receipt.Participant]; exists {
			return fmt.Errorf("%w: active manifest carries two receipts for %q", ErrInvalidManifest, receipt.Participant)
		}
		participants[receipt.Participant] = struct{}{}
	}
	for _, guard := range m.Guards {
		if err := guard.Validate(); err != nil {
			return err
		}
		if guard.Generation != m.Generation {
			return fmt.Errorf("%w: retained guard on %q enforces generation %d, manifest is %d", ErrInvalidManifest, guard.Component, guard.Generation, m.Generation)
		}
	}
	for _, source := range m.RetainedSources {
		if err := source.Validate(); err != nil {
			return err
		}
		if source.WitnessVersion != m.WitnessAlgorithm {
			return fmt.Errorf("%w: retained source %q pins witness algorithm %q, manifest uses %q", ErrInvalidManifest, source.Component, source.WitnessVersion, m.WitnessAlgorithm)
		}
	}
	for _, proof := range m.WorkProofs {
		if err := proof.Validate(); err != nil {
			return err
		}
		if proof.Attempt != m.Attempt || proof.Generation != m.Generation {
			return fmt.Errorf("%w: work proof is bound to another attempt or generation", ErrInvalidManifest)
		}
	}
	if err := validateCanonicalSHA256Digest("composite config digest", string(m.ConfigDigest)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidManifest, err)
	}
	if len(m.BindingConfigDigests) == 0 {
		return fmt.Errorf("%w: active manifest records no per-binding config digest", ErrInvalidManifest)
	}
	for name, digest := range m.BindingConfigDigests {
		if err := validateIdentifier("binding name", string(name)); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidManifest, err)
		}
		if err := validateCanonicalSHA256Digest("binding config digest", string(digest)); err != nil {
			return fmt.Errorf("%w: binding %q: %w", ErrInvalidManifest, name, err)
		}
	}
	if strings.TrimSpace(m.WitnessAlgorithm) == "" {
		return fmt.Errorf("%w: active manifest pins no witness algorithm version", ErrInvalidManifest)
	}
	if err := validateSecretFree("witness algorithm", m.WitnessAlgorithm); err != nil {
		return err
	}
	if m.CutoverGeneration != m.Generation {
		return fmt.Errorf("%w: cutover generation %d does not match the active generation %d", ErrInvalidManifest, m.CutoverGeneration, m.Generation)
	}
	if m.RollbackGeneration != 0 && m.RollbackGeneration >= m.Generation {
		return fmt.Errorf("%w: rollback generation %d does not precede the active generation %d", ErrInvalidManifest, m.RollbackGeneration, m.Generation)
	}
	return nil
}

// descriptorFor returns the active descriptor recorded for one binding.
func (m *ActiveManifest) descriptorFor(name BindingName) (Descriptor, bool) {
	for _, descriptor := range m.Descriptors {
		if descriptor.Name == name {
			return descriptor.Descriptor.Clone(), true
		}
	}
	return Descriptor{}, false
}

// retains reports whether an inspected binding is one this generation retained
// as a read-only source. A desired assignment that lands on a retained source is
// a reverse move, not a fresh relocation.
func (m *ActiveManifest) retains(inspection Inspection) bool {
	for _, source := range m.RetainedSources {
		if targetCoversComponent(inspection.Target, source.Component, source.PhysicalIdentity) {
			return true
		}
	}
	return false
}
