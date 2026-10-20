package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
)

// backstopPredicate adapts the shared nudge-backstop engine (observe → nudge
// → backoff → give-up, see decideBackstopAction) to one class of session.
// Each predicate owns its own eligibility test, outstanding-work resolution,
// nudge content, and persisted-metadata shape; the engine drives only the
// shared timing decision and the delivery: a direct runtime.Provider.Nudge,
// or, when the controller's lane host executes message ops, a planner notice
// op (proposeBackstopNotice).
//
// poolClaimBackstop and poolContinuationBackstop (idle_nudge.go) and
// poolExecutionBackstop (execution_backstop.go) are the three predicates:
// initial trigger delivery, later graph-v2 successor delivery, and a claimed
// bead never executed.
type backstopPredicate interface {
	// governs reports whether this predicate applies to the session bead at
	// all.
	governs(s beads.Bead) bool

	// resolve classifies the current evidence for sessName. Definite absence
	// returns backstopResolutionClear; incomplete or ambiguous evidence returns
	// backstopResolutionHold so persisted pacing state is not erased.
	resolve(s beads.Bead, work map[string]beads.Bead, sessName string) (target backstopTarget, resolution backstopResolution)

	// state reads the persisted pacing state for target. same is false when
	// target is an assignment not yet observed, in which case the engine calls
	// observe to (re)start the grace clock instead of consulting attempts.
	state(s beads.Bead, target backstopTarget) (same bool, attempts int, last time.Time)

	// content resolves the text to nudge with, or "" to skip silently.
	content(s beads.Bead) string

	// revalidate checks the exact target immediately before attempt reservation
	// and delivery. It closes the desired-state-snapshot race without treating
	// a read failure as proof that work disappeared.
	revalidate(target backstopTarget) backstopResolution

	// observe persists the start of a new assignment's grace window.
	observe(store beads.Store, s *beads.Bead, target backstopTarget, now time.Time, stdout io.Writer)

	// reserve durably records a delivery attempt: before a direct nudge, or
	// after its planner notice is proposed. false means the write failed:
	// the provider must not be nudged, and a proposed notice's attempt is
	// re-proposed (idempotently) on a later tick.
	reserve(store beads.Store, s *beads.Bead, target backstopTarget, attempts int, now time.Time, stdout io.Writer) bool

	// exhausted is invoked once attempts reach the shared max attempts.
	exhausted(store beads.Store, s *beads.Bead, stdout io.Writer)

	// clear wipes persisted state once nothing is outstanding, and reports
	// whether it wiped any: false when there was none, or the write failed.
	clear(store beads.Store, s *beads.Bead, stdout io.Writer) bool
}

// backstopHoldObserver is an optional predicate extension. A predicate that
// implements it is told which engine-level gate held a session this tick
// (backstopHold* reasons) and when the session became actionable again, so it
// can leave a durable breadcrumb. Without one, a backstop that is holding looks
// exactly like one that is broken.
type backstopHoldObserver interface {
	observeHold(store beads.Store, s *beads.Bead, reason string, stdout io.Writer)
	clearHold(store beads.Store, s *beads.Bead, stdout io.Writer)
}

// Engine-level hold reasons passed to backstopHoldObserver.observeHold.
const (
	backstopHoldRuntimeNotRunning = "runtime_not_running"
	backstopHoldRevalidate        = "revalidate_hold"
)

func observeBackstopHold(pred backstopPredicate, store beads.Store, s *beads.Bead, reason string, stdout io.Writer) {
	if ho, ok := pred.(backstopHoldObserver); ok {
		ho.observeHold(store, s, reason, stdout)
	}
}

func clearBackstopHold(pred backstopPredicate, store beads.Store, s *beads.Bead, stdout io.Writer) {
	if ho, ok := pred.(backstopHoldObserver); ok {
		ho.clearHold(store, s, stdout)
	}
}

// clearBackstop acts on a resolution that nothing is outstanding for s: pred
// wipes its pacing state, label's open notices are withdrawn once that wipe
// lands (so a later tick with nothing to wipe reads no ops), and the hold
// breadcrumb is cleared.
func clearBackstop(pred backstopPredicate, store beads.Store, s *beads.Bead, label string, stdout io.Writer) {
	if pred.clear(store, s, stdout) {
		withdrawBackstopNotices(store, s, label, "no work is outstanding", stdout)
	}
	clearBackstopHold(pred, store, s, stdout)
}

