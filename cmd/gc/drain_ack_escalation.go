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
// That guard is load-bearing, and it was doing TWO jobs. Replacing it needs both.
//
// Session-level authorization: the bead must be in the drain-ack stop-pending
// wedge state, its drain bound must have elapsed, it must hold no assigned work,
// nobody may be attached and the pane must have been quiet, the runtime's
// GC_INSTANCE_TOKEN must still match the incarnation we parked, and the attempt
// must be durably paced.
//
// PER-PROCESS attribution, which is the half that is easy to miss: none of the
// above says WHICH process to signal. GC_SESSION_ID and GC_CITY_PATH are
// ordinary environment variables inherited by every child the agent ever
// spawned, and the scan promotes any of them to an "agent root" once it
// reparents to init — so matching on those two fields proves lineage, not
// ownership, and sweeps in every daemon an agent ever backgrounded. See
// drainAckForceTerminationTargets, which is where that fence lives.
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
	"os"
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
	// drainAckEscalationQuietGrace is how quiet the pane must have been before
	// force is applied. Deliberately longer than drainReminderGrace: that one
	// gates an informational nudge, this one gates SIGKILL of a process group.
	drainAckEscalationQuietGrace = 10 * time.Minute
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
// and returns the new attempt count and whether the write LANDED.
//
// The caller must refuse to escalate when it did not. The pacing that makes this
// kill path defensible lives entirely in that metadata: drainAckEscalationPaced
// reads it back out of the bead, so a discarded write turns the 15-minute pace
// into a fresh SIGTERM/SIGKILL wave plus a counted event on every tick, forever,
// each one reporting "attempt 1" because the count is re-derived from metadata
// that never landed. A store that goes read-only while still serving reads
// (sqlite query_only) is a documented failure mode on this fleet and is not
// otherwise fatal to the tick, so this is reachable without anything else
// breaking. No durable pacing, no escalation.
func recordDrainAckEscalationAttempt(store beads.Store, bead beads.Bead, now time.Time) (int, bool) {
	drainID := drainReminderIdentity(bead)
	count := 0
	if strings.TrimSpace(bead.Metadata[drainAckEscalationDrainKey]) == drainID {
		count = atoiOr0(bead.Metadata[drainAckEscalationCountKey])
	}
	count++
	if err := store.SetMetadataBatch(bead.ID, map[string]string{
		drainAckEscalationAtKey:    now.UTC().Format(time.RFC3339),
		drainAckEscalationDrainKey: drainID,
		drainAckEscalationCountKey: strconv.Itoa(count),
	}); err != nil {
		return 0, false
	}
	return count, true
}

