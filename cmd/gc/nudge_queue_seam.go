package main

import (
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// nudgeQueueHandle is the city's nudge queue as one invocation sees it.
type nudgeQueueHandle struct {
	// Queue is the queue's contract surface.
	Queue storebinding.NudgeQueue
	// Location tells an operator where a queued item lives.
	Location string
	// Skips persists the dispatch tick's skip counters, an operator
	// diagnostic outside the queue contract.
	Skips nudgeDispatchSkipRecorder
	// Repin moves in-flight claims onto a new delivery key without a
	// maintenance pass or a store open (rekeyQueuedNudgesAfterFailedBookkeeping).
	Repin nudgeClaimRepinner
}

// nudgeDispatchSkipRecorder persists and reads the dispatch skip counters.
type nudgeDispatchSkipRecorder interface {
	RecordDispatchSkips(map[string]int64) error
	DispatchSkips() (map[string]int64, error)
}

// nudgeClaimRepinner pins in-flight claims to a delivery key, all or nothing.
type nudgeClaimRepinner interface {
	RepinClaims(ids []string, batch string) error
}

// openNudgeQueue is the only constructor of the city's nudge queue in cmd/gc.
// store is the nudges-class store the caller already holds. With the zero
// value, Enqueue, RecordFailure and the verbs that run a maintenance pass open
// their own, as the facades always did; Rollback and WithdrawQueuedWaitNudges
// do not, and reach shadows only through a caller-passed store, so with the
// zero value they retire the queue item and leave its shadow open.
func openNudgeQueue(cityPath string, store beads.NudgesStore) nudgeQueueHandle {
	q := &fileNudgeQueue{cityPath: cityPath, store: store, clk: clock.Real{}}
	return nudgeQueueHandle{
		Queue:    q,
		Location: q.location(),
		Skips:    q,
		Repin:    q,
	}
}

// nudgeShadows is the shadow projection over store. It takes no city path, so
// the wait helpers that hold only a store reach it.
func nudgeShadows(store beads.NudgesStore) storebinding.NudgeShadows {
	return nudgeFrontDoor(store)
}

// claimTarget is the contract's view of t: its queue keys, its fence, and a
// census of cityPath's sessions, taken at most once, that lets it adopt a
// superseded session's nudges.
func (t nudgeTarget) claimTarget(cityPath string) storebinding.ClaimTarget {
	var once sync.Once
	var ids map[string]struct{}
	return storebinding.ClaimTarget{
		QueueKeys:         t.queueKeys(),
		SessionID:         t.sessionID,
		ContinuationEpoch: t.continuationEpoch,
		LiveFenceSessions: func() (map[string]struct{}, bool) {
			once.Do(func() { ids = liveNudgeFenceSessionIDsForCity(cityPath) })
			return ids, ids != nil
		},
	}
}
