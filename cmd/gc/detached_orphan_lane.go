package main

// The detached-handoff-orphan lane: events for freshness, a cadenced scan for
// convergence — the third application of the split route recovery and the
// completions reconcile already run.
//
// # What was wrong
//
// sweepDetachedHandoffOrphans repairs a work bead whose failed done sequence
// cleared assignee and gc.routed_to together, leaving it invisible to pool
// demand. That is a CONVERGENCE repair of a rare failure, and it was wired to
// run at DELTA cadence: a cache-bypassing full live open-corpus scan of the city
// work ledger AND every rig store, serially, on every controller tick.
//
// On maintainer-city that leg measured 180.8s of a 373s tick — 48.5% — dead
// constant across ticks with restored_count=0 on every one (ga-l7jdg, post-S1/S2
// profile). The variance is the tell, the same tell as its sibling: a fixed-size
// scan of a corpus that does not change.
//
// The in-code note that "in steady state there are no candidates, so the
// expensive session-index lookup is skipped entirely" is true and misses the
// cost. The session-index lookup is skipped; the Live open scan that FINDS the
// candidates is not, and that scan is the whole leg.
//
// route_recovery.go:71 already said the two were kin ("Mirrors
// sweepDetachedHandoffOrphans"). This is that treatment, applied.
//
// # The split
//
//   - The DELTA pass runs in the tick. Its candidates are the beads the journal
//     named whose snapshot carries the detached-orphan signature, and nothing
//     else. A steady tick names nothing, builds no plan, and issues no read.
//   - The BACKSTOP pass is the old scan, unchanged in what it repairs, demoted to
//     a background lane on an hourly cadence — plus, immediately, on every way
//     the event feed can lie: startup, a cursor gap, a watcher that could not
//     start or restarted, a candidate overflow, and a leg that errored last pass.
//
// Unlike its route-recovery sibling there is no SYNCHRONOUS startup scan. The
// sibling has one because ga-n2d.4's restart-recovery contract is a startup
// backstop on the readiness path; this scan has no such contract and is the
// 180.8s leg, so putting it on the boot path would trade a tick cost for a boot
// cost. The startup pass runs on the background lane instead, within one poll
// interval of the feed being armed.
//
// # Two things the pre-lane sweep could not reach, and now does
//
// The old sweep took the city store plus the rig store map. On a converged split
// city that set does not include the class binding, so a binding-resident orphan
// had no lane at all. The backstop plans over storeref.RoutedWork on the
// reconcile plane, which reads every leg INCLUDING the binding — the plane
// doctrine's asymmetry (planeReadsLeg): the runtime plane narrows for latency,
// the reconcile plane never narrows, because a leg it skips is a leg nothing
// converges.
//
// It also rebuilt the session route index once per scope. The index is now built
// at most once per pass, lazily, and only when a candidate survives its live
// re-check — so the ordinary pass that finds nothing pays for nothing.

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
	// detachedOrphanBackstopInterval is how often the authoritative scan runs
	// when nothing forces it sooner. Hourly, matching its sibling lane: this is
	// a convergence backstop for a failure that is rare by construction.
	detachedOrphanBackstopInterval = time.Hour

	// detachedOrphanBackstopRetryInterval is the cadence after a pass that could
	// not read every leg, so a dark ledger does not make the rigs wait out the
	// full hour.
	detachedOrphanBackstopRetryInterval = 5 * time.Minute

	// detachedOrphanCandidateCap bounds the pending candidate set. Overflow is a
	// cursor gap: the feed can no longer claim to name everything, so the scan
	// answers instead of candidates being dropped.
	detachedOrphanCandidateCap = 4096
)

// detachedOrphanReport is one pass's outcome in the terms the tick trace and the
// operator log need.
type detachedOrphanReport struct {
	lane       string
	reason     string
	candidates int
	restored   int
	// legReads counts store round trips this pass issued — the unit the tick's
	// latency is actually measured in, and what the budget golden asserts on.
	legReads int
	// legs counts the plan legs this pass was allowed to read. A pass reporting
	// zero legs converged nothing, which must not read as "nothing to converge".
	legs int
	// unresolved counts named candidates this pass could not repair: the bead
	// lives on a leg this plane refuses, it was claimed or closed since its
	// event, or its session carries no recoverable route. All three wait for the
	// convergence lane, which is why one counter serves.
	unresolved int
	duration   time.Duration
	partial    bool
	err        error
}