// withdrawBackstopNotices withdraws label's open notices in session row s's
// conversation (withdrawPlannerNotices) when the controller's lane host
// executes message ops for store, because their cause has cleared or moved
// (reason). It is best-effort: a failure is logged, and a notice it leaves
// stands until its start_by.
func withdrawBackstopNotices(store beads.Store, s *beads.Bead, label, reason string, stdout io.Writer) {
	lane := plannerNoticeLaneFor(store)
	if lane == nil {
		return
	}
	if err := withdrawPlannerNotices(lane, *s, label, reason); err != nil {
		fmt.Fprintf(stdout, "%s: withdrawing notices for %s failed: %v\n", label, strings.TrimSpace(s.Metadata["session_name"]), err) //nolint:errcheck // best-effort
	}
}

// backstopTarget is the durable identity of one outstanding delivery target.
// ID is the human-facing work bead. RootID, StoreRef, and Generation are
// optional persisted provenance fields: the initial pool-claim predicate needs
// only ID, while continuation claims persist all four so same-ID rows in
// independent stores, recycled graph roots, and recycled pool generations
// never share pacing state. Assignee and Store retain the exact live-read
// authority used only for pre-delivery revalidation.
type backstopTarget struct {
	ID         string
	RootID     string
	StoreRef   string
	Generation string
	Assignee   string
	Store      beads.Store
}

// backstopResolution distinguishes definite completion from uncertainty.
// Conflating hold with clear resets persisted attempt caps during transient
// store or identity ambiguity and can turn a bounded backstop into churn.
type backstopResolution int

const (
	backstopResolutionClear backstopResolution = iota
	backstopResolutionHold
	backstopResolutionOutstanding
)

// backstopAction is the shared timing engine's verdict for one session on one
// reconcile tick.
type backstopAction int

const (
	backstopActionWait backstopAction = iota
	backstopActionNudge
	backstopActionExhausted
)

// decideBackstopAction is the observe(grace) → nudge → backoff → give-up
// timing rule shared by every backstop predicate, extracted unchanged from
// nudgeStalledPoolClaims. attempts is the number of delivery attempts already
// reserved for the current assignment; last is the time of the last attempt,
// or of first observation when attempts is 0. Pacing reuses the exact constants
// proven by the pool-claim backstop (idleClaimNudgeGrace/Backoff/MaxAttempts,
// idle_nudge.go).
func decideBackstopAction(attempts int, last, now time.Time) backstopAction {
	switch {
	case attempts == 0:
		if now.Sub(last) < idleClaimNudgeGrace {
			return backstopActionWait // still inside the observe-first grace
		}
	case attempts >= idleClaimNudgeMaxAttempts:
		return backstopActionExhausted // gave up; manual re-nudge is the escape hatch
	default:
		if now.Sub(last) < idleClaimNudgeBackoff {
			return backstopActionWait // waiting out the backoff before the next retry
		}
	}
	return backstopActionNudge
}

