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

	// passMu admits ONE authoritative scan at a time. The startup scan can run
	// for minutes on a large city while the background poller is already ticking,
	// and two concurrent full scans would double the ledger load to converge the
	// same state twice.
	passMu sync.Mutex

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

// beginBackstop reports whether this caller owns the authoritative scan. A
// caller that loses simply skips: the pass in flight is reading the same state.
func (l *routeRecoveryLane) beginBackstop() bool { return l.passMu.TryLock() }

// endBackstop releases the scan slot.
func (l *routeRecoveryLane) endBackstop() { l.passMu.Unlock() }

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

// startEventFeed tails the journal and feeds this lane.
func (l *routeRecoveryLane) startEventFeed(ctx context.Context, prov events.Provider) {
	watchJournalForDeltaLanes(ctx, prov,
		func() { l.force(routeRecoveryBackstopForced) },
		l.observe)
}

// watchJournalForDeltaLanes tails the event journal and hands every event to the
// tick's delta lanes. It watches from the CURRENT head: history before this point
// is the startup backstop's job, and replaying it would be a second full pass
// wearing the delta lane's name.
//
// Every failure mode here calls onGap, which is the whole reason the backstops
// exist: a feed that cannot promise to name every change must not be the only
// thing looking.
func watchJournalForDeltaLanes(ctx context.Context, prov events.Provider, onGap func(), observe func(events.Event)) {
	if prov == nil {
		onGap()
		return
	}
	seq, err := prov.LatestSeq()
	if err != nil {
		onGap()
		return
	}
	go func() {
		for {
			watcher, err := prov.Watch(ctx, seq)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				onGap()
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
					onGap()
				}
				seq = evt.Seq
				observe(evt)
			}
			if ctx.Err() != nil {
				return
			}
			// The watcher ended without the context being done: the tail broke.
			// Whatever it missed between here and the next Watch is a gap.
			onGap()
		}
	}()
}

// storePlane names WHICH legs of a resolved plan a pass is allowed to read.
//
// # The operator invariant (2026-08-15, ga-l7jdg)
//
// Every bd operation on the RUNTIME plane — ticks, hooks, claims, sweeps,
// census — hits the infra/class binding ONLY. A work-ledger leg on the runtime
// path is a misrouting bug by definition, not a cost to amortize: it is why a
// claim needs a 240s window and why this leg cost 185s of a 360s tick. The
// remote work ledger serves backlog and task management, which are not the
// runtime plane.
//
// So the plane is a property of the CALLER, and the two lanes of this file sit
// on opposite sides of it. The tick's delta pass is runtime; the rare,
// separately-scheduled convergence scan is not, which is the only reason it may
// still consult the ledger at all — and it does so off the tick, on its own
// cadence, never inline.
type storePlane int

const (
	// runtimePlane is city operations. Infra/class binding only.
	runtimePlane storePlane = iota
	// reconcilePlane is the rare off-tick convergence lane, which may read the
	// work ledger because converging it is the whole reason it exists.
	reconcilePlane
)

// routeRecoveryLeg is one plan leg the lane may repair through, with the label
// the operator log has always spelled it with.
type routeRecoveryLeg struct {
	label string
	store beads.Store
}

// routeRecoveryLegLabel spells a plan leg the way the pre-lane log line did:
// "city" for the work ledger, "rig <name>" for a rig, and the class ref itself
// for a binding.
func routeRecoveryLegLabel(ref storeref.StoreRef) string {
	if storeref.IsClassRef(string(ref)) {
		return string(ref)
	}
	if rig, ok := storeref.ScopeRigContext(string(ref)); ok && rig != "" {
		return "rig " + rig
	}
	return "city"
}

