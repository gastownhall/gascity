package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// exactSessionConfigDriftHalf names which fingerprint moved on one durable row.
// Legacy spells the two halves as two trace sites reached through one compare
// chain — the live half is the ELSE of the core compare — so the keyed family
// carries the same exclusivity: a core-drifted row is never also a live-drift
// candidate on the same pass.
type exactSessionConfigDriftHalf string

const (
	exactSessionConfigDriftCore exactSessionConfigDriftHalf = "core"
	exactSessionConfigDriftLive exactSessionConfigDriftHalf = "live"
)

// exactSessionConfigDrift is one resolved drift condition: which half moved,
// the two hashes, and the resolved template the convergence effects execute
// against. Resolving the template is the expensive rung, so the seam guard and
// the handler share ONE resolution rather than answering the same question from
// two derivations that could skew.
type exactSessionConfigDrift struct {
	Half        exactSessionConfigDriftHalf
	Site        TraceSiteCode
	StoredHash  string
	CurrentHash string
	// DriftKey is legacy's own deferral key — "<storedCore>:<currentCore>" — so
	// a deferral stamp legacy wrote is read back by exactly the same name.
	DriftKey string
	Template TemplateParams
	AgentCfg runtime.Config
	// LaunchOnly reports that the provision half held while the launch half
	// moved, so the agent can be relaunched into the existing warm box.
	LaunchOnly           bool
	StoredProvisionHash  string
	StoredLaunchHash     string
	DriftedFields        []string
	SessionName          string
	Named                bool
	CurrentLiveFinger    string
	SessionLiveConfigLen int
}

// resolveExactSessionConfigDrift re-derives the whole condition from ONE durable
// row: the template, the executable-config-for-hash form, and both fingerprint
// compares. It is the family's single source of truth — the detector's reason is
// a scheduling hint and the row is the authority (the seam's second rule) — and
// it is pure apart from the template resolution's idempotent hook install, which
// the ordinary start path already pays per admission.
//
// The cheap durable rungs run first so a row that cannot possibly be drifted
// (closed, unnamed, no baseline stamped yet, a create still in flight) never
// pays the resolution at all.
func resolveExactSessionConfigDrift(
	params exactSessionStartParams,
	info sessionpkg.Info,
	clk clock.Clock,
) (exactSessionConfigDrift, bool) {
	if clk == nil {
		clk = clock.Real{}
	}
	name := strings.TrimSpace(info.SessionNameMetadata)
	storedHash := strings.TrimSpace(info.StartedConfigHash)
	// #127: before the startup window stamps a baseline there is nothing to
	// compare against, and calling a starting session drifted would drain it.
	if info.Closed || name == "" || storedHash == "" {
		return exactSessionConfigDrift{}, false
	}
	// A create still holds the row. Legacy's drift arms run on rows that are
	// alive (or asleep and named); a queued or leased create is the pending-create
	// recovery path's, and D-STALE-CREATE's when its lease expires.
	if info.PendingCreateClaim || pendingCreateQueuedOrCreatingState(info.MetadataState) {
		return exactSessionConfigDrift{}, false
	}
	// A restart already requested on the row is the RESTART family's, never this
	// one, and the rung is legacy's own pass order: the restart-requested block
	// (session_reconciler.go:2806, reading the same durable marker at :2819) runs
	// ABOVE the config-drift block (:3050) and `continue`s the row past it once
	// the kill lands (:2906). The single path that falls through has already
	// applied RestartRequestPatch, which clears started_config_hash — so legacy's
	// drift compare cannot see a drifted row carrying this marker by either
	// route. The restart is also what converges the drift: the fresh start
	// re-stamps all four fingerprints, so yielding here costs no convergence.
	//
	// Claiming it inverted that order and swallowed a public `gc session reset`
	// whole (ga-f7v2ft.138): the keyed ordinary reset arm lives BELOW the
	// detector-family seam in reconcileExactSessionStartWithOwner, so a D-DRIFT
	// answer here consumed the admission and the reset never ran — and, because
	// this family carries no drain-tracker gate, it acted on rows ga-f7v2ft.103's
	// legacy-drain park fence exists to leave alone. The marker is the whole
	// predicate: named and pool rows keyed does not own belong to legacy's block
	// above, which this yield leaves untouched.
	if strings.TrimSpace(info.RestartRequested) == "true" {
		return exactSessionConfigDrift{}, false
	}
	template := normalizedSessionTemplateInfo(info, params.Config)
	if template == "" {
		template = strings.TrimSpace(info.Template)
	}
	cfgAgent := findAgentByTemplate(params.Config, template)
	if template == "" || cfgAgent == nil {
		return exactSessionConfigDrift{}, false
	}
	stderr := params.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	tp, resolvedInfo, err := resolveExactSessionStartTemplate(params, info, cfgAgent, clk, stderr)
	// Fold the resolver's Info back: the named path may have durably cleared a
	// stale trigger stamp, and the drift verdict below is computed off info.
	info = resolvedInfo
	if err != nil {
		// An unresolvable template is not a drift verdict. Fail closed: the row
		// keeps whatever legacy makes of it, and the condition is re-detected.
		return exactSessionConfigDrift{}, false
	}
	agentCfg := sessionCoreConfigForHashInfo(tp, info)
	currentHash := runtime.CoreFingerprint(agentCfg)
	drift := exactSessionConfigDrift{
		Template:             tp,
		AgentCfg:             agentCfg,
		SessionName:          name,
		Named:                isNamedSessionInfo(info),
		DriftKey:             storedHash + ":" + currentHash,
		CurrentLiveFinger:    runtime.LiveFingerprint(agentCfg),
		SessionLiveConfigLen: len(agentCfg.SessionLive),
	}
	if storedHash != currentHash {
		drift.Half = exactSessionConfigDriftCore
		drift.Site = TraceSiteReconcilerConfigDrift
		drift.StoredHash = storedHash
		drift.CurrentHash = currentHash
		drift.StoredProvisionHash = info.StartedProvisionHash
		drift.StoredLaunchHash = info.StartedLaunchHash
		// Empty sub-hashes (a session started before the partitioned
		// fingerprints existed) are NOT launch-only: the full restart re-stamps
		// them and self-heals.
		drift.LaunchOnly = drift.StoredProvisionHash != "" && drift.StoredLaunchHash != "" &&
			drift.StoredProvisionHash == runtime.ProvisionFingerprint(agentCfg) &&
			drift.StoredLaunchHash != runtime.LaunchFingerprint(agentCfg)
		drift.DriftedFields = runtime.CoreFingerprintDriftFieldsFromJSON(info.CoreHashBreakdown, agentCfg)
		return drift, true
	}
	storedLive := info.StartedLiveHash
	if storedLive == drift.CurrentLiveFinger {
		return exactSessionConfigDrift{}, false
	}
	drift.Half = exactSessionConfigDriftLive
	drift.Site = TraceSiteReconcilerLiveDrift
	drift.StoredHash = storedLive
	drift.CurrentHash = drift.CurrentLiveFinger
	return drift, true
}

