package main

// The route-recovery lane: events for freshness, a cadenced scan for convergence.
//
// # What was wrong
//
// restoreCarriedWorkRoutes is a CONVERGENCE backstop — it repairs a route that a
// mid-write crash, a legacy import or a claim/release flip lost — and it was
// wired to run at DELTA cadence: a full live open-corpus scan of the city work
// ledger and every rig, on every controller tick. On maintainer-city, whose work
// ledger answers from remote postgres at ~5.4s per query, that single leg was
// 185.3s +/- 0.7s of a ~360s tick (ga-l7jdg, measured on ga-4qdfn). The variance
// under 0.4% is the tell: a fixed-size scan of a corpus that does not change.
//
// The read-only discriminator on mc (ga-l7jdg S1 step 1) says which of the three
// decompositions it is: across 16h of controller journal there is not one
// `route recovery (...): restored gc.routed_to on N ready bead(s)` line, and the
// ledger's open corpus carries gc.run_target on ZERO beads. So there is no
// re-stamp flap to fix in a sibling lane and no per-candidate Get fan-out — the
// whole leg is the scan itself, paid every tick to discover nothing.
//
// # The split
//
// This is the CachingStore doctrine (internal/beads/caching_store_reconcile.go)
// promoted from the store layer to the tick's leg vocabulary: events are
// freshness, a rare authoritative scan is convergence.
//
//   - The DELTA pass runs in the tick. Its candidates are the beads named by
//     bead.created / bead.updated since the lane's journal cursor, and nothing
//     else. A steady tick names nothing and therefore reads nothing: it does not
//     even build a plan.
//   - The BACKSTOP pass is the old full scan, unchanged in what it repairs,
//     demoted to a background lane on an hourly cadence — plus, immediately, on
//     every way the event feed can lie: startup, a cursor gap, a watcher that
//     could not start or restarted, a candidate queue overflow, and a leg that
//     errored on the previous pass.
//
// Events CAN be lost — an agent's bd write reaches the journal through a hook
// chain that can be killed, and a graph store emits no bead.closed at all — so
// the backstop is not optional and its "events lost, backstop heals" behavior is
// pinned by its own control test.
//
// # Two things the old scanner could not say
//
// A candidate whose live re-check keeps failing used to be re-read forever in
// silence. It is now QUARANTINED after two consecutive backstop passes: a
// metadata marker the `route-recovery-quarantine` doctor check surfaces, cleared
// automatically the moment the re-check passes, and liftable with
// `gc doctor --fix`. It is a label, never a skip — the bead stays a candidate.
//
// A bead whose restore SUCCEEDS every pass is the opposite failure: some sibling
// lane is clearing gc.routed_to behind us, and a faster treadmill is not a fix.
// Re-stamps per bead are bounded; past the bound the lane stops writing, says so
// loudly, and quarantines the bead for the operator.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/storeref"
)

const (
	// routeRecoveryBackstopInterval is how often the authoritative full scan
	// runs when nothing forces it sooner. Hourly, in the shape wisp_gc's
	// shouldRun and orderRescanInterval already use: explicit cadence state, not
	// trigger-name gating. Under overload every tick is a "patrol" tick, which
	// is exactly how a backstop ends up hot.
	routeRecoveryBackstopInterval = time.Hour

	// routeRecoveryBackstopRetryInterval is the cadence after a pass that could
	// not read every leg. The resolver marks the work ledger Fatal, so a ledger
	// outage aborts the pass before the rig legs — short retry is what keeps the
	// rigs' convergence from waiting out the full hour, and it is bounded so a
	// persistently dark ledger costs 12 failed scans an hour rather than a spin.
	routeRecoveryBackstopRetryInterval = 5 * time.Minute

	// routeRecoveryQuarantinePasses is how many CONSECUTIVE backstop passes a
	// candidate must fail its live re-check before it is marked. Two, because
	// one failure is the ordinary race the re-check exists to catch: a claim
	// landing between the scan and the write.
	routeRecoveryQuarantinePasses = 2

	// routeRecoveryFlapLimit bounds how many times this lane will restore the
	// SAME bead's route. A route that has to be restored again and again is not
	// a lost route, it is a lane clearing it; re-stamping forever hides that.
	routeRecoveryFlapLimit = 3

	// routeRecoveryCandidateCap bounds the pending candidate set. Overflow is
	// treated exactly like a cursor gap — the delta feed can no longer claim to
	// name everything, so the authoritative scan answers instead.
	routeRecoveryCandidateCap = 4096
)

