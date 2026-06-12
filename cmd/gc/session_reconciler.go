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
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"runtime/debug"
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

const maxIdleSleepProbesPerTick = 3

type wakeTarget struct {
	session *beads.Bead
	tp      TemplateParams
	alive   bool
}

func lifecycleTimerBlocker(metadata map[string]string, now time.Time) string {
	switch {
	case metadataTimeInFuture(metadata["held_until"], now):
		return "user_hold"
	case metadataTimeInFuture(metadata["quarantined_until"], now):
		return "quarantine"
	default:
		return ""
	}
}

func isDrainAckStopPending(session beads.Bead) bool {
	return strings.TrimSpace(session.Metadata["state"]) == string(sessionpkg.StateDraining) &&
		strings.TrimSpace(session.Metadata["state_reason"]) == sessionpkg.DrainAckStopPendingReason
}

func markDrainAckStopPending(session *beads.Bead, store beads.Store, clk clock.Clock, stderr io.Writer) bool {
	if session == nil || store == nil || session.ID == "" {
		return false
	}
	if stderr == nil {
		stderr = io.Discard
	}
	batch := sessionpkg.DrainAckStopPendingPatch(clk.Now().UTC())
	if err := store.SetMetadataBatch(session.ID, batch); err != nil {
		name := strings.TrimSpace(session.Metadata["session_name"])
		if name == "" {
			name = session.ID
		}
		fmt.Fprintf(stderr, "session reconciler: marking drain-ack stop-pending %s: %v\n", name, err) //nolint:errcheck
		return false
	}
	if session.Metadata == nil {
		session.Metadata = make(map[string]string, len(batch))
	}
	for key, value := range batch {
		session.Metadata[key] = value
	}
	return true
}

func clearDrainTrackerForStopPending(session *beads.Bead, dt *drainTracker) {
	if session == nil || dt == nil {
		return
	}
	dt.clearIdleProbe(session.ID)
	dt.remove(session.ID)
}

func assignedWorkDrainCancelReason(session beads.Bead, sp runtime.Provider, dt *drainTracker, name string) string {
	if dt != nil {
		if ds := dt.get(session.ID); ds != nil && assignedWorkDrainReasonCancelable(ds.reason) {
			return ds.reason
		}
	}
	if reason, ok := reconcilerDrainAckMatchesSession(session, sp, name); ok && assignedWorkDrainReasonCancelable(reason) {
		return reason
	}
	return "orphaned"
}

func resetPendingCommittedAt(session beads.Bead) (string, time.Time, bool) {
	if strings.TrimSpace(session.Metadata["continuation_reset_pending"]) != "true" {
		return "", time.Time{}, false
	}
	raw := strings.TrimSpace(session.Metadata[sessionpkg.ResetCommittedAtKey])
	if raw == "" {
		return "", time.Time{}, false
	}
	committedAt, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return "", time.Time{}, false
	}
	return raw, committedAt, true
}

func recordResetStallIfDue(
	session beads.Bead,
	template string,
	name string,
	alive bool,
	startupTimeout time.Duration,
	now time.Time,
	dt *drainTracker,
	rec events.Recorder,
	stderr io.Writer,
	trace *sessionReconcilerTraceCycle,
) {
	resetCommittedAt, committedAt, pending := resetPendingCommittedAt(session)
	if !pending {
		if dt != nil {
			dt.clearResetStall(session.ID)
		}
		return
	}
	if alive || startupTimeout <= 0 {
		return
	}
	elapsed := now.Sub(committedAt)
	if elapsed <= startupTimeout {
		return
	}
	if dt != nil && !dt.markResetStall(session.ID) {
		return
	}
	if stderr == nil {
		stderr = io.Discard
	}
	elapsedSeconds := int(elapsed / time.Second)
	msg := fmt.Sprintf(
		"session reconciler: reset stalled for %s: elapsed_s=%d reset_committed_at=%s bead_id=%s",
		name, elapsedSeconds, resetCommittedAt, session.ID,
	)
	fmt.Fprintln(stderr, msg) //nolint:errcheck

	if rec != nil {
		rec.Record(events.Event{
			Type:    events.SessionResetStalled,
			Actor:   "gc",
			Subject: name,
			Message: msg,
			Payload: events.SessionResetStalledPayloadJSON(name, template, resetCommittedAt, elapsedSeconds),
		})
	}
	if trace != nil {
		trace.RecordDecision(
			TraceSiteReconcilerResetStalled,
			TraceReasonResetStalled,
			TraceOutcomeFailed,
			template,
			name,
			map[string]any{
				"bead_id":            session.ID,
				"elapsed_s":          elapsedSeconds,
				"reset_committed_at": resetCommittedAt,
				"startup_timeout_s":  int(startupTimeout / time.Second),
			},
		)
	}
}

func drainAckAsyncStopKey(sessionID, name string) string {
	if id := strings.TrimSpace(sessionID); id != "" {
		return "id:" + id
	}
	return "name:" + strings.TrimSpace(name)
}

// drainAckAsyncStopPokeController is a mutable test seam over pokeController
// for the async drain-ack stop path (see queueDrainAckAsyncStop).
var drainAckAsyncStopPokeController = pokeController

func queueDrainAckAsyncStop(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, sessionID, name string, tracker *asyncStartTracker, stderr io.Writer) {
	name = strings.TrimSpace(name)
	if name == "" || sp == nil {
		return
	}
	if stderr == nil {
		stderr = io.Discard
	}
	key := drainAckAsyncStopKey(sessionID, name)
	done, tracking := tracker.startDrainAckStop(key)
	if !tracking {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(stderr, "session reconciler: async drain-ack stop %s panicked: %v\n%s", name, r, debug.Stack()) //nolint:errcheck
			}
			done()
		}()
		if err := workerKillSessionTargetWithConfig(cityPath, store, sp, cfg, name); err != nil && !runtime.IsSessionGone(err) {
			fmt.Fprintf(stderr, "session reconciler: async drain-ack stop %s: %v\n", name, err) //nolint:errcheck
			return
		}
		// The runtime session is now gone, but its pool session bead stays open
		// (occupying the pool slot) until finalizeDrainAckStopPendingSessions
		// closes it on a subsequent tick. Poke the controller so finalize +
		// pool respawn runs on the next event-driven tick instead of waiting up
		// to a full patrol interval (ga-ryhnhd). Mirrors the drain-ack CLI poke.
		// Poke is best-effort: a failure is not logged because the goroutine may
		// outlive its reconcile invocation and write to stderr concurrently with
		// the caller's subsequent writes on the same writer (data race on
		// non-goroutine-safe buffers). The controller reconciles on the next
		// patrol tick regardless.
		_ = drainAckAsyncStopPokeController(cityPath)
	}()
}

func recordDrainAckAssignedWorkEvent(
	cityPath string,
	cfg *config.City,
	store beads.Store,
	rigStores map[string]beads.Store,
	session beads.Bead,
	subject string,
	template string,
	name string,
	rec events.Recorder,
	stderr io.Writer,
) {
	if rec == nil {
		return
	}
	strandedBead, found, beadLookupErr := firstOpenAssignedWorkBeadForReachableStore(cityPath, cfg, store, rigStores, session)
	if beadLookupErr != nil {
		fmt.Fprintf(stderr, "session reconciler: locating stranded bead for drain-acked %s: %v\n", name, beadLookupErr) //nolint:errcheck
	}
	if !found {
		return
	}
	rec.Record(events.Event{
		Type:    events.SessionDrainAckedWithAssignedWork,
		Actor:   "gc",
		Subject: subject,
		Message: "session drain-acked while still assigned to work bead",
		Payload: api.SessionDrainAckedWithAssignedWorkPayloadJSON(
			session.ID,
			strandedBead.ID,
			template,
			strandedBead.Status,
			"drain_acked_with_assigned_work",
		),
	})
}

func finalizeDrainAckStoppedSession(
	cityPath string,
	cfg *config.City,
	store beads.Store,
	rigStores map[string]beads.Store,
	session *beads.Bead,
	template string,
	closeIfUnassigned bool,
	dops drainOps,
	dt *drainTracker,
	clk clock.Clock,
	rec events.Recorder,
	stderr io.Writer,
) {
	if session == nil || store == nil || session.ID == "" {
		return
	}
	name := strings.TrimSpace(session.Metadata["session_name"])
	if template == "" {
		template = normalizedSessionTemplate(*session, cfg)
	}
	if template == "" {
		template = session.Metadata["template"]
	}
	recordStopped := func() {
		if rec == nil {
			return
		}
		rec.Record(events.Event{
			Type:    events.SessionStopped,
			Actor:   "gc",
			Subject: template,
			Message: "drain acknowledged by agent",
			Payload: api.SessionLifecyclePayloadJSON(session.ID, template, "drain acknowledged"),
		})
	}
	hasAssignedWork, assignedErr := sessionHasOpenAssignedWorkForReachableStore(cityPath, cfg, store, rigStores, *session)
	if assignedErr != nil {
		fmt.Fprintf(stderr, "session reconciler: checking assigned work for drain-acked %s: %v\n", name, assignedErr) //nolint:errcheck
		hasAssignedWork = true
	}
	if closeIfUnassigned && !hasAssignedWork {
		if closeSessionBeadIfReachableStoreUnassigned(cityPath, cfg, store, rigStores, *session, "drained", clk.Now().UTC(), stderr) {
			session.Status = "closed"
			if session.Metadata == nil {
				session.Metadata = make(map[string]string)
			}
			for key, value := range sessionpkg.ClosePatch(clk.Now().UTC(), "drained") {
				session.Metadata[key] = value
			}
			if dops != nil {
				_ = dops.clearDrain(name)
			}
			if dt != nil {
				dt.clearIdleProbe(session.ID)
				dt.remove(session.ID)
			}
			recordStopped()
			return
		}
		if latest, err := store.Get(session.ID); err == nil && latest.Status == "closed" {
			session.Status = latest.Status
			session.Metadata = latest.Metadata
			if dops != nil {
				_ = dops.clearDrain(name)
			}
			if dt != nil {
				dt.clearIdleProbe(session.ID)
				dt.remove(session.ID)
			}
			recordStopped()
			return
		}
		assignedAfterCloseGate, closeGateAssignedErr := sessionHasOpenAssignedWorkForReachableStore(cityPath, cfg, store, rigStores, *session)
		if closeGateAssignedErr != nil {
			fmt.Fprintf(stderr, "session reconciler: checking assigned work after failed drain-ack close gate for %s: %v\n", name, closeGateAssignedErr) //nolint:errcheck
			assignedAfterCloseGate = true
		}
		if assignedAfterCloseGate {
			hasAssignedWork = true
		}
	}
	batch := sessionpkg.AcknowledgeDrainPatch(session.Metadata["wake_mode"] == "fresh")
	if hasAssignedWork {
		batch = sessionpkg.CompleteDrainPatch(clk.Now().UTC(), "idle", session.Metadata["wake_mode"] == "fresh")
	}
	// A drain-ack that completes a restart-request cycle (gc session reset →
	// agent drain-ack) must also consume restart_requested. The drain-ack
	// branch handles the stop and continues before the restart-requested
	// branch runs, so nothing else clears the flag; if it survives in the
	// store, a later cache-reconcile re-emission resurrects it and the
	// controller honors it as a fresh restart request — a phantom second
	// restart that rotates session_key and destroys resume continuity (#2574).
	if session.Metadata["restart_requested"] == "true" {
		batch["restart_requested"] = ""
	}
	if err := store.SetMetadataBatch(session.ID, batch); err != nil {
		fmt.Fprintf(stderr, "session reconciler: finalizing drain-ack stopped %s: %v\n", name, err) //nolint:errcheck
		return
	}
	if session.Metadata == nil {
		session.Metadata = make(map[string]string, len(batch))
	}
	for key, value := range batch {
		session.Metadata[key] = value
	}
	if dops != nil {
		_ = dops.clearDrain(name)
	}
	if dt != nil {
		dt.clearIdleProbe(session.ID)
		dt.remove(session.ID)
	}
	recordStopped()
	if hasAssignedWork {
		recordDrainAckAssignedWorkEvent(cityPath, cfg, store, rigStores, *session, template, template, name, rec, stderr)
	}
}

