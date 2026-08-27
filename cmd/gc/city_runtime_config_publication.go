package main

import (
	"sync/atomic"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// publishRuntimeConfig keeps the loop-owned and API-visible runtime snapshots
// on the same accepted revision. A controller-state rejection means a newer
// durable config won while this reload was preparing, so none of the loop's
// core config/provider pointers may advance to the stale candidate.
func (cr *CityRuntime) publishRuntimeConfig(
	cfg *config.City,
	sp runtime.Provider,
	dops drainOps,
	revision string,
) bool {
	if cr == nil {
		return false
	}
	if cr.cs != nil && !cr.cs.updateFromRuntime(cfg, sp, revision) {
		return false
	}

	cr.serviceStateMu.Lock()
	cr.cfg = cfg
	cr.sp = sp
	cr.dops = dops
	cr.serviceStateMu.Unlock()
	cr.demandSnapshot = nil
	return true
}

func (cr *CityRuntime) requestConfigReloadRetry() {
	if cr == nil {
		return
	}
	cr.markConfigReloadDirty()
	select {
	case cr.pokeCh <- struct{}{}:
	default:
	}
}

func (cr *CityRuntime) markConfigReloadDirty() {
	if cr == nil {
		return
	}
	cr.reloadMu.Lock()
	if cr.configDirty == nil {
		cr.configDirty = &atomic.Bool{}
	}
	cr.configDirty.Store(true)
	cr.reloadMu.Unlock()
}