// Quarantine reasons, as they appear in bead metadata and in the doctor advisory.
const (
	routeRecoveryQuarantineRecheckFailed = "recheck-failed"
	routeRecoveryQuarantineRestoreFlap   = "restore-flap"
)

// Backstop reasons, as they appear in the trace and the log line.
const (
	routeRecoveryBackstopStartup    = "startup"
	routeRecoveryBackstopCadence    = "cadence"
	routeRecoveryBackstopForced     = "cursor-gap"
	routeRecoveryBackstopLegDegrade = "leg-degrade"
)

// routeRecoveryReport is one pass's outcome, in the terms the tick trace and the
// operator log need: what it repaired, what it could not, and whether its answer
// was complete.
type routeRecoveryReport struct {
	lane        string
	reason      string
	candidates  int
	restored    int
	quarantined int
	flapping    []string
	// legReads counts store round trips this pass issued. It is the unit the
	// tick's latency is actually measured in, and the budget test asserts on it.
	legReads int
	partial  bool
	err      error
}

// fields renders the report for the reconciler trace.
func (r routeRecoveryReport) fields() map[string]any {
	out := map[string]any{
		"lane":        r.lane,
		"candidates":  r.candidates,
		"restored":    r.restored,
		"leg_reads":   r.legReads,
		"quarantined": r.quarantined,
	}
	if r.reason != "" {
		out["reason"] = r.reason
	}
	if len(r.flapping) > 0 {
		out["flapping"] = strings.Join(r.flapping, ",")
	}
	if r.partial {
		out["partial"] = true
	}
	return out
}

func (r routeRecoveryReport) outcome() TraceOutcomeCode {
	switch {
	case r.err != nil:
		return TraceOutcomeFailed
	case r.partial || len(r.flapping) > 0:
		return TraceOutcomePartial
	default:
		return TraceOutcomeComplete
	}
}

// routeRecoveryLane holds the cadence and accounting state the two passes share.
// It owns no stores and opens nothing: a caller hands it the plan for the pass,
// which keeps the suspension frame told-not-decided exactly as the census arms
// do.
type routeRecoveryLane struct {
	mu sync.Mutex

	// pending is the delta feed's candidate set: bead ids the journal named
	// since the cursor whose snapshot carried a recoverable route.
	pending map[string]struct{}

	// forced records that the event feed cannot be trusted for the next pass —
	// it never started, it restarted, its cursor regressed, or pending
	// overflowed — with the reason the trace should carry.
	forced       bool
	forcedReason string

	lastBackstopAt time.Time
	backstopRan    bool
	retrySoon      bool

	// consecutiveRecheckFailures and restores are per-bead accounting for the
	// two things a silent re-scan could never report.
	consecutiveRecheckFailures map[string]int
	restores                   map[string]int

	interval time.Duration
	retry    time.Duration
}

func newRouteRecoveryLane() *routeRecoveryLane {
	return &routeRecoveryLane{
		pending:                    map[string]struct{}{},
		consecutiveRecheckFailures: map[string]int{},
		restores:                   map[string]int{},
		interval:                   routeRecoveryBackstopInterval,
		retry:                      routeRecoveryBackstopRetryInterval,
		// Nothing has scanned yet, so the first thing this lane does is scan.
		forced:       true,
		forcedReason: routeRecoveryBackstopStartup,
	}
}

// force marks the next backstop pass due immediately. Every way the event feed
// can stop naming everything funnels through here.
func (l *routeRecoveryLane) force(reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.forced = true
	if l.forcedReason == "" || l.forcedReason == routeRecoveryBackstopCadence {
		l.forcedReason = reason
	}
}

