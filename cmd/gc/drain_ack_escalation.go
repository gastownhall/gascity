package main

// The terminal exit for a drain-ack stop-pending seat whose agent left its TURN
// but not its RUNTIME.
//
// A drain-acked session is parked at `state=draining,
// state_reason=drain-ack-stop-pending` and its stop is queued asynchronously.
// When the agent acks correctly and then keeps sitting at its prompt, the
// runtime never dies, so `finalizeDrainAckStopPendingSessions` observes
// `obs.Running || obs.Alive` on every tick and re-queues the same stop forever.
// Seven live seats sat that way for two days, each holding a pool slot name, so
// buildDesiredState answered "pool session name unavailable" and the pool could
// not mint replacements.
//
// # Why re-issuing the ordinary stop cannot be the answer
//
// The pre-existing loop ALREADY kills every tick, and for a pane that answers
// the kill it already works: the runtime dies, the next tick observes it dead,
// and the existing terminus closes the bead and frees the name. So the rows that
// sit for days are, by construction, exactly the ones the ordinary kill does not
// terminate — and `worker.Handle.Kill` is not a stronger kill than `Stop`, it is
// `Stop` minus the bead write (both land on `runtime.Provider.Stop`). An
// escalation that re-sends the same signal is the same signal sent more times.
//
// The tmux Stop does walk the pane's process tree with SIGTERM then SIGKILL, but
// it discovers targets by topology (descendants of the pane, plus process-group
// members reparented to init) and it never re-checks liveness after its final
// KILL wave, so it returns success unconditionally. A process that has escaped
// that topology outlives it silently.
//
// So the escalation's added force is `runtime.ProcessTableScanner`: it finds
// live agent roots by their GC_SESSION_ID environment variable — independent of
// provider artifacts and immune to setsid/reparenting/PGID changes — and
// terminates by PID with process-group signaling, 5s/3s windows, start-time
// identity, and an error when death is NOT confirmed. That capability already
// exists in-tree; what did not exist is a route to it for a live, tracked,
// open-bead session. Both existing callers skip `IsTracked` runtimes by design,
// which is every session we are trying to rescue.
//
// # Bypassing the IsTracked guard safely
//
// That guard is load-bearing, so this route replaces it with a narrower fence:
// the bead must be in the drain-ack stop-pending wedge state, its drain bound
// must have elapsed, it must hold no assigned work, the runtime's
// GC_INSTANCE_TOKEN must still match the incarnation we parked, and the process
// must be positively attributed to THIS city (the /proc scan is supervisor-wide;
// terminating on anything less would SIGKILL a sibling city's healthy session).
//
// # What this pass does NOT do
//
// It does not close the bead. Closing is left to the finalizer's own
// `!obs.Running && !obs.Alive` arm on a later tick, which re-observes from
// scratch. Authorizing a close from inside the kill path was a real hazard:
// confirmDrainAckRuntimeDead reports true on a definite instance-token MISMATCH
// (meaning "someone else owns this name now", which is correct for "stop
// re-killing" and catastrophic for "close the bead"), and closing while a live
// pane holds the runtime name frees the bead without freeing the name, so the
// pool's next create fails the same way.
//
// # The identity set is the whole ballgame
//
// The assigned-work gate probes session.AssigneeIdentities, NOT the narrow
// {ID, session_name, configured_named_identity} set. Pool polecat aliases are
// first-class assignment identities: an agent that claimed work as "nux" holds
// it under an identifier the narrow set cannot see, so a narrow probe would
// report "no assigned work" for a busy agent and authorize killing it.
//
// The drain-ack CLOSE gate deliberately does NOT adopt this wide set. A
// transient pool SLOT alias ("gascity/gc.run-operator-1") is a rebinding chair
// rather than an owner, and no guard may honor one, or a rebind lets a fresh
// session shield or inherit a dead session's claim (#4981/#5241, pinned by
// TestAssignmentGuardsIgnoreTransientPoolSlotAliases). The two gates can differ
// because their errors are not symmetric: over-refusing a KILL leaves a row
// wedged exactly as it is today, while over-honoring ownership at the CLOSE
// keeps dead rows open forever. Erring wide belongs in front of the kill only.

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/telemetry"

	"github.com/gastownhall/gascity/internal/api"
)

