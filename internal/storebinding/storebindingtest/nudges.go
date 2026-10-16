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
	"context"
	"errors"
	"fmt"
	"sort"
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
	// Policy declares the legacy behaviors the queue keeps. The zero value is
	// the contract.
	Policy NudgeQueuePolicy
}

// NudgeQueuePolicy declares the two behaviors the flock'd state.json queue
// keeps from before the Maintain contract. The zero value is the contract;
// only the legacy file queue may set either field, and the cases that cover
// them assert the declared behavior in both directions.
type NudgeQueuePolicy struct {
	// VerbsMaintain: every verb first runs the whole-queue recovery, so a
	// read past retention ages a dead letter out.
	VerbsMaintain bool
	// ForgetsRetiredIDs: an acked id enqueued again before any sweep is
	// queued again.
	ForgetsRetiredIDs bool
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

	// Nothing runs Maintain before a claim, so an item can expire while its
	// record still says live; ClaimDue's own check is all that keeps it from a
	// deliverer. The bystander, due at the same instant, shows the claim ran.
	r.Run("ClaimDueNeverClaimsAnExpiredItem", func(r Runner) {
		queue := mustQueue(r, suite)
		now := time.Now().UTC()
		expiring := nudgeItem("n-expiring", "", now)
		expiring.ExpiresAt = now.Add(time.Minute)
		bystander := nudgeItem("n-bystander", "", now)
		for _, item := range []nudgequeue.Item{expiring, bystander} {
			if err := queue.Enqueue(item); err != nil {
				r.Fatalf("Enqueue(%s): %v", item.ID, err)
			}
		}
		afterExpiry := now.Add(2 * time.Minute)
		claimed, err := queue.ClaimDue(storebinding.ClaimTarget{QueueKeys: []string{ConformanceNudgeAgent}}, afterExpiry)
		if err != nil {
			r.Fatalf("ClaimDue: %v", err)
		}
		if len(claimed) != 1 || claimed[0].ID != "n-bystander" {
			r.Fatalf("ClaimDue = %v, want only the unexpired nudge", nudgeIDs(claimed))
		}
		pending, inFlight, dead, err := queue.ListForAgent(ConformanceNudgeAgent, afterExpiry)
		if err != nil {
			r.Fatalf("ListForAgent after ClaimDue: %v", err)
		}
		if len(dead) != 1 || dead[0].ID != "n-expiring" {
			r.Fatalf("dead = %v, want the expired nudge", nudgeIDs(dead))
		}
		if dead[0].LastError != "expired" {
			r.Errorf("dead LastError = %q, want %q", dead[0].LastError, "expired")
		}
		if len(pending) != 0 || len(inFlight) != 1 || inFlight[0].ID != "n-bystander" {
			r.Errorf("pending=%v in-flight=%v, want only the bystander in flight", nudgeIDs(pending), nudgeIDs(inFlight))
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
		if err := queue.ReleaseClaims([]string{"n-release"}, time.Time{}); err != nil {
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

	// Maintain is the whole-queue recovery pass the delivery verbs no longer
	// run. The cases below are its observable effects through the contract:
	// an expired item leaves delivery for the dead bucket and, past
	// retention, leaves the queue; a dead letter ages out only through
	// Maintain; and a retired record is never served again. Retention windows
	// are provider policy, so each case runs Maintain at maintainHorizon, past
	// any window a conforming queue may choose.
	//
	// The expiry case asserts what only a RECORDED dead letter shows. A read
	// already projects an expired item as dead, so "dead after Maintain"
	// proves nothing on its own; a queue that never records the expiry keeps
	// the item visible after a pass past retention, because to that pass it
	// has only just died.
	r.Run("MaintainDeadLettersExpiredItems", func(r Runner) {
		queue := mustQueue(r, suite)
		now := time.Now().UTC()
		expiring := nudgeItem("n-expiring", "", now)
		expiring.ExpiresAt = now.Add(time.Minute)
		bystander := nudgeItem("n-bystander", "", now)
		bystander.ExpiresAt = now.Add(2 * maintainHorizon)
		for _, item := range []nudgequeue.Item{expiring, bystander} {
			if err := queue.Enqueue(item); err != nil {
				r.Fatalf("Enqueue(%s): %v", item.ID, err)
			}
		}
		afterExpiry := now.Add(2 * time.Minute)
		_, _, projected, err := queue.ListForAgent(ConformanceNudgeAgent, afterExpiry)
		if err != nil {
			r.Fatalf("ListForAgent before Maintain: %v", err)
		}
		if len(projected) != 1 || projected[0].ID != "n-expiring" {
			r.Fatalf("dead before Maintain = %v, want the expired nudge read as dead", nudgeIDs(projected))
		}
		if _, err := queue.Maintain(context.Background(), afterExpiry); err != nil {
			r.Fatalf("Maintain: %v", err)
		}
		pending, inFlight, dead, err := queue.ListForAgent(ConformanceNudgeAgent, afterExpiry)
		if err != nil {
			r.Fatalf("ListForAgent after Maintain: %v", err)
		}
		if len(dead) != 1 || dead[0].ID != "n-expiring" {
			r.Fatalf("dead = %v, want the expired nudge dead-lettered by Maintain", nudgeIDs(dead))
		}
		// How the expiry reads must not change when it is recorded: a
		// dashboard that ages dead letters by DeadAt would otherwise see the
		// same letter die twice.
		if !dead[0].DeadAt.Equal(projected[0].DeadAt) || dead[0].LastError != projected[0].LastError {
			r.Errorf("after Maintain DeadAt=%v LastError=%q, want the projected DeadAt=%v LastError=%q",
				dead[0].DeadAt, dead[0].LastError, projected[0].DeadAt, projected[0].LastError)
		}
		if len(inFlight) != 0 || len(pending) != 1 || pending[0].ID != "n-bystander" {
			r.Errorf("pending=%v in-flight=%v, want only the unexpired nudge still queued", nudgeIDs(pending), nudgeIDs(inFlight))
		}
		later := afterExpiry.Add(maintainHorizon)
		if _, err := queue.Maintain(context.Background(), later); err != nil {
			r.Fatalf("Maintain past retention: %v", err)
		}
		pending, inFlight, dead, err = queue.ListForAgent(ConformanceNudgeAgent, later)
		if err != nil {
			r.Fatalf("ListForAgent past retention: %v", err)
		}
		if ids := append(append(nudgeIDs(pending), nudgeIDs(inFlight)...), nudgeIDs(dead)...); containsID(ids, "n-expiring") {
			r.Fatalf("past retention the queue still serves %v, want the expired nudge aged out; Maintain never recorded its expiry", ids)
		}
	})

	r.Run("MaintainAgesDeadLettersOut", func(r Runner) {
		queue := mustQueue(r, suite)
		now := time.Now().UTC()
		item := nudgeItem("n-rolled-back", "", now)
		if err := queue.Enqueue(item); err != nil {
			r.Fatalf("Enqueue: %v", err)
		}
		if err := queue.Rollback(item, "conformance rollback"); err != nil {
			r.Fatalf("Rollback: %v", err)
		}
		_, _, dead, err := queue.ListForAgent(ConformanceNudgeAgent, now)
		if err != nil {
			r.Fatalf("ListForAgent: %v", err)
		}
		if len(dead) != 1 || dead[0].ID != "n-rolled-back" {
			r.Fatalf("dead = %v, want the rolled-back nudge", nudgeIDs(dead))
		}
		// Aging out is Maintain's alone: a read far past any retention still
		// serves the dead letter. A queue that declares VerbsMaintain ages it
		// out on that read instead, and must.
		later := now.Add(maintainHorizon)
		_, _, dead, err = queue.ListForAgent(ConformanceNudgeAgent, later)
		if err != nil {
			r.Fatalf("ListForAgent past retention: %v", err)
		}
		if suite.Policy.VerbsMaintain {
			if len(dead) != 0 {
				r.Fatalf("dead past retention = %v, want the read to age the dead letter out; the queue declares that its verbs maintain", nudgeIDs(dead))
			}
		} else if len(dead) != 1 || dead[0].ID != "n-rolled-back" {
			r.Fatalf("dead past retention before Maintain = %v, want a read to leave the dead letter alone", nudgeIDs(dead))
		}
		if _, err := queue.Maintain(context.Background(), later); err != nil {
			r.Fatalf("Maintain: %v", err)
		}
		pending, inFlight, dead, err := queue.ListForAgent(ConformanceNudgeAgent, later)
		if err != nil {
			r.Fatalf("ListForAgent after Maintain: %v", err)
		}
		if len(pending)+len(inFlight)+len(dead) != 0 {
			r.Fatalf("after Maintain past retention pending=%v in-flight=%v dead=%v, want the dead letter aged out",
				nudgeIDs(pending), nudgeIDs(inFlight), nudgeIDs(dead))
		}
	})

	// Whether a swept id may be enqueued again is provider policy: a queue
	// may keep a permanent tombstone so a pinned id never executes twice. What
	// every queue owes is that retiring and sweeping never brings the item
	// back, and that a later submit of the same id never yields two.
	r.Run("MaintainNeverResurrectsRetiredRecords", func(r Runner) {
		queue := mustQueue(r, suite)
		now := time.Now().UTC()
		item := nudgeItem("n-delivered", "", now)
		if err := queue.Enqueue(item); err != nil {
			r.Fatalf("Enqueue: %v", err)
		}
		if _, err := queue.ClaimDue(storebinding.ClaimTarget{QueueKeys: []string{ConformanceNudgeAgent}}, now); err != nil {
			r.Fatalf("ClaimDue: %v", err)
		}
		if err := queue.Ack([]string{"n-delivered"}, "delivered", "", "commit"); err != nil {
			r.Fatalf("Ack: %v", err)
		}
		// Until the sweep the retired record still answers for its id, which
		// is what makes a retried submit of a delivered nudge a no-op. A
		// queue that declares ForgetsRetiredIDs queues it again, and must.
		if err := queue.Enqueue(item); err != nil {
			r.Fatalf("re-Enqueue before the sweep: %v", err)
		}
		pending, _, _, err := queue.ListForAgent(ConformanceNudgeAgent, now)
		if err != nil {
			r.Fatalf("ListForAgent: %v", err)
		}
		if suite.Policy.ForgetsRetiredIDs {
			if len(pending) != 1 || pending[0].ID != "n-delivered" {
				r.Fatalf("pending = %v, want the retried submit queued again; the queue declares that it forgets retired ids", nudgeIDs(pending))
			}
			return
		}
		if len(pending) != 0 {
			r.Fatalf("pending = %v, want a retried submit of a delivered nudge to stay retired", nudgeIDs(pending))
		}
		later := now.Add(maintainHorizon)
		if _, err := queue.Maintain(context.Background(), later); err != nil {
			r.Fatalf("Maintain: %v", err)
		}
		pending, inFlight, dead, err := queue.ListForAgent(ConformanceNudgeAgent, later)
		if err != nil {
			r.Fatalf("ListForAgent after the sweep: %v", err)
		}
		if len(pending)+len(inFlight)+len(dead) != 0 {
			r.Fatalf("after the sweep pending=%v in-flight=%v dead=%v, want the retired nudge never served again",
				nudgeIDs(pending), nudgeIDs(inFlight), nudgeIDs(dead))
		}
		if err := queue.Enqueue(nudgeItem("n-delivered", "", later)); err != nil {
			r.Fatalf("Enqueue after the sweep: %v", err)
		}
		pending, inFlight, dead, err = queue.ListForAgent(ConformanceNudgeAgent, later)
		if err != nil {
			r.Fatalf("ListForAgent after resubmitting: %v", err)
		}
		if n := len(pending) + len(inFlight) + len(dead); n > 1 || len(inFlight)+len(dead) != 0 {
			r.Fatalf("after resubmitting a swept id pending=%v in-flight=%v dead=%v, want at most one pending copy",
				nudgeIDs(pending), nudgeIDs(inFlight), nudgeIDs(dead))
		}
	})

	// A delivery verb that touches an item past its deliver-by deadline
	// dead-letters it as expired before anything else: an expired nudge is
	// not acked as delivered, not released back to pending, and not charged a
	// delivery attempt that would report it as a failure.
	r.Run("TouchingAnExpiredItemDeadLettersIt", func(r Runner) {
		queue := mustQueue(r, suite)
		wall := time.Now().UTC()
		// Claimed while still deliverable, expired by the time the verbs run
		// against the wall clock.
		claimedAt := wall.Add(-90 * time.Second)
		for _, id := range []string{"n-ack", "n-release", "n-fail"} {
			item := nudgeItem(id, "", claimedAt.Add(-time.Minute))
			item.ExpiresAt = wall.Add(-time.Minute)
			if err := queue.Enqueue(item); err != nil {
				r.Fatalf("Enqueue(%s): %v", id, err)
			}
		}
		claimed, err := queue.ClaimDue(storebinding.ClaimTarget{QueueKeys: []string{ConformanceNudgeAgent}}, claimedAt)
		if err != nil {
			r.Fatalf("ClaimDue: %v", err)
		}
		// A queue whose verbs maintain runs its recovery against the wall
		// clock inside every Enqueue, so the items already expired there and
		// nothing is left to claim. The verbs below must still leave them
		// dead as expired.
		if len(claimed) != 3 && !suite.Policy.VerbsMaintain {
			r.Fatalf("ClaimDue = %v, want all three nudges claimed before their deadline", nudgeIDs(claimed))
		}
		if err := queue.Ack([]string{"n-ack"}, "delivered", "", "commit"); err != nil {
			r.Fatalf("Ack: %v", err)
		}
		if err := queue.ReleaseClaims([]string{"n-release"}, time.Time{}); err != nil {
			r.Fatalf("ReleaseClaims: %v", err)
		}
		reported, err := queue.RecordFailure([]string{"n-fail"}, errors.New("delivery refused"), wall)
		if err != nil {
			r.Fatalf("RecordFailure: %v", err)
		}
		if len(reported) != 0 {
			r.Errorf("RecordFailure on an expired nudge reported %v, want nothing: it expired, it did not fail", nudgeIDs(reported))
		}
		pending, inFlight, dead, err := queue.ListForAgent(ConformanceNudgeAgent, wall)
		if err != nil {
			r.Fatalf("ListForAgent: %v", err)
		}
		if len(pending)+len(inFlight) != 0 {
			r.Errorf("pending=%v in-flight=%v, want every expired nudge out of delivery", nudgeIDs(pending), nudgeIDs(inFlight))
		}
		got := map[string]nudgequeue.Item{}
		for _, item := range dead {
			got[item.ID] = item
		}
		for _, id := range []string{"n-ack", "n-release", "n-fail"} {
			item, ok := got[id]
			if !ok {
				r.Errorf("dead = %v, want %s dead-lettered as expired", nudgeIDs(dead), id)
				continue
			}
			if item.LastError != "expired" || item.Attempts != 0 {
				r.Errorf("%s dead with LastError=%q Attempts=%d, want expired with no attempt charged", id, item.LastError, item.Attempts)
			}
		}
	})

	// The empty queue key addresses nobody: an empty key in a target's key set
	// or as an agent name must not match an item whose agent is unset.
	r.Run("EmptyQueueKeyMatchesNothing", func(r Runner) {
		queue := mustQueue(r, suite)
		now := time.Now().UTC()
		anonymous := nudgeItem("n-anonymous", "", now)
		anonymous.Agent = ""
		for _, item := range []nudgequeue.Item{anonymous, nudgeItem("n-addressed", "", now)} {
			if err := queue.Enqueue(item); err != nil {
				r.Fatalf("Enqueue(%s): %v", item.ID, err)
			}
		}
		pending, inFlight, dead, err := queue.ListForAgent("", now)
		if err != nil {
			r.Fatalf("ListForAgent(\"\"): %v", err)
		}
		if n := len(pending) + len(inFlight) + len(dead); n != 0 {
			r.Errorf("ListForAgent(\"\") = %v, want nothing", nudgeIDs(append(append(pending, inFlight...), dead...)))
		}
		empty := storebinding.ClaimTarget{QueueKeys: []string{""}}
		pending, _, _, err = queue.ListFor(empty, now)
		if err != nil {
			r.Fatalf("ListFor(empty key): %v", err)
		}
		if len(pending) != 0 {
			r.Errorf("ListFor(empty key) pending = %v, want nothing", nudgeIDs(pending))
		}
		claimed, err := queue.ClaimDue(empty, now)
		if err != nil {
			r.Fatalf("ClaimDue(empty key): %v", err)
		}
		if len(claimed) != 0 {
			r.Fatalf("ClaimDue(empty key) = %v, want nothing claimed", nudgeIDs(claimed))
		}
	})

	runNudgeLegBTests(r, suite)
	runNudgeBatchTests(r, suite)
	runNudgeWholeQueueReadTests(r, suite)

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

// maintainHorizon is how far past an item's life the Maintain cases run. It is
// longer than any dead-letter or retired-record retention a conforming queue
// may choose.
const maintainHorizon = 30 * 24 * time.Hour

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

// runNudgeLegBTests covers session-replacement adoption: a target may claim a
// nudge pinned to a session that no longer holds a seat, and only when the
// target's lazy census says so.
func runNudgeLegBTests(r Runner, suite NudgesSuite) {
	r.Helper()
	r.Run("ClaimDueAdoptsANudgeFencedToASupersededSession", func(r Runner) {
		now := time.Now().UTC()
		census := func(ids ...string) func() (map[string]struct{}, bool) {
			return func() (map[string]struct{}, bool) {
				live := make(map[string]struct{}, len(ids))
				for _, id := range ids {
					live[id] = struct{}{}
				}
				return live, true
			}
		}
		cases := []struct {
			name  string
			live  func() (map[string]struct{}, bool)
			adopt bool
		}{
			{name: "a census naming only the successor", live: census("session-new"), adopt: true},
			{name: "a census in which the old session still holds its seat", live: census("session-new", "session-old")},
			{name: "a census that does not name the successor", live: census("session-other")},
			{name: "an unavailable census", live: func() (map[string]struct{}, bool) { return nil, false }},
			{name: "no census at all"},
		}
		for _, testCase := range cases {
			queue := mustQueue(r, suite)
			if err := queue.Enqueue(nudgeItem("n-orphaned", "session-old", now)); err != nil {
				r.Fatalf("%s: Enqueue: %v", testCase.name, err)
			}
			claimed, err := queue.ClaimDue(storebinding.ClaimTarget{
				QueueKeys:         []string{ConformanceNudgeAgent},
				SessionID:         "session-new",
				LiveFenceSessions: testCase.live,
			}, now)
			if err != nil {
				r.Fatalf("%s: ClaimDue: %v", testCase.name, err)
			}
			adopted := containsID(nudgeIDs(claimed), "n-orphaned")
			switch {
			case testCase.adopt && !adopted:
				r.Errorf("%s: ClaimDue = %v, want the successor to adopt the superseded session's nudge", testCase.name, nudgeIDs(claimed))
			case !testCase.adopt && adopted:
				r.Errorf("%s: ClaimDue = %v, want the nudge left fenced to its session", testCase.name, nudgeIDs(claimed))
			}
		}

		// The census is a session listing, so a claim asks it at most once
		// however many candidates are pinned elsewhere, and never when no due
		// candidate is pinned to another session.
		counted := func(calls *int) func() (map[string]struct{}, bool) {
			return func() (map[string]struct{}, bool) {
				*calls++
				return map[string]struct{}{"session-new": {}}, true
			}
		}
		queue := mustQueue(r, suite)
		for _, item := range []nudgequeue.Item{nudgeItem("n-old-1", "session-old", now), nudgeItem("n-old-2", "session-gone", now)} {
			if err := queue.Enqueue(item); err != nil {
				r.Fatalf("Enqueue(%s): %v", item.ID, err)
			}
		}
		calls := 0
		claimed, err := queue.ClaimDue(storebinding.ClaimTarget{
			QueueKeys:         []string{ConformanceNudgeAgent},
			SessionID:         "session-new",
			LiveFenceSessions: counted(&calls),
		}, now)
		if err != nil {
			r.Fatalf("ClaimDue with a counting census: %v", err)
		}
		if len(claimed) != 2 {
			r.Errorf("ClaimDue = %v, want both orphaned nudges adopted", nudgeIDs(claimed))
		}
		if calls != 1 {
			r.Errorf("one claim asked the census %d times, want exactly once", calls)
		}

		quiet := mustQueue(r, suite)
		later := nudgeItem("n-not-due", "session-old", now)
		later.DeliverAfter = now.Add(time.Hour)
		for _, item := range []nudgequeue.Item{nudgeItem("n-unpinned", "", now), nudgeItem("n-mine", "session-new", now), later} {
			if err := quiet.Enqueue(item); err != nil {
				r.Fatalf("Enqueue(%s): %v", item.ID, err)
			}
		}
		calls = 0
		if _, err := quiet.ClaimDue(storebinding.ClaimTarget{
			QueueKeys:         []string{ConformanceNudgeAgent},
			SessionID:         "session-new",
			LiveFenceSessions: counted(&calls),
		}, now); err != nil {
			r.Fatalf("ClaimDue with no foreign-pinned candidate: %v", err)
		}
		if calls != 0 {
			r.Errorf("a claim with no due candidate pinned to another session asked the census %d times, want never", calls)
		}
	})
}

// runNudgeBatchTests covers pinned delivery batches. A batch is delivered
// under one key, and a redelivery after an unknown outcome must be the same
// delivery under the same key: a strict subset would let the runtime commit
// the partial text and later replay that receipt for the missing items,
// acking them unseen.
func runNudgeBatchTests(r Runner, suite NudgesSuite) {
	r.Helper()
	target := storebinding.ClaimTarget{QueueKeys: []string{ConformanceNudgeAgent}}

	r.Run("ClaimBatchPinsANewBatch", func(r Runner) {
		queue := mustQueue(r, suite)
		now := time.Now().UTC()
		enqueueNudges(r, queue, now, "n-a", "n-b", "n-c")
		claimed, err := queue.ClaimBatch(target, now)
		if err != nil {
			r.Fatalf("ClaimBatch: %v", err)
		}
		key := assertOneBatch(r, "ClaimBatch", claimed, "n-a", "n-b", "n-c")
		for _, item := range claimed {
			if item.ClaimedAt.IsZero() || !item.LeaseUntil.After(now) {
				r.Errorf("claimed %s ClaimedAt=%v LeaseUntil=%v, want a claim and a live lease", item.ID, item.ClaimedAt, item.LeaseUntil)
			}
		}
		_, inFlight, _, err := queue.ListFor(target, now)
		if err != nil {
			r.Fatalf("ListFor: %v", err)
		}
		if len(inFlight) != 3 {
			r.Fatalf("in-flight = %v, want the three claimed nudges", nudgeIDs(inFlight))
		}
		for _, item := range inFlight {
			if item.DeliveryBatch != key {
				r.Errorf("in-flight %s DeliveryBatch = %q, want the claimed batch %q", item.ID, item.DeliveryBatch, key)
			}
		}
	})

	r.Run("ClaimBatchResumesAPinnedBatchWhole", func(r Runner) {
		queue := mustQueue(r, suite)
		now := time.Now().UTC()
		enqueueNudges(r, queue, now, "n-a", "n-b")
		first, err := queue.ClaimBatch(target, now)
		if err != nil {
			r.Fatalf("ClaimBatch: %v", err)
		}
		key := assertOneBatch(r, "first ClaimBatch", first, "n-a", "n-b")
		if err := queue.ReleaseClaims([]string{"n-a", "n-b"}, time.Time{}); err != nil {
			r.Fatalf("ReleaseClaims: %v", err)
		}
		enqueueNudges(r, queue, now, "n-c")
		resumed, err := queue.ClaimBatch(target, now)
		if err != nil {
			r.Fatalf("ClaimBatch after the release: %v", err)
		}
		if got := assertOneBatch(r, "resumed ClaimBatch", resumed, "n-a", "n-b"); got != key {
			r.Fatalf("resumed batch key = %q, want the pinned %q; a redelivery must be the same delivery", got, key)
		}
		if err := queue.Ack([]string{"n-a", "n-b"}, "delivered", "", "commit"); err != nil {
			r.Fatalf("Ack: %v", err)
		}
		next, err := queue.ClaimBatch(target, now)
		if err != nil {
			r.Fatalf("ClaimBatch after the ack: %v", err)
		}
		if got := assertOneBatch(r, "ClaimBatch after the ack", next, "n-c"); got == key {
			r.Fatalf("a new batch reused the acked batch's key %q", key)
		}
	})

	r.Run("ClaimBatchNeverClaimsAStrictSubset", func(r Runner) {
		queue := mustQueue(r, suite)
		now := time.Now().UTC()
		enqueueNudges(r, queue, now, "n-a", "n-b")
		first, err := queue.ClaimBatch(target, now)
		if err != nil {
			r.Fatalf("ClaimBatch: %v", err)
		}
		key := assertOneBatch(r, "first ClaimBatch", first, "n-a", "n-b")
		if err := queue.ReleaseClaims([]string{"n-a"}, time.Time{}); err != nil {
			r.Fatalf("ReleaseClaims(n-a): %v", err)
		}
		partial, err := queue.ClaimBatch(target, now)
		if err != nil {
			r.Fatalf("ClaimBatch with one member in flight: %v", err)
		}
		if len(partial) != 0 {
			r.Fatalf("ClaimBatch = %v with n-b still in flight, want nothing: a pinned batch is never claimed in part", nudgeIDs(partial))
		}
		if err := queue.ReleaseClaims([]string{"n-b"}, time.Time{}); err != nil {
			r.Fatalf("ReleaseClaims(n-b): %v", err)
		}
		whole, err := queue.ClaimBatch(target, now)
		if err != nil {
			r.Fatalf("ClaimBatch after both releases: %v", err)
		}
		if got := assertOneBatch(r, "ClaimBatch after both releases", whole, "n-a", "n-b"); got != key {
			r.Fatalf("batch key = %q, want the pinned %q", got, key)
		}
	})

	r.Run("ClaimBatchWaitsOnASplitBatch", func(r Runner) {
		queue := mustQueue(r, suite)
		now := time.Now().UTC()
		const otherKey = "alpha/former"
		both := storebinding.ClaimTarget{QueueKeys: []string{ConformanceNudgeAgent, otherKey}}
		away := nudgeItem("n-away", "", now)
		away.Agent = otherKey
		enqueueNudges(r, queue, now, "n-here")
		if err := queue.Enqueue(away); err != nil {
			r.Fatalf("Enqueue(n-away): %v", err)
		}
		first, err := queue.ClaimBatch(both, now)
		if err != nil {
			r.Fatalf("ClaimBatch: %v", err)
		}
		key := assertOneBatch(r, "first ClaimBatch", first, "n-away", "n-here")
		if err := queue.ReleaseClaims([]string{"n-here", "n-away"}, time.Time{}); err != nil {
			r.Fatalf("ReleaseClaims: %v", err)
		}
		split, err := queue.ClaimBatch(target, now)
		if err != nil {
			r.Fatalf("ClaimBatch for a target missing one member's key: %v", err)
		}
		if len(split) != 0 {
			r.Fatalf("ClaimBatch = %v for a target that lacks %q, want nothing: n-away would be left out of its own batch", nudgeIDs(split), otherKey)
		}
		whole, err := queue.ClaimBatch(both, now)
		if err != nil {
			r.Fatalf("ClaimBatch for a target with every key: %v", err)
		}
		if got := assertOneBatch(r, "ClaimBatch with every key", whole, "n-away", "n-here"); got != key {
			r.Fatalf("batch key = %q, want the pinned %q", got, key)
		}
	})

	r.Run("ClaimBatchHonorsTheMemberFence", func(r Runner) {
		queue := mustQueue(r, suite)
		now := time.Now().UTC()
		for _, item := range []nudgequeue.Item{nudgeItem("n-open", "", now), nudgeItem("n-fenced", "session-1", now)} {
			if err := queue.Enqueue(item); err != nil {
				r.Fatalf("Enqueue(%s): %v", item.ID, err)
			}
		}
		first, err := queue.ClaimBatch(storebinding.ClaimTarget{QueueKeys: []string{ConformanceNudgeAgent}, SessionID: "session-1"}, now)
		if err != nil {
			r.Fatalf("ClaimBatch by session-1: %v", err)
		}
		key := assertOneBatch(r, "ClaimBatch by session-1", first, "n-fenced", "n-open")
		if err := queue.ReleaseClaims([]string{"n-open", "n-fenced"}, time.Time{}); err != nil {
			r.Fatalf("ReleaseClaims: %v", err)
		}
		bothSeated := func() (map[string]struct{}, bool) {
			return map[string]struct{}{"session-1": {}, "session-2": {}}, true
		}
		sibling, err := queue.ClaimBatch(storebinding.ClaimTarget{
			QueueKeys:         []string{ConformanceNudgeAgent},
			SessionID:         "session-2",
			LiveFenceSessions: bothSeated,
		}, now)
		if err != nil {
			r.Fatalf("ClaimBatch by sibling session-2: %v", err)
		}
		if len(sibling) != 0 {
			r.Fatalf("sibling session-2 claimed %v while session-1 still holds its seat, want nothing: n-fenced belongs to session-1 and its batch is claimed whole or not at all", nudgeIDs(sibling))
		}
		owner, err := queue.ClaimBatch(storebinding.ClaimTarget{QueueKeys: []string{ConformanceNudgeAgent}, SessionID: "session-1"}, now)
		if err != nil {
			r.Fatalf("ClaimBatch by session-1 again: %v", err)
		}
		if got := assertOneBatch(r, "ClaimBatch by session-1 again", owner, "n-fenced", "n-open"); got != key {
			r.Fatalf("batch key = %q, want the pinned %q", got, key)
		}
	})

	// A batch whose every member is backed off is not claimable, so a claim
	// never asks the census whether it splits across sessions.
	r.Run("ClaimBatchSkipsTheCensusForABackedOffBatch", func(r Runner) {
		queue := mustQueue(r, suite)
		now := time.Now().UTC()
		if err := queue.Enqueue(nudgeItem("n-old", "session-old", now)); err != nil {
			r.Fatalf("Enqueue: %v", err)
		}
		first, err := queue.ClaimBatch(storebinding.ClaimTarget{QueueKeys: []string{ConformanceNudgeAgent}, SessionID: "session-old"}, now)
		if err != nil {
			r.Fatalf("ClaimBatch by session-old: %v", err)
		}
		key := assertOneBatch(r, "ClaimBatch by session-old", first, "n-old")
		retry := now.Add(time.Hour)
		if err := queue.ReleaseClaims([]string{"n-old"}, retry); err != nil {
			r.Fatalf("ReleaseClaims: %v", err)
		}
		calls := 0
		successor := storebinding.ClaimTarget{
			QueueKeys: []string{ConformanceNudgeAgent},
			SessionID: "session-new",
			LiveFenceSessions: func() (map[string]struct{}, bool) {
				calls++
				return map[string]struct{}{"session-new": {}}, true
			},
		}
		early, err := queue.ClaimBatch(successor, now)
		if err != nil {
			r.Fatalf("ClaimBatch before the batch is due: %v", err)
		}
		if len(early) != 0 {
			r.Fatalf("ClaimBatch before the batch is due = %v, want nothing", nudgeIDs(early))
		}
		if calls != 0 {
			r.Errorf("a ClaimBatch with no due candidate asked the census %d times, want never", calls)
		}
		resumed, err := queue.ClaimBatch(successor, retry.Add(time.Second))
		if err != nil {
			r.Fatalf("ClaimBatch once the batch is due: %v", err)
		}
		if got := assertOneBatch(r, "ClaimBatch once the batch is due", resumed, "n-old"); got != key {
			r.Fatalf("adopted batch key = %q, want the pinned %q", got, key)
		}
		if calls != 1 {
			r.Errorf("one ClaimBatch with a due batch pinned elsewhere asked the census %d times, want exactly once", calls)
		}
	})

	r.Run("ClaimBatchAdoptsASupersededSessionsBatch", func(r Runner) {
		queue := mustQueue(r, suite)
		now := time.Now().UTC()
		for _, item := range []nudgequeue.Item{nudgeItem("n-1", "session-old", now), nudgeItem("n-2", "session-old", now)} {
			if err := queue.Enqueue(item); err != nil {
				r.Fatalf("Enqueue(%s): %v", item.ID, err)
			}
		}
		first, err := queue.ClaimBatch(storebinding.ClaimTarget{QueueKeys: []string{ConformanceNudgeAgent}, SessionID: "session-old"}, now)
		if err != nil {
			r.Fatalf("ClaimBatch by session-old: %v", err)
		}
		key := assertOneBatch(r, "ClaimBatch by session-old", first, "n-1", "n-2")
		if err := queue.ReleaseClaims([]string{"n-1", "n-2"}, time.Time{}); err != nil {
			r.Fatalf("ReleaseClaims: %v", err)
		}
		calls := 0
		successorOnly := func() (map[string]struct{}, bool) {
			calls++
			return map[string]struct{}{"session-new": {}}, true
		}
		resumed, err := queue.ClaimBatch(storebinding.ClaimTarget{
			QueueKeys:         []string{ConformanceNudgeAgent},
			SessionID:         "session-new",
			LiveFenceSessions: successorOnly,
		}, now)
		if err != nil {
			r.Fatalf("ClaimBatch by session-new: %v", err)
		}
		if got := assertOneBatch(r, "ClaimBatch by session-new", resumed, "n-1", "n-2"); got != key {
			r.Fatalf("adopted batch key = %q, want the pinned %q; a redelivery must be the same delivery", got, key)
		}
		if calls != 1 {
			r.Errorf("one ClaimBatch asked the census %d times, want exactly once", calls)
		}
	})

	r.Run("RecordFailureRejectionUnpins", func(r Runner) {
		queue := mustQueue(r, suite)
		now := time.Now().UTC()
		enqueueNudges(r, queue, now, "n-a", "n-b")
		first, err := queue.ClaimBatch(target, now)
		if err != nil {
			r.Fatalf("ClaimBatch: %v", err)
		}
		key := assertOneBatch(r, "first ClaimBatch", first, "n-a", "n-b")

		// An ordinary failure leaves the outcome unknown, so the batch keeps
		// its key and redelivers as the same delivery.
		retry := now.Add(time.Hour)
		if _, err := queue.RecordFailure([]string{"n-a", "n-b"}, errors.New("delivery timed out"), now); err != nil {
			r.Fatalf("RecordFailure: %v", err)
		}
		again, err := queue.ClaimBatch(target, retry)
		if err != nil {
			r.Fatalf("ClaimBatch after an ordinary failure: %v", err)
		}
		if got := assertOneBatch(r, "ClaimBatch after an ordinary failure", again, "n-a", "n-b"); got != key {
			r.Fatalf("after an ordinary failure the batch key = %q, want the pinned %q", got, key)
		}

		// A definitive rejection delivered nothing, so the pin is cleared and
		// the items retry with backoff under a new key.
		cause := fmt.Errorf("delivering batch %s: %w", key, storebinding.ErrNudgeDeliveryRejected)
		reported, err := queue.RecordFailure([]string{"n-a", "n-b"}, cause, retry)
		if err != nil {
			r.Fatalf("RecordFailure(rejected): %v", err)
		}
		if len(reported) != 0 {
			r.Fatalf("RecordFailure(rejected) reported %v as dead letters, want the items requeued", nudgeIDs(reported))
		}
		pending, inFlight, _, err := queue.ListFor(target, retry)
		if err != nil {
			r.Fatalf("ListFor after the rejection: %v", err)
		}
		if len(pending) != 2 || len(inFlight) != 0 {
			r.Fatalf("after the rejection pending=%v in-flight=%v, want both requeued", nudgeIDs(pending), nudgeIDs(inFlight))
		}
		for _, item := range pending {
			if item.DeliveryBatch != "" {
				r.Errorf("rejected %s still carries DeliveryBatch %q, want it unpinned", item.ID, item.DeliveryBatch)
			}
			if !item.DeliverAfter.After(retry) {
				r.Errorf("rejected %s DeliverAfter = %v, want a backoff past %v", item.ID, item.DeliverAfter, retry)
			}
		}
		early, err := queue.ClaimBatch(target, retry)
		if err != nil {
			r.Fatalf("ClaimBatch inside the backoff: %v", err)
		}
		if len(early) != 0 {
			r.Fatalf("ClaimBatch inside the backoff = %v, want nothing", nudgeIDs(early))
		}
		repinned, err := queue.ClaimBatch(target, retry.Add(time.Hour))
		if err != nil {
			r.Fatalf("ClaimBatch after the backoff: %v", err)
		}
		if got := assertOneBatch(r, "ClaimBatch after the rejection", repinned, "n-a", "n-b"); got == key {
			r.Fatalf("after a rejection the batch kept key %q, want a new one", key)
		}
	})

	r.Run("ReleaseClaimsHonorsNotBefore", func(r Runner) {
		queue := mustQueue(r, suite)
		now := time.Now().UTC()
		enqueueNudges(r, queue, now, "n-a", "n-b")
		first, err := queue.ClaimBatch(target, now)
		if err != nil {
			r.Fatalf("ClaimBatch: %v", err)
		}
		key := assertOneBatch(r, "first ClaimBatch", first, "n-a", "n-b")
		notBefore := now.Add(10 * time.Minute)
		if err := queue.ReleaseClaims([]string{"n-a", "n-b"}, notBefore); err != nil {
			r.Fatalf("ReleaseClaims: %v", err)
		}
		early := notBefore.Add(-time.Minute)
		if claimed, err := queue.ClaimBatch(target, early); err != nil || len(claimed) != 0 {
			r.Fatalf("ClaimBatch before notBefore = %v, %v; want nothing", nudgeIDs(claimed), err)
		}
		if claimed, err := queue.ClaimDue(target, early); err != nil || len(claimed) != 0 {
			r.Fatalf("ClaimDue before notBefore = %v, %v; want nothing", nudgeIDs(claimed), err)
		}
		pending, _, _, err := queue.ListFor(target, early)
		if err != nil {
			r.Fatalf("ListFor: %v", err)
		}
		if len(pending) != 2 {
			r.Fatalf("pending = %v, want both released nudges", nudgeIDs(pending))
		}
		for _, item := range pending {
			if item.DeliveryBatch != key {
				r.Errorf("released %s DeliveryBatch = %q, want the pin %q kept", item.ID, item.DeliveryBatch, key)
			}
		}
		resumed, err := queue.ClaimBatch(target, notBefore)
		if err != nil {
			r.Fatalf("ClaimBatch at notBefore: %v", err)
		}
		if got := assertOneBatch(r, "ClaimBatch at notBefore", resumed, "n-a", "n-b"); got != key {
			r.Fatalf("batch key = %q, want the pinned %q", got, key)
		}
	})

	r.Run("ReleaseClaimsLeavesALapsedLeaseAlone", func(r Runner) {
		queue := mustQueue(r, suite)
		now := time.Now().UTC()
		claimedAt := now.Add(-time.Hour)
		lapsed := nudgeItem("n-lapsed", "", claimedAt)
		if err := queue.Enqueue(lapsed); err != nil {
			r.Fatalf("Enqueue: %v", err)
		}
		if claimed, err := queue.ClaimDue(target, claimedAt); err != nil || len(claimed) != 1 {
			r.Fatalf("ClaimDue an hour ago = %v, %v; want n-lapsed", nudgeIDs(claimed), err)
		}
		// The lease ran out long ago, so the item is no longer in flight and
		// a backoff release has no claim to give back.
		if err := queue.ReleaseClaims([]string{"n-lapsed"}, now.Add(10*time.Minute)); err != nil {
			r.Fatalf("ReleaseClaims: %v", err)
		}
		claimed, err := queue.ClaimDue(target, now)
		if err != nil {
			r.Fatalf("ClaimDue after the release: %v", err)
		}
		if len(claimed) != 1 {
			r.Fatalf("ClaimDue = %v, want n-lapsed claimable now: releasing a lapsed lease must not impose a backoff", nudgeIDs(claimed))
		}
	})

	r.Run("DeliveryBatchRoundTrips", func(r Runner) {
		queue := mustQueue(r, suite)
		now := time.Now().UTC()
		pinned := nudgeItem("n-pinned", "", now)
		pinned.DeliveryBatch = "nudge-batch-conformance"
		if err := queue.Enqueue(pinned); err != nil {
			r.Fatalf("Enqueue: %v", err)
		}
		pending, _, _, err := queue.ListForAgent(ConformanceNudgeAgent, now)
		if err != nil {
			r.Fatalf("ListForAgent: %v", err)
		}
		if len(pending) != 1 || pending[0].DeliveryBatch != pinned.DeliveryBatch {
			r.Fatalf("pending = %+v, want n-pinned carrying DeliveryBatch %q", pending, pinned.DeliveryBatch)
		}
		state, err := queue.Snapshot()
		if err != nil {
			r.Fatalf("Snapshot: %v", err)
		}
		if len(state.Pending) != 1 || state.Pending[0].DeliveryBatch != pinned.DeliveryBatch {
			r.Fatalf("Snapshot pending = %+v, want n-pinned carrying DeliveryBatch %q", state.Pending, pinned.DeliveryBatch)
		}
		claimed, err := queue.ClaimBatch(target, now)
		if err != nil {
			r.Fatalf("ClaimBatch: %v", err)
		}
		if got := assertOneBatch(r, "ClaimBatch", claimed, "n-pinned"); got != pinned.DeliveryBatch {
			r.Fatalf("batch key = %q, want the stored pin %q", got, pinned.DeliveryBatch)
		}
	})
}

// runNudgeWholeQueueReadTests covers the reads by id and the whole-queue reads
// the queue's owner runs: Lookup, DueKeys and Empty.
func runNudgeWholeQueueReadTests(r Runner, suite NudgesSuite) {
	r.Helper()
	r.Run("LookupReportsBuckets", func(r Runner) {
		queue := mustQueue(r, suite)
		now := time.Now().UTC()
		const elsewhere = "alpha/elsewhere"
		pending := nudgeItem("n-pending", "", now)
		pending.Agent = elsewhere
		dead := nudgeItem("n-dead", "", now)
		dead.Agent = elsewhere
		expiring := nudgeItem("n-expired", "", now)
		expiring.Agent = elsewhere
		expiring.ExpiresAt = now.Add(10 * time.Second)
		for _, item := range []nudgequeue.Item{pending, dead, expiring} {
			if err := queue.Enqueue(item); err != nil {
				r.Fatalf("Enqueue(%s): %v", item.ID, err)
			}
		}
		enqueueNudges(r, queue, now, "n-in-flight", "n-acked")
		if _, err := queue.ClaimDue(storebinding.ClaimTarget{QueueKeys: []string{ConformanceNudgeAgent}}, now); err != nil {
			r.Fatalf("ClaimDue: %v", err)
		}
		if err := queue.Ack([]string{"n-acked"}, "delivered", "", "commit"); err != nil {
			r.Fatalf("Ack: %v", err)
		}
		if err := queue.Rollback(dead, "conformance rollback"); err != nil {
			r.Fatalf("Rollback: %v", err)
		}
		at := now.Add(30 * time.Second)
		got, err := queue.Lookup([]string{"n-pending", "n-in-flight", "n-dead", "n-expired", "n-acked", "n-unknown"}, at)
		if err != nil {
			r.Fatalf("Lookup: %v", err)
		}
		want := map[string]nudgequeue.Bucket{
			"n-pending":   nudgequeue.BucketPending,
			"n-in-flight": nudgequeue.BucketInFlight,
			"n-dead":      nudgequeue.BucketDead,
			"n-expired":   nudgequeue.BucketDead,
		}
		if len(got) != len(want) {
			r.Errorf("Lookup = %v, want exactly %v; acked and unknown ids are absent", got, want)
		}
		for id, bucket := range want {
			if got[id] != bucket {
				r.Errorf("Lookup[%s] = %q, want %q", id, got[id], bucket)
			}
		}
	})

	r.Run("DueKeys", func(r Runner) {
		queue := mustQueue(r, suite)
		now := time.Now().UTC()
		item := func(id, agent string, created time.Time) nudgequeue.Item {
			queued := nudgeItem(id, "", created)
			queued.Agent = agent
			return queued
		}
		future := item("n-future", "alpha/future", now)
		future.DeliverAfter = now.Add(time.Hour)
		dead := item("n-dead", "alpha/dead", now)
		for _, queued := range []nudgequeue.Item{
			item("n-due", "alpha/due", now),
			future,
			item("n-leased", "alpha/leased", now),
			item("n-lapsed", "alpha/lapsed", now.Add(-time.Hour)),
			dead,
			item("n-anonymous", "", now),
		} {
			if err := queue.Enqueue(queued); err != nil {
				r.Fatalf("Enqueue(%s): %v", queued.ID, err)
			}
		}
		if claimed, err := queue.ClaimDue(storebinding.ClaimTarget{QueueKeys: []string{"alpha/leased"}}, now); err != nil || len(claimed) != 1 {
			r.Fatalf("ClaimDue(leased) = %v, %v; want n-leased", nudgeIDs(claimed), err)
		}
		if claimed, err := queue.ClaimDue(storebinding.ClaimTarget{QueueKeys: []string{"alpha/lapsed"}}, now.Add(-time.Hour)); err != nil || len(claimed) != 1 {
			r.Fatalf("ClaimDue(lapsed) = %v, %v; want n-lapsed", nudgeIDs(claimed), err)
		}
		if err := queue.Rollback(dead, "conformance rollback"); err != nil {
			r.Fatalf("Rollback: %v", err)
		}
		keys, err := queue.DueKeys(now.Add(30 * time.Second))
		if err != nil {
			r.Fatalf("DueKeys: %v", err)
		}
		want := []string{"alpha/due", "alpha/lapsed"}
		if fmt.Sprint(keys) != fmt.Sprint(want) {
			r.Fatalf("DueKeys = %v, want %v: a due pending item and a lapsed lease are due; a future, leased, dead or unaddressed item is not", keys, want)
		}
	})

	r.Run("Empty", func(r Runner) {
		queue := mustQueue(r, suite)
		now := time.Now().UTC()
		assertEmpty := func(when string, want bool) {
			r.Helper()
			empty, err := queue.Empty()
			if err != nil {
				r.Fatalf("Empty %s: %v", when, err)
			}
			if empty != want {
				r.Fatalf("Empty %s = %v, want %v", when, empty, want)
			}
		}
		assertEmpty("on a fresh queue", true)
		enqueueNudges(r, queue, now, "n-a")
		assertEmpty("with a pending nudge", false)
		if claimed, err := queue.ClaimDue(storebinding.ClaimTarget{QueueKeys: []string{ConformanceNudgeAgent}}, now); err != nil || len(claimed) != 1 {
			r.Fatalf("ClaimDue = %v, %v; want n-a", nudgeIDs(claimed), err)
		}
		assertEmpty("with a nudge in flight", false)
		if err := queue.Ack([]string{"n-a"}, "delivered", "", "commit"); err != nil {
			r.Fatalf("Ack: %v", err)
		}
		assertEmpty("after the ack", true)
		dead := nudgeItem("n-dead", "", now)
		if err := queue.Enqueue(dead); err != nil {
			r.Fatalf("Enqueue(n-dead): %v", err)
		}
		if err := queue.Rollback(dead, "conformance rollback"); err != nil {
			r.Fatalf("Rollback: %v", err)
		}
		assertEmpty("with only a dead letter", false)
	})
}

// enqueueNudges enqueues one conformance nudge per id, all due at now.
func enqueueNudges(r Runner, queue storebinding.NudgeQueue, now time.Time, ids ...string) {
	r.Helper()
	for _, id := range ids {
		if err := queue.Enqueue(nudgeItem(id, "", now)); err != nil {
			r.Fatalf("Enqueue(%s): %v", id, err)
		}
	}
}

// assertOneBatch requires items to be exactly the ids, all pinned to one
// non-empty batch key, and returns that key.
func assertOneBatch(r Runner, verb string, items []nudgequeue.Item, ids ...string) string {
	r.Helper()
	got := nudgeIDs(items)
	sort.Strings(got)
	want := append([]string(nil), ids...)
	sort.Strings(want)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		r.Fatalf("%s = %v, want exactly %v", verb, got, want)
	}
	key := items[0].DeliveryBatch
	if key == "" {
		r.Fatalf("%s claimed %v with no delivery batch", verb, got)
	}
	for _, item := range items {
		if item.DeliveryBatch != key {
			r.Fatalf("%s claimed %s under batch %q and %s under %q, want one batch", verb, items[0].ID, key, item.ID, item.DeliveryBatch)
		}
	}
	return key
}