// observe feeds one journal event to the delta lane. It decodes the bead
// snapshot the event carries and keeps the id only when that snapshot declares a
// recoverable route — so a busy city's ordinary bead traffic costs the tick
// nothing.
func (l *routeRecoveryLane) observe(evt events.Event) {
	switch evt.Type {
	case events.BeadCreated, events.BeadUpdated:
	default:
		return
	}
	bead, ok := beads.DecodeBeadEventPayload(evt.Payload)
	if !ok || strings.TrimSpace(bead.ID) == "" || carriedPoolRoute(bead) == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.pending) >= routeRecoveryCandidateCap {
		// The feed can no longer claim to name everything. Hand the question to
		// the scan rather than silently dropping candidates.
		l.pending = map[string]struct{}{}
		l.forced = true
		l.forcedReason = routeRecoveryBackstopForced
		return
	}
	l.pending[bead.ID] = struct{}{}
}

// takePending drains the candidate set.
func (l *routeRecoveryLane) takePending() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.pending) == 0 {
		return nil
	}
	out := make([]string, 0, len(l.pending))
	for id := range l.pending {
		out = append(out, id)
	}
	l.pending = map[string]struct{}{}
	sort.Strings(out)
	return out
}

// backstopDue reports whether the authoritative scan should run now, and why.
func (l *routeRecoveryLane) backstopDue(now time.Time) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.forced {
		reason := l.forcedReason
		if reason == "" {
			reason = routeRecoveryBackstopForced
		}
		return reason, true
	}
	if !l.backstopRan {
		return routeRecoveryBackstopStartup, true
	}
	cadence := l.interval
	if l.retrySoon {
		cadence = l.retry
	}
	if now.Sub(l.lastBackstopAt) >= cadence {
		return routeRecoveryBackstopCadence, true
	}
	return "", false
}

// noteBackstopRan records the pass and clears the force latch. A pass that could
// not read every leg schedules itself back on the short retry cadence: the leg
// it missed is exactly the one whose convergence is now overdue.
func (l *routeRecoveryLane) noteBackstopRan(now time.Time, partial bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lastBackstopAt = now
	l.backstopRan = true
	l.forced = false
	l.forcedReason = ""
	l.retrySoon = partial
}

// startEventFeed tails the journal and feeds the delta lane. It watches from the
// CURRENT head: history before this point is the startup backstop's job, and
// replaying it would be a second full pass wearing the delta lane's name.
//
// Every failure mode here forces the backstop, which is the whole reason the
// backstop exists: a feed that cannot promise to name every change must not be
// the only thing looking.
func (l *routeRecoveryLane) startEventFeed(ctx context.Context, prov events.Provider) {
	if prov == nil {
		l.force(routeRecoveryBackstopForced)
		return
	}
	seq, err := prov.LatestSeq()
	if err != nil {
		l.force(routeRecoveryBackstopForced)
		return
	}
	go func() {
		for {
			watcher, err := prov.Watch(ctx, seq)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				l.force(routeRecoveryBackstopForced)
				select {
				case <-ctx.Done():
					return
				case <-time.After(beadEventWatcherRetryDelay):
					continue
				}
			}
			for {
				evt, err := watcher.Next()
				if err != nil {
					_ = watcher.Close()
					break
				}
				if evt.Seq < seq {
					// A regressed sequence means the log this watcher is reading
					// is not the log the cursor came from.
					l.force(routeRecoveryBackstopForced)
				}
				seq = evt.Seq
				l.observe(evt)
			}
			if ctx.Err() != nil {
				return
			}
			// The watcher ended without the context being done: the tail broke.
			// Whatever it missed between here and the next Watch is a gap.
			l.force(routeRecoveryBackstopForced)
		}
	}()
}

// routeRecoveryLeg is one plan leg the lane may repair through, with the label
// the operator log has always spelled it with.
type routeRecoveryLeg struct {
	label string
	store beads.Store
}

// routeRecoveryLegLabel spells a plan leg the way the pre-lane log line did:
// "city" for the work ledger, "rig <name>" for a rig.
func routeRecoveryLegLabel(ref storeref.StoreRef) string {
	if rig, ok := storeref.ScopeRigContext(string(ref)); ok && rig != "" {
		return "rig " + rig
	}
	return "city"
}

