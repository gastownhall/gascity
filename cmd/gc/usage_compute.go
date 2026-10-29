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

// liveModelSweepMinInterval floors how often the reconcile tick re-sweeps one
// awake session's transcript for model usage. The terminal lane is gated by a
// persisted per-interval marker, so it touches each session once; a live session
// has no such endpoint and is a candidate on EVERY tick, which makes the live
// lane's cost fleet-proportional AND repeated at the tick cadence — a bounded
// rollout discovery scan plus a transcript tail read per awake session, on the
// SYNCHRONOUS reconcile tick. Without a floor, a poke-driven sub-second cadence
// turns that into per-tick file I/O across the whole live fleet. Thirty seconds
// is far below the interval-scale staleness this lane exists to fix (usage
// previously appeared only at retirement, hours later) and far above the tick
// cadence that produces the storm; nothing is lost by waiting, because the
// cursor-guarded sweep bills the whole batch pending at the moment it next runs.
const liveModelSweepMinInterval = 30 * time.Second

// closedIntervalSweepGiveUp is how long a closed row's trailing model-usage
// sweep may stay unsettled before the settle gives up on it: past this
// deadline the compute interval is committed without the trailing model
// facts, which a keyed transcript that never appeared can no longer yield.
const closedIntervalSweepGiveUp = 15 * time.Minute

// isComputeTerminalState reports whether a session state marks the end of an
// awake interval, at which a compute fact should be emitted. It covers every
// non-running lifecycle endpoint the controller's open-bead scan can observe:
// idle-sleep (asleep), controller drain (drained), retirement (archived),
// operator suspend (suspended), and crash-loop quarantine (quarantined). A
// session closed directly from active without first passing through one of
// these open states never shows here; its interval is settled by the
// closed-interval lane (settleClosedInterval, issue #6672), which the close's
// usage_close_pending marker feeds.
func isComputeTerminalState(state string) bool {
	switch session.State(strings.TrimSpace(state)) {
	case session.StateAsleep, session.StateDrained, session.StateArchived,
		session.StateSuspended, session.StateQuarantined:
		return true
	}
	return false
}