// drainAckEscalationQuietHold refuses a kill under the same conditions the
// reminder pass refuses to send a MESSAGE: someone is attached, the pane has
// been active recently, or the activity signal cannot be read at all.
//
// Without this the pass will SIGKILL a pane it would not even nudge. Two shapes
// make that concrete. An operator attaches to a wedged seat to investigate why
// it will not exit — the documented response to this incident — and is killed out
// from under the cursor. And "no assigned work" is not "not working": a worker
// that follows the session-completion protocol acks the drain AFTER closing its
// last bead, then runs quality gates and pushes, so it holds zero beads while
// genuinely busy; if that wrap-up outruns the grace, the kill lands mid-push and
// leaves a lock file and a half-written handoff with no bead evidence anything
// was in flight.
//
// Unreadable activity HOLDS, matching drainReminderQuietHold's #312 rule that
// "we cannot tell" is never "idle" — the more so here, where the action is
// destructive rather than informational.
func drainAckEscalationQuietHold(sp runtime.Provider, name string, now time.Time) (string, bool) {
	if sp.IsAttached(name) {
		return "attached", true
	}
	activity, err := sp.GetLastActivity(name)
	if err != nil || activity.IsZero() {
		return "activity_unreadable", true
	}
	if now.Sub(activity) < drainAckEscalationQuietGrace {
		return "recently_active", true
	}
	return "", false
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

	// Gate 5 — the quiet hold. Cheap provider reads, but they belong after the
	// correctness gates so an ineligible row never pays them.
	if hold, held := drainAckEscalationQuietHold(sp, name, now); held {
		fmt.Fprintf(stderr, "%s: %s held (%s); not escalating\n", drainAckEscalationLabel, name, hold) //nolint:errcheck
		return false
	}

	// Pacing is written BEFORE any force is applied, and a write that does not
	// land refuses the escalation outright.
	attempt, paced := recordDrainAckEscalationAttempt(store, bead, now)
	if !paced {
		fmt.Fprintf(stderr, "%s: %s: could not persist the escalation attempt; refusing to escalate unpaced\n", drainAckEscalationLabel, name) //nolint:errcheck
		return false
	}
	fmt.Fprintf(stderr, //nolint:errcheck
		"%s: escalating %s (attempt %d, %s)\n",
		drainAckEscalationLabel, name, attempt, reason)

	// The event is emitted from the goroutine, at the point an outcome is known
	// — see recordDrainAckEscalation. If the termination could not even be
	// queued, report false so the caller still issues its ordinary stop rather
	// than losing this row's stop entirely.
	return queueDrainAckForcedTermination(cityPath, store, sp, cfg, info, name, reason, attempt, processNames, tracker, rec, stderr)
}

// queueDrainAckForcedTermination runs the forceful termination on a DETACHED
// goroutine. It must never run on the reconcile tick: the ordinary stop's
// confirm-dead loop alone is bounded at drainAckStopConfirmDeadTimeout, and the
// process-table walk adds its own 5s/3s windows, so N wedged rows would stall
// every controller tick by N times that — starving the pool respawn, order
// dispatch and health patrol that the wedge is already starving. The pre-existing
// handler for these same rows is detached for exactly this reason.
// It reports whether the termination was actually queued. A false return means
// nothing was started — the caller must fall back to its ordinary stop rather
// than leaving the row with no stop at all on this tick.
func queueDrainAckForcedTermination(
	cityPath string,
	store beads.Store,
	sp runtime.Provider,
	cfg *config.City,
	info sessionpkg.Info,
	name, reason string,
	attempt int,
	processNames []string,
	tracker *asyncStartTracker,
	rec events.Recorder,
	stderr io.Writer,
) bool {
	sessionID := strings.TrimSpace(info.ID)
	expectedToken := info.InstanceToken
	// Distinct key space from queueDrainAckAsyncStop so an in-flight ordinary
	// stop does not dedup the escalation away (and vice versa).
	done, tracking := tracker.startDrainAckStop("escalate:" + drainAckAsyncStopKey(sessionID, name))
	if !tracking {
		fmt.Fprintf(stderr, "%s: %s: a termination is already in flight; leaving this tick to the ordinary stop\n", drainAckEscalationLabel, name) //nolint:errcheck
		return false
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
			recordDrainAckEscalation(cfg, info, name, reason, "stopped_without_force", attempt, rec)
			return
		}
		// The pane outlived the ordinary stop. This is the population the whole
		// pass exists for, so apply the force the ordinary path does not have.
		outcome := terminateDrainAckRuntimeByProcessTable(cityPath, sp, sessionID, name, expectedToken, stderr)
		recordDrainAckEscalation(cfg, info, name, reason, outcome, attempt, rec)
	}()
	return true
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
) string {
	scanner, ok := sp.(runtime.ProcessTableScanner)
	if !ok {
		fmt.Fprintf(stderr, "%s: %s survived its stop and the provider cannot scan the process table; slot stays occupied\n", //nolint:errcheck
			drainAckEscalationLabel, name)
		return "no_process_table"
	}
	// Re-check the fence immediately before the destructive act: the ordinary
	// stop and its confirm loop have just spent seconds, and a replacement may
	// have taken the name in the meantime.
	if expected := strings.TrimSpace(expectedToken); expected != "" {
		if actual, _ := sp.GetMeta(name, "GC_INSTANCE_TOKEN"); actual != "" && strings.TrimSpace(actual) != expected {
			fmt.Fprintf(stderr, "%s: %s force-terminate skipped: instance token mismatch (session was replaced)\n", drainAckEscalationLabel, name) //nolint:errcheck
			return "token_mismatch"
		}
	}
	found, err := scanner.FindRuntimesBySessionID(sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "%s: scanning process table for %s: %v\n", drainAckEscalationLabel, name, err) //nolint:errcheck
	}
	targets, skipped := drainAckForceTerminationTargets(found, sessionID, cityPath, name, stderr)
	if len(targets) == 0 {
		fmt.Fprintf(stderr, //nolint:errcheck
			"%s: %s survived its stop but no process was attributable to this seat (%d scan hit(s) refused); slot stays occupied\n",
			drainAckEscalationLabel, name, skipped)
		return "nothing_attributable"
	}
	killed := 0
	for _, live := range targets {
		if err := scanner.TerminateRuntime(live); err != nil {
			fmt.Fprintf(stderr, "%s: force-terminating pid=%d session=%s: %v\n", drainAckEscalationLabel, live.PID, sessionID, err) //nolint:errcheck
			continue
		}
		killed++
		fmt.Fprintf(stderr, "%s: force-terminated pid=%d session=%s (survived the provider stop)\n", drainAckEscalationLabel, live.PID, sessionID) //nolint:errcheck
	}
	if killed == 0 {
		return "termination_failed"
	}
	return "force_terminated"
}

