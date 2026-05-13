// session_reconciler.go implements the bead-driven reconciliation loop.
// It uses a wake/sleep model: for each session
// bead, compute whether the session should be awake, and manage lifecycle
// transitions using the Phase 2 building blocks.
//
// This reconciler uses desiredState (map[string]TemplateParams) for config
// queries and runtime.Provider directly for lifecycle operations. There
// is no dependency on agent types.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
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
	"github.com/gastownhall/gascity/internal/worker"
)

const maxIdleSleepProbesPerTick = 3

type wakeTarget struct {
	session *beads.Bead
	tp      TemplateParams
	alive   bool
}

// buildDepsMap extracts template dependency edges from config for topo ordering.
// Maps template QualifiedName -> list of dependency template QualifiedNames.
func buildDepsMap(cfg *config.City) map[string][]string {
	if cfg == nil {
		return nil
	}
	deps := make(map[string][]string)
	for _, a := range cfg.Agents {
		if len(a.DependsOn) > 0 {
			deps[a.QualifiedName()] = append([]string(nil), a.DependsOn...)
		}
	}
	return deps
}

func freshRestartSessionKey(tp TemplateParams, meta map[string]string) (string, bool) {
	if tp.ResolvedProvider != nil {
		if strings.TrimSpace(tp.ResolvedProvider.SessionIDFlag) != "" {
			newKey, err := sessionpkg.GenerateSessionKey()
			if err != nil {
				return "", false
			}
			return newKey, true
		}
		if strings.TrimSpace(tp.ResolvedProvider.ResumeFlag) != "" ||
			strings.TrimSpace(tp.ResolvedProvider.ResumeCommand) != "" ||
			strings.TrimSpace(tp.ResolvedProvider.ResumeStyle) != "" {
			return "", true
		}
	}
	if strings.TrimSpace(meta["session_id_flag"]) != "" {
		newKey, err := sessionpkg.GenerateSessionKey()
		if err != nil {
			return "", false
		}
		return newKey, true
	}
	if strings.TrimSpace(meta["resume_flag"]) != "" ||
		strings.TrimSpace(meta["resume_command"]) != "" ||
		strings.TrimSpace(meta["resume_style"]) != "" {
		return "", true
	}
	newKey, err := sessionpkg.GenerateSessionKey()
	if err != nil {
		return "", false
	}
	return newKey, true
}

// allDependenciesAliveForTemplate checks that all template dependencies of a
// resolved logical template have at least one alive instance. Uses the
// runtime.Provider directly instead of agent types for liveness checks.
func allDependenciesAliveForTemplate(
	template string,
	cfg *config.City,
	desiredState map[string]TemplateParams,
	sp runtime.Provider,
	cityName string,
	store beads.Store,
) bool {
	return allDependenciesAliveForTemplateWithClock(template, cfg, desiredState, sp, cityName, store, clock.Real{})
}

func allDependenciesAliveForTemplateWithClock(
	template string,
	cfg *config.City,
	desiredState map[string]TemplateParams,
	sp runtime.Provider,
	cityName string,
	store beads.Store,
	clk clock.Clock,
) bool {
	cfgAgent := findAgentByTemplate(cfg, template)
	if cfgAgent == nil || len(cfgAgent.DependsOn) == 0 {
		return true
	}
	for _, dep := range cfgAgent.DependsOn {
		depCfg := findAgentByTemplate(cfg, dep)
		if depCfg == nil {
			continue // dependency not in config — skip
		}
		if !dependencyTemplateAlive(dep, cfg, desiredState, sp, cityName, store, clk) {
			return false
		}
	}
	return true
}

// allDependenciesAlive checks that all template dependencies of a session
// have at least one alive instance. Uses the runtime.Provider directly
// instead of agent types for liveness checks.
func allDependenciesAlive(
	session beads.Bead,
	cfg *config.City,
	desiredState map[string]TemplateParams,
	sp runtime.Provider,
	cityName string,
	store beads.Store,
) bool {
	return allDependenciesAliveForTemplateWithClock(normalizedSessionTemplate(session, cfg), cfg, desiredState, sp, cityName, store, clock.Real{})
}

func pendingCreateSessionStillLeased(session beads.Bead, cfg *config.City, clk clock.Clock) bool {
	var startupTimeout time.Duration
	if cfg != nil {
		startupTimeout = cfg.Session.StartupTimeoutDuration()
	}
	if strings.TrimSpace(session.Metadata["pending_create_claim"]) == "true" {
		if !pendingCreateLeaseActive(session, clk, startupTimeout) {
			return false
		}
		template := normalizedSessionTemplate(session, cfg)
		if template == "" {
			template = session.Metadata["template"]
		}
		agent := findAgentByTemplate(cfg, template)
		if agent != nil {
			return !agent.Suspended
		}
		return true
	}
	if !sessionStartRequested(session, clk) {
		return false
	}
	template := normalizedSessionTemplate(session, cfg)
	if template == "" {
		template = session.Metadata["template"]
	}
	agent := findAgentByTemplate(cfg, template)
	if agent != nil {
		return !agent.Suspended
	}
	return false
}

func pendingCreateStartInFlight(session beads.Bead, clk clock.Clock, startupTimeout time.Duration) bool {
	if strings.TrimSpace(session.Metadata["pending_create_claim"]) != "true" {
		return false
	}
	lastWoke := strings.TrimSpace(session.Metadata["last_woke_at"])
	if lastWoke == "" {
		return false
	}
	started, err := time.Parse(time.RFC3339, lastWoke)
	if err != nil {
		return false
	}
	if startupTimeout <= 0 {
		// Disabling the provider Start() deadline must not disable stuck-bead
		// recovery forever. Use the default lease window for in-flight detection
		// while leaving the actual Start() context unwrapped.
		startupTimeout = time.Minute
	}
	now := time.Now()
	if clk != nil {
		now = clk.Now()
	}
	return now.Before(started.Add(startupTimeout + staleKeyDetectDelay + 5*time.Second))
}

func pendingCreateLeaseActive(session beads.Bead, clk clock.Clock, startupTimeout time.Duration) bool {
	if strings.TrimSpace(session.Metadata["pending_create_claim"]) != "true" {
		return false
	}
	if pendingCreateStartInFlight(session, clk, startupTimeout) {
		return true
	}
	if strings.TrimSpace(session.Metadata["last_woke_at"]) == "" {
		return !pendingCreateNeverStartedLeaseExpired(session, clk)
	}
	return !pendingCreateAttemptStale(session, clk)
}

// pendingCreateNeverStartedTimeout is the rollback floor for pending creates
// with no last_woke_at start lease. Production-created pending beads record
// pending_create_started_at when they enter state=creating; use that timestamp
// as the lease anchor when present, with CreatedAt as the legacy fallback.
//
// It is intentionally longer than staleCreatingStateTimeout: that one-minute
// window still handles corrupt/unparseable last_woke_at metadata and generic
// creating-state cleanup, while never-started creates need enough time to sit
// behind a busy pool start queue.
const pendingCreateNeverStartedTimeout = 10 * time.Minute

func pendingCreateNeverStartedExpired(session beads.Bead, clk clock.Clock) bool {
	if strings.TrimSpace(session.Metadata["pending_create_claim"]) != "true" {
		return false
	}
	if strings.TrimSpace(session.Metadata["state"]) != "creating" {
		return false
	}
	return pendingCreateNeverStartedLeaseExpired(session, clk)
}

func pendingCreateNeverStartedLeaseExpired(session beads.Bead, clk clock.Clock) bool {
	if strings.TrimSpace(session.Metadata["pending_create_claim"]) != "true" {
		return false
	}
	if strings.TrimSpace(session.Metadata["last_woke_at"]) != "" {
		return false
	}
	anchor := session.CreatedAt
	if started, ok := parseRFC3339Metadata(session.Metadata["pending_create_started_at"]); ok {
		anchor = started
	}
	if anchor.IsZero() {
		return true
	}
	now := time.Now()
	if clk != nil {
		now = clk.Now()
	}
	return now.After(anchor.Add(pendingCreateNeverStartedTimeout))
}

func pendingCreateLeaseExpiredForRollback(session beads.Bead, clk clock.Clock, startupTimeout time.Duration) bool {
	if strings.TrimSpace(session.Metadata["pending_create_claim"]) != "true" {
		return false
	}
	if strings.TrimSpace(session.Metadata["state"]) != "creating" {
		return false
	}
	if pendingCreateStartInFlight(session, clk, startupTimeout) {
		return false
	}
	if strings.TrimSpace(session.Metadata["last_woke_at"]) == "" {
		return pendingCreateNeverStartedExpired(session, clk)
	}
	return staleCreatingState(session, clk)
}

// reconcileSessionBeads performs bead-driven reconciliation using wake/sleep
// semantics. For each session bead, it determines if the session should be
// awake (has a matching entry in the desired state) and manages lifecycle
// transitions using the Phase 2 building blocks.
//
// The function assumes session beads are already synced (syncSessionBeads
// called before this function). When the bead reconciler is active,
// syncSessionBeads does NOT close orphan/suspended beads (skipClose=true),
// so the sessions slice may include beads with no matching desired entry.
// These are handled by the orphan/suspended drain phase.
//
// desiredState maps sessionName → TemplateParams for all agents that should
// be running. Built by buildDesiredState from config + scale_check results.
//
// configuredNames is the set of ALL configured agent session names (including
// suspended agents). Used to distinguish "orphaned" (removed from config)
// from "suspended" (still in config, not runnable) when closing beads.
//
// Returns the number of start attempts issued or enqueued this tick.
//
//nolint:unparam // compatibility wrapper retains the full production signature.
func reconcileSessionBeads(
	ctx context.Context,
	sessions []beads.Bead,
	desiredState map[string]TemplateParams,
	configuredNames map[string]bool,
	cfg *config.City,
	sp runtime.Provider,
	store beads.Store,
	dops drainOps,
	assignedWorkBeads []beads.Bead,
	readyWaitSet map[string]bool,
	dt *drainTracker,
	poolDesired map[string]int,
	storeQueryPartial bool,
	workSet map[string]bool,
	cityName string,
	it idleTracker,
	clk clock.Clock,
	rec events.Recorder,
	startupTimeout time.Duration,
	driftDrainTimeout time.Duration,
	stdout, stderr io.Writer,
) int {
	return reconcileSessionBeadsAtPath(
		ctx, "", sessions, desiredState, configuredNames, cfg, sp, store, dops, assignedWorkBeads, nil, readyWaitSet, dt,
		poolDesired, storeQueryPartial, workSet, cityName, it, clk, rec, startupTimeout, driftDrainTimeout, stdout, stderr,
	)
}

// reconcileSessionBeadsAtPath runs the reconciler for a specific city
// path. rigStores supplies the attached rig bead stores so live
// cross-store ownership checks (sessionHasOpenAssignedWork) can see
// work that lives outside the primary store. Pass nil when no rig
// stores are attached; the reconciler will fall back to primary-store-
// only queries.
//
//nolint:unparam // compatibility wrapper keeps the established test/helper signature.
func reconcileSessionBeadsAtPath(
	ctx context.Context,
	cityPath string,
	sessions []beads.Bead,
	desiredState map[string]TemplateParams,
	configuredNames map[string]bool,
	cfg *config.City,
	sp runtime.Provider,
	store beads.Store,
	dops drainOps,
	assignedWorkBeads []beads.Bead,
	rigStores map[string]beads.Store,
	readyWaitSet map[string]bool,
	dt *drainTracker,
	poolDesired map[string]int,
	storeQueryPartial bool,
	workSet map[string]bool,
	cityName string,
	it idleTracker,
	clk clock.Clock,
	rec events.Recorder,
	startupTimeout time.Duration,
	driftDrainTimeout time.Duration,
	stdout, stderr io.Writer,
) int {
	return reconcileSessionBeadsAtPathWithNamedDemand(
		ctx, cityPath, sessions, desiredState, configuredNames, cfg, sp, store, dops, assignedWorkBeads, rigStores, readyWaitSet, dt,
		poolDesired, nil, storeQueryPartial, workSet, cityName, it, clk, rec, startupTimeout, driftDrainTimeout, stdout, stderr,
	)
}

