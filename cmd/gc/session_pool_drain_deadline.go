package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/telemetry"
)

// poolSlotDrainRetireDeadline bounds how long a pool-managed session bead may
// sit in an unfinalized drain before the reconciler retires it outright.
//
// Why a bound is needed at all: a session bead that enters drain and never
// finalizes stays status=open forever, and an open session bead owns its
// session_name. A pool slot's runtime name is a pure function of its identity
// by design (ga-vcjr9 — minting a second box beside a live one leaks a runtime
// nothing will ever address again), so the pool cannot route around the held
// name. It drops to ZERO seats and every bead routed to that template becomes
// unclaimable. Production ran that way for 3d10h (ga-rxhu2).
//
// Ordering, load-bearing: this deadline must stay well ABOVE the drain-ack
// deadline cycle and strandedRepairConfirmGrace (session_beads.go) so the
// ordinary drain path always finalizes first and this bound only ever sees
// seats that machinery has already given up on. A healthy drain completes in
// seconds to minutes; the measured pathology is hours to days. Do not lower
// this below the drain-ack cycle without re-reading both.
const poolSlotDrainRetireDeadline = 30 * time.Minute

// drainFinalizeMetadataKey records HOW a drain reached its terminal close. It
// is absent on the ordinary path (the drain finalized on its own) and set to
// drainFinalizeDeadline when the bound below forced the retirement, so forced
// retirements are greppable in the store as well as countable on the event bus:
//
//	bd list --closed --json | jq 'select(.metadata.drain_finalize=="deadline")'
//
// It is stamped just before the close and CLEARED again if that close is
// refused, so the marker and the counted event never disagree — a seat whose
// forced close was refused and which later finalizes through the ordinary path
// must not close carrying deadline provenance it did not earn.
const drainFinalizeMetadataKey = "drain_finalize"

// drainFinalizeDeadline is the drainFinalizeMetadataKey value stamped on a pool
// seat retired by poolSlotDrainRetireDeadline rather than by its own drain.
const drainFinalizeDeadline = "deadline"

// poolSlotRetireWorktreePrune is the worktree reclaim this path performs after
// a retirement, as a swappable seam so the pin can observe it without a real
// worktree on disk. Production value is the same helper the pool-freeable close
// uses; the deadline path preempts that close, so without this call every
// retired seat would leak its worktree.
var poolSlotRetireWorktreePrune = pruneAgentHomeWorktreeIfSafeInfo

func swapWorktreePruneForTest(fn func(sessionpkg.Info, string, *config.City, io.Writer)) func() {
	prev := poolSlotRetireWorktreePrune
	poolSlotRetireWorktreePrune = fn
	return func() { poolSlotRetireWorktreePrune = prev }
}

// sessionInUnfinalizedDrain reports whether info is still parked in the drain
// that began at drainAt — as opposed to a seat that drained once, came back,
// and is working normally with a stale marker.
//
// Two independent discriminators, because the obvious one is not enough. The
// reconciler re-heals a drained bead whose runtime is still alive back to
// state=awake (healStatePatchWithRollbackInfo → ProjectLifecycle), so by the
// second tick the stuck seat no longer reads as drained in its state at all.
// What survives that heal is sleep_reason=drained and an empty last_woke_at:
// every drain-completion patch (AcknowledgeDrainPatch, SleepPatch,
// CompleteDrainPatch) clears last_woke_at, and only a real wake re-stamps it.
//
// So: a wake at or after drainAt ended the drain, and an unparseable wake
// marker fails closed. Beyond that the bead must still carry drain provenance,
// which excludes the live seat whose drain was CANCELED (scale-back-up) and
// which therefore kept its pre-drain last_woke_at and lost its sleep_reason.
//
// state=draining is deliberately NOT matched. In production it is unreachable
// here: BeginDrainPatch is the sole writer of both state=draining and drain_at,
// its only non-test caller is DrainAckStopPendingPatch, and every row that pairs
// produces is intercepted by reconcileDrainAckStopPending, which continues
// before this gate. That population converges through its own machinery when its
// runtime is killable (measured: 3 ticks); when the runtime is NOT killable it
// stays open — correctly, because no bound may close a bead over a live agent.
func sessionInUnfinalizedDrain(info sessionpkg.Info, drainAt time.Time) bool {
	if raw := strings.TrimSpace(info.LastWokeAt); raw != "" {
		wokeAt, err := time.Parse(time.RFC3339, raw)
		if err != nil || !wokeAt.Before(drainAt) {
			return false
		}
	}
	if strings.TrimSpace(info.SleepReason) == string(sessionpkg.SleepReasonDrained) {
		return true
	}
	return strings.TrimSpace(info.MetadataState) == string(sessionpkg.StateDrained)
}

