package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/usage"
)

// usageComputeEmittedAtKey marks the awake interval (by its awake_started_at
// value) whose compute Fact has already been recorded, so a later tick does
// not re-emit it. A new awake interval has a new awake_started_at, so emission
// across intervals is allowed.
const usageComputeEmittedAtKey = "usage_compute_emitted_at"

// isComputeTerminalState reports whether a session state marks the end of an
// awake interval, at which a compute fact should be emitted. It covers every
// non-running lifecycle endpoint the controller's open-bead scan can observe:
// idle-sleep (asleep), controller drain (drained), retirement (archived),
// operator suspend (suspended), and crash-loop quarantine (quarantined). A
// session closed directly from active without first passing through one of
// these open states is the known v0 scan limitation (see
// engdocs/design/usage-facts-v0.md).
func isComputeTerminalState(state string) bool {
	switch session.State(strings.TrimSpace(state)) {
	case session.StateAsleep, session.StateDrained, session.StateArchived,
		session.StateSuspended, session.StateQuarantined:
		return true
	}
	return false
}

// emitComputeFactForBead records one compute Fact for a session bead's
// completed awake interval, exactly once per awake_started_at epoch. Returns
// true when a fact was recorded. It is a no-op when the sink is discard/nil,
// when there is no awake_started_at (the session never confirmed a start), or
// when the interval was already recorded. Sink and marker write failures are
// reported through logf (when non-nil) rather than dropped silently.
//
// wall_seconds is measured from awake_started_at to slept_at when present (the
// graceful-sleep end), else to now (best-effort for other terminal transitions).
//
// RunID is resolved from the session bead's own run chain (workflow_id ||
// molecule_id || gc.root_bead_id-or-self || bead id). Per-work-bead attribution
// is deferred until a dispatch/claim writer exists, so pooled sessions roll up
// per-session for now (see engdocs/design/usage-facts-v0.md).
func emitComputeFactForBead(ctx context.Context, sink usage.Sink, store beads.Store, bead beads.Bead, runtimeKind, city string, now time.Time, logf func(string, ...any)) bool {
	if sink == nil || sink == usage.Discard || store == nil {
		return false
	}
	meta := bead.Metadata
	if meta == nil {
		return false
	}
	startRaw := strings.TrimSpace(meta["awake_started_at"])
	if startRaw == "" {
		return false
	}
	if strings.TrimSpace(meta[usageComputeEmittedAtKey]) == startRaw {
		return false // already emitted this interval
	}
	startedAt, err := time.Parse(time.RFC3339, startRaw)
	if err != nil {
		return false
	}
	// Prefer the recorded sleep time as the interval end, but only when it falls
	// after this interval's start — slept_at can be stale for non-sleep terminal
	// states (drained/archived) that don't refresh it. Otherwise use now.
	end := now
	if sleptRaw := strings.TrimSpace(meta["slept_at"]); sleptRaw != "" {
		if t, perr := time.Parse(time.RFC3339, sleptRaw); perr == nil && t.After(startedAt) {
			end = t
		}
	}
	wall := end.Sub(startedAt).Seconds()
	if wall < 0 {
		wall = 0
	}
	runID := beadmeta.ResolveRunID(bead.Metadata, bead.ID, "")
	fact := usage.Fact{
		RunID:          runID,
		Worker:         strings.TrimSpace(meta["session_name"]),
		City:           city,
		Kind:           usage.KindCompute,
		Runtime:        runtimeKind,
		WallSeconds:    wall,
		UpstreamReqID:  bead.ID + ":" + startRaw,
		At:             now.UnixMilli(),
		IdempotencyKey: usage.ComputeIdempotencyKey(runID, bead.ID, startRaw),
	}
	if err := sink.Record(ctx, fact); err != nil {
		// Surface the failure instead of dropping it silently; leave the marker
		// unset so a later tick retries. The durable LocalSink's read-time dedup
		// by IdempotencyKey backstops a partial double-emit.
		if logf != nil {
			logf("usage: recording compute fact for session %s failed; will retry next tick: %v", bead.ID, err)
		}
		return false
	}
	// Single-key marker → atomic on every store impl.
	if err := store.SetMetadata(bead.ID, usageComputeEmittedAtKey, startRaw); err != nil {
		// The fact is durably recorded; a missed marker only risks a re-emit that
		// IdempotencyKey collapses at read time. Still surface it.
		if logf != nil {
			logf("usage: marking compute fact emitted for session %s failed; may re-emit (deduped by idempotency key): %v", bead.ID, err)
		}
	}
	return true
}

// emitDueComputeFacts emits a compute Fact for any of the given session beads
// whose awake interval has ended (terminal state) and has not yet been
// recorded. It reuses the reconcile tick's already-loaded open-session snapshot
// rather than issuing its own redundant store scan. Best-effort: it never blocks
// or fails the reconcile tick.
func (cr *CityRuntime) emitDueComputeFacts(ctx context.Context, sessions []beads.Bead) {
	if cr.cs == nil {
		return
	}
	sink := cr.cs.UsageSink()
	if sink == nil || sink == usage.Discard {
		return
	}
	store := cr.cityBeadStore()
	if store == nil {
		return
	}
	runtimeKind := ""
	if cr.cfg != nil {
		runtimeKind = cr.cfg.Session.Provider
	}
	// Throttle sink-failure noise: a persistently broken sink would otherwise log
	// once per terminal bead per tick. One line per tick is enough signal that
	// the sink is failing without flooding the controller log.
	logged := false
	logf := func(format string, args ...any) {
		if logged || cr.stderr == nil {
			return
		}
		logged = true
		fmt.Fprintf(cr.stderr, format+"\n", args...) //nolint:errcheck // best-effort stderr
	}
	now := time.Now().UTC()
	for _, b := range sessions {
		if b.Metadata == nil || !isComputeTerminalState(b.Metadata["state"]) {
			continue
		}
		emitComputeFactForBead(ctx, sink, store, b, runtimeKind, cr.cityName, now, logf)
	}
}
