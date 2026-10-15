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