func reconcileSessionBeadsAtPathWithNamedDemand(
	ctx context.Context,
	cityPath string,
	sessions []beads.Bead,
	desiredState map[string]TemplateParams,
	configuredNames map[string]bool,
	cfg *config.City,
	sp runtime.Provider,
	store beads.Store,
	dops drainOps,
	assignedWorkBeads []beads.Bead,
	rigStores map[string]beads.Store,
	readyWaitSet map[string]bool,
	dt *drainTracker,
	poolDesired map[string]int,
	namedSessionDemand map[string]bool,
	storeQueryPartial bool,
	workSet map[string]bool,
	cityName string,
	it idleTracker,
	clk clock.Clock,
	rec events.Recorder,
	startupTimeout time.Duration,
	driftDrainTimeout time.Duration,
	stdout, stderr io.Writer,
) int {
	return reconcileSessionBeadsTracedWithNamedDemand(
		ctx, cityPath, sessions, desiredState, configuredNames, cfg, sp, store, dops, assignedWorkBeads, rigStores, readyWaitSet, dt,
		poolDesired, namedSessionDemand, storeQueryPartial, workSet, cityName, it, clk, rec, startupTimeout, driftDrainTimeout, stdout, stderr, nil,
	)
}

func reconcileSessionBeadsTraced(
	ctx context.Context,
	cityPath string,
	sessions []beads.Bead,
	desiredState map[string]TemplateParams,
	configuredNames map[string]bool,
	cfg *config.City,
	sp runtime.Provider,
	store beads.Store,
	dops drainOps,
	assignedWorkBeads []beads.Bead,
	rigStores map[string]beads.Store,
	readyWaitSet map[string]bool,
	dt *drainTracker,
	poolDesired map[string]int,
	storeQueryPartial bool,
	workSet map[string]bool,
	cityName string,
	it idleTracker,
	clk clock.Clock,
	rec events.Recorder,
	startupTimeout time.Duration,
	driftDrainTimeout time.Duration,
	stdout, stderr io.Writer,
	trace *sessionReconcilerTraceCycle,
	startOptions ...startExecutionOption,
) int {
	return reconcileSessionBeadsTracedWithNamedDemand(
		ctx, cityPath, sessions, desiredState, configuredNames, cfg, sp, store, dops, assignedWorkBeads, rigStores, readyWaitSet, dt,
		poolDesired, nil, storeQueryPartial, workSet, cityName, it, clk, rec, startupTimeout, driftDrainTimeout, stdout, stderr, trace,
		startOptions...,
	)
}

