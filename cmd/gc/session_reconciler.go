// session_reconciler.go implements the bead-driven reconciliation loop.
// It replaces doReconcileAgents with a wake/sleep model: for each session
// bead, compute whether the session should be awake, and manage lifecycle
// transitions using the Phase 2 building blocks.
//
// This reconciler uses desiredState (map[string]TemplateParams) for config
// queries and runtime.Provider directly for lifecycle operations. There
// is no dependency on agent types.
package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

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

// derivePoolDesired computes pool desired counts from the desired state map.
// Since buildDesiredState already ran evaluatePool, the number of instances
// per template in the desired state IS the desired count.
func derivePoolDesired(desiredState map[string]TemplateParams, cfg *config.City) map[string]int {
	if cfg == nil {
		return nil
	}
	counts := make(map[string]int)
	for _, tp := range desiredState {
		cfgAgent := findAgentByTemplate(cfg, tp.TemplateName)
		if cfgAgent != nil && cfgAgent.Pool != nil {
			counts[tp.TemplateName]++
		}
	}
	return counts
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
) bool {
	template := session.Metadata["template"]
	cfgAgent := findAgentByTemplate(cfg, template)
	if cfgAgent == nil || len(cfgAgent.DependsOn) == 0 {
		return true
	}
	st := cfg.Workspace.SessionTemplate
	for _, dep := range cfgAgent.DependsOn {
		depCfg := findAgentByTemplate(cfg, dep)
		if depCfg == nil {
			continue // dependency not in config — skip
		}
		if depCfg.Pool != nil {
			// Pool: check if any instance is alive via Provider (includes zombie detection).
			anyAlive := false
			for sn, tp := range desiredState {
				if tp.TemplateName == dep && sp.IsRunning(sn) && sp.ProcessAlive(sn, tp.Hints.ProcessNames) {
					anyAlive = true
					break
				}
			}
			if !anyAlive {
				return false
			}
		} else {
			// Fixed agent: check single instance via Provider.
			sn := agent.SessionNameFor(cityName, dep, st)
			depTP, hasDep := desiredState[sn]
			var depProcessNames []string
			if hasDep {
				depProcessNames = depTP.Hints.ProcessNames
			}
			if !sp.IsRunning(sn) || !sp.ProcessAlive(sn, depProcessNames) {
				return false
			}
		}
	}
	return true
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
func reconcileSessionBeads(
	ctx context.Context,
	sessions []beads.Bead,
	desiredState map[string]TemplateParams,
	configuredNames map[string]bool,
	cfg *config.City,
	sp runtime.Provider,
	store beads.Store,
	workSet map[string]bool,
	dt *drainTracker,
	poolDesired map[string]int,
	cityName string,
	clk clock.Clock,
	rec events.Recorder,
	startupTimeout time.Duration,
	driftDrainTimeout time.Duration,
	stdout, stderr io.Writer,
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
	wakeCount := 0
	for i := range ordered {
		session := &ordered[i]

		// Skip beads with unrecognized states. This enables forward-compatible
		// rollback: if a newer version writes "draining" or "archived", the
		// older reconciler ignores those beads rather than crashing.
		if !isKnownState(*session) {
			fmt.Fprintf(stderr, "session reconciler: skipping %s with unknown state %q\n", //nolint:errcheck // best-effort stderr
				session.Metadata["session_name"], session.Metadata["state"])
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
			healState(session, providerAlive, store)
			if providerAlive {
				reason := "orphaned"
				if configuredNames[name] {
					reason = "suspended"
				}
				beginSessionDrain(*session, sp, dt, reason, clk, defaultDrainTimeout)
				fmt.Fprintf(stdout, "Draining session '%s': %s\n", name, reason) //nolint:errcheck
			} else {
				// Not running and not desired — close the bead.
				reason := "orphaned"
				if configuredNames[name] {
					reason = "suspended"
				}
				closeBead(store, session.ID, reason, clk.Now().UTC(), stderr)
			}
			continue
		}

		// Liveness includes zombie detection: tmux session exists AND
		// the expected child process is alive (when ProcessNames configured).
		alive := sp.IsRunning(name) && sp.ProcessAlive(name, tp.Hints.ProcessNames)

		// Heal advisory state metadata.
		healState(session, alive, store)

		// Stability check: detect rapid exit (crash).
		if checkStability(session, alive, dt, store, clk) {
			continue // crash recorded, skip further processing
		}

		// Clear wake failures for sessions that have been stable long enough.
		if alive && stableLongEnough(*session, clk) {
			clearWakeFailures(session, store)
		}

		// Config drift: if alive and config changed, drain for restart.
		// Live-only drift: re-apply session_live without restart.
		if alive {
			template := session.Metadata["template"]
			storedHash := session.Metadata["config_hash"]
			if sh := session.Metadata["started_config_hash"]; sh != "" {
				storedHash = sh
			}
			if template != "" && storedHash != "" {
				cfgAgent := findAgentByTemplate(cfg, template)
				if cfgAgent != nil {
					agentCfg := templateParamsToConfig(tp)
					currentHash := runtime.CoreFingerprint(agentCfg)
					if storedHash != currentHash {
						ddt := driftDrainTimeout
						if ddt <= 0 {
							ddt = defaultDrainTimeout
						}
						beginSessionDrain(*session, sp, dt, "config-drift", clk, ddt)
						fmt.Fprintf(stdout, "Draining session '%s': config-drift\n", name) //nolint:errcheck
						rec.Record(events.Event{
							Type:    events.AgentDraining,
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
					if storedLive != "" {
						currentLive := runtime.LiveFingerprint(agentCfg)
						if storedLive != currentLive {
							fmt.Fprintf(stdout, "Live config changed for '%s', re-applying...\n", tp.DisplayName()) //nolint:errcheck
							if err := sp.RunLive(name, agentCfg); err != nil {
								fmt.Fprintf(stderr, "session reconciler: RunLive %s: %v\n", name, err) //nolint:errcheck
							} else {
								_ = store.SetMetadataBatch(session.ID, map[string]string{
									"live_hash":         currentLive,
									"started_live_hash": currentLive,
								})
								rec.Record(events.Event{
									Type:    events.AgentUpdated,
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

		// Compute wake reasons using the full contract (includes held_until,
		// attachment checks, pool desired counts).
		reasons := wakeReasons(*session, cfg, sp, poolDesired, workSet, clk)
		shouldWake := len(reasons) > 0

		if shouldWake && !alive {
			// Session should be awake but isn't — wake it.
			if sessionIsQuarantined(*session, clk) {
				continue // crash-loop protection
			}
			if wakeCount >= defaultMaxWakesPerTick {
				continue // budget exceeded, defer to next tick
			}
			if !allDependenciesAlive(*session, cfg, desiredState, sp, cityName) {
				continue // dependencies not ready
			}

			// Two-phase wake: persist metadata BEFORE starting process.
			if _, _, err := preWakeCommit(session, store, clk); err != nil {
				fmt.Fprintf(stderr, "session reconciler: pre-wake %s: %v\n", name, err) //nolint:errcheck
				continue
			}

			// Start via Provider directly with startup timeout.
			startCtx := ctx
			var startCancel context.CancelFunc
			if startupTimeout > 0 {
				startCtx, startCancel = context.WithTimeout(ctx, startupTimeout)
			}
			agentCfg := templateParamsToConfig(tp)
			err := sp.Start(startCtx, name, agentCfg)
			if startCancel != nil {
				startCancel()
			}
			if err != nil {
				fmt.Fprintf(stderr, "session reconciler: starting %s: %v\n", name, err) //nolint:errcheck
				// Clear last_woke_at so checkStability on the next tick
				// doesn't see a recent wake and double-count this failure.
				_ = store.SetMetadata(session.ID, "last_woke_at", "")
				session.Metadata["last_woke_at"] = ""
				recordWakeFailure(session, store, clk)
				continue
			}

			wakeCount++
			fmt.Fprintf(stdout, "Woke session '%s'\n", tp.DisplayName()) //nolint:errcheck
			rec.Record(events.Event{
				Type:    events.AgentStarted,
				Actor:   "gc",
				Subject: tp.DisplayName(),
			})

			// Store config fingerprint after successful start.
			coreHash := runtime.CoreFingerprint(agentCfg)
			liveHash := runtime.LiveFingerprint(agentCfg)
			if err := store.SetMetadataBatch(session.ID, map[string]string{
				"config_hash":         coreHash,
				"started_config_hash": coreHash,
				"live_hash":           liveHash,
				"started_live_hash":   liveHash,
			}); err != nil {
				fmt.Fprintf(stderr, "session reconciler: storing hashes for %s: %v\n", name, err) //nolint:errcheck
			}
		}

		if shouldWake && alive {
			// Session is correctly awake. Cancel any non-drift drain
			// (handles scale-back-up: agent returns to desired set while draining).
			cancelSessionDrain(*session, dt)
		}

		if !shouldWake && alive {
			// No reason to be awake — begin drain.
			beginSessionDrain(*session, sp, dt, "no-wake-reason", clk, defaultDrainTimeout)
			fmt.Fprintf(stdout, "Draining session '%s': no-wake-reason\n", name) //nolint:errcheck
		}
	}

	// Phase 2: Advance all in-flight drains.
	sessionLookup := func(id string) *beads.Bead {
		return beadByID[id]
	}
	advanceSessionDrains(dt, sp, store, sessionLookup, cfg, poolDesired, workSet, clk)

	return wakeCount
}