// isLiveModelSweepState reports whether a session is currently awake and may
// still append model invocations to its transcript. It is deliberately
// disjoint from isComputeTerminalState.
func isLiveModelSweepState(state string) bool {
	switch session.State(strings.TrimSpace(state)) {
	case session.StateActive, session.StateAwake:
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
// wall_seconds is measured from awake_started_at to the interval end
// session.UsageIntervalEnd picks from the bead's metadata: the close anchor
// when the row's close ended the interval, else slept_at when present (the
// graceful-sleep end), else now (best-effort for other terminal transitions).
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
	// The end follows session.UsageIntervalEnd: the close anchor when the
	// row's close ended this interval, else slept_at when it falls after the
	// start (the drain-ack and graceful-sleep ends, more accurate than the
	// tick's now), else now. The After guards reject a slept_at carried over
	// from a PRIOR awake interval (archive and quarantine exits don't refresh
	// it) and an anchor from a prior interval a reopen kept.
	end := session.UsageIntervalEnd(meta, startedAt, now)
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

// liveModelSweepCandidate reports whether an open snapshot row is worth
// loading for an incremental transcript sweep. Unlike terminal compute
// accounting, a live session remains a candidate every tick; the persisted
// invocation cursor makes repeated sweeps idempotent.
func liveModelSweepCandidate(info session.Info) bool {
	return isLiveModelSweepState(info.MetadataState) &&
		strings.TrimSpace(info.AwakeStartedAt) != ""
}

// emitDueComputeFacts accounts for terminal compute intervals and incrementally
// sweeps model usage from awake sessions. It reuses the reconcile tick's already-loaded
// Info snapshot for the cheap candidate filters (computeFactGetCandidate,
// liveModelSweepCandidate), then fetches the raw bead ONLY for the few sessions that
// pass one: the usage lane genuinely needs the whole bead (ResolveRunID walks the
// run-chain keys, and neither slept_at nor the transcript cursor is projected onto
// session.Info), so this is the usage lane's OWN edge read rather than a snapshot
// raw-half read. A steady fleet of parked sessions whose intervals are already
// accounted issues zero Gets. It also settles the awake intervals a close ended
// straight from an awake state (issue #6672), on closed rows the close marked —
// a listing filtered by that marker, not closed history. Best-effort: it never
// blocks or fails the reconcile tick.
//
// bootReconcile disables the live lane and the marked closed listing. The
// terminal lane's cost is unchanged by boot — it is gated by a persisted
// per-interval marker, so it fires once per interval whenever the pass runs —
// and a marked open row settles on boot through the open loop it already
// feeds, but the live lane's transcript discovery and reads are proportional
// to the awake fleet, and the closed listing is one more store round trip
// the boot pass covers on the synchronous readiness path. Deferring both to
// the first steady-state tick costs one tick of billing latency and keeps
// startup off the critical path (the same trade beadReconcileTick makes for
// the pool sweep).
func (cr *CityRuntime) emitDueComputeFacts(ctx context.Context, sessions []session.Info, bootReconcile bool) {
	if cr.cs == nil {
		return
	}
	sink := cr.cs.UsageSink()
	if sink == nil || sink == usage.Discard {
		return
	}
	// Every bead this lane touches is a session bead (Get by session id,
	// SetMetadata of the usage markers), so it reads the sessions class, not the
	// work store. Identity to the work store on a city that relocates nothing.
	store := cr.sessionsBeadStore().Store
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
		f, ferr := workerFactoryWithAttribution(cr.cityPath, store, cr.sp, cr.cfg, nil,
			cr.formulaBeadResolver(cr.cfg, nil), cr.formulaMemo())
		if ferr != nil {
			logf("usage: building worker factory for model-usage sweep failed: %v", ferr)
			return nil
		}
		sweepFactory = f
		return sweepFactory
	}
	now := time.Now().UTC()
	liveLane := !bootReconcile
	processSessionBead := func(b beads.Bead) {
		if b.Metadata == nil {
			return
		}
		// A marked row settles first, whatever else the fresh bead shows: a
		// close's pending interval outranks the live and terminal lanes, which
		// either would mis-account it (a live read of a row whose interval
		// really ended at the close) or skip it (a stopped/dormant reopen that
		// no open-lane filter selects).
		if strings.TrimSpace(b.Metadata[session.MetadataKeyUsageClosePending]) == "true" {
			cr.settleClosedInterval(ctx, sink, store, b, runtimeKind, now, logf, modelSweepFactory)
			return
		}
		state := b.Metadata["state"]
		if isLiveModelSweepState(state) {
			// Routed off the FRESH bead, so a session that woke since the snapshot
			// lands here too — and on the boot pass it is skipped just like a
			// snapshot-live one.
			if liveLane {
				cr.sweepLiveSessionModelUsage(ctx, b, now, logf, modelSweepFactory)
			}
			return
		}
		// Re-check the terminal state from the FRESH bead: a session that re-awoke in
		// the window since the snapshot was taken must not mint a tiny-wall fact for its
		// just-STARTED interval and suppress the real end-of-interval emission. Best-
		// effort accounting, the same NDI class as the sync-tail re-list delta.
		if !isComputeTerminalState(state) {
			return
		}
		cr.accountTerminalInterval(ctx, sink, store, b, runtimeKind, now, logf, modelSweepFactory)
	}
	processed := make(map[string]bool)
	for _, info := range sessions {
		// A canceled tick (controller shutdown, reconcile deadline) stops here
		// rather than working through the rest of the fleet: every remaining
		// session is picked up idempotently by the next tick.
		if ctx.Err() != nil {
			return
		}
		liveCandidate := liveLane && liveModelSweepCandidate(info)
		// A marked row joins the candidates whatever its state: a close's
		// pending interval must settle even when the row sits open in a state
		// neither open-lane filter selects (a stopped/dormant reopen, issue
		// #6672's AC-4).
		marked := strings.TrimSpace(info.UsageClosePending) == "true"
		if !computeFactGetCandidate(info) && !liveCandidate && !marked {
			continue
		}
		b, err := store.Get(info.ID)
		if err != nil {
			logf("usage: loading session %s for usage facts failed: %v", info.ID, err)
			continue
		}
		processed[info.ID] = true
		processSessionBead(b)
	}
	// Closed rows a close marked (issue #6672): a drain-ack (or other) close
	// from an awake state closes the bead in one commit, so the open snapshot
	// never holds it and the open lanes above cannot account its interval.
	// The listing is filtered by the equality marker, so its cost follows
	// UNSETTLED marked rows only — never closed history — and is capped at
	// 64 rows oldest-first per pass. The boot pass issues no listing: rows
	// marked while the controller was down are settled by the first
	// steady-state pass, keeping startup's synchronous path off the store.
	if bootReconcile {
		return
	}
	rows, err := store.List(beads.ListQuery{
		Type:          session.BeadType,
		Status:        "closed",
		IncludeClosed: true,
		TierMode:      beads.TierBoth,
		Metadata:      map[string]string{session.MetadataKeyUsageClosePending: "true"},
		Sort:          beads.SortCreatedAsc,
		Limit:         64,
	})
	if err != nil {
		logf("usage: listing closed sessions with pending usage markers failed: %v", err)
		return
	}
	for _, b := range rows {
		// A canceled tick stops between rows rather than working through the
		// rest of the listing: remaining rows are picked up idempotently by
		// the next pass.
		if ctx.Err() != nil {
			return
		}
		// A row the open pass above already settled or attempted this tick.
		if processed[b.ID] {
			continue
		}
		cr.settleClosedInterval(ctx, sink, store, b, runtimeKind, now, logf, modelSweepFactory)
	}
}

// accountTerminalInterval records a completed awake interval — the trailing
// model-usage sweep, the compute fact, and the interval-closing marker batch —
// for a session bead whose interval has ended, whether the row sits open in a
// compute-terminal state (the open terminal lane) or was closed straight from
// an awake state (the closed-interval settle lane, issue #6672).
//
// The sweep runs FIRST, symmetric to and beside the compute fact: it recovers
// the terminal interval's trailing model-token usage that the prompt-op seam
// never recorded (pool-routed, hook-self-driven agents self-drive after the
// claim nudge), gated by its OWN per-interval marker (usageModelSweptAtKey) so
// a transient miss retries on a later tick instead of being lost. A live
// sweep memo's resolved transcript path is reused when it describes the same
// awake epoch and conversation; a memo that settled a discovery miss is never
// trusted, because the transcript may have flushed since (the drain-ack
// turn). Best-effort — a sweep error never fails the reconcile tick; overlap
// with the prompt-op seam is collapsed at read time by the shared
// usage.ModelIdempotencyKey.
//
// The compute fact is always recorded with commit=false; its interval-closing
// side effects land in ONE SetMetadataBatch below, atomically with the sweep
// marker and the closed lane's settle markers, only once the sweep has
// settled and the fact is durably recorded (or the interval was already
// recorded). An unsettled sweep or a sink Record failure keeps every marker,
// so the next pass retries idempotently — floored at liveModelSweepMinInterval
// per session, like the live lane's attempts; and a closed row's unsettled
// sweep gives up at usage_closed_at + closedIntervalSweepGiveUp, committing
// the compute marker and clearing the settle markers without the swept
// marker. The settle markers this batch clears are no-ops on a row that was
// never marked.
func (cr *CityRuntime) accountTerminalInterval(
	ctx context.Context,
	sink usage.Sink,
	store beads.Store,
	b beads.Bead,
	runtimeKind string,
	now time.Time,
	logf func(string, ...any),
	modelSweepFactory func() *worker.Factory,
) {
	meta := b.Metadata
	awakeStart := strings.TrimSpace(meta["awake_started_at"])
	// A marked row is a closed-interval settle (issue #6672): its retries are
	// floored and eventually given up (below). An unmarked row is the open
	// terminal lane, whose per-tick sweep retry P2-2 pins — the floor and the
	// give-up do not apply to it.
	marked := strings.TrimSpace(meta[session.MetadataKeyUsageClosePending]) == "true"
	// sweepSettled defaults true so a nil factory / already-swept interval
	// never blocks the commit.
	sweepSettled := true
	if factory := modelSweepFactory(); factory != nil && awakeStart != "" &&
		strings.TrimSpace(meta[usageModelSweptAtKey]) != awakeStart {
		memo := cr.liveSweepMemoFor(b.ID, awakeStart, strings.TrimSpace(meta["session_key"]))
		// A marked row's attempt is floored per session
		// (liveModelSweepMinInterval), the same floor the live lane takes: a
		// pass inside the floor makes no attempt — no sweep, no compute
		// record, no marker write — so an unsettled closed interval retries at
		// most once per floor window per session (issue #6672's AC-6). A
		// fresh memo's zero floor never blocks the first attempt.
		if marked && now.Before(memo.nextSweepAt) {
			return
		}
		var serr error
		if memo.path != "" {
			_, sweepSettled, serr = factory.SweepSessionModelUsageAtPath(ctx, b.ID, meta, memo.path, now)
		} else {
			_, sweepSettled, serr = factory.SweepSessionModelUsage(ctx, b.ID, meta, now)
		}
		if serr != nil {
			logf("usage: model-usage sweep for session %s failed; will retry: %v", b.ID, serr)
		}
		// Take the floor before deciding anything else, marked rows only.
		// settledMiss is deliberately NOT recorded here: the transcript may
		// have flushed since (the drain-ack turn), so a miss is retried,
		// floored, until the give-up deadline below.
		if marked {
			memo.nextSweepAt = now.Add(liveModelSweepMinInterval)
			cr.storeLiveSweepMemo(b.ID, memo)
		}
	}
	// Record the compute fact without committing the interval: the fact is
	// idempotent (ComputeIdempotencyKey), so an unsettled sweep retries the
	// record next pass at zero cost.
	recorded := emitComputeFactForBead(ctx, sink, store, b, runtimeKind, cr.cityName, now, logf, false)
	already := awakeStart != "" && strings.TrimSpace(meta[usageComputeEmittedAtKey]) == awakeStart
	// A closed row gives up on an unsettled sweep at usage_closed_at +
	// closedIntervalSweepGiveUp: past that deadline the compute marker is
	// committed (and the settle markers cleared) without the swept marker,
	// since a keyed transcript that never appeared can no longer yield the
	// trailing model facts. An open row takes no give-up — its interval has
	// no deadline, and the next pass simply retries, floored.
	giveUp := false
	if anchorRaw := strings.TrimSpace(meta[session.MetadataKeyUsageClosedAt]); anchorRaw != "" {
		if anchor, aerr := time.Parse(time.RFC3339Nano, anchorRaw); aerr == nil &&
			!now.Before(anchor.Add(closedIntervalSweepGiveUp)) {
			giveUp = true
		}
	}
	if !sweepSettled && !giveUp || !recorded && !already {
		return
	}
	batch := map[string]string{
		usageComputeEmittedAtKey:             awakeStart,
		session.MetadataKeyUsageClosePending: "",
		session.MetadataKeyUsageClosedAt:     "",
	}
	if sweepSettled {
		batch[usageModelSweptAtKey] = awakeStart
	}
	if err := store.SetMetadataBatch(b.ID, batch); err != nil {
		logf("usage: closing interval markers for session %s failed; will retry (deduped by idempotency keys): %v", b.ID, err)
		return
	}
	// The interval is accounted; the memo (a live-epoch discovery) is spent.
	cr.liveSweepMemos.Delete(b.ID)
}

// settleClosedInterval settles the awake interval a close ended straight from
// an awake state (issue #6672) on a row the close marked with
// usage_close_pending. Such a row never sits open in a compute-terminal
// state, so the open-snapshot scan cannot account it; this lane does — for a
// closed row off the marked listing, and for an open marked row (a configured
// named session the sync phase reopened before the reconciler re-wakes it)
// off the open snapshot, where the settle takes precedence over the live and
// terminal lanes. A row with nothing to account — no confirmed start, an
// already-recorded interval, an open row re-woken on a newer interval (the
// marked one is superseded and lost), or an open row still live on the
// interval the close claims to have ended (a marker that raced a re-wake) —
// has its markers cleared so the listing and the open filter stop returning
// it; otherwise the interval is accounted through the shared terminal block,
// which clears the markers on commit.
func (cr *CityRuntime) settleClosedInterval(
	ctx context.Context,
	sink usage.Sink,
	store beads.Store,
	b beads.Bead,
	runtimeKind string,
	now time.Time,
	logf func(string, ...any),
	modelSweepFactory func() *worker.Factory,
) {
	meta := b.Metadata
	awakeStart := strings.TrimSpace(meta["awake_started_at"])
	if awakeStart != "" && strings.TrimSpace(meta[usageComputeEmittedAtKey]) != awakeStart {
		if anchorRaw := strings.TrimSpace(meta[session.MetadataKeyUsageClosedAt]); anchorRaw != "" {
			if anchor, err := time.Parse(time.RFC3339Nano, anchorRaw); err == nil {
				if started, serr := time.Parse(time.RFC3339, awakeStart); serr == nil {
					if started.After(anchor) {
						// The row re-woke on a NEWER interval than the one the
						// close ended: the marked interval is superseded — its
						// end is no longer measurable, so it is lost — and the
						// live interval's own accounting (when it ends) is not
						// this settle's to touch.
						logf("usage: closed interval of session %s ended at %s is superseded by a newer interval; dropping the marker",
							b.ID, anchorRaw)
						cr.clearUsageCloseMarkers(store, b.ID, logf)
						return
					}
					if b.Status == "open" && isLiveModelSweepState(meta["state"]) {
						// The marker raced a row still awake on the very
						// interval the close claims to have ended: the
						// interval is live, not closed. Leave its accounting
						// to the ordinary lanes when it really ends.
						logf("usage: close marker of session %s raced an awake interval; dropping the marker",
							b.ID)
						cr.clearUsageCloseMarkers(store, b.ID, logf)
						return
					}
				}
			}
		}
		cr.accountTerminalInterval(ctx, sink, store, b, runtimeKind, now, logf, modelSweepFactory)
		return
	}
	// Nothing to account. Clear the markers so the closed listing stops
	// returning the row.
	cr.clearUsageCloseMarkers(store, b.ID, logf)
}

// clearUsageCloseMarkers clears a row's close-settle markers (issue #6672):
// the closed interval is settled, or the row had nothing to account. The row
// leaves both the marked closed listing and the open loop's marked candidacy.
func (cr *CityRuntime) clearUsageCloseMarkers(store beads.Store, id string, logf func(string, ...any)) {
	if err := store.SetMetadataBatch(id, map[string]string{
		session.MetadataKeyUsageClosePending: "",
		session.MetadataKeyUsageClosedAt:     "",
	}); err != nil {
		logf("usage: clearing close markers for session %s failed; will retry: %v", id, err)
	}
}

// liveSweepMemo is one awake session's live model-usage sweep state, held for
// the process lifetime because the worker factory is rebuilt every tick.
//
// awakeStart and sessionKey stamp the epoch and conversation the memo describes:
// a re-wake or a replacement conversation invalidates it, so it resolves its own
// rollout rather than sweeping a stale path. Keying the map by session id (with
// the epoch inside the value) means a long-lived session replaces its memo on
// each wake instead of accumulating one entry per epoch forever.
type liveSweepMemo struct {
	awakeStart string
	sessionKey string
	// path is the resolved transcript, empty until discovery succeeds.
	path string
	// settledMiss records a DEFINITIVE discovery miss — there is nothing to find
	// for this epoch, so discovery is never re-attempted for it.
	settledMiss bool
	// nextSweepAt floors the sweep cadence (liveModelSweepMinInterval). It also
	// backs off an unsettled discovery miss, so a session whose transcript cannot
	// be resolved yet re-attempts discovery on that same floor instead of on
	// every tick forever.
	nextSweepAt time.Time
}

// sweepLiveSessionModelUsage incrementally records model usage for an awake
// session without closing its compute interval or stamping the terminal sweep
// marker. Transcript discovery and the transcript read are both memoized and
// throttled per session (see liveSweepMemo and liveModelSweepMinInterval), so a
// live fleet costs at most one bounded discovery plus one tail read per session
// per liveModelSweepMinInterval no matter how fast the reconcile tick spins.
func (cr *CityRuntime) sweepLiveSessionModelUsage(
	ctx context.Context,
	b beads.Bead,
	now time.Time,
	logf func(string, ...any),
	modelSweepFactory func() *worker.Factory,
) {
	if b.Metadata == nil || !isLiveModelSweepState(b.Metadata["state"]) {
		return
	}
	awakeStart := strings.TrimSpace(b.Metadata["awake_started_at"])
	if awakeStart == "" {
		return
	}
	memo := cr.liveSweepMemoFor(b.ID, awakeStart, strings.TrimSpace(b.Metadata["session_key"]))
	if memo.settledMiss || now.Before(memo.nextSweepAt) {
		return
	}
	factory := modelSweepFactory()
	if factory == nil {
		return
	}
	if memo.path == "" {
		// A settled miss is definitive for this epoch (unregistered provider family;
		// a keyless codex session whose CLEAN workdir+window scan found nothing —
		// ambiguity, an out-of-window filename, or a TZ shift, none of which a retry
		// resolves; or a keyless claude session whose transcript lookup cleanly
		// REFUSED an ambiguous shared workdir, which stays ambiguous while the pool
		// shares it — note an unambiguous claude session whose transcript is merely
		// not written yet is NOT settled, so the live lane keeps rediscovering it).
		// Record it so the scan is never repeated; the session's usage is
		// still recovered by the terminal sweep when its interval ends — open
		// in a terminal state, or closed straight from awake by the
		// closed-interval lane — and a re-wake starts a fresh epoch that
		// discovers again.
		path, settled := factory.DiscoverSweepTranscript(b.ID, b.Metadata, now)
		memo.path = path
		memo.settledMiss = path == "" && settled
	}
	// Persist the memo BEFORE the miss return: an unsettled miss must still take
	// the interval floor, or discovery repeats on every tick for a session whose
	// transcript never resolves.
	memo.nextSweepAt = now.Add(liveModelSweepMinInterval)
	cr.storeLiveSweepMemo(b.ID, memo)
	if memo.path == "" {
		return
	}
	if _, _, err := factory.SweepSessionModelUsageAtPath(ctx, b.ID, b.Metadata, memo.path, now); err != nil {
		logf("usage: live model-usage sweep for session %s failed; will retry: %v", b.ID, err)
	}
}

// liveSweepMemoFor returns the session's memo for the given awake epoch and
// provider session key, or a fresh one stamped with that identity when none is
// held or the held one describes a superseded epoch or conversation.
func (cr *CityRuntime) liveSweepMemoFor(sessionID, awakeStart, sessionKey string) liveSweepMemo {
	if value, ok := cr.liveSweepMemos.Load(sessionID); ok {
		if memo, isMemo := value.(liveSweepMemo); isMemo &&
			memo.awakeStart == awakeStart && memo.sessionKey == sessionKey {
			return memo
		}
	}
	return liveSweepMemo{awakeStart: awakeStart, sessionKey: sessionKey}
}

func (cr *CityRuntime) storeLiveSweepMemo(sessionID string, memo liveSweepMemo) {
	cr.liveSweepMemos.Store(sessionID, memo)
}
