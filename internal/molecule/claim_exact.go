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
// would desync it from the one fenced write (UpdateIfMatch, or
// UpdateIfAssignment on ClaimExactWithWriters' assignment tier) that commits
// the generation bump and onSuccess's effects together.
var ErrClaimGenerationReserved = errors.New("claim exact: onSuccess.Metadata must not set the reserved claim generation key")

// ClaimExactPreconditions is the set of caller-observed facts a scheduler-bound
// launch path expects to still hold on the one bead it names. A nil field
// means "don't care"; a non-nil field (including a pointer to "") must equal
// the bead's current value exactly.
type ClaimExactPreconditions struct {
	Status            *string
	Assignee          *string
	RoutedTo          *string
	RootBeadID        *string
	StepID            *string
	ContinuationGroup *string
}

// ClaimExactOutcome classifies how ClaimExact resolved. It is always paired
// with a nil error; a non-nil error means the call could not be confirmed as a
// clean win (unknown bead, store without conditional-write support, an
// unsupported onSuccess field, a write that did not land or could not be
// settled) and the outcome is "". ClaimExactWithWriters adds one exception: a
// landed claim whose re-read failed (ErrClaimExactReadback). On error the
// returned bead is the freshest snapshot this call held: a re-Get after a
// failed write, or the pre-write Get snapshot. It is the zero Bead only when
// the reserved-key guard rejected the call before any read, or when the
// initial Get itself failed — check for that before inspecting it.
type ClaimExactOutcome string

const (
	// ClaimExactClaimed means the preconditions matched, one fenced write
	// advanced the generation and applied onSuccess together, and the
	// readback still carries the observation the claim was decided on. On
	// the revision tier no other write could have interleaved between this
	// call's read and its write; the assignment tier fences only the status
	// and assignee inside its write (ClaimExactWithWriters names its residue).
	ClaimExactClaimed ClaimExactOutcome = "claimed"
	// ClaimExactPreconditionFailed means the bead's current status, assignee,
	// routing, or workflow root/step/group identity did not match want. No
	// write was attempted.
	ClaimExactPreconditionFailed ClaimExactOutcome = "precondition_failed"
	// ClaimExactStale means the bead's claim generation was not exactly
	// fromGeneration (checked before the write), or the write's fence no
	// longer held when it ran (the bead's revision had moved, or on the
	// assignment tier its status or assignee had), so this call did not win
	// the transition and onSuccess was never applied — including
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
	// ClaimExactObservationMoved means the claim's write landed, on either
	// tier, so the caller now holds the bead, but its readback no longer
	// carries the observation as the write left it: a step, root, route or
	// group key differs from want (or from the value onSuccess wrote to it),
	// or the claim generation is not the one this call wrote. The bead the
	// claim was decided on moved under it. The caller must give the bead
	// back.
	ClaimExactObservationMoved ClaimExactOutcome = "observation_moved"
)

// ClaimExactReader is the read half of an exact claim: the one-bead Get the
// preconditions and the claim generation are checked against.
type ClaimExactReader interface {
	Get(id string) (beads.Bead, error)
}

// ClaimExactWriters is the write half of an exact claim, kept apart from the
// reader so a caller whose read and write handles differ (a work leg's bd
// context, a relocated class binding) can hand each in as it holds it.
//
// Revision is the revision fence, tried first. Assignment is the
// status-and-assignee guard the claim falls to when Revision is nil or answers
// beads.ErrConditionalWriteUnsupported. A nil field means the caller has no
// such fence to offer; with both nil the claim fails closed.
type ClaimExactWriters struct {
	Revision   beads.ConditionalWriter
	Assignment beads.AssignmentGuardedUpdater
}

// ClaimExactRequest names the one bead an exact claim targets and what the
// caller observed on it.
//
// FromGeneration is the beadmeta.ClaimGenerationMetadataKey value the caller
// observed. Nil means the caller did not observe the key (a work-query
// projection that omits it); the generation this call reads is then the one
// it advances, and that advance is still committed by the same fenced write.
// OnSuccess is applied together with the generation advance and must not set
// the generation key itself.
type ClaimExactRequest struct {
	ID             string
	Want           ClaimExactPreconditions
	FromGeneration *string
	OnSuccess      beads.UpdateOpts
}

