package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// backstopPredicate adapts the shared nudge-backstop engine (observe → nudge
// → backoff → give-up, see decideBackstopAction) to one class of session.
// Each predicate owns its own eligibility test, outstanding-work resolution,
// nudge content, and persisted-metadata shape; the engine drives only the
// shared timing decision and the actual runtime.Provider.Nudge delivery.
//
// Each backstop lane implements this interface and registers through its own
// runNudgeBackstop call site; those call sites are the current list of lanes.
type backstopPredicate interface {
	// governs reports whether this predicate applies to the session bead at
	// all.
	governs(s beads.Bead) bool

	// resolve classifies the current evidence for sessName. Definite absence
	// returns backstopResolutionClear; incomplete or ambiguous evidence returns
	// backstopResolutionHold so persisted pacing state is not erased.
	resolve(s beads.Bead, sessName string) (target backstopTarget, resolution backstopResolution)

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

	// reserve durably records a nudge attempt before delivery. false means the
	// write failed and the provider must not be nudged.
	reserve(store beads.Store, s *beads.Bead, target backstopTarget, attempts int, now time.Time, stdout io.Writer) bool

	// exhausted is invoked once attempts reach the shared max attempts.
	exhausted(store beads.Store, s *beads.Bead, stdout io.Writer)

	// clear wipes persisted state once nothing is outstanding.
	clear(store beads.Store, s *beads.Bead, stdout io.Writer)
}