// walkRouteRecoveryLegs runs visit over the plan's WORK legs in the resolver's
// order and under the resolver's per-leg error policy.
//
// # Why the class bindings are skipped, deliberately and visibly
//
// Plan(RoutedWork) puts every relocated class binding last, and this lane does
// not read them. It is the same choice RoleShadow's doc names for a consumer
// whose pre-resolver list did not include a family of legs: it either plans over
// a topology without them or drops them, and its slice pins which. Dropping is
// what this slice pins, because the graph binding holds graph.v2 workflow roots
// and carriedPoolRoute's legacy-workflow arm would treat one carrying
// gc.run_target as a recoverable pool route. Re-stamping gc.routed_to there
// respawns a worker that drains no-op on every pass — the exact hazard
// restoreCarriedWorkRoutes' gc-4zb comment describes. Widening this lane onto
// the binding is a behavior change that belongs to the demand slice, with its
// own evidence; a latency slice must not make it silently.
func walkRouteRecoveryLegs(plan storeref.ResolvedPlan, visit func(routeRecoveryLeg) error) (partial bool, err error) {
	result, walkErr := storeref.Walk(plan, func(leg storeref.Leg) (bool, error) {
		if storeref.IsClassRef(string(leg.Ref)) || leg.Store == nil {
			return false, nil
		}
		return false, visit(routeRecoveryLeg{label: routeRecoveryLegLabel(leg.Ref), store: leg.Store})
	})
	return result.Partial, walkErr
}

// deltaPass repairs only the beads the journal named since the last pass.
//
// The steady-state property this whole slice exists for lives in the first two
// lines: no candidates, no plan, no store read at all.
func (l *routeRecoveryLane) deltaPass(plan storeref.ResolvedPlan, candidates []string) routeRecoveryReport {
	report := routeRecoveryReport{lane: "delta", candidates: len(candidates)}
	if len(candidates) == 0 {
		return report
	}
	var errs []error
	partial, walkErr := walkRouteRecoveryLegs(plan, func(leg routeRecoveryLeg) error {
		rows, reads, err := liveRouteCandidates(leg.store, candidates)
		report.legReads += reads
		if err != nil {
			return fmt.Errorf("re-reading %d route candidate(s): %w", len(candidates), err)
		}
		for _, row := range rows {
			outcome := l.restoreRoute(leg.store, row, false)
			report.legReads += outcome.writes
			switch {
			case outcome.restored:
				report.restored++
			case outcome.flapping:
				report.flapping = append(report.flapping, row.ID)
			}
			if outcome.quarantined {
				report.quarantined++
			}
			if outcome.err != nil {
				errs = append(errs, outcome.err)
			}
		}
		return nil
	})
	report.partial = partial
	report.err = errors.Join(append(errs, walkErr)...)
	sort.Strings(report.flapping)
	return report
}

// backstopPass is the authoritative convergence scan: today's full live open
// read of every work leg, with the per-candidate Get fan-out replaced by one
// batched IN-list re-verify per leg.
func (l *routeRecoveryLane) backstopPass(plan storeref.ResolvedPlan, reason string) routeRecoveryReport {
	report := routeRecoveryReport{lane: "backstop", reason: reason}
	var errs []error
	partial, walkErr := walkRouteRecoveryLegs(plan, func(leg routeRecoveryLeg) error {
		legReport := l.backstopLeg(leg.store)
		report.candidates += legReport.candidates
		report.restored += legReport.restored
		report.quarantined += legReport.quarantined
		report.legReads += legReport.legReads
		report.flapping = append(report.flapping, legReport.flapping...)
		return legReport.err
	})
	report.partial = partial
	report.err = errors.Join(append(errs, walkErr)...)
	sort.Strings(report.flapping)
	return report
}

