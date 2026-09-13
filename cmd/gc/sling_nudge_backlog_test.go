package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
)

// deadBacklogID returns a zero-padded ID so that lexicographic order (the
// nudgequeue.SortState ID tiebreak, used when every item shares the same
// DeadAt/CreatedAt as seeded here) matches seed/index order 0..n-1.
func deadBacklogID(i int) string {
	return fmt.Sprintf("nudge-dead-%04d", i)
}

func seedDeadBacklog(t *testing.T, cityPath string, now time.Time, n int) map[string]string {
	t.Helper()
	buckets := make(map[string]string, n)
	if err := withNudgeQueueState(cityPath, func(state *nudgeQueueState) error {
		for i := 0; i < n; i++ {
			id := deadBacklogID(i)
			buckets[id] = "dead"
			state.Dead = append(state.Dead, queuedNudge{
				ID: id, BeadID: "bead-" + id,
				Agent: "gascity/deployer", Source: "sling", Message: "backlog",
				CreatedAt: now.Add(-2 * time.Hour), DeadAt: now.Add(-2 * time.Hour),
				LastError: "expired",
			})
		}
		return nil
	}); err != nil {
		t.Fatalf("seeding backlog: %v", err)
	}
	return buckets
}

// deadBacklogProcessed returns how many leading (by seed/index order, which
// SortState's ID tiebreak preserves here) dead items the maintenance sweep
// repairs-and-prunes before nudgeEnqueueMaintenanceBudget elapses. Each
// processed item costs advancingNudgeStoreDeadItemOps store ops at the given
// latency; the sweep checks the deadline before processing each item, so the
// count is the number of items whose cumulative cost falls at-or-under the
// budget.
func deadBacklogProcessed(backlog int, latency time.Duration) int {
	itemCost := time.Duration(advancingNudgeStoreDeadItemOps) * latency
	processed := int(nudgeEnqueueMaintenanceBudget/itemCost) + 1
	if processed > backlog {
		processed = backlog
	}
	return processed
}

type enqueueTiming struct {
	virtualElapsed time.Duration
	operations     int64
}

func timeEnqueue(t *testing.T, backlog int, latency time.Duration) enqueueTiming {
	t.Helper()
	cityPath := t.TempDir()
	fakeClock := &clock.Fake{Time: time.Now().UTC()}
	seededBuckets := seedDeadBacklog(t, cityPath, fakeClock.Now(), backlog)
	maxOps := int64(nudgeEnqueueMaintenanceBudget/latency) + advancingNudgeStoreSetupOps + advancingNudgeStoreDeadItemOps
	store := &advancingNudgeStore{fakeClock: fakeClock, latency: latency, failAfter: maxOps}
	item := queuedNudge{ID: "nudge-new", Agent: "gascity/deployer", Source: "sling", Message: "Work slung. Check your hook."}
	start := fakeClock.Now()
	if err := enqueueQueuedNudgeWithStoreAndClock(cityPath, beads.NudgesStore{Store: store}, item, fakeClock); err != nil {
		t.Fatalf("enqueue (backlog=%d): %v", backlog, err)
	}
	timing := enqueueTiming{
		virtualElapsed: fakeClock.Now().Sub(start),
		operations:     store.operations(),
	}
	if timing.virtualElapsed <= nudgeEnqueueMaintenanceBudget {
		t.Fatalf("virtual enqueue elapsed (backlog=%d) = %v, want the dead backlog to exhaust the %v budget", backlog, timing.virtualElapsed, nudgeEnqueueMaintenanceBudget)
	}
	if maxElapsed := nudgeEnqueueMaintenanceBudget + (advancingNudgeStoreSetupOps+advancingNudgeStoreDeadItemOps)*latency; timing.virtualElapsed > maxElapsed {
		t.Fatalf("virtual enqueue elapsed (backlog=%d) = %v, want at most %v", backlog, timing.virtualElapsed, maxElapsed)
	}
	if timing.operations > maxOps {
		t.Fatalf("advancing store ops (backlog=%d) = %d, want at most %d", backlog, timing.operations, maxOps)
	}

	processed := deadBacklogProcessed(backlog, latency)
	survivors := backlog - processed
	buckets := nudgeQueueBucketsByID(t, cityPath)
	if got, want := len(buckets), survivors+1; got != want {
		t.Fatalf("queued item count (backlog=%d) = %d, want %d (processed=%d survived=%d); buckets=%v", backlog, got, want, processed, survivors, buckets)
	}
	for i := 0; i < backlog; i++ {
		id := deadBacklogID(i)
		bucket, present := buckets[id]
		if i < processed {
			if present {
				t.Fatalf("dead nudge %q (i=%d, backlog=%d) still present as %q, want repaired-and-pruned; buckets=%v", id, i, backlog, bucket, buckets)
			}
			continue
		}
		if bucket != seededBuckets[id] {
			t.Fatalf("surviving dead nudge %q (i=%d, backlog=%d) bucket = %q, want %q; buckets=%v", id, i, backlog, bucket, seededBuckets[id], buckets)
		}
	}
	if bucket := buckets[item.ID]; bucket != "pending" {
		t.Fatalf("new queued nudge bucket = %q, want pending; buckets=%v", bucket, buckets)
	}
	return timing
}

// The foreground `--nudge` enqueue must be bounded regardless of nudge-queue
// backlog. The advancing store makes the aggregate deadline observable without
// spending the maintenance budget in real time.
func TestSlingNudgeEnqueueBoundedByBacklog(t *testing.T) {
	const latency = 20 * time.Millisecond
	small := timeEnqueue(t, 40, latency)
	big := timeEnqueue(t, 160, latency)
	t.Logf("enqueue backlog=40 -> virtual=%v ops=%d ; backlog=160 -> virtual=%v ops=%d",
		small.virtualElapsed, small.operations, big.virtualElapsed, big.operations)
	if small != big {
		t.Fatalf("foreground enqueue grew with backlog: backlog=40 got %+v, backlog=160 got %+v", small, big)
	}
	if fullBacklogOps := int64(advancingNudgeStoreSetupOps + advancingNudgeStoreDeadItemOps*40); small.operations >= fullBacklogOps {
		t.Fatalf("advancing store ops = %d, want fewer than full 40-item backlog cost %d to prove cutoff", small.operations, fullBacklogOps)
	}
}
