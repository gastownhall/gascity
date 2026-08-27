package main

// The drain reminder: a stop nobody was ever asked to make.
//
// A drain-acked session is moved to the durable stop-pending state and its
// runtime is stopped asynchronously. When that stop does not take — the pane
// survives the signal, the process reparents, the provider refuses — the row
// stays `state=draining, state_reason=drain-ack-stop-pending` with a LIVE
// runtime, and the controller re-queues the same stop on every tick, forever.
// Nothing in that loop ever tells the AGENT anything. It cannot: the drain
// signal is metadata, learned by polling, and the session this happens to is
// the one sitting idle at its prompt not polling anything.
//
// So the loop has no exit but an operator killing the pane. This pass adds the
// one thing missing from it: ask the agent. It runs off the same durable row
// state the re-examination already reads, in the bounded shape the two nudge
// backstops use (observe, remind, back off, give up), and it is purely
// informational — it never stops anything.
//
// # It is anchored on the ROW, not on the drain tracker
//
// The in-memory drainTracker is the wrong anchor and an earlier revision of
// this file proved it: a tracked drain is tracker-resident for about two
// advances before the stop-pending handover clears it, the whole tracked phase
// is bounded by defaultDrainTimeout, and a controller restart drops the tracker
// entirely. The wedged rows — the ones that sit for days — have no tracker
// entry at all. Everything here therefore keys off the session bead and the
// durable stop-pending state, which are exactly what survives.
//
// # The reminder carries the identity because the pane's environment may not
//
// `gc runtime drain-ack` with no argument binds the acknowledgement through the
// caller's own GC_SESSION_ID/GC_INSTANCE_TOKEN. An adopted pane whose
// environment did not survive a restart therefore acks as nobody, which reads
// downstream as no acknowledgement at all. The reminder text names the session
// id so the agent runs the explicit-argument form, which resolves the target
// from the store instead of from the pane.
//
// # Idleness comes from the runtime, not from a cache
//
// The quiet test reads the provider's activity signal directly (raw tmux window
// activity on the tmux provider), never the observation cache: on an idle pane
// that cache can read hours fresher than tmux does (ga-gg4mv), which would hold
// the reminder forever on exactly the sessions it exists for. An unreadable
// signal HOLDS rather than proceeds — the #312 rule that "we cannot tell" is
// never "idle".
//
// Known bounded exposure: the k8s provider's GetLastActivity shells
// `tmux display-message -p '#{session_activity}'` inside the pod, and
// session-level activity does not advance on pane I/O for a detached session
// (see Tmux.rawSessionActivity, which queries per-window activity for exactly
// this reason). A busy k8s agent therefore reads as idle-since-attach and can
// be nudged mid-turn. Every other provider is fail-closed. The exposure is
// capped at drainReminderMaxAttempts informational nudges per drain and is
// tracked as its own fix rather than papered over here.

import (
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/runtime"
	sessions "github.com/gastownhall/gascity/internal/session"
)

// Pacing. There is no observe-first grace: the caller only reaches this pass
// for a row that is ALREADY durably wedged — drain-acked, stop-pending, and
// still alive after its stop was queued — so the first sight of one is already
// late. After that it is one reminder per drainReminderInterval, capped at
// drainReminderMaxAttempts so a session that will never answer costs a bounded
// number of nudges rather than one per tick forever.
const (
	drainReminderInterval    = 10 * time.Minute
	drainReminderGrace       = 2 * time.Minute
	drainReminderMaxAttempts = 3
)

// Session-bead metadata keys. Persisted for the reason every sibling backstop
// persists its pacing state: a controller restart must RESUME the state
// machine, never replay it.
const (
	drainReminderCountKey = "drain_reminder_count"
	// drainReminderFailedKey counts the attempts whose delivery FAILED. The
	// spend is what bounds the loop and it is never unwound — a permanently
	// input-dead pane must not re-send forever — but a spent attempt that never
	// arrived is not a reminder anybody ignored, and the escalation's record has
	// to be able to tell those apart.
	drainReminderFailedKey = "drain_reminder_failed"
	drainReminderAtKey     = "drain_reminder_at"
	// drainReminderDrainKey scopes the budget to ONE drain of one incarnation.
	// Binding to the instance token alone was not enough: a canceled drain
	// leaves its markers on a session that goes back to work, and the next
	// drain of that same incarnation would inherit a spent budget it never
	// earned — skipping its reminders, or (on the enterprise line) authorizing
	// an escalation for a drain nobody was ever asked about.
	drainReminderDrainKey = "drain_reminder_drain"
	// drainReminderHoldKey names why the most recent DUE evaluation did not
	// remind. Written on transition only, so a session held for hours costs one
	// write, not one per tick.
	drainReminderHoldKey = "drain_reminder_hold"
)

