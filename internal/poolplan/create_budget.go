// Package poolplan contains pure planning policy for agent pools.
package poolplan

import "sync"

// Demand describes one pool template's fresh-session demand. Each template
// must appear at most once, and slice order defines seed-rotation order.
type Demand struct {
	Template     string
	FreshCreates int
	// FreshPriorities carries one priority band per fresh create. Missing
	// entries default to P2; callers should preserve request order.
	FreshPriorities []int
	// RecoveryCreates preserves already-committed capacity ahead of fresh
	// admission. Recovery still participates in seed rotation with peers.
	RecoveryCreates int
	// HasFloor reports that at least one fresh create satisfies a configured
	// floor. It reserves at most one floor token; remaining demand participates
	// in the general round-robin rather than the elastic reserve.
	HasFloor bool
}

// CreateBudget coordinates a shared limit across concurrent pool creates.
type CreateBudget struct {
	mu                sync.Mutex
	remaining         int
	templateRemaining map[string]int
	spare             int
}

// NewCreateBudget returns a budget with limit tokens. A non-positive limit
// disables budgeting; methods on the returned nil budget preserve unlimited
// behavior.
func NewCreateBudget(limit int) *CreateBudget {
	if limit <= 0 {
		return nil
	}
	return &CreateBudget{remaining: limit}
}

// ConfigureFairShare reserves the remaining tokens across unique, ordered pool
// templates. Seed rotation prevents stable input order from starving the same
// template on every planning cycle. ConfigureFairShare must run before claims;
// reconfiguration distributes only the capacity that remains.
func (b *CreateBudget) ConfigureFairShare(demands []Demand, seed uint64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.templateRemaining, b.spare = fairShares(demands, b.remaining, seed)
}

// TryClaim atomically claims a token assigned to template or an unassigned
// spare token.
func (b *CreateBudget) TryClaim(template string) bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.remaining <= 0 {
		return false
	}
	if b.templateRemaining != nil {
		switch {
		case b.templateRemaining[template] > 0:
			b.templateRemaining[template]--
		case b.spare > 0:
			b.spare--
		default:
			return false
		}
	}
	b.remaining--
	return true
}

// Release refunds one successfully claimed token as fungible capacity reusable
// by any template. Callers must release exactly once per failed claimed create;
// CreateBudget does not guard against over-release.
func (b *CreateBudget) Release() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.remaining++
	if b.templateRemaining != nil {
		b.spare++
	}
}

func fairShares(demands []Demand, limit int, seed uint64) (map[string]int, int) {
	if limit <= 0 {
		return nil, 0
	}
	active := make([]Demand, 0, len(demands))
	for _, demand := range demands {
		if demand.FreshCreates > 0 || demand.RecoveryCreates > 0 {
			active = append(active, demand)
		}
	}
	if len(active) <= 1 {
		return nil, 0
	}

	shares := make(map[string]int, len(active))
	freshGiven := make(map[string]int, len(active))
	remaining := limit
	start := int(seed % uint64(len(active)))

	// Preserve committed capacity first. This is the existing recovery-before-
	// fresh invariant; priority bands never compete with this reservation.
	for remaining > 0 {
		progressed := false
		for offset := 0; offset < len(active) && remaining > 0; offset++ {
			demand := active[(start+offset)%len(active)]
			if shares[demand.Template]-freshGiven[demand.Template] >= demand.RecoveryCreates {
				continue
			}
			shares[demand.Template]++
			remaining--
			progressed = true
		}
		if !progressed {
			break
		}
	}

	// Reserve one quarter of the post-recovery budget for non-floor demand.
	// Floor-bearing templates retain priority while large floor sets cannot
	// starve elastic pools. Fresh budgets below four preserve strict floor-first
	// behavior.
	elasticDemand := 0
	for _, demand := range active {
		if !demand.HasFloor {
			elasticDemand += demand.FreshCreates
		}
	}
	elasticReserve := remaining / 4
	if elasticReserve > elasticDemand {
		elasticReserve = elasticDemand
	}

	// Reserve one token per floor-bearing template in seed-rotated order.
	floorBudget := remaining - elasticReserve
	if floorBudget < 0 {
		floorBudget = 0
	}
	floorUsed := 0
	for offset := 0; offset < len(active); offset++ {
		if floorUsed >= floorBudget {
			break
		}
		demand := active[(start+offset)%len(active)]
		if demand.HasFloor {
			shares[demand.Template]++
			freshGiven[demand.Template]++
			remaining--
			floorUsed++
		}
	}

	// Preserve the existing non-floor elastic reserve, then distribute the
	// remainder. Within each quota, the lowest active priority band wins;
	// seed rotation remains the tie-break among templates in the same band.
	elasticGiven := allocateFreshByPriority(active, shares, freshGiven, remaining, elasticReserve, start, func(d Demand) bool {
		return !d.HasFloor
	})
	remaining -= elasticGiven
	remaining -= allocateFreshByPriority(active, shares, freshGiven, remaining, remaining, start, func(Demand) bool { return true })
	return shares, remaining
}

func allocateFreshByPriority(active []Demand, shares, freshGiven map[string]int, remaining, quota, start int, eligible func(Demand) bool) int {
	allocated := 0
	for remaining > 0 && allocated < quota {
		bestPriority := int(^uint(0) >> 1)
		for _, demand := range active {
			if !eligible(demand) || freshGiven[demand.Template] >= demand.FreshCreates {
				continue
			}
			priority := freshPriorityAt(demand, freshGiven[demand.Template])
			if priority < bestPriority {
				bestPriority = priority
			}
		}
		progressed := false
		for offset := 0; offset < len(active) && remaining > 0 && allocated < quota; offset++ {
			demand := active[(start+offset)%len(active)]
			if !eligible(demand) || freshGiven[demand.Template] >= demand.FreshCreates ||
				freshPriorityAt(demand, freshGiven[demand.Template]) != bestPriority {
				continue
			}
			shares[demand.Template]++
			freshGiven[demand.Template]++
			remaining--
			allocated++
			progressed = true
		}
		if !progressed {
			break
		}
	}
	return allocated
}

func freshPriorityAt(demand Demand, index int) int {
	if index < len(demand.FreshPriorities) {
		return demand.FreshPriorities[index]
	}
	return 2
}