// drainAckForceTerminationTargets selects, from the scan hits for a session, the
// processes that are actually THAT SEAT'S RUNTIME — and reports how many hits it
// refused.
//
// This is the load-bearing fence, and it exists because matching on
// GC_SESSION_ID + GC_CITY_PATH proves ENV INHERITANCE, not runtime ownership.
// Both are ordinary environment variables inherited by every child of the
// agent's shell, and proctable promotes any such process to an "agent root" as
// soon as its parent is gone (ppid <= 1), excluding only tmux. So the naive
// match sweeps in every long-lived process an agent ever backgrounded:
//
//   - the managed-Dolt SCOPE WATCHDOG, which is re-exec'd with Setpgid and the
//     agent's environment verbatim; its SIGTERM handler kills the city's SHARED
//     dolt sql-server, i.e. the beads store for the controller and every other
//     agent in that city;
//   - a detached supervisor from `gc start` / `gc supervisor start`, likewise
//     Setpgid and env-inheriting, whose process group spans every city on the box.
//
// And TerminateRuntime signals the process GROUP (kill(-pid, ...)), so hitting
// one of those does not cost one process, it costs the group.
//
// The removed IsTracked guard was doing this job by accident: the tmux scanner
// keys tracked-ness by SESSION ID, so it marks every root carrying a live
// session's ID as tracked, and the pre-existing callers skip all of them. The
// five session-level fences re-authorize the SESSION; only this one re-supplies
// the PER-PROCESS attribution.
//
// The discriminator is reparenting. A pane the provider still owns has the
// provider's server as its parent, so PPID > 1; a backgrounded daemon leads its
// own group and reparents to init. Process-NAME matching is deliberately not
// used as the test: the agent process is frequently a DESCENDANT of the runtime
// root rather than the root itself (a pane's foreground can be a wrapper), so
// requiring the root's name to match the agent's process hints would refuse the
// legitimate target.
func drainAckForceTerminationTargets(
	found []runtime.LiveRuntime,
	sessionID, cityPath, name string,
	stderr io.Writer,
) ([]runtime.LiveRuntime, int) {
	normalizedCity := normalizePathForCompare(strings.TrimSpace(cityPath))
	self := os.Getpid()
	var targets []runtime.LiveRuntime
	skipped := 0
	for _, live := range found {
		switch {
		case strings.TrimSpace(live.SessionID) != strings.TrimSpace(sessionID):
			skipped++
			fmt.Fprintf(stderr, "%s: %s skipping pid=%d: session id %q is not this seat\n", //nolint:errcheck
				drainAckEscalationLabel, name, live.PID, strings.TrimSpace(live.SessionID))
		case normalizedCity == "" || normalizePathForCompare(strings.TrimSpace(live.City)) != normalizedCity:
			// The /proc scan is supervisor-wide. When our own city path is unknown
			// we cannot attribute safely, so we refuse rather than guess.
			skipped++
			fmt.Fprintf(stderr, "%s: %s skipping pid=%d: city %q is not this city\n", //nolint:errcheck
				drainAckEscalationLabel, name, live.PID, strings.TrimSpace(live.City))
		case live.PID <= 0 || live.PID == self:
			// proctable merges the caller's own environment into its scan record
			// for pid == os.Getpid(), so a controller that was itself daemonized
			// from an agent pane is otherwise a legal target of its own escalation.
			skipped++
			fmt.Fprintf(stderr, "%s: %s skipping pid=%d: that is this process\n", //nolint:errcheck
				drainAckEscalationLabel, name, live.PID)
		case !live.IsTracked || strings.TrimSpace(live.ProviderName) != strings.TrimSpace(name):
			// Positive binding to the runtime this pool name actually names. Not
			// sufficient on its own — the tmux scanner keys tracked-ness by SESSION
			// ID, so it stamps this same ProviderName onto every root carrying the
			// id, the watchdog included — which is exactly why the reparenting test
			// below exists. It is still required: a hit the provider does not bind
			// at all is not a runtime we own.
			skipped++
			fmt.Fprintf(stderr, "%s: %s skipping pid=%d: not bound to this runtime name (provider=%q tracked=%v)\n", //nolint:errcheck
				drainAckEscalationLabel, name, live.PID, strings.TrimSpace(live.ProviderName), live.IsTracked)
		case live.PPID <= 1:
			skipped++
			fmt.Fprintf(stderr, //nolint:errcheck
				"%s: %s skipping pid=%d (%s): reparented to init, so it only INHERITED this session's environment "+
					"— it is not the seat's runtime, and killing it would signal its whole process group\n",
				drainAckEscalationLabel, name, live.PID, live.Name)
		default:
			targets = append(targets, live)
		}
	}
	return targets, skipped
}