// walkRouteRecoveryLegs runs visit over the legs THIS PLANE may read, in the
// resolver's order and under the resolver's per-leg error policy.
//
// # Which legs each plane gets, and why the split is here rather than in Plan()
//
// The runtime plane reads the class bindings and nothing else — the operator
// invariant above. A city that relocates no class has no binding, and there its
// work store IS its infra store, so the plan's work leg is the runtime leg: the
// rule degrades to "the only store there is" rather than to "no store at all",
// which would silently disable the delta lane on every single-store city.
//
// The reconcile plane is the mirror image: work and rigs, no binding. It is the
// lane that converges the ledger, and the binding is already the runtime plane's
// to keep fresh.
//
// # What reading the binding means for a workflow root
//
// carriedPoolRoute's legacy arm restores gc.routed_to on a gc.kind=workflow root
// whose gc.run_target is set and gc.routed_to empty — the pre-ga-eld2x relic
// shape, and the graph binding is where those roots live. That is not a new
// hazard, it is the same repair `gc doctor --fix`'s run-target-routed-to-backfill
// already performs there, and it is the operator ruling's whole point: routed
// work lives ONLY in the graph store, so the binding is where a lost route can
// be lost. The re-stamp-a-blocked-root failure (gc-4zb) is guarded where it
// always was — the Live raw-status filter on the open read, not by which store
// is asked.
//
// Expressing the invariant as a leg filter here, rather than as a new intent in
// internal/storeref, is deliberate. Plan(RoutedWork) orders the binding LAST on
// purpose (#5148 co-residence), and a runtime-plane intent that structurally
// refuses ledger legs is the resolver's own relevance-descriptor work — the S4
// surface this slice was told not to grow. TODO(ga-l7jdg/ga-qdt5y): move this
// refusal into Plan() when that descriptor lands, so a runtime-plane caller
// cannot even be HANDED a ledger leg.
func walkRouteRecoveryLegs(plan storeref.ResolvedPlan, plane storePlane, visit func(routeRecoveryLeg) error) (partial bool, err error) {
	bindingOnly := plane == runtimePlane && plan.TouchesBinding()
	result, walkErr := storeref.Walk(plan, func(leg storeref.Leg) (bool, error) {
		if leg.Store == nil || !planeReadsLeg(plane, leg.Ref, bindingOnly) {
			return false, nil
		}
		return false, visit(routeRecoveryLeg{label: routeRecoveryLegLabel(leg.Ref), store: leg.Store})
	})
	return result.Partial, walkErr
}

// planeReadsLeg is the per-leg half of the invariant.
func planeReadsLeg(plane storePlane, ref storeref.StoreRef, bindingOnly bool) bool {
	isBinding := storeref.IsClassRef(string(ref))
	if plane == runtimePlane {
		// Binding-only where a binding exists; otherwise the single-store city's
		// work store, which is its infra store.
		return isBinding || !bindingOnly
	}
	// The convergence lane owns the ledger and the rigs. The binding is the
	// runtime plane's, and re-reading it here would only duplicate that work.
	return !isBinding
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
	partial, walkErr := walkRouteRecoveryLegs(plan, runtimePlane, func(leg routeRecoveryLeg) error {
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
	return l.backstopPassOnPlane(plan, reason, reconcilePlane)
}

// backstopPassOnPlane is the scan restricted to one plane's legs. Only the
// convergence lane's plane is used in production; the parameter exists so the
// invariant can be asserted from both sides of it.
func (l *routeRecoveryLane) backstopPassOnPlane(plan storeref.ResolvedPlan, reason string, plane storePlane) routeRecoveryReport {
	report := routeRecoveryReport{lane: "backstop", reason: reason}
	var errs []error
	partial, walkErr := walkRouteRecoveryLegs(plan, plane, func(leg routeRecoveryLeg) error {
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
	l.pruneRestoresLocked()
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
		writes[beadmeta.RouteQuarantineMetadataKey] = ""
		writes[beadmeta.RouteQuarantineReasonMetadataKey] = ""
	}
	if err := store.SetMetadataBatch(live.ID, writes); err != nil {
		return routeRestoreOutcome{writes: 1, err: fmt.Errorf("bead %s: restoring gc.routed_to=%q: %w", live.ID, route, err)}
	}
	return routeRestoreOutcome{restored: true, writes: 1}
}

// pruneRestoresLocked bounds the per-bead restore tally, which otherwise grows
// for the life of the controller.
//
// A single restore is the normal outcome, not a flap, so those entries are the
// ones worth forgetting. If forgetting them is not enough the whole tally is
// dropped: flap detection restarting is a degraded diagnostic, and an unbounded
// map in a process that runs for weeks is a defect.
func (l *routeRecoveryLane) pruneRestoresLocked() {
	if len(l.restores) < routeRecoveryCandidateCap {
		return
	}
	for id, count := range l.restores {
		if count <= 1 {
			delete(l.restores, id)
		}
	}
	if len(l.restores) >= routeRecoveryCandidateCap {
		l.restores = map[string]int{}
	}
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
	if strings.TrimSpace(bead.Metadata[beadmeta.RouteQuarantineReasonMetadataKey]) == reason &&
		isRouteRecoveryQuarantined(bead) {
		return false, nil
	}
	err := store.SetMetadataBatch(bead.ID, map[string]string{
		beadmeta.RouteQuarantineMetadataKey:       "true",
		beadmeta.RouteQuarantineReasonMetadataKey: reason,
	})
	if err != nil {
		return false, fmt.Errorf("bead %s: marking route-recovery quarantine (%s): %w", bead.ID, reason, err)
	}
	return true, nil
}

// isRouteRecoveryQuarantined reports whether a bead already carries the marker.
func isRouteRecoveryQuarantined(b beads.Bead) bool {
	return strings.TrimSpace(b.Metadata[beadmeta.RouteQuarantineMetadataKey]) == "true"
}
