package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/telemetry"
)

// runStuckSweep iterates controller-owned running sessions, checks each
// against the stuckTracker predicate, and files a warrant bead for any
// session flagged as stuck. Idempotent per target: an existing open warrant
// for the same session suppresses duplicates.
//
// Fail-open semantics:
//   - One bd list --json shellout for wisp freshness per sweep. Failure
//     aborts the whole sweep (whole-sweep fail-open, not per-session).
//   - Peek / GetLastActivity errors skip the session and continue.
//   - agent.stuck event emitted ONLY on successful warrant Create.
func (cr *CityRuntime) runStuckSweep(ctx context.Context, now time.Time) {
	if cr == nil || cr.stuck == nil {
		return
	}
	// Short-circuit when stuck_sweep=false or stuck_warrant_label resolves
	// empty: StuckSweepEnabled() covers both cases. The noop tracker path
	// (noopStuckTracker) is never reached in M3 — the sweep is gated here
	// before any bd shellout. Zero patterns no longer results in noop;
	// the progress-mismatch axis remains active when patterns are absent.
	if !cr.cfg.Daemon.StuckSweepEnabled() {
		return
	}
	if cr.sp == nil {
		return
	}
	if ctx.Err() != nil {
		return
	}
	// Defense-in-depth halt gate (E14/R7): tick() already checks halt before
	// calling runStuckSweep, but we re-check here so direct unit invocations
	// honor the same ordering as production.
	if cr.halt.check(cr.cityPath, cr.stderr) {
		return
	}

	runner := cr.runner
	if runner == nil {
		runner = bdCommandRunnerForCity(cr.cityPath)
	}
	freshnessBySession, err := sweepWispFreshness(cr.cityPath, runner)
	if err != nil {
		fmt.Fprintf(cr.stderr, "%s: stuck sweep: %v (fail-open this tick)\n", //nolint:errcheck // best-effort stderr
			cr.logPrefix, err)
		return
	}

	running, err := cr.sp.ListRunning("")
	if err != nil {
		fmt.Fprintf(cr.stderr, "%s: stuck sweep: list running: %v\n", cr.logPrefix, err) //nolint:errcheck // best-effort stderr
		return
	}

	store := cr.cityBeadStore()
	// Route all tracker inputs through the stuckTracker interface and config
	// accessors rather than a concrete-type cast: this keeps alternative
	// implementations (e.g., noopStuckTracker) viable and keeps sweep behavior
	// aligned with the config source of truth.
	peekLines := cr.cfg.Daemon.StuckPeekLinesOrDefault()
	warrantLabel := cr.cfg.Daemon.StuckWarrantLabelOrDefault()

	for _, session := range running {
		if ctx.Err() != nil {
			return
		}

		freshness, hasWisp := freshnessBySession[session]
		if !hasWisp {
			continue
		}

		paneOutput, peekErr := cr.sp.Peek(session, peekLines)
		if peekErr != nil {
			fmt.Fprintf(cr.stderr, "%s: stuck sweep: peek %s: %v\n", cr.logPrefix, session, peekErr) //nolint:errcheck // best-effort stderr
			continue
		}

		lastActivity, actErr := cr.sp.GetLastActivity(session)
		if actErr != nil {
			// GetLastActivity error ⇒ insufficient evidence; fail-open.
			continue
		}

		stuck, reason, matchedPattern := cr.stuck.checkStuck(session, paneOutput,
			freshness.updatedAt, lastActivity, now)
		if !stuck {
			continue
		}

		// Idempotence: suppress if an open warrant already targets this
		// session. Reads store, not tracker state, so it survives reload.
		// Fail-open direction: on store error, treat as "already warranted"
		// to avoid duplicate flood while the store is transiently unavailable.
		if store != nil {
			already, lookupErr := hasOpenStuckWarrant(store, warrantLabel, session)
			if lookupErr != nil {
				fmt.Fprintf(cr.stderr, "%s: stuck sweep: warrant lookup %s: %v (skipping)\n", //nolint:errcheck // best-effort stderr
					cr.logPrefix, session, lookupErr)
				continue
			}
			if already {
				continue
			}
		}

		// Race check: if the process is already dead, the crashTracker
		// will handle it; do not file a warrant.
		if !cr.sp.ProcessAlive(session, nil) {
			continue
		}

		if store == nil {
			// No bead store available; log detection for observability.
			fmt.Fprintf(cr.stderr, "%s: stuck detected (no store): session=%s reason=%s\n", //nolint:errcheck // best-effort stderr
				cr.logPrefix, session, reason)
			continue
		}

		wispAge := now.Sub(freshness.updatedAt).Round(time.Second).String()
		warrant := beads.Bead{
			Title:  "stuck:" + session,
			Type:   "warrant",
			Labels: []string{warrantLabel},
			Metadata: map[string]string{
				"target":          session,
				"reason":          reason,
				"requester":       "controller",
				"matched_pattern": matchedPattern,
				"wisp_id":         freshness.id,
				"wisp_age":        wispAge,
			},
		}
		if _, createErr := store.Create(warrant); createErr != nil {
			fmt.Fprintf(cr.stderr, "%s: stuck sweep: filing warrant for %s: %v\n", //nolint:errcheck // best-effort stderr
				cr.logPrefix, session, createErr)
			continue
		}

		if cr.rec != nil {
			cr.rec.Record(events.Event{
				Type:    events.AgentStuck,
				Actor:   "controller",
				Subject: session,
				Message: reason,
			})
		}
		// axis is a low-cardinality summary: "regex" or "progress_mismatch".
		// The session attribute is intentionally included (consistent with
		// RecordAgentIdleKill) and may have high cardinality in large fleets —
		// operators should be aware. Full reason lives on the warrant bead.
		axis := "regex"
		if matchedPattern == "" {
			axis = "progress_mismatch"
		}
		telemetry.RecordAgentStuckWarrant(ctx, session, axis)
		fmt.Fprintf(cr.stderr, "%s: stuck warrant filed: session=%s reason=%s\n", //nolint:errcheck // best-effort stderr
			cr.logPrefix, session, reason)
	}
}

// hasOpenStuckWarrant reports whether the store already contains an open
// warrant bead with the given label and metadata.target matching session.
//
// Fail-open direction (R6): on ListByLabel error, returns (true, err). Treating
// an unknown-state query as "already warranted" prevents duplicate-warrant
// floods when the store is transiently unavailable. The caller should skip
// warrant creation (and optionally log the error) on error returns.
func hasOpenStuckWarrant(store beads.Store, label, session string) (bool, error) {
	beadsList, err := store.ListByLabel(label, 0)
	if err != nil {
		return true, err
	}
	for _, b := range beadsList {
		if b.Type != "warrant" {
			continue
		}
		if strings.TrimSpace(b.Metadata["target"]) == session {
			return true, nil
		}
	}
	return false, nil
}
