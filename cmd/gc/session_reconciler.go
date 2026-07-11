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
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
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

// lifecycleTimerBlockerInfo is the session.Info sibling of lifecycleTimerBlocker:
// it reports the active lifecycle timer blocker (user hold / quarantine) from the
// typed Info.HeldUntil / Info.QuarantinedUntil mirrors, using the same
// metadataTimeInFuture rule. Equivalence-proven (TestSessionClassifierInfoEquivalence).
func lifecycleTimerBlockerInfo(info sessionpkg.Info, now time.Time) string {
	switch {
	case metadataTimeInFuture(info.HeldUntil, now):
		return "user_hold"
	case metadataTimeInFuture(info.QuarantinedUntil, now):
		return "quarantine"
	default:
		return ""
	}
}

func isDrainAckStopPending(session beads.Bead) bool {
	return strings.TrimSpace(session.Metadata["state"]) == string(sessionpkg.StateDraining) &&
		strings.TrimSpace(session.Metadata["state_reason"]) == sessionpkg.DrainAckStopPendingReason
}

// isDrainAckStopPendingInfo is the session.Info sibling of isDrainAckStopPending:
// it reports whether a session is parked in the drain-ack stop-pending state from
// the typed Info.MetadataState (raw "state") / Info.StateReason mirrors, with the
// same TrimSpace compares. Equivalence-proven (TestSessionClassifierInfoEquivalence).
func isDrainAckStopPendingInfo(info sessionpkg.Info) bool {
	return strings.TrimSpace(info.MetadataState) == string(sessionpkg.StateDraining) &&
		strings.TrimSpace(info.StateReason) == sessionpkg.DrainAckStopPendingReason
}

// markDrainAckStopPending persists the drain-ack stop-pending transition through
// the session front door, reading the session identity/name from the typed Info
// snapshot (front-door migration Step 5b). It no longer mirrors the patch onto a
// raw *beads.Bead: the two reconciler callers reconstruct DrainAckStopPendingPatch
// and fold it onto infoByID themselves, and no later this-tick reader consumes the
// raw bead for these keys — a drain-acked session `continue`s before the
// wakeTargets/startCandidates append, and the post-loop scans read only ordered[i].ID.
func markDrainAckStopPending(info sessionpkg.Info, sessFront *sessionpkg.Store, clk clock.Clock, stderr io.Writer) bool {
	if info.ID == "" || sessFront == nil {
		return false
	}
	if stderr == nil {
		stderr = io.Discard
	}
	batch := sessionpkg.DrainAckStopPendingPatch(clk.Now().UTC())
	if err := sessFront.ApplyPatch(info.ID, batch); err != nil {
		name := strings.TrimSpace(info.SessionNameMetadata)
		if name == "" {
			name = info.ID
		}
		fmt.Fprintf(stderr, "session reconciler: marking drain-ack stop-pending %s: %v\n", name, err) //nolint:errcheck
		return false
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

// resetPendingCommittedAtInfo is the session.Info mirror of
// resetPendingCommittedAt: it reads the raw continuation_reset_pending and
// reset_committed_at markers (Info.ContinuationResetPending / Info.ResetCommittedAt)
// with the same trim + RFC3339 parse rules.
func resetPendingCommittedAtInfo(info sessionpkg.Info) (string, time.Time, bool) {
	if strings.TrimSpace(info.ContinuationResetPending) != "true" {
		return "", time.Time{}, false
	}
	raw := strings.TrimSpace(info.ResetCommittedAt)
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
			Type:      events.SessionResetStalled,
			Actor:     "gc",
			Subject:   name,
			Message:   msg,
			SessionID: session.ID,
			Payload:   events.SessionResetStalledPayloadJSON(name, template, resetCommittedAt, elapsedSeconds),
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
		Type:      events.SessionDrainAckedWithAssignedWork,
		Actor:     "gc",
		Subject:   subject,
		Message:   "session drain-acked while still assigned to work bead",
		SessionID: session.ID,
		Payload: api.SessionDrainAckedWithAssignedWorkPayloadJSON(
			session.ID,
			strandedBead.ID,
			template,
			strandedBead.Status,
			"drain_acked_with_assigned_work",
		),
	})
}

// drainAckFinalizeResult captures the Info-snapshot effect of a
// finalizeDrainAckStoppedSession call so the reconciler can refresh its typed
// infoByID snapshot from the write it just performed (front-door migration Step
// 6d write-returns-Info) instead of re-projecting the raw working bead. The zero
// value is a no-op — the call mutated nothing (async/early-return/persist-error)
// so applyTo returns the snapshot Info unchanged.
type drainAckFinalizeResult struct {
	// batch is the metadata patch mirrored onto the session bead this call: the
	// close ClosePatch (Path A) or the AcknowledgeDrain/CompleteDrain patch (the
	// non-close drain-ack path). nil when the call wrote no metadata.
	batch sessionpkg.MetadataPatch
	// closed reports that the call closed the bead in memory
	// (session.Status = "closed"); the snapshot must fold that status close via
	// MarkClosed, which no metadata patch can carry (Info.Closed derives from
	// Status, not metadata).
	closed bool
	// witnessInfo carries a full reprojection for the NDI witness close, where the
	// call adopts the store's authoritative metadata wholesale
	// (session.Metadata = latest.Metadata) rather than applying a known patch, so
	// the post-Info cannot be folded from batch and is reprojected instead.
	witnessInfo *sessionpkg.Info
}

// applyTo folds the finalize result onto the coherent pre-call snapshot Info,
// byte-identically to re-projecting the mutated bead (the raw refreshSessionInfo
// path): the witness reprojection wins outright; otherwise the metadata patch
// folds via ApplyPatch and an in-memory close folds via MarkClosed. The caller
// must pass the session's coherent snapshot entry — infoByID[id] equal to the
// pre-call InfoFromPersistedBead(*session) — which holds at every finalize call
// site (top-of-loop / post-heal / post-zombie refresh, no un-refreshed *session
// mutation reaches the call).
func (r drainAckFinalizeResult) applyTo(info sessionpkg.Info) sessionpkg.Info {
	if r.witnessInfo != nil {
		return *r.witnessInfo
	}
	if r.batch != nil {
		info = info.ApplyPatch(r.batch)
	}
	if r.closed {
		info = info.MarkClosed()
	}
	return info
}

