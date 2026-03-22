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
	"github.com/gastownhall/gascity/internal/telemetry"
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
func reconcileSessionBeads(
	ctx context.Context,
	sessions []beads.Bead,
	desiredState map[string]TemplateParams,
	configuredNames map[string]bool,
	cfg *config.City,
	sp runtime.Provider,
	store beads.Store,
	dops drainOps,
	workSet map[string]bool,
	readyWaitSet map[string]bool,
	dt *drainTracker,
	poolDesired map[string]int,
	cityName string,
	it idleTracker,
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
	var startCandidates []startCandidate
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

		// Zombie capture: session exists but process dead — grab scrollback for forensics.
		if sp.IsRunning(name) && !alive {
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
			rollbackPendingCreate(session, store, clk.Now().UTC(), stderr)
			continue
		}

		// Heal advisory state metadata.
		healState(session, alive, store)

		// Stability check: detect rapid exit (crash).
		if checkStability(session, cfg, alive, dt, store, clk) {
			continue // crash recorded, skip further processing
		}

		// Clear wake failures for sessions that have been stable long enough.
		if alive && stableLongEnough(*session, clk) {
			clearWakeFailures(session, store)
		}
		if alive && shouldRollbackPendingCreate(session) {
			if err := clearPendingCreateClaim(session, store); err != nil {
				fmt.Fprintf(stderr, "session reconciler: clearing pending create claim for %s: %v\n", name, err) //nolint:errcheck
			}
		}

		// Drain-ack: agent signaled it's done (gc runtime drain-ack).
		// Stop the session immediately so the pool can reclaim the slot
		// and a fresh session handles the next work item.
		if alive && dops != nil {
			if acked, _ := dops.isDrainAcked(name); acked {
				_ = dops.clearDrain(name)
				if err := sp.Stop(name); err != nil {
					fmt.Fprintf(stderr, "session reconciler: stopping drain-acked %s: %v\n", name, err) //nolint:errcheck
				} else {
					fmt.Fprintf(stdout, "Stopped drain-acked session '%s'\n", name) //nolint:errcheck
					rec.Record(events.Event{
						Type:    events.SessionStopped,
						Actor:   "gc",
						Subject: tp.DisplayName(),
						Message: "drain acknowledged by agent",
					})
				}
				continue
			}
		}

		// Restart-requested: agent asked for a fresh session
		// (gc runtime request-restart). Stop immediately; the next
		// tick will re-create and re-wake.
		if alive && dops != nil {
			if requested, _ := dops.isRestartRequested(name); requested {
				_ = dops.clearRestartRequested(name)
				if err := sp.Stop(name); err != nil {
					fmt.Fprintf(stderr, "session reconciler: stopping restart-requested %s: %v\n", name, err) //nolint:errcheck
				} else {
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
						// Defer config-drift drain while a user is attached.
						// Killing a session mid-conversation is disruptive;
						// the drift will be applied when the user detaches.
						if sp.IsAttached(name) {
							continue
						}
						ddt := driftDrainTimeout
						if ddt <= 0 {
							ddt = defaultDrainTimeout
						}
						beginSessionDrain(*session, sp, dt, "config-drift", clk, ddt)
						fmt.Fprintf(stdout, "Draining session '%s': config-drift\n", name) //nolint:errcheck
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
				alive = false
			}
			// Fall through to wakeReasons — it will re-wake immediately if config present
		}

		// Compute wake reasons using the full contract (includes held_until,
		// attachment checks, pool desired counts).
		reasons := wakeReasons(*session, cfg, sp, poolDesired, workSet, readyWaitSet, clk)
		shouldWake := len(reasons) > 0

		if shouldWake && !alive {
			// Session should be awake but isn't — wake it.
			if sessionIsQuarantined(*session, clk) {
				continue // crash-loop protection
			}
			startCandidates = append(startCandidates, startCandidate{
				session: session,
				tp:      tp,
				order:   len(startCandidates),
			})
		}

		if shouldWake && alive {
			// Session is correctly awake. Cancel any non-drift drain
			// (handles scale-back-up: agent returns to desired set while draining).
			cancelSessionDrain(*session, dt)
		}

		if !shouldWake && alive {
			// No reason to be awake — begin drain.
			reason := "no-wake-reason"
			if intent := session.Metadata["sleep_intent"]; intent != "" {
				reason = intent
			}
			beginSessionDrain(*session, sp, dt, reason, clk, defaultDrainTimeout)
			fmt.Fprintf(stdout, "Draining session '%s': %s\n", name, reason) //nolint:errcheck
		}
	}

	plannedWakes := executePlannedStarts(
		ctx, startCandidates, cfg, desiredState, sp, store, cityName,
		clk, rec, startupTimeout, stdout, stderr,
	)

	// Phase 2: Advance all in-flight drains.
	sessionLookup := func(id string) *beads.Bead {
		return beadByID[id]
	}
	advanceSessionDrains(dt, sp, store, sessionLookup, cfg, poolDesired, workSet, readyWaitSet, clk)

	return plannedWakes
}

// resolveTaskWorkDir checks the agent's assigned task beads for a work_dir
// metadata field. If a task bead has work_dir set and the directory exists
// on disk, that path is returned. This lets the reconciler start the agent
// in the worktree that the previous session (or this session's prior run)
// created, without any prompt-side logic.
func resolveTaskWorkDir(store beads.Store, agentName string) string {
	assigned, err := store.ListByAssignee(agentName, "in_progress", 0)
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
// On first start (no prior session exists), it uses SessionIDFlag to create a
// session with the given key as its ID. On subsequent wakes, it uses
// resolveResumeCommand to resume the existing session.
func resolveSessionCommand(command, sessionKey string, rp *config.ResolvedProvider, firstStart bool) string {
	if firstStart && rp.SessionIDFlag != "" {
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