// fields renders the report for the reconciler trace. restored_count keeps its
// pre-lane spelling so an operator's existing query still resolves.
func (r detachedOrphanReport) fields() map[string]any {
	out := map[string]any{
		"lane":           r.lane,
		"restored_count": r.restored,
		"candidates":     r.candidates,
		"leg_reads":      r.legReads,
		"legs":           r.legs,
	}
	if r.unresolved > 0 {
		out["unresolved"] = r.unresolved
	}
	if r.reason != "" {
		out["reason"] = r.reason
	}
	if r.partial {
		out["partial"] = true
	}
	return out
}

// detachedOrphanLane holds the cadence and candidate state the two passes share.
// It owns no stores and opens nothing: a caller hands it the plan for the pass,
// which keeps the suspension frame told-not-decided exactly as the census arms
// and the route-recovery lane do.
type detachedOrphanLane struct {
	mu sync.Mutex

	// passMu admits ONE authoritative scan at a time. The scan outlives several
	// poller ticks on a large city, and stacking scans would multiply the ledger
	// load to converge once.
	passMu sync.Mutex

	pending map[string]struct{}

	forced       bool
	forcedReason string

	lastBackstopAt     time.Time
	lastBackstopReason string
	backstopRan        bool
	retrySoon          bool

	interval time.Duration
	retry    time.Duration
	poll     time.Duration
}

func newDetachedOrphanLane() *detachedOrphanLane {
	return &detachedOrphanLane{
		pending:  map[string]struct{}{},
		interval: detachedOrphanBackstopInterval,
		retry:    detachedOrphanBackstopRetryInterval,
		poll:     backstopPollInterval,
		// Nothing has converged yet, so the first thing this lane does is scan.
		forced:       true,
		forcedReason: backstopReasonStartup,
	}
}

// detachedOrphanLaneOf returns this runtime's lane, creating it on first use so
// a directly-constructed CityRuntime (every test, every one-shot) needs no
// wiring.
func (cr *CityRuntime) detachedOrphanLaneOf() *detachedOrphanLane {
	cr.detachedOrphanOnce.Do(func() { cr.detachedOrphan = newDetachedOrphanLane() })
	return cr.detachedOrphan
}

func (l *detachedOrphanLane) beginBackstop() bool { return l.passMu.TryLock() }
func (l *detachedOrphanLane) endBackstop()        { l.passMu.Unlock() }

// force marks the next backstop pass due immediately. Every way the event feed
// can stop naming everything funnels through here.
func (l *detachedOrphanLane) force(reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.forced = true
	if l.forcedReason == "" || l.forcedReason == backstopReasonCadence {
		l.forcedReason = reason
	}
}

// observe feeds one journal event to the delta lane, keeping the bead id only
// when the snapshot the event carries has the detached-orphan signature. A busy
// city's ordinary bead traffic therefore costs the tick nothing.
func (l *detachedOrphanLane) observe(evt events.Event) {
	switch evt.Type {
	case events.BeadCreated, events.BeadUpdated:
	default:
		return
	}
	bead, ok := beads.DecodeBeadEventPayload(evt.Payload)
	if !ok || strings.TrimSpace(bead.ID) == "" || !isDetachedHandoffOrphanCandidate(bead) {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.pending) >= detachedOrphanCandidateCap {
		// The feed can no longer claim to name everything. Hand the question to
		// the scan rather than silently dropping candidates.
		l.pending = map[string]struct{}{}
		l.forced = true
		l.forcedReason = backstopReasonCursorGap
		return
	}
	l.pending[bead.ID] = struct{}{}
}

// takePending drains the candidate set.
func (l *detachedOrphanLane) takePending() []string {
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
func (l *detachedOrphanLane) backstopDue(now time.Time) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.forced {
		reason := l.forcedReason
		if reason == "" {
			reason = backstopReasonCursorGap
		}
		return reason, true
	}
	if !l.backstopRan {
		return backstopReasonStartup, true
	}
	cadence := l.interval
	if l.retrySoon {
		cadence = l.retry
	}
	if now.Sub(l.lastBackstopAt) >= cadence {
		return backstopReasonCadence, true
	}
	return "", false
}

