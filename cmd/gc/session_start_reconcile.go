package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

type exactLoadedSessionObserver func(
	context.Context,
	string,
	beads.Store,
	runtime.Provider,
	*config.City,
	sessionpkg.Info,
	[]string,
) (worker.LiveObservation, error)

// exactSessionStartParams is one coherent runtime generation for exact-key
// start reconciliation. Callers must capture Generation, Config, Provider,
// and Store together before invoking reconcileExactSessionStart.
type exactSessionStartParams struct {
	Generation                         uint64
	CityPath                           string
	CityName                           string
	Config                             *config.City
	Provider                           runtime.Provider
	Store                              beads.Store
	StatusWriter                       beads.ConditionalWriter
	StatusWriterError                  error
	Clock                              clock.Clock
	Recorder                           events.Recorder
	Stdout                             io.Writer
	Stderr                             io.Writer
	ObserveLoadedSession               exactLoadedSessionObserver
	StartOptions                       []startExecutionOption
	AsyncStopTracker                   *asyncStartTracker
	AsyncStopCompletion                func(drainAckAsyncStopCompletion)
	AsyncStopQueued                    func()
	RolloutMode                        rollout.Mode
	RigStores                          map[string]beads.Store
	DrainOps                           drainOps
	DrainTracker                       *drainTracker
	Trace                              *SessionReconcilerTracer
	AuthorizePoolStart                 func(context.Context, sessionpkg.Info, routedWorkPoolStartLease) (bool, error)
	AuthorizePoolDrainAck              func(sessionpkg.Info, routedWorkPoolDrainAckLease) (bool, drainAckRefusal, error)
	RecoverPoolDrainAck                func(sessionpkg.Info) (routedWorkPoolDrainAckLease, bool, bool, error)
	ValidateWaitDependencyPoolWitness  func(sessionpkg.Info, sessionWaitDependencyStartLease) bool
	ValidateConfiguredDependencyStart  func(sessionpkg.Info, configuredDependencyStartLease) bool
	EnterConfiguredDependencyStart     func(configuredDependencyStartLease) bool
	ValidateStrictDefaultPoolWakeStart func(sessionpkg.Info, strictDefaultPoolWakeStartLease) bool
	EnterStrictDefaultPoolWakeStart    func(strictDefaultPoolWakeStartLease) bool
	ValidateConfiguredNamedWakeStart   func(sessionpkg.Info, configuredNamedWakeStartLease) bool
	EnterConfiguredNamedWakeStart      func(configuredNamedWakeStartLease) bool

	// CertifyWakeFamilyStart closes the PRE-LEASE ownership seam — WD.10a's
	// second entry gate beside Q1 (ga-ij8mh ruling 4, amendment 2).
	//
	// The losing interleave it removes: a wake write fires BeadUpdated, the
	// in-process admission lands here with no lease,
	// classifyExactSessionStartOwnership sends every pool-managed row to legacy,
	// the yield asks for a legacy fallback poke, and that poke's tick runs
	// legacy's PreWakePatch — which consumes wake_request
	// (internal/session/lifecycle_transition.go:203-207) before any certified
	// lease exists. The held-lease exclusion protects only AFTER admission, so
	// the window is held open by the classifier itself.
	//
	// The carve-out is placed HERE, at the yield, rather than inside
	// classifyExactSessionStartOwnership, for two reasons. The classifier is a
	// pure projection over (row, config, now) and certification needs the store,
	// the provider, and the controller generation. And re-classifying pool-managed
	// rows as keyed would ROUTE them past the socket handler's certification arm
	// (city_runtime_session_start.go, which certifies only when owner != keyed)
	// into the ordinary keyed start path, which has no dependency gate. Certifying
	// at the yield reuses the read the classification already paid for, so the
	// window is closed by construction: the same read that would have surrendered
	// the row is the one that certifies it.
	//
	// Returning true means the implementation certified a wake-family lease and
	// re-admitted this exact key under it; the handler then reports keyed
	// ownership with no effect, so no fallback poke fires and the durable wake
	// cause survives for the re-admitted pass.
	CertifyWakeFamilyStart func(info sessionpkg.Info, sessionRevision int64) bool

	// The lifecycle-timer trackers the D-DEADLINE handler re-derives its
	// condition from. They are the same singletons the fleet loop uses, so the
	// keyed and legacy deadline arms can never disagree about a threshold.
	IdleTracker              idleTracker
	MaxSessionAgeTracker     maxSessionAgeTracker
	AssignedWorkDeferTracker assignedWorkDeferTracker

	// DesiredSessionNames returns the fleet's own desired-session view, the same
	// one the tick hands the sweep and the god function. The D-ORPHAN close
	// handler re-derives undesiredness from it per key: undesiredness is
	// fleet-shaped (pool counts, named specs, demand), so no per-row predicate
	// can answer it, and recomputing buildDesiredState per key would turn an
	// O(1) handler into an O(fleet) one. Nil — or a view no tick has published
	// yet — fails the close closed.
	DesiredSessionNames func() map[string]bool

	// ProviderHealth returns the ADR-0013 provider-health snapshot the tick's
	// detector sweep already loaded — one file read per sweep, shared by every
	// key the sweep produced, instead of one file read per key inside each
	// gate (DETECTOR.md §3, circuit/health: the sweep is the hydration point).
	// A nil accessor, or one no tick has published yet, falls back to the
	// per-call file read so a controller-free entry point keeps today's
	// behavior.
	ProviderHealth func() *providerHealthSnapshot

	// SessionLiveness returns the two-bit provider observation the tick's
	// detector sweep already made over the bead-awake fleet, keyed by bead ID.
	// The keyed D-ZOMBIE guard consults it instead of probing: that family's
	// whole condition is provider I/O, so without a fleet view every admission
	// on a healthy awake row would pay a probe only to be declined. It is a
	// scheduling filter, never authority — the handler re-observes before it
	// writes. A nil accessor, or a view no tick has published yet, declines the
	// family rather than probing.
	SessionLiveness func() map[string]detectorLivenessBits

	// SessionWakeEvaluations returns the wake verdicts the tick's detector sweep
	// derived for the whole fleet, keyed by bead ID — the same
	// awakeSetToWakeEvals projection the fleet drain scan reads.
	//
	// The keyed D-DRAIN advance needs it for its third cancel arm, which cancels
	// a cancelable drain on a session that has reacquired ANY reason to be awake
	// (ga-f7v2ft.179). Its two narrow siblings re-pay the probe and the store
	// query their reasons were built from, but "any wake reason" is fleet-shaped
	// in the same way DesiredSessionNames is — pool counts, named and routed
	// demand, the ready-wait set — so no per-key predicate can re-derive it, and
	// recomputing ComputeAwakeSet per key would turn an O(1) handler into an
	// O(fleet) one.
	//
	// A nil accessor, a view no tick has published yet, or a key the view does
	// not carry all decline the cancel, which is the fleet scan's own behavior
	// for a row absent from wakeEvals and leaves the remaining arms untouched.
	SessionWakeEvaluations func() map[string]wakeEvaluation
}

// exactSessionProviderHealth resolves the provider-health snapshot a keyed gate
// must answer from. The sweep's published snapshot wins; an unpublished view
// falls back to the file read the gate performed before WD.11.
func exactSessionProviderHealth(params exactSessionStartParams) *providerHealthSnapshot {
	if params.ProviderHealth != nil {
		if snap := params.ProviderHealth(); snap != nil {
			return snap
		}
	}
	return loadProviderHealthSnapshot(params.CityPath)
}

// exactSessionProviderUnavailable is the keyed half of the ADR-0013 respawn
// gate: true only when the registry has a fresh entry for this provider AND
// that entry is red. An absent registry or a stale entry fails OPEN, exactly as
// the fleet arm's `!phPresent` branch does.
func exactSessionProviderUnavailable(params exactSessionStartParams, providerName string) bool {
	if strings.TrimSpace(providerName) == "" {
		return false
	}
	healthy, present := exactSessionProviderHealth(params).check(providerName)
	return present && !healthy
}

// exactSessionCircuitOpen answers the keyed start gate's respawn-breaker
// question, and it is where WD.11 moved reset persistence to.
//
// The rule is: where the in-memory MODEL knows this identity, the model is the
// authority; where it does not, the durable string is, and the gate fails
// closed on it.
//
// That split is the whole fix. The breaker's cooldown auto-reset happens in
// memory (maybeAutoReset, applied inside restoreFromMetadata and IsOpen) while
// the durable cluster keeps saying "open" until somebody writes. Before WD.11
// this gate simply OR-ed the raw persisted string into its answer and refused
// with zero writes — which was survivable only because legacy's Phase 0.5
// persisted the reset every tick. With the sweep as the hydration point, and
// with legacy gone at WE, that OR would strand a durable "open" string the
// refusing handler never clears and auto-recovery would be lost. So the gate
// converges the row onto the model — one idempotent write through
// persistSessionCircuitBreakerMetadata — BEFORE it evaluates the gate, and only
// then answers.
//
// The cold-model branch is not a fallback, it is the fail-closed direction: a
// controller that has just restarted and not yet swept has no grounds to
// believe a persisted OPEN breaker has cooled down, so it refuses and the next
// sweep's hydration converges it. Trip accounting is untouched and stays at the
// shared start-failure write (session_lifecycle_parallel.go).
func exactSessionCircuitOpen(params exactSessionStartParams, info sessionpkg.Info, now time.Time) bool {
	open, identity, resetOwed := exactSessionCircuitOpenObserved(params, info, now)
	if !resetOwed {
		return open
	}
	cb := defaultSessionCircuitBreaker()
	if err := persistSessionCircuitBreakerMetadata(sessionFrontDoor(params.Store), info.ID, cb, identity, now); err != nil && params.Stderr != nil {
		fmt.Fprintf(params.Stderr, "session reconciler: persisting exact circuit breaker reset for %q: %v\n", info.ID, err) //nolint:errcheck // best-effort stderr
	}
	return open
}

// exactSessionCircuitOpenObserved is the read-only half of the gate: it answers
// the question and reports whether the durable row owes a reset, without writing
// anything. The effect-free dependency shadow uses it directly — that plan is
// contractually side-effect-free, so it may observe the divergence but never
// converge it; the real gate above pays that debt on the next admission for the
// same key.
func exactSessionCircuitOpenObserved(
	params exactSessionStartParams,
	info sessionpkg.Info,
	now time.Time,
) (open bool, identity string, resetOwed bool) {
	durableOpen := strings.TrimSpace(info.SessionCircuitState) == sessionpkg.SessionCircuitStateOpen
	cbCfg, enabled := sessionCircuitBreakerConfigFromCity(params.Config)
	if !enabled {
		return durableOpen, "", false
	}
	identity = namedSessionIdentityInfo(info)
	if identity == "" {
		return durableOpen, "", false
	}
	cb := defaultSessionCircuitBreaker()
	cb.configure(cbCfg)
	if !cb.snapshotIdentity(identity).hadEntry {
		return durableOpen, identity, false
	}
	open = cb.IsOpen(identity, now)
	return open, identity, !open && durableOpen
}

type configuredDependencyStartLease struct {
	SessionID               string
	TargetTemplate          string
	DependencyTemplate      string
	DependencySessionID     string
	DependencySessionName   string
	DependencyInstanceToken string
	ControllerGeneration    uint64
}

// strictDefaultPoolWakeStartLease binds one explicit wake to the exact
// ordinary member identity that socket ingress certified. It carries no
// allocation authority: reconciliation may only start this durable row.
type strictDefaultPoolWakeStartLease struct {
	SessionID            string
	SessionName          string
	InstanceToken        string
	SessionRevision      int64
	PoolTarget           string
	PoolSlot             string
	TriggerBeadID        string
	TriggerBeadStoreRef  string
	ControllerGeneration uint64
}

// configuredNamedWakeStartLease binds one explicit or pinned wake to an
// existing canonical configured named session. It carries no materialization authority.
type configuredNamedWakeStartLease struct {
	SessionID            string
	SessionName          string
	InstanceToken        string
	SessionRevision      int64
	Identity             string
	Mode                 string
	Template             string
	Cause                sessionpkg.WakeCause
	ControllerGeneration uint64
}

func exactUserHoldSuspendCurrent(info sessionpkg.Info, now time.Time) bool {
	if info.Closed || info.MetadataState != string(sessionpkg.StateSuspended) ||
		strings.TrimSpace(info.SleepIntent) != "user-hold" || strings.TrimSpace(info.SessionNameMetadata) == "" ||
		strings.TrimSpace(info.InstanceToken) == "" {
		return false
	}
	heldUntil, err := time.Parse(time.RFC3339, strings.TrimSpace(info.HeldUntil))
	return err == nil && heldUntil.After(now)
}

// exactOrdinaryResetStartLease binds one committed reset handoff to the exact
// ordinary row it may restart. It carries no wake authority of its own: the
// committed handoff on that row is the only thing it proves.
type exactOrdinaryResetStartLease struct {
	SessionID        string
	SessionName      string
	ResetCommittedAt string
}

// matches reports whether the durable row is still the one whose reset this
// lease committed. The pre-wake patch consumes continuation_reset_pending, so
// only the committed timestamp survives to the provider-entry recheck.
func (l exactOrdinaryResetStartLease) matches(info sessionpkg.Info) bool {
	return !info.Closed && info.ID == l.SessionID &&
		strings.TrimSpace(info.SessionNameMetadata) == l.SessionName &&
		strings.TrimSpace(info.ResetCommittedAt) == l.ResetCommittedAt
}

// pending is matches before the pre-wake patch, where the row still owes the
// start its committed reset requested.
func (l exactOrdinaryResetStartLease) pending(info sessionpkg.Info) bool {
	return l.matches(info) && strings.TrimSpace(info.ContinuationResetPending) == "true"
}

// exactOrdinaryResetRequested reports the durable marker pair that a public
// session reset persists before any runtime effect happens.
func exactOrdinaryResetRequested(info sessionpkg.Info) bool {
	return strings.TrimSpace(info.RestartRequested) == "true" &&
		strings.TrimSpace(info.ContinuationResetPending) == "true"
}

// exactOrdinaryResetCommitted reports the durable handoff RestartRequestPatch
// leaves behind: the requested marker is consumed and the row still owes the
// fresh start that clears the reset markers.
func exactOrdinaryResetCommitted(info sessionpkg.Info) bool {
	_, _, committed := resetPendingCommittedAtInfo(info)
	return committed && strings.TrimSpace(info.RestartRequested) != "true"
}

// exactOrdinaryResetAuthorityMatches reports whether a reread still carries the
// exact identity the reset was admitted against.
func exactOrdinaryResetAuthorityMatches(latest, expected sessionpkg.Info) bool {
	return !latest.Closed && latest.ID == expected.ID &&
		strings.TrimSpace(latest.SessionNameMetadata) == strings.TrimSpace(expected.SessionNameMetadata) &&
		strings.TrimSpace(latest.InstanceToken) == strings.TrimSpace(expected.InstanceToken) &&
		strings.TrimSpace(latest.Generation) == strings.TrimSpace(expected.Generation)
}

// exactOrdinaryResetCurrent reports whether one live ordinary row carries a
// reset the keyed lane owns end to end — either the marker pair a public reset
// just persisted or the committed handoff a stopped incarnation still owes a
// start. Named canonicalization, pool capacity, and dependency waves remain
// fleet projections, so those rows stay legacy-owned, as do held, quarantined,
// and terminal ones.
func exactOrdinaryResetCurrent(info sessionpkg.Info, cfg *config.City, now time.Time) bool {
	if info.Closed || strings.TrimSpace(info.SessionNameMetadata) == "" ||
		strings.TrimSpace(info.InstanceToken) == "" || strings.TrimSpace(info.Generation) == "" {
		return false
	}
	if !exactOrdinaryResetRequested(info) && !exactOrdinaryResetCommitted(info) {
		return false
	}
	if isNamedSessionInfo(info) || isPoolManagedSessionInfo(info) || info.DependencyOnly {
		return false
	}
	switch sessionpkg.State(strings.TrimSpace(info.MetadataState)) {
	case sessionpkg.StateActive, sessionpkg.StateAwake:
	default:
		return false
	}
	cfgAgent := findAgentByTemplate(cfg, resolvedSessionTemplateInfo(info, cfg))
	if cfgAgent == nil || len(cfgAgent.DependsOn) > 0 {
		return false
	}
	lifecycleInput := sessionpkg.LifecycleInputFromInfo(info)
	lifecycleInput.Now = now
	lifecycleInput.CreatedAt = info.CreatedAt
	lifecycleInput.StaleCreatingAfter = staleCreatingStateTimeout
	lifecycle := sessionpkg.ProjectLifecycle(lifecycleInput)
	return !lifecycle.Terminal &&
		!lifecycle.HasBlocker(sessionpkg.BlockerHeld) &&
		!lifecycle.HasBlocker(sessionpkg.BlockerQuarantined)
}

// commitExactOrdinaryResetHandoff completes the durable half of one reset on
// the exact key for the ORDINARY reset family (ga-f7v2ft.103). Its pre-stop
// authority is exactOrdinaryResetCurrent, which is that family's OWNERSHIP
// predicate: named, pool-managed and dependency-bearing rows stay legacy's for
// a public reset.
func commitExactOrdinaryResetHandoff(
	params exactSessionStartParams,
	info sessionpkg.Info,
	initialResponse sessionpkg.PersistedResponse,
	tp TemplateParams,
	clk clock.Clock,
	stderr io.Writer,
) (sessionpkg.Info, sessionpkg.PersistedResponse, error) {
	return commitExactSessionResetHandoff(params, info, initialResponse, tp, clk, stderr, func(latest sessionpkg.Info) bool {
		return exactOrdinaryResetCurrent(latest, params.Config, clk.Now().UTC())
	})
}

// commitExactSessionResetHandoff is the shared reset machinery: it stops the
// live incarnation under its own instance token, confirms the death, and
// commits the existing restart handoff so the start that follows runs a fresh
// conversation on the same bead and name. It rereads the durable authority
// immediately before the stop and again before the write, and returns the
// committed row the start must authorize against.
//
// `authority` is the caller's own pre-stop authority — "does this row still
// qualify for the reset it was admitted for". It is a parameter because the
// question is family-scoped, not universal: the ordinary reset family asks
// about its ownership lattice, while D-STALL (WD.12) targets exactly the named
// and pool rows that lattice excludes and asks instead whether the row still
// owes the reset its handler just persisted. Everything below the authority —
// the D2 capability pair, the token-bound stop, the death confirmation, the
// revision fence, and the RestartRequestPatch commit — is shared verbatim, so
// there is only ever one recycle implementation.
func commitExactSessionResetHandoff(
	params exactSessionStartParams,
	info sessionpkg.Info,
	initialResponse sessionpkg.PersistedResponse,
	tp TemplateParams,
	clk clock.Clock,
	stderr io.Writer,
	authority func(sessionpkg.Info) bool,
) (sessionpkg.Info, sessionpkg.PersistedResponse, error) {
	if params.DrainTracker != nil && params.DrainTracker.get(info.ID) != nil {
		return info, initialResponse, errors.New("exact reset session has an active legacy drain")
	}
	if _, ok := params.Provider.(runtime.FreshLivenessObserver); !ok {
		return info, initialResponse, errors.New("exact reset session provider cannot prove fresh liveness")
	}
	if _, ok := params.Provider.(runtime.UnattendedSessionStopper); !ok {
		return info, initialResponse, errors.New("exact reset session provider cannot prove unattended stop")
	}
	processNames := drainAckStopPendingProcessNames(params.Config, info)
	incarnationStartedAt := drainAckIncarnationStartedAt(info)
	liveness := runtime.ObserveFreshLiveness(params.Provider, runtime.LivenessTarget{
		SessionID:            info.ID,
		SessionName:          info.SessionNameMetadata,
		ProcessNames:         processNames,
		IncarnationStartedAt: incarnationStartedAt,
	})
	// Scan completeness proves ABSENCE; a positive observation is decisive on its
	// own. The recycle's stop is destructive BY INTENT — a reset exists to kill
	// the live incarnation so the restart runs a fresh conversation — and a live
	// pane withholds the tmux-absence license (TmuxSessionProvenAbsent) the /proc
	// sweep needs, so gating it on Complete meant no busy-host reset ever
	// recycled anything (ga-bxa8r).
	//
	// The negative arm below keeps the demand, and this is the arm where
	// completeness genuinely earns it: skipping the stop on an UNPROVEN absence
	// would commit the restart handoff while the old incarnation may still be
	// alive, and the start that follows would put a second incarnation on the
	// same name.
	if liveness.Running || liveness.Alive {
		latest, latestResponse, readErr := getAuthoritativeSessionStartPersistedRecord(params.Store, info.ID)
		if readErr != nil {
			return info, initialResponse, fmt.Errorf("re-reading exact reset session %q before stop: %w", info.ID, readErr)
		}
		if latestResponse.Revision != initialResponse.Revision || !exactOrdinaryResetAuthorityMatches(latest, info) ||
			authority == nil || !authority(latest) {
			return info, initialResponse, errors.New("exact reset authority changed before stop")
		}
		stopStartedAt := time.Now()
		if stopErr := workerStopUnattendedSessionByIDWithConfig(params.CityPath, params.Store, params.Provider, params.Config, info.ID, info.InstanceToken); stopErr != nil {
			return info, initialResponse, fmt.Errorf("stopping exact reset session %q: %w", info.ID, stopErr)
		}
		if completion := confirmDrainAckRuntimeDeadCompletion(params.CityPath, params.Store, params.Provider, params.Config,
			info.ID, info.SessionNameMetadata, info.InstanceToken, processNames, stderr, incarnationStartedAt, true); completion != drainAckAsyncStopConfirmed {
			return info, initialResponse, fmt.Errorf("confirming exact reset session %q stopped: %v", info.ID, completion)
		}
		recordExactOrdinaryResetStopTrace(params, info, time.Since(stopStartedAt))
	} else if !liveness.Complete {
		return info, initialResponse, errors.New("exact reset session liveness observation is incomplete")
	}
	if exactOrdinaryResetCommitted(info) {
		return info, initialResponse, nil
	}
	current, currentResponse, readErr := getAuthoritativeSessionStartPersistedRecord(params.Store, info.ID)
	if readErr != nil {
		return info, initialResponse, fmt.Errorf("re-reading exact reset session %q before the restart handoff: %w", info.ID, readErr)
	}
	if currentResponse.Revision != initialResponse.Revision || !exactOrdinaryResetAuthorityMatches(current, info) ||
		!exactOrdinaryResetRequested(current) {
		return info, initialResponse, errors.New("exact reset authority changed before the restart handoff")
	}
	// The named circuit-breaker clear travels with the recycle (WD.11, closing
	// WD.12 delta 9). Legacy's restart block clears the breaker for a named
	// identity between the kill and the handoff commit; .103's reset machinery —
	// which this body is — did not, so until now a named row recycled through
	// the keyed lane kept whatever breaker state it had. That is not cosmetic: a
	// deliberate recycle is exactly the intervention whose restart must not
	// count against a breaker that is one restart from tripping, and leaving the
	// count in place makes the fleet stop respawning the session the recycle
	// just asked it to restart.
	//
	// It sits BELOW the authority re-read rather than at legacy's textual
	// position, because the clear is itself a store write: above the fence its
	// own revision bump would fail the very authority check that licenses the
	// handoff. Below it the ordering legacy actually depends on still holds —
	// the breaker is clear before the restart is committed. It is a no-op for
	// .103's own arm, whose ownership lattice excludes named rows, so the shared
	// body stays behavior-identical there.
	handoffRevision := currentResponse.Revision
	if identity := namedSessionIdentityInfo(current); identity != "" {
		if err := resetSessionCircuitBreakerState(params.Store, current.ID, identity, defaultSessionCircuitBreaker()); err != nil {
			return info, initialResponse, fmt.Errorf("clearing session circuit breaker for exact reset %q: %w", current.ID, err)
		}
		// The clear is OUR OWN write on this row, so it may have moved the very
		// revision the handoff below fences on. Re-read and re-verify rather than
		// fencing on a revision we know we invalidated — a fence that its own
		// caller reliably breaks is worse than none, because it fails on exactly
		// the rows the clear ran for while still admitting every real race.
		refreshed, refreshedResponse, refreshErr := getAuthoritativeSessionStartPersistedRecord(params.Store, current.ID)
		if refreshErr != nil {
			return info, initialResponse, fmt.Errorf("re-reading exact reset session %q after the circuit-breaker clear: %w", current.ID, refreshErr)
		}
		if !exactOrdinaryResetAuthorityMatches(refreshed, info) || !exactOrdinaryResetRequested(refreshed) {
			return info, initialResponse, errors.New("exact reset authority changed during the circuit-breaker clear")
		}
		current, handoffRevision = refreshed, refreshedResponse.Revision
	}
	sessionKey, hasCapability := freshRestartSessionKeyInfo(tp, current)
	batch := sessionpkg.RestartRequestPatch(sessionKey, clk.Now().UTC())
	if hasCapability && sessionKey == "" {
		batch["session_key"] = ""
	}
	// The authority check above and this write are one decision, so they are
	// fenced as one (the ga-l1j53 P1 rule). Unfenced, the window between them
	// admits any writer — the public reset path, a legacy arm, another
	// controller — and the handoff commits a fresh-conversation restart on top
	// of a row whose reset intent, identity or token has already moved. A lost
	// fence is a REFUSAL with zero effect, never a silent overwrite: the stop
	// already landed and the durable reset markers are retained, so the next
	// admission re-derives the same handoff from the row the winner left.
	applied, writeErr := applyFencedSessionLifecyclePatch(
		sessionFrontDoor(params.Store), "exact reset handoff", current.ID, handoffRevision, batch,
	)
	if writeErr != nil {
		return info, initialResponse, fmt.Errorf("recording exact reset handoff for %q: %w", current.ID, writeErr)
	}
	if !applied {
		return info, initialResponse, errors.New("exact reset handoff was superseded before it committed")
	}
	committed, committedResponse, readErr := getAuthoritativeSessionStartPersistedRecord(params.Store, current.ID)
	if readErr != nil {
		return info, initialResponse, fmt.Errorf("re-reading exact reset session %q after the restart handoff: %w", current.ID, readErr)
	}
	if !exactOrdinaryResetCommitted(committed) {
		return info, initialResponse, errors.New("exact reset handoff did not commit a durable restart")
	}
	return committed, committedResponse, nil
}

