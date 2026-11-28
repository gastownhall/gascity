package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/session"
)

// nudgeRetiredSweepRowReadBudget caps how many session rows one retired-fence
// sweep pass reads (#4534): the pass runs off the controller's watchdog and
// the maintaining list path, so its reads must stay bounded by this constant
// rather than the queue's size. Fences past the budget are judged by the next
// pass; the queue's TTL remains the backstop.
const nudgeRetiredSweepRowReadBudget = 200

// sweepRetiredSessionNudges dead-letters the pending nudges fenced to retired
// sessions — rows closed for good that no session will ever run again
// (#4534). A pending item whose fence is retired can only wait out its TTL,
// so the sweep moves it to Dead with the session-terminated reason and owes
// its gc:nudge shadow the failed close.
//
// The pass is safe to run from any observer — the controller's watchdog, a
// maintaining list — and converges idempotently: it judges the queue's
// snapshot, plans only what is provably orphaned, and re-checks each item's
// fence under the queue lock before moving it, so an item claimed, delivered
// or re-fenced between the snapshot and the locked run is left alone.
// Errors never dead-letter anything: a census failure aborts the pass, and
// a row that cannot be read (including not-found — the session may exist in
// a store this reader cannot see) is left to the TTL and named in the
// returned error. Row reads are capped per pass by
// nudgeRetiredSweepRowReadBudget. Both handles must be the city's routed
// class stores (storeref / cliStores), so a relocated sessions class
// answers the reads; a zero session handle makes the pass a no-op.
func sweepRetiredSessionNudges(cityPath string, cfg *config.City, nudges beads.NudgesStore, sessStore beads.SessionStore, now time.Time) (int, error) {
	if cfg == nil || strings.TrimSpace(cityPath) == "" || sessStore == (beads.SessionStore{}) {
		return 0, nil
	}
	// (a) Snapshot, lock-free, through the queue seam. InFlight items are
	// never candidates: Maintain returns lapsed leases to Pending, and the
	// next pass judges them.
	state, err := openNudgeQueue(cityPath, beads.NudgesStore{}).Queue.Snapshot()
	if err != nil {
		return 0, fmt.Errorf("loading nudge queue state: %w", err)
	}
	candidates := pendingFencedNudges(state)
	if len(candidates) == 0 {
		return 0, nil
	}
	front := session.NewStore(sessStore)
	// (b) Census of open sessions: an open row owns its fence, whatever its
	// state, and an open target's queue keys may still answer another
	// item's key.
	openInfos, err := front.ListAll(session.ListAllOptions{})
	if err != nil {
		return 0, fmt.Errorf("reading the open-session census: %w", err)
	}
	openIDs := make(map[string]struct{}, len(openInfos))
	openTargets := make([]nudgeTarget, 0, len(openInfos))
	for _, info := range openInfos {
		openIDs[info.ID] = struct{}{}
		openTargets = append(openTargets, resolveNudgeTargetFromSessionInfo(cityPath, cfg, info))
	}
	// (c) Row reads for the fences the census did not list, capped by the
	// per-pass budget. An unreadable row is never retired: it is named in
	// the returned error and left to the TTL.
	retiredFence := make(map[string]bool, len(candidates))
	reads := 0
	var readErrs []error
	judged := make(map[string]struct{}, len(candidates))
	for _, item := range candidates {
		if _, done := judged[item.SessionID]; done {
			continue
		}
		judged[item.SessionID] = struct{}{}
		if _, live := openIDs[item.SessionID]; live {
			continue
		}
		if reads >= nudgeRetiredSweepRowReadBudget {
			break
		}
		reads++
		_, persisted, err := front.GetPersistedResponse(item.SessionID)
		if err != nil {
			readErrs = append(readErrs, fmt.Errorf("reading session %q: %w", item.SessionID, err))
			continue
		}
		retiredFence[item.SessionID] = nudgeFenceRetired(cfg, persisted)
	}
	// (d) Plan: a retired fence alone is not enough — some seat must still
	// serve the item's key before it is an orphan.
	planned := make(map[string]queuedNudge, len(candidates))
	for _, item := range candidates {
		if !retiredFence[item.SessionID] {
			continue
		}
		if findNudgeKeySeat(cfg, openTargets, item.Agent) {
			continue
		}
		planned[item.ID] = item
	}
	if len(planned) == 0 {
		return 0, errors.Join(readErrs...)
	}
	// (e) Locked run, on the fence the snapshot planned from, through the
	// queue seam — the same dead-letter pattern the other terminal paths
	// run, with the shadow debts paid after the lock.
	retiredSweepBeforeLock()
	moved, err := openNudgeQueue(cityPath, nudges).RetiredFence.DeadLetterRetiredFence(planned, now)
	if err != nil {
		return 0, errors.Join(append(readErrs, fmt.Errorf("moving retired-fence nudges to dead: %w", err))...)
	}
	return moved, errors.Join(readErrs...)
}