func reconcileSessionBeadsTracedWithNamedDemand(
	ctx context.Context,
	cityPath string,
	sessions []beads.Bead,
	desiredState map[string]TemplateParams,
	configuredNames map[string]bool,
	cfg *config.City,
	sp runtime.Provider,
	store beads.Store,
	dops drainOps,
	assignedWorkBeads []beads.Bead,
	rigStores map[string]beads.Store,
	readyWaitSet map[string]bool,
	dt *drainTracker,
	poolDesired map[string]int,
	namedSessionDemand map[string]bool,
	storeQueryPartial bool,
	workSet map[string]bool,
	cityName string,
	it idleTracker,
	clk clock.Clock,
	rec events.Recorder,
	startupTimeout time.Duration,
	driftDrainTimeout time.Duration,
	stdout, stderr io.Writer,
	trace *sessionReconcilerTraceCycle,
	startOptions ...startExecutionOption,
) int {
	if ctx != nil && ctx.Err() != nil {
		return 0
	}
	reconcileOpts := startExecutionOptions{}
	for _, apply := range startOptions {
		if apply != nil {
			apply(&reconcileOpts)
		}
	}
	maxAgeTr := reconcileOpts.maxSessionAgeTr
	deps := buildDepsMap(cfg)
	if cityName == "" {
		cityName = config.EffectiveCityName(cfg, "")
	}

	// Phase 0: Heal expired timers on all sessions.
	for i := range sessions {
		healExpiredTimers(&sessions[i], store, clk)
	}
	if cfg != nil {
		bySessionName := make(map[string]beads.Bead, len(sessions))
		indexBySessionName := make(map[string]int, len(sessions))
		for i, b := range sessions {
			if b.Status == "closed" {
				continue
			}
			if sn := strings.TrimSpace(b.Metadata["session_name"]); sn != "" {
				bySessionName[sn] = b
				indexBySessionName[sn] = i
			}
		}
		sessions = retireDuplicateConfiguredNamedSessionBeads(
			store, rigStores, sp, cfg, cityName, sessions, bySessionName, indexBySessionName, clk.Now().UTC(), stderr,
		)
	}

	// Topo-order sessions by template dependencies.
	ordered := topoOrder(sessions, deps)

	cbNow := clk.Now().UTC()
	cbCfg, cbEnabled := sessionCircuitBreakerConfigFromCity(cfg)
	var cb *sessionCircuitBreaker
	var circuitSessionByIdentity map[string]*beads.Bead
	if cbEnabled {
		// Phase 0.5: Feed the respawn circuit breaker persisted state and the
		// current progress signature for every named-session identity. A change
		// in the aggregate status of an identity's assigned work beads is treated
		// as an observable progress signal and keeps the breaker CLOSED even if
		// restarts accumulate. See session_circuit_breaker.go.
		cb = defaultSessionCircuitBreaker()
		cb.configure(cbCfg)
		circuitSessionByIdentity = make(map[string]*beads.Bead, len(ordered))
		for i := range ordered {
			identity := namedSessionIdentity(ordered[i])
			if identity == "" {
				continue
			}
			circuitSessionByIdentity[identity] = &ordered[i]
			if err := cb.observeResetGenerationFromMetadata(identity, ordered[i].Metadata); err != nil {
				fmt.Fprintf(stderr, "session reconciler: loading session circuit breaker reset generation for %s: %v\n", identity, err) //nolint:errcheck // best-effort stderr
			}
		}
		for i := range ordered {
			identity := namedSessionIdentity(ordered[i])
			if identity == "" {
				continue
			}
			if reset, err := cb.restoreFromMetadata(identity, ordered[i].Metadata, cbNow); err != nil {
				fmt.Fprintf(stderr, "session reconciler: loading session circuit breaker state for %s: %v\n", identity, err) //nolint:errcheck // best-effort stderr
			} else if reset {
				if err := persistSessionCircuitBreakerMetadata(store, &ordered[i], cb, identity, cbNow); err != nil {
					fmt.Fprintf(stderr, "session reconciler: %v\n", err) //nolint:errcheck // best-effort stderr
				}
			}
		}
		for identity, sig := range computeNamedSessionProgressSignatures(ordered, assignedWorkBeads) {
			if cb.ObserveProgressSignature(identity, sig, cbNow) {
				if session := circuitSessionByIdentity[identity]; session != nil {
					if err := persistSessionCircuitBreakerMetadata(store, session, cb, identity, cbNow); err != nil {
						fmt.Fprintf(stderr, "session reconciler: %v\n", err) //nolint:errcheck // best-effort stderr
					}
				}
			}
		}
		cb.pruneIdle(cbNow)
	}

	// Build session ID -> *beads.Bead lookup for advanceSessionDrains.
	// These pointers intentionally alias into the ordered slice so that
	// mutations in Phase 1 (healState, clearWakeFailures, etc.) are
	// visible to Phase 2's advanceSessionDrains via this map.
	beadByID := make(map[string]*beads.Bead, len(ordered))
	for i := range ordered {
		beadByID[ordered[i].ID] = &ordered[i]
	}

	// Phase 1: Forward pass (topo order) — wake sessions, handle alive state.
	var startCandidates []startCandidate
	var wakeTargets []wakeTarget
	// Rate-limit rollbacks per tick. Each rollbackPendingCreate fires three
	// bd subprocess calls (~2s each at the bd dolt-commit cost), so an
	// unbounded rollback storm easily blows the tick past
	// staleCreatingStateTimeout (60s) and starves executePlannedStartsTraced
	// — fresh pending-create beads age out before op=start fires. Capping
	// rollbacks per tick lets the rest of the tick make forward progress;
	// remaining stale beads roll back on subsequent ticks.
	const maxRollbacksPerTick = 5
	rollbacksThisTick := 0
	attemptRollbackPendingCreate := func(session *beads.Bead, templateName, name, action, detail string) {
		if rollbacksThisTick >= maxRollbacksPerTick {
			fmt.Fprintf(stderr, "session reconciler: deferring rollback of %s (%s): rollback budget exhausted this tick\n", name, detail) //nolint:errcheck
			if trace != nil {
				trace.recordDecision("reconciler.session.pending_create", templateName, name, action, "rollback_deferred", traceRecordPayload{
					"rollbacks_this_tick":    rollbacksThisTick,
					"max_rollbacks_per_tick": maxRollbacksPerTick,
				}, nil, "")
			}
			return
		}
		rollbacksThisTick++
		fmt.Fprintf(stderr, "session reconciler: rolling back pending create %s: %s\n", name, detail) //nolint:errcheck
		if trace != nil {
			trace.recordDecision("reconciler.session.pending_create", templateName, name, action, "rollback", nil, nil, "")
		}
		rollbackPendingCreate(session, store, clk.Now().UTC(), stderr)
	}
	for i := range ordered {
		if ctx != nil && ctx.Err() != nil {
			return 0
		}
		session := &ordered[i]

		// Skip beads with unrecognized states. This enables forward-compatible
		// rollback: if a newer version writes "draining" or "archived", the
		// older reconciler ignores those beads rather than crashing.
		if !isKnownState(*session) {
			fmt.Fprintf(stderr, "session reconciler: skipping %s with unknown state %q\n", //nolint:errcheck // best-effort stderr
				session.Metadata["session_name"], session.Metadata["state"])
			if trace != nil {
				trace.recordDecision("reconciler.session.unknown_state", session.Metadata["template"], session.Metadata["session_name"], "unknown_state_skipped", "skipped", traceRecordPayload{
					"state": session.Metadata["state"],
				}, nil, "")
			}
			continue
		}

		name := strings.TrimSpace(session.Metadata["session_name"])
		tp, desired := desiredState[name]

		// Orphan/suspended: bead exists but not in desired state.
		// Handle BEFORE heal/stability to avoid false crash detection —
		// a running session that leaves the desired set is not a crash.
		if !desired {
			providerAlive, err := workerSessionTargetRunningWithConfig(cityPath, store, sp, cfg, session.ID)
			if err != nil {
				providerAlive = false
			}
			preserveNamed := preserveConfiguredNamedSessionBead(*session, cfg, cityName)
			var (
				preservedTP  TemplateParams
				preserveErr  error
				rateLimitHit bool
				rateLimitErr error
			)
			if preserveNamed {
				preservedTP, preserveErr = resolvePreservedConfiguredNamedSessionTemplate(cityPath, cityName, cfg, sp, store, ordered, *session, clk, stderr)
				if preserveErr == nil {
					obs, obsErr := workerObserveSessionTargetWithRuntimeHintsWithConfig(cityPath, store, sp, cfg, session.ID, preservedTP.Hints.ProcessNames)
					rateLimitAlive := rateLimitAliveFromObservation(obs.Alive, obsErr)
					peek := cachedSessionPeek(cityPath, store, sp, cfg, session.ID, preservedTP.Hints.ProcessNames)
					rateLimitHit, rateLimitErr = checkRateLimitStability(session, cfg, rateLimitAlive, dt, store, clk, peek)
				}
			}
			if rateLimitHit || rateLimitErr != nil {
				if trace != nil {
					template := normalizedSessionTemplate(*session, cfg)
					if template == "" {
						template = session.Metadata["template"]
					}
					result := "held"
					if rateLimitErr != nil {
						result = "hold_deferred"
					}
					trace.recordDecision("reconciler.session.preserve_configured_named", template, name, "rate_limit", result, traceRecordPayload{
						"provider_alive": providerAlive,
					}, nil, "")
				}
				continue
			}
			// Heal state using provider liveness, not agent membership.
			healState(session, providerAlive, store, clk)
			switch {
			case preserveNamed:
				template := normalizedSessionTemplate(*session, cfg)
				if template == "" {
					template = session.Metadata["template"]
				}
				switch {
				case preserveErr != nil:
					fmt.Fprintf(stderr, "session reconciler: resolve preserved named session %s: %v\n", name, preserveErr) //nolint:errcheck
				default:
					tp = preservedTP
					desired = true
				}
				if trace != nil {
					trace.recordDecision("reconciler.session.preserve_configured_named", template, name, "preserve", map[bool]string{
						true:  "kept_open",
						false: "resolution_failed",
					}[desired], traceRecordPayload{
						"provider_alive": providerAlive,
						"degraded":       preserveErr != nil,
					}, nil, "")
				}
			case pendingCreateSessionStillLeased(*session, cfg, clk):
				template := normalizedSessionTemplate(*session, cfg)
				if template == "" {
					template = session.Metadata["template"]
				}
				if trace != nil {
					trace.recordDecision("reconciler.session.pending_create_preserved", template, name, "pending_create", "kept_open", traceRecordPayload{
						"pending_create_claim": strings.TrimSpace(session.Metadata["pending_create_claim"]),
						"provider_alive":       providerAlive,
						"state":                session.Metadata["state"],
					}, nil, "")
				}
				continue
			default:
				if dops != nil {
					if acked, _ := dops.isDrainAcked(name); acked {
						hasAssignedWork, assignedErr := sessionHasOpenAssignedWorkForReachableStore(cityPath, cfg, store, rigStores, *session)
						if assignedErr != nil {
							fmt.Fprintf(stderr, "session reconciler: checking assigned work for drain-acked %s: %v\n", name, assignedErr) //nolint:errcheck
							hasAssignedWork = true
						}
						if providerAlive && hasAssignedWork {
							if cancelOrphanedSessionDrainForAssignedWork(*session, sp, dt) ||
								cancelRecoveredOrphanedDrainForAssignedWork(*session, sp, name) {
								_ = dops.clearDrain(name)
								template := normalizedSessionTemplate(*session, cfg)
								if template == "" {
									template = session.Metadata["template"]
								}
								fmt.Fprintf(stdout, "Canceled drain-acked session '%s' (assigned work)\n", name) //nolint:errcheck
								if trace != nil {
									trace.recordDecision("reconciler.drain.cancel", template, name, "orphaned", "cancel_assigned_work", nil, nil, "")
								}
								continue
							}
						}
						stopped := !providerAlive
						if providerAlive {
							if err := workerKillSessionTargetWithConfig("", store, sp, cfg, name); err != nil {
								fmt.Fprintf(stderr, "session reconciler: stopping drain-acked %s: %v\n", name, err) //nolint:errcheck
							} else {
								stopped = true
								fmt.Fprintf(stdout, "Stopped drain-acked session '%s'\n", name) //nolint:errcheck
							}
						}
						if stopped {
							template := normalizedSessionTemplate(*session, cfg)
							if template == "" {
								template = session.Metadata["template"]
							}
							rec.Record(events.Event{
								Type:    events.SessionStopped,
								Actor:   "gc",
								Subject: template,
								Message: "drain acknowledged by agent",
								Payload: api.SessionLifecyclePayloadJSON(session.ID, template, "drain acknowledged"),
							})
							if hasAssignedWork {
								batch := sessionpkg.CompleteDrainPatch(clk.Now().UTC(), "idle", session.Metadata["wake_mode"] == "fresh")
								_ = store.SetMetadataBatch(session.ID, batch)
								if session.Metadata == nil {
									session.Metadata = make(map[string]string, len(batch))
								}
								for key, value := range batch {
									session.Metadata[key] = value
								}
								_ = dops.clearDrain(name)
								if dt != nil {
									dt.clearIdleProbe(session.ID)
									dt.remove(session.ID)
								}
								continue
							}
							_ = dops.clearDrain(name)
							if dt != nil {
								dt.clearIdleProbe(session.ID)
								dt.remove(session.ID)
							}
							if closeSessionBeadIfReachableStoreUnassigned(cityPath, cfg, store, rigStores, *session, "drained", clk.Now().UTC(), stderr) {
								session.Status = "closed"
							}
						}
						continue
					}
				}
				if providerAlive {
					// When a store query failed (partial results),
					// skip drain — the session may have work that we
					// couldn't see due to the transient failure.
					// Draining would send Ctrl-C and interrupt the
					// running agent mid-tool-call.
					if storeQueryPartial {
						fmt.Fprintf(stdout, "Skipping drain for '%s': store query partial (transient failure)\n", name) //nolint:errcheck
						continue
					}
					reason := "orphaned"
					if configuredNames[name] {
						reason = "suspended"
					}
					hasAssignedWork, assignedErr := sessionHasOpenAssignedWork(store, rigStores, *session)
					if assignedErr != nil {
						fmt.Fprintf(stderr, "session reconciler: checking assigned work before %s drain for %s: %v\n", reason, name, assignedErr) //nolint:errcheck
						continue
					}
					if hasAssignedWork {
						if trace != nil {
							template := normalizedSessionTemplate(*session, cfg)
							if template == "" {
								template = session.Metadata["template"]
							}
							trace.recordDecision("reconciler.session.orphan_or_suspended", template, name, reason, "kept_open", traceRecordPayload{
								"store_query_partial": storeQueryPartial,
								"provider_alive":      providerAlive,
								"live_assigned_work":  true,
							}, nil, "")
						}
						fmt.Fprintf(stdout, "Skipping drain for '%s': live assigned work found\n", name) //nolint:errcheck
						continue
					}
					if beginSessionDrain(*session, sp, dt, reason, clk, defaultDrainTimeout) {
						if trace != nil {
							template := normalizedSessionTemplate(*session, cfg)
							if template == "" {
								template = session.Metadata["template"]
							}
							trace.recordDecision("reconciler.session.orphan_or_suspended", template, name, reason, "drain", traceRecordPayload{
								"store_query_partial": storeQueryPartial,
								"provider_alive":      providerAlive,
							}, nil, "")
						}
						fmt.Fprintf(stdout, "Draining session '%s': %s\n", name, reason) //nolint:errcheck
					}
				} else {
					// Not running and not desired — close the bead.
					reason := "orphaned"
					if configuredNames[name] {
						reason = "suspended"
					}
					template := normalizedSessionTemplate(*session, cfg)
					if template == "" {
						template = session.Metadata["template"]
					}
					if trace != nil {
						trace.recordDecision("reconciler.session.close_orphan", template, name, reason, "closed", nil, nil, "")
					}
					if storeQueryPartial {
						continue
					}
					if closeSessionBeadIfReachableStoreUnassigned(cityPath, cfg, store, rigStores, *session, reason, clk.Now().UTC(), stderr) {
						session.Status = "closed"
					}
				}
				continue
			}
		}

		// Liveness includes zombie detection: tmux session exists AND
		// the expected child process is alive (when ProcessNames configured).
		obs, err := workerObserveSessionTargetWithRuntimeHintsWithConfig(cityPath, store, sp, cfg, session.ID, tp.Hints.ProcessNames)
		if err != nil {
			obs = worker.LiveObservation{}
		}
		running := obs.Running
		alive := obs.Alive
		peek := cachedSessionPeek(cityPath, store, sp, cfg, session.ID, tp.Hints.ProcessNames)

		// Zombie capture: session exists but process dead — grab scrollback for forensics.
		if running && !alive {
			if output, err := peek(rateLimitPeekLines); err == nil && output != "" {
				if !runtime.ContainsProviderRateLimitScreen(output) {
					rec.Record(events.Event{
						Type:    events.SessionCrashed,
						Actor:   "gc",
						Subject: tp.DisplayName(),
						Message: output,
						Payload: api.SessionLifecyclePayloadJSON(session.ID, tp.TemplateName, "zombie process"),
					})
					telemetry.RecordAgentCrash(context.Background(), tp.DisplayName(), output)
				}
			}
		}
		if alive && shouldRollbackPendingCreate(session) && !runningSessionMatchesPendingCreate(session, name, sp) {
			attemptRollbackPendingCreate(session, tp.TemplateName, name, "pending_create_rollback", "live runtime belongs to another session")
			continue
		}
		// Desired-branch counterpart to pendingCreateSessionStillLeased: a
		// session bead in the desired set with pending_create_claim=true but
		// no live runtime AND no active lease is stuck. Without this rollback,
		// the bead lives forever holding its alias, blocking new spawn
		// attempts ("alias already belongs to gm-XXXX") for any session whose
		// template still has demand. Rolling back closes the dead bead so the
		// next reconciler tick can allocate a fresh slot under the same alias.
		if !alive && shouldRollbackPendingCreate(session) {
			var startupTimeout time.Duration
			if cfg != nil {
				startupTimeout = cfg.Session.StartupTimeoutDuration()
			}
			if pendingCreateLeaseExpiredForRollback(*session, clk, startupTimeout) {
				rateLimitHit, rateLimitErr := checkRateLimitStability(session, cfg, alive, dt, store, clk, peek)
				if rateLimitHit || rateLimitErr != nil {
					continue
				}
				attemptRollbackPendingCreate(session, tp.TemplateName, name, "pending_create_lease_expired", "lease expired and no live runtime")
				continue
			}
		}

		// Drain-ack: agent signaled it's done (gc runtime drain-ack).
		// Honor the ack even if the agent exited before this tick; otherwise
		// the session falls through to orphan handling and can block the next
		// worker wave until the stale awake bead ages out.
		if dops != nil {
			if acked, _ := dops.isDrainAcked(name); acked {
				if !alive && staleOrLegacyDrainAckBeforeStart(*session, sp, name) {
					clearReconcilerDrainAckMetadata(sp, name)
				} else {
					if staleReconcilerDrainAck(*session, sp, name) {
						clearReconcilerDrainAckMetadata(sp, name)
						if trace != nil {
							trace.recordDecision("reconciler.session.drain_ack", tp.TemplateName, name, "stale_generation", "clear", nil, nil, "")
						}
						continue
					}
					ackReason, reconcilerOwnedAck := reconcilerDrainAckMatchesSession(*session, sp, name)
					if reconcilerOwnedAck && ackReason == "config-drift" {
						driftKey := sessionConfigDriftKey(*session, cfg, tp)
						attached, attachErr := sessionAttachedForConfigDrift(*session, sp, cityPath, store, cfg, name)
						if attachErr != nil {
							fmt.Fprintf(stderr, "session reconciler: observing config-drift attachment for %s: %v\n", name, attachErr) //nolint:errcheck
						}
						if attached {
							if driftKey != "" {
								if err := recordSessionAttachedConfigDriftDeferral(*session, store, clk, driftKey); err != nil {
									fmt.Fprintf(stderr, "session reconciler: recording attached config-drift deferral for %s: %v\n", name, err) //nolint:errcheck
								}
							}
							drainCancelled := cancelSessionConfigDriftDrain(*session, sp, dt)
							if !drainCancelled {
								clearReconcilerDrainAckMetadata(sp, name)
							}
							if trace != nil {
								trace.recordDecision("reconciler.session.drain_ack", tp.TemplateName, name, "config_drift_attached", "cancel_reconciler_ack", traceRecordPayload{
									"drain_canceled": drainCancelled,
								}, nil, "")
							}
							continue
						}
						if driftKey != "" && recentlyDeferredSessionAttachedConfigDrift(*session, clk, driftKey) {
							drainCancelled := cancelSessionConfigDriftDrain(*session, sp, dt)
							if !drainCancelled {
								clearReconcilerDrainAckMetadata(sp, name)
							}
							if trace != nil {
								trace.recordDecision("reconciler.session.drain_ack", tp.TemplateName, name, "config_drift_recently_attached", "cancel_reconciler_ack", traceRecordPayload{
									"drain_canceled": drainCancelled,
								}, nil, "")
							}
							continue
						}
					}
					if pendingInteractionKeepsAwake(*session, sp, name, clk) &&
						(cancelReconcilerAckedDrain(*session, sp, dt) || cancelRecoveredReconcilerAckedDrain(*session, sp, name)) {
						if trace != nil {
							trace.recordDecision("reconciler.session.drain_ack", tp.TemplateName, name, "pending", "cancel_reconciler_ack", nil, nil, "")
						}
						continue
					}
					stopped := !alive // already dead = effectively stopped
					if alive {
						if err := workerKillSessionTargetWithConfig("", store, sp, cfg, name); err != nil {
							fmt.Fprintf(stderr, "session reconciler: stopping drain-acked %s: %v\n", name, err) //nolint:errcheck
							if !reconcilerOwnedAck && dt != nil {
								dt.clearIdleProbe(session.ID)
								dt.remove(session.ID)
							}
						} else {
							stopped = true
							fmt.Fprintf(stdout, "Stopped drain-acked session '%s'\n", name) //nolint:errcheck
						}
					}
					if stopped && store != nil && session.ID != "" {
						_ = dops.clearDrain(name)
						rec.Record(events.Event{
							Type:    events.SessionStopped,
							Actor:   "gc",
							Subject: tp.DisplayName(),
							Message: "drain acknowledged by agent",
							Payload: api.SessionLifecyclePayloadJSON(session.ID, tp.TemplateName, "drain acknowledged"),
						})
						// Drain-ack lands here right after the agent ran
						// `bd close` on its last unit of work. The cached
						// `ownershipWorkBeads` snapshot taken earlier in
						// this tick predates that close, so it still shows
						// the bead as open+assigned and falsely flipped
						// pool workers into CompleteDrainPatch
						// (state=asleep + sleep_reason=idle) instead of
						// AcknowledgeDrainPatch (state=drained). That hid
						// the bead from the close gate and stranded new
						// queue work on a ghost slot. Re-query the store
						// so the decision reflects reality.
						hasAssignedWork, assignedErr := sessionHasOpenAssignedWorkForReachableStore(cityPath, cfg, store, rigStores, *session)
						sleepReason := "idle"
						if assignedErr != nil {
							fmt.Fprintf(stderr, "session reconciler: checking assigned work for drain-acked %s: %v\n", name, assignedErr) //nolint:errcheck
							hasAssignedWork = true
						}
						batch := sessionpkg.AcknowledgeDrainPatch(session.Metadata["wake_mode"] == "fresh")
						if hasAssignedWork {
							batch = sessionpkg.CompleteDrainPatch(clk.Now().UTC(), sleepReason, session.Metadata["wake_mode"] == "fresh")
						}
						_ = store.SetMetadataBatch(session.ID, batch)
						if session.Metadata == nil {
							session.Metadata = make(map[string]string, len(batch))
						}
						for key, value := range batch {
							session.Metadata[key] = value
						}
						if !reconcilerOwnedAck && dt != nil {
							dt.clearIdleProbe(session.ID)
							dt.remove(session.ID)
						}
					}
					continue
				}
			}
		}

		policy := resolveSessionSleepPolicy(*session, cfg, sp)

		rateLimitHit, rateLimitErr := checkRateLimitStability(session, cfg, alive, dt, store, clk, peek)
		if rateLimitHit || rateLimitErr != nil {
			continue // rate-limit hold recorded before state healing resets continuity metadata
		}

		// Heal advisory state metadata.
		stateBeforeHeal := sessionpkg.State(strings.TrimSpace(session.Metadata["state"]))
		healState(session, alive, store, clk)
		if recoverPendingIdleSleep(session, store, running, clk) {
			alive = false
		}
		reconcileDetachedAt(session, store, policy, alive, sp, clk)

		// Stability check: detect rapid crash after state healing. Rate-limit
		// detection intentionally ran above before healState.
		if checkStability(session, cfg, alive, dt, store, clk, nil) {
			continue // rapid exit recorded, skip further processing
		}

		// Churn check: detect context exhaustion death spiral.
		// Fires for sessions that survived past stabilityThreshold but
		// died before churnProductivityThreshold — alive long enough to
		// not be a rapid crash, but too short to be productive.
		if checkChurn(session, cfg, alive, dt, store, clk) {
			continue // churn recorded, skip further processing
		}

		// Clear wake failures for sessions that have been stable long enough.
		if alive && stableLongEnough(*session, clk) {
			clearWakeFailures(session, store)
		}
		// Clear churn counter for sessions that have been productive.
		if alive && productiveLongEnough(*session, clk) {
			clearChurn(session, store)
		}
		if alive && shouldRollbackPendingCreate(session) {
			if stateBeforeHeal == sessionpkg.StateCreating && pendingCreateStartInFlight(*session, clk, startupTimeout) {
				if trace != nil {
					trace.recordDecision("reconciler.session.pending_create", tp.TemplateName, name, "pending_create_recovery_in_flight", "deferred", nil, nil, "")
				}
				continue
			}
			if !recoverRunningPendingCreate(session, tp, cfg, store, clk, trace) {
				fmt.Fprintf(stderr, "session reconciler: recovering pending create %s: metadata repair incomplete\n", name) //nolint:errcheck
			}
		}

		// Restart-requested: agent asked for a fresh session
		// (gc runtime request-restart / gc handoff). Rotate session_key
		// to a fresh value and clear started_config_hash so the next wake
		// builds a first-start command (--session-id <new_key>). Also set
		// continuation_reset_pending so the next wake bumps the continuation
		// epoch instead of silently reusing the prior continuation lineage.
		// Then stop immediately; the next tick will re-create and re-wake.
		//
		// Check both tmux metadata (dops) and bead metadata. The bead
		// metadata flag survives tmux session death, so this works even
		// when the session is already dead.
		{
			tmuxRequested := false
			if alive && dops != nil {
				tmuxRequested, _ = dops.isRestartRequested(name)
			}
			beadRequested := session.Metadata["restart_requested"] == "true"
			if tmuxRequested || beadRequested {
				if alive {
					if err := workerKillSessionTargetWithConfig("", store, sp, cfg, name); err != nil {
						fmt.Fprintf(stderr, "session reconciler: stopping restart-requested %s: %v\n", name, err) //nolint:errcheck
						continue
					}
				}
				// Providers that can inject a fresh session ID get a
				// rotated key here so the next wake starts a brand-new
				// conversation. Providers without SessionIDFlag must
				// clear any stored key and wake fresh without resume.
				// Clearing started_config_hash forces firstStart=true in
				// resolveSessionCommand. Clearing last_woke_at masks the
				// intentional death from crash and churn trackers (both
				// check last_woke_at first).
				newSessionKey, hasCapability := freshRestartSessionKey(tp, session.Metadata)
				batch := sessionpkg.RestartRequestPatch(newSessionKey)
				if hasCapability && newSessionKey == "" {
					batch["session_key"] = ""
				}
				if err := store.SetMetadataBatch(session.ID, batch); err != nil {
					fmt.Fprintf(stderr, "session reconciler: recording restart handoff for %s: %v\n", name, err) //nolint:errcheck
					continue
				}
				if session.Metadata == nil {
					session.Metadata = make(map[string]string, len(batch))
				}
				for key, value := range batch {
					session.Metadata[key] = value
				}
				if alive {
					if tmuxRequested && dops != nil {
						_ = dops.clearRestartRequested(name)
					}
					fmt.Fprintf(stdout, "Stopped restart-requested session '%s'\n", name) //nolint:errcheck
				}
				continue
			}
		}

		// Config drift: if alive and config changed, drain for restart.
		// Live-only drift: re-apply session_live without restart.
		if alive {
			template := tp.TemplateName
			if template == "" {
				template = normalizedSessionTemplate(*session, cfg)
			}
			// Use started_config_hash for drift detection — it records
			// what config the session actually started with. Before it's
			// written (during the startup window), skip the drift check
			// to avoid false-positive drains. Fixes #127.
			storedHash := session.Metadata["started_config_hash"]
			if template != "" && storedHash != "" {
				cfgAgent := findAgentByTemplate(cfg, template)
				if cfgAgent != nil {
					agentCfg := templateParamsToConfig(tp)
					// Apply template_overrides using the same resolution as
					// prepareSessionStart: merge defaults + overrides, then
					// replaceSchemaFlags to strip and re-add all schema flags.
					applyTemplateOverridesToConfig(&agentCfg, *session, tp)
					currentHash := runtime.CoreFingerprint(agentCfg)
					if storedHash != currentHash {
						fmt.Fprintf(stderr, "config-drift %s: stored=%s current=%s cmd=%q\n", name, storedHash[:12], currentHash[:12], agentCfg.Command) //nolint:errcheck
						// Diagnostic: log per-field breakdown to identify the drifting field.
						var storedBreakdown map[string]string
						if raw := session.Metadata["core_hash_breakdown"]; raw != "" {
							_ = json.Unmarshal([]byte(raw), &storedBreakdown)
						}
						driftedFields := runtime.CoreFingerprintDriftFields(storedBreakdown, agentCfg)
						runtime.LogCoreFingerprintDrift(stderr, name, storedBreakdown, agentCfg)
						restartedInPlace := false
						// Attached sessions never get config-drift restarts.
						// The human will restart when ready; drift applies
						// after detach. Checked before named/non-named paths
						// because named session config drift is an immediate
						// kill; a single transient IsAttached false negative
						// would destroy conversation context irreversibly.
						driftKey := storedHash + ":" + currentHash
						attached, attachErr := sessionAttachedForConfigDrift(*session, sp, cityPath, store, cfg, name)
						if attachErr != nil {
							fmt.Fprintf(stderr, "session reconciler: observing config-drift attachment for %s: %v\n", name, attachErr) //nolint:errcheck
						}
						if attached {
							if err := recordSessionAttachedConfigDriftDeferral(*session, store, clk, driftKey); err != nil {
								fmt.Fprintf(stderr, "session reconciler: recording attached config-drift deferral for %s: %v\n", name, err) //nolint:errcheck
							}
							drainCancelled := cancelSessionConfigDriftDrain(*session, sp, dt)
							if trace != nil {
								trace.recordDecision("reconciler.session.config_drift", tp.TemplateName, name, "config_drift", string(TraceOutcomeDeferredAttached), configDriftTracePayload(storedHash, currentHash, driftedFields, traceRecordPayload{
									"active_reason":  "attached",
									"drain_canceled": drainCancelled,
								}), nil, "")
							}
							continue
						}
						if recentlyDeferredSessionAttachedConfigDrift(*session, clk, driftKey) {
							if trace != nil {
								trace.recordDecision("reconciler.session.config_drift", tp.TemplateName, name, "config_drift", string(TraceOutcomeDeferredAttached), configDriftTracePayload(storedHash, currentHash, driftedFields, traceRecordPayload{
									"active_reason": "attached_recently",
								}), nil, "")
							}
							continue
						}
						if isNamedSessionBead(*session) {
							// Defer config-drift restart for named sessions
							// that are actively in use (pending interaction,
							// tmux-attached, or recent activity). This prevents
							// draining a working agent mid-task without graceful
							// handoff. See gastownhall/gascity#119.
							activeReason, active, deferErr := shouldDeferNamedSessionConfigDrift(*session, store, sp, name, clk, driftKey)
							if deferErr != nil {
								fmt.Fprintf(stderr, "session reconciler: recording config-drift deferral for %s: %v\n", name, deferErr) //nolint:errcheck
							}
							if active {
								if trace != nil {
									trace.recordDecision("reconciler.session.config_drift", tp.TemplateName, name, "config_drift", string(TraceOutcomeDeferredActive), configDriftTracePayload(storedHash, currentHash, driftedFields, traceRecordPayload{
										"active_reason": activeReason,
									}), nil, "")
								}
								continue
							}
							resetConfiguredNamedSessionForConfigDrift(session, store, sp, name, alive, "creating", clk.Now().UTC(), stderr)
							if trace != nil {
								trace.recordDecision("reconciler.session.config_drift", tp.TemplateName, name, "config_drift", "restart_in_place", configDriftTracePayload(storedHash, currentHash, driftedFields, nil), nil, "")
							}
							rec.Record(events.Event{
								Type:    events.SessionDraining,
								Actor:   "gc",
								Subject: tp.DisplayName(),
								Message: "config drift detected",
							})
							alive = false
							restartedInPlace = true
						}
						if !restartedInPlace {
							// Defer ordinary-session config-drift drain while a
							// user is attached. Named-session config drift is
							// deferred when actively in use (see above).
							if pendingInteractionKeepsAwake(*session, sp, name, clk) {
								drainCancelled := false
								if dt != nil {
									drainCancelled = cancelSessionDrainForPending(*session, sp, dt)
								}
								if trace != nil {
									trace.recordDecision("reconciler.session.config_drift", tp.TemplateName, name, "pending", "deferred_pending", configDriftTracePayload(storedHash, currentHash, driftedFields, traceRecordPayload{
										"drain_canceled": drainCancelled,
									}), nil, "")
								}
								continue
							}
							ddt := driftDrainTimeout
							if ddt <= 0 {
								ddt = defaultDrainTimeout
							}
							if beginSessionDrain(*session, sp, dt, "config-drift", clk, ddt) {
								fmt.Fprintf(stdout, "Draining session '%s': config-drift\n", name) //nolint:errcheck
								if trace != nil {
									trace.recordDecision("reconciler.session.config_drift", tp.TemplateName, name, "config_drift", "drain", configDriftTracePayload(storedHash, currentHash, driftedFields, nil), nil, "")
								}
								rec.Record(events.Event{
									Type:    events.SessionDraining,
									Actor:   "gc",
									Subject: tp.DisplayName(),
									Message: "config drift detected",
								})
							}
							continue
						}
					}

					if err := clearSessionConfigDriftDeferral(*session, store); err != nil {
						fmt.Fprintf(stderr, "session reconciler: clearing config-drift deferral for %s: %v\n", name, err) //nolint:errcheck
					}

					// Core config matches — check live-only drift.
					// Use started_live_hash exclusively, matching
					// the started_config_hash pattern above.
					storedLive := session.Metadata["started_live_hash"]
					currentLive := runtime.LiveFingerprint(agentCfg)
					if storedLive != currentLive {
						if storedLive == "" && len(agentCfg.SessionLive) == 0 {
							// No stored hash and no live config — silently
							// backfill the hash without running anything.
							_ = store.SetMetadataBatch(session.ID, map[string]string{
								"live_hash":         currentLive,
								"started_live_hash": currentLive,
							})
						} else {
							fmt.Fprintf(stdout, "Live config changed for '%s', re-applying...\n", tp.DisplayName()) //nolint:errcheck
							if err := sp.RunLive(name, agentCfg); err != nil {
								fmt.Fprintf(stderr, "session reconciler: RunLive %s: %v\n", name, err) //nolint:errcheck
							} else {
								_ = store.SetMetadataBatch(session.ID, map[string]string{
									"live_hash":         currentLive,
									"started_live_hash": currentLive,
								})
								rec.Record(events.Event{
									Type:    events.SessionUpdated,
									Actor:   "gc",
									Subject: tp.DisplayName(),
									Message: "session_live re-applied",
								})
							}
						}
					}
				}
			}
		}

		if !alive && isNamedSessionBead(*session) {
			template := tp.TemplateName
			if template == "" {
				template = normalizedSessionTemplate(*session, cfg)
			}
			storedHash := session.Metadata["started_config_hash"]
			if template != "" && storedHash != "" {
				if cfgAgent := findAgentByTemplate(cfg, template); cfgAgent != nil {
					agentCfg := templateParamsToConfig(tp)
					currentHash := runtime.CoreFingerprint(agentCfg)
					if storedHash != currentHash {
						var storedBreakdown map[string]string
						if raw := session.Metadata["core_hash_breakdown"]; raw != "" {
							_ = json.Unmarshal([]byte(raw), &storedBreakdown)
						}
						driftedFields := runtime.CoreFingerprintDriftFields(storedBreakdown, agentCfg)
						resetConfiguredNamedSessionForConfigDrift(session, store, sp, name, false, "asleep", clk.Now().UTC(), stderr)
						if trace != nil {
							trace.recordDecision("reconciler.session.config_drift", tp.TemplateName, name, "config_drift", "repair_in_place", configDriftTracePayload(storedHash, currentHash, driftedFields, nil), nil, "")
						}
						continue
					}
				}
			}
		}

		// Preemptive max session age: restart sessions whose wall-clock age
		// exceeds the agent's max_session_age threshold. Motivation: provider
		// SDKs that cache credentials at session start (e.g., Claude Code via
		// Bedrock) can wedge silently when the underlying token expires. This
		// is session age, not idle time — a busy session is still subject to
		// the threshold — but the restart is skipped while the agent is
		// mid-turn (pending interaction) or holds an open assigned work bead,
		// so no work is lost mid-flight. The next tick retries.
		if maxAgeTr != nil && alive {
			creationCompleteAt, hasAnchor := parseRFC3339Metadata(session.Metadata["creation_complete_at"])
			if hasAnchor && maxAgeTr.shouldRestart(name, creationCompleteAt, clk.Now()) {
				if pendingInteractionKeepsAwake(*session, sp, name, clk) {
					if trace != nil {
						trace.recordDecision("reconciler.session.max_session_age", tp.TemplateName, name, "pending", "deferred_pending", nil, nil, "")
					}
				} else {
					hasWork, assignedErr := sessionHasOpenAssignedWorkForReachableStore(cityPath, cfg, store, rigStores, *session)
					if assignedErr != nil {
						// Fail closed: treat error as "has work" so a transient
						// store blip doesn't kill a session that may still hold
						// in-flight work. Mirrors the drain-ack path above.
						fmt.Fprintf(stderr, "session reconciler: checking assigned work for max-age %s: %v\n", name, assignedErr) //nolint:errcheck // best-effort stderr
						hasWork = true
					}
					if hasWork {
						if trace != nil {
							trace.recordDecision("reconciler.session.max_session_age", tp.TemplateName, name, "assigned_work", "deferred_busy", nil, nil, "")
						}
					} else {
						fmt.Fprintf(stderr, "session reconciler: preemptive max-age restart for %s (age=%s)\n", tp.DisplayName(), clk.Now().Sub(creationCompleteAt).Round(time.Second)) //nolint:errcheck // best-effort stderr
						if trace != nil {
							trace.recordDecision("reconciler.session.max_session_age", tp.TemplateName, name, "max_session_age", "stop", nil, nil, "")
						}
						if err := workerKillSessionTargetWithConfig("", store, sp, cfg, name); err != nil {
							fmt.Fprintf(stderr, "session reconciler: stopping aged %s: %v\n", name, err) //nolint:errcheck // best-effort stderr
						} else {
							_ = sp.ClearScrollback(name)
							rec.Record(events.Event{
								Type:    events.SessionMaxAgeKilled,
								Actor:   "gc",
								Subject: tp.DisplayName(),
							})
							telemetry.RecordAgentMaxAgeKill(context.Background(), tp.DisplayName())
							batch := sessionpkg.SleepPatch(clk.Now(), "max-session-age")
							_ = store.SetMetadataBatch(session.ID, batch)
							if session.Metadata == nil {
								session.Metadata = make(map[string]string, len(batch))
							}
							for key, value := range batch {
								session.Metadata[key] = value
							}
							alive = false
						}
					}
				}
			}
		}

		// Idle timeout: restart sessions idle longer than configured threshold.
		if it != nil && alive && it.checkIdle(name, sp, clk.Now()) {
			if pendingInteractionKeepsAwake(*session, sp, name, clk) {
				drainCancelled := false
				if dt != nil {
					drainCancelled = cancelSessionDrain(*session, sp, dt)
				}
				if trace != nil {
					trace.recordDecision("reconciler.session.idle_timeout", tp.TemplateName, name, "pending", "deferred_pending", traceRecordPayload{
						"drain_canceled": drainCancelled,
					}, nil, "")
				}
				continue
			}
			fmt.Fprintf(stderr, "session reconciler: idle timeout for %s\n", tp.DisplayName()) //nolint:errcheck // best-effort stderr
			if trace != nil {
				trace.recordDecision("reconciler.session.idle_timeout", tp.TemplateName, name, "idle_timeout", "stop", nil, nil, "")
			}
			if err := workerKillSessionTargetWithConfig("", store, sp, cfg, name); err != nil {
				fmt.Fprintf(stderr, "session reconciler: stopping idle %s: %v\n", name, err) //nolint:errcheck // best-effort stderr
			} else {
				_ = sp.ClearScrollback(name)
				rec.Record(events.Event{
					Type:    events.SessionIdleKilled,
					Actor:   "gc",
					Subject: tp.DisplayName(),
				})
				telemetry.RecordAgentIdleKill(context.Background(), tp.DisplayName())
				// Mark for immediate re-wake on this same tick by clearing
				// last_woke_at and setting state to asleep. The wake logic
				// below will pick it up.
				batch := sessionpkg.SleepPatch(clk.Now(), "idle-timeout")
				_ = store.SetMetadataBatch(session.ID, batch)
				if session.Metadata == nil {
					session.Metadata = make(map[string]string, len(batch))
				}
				for key, value := range batch {
					session.Metadata[key] = value
				}
				alive = false
			}
			// Fall through to wakeReasons — it will re-wake immediately if config present
		}

		wakeTargets = append(wakeTargets, wakeTarget{session: session, tp: tp, alive: alive})
	}

	if ctx != nil && ctx.Err() != nil {
		return 0
	}

	// Use ComputeAwakeSet for the wake/sleep decision.
	awakeInput := buildAwakeInputFromReconciler(
		cfg, ordered, poolDesired, namedSessionDemand, workSet, readyWaitSet,
		assignedWorkBeads, wakeTargets, sp, clk.Now(),
	)
	awakeDecisions := ComputeAwakeSet(awakeInput)
	wakeEvals := awakeSetToWakeEvals(awakeDecisions, awakeInput.SessionBeads)

	// Resolve full sleep policies before idle probe selection. ComputeAwakeSet
	// handles agent-level SleepAfterIdle but the workspace-level session_sleep
	// policies (InteractiveResume, NonInteractive, etc.) require cfg + provider.
	// This pass updates wakeEvals so selectIdleProbeTargets sees the correct
	// ConfigSuppressed and Policy fields.
	for _, target := range wakeTargets {
		eval := wakeEvals[target.session.ID]
		policy := resolveSessionSleepPolicy(*target.session, cfg, sp)
		eval.Policy = policy
		name := target.session.Metadata["session_name"]
		decision := awakeDecisions[name]
		if decision.ShouldWake && !pendingInteractionReady(sp, name) && target.session.Metadata["pin_awake"] != "true" && configWakeSuppressed(*target.session, policy, sp, clk) {
			// Active demand (poolDesired > 0) overrides sleep suppression
			// for non-interactive sessions (matching the old
			// evaluateWakeReasons behavior). Interactive sessions honor
			// their idle window regardless of demand — an idle chat
			// session should still sleep to release resources.
			// Explicit sleep_intent always wins — if the session has
			// signaled it wants to sleep, honor that regardless of demand.
			template := normalizedSessionTemplate(*target.session, cfg)
			hasDemand := poolDesired[template] > 0
			hasExplicitSleepIntent := target.session.Metadata["sleep_intent"] != ""
			demandOverrides := hasDemand && policy.Class == config.SessionSleepNonInteractive && !hasExplicitSleepIntent
			if !demandOverrides {
				eval.ConfigSuppressed = true
				eval.Reasons = nil // Clear reasons so Phase 2 does not cancel the drain.
				eval.Reason = ""
			}
		}
		wakeEvals[target.session.ID] = eval
	}

	idleProbeTargets := selectIdleProbeTargets(wakeTargets, wakeEvals, dt)
	launchIdleProbes(ctx, idleProbeTargets, wakeTargets, dt, sp, clk)

	for _, target := range wakeTargets {
		if ctx != nil && ctx.Err() != nil {
			return 0
		}
		name := target.session.Metadata["session_name"]
		decision, hasDec := awakeDecisions[name]
		shouldWake := hasDec && decision.ShouldWake

		eval := wakeEvals[target.session.ID]
		if shouldWake && eval.ConfigSuppressed {
			shouldWake = false
		}
		persistSleepPolicyMetadata(target.session, store, eval.Policy, eval.ConfigSuppressed)

		if shouldWake && !target.alive {
			// Session should be awake but isn't — wake it.
			if isFailedCreateSessionBead(*target.session) {
				if trace != nil {
					trace.recordDecision("reconciler.session.wake", target.tp.TemplateName, name, "wake", "failed_create", traceRecordPayload{
						"pending_create_claim": strings.TrimSpace(target.session.Metadata["pending_create_claim"]),
					}, nil, "")
				}
				continue
			}
			if sessionIsQuarantined(*target.session, clk) {
				continue // crash-loop protection
			}
			if pendingCreateStartInFlight(*target.session, clk, startupTimeout) {
				if trace != nil {
					trace.recordDecision("reconciler.session.wake", target.tp.TemplateName, name, "wake", "start_in_flight", traceRecordPayload{
						"pending_create_claim": strings.TrimSpace(target.session.Metadata["pending_create_claim"]),
						"last_woke_at":         target.session.Metadata["last_woke_at"],
					}, nil, "")
				}
				continue
			}
			// Respawn circuit breaker: for named sessions the supervisor
			// will otherwise retry indefinitely. This phase only blocks
			// already-OPEN breakers; restart accounting happens at the
			// prepared-start boundary after dependency and wake-budget gates.
			if cbEnabled {
				identity := namedSessionIdentity(*target.session)
				if identity != "" {
					if cb.IsOpen(identity, cbNow) {
						if err := persistSessionCircuitBreakerMetadata(store, target.session, cb, identity, cbNow); err != nil {
							fmt.Fprintf(stderr, "session reconciler: %v\n", err) //nolint:errcheck // best-effort stderr
						}
						cb.LogOpenOnce(identity, stderr)
						if trace != nil {
							trace.recordDecision("reconciler.session.circuit_open", target.tp.TemplateName, name, "circuit_open", "skipped", traceRecordPayload{
								"identity": identity,
							}, nil, "")
						}
						continue
					}
				}
			}
			if trace != nil {
				trace.recordDecision("reconciler.session.wake", target.tp.TemplateName, name, "wake", "start_candidate", traceRecordPayload{
					"should_wake": shouldWake,
				}, nil, "")
			}
			startCandidates = append(startCandidates, startCandidate{
				session: target.session,
				tp:      target.tp,
				order:   len(startCandidates),
			})
		}

		if shouldWake && target.alive {
			// Session is correctly awake. Cancel any non-drift drain
			// (handles scale-back-up: agent returns to desired set while draining).
			cancelSessionDrain(*target.session, sp, dt)
			clearCompletedIdleProbe(target.session.ID, dt)
			if target.session.Metadata["sleep_intent"] == "idle-stop-pending" {
				_ = store.SetMetadata(target.session.ID, "sleep_intent", "")
				target.session.Metadata["sleep_intent"] = ""
			}
		}

		if !shouldWake && target.alive {
			// No reason to be awake — begin drain.
			intent := target.session.Metadata["sleep_intent"]
			var reason string
			switch {
			case intent == "idle-stop-pending":
				reason = "idle"
			case intent != "":
				reason = intent
			case hasDec && decision.Reason == "idle-sleep":
				reason = "idle"
			case eval.ConfigSuppressed:
				reason = "idle"
			default:
				reason = "no-wake-reason"
			}
			if reason != "idle" {
				clearCompletedIdleProbe(target.session.ID, dt)
			}
			if reason == "idle" && dt.get(target.session.ID) == nil {
				if intent != "idle-stop-pending" && !shouldBeginIdleDrain(target.session, eval, dt, sp) {
					continue
				}
				if intent != "idle-stop-pending" {
					markIdleSleepPending(target.session, store)
				}
			}
			if beginSessionDrain(*target.session, sp, dt, reason, clk, defaultDrainTimeout) {
				fmt.Fprintf(stdout, "Draining session '%s': %s\n", target.session.Metadata["session_name"], reason) //nolint:errcheck
				if trace != nil {
					trace.recordDecision("reconciler.session.drain", target.tp.TemplateName, target.session.Metadata["session_name"], reason, "drain", traceRecordPayload{
						"sleep_intent": intent,
					}, nil, "")
				}
			}
		}

		// Pool-managed sessions whose runtime has exited and whose bead is in
		// a terminal sleep state (drained, or asleep from a normal idle drain)
		// must free their slot so a fresh worker can spawn for new queue work.
		// Anything else (wait-hold, pending interaction, named/singleton) is
		// preserved.
		//
		// A pre-tick ownership snapshot predates the agent's own `bd close`
		// of its last unit of work, so this gate (and the drain-ack handler
		// above) queries the live store — across the primary store AND any
		// attached rig stores — via sessionHasOpenAssignedWork to avoid
		// closing a session that still owns work. Only pool-managed sessions
		// are disposable; singleton/named controller-managed identities must
		// keep the same bead so later wake/restart happens in place instead
		// of minting a fresh canonical owner.
		hasAssignedWork := false
		poolFreeable := !shouldWake && !target.alive && isPoolSessionSlotFreeable(*target.session) && isPoolManagedSessionBead(*target.session)
		if poolFreeable {
			var assignedErr error
			hasAssignedWork, assignedErr = sessionHasOpenAssignedWorkForReachableStore(cityPath, cfg, store, rigStores, *target.session)
			if assignedErr != nil {
				fmt.Fprintf(stderr, "session reconciler: checking assigned work for drained %s: %v\n", target.session.Metadata["session_name"], assignedErr) //nolint:errcheck
				hasAssignedWork = true
			}
		}
		if poolFreeable && !hasAssignedWork {
			// Close directly rather than via closeSessionBeadIfUnassigned.
			// That helper also runs a live sessionHasOpenAssignedWork query
			// and would redundantly re-query a store we just hit — skip the
			// duplicate I/O and pass through the preserved sleep_reason as
			// the close_reason below.
			//
			// Preserve the original sleep_reason (idle / idle-timeout / drained)
			// on the closed bead for forensic fidelity; fall back to "drained"
			// when the metadata is missing. Ops can then distinguish a natural
			// idle-timeout recycle from an explicit drain in the closed record.
			closeReason := strings.TrimSpace(target.session.Metadata["sleep_reason"])
			if closeReason == "" {
				closeReason = "drained"
			}
			closeBead(store, target.session.ID, closeReason, clk.Now().UTC(), stderr)
		}
	}

	if ctx != nil && ctx.Err() != nil {
		return 0
	}

	plannedWakes := executePlannedStartsTraced(
		ctx, startCandidates, cfg, desiredState, sp, store, cityName,
		cityPath,
		clk, rec, startupTimeout, stdout, stderr, trace,
		startOptions...,
	)

	if ctx != nil && ctx.Err() != nil {
		return plannedWakes
	}

	// Phase 2: Advance all in-flight drains.
	sessionLookup := func(id string) *beads.Bead {
		return beadByID[id]
	}
	advanceSessionDrainsWithSessionsTraced(dt, sp, store, sessionLookup, ordered, wakeEvals, cfg, poolDesired, nil, readyWaitSet, clk, trace)
	clearMissingIdleProbes(dt, beadByID)

	return plannedWakes
}

