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

// The tests in this file pin #9178: every store operation of a claim tier —
// the pre-CAS workflow stop gate, the stale reclaim, the claim CAS and the
// post-CAS re-check — runs on a budget of its own, so a slow ancestry read
// cannot leave a later operation an expired context. They shorten the package
// mutation budget, so none of them run in parallel.

// hookClaimTestBudget is the shortened claim budget these tests run with: long
// enough that an operation started on a fresh budget finishes inside it, short
// enough that spending a whole budget costs the suite little.
const hookClaimTestBudget = 300 * time.Millisecond

// shortenHookClaimBudget sets the reserved claim budget for one test.
func shortenHookClaimBudget(t *testing.T, d time.Duration) {
	t.Helper()
	old := hookClaimMutationTimeout
	hookClaimMutationTimeout = d
	t.Cleanup(func() { hookClaimMutationTimeout = old })
}

// budgetSpendingStore is the workflow store a stop gate opens, bound to the
// context the gate opened it with. A read of a slow id finishes only once that
// context is done — an ancestry read that spends the gate's whole budget — and
// then returns the row.
type budgetSpendingStore struct {
	beads.Store
	ctx  context.Context
	slow map[string]bool
}

func (s budgetSpendingStore) Get(id string) (beads.Bead, error) {
	if s.slow[id] {
		<-s.ctx.Done()
	}
	return s.Store.Get(id)
}

// deadlineHonoringStore is a workflow store that fails every read once the
// context it was opened with is done, as a bd child bound to that context does.
type deadlineHonoringStore struct {
	beads.Store
	ctx context.Context
}

func (s deadlineHonoringStore) Get(id string) (beads.Bead, error) {
	if err := s.ctx.Err(); err != nil {
		return beads.Bead{}, fmt.Errorf("showing bead %q: timed out after 0s (caller deadline): %w", id, err)
	}
	return s.Store.Get(id)
}

// budgetClaimRecorder records the context state each claim write saw.
type budgetClaimRecorder struct {
	claimExpired   []bool
	reclaimExpired []bool
	reclaimedStale []string
}

// bdLikeClaim wraps claim the way bd honors the caller deadline: a CAS started
// on an expired context fails without writing.
func (r *budgetClaimRecorder) bdLikeClaim(claim hookClaimFunc) hookClaimFunc {
	return func(ctx context.Context, dir string, env []string, id, assignee string) (beads.Bead, bool, error) {
		expired := ctx.Err() != nil
		r.claimExpired = append(r.claimExpired, expired)
		if expired {
			return beads.Bead{}, false, fmt.Errorf("claiming bead %q: timed out after 0s (caller deadline)", id)
		}
		return claim(ctx, dir, env, id, assignee)
	}
}

// reclaimFrom reverts a stale lease in f's store the way bd's reclaim does,
// refusing like bd when the context it was handed is already spent.
func (r *budgetClaimRecorder) reclaimFrom(f *hookWorkflowStopFixture) hookClaimReclaimFunc {
	return func(ctx context.Context, _ string, _ []string, id string) (bool, string, error) {
		expired := ctx.Err() != nil
		r.reclaimExpired = append(r.reclaimExpired, expired)
		if expired {
			return false, "", fmt.Errorf("reclaiming bead %q: timed out after 0s (caller deadline)", id)
		}
		current, err := f.store.Get(id)
		if err != nil {
			return false, "", err
		}
		none := ""
		if err := f.store.Update(id, beads.UpdateOpts{Assignee: &none}); err != nil {
			return false, "", err
		}
		return true, current.Assignee, nil
	}
}

// slowStopGate makes every stop-gate read of the slow ids spend the gate's
// whole budget.
func slowStopGate(f *hookWorkflowStopFixture, slow ...string) hookWorkflowStoreFunc {
	ids := map[string]bool{}
	for _, id := range slow {
		ids[id] = true
	}
	return func(ctx context.Context, _ string, _ []string, _, _ string) beads.Store {
		return budgetSpendingStore{Store: f.store, ctx: ctx, slow: ids}
	}
}

// budgetClaimRun is one `gc hook --claim` invocation's observable result.
type budgetClaimRun struct {
	code   int
	handed string
	reason string
	stdout string
	stderr string
}

