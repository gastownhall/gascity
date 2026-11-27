package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// errHookClaimReadBudgetSpent marks a claim-path work-query read cut by the
// invocation's read budget: one made once work only a CAS can take was in hand
// that would start, or was still running, at the read deadline
// (hookClaimReadGuard.read), or one that answered at or after the deadline with
// work a claim mutation was then refused on (hookClaimOps.RefuseLateAnswer). It
// wraps context.DeadlineExceeded, so the cut classifies exactly like a
// timed-out read (exit 2 with the store-unavailable token, no drain).
var errHookClaimReadBudgetSpent = fmt.Errorf("the claim read budget is spent: %w", context.DeadlineExceeded)

// claimReadReserve is the slice of the claim window held back from the reads:
// the window's claim share, hookClaimMutationTimeout (hookClaimWindowDefault
// is the read share plus it), or half the window when an override makes the
// window shorter than twice that, so the reserve never eats the whole window.
// It is the time the steps between the last read and the claim CAS (the drain
// probe, workflow retirement, the seat checks) have to start the CAS inside
// the window; the CAS itself then gets its own budget (claimMutationContext).
func (ops *hookClaimOps) claimReadReserve() time.Duration {
	return min(hookClaimMutationTimeout, ops.claimWindowOrDefault()/2)
}

// claimReadBudget is how much of the claim window the claim-path reads of one
// invocation share before the work they find must be in hand: the window less
// the reserve.
//
// The window is one budget anchored at InvokedAt, while each read gets its own
// fresh hookWorkQueryTimeout. Without a shared read budget a fan-out of slow
// legs could spend the whole window reading and then refuse the work it found
// as "the claim window is spent". The budget binds the claim mutations and the
// reads that could hold one up: an answer that arrives past it is still acted
// on where it needs no mutation, a mutation on its work is refused
// (hookClaimOps.RefuseLateAnswer), and once work is in hand the reads after it
// end by the deadline (hookClaimReadGuard.read).
func (ops *hookClaimOps) claimReadBudget() time.Duration {
	return ops.claimWindowOrDefault() - ops.claimReadReserve()
}

// claimReadDeadline is the instant work only a CAS can take must be in hand by
// (InvokedAt + claimReadBudget), and false when the caller opened no invocation
// window (zero InvokedAt), which has no read budget.
func (ops *hookClaimOps) claimReadDeadline() (time.Time, bool) {
	if ops.InvokedAt.IsZero() {
		return time.Time{}, false
	}
	return ops.InvokedAt.Add(ops.claimReadBudget()), true
}

// claimReadBudgetSpent reports whether the read deadline has passed, or is now.
// The boundary counts as spent: a read started at the last instant cannot
// answer inside the budget, and one that answers then leaves the mutation no
// reserve. Like claimWindowSpent, it never fires without an invocation window.
func (ops *hookClaimOps) claimReadBudgetSpent() bool {
	deadline, ok := ops.claimReadDeadline()
	if !ok {
		return false
	}
	return !ops.nowOrWallClock().Before(deadline)
}

// claimReadContext is the parent context of a read that must end by the read
// deadline, as every read made while claimable work is in hand must
// (hookClaimReadGuard.readContext): its deadline is the read deadline, with
// errHookClaimReadBudgetSpent as the cause, so a production subprocess still
// running then is killed there rather than at its own, later,
// hookWorkQueryTimeout. Without an invocation window it carries no deadline.
func (ops *hookClaimOps) claimReadContext() (context.Context, context.CancelFunc) {
	deadline, ok := ops.claimReadDeadline()
	if !ok {
		return context.WithCancel(context.Background())
	}
	return context.WithDeadlineCause(context.Background(), deadline, errHookClaimReadBudgetSpent)
}

// Read-trace outcomes: what one claim-path read answered.
const (
	// hookClaimReadWork is ready work only a claim CAS can take.
	hookClaimReadWork = "work"
	// hookClaimReadResume is this session's own in-progress work, which
	// adoption resumes with no CAS (hookOwnResumeAnswer).
	hookClaimReadResume    = "resume"
	hookClaimReadEmpty     = "empty"
	hookClaimReadError     = "error"
	hookClaimReadBudgetCut = "budget-cut"
)

// hookClaimRead is one claim-path read as the trace reports it: which leg, when
// it started (invocation age), how long it took, and what it answered. store is
// the leg it read (dir and env), and late reports that its answer arrived at or
// after the read deadline and was kept (hookClaimReadGuard.lateAnswer).
type hookClaimRead struct {
	leg      string
	start    time.Duration
	duration time.Duration
	outcome  string
	store    hookStore
	late     bool
}