func cachedSessionPeek(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, target string, processNames []string) func(lines int) (string, error) {
	var (
		cached      bool
		cachedLines int
		content     string
	)
	return func(lines int) (string, error) {
		if cached && cachedLines >= lines {
			return content, nil
		}
		nextContent, nextErr := workerSessionTargetPeekWithConfig(cityPath, store, sp, cfg, target, lines, processNames)
		if nextErr != nil {
			return nextContent, nextErr
		}
		// Cache only successful peeks; transient capture errors must not
		// suppress a later rate-limit classifier in the same reconcile tick.
		content = nextContent
		cachedLines = lines
		cached = true
		return content, nil
	}
}

func rateLimitAliveFromObservation(alive bool, err error) bool {
	if err != nil {
		return false
	}
	return alive
}

func resolvePreservedConfiguredNamedSessionTemplate(
	cityPath, cityName string,
	cfg *config.City,
	sp runtime.Provider,
	store beads.Store,
	openSessions []beads.Bead,
	session beads.Bead,
	clk clock.Clock,
	stderr io.Writer,
) (TemplateParams, error) {
	if cityPath == "" {
		cityPath = "."
	}
	if cityName == "" && cfg != nil {
		cityName = cfg.EffectiveCityName()
	}
	identity := namedSessionIdentity(session)
	spec, ok := findNamedSessionSpec(cfg, cityName, identity)
	if !ok || spec.Agent == nil {
		return TemplateParams{}, fmt.Errorf("configured named session %q not found", identity)
	}
	bp := newAgentBuildParams(cityName, cityPath, cfg, sp, clk.Now().UTC(), store, stderr)
	bp.sessionBeads = newSessionBeadSnapshot(openSessions)
	fpExtra := buildFingerprintExtra(spec.Agent)
	tp, err := resolveTemplateForSessionBead(bp, spec.Agent, identity, fpExtra, session)
	if err != nil {
		return TemplateParams{}, err
	}
	tp.Alias = identity
	tp.TemplateName = namedSessionBackingTemplate(spec)
	tp.InstanceName = identity
	tp.ConfiguredNamedIdentity = identity
	tp.ConfiguredNamedMode = spec.Mode
	if tp.Env == nil {
		tp.Env = make(map[string]string)
	}
	tp.Env["GC_TEMPLATE"] = namedSessionBackingTemplate(spec)
	tp.Env["GC_ALIAS"] = identity
	tp.Env["GC_AGENT"] = identity
	tp.Env["GC_SESSION_ORIGIN"] = "named"
	installAgentSideEffects(bp, spec.Agent, tp, stderr)
	return tp, nil
}

