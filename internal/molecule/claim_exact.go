package molecule

import (
	"errors"
	"fmt"
	"math"
	"strconv"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// ErrClaimGenerationReserved is returned when onSuccess.Metadata passed to
// ClaimExact tries to set beadmeta.ClaimGenerationMetadataKey directly. That
// key is owned by ClaimExact's own generation fence; a caller-supplied value
// would desync it from the single revision-fenced UpdateIfMatch call that
// commits the generation bump and onSuccess's effects together.
var ErrClaimGenerationReserved = errors.New("claim exact: onSuccess.Metadata must not set the reserved claim generation key")

// ErrClaimExactReadback is returned when a claim's write committed but the
// confirmation read of the bead failed. The paired outcome is
// ClaimExactClaimed: an obligation was minted, so the caller must stop and
// reconcile that bead rather than drain as if nothing happened.
var ErrClaimExactReadback = errors.New("claim exact: committed write could not be re-read")

// ClaimExactPreconditions is the set of caller-observed facts a scheduler-bound
// launch path expects to still hold on the one bead it names. A nil field
// means "don't care"; a non-nil field (including a pointer to "") must equal
// the bead's current value exactly.
type ClaimExactPreconditions struct {
	Status            *string
	RoutedTo          *string
	RootBeadID        *string
	ContinuationGroup *string
	// Assignee fences on the bead's assignee column; a pointer to "" requires
	// the bead to be unassigned, which is what a fresh routed claim observed.
	Assignee *string
	// StepID fences on beadmeta.StepIDMetadataKey, the step identity inside
	// the workflow root the bead belongs to.
	StepID *string
}

// ClaimExactOutcome classifies how ClaimExact resolved. It is always paired
// with a nil error; a non-nil error means the call could not be confirmed as a
// clean win (unknown bead, store without conditional-write support, an
// unsupported onSuccess field) and the outcome is "". On error the returned
// bead is the freshest snapshot this call held: a re-Get after a failed
// write, or the pre-write Get snapshot. It is the zero Bead only when the
// reserved-key guard rejected the call before any read, or when the initial
// Get itself failed — check for that before inspecting it.
type ClaimExactOutcome string

const (
	// ClaimExactClaimed means the preconditions matched and a single
	// revision-fenced UpdateIfMatch atomically advanced the generation
	// transition and applied onSuccess together; no other write could have
	// interleaved between the two.
	ClaimExactClaimed ClaimExactOutcome = "claimed"
	// ClaimExactPreconditionFailed means the bead's current status, routing,
	// or workflow root/group identity did not match want. No write was
	// attempted.
	ClaimExactPreconditionFailed ClaimExactOutcome = "precondition_failed"
	// ClaimExactStale means the bead's claim generation was not exactly
	// fromGeneration (checked before the write), or the bead's revision had
	// moved by the time the single UpdateIfMatch call ran, so this call did
	// not win the transition and onSuccess was never applied — including
	// when the current generation already equals what this call would have
	// advanced to. That case is deliberately NOT treated as an idempotent success:
	// this primitive cannot tell "I already won this" from "a different
	// caller landed on the same next value", so it fails closed either way.
	// Stale is a definitive statement that this call's write did NOT apply,
	// not a conclusion that another claimant won: per the ConditionalWriter
	// revision contract, a derived-column rewrite driven by an unrelated
	// bead's write may bump the revision on some backends. Callers must
	// treat Stale as a re-read trigger and may need a bounded retry.
	// A caller that needs crash-retry idempotency must inspect the returned
	// bead (e.g. compare its own execution identity against what's there)
	// rather than rely on ClaimExact to re-apply effects silently.
	ClaimExactStale ClaimExactOutcome = "stale"
	// ClaimExactObservationMoved means the assignment-tier write landed, but
	// the confirmation read shows the bead is no longer the one this call
	// observed: its step/root/route/group identity moved, or its claim
	// generation was not the one this call wrote. The write committed, so the
	// caller now owns the bead and must give it back (release it) before
	// re-deciding; this outcome never appears from the revision tier, whose
	// single fenced write cannot commit onto a row that changed underneath it.
	ClaimExactObservationMoved ClaimExactOutcome = "observation_moved"
)

// ClaimExactFence names which conditional-write tier committed a claim: the
// bead-revision CAS, or the status+assignee guard. It is set on every outcome
// a tier produced (claimed, stale, observation_moved) and empty on
// precondition failures and errors.
type ClaimExactFence string

const (
	// ClaimExactFenceRevision marks a claim committed through the
	// bead-revision CAS.
	ClaimExactFenceRevision ClaimExactFence = "revision"
	// ClaimExactFenceAssignment marks a claim committed through the
	// status+assignee guard.
	ClaimExactFenceAssignment ClaimExactFence = "assignment"
)

// ClaimExactReader is the read side an exact claim needs: one Get of the
// bead it is about to fence. Any store (or store adapter) with a Get
// satisfies it, so callers inject the store their claim must run against
// without dragging a whole interface along.
type ClaimExactReader interface {
	Get(id string) (beads.Bead, error)
}

// ClaimExactWriters are the two conditional-write tiers an exact claim can
// commit through; either may be nil. The revision tier
// (beads.ConditionalWriter) is tried first; the assignment tier
// (beads.AssignmentGuardedUpdater) is used when the revision tier reports the
// capability missing for this store instance. With neither tier usable the
// claim fails closed with beads.ErrConditionalWriteUnsupported — there is no
// unconditional fallback.
type ClaimExactWriters struct {
	Revision   beads.ConditionalWriter
	Assignment beads.AssignmentGuardedUpdater
}

// ClaimExactRequest is one exact claim: the bead ID, the caller-observed
// facts that must still hold (Want), the claim generation the caller observed
// (FromGeneration), and the effects to apply on success (OnSuccess).
//
// A nil FromGeneration means the caller never observed a generation: the
// claim advances whatever counter the row carries (never resets it) and is
// fenced by the preconditions and the write tier alone. A non-nil
// FromGeneration must still equal the row's current generation or the claim
// is stale before any write.
type ClaimExactRequest struct {
	ID             string
	Want           ClaimExactPreconditions
	FromGeneration *string
	OnSuccess      beads.UpdateOpts
}

// ClaimExactResult is how ClaimExactWithWriters resolved. Bead is the freshest
// snapshot this call held: the confirmation read on success, the moved
// snapshot on observation_moved, and the pre-write Get (or a best-effort
// re-read) otherwise.
type ClaimExactResult struct {
	Bead    beads.Bead
	Outcome ClaimExactOutcome
	Fence   ClaimExactFence
}

// ClaimExact claims the single bead id only when want's fields match the
// bead's current status/routing/root/group and the bead's current
// beadmeta.ClaimGenerationMetadataKey equals fromGeneration. On a match it
// atomically advances the generation key by one AND applies onSuccess
// (typically execution identity: Assignee, DirectSessionID/SessionName
// metadata, Status) in a single revision-fenced write.
//
// It is the strict shape of ClaimExactWithWriters over the store's own
// conditional writer: the assignment tier is never used, and a store that
// exposes no beads.ConditionalWriter (or disables it for this instance) fails
// closed with beads.ErrConditionalWriteUnsupported rather than downgrading to
// an unconditional write.
//
// This is the scheduler-owned counterpart to the generic gc hook --claim pool
// path: it never falls back to an unconditional write when the store lacks
// conditional-write support, and it never retries against a different bead.
// A stale or conflicting caller gets ClaimExactStale (or an error, for a store
// that cannot conditionally write at all) and must re-read and re-decide;
// ClaimExact will not silently hand it a different ready bead the way a
// generic pool claim would.
//
// onSuccess.Metadata must not set beadmeta.ClaimGenerationMetadataKey; doing
// so returns ErrClaimGenerationReserved without writing anything.
func ClaimExact(store beads.MoleculeTarget, id string, want ClaimExactPreconditions, fromGeneration string, onSuccess beads.UpdateOpts) (beads.Bead, ClaimExactOutcome, error) {
	writer, _ := beads.As[beads.ConditionalWriter](store)
	res, err := ClaimExactWithWriters(store, ClaimExactWriters{Revision: writer}, ClaimExactRequest{
		ID:             id,
		Want:           want,
		FromGeneration: &fromGeneration,
		OnSuccess:      onSuccess,
	})
	return res.Bead, res.Outcome, err
}

// ClaimExactWithWriters claims the single bead named by req.ID, fenced to the
// caller-observed facts in req.Want, committing through the first usable tier
// in writers:
//
//   - The revision tier issues ONE beads.ConditionalWriter.UpdateIfMatch call,
//     fenced on the bead revision this call's own Get observed, folding the
//     generation bump and req.OnSuccess together. A revision precondition
//     failure is ClaimExactStale; ErrConditionalWriteUnsupported drops to the
//     assignment tier rather than failing.
//   - The assignment tier requires Want.Status and Want.Assignee (the observed
//     row the guard can check) and issues ONE
//     beads.AssignmentGuardedUpdater.UpdateIfAssignment guarded on exactly
//     those facts. A guard rejection is ClaimExactStale with nothing written.
//     Because that guard cannot see the row's revision, a write that lands is
//     confirmed by re-reading: identity preconditions or a generation that no
//     longer match make the outcome ClaimExactObservationMoved — the write
//     committed, so the caller owns the bead and must give it back.
//
// With neither tier usable the claim fails closed with
// beads.ErrConditionalWriteUnsupported and writes nothing — there is no
// unconditional fallback on any path.
//
// A write that commits but cannot be re-read returns ClaimExactClaimed paired
// with an error wrapping ErrClaimExactReadback: the obligation exists, so the
// caller must stop and reconcile rather than drain.
//
// req.OnSuccess.Metadata must not set beadmeta.ClaimGenerationMetadataKey;
// doing so returns ErrClaimGenerationReserved without writing anything.
func ClaimExactWithWriters(reader ClaimExactReader, writers ClaimExactWriters, req ClaimExactRequest) (ClaimExactResult, error) {
	id := req.ID
	if _, reserved := req.OnSuccess.Metadata[beadmeta.ClaimGenerationMetadataKey]; reserved {
		return ClaimExactResult{}, fmt.Errorf("claim exact %q: %w", id, ErrClaimGenerationReserved)
	}

	b, err := reader.Get(id)
	if err != nil {
		return ClaimExactResult{}, fmt.Errorf("claim exact %q: %w", id, err)
	}

	if !claimExactPreconditionsMatch(b, req.Want) {
		return ClaimExactResult{Bead: b, Outcome: ClaimExactPreconditionFailed}, nil
	}

	// Resolve the generation this claim fences on and advances from. A nil
	// FromGeneration trusts this call's own Get: whatever counter the row
	// carries is the one advanced, so a caller that observed no generation
	// cannot reset one.
	from := b.Metadata[beadmeta.ClaimGenerationMetadataKey]
	if req.FromGeneration != nil {
		from = *req.FromGeneration
	}
	if b.Metadata[beadmeta.ClaimGenerationMetadataKey] != from {
		return ClaimExactResult{Bead: b, Outcome: ClaimExactStale}, nil
	}

	toGeneration, err := nextClaimGeneration(from)
	if err != nil {
		return ClaimExactResult{Bead: b}, fmt.Errorf("claim exact %q: %w", id, err)
	}
	effects := claimEffectsWithGeneration(req.OnSuccess, toGeneration)

	var revisionUnsupported bool
	if writers.Revision != nil {
		res, unsupported, tierErr := claimExactRevisionTier(reader, writers.Revision, id, b, effects)
		if !unsupported {
			return res, tierErr
		}
		// The store instance cannot revision-fence this write. Fall through
		// to the assignment tier rather than failing: the guard fences the
		// same observed row on the facts it can see.
		revisionUnsupported = true
	}
	if writers.Assignment != nil {
		res, err := claimExactAssignmentTier(reader, writers.Assignment, req, id, b, toGeneration, effects)
		if err != nil && revisionUnsupported && beads.IsConditionalWriteUnsupported(err) {
			// Both fences refused the claim. Name the two capabilities that
			// were missing so the skip line a hook prints says what to fix,
			// instead of a bare sentinel the operator has to decode.
			return ClaimExactResult{Bead: b}, fmt.Errorf("%w: the revision CAS and the status+assignee guard are both unavailable on this store", err)
		}
		return res, err
	}
	if revisionUnsupported {
		return ClaimExactResult{Bead: b}, fmt.Errorf("claim exact %q: %w: the revision CAS is unavailable and this claim was given no status+assignee guard writer", id, beads.ErrConditionalWriteUnsupported)
	}
	return ClaimExactResult{Bead: b}, fmt.Errorf("claim exact %q: %w: no conditional writer was given", id, beads.ErrConditionalWriteUnsupported)
}

// claimExactRevisionTier commits through the bead-revision CAS. unsupported
// reports that the tier is unusable for this store instance (the caller
// drops to the assignment tier); every other return is final for the claim.
func claimExactRevisionTier(reader ClaimExactReader, writer beads.ConditionalWriter, id string, b beads.Bead, effects beads.UpdateOpts) (ClaimExactResult, bool, error) {
	if err := writer.UpdateIfMatch(id, b.Revision, effects); err != nil {
		if beads.IsConditionalWriteUnsupported(err) {
			return ClaimExactResult{}, true, nil
		}
		if beads.IsPreconditionFailed(err) {
			return ClaimExactResult{Bead: claimExactBestEffortGet(reader, id, b), Outcome: ClaimExactStale, Fence: ClaimExactFenceRevision}, false, nil
		}
		return ClaimExactResult{Bead: claimExactBestEffortGet(reader, id, b)}, false, fmt.Errorf("claim exact %q: applying claim effects: %w", id, err)
	}
	final, err := reader.Get(id)
	if err != nil {
		return ClaimExactResult{Bead: b, Outcome: ClaimExactClaimed, Fence: ClaimExactFenceRevision}, false, fmt.Errorf("claim exact %q: re-read after claim: %w: %w", id, ErrClaimExactReadback, err)
	}
	return ClaimExactResult{Bead: final, Outcome: ClaimExactClaimed, Fence: ClaimExactFenceRevision}, false, nil
}

// claimExactAssignmentTier commits through the status+assignee guard and
// confirms the landing by re-reading: the guard cannot see the revision, so
// only the readback can catch a row whose identity moved between this call's
// Get and the guarded write.
func claimExactAssignmentTier(reader ClaimExactReader, writer beads.AssignmentGuardedUpdater, req ClaimExactRequest, id string, b beads.Bead, toGeneration string, effects beads.UpdateOpts) (ClaimExactResult, error) {
	if req.Want.Status == nil || req.Want.Assignee == nil {
		return ClaimExactResult{Bead: b}, fmt.Errorf("claim exact %q: assignment fence requires Want.Status and Want.Assignee (the observed row the guard checks), got status=%v assignee=%v", id, req.Want.Status, req.Want.Assignee)
	}
	landed, err := writer.UpdateIfAssignment(id, *req.Want.Status, *req.Want.Assignee, effects)
	if err != nil {
		if beads.IsConditionalWriteUnsupported(err) {
			// Wrap the store's own refusal (it carries the sentinel) so the
			// caller, which knows whether the revision tier also refused,
			// can name the missing capabilities once without duplicating the
			// claim's ID context.
			return ClaimExactResult{Bead: b}, fmt.Errorf("claim exact %q: %w", id, err)
		}
		return ClaimExactResult{Bead: claimExactBestEffortGet(reader, id, b)}, fmt.Errorf("claim exact %q: applying claim effects: %w", id, err)
	}
	if !landed {
		return ClaimExactResult{Bead: claimExactBestEffortGet(reader, id, b), Outcome: ClaimExactStale, Fence: ClaimExactFenceAssignment}, nil
	}
	final, err := reader.Get(id)
	if err != nil {
		return ClaimExactResult{Bead: b, Outcome: ClaimExactClaimed, Fence: ClaimExactFenceAssignment}, fmt.Errorf("claim exact %q: re-read after claim: %w: %w", id, ErrClaimExactReadback, err)
	}
	if claimExactIdentityMoved(final, req.Want) || final.Metadata[beadmeta.ClaimGenerationMetadataKey] != toGeneration {
		return ClaimExactResult{Bead: final, Outcome: ClaimExactObservationMoved, Fence: ClaimExactFenceAssignment}, nil
	}
	return ClaimExactResult{Bead: final, Outcome: ClaimExactClaimed, Fence: ClaimExactFenceAssignment}, nil
}

// claimExactIdentityMoved reports whether the bead's workflow identity — the
// fields a guarded write cannot fence — differs from the caller's observed
// facts. Status and assignee are deliberately absent: the claim's own effects
// legitimately change them.
func claimExactIdentityMoved(b beads.Bead, want ClaimExactPreconditions) bool {
	if want.RoutedTo != nil && b.Metadata[beadmeta.RoutedToMetadataKey] != *want.RoutedTo {
		return true
	}
	if want.RootBeadID != nil && b.Metadata[beadmeta.RootBeadIDMetadataKey] != *want.RootBeadID {
		return true
	}
	if want.ContinuationGroup != nil && b.Metadata[beadmeta.ContinuationGroupMetadataKey] != *want.ContinuationGroup {
		return true
	}
	if want.StepID != nil && b.Metadata[beadmeta.StepIDMetadataKey] != *want.StepID {
		return true
	}
	return false
}

// claimEffectsWithGeneration returns a copy of onSuccess with
// beadmeta.ClaimGenerationMetadataKey added to its Metadata, so the
// generation fence and the caller's effects travel in one UpdateIfMatch call.
// The caller's onSuccess.Metadata is never mutated.
func claimEffectsWithGeneration(onSuccess beads.UpdateOpts, toGeneration string) beads.UpdateOpts {
	metadata := make(map[string]string, len(onSuccess.Metadata)+1)
	for k, v := range onSuccess.Metadata {
		metadata[k] = v
	}
	metadata[beadmeta.ClaimGenerationMetadataKey] = toGeneration
	effects := onSuccess
	effects.Metadata = metadata
	return effects
}

// bestEffortGet re-reads id and returns the fresh snapshot, falling back to
// fallback if the re-read itself fails. It is only used on error paths where
// the caller already has a reasonably fresh bead in hand and a failed re-read
// should not turn into an empty, uninformative zero value.
func claimExactBestEffortGet(reader ClaimExactReader, id string, fallback beads.Bead) beads.Bead {
	if fresh, err := reader.Get(id); err == nil {
		return fresh
	}
	return fallback
}

func claimExactPreconditionsMatch(b beads.Bead, want ClaimExactPreconditions) bool {
	if want.Status != nil && b.Status != *want.Status {
		return false
	}
	if want.RoutedTo != nil && b.Metadata[beadmeta.RoutedToMetadataKey] != *want.RoutedTo {
		return false
	}
	if want.RootBeadID != nil && b.Metadata[beadmeta.RootBeadIDMetadataKey] != *want.RootBeadID {
		return false
	}
	if want.ContinuationGroup != nil && b.Metadata[beadmeta.ContinuationGroupMetadataKey] != *want.ContinuationGroup {
		return false
	}
	if want.Assignee != nil && b.Assignee != *want.Assignee {
		return false
	}
	if want.StepID != nil && b.Metadata[beadmeta.StepIDMetadataKey] != *want.StepID {
		return false
	}
	return true
}

// nextClaimGeneration advances a beadmeta.ClaimGenerationMetadataKey value by
// one. The empty string (a bead never claimed through this primitive) is
// generation 0, so its next value is "1". A present-but-unparseable value, one
// that is not a positive counter (this primitive never produces zero or
// negative generations), or math.MaxInt64 (n+1 would silently overflow to a
// negative value) fails closed rather than guessing a restart point or
// writing a corrupt generation.
func nextClaimGeneration(current string) (string, error) {
	if current == "" {
		return "1", nil
	}
	n, err := strconv.ParseInt(current, 10, 64)
	if err != nil {
		return "", fmt.Errorf("claim generation %q is not a decimal counter: %w", current, err)
	}
	if n <= 0 {
		return "", fmt.Errorf("claim generation %q is not a positive counter", current)
	}
	if n == math.MaxInt64 {
		return "", fmt.Errorf("claim generation %q is at the int64 ceiling and cannot be advanced", current)
	}
	return strconv.FormatInt(n+1, 10), nil
}