// hookClaimReadGuard holds the claim-path reads of one invocation to its read
// budget, whatever runner performs them, and records each read for the read
// trace and for the claim that acts on its answer (lateAnswer). It times every
// read itself, so the budget holds for any runner, a fake one included; the
// parent context it gives each read (readContext) additionally kills a
// production subprocess at the deadline.
type hookClaimReadGuard struct {
	ops    *hookClaimOps
	run    hookClaimReadRunner
	stores []hookStore
	reads  []hookClaimRead
	// selectionStart is the index in reads of the current store selection's
	// first read.
	selectionStart int
	// workInHand reports that a read of the current store selection answered,
	// inside the budget, with work only a CAS can take.
	workInHand bool
}

// hookClaimReadRunner runs one claim-path work query, as hookStoreRunner does,
// under parent: the context the read guard gives that read.
type hookClaimReadRunner func(parent context.Context, command, dir string, env []string) (string, error)

// hookClaimReadRunnerFor returns run as a hookClaimReadRunner that ignores the
// parent context, for a runner whose reads no read deadline kills: a test's
// scripted one, or plain discovery's (hookRigLegRunner). The read guard still
// holds a claim's answers to the read budget.
func hookClaimReadRunnerFor(run hookStoreRunner) hookClaimReadRunner {
	return func(_ context.Context, command, dir string, env []string) (string, error) {
		return run(command, dir, env)
	}
}

// newHookClaimReadGuard wraps run for the invocation ops describes over the
// fan-out stores, whose first entry is the agent's own store.
func newHookClaimReadGuard(ops *hookClaimOps, run hookClaimReadRunner, stores []hookStore) *hookClaimReadGuard {
	return &hookClaimReadGuard{ops: ops, run: run, stores: stores}
}

// beginSelection starts one store selection (selectStoreWithWorkRetrying): no
// read of it has found work yet.
func (g *hookClaimReadGuard) beginSelection() {
	g.workInHand = false
	g.selectionStart = len(g.reads)
}

// read is the guarded runner. The read deadline exists so found work can start
// its CAS inside the claim window, so it binds the claim mutations, not the
// answers, and only the reads that could hold up a CAS already in sight:
//
//   - Until the selection has work only a CAS can take in hand from an answer
//     that arrived in time, a read runs to its own hookWorkQueryTimeout,
//     whenever it starts, and its answer is kept however late it arrives. The
//     claim acts on a late answer wherever it needs no mutation (an empty
//     answer's no_work drain, adoption of this session's own in-progress
//     work, workflow retirement, the seat and deferral skips) and refuses a
//     mutation on it (lateAnswer, hookClaimOps.RefuseLateAnswer).
//   - Once such work is in hand, every later read must end by the deadline so
//     that work's CAS can still start in the window: a read does not start past
//     it, and one still running then is cut, whatever it answers.
//
// A read that failed keeps its own error.
func (g *hookClaimReadGuard) read(command, dir string, env []string) (string, error) {
	start := g.ops.invocationAge()
	store := hookStore{dir: dir, env: env}
	bounded := g.workInHand
	if bounded && g.ops.claimReadBudgetSpent() {
		g.record(store, start, hookClaimReadBudgetCut, false)
		return "", g.budgetError(command, "not started")
	}
	parent, cancel := g.readContext()
	out, err := g.run(parent, command, dir, env)
	cancel()
	late := err == nil && g.ops.claimReadBudgetSpent()
	if bounded && late {
		// Production kills such a read at the deadline (readContext); an answer
		// a runner gave anyway is cut the same way.
		g.record(store, start, hookClaimReadBudgetCut, false)
		return "", g.budgetError(command, "answered past the deadline")
	}
	outcome := g.judge(out, err)
	g.record(store, start, outcome, late)
	if outcome == hookClaimReadWork && !late {
		g.workInHand = true
	}
	return out, err
}

// readContext is the parent context of the read about to run: the read
// deadline (claimReadContext) once the selection has claimable work in hand,
// else none, so the read runs to its own hookWorkQueryTimeout.
func (g *hookClaimReadGuard) readContext() (context.Context, context.CancelFunc) {
	if g.workInHand {
		return g.ops.claimReadContext()
	}
	return context.WithCancel(context.Background())
}

// judge is the read-trace outcome of a finished read: what it answered, a
// failed read, or a budget cut when the read deadline killed it (readContext).
func (g *hookClaimReadGuard) judge(out string, err error) string {
	switch {
	case errors.Is(err, errHookClaimReadBudgetSpent):
		return hookClaimReadBudgetCut
	case err != nil:
		return hookClaimReadError
	}
	return hookClaimAnswerOutcome(out, g.ops.nowOrWallClock())
}

// lateAnswer reports whether the answer the current selection last read from st
// arrived at or after the read deadline (read), with that read's index in
// reads. The selection serves the answer of its last read of a store, so that
// is the answer a claim on st acts on.
func (g *hookClaimReadGuard) lateAnswer(st hookStore) (int, bool) {
	for i := len(g.reads) - 1; i >= g.selectionStart; i-- {
		if sameHookStore(g.reads[i].store, st) {
			return i, g.reads[i].late
		}
	}
	return 0, false
}

