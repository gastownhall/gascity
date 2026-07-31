package main

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

type statusListingBudgetStore struct {
	beads.Store
	latency time.Duration
	ops     int64
}

func (s *statusListingBudgetStore) tick() {
	atomic.AddInt64(&s.ops, 1)
	time.Sleep(s.latency)
}

func (s *statusListingBudgetStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.tick()
	return s.Store.List(query)
}

func (s *statusListingBudgetStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.tick()
	return s.Store.SetMetadataBatch(id, kvs)
}

func (s *statusListingBudgetStore) Close(id string) error {
	s.tick()
	return s.Store.Close(id)
}

func seedNudgeStatusBudgetFixture(t *testing.T, cityPath string, now time.Time, deadBacklog int) *beads.MemStore {
	t.Helper()
	backing := beads.NewMemStore()
	targetPending := newQueuedNudgeWithOptions("gascity/deployer", "pending status result", "session", now, queuedNudgeOptions{
		ID: "nudge-status-pending",
	})
	targetPending.DeliverAfter = now.Add(time.Minute).UTC()
	targetPending.ExpiresAt = now.Add(time.Hour).UTC()
	targetInFlight := newQueuedNudgeWithOptions("gascity/deployer", "in-flight status result", "session", now, queuedNudgeOptions{
		ID: "nudge-status-in-flight",
	})
	targetInFlight.ClaimedAt = now.Add(-time.Minute).UTC()
	targetInFlight.LeaseUntil = now.Add(time.Hour).UTC()
	targetInFlight.ExpiresAt = now.Add(time.Hour).UTC()
	targetDead := queuedNudge{
		ID:        "nudge-status-dead",
		Agent:     "gascity/deployer",
		Source:    "session",
		Message:   "dead status result",
		CreatedAt: now.Add(-20 * time.Minute).UTC(),
		DeadAt:    now.Add(-10 * time.Minute).UTC(),
		LastError: "delivery failed",
	}

	store := beads.NudgesStore{Store: backing}
	if err := withNudgeQueueState(cityPath, func(state *nudgeQueueState) error {
		state.Pending = append(state.Pending, targetPending)
		state.InFlight = append(state.InFlight, targetInFlight)
		state.Dead = append(state.Dead, targetDead)
		for i := 0; i < deadBacklog; i++ {
			item := queuedNudge{
				ID:        fmt.Sprintf("nudge-status-dead-backlog-%03d", i),
				Agent:     "gascity/other",
				Source:    "sling",
				Message:   "old dead backlog",
				CreatedAt: now.Add(-3 * time.Hour).UTC(),
				DeadAt:    now.Add(-2 * time.Hour).UTC(),
				LastError: "delivery failed",
			}
			beadID, _, err := ensureQueuedNudgeBead(store, item)
			if err != nil {
				return fmt.Errorf("creating shadow bead for %s: %w", item.ID, err)
			}
			item.BeadID = beadID
			if err := markQueuedNudgeTerminal(store, item, "failed", item.LastError, "", item.DeadAt); err != nil {
				return fmt.Errorf("terminalizing shadow bead for %s: %w", item.ID, err)
			}
			state.Dead = append(state.Dead, item)
		}
		return nil
	}); err != nil {
		t.Fatalf("seeding nudge status budget fixture: %v", err)
	}
	return backing
}

func installStatusListingBudgetStore(t *testing.T, backing beads.Store) *statusListingBudgetStore {
	t.Helper()
	slow := &statusListingBudgetStore{
		Store:   backing,
		latency: 15 * time.Millisecond,
	}
	prev := openNudgeBeadStore
	openNudgeBeadStore = func(string) beads.NudgesStore {
		return beads.NudgesStore{Store: slow}
	}
	t.Cleanup(func() { openNudgeBeadStore = prev })
	return slow
}

func TestNudgeStatusListingMaintenanceBudgetPreservesStatusAndSkippedBacklog(t *testing.T) {
	const deadBacklog = 180
	// Seed against the real wall clock: the maintenance budget is enforced in
	// the prune/recover loops against time.Now(), so a fixed past/future now
	// would make the deadline already-expired (zero budget) or unreachable
	// (unbounded) and the partial-maintenance assertions below would pass
	// vacuously. Mirrors TestNudgeDrainClaimBoundedByBacklog.
	now := time.Now().UTC()
	cases := []struct {
		name string
		list func(string, nudgeTarget, time.Time) ([]queuedNudge, []queuedNudge, []queuedNudge, error)
	}{
		{
			name: "agent_name_listing",
			list: func(cityPath string, _ nudgeTarget, now time.Time) ([]queuedNudge, []queuedNudge, []queuedNudge, error) {
				return listQueuedNudges(cityPath, "gascity/deployer", now)
			},
		},
		{
			name: "target_listing",
			list: listQueuedNudgesForTarget,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			backing := seedNudgeStatusBudgetFixture(t, cityPath, now, deadBacklog)
			slow := installStatusListingBudgetStore(t, backing)

			pending, inFlight, dead, err := tc.list(cityPath, nudgeTarget{alias: "gascity/deployer"}, now)
			if err != nil {
				t.Fatalf("%s returned error after %d slow store ops: %v", tc.name, atomic.LoadInt64(&slow.ops), err)
			}
			if got := queuedNudgeIDs(pending); len(got) != 1 || got[0] != "nudge-status-pending" {
				t.Fatalf("pending IDs = %v, want [nudge-status-pending]", got)
			}
			if got := queuedNudgeIDs(inFlight); len(got) != 1 || got[0] != "nudge-status-in-flight" {
				t.Fatalf("in-flight IDs = %v, want [nudge-status-in-flight]", got)
			}
			if got := queuedNudgeIDs(dead); len(got) != 1 || got[0] != "nudge-status-dead" {
				t.Fatalf("dead IDs = %v, want [nudge-status-dead]", got)
			}
			ops := atomic.LoadInt64(&slow.ops)
			if ops == 0 {
				t.Fatalf("status maintenance performed no store ops; want partial progress within the budget (a zero-progress deadline — the logical-now/wall-clock mismatch — is the regression this guards)")
			}
			if ops >= deadBacklog {
				t.Fatalf("status maintenance store ops = %d, want fewer than dead backlog %d to prove the deadline cut in", ops, deadBacklog)
			}

			buckets := nudgeQueueBucketsByID(t, cityPath)
			remainingBacklog := 0
			for i := 0; i < deadBacklog; i++ {
				if buckets[fmt.Sprintf("nudge-status-dead-backlog-%03d", i)] == "dead" {
					remainingBacklog++
				}
			}
			if remainingBacklog == 0 {
				t.Fatalf("dead backlog was fully pruned by foreground status maintenance; want skipped items left queued for a later pass")
			}
		})
	}
}