// exactSessionConfigDriftCandidate is the D-DRIFT seam guard, shared by BOTH of
// the family's halves: the fact that forks converge from defer is attachment,
// which is provider I/O the guard may not pay, so one guard admits the row and
// the handler's ladder decides. It reads nothing but the durable row and the
// config — never admission.Source — because drift is level-triggered and the
// controller coalesces admissions on a key: a config edit drifts the fleet onto
// keys that already carry ordinary start admissions, and every one of those must
// reach this family.
func exactSessionConfigDriftCandidate(
	params exactSessionStartParams,
	info sessionpkg.Info,
	response sessionpkg.PersistedResponse,
	clk clock.Clock,
) bool {
	if response.Revision == 0 {
		return false
	}
	_, ok := resolveExactSessionConfigDrift(params, info, clk)
	return ok
}

// exactSessionConfigDriftDeferralRung names which of the ladder's five A6 rungs
// a row landed on. The rung — not the free-text reason — selects the effect,
// because two different rungs can report the SAME reason: a named session in
// active use and an ordinary session with a user waiting both answer
// "pending_interaction", and legacy applies a window stamp to the first and a
// drain cancel to the second.
type exactSessionConfigDriftDeferralRung string

const (
	driftDeferralAttached           exactSessionConfigDriftDeferralRung = "attached"
	driftDeferralAttachedRecently   exactSessionConfigDriftDeferralRung = "attached_recently"
	driftDeferralNamedActive        exactSessionConfigDriftDeferralRung = "named_active"
	driftDeferralPendingInteraction exactSessionConfigDriftDeferralRung = "pending_interaction"
	driftDeferralAssignedWork       exactSessionConfigDriftDeferralRung = "live_assigned_work"
)

// exactSessionConfigDriftDeferral is one rung of the ladder's A6 half: a reason
// a human is engaged with this session, so the convergence must wait. Reason is
// legacy's own active_reason payload, kept verbatim so the WD.15 parity join
// compares the two populations field for field.
type exactSessionConfigDriftDeferral struct {
	Rung        exactSessionConfigDriftDeferralRung
	Reason      string
	Outcome     TraceOutcomeCode
	TraceReason TraceReasonCode
}