// retiredSweepBeforeLock runs between the retired-session sweep's unlocked
// planning and its locked run. Tests replace it to change the queue in that
// window.
var retiredSweepBeforeLock = func() {}

// sweepRetiredSessionNudgesFromList is the maintaining list's best-effort
// pass of the retired-fence sweep: the same convergence the controller's
// watchdog drives every five minutes, run when a reader lists a target's
// queue, so a status read after a session closed reports the dead letter
// without waiting for the tick. It needs the target's config — a claim
// target with none does nothing — and it stops before opening a store when
// the queue holds no fenced pending item. Errors are warned, never fatal to
// the list: the TTL remains the backstop.
func sweepRetiredSessionNudgesFromList(cityPath string, cfg *config.City, now time.Time) {
	if cfg == nil || strings.TrimSpace(cityPath) == "" {
		return
	}
	state, err := openNudgeQueue(cityPath, beads.NudgesStore{}).Queue.Snapshot()
	if err != nil {
		warnRetiredSessionSweep(cityPath, fmt.Errorf("loading nudge queue state: %w", err))
		return
	}
	if len(pendingFencedNudges(state)) == 0 {
		return
	}
	work, err := openNudgeWorkStore(cityPath, cityPath)
	if err != nil {
		warnRetiredSessionSweep(cityPath, fmt.Errorf("opening the city store at %q: %w", cityPath, err))
		return
	}
	defer closeBeadStoreHandle(work) //nolint:errcheck // best-effort close of the handle this frame opened
	stores := cliStores(cityPath, work)
	if _, err := sweepRetiredSessionNudges(cityPath, cfg, stores.Nudges(), stores.Sessions(), now); err != nil {
		warnRetiredSessionSweep(cityPath, err)
	}
}

// warnRetiredSessionSweep reports a retired-fence sweep failure the list
// path survived, on the nudge warning writer (stderr in production).
func warnRetiredSessionSweep(cityPath string, err error) {
	if nudgeWarningWriter != nil {
		fmt.Fprintf(nudgeWarningWriter, "gc nudge: warning: retired-session sweep for %s: %v\n", cityPath, err) //nolint:errcheck // best-effort warning
	}
}

// pendingFencedNudges returns the snapshot's pending items fenced to a
// session id: the retired-fence sweep's candidates.
func pendingFencedNudges(state nudgequeue.State) []nudgequeue.Item {
	var candidates []nudgequeue.Item
	for _, item := range state.Pending {
		if item.SessionID != "" {
			candidates = append(candidates, item)
		}
	}
	return candidates
}

// findNudgeKeySeat reports whether a seat still serves queue key agent: a
// configured named session, a configured agent template (whose key a
// successor session adopts through Leg B), or an open session whose queue
// keys answer it. An item with a seat is not an orphan, however retired its
// old fence is.
func findNudgeKeySeat(cfg *config.City, openTargets []nudgeTarget, agent string) bool {
	agent = strings.TrimSpace(agent)
	if agent == "" {
		return false
	}
	if config.FindNamedSession(cfg, agent) != nil || config.FindAgent(cfg, agent) != nil {
		return true
	}
	for _, target := range openTargets {
		if target.matchesQueueAgent(agent) {
			return true
		}
	}
	return false
}
