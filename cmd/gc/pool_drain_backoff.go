package main

import (
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
)

// Drain-without-work circuit-breaker.
//
// This detector measures the spawn-storm pathology directly: count
// drain-acked sessions that exited without claiming any work, per pool,
// per rolling window. If that count crosses a threshold AND no claims
// happened pool-wide in the same window (the Q6 rate-condition),
// suppress new spawns with exponential backoff.
//
// It is a supplement to per-bead positive-signal detection: both share
// cooldown surface, but this one is robust across worker-pool model
// changes because it observes worker outcomes (drained without claiming),
// not supervisor model preconditions.
//
// State is in-memory only; lost on controller restart, by design.
const (
	drainBackoffWindow    = 5 * time.Minute
	drainBackoffThreshold = 3
	drainBackoffBase      = 30 * time.Second
	// drainBackoffMaxStreak guards against int64 overflow on the bit
	// shift in backoffDurationForStreak; the schedule effectively caps
	// at drainBackoffCap long before this clamp matters.
	drainBackoffMaxStreak = 30
	drainBackoffCap       = 5 * time.Minute
)

// poolBackoffStats tracks zero-claim drain-acks and active backoff state
// for a single pool (agent template).
type poolBackoffStats struct {
	zeroClaimDrains []time.Time // sliding window of zero-claim drain-ack timestamps
	backoffStreak   int         // consecutive backoff escalations (0 = inactive or just reset)
	backoffUntil    time.Time   // backoff active when now < backoffUntil
}

// poolDrainBackoff is the long-lived detector state, one instance per
// reconciler. Add it to CityRuntime alongside sessionDrains.
type poolDrainBackoff struct {
	mu      sync.Mutex
	perPool map[string]*poolBackoffStats
	clk     clock.Clock
}

func newPoolDrainBackoff(clk clock.Clock) *poolDrainBackoff {
	if clk == nil {
		clk = clock.Real{}
	}
	return &poolDrainBackoff{
		perPool: make(map[string]*poolBackoffStats),
		clk:     clk,
	}
}

// RecordDrainAck records a drain-ack observation for a pool template.
// hadClaims=true means the session claimed at least one work bead during
// its lifetime; hadClaims=false is the zero-claim drain-ack the detector
// is watching for.
//
// Stale entries outside the window are pruned eagerly here so the per-pool
// slice stays bounded by threshold + churn during a single window.
func (p *poolDrainBackoff) RecordDrainAck(template string, hadClaims bool) {
	if p == nil || template == "" || hadClaims {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	stats := p.statsLocked(template)
	now := p.clk.Now()
	stats.zeroClaimDrains = pruneStale(stats.zeroClaimDrains, now.Add(-drainBackoffWindow))
	stats.zeroClaimDrains = append(stats.zeroClaimDrains, now)
}

// NoteClaim records that a claim was observed for this pool. Successful
// claims reset the backoff streak — the pool is healthy enough to make
// progress, so the detector should not stack escalations.
func (p *poolDrainBackoff) NoteClaim(template string) {
	if p == nil || template == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	stats := p.statsLocked(template)
	stats.backoffStreak = 0
	stats.backoffUntil = time.Time{}
	stats.zeroClaimDrains = nil
}

// Evaluate returns whether new spawns should be suppressed for this
// template right now, and the time at which suppression ends.
//
// hasRecentPoolClaim is the Q6 rate-condition: when true, the pool had at
// least one claim in the recent window pool-wide, so the detector treats
// the zero-claim drain-acks as race-loser noise and does not fire. Callers
// must compute this from the bead store at evaluation time.
func (p *poolDrainBackoff) Evaluate(template string, hasRecentPoolClaim bool) (suppress bool, until time.Time) {
	if p == nil || template == "" {
		return false, time.Time{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	stats := p.statsLocked(template)
	now := p.clk.Now()

	// If a claim happened pool-wide, reset state — the pool is healthy.
	if hasRecentPoolClaim {
		stats.backoffStreak = 0
		stats.backoffUntil = time.Time{}
		stats.zeroClaimDrains = nil
		return false, time.Time{}
	}

	stats.zeroClaimDrains = pruneStale(stats.zeroClaimDrains, now.Add(-drainBackoffWindow))

	// Already in active backoff window.
	if !stats.backoffUntil.IsZero() && now.Before(stats.backoffUntil) {
		return true, stats.backoffUntil
	}

	// Threshold check: enough zero-claim drain-acks in window to escalate?
	if len(stats.zeroClaimDrains) < drainBackoffThreshold {
		return false, time.Time{}
	}

	// Escalate: increment streak, set new backoffUntil.
	stats.backoffStreak++
	stats.backoffUntil = now.Add(backoffDurationForStreak(stats.backoffStreak))
	return true, stats.backoffUntil
}

// statsLocked returns the per-template stats, allocating on first use.
// Caller must hold p.mu.
func (p *poolDrainBackoff) statsLocked(template string) *poolBackoffStats {
	stats := p.perPool[template]
	if stats == nil {
		stats = &poolBackoffStats{}
		p.perPool[template] = stats
	}
	return stats
}

// backoffDurationForStreak returns the backoff window for the given
// escalation streak. Streak 1 = base, streak 2 = 2*base, ... capped at
// drainBackoffCap. Mirrors the per-bead detector's exponential schedule
// so the two share a familiar shape.
func backoffDurationForStreak(streak int) time.Duration {
	if streak <= 0 {
		return 0
	}
	if streak > drainBackoffMaxStreak {
		streak = drainBackoffMaxStreak
	}
	d := drainBackoffBase << (streak - 1)
	if d <= 0 || d > drainBackoffCap {
		return drainBackoffCap
	}
	return d
}

// pruneStale drops timestamps strictly older than threshold, returning the
// retained suffix without copying when no pruning is needed.
func pruneStale(ts []time.Time, threshold time.Time) []time.Time {
	cut := 0
	for cut < len(ts) && ts[cut].Before(threshold) {
		cut++
	}
	if cut == 0 {
		return ts
	}
	return ts[cut:]
}