func recordExactOrdinaryResetStopTrace(params exactSessionStartParams, info sessionpkg.Info, elapsed time.Duration) {
	if params.Trace == nil {
		return
	}
	cycle := params.Trace.BeginCycle(TraceTickTriggerControl, "exact_session_reset_stop", time.Now().UTC(), params.Config)
	if cycle == nil {
		return
	}
	template := normalizedSessionTemplateInfo(info, params.Config)
	cycle.recordKeyedEffect(
		TraceSiteLifecycleDrainAdvance,
		TraceReasonFreshCycle,
		TraceOutcomeSuccess,
		"exact_session_reset_stop",
		template,
		info.ID,
		info.SessionNameMetadata,
		elapsed,
		map[string]any{
			"generation":     params.Generation,
			"instance_token": info.InstanceToken,
			"effect_owner":   detectorKeyedEffectOwner,
			"effect_applied": true,
		},
	)
	if traceErr := cycle.End(TraceCompletionCompleted, nil); traceErr != nil && params.Stderr != nil {
		fmt.Fprintf(params.Stderr, "session reconciler: recording exact reset stop trace: %v\n", traceErr) //nolint:errcheck // tracing is observational
	}
}

func validateConfiguredNamedWakeStartLease(lease configuredNamedWakeStartLease) error {
	if lease.SessionRevision == 0 || lease.ControllerGeneration == 0 {
		return errors.New("configured named wake lease lacks revision or controller generation")
	}
	fields := []struct {
		name  string
		value string
	}{
		{name: "session ID", value: lease.SessionID},
		{name: "session name", value: lease.SessionName},
		{name: "instance token", value: lease.InstanceToken},
		{name: "identity", value: lease.Identity},
		{name: "mode", value: lease.Mode},
		{name: "template", value: lease.Template},
	}
	for _, field := range fields {
		if field.value == "" || strings.TrimSpace(field.value) != field.value {
			return fmt.Errorf("configured named wake lease has invalid %s", field.name)
		}
	}
	if lease.Mode != "always" && lease.Mode != "on_demand" {
		return errors.New("configured named wake lease has invalid mode")
	}
	if lease.Cause != sessionpkg.WakeCauseExplicit && lease.Cause != sessionpkg.WakeCausePinned {
		return errors.New("configured named wake lease has invalid cause")
	}
	return nil
}

func configuredNamedWakeIdentityMatches(info sessionpkg.Info, cfg *config.City, cityName string, lease configuredNamedWakeStartLease) bool {
	if cfg == nil || validateConfiguredNamedWakeStartLease(lease) != nil || info.ID != lease.SessionID || info.Closed ||
		info.PendingCreateClaim || info.DependencyOnly || isPoolManagedSessionInfo(info) || !isNamedSessionInfo(info) ||
		strings.TrimSpace(info.SessionOrigin) != "named" || strings.TrimSpace(info.SessionNameMetadata) != lease.SessionName ||
		namedSessionIdentityInfo(info) != lease.Identity || namedSessionModeInfo(info) != lease.Mode ||
		normalizedSessionTemplateInfo(info, cfg) != lease.Template {
		return false
	}
	spec, ok := findNamedSessionSpec(cfg, config.EffectiveCityName(cfg, cityName), lease.Identity)
	return ok && spec.Identity == lease.Identity && spec.SessionName == lease.SessionName && spec.Mode == lease.Mode &&
		namedSessionBackingTemplate(spec) == lease.Template && spec.Agent != nil && len(spec.Agent.DependsOn) == 0 &&
		!isManualSessionInfoForAgent(info, spec.Agent)
}

func configuredNamedWakeCauseCurrent(info sessionpkg.Info, cause sessionpkg.WakeCause, now time.Time) bool {
	input := sessionpkg.LifecycleInputFromInfo(info)
	input.Now = now
	input.CreatedAt = info.CreatedAt
	input.StaleCreatingAfter = staleCreatingStateTimeout
	lifecycle := sessionpkg.ProjectLifecycle(input)
	return lifecycle.HasWakeCause(cause) &&
		!lifecycle.HasWakeCause(sessionpkg.WakeCausePendingCreate) &&
		!lifecycle.HasBlocker(sessionpkg.BlockerHeld) &&
		!lifecycle.HasBlocker(sessionpkg.BlockerQuarantined) && !lifecycle.Terminal
}

func configuredNamedWakeStartMatches(info sessionpkg.Info, cfg *config.City, cityName string, lease configuredNamedWakeStartLease, now time.Time) bool {
	return configuredNamedWakeIdentityMatches(info, cfg, cityName, lease) &&
		info.MetadataState == string(sessionpkg.StateAsleep) && strings.TrimSpace(info.InstanceToken) == lease.InstanceToken &&
		configuredNamedWakeCauseCurrent(info, lease.Cause, now)
}

func configuredNamedWakeEnteredMatches(info sessionpkg.Info, cfg *config.City, cityName string, lease configuredNamedWakeStartLease, now time.Time) bool {
	if !configuredNamedWakeIdentityMatches(info, cfg, cityName, lease) ||
		info.MetadataState != string(sessionpkg.StateCreating) || strings.TrimSpace(info.InstanceToken) == "" ||
		strings.TrimSpace(info.InstanceToken) == lease.InstanceToken {
		return false
	}
	if lease.Cause == sessionpkg.WakeCausePinned && !configuredNamedWakeCauseCurrent(info, lease.Cause, now) {
		return false
	}
	input := sessionpkg.LifecycleInputFromInfo(info)
	input.Now = now
	input.CreatedAt = info.CreatedAt
	input.StaleCreatingAfter = staleCreatingStateTimeout
	lifecycle := sessionpkg.ProjectLifecycle(input)
	return !lifecycle.HasBlocker(sessionpkg.BlockerHeld) && !lifecycle.HasBlocker(sessionpkg.BlockerQuarantined) && !lifecycle.Terminal
}

func certifyConfiguredNamedWakeStartLease(
	info sessionpkg.Info,
	sessionRevision int64,
	cfg *config.City,
	cityName string,
	controllerGeneration uint64,
	now time.Time,
) (configuredNamedWakeStartLease, bool) {
	identity := namedSessionIdentityInfo(info)
	spec, ok := findNamedSessionSpec(cfg, config.EffectiveCityName(cfg, cityName), identity)
	if !ok {
		return configuredNamedWakeStartLease{}, false
	}
	cause := sessionpkg.WakeCauseExplicit
	if !configuredNamedWakeCauseCurrent(info, cause, now) {
		cause = sessionpkg.WakeCausePinned
		if !configuredNamedWakeCauseCurrent(info, cause, now) {
			return configuredNamedWakeStartLease{}, false
		}
	}
	lease := configuredNamedWakeStartLease{
		SessionID:            info.ID,
		SessionName:          strings.TrimSpace(info.SessionNameMetadata),
		InstanceToken:        strings.TrimSpace(info.InstanceToken),
		SessionRevision:      sessionRevision,
		Identity:             identity,
		Mode:                 namedSessionModeInfo(info),
		Template:             namedSessionBackingTemplate(spec),
		Cause:                cause,
		ControllerGeneration: controllerGeneration,
	}
	if !configuredNamedWakeStartMatches(info, cfg, cityName, lease, now) {
		return configuredNamedWakeStartLease{}, false
	}
	return lease, true
}

func validateStrictDefaultPoolWakeStartLease(lease strictDefaultPoolWakeStartLease) error {
	if err := validateSessionStartAdmission(lease.SessionID, sessionStartAdmissionSocket); err != nil {
		return err
	}
	if lease.SessionRevision == 0 || lease.ControllerGeneration == 0 {
		return errors.New("strict-default pool wake lease lacks revision or controller generation")
	}
	fields := []struct {
		name  string
		value string
	}{
		{name: "session name", value: lease.SessionName},
		{name: "instance token", value: lease.InstanceToken},
		{name: "pool target", value: lease.PoolTarget},
		{name: "pool slot", value: lease.PoolSlot},
		{name: "trigger bead ID", value: lease.TriggerBeadID},
		{name: "trigger bead store ref", value: lease.TriggerBeadStoreRef},
	}
	for _, field := range fields {
		if field.value == "" || strings.TrimSpace(field.value) != field.value {
			return fmt.Errorf("strict-default pool wake lease has invalid %s", field.name)
		}
	}
	slot, err := strconv.Atoi(lease.PoolSlot)
	if err != nil || slot <= 0 || strconv.Itoa(slot) != lease.PoolSlot {
		return errors.New("strict-default pool wake lease has invalid pool slot")
	}
	return nil
}

func strictDefaultPoolWakeExplicitCurrent(info sessionpkg.Info, now time.Time) bool {
	input := sessionpkg.LifecycleInputFromInfo(info)
	input.Now = now
	input.CreatedAt = info.CreatedAt
	input.StaleCreatingAfter = staleCreatingStateTimeout
	lifecycle := sessionpkg.ProjectLifecycle(input)
	return lifecycle.HasWakeCause(sessionpkg.WakeCauseExplicit) &&
		!lifecycle.HasWakeCause(sessionpkg.WakeCausePendingCreate) &&
		!lifecycle.HasBlocker(sessionpkg.BlockerHeld) &&
		!lifecycle.HasBlocker(sessionpkg.BlockerQuarantined) &&
		!lifecycle.Terminal
}

func strictDefaultPoolWakeIdentityMatches(info sessionpkg.Info, cfg *config.City, lease strictDefaultPoolWakeStartLease) bool {
	if cfg == nil || validateStrictDefaultPoolWakeStartLease(lease) != nil ||
		info.ID != lease.SessionID || info.Closed || info.PendingCreateClaim || info.DependencyOnly ||
		info.SessionOrigin != "ephemeral" ||
		!isPoolManagedSessionInfo(info) || isNamedSessionInfo(info) ||
		strings.TrimSpace(info.SessionNameMetadata) != lease.SessionName ||
		strings.TrimSpace(info.PoolSlot) != lease.PoolSlot ||
		strings.TrimSpace(info.TriggerBeadID) != lease.TriggerBeadID ||
		strings.TrimSpace(info.TriggerBeadStoreRef) != lease.TriggerBeadStoreRef {
		return false
	}
	agent := findAgentByTemplate(cfg, resolvedSessionTemplateInfo(info, cfg))
	if agent == nil || agent.QualifiedName() != lease.PoolTarget || isManualSessionInfoForAgent(info, agent) {
		return false
	}
	namedTemplates := make(map[string]struct{}, len(cfg.NamedSessions))
	for i := range cfg.NamedSessions {
		namedTemplates[cfg.NamedSessions[i].TemplateQualifiedName()] = struct{}{}
	}
	// Q1, RESOLVED (ga-f7v2ft.116): eligibility is supported() at EVERY
	// pool-family site. This predicate used to demand reason == Eligible, which
	// accepted unlimited pools ONLY, while its sibling at
	// city_runtime_wait_dependency_index.go demanded EligibleAgentCap and
	// accepted bounded pools only — two strict sites, inverted relative to each
	// other, each encoding one slice's scope into the eligibility REASON. Reason
	// narrowing makes eligibility site-dependent and silently unsatisfiable; the
	// inversion was the proof.
	//
	// The one genuinely deliberate exclusion survives with an honest name: max==1
	// IS the canonical-singleton identity
	// (config.Agent.UsesCanonicalSingletonPoolIdentity), whose rows ride the
	// configured-named and configured-dependency families, not this one. It is
	// spelled as the capacity clause it is: under supported(), max==1 already
	// implies reason==EligibleAgentCap (poolAllocationShadowPolicy's type doc),
	// so the reason half this used to carry said nothing the cap did not.
	//
	// The pool's own CAP is a separate explicit check: it lives at
	// strictDefaultPoolWakeStartWitnessCurrent, where the fleet's certified
	// membership view is reachable, because a wake CAN change the active count.
	policy := newPoolAllocationShadowPolicy(cfg, agent, namedTemplates)
	if !policy.supported() || policy.maxActiveSessions == 1 {
		return false
	}
	slot, _ := strconv.Atoi(lease.PoolSlot)
	return existingPoolSlotWithConfigInfo(cfg, agent, info) == slot &&
		info.AgentName == agent.QualifiedInstanceName(poolInstanceName(agent.Name, slot, agent)) &&
		lease.SessionName == PoolSessionName(agent.QualifiedName(), info.ID)
}

// strictDefaultPoolWakeCapacityAvailable is clause 2 of the uniform predicate
// contract: capacity is checked exactly where the action can change the ACTIVE
// count, and the session performing the action never counts against the cap it
// is re-entering. An unlimited policy (maximum < 0) passes trivially.
func strictDefaultPoolWakeCapacityAvailable(maximum, occupied int, selfOccupied bool) bool {
	if maximum < 0 {
		return true
	}
	if selfOccupied && occupied > 0 {
		occupied--
	}
	return occupied < maximum
}

func strictDefaultPoolWakeStartMatches(info sessionpkg.Info, cfg *config.City, lease strictDefaultPoolWakeStartLease, now time.Time) bool {
	return strictDefaultPoolWakeIdentityMatches(info, cfg, lease) &&
		info.MetadataState == string(sessionpkg.StateAsleep) &&
		strings.TrimSpace(info.InstanceToken) == lease.InstanceToken &&
		strictDefaultPoolWakeExplicitCurrent(info, now)
}

func strictDefaultPoolWakeEnteredMatches(info sessionpkg.Info, cfg *config.City, lease strictDefaultPoolWakeStartLease, now time.Time) bool {
	if !strictDefaultPoolWakeIdentityMatches(info, cfg, lease) ||
		info.MetadataState != string(sessionpkg.StateCreating) ||
		strings.TrimSpace(info.InstanceToken) == "" || strings.TrimSpace(info.InstanceToken) == lease.InstanceToken {
		return false
	}
	input := sessionpkg.LifecycleInputFromInfo(info)
	input.Now = now
	input.CreatedAt = info.CreatedAt
	input.StaleCreatingAfter = staleCreatingStateTimeout
	lifecycle := sessionpkg.ProjectLifecycle(input)
	return !lifecycle.HasBlocker(sessionpkg.BlockerHeld) &&
		!lifecycle.HasBlocker(sessionpkg.BlockerQuarantined) && !lifecycle.Terminal
}

func certifyStrictDefaultPoolWakeStartLease(
	info sessionpkg.Info,
	sessionRevision int64,
	cfg *config.City,
	controllerGeneration uint64,
	now time.Time,
) (strictDefaultPoolWakeStartLease, bool) {
	agent := findAgentByTemplate(cfg, resolvedSessionTemplateInfo(info, cfg))
	if agent == nil {
		return strictDefaultPoolWakeStartLease{}, false
	}
	lease := strictDefaultPoolWakeStartLease{
		SessionID:            info.ID,
		SessionName:          strings.TrimSpace(info.SessionNameMetadata),
		InstanceToken:        strings.TrimSpace(info.InstanceToken),
		SessionRevision:      sessionRevision,
		PoolTarget:           agent.QualifiedName(),
		PoolSlot:             strings.TrimSpace(info.PoolSlot),
		TriggerBeadID:        strings.TrimSpace(info.TriggerBeadID),
		TriggerBeadStoreRef:  strings.TrimSpace(info.TriggerBeadStoreRef),
		ControllerGeneration: controllerGeneration,
	}
	if !strictDefaultPoolWakeStartMatches(info, cfg, lease, now) {
		return strictDefaultPoolWakeStartLease{}, false
	}
	return lease, true
}

type retainedExactStartPreWakeStore struct {
	beads.Store
	sessionID string
	enter     func() bool
	entered   bool
}

func (s *retainedExactStartPreWakeStore) Handles() beads.StoreHandles {
	handles := beads.HandlesFor(s.Store)
	handles.Writer = s
	return handles
}

func (s *retainedExactStartPreWakeStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if err := s.Store.SetMetadataBatch(id, kvs); err != nil {
		return err
	}
	if s.entered || id != s.sessionID || kvs["state"] != string(sessionpkg.StateCreating) ||
		kvs["last_woke_at"] == "" || kvs["instance_token"] == "" {
		return nil
	}
	if s.enter == nil || !s.enter() {
		return errors.New("retained exact-start admission changed after pre-wake commit")
	}
	s.entered = true
	return nil
}

func validateConfiguredDependencyStartLease(lease configuredDependencyStartLease) error {
	if lease.SessionID == "" || strings.TrimSpace(lease.SessionID) != lease.SessionID {
		return errors.New("configured-dependency start lease has invalid session id")
	}
	if lease.TargetTemplate == "" || strings.TrimSpace(lease.TargetTemplate) != lease.TargetTemplate {
		return errors.New("configured-dependency start lease has invalid target template")
	}
	if lease.DependencyTemplate == "" || strings.TrimSpace(lease.DependencyTemplate) != lease.DependencyTemplate {
		return errors.New("configured-dependency start lease has invalid dependency template")
	}
	if lease.DependencySessionID == "" || strings.TrimSpace(lease.DependencySessionID) != lease.DependencySessionID {
		return errors.New("configured-dependency start lease has invalid dependency session id")
	}
	if lease.DependencySessionName == "" || strings.TrimSpace(lease.DependencySessionName) != lease.DependencySessionName {
		return errors.New("configured-dependency start lease has invalid dependency session name")
	}
	if lease.DependencyInstanceToken == "" || strings.TrimSpace(lease.DependencyInstanceToken) != lease.DependencyInstanceToken {
		return errors.New("configured-dependency start lease has invalid dependency instance token")
	}
	if lease.ControllerGeneration == 0 {
		return errors.New("configured-dependency start lease lacks controller generation")
	}
	return nil
}

func configuredDependencyStartTargetMatches(info sessionpkg.Info, cfg *config.City, lease configuredDependencyStartLease) bool {
	if cfg == nil || info.ID != lease.SessionID || info.Closed || info.PendingCreateClaim || info.DependencyOnly ||
		isNamedSessionInfo(info) {
		return false
	}
	target := findAgentByTemplate(cfg, resolvedSessionTemplateInfo(info, cfg))
	if target == nil || target.QualifiedName() != lease.TargetTemplate || isManualSessionInfoForAgent(info, target) || len(target.DependsOn) != 1 {
		return false
	}
	if !configuredDependencyWakeShapeMatches(info, target) {
		return false
	}
	dependency := findAgentByTemplate(cfg, target.DependsOn[0])
	return dependency != nil && dependency.QualifiedName() == lease.DependencyTemplate && !isMultiSessionCfgAgent(dependency)
}

