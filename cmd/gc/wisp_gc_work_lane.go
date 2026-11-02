package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
)

// The wisp GC's WORK arm, on its own lane.
//
// On a city that relocates the graph class, a v1 POURED molecule is work class
// and lives on the work ledgers (the city work store and every rig store), not
// in the binding the graph arm sweeps. This arm closes abandoned ones and
// purges expired closed ones there.
//
// Every read here lands on a work ledger, which on maintainer-city is bd: one
// subprocess per read, about 0.75 s each. So the arm runs off the controller
// tick, on a paced background lane the tick only wakes, and each pass spends
// one budget shared across all legs (wispGCWorkArmWalkBudget walks and
// wispGCWorkArmWallBudget of wall clock), resuming where the last pass stopped
// (a rotating leg and root cursor). Outcomes that leave a root alone (a live
// subtree, a held stepless root, a refused or failed purge) are remembered by
// root id and updated_at, so a backlog of busy roots cannot take every walk of
// every pass.

// wispGCWorkArmWalkBudget is how many roots one work-arm pass walks, across
// every leg. Package var so tests can change it.
var wispGCWorkArmWalkBudget = 5

// wispGCWorkArmWallBudget is the wall clock one work-arm pass may spend
// walking, across every leg. Package var so tests can change it.
var wispGCWorkArmWallBudget = 5 * time.Second

// workMoleculeGC is the work arm's state, owned by its lane: the memo of
// outcomes and log lines, and the rotation cursors.
type workMoleculeGC struct {
	mu    sync.Mutex
	notes wispGCNotes
	// legCursor is the leg position the next pass starts at. It advances by
	// one EVERY pass, so a leg whose work outlasts the budget cannot pin the
	// lane: every leg is the first leg once every len(legs) passes.
	legCursor int
	// rootCursor is, per leg (keyed by its store ref) and arm, the last root
	// id walked, so a leg resumes where its last visit stopped.
	rootCursor map[string]string
	// purgeFirst is, per leg, which arm goes first on its next visit: the
	// arms alternate (with one walk a pass, each gets every other visit), and
	// a close that goes first with two or more walks left leaves one for the
	// purge, so neither arm can starve the other.
	purgeFirst map[string]bool
}

func newWorkMoleculeGC() *workMoleculeGC {
	return &workMoleculeGC{rootCursor: make(map[string]string), purgeFirst: make(map[string]bool)}
}

const (
	workArmCloseMemo = "work-close"
	workArmPurgeMemo = "work-purge"
)

// workArmLeg is one work leg the arm sweeps, named by its store ref.
type workArmLeg struct {
	ref   string
	store beads.Store
}

