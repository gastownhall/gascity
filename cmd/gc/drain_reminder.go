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
// Known bounded exposure: the k8s and ssh providers' GetLastActivity shell
// `tmux display-message -p '#{session_activity}'` inside the pod, and
// session-level activity does not advance on pane I/O for a detached session
// (see Tmux.rawSessionActivity, which queries per-window activity for exactly
// this reason). A busy k8s agent therefore reads as idle-since-attach and can
// be nudged mid-turn. The ssh provider has the same exposure for the same
// reason (internal/runtime/ssh/provider.go reads #{session_activity} and
// reports CanReportActivity: true). Every other provider is fail-closed. The
// exposure is capped at drainReminderMaxAttempts informational nudges per drain
// and is tracked as its own fix rather than papered over here.

import (
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/opjournal"
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
	// drainReminderSettleWindow bounds how long the last attempt's notice
	// holds the exhaustion and the escalation after it was spent: its start_by
	// falls inside drainReminderInterval, and a notice the lane is still
	// delivering then has at most one lease more (opjournal.DefaultLeaseTTL).
	drainReminderSettleWindow = drainReminderInterval + opjournal.DefaultLeaseTTL
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
	// drainReminderSettledKey counts the attempts drainReminderFailedKey
	// accounts for. A direct nudge's fate is known when it is spent; a lane
	// notice's only once the lane has had its chance at it, so the lane path
	// settles an attempt when its successor is spent or, for the last one,
	// before the budget is announced exhausted (settleLastDrainReminder).
	// Absent, as on markers written before it existed, every attempt is
	// settled.
	drainReminderSettledKey = "drain_reminder_settled"
	drainReminderAtKey      = "drain_reminder_at"
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
	drainReminderHoldBusy          = "busy"
	drainReminderHoldAttached      = "attached"
	drainReminderHoldAttachUnknown = "attach_unknown"
	drainReminderHoldUnreadable    = "activity_unreadable"
	drainReminderHoldExhausted     = "attempts_exhausted"
	drainReminderHoldAckUnknown    = "ack_unreadable"
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
	// drainReminderProposed: a reminder was proposed as a planner notice op
	// and reserved; the conversation lane delivers it at the session's
	// boundary.
	drainReminderProposed
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
	if !drainReminderEligible(info, name) {
		return drainReminderSkipped
	}

	// One store read pays for both the pacing state and the drain identity, and
	// the free cadence wait — the overwhelmingly common answer — returns here
	// before any provider round-trip.
	due, ok := loadDrainReminderDue(store, info, clk)
	if !ok {
		return drainReminderSkipped
	}

	// From here something will be written, so the acknowledgement pin runs
	// first: a reminder that clobbers a landed agent ack converts the success
	// this pass exists to produce into the refusal that wedges the row.
	if outcome, proceed := drainReminderAckPin(sp, store, info, name, due.token); !proceed {
		return outcome
	}

	if due.exhausted {
		return announceDrainReminderExhausted(store, info, name, due, stdout)
	}

	if outcome, proceed := drainReminderQuietHold(sp, store, info, name, due.now); !proceed {
		return outcome
	}

	return deliverDrainReminder(sp, store, info, name, due, stdout)
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

// drainReminderNoticePurpose names the drain reminder as a planner notice.
const drainReminderNoticePurpose = "drain-reminder"

// laneDrainNoticeClass is the drain reminder's notice op as the conversation
// lane's content class: the one content a draining row admits (I7's
// exemption, session.GateContentDrainNotice). It matches the policy
// plannerNoticeRequest gives every planner notice, from this purpose's
// principal.
var laneDrainNoticeClass = opjournal.ContentClass{
	Role:      opjournal.RoleNotice,
	When:      opjournal.WhenBoundary,
	Principal: plannerNoticePrincipalPrefix + drainReminderNoticePurpose,
}

// drainReminderDue is the loaded pacing state for a row that has cleared the
// cheap eligibility gates and is due for evaluation. now is sampled once so
// every downstream gate reasons from the same instant.
type drainReminderDue struct {
	drainID string
	// token is the incarnation the budget is scoped to. The ack pin binds
	// against the same incarnation, but the two scopes are not the same scope:
	// the budget is keyed on token + "/" + drain_at (drainReminderIdentity),
	// while the binding is keyed on the token alone. They agree within a single
	// drain and can disagree across a re-drain of one incarnation, where a fresh
	// budget meets an acknowledgement the previous drain left bound to the same
	// still-live token.
	// Non-empty by construction: drainID is only non-empty when this is.
	token string
	// row is the session bead the state was loaded from, which a planner
	// notice is proposed against.
	row      beads.Bead
	attempts int
	failed   int
	// settled is how many of attempts failed accounts for: attempts, or
	// one less while the last lane notice's fate is unread.
	settled   int
	last      time.Time
	now       time.Time
	exhausted bool
}

// drainReminderEligible reports whether info names a row this pass may ever
// remind: it must carry an id, be durably drain-ack stop-pending (the wedge
// class), and occupy a pool seat. A row that is not durably stop-pending is
// converging on its own; a named row's acknowledgement lane refuses the agent
// provenance a reminder would mint (named_row, policy_unsupported) and converges
// through legacy timeout, so reminding one makes its outcome WORSE than today.
func drainReminderEligible(info sessions.Info, name string) bool {
	if name == "" || strings.TrimSpace(info.ID) == "" {
		return false
	}
	if !isDrainAckStopPendingInfo(info) {
		return false
	}
	return isPoolManagedSessionInfo(info)
}

// loadDrainReminderDue reads the one bead that carries both the drain identity
// and the pacing markers, then applies the free cadence gate. ok is false when
// nothing is owed yet: the row cannot be read, has no drain to scope a budget
// to, or is still waiting out its interval — the common, zero-provider-cost case.
func loadDrainReminderDue(store beads.Store, info sessions.Info, clk clock.Clock) (drainReminderDue, bool) {
	bead, err := store.Get(info.ID)
	if err != nil {
		return drainReminderDue{}, false
	}
	drainID := drainReminderIdentity(bead)
	if drainID == "" {
		return drainReminderDue{}, false // no drain to scope a budget to
	}
	attempts, failed, settled, last := drainReminderState(bead, drainID)
	now := clk.Now()
	exhausted := attempts >= drainReminderMaxAttempts
	if !exhausted && attempts > 0 && now.Sub(last) < drainReminderInterval {
		return drainReminderDue{}, false // waiting out the cadence — free, and the common case
	}
	return drainReminderDue{
		drainID:   drainID,
		token:     strings.TrimSpace(bead.Metadata["instance_token"]),
		row:       bead,
		attempts:  attempts,
		failed:    failed,
		settled:   settled,
		last:      last,
		now:       now,
		exhausted: exhausted,
	}, true
}

// drainReminderAckPin reads the acknowledgement source ahead of any write. It
// stands in front of writes and a destructive-adjacent lane, so it fails closed:
// proceed is false when an UNREADABLE source holds without a breadcrumb (writing
// one is itself a write over a row whose state could not be read), and when a
// landed agent ack — the outcome this whole pass exists to produce — must not be
// clobbered.
//
// An agent ack counts only for the incarnation that WROTE it. Pane environment
// is per-chair state and pool chairs are recycled under the same name, so a
// previous occupant's acknowledgement sits there for whoever sits down next.
// Read unbound, that residue says "the agent already answered" about an agent
// that no longer exists — and because a skip writes nothing, it says it in
// total silence, which is how a chair stays wedged for hours with no reminder
// line and no marker anywhere (ga-o6uw0).
func drainReminderAckPin(sp runtime.Provider, store beads.Store, info sessions.Info, name, rowToken string) (drainReminderOutcome, bool) {
	source, err := sp.GetMeta(name, reconcilerDrainAckSourceKey)
	if err != nil {
		return drainReminderHeld, false // no breadcrumb: writing one is itself a write
	}
	if strings.TrimSpace(source) != drainAckSourceAgentValue {
		return drainReminderSkipped, true
	}
	binding, bindErr := classifyAgentDrainAckBinding(sp, name, rowToken)
	if bindErr != nil {
		// Source says an agent answered but the binding is unreadable. Hold: an
		// unreadable binding is not evidence the ack is stale, and nudging over a
		// genuine acknowledgement is what this pin exists to prevent. Unlike the
		// source read above, this arm leaves a breadcrumb — the silent decline is
		// what hid the wedge.
		noteDrainReminderHold(store, info, drainReminderHoldAckUnknown)
		return drainReminderHeld, false
	}
	if binding == agentAckBindingCurrent {
		return drainReminderSkipped, false
	}
	// Stale or unprovable: keep asking. This arm is informational, so the worst
	// case is a redundant nudge to an agent that already answered, against a
	// suppressed one costing a chair for hours.
	return drainReminderSkipped, true
}

// agentAckBinding is what an agent-sourced acknowledgement on the pane proves
// about the incarnation the row currently describes. Three answers, not two:
// the difference between "provably somebody else's" and "cannot tell" is what
// lets each reader fail in its own direction.
type agentAckBinding int

const (
	// agentAckBindingUnprovable: the acknowledgement carries no requester token
	// to bind. Acks written before the stamp existed land here, and so does an
	// ack from a degraded pane whose GC_INSTANCE_TOKEN did not survive adoption
	// — the population the reminder text's explicit-argument form exists for.
	agentAckBindingUnprovable agentAckBinding = iota
	// agentAckBindingCurrent: the requester token is this row's token.
	agentAckBindingCurrent
	// agentAckBindingStale: the requester token names a DIFFERENT incarnation.
	// Positive proof of residue on a recycled chair.
	agentAckBindingStale
)

// classifyAgentDrainAckBinding binds an agent-sourced acknowledgement to the
// incarnation the row describes. `gc runtime drain-ack` stamps a digest of the
// requester's own instance token beside the source (setDrainAck), so the
// comparison is against evidence the acknowledging agent wrote about itself.
//
// The row carries the token in the clear, so the digest is taken here — AFTER
// the empty check, so an absent stamp or an unknown row token still lands on the
// unprovable arm instead of comparing two digests of "". Digesting rather than
// comparing raw keeps the capability off the pane; see
// drainAckInstanceTokenDigest.
//
// A read ERROR is distinct from an absent value and is returned as such, so a
// caller can tell "no stamp" from "could not look".
func classifyAgentDrainAckBinding(sp runtime.Provider, name, rowToken string) (agentAckBinding, error) {
	requester, err := sp.GetMeta(name, drainAckRequesterInstanceTokenKey)
	if err != nil {
		return agentAckBindingUnprovable, err
	}
	requester = strings.TrimSpace(requester)
	rowToken = strings.TrimSpace(rowToken)
	if requester == "" || rowToken == "" {
		return agentAckBindingUnprovable, nil
	}
	if requester == drainAckInstanceTokenDigest(rowToken) {
		return agentAckBindingCurrent, nil
	}
	return agentAckBindingStale, nil
}

// announceDrainReminderExhausted records the spent-budget transition once — the
// transition is the observable fact, and the caller decides whether anything
// escalates from a drain that is still unacknowledged. It waits, holding,
// until the last attempt is settled (settleLastDrainReminder).
func announceDrainReminderExhausted(store beads.Store, info sessions.Info, name string, due drainReminderDue, stdout io.Writer) drainReminderOutcome {
	failed, settled := settleLastDrainReminder(store, info, name, due, stdout)
	if !settled {
		return drainReminderHeld
	}
	if noteDrainReminderHold(store, info, drainReminderHoldExhausted) {
		fmt.Fprintf(stdout, //nolint:errcheck // best-effort
			"%s: drain reminders exhausted for %s after %s (still unacknowledged)\n",
			drainReminderLabel, name, drainReminderSpendPhrase(due.attempts, failed))
	}
	return drainReminderExhausted
}

// settleLastDrainReminder accounts for the last attempt's lane notice before
// the budget is announced exhausted, and reports the failed count and whether
// the attempt is settled. While the notice stands (in flight, or open and
// still due) it is not settled, so the exhaustion and the escalation
// (drainRemindersSpent) wait for it to have its chance, for at most
// drainReminderSettleWindow after it was spent. A notice that closed without
// a delivery, or still stands past that window, counts as undeliverable. A
// failed read holds within the window, logged at most once per
// laneErrorLogInterval, and past it settles on what is known. Without a lane
// host the notice cannot be read and the attempt settles as it stands.
func settleLastDrainReminder(store beads.Store, info sessions.Info, name string, due drainReminderDue, stdout io.Writer) (int, bool) {
	if due.settled >= due.attempts {
		return due.failed, true
	}
	failed := due.failed
	withinWindow := due.now.Before(due.last.Add(drainReminderSettleWindow))
	if lane := plannerNoticeLaneFor(store); lane != nil {
		n := drainReminderNotice(info, due, due.attempts)
		failKey := plannerNoticeTallyKey(info.ID, n, "exhausted")
		fate, err := plannerNoticeAttemptFate(lane, due.row, n, due.now)
		switch {
		case err != nil:
			if _, logNow := lane.noticeFailed(failKey); logNow {
				fmt.Fprintf(stdout, "%s: reading the last reminder's notice for %s failed: %v\n", drainReminderLabel, name, err) //nolint:errcheck // best-effort
			}
			if withinWindow {
				return 0, false
			}
		case fate == plannerNoticeFateStands && withinWindow:
			lane.noticeRecovered(failKey)
			return 0, false
		case fate == plannerNoticeFateUndelivered, fate == plannerNoticeFateStands:
			lane.noticeRecovered(failKey)
			failed++
		default:
			lane.noticeRecovered(failKey)
		}
	}
	if err := store.SetMetadataBatch(info.ID, map[string]string{
		drainReminderFailedKey:  strconv.Itoa(failed),
		drainReminderSettledKey: strconv.Itoa(due.attempts),
	}); err != nil {
		fmt.Fprintf(stdout, "%s: settling %s's last reminder failed: %v\n", drainReminderLabel, info.ID, err) //nolint:errcheck // best-effort
		return 0, false
	}
	return failed, true
}

// drainReminderQuietHold runs the liveness and quiet gates that can still stop a
// due reminder before delivery: the stop finally took, a human is at the pane,
// or the activity signal is unreadable or too recent (an agent in the middle of
// a turn is answering its own drain; an unreadable signal is not evidence it is
// not). proceed is false when one of those holds, carrying the outcome to return.
// An attachment probe that cannot tell holds under its own reason.
func drainReminderQuietHold(sp runtime.Provider, store beads.Store, info sessions.Info, name string, now time.Time) (drainReminderOutcome, bool) {
	if !sp.IsRunning(name) {
		return drainReminderSkipped, false // the stop took after all; the finalizer owns it
	}
	if attached, err := attachmentHolds(sp, name); err != nil {
		noteDrainReminderHold(store, info, drainReminderHoldAttachUnknown)
		return drainReminderHeld, false
	} else if attached {
		noteDrainReminderHold(store, info, drainReminderHoldAttached)
		return drainReminderHeld, false
	}
	activity, err := sp.GetLastActivity(name)
	if err != nil || activity.IsZero() {
		noteDrainReminderHold(store, info, drainReminderHoldUnreadable)
		return drainReminderHeld, false
	}
	if now.Sub(activity) < drainReminderGrace {
		noteDrainReminderHold(store, info, drainReminderHoldBusy)
		return drainReminderHeld, false
	}
	return drainReminderDelivered, true
}

// deliverDrainReminder delivers one due reminder.
//
// When the controller's lane host executes message ops for store and a lane
// can type into the session, the reminder is a planner notice op
// (proposeDrainReminderNotice) that the lane delivers at the session's
// boundary: the lane's content gate admits the drain reminder's notice into
// a draining row (session.GateContentDrainNotice), and nothing else. A row
// the gate holds the notice from for a reason other than draining (an idle
// drain's latch, which the drain-ack stop-pending mark keeps, reads
// stop_pending) is nudged directly, as it is with the lane off: a notice
// there would only wait out its start_by.
//
// Otherwise it writes the pacing marker ahead of the nudge, exactly as the
// sibling backstops do: a crash between the two costs one attempt but can
// never replay an unbounded stream. A nudge error still spends the attempt
// (the write-ahead is what bounds the loop, and unwinding it would let a
// permanently input-dead pane re-send forever) and records that nothing
// ARRIVED, so the escalation reports what actually happened. The one
// exception is a deferral (runtime.ErrSessionBusy): the runtime typed nothing
// because the session was running a turn its submit keys would have
// interrupted, so nothing was attempted and the marker is restored to what it
// held, under the busy hold.
func deliverDrainReminder(sp runtime.Provider, store beads.Store, info sessions.Info, name string, due drainReminderDue, stdout io.Writer) drainReminderOutcome {
	failed := due.failed
	journalPrior := func() {}
	if lane := plannerNoticeLaneFor(store); lane != nil {
		block := drainReminderNoticeBlock(due)
		if block == "" {
			if outcome, handled := proposeDrainReminderNotice(lane, store, info, name, due, stdout); handled {
				return outcome
			}
		} else {
			fmt.Fprintf(stdout, "%s: the lane's content gate holds the notice for %s (%s); nudging directly\n", drainReminderLabel, name, block) //nolint:errcheck // best-effort
			// The attempt before this one may have been a lane notice; the
			// direct attempt settles it.
			priorUndelivered, priorErr := drainReminderPriorUndelivered(lane, info, due)
			if priorUndelivered {
				failed++
			}
			journalPrior = func() { journalDrainReminderPrior(stdout, name, due, priorUndelivered, priorErr) }
		}
	}
	if !writeDrainReminderMarker(store, info, due.row, due.drainID, due.attempts+1, failed, due.attempts+1, due.now, stdout) {
		return drainReminderSkipped
	}
	err := nudgeDrainReminder(sp, name, info)
	if errors.Is(err, runtime.ErrSessionBusy) {
		writeDrainReminderMarker(store, info, due.row, due.drainID, due.attempts, due.failed, due.settled, due.last, stdout)
		noteDrainReminderHold(store, info, drainReminderHoldBusy)
		return drainReminderHeld
	}
	journalPrior()
	if err != nil {
		// A nil return is not proof of delivery: the seam adapter answers nil when
		// it cannot attach, so some failures are invisible here and counted as
		// delivered. That is the best knowledge available at this boundary, and it
		// errs toward the gentler journal line.
		writeDrainReminderMarker(store, info, due.row, due.drainID, due.attempts+1, failed+1, due.attempts+1, due.now, stdout)
		fmt.Fprintf(stdout, //nolint:errcheck // best-effort
			"%s: reminder %d/%d to %s was undeliverable: %v\n",
			drainReminderLabel, due.attempts+1, drainReminderMaxAttempts, name, err)
		return drainReminderSkipped
	}
	clearDrainReminderHold(store, info)
	fmt.Fprintf(stdout, //nolint:errcheck // best-effort
		"%s: drain reminder %d/%d delivered to %s (stop-pending, unacked)\n",
		drainReminderLabel, due.attempts+1, drainReminderMaxAttempts, name)
	return drainReminderDelivered
}

// nudgeDrainReminder types the reminder into the session directly: with
// [ops] authoritative empty, without a lane host for the sessions store, or
// for a session no lane can type into.
func nudgeDrainReminder(sp runtime.Provider, name string, info sessions.Info) error {
	return sp.Nudge(name, runtime.TextContent(drainReminderContent(info)))
}

// drainReminderNoticeBlock is why the lane's content gate would hold the
// drain reminder's notice from due's row now, or "": the gate the lane
// executor asks (session.ContentGate for GateContentDrainNotice), over the
// row as loaded, whose runtime the quiet hold has just seen running.
func drainReminderNoticeBlock(due drainReminderDue) string {
	return sessions.ContentGate(sessionInfoFromBead(due.row), true, due.now, sessions.GateContentDrainNotice)
}

// drainReminderNotice is attempt of due's reminder as a planner notice: keyed
// by its drain (the row's drain_at; the incarnation is the key's token) and
// its attempt, about the session row itself, and standing until just before
// the next attempt is due.
func drainReminderNotice(info sessions.Info, due drainReminderDue, attempt int) plannerNotice {
	return plannerNotice{
		Purpose:     drainReminderNoticePurpose,
		Trigger:     strings.TrimSpace(due.row.Metadata["drain_at"]),
		Causation:   strings.TrimSpace(info.ID),
		Attempt:     attempt,
		Content:     drainReminderContent(info),
		StartWithin: plannerNoticeStartWithin(drainReminderInterval),
	}
}

// proposeDrainReminderNotice is the reminder's delivery through the
// conversation lane, the op proposed before its marker (the op is idempotent
// by key, so a crash between the two re-proposes the same op and then
// spends the attempt). handled is false when no lane can type into the
// session, for the caller to nudge directly; otherwise:
//   - while an earlier attempt's notice is in flight nothing is written and
//     the attempt is not spent (drainReminderHeld);
//   - a proposed attempt, or one whose op already stood, is spent
//     (drainReminderProposed);
//   - a notice the op store refuses for the request itself is spent at once,
//     and one that fails plannerNoticeMaxFailures times in a row is spent
//     then, each counted as undeliverable; a failure short of that leaves the
//     attempt for a later tick, logged at most once per laneErrorLogInterval.
//
// Spending an attempt settles the one before it (drainReminderPriorUndelivered):
// its notice, read once the new attempt's proposal has superseded it if it
// never started, counts as undeliverable unless the lane delivered it.
//
// Nothing falls back to a direct nudge once a lane serves the session and
// its gate admits the notice.
//
// The reminder's own holds (an acknowledgement, an attached human, a recent
// pane) are checked only when an attempt is due, at its proposal: nothing
// re-evaluates them while its notice waits for the session's boundary, so a
// notice can reach an agent that acknowledged, or a pane a human attached
// to, after it was proposed, within its start_by (one cadence). The lane
// still delivers only at a quiescent boundary. That is the cost the direct
// nudge does not pay, bounded at one informational reminder per attempt; a
// withdraw on those transitions would need the per-tick provider reads the
// cadence gate exists to avoid.
func proposeDrainReminderNotice(lane plannerNoticeLane, store beads.Store, info sessions.Info, name string, due drainReminderDue, stdout io.Writer) (drainReminderOutcome, bool) {
	n := drainReminderNotice(info, due, due.attempts+1)
	failKey := plannerNoticeFailureKey(info.ID, n)
	outcome, op, err := proposePlannerNotice(lane, due.row, n)
	if err != nil && !plannerNoticeRefused(err) {
		failures, logNow := lane.noticeFailed(failKey)
		if failures < plannerNoticeMaxFailures {
			if logNow {
				fmt.Fprintf(stdout, "%s: notice for %s failed (%d of %d before reminder %d/%d is spent): %v\n", //nolint:errcheck // best-effort
					drainReminderLabel, name, failures, plannerNoticeMaxFailures, n.Attempt, drainReminderMaxAttempts, err)
			}
			return drainReminderSkipped, true
		}
	}
	lane.noticeRecovered(failKey)
	// This attempt's fate is known now when its notice never landed, so it
	// settles with it; a landed notice is settled by the next spend.
	undelivered, settled := false, due.attempts
	var line string
	switch {
	case err != nil && plannerNoticeRefused(err):
		undelivered, settled = true, n.Attempt
		line = fmt.Sprintf("reminder %d/%d to %s was undeliverable: its notice was refused: %v", n.Attempt, drainReminderMaxAttempts, name, err)
	case err != nil:
		undelivered, settled = true, n.Attempt
		line = fmt.Sprintf("reminder %d/%d to %s was undeliverable: its notice failed %d times in a row: %v", n.Attempt, drainReminderMaxAttempts, name, plannerNoticeMaxFailures, err)
	case outcome == plannerNoticeDirect:
		return drainReminderSkipped, false
	case outcome == plannerNoticePending:
		return drainReminderHeld, true
	case outcome == plannerNoticeAlreadyProposed:
		line = fmt.Sprintf("drain reminder %d/%d for %s was already proposed (%s, %s); attempt spent", n.Attempt, drainReminderMaxAttempts, name, op.ID, op.Status)
	default:
		line = fmt.Sprintf("queued drain reminder %d/%d for %s (stop-pending, unacked)", n.Attempt, drainReminderMaxAttempts, name)
	}
	failed := due.failed
	if undelivered {
		failed++
	}
	priorUndelivered, priorErr := drainReminderPriorUndelivered(lane, info, due)
	if priorUndelivered {
		failed++
	}
	if !writeDrainReminderMarker(store, info, due.row, due.drainID, n.Attempt, failed, settled, due.now, stdout) {
		return drainReminderSkipped, true
	}
	fmt.Fprintf(stdout, "%s: %s\n", drainReminderLabel, line) //nolint:errcheck // best-effort
	journalDrainReminderPrior(stdout, name, due, priorUndelivered, priorErr)
	if undelivered {
		return drainReminderSkipped, true
	}
	clearDrainReminderHold(store, info)
	return drainReminderProposed, true
}

// drainReminderPriorUndelivered reports whether the attempt before due's next
// one, when it is a lane notice not yet settled, was never delivered: it
// closed without a delivery or stands open past its start_by. It reads
// nothing when that attempt is settled. A notice still standing (one the
// new attempt neither superseded nor waits on) is not counted, and a failed
// read counts nothing and is returned for the caller to journal.
func drainReminderPriorUndelivered(lane plannerNoticeLane, info sessions.Info, due drainReminderDue) (bool, error) {
	if due.attempts < 1 || due.settled >= due.attempts {
		return false, nil
	}
	fate, err := plannerNoticeAttemptFate(lane, due.row, drainReminderNotice(info, due, due.attempts), due.now)
	return fate == plannerNoticeFateUndelivered, err
}

// journalDrainReminderPrior journals what spending due's next attempt
// settled of the one before it: a notice the lane never delivered, or a read
// that failed, so that attempt was not counted.
func journalDrainReminderPrior(stdout io.Writer, name string, due drainReminderDue, undelivered bool, err error) {
	switch {
	case err != nil:
		fmt.Fprintf(stdout, "%s: reading reminder %d/%d's notice for %s failed; not counting it undeliverable: %v\n", //nolint:errcheck // best-effort
			drainReminderLabel, due.attempts, drainReminderMaxAttempts, name, err)
	case undelivered:
		fmt.Fprintf(stdout, "%s: reminder %d/%d to %s was undeliverable: the lane never delivered its notice\n", //nolint:errcheck // best-effort
			drainReminderLabel, due.attempts, drainReminderMaxAttempts, name)
	}
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
//
// "Requester" above is the ROW the ack lands on, and that is all the explicit
// argument promises: it resolves the target through the store, so the ack
// reaches the right session even from a pane whose own identity env is
// degraded. The incarnation binding riding under the same word — the stamp
// drainAckRequesterInstanceToken supplies — is a separate thing, taken from
// pane environment on BOTH ack forms, so it is absent on exactly that degraded
// pane. Such an ack reads agentAckBindingUnprovable and keeps being reminded,
// which is the direction this reader is meant to fail in.
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
func drainReminderState(bead beads.Bead, drainID string) (attempts, failed, settled int, last time.Time) {
	if strings.TrimSpace(bead.Metadata[drainReminderDrainKey]) != drainID {
		return 0, 0, 0, time.Time{}
	}
	attempts = atoiOr0(bead.Metadata[drainReminderCountKey])
	settled = attempts
	if v := strings.TrimSpace(bead.Metadata[drainReminderSettledKey]); v != "" {
		settled = min(atoiOr0(v), attempts)
	}
	return attempts, atoiOr0(bead.Metadata[drainReminderFailedKey]), settled,
		parseRFC3339OrZero(bead.Metadata[drainReminderAtKey])
}

// drainRemindersSpent reports whether the budget for THIS drain is exhausted and
// nothing more is worth waiting for. It is the durable question the terminal
// escalation asks of the markers this file writes.
//
// Every budget earns a full answer window after its last attempt, including one
// whose attempts were all UNDELIVERABLE. An earlier revision short-circuited the
// undeliverable case on the reasoning that waiting out an answer window for
// messages that never arrived is waiting for nothing. That reasoning is right
// about REMINDERS and wrong about what this authorizes: `failed` counts any
// sp.Nudge transport error — an ssh or k8s exec failure, a tmux send-keys
// failure — which is not evidence of an input-dead pane, and this function gates
// a kill. Short-circuiting treated an undelivered reminder MORE harshly than a
// refused one, and the quiet-hold guard that would otherwise protect a busy
// agent is documented non-functional on exactly the providers whose Nudge rides
// a fallible transport (see this file's header). So the window is unconditional;
// only the journal phrasing distinguishes the two cases.
func drainRemindersSpent(bead beads.Bead, now time.Time) bool {
	drainID := drainReminderIdentity(bead)
	if drainID == "" {
		return false
	}
	attempts, _, settled, last := drainReminderState(bead, drainID)
	if attempts < drainReminderMaxAttempts || last.IsZero() {
		return false
	}
	// The last attempt's lane notice holds the window open until it is
	// settled, for at most drainReminderSettleWindow.
	if settled < attempts {
		return now.Sub(last) >= drainReminderSettleWindow
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
	attempts, failed, _, _ := drainReminderState(bead, drainReminderIdentity(bead))
	return drainReminderSpendPhrase(attempts, failed)
}

// writeDrainReminderMarker writes the pacing marker over row, the session
// bead the state was loaded from. The settled count is written only when it
// says something an absent key does not (some attempt unsettled) or the row
// already carries one to overwrite, so a budget spent wholly on direct
// nudges leaves the marker it left before the lane existed.
func writeDrainReminderMarker(store beads.Store, info sessions.Info, row beads.Bead, drainID string, attempts, failed, settled int, now time.Time, stdout io.Writer) bool {
	patch := map[string]string{
		drainReminderCountKey:  strconv.Itoa(attempts),
		drainReminderFailedKey: strconv.Itoa(failed),
		drainReminderAtKey:     now.UTC().Format(time.RFC3339),
		drainReminderDrainKey:  drainID,
	}
	if _, carried := row.Metadata[drainReminderSettledKey]; carried || settled != attempts {
		patch[drainReminderSettledKey] = strconv.Itoa(settled)
	}
	err := store.SetMetadataBatch(info.ID, patch)
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