// configuredDependencyWakeShapeMatches partitions the two keyed wake families on
// SLOT markers, not on `pool_managed` (ga-ij8mh ruling 4, amendment 1).
//
// The family was born refusing every pool-managed row, which reads as "leave the
// pool lattice alone" but is not what it does: syncSessionBeads stamps
// session_origin=ephemeral AND pool_managed=true on the row of every configured
// single-session agent within one tick (session_beads.go:1625/:1636-1637 at
// create, :1834-1839 on update), and a dependency-bearing agent is exactly that
// shape — poolAllocationShadowDependencies (pool_allocation_shadow.go:82-86)
// categorically excludes it from the strict-pool lattice, so no other family can
// own it either. The refusal therefore excluded the only shape production
// sustains, leaving "clean legacy ownership classification" as the sole outcome.
//
// The honest partition is the slot: a row carrying a pool_slot (with its trigger
// bead and PoolSessionName naming) is a strict-default pool member and stays with
// that family; a pool-managed row WITHOUT a slot is the canonical singleton
// identity (config.Agent.UsesCanonicalSingletonPoolIdentity) and belongs here.
// The origin-less legacy shape is still accepted for rows a sync has not yet
// touched.
func configuredDependencyWakeShapeMatches(info sessionpkg.Info, target *config.Agent) bool {
	if !isPoolManagedSessionInfo(info) {
		return true
	}
	return target.UsesCanonicalSingletonPoolIdentity() &&
		isCanonicalPoolManagedSessionInfoForTemplate(info, target.QualifiedName())
}

func configuredDependencyStartDependencyIdentity(
	store beads.Store,
	cfg *config.City,
	cityName, dependencyTemplate string,
) (sessionpkg.Info, bool) {
	if store == nil || cfg == nil || dependencyTemplate == "" {
		return sessionpkg.Info{}, false
	}
	sessionName := lookupSessionNameOrLegacy(store, cityName, dependencyTemplate, cfg.Workspace.SessionTemplate)
	if sessionName == "" {
		return sessionpkg.Info{}, false
	}
	candidates, err := sessionpkg.ExactMetadataSessionCandidatesInfo(store, false, map[string]string{"session_name": sessionName})
	if err != nil {
		return sessionpkg.Info{}, false
	}
	candidateID := ""
	for _, candidate := range candidates {
		if candidate.Closed || strings.TrimSpace(candidate.SessionNameMetadata) != sessionName ||
			normalizedSessionTemplateInfo(candidate, cfg) != dependencyTemplate {
			continue
		}
		if candidateID != "" {
			return sessionpkg.Info{}, false
		}
		candidateID = candidate.ID
	}
	if candidateID == "" {
		return sessionpkg.Info{}, false
	}
	current, _, err := getAuthoritativeSessionStartRecord(store, candidateID)
	if err != nil || current.Closed || strings.TrimSpace(current.SessionNameMetadata) != sessionName ||
		normalizedSessionTemplateInfo(current, cfg) != dependencyTemplate || strings.TrimSpace(current.InstanceToken) == "" {
		return sessionpkg.Info{}, false
	}
	return current, true
}

func configuredDependencyStartDependencyMatches(
	store beads.Store,
	cfg *config.City,
	cityName string,
	lease configuredDependencyStartLease,
) bool {
	if store == nil || cfg == nil ||
		lookupSessionNameOrLegacy(store, cityName, lease.DependencyTemplate, cfg.Workspace.SessionTemplate) != lease.DependencySessionName {
		return false
	}
	current, _, err := getAuthoritativeSessionStartRecord(store, lease.DependencySessionID)
	return err == nil && !current.Closed && current.ID == lease.DependencySessionID &&
		strings.TrimSpace(current.SessionNameMetadata) == lease.DependencySessionName &&
		strings.TrimSpace(current.InstanceToken) == lease.DependencyInstanceToken &&
		normalizedSessionTemplateInfo(current, cfg) == lease.DependencyTemplate
}

func configuredDependencyExplicitWakeCurrent(info sessionpkg.Info, now time.Time) bool {
	input := sessionpkg.LifecycleInputFromInfo(info)
	input.Now = now
	input.CreatedAt = info.CreatedAt
	input.StaleCreatingAfter = staleCreatingStateTimeout
	lifecycle := sessionpkg.ProjectLifecycle(input)
	return lifecycle.HasWakeCause(sessionpkg.WakeCauseExplicit) &&
		!lifecycle.HasWakeCause(sessionpkg.WakeCausePendingCreate) && !lifecycle.Terminal
}

// wakeCurrentSingletonPreservesUndesiredRow is WD.10a's sweep rule (ga-ij8mh
// ruling 4, amendment 4), in ONE spelling for every reaper.
//
// A canonical singleton row carrying a CURRENT explicit wake is not garbage: it
// is the wake family's live target. Sync stamps that shape
// (pool_managed + ephemeral origin, no slot) on every configured
// single-session agent's row within one tick, and no such agent generates pool
// demand of its own — `poolAllocationShadowDependencies` excludes the
// dependency-bearing ones outright, and the pool desired state is driven by
// assigned WORK — so the row an operator just asked to wake is, to every
// undesiredness test in the fleet, an orphan. Reaping it is how the
// born-unreachable dependency wake ended Closed=true.
//
// The guard is deliberately narrow in three ways. It requires a CURRENT wake
// cause, so a stale or already-consumed one still reaps and no row can be
// stranded by it. It does not cover slotized pool members, whose freeing is the
// pool lattice's own business. And it preserves only — it never makes a row
// desired, so nothing here creates work, starts a session, or feeds demand; the
// certified wake lease remains the only thing that can start the row.
//
// Amendment 4 named `sweepUndesiredPoolSessions`/`GCSweepSessionBeads`, which
// was the observed reaper on the pre-WD.3 evidence. Post-batch-3 the acting
// D-ORPHAN close family reaps the same row first, and legacy's sync and
// forward-pass arms reap it when neither keyed family holds the key. The rule is
// about the ROW, not about one reaper, so it is applied at each of them from
// this predicate — detection and re-derivation answering from one spelling, the
// rule WD.13 recorded.
func wakeCurrentSingletonPreservesUndesiredRow(info sessionpkg.Info, cfg *config.City, now time.Time) bool {
	if cfg == nil || info.Closed || info.DependencyOnly || isNamedSessionInfo(info) {
		return false
	}
	agent := findAgentByTemplate(cfg, resolvedSessionTemplateInfo(info, cfg))
	if agent == nil || !agent.UsesCanonicalSingletonPoolIdentity() ||
		isManualSessionInfoForAgent(info, agent) ||
		!configuredDependencyWakeShapeMatches(info, agent) {
		return false
	}
	return configuredDependencyExplicitWakeCurrent(info, now)
}

// wakeCurrentSingletonPreservesUndesiredBead is the beads.Bead mirror of
// wakeCurrentSingletonPreservesUndesiredRow, for legacy's sync close pass, which
// works over raw rows. Same three rungs, same narrowness.
func wakeCurrentSingletonPreservesUndesiredBead(b beads.Bead, cfg *config.City, now time.Time) bool {
	if cfg == nil || b.Status == "closed" || isNamedSessionBead(b) {
		return false
	}
	template := normalizedSessionTemplate(b, cfg)
	if template == "" {
		template = sessionBeadStoredTemplate(b)
	}
	agent := findAgentByTemplate(cfg, template)
	if agent == nil || !agent.UsesCanonicalSingletonPoolIdentity() || isManualSessionBeadForAgent(b, agent) {
		return false
	}
	if isPoolManagedSessionBead(b) && !isCanonicalPoolManagedSessionBeadForTemplate(b, agent.QualifiedName()) {
		return false
	}
	input := sessionpkg.LifecycleInputFromMetadata(b.Status, b.Metadata)
	input.Now = now
	input.StaleCreatingAfter = staleCreatingStateTimeout
	lifecycle := sessionpkg.ProjectLifecycle(input)
	return lifecycle.HasWakeCause(sessionpkg.WakeCauseExplicit) &&
		!lifecycle.HasWakeCause(sessionpkg.WakeCausePendingCreate) && !lifecycle.Terminal
}

func certifyConfiguredDependencyStartLease(
	info sessionpkg.Info,
	cfg *config.City,
	provider runtime.Provider,
	cityName string,
	store beads.Store,
	generation uint64,
	now time.Time,
) (configuredDependencyStartLease, bool) {
	if cfg == nil || provider == nil || store == nil || generation == 0 || !configuredDependencyExplicitWakeCurrent(info, now) {
		return configuredDependencyStartLease{}, false
	}
	target := findAgentByTemplate(cfg, resolvedSessionTemplateInfo(info, cfg))
	if target == nil || len(target.DependsOn) != 1 {
		return configuredDependencyStartLease{}, false
	}
	dependency := findAgentByTemplate(cfg, target.DependsOn[0])
	if dependency == nil {
		return configuredDependencyStartLease{}, false
	}
	dependencyInfo, identified := configuredDependencyStartDependencyIdentity(store, cfg, cityName, dependency.QualifiedName())
	if !identified {
		return configuredDependencyStartLease{}, false
	}
	lease := configuredDependencyStartLease{
		SessionID:               info.ID,
		TargetTemplate:          target.QualifiedName(),
		DependencyTemplate:      dependency.QualifiedName(),
		DependencySessionID:     dependencyInfo.ID,
		DependencySessionName:   strings.TrimSpace(dependencyInfo.SessionNameMetadata),
		DependencyInstanceToken: strings.TrimSpace(dependencyInfo.InstanceToken),
		ControllerGeneration:    generation,
	}
	if validateConfiguredDependencyStartLease(lease) != nil || !configuredDependencyStartTargetMatches(info, cfg, lease) ||
		!allDependenciesAliveForTemplateWithClock(lease.TargetTemplate, cfg, nil, provider, cityName, store, &clock.Fake{Time: now}) {
		return configuredDependencyStartLease{}, false
	}
	return lease, true
}

// sessionWaitDependencyStartLease binds one dependency-ready wait to the exact
// session row and controller generation that certified it. It is deliberately
// small: the durable wait and session rows remain the source of truth.
type sessionWaitDependencyStartLease struct {
	WaitID                 string
	SessionID              string
	DepIDs                 []string
	DepMode                string
	RegisteredEpoch        string
	WaitRevision           int64
	SessionRevision        int64
	IndexGeneration        uint64
	ControllerGeneration   uint64
	PoolTarget             string
	PoolMembershipRevision uint64
	Operation              string
}

func isCanonicalConfiguredNamedSessionForStart(info sessionpkg.Info, cfg *config.City) bool {
	identity := strings.TrimSpace(info.ConfiguredNamedIdentity)
	if !isNamedSessionInfo(info) || identity == "" || cfg == nil {
		return false
	}
	spec, ok := findNamedSessionSpec(cfg, cfg.EffectiveCityName(), identity)
	return ok && info.SessionName == spec.SessionName
}

func validateSessionWaitDependencyStartLease(lease sessionWaitDependencyStartLease) error {
	if lease.WaitID == "" || strings.TrimSpace(lease.WaitID) != lease.WaitID {
		return errors.New("dependency wait lease has invalid wait id")
	}
	if lease.SessionID == "" || strings.TrimSpace(lease.SessionID) != lease.SessionID {
		return errors.New("dependency wait lease has invalid session id")
	}
	if (lease.DepMode != "all" && lease.DepMode != "any") || len(lease.DepIDs) == 0 {
		return errors.New("dependency wait lease is outside the exact deps cohort")
	}
	for _, dependencyID := range lease.DepIDs {
		if dependencyID == "" || strings.TrimSpace(dependencyID) != dependencyID {
			return errors.New("dependency wait lease is outside the exact deps cohort")
		}
	}
	if lease.WaitRevision == 0 || lease.SessionRevision == 0 || lease.IndexGeneration == 0 || lease.ControllerGeneration == 0 {
		return errors.New("dependency wait lease lacks revision or generation provenance")
	}
	if lease.RegisteredEpoch == "" || strings.TrimSpace(lease.RegisteredEpoch) != lease.RegisteredEpoch {
		return errors.New("dependency wait lease lacks an exact registered epoch")
	}
	if lease.Operation == "" || strings.TrimSpace(lease.Operation) != lease.Operation {
		return errors.New("dependency wait lease has invalid operation")
	}
	if (lease.PoolTarget == "") != (lease.PoolMembershipRevision == 0) ||
		lease.PoolTarget != strings.TrimSpace(lease.PoolTarget) {
		return errors.New("dependency wait lease has an incomplete bounded-pool witness")
	}
	return nil
}

// certifySessionWaitDependencyStartLease rereads the two durable rows that a
// dependency-ready hint names. The producer index is only routing state; this
// certificate is the authority retained by the keyed worker before it mutates
// either row.
func certifySessionWaitDependencyStartLease(
	store beads.Store,
	target sessionWaitDependencyTarget,
	dependencies waitDependencyReader,
	cfg *config.City,
	provider runtime.Provider,
	cityName string,
	generation uint64,
	membership *poolMembershipIndex,
	now time.Time,
) (sessionWaitDependencyStartLease, exactSessionStartOwner, error) {
	if store == nil || cfg == nil || provider == nil || generation == 0 {
		return sessionWaitDependencyStartLease{}, exactSessionStartUnowned, errors.New("dependency wait start prerequisites are unavailable")
	}
	if outcome, err := validateExactSessionWaitDependencyShadow(store, target, dependencies, now); err != nil || outcome != sessionWaitDependencyEvaluationReady {
		if err != nil {
			return sessionWaitDependencyStartLease{}, exactSessionStartUnowned, err
		}
		return sessionWaitDependencyStartLease{}, exactSessionStartUnowned, nil
	}
	readStore := authoritativeSessionStartReadStore{Store: store, live: beads.HandlesFor(store).Live}
	wait, persistedWait, err := sessionFrontDoor(readStore).GetWaitPersistedResponse(target.WaitID)
	if err != nil {
		return sessionWaitDependencyStartLease{}, exactSessionStartUnowned, fmt.Errorf("reading certified dependency wait %q: %w", target.WaitID, err)
	}
	info, persistedSession, err := getAuthoritativeSessionStartPersistedRecord(store, target.SessionID)
	if err != nil {
		return sessionWaitDependencyStartLease{}, exactSessionStartUnowned, fmt.Errorf("reading certified dependency session %q: %w", target.SessionID, err)
	}
	registration, indexable, err := waitDependencyRegistrationFrom(wait)
	if err != nil {
		return sessionWaitDependencyStartLease{}, exactSessionStartUnowned, fmt.Errorf("canonicalizing certified dependency wait %q: %w", target.WaitID, err)
	}
	if wait.ID != target.WaitID || !indexable || registration.sessionID != target.SessionID || registration.depMode != target.DepMode || !slices.Equal(registration.depIDs, target.DepIDs) {
		return sessionWaitDependencyStartLease{}, exactSessionStartUnowned, nil
	}
	if info.ID != target.SessionID || info.Closed || persistedWait.Revision == 0 || persistedSession.Revision == 0 {
		return sessionWaitDependencyStartLease{}, exactSessionStartUnowned, nil
	}
	_, cfgAgent, _ := classifyExactSessionStartOwnership(info, cfg, now)
	if cfgAgent == nil {
		template := resolvedSessionTemplateInfo(info, cfg)
		cfgAgent = findAgentByTemplate(cfg, template)
	}
	if cfgAgent == nil || !waitDependencyConfiguredTemplateEligible(info, cfg, provider, cityName, store, now) || info.DependencyOnly || (isNamedSessionInfo(info) && !isCanonicalConfiguredNamedSessionForStart(info, cfg)) || wait.RegisteredEpoch == "" || info.ContinuationEpoch == "" || wait.RegisteredEpoch != info.ContinuationEpoch || target.generation == 0 {
		return sessionWaitDependencyStartLease{}, exactSessionStartLegacyOwner, nil
	}
	if info.MetadataState != string(sessionpkg.StateAsleep) || info.PendingCreateClaim ||
		info.WaitHold == "" || info.SleepIntent != string(sessionpkg.SleepReasonWaitHold) || info.SleepReason != string(sessionpkg.SleepReasonWaitHold) {
		return sessionWaitDependencyStartLease{}, exactSessionStartLegacyOwner, nil
	}
	poolTarget := ""
	poolMembershipRevision := uint64(0)
	if boundedTarget, bounded := waitDependencyBoundedPoolTarget(info, cfg); bounded {
		observation, memberIDs, exact := membership.observeMemberIDs(boundedTarget)
		if !exact || observation.revision == 0 || !observation.certified || observation.members != 1 || observation.occupied != 0 ||
			len(memberIDs) != 1 || memberIDs[0] != info.ID {
			return sessionWaitDependencyStartLease{}, exactSessionStartLegacyOwner, nil
		}
		poolTarget = boundedTarget
		poolMembershipRevision = observation.revision
	}
	lease := sessionWaitDependencyStartLease{
		WaitID:                 wait.ID,
		SessionID:              info.ID,
		DepIDs:                 append([]string(nil), registration.depIDs...),
		DepMode:                registration.depMode,
		RegisteredEpoch:        wait.RegisteredEpoch,
		WaitRevision:           persistedWait.Revision,
		SessionRevision:        persistedSession.Revision,
		IndexGeneration:        target.generation,
		ControllerGeneration:   generation,
		PoolTarget:             poolTarget,
		PoolMembershipRevision: poolMembershipRevision,
		Operation:              sessionpkg.NewInstanceToken(),
	}
	if err := validateSessionWaitDependencyStartLease(lease); err != nil {
		return sessionWaitDependencyStartLease{}, exactSessionStartUnowned, err
	}
	return lease, exactSessionStartKeyedOwner, nil
}

// waitDependencyBoundedPoolTarget names the pool a dependency-wait resume owes a
// membership witness for.
//
// Folded onto the uniform predicate contract at WD.10a (council F13) — as a
// RE-SPELLING, not a behavior change, and the distinction matters. Unlike the
// two sites the Q1 adjudication indicted, this one's `reason ==
// EligibleAgentCap` was never a scope narrowing: under `supported()` the reason
// is EligibleAgentCap if and ONLY if the policy carries a cap, so the test was
// already the contract's CAPACITY clause wearing a reason's clothes. Spelling it
// as the capacity clause makes that visible and keeps the answer identical:
// unlimited pools owe no witness because there is no cap for membership to
// witness against (clause 2's "trivially pass when unlimited"), and the
// canonical singleton is excluded by its own identity.
func waitDependencyBoundedPoolTarget(info sessionpkg.Info, cfg *config.City) (string, bool) {
	if cfg == nil || !isPoolManagedSessionInfo(info) {
		return "", false
	}
	agent := findAgentByTemplate(cfg, resolvedSessionTemplateInfo(info, cfg))
	if agent == nil {
		return "", false
	}
	namedTemplates := make(map[string]struct{}, len(cfg.NamedSessions))
	for i := range cfg.NamedSessions {
		namedTemplates[cfg.NamedSessions[i].TemplateQualifiedName()] = struct{}{}
	}
	policy := newPoolAllocationShadowPolicy(cfg, agent, namedTemplates)
	return agent.QualifiedName(), policy.supported() &&
		// Capacity clause: a witness is owed only where there is a cap to prove
		// the resume stays inside, and only above the canonical singleton, whose
		// rows ride the wake families instead. The min-floor test is a redundant
		// belt — min>0 yields reason=MinFloor, which supported() already rejects.
		policy.maxActiveSessions > 1 &&
		agent.EffectiveMinActiveSessions() == 0
}

// waitDependencyConfiguredTemplateEligible admits ordinary configured sessions
// and configured dependencies only when every dependency is a currently-live
// canonical singleton.
// poolMemberOwnsItsRuntimeName reports whether info carries a runtime session
// name derived from its OWN pool identity, and is therefore addressing its own
// box rather than one minted for a different bead.
//
// What it enforces is the ga-vcjr9 invariant: the name is a pure function of the
// configured identity and free of the bead ID. A bead-ID-scoped name mints a
// fresh runtime identity per start attempt, and since the runtime name is the
// sandbox (and pod) name, a pool whose start op keeps failing then leaks one box
// per attempt.
//
// Three spellings satisfy that invariant and all three are accepted. The two
// identity-derived ones differ only by the "-pool" step-aside, which the create
// path decides from the resolved identity's own transient-slot flag rather than
// from config, so a reader cannot re-derive which was used; both are equally
// free of the bead ID, and the sibling conjuncts have already pinned AgentName
// to the canonical instance identity. The third is the legacy bead-ID form,
// which beads created before the fix still carry — PoolSessionName survives
// upstream as the recognizer for exactly those.
func poolMemberOwnsItsRuntimeName(cfg *config.City, cfgAgent *config.Agent, info sessionpkg.Info) bool {
	name := strings.TrimSpace(info.SessionNameMetadata)
	if name == "" {
		return false
	}
	template := cfgAgent.QualifiedName()
	for _, transientSlot := range []bool{false, true} {
		if name == poolRuntimeSessionName(cfg, info.AgentName, template, transientSlot) {
			return true
		}
	}
	return name == PoolSessionName(template, info.ID)
}

func waitDependencyConfiguredTemplateEligible(
	info sessionpkg.Info,
	cfg *config.City,
	provider runtime.Provider,
	cityName string,
	store beads.Store,
	now time.Time,
) bool {
	template := resolvedSessionTemplateInfo(info, cfg)
	cfgAgent := findAgentByTemplate(cfg, template)
	if cfgAgent == nil {
		return false
	}
	if isPoolManagedSessionInfo(info) {
		poolSlot, err := strconv.Atoi(strings.TrimSpace(info.PoolSlot))
		if info.SessionOrigin != "ephemeral" || info.TriggerBeadID == "" || info.DependencyOnly ||
			isNamedSessionInfo(info) || isManualSessionInfoForAgent(info, cfgAgent) ||
			!isEphemeralSessionInfoForAgent(info, cfgAgent) || err != nil || poolSlot <= 0 ||
			existingPoolSlotWithConfigInfo(cfg, cfgAgent, info) != poolSlot ||
			info.AgentName != cfgAgent.QualifiedInstanceName(poolInstanceName(cfgAgent.Name, poolSlot, cfgAgent)) ||
			!poolMemberOwnsItsRuntimeName(cfg, cfgAgent, info) {
			return false
		}
		namedTemplates := make(map[string]struct{}, len(cfg.NamedSessions))
		for i := range cfg.NamedSessions {
			namedTemplates[cfg.NamedSessions[i].TemplateQualifiedName()] = struct{}{}
		}
		// Same contract, same re-spelling (council F13), same non-change: the
		// two-reason disjunction this replaces was eligibility AND capacity fused
		// into one expression. Split apart, eligibility is supported() and the
		// capacity clause admits an unlimited pool or a cap above the canonical
		// singleton. A future eligibility reason now reaches every pool-family
		// site at once instead of dropping out of the ones spelled by reason.
		policy := newPoolAllocationShadowPolicy(cfg, cfgAgent, namedTemplates)
		return policy.supported() &&
			(policy.maxActiveSessions < 0 || policy.maxActiveSessions > 1) &&
			cfgAgent.EffectiveMinActiveSessions() == 0
	}
	if len(cfgAgent.DependsOn) == 0 {
		return true
	}
	for _, dependencyTemplate := range cfgAgent.DependsOn {
		dependency := findAgentByTemplate(cfg, dependencyTemplate)
		if dependency == nil || isMultiSessionCfgAgent(dependency) {
			return false
		}
	}
	return allDependenciesAliveForTemplateWithClock(template, cfg, nil, provider, cityName, store, &clock.Fake{Time: now})
}