// exactSessionConfigDriftDeferralReason classifies the row against the ladder's
// deferral rungs. It is a pure classifier: the writes each rung owes — the
// attached window stamp, the named bounded-window stamp, the queued drift-drain
// cancel — belong to applyExactSessionConfigDriftDeferral, so the decision and
// the effect never answer from two derivations that could skew.
//
// It returns an error only when the observation itself failed. Legacy `continue`s
// on that error, so the handler refuses with zero effect for the same reason: an
// unreadable attachment probe is not "nobody is attached".
func exactSessionConfigDriftDeferralReason(
	params exactSessionStartParams,
	info sessionpkg.Info,
	drift exactSessionConfigDrift,
	clk clock.Clock,
) (exactSessionConfigDriftDeferral, error) {
	name := drift.SessionName
	attached, attachErr := sessionAttachedForConfigDrift(info.ID, params.Provider, params.CityPath, params.Store, params.Config, name)
	if attachErr != nil {
		return exactSessionConfigDriftDeferral{}, fmt.Errorf("observing config-drift attachment for %q: %w", name, attachErr)
	}
	if attached {
		return exactSessionConfigDriftDeferral{
			Rung: driftDeferralAttached, Reason: "attached",
			Outcome: TraceOutcomeDeferredAttached, TraceReason: TraceReasonConfigDrift,
		}, nil
	}
	// A single transient IsAttached false negative would destroy an attached
	// conversation irreversibly, so a recent deferral for the SAME drift key
	// still counts as attached.
	if recentlyDeferredSessionAttachedConfigDrift(info, clk, drift.DriftKey) {
		return exactSessionConfigDriftDeferral{
			Rung: driftDeferralAttachedRecently, Reason: "attached_recently",
			Outcome: TraceOutcomeDeferredAttached, TraceReason: TraceReasonConfigDrift,
		}, nil
	}
	if drift.Named {
		// An operator-pinned named session is a declared critical conversation:
		// config drift must never collaterally recycle it.
		if pinnedConfiguredNamedSessionKillProtected(info) {
			return exactSessionConfigDriftDeferral{
				Rung: driftDeferralNamedActive, Reason: "pinned",
				Outcome: TraceOutcomeDeferredActive, TraceReason: TraceReasonConfigDrift,
			}, nil
		}
		reason, active := namedSessionActiveUseReasonInfo(info, params.Provider, name, clk)
		if active && namedSessionConfigDriftDeferralStillBinding(info, clk, drift.DriftKey, reason) {
			return exactSessionConfigDriftDeferral{
				Rung: driftDeferralNamedActive, Reason: reason,
				Outcome: TraceOutcomeDeferredActive, TraceReason: TraceReasonConfigDrift,
			}, nil
		}
		return exactSessionConfigDriftDeferral{}, nil
	}
	if pendingInteractionKeepsAwakeInfo(info, params.Provider, name, clk) {
		// Legacy traces this rung under the PENDING reason rather than the drift
		// reason; the keyed record carries the same pair so the join lines up.
		return exactSessionConfigDriftDeferral{
			Rung: driftDeferralPendingInteraction, Reason: "pending_interaction",
			Outcome: TraceOutcomeDeferredPending, TraceReason: TraceReasonPending,
		}, nil
	}
	// A pool-routed session mid-task must not be drained: the assigned bead
	// would be orphaned (assignee pointing at a dead session, status stuck at
	// in_progress). The next pass sees no assigned work and converges naturally.
	hasAssignedWork, assignedErr := sessionHasOpenAssignedWorkForReachableStore(params.CityPath, params.Config, params.Store, params.RigStores, info)
	if assignedErr != nil {
		return exactSessionConfigDriftDeferral{}, fmt.Errorf("checking assigned work before config-drift convergence of %q: %w", name, assignedErr)
	}
	if hasAssignedWork {
		return exactSessionConfigDriftDeferral{
			Rung: driftDeferralAssignedWork, Reason: "live_assigned_work",
			Outcome: TraceOutcomeDeferredActive, TraceReason: TraceReasonConfigDrift,
		}, nil
	}
	return exactSessionConfigDriftDeferral{}, nil
}

// configDriftDeferralWindowStart reads back the named deferral window's start
// for THIS drift key. It is the single source of truth for "has the bounded
// window started" — legacy's boundedNamedSessionConfigDriftDeferral asks it
// before stamping, the keyed handler asks it before stamping, and
// namedSessionConfigDriftDeferralStillBinding asks it before expiring — so a
// stamp written by either writer is read identically by both.
func configDriftDeferralWindowStart(info sessionpkg.Info, driftKey string) (time.Time, bool) {
	if info.ConfigDriftDeferredKey != driftKey || info.ConfigDriftDeferredAt == "" {
		return time.Time{}, false
	}
	deferredAt, err := time.Parse(time.RFC3339, info.ConfigDriftDeferredAt)
	if err != nil {
		return time.Time{}, false
	}
	return deferredAt, true
}

