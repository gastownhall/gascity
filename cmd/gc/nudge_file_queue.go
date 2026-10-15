package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
)

// The flock'd state.json nudge queue: <city>/.gc/nudges/state.json, guarded
// by state.lock. The file lock is not reentrant, so no function here calls
// another queue verb inside a withNudgeQueueState callback.

// pinQueuedNudgeClaims pins the in-flight nudges ids to delivery key batch,
// writing nothing unless every one of them is still in flight. It runs no
// maintenance pass and opens no bead store, so it works while the bead store
// whose failed write made the caller re-key is still failing.
func pinQueuedNudgeClaims(cityPath string, ids []string, batch string) error {
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	return withNudgeQueueState(cityPath, func(state *nudgeQueueState) error {
		pinned := 0
		for i := range state.InFlight {
			if want[state.InFlight[i].ID] {
				state.InFlight[i].DeliveryBatch = batch
				pinned++
			}
		}
		if pinned != len(want) {
			return fmt.Errorf("%d of %d claimed nudges are no longer in flight", len(want)-pinned, len(want))
		}
		return nil
	})
}

// queuedNudgeClaimableForTarget reports whether this target may claim item.
// liveSessions is a lazy census of session IDs that still hold a nudge fence;
// it is invoked only on the session-replacement path so the idle/exact-match
// claim tick does not list session beads.
//
// The two fence predicates agree this way:
//   - matching fence (same SessionID, including stale epoch) → claimable (Leg A)
//   - mismatched SessionID whose fenced session is no longer live, and whose
//     successor target is in the census → claimable (Leg B); the item is
//     re-fenced onto the successor (refencedQueuedNudge)
//   - mismatched SessionID that is still live → not claimable (sibling fence)
//   - census unavailable, empty, or missing the target → fail closed
func queuedNudgeClaimableForTarget(target nudgeTarget, item queuedNudge, liveSessions func() map[string]struct{}) bool {
	if !target.matchesQueueAgent(item.Agent) {
		return false
	}
	if item.SessionID != "" {
		if target.sessionID == "" {
			return false
		}
		if item.SessionID == target.sessionID {
			return true
		}
		if liveSessions == nil {
			return false
		}
		live := liveSessions()
		if live == nil {
			return false
		}
		if _, targetLive := live[target.sessionID]; !targetLive {
			// Census did not observe this target (empty city, store
			// unavailable, or the successor bead is not listed). Fail
			// closed rather than treating "no live sessions" as a
			// license to steal sibling fences.
			return false
		}
		_, stillLive := live[item.SessionID]
		return !stillLive
	}
	if item.ContinuationEpoch != "" && target.continuationEpoch == "" {
		return false
	}
	return true
}

// nudgeMaintenanceStore lazily opens the Dolt-backed nudge front-door store the
// first time a poll helper actually has front-door work to do — i.e. the flock'd
// state.json queue is non-empty and a recover/prune/terminalize pass may need to
// shadow a terminal-state bead. On the common idle tick the queue is empty, the
// maintenance passes are no-ops, and the store is never opened, so N idle
// `gc nudge poll` sidecars no longer each dial the sql-server (~2 connections:
// main pool + a SHOW DATABASES init probe) every 2s. See
// TestNudgePollHelpersSkipDoltOpenOnEmptyQueue.
//
// It closes exactly the handle it opened, and only if it opened one,
// preserving the closeBeadStoreHandle ownership contract the …WithStore
// variants model. "The handle it opened" is not "the store it holds": once the
// NUDGES class relocates, the store is the storage routes' process-shared
// engine, which this frame holds but must never close (openOwnedNudgeBeadStore).
type nudgeMaintenanceStore struct {
	cityPath string
	opened   bool
	store    beads.NudgesStore
	// handle is what ensureOpen actually opened, which is not always
	// store.Store: see openOwnedNudgeBeadStore. close releases this and
	// nothing else.
	handle beads.Store
	front  *nudgequeue.Store
}

// frontForState returns the front-door handle to use for the maintenance passes
// over state, opening the underlying store on first need. When the queue has no
// Pending/InFlight/Dead items there is nothing for recover/prune/terminalize to
// do, so the store is left closed and the returned front is nil — every
// maintenance pass only dereferences front while iterating a non-empty slice, so
// a nil front is never touched on an empty queue.
func (m *nudgeMaintenanceStore) frontForState(state *nudgeQueueState) *nudgequeue.Store {
	if nudgeQueueHasWork(state) {
		m.ensureOpen()
	}
	return m.front
}