// ClaimExactFence names the write fence that committed a claim.
type ClaimExactFence string

const (
	// ClaimExactFenceRevision means the claim landed through one
	// beads.ConditionalWriter.UpdateIfMatch fenced on the revision this call
	// read.
	ClaimExactFenceRevision ClaimExactFence = "revision"
	// ClaimExactFenceAssignment means the claim landed through one
	// beads.AssignmentGuardedUpdater.UpdateIfAssignment guarded on the
	// observed status and assignee.
	ClaimExactFenceAssignment ClaimExactFence = "assignment"
)

// ErrClaimExactReadback is wrapped by the error of a claim whose write landed
// but whose re-read failed. Such a result carries Outcome ClaimExactClaimed,
// so a caller can tell a committed claim it could not confirm from a claim
// that wrote nothing.
var ErrClaimExactReadback = errors.New("claim exact: the claim landed but its re-read failed")

// ErrClaimExactUnsettled is wrapped by the error of a claim whose write
// answered neither success nor a precondition (an i/o timeout, say, which can
// follow a commit) and whose re-read, the one way to settle it, failed too.
// The claim may have landed, so a caller must not take the bead for one that
// wrote nothing and claim other work in its place. Such a result carries no
// Outcome.
var ErrClaimExactUnsettled = errors.New("claim exact: the claim's write failed without an answer and its re-read failed, so the claim may have landed")

// ClaimExactResult is how ClaimExactWithWriters resolved: the freshest bead
// snapshot the call held, the outcome, and the fence that committed the
// claim's write (empty unless Outcome is ClaimExactClaimed or
// ClaimExactObservationMoved).
type ClaimExactResult struct {
	Bead    beads.Bead
	Outcome ClaimExactOutcome
	Fence   ClaimExactFence
}

// ClaimExact claims the single bead id only when want's fields match the
// bead's current status/assignee/routing/root/step/group and the bead's current
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
// A landed claim is re-read, and a readback that no longer carries want as
// the write left it is ClaimExactObservationMoved: the caller holds the bead
// and must give it back. A write that answered an error other than a
// precondition is settled by that same re-read (ClaimExactWithWriters).
//
// onSuccess.Metadata must not set beadmeta.ClaimGenerationMetadataKey; doing
// so returns ErrClaimGenerationReserved without writing anything.
func ClaimExact(store beads.MoleculeTarget, id string, want ClaimExactPreconditions, fromGeneration string, onSuccess beads.UpdateOpts) (beads.Bead, ClaimExactOutcome, error) {
	var writers ClaimExactWriters
	if writer, ok := beads.As[beads.ConditionalWriter](store); ok {
		writers.Revision = writer
	}
	res, err := ClaimExactWithWriters(store, writers, ClaimExactRequest{
		ID:             id,
		Want:           want,
		FromGeneration: &fromGeneration,
		OnSuccess:      onSuccess,
	})
	if err != nil {
		return res.Bead, "", err
	}
	return res.Bead, res.Outcome, nil
}

