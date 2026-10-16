package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// The flock'd state.json nudge queue: <city>/.gc/nudges/state.json, guarded
// by state.lock. Callers elsewhere in cmd/gc reach it through openNudgeQueue
// (nudge_queue_seam.go), usually by way of the facades in cmd_nudge.go. The
// file lock is not reentrant, so no function here calls another queue verb
// inside a withNudgeQueueState callback.

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

// fileLookupQueuedNudges reports the bucket each of the given IDs is in; an ID
// in no bucket is absent. It first runs a full maintenance pass at now, so a
// since-expired or since-recovered item is reported from current state rather
// than a stale snapshot.
func fileLookupQueuedNudges(cityPath string, ids []string, now time.Time) (map[string]nudgequeue.Bucket, error) {
	maint := nudgeMaintenanceStore{cityPath: cityPath}
	defer maint.close() //nolint:errcheck // best-effort
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	buckets := make(map[string]nudgequeue.Bucket, len(ids))
	err := withNudgeQueueState(cityPath, func(state *nudgeQueueState) error {
		front := maint.frontForState(state)
		deadline := noMaintenanceDeadline()
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
			if want[item.ID] {
				buckets[item.ID] = nudgequeue.BucketPending
			}
		}
		for _, item := range state.InFlight {
			if want[item.ID] {
				buckets[item.ID] = nudgequeue.BucketInFlight
			}
		}
		for _, item := range state.Dead {
			if want[item.ID] {
				buckets[item.ID] = nudgequeue.BucketDead
			}
		}
		return nil
	})
	return buckets, err
}

// fileRollbackQueuedNudge dead-letters item after the action that queued it
// failed, and closes its shadow as failed with reason. An item no longer in the
// queue still has its shadow closed, found by its id.
func fileRollbackQueuedNudge(cityPath string, front *nudgequeue.Store, item queuedNudge, reason string) error {
	if cityPath == "" || item.ID == "" {
		return nil
	}
	now := time.Now()
	found := false
	err := withNudgeQueueState(cityPath, func(state *nudgeQueueState) error {
		removed := []queuedNudge(nil)
		state.Pending, removed = takeQueuedNudgesByID(state.Pending, item.ID, removed)
		state.InFlight, removed = takeQueuedNudgesByID(state.InFlight, item.ID, removed)
		for _, queued := range removed {
			found = true
			queued.LastError = reason
			queued.DeadAt = now.UTC()
			state.Dead = append(state.Dead, queued)
			if err := front.Terminalize(queued, "failed", reason, "", now); err != nil {
				return err
			}
		}
		sortQueuedNudges(state)
		return nil
	})
	if err != nil {
		return err
	}
	if found {
		return nil
	}
	item.LastError = reason
	return front.Terminalize(item, "failed", reason, "", now)
}

// fileAckQueuedNudges retires the pending and in-flight ids, closing each
// shadow with outcome inside the queue lock; a shadow write that fails aborts
// the queue write.
func fileAckQueuedNudges(cityPath string, ids []string, outcome, reason, commitBoundary string) error {
	if len(ids) == 0 {
		return nil
	}
	maint := nudgeMaintenanceStore{cityPath: cityPath}
	defer maint.close() //nolint:errcheck // best-effort
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	return withNudgeQueueState(cityPath, func(state *nudgeQueueState) error {
		now := time.Now()
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
		var terminal []queuedNudge
		filtered := state.Pending[:0]
		for _, item := range state.Pending {
			if want[item.ID] {
				terminal = append(terminal, item)
				continue
			}
			filtered = append(filtered, item)
		}
		state.Pending = filtered
		inFlight := state.InFlight[:0]
		for _, item := range state.InFlight {
			if want[item.ID] {
				terminal = append(terminal, item)
				continue
			}
			inFlight = append(inFlight, item)
		}
		state.InFlight = inFlight
		for _, item := range terminal {
			// terminal items come from a non-empty Pending/InFlight, so the
			// store is already open; ensureOpen is idempotent and just returns
			// the cached handle here.
			if err := markQueuedNudgeTerminal(maint.ensureOpen(), item, outcome, reason, commitBoundary, now); err != nil {
				return err
			}
		}
		return nil
	})
}

