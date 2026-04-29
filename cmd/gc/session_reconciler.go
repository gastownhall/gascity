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
	if !sessionStartRequested(session, clk) {
		return false
	}
	template := normalizedSessionTemplate(session, cfg)
	if template == "" {
		template = session.Metadata["template"]
	}
	agent := findAgentByTemplate(cfg, template)
	return agent != nil && !agent.Suspended
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
	return reconcileSessionBeadsTraced(
		ctx, cityPath, sessions, desiredState, configuredNames, cfg, sp, store, dops, assignedWorkBeads, rigStores, readyWaitSet, dt,
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
			// Heal state using provider liveness, not agent membership.
			healState(session, providerAlive, store, clk)
			switch {
			case preserveConfiguredNamedSessionBead(*session, cfg, cityName):
				template := normalizedSessionTemplate(*session, cfg)
				if template == "" {
					template = session.Metadata["template"]
				}
				preservedTP, err := resolvePreservedConfiguredNamedSessionTemplate(cityPath, cityName, cfg, sp, store, ordered, *session, clk, stderr)
				switch {
				case err != nil:
					fmt.Fprintf(stderr, "session reconciler: resolve preserved named session %s: %v\n", name, err) //nolint:errcheck
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
						"degraded":       err != nil,
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
							})
							hasAssignedWork, assignedErr := sessionHasOpenAssignedWork(store, rigStores, *session)
							if assignedErr != nil {
								fmt.Fprintf(stderr, "session reconciler: checking assigned work for drain-acked %s: %v\n", name, assignedErr) //nolint:errcheck
								hasAssignedWork = true
							}
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
							closeSessionBeadIfUnassigned(store, rigStores, *session, "drained", clk.Now().UTC(), stderr)
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
					closeSessionBeadIfUnassigned(store, rigStores, *session, reason, clk.Now().UTC(), stderr)
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

		// Zombie capture: session exists but process dead — grab scrollback for forensics.
		if running && !alive {
			if output, err := workerSessionTargetPeekWithConfig(cityPath, store, sp, cfg, session.ID, 50, tp.Hints.ProcessNames); err == nil && output != "" {
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
					_, reconcilerOwnedAck := reconcilerDrainAckMatchesSession(*session, sp, name)
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
						hasAssignedWork, assignedErr := sessionHasOpenAssignedWork(store, rigStores, *session)
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

		// Heal advisory state metadata.
		stateBeforeHeal := sessionpkg.State(strings.TrimSpace(session.Metadata["state"]))
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
				if tmuxRequested && dops != nil {
					_ = dops.clearRestartRequested(name)
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
				_ = store.SetMetadataBatch(session.ID, batch)
				if session.Metadata == nil {
					session.Metadata = make(map[string]string, len(batch))
				}
				for key, value := range batch {
					session.Metadata[key] = value
				}
				if alive {
					if err := workerKillSessionTargetWithConfig("", store, sp, cfg, name); err != nil {
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
						// Diagnostic: log per-field breakdown to identify the drifting field.
						var storedBreakdown map[string]string
						if raw := session.Metadata["core_hash_breakdown"]; raw != "" {
							_ = json.Unmarshal([]byte(raw), &storedBreakdown)
						}
						runtime.LogCoreFingerprintDrift(stderr, name, storedBreakdown, agentCfg)
						restartedInPlace := false
						if isNamedSessionBead(*session) {
							// Defer config-drift restart for named sessions
							// that are actively in use (pending interaction,
							// tmux-attached, or recent activity). This prevents
							// draining a working agent mid-task without graceful
							// handoff. See gastownhall/gascity#119.
							activeReason, active, deferErr := shouldDeferNamedSessionConfigDrift(*session, store, sp, name, clk, storedHash+":"+currentHash)
							if deferErr != nil {
								fmt.Fprintf(stderr, "session reconciler: recording config-drift deferral for %s: %v\n", name, deferErr) //nolint:errcheck
							}
							if active {
								if trace != nil {
									trace.recordDecision("reconciler.session.config_drift", tp.TemplateName, name, "config_drift", string(TraceOutcomeDeferredActive), traceRecordPayload{
										"stored_hash":   storedHash,
										"current_hash":  currentHash,
										"active_reason": activeReason,
									}, nil, "")
								}
								continue
							}
							resetConfiguredNamedSessionForConfigDrift(session, store, sp, name, alive, "creating", stderr)
							if trace != nil {
								trace.recordDecision("reconciler.session.config_drift", tp.TemplateName, name, "config_drift", "restart_in_place", traceRecordPayload{
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
									trace.recordDecision("reconciler.session.config_drift", tp.TemplateName, name, "pending", "deferred_pending", traceRecordPayload{
										"stored_hash":    storedHash,
										"current_hash":   currentHash,
										"drain_canceled": drainCancelled,
									}, nil, "")
								}
								continue
							}
							attached, err := workerSessionTargetAttachedWithConfig(cityPath, store, sp, cfg, session.ID)
							if err == nil && attached {
								if trace != nil {
									trace.recordDecision("reconciler.session.config_drift", tp.TemplateName, name, "config_drift", "deferred_attached", traceRecordPayload{
										"stored_hash":  storedHash,
										"current_hash": currentHash,
									}, nil, "")
								}
								continue
							}
							// Defer ordinary-session config-drift drain while a
							// user is attached. Named-session config drift is
							// non-deferrable and is handled above.
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
							if beginSessionDrain(*session, sp, dt, "config-drift", clk, ddt) {
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
							}
							continue
						}
					}

					if isNamedSessionBead(*session) {
						if err := clearNamedSessionConfigDriftDeferral(*session, store); err != nil {
							fmt.Fprintf(stderr, "session reconciler: clearing config-drift deferral for %s: %v\n", name, err) //nolint:errcheck
						}
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
						resetConfiguredNamedSessionForConfigDrift(session, store, sp, name, false, "asleep", stderr)
						if trace != nil {
							trace.recordDecision("reconciler.session.config_drift", tp.TemplateName, name, "config_drift", "repair_in_place", traceRecordPayload{
								"stored_hash":  storedHash,
								"current_hash": currentHash,
							}, nil, "")
						}
						continue
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
			if pendingCreateStartInFlight(*target.session, clk, startupTimeout) {
				if trace != nil {
					trace.recordDecision("reconciler.session.wake", target.tp.TemplateName, name, "wake", "start_in_flight", traceRecordPayload{
						"pending_create_claim": strings.TrimSpace(target.session.Metadata["pending_create_claim"]),
						"last_woke_at":         target.session.Metadata["last_woke_at"],
					}, nil, "")
				}
				continue
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
			hasAssignedWork, assignedErr = sessionHasOpenAssignedWork(store, rigStores, *target.session)
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

	plannedWakes := executePlannedStartsTraced(
		ctx, startCandidates, cfg, desiredState, sp, store, cityName,
		cityPath,
		clk, rec, startupTimeout, stdout, stderr, trace,
		startOptions...,
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

// sessionHasOpenAssignedWork reports whether any open or in-progress
// work bead is assigned to the given session across the primary store
// AND any attached rig stores. This preserves cross-store ownership
// coverage that used to come from the retired ownership snapshot.
//
// A session's work bead can live in a rig store (e.g., a city-stored
// session whose work was routed to a rig), so the close gate and
// drain-ack must check every store the bead could live in before
// recycling the session's slot. Live queries are used throughout:
// any individual store failure fails the whole check closed so
// transient errors cannot cause premature close.
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
	namedSessionConfigDriftDeferredAtMetadata          = "config_drift_deferred_at"
	namedSessionConfigDriftDeferredKeyMetadata         = "config_drift_deferred_key"
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

func clearNamedSessionConfigDriftDeferral(session beads.Bead, store beads.Store) error {
	if store == nil || session.ID == "" {
		return nil
	}
	if session.Metadata[namedSessionConfigDriftDeferredAtMetadata] == "" &&
		session.Metadata[namedSessionConfigDriftDeferredKeyMetadata] == "" {
		return nil
	}
	return store.SetMetadataBatch(session.ID, map[string]string{
		namedSessionConfigDriftDeferredAtMetadata:  "",
		namedSessionConfigDriftDeferredKeyMetadata: "",
	})
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
	batch := sessionpkg.ConfigDriftResetPatch(sessionpkg.State(nextState), newSessionKey)
	batch[namedSessionConfigDriftDeferredAtMetadata] = ""
	batch[namedSessionConfigDriftDeferredKeyMetadata] = ""
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