// ClaimExactWithWriters is ClaimExact over a separate reader and writer set,
// taking the observation as a ClaimExactRequest.
//
// The flow is ClaimExact's: the reserved-key guard, one Get, the
// preconditions, the generation check (skipped when req.FromGeneration is
// nil), then one write that advances the generation and applies
// req.OnSuccess together. The write goes to the first of two tiers the
// writers offer:
//
//   - Revision tier: a revision-fenced UpdateIfMatch. Every precondition is
//     fenced inside the write, because any change to the bead after the Get
//     moves its revision. *beads.PreconditionFailedError is ClaimExactStale;
//     beads.ErrConditionalWriteUnsupported wrote nothing and drops the call
//     to the assignment tier.
//   - Assignment tier: one UpdateIfAssignment guarded on req.Want's Status
//     and Assignee, which must both be set. Status and assignee, the facts
//     that define ownership, are fenced inside the write, so two claimants
//     cannot both win; the other preconditions and the generation are checked
//     at read time and again on the readback. A guard that no longer holds is
//     ClaimExactStale. Its residue is ABA: another actor claiming and
//     releasing the bead between this call's read and its write, which can
//     leave two claims carrying the same generation.
//
// A write that landed is re-read on either tier, and the readback is the
// result: ClaimExactClaimed while it carries req.Want's routing, root, step
// and group keys and the generation the write advanced to, each as the write
// left it, and ClaimExactObservationMoved once it does not. The readback is
// the snapshot the caller acts on, so the revision tier is held to it too:
// its fence proves the observation held when the write ran, not when the
// readback was taken.
//
// A write that answers any other error is settled by one re-read, never by
// the error alone: an ambiguous transport error, such as an i/o timeout, can
// follow a commit, and the stores do not replay such a write. A row that
// carries the write (the status it set, its assignee or the observed one it
// kept, and the generation it advanced to) is the claim landing, and its
// readback is classified as above. A row without it means the write did not
// land, and its error stands. A row another caller brought to that same
// status, assignee and generation reads as this claim's own; it holds the
// bead under the owner this claim named.
//
// With neither tier available the call writes nothing and returns an error
// wrapping beads.ErrConditionalWriteUnsupported: there is no unconditional
// path.
//
// A non-nil error leaves Outcome empty, except for a landed write whose
// re-read failed: that returns ClaimExactClaimed with an error wrapping
// ErrClaimExactReadback. A write error whose re-read failed too wraps
// ErrClaimExactUnsettled: the claim may have landed. The returned Bead is the
// freshest snapshot the call held, and the zero Bead only when the
// reserved-key guard or the initial Get failed.
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

	fromGeneration := b.Metadata[beadmeta.ClaimGenerationMetadataKey]
	if req.FromGeneration != nil && fromGeneration != *req.FromGeneration {
		return ClaimExactResult{Bead: b, Outcome: ClaimExactStale}, nil
	}

	toGeneration, err := nextClaimGeneration(fromGeneration)
	if err != nil {
		return ClaimExactResult{Bead: b}, fmt.Errorf("claim exact %q: %w", id, err)
	}

	effects := claimEffectsWithGeneration(req.OnSuccess, toGeneration)
	landing := claimExactLanding{reader: reader, id: id, read: b, want: req.Want, effects: effects}
	if writers.Revision != nil {
		err := writers.Revision.UpdateIfMatch(id, b.Revision, effects)
		switch {
		case err == nil:
			return landing.confirm(ClaimExactFenceRevision)
		case beads.IsPreconditionFailed(err):
			return ClaimExactResult{Bead: bestEffortGet(reader, id, b), Outcome: ClaimExactStale}, nil
		case !errors.Is(err, beads.ErrConditionalWriteUnsupported):
			return landing.settle(ClaimExactFenceRevision, err)
		}
	}
	if writers.Assignment == nil {
		return ClaimExactResult{Bead: b}, claimExactNoFence(id)
	}
	if req.Want.Status == nil || req.Want.Assignee == nil {
		return ClaimExactResult{Bead: b}, fmt.Errorf("claim exact %q: the assignment guard needs an observed status and assignee: %w", id, beads.ErrConditionalWriteUnsupported)
	}
	landed, err := writers.Assignment.UpdateIfAssignment(id, *req.Want.Status, *req.Want.Assignee, effects)
	switch {
	case errors.Is(err, beads.ErrConditionalWriteUnsupported):
		return ClaimExactResult{Bead: b}, claimExactNoFence(id)
	case err != nil:
		return landing.settle(ClaimExactFenceAssignment, err)
	case !landed:
		return ClaimExactResult{Bead: bestEffortGet(reader, id, b), Outcome: ClaimExactStale}, nil
	}
	return landing.confirm(ClaimExactFenceAssignment)
}

// claimExactNoFence is the error of a claim that wrote nothing because the
// store offered neither a revision fence nor an assignment guard.
func claimExactNoFence(id string) error {
	return fmt.Errorf("claim exact %q: store supports neither a revision fence nor an assignment guard: %w", id, beads.ErrConditionalWriteUnsupported)
}

// claimExactLanding is an exact claim from its write on: the bead as the
// claim read it, the observation it matched, and the effects its write
// carried.
type claimExactLanding struct {
	reader  ClaimExactReader
	id      string
	read    beads.Bead
	want    ClaimExactPreconditions
	effects beads.UpdateOpts
}