// recordDrainAckEscalation emits the counted typed event for a terminal
// escalation. This pass applies force to a session nobody asked it to kill, so
// it must never be silent: a rising rate of these is the signal that agents are
// not exiting on drain-ack, and a silent backstop would mask exactly the
// drain-ack tail it exists to survive.
// It is recorded from the termination goroutine, at the point an OUTCOME is
// known, never at decision time. A counter that increments on decisions cannot
// answer the question this event exists for: an escalation that found nothing
// attributable to kill is indistinguishable from one that freed the slot, and
// "nothing was reachable to kill" is precisely the state the fleet needs to see.
// outcome therefore rides in both the payload reason and the message.
func recordDrainAckEscalation(cfg *config.City, info sessionpkg.Info, name, reason, outcome string, attempt int, rec events.Recorder) {
	template := normalizedSessionTemplateInfo(info, cfg)
	if template == "" {
		template = info.Template
	}
	telemetry.RecordDrainTransition(context.Background(), name, reason+"/"+outcome, "escalate")
	if rec == nil {
		return
	}
	rec.Record(events.Event{
		Type:      events.SessionDrainStopEscalated,
		Actor:     "gc",
		Subject:   template,
		Message:   fmt.Sprintf("drain-ack stop-pending escalation (%s), attempt %d: %s", reason, attempt, outcome),
		SessionID: info.ID,
		Payload:   api.SessionLifecyclePayloadJSON(info.ID, template, reason+"/"+outcome),
	})
}
