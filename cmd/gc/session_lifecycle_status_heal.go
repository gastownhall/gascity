package main

import (
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/session"
)

// sessionLifecycleStatusHealContext carries the site-specific runtime evidence
// used both by the observational status plan and the legacy status-heal writer.
type sessionLifecycleStatusHealContext struct {
	Site            sessionLifecycleStatusHealSite
	RuntimeObserved bool
	RuntimeAlive    bool
	// LoadedRevision is the row's revision as of the snapshot the heal decision
	// was computed from. The legacy write fences on it so a concurrent writer
	// that changed the row after the snapshot (a `gc session suspend` landing
	// mid-tick, say) is not silently overwritten by this advisory heal. Zero
	// means "unknown"; see healStateWithRollbackInfo.
	LoadedRevision    int64
	RollbackAvailable bool
}

// applySessionLifecycleStatusHeal keeps the observational status plan, legacy
// write, comparison report, and successful same-tick fold on one synchronous
// path. The legacy write remains authoritative until the rollout gate transfers
// ownership; a parked candidate therefore never suppresses it.
func applySessionLifecycleStatusHeal(
	tick *reconcileTick,
	sessionID string,
	healContext sessionLifecycleStatusHealContext,
	sessFront *session.Store,
	clk clock.Clock,
	startupTimeout time.Duration,
	observer sessionLifecycleStatusComparisonObserver,
) (map[string]string, error) {
	info, ok := tick.infoByID[sessionID]
	if !ok {
		return nil, fmt.Errorf("applying session lifecycle status heal: session %q missing from reconcile tick", sessionID)
	}
	if info.ID != sessionID {
		return nil, fmt.Errorf("applying session lifecycle status heal: requested session ID %q, tick info ID %q", sessionID, info.ID)
	}
	var candidate *sessionLifecycleStatusPlan
	if observer != nil {
		planned := planSessionLifecycleStatus(sessionLifecycleShadowInput{
			Info:              info,
			RuntimeObserved:   healContext.RuntimeObserved,
			RuntimeAlive:      healContext.RuntimeAlive,
			ObservedAt:        clk.Now().UTC(),
			StartupTimeout:    startupTimeout,
			RollbackAvailable: healContext.RollbackAvailable,
		})
		candidate = &planned
	}

	patch, err := healStateWithRollbackInfo(info, healContext.RuntimeAlive, healContext.RuntimeObserved, sessFront, clk, startupTimeout, healContext.RollbackAvailable, healContext.LoadedRevision)
	if candidate != nil {
		observer(compareSessionLifecycleStatus(healContext.Site, *candidate, patch, err))
	}
	if err != nil {
		return nil, err
	}
	tick.apply(sessionID, patch)
	return patch, nil
}