// Hold reasons recorded in drainReminderHoldKey.
const (
	drainReminderHoldBusy       = "busy"
	drainReminderHoldAttached   = "attached"
	drainReminderHoldUnreadable = "activity_unreadable"
	drainReminderHoldAckUnknown = "ack_unreadable"
	drainReminderHoldExhausted  = "attempts_exhausted"
)

// drainReminderLabel prefixes this pass's journal lines so they are greppable
// next to the sibling backstops' own labeled output.
const drainReminderLabel = "drain-reminder"

// drainReminderOutcome is what one row's reminder evaluation did.
type drainReminderOutcome int

const (
	// drainReminderSkipped: nothing was owed (not this pass's row, not due, or
	// already acknowledged). No write of any kind.
	drainReminderSkipped drainReminderOutcome = iota
	// drainReminderHeld: a reminder was due but the evidence said hold.
	drainReminderHeld
	// drainReminderDelivered: a reminder was reserved and delivered.
	drainReminderDelivered
	// drainReminderExhausted: the budget is spent and the row is still
	// unacknowledged. On the enterprise line this is the escalation's cue.
	drainReminderExhausted
)

// maybeRemindDrainingSession evaluates one durably stop-pending row whose
// runtime is still alive and, when all the preconditions hold, delivers one
// reminder nudge through the provider's hardened nudge path.
//
// The gates are ordered cheapest-first on purpose. This runs inside the
// reconcile tick, so the overwhelmingly common answer — "not due" — must cost
// one store read and zero provider round-trips; a tmux show-environment is a
// subprocess, and paying two of them per wedged row per tick would put the
// reconciler's throughput behind a wedged tmux server.
func maybeRemindDrainingSession(
	sp runtime.Provider,
	store beads.Store,
	info sessions.Info,
	clk clock.Clock,
	stdout io.Writer,
) drainReminderOutcome {
	if sp == nil || store == nil || clk == nil {
		return drainReminderSkipped
	}
	if stdout == nil {
		stdout = io.Discard
	}
	name := strings.TrimSpace(info.SessionNameMetadata)
	if name == "" || strings.TrimSpace(info.ID) == "" {
		return drainReminderSkipped
	}
	// Only the wedge class. A row that is not durably stop-pending is either
	// converging normally or belongs to a lane with its own convergence.
	if !isDrainAckStopPendingInfo(info) {
		return drainReminderSkipped
	}
	// Pool seats only. A named row's acknowledgement lane refuses it
	// (named_row, policy_unsupported) and converges it through legacy timeout
	// instead, so a reminder here would talk an agent into minting exactly the
	// agent provenance that routes its row into a refusal class — making a
	// named row's outcome WORSE than the behavior it has today.
	if !isPoolManagedSessionInfo(info) {
		return drainReminderSkipped
	}

	// One store read pays for both the pacing state and the drain identity.
	bead, err := store.Get(info.ID)
	if err != nil {
		return drainReminderSkipped
	}
	drainID := drainReminderIdentity(bead)
	if drainID == "" {
		return drainReminderSkipped // no drain to scope a budget to
	}
	attempts, failed, last := drainReminderState(bead, drainID)

	now := clk.Now()
	exhausted := attempts >= drainReminderMaxAttempts
	if !exhausted && attempts > 0 && now.Sub(last) < drainReminderInterval {
		return drainReminderSkipped // waiting out the cadence — free, and the common case
	}

	// From here something will be written, so the acknowledgement pin runs
	// first. A landed agent ack is the outcome this whole pass exists to
	// produce; a reminder that clobbers one — by bumping a counter, by leaving a
	// breadcrumb over it — converts a success into the refusal that wedges the
	// row. An UNREADABLE source holds rather than proceeds: this guard stands in
	// front of writes and a destructive-adjacent lane, so it fails closed.
	source, sourceErr := sp.GetMeta(name, reconcilerDrainAckSourceKey)
	if sourceErr != nil {
		return drainReminderHeld // no breadcrumb: writing one is itself a write
	}
	if strings.TrimSpace(source) == drainAckSourceAgentValue {
		return drainReminderSkipped
	}

	if exhausted {
		// Budget spent. Say so once — the transition is the observable fact, and
		// the caller decides whether anything escalates from here.
		if noteDrainReminderHold(store, info, drainReminderHoldExhausted) {
			fmt.Fprintf(stdout, //nolint:errcheck // best-effort
				"%s: drain reminders exhausted for %s after %s (still unacknowledged)\n",
				drainReminderLabel, name, drainReminderSpendPhrase(attempts, failed))
		}
		return drainReminderExhausted
	}

	if !sp.IsRunning(name) {
		return drainReminderSkipped // the stop took after all; the finalizer owns it
	}
	// A human at the pane is already handling this row; do not type into their
	// session.
	if sp.IsAttached(name) {
		noteDrainReminderHold(store, info, drainReminderHoldAttached)
		return drainReminderHeld
	}
	// Quiet check. An agent in the middle of a turn is answering its own drain;
	// an unreadable signal is not evidence that it is not.
	activity, activityErr := sp.GetLastActivity(name)
	if activityErr != nil || activity.IsZero() {
		noteDrainReminderHold(store, info, drainReminderHoldUnreadable)
		return drainReminderHeld
	}
	if now.Sub(activity) < drainReminderGrace {
		noteDrainReminderHold(store, info, drainReminderHoldBusy)
		return drainReminderHeld
	}

	// Write ahead of the delivery, exactly as the sibling backstops do: a crash
	// or a store failure between here and the nudge can cost one attempt, but it
	// can never replay an unbounded stream of them.
	if !writeDrainReminderMarker(store, info, drainID, attempts+1, failed, now, stdout) {
		return drainReminderSkipped
	}
	if err := sp.Nudge(name, runtime.TextContent(drainReminderContent(info))); err != nil {
		// The attempt is spent either way — the write-ahead above is what bounds
		// this loop, and unwinding it would let a permanently input-dead pane
		// re-send forever. Record instead that nothing ARRIVED, so the escalation
		// that follows reports what actually happened rather than three
		// conversations that never occurred.
		//
		// Limit worth knowing: a nil return is not proof of delivery. The seam
		// adapter answers nil when it cannot attach, so some failures are
		// invisible here and are counted as delivered. That is the best knowledge
		// available at this boundary, and it errs toward the gentler journal line.
		writeDrainReminderMarker(store, info, drainID, attempts+1, failed+1, now, stdout)
		fmt.Fprintf(stdout, //nolint:errcheck // best-effort
			"%s: reminder %d/%d to %s was undeliverable: %v\n",
			drainReminderLabel, attempts+1, drainReminderMaxAttempts, name, err)
		return drainReminderSkipped
	}
	clearDrainReminderHold(store, info)
	fmt.Fprintf(stdout, //nolint:errcheck // best-effort
		"%s: drain reminder %d/%d delivered to %s (stop-pending, unacked)\n",
		drainReminderLabel, attempts+1, drainReminderMaxAttempts, name)
	return drainReminderDelivered
}