// ensureOpen opens the underlying store exactly once (idempotent) and returns
// it. ack uses it directly to stamp terminal beads once it has confirmed
// terminal items to terminalize.
func (m *nudgeMaintenanceStore) ensureOpen() beads.NudgesStore {
	if !m.opened {
		m.opened = true
		m.store, m.handle = openOwnedNudgeBeadStore(m.cityPath)
		if m.store.Store != nil {
			m.front = nudgeFrontDoor(m.store)
		}
	}
	return m.store
}

// close releases the handle this frame opened (if any). It never touches a
// caller-passed store because this type only ever opens its own, and never the
// storage routes' shared engine, which it may hold but does not own.
func (m *nudgeMaintenanceStore) close() error {
	if !m.opened {
		return nil
	}
	return closeBeadStoreHandle(m.handle)
}

// nudgeQueueHasWork reports whether the queue holds any item a maintenance pass
// could act on. An empty queue means recover/prune/terminalize are all no-ops,
// so the Dolt front door need not be opened for this tick.
func nudgeQueueHasWork(state *nudgeQueueState) bool {
	return len(state.Pending) > 0 || len(state.InFlight) > 0 || len(state.Dead) > 0
}

func claimDueQueuedNudgesMatching(cityPath string, now time.Time, match func(queuedNudge) bool) ([]queuedNudge, error) {
	return claimDueQueuedNudges(cityPath, now, match, nil)
}

// claimDueQueuedNudges claims every due nudge match accepts and pins nothing
// (claimDueQueuedNudgesMatching), or, given a batchTarget, that target's next
// delivery batch (claimDueQueuedNudgeBatchForTarget).
func claimDueQueuedNudges(cityPath string, now time.Time, match func(queuedNudge) bool, batchTarget *nudgeTarget) ([]queuedNudge, error) {
	maint := nudgeMaintenanceStore{cityPath: cityPath}
	defer maint.close() //nolint:errcheck // best-effort
	var claimed []queuedNudge
	err := withNudgeQueueState(cityPath, func(state *nudgeQueueState) error {
		front := maint.frontForState(state)
		deadline := now.Add(nudgeEnqueueMaintenanceBudget)
		if err := recoverExpiredInFlightNudges(state, front, now, deadline); err != nil {
			return err
		}
		if err := pruneExpiredQueuedNudges(state, front, now, deadline); err != nil {
			return err
		}
		if err := pruneDeadQueuedNudges(state, front, now, deadline); err != nil {
			return err
		}
		due := func(item queuedNudge) bool {
			return item.DeliverAfter.IsZero() || !item.DeliverAfter.After(now)
		}
		// due before match: match may list the city's sessions (the Leg-B
		// census), which a candidate that is not yet due never needs.
		claim := func(item queuedNudge) bool { return due(item) && match(item) }
		stamp := func(item queuedNudge) queuedNudge { return item }
		if batchTarget != nil {
			pinned := wholePinnedBatch(state, claim, match)
			batch := pinned
			if pinned != "" {
				claim = func(item queuedNudge) bool { return item.DeliveryBatch == pinned && match(item) }
			} else {
				batch = newQueuedNudgeBatchID()
				claim = func(item queuedNudge) bool { return item.DeliveryBatch == "" && due(item) && match(item) }
			}
			stamp = func(item queuedNudge) queuedNudge {
				item = refencedQueuedNudge(*batchTarget, item)
				item.DeliveryBatch = batch
				return item
			}
		}
		pending := state.Pending[:0]
		for _, item := range state.Pending {
			if !claim(item) {
				pending = append(pending, item)
				continue
			}
			item = stamp(item)
			item.ClaimedAt = now.UTC()
			item.LeaseUntil = now.Add(defaultQueuedNudgeClaimTTL).UTC()
			state.InFlight = append(state.InFlight, item)
			claimed = append(claimed, item)
		}
		state.Pending = pending
		sortQueuedNudges(state)
		return nil
	})
	return claimed, err
}

