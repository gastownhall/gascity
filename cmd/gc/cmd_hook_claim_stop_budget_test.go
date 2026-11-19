package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// This file pins the per-operation claim budgets (#9178): the workflow stop
// gate, the stale reclaim, the claim CAS and the post-CAS re-check each run on
// a context of their own, so a slow ancestry read before the CAS can no longer
// spend the CAS's reserved budget and leave bd writing on an expired context
// (`claiming bead "<id>": timed out after 0s (caller deadline)`).
//
// None of these tests run in parallel: they change the package-level
// hookClaimMutationTimeout.

// shortenHookClaimBudget shortens the reserved claim mutation budget for the
// duration of one test. These tests make store reads that spend a whole
// budget, so the 10s production default would make them crawl; the
// TestReadyHookAssignmentDeadlineDoesNotFallThroughToFreshWork precedent covers
// changing this package var.
func shortenHookClaimBudget(t *testing.T) {
	t.Helper()
	old := hookClaimMutationTimeout
	hookClaimMutationTimeout = 300 * time.Millisecond
	t.Cleanup(func() { hookClaimMutationTimeout = old })
}

// budgetSpendingStore makes the reads the stop gate makes for the ids under
// test behave like a store at the bottom of its caller's budget: a read that
// starts inside the budget blocks until that budget is gone and then finishes
// (a slow ancestry read that spends the whole budget and still answers), and a
// read that starts on an already-spent context fails the way bd fails it.
type budgetSpendingStore struct {
	beads.Store
	ctx     context.Context
	slowIDs map[string]bool
}

func (s budgetSpendingStore) Get(id string) (beads.Bead, error) {
	if s.slowIDs[id] {
		if err := s.ctx.Err(); err != nil {
			return beads.Bead{}, fmt.Errorf("getting %s: %w", id, err)
		}
		<-s.ctx.Done()
	}
	return s.Store.Get(id)
}

// budgetCtxCall samples, at the moment a claim-tier store operation starts,
// the context it was handed: after the tier returns every per-operation
// context is canceled, so the sample has to be taken inside the seam.
type budgetCtxCall struct {
	beadID string
	ctxErr error
}

// bdDeadlineClaim wraps a Claim seam the way bd behaves at the CAS: a claim
// started on an already-spent context fails with the caller-deadline error
// the incident showed and mutates nothing. Every attempt is recorded.
func bdDeadlineClaim(base hookClaimFunc, calls *[]budgetCtxCall) hookClaimFunc {
	return func(ctx context.Context, dir string, env []string, id, assignee string) (beads.Bead, bool, error) {
		*calls = append(*calls, budgetCtxCall{beadID: id, ctxErr: ctx.Err()})
		if ctx.Err() != nil {
			return beads.Bead{}, false, fmt.Errorf("claiming bead %q: timed out after 0s (caller deadline)", id)
		}
		return base(ctx, dir, env, id, assignee)
	}
}

// hookClaimBudgetEmissions records the typed-event emissions the budget tests
// assert on, so no test depends on the production emitters reaching anything.
type hookClaimBudgetEmissions struct {
	windowExpired  int
	claimFailed    int
	reclaimedStale [][3]string
}

func (e *hookClaimBudgetEmissions) install(ops *hookClaimOps) {
	ops.EmitClaimWindowExpired = func(hookClaimWindowExpiry) { e.windowExpired++ }
	ops.EmitClaimFailed = func(string, hookClaimOptions, error) { e.claimFailed++ }
	ops.EmitHookClaimReclaimedStale = func(beadID, previousOwner, newAssignee string) {
		e.reclaimedStale = append(e.reclaimedStale, [3]string{beadID, previousOwner, newAssignee})
	}
}

// runBudgetClaim drives one `gc hook --claim` and returns its exit code, the
// bead id it handed out (empty when it handed out nothing), and stderr.
func runBudgetClaim(t *testing.T, opts hookClaimOptions, ops hookClaimOps) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", t.TempDir(), opts, ops, &stdout, &stderr)
	handedOut := ""
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		var result hookClaimJSONResult
		if json.Unmarshal([]byte(line), &result) == nil && result.Action == "work" {
			handedOut = result.BeadID
		}
	}
	return code, handedOut, stderr.String()
}

