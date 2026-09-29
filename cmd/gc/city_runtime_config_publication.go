package main

import (
	"sync/atomic"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// publishRuntimeConfig keeps the loop-owned and API-visible runtime snapshots
// on the same accepted revision. The controller state decides first: a
// rejection means a newer config (an API mutation or an on-disk edit) won
// while this reload was preparing its candidate, so none of the loop's
// config/provider pointers may advance to the stale candidate.
func (cr *CityRuntime) publishRuntimeConfig(
	cfg *config.City,
	sp runtime.Provider,
	dops drainOps,
	revision string,
) bool {
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

// requestConfigReloadRetry leaves a config reload pending for the next tick.
// It deliberately does not poke: whoever superseded the candidate (the config
// watcher or an API mutation) already pokes, and a rejection that repeats on
// the same on-disk revision would otherwise hot-loop reconciliation ticks.
func (cr *CityRuntime) requestConfigReloadRetry() {
	cr.reloadMu.Lock()
	if cr.configDirty == nil {
		cr.configDirty = &atomic.Bool{}
	}
	cr.configDirty.Store(true)
	cr.reloadMu.Unlock()
}
