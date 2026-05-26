package coordstore

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"
)

// Seeder populates a StoreAdapter with a realistic starting population
// derived from the S2 volume census.
type Seeder struct {
	rng *rand.Rand
}

// NewSeeder creates a Seeder with the given random seed.
func NewSeeder(seed uint64) *Seeder {
	return &Seeder{rng: rand.New(rand.NewPCG(seed, seed^0xdeadbeef))}
}

// SeedResult describes the population created by Seed.
type SeedResult struct {
	MainOpenIDs   []string
	MainClosedIDs []string
	WispOpenIDs   []string
	DepEdges      []Dep
}

// Seed populates the adapter with the population described by wl.
// Returns the IDs of all created records (for use in workload operations).
func (s *Seeder) Seed(ctx context.Context, a StoreAdapter, wl WorkloadConfig) (SeedResult, error) {
	var result SeedResult
	now := time.Now()

	recordTypes := []string{"task", "task", "task", "session", "step", "molecule"}
	labels := []string{
		"P1", "P2", "needs-review", "needs-design", "source:actual-architect",
		"bug", "feature", "chore", "epic",
	}

	// Create open main-tier records.
	for i := range wl.MainOpenCount {
		rType := recordTypes[s.rng.IntN(len(recordTypes))]
		assignee := MailAssignees[s.rng.IntN(len(MailAssignees))]
		r := Record{
			Title:     fmt.Sprintf("open-main-%d", i),
			Status:    s.openStatus(),
			Type:      rType,
			Assignee:  assignee,
			CreatedAt: now.Add(-time.Duration(s.rng.Int64N(int64(72 * time.Hour)))),
			Labels:    s.pickLabels(labels, 2),
			Metadata: map[string]string{
				"gc.routed_to": assignee,
				"env":          "production",
			},
		}
		created, err := a.Create(ctx, r)
		if err != nil {
			return result, fmt.Errorf("seeding main open %d: %w", i, err)
		}
		result.MainOpenIDs = append(result.MainOpenIDs, created.ID)
	}

	// Create closed main-tier records (dead weight — tests index selectivity).
	for i := range wl.MainClosedCount {
		r := Record{
			Title:     fmt.Sprintf("closed-main-%d", i),
			Status:    "closed",
			Type:      "task",
			Assignee:  MailAssignees[s.rng.IntN(len(MailAssignees))],
			CreatedAt: now.Add(-time.Duration(s.rng.Int64N(int64(30 * 24 * time.Hour)))),
		}
		created, err := a.Create(ctx, r)
		if err != nil {
			return result, fmt.Errorf("seeding main closed %d: %w", i, err)
		}
		result.MainClosedIDs = append(result.MainClosedIDs, created.ID)
	}

	// Create open ephemeral records (wisps: mail messages + order-tracking).
	for i := range wl.WispOpenCount {
		wType := "message"
		if s.rng.Float32() < 0.37 { // ~37% order-tracking (3500 of 6400 wisps per S2)
			wType = "order-tracking"
		}
		assignee := MailAssignees[s.rng.IntN(len(MailAssignees))]
		ttl := time.Duration(0)
		var expiresAt time.Time
		if wType == "order-tracking" {
			ttl = 24 * time.Hour
			expiresAt = now.Add(ttl - time.Duration(s.rng.Int64N(int64(ttl))))
		}
		r := Record{
			Title:     fmt.Sprintf("wisp-%d", i),
			Status:    "open",
			Type:      wType,
			Assignee:  assignee,
			CreatedAt: now.Add(-time.Duration(s.rng.Int64N(int64(24 * time.Hour)))),
			Ephemeral: true,
			ExpiresAt: expiresAt,
		}
		created, err := a.Create(ctx, r)
		if err != nil {
			return result, fmt.Errorf("seeding wisp %d: %w", i, err)
		}
		result.WispOpenIDs = append(result.WispOpenIDs, created.ID)
	}

	// Create dependency edges between open main-tier records.
	openCount := len(result.MainOpenIDs)
	for i := range wl.DepEdgeCount {
		if openCount < 2 {
			break
		}
		fromIdx := s.rng.IntN(openCount)
		toIdx := s.rng.IntN(openCount)
		if fromIdx == toIdx {
			toIdx = (toIdx + 1) % openCount
		}
		fromID := result.MainOpenIDs[fromIdx]
		toID := result.MainOpenIDs[toIdx]
		if err := a.DepAdd(ctx, fromID, toID, "blocks"); err != nil {
			return result, fmt.Errorf("seeding dep %d: %w", i, err)
		}
		result.DepEdges = append(result.DepEdges, Dep{FromID: fromID, ToID: toID, DepType: "blocks"})
	}

	return result, nil
}

func (s *Seeder) openStatus() string {
	if s.rng.Float32() < 0.3 {
		return "in_progress"
	}
	return "open"
}

func (s *Seeder) pickLabels(pool []string, maxN int) []string {
	n := s.rng.IntN(maxN + 1)
	if n == 0 || len(pool) == 0 {
		return nil
	}
	perm := s.rng.Perm(len(pool))
	if n > len(perm) {
		n = len(perm)
	}
	out := make([]string, n)
	for i := range n {
		out[i] = pool[perm[i]]
	}
	return out
}
