package main

// The orphaned-pool-assignment release lane: a delta pass in the tick, a
// convergence backstop off it — the fourth application of the split route
// recovery, the completions reconcile and the detached-orphan sweep already run
// (ga-l7jdg), here for release_orphaned_pool_assignments (ga-8uf72n, ruled in
// ga-57er0d).
//
// # What was wrong
//
// releaseOrphanedPoolAssignments reopens active pool-routed work whose
// assignee no longer maps to any open session. That is a CONVERGENCE repair of a
// rare failure — a worker's session closed or died while it still held a claim —
// and it ran at DELTA cadence: every candidate in the assigned-work snapshot
// went through the in-memory identity checks, the foreign-claim gate, and a live
// per-assignee session-store probe on every controller tick, to release nothing
// on almost all of them. On gc-management that probe loop alone measured 645-725s
// of a 30s patrol interval (ga-451jnv) before it was memoized; the memo bounded
// the cost per tick, it did not make the tick stop paying it.
//
// # The split
//
//   - The DELTA pass runs in the tick. Its candidates are the assignees of
//     sessions that were open on the previous tick and are not now — the one
//     event that turns a live claim into an orphan. A steady tick sees no such
//     transition, so it has no candidate, builds no plan and issues no store read.
//     It runs in the tick, not beside it, so the tick's own tail keeps working:
//     the dead-assignee event and the snapshot filter that drops released work
//     from this tick's demand.
//   - The BACKSTOP pass is the old sweep, unchanged in what it repairs, run off
//     the tick over the inputs the last tick PUBLISHED. It is not hourly like its
//     siblings: the delta trigger only sees a session that lived across two
//     ticks, so an orphan of a session that opened and closed between ticks, or
//     of a path the delta does not observe (ga-2fpf9z), is converged by this
//     pass alone, and an hour of stranded work is too long. Low minutes.
//
// # Why a stale published snapshot is safe
//
// The backstop reads inputs the tick built some seconds or minutes ago. Two
// properties make that safe, and both are tested:
//
//   - The release loop re-verifies every candidate LIVE before it writes
//     (liveWorkAssignmentStillReleasable, the memoized live session probe, and
//     the conditional release). A snapshot that is stale about a bead can only
//     make the pass look at a bead it then declines to release.
//   - The wake-protected set is published with the snapshot and consulted before
//     every release write, by the backstop exactly as by the tick. It was
//     computed from the same pre-tick session snapshot as the release inputs,
//     and a set that is out of date can only RETAIN work the wake arm no longer
//     serves — which the next tick's published set corrects. It can never cause a
//     release.
//
// The lane owns a copy of everything it publishes: the tick goes on to filter
// and reslice its own assigned-work slices after the call site.