// remindStopPendingDrain is the reconciler-facing entry point: it is called from
// the stop-pending re-examination, on the arm that has just observed the runtime
// still ALIVE after re-queueing its stop. That arm is the wedge, and it is the
// only caller — the pass owns no scan of its own.
func remindStopPendingDrain(sp runtime.Provider, store beads.Store, info sessions.Info, clk clock.Clock, stdout io.Writer) drainReminderOutcome {
	if clk == nil {
		clk = clock.Real{}
	}
	return maybeRemindDrainingSession(sp, store, info, clk, stdout)
}

// drainReminderIdentity names the ONE drain a budget belongs to: this
// incarnation, this drain. Both halves are required — the token alone lets a
// later drain inherit a canceled drain's spent budget, and drain_at alone would
// carry across a re-wake.
func drainReminderIdentity(bead beads.Bead) string {
	token := strings.TrimSpace(bead.Metadata["instance_token"])
	drainAt := strings.TrimSpace(bead.Metadata["drain_at"])
	if token == "" || drainAt == "" {
		return ""
	}
	return token + "/" + drainAt
}

// drainReminderContent is the reminder text. It names the session id twice on
// purpose: once so a human reading the pane knows which row this is about, and
// once inside the command so the agent runs the explicit-argument ack, which
// binds the requester from the store rather than from a pane environment that
// may not have survived adoption.
func drainReminderContent(info sessions.Info) string {
	id := strings.TrimSpace(info.ID)
	return fmt.Sprintf(
		"A drain of session %s (name %s) is pending and unacknowledged. "+
			"Finish nothing new. Run: gc runtime drain-ack %s — then exit.",
		id, strings.TrimSpace(info.SessionNameMetadata), id)
}