// lastBackstop reports when the authoritative scan last ran, why it was due, and
// whether one ever has. It is what the tick's trace record carries: a backstop
// whose age nobody can see is a backstop nobody notices has stopped.
func (l *detachedOrphanLane) lastBackstop() (at time.Time, reason string, ran bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastBackstopAt, l.lastBackstopReason, l.backstopRan
}

// noteBackstopRan records the pass and clears the force latch. A pass that could
// not read every leg reschedules itself on the short retry cadence: the leg it
// missed is exactly the one whose convergence is now overdue.
func (l *detachedOrphanLane) noteBackstopRan(now time.Time, reason string, partial bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lastBackstopAt = now
	l.lastBackstopReason = reason
	l.backstopRan = true
	l.forced = false
	l.forcedReason = ""
	l.retrySoon = partial
}

// pollEvery is how often the background loop asks whether the scan is due.
func (l *detachedOrphanLane) pollEvery() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.poll <= 0 {
		return backstopPollInterval
	}
	return l.poll
}

// detachedOrphanRouteResolver builds one leg's session route index at most once
// per pass, and only if a candidate actually needs it.
//
// Laziness is the point on the tick. The delta pass re-verifies its named
// candidates first; a pass whose candidates were all claimed or closed since
// their event never reads a session bead at all.
//
// The index comes from detachedOrphanRoutesFor — the same resolution the
// convergence scan uses — so the two lanes cannot disagree about which session
// bead answers for an orphan.
type detachedOrphanRouteResolver struct {
	store    beads.Store
	sessions beads.Store
	index    detachedOrphanRouteIndex
	built    bool
	reads    int
	err      error
}

// route resolves a candidate's pool route, building the leg's index on first use.
func (r *detachedOrphanRouteResolver) route(sessionID, sessionName string) (string, error) {
	if !r.built {
		r.built = true
		var reads int
		r.index, reads, r.err = detachedOrphanRoutesFor(r.store, r.sessions)
		r.reads += reads
	}
	if r.err != nil {
		return "", r.err
	}
	return r.index.route(sessionID, sessionName), nil
}

// deltaPass repairs only the beads the journal named since the last pass.
//
// The steady-state property this whole slice exists for lives in the first two
// lines: no candidates, no plan, no store read at all.
func (l *detachedOrphanLane) deltaPass(plan storeref.ResolvedPlan, sessions beads.Store, candidates []string) detachedOrphanReport {
	report := detachedOrphanReport{lane: "delta", candidates: len(candidates)}
	if len(candidates) == 0 {
		return report
	}
	var errs []error
	repaired := make(map[string]struct{}, len(candidates))
	partial, walkErr := walkPlaneLegs(plan, runtimePlane, func(leg planeLeg) error {
		report.legs++
		rows, reads, err := liveOpenCandidates(leg.store, candidates)
		report.legReads += reads
		if err != nil {
			return fmt.Errorf("re-reading %d detached-orphan candidate(s): %w", len(candidates), err)
		}
		resolver := &detachedOrphanRouteResolver{store: leg.store, sessions: sessions}
		defer func() { report.legReads += resolver.reads }()
		for _, row := range rows {
			// The LIVE row decides, not the event snapshot: a claim atomically
			// flips the bead to in_progress and consumes gc.routed_to (ga-sa0),
			// so re-stamping from a stale snapshot would hand the dispatcher a
			// phantom pool-demand bead that flaps (ga-bgu).
			if !isDetachedHandoffOrphanCandidate(row) {
				continue
			}
			outcome := restoreDetachedOrphanRoute(leg.store, row, resolver)
			report.legReads += outcome.writes
			if outcome.err != nil {
				errs = append(errs, outcome.err)
			}
			if outcome.restored {
				repaired[row.ID] = struct{}{}
				report.restored++
			}
		}
		return nil
	})
	report.unresolved = len(candidates) - len(repaired)
	report.partial = partial
	report.err = errors.Join(append(errs, walkErr)...)
	return report
}

// backstopPass is the authoritative convergence scan across every plan leg the
// reconcile plane reads.
func (l *detachedOrphanLane) backstopPass(plan storeref.ResolvedPlan, sessions beads.Store, reason string) detachedOrphanReport {
	return l.backstopPassOnPlane(plan, sessions, reason, reconcilePlane)
}