// drainAckEscalationLabel prefixes this pass's journal lines so a terminal kill
// is greppable next to the reminder lines that preceded it.
const drainAckEscalationLabel = "drain-escalation"

// Pacing and bounds.
const (
	// drainAckEscalationGrace is how long an AGENT-ACKNOWLEDGED seat is left to
	// exit on its own before the escalation applies force. It is measured from
	// drain_at, which DrainAckStopPendingPatch rewrites at the moment the row
	// enters stop-pending, so it times the wedge and not the original drain.
	//
	// The agent-acked class needs its own bound because the reminder budget can
	// never be spent on it: reminders are refused outright once the pane reports
	// an agent-authored ack (drainReminderAckPin), and correctly so — there is
	// nothing to remind an agent of when it has already acknowledged. That
	// acknowledgement is also what makes this the LESS ambiguous class: the agent
	// said it was finished, so what remains is a runtime that will not exit.
	drainAckEscalationGrace = 30 * time.Minute
	// drainAckEscalationRetryInterval paces repeat attempts. Without it the
	// escalation is re-paid on every tick for every wedged row forever.
	drainAckEscalationRetryInterval = 15 * time.Minute
)

// Session-bead metadata keys recording escalation attempts. Persisted for the
// same reason the reminder markers are: a controller restart must RESUME the
// pacing, never replay it.
const (
	drainAckEscalationAtKey = "drain_escalation_at"
	// drainAckEscalationDrainKey scopes the attempt record to ONE drain of one
	// incarnation, mirroring drainReminderDrainKey. A canceled drain's marker
	// must not pace (or authorize) the next drain of the same seat.
	drainAckEscalationDrainKey = "drain_escalation_drain"
	drainAckEscalationCountKey = "drain_escalation_count"
)

// drainAckAssigneeIdentities is the WIDE identity set every drain-ack
// destructive decision probes: every identifier under which a work bead could be
// assigned to this session (session.AssigneeIdentities — bead ID, session_name,
// configured_named_identity, current alias, and every prior alias in
// alias_history), unioned with the configured-named-session fallback the other
// reconciler gates resolve from config.
//
// Deliberately wider than sessionAssignmentIdentifiersForConfigInfo, which stops
// at {ID, session_name, configured_named_identity} and therefore cannot see work
// claimed under a pool alias. Being a superset it can only ever find MORE work,
// so it can only refuse more kills and more closes — never authorize either.
func drainAckAssigneeIdentities(info sessionpkg.Info, cfg *config.City) []string {
	configured := sessionAssignmentIdentifiersForConfigInfo(info, cfg)
	wide := sessionpkg.AssigneeIdentities(info)
	all := make([]string, 0, len(configured)+len(wide))
	all = append(all, configured...)
	all = append(all, wide...)
	return compactSessionAssignmentIdentifiers(all)
}

// sessionHasOpenAssignedWorkForEscalation is the escalation's assigned-work
// gate. It reuses the drain-ack close-gate per-store query (which excludes the
// session's own mol-do-work "drain" step — a session that has already signaled
// completion is not still working) but feeds it the WIDE identity set.
//
// The close gate stays narrow for the transient-slot-alias reason documented on
// sessionHasOpenAssignedWorkForReachableStoreForCloseGate. This gate does not,
// because the two errors are not symmetric: over-refusing here leaves a row
// wedged exactly as it is today, while under-refusing ends a live agent's turn.
// Erring toward "this session still holds work" is the only safe direction in
// front of a kill.
func sessionHasOpenAssignedWorkForEscalation(
	cityPath string,
	cfg *config.City,
	store beads.Store,
	rigStores map[string]beads.Store,
	info sessionpkg.Info,
) (bool, error) {
	identifiers := drainAckAssigneeIdentities(info, cfg)
	return assignedWorkExistsForSession(cityPath, cfg, store, rigStores, info, func(s beads.Store) (bool, error) {
		return sessionHasOpenAssignedWorkInStoreByIdentifiersForCloseGate(s, identifiers)
	})
}