// pass runs one work-arm pass over legs (every leg but the graph store),
// purging closed roots past ttl. It returns how many closures it purged.
//
// Dry-run (GC_WISP_GC_WORK_MOLECULES unset) only LISTS and counts: no subtree
// or closure is walked, and one summary line is logged.
//
// The pass stops starting work as soon as ctx is done or the wall-clock budget
// is spent: no leg listing and no root walk starts past either (a leg's closed
// listing is not started once ctx is done either). Work already started is not
// interrupted: the controller's stores are not context-bound, and a walk reads
// its closure and deletes it as one unit. So a stop waits for the bd call in
// flight and, when a walk has started, for the rest of that walk: at most one
// walk, ~8 bd calls, ~6 s at the latency below. The deadline does not stop the
// pass's FIRST walk (ctx does), so a pass whose listings alone outlast the
// wall budget still makes progress; a pass that ends with no walk while legs
// were left unlisted says so in the log.
//
// Throughput, measured with a bd store answering in ~0.75 s per call (the
// maintainer-city figure): listing a leg is 3 calls, and walking and deleting
// a closure of a root and a step is 8 (the member list, one batched children
// read per level, the live strand guard on the root, the delete; the roots'
// rows come from one prefetch). The deadline never stops the first walk of a
// pass, so a pass that has work makes exactly one walk at that latency and
// runs ~8-11 s, over its 5 s wall budget by that one walk: about one closure
// per pass, ~6 an hour city-wide at the 10-minute wisp-GC cadence. That is a
// backstop rate for a backlog; the reactive autoclose and the graph arm do the
// routine work.
func (w *workMoleculeGC) pass(ctx context.Context, legs wispGCLegs, graph beads.Store, ttl time.Duration, now time.Time) (int, error) {
	if legs == nil || legs.unusable() != nil || ttl <= 0 {
		return 0, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.rootCursor == nil {
		w.rootCursor = make(map[string]string)
	}
	if w.purgeFirst == nil {
		w.purgeFirst = make(map[string]bool)
	}
	var ls []workArmLeg
	legs.eachLeg(func(ref string, store beads.Store) {
		if store != nil && !sameBeadStore(store, graph) {
			ls = append(ls, workArmLeg{ref: ref, store: store})
		}
	})
	if len(ls) == 0 {
		return 0, nil
	}
	enforce := workMoleculesEnforced()
	cutoff := now.Add(-ttl)
	budget := &wispGCBudget{ctx: ctx, walks: wispGCWorkArmWalkBudget, deadline: wispGCClock().Add(wispGCWorkArmWallBudget)}
	start := w.legCursor % len(ls)
	w.legCursor = (start + 1) % len(ls)

	purged, idle, expired, walked, listed := 0, 0, 0, 0, 0
	var errs error
	for i := 0; i < len(ls); i++ {
		if budget.stopped() || (enforce && budget.exhausted()) {
			// Out of time, stopping, or out of walks: the remaining legs are
			// not listed (a listing is bd calls too). The leg cursor already
			// moved, so they come first on a later pass.
			break
		}
		l := ls[(start+i)%len(ls)]
		open, err := enumerateWorkClassMoleculeRoots(l.store, "open", "in_progress")
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("listing open work-class molecule roots on %s: %w", l.ref, err))
		}
		if budget.ctxDone() {
			break
		}
		closed, cerr := enumerateWorkClassMoleculeRoots(l.store, "closed")
		if cerr != nil {
			errs = errors.Join(errs, fmt.Errorf("listing closed work-class molecule roots on %s: %w", l.ref, cerr))
		}
		listed++
		// The memos are kept per leg and pruned against that leg's own
		// listing, so a pass that lists only some legs (the usual case at bd
		// latency) still prunes them. A listing that failed reports no
		// roots; its memos are kept.
		closeMemo, purgeMemo := workArmMemo(workArmCloseMemo, l.ref), workArmMemo(workArmPurgeMemo, l.ref)
		if err == nil {
			w.notes.pruneRefusals(closeMemo, presentIDs(open))
		}
		if cerr == nil {
			w.notes.pruneRefusals(purgeMemo, presentIDs(closed))
		}
		if !enforce {
			idle += countIdleAbandonedCandidates(open, now)
			for _, root := range closed {
				if workArmPurgeAge(root).Before(cutoff) {
					expired++
				}
			}
			continue
		}
		closeKey, purgeKey := l.ref+"\x00close", l.ref+"\x00purge"
		closeWalked := map[string]bool{}
		runClose := func(reserve int) {
			if err != nil {
				return
			}
			budget.reserve = reserve
			defer func() { budget.reserve = 0 }()
			var candidates []beads.Bead
			for _, r := range rotateAfter(open, w.rootCursor[closeKey]) {
				if !closeWalked[r.ID] {
					candidates = append(candidates, r)
				}
			}
			holders := wispGCHolders{graph: graph, own: l.store}
			errs = errors.Join(errs, closeAbandonedRootCandidatesMode(l.store, candidates, holders, now, abandonedCloseMode{
				enforce:    closeAbandonedEnforced(),
				dryRunHint: fmt.Sprintf("set %s=1 to enforce", closeAbandonedEnv),
				label:      "work-class molecule root",
				budget:     budget,
				notes:      &w.notes,
				memo:       &w.notes,
				memoArm:    closeMemo,
				onWalk:     func(id string) { w.rootCursor[closeKey] = id; closeWalked[id] = true; walked++ },
			}))
		}
		runPurge := func() {
			if cerr != nil {
				return
			}
			n, perr := purgeExpiredBeadClosuresWith(l.store, rotateAfter(closed, w.rootCursor[purgeKey]), closurePurgeOpts{
				cutoff:       cutoff,
				batchCap:     wispGCClosurePurgeBatchCap,
				budget:       budget,
				age:          workArmPurgeAge,
				arm:          purgeMemo,
				notes:        &w.notes,
				now:          now,
				memoFailures: true,
				prefetch:     true,
				onWalk:       func(id string) { w.rootCursor[purgeKey] = id; walked++ },
			})
			purged += n
			errs = errors.Join(errs, perr)
		}
		walkedBefore := walked
		if w.purgeFirst[l.ref] {
			runPurge()
			runClose(0)
		} else {
			// The close goes first. With two or more walks left it leaves
			// one for the purge; with one, the alternation is what gives the
			// purge its turn on the next visit. A walk the purge did not need
			// goes back to the close.
			reserve := 0
			if budget.walks >= 2 {
				reserve = 1
			}
			runClose(reserve)
			runPurge()
			runClose(0)
		}
		// A visit the deadline or ctx cut before either arm could walk is
		// not a turn: flipping on it would let the cut visits fix which arm
		// the walking visits start with, and starve the other arm.
		if walked > walkedBefore || !budget.stopped() {
			w.purgeFirst[l.ref] = !w.purgeFirst[l.ref]
		}
	}
	// A pass reaches only some roots, so a per-root line not repeated this
	// pass is not forgotten at once (that would re-log it whenever a pass
	// skipped it); it ages out after the recheck window.
	w.notes.endPartialPass(now)
	if enforce && walked == 0 && listed < len(ls) && !budget.ctxDone() {
		log.Printf("wisp gc: work arm pass ended with no walk: its wall budget (%s) ran out after listing %d of %d leg(s)", wispGCWorkArmWallBudget, listed, len(ls))
	}
	if !enforce && (idle > 0 || expired > 0) {
		log.Printf("wisp gc: work arm (dry-run; set %s=1 to enforce): %d open work-class molecule root(s) idle past %s, %d closed root(s) past retention on %d of %d leg(s); subtrees and closures not inspected",
			workMoleculesEnv, idle, wispGCCloseAbandonedTTL, expired, listed, len(ls))
	}
	if enforce && (walked > 0 || purged > 0) {
		log.Printf("wisp gc: work arm walked %d root(s) across %d of %d leg(s), purged %d closure(s)", walked, listed, len(ls), purged)
	}
	return purged, errs
}