// poolSlotDrainAgePastDeadline returns how long info has been parked in an
// unfinalized drain, and whether that exceeds poolSlotDrainRetireDeadline.
//
// drain_at is the only persistent clock for this: the in-memory drainTracker
// resets on every controller restart, and drain_at survives the
// draining → drained transition. A missing or unparseable marker fails closed —
// without a durable start instant there is no evidence the seat is overdue.
func poolSlotDrainAgePastDeadline(info sessionpkg.Info, now time.Time) (time.Duration, bool) {
	drainAt, err := time.Parse(time.RFC3339, strings.TrimSpace(info.DrainAt))
	if err != nil {
		return 0, false
	}
	if !sessionInUnfinalizedDrain(info, drainAt) {
		return 0, false
	}
	age := now.Sub(drainAt)
	if age < poolSlotDrainRetireDeadline {
		return 0, false
	}
	return age, true
}

// poolSlotRetireBlocker names the advisory hold that forbids retiring info, or
// "" when none applies.
//
// isPoolSessionSlotFreeable promises that a session "parked via `gc session
// wait`, held by context-churn quarantine, or otherwise signaling 'don't touch
// me' keeps its slot". Base honors that only because the state heal rewrites
// `state` before that gate runs. This bound reads the RAW pre-heal state and
// runs at the top of the forward pass, ahead of all wake/sleep/hold handling,
// so it must check the blockers itself or it silently overrides every one of
// them — re-minting the seat a churn quarantine exists to hold back, and
// reaping a session an operator explicitly parked.
func poolSlotRetireBlocker(info sessionpkg.Info, now time.Time) string {
	if blocker := lifecycleTimerBlockerInfo(info, now); blocker != "" {
		return blocker
	}
	if strings.TrimSpace(info.WaitHold) != "" {
		return "wait_hold"
	}
	return ""
}

// poolSlotRetireAssigneeIdentities is the identity set this path probes work
// under: the canonical session.AssigneeIdentities set (bead ID, session_name,
// configured_named_identity, alias, and every prior alias in alias_history),
// unioned with the configured-named resolution the ordinary close gate applies.
//
// The alias is not optional here. An agent claims beads as BEADS_ACTOR, which
// AssigneeIdentifier resolves ALIAS-FIRST, and sling treats an assignee of the
// form <template>-<n> as a legitimate claim by that pool's own session. A pool
// slot's alias diverges from its session_name exactly when the runtime name
// steps aside to "<identity>-pool" — the ga-rxhu2 specimen's own shape. Probing
// the narrower {ID, session_name, configured_named_identity} set would be blind
// to the agent's own claims on precisely the configuration this bound targets,
// and unlike every other consumer of that narrow set, this path uses the answer
// to authorize a Kill, not just a close of an already-dead runtime.
func poolSlotRetireAssigneeIdentities(info sessionpkg.Info, cfg *config.City) []string {
	raw := append([]string{}, sessionBeadAssigneeIdentitiesInfo(info)...)
	raw = append(raw, sessionAssignmentIdentifiersForConfigInfo(info, cfg)...)
	return compactSessionAssignmentIdentifiers(raw)
}

// poolSlotRetireHasAssignedWork probes every reachable store for work held
// under any of the seat's assignment identities, excluding the session's own
// mol-do-work drain step exactly as the drain-ack close gate does. It fails
// closed on an unreadable leg (a smaller answer presented as authoritative
// would read as "holds nothing", and this path acts on that).
func poolSlotRetireHasAssignedWork(
	cityPath string,
	cfg *config.City,
	store beads.Store,
	rigStores map[string]beads.Store,
	info sessionpkg.Info,
) (bool, error) {
	identifiers := poolSlotRetireAssigneeIdentities(info, cfg)
	return assignedWorkExistsForSession(cityPath, cfg, store, rigStores, info, func(s beads.Store) (bool, error) {
		return sessionHasOpenAssignedWorkInStoreByIdentifiersForCloseGate(s, identifiers)
	})
}