// fileReleaseQueuedNudgeClaims returns claimed nudges to pending, keeping
// their delivery batch. A non-zero deliverAfter keeps them from being claimed
// again before then.
func fileReleaseQueuedNudgeClaims(cityPath string, ids []string, deliverAfter time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	maint := nudgeMaintenanceStore{cityPath: cityPath}
	defer maint.close() //nolint:errcheck // best-effort
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	return withNudgeQueueState(cityPath, func(state *nudgeQueueState) error {
		now := time.Now()
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
		var released []queuedNudge
		inFlight := state.InFlight[:0]
		for _, item := range state.InFlight {
			if !want[item.ID] {
				inFlight = append(inFlight, item)
				continue
			}
			item.ClaimedAt = time.Time{}
			item.LeaseUntil = time.Time{}
			if !deliverAfter.IsZero() {
				item.DeliverAfter = deliverAfter.UTC()
			}
			released = append(released, item)
		}
		state.InFlight = inFlight
		state.Pending = append(state.Pending, released...)
		sortQueuedNudges(state)
		return nil
	})
}

// fileRecordQueuedNudgeFailure charges one failed delivery attempt to each of
// the pending and in-flight ids (failedQueuedNudge) and returns the ones it
// dead-lettered. The dead-lettered slice reports which of the given IDs actually
// reached the dead letter queue in this pass, which is not the same set the caller asked
// for: an item delivered and acked since the caller last read the queue is
// simply gone by the time the write pass takes the lock. doNudgeDrop reports
// per-ID outcomes from this slice for exactly that reason;
// recordQueuedNudgeFailureWithStore discards it.
func fileRecordQueuedNudgeFailure(cityPath string, store beads.NudgesStore, ids []string, cause error, now time.Time) ([]queuedNudge, error) {
	if len(ids) == 0 {
		return nil, nil
	}
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
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	var deadLettered []queuedNudge
	err := withNudgeQueueState(cityPath, func(state *nudgeQueueState) error {
		deadLettered = deadLettered[:0]
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
		var requeued []queuedNudge
		var dead []queuedNudge
		pending := state.Pending[:0]
		for _, item := range state.Pending {
			if !want[item.ID] {
				pending = append(pending, item)
				continue
			}
			updated, deadLetter := failedQueuedNudge(item, cause, now)
			if deadLetter {
				dead = append(dead, updated)
				deadLettered = append(deadLettered, updated)
				continue
			}
			requeued = append(requeued, updated)
		}
		state.Pending = pending
		inFlight := state.InFlight[:0]
		for _, item := range state.InFlight {
			if !want[item.ID] {
				inFlight = append(inFlight, item)
				continue
			}
			updated, deadLetter := failedQueuedNudge(item, cause, now)
			if deadLetter {
				dead = append(dead, updated)
				deadLettered = append(deadLettered, updated)
				continue
			}
			requeued = append(requeued, updated)
		}
		state.InFlight = inFlight
		state.Pending = append(state.Pending, requeued...)
		state.Dead = append(state.Dead, dead...)
		sortQueuedNudges(state)
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Mark backing beads terminal outside the queue lock, best-effort. The
	// dead-letter transition above is authoritative; a failed bead write must
	// not roll it back, or the items bounce between InFlight and Pending
	// forever and stale backlogs wedge every delivery pass that claims them.
	// pruneDeadQueuedNudges repairs dead entries whose backing bead missed
	// terminal state, so drift converges on later queue operations.
	for _, item := range deadLettered {
		if markErr := markQueuedNudgeTerminal(store, item, "failed", item.LastError, "", now); markErr != nil && nudgeWarningWriter != nil {
			fmt.Fprintf(nudgeWarningWriter, "gc nudge: warning: marking dead-lettered nudge %q terminal: %v\n", item.ID, markErr) //nolint:errcheck
		}
	}
	return deadLettered, nil
}

// fileNudgeQueue is the flock'd state.json queue behind the NudgeQueue
// contract. Each verb is the queue mechanic its facade always ran, so file
// mode behaves as it did before the seam. It keeps two behaviors the contract
// does not have, declared to the conformance suite: Enqueue, ClaimDue,
// ClaimBatch, ListFor, ListForAgent, Lookup, Ack, ReleaseClaims and
// RecordFailure first run the whole-queue recovery (VerbsMaintain), and an
// acked id is forgotten (ForgetsRetiredIDs).
//
// store is forwarded only to the verbs that always took one (Enqueue,
// RecordFailure, Rollback, WithdrawQueuedWaitNudges); the others open and close
// their own, so every store-open count is what it was.
type fileNudgeQueue struct {
	cityPath string
	store    beads.NudgesStore
	clk      clock.Clock
}

var (
	_ storebinding.NudgeQueue   = (*fileNudgeQueue)(nil)
	_ nudgeDispatchSkipRecorder = (*fileNudgeQueue)(nil)
	_ nudgeClaimRepinner        = (*fileNudgeQueue)(nil)
)

// fileClaimTarget is ct as the file mechanics read a target: exactly ct's
// queue keys, and its fence.
func fileClaimTarget(ct storebinding.ClaimTarget) nudgeTarget {
	return nudgeTarget{
		fromClaim:         true,
		claimKeys:         ct.QueueKeys,
		sessionID:         ct.SessionID,
		continuationEpoch: ct.ContinuationEpoch,
	}
}

// fileClaimMatcher is the claim predicate for ct, asking ct's census at most
// once per claim.
func fileClaimMatcher(ct storebinding.ClaimTarget) func(queuedNudge) bool {
	target := fileClaimTarget(ct)
	var census func() map[string]struct{}
	if ct.LiveFenceSessions != nil {
		var once sync.Once
		var live map[string]struct{}
		census = func() map[string]struct{} {
			once.Do(func() {
				if ids, ok := ct.LiveFenceSessions(); ok {
					live = ids
				}
			})
			return live
		}
	}
	return func(item queuedNudge) bool { return queuedNudgeClaimableForTarget(target, item, census) }
}

func (q *fileNudgeQueue) location() string { return nudgequeue.StatePath(q.cityPath) }

// Enqueue queues item, saving its shadow first.
func (q *fileNudgeQueue) Enqueue(item nudgequeue.Item) error {
	return enqueueQueuedNudgeWithStoreAndClock(q.cityPath, q.store, item, q.clk)
}

// EnqueueDeferred appends a deferred submit.
func (q *fileNudgeQueue) EnqueueDeferred(item nudgequeue.Item) error {
	return nudgequeue.StateFileQueue{CityPath: q.cityPath}.EnqueueDeferred(item)
}

// ClaimDue claims every due nudge ct may take and pins nothing.
func (q *fileNudgeQueue) ClaimDue(ct storebinding.ClaimTarget, now time.Time) ([]nudgequeue.Item, error) {
	return claimDueQueuedNudgesMatching(q.cityPath, now, fileClaimMatcher(ct))
}

// ClaimBatch claims ct's next delivery batch. It keeps the file queue's batch
// rule (claimDueQueuedNudgeBatchForTarget, wholePinnedBatch), not the
// contract's "every live member due": a pinned batch with a due member is
// claimed whole whatever its other members' deliver_after, so ct's census may
// be asked about a member that is not yet due.
func (q *fileNudgeQueue) ClaimBatch(ct storebinding.ClaimTarget, now time.Time) ([]nudgequeue.Item, error) {
	target := fileClaimTarget(ct)
	return claimDueQueuedNudges(q.cityPath, now, fileClaimMatcher(ct), &target)
}

// ListForAgent lists the items addressed to exactly agent. The empty agent
// addresses nothing.
func (q *fileNudgeQueue) ListForAgent(agent string, now time.Time) ([]nudgequeue.Item, []nudgequeue.Item, []nudgequeue.Item, error) {
	if strings.TrimSpace(agent) == "" {
		return nil, nil, nil, nil
	}
	return listQueuedNudges(q.cityPath, agent, now)
}

// ListFor lists the items addressed to any of ct's queue keys.
func (q *fileNudgeQueue) ListFor(ct storebinding.ClaimTarget, now time.Time) ([]nudgequeue.Item, []nudgequeue.Item, []nudgequeue.Item, error) {
	return listQueuedNudgesForTargetWithClock(q.cityPath, fileClaimTarget(ct), now, q.clk)
}

// Lookup reports the bucket each id is in after a full maintenance pass.
func (q *fileNudgeQueue) Lookup(ids []string, now time.Time) (map[string]nudgequeue.Bucket, error) {
	return fileLookupQueuedNudges(q.cityPath, ids, now)
}

// DueKeys reads, without the lock, the agents of the pending items due by now
// and of the in-flight items whose lease ran out before now.
func (q *fileNudgeQueue) DueKeys(now time.Time) ([]string, error) {
	state, err := nudgequeue.LoadState(q.cityPath)
	if err != nil {
		return nil, err
	}
	due := map[string]bool{}
	for _, item := range state.Pending {
		if item.Agent == "" || (!item.DeliverAfter.IsZero() && item.DeliverAfter.After(now)) {
			continue
		}
		due[item.Agent] = true
	}
	for _, item := range state.InFlight {
		if item.Agent == "" || item.LeaseUntil.IsZero() || !item.LeaseUntil.Before(now) {
			continue
		}
		due[item.Agent] = true
	}
	keys := make([]string, 0, len(due))
	for key := range due {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, nil
}

// Empty reads, without the lock, whether the queue holds no item.
func (q *fileNudgeQueue) Empty() (bool, error) {
	state, err := nudgequeue.LoadState(q.cityPath)
	if err != nil {
		return false, err
	}
	return !nudgeQueueHasWork(&state), nil
}

// Snapshot reads the queue without the lock.
func (q *fileNudgeQueue) Snapshot() (nudgequeue.State, error) {
	return nudgequeue.LoadState(q.cityPath)
}

// Ack retires ids with outcome.
func (q *fileNudgeQueue) Ack(ids []string, outcome, reason, commitBoundary string) error {
	return fileAckQueuedNudges(q.cityPath, ids, outcome, reason, commitBoundary)
}

// ReleaseClaims returns in-flight ids to pending.
func (q *fileNudgeQueue) ReleaseClaims(ids []string, notBefore time.Time) error {
	return fileReleaseQueuedNudgeClaims(q.cityPath, ids, notBefore)
}

// RecordFailure charges a failed delivery attempt to ids.
func (q *fileNudgeQueue) RecordFailure(ids []string, cause error, now time.Time) ([]nudgequeue.Item, error) {
	return fileRecordQueuedNudgeFailure(q.cityPath, q.store, ids, cause, now)
}

// Rollback dead-letters item after the action that queued it failed.
func (q *fileNudgeQueue) Rollback(item nudgequeue.Item, reason string) error {
	return fileRollbackQueuedNudge(q.cityPath, nudgeFrontDoor(q.store), item, reason)
}

// WithdrawQueuedWaitNudges withdraws the queued wait nudges ids.
func (q *fileNudgeQueue) WithdrawQueuedWaitNudges(ids []string) error {
	return nudgequeue.StateFileQueue{CityPath: q.cityPath, Shadows: q.store.Store}.WithdrawQueuedWaitNudges(ids)
}

// Maintain runs the debounced whole-queue sweep at now. The sweep's own time
// budget bounds it, so ctx is not consulted, and it reports nothing beyond
// its error.
func (q *fileNudgeQueue) Maintain(_ context.Context, now time.Time) (storebinding.NudgeMaintenance, error) {
	return storebinding.NudgeMaintenance{}, runNudgeQueueMaintenanceSweep(q.cityPath, now)
}

// RecordDispatchSkips adds a dispatch tick's skip counts to the persisted
// totals.
func (q *fileNudgeQueue) RecordDispatchSkips(counts map[string]int64) error {
	return recordNudgeDispatchSkips(q.cityPath, counts)
}

// DispatchSkips reads the persisted skip totals without the lock.
func (q *fileNudgeQueue) DispatchSkips() (map[string]int64, error) {
	state, err := nudgequeue.LoadState(q.cityPath)
	if err != nil {
		return nil, err
	}
	return state.DispatchSkips, nil
}

// RepinClaims pins the in-flight ids to batch, all or nothing.
func (q *fileNudgeQueue) RepinClaims(ids []string, batch string) error {
	return pinQueuedNudgeClaims(q.cityPath, ids, batch)
}
