package main

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/events"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

const (
	// poolSpawnChurnThreshold is the number of consecutive "blind" pool
	// spawns (a session created for scale_check-only demand with no
	// identified candidate WorkBeadID, per computePoolDesiredStates' new-tier
	// loop) that must be observed to claim zero work before the breaker
	// suppresses further blind spawns for that template (ra-co9epr).
	// Measured overnight: 46 real sessions in 18.5 minutes on the novices
	// pool, 0% useful, at roughly one every 24s. Three keeps the bound tight
	// (under two minutes of churn) while not tripping on a single unlucky
	// spawn racing a real claim.
	poolSpawnChurnThreshold = 3
	// poolSpawnChurnWindow bounds how long a streak of churned spawns may
	// span before it is treated as stale and restarted at 1 — the same
	// per-window-streak shape as drainAckAssignedWorkCycleWindow
	// (redispatch_cap.go), so an occasional lone blind spawn spread across a
	// healthy pool's life never accumulates toward the threshold.
	poolSpawnChurnWindow = 5 * time.Minute
	// poolSpawnChurnCooldown is how long blind spawns stay suppressed for a
	// template once the breaker trips. Long enough that a genuinely-stuck
	// misroute stops burning sessions for a meaningful stretch; short enough
	// that once the underlying cause clears (bead re-routed, held, or
	// closed), the pool resumes elastic scale-up on its own without a
	// restart.
	poolSpawnChurnCooldown = 5 * time.Minute
)

// poolSpawnChurnState is one template's in-flight churn streak. It lives
// only in memory (like detachedProbeErrorCounts and
// poolSessionCreateFairShareCounter) for the lifetime of the gc process:
// the reconciler runs as a long-lived daemon, so a restart naturally clears
// the breaker along with every other in-flight reconcile decision, and there
// is no bead-level home for a per-template (not per-bead) counter.
type poolSpawnChurnState struct {
	consecutive   int
	lastAt        time.Time
	cooldownUntil time.Time
}

var poolSpawnChurnBreaker = struct {
	sync.Mutex
	byTemplate map[string]*poolSpawnChurnState
}{byTemplate: make(map[string]*poolSpawnChurnState)}

// recordPoolSpawnChurn registers one observed "blind pool spawn claimed no
// work" occurrence for template and returns whether this exact observation
// is the one that tripped the cooldown (so the caller emits
// PoolSpawnChurnCoolingDown exactly once per trip, not on every subsequent
// churned spawn while already cooling down), the current consecutive streak,
// and the cooldown deadline. A gap longer than poolSpawnChurnWindow since the
// last observation restarts the streak at 1 rather than accumulating, so a
// rare isolated blind spawn on an otherwise-healthy pool never trips the
// breaker.
func recordPoolSpawnChurn(template string, now time.Time) (tripped bool, consecutive int, cooldownUntil time.Time) {
	template = strings.TrimSpace(template)
	if template == "" {
		return false, 0, time.Time{}
	}
	poolSpawnChurnBreaker.Lock()
	defer poolSpawnChurnBreaker.Unlock()
	st := poolSpawnChurnBreaker.byTemplate[template]
	if st == nil {
		st = &poolSpawnChurnState{}
		poolSpawnChurnBreaker.byTemplate[template] = st
	}
	if !st.lastAt.IsZero() && now.Sub(st.lastAt) > poolSpawnChurnWindow {
		st.consecutive = 0
	}
	st.consecutive++
	st.lastAt = now
	if st.consecutive >= poolSpawnChurnThreshold {
		tripped = st.consecutive == poolSpawnChurnThreshold
		st.cooldownUntil = now.Add(poolSpawnChurnCooldown)
	}
	return tripped, st.consecutive, st.cooldownUntil
}

// poolSpawnChurnCooldownTemplates returns the set of templates currently
// cooling down — recordPoolSpawnChurn tripped their breaker and the cooldown
// has not yet elapsed as of now. computePoolDesiredStates' new-demand loop
// consults this (via ComputePoolDesiredStatesWithDemandChurnTraced's
// churnCooldownTemplates parameter) to stop materializing blind ("new" tier,
// no identified WorkBeadID) requests for a cooling-down template. A nil/empty
// result (the common case) means every template is clear.
func poolSpawnChurnCooldownTemplates(now time.Time) map[string]bool {
	poolSpawnChurnBreaker.Lock()
	defer poolSpawnChurnBreaker.Unlock()
	var out map[string]bool
	for template, st := range poolSpawnChurnBreaker.byTemplate {
		if !st.cooldownUntil.IsZero() && now.Before(st.cooldownUntil) {
			if out == nil {
				out = make(map[string]bool, len(poolSpawnChurnBreaker.byTemplate))
			}
			out[template] = true
		}
	}
	return out
}

// resetPoolSpawnChurnBreakerForTest clears every template's churn state.
// Tests that exercise recordPoolSpawnChurn/poolSpawnChurnCooldownTemplates
// must call this first — the breaker is process-lifetime global state
// (mirrors resetDetachedProbeErrorCountsForTest), so a prior test's trip
// would otherwise leak into the next one.
func resetPoolSpawnChurnBreakerForTest() {
	poolSpawnChurnBreaker.Lock()
	defer poolSpawnChurnBreaker.Unlock()
	poolSpawnChurnBreaker.byTemplate = make(map[string]*poolSpawnChurnState)
}

// recordPoolSpawnChurnForClosedSession is the ra-co9epr spawn-churn-breaker
// hook. finalizeDrainAckStoppedSession calls this whenever a pool session
// bead closes via the "drained, no assigned work" path — the drain-ack
// completed and the close-gate found nothing assigned, so the session is
// closing itself as done. When that session was pool-managed and never once
// bound to a trigger work bead (info.TriggerBeadID empty for its entire
// life), this is exactly the churn signature measured overnight: a session
// spawned for scale_check-only demand that no hook could ever hand it,
// living 16-28s before self-draining as "no_work". A session that DID bind a
// trigger bead at some point (even if that work has since closed and it is
// now legitimately idle) is not churn and must not count against the
// breaker — computePoolTriggerBindingPatch only clears TriggerBeadID on an
// explicit reassignment to no bead, so a completed real task still carries
// its last TriggerBeadID here.
func recordPoolSpawnChurnForClosedSession(template string, info sessionpkg.Info, clk clock.Clock, rec events.Recorder) {
	template = strings.TrimSpace(template)
	if template == "" || !isPoolManagedSessionInfo(info) {
		return
	}
	if strings.TrimSpace(info.TriggerBeadID) != "" {
		return
	}
	if clk == nil {
		clk = clock.Real{}
	}
	now := clk.Now().UTC()
	tripped, consecutive, cooldownUntil := recordPoolSpawnChurn(template, now)
	if !tripped || rec == nil {
		return
	}
	rec.Record(events.Event{
		Type:    events.PoolSpawnChurnCoolingDown,
		Ts:      now,
		Actor:   "gc",
		Subject: template,
		Message: fmt.Sprintf("suppressing blind pool spawns for %s until %s: %d consecutive sessions claimed no work",
			template, cooldownUntil.Format(time.RFC3339), consecutive),
		Payload: api.PoolSpawnChurnCoolingDownPayloadJSON(template, consecutive, cooldownUntil),
	})
}