// namedSessionConfigDriftDeferralStillBinding reports whether a named session's
// active-use rung is still a deferral. The two bounded rungs — the provider
// cannot report activity, or it reported activity recently — bind only until
// their window elapses; every other active reason binds unconditionally.
func namedSessionConfigDriftDeferralStillBinding(info sessionpkg.Info, clk clock.Clock, driftKey, reason string) bool {
	limit, bounded := namedSessionConfigDriftDeferralWindow(reason)
	if !bounded || clk == nil {
		return true
	}
	deferredAt, started := configDriftDeferralWindowStart(info, driftKey)
	if !started {
		return true
	}
	return clk.Now().UTC().Sub(deferredAt) < limit
}

// namedSessionConfigDriftDeferralWindow returns the bounded window a named
// active-use reason is deferred for, and whether that reason is bounded at all.
func namedSessionConfigDriftDeferralWindow(reason string) (time.Duration, bool) {
	switch reason {
	case "activity_unknown":
		return namedSessionActivityThreshold, true
	case "recent_activity":
		return namedSessionRecentActivityConfigDriftDeferralLimit, true
	}
	return 0, false
}

// namedSessionConfigDriftDeferralNeedsStamp answers legacy's own write condition
// for the named deferral window, rung by rung. Only three of the named reasons
// stamp: the two bounded ones — where the stamp IS the window that will later
// retire them — and the pinned one, which binds forever but still records the
// drift so it stays observable rather than silently ignored. The rest
// (pending_interaction, attached on a named row) bind unconditionally and legacy
// writes nothing for them, so neither does this handler.
func namedSessionConfigDriftDeferralNeedsStamp(info sessionpkg.Info, driftKey, reason string) bool {
	if reason == "pinned" {
		return info.ConfigDriftDeferredKey != driftKey || info.ConfigDriftDeferredAt == ""
	}
	if _, bounded := namedSessionConfigDriftDeferralWindow(reason); !bounded {
		return false
	}
	_, started := configDriftDeferralWindowStart(info, driftKey)
	return !started
}

