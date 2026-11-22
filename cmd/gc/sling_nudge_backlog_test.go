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
	if err := withNudgeQueueState(cityPath, "test", func(state *nudgeQueueState) error {
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

type enqueueTiming struct {
	virtualElapsed time.Duration
	operations     int64
}

// timeEnqueue enqueues one nudge over a dead backlog of older-binary dead
// letters (shadow-backed, no debt) on a store whose every call advances a fake
// clock by latency. The hold is pure (ga-fsp5w9): the backlog costs no store
// call, each dead letter gets a one-time Verify debt left for the controller,
// and the enqueue costs only its shadow save.
func timeEnqueue(t *testing.T, backlog int, latency time.Duration) enqueueTiming {
	t.Helper()
	cityPath := t.TempDir()
	fakeClock := &clock.Fake{Time: time.Now().UTC()}
	seededBuckets := seedDeadBacklog(t, cityPath, fakeClock.Now(), backlog)
	store := &advancingNudgeStore{fakeClock: fakeClock, latency: latency, failAfter: advancingNudgeStoreSetupOps}
	item := queuedNudge{ID: "nudge-new", Agent: "gascity/deployer", Source: "sling", Message: "Work slung. Check your hook."}
	start := fakeClock.Now()
	if err := enqueueQueuedNudgeWithStoreAndClock(cityPath, beads.NudgesStore{Store: store}, item, fakeClock); err != nil {
		t.Fatalf("enqueue (backlog=%d): %v", backlog, err)
	}
	timing := enqueueTiming{
		virtualElapsed: fakeClock.Now().Sub(start),
		operations:     store.operations(),
	}
	if want := advancingNudgeStoreSetupOps * latency; timing.virtualElapsed != want {
		t.Fatalf("virtual enqueue elapsed (backlog=%d) = %v, want %v (the shadow save alone)", backlog, timing.virtualElapsed, want)
	}

	buckets := nudgeQueueBucketsByID(t, cityPath)
	if got, want := len(buckets), backlog+1; got != want {
		t.Fatalf("queued item count (backlog=%d) = %d, want %d; buckets=%v", backlog, got, want, buckets)
	}
	for id, bucket := range seededBuckets {
		if buckets[id] != bucket {
			t.Fatalf("dead nudge %q bucket = %q, want %q", id, buckets[id], bucket)
		}
	}
	state := loadNudgeStateForTest(t, cityPath)
	for _, dead := range state.Dead {
		if dead.Shadow == nil || !dead.Shadow.Verify || !dead.Shadow.NotBefore.IsZero() {
			t.Fatalf("dead nudge %q debt = %+v, want an untaken Verify debt for the controller", dead.ID, dead.Shadow)
		}
	}
	if bucket := buckets[item.ID]; bucket != "pending" {
		t.Fatalf("new queued nudge bucket = %q, want pending; buckets=%v", bucket, buckets)
	}
	return timing
}

// The foreground `--nudge` enqueue must be bounded regardless of nudge-queue
// backlog: its hold does no store I/O, so a dead backlog adds nothing.
func TestSlingNudgeEnqueueBoundedByBacklog(t *testing.T) {
	const latency = 20 * time.Millisecond
	small := timeEnqueue(t, 40, latency)
	big := timeEnqueue(t, 160, latency)
	t.Logf("enqueue backlog=40 -> virtual=%v ops=%d ; backlog=160 -> virtual=%v ops=%d",
		small.virtualElapsed, small.operations, big.virtualElapsed, big.operations)
	if small != big {
		t.Fatalf("foreground enqueue grew with backlog: backlog=40 got %+v, backlog=160 got %+v", small, big)
	}
}
