package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessions "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/telemetry"
)

// errTokenMismatch indicates the running session's instance token
// doesn't match the expected one — the session was re-woken by a
// different incarnation and this drain/stop is stale.
var errTokenMismatch = errors.New("instance token mismatch")

// preWakeCommit persists a new incarnation (generation + token) BEFORE
// starting the process. This is Phase 1 of the two-phase wake protocol.
// Returns the new generation and instance token on success.
func preWakeCommit(
	session *beads.Bead,
	store beads.Store,
	clk clock.Clock,
) (newGen int, token string, err error) {
	name := session.Metadata["session_name"]
	if !sessions.IsSessionNameSyntaxValid(name) {
		return 0, "", fmt.Errorf("invalid session_name %q", name)
	}

	gen, _ := strconv.Atoi(session.Metadata["generation"])
	newGen = gen + 1
	token = sessions.NewInstanceToken()
	continuationEpoch, _ := strconv.Atoi(session.Metadata["continuation_epoch"])
	if continuationEpoch <= 0 {
		continuationEpoch = sessions.DefaultContinuationEpoch
	}
	if shouldBumpContinuationEpoch(session.Metadata) {
		continuationEpoch++
	}

	sleepReason := ""
	if session.Metadata["sleep_reason"] == "idle-timeout" {
		// Preserve the idle-timeout wake override until the replacement
		// session has actually started. Failed starts must retry next tick.
		sleepReason = "idle-timeout"
	}

	// Use one batched metadata update to avoid paying multiple bd update
	// round-trips before every wake.
	batch := map[string]string{
		"instance_token":             token,
		"continuation_epoch":         strconv.Itoa(continuationEpoch),
		"continuation_reset_pending": "",
		"detached_at":                "",
		"last_woke_at":               clk.Now().UTC().Format(time.RFC3339),
		"sleep_reason":               sleepReason,
		"sleep_intent":               "",
		"generation":                 strconv.Itoa(newGen),
	}
	if writeErr := store.SetMetadataBatch(session.ID, batch); writeErr != nil {
		return 0, "", fmt.Errorf("pre-wake metadata commit: %w", writeErr)
	}
	if session.Metadata == nil {
		session.Metadata = make(map[string]string, len(batch))
	}
	for k, v := range batch {
		session.Metadata[k] = v
	}

	return newGen, token, nil
}

func shouldBumpContinuationEpoch(meta map[string]string) bool {
	if meta == nil {
		return false
	}
	if meta["continuation_reset_pending"] != "" {
		return true
	}
	return meta["wake_mode"] == "fresh" && meta["last_woke_at"] != ""
}

// validateWorkDir ensures the path is safe to use as a working directory.
func validateWorkDir(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if abs != filepath.Clean(abs) {
		return fmt.Errorf("non-canonical path")
	}
	info, err := os.Stat(abs)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("not a directory")
	}
	return nil
}

// beginSessionDrain initiates an async drain. Returns immediately.
// The drainTracker stores in-memory state; advanceSessionDrains progresses it.
//
// The interrupt signal (Ctrl-C) is NOT sent immediately. It is deferred to
// the next reconciler tick via advanceSessionDrains. This gives the drain
// one full tick to be canceled (e.g., if the session was falsely orphaned
// due to a transient store failure) before any signal reaches the process.
// Without this, a single bad tick can interrupt a working agent mid-tool-call.
func beginSessionDrain(
	session beads.Bead,
	_ runtime.Provider, // kept for caller compatibility; interrupt deferred to advanceSessionDrains
	dt *drainTracker,
	reason string,
	clk clock.Clock,
	timeout time.Duration,
) {
	if dt.get(session.ID) != nil {
		return // already draining
	}
	gen, _ := strconv.Atoi(session.Metadata["generation"])

	dt.set(session.ID, &drainState{
		startedAt:  clk.Now(),
		deadline:   clk.Now().Add(timeout),
		reason:     reason,
		generation: gen,
	})

	name := session.Metadata["session_name"]
	if os.Getenv("GC_TMUX_TRACE") == "1" {
		log.Printf("[DRAIN-TRACE] beginSessionDrain session=%s reason=%s", name, reason)
	}
	telemetry.RecordDrainTransition(context.Background(), name, reason, "begin")
}

// cancelSessionDrain removes a drain if wake reasons reappeared for the same generation.
// If GC_DRAIN_ACK was already set by the reconciler (deferred drain signal),
// it is cleared so the Phase 1 drain-ack check doesn't kill the session.
func cancelSessionDrain(session beads.Bead, sp runtime.Provider, dt *drainTracker) bool {
	ds := dt.get(session.ID)
	if ds == nil {
		return false
	}
	gen, _ := strconv.Atoi(session.Metadata["generation"])
	if gen == ds.generation {
		dt.clearIdleProbe(session.ID)
		dt.remove(session.ID)
		name := session.Metadata["session_name"]
		// Clear GC_DRAIN_ACK if it was set — prevents stale ack from
		// killing the session on the next Phase 1 drain-ack check.
		if ds.ackSet {
			_ = sp.RemoveMeta(name, "GC_DRAIN_ACK")
			_ = sp.RemoveMeta(name, "GC_DRAIN")
		}
		telemetry.RecordDrainTransition(context.Background(), name, ds.reason, "cancel")
		return true
	}
	return false
}