// planExactSessionWaitDependencyStartShadow reads one dependency-ready session
// and evaluates the existing start-selection planner without retaining a plan
// or performing a lifecycle effect.
func planExactSessionWaitDependencyStartShadow(
	ctx context.Context,
	sessionID string,
	params exactSessionStartParams,
) (sessionLifecycleStartSelectionPlan, error) {
	plan := sessionLifecycleStartSelectionPlan{SessionID: sessionID}
	if ctx == nil || ctx.Err() != nil {
		plan.Outcome = sessionLifecycleStartSelectionPark
		plan.Reason = sessionLifecycleStartSelectionReasonRuntimeUnknown
		return plan, nil
	}
	if sessionID == "" || strings.TrimSpace(sessionID) != sessionID {
		plan.Outcome = sessionLifecycleStartSelectionPark
		plan.Reason = sessionLifecycleStartSelectionReasonInvalidInput
		return plan, nil
	}
	if params.Config == nil || params.Provider == nil || params.Store == nil {
		plan.Outcome = sessionLifecycleStartSelectionPark
		plan.Reason = sessionLifecycleStartSelectionReasonConfigSuppressed
		return plan, nil
	}
	clk := params.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	info, _, err := getAuthoritativeSessionStartRecord(params.Store, sessionID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) || errors.Is(err, sessionpkg.ErrSessionNotFound) {
			plan.Outcome = sessionLifecycleStartSelectionNoop
			plan.Reason = sessionLifecycleStartSelectionReasonTerminal
			return plan, nil
		}
		return plan, fmt.Errorf("%w: planning exact dependency session start %q: %w", errSessionWaitDependencyTargetReadUnavailable, sessionID, err)
	}
	if info.ID != sessionID {
		return plan, fmt.Errorf("planning exact dependency session start %q: authoritative read returned %q", sessionID, info.ID)
	}
	if info.Closed {
		return planSessionLifecycleStartSelection(sessionLifecycleStartShadowInput{Info: info}), nil
	}
	template := resolvedSessionTemplateInfo(info, params.Config)
	cfgAgent := findAgentByTemplate(params.Config, template)
	if cfgAgent == nil {
		return planSessionLifecycleStartSelection(sessionLifecycleStartShadowInput{
			Info:             info,
			ConfigSuppressed: true,
		}), nil
	}
	if isAgentEffectivelySuspendedWith(params.Config, params.CityPath, cfgAgent, loadSuspensionStateBestEffort(params.CityPath)) {
		return planSessionLifecycleStartSelection(sessionLifecycleStartShadowInput{
			Info:                 info,
			WakeDecisionObserved: true,
			ShouldWake:           true,
			ConfigSuppressed:     true,
		}), nil
	}
	resolvedProvider, err := config.ResolveProvider(cfgAgent, &params.Config.Workspace, params.Config.Providers, exec.LookPath)
	if err != nil {
		return planSessionLifecycleStartSelection(sessionLifecycleStartShadowInput{
			Info:                 info,
			WakeDecisionObserved: true,
			ShouldWake:           true,
			ConfigSuppressed:     true,
		}), nil
	}
	observeLoadedSession := params.ObserveLoadedSession
	if observeLoadedSession == nil {
		observeLoadedSession = observeExactSessionWaitDependencyShadowRuntime
	}
	observation, err := observeLoadedSession(ctx, params.CityPath, params.Store, params.Provider, params.Config, info, resolvedProvider.ProcessNames)
	if err != nil {
		return planSessionLifecycleStartSelection(sessionLifecycleStartShadowInput{
			Info:                 info,
			WakeDecisionObserved: true,
			ShouldWake:           true,
		}), nil
	}
	now := clk.Now().UTC()
	// Observed, not converged: this plan is contractually effect-free, so it
	// reads the hydrated model without paying the row's reset debt. The real
	// gate settles that on the next admission for the same key.
	circuitOpen, _, _ := exactSessionCircuitOpenObserved(params, info, now)
	providerUnavailable := false
	if resolvedProvider != nil {
		providerUnavailable = exactSessionProviderUnavailable(params, resolvedProvider.Name)
	}
	return planSessionLifecycleStartSelection(sessionLifecycleStartShadowInput{
		Info:                 info,
		WakeDecisionObserved: true,
		ShouldWake:           true,
		RuntimeObserved:      true,
		RuntimeAlive:         runtimeObservationLive(observation),
		ObservedAt:           now,
		StartupTimeout:       params.Config.Session.StartupTimeoutDuration(),
		CircuitOpen:          circuitOpen,
		ProviderUnavailable:  providerUnavailable,
	}), nil
}

// observeExactSessionWaitDependencyShadowRuntime performs the one liveness
// observation needed by the effect-free dependency shadow. It deliberately
// avoids worker/session construction, which can register ACP routing.
func observeExactSessionWaitDependencyShadowRuntime(
	ctx context.Context,
	_ string,
	_ beads.Store,
	provider runtime.Provider,
	_ *config.City,
	info sessionpkg.Info,
	processNames []string,
) (worker.LiveObservation, error) {
	if ctx == nil {
		return worker.LiveObservation{}, fmt.Errorf("observing dependency shadow session: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return worker.LiveObservation{}, err
	}
	if provider == nil {
		return worker.LiveObservation{}, fmt.Errorf("observing dependency shadow session: runtime provider is nil")
	}
	liveness := runtime.ObserveLiveness(provider, info.SessionName, processNames)
	return worker.LiveObservation{
		Running: liveness.Running, Alive: liveness.Alive, SessionID: info.ID, RuntimeSessionID: info.ID, SessionName: info.SessionName,
	}, nil
}

type exactSessionStartOwner uint8

const (
	exactSessionStartUnowned exactSessionStartOwner = iota
	exactSessionStartKeyedOwner
	exactSessionStartLegacyOwner
)

type exactSessionStartPreWakeSkip struct {
	owner exactSessionStartOwner
}

var errExactPoolRecoveryAuthorityLost = errors.New("exact pool recovery authority lost")

func (e *exactSessionStartPreWakeSkip) Error() string {
	return "exact session start became ineligible before pre-wake commit"
}

// authoritativeSessionStartReadStore forces one exact-key read through the
// store's live handle. It is deliberately confined to start admission and
// commit fences: writes and ordinary reads must retain the original store so
// optional write capabilities and cache refreshes survive.
type authoritativeSessionStartReadStore struct {
	beads.Store
	live beads.LiveReader
}

func (s authoritativeSessionStartReadStore) Get(id string) (beads.Bead, error) {
	return s.live.Get(id)
}

func (s authoritativeSessionStartReadStore) IDPrefix() string {
	if prefixed, ok := s.Store.(interface{ IDPrefix() string }); ok {
		return prefixed.IDPrefix()
	}
	return ""
}

// getAuthoritativeSessionStartRecord reads one persisted session through the
// typed session front door while bypassing an eventual-consistency cache. An
// external CLI can commit a wake and send its socket hint before the matching
// event refreshes the controller cache. Its revision is from that same read;
// callers must not refresh it before a future fenced write.
func getAuthoritativeSessionStartPersistedRecord(store beads.Store, id string) (sessionpkg.Info, sessionpkg.PersistedResponse, error) {
	if store == nil {
		return sessionpkg.Info{}, sessionpkg.PersistedResponse{}, fmt.Errorf("session store is nil")
	}
	readStore := authoritativeSessionStartReadStore{
		Store: store,
		live:  beads.HandlesFor(store).Live,
	}
	info, response, err := sessionFrontDoor(readStore).GetPersistedResponse(id)
	if err != nil {
		return sessionpkg.Info{}, sessionpkg.PersistedResponse{}, err
	}
	return info, response, nil
}

func getAuthoritativeSessionStartRecord(store beads.Store, id string) (sessionpkg.Info, int64, error) {
	info, response, err := getAuthoritativeSessionStartPersistedRecord(store, id)
	if err != nil {
		return sessionpkg.Info{}, 0, err
	}
	return info, response.Revision, nil
}

var drainAckStopPendingRollbackKeys = [...]string{
	"state",
	"state_reason",
	"drain_at",
	"pending_create_claim",
	"pending_create_started_at",
	sessionpkg.DrainAckSourceMetadataKey,
	sessionpkg.DrainAckRequesterSessionIDMetadataKey,
	sessionpkg.DrainAckRequesterInstanceTokenMetadataKey,
}

// drainAckStopPendingRollback captures the exact durable values replaced
// by DrainAckStopPendingPatch and the post-CAS revision that exclusively owns
// their restoration. It intentionally derives both from PersistedResponse,
// rather than adding raw metadata mirrors to session.Info.
type drainAckStopPendingRollback struct {
	revision int64
	values   sessionpkg.MetadataPatch
}

type drainAckStopPendingFence struct {
	revision int64
	values   sessionpkg.MetadataPatch
}

func newDrainAckStopPendingFence(response sessionpkg.PersistedResponse) drainAckStopPendingFence {
	values := make(sessionpkg.MetadataPatch, len(drainAckStopPendingRollbackKeys))
	for _, key := range drainAckStopPendingRollbackKeys {
		values[key] = response.Metadata[key]
	}
	return drainAckStopPendingFence{revision: response.Revision, values: values}
}

func (f drainAckStopPendingFence) matches(info sessionpkg.Info, response sessionpkg.PersistedResponse, expectedID, expectedToken string) bool {
	if f.revision == 0 || response.Revision != f.revision || !isCanonicalDrainAckStopPendingRow(info, response, expectedID, expectedToken) {
		return false
	}
	for _, key := range drainAckStopPendingRollbackKeys {
		if response.Metadata[key] != f.values[key] {
			return false
		}
	}
	return true
}

// recordDrainAckHandbackTrace records why the keyed drain-ack fence gave a row
// back, at the same site and with the same effect_owner as every other keyed
// drain record so `gc trace` shows the refusal beside the drain it refused.
//
// It exists because a stderr-only yield is a trace lie by the program's own
// delta-8 standard: the controller log for the failing journey runs carried no
// drain-ack diagnostics at all for the drained row, so the one question the
// evidence had to answer — why does the keyed family not hold this key at the
// moment legacy sweeps it — could not be read out of a run at all.
//
// The outcome is deliberately outside legacyDrainEffectOutcomes and the record
// carries effect_owner=keyed, so the row-scoped drain purity assertion cannot
// mistake the refusal for the effect it is refusing to apply.
func recordDrainAckHandbackTrace(
	params exactSessionStartParams,
	admission sessionStartAdmission,
	info sessionpkg.Info,
	lease *routedWorkPoolDrainAckLease,
	refusal drainAckRefusal,
	cause error,
) {
	if refusal == drainAckRefusalNone {
		refusal = drainAckRefusalLeaseInvalid
	}
	extra := map[string]any{"handed_back": true}
	if cause != nil {
		extra["error"] = cause.Error()
	}
	if lease != nil {
		extra["membership_occupied"] = lease.MembershipOccupied
		extra["membership_revision"] = lease.MembershipRevision
		extra["trigger_from_ack"] = lease.TriggerFromAck
		extra["durable_agent_provenance"] = lease.DurableAgentProvenance
		extra["work_id"] = lease.WorkID
	}
	recordExactSessionDrainTrace(params, admission, info, strings.TrimSpace(info.StateReason),
		TraceSiteReconcilerDrainAck, TraceReasonCode(refusal), TraceOutcomeRejected, 0, false, extra)
}

func (f drainAckStopPendingFence) hasAgentProvenance(expectedID, expectedToken string) bool {
	return f.values[sessionpkg.DrainAckSourceMetadataKey] == sessionpkg.DrainAckSourceAgentValue &&
		f.values[sessionpkg.DrainAckRequesterSessionIDMetadataKey] == expectedID &&
		f.values[sessionpkg.DrainAckRequesterInstanceTokenMetadataKey] == expectedToken
}

func isCanonicalDrainAckStopPendingRow(info sessionpkg.Info, response sessionpkg.PersistedResponse, expectedID, expectedToken string) bool {
	if expectedID == "" || response.Revision == 0 || info.ID != expectedID || info.Closed || response.Status != "open" ||
		strings.TrimSpace(info.InstanceToken) != expectedToken || !isDrainAckStopPendingInfo(info) ||
		response.Metadata["pending_create_claim"] != "" || response.Metadata["pending_create_started_at"] != "" {
		return false
	}
	drainAt, err := time.Parse(time.RFC3339, response.Metadata["drain_at"])
	return err == nil && drainAt.UTC().Format(time.RFC3339) == response.Metadata["drain_at"]
}

func newDrainAckStopPendingRollback(response sessionpkg.PersistedResponse) drainAckStopPendingRollback {
	values := make(sessionpkg.MetadataPatch, len(drainAckStopPendingRollbackKeys))
	for _, key := range drainAckStopPendingRollbackKeys {
		values[key] = response.Metadata[key]
	}
	return drainAckStopPendingRollback{values: values}
}

func (r drainAckStopPendingRollback) matches(info sessionpkg.Info, response sessionpkg.PersistedResponse, expectedID, expectedToken string, patch sessionpkg.MetadataPatch) bool {
	if !isCanonicalDrainAckStopPendingRow(info, response, expectedID, expectedToken) {
		return false
	}
	for _, key := range drainAckStopPendingRollbackKeys {
		if response.Metadata[key] != patch[key] {
			return false
		}
	}
	return true
}

func (r drainAckStopPendingRollback) restore(writer beads.ConditionalWriter, id string) error {
	if writer == nil {
		return errors.New("drain acknowledgement conditional writer is unavailable")
	}
	if len(r.values) != len(drainAckStopPendingRollbackKeys) {
		return errors.New("drain acknowledgement rollback values are unavailable")
	}
	if err := writer.UpdateIfMatch(id, r.revision, beads.UpdateOpts{Metadata: r.values}); err != nil {
		return fmt.Errorf("restoring drain acknowledgement stop-pending transition: %w", err)
	}
	return nil
}

// sessionWaitDependencyEvaluation is the durable wait result observed by the
// effect-free dependency shadow immediately before lifecycle planning.
type sessionWaitDependencyEvaluation string

const (
	sessionWaitDependencyEvaluationReady             sessionWaitDependencyEvaluation = "ready"
	sessionWaitDependencyEvaluationPending           sessionWaitDependencyEvaluation = "pending"
	sessionWaitDependencyEvaluationStaleEpoch        sessionWaitDependencyEvaluation = "stale_epoch"
	sessionWaitDependencyEvaluationClosedSession     sessionWaitDependencyEvaluation = "closed_session"
	sessionWaitDependencyEvaluationExpired           sessionWaitDependencyEvaluation = "expired"
	sessionWaitDependencyEvaluationMissingDependency sessionWaitDependencyEvaluation = "missing_dependency"
	sessionWaitDependencyEvaluationNoopTerminal      sessionWaitDependencyEvaluation = "noop_terminal"
	sessionWaitDependencyEvaluationParkReadError     sessionWaitDependencyEvaluation = "park_read_error"
	sessionWaitDependencyEvaluationStaleTarget       sessionWaitDependencyEvaluation = "stale_target"
)

// getAuthoritativeSessionWait reads a single durable wait through the same
// live store handle used for exact lifecycle admission.
func getAuthoritativeSessionWait(store beads.Store, id string) (sessionpkg.WaitInfo, error) {
	if store == nil {
		return sessionpkg.WaitInfo{}, fmt.Errorf("session store is nil")
	}
	readStore := authoritativeSessionStartReadStore{
		Store: store,
		live:  beads.HandlesFor(store).Live,
	}
	return sessionFrontDoor(readStore).GetWait(id)
}

// validateExactSessionWaitDependencyShadow mirrors the read-only portion of
// the legacy wait ladder for one certified target. It never repairs or
// advances durable state: legacy reconciliation remains the sole mutator.
func validateExactSessionWaitDependencyShadow(
	store beads.Store,
	target sessionWaitDependencyTarget,
	dependencies waitDependencyReader,
	now time.Time,
) (sessionWaitDependencyEvaluation, error) {
	wait, err := getAuthoritativeSessionWait(store, target.WaitID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) || errors.Is(err, sessionpkg.ErrNotAWait) {
			return sessionWaitDependencyEvaluationNoopTerminal, nil
		}
		return sessionWaitDependencyEvaluationParkReadError, fmt.Errorf("%w: reading dependency wait %q: %w", errSessionWaitDependencyTargetReadUnavailable, target.WaitID, err)
	}
	if wait.Status != "open" || sessionpkg.IsWaitTerminalState(wait.State) {
		return sessionWaitDependencyEvaluationNoopTerminal, nil
	}
	if wait.State != waitStatePending {
		return sessionWaitDependencyEvaluationPending, nil
	}
	registration, indexable, err := waitDependencyRegistrationFrom(wait)
	if err != nil {
		return sessionWaitDependencyEvaluationParkReadError, fmt.Errorf("validating dependency wait %q: %w", target.WaitID, err)
	}
	if !indexable || !sameSessionWaitDependencyTarget(sessionWaitDependencyTarget{
		WaitID: target.WaitID, SessionID: registration.sessionID, DepIDs: registration.depIDs, DepMode: registration.depMode,
	}, target) {
		return sessionWaitDependencyEvaluationStaleTarget, nil
	}
	info, _, err := getAuthoritativeSessionStartRecord(store, target.SessionID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) || errors.Is(err, sessionpkg.ErrSessionNotFound) {
			return sessionWaitDependencyEvaluationNoopTerminal, nil
		}
		return sessionWaitDependencyEvaluationParkReadError, fmt.Errorf("%w: reading dependency wait session %q: %w", errSessionWaitDependencyTargetReadUnavailable, target.SessionID, err)
	}
	if wait.RegisteredEpoch != "" && info.ContinuationEpoch != "" && wait.RegisteredEpoch != info.ContinuationEpoch {
		return sessionWaitDependencyEvaluationStaleEpoch, nil
	}
	if info.Closed {
		return sessionWaitDependencyEvaluationClosedSession, nil
	}
	if wait.ExpiresAt != "" {
		expiresAt, err := time.Parse(time.RFC3339, wait.ExpiresAt)
		if err == nil && !expiresAt.After(now) {
			return sessionWaitDependencyEvaluationExpired, nil
		}
	}
	ready, err := depsWaitReadyDetailedFrom(dependencies, wait)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return sessionWaitDependencyEvaluationMissingDependency, nil
		}
		return sessionWaitDependencyEvaluationParkReadError, fmt.Errorf("%w: reading dependency wait %q dependencies: %w", errSessionWaitDependencyTargetReadUnavailable, target.WaitID, err)
	}
	if !ready {
		return sessionWaitDependencyEvaluationPending, nil
	}
	return sessionWaitDependencyEvaluationReady, nil
}

func sessionLifecycleStartSelectionTraceOutcome(outcome sessionLifecycleStartSelectionOutcome) string {
	switch outcome {
	case sessionLifecycleStartSelectionNoop:
		return "noop"
	case sessionLifecycleStartSelectionPrepare:
		return "prepare"
	case sessionLifecycleStartSelectionPark:
		return "park"
	default:
		return ""
	}
}

// getAuthoritativeExactSessionStartInfoBeforeWake returns the keyed entrant's
// authoritative pre-wake read together with the revision it was loaded at, so
// the shared pre-wake commit can fence on exactly that read.
func getAuthoritativeExactSessionStartInfoBeforeWake(
	store beads.Store,
	id string,
	cfg *config.City,
	now time.Time,
) (sessionpkg.Info, int64, error) {
	info, revision, err := getAuthoritativeSessionStartRecord(store, id)
	if err != nil {
		return sessionpkg.Info{}, 0, err
	}
	if isDrainAckStopPendingInfo(info) {
		return sessionpkg.Info{}, 0, &exactSessionStartPreWakeSkip{owner: exactSessionStartKeyedOwner}
	}
	lifecycle, _, owner := classifyExactSessionStartOwnership(info, cfg, now)
	if owner != exactSessionStartKeyedOwner {
		return sessionpkg.Info{}, 0, &exactSessionStartPreWakeSkip{owner: owner}
	}
	if lifecycle.HasBlocker(sessionpkg.BlockerHeld) || lifecycle.HasBlocker(sessionpkg.BlockerQuarantined) {
		return sessionpkg.Info{}, 0, &exactSessionStartPreWakeSkip{owner: owner}
	}
	return info, revision, nil
}

// reconcileExactSessionStart rereads one durable session key and executes only
// the pending-create and explicit-wake start family. The admission source is a
// scheduling hint; persisted lifecycle state remains authoritative.
func reconcileExactSessionStart(ctx context.Context, admission sessionStartAdmission, params exactSessionStartParams) error {
	_, err := reconcileExactSessionStartWithOwner(ctx, admission, params)
	return err
}

