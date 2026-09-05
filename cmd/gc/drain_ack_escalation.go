package main

// The terminal exit for a drain-ack stop-pending seat whose agent left its TURN
// but not its RUNTIME.
//
// A drain-acked session is parked at `state=draining,
// state_reason=drain-ack-stop-pending` and its stop is queued asynchronously.
// When the agent acks correctly and then simply keeps sitting at its prompt, the
// runtime never dies, so `finalizeDrainAckStopPendingSessions` observes
// `obs.Running || obs.Alive` on every tick and re-queues the same stop forever.
// The row's own comment concedes the shape of it: this is the one drain state
// with no exit of its own, and the only thing that has ever cleared such a row is
// an operator killing the pane. Seven live seats sat that way for two days.
//
// The cost is not cosmetic. An OPEN session bead owns its `session_name`
// whatever its state, so each wedged row holds a pool slot name;
// `buildDesiredState` answers "pool session name unavailable" and the pool
// cannot mint a replacement. Suspending or archiving the row would not help —
// `suspended`, `asleep` and `archived` are all still OPEN. Only `status=closed`
// frees the name, and `CmdClose` is already legal from `StateDraining`. So the
// exit has to end in a close.
//
// Closing over a live agent is the one thing this pass must never do, so the
// escalation is gated rather than timed:
//
//  1. the drain reminder budget is spent AND its answer window has elapsed
//     (drainRemindersSpent — the durable question drain_reminder.go's markers
//     were built to answer);
//  2. the session holds no assigned work, probed across the WIDE identity set;
//  3. the instance-token fence agrees the runtime is still the one we meant to
//     stop, not a re-woken same-name replacement;
//  4. only then: kill, re-observe until confirmed dead, and let the caller's
//     existing finalize terminus close the bead.
//
// # The identity set is the whole ballgame
//
// Gate 2 uses session.AssigneeIdentities, NOT the narrow
// {ID, session_name, configured_named_identity} set the other close gates use.
// Pool polecat aliases are first-class assignment identities: an agent that
// claimed work as "nux" holds that work under an identifier the narrow set
// cannot see. A probe blind to `alias`/`alias_history` would report "no assigned
// work" for a busy agent and authorize killing it — the precise class that bit
// the 7g review. The wide set is a strict superset, so it can only ever refuse
// more escalations than the narrow one, never more kills.

