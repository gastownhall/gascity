package main

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
)

const (
	advancingNudgeStoreSetupOps    = 1
	advancingNudgeStoreDeadItemOps = 4
)

type advancingNudgeStore struct {
	beads.Store
	fakeClock *clock.Fake
	latency   time.Duration
	failAfter int64
	ops       int64
}

func (s *advancingNudgeStore) tick() error {
	op := atomic.AddInt64(&s.ops, 1)
	s.fakeClock.Advance(s.latency)
	if s.failAfter > 0 && op > s.failAfter {
		return fmt.Errorf("advancing nudge store operation %d exceeded test budget %d", op, s.failAfter)
	}
	return nil
}

func (s *advancingNudgeStore) operations() int64 {
	return atomic.LoadInt64(&s.ops)
}

func (s *advancingNudgeStore) List(beads.ListQuery) ([]beads.Bead, error) {
	if err := s.tick(); err != nil {
		return nil, err
	}
	return []beads.Bead{{
		ID:       "shadow-open",
		Type:     nudgeBeadType,
		Status:   "open",
		Labels:   []string{nudgeBeadLabel},
		Metadata: map[string]string{"state": "queued"},
	}}, nil
}

func (s *advancingNudgeStore) Create(b beads.Bead) (beads.Bead, error) {
	if err := s.tick(); err != nil {
		return beads.Bead{}, err
	}
	if b.ID == "" {
		b.ID = "created-shadow"
	}
	b.Status = "open"
	return b, nil
}

func (s *advancingNudgeStore) Get(id string) (beads.Bead, error) {
	if err := s.tick(); err != nil {
		return beads.Bead{}, err
	}
	return beads.Bead{ID: id, Type: nudgeBeadType, Status: "open", Metadata: map[string]string{"state": "queued"}}, nil
}

func (s *advancingNudgeStore) Close(string) error {
	return s.tick()
}

func (s *advancingNudgeStore) SetMetadata(string, string, string) error {
	return s.tick()
}

func (s *advancingNudgeStore) SetMetadataBatch(string, map[string]string) error {
	return s.tick()
}

func seedNudgeBudgetPreservationBacklog(t *testing.T, cityPath string, now time.Time, reference *nudgeReference, deadCount int) map[string]string {
	t.Helper()
	now = now.UTC()
	buckets := make(map[string]string, deadCount+4)
	if err := withNudgeQueueState(cityPath, func(state *nudgeQueueState) error {
		for i := 0; i < 2; i++ {
			id := fmt.Sprintf("nudge-pending-preserve-%d", i)
			buckets[id] = "pending"
			state.Pending = append(state.Pending, queuedNudge{
				ID:           id,
				BeadID:       "bead-" + id,
				Agent:        "gascity/deployer",
				Source:       "sling",
				Message:      "pending supersede tail",
				Reference:    reference,
				CreatedAt:    now.Add(-time.Hour),
				DeliverAfter: now.Add(time.Hour),
				ExpiresAt:    now.Add(time.Hour),
			})
		}
		for i := 0; i < 2; i++ {
			id := fmt.Sprintf("nudge-in-flight-preserve-%d", i)
			buckets[id] = "in-flight"
			state.InFlight = append(state.InFlight, queuedNudge{
				ID:           id,
				BeadID:       "bead-" + id,
				Agent:        "gascity/deployer",
				Source:       "sling",
				Message:      "in-flight supersede tail",
				Reference:    reference,
				CreatedAt:    now.Add(-time.Hour),
				DeliverAfter: now.Add(-time.Minute),
				ExpiresAt:    now.Add(time.Hour),
				ClaimedAt:    now.Add(-time.Minute),
				LeaseUntil:   now.Add(time.Hour),
			})
		}
		for i := 0; i < deadCount; i++ {
			id := fmt.Sprintf("nudge-dead-preserve-%03d", i)
			buckets[id] = "dead"
			state.Dead = append(state.Dead, queuedNudge{
				ID:        id,
				BeadID:    "bead-" + id,
				Agent:     "gascity/deployer",
				Source:    "sling",
				Message:   "dead backlog",
				CreatedAt: now.Add(-2 * time.Hour),
				DeadAt:    now.Add(-2 * time.Hour),
				LastError: "expired",
			})
		}
		return nil
	}); err != nil {
		t.Fatalf("seeding nudge backlog: %v", err)
	}
	return buckets
}

func nudgeQueueBucketsByID(t *testing.T, cityPath string) map[string]string {
	t.Helper()
	buckets := make(map[string]string)
	if err := withNudgeQueueState(cityPath, func(state *nudgeQueueState) error {
		for _, item := range state.Pending {
			buckets[item.ID] = "pending"
		}
		for _, item := range state.InFlight {
			buckets[item.ID] = "in-flight"
		}
		for _, item := range state.Dead {
			buckets[item.ID] = "dead"
		}
		return nil
	}); err != nil {
		t.Fatalf("reading nudge queue state: %v", err)
	}
	return buckets
}

