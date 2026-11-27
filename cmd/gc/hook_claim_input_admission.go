package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/inputadmission"
)

// hookInputAdmissionFunc judges candidates (ctx, dir, env, candidates) and
// returns the input-withheld ones by id.
type hookInputAdmissionFunc func(ctx context.Context, dir string, env []string, candidates []beads.Bead) (map[string]inputadmission.Withhold, error)

// hookInputAdmissionWithBdStore is the default InputAdmission seam: one judge
// over the bd store at dir.
func hookInputAdmissionWithBdStore(ctx context.Context, dir string, env []string, candidates []beads.Bead) (map[string]inputadmission.Withhold, error) {
	sources := []inputadmission.Source{{Label: "bd store at " + dir, Reader: hookClaimBdStoreContext(ctx, dir, env, "")}}
	return inputadmission.New(sources, time.Now()).Judge(ctx, 0, candidates)
}

// screenHookClaimOutput turns one work-query answer into the candidates the
// claim tiers may look at: ready rows decoded, done workflows retired, and
// input-withheld rows dropped. withheld reports that admission dropped any. A
// non-nil result is terminal: its error is already on stderr.
func screenHookClaimOutput(output string, opts hookClaimOptions, ops hookClaimOps, dir string, stderr io.Writer) (candidates []beads.Bead, withheld bool, failed *hookClaimResult) {
	normalized := normalizeWorkQueryOutput(strings.TrimSpace(output))
	normalized = filterUnreadyHookCandidates(normalized, ops.nowOrWallClock())
	if !workQueryHasReadyWork(normalized) {
		return nil, false, nil
	}
	candidates, skipped, err := decodeHookClaimBeads(normalized)
	if err != nil {
		fmt.Fprintf(stderr, "gc hook --claim: requires JSON work_query output to identify claim candidates: %v\n", err) //nolint:errcheck
		return nil, false, &hookClaimResult{terminal: true, code: 1}
	}
	for _, skip := range skipped {
		fmt.Fprintf(stderr, "gc hook --claim: skipping undecodable bead %s: %v\n", skip.ID, skip.Err) //nolint:errcheck
	}
	if len(candidates) == 0 {
		return nil, false, nil
	}
	// Retire, before any tier looks at them, the candidates whose workflow has
	// nothing left to do because its input convoy work is already terminal or
	// no longer exists anywhere. Retirement runs before admission: a done
	// workflow is closed, not withheld.
	candidates = retireDoneWorkflowCandidates(candidates, opts, ops, dir, stderr)
	candidates, withheld, err = withholdInputGatedCandidates(candidates, opts, ops, dir, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc hook --claim: %v; claiming nothing\n", err) //nolint:errcheck
		return nil, false, &hookClaimResult{terminal: true, code: 1}
	}
	return candidates, withheld, nil
}

// withholdInputGatedCandidates drops every candidate this session would serve
// whose workflow input withholds it, naming each on stderr and leaving its
// status, assignee and metadata untouched. withheld reports that any was
// dropped. A judgment that could not be read is an error: a row that was not
// judged is never claimed.
func withholdInputGatedCandidates(candidates []beads.Bead, opts hookClaimOptions, ops hookClaimOps, dir string, stderr io.Writer) (kept []beads.Bead, withheld bool, err error) {
	if ops.InputAdmission == nil {
		return candidates, false, nil
	}
	now := ops.nowOrWallClock()
	var subjects []beads.Bead
	for _, candidate := range candidates {
		if hookClaimCandidateIsMessage(candidate) || !inputadmission.Subject(candidate) {
			continue
		}
		if hookClaimHasIdentity(candidate.Assignee, opts.IdentityCandidates) || hookCandidateClaimable(candidate, opts.RouteTargets, now) {
			subjects = append(subjects, candidate)
		}
	}
	if len(subjects) == 0 {
		return candidates, false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookClaimMutationTimeout)
	defer cancel()
	verdicts, err := ops.InputAdmission(ctx, dir, opts.Env, subjects)
	if err != nil {
		return nil, false, err
	}
	if len(verdicts) == 0 {
		return candidates, false, nil
	}
	kept = candidates[:0:0]
	for _, candidate := range candidates {
		if verdict, ok := verdicts[candidate.ID]; ok {
			fmt.Fprintf(stderr, "gc hook --claim: withheld %s: %s\n", candidate.ID, verdict) //nolint:errcheck
			continue
		}
		kept = append(kept, candidate)
	}
	if opts.AdmissionBlindQuery {
		fmt.Fprintln(stderr, "gc hook --claim: the agent's custom work_query does not read through input admission; the withheld rows it returned were skipped") //nolint:errcheck
	}
	return kept, true, nil
}