// reconcileExactSessionConfigDrift resolves ONE drifted session by exact key.
// The ladder is legacy's, rung for rung and in legacy's order:
//
//	version artifact  → silent rebaseline, no restart
//	human engaged     → defer: stamp the window, cancel a queued drift drain
//	launch-only drift → relaunch the agent in the existing warm box
//	named + detached  → restart in place
//	ordinary          → begin the config-drift drain
//	live half only    → backfill, rebaseline, or re-apply session_live
//
// Every effect is an EXISTING helper called once behind a revision fence; the
// handler adds no second convergence implementation and no second deferral
// store. The row is re-read and the compare re-run before any effect, because
// the detector's reason is a hint.
//
// The two halves carry SEPARATE act constants because they crossed in separate
// slices (WD.8 converge, WD.9 defer) while riding one detected condition: the
// fact that forks them is attachment, which is provider I/O the sweep may not
// pay. Each half's effects are therefore gated on its own constant, not on the
// family.
//
// The ASLEEP-named repair arm is not this family's yet: it is legacy's own
// separate site, guarded by tick-local restart state the keyed handler cannot
// see, so a row whose runtime is not alive is refused here with zero effect.
func reconcileExactSessionConfigDrift(
	ctx context.Context,
	admission sessionStartAdmission,
	params exactSessionStartParams,
	info sessionpkg.Info,
	response sessionpkg.PersistedResponse,
	clk clock.Clock,
) (exactSessionStartOwner, error) {
	if clk == nil {
		clk = clock.Real{}
	}
	stderr := params.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	stdout := params.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	yieldOrPark := func(cause error) (exactSessionStartOwner, error) {
		if params.RolloutMode == rollout.Auto {
			return exactSessionStartLegacyOwner, fmt.Errorf("%w: %w", errSessionStartLegacyFallbackRequired, cause)
		}
		return exactSessionStartKeyedOwner, cause
	}
	if !detectorActDriftConverge && !detectorActDriftDefer {
		return exactSessionStartKeyedOwner, nil
	}

	// The fence: re-read the authoritative row and refuse unless it is still the
	// exact incarnation the condition was detected on.
	latest, latestResponse, readErr := getAuthoritativeSessionStartPersistedRecord(params.Store, info.ID)
	if readErr != nil || latestResponse.Revision == 0 || latestResponse.Revision != response.Revision ||
		latest.Closed || strings.TrimSpace(latest.InstanceToken) != strings.TrimSpace(info.InstanceToken) ||
		strings.TrimSpace(latest.SessionNameMetadata) != strings.TrimSpace(info.SessionNameMetadata) {
		return exactSessionStartKeyedOwner, nil
	}
	drift, drifted := resolveExactSessionConfigDrift(params, latest, clk)
	if !drifted {
		// Converged between detection and dispatch. Zero effect, and no trace
		// noise: the condition simply no longer holds.
		return exactSessionStartKeyedOwner, nil
	}
	name := drift.SessionName

	// Proven presence, not assumed presence. Every convergence effect below acts
	// on a RUNNING agent — relaunch it, kill and reset it, drain it, re-apply its
	// live config — so the observation is paid per key and fails CLOSED: an
	// unreadable provider is not "alive", and the condition is re-detected.
	running, livenessErr := workerSessionTargetRunningWithConfig(params.CityPath, params.Store, params.Provider, params.Config, latest.ID)
	if livenessErr != nil {
		recordExactSessionConfigDriftTrace(params, admission, latest, drift, TraceOutcomeSkippedLivenessError, false, map[string]any{
			"liveness_error": livenessErr.Error(),
		})
		return exactSessionStartKeyedOwner, nil
	}
	if !running {
		// Legacy's alive lane is the only drift lane this family owns. The
		// asleep-named repair stays legacy's for the WD wave.
		recordExactSessionConfigDriftTrace(params, admission, latest, drift, TraceOutcomeNoChange, false, map[string]any{
			"refusal": "runtime_not_alive",
		})
		return exactSessionStartKeyedOwner, nil
	}

	// A version artifact is not real drift: rebaseline all four fingerprints
	// rather than disturbing the agent. It sits above the deferral rungs because
	// it is a silent metadata write, exactly as in legacy — and legacy `continue`s
	// after it, so a version-artifact row never reaches a deferral rung at all.
	if runtime.IsLegacyOrMismatchedVersion(drift.StoredHash) {
		if !detectorActDriftConverge {
			return exactSessionStartKeyedOwner, nil
		}
		if ctx != nil && ctx.Err() != nil {
			return exactSessionStartKeyedOwner, nil
		}
		_, rebaseErr := silentRebaselineSessionHashes(latest.ID, sessionFrontDoor(params.Store), drift.AgentCfg)
		if rebaseErr != nil {
			return exactSessionStartKeyedOwner, fmt.Errorf("rebaselining legacy hash for %q: %w", name, rebaseErr)
		}
		fmt.Fprintf(stderr, "rebaselined legacy hash for %s (stored=%s current=%s)\n", name, truncateHashForLog(drift.StoredHash), truncateHashForLog(drift.CurrentHash)) //nolint:errcheck
		recordExactSessionConfigDriftTrace(params, admission, latest, drift, rebaselineLegacyHashOutcome(drift.StoredHash), true, nil)
		return exactSessionStartKeyedOwner, nil
	}

	if drift.Half == exactSessionConfigDriftLive {
		// The live lane carries no deferral rungs, exactly as legacy's live clause
		// does not: re-applying session_live neither stops nor interrupts the agent.
		if !detectorActDriftConverge {
			return exactSessionStartKeyedOwner, nil
		}
		return reconcileExactSessionLiveDriftReapply(ctx, admission, params, latest, drift, stdout)
	}

	deferral, deferErr := exactSessionConfigDriftDeferralReason(params, latest, drift, clk)
	if deferErr != nil {
		fmt.Fprintf(stderr, "session reconciler: %v\n", deferErr) //nolint:errcheck
		return exactSessionStartKeyedOwner, nil
	}
	if deferral.Rung != "" {
		if !detectorActDriftDefer {
			return exactSessionStartKeyedOwner, nil
		}
		return applyExactSessionConfigDriftDeferral(admission, params, latest, drift, deferral, clk, stdout)
	}

	if !detectorActDriftConverge {
		return exactSessionStartKeyedOwner, nil
	}
	if ctx != nil && ctx.Err() != nil {
		return exactSessionStartKeyedOwner, nil
	}
	if drift.LaunchOnly {
		// The box held and only the agent moved: relaunch into the warm box
		// rather than re-provisioning. The helper owns its own anti-skew gate,
		// its speculative-resume-key guard, and the sub-hash rebaseline; the
		// keyed record is written here so the trace carries effect_owner.
		//
		// The returned batch is the fleet tick's snapshot fold, which a keyed
		// handler has no use for: every write in it is already persisted, and the
		// next admission re-reads the authoritative row. Passing a nil trace
		// cycle is deliberate for the same reason — the helper would otherwise
		// write legacy's own payload at the same site, without effect_owner.
		relaunched, _ := relaunchAgentForLaunchDrift(ctx, params.Provider, sessionFrontDoor(params.Store), latest, name,
			drift.Template, params.CityPath, params.Config, params.Store, drift.StoredHash, drift.CurrentHash,
			drift.StoredProvisionHash, drift.StoredLaunchHash, drift.DriftedFields,
			exactSessionConfigDriftRecorder(params), nil, stdout, stderr)
		if relaunched {
			recordExactSessionConfigDriftTrace(params, admission, latest, drift, TraceOutcomeRelaunch, true, nil)
			return exactSessionStartKeyedOwner, nil
		}
		// The provider could not relaunch, or the prepared config skewed. Fall
		// through to the full restart, which is what legacy does — and what
		// re-stamps the sub-hashes so the next pass self-heals.
	}

	if drift.Named {
		if params.Store == nil {
			return yieldOrPark(errors.New("exact config-drift restart-in-place has no store to reset through"))
		}
		// The reset stages start-pending WITH the pending-create claim, which is
		// what makes an off-tick keyed restart safe: legacy protects its own
		// in-tick reset with the tick-local driftRestartedInPlace flag, but the
		// staged row also satisfies pendingResumePreservingNamedRestartInfo, so
		// the next fleet pass's asleep-named repair leaves the preserved
		// session_key and baseline alone and simply starts the session.
		batch := resetConfiguredNamedSessionForConfigDriftInfo(latest, params.Store, params.Provider, name,
			true, string(sessionpkg.StateStartPending), clk.Now().UTC(), stderr)
		if batch == nil {
			return exactSessionStartKeyedOwner, fmt.Errorf("restarting exact config-drift named session %q in place: reset was not recorded", latest.ID)
		}
		exactSessionConfigDriftRecorder(params).Record(events.Event{
			Type:      events.SessionDraining,
			Actor:     "gc",
			Subject:   drift.Template.DisplayName(),
			Message:   "config drift detected",
			SessionID: latest.ID,
		})
		recordExactSessionConfigDriftTrace(params, admission, latest, drift, TraceOutcomeRestartInPlace, true, nil)
		return exactSessionStartKeyedOwner, nil
	}

	if params.DrainTracker == nil {
		return yieldOrPark(errors.New("exact config-drift drain has no tracker to record drain intent in"))
	}
	// The drain library, not a second drain engine, with its enqueue-only begin
	// semantics preserved: the interrupt is deferred to the next advance, which
	// is what gives a session one full pass to be rescued.
	if !beginSessionDrainInfo(latest, params.Provider, params.DrainTracker, "config-drift", clk, exactSessionConfigDriftDrainTimeout(params.Config)) {
		// A drain is already in flight for this key; advancing it is D-DRAIN's.
		recordExactSessionConfigDriftTrace(params, admission, latest, drift, TraceOutcomeNoChange, false, nil)
		return exactSessionStartKeyedOwner, nil
	}
	state := params.DrainTracker.get(latest.ID)
	if state == nil || state.reason != "config-drift" {
		return exactSessionStartKeyedOwner, fmt.Errorf(
			"beginning exact config-drift drain of %q: drain intent is absent after begin", latest.ID)
	}
	fmt.Fprintf(stdout, "Draining session '%s': config-drift\n", name) //nolint:errcheck
	exactSessionConfigDriftRecorder(params).Record(events.Event{
		Type:      events.SessionDraining,
		Actor:     "gc",
		Subject:   drift.Template.DisplayName(),
		Message:   "config drift detected",
		SessionID: latest.ID,
	})
	recordExactSessionConfigDriftTrace(params, admission, latest, drift, TraceOutcomeDrain, true, map[string]any{
		"drain_timeout_seconds": int64(state.deadline.Sub(state.startedAt).Seconds()),
	})
	return exactSessionStartKeyedOwner, nil
}