func runBudgetClaim(t *testing.T, ops hookClaimOps, autoReclaim bool) budgetClaimRun {
	t.Helper()
	if ops.EmitClaimFailed == nil {
		ops.EmitClaimFailed = func(string, hookClaimOptions, error) {}
	}
	if ops.EmitClaimWindowExpired == nil {
		ops.EmitClaimWindowExpired = func(hookClaimWindowExpiry) {}
	}
	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", t.TempDir(), hookClaimOptions{
		Assignee:               "worker-1",
		IdentityCandidates:     []string{"worker-1"},
		RouteTargets:           []string{"worker"},
		DrainAck:               true,
		JSON:                   true,
		AutoReclaimStaleClaims: autoReclaim,
	}, ops, &stdout, &stderr)
	run := budgetClaimRun{code: code, stdout: stdout.String(), stderr: stderr.String()}
	for _, line := range strings.Split(strings.TrimSpace(run.stdout), "\n") {
		var result hookClaimJSONResult
		if json.Unmarshal([]byte(line), &result) != nil {
			continue
		}
		run.reason = result.Reason
		if result.Action == "work" {
			run.handed = result.BeadID
		}
	}
	return run
}

// assign pins id to assignee, the shape of a ready assignment.
func (f *hookWorkflowStopFixture) assign(id, assignee string) {
	f.t.Helper()
	if err := f.store.Update(id, beads.UpdateOpts{Assignee: &assignee}); err != nil {
		f.t.Fatalf("assign %s: %v", id, err)
	}
}

// claimTiers are the two tiers that run a claim CAS: a routed candidate
// reaches the fresh tier, one already assigned to this session the ready tier.
var claimTiers = []struct {
	name     string
	assignee string
	reason   string
}{
	{name: "routed", reason: "claimed"},
	{name: "ready assignment", assignee: "worker-1", reason: "ready_assignment"},
}

func wantHandedOut(t *testing.T, f *hookWorkflowStopFixture, run budgetClaimRun, id, reason string) {
	t.Helper()
	if run.code != 0 || run.handed != id || run.reason != reason {
		t.Fatalf("doHookClaim = %d handed out %q (%s), want 0 handing out %s (%s); claims=%v\nstdout=%s\nstderr=%s",
			run.code, run.handed, run.reason, id, reason, f.claims, run.stdout, run.stderr)
	}
	for _, starved := range []string{"caller deadline", "deadline exhausted"} {
		if strings.Contains(run.stderr, starved) {
			t.Fatalf("stderr reports %q: a claim write ran on a spent budget\nstderr=%s", starved, run.stderr)
		}
	}
	got := f.get(id)
	if got.Status != "in_progress" || got.Assignee != "worker-1" {
		t.Fatalf("%s = status %q assignee %q, want in_progress worker-1", id, got.Status, got.Assignee)
	}
}

func wantEveryClaimUnexpired(t *testing.T, rec *budgetClaimRecorder) {
	t.Helper()
	if len(rec.claimExpired) == 0 {
		t.Fatal("no claim CAS ran")
	}
	for i, expired := range rec.claimExpired {
		if expired {
			t.Fatalf("claim CAS %d ran on an expired context (claims expired=%v)", i, rec.claimExpired)
		}
	}
}

// TestHookClaimCASKeepsItsBudgetWhenStopGateSpendsItsOwn (AC-1): a stop gate
// whose ancestry read spends its whole budget on a live root must leave the
// claim CAS an unexpired budget of its own, so the candidate is handed out.
func TestHookClaimCASKeepsItsBudgetWhenStopGateSpendsItsOwn(t *testing.T) {
	for _, tier := range claimTiers {
		t.Run(tier.name, func(t *testing.T) {
			shortenHookClaimBudget(t, hookClaimTestBudget)
			f := newHookWorkflowStopFixture(t)
			live := f.root("live run", "")
			step := f.step("implement", live.ID, nil)
			if tier.assignee != "" {
				f.assign(step.ID, tier.assignee)
			}
			f.served = []string{step.ID}

			rec := &budgetClaimRecorder{}
			ops := f.ops()
			ops.WorkflowStore = slowStopGate(f, live.ID)
			ops.Claim = rec.bdLikeClaim(ops.Claim)

			run := runBudgetClaim(t, ops, false)

			wantHandedOut(t, f, run, step.ID, tier.reason)
			wantEveryClaimUnexpired(t, rec)
		})
	}
}