func budgetClaimOpts() hookClaimOptions {
	return hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		DrainAck:           true,
		JSON:               true,
	}
}

// TestHookClaimCASKeepsItsBudgetWhenStopGateSpendsItsOwn covers AC-1: the
// pre-CAS stop gate's ancestry read spends its whole budget before answering,
// and the claim CAS that follows must still run on an unexpired budget of its
// own and hand the live step out. On the shared-context base the same
// invocation failed the CAS with the caller-deadline error: the routed tier
// skipped the step into a claims_errored drain, and the ready tier exited 1
// on "promoting ready assignment".
func TestHookClaimCASKeepsItsBudgetWhenStopGateSpendsItsOwn(t *testing.T) {
	shortenHookClaimBudget(t)
	for _, tc := range []struct {
		name     string
		assignee string
	}{
		{name: "routed"},
		{name: "ready assignment", assignee: "worker-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHookWorkflowStopFixture(t)
			live := f.root("live run", "")
			step := f.step("implement", live.ID, nil)
			if tc.assignee != "" {
				assignee := tc.assignee
				if err := f.store.Update(step.ID, beads.UpdateOpts{Assignee: &assignee}); err != nil {
					t.Fatal(err)
				}
			}
			f.served = []string{step.ID}

			ops := f.ops()
			// The gate's ancestry read of the live root finishes only after
			// the gate's whole budget is spent.
			ops.WorkflowStore = func(ctx context.Context, _ string, _ []string, _, _ string) beads.Store {
				return budgetSpendingStore{Store: f.store, ctx: ctx, slowIDs: map[string]bool{live.ID: true}}
			}
			var casCalls []budgetCtxCall
			ops.Claim = bdDeadlineClaim(ops.Claim, &casCalls)
			var emissions hookClaimBudgetEmissions
			emissions.install(&ops)

			code, got, stderr := runBudgetClaim(t, budgetClaimOpts(), ops)

			if code != 0 || got != step.ID {
				t.Fatalf("doHookClaim = %d handing out %q, want 0 handing out %s; the stop gate starved the claim CAS (claims=%v)\nstderr=%s",
					code, got, step.ID, f.claims, stderr)
			}
			if len(f.claims) != 1 || f.claims[0] != step.ID {
				t.Fatalf("claim CASes = %v, want exactly [%s]", f.claims, step.ID)
			}
			for _, call := range casCalls {
				if call.ctxErr != nil {
					t.Fatalf("claim CAS for %s started on a spent context: %v", call.beadID, call.ctxErr)
				}
			}
			if strings.Contains(stderr, "caller deadline") || strings.Contains(stderr, "deadline exhausted") {
				t.Fatalf("stderr reports the budget starvation the per-operation budgets exist to prevent: %s", stderr)
			}
			served := f.get(step.ID)
			if served.Status != "in_progress" || served.Assignee != "worker-1" {
				t.Fatalf("step = status %q assignee %q, want in_progress worker-1", served.Status, served.Assignee)
			}
		})
	}
}

