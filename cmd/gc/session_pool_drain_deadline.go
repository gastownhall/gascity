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
// state=draining is deliberately NOT matched, and the premise for that is
// narrower than it first looks. BeginDrainPatch is the sole writer of both
// state=draining and drain_at, but it has TWO callers, not one:
// DrainAckStopPendingPatch, whose rows reconcileDrainAckStopPending intercepts
// and continues before this gate, and the exported session.Manager.BeginDrain,
// which has no production caller today (only tests). So the real premise is
// "drain_at is stamped on the controller's drain-ack path", and it holds by
// call-graph accident rather than by invariant: wiring an operator-facing drain
// to Manager.BeginDrain would widen this bound's population with nothing
// failing. That is why gate 1 (poolSlotRetireOwnsSeat) enforces the identity
// and state exclusions instead of arguing them from reachability.
//
// The drain-ack population converges through its own machinery when its runtime
// is killable (measured: 3 ticks); when the runtime is NOT killable it stays
// open — correctly, because no bound may close a bead over a live agent.
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

// poolSlotRetireTemplate resolves the template name this retirement gates on
// and reports.
//
// The reconciler passes the DESIRED template, which is the zero value for a
// seat whose name is absent from desiredState. The seat's own projected
// info.Template is the durable answer for that case. For the legacy-manual
// shape, what it buys is ATTRIBUTION rather than seat safety: an empty template
// resolves no config agent, so for a seat that merely left the desired set
// while its agent is STILL configured the legacy-manual exclusion in
// poolSlotRetireOwnsSeat does degrade to a no-op — but gate 1's marker-less
// refusal then catches that seat on the nil-agent arm instead, so it is refused
// either way and only the check doing the refusing differs. Separately, for a
// seat that IS retired while absent from desiredState, both emitted events
// would name an empty template, one of the two fields the payload doc names for
// reading the age distribution per pool.
//
// There is one shape whose OUTCOME turns on this fallback, and it turns toward
// acting rather than refusing: the derived-ephemeral seat gate 1 deliberately
// admits — raw-empty session_origin, slot-shaped name, multi-session agent —
// reaches its agent only through info.Template, and an empty template would
// send it to the marker-less refusal instead of freeing the slot name it
// squats. That population is "undesired and pre-backfill", so it is exactly the
// population this fallback exists for.
//
// It cannot recover the case where the agent itself left config, because then
// no name resolves an agent at all — there the unattributable-identity check in
// poolSlotRetireOwnsSeat, not this fallback, is what holds the bound off the
// seat.
func poolSlotRetireTemplate(info sessionpkg.Info, desiredTemplate string) string {
	if template := strings.TrimSpace(desiredTemplate); template != "" {
		return template
	}
	return strings.TrimSpace(info.Template)
}