// TestHookClaimLaterCandidateKeepsBudgetAfterEarlierStopGate (AC-2): the first
// candidate's stop gate spends its whole budget and closes it under a failed
// root; the live candidate behind it is gated and claimed on fresh budgets
// rather than abandoned as if the tier's time were gone. The live candidate's
// own gate spends a whole budget too, so its CAS must start on a budget of its
// own as well.
func TestHookClaimLaterCandidateKeepsBudgetAfterEarlierStopGate(t *testing.T) {
	for _, tier := range claimTiers {
		t.Run(tier.name, func(t *testing.T) {
			shortenHookClaimBudget(t, hookClaimTestBudget)
			f := newHookWorkflowStopFixture(t)
			failed := f.root("failed run", "")
			stopped := f.step("implement", failed.ID, nil)
			f.closeAs(failed.ID, "fail", nil)
			live := f.root("live run", "")
			next := f.step("implement", live.ID, nil)
			if tier.assignee != "" {
				f.assign(stopped.ID, tier.assignee)
				f.assign(next.ID, tier.assignee)
			}
			f.served = []string{stopped.ID, next.ID}

			rec := &budgetClaimRecorder{}
			ops := f.ops()
			ops.WorkflowStore = slowStopGate(f, failed.ID, live.ID)
			ops.Claim = rec.bdLikeClaim(ops.Claim)

			run := runBudgetClaim(t, ops, false)

			wantHandedOut(t, f, run, next.ID, tier.reason)
			wantEveryClaimUnexpired(t, rec)
			f.wantClosed(stopped.ID, "skipped")
			f.wantNeverClaimed(stopped.ID)
		})
	}
}

// TestHookClaimReclaimKeepsItsBudgetAfterEarlierStopGate (AC-3): after the
// first candidate's stop gate spends its whole budget, the fresh tier still
// reclaims the stale lease behind it and claims it, each write on an unexpired
// budget of its own.
func TestHookClaimReclaimKeepsItsBudgetAfterEarlierStopGate(t *testing.T) {
	shortenHookClaimBudget(t, hookClaimTestBudget)
	f := newHookWorkflowStopFixture(t)
	failed := f.root("failed run", "")
	stopped := f.step("implement", failed.ID, nil)
	f.closeAs(failed.ID, "fail", nil)
	live := f.root("live run", "")
	stale := f.step("implement", live.ID, nil)
	f.assign(stale.ID, "dead-worker")
	f.served = []string{stopped.ID, stale.ID}

	rec := &budgetClaimRecorder{}
	ops := f.ops()
	ops.WorkflowStore = slowStopGate(f, failed.ID)
	ops.Claim = rec.bdLikeClaim(ops.Claim)
	ops.ReclaimStale = rec.reclaimFrom(f)
	ops.EmitHookClaimReclaimedStale = func(beadID, previousOwner, _ string) {
		rec.reclaimedStale = append(rec.reclaimedStale, beadID+"<-"+previousOwner)
	}

	run := runBudgetClaim(t, ops, true)

	wantHandedOut(t, f, run, stale.ID, "claimed")
	wantEveryClaimUnexpired(t, rec)
	if len(rec.reclaimExpired) != 1 || rec.reclaimExpired[0] {
		t.Fatalf("reclaim contexts expired = %v, want one reclaim on an unexpired budget", rec.reclaimExpired)
	}
	if want := []string{stale.ID + "<-dead-worker"}; len(rec.reclaimedStale) != 1 || rec.reclaimedStale[0] != want[0] {
		t.Fatalf("reclaimed_stale events = %v, want %v", rec.reclaimedStale, want)
	}
	f.wantClosed(stopped.ID, "skipped")
	f.wantNeverClaimed(stopped.ID)
}

// TestHookClaimPostCASRecheckHasItsOwnBudget (AC-4): the workflow root settles
// while the claim CAS is in flight and the CAS spends its whole budget. The
// re-check across the CAS must read on an unexpired budget of its own, see the
// stop and close the claim, not fail open and hand the step out.
func TestHookClaimPostCASRecheckHasItsOwnBudget(t *testing.T) {
	for _, tier := range claimTiers {
		t.Run(tier.name, func(t *testing.T) {
			shortenHookClaimBudget(t, hookClaimTestBudget)
			f := newHookWorkflowStopFixture(t)
			root := f.root("do-work", "")
			step := f.step("implement", root.ID, nil)
			if tier.assignee != "" {
				f.assign(step.ID, tier.assignee)
			}
			f.served = []string{step.ID}
			f.afterClaim = func(string) { f.closeAs(root.ID, "fail", nil) }

			rec := &budgetClaimRecorder{}
			ops := f.ops()
			ops.WorkflowStore = func(ctx context.Context, _ string, _ []string, _, _ string) beads.Store {
				return deadlineHonoringStore{Store: f.store, ctx: ctx}
			}
			baseClaim := ops.Claim
			// The CAS commits, then returns only once its whole budget is spent.
			ops.Claim = rec.bdLikeClaim(func(ctx context.Context, dir string, env []string, id, assignee string) (beads.Bead, bool, error) {
				claimed, ok, err := baseClaim(ctx, dir, env, id, assignee)
				<-ctx.Done()
				return claimed, ok, err
			})

			run := runBudgetClaim(t, ops, false)

			if run.handed != "" {
				t.Fatalf("handed out %q, want nothing once the root settled under the claim\nstdout=%s\nstderr=%s", run.handed, run.stdout, run.stderr)
			}
			if strings.Contains(run.stderr, "workflow stop check unavailable") {
				t.Fatalf("the re-check failed open on a spent budget\nstderr=%s", run.stderr)
			}
			wantEveryClaimUnexpired(t, rec)
			f.wantClosed(step.ID, "skipped")
		})
	}
}

