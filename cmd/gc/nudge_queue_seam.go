package main

import (
	"sync"
	"time"

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
	// Probe answers whether a claim would claim anything, from a lock-free
	// snapshot (the poll loop's due-work gate, nudgePollGate).
	Probe nudgeClaimProber
}

// nudgeClaimProber reports, without the queue lock and without claiming,
// whether ClaimBatch for ct at now would claim anything.
type nudgeClaimProber interface {
	WouldClaimBatch(ct storebinding.ClaimTarget, now time.Time) (bool, error)
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
// value, Enqueue opens its own to save the shadow, the verbs whose commit owes
// shadow debts open their own after the queue lock to pay them
// (settleNudgeShadowDebts), and so does the shim's import (importMigrated),
// which saves the moved items' shadows. WithdrawQueuedWaitNudges does not, and
// reaches shadows only through a caller-passed store: with the zero value it
// retires the queue item and leaves its shadow open, as does Rollback of an
// item no longer queued.
//
// It builds the file queue only: the beads queue, its shadow view
// (storebinding.NewBeadsNudgeFrontDoors) and the shim that moves live work
// between the two (shimNudgeQueue) have no production caller yet. The change
// that selects the queue by mode here owes three more things (ga-w0hgvz):
//
//   - In beads mode, the wait helpers' shadow reads (nudgeShadows) must go
//     through that view: a moved item's legacy gc:nudge shadow stays open, so
//     a direct read sees the item still queued and never finalizes its wait.
//     The view's Save confirms a record in any state, even one retired
//     migrated; Find is the state-aware read.
//   - A shim pass must run in the configured mode's direction from the site
//     that reads the mode.
//   - file → beads must reinstate a record retired "migrated"
//     (ReinstateMigrated, DES-NQ D12), or a quick flip back strands the
//     batch in the file queue (the shim header).
//   - The shim reads the beads queue through ListLive, which only the
//     concrete queue has, so the caller type-asserts it: add ListLive to the
//     contract, or have NewBeadsNudgeFrontDoors return a queue the shim takes
//     as it is.
func openNudgeQueue(cityPath string, store beads.NudgesStore) nudgeQueueHandle {
	q := &fileNudgeQueue{cityPath: cityPath, store: store, clk: clock.Real{}}
	return nudgeQueueHandle{
		Queue:    q,
		Location: q.location(),
		Skips:    q,
		Repin:    q,
		Probe:    q,
	}
}

// nudgeShadows is the shadow projection over store. It takes no city path, so
// the wait helpers that hold only a store reach it.
func nudgeShadows(store beads.NudgesStore) storebinding.NudgeShadows {
	return nudgeFrontDoor(store)
}

// claimTarget is the contract's view of t: its queue keys, its fence, and a
// census of cityPath's sessions, taken at most once, that lets it adopt a
// superseded session's nudges. A target that carries a census of its own
// (fenceCensus, the dispatcher's skip-only snapshot census) answers with it
// instead.
func (t nudgeTarget) claimTarget(cityPath string) storebinding.ClaimTarget {
	if t.fenceCensus != nil {
		return storebinding.ClaimTarget{
			QueueKeys:         t.queueKeys(),
			SessionID:         t.sessionID,
			ContinuationEpoch: t.continuationEpoch,
			LiveFenceSessions: t.fenceCensus,
		}
	}
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