// applyExactSessionConfigDriftDeferral is the A6 half of the ladder by exact
// key: the session stays exactly where the human left it, and the only writes
// are the ones that keep it there. Each rung applies precisely what legacy's own
// arm applies and nothing more —
//
//	attached           → refresh the attached window, cancel a queued drift drain
//	attached_recently  → nothing; the window legacy or this handler already wrote
//	                     is what is holding the session, and re-stamping it would
//	                     extend the false-negative guard indefinitely
//	named + active     → start the bounded window for the rungs that expire
//	pending_interaction→ cancel a pending-cancelable drain; a user is waiting
//	live_assigned_work → nothing; draining would orphan the assigned bead
//
// — because the deferral records are legacy's own metadata keys and the cancels
// are legacy's own drain-library helpers. There is no new store and no new
// marker: the durable state a deferral leaves behind is byte-identical to what
// the fleet pass leaves behind, so either writer's stamp is read by both.
//
// A failed stamp is an ERROR, not a swallowed warning: the window it starts is
// what retires the bounded rungs and what defends against a transient IsAttached
// false negative, so a deferral that silently failed to persist would look like
// a converged row on the next pass.
func applyExactSessionConfigDriftDeferral(
	admission sessionStartAdmission,
	params exactSessionStartParams,
	info sessionpkg.Info,
	drift exactSessionConfigDrift,
	deferral exactSessionConfigDriftDeferral,
	clk clock.Clock,
	stdout io.Writer,
) (exactSessionStartOwner, error) {
	name := drift.SessionName
	stamped := false
	canceled := false
	switch deferral.Rung {
	case driftDeferralAttached:
		if err := recordSessionAttachedConfigDriftDeferral(info, sessionFrontDoor(params.Store), clk, drift.DriftKey); err != nil {
			return exactSessionStartKeyedOwner, fmt.Errorf("recording attached config-drift deferral for %q: %w", name, err)
		}
		stamped = true
		canceled = cancelSessionConfigDriftDrainInfo(info, params.Provider, params.DrainTracker)
	case driftDeferralNamedActive:
		if namedSessionConfigDriftDeferralNeedsStamp(info, drift.DriftKey, deferral.Reason) {
			if err := recordNamedSessionConfigDriftDeferredAt(info, sessionFrontDoor(params.Store), clk.Now().UTC(), drift.DriftKey); err != nil {
				return exactSessionStartKeyedOwner, fmt.Errorf("recording named config-drift deferral for %q: %w", name, err)
			}
			stamped = true
		}
	case driftDeferralPendingInteraction:
		if params.DrainTracker != nil {
			canceled = cancelSessionDrainForPendingInfo(info, params.Provider, params.DrainTracker)
		}
	case driftDeferralAssignedWork:
		fmt.Fprintf(stdout, "Skipping config-drift drain for '%s': live assigned work found\n", name) //nolint:errcheck
	}
	recordExactSessionConfigDriftDeferralTrace(params, admission, info, drift, deferral, stamped, canceled)
	return exactSessionStartKeyedOwner, nil
}