// TestHookClaimLaterCandidateKeepsBudgetAfterEarlierStopGate covers AC-2: a
// first candidate's stop gate spends its whole budget closing it as stopped,
// and the live candidate behind it must still be gated and claimed on fresh
// budgets. On the shared-context base the fresh tier broke out of the loop
// into a no_work drain and the ready tier exited 1 with "claim deadline
// exhausted", stranding the live work.
func TestHookClaimLaterCandidateKeepsBudgetAfterEarlierStopGate(t *testing.T) {
	shortenHookClaimBudget(t)
	for _, tc := range []struct {
		name     string
		assignee string
	}{
		{name: "routed"},
		{name: "ready assignment", assignee: "worker-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHookWorkflowStopFixture(t)
			failed := f.root("stopped run", "")
			stranded := f.step("implement", failed.ID, nil)
			f.closeAs(failed.ID, "fail", nil)
			live := f.root("live run", "")
			next := f.step("next", live.ID, nil)
			if tc.assignee != "" {
				for _, id := range []string{stranded.ID, next.ID} {
					assignee := tc.assignee
					if err := f.store.Update(id, beads.UpdateOpts{Assignee: &assignee}); err != nil {
						t.Fatal(err)
					}
				}
			}
			f.served = []string{stranded.ID, next.ID}

			ops := f.ops()
			// Both gates' ancestry reads spend their whole budget before
			// answering.
			ops.WorkflowStore = func(ctx context.Context, _ string, _ []string, _, _ string) beads.Store {
				return budgetSpendingStore{Store: f.store, ctx: ctx, slowIDs: map[string]bool{failed.ID: true, live.ID: true}}
			}
			var casCalls []budgetCtxCall
			ops.Claim = bdDeadlineClaim(ops.Claim, &casCalls)
			var emissions hookClaimBudgetEmissions
			emissions.install(&ops)

			code, got, stderr := runBudgetClaim(t, budgetClaimOpts(), ops)

			if code != 0 || got != next.ID {
				t.Fatalf("doHookClaim = %d handing out %q, want 0 handing out the live step %s behind the stopped one; claims=%v\nstderr=%s",
					code, got, next.ID, f.claims, stderr)
			}
			f.wantNeverClaimed(stranded.ID)
			f.wantClosed(stranded.ID, "skipped")
			for _, call := range casCalls {
				if call.ctxErr != nil {
					t.Fatalf("claim CAS for %s started on a spent context: %v", call.beadID, call.ctxErr)
				}
			}
		})
	}
}

// TestHookClaimReclaimKeepsItsBudgetAfterEarlierStopGate covers AC-3: with
// AutoReclaimStaleClaims on, a reclaim-eligible candidate reached after an
// earlier candidate's stop gate spent its whole budget must get its reclaim
// and its claim CAS on unexpired budgets of their own, and the reclaimed
// candidate is handed out. On the shared-context base the spent-budget break
// before the reclaim stopped the loop before the reclaim ever ran.
func TestHookClaimReclaimKeepsItsBudgetAfterEarlierStopGate(t *testing.T) {
	shortenHookClaimBudget(t)
	f := newHookWorkflowStopFixture(t)
	failed := f.root("stopped run", "")
	stranded := f.step("implement", failed.ID, nil)
	f.closeAs(failed.ID, "fail", nil)
	live := f.root("live run", "")
	stale := f.step("retry me", live.ID, nil)
	staleAssignee := "dead-worker"
	if err := f.store.Update(stale.ID, beads.UpdateOpts{Assignee: &staleAssignee}); err != nil {
		t.Fatal(err)
	}
	f.served = []string{stranded.ID, stale.ID}

	ops := f.ops()
	ops.WorkflowStore = func(ctx context.Context, _ string, _ []string, _, _ string) beads.Store {
		return budgetSpendingStore{Store: f.store, ctx: ctx, slowIDs: map[string]bool{failed.ID: true}}
	}
	var reclaimCalls []budgetCtxCall
	ops.ReclaimStale = func(ctx context.Context, _ string, _ []string, beadID string) (bool, string, error) {
		reclaimCalls = append(reclaimCalls, budgetCtxCall{beadID: beadID, ctxErr: ctx.Err()})
		if ctx.Err() != nil {
			return false, "", ctx.Err()
		}
		none := ""
		if err := f.store.Update(beadID, beads.UpdateOpts{Assignee: &none}); err != nil {
			return false, "", err
		}
		return true, staleAssignee, nil
	}
	var casCalls []budgetCtxCall
	ops.Claim = bdDeadlineClaim(ops.Claim, &casCalls)
	var emissions hookClaimBudgetEmissions
	emissions.install(&ops)

	opts := budgetClaimOpts()
	opts.AutoReclaimStaleClaims = true
	code, got, stderr := runBudgetClaim(t, opts, ops)

	if code != 0 || got != stale.ID {
		t.Fatalf("doHookClaim = %d handing out %q, want 0 handing out the reclaimed step %s; claims=%v\nstderr=%s",
			code, got, stale.ID, f.claims, stderr)
	}
	if len(reclaimCalls) != 1 || reclaimCalls[0].beadID != stale.ID {
		t.Fatalf("reclaim calls = %v, want exactly one on %s", reclaimCalls, stale.ID)
	}
	if err := reclaimCalls[0].ctxErr; err != nil {
		t.Fatalf("reclaim for %s started on a spent context: %v", reclaimCalls[0].beadID, err)
	}
	for _, call := range casCalls {
		if call.ctxErr != nil {
			t.Fatalf("claim CAS for %s started on a spent context: %v", call.beadID, call.ctxErr)
		}
	}
	if len(emissions.reclaimedStale) != 1 || emissions.reclaimedStale[0] != [3]string{stale.ID, staleAssignee, "worker-1"} {
		t.Fatalf("reclaimed_stale emissions = %v, want exactly one naming %s reclaimed from %s for worker-1",
			emissions.reclaimedStale, stale.ID, staleAssignee)
	}
	f.wantNeverClaimed(stranded.ID)
	f.wantClosed(stranded.ID, "skipped")
}