// sessionHasOpenAssignedWork reports whether any open or in-progress work bead
// is assigned to the given session across all known stores. Use this
// cross-store query for cleanup-of-record paths that must not orphan work in
// any attached store; callers preserve fail-closed behavior by refusing close
// decisions on query errors. Reconciler close paths that should honor the
// session's configured store reachability must use
// sessionHasOpenAssignedWorkForReachableStore instead.
func sessionHasOpenAssignedWork(store beads.Store, rigStores map[string]beads.Store, session beads.Bead) (bool, error) {
	if has, err := sessionHasOpenAssignedWorkInStore(store, session); err != nil || has {
		return has, err
	}
	for _, rs := range rigStores {
		if has, err := sessionHasOpenAssignedWorkInStore(rs, session); err != nil || has {
			return has, err
		}
	}
	return false, nil
}

// sessionHasOpenAssignedWorkForReachableStore reports whether any open or
// in-progress work bead is assigned to the given session in the store its
// configured agent can query and claim from.
func sessionHasOpenAssignedWorkForReachableStore(
	cityPath string,
	cfg *config.City,
	store beads.Store,
	rigStores map[string]beads.Store,
	session beads.Bead,
) (bool, error) {
	storeRef, ok := assignedWorkStoreRefForSession(cityPath, cfg, session)
	if !ok {
		return sessionHasOpenAssignedWork(store, rigStores, session)
	}
	if storeRef == "" {
		return sessionHasOpenAssignedWorkInStore(store, session)
	}
	rigStore, ok := rigStores[storeRef]
	if !ok || rigStore == nil {
		return false, fmt.Errorf("rig store %q unavailable for session %q", storeRef, session.Metadata["session_name"])
	}
	return sessionHasOpenAssignedWorkInStore(rigStore, session)
}

