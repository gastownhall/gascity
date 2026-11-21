package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/molecule"
)

// hookClaimExactVerdict is how an exact claim resolved, in the claim loop's
// vocabulary: a claim that landed, a rejection that lost the race or found
// the row changed before the write, or a write that landed onto a row whose
// observed identity had already moved (the caller must give that bead back).
type hookClaimExactVerdict string

const (
	hookClaimExactClaimedVerdict  hookClaimExactVerdict = "claimed"
	hookClaimExactRejectedVerdict hookClaimExactVerdict = "rejected"
	hookClaimExactMovedVerdict    hookClaimExactVerdict = "moved"
)

// hookClaimExactFunc is the seam the claim tiers drive graph-step candidates
// through. It must commit the request through conditional writers only —
// never an unconditional `bd update --claim` — so the observed row is the row
// claimed.
type hookClaimExactFunc func(ctx context.Context, dir string, env []string, actor string, req molecule.ClaimExactRequest) (molecule.ClaimExactResult, error)

// hookClaimExactWithBdStore is the default ClaimExact seam: the work
// directory's bd context as both the reader and the conditional writer.
func hookClaimExactWithBdStore(ctx context.Context, dir string, env []string, actor string, req molecule.ClaimExactRequest) (molecule.ClaimExactResult, error) {
	store := hookClaimBdStoreContext(ctx, dir, env, actor)
	return molecule.ClaimExactWithWriters(store, molecule.ClaimExactWriters{Revision: store}, req)
}

// hookClaimExactCandidate reports whether the hook's work query returned a
// graph step: a bead bound to a workflow root with a step identity. The
// row's step, root, route, and assignee can all move between the query and
// the claim, so these candidates claim through the exact seam only and never
// take the unfenced pool claim.
func hookClaimExactCandidate(candidate beads.Bead) bool {
	return strings.TrimSpace(candidate.Metadata[beadmeta.RootBeadIDMetadataKey]) != "" &&
		strings.TrimSpace(candidate.Metadata[beadmeta.StepIDMetadataKey]) != ""
}

// hookClaimExactPlan builds the exact-claim request for the graph-step
// candidate the hook observed. Every identity fact the projection carried
// becomes a precondition — status, assignee (empty means unassigned), step,
// root, and route/continuation group when present — and the claim effects are
// the pool claim's status transition plus the assignee, applied only when the
// observed row was unassigned (a ready assignment already carries its owner).
func hookClaimExactPlan(candidate beads.Bead, actor string) molecule.ClaimExactRequest {
	status := strings.TrimSpace(candidate.Status)
	assignee := strings.TrimSpace(candidate.Assignee)
	stepID := strings.TrimSpace(candidate.Metadata[beadmeta.StepIDMetadataKey])
	rootID := strings.TrimSpace(candidate.Metadata[beadmeta.RootBeadIDMetadataKey])
	want := molecule.ClaimExactPreconditions{
		Status:     &status,
		Assignee:   &assignee,
		StepID:     &stepID,
		RootBeadID: &rootID,
	}
	if routed := strings.TrimSpace(candidate.Metadata[beadmeta.RoutedToMetadataKey]); routed != "" {
		want.RoutedTo = &routed
	}
	if group := strings.TrimSpace(candidate.Metadata[beadmeta.ContinuationGroupMetadataKey]); group != "" {
		want.ContinuationGroup = &group
	}
	inProgress := "in_progress"
	onSuccess := beads.UpdateOpts{Status: &inProgress}
	if assignee == "" {
		owner := actor
		onSuccess.Assignee = &owner
	}
	return molecule.ClaimExactRequest{ID: candidate.ID, Want: want, OnSuccess: onSuccess}
}

