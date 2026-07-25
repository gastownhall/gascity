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
	"github.com/gastownhall/gascity/internal/worker"
)

// usageComputeEmittedAtKey marks the awake interval (by its awake_started_at
// value) whose compute Fact has already been recorded, so a later tick does
// not re-emit it. A new awake interval has a new awake_started_at, so emission
// across intervals is allowed.
const usageComputeEmittedAtKey = "usage_compute_emitted_at"

// usageModelSweptAtKey marks the awake interval (by its awake_started_at value)
// whose model-usage sweep has settled — recorded independently of the compute
// marker so a transient sweep miss (transcript not yet flushed, extraction tear,
// or sink failure) retries on the next tick instead of being permanently lost
// with the compute fact. Once stamped, the sweep is not re-run for the interval.
// Non-gc-prefixed to match its sibling usageComputeEmittedAtKey (both are
// session-interval accounting markers, not domain metadata).
const usageModelSweptAtKey = "usage_model_swept_at"

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
// commit governs the interval-accounting side effects, decoupled from the fact
// write so the model-usage sweep can retry across ticks: when commit is true the
// usage_compute_emitted_at marker is stamped (closing the interval to further
// Gets); when false the fact is still recorded but the interval stays open, so a
// caller that has not yet settled the model sweep leaves the session a candidate
// for the next tick. Re-recording the fact on a later tick is collapsed by
// ComputeIdempotencyKey at read time.
//
// SessionID is stamped from bead.ID so compute facts carry the same session
// bead join key as model facts.
//
// wall_seconds is measured from awake_started_at to slept_at when present (the
// graceful-sleep end), else to now (best-effort for other terminal transitions).
//
// RunID is resolved from the session bead's own run chain (workflow_id ||
// molecule_id || gc.root_bead_id-or-self || bead id). Per-work-bead attribution
// is deferred until a dispatch/claim writer exists, so pooled sessions roll up
// per-session for now (see engdocs/design/usage-facts-v0.md).
func emitComputeFactForBead(ctx context.Context, sink usage.Sink, store beads.Store, bead beads.Bead, runtimeKind, city string, now time.Time, logf func(string, ...any), commit bool) bool {
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
		RunID: runID,
		// The reconcile snapshot hands us the session bead directly, so bead.ID IS
		// the session bead id — the same value RunID resolution and the idempotency
		// key already consume below. Stamp it so compute facts carry the session
		// join key symmetrically with model facts (a session-keyed cost rollup must
		// union both Kinds; an unset SessionID here would silently drop compute/wall
		// cost from the join).
		SessionID:      strings.TrimSpace(bead.ID),
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
	if !commit {
		// The fact is durably recorded, but the interval is intentionally left open
		// (marker unset) so the model-usage sweep retries on a later tick. The
		// re-recorded fact is collapsed by IdempotencyKey.
		return true
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

// computeFactGetCandidate reports whether a session is worth a per-session store Get for
// a compute Fact, decided purely from its Info projection — BEFORE any Get. A session
// qualifies only when it is in a compute-terminal state, has an awake interval to account
// (awake_started_at set), and that interval is not already recorded
// (usage_compute_emitted_at != awake_started_at). This is the same short-circuit
// emitComputeFactForBead applies AFTER the Get, hoisted onto Info so a parked (idle/
// asleep) session whose interval is already accounted costs zero Gets — the common steady
// state. It is the pure, testable gate behind emitDueComputeFacts's per-session Get.
func computeFactGetCandidate(info session.Info) bool {
	if !isComputeTerminalState(info.MetadataState) {
		return false
	}
	start := strings.TrimSpace(info.AwakeStartedAt)
	if start == "" {
		return false
	}
	return strings.TrimSpace(info.UsageComputeEmittedAt) != start
}

// emitDueComputeFacts emits a compute Fact for any of the given open sessions whose
// awake interval has ended (terminal state) and has not yet been recorded. It reuses the
// reconcile tick's already-loaded Info snapshot for the cheap candidate filter
// (computeFactGetCandidate), then fetches the raw bead ONLY for the few sessions that
// pass it: the usage lane genuinely needs the whole bead (ResolveRunID walks the
// run-chain keys, and slept_at is not projected onto session.Info), so this is the usage
// lane's OWN edge read rather than a snapshot raw-half read. A steady fleet of parked
// sessions whose intervals are already accounted issues zero Gets. Best-effort: it never
// blocks or fails the reconcile tick.
func (cr *CityRuntime) emitDueComputeFacts(ctx context.Context, sessions []session.Info) {
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
	// Lazily built worker factory for the end-of-interval model-usage sweep. It is
	// constructed at most once per tick, and only when a terminal session actually
	// needs it, so a steady fleet of parked sessions builds nothing. A build
	// failure (or nil cfg) degrades to compute-only accounting for this tick.
	var (
		sweepFactory      *worker.Factory
		sweepFactoryTried bool
	)
	modelSweepFactory := func() *worker.Factory {
		if sweepFactoryTried {
			return sweepFactory
		}
		sweepFactoryTried = true
		if cr.cfg == nil {
			return nil
		}
		f, ferr := workerFactoryWithConfig(cr.cityPath, store, cr.sp, cr.cfg)
		if ferr != nil {
			logf("usage: building worker factory for model-usage sweep failed: %v", ferr)
			return nil
		}
		sweepFactory = f
		return sweepFactory
	}
	now := time.Now().UTC()
	for _, info := range sessions {
		if !computeFactGetCandidate(info) {
			continue
		}
		b, err := store.Get(info.ID)
		if err != nil {
			logf("usage: loading session %s for compute fact failed: %v", info.ID, err)
			continue
		}
		// Re-check the terminal state from the FRESH bead: a session that re-awoke in
		// the window since the snapshot was taken must not mint a tiny-wall fact for its
		// just-STARTED interval and suppress the real end-of-interval emission. Best-
		// effort accounting, the same NDI class as the sync-tail re-list delta.
		if b.Metadata == nil || !isComputeTerminalState(b.Metadata["state"]) {
			continue
		}
		awakeStart := strings.TrimSpace(b.Metadata["awake_started_at"])
		// Model-usage lane FIRST, symmetric to and beside the compute fact: recover the
		// terminal interval's trailing model-token usage that the prompt-op seam never
		// recorded (pool-routed, hook-self-driven agents self-drive after the claim
		// nudge). It runs before the compute commit so its settle result gates whether
		// the interval closes this tick. Best-effort — a sweep error never
		// fails the reconcile tick; overlap with the prompt-op seam is collapsed at read
		// time by the shared usage.ModelIdempotencyKey.
		//
		// The sweep is gated by its OWN per-interval marker (usageModelSweptAtKey),
		// distinct from the compute marker, so a transient miss retries on a later tick
		// instead of being lost. sweepSettled defaults true so a nil factory / no-op
		// sink never blocks the compute commit.
		sweepSettled := true
		if factory := modelSweepFactory(); factory != nil && awakeStart != "" &&
			strings.TrimSpace(b.Metadata[usageModelSweptAtKey]) != awakeStart {
			_, settled, serr := factory.SweepSessionModelUsage(ctx, b.ID, b.Metadata, now)
			if serr != nil {
				logf("usage: model-usage sweep for session %s failed; will retry: %v", b.ID, serr)
			}
			sweepSettled = settled
			if settled {
				if merr := store.SetMetadata(b.ID, usageModelSweptAtKey, awakeStart); merr != nil {
					logf("usage: marking model-usage swept for session %s failed; may re-sweep (deduped by idempotency key): %v", b.ID, merr)
				}
			}
		}
		// Commit the interval (stamp usage_compute_emitted_at) only once the sweep
		// has settled — an unsettled sweep leaves the interval a
		// candidate so both lanes retry next tick. The compute fact itself is always
		// recorded (idempotent), so wall-time accounting is never delayed by a pending
		// sweep.
		emitComputeFactForBead(ctx, sink, store, b, runtimeKind, cr.cityName, now, logf, sweepSettled)
	}
}