// backstopPassOnPlane is the scan restricted to one plane's legs. Only the
// convergence lane's plane is used in production; the parameter exists so the
// invariant can be asserted from both sides of it.
//
// Each leg is the pre-lane sweep verbatim — the full live open-corpus read, the
// gc-4zb Live filter, the ga-bgu cache-bypassing re-read before every write. The
// lane changed WHEN it runs and WHICH legs it covers, not what it repairs.
func (l *detachedOrphanLane) backstopPassOnPlane(plan storeref.ResolvedPlan, sessions beads.Store, reason string, plane storePlane) detachedOrphanReport {
	report := detachedOrphanReport{lane: "backstop", reason: reason}
	var errs []error
	partial, walkErr := walkPlaneLegs(plan, plane, func(leg planeLeg) error {
		report.legs++
		result, err := sweepDetachedHandoffOrphansWithRouteStore(leg.store, sessions)
		report.candidates += result.candidates
		report.restored += result.restored
		report.legReads += result.reads
		if err != nil {
			return fmt.Errorf("%s: %w", leg.label, err)
		}
		return nil
	})
	report.unresolved = report.candidates - report.restored
	report.partial = partial
	report.err = errors.Join(append(errs, walkErr)...)
	return report
}

// detachedOrphanRestoreOutcome is what one candidate's repair decided.
type detachedOrphanRestoreOutcome struct {
	restored bool
	writes   int
	err      error
}

// restoreDetachedOrphanRoute re-stamps gc.routed_to from the route the LIVE row's
// session bead declares. It is the only place the delta lane writes a route.
func restoreDetachedOrphanRoute(store beads.Store, live beads.Bead, resolver *detachedOrphanRouteResolver) detachedOrphanRestoreOutcome {
	sessionID := strings.TrimSpace(live.Metadata[beadmeta.SessionIDMetadataKey])
	sessionName := strings.TrimSpace(live.Metadata[beadmeta.SessionNameMetadataKey])
	route, err := resolver.route(sessionID, sessionName)
	if err != nil {
		return detachedOrphanRestoreOutcome{err: fmt.Errorf("bead %s: %w", live.ID, err)}
	}
	if route == "" {
		// No recoverable route: the session bead is gone, carries no template, or
		// its name is ambiguous. Counted as unresolved rather than logged per
		// tick — the convergence scan says so once, loudly, with its own line.
		return detachedOrphanRestoreOutcome{}
	}
	if setErr := store.SetMetadata(live.ID, beadmeta.RoutedToMetadataKey, route); setErr != nil {
		return detachedOrphanRestoreOutcome{writes: 1, err: fmt.Errorf("bead %s: restoring gc.routed_to=%q: %w", live.ID, route, setErr)}
	}
	return detachedOrphanRestoreOutcome{restored: true, writes: 1}
}

// detachedOrphanPlan resolves the work legs an orphan-repair pass reads.
//
// It is the SAME plan the route-recovery lane resolves, and deliberately so: a
// bead whose gc.routed_to was cleared is invisible to pool demand for the same
// reason whichever lane lost it, so "which stores hold claimable/routed work" is
// the same question. Two derivations of it would be the split-store bug class
// (#5125, #5127) rather than a second opinion.
func (cr *CityRuntime) detachedOrphanPlan() (storeref.ResolvedPlan, error) {
	return cr.routeRecoveryPlan()
}

// detachedOrphanSessionStore is the cross-store half of route resolution: the
// session beads a route is recovered FROM when the leg holding the orphan does
// not carry them.
//
// The session CLASS store, where the pre-lane sweep hard-coded the city store.
// Session beads are class-owned, so on a converged split city the city work
// store no longer holds them and the pre-lane rule resolved nothing there. On a
// city that relocates no class this IS the city store, so the resolution is
// byte-identical to the pre-lane one.
func (cr *CityRuntime) detachedOrphanSessionStore() beads.Store {
	return cr.sessionsBeadStore().Store
}

