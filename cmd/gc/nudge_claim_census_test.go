package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// A claim lists the city's sessions only for a due candidate pinned to another
// session: an item that is not yet due cannot be claimed, so asking the census
// about it opens a store and lists session beads for nothing. Once the item is
// due, the census runs once and the claim adopts it from the superseded
// session. Both the drain claim and the batch claim obey this.
func TestQueuedNudgeClaimAsksTheCensusOnlyForADueCandidate(t *testing.T) {
	claims := []struct {
		name  string
		claim func(cityPath string, target nudgeTarget, now time.Time) ([]queuedNudge, error)
	}{
		{name: "drain", claim: claimDueQueuedNudgesForTarget},
		{name: "batch", claim: claimDueQueuedNudgeBatchForTarget},
	}
	for _, tc := range claims {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_BEADS", "file")
			dir := t.TempDir()
			now := time.Now().UTC()
			dueAt := now.Add(time.Hour)
			item := newQueuedNudgeWithOptions("worker", "for the old session", "session", now, queuedNudgeOptions{
				ID:                "n-later",
				SessionID:         "gc-old",
				ContinuationEpoch: "1",
			})
			item.DeliverAfter = dueAt
			if err := enqueueQueuedNudge(dir, item); err != nil {
				t.Fatalf("enqueueQueuedNudge: %v", err)
			}

			calls := 0
			prev := liveNudgeFenceSessionIDs
			t.Cleanup(func() { liveNudgeFenceSessionIDs = prev })
			liveNudgeFenceSessionIDs = func(string) map[string]struct{} {
				calls++
				return map[string]struct{}{"gc-new": {}}
			}
			target := nudgeTarget{agent: config.Agent{Name: "worker"}, sessionID: "gc-new", continuationEpoch: "2"}

			claimed, err := tc.claim(dir, target, now)
			if err != nil {
				t.Fatalf("claim before due: %v", err)
			}
			if len(claimed) != 0 {
				t.Fatalf("claimed before due = %#v, want nothing", claimed)
			}
			if calls != 0 {
				t.Fatalf("a claim with no due candidate asked the census %d times, want never", calls)
			}

			claimed, err = tc.claim(dir, target, dueAt.Add(time.Second))
			if err != nil {
				t.Fatalf("claim once due: %v", err)
			}
			if calls != 1 {
				t.Fatalf("a claim with one due candidate pinned to another session asked the census %d times, want once", calls)
			}
			if len(claimed) != 1 || claimed[0].ID != item.ID {
				t.Fatalf("claimed once due = %#v, want n-later adopted from the superseded session", claimed)
			}
		})
	}
}

// A batch claim asks the census only for a pinned batch that has a due,
// claimable member. A batch whose every member is backed off is not claimable,
// so deciding whether it splits across sessions is work for nothing. One case
// still asks: a batch with a due member of this session beside a not-yet-due
// member pinned elsewhere, because the file queue claims a pinned batch whole
// whatever its members' deliver_after, so whether the batch splits decides the
// claim. The batch is claimed whole once the other session is gone, and waits
// while that session still holds a live fence.
func TestQueuedNudgeBatchClaimAsksTheCensusOnlyForADuePinnedBatch(t *testing.T) {
	const batch = "nudge-batch-0123456789abcdef"
	target := nudgeTarget{agent: config.Agent{Name: "worker"}, sessionID: "gc-new", continuationEpoch: "2"}
	pinned := func(id, session, epoch string, now, deliverAfter time.Time) queuedNudge {
		item := newQueuedNudgeWithOptions("worker", "pinned "+id, "session", now, queuedNudgeOptions{
			ID: id, SessionID: session, ContinuationEpoch: epoch,
		})
		item.DeliverAfter = deliverAfter
		item.DeliveryBatch = batch
		return item
	}
	countCensus := func(t *testing.T, live ...string) *int {
		calls := 0
		prev := liveNudgeFenceSessionIDs
		t.Cleanup(func() { liveNudgeFenceSessionIDs = prev })
		liveNudgeFenceSessionIDs = func(string) map[string]struct{} {
			calls++
			ids := make(map[string]struct{}, len(live))
			for _, id := range live {
				ids[id] = struct{}{}
			}
			return ids
		}
		return &calls
	}

	t.Run("backed-off batch pinned elsewhere", func(t *testing.T) {
		t.Setenv("GC_BEADS", "file")
		dir := t.TempDir()
		now := time.Now().UTC()
		dueAt := now.Add(time.Hour)
		item := pinned("n-pinned", "gc-old", "1", now, dueAt)
		if err := enqueueQueuedNudge(dir, item); err != nil {
			t.Fatalf("enqueueQueuedNudge: %v", err)
		}
		calls := countCensus(t, "gc-new")

		claimed, err := claimDueQueuedNudgeBatchForTarget(dir, target, now)
		if err != nil {
			t.Fatalf("claim before due: %v", err)
		}
		if len(claimed) != 0 || *calls != 0 {
			t.Fatalf("claim before due: claimed %d, census asked %d times; want nothing and never", len(claimed), *calls)
		}

		claimed, err = claimDueQueuedNudgeBatchForTarget(dir, target, dueAt.Add(time.Second))
		if err != nil {
			t.Fatalf("claim once due: %v", err)
		}
		if *calls != 1 {
			t.Fatalf("a claim with a due batch pinned elsewhere asked the census %d times, want once", *calls)
		}
		if len(claimed) != 1 || claimed[0].ID != item.ID || claimed[0].DeliveryBatch != batch {
			t.Fatalf("claimed once due = %#v, want n-pinned adopted under its pinned batch", claimed)
		}
	})

	t.Run("due own member beside a backed-off member pinned elsewhere", func(t *testing.T) {
		t.Setenv("GC_BEADS", "file")
		dir := t.TempDir()
		now := time.Now().UTC()
		for _, item := range []queuedNudge{
			pinned("n-mine", "gc-new", "2", now, time.Time{}),
			pinned("n-theirs", "gc-old", "1", now, now.Add(time.Hour)),
		} {
			if err := enqueueQueuedNudge(dir, item); err != nil {
				t.Fatalf("enqueueQueuedNudge(%s): %v", item.ID, err)
			}
		}
		calls := countCensus(t, "gc-new")

		claimed, err := claimDueQueuedNudgeBatchForTarget(dir, target, now)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if *calls != 1 {
			t.Fatalf("deciding whether a due batch splits asked the census %d times, want once", *calls)
		}
		if len(claimed) != 2 {
			t.Fatalf("claimed = %#v, want the pinned batch claimed whole", claimed)
		}
	})

	t.Run("due own member beside a backed-off member fenced by a live session", func(t *testing.T) {
		t.Setenv("GC_BEADS", "file")
		dir := t.TempDir()
		now := time.Now().UTC()
		for _, item := range []queuedNudge{
			pinned("n-mine", "gc-new", "2", now, time.Time{}),
			pinned("n-theirs", "gc-old", "1", now, now.Add(time.Hour)),
		} {
			if err := enqueueQueuedNudge(dir, item); err != nil {
				t.Fatalf("enqueueQueuedNudge(%s): %v", item.ID, err)
			}
		}
		calls := countCensus(t, "gc-new", "gc-old")

		claimed, err := claimDueQueuedNudgeBatchForTarget(dir, target, now)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if *calls != 1 {
			t.Fatalf("deciding whether a due batch splits asked the census %d times, want once", *calls)
		}
		if len(claimed) != 0 {
			t.Fatalf("claimed = %#v, want nothing: n-theirs is fenced by a live session, so the batch splits and waits", claimed)
		}
	})
}