// runNudgeBackstop drives pred over sessionBeads: for each session it governs
// that is running and has outstanding work, it paces re-delivery of pred's
// nudge content through the shared grace → nudge → backoff → give-up engine,
// persisting all state via pred so a controller restart cannot replay it.
// label prefixes stdout diagnostics so multiple backstops stay distinguishable
// in logs, and names the backstop's planner notices. When the controller's
// lane host executes message ops for store, each attempt is a notice op the
// lane delivers at the session's boundary (proposeBackstopNotice) instead of
// a direct runtime.Provider.Nudge.
func runNudgeBackstop(
	sp runtime.Provider,
	store beads.Store,
	sessionBeads []beads.Bead,
	work []beads.Bead,
	now time.Time,
	stdout io.Writer,
	label string,
	pred backstopPredicate,
) {
	if sp == nil || store == nil {
		return // hot reconcile path: never panic on a half-built dependency
	}
	workByID := make(map[string]beads.Bead, len(work))
	for _, w := range work {
		workByID[w.ID] = w
	}

	for i := range sessionBeads {
		s := &sessionBeads[i]
		if !pred.governs(*s) {
			continue
		}
		sessName := strings.TrimSpace(s.Metadata["session_name"])
		if sessName == "" || !sp.IsRunning(sessName) {
			observeBackstopHold(pred, store, s, backstopHoldRuntimeNotRunning, stdout)
			continue
		}

		target, resolution := pred.resolve(*s, workByID, sessName)
		switch resolution {
		case backstopResolutionHold:
			continue
		case backstopResolutionClear:
			clearBackstop(pred, store, s, label, stdout)
			continue
		case backstopResolutionOutstanding:
			// Continue below. The hold breadcrumb is cleared where the engine
			// actually advances, not here: clearing on every outstanding tick
			// would flap it against a standing revalidate hold, costing two
			// writes per tick for as long as that hold lasts.
		default:
			continue
		}

		same, attempts, last := pred.state(*s, target)
		if !same {
			// First observation of this assignment: start the grace clock,
			// don't nudge yet — a normal claim/confirmation almost always
			// lands within the grace window. The notices of the assignment it
			// replaces are withdrawn, as this one's share no correlation with
			// them and so cannot supersede them; that runs before the marker
			// write, so a tick whose write fails withdraws them again next
			// tick.
			clearBackstopHold(pred, store, s, stdout)
			withdrawBackstopNotices(store, s, label, "the reminder's target changed", stdout)
			pred.observe(store, s, target, now, stdout)
			continue
		}

		switch decideBackstopAction(attempts, last, now) {
		case backstopActionWait:
			clearBackstopHold(pred, store, s, stdout)
			continue
		case backstopActionExhausted:
			clearBackstopHold(pred, store, s, stdout)
			if backstopLastNoticeStands(store, s, target, last, now, label, stdout) {
				continue
			}
			pred.exhausted(store, s, stdout)
			continue
		case backstopActionNudge:
			switch pred.revalidate(target) {
			case backstopResolutionHold:
				observeBackstopHold(pred, store, s, backstopHoldRevalidate, stdout)
				continue
			case backstopResolutionClear:
				clearBackstop(pred, store, s, label, stdout)
				continue
			case backstopResolutionOutstanding:
				clearBackstopHold(pred, store, s, stdout)
				// Deliver below.
			default:
				continue
			}
			content := pred.content(*s)
			if content == "" {
				// Nothing is deliverable for this session. `agent.Nudge` is
				// optional and pool templates routinely leave it unset, so this
				// is the common case, not the exotic one. Skipping silently
				// parks the state machine on its observe marker forever: no
				// attempt is ever reserved, the cap is never reached, and the
				// predicate's terminal action never runs — which for the
				// execution backstop is the drain that is the only thing that
				// releases the claim. The bounded attempts exist to give the
				// agent a chance to ANSWER a nudge; with no nudge to send there
				// is nothing to wait for, so go straight to the terminal action
				// after the same grace window.
				//
				// poolExecutionBackstop.escalatesNext mirrors this shortcut;
				// change the two in lockstep, or a started seat is drained or
				// held with nothing sent.
				pred.exhausted(store, s, stdout)
				continue
			}
			if lane := plannerNoticeLaneFor(store); lane != nil &&
				proposeBackstopNotice(lane, pred, store, s, target, attempts, last, content, now, stdout, label) {
				continue
			}
			// Write ahead of the external delivery. If the process crashes
			// after this point, an attempt may be consumed without delivery,
			// but a crash or store failure can never replay an unbounded nudge.
			if !pred.reserve(store, s, target, attempts+1, now, stdout) {
				continue
			}
			if err := sp.Nudge(sessName, runtime.TextContent(content)); err != nil {
				fmt.Fprintf(stdout, "%s: %s failed: %v\n", label, sessName, err) //nolint:errcheck // best-effort
				continue
			}
			fmt.Fprintf(stdout, "%s: nudged %s for %s (attempt %d/%d)\n", label, sessName, target.ID, attempts+1, idleClaimNudgeMaxAttempts) //nolint:errcheck // best-effort
		}
	}
}