// activityDecayingBackstop is an optional backstopPredicate extension. A
// predicate whose outstanding condition LOOKS like a working agent — an
// in-progress claim, which a busy seat holds by design — implements it to
// re-arm its pacing when the runtime reports fresh activity since the last
// attempt. A human-paced interactive seat answers a nudge, works in a burst,
// and pauses again; without this the accumulated quiet marches it to the
// terminal drain even though it is plainly alive. The open-bead predicates do
// NOT implement it: their condition vanishes the instant the agent acts, so
// they never need to tell "working" from "stalled" by activity.
type activityDecayingBackstop interface {
	// decay re-arms the predicate's pacing window for target and reports
	// whether it did. The implementation owns BOTH halves of that judgement:
	// whether sessName showed runtime activity after last (the persisted time
	// of the last attempt), and whether its own bounded re-arm budget for this
	// assignment still allows one. A false answer hands the session to the
	// ordinary ladder below, so a predicate can never trade convergence away
	// for an activity signal it cannot fully trust.
	decay(store beads.Store, s *beads.Bead, target backstopTarget, sessName string, last, now time.Time, stdout io.Writer) bool
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

// decayedWindow gives a predicate that tracks a working-looking seat the chance
// to re-arm its own pacing window instead of taking another step toward the
// terminal action — the seat answered the previous nudge and paused again on its
// own cadence, so it has earned a fresh window (see activityDecayingBackstop,
// which owns the decision and its bound). Reports whether the window was
// re-armed, in which case this tick is done.
//
// attempts>0 is the gate: at attempts==0 decideBackstopAction's grace check
// already grants the first free pass, and there is no accumulated budget to
// shed. Predicates that do not implement the extension never decay.
func decayedWindow(
	pred backstopPredicate,
	store beads.Store,
	s *beads.Bead,
	target backstopTarget,
	sessName string,
	attempts int,
	last, now time.Time,
	stdout io.Writer,
) bool {
	if attempts == 0 {
		return false
	}
	decayer, ok := pred.(activityDecayingBackstop)
	if !ok {
		return false
	}
	return decayer.decay(store, s, target, sessName, last, now, stdout)
}

// runNudgeBackstop drives pred over sessionBeads: for each session it governs
// that is running and has outstanding work, it paces re-delivery of pred's
// nudge content through the shared grace → nudge → backoff → give-up engine,
// persisting all state via pred so a controller restart cannot replay it.
// label prefixes stdout diagnostics so multiple backstops stay distinguishable
// in logs.
func runNudgeBackstop(
	sp runtime.Provider,
	store beads.Store,
	sessionBeads []beads.Bead,
	now time.Time,
	stdout io.Writer,
	label string,
	pred backstopPredicate,
) {
	if sp == nil || store == nil {
		return // hot reconcile path: never panic on a half-built dependency
	}

	for i := range sessionBeads {
		s := &sessionBeads[i]
		sessName := strings.TrimSpace(s.Metadata["session_name"])
		if sessName == "" {
			continue
		}
		if !nudgeBackstopSeat(sp, store, s, pred, sessName, label, now, stdout) {
			// Every outcome except the prompted skip means this lane is not
			// waiting on a human at this seat — including the filters that drop
			// it before any probe runs (no longer governed, resolved away, gone,
			// or simply paced to wait). Reclaiming here rather than at each exit
			// is what keeps the process-global notice map down to the seats this
			// pass still enumerates. It cannot reclaim more than the pass sees:
			// a seat whose session bead is closed while it sits at its prompt
			// leaves sessionBeads entirely (loadSessionBeadSnapshot deliberately
			// does not load closed history) and never reaches this loop again.
			backstopPromptNoticeForget(label, sessName)
		}
	}
}

// nudgeBackstopSeat runs one seat through the lane's pacing state machine and
// reports whether this tick ended in the prompted skip — the one outcome whose
// notice bookkeeping must survive to the next tick, so waiting on a human is
// announced once per prompt rather than once per reconcile tick.
func nudgeBackstopSeat(
	sp runtime.Provider,
	store beads.Store,
	s *beads.Bead,
	pred backstopPredicate,
	sessName, label string,
	now time.Time,
	stdout io.Writer,
) bool {
	if !pred.governs(*s) {
		return false
	}
	if !sp.IsRunning(sessName) {
		return false
	}

	target, resolution := pred.resolve(*s, sessName)
	switch resolution {
	case backstopResolutionHold:
		return false
	case backstopResolutionClear:
		pred.clear(store, s, stdout)
		return false
	case backstopResolutionOutstanding:
		// Continue below.
	default:
		return false
	}

	same, attempts, last := pred.state(*s, target)
	if !same {
		// First observation of this assignment: start the grace clock,
		// don't nudge yet — a normal claim/confirmation almost always
		// lands within the grace window.
		pred.observe(store, s, target, now, stdout)
		return false
	}

	if decayedWindow(pred, store, s, target, sessName, attempts, last, now, stdout) {
		return false
	}

	switch decideBackstopAction(attempts, last, now) {
	case backstopActionExhausted:
		pred.exhausted(store, s, stdout)
	case backstopActionNudge:
		return deliverBackstopNudge(sp, store, s, pred, target, sessName, label, attempts, now, stdout)
	case backstopActionWait:
	}
	return false
}

// deliverBackstopNudge runs the delivery arm for one session the timing engine
// has decided to nudge on this tick: revalidate → content → seat probe →
// reserve → deliver. Split out of runNudgeBackstop so the engine reads as the
// pacing state machine it is and the pre-delivery refusals have room to state
// their own reasoning. Reports whether the tick ended in a prompted skip, which
// is what keeps the seat's notice entry from being reclaimed under it.
func deliverBackstopNudge(
	sp runtime.Provider,
	store beads.Store,
	s *beads.Bead,
	pred backstopPredicate,
	target backstopTarget,
	sessName, label string,
	attempts int,
	now time.Time,
	stdout io.Writer,
) bool {
	switch pred.revalidate(target) {
	case backstopResolutionHold:
		return false
	case backstopResolutionClear:
		pred.clear(store, s, stdout)
		return false
	case backstopResolutionOutstanding:
		// Deliver below.
	default:
		return false
	}
	content := pred.content(*s)
	if content == "" {
		// Nothing is deliverable for this session — an ordinary path
		// rather than an exotic one in every lane, though for different
		// reasons. The lanes that resolve content through the
		// default-nudge fallback (the claim and execution backstops)
		// reach it only when the template or agent cannot be resolved
		// at all; the continuation lane reads the configured nudge
		// directly, and `agent.Nudge` is optional and routinely unset.
		// Skipping silently parks the state machine on its observe
		// marker forever: no attempt is ever reserved, the cap is never
		// reached, and the predicate's terminal action never runs —
		// which for the execution backstop is the drain that is the only
		// thing that releases the claim. The bounded attempts exist to
		// give the agent a chance to ANSWER a nudge; with no nudge to
		// send there is nothing to wait for, so go straight to the
		// terminal action after the same grace window.
		pred.exhausted(store, s, stdout)
		return false
	}
	wait, episode, probeErr := backstopSeatAwaitsHumanInput(sp, sessName)
	switch wait {
	case backstopSeatPrompted:
		// A seat sitting at an approval or selection prompt is waiting on a
		// human, not stalled. Nudging it types into that prompt and answers on
		// the operator's behalf. This skip is deliberately unbounded and
		// deliberately reserves nothing: a human may take as long as they
		// take, and no attempt may be spent — nor pacing state advanced —
		// while the lane is waiting on one. Reported once per prompt rather
		// than once per tick, because the tick rate is the reconciler's, not
		// the operator's.
		if backstopPromptNoticeIsNew(label, sessName, episode) {
			fmt.Fprintf(stdout, "%s: %s skipped: awaiting human input\n", label, sessName) //nolint:errcheck // best-effort
		}
		return true
	case backstopSeatProbeFailed:
		// A probe that will not answer is not a human, and must not be paid
		// for like one. Refusing unboundedly here is the same shape the
		// content == "" comment above rejects: no attempt is ever reserved,
		// the cap is never reached, and the terminal action — for the
		// execution backstop, the drain that is the only thing that releases
		// the claim — never runs, on a condition that has nothing to do with
		// anyone waiting. So still refuse the delivery, but charge it an
		// attempt exactly as an undeliverable nudge is charged below, and let
		// the bounded ladder reach pred.exhausted if the probe stays broken.
		fmt.Fprintf(stdout, "%s: %s skipped: probing pending interaction: %v (attempt %d/%d)\n", label, sessName, probeErr, attempts+1, idleClaimNudgeMaxAttempts) //nolint:errcheck // best-effort
		pred.reserve(store, s, target, attempts+1, now, stdout)
		return false
	}
	// Write ahead of the external delivery. If the process crashes
	// after this point, an attempt may be consumed without delivery,
	// but a crash or store failure can never replay an unbounded nudge.
	if !pred.reserve(store, s, target, attempts+1, now, stdout) {
		return false
	}
	// Deliver through the guarded path rather than sp.Nudge: that call is a
	// wait and then a send, so the probe above only proves the seat was
	// unprompted a wait ago — on the tmux runtime, up to NudgeIdleTimeout ago,
	// and that wait is liable to return precisely because a dialog opened. The
	// guarded path re-probes between the wait and the keystrokes.
	if err := runtime.NudgeUnlessPendingFor(sp, sessName, runtime.TextContent(content)); err != nil {
		if errors.Is(err, runtime.ErrNudgeRefusedPendingInteraction) {
			// The human the probe above refuses for, arriving during the wait.
			// Reported on its own line because it is a different observation
			// than the pre-probe skip, and because the reserved attempt above
			// is not given back: the write-ahead is what stops a crash from
			// replaying an unbounded nudge. It normally costs one attempt per
			// prompt episode, but not because the next tick re-probes: the
			// reservation advances last, so the ticks inside the backoff take
			// backstopActionWait and reach no probe at all. The first tick that
			// does reach one meets the still-open prompt at the pre-probe,
			// which reserves nothing. Those waiting ticks return false, so the
			// notice is reclaimed and that pre-probe skip announces itself
			// again.
			fmt.Fprintf(stdout, "%s: %s skipped: prompted during delivery (attempt %d/%d)\n", label, sessName, attempts+1, idleClaimNudgeMaxAttempts) //nolint:errcheck // best-effort
			return true
		}
		fmt.Fprintf(stdout, "%s: %s failed: %v\n", label, sessName, err) //nolint:errcheck // best-effort
		return false
	}
	fmt.Fprintf(stdout, "%s: nudged %s for %s (attempt %d/%d)\n", label, sessName, target.ID, attempts+1, idleClaimNudgeMaxAttempts) //nolint:errcheck // best-effort
	return false
}

// backstopSeatWait is why a seat must not be nudged on this tick.
//
// The two refusals are reported apart because they must be PACED apart: one is
// a human the lane owes unlimited patience, the other is a broken observation
// that must never be allowed to withhold the lane's terminal action. Collapsing
// them into one bool is what made the probe-error case indistinguishable, so
// the distinction lives in the type rather than in the caller's guesswork.
type backstopSeatWait int

const (
	// backstopSeatReady means nothing blocks delivery.
	backstopSeatReady backstopSeatWait = iota
	// backstopSeatPrompted means the seat is sitting at an approval or
	// selection prompt and a human is being waited on.
	backstopSeatPrompted
	// backstopSeatProbeFailed means the probe itself could not answer, so the
	// seat's state is simply unknown.
	backstopSeatProbeFailed
)

// backstopSeatAwaitsHumanInput reports why, if at all, sessName must not be
// nudged right now. episode identifies the specific prompt a prompted seat is
// sitting at, so a caller can report one notice per prompt instead of one per
// tick; err carries the probe failure for backstopSeatProbeFailed.
//
// Refuses on probe error as well as on a confirmed prompt, via the shared
// runtime.PendingInteractionFor normalization. The two failure directions are
// not symmetric: skipping a nudge costs one tick, because the backstop
// re-evaluates the same seat on the next pass, while delivering into an open
// prompt answers on the operator's behalf and cannot be taken back.
func backstopSeatAwaitsHumanInput(sp runtime.Provider, sessName string) (wait backstopSeatWait, episode string, err error) {
	pending, err := runtime.PendingInteractionFor(sp, sessName)
	switch {
	case err != nil:
		return backstopSeatProbeFailed, "", err
	case pending != nil:
		return backstopSeatPrompted, pending.RequestID, nil
	}
	return backstopSeatReady, "", nil
}

// backstopPromptNoticeSeen remembers the prompt each lane last reported a seat
// as blocked on, so the unbounded prompted skip stays observable without
// emitting a line on every reconcile tick for as long as an operator takes to
// answer. Keyed by lane and seat; the entry is dropped on any tick that does
// not end in the prompted skip — including the ticks that filter the seat out
// before probing it — so the next prompt is reported again.
//
// The reclaim reaches exactly as far as the enumeration does. A seat whose
// session bead is closed while it sits at its prompt drops out of the snapshot
// and keeps its entry for the life of the process: a residue of one small entry
// per (lane, seat), and a stale-suppressing one. The episode is the prompt's
// RequestID, and on tmux — today the only runtime that ever reports a prompt —
// that is a content hash of the approval: "tmux-" followed by the first eight
// bytes of sha256(ToolName + "\x00" + Input), see approvalHash in
// internal/runtime/tmux. So a later prompt of the SAME shape on a reused seat
// name compares equal to the residue and is suppressed; only a differently
// shaped prompt re-announces.
var backstopPromptNoticeSeen sync.Map // label \x00 sessName -> episode

func backstopPromptNoticeKey(label, sessName string) string {
	return label + "\x00" + sessName
}

// backstopPromptNoticeIsNew reports whether this lane has yet to announce that
// sessName is blocked on this particular episode, and records that it has.
func backstopPromptNoticeIsNew(label, sessName, episode string) bool {
	previous, seen := backstopPromptNoticeSeen.Swap(backstopPromptNoticeKey(label, sessName), episode)
	return !seen || previous != episode
}

// backstopPromptNoticeForget clears the recorded episode for a seat this lane
// has just observed unblocked.
func backstopPromptNoticeForget(label, sessName string) {
	backstopPromptNoticeSeen.Delete(backstopPromptNoticeKey(label, sessName))
}