// confirm re-reads a claim whose write landed through fence and classifies
// the readback (outcome). A failed re-read still reports the claim, with an
// error wrapping ErrClaimExactReadback.
func (l claimExactLanding) confirm(fence ClaimExactFence) (ClaimExactResult, error) {
	final, err := l.reader.Get(l.id)
	if err != nil {
		return ClaimExactResult{Bead: l.read, Outcome: ClaimExactClaimed, Fence: fence},
			fmt.Errorf("claim exact %q: %w: %w", l.id, ErrClaimExactReadback, err)
	}
	return l.outcome(final, fence), nil
}

// settle decides, from one re-read, a write through fence that answered
// writeErr rather than success or a precondition: a row that carries the
// write is the claim landing (outcome), and a row that does not leaves
// writeErr standing. A failed re-read leaves the claim unsettled, with an
// error wrapping ErrClaimExactUnsettled, writeErr and the re-read's error. A
// caller checks ErrClaimExactUnsettled first: a not-found the error also
// wraps does not prove the claim wrote nothing.
func (l claimExactLanding) settle(fence ClaimExactFence, writeErr error) (ClaimExactResult, error) {
	writeErr = fmt.Errorf("claim exact %q: applying claim effects: %w", l.id, writeErr)
	current, err := l.reader.Get(l.id)
	switch {
	case err != nil:
		return ClaimExactResult{Bead: l.read}, fmt.Errorf("%w: %w (re-read: %w)", ErrClaimExactUnsettled, writeErr, err)
	case !l.carriesWrite(current):
		return ClaimExactResult{Bead: current}, writeErr
	}
	return l.outcome(current, fence), nil
}

// outcome classifies the readback of a claim whose write landed through
// fence: ClaimExactClaimed while the observation holds on it
// (observationHolds), ClaimExactObservationMoved once it does not.
func (l claimExactLanding) outcome(final beads.Bead, fence ClaimExactFence) ClaimExactResult {
	outcome := ClaimExactClaimed
	if !l.observationHolds(final) {
		outcome = ClaimExactObservationMoved
	}
	return ClaimExactResult{Bead: final, Outcome: outcome, Fence: fence}
}

// observationHolds reports whether a landed claim's readback still carries
// want's routing, root, step and group keys and the generation the write
// advanced to, each as the write left it: a key the write itself set is held
// to the written value. The status and assignee are the caller's to check;
// the write changed them.
func (l claimExactLanding) observationHolds(final beads.Bead) bool {
	for key, observed := range map[string]*string{
		beadmeta.RoutedToMetadataKey:          l.want.RoutedTo,
		beadmeta.RootBeadIDMetadataKey:        l.want.RootBeadID,
		beadmeta.StepIDMetadataKey:            l.want.StepID,
		beadmeta.ContinuationGroupMetadataKey: l.want.ContinuationGroup,
		beadmeta.ClaimGenerationMetadataKey:   nil,
	} {
		expected, written := l.effects.Metadata[key]
		if !written {
			if observed == nil {
				continue
			}
			expected = *observed
		}
		if final.Metadata[key] != expected {
			return false
		}
	}
	return true
}

// carriesWrite reports whether b carries the claim's write: the status it
// set, its assignee (the observed one, when the write kept it), and the
// generation it advanced to.
func (l claimExactLanding) carriesWrite(b beads.Bead) bool {
	assignee := l.want.Assignee
	if l.effects.Assignee != nil {
		assignee = l.effects.Assignee
	}
	return (l.effects.Status == nil || b.Status == *l.effects.Status) &&
		(assignee == nil || b.Assignee == *assignee) &&
		b.Metadata[beadmeta.ClaimGenerationMetadataKey] == l.effects.Metadata[beadmeta.ClaimGenerationMetadataKey]
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
func bestEffortGet(store ClaimExactReader, id string, fallback beads.Bead) beads.Bead {
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
	if want.Assignee != nil && b.Assignee != *want.Assignee {
		return false
	}
	if want.RootBeadID != nil && b.Metadata[beadmeta.RootBeadIDMetadataKey] != *want.RootBeadID {
		return false
	}
	if want.StepID != nil && b.Metadata[beadmeta.StepIDMetadataKey] != *want.StepID {
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
