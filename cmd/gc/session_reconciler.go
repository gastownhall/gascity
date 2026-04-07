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

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/telemetry"
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
	cfgAgent := findAgentByTemplate(cfg, template)
	if cfgAgent == nil || len(cfgAgent.DependsOn) == 0 {
		return true
	}
	for _, dep := range cfgAgent.DependsOn {
		depCfg := findAgentByTemplate(cfg, dep)
		if depCfg == nil {
			continue // dependency not in config — skip
		}
		if !dependencyTemplateAlive(dep, cfg, desiredState, sp, cityName, store) {
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
	return allDependenciesAliveForTemplate(normalizedSessionTemplate(session, cfg), cfg, desiredState, sp, cityName, store)
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
// Returns the number of sessions woken this tick.
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
		ctx, "", sessions, desiredState, configuredNames, cfg, sp, store, dops, assignedWorkBeads, readyWaitSet, dt,
		poolDesired, storeQueryPartial, workSet, cityName, it, clk, rec, startupTimeout, driftDrainTimeout, stdout, stderr,
	)
}

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
	return reconcileSessionBeadsTraced(
		ctx, cityPath, sessions, desiredState, configuredNames, cfg, sp, store, dops, assignedWorkBeads, readyWaitSet, dt,
		poolDesired, storeQueryPartial, workSet, cityName, it, clk, rec, startupTimeout, driftDrainTimeout, stdout, stderr, nil,
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
) int {
	deps := buildDepsMap(cfg)

	// Phase 0: Heal expired timers on all sessions.
	for i := range sessions {
		healExpiredTimers(&sessions[i], store, clk)
	}

	// Topo-order sessions by template dependencies.
	ordered := topoOrder(sessions, deps)

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
	for i := range ordered {
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

		name := session.Metadata["session_name"]
		tp, desired := desiredState[name]

		// Orphan/suspended: bead exists but not in desired state.
		// Handle BEFORE heal/stability to avoid false crash detection —
		// a running session that leaves the desired set is not a crash.
		if !desired {
			providerAlive := sp.IsRunning(name)
			// Heal state using provider liveness, not agent membership.
			healState(session, providerAlive, store, clk)
			if preserveConfiguredNamedSessionBead(*session, cfg, cityName) {
				template := normalizedSessionTemplate(*session, cfg)
				if template == "" {
					template = session.Metadata["template"]
				}
				preservedTP, err := resolvePreservedConfiguredNamedSessionTemplate(cityPath, cityName, cfg, sp, store, ordered, *session, clk, stderr)
				if err != nil {
					fmt.Fprintf(stderr, "session reconciler: resolve preserved named session %s: %v\n", name, err) //nolint:errcheck
				} else {
					tp = preservedTP
					desired = true
				}
				if trace != nil {
					trace.recordDecision("reconciler.session.preserve_configured_named", template, name, "preserve", map[bool]string{
						true:  "kept_open",
						false: "resolution_failed",
					}[desired], traceRecordPayload{
						"provider_alive": providerAlive,
						"degraded":       err != nil,
					}, nil, "")
				}
			} else {
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
					template := normalizedSessionTemplate(*session, cfg)
					if template == "" {
						template = session.Metadata["template"]
					}
					if trace != nil {
						trace.recordDecision("reconciler.session.orphan_or_suspended", template, name, reason, "drain", traceRecordPayload{
							"store_query_partial": storeQueryPartial,
							"provider_alive":      providerAlive,
						}, nil, "")
					}
					beginSessionDrain(*session, sp, dt, reason, clk, defaultDrainTimeout)
					fmt.Fprintf(stdout, "Draining session '%s': %s\n", name, reason) //nolint:errcheck
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
					closeBead(store, session.ID, reason, clk.Now().UTC(), stderr)
				}
				continue
			}
		}

		// Liveness includes zombie detection: tmux session exists AND
		// the expected child process is alive (when ProcessNames configured).
		running := sp.IsRunning(name)
		alive := running && sp.ProcessAlive(name, tp.Hints.ProcessNames)

		// Zombie capture: session exists but process dead — grab scrollback for forensics.
		if running && !alive {
			if output, err := sp.Peek(name, 50); err == nil && output != "" {
				rec.Record(events.Event{
					Type:    events.SessionCrashed,
					Actor:   "gc",
					Subject: tp.DisplayName(),
					Message: output,
				})
				telemetry.RecordAgentCrash(context.Background(), tp.DisplayName(), output)
			}
		}
		if alive && shouldRollbackPendingCreate(session) && !runningSessionMatchesPendingCreate(session, name, sp) {
			fmt.Fprintf(stderr, "session reconciler: rolling back pending create %s: live runtime belongs to another session\n", name) //nolint:errcheck
			if trace != nil {
				trace.recordDecision("reconciler.session.pending_create", tp.TemplateName, name, "pending_create_rollback", "rollback", nil, nil, "")
			}
			rollbackPendingCreate(session, store, clk.Now().UTC(), stderr)
			continue
		}

		// Drain-ack: agent signaled it's done (gc runtime drain-ack).
		// Honor the ack even if the agent exited before this tick; otherwise
		// the session falls through to orphan handling and can block the next
		// worker wave until the stale awake bead ages out.
		if dops != nil {
			if acked, _ := dops.isDrainAcked(name); acked {
				_ = dops.clearDrain(name)
				if alive {
					if err := sp.Stop(name); err != nil {
						fmt.Fprintf(stderr, "session reconciler: stopping drain-acked %s: %v\n", name, err) //nolint:errcheck
					} else {
						fmt.Fprintf(stdout, "Stopped drain-acked session '%s'\n", name) //nolint:errcheck
					}
				}
				rec.Record(events.Event{
					Type:    events.SessionStopped,
					Actor:   "gc",
					Subject: tp.DisplayName(),
					Message: "drain acknowledged by agent",
				})
				if store != nil && session.ID != "" {
					_ = store.SetMetadata(session.ID, "state", "drained")
					session.Metadata["state"] = "drained"
				}
				continue
			}
		}

		policy := resolveSessionSleepPolicy(*session, cfg, sp)

		// Heal advisory state metadata.
		healState(session, alive, store, clk)
		if recoverPendingIdleSleep(session, store, running, clk) {
			alive = false
		}
		reconcileDetachedAt(session, store, policy, alive, sp, clk)

		// Stability check: detect rapid exit (crash).
		if checkStability(session, cfg, alive, dt, store, clk) {
			continue // crash recorded, skip further processing
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
			if err := clearPendingCreateClaim(session, store); err != nil {
				fmt.Fprintf(stderr, "session reconciler: clearing pending create claim for %s: %v\n", name, err) //nolint:errcheck
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
				if tmuxRequested && dops != nil {
					_ = dops.clearRestartRequested(name)
				}
				// Rotate session_key so the next start gets a fresh
				// conversation. Clearing started_config_hash forces
				// firstStart=true in resolveSessionCommand. Clearing
				// last_woke_at masks the intentional death from crash
				// and churn trackers (both check last_woke_at first).
				batch := map[string]string{
					"restart_requested":          "",
					"started_config_hash":        "",
					"continuation_reset_pending": "true",
					"last_woke_at":               "",
				}
				if newKey, err := sessionpkg.GenerateSessionKey(); err == nil {
					batch["session_key"] = newKey
					session.Metadata["session_key"] = newKey
				}
				_ = store.SetMetadataBatch(session.ID, batch)
				session.Metadata["restart_requested"] = ""
				session.Metadata["started_config_hash"] = ""
				session.Metadata["continuation_reset_pending"] = "true"
				session.Metadata["last_woke_at"] = ""
				if alive {
					if err := sp.Stop(name); err != nil {
						fmt.Fprintf(stderr, "session reconciler: stopping restart-requested %s: %v\n", name, err) //nolint:errcheck
					} else {
						fmt.Fprintf(stdout, "Stopped restart-requested session '%s'\n", name) //nolint:errcheck
					}
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
			storedHash := session.Metadata["config_hash"]
			if sh := session.Metadata["started_config_hash"]; sh != "" {
				storedHash = sh
			}
			if template != "" && storedHash != "" {
				cfgAgent := findAgentByTemplate(cfg, template)
				if cfgAgent != nil {
					agentCfg := templateParamsToConfig(tp)
					// Apply template_overrides using the same resolution as
					// prepareSessionStart: merge defaults + overrides, then
					// replaceSchemaFlags to strip and re-add all schema flags.
					if rawOvr := session.Metadata["template_overrides"]; rawOvr != "" {
						if tp.ResolvedProvider != nil && len(tp.ResolvedProvider.OptionsSchema) > 0 {
							var ovr map[string]string
							if err := json.Unmarshal([]byte(rawOvr), &ovr); err == nil && len(ovr) > 0 {
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
								if extra, rErr := config.ResolveExplicitOptions(tp.ResolvedProvider.OptionsSchema, fullOptions); rErr == nil && len(extra) > 0 {
									agentCfg.Command = replaceSchemaFlags(agentCfg.Command, tp.ResolvedProvider.OptionsSchema, extra)
								}
							}
						}
					}
					currentHash := runtime.CoreFingerprint(agentCfg)
					if storedHash != currentHash {
						fmt.Fprintf(stderr, "config-drift %s: stored=%s current=%s cmd=%q\n", name, storedHash[:12], currentHash[:12], agentCfg.Command) //nolint:errcheck
						// Defer config-drift drain while a user is attached.
						// Killing a session mid-conversation is disruptive;
						// the drift will be applied when the user detaches.
						if sp.IsAttached(name) {
							if trace != nil {
								trace.recordDecision("reconciler.session.config_drift", tp.TemplateName, name, "config_drift", "deferred_attached", traceRecordPayload{
									"stored_hash":  storedHash,
									"current_hash": currentHash,
								}, nil, "")
							}
							continue
						}
						ddt := driftDrainTimeout
						if ddt <= 0 {
							ddt = defaultDrainTimeout
						}
						beginSessionDrain(*session, sp, dt, "config-drift", clk, ddt)
						fmt.Fprintf(stdout, "Draining session '%s': config-drift\n", name) //nolint:errcheck
						if trace != nil {
							trace.recordDecision("reconciler.session.config_drift", tp.TemplateName, name, "config_drift", "drain", traceRecordPayload{
								"stored_hash":  storedHash,
								"current_hash": currentHash,
							}, nil, "")
						}
						rec.Record(events.Event{
							Type:    events.SessionDraining,
							Actor:   "gc",
							Subject: tp.DisplayName(),
							Message: "config drift detected",
						})
						continue
					}

					// Core config matches — check live-only drift.
					storedLive := session.Metadata["live_hash"]
					if sl := session.Metadata["started_live_hash"]; sl != "" {
						storedLive = sl
					}
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

		// Idle timeout: restart sessions idle longer than configured threshold.
		if it != nil && alive && it.checkIdle(name, sp, clk.Now()) {
			fmt.Fprintf(stderr, "session reconciler: idle timeout for %s\n", tp.DisplayName()) //nolint:errcheck // best-effort stderr
			if trace != nil {
				trace.recordDecision("reconciler.session.idle_timeout", tp.TemplateName, name, "idle_timeout", "stop", nil, nil, "")
			}
			if err := sp.Stop(name); err != nil {
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
				_ = store.SetMetadataBatch(session.ID, map[string]string{
					"state": "asleep", "last_woke_at": "", "sleep_reason": "idle-timeout",
				})
				session.Metadata["state"] = "asleep"
				session.Metadata["last_woke_at"] = ""
				session.Metadata["sleep_reason"] = "idle-timeout"
				alive = false
			}
			// Fall through to wakeReasons — it will re-wake immediately if config present
		}

		wakeTargets = append(wakeTargets, wakeTarget{session: session, tp: tp, alive: alive})
	}

	// Use ComputeAwakeSet for the wake/sleep decision.
	awakeInput := buildAwakeInputFromReconciler(
		cfg, ordered, poolDesired, workSet, readyWaitSet,
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
		if decision.ShouldWake && configWakeSuppressed(*target.session, policy, sp, clk) {
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
			}
		}
		wakeEvals[target.session.ID] = eval
	}

	idleProbeTargets := selectIdleProbeTargets(wakeTargets, wakeEvals, dt)
	launchIdleProbes(ctx, idleProbeTargets, wakeTargets, dt, sp, clk)

	for _, target := range wakeTargets {
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
			if sessionIsQuarantined(*target.session, clk) {
				continue // crash-loop protection
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
			beginSessionDrain(*target.session, sp, dt, reason, clk, defaultDrainTimeout)
			fmt.Fprintf(stdout, "Draining session '%s': %s\n", target.session.Metadata["session_name"], reason) //nolint:errcheck
			if trace != nil {
				trace.recordDecision("reconciler.session.drain", target.tp.TemplateName, target.session.Metadata["session_name"], reason, "drain", traceRecordPayload{
					"sleep_intent": intent,
				}, nil, "")
			}
		}

		if !shouldWake && !target.alive && isDrainedSessionBead(*target.session) {
			// Drained pool session: process exited and no wake reason.
			// Close the bead so syncSessionBeads creates a fresh one
			// when new work arrives.
			closeBead(store, target.session.ID, "drained", clk.Now().UTC(), stderr)
		}
	}

	plannedWakes := executePlannedStartsTraced(
		ctx, startCandidates, cfg, desiredState, sp, store, cityName,
		clk, rec, startupTimeout, stdout, stderr, trace,
	)

	// Phase 2: Advance all in-flight drains.
	sessionLookup := func(id string) *beads.Bead {
		return beadByID[id]
	}
	advanceSessionDrainsWithSessionsTraced(dt, sp, store, sessionLookup, ordered, wakeEvals, cfg, poolDesired, nil, readyWaitSet, clk, trace)
	clearMissingIdleProbes(dt, beadByID)

	return plannedWakes
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
	tp.ConfiguredNamedIdentity = identity
	tp.ConfiguredNamedMode = spec.Mode
	installAgentSideEffects(bp, spec.Agent, tp, stderr)
	return tp, nil
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
	lastActivity, err := sp.GetLastActivity(session.Metadata["session_name"])
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
		dt.beginIdleProbe()
		go func(beadID, sessionName string, probe *idleProbeState) {
			defer dt.doneIdleProbe()
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
func resolveTaskWorkDir(store beads.Store, agentName string) string {
	assigned, err := store.List(beads.ListQuery{
		Assignee: agentName,
		Status:   "in_progress",
		Sort:     beads.SortCreatedDesc,
	})
	if err != nil {
		return ""
	}
	for _, b := range assigned {
		wd := b.Metadata["work_dir"]
		if wd != "" {
			if info, err := os.Stat(wd); err == nil && info.IsDir() {
				return wd
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
