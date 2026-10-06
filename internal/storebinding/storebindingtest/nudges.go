package storebindingtest

// Bare Nudges class conformance: the merged durable queue and its shadow
// projection, over the closed storebinding.NudgeQueue and NudgeShadows
// contracts only.
//
// The assertions that carry the most weight are the claim fence, the partial
// result, and the permanent classification. A queue that hands a
// session-pinned nudge to the wrong generation delivers work to a session that
// no longer exists; a RecordFailure that reports the retried items alongside
// the dead-lettered ones makes the caller dead-letter work it was about to
// redeliver; and a queue that reads a fence mismatch as an ordinary failure
// puts a nudge on the retry treadmill toward a generation that is already
// gone.

import (
	"errors"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// ConformanceNudgeAgent is the single queue key the Nudges suite addresses.
// It is exported so a provider that has to pre-register delivery targets can
// name the exact key the suite will use.
const ConformanceNudgeAgent = "alpha/polly"

// NudgesSuite configures one bare Nudges class conformance run.
type NudgesSuite struct {
	// NewFrontDoors returns fresh, empty queue and shadow front doors per
	// assertion.
	NewFrontDoors func(TB) storebinding.NudgeFrontDoors
	// Capability is what the provider declares for the Nudges class.
	Capability storebinding.ClassCapability
}

// RunNudgeFrontDoorTests runs the bare Nudges class conformance suite.
func RunNudgeFrontDoorTests(r Runner, suite NudgesSuite) {
	r.Helper()
	if suite.NewFrontDoors == nil {
		r.Fatalf("storebindingtest: NudgesSuite.NewFrontDoors is required")
	}

	assertClassDeclaredAvailable(r, "Nudges", suite.Capability)

	r.Run("EnqueueThenListShowsPending", func(r Runner) {
		queue := mustQueue(r, suite)
		now := time.Now().UTC()
		if err := queue.Enqueue(nudgeItem("n-1", "", now)); err != nil {
			r.Fatalf("Enqueue: %v", err)
		}
		pending, inFlight, dead, err := queue.ListForAgent(ConformanceNudgeAgent, now)
		if err != nil {
			r.Fatalf("ListForAgent: %v", err)
		}
		if len(pending) != 1 || pending[0].ID != "n-1" {
			r.Fatalf("pending = %v, want exactly n-1", nudgeIDs(pending))
		}
		if len(inFlight) != 0 || len(dead) != 0 {
			r.Errorf("a freshly enqueued nudge landed in-flight=%v dead=%v", nudgeIDs(inFlight), nudgeIDs(dead))
		}
	})

	r.Run("EnqueueIsIdempotentByID", func(r Runner) {
		queue := mustQueue(r, suite)
		now := time.Now().UTC()
		item := nudgeItem("n-dup", "", now)
		if err := queue.Enqueue(item); err != nil {
			r.Fatalf("Enqueue: %v", err)
		}
		if err := queue.Enqueue(item); err != nil {
			r.Fatalf("second Enqueue: %v", err)
		}
		pending, _, _, err := queue.ListForAgent(ConformanceNudgeAgent, now)
		if err != nil {
			r.Fatalf("ListForAgent: %v", err)
		}
		if len(pending) != 1 {
			r.Fatalf("pending = %v after re-enqueueing one ID, want exactly one row", nudgeIDs(pending))
		}
	})

	r.Run("ClaimDueHonorsTheSessionFence", func(r Runner) {
		queue := mustQueue(r, suite)
		now := time.Now().UTC()
		unfenced := nudgeItem("n-unfenced", "", now)
		fenced := nudgeItem("n-fenced", "session-live", now)
		stale := nudgeItem("n-stale", "session-gone", now)
		for _, item := range []nudgequeue.Item{unfenced, fenced, stale} {
			if err := queue.Enqueue(item); err != nil {
				r.Fatalf("Enqueue(%s): %v", item.ID, err)
			}
		}
		claimed, err := queue.ClaimDue(storebinding.ClaimTarget{
			QueueKeys: []string{ConformanceNudgeAgent},
			SessionID: "session-live",
		}, now)
		if err != nil {
			r.Fatalf("ClaimDue: %v", err)
		}
		ids := nudgeIDs(claimed)
		if !containsID(ids, "n-unfenced") {
			r.Errorf("ClaimDue = %v, want it to claim the unfenced nudge", ids)
		}
		if !containsID(ids, "n-fenced") {
			r.Errorf("ClaimDue = %v, want it to claim the nudge fenced to this session", ids)
		}
		if containsID(ids, "n-stale") {
			r.Fatalf("ClaimDue = %v, want it to leave the nudge fenced to a dead session; the fence is not enforced", ids)
		}
		pending, inFlight, _, err := queue.ListForAgent(ConformanceNudgeAgent, now)
		if err != nil {
			r.Fatalf("ListForAgent after claim: %v", err)
		}
		if len(inFlight) != 2 {
			r.Errorf("in-flight = %v, want the two claimed nudges", nudgeIDs(inFlight))
		}
		if len(pending) != 1 || pending[0].ID != "n-stale" {
			r.Errorf("pending = %v, want the fenced-out nudge to stay queued", nudgeIDs(pending))
		}
	})

	r.Run("ClaimDueSkipsUndeliverableItems", func(r Runner) {
		queue := mustQueue(r, suite)
		now := time.Now().UTC()
		future := nudgeItem("n-later", "", now)
		future.DeliverAfter = now.Add(time.Hour)
		if err := queue.Enqueue(future); err != nil {
			r.Fatalf("Enqueue: %v", err)
		}
		claimed, err := queue.ClaimDue(storebinding.ClaimTarget{QueueKeys: []string{ConformanceNudgeAgent}}, now)
		if err != nil {
			r.Fatalf("ClaimDue: %v", err)
		}
		if len(claimed) != 0 {
			r.Fatalf("ClaimDue = %v, want nothing before deliver_after", nudgeIDs(claimed))
		}
	})

	r.Run("AckRetiresTheClaimedNudge", func(r Runner) {
		queue := mustQueue(r, suite)
		now := time.Now().UTC()
		if err := queue.Enqueue(nudgeItem("n-ack", "", now)); err != nil {
			r.Fatalf("Enqueue: %v", err)
		}
		if _, err := queue.ClaimDue(storebinding.ClaimTarget{QueueKeys: []string{ConformanceNudgeAgent}}, now); err != nil {
			r.Fatalf("ClaimDue: %v", err)
		}
		if err := queue.Ack([]string{"n-ack"}, "delivered", "", "commit"); err != nil {
			r.Fatalf("Ack: %v", err)
		}
		pending, inFlight, dead, err := queue.ListForAgent(ConformanceNudgeAgent, now)
		if err != nil {
			r.Fatalf("ListForAgent after Ack: %v", err)
		}
		if len(pending)+len(inFlight)+len(dead) != 0 {
			r.Fatalf("an acked nudge is still queued: pending=%v in-flight=%v dead=%v",
				nudgeIDs(pending), nudgeIDs(inFlight), nudgeIDs(dead))
		}
	})

	r.Run("ReleaseClaimsReturnsWorkToPending", func(r Runner) {
		queue := mustQueue(r, suite)
		now := time.Now().UTC()
		if err := queue.Enqueue(nudgeItem("n-release", "", now)); err != nil {
			r.Fatalf("Enqueue: %v", err)
		}
		if _, err := queue.ClaimDue(storebinding.ClaimTarget{QueueKeys: []string{ConformanceNudgeAgent}}, now); err != nil {
			r.Fatalf("ClaimDue: %v", err)
		}
		if err := queue.ReleaseClaims([]string{"n-release"}); err != nil {
			r.Fatalf("ReleaseClaims: %v", err)
		}
		pending, inFlight, _, err := queue.ListForAgent(ConformanceNudgeAgent, now)
		if err != nil {
			r.Fatalf("ListForAgent after release: %v", err)
		}
		if len(pending) != 1 || len(inFlight) != 0 {
			r.Fatalf("after ReleaseClaims pending=%v in-flight=%v, want the nudge back in pending",
				nudgeIDs(pending), nudgeIDs(inFlight))
		}
	})

	r.Run("RecordFailureReportsOnlyDeadLetters", func(r Runner) {
		queue := mustQueue(r, suite)
		now := time.Now().UTC()
		failing := nudgeItem("n-retry", "", now)
		bystander := nudgeItem("n-bystander", "", now)
		for _, item := range []nudgequeue.Item{failing, bystander} {
			if err := queue.Enqueue(item); err != nil {
				r.Fatalf("Enqueue(%s): %v", item.ID, err)
			}
		}
		// The attempt ceiling is provider policy, so the assertion drives the
		// item to its dead letter instead of assuming a number: every failure
		// before the last must report NOTHING, because an item that is going
		// to be redelivered is not a dead letter.
		cause := errors.New("delivery refused")
		const ceiling = 20
		reported := []nudgequeue.Item(nil)
		for attempt := 1; attempt <= ceiling; attempt++ {
			dead, err := queue.RecordFailure([]string{"n-retry"}, cause, now)
			if err != nil {
				r.Fatalf("RecordFailure (attempt %d): %v", attempt, err)
			}
			if len(dead) == 0 {
				continue
			}
			reported = dead
			break
		}
		if len(reported) == 0 {
			r.Fatalf("%d failures never dead-lettered the nudge; it would be redelivered forever", ceiling)
		}
		ids := nudgeIDs(reported)
		if len(ids) != 1 || ids[0] != "n-retry" {
			r.Fatalf("RecordFailure reported %v, want exactly the dead-lettered nudge; anything else is delivered work thrown away", ids)
		}
		pending, _, deadBucket, err := queue.ListForAgent(ConformanceNudgeAgent, now)
		if err != nil {
			r.Fatalf("ListForAgent after RecordFailure: %v", err)
		}
		if len(deadBucket) != 1 || deadBucket[0].ID != "n-retry" {
			r.Errorf("dead bucket = %v, want exactly the failed nudge", nudgeIDs(deadBucket))
		}
		if len(pending) != 1 || pending[0].ID != "n-bystander" {
			r.Errorf("pending = %v, want the untouched nudge still queued", nudgeIDs(pending))
		}
	})

	// The permanent classification, which is delivery-protocol vocabulary
	// rather than storage behavior: a queued nudge fenced to a session
	// generation that is gone can never be delivered, so the first failure IS
	// the last one. An implementation that instead reads the cause as ordinary
	// redelivers the nudge to a dead generation for its whole backoff schedule
	// and dead-letters it anyway — a divergence no other assertion here can
	// see, because every one of them exercises the ceiling path.
	//
	// The converse half — an ordinary failure with attempts left is requeued —
	// is deliberately absent: the attempt ceiling is provider policy (a
	// one-attempt queue is conforming), so it belongs in each implementation's
	// own tests, not in the substitution corpus.
	r.Run("RecordFailureDeadLettersAFenceMismatchImmediately", func(r Runner) {
		queue := mustQueue(r, suite)
		now := time.Now().UTC()
		fenced := nudgeItem("n-fenced", "session-gone", now)
		bystander := nudgeItem("n-bystander", "", now)
		for _, item := range []nudgequeue.Item{fenced, bystander} {
			if err := queue.Enqueue(item); err != nil {
				r.Fatalf("Enqueue(%s): %v", item.ID, err)
			}
		}
		// Wrapped, because a caller reports the mismatch with its own context
		// attached; an implementation matching only the bare sentinel would
		// miss every real report.
		cause := fmt.Errorf("delivering to session-gone: %w", storebinding.ErrNudgeSessionFenceMismatch)
		reported, err := queue.RecordFailure([]string{"n-fenced"}, cause, now)
		if err != nil {
			r.Fatalf("RecordFailure: %v", err)
		}
		// Membership, not equality: whether the report also carries items it
		// should not is RecordFailureReportsOnlyDeadLetters' property, and one
		// defect should fail one named assertion.
		if ids := nudgeIDs(reported); !containsID(ids, "n-fenced") {
			r.Fatalf("the first RecordFailure on a fence mismatch reported %v, want the nudge dead-lettered at once; it is queued for a session generation that no longer exists", ids)
		}
		pending, inFlight, deadBucket, err := queue.ListForAgent(ConformanceNudgeAgent, now)
		if err != nil {
			r.Fatalf("ListForAgent after RecordFailure: %v", err)
		}
		if len(deadBucket) != 1 || deadBucket[0].ID != "n-fenced" {
			r.Errorf("dead bucket = %v, want the fence-mismatched nudge", nudgeIDs(deadBucket))
		}
		if len(inFlight) != 0 || len(pending) != 1 || pending[0].ID != "n-bystander" {
			r.Errorf("pending=%v in-flight=%v, want only the untouched nudge still queued",
				nudgeIDs(pending), nudgeIDs(inFlight))
		}
		// The report is not the whole claim: an implementation that reports the
		// dead letter and leaves the row claimable still redelivers it.
		claimed, err := queue.ClaimDue(storebinding.ClaimTarget{
			QueueKeys: []string{ConformanceNudgeAgent},
			SessionID: "session-gone",
		}, now.Add(time.Hour))
		if err != nil {
			r.Fatalf("ClaimDue after the dead letter: %v", err)
		}
		if containsID(nudgeIDs(claimed), "n-fenced") {
			r.Fatalf("ClaimDue = %v, want a fence-mismatched nudge never redelivered", nudgeIDs(claimed))
		}
	})

	r.Run("SnapshotSeesEveryBucket", func(r Runner) {
		queue := mustQueue(r, suite)
		now := time.Now().UTC()
		if err := queue.Enqueue(nudgeItem("n-a", "", now)); err != nil {
			r.Fatalf("Enqueue: %v", err)
		}
		if err := queue.Enqueue(nudgeItem("n-b", "", now)); err != nil {
			r.Fatalf("Enqueue: %v", err)
		}
		if _, err := queue.ClaimDue(storebinding.ClaimTarget{QueueKeys: []string{ConformanceNudgeAgent}}, now); err != nil {
			r.Fatalf("ClaimDue: %v", err)
		}
		state, err := queue.Snapshot()
		if err != nil {
			r.Fatalf("Snapshot: %v", err)
		}
		if len(state.InFlight) != 2 {
			r.Errorf("Snapshot in-flight = %v, want both claimed nudges", nudgeIDs(state.InFlight))
		}
	})

	r.Run("ShadowsProjectTheQueuedNudge", func(r Runner) {
		fronts := mustFrontDoors(r, suite)
		if fronts.Shadows == nil {
			r.Fatalf("the Nudges front doors carry no shadow projection")
		}
		now := time.Now().UTC()
		item := nudgeItem("n-shadow", "", now)
		if err := fronts.Queue.Enqueue(item); err != nil {
			r.Fatalf("Enqueue: %v", err)
		}
		if _, _, err := fronts.Shadows.Save(item); err != nil {
			r.Fatalf("Save: %v", err)
		}
		found, ok, err := fronts.Shadows.Find(item.ID)
		if err != nil {
			r.Fatalf("Find: %v", err)
		}
		if !ok {
			r.Fatalf("the saved shadow is not findable by its nudge ID")
		}
		if found.ID != item.ID {
			r.Errorf("shadow nudge ID = %q, want %q", found.ID, item.ID)
		}
	})
}

func mustFrontDoors(r Runner, suite NudgesSuite) storebinding.NudgeFrontDoors {
	r.Helper()
	fronts := suite.NewFrontDoors(r)
	if fronts.Queue == nil {
		r.Fatalf("the Nudges front doors carry no queue")
	}
	return fronts
}

func mustQueue(r Runner, suite NudgesSuite) storebinding.NudgeQueue {
	r.Helper()
	return mustFrontDoors(r, suite).Queue
}

func nudgeItem(id, sessionID string, now time.Time) nudgequeue.Item {
	return nudgequeue.Item{
		ID:           id,
		Agent:        ConformanceNudgeAgent,
		SessionID:    sessionID,
		Source:       "conformance",
		Message:      "check your hook",
		CreatedAt:    now,
		DeliverAfter: now,
		ExpiresAt:    now.Add(24 * time.Hour),
	}
}

func nudgeIDs(items []nudgequeue.Item) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}