// wholePinnedBatch returns the first pinned delivery batch that has a due,
// claimable item and can be claimed whole: every pending item carrying its pin
// matches, and none of its items is in flight. A batch is only ever delivered
// whole under its key; a strict subset would let the runtime commit the
// partial text and later replay that receipt for the missing items, acking
// them unseen. A batch that cannot be claimed whole waits this pass, to be
// reassembled on a later pass or retired when its items expire.
//
// Whether a batch splits is decided only for a batch that has a due,
// claimable item, because match may list the city's sessions (the Leg-B
// census): a batch whose items are all backed off never asks it. Deciding it
// does ask about the batch's items that are not yet due: the batch is claimed
// whole whatever their deliver_after, so whether each passes the fence decides
// the claim.
func wholePinnedBatch(state *nudgeQueueState, claimable, match func(queuedNudge) bool) string {
	inFlight := make(map[string]bool, len(state.InFlight))
	for _, item := range state.InFlight {
		if item.DeliveryBatch != "" {
			inFlight[item.DeliveryBatch] = true
		}
	}
	split := map[string]bool{}
	splits := func(batch string) bool {
		if decided, ok := split[batch]; ok {
			return decided
		}
		split[batch] = false
		for _, item := range state.Pending {
			if item.DeliveryBatch == batch && !match(item) {
				split[batch] = true
				break
			}
		}
		return split[batch]
	}
	for _, item := range state.Pending {
		batch := item.DeliveryBatch
		if batch == "" || inFlight[batch] || !claimable(item) || splits(batch) {
			continue
		}
		return batch
	}
	return ""
}

func listQueuedNudges(cityPath, agentName string, now time.Time) ([]queuedNudge, []queuedNudge, []queuedNudge, error) {
	maint := nudgeMaintenanceStore{cityPath: cityPath}
	defer maint.close() //nolint:errcheck // best-effort
	var pending []queuedNudge
	var inFlight []queuedNudge
	var dead []queuedNudge
	err := withNudgeQueueState(cityPath, func(state *nudgeQueueState) error {
		front := maint.frontForState(state)
		deadline := now.Add(nudgeEnqueueMaintenanceBudget)
		if err := recoverExpiredInFlightNudges(state, front, now, deadline); err != nil {
			return err
		}
		if err := pruneExpiredQueuedNudges(state, front, now, deadline); err != nil {
			return err
		}
		if err := pruneDeadQueuedNudges(state, front, now, deadline); err != nil {
			return err
		}
		for _, item := range state.Pending {
			if item.Agent == agentName {
				pending = append(pending, item)
			}
		}
		for _, item := range state.InFlight {
			if item.Agent == agentName {
				inFlight = append(inFlight, item)
			}
		}
		for _, item := range state.Dead {
			if item.Agent == agentName {
				dead = append(dead, item)
			}
		}
		return nil
	})
	return pending, inFlight, dead, err
}

// listQueuedNudgesForTargetWithClock is the clock-injectable core of
// listQueuedNudgesForTarget (mirrors enqueueQueuedNudgeWithStoreAndClock's
// seam). Tests can pass a clock.Fake so the maintenance pass's
// now.Add(nudgeEnqueueMaintenanceBudget) deadline check is evaluated against
// a controlled clock instead of real wall-clock time, which otherwise races
// against this pass's own file I/O under -race.
func listQueuedNudgesForTargetWithClock(cityPath string, target nudgeTarget, now time.Time, clk clock.Clock) ([]queuedNudge, []queuedNudge, []queuedNudge, error) {
	maint := nudgeMaintenanceStore{cityPath: cityPath}
	defer maint.close() //nolint:errcheck // best-effort
	var pending []queuedNudge
	var inFlight []queuedNudge
	var dead []queuedNudge
	err := withNudgeQueueState(cityPath, func(state *nudgeQueueState) error {
		front := maint.frontForState(state)
		deadline := now.Add(nudgeEnqueueMaintenanceBudget)
		if err := recoverExpiredInFlightNudgesWithClock(state, front, now, deadline, clk); err != nil {
			return err
		}
		if err := pruneExpiredQueuedNudgesWithClock(state, front, now, deadline, clk); err != nil {
			return err
		}
		if err := pruneDeadQueuedNudgesWithClock(state, front, now, deadline, clk); err != nil {
			return err
		}
		for _, item := range state.Pending {
			if target.matchesQueueAgent(item.Agent) {
				pending = append(pending, item)
			}
		}
		for _, item := range state.InFlight {
			if target.matchesQueueAgent(item.Agent) {
				inFlight = append(inFlight, item)
			}
		}
		for _, item := range state.Dead {
			if target.matchesQueueAgent(item.Agent) {
				dead = append(dead, item)
			}
		}
		return nil
	})
	return pending, inFlight, dead, err
}