// drainAckEscalationDue reports whether a wedged row has exceeded its bound, and
// names which arm authorized it (for the journal and the event payload).
//
// Two arms, because the two populations are paced by different evidence:
//
//   - AGENT-acked: the pane carries an agent-authored acknowledgement, so the
//     reminder budget is structurally unspendable. Bounded by time since the
//     stop-pending transition.
//   - Everything else: bounded by the reminder budget and its answer window,
//     which is what drainRemindersSpent reports.
func drainAckEscalationDue(sp runtime.Provider, bead beads.Bead, name string, now time.Time) (string, bool) {
	if drainReminderIdentity(bead) == "" {
		return "", false // no drain to scope an escalation to
	}
	source, err := sp.GetMeta(name, reconcilerDrainAckSourceKey)
	if err != nil {
		// Fail closed: an unreadable pane is not evidence of anything, and this
		// gate stands in front of a kill.
		return "", false
	}
	if strings.TrimSpace(source) == drainAckSourceAgentValue {
		drainAt := parseRFC3339OrZero(bead.Metadata["drain_at"])
		if drainAt.IsZero() || now.Sub(drainAt) < drainAckEscalationGrace {
			return "", false
		}
		return "agent_acked_runtime_survived", true
	}
	if drainRemindersSpent(bead, now) {
		return "reminders_unanswered", true
	}
	return "", false
}

// drainAckEscalationPaced reports whether an attempt was already made recently
// for THIS drain. The record is written before the attempt, so a crash costs one
// interval rather than replaying an unbounded stream of kills.
func drainAckEscalationPaced(bead beads.Bead, now time.Time) bool {
	if strings.TrimSpace(bead.Metadata[drainAckEscalationDrainKey]) != drainReminderIdentity(bead) {
		return false // markers belong to a different drain
	}
	last := parseRFC3339OrZero(bead.Metadata[drainAckEscalationAtKey])
	if last.IsZero() {
		return false
	}
	return now.Sub(last) < drainAckEscalationRetryInterval
}

// recordDrainAckEscalationAttempt persists the attempt marker ahead of the kill
// and returns the new attempt count.
func recordDrainAckEscalationAttempt(store beads.Store, bead beads.Bead, now time.Time) int {
	drainID := drainReminderIdentity(bead)
	count := 0
	if strings.TrimSpace(bead.Metadata[drainAckEscalationDrainKey]) == drainID {
		count = atoiOr0(bead.Metadata[drainAckEscalationCountKey])
	}
	count++
	_ = store.SetMetadataBatch(bead.ID, map[string]string{
		drainAckEscalationAtKey:    now.UTC().Format(time.RFC3339),
		drainAckEscalationDrainKey: drainID,
		drainAckEscalationCountKey: strconv.Itoa(count),
	})
	return count
}