// sweepDetachedHandoffOrphansDelta is the tick's leg: it repairs only the beads
// the event feed named since the last pass. A steady tick names nothing, builds
// no plan, and issues no store read.
func (cr *CityRuntime) sweepDetachedHandoffOrphansDelta() detachedOrphanReport {
	lane := cr.detachedOrphanLaneOf()
	candidates := lane.takePending()
	if len(candidates) == 0 {
		// deltaPass guards this too, and deliberately: that guard is its API
		// contract (it takes a plan and must be cheap for any caller), while this
		// one is what keeps a steady tick from BUILDING the plan at all.
		return detachedOrphanReport{lane: "delta"}
	}
	plan, err := cr.detachedOrphanPlan()
	if err != nil {
		fmt.Fprintf(cr.stderr, "%s: detached handoff orphan sweep: resolving work legs: %v\n", cr.logPrefix, err) //nolint:errcheck // best-effort stderr
		lane.force(backstopReasonCursorGap)
		return detachedOrphanReport{lane: "delta", candidates: len(candidates), err: err}
	}
	report := lane.deltaPass(plan, cr.detachedOrphanSessionStore(), candidates)
	if report.partial || report.err != nil {
		// A leg the delta pass could not read is a leg whose convergence is now
		// owed to the scan.
		lane.force(backstopReasonLegDegrade)
	}
	cr.logDetachedOrphanSweep(report)
	return report
}

// runDetachedOrphanBackstop executes one authoritative pass and reports it.
func (cr *CityRuntime) runDetachedOrphanBackstop(reason string) detachedOrphanReport {
	lane := cr.detachedOrphanLaneOf()
	if !lane.beginBackstop() {
		// Another pass is already reading the same state. On a large city the
		// scan outlives several poller ticks, and stacking scans would multiply
		// the ledger load to converge once.
		return detachedOrphanReport{lane: "backstop", reason: reason}
	}
	defer lane.endBackstop()
	plan, err := cr.detachedOrphanPlan()
	if err != nil {
		// A refused city is the one case Plan declines to answer, and its remedy
		// is in the error. Scanning the work ledger anyway would be the answer
		// that looks like success while reading the store the relocated classes
		// were migrated off.
		fmt.Fprintf(cr.stderr, "%s: detached handoff orphan sweep: resolving work legs: %v\n", cr.logPrefix, err) //nolint:errcheck // best-effort stderr
		lane.force(backstopReasonCursorGap)
		return detachedOrphanReport{lane: "backstop", reason: reason, err: err}
	}
	started := time.Now()
	report := lane.backstopPass(plan, cr.detachedOrphanSessionStore(), reason)
	report.duration = time.Since(started)
	lane.noteBackstopRan(time.Now(), reason, report.partial || report.err != nil)
	cr.logDetachedOrphanSweep(report)
	// The convergence lane always says it ran. A clean pass that logs nothing is
	// indistinguishable from a lane that stopped running, and this one runs on a
	// background goroutine where nothing else would notice.
	summary := fmt.Sprintf("pass reason=%s legs=%d reads=%d candidates=%d restored=%d unresolved=%d partial=%t took=%s",
		reason, report.legs, report.legReads, report.candidates, report.restored, report.unresolved, report.partial,
		report.duration.Round(time.Millisecond))
	fmt.Fprintf(cr.stderr, "%s: detached handoff orphan sweep (backstop): %s\n", cr.logPrefix, summary) //nolint:errcheck // best-effort stderr
	return report
}

// runDetachedOrphanBackstopLoop polls the convergence cadence off-tick.
func (cr *CityRuntime) runDetachedOrphanBackstopLoop(ctx context.Context, lane *detachedOrphanLane) {
	ticker := time.NewTicker(lane.pollEvery())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if reason, due := lane.backstopDue(time.Now()); due {
			cr.safeTick(func() { cr.runDetachedOrphanBackstop(reason) }, "detached-orphan-backstop")
		}
	}
}

// logDetachedOrphanSweep emits the operator-facing lines. The restored line keeps
// its pre-lane wording so an operator's grep still finds it.
func (cr *CityRuntime) logDetachedOrphanSweep(report detachedOrphanReport) {
	if cr.stderr == nil {
		return
	}
	if report.err != nil {
		fmt.Fprintf(cr.stderr, "%s: detached handoff orphan sweep (%s): %v\n", cr.logPrefix, report.lane, report.err) //nolint:errcheck // best-effort stderr
	}
	if report.restored > 0 {
		fmt.Fprintf(cr.stderr, "%s: detached handoff orphan sweep (%s): restored gc.routed_to on %d bead(s)\n", cr.logPrefix, report.lane, report.restored) //nolint:errcheck // best-effort stderr
	}
}