func takeQueuedNudgesByID(items []queuedNudge, id string, removed []queuedNudge) ([]queuedNudge, []queuedNudge) {
	if len(items) == 0 {
		return items, removed
	}
	filtered := make([]queuedNudge, 0, len(items))
	for _, item := range items {
		if item.ID == id {
			removed = append(removed, item)
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered, removed
}

func enqueueQueuedNudgeWithStoreAndClock(cityPath string, store beads.NudgesStore, item queuedNudge, clk clock.Clock) error {
	ownStore := false
	var opened beads.Store
	if store.Store == nil {
		store, opened = openOwnedNudgeBeadStore(cityPath)
		ownStore = true
	}
	if ownStore {
		defer closeBeadStoreHandle(opened) //nolint:errcheck // best-effort
	}
	var front *nudgequeue.Store
	if store.Store != nil {
		front = nudgeFrontDoor(store)
	}
	beadID, created, err := ensureQueuedNudgeBead(store, item)
	if err != nil {
		return err
	}
	if beadID != "" {
		item.BeadID = beadID
	}
	err = withNudgeQueueState(cityPath, func(state *nudgeQueueState) error {
		now := clk.Now()
		deadline := now.Add(nudgeEnqueueMaintenanceBudget)
		if err := recoverExpiredInFlightNudgesWithClock(state, front, now, deadline, clk); err != nil {
			return err
		}
		if err := pruneExpiredQueuedNudgesWithClock(state, front, now, deadline, clk); err != nil {
			return err
		}
		if err := pruneDeadQueuedNudgesWithClock(state, front, now, deadline, clk); err != nil {
			return err
		}
		if queuedNudgeExists(state, item.ID) {
			return nil
		}
		// Supersede pending and in-flight nudges for the same (agent, source, reference).
		if item.Reference != nil && item.Reference.ID != "" {
			matchesSupersession := func(existing queuedNudge) bool {
				return existing.Agent == item.Agent && existing.Source == item.Source &&
					existing.Reference != nil && existing.Reference.Kind == item.Reference.Kind &&
					existing.Reference.ID == item.Reference.ID
			}
			filtered := state.Pending[:0]
			for i, existing := range state.Pending {
				if clk.Now().After(deadline) {
					filtered = append(filtered, state.Pending[i:]...)
					break
				}
				if matchesSupersession(existing) {
					existing.DeadAt = now.UTC()
					existing.LastError = "superseded"
					state.Dead = append(state.Dead, existing)
					if err := markQueuedNudgeTerminal(store, existing, "superseded", "superseded", "", now); err != nil {
						return err
					}
					continue
				}
				filtered = append(filtered, existing)
			}
			state.Pending = filtered
			// Also supersede in-flight nudges. Note: an active delivery may
			// already be running for a superseded item. When it completes, its
			// ack/failure won't find the item in InFlight and will no-op.
			// This causes at most one redundant delivery, not data corruption.
			inFlight := state.InFlight[:0]
			for i, existing := range state.InFlight {
				if clk.Now().After(deadline) {
					inFlight = append(inFlight, state.InFlight[i:]...)
					break
				}
				if matchesSupersession(existing) {
					existing.DeadAt = now.UTC()
					existing.LastError = "superseded"
					state.Dead = append(state.Dead, existing)
					if err := markQueuedNudgeTerminal(store, existing, "superseded", "superseded", "", now); err != nil {
						return err
					}
					continue
				}
				inFlight = append(inFlight, existing)
			}
			state.InFlight = inFlight
		}
		state.Pending = append(state.Pending, item)
		sortQueuedNudges(state)
		return nil
	})
	if err != nil && created && store.Store != nil && beadID != "" {
		// Roll back the leaked shadow bead through the nudge front door, which
		// stamps the canonical close_reason before Close so BdStore.Close can
		// forward it as `bd close --reason` and satisfy validation.on-close=error.
		// Preserve the original enqueue error, but return rollback failures too
		// so leaked open nudge beads are diagnosable.
		if rbErr := nudgeFrontDoor(store).RollbackEnqueue(beadID); rbErr != nil {
			err = errors.Join(err, fmt.Errorf("rollback nudge bead %q: %w", beadID, rbErr))
		}
	}
	if err == nil {
		// Best-effort wake of the supervisor's nudge dispatcher. Legacy-mode
		// cities and ad-hoc invocations (no listener) get a fast dial
		// failure and fall through to the per-session poller / patrol tick.
		pingNudgeWakeSocket(cityPath)
	}
	return err
}

func failedQueuedNudge(item queuedNudge, cause error, now time.Time) (queuedNudge, bool) {
	item.Attempts++
	item.LastAttemptAt = now.UTC()
	item.LastError = cause.Error()
	item.ClaimedAt = time.Time{}
	item.LeaseUntil = time.Time{}
	if errors.Is(cause, errNudgeSessionFenceMismatch) {
		item.DeadAt = now.UTC()
		return item, true
	}
	if errors.Is(cause, runtime.ErrNudgeRejected) {
		// The runtime refused the batch, so nothing was delivered: unpin it,
		// so the next claim delivers it as a new batch under a new key.
		item.DeliveryBatch = ""
	}
	if item.Attempts >= defaultQueuedNudgeMaxAttempts || (!item.ExpiresAt.IsZero() && !item.ExpiresAt.After(now)) {
		item.DeadAt = now.UTC()
		return item, true
	}
	item.DeliverAfter = now.Add(defaultQueuedNudgeRetryDelay).UTC()
	return item, false
}

func terminalStateForDeadQueuedNudge(item queuedNudge) string {
	switch strings.TrimSpace(item.LastError) {
	case "expired":
		return "expired"
	case "superseded":
		return "superseded"
	default:
		return "failed"
	}
}

// noMaintenanceDeadline returns a deadline far enough in the future that a
// nudge-queue maintenance pass never stops early. Used by tests that
// exercise the recover/prune primitives' own full-drain behavior directly.
// Every production call site is bounded instead -- nudgeEnqueueMaintenanceBudget
// for per-call maintenance (claim, list, ack, release, record-failure),
// nudgeMaintenanceSweepBudget for the supervisor's whole-queue sweep -- per
// ga-2kzci3: an unconditional full-backlog drain on every call does
// unbounded O(backlog) work under the queue's exclusive flock, and the
// recover/prune passes already bail out cleanly on deadline, leaving the
// remainder for the next bounded pass to pick up.
func noMaintenanceDeadline() time.Time {
	return time.Now().Add(24 * time.Hour)
}

// recordNudgeDispatchSkips merges a dispatch tick's per-reason skip counts
// into the queue state's running totals. Called at most once per tick, after
// the per-session loop finishes, so it never nests inside the loop's own
// withNudgeQueueState-backed calls (claimDueQueuedNudgesForTarget et al) —
// the queue lock is a per-process flock and is not reentrant.
func recordNudgeDispatchSkips(cityPath string, counts map[string]int64) error {
	if len(counts) == 0 {
		return nil
	}
	return withNudgeQueueState(cityPath, func(state *nudgeQueueState) error {
		if state.DispatchSkips == nil {
			state.DispatchSkips = make(map[string]int64, len(counts))
		}
		for reason, n := range counts {
			state.DispatchSkips[reason] += n
		}
		return nil
	})
}

// nudgeMaintenanceSweepBudget bounds how long a single
// runNudgeQueueMaintenanceSweep pass is allowed to consider itself current
// for, relative to the caller's own now (ga-2kzci3 FR3). Unlike
// noMaintenanceDeadline (real-wall-clock + 24h, used by the latency-
// insensitive callers that must always drain the full backlog), this
// deadline is derived from the passed-in now so a stale now -- one whose
// budget has already elapsed relative to the real clock -- causes the
// per-item bail check in recoverExpiredInFlightNudgesWithClock /
// pruneExpiredQueuedNudgesWithClock / pruneDeadQueuedNudgesWithClock to stop
// before mutating anything, rather than silently draining the whole backlog
// against a wall-clock deadline that ignores how stale now actually is.
// Comfortably above a realistic single-pass sweep duration and comfortably
// below the staleness a caller is expected to catch.
const nudgeMaintenanceSweepBudget = 5 * time.Minute

// nudgeMaintenanceDebounceWindow bounds how close together two
// runNudgeQueueMaintenanceSweep calls for the same city can be, in terms of
// their own now, before the second is treated as a redundant same-tick
// sweep and skipped (ga-2kzci3 FR4). The sweep has one production caller,
// dispatchAllQueuedNudges (nudge_dispatcher.go), which runs it once per
// dispatch pass, before the per-session loop -- so the bursts come from how
// often a pass fires, not from the session count. nudgeDispatchTick fires
// on every wake-socket signal plus once at the end of each patrol tick, so
// a busy city can invoke this many times in quick succession for the same
// queue.
const nudgeMaintenanceDebounceWindow = time.Second

var (
	nudgeMaintenanceDebounceMu  sync.Mutex
	nudgeMaintenanceLastSweepAt = map[string]time.Time{}
)

// nudgeMaintenanceSweepDebounced reports whether a sweep for cityPath at now
// falls within nudgeMaintenanceDebounceWindow of the now of the last sweep
// that actually ran for that city, and if not, records now as that marker.
// A debounced call leaves the marker untouched.
func nudgeMaintenanceSweepDebounced(cityPath string, now time.Time) bool {
	nudgeMaintenanceDebounceMu.Lock()
	defer nudgeMaintenanceDebounceMu.Unlock()
	if last, ok := nudgeMaintenanceLastSweepAt[cityPath]; ok {
		elapsed := now.Sub(last)
		if elapsed < 0 {
			elapsed = -elapsed
		}
		if elapsed < nudgeMaintenanceDebounceWindow {
			return true
		}
	}
	nudgeMaintenanceLastSweepAt[cityPath] = now
	return false
}

// runNudgeQueueMaintenanceSweep runs the queue's recover/TTL-expiry/dead-
// letter-retention passes over the whole queue, independent of whether any
// pending item currently matches an open session. Every other maintenance
// call site (claimDueQueuedNudgesMatching, listQueuedNudges,
// listQueuedNudgesForTarget, ...) only runs these passes as a side effect of
// an operation scoped to a specific agent/target, so an item whose target
// never matches never gets swept by any of them. The supervisor dispatch
// tick is the one path that iterates the whole queue every cycle regardless
// of match outcome, so it owns running this sweep unconditionally.
func runNudgeQueueMaintenanceSweep(cityPath string, now time.Time) error {
	if nudgeMaintenanceSweepDebounced(cityPath, now) {
		return nil
	}
	maint := nudgeMaintenanceStore{cityPath: cityPath}
	defer maint.close() //nolint:errcheck // best-effort
	return withNudgeQueueState(cityPath, func(state *nudgeQueueState) error {
		front := maint.frontForState(state)
		deadline := now.Add(nudgeMaintenanceSweepBudget)
		if err := recoverExpiredInFlightNudges(state, front, now, deadline); err != nil {
			return err
		}
		if err := pruneExpiredQueuedNudges(state, front, now, deadline); err != nil {
			return err
		}
		return pruneDeadQueuedNudges(state, front, now, deadline)
	})
}

func pruneExpiredQueuedNudges(state *nudgeQueueState, front *nudgequeue.Store, now, deadline time.Time) error {
	return pruneExpiredQueuedNudgesWithClock(state, front, now, deadline, clock.Real{})
}

func pruneExpiredQueuedNudgesWithClock(state *nudgeQueueState, front *nudgequeue.Store, now, deadline time.Time, clk clock.Clock) error {
	filtered := state.Pending[:0]
	for i, item := range state.Pending {
		if clk.Now().After(deadline) {
			filtered = append(filtered, state.Pending[i:]...)
			break
		}
		if !item.ExpiresAt.IsZero() && !item.ExpiresAt.After(now) {
			item.DeadAt = now.UTC()
			if item.LastError == "" {
				item.LastError = "expired"
			}
			state.Dead = append(state.Dead, item)
			// Best-effort: remove expired item from pending even if bead update fails.
			// A failed bead update here would trap the item in pending forever.
			_ = front.Terminalize(item, "expired", item.LastError, "", now)
			continue
		}
		filtered = append(filtered, item)
	}
	state.Pending = filtered
	sortQueuedNudges(state)
	return nil
}

func recoverExpiredInFlightNudges(state *nudgeQueueState, front *nudgequeue.Store, now, deadline time.Time) error {
	return recoverExpiredInFlightNudgesWithClock(state, front, now, deadline, clock.Real{})
}

func recoverExpiredInFlightNudgesWithClock(state *nudgeQueueState, front *nudgequeue.Store, now, deadline time.Time, clk clock.Clock) error {
	filtered := state.InFlight[:0]
	for i, item := range state.InFlight {
		if clk.Now().After(deadline) {
			filtered = append(filtered, state.InFlight[i:]...)
			break
		}
		if !item.ExpiresAt.IsZero() && !item.ExpiresAt.After(now) {
			item.DeadAt = now.UTC()
			if item.LastError == "" {
				item.LastError = "expired"
			}
			state.Dead = append(state.Dead, item)
			// Best-effort: remove expired item from in-flight even if bead update fails.
			_ = front.Terminalize(item, "expired", item.LastError, "", now)
			continue
		}
		if item.LeaseUntil.IsZero() || !item.LeaseUntil.After(now) {
			item.ClaimedAt = time.Time{}
			item.LeaseUntil = time.Time{}
			item.DeliverAfter = now.UTC()
			state.Pending = append(state.Pending, item)
			continue
		}
		filtered = append(filtered, item)
	}
	state.InFlight = filtered
	sortQueuedNudges(state)
	return nil
}

// pruneDeadQueuedNudges removes dead-letter items older than defaultQueuedNudgeDeadRetention.
// A shadow-backed item (non-empty BeadID) is pruned only once its terminal bead record is
// confirmed in the store; shadow-backed items without a confirmed terminal bead are retained
// so terminal history is not lost if the bead store write failed. A shadowless item has no
// record to confirm and is pruned on DeadAt alone. Items with a zero DeadAt are retained.
func pruneDeadQueuedNudges(state *nudgeQueueState, front *nudgequeue.Store, now, deadline time.Time) error {
	return pruneDeadQueuedNudgesWithClock(state, front, now, deadline, clock.Real{})
}

func pruneDeadQueuedNudgesWithClock(state *nudgeQueueState, front *nudgequeue.Store, now, deadline time.Time, clk clock.Clock) error {
	cutoff := now.Add(-defaultQueuedNudgeDeadRetention)
	filtered := state.Dead[:0]
	for i, item := range state.Dead {
		if clk.Now().After(deadline) {
			filtered = append(filtered, state.Dead[i:]...)
			break
		}
		if item.BeadID != "" {
			if front == nil {
				// No store available — retain the item to avoid data loss.
				filtered = append(filtered, item)
				continue
			}
			shadow, ok, err := front.FindIncludingTerminal(item.ID)
			if err != nil {
				// Fail open: store lookup errors retain the item rather than
				// blocking the entire queue operation. Pruning is best-effort.
				filtered = append(filtered, item)
				continue
			}
			if !ok || !nudgequeue.IsTerminalState(shadow.State) {
				// Repair historical dead-letter entries whose queue state was
				// durable but whose backing bead never received terminal state.
				reason := strings.TrimSpace(item.LastError)
				if reason == "" {
					reason = "failed"
				}
				terminalAt := now
				if !item.DeadAt.IsZero() {
					terminalAt = item.DeadAt
				}
				if err := front.Terminalize(item, terminalStateForDeadQueuedNudge(item), reason, "", terminalAt); err != nil {
					filtered = append(filtered, item)
					continue
				}
				// Terminalize's own error return is sufficient confirmation:
				// nil means either the bead was really terminalized, or it
				// tolerated a missing bead as a no-op because the bead was
				// already reaped by an unrelated retention sweep (wisp
				// compaction etc.). Re-querying FindIncludingTerminal here
				// would never find a reaped bead again, so entries whose
				// bead is gone would be retained forever and pay a store
				// lookup on every sweep (gastownhall/gascity#5278).
			}
			if !item.DeadAt.IsZero() && item.DeadAt.Before(cutoff) {
				// Terminal (or the backing bead is gone entirely) — safe to
				// prune once past retention.
				continue
			}
		} else if !item.DeadAt.IsZero() && item.DeadAt.Before(cutoff) {
			// No shadow to confirm: the queue entry is the only record, so
			// it ages out on DeadAt alone.
			continue
		}
		filtered = append(filtered, item)
	}
	state.Dead = filtered
	return nil
}

func queuedNudgeExists(state *nudgeQueueState, id string) bool {
	for _, item := range state.Pending {
		if item.ID == id {
			return true
		}
	}
	for _, item := range state.InFlight {
		if item.ID == id {
			return true
		}
	}
	for _, item := range state.Dead {
		if item.ID == id {
			return true
		}
	}
	return false
}

func sortQueuedNudges(state *nudgeQueueState) {
	nudgequeue.SortState(state)
}

func withNudgeQueueState(cityPath string, fn func(*nudgeQueueState) error) error {
	return nudgequeue.WithState(cityPath, fn)
}