// TestHookClaimPreCASGateCarryingLandingPastReserveIsUnwound (AC-6): the stop
// gate no longer spends the CAS's budget, so a slow pre-CAS gate can carry a
// claim that passed the window fence to a landing past window + reserve. The
// straddle fence still releases it rather than parking it on a gone turn.
func TestHookClaimPreCASGateCarryingLandingPastReserveIsUnwound(t *testing.T) {
	const window = time.Minute
	for _, tier := range claimTiers {
		t.Run(tier.name, func(t *testing.T) {
			f := newHookWorkflowStopFixture(t)
			live := f.root("live run", "")
			step := f.step("implement", live.ID, nil)
			if tier.assignee != "" {
				f.assign(step.ID, tier.assignee)
			}
			f.served = []string{step.ID}

			invokedAt := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
			clock := invokedAt
			claimed := false
			ops := f.ops()
			ops.Now = func() time.Time { return clock }
			ops.InvokedAt = invokedAt
			ops.ClaimWindow = window
			baseClaim := ops.Claim
			ops.Claim = func(ctx context.Context, dir string, env []string, id, assignee string) (beads.Bead, bool, error) {
				claimed = true
				return baseClaim(ctx, dir, env, id, assignee)
			}
			// The pre-CAS gate's reads carry the invocation past window + reserve.
			ops.WorkflowStore = func(context.Context, string, []string, string, string) beads.Store {
				return slowWorkflowStore{Store: f.store, charge: func() {
					if !claimed {
						clock = invokedAt.Add(window + hookClaimMutationTimeout + time.Second)
					}
				}}
			}

			run := runBudgetClaim(t, ops, false)

			if !claimed {
				t.Fatalf("no claim CAS ran; the window fence must pass before the gate\nstderr=%s", run.stderr)
			}
			if run.code != 1 || run.handed != "" || !strings.Contains(run.stderr, "claim window") {
				t.Fatalf("doHookClaim = %d handed out %q, want exit 1 with the straddle unwind\nstdout=%s\nstderr=%s", run.code, run.handed, run.stdout, run.stderr)
			}
			if got := f.get(step.ID); got.Status != "open" || got.Assignee != "" {
				t.Fatalf("step = status %q assignee %q, want the straddling claim released", got.Status, got.Assignee)
			}
		})
	}
}

// TestHookClaimFreshTierSpentBudgetAttemptsNoCAS (AC-5): with every claim
// budget spent at creation, the fresh tier refuses to start a CAS on routed
// work it cannot finish in time and keeps today's no_work drain; the ids stay
// open for the next poll.
func TestHookClaimFreshTierSpentBudgetAttemptsNoCAS(t *testing.T) {
	shortenHookClaimBudget(t, 0)
	f := newHookWorkflowStopFixture(t)
	live := f.root("live run", "")
	first := f.step("implement", live.ID, nil)
	second := f.step("review", live.ID, nil)
	f.served = []string{first.ID, second.ID}

	ops := f.ops()
	ops.Claim = func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, bool, error) {
		t.Fatalf("claim CAS ran for %s on a budget spent at creation", id)
		return beads.Bead{}, false, nil
	}

	run := runBudgetClaim(t, ops, false)

	if run.code != 0 || run.handed != "" || run.reason != hookClaimReasonNoWork {
		t.Fatalf("doHookClaim = %d handed out %q (%s), want 0 with the %s drain and no CAS\nstdout=%s\nstderr=%s",
			run.code, run.handed, run.reason, hookClaimReasonNoWork, run.stdout, run.stderr)
	}
	for _, id := range []string{first.ID, second.ID} {
		if got := f.get(id); got.Status != "open" || got.Assignee != "" {
			t.Fatalf("%s = status %q assignee %q, want open and unassigned", id, got.Status, got.Assignee)
		}
	}
}