// reconcileExactSessionLiveDriftReapply converges the live half. It carries no
// deferral rungs on purpose: legacy's live-drift clause has none either, because
// re-applying session_live neither stops nor interrupts the agent.
func reconcileExactSessionLiveDriftReapply(
	ctx context.Context,
	admission sessionStartAdmission,
	params exactSessionStartParams,
	info sessionpkg.Info,
	drift exactSessionConfigDrift,
	stdout io.Writer,
) (exactSessionStartOwner, error) {
	if ctx != nil && ctx.Err() != nil {
		return exactSessionStartKeyedOwner, nil
	}
	livePatch := sessionpkg.MetadataPatch{
		"live_hash":         drift.CurrentLiveFinger,
		"started_live_hash": drift.CurrentLiveFinger,
	}
	// No stored hash and no live config: there is nothing to run, so stamp the
	// baseline and stop calling it drift.
	if drift.StoredHash == "" && drift.SessionLiveConfigLen == 0 {
		if err := sessionFrontDoor(params.Store).ApplyPatch(info.ID, livePatch); err != nil {
			return exactSessionStartKeyedOwner, fmt.Errorf("backfilling live hash for %q: %w", drift.SessionName, err)
		}
		recordExactSessionConfigDriftTrace(params, admission, info, drift, TraceOutcomeNoChange, true, map[string]any{
			"live_effect": "backfill",
		})
		return exactSessionStartKeyedOwner, nil
	}
	if params.Provider == nil {
		return exactSessionStartKeyedOwner, fmt.Errorf("re-applying session_live for %q: no provider", drift.SessionName)
	}
	fmt.Fprintf(stdout, "Live config changed for '%s', re-applying...\n", drift.Template.DisplayName()) //nolint:errcheck
	if err := params.Provider.RunLive(drift.SessionName, drift.AgentCfg); err != nil {
		return exactSessionStartKeyedOwner, fmt.Errorf("re-applying session_live for %q: %w", drift.SessionName, err)
	}
	if err := sessionFrontDoor(params.Store).ApplyPatch(info.ID, livePatch); err != nil {
		return exactSessionStartKeyedOwner, fmt.Errorf("rebaselining live hash for %q: %w", drift.SessionName, err)
	}
	exactSessionConfigDriftRecorder(params).Record(events.Event{
		Type:    events.SessionUpdated,
		Actor:   "gc",
		Subject: drift.Template.DisplayName(),
		Message: "session_live re-applied",
	})
	recordExactSessionConfigDriftTrace(params, admission, info, drift, TraceOutcomeSuccess, true, map[string]any{
		"live_effect": "run_live",
	})
	return exactSessionStartKeyedOwner, nil
}