// reconcileExactSessionStartWithOwner returns the durable row's owner as seen
// by the same authoritative read used for reconciliation. CityRuntime uses a
// legacy result to request an immediate fleet tick, closing the race where a
// key changes ownership after socket admission.
func reconcileExactSessionStartWithOwner(
	ctx context.Context,
	admission sessionStartAdmission,
	params exactSessionStartParams,
) (exactSessionStartOwner, error) {
	if ctx == nil {
		return exactSessionStartUnowned, fmt.Errorf("reconciling exact session start %q: context is nil", admission.SessionID)
	}
	if err := ctx.Err(); err != nil {
		return exactSessionStartUnowned, err
	}
	if params.Config == nil {
		return exactSessionStartUnowned, fmt.Errorf("reconciling exact session start %q: config is nil", admission.SessionID)
	}
	if params.Provider == nil {
		return exactSessionStartUnowned, fmt.Errorf("reconciling exact session start %q: runtime provider is nil", admission.SessionID)
	}
	if params.Store == nil {
		return exactSessionStartUnowned, fmt.Errorf("reconciling exact session start %q: session store is nil", admission.SessionID)
	}
	clk := params.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	stdout := params.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	stderr := params.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	recorder := params.Recorder
	if recorder == nil {
		recorder = events.Discard
	}
	startOpts := startExecutionOptions{}
	for _, apply := range params.StartOptions {
		if apply != nil {
			apply(&startOpts)
		}
	}
	observeLoadedSession := params.ObserveLoadedSession
	if observeLoadedSession == nil {
		observeLoadedSession = workerObserveLoadedSessionWithRuntimeHintsWithConfig
	}
	var statusResult *exactSessionLifecycleStatusResult
	retainStatus := func(input exactSessionLifecycleStatusInput) {
		if startOpts.exactStatusObserver == nil && params.StatusWriter == nil && params.StatusWriterError == nil {
			return
		}
		result := evaluateExactSessionLifecycleStatus(input)
		statusResult = &result
	}
	defer func() {
		if statusResult != nil {
			reportExactSessionLifecycleStatus(stderr, startOpts.exactStatusObserver, *statusResult)
		}
	}()

	info, initialResponse, err := getAuthoritativeSessionStartPersistedRecord(params.Store, admission.SessionID)
	loadedRevision := initialResponse.Revision
	if err != nil {
		if errors.Is(err, beads.ErrIDCollision) {
			retainStatus(exactSessionLifecycleStatusInput{
				Admission:            admission,
				ControllerGeneration: params.Generation,
				RequestedID:          admission.SessionID,
				Info:                 sessionpkg.Info{ID: admission.SessionID},
				UnavailableReason:    exactSessionLifecycleStatusReasonInvalidInput,
				Error:                err.Error(),
			})
			return exactSessionStartUnowned, fmt.Errorf("reconciling exact session start %q: authoritative ID collision: %w", admission.SessionID, err)
		}
		if errors.Is(err, beads.ErrNotFound) {
			return exactSessionStartUnowned, nil
		}
		if errors.Is(err, sessionpkg.ErrSessionNotFound) {
			retainStatus(exactSessionLifecycleStatusInput{
				Admission:            admission,
				ControllerGeneration: params.Generation,
				RequestedID:          admission.SessionID,
				Info:                 sessionpkg.Info{ID: admission.SessionID},
				UnavailableReason:    exactSessionLifecycleStatusReasonInvalidInput,
				Error:                err.Error(),
			})
			return exactSessionStartUnowned, nil
		}
		return exactSessionStartUnowned, fmt.Errorf("reconciling exact session start %q: %w", admission.SessionID, err)
	}
	retainStatusFromInitialRead := func(input exactSessionLifecycleStatusInput) {
		input.Admission = admission
		input.ControllerGeneration = params.Generation
		input.RequestedID = admission.SessionID
		input.Info = info
		input.LoadedRevision = loadedRevision
		retainStatus(input)
	}
	if info.ID != admission.SessionID {
		mismatchErr := fmt.Errorf("authoritative read returned %q", info.ID)
		retainStatusFromInitialRead(exactSessionLifecycleStatusInput{
			UnavailableReason: exactSessionLifecycleStatusReasonInvalidInput,
			Error:             mismatchErr.Error(),
		})
		return exactSessionStartUnowned, fmt.Errorf("reconciling exact session start %q: %w", admission.SessionID, mismatchErr)
	}
	if info.Closed {
		retainStatusFromInitialRead(exactSessionLifecycleStatusInput{})
		return exactSessionStartUnowned, nil
	}
	// Expired hold and quarantine timers are healed HERE rather than by a
	// detector family of their own (DETECTOR.md §3, D-DUP entry): this path
	// already re-reads the authoritative row, so clearing an elapsed timer costs
	// one write on exactly the rows that have one and adds no second heal path.
	// It runs before every arm below so the suspend arm, the detector-family
	// seam, and the ordinary start path all decide against a row whose lapsed
	// timers are already gone. A current (future-dated) timer is untouched.
	if healed, healedResponse, applied := healExactSessionAdmissionTimers(params, info, initialResponse, clk); applied {
		info, initialResponse = healed, healedResponse
		loadedRevision = initialResponse.Revision
	}
	// The suspend family dispatches on the durable row, not on how the admission
	// arrived: state=suspended + sleep_intent=user-hold + a future held_until is a
	// level-triggered condition the user wrote, and the row is the authority.
	// Source-gating it to socket|antiEntropy let the controller's in_process
	// coalescing rule (a pending in_process admission keeps its source when a
	// later socket admission is folded onto the same key) silently route a user
	// suspend request into the ordinary path's held-blocker dead end, consuming
	// the admission with no stop and nothing to re-detect (ga-f7v2ft.125). Every
	// guard below — drain-tracker yield, capability checks, fresh liveness, the
	// revision reread, the token-bound stop, confirm-dead — is unchanged.
	if exactUserHoldSuspendCurrent(info, clk.Now().UTC()) && initialResponse.Revision != 0 {
		yieldOrPark := func(cause error) (exactSessionStartOwner, error) {
			if params.RolloutMode == rollout.Auto {
				return exactSessionStartLegacyOwner, fmt.Errorf("%w: %w", errSessionStartLegacyFallbackRequired, cause)
			}
			return exactSessionStartKeyedOwner, cause
		}
		if params.DrainTracker != nil && params.DrainTracker.get(info.ID) != nil {
			return yieldOrPark(errors.New("exact suspended session has an active legacy drain"))
		}
		if _, ok := params.Provider.(runtime.FreshLivenessObserver); !ok {
			return yieldOrPark(errors.New("exact suspended session provider cannot prove fresh liveness"))
		}
		if _, ok := params.Provider.(runtime.UnattendedSessionStopper); !ok {
			return yieldOrPark(errors.New("exact suspended session provider cannot prove unattended stop"))
		}
		processNames := drainAckStopPendingProcessNames(params.Config, info)
		incarnationStartedAt := drainAckIncarnationStartedAt(info)
		liveness := runtime.ObserveFreshLiveness(params.Provider, runtime.LivenessTarget{
			SessionID:            info.ID,
			SessionName:          info.SessionNameMetadata,
			ProcessNames:         processNames,
			IncarnationStartedAt: incarnationStartedAt,
		})
		// Scan completeness proves ABSENCE; a positive observation is decisive on
		// its own. Like D-DEADLINE's, this stop is destructive BY INTENT — the
		// user asked for it and the runtime is stopped precisely because it is
		// still live — so gating it on Complete demanded a proof a live target
		// can never supply: a live pane withholds the tmux-absence license
		// (TmuxSessionProvenAbsent) the /proc sweep needs, and on a busy host
		// `gc session suspend` left the runtime up with the admission consumed
		// (ga-bxa8r). Identity is fenced by the revision + instance-token + name
		// re-read below, the token-bound stop, and the COMPLETE proven-dead
		// confirm — which is satisfiable, because absence licenses the scan.
		if !liveness.Running && !liveness.Alive {
			if !liveness.Complete {
				// Dead cannot be told apart from unobserved, and the silent no-op
				// below would consume the admission and retire a suspend the row
				// still owes. Fail closed; the condition is level-triggered.
				return yieldOrPark(errors.New("exact suspended session liveness observation is incomplete"))
			}
			return exactSessionStartKeyedOwner, nil
		}
		latest, latestResponse, readErr := getAuthoritativeSessionStartPersistedRecord(params.Store, info.ID)
		if readErr != nil || latestResponse.Revision != initialResponse.Revision || !exactUserHoldSuspendCurrent(latest, clk.Now().UTC()) ||
			latest.InstanceToken != info.InstanceToken || latest.SessionNameMetadata != info.SessionNameMetadata {
			return exactSessionStartKeyedOwner, nil
		}
		if params.DrainTracker != nil && params.DrainTracker.get(info.ID) != nil {
			return yieldOrPark(errors.New("exact suspended session entered an active legacy drain before stop"))
		}
		stopStartedAt := time.Now()
		if stopErr := workerStopUnattendedSessionByIDWithConfig(params.CityPath, params.Store, params.Provider, params.Config, info.ID, info.InstanceToken); stopErr != nil {
			return exactSessionStartKeyedOwner, fmt.Errorf("stopping exact suspended session %q: %w", info.ID, stopErr)
		}
		if completion := confirmDrainAckRuntimeDeadCompletion(params.CityPath, params.Store, params.Provider, params.Config, info.ID, info.SessionNameMetadata, info.InstanceToken, processNames, stderr, incarnationStartedAt, true); completion != drainAckAsyncStopConfirmed {
			return exactSessionStartKeyedOwner, fmt.Errorf("confirming exact suspended session %q stopped: %v", info.ID, completion)
		}
		if params.Trace != nil {
			cycle := params.Trace.BeginCycle(TraceTickTriggerControl, "exact_session_suspend_stop", time.Now().UTC(), params.Config)
			if cycle != nil {
				template := normalizedSessionTemplateInfo(info, params.Config)
				cycle.recordKeyedEffect(
					TraceSiteLifecycleDrainAdvance,
					TraceReasonUserHold,
					TraceOutcomeSuccess,
					"exact_session_suspend_stop",
					template,
					info.ID,
					info.SessionNameMetadata,
					time.Since(stopStartedAt),
					map[string]any{
						"admission":         string(admission.Source),
						"admission_version": admission.Version,
						"generation":        params.Generation,
						"instance_token":    info.InstanceToken,
						"effect_owner":      detectorKeyedEffectOwner,
						"effect_applied":    true,
					},
				)
				if traceErr := cycle.End(TraceCompletionCompleted, nil); traceErr != nil && params.Stderr != nil {
					fmt.Fprintf(params.Stderr, "session reconciler: recording exact suspend stop trace: %v\n", traceErr) //nolint:errcheck // tracing is observational
				}
			}
		}
		return exactSessionStartKeyedOwner, nil
	}
	if handled, owner, familyErr := reconcileExactSessionDetectorFamily(ctx, admission, params, info, initialResponse, clk); handled {
		// Claiming the key ends the pass here, above the ordinary lane's status
		// heal, and legacy's desired-site heal is already excluded for a
		// keyed-owned row — so without this the advisory alias has no owner at
		// all for as long as a family holds the key (ga-f7v2ft.140). Only the
		// projection-neutral alias is healed, and only after a clean family
		// completion: a parked family leaves the row's disposition uncertain.
		if familyErr == nil {
			healExactSessionActiveAlias(params, admission.SessionID, clk)
		}
		return owner, familyErr
	}
	var drainAckRollback *drainAckStopPendingRollback
	var drainAckStopPendingPatch sessionpkg.MetadataPatch
	var drainAckStopPendingFence *drainAckStopPendingFence
	if isDrainAckStopPendingInfo(info) {
		fence := newDrainAckStopPendingFence(initialResponse)
		if fence.matches(info, initialResponse, info.ID, strings.TrimSpace(info.InstanceToken)) {
			drainAckStopPendingFence = &fence
		}
	}
	if admission.PoolDrainAck != nil && !isDrainAckStopPendingInfo(info) {
		transitionFailure := func(refusal drainAckRefusal, cause error) (exactSessionStartOwner, error) {
			recordDrainAckHandbackTrace(params, admission, info, admission.PoolDrainAck, refusal, cause)
			// The refusal rides the error as well as the trace. A handback that
			// only says "authorization no longer holds" costs a trace query to
			// diagnose, and the controller log is what a journey run actually
			// prints on failure.
			cause = fmt.Errorf("%w (refusal=%s)", cause, refusal)
			if params.RolloutMode == rollout.Require {
				return exactSessionStartKeyedOwner, fmt.Errorf("required exact pool drain acknowledgement refused closed: %w", cause)
			}
			return exactSessionStartLegacyOwner, fmt.Errorf("%w: %w", errSessionStartLegacyFallbackRequired, cause)
		}
		if _, ok := beads.AtomicConditionalCloserFor(params.Store); !ok {
			return transitionFailure(drainAckRefusalUnavailable, errors.New("drain acknowledgement atomic terminal closer is unavailable"))
		}
		if params.AuthorizePoolDrainAck == nil {
			return transitionFailure(drainAckRefusalUnavailable, errors.New("drain acknowledgement authorization is unavailable"))
		}
		authorized, refusal, authorizeErr := params.AuthorizePoolDrainAck(info, *admission.PoolDrainAck)
		if authorizeErr != nil {
			return transitionFailure(refusal, fmt.Errorf("drain acknowledgement authorization: %w", authorizeErr))
		}
		if !authorized {
			return transitionFailure(refusal, errors.New("drain acknowledgement authorization no longer holds"))
		}
		if params.StatusWriterError != nil {
			return transitionFailure(drainAckRefusalUnavailable, fmt.Errorf("drain acknowledgement conditional writer: %w", params.StatusWriterError))
		}
		if params.StatusWriter == nil {
			return transitionFailure(drainAckRefusalUnavailable, errors.New("drain acknowledgement conditional writer is unavailable"))
		}
		if initialResponse.Status != "open" || loadedRevision == 0 {
			return exactSessionStartKeyedOwner, fmt.Errorf("%w: drain acknowledgement initial row is not an exact open revisioned record", errSessionStartPoolDrainAckPending)
		}
		rollback := newDrainAckStopPendingRollback(initialResponse)
		patch := sessionpkg.AgentDrainAckStopPendingPatch(
			clk.Now().UTC(), admission.PoolDrainAck.RequesterSessionID, admission.PoolDrainAck.RequesterInstanceToken,
		)
		writeErr := params.StatusWriter.UpdateIfMatch(info.ID, loadedRevision, beads.UpdateOpts{Metadata: patch})
		postTransitionFailure := func(refusal drainAckRefusal, cause error) (exactSessionStartOwner, error) {
			recordDrainAckHandbackTrace(params, admission, info, admission.PoolDrainAck, refusal, cause)
			if params.RolloutMode == rollout.Require {
				return exactSessionStartKeyedOwner, fmt.Errorf("required exact pool drain acknowledgement refused closed after stop-pending transition: %w", cause)
			}
			if rollbackErr := rollback.restore(params.StatusWriter, info.ID); rollbackErr != nil {
				return exactSessionStartKeyedOwner, fmt.Errorf("reconciling exact pool drain acknowledgement %q: %w; rollback: %w", info.ID, cause, rollbackErr)
			}
			return exactSessionStartLegacyOwner, fmt.Errorf("%w: %w", errSessionStartLegacyFallbackRequired, cause)
		}
		postInfo, postResponse, readErr := getAuthoritativeSessionStartPersistedRecord(params.Store, info.ID)
		if readErr != nil {
			return exactSessionStartKeyedOwner, fmt.Errorf("%w: marking drain acknowledgement stop-pending %q; authoritative reread: %w", errSessionStartPoolDrainAckPending, info.ID, readErr)
		}
		if !rollback.matches(postInfo, postResponse, info.ID, admission.PoolDrainAck.InstanceToken, patch) {
			if writeErr != nil {
				unchanged := postInfo.ID == info.ID && !postInfo.Closed && postResponse.Status == "open" &&
					strings.TrimSpace(postInfo.InstanceToken) == admission.PoolDrainAck.InstanceToken && postResponse.Revision == loadedRevision
				for _, key := range drainAckStopPendingRollbackKeys {
					unchanged = unchanged && postResponse.Metadata[key] == initialResponse.Metadata[key]
				}
				if unchanged {
					return transitionFailure(drainAckRefusalUnavailable, fmt.Errorf("marking drain acknowledgement stop-pending: %w", writeErr))
				}
				return exactSessionStartKeyedOwner, fmt.Errorf("marking drain acknowledgement stop-pending: %w; authoritative reread does not prove unchanged or committed transition", writeErr)
			}
			return exactSessionStartKeyedOwner, fmt.Errorf("%w: stop-pending transition no longer owns the exact session row", errSessionStartPoolDrainAckPending)
		}
		rollback.revision = postResponse.Revision
		// The transition just committed the agent's stamps onto the row and
		// rollback.matches re-proved every one of them over the committed
		// revision, so from here the acknowledgement is proven DURABLY. The
		// re-authorization is told so, and stops asking the runtime — which
		// this very acknowledgement is about to stop — to prove it again.
		fence := newDrainAckStopPendingFence(postResponse)
		postLease := *admission.PoolDrainAck
		postLease.DurableAgentProvenance = fence.hasAgentProvenance(info.ID, admission.PoolDrainAck.InstanceToken)
		authorized, refusal, authorizeErr = params.AuthorizePoolDrainAck(postInfo, postLease)
		if authorizeErr != nil {
			return postTransitionFailure(refusal, fmt.Errorf("authorizing stop-pending transition: %w", authorizeErr))
		}
		if !authorized {
			return postTransitionFailure(refusal, errors.New("drain acknowledgement authorization no longer holds after stop-pending transition"))
		}
		drainAckRollback = &rollback
		drainAckStopPendingPatch = patch
		drainAckStopPendingFence = &fence
		info = postInfo
		loadedRevision = postResponse.Revision
	}
	if isDrainAckStopPendingInfo(info) {
		park := func(cause error) (exactSessionStartOwner, error) {
			return exactSessionStartKeyedOwner, fmt.Errorf("%w: %w", errSessionStartPoolDrainAckPending, cause)
		}
		if drainAckStopPendingFence == nil {
			return park(errors.New("drain acknowledgement stop-pending row is not canonical or lacks revision provenance"))
		}
		var drainAckLease *routedWorkPoolDrainAckLease
		provenanceUnrecognized := false
		recoverDrainAckLease := func() (exactSessionStartOwner, error) {
			if drainAckLease != nil {
				return exactSessionStartKeyedOwner, nil
			}
			if params.RecoverPoolDrainAck == nil {
				return park(errors.New("drain acknowledgement lease recovery is unavailable"))
			}
			recoveredLease, agentDrainAck, legacyMarker, recoverErr := params.RecoverPoolDrainAck(info)
			if recoverErr != nil {
				return park(fmt.Errorf("recovering drain acknowledgement lease: %w", recoverErr))
			}
			if !agentDrainAck && legacyMarker {
				if params.RolloutMode == rollout.Require {
					return park(errors.New("required drain acknowledgement lease recovery did not prove an agent acknowledgement"))
				}
				// The one genuinely unprovable acknowledgement: a reconciler
				// marker with no agent stamps anywhere. Auto mode still hands
				// it to legacy, but no longer silently — this is the record
				// that lets the divergence taxonomy classify the fallback
				// instead of guessing at it (council R1).
				recordDrainAckHandbackTrace(params, admission, info, nil, drainAckRefusalNotAgentStamped,
					errors.New("drain acknowledgement is a legacy marker with no agent provenance"))
				return exactSessionStartLegacyOwner, nil
			}
			if !agentDrainAck {
				// No recognizable provenance anywhere: neither agent stamps nor
				// a confirmed legacy marker. The marker is a MEANS of
				// re-validation, not an end — its absence is not a verdict, so
				// this is decided below against CURRENT authoritative state
				// under the per-key lock: a fresh COMPLETE observation proving
				// the runtime dead supersedes the lease; a live or unprovable
				// runtime keeps the protection and stays refused
				// (ga-f7v2ft.173, the post-flip drain-ack provenance
				// treadmill).
				provenanceUnrecognized = true
				return exactSessionStartKeyedOwner, nil
			}
			drainAckLease = &recoveredLease
			return exactSessionStartKeyedOwner, nil
		}
		durableAgentProvenance := drainAckStopPendingFence.hasAgentProvenance(info.ID, strings.TrimSpace(info.InstanceToken))
		if !durableAgentProvenance {
			owner, recoverErr := recoverDrainAckLease()
			if recoverErr != nil || owner != exactSessionStartKeyedOwner {
				return owner, recoverErr
			}
		}
		// A durable legacy marker is still owned by legacy reconciliation in auto
		// mode. Only agent-proven exact STOP ownership must prove it can finish
		// with the fenced terminal close before liveness observation or STOP.
		if _, ok := beads.AtomicConditionalCloserFor(params.Store); !ok {
			return park(errors.New("drain acknowledgement atomic terminal closer is unavailable"))
		}
		if _, ok := params.Provider.(runtime.FreshLivenessObserver); !ok {
			switch params.RolloutMode {
			case rollout.Auto:
				if drainAckRollback == nil {
					return park(errors.New("agent drain acknowledgement cannot prove fresh liveness"))
				}
				if rollbackErr := drainAckRollback.restore(params.StatusWriter, info.ID); rollbackErr != nil {
					return park(fmt.Errorf("restoring drain acknowledgement without fresh liveness: %w", rollbackErr))
				}
				return exactSessionStartLegacyOwner, fmt.Errorf("%w: agent drain acknowledgement cannot prove fresh liveness", errSessionStartLegacyFallbackRequired)
			case rollout.Require:
				return park(errors.New("agent drain acknowledgement cannot prove fresh liveness"))
			}
		}
		name := strings.TrimSpace(info.SessionNameMetadata)
		token := strings.TrimSpace(info.InstanceToken)
		if name == "" || token == "" {
			return park(errors.New("drain acknowledgement stop lacks exact session identity"))
		}
		processNames := drainAckStopPendingProcessNames(params.Config, info)
		incarnationStartedAt := drainAckIncarnationStartedAt(info)
		liveness := runtime.ObserveFreshLiveness(params.Provider, runtime.LivenessTarget{
			SessionID:            info.ID,
			SessionName:          name,
			ProcessNames:         processNames,
			IncarnationStartedAt: incarnationStartedAt,
		})
		if !liveness.Running && !liveness.Alive {
			if !liveness.Complete {
				return park(errors.New("drain acknowledgement liveness observation is incomplete"))
			}
			terminal := agentDrainAckTerminalProvenance(info)
			if !durableAgentProvenance {
				if !provenanceUnrecognized {
					return park(errors.New("drain acknowledgement stopped runtime lacks durable agent provenance"))
				}
				// Accept-and-supersede: the lease has no recognizable
				// provenance and the runtime it protected is proven dead by a
				// fresh COMPLETE observation, so there is no destructive stop
				// left for the provenance check to fence. The only remaining
				// effect is finalization, revision-fenced on the row this
				// cycle validated, and the recovery is stamped keyed going
				// forward — a named supersede source, never a forged
				// acknowledgement (ga-f7v2ft.173).
				terminal = supersededDrainAckTerminalProvenance()
			}
			result := finalizeDrainAckStoppedSession(
				params.CityPath, params.Config, params.Store, params.RigStores, info,
				normalizedSessionTemplateInfo(info, params.Config), isPoolManagedSessionInfo(info),
				params.DrainOps, params.DrainTracker, clk, recorder, stderr, drainAckStopPendingFence, &terminal,
			)
			if result.batch == nil && !result.closed && result.folded == nil && result.witnessInfo == nil {
				return park(fmt.Errorf("reconciling exact drain-ack stop %q: durable finalization made no progress", info.ID))
			}
			return exactSessionStartKeyedOwner, nil
		}
		if provenanceUnrecognized {
			// The control the supersede arm must never widen into: a LIVE
			// runtime whose acknowledgement cannot be proven is exactly what
			// the provenance check exists to protect. Zero STOP effects; the
			// obligation stays parked and the controller's refusal bound
			// escalates it instead of retrying forever.
			return park(errors.New("live runtime holds no recognizable drain acknowledgement provenance"))
		}
		if drainAckLease == nil && admission.PoolDrainAck != nil {
			drainAckLease = admission.PoolDrainAck
		}
		if drainAckLease == nil {
			owner, recoverErr := recoverDrainAckLease()
			if recoverErr != nil || owner != exactSessionStartKeyedOwner {
				return owner, recoverErr
			}
			if provenanceUnrecognized {
				return park(errors.New("live runtime holds no recognizable drain acknowledgement provenance"))
			}
		}
		if params.AuthorizePoolDrainAck == nil {
			return park(errors.New("drain acknowledgement authorization is unavailable"))
		}
		if !durableAgentProvenance {
			if params.StatusWriterError != nil {
				return park(fmt.Errorf("resolving drain acknowledgement provenance writer: %w", params.StatusWriterError))
			}
			if params.StatusWriter == nil {
				return park(errors.New("drain acknowledgement provenance writer is unavailable"))
			}
			authorized, refusal, authorizeErr := params.AuthorizePoolDrainAck(info, *drainAckLease)
			if authorizeErr != nil || !authorized {
				cause := errors.New("recovered drain acknowledgement authorization no longer holds before provenance write")
				if authorizeErr != nil {
					cause = fmt.Errorf("authorizing recovered drain acknowledgement before provenance write: %w", authorizeErr)
				}
				recordDrainAckHandbackTrace(params, admission, info, drainAckLease, refusal, cause)
				return park(cause)
			}
			provenance := sessionpkg.MetadataPatch{
				sessionpkg.DrainAckSourceMetadataKey:                 sessionpkg.DrainAckSourceAgentValue,
				sessionpkg.DrainAckRequesterSessionIDMetadataKey:     info.ID,
				sessionpkg.DrainAckRequesterInstanceTokenMetadataKey: info.InstanceToken,
			}
			expectedMetadata := maps.Clone(initialResponse.Metadata)
			for key, value := range provenance {
				expectedMetadata[key] = value
			}
			writeErr := params.StatusWriter.UpdateIfMatch(info.ID, drainAckStopPendingFence.revision, beads.UpdateOpts{Metadata: provenance})
			upgradedInfo, upgradedResponse, readErr := getAuthoritativeSessionStartPersistedRecord(params.Store, info.ID)
			if readErr != nil {
				return park(fmt.Errorf("re-reading recovered drain acknowledgement provenance: %w", readErr))
			}
			upgradedFence := newDrainAckStopPendingFence(upgradedResponse)
			if upgradedResponse.Revision == 0 || upgradedResponse.Revision == drainAckStopPendingFence.revision ||
				upgradedResponse.Status != initialResponse.Status || !maps.Equal(upgradedResponse.Metadata, expectedMetadata) ||
				!upgradedFence.matches(upgradedInfo, upgradedResponse, info.ID, info.InstanceToken) ||
				!upgradedFence.hasAgentProvenance(info.ID, info.InstanceToken) {
				if writeErr != nil {
					return park(fmt.Errorf("recording recovered drain acknowledgement provenance: %w", writeErr))
				}
				return park(errors.New("recovered drain acknowledgement provenance did not persist exactly"))
			}
			// Same durable-first rule as the stop-pending transition: the
			// provenance write above committed the agent stamps and
			// upgradedFence.hasAgentProvenance re-proved them, so this
			// re-authorization reads the row, not the runtime.
			upgradedLease := *drainAckLease
			upgradedLease.DurableAgentProvenance = true
			authorized, refusal, authorizeErr = params.AuthorizePoolDrainAck(upgradedInfo, upgradedLease)
			if authorizeErr != nil || !authorized {
				cause := errors.New("recovered drain acknowledgement authorization no longer holds after provenance write")
				if authorizeErr != nil {
					cause = fmt.Errorf("authorizing recovered drain acknowledgement after provenance write: %w", authorizeErr)
				}
				recordDrainAckHandbackTrace(params, admission, upgradedInfo, &upgradedLease, refusal, cause)
				return park(cause)
			}
			info = upgradedInfo
			drainAckStopPendingFence = &upgradedFence
		}
		if params.AsyncStopTracker == nil {
			return park(errors.New("drain acknowledgement async stop tracker is unavailable"))
		}
		beforeStop := func() error {
			current, response, readErr := getAuthoritativeSessionStartPersistedRecord(params.Store, info.ID)
			if readErr != nil {
				return fmt.Errorf("re-reading drain acknowledgement before stop: %w", readErr)
			}
			if !drainAckStopPendingFence.matches(current, response, info.ID, drainAckLease.InstanceToken) {
				return errors.New("drain acknowledgement stop-pending row no longer matches the admitted lease")
			}
			if drainAckRollback != nil &&
				(!drainAckRollback.matches(current, response, info.ID, drainAckLease.InstanceToken, drainAckStopPendingPatch) || response.Revision != drainAckRollback.revision) {
				return errors.New("drain acknowledgement stop-pending rollback fence no longer matches")
			}
			// By this point the row carries the agent's committed stamps and
			// the fence above has just re-proved them, so the last gate before
			// the STOP is durable too. Re-reading the runtime here was the
			// narrowest and worst of the re-derivations: it ran with the
			// acknowledged agent already on its way out.
			stopLease := *drainAckLease
			stopLease.DurableAgentProvenance = drainAckStopPendingFence.hasAgentProvenance(current.ID, strings.TrimSpace(current.InstanceToken))
			authorized, refusal, authorizeErr := params.AuthorizePoolDrainAck(current, stopLease)
			if authorizeErr == nil && authorized {
				return nil
			}
			cause := errors.New("drain acknowledgement authorization no longer holds before stop")
			if authorizeErr != nil {
				cause = fmt.Errorf("drain acknowledgement authorization before stop: %w", authorizeErr)
			}
			recordDrainAckHandbackTrace(params, admission, current, &stopLease, refusal, cause)
			if params.RolloutMode == rollout.Require || drainAckRollback == nil {
				return cause
			}
			if rollbackErr := drainAckRollback.restore(params.StatusWriter, current.ID); rollbackErr != nil {
				return fmt.Errorf("%w; rollback: %w", cause, rollbackErr)
			}
			return errDrainAckAsyncStopYielded
		}
		queued := queueExactDrainAckAsyncStop(
			params.CityPath,
			params.Store,
			params.Provider,
			params.Config,
			info.ID,
			name,
			token,
			processNames,
			incarnationStartedAt,
			params.AsyncStopTracker,
			stderr,
			beforeStop,
			func(completion drainAckAsyncStopCompletion) {
				if params.AsyncStopCompletion != nil {
					params.AsyncStopCompletion(completion)
				}
			},
		)
		if queued && params.AsyncStopQueued != nil {
			params.AsyncStopQueued()
		}
		if queued || params.AsyncStopTracker.drainAckStopInFlight(drainAckAsyncStopKey(info.ID, name)) {
			return exactSessionStartKeyedOwner, errSessionStartPoolDrainAckPending
		}
		return park(errors.New("drain acknowledgement stop could not be queued"))
	}

	ownershipNow := clk.Now().UTC()
	lifecycle, cfgAgent, owner := classifyExactSessionStartOwnership(info, params.Config, ownershipNow)
	// The ordinary reset family dispatches on the durable row, not on how the
	// admission arrived — F2's rule (ga-f7v2ft.125) applied to the last family
	// that still carried the gate (ga-f7v2ft.139). The controller keeps ONE
	// source per key: the earlier one when a pending in_process or an incoming
	// anti_entropy folds, the later one otherwise (session_start_controller.go
	// admit(), :435). So ANY family that lands on a reset-carrying key before
	// this arm sees it consumes the admission, and a source gate here answers
	// with no effect and nothing left to re-detect until the next anti-entropy
	// pass. exactOrdinaryResetCurrent is the whole authority: it is this
	// family's ownership lattice (live, ordinary, awake, no dependencies, not
	// held or quarantined), so no source can widen what it admits.
	resetAdmitted := initialResponse.Revision != 0 &&
		exactOrdinaryResetCurrent(info, params.Config, ownershipNow)
	if resetAdmitted {
		cfgAgent = findAgentByTemplate(params.Config, resolvedSessionTemplateInfo(info, params.Config))
		owner = exactSessionStartKeyedOwner
	}
	var configuredDependencyLease *configuredDependencyStartLease
	configuredDependencyCurrent := func(sessionpkg.Info, bool) bool { return false }
	configuredDependencyFailure := func(cause error) (exactSessionStartOwner, error) {
		if params.RolloutMode == rollout.Auto && !admission.ConfiguredDependencyEntered {
			return exactSessionStartLegacyOwner, nil
		}
		return exactSessionStartKeyedOwner, fmt.Errorf("required configured-dependency start parked: %w", cause)
	}
	if admission.ConfiguredDependency != nil {
		lease := *admission.ConfiguredDependency
		configuredDependencyLease = &lease
		configuredDependencyCurrent = func(current sessionpkg.Info, requireExplicitWake bool) bool {
			return validateConfiguredDependencyStartLease(lease) == nil &&
				lease.ControllerGeneration == params.Generation &&
				configuredDependencyStartTargetMatches(current, params.Config, lease) &&
				(!requireExplicitWake || configuredDependencyExplicitWakeCurrent(current, clk.Now().UTC())) &&
				params.ValidateConfiguredDependencyStart != nil &&
				params.ValidateConfiguredDependencyStart(current, lease)
		}
		if !configuredDependencyCurrent(info, !admission.ConfiguredDependencyEntered) {
			return configuredDependencyFailure(errors.New("configured-dependency witness changed before reconciliation"))
		}
		cfgAgent = findAgentByTemplate(params.Config, lease.TargetTemplate)
		if cfgAgent == nil {
			return configuredDependencyFailure(errors.New("configured-dependency target is unavailable"))
		}
		owner = exactSessionStartKeyedOwner
	}
	var strictDefaultPoolWakeLease *strictDefaultPoolWakeStartLease
	strictDefaultPoolWakeCurrent := func(sessionpkg.Info, bool) bool { return false }
	strictDefaultPoolWakeFailure := func(cause error) (exactSessionStartOwner, error) {
		if params.RolloutMode == rollout.Auto && !admission.StrictDefaultPoolWakeEntered {
			return exactSessionStartLegacyOwner, nil
		}
		return exactSessionStartKeyedOwner, fmt.Errorf("strict-default pool wake parked: %w", cause)
	}
	if admission.StrictDefaultPoolWake != nil {
		lease := *admission.StrictDefaultPoolWake
		strictDefaultPoolWakeLease = &lease
		strictDefaultPoolWakeCurrent = func(current sessionpkg.Info, entered bool) bool {
			matches := strictDefaultPoolWakeStartMatches(current, params.Config, lease, clk.Now().UTC())
			if entered {
				matches = strictDefaultPoolWakeEnteredMatches(current, params.Config, lease, clk.Now().UTC())
			}
			return matches && lease.ControllerGeneration == params.Generation &&
				params.ValidateStrictDefaultPoolWakeStart != nil &&
				params.ValidateStrictDefaultPoolWakeStart(current, lease)
		}
		if !admission.StrictDefaultPoolWakeEntered && initialResponse.Revision != lease.SessionRevision {
			return strictDefaultPoolWakeFailure(errors.New("strict-default pool wake row revision changed before reconciliation"))
		}
		if !strictDefaultPoolWakeCurrent(info, admission.StrictDefaultPoolWakeEntered) {
			return strictDefaultPoolWakeFailure(errors.New("strict-default pool wake witness changed before reconciliation"))
		}
		cfgAgent = findAgentByTemplate(params.Config, lease.PoolTarget)
		if cfgAgent == nil {
			return strictDefaultPoolWakeFailure(errors.New("strict-default pool wake target is unavailable"))
		}
		owner = exactSessionStartKeyedOwner
	}
	var configuredNamedWakeLease *configuredNamedWakeStartLease
	configuredNamedWakeCurrent := func(sessionpkg.Info, bool) bool { return false }
	configuredNamedWakeFailure := func(cause error) (exactSessionStartOwner, error) {
		if params.RolloutMode == rollout.Auto && !admission.ConfiguredNamedWakeEntered {
			return exactSessionStartLegacyOwner, nil
		}
		return exactSessionStartKeyedOwner, fmt.Errorf("configured named wake parked: %w", cause)
	}
	if admission.ConfiguredNamedWake != nil {
		lease := *admission.ConfiguredNamedWake
		configuredNamedWakeLease = &lease
		configuredNamedWakeCurrent = func(current sessionpkg.Info, entered bool) bool {
			matches := configuredNamedWakeStartMatches(current, params.Config, params.CityName, lease, clk.Now().UTC())
			if entered {
				matches = configuredNamedWakeEnteredMatches(current, params.Config, params.CityName, lease, clk.Now().UTC())
			}
			return matches && lease.ControllerGeneration == params.Generation &&
				params.ValidateConfiguredNamedWakeStart != nil && params.ValidateConfiguredNamedWakeStart(current, lease)
		}
		if !admission.ConfiguredNamedWakeEntered && initialResponse.Revision != lease.SessionRevision {
			return configuredNamedWakeFailure(errors.New("configured named wake row revision changed before reconciliation"))
		}
		if !configuredNamedWakeCurrent(info, admission.ConfiguredNamedWakeEntered) {
			return configuredNamedWakeFailure(errors.New("configured named wake witness changed before reconciliation"))
		}
		cfgAgent = findAgentByTemplate(params.Config, lease.Template)
		if cfgAgent == nil {
			return configuredNamedWakeFailure(errors.New("configured named wake template is unavailable"))
		}
		owner = exactSessionStartKeyedOwner
	}
	if admission.WaitDependency != nil && cfgAgent == nil {
		cfgAgent = findAgentByTemplate(params.Config, resolvedSessionTemplateInfo(info, params.Config))
	}
	if admission.WaitDependency != nil && cfgAgent != nil && waitDependencyConfiguredTemplateEligible(info, params.Config, params.Provider, params.CityName, params.Store, ownershipNow) &&
		!info.DependencyOnly && (!isNamedSessionInfo(info) || isCanonicalConfiguredNamedSessionForStart(info, params.Config)) {
		// A retained dependency-wait lease is the narrow proof that this otherwise
		// legacy sleeping session belongs to the keyed handoff.
		owner = exactSessionStartKeyedOwner
	}
	poolStartAuthorized := false
	if (owner == exactSessionStartLegacyOwner || owner == exactSessionStartUnowned) && admission.PoolAllocation != nil && params.AuthorizePoolStart != nil &&
		isPoolManagedSessionInfo(info) && !isNamedSessionInfo(info) {
		authorized, authorizeErr := params.AuthorizePoolStart(ctx, info, *admission.PoolAllocation)
		if authorizeErr != nil {
			return owner, fmt.Errorf("reconciling exact pool session start %q: authorizing allocation: %w", info.ID, authorizeErr)
		}
		if authorized {
			template := resolvedSessionTemplateInfo(info, params.Config)
			cfgAgent = findAgentByTemplate(params.Config, template)
			if cfgAgent == nil {
				return owner, fmt.Errorf("reconciling exact pool session start %q: authorized template %q is unavailable", info.ID, template)
			}
			owner = exactSessionStartKeyedOwner
			poolStartAuthorized = true
		}
	}
	if owner == exactSessionStartLegacyOwner && exactSessionStartWakeFamilyCandidate(info, params.Config) &&
		params.CertifyWakeFamilyStart != nil && params.CertifyWakeFamilyStart(info, initialResponse.Revision) {
		// The pre-lease seam took the row: a certified wake lease now names this
		// exact key, so the family's legacy exclusion is live and no fallback poke
		// may fire. This pass applies no effect — the re-admitted, lease-bearing
		// pass does — so the status read is retained unchanged.
		retainStatusFromInitialRead(exactSessionLifecycleStatusInput{
			Context:           exactSessionLifecycleStatusContextUnavailable,
			UnavailableReason: exactSessionLifecycleStatusReasonNotObserved,
		})
		return exactSessionStartKeyedOwner, nil
	}
	if owner != exactSessionStartKeyedOwner {
		reason := exactSessionLifecycleStatusReasonNotObserved
		if owner == exactSessionStartLegacyOwner {
			template := resolvedSessionTemplateInfo(info, params.Config)
			if template == "" || findAgentByTemplate(params.Config, template) == nil {
				reason = exactSessionLifecycleStatusReasonPrerequisiteUnavailable
			}
		}
		retainStatusFromInitialRead(exactSessionLifecycleStatusInput{
			Context:           exactSessionLifecycleStatusContextUnavailable,
			UnavailableReason: reason,
		})
		return owner, nil
	}

	template := resolvedSessionTemplateInfo(info, params.Config)
	if template == "" {
		templateErr := fmt.Errorf("persisted template is empty")
		retainStatusFromInitialRead(exactSessionLifecycleStatusInput{UnavailableReason: exactSessionLifecycleStatusReasonPrerequisiteUnavailable, Error: templateErr.Error()})
		return owner, fmt.Errorf("reconciling exact session start %q: %w", info.ID, templateErr)
	}
	if cfgAgent == nil {
		retainStatusFromInitialRead(exactSessionLifecycleStatusInput{UnavailableReason: exactSessionLifecycleStatusReasonPrerequisiteUnavailable})
		return owner, nil
	}
	if isAgentEffectivelySuspendedWith(params.Config, params.CityPath, cfgAgent, loadSuspensionStateBestEffort(params.CityPath)) {
		retainStatusFromInitialRead(exactSessionLifecycleStatusInput{UnavailableReason: exactSessionLifecycleStatusReasonNotObserved})
		return owner, nil
	}
	if lifecycle.HasBlocker(sessionpkg.BlockerHeld) || lifecycle.HasBlocker(sessionpkg.BlockerQuarantined) {
		retainStatusFromInitialRead(exactSessionLifecycleStatusInput{UnavailableReason: exactSessionLifecycleStatusReasonNotObserved})
		return owner, nil
	}

	tp, resolvedInfo, err := resolveExactSessionStartTemplate(params, info, cfgAgent, clk, stderr)
	// Fold the resolver's Info back before the error branch reads info.ID: the
	// named resolver may have durably cleared a stale trigger stamp on the way
	// in, and every downstream read here must see the cleared row.
	info = resolvedInfo
	if err != nil {
		retainStatusFromInitialRead(exactSessionLifecycleStatusInput{UnavailableReason: exactSessionLifecycleStatusReasonPrerequisiteUnavailable, Error: err.Error()})
		return owner, fmt.Errorf("reconciling exact session start %q: resolving template: %w", info.ID, err)
	}
	var resetLease *exactOrdinaryResetStartLease
	if resetAdmitted {
		committed, committedResponse, resetErr := commitExactOrdinaryResetHandoff(params, info, initialResponse, tp, clk, stderr)
		if resetErr != nil {
			// The Auto handback is LIVE but does only one of the two things its
			// shape suggests (evaluated for ga-f7v2ft.133 item 2; it dies with the
			// flag at WE, not before). It transfers the row exactly when the
			// handoff refused because the AUTHORITY CHANGED — the pre-stop reread,
			// the pre-handoff reread, the post-breaker-clear reread, or a lost
			// write fence — because a row that left exactOrdinaryResetCurrent is a
			// row legacy is no longer excluded from, and legacy is then its
			// rightful owner.
			//
			// On every OTHER refusal it is a release, not a transfer, and the same
			// predicate is why: exactOrdinaryResetCurrent is BOTH this family's
			// ownership lattice and legacy's exclusion test
			// (resolveExactSessionStartOrDrainAckStopOwnership, consulted by
			// sessionStartLegacyExclusionPredicate under Auto and Require alike).
			// The refusals that leave the row untouched — a capability gap, an
			// incomplete liveness observation, a failed stop or confirm-dead, a
			// read error — leave the row still reset-current, so legacy declines
			// it and the reset waits for the next anti-entropy admission to
			// re-take the key. The stop itself writes nothing durable
			// (Manager.StopUnattendedSession is a pure provider call), so a
			// post-stop refusal does not move the row out of the lattice either.
			if params.RolloutMode == rollout.Auto {
				return exactSessionStartLegacyOwner, fmt.Errorf("%w: %w", errSessionStartLegacyFallbackRequired, resetErr)
			}
			return exactSessionStartKeyedOwner, resetErr
		}
		info = committed
		initialResponse = committedResponse
		loadedRevision = committedResponse.Revision
		resetLease = &exactOrdinaryResetStartLease{
			SessionID:        info.ID,
			SessionName:      strings.TrimSpace(info.SessionNameMetadata),
			ResetCommittedAt: strings.TrimSpace(info.ResetCommittedAt),
		}
	}
	if admission.Source == sessionStartAdmissionInProcess || admission.Source == sessionStartAdmissionWaitDependency {
		if invalidator, ok := params.Provider.(runtime.LivenessInvalidator); ok {
			invalidator.InvalidateLiveness(info.SessionName)
		}
	}
	observation, err := observeLoadedSession(
		ctx, params.CityPath, params.Store, params.Provider, params.Config, info, tp.Hints.ProcessNames,
	)
	if err != nil {
		retainStatusFromInitialRead(exactSessionLifecycleStatusInput{UnavailableReason: exactSessionLifecycleStatusReasonObservationUnavailable, Error: err.Error()})
		return owner, fmt.Errorf("reconciling exact session start %q: observing runtime: %w", info.ID, err)
	}
	statusObservedAt := clk.Now().UTC()
	retainStatusFromInitialRead(exactSessionLifecycleStatusInput{
		Context:             exactSessionLifecycleStatusContextDesired,
		Observation:         observation,
		ObservedAt:          statusObservedAt,
		StartupTimeout:      params.Config.Session.StartupTimeoutDuration(),
		HealInputsRowBacked: exactSessionStatusHealInputsAreRowBacked(info, params.Config),
	})
	if (params.StatusWriter != nil || params.StatusWriterError != nil) &&
		statusResult != nil && statusResult.RuntimeLive && statusResult.Plan != nil && statusResult.Plan.Outcome == sessionLifecycleStatusHeal {
		if params.StatusWriterError != nil {
			return owner, fmt.Errorf("reconciling exact session start %q: resolving session-status writer: %w", info.ID, params.StatusWriterError)
		}
		plan := statusResult.Plan
		if statusResult.Context != exactSessionLifecycleStatusContextDesired ||
			statusResult.Disposition != exactSessionLifecycleStatusDispositionCandidate ||
			statusResult.RequestedID == "" || statusResult.RequestedID != statusResult.LoadedID ||
			statusResult.RequestedID != plan.SessionID || statusResult.LoadedRevision == 0 || len(plan.Patch) == 0 {
			return owner, fmt.Errorf("reconciling exact session start %q: malformed session-status heal candidate", info.ID)
		}
		if err := params.StatusWriter.UpdateIfMatch(statusResult.RequestedID, statusResult.LoadedRevision, beads.UpdateOpts{Metadata: plan.Patch}); err != nil {
			return owner, fmt.Errorf("reconciling exact session start %q: applying session-status heal: %w", info.ID, err)
		}
		statusResult.EffectApplied = true
	}

	startupTimeout := params.Config.Session.StartupTimeoutDuration()
	circuitOpen := exactSessionCircuitOpen(params, info, ownershipNow)
	providerUnavailable := false
	if tp.ResolvedProvider != nil {
		providerUnavailable = exactSessionProviderUnavailable(params, tp.ResolvedProvider.Name)
	}
	plan := planSessionLifecycleStartSelection(sessionLifecycleStartShadowInput{
		Info:                 info,
		WakeDecisionObserved: true,
		ShouldWake:           true,
		RuntimeObserved:      true,
		RuntimeAlive:         runtimeObservationLive(observation),
		ObservedAt:           ownershipNow,
		StartupTimeout:       startupTimeout,
		CircuitOpen:          circuitOpen,
		ProviderUnavailable:  providerUnavailable,
	})
	if plan.Outcome != sessionLifecycleStartSelectionPrepare {
		return owner, nil
	}
	// The dependency wait itself is the wake reason, so the selection input
	// intentionally says ShouldWake even while wait_hold remains durable. Every
	// other ordinary start gate above still applies before the wait is claimed.
	if admission.WaitDependency != nil {
		return reconcileExactWaitDependencyStart(
			ctx, admission, params, info, initialResponse, startCandidate{info: info, tp: tp}, clk, recorder, stdout, stderr, startupTimeout, startOpts,
		)
	}
	if poolStartAuthorized && admission.PoolAllocation.RecoverActive {
		return reconcileExactPoolRecoveryStart(
			ctx,
			admission,
			params,
			startCandidate{info: info, tp: tp},
			clk,
			recorder,
			stdout,
			stderr,
			startupTimeout,
			startOpts,
		)
	}

	var preWakeRead func(beads.Store, string) (sessionpkg.Info, int64, error)
	switch {
	case configuredNamedWakeLease != nil && !admission.ConfiguredNamedWakeEntered:
		preWakeRead = func(store beads.Store, id string) (sessionpkg.Info, int64, error) {
			current, persisted, readErr := getAuthoritativeSessionStartPersistedRecord(store, id)
			if readErr != nil {
				return sessionpkg.Info{}, 0, readErr
			}
			if persisted.Revision != configuredNamedWakeLease.SessionRevision || !configuredNamedWakeCurrent(current, false) {
				if params.RolloutMode == rollout.Auto {
					return sessionpkg.Info{}, 0, &exactSessionStartPreWakeSkip{owner: exactSessionStartLegacyOwner}
				}
				return sessionpkg.Info{}, 0, errors.New("required configured named wake witness changed before pre-wake")
			}
			return current, persisted.Revision, nil
		}
	case strictDefaultPoolWakeLease != nil && !admission.StrictDefaultPoolWakeEntered:
		preWakeRead = func(store beads.Store, id string) (sessionpkg.Info, int64, error) {
			current, persisted, readErr := getAuthoritativeSessionStartPersistedRecord(store, id)
			if readErr != nil {
				return sessionpkg.Info{}, 0, readErr
			}
			if persisted.Revision != strictDefaultPoolWakeLease.SessionRevision || !strictDefaultPoolWakeCurrent(current, false) {
				if params.RolloutMode == rollout.Auto {
					return sessionpkg.Info{}, 0, &exactSessionStartPreWakeSkip{owner: exactSessionStartLegacyOwner}
				}
				return sessionpkg.Info{}, 0, errors.New("required strict-default pool wake witness changed before pre-wake")
			}
			return current, persisted.Revision, nil
		}
	case configuredDependencyLease != nil && !admission.ConfiguredDependencyEntered:
		preWakeRead = func(store beads.Store, id string) (sessionpkg.Info, int64, error) {
			current, revision, readErr := getAuthoritativeSessionStartRecord(store, id)
			if readErr != nil {
				return sessionpkg.Info{}, 0, readErr
			}
			if !configuredDependencyCurrent(current, true) {
				if params.RolloutMode == rollout.Auto {
					return sessionpkg.Info{}, 0, &exactSessionStartPreWakeSkip{owner: exactSessionStartLegacyOwner}
				}
				return sessionpkg.Info{}, 0, errors.New("required configured-dependency witness changed before pre-wake")
			}
			return current, revision, nil
		}
	case resetLease != nil:
		lease := *resetLease
		preWakeRead = func(store beads.Store, id string) (sessionpkg.Info, int64, error) {
			current, revision, readErr := getAuthoritativeSessionStartRecord(store, id)
			if readErr != nil {
				return sessionpkg.Info{}, 0, readErr
			}
			if !lease.pending(current) {
				return sessionpkg.Info{}, 0, errors.New("exact reset witness changed before pre-wake")
			}
			return current, revision, nil
		}
	case poolStartAuthorized:
		lease := *admission.PoolAllocation
		preWakeRead = func(store beads.Store, id string) (sessionpkg.Info, int64, error) {
			current, revision, readErr := getAuthoritativeSessionStartRecord(store, id)
			if readErr != nil {
				return sessionpkg.Info{}, 0, readErr
			}
			authorized, authorizeErr := params.AuthorizePoolStart(ctx, current, lease)
			if authorizeErr != nil {
				return sessionpkg.Info{}, 0, authorizeErr
			}
			if !authorized {
				if lease.RecoverActive && params.RolloutMode == rollout.Require {
					return sessionpkg.Info{}, 0, &exactSessionStartPreWakeSkip{owner: exactSessionStartUnowned}
				}
				return sessionpkg.Info{}, 0, &exactSessionStartPreWakeSkip{owner: exactSessionStartLegacyOwner}
			}
			return current, revision, nil
		}
	}
	var prepared *preparedStart
	if configuredNamedWakeLease != nil && admission.ConfiguredNamedWakeEntered ||
		strictDefaultPoolWakeLease != nil && admission.StrictDefaultPoolWakeEntered ||
		configuredDependencyLease != nil && admission.ConfiguredDependencyEntered {
		prepared, _, err = buildPreparedStartWithWorkDirResolver(
			startCandidate{info: info, tp: tp}, params.CityPath, params.Config, params.Store, startOpts.workDirResolver,
		)
	} else {
		prepareStore := params.Store
		switch {
		case configuredNamedWakeLease != nil:
			lease := *configuredNamedWakeLease
			prepareStore = &retainedExactStartPreWakeStore{
				Store:     params.Store,
				sessionID: lease.SessionID,
				enter: func() bool {
					return params.EnterConfiguredNamedWakeStart != nil && params.EnterConfiguredNamedWakeStart(lease)
				},
			}
		case strictDefaultPoolWakeLease != nil:
			lease := *strictDefaultPoolWakeLease
			prepareStore = &retainedExactStartPreWakeStore{
				Store:     params.Store,
				sessionID: lease.SessionID,
				enter: func() bool {
					return params.EnterStrictDefaultPoolWakeStart != nil && params.EnterStrictDefaultPoolWakeStart(lease)
				},
			}
		case configuredDependencyLease != nil:
			lease := *configuredDependencyLease
			prepareStore = &retainedExactStartPreWakeStore{
				Store:     params.Store,
				sessionID: lease.SessionID,
				enter: func() bool {
					return params.EnterConfiguredDependencyStart != nil && params.EnterConfiguredDependencyStart(lease)
				},
			}
		}
		prepared, err = prepareExactStartCandidateForCity(
			startCandidate{info: info, tp: tp},
			params.CityPath,
			params.CityName,
			params.Config,
			params.Provider,
			prepareStore,
			clk,
			stderr,
			startOpts.workDirResolver,
			preWakeRead,
		)
	}
	if err != nil {
		var skip *exactSessionStartPreWakeSkip
		if errors.As(err, &skip) {
			return skip.owner, nil
		}
		// Another writer moved the row between this entrant's authoritative
		// re-read and its commit. Unlike startCommitSuperseded — which reports a
		// start that already RAN and must not be repeated — a lost pre-wake CAS
		// wrote nothing and started nothing, and the durable wake cause is still
		// there. Surfacing it keeps the key on the exact-start workqueue so the
		// next attempt re-reads; converging silently would strand the wake until
		// some unrelated admission happened to arrive (ga-l1j53).
		if errors.Is(err, errPreWakeSuperseded) {
			return exactSessionStartKeyedOwner, fmt.Errorf("reconciling exact session start %q: %w", info.ID, err)
		}
		return owner, fmt.Errorf("reconciling exact session start %q: preparing start: %w", info.ID, err)
	}
	var result startResult
	switch {
	case configuredNamedWakeLease != nil:
		authorize := func(context.Context) error {
			latest, _, readErr := getAuthoritativeSessionStartRecord(params.Store, configuredNamedWakeLease.SessionID)
			if readErr != nil {
				return fmt.Errorf("reading configured named session before provider start: %w", readErr)
			}
			if !asyncStartIdentityMatchesInfo(prepared.candidate.info, latest) {
				return errors.New("configured named session identity changed before provider start")
			}
			if !configuredNamedWakeCurrent(latest, true) {
				return errors.New("configured named wake witness changed before provider start")
			}
			return nil
		}
		result = runPreparedStartCandidateAuthorized(
			ctx,
			*prepared,
			params.CityPath,
			params.Provider,
			params.Store,
			params.Config,
			startupTimeout,
			resolveStartStabilityWaiter(startOpts.stabilityWaiter),
			startOpts.sessionStaleKeyDetectionWaiter,
			authorize,
		)
	case strictDefaultPoolWakeLease != nil:
		authorize := func(context.Context) error {
			latest, _, readErr := getAuthoritativeSessionStartRecord(params.Store, strictDefaultPoolWakeLease.SessionID)
			if readErr != nil {
				return fmt.Errorf("reading strict-default pool member before provider start: %w", readErr)
			}
			if !asyncStartIdentityMatchesInfo(prepared.candidate.info, latest) {
				return errors.New("strict-default pool member identity changed before provider start")
			}
			if !strictDefaultPoolWakeCurrent(latest, true) {
				return errors.New("strict-default pool wake witness changed before provider start")
			}
			return nil
		}
		result = runPreparedStartCandidateAuthorized(
			ctx,
			*prepared,
			params.CityPath,
			params.Provider,
			params.Store,
			params.Config,
			startupTimeout,
			resolveStartStabilityWaiter(startOpts.stabilityWaiter),
			startOpts.sessionStaleKeyDetectionWaiter,
			authorize,
		)
	case configuredDependencyLease != nil:
		authorize := func(context.Context) error {
			latest, _, readErr := getAuthoritativeSessionStartRecord(params.Store, configuredDependencyLease.SessionID)
			if readErr != nil {
				return fmt.Errorf("reading configured-dependency target before provider start: %w", readErr)
			}
			if !asyncStartIdentityMatchesInfo(prepared.candidate.info, latest) {
				return errors.New("configured-dependency target identity changed before provider start")
			}
			if !configuredDependencyCurrent(latest, false) {
				return errors.New("configured-dependency witness changed before provider start")
			}
			return nil
		}
		result = runPreparedStartCandidateAuthorized(
			ctx,
			*prepared,
			params.CityPath,
			params.Provider,
			params.Store,
			params.Config,
			startupTimeout,
			resolveStartStabilityWaiter(startOpts.stabilityWaiter),
			startOpts.sessionStaleKeyDetectionWaiter,
			authorize,
		)
	case resetLease != nil:
		lease := *resetLease
		authorize := func(context.Context) error {
			latest, _, readErr := getAuthoritativeSessionStartRecord(params.Store, lease.SessionID)
			if readErr != nil {
				return fmt.Errorf("reading exact reset session before provider start: %w", readErr)
			}
			if !asyncStartIdentityMatchesInfo(prepared.candidate.info, latest) {
				return errors.New("exact reset session identity changed before provider start")
			}
			if !lease.matches(latest) {
				return errors.New("exact reset witness changed before provider start")
			}
			return nil
		}
		result = runPreparedStartCandidateAuthorized(
			ctx,
			*prepared,
			params.CityPath,
			params.Provider,
			params.Store,
			params.Config,
			startupTimeout,
			resolveStartStabilityWaiter(startOpts.stabilityWaiter),
			startOpts.sessionStaleKeyDetectionWaiter,
			authorize,
		)
	default:
		results := executePreparedStartWaveForCity(
			ctx,
			[]preparedStart{*prepared},
			params.CityPath,
			params.Provider,
			params.Store,
			params.Config,
			startupTimeout,
			1,
			params.StartOptions...,
		)
		if len(results) != 1 {
			return owner, fmt.Errorf("reconciling exact session start %q: start returned %d results", info.ID, len(results))
		}
		result = results[0]
	}
	disposition := commitStartResultWithFreshness(
		ctx, result, params.Provider, params.Store, clk, recorder, 0, stdout, stderr, nil,
	)
	if disposition == startCommitSuperseded {
		return owner, nil
	}
	if disposition != startCommitCommitted {
		if result.err != nil {
			return owner, fmt.Errorf("reconciling exact session start %q: %w", info.ID, result.err)
		}
		return owner, fmt.Errorf("reconciling exact session start %q: start result did not commit", info.ID)
	}
	recordExactSessionStartCommit(params, admission, result)
	return owner, nil
}