// refuseLateAnswer is the failed read a claim mutation on the late answer at
// reads[i], read from st with command, is refused as: the read the budget cut,
// so labeled in the trace. It is shaped as bestStoreWithWork shapes a failed
// read with no work anywhere, the agent's own store's error as itself and any
// other store's as a hookSecondaryStoreFailure naming it, so it is reported and
// classified as one.
func (g *hookClaimReadGuard) refuseLateAnswer(i int, st hookStore, command string) error {
	g.reads[i].outcome = hookClaimReadBudgetCut
	err := g.budgetError(command, "answered past the deadline")
	if len(g.stores) > 0 && sameHookStore(st, g.stores[0]) {
		return err
	}
	return &hookSecondaryStoreFailure{failures: []*hookStoreReadError{{dir: st.dir, rig: st.rig, err: err}}}
}

// hookClaimAnswerOutcome is what a claim read's answer holds, by the readiness
// bestStoreWithWork selects on: no ready work, this session's own in-progress
// work (hookOwnResumeAnswer), or work only a claim CAS can take.
func hookClaimAnswerOutcome(out string, now time.Time) string {
	if !workQueryHasReadyWork(filterUnreadyHookCandidates(normalizeWorkQueryOutput(strings.TrimSpace(out)), now)) {
		return hookClaimReadEmpty
	}
	if _, resume := hookOwnResumeAnswer(out, now); resume {
		return hookClaimReadResume
	}
	return hookClaimReadWork
}

// record appends one read of store, started at invocation age start and ending
// now; late reports that its kept answer arrived at or after the read deadline.
func (g *hookClaimReadGuard) record(store hookStore, start time.Duration, outcome string, late bool) {
	g.reads = append(g.reads, hookClaimRead{
		leg:      g.legLabel(store.dir),
		start:    start,
		duration: g.ops.invocationAge() - start,
		outcome:  outcome,
		store:    store,
		late:     late,
	})
}

// legLabel names the store at dir: "own" for the agent's own store,
// "rig:<name>" for a rig leg, else the store's dir.
func (g *hookClaimReadGuard) legLabel(dir string) string {
	for i, st := range g.stores {
		if st.dir != dir {
			continue
		}
		if i == 0 {
			return "own"
		}
		if st.rig != "" {
			return "rig:" + st.rig
		}
		return "store:" + dir
	}
	return "store:" + dir
}

// writeTrace prints the invocation's one read-trace line, ending with how the
// claim ended (tail). It is written only for an invocation that ends without
// claiming for a reason other than every leg answering empty, so the reads that
// spent its window are on record next to the failure.
func (g *hookClaimReadGuard) writeTrace(stderr io.Writer, tail string) {
	entries := make([]string, 0, len(g.reads)+1)
	for _, r := range g.reads {
		entries = append(entries, fmt.Sprintf("%s +%s %s %s", r.leg, hookClaimTraceDuration(r.start), hookClaimTraceDuration(r.duration), r.outcome))
	}
	entries = append(entries, tail)
	_, _ = fmt.Fprintf(stderr, "gc hook --claim: reads (budget %s of %s window): %s\n",
		hookClaimTraceDuration(g.ops.claimReadBudget()), hookClaimTraceDuration(g.ops.claimWindowOrDefault()), strings.Join(entries, "; "))
}

// hookClaimSelectionFailedTail ends the read trace of a claim whose store
// selection failed, or whose selected answer arrived too late for the claim
// mutation it led to (hookClaimOps.RefuseLateAnswer). A first selection
// started no claim. A reselection follows a claim that took nothing (its rows
// were lost or skipped) or whose mutations errored, and the tail says which,
// since the reads the trace lists do not show the claim.
func hookClaimSelectionFailedTail(reselecting, claimsErrored bool) string {
	switch {
	case claimsErrored:
		return "reselection read failed after claims errored"
	case reselecting:
		return "reselection read failed after a claim that took nothing"
	default:
		return "no claim started"
	}
}

// hookClaimTraceDuration renders a read-trace duration at 100ms resolution.
func hookClaimTraceDuration(d time.Duration) string {
	return d.Round(100 * time.Millisecond).String()
}

// budgetError is the error for a read the budget cut. It keeps the "timed out
// after" phrase the kill classifier names a timeout by.
func (g *hookClaimReadGuard) budgetError(command, how string) error {
	return fmt.Errorf("running work query %q: timed out after %s (%s; claim read budget of the %s claim window): %w",
		command, g.ops.claimReadBudget(), how, g.ops.claimWindowOrDefault(), errHookClaimReadBudgetSpent)
}
