package storebindingtest

// Deliberately non-conforming implementations.
//
// A conformance suite that has never been shown to reject anything is a suite
// nobody can trust. Each fake below wraps the conforming reference and injects
// exactly ONE defect drawn from the categories the contract has to police —
// transactions, ordering, claims, partial results, capability loss, and
// double close. brokenfakes_test.go drives the matching suite with a recording
// runner and asserts that the named assertion, and no unrelated one, fails.
//
// They are exported because a provider overlay wants them too: the cheapest
// way to find out whether a new provider's test wiring actually runs the
// suites is to feed the wiring a fake that must fail.

import (
	"errors"
	"sort"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// GraphDefect names one deliberate Graph-class defect.
type GraphDefect uint8

const (
	// GraphDefectNone wraps the reference without changing it. It is the
	// control: the wrapper itself must not fail the suite.
	GraphDefectNone GraphDefect = iota
	// GraphDefectNonAtomicTransaction runs the callback's writes directly and
	// keeps them when the callback fails — a store that declares transactions
	// and delivers a pass-through.
	GraphDefectNonAtomicTransaction
	// GraphDefectClaimIgnoresHolder hands the claim to whoever asks last, so
	// two workers both believe they own the bead.
	GraphDefectClaimIgnoresHolder
	// GraphDefectStaleWriteAccepted applies a conditional write whose expected
	// revision no longer matches, silently overwriting a concurrent commit.
	GraphDefectStaleWriteAccepted
	// GraphDefectNotFoundMisclassified reports an absent bead as a generic
	// failure, so callers that branch on beads.ErrNotFound treat "missing" as
	// "the store is broken".
	GraphDefectNotFoundMisclassified
	// GraphDefectReadinessIgnoresDependencies lost the dependency projection
	// but still answers reads — the capability-loss shape, where a class stays
	// "available" while quietly serving blocked work as ready.
	GraphDefectReadinessIgnoresDependencies
)

// NewBrokenGraphStore wraps base with exactly one Graph-class defect.
func NewBrokenGraphStore(base storebinding.GraphStore, defect GraphDefect) storebinding.GraphStore {
	return &brokenGraphStore{GraphStore: base, defect: defect}
}

type brokenGraphStore struct {
	storebinding.GraphStore
	defect GraphDefect
}

func (s *brokenGraphStore) Get(id string) (beads.Bead, error) {
	bead, err := s.GraphStore.Get(id)
	if err != nil && s.defect == GraphDefectNotFoundMisclassified && errors.Is(err, beads.ErrNotFound) {
		return beads.Bead{}, errors.New("graph read failed")
	}
	return bead, err
}

func (s *brokenGraphStore) Tx(message string, fn func(storebinding.GraphTx) error) error {
	if s.defect != GraphDefectNonAtomicTransaction {
		return s.GraphStore.Tx(message, fn)
	}
	if fn == nil {
		return s.GraphStore.Tx(message, nil)
	}
	// Every write lands directly on the store and nothing is undone.
	return fn(passthroughGraphTx{store: s.GraphStore})
}

func (s *brokenGraphStore) Claim(id, assignee string) (beads.Bead, bool, error) {
	if s.defect != GraphDefectClaimIgnoresHolder {
		return s.GraphStore.Claim(id, assignee)
	}
	bead, ok, err := s.GraphStore.Claim(id, assignee)
	if err != nil {
		return bead, ok, err
	}
	if ok {
		return bead, true, nil
	}
	// The contended path: report a win the store never granted.
	current, getErr := s.GraphStore.Get(id)
	if getErr != nil {
		return beads.Bead{}, true, nil
	}
	current.Assignee = assignee
	return current, true, nil
}

func (s *brokenGraphStore) UpdateIfMatch(id string, expected int64, opts beads.UpdateOpts) error {
	if s.defect == GraphDefectStaleWriteAccepted {
		return s.Update(id, opts)
	}
	return s.GraphStore.UpdateIfMatch(id, expected, opts)
}

func (s *brokenGraphStore) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	if s.defect != GraphDefectReadinessIgnoresDependencies {
		return s.GraphStore.Ready(query...)
	}
	return s.List(beads.ListQuery{Status: "open"})
}