func finalizeDrainAckStoppedSession(
	cityPath string,
	cfg *config.City,
	store beads.Store,
	rigStores map[string]beads.Store,
	session *beads.Bead,
	info sessionpkg.Info,
	template string,
	closeIfUnassigned bool,
	dops drainOps,
	dt *drainTracker,
	clk clock.Clock,
	rec events.Recorder,
	stderr io.Writer,
) drainAckFinalizeResult {
	if session == nil || store == nil || session.ID == "" {
		return drainAckFinalizeResult{}
	}
	// Decision reads come off the typed Info snapshot (front-door migration Step
	// 5b); the raw *session is retained only for the whole-bead raw-by-design
	// helpers below (sessionHasOpenAssignedWorkForReachableStore,
	// closeSessionBeadIfReachableStoreUnassigned, recordDrainAckAssignedWorkEvent,
	// sessionAgentMetricIdentity) and the store.Get witness reprojection. Callers
	// pass the coherent infoByID[session.ID] (== InfoFromPersistedBead(*session)).
	name := strings.TrimSpace(info.SessionNameMetadata)
	if template == "" {
		template = normalizedSessionTemplateInfo(info, cfg)
	}
	if template == "" {
		template = info.Template
	}
	recordStopped := func(performedStop bool) {
		// gc.agent.stops.total counts the stop action, so only the observer
		// that actually performs the stop transition records it. Under NDI
		// multiple observers process the same drain-ack; the witness branch
		// (the bead was already closed by another observer) still re-emits the
		// SessionStopped event for parity with existing event semantics (events
		// dedupe downstream by session id) but must not inflate the monotonic
		// action counter.
		if performedStop {
			telemetry.RecordAgentStop(context.Background(), name, sessionAgentMetricIdentity(*session, cfg), "drain-ack", nil)
		}
		if rec == nil {
			return
		}
		rec.Record(events.Event{
			Type:      events.SessionStopped,
			Actor:     "gc",
			Subject:   template,
			Message:   "drain acknowledged by agent",
			SessionID: session.ID,
			Payload:   api.SessionLifecyclePayloadJSON(session.ID, template, "drain acknowledged"),
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
			closePatch := sessionpkg.ClosePatch(clk.Now().UTC(), "drained")
			if dops != nil {
				_ = dops.clearDrain(name)
			}
			if dt != nil {
				dt.clearIdleProbe(session.ID)
				dt.remove(session.ID)
			}
			recordStopped(true)
			// write-returns-Info (Step 6d): the snapshot fold is ApplyPatch(the
			// ClosePatch) + MarkClosed(the Status="closed"). The raw metadata mirror
			// loop is dropped (Step 5b) — no later this-tick reader consumes the raw
			// bead metadata; the raw session.Status="closed" set stays (a struct field,
			// not a Metadata bracket write, and asserted by the telemetry close-path test).
			return drainAckFinalizeResult{batch: closePatch, closed: true}
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
			recordStopped(false)
			// NDI witness close: another observer already closed the bead and this
			// call adopted its authoritative metadata wholesale, so the post-Info is
			// a full reprojection, not a patch fold. This is the one finalize path
			// still reading the raw bead; it is byte-identical to the old
			// refreshSessionInfo and is reworked when the lockstep drops.
			witnessInfo := sessionpkg.InfoFromPersistedBead(*session)
			return drainAckFinalizeResult{witnessInfo: &witnessInfo}
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
	batch := sessionpkg.AcknowledgeDrainPatch(info.WakeMode == "fresh")
	if hasAssignedWork {
		batch = sessionpkg.CompleteDrainPatch(clk.Now().UTC(), "idle", info.WakeMode == "fresh")
	}
	// A drain-ack that completes a restart-request cycle (gc session reset →
	// agent drain-ack) must also consume restart_requested. The drain-ack
	// branch handles the stop and continues before the restart-requested
	// branch runs, so nothing else clears the flag; if it survives in the
	// store, a later cache-reconcile re-emission resurrects it and the
	// controller honors it as a fresh restart request — a phantom second
	// restart that rotates session_key and destroys resume continuity (#2574).
	if info.RestartRequested == "true" {
		batch["restart_requested"] = ""
	}
	if err := sessionFrontDoor(store).ApplyPatch(session.ID, batch); err != nil {
		fmt.Fprintf(stderr, "session reconciler: finalizing drain-ack stopped %s: %v\n", name, err) //nolint:errcheck
		// Store write failed, so nothing changed — the snapshot must stay unchanged
		// (zero result → applyTo no-op).
		return drainAckFinalizeResult{}
	}
	// The raw metadata mirror loop is dropped (Step 5b): the caller folds the
	// returned batch onto infoByID, and no later this-tick reader consumes the raw
	// bead metadata for these keys (a drain-acked session `continue`s before the
	// wakeTargets/startCandidates append; recordStopped/recordDrainAckAssignedWorkEvent
	// below read identity + store-query results, not the drain-ack batch keys).
	if dops != nil {
		_ = dops.clearDrain(name)
	}
	if dt != nil {
		dt.clearIdleProbe(session.ID)
		dt.remove(session.ID)
	}
	recordStopped(true)
	if hasAssignedWork {
		recordDrainAckAssignedWorkEvent(cityPath, cfg, store, rigStores, *session, template, template, name, rec, stderr)
	}
	// Non-close drain-ack: the snapshot fold is ApplyPatch(the drain-ack batch just
	// mirrored) with no status close.
	return drainAckFinalizeResult{batch: batch}
}

func reconcileDrainAckStopPending(
	cityPath string,
	cfg *config.City,
	sp runtime.Provider,
	store beads.Store,
	rigStores map[string]beads.Store,
	session *beads.Bead,
	info sessionpkg.Info,
	tp TemplateParams,
	desired bool,
	dops drainOps,
	dt *drainTracker,
	asyncStopTracker *asyncStartTracker,
	clk clock.Clock,
	rec events.Recorder,
	stderr io.Writer,
) (bool, drainAckFinalizeResult) {
	if session == nil || !isDrainAckStopPendingInfo(info) {
		return false, drainAckFinalizeResult{}
	}
	name := strings.TrimSpace(info.SessionNameMetadata)
	obs, err := workerObserveSessionTargetWithRuntimeHintsWithConfig(cityPath, store, sp, cfg, session.ID, tp.Hints.ProcessNames)
	if err != nil || obs.Running || obs.Alive {
		// Async-stop: queueDrainAckAsyncStop takes the session ID (not *session) and
		// mutates only the async tracker, so the bead is untouched and the snapshot
		// stays coherent — a zero result (applyTo no-op) matches the old refresh of
		// the unmutated bead.
		queueDrainAckAsyncStop(cityPath, store, sp, cfg, session.ID, name, asyncStopTracker, stderr)
		return true, drainAckFinalizeResult{}
	}
	return true, finalizeDrainAckStoppedSession(
		cityPath, cfg, store, rigStores, session, info, tp.TemplateName,
		!desired || isPoolManagedSessionBead(*session),
		dops, dt, clk, rec, stderr,
	)
}

func finalizeDrainAckStopPendingSessions(
	cityPath string,
	cfg *config.City,
	sp runtime.Provider,
	sessStore beads.SessionStore,
	rigStores map[string]beads.Store,
	sessions []beads.Bead,
	dops drainOps,
	dt *drainTracker,
	asyncStopTracker *asyncStartTracker,
	clk clock.Clock,
	rec events.Recorder,
	stderr io.Writer,
) int {
	// Session class typed at the boundary; the drain-ack helpers below take the
	// unwrapped beads.Store. Same underlying store value, behavior unchanged.
	store := sessStore.Store
	if store == nil || sp == nil || len(sessions) == 0 {
		return 0
	}
	finalized := 0
	for i := range sessions {
		session := &sessions[i]
		// Boundary per-bead projection (same pattern as the advanceSessionDrains
		// wrappers): this non-reconciler pass loads its own []beads.Bead, so it
		// projects Info here and feeds the drain-ack helpers off it.
		info := sessionpkg.InfoFromPersistedBead(*session)
		if !isDrainAckStopPendingInfo(info) {
			continue
		}
		name := strings.TrimSpace(info.SessionNameMetadata)
		obs, err := workerObserveSessionTargetWithRuntimeHintsWithConfig(cityPath, store, sp, cfg, session.ID, nil)
		if err != nil || obs.Running || obs.Alive {
			queueDrainAckAsyncStop(cityPath, store, sp, cfg, session.ID, name, asyncStopTracker, stderr)
			continue
		}
		// Pool-managed stop-pending beads close here instead of staying open as
		// state=drained: open pool session beads occupy slots in the next demand
		// calculation, while closed beads remain only as lifecycle history.
		finalizeDrainAckStoppedSession(
			cityPath, cfg, store, rigStores, session, info,
			normalizedSessionTemplateInfo(info, cfg),
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
	// No resume capability detected in provider config or bead metadata.
	// Return hasCapability=true so the caller clears any stored session_key;
	// leaving a stale key would trigger stale-key detection on the next start.
	return "", true
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

// pendingCreateSessionStillLeasedInfo is the session.Info sibling of
// pendingCreateSessionStillLeased. Equivalence-proven. The template resolution
// mirrors the raw form: normalizedSessionTemplateInfo with an Info.Template
// fallback (Info.Template is the raw metadata["template"] mirror), and
// findAgentByTemplate keys off the same resolved template. The claim branch and
// the sessionStartRequestedInfo fallback both compose already-proven siblings.
func pendingCreateSessionStillLeasedInfo(i sessionpkg.Info, cfg *config.City, clk clock.Clock) bool {
	var startupTimeout time.Duration
	if cfg != nil {
		startupTimeout = cfg.Session.StartupTimeoutDuration()
	}
	if i.PendingCreateClaim {
		if !pendingCreateLeaseActiveInfo(i, clk, startupTimeout) {
			return false
		}
		template := normalizedSessionTemplateInfo(i, cfg)
		if template == "" {
			template = i.Template
		}
		agent := findAgentByTemplate(cfg, template)
		if agent != nil {
			return !agent.Suspended
		}
		return true
	}
	if !sessionStartRequestedInfo(i, clk) {
		return false
	}
	template := normalizedSessionTemplateInfo(i, cfg)
	if template == "" {
		template = i.Template
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

// pendingCreateStartInFlightInfo is the session.Info sibling of
// pendingCreateStartInFlight. Equivalence-proven.
func pendingCreateStartInFlightInfo(i sessionpkg.Info, clk clock.Clock, startupTimeout time.Duration) bool {
	if !i.PendingCreateClaim &&
		sessionpkg.State(strings.TrimSpace(i.MetadataState)) != sessionpkg.StateCreating {
		return false
	}
	lastWoke := strings.TrimSpace(i.LastWokeAt)
	if lastWoke == "" {
		return false
	}
	started, err := time.Parse(time.RFC3339, lastWoke)
	if err != nil {
		return false
	}
	if startupTimeout <= 0 {
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

// pendingCreateLeaseActiveInfo is the session.Info sibling of
// pendingCreateLeaseActive. Equivalence-proven.
func pendingCreateLeaseActiveInfo(i sessionpkg.Info, clk clock.Clock, startupTimeout time.Duration) bool {
	if !i.PendingCreateClaim {
		return false
	}
	if pendingCreateStartInFlightInfo(i, clk, startupTimeout) {
		return true
	}
	if strings.TrimSpace(i.LastWokeAt) == "" {
		return !pendingCreateNeverStartedLeaseExpiredInfo(i, clk)
	}
	return !pendingCreateAttemptStaleInfo(i, clk)
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

// pendingCreateNeverStartedExpiredInfo is the session.Info sibling of
// pendingCreateNeverStartedExpired. Info.MetadataState is the RAW state metadata
// (verbatim, untrimmed), matching the raw session.Metadata["state"] handed to
// pendingCreateRollbackState (which trims internally). Equivalence-proven.
func pendingCreateNeverStartedExpiredInfo(i sessionpkg.Info, clk clock.Clock) bool {
	if !i.PendingCreateClaim {
		return false
	}
	if !pendingCreateRollbackState(i.MetadataState) {
		return false
	}
	return pendingCreateNeverStartedLeaseExpiredInfo(i, clk)
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

// pendingCreateNeverStartedLeaseExpiredInfo is the session.Info sibling of
// pendingCreateNeverStartedLeaseExpired. Equivalence-proven.
func pendingCreateNeverStartedLeaseExpiredInfo(i sessionpkg.Info, clk clock.Clock) bool {
	if !i.PendingCreateClaim {
		return false
	}
	if strings.TrimSpace(i.LastWokeAt) != "" {
		return false
	}
	anchor := i.CreatedAt
	if started, ok := parseRFC3339Metadata(i.PendingCreateStartedAt); ok {
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

// pendingCreateLeaseExpiredForRollbackInfo is the session.Info sibling of
// pendingCreateLeaseExpiredForRollback. Each sub-leaf it composes
// (pendingCreateStartInFlightInfo, pendingCreateNeverStartedExpiredInfo,
// pendingCreateAttemptStaleInfo) is already equivalence-proven; the state read
// uses the RAW Info.MetadataState to match the untrimmed-then-trimmed original.
func pendingCreateLeaseExpiredForRollbackInfo(i sessionpkg.Info, clk clock.Clock, startupTimeout time.Duration) bool {
	if !i.PendingCreateClaim {
		return false
	}
	state := sessionpkg.State(strings.TrimSpace(i.MetadataState))
	if !pendingCreateRollbackState(string(state)) {
		return false
	}
	if state == sessionpkg.StateAsleep {
		if strings.TrimSpace(i.LastWokeAt) == "" {
			return pendingCreateNeverStartedExpiredInfo(i, clk)
		}
		return pendingCreateAttemptStaleInfo(i, clk)
	}
	if pendingCreateStartInFlightInfo(i, clk, startupTimeout) {
		return false
	}
	if strings.TrimSpace(i.LastWokeAt) == "" {
		return pendingCreateNeverStartedExpiredInfo(i, clk)
	}
	return pendingCreateAttemptStaleInfo(i, clk)
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

// pendingResumePreservingNamedRestartInfo is the session.Info-typed sibling of
// pendingResumePreservingNamedRestart. It routes the asleep-named-session
// drift-repair skip decision through the typed projection: the start-pending/
// creating state gate, the pending-create claim, session_key, started_config_hash
// (the Info.StartedConfigHash mirror), and pending_create_started_at all read from
// Info, with the lease-active tail delegated to pendingCreateLeaseActiveInfo.
// Kept byte-identical to the raw form by TestSessionClassifierInfoEquivalence.
func pendingResumePreservingNamedRestartInfo(i sessionpkg.Info, clk clock.Clock, startupTimeout time.Duration) bool {
	switch sessionpkg.State(strings.TrimSpace(i.MetadataState)) {
	case sessionpkg.StateStartPending, sessionpkg.StateCreating:
	default:
		return false
	}
	if !i.PendingCreateClaim {
		return false
	}
	if strings.TrimSpace(i.SessionKey) == "" {
		return false
	}
	if strings.TrimSpace(i.StartedConfigHash) == "" {
		return false
	}
	if _, ok := parseRFC3339Metadata(i.PendingCreateStartedAt); !ok {
		return false
	}
	if !pendingCreateLeaseActiveInfo(i, clk, startupTimeout) {
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
	startOptions ...startExecutionOption,
) int {
	return reconcileSessionBeadsAtPath(
		ctx, "", sessions, desiredState, configuredNames, cfg, sp, store, dops, assignedWorkBeads, nil, readyWaitSet, dt,
		poolDesired, storeQueryPartial, workSet, cityName, it, clk, rec, startupTimeout, driftDrainTimeout, stdout, stderr,
		startOptions...,
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
	startOptions ...startExecutionOption,
) int {
	return reconcileSessionBeadsAtPathWithNamedDemand(
		ctx, cityPath, sessions, desiredState, configuredNames, cfg, sp, store, dops, assignedWorkBeads, rigStores, readyWaitSet, dt, nil,
		poolDesired, nil, storeQueryPartial, workSet, cityName, it, clk, rec, startupTimeout, driftDrainTimeout, stdout, stderr,
		startOptions...,
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
	startOptions ...startExecutionOption,
) int {
	return reconcileSessionBeadsTracedWithNamedDemand(
		ctx, cityPath, sessions, desiredState, configuredNames, cfg, sp, beads.SessionStore{Store: store}, dops, assignedWorkBeads, rigStores, readyWaitSet, dt, gate,
		poolDesired, namedSessionDemand, storeQueryPartial, workSet, cityName, it, clk, rec, startupTimeout, driftDrainTimeout, stdout, stderr, nil,
		startOptions...,
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
		ctx, cityPath, sessions, desiredState, configuredNames, cfg, sp, beads.SessionStore{Store: store}, dops, assignedWorkBeads, rigStores, readyWaitSet, dt, nil,
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
	sessStore beads.SessionStore,
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
	// The session class enters typed at the boundary; the generic beads.Store
	// helpers this function fans out to (heal, persist, drain-ack, worker probes)
	// take the unwrapped store. It is the same underlying store value, so
	// behavior is unchanged.
	store := sessStore.Store
	// The typed session front door is constructed once at this reconciler root
	// and threaded to the session-only leaves it calls (heal, drift deferral,
	// circuit metadata, rate-limit/wake-failure/churn accounting, lease clears,
	// drain-ack stop-pending). The raw store stays for the work/by-id/worker
	// residual. Same underlying store, so every session bead write is
	// byte-identical.
	sessFront := sessionpkg.NewStore(sessStore)
	// Every tick counts as a cycle, including ticks aborted by context
	// cancellation after real work (e.g. starts) already executed — the
	// counter means "cycles", not "cycles that ran to completion". started
	// counts the planned wakes the tick actually executed. Stops are applied
	// asynchronously (drain advance, drain-ack goroutines) and skips are
	// per-session trace decisions, so honest tick-boundary counts cannot
	// exist for them and the metric deliberately omits them (settled in
	// ga-ebb62d). The ctx param may legitimately be nil here, so the metric
	// uses context.Background().
	startedThisTick := 0
	defer func() {
		telemetry.RecordReconcileCycle(context.Background(), startedThisTick)
	}()
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
		effectiveStartOptions = append(append([]startExecutionOption(nil), startOptions...), withTaskWorkDirResolver(newAssignedTaskWorkDirResolver(cityPath, assignedWorkBeads)))
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
		healExpiredTimers(&sessions[i], sessFront, clk)
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
	var circuitIDByIdentity map[string]string
	if cbEnabled {
		// Phase 0.5: Feed the respawn circuit breaker persisted state and the
		// current progress signature for every named-session identity. A change
		// in the aggregate status of an identity's assigned work beads is treated
		// as an observable progress signal and keeps the breaker CLOSED even if
		// restarts accumulate. See session_circuit_breaker.go.
		cb = defaultSessionCircuitBreaker()
		cb.configure(cbCfg)
		circuitIDByIdentity = make(map[string]string, len(ordered))
		for i := range ordered {
			identity := namedSessionIdentity(ordered[i])
			if identity == "" {
				continue
			}
			circuitIDByIdentity[identity] = ordered[i].ID
			// Read the persisted breaker cluster through the typed CircuitState
			// front door instead of cracking ordered[i].Metadata inline. This runs
			// in Phase 0.5, before the reconciler's coherent infoByID snapshot
			// exists (and CircuitState is a distinct concern from Info anyway), so
			// it projects per bead — the same shape computeNamedSessionProgressSignatures
			// uses. The projection is pure, so it is byte-identical to the raw reads.
			if err := cb.observeResetGenerationFromMetadata(identity, sessionpkg.CircuitStateFromMetadata(ordered[i].Metadata)); err != nil {
				fmt.Fprintf(stderr, "session reconciler: loading session circuit breaker reset generation for %s: %v\n", identity, err) //nolint:errcheck // best-effort stderr
			}
		}
		for i := range ordered {
			identity := namedSessionIdentity(ordered[i])
			if identity == "" {
				continue
			}
			if reset, err := cb.restoreFromMetadata(identity, sessionpkg.CircuitStateFromMetadata(ordered[i].Metadata), cbNow); err != nil {
				fmt.Fprintf(stderr, "session reconciler: loading session circuit breaker state for %s: %v\n", identity, err) //nolint:errcheck // best-effort stderr
			} else if reset {
				if err := persistSessionCircuitBreakerMetadata(sessFront, ordered[i].ID, cb, identity, cbNow); err != nil {
					fmt.Fprintf(stderr, "session reconciler: %v\n", err) //nolint:errcheck // best-effort stderr
				}
			}
		}
		for identity, sig := range computeNamedSessionProgressSignatures(ordered, assignedWorkBeads) {
			if cb.ObserveProgressSignature(identity, sig, cbNow) {
				if id := circuitIDByIdentity[identity]; id != "" {
					if err := persistSessionCircuitBreakerMetadata(sessFront, id, cb, identity, cbNow); err != nil {
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

	// Coherent typed snapshot of the tick's working set, loaded once (front-door
	// migration Phase 5, Step 2). Reconciler decision reads route through this
	// instead of a per-iteration InfoFromPersistedBead(*session) re-derive: it is
	// the typed replacement for the raw session.Metadata[k]=v lockstep, which is
	// kept in lockstep with it until every dependent read has moved onto the
	// snapshot (Step 6). Built here from `ordered` (post-Phase-0.5), so each entry
	// is byte-identical to a fresh projection of that session's bead at loop entry
	// — Phase 1 mutates only the current iteration's session, so no entry goes
	// stale before it is visited. Entries are refreshed from the store (via Get)
	// after a mutation as the post-mutation reads migrate onto them (Step 3+).
	infoByID := make(map[string]sessionpkg.Info, len(ordered))
	// orderedIDs carries the tick's topo order as plain session IDs (Step 5e). The
	// order-sensitive decision-domain rebuilds (the awake-scan `sessionInfos` feed
	// and the preserve-template feed) walk it instead of the raw `ordered` beads,
	// so those rebuilds no longer reach into `ordered[i]` — `ordered` is demoted to
	// the load-time slice that builds this snapshot and carries raw beads into the
	// documented raw-by-design / start-execution consumers. Order is load-bearing:
	// ComputeAwakeSet resolves the non-unique SessionName last-write-wins, so these
	// rebuilds must stay in topo order and never `range infoByID`.
	orderedIDs := make([]string, len(ordered))
	for i := range ordered {
		orderedIDs[i] = ordered[i].ID
		infoByID[ordered[i].ID] = sessionpkg.InfoFromPersistedBead(ordered[i])
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
	// attemptRollbackPendingCreate returns the metadata batch the rollback mirrored
	// onto the raw bead (nil when the per-tick budget is exhausted, i.e. nothing was
	// rolled back), so each forward-pass caller can fold it onto the typed snapshot
	// (Step 6d write-returns-Info). The batch carries NO Closed change: the close is
	// store-only, so a raw re-projection of *session still sees it open — the fold
	// must match that.
	attemptRollbackPendingCreate := func(session *beads.Bead, templateName, name, action, detail string, clearClaim bool) map[string]string {
		if rollbacksThisTick >= maxRollbacksPerTick {
			fmt.Fprintf(stderr, "session reconciler: deferring rollback of %s (%s): rollback budget exhausted this tick\n", name, detail) //nolint:errcheck
			if trace != nil {
				trace.RecordDecision(TraceSiteReconcilerPendingCreate, TraceReasonCode(action), TraceOutcomeRollbackDeferred, templateName, name, traceRecordPayload{
					"rollbacks_this_tick":    rollbacksThisTick,
					"max_rollbacks_per_tick": maxRollbacksPerTick,
				})
			}
			return nil
		}
		rollbacksThisTick++
		fmt.Fprintf(stderr, "session reconciler: rolling back pending create %s: %s\n", name, detail) //nolint:errcheck
		if trace != nil {
			trace.RecordDecision(TraceSiteReconcilerPendingCreate, TraceReasonCode(action), TraceOutcomeRollback, templateName, name, nil)
		}
		if clearClaim {
			return rollbackPendingCreateClearingClaim(session, sessFront, clk.Now().UTC(), stderr)
		}
		return rollbackPendingCreate(session, sessFront, clk.Now().UTC(), stderr)
	}
	phaseStart = time.Now()
	for i := range ordered {
		if ctx != nil && ctx.Err() != nil {
			return 0
		}
		session := &ordered[i]
		// Typed projection for this iteration's mutation-free preamble decision
		// reads (session_name, reset-pending, known-state, and the unknown-state
		// trace), read from the coherent snapshot loaded above rather than a fresh
		// per-iteration re-derive. The snapshot entry equals InfoFromPersistedBead
		// (*session) at this point: it was built from `ordered` at loop entry and
		// Phase 1 mutates only the current session, so no earlier iteration could
		// have staled it. reconcileDrainAckStopPending below only mutates on its
		// true/continue paths, so when control falls through to the known-state
		// check the session is still unmutated and this projection stays
		// byte-identical. Reads after the first mutation (heal/rollback/close)
		// stay raw / re-derived for now — later clusters refresh the snapshot
		// entry after each mutation (Step 3+).
		info := infoByID[session.ID]
		name := strings.TrimSpace(info.SessionNameMetadata)
		tp, desired := desiredState[name]
		if _, _, pending := resetPendingCommittedAtInfo(info); !pending && dt != nil {
			dt.clearResetStall(session.ID)
		}
		// #3630: the session is in the desired set this tick, so its spec is
		// present — reset any suspend-drain confirmation window accrued during a
		// transient spec-enumeration collapse.
		if desired {
			dt.clearSuspendDeferral(session.ID)
		}

		if handled, result := reconcileDrainAckStopPending(cityPath, cfg, sp, store, rigStores, session, info, tp, desired, dops, dt, asyncStopTracker, clk, rec, stderr); handled {
			// finalizeDrainAckStoppedSession (inside reconcileDrainAckStopPending)
			// may close the bead in memory (Status=closed) on this true/continue
			// path; fold that close onto the snapshot so the cross-session min-floor
			// scan (openPoolSessionCountForTemplate, !Info.Closed) excludes a pool
			// session closed this tick. write-returns-Info (Step 6d) replaces the raw
			// refreshSessionInfo re-projection; the async-stop branch returns a zero
			// result so applyTo is a no-op there, matching the old refresh of the
			// unmutated bead. infoByID[session.ID] is coherent here (top-of-loop
			// snapshot, no *session mutation before the finalize call). Guarded by
			// TestReconcileSessionBeads_MinFloorCountReflectsMidTickCloseDrainAck.
			infoByID[session.ID] = result.applyTo(infoByID[session.ID])
			continue
		}

		// Skip beads with unrecognized states. This enables forward-compatible
		// rollback: if a newer version writes "draining" or "archived", the
		// older reconciler ignores those beads rather than crashing.
		if !isKnownStateInfo(info) {
			fmt.Fprintf(stderr, "session reconciler: skipping %s with unknown state %q\n", //nolint:errcheck // best-effort stderr
				info.SessionNameMetadata, info.MetadataState)
			if trace != nil {
				trace.RecordDecision(TraceSiteReconcilerUnknownState, TraceReasonUnknownStateSkipped, TraceOutcomeSkipped, info.Template, info.SessionNameMetadata, traceRecordPayload{
					"state": info.MetadataState,
				})
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
			//
			// The pure decision reads in this block route through the top-of-loop
			// `info`: this is the pre-heal region, and the only mutations reachable
			// here (checkRateLimitStability on its hit/err path, and
			// attemptRollbackPendingCreate) each `continue`, so control only falls
			// through to the next read on the unmutated bead — `info` stays
			// byte-identical. workerSessionTargetRunningWithConfig above reads by ID,
			// not the bead pointer, so it does not mutate either. The two mutations
			// keep the raw *session pointer they write through.
			if !storeQueryPartial && !providerAlive && shouldRollbackPendingCreateInfo(info) {
				var startupTimeout time.Duration
				if cfg != nil {
					startupTimeout = cfg.Session.StartupTimeoutDuration()
				}
				if pendingCreateLeaseExpiredForRollbackInfo(info, clk, startupTimeout) {
					template := normalizedSessionTemplateInfo(info, cfg)
					if template == "" {
						template = info.Template
					}
					peek := cachedSessionPeek(cityPath, store, sp, cfg, session.ID, nil)
					rateLimitHit, rlBatch, rateLimitErr := checkRateLimitStability(session, cfg, providerAlive, dt, sessFront, clk, peek)
					if rateLimitHit || rateLimitErr != nil {
						// Fold the rate-limit batch onto the snapshot (Step 6d write-returns-Info).
						// Pre-pass-masked (STEP6-PREPASS-AUDIT group 1).
						infoByID[session.ID] = infoByID[session.ID].ApplyPatch(rlBatch)
						continue
					}
					clearClaim := configuredNamedSessionBeadHasSpecInfo(info, cfg, cityName)
					// Fold the rollback's mirrored metadata onto the snapshot (Step 6d
					// write-returns-Info; no Closed change — store-only close).
					// Pre-pass-masked (STEP6-PREPASS-AUDIT group 2).
					infoByID[session.ID] = infoByID[session.ID].ApplyPatch(attemptRollbackPendingCreate(session, template, name, "pending_create_lease_expired", "lease expired and no live runtime", clearClaim))
					continue
				}
			}
			// Still pre-heal: the preserve-named + failed-create-close decision
			// reads below reuse the top-of-loop `info` with no re-derive. This read
			// runs on an unmutated bead (the rollback block above only mutates on
			// its `continue` paths). The one mutation reachable further down —
			// checkRateLimitStability (~1433) — writes only state/sleep/health/
			// quarantine keys, never template/agent_name/alias, so the template
			// trace read on its hit/err path stays byte-identical against `info`;
			// and the failed-create-close reads are reached only when it took its
			// non-mutating (false,nil) return (any mutation sets hit/err → continue).
			// The two trace-payload reads (pending_create_claim, state) read the typed
			// snapshot: Info.PendingCreateClaimMetadata (the verbatim raw-string mirror,
			// Step 6a) and Info.MetadataState (Step 5a).
			preserveNamed := preserveConfiguredNamedSessionBeadInfo(info, cfg, cityName)
			// #3630: the configured spec is present this tick — reset any
			// suspend-drain confirmation window so a later genuine removal still
			// gets the full confirmation buffer.
			if preserveNamed {
				dt.clearSuspendDeferral(session.ID)
			}
			var (
				preservedTP  TemplateParams
				preserveErr  error
				rateLimitHit bool
				rateLimitErr error
				rlBatchNamed map[string]string
			)
			if preserveNamed {
				// Feed the preserve template resolver from the live mid-tick
				// infoByID snapshot in topo (orderedIDs) order (front-door
				// Step 4/5e), not the raw `ordered` working set. Byte-identical
				// today (every pre-call close still writes raw Status in lockstep,
				// so membership matches) and forward-correct once that lockstep
				// drops. The only reachable snapshot read is OpenInfos().
				preservedInfos := make([]sessionpkg.Info, len(orderedIDs))
				for k := range orderedIDs {
					preservedInfos[k] = infoByID[orderedIDs[k]]
				}
				preservedTP, preserveErr = resolvePreservedConfiguredNamedSessionTemplate(cityPath, cityName, cfg, sp, store, preservedInfos, info, clk, stderr)
				if preserveErr == nil {
					obs, obsErr := workerObserveSessionTargetWithRuntimeHintsWithConfig(cityPath, store, sp, cfg, session.ID, preservedTP.Hints.ProcessNames)
					rateLimitAlive := rateLimitAliveFromObservation(obs.Alive, obsErr)
					peek := cachedSessionPeek(cityPath, store, sp, cfg, session.ID, preservedTP.Hints.ProcessNames)
					rateLimitHit, rlBatchNamed, rateLimitErr = checkRateLimitStability(session, cfg, rateLimitAlive, dt, sessFront, clk, peek)
				}
			}
			if rateLimitHit || rateLimitErr != nil {
				if trace != nil {
					template := normalizedSessionTemplateInfo(info, cfg)
					if template == "" {
						template = info.Template
					}
					result := "held"
					if rateLimitErr != nil {
						result = "hold_deferred"
					}
					trace.RecordDecision(TraceSiteReconcilerPreserveConfiguredNamed, TraceReasonRateLimit, TraceOutcomeCode(result), template, name, traceRecordPayload{
						"provider_alive": providerAlive,
					})
				}
				// Fold the rate-limit batch onto the snapshot (Step 6d write-returns-Info).
				// Pre-pass-masked (STEP6-PREPASS-AUDIT group 1).
				infoByID[session.ID] = infoByID[session.ID].ApplyPatch(rlBatchNamed)
				continue
			}
			if isFailedCreateSessionInfo(info) {
				template := normalizedSessionTemplateInfo(info, cfg)
				if template == "" {
					template = info.Template
				}
				if pendingCreateSessionStillLeasedInfo(info, cfg, clk) {
					if trace != nil {
						trace.RecordDecision(TraceSiteReconcilerPendingCreatePreserved, TraceReasonPendingCreate, TraceOutcomeKeptOpen, template, name, traceRecordPayload{
							"pending_create_claim": strings.TrimSpace(infoByID[session.ID].PendingCreateClaimMetadata),
							"provider_alive":       providerAlive,
							"state":                infoByID[session.ID].MetadataState,
						})
					}
					continue
				}
				if !providerAlive {
					if trace != nil {
						trace.RecordDecision(TraceSiteReconcilerCloseFailedCreate, TraceReasonCode(sessionpkg.StateFailedCreate), TraceOutcomeClosed, template, name, nil)
					}
					if storeQueryPartial || reconcileOpts.deferSessionClosesOnBoot {
						continue
					}
					if closeSessionBeadIfReachableStoreUnassigned(cityPath, cfg, store, rigStores, *session, string(sessionpkg.StateFailedCreate), clk.Now().UTC(), stderr) {
						session.Status = "closed"
						// Reflect the in-memory close on the snapshot: the cross-session
						// min-floor scan (below) reads Info.Closed off infoByID, so a
						// session closed this tick must not still count as open in its
						// pool. This is a store-only close — closeFailedCreateBead stamps
						// its ClosePatch on the store, not the raw bead — so the only
						// raw-bead change is Status="closed", and the snapshot refresh is
						// byte-identical to MarkClosed (Closed=true, State="") rather than
						// a raw re-projection. This is the write-returns-Info status-close
						// half of the Step-6d front-door cutover; the raw session.Status
						// lockstep above stays until the final lockstep drop. Guarded by
						// TestReconcileSessionBeads_MinFloorCountReflectsMidTickClose.
						infoByID[session.ID] = infoByID[session.ID].MarkClosed()
					}
					continue
				}
			}
			// Heal state using provider liveness, not agent membership.
			// rollbackAvailable mirrors the rollback gate at line ~639: when
			// storeQueryPartial=true the formal rollback is deferred, so the
			// heal path must also preserve pending_create_claim to avoid a
			// half-applied rollback that races the next complete tick.
			stateBeforeHeal := strings.TrimSpace(infoByID[session.ID].MetadataState)
			pendingCreateStartedAtBeforeHeal := strings.TrimSpace(infoByID[session.ID].PendingCreateStartedAt)
			lastWokeAtBeforeHeal := strings.TrimSpace(infoByID[session.ID].LastWokeAt)
			healBatch := healStateWithRollback(session, providerAlive, sessFront, clk, startupTimeout, !storeQueryPartial)
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
			// Post-heal refresh: healStateWithRollback (above) persists through
			// sessFront and mirrors healBatch onto session.Metadata in lockstep, so
			// the top-of-loop `info` (from the snapshot at loop entry) is now stale
			// for this switch. Fold that same healBatch onto the snapshot via
			// write-returns-Info (Step 6d) instead of re-projecting the raw bead:
			// healStateWithRollback returns exactly the batch it mirrored (even on a
			// persist error the mirror runs, so the returned batch always matches the
			// bead), and nil when it healed nothing (ApplyPatch(nil) is a no-op). This
			// is byte-identical to the raw refresh because infoByID[session.ID] is
			// coherent here: the top-of-loop snapshot entry, unmutated on the path
			// that reaches the heal (the pre-heal checkRateLimitStability/rollback/
			// failed-create-close sites all `continue`). The trace call above takes
			// the bead by value (cannot mutate), and Go switch cases do not fall
			// through, so both the preserveNamed body and the
			// pendingCreateSessionStillLeased guard/body below read the same
			// post-heal snapshot. This fold is LOAD-BEARING (and newly so in this
			// commit): the pendingCreateSessionStillLeasedInfo guard below reads the
			// healed MetadataState off infoPostHeal, and the downstream zombie refresh
			// is now ApplyPatch(terminalErrBatch) — a no-op when there is no terminal
			// error — rather than the old raw re-projection that would have repaired a
			// stale heal snapshot, so the healed state must reach that guard (and the
			// post-zombie rollback read on the preserveNamed fall-through) through this
			// fold alone. Guarded by
			// TestReconcileSessionBeads_HealStateReflectedOnSnapshot.
			infoByID[session.ID] = infoByID[session.ID].ApplyPatch(healBatch)
			infoPostHeal := infoByID[session.ID]
			switch {
			case preserveNamed:
				template := normalizedSessionTemplateInfo(infoPostHeal, cfg)
				if template == "" {
					template = infoPostHeal.Template
				}
				switch {
				case preserveErr != nil:
					fmt.Fprintf(stderr, "session reconciler: resolve preserved named session %s: %v\n", name, preserveErr) //nolint:errcheck
				default:
					tp = preservedTP
					desired = true
				}
				if trace != nil {
					trace.RecordDecision(TraceSiteReconcilerPreserveConfiguredNamed, TraceReasonPreserve, TraceOutcomeCode(map[bool]string{
						true:  "kept_open",
						false: "resolution_failed",
					}[desired]), template, name, traceRecordPayload{
						"provider_alive": providerAlive,
						"degraded":       preserveErr != nil,
					})
				}
			case pendingCreateSessionStillLeasedInfo(infoPostHeal, cfg, clk):
				template := normalizedSessionTemplateInfo(infoPostHeal, cfg)
				if template == "" {
					template = infoPostHeal.Template
				}
				if trace != nil {
					trace.RecordDecision(TraceSiteReconcilerPendingCreatePreserved, TraceReasonPendingCreate, TraceOutcomeKeptOpen, template, name, traceRecordPayload{
						"pending_create_claim": strings.TrimSpace(infoByID[session.ID].PendingCreateClaimMetadata),
						"provider_alive":       providerAlive,
						"state":                infoByID[session.ID].MetadataState,
					})
				}
				continue
			default:
				if dops != nil {
					if acked, _ := dops.isDrainAcked(name); acked {
						// gc-hz0nu: every drain-acked decision below depends on the
						// store-derived desired-state / assigned-work view. During a
						// partial store query (transient Dolt failure) that view is
						// incomplete, so an ack minted from it cannot be trusted to
						// mean "orphaned". Defer the whole decision until the store is
						// healthy — the same protection the plain drain path applies
						// just below. Stopping a live session here on degraded data is
						// what killed coordinator sessions on 2026-06-09.
						if storeQueryPartial {
							fmt.Fprintf(stdout, "Skipping drain-ack stop for '%s': store query partial (transient failure)\n", name) //nolint:errcheck
							if trace != nil {
								template := normalizedSessionTemplateInfo(infoPostHeal, cfg)
								if template == "" {
									template = infoPostHeal.Template
								}
								trace.RecordDecision(TraceSiteReconcilerDrainAck, TraceReasonStoreQueryPartial, TraceOutcomeDeferred, template, name, traceRecordPayload{
									"store_query_partial": true,
									"provider_alive":      providerAlive,
								})
							}
							continue
						}
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
								template := normalizedSessionTemplateInfo(infoPostHeal, cfg)
								if template == "" {
									template = infoPostHeal.Template
								}
								fmt.Fprintf(stdout, "Canceled drain-acked session '%s' (assigned work)\n", name) //nolint:errcheck
								if trace != nil {
									trace.RecordDecision(TraceSiteDrainCancel, TraceReasonCode(ackReason), TraceOutcomeCancelAssignedWork, template, name, nil)
								}
								continue
							}
						}
						if providerAlive {
							template := normalizedSessionTemplateInfo(infoPostHeal, cfg)
							if template == "" {
								template = infoPostHeal.Template
							}
							if markDrainAckStopPending(infoByID[session.ID], sessFront, clk, stderr) {
								// Fold the stop-pending transition onto the snapshot (Step 6d):
								// markDrainAckStopPending mirrors DrainAckStopPendingPatch only on
								// this true return; its Info keys (state=draining,
								// state_reason=drain-ack-stop-pending, cleared pending_create_*) are
								// time-independent, so reconstructing the patch reproduces the
								// mirror (drain_at is non-Info). Cross-session isDrainAckStopPendingInfo
								// reader. Pre-pass-masked (STEP6-PREPASS-AUDIT group 3).
								infoByID[session.ID] = infoByID[session.ID].ApplyPatch(sessionpkg.DrainAckStopPendingPatch(clk.Now().UTC()))
								clearDrainTrackerForStopPending(session, dt)
								queueDrainAckAsyncStop(cityPath, store, sp, cfg, session.ID, name, asyncStopTracker, stderr)
								if trace != nil {
									trace.RecordDecision(TraceSiteReconcilerDrainAck, TraceReasonOrphaned, TraceOutcomeStopPending, template, name, nil)
								}
							}
							continue
						}
						template := normalizedSessionTemplateInfo(infoPostHeal, cfg)
						if template == "" {
							template = infoPostHeal.Template
						}
						result := finalizeDrainAckStoppedSession(
							cityPath, cfg, store, rigStores, session, infoByID[session.ID], template,
							true, dops, dt, clk, rec, stderr,
						)
						// finalizeDrainAckStoppedSession may close the bead in memory; fold
						// that close onto the snapshot so the cross-session min-floor scan
						// stays coherent (write-returns-Info, Step 6d, replacing the raw
						// refreshSessionInfo re-projection). infoByID[session.ID] holds the
						// coherent post-heal Info (refreshed at the heal above; no *session
						// mutation reaches here on this !providerAlive path).
						infoByID[session.ID] = result.applyTo(infoByID[session.ID])
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
							template := normalizedSessionTemplateInfo(infoPostHeal, cfg)
							if template == "" {
								template = infoPostHeal.Template
							}
							trace.RecordDecision(TraceSiteReconcilerOrphaned, TraceReasonCode(reason), TraceOutcomeKeptOpen, template, name, traceRecordPayload{
								"store_query_partial": storeQueryPartial,
								"provider_alive":      providerAlive,
								"live_assigned_work":  true,
							})
						}
						fmt.Fprintf(stdout, "Skipping drain for '%s': live assigned work found\n", name) //nolint:errcheck
						continue
					}
					// #3630: a LIVE named session reaches this drain only because
					// its configured spec is absent this tick (preserve did not fire
					// above) and it has no live assigned work. A namedSessionSpecs
					// enumeration collapse during boot can drop a spec for a single
					// tick and restore it on the next; draining the live runtime
					// respawns it fresh and loses in-session context. Suspend-class
					// drains are revertible, so require namedSuspendConfirmTicks
					// consecutive confirming ticks before draining. The counter is
					// cleared above once the spec reappears. Scoped to live sessions:
					// a dead bead with no spec still releases its alias immediately
					// (ga-ue1r).
					if isNamedSessionInfo(infoPostHeal) {
						if n := dt.bumpSuspendDeferral(session.ID); n < namedSuspendConfirmTicks {
							if trace != nil {
								template := normalizedSessionTemplateInfo(infoPostHeal, cfg)
								if template == "" {
									template = infoPostHeal.Template
								}
								trace.RecordDecision(TraceSiteReconcilerOrphaned, TraceReasonCode(reason), TraceOutcomeDeferredConfirm, template, name, traceRecordPayload{
									"confirm_ticks":    n,
									"confirm_required": namedSuspendConfirmTicks,
									"provider_alive":   providerAlive,
								})
							}
							fmt.Fprintf(stdout, "Deferring drain for named session '%s': awaiting spec-absence confirmation (%d/%d) — transient enumeration-collapse guard (#3630)\n", name, n, namedSuspendConfirmTicks) //nolint:errcheck
							continue
						}
					}
					if beginSessionDrain(*session, sp, dt, reason, clk, defaultDrainTimeout) {
						if trace != nil {
							template := normalizedSessionTemplateInfo(infoPostHeal, cfg)
							if template == "" {
								template = infoPostHeal.Template
							}
							trace.RecordDecision(TraceSiteReconcilerOrphaned, TraceReasonCode(reason), TraceOutcomeDrain, template, name, traceRecordPayload{
								"store_query_partial": storeQueryPartial,
								"provider_alive":      providerAlive,
							})
						}
						fmt.Fprintf(stdout, "Draining session '%s': %s\n", name, reason) //nolint:errcheck
					}
				} else {
					// Not running and not desired — close the bead.
					reason := "orphaned"
					if configuredNames[name] {
						reason = "suspended"
					}
					template := normalizedSessionTemplateInfo(infoPostHeal, cfg)
					if template == "" {
						template = infoPostHeal.Template
					}
					if trace != nil {
						trace.RecordDecision(TraceSiteReconcilerCloseOrphan, TraceReasonCode(reason), TraceOutcomeClosed, template, name, nil)
					}
					if storeQueryPartial || reconcileOpts.deferSessionClosesOnBoot {
						continue
					}
					if closeSessionBeadIfReachableStoreUnassigned(cityPath, cfg, store, rigStores, *session, reason, clk.Now().UTC(), stderr) {
						session.Status = "closed"
						// Keep the snapshot's Info.Closed in step with the in-memory
						// close so the cross-session min-floor scan does not count this
						// orphan. Store-only close (same helper family as the
						// failed-create site above: closeBead/closeFailedCreateBead stamp
						// the ClosePatch on the store, not the raw bead), so the only
						// raw-bead change is Status="closed" and the byte-identical
						// snapshot refresh is MarkClosed (Closed=true, State="") — the
						// write-returns-Info status-close half of the Step-6d cutover.
						// The heal refresh (~1628) already synced this entry, so
						// MarkClosed folds onto a coherent pre-close Info. Guarded by
						// TestReconcileSessionBeads_MinFloorCountReflectsMidTickCloseOrphan.
						infoByID[session.ID] = infoByID[session.ID].MarkClosed()
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
		// terminalErrBatch carries the markProviderTerminalError mirror (if it ran) out
		// to the snapshot refresh below; nil when nothing was written.
		var terminalErrBatch map[string]string
		if running && !alive {
			if output, err := peek(rateLimitPeekLines); err == nil && output != "" {
				if reason := runtime.ProviderTerminalErrorReason(output); reason != "" {
					markBatch, markErr := markProviderTerminalError(session, sessFront, clk, reason)
					if markErr != nil {
						fmt.Fprintf(stderr, "session reconciler: marking terminal provider error for %s: %v\n", name, markErr) //nolint:errcheck
					}
					terminalErrBatch = markBatch
					if trace != nil {
						trace.RecordDecision(TraceSiteReconcilerTerminalProviderError, TraceReasonCode(reason), TraceOutcomeUnhealthy, tp.TemplateName, name, traceRecordPayload{
							"session_bead_id": session.ID,
						})
					}
				}
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
		// Refresh the snapshot after the zombie-capture block by folding the
		// markProviderTerminalError batch onto it via write-returns-Info (Step 6d),
		// instead of re-projecting the raw bead. markProviderTerminalError mirrors
		// terminalErrBatch onto session.Metadata in lockstep and returns exactly that
		// batch (nil when it wrote nothing — not a zombie, empty reason, or a persist
		// error), so ApplyPatch(terminalErrBatch) reproduces the raw refresh: nil ⇒
		// no-op. This is byte-identical because infoByID[session.ID] is coherent here
		// — terminalErrBatch is the only session.Metadata mutation on the paths that
		// reach this point. Only two path shapes arrive: the desired fast path (skips
		// the `if !desired` block and mutates nothing but the drain tracker via
		// recordResetStallIfDue, which takes the bead by value), and the ONE
		// non-continue arm of that block — the post-heal `case preserveNamed:` — whose
		// body sets local tp/desired and records a trace only, and which was
		// heal-folded just above (~1713). (Every drain/drain-ack/orphan-close arm of
		// the switch `continue`s, so no drained bead reaches this fold.) The
		// alive-gated read just below never sees a
		// markProviderTerminalError mutation (that runs only under `running && !alive`,
		// mutually exclusive with `alive`); the !alive rollback reads below run on the
		// folded snapshot, and the further mutations between them sit on `continue`
		// paths (attemptRollbackPendingCreate; checkRateLimitStability on hit), so
		// infoPostZombie stays byte-identical throughout. Guarded by
		// TestReconcileSessionBeads_ZombieTerminalErrorReflectedOnSnapshot.
		infoByID[session.ID] = infoByID[session.ID].ApplyPatch(terminalErrBatch)
		infoPostZombie := infoByID[session.ID]
		if alive && shouldRollbackPendingCreateInfo(infoPostZombie) && !runningSessionMatchesPendingCreate(session, name, sp) {
			// Fold the rollback's mirrored metadata onto the snapshot (Step 6d;
			// no Closed change — store-only close). STEP6-PREPASS-AUDIT group 2.
			infoByID[session.ID] = infoByID[session.ID].ApplyPatch(attemptRollbackPendingCreate(session, tp.TemplateName, name, "pending_create_rollback", "live runtime belongs to another session", false))
			continue
		}
		// Desired-branch counterpart to pendingCreateSessionStillLeased: a
		// session bead in the desired set with pending_create_claim=true but
		// no live runtime AND no active lease is stuck. Without this rollback,
		// the bead lives forever holding its alias, blocking new spawn
		// attempts ("alias already belongs to gm-XXXX") for any session whose
		// template still has demand. Rolling back closes the dead bead so the
		// next reconciler tick can allocate a fresh slot under the same alias.
		if !alive && shouldRollbackPendingCreateInfo(infoPostZombie) {
			var startupTimeout time.Duration
			if cfg != nil {
				startupTimeout = cfg.Session.StartupTimeoutDuration()
			}
			if pendingCreateLeaseExpiredForRollbackInfo(infoPostZombie, clk, startupTimeout) {
				rateLimitHit, rlBatch, rateLimitErr := checkRateLimitStability(session, cfg, alive, dt, sessFront, clk, peek)
				if rateLimitHit || rateLimitErr != nil {
					// Fold the rate-limit batch onto the snapshot (Step 6d write-returns-Info).
					// Pre-pass-masked (STEP6-PREPASS-AUDIT group 1).
					infoByID[session.ID] = infoByID[session.ID].ApplyPatch(rlBatch)
					continue
				}
				// Fold the rollback's mirrored metadata onto the snapshot (Step 6d;
				// no Closed change — store-only close). STEP6-PREPASS-AUDIT group 2.
				infoByID[session.ID] = infoByID[session.ID].ApplyPatch(attemptRollbackPendingCreate(session, tp.TemplateName, name, "pending_create_lease_expired", "lease expired and no live runtime", false))
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
							trace.RecordDecision(TraceSiteReconcilerDrainAck, TraceReasonStaleGeneration, TraceOutcomeClear, tp.TemplateName, name, nil)
						}
						continue
					}
					ackReason, reconcilerOwnedAck := reconcilerDrainAckMatchesSession(*session, sp, name)
					// gc-kkgak: a reconciler-owned drain ack is minted from the
					// desired-state / assigned-work view. During a partial store
					// query that view is unreliable, so defer the reconciler-owned
					// cancel/stop decision until the store is healthy — same
					// rationale as gc-hz0nu's orphan branch. Agent-sourced handoff
					// acks are not reconciler-owned and fall through to stop
					// promptly: their intent is explicit, not derived from the store.
					if reconcilerOwnedAck && storeQueryPartial {
						fmt.Fprintf(stdout, "Skipping reconciler drain-ack stop for '%s': store query partial (transient failure)\n", name) //nolint:errcheck
						if trace != nil {
							trace.RecordDecision(TraceSiteReconcilerDrainAck, TraceReasonStoreQueryPartial, TraceOutcomeDeferred, tp.TemplateName, name, traceRecordPayload{
								"store_query_partial":  true,
								"reconciler_owned_ack": true,
							})
						}
						continue
					}
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
								trace.RecordDecision(TraceSiteDrainCancel, TraceReasonCode(ackReason), TraceOutcomeCancelAssignedWork, tp.TemplateName, name, nil)
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
								trace.RecordDecision(TraceSiteReconcilerDrainAck, TraceReasonConfigDriftAttachmentError, TraceOutcomeCancelReconcilerAck, tp.TemplateName, name, traceRecordPayload{
									"drain_canceled": drainCancelled,
									"error":          attachErr.Error(),
								})
							}
							continue
						}
						if attached {
							if driftKey != "" {
								if err := recordSessionAttachedConfigDriftDeferral(*session, sessFront, clk, driftKey); err != nil {
									fmt.Fprintf(stderr, "session reconciler: recording attached config-drift deferral for %s: %v\n", name, err) //nolint:errcheck
								}
							}
							drainCancelled := cancelSessionConfigDriftDrain(*session, sp, dt)
							if !drainCancelled {
								_ = clearReconcilerDrainAckMetadata(sp, name)
							}
							if trace != nil {
								trace.RecordDecision(TraceSiteReconcilerDrainAck, TraceReasonConfigDriftAttached, TraceOutcomeCancelReconcilerAck, tp.TemplateName, name, traceRecordPayload{
									"drain_canceled": drainCancelled,
								})
							}
							continue
						}
						if driftKey != "" && recentlyDeferredSessionAttachedConfigDrift(*session, clk, driftKey) {
							drainCancelled := cancelSessionConfigDriftDrain(*session, sp, dt)
							if !drainCancelled {
								_ = clearReconcilerDrainAckMetadata(sp, name)
							}
							if trace != nil {
								trace.RecordDecision(TraceSiteReconcilerDrainAck, TraceReasonConfigDriftRecentlyAttached, TraceOutcomeCancelReconcilerAck, tp.TemplateName, name, traceRecordPayload{
									"drain_canceled": drainCancelled,
								})
							}
							continue
						}
					}
					if pendingInteractionKeepsAwake(*session, sp, name, clk) &&
						(cancelReconcilerAckedDrain(*session, sp, dt) || cancelRecoveredReconcilerAckedDrain(*session, sp, name)) {
						if trace != nil {
							trace.RecordDecision(TraceSiteReconcilerDrainAck, TraceReasonPending, TraceOutcomeCancelReconcilerAck, tp.TemplateName, name, nil)
						}
						continue
					}
					if alive {
						if markDrainAckStopPending(infoByID[session.ID], sessFront, clk, stderr) {
							// Fold the stop-pending transition onto the snapshot (Step 6d);
							// deterministic DrainAckStopPendingPatch reconstruction, same as the
							// orphan-arm site above (STEP6-PREPASS-AUDIT group 3).
							infoByID[session.ID] = infoByID[session.ID].ApplyPatch(sessionpkg.DrainAckStopPendingPatch(clk.Now().UTC()))
							clearDrainTrackerForStopPending(session, dt)
							queueDrainAckAsyncStop(cityPath, store, sp, cfg, session.ID, name, asyncStopTracker, stderr)
							if trace != nil {
								trace.RecordDecision(TraceSiteReconcilerDrainAck, TraceReasonAcknowledged, TraceOutcomeStopPending, tp.TemplateName, name, nil)
							}
						}
						continue
					}
					finalizeDT := dt
					if reconcilerOwnedAck {
						finalizeDT = nil
					}
					result := finalizeDrainAckStoppedSession(
						cityPath, cfg, store, rigStores, session, infoByID[session.ID], tp.TemplateName,
						isPoolManagedSessionBead(*session),
						dops, finalizeDT,
						clk, rec, stderr,
					)
					// finalizeDrainAckStoppedSession may close the bead in memory; fold
					// that close onto the snapshot so the cross-session min-floor scan
					// stays coherent (write-returns-Info, Step 6d, replacing the raw
					// refreshSessionInfo re-projection). infoByID[session.ID] holds the
					// coherent post-zombie Info (refreshed above; no *session mutation
					// reaches here on this !alive fall-through path).
					infoByID[session.ID] = result.applyTo(infoByID[session.ID])
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
							openInPool := openPoolSessionCountForTemplate(infoByID, cfg, tp.TemplateName)
							if isMinFloorIdleWorker(minFloor, openInPool) {
								exempt = true
								if trace != nil {
									trace.RecordDecision(TraceSiteReconcilerProgressStallExempt, TraceReasonMinFloorIdleWorker, TraceOutcomeExempt, tp.TemplateName, name, traceRecordPayload{
										"pool_min":  minFloor,
										"pool_open": openInPool,
									})
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
					// Record the restart request on the typed snapshot only. This
					// marker is decision-state consumed by the restart-request block
					// below (which reads Info.RestartRequested off infoByID) and never
					// read off the raw session bead — not by the start-execution path —
					// so Step 5c dropped its raw session.Metadata mirror. The consume
					// clears it on the snapshot (else #2574 re-fires a phantom second
					// restart). The base is coherent here (the zombie fold synced
					// infoByID and every intervening mutating block `continue`s).
					infoByID[session.ID] = infoByID[session.ID].ApplyPatch(sessionpkg.MetadataPatch{"restart_requested": "true"})
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
			beadRequested := infoByID[session.ID].RestartRequested == "true"
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
				if err := sessionFrontDoor(store).ApplyPatch(session.ID, batch); err != nil {
					fmt.Fprintf(stderr, "session reconciler: recording restart handoff for %s: %v\n", name, err) //nolint:errcheck
					continue
				}
				if session.Metadata == nil {
					session.Metadata = make(map[string]string, len(batch))
				}
				// Fold the mirrored batch onto the snapshot too (Step 6d
				// write-returns-Info), so the restart handoff — which CONSUMES the
				// in-memory restart_requested marker (RestartRequestPatch sets it to "")
				// and clears started_config_hash / last_woke_at / pending_create_* —
				// clears the marker (and its siblings) on the snapshot the awake scan
				// reads. Without this, once the blanket pre-pass is dropped a consumed
				// restart_requested would survive on the snapshot and re-fire as a
				// phantom second restart (#2574). Excludes ResetCommittedAtKey exactly
				// like the in-memory mirror above: the durable reset marker is for the
				// next tick, and admitting it here would force-wake on-demand sessions
				// without demand (#2345).
				//
				// START-EXECUTION COUPLING (Step 5c): the raw session.Metadata mirror
				// is RETAINED. On the runtime-already-dead fall-through below this
				// session can reach startCandidates this same tick, and the start
				// executor reads last_woke_at (cleared by RestartRequestPatch) off the
				// raw bead via wakeFairnessTime BEFORE it re-Gets the bead from the
				// store — dropping the mirror would perturb the wake-fairness ordering.
				restartFold := make(sessionpkg.MetadataPatch, len(batch))
				for key, value := range batch {
					if key == sessionpkg.ResetCommittedAtKey {
						continue
					}
					session.Metadata[key] = value
					restartFold[key] = value
				}
				infoByID[session.ID] = infoByID[session.ID].ApplyPatch(restartFold)
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

		rateLimitHit, rlBatchFwd, rateLimitErr := checkRateLimitStability(session, cfg, alive, dt, sessFront, clk, peek)
		if rateLimitHit || rateLimitErr != nil {
			// Fold the rate-limit batch onto the snapshot (Step 6d write-returns-Info).
			// Pre-pass-masked (STEP6-PREPASS-AUDIT group 1).
			infoByID[session.ID] = infoByID[session.ID].ApplyPatch(rlBatchFwd)
			continue // rate-limit hold recorded before state healing resets continuity metadata
		}

		// Heal advisory state metadata.
		stateBeforeHeal := sessionpkg.State(strings.TrimSpace(infoByID[session.ID].MetadataState))
		pendingCreateStartedAtBeforeHeal := strings.TrimSpace(infoByID[session.ID].PendingCreateStartedAt)
		lastWokeAtBeforeHeal := strings.TrimSpace(infoByID[session.ID].LastWokeAt)
		healBatch := healStateWithRollback(session, alive, sessFront, clk, startupTimeout, true)
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
		// Fold heal#2's batch onto the snapshot (Step 6d write-returns-Info),
		// identical to the heal#1 fold above (~1713): healStateWithRollback returns
		// exactly the batch it mirrored (nil ⇒ ApplyPatch no-op). The base is
		// coherent here — the pre-heal rate-limit gate `continue`s on hit and the
		// restart/drain-ack blocks above either `continue` or self-refresh. This is
		// one of the forward-pass writers the blanket pre-pass still masks; folding it
		// is a prerequisite for that pre-pass's deletion (STEP6-PREPASS-AUDIT group 4).
		infoByID[session.ID] = infoByID[session.ID].ApplyPatch(healBatch)
		if recoverPendingIdleSleep(session, sessFront, running, clk) {
			alive = false
			// Fold the idle-stop-pending recovery sleep onto the snapshot (Step 6d).
			// recoverPendingIdleSleep mirrors SleepPatch(now,"idle") only on this true
			// return; its Info-projected keys are time-independent, so reconstructing
			// the same SleepPatch reproduces the mirror exactly (slept_at /
			// sleep_policy_fingerprint are non-Info). Pre-pass-masked (STEP6-PREPASS-AUDIT
			// group 6).
			infoByID[session.ID] = infoByID[session.ID].ApplyPatch(sessionpkg.SleepPatch(clk.Now().UTC(), "idle"))
		}
		// Fold detached_at change onto the snapshot (Step 6d write-returns-Info).
		// reconcileDetachedAt returns the {"detached_at": <value>} batch it mirrored,
		// or nil on no-op. Pre-pass-masked (STEP6-PREPASS-AUDIT group 6).
		infoByID[session.ID] = infoByID[session.ID].ApplyPatch(reconcileDetachedAt(session, store, policy, alive, sp, clk))

		// Stability check: detect rapid crash after state healing. Rate-limit
		// detection intentionally ran above before healState.
		// Fold the returned batch onto the snapshot (Step 6d write-returns-Info);
		// nil (no-op) when no stability event was recorded.
		// Pre-pass-masked (STEP6-PREPASS-AUDIT group 2).
		if stab, stabBatch := checkStability(session, cfg, alive, dt, sessFront, clk, nil); stab {
			infoByID[session.ID] = infoByID[session.ID].ApplyPatch(stabBatch)
			continue // rapid exit recorded, skip further processing
		}

		// Churn check: detect context exhaustion death spiral.
		// Fires for sessions that survived past stabilityThreshold but
		// died before churnProductivityThreshold — alive long enough to
		// not be a rapid crash, but too short to be productive.
		// Fold the returned batch onto the snapshot (Step 6d write-returns-Info)
		// regardless of the bool — ExitProductiveDeath may clear churn_count.
		// Pre-pass-masked (STEP6-PREPASS-AUDIT group 5).
		churn, churnBatch := checkChurn(session, cfg, alive, dt, sessFront, clk)
		infoByID[session.ID] = infoByID[session.ID].ApplyPatch(churnBatch)
		if churn {
			continue // churn recorded, skip further processing
		}

		// Clear wake failures for sessions that have been stable long enough.
		// Fold the returned batch onto the snapshot (Step 6d write-returns-Info);
		// nil (no-op) when nothing was cleared. Pre-pass-masked (STEP6-PREPASS-AUDIT group 5).
		if alive && stableLongEnough(*session, clk) {
			infoByID[session.ID] = infoByID[session.ID].ApplyPatch(clearWakeFailures(session, sessFront))
		}
		// Clear churn counter for sessions that have been productive.
		// Fold the returned batch onto the snapshot (Step 6d write-returns-Info);
		// nil (no-op) when churn_count was already absent/zero. Pre-pass-masked (STEP6-PREPASS-AUDIT group 5).
		if alive && productiveLongEnough(*session, clk) {
			infoByID[session.ID] = infoByID[session.ID].ApplyPatch(clearChurn(session, sessFront))
		}
		if alive && shouldRollbackPendingCreate(session) {
			switch stateBeforeHeal {
			case sessionpkg.StateStartPending, sessionpkg.StateCreating:
				if pendingCreateStartInFlight(*session, clk, startupTimeout) {
					if trace != nil {
						trace.RecordDecision(TraceSiteReconcilerPendingCreate, TraceReasonPendingCreateRecoveryInFlight, TraceOutcomeDeferred, tp.TemplateName, name, nil)
					}
					continue
				}
			}
			// Fold recoverRunningPendingCreate's batch onto the snapshot (Step 6d
			// write-returns-Info). The batch carries CommitStartedPatch PLUS
			// buildPreparedStart's persisted residue (threaded out in
			// pendingCreateResidueFold, on the abort paths): the instance_token mint,
			// read by the Phase-2 drain scan (info.InstanceToken via verifiedStop,
			// Step 2b), and the stale-resume started_config_hash clear, read by the
			// forward-pass config-drift gate below (info.StartedConfigHash, Step 5a,
			// #127). STEP6-PREPASS-AUDIT group 7. The other two clearStaleResumeKeyMetadata
			// keys (session_key/continuation_reset_pending) stay unthreaded — neither has
			// a same-tick Info reader whose verdict the residue changes — and self-heal on
			// the next tick's store reload.
			ok, commitBatch := recoverRunningPendingCreate(session, tp, cfg, store, clk, trace)
			if !ok {
				fmt.Fprintf(stderr, "session reconciler: recovering pending create %s: metadata repair incomplete\n", name) //nolint:errcheck
			}
			infoByID[session.ID] = infoByID[session.ID].ApplyPatch(commitBatch)
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
			storedHash := infoByID[session.ID].StartedConfigHash
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
							// Fold the rebaseline patch onto the snapshot (Step 6d write-returns-Info).
							// This site `continue`s, so the fold must run before the continue.
							// Pre-pass-masked (STEP6-PREPASS-AUDIT group 8).
							rebaseBatch, rebaseErr := silentRebaselineSessionHashes(session, sessFront, agentCfg)
							if rebaseErr != nil {
								fmt.Fprintf(stderr, "session reconciler: rebaselining legacy hash for %s: %v\n", name, rebaseErr) //nolint:errcheck
							} else {
								fmt.Fprintf(stderr, "rebaselined legacy hash for %s (stored=%s current=%s)\n", name, truncateHashForLog(storedHash), truncateHashForLog(currentHash)) //nolint:errcheck
							}
							infoByID[session.ID] = infoByID[session.ID].ApplyPatch(rebaseBatch)
							if trace != nil {
								trace.RecordDecision(TraceSiteReconcilerConfigDrift, TraceReasonConfigDrift, outcome, tp.TemplateName, name, traceRecordPayload{
									"stored_hash":  storedHash,
									"current_hash": currentHash,
								})
							}
							continue
						}
						fmt.Fprintf(stderr, "config-drift %s: stored=%s current=%s cmd=%q\n", name, truncateHashForLog(storedHash), truncateHashForLog(currentHash), agentCfg.Command) //nolint:errcheck
						// Diagnostic: log per-field breakdown to identify the drifting field.
						driftedFields := runtime.CoreFingerprintDriftFieldsFromJSON(infoByID[session.ID].CoreHashBreakdown, agentCfg)
						runtime.LogCoreFingerprintDrift(stderr, name, infoByID[session.ID].CoreHashBreakdown, agentCfg)
						// Launch-only drift (B2.3): the box (provision half) is
						// unchanged but the agent (launch half) moved. When the
						// provider can relaunch the agent in the existing warm box,
						// the named/ordinary branches below relaunch instead of a
						// full re-provision restart — but only AFTER the same
						// attached/active/pending/open-work deferral guards, because
						// a respawn is just as disruptive mid-turn. Empty sub-hashes
						// (a session started before B2.2) are treated as "not
						// launch-only" → full restart, which re-stamps the sub-hashes
						// and self-heals.
						storedProvision := infoByID[session.ID].StartedProvisionHash
						storedLaunch := infoByID[session.ID].StartedLaunchHash
						launchOnlyDrift := storedProvision != "" && storedLaunch != "" &&
							storedProvision == runtime.ProvisionFingerprint(agentCfg) &&
							storedLaunch != runtime.LaunchFingerprint(agentCfg)
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
							if err := recordSessionAttachedConfigDriftDeferral(*session, sessFront, clk, driftKey); err != nil {
								fmt.Fprintf(stderr, "session reconciler: recording attached config-drift deferral for %s: %v\n", name, err) //nolint:errcheck
							}
							drainCancelled := cancelSessionConfigDriftDrain(*session, sp, dt)
							if trace != nil {
								trace.RecordDecision(TraceSiteReconcilerConfigDrift, TraceReasonConfigDrift, TraceOutcomeDeferredAttached, tp.TemplateName, name, configDriftTracePayload(storedHash, currentHash, driftedFields, traceRecordPayload{
									"active_reason":  "attached",
									"drain_canceled": drainCancelled,
								}))
							}
							continue
						}
						if recentlyDeferredSessionAttachedConfigDrift(*session, clk, driftKey) {
							if trace != nil {
								trace.RecordDecision(TraceSiteReconcilerConfigDrift, TraceReasonConfigDrift, TraceOutcomeDeferredAttached, tp.TemplateName, name, configDriftTracePayload(storedHash, currentHash, driftedFields, traceRecordPayload{
									"active_reason": "attached_recently",
								}))
							}
							continue
						}
						if isNamedSessionBead(*session) {
							// Defer config-drift restart for named sessions
							// that are actively in use (pending interaction,
							// tmux-attached, or recent activity). This prevents
							// draining a working agent mid-task without graceful
							// handoff. See gastownhall/gascity#119.
							activeReason, active, deferErr := shouldDeferNamedSessionConfigDrift(*session, sessFront, sp, name, clk, driftKey)
							if deferErr != nil {
								fmt.Fprintf(stderr, "session reconciler: recording config-drift deferral for %s: %v\n", name, deferErr) //nolint:errcheck
							}
							if active {
								if trace != nil {
									trace.RecordDecision(TraceSiteReconcilerConfigDrift, TraceReasonConfigDrift, TraceOutcomeDeferredActive, tp.TemplateName, name, configDriftTracePayload(storedHash, currentHash, driftedFields, traceRecordPayload{
										"active_reason": activeReason,
									}))
								}
								continue
							}
							if launchOnlyDrift {
								relaunched, launchBatch := relaunchAgentForLaunchDrift(ctx, sp, sessFront, session, name,
									tp, cityPath, cfg, store, storedHash, currentHash, storedProvision, storedLaunch,
									driftedFields, rec, trace, stdout, stderr)
								// Fold the returned batch unconditionally (Step 6d write-returns-Info).
								// On success it is the rebaseline patch; on the prepare/skew/relaunch
								// failure paths it is the buildPreparedStart prepare residue
								// — only started_config_hash and instance_token are folded, while
								// session_key and continuation_reset_pending stay intentionally
								// unthreaded (no same-tick Info reader) and self-heal on the next
								// store reload. ApplyPatch(nil) is a no-op.
								infoByID[session.ID] = infoByID[session.ID].ApplyPatch(launchBatch)
								if relaunched {
									continue
								}
							}
							// Fold the config-drift reset onto the snapshot (Step 6d
							// write-returns-Info). The alive lane falls through to the
							// aggregating refresh @~2710 today, but folding here future-proofs
							// that refresh's retirement (STEP6-PREPASS-AUDIT group 10).
							infoByID[session.ID] = infoByID[session.ID].ApplyPatch(resetConfiguredNamedSessionForConfigDrift(session, store, sp, name, alive, string(sessionpkg.StateStartPending), clk.Now().UTC(), stderr))
							if trace != nil {
								trace.RecordDecision(TraceSiteReconcilerConfigDrift, TraceReasonConfigDrift, TraceOutcomeRestartInPlace, tp.TemplateName, name, configDriftTracePayload(storedHash, currentHash, driftedFields, nil))
							}
							rec.Record(events.Event{
								Type:      events.SessionDraining,
								Actor:     "gc",
								Subject:   tp.DisplayName(),
								Message:   "config drift detected",
								SessionID: session.ID,
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
									trace.RecordDecision(TraceSiteReconcilerConfigDrift, TraceReasonPending, TraceOutcomeDeferredPending, tp.TemplateName, name, configDriftTracePayload(storedHash, currentHash, driftedFields, traceRecordPayload{
										"drain_canceled": drainCancelled,
									}))
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
									trace.RecordDecision(TraceSiteReconcilerConfigDrift, TraceReasonConfigDrift, TraceOutcomeDeferredActive, tp.TemplateName, name, configDriftTracePayload(storedHash, currentHash, driftedFields, traceRecordPayload{
										"active_reason": "live_assigned_work",
									}))
								}
								fmt.Fprintf(stdout, "Skipping config-drift drain for '%s': live assigned work found\n", name) //nolint:errcheck
								continue
							}
							if launchOnlyDrift {
								relaunched, launchBatch := relaunchAgentForLaunchDrift(ctx, sp, sessFront, session, name,
									tp, cityPath, cfg, store, storedHash, currentHash, storedProvision, storedLaunch,
									driftedFields, rec, trace, stdout, stderr)
								// Fold the returned batch unconditionally (Step 6d write-returns-Info).
								// On success it is the rebaseline patch; on the prepare/skew/relaunch
								// failure paths it is the buildPreparedStart prepare residue
								// — only started_config_hash and instance_token are folded, while
								// session_key and continuation_reset_pending stay intentionally
								// unthreaded (no same-tick Info reader) and self-heal on the next
								// store reload. ApplyPatch(nil) is a no-op.
								infoByID[session.ID] = infoByID[session.ID].ApplyPatch(launchBatch)
								if relaunched {
									continue
								}
							}
							ddt := driftDrainTimeout
							if ddt <= 0 {
								ddt = defaultDrainTimeout
							}
							if beginSessionDrain(*session, sp, dt, "config-drift", clk, ddt) {
								fmt.Fprintf(stdout, "Draining session '%s': config-drift\n", name) //nolint:errcheck
								if trace != nil {
									trace.RecordDecision(TraceSiteReconcilerConfigDrift, TraceReasonConfigDrift, TraceOutcomeDrain, tp.TemplateName, name, configDriftTracePayload(storedHash, currentHash, driftedFields, nil))
								}
								rec.Record(events.Event{
									Type:      events.SessionDraining,
									Actor:     "gc",
									Subject:   tp.DisplayName(),
									Message:   "config drift detected",
									SessionID: session.ID,
								})
							}
							continue
						}
					}

					if err := clearSessionConfigDriftDeferral(*session, sessFront); err != nil {
						fmt.Fprintf(stderr, "session reconciler: clearing config-drift deferral for %s: %v\n", name, err) //nolint:errcheck
					}

					// Core config matches — check live-only drift.
					// Use started_live_hash exclusively, matching
					// the started_config_hash pattern above.
					storedLive := infoByID[session.ID].StartedLiveHash
					currentLive := runtime.LiveFingerprint(agentCfg)
					if storedLive != currentLive {
						switch {
						case storedLive == "" && len(agentCfg.SessionLive) == 0:
							// No stored hash and no live config — silently
							// backfill the hash without running anything.
							_ = sessionFrontDoor(store).ApplyPatch(session.ID, map[string]string{
								"live_hash":         currentLive,
								"started_live_hash": currentLive,
							})
						case runtime.IsLegacyOrMismatchedVersion(storedLive):
							// Stored live hash from a pre-versioning or
							// version-mismatched binary — silently rebaseline
							// all four fingerprint fields rather than running
							// SessionLive again. ga-s760 FRs 1-3.
							outcome := rebaselineLegacyHashOutcome(storedLive)
							// Fold the rebaseline patch onto the snapshot (Step 6d write-returns-Info).
							// Pre-pass-masked (STEP6-PREPASS-AUDIT group 8).
							rebaseBatch, rebaseErr := silentRebaselineSessionHashes(session, sessFront, agentCfg)
							if rebaseErr != nil {
								fmt.Fprintf(stderr, "session reconciler: rebaselining legacy live hash for %s: %v\n", name, rebaseErr) //nolint:errcheck
							} else {
								fmt.Fprintf(stderr, "rebaselined legacy live hash for %s (stored=%s current=%s)\n", name, truncateHashForLog(storedLive), truncateHashForLog(currentLive)) //nolint:errcheck
							}
							infoByID[session.ID] = infoByID[session.ID].ApplyPatch(rebaseBatch)
							if trace != nil {
								trace.RecordDecision(TraceSiteReconcilerLiveDrift, TraceReasonLiveDrift, outcome, tp.TemplateName, name, traceRecordPayload{
									"stored_hash":  storedLive,
									"current_hash": currentLive,
								})
							}
						default:
							fmt.Fprintf(stdout, "Live config changed for '%s', re-applying...\n", tp.DisplayName()) //nolint:errcheck
							if err := sp.RunLive(name, agentCfg); err != nil {
								fmt.Fprintf(stderr, "session reconciler: RunLive %s: %v\n", name, err) //nolint:errcheck
							} else {
								_ = sessionFrontDoor(store).ApplyPatch(session.ID, map[string]string{
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
		// Read the drift-repair skip decision off the coherent snapshot. The
		// desired-path blocks above (drain-ack, restart-request, alive config-drift)
		// all fold their mutations onto infoByID now (Step 6d write-returns-Info), so
		// the snapshot entry is already byte-identical to the lockstep-updated bead —
		// no re-projection needed. (The restart-handoff consume above folds a batch
		// that excludes reset_committed_at, so that durable next-tick marker stays off
		// this tick's snapshot exactly as the old raw refresh kept it off; #2345.)
		infoAsleepDrift := infoByID[session.ID]
		skipAsleepDriftRepair := driftRestartedInPlace ||
			pendingResumePreservingNamedRestartInfo(infoAsleepDrift, clk, startupTimeout)
		if !alive && isNamedSessionBead(*session) && !skipAsleepDriftRepair {
			template := tp.TemplateName
			if template == "" {
				template = normalizedSessionTemplate(*session, cfg)
			}
			storedHash := infoByID[session.ID].StartedConfigHash
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
							// Fold the rebaseline patch onto the snapshot (Step 6d write-returns-Info).
							// This site `continue`s, so the fold must run before the continue.
							// Pre-pass-masked (STEP6-PREPASS-AUDIT group 8).
							rebaseBatch, rebaseErr := silentRebaselineSessionHashes(session, sessFront, agentCfg)
							if rebaseErr != nil {
								fmt.Fprintf(stderr, "session reconciler: rebaselining legacy hash for %s: %v\n", name, rebaseErr) //nolint:errcheck
							} else {
								fmt.Fprintf(stderr, "rebaselined legacy hash for %s (stored=%s current=%s)\n", name, truncateHashForLog(storedHash), truncateHashForLog(currentHash)) //nolint:errcheck
							}
							infoByID[session.ID] = infoByID[session.ID].ApplyPatch(rebaseBatch)
							if trace != nil {
								trace.RecordDecision(TraceSiteReconcilerConfigDrift, TraceReasonConfigDrift, outcome, tp.TemplateName, name, traceRecordPayload{
									"stored_hash":  storedHash,
									"current_hash": currentHash,
								})
							}
							continue
						}
						driftedFields := runtime.CoreFingerprintDriftFieldsFromJSON(infoByID[session.ID].CoreHashBreakdown, agentCfg)
						// Fold the config-drift reset onto the snapshot (Step 6d
						// write-returns-Info); this asleep lane `continue`s, so the fold must
						// run before the continue. Clears restart_requested on the snapshot
						// (#2574). Pre-pass-masked (STEP6-PREPASS-AUDIT group 10).
						infoByID[session.ID] = infoByID[session.ID].ApplyPatch(resetConfiguredNamedSessionForConfigDrift(session, store, sp, name, false, "asleep", clk.Now().UTC(), stderr))
						if trace != nil {
							trace.RecordDecision(TraceSiteReconcilerConfigDrift, TraceReasonConfigDrift, TraceOutcomeRepairInPlace, tp.TemplateName, name, configDriftTracePayload(storedHash, currentHash, driftedFields, nil))
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
			creationCompleteAt, hasAnchor := parseRFC3339Metadata(infoByID[session.ID].CreationCompleteAt)
			facts := sessionpkg.TimerFacts{
				Triggered: hasAnchor && maxAgeTr.shouldRestart(name, tp.TemplateName, creationCompleteAt, clk.Now()),
			}
			if facts.Triggered {
				facts.Blocker = lifecycleTimerBlockerInfo(infoByID[session.ID], clk.Now())
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
					trace.RecordDecision(TraceSiteReconcilerMaxSessionAge, TraceReasonCode(dec.TraceReason), TraceOutcomeCode(dec.TraceOutcome), tp.TemplateName, name, nil)
				}
			case sessionpkg.TimerActionStop:
				fmt.Fprintf(stderr, "session reconciler: preemptive max-age restart for %s (age=%s)\n", tp.DisplayName(), clk.Now().Sub(creationCompleteAt).Round(time.Second)) //nolint:errcheck // best-effort stderr
				if trace != nil {
					trace.RecordDecision(TraceSiteReconcilerMaxSessionAge, TraceReasonCode(dec.TraceReason), TraceOutcomeCode(dec.TraceOutcome), tp.TemplateName, name, nil)
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
					_ = sessionFrontDoor(store).ApplyPatch(session.ID, batch)
					if session.Metadata == nil {
						session.Metadata = make(map[string]string, len(batch))
					}
					for key, value := range batch {
						session.Metadata[key] = value
					}
					// Fold the sleep onto the snapshot (Step 6d write-returns-Info): this
					// max-age kill falls through to the wakeTargets append below, whose
					// awake-scan read of state=asleep drives a same-tick re-wake — so the
					// snapshot must carry the sleep. Base is coherent (the aggregating
					// refresh @~2692 synced it and the intervening drift blocks `continue`).
					// A pre-pass-masked writer (STEP6-PREPASS-AUDIT group 11).
					//
					// START-EXECUTION COUPLING (Step 5c): the raw session.Metadata mirror
					// loop above is RETAINED. The same-tick re-wake can reach
					// startCandidates, and the start executor reads last_woke_at (cleared
					// by SleepPatch) off the raw bead via wakeFairnessTime before it
					// re-Gets from the store; dropping the mirror would perturb ordering.
					infoByID[session.ID] = infoByID[session.ID].ApplyPatch(batch)
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
				facts.Blocker = lifecycleTimerBlockerInfo(infoByID[session.ID], clk.Now())
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
					trace.RecordDecision(TraceSiteReconcilerIdleTimeout, TraceReasonCode(dec.TraceReason), TraceOutcomeCode(dec.TraceOutcome), tp.TemplateName, name, payload)
				}
				if dec.SkipWakePass {
					continue
				}
			case sessionpkg.TimerActionStop:
				fmt.Fprintf(stderr, "session reconciler: idle timeout for %s\n", tp.DisplayName()) //nolint:errcheck // best-effort stderr
				if trace != nil {
					trace.RecordDecision(TraceSiteReconcilerIdleTimeout, TraceReasonCode(dec.TraceReason), TraceOutcomeCode(dec.TraceOutcome), tp.TemplateName, name, nil)
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
					_ = sessionFrontDoor(store).ApplyPatch(session.ID, batch)
					if session.Metadata == nil {
						session.Metadata = make(map[string]string, len(batch))
					}
					for key, value := range batch {
						session.Metadata[key] = value
					}
					// Fold the sleep onto the snapshot (Step 6d write-returns-Info): the
					// idle kill falls through to the wakeTargets append below, whose
					// awake-scan read of state=asleep drives a same-tick re-wake. Base
					// coherent (aggregating refresh @~2692 + intervening `continue`s). A
					// pre-pass-masked writer (STEP6-PREPASS-AUDIT group 12).
					//
					// START-EXECUTION COUPLING (Step 5c): the raw session.Metadata mirror
					// loop above is RETAINED — same rationale as the max-age kill: the
					// same-tick re-wake reads last_woke_at (cleared by SleepPatch) off the
					// raw bead via wakeFairnessTime before the start executor re-Gets it.
					infoByID[session.ID] = infoByID[session.ID].ApplyPatch(batch)
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

	// Use ComputeAwakeSet for the wake/sleep decision. The awake scan reads every
	// session's typed Info from the coherent infoByID snapshot. The former blanket
	// pre-pass (a full re-project of every session right before this scan) is GONE
	// (Step 6d): every forward-pass writer now folds its own mutation onto the
	// snapshot via write-returns-Info (STEP6-PREPASS-AUDIT groups 1-12), so the
	// snapshot is already coherent here without re-projecting the raw beads.
	phaseStart = time.Now()
	// Build the awake-scan domain from the coherent typed snapshot in `ordered`
	// slice order (load-bearing — ComputeAwakeSet resolves SessionName
	// last-write-wins over a non-unique key, so map iteration order must not
	// leak in). Every orderedIDs entry keys infoByID (built at tick entry, only
	// updated thereafter, never deleted), so this reproduces the former
	// per-bead snapshot lookup exactly (Step 5e: walk orderedIDs, not raw beads).
	sessionInfos := make([]sessionpkg.Info, len(orderedIDs))
	for i := range orderedIDs {
		sessionInfos[i] = infoByID[orderedIDs[i]]
	}
	awakeInput := buildAwakeInputFromReconciler(
		cfg, cityPath, sessionInfos, poolDesired, namedSessionDemand, workSet, readyWaitSet,
		assignedWorkBeads, reconcileOpts.readyAssignedFlags, wakeTargets, sp, clk.Now(),
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
		// Typed projection for this iteration's decision reads (session_name,
		// pin_awake, template, sleep_intent). Refreshed from the snapshot: this is
		// a post-Phase-1 loop, and every Phase-1 mutation folds onto infoByID now
		// (Step 6d write-returns-Info), so the snapshot entry is already coherent —
		// no re-projection needed. The loop itself writes only wakeEvals/eval, never
		// the bead. The sleep policy resolvers (resolveSessionSleepPolicy,
		// configWakeSuppressed) read whole-bead + runtime state and stay raw.
		info := infoByID[target.session.ID]
		policy := resolveSessionSleepPolicy(*target.session, cfg, sp)
		eval.Policy = policy
		name := info.SessionNameMetadata
		decision := awakeDecisions[name]
		if decision.ShouldWake && !pendingInteractionReady(sp, name) && info.PinAwake != "true" && configWakeSuppressed(*target.session, policy, sp, clk) {
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
			template := normalizedSessionTemplateInfo(info, cfg)
			hasExplicitSleepIntent := info.SleepIntent != ""
			demandOverrides := wakeDemandOverridesSleepSuppression(decision, eval, policy, poolDesired, template, hasExplicitSleepIntent)
			if !demandOverrides {
				eval.ConfigSuppressed = true
				eval.Reasons = nil // Clear reasons so Phase 2 does not cancel the drain.
				eval.Reason = ""
			}
		}
		wakeEvals[target.session.ID] = eval
	}

	idleProbeTargets := selectIdleProbeTargets(wakeTargets, wakeEvals, dt, infoByID)
	launchIdleProbes(ctx, idleProbeTargets, wakeTargets, dt, sp, clk, infoByID)
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
		// Typed projection for this iteration's decision reads. infoByID is
		// coherent here: every forward-pass mutation folds onto it, and this
		// loop's own mutations fold back before any later read observes them.
		// The whole-bead helpers below (persistSleepPolicyMetadata,
		// sessionHasOpenAssignedWorkForReachableStore, pruneAgentHomeWorktreeIfSafe,
		// collectSessionAssignedWork inside emitSessionStrandedDiagnostic) stay raw
		// by design.
		info := infoByID[target.session.ID]
		name := info.SessionNameMetadata
		decision, hasDec := awakeDecisions[name]
		shouldWake := hasDec && decision.ShouldWake

		eval := wakeEvals[target.session.ID]
		if shouldWake && eval.ConfigSuppressed {
			shouldWake = false
		}
		persistSleepPolicyMetadata(target.session, sessFront, eval.Policy, eval.ConfigSuppressed)

		if shouldWake && !target.alive {
			// Session should be awake but isn't — wake it.
			if isFailedCreateSessionInfo(info) {
				if trace != nil {
					trace.RecordDecision(TraceSiteReconcilerWakeDecision, TraceReasonWake, TraceOutcomeFailedCreate, target.tp.TemplateName, name, traceRecordPayload{
						"pending_create_claim": strings.TrimSpace(info.PendingCreateClaimMetadata),
					})
				}
				continue
			}
			if sessionIsQuarantinedInfo(info, clk) {
				continue // crash-loop protection
			}
			if pendingCreateStartInFlightInfo(info, clk, startupTimeout) {
				if trace != nil {
					trace.RecordDecision(TraceSiteReconcilerWakeDecision, TraceReasonWake, TraceOutcomeStartInFlight, target.tp.TemplateName, name, traceRecordPayload{
						"pending_create_claim": strings.TrimSpace(info.PendingCreateClaimMetadata),
						"last_woke_at":         info.LastWokeAt,
					})
				}
				continue
			}
			// Respawn circuit breaker: for named sessions the supervisor
			// will otherwise retry indefinitely. This phase only blocks
			// already-OPEN breakers; restart accounting happens at the
			// prepared-start boundary after dependency and wake-budget gates.
			if cbEnabled {
				identity := namedSessionIdentityInfo(info)
				if identity != "" {
					if cb.IsOpen(identity, cbNow) {
						if err := persistSessionCircuitBreakerMetadata(sessFront, target.session.ID, cb, identity, cbNow); err != nil {
							fmt.Fprintf(stderr, "session reconciler: %v\n", err) //nolint:errcheck // best-effort stderr
						}
						cb.LogOpenOnce(identity, stderr)
						if trace != nil {
							trace.RecordDecision(TraceSiteReconcilerCircuitOpen, TraceReasonCircuitOpen, TraceOutcomeSkipped, target.tp.TemplateName, name, traceRecordPayload{
								"identity": identity,
							})
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
						trace.RecordDecision(TraceSiteReconcilerProviderHealthGate, TraceReasonProviderRed, TraceOutcomeRespawnSkipped, target.tp.TemplateName, name, traceRecordPayload{
							"provider": phProvider,
						})
					}
					continue // skip startCandidates; wake budget is NOT consumed
				}
			}

			if trace != nil {
				trace.RecordDecision(TraceSiteReconcilerWakeDecision, TraceReasonWake, TraceOutcomeStartCandidate, target.tp.TemplateName, name, traceRecordPayload{
					"should_wake": shouldWake,
				})
			}
			if fold := recordCurrentBeadIDOnWake(target.session, sessFront, decision.AssignedWorkBeadID, stderr); fold != nil {
				infoByID[target.session.ID] = infoByID[target.session.ID].ApplyPatch(fold)
			}
			startCandidates = append(startCandidates, startCandidate{
				session: target.session,
				tp:      target.tp,
				order:   len(startCandidates),
			})
		}

		if shouldWake && target.alive {
			// Bead-reassignment cycle: when an alive named session is
			// reassigned to a different bead than the one it's currently
			// processing, wake_mode=fresh requires a brand-new conversation
			// on the new bead. ComputeAwakeSet signals this via
			// RequiresFreshCycle; honor it by routing through the same
			// restart-handoff machinery as `gc runtime request-restart`.
			// See #1893 (controller: alive on_demand session ignores
			// bd update --assignee).
			if decision.RequiresFreshCycle && info.WakeMode == "fresh" {
				if ran, fold := cycleAliveSessionForFreshReassign(target.session, target.tp, sp, store, cfg, cb, name, decision.AssignedWorkBeadID, clk.Now(), stdout, stderr, trace); ran {
					if fold != nil {
						infoByID[target.session.ID] = infoByID[target.session.ID].ApplyPatch(fold)
					}
					continue
				}
			}
			// Stamp currently_processing_bead_id so the next divergence
			// check has a baseline. Backfills legacy sessions that were
			// already alive before this metadata existed and refreshes the
			// record after the agent picks up its next bead in resume mode.
			if fold := recordCurrentBeadIDOnWake(target.session, sessFront, decision.AssignedWorkBeadID, stderr); fold != nil {
				infoByID[target.session.ID] = infoByID[target.session.ID].ApplyPatch(fold)
			}
			// Session is correctly awake. Cancel any non-drift drain
			// (handles scale-back-up: agent returns to desired set while draining).
			cancelSessionDrainInfo(info, sp, dt)
			clearCompletedIdleProbe(target.session.ID, dt)
			if info.SleepIntent == "idle-stop-pending" {
				// Persist the intent clear to the store and the typed snapshot. This
				// runs on an ALIVE session (the shouldWake && alive arm), which never
				// enters startCandidates, and sleep_intent is not read off the raw
				// session bead anywhere downstream this tick — so Step 5c dropped the
				// raw session.Metadata mirror.
				_ = sessionFrontDoor(store).SetMarker(target.session.ID, "sleep_intent", "")
				infoByID[target.session.ID] = infoByID[target.session.ID].ApplyPatch(sessionpkg.MetadataPatch{"sleep_intent": ""})
			}
		}

		if !shouldWake && target.alive {
			// No reason to be awake — begin drain.
			intent := info.SleepIntent
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
				if intent != "idle-stop-pending" && !shouldBeginIdleDrainInfo(info, eval, dt, sp) {
					continue
				}
				if intent != "idle-stop-pending" {
					if fold := markIdleSleepPending(target.session, sessFront); fold != nil {
						infoByID[target.session.ID] = infoByID[target.session.ID].ApplyPatch(fold)
					}
				}
			}
			if beginSessionDrainInfo(info, sp, dt, reason, clk, defaultDrainTimeout) {
				fmt.Fprintf(stdout, "Draining session '%s': %s\n", name, reason) //nolint:errcheck
				if trace != nil {
					trace.RecordDecision(TraceSiteReconcilerDrainDecision, TraceReasonCode(reason), TraceOutcomeDrain, target.tp.TemplateName, name, traceRecordPayload{
						"sleep_intent": intent,
					})
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
		poolFreeable := !shouldWake && !target.alive && isPoolSessionSlotFreeableInfo(info) && isPoolManagedSessionInfo(info)
		if poolFreeable {
			var assignedErr error
			hasAssignedWork, assignedErr = sessionHasOpenAssignedWorkForReachableStore(cityPath, cfg, store, rigStores, *target.session)
			if assignedErr != nil {
				fmt.Fprintf(stderr, "session reconciler: checking assigned work for drained %s: %v\n", name, assignedErr) //nolint:errcheck
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
			if fold := emitSessionStrandedDiagnostic(cityPath, cfg, store, rigStores, target.session, target.tp.TemplateName, rec, clk, stderr); fold != nil {
				infoByID[target.session.ID] = infoByID[target.session.ID].ApplyPatch(fold)
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
			closeReason := strings.TrimSpace(info.SleepReason)
			if closeReason == "" {
				closeReason = "drained"
			}
			if closeBead(store, target.session.ID, closeReason, clk.Now().UTC(), stderr) {
				// Store-only close family: mirror the close onto the snapshot
				// (write-returns-Info) so a later reader sees Closed=true.
				infoByID[target.session.ID] = infoByID[target.session.ID].MarkClosed()
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
	startedThisTick = plannedWakes
	recordPhase(TraceSiteSessionReconcileStartExecution, "session_reconcile.execute_planned_starts", phaseStart, map[string]any{
		"start_candidate_count": len(startCandidates),
		"planned_wake_count":    plannedWakes,
	})

	if ctx != nil && ctx.Err() != nil {
		return plannedWakes
	}

	// Phase 2: Advance all in-flight drains. The drain scan reads the coherent
	// typed snapshot (write-returns-Info keeps it current through Phase 1), not
	// the raw working beads — so it observes the same post-forward-pass state the
	// old &ordered[i] aliases carried, without holding a raw pointer map.
	phaseStart = time.Now()
	infoLookup := func(id string) (sessionpkg.Info, bool) {
		info, ok := infoByID[id]
		return info, ok
	}
	advanceSessionDrainsWithSessionsTraced(dt, sp, store, infoLookup, wakeEvals, cfg, clk, trace)
	clearMissingIdleProbes(dt, infoByID)
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
	openInfos []sessionpkg.Info,
	info sessionpkg.Info,
	clk clock.Clock,
	stderr io.Writer,
) (TemplateParams, error) {
	if cityPath == "" {
		cityPath = "."
	}
	if cityName == "" && cfg != nil {
		cityName = cfg.EffectiveCityName()
	}
	identity := namedSessionIdentityInfo(info)
	spec, ok := findNamedSessionSpec(cfg, cityName, identity)
	if !ok || spec.Agent == nil {
		return TemplateParams{}, fmt.Errorf("configured named session %q not found", identity)
	}
	bp := newAgentBuildParams(cityName, cityPath, cfg, sp, clk.Now().UTC(), store, stderr)
	bp.sessionBeads = newSessionBeadSnapshotFromInfos(openInfos)
	fpExtra := buildFingerprintExtra(spec.Agent)
	tp, err := resolveTemplateForSessionBeadInfo(bp, spec.Agent, identity, fpExtra, info)
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
	stores, err := reachableStoresForSession(cityPath, cfg, store, rigStores, session)
	if err != nil {
		return false, err
	}
	for _, s := range stores {
		if has, err := sessionHasOpenAssignedWorkInStoreByIdentifiers(s, identifiers); err != nil || has {
			return has, err
		}
	}
	return false, nil
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
	stores, err := reachableStoresForSession(cityPath, cfg, store, rigStores, session)
	if err != nil {
		return false, err
	}
	for _, s := range stores {
		if has, err := sessionHasAwakeAssignedWorkInStoreByIdentifiers(s, identifiers); err != nil || has {
			return has, err
		}
	}
	return false, nil
}

// reachableStoresForSession returns the store(s) in which the session's assigned
// work can live, applying the same cross-store model as openSessionReachableStoreRef.
// A cross-store-eligible (city-scoped) session federates across the primary store
// and every rig store (vp-kvp); a session whose template/agent can't be resolved
// falls back to the same fan-out (legacy keep-on-match fail-safe); a rig-bound
// session routes to its one rig store; every other session routes to the primary
// store. The slice is ordered primary-first so "first match" callers keep their
// historical ordering. Returns an error only when a resolved rig store is missing.
func reachableStoresForSession(cityPath string, cfg *config.City, store beads.Store, rigStores map[string]beads.Store, session beads.Bead) ([]beads.Store, error) {
	agentCfg := sessionAgentConfig(cfg, session)
	if agentCfg == nil || agentIsCrossStoreEligible(agentCfg) {
		// Cross-store-eligible work lives in the work-class candidate set: the
		// primary work store plus every rig work store. The downstream
		// List{Assignee,Status} probes are work queries, so this is the work
		// arm; on a single-store city it collapses to the same store the
		// session probes use (identity).
		return workAssignmentStores(store, rigStores), nil
	}
	storeRef := assignedWorkStoreRefForAgent(cityPath, cfg, agentCfg)
	if storeRef == "" {
		return []beads.Store{store}, nil
	}
	rigStore, ok := rigStores[storeRef]
	if !ok || rigStore == nil {
		return nil, fmt.Errorf("rig store %q unavailable for session %q", storeRef, session.Metadata["session_name"])
	}
	return []beads.Store{rigStore}, nil
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
	stores, err := reachableStoresForSession(cityPath, cfg, store, rigStores, session)
	if err != nil {
		return beads.Bead{}, false, err
	}
	for _, s := range stores {
		if bead, found, err := firstOpenAssignedWorkBeadInStoreByIdentifiers(s, identifiers); err != nil || found {
			return bead, found, err
		}
	}
	return beads.Bead{}, false, nil
}

func firstOpenAssignedWorkBeadInStoreByIdentifiers(store beads.Store, identifiers []string) (beads.Bead, bool, error) {
	if store == nil {
		return beads.Bead{}, false, nil
	}
	wa := workAssignmentForStore(beads.WorkStore{Store: store})
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
			items, err := wa.OpenAssignedTo(assignee, status, beads.TierBoth, true)
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
) sessionpkg.MetadataPatch {
	if rec == nil || session == nil {
		return nil
	}
	if session.Metadata == nil {
		session.Metadata = make(map[string]string, 1)
	}
	if strings.TrimSpace(session.Metadata[strandedEventEmittedKey]) != "" {
		return nil
	}
	assignedWork, err := collectSessionAssignedWork(cityPath, cfg, store, rigStores, *session)
	if err != nil {
		fmt.Fprintf(stderr, "session reconciler: collecting stranded work ids for %s: %v\n", session.Metadata["session_name"], err) //nolint:errcheck
	}
	diagnosticWork := filterDetachedStrandedDiagnosticWork(assignedWork)
	if err == nil && len(assignedWork) > 0 && len(diagnosticWork) == 0 {
		return nil
	}
	ids := strandedAssignedWorkIDs(diagnosticWork)
	now := clk.Now().UTC()
	rec.Record(events.Event{
		Type:      events.SessionStranded,
		Ts:        now,
		Actor:     "gc",
		Subject:   session.ID,
		Message:   formatStrandedMessage(template, session.Metadata["session_name"], ids),
		SessionID: session.ID,
		Payload:   api.SessionStrandedPayloadJSON(session.ID, session.Metadata["session_name"], template, ids),
	})
	// CROSS-TICK EMIT-ONCE COUPLING (Step 5c): the raw session.Metadata mirror is
	// RETAINED. Set the in-memory marker BEFORE the durable SetMarker write so a
	// transient store-write failure cannot cause the next tick — still holding this
	// same *Bead value (the controller may carry a bead forward across ticks) or a
	// re-fetch whose durable write is missing — to re-emit and produce a
	// duplicate-emission storm. Regression-guarded by
	// TestReconcileSessionBeads_PoolSlotStrandedThrottleSurvivesSetMetadataFailure.
	session.Metadata[strandedEventEmittedKey] = now.Format(time.RFC3339)
	if err := sessionFrontDoor(store).SetMarker(session.ID, strandedEventEmittedKey, now.Format(time.RFC3339)); err != nil {
		fmt.Fprintf(stderr, "session reconciler: stamping stranded throttle marker on %s: %v\n", session.ID, err) //nolint:errcheck
	}
	// Return the throttle-marker fold so the reconciler can apply it to the
	// infoByID snapshot (write-returns-Info). Applied regardless of the
	// SetMarker store result — the in-memory marker above is the emit-once
	// guard, and the snapshot must match it.
	return sessionpkg.MetadataPatch{strandedEventEmittedKey: now.Format(time.RFC3339)}
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
		wa := workAssignmentForStore(beads.WorkStore{Store: s})
		for _, status := range []string{"open", "in_progress"} {
			for _, assignee := range identifiers {
				if assignee == "" {
					continue
				}
				items, err := wa.OpenAssignedTo(assignee, status, beads.TierBoth, true)
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
	// Route to the same store(s) the gate routed to.
	stores, err := reachableStoresForSession(cityPath, cfg, store, rigStores, session)
	if err != nil {
		return out, err
	}
	for _, s := range stores {
		if err := collect(s); err != nil {
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
	wa := workAssignmentForStore(beads.WorkStore{Store: store})
	// This positive-only probe intentionally keeps the tier-scoped cache
	// helper: HandlesFor(...).Cached.List reads both tiers by contract. The
	// CachedList assertion lives inside the façade on the unwrapped .Store.
	if items, ok := wa.CachedOpenAssignedWisps(assignee, status); ok {
		if wa.HasNonSessionWork(items) {
			return true, nil
		}
	}
	return sessionHasOpenAssignedWorkForTier(store, assignee, status, beads.TierWisps, true)
}

func sessionHasReadyAssignedWorkForTier(store beads.Store, assignee string, tierMode beads.TierMode) (bool, error) {
	wa := workAssignmentForStore(beads.WorkStore{Store: store})
	items, err := wa.ReadyAssignedTo(assignee, tierMode)
	if err != nil {
		return false, err
	}
	return wa.HasNonSessionWork(items), nil
}

func sessionHasOpenAssignedWorkForTier(store beads.Store, assignee, status string, tierMode beads.TierMode, live bool) (bool, error) {
	wa := workAssignmentForStore(beads.WorkStore{Store: store})
	items, err := wa.OpenAssignedTo(assignee, status, tierMode, live)
	if err != nil {
		return false, err
	}
	return wa.HasNonSessionWork(items), nil
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

func shouldDeferNamedSessionConfigDrift(session beads.Bead, sessFront *sessionpkg.Store, sp runtime.Provider, name string, clk clock.Clock, driftKey string) (string, bool, error) {
	reason, active := namedSessionActiveUseReason(session, sp, name, clk)
	if !active {
		return "", false, nil
	}
	switch reason {
	case "activity_unknown":
		return boundedNamedSessionConfigDriftDeferral(session, sessFront, clk, driftKey, reason, namedSessionActivityThreshold)
	case "recent_activity":
		return boundedNamedSessionConfigDriftDeferral(session, sessFront, clk, driftKey, reason, namedSessionRecentActivityConfigDriftDeferralLimit)
	}
	return reason, true, nil
}

func boundedNamedSessionConfigDriftDeferral(
	session beads.Bead,
	sessFront *sessionpkg.Store,
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
		if err := recordNamedSessionConfigDriftDeferredAt(session, sessFront, now, driftKey); err != nil {
			return "", false, err
		}
		return reason, true, nil
	}
	raw := session.Metadata[namedSessionConfigDriftDeferredAtMetadata]
	if raw == "" {
		if err := recordNamedSessionConfigDriftDeferredAt(session, sessFront, now, driftKey); err != nil {
			return "", false, err
		}
		return reason, true, nil
	}
	deferredAt, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		if err := recordNamedSessionConfigDriftDeferredAt(session, sessFront, now, driftKey); err != nil {
			return "", false, err
		}
		return reason, true, nil
	}
	if now.Sub(deferredAt) < limit {
		return reason, true, nil
	}
	return "", false, nil
}

func recordNamedSessionConfigDriftDeferredAt(session beads.Bead, sessFront *sessionpkg.Store, t time.Time, driftKey string) error {
	if sessFront == nil || session.ID == "" {
		return nil
	}
	return sessFront.ApplyPatch(session.ID, map[string]string{
		namedSessionConfigDriftDeferredAtMetadata:  t.UTC().Format(time.RFC3339),
		namedSessionConfigDriftDeferredKeyMetadata: driftKey,
	})
}

func clearSessionConfigDriftDeferral(session beads.Bead, sessFront *sessionpkg.Store) error {
	if sessFront == nil || session.ID == "" {
		return nil
	}
	if session.Metadata[namedSessionConfigDriftDeferredAtMetadata] == "" &&
		session.Metadata[namedSessionConfigDriftDeferredKeyMetadata] == "" &&
		session.Metadata[sessionAttachedConfigDriftDeferredAtMetadata] == "" &&
		session.Metadata[sessionAttachedConfigDriftDeferredKeyMetadata] == "" {
		return nil
	}
	return sessFront.ApplyPatch(session.ID, map[string]string{
		namedSessionConfigDriftDeferredAtMetadata:     "",
		namedSessionConfigDriftDeferredKeyMetadata:    "",
		sessionAttachedConfigDriftDeferredAtMetadata:  "",
		sessionAttachedConfigDriftDeferredKeyMetadata: "",
	})
}

func recordSessionAttachedConfigDriftDeferral(session beads.Bead, sessFront *sessionpkg.Store, clk clock.Clock, driftKey string) error {
	if sessFront == nil || session.ID == "" {
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
	return sessFront.ApplyPatch(session.ID, map[string]string{
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
	trace.RecordDecision(TraceSiteReconcilerPendingCreate, TraceReasonHealClearedStaleLease, TraceOutcomeApplied, template, name, traceRecordPayload{
		"last_woke_at":              lastWokeAtBeforeHeal,
		"pending_create_started_at": pendingCreateStartedAtBeforeHeal,
		"provider_alive":            providerAlive,
		"state_after":               session.Metadata["state"],
		"state_before":              stateBeforeHeal,
	})
}

func applyTemplateOverridesToConfig(agentCfg *runtime.Config, session beads.Bead, tp TemplateParams) {
	applyTemplateOverridesToConfigInfo(agentCfg, sessionpkg.InfoFromPersistedBead(session), tp)
}

// applyTemplateOverridesToConfigInfo is the session.Info form of
// applyTemplateOverridesToConfig: byte-identical logic reading the parsed
// template overrides directly off Info instead of re-projecting a raw bead.
func applyTemplateOverridesToConfigInfo(agentCfg *runtime.Config, info sessionpkg.Info, tp TemplateParams) {
	if agentCfg == nil {
		return
	}
	if tp.ResolvedProvider == nil || len(tp.ResolvedProvider.OptionsSchema) == 0 {
		return
	}
	ovr, err := sessionpkg.ParseTemplateOverridesFromInfo(info)
	if err != nil || len(ovr) == 0 {
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
) map[string]string {
	if session == nil || store == nil {
		return nil
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
	if err := sessionFrontDoor(store).ApplyPatch(session.ID, batch); err != nil {
		fmt.Fprintf(stderr, "session reconciler: recording config-drift repair for %s: %v\n", sessionName, err) //nolint:errcheck
		return nil
	}
	if session.Metadata == nil {
		session.Metadata = make(map[string]string, len(batch))
	}
	// START-EXECUTION COUPLING (Step 5c): the raw session.Metadata mirror loop is
	// RETAINED. The start-pending caller (the alive lane) falls through without a
	// `continue`, so the repaired session can reach startCandidates this same tick,
	// and the start executor reads last_woke_at (cleared by ConfigDriftResetPatch)
	// off the raw bead via wakeFairnessTime before it re-Gets from the store.
	for key, value := range batch {
		session.Metadata[key] = value
	}
	// Return the mirrored batch so the caller can fold it onto the typed snapshot
	// (Step 6d write-returns-Info). The batch clears restart_requested (part of
	// ConfigDriftResetPatch), so folding it keeps a consumed restart marker off the
	// snapshot once the pre-pass is dropped (#2574).
	return batch
}

// shouldBeginIdleDrainInfo reads the session id and session_name off the Info
// snapshot (both verbatim raw mirrors), so it is byte-identical to the raw form
// it replaced. The former nil-bead guard is gone: the sole caller passes
// infoByID[target.session.ID] for a wakeTarget whose bead is always non-nil.
func shouldBeginIdleDrainInfo(
	info sessionpkg.Info,
	eval wakeEvaluation,
	dt *drainTracker,
	sp runtime.Provider,
) bool {
	if eval.Policy.Class == config.SessionSleepNonInteractive {
		return true
	}
	if eval.Policy.Capability != runtime.SessionSleepCapabilityFull || sp == nil {
		return false
	}
	probe, ok := dt.idleProbe(info.ID)
	if !ok || !probe.ready {
		return false
	}
	defer dt.clearIdleProbe(info.ID)
	if !probe.success {
		return false
	}
	lastActivity, err := workerSessionTargetLastActivityWithConfig("", nil, sp, nil, info.SessionNameMetadata)
	if err != nil {
		return false
	}
	return lastActivity.IsZero() || !lastActivity.After(probe.completedAt)
}

func selectIdleProbeTargets(
	wakeTargets []wakeTarget,
	wakeEvals map[string]wakeEvaluation,
	dt *drainTracker,
	infoByID map[string]sessionpkg.Info,
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
		if infoByID[target.session.ID].SleepIntent != "" {
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
	infoByID map[string]sessionpkg.Info,
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
		name := infoByID[target.session.ID].SessionNameMetadata
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

// clearMissingIdleProbes drops idle-probe state for any session that has left
// the tick's working set. It uses infoByID purely as a presence oracle: an id
// absent from the snapshot is a session no longer under reconciliation, so its
// stale probe must be cleared. infoByID carries exactly the ids of the raw
// working set (both are built 1:1 from `ordered`, the snapshot is never keyed
// beyond it, and refresh only updates existing entries), so routing this off the
// typed snapshot instead of the raw beadByID pointer map is presence-identical
// (front-door migration Step 6c: retire a read-side raw working-set consumer).
func clearMissingIdleProbes(dt *drainTracker, infoByID map[string]sessionpkg.Info) {
	if dt == nil {
		return
	}
	dt.mu.Lock()
	var stale []string
	for id := range dt.idleProbes {
		if _, ok := infoByID[id]; !ok {
			stale = append(stale, id)
		}
	}
	dt.mu.Unlock()
	for _, id := range stale {
		dt.clearIdleProbe(id)
	}
}

// resolveWorkDirAgainstCity anchors a bead-stored work_dir value to the city
// root. Worktree-per-bead dispatch stores this metadata city-relative (e.g.
// ".gc/worktrees/gascity/builder/<slug>") so the value stays valid across
// machines with different absolute city paths; resolving it with os.Stat
// directly would instead resolve against the calling process's cwd, which is
// how scaffold staging leaked into shared long-lived worktrees (ga-ajw1no).
// Already-absolute values (the legacy convention) pass through unchanged.
func resolveWorkDirAgainstCity(cityPath, workDir string) string {
	if workDir == "" || cityPath == "" || filepath.IsAbs(workDir) {
		return workDir
	}
	return filepath.Join(cityPath, workDir)
}

// resolveTaskWorkDir checks the agent's assigned task beads for a work_dir
// metadata field. If a task bead has work_dir set and the directory exists
// on disk, that path is returned. This lets the reconciler start the agent
// in the worktree that the previous session (or this session's prior run)
// created, without any prompt-side logic.
func resolveTaskWorkDir(cityPath string, store beads.Store, assignees ...string) string {
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
			if wd == "" {
				continue
			}
			resolved := resolveWorkDirAgainstCity(cityPath, wd)
			if info, err := os.Stat(resolved); err == nil && info.IsDir() {
				return resolved
			}
		}
	}
	return ""
}

// dispatchOptionMetadataKey returns the bead-metadata key carrying a
// per-dispatch provider option choice for the given OptionsSchema key.
func dispatchOptionMetadataKey(key string) string {
	return beadmeta.OptionMetadataPrefix + key
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
func newAssignedTaskWorkDirResolver(cityPath string, assignedWorkBeads []beads.Bead) taskWorkDirResolver {
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
		workDir = resolveWorkDirAgainstCity(cityPath, workDir)
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

// sessionHashRebaselineMetadata builds the fingerprint metadata fields
// — started_config_hash, started_live_hash, live_hash, started_provision_hash,
// started_launch_hash, core_hash_breakdown — from a resolved agent config.
// Callers merge the result into a session bead's metadata batch to move its
// config-drift baseline to agentCfg. This is the full-rebaseline form (legacy/
// version-artifact rebaseline): the config did not actually change, so every
// baseline — including the live half — moves to the current binary's hashes.
func sessionHashRebaselineMetadata(agentCfg runtime.Config) (map[string]string, error) {
	breakdownJSON, err := json.Marshal(runtime.CoreFingerprintBreakdown(agentCfg))
	if err != nil {
		return nil, fmt.Errorf("marshaling core_hash_breakdown: %w", err)
	}
	liveHash := runtime.LiveFingerprint(agentCfg)
	return map[string]string{
		"started_config_hash":    runtime.CoreFingerprint(agentCfg),
		"started_live_hash":      liveHash,
		"live_hash":              liveHash,
		"started_provision_hash": runtime.ProvisionFingerprint(agentCfg),
		"started_launch_hash":    runtime.LaunchFingerprint(agentCfg),
		"core_hash_breakdown":    string(breakdownJSON),
	}, nil
}

// silentRebaselineSessionHashes overwrites the four fingerprint metadata
// fields (started_config_hash, started_live_hash, live_hash,
// core_hash_breakdown) with values produced by the current binary. Used
// when a stored hash carries no version prefix or a version prefix that
// does not match runtime.FingerprintVersion. The reconciler invokes this
// instead of draining the session — the hash mismatch is purely a
// versioning artifact, not real config drift.
//
// Returns (patch, nil) on success, (nil, err) on persist error, (nil, nil)
// when nothing was written (nil session or nil front-door). The caller folds
// the returned patch onto the typed snapshot via ApplyPatch (nil is a no-op).
func silentRebaselineSessionHashes(session *beads.Bead, sessFront *sessionpkg.Store, agentCfg runtime.Config) (map[string]string, error) {
	if session == nil || sessFront == nil {
		return nil, nil
	}
	patch, err := sessionHashRebaselineMetadata(agentCfg)
	if err != nil {
		return nil, err
	}
	if err := sessFront.ApplyPatch(session.ID, patch); err != nil {
		return nil, fmt.Errorf("rebaselining hashes: %w", err)
	}
	// The caller folds the returned patch onto the typed snapshot (write-returns-
	// Info). The rebaselined hash fields are never read off the raw session bead
	// this tick — the drift decision reads Info.StartedConfigHash and the start
	// path re-reads started_config_hash off a fresh store.Get — so Step 5c dropped
	// the raw session.Metadata mirror.
	return patch, nil
}

// relaunchAgentForLaunchDrift handles a launch-only config-drift (B2.3): the
// LaunchFingerprint moved while the ProvisionFingerprint held, so the agent can
// be re-launched in the existing warm box instead of a full re-provision
// restart. It mirrors the live-drift→RunLive clause: act, and on success
// rebaseline the Core/provision/launch baselines so the next tick sees no drift.
//
// The config handed to Relaunch is derived by buildPreparedStart — the SAME
// derivation the fresh-start and pending-create-recovery paths use — so the
// relaunched agent resumes its tracked conversation (resolveSessionCommand adds
// --resume/--session-id), carries the runtime env (GC_SESSION_ID, instance
// token, GC_PROVIDER, trigger-bead env), and does NOT re-send the full startup
// prompt (the !firstStart prompt-strip + restart-nudge block). The drift
// COMPARISON still uses the hash-form sessionCoreConfigForHash; only the
// EXECUTED config and the rebaselined baselines come from buildPreparedStart.
//
// Returns (true, launchBatch) iff the agent was relaunched and hashes were
// rebaselined (the caller folds launchBatch onto the typed snapshot and
// `continue`s). Returns (false, fold) when the provider cannot relaunch,
// buildPreparedStart minted a speculative resume key (a warm relaunch would
// --resume a key naming a conversation that was never created), the
// prepare/precondition/relaunch step failed, or the rebaseline failed — the
// caller folds the prepare residue (buildPreparedStart mutates the raw bead:
// instance_token mint, stale-resume-key clear) and falls through to the full
// restart. The fold is nil only when no buildPreparedStart side effect ran (the
// RelaunchProvider gate rejected the runtime before any preparation).
//
// The deferral guards (attached / named-active / pending-interaction / open
// assigned work) are honored by the CALLER: this is invoked only after those
// guards have passed at each restart site, exactly where the full restart would
// otherwise fire — a respawn is as disruptive as a restart, so it earns the same
// protection.
func relaunchAgentForLaunchDrift(
	ctx context.Context,
	sp runtime.Provider,
	sessFront *sessionpkg.Store,
	session *beads.Bead,
	name string,
	tp TemplateParams,
	cityPath string,
	cfg *config.City,
	store beads.Store,
	storedHash, currentHash string,
	storedProvisionHash, storedLaunchHash string,
	driftedFields []string,
	rec events.Recorder,
	trace *sessionReconcilerTraceCycle,
	stdout, stderr io.Writer,
) (bool, map[string]string) {
	r, ok := sp.(runtime.RelaunchProvider)
	if !ok {
		// Conjoined runtimes (subprocess/acp/t3bridge) do not implement
		// RelaunchProvider; fall through to the full restart. No side effects yet —
		// no buildPreparedStart residue to fold.
		return false, nil
	}
	// Capture whether the bead already tracked a resumable conversation BEFORE
	// buildPreparedStart runs. An empty session_key means any key the preparation
	// mints below (line ~911, for a SessionIDFlag provider) is speculative: it
	// names a conversation the relaunch has not created yet. Such a speculative key
	// must never be executed as `--resume` and must never survive into the
	// full-restart fallback, or a future start would --resume a phantom
	// conversation. Both halves are enforced below: the minted-speculative-key guard
	// before Relaunch prevents execution, and relaunchAbortResidueFold clears the key
	// on every abort path.
	hadResumeKeyBeforePrepare := strings.TrimSpace(session.Metadata["session_key"]) != ""
	// Derive the executable config exactly as the fresh-start / pending-create
	// recovery paths do. cityPath resolves session.Metadata["work_dir"] against
	// the city; the nil work-dir resolver is correct because both call sites sit
	// behind the no-open-assigned-work / not-active deferral guards. Deliberately
	// buildPreparedStart*, NOT prepareStartCandidateForCity — the session is
	// alive, not waking, so no preWakeCommit / named-template refresh.
	prepared, err := buildPreparedStartWithWorkDirResolver(startCandidate{session: session, tp: tp}, cityPath, cfg, store, nil)
	if err != nil {
		fmt.Fprintf(stderr, "session reconciler: preparing relaunch config for %s: %v; falling back to full restart\n", name, err) //nolint:errcheck
		return false, relaunchAbortResidueFold(session, sessFront, hadResumeKeyBeforePrepare)
	}
	// Anti-skew gate: the launch-only-drift verdict was computed from the
	// hash-form config; relaunch only if it still holds for the prepared config.
	// A mismatch means a concurrent bead mutation or a derivation divergence
	// between the hash-form and prepared configs — take the full restart rather
	// than relaunch-then-rebaseline against an unverified baseline.
	if prepared.coreHash != currentHash || prepared.provisionHash != storedProvisionHash || prepared.launchHash == storedLaunchHash {
		fmt.Fprintf(stderr, "session reconciler: relaunch precondition skew for %s (core=%v provision=%v launch-unchanged=%v); falling back to full restart\n", //nolint:errcheck
			name, prepared.coreHash != currentHash, prepared.provisionHash != storedProvisionHash, prepared.launchHash == storedLaunchHash)
		return false, relaunchAbortResidueFold(session, sessFront, hadResumeKeyBeforePrepare)
	}
	// A warm-box relaunch resumes a TRACKED conversation. When the bead carried no
	// session_key before preparation but buildPreparedStart minted one — a
	// SessionIDFlag provider with no prior key (session_lifecycle_parallel.go:911)
	// — that key is speculative: started_config_hash is set, so firstStart is false
	// and resolveSessionCommand built `--resume <minted-key>` for a conversation
	// that was never created. Executing that relaunch resumes a phantom, and a
	// provider that reports success would then rebaseline and persist the minted
	// key, tying every future start to a conversation that does not exist. Fall back
	// to the full restart, which starts fresh; relaunchAbortResidueFold clears the
	// speculative key so resetConfiguredNamedSessionForConfigDrift's preserve-resume
	// gate cannot carry it forward.
	//
	// Scope this to an ACTUAL mint (session_key populated only during preparation),
	// not merely "no prior key": a provider that mints no key (nil resolver, no
	// SessionIDFlag) built no `--resume`, so its bare warm relaunch carries no
	// phantom and must still proceed. A merely-stale prior key is also unaffected —
	// buildPreparedStart cleared it and zeroed started_config_hash before
	// re-minting, so firstStart is true, the command is a fresh `--session-id`, and
	// hadResumeKeyBeforePrepare is true, so this guard does not fire.
	if mintedSpeculativeResumeKey := !hadResumeKeyBeforePrepare && strings.TrimSpace(session.Metadata["session_key"]) != ""; mintedSpeculativeResumeKey {
		fmt.Fprintf(stderr, "session reconciler: launch-drift relaunch for %s minted a speculative resume key (no prior conversation); falling back to full restart\n", name) //nolint:errcheck
		return false, relaunchAbortResidueFold(session, sessFront, hadResumeKeyBeforePrepare)
	}
	if err := r.Relaunch(ctx, name, prepared.cfg); err != nil {
		// ErrRelaunchUnsupported (a wrapper whose backend cannot relaunch) or a
		// genuine failure (e.g. the warm box vanished → ErrSessionNotFound). Fall
		// back to the full restart so the launch change is still applied.
		if !errors.Is(err, runtime.ErrRelaunchUnsupported) {
			fmt.Fprintf(stderr, "session reconciler: relaunch %s: %v; falling back to full restart\n", name, err) //nolint:errcheck
		}
		return false, relaunchAbortResidueFold(session, sessFront, hadResumeKeyBeforePrepare)
	}
	fmt.Fprintf(stdout, "Launch-only config change for '%s', relaunched agent in warm box\n", tp.DisplayName()) //nolint:errcheck
	// Rebaseline the Core baseline (started_config_hash) and the partition
	// sub-hashes so the next tick sees no Core drift. The hashes come from
	// buildPreparedStart's PRE-rewrite fingerprints (prepared.coreHash etc.), NOT
	// the executed prepared.cfg (which carries the --resume rewrite + runtime env,
	// neither a fingerprint input), so the baseline matches what the next tick's
	// sessionCoreConfigForHash comparison reproduces. started_live_hash is
	// DELIBERATELY left untouched: a relaunch MAY re-run SessionLive via the
	// shared orchestration tail (tmux and ssh do; k8s does not), so the live
	// half is not reliably re-applied here. Leaving the live hash alone keeps
	// this provider-independent — any concurrent live drift is re-applied
	// idempotently by the live-drift clause on the next tick (a redundant
	// SessionLive re-apply is harmless; a missed one self-heals).
	launchBatch, rebaseErr := rebaselineLaunchDriftHashesWithBatch(session, sessFront, prepared.coreHash, prepared.provisionHash, prepared.launchHash, prepared.coreBreakdown)
	if rebaseErr != nil {
		// The agent is already relaunched; do not trigger a second restart. The
		// stale Core baseline self-corrects on a later rebaseline tick. Fold the
		// prepare residue so the snapshot still matches the raw bead.
		fmt.Fprintf(stderr, "session reconciler: rebaselining launch-drift hashes for %s: %v\n", name, rebaseErr) //nolint:errcheck
		launchBatch = pendingCreateResidueFold(session)
	} else if tok := session.Metadata["instance_token"]; tok != "" && launchBatch != nil {
		// buildPreparedStart may mint instance_token onto the raw bead + store
		// (SetMarker) — a residue outside the rebaseline patch. Carry it in the
		// fold so the snapshot reflects it (mirrors pendingCreateResidueFold). Guard
		// the write on launchBatch != nil: rebaselineLaunchDriftHashesWithBatch
		// documents a (nil, nil) return when the session/front-door is nil, and a
		// write to a nil map panics.
		launchBatch["instance_token"] = tok
	}
	if trace != nil {
		trace.RecordDecision(TraceSiteReconcilerConfigDrift, TraceReasonConfigDrift, TraceOutcomeRelaunch, tp.TemplateName, name, configDriftTracePayload(storedHash, currentHash, driftedFields, nil))
	}
	rec.Record(events.Event{
		Type:    events.SessionUpdated,
		Actor:   "gc",
		Subject: tp.DisplayName(),
		Message: "agent relaunched (launch-only config change)",
	})
	return true, launchBatch
}

// relaunchAbortResidueFold is the buildPreparedStart residue fold for the paths
// that abort the launch-only-drift relaunch (prepare error, anti-skew skew, or
// relaunch failure) and fall back to the full restart. It exists to keep a
// speculatively-minted resume key from surviving the fallback.
//
// When the bead carried no session_key before preparation,
// buildPreparedStartWithWorkDirResolver minted one (persisting it to the raw
// bead + store via SetMarker) so it could build the relaunch command. That key
// names a conversation the aborted relaunch never created. Left in place,
// resetConfiguredNamedSessionForConfigDrift would see a non-empty session_key
// plus the stale started_config_hash and PRESERVE both, so the next start would
// --resume a phantom conversation instead of doing the fresh restart the
// fallback is meant to provide. Clear the speculative key exactly as
// buildPreparedStart's own stale-resume guard does (session_key +
// started_config_hash + continuation_reset_pending, raw bead + store), which the
// pendingCreateResidueFold below then folds onto the caller's snapshot.
//
// When a real resume key predated preparation, leave it untouched so the
// fallback resumes the prior conversation (the intended preserve-resume path).
func relaunchAbortResidueFold(session *beads.Bead, sessFront *sessionpkg.Store, hadResumeKeyBeforePrepare bool) map[string]string {
	if session != nil && !hadResumeKeyBeforePrepare && strings.TrimSpace(session.Metadata["session_key"]) != "" {
		clearStaleResumeKeyMetadata(session, sessFront)
	}
	return pendingCreateResidueFold(session)
}

// rebaselineLaunchDriftHashesWithBatch moves a session's Core drift baseline to
// the relaunched config after a successful warm-box relaunch —
// started_config_hash + the provision/launch sub-hashes + core_hash_breakdown —
// WITHOUT touching started_live_hash/live_hash. The relaunch re-applied the
// launch half (the agent now runs the prepared config); the provision half was
// unchanged by definition. The live hash is left untouched because relaunch does
// not reliably re-apply the live half (tmux/ssh re-run SessionLive via the
// shared orchestration tail; k8s does not), so a concurrent SessionLive change
// is re-applied idempotently by the live-drift clause on the next tick. Contrast
// sessionHashRebaselineMetadata, which rebaselines every field (used when the
// config did not actually change).
//
// The hashes are passed in explicitly (from buildPreparedStart's pre-rewrite
// fingerprints) rather than recomputed here: the executed config carries the
// resolveSessionCommand --resume/--session-id rewrite and runtime env, which are
// NOT fingerprint inputs, so the baseline must be the durable-config hashes the
// next tick's sessionCoreConfigForHash comparison will reproduce.
//
// Returns the mirrored patch on success so the caller can fold it onto the typed
// snapshot via ApplyPatch. Returns (nil, nil) when there is nothing to do (nil
// session/front-door), (nil, err) on any failure.
func rebaselineLaunchDriftHashesWithBatch(session *beads.Bead, sessFront *sessionpkg.Store, coreHash, provisionHash, launchHash string, breakdown runtime.BreakdownV1) (map[string]string, error) {
	if session == nil || sessFront == nil {
		return nil, nil
	}
	breakdownJSON, err := json.Marshal(breakdown)
	if err != nil {
		return nil, fmt.Errorf("marshaling core_hash_breakdown: %w", err)
	}
	patch := map[string]string{
		"started_config_hash":    coreHash,
		"started_provision_hash": provisionHash,
		"started_launch_hash":    launchHash,
		"core_hash_breakdown":    string(breakdownJSON),
	}
	if err := sessFront.ApplyPatch(session.ID, patch); err != nil {
		return nil, fmt.Errorf("rebaselining launch-drift hashes: %w", err)
	}
	// The caller folds the returned patch onto the typed snapshot (write-returns-
	// Info). These rebaselined hash fields are never read off the raw session bead
	// this tick — the drift decision reads Info and the start path re-reads the
	// hash off a fresh store.Get — so Step 5c dropped the raw session.Metadata
	// mirror.
	return patch, nil
}

// resolveSessionCommand returns the command to use when starting a session.
// Precedence on a first start: fork (parentSID present + provider supports it)
// > fresh (SessionIDFlag) > resume. The fork form resumes a parent brain
// session, forks it into a new conversation, and binds gc's own session key so
// all downstream tracking treats the child as a normal session. On any
// subsequent wake (firstStart=false) the fork branch is skipped and the forked
// child resumes via its own key. wake_mode=fresh still mints a new conversation
// via SessionIDFlag. Fork preconditions (provider support, parent staleness,
// wake_mode) are validated upstream in buildPreparedStartWithWorkDirResolver,
// which fails loud rather than ever silently degrading a fork to a fresh start.
func resolveSessionCommand(command, sessionKey, parentSID string, rp *config.ResolvedProvider, firstStart, forceFresh bool) string {
	// forceFresh is part of the fork guard so this branch is self-contained: a
	// fork resumes the parent brain, which contradicts the "discard context, start
	// new" intent of wake_mode=fresh. validateForkLaunch already fails loud on a
	// forceFresh fork upstream, but keeping the guard here means the function
	// honors its own docstring in isolation and is not a trap for future callers.
	if firstStart && !forceFresh && parentSID != "" && rp.ForkFlag != "" && rp.SessionIDFlag != "" {
		return command + " " + rp.ResumeFlag + " " + parentSID +
			" " + rp.ForkFlag + " " + rp.SessionIDFlag + " " + sessionKey
	}
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