func assignedWorkStoreRefForSession(cityPath string, cfg *config.City, session beads.Bead) (string, bool) {
	if cfg == nil {
		return "", false
	}
	template := normalizedSessionTemplate(session, cfg)
	if template == "" {
		template = strings.TrimSpace(session.Metadata["template"])
	}
	if template == "" {
		template = strings.TrimSpace(session.Metadata["common_name"])
	}
	if template == "" {
		return "", false
	}
	agentCfg := findAgentByTemplate(cfg, template)
	if agentCfg == nil {
		return "", false
	}
	return assignedWorkStoreRefForAgent(cityPath, cfg, agentCfg), true
}

func sessionHasOpenAssignedWorkInStore(store beads.Store, session beads.Bead) (bool, error) {
	if store == nil {
		return false, nil
	}
	identifiers := sessionAssignmentIdentifiers(session)
	seen := make(map[string]struct{}, len(identifiers))
	for _, status := range []string{"open", "in_progress"} {
		for _, assignee := range identifiers {
			if assignee == "" {
				continue
			}
			key := status + "\x00" + assignee
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			items, err := store.List(beads.ListQuery{Assignee: assignee, Status: status, Live: true})
			if err != nil {
				return false, err
			}
			for _, item := range items {
				if sessionpkg.IsSessionBeadOrRepairable(item) {
					continue
				}
				return true, nil
			}
		}
	}
	return false, nil
}

