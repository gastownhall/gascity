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
	return updateNudgeQueueState(cityPath, "repin", func(state *nudgeQueueState) error {
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

// nudgeMaintenanceStore lazily opens the nudges store a verb pays its shadow
// debts with (settleNudgeShadowDebts), after the queue lock is released and
// only when the verb has debts to pay. On the common tick nothing leaves the
// queue, there is no debt, and the store is never opened, so N idle
// `gc nudge poll` sidecars do not each dial the sql-server every 2s. See
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

// frontDoor opens the underlying store on first need and returns its front
// door, nil when no store opens.
func (m *nudgeMaintenanceStore) frontDoor() *nudgequeue.Store {
	m.ensureOpen()
	return m.front
}

// ensureOpen opens the underlying store exactly once (idempotent).
func (m *nudgeMaintenanceStore) ensureOpen() {
	if !m.opened {
		m.opened = true
		m.store, m.handle = openOwnedNudgeBeadStore(m.cityPath)
		if m.store.Store != nil {
			m.front = nudgeFrontDoor(m.store)
		}
	}
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

// nudgeQueueHasWork reports whether the queue holds any item a contract verb
// serves or a lister reports. Retired items wait only for their shadow debt.
func nudgeQueueHasWork(state *nudgeQueueState) bool {
	return len(state.Pending) > 0 || len(state.InFlight) > 0 || len(state.Dead) > 0
}

// maintainNudgeQueue runs the whole-queue recovery passes in the lock: lapsed
// leases return to pending, items past their deadline are dead-lettered (their
// shadow debts taken by ledger), and dead letters and retired items age out.
// It is pure: it touches no store.
func maintainNudgeQueue(state *nudgeQueueState, ledger *nudgeShadowLedger, now, deadline time.Time, clk clock.Clock) {
	recoverExpiredInFlightNudgesWithClock(state, ledger, now, deadline, clk)
	pruneExpiredQueuedNudgesWithClock(state, ledger, now, deadline, clk)
	pruneDeadQueuedNudgesWithClock(state, now, deadline, clk)
	pruneRetiredQueuedNudges(state, now)
}

func claimDueQueuedNudgesMatching(cityPath string, now time.Time, match func(queuedNudge) bool) ([]queuedNudge, error) {
	plain := func(item queuedNudge, _ func() map[string]struct{}) bool { return match(item) }
	return claimDueQueuedNudges(cityPath, now, plain, nil, nil)
}

// nudgeClaimMatch is a claim predicate given the claim's Leg-B census (see
// queuedNudgeClaimableForTarget).
type nudgeClaimMatch func(item queuedNudge, liveSessions func() map[string]struct{}) bool

// nudgeClaimFence is a queued item's fence: its id and the session it is
// fenced to.
type nudgeClaimFence struct {
	id        string
	sessionID string
}

// nudgeClaimCensus is a claim's Leg-B session census. The census lists the
// city's sessions, so it is read outside the queue lock, before the claim's
// locked run (ga-fsp5w9): a run that meets a candidate fenced to another
// session before the census was read records that it needs it and treats the
// candidate as not claimable, which is the fail-closed answer.
//
// The census judges only the fences of the snapshot read before it (fences):
// a session such a fence names existed before the census was read, so the
// census lists it or saw it gone. A fence first seen after that snapshot, on
// an item queued or re-fenced since, may name a session the census predates,
// so it has no census and fails closed too.
type nudgeClaimCensus struct {
	read   bool
	live   map[string]struct{}
	needed bool
	fences map[nudgeClaimFence]struct{}
}

// recordFences records the fences of the items state serves, as a run that
// has no census yet sees them.
func (c *nudgeClaimCensus) recordFences(state *nudgeQueueState) {
	c.fences = make(map[nudgeClaimFence]struct{})
	for _, bucket := range [][]queuedNudge{state.Pending, state.InFlight} {
		for _, item := range bucket {
			if item.SessionID != "" {
				c.fences[nudgeClaimFence{id: item.ID, sessionID: item.SessionID}] = struct{}{}
			}
		}
	}
}

// liveSessionsFor is the census a run gives the candidate fenced as fence:
// none before the census is read, and after, the census only for a fence of
// the snapshot read before it.
func (c *nudgeClaimCensus) liveSessionsFor(fence nudgeClaimFence) map[string]struct{} {
	if !c.read {
		c.needed = true
		return nil
	}
	if _, ok := c.fences[fence]; !ok {
		return nil
	}
	return c.live
}

// nudgeClaimRun is one claim, run as a pure queue callback: the whole-queue
// maintenance pass, then the claim.
type nudgeClaimRun struct {
	cityPath    string
	now         time.Time
	match       nudgeClaimMatch
	readCensus  func() (map[string]struct{}, bool)
	batchTarget *nudgeTarget

	census  nudgeClaimCensus
	ledger  *nudgeShadowLedger
	claimed []queuedNudge
}

func newNudgeClaimRun(cityPath string, now time.Time, match nudgeClaimMatch, readCensus func() (map[string]struct{}, bool), batchTarget *nudgeTarget) *nudgeClaimRun {
	return &nudgeClaimRun{cityPath: cityPath, now: now, match: match, readCensus: readCensus, batchTarget: batchTarget, ledger: newNudgeShadowLedger(now)}
}

// run is the claim's queue callback. Its maintenance budget starts when the
// run does, not at now: a claim reaches its locked run after a dry run, maybe
// a census read and a lock wait, and still has its whole budget to recover
// lapsed leases. now is only the time items are judged at.
func (r *nudgeClaimRun) run(state *nudgeQueueState) error {
	r.ledger.reset()
	r.claimed = nil
	if !r.census.read {
		r.census.recordFences(state)
	}
	maintainNudgeQueue(state, r.ledger, r.now, time.Now().Add(nudgeEnqueueMaintenanceBudget), clock.Real{})
	r.claimed = claimQueuedNudges(state, r.now, func(item queuedNudge) bool {
		fence := nudgeClaimFence{id: item.ID, sessionID: item.SessionID}
		return r.match(item, func() map[string]struct{} { return r.census.liveSessionsFor(fence) })
	}, r.batchTarget)
	return nil
}

// dryRun runs the claim on a lock-free snapshot and reports whether it would
// change the queue. When the snapshot has a candidate only the census can
// decide, it reads the census once, outside any lock, and runs again.
func (r *nudgeClaimRun) dryRun() (bool, error) {
	changed, err := nudgequeue.DryRun(r.cityPath, r.run)
	if err != nil || !r.census.needed || r.census.read || r.readCensus == nil {
		return changed, err
	}
	live, ok := r.readCensus()
	if !ok {
		live = nil
	}
	r.census.read, r.census.live = true, live
	return nudgequeue.DryRun(r.cityPath, r.run)
}

// claimDueQueuedNudges claims every due nudge match accepts and pins nothing,
// or, given a batchTarget, that target's next delivery batch
// (claimDueQueuedNudgeBatchForTarget). A claim that would change nothing (no
// claimable item, no maintenance to persist) is answered from a lock-free
// snapshot and takes no lock; otherwise the claim runs again under the lock,
// with the census its dry run read, and that run's answer is authoritative. A
// Leg-B candidate whose fence is not one of the snapshot read before the
// census (an item queued or re-fenced since) has no census and stays pending
// for the next pass.
func claimDueQueuedNudges(cityPath string, now time.Time, match nudgeClaimMatch, readCensus func() (map[string]struct{}, bool), batchTarget *nudgeTarget) ([]queuedNudge, error) {
	r := newNudgeClaimRun(cityPath, now, match, readCensus, batchTarget)
	if changed, err := r.dryRun(); err == nil && !changed {
		return nil, nil
	}
	if nudgeClaimAfterDryRun != nil {
		nudgeClaimAfterDryRun()
	}
	if err := withNudgeQueueState(cityPath, "claim", r.run); err != nil {
		return nil, err
	}
	settleNudgeShadowDebts(cityPath, beads.NudgesStore{}, r.ledger.taken, nudgeEnqueueMaintenanceBudget, now)
	return r.claimed, nil
}

// nudgeClaimAfterDryRun is a test seam that runs between a claim's dry run and
// its locked run, so a test can change the queue in between. Nil in
// production. Tests that set it must stay serial.
var nudgeClaimAfterDryRun func()

// wouldClaimQueuedNudges reports, without the lock and without claiming,
// whether claimDueQueuedNudges would claim anything at now.
func wouldClaimQueuedNudges(cityPath string, now time.Time, match nudgeClaimMatch, readCensus func() (map[string]struct{}, bool), batchTarget *nudgeTarget) (bool, error) {
	r := newNudgeClaimRun(cityPath, now, match, readCensus, batchTarget)
	if _, err := r.dryRun(); err != nil {
		return false, err
	}
	return len(r.claimed) > 0, nil
}

// claimQueuedNudges moves the due items match accepts from pending to in
// flight and returns them: every one, pinning nothing, or, given a
// batchTarget, that target's next delivery batch.
func claimQueuedNudges(state *nudgeQueueState, now time.Time, match func(queuedNudge) bool, batchTarget *nudgeTarget) []queuedNudge {
	var claimed []queuedNudge
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
	return claimed
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
	return listQueuedNudgesMatching(cityPath, now, clock.Real{}, func(agent string) bool { return agent == agentName })
}

// listQueuedNudgesForTargetWithClock is the clock-injectable core of
// listQueuedNudgesForTarget (mirrors enqueueQueuedNudgeWithStoreAndClock's
// seam). Tests can pass a clock.Fake so the maintenance pass's budget
// (nudgeEnqueueMaintenanceBudget from the pass's start) is spent against a
// controlled clock instead of real wall-clock time.
func listQueuedNudgesForTargetWithClock(cityPath string, target nudgeTarget, now time.Time, clk clock.Clock) ([]queuedNudge, []queuedNudge, []queuedNudge, error) {
	return listQueuedNudgesMatching(cityPath, now, clk, target.matchesQueueAgent)
}

// listQueuedNudgesMatching runs the maintenance pass at now and lists the
// items whose agent matches, by bucket. The pass's budget starts when its run
// does, by clk, so a listing that waited for the lock still maintains.
func listQueuedNudgesMatching(cityPath string, now time.Time, clk clock.Clock, matches func(agent string) bool) ([]queuedNudge, []queuedNudge, []queuedNudge, error) {
	ledger := newNudgeShadowLedger(now)
	var pending, inFlight, dead []queuedNudge
	err := updateNudgeQueueState(cityPath, "list", func(state *nudgeQueueState) error {
		ledger.reset()
		pending, inFlight, dead = nil, nil, nil
		maintainNudgeQueue(state, ledger, now, clk.Now().Add(nudgeEnqueueMaintenanceBudget), clk)
		for _, item := range state.Pending {
			if matches(item.Agent) {
				pending = append(pending, item)
			}
		}
		for _, item := range state.InFlight {
			if matches(item.Agent) {
				inFlight = append(inFlight, item)
			}
		}
		for _, item := range state.Dead {
			if matches(item.Agent) {
				dead = append(dead, item)
			}
		}
		return nil
	})
	if err != nil {
		return pending, inFlight, dead, err
	}
	settleNudgeShadowDebts(cityPath, beads.NudgesStore{}, ledger.taken, nudgeEnqueueMaintenanceBudget, now)
	return pending, inFlight, dead, nil
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
	beadID, created, err := ensureQueuedNudgeBead(store, item)
	if err != nil {
		return err
	}
	if beadID != "" {
		item.BeadID = beadID
	}
	now := clk.Now()
	ledger := newNudgeShadowLedger(now)
	err = updateNudgeQueueState(cityPath, "enqueue", func(state *nudgeQueueState) error {
		ledger.reset()
		// The budget runs from this run's start, not from now: a run that
		// waited for the lock still has its whole budget to supersede.
		deadline := clk.Now().Add(nudgeEnqueueMaintenanceBudget)
		maintainNudgeQueue(state, ledger, now, deadline, clk)
		if queuedNudgeExists(state, item.ID) {
			return nil
		}
		if err := dropRetiredDebtsOnShadow(state, item, clk.Now()); err != nil {
			return err
		}
		// Supersede pending and in-flight nudges for the same (agent, source, reference).
		if item.Reference != nil && item.Reference.ID != "" {
			matchesSupersession := func(existing queuedNudge) bool {
				return existing.Agent == item.Agent && existing.Source == item.Source &&
					existing.Reference != nil && existing.Reference.Kind == item.Reference.Kind &&
					existing.Reference.ID == item.Reference.ID
			}
			supersede := func(existing queuedNudge) {
				existing.DeadAt = now.UTC()
				existing.LastError = "superseded"
				ledger.owe(&existing, "superseded", "superseded", "", now)
				state.Dead = append(state.Dead, existing)
			}
			filtered := state.Pending[:0]
			for i, existing := range state.Pending {
				if clk.Now().After(deadline) {
					filtered = append(filtered, state.Pending[i:]...)
					break
				}
				if matchesSupersession(existing) {
					supersede(existing)
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
					supersede(existing)
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
	if err != nil {
		if created && store.Store != nil && beadID != "" {
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
	settleNudgeShadowDebts(cityPath, store, ledger.taken, nudgeEnqueueMaintenanceBudget, now)
	return nil
}

// errNudgeShadowRetiring refuses to queue an item on the shadow of an earlier,
// acked item whose payer holds its retired debt: the payer writes the copy it
// took whatever the queue says, so its terminal write would close the shadow
// of an item the queue serves. Nothing is queued, and the queue reads busy:
// once the payer has written, the shadow is closed and a retry saves the item
// a new one.
var errNudgeShadowRetiring = fmt.Errorf("an earlier nudge's terminal write to its shadow is in flight: %w", nudgequeue.ErrQueueBusy)

// dropRetiredDebtsOnShadow drops the retired items whose id is item's and whose
// unpaid debt names item's shadow: Save found that shadow still open and the new
// item reuses it, so paying the old debt would close the shadow of an item the
// queue serves. The new item's own exit will terminalize it. A debt a payer
// holds at now (nudgeDebtLeased) is not dropped, and the item may not reuse
// the shadow: dropping it would not stop that payer's write, so it fails with
// errNudgeShadowRetiring and leaves state unchanged.
//
// A payer that wrote and confirmed between the item's Save and this run left
// no retired item to find, so the item is queued on the shadow that payer
// closed. That is the window rollbackNudgeShadows describes for a rollback
// (ga-w68ksk): a Save outside the lock reuses a shadow that a concurrent
// terminal write closes.
func dropRetiredDebtsOnShadow(state *nudgeQueueState, item queuedNudge, now time.Time) error {
	if item.BeadID == "" || len(state.Retired) == 0 {
		return nil
	}
	onShadow := func(retired queuedNudge) bool {
		return retired.ID == item.ID && retired.BeadID == item.BeadID
	}
	for _, retired := range state.Retired {
		if onShadow(retired) && nudgeDebtLeased(retired, now) {
			return fmt.Errorf("queueing nudge %q on shadow %s: %w", item.ID, item.BeadID, errNudgeShadowRetiring)
		}
	}
	filtered := state.Retired[:0]
	for _, retired := range state.Retired {
		if onShadow(retired) {
			continue
		}
		filtered = append(filtered, retired)
	}
	state.Retired = filtered
	return nil
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
	return withNudgeQueueState(cityPath, "record_skips", func(state *nudgeQueueState) error {
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
	return runNudgeQueueMaintenanceSweepWithStore(cityPath, beads.NudgesStore{}, now)
}

// runNudgeQueueMaintenanceSweepWithStore is runNudgeQueueMaintenanceSweep
// paying the debts with store, or with a store it opens when store is the
// zero store.
func runNudgeQueueMaintenanceSweepWithStore(cityPath string, store beads.NudgesStore, now time.Time) error {
	if nudgeMaintenanceSweepDebounced(cityPath, now) {
		return nil
	}
	ledger := newNudgeShadowLedger(now)
	if err := updateNudgeQueueState(cityPath, "maintain", func(state *nudgeQueueState) error {
		ledger.reset()
		maintainNudgeQueue(state, ledger, now, now.Add(nudgeMaintenanceSweepBudget), clock.Real{})
		// The controller also pays the debts other payers left: a crash, a
		// spent budget, a failing store.
		takeDueNudgeShadowDebts(state, ledger, len(ledger.taken)+nudgeShadowPayBatch)
		return nil
	}); err != nil {
		return err
	}
	settleNudgeShadowDebts(cityPath, store, ledger.taken, nudgeControllerPayBudget, now)
	return nil
}

func pruneExpiredQueuedNudges(state *nudgeQueueState, ledger *nudgeShadowLedger, now, deadline time.Time) {
	pruneExpiredQueuedNudgesWithClock(state, ledger, now, deadline, clock.Real{})
}

// pruneExpiredQueuedNudgesWithClock dead-letters the pending items past their
// deadline, each owing its shadow an expired record.
func pruneExpiredQueuedNudgesWithClock(state *nudgeQueueState, ledger *nudgeShadowLedger, now, deadline time.Time, clk clock.Clock) {
	filtered := state.Pending[:0]
	for i, item := range state.Pending {
		if clk.Now().After(deadline) {
			filtered = append(filtered, state.Pending[i:]...)
			break
		}
		if !item.ExpiresAt.IsZero() && !item.ExpiresAt.After(now) {
			expireQueuedNudge(state, ledger, item, now)
			continue
		}
		filtered = append(filtered, item)
	}
	state.Pending = filtered
	sortQueuedNudges(state)
}

// expireQueuedNudge dead-letters item as expired.
func expireQueuedNudge(state *nudgeQueueState, ledger *nudgeShadowLedger, item queuedNudge, now time.Time) {
	item.DeadAt = now.UTC()
	if item.LastError == "" {
		item.LastError = "expired"
	}
	ledger.owe(&item, "expired", item.LastError, "", now)
	state.Dead = append(state.Dead, item)
}

func recoverExpiredInFlightNudges(state *nudgeQueueState, ledger *nudgeShadowLedger, now, deadline time.Time) {
	recoverExpiredInFlightNudgesWithClock(state, ledger, now, deadline, clock.Real{})
}

// recoverExpiredInFlightNudgesWithClock dead-letters the in-flight items past
// their deadline and returns those whose lease ran out to pending.
func recoverExpiredInFlightNudgesWithClock(state *nudgeQueueState, ledger *nudgeShadowLedger, now, deadline time.Time, clk clock.Clock) {
	filtered := state.InFlight[:0]
	for i, item := range state.InFlight {
		if clk.Now().After(deadline) {
			filtered = append(filtered, state.InFlight[i:]...)
			break
		}
		if !item.ExpiresAt.IsZero() && !item.ExpiresAt.After(now) {
			expireQueuedNudge(state, ledger, item, now)
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
}

// pruneDeadQueuedNudges removes dead-letter items older than
// defaultQueuedNudgeDeadRetention whose shadow owes nothing: one with no shadow
// (no debt: a shadowless item, NQ-F2) or whose debt is paid. A dead letter
// whose debt is unpaid is retained, so terminal history is not lost while the
// store write is outstanding; it costs no store read per pass. A dead letter an
// older binary wrote that names a shadow gets a one-time Verify debt. Items
// with a zero DeadAt are retained.
func pruneDeadQueuedNudges(state *nudgeQueueState, now, deadline time.Time) {
	pruneDeadQueuedNudgesWithClock(state, now, deadline, clock.Real{})
}

func pruneDeadQueuedNudgesWithClock(state *nudgeQueueState, now, deadline time.Time, clk clock.Clock) {
	cutoff := now.Add(-defaultQueuedNudgeDeadRetention)
	filtered := state.Dead[:0]
	for i, item := range state.Dead {
		if clk.Now().After(deadline) {
			filtered = append(filtered, state.Dead[i:]...)
			break
		}
		verifyDebtForLegacyDeadLetter(&item, now)
		owesNothing := item.Shadow == nil || item.Shadow.Paid
		if owesNothing && !item.DeadAt.IsZero() && item.DeadAt.Before(cutoff) {
			continue
		}
		filtered = append(filtered, item)
	}
	state.Dead = filtered
}

// pruneRetiredQueuedNudges drops, with a warning, a retired item whose shadow
// debt has gone unpaid for defaultQueuedNudgeTTL: the store has failed for a
// day, and the stale-shadow sweep owns that shadow. This bounds the file.
func pruneRetiredQueuedNudges(state *nudgeQueueState, now time.Time) {
	cutoff := now.Add(-defaultQueuedNudgeTTL)
	filtered := state.Retired[:0]
	for _, item := range state.Retired {
		if item.Shadow == nil || item.Shadow.At.Before(cutoff) {
			if item.Shadow != nil && nudgeWarningWriter != nil {
				fmt.Fprintf(nudgeWarningWriter, "gc nudge: warning: dropping acked nudge %q whose shadow write has failed since %s: %s\n", //nolint:errcheck
					item.ID, item.Shadow.At.Format(time.RFC3339), item.Shadow.LastError)
			}
			continue
		}
		filtered = append(filtered, item)
	}
	state.Retired = filtered
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

// withNudgeQueueState runs fn under the queue lock, waiting for it no longer
// than nudgeQueueLockWait; verb names the holder for the lock telemetry
// (observeNudgeQueueLock).
func withNudgeQueueState(cityPath, verb string, fn func(*nudgeQueueState) error) error {
	return nudgequeue.WithStateWithin(cityPath, verb, nudgeQueueLockWait(), fn)
}

// updateNudgeQueueState is withNudgeQueueState for a verb that is often a
// no-op: fn runs first on a lock-free snapshot, and the lock is taken only when
// that run changes the state (nudgequeue.UpdateIfChanged). fn must be pure and
// reset what it captures at its start.
func updateNudgeQueueState(cityPath, verb string, fn func(*nudgeQueueState) error) error {
	return nudgequeue.UpdateIfChangedWithin(cityPath, verb, nudgeQueueLockWait(), fn)
}

// fileLookupQueuedNudges reports the bucket each of the given IDs is in; an ID
// in no bucket is absent. It first runs a full maintenance pass at now, so a
// since-expired or since-recovered item is reported from current state rather
// than a stale snapshot. The pass is pure, so unbounded it is bounded by the
// queue's size in memory.
func fileLookupQueuedNudges(cityPath string, ids []string, now time.Time) (map[string]nudgequeue.Bucket, error) {
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	ledger := newNudgeShadowLedger(now)
	var buckets map[string]nudgequeue.Bucket
	err := updateNudgeQueueState(cityPath, "lookup", func(state *nudgeQueueState) error {
		ledger.reset()
		buckets = make(map[string]nudgequeue.Bucket, len(ids))
		maintainNudgeQueue(state, ledger, now, noMaintenanceDeadline(), clock.Real{})
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
	if err != nil {
		return buckets, err
	}
	settleNudgeShadowDebts(cityPath, beads.NudgesStore{}, ledger.taken, nudgeEnqueueMaintenanceBudget, now)
	return buckets, nil
}

// fileRollbackQueuedNudge dead-letters item after the action that queued it
// failed, and closes its shadow as failed with reason: the dead letter owes
// that write, paid after the lock with store, or with a store it opens when
// store is the zero store. An item no longer in the queue still has its shadow
// closed, found by its id, through store alone.
func fileRollbackQueuedNudge(cityPath string, store beads.NudgesStore, item queuedNudge, reason string) error {
	if cityPath == "" || item.ID == "" {
		return nil
	}
	now := time.Now()
	found := false
	ledger := newNudgeShadowLedger(now)
	err := updateNudgeQueueState(cityPath, "rollback", func(state *nudgeQueueState) error {
		ledger.reset()
		found = false
		removed := []queuedNudge(nil)
		state.Pending, removed = takeQueuedNudgesByID(state.Pending, item.ID, removed)
		state.InFlight, removed = takeQueuedNudgesByID(state.InFlight, item.ID, removed)
		for _, queued := range removed {
			found = true
			queued.LastError = reason
			queued.DeadAt = now.UTC()
			ledger.owe(&queued, "failed", reason, "", now)
			state.Dead = append(state.Dead, queued)
		}
		sortQueuedNudges(state)
		return nil
	})
	if err != nil {
		return err
	}
	if found {
		settleNudgeShadowDebts(cityPath, store, ledger.taken, nudgeEnqueueMaintenanceBudget, now)
		return nil
	}
	item.LastError = reason
	return nudgeFrontDoor(store).Terminalize(item, "failed", reason, "", now)
}

// fileAckQueuedNudges retires the pending and in-flight ids. The queue write
// commits whatever the shadow store does: each acked item that can have a
// shadow moves to the retired bucket owing its shadow the outcome, and the
// verb pays that debt after the lock is released (settleNudgeShadowDebts). A
// failed shadow write leaves the debt for a later payer; it does not undo the
// ack, so it cannot redeliver the item.
func fileAckQueuedNudges(cityPath string, ids []string, outcome, reason, commitBoundary string) error {
	if len(ids) == 0 {
		return nil
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	now := time.Now()
	ledger := newNudgeShadowLedger(now)
	err := updateNudgeQueueState(cityPath, "ack", func(state *nudgeQueueState) error {
		ledger.reset()
		maintainNudgeQueue(state, ledger, now, time.Now().Add(nudgeEnqueueMaintenanceBudget), clock.Real{})
		retire := func(item queuedNudge) {
			ledger.owe(&item, outcome, reason, commitBoundary, now)
			if item.Shadow != nil {
				state.Retired = append(state.Retired, item)
			}
		}
		filtered := state.Pending[:0]
		for _, item := range state.Pending {
			if want[item.ID] {
				retire(item)
				continue
			}
			filtered = append(filtered, item)
		}
		state.Pending = filtered
		inFlight := state.InFlight[:0]
		for _, item := range state.InFlight {
			if want[item.ID] {
				retire(item)
				continue
			}
			inFlight = append(inFlight, item)
		}
		state.InFlight = inFlight
		sortQueuedNudges(state)
		return nil
	})
	if err != nil {
		return err
	}
	settleNudgeShadowDebts(cityPath, beads.NudgesStore{}, ledger.taken, nudgeEnqueueMaintenanceBudget, now)
	return nil
}

// fileReleaseQueuedNudgeClaims returns claimed nudges to pending, keeping
// their delivery batch. A non-zero deliverAfter keeps them from being claimed
// again before then.
func fileReleaseQueuedNudgeClaims(cityPath string, ids []string, deliverAfter time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	now := time.Now()
	ledger := newNudgeShadowLedger(now)
	err := updateNudgeQueueState(cityPath, "release", func(state *nudgeQueueState) error {
		ledger.reset()
		maintainNudgeQueue(state, ledger, now, time.Now().Add(nudgeEnqueueMaintenanceBudget), clock.Real{})
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
	if err != nil {
		return err
	}
	settleNudgeShadowDebts(cityPath, beads.NudgesStore{}, ledger.taken, nudgeEnqueueMaintenanceBudget, now)
	return nil
}

// fileRecordQueuedNudgeFailure charges one failed delivery attempt to each of
// the pending and in-flight ids (failedQueuedNudge) and returns the ones it
// dead-lettered. The dead-lettered slice reports which of the given IDs actually
// reached the dead letter queue in this pass, which is not the same set the caller asked
// for: an item delivered and acked since the caller last read the queue is
// simply gone by the time the write pass takes the lock. doNudgeDrop reports
// per-ID outcomes from this slice for exactly that reason;
// recordQueuedNudgeFailureWithStore discards it.
//
// The dead-letter transition is authoritative: each dead letter owes its shadow
// a failed record, paid after the lock is released, and a failed payment
// leaves the debt for a later payer instead of rolling the transition back.
//
// The maintenance budget starts when the run does: a failure record that
// waited for the lock, or is a retry after a busy one, still maintains.
func fileRecordQueuedNudgeFailure(cityPath string, store beads.NudgesStore, ids []string, cause error, now time.Time) ([]queuedNudge, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	ledger := newNudgeShadowLedger(now)
	var deadLettered []queuedNudge
	err := updateNudgeQueueState(cityPath, "record_failure", func(state *nudgeQueueState) error {
		ledger.reset()
		deadLettered = nil
		maintainNudgeQueue(state, ledger, now, time.Now().Add(nudgeEnqueueMaintenanceBudget), clock.Real{})
		var requeued []queuedNudge
		fail := func(item queuedNudge) {
			updated, deadLetter := failedQueuedNudge(item, cause, now)
			if !deadLetter {
				requeued = append(requeued, updated)
				return
			}
			ledger.owe(&updated, "failed", updated.LastError, "", now)
			state.Dead = append(state.Dead, updated)
			deadLettered = append(deadLettered, updated)
		}
		pending := state.Pending[:0]
		for _, item := range state.Pending {
			if !want[item.ID] {
				pending = append(pending, item)
				continue
			}
			fail(item)
		}
		state.Pending = pending
		inFlight := state.InFlight[:0]
		for _, item := range state.InFlight {
			if !want[item.ID] {
				inFlight = append(inFlight, item)
				continue
			}
			fail(item)
		}
		state.InFlight = inFlight
		state.Pending = append(state.Pending, requeued...)
		sortQueuedNudges(state)
		return nil
	})
	if err != nil {
		return nil, err
	}
	settleNudgeShadowDebts(cityPath, store, ledger.taken, nudgeEnqueueMaintenanceBudget, now)
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
// The queue lock covers state.json and nothing else (ga-fsp5w9): every
// callback is pure, a transition that owes a shadow write records a debt that
// the verb pays after unlocking (nudge_shadow_debts.go), and an acked item that
// owes one waits in the retired bucket, which no verb serves.
//
// store is forwarded only to the verbs that always took one (Enqueue,
// RecordFailure, Rollback, WithdrawQueuedWaitNudges); the others open and close
// their own after the lock, and only when they have debts to pay.
type fileNudgeQueue struct {
	cityPath string
	store    beads.NudgesStore
	clk      clock.Clock
}

var (
	_ storebinding.NudgeQueue   = (*fileNudgeQueue)(nil)
	_ nudgeDispatchSkipRecorder = (*fileNudgeQueue)(nil)
	_ nudgeClaimRepinner        = (*fileNudgeQueue)(nil)
	_ nudgeClaimProber          = (*fileNudgeQueue)(nil)
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

// fileClaimMatcher is the claim predicate for ct. The census it is given is
// the claim's, read outside the queue lock (nudgeClaimCensus); with no
// LiveFenceSessions the fence is strict.
func fileClaimMatcher(ct storebinding.ClaimTarget) nudgeClaimMatch {
	target := fileClaimTarget(ct)
	return func(item queuedNudge, liveSessions func() map[string]struct{}) bool {
		if ct.LiveFenceSessions == nil {
			liveSessions = nil
		}
		return queuedNudgeClaimableForTarget(target, item, liveSessions)
	}
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
	return claimDueQueuedNudges(q.cityPath, now, fileClaimMatcher(ct), ct.LiveFenceSessions, nil)
}

// ClaimBatch claims ct's next delivery batch. It keeps the file queue's batch
// rule (claimDueQueuedNudgeBatchForTarget, wholePinnedBatch), not the
// contract's "every live member due": a pinned batch with a due member is
// claimed whole whatever its other members' deliver_after, so ct's census may
// be asked about a member that is not yet due.
func (q *fileNudgeQueue) ClaimBatch(ct storebinding.ClaimTarget, now time.Time) ([]nudgequeue.Item, error) {
	target := fileClaimTarget(ct)
	return claimDueQueuedNudges(q.cityPath, now, fileClaimMatcher(ct), ct.LiveFenceSessions, &target)
}

// WouldClaimBatch reports, from a lock-free snapshot, whether ClaimBatch for
// ct at now would claim anything. It claims nothing and writes nothing; ct's
// census is asked only for a due candidate fenced to another session, as
// ClaimBatch asks it.
func (q *fileNudgeQueue) WouldClaimBatch(ct storebinding.ClaimTarget, now time.Time) (bool, error) {
	target := fileClaimTarget(ct)
	return wouldClaimQueuedNudges(q.cityPath, now, fileClaimMatcher(ct), ct.LiveFenceSessions, &target)
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
	return fileRollbackQueuedNudge(q.cityPath, q.store, item, reason)
}

// WithdrawQueuedWaitNudges withdraws the queued wait nudges ids.
func (q *fileNudgeQueue) WithdrawQueuedWaitNudges(ids []string) error {
	return nudgequeue.StateFileQueue{CityPath: q.cityPath, Shadows: q.store.Store}.WithdrawQueuedWaitNudges(ids)
}

// Maintain runs the debounced whole-queue sweep at now. The sweep's own time
// budget bounds it, so ctx is not consulted, and it reports nothing beyond
// its error.
func (q *fileNudgeQueue) Maintain(_ context.Context, now time.Time) (storebinding.NudgeMaintenance, error) {
	return storebinding.NudgeMaintenance{}, runNudgeQueueMaintenanceSweepWithStore(q.cityPath, q.store, now)
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

// migrationCandidates reads the queue without the lock and returns the live
// items the queue-mode shim may move out of it, grouped into whole delivery
// batches: pending items and in-flight items whose lease has run out. A batch
// any live member of which holds an unexpired lease waits. Dead letters, and
// items past their deliver-by deadline, are not live and never move.
func (q *fileNudgeQueue) migrationCandidates(now time.Time) (nudgeMigrationCandidates, error) {
	state, err := nudgequeue.LoadState(q.cityPath)
	if err != nil {
		return nudgeMigrationCandidates{}, err
	}
	live := make([]nudgequeue.Item, 0, len(state.Pending)+len(state.InFlight))
	for _, bucket := range [][]nudgequeue.Item{state.Pending, state.InFlight} {
		for _, item := range bucket {
			if !item.ExpiresAt.IsZero() && !item.ExpiresAt.After(now) {
				continue
			}
			live = append(live, item)
		}
	}
	return groupNudgeMigrationBatches(live, func(item nudgequeue.Item) bool {
		return nudgeLeaseHeld(item, now)
	}), nil
}

// removeMigrated removes, under the lock, one batch the shim moved, whole or
// not at all. Each item is matched by (ID, CreatedAt), as WithdrawWaitNudges
// matches, in pending or in flight; an item an old binary re-added meanwhile
// under the same id with a new creation time is a different item and stays.
// If any matched item holds an unexpired lease (a process still writing this
// queue claimed it meanwhile), nothing is removed, so the queue never keeps
// only part of a pinned batch. It returns the items it left in place: all of
// them when one was claimed, or else the ones already gone.
func (q *fileNudgeQueue) removeMigrated(items []nudgequeue.Item, now time.Time) ([]nudgequeue.Item, error) {
	want := make(map[nudgeMigrationKey]bool, len(items))
	for _, item := range items {
		want[nudgeMigrationKeyOf(item)] = true
	}
	removed := make(map[nudgeMigrationKey]bool, len(items))
	err := withNudgeQueueState(q.cityPath, "remove_migrated", func(state *nudgeQueueState) error {
		for _, bucket := range [][]queuedNudge{state.Pending, state.InFlight} {
			for _, item := range bucket {
				if want[nudgeMigrationKeyOf(item)] && nudgeLeaseHeld(item, now) {
					return nil
				}
			}
		}
		take := func(bucket []queuedNudge) []queuedNudge {
			filtered := bucket[:0]
			for _, item := range bucket {
				key := nudgeMigrationKeyOf(item)
				if want[key] {
					removed[key] = true
					continue
				}
				filtered = append(filtered, item)
			}
			return filtered
		}
		state.Pending = take(state.Pending)
		state.InFlight = take(state.InFlight)
		return nil
	})
	if err != nil {
		return nil, err
	}
	var kept []nudgequeue.Item
	for _, item := range items {
		if !removed[nudgeMigrationKeyOf(item)] {
			kept = append(kept, item)
		}
	}
	return kept, nil
}

// importMigrated appends the items the shim moves into the queue, as pending,
// with the claim and the delivery batch cleared and every other field kept
// (nudgeItemForMove) but BeadID. This queue's wait readers and Ack read an
// item's gc:nudge shadow, which a beads-born item never had and a beads-mode
// sweep may have closed, so each appended item gets an open shadow, saved as
// Enqueue saves one, and BeadID names it. With the zero store it opens its own,
// and fails if none opens: an item queued without its shadow is one the wait
// readers never see.
//
// The shadows are saved before the lock, for the items a lock-free snapshot
// says will be imported, so the hold does no store I/O (ga-fsp5w9). The shadows
// it created are rolled back, after the lock, if the queue write fails, and for
// the items the locked pass skipped. An item the locked pass would import but
// the snapshot skipped has no shadow, so the whole import fails and the shim
// retries the batch.
//
// Each item is matched by (ID, CreatedAt), as removeMigrated matches. One the
// queue already holds live is skipped, shadow and all, so a re-run imports
// nothing twice. One it holds as a dead letter is skipped and reported Dead.
// An id it holds under another creation time names a different nudge, and the
// queue keeps one item per id, so that item is skipped and reported Collided.
func (q *fileNudgeQueue) importMigrated(items []nudgequeue.Item) (nudgeImportSkips, error) {
	store := q.store
	if store.Store == nil {
		opened, handle, err := openNudgeBeadStoreOwned(q.cityPath)
		if err != nil {
			return nudgeImportSkips{}, err
		}
		defer closeBeadStoreHandle(handle) //nolint:errcheck // best-effort
		store = opened
	}
	snapshot, err := nudgequeue.LoadState(q.cityPath)
	if err != nil {
		return nudgeImportSkips{}, err
	}
	planned, _ := planNudgeImport(&snapshot, items)
	moved := make(map[nudgeMigrationKey]nudgequeue.Item, len(planned))
	createdFor := make(map[nudgeMigrationKey]string, len(planned))
	var created []string
	for _, item := range planned {
		copied, createdID, err := shadowMovedNudge(store, item)
		if err != nil {
			return nudgeImportSkips{}, rollbackNudgeShadows(store, created, err)
		}
		key := nudgeMigrationKeyOf(item)
		moved[key] = copied
		if createdID != "" {
			created = append(created, createdID)
			createdFor[key] = createdID
		}
	}
	var skips nudgeImportSkips
	var imported map[nudgeMigrationKey]bool
	err = withNudgeQueueState(q.cityPath, "import_migrated", func(state *nudgeQueueState) error {
		importing, lockedSkips := planNudgeImport(state, items)
		imported = make(map[nudgeMigrationKey]bool, len(importing))
		for _, item := range importing {
			key := nudgeMigrationKeyOf(item)
			copied, ok := moved[key]
			if !ok {
				return fmt.Errorf("moved nudge %q became importable while the moved shadows were saved; retry the batch", item.ID)
			}
			imported[key] = true
			if err := dropRetiredDebtsOnShadow(state, copied, time.Now()); err != nil {
				return err
			}
			state.Pending = append(state.Pending, copied)
		}
		skips = lockedSkips
		sortQueuedNudges(state)
		return nil
	})
	if err != nil {
		return nudgeImportSkips{}, rollbackNudgeShadows(store, created, err)
	}
	var unused []string
	for key, beadID := range createdFor {
		if !imported[key] {
			unused = append(unused, beadID)
		}
	}
	sort.Strings(unused)
	if rbErr := rollbackNudgeShadows(store, unused, nil); rbErr != nil && nudgeWarningWriter != nil {
		fmt.Fprintf(nudgeWarningWriter, "gc nudge: warning: rolling back the shadows of moved nudges the queue skipped: %v\n", rbErr) //nolint:errcheck
	}
	return skips, nil
}

// planNudgeImport splits the moved items into those state would import and
// the skips it reports for the rest.
func planNudgeImport(state *nudgeQueueState, items []nudgequeue.Item) ([]nudgequeue.Item, nudgeImportSkips) {
	var skips nudgeImportSkips
	var importing []nudgequeue.Item
	importingIDs := map[string]bool{}
	live, dead := heldNudgeKeys(state)
	for _, item := range items {
		key := nudgeMigrationKeyOf(item)
		switch {
		case live[key]:
			continue
		case dead[key]:
			skips.Dead = append(skips.Dead, item.ID)
			continue
		case queuedNudgeExists(state, item.ID) || importingIDs[item.ID]:
			skips.Collided = append(skips.Collided, item.ID)
			continue
		}
		importingIDs[item.ID] = true
		importing = append(importing, item)
	}
	return importing, skips
}

// heldNudgeKeys returns the keys of the items state holds live, pending or in
// flight, and of those it holds as dead letters.
func heldNudgeKeys(state *nudgeQueueState) (live, dead map[nudgeMigrationKey]bool) {
	live = make(map[nudgeMigrationKey]bool, len(state.Pending)+len(state.InFlight))
	for _, bucket := range [][]queuedNudge{state.Pending, state.InFlight} {
		for _, item := range bucket {
			live[nudgeMigrationKeyOf(item)] = true
		}
	}
	dead = make(map[nudgeMigrationKey]bool, len(state.Dead))
	for _, item := range state.Dead {
		dead[nudgeMigrationKeyOf(item)] = true
	}
	return live, dead
}

// shadowMovedNudge is the copy of item importMigrated queues: nudgeItemForMove,
// with BeadID naming the open shadow Save finds or creates for it. A Save that
// names no shadow fails, since the copy would point at a shadow that does not
// exist. It also returns the shadow's id when Save created it, for the
// rollback.
func shadowMovedNudge(store beads.NudgesStore, item nudgequeue.Item) (nudgequeue.Item, string, error) {
	moved := nudgeItemForMove(item)
	beadID, created, err := ensureQueuedNudgeBead(store, moved)
	if err != nil {
		return moved, "", fmt.Errorf("saving the shadow of moved nudge %q: %w", item.ID, err)
	}
	if beadID == "" {
		return moved, "", fmt.Errorf("saving the shadow of moved nudge %q: no nudges store is open", item.ID)
	}
	moved.BeadID = beadID
	if !created {
		return moved, "", nil
	}
	return moved, beadID, nil
}

// rollbackNudgeShadows closes the shadows created for a queue write that failed
// with err, through the front door's rollback as Enqueue closes its own, and
// joins any rollback failure onto err so a leaked open shadow is diagnosable.
//
// Like Enqueue's rollback, it runs after the lock is released, which leaves a
// window open (ga-w68ksk): an Enqueue of the same nudge id in between reuses
// the open shadow and commits an item whose BeadID names the shadow this then
// closes.
func rollbackNudgeShadows(store beads.NudgesStore, beadIDs []string, err error) error {
	for _, beadID := range beadIDs {
		if rbErr := nudgeFrontDoor(store).RollbackEnqueue(beadID); rbErr != nil {
			err = errors.Join(err, fmt.Errorf("rollback nudge bead %q: %w", beadID, rbErr))
		}
	}
	return err
}
