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
