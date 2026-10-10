package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// wakeVerdictDeps are the reads wakeWillNotStart needs beyond the row.
type wakeVerdictDeps struct {
	cfg       *config.City
	cityPath  string
	sessFront *sessionpkg.Store // startup-health episodes
	sp        runtime.Provider  // the sleep policy's capability
	// work and rigStores (opened only when needed) read assigned work, which
	// overrides an idle latch. No work store never refuses on it.
	work      cityWorkStore
	rigStores func() map[string]beads.Store
}

// wakeWillNotStart is CONTRACT v5.9 D8 7(a)'s one will-not-start predicate,
// shared by `gc session wake` and the API wake (ARCH-RESTRUCTURE R3's
// WakeVerdict): why the controller will not start info's session on its
// recorded explicit wake, or "" when it will. info is the row after the wake
// was recorded. It refuses only on gates that outlive the wake; a gate it
// cannot read (a work store, a dynamic provider or capacity gate) never
// refuses. certain is false when it does not refuse but cannot tell: a
// controller pool seat starts only for pool demand, which it cannot see
// (legacy drops a seat without demand whatever its pin).
func wakeWillNotStart(info sessionpkg.Info, d wakeVerdictDeps, now time.Time) (why string, certain bool) {
	if d.cfg == nil {
		return "", true
	}
	agent := sessionWakeResolveAgentInfo(info, d.cfg)
	if agent == nil {
		return fmt.Sprintf("no configured agent runs its template %q", info.Template), true
	}
	pinned := strings.TrimSpace(info.PinAwake) != ""
	controllerPool := (info.PoolManaged || strings.TrimSpace(info.PoolSlot) != "") &&
		!isManualSessionInfoForAgent(info, agent) && !isNamedSessionInfo(info)
	certain = !controllerPool
	switch scope, name, suspended := agentSuspensionCauseWith(d.cfg, d.cityPath, agent, loadSuspensionStateBestEffort(d.cityPath)); {
	case info.DependencyOnly:
		return "it is a dependency-only session; it starts with its dependents", true
	case suspended && scope == "city":
		return "the city is suspended; run `gc resume`", true
	case suspended:
		return fmt.Sprintf("%s %q is suspended; run `gc %s resume %s`", scope, name, scope, name), true
	case sessionpkg.DemandOnlySingletonWakeRefused(d.cfg, agent, info):
		return sessionpkg.DemandOnlySingletonExplanation(agent.QualifiedName()), true
	case controllerPool && isDrainedSessionInfo(info):
		return "it is a drained pool seat; the pool restarts seats only for demand", true
	case sessionWakeCreateAbandonedInfo(info, d.cfg.Session.StartupTimeoutDuration()):
		return "its create never completed; use `gc session close` to release the slot", true
	case info.SessionCircuitState == sessionpkg.SessionCircuitStateOpen:
		return fmt.Sprintf("its respawn circuit breaker is open; `gc session reset %s` clears it", info.ID), true
	}
	if name := info.SessionNameMetadata; name != "" && d.sessFront != nil {
		if ep, err := d.sessFront.LoadStartupHealthEpisode(startupHealthEpisodeKey(info, name)); err == nil && ep.QuarantinedUntil.After(now) {
			return "its startup-health quarantine holds it until " + ep.QuarantinedUntil.UTC().Format(time.RFC3339), true
		}
	}
	policy := resolveSessionSleepPolicyInfo(info, d.cfg, d.sp)
	// The idle latch yields to pin_awake, to assigned work, and (for a
	// non-interactive policy) to pool demand, which this read cannot see.
	if pinned || d.work.store == nil || policy.Class == config.SessionSleepNonInteractive ||
		!configWakeSuppressedInfo(info, policy, d.sp, clock.Real{}) {
		return "", certain
	}
	legs := workLegsFromCensus(d.cityPath, d.cfg, d.work, d.rigStores())
	if work, err := sessionHasAwakeAssignedWorkForReachableStore(seatWorkFor(legs, releaseScope(info, d.cfg), 0), info); err != nil || work {
		return "", certain
	}
	return "its idle sleep policy holds it asleep until work or demand wakes it; send with `resume: true` (or `gc session attach`) to start it now", true
}

// WakeStartRefusal is api.WakeStartRefuser over wakeWillNotStart.
func (cs *controllerState) WakeStartRefusal(info sessionpkg.Info) (string, bool) {
	return wakeWillNotStart(info, wakeVerdictDeps{
		cfg: cs.Config(), cityPath: cs.CityPath(), sessFront: sessionpkg.NewStore(cs.SessionsBeadStore()),
		sp: cs.SessionProvider(), work: cityWorkStoreOf(cs.CityBeadStore()), rigStores: cs.wakeRigStores,
	}, time.Now())
}

// wakeRigStores is the controller's rig stores without the city's, as
// CityRuntime.rigBeadStores hands the reconciler's assigned-work read.
//
// residency:allow — returns a copy of the controller's own snapshot; it opens
// no store and resolves no bead.
func (cs *controllerState) wakeRigStores() map[string]beads.Store {
	stores := cs.BeadStores() // residency:allow — the controller's own snapshot, handed to workLegsFromCensus, which plans the legs
	delete(stores, cs.cityName)
	return stores
}

var _ api.WakeStartRefuser = (*controllerState)(nil)
