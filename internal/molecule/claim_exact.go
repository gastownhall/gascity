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
// key is owned by ClaimExact's own CAS; a caller-supplied value would desync
// the fence from the effects write that is supposed to follow it.
var ErrClaimGenerationReserved = errors.New("claim exact: onSuccess.Metadata must not set the reserved claim generation key")

// ClaimExactPreconditions is the set of caller-observed facts a scheduler-bound
// launch path expects to still hold on the one bead it names. A nil field
// means "don't care"; a non-nil field (including a pointer to "") must equal
// the bead's current value exactly.
type ClaimExactPreconditions struct {
	Status            *string
	RoutedTo          *string
	RootBeadID        *string
	ContinuationGroup *string
}

// ClaimExactOutcome classifies how ClaimExact resolved. It is always paired
// with a nil error; a non-nil error means the call could not be confirmed as a
// clean win (unknown bead, store without conditional-write support, an
// unsupported onSuccess field) and the outcome is "". On error the returned
// bead is still the best available snapshot (a best-effort re-Get), not
// necessarily empty — inspect it before deciding whether to retry.
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
	// fromGeneration when the CAS ran, so this call did not win the
	// transition and onSuccess was never applied — including when the
	// current generation already equals what this call would have advanced
	// to. That case is deliberately NOT treated as an idempotent success:
	// this primitive cannot tell "I already won this" from "a different
	// caller landed on the same next value", so it fails closed either way.
	// A caller that needs crash-retry idempotency must inspect the returned
	// bead (e.g. compare its own execution identity against what's there)
	// rather than rely on ClaimExact to re-apply effects silently.
	ClaimExactStale ClaimExactOutcome = "stale"
)

// ClaimExact claims the single bead id only when want's fields match the
// bead's current status/routing/root/group and the bead's current
// beadmeta.ClaimGenerationMetadataKey equals fromGeneration. On a match it
// atomically advances the generation key by one AND applies onSuccess
// (typically execution identity: Assignee, DirectSessionID/SessionName
// metadata, Status) in a single revision-fenced write.
//
// The generation bump and onSuccess are folded into one
// beads.ConditionalWriter.UpdateIfMatch call, fenced on the bead's revision as
// observed by this call's own Get. UpdateIfMatch applies opts only if the
// bead's revision still equals that snapshot; otherwise the whole write is
// rejected atomically (*beads.PreconditionFailedError), so there is no window
// in which a second claimant's write can land between the generation CAS and
// the effects write the way a two-step CAS-then-Update could race. A store
// that exposes no beads.ConditionalWriter (beads.ConditionalWriterFor returns
// ok=false), or one whose ConditionalWriter is disabled for this instance,
// fails closed with beads.ErrConditionalWriteUnsupported rather than
// downgrading to an unconditional write.
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
func ClaimExact(store beads.Store, id string, want ClaimExactPreconditions, fromGeneration string, onSuccess beads.UpdateOpts) (beads.Bead, ClaimExactOutcome, error) {
	if _, reserved := onSuccess.Metadata[beadmeta.ClaimGenerationMetadataKey]; reserved {
		return beads.Bead{}, "", fmt.Errorf("claim exact %q: %w", id, ErrClaimGenerationReserved)
	}

	b, err := store.Get(id)
	if err != nil {
		return beads.Bead{}, "", fmt.Errorf("claim exact %q: %w", id, err)
	}

	if !claimExactPreconditionsMatch(b, want) {
		return b, ClaimExactPreconditionFailed, nil
	}

	if b.Metadata[beadmeta.ClaimGenerationMetadataKey] != fromGeneration {
		return b, ClaimExactStale, nil
	}

	toGeneration, err := nextClaimGeneration(fromGeneration)
	if err != nil {
		return b, "", fmt.Errorf("claim exact %q: %w", id, err)
	}

	writer, ok := beads.ConditionalWriterFor(store)
	if !ok {
		return b, "", fmt.Errorf("claim exact %q: %w", id, beads.ErrConditionalWriteUnsupported)
	}

	effects := claimEffectsWithGeneration(onSuccess, toGeneration)
	if err := writer.UpdateIfMatch(id, b.Revision, effects); err != nil {
		if beads.IsPreconditionFailed(err) {
			return bestEffortGet(store, id, b), ClaimExactStale, nil
		}
		return bestEffortGet(store, id, b), "", fmt.Errorf("claim exact %q: applying claim effects: %w", id, err)
	}

	final, err := store.Get(id)
	if err != nil {
		return b, "", fmt.Errorf("claim exact %q: re-read after claim: %w", id, err)
	}
	return final, ClaimExactClaimed, nil
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
func bestEffortGet(store beads.Store, id string, fallback beads.Bead) beads.Bead {
	if fresh, err := store.Get(id); err == nil {
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