// advanceSessionDrains checks all in-progress drains. Called once per tick.
//
//nolint:unparam // workSet is nil in the drain path; WakeWork flows via ComputeAwakeSet instead
func advanceSessionDrains(
	dt *drainTracker,
	sp runtime.Provider,
	store beads.Store,
	sessionLookup func(id string) *beads.Bead,
	cfg *config.City,
	poolDesired map[string]int,
	workSet map[string]bool,
	readyWaitSet map[string]bool,
	clk clock.Clock,
) {
	var sessions []beads.Bead
	for id := range dt.all() {
		if session := sessionLookup(id); session != nil {
			sessions = append(sessions, *session)
		}
	}
	advanceSessionDrainsWithSessions(dt, sp, store, sessionLookup, sessions, nil, cfg, poolDesired, workSet, readyWaitSet, clk)
}

func advanceSessionDrainsWithSessions(
	dt *drainTracker,
	sp runtime.Provider,
	store beads.Store,
	sessionLookup func(id string) *beads.Bead,
	sessions []beads.Bead,
	wakeEvals map[string]wakeEvaluation,
	cfg *config.City,
	poolDesired map[string]int,
	workSet map[string]bool,
	readyWaitSet map[string]bool,
	clk clock.Clock,
) {
	advanceSessionDrainsWithSessionsTraced(dt, sp, store, sessionLookup, sessions, wakeEvals, cfg, poolDesired, workSet, readyWaitSet, clk, nil)
}

func advanceSessionDrainsWithSessionsTraced(
	dt *drainTracker,
	sp runtime.Provider,
	store beads.Store,
	sessionLookup func(id string) *beads.Bead,
	sessions []beads.Bead,
	wakeEvals map[string]wakeEvaluation,
	cfg *config.City,
	poolDesired map[string]int,
	workSet map[string]bool,
	readyWaitSet map[string]bool,
	clk clock.Clock,
	trace *sessionReconcilerTraceCycle,
) {
	if wakeEvals == nil {
		wakeEvals = computeWakeEvaluations(sessions, cfg, sp, poolDesired, workSet, readyWaitSet, clk)
	}
	for id, ds := range dt.all() {
		session := sessionLookup(id)
		if session == nil {
			dt.clearIdleProbe(id)
			dt.remove(id)
			continue
		}
		name := session.Metadata["session_name"]

		// Stale check: if session was re-woken (generation changed), cancel drain.
		gen, _ := strconv.Atoi(session.Metadata["generation"])
		if gen != ds.generation {
			dt.clearIdleProbe(id)
			dt.remove(id)
			if trace != nil {
				trace.recordDecision("reconciler.drain.stale", normalizedSessionTemplate(*session, cfg), name, "stale_generation", "cancel", traceRecordPayload{
					"drain_reason":       ds.reason,
					"drain_generation":   ds.generation,
					"session_generation": gen,
				}, nil, "")
			}
			continue
		}

		// Check if process exited.
		if !sp.IsRunning(name) {
			// Process exited — drain complete.
			completeDrain(session, store, ds, clk)
			dt.clearIdleProbe(id)
			dt.remove(id)
			telemetry.RecordDrainTransition(context.Background(), name, ds.reason, "complete")
			if trace != nil {
				trace.recordDecision("reconciler.drain.complete", normalizedSessionTemplate(*session, cfg), name, ds.reason, "complete", traceRecordPayload{
					"drain_started_at": ds.startedAt,
				}, nil, "")
			}
			continue
		}

		// Cancelation check: if wake reasons reappeared, cancel drain.
		// Config-drift, orphaned, and suspended drains are NOT cancelable —
		// they represent explicit lifecycle decisions that should not be
		// reversed by the wake contract (the session is leaving the desired set).
		if ds.reason != "config-drift" && ds.reason != "orphaned" && ds.reason != "suspended" {
			if eval, ok := wakeEvals[session.ID]; ok && len(eval.Reasons) > 0 {
				dt.clearIdleProbe(id)
				// Clear GC_DRAIN_ACK if it was set — prevents stale ack
				// from killing the session on the next Phase 1 check.
				if ds.ackSet {
					_ = sp.RemoveMeta(name, "GC_DRAIN_ACK")
					_ = sp.RemoveMeta(name, "GC_DRAIN")
				}
				dt.remove(id)
				if trace != nil {
					trace.recordDecision("reconciler.drain.cancel", normalizedSessionTemplate(*session, cfg), name, ds.reason, "cancel", nil, nil, "")
				}
				continue
			}
		}

		// Deferred drain signal: set GC_DRAIN_ACK after the drain has survived
		// at least one full tick without being canceled. This prevents a
		// single transient store failure from interrupting a working agent
		// — the false-orphan drain is canceled on the next tick when the
		// store recovers, before any signal is set.
		//
		// Uses the same GC_DRAIN_ACK env var that agents set via
		// `gc runtime drain-ack`. The reconciler's Phase 1 drain-ack check
		// sees it on the next tick and calls sp.Stop() for a clean
		// SIGTERM/SIGKILL — no Ctrl-C keystroke injection into the pane.
		if !ds.ackSet {
			if os.Getenv("GC_TMUX_TRACE") == "1" {
				log.Printf("[DRAIN-TRACE] advanceSessionDrains: setting GC_DRAIN_ACK session=%s reason=%s", name, ds.reason)
			}
			err := sp.SetMeta(name, "GC_DRAIN_ACK", "1")
			ds.ackSet = true
			ds.followUp = true
			if trace != nil {
				outcome := "success"
				fields := traceRecordPayload{
					"reason":          ds.reason,
					"deferred_signal": true,
				}
				if err != nil {
					outcome = "failed"
					fields["error"] = err.Error()
				}
				trace.recordMutation("runtime_meta", normalizedSessionTemplate(*session, cfg), name, "provider_meta", name, "GC_DRAIN_ACK", "", "1", outcome, fields, "")
			}
		}

		if clk.Now().After(ds.deadline) {
			// Drain timed out — force stop.
			if err := verifiedStop(*session, sp); err != nil {
				if errors.Is(err, errTokenMismatch) {
					// Session was re-woken by a different incarnation.
					// This drain is stale — cancel it.
					dt.clearIdleProbe(id)
					dt.remove(id)
				}
				// Other errors (transient stop failure): keep drain
				// active for retry on next tick.
				if trace != nil {
					trace.recordDecision("reconciler.drain.timeout", normalizedSessionTemplate(*session, cfg), name, ds.reason, "retry", traceRecordPayload{
						"error": err.Error(),
					}, nil, "")
				}
				continue
			}
			// Re-probe after stop to confirm process actually exited
			// before marking metadata as asleep.
			if !sp.IsRunning(name) {
				completeDrain(session, store, ds, clk)
				dt.clearIdleProbe(id)
				dt.remove(id)
				telemetry.RecordDrainTransition(context.Background(), name, ds.reason, "timeout")
				if trace != nil {
					trace.recordDecision("reconciler.drain.timeout", normalizedSessionTemplate(*session, cfg), name, ds.reason, "complete", nil, nil, "")
				}
			}
			// If still running after stop, keep drain for next tick.
		}
		// Else: still draining, check again next tick.
	}
}