// reconcileExactWaitDependencyStart owns the short, durable handoff from one
// satisfied wait to one provider start. Before the wait claim Auto may yield;
// once the claim commits every later uncertainty remains keyed.
func reconcileExactWaitDependencyStart(
	ctx context.Context,
	admission sessionStartAdmission,
	params exactSessionStartParams,
	info sessionpkg.Info,
	initial sessionpkg.PersistedResponse,
	candidate startCandidate,
	clk clock.Clock,
	recorder events.Recorder,
	stdout, stderr io.Writer,
	startupTimeout time.Duration,
	startOpts startExecutionOptions,
) (exactSessionStartOwner, error) {
	lease := *admission.WaitDependency
	if err := validateSessionWaitDependencyStartLease(lease); err != nil {
		return exactSessionStartKeyedOwner, fmt.Errorf("dependency wait start lease is invalid: %w", err)
	}
	if lease.ControllerGeneration != params.Generation {
		return exactSessionStartKeyedOwner, errors.New("dependency wait start lease belongs to a different controller generation")
	}
	preClaimFailure := func(cause error) (exactSessionStartOwner, error) {
		if params.RolloutMode == rollout.Auto {
			return exactSessionStartLegacyOwner, nil
		}
		return exactSessionStartKeyedOwner, cause
	}
	readStore := authoritativeSessionStartReadStore{Store: params.Store, live: beads.HandlesFor(params.Store).Live}
	wait, waitPersisted, err := sessionFrontDoor(readStore).GetWaitPersistedResponse(lease.WaitID)
	if err != nil {
		return preClaimFailure(fmt.Errorf("reading dependency wait before claim: %w", err))
	}
	registeredWait := wait
	registeredWait.State = waitStatePending
	registration, indexable, err := waitDependencyRegistrationFrom(registeredWait)
	if err != nil {
		return preClaimFailure(fmt.Errorf("canonicalizing dependency wait before claim: %w", err))
	}
	if wait.ID != lease.WaitID || !indexable || registration.sessionID != lease.SessionID || registration.depMode != lease.DepMode || !slices.Equal(registration.depIDs, lease.DepIDs) || wait.RegisteredEpoch != lease.RegisteredEpoch {
		return preClaimFailure(errors.New("dependency wait no longer matches leased pending revision"))
	}
	alreadyClaimed := wait.State == waitStateReady && wait.ReadyOwner == string(sessionpkg.WaitReadyOwnerDependency) && wait.ReadyOperation == lease.Operation
	if info.ID != lease.SessionID || info.Closed || initial.Revision != lease.SessionRevision {
		if alreadyClaimed {
			return exactSessionStartKeyedOwner, errors.New("dependency session changed after this operation claimed the wait")
		}
		return preClaimFailure(errors.New("dependency wait session no longer matches leased revision"))
	}
	if wait.ExpiresAt != "" {
		expiresAt, parseErr := time.Parse(time.RFC3339, wait.ExpiresAt)
		if parseErr != nil || !expiresAt.After(clk.Now().UTC()) {
			if alreadyClaimed {
				return exactSessionStartKeyedOwner, errors.New("dependency wait expired after this operation claimed it")
			}
			return preClaimFailure(errors.New("dependency wait expired before claim"))
		}
	}
	if !alreadyClaimed && (wait.State != waitStatePending || waitPersisted.Revision != lease.WaitRevision) {
		return preClaimFailure(errors.New("dependency wait no longer matches leased pending revision"))
	}
	ready, err := depsWaitReadyDetailedFrom(newAuthoritativeWaitDependencyStoreSet(params.Store, params.RigStores), wait)
	if err != nil || !ready {
		if alreadyClaimed {
			if err != nil {
				return exactSessionStartKeyedOwner, fmt.Errorf("rechecking dependency readiness after wait claim: %w", err)
			}
			return exactSessionStartKeyedOwner, errors.New("dependency readiness changed after wait claim")
		}
		if err != nil {
			return preClaimFailure(fmt.Errorf("rechecking dependency readiness: %w", err))
		}
		return preClaimFailure(errors.New("dependency wait is no longer ready"))
	}
	boundedPoolTarget, boundedPool := waitDependencyBoundedPoolTarget(info, params.Config)
	if boundedPool && (lease.PoolTarget != boundedPoolTarget || lease.PoolMembershipRevision == 0 ||
		params.ValidateWaitDependencyPoolWitness == nil || !params.ValidateWaitDependencyPoolWitness(info, lease)) {
		return preClaimFailure(errors.New("bounded-pool dependency wait witness changed before claim"))
	}
	if !boundedPool && lease.PoolTarget != "" {
		return preClaimFailure(errors.New("dependency wait retained a bounded-pool witness outside that cohort"))
	}
	waitFront := sessionFrontDoor(params.Store) // retain the original front door so its conditional writer remains reachable.
	if !alreadyClaimed {
		claim, claimErr := waitFront.ClaimPendingWaitReady(wait, waitPersisted, clk.Now().UTC(), sessionpkg.WaitReadyOwnerDependency, lease.Operation)
		if claim.Outcome == sessionpkg.WaitReadyClaimNotApplied {
			return preClaimFailure(claimErr)
		}
		if claimErr != nil || claim.Outcome != sessionpkg.WaitReadyClaimCommitted {
			if claimErr != nil {
				return exactSessionStartKeyedOwner, fmt.Errorf("claiming dependency wait %q: %w", lease.WaitID, claimErr)
			}
			return exactSessionStartKeyedOwner, fmt.Errorf("claiming dependency wait %q did not commit", lease.WaitID)
		}
	}
	if params.StatusWriterError != nil || params.StatusWriter == nil {
		if params.StatusWriterError != nil {
			return exactSessionStartKeyedOwner, fmt.Errorf("resolving dependency start conditional writer: %w", params.StatusWriterError)
		}
		return exactSessionStartKeyedOwner, errors.New("dependency start conditional writer is unavailable")
	}
	current, persisted, err := getAuthoritativeSessionStartPersistedRecord(params.Store, lease.SessionID)
	if err != nil {
		return exactSessionStartKeyedOwner, fmt.Errorf("reading dependency session before pre-wake: %w", err)
	}
	if current.ID != lease.SessionID || current.Closed || persisted.Status != "open" {
		return exactSessionStartKeyedOwner, errors.New("dependency session no longer matches leased revision after wait claim")
	}
	preWakeRecovered := alreadyClaimed && current.InstanceToken == lease.Operation && persisted.Metadata["pending_create_claim"] == "true"
	if !preWakeRecovered && persisted.Revision != lease.SessionRevision {
		return exactSessionStartKeyedOwner, errors.New("dependency session no longer matches leased revision after wait claim")
	}
	committed, committedPersisted := current, persisted
	if !preWakeRecovered {
		_, token, patch, err := buildPreWakePatchWithToken(current, clk, lease.Operation)
		if err != nil {
			return exactSessionStartKeyedOwner, fmt.Errorf("building dependency start pre-wake patch: %w", err)
		}
		if token != lease.Operation {
			return exactSessionStartKeyedOwner, errors.New("dependency start pre-wake token differs from durable wait operation")
		}
		patch["pending_create_claim"] = "true"
		expected := patch.Apply(persisted.Metadata)
		writeErr := params.StatusWriter.UpdateIfMatch(current.ID, persisted.Revision, beads.UpdateOpts{Metadata: patch})
		var readErr error
		committed, committedPersisted, readErr = getAuthoritativeSessionStartPersistedRecord(params.Store, current.ID)
		if readErr != nil {
			return exactSessionStartKeyedOwner, fmt.Errorf("re-reading dependency start pre-wake: %w", readErr)
		}
		if writeErr != nil || committed.ID != current.ID || committed.Closed || committedPersisted.Revision == persisted.Revision || !maps.Equal(committedPersisted.Metadata, expected) {
			if writeErr != nil {
				return exactSessionStartKeyedOwner, fmt.Errorf("committing dependency start pre-wake: %w", writeErr)
			}
			return exactSessionStartKeyedOwner, errors.New("dependency start pre-wake did not persist exactly")
		}
	}
	prepared, _, err := buildPreparedStartWithWorkDirResolver(startCandidate{info: committed, tp: candidate.tp}, params.CityPath, params.Config, params.Store, startOpts.workDirResolver)
	if err != nil {
		return exactSessionStartKeyedOwner, fmt.Errorf("preparing dependency start: %w", err)
	}
	authorize := func(context.Context) error {
		latest, latestPersisted, readErr := getAuthoritativeSessionStartPersistedRecord(params.Store, lease.SessionID)
		if readErr != nil || latest.ID != lease.SessionID || latest.Closed || latestPersisted.Revision != committedPersisted.Revision || latest.InstanceToken != lease.Operation {
			return errors.New("dependency start session changed before provider start")
		}
		if !waitDependencyConfiguredTemplateEligible(latest, params.Config, params.Provider, params.CityName, params.Store, clk.Now().UTC()) {
			return errors.New("configured dependency liveness changed before provider start")
		}
		boundedPoolTarget, boundedPool := waitDependencyBoundedPoolTarget(latest, params.Config)
		if boundedPool && (lease.PoolTarget != boundedPoolTarget || lease.PoolMembershipRevision == 0 ||
			params.ValidateWaitDependencyPoolWitness == nil || !params.ValidateWaitDependencyPoolWitness(latest, lease)) {
			return errors.New("bounded-pool dependency wait witness changed before provider start")
		}
		if !boundedPool && lease.PoolTarget != "" {
			return errors.New("dependency wait retained a bounded-pool witness outside that cohort")
		}
		liveWait, _, waitErr := waitFront.GetWaitPersistedResponse(lease.WaitID)
		registeredLiveWait := liveWait
		registeredLiveWait.State = waitStatePending
		registration, indexable, registrationErr := waitDependencyRegistrationFrom(registeredLiveWait)
		if waitErr != nil || registrationErr != nil || liveWait.ID != lease.WaitID || !indexable || registration.sessionID != lease.SessionID || registration.depMode != lease.DepMode || !slices.Equal(registration.depIDs, lease.DepIDs) || liveWait.State != waitStateReady || liveWait.ReadyOwner != string(sessionpkg.WaitReadyOwnerDependency) || liveWait.ReadyOperation != lease.Operation || liveWait.RegisteredEpoch != lease.RegisteredEpoch {
			return errors.New("dependency wait changed before provider start")
		}
		ready, depErr := depsWaitReadyDetailedFrom(newAuthoritativeWaitDependencyStoreSet(params.Store, params.RigStores), liveWait)
		if depErr != nil || !ready {
			return errors.New("dependency readiness changed before provider start")
		}
		return nil
	}
	result := runPreparedStartCandidateAuthorized(ctx, *prepared, params.CityPath, params.Provider, params.Store, params.Config, startupTimeout, resolveStartStabilityWaiter(startOpts.stabilityWaiter), startOpts.sessionStaleKeyDetectionWaiter, authorize)
	disposition := commitStartResultWithFreshness(ctx, result, params.Provider, params.Store, clk, recorder, 0, stdout, stderr, nil)
	if disposition == startCommitSuperseded {
		return exactSessionStartKeyedOwner, nil
	}
	if disposition != startCommitCommitted {
		if result.err != nil {
			return exactSessionStartKeyedOwner, fmt.Errorf("reconciling dependency wait start %q: %w", lease.SessionID, result.err)
		}
		return exactSessionStartKeyedOwner, fmt.Errorf("reconciling dependency wait start %q: start result did not commit", lease.SessionID)
	}
	recordExactSessionStartCommit(params, admission, result)
	return exactSessionStartKeyedOwner, nil
}