// deadlineWorkflowStore fails a read whose context is already spent, the way
// bd fails a read started after its caller deadline, and serves every other
// read normally.
type deadlineWorkflowStore struct {
	beads.Store
	ctx context.Context
}

func (s deadlineWorkflowStore) Get(id string) (beads.Bead, error) {
	if err := s.ctx.Err(); err != nil {
		return beads.Bead{}, fmt.Errorf("getting %s: %w", id, err)
	}
	return s.Store.Get(id)
}

// TestHookClaimPostCASRecheckHasItsOwnBudget covers AC-4: the claim CAS spends
// its whole budget while the workflow root settles under it, and the post-CAS
// re-check must read on an unexpired budget of its own, detect the stop and
// close the claim as skipped rather than hand it out. On the shared-context
// base the re-check read on the CAS's expired context, WorkflowStopReason
// failed open, and the stopped step was served.
func TestHookClaimPostCASRecheckHasItsOwnBudget(t *testing.T) {
	shortenHookClaimBudget(t)
	for _, tc := range []struct {
		name     string
		assignee string
	}{
		{name: "routed"},
		{name: "ready assignment", assignee: "worker-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHookWorkflowStopFixture(t)
			root := f.root("do-work", "")
			step := f.step("implement", root.ID, nil)
			if tc.assignee != "" {
				assignee := tc.assignee
				if err := f.store.Update(step.ID, beads.UpdateOpts{Assignee: &assignee}); err != nil {
					t.Fatal(err)
				}
			}
			f.served = []string{step.ID}

			ops := f.ops()
			// The gate reads fine on a live budget and fail on a spent one,
			// the way bd does.
			ops.WorkflowStore = func(ctx context.Context, _ string, _ []string, _, _ string) beads.Store {
				return deadlineWorkflowStore{Store: f.store, ctx: ctx}
			}
			// The CAS commits, the root settles under it, and the CAS then
			// spends its whole budget before returning.
			baseClaim := ops.Claim
			ops.Claim = func(ctx context.Context, dir string, env []string, id, assignee string) (beads.Bead, bool, error) {
				claimed, ok, err := baseClaim(ctx, dir, env, id, assignee)
				if err != nil || !ok {
					return claimed, ok, err
				}
				f.closeAs(root.ID, "fail", nil)
				<-ctx.Done()
				return claimed, ok, err
			}
			var emissions hookClaimBudgetEmissions
			emissions.install(&ops)

			code, got, stderr := runBudgetClaim(t, budgetClaimOpts(), ops)

			if got != "" {
				t.Fatalf("doHookClaim = %d handed out %q, want nothing: the root settled under the claim (claims=%v)\nstderr=%s",
					code, got, f.claims, stderr)
			}
			f.wantClosed(step.ID, "skipped")
			if len(f.claims) != 1 || f.claims[0] != step.ID {
				t.Fatalf("claim CASes = %v, want exactly [%s]", f.claims, step.ID)
			}
		})
	}
}