import (
	"context"
	"fmt"
	"io"
	"strings"

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

// drainAckEscalationAssigneeIdentities is the WIDE identity set the escalation's
// assigned-work gate probes: every identifier under which a work bead could be
// assigned to this session (session.AssigneeIdentities — bead ID, session_name,
// configured_named_identity, current alias, and every prior alias in
// alias_history), unioned with the configured-named-session fallback the other
// reconciler gates resolve from config.
//
// This is deliberately wider than sessionAssignmentIdentifiersForConfigInfo,
// which stops at {ID, session_name, configured_named_identity}. That narrow set
// cannot see work claimed under a pool alias, and this gate stands in front of a
// kill: a false "no work" here ends a live agent's turn. Being a superset, it can
// only refuse escalations the narrow set would have allowed.
func drainAckEscalationAssigneeIdentities(info sessionpkg.Info, cfg *config.City) []string {
	configured := sessionAssignmentIdentifiersForConfigInfo(info, cfg)
	wide := sessionpkg.AssigneeIdentities(info)
	all := make([]string, 0, len(configured)+len(wide))
	all = append(all, configured...)
	all = append(all, wide...)
	return compactSessionAssignmentIdentifiers(all)
}

// sessionHasOpenAssignedWorkForEscalation is the escalation's assigned-work
// probe. It reuses the drain-ack close-gate per-store query (which excludes the
// session's own mol-do-work "drain" step — a session that has already signaled
// completion is not still working) but feeds it the wide identity set above.
func sessionHasOpenAssignedWorkForEscalation(
	cityPath string,
	cfg *config.City,
	store beads.Store,
	rigStores map[string]beads.Store,
	info sessionpkg.Info,
) (bool, error) {
	identifiers := drainAckEscalationAssigneeIdentities(info, cfg)
	return assignedWorkExistsForSession(cityPath, cfg, store, rigStores, info, func(s beads.Store) (bool, error) {
		return sessionHasOpenAssignedWorkInStoreByIdentifiersForCloseGate(s, identifiers)
	})
}

// escalateWedgedDrainAckStopPending decides whether a stop-pending row whose
// runtime is still alive has earned a terminal kill, and performs it. It returns
// true only when the runtime is CONFIRMED DEAD, which is the caller's signal to
// fall through to its ordinary finalize terminus and close the bead.
//
// Every gate fails CLOSED: anything unreadable, unresolvable, or ambiguous
// returns false and leaves the caller's existing remind-and-requeue behavior
// exactly as it was. Returning false is always safe — it is what happens today.
func escalateWedgedDrainAckStopPending(
	cityPath string,
	cfg *config.City,
	sp runtime.Provider,
	store beads.Store,
	rigStores map[string]beads.Store,
	info sessionpkg.Info,
	name string,
	processNames []string,
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
	// Scoped to pool seats, matching the reminder budget that authorizes this
	// (drainReminderEligible reminds pool rows only). Closing a non-pool row
	// releases no pool name, so it buys nothing and is not this pass's business.
	if !isPoolManagedSessionInfo(info) {
		return false
	}

	// Gate 1 — the budget and its bound. One store read, and the overwhelmingly
	// common answer ("not spent") returns before any provider round-trip or work
	// query. drainRemindersSpent is scoped to THIS drain of THIS incarnation, so a
	// canceled drain's spent markers cannot authorize an escalation for a drain
	// nobody was ever asked about.
	bead, err := store.Get(info.ID)
	if err != nil {
		return false
	}
	if !drainRemindersSpent(bead, clk.Now()) {
		return false
	}

	// Gate 2 — never terminate an agent that still owns work (7g §3.5). Probed
	// across the wide identity set, and fail-closed on an unreadable store: a
	// smaller answer presented as authoritative reads as "holds nothing", which
	// is exactly the error that authorizes a wrongful kill.
	hasAssignedWork, err := sessionHasOpenAssignedWorkForEscalation(cityPath, cfg, store, rigStores, info)
	if err != nil {
		fmt.Fprintf(stderr, "%s: checking assigned work for %s: %v; not escalating\n", drainAckEscalationLabel, name, err) //nolint:errcheck
		return false
	}
	if hasAssignedWork {
		fmt.Fprintf(stderr, "%s: %s still holds assigned work; not escalating\n", drainAckEscalationLabel, name) //nolint:errcheck
		return false
	}

	// Gate 3 — the token fence (mirrors verifiedStop and the async stop path).
	// This kill targets the session by NAME. If the name was reused by a re-woken
	// replacement, its GC_INSTANCE_TOKEN differs from the one we parked; killing
	// it would take out a live, working session. Only a DEFINITE mismatch refuses:
	// an empty expected or live token means "cannot verify" and falls through,
	// matching the conservative posture of the sibling fences.
	if expected := strings.TrimSpace(info.InstanceToken); expected != "" {
		if actual, _ := sp.GetMeta(name, "GC_INSTANCE_TOKEN"); actual != "" && strings.TrimSpace(actual) != expected {
			fmt.Fprintf(stderr, "%s: %s skipped: instance token mismatch (session was replaced)\n", drainAckEscalationLabel, name) //nolint:errcheck
			return false
		}
	}

	// Gate 4 — kill, then prove it. The kill is best-effort and does not verify
	// the agent exited; a survivor that kept the slot would be the same wedge in a
	// new costume, so the close is authorized by the confirm-dead loop, never by
	// the kill returning nil.
	if err := workerKillSessionTargetWithConfig(cityPath, store, sp, cfg, name); err != nil && !runtime.IsSessionGone(err) {
		fmt.Fprintf(stderr, "%s: killing %s: %v\n", drainAckEscalationLabel, name, err) //nolint:errcheck
		return false
	}
	if !confirmDrainAckRuntimeDead(cityPath, store, sp, cfg, name, info.InstanceToken, processNames, stderr, drainAckStopConfirmDeadTimeout, drainAckStopConfirmDeadPoll) {
		fmt.Fprintf(stderr, "%s: %s outlived its kill; leaving the row open rather than freeing the slot under a live agent\n", drainAckEscalationLabel, name) //nolint:errcheck
		return false
	}

	recordDrainAckEscalation(cfg, info, bead, name, rec)
	fmt.Fprintf(stderr, //nolint:errcheck
		"%s: killed %s after %s; closing the row to release its pool slot\n",
		drainAckEscalationLabel, name, drainReminderSpendPhraseFor(bead))
	return true
}

// recordDrainAckEscalation emits the counted typed event for a terminal
// escalation. This pass kills a session the operator did not ask it to kill, so
// it must never be silent: a rising rate of these is the signal that agents are
// not exiting on drain-ack, and a silent backstop would mask exactly that.
func recordDrainAckEscalation(cfg *config.City, info sessionpkg.Info, bead beads.Bead, name string, rec events.Recorder) {
	template := normalizedSessionTemplateInfo(info, cfg)
	if template == "" {
		template = info.Template
	}
	telemetry.RecordDrainTransition(context.Background(), name, sessionpkg.DrainAckStopPendingReason, "escalate")
	if rec == nil {
		return
	}
	rec.Record(events.Event{
		Type:      events.SessionDrainStopEscalated,
		Actor:     "gc",
		Subject:   template,
		Message:   "drain-ack stop-pending runtime killed after " + drainReminderSpendPhraseFor(bead),
		SessionID: info.ID,
		Payload:   api.SessionLifecyclePayloadJSON(info.ID, template, "drain_stop_escalated"),
	})
}