func reconcileExactPoolRecoveryStart(
	ctx context.Context,
	admission sessionStartAdmission,
	params exactSessionStartParams,
	candidate startCandidate,
	clk clock.Clock,
	recorder events.Recorder,
	stdout, stderr io.Writer,
	startupTimeout time.Duration,
	startOpts startExecutionOptions,
) (exactSessionStartOwner, error) {
	lease := *admission.PoolAllocation
	fail := func(cause error) (exactSessionStartOwner, error) {
		if params.RolloutMode == rollout.Require {
			return exactSessionStartKeyedOwner, fmt.Errorf("required exact pool recovery parked: %w", cause)
		}
		return exactSessionStartLegacyOwner, fmt.Errorf("%w: exact pool recovery yielded: %w", errSessionStartLegacyFallbackRequired, cause)
	}
	if params.StatusWriterError != nil {
		return fail(fmt.Errorf("resolving recovery conditional writer: %w", params.StatusWriterError))
	}
	if params.StatusWriter == nil {
		return fail(errors.New("recovery conditional writer is unavailable"))
	}

	current, before, err := getAuthoritativeSessionStartPersistedRecord(params.Store, candidate.info.ID)
	if err != nil {
		return fail(fmt.Errorf("reading exact recovery row before pre-wake: %w", err))
	}
	if current.ID != lease.SessionID || before.Status != "open" || before.Revision != lease.SessionRevision {
		return fail(errors.New("exact recovery row no longer matches its leased revision"))
	}
	authorized, err := params.AuthorizePoolStart(ctx, current, lease)
	if err != nil {
		return fail(fmt.Errorf("authorizing exact recovery before pre-wake: %w", err))
	}
	if !authorized {
		return fail(errors.New("exact recovery authority no longer holds before pre-wake"))
	}

	_, token, patch, err := buildPreWakePatch(current, clk)
	if err != nil {
		return fail(fmt.Errorf("building exact recovery pre-wake patch: %w", err))
	}
	rollbackPatch := make(sessionpkg.MetadataPatch, len(patch))
	for key := range patch {
		rollbackPatch[key] = before.Metadata[key]
	}
	expectedMetadata := patch.Apply(before.Metadata)
	writeErr := params.StatusWriter.UpdateIfMatch(current.ID, before.Revision, beads.UpdateOpts{Metadata: patch})
	committedInfo, committed, readErr := getAuthoritativeSessionStartPersistedRecord(params.Store, current.ID)
	if readErr != nil {
		cause := fmt.Errorf("re-reading exact recovery pre-wake commit: %w", readErr)
		if writeErr != nil {
			cause = fmt.Errorf("%w; conditional write: %w", cause, writeErr)
		}
		return fail(cause)
	}
	if committedInfo.ID != current.ID || committedInfo.Closed || committed.Status != before.Status ||
		committed.Revision == 0 || committed.Revision == before.Revision || !maps.Equal(committed.Metadata, expectedMetadata) {
		if writeErr != nil {
			return fail(fmt.Errorf("committing exact recovery pre-wake metadata: %w", writeErr))
		}
		return fail(errors.New("exact recovery pre-wake metadata did not persist exactly"))
	}
	freshWake := current.WakeMode == "fresh" || pendingContinuationResetNeedsFreshStart(current)
	traceFreshWakeMetadataReset(current.SessionNameMetadata, freshWakeResetPriorValues(current), patch, freshWake)

	rollback := func(cause error) (exactSessionStartOwner, error) {
		if rollbackErr := params.StatusWriter.UpdateIfMatch(current.ID, committed.Revision, beads.UpdateOpts{Metadata: rollbackPatch}); rollbackErr != nil {
			cause = fmt.Errorf("%w; fenced pre-wake restore: %w", cause, rollbackErr)
		}
		return fail(cause)
	}
	prepared, _, err := buildPreparedStartWithWorkDirResolver(
		startCandidate{info: committedInfo, tp: candidate.tp}, params.CityPath, params.Config, params.Store, startOpts.workDirResolver,
	)
	if err != nil {
		return rollback(fmt.Errorf("preparing exact recovery start: %w", err))
	}

	lease.InstanceToken = token
	lease.SessionRevision = committed.Revision
	lease.RecoveryPreWakeCommitted = true
	authorizeAtStart := func(effectCtx context.Context) error {
		latest, persisted, readErr := getAuthoritativeSessionStartPersistedRecord(params.Store, current.ID)
		if readErr != nil {
			return fmt.Errorf("%w: reading effect-boundary row: %w", errExactPoolRecoveryAuthorityLost, readErr)
		}
		if persisted.Revision != lease.SessionRevision || latest.ID != lease.SessionID || strings.TrimSpace(latest.InstanceToken) != lease.InstanceToken {
			return fmt.Errorf("%w: effect-boundary row no longer matches the post-CAS lease", errExactPoolRecoveryAuthorityLost)
		}
		authorized, authorizeErr := params.AuthorizePoolStart(effectCtx, latest, lease)
		if authorizeErr != nil {
			return fmt.Errorf("%w: effect-boundary authorization: %w", errExactPoolRecoveryAuthorityLost, authorizeErr)
		}
		if !authorized {
			return fmt.Errorf("%w: effect-boundary authorization no longer holds", errExactPoolRecoveryAuthorityLost)
		}
		return nil
	}
	result := runPreparedStartCandidateAuthorized(
		ctx,
		*prepared,
		params.CityPath,
		params.Provider,
		params.Store,
		params.Config,
		startupTimeout,
		resolveStartStabilityWaiter(startOpts.stabilityWaiter),
		startOpts.sessionStaleKeyDetectionWaiter,
		authorizeAtStart,
	)
	if errors.Is(result.err, errExactPoolRecoveryAuthorityLost) {
		return rollback(result.err)
	}
	disposition := commitStartResultWithFreshness(
		ctx, result, params.Provider, params.Store, clk, recorder, 0, stdout, stderr, nil,
	)
	if disposition == startCommitSuperseded {
		return exactSessionStartKeyedOwner, nil
	}
	if disposition != startCommitCommitted {
		if result.err != nil {
			return exactSessionStartKeyedOwner, fmt.Errorf("reconciling exact pool recovery %q: %w", current.ID, result.err)
		}
		return exactSessionStartKeyedOwner, fmt.Errorf("reconciling exact pool recovery %q: start result did not commit", current.ID)
	}
	recordExactSessionStartCommit(params, admission, result)
	return exactSessionStartKeyedOwner, nil
}

