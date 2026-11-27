package main

import (
	"context"
	"fmt"
	"time"
)

// errHookClaimReadBudgetSpent marks a claim-path work-query read cut by the
// invocation's read budget: a read that would start after the read deadline, or
// whose answer arrived at or after it. It wraps context.DeadlineExceeded, so the
// cut classifies exactly like a timed-out read (exit 2 with the
// store-unavailable token, no drain).
var errHookClaimReadBudgetSpent = fmt.Errorf("the claim read budget is spent: %w", context.DeadlineExceeded)

// claimReadReserve is the slice of the claim window held back from the reads
// for the claim mutation that follows them: hookClaimMutationTimeout, or half
// the window when an override makes the window shorter than twice that, so the
// reserve never eats the whole window.
func (ops *hookClaimOps) claimReadReserve() time.Duration {
	return min(hookClaimMutationTimeout, ops.claimWindowOrDefault()/2)
}

// claimReadBudget is how much of the claim window every claim-path read of one
// invocation shares: the window less the mutation reserve.
//
// The window is one budget anchored at InvokedAt, while each read gets its own
// fresh hookWorkQueryTimeout. Without a shared read budget a fan-out of slow
// legs could spend the whole window reading and then refuse the work it found
// as "the claim window is spent".
func (ops *hookClaimOps) claimReadBudget() time.Duration {
	return ops.claimWindowOrDefault() - ops.claimReadReserve()
}

// claimReadDeadline is the instant the invocation's claim reads must finish by
// (InvokedAt + claimReadBudget), and false when the caller opened no invocation
// window (zero InvokedAt), which has no read budget.
func (ops *hookClaimOps) claimReadDeadline() (time.Time, bool) {
	if ops.InvokedAt.IsZero() {
		return time.Time{}, false
	}
	return ops.InvokedAt.Add(ops.claimReadBudget()), true
}

// claimReadBudgetSpentAfter reports whether the read budget is spent, or runs
// out exactly then, once a further wait of d has elapsed. The boundary counts
// as spent, as for claimWindowSpentAfter. It never fires without an invocation
// window.
func (ops *hookClaimOps) claimReadBudgetSpentAfter(d time.Duration) bool {
	deadline, ok := ops.claimReadDeadline()
	if !ok {
		return false
	}
	return !ops.nowOrWallClock().Add(d).Before(deadline)
}

// claimReadContext is the parent context for the invocation's production
// work-query subprocesses: its deadline is the read deadline, with
// errHookClaimReadBudgetSpent as the cause, so a read still running when the
// budget runs out is killed there rather than at its own, later,
// hookWorkQueryTimeout. Without an invocation window it carries no deadline.
func (ops *hookClaimOps) claimReadContext() (context.Context, context.CancelFunc) {
	deadline, ok := ops.claimReadDeadline()
	if !ok {
		return context.WithCancel(context.Background())
	}
	return context.WithDeadlineCause(context.Background(), deadline, errHookClaimReadBudgetSpent)
}

// hookClaimReadGuard holds every claim-path read of one invocation to its read
// budget, whatever runner performs it. It is the enforcement that holds for any
// runner, a fake one included; claimReadContext additionally kills a production
// subprocess at the deadline.
type hookClaimReadGuard struct {
	ops *hookClaimOps
	run hookStoreRunner
}

// newHookClaimReadGuard wraps run for the invocation ops describes.
func newHookClaimReadGuard(ops *hookClaimOps, run hookStoreRunner) *hookClaimReadGuard {
	return &hookClaimReadGuard{ops: ops, run: run}
}

// read is the guarded runner. A read never starts once the budget is spent,
// and a successful answer that arrived at or after the deadline is that
// timed-out read: work it carries cannot reach a CAS inside the window. A read
// that failed keeps its own error.
func (g *hookClaimReadGuard) read(command, dir string, env []string) (string, error) {
	if g.ops.claimReadBudgetSpentAfter(0) {
		return "", g.budgetError(command, "not started")
	}
	out, err := g.run(command, dir, env)
	if err == nil && g.ops.claimReadBudgetSpentAfter(0) {
		return "", g.budgetError(command, "answered past the deadline")
	}
	return out, err
}

// budgetError is the error for a read the budget cut. It keeps the "timed out
// after" phrase the kill classifier names a timeout by.
func (g *hookClaimReadGuard) budgetError(command, how string) error {
	return fmt.Errorf("running work query %q: timed out after %s (%s; claim read budget of the %s claim window): %w",
		command, g.ops.claimReadBudget(), how, g.ops.claimWindowOrDefault(), errHookClaimReadBudgetSpent)
}