type passthroughGraphTx struct{ store storebinding.GraphStore }

func (tx passthroughGraphTx) Create(bead beads.Bead) (beads.Bead, error) {
	return tx.store.Create(bead)
}

func (tx passthroughGraphTx) Update(id string, opts beads.UpdateOpts) error {
	return tx.store.Update(id, opts)
}

func (tx passthroughGraphTx) SetMetadataBatch(id string, kvs map[string]string) error {
	return tx.store.SetMetadataBatch(id, kvs)
}

func (tx passthroughGraphTx) Close(id string) error { return tx.store.Close(id) }

// OrdersDefect names one deliberate Orders-class defect.
type OrdersDefect uint8

const (
	// OrdersDefectNone wraps the reference without changing it.
	OrdersDefectNone OrdersDefect = iota
	// OrdersDefectRecentRunsOldestFirst reverses recency, so a dispatcher
	// reading "the most recent run" for its cooldown clock reads the first run
	// it ever made.
	OrdersDefectRecentRunsOldestFirst
	// OrdersDefectClosedRunStaysOpen leaves the single-flight marker set after
	// a run closes, so the order never dispatches again.
	OrdersDefectClosedRunStaysOpen
)

// NewBrokenOrdersStore wraps base with exactly one Orders-class defect.
func NewBrokenOrdersStore(base storebinding.OrdersStore, defect OrdersDefect) storebinding.OrdersStore {
	return &brokenOrdersStore{OrdersStore: base, defect: defect}
}

type brokenOrdersStore struct {
	storebinding.OrdersStore
	defect OrdersDefect
}

func (s *brokenOrdersStore) RecentRuns(scoped string, limit int) ([]orders.OrderRun, error) {
	runs, err := s.OrdersStore.RecentRuns(scoped, limit)
	if err != nil || s.defect != OrdersDefectRecentRunsOldestFirst {
		return runs, err
	}
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].CreatedAt.Before(runs[j].CreatedAt) })
	return runs, nil
}

func (s *brokenOrdersStore) LatestOpenRun(scoped string) (orders.OrderRun, bool, error) {
	run, found, err := s.OrdersStore.LatestOpenRun(scoped)
	if err != nil || s.defect != OrdersDefectClosedRunStaysOpen {
		return run, found, err
	}
	if found {
		return run, true, nil
	}
	recent, err := s.OrdersStore.RecentRuns(scoped, 1)
	if err != nil || len(recent) == 0 {
		return run, false, err
	}
	return recent[0], true, nil
}

// NudgesDefect names one deliberate Nudges-class defect.
type NudgesDefect uint8

const (
	// NudgesDefectNone wraps the reference without changing it.
	NudgesDefectNone NudgesDefect = iota
	// NudgesDefectClaimIgnoresFence claims session-pinned nudges for any
	// target, delivering work to a generation that no longer exists.
	NudgesDefectClaimIgnoresFence
	// NudgesDefectRecordFailureReportsEveryItem returns the retried items
	// alongside the dead-lettered ones, so the caller dead-letters work the
	// queue is about to redeliver — the partial-result defect.
	NudgesDefectRecordFailureReportsEveryItem
)

// NewBrokenNudgeFrontDoors wraps base's queue with exactly one Nudges-class
// defect, leaving the shadow projection intact.
func NewBrokenNudgeFrontDoors(base storebinding.NudgeFrontDoors, defect NudgesDefect) storebinding.NudgeFrontDoors {
	return storebinding.NudgeFrontDoors{
		Queue:   &brokenNudgeQueue{NudgeQueue: base.Queue, defect: defect},
		Shadows: base.Shadows,
	}
}

type brokenNudgeQueue struct {
	storebinding.NudgeQueue
	defect NudgesDefect
}