// completeDrain writes drain-complete metadata to the bead.
func completeDrain(session *beads.Bead, store beads.Store, ds *drainState, clk clock.Clock) {
	batch := map[string]string{
		"slept_at":     clk.Now().UTC().Format(time.RFC3339),
		"sleep_reason": ds.reason,
		"sleep_intent": "",
		"state":        "asleep",
		"last_woke_at": "", // Clear to prevent false crash detection.
	}
	if session.Metadata["wake_mode"] == "fresh" {
		batch["session_key"] = ""
		batch["started_config_hash"] = ""
		batch["continuation_reset_pending"] = "true"
	}
	if err := store.SetMetadataBatch(session.ID, batch); err == nil {
		if session.Metadata == nil {
			session.Metadata = make(map[string]string)
		}
		for k, v := range batch {
			session.Metadata[k] = v
		}
	}
}

// verifiedStop stops a session after verifying the instance_token matches.
// Prevents stale drain operations from targeting a re-woken session.
// Returns errTokenMismatch if the running process has a different token.
//
// NOTE: On composite providers (auto/hybrid), GetMeta and Stop may route
// to different backends if the route table is stale. This is a pre-existing
// routing limitation — when the reconciler is wired in, consider a
// provider-level VerifiedStop that atomically verifies+stops on the same backend.
func verifiedStop(session beads.Bead, sp runtime.Provider) error {
	name := session.Metadata["session_name"]
	expectedToken := session.Metadata["instance_token"]
	if expectedToken != "" {
		actualToken, _ := sp.GetMeta(name, "GC_INSTANCE_TOKEN")
		if actualToken != "" && actualToken != expectedToken {
			return fmt.Errorf("%w for session %s", errTokenMismatch, session.ID)
		}
	}
	return sp.Stop(name)
}

// verifiedInterrupt sends an interrupt signal after verifying instance_token.
func verifiedInterrupt(session beads.Bead, sp runtime.Provider) error {
	name := session.Metadata["session_name"]
	expectedToken := session.Metadata["instance_token"]
	if expectedToken != "" {
		actualToken, _ := sp.GetMeta(name, "GC_INSTANCE_TOKEN")
		if actualToken != "" && actualToken != expectedToken {
			return fmt.Errorf("%w for session %s", errTokenMismatch, session.ID)
		}
	}
	return sp.Interrupt(name)
}
