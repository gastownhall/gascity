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

// hookClaimVerdict is how one claim attempt on a candidate resolved.
type hookClaimVerdict string

const (
	// hookClaimVerdictNone means no claim landed for this session: the claim
	// errored, or a --claim was lost to another claimant.
	hookClaimVerdictNone hookClaimVerdict = ""
	// hookClaimVerdictClaimed means this session now holds the bead.
	hookClaimVerdictClaimed hookClaimVerdict = "claimed"
	// hookClaimVerdictRejected means an exact claim wrote nothing because the
	// bead no longer matched the observation it was fenced on, or its
	// readback names another owner: a lost race, not an error.
	hookClaimVerdictRejected hookClaimVerdict = "rejected"
)

// hookClaimExactFunc (ctx, dir, env, actor, req) claims exactly the bead req
// names, only while it still matches what the work query observed on it.
type hookClaimExactFunc func(ctx context.Context, dir string, env []string, actor string, req molecule.ClaimExactRequest) (beads.Bead, hookClaimVerdict, error)

// hookClaimCandidateIsGraphStep reports whether a candidate is a step of a
// workflow graph: it names both its root and its step. Such a candidate is
// claimed only through the exact claim, fenced on that observation, because
// bd's --claim checks status and assignee alone and would land on a row whose
// step identity moved after the work query read it.
func hookClaimCandidateIsGraphStep(candidate beads.Bead) bool {
	return strings.TrimSpace(candidate.Metadata[beadmeta.RootBeadIDMetadataKey]) != "" &&
		strings.TrimSpace(candidate.Metadata[beadmeta.StepIDMetadataKey]) != ""
}

// hookClaimExactPlan builds the exact claim of candidate from the work query's
// observation of it: the observed status, assignee and identity keys are the
// claim's preconditions, and the claim's effects are the transition to
// in_progress (plus the assignee when assign is set, i.e. the bead is
// unassigned). A routing or group key the projection did not carry is left
// unfenced rather than fenced on an absent value it never saw.
//
// reclaimed means this invocation just reverted the candidate's stale lease:
// the observation predates that revert, so the claim is fenced on the open,
// unassigned row the reclaim left instead of the observed lease.
func hookClaimExactPlan(candidate beads.Bead, actor string, assign, reclaimed bool) molecule.ClaimExactRequest {
	status := strings.TrimSpace(candidate.Status)
	assignee := strings.TrimSpace(candidate.Assignee)
	if reclaimed {
		status, assignee = "open", ""
	}
	stepID := candidate.Metadata[beadmeta.StepIDMetadataKey]
	rootID := candidate.Metadata[beadmeta.RootBeadIDMetadataKey]
	want := molecule.ClaimExactPreconditions{
		Status:     &status,
		Assignee:   &assignee,
		StepID:     &stepID,
		RootBeadID: &rootID,
	}
	if routedTo, ok := candidate.Metadata[beadmeta.RoutedToMetadataKey]; ok {
		want.RoutedTo = &routedTo
	}
	if group, ok := candidate.Metadata[beadmeta.ContinuationGroupMetadataKey]; ok {
		want.ContinuationGroup = &group
	}
	inProgress := "in_progress"
	onSuccess := beads.UpdateOpts{Status: &inProgress}
	if assign {
		assignee := actor
		onSuccess.Assignee = &assignee
	}
	return molecule.ClaimExactRequest{ID: candidate.ID, Want: want, OnSuccess: onSuccess}
}

// claimHookCandidate claims one candidate as actor and reports the verdict. A
// graph step goes through ops.ClaimExact and never falls back to ops.Claim;
// every other candidate keeps the --claim path. assign is whether the claim
// sets the assignee (the candidate is unassigned) rather than promoting a bead
// already assigned to actor; reclaimed is hookClaimExactPlan's.
//
// A rejected exact claim is named on stderr: the caller reports it as a lost
// race, and bead.claim_rejected names only a rival owner, which a moved step
// identity does not have.
func claimHookCandidate(ctx context.Context, candidate beads.Bead, actor string, assign, reclaimed bool, opts hookClaimOptions, ops hookClaimOps, dir string, stderr io.Writer) (beads.Bead, hookClaimVerdict, error) {
	if !hookClaimCandidateIsGraphStep(candidate) {
		claimed, ok, err := ops.Claim(ctx, dir, opts.Env, candidate.ID, actor)
		if ok {
			return claimed, hookClaimVerdictClaimed, err
		}
		return claimed, hookClaimVerdictNone, err
	}
	claimed, verdict, err := ops.ClaimExact(ctx, dir, opts.Env, actor, hookClaimExactPlan(candidate, actor, assign, reclaimed))
	if err == nil && verdict == hookClaimVerdictRejected {
		fmt.Fprintf(stderr, "gc hook --claim: exact claim of %s rejected: the bead no longer matches the work query's observation of it\n", candidate.ID) //nolint:errcheck
	}
	return claimed, verdict, err
}

// hookClaimExactWithBdStore is the default ClaimExact seam: the exact claim
// against the work-directory bd context, writing as actor.
func hookClaimExactWithBdStore(ctx context.Context, dir string, env []string, actor string, req molecule.ClaimExactRequest) (beads.Bead, hookClaimVerdict, error) {
	store := hookClaimBdStoreContext(ctx, dir, env, actor)
	return hookClaimExactThroughStore(store, molecule.ClaimExactWriters{Revision: store}, actor, req)
}

// hookClaimExactThroughStore runs one exact claim and classifies it the way
// hookClaimThroughStore classifies a --claim: a claim whose readback names
// another owner is not ours, and an answered precondition or fence mismatch
// is a lost race rather than an error.
func hookClaimExactThroughStore(reader molecule.ClaimExactReader, writers molecule.ClaimExactWriters, actor string, req molecule.ClaimExactRequest) (beads.Bead, hookClaimVerdict, error) {
	res, err := molecule.ClaimExactWithWriters(reader, writers, req)
	if err != nil {
		return res.Bead, hookClaimVerdictNone, err
	}
	switch res.Outcome {
	case molecule.ClaimExactClaimed:
		if !hookClaimHasIdentity(res.Bead.Assignee, []string{actor}) {
			return res.Bead, hookClaimVerdictRejected, nil
		}
		return res.Bead, hookClaimVerdictClaimed, nil
	case molecule.ClaimExactPreconditionFailed, molecule.ClaimExactStale:
		return res.Bead, hookClaimVerdictRejected, nil
	default:
		return res.Bead, hookClaimVerdictNone, fmt.Errorf("claim exact %q: unknown outcome %q", req.ID, res.Outcome)
	}
}
