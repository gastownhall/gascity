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

// hookInputAdmissionFunc judges candidates (ctx, dir, env, candidates): the
// input-withheld ones, and the ones a prerequisite beyond the judge's stores
// leaves unresolved, by id.
type hookInputAdmissionFunc func(ctx context.Context, dir string, env []string, candidates []beads.Bead) (inputadmission.Verdicts, error)

// hookInputAdmissionWithBdStore is the default InputAdmission seam: one judge
// over the store at dir, opened through the workflow-store seam the stop gate
// and retirement read (hookWorkflowStoreWithBdStore). It reads that store
// alone, so a prerequisite target only another store holds is unresolved there
// (inputadmission.Unresolved), and withholdInputGatedCandidates leaves that
// row to the federated reader's verdict instead of deciding it on this store.
func hookInputAdmissionWithBdStore(ctx context.Context, dir string, env []string, candidates []beads.Bead) (inputadmission.Verdicts, error) {
	return inputadmission.New([]inputadmission.Source{
		{Label: "bd store at " + dir, Reader: hookWorkflowStoreWithBdStore(ctx, dir, env, "", "")},
	}, time.Now()).Verdicts(ctx, 0, candidates)
}

// hookAdmission is what input admission did to one work-query answer.
type hookAdmission struct {
	// withheld: it dropped a candidate whose workflow input withholds it.
	withheld bool
	// deferred: it held back a candidate whose input waits on a prerequisite
	// no store the claim's judge reads holds, for the federated form of the
	// claim's query to decide (AdmissionFallbackQuery).
	deferred bool
}

// reAsk reports whether the claim must re-ask the federated form of its query
// rather than claim from kept, the candidates this answer left: admission
// deferred a candidate to that form's verdict, or withheld every one of them.
func (a hookAdmission) reAsk(kept []beads.Bead) bool {
	return a.deferred || (a.withheld && len(kept) == 0)
}

// screenHookClaimOutput turns one work-query answer into the candidates the
// claim tiers may look at (decodeHookClaimOutput, then
// screenHookClaimCandidates). admission reports what input admission did. A
// non-nil result is terminal: its error is already on stderr.
func screenHookClaimOutput(output string, opts hookClaimOptions, ops hookClaimOps, dir string, stderr io.Writer) (candidates []beads.Bead, admission hookAdmission, failed *hookClaimResult) {
	candidates, failed = decodeHookClaimOutput(output, ops, stderr)
	if failed != nil || len(candidates) == 0 {
		return nil, hookAdmission{}, failed
	}
	return screenHookClaimCandidates(candidates, opts, ops, dir, stderr)
}

// decodeHookClaimOutput decodes the ready rows of one work-query answer. A
// non-nil result is terminal: its error is already on stderr.
func decodeHookClaimOutput(output string, ops hookClaimOps, stderr io.Writer) ([]beads.Bead, *hookClaimResult) {
	normalized := normalizeWorkQueryOutput(strings.TrimSpace(output))
	normalized = filterUnreadyHookCandidates(normalized, ops.nowOrWallClock())
	if !workQueryHasReadyWork(normalized) {
		return nil, nil
	}
	candidates, skipped, err := decodeHookClaimBeads(normalized)
	if err != nil {
		fmt.Fprintf(stderr, "gc hook --claim: requires JSON work_query output to identify claim candidates: %v\n", err) //nolint:errcheck
		return nil, &hookClaimResult{terminal: true, code: 1}
	}
	for _, skip := range skipped {
		fmt.Fprintf(stderr, "gc hook --claim: skipping undecodable bead %s: %v\n", skip.ID, skip.Err) //nolint:errcheck
	}
	return candidates, nil
}