// escalateWedgedDrainAckStopPending decides whether a stop-pending row whose
// runtime is still alive has earned a terminal escalation, records it, and
// queues the forceful termination OFF the reconcile tick.
//
// It returns whether an escalation was queued. That is a reporting signal only:
// the caller must still take its ordinary remind-and-requeue path, because
// nothing here closes the bead. The close belongs to the finalizer's own
// fresh-observation arm on a later tick.
//
// Every gate fails CLOSED, and they are ordered cheapest-first: one store read
// and at most two provider GetMeta calls stand in front of the cross-store
// assigned-work fan-out, which is the only expensive probe and therefore last.
func escalateWedgedDrainAckStopPending(
	cityPath string,
	cfg *config.City,
	sp runtime.Provider,
	store beads.Store,
	rigStores map[string]beads.Store,
	info sessionpkg.Info,
	name string,
	processNames []string,
	tracker *asyncStartTracker,
	clk clock.Clock,
	rec events.Recorder,
	stderr io.Writer,
) bool {
	if sp == nil || store == nil || clk == nil {
		return false
	}
	if stderr == nil {
		stderr = io.Discard
	}
	name = strings.TrimSpace(name)
	if name == "" || strings.TrimSpace(info.ID) == "" {
		return false
	}
	// Scoped to pool seats: closing a non-pool row releases no pool name, so it
	// buys nothing and is not this pass's business.
	if !isPoolManagedSessionInfo(info) {
		return false
	}

	bead, err := store.Get(info.ID)
	if err != nil {
		return false
	}
	now := clk.Now()

	// Gate 1 — the bound, per population. Cheap: one already-read bead plus one
	// provider GetMeta.
	reason, due := drainAckEscalationDue(sp, bead, name, now)
	if !due {
		return false
	}

	// Gate 2 — pacing. The overwhelmingly common answer for a row that will never
	// die is "we already tried recently", and it must cost nothing further.
	if drainAckEscalationPaced(bead, now) {
		return false
	}

	// Gate 3 — the token fence (mirrors verifiedStop and the async stop path).
	// Cheap, and ahead of the work fan-out on purpose. Only a DEFINITE mismatch
	// refuses: an empty expected or live token means "cannot verify" and falls
	// through, matching the conservative posture of the sibling fences.
	if expected := strings.TrimSpace(info.InstanceToken); expected != "" {
		if actual, _ := sp.GetMeta(name, "GC_INSTANCE_TOKEN"); actual != "" && strings.TrimSpace(actual) != expected {
			fmt.Fprintf(stderr, "%s: %s skipped: instance token mismatch (session was replaced)\n", drainAckEscalationLabel, name) //nolint:errcheck
			return false
		}
	}

	// Gate 4 — never terminate an agent that still owns work (7g §3.5). The only
	// expensive probe, so it runs last, and it fails closed on an unreadable
	// store: a smaller answer presented as authoritative reads as "holds
	// nothing", which is exactly the error that authorizes a wrongful kill.
	hasAssignedWork, err := sessionHasOpenAssignedWorkForEscalation(cityPath, cfg, store, rigStores, info)
	if err != nil {
		fmt.Fprintf(stderr, "%s: checking assigned work for %s: %v; not escalating\n", drainAckEscalationLabel, name, err) //nolint:errcheck
		return false
	}
	if hasAssignedWork {
		fmt.Fprintf(stderr, "%s: %s still holds assigned work; not escalating\n", drainAckEscalationLabel, name) //nolint:errcheck
		return false
	}

	attempt := recordDrainAckEscalationAttempt(store, bead, now)
	recordDrainAckEscalation(cfg, info, name, reason, attempt, rec)
	fmt.Fprintf(stderr, //nolint:errcheck
		"%s: escalating %s (attempt %d, %s): forcing runtime termination\n",
		drainAckEscalationLabel, name, attempt, reason)

	queueDrainAckForcedTermination(cityPath, store, sp, cfg, info.ID, name, info.InstanceToken, processNames, tracker, stderr)
	return true
}

// queueDrainAckForcedTermination runs the forceful termination on a DETACHED
// goroutine. It must never run on the reconcile tick: the ordinary stop's
// confirm-dead loop alone is bounded at drainAckStopConfirmDeadTimeout, and the
// process-table walk adds its own 5s/3s windows, so N wedged rows would stall
// every controller tick by N times that — starving the pool respawn, order
// dispatch and health patrol that the wedge is already starving. The pre-existing
// handler for these same rows is detached for exactly this reason.
func queueDrainAckForcedTermination(
	cityPath string,
	store beads.Store,
	sp runtime.Provider,
	cfg *config.City,
	sessionID, name, expectedToken string,
	processNames []string,
	tracker *asyncStartTracker,
	stderr io.Writer,
) {
	// Distinct key space from queueDrainAckAsyncStop so an in-flight ordinary
	// stop does not dedup the escalation away (and vice versa).
	done, tracking := tracker.startDrainAckStop("escalate:" + drainAckAsyncStopKey(sessionID, name))
	if !tracking {
		return
	}
	confirmTimeout, confirmPoll := drainAckStopConfirmDeadTimeout, drainAckStopConfirmDeadPoll
	go func() {
		defer done()
		// Try the ordinary provider stop once more first: it is the cheap path and
		// it is what a merely-slow pane needs.
		if err := workerKillSessionTargetWithConfig(cityPath, store, sp, cfg, name); err != nil && !runtime.IsSessionGone(err) {
			fmt.Fprintf(stderr, "%s: stopping %s: %v\n", drainAckEscalationLabel, name, err) //nolint:errcheck
		}
		if confirmDrainAckRuntimeDead(cityPath, store, sp, cfg, name, expectedToken, processNames, stderr, confirmTimeout, confirmPoll) {
			return
		}
		// The pane outlived the ordinary stop. This is the population the whole
		// pass exists for, so apply the force the ordinary path does not have.
		terminateDrainAckRuntimeByProcessTable(cityPath, sp, sessionID, name, expectedToken, stderr)
	}()
}

