package main

import (
	"context"
	"fmt"
	"time"
)

// The invocation read budget (issue #5693).
//
// hookClaimWindowDefault was derived for ONE work-query read
// (hookWorkQueryTimeout, 150s) plus the claim CAS (hookClaimMutationTimeout,
// 10s) — but the claim path made several reads: every leg of the fan-out, paced
// retries, each with its own fresh hookWorkQueryTimeout. Under host load, where
// one read can cost most of the timeout, the reads together spent the window
// and the CAS was then refused with "the claim window is spent" on every
// patrol — routed unassigned beads starved for hours while Dolt answered in
// under 200ms and a manual `bd update --claim` (one CAS, no read phase) took
// the same rows instantly.
//
// The budget gives the whole invocation ONE read share of the window: every
// claim-path read must finish by InvokedAt+ClaimWindow−reserve, where the
// reserve keeps the CAS its full mutation budget. A read phase that cannot fit
// fails as what it is — a timed-out read (exit 2, GC_HOOK_STORE_UNAVAILABLE, a
// work-query-failure event) — instead of being laundered into a window refusal
// of work the reads already found.

// hookClaimReadReserve is the part of the claim window held back from reads so
// the claim CAS keeps its full mutation budget: hookClaimMutationTimeout,
// capped at half the window so a small GC_HOOK_CLAIM_WINDOW override can never
// shrink the read share to nothing.
func hookClaimReadReserve(window time.Duration) time.Duration {
	reserve := hookClaimMutationTimeout
	if half := window / 2; half < reserve {
		reserve = half
	}
	return reserve
}

// readDeadline is the instant claim-path reads must finish by:
// InvokedAt + ClaimWindow − reserve. ok is false for a caller that opened no
// invocation window (zero InvokedAt): it has no read budget, only the per-read
// cap hookWorkQueryTimeout imposes.
func (ops *hookClaimOps) readDeadline() (time.Time, bool) {
	if ops.InvokedAt.IsZero() {
		return time.Time{}, false
	}
	window := ops.claimWindowOrDefault()
	return ops.InvokedAt.Add(window - hookClaimReadReserve(window)), true
}

// readBudgetSpentAfter reports whether the invocation read deadline is reached,
// or closes exactly then, once a further wait of d has elapsed. The retry loop
// consults it before a pacing sleep and again after it, so a sleep cannot carry
// a read past the read deadline; the boundary counts as spent, like
// claimWindowSpentAfter. Never fires for a caller that opened no invocation
// window.
func (ops *hookClaimOps) readBudgetSpentAfter(d time.Duration) bool {
	deadline, ok := ops.readDeadline()
	if !ok {
		return false
	}
	return !ops.nowOrWallClock().Add(d).Before(deadline)
}

// claimReadContext is the parent context production claim-path reads run
// under: bounded by the invocation read deadline, so a work-query subprocess is
// cut at the deadline however long its own hookWorkQueryTimeout would let it
// run. A caller that opened no window gets an unbounded context and keeps the
// per-read cap alone.
func (ops *hookClaimOps) claimReadContext() (context.Context, context.CancelFunc) {
	if deadline, ok := ops.readDeadline(); ok {
		return context.WithDeadline(context.Background(), deadline)
	}
	return context.WithCancel(context.Background())
}

// hookClaimReadGuard enforces the invocation read budget at the runner seam, so
// every read the claim path makes — every fan-out leg, every paced retry —
// shares it, whatever runner is injected:
//
//   - once the budget is spent, no read starts: the guard refuses without
//     spawning a process;
//   - a successful answer that arrives at or after the read deadline counts as
//     a timed-out read (this also covers the subprocess WaitDelay tail); an
//     error answer keeps its own error.
//
// The refusal wraps context.DeadlineExceeded, so classification treats it
// exactly like a read whose own hookWorkQueryTimeout fired: the primary leg's
// failure exits 2 with GC_HOOK_STORE_UNAVAILABLE, and a secondary leg's failure
// ends the invocation through hookSecondaryStoreFailure — a timed-out read, not
// an idle store.
type hookClaimReadGuard struct {
	ops *hookClaimOps
	// stores and primary name each leg in the refusal's message: the agent's
	// own store, "rig:<name>", or the store's directory.
	stores  []hookStore
	primary hookStore
}

// runner wraps run with the budget's two rules.
func (g *hookClaimReadGuard) runner(run hookStoreRunner) hookStoreRunner {
	return func(command, dir string, env []string) (string, error) {
		deadline, bounded := g.ops.readDeadline()
		start := g.ops.nowOrWallClock()
		if bounded && !start.Before(deadline) {
			return "", g.refusal(g.legLabel(dir, env))
		}
		out, err := run(command, dir, env)
		if err == nil && bounded && !g.ops.nowOrWallClock().Before(deadline) {
			return "", g.refusal(g.legLabel(dir, env))
		}
		return out, err
	}
}

// refusal is the guard's answer for a read the budget would not admit or cut
// short. It wraps context.DeadlineExceeded so the failure classifies
// store-unavailable (exit 2 + token) rather than masquerading as an empty
// queue, and names the leg and the budget so the operator sees which read ran
// out of which share.
func (g *hookClaimReadGuard) refusal(leg string) error {
	window := g.ops.claimWindowOrDefault()
	budget := window - hookClaimReadReserve(window)
	return fmt.Errorf("claim read of %s cut by the invocation read budget (%s of the %s claim window): %w",
		leg, budget, window, context.DeadlineExceeded)
}

// legLabel names one fan-out leg for diagnostics: "own" for the agent's store,
// "rig:<name>" for a rig leg, the store's directory for any other secondary.
func (g *hookClaimReadGuard) legLabel(dir string, env []string) string {
	leg := hookStore{dir: dir, env: env}
	if sameHookStore(leg, g.primary) {
		return "own"
	}
	for _, st := range g.stores {
		if sameHookStore(leg, st) {
			if st.rig != "" {
				return "rig:" + st.rig
			}
			return st.dir
		}
	}
	return dir
}