// screenHookClaimCandidates retires the candidates whose workflow is done and
// drops the input-withheld ones. admission reports what input admission did.
// A non-nil result is terminal: its error is already on stderr.
func screenHookClaimCandidates(candidates []beads.Bead, opts hookClaimOptions, ops hookClaimOps, dir string, stderr io.Writer) ([]beads.Bead, hookAdmission, *hookClaimResult) {
	// Retire, before any tier looks at them, the candidates whose workflow has
	// nothing left to do because its input convoy work is already terminal or
	// no longer exists anywhere. Retirement runs before admission: a done
	// workflow is closed, not withheld.
	candidates = retireDoneWorkflowCandidates(candidates, opts, ops, dir, stderr)
	candidates, admission, err := withholdInputGatedCandidates(candidates, opts, ops, dir, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc hook --claim: %v; claiming nothing\n", err) //nolint:errcheck
		return nil, hookAdmission{}, &hookClaimResult{terminal: true, code: 1}
	}
	return candidates, admission, nil
}

// reAskFederatedHookClaim runs the federated form of the claim's query
// (AdmissionFallbackQuery) once and screens its answer, for a single-store
// answer whose screen called for it (hookAdmission.reAsk). That form's tiers
// admit before they bound, judging every store, so a row there that the
// claim's own judge cannot resolve was admitted by the reader that can, and is
// kept. The answer spans the city while the claim writes through the store at
// dir, so only the rows that store holds are screened and offered to the
// claim tiers (keepHookRowsTheLegHolds); the store loop claims the rest from
// the legs that hold them.
//
// A read the re-ask cannot make is a failed read elsewhere, which is no reason
// to refuse work the claim's own judge admitted: the federated form itself
// (gc ready fails whole when any store or input judgment it reads fails), or
// which of its rows the store at dir holds. Then it returns survivors, the
// candidates the single-store answer's screen kept, with that read's error;
// the rows that screen withheld or deferred stay refused. The caller reports
// the failure as a failed secondary read (hookStoreFailureReport.reAskFailed),
// and an invocation that then claims nothing ends without a drain
// (hookClaimResult.reAskFailed). A non-nil result is terminal: its error is
// already on stderr.
func reAskFederatedHookClaim(survivors []beads.Bead, opts hookClaimOptions, ops hookClaimOps, dir string, stderr io.Writer) ([]beads.Bead, *hookClaimResult, error) {
	output, err := ops.Runner(opts.AdmissionFallbackQuery, dir)
	if err != nil {
		return survivors, nil, err
	}
	candidates, failed := decodeHookClaimOutput(output, ops, stderr)
	if failed != nil || len(candidates) == 0 {
		return nil, failed, nil
	}
	if candidates, err = keepHookRowsTheLegHolds(candidates, opts, ops, dir); err != nil {
		return survivors, nil, err
	}
	if len(candidates) == 0 {
		return nil, nil, nil
	}
	federated := opts
	federated.AdmissionFallbackQuery = ""
	candidates, _, failed = screenHookClaimCandidates(candidates, federated, ops, dir, stderr)
	return candidates, failed, nil
}

// keepHookRowsTheLegHolds keeps, in order, the rows of a federated answer
// that the store at dir holds. The claim tiers write through that store, so a
// row another leg's store holds would fail there as not found, and each such
// failure is a bead.claim_failed the claim-loop guard counts against a bead
// that is claimable; the store loop claims it from the leg that holds it. One
// batched read by id answers for the issue tier. bd's batch read can miss the
// wisp tier, so a wisp row the batch did not find is read on its own
// (beads.GetEach). A read that fails is an error: a row whose residence was
// not read is not claimed.
func keepHookRowsTheLegHolds(rows []beads.Bead, opts hookClaimOptions, ops hookClaimOps, dir string) ([]beads.Bead, error) {
	ctx, cancel := context.WithTimeout(context.Background(), hookClaimMutationTimeout)
	defer cancel()
	store := ops.WorkflowStore(ctx, dir, opts.Env, "", "")
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
	}
	held, err := beads.GetMany(store, ids)
	if err != nil {
		return nil, fmt.Errorf("reading which federated rows the store at %s holds: %w", dir, err)
	}
	var wisps []string
	for _, row := range rows {
		if _, ok := held[row.ID]; !ok && row.Ephemeral {
			wisps = append(wisps, row.ID)
		}
	}
	heldWisps, err := beads.GetEach(store, wisps)
	if err != nil {
		return nil, fmt.Errorf("reading which federated wisps the store at %s holds: %w", dir, err)
	}
	kept := rows[:0:0]
	for _, row := range rows {
		_, batched := held[row.ID]
		_, wisp := heldWisps[row.ID]
		if batched || wisp {
			kept = append(kept, row)
		}
	}
	return kept, nil
}