// namedSessionActivityThreshold is the maximum age of the last reliable
// activity reference for a named session to be considered "actively in use".
//
// namedSessionRecentActivityConfigDriftDeferralLimit bounds recent-activity
// deferrals for one fixed drift episode. Recent output is only a heuristic,
// unlike an attachment or pending interaction, so it should not hide config
// drift indefinitely.
const (
	namedSessionActivityThreshold                      = 2 * time.Minute
	namedSessionRecentActivityConfigDriftDeferralLimit = 30 * time.Second
	sessionAttachedConfigDriftFalseNegativeLimit       = 30 * time.Second
	namedSessionConfigDriftDeferredAtMetadata          = "config_drift_deferred_at"
	namedSessionConfigDriftDeferredKeyMetadata         = "config_drift_deferred_key"
	sessionAttachedConfigDriftDeferredAtMetadata       = "attached_config_drift_deferred_at"
	sessionAttachedConfigDriftDeferredKeyMetadata      = "attached_config_drift_deferred_key"
)

// namedSessionActivelyInUse returns true if a named session is currently
// in active use and should not be immediately drained for config-drift.
// It checks three positive-use signals:
//  1. A pending interaction (user waiting for response)
//  2. Tmux session attachment
//  3. A recent reliable activity timestamp within the activity threshold
//
// If the provider cannot report activity, the function is conservative and
// treats the live named session as active because config-drift cannot prove the
// session is idle.
func namedSessionActivelyInUse(session beads.Bead, sp runtime.Provider, name string, clk clock.Clock) bool {
	_, active := namedSessionActiveUseReason(session, sp, name, clk)
	return active
}

func shouldDeferNamedSessionConfigDrift(session beads.Bead, store beads.Store, sp runtime.Provider, name string, clk clock.Clock, driftKey string) (string, bool, error) {
	reason, active := namedSessionActiveUseReason(session, sp, name, clk)
	if !active {
		return "", false, nil
	}
	switch reason {
	case "activity_unknown":
		return boundedNamedSessionConfigDriftDeferral(session, store, clk, driftKey, reason, namedSessionActivityThreshold)
	case "recent_activity":
		return boundedNamedSessionConfigDriftDeferral(session, store, clk, driftKey, reason, namedSessionRecentActivityConfigDriftDeferralLimit)
	}
	return reason, true, nil
}

func boundedNamedSessionConfigDriftDeferral(
	session beads.Bead,
	store beads.Store,
	clk clock.Clock,
	driftKey string,
	reason string,
	limit time.Duration,
) (string, bool, error) {
	if clk == nil {
		return reason, true, nil
	}
	now := clk.Now().UTC()
	if session.Metadata[namedSessionConfigDriftDeferredKeyMetadata] != driftKey {
		if err := recordNamedSessionConfigDriftDeferredAt(session, store, now, driftKey); err != nil {
			return "", false, err
		}
		return reason, true, nil
	}
	raw := session.Metadata[namedSessionConfigDriftDeferredAtMetadata]
	if raw == "" {
		if err := recordNamedSessionConfigDriftDeferredAt(session, store, now, driftKey); err != nil {
			return "", false, err
		}
		return reason, true, nil
	}
	deferredAt, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		if err := recordNamedSessionConfigDriftDeferredAt(session, store, now, driftKey); err != nil {
			return "", false, err
		}
		return reason, true, nil
	}
	if now.Sub(deferredAt) < limit {
		return reason, true, nil
	}
	return "", false, nil
}

func recordNamedSessionConfigDriftDeferredAt(session beads.Bead, store beads.Store, t time.Time, driftKey string) error {
	if store == nil || session.ID == "" {
		return nil
	}
	return store.SetMetadataBatch(session.ID, map[string]string{
		namedSessionConfigDriftDeferredAtMetadata:  t.UTC().Format(time.RFC3339),
		namedSessionConfigDriftDeferredKeyMetadata: driftKey,
	})
}

func clearSessionConfigDriftDeferral(session beads.Bead, store beads.Store) error {
	if store == nil || session.ID == "" {
		return nil
	}
	if session.Metadata[namedSessionConfigDriftDeferredAtMetadata] == "" &&
		session.Metadata[namedSessionConfigDriftDeferredKeyMetadata] == "" &&
		session.Metadata[sessionAttachedConfigDriftDeferredAtMetadata] == "" &&
		session.Metadata[sessionAttachedConfigDriftDeferredKeyMetadata] == "" {
		return nil
	}
	return store.SetMetadataBatch(session.ID, map[string]string{
		namedSessionConfigDriftDeferredAtMetadata:     "",
		namedSessionConfigDriftDeferredKeyMetadata:    "",
		sessionAttachedConfigDriftDeferredAtMetadata:  "",
		sessionAttachedConfigDriftDeferredKeyMetadata: "",
	})
}

func recordSessionAttachedConfigDriftDeferral(session beads.Bead, store beads.Store, clk clock.Clock, driftKey string) error {
	if store == nil || session.ID == "" {
		return nil
	}
	now := time.Now().UTC()
	if clk != nil {
		now = clk.Now().UTC()
	}
	// Skip the write when the same drift key is already deferred and the
	// existing stamp is comfortably within the false-negative TTL window.
	// Without this guard the reconciler emits a bead.updated event on every
	// tick (~1.4s) for every attached session with persistent drift.
	// Refreshing at TTL/2 leaves enough headroom that a paused reconcile
	// loop cannot accidentally let the deferral expire.
	if driftKey != "" && session.Metadata[sessionAttachedConfigDriftDeferredKeyMetadata] == driftKey {
		if raw := session.Metadata[sessionAttachedConfigDriftDeferredAtMetadata]; raw != "" {
			if existing, err := time.Parse(time.RFC3339, raw); err == nil &&
				!existing.After(now) &&
				now.Sub(existing) < sessionAttachedConfigDriftFalseNegativeLimit/2 {
				return nil
			}
		}
	}
	return store.SetMetadataBatch(session.ID, map[string]string{
		sessionAttachedConfigDriftDeferredAtMetadata:  now.Format(time.RFC3339),
		sessionAttachedConfigDriftDeferredKeyMetadata: driftKey,
	})
}

func recentlyDeferredSessionAttachedConfigDrift(session beads.Bead, clk clock.Clock, driftKey string) bool {
	if driftKey == "" || session.Metadata[sessionAttachedConfigDriftDeferredKeyMetadata] != driftKey {
		return false
	}
	raw := session.Metadata[sessionAttachedConfigDriftDeferredAtMetadata]
	if raw == "" {
		return false
	}
	deferredAt, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return false
	}
	now := time.Now().UTC()
	if clk != nil {
		now = clk.Now().UTC()
	}
	if now.Before(deferredAt) {
		return true
	}
	return now.Sub(deferredAt) < sessionAttachedConfigDriftFalseNegativeLimit
}

// sessionAttachedForConfigDrift reports whether a session is currently
// attached (a user terminal is connected) and should skip config-drift
// handling. It checks worker-handle observation first and falls back to the
// provider's direct attachment probe.
func sessionAttachedForConfigDrift(session beads.Bead, sp runtime.Provider, cityPath string, store beads.Store, cfg *config.City, name string) (bool, error) {
	if sp == nil {
		return false, nil
	}
	var observeErr error
	if attached, err := workerSessionTargetAttachedWithConfig(cityPath, store, sp, cfg, session.ID); err != nil {
		observeErr = err
	} else if attached {
		return true, nil
	}
	if sp.IsAttached(name) {
		return true, observeErr
	}
	return false, observeErr
}

func sessionConfigDriftKey(session beads.Bead, cfg *config.City, tp TemplateParams) string {
	template := tp.TemplateName
	if template == "" {
		template = normalizedSessionTemplate(session, cfg)
	}
	storedHash := session.Metadata["started_config_hash"]
	if template == "" || storedHash == "" {
		return ""
	}
	if findAgentByTemplate(cfg, template) == nil {
		return ""
	}
	agentCfg := templateParamsToConfig(tp)
	applyTemplateOverridesToConfig(&agentCfg, session, tp)
	currentHash := runtime.CoreFingerprint(agentCfg)
	if storedHash == currentHash {
		return ""
	}
	return storedHash + ":" + currentHash
}