func (q *brokenNudgeQueue) ClaimDue(target storebinding.ClaimTarget, now time.Time) ([]nudgequeue.Item, error) {
	if q.defect != NudgesDefectClaimIgnoresFence {
		return q.NudgeQueue.ClaimDue(target, now)
	}
	unfenced := target
	unfenced.SessionID = ""
	claimed, err := q.NudgeQueue.ClaimDue(unfenced, now)
	if err != nil {
		return nil, err
	}
	// The fence is dropped a second time: every DUE item still pending for
	// these keys is claimed regardless of the session it was pinned to. Items
	// that are not yet deliverable stay put, so this fake carries the fence
	// defect and nothing else.
	pending, _, _, err := q.ListFor(target, now)
	if err != nil {
		return claimed, err
	}
	for _, item := range pending {
		if item.DeliverAfter.After(now) {
			continue
		}
		claimed = append(claimed, item)
	}
	return claimed, nil
}

func (q *brokenNudgeQueue) RecordFailure(ids []string, cause error, now time.Time) ([]nudgequeue.Item, error) {
	dead, err := q.NudgeQueue.RecordFailure(ids, cause, now)
	if err != nil || q.defect != NudgesDefectRecordFailureReportsEveryItem {
		return dead, err
	}
	pending, inFlight, _, listErr := q.ListForAgent(ConformanceNudgeAgent, now)
	if listErr != nil {
		return dead, listErr
	}
	reported := append([]nudgequeue.Item(nil), dead...)
	reported = append(reported, pending...)
	reported = append(reported, inFlight...)
	return reported, nil
}

// MessagingDefect names one deliberate Messaging-class defect.
type MessagingDefect uint8

const (
	// MessagingDefectNone wraps the reference without changing it.
	MessagingDefectNone MessagingDefect = iota
	// MessagingDefectMissingTranscripts publishes an incomplete front-door
	// set: the class is declared available but one service never arrived.
	MessagingDefectMissingTranscripts
	// MessagingDefectInboxIgnoresRecipient shows every agent everyone's mail.
	MessagingDefectInboxIgnoresRecipient
)

// NewBrokenMessagingFrontDoors wraps base with exactly one Messaging-class
// defect.
func NewBrokenMessagingFrontDoors(base storebinding.MessagingFrontDoors, defect MessagingDefect) storebinding.MessagingFrontDoors {
	broken := base
	switch defect {
	case MessagingDefectMissingTranscripts:
		broken.Transcripts = nil
	case MessagingDefectInboxIgnoresRecipient:
		broken.Mail = &brokenMailProvider{Provider: base.Mail}
	case MessagingDefectNone:
	}
	return broken
}

type brokenMailProvider struct{ mail.Provider }

// Inbox leaks: it appends the mail addressed to a second, hard-coded agent to
// every recipient's inbox.
func (p *brokenMailProvider) Inbox(recipient string) ([]mail.Message, error) {
	own, err := p.Provider.Inbox(recipient)
	if err != nil {
		return nil, err
	}
	if recipient == leakedMailRecipient {
		return own, nil
	}
	others, err := p.Provider.Inbox(leakedMailRecipient)
	if err != nil {
		return nil, err
	}
	return append(own, others...), nil
}

// leakedMailRecipient is the second mailbox the broken provider spills into
// every other inbox. The Messaging suite addresses it by name.
const leakedMailRecipient = "carol"

// CloserDefect names one deliberate close-ownership defect.
type CloserDefect uint8

const (
	// CloserDefectNone closes exactly once and reports nothing twice.
	CloserDefectNone CloserDefect = iota
	// CloserDefectSecondCloseFails turns a redundant shutdown into an error
	// the operator has to explain.
	CloserDefectSecondCloseFails
)

// ErrAlreadyClosed is what the broken closer reports on its second close.
var ErrAlreadyClosed = errors.New("handle is already closed")

// NewBrokenCloser returns a close function carrying exactly one defect.
func NewBrokenCloser(defect CloserDefect) func() error {
	closed := false
	return func() error {
		if closed && defect == CloserDefectSecondCloseFails {
			return ErrAlreadyClosed
		}
		closed = true
		return nil
	}
}

// The fakes stay on the closed contracts they claim to implement.
var (
	_ storebinding.GraphStore  = (*brokenGraphStore)(nil)
	_ storebinding.OrdersStore = (*brokenOrdersStore)(nil)
	_ storebinding.NudgeQueue  = (*brokenNudgeQueue)(nil)
	_ mail.Provider            = (*brokenMailProvider)(nil)
)