// backstopLeg is the authoritative scan of ONE work store: the full live
// open-corpus read, then a single batched re-verify of the candidates it found.
//
// It is the whole of the pre-lane restoreCarriedWorkRoutes with the per-candidate
// Get fan-out collapsed — one IN-list read per leg instead of one Get per bead.
// Every guard it carried is preserved and separately pinned:
//
//   - Live on the open List is what makes Status:"open" mean open (gc-4zb).
//     mapBdStatus folds bd's blocked/deferred/review/testing into "open", so a
//     blocked bead is indistinguishable from ready work in every beads.Bead this
//     code can read; only the backing store's raw --status=open filter excludes
//     it, and only a Live query reaches that filter.
//   - The re-verify reads through the store's authoritative, cache-bypassing
//     handle, because a plain read can return a cached bead that predates a
//     cross-process claim (ga-bgu). A claim flips the bead to in_progress and
//     consumes gc.routed_to in one update (ga-sa0); re-stamping over it hands the
//     dispatcher a phantom pool-demand bead that flaps.
//   - The write is keyed on the LIVE row's own carried route, so a bead another
//     pass already restored yields "" and the pass stays idempotent.
//
// The window between the re-verify and the write is narrowed, not closed. The
// re-stamp stays monotonic (never worse than the prior blind write), so the
// residual degrades to the pre-guard behavior rather than a new failure.
func (l *routeRecoveryLane) backstopLeg(store beads.Store) routeRecoveryReport {
	report := routeRecoveryReport{lane: "backstop"}
	if store == nil {
		return report
	}
	items, err := store.List(beads.ListQuery{Status: "open", AllowScan: true, Live: true})
	report.legReads++
	if err != nil {
		report.err = fmt.Errorf("listing open work: %w", err)
		return report
	}
	var ids []string
	for _, b := range items {
		// Belt-and-braces with the Status:"open" query so the guarantee holds
		// regardless of store-level filtering semantics: an assigned bead is
		// already claimed and needs no route.
		if carriedPoolRoute(b) == "" || b.Status != "open" || strings.TrimSpace(b.Assignee) != "" {
			continue
		}
		ids = append(ids, b.ID)
	}
	report.candidates = len(ids)
	if len(ids) == 0 {
		return report
	}
	rows, reads, err := liveRouteCandidates(store, ids)
	report.legReads += reads
	if err != nil {
		report.err = fmt.Errorf("re-reading %d route candidate(s): %w", len(ids), err)
		return report
	}
	// The re-verify answers for the ids it returned; the ones it dropped are the
	// candidates whose live row no longer agrees with the scan, and a
	// disagreement that survives two passes is what quarantine surfaces.
	var errs []error
	returned := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		returned[row.ID] = struct{}{}
		outcome := l.restoreRoute(store, row, true)
		report.legReads += outcome.writes
		switch {
		case outcome.restored:
			report.restored++
		case outcome.flapping:
			report.flapping = append(report.flapping, row.ID)
		}
		if outcome.quarantined {
			report.quarantined++
		}
		if outcome.err != nil {
			errs = append(errs, outcome.err)
		}
	}
	for _, id := range ids {
		if _, ok := returned[id]; ok {
			continue
		}
		marked, err := l.noteRecheckFailure(store, id)
		if marked {
			report.legReads++
			report.quarantined++
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	report.err = errors.Join(errs...)
	return report
}

// liveRouteCandidates re-reads the named beads through the store's
// authoritative, cache-bypassing handle, still filtered to raw-open.
//
// One query for the whole set is the point: the scan it replaces issued one Get
// per candidate, and against a remote ledger a batch of 33 Gets is 33 sequential
// round trips. A single candidate stays a Get, which is strictly cheaper than a
// filtered List on a backend that cannot push the IN-list down.
//
// It returns the number of store round trips it made so the caller can budget.
func liveRouteCandidates(store beads.Store, ids []string) ([]beads.Bead, int, error) {
	if store == nil || len(ids) == 0 {
		return nil, 0, nil
	}
	if len(ids) == 1 {
		bead, err := beads.HandlesFor(store).Live.Get(ids[0])
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				return nil, 1, nil
			}
			return nil, 1, err
		}
		if bead.Status != "open" {
			return nil, 1, nil
		}
		return []beads.Bead{bead}, 1, nil
	}
	rows, err := store.List(beads.ListQuery{IDs: ids, Status: "open", Live: true})
	if err != nil {
		return nil, 1, err
	}
	return rows, 1, nil
}

// routeRestoreOutcome is what one candidate's re-verify decided.
type routeRestoreOutcome struct {
	restored    bool
	flapping    bool
	quarantined bool
	writes      int
	err         error
}