// terminateDrainAckRuntimeByProcessTable is the escalation's actual added force:
// find this session's live agent roots by their GC_SESSION_ID environment
// variable and terminate them by PID. Unlike the provider stop, discovery does
// not depend on tmux topology, so a process that has reparented or left its
// process group is still found, and termination reports an error when death is
// not confirmed instead of returning success unconditionally.
//
// The existing ProcessTableScanner callers refuse to touch IsTracked runtimes,
// which is every live session; this route replaces that guard with the caller's
// gates plus two fences applied here: the runtime must still carry the instance
// token we parked, and the process must be positively attributed to THIS city.
// The /proc scan is supervisor-wide, so without the city fence this would
// SIGKILL a sibling city's healthy session.
func terminateDrainAckRuntimeByProcessTable(
	cityPath string,
	sp runtime.Provider,
	sessionID, name, expectedToken string,
	stderr io.Writer,
) {
	scanner, ok := sp.(runtime.ProcessTableScanner)
	if !ok {
		fmt.Fprintf(stderr, "%s: %s survived its stop and the provider cannot scan the process table; slot stays occupied\n", //nolint:errcheck
			drainAckEscalationLabel, name)
		return
	}
	// Re-check the fence immediately before the destructive act: the ordinary
	// stop and its confirm loop have just spent seconds, and a replacement may
	// have taken the name in the meantime.
	if expected := strings.TrimSpace(expectedToken); expected != "" {
		if actual, _ := sp.GetMeta(name, "GC_INSTANCE_TOKEN"); actual != "" && strings.TrimSpace(actual) != expected {
			fmt.Fprintf(stderr, "%s: %s force-terminate skipped: instance token mismatch (session was replaced)\n", drainAckEscalationLabel, name) //nolint:errcheck
			return
		}
	}
	found, err := scanner.FindRuntimesBySessionID(sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "%s: scanning process table for %s: %v\n", drainAckEscalationLabel, name, err) //nolint:errcheck
	}
	normalizedCity := normalizePathForCompare(strings.TrimSpace(cityPath))
	for _, live := range found {
		if strings.TrimSpace(live.SessionID) != strings.TrimSpace(sessionID) {
			continue
		}
		// City attribution. When our own city path is unknown we cannot attribute
		// safely, so we refuse rather than guess.
		if normalizedCity == "" || normalizePathForCompare(strings.TrimSpace(live.City)) != normalizedCity {
			continue
		}
		if err := scanner.TerminateRuntime(live); err != nil {
			fmt.Fprintf(stderr, "%s: force-terminating pid=%d session=%s: %v\n", drainAckEscalationLabel, live.PID, sessionID, err) //nolint:errcheck
			continue
		}
		fmt.Fprintf(stderr, "%s: force-terminated pid=%d session=%s (survived the provider stop)\n", drainAckEscalationLabel, live.PID, sessionID) //nolint:errcheck
	}
}

// recordDrainAckEscalation emits the counted typed event for a terminal
// escalation. This pass applies force to a session nobody asked it to kill, so
// it must never be silent: a rising rate of these is the signal that agents are
// not exiting on drain-ack, and a silent backstop would mask exactly the
// drain-ack tail it exists to survive.
func recordDrainAckEscalation(cfg *config.City, info sessionpkg.Info, name, reason string, attempt int, rec events.Recorder) {
	template := normalizedSessionTemplateInfo(info, cfg)
	if template == "" {
		template = info.Template
	}
	telemetry.RecordDrainTransition(context.Background(), name, reason, "escalate")
	if rec == nil {
		return
	}
	rec.Record(events.Event{
		Type:      events.SessionDrainStopEscalated,
		Actor:     "gc",
		Subject:   template,
		Message:   fmt.Sprintf("drain-ack stop-pending runtime did not exit (%s); forcing termination, attempt %d", reason, attempt),
		SessionID: info.ID,
		Payload:   api.SessionLifecyclePayloadJSON(info.ID, template, reason),
	})
}