// TestSlingNudgeEnqueueBudgetPreservesQueuedItems exercises all three
// maintenance loops AND the supersede loop at once: a Dead backlog large
// enough to guarantee the budget cuts in, plus Pending/InFlight items that
// share the new item's supersession reference, so their "did the budget's
// early exit correctly leave supersede candidates untouched" behavior is
// asserted, not just inferred from pruneDeadQueuedNudges alone.
func TestSlingNudgeEnqueueBudgetPreservesQueuedItems(t *testing.T) {
	const (
		deadBacklog = 160
		latency     = 40 * time.Millisecond
	)
	reference := &nudgeReference{Kind: "bead", ID: "ga-budget-preservation"}
	cityPath := t.TempDir()
	fakeClock := &clock.Fake{Time: time.Now().UTC()}
	seededBuckets := seedNudgeBudgetPreservationBacklog(t, cityPath, fakeClock.Now(), reference, deadBacklog)
	maxOps := int64(nudgeEnqueueMaintenanceBudget/latency) + advancingNudgeStoreSetupOps + advancingNudgeStoreDeadItemOps
	store := &advancingNudgeStore{fakeClock: fakeClock, latency: latency, failAfter: maxOps}
	item := newQueuedNudgeWithOptions("gascity/deployer", "Work slung. Check your hook.", "sling", fakeClock.Now(), queuedNudgeOptions{
		ID:        "nudge-new-preservation",
		Reference: reference,
	})

	start := fakeClock.Now()
	if err := enqueueQueuedNudgeWithStoreAndClock(cityPath, beads.NudgesStore{Store: store}, item, fakeClock); err != nil {
		t.Fatalf("enqueueQueuedNudgeWithStoreAndClock: %v", err)
	}
	virtualElapsed := fakeClock.Now().Sub(start)
	if virtualElapsed <= nudgeEnqueueMaintenanceBudget {
		t.Fatalf("virtual enqueue elapsed = %v, want the dead backlog to exhaust the %v budget", virtualElapsed, nudgeEnqueueMaintenanceBudget)
	}
	if maxElapsed := nudgeEnqueueMaintenanceBudget + (advancingNudgeStoreSetupOps+advancingNudgeStoreDeadItemOps)*latency; virtualElapsed > maxElapsed {
		t.Fatalf("virtual enqueue elapsed = %v, want at most %v", virtualElapsed, maxElapsed)
	}
	if ops := store.operations(); ops > maxOps {
		t.Fatalf("advancing store ops = %d, want at most %d to prove the maintenance budget cut in", ops, maxOps)
	}

	buckets := nudgeQueueBucketsByID(t, cityPath)
	if got, want := len(buckets), len(seededBuckets)+1; got != want {
		t.Fatalf("queued item count = %d, want %d; buckets=%v", got, want, buckets)
	}
	for id, wantBucket := range seededBuckets {
		if bucket := buckets[id]; bucket != wantBucket {
			t.Fatalf("seeded queued nudge %q bucket = %q, want %q; buckets=%v", id, bucket, wantBucket, buckets)
		}
	}
	for i := 0; i < 2; i++ {
		id := fmt.Sprintf("nudge-pending-preserve-%d", i)
		if bucket := buckets[id]; bucket != "pending" {
			t.Fatalf("%s bucket = %q, want pending because supersede budget was already exhausted", id, bucket)
		}
		id = fmt.Sprintf("nudge-in-flight-preserve-%d", i)
		if bucket := buckets[id]; bucket != "in-flight" {
			t.Fatalf("%s bucket = %q, want in-flight because supersede budget was already exhausted", id, bucket)
		}
	}
	if bucket := buckets[item.ID]; bucket != "pending" {
		t.Fatalf("new queued nudge bucket = %q, want pending", bucket)
	}
}

// TestSlingNudgeEnqueueEmptyBacklogFast pins that an empty queue still
// enqueues near-instantly: none of the three maintenance loops iterate, so
// the deadline check never fires regardless of nudgeEnqueueMaintenanceBudget.
func TestSlingNudgeEnqueueEmptyBacklogFast(t *testing.T) {
	cityPath := t.TempDir()
	store := &advancingNudgeStore{fakeClock: &clock.Fake{Time: time.Now().UTC()}, failAfter: 4}
	item := newQueuedNudgeWithOptions("gascity/deployer", "Work slung. Check your hook.", "sling", time.Now(), queuedNudgeOptions{
		ID: "nudge-empty-backlog",
	})

	start := time.Now()
	if err := enqueueQueuedNudgeWithStore(cityPath, beads.NudgesStore{Store: store}, item); err != nil {
		t.Fatalf("enqueueQueuedNudgeWithStore: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("empty-backlog enqueue elapsed = %v, want under 500ms", elapsed.Round(time.Millisecond))
	}
	if ops := store.operations(); ops > 2 {
		t.Fatalf("advancing store ops = %d, want at most backing-bead setup ops for an empty backlog", ops)
	}

	buckets := nudgeQueueBucketsByID(t, cityPath)
	if got := buckets[item.ID]; got != "pending" {
		t.Fatalf("new queued nudge bucket = %q, want pending; buckets=%v", got, buckets)
	}
	if got, want := len(buckets), 1; got != want {
		t.Fatalf("queued item count = %d, want %d; buckets=%v", got, want, buckets)
	}
}
