package main

import (
	"context"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// Arm A12 and the zombie effect (CONTRACT v5 S5; plan C5c2): a row the
// allocation does not want awake whose runtime reads dead (a corpse, or a
// zombie: pane up, agent dead) is classified once per (row, runtime token),
// unless A21 closes it (deadRuntimeCloses): legacy closes an undesired row's
// dead runtime unclassified, and a quarantine would hold its slot.
// The effect reads the runtime fresh and peeks the row's own dead pane, and
// classifies it as the start's recycle does (deadScreen): a terminal provider
// error parks the row, a rate-limit screen of a crash candidate quarantines
// it, other output of a zombie, or its terminal error, records
// session.crashed (SESS-088..090, SESS-530). It never stops a pane: teardown is the close's, the drain's and
// the start's. Whatever it finds, a runtime no longer dead, another row's,
// or a pane with nothing on it, it records the pair and settles, so the row
// falls through to the later arms; only a read or peek that fails refuses.

const decideZombie = "zombie-classify"

// armZombie is A12: a dead row not wanted awake, which A21 does not close,
// whose runtime the planner has not classified (World.Classified) proposes
// the zombie effect.
func armZombie(r *rowFacts) (intent, bool) {
	e := r.entry
	if e == nil || e.Liveness != livenessDead || e.Desired == desireWake || r.deadRuntimeCloses() {
		return intent{}, false // A21 closes it, unclassified, as legacy does
	}
	if c, ok := r.w.Classified[r.k]; ok && !c.Recycle && c.classifies(r.w.Observed[r.k].Identity) {
		return intent{}, false // the start's recycle's classification A12 adopts
	}
	r.rests |= restRuntime
	basis := rowBasis{Incarnation: r.row.Incarnation, InstanceToken: r.row.InstanceToken}
	return intent{Kind: intentZombie, Reason: decideZombie, Basis: basis}, true
}

// zombieSections are the zombie effect's one section, with the crash
// telemetry for its Call.
var zombieSections = []section{calledWhen(0, probed(peekDead, zombieStep), zombieCrash.found, recordZombieCrash)}

// zombieCrash is the crash the classification found, for legacy's crash
// metric.
type zombieCrash struct{ agent, output string }

func (c zombieCrash) found() bool { return c.agent != "" }

// recordZombieCrash records the crash metric, as legacy's zombie capture
// and the start's recycle do.
func recordZombieCrash(_ context.Context, _ txCaps, c zombieCrash) (struct{}, error) {
	recordAgentCrash(context.Background(), c.agent, c.output)
	return struct{}{}, nil
}

// zombieStep is the classification on the fresh read and the peek of the
// row's own dead pane. A zombie's crash output, or its terminal error
// (legacy's zombie capture records both), records session.crashed and the
// crash metric.
func zombieStep(v txView, peek deadPeek, peekErr error) txStep {
	done := effectFacts{Classified: &classifiedRuntime{Key: v.It.Key, Token: deadToken(v.RT.Identity)}}
	dead := v.RT.Class == rtCorpse || v.RT.Class == rtZombie
	switch {
	case dead && !v.RT.Same:
		return txStep{Refuse: causeLivenessUnknown} // the object changed across the read
	case !dead, !peek.read && peekErr == nil:
		return txStep{Done: true, Facts: done} // nothing of this row's to classify
	case peekErr != nil:
		return txStep{Refuse: causePeekError, Err: peekErr}
	case peek.known != nil:
		// The start's recycle classified this dead runtime and recorded the
		// crash it found, its kill refused: A12 adopts the classification
		// without peeking again, records that crash once, and records the
		// pair as its own, which A6's heals then take (CONTRACT A6, v5.9m).
		step := txStep{Done: true, Facts: done}
		if c := peek.known.Crash; c != nil {
			step.Facts.Events = []events.Event{*c}
			step.Done, step.Last, step.Pass = false, true, zombieCrash{agent: c.Subject, output: c.Message}
		}
		return step
	case strings.TrimSpace(peek.content) == "":
		return txStep{Done: true, Facts: done}
	}
	step := txStep{Done: true, Facts: done}
	screen, screened := deadScreen(v, peek.content)
	switch {
	case screened && parkedTerminal(v.Row) && screen.patch["sleep_reason"] == string(session.SleepReasonProviderTerminalError):
		return step // already parked
	case screened:
		step.Write, step.Class = screen.patch, screen.class
	}
	if v.RT.Class != rtZombie || runtime.ContainsProviderRateLimitScreen(peek.content) && screen.patch["sleep_reason"] != string(session.SleepReasonProviderTerminalError) {
		return step // legacy records a crash for a zombie alone, and never a rate-limit screen's
	}
	res, _ := v.World.Templates.lookup(v.Row)
	crash := events.Event{
		Type: events.SessionCrashed, Actor: "gc", Subject: res.TP.DisplayName(),
		Message: truncateCrashPaneOutput(peek.content, crashEventPaneOutputMaxLines),
		Payload: api.SessionLifecyclePayloadJSON(v.Row.ID, res.TP.TemplateName, "zombie process"),
	}
	step.Facts.Events = []events.Event{crash}
	step.Done, step.Last, step.Pass = false, true, zombieCrash{agent: crash.Subject, output: crash.Message}
	return step
}
