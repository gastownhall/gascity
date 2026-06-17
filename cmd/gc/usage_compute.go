package main

import (
	"context"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/usage"
)

// usageComputeEmittedAtKey marks the awake interval (by its awake_started_at
// value) whose compute Fact has already been recorded, so a later tick does
// not re-emit it. A new awake interval has a new awake_started_at, so emission
// across intervals is allowed.
const usageComputeEmittedAtKey = "usage_compute_emitted_at"

// isComputeTerminalState reports whether a session state marks the end of an
// awake interval, at which a compute fact should be emitted.
func isComputeTerminalState(state string) bool {
	switch session.State(strings.TrimSpace(state)) {
	case session.StateAsleep, session.StateDrained, session.StateArchived:
		return true
	}
	return false
}

// resolveComputeRunID mirrors the worker's run-root resolution for a session
// bead: workflow_id || molecule_id || gc.root_bead_id || bead id.
func resolveComputeRunID(bead beads.Bead) string {
	if bead.Metadata != nil {
		for _, k := range []string{"workflow_id", "molecule_id", beadmeta.RootBeadIDMetadataKey} {
			if v := strings.TrimSpace(bead.Metadata[k]); v != "" {
				return v
			}
		}
	}
	return strings.TrimSpace(bead.ID)
}

// emitComputeFactForBead records one compute Fact for a session bead's
// completed awake interval, exactly once per awake_started_at epoch. Returns
// true when a fact was recorded. It is a no-op when the sink is discard/nil,
// when there is no awake_started_at (the session never confirmed a start), or
// when the interval was already recorded.
//
// wall_seconds is measured from awake_started_at to slept_at when present (the
// graceful-sleep end), else to now (best-effort for other terminal transitions).
func emitComputeFactForBead(ctx context.Context, sink usage.Sink, store beads.Store, bead beads.Bead, runtimeKind, city string, now time.Time) bool {
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
	runID := strings.TrimSpace(meta["run_id"])
	if runID == "" {
		runID = resolveComputeRunID(bead)
	}
	fact := usage.Fact{
		RunID:          runID,
		StepID:         strings.TrimSpace(meta[beadmeta.ActiveWorkBeadMetadataKey]),
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
		// Leave the marker unset so a later tick retries; the durable LocalSink's
		// read-time dedup by IdempotencyKey backstops a partial double-emit.
		return false
	}
	// Single-key marker → atomic on every store impl.
	_ = store.SetMetadata(bead.ID, usageComputeEmittedAtKey, startRaw)
	return true
}

// emitDueComputeFacts scans the city's session beads and emits a compute
// Fact for any whose awake interval has ended (terminal state) and has not
// yet been recorded. Best-effort: it never blocks or fails the reconcile tick.
func (cr *CityRuntime) emitDueComputeFacts(ctx context.Context) {
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
	sessions, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		return
	}
	runtimeKind := ""
	if cr.cfg != nil {
		runtimeKind = cr.cfg.Session.Provider
	}
	now := time.Now().UTC()
	for _, b := range sessions {
		if b.Metadata == nil || !isComputeTerminalState(b.Metadata["state"]) {
			continue
		}
		emitComputeFactForBead(ctx, sink, store, b, runtimeKind, cr.cityName, now)
	}
}