func reconcileDrainAckStopPending(
	cityPath string,
	cfg *config.City,
	sp runtime.Provider,
	store beads.Store,
	rigStores map[string]beads.Store,
	session *beads.Bead,
	tp TemplateParams,
	desired bool,
	dops drainOps,
	dt *drainTracker,
	asyncStopTracker *asyncStartTracker,
	clk clock.Clock,
	rec events.Recorder,
	stderr io.Writer,
) bool {
	if session == nil || !isDrainAckStopPending(*session) {
		return false
	}
	name := strings.TrimSpace(session.Metadata["session_name"])
	obs, err := workerObserveSessionTargetWithRuntimeHintsWithConfig(cityPath, store, sp, cfg, session.ID, tp.Hints.ProcessNames)
	if err != nil || obs.Running || obs.Alive {
		queueDrainAckAsyncStop(cityPath, store, sp, cfg, session.ID, name, asyncStopTracker, stderr)
		return true
	}
	finalizeDrainAckStoppedSession(
		cityPath, cfg, store, rigStores, session, tp.TemplateName,
		!desired || isPoolManagedSessionBead(*session),
		dops, dt, clk, rec, stderr,
	)
	return true
}

func finalizeDrainAckStopPendingSessions(
	cityPath string,
	cfg *config.City,
	sp runtime.Provider,
	store beads.Store,
	rigStores map[string]beads.Store,
	sessions []beads.Bead,
	dops drainOps,
	dt *drainTracker,
	asyncStopTracker *asyncStartTracker,
	clk clock.Clock,
	rec events.Recorder,
	stderr io.Writer,
) int {
	if store == nil || sp == nil || len(sessions) == 0 {
		return 0
	}
	finalized := 0
	for i := range sessions {
		session := &sessions[i]
		if !isDrainAckStopPending(*session) {
			continue
		}
		name := strings.TrimSpace(session.Metadata["session_name"])
		obs, err := workerObserveSessionTargetWithRuntimeHintsWithConfig(cityPath, store, sp, cfg, session.ID, nil)
		if err != nil || obs.Running || obs.Alive {
			queueDrainAckAsyncStop(cityPath, store, sp, cfg, session.ID, name, asyncStopTracker, stderr)
			continue
		}
		// Pool-managed stop-pending beads close here instead of staying open as
		// state=drained: open pool session beads occupy slots in the next demand
		// calculation, while closed beads remain only as lifecycle history.
		finalizeDrainAckStoppedSession(
			cityPath, cfg, store, rigStores, session,
			normalizedSessionTemplate(*session, cfg),
			isPoolManagedSessionBead(*session),
			dops, dt, clk, rec, stderr,
		)
		finalized++
	}
	return finalized
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
	if strings.TrimSpace(session.Metadata["pending_create_claim"]) != "true" &&
		sessionpkg.State(strings.TrimSpace(session.Metadata["state"])) != sessionpkg.StateCreating {
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
	if !pendingCreateRollbackState(session.Metadata["state"]) {
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
	state := sessionpkg.State(strings.TrimSpace(session.Metadata["state"]))
	if !pendingCreateRollbackState(string(state)) {
		return false
	}
	if state == sessionpkg.StateAsleep {
		if strings.TrimSpace(session.Metadata["last_woke_at"]) == "" {
			return pendingCreateNeverStartedExpired(session, clk)
		}
		return pendingCreateAttemptStale(session, clk)
	}
	if pendingCreateStartInFlight(session, clk, startupTimeout) {
		return false
	}
	if strings.TrimSpace(session.Metadata["last_woke_at"]) == "" {
		return pendingCreateNeverStartedExpired(session, clk)
	}
	return pendingCreateAttemptStale(session, clk)
}

func pendingCreateQueuedOrCreatingState(state string) bool {
	switch sessionpkg.State(strings.TrimSpace(state)) {
	case sessionpkg.StateStartPending, sessionpkg.StateCreating:
		return true
	default:
		return false
	}
}

func pendingCreateRollbackState(state string) bool {
	if pendingCreateQueuedOrCreatingState(state) {
		return true
	}
	return sessionpkg.State(strings.TrimSpace(state)) == sessionpkg.StateAsleep
}

func pendingResumePreservingNamedRestart(session beads.Bead, clk clock.Clock, startupTimeout time.Duration) bool {
	switch sessionpkg.State(strings.TrimSpace(session.Metadata["state"])) {
	case sessionpkg.StateStartPending, sessionpkg.StateCreating:
	default:
		return false
	}
	if strings.TrimSpace(session.Metadata["pending_create_claim"]) != "true" {
		return false
	}
	if strings.TrimSpace(session.Metadata["session_key"]) == "" {
		return false
	}
	if strings.TrimSpace(session.Metadata["started_config_hash"]) == "" {
		return false
	}
	if _, ok := parseRFC3339Metadata(session.Metadata["pending_create_started_at"]); !ok {
		return false
	}
	if !pendingCreateLeaseActive(session, clk, startupTimeout) {
		return false
	}
	return true
}

func wakeDemandOverridesSleepSuppression(
	decision AwakeDecision,
	eval wakeEvaluation,
	policy resolvedSessionSleepPolicy,
	poolDesired map[string]int,
	template string,
	hasExplicitSleepIntent bool,
) bool {
	if hasExplicitSleepIntent {
		return false
	}
	if eval.HasAssignedWork {
		return true
	}
	hasDemand := poolDesired[template] > 0
	if hasDemand && policy.Class == config.SessionSleepNonInteractive {
		return true
	}
	return decision.Reason == "min-active" && containsWakeReason(eval.Reasons, WakeConfig)
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
		ctx, cityPath, sessions, desiredState, configuredNames, cfg, sp, store, dops, assignedWorkBeads, rigStores, readyWaitSet, dt, nil,
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
	gate *providerHealthGate,
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
		ctx, cityPath, sessions, desiredState, configuredNames, cfg, sp, store, dops, assignedWorkBeads, rigStores, readyWaitSet, dt, gate,
		poolDesired, namedSessionDemand, storeQueryPartial, workSet, cityName, it, clk, rec, startupTimeout, driftDrainTimeout, stdout, stderr, nil,
	)
}

//nolint:unparam // compatibility wrapper keeps the established traced test/helper signature.
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
		ctx, cityPath, sessions, desiredState, configuredNames, cfg, sp, store, dops, assignedWorkBeads, rigStores, readyWaitSet, dt, nil,
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
	gate *providerHealthGate,
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
	// Load provider-health snapshot once per tick (ADR-0013 A1 M3a).
	// All per-session gate checks in Phase 2 use this snapshot — no I/O per session.
	phSnap := loadProviderHealthSnapshot(cityPath)
	reconcileOpts := startExecutionOptions{}
	for _, apply := range startOptions {
		if apply != nil {
			apply(&reconcileOpts)
		}
	}
	effectiveStartOptions := startOptions
	if !storeQueryPartial && reconcileOpts.workDirResolver == nil && len(assignedWorkBeads) > 0 {
		effectiveStartOptions = append(append([]startExecutionOption(nil), startOptions...), withTaskWorkDirResolver(newAssignedTaskWorkDirResolver(assignedWorkBeads)))
	}
	if startupTimeout <= 0 && cfg != nil {
		startupTimeout = cfg.Session.StartupTimeoutDuration()
	}
	maxAgeTr := reconcileOpts.maxSessionAgeTr
	asyncStopTracker := reconcileOpts.asyncStopTracker
	recordPhase := func(site TraceSiteCode, name string, start time.Time, fields map[string]any) {
		if trace != nil {
			trace.RecordControllerOperation(site, TraceReasonRetained, TraceOutcomeComplete, name, time.Since(start), fields)
		}
	}
	phaseStart := time.Now()
	deps := buildDepsMap(cfg)
	if cityName == "" {
		cityName = config.EffectiveCityName(cfg, "")
	}
	recordPhase(TraceSiteSessionReconcileBuildDeps, "session_reconcile.build_deps", phaseStart, map[string]any{
		"dependency_template_count": len(deps),
	})

	// Phase 0: Heal expired timers on all sessions.
	phaseStart = time.Now()
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
	recordPhase(TraceSiteSessionReconcileHealRetire, "session_reconcile.heal_and_retire_duplicates", phaseStart, map[string]any{
		"session_count": len(sessions),
	})

	// Topo-order sessions by template dependencies.
	phaseStart = time.Now()
	ordered := topoOrder(sessions, deps)
	recordPhase(TraceSiteSessionReconcileTopoOrder, "session_reconcile.topo_order", phaseStart, map[string]any{
		"ordered_session_count": len(ordered),
	})

	phaseStart = time.Now()
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
	recordPhase(TraceSiteSessionReconcileCircuitBreaker, "session_reconcile.circuit_breaker_restore", phaseStart, map[string]any{
		"enabled":       cbEnabled,
		"session_count": len(ordered),
	})

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
	attemptRollbackPendingCreate := func(session *beads.Bead, templateName, name, action, detail string, clearClaim bool) {
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
		if clearClaim {
			rollbackPendingCreateClearingClaim(session, store, clk.Now().UTC(), stderr)
			return
		}
		rollbackPendingCreate(session, store, clk.Now().UTC(), stderr)
	}
	phaseStart = time.Now()
	for i := range ordered {
		if ctx != nil && ctx.Err() != nil {
			return 0
		}
		session := &ordered[i]
		name := strings.TrimSpace(session.Metadata["session_name"])
		tp, desired := desiredState[name]
		if _, _, pending := resetPendingCommittedAt(*session); !pending && dt != nil {
			dt.clearResetStall(session.ID)
		}

		if reconcileDrainAckStopPending(cityPath, cfg, sp, store, rigStores, session, tp, desired, dops, dt, asyncStopTracker, clk, rec, stderr) {
			continue
		}

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

		// Orphan/suspended: bead exists but not in desired state.
		// Handle BEFORE heal/stability to avoid false crash detection —
		// a running session that leaves the desired set is not a crash.
		if !desired {
			providerAlive, err := workerSessionTargetRunningWithConfig(cityPath, store, sp, cfg, session.ID)
			if err != nil {
				providerAlive = false
			}
			// Run this before configured named-session preservation. A stale
			// state=creating bead with an expired pending-create lease would
			// otherwise stay open and keep holding its alias forever.
			if !storeQueryPartial && !providerAlive && shouldRollbackPendingCreate(session) {
				var startupTimeout time.Duration
				if cfg != nil {
					startupTimeout = cfg.Session.StartupTimeoutDuration()
				}
				if pendingCreateLeaseExpiredForRollback(*session, clk, startupTimeout) {
					template := normalizedSessionTemplate(*session, cfg)
					if template == "" {
						template = session.Metadata["template"]
					}
					peek := cachedSessionPeek(cityPath, store, sp, cfg, session.ID, nil)
					rateLimitHit, rateLimitErr := checkRateLimitStability(session, cfg, providerAlive, dt, store, clk, peek)
					if rateLimitHit || rateLimitErr != nil {
						continue
					}
					clearClaim := configuredNamedSessionBeadHasSpec(*session, cfg, cityName)
					attemptRollbackPendingCreate(session, template, name, "pending_create_lease_expired", "lease expired and no live runtime", clearClaim)
					continue
				}
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
			if isFailedCreateSessionBead(*session) {
				template := normalizedSessionTemplate(*session, cfg)
				if template == "" {
					template = session.Metadata["template"]
				}
				if pendingCreateSessionStillLeased(*session, cfg, clk) {
					if trace != nil {
						trace.recordDecision("reconciler.session.pending_create_preserved", template, name, "pending_create", "kept_open", traceRecordPayload{
							"pending_create_claim": strings.TrimSpace(session.Metadata["pending_create_claim"]),
							"provider_alive":       providerAlive,
							"state":                session.Metadata["state"],
						}, nil, "")
					}
					continue
				}
				if !providerAlive {
					if trace != nil {
						trace.recordDecision("reconciler.session.close_failed_create", template, name, string(sessionpkg.StateFailedCreate), "closed", nil, nil, "")
					}
					if storeQueryPartial {
						continue
					}
					if closeSessionBeadIfReachableStoreUnassigned(cityPath, cfg, store, rigStores, *session, string(sessionpkg.StateFailedCreate), clk.Now().UTC(), stderr) {
						session.Status = "closed"
					}
					continue
				}
			}
			// Heal state using provider liveness, not agent membership.
			// rollbackAvailable mirrors the rollback gate at line ~639: when
			// storeQueryPartial=true the formal rollback is deferred, so the
			// heal path must also preserve pending_create_claim to avoid a
			// half-applied rollback that races the next complete tick.
			stateBeforeHeal := strings.TrimSpace(session.Metadata["state"])
			pendingCreateStartedAtBeforeHeal := strings.TrimSpace(session.Metadata["pending_create_started_at"])
			lastWokeAtBeforeHeal := strings.TrimSpace(session.Metadata["last_woke_at"])
			healBatch := healStateWithRollback(session, providerAlive, store, clk, startupTimeout, !storeQueryPartial)
			traceHealClearedPendingCreateLease(
				trace,
				*session,
				cfg,
				"",
				name,
				stateBeforeHeal,
				pendingCreateStartedAtBeforeHeal,
				lastWokeAtBeforeHeal,
				providerAlive,
				healBatch,
			)
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
						ackReason := assignedWorkDrainCancelReason(*session, sp, dt, name)
						hasAssignedWork, assignedErr := sessionHasAwakeAssignedWorkForReachableStore(cityPath, cfg, store, rigStores, *session)
						if assignedErr != nil {
							fmt.Fprintf(stderr, "session reconciler: checking assigned work for drain-acked %s: %v\n", name, assignedErr) //nolint:errcheck
							hasAssignedWork = true
						}
						if providerAlive && hasAssignedWork {
							if cancelSessionDrainForAssignedWork(*session, sp, dt) ||
								cancelRecoveredDrainForAssignedWork(*session, sp, name) {
								_ = dops.clearDrain(name)
								template := normalizedSessionTemplate(*session, cfg)
								if template == "" {
									template = session.Metadata["template"]
								}
								fmt.Fprintf(stdout, "Canceled drain-acked session '%s' (assigned work)\n", name) //nolint:errcheck
								if trace != nil {
									trace.recordDecision("reconciler.drain.cancel", template, name, ackReason, "cancel_assigned_work", nil, nil, "")
								}
								continue
							}
						}
						if providerAlive {
							template := normalizedSessionTemplate(*session, cfg)
							if template == "" {
								template = session.Metadata["template"]
							}
							if markDrainAckStopPending(session, store, clk, stderr) {
								clearDrainTrackerForStopPending(session, dt)
								queueDrainAckAsyncStop(cityPath, store, sp, cfg, session.ID, name, asyncStopTracker, stderr)
								if trace != nil {
									trace.recordDecision("reconciler.session.drain_ack", template, name, "orphaned", "stop_pending", nil, nil, "")
								}
							}
							continue
						}
						template := normalizedSessionTemplate(*session, cfg)
						if template == "" {
							template = session.Metadata["template"]
						}
						finalizeDrainAckStoppedSession(
							cityPath, cfg, store, rigStores, session, template,
							true, dops, dt, clk, rec, stderr,
						)
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
					hasAssignedWork, assignedErr := sessionHasOpenAssignedWorkForConfig(store, rigStores, *session, cfg)
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
		// The desired-session fast path only needs running/alive; attachment
		// and activity are probed by the narrower branches that use them.
		running, alive := observeRuntimeProviderLiveness(sp, name, tp.Hints.ProcessNames)
		peek := cachedSessionPeek(cityPath, store, sp, cfg, session.ID, tp.Hints.ProcessNames)
		recordResetStallIfDue(*session, tp.TemplateName, name, alive, startupTimeout, clk.Now().UTC(), dt, rec, stderr, trace)

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
			attemptRollbackPendingCreate(session, tp.TemplateName, name, "pending_create_rollback", "live runtime belongs to another session", false)
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
				attemptRollbackPendingCreate(session, tp.TemplateName, name, "pending_create_lease_expired", "lease expired and no live runtime", false)
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
					_ = clearReconcilerDrainAckMetadata(sp, name)
				} else {
					if staleReconcilerDrainAck(*session, sp, name) {
						_ = clearReconcilerDrainAckMetadata(sp, name)
						if trace != nil {
							trace.recordDecision("reconciler.session.drain_ack", tp.TemplateName, name, "stale_generation", "clear", nil, nil, "")
						}
						continue
					}
					ackReason, reconcilerOwnedAck := reconcilerDrainAckMatchesSession(*session, sp, name)
					if reconcilerOwnedAck && assignedWorkDrainReasonCancelable(ackReason) {
						hasAssignedWork, assignedErr := sessionHasAwakeAssignedWorkForReachableStore(cityPath, cfg, store, rigStores, *session)
						if assignedErr != nil {
							fmt.Fprintf(stderr, "session reconciler: checking assigned work for drain-acked %s: %v\n", name, assignedErr) //nolint:errcheck
							hasAssignedWork = true
						}
						if alive && hasAssignedWork &&
							(cancelSessionDrainForAssignedWork(*session, sp, dt) || cancelRecoveredDrainForAssignedWork(*session, sp, name)) {
							_ = dops.clearDrain(name)
							if trace != nil {
								trace.recordDecision("reconciler.drain.cancel", tp.TemplateName, name, ackReason, "cancel_assigned_work", nil, nil, "")
							}
							continue
						}
					}
					configDriftAck := reconcilerOwnedAck && ackReason == "config-drift"
					if !configDriftAck && dt != nil {
						if ds := dt.get(session.ID); ds != nil && ds.ackSet && ds.reason == "config-drift" {
							configDriftAck = true
						}
					}
					if configDriftAck {
						driftKey := sessionConfigDriftKey(*session, cfg, tp)
						attached, attachErr := sessionAttachedForConfigDrift(*session, sp, cityPath, store, cfg, name)
						if attachErr != nil {
							fmt.Fprintf(stderr, "session reconciler: observing config-drift attachment for %s: %v\n", name, attachErr) //nolint:errcheck
							drainCancelled := cancelSessionConfigDriftDrain(*session, sp, dt)
							if !drainCancelled {
								_ = clearReconcilerDrainAckMetadata(sp, name)
							}
							if trace != nil {
								trace.recordDecision("reconciler.session.drain_ack", tp.TemplateName, name, "config_drift_attachment_error", "cancel_reconciler_ack", traceRecordPayload{
									"drain_canceled": drainCancelled,
									"error":          attachErr.Error(),
								}, nil, "")
							}
							continue
						}
						if attached {
							if driftKey != "" {
								if err := recordSessionAttachedConfigDriftDeferral(*session, store, clk, driftKey); err != nil {
									fmt.Fprintf(stderr, "session reconciler: recording attached config-drift deferral for %s: %v\n", name, err) //nolint:errcheck
								}
							}
							drainCancelled := cancelSessionConfigDriftDrain(*session, sp, dt)
							if !drainCancelled {
								_ = clearReconcilerDrainAckMetadata(sp, name)
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
								_ = clearReconcilerDrainAckMetadata(sp, name)
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
					if alive {
						if markDrainAckStopPending(session, store, clk, stderr) {
							clearDrainTrackerForStopPending(session, dt)
							queueDrainAckAsyncStop(cityPath, store, sp, cfg, session.ID, name, asyncStopTracker, stderr)
							if trace != nil {
								trace.recordDecision("reconciler.session.drain_ack", tp.TemplateName, name, "acknowledged", "stop_pending", nil, nil, "")
							}
						}
						continue
					}
					finalizeDT := dt
					if reconcilerOwnedAck {
						finalizeDT = nil
					}
					finalizeDrainAckStoppedSession(
						cityPath, cfg, store, rigStores, session, tp.TemplateName,
						isPoolManagedSessionBead(*session),
						dops, finalizeDT,
						clk, rec, stderr,
					)
					continue
				}
			}
		}

		// Progress-aware recycle (ADR-0013 Amendment A1, move 3b): a desired,
		// alive session that has stopped progressing has likely parked (e.g. its
		// turn ended on a provider auth error) and will not self-recover. Opt-in
		// via [session] progress_stall_timeout; disabled (zero) by default, so
		// this is a no-op unless a city sets a threshold above its agents'
		// longest legitimate alive-idle period. The cheap time check gates the
		// store/health queries so they run only for the rare already-stalled
		// session. Set the restart_requested marker and let the block below
		// perform the fresh-restart handoff.
		if threshold := cfg.Session.ProgressStallTimeoutDuration(); threshold > 0 && alive && sessionActivityReportable(sp, name) {
			lastActivity, lastActivityErr := sp.GetLastActivity(name)
			if lastActivityErr != nil {
				fmt.Fprintf(stderr, "session reconciler: reading last activity before progress-stall recycle for %s: %v\n", name, lastActivityErr) //nolint:errcheck
			}
			if lastActivityErr == nil && !lastActivity.IsZero() && clk.Now().Sub(lastActivity) > threshold {
				exempt := pendingInteractionKeepsAwake(*session, sp, name, clk) ||
					pendingCreateStartInFlight(*session, clk, startupTimeout)
				if !exempt {
					attached, attachErr := sessionAttachedForConfigDrift(*session, sp, cityPath, store, cfg, name)
					if attachErr != nil {
						// Fail safe: an unreadable attachment check must not recycle a
						// session a human may be attached to. Mirrors the claim-check
						// guard below — skip the destructive action on error rather
						// than assume the session is unattended.
						fmt.Fprintf(stderr, "session reconciler: checking attachment before progress-stall recycle for %s: %v\n", name, attachErr) //nolint:errcheck
						exempt = true
					} else if attached {
						exempt = true
					}
				}
				// Min-floor idle workers are legitimately unclaimed: they hold no
				// bead because they are waiting for routed work to arrive, not
				// because they parked on an error. Exempt them before the
				// I/O-bound claim and provider-health checks so those queries
				// are skipped entirely for floor workers every reconcile tick.
				if !exempt && cfg != nil {
					if cfgAgent := findAgentByTemplate(cfg, tp.TemplateName); cfgAgent != nil {
						minFloor := cfgAgent.EffectiveMinActiveSessions()
						if minFloor > 0 {
							openInPool := 0
							for j := range ordered {
								if ordered[j].Status != "closed" && normalizedSessionTemplate(ordered[j], cfg) == tp.TemplateName {
									openInPool++
								}
							}
							if isMinFloorIdleWorker(minFloor, openInPool) {
								exempt = true
								if trace != nil {
									trace.recordDecision(string(TraceSiteReconcilerProgressStallExempt), tp.TemplateName, name, "min_floor_idle_worker", "exempt", traceRecordPayload{
										"pool_min":  minFloor,
										"pool_open": openInPool,
									}, nil, "")
								}
							}
						}
					}
				}
				holdsClaim := false
				if !exempt {
					has, err := sessionHasInProgressAssignedWorkForConfig(store, rigStores, *session, cfg)
					if err != nil {
						// Fail safe: an unreadable claim check must not recycle a
						// session that may hold in-progress work. Mirrors the drain
						// guards elsewhere (they skip the destructive action on a
						// claim-check error rather than assume the session is idle).
						fmt.Fprintf(stderr, "session reconciler: checking assigned work before progress-stall recycle for %s: %v\n", name, err) //nolint:errcheck
						holdsClaim = true
					} else {
						holdsClaim = has
					}
				}
				providerHealthy := true
				if !exempt && !holdsClaim && tp.ResolvedProvider != nil {
					// Reuse the per-tick provider-health snapshot (#2962). Gate 1
					// (provider RED) takes precedence: never recycle a session whose
					// provider is red. Fail-open — absent/stale registry → healthy.
					if h, present := phSnap.check(tp.ResolvedProvider.Name); present {
						providerHealthy = h
					}
				}
				if sessionProgressStalled(threshold, holdsClaim, providerHealthy, exempt, lastActivity, clk.Now()) {
					if session.Metadata == nil {
						session.Metadata = map[string]string{}
					}
					session.Metadata["restart_requested"] = "true"
					fmt.Fprintf(stderr, "session reconciler: %s progress-stalled (no progress for >%s, no open claim, provider healthy); requesting fresh restart\n", name, threshold) //nolint:errcheck
				}
			}
		}

		// Restart-requested: agent asked for a fresh session
		// (gc runtime request-restart / gc handoff). This runs after
		// drain-ack handling, but before autonomous rate-limit,
		// stability, and churn gates so an explicit operator/model reset
		// is not swallowed by crash heuristics. Use provider-session
		// liveness (running), not process liveness (alive), so a zombie
		// tmux/container session is still stopped before the next wake.
		{
			runtimeRunning := running || alive
			tmuxRequested := false
			if runtimeRunning && dops != nil {
				tmuxRequested, _ = dops.isRestartRequested(name)
			}
			beadRequested := session.Metadata["restart_requested"] == "true"
			if tmuxRequested || beadRequested {
				if runtimeRunning {
					if err := workerKillSessionTargetWithConfig("", store, sp, cfg, name); err != nil {
						fmt.Fprintf(stderr, "session reconciler: stopping restart-requested %s: %v\n", name, err) //nolint:errcheck
						continue
					}
				}
				if identity := namedSessionIdentity(*session); identity != "" {
					if err := resetSessionCircuitBreakerState(store, session.ID, identity, cb); err != nil {
						fmt.Fprintf(stderr, "session reconciler: clearing session circuit breaker for restart-requested %s: %v\n", name, err) //nolint:errcheck
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
				batch := sessionpkg.RestartRequestPatch(newSessionKey, clk.Now())
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
					// The durable reset commit marker is for the next
					// reconciler pass; keeping it out of this tick's
					// in-memory bead prevents on-demand sessions from
					// being force-woken without demand.
					if key == sessionpkg.ResetCommittedAtKey {
						continue
					}
					session.Metadata[key] = value
				}
				if runtimeRunning {
					if tmuxRequested && dops != nil {
						if err := dops.clearRestartRequested(name); err != nil {
							if !runtime.IsSessionGone(err) {
								fmt.Fprintf(stderr, "session reconciler: clearing restart-requested marker for %s (bead %s): %v\n", name, session.ID, err) //nolint:errcheck
							}
						}
					}
					fmt.Fprintf(stdout, "Stopped restart-requested session '%s'\n", name) //nolint:errcheck
					// Yield this tick so the kill and the next wake run
					// on separate reconciler passes; the new start should
					// not race the tmux alias release.
					continue
				}
				// Runtime was already dead — no kill happened, no alias
				// release to wait on. Fall through so the wake decision
				// can pick up the freshly cleared metadata and emit a
				// start_candidate on this same tick. See #2345.
			}
		}

		policy := resolveSessionSleepPolicy(*session, cfg, sp)

		rateLimitHit, rateLimitErr := checkRateLimitStability(session, cfg, alive, dt, store, clk, peek)
		if rateLimitHit || rateLimitErr != nil {
			continue // rate-limit hold recorded before state healing resets continuity metadata
		}

		// Heal advisory state metadata.
		stateBeforeHeal := sessionpkg.State(strings.TrimSpace(session.Metadata["state"]))
		pendingCreateStartedAtBeforeHeal := strings.TrimSpace(session.Metadata["pending_create_started_at"])
		lastWokeAtBeforeHeal := strings.TrimSpace(session.Metadata["last_woke_at"])
		healBatch := healStateWithRollback(session, alive, store, clk, startupTimeout, true)
		traceHealClearedPendingCreateLease(
			trace,
			*session,
			cfg,
			tp.TemplateName,
			name,
			string(stateBeforeHeal),
			pendingCreateStartedAtBeforeHeal,
			lastWokeAtBeforeHeal,
			alive,
			healBatch,
		)
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
			switch stateBeforeHeal {
			case sessionpkg.StateStartPending, sessionpkg.StateCreating:
				if pendingCreateStartInFlight(*session, clk, startupTimeout) {
					if trace != nil {
						trace.recordDecision("reconciler.session.pending_create", tp.TemplateName, name, "pending_create_recovery_in_flight", "deferred", nil, nil, "")
					}
					continue
				}
			}
			if !recoverRunningPendingCreate(session, tp, cfg, store, clk, trace) {
				fmt.Fprintf(stderr, "session reconciler: recovering pending create %s: metadata repair incomplete\n", name) //nolint:errcheck
			}
		}

		// driftRestartedInPlace tracks whether the alive-restart branch ran
		// the named-session in-place restart on this tick. Hoisted out of
		// the inner block so the downstream asleep-named-session drift
		// repair block can skip when we just restarted, preventing the
		// preserved resume metadata from being undone before the new
		// process commits.
		driftRestartedInPlace := false
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
					agentCfg := sessionCoreConfigForHash(tp, *session)
					currentHash := runtime.CoreFingerprint(agentCfg)
					if storedHash != currentHash {
						// Stored hash has no version prefix or carries a
						// different version than the current binary — silently
						// rebaseline all four fingerprint fields rather than
						// draining the session. The mismatch is a versioning
						// artifact, not real config drift. See ga-s760 FRs 1-3.
						if runtime.IsLegacyOrMismatchedVersion(storedHash) {
							outcome := rebaselineLegacyHashOutcome(storedHash)
							if err := silentRebaselineSessionHashes(session, store, agentCfg); err != nil {
								fmt.Fprintf(stderr, "session reconciler: rebaselining legacy hash for %s: %v\n", name, err) //nolint:errcheck
							} else {
								fmt.Fprintf(stderr, "rebaselined legacy hash for %s (stored=%s current=%s)\n", name, truncateHashForLog(storedHash), truncateHashForLog(currentHash)) //nolint:errcheck
							}
							if trace != nil {
								trace.recordDecision("reconciler.session.config_drift", tp.TemplateName, name, "config_drift", string(outcome), traceRecordPayload{
									"stored_hash":  storedHash,
									"current_hash": currentHash,
								}, nil, "")
							}
							continue
						}
						fmt.Fprintf(stderr, "config-drift %s: stored=%s current=%s cmd=%q\n", name, truncateHashForLog(storedHash), truncateHashForLog(currentHash), agentCfg.Command) //nolint:errcheck
						// Diagnostic: log per-field breakdown to identify the drifting field.
						driftedFields := runtime.CoreFingerprintDriftFieldsFromJSON(session.Metadata["core_hash_breakdown"], agentCfg)
						runtime.LogCoreFingerprintDrift(stderr, name, session.Metadata["core_hash_breakdown"], agentCfg)
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
							continue
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
							resetConfiguredNamedSessionForConfigDrift(session, store, sp, name, alive, string(sessionpkg.StateStartPending), clk.Now().UTC(), stderr)
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
							driftRestartedInPlace = true
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
							// Pool-routed sessions reach this branch when their
							// config_hash drifts but they're not configured as
							// named sessions (so restart-in-place at line 1173
							// did not fire). If such a session is actively
							// processing assigned work, draining mid-task would
							// orphan the work bead (assignee still pointing at
							// the dead session, status stuck at in_progress) and
							// kill the agent before it can complete. Defer drain
							// until the work completes; the next tick will see no
							// assigned work and drain naturally. The same shape
							// of protection is already applied to the
							// orphan/suspended drain at line 754.
							hasAssignedWork, assignedErr := sessionHasOpenAssignedWorkForReachableStore(cityPath, cfg, store, rigStores, *session)
							if assignedErr != nil {
								fmt.Fprintf(stderr, "session reconciler: checking assigned work before config-drift drain for %s: %v\n", name, assignedErr) //nolint:errcheck
								continue
							}
							if hasAssignedWork {
								if trace != nil {
									trace.recordDecision("reconciler.session.config_drift", tp.TemplateName, name, "config_drift", string(TraceOutcomeDeferredActive), configDriftTracePayload(storedHash, currentHash, driftedFields, traceRecordPayload{
										"active_reason": "live_assigned_work",
									}), nil, "")
								}
								fmt.Fprintf(stdout, "Skipping config-drift drain for '%s': live assigned work found\n", name) //nolint:errcheck
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
						switch {
						case storedLive == "" && len(agentCfg.SessionLive) == 0:
							// No stored hash and no live config — silently
							// backfill the hash without running anything.
							_ = store.SetMetadataBatch(session.ID, map[string]string{
								"live_hash":         currentLive,
								"started_live_hash": currentLive,
							})
						case runtime.IsLegacyOrMismatchedVersion(storedLive):
							// Stored live hash from a pre-versioning or
							// version-mismatched binary — silently rebaseline
							// all four fingerprint fields rather than running
							// SessionLive again. ga-s760 FRs 1-3.
							outcome := rebaselineLegacyHashOutcome(storedLive)
							if err := silentRebaselineSessionHashes(session, store, agentCfg); err != nil {
								fmt.Fprintf(stderr, "session reconciler: rebaselining legacy live hash for %s: %v\n", name, err) //nolint:errcheck
							} else {
								fmt.Fprintf(stderr, "rebaselined legacy live hash for %s (stored=%s current=%s)\n", name, truncateHashForLog(storedLive), truncateHashForLog(currentLive)) //nolint:errcheck
							}
							if trace != nil {
								trace.recordDecision("reconciler.session.live_drift", tp.TemplateName, name, "live_drift", string(outcome), traceRecordPayload{
									"stored_hash":  storedLive,
									"current_hash": currentLive,
								}, nil, "")
							}
						default:
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

		// Asleep-named-session drift repair. Skipped while an in-place
		// restart is still leased in creating: the preserved
		// started_config_hash intentionally points at the previous runtime
		// hash until the new process commits. Without the durable guard,
		// a deferred start's next reconcile tick would clear the preserved
		// hash and rotate session_key before --resume can be prepared.
		skipAsleepDriftRepair := driftRestartedInPlace ||
			pendingResumePreservingNamedRestart(*session, clk, startupTimeout)
		if !alive && isNamedSessionBead(*session) && !skipAsleepDriftRepair {
			template := tp.TemplateName
			if template == "" {
				template = normalizedSessionTemplate(*session, cfg)
			}
			storedHash := session.Metadata["started_config_hash"]
			if template != "" && storedHash != "" {
				if cfgAgent := findAgentByTemplate(cfg, template); cfgAgent != nil {
					agentCfg := sessionCoreConfigForHash(tp, *session)
					currentHash := runtime.CoreFingerprint(agentCfg)
					if storedHash != currentHash {
						// Stored hash carries no version prefix or a different
						// version — silently rebaseline rather than treating
						// the asleep named session as drifted. ga-s760 FRs 1-3.
						if runtime.IsLegacyOrMismatchedVersion(storedHash) {
							outcome := rebaselineLegacyHashOutcome(storedHash)
							if err := silentRebaselineSessionHashes(session, store, agentCfg); err != nil {
								fmt.Fprintf(stderr, "session reconciler: rebaselining legacy hash for %s: %v\n", name, err) //nolint:errcheck
							} else {
								fmt.Fprintf(stderr, "rebaselined legacy hash for %s (stored=%s current=%s)\n", name, truncateHashForLog(storedHash), truncateHashForLog(currentHash)) //nolint:errcheck
							}
							if trace != nil {
								trace.recordDecision("reconciler.session.config_drift", tp.TemplateName, name, "config_drift", string(outcome), traceRecordPayload{
									"stored_hash":  storedHash,
									"current_hash": currentHash,
								}, nil, "")
							}
							continue
						}
						driftedFields := runtime.CoreFingerprintDriftFieldsFromJSON(session.Metadata["core_hash_breakdown"], agentCfg)
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
		// sessionpkg.DecideMaxSessionAge owns the decision ladder (blocker,
		// then pending interaction, then assigned work, then stop); this
		// block gathers the facts it asks for and executes the outcome.
		if maxAgeTr != nil && alive {
			creationCompleteAt, hasAnchor := parseRFC3339Metadata(session.Metadata["creation_complete_at"])
			facts := sessionpkg.TimerFacts{
				Triggered: hasAnchor && maxAgeTr.shouldRestart(name, tp.TemplateName, creationCompleteAt, clk.Now()),
			}
			if facts.Triggered {
				facts.Blocker = lifecycleTimerBlocker(session.Metadata, clk.Now())
			}
			dec := sessionpkg.DecideMaxSessionAge(facts)
			for dec.Action == sessionpkg.TimerActionGatherPending || dec.Action == sessionpkg.TimerActionGatherAssignedWork {
				if dec.Action == sessionpkg.TimerActionGatherPending {
					facts.Pending = sessionpkg.PendingNo
					if pendingInteractionKeepsAwake(*session, sp, name, clk) {
						facts.Pending = sessionpkg.PendingYes
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
					facts.AssignedWork = sessionpkg.AssignedWorkNone
					if hasWork {
						facts.AssignedWork = sessionpkg.AssignedWorkHas
					}
				}
				dec = sessionpkg.DecideMaxSessionAge(facts)
			}
			switch dec.Action {
			case sessionpkg.TimerActionDefer:
				// Deferrals include lifecycle timer blockers already enforced
				// by wake evaluation: bypass the max-age restart so SleepPatch
				// does not rewrite the intended sleep state.
				if trace != nil {
					trace.recordDecision("reconciler.session.max_session_age", tp.TemplateName, name, dec.TraceReason, dec.TraceOutcome, nil, nil, "")
				}
			case sessionpkg.TimerActionStop:
				fmt.Fprintf(stderr, "session reconciler: preemptive max-age restart for %s (age=%s)\n", tp.DisplayName(), clk.Now().Sub(creationCompleteAt).Round(time.Second)) //nolint:errcheck // best-effort stderr
				if trace != nil {
					trace.recordDecision("reconciler.session.max_session_age", tp.TemplateName, name, dec.TraceReason, dec.TraceOutcome, nil, nil, "")
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
					batch := sessionpkg.SleepPatch(clk.Now(), dec.SleepReason)
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

		// Idle timeout: restart sessions idle longer than configured threshold.
		// Pass the agent template so the tracker can fall back to a per-template
		// timeout for pool sessions whose bead-derived runtime names are not
		// registered directly. sessionpkg.DecideIdleTimeout owns the decision
		// ladder; this block gathers the facts it asks for and executes the
		// outcome.
		if it != nil && alive {
			facts := sessionpkg.TimerFacts{
				Triggered: it.checkIdle(name, tp.TemplateName, sp, clk.Now()),
			}
			if facts.Triggered {
				facts.Blocker = lifecycleTimerBlocker(session.Metadata, clk.Now())
			}
			dec := sessionpkg.DecideIdleTimeout(facts)
			for dec.Action == sessionpkg.TimerActionGatherPending {
				facts.Pending = sessionpkg.PendingNo
				if pendingInteractionKeepsAwake(*session, sp, name, clk) {
					facts.Pending = sessionpkg.PendingYes
				}
				dec = sessionpkg.DecideIdleTimeout(facts)
			}
			switch dec.Action {
			case sessionpkg.TimerActionDefer:
				// Blocker deferrals respect lifecycle timer blockers without
				// skipping the post-loop wake/drain pass. A metadata-only
				// suspend uses sleep_intent=user-hold and still needs that
				// pass to drain the live runtime. Pending-interaction
				// deferrals cancel any pending drain and skip this tick's
				// wake pass for the session.
				var payload traceRecordPayload
				if dec.CancelDrain {
					drainCancelled := false
					if dt != nil {
						drainCancelled = cancelSessionDrain(*session, sp, dt)
					}
					payload = traceRecordPayload{"drain_canceled": drainCancelled}
				}
				if trace != nil {
					trace.recordDecision("reconciler.session.idle_timeout", tp.TemplateName, name, dec.TraceReason, dec.TraceOutcome, payload, nil, "")
				}
				if dec.SkipWakePass {
					continue
				}
			case sessionpkg.TimerActionStop:
				fmt.Fprintf(stderr, "session reconciler: idle timeout for %s\n", tp.DisplayName()) //nolint:errcheck // best-effort stderr
				if trace != nil {
					trace.recordDecision("reconciler.session.idle_timeout", tp.TemplateName, name, dec.TraceReason, dec.TraceOutcome, nil, nil, "")
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
					batch := sessionpkg.SleepPatch(clk.Now(), dec.SleepReason)
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
			// Fall through to wakeReasons — it will re-wake immediately if config present
		}

		wakeTargets = append(wakeTargets, wakeTarget{session: session, tp: tp, alive: alive})
	}
	recordPhase(TraceSiteSessionReconcileForwardPass, "session_reconcile.forward_pass", phaseStart, map[string]any{
		"ordered_session_count":  len(ordered),
		"wake_target_count":      len(wakeTargets),
		"rollback_count":         rollbacksThisTick,
		"rollback_budget":        maxRollbacksPerTick,
		"start_candidate_count":  len(startCandidates),
		"assigned_work_bead_cnt": len(assignedWorkBeads),
	})

	if ctx != nil && ctx.Err() != nil {
		return 0
	}

	// Use ComputeAwakeSet for the wake/sleep decision.
	phaseStart = time.Now()
	awakeInput := buildAwakeInputFromReconciler(
		cfg, cityPath, ordered, poolDesired, namedSessionDemand, workSet, readyWaitSet,
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
			// Direct assigned work overrides sleep suppression for every
			// sleep class — the assignment is session-specific, so a pool
			// sibling cannot serve it. Pool-scale demand (poolDesired > 0)
			// overrides suppression only for non-interactive sessions
			// (matching the old evaluateWakeReasons behavior). Min-active
			// city-stop revival is also config demand: stale detach metadata
			// from before gc stop must not cancel the post-start guarantee.
			// Interactive sessions honor their idle window against
			// pool-scale demand — an idle chat session should still sleep
			// to release resources.
			// Explicit sleep_intent always wins — if the session has
			// signaled it wants to sleep, honor that regardless of demand.
			template := normalizedSessionTemplate(*target.session, cfg)
			hasExplicitSleepIntent := target.session.Metadata["sleep_intent"] != ""
			demandOverrides := wakeDemandOverridesSleepSuppression(decision, eval, policy, poolDesired, template, hasExplicitSleepIntent)
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
	recordPhase(TraceSiteSessionReconcileAwakeSet, "session_reconcile.compute_awake_set_and_idle_probes", phaseStart, map[string]any{
		"wake_target_count":      len(wakeTargets),
		"idle_probe_target_cnt":  len(idleProbeTargets),
		"awake_decision_count":   len(awakeDecisions),
		"awake_eval_count":       len(wakeEvals),
		"assigned_work_bead_cnt": len(assignedWorkBeads),
	})

	phaseStart = time.Now()
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
			// Provider-health gate (ADR-0013 A1 M3a): skip respawn when the
			// provider is red. Does NOT consume the wake budget (no append to
			// startCandidates). Episode tracking fires exactly one alert per
			// red episode via emitProviderHealthGateAlert.
			if gate != nil && target.tp.ResolvedProvider != nil {
				phProvider := target.tp.ResolvedProvider.Name
				phHealthy, phPresent := phSnap.check(phProvider)
				if !phPresent {
					// Registry absent or no fresh entry — fail-open, log once per provider per tick.
					fmt.Fprintf(stderr, "session reconciler: provider-health registry unavailable for %q; treating as green\n", phProvider) //nolint:errcheck
				} else if !phHealthy {
					gate.recordRedSkip(phProvider, clk.Now().UTC(), func(p, epID string, since time.Time, count int) {
						emitProviderHealthGateAlert(rec, stdout, p, epID, since, count)
					})
					if trace != nil {
						trace.recordDecision("reconciler.session.provider_health_gate", target.tp.TemplateName, name, "provider_red", "respawn_skipped", traceRecordPayload{
							"provider": phProvider,
						}, nil, "")
					}
					continue // skip startCandidates; wake budget is NOT consumed
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
		if poolFreeable && hasAssignedWork {
			// The runtime is gone but the session bead still owns
			// in_progress work — almost always a CLI process that
			// exited or hung without going through the clean drain
			// path. The close gate below correctly preserves the
			// pool slot, but without an event there is no signal
			// for operators or downstream recovery agents that this
			// happened. Emit a single diagnostic per session bead
			// generation; the throttle marker on the bead itself
			// keeps subsequent reconciler ticks quiet.
			emitSessionStrandedDiagnostic(cityPath, cfg, store, rigStores, target.session, target.tp.TemplateName, rec, clk, stderr)
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
			if closeBead(store, target.session.ID, closeReason, clk.Now().UTC(), stderr) {
				// Pool worktrees are transient by design — reclaim disk
				// when the session bead is retired. Skipped under safety
				// gates (uncommitted, unpushed, stashed) and overridable
				// via cfg.Daemon.AutoPruneWorkerDir.
				pruneAgentHomeWorktreeIfSafe(*target.session, cityPath, cfg, stderr)
			}
		}
	}
	recordPhase(TraceSiteSessionReconcileWakeSleep, "session_reconcile.apply_wake_sleep_decisions", phaseStart, map[string]any{
		"wake_target_count":     len(wakeTargets),
		"start_candidate_count": len(startCandidates),
	})

	// Flush green ticks so episode state clears even when all sessions for a
	// provider are already alive (and never enter the shouldWake && !alive path).
	if gate != nil {
		for _, p := range phSnap.healthyProviders() {
			gate.recordGreenTick(p)
		}
	}

	if ctx != nil && ctx.Err() != nil {
		return 0
	}

	phaseStart = time.Now()
	plannedWakes := executePlannedStartsTraced(
		ctx, startCandidates, cfg, desiredState, sp, store, cityName,
		cityPath,
		clk, rec, startupTimeout, stdout, stderr, trace,
		effectiveStartOptions...,
	)
	recordPhase(TraceSiteSessionReconcileStartExecution, "session_reconcile.execute_planned_starts", phaseStart, map[string]any{
		"start_candidate_count": len(startCandidates),
		"planned_wake_count":    plannedWakes,
	})

	if ctx != nil && ctx.Err() != nil {
		return plannedWakes
	}

	// Phase 2: Advance all in-flight drains.
	phaseStart = time.Now()
	sessionLookup := func(id string) *beads.Bead {
		return beadByID[id]
	}
	advanceSessionDrainsWithSessionsTraced(dt, sp, store, sessionLookup, ordered, wakeEvals, cfg, poolDesired, nil, readyWaitSet, clk, trace)
	clearMissingIdleProbes(dt, beadByID)
	recordPhase(TraceSiteSessionReconcileDrainAdvance, "session_reconcile.advance_drains", phaseStart, map[string]any{
		"ordered_session_count": len(ordered),
		"wake_eval_count":       len(wakeEvals),
	})

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

// sessionHasOpenAssignedWorkForConfig uses the same configured-named-session
// fallback identity strategy as sessionAssigneeMatches, but queries all known
// stores instead of a single configured reachable store. Use this cross-store
// query for cleanup-of-record paths that must not orphan work in any attached
// store; callers preserve fail-closed behavior by refusing close decisions on
// query errors.
func sessionHasOpenAssignedWorkForConfig(store beads.Store, rigStores map[string]beads.Store, session beads.Bead, cfg *config.City) (bool, error) {
	return sessionHasOpenAssignedWorkInStores(store, rigStores, sessionAssignmentIdentifiersForConfig(session, cfg))
}

// sessionHasInProgressAssignedWorkForConfig reports only claimed work for
// progress-stall recycle. Open assigned work has not been claimed yet and must
// not suppress claim-less parked-session recovery.
func sessionHasInProgressAssignedWorkForConfig(store beads.Store, rigStores map[string]beads.Store, session beads.Bead, cfg *config.City) (bool, error) {
	return sessionHasAssignedWorkInStoresForStatuses(store, rigStores, sessionAssignmentIdentifiersForConfig(session, cfg), []string{"in_progress"})
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
	identifiers := sessionAssignmentIdentifiersForConfig(session, cfg)
	storeRef, ok := assignedWorkStoreRefForSession(cityPath, cfg, session)
	if !ok {
		return sessionHasOpenAssignedWorkInStores(store, rigStores, identifiers)
	}
	if storeRef == "" {
		return sessionHasOpenAssignedWorkInStoreByIdentifiers(store, identifiers)
	}
	rigStore, ok := rigStores[storeRef]
	if !ok || rigStore == nil {
		return false, fmt.Errorf("rig store %q unavailable for session %q", storeRef, session.Metadata["session_name"])
	}
	return sessionHasOpenAssignedWorkInStoreByIdentifiers(rigStore, identifiers)
}

// sessionHasAwakeAssignedWorkForReachableStore reports whether assigned work
// should keep a session awake: in-progress work always counts, while open work
// counts only when it is ready: unblocked, not deferred, and not ready-excluded.
func sessionHasAwakeAssignedWorkForReachableStore(
	cityPath string,
	cfg *config.City,
	store beads.Store,
	rigStores map[string]beads.Store,
	session beads.Bead,
) (bool, error) {
	identifiers := sessionAssignmentIdentifiersForConfig(session, cfg)
	storeRef, ok := assignedWorkStoreRefForSession(cityPath, cfg, session)
	if !ok {
		return sessionHasAwakeAssignedWorkInStores(store, rigStores, identifiers)
	}
	if storeRef == "" {
		return sessionHasAwakeAssignedWorkInStoreByIdentifiers(store, identifiers)
	}
	rigStore, ok := rigStores[storeRef]
	if !ok || rigStore == nil {
		return false, fmt.Errorf("rig store %q unavailable for session %q", storeRef, session.Metadata["session_name"])
	}
	return sessionHasAwakeAssignedWorkInStoreByIdentifiers(rigStore, identifiers)
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

// firstOpenAssignedWorkBeadForReachableStore returns the first open or
// in-progress work bead still assigned to the given session in the store the
// session's configured agent can query, plus whether one was found. Uses the
// same reachability resolution as sessionHasOpenAssignedWorkForReachableStore
// (configured agent's store, with cross-store fallback when the agent
// template isn't resolvable); emission sites that need the stranded bead's
// ID (e.g., for the SessionDrainAckedWithAssignedWork event payload per
// gastownhall/gascity#2293) call this instead of the bool-only helper.
// Status iteration prefers "in_progress" over "open" so the bead returned is
// the most-urgent stranded candidate — this is intentional and asymmetric
// with the bool helpers, which short-circuit on any match and so iterate
// in the historical "open" / "in_progress" order.
// Returns (zero-bead, false, nil) when nothing matches.
func firstOpenAssignedWorkBeadForReachableStore(
	cityPath string,
	cfg *config.City,
	store beads.Store,
	rigStores map[string]beads.Store,
	session beads.Bead,
) (beads.Bead, bool, error) {
	identifiers := sessionAssignmentIdentifiersForConfig(session, cfg)
	storeRef, ok := assignedWorkStoreRefForSession(cityPath, cfg, session)
	if !ok {
		if bead, found, err := firstOpenAssignedWorkBeadInStoreByIdentifiers(store, identifiers); err != nil || found {
			return bead, found, err
		}
		for _, rs := range rigStores {
			if bead, found, err := firstOpenAssignedWorkBeadInStoreByIdentifiers(rs, identifiers); err != nil || found {
				return bead, found, err
			}
		}
		return beads.Bead{}, false, nil
	}
	if storeRef == "" {
		return firstOpenAssignedWorkBeadInStoreByIdentifiers(store, identifiers)
	}
	rigStore, ok := rigStores[storeRef]
	if !ok || rigStore == nil {
		return beads.Bead{}, false, fmt.Errorf("rig store %q unavailable for session %q", storeRef, session.Metadata["session_name"])
	}
	return firstOpenAssignedWorkBeadInStoreByIdentifiers(rigStore, identifiers)
}

func firstOpenAssignedWorkBeadInStoreByIdentifiers(store beads.Store, identifiers []string) (beads.Bead, bool, error) {
	if store == nil {
		return beads.Bead{}, false, nil
	}
	seen := make(map[string]struct{}, len(identifiers))
	for _, status := range []string{"in_progress", "open"} {
		for _, assignee := range identifiers {
			if assignee == "" {
				continue
			}
			key := status + "\x00" + assignee
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			items, err := store.List(beads.ListQuery{Assignee: assignee, Status: status, Live: true, TierMode: beads.TierBoth})
			if err != nil {
				return beads.Bead{}, false, err
			}
			for _, item := range items {
				if sessionpkg.IsSessionBeadOrRepairable(item) {
					continue
				}
				return item, true, nil
			}
		}
	}
	return beads.Bead{}, false, nil
}

// strandedEventEmittedKey is the per-session-bead throttle marker for
// session.stranded diagnostics. Set after the first emission so the
// reconciler doesn't re-fire the event on every subsequent tick while
// the same orphaned condition holds. Cleared implicitly when the bead
// is closed (and a fresh session bead, with its own generation, gets
// its own opportunity to emit).
const strandedEventEmittedKey = "stranded_event_emitted_at"

// strandedWorkIDListLimit caps how many work bead IDs land in the
// session.stranded message body. Anything beyond that is summarized as
// "+N more" so a runaway count doesn't produce an unbounded message.
const strandedWorkIDListLimit = 10

// emitSessionStrandedDiagnostic records a session.stranded event when
// the reconciler observes a pool-managed session bead that is no
// longer alive but still has open in_progress work assigned. Throttled
// per session bead via metadata so repeated reconciler ticks of the
// same condition only emit once.
//
// The in-memory throttle marker on session.Metadata is set BEFORE the
// durable store write, so a SetMetadata failure (disk pressure,
// partition, slow remote) cannot cause the next tick to re-read the
// unmarked bead and emit a duplicate event. SetMetadata is best-effort
// for cross-restart durability; the in-memory marker is the
// load-bearing single-emission guarantee within a controller lifetime.
func emitSessionStrandedDiagnostic(
	cityPath string,
	cfg *config.City,
	store beads.Store,
	rigStores map[string]beads.Store,
	session *beads.Bead,
	template string,
	rec events.Recorder,
	clk clock.Clock,
	stderr io.Writer,
) {
	if rec == nil || session == nil {
		return
	}
	if session.Metadata == nil {
		session.Metadata = make(map[string]string, 1)
	}
	if strings.TrimSpace(session.Metadata[strandedEventEmittedKey]) != "" {
		return
	}
	assignedWork, err := collectSessionAssignedWork(cityPath, cfg, store, rigStores, *session)
	if err != nil {
		fmt.Fprintf(stderr, "session reconciler: collecting stranded work ids for %s: %v\n", session.Metadata["session_name"], err) //nolint:errcheck
	}
	diagnosticWork := filterDetachedStrandedDiagnosticWork(assignedWork)
	if err == nil && len(assignedWork) > 0 && len(diagnosticWork) == 0 {
		return
	}
	ids := strandedAssignedWorkIDs(diagnosticWork)
	now := clk.Now().UTC()
	rec.Record(events.Event{
		Type:    events.SessionStranded,
		Ts:      now,
		Actor:   "gc",
		Subject: session.ID,
		Message: formatStrandedMessage(template, session.Metadata["session_name"], ids),
		Payload: api.SessionStrandedPayloadJSON(session.ID, session.Metadata["session_name"], template, ids),
	})
	// Set the in-memory marker first so a SetMetadata failure below
	// can't cause the next tick (still seeing this same *Bead value or
	// a re-fetch with the durable write missing) to emit again.
	session.Metadata[strandedEventEmittedKey] = now.Format(time.RFC3339)
	if err := store.SetMetadata(session.ID, strandedEventEmittedKey, now.Format(time.RFC3339)); err != nil {
		fmt.Fprintf(stderr, "session reconciler: stamping stranded throttle marker on %s: %v\n", session.ID, err) //nolint:errcheck
	}
}

type strandedAssignedWork struct {
	bead  beads.Bead
	store beads.Store
}

func filterDetachedStrandedDiagnosticWork(work []strandedAssignedWork) []strandedAssignedWork {
	if len(work) == 0 {
		return work
	}
	out := make([]strandedAssignedWork, 0, len(work))
	for _, item := range work {
		spec := strings.TrimSpace(item.bead.Metadata[detachedProbeMetadataKey])
		if spec == "" {
			out = append(out, item)
			continue
		}
		result := probeDetachedWork(context.Background(), spec)
		switch result.Status {
		case detachedProbeAlive:
			log.Printf("session reconciler: suppressing session.stranded for %s: detached probe alive: %s", item.bead.ID, spec)
			continue
		case detachedProbeDead:
			log.Printf("session reconciler: clearing dead detached probe for %s before session.stranded: %s", item.bead.ID, spec)
			clearDetachedProbeMetadata(item.store, item.bead.ID)
			out = append(out, item)
		default:
			log.Printf("session reconciler: preserving session.stranded for %s after detached probe %s: %v", item.bead.ID, result.Status, result.Err)
			out = append(out, item)
		}
	}
	return out
}

// formatStrandedMessage builds the diagnostic message body for a
// session.stranded event. Names the agent template and lists the
// stranded work bead IDs (truncated past strandedWorkIDListLimit).
func formatStrandedMessage(template, sessionName string, ids []string) string {
	if template == "" {
		template = "<unknown-template>"
	}
	prefix := fmt.Sprintf("pool session %q (template %q) terminated without clean drain", strings.TrimSpace(sessionName), template)
	if len(ids) == 0 {
		return prefix + "; close gate retains slot — work assignee count unavailable"
	}
	shown := ids
	suffix := ""
	if len(ids) > strandedWorkIDListLimit {
		shown = ids[:strandedWorkIDListLimit]
		suffix = fmt.Sprintf(" (+%d more)", len(ids)-strandedWorkIDListLimit)
	}
	return fmt.Sprintf("%s; %d in-progress work bead(s) stranded: %s%s",
		prefix, len(ids), strings.Join(shown, ","), suffix)
}

// collectSessionAssignedWork returns the open/in_progress work beads
// assigned to the session, excluding session beads themselves, along
// with the store that owns each work bead. Mirrors the identifier
// resolution and store routing of sessionHasOpenAssignedWorkForReachableStore
// so the diagnostic path lists and mutates exactly the beads the gate
// considered when deciding to emit.
//
// Without this alignment the gate could see assigned work (via the
// config-derived named-session identity, or via a rig-store-routed
// query) while the collector queried only the bare bead identifiers
// against every store — producing a "0 stranded beads" message in the
// exact failure mode the diagnostic exists to surface.
func collectSessionAssignedWork(cityPath string, cfg *config.City, store beads.Store, rigStores map[string]beads.Store, session beads.Bead) ([]strandedAssignedWork, error) {
	identifiers := sessionAssignmentIdentifiersForConfig(session, cfg)
	seen := make(map[string]struct{})
	out := make([]strandedAssignedWork, 0, 4)
	collect := func(s beads.Store) error {
		if s == nil {
			return nil
		}
		for _, status := range []string{"open", "in_progress"} {
			for _, assignee := range identifiers {
				if assignee == "" {
					continue
				}
				items, err := s.List(beads.ListQuery{Assignee: assignee, Status: status, Live: true, TierMode: beads.TierBoth})
				if err != nil {
					return err
				}
				for _, item := range items {
					if sessionpkg.IsSessionBeadOrRepairable(item) {
						continue
					}
					if _, dup := seen[item.ID]; dup {
						continue
					}
					seen[item.ID] = struct{}{}
					out = append(out, strandedAssignedWork{bead: item, store: s})
				}
			}
		}
		return nil
	}
	// Route to the same store the gate routed to.
	storeRef, ok := assignedWorkStoreRefForSession(cityPath, cfg, session)
	switch {
	case !ok:
		// No agent template resolvable: gate fans out across the
		// primary store + all rig stores. Mirror that.
		if err := collect(store); err != nil {
			return out, err
		}
		for _, rs := range rigStores {
			if err := collect(rs); err != nil {
				return out, err
			}
		}
	case storeRef == "":
		// Agent template resolvable but no rig store binding: gate
		// queries only the primary store.
		if err := collect(store); err != nil {
			return out, err
		}
	default:
		rigStore, found := rigStores[storeRef]
		if !found || rigStore == nil {
			return out, fmt.Errorf("rig store %q unavailable for session %q", storeRef, session.Metadata["session_name"])
		}
		if err := collect(rigStore); err != nil {
			return out, err
		}
	}
	return out, nil
}

func strandedAssignedWorkIDs(work []strandedAssignedWork) []string {
	ids := make([]string, 0, len(work))
	for _, item := range work {
		ids = append(ids, item.bead.ID)
	}
	return ids
}

func sessionHasOpenAssignedWorkInStore(store beads.Store, session beads.Bead) (bool, error) {
	return sessionHasOpenAssignedWorkInStoreByIdentifiers(store, sessionAssignmentIdentifiers(session))
}

func sessionHasOpenAssignedWorkInStores(store beads.Store, rigStores map[string]beads.Store, identifiers []string) (bool, error) {
	return sessionHasAssignedWorkInStoresForStatuses(store, rigStores, identifiers, []string{"open", "in_progress"})
}

func sessionHasAssignedWorkInStoresForStatuses(store beads.Store, rigStores map[string]beads.Store, identifiers []string, statuses []string) (bool, error) {
	if has, err := sessionHasAssignedWorkInStoreByIdentifiersForStatuses(store, identifiers, statuses); err != nil || has {
		return has, err
	}
	for _, rs := range rigStores {
		if has, err := sessionHasAssignedWorkInStoreByIdentifiersForStatuses(rs, identifiers, statuses); err != nil || has {
			return has, err
		}
	}
	return false, nil
}

func sessionHasAwakeAssignedWorkInStores(store beads.Store, rigStores map[string]beads.Store, identifiers []string) (bool, error) {
	if has, err := sessionHasAwakeAssignedWorkInStoreByIdentifiers(store, identifiers); err != nil || has {
		return has, err
	}
	for _, rs := range rigStores {
		if has, err := sessionHasAwakeAssignedWorkInStoreByIdentifiers(rs, identifiers); err != nil || has {
			return has, err
		}
	}
	return false, nil
}

func sessionHasOpenAssignedWorkInStoreByIdentifiers(store beads.Store, identifiers []string) (bool, error) {
	return sessionHasAssignedWorkInStoreByIdentifiersForStatuses(store, identifiers, []string{"open", "in_progress"})
}

func sessionHasAssignedWorkInStoreByIdentifiersForStatuses(store beads.Store, identifiers []string, statuses []string) (bool, error) {
	if store == nil {
		return false, nil
	}
	seen := make(map[string]struct{}, len(identifiers))
	for _, status := range statuses {
		for _, assignee := range identifiers {
			if assignee == "" {
				continue
			}
			key := status + "\x00" + assignee
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			if has, err := sessionHasOpenAssignedWorkForTier(store, assignee, status, beads.TierIssues, true); err != nil || has {
				return has, err
			}
			if has, err := sessionHasOpenAssignedWispWork(store, assignee, status); err != nil || has {
				return has, err
			}
		}
	}
	return false, nil
}

func sessionHasAwakeAssignedWorkInStoreByIdentifiers(store beads.Store, identifiers []string) (bool, error) {
	if store == nil {
		return false, nil
	}
	seen := make(map[string]struct{}, len(identifiers))
	for _, assignee := range identifiers {
		if assignee == "" {
			continue
		}
		if _, ok := seen[assignee]; ok {
			continue
		}
		seen[assignee] = struct{}{}
		for _, tierMode := range []beads.TierMode{beads.TierIssues, beads.TierWisps} {
			if has, err := sessionHasInProgressAssignedWorkForTier(store, assignee, tierMode); err != nil || has {
				return has, err
			}
			if has, err := sessionHasReadyAssignedWorkForTier(store, assignee, tierMode); err != nil || has {
				return has, err
			}
		}
	}
	return false, nil
}

func sessionHasInProgressAssignedWorkForTier(store beads.Store, assignee string, tierMode beads.TierMode) (bool, error) {
	if tierMode == beads.TierWisps {
		return sessionHasOpenAssignedWispWork(store, assignee, "in_progress")
	}
	return sessionHasOpenAssignedWorkForTier(store, assignee, "in_progress", tierMode, true)
}

func sessionHasOpenAssignedWispWork(store beads.Store, assignee, status string) (bool, error) {
	query := beads.ListQuery{Assignee: assignee, Status: status, TierMode: beads.TierWisps}
	if cache, ok := store.(interface {
		CachedList(beads.ListQuery) ([]beads.Bead, bool)
	}); ok {
		// This positive-only probe intentionally keeps the tier-scoped cache
		// helper: HandlesFor(...).Cached.List reads both tiers by contract.
		if items, ok := cache.CachedList(query); ok {
			if hasNonSessionAssignedWork(items) {
				return true, nil
			}
		}
	}
	return sessionHasOpenAssignedWorkForTier(store, assignee, status, beads.TierWisps, true)
}

func sessionHasReadyAssignedWorkForTier(store beads.Store, assignee string, tierMode beads.TierMode) (bool, error) {
	items, err := beads.ReadyLive(store, beads.ReadyQuery{Assignee: assignee, TierMode: tierMode})
	if err != nil {
		return false, err
	}
	return hasNonSessionAssignedWork(items), nil
}

func sessionHasOpenAssignedWorkForTier(store beads.Store, assignee, status string, tierMode beads.TierMode, live bool) (bool, error) {
	items, err := store.List(beads.ListQuery{Assignee: assignee, Status: status, Live: live, TierMode: tierMode})
	if err != nil {
		return false, err
	}
	return hasNonSessionAssignedWork(items), nil
}

func hasNonSessionAssignedWork(items []beads.Bead) bool {
	for _, item := range items {
		if sessionpkg.IsSessionBeadOrRepairable(item) {
			continue
		}
		return true
	}
	return false
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
	// sessionAttachedConfigDriftFalseNegativeLimit is how long a recorded
	// attached-drift deferral keeps suppressing a config-drift drain, measured
	// from the deferral's stored timestamp (NOT from the last positive
	// attachment observation — see the refresh-interval note below). It bridges
	// transient attachment-detection flicker (a probe momentarily reporting
	// "not attached" between ticks) so a still-attached session is not drained.
	//
	// Because the stamp is refreshed at most once per
	// sessionAttachedConfigDriftRefreshInterval, the GUARANTEED bridge after a
	// positive observation is the worst case (limit - refreshInterval): a
	// positive observation can be skipped just after a refresh, then attachment
	// flickers false, and the deferral lapses limit after the last stored stamp.
	// With the values below that worst case is 5m - 2m = 3m of flicker
	// tolerance, which is ample; attachment detection flickers on the order of a
	// tick, not minutes. Attachment is a reliable signal, so the window is kept
	// generous and is intentionally decoupled from the re-stamp cadence.
	//
	// Tradeoff: raising this from 30s to 5m also raises the worst-case latency
	// from a GENUINE detach to config-drift handling, since after a real detach
	// the not-attached branch is gated solely by this window (drift waits it
	// out, up to ~5m). That is acceptable — config-drift handling is a
	// reconvergence restart, not a safety kill, and the existing activity
	// heuristic already tolerates a 2m staleness window — but it is a conscious
	// cost of removing the per-tick commit churn. The composed safety invariant
	// (refresh interval + patrol < this limit) is asserted by
	// TestRecordSessionAttachedConfigDriftDeferral_SurvivesSkippedRefreshThenFlicker.
	sessionAttachedConfigDriftFalseNegativeLimit = 5 * time.Minute
	// sessionAttachedConfigDriftRefreshInterval is the minimum age the existing
	// stamp must reach before record() rewrites it. It is deliberately SEPARATE
	// from (and smaller than) the false-negative limit: while attached, record()
	// runs every reconciler tick, so the stamp only needs refreshing rarely —
	// just often enough that it never ages out of the false-negative window
	// during a real flicker. Coupling the refresh to the validity limit (the old
	// "rewrite after limit/2" rule) caused a durable metadata write — and a Dolt
	// commit — on essentially every tick whenever the patrol interval was >=
	// limit/2 (e.g. the default 30s patrol with the old 30s limit), flooding the
	// store with no-op churn for every persistently-attached drifted session.
	sessionAttachedConfigDriftRefreshInterval     = 2 * time.Minute
	namedSessionConfigDriftDeferredAtMetadata     = "config_drift_deferred_at"
	namedSessionConfigDriftDeferredKeyMetadata    = "config_drift_deferred_key"
	sessionAttachedConfigDriftDeferredAtMetadata  = "attached_config_drift_deferred_at"
	sessionAttachedConfigDriftDeferredKeyMetadata = "attached_config_drift_deferred_key"
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
	// existing stamp is still fresh. While attached, the reconciler calls this
	// on every tick, so without a throttle it would emit a bead.updated event —
	// and a durable Dolt commit — every tick for every attached session with
	// persistent drift. The refresh interval is decoupled from (and well below)
	// the false-negative limit, so the stamp is rewritten only occasionally yet
	// can never age out of the validity window between two refreshes.
	if driftKey != "" && session.Metadata[sessionAttachedConfigDriftDeferredKeyMetadata] == driftKey {
		if raw := session.Metadata[sessionAttachedConfigDriftDeferredAtMetadata]; raw != "" {
			if existing, err := time.Parse(time.RFC3339, raw); err == nil &&
				!existing.After(now) &&
				now.Sub(existing) < sessionAttachedConfigDriftRefreshInterval {
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
	if store != nil && strings.TrimSpace(session.ID) != "" {
		if _, _, err := sessionpkg.ResolveSessionBeadByExactID(store, session.ID); err != nil && !errors.Is(err, sessionpkg.ErrSessionNotFound) {
			return false, err
		}
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
	agentCfg := sessionCoreConfigForHash(tp, session)
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

func traceHealClearedPendingCreateLease(
	trace *sessionReconcilerTraceCycle,
	session beads.Bead,
	cfg *config.City,
	template string,
	name string,
	stateBeforeHeal string,
	pendingCreateStartedAtBeforeHeal string,
	lastWokeAtBeforeHeal string,
	providerAlive bool,
	batch map[string]string,
) {
	if trace == nil || !pendingCreateQueuedOrCreatingState(stateBeforeHeal) {
		return
	}
	if cleared, ok := batch["pending_create_claim"]; !ok || cleared != "" {
		return
	}
	template = strings.TrimSpace(template)
	if template == "" {
		template = normalizedSessionTemplate(session, cfg)
	}
	if template == "" {
		template = session.Metadata["template"]
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = session.Metadata["session_name"]
	}
	trace.recordDecision("reconciler.session.pending_create", template, name, "heal_cleared_stale_lease", string(TraceOutcomeApplied), traceRecordPayload{
		"last_woke_at":              lastWokeAtBeforeHeal,
		"pending_create_started_at": pendingCreateStartedAtBeforeHeal,
		"provider_alive":            providerAlive,
		"state_after":               session.Metadata["state"],
		"state_before":              stateBeforeHeal,
	}, nil, "")
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
	if !sessionActivityReportable(sp, name) {
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
	// Preserve resume-eligible prior conversation metadata (session_key +
	// started_config_hash) when transitioning straight back into creating,
	// so the next wake builds `--resume <prior-key>` instead of
	// `--session-id <new-uuid>`. Gated on StateCreating because the asleep
	// repair path (called from the asleep-named-session drift block) must
	// still clear started_config_hash — an asleep-bound reset that
	// preserved the stale hash would re-trigger drift every tick.
	// Conversation health is validated post-start: a stale resume that
	// Claude rejects is recovered by recordWakeFailure clearing both
	// fields, and the next reconcile tick mints a fresh session_key.
	// This intentionally reads the current per-session snapshot at this
	// call site and does not provide CAS protection — external store
	// implementations may apply SetMetadataBatch sequentially with partial
	// application possible. If preservation is extended to additional
	// reset sites, reload via store.Get or add conditional-write support
	// before deciding what to preserve.
	nextSessionState := sessionpkg.State(nextState)
	priorSessionKey := strings.TrimSpace(session.Metadata["session_key"])
	priorStartedConfigHash := strings.TrimSpace(session.Metadata["started_config_hash"])
	preserveResume := (nextSessionState == sessionpkg.StateStartPending || nextSessionState == sessionpkg.StateCreating) &&
		priorSessionKey != "" && priorStartedConfigHash != ""

	rotatedSessionKey := ""
	if preserveResume {
		rotatedSessionKey = priorSessionKey
	} else if newKey, err := sessionpkg.GenerateSessionKey(); err == nil {
		rotatedSessionKey = newKey
	}
	batch := sessionpkg.ConfigDriftResetPatch(nextSessionState, rotatedSessionKey, now)
	if preserveResume {
		batch["started_config_hash"] = priorStartedConfigHash
	}
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
	if store == nil {
		return ""
	}
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
			TierMode: beads.TierBoth,
			Sort:     beads.SortCreatedDesc,
		})
		if err != nil {
			continue
		}
		for _, b := range assigned {
			wd := strings.TrimSpace(b.Metadata["work_dir"])
			if wd != "" {
				if info, err := os.Stat(wd); err == nil && info.IsDir() {
					return wd
				}
			}
		}
	}
	return ""
}

const dispatchOptionMetadataPrefix = "opt_"

func dispatchOptionMetadataKey(key string) string {
	return dispatchOptionMetadataPrefix + key
}

// resolveTaskOptionOverrides returns provider option choices requested by the
// newest in-progress work bead assigned to the candidate's identifiers. Work
// beads use the same opt_<OptionsSchema key> metadata convention as session
// beads, so a provider can consume opt_model, opt_effort, or future schema
// options without a new gc.* field. Values are validated against the resolved
// provider OptionsSchema and invalid values are skipped.
func resolveTaskOptionOverrides(store beads.Store, rp *config.ResolvedProvider, assignees ...string) map[string]string {
	if store == nil || rp == nil || len(rp.OptionsSchema) == 0 {
		return nil
	}
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
			TierMode: beads.TierBoth,
			Sort:     beads.SortCreatedDesc,
		})
		if err != nil {
			continue
		}
		for _, b := range assigned {
			overrides, sawOptions := workBeadOptionOverrides(b, rp)
			if sawOptions {
				return overrides
			}
		}
	}
	return nil
}

func workBeadOptionOverrides(b beads.Bead, rp *config.ResolvedProvider) (map[string]string, bool) {
	if rp == nil {
		return nil, false
	}
	overrides := make(map[string]string)
	sawOptions := false
	for _, opt := range rp.OptionsSchema {
		metadataKey := dispatchOptionMetadataKey(opt.Key)
		raw, ok := b.Metadata[metadataKey]
		if !ok {
			continue
		}
		sawOptions = true
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		if _, err := config.ResolveExplicitOptions(rp.OptionsSchema, map[string]string{opt.Key: value}); err != nil {
			log.Printf("work %s: ignoring %s=%q: %v", b.ID, metadataKey, value, err)
			continue
		}
		overrides[opt.Key] = value
	}
	return overrides, sawOptions
}

type assignedTaskWorkDir struct {
	path      string
	createdAt time.Time
}

// newAssignedTaskWorkDirResolver resolves work_dir values from the
// reconciler's snapshot; misses intentionally fall back to the live lookup.
func newAssignedTaskWorkDirResolver(assignedWorkBeads []beads.Bead) taskWorkDirResolver {
	index := make(map[string]assignedTaskWorkDir)
	for _, bead := range assignedWorkBeads {
		if bead.Status != "in_progress" {
			continue
		}
		assignee := strings.TrimSpace(bead.Assignee)
		if assignee == "" {
			continue
		}
		workDir := strings.TrimSpace(bead.Metadata["work_dir"])
		if workDir == "" {
			continue
		}
		info, err := os.Stat(workDir)
		if err != nil || !info.IsDir() {
			continue
		}
		current, ok := index[assignee]
		if ok && !bead.CreatedAt.After(current.createdAt) {
			continue
		}
		index[assignee] = assignedTaskWorkDir{path: workDir, createdAt: bead.CreatedAt}
	}
	return func(candidate startCandidate, cfg *config.City) string {
		for _, assignee := range taskWorkDirAssignees(candidate, cfg) {
			if workDir := index[strings.TrimSpace(assignee)].path; workDir != "" {
				return workDir
			}
		}
		return ""
	}
}

// truncateHashForLog returns a short representation of a fingerprint hash
// for log output. Preserves any v<digits>: prefix so the version stays
// visible alongside the hex tail.
func truncateHashForLog(h string) string {
	if i := strings.IndexByte(h, ':'); i >= 0 {
		end := i + 1 + 10
		if end > len(h) {
			end = len(h)
		}
		return h[:end]
	}
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// rebaselineLegacyHashOutcome picks the trace outcome that matches a
// stored hash about to be silently rebaselined.
func rebaselineLegacyHashOutcome(stored string) TraceOutcomeCode {
	if runtime.IsVersionMismatchedHash(stored) {
		return TraceOutcomeRebaselinedVersionMismatch
	}
	return TraceOutcomeRebaselinedUnversioned
}

// sessionHashRebaselineMetadata builds the four fingerprint metadata fields
// — started_config_hash, started_live_hash, live_hash, core_hash_breakdown —
// from a resolved agent config. Callers merge the result into a session
// bead's metadata batch to move its config-drift baseline to agentCfg.
func sessionHashRebaselineMetadata(agentCfg runtime.Config) (map[string]string, error) {
	breakdownJSON, err := json.Marshal(runtime.CoreFingerprintBreakdown(agentCfg))
	if err != nil {
		return nil, fmt.Errorf("marshaling core_hash_breakdown: %w", err)
	}
	liveHash := runtime.LiveFingerprint(agentCfg)
	return map[string]string{
		"started_config_hash": runtime.CoreFingerprint(agentCfg),
		"started_live_hash":   liveHash,
		"live_hash":           liveHash,
		"core_hash_breakdown": string(breakdownJSON),
	}, nil
}

// silentRebaselineSessionHashes overwrites the four fingerprint metadata
// fields (started_config_hash, started_live_hash, live_hash,
// core_hash_breakdown) with values produced by the current binary. Used
// when a stored hash carries no version prefix or a version prefix that
// does not match runtime.FingerprintVersion. The reconciler invokes this
// instead of draining the session — the hash mismatch is purely a
// versioning artifact, not real config drift.
func silentRebaselineSessionHashes(session *beads.Bead, store beads.Store, agentCfg runtime.Config) error {
	if session == nil || store == nil {
		return nil
	}
	patch, err := sessionHashRebaselineMetadata(agentCfg)
	if err != nil {
		return err
	}
	if err := store.SetMetadataBatch(session.ID, patch); err != nil {
		return fmt.Errorf("rebaselining hashes: %w", err)
	}
	if session.Metadata == nil {
		session.Metadata = make(map[string]string, len(patch))
	}
	for k, v := range patch {
		session.Metadata[k] = v
	}
	return nil
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
