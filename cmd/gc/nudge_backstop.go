package main

import (
	"fmt"
	"io"
	"strings"
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
// poolClaimBackstop and poolContinuationBackstop (idle_nudge.go) are the two
// predicates: initial trigger delivery and later graph-v2 successor delivery.
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

	// reserve durably records a nudge attempt before delivery. false means the
	// write failed and the provider must not be nudged.
	reserve(store beads.Store, s *beads.Bead, target backstopTarget, attempts int, now time.Time, stdout io.Writer) bool

	// exhausted is invoked once attempts reach the shared max attempts.
	exhausted(store beads.Store, s *beads.Bead, stdout io.Writer)

	// clear wipes persisted state once nothing is outstanding.
	clear(store beads.Store, s *beads.Bead, stdout io.Writer)
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
// in logs.
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
			continue
		}

		target, resolution := pred.resolve(*s, workByID, sessName)
		switch resolution {
		case backstopResolutionHold:
			continue
		case backstopResolutionClear:
			pred.clear(store, s, stdout)
			continue
		case backstopResolutionOutstanding:
			// Continue below.
		default:
			continue
		}

		same, attempts, last := pred.state(*s, target)
		if !same {
			// First observation of this assignment: start the grace clock,
			// don't nudge yet — a normal claim/confirmation almost always
			// lands within the grace window.
			pred.observe(store, s, target, now, stdout)
			continue
		}

		switch decideBackstopAction(attempts, last, now) {
		case backstopActionWait:
			continue
		case backstopActionExhausted:
			pred.exhausted(store, s, stdout)
			continue
		case backstopActionNudge:
			switch pred.revalidate(target) {
			case backstopResolutionHold:
				continue
			case backstopResolutionClear:
				pred.clear(store, s, stdout)
				continue
			case backstopResolutionOutstanding:
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
				pred.exhausted(store, s, stdout)
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