import (
	"context"
	"fmt"
	"log"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

const (
	// orphanReleaseBackstopInterval is how often the authoritative release pass
	// runs when nothing forces it sooner. Low minutes, well below the hourly
	// precedent of the sibling lanes: the delta trigger under-fires (see the
	// package comment), so this cadence is the bound on how long a true orphan can
	// stay assigned to a dead session.
	orphanReleaseBackstopInterval = 5 * time.Minute

	// orphanReleaseBackstopRetryInterval is the cadence after a pass that could not
	// run to completion — a partial input snapshot, a lane whose inputs were not
	// yet published — so a dark ledger does not make the fleet wait out the full
	// interval.
	orphanReleaseBackstopRetryInterval = 2 * time.Minute

	// orphanReleaseCandidateCap bounds the pending assignee set. Overflow means
	// the delta can no longer claim to name everything, so the backstop answers
	// instead of candidates being dropped.
	orphanReleaseCandidateCap = 4096

	// orphanReleaseSkipUnpublished and orphanReleaseSkipPartial are the reasons a
	// pass declined to release anything. A pass over missing inputs converges
	// nothing, which must not read as "nothing to converge".
	orphanReleaseSkipUnpublished = "unpublished"
	orphanReleaseSkipPartial     = "partial-snapshot"
)

// orphanReleaseInputs is the bundle of tick-built inputs one release pass reads.
// It is exactly what the in-tick sweep took as parameters, gathered so the tick
// can hand it to the backstop.
type orphanReleaseInputs struct {
	store        beads.WorkStore
	sessionStore beads.SessionStore
	cfg          *config.City
	cityPath     string
	openInfos    []session.Info
	// result carries the assigned-work snapshot (AssignedWorkBeads, Stores,
	// StoreRefs) and the partial flags that gate the pass.
	result DesiredStateResult
	// protected is the wake-candidate set the release arm must not reopen.
	protected map[storeScopedBeadKey]struct{}
}

// clone returns inputs the caller can go on mutating without touching the lane.
func (in orphanReleaseInputs) clone() orphanReleaseInputs {
	out := in
	out.openInfos = slices.Clone(in.openInfos)
	out.protected = maps.Clone(in.protected)
	out.result = DesiredStateResult{
		StoreQueryPartial:     in.result.StoreQueryPartial,
		SessionQueryPartial:   in.result.SessionQueryPartial,
		AssignedWorkBeads:     cloneOrphanReleaseBeads(in.result.AssignedWorkBeads),
		AssignedWorkStores:    slices.Clone(in.result.AssignedWorkStores),
		AssignedWorkStoreRefs: slices.Clone(in.result.AssignedWorkStoreRefs),
	}
	return out
}

func cloneOrphanReleaseBeads(in []beads.Bead) []beads.Bead {
	if in == nil {
		return nil
	}
	out := make([]beads.Bead, len(in))
	for i, b := range in {
		b.Metadata = maps.Clone(b.Metadata)
		b.Labels = slices.Clone(b.Labels)
		b.Needs = slices.Clone(b.Needs)
		b.Dependencies = slices.Clone(b.Dependencies)
		out[i] = b
	}
	return out
}

// orphanReleaseReport is one pass's outcome in the terms the tick trace and the
// operator log need.
type orphanReleaseReport struct {
	lane   string
	reason string
	// skipped is why the pass declined to release (orphanReleaseSkip*); empty
	// when it ran.
	skipped    string
	candidates int
	released   []releasedPoolAssignment
	// assigned is the slice released[i].Index points into: the caller's full
	// assigned-work slice for the delta, the published copy for the backstop.
	assigned []beads.Bead
	duration time.Duration
}

// fields renders the report for the reconciler trace. released_count keeps its
// pre-lane spelling so an operator's existing query still resolves.
func (r orphanReleaseReport) fields() map[string]any {
	out := map[string]any{
		"lane":           r.lane,
		"released_count": len(r.released),
		"candidates":     r.candidates,
	}
	if r.reason != "" {
		out["reason"] = r.reason
	}
	if r.skipped != "" {
		out["skipped"] = r.skipped
	}
	return out
}

// orphanReleaseLane holds the cadence, candidate and published-input state the
// two passes share. It owns no stores: the tick hands it the inputs for the
// pass, which keeps the suspension frame told-not-decided as the sibling lanes
// do.
type orphanReleaseLane struct {
	mu sync.Mutex

	// passMu admits ONE authoritative pass at a time; stacking passes would
	// multiply the live-probe load to converge once.
	passMu sync.Mutex

	// pending is the assignee identities whose session just went away.
	pending map[string]struct{}
	// openByID is the identities of every session open at the last observation,
	// keyed by session bead ID; nil until the first observation, which has
	// nothing to diff against.
	openByID map[string][]string
	observed bool

	published *orphanReleaseInputs

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

func newOrphanReleaseLane() *orphanReleaseLane {
	return &orphanReleaseLane{
		pending:  map[string]struct{}{},
		interval: orphanReleaseBackstopInterval,
		retry:    orphanReleaseBackstopRetryInterval,
		poll:     clock.Backstop(backstopPollInterval),
		// Nothing has converged yet, so the first thing this lane does is a pass.
		forced:       true,
		forcedReason: backstopReasonStartup,
	}
}

// orphanReleaseLaneOf returns this runtime's lane, creating it on first use so a
// directly-constructed CityRuntime (every test, every one-shot) needs no wiring.
func (cr *CityRuntime) orphanReleaseLaneOf() *orphanReleaseLane {
	cr.orphanReleaseOnce.Do(func() {
		cr.orphanRelease = newOrphanReleaseLane()
	})
	return cr.orphanRelease
}

func (l *orphanReleaseLane) beginBackstop() bool { return l.passMu.TryLock() }
func (l *orphanReleaseLane) endBackstop()        { l.passMu.Unlock() }

// force marks the next backstop pass due immediately.
func (l *orphanReleaseLane) force(reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.forceLocked(reason)
}

func (l *orphanReleaseLane) forceLocked(reason string) {
	l.forced = true
	if l.forcedReason == "" || l.forcedReason == backstopReasonCadence {
		l.forcedReason = reason
	}
}

// observeSessions is the delta trigger. It diffs the open sessions of this tick
// against the previous observation and queues the assignee identities of every
// session that is no longer open — closed, or confirmed dead and so dropped from
// the open set. The first observation has nothing to diff against and queues
// nothing; a steady tick queues nothing.
//
// The queued identities are candidates, not verdicts: the release loop still
// asks whether any other open session answers to the identity (a template
// identity outlives any one of its sessions).
func (l *orphanReleaseLane) observeSessions(infos []session.Info, cfg *config.City) {
	current := make(map[string][]string, len(infos))
	for _, info := range infos {
		if info.Closed {
			continue
		}
		current[info.ID] = sessionBeadAssigneeIdentitiesForConfigInfo(info, cfg)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.observed {
		for id, identities := range l.openByID {
			if _, stillOpen := current[id]; !stillOpen {
				l.queueLocked(identities)
			}
		}
	}
	l.openByID = current
	l.observed = true
}

// queueAssignees marks assignee identities as candidates for the next delta pass.
func (l *orphanReleaseLane) queueAssignees(identities ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.queueLocked(identities)
}

func (l *orphanReleaseLane) queueLocked(identities []string) {
	for _, id := range identities {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if len(l.pending) >= orphanReleaseCandidateCap {
			// The delta can no longer claim to name everything. Hand the question
			// to the backstop rather than silently dropping candidates.
			l.pending = map[string]struct{}{}
			l.forceLocked(backstopReasonCursorGap)
			return
		}
		l.pending[id] = struct{}{}
	}
}

// takePending drains the candidate set, sorted.
func (l *orphanReleaseLane) takePending() []string {
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

// publish stores a private copy of the tick's inputs for the backstop to read.
func (l *orphanReleaseLane) publish(in orphanReleaseInputs) {
	cp := in.clone()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.published = &cp
}

// publishedInputs returns the last published inputs. They are never mutated
// after publish, so the caller may read them without holding the lane's mutex.
func (l *orphanReleaseLane) publishedInputs() (*orphanReleaseInputs, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.published, l.published != nil
}

// backstopDue reports whether the authoritative pass should run now, and why.
func (l *orphanReleaseLane) backstopDue(now time.Time) (string, bool) {
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

// lastBackstop reports when the authoritative pass last ran, why it was due, and
// whether one ever has. It is what the tick's trace record carries: a backstop
// whose age nobody can see is a backstop nobody notices has stopped.
func (l *orphanReleaseLane) lastBackstop() (at time.Time, reason string, ran bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastBackstopAt, l.lastBackstopReason, l.backstopRan
}

// noteBackstopRan records the pass and clears the force latch. A pass that could
// not run to completion reschedules itself on the short retry cadence.
func (l *orphanReleaseLane) noteBackstopRan(now time.Time, reason string, partial bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lastBackstopAt = now
	l.lastBackstopReason = reason
	l.backstopRan = true
	l.forced = false
	l.forcedReason = ""
	l.retrySoon = partial
}

// pollEvery is how often the background loop asks whether the pass is due.
func (l *orphanReleaseLane) pollEvery() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.poll <= 0 {
		return backstopPollInterval
	}
	return l.poll
}

// release runs the sweep over one assigned-work snapshot. The rig-store fallback
// is nil: every production snapshot carries per-bead stores aligned with its
// work, which is what the sweep resolves liveness and writes through.
func (l *orphanReleaseLane) release(in orphanReleaseInputs, res DesiredStateResult, keeps *deadAssigneeKeepLog) []releasedPoolAssignment {
	return releaseOrphanedPoolAssignments(in.store, in.sessionStore, in.cfg, in.cityPath, in.openInfos, res.AssignedWorkBeads, res.AssignedWorkStores, res.AssignedWorkStoreRefs, nil, in.protected, nil, keeps)
}

// deltaPass evaluates only the assigned work whose assignee lost a session since
// the last pass.
//
// The steady-state property this lane exists for lives in the first lines: no
// pending assignee, no evaluation, no store read.
func (l *orphanReleaseLane) deltaPass(in orphanReleaseInputs, keeps *deadAssigneeKeepLog) orphanReleaseReport {
	report := orphanReleaseReport{lane: "delta", assigned: in.result.AssignedWorkBeads}
	pending := l.takePending()
	if len(pending) == 0 {
		return report
	}
	if in.result.snapshotQueryPartial() {
		// Missing work or sessions make active work look orphaned for this tick
		// only. The identities stay owed: requeue them for the next complete tick.
		l.queueAssignees(pending...)
		report.skipped = orphanReleaseSkipPartial
		return report
	}
	want := make(map[string]struct{}, len(pending))
	for _, id := range pending {
		want[id] = struct{}{}
	}
	all := in.result.AssignedWorkBeads
	var picked []int
	for i, wb := range all {
		if _, ok := want[strings.TrimSpace(wb.Assignee)]; ok {
			picked = append(picked, i)
		}
	}
	report.candidates = len(picked)
	if len(picked) == 0 {
		return report
	}
	work := make([]beads.Bead, len(picked))
	for n, i := range picked {
		work[n] = all[i]
	}
	narrowed := DesiredStateResult{
		AssignedWorkBeads:     work,
		AssignedWorkStores:    pickAligned(in.result.AssignedWorkStores, picked, len(all), "stores"),
		AssignedWorkStoreRefs: pickAligned(in.result.AssignedWorkStoreRefs, picked, len(all), "store refs"),
	}
	released := l.release(in, narrowed, keeps)
	// The release loop indexes into the narrowed slice; callers index into the
	// full one.
	for k := range released {
		released[k].Index = picked[released[k].Index]
	}
	report.released = released
	return report
}

// pickAligned narrows an index-aligned side slice to the picked entries. A slice
// that is not aligned with the work (absent, or the length-mismatch the release
// loop reports on its own) is passed through as absent so the loop's
// storeAware/storeRefAware detection sees the same shape it would have seen.
func pickAligned[T any](side []T, picked []int, n int, what string) []T {
	if len(side) == 0 {
		return nil
	}
	if len(side) != n {
		log.Printf("orphan release delta: assigned work/%s length mismatch: work=%d %s=%d", what, n, what, len(side))
		return nil
	}
	out := make([]T, len(picked))
	for k, i := range picked {
		out[k] = side[i]
	}
	return out
}

// backstopPass is the authoritative convergence pass over the last published
// inputs: the old sweep, every assigned bead, run off the tick.
func (l *orphanReleaseLane) backstopPass(reason string, keeps *deadAssigneeKeepLog) orphanReleaseReport {
	report := orphanReleaseReport{lane: "backstop", reason: reason}
	in, ok := l.publishedInputs()
	if !ok {
		report.skipped = orphanReleaseSkipUnpublished
		return report
	}
	report.assigned = in.result.AssignedWorkBeads
	if in.result.snapshotQueryPartial() {
		report.skipped = orphanReleaseSkipPartial
		return report
	}
	report.candidates = len(in.result.AssignedWorkBeads)
	report.released = l.release(*in, in.result, keeps)
	return report
}

// runOrphanReleaseBackstop executes one authoritative pass and reports it.
func (cr *CityRuntime) runOrphanReleaseBackstop(reason string) orphanReleaseReport {
	if cr.beadsQuiescent.Load() {
		// Suspended with nothing running: no store is read until resume.
		return orphanReleaseReport{lane: "backstop", reason: reason}
	}
	lane := cr.orphanReleaseLaneOf()
	if !lane.beginBackstop() {
		// Another pass is already reading the same state.
		return orphanReleaseReport{lane: "backstop", reason: reason}
	}
	defer lane.endBackstop()
	started := time.Now()
	report := lane.backstopPass(reason, &cr.deadAssigneeKeeps)
	report.duration = time.Since(started)
	if report.skipped == orphanReleaseSkipUnpublished {
		// No tick has published yet, so no pass ran: leave the startup latch set
		// and ask again at the next poll, rather than burning it on nothing.
		return report
	}
	lane.noteBackstopRan(time.Now(), reason, report.skipped != "")
	cr.logOrphanRelease(report)
	if cr.stderr != nil {
		// The convergence lane always says it ran. A clean pass that logs nothing
		// is indistinguishable from a lane that stopped running, and this one runs
		// on a background goroutine where nothing else would notice.
		fmt.Fprintf(cr.stderr, "%s: orphan release sweep (backstop): pass reason=%s candidates=%d released=%d skipped=%q took=%s\n", //nolint:errcheck // best-effort stderr
			cr.logPrefix, reason, report.candidates, len(report.released), report.skipped, report.duration.Round(time.Millisecond))
	}
	return report
}

// logOrphanRelease turns a pass's releases into the same operator line and
// dead-assignee event the in-tick sweep always produced.
func (cr *CityRuntime) logOrphanRelease(report orphanReleaseReport) {
	if len(report.released) == 0 {
		return
	}
	if cr.stderr != nil {
		for _, r := range report.released {
			fmt.Fprintf(cr.stderr, "released orphaned pool work: %s\n", r.ID) //nolint:errcheck // best-effort stderr
		}
	}
	emitDeadAssigneeReopenedEvents(cr.rec, report.assigned, report.released, time.Now())
}

// runOrphanReleaseBackstopLoop polls the convergence cadence off-tick.
func (cr *CityRuntime) runOrphanReleaseBackstopLoop(ctx context.Context, lane *orphanReleaseLane) {
	ticker := time.NewTicker(lane.pollEvery())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if reason, due := lane.backstopDue(time.Now()); due {
			cr.safeTick(func() { cr.runOrphanReleaseBackstop(reason) }, "orphan-release-backstop")
		}
	}
}