// exactSessionConfigDriftDrainTimeout reads the configured drift-drain window,
// falling back to the shared default exactly as the fleet arm does.
func exactSessionConfigDriftDrainTimeout(cfg *config.City) time.Duration {
	if cfg != nil {
		if ddt := cfg.Daemon.DriftDrainTimeoutDuration(); ddt > 0 {
			return ddt
		}
	}
	return defaultDrainTimeout
}

func exactSessionConfigDriftRecorder(params exactSessionStartParams) events.Recorder {
	if params.Recorder == nil {
		return events.Discard
	}
	return params.Recorder
}

// recordExactSessionConfigDriftTrace fires the SAME legacy trace sites the fleet
// drift arms fire — ConfigDrift and LiveDrift — with effect_owner=keyed and the
// honest effect_applied, so the WD.15 parity join can put the keyed convergence
// beside legacy's on one cycle.
func recordExactSessionConfigDriftTrace(
	params exactSessionStartParams,
	admission sessionStartAdmission,
	info sessionpkg.Info,
	drift exactSessionConfigDrift,
	outcome TraceOutcomeCode,
	applied bool,
	extra map[string]any,
) {
	reason := TraceReasonConfigDrift
	if drift.Half == exactSessionConfigDriftLive {
		reason = TraceReasonLiveDrift
	}
	recordExactSessionConfigDriftRecord(params, admission, info, drift, reason, outcome, detectorKeyedEffectOwner, applied, extra)
}

// recordExactSessionConfigDriftDeferralTrace fires the SAME legacy (site,
// reason, outcome) triple the fleet's deferral arms fire, with
// effect_owner=keyed. effect_applied is true on every rung: the EFFECT of a
// deferral arm is the deferral itself — legacy stood down and the session was
// held where the human left it — and the two sub-effects that vary by rung are
// reported as their own fields, exactly as legacy reports drain_canceled.
func recordExactSessionConfigDriftDeferralTrace(
	params exactSessionStartParams,
	admission sessionStartAdmission,
	info sessionpkg.Info,
	drift exactSessionConfigDrift,
	deferral exactSessionConfigDriftDeferral,
	stamped bool,
	canceled bool,
) {
	recordExactSessionConfigDriftRecord(params, admission, info, drift,
		deferral.TraceReason, deferral.Outcome, detectorKeyedEffectOwner, true, map[string]any{
			"active_reason":    deferral.Reason,
			"deferral_rung":    string(deferral.Rung),
			"deferral_stamped": stamped,
			"drain_canceled":   canceled,
		})
}

func recordExactSessionConfigDriftRecord(
	params exactSessionStartParams,
	admission sessionStartAdmission,
	info sessionpkg.Info,
	drift exactSessionConfigDrift,
	reason TraceReasonCode,
	outcome TraceOutcomeCode,
	effectOwner string,
	applied bool,
	extra map[string]any,
) {
	if params.Trace == nil || drift.Site == "" {
		return
	}
	cycle := params.Trace.BeginCycle(TraceTickTriggerControl, "exact_session_config_drift_converge", time.Now().UTC(), params.Config)
	if cycle == nil {
		return
	}
	template := drift.Template.TemplateName
	if template == "" {
		template = normalizedSessionTemplateInfo(info, params.Config)
	}
	fields := map[string]any{
		"admission":         string(admission.Source),
		"admission_version": admission.Version,
		"generation":        params.Generation,
		"instance_token":    info.InstanceToken,
		"drift_half":        string(drift.Half),
		"stored_hash":       drift.StoredHash,
		"current_hash":      drift.CurrentHash,
		"launch_only":       drift.LaunchOnly,
		"effect_owner":      effectOwner,
		"effect_applied":    applied,
	}
	for k, v := range extra {
		fields[k] = v
	}
	cycle.recordKeyedEffect(
		drift.Site,
		reason,
		outcome,
		"exact_session_config_drift_converge",
		template,
		info.ID,
		info.SessionNameMetadata,
		0,
		fields,
	)
	if err := cycle.End(TraceCompletionCompleted, nil); err != nil && params.Stderr != nil {
		fmt.Fprintf(params.Stderr, "session reconciler: recording exact config-drift trace: %v\n", err) //nolint:errcheck // tracing is observational
	}
}