// retirePoolSlotAtDrainDeadline force-retires a pool-managed seat whose drain
// has outlived poolSlotDrainRetireDeadline, freeing the runtime name its slot
// is pinned to. It returns the metadata fold for the reconciler's typed
// snapshot and whether the retirement happened.
//
// The order of the gates is the safety argument, and it is not rearrangeable:
//
//  1. Pool-managed only. A named or manual session's identity is not
//     disposable, and nothing about it can starve a pool to zero seats.
//  2. Not a degraded tick. A partial store enumeration cannot prove a seat is
//     idle, and the boot tick defers session closes because this exact
//     per-candidate multi-store fan-out is what #3288 moved off the readiness
//     path. Every other close in this loop honors both; so does this one.
//  3. No advisory hold (poolSlotRetireBlocker).
//  4. Past the deadline, in a drain that never finalized.
//  5. No assigned work under ANY of the seat's identities — checked BEFORE
//     anything touches the runtime, and AGAIN immediately before the kill,
//     because the first probe walks every residency leg and a seat healed back
//     to awake is an ordinary live seat to anything that routes work. Claims
//     that outlive their session are a different defect (ga-ee8eo).
//  6. The runtime is proven absent, or stopped and then re-observed gone. A
//     stop whose result is unknown does not authorize a close: closing over a
//     surviving agent produces a live runtime that no bead owns, which is
//     strictly worse than the stalled slot it would replace.
//  7. A final work fence immediately before the write, because the stop itself
//     takes time.
//
// Every failure is fail-closed: a degraded store read or a degraded runtime
// observation proves nothing, so the seat keeps its bead and the next tick
// re-decides.
func retirePoolSlotAtDrainDeadline(
	cityPath string,
	cfg *config.City,
	sp runtime.Provider,
	store beads.Store,
	rigStores map[string]beads.Store,
	info sessionpkg.Info,
	template string,
	processNames []string,
	storeQueryPartial bool,
	deferClosesOnBoot bool,
	clk clock.Clock,
	rec events.Recorder,
	stderr io.Writer,
) (sessionpkg.MetadataPatch, bool) {
	if store == nil || sp == nil || info.ID == "" || info.Closed {
		return nil, false
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if storeQueryPartial || deferClosesOnBoot {
		return nil, false
	}
	if !isPoolManagedSessionInfo(info) {
		return nil, false
	}
	name := strings.TrimSpace(info.SessionNameMetadata)
	if name == "" {
		// No persisted runtime name means no pool slot name is being held, so
		// there is nothing here for this bound to free.
		return nil, false
	}
	if blocker := poolSlotRetireBlocker(info, clk.Now()); blocker != "" {
		return nil, false
	}
	drainAge, overdue := poolSlotDrainAgePastDeadline(info, clk.Now().UTC())
	if !overdue {
		return nil, false
	}

	hasAssignedWork, err := poolSlotRetireHasAssignedWork(cityPath, cfg, store, rigStores, info)
	if err != nil {
		fmt.Fprintf(stderr, "session reconciler: checking assigned work for drain-deadline retire of %s: %v\n", name, err) //nolint:errcheck
		return nil, false
	}
	if hasAssignedWork {
		return nil, false
	}

	stopped, performedStop := poolSlotRuntimeStoppedForRetire(cityPath, cfg, sp, store, rigStores, info, name, processNames, stderr)
	if !stopped {
		return nil, false
	}

	now := clk.Now().UTC()
	// Final fence: the stop above takes time, and the close must not land on a
	// seat that claimed work while it was running.
	stillAssigned, err := poolSlotRetireHasAssignedWork(cityPath, cfg, store, rigStores, info)
	if err != nil {
		fmt.Fprintf(stderr, "session reconciler: re-checking assigned work before drain-deadline close of %s: %v\n", name, err) //nolint:errcheck
		return nil, false
	}
	if stillAssigned {
		return nil, false
	}
	// Stamp the provenance ahead of the close so it lands in the same terminal
	// record, and roll it back if the close does not happen — nothing else ever
	// clears this key, so a marker left on a still-open bead would follow it
	// into whatever close comes later and inflate the deadline-retirement count.
	if err := sessionFrontDoor(store).ApplyPatch(info.ID, sessionpkg.MetadataPatch{drainFinalizeMetadataKey: drainFinalizeDeadline}); err != nil {
		fmt.Fprintf(stderr, "session reconciler: stamping drain-deadline provenance on %s: %v\n", name, err) //nolint:errcheck
		return nil, false
	}
	if !closeBead(store, info.ID, "drained", now, stderr) {
		if clearErr := sessionFrontDoor(store).ApplyPatch(info.ID, sessionpkg.MetadataPatch{drainFinalizeMetadataKey: ""}); clearErr != nil {
			fmt.Fprintf(stderr, "session reconciler: clearing drain-deadline provenance after a refused close of %s: %v\n", name, clearErr) //nolint:errcheck
		}
		return nil, false
	}

	// Pool worktrees are transient by design; the deadline path preempts the
	// pool-freeable close, which is the only other site that reclaims them.
	poolSlotRetireWorktreePrune(info, cityPath, cfg, stderr)

	fmt.Fprintf(stderr, "session reconciler: retired pool slot %s at the drain deadline after %s in an unfinalized drain; its runtime name is free again\n", name, drainAge.Round(time.Second)) //nolint:errcheck
	if performedStop {
		// A stop this path performed is a real agent stop: it belongs in the
		// stop counter and on the session.stopped envelope, or the lifecycle
		// timeline for a retired seat ends with no stop at all.
		telemetry.RecordAgentStop(context.Background(), name, sessionAgentMetricIdentityInfo(info, cfg), "drain-deadline", nil)
	}
	if rec != nil {
		if performedStop {
			rec.Record(events.Event{
				Type:      events.SessionStopped,
				Actor:     "gc",
				Subject:   template,
				Message:   "stopped at the pool-slot drain deadline",
				SessionID: info.ID,
				Payload:   api.SessionLifecyclePayloadJSON(info.ID, template, "drain deadline"),
			})
		}
		rec.Record(events.Event{
			Type:      events.SessionPoolSlotRetiredAtDrainDeadline,
			Actor:     "gc",
			Subject:   info.ID,
			Message:   fmt.Sprintf("pool slot %s retired at the drain deadline after %s in an unfinalized drain", name, drainAge.Round(time.Second)),
			SessionID: info.ID,
			Payload: api.SessionPoolSlotRetiredAtDrainDeadlinePayloadJSON(
				info.ID,
				name,
				template,
				strings.TrimSpace(info.DrainAt),
				drainAge,
			),
		})
	}

	// The returned patch mirrors the terminal record the store now carries. The
	// provenance key rides along for fidelity even though the Info codec does
	// not project it — the same shape ClosePatch's own synced_at has.
	patch := sessionpkg.ClosePatch(now, "drained")
	patch[drainFinalizeMetadataKey] = drainFinalizeDeadline
	return patch, true
}

// poolSlotRuntimeStoppedForRetire proves the seat's runtime is gone before its
// bead may be retired: absent already, or killed and then re-observed absent.
// It reports whether the runtime is confirmed gone, and whether this call is
// the one that stopped it (an already-absent runtime is not a stop this path
// performed, and must not inflate the stop counter).
//
// It returns false for every uncertain outcome — a failed observation, a failed
// kill, or a runtime that survived the kill — because the close it authorizes
// is what makes a still-running agent unowned.
//
// The kill is fenced twice. The instance token guards against killing a
// different incarnation that has since taken the name, exactly as every other
// kill-by-name in the reconciler does (verifiedStop, queueDrainAckAsyncStop).
// The assigned-work re-probe guards against killing an agent that claimed work
// during the multi-store walk the caller's first probe performed: the close is
// re-fenced downstream, but a kill cannot be taken back.
func poolSlotRuntimeStoppedForRetire(
	cityPath string,
	cfg *config.City,
	sp runtime.Provider,
	store beads.Store,
	rigStores map[string]beads.Store,
	info sessionpkg.Info,
	name string,
	processNames []string,
	stderr io.Writer,
) (confirmedGone bool, performedStop bool) {
	obs, err := workerObserveSessionTargetWithRuntimeHintsWithConfig(cityPath, store, sp, cfg, info.ID, processNames)
	if err != nil {
		fmt.Fprintf(stderr, "session reconciler: observing %s for drain-deadline retire: %v\n", name, err) //nolint:errcheck
		return false, false
	}
	if !obs.Running && !obs.Alive {
		return true, false
	}
	if expected := strings.TrimSpace(info.InstanceToken); expected != "" {
		if actual, _ := sp.GetMeta(name, "GC_INSTANCE_TOKEN"); actual != "" && actual != expected {
			fmt.Fprintf(stderr, "session reconciler: drain-deadline retire of %s skipped: instance token mismatch (session was replaced)\n", name) //nolint:errcheck
			return false, false
		}
	}
	claimed, err := poolSlotRetireHasAssignedWork(cityPath, cfg, store, rigStores, info)
	if err != nil {
		fmt.Fprintf(stderr, "session reconciler: re-checking assigned work before drain-deadline stop of %s: %v\n", name, err) //nolint:errcheck
		return false, false
	}
	if claimed {
		return false, false
	}
	if err := workerKillSessionTargetWithConfig(cityPath, store, sp, cfg, name); err != nil && !runtime.IsSessionGone(err) {
		fmt.Fprintf(stderr, "session reconciler: drain-deadline stop of %s: %v\n", name, err) //nolint:errcheck
		return false, false
	}
	after, err := workerObserveSessionTargetWithRuntimeHintsWithConfig(cityPath, store, sp, cfg, info.ID, processNames)
	if err != nil || after.Running || after.Alive {
		fmt.Fprintf(stderr, "session reconciler: %s survived its drain-deadline stop; leaving the slot open rather than orphaning a live runtime\n", name) //nolint:errcheck
		return false, false
	}
	return true, true
}