// workArmMemo names one leg's memo for an arm: memos are pruned per leg.
func workArmMemo(arm, ref string) string { return arm + "@" + ref }

// presentIDs is the id set of roots.
func presentIDs(roots []beads.Bead) map[string]bool {
	out := make(map[string]bool, len(roots))
	for _, r := range roots {
		out[r.ID] = true
	}
	return out
}

// rotateAfter returns roots ordered by id, starting after cursor and wrapping
// around, so a pass that stops early resumes where the last one stopped.
func rotateAfter(roots []beads.Bead, cursor string) []beads.Bead {
	out := append([]beads.Bead(nil), roots...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if cursor == "" {
		return out
	}
	i := sort.Search(len(out), func(i int) bool { return out[i].ID > cursor })
	return append(out[i:], out[:i]...)
}

// workArmPurgeAge is the clock a closed work-class molecule root ages on: its
// last update, which its close stamps, not its creation. A long-lived v1
// molecule closed a minute ago is not past retention because it was poured
// long ago.
func workArmPurgeAge(root beads.Bead) time.Time {
	return beadLastActivity(root)
}

// countIdleAbandonedCandidates counts, from a listing alone, the roots the
// abandoned-root closer would go on to inspect: in scope, not exempt, not
// terminal, idle past the TTL. It reads nothing.
func countIdleAbandonedCandidates(roots []beads.Bead, now time.Time) int {
	cutoff := now.Add(-wispGCCloseAbandonedTTL)
	n := 0
	for _, root := range roots {
		if isAbandonedRootCandidate(root) && !convoycore.IsTerminalStatus(root.Status) && !isGCExempt(root) && beadLastActivity(root).Before(cutoff) {
			n++
		}
	}
	return n
}

// workGCLane runs the work arm off the controller tick.
type workGCLane struct {
	wakeCh chan struct{}
	gc     *workMoleculeGC

	statusMu   sync.Mutex
	lastPassAt time.Time
}

func newWorkGCLane() *workGCLane {
	return &workGCLane{wakeCh: make(chan struct{}, 1), gc: newWorkMoleculeGC()}
}

// workGCLaneOf returns this runtime's lane, creating it on first use.
func (cr *CityRuntime) workGCLaneOf() *workGCLane {
	cr.workGCLaneOnce.Do(func() { cr.workGCLane = newWorkGCLane() })
	return cr.workGCLane
}

// wake asks the lane for a pass. Non-blocking.
func (l *workGCLane) wake() {
	select {
	case l.wakeCh <- struct{}{}:
	default:
	}
}

// lastPassAge is how long ago the last pass finished, and whether one has.
func (l *workGCLane) lastPassAge(now time.Time) (time.Duration, bool) {
	l.statusMu.Lock()
	defer l.statusMu.Unlock()
	if l.lastPassAt.IsZero() {
		return 0, false
	}
	return now.Sub(l.lastPassAt), true
}

// startWorkGCLane starts the lane goroutine and returns a channel closed when it
// exits. The tick wakes it whenever the wisp GC runs; a backstop runs a pass
// one wisp-GC interval after the last one otherwise.
func (cr *CityRuntime) startWorkGCLane(ctx context.Context) <-chan struct{} {
	lane := cr.workGCLaneOf()
	interval := 10 * time.Minute
	if cfg := cr.serviceConfigSnapshot(); cfg != nil {
		if d := cfg.Daemon.WispGCIntervalDuration(); d > 0 {
			interval = d
		}
	}
	return startPacedLane(ctx, interval, 0, lane.wakeCh, func(bool) {
		cr.safeTick(func() { cr.runWorkGCLanePass(ctx, time.Now()) }, "work-gc-lane")
	})
}

// runWorkGCLanePass is one lane pass. It reads the published config snapshot,
// not cr.cfg, because it runs on its own goroutine.
func (cr *CityRuntime) runWorkGCLanePass(ctx context.Context, now time.Time) {
	if ctx.Err() != nil || cr.beadsQuiescent.Load() {
		return
	}
	cfg := cr.serviceConfigSnapshot()
	if cfg == nil {
		return
	}
	ttl := cfg.Daemon.WispTTLDuration()
	if ttl <= 0 || cfg.Daemon.WispGCIntervalDuration() <= 0 {
		return
	}
	if _, relocated := graphClassBinding(cr.storageRoutes); !relocated { // residency:allow — asks only whether the graph class is relocated; the legs come from the census plan (workLegsFromCensus)
		return
	}
	lane := cr.workGCLaneOf()
	graph := resolveGraphStore(cr.storageRoutes, cr.cityBeadStore(), cfg, cr.cityPath, cr.rec)
	legs := cr.workLegs()
	purged, err := lane.gc.pass(ctx, legs, graph, ttl, now)
	if err != nil {
		for _, line := range strings.Split(err.Error(), "\n") {
			if line != "" {
				fmt.Fprintf(cr.stderr, "%s: wisp gc (work arm): %s\n", cr.logPrefix, line) //nolint:errcheck // best-effort stderr
			}
		}
	}
	if purged > 0 {
		fmt.Fprintf(cr.stdout, "Bead GC (work arm): purged %d expired molecule closure(s)\n", purged) //nolint:errcheck // best-effort stdout
	}
	lane.statusMu.Lock()
	lane.lastPassAt = time.Now()
	lane.statusMu.Unlock()
}