func configDriftTracePayload(storedHash, currentHash string, driftedFields []string, extra traceRecordPayload) traceRecordPayload {
	fields := append([]string(nil), driftedFields...)
	if fields == nil {
		fields = []string{}
	}
	payload := traceRecordPayload{}
	for k, v := range extra {
		payload[k] = v
	}
	payload["stored_hash"] = storedHash
	payload["current_hash"] = currentHash
	payload["drifted_fields"] = fields
	return payload
}

func applyTemplateOverridesToConfig(agentCfg *runtime.Config, session beads.Bead, tp TemplateParams) {
	if agentCfg == nil {
		return
	}
	rawOvr := session.Metadata["template_overrides"]
	if rawOvr == "" || tp.ResolvedProvider == nil || len(tp.ResolvedProvider.OptionsSchema) == 0 {
		return
	}
	var ovr map[string]string
	if err := json.Unmarshal([]byte(rawOvr), &ovr); err != nil || len(ovr) == 0 {
		return
	}
	fullOptions := make(map[string]string)
	for k, v := range tp.ResolvedProvider.EffectiveDefaults {
		fullOptions[k] = v
	}
	for k, v := range ovr {
		if k == "initial_message" {
			continue
		}
		fullOptions[k] = v
	}
	extra, err := config.ResolveExplicitOptions(tp.ResolvedProvider.OptionsSchema, fullOptions)
	if err != nil || len(extra) == 0 {
		return
	}
	agentCfg.Command = replaceSchemaFlags(agentCfg.Command, tp.ResolvedProvider.OptionsSchema, extra)
}

func namedSessionActiveUseReason(session beads.Bead, sp runtime.Provider, name string, clk clock.Clock) (string, bool) {
	if sp == nil || name == "" {
		return "", false
	}
	// Pending interaction means a user is actively waiting.
	if pendingInteractionKeepsAwake(session, sp, name, clk) {
		return "pending_interaction", true
	}
	// Tmux attachment means a user is watching.
	if sp.IsAttached(name) {
		return "attached", true
	}
	// Providers that cannot report activity for this routed session cannot
	// prove a live named session is idle. Defer config-drift rather than
	// stopping a potentially working headless agent mid-task.
	sleepCapability := resolveSleepCapability(sp, name)
	if sleepCapability == runtime.SessionSleepCapabilityDisabled ||
		(sleepCapability == runtime.SessionSleepCapabilityTimedOnly && !sp.Capabilities().CanReportActivity) {
		return "activity_unknown", true
	}
	// Recent activity means the agent may still be in active use.
	if clk != nil {
		if lastActivity, err := sp.GetLastActivity(name); err == nil && !lastActivity.IsZero() && clk.Now().Sub(lastActivity) < namedSessionActivityThreshold {
			return "recent_activity", true
		}
	}
	return "", false
}

func resetConfiguredNamedSessionForConfigDrift(
	session *beads.Bead,
	store beads.Store,
	sp runtime.Provider,
	sessionName string,
	alive bool,
	nextState string,
	now time.Time,
	stderr io.Writer,
) {
	if session == nil || store == nil {
		return
	}
	if nextState == "" {
		nextState = "asleep"
	}
	if alive && sp != nil && sessionName != "" {
		if err := workerKillSessionTargetWithConfig("", store, sp, nil, sessionName); err != nil {
			fmt.Fprintf(stderr, "session reconciler: stopping config-drift named session %s: %v\n", sessionName, err) //nolint:errcheck
		}
	}
	newSessionKey := ""
	if newKey, err := sessionpkg.GenerateSessionKey(); err == nil {
		newSessionKey = newKey
	}
	batch := sessionpkg.ConfigDriftResetPatch(sessionpkg.State(nextState), newSessionKey, now)
	batch[namedSessionConfigDriftDeferredAtMetadata] = ""
	batch[namedSessionConfigDriftDeferredKeyMetadata] = ""
	batch[sessionAttachedConfigDriftDeferredAtMetadata] = ""
	batch[sessionAttachedConfigDriftDeferredKeyMetadata] = ""
	if err := store.SetMetadataBatch(session.ID, batch); err != nil {
		fmt.Fprintf(stderr, "session reconciler: recording config-drift repair for %s: %v\n", sessionName, err) //nolint:errcheck
		return
	}
	if session.Metadata == nil {
		session.Metadata = make(map[string]string, len(batch))
	}
	for key, value := range batch {
		session.Metadata[key] = value
	}
}

func shouldBeginIdleDrain(
	session *beads.Bead,
	eval wakeEvaluation,
	dt *drainTracker,
	sp runtime.Provider,
) bool {
	if session == nil {
		return false
	}
	if eval.Policy.Class == config.SessionSleepNonInteractive {
		return true
	}
	if eval.Policy.Capability != runtime.SessionSleepCapabilityFull || sp == nil {
		return false
	}
	probe, ok := dt.idleProbe(session.ID)
	if !ok || !probe.ready {
		return false
	}
	defer dt.clearIdleProbe(session.ID)
	if !probe.success {
		return false
	}
	lastActivity, err := workerSessionTargetLastActivityWithConfig("", nil, sp, nil, session.Metadata["session_name"])
	if err != nil {
		return false
	}
	return lastActivity.IsZero() || !lastActivity.After(probe.completedAt)
}

func selectIdleProbeTargets(
	wakeTargets []wakeTarget,
	wakeEvals map[string]wakeEvaluation,
	dt *drainTracker,
) map[string]bool {
	targets := make(map[string]bool)
	if dt == nil {
		return targets
	}
	var candidates []string
	// Snapshot drain/probe state under one lock. Do not call other
	// drainTracker helpers while holding dt.mu.
	dt.mu.Lock()
	defer dt.mu.Unlock()
	activeProbes := 0
	for _, probe := range dt.idleProbes {
		if probe != nil && !probe.ready {
			activeProbes++
		}
	}
	limit := maxIdleSleepProbesPerTick - activeProbes
	if limit <= 0 {
		return targets
	}
	for _, target := range wakeTargets {
		if target.session == nil || !target.alive {
			continue
		}
		if target.session.Metadata["sleep_intent"] != "" {
			continue
		}
		if dt.drains[target.session.ID] != nil {
			continue
		}
		if dt.idleProbes[target.session.ID] != nil {
			continue
		}
		eval, ok := wakeEvals[target.session.ID]
		if !ok || len(eval.Reasons) > 0 || !eval.ConfigSuppressed || !eval.Policy.enabled() {
			continue
		}
		if eval.Policy.Class == config.SessionSleepNonInteractive {
			continue
		}
		candidates = append(candidates, target.session.ID)
	}
	if len(candidates) == 0 {
		if activeProbes == 0 {
			dt.idleProbeCursor = 0
		}
		return targets
	}
	start := dt.idleProbeCursor % len(candidates)
	if limit > len(candidates) {
		limit = len(candidates)
	}
	for i := 0; i < limit; i++ {
		targets[candidates[(start+i)%len(candidates)]] = true
	}
	dt.idleProbeCursor = (start + limit) % len(candidates)
	return targets
}

func launchIdleProbes(
	ctx context.Context,
	idleProbeTargets map[string]bool,
	wakeTargets []wakeTarget,
	dt *drainTracker,
	sp runtime.Provider,
	clk clock.Clock,
) {
	if len(idleProbeTargets) == 0 || dt == nil || sp == nil {
		return
	}
	wp, ok := sp.(runtime.IdleWaitProvider)
	if !ok {
		return
	}
	for _, target := range wakeTargets {
		if target.session == nil || !idleProbeTargets[target.session.ID] {
			continue
		}
		name := target.session.Metadata["session_name"]
		probe := dt.startIdleProbe(target.session.ID)
		if name == "" || probe == nil {
			continue
		}
		go func(beadID, sessionName string, probe *idleProbeState) {
			err := wp.WaitForIdle(ctx, sessionName, idleSleepProbeTimeout)
			dt.finishIdleProbe(beadID, probe, err == nil, clk.Now().UTC())
		}(target.session.ID, name, probe)
	}
}

func clearCompletedIdleProbe(beadID string, dt *drainTracker) {
	if dt == nil {
		return
	}
	probe, ok := dt.idleProbe(beadID)
	if ok && probe.ready {
		dt.clearIdleProbe(beadID)
	}
}

func clearMissingIdleProbes(dt *drainTracker, beadByID map[string]*beads.Bead) {
	if dt == nil {
		return
	}
	dt.mu.Lock()
	var stale []string
	for id := range dt.idleProbes {
		if beadByID[id] == nil {
			stale = append(stale, id)
		}
	}
	dt.mu.Unlock()
	for _, id := range stale {
		dt.clearIdleProbe(id)
	}
}

// resolveTaskWorkDir checks the agent's assigned task beads for a work_dir
// metadata field. If a task bead has work_dir set and the directory exists
// on disk, that path is returned. This lets the reconciler start the agent
// in the worktree that the previous session (or this session's prior run)
// created, without any prompt-side logic.
func resolveTaskWorkDir(store beads.Store, assignees ...string) string {
	seen := make(map[string]bool, len(assignees))
	for _, assignee := range assignees {
		assignee = strings.TrimSpace(assignee)
		if assignee == "" || seen[assignee] {
			continue
		}
		seen[assignee] = true
		assigned, err := store.List(beads.ListQuery{
			Assignee: assignee,
			Status:   "in_progress",
			Live:     true,
			Sort:     beads.SortCreatedDesc,
		})
		if err != nil {
			continue
		}
		for _, b := range assigned {
			wd := b.Metadata["work_dir"]
			if wd != "" {
				if info, err := os.Stat(wd); err == nil && info.IsDir() {
					return wd
				}
			}
		}
	}
	return ""
}

// resolveSessionCommand returns the command to use when starting a session.
// On a fresh provider start (first boot or wake_mode=fresh), it uses
// SessionIDFlag to create a new provider conversation with the given key as
// its ID. Otherwise it resumes the existing conversation.
func resolveSessionCommand(command, sessionKey string, rp *config.ResolvedProvider, firstStart, forceFresh bool) string {
	if (firstStart || forceFresh) && rp.SessionIDFlag != "" {
		return command + " " + rp.SessionIDFlag + " " + sessionKey
	}
	return resolveResumeCommand(command, sessionKey, rp)
}

// resolveResumeCommand returns the command to use when resuming a session.
// Priority: explicit resume_command (with {{.SessionKey}} expansion) >
// ResumeFlag/ResumeStyle auto-construction > original command unchanged.
func resolveResumeCommand(command, sessionKey string, rp *config.ResolvedProvider) string {
	// Explicit resume_command takes precedence.
	if rp.ResumeCommand != "" {
		return strings.ReplaceAll(rp.ResumeCommand, "{{.SessionKey}}", sessionKey)
	}
	// Fall back to ResumeFlag/ResumeStyle auto-construction.
	if rp.ResumeFlag == "" {
		return command
	}
	switch rp.ResumeStyle {
	case "subcommand":
		parts := strings.SplitN(command, " ", 2)
		if len(parts) == 2 {
			return parts[0] + " " + rp.ResumeFlag + " " + sessionKey + " " + parts[1]
		}
		return command + " " + rp.ResumeFlag + " " + sessionKey
	default: // "flag"
		return command + " " + rp.ResumeFlag + " " + sessionKey
	}
}
