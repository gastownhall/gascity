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
// poolClaimBackstop (idle_nudge.go) is the first predicate. A second, for
// named/direct startup kickoff, is tracked as a separate bead rather than
// built here — this engine exists because two concrete predicates are now
// in scope, not speculatively ahead of them.
type backstopPredicate interface {
	// governs reports whether this predicate applies to the session bead at
	// all.
	governs(s beads.Bead) bool

	// outstandingID resolves the id of the work item sessName is waiting on.
	// ok is false when nothing is outstanding, in which case clear is
	// invoked to wipe any persisted state.
	outstandingID(s beads.Bead, work map[string]beads.Bead, sessName string) (id string, ok bool)

	// state reads the persisted pacing state for id. same is false when id
	// is an assignment not yet observed, in which case the engine calls
	// observe to (re)start the grace clock instead of consulting attempts.
	state(s beads.Bead, id string) (same bool, attempts int, last time.Time)

	// content resolves the text to nudge with, or "" to skip silently.
	content(s beads.Bead) string

	// observe persists the start of a new assignment's grace window.
	observe(store beads.Store, s *beads.Bead, id string, now time.Time, stdout io.Writer)

	// record persists a delivered nudge attempt.
	record(store beads.Store, s *beads.Bead, id string, attempts int, now time.Time, stdout io.Writer)

	// exhausted is invoked once attempts reach the shared max attempts.
	exhausted(store beads.Store, s *beads.Bead, stdout io.Writer)

	// clear wipes persisted state once nothing is outstanding.
	clear(store beads.Store, s *beads.Bead, stdout io.Writer)
}

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
// nudgeStalledPoolClaims. attempts is the number of nudges already delivered
// for the current assignment; last is the time of the last attempt, or of
// first observation when attempts is 0. Pacing reuses the exact constants
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

		id, ok := pred.outstandingID(*s, workByID, sessName)
		if !ok {
			pred.clear(store, s, stdout)
			continue
		}

		same, attempts, last := pred.state(*s, id)
		if !same {
			// First observation of this assignment: start the grace clock,
			// don't nudge yet — a normal claim/confirmation almost always
			// lands within the grace window.
			pred.observe(store, s, id, now, stdout)
			continue
		}

		switch decideBackstopAction(attempts, last, now) {
		case backstopActionWait:
			continue
		case backstopActionExhausted:
			pred.exhausted(store, s, stdout)
			continue
		case backstopActionNudge:
			content := pred.content(*s)
			if content == "" {
				continue
			}
			if err := sp.Nudge(sessName, runtime.TextContent(content)); err != nil {
				fmt.Fprintf(stdout, "%s: %s failed: %v\n", label, sessName, err) //nolint:errcheck // best-effort
				continue
			}
			fmt.Fprintf(stdout, "%s: nudged %s for %s (attempt %d/%d)\n", label, sessName, id, attempts+1, idleClaimNudgeMaxAttempts) //nolint:errcheck // best-effort
			pred.record(store, s, id, attempts+1, now, stdout)
		}
	}
}