// hookClaimExactThroughStore classifies a molecule.ClaimExactWithWriters
// result into the claim loop's vocabulary — the exact-claim counterpart of
// hookClaimThroughStore, factored for the same reason: the classification IS
// the contract the caller reads. A lost race (precondition or generation
// mismatch) is a rejection and not an error, so the hook can move on to other
// work; a committed write whose readback failed keeps the claimed verdict
// alongside the error so the caller stops instead of draining; a claimed
// result the store does not hand to this actor is a rejection, never ours.
func hookClaimExactThroughStore(actor string, res molecule.ClaimExactResult, err error) (beads.Bead, hookClaimExactVerdict, error) {
	if err != nil {
		if res.Outcome == molecule.ClaimExactClaimed {
			return res.Bead, hookClaimExactClaimedVerdict, err
		}
		return res.Bead, "", err
	}
	switch res.Outcome {
	case molecule.ClaimExactClaimed:
		if !hookClaimHasIdentity(res.Bead.Assignee, []string{actor}) {
			return res.Bead, hookClaimExactRejectedVerdict, nil
		}
		return res.Bead, hookClaimExactClaimedVerdict, nil
	case molecule.ClaimExactPreconditionFailed, molecule.ClaimExactStale:
		return res.Bead, hookClaimExactRejectedVerdict, nil
	case molecule.ClaimExactObservationMoved:
		return res.Bead, hookClaimExactMovedVerdict, nil
	}
	return res.Bead, "", fmt.Errorf("claim exact %q: unresolved outcome %q", res.Bead.ID, res.Outcome)
}

// hookClaimEligibleTierClaim runs one eligible-tier candidate's claim. A
// graph step — a candidate carrying both a workflow root and a step identity —
// claims through the exact seam, fenced to the row this hook observed; it
// never falls back to the unfenced pool claim, whatever the exact seam
// answers. Every other candidate takes the pool claim unchanged.
//
// reclaimed reports that the reclaim lane just reverted this candidate's
// stale lease: bd reclaim leaves the row open and unassigned, so the exact
// fence keys on the row as this invocation now knows it (assignee empty)
// rather than as the work query saw it.
func hookClaimEligibleTierClaim(ctx context.Context, candidate beads.Bead, reclaimed bool, opts hookClaimOptions, ops hookClaimOps, dir string) (beads.Bead, bool, bool, error) {
	if !hookClaimExactCandidate(candidate) {
		claimed, ok, err := ops.Claim(ctx, dir, opts.Env, candidate.ID, opts.Assignee)
		return claimed, ok, false, err
	}
	observed := candidate
	if reclaimed {
		observed.Assignee = ""
	}
	res, resErr := ops.ClaimExact(ctx, dir, opts.Env, opts.Assignee, hookClaimExactPlan(observed, opts.Assignee))
	claimed, verdict, err := hookClaimExactThroughStore(opts.Assignee, res, resErr)
	return claimed, verdict == hookClaimExactClaimedVerdict, verdict == hookClaimExactMovedVerdict, err
}

// releaseMovedHookExactClaim gives back a claim whose guarded write landed on
// a row that is no longer the step this hook observed (outcome
// observation_moved): the write committed, so this session owns the bead and
// must release it before the hook tries any other candidate. Mirrors
// releaseExcludedHookClaim; the reason string travels on the
// bead.claim_released event.
func releaseMovedHookExactClaim(claimed beads.Bead, opts hookClaimOptions, ops hookClaimOps, dir string, stderr io.Writer) bool {
	assignee := strings.TrimSpace(claimed.Assignee)
	if assignee == "" {
		assignee = opts.Assignee
	}
	forgetHookWorkStarted(opts, ops, hookStartedKeyFor(ops, dir, claimed.ID, hookClaimMinted))
	ctx, cancel := context.WithTimeout(context.Background(), hookClaimMutationTimeout)
	defer cancel()
	released, err := ops.Release(ctx, dir, opts.Env, claimed.ID, assignee)
	switch {
	case err != nil:
		fmt.Fprintf(stderr, "gc hook --claim: exact claim on %s landed after its observed step moved, and releasing it failed: %v (recover with: bd update %s --status open --assignee \"\")\n", //nolint:errcheck
			claimed.ID, err, claimed.ID)
		return false
	case !released:
		fmt.Fprintf(stderr, "gc hook --claim: exact claim on %s landed after its observed step moved; it was no longer ours to release\n", //nolint:errcheck
			claimed.ID)
	default:
		ops.EmitClaimReleased(hookClaimReleaseRecord{BeadID: claimed.ID, Assignee: assignee, Reason: "observation_moved"})
		fmt.Fprintf(stderr, "gc hook --claim: released %s: its observed step moved after the claim landed\n", //nolint:errcheck
			claimed.ID)
	}
	return true
}
