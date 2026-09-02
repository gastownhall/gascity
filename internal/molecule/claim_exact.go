package molecule

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// ErrClaimGenerationReserved is returned when onSuccess.Metadata passed to
// ClaimExact tries to set beadmeta.ClaimGenerationMetadataKey directly. That
// key is owned by ClaimExact's own CAS; a caller-supplied value would desync
// the fence from the effects write that is supposed to follow it.
var ErrClaimGenerationReserved = errors.New("claim exact: onSuccess.Metadata must not set the reserved claim generation key")

// ErrClaimEffectsRaced is returned when ClaimExact's generation CAS won, but
// by the time onSuccess had been written and the bead re-read, the generation
// key no longer read back as toGeneration. That means a second caller (one
// that independently observed the same just-advanced generation and called
// ClaimExact with it as its own fromGeneration) also won a CAS in the window
// between this call's CAS and its effects write, and applied its own
// unconditional onSuccess write. Store.Update has no compare-and-set of its
// own, so this call cannot tell whether its effects landed and were then
// overwritten, or never landed at all — either way it must not report
// ClaimExactClaimed. The caller must re-Get the bead and treat its own
// effects as unconfirmed rather than retry blindly.
var ErrClaimEffectsRaced = errors.New("claim exact: effects write raced with a concurrent generation advance")

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
// clean win (unknown bead, store without CAS support, effects raced with a
// concurrent claimant) and the outcome is "". On error the returned bead is
// still the best available snapshot (a best-effort re-Get), not necessarily
// empty — inspect it before deciding whether to retry.
type ClaimExactOutcome string

const (
	// ClaimExactClaimed means the preconditions matched, this call's CAS won
	// the generation transition, onSuccess was applied, and a re-read
	// confirmed the generation still reads as this call's toGeneration (no
	// other claimant advanced it again before that read).
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
// atomically advances the generation key by one, then applies onSuccess
// (typically execution identity: Assignee, DirectSessionID/SessionName
// metadata, Status) as a separate write.
//
// Only the generation-key advance is atomic (a single MetadataCASWriter CAS).
// Applying onSuccess is a second, unconditional beads.Store.Update — the store
// exposes no compound compare-and-set that could cover both in one operation
// (see internal/beads/metadata_cas.go's file doc for why). ClaimExact narrows
// but does not close the resulting window: after the CAS wins, it re-reads the
// bead and confirms the generation still reads as toGeneration before
// reporting ClaimExactClaimed. This catches the case where a second, later
// claimant's CAS-and-effects both completed strictly after this call's
// re-read window opened (that claimant correctly gets ClaimExactClaimed and
// this call gets ErrClaimEffectsRaced instead). It cannot prevent a second
// claimant's CAS landing between this call's CAS and its own Update, and that
// claimant's Update completing before this call's Update does — in that
// ordering, this call's stale onSuccess can still overwrite the second
// claimant's effects even though the second claimant's own re-read shows it
// "won". A caller that must have a compound atomic guarantee needs a store
// capability this codebase does not yet have; until then, treat
// ErrClaimEffectsRaced as this primitive's only signal that its own effects
// may not be authoritative and re-Get to check.
//
// This is the scheduler-owned counterpart to the generic gc hook --claim pool
// path: it never falls back to an unconditional write when the store lacks
// conditional-write support, and it never retries against a different bead.
// A stale or conflicting caller gets ClaimExactStale (or an error, for a store
// that cannot CAS at all) and must re-read and re-decide; ClaimExact will not
// silently hand it a different ready bead the way a generic pool claim would.
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

	outcome, err := beads.ApplyMetadataCAS(store, id, beadmeta.ClaimGenerationMetadataKey, fromGeneration, toGeneration)
	if err != nil {
		return bestEffortGet(store, id, b), "", fmt.Errorf("claim exact %q: %w", id, err)
	}
	if outcome != beads.MetadataCASSwapped {
		return bestEffortGet(store, id, b), ClaimExactStale, nil
	}

	if err := store.Update(id, onSuccess); err != nil {
		return bestEffortGet(store, id, b), "", fmt.Errorf("claim exact %q: applying claim effects: %w", id, err)
	}

	final, err := store.Get(id)
	if err != nil {
		return b, "", fmt.Errorf("claim exact %q: re-read after claim: %w", id, err)
	}
	if final.Metadata[beadmeta.ClaimGenerationMetadataKey] != toGeneration {
		return final, "", fmt.Errorf("claim exact %q: %w (generation now %q, wanted %q)", id, ErrClaimEffectsRaced, final.Metadata[beadmeta.ClaimGenerationMetadataKey], toGeneration)
	}
	return final, ClaimExactClaimed, nil
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
// generation 0, so its next value is "1". A present-but-unparseable value, or
// one that is not a positive counter (this primitive never produces zero or
// negative generations), fails closed rather than guessing a restart point.
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
	return strconv.FormatInt(n+1, 10), nil
}