// withholdInputGatedCandidates drops every candidate this session would serve
// whose workflow input withholds it, naming each on stderr and leaving its
// status, assignee and metadata untouched. A judgment that could not be read
// is an error: a row that was not judged is never claimed.
//
// A candidate whose input waits on a prerequisite no store the judge reads
// holds (inputadmission.Unresolved) is the federated reader's to decide. When
// the claim's query has a federated form (AdmissionFallbackQuery), it is held
// back, named on stderr, and reported deferred, so the claim re-asks that form
// and claims only from its answer (reAskFederatedHookClaim). Otherwise it is
// kept. The answer either came from the federated reader, which judged every
// store (a federated query, or the re-asked form's own answer), or from a
// custom work_query. A custom query reads through no federated reader, so
// there is no verdict to take: the row is named on stderr and served on this
// store's read. Refusing it would refuse it for good, since this judge never
// reads the store that holds the prerequisite.
func withholdInputGatedCandidates(candidates []beads.Bead, opts hookClaimOptions, ops hookClaimOps, dir string, stderr io.Writer) (kept []beads.Bead, admission hookAdmission, err error) {
	if ops.InputAdmission == nil {
		return candidates, hookAdmission{}, nil
	}
	subjects := hookInputAdmissionSubjects(candidates, opts, ops.nowOrWallClock())
	if len(subjects) == 0 {
		return candidates, hookAdmission{}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookClaimMutationTimeout)
	defer cancel()
	verdicts, err := ops.InputAdmission(ctx, dir, opts.Env, subjects)
	if err != nil {
		return nil, hookAdmission{}, err
	}
	if len(verdicts.Withheld) == 0 && len(verdicts.Unresolved) == 0 {
		return candidates, hookAdmission{}, nil
	}
	deferUnresolved := opts.AdmissionFallbackQuery != ""
	kept = candidates[:0:0]
	for _, candidate := range candidates {
		if verdict, ok := verdicts.Withheld[candidate.ID]; ok {
			fmt.Fprintf(stderr, "gc hook --claim: withheld %s: %s\n", candidate.ID, verdict) //nolint:errcheck
			admission.withheld = true
			continue
		}
		unknown, unresolved := verdicts.Unresolved[candidate.ID]
		switch {
		case unresolved && deferUnresolved:
			fmt.Fprintf(stderr, "gc hook --claim: deferred %s to the federated reader: %s\n", candidate.ID, unknown) //nolint:errcheck
			admission.deferred = true
			continue
		case unresolved && opts.AdmissionBlindQuery:
			fmt.Fprintf(stderr, "gc hook --claim: serving %s on this store's read: %s; the agent's custom work_query does not read through the federated reader that judges it\n", candidate.ID, unknown) //nolint:errcheck
		}
		kept = append(kept, candidate)
	}
	if admission.withheld && opts.AdmissionBlindQuery {
		fmt.Fprintln(stderr, "gc hook --claim: the agent's custom work_query does not read through input admission; the withheld rows it returned were skipped") //nolint:errcheck
	}
	return kept, admission, nil
}

// hookInputAdmissionSubjects is the candidates input admission judges: those
// this session would serve (its own, or claimable on its routes) that are
// subject to admission at all.
func hookInputAdmissionSubjects(candidates []beads.Bead, opts hookClaimOptions, now time.Time) []beads.Bead {
	var subjects []beads.Bead
	for _, candidate := range candidates {
		if hookClaimCandidateIsMessage(candidate) || !inputadmission.Subject(candidate) {
			continue
		}
		if hookClaimHasIdentity(candidate.Assignee, opts.IdentityCandidates) || hookCandidateClaimable(candidate, opts.RouteTargets, now) {
			subjects = append(subjects, candidate)
		}
	}
	return subjects
}