// poolSlotRetireOwnsSeat is gate 1: whether this bound may act on info's seat
// at all. Each paragraph below is an exclusion the rest of the ladder assumes.
//
// Identity. isPoolManagedSessionInfo is satisfied by session_origin=ephemeral
// ALONE, and that is exactly the shape the repo's own
// isLegacyManualSessionInfoForAgent defines as a legacy USER-created seat
// (ephemeral origin, no pool_managed, no pool_slot — sessions persisted before
// the manual-origin backfill). So the pool predicate by itself does not deliver
// "a named or manual session's identity is not disposable". The sibling
// pool-freeable close pairs the same predicate with !isNamedSessionInfo; this
// path needs the manual exclusion too, and needs it more, because it answers
// with a Kill of a LIVE runtime rather than a close over an already-dead one.
//
// Attribution. That exclusion is only as good as the pool it resolves. Narrow
// the population to what can still reach this point — admitted by the pool
// predicate, already past the named check, and carrying NO positive pool marker
// — and three of isLegacyManualSessionInfoForAgent's bail-out clauses are dead
// by construction (named, pool_managed, pool_slot). Four stay live, and a seat
// slips the legacy arm on any one of them: a nil agent; an agent that resolves
// but is not multi-session (no namepool and max_active_sessions=1 — see
// config.Agent.SupportsMultipleSessions); info.DependencyOnly; and a raw
// info.SessionOrigin that is not "ephemeral".
//
// That last one is live because admission never required a STAMPED origin.
// sessionOriginInfo DERIVES "ephemeral" from a raw-empty origin — on
// DependencyOnly alone, or on a slot-shaped session name via resolvePoolSlot —
// while the legacy predicate tests the raw field. It also fires FIRST, ahead of
// the DependencyOnly clause, so for this file's own dependency-only fixture,
// which deliberately stamps no session_origin, the origin bail-out is what
// slips the legacy arm rather than that final clause.
//
// The refusal below covers three of those four shapes: nil agent,
// single-session agent, and dependency-only. The first two collapse into a
// single term because SupportsMultipleSessions is false on a nil receiver; the
// third is a separate OR because a dependency-only seat's agent need not be
// single-session, and ANDing the terms would leave exactly that seat admitted.
// The fourth shape stays deliberately ADMITTED: a raw-empty-origin seat with a
// slot-shaped name under a multi-session agent is squatting a pool slot name,
// which is the exact contention this bound exists to free.
//
// The condition is not "no agent resolves" — it is "no agent resolves a POOL".
// An unattributable seat cannot be proven disposable either way, and a
// dependency-only one holds no pool slot at all (sessionOriginInfo calls it
// ephemeral on DependencyOnly alone, with no marker required), so retiring it
// would free nothing this bound exists to free while still killing a live
// runtime. Multi-session capability arrives here as a side effect of reusing
// the legacy predicate's own semantics: it is a MIGRATION-eligibility condition
// upstream, so the refusal mirrors it rather than importing it as a
// disposability test in its own right.
//
// The refusal stays narrowed to seats carrying NO positive pool marker, because
// a pool_managed/pool_slot seat whose agent left config is exactly the stuck
// population this bound exists to retire — failing closed on it would turn the
// gate into a feature-off switch. Markers are written by this reconciler;
// ephemeral origin alone is not.
//
// Every refusal here is deliberately silent: this is a pure predicate over
// every session on every tick, so a log line belongs with the gate's other
// observability rather than inside it. When the bound never fires on a seat you
// expected it to free, read pool_managed/pool_slot on the bead first — their
// absence is the precondition for the refusal above, and a marker-bearing seat
// never reaches it.
//
// State. This gate runs ABOVE the reconciler's !isKnownStateInfo skip, and
// sessionInUnfinalizedDrain's sleep_reason arm matches without consulting state
// at all — so without this check an older reconciler rolled back under a newer
// writer could kill and close a bead parked in a state it has never seen, which
// is precisely the population that skip exists to leave alone. The precedent
// that also acts pre-skip (reconcileDrainAckStopPending) matches one exact
// state it owns outright; this path matches a provenance marker, so it is the
// first pre-skip handler that can fire on a state it has never seen and it must
// respect the allowlist itself. Both shapes this bound targets — drained, and
// the awake ghost the heal decays it into — are in knownSessionStates, so
// nothing in scope is lost. draining is NOT in that set (it is exactly the
// forward-compat example the skip's own comment names), which makes the
// "state=draining is deliberately NOT matched" claim above structural here
// rather than an argument from reachability.
func poolSlotRetireOwnsSeat(info sessionpkg.Info, cfg *config.City, template string) bool {
	if !isPoolManagedSessionInfo(info) {
		return false
	}
	cfgAgent := findAgentByTemplate(cfg, template)
	if isNamedSessionInfo(info) || isManualSessionInfoForAgent(info, cfgAgent) {
		return false
	}
	if !info.PoolManaged && strings.TrimSpace(info.PoolSlot) == "" &&
		(!cfgAgent.SupportsMultipleSessions() || info.DependencyOnly) {
		return false
	}
	return isKnownStateInfo(info)
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
//  1. A seat this bound owns (poolSlotRetireOwnsSeat): pool-managed, never a
//     named or manual identity — including the LEGACY manual shape that the
//     pool predicate alone admits — attributable to a configured POOL (an agent
//     that resolves and is multi-session, and not a dependency-only seat) unless
//     it carries this reconciler's own markers, and in a state this reconciler
//     recognizes. A human-owned identity is not disposable, an unattributable
//     one cannot be shown to be otherwise, a dependency-only one holds no slot
//     to free, nothing about any of them can starve a pool to zero seats, and a
//     state written by a newer version belongs to the forward-compat skip below
//     this call site, not to this path.
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
	template = poolSlotRetireTemplate(info, template)
	if !poolSlotRetireOwnsSeat(info, cfg, template) {
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
	if performedStop {
		// A stop this path performed is a fact the moment the kill is
		// re-observed, and it is recorded here rather than after the close
		// because everything below can still refuse: the final work fence, the
		// provenance stamp, and the close each return early. Emitting only on
		// the successful path loses the stop outright on those arms — the next
		// tick finds the runtime already absent, retires with
		// performedStop=false, and never emits it — so a real agent stop would
		// be permanently invisible to the stop counter and the lifecycle
		// timeline. Only the retirement event below belongs to the close.
		telemetry.RecordAgentStop(context.Background(), name, sessionAgentMetricIdentityInfo(info, cfg), "drain-deadline", nil)
		if rec != nil {
			rec.Record(events.Event{
				Type:      events.SessionStopped,
				Actor:     "gc",
				Subject:   template,
				Message:   "stopped at the pool-slot drain deadline",
				SessionID: info.ID,
				Payload:   api.SessionLifecyclePayloadJSON(info.ID, template, "drain deadline"),
			})
		}
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
	if rec != nil {
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