// Lease-family names recorded as `start_lease` on a start-commit trace. They are
// a wire-visible vocabulary — the WD.15 parity join and the v59 journey both
// filter on them — so they live as constants rather than as literals scattered
// across producers and assertions.
const (
	configuredDependencyLeaseFamily  = "configured_dependency"
	configuredNamedWakeLeaseFamily   = "configured_named_wake"
	strictDefaultPoolWakeLeaseFamily = "strict_default_pool_wake"
	waitDependencyLeaseFamily        = "wait_dependency"
	poolAllocationLeaseFamily        = "pool_allocation"
)

// sessionStartAdmissionLeaseFamily names the certified lease an admission is
// carrying, or "" for an ordinary keyed start.
//
// The admission SOURCE cannot answer this. It is deliberately sticky — a key
// first admitted in-process keeps that source across later admissions
// (sessionStartController.admit) — so a start driven by a certificate minted at
// the pre-lease seam or the detector's routing seam still traces as
// `in_process`. The lease is the ownership proof, so the commit records it by
// name: the WD.15 parity join and the v59 journey both need to distinguish "the
// keyed wake family started this row" from "some keyed admission did".
func sessionStartAdmissionLeaseFamily(admission sessionStartAdmission) string {
	switch {
	case admission.ConfiguredDependency != nil:
		return configuredDependencyLeaseFamily
	case admission.ConfiguredNamedWake != nil:
		return configuredNamedWakeLeaseFamily
	case admission.StrictDefaultPoolWake != nil:
		return strictDefaultPoolWakeLeaseFamily
	case admission.WaitDependency != nil:
		return waitDependencyLeaseFamily
	case admission.PoolAllocation != nil:
		return poolAllocationLeaseFamily
	}
	return ""
}

func recordExactSessionStartCommit(params exactSessionStartParams, admission sessionStartAdmission, result startResult) {
	if params.Trace == nil {
		return
	}
	info := result.prepared.candidate.info
	template := result.prepared.candidate.tp.TemplateName
	cycle := params.Trace.BeginCycle(TraceTickTriggerControl, "exact_session_start_commit", time.Now().UTC(), params.Config)
	if cycle == nil {
		return
	}
	duration := result.finished.Sub(result.started)
	payload := result.phases.tracePayload(info.ID, duration)
	payload["admission"] = string(admission.Source)
	payload["admission_version"] = admission.Version
	if lease := sessionStartAdmissionLeaseFamily(admission); lease != "" {
		payload["start_lease"] = lease
	}
	payload["generation"] = params.Generation
	payload["instance_token"] = info.InstanceToken
	payload["effect_owner"] = detectorKeyedEffectOwner
	payload["effect_applied"] = true
	cycle.recordKeyedEffect(
		TraceSiteLifecycleStartCommit,
		TraceReasonStart,
		TraceOutcomeSuccess,
		"exact_session_start_commit",
		template,
		info.ID,
		info.SessionName,
		duration,
		payload,
	)
	if err := cycle.End(TraceCompletionCompleted, nil); err != nil && params.Stderr != nil {
		fmt.Fprintf(params.Stderr, "session reconciler: recording exact start commit trace: %v\n", err) //nolint:errcheck
	}
}

func drainAckIncarnationStartedAt(info sessionpkg.Info) time.Time {
	if wokeAt, ok := parseRFC3339Metadata(info.LastWokeAt); ok {
		return wokeAt
	}
	if awakeStartedAt, ok := parseRFC3339Metadata(info.AwakeStartedAt); ok {
		return awakeStartedAt
	}
	return time.Time{}
}

// resolveExactSessionStartOwnership projects the durable start family once and
// returns whether the keyed controller owns it. Dependency-bearing templates
// remain legacy-owned until keyed dependency fan-out exists.
func resolveExactSessionStartOwnership(
	info sessionpkg.Info,
	cfg *config.City,
	now time.Time,
) bool {
	_, _, owner := classifyExactSessionStartOwnership(info, cfg, now)
	return owner == exactSessionStartKeyedOwner
}

// exactSessionStatusHealInputsAreRowBacked reports whether the status-heal
// candidate's identity comes from revision-guarded row content. Labels persist
// separately in bd, while common_name and aliases require fallback resolution,
// so only an agent_name that resolves in the current config or a valid stored
// template can authorize a whole-row conditional update.
func exactSessionStatusHealInputsAreRowBacked(info sessionpkg.Info, cfg *config.City) bool {
	if info.Type != sessionpkg.BeadType {
		return false
	}
	if resolvedTemplateForIdentity(info.AgentName, cfg) != "" {
		return true
	}
	return findAgentByTemplate(cfg, info.Template) != nil
}

func resolveExactSessionStartOrDrainAckStopOwnership(
	info sessionpkg.Info,
	cfg *config.City,
	now time.Time,
) bool {
	return isDrainAckStopPendingInfo(info) || exactUserHoldSuspendCurrent(info, now) ||
		exactOrdinaryResetCurrent(info, cfg, now) || resolveExactSessionStartOwnership(info, cfg, now)
}

// exactSessionStartWakeFamilyCandidate is the cheap screen in front of the
// pre-lease ownership seam: it names the rows classifyExactSessionStartOwnership
// hands to legacy PURELY because of their identity shape — a named row or a
// pool-managed row (:2893's fleet-invariant arm) — and that a certified wake
// lease could therefore own. It reads nothing but the row, so a city with no
// wake demand pays one struct test per admission and never reaches certification.
func exactSessionStartWakeFamilyCandidate(info sessionpkg.Info, cfg *config.City) bool {
	if cfg == nil || info.Closed || info.PendingCreateClaim || info.DependencyOnly {
		return false
	}
	if strings.TrimSpace(info.WakeRequest) == "" && strings.TrimSpace(info.PinAwake) != "true" {
		return false
	}
	if isNamedSessionInfo(info) || isPoolManagedSessionInfo(info) {
		return true
	}
	// The classifier's OTHER identity-shaped legacy arm: a dependency-bearing
	// template (:2908). It matters because the wake write lands in the sub-tick
	// window BEFORE sync stamps the row's pool markers, so screening on the
	// stamped shape alone would miss the row at exactly the moment the race is
	// decided — the row would be surrendered, the fallback poke would fire, and
	// the tick it starts would both stamp the markers and consume the wake.
	agent := findAgentByTemplate(cfg, resolvedSessionTemplateInfo(info, cfg))
	return agent != nil && len(agent.DependsOn) > 0
}

func classifyExactSessionStartOwnership(
	info sessionpkg.Info,
	cfg *config.City,
	now time.Time,
) (sessionpkg.LifecycleView, *config.Agent, exactSessionStartOwner) {
	lifecycleInput := sessionpkg.LifecycleInputFromInfo(info)
	lifecycleInput.Now = now
	lifecycleInput.CreatedAt = info.CreatedAt
	lifecycleInput.StaleCreatingAfter = staleCreatingStateTimeout
	lifecycle := sessionpkg.ProjectLifecycle(lifecycleInput)
	if info.Closed {
		return lifecycle, nil, exactSessionStartUnowned
	}
	ownedCause := lifecycle.HasWakeCause(sessionpkg.WakeCausePendingCreate) ||
		lifecycle.HasWakeCause(sessionpkg.WakeCauseExplicit)
	if !ownedCause || lifecycle.Terminal {
		return lifecycle, nil, exactSessionStartUnowned
	}
	// Named-session canonicalization and pool capacity/slot validation are fleet
	// invariants. Until those projections are available by immutable key, the
	// fleet reconciler remains their sole effect owner.
	if isNamedSessionInfo(info) || isPoolManagedSessionInfo(info) {
		return lifecycle, nil, exactSessionStartLegacyOwner
	}

	template := resolvedSessionTemplateInfo(info, cfg)
	if template == "" {
		return lifecycle, nil, exactSessionStartKeyedOwner
	}
	cfgAgent := findAgentByTemplate(cfg, template)
	if cfgAgent == nil {
		return lifecycle, nil, exactSessionStartLegacyOwner
	}
	// Dependency-bearing templates remain legacy-owned until the keyed reverse
	// dependency index lands. Starting them here would bypass the existing
	// dependency wave gate.
	if len(cfgAgent.DependsOn) > 0 {
		return lifecycle, cfgAgent, exactSessionStartLegacyOwner
	}
	if lifecycle.HasWakeCause(sessionpkg.WakeCauseExplicit) && info.DependencyOnly {
		return lifecycle, cfgAgent, exactSessionStartLegacyOwner
	}
	return lifecycle, cfgAgent, exactSessionStartKeyedOwner
}

func exactSessionStartOwnerForKey(
	store beads.Store,
	cfg *config.City,
	sessionID string,
	now time.Time,
) (exactSessionStartOwner, error) {
	if store == nil {
		return exactSessionStartUnowned, fmt.Errorf("session store is nil")
	}
	info, _, err := getAuthoritativeSessionStartRecord(store, sessionID)
	if err != nil {
		return exactSessionStartUnowned, err
	}
	_, _, owner := classifyExactSessionStartOwnership(info, cfg, now)
	return owner, nil
}

// resolveExactSessionStartTemplate resolves the template for one exact keyed
// session. Like resolvePreservedConfiguredNamedSessionTemplate, whose result it
// returns verbatim on the named path, it hands back the session Info the params
// were resolved from as the SECOND value on every path — success and error
// alike — because the named resolver's bindNamedSessionTriggerBead may have
// cleared a stale trigger stamp durably before the resolve. Callers must fold
// that Info back onto their own copy (write-returns-Info); keeping the pre-call
// Info re-injects the cleared stamp downstream (gascity#4373). On the
// non-named path the returned Info is the unchanged input.
func resolveExactSessionStartTemplate(
	params exactSessionStartParams,
	info sessionpkg.Info,
	cfgAgent *config.Agent,
	clk clock.Clock,
	stderr io.Writer,
) (TemplateParams, sessionpkg.Info, error) {
	cityName := params.CityName
	if cityName == "" {
		cityName = config.EffectiveCityName(params.Config, "")
	}
	if isNamedSessionInfo(info) {
		return resolvePreservedConfiguredNamedSessionTemplate(
			params.CityPath,
			cityName,
			params.Config,
			params.Provider,
			params.Store,
			[]sessionpkg.Info{info},
			info,
			clk,
			stderr,
		)
	}

	bp := newAgentBuildParams(cityName, params.CityPath, params.Config, params.Provider, clk.Now().UTC(), params.Store, stderr)
	bp.sessionBeads = newSessionBeadSnapshotFromInfos([]sessionpkg.Info{info})
	var (
		resolveAgent  *config.Agent
		qualifiedName string
	)
	if isManualSessionInfoForAgent(info, cfgAgent) {
		qualifiedName = sessionBeadQualifiedNameInfo(params.CityPath, cfgAgent, bp.rigs, info)
		resolveAgent = sessionBeadConfigAgent(cfgAgent, qualifiedName)
	} else {
		resolveAgent, qualifiedName = canonicalSessionIdentityWithConfigInfo(params.Config, cfgAgent, info)
	}
	if resolveAgent == nil || qualifiedName == "" {
		return TemplateParams{}, info, fmt.Errorf("configured session identity is unresolved")
	}
	tp, err := resolveTemplateForSessionBeadInfo(bp, resolveAgent, qualifiedName, buildFingerprintExtra(resolveAgent), info)
	if err != nil {
		return TemplateParams{}, info, err
	}
	tp.ManualSession = isManualSessionInfoForAgent(info, cfgAgent)
	if tp.ManualSession {
		if alias := strings.TrimSpace(info.Alias); alias != "" {
			tp.Alias = alias
		}
	}
	if isEphemeralSessionInfoForAgent(info, cfgAgent) {
		if !tp.ManualSession || strings.TrimSpace(info.Alias) == "" {
			tp.Alias = ""
		}
		if tp.ManualSession && qualifiedName != "" {
			tp.InstanceName = qualifiedName
		} else {
			tp.InstanceName = info.SessionNameMetadata
		}
	}
	installAgentSideEffects(bp, cfgAgent, tp, stderr)
	return tp, info, nil
}