// proposeBackstopNotice is the backstop's delivery through the conversation
// lane: attempt attempts+1 of label's notice for target, in the pacing
// episode last names, proposed before its reservation (the op is idempotent
// by key, so a crash between the two re-proposes the same op and then
// reserves). It reports false when no lane can type into the session, for
// the caller to nudge directly; otherwise the attempt is handled:
//   - while an earlier notice for target is in flight nothing is written and
//     the attempt is not spent; one that is still due but not started is
//     superseded by this attempt's (proposePlannerNotice);
//   - an attempt whose op already stood, with this text or an earlier one,
//     is spent, with its own line;
//   - a notice the op store refuses for the request itself is spent at once,
//     and one that fails plannerNoticeMaxFailures times in a row is spent
//     then; a failure is logged at most once per laneErrorLogInterval, so a
//     journal that cannot be written never spams a tick nor holds the
//     backstop from its terminal action.
//
// Nothing falls back to a direct nudge once a lane serves the session.
func proposeBackstopNotice(
	lane plannerNoticeLane,
	pred backstopPredicate,
	store beads.Store,
	s *beads.Bead,
	target backstopTarget,
	attempts int,
	last time.Time,
	content string,
	now time.Time,
	stdout io.Writer,
	label string,
) bool {
	sessName := strings.TrimSpace(s.Metadata["session_name"])
	n := plannerNotice{
		Purpose:     label,
		Trigger:     backstopNoticeTrigger(target),
		Causation:   target.ID,
		Episode:     plannerNoticeEpisode(last),
		Attempt:     attempts + 1,
		Content:     content,
		StartWithin: plannerNoticeStartWithin(idleClaimNudgeBackoff),
	}
	failKey := plannerNoticeFailureKey(s.ID, n)
	outcome, op, err := proposePlannerNotice(lane, *s, n)
	if err != nil && !plannerNoticeRefused(err) {
		failures, logNow := lane.noticeFailed(failKey)
		if failures < plannerNoticeMaxFailures {
			if logNow {
				fmt.Fprintf(stdout, "%s: notice for %s for %s failed (%d of %d before attempt %d/%d is spent): %v\n", label, sessName, target.ID, failures, plannerNoticeMaxFailures, n.Attempt, idleClaimNudgeMaxAttempts, err) //nolint:errcheck // best-effort
			}
			return true
		}
	}
	lane.noticeRecovered(failKey)
	var line string
	switch {
	case err != nil && plannerNoticeRefused(err):
		line = fmt.Sprintf("notice for %s for %s refused; attempt %d/%d spent: %v", sessName, target.ID, n.Attempt, idleClaimNudgeMaxAttempts, err)
	case err != nil:
		line = fmt.Sprintf("notice for %s for %s failed %d times in a row; attempt %d/%d spent: %v", sessName, target.ID, plannerNoticeMaxFailures, n.Attempt, idleClaimNudgeMaxAttempts, err)
	case outcome == plannerNoticeDirect:
		return false
	case outcome == plannerNoticePending:
		return true
	case outcome == plannerNoticeAlreadyProposed:
		line = fmt.Sprintf("notice for %s for %s attempt %d/%d was already proposed (%s, %s); attempt spent", sessName, target.ID, n.Attempt, idleClaimNudgeMaxAttempts, op.ID, op.Status)
	default:
		line = fmt.Sprintf("queued notice for %s for %s (attempt %d/%d)", sessName, target.ID, n.Attempt, idleClaimNudgeMaxAttempts)
	}
	if !pred.reserve(store, s, target, n.Attempt, now, stdout) {
		return true
	}
	fmt.Fprintf(stdout, "%s: %s\n", label, line) //nolint:errcheck // best-effort
	return true
}

// backstopLastNoticeStands reports whether an exhausted backstop's terminal
// action must wait: the controller's lane host executes message ops for
// store and label's last notice for target is still open and due, so the
// last reminder has not yet had its chance at the session's boundary. Once
// a backoff has passed since the last reservation (last) the last notice is
// past its start_by, so only one the lane is still delivering (executing or
// settling) holds the action, and for at most one lease more, the lease the
// conversation lane takes it under (opjournal.DefaultLeaseTTL). Past that it
// is no longer read: an exhausted backstop whose terminal action leaves it
// exhausted pays no listing per tick. A failed read does not hold the
// terminal action; it is logged at most once per laneErrorLogInterval.
func backstopLastNoticeStands(store beads.Store, s *beads.Bead, target backstopTarget, last, now time.Time, label string, stdout io.Writer) bool {
	if !now.Before(last.Add(idleClaimNudgeBackoff + opjournal.DefaultLeaseTTL)) {
		return false
	}
	lane := plannerNoticeLaneFor(store)
	if lane == nil {
		return false
	}
	n := plannerNotice{Purpose: label, Trigger: backstopNoticeTrigger(target)}
	failKey := plannerNoticeTallyKey(s.ID, n, "exhausted")
	open, err := plannerNoticeOpen(lane, *s, n)
	if err != nil {
		if _, logNow := lane.noticeFailed(failKey); logNow {
			fmt.Fprintf(stdout, "%s: reading the last notice for %s failed; not holding the terminal action: %v\n", label, strings.TrimSpace(s.Metadata["session_name"]), err) //nolint:errcheck // best-effort
		}
		return false
	}
	lane.noticeRecovered(failKey)
	return open
}