// drainReminderState reads the persisted pacing state, resetting it when the
// markers belong to a different drain than the one in front of us. failed
// counts the attempts whose delivery returned an error — spent, but never
// received.
func drainReminderState(bead beads.Bead, drainID string) (attempts, failed int, last time.Time) {
	if strings.TrimSpace(bead.Metadata[drainReminderDrainKey]) != drainID {
		return 0, 0, time.Time{}
	}
	return atoiOr0(bead.Metadata[drainReminderCountKey]),
		atoiOr0(bead.Metadata[drainReminderFailedKey]),
		parseRFC3339OrZero(bead.Metadata[drainReminderAtKey])
}

// drainRemindersSpent reports whether the budget for THIS drain is exhausted and
// nothing more is worth waiting for. It is the durable question the enterprise
// escalation asks of the markers this file writes.
//
// A delivered reminder earns a full interval to be answered. A budget in which
// every attempt was UNDELIVERABLE earns none: waiting out an answer window for
// messages that never arrived is waiting for nothing, and the pane that cannot
// take input is precisely the one no further attempt will reach.
func drainRemindersSpent(bead beads.Bead, now time.Time) bool {
	drainID := drainReminderIdentity(bead)
	if drainID == "" {
		return false
	}
	attempts, failed, last := drainReminderState(bead, drainID)
	if attempts < drainReminderMaxAttempts || last.IsZero() {
		return false
	}
	if failed >= drainReminderMaxAttempts {
		return true
	}
	return now.Sub(last) >= drainReminderInterval
}

// drainReminderSpendPhrase names what the budget was actually spent on, so a
// journal line never claims a conversation that did not happen. It is the whole
// point of tracking delivery separately from spend: the kill that follows is the
// right outcome for an input-dead idle pane either way, but the record of why
// has to be true.
func drainReminderSpendPhrase(attempts, failed int) string {
	switch {
	case failed >= drainReminderMaxAttempts:
		return fmt.Sprintf("%d undeliverable reminder attempts (input-dead pane)", failed)
	case failed > 0:
		return fmt.Sprintf("%d unanswered reminders (%d more undeliverable)", attempts-failed, failed)
	default:
		return fmt.Sprintf("%d unanswered reminders", attempts)
	}
}

// drainReminderSpendPhraseFor is the bead-facing form for callers that hold the
// row rather than the counts.
func drainReminderSpendPhraseFor(bead beads.Bead) string {
	attempts, failed, _ := drainReminderState(bead, drainReminderIdentity(bead))
	return drainReminderSpendPhrase(attempts, failed)
}

func writeDrainReminderMarker(store beads.Store, info sessions.Info, drainID string, attempts, failed int, now time.Time, stdout io.Writer) bool {
	err := store.SetMetadataBatch(info.ID, map[string]string{
		drainReminderCountKey:  strconv.Itoa(attempts),
		drainReminderFailedKey: strconv.Itoa(failed),
		drainReminderAtKey:     now.UTC().Format(time.RFC3339),
		drainReminderDrainKey:  drainID,
	})
	if err != nil {
		fmt.Fprintf(stdout, "%s: marking %s failed: %v\n", drainReminderLabel, info.ID, err) //nolint:errcheck // best-effort
		return false
	}
	return true
}

// noteDrainReminderHold records why this evaluation held, and reports whether
// that was a TRANSITION. Steady-state holds are write-free, so a row parked on
// the same reason for a day leaves one breadcrumb rather than a day of them.
func noteDrainReminderHold(store beads.Store, info sessions.Info, reason string) bool {
	bead, err := store.Get(info.ID)
	if err == nil && strings.TrimSpace(bead.Metadata[drainReminderHoldKey]) == reason {
		return false
	}
	if err := store.SetMetadataBatch(info.ID, map[string]string{drainReminderHoldKey: reason}); err != nil {
		log.Printf("%s: recording hold %q for %s: %v", drainReminderLabel, reason, info.ID, err)
		return false
	}
	return true
}

func clearDrainReminderHold(store beads.Store, info sessions.Info) {
	bead, err := store.Get(info.ID)
	if err != nil || strings.TrimSpace(bead.Metadata[drainReminderHoldKey]) == "" {
		return
	}
	_ = store.SetMetadataBatch(info.ID, map[string]string{drainReminderHoldKey: ""})
}