// restoreRoute re-stamps gc.routed_to from the route the LIVE row still
// declares, and is the only place this lane writes a route.
//
// The live row is the authority: recomputing carriedPoolRoute on it is what
// makes the pass idempotent (a bead another pass already restored yields "") and
// what keeps a claim that landed since the scan from being clobbered — a claim
// flips the bead to in_progress and consumes gc.routed_to in one update (ga-sa0,
// ga-bgu).
func (l *routeRecoveryLane) restoreRoute(store beads.Store, live beads.Bead, backstop bool) routeRestoreOutcome {
	route := carriedPoolRoute(live)
	if route == "" || live.Status != "open" || strings.TrimSpace(live.Assignee) != "" {
		if backstop {
			marked, err := l.noteRecheckFailure(store, live.ID)
			if marked {
				return routeRestoreOutcome{quarantined: true, writes: 1, err: err}
			}
			return routeRestoreOutcome{err: err}
		}
		return routeRestoreOutcome{}
	}

	l.mu.Lock()
	delete(l.consecutiveRecheckFailures, live.ID)
	l.restores[live.ID]++
	restoreCount := l.restores[live.ID]
	l.mu.Unlock()

	if restoreCount > routeRecoveryFlapLimit {
		// The route keeps coming back empty after we set it: a sibling lane is
		// clearing it. A faster treadmill is not a fix, so stop writing and make
		// the flap visible instead.
		marked, err := l.quarantine(store, live, routeRecoveryQuarantineRestoreFlap)
		out := routeRestoreOutcome{flapping: true, err: err}
		if marked {
			out.quarantined = true
			out.writes = 1
		}
		return out
	}

	writes := map[string]string{beadmeta.RoutedToMetadataKey: route}
	if isRouteRecoveryQuarantined(live) {
		// The re-check passes now, so the quarantine verdict is stale. Clearing
		// it in the same batch as the restore keeps it to one round trip.
		writes[beadmeta.RouteRecoveryQuarantinedMetadataKey] = ""
		writes[beadmeta.RouteRecoveryQuarantineReasonMetadataKey] = ""
	}
	if err := store.SetMetadataBatch(live.ID, writes); err != nil {
		return routeRestoreOutcome{writes: 1, err: fmt.Errorf("bead %s: restoring gc.routed_to=%q: %w", live.ID, route, err)}
	}
	return routeRestoreOutcome{restored: true, writes: 1}
}

// noteRecheckFailure counts a candidate whose live row disagreed with the scan
// and marks it once the disagreement has survived two consecutive backstop
// passes. It reports whether it wrote.
func (l *routeRecoveryLane) noteRecheckFailure(store beads.Store, id string) (bool, error) {
	l.mu.Lock()
	l.consecutiveRecheckFailures[id]++
	failures := l.consecutiveRecheckFailures[id]
	l.mu.Unlock()
	if failures != routeRecoveryQuarantinePasses {
		// Exactly-at-threshold, so the marker is written once per streak rather
		// than on every pass thereafter.
		return false, nil
	}
	return l.quarantine(store, beads.Bead{ID: id}, routeRecoveryQuarantineRecheckFailed)
}

// quarantine marks a bead for the doctor advisory. Quarantine is a LABEL, never
// a skip: the bead stays a candidate, the next pass re-evaluates it, and a pass
// whose re-check succeeds clears the marker. Nothing here drops work silently.
func (l *routeRecoveryLane) quarantine(store beads.Store, bead beads.Bead, reason string) (bool, error) {
	if store == nil || strings.TrimSpace(bead.ID) == "" {
		return false, nil
	}
	if strings.TrimSpace(bead.Metadata[beadmeta.RouteRecoveryQuarantineReasonMetadataKey]) == reason &&
		isRouteRecoveryQuarantined(bead) {
		return false, nil
	}
	err := store.SetMetadataBatch(bead.ID, map[string]string{
		beadmeta.RouteRecoveryQuarantinedMetadataKey:      "true",
		beadmeta.RouteRecoveryQuarantineReasonMetadataKey: reason,
	})
	if err != nil {
		return false, fmt.Errorf("bead %s: marking route-recovery quarantine (%s): %w", bead.ID, reason, err)
	}
	return true, nil
}

// isRouteRecoveryQuarantined reports whether a bead already carries the marker.
func isRouteRecoveryQuarantined(b beads.Bead) bool {
	return strings.TrimSpace(b.Metadata[beadmeta.RouteRecoveryQuarantinedMetadataKey]) == "true"
}
