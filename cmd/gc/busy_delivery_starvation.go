package main

import (
	"context"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

// The starvation rule for a delivery to a session whose activity clock never
// quiets (#8544), and the program record's mid-turn hold on it (#8840). Both
// delivery paths judge it here: the legacy poller's starved queue
// (queuedNudgeGate, admit) and the conversation lane's keystroke boundary
// (conversationLaneExec.starvedBoundary). Nothing here is queue-shaped.

const (
	// queuedNudgeStarvedAfter is how long a queued item waits on a session
	// whose activity clock never quiets before the delivery carrying it
	// stops waiting on that clock (ga-atd7r2): a session that stays
	// continuously busy — a repainting idle TUI, or back-to-back turns —
	// otherwise starves its queue, because the quiescence gate never
	// clears for it. It sits above the #5317 busy-skip fixture (1 minute),
	// so a fresh busy deferral still reads as a quiet-session courtesy
	// skip, not a starvation.
	queuedNudgeStarvedAfter = 2 * time.Minute

	// queuedNudgeTurnLookTimeout bounds the protocol turn read a starved
	// queue takes before it goes (starvedQueuedNudgeDelivery): t3bridge
	// answers it over the network, and the dispatcher runs on the
	// controller's loop.
	queuedNudgeTurnLookTimeout = 5 * time.Second
)

// starvedQueuedNudgeDelivery is how a starved queue goes to sp's session
// name, one its activity clock calls busy (queuedNudgeStarvedForTarget),
// decided by one look at the turn of the backend serving it
// (runtime.ResolveBackend). hold reports that a turn runs. A look that fails
// is never an idle reading: it holds too, and err says why.
//
//   - A leaf that reads its turn on the screen (runtime.TurnRunningProvider)
//     and sees none delivers immediately: the idle question the default path
//     would wait out is already answered.
//   - A leaf that reports its turn by protocol (runtime.TurnFenceProvider:
//     ACP, t3bridge) and has none in progress takes the default path. Sent
//     into a running turn, ACP's nudge would hold the dispatch for its whole
//     busy wait (nudge_busy_timeout), and t3bridge's would start a second
//     turn.
//   - A leaf with neither reading takes the default path unread, as the open
//     gate would send it: holding it would leave the starvation in place.
//
// The gate and admit ask through starvedQueuedNudgeBypass, which adds the
// program record's veto on the screen's bypass.
func starvedQueuedNudgeDelivery(sp runtime.Provider, name string) (delivery worker.NudgeDelivery, hold bool, err error) {
	if tr, ok := runtime.TurnRunningFor(sp, name); ok {
		running, err := tr.TurnRunning(name)
		if err != nil {
			return "", true, fmt.Errorf("reading the turn of %q: %w", name, err)
		}
		return worker.NudgeDeliveryImmediate, running, nil
	}
	leaf, _, _ := runtime.ResolveBackend(sp, name)
	tf, ok := leaf.(runtime.TurnFenceProvider)
	if !ok {
		return worker.NudgeDeliveryDefault, false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), queuedNudgeTurnLookTimeout)
	defer cancel()
	turn, err := tf.ObserveTurn(ctx, name)
	if err != nil {
		return "", true, fmt.Errorf("reading the turn of %q: %w", name, err)
	}
	return worker.NudgeDeliveryDefault, turn.Active(), nil
}

// starvedQueuedNudgeBypass is starvedQueuedNudgeDelivery for target, the
// one path that types a queued delivery into a session its activity clock
// calls busy: a starved queue bypassing the quiescence gate on the screen's
// turn look (an immediate delivery). Under the program-status switch on, a
// target whose provider declares hold_mid_turn (its program aborts a turn
// input is typed into) has that bypass vetoed while a fresh read of its
// program's record says a turn is running (runtime.ProgramRecordTypingHold):
// the screen can show no turn while one runs (the 2026-11-04 Tier-4 cohort:
// 9 of 16 long Codex turns aborted by this bypass). A vetoed bypass holds,
// as a turn on screen does: nothing is claimed or charged, and the next pass
// asks again. Every other delivery, the quiescence gate's own included,
// decides as without the record. Under the shadow switch the bypass's
// verdict is recorded beside the record's (starved_bypass), read fresh.
func starvedQueuedNudgeBypass(target nudgeTarget, sp runtime.Provider, now time.Time) (worker.NudgeDelivery, bool, error) {
	how, hold, _, err := starvedBypassLook(target, sp, now)
	return how, hold, err
}

// starvedBypassLook is starvedQueuedNudgeBypass's look, which also reports
// whether the fresh read of the program's record it took decided
// (runtime.ProgramStatus.Decides): it takes one only for an immediate bypass
// the screen allows, of a target that declares hold_mid_turn under the
// switch on (programHoldDeclared), and decided is false wherever it took
// none.
func starvedBypassLook(target nudgeTarget, sp runtime.Provider, now time.Time) (how worker.NudgeDelivery, hold, decided bool, err error) {
	how, hold, err = starvedQueuedNudgeDelivery(sp, target.sessionName)
	if err != nil || how != worker.NudgeDeliveryImmediate {
		return how, hold, false, err
	}
	scrape := runtime.ProgramVerdictNoHold
	if hold {
		scrape = runtime.ProgramVerdictHold
	}
	runtime.ShadowCompareFresh(sp, target.sessionName, target.sessionID, target.providerFamily(),
		runtime.ProgramPointStarvedBypass, scrape, now, runtime.ProgramTurnHoldVerdict)
	if hold || !target.programHoldDeclared() {
		return how, hold, false, nil
	}
	r, held := runtime.ProgramRecordTypingHold(sp, target.sessionName, now)
	return how, held, r.Status.Decides(), nil
}

// programHoldDeclared reports whether target's resolved provider declares
// hold_mid_turn under the city switch on and its transport (the arm every
// start derives, config.ProgramStatusArmFor).
func (t nudgeTarget) programHoldDeclared() bool {
	if t.cfg == nil || t.resolved == nil || t.cfg.Session.ProgramStatusMode() != runtime.ProgramStatusOn {
		return false
	}
	arm, _ := config.ProgramStatusArmFor(t.cfg, t.resolved, t.sessionTransport())
	return arm.HoldMidTurn
}
