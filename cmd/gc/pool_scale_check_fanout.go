package main

import "github.com/gastownhall/gascity/internal/config"

// poolStoreProbe is one (store, dir, env) leg of a city-scoped agent's
// custom scale_check fan-out across the city store and its non-suspended
// rig stores.
type poolStoreProbe struct {
	ref string
	dir string
	env map[string]string
}

// cityScopedFanOutProbes builds the probe list for a city-scoped agent's
// custom scale_check fan-out: the agent's own (city) probe plus one probe
// per non-suspended rig, mirroring activeStores' suspended-rig filter.
// Not yet implemented -- TDD RED (ga-drb140).
func cityScopedFanOutProbes(_ string, _ *config.City, _ *config.Agent, _ string, _ map[string]string, _ map[string]bool) []poolStoreProbe {
	return nil
}

// evaluatePoolFanOutSum runs sp.Check via runner against every probe
// concurrently, sharing the caller's own sem (never a nested semaphore), and
// sums the parsed per-probe counts -- clamping the aggregate once when
// newDemand is false, mirroring evaluatePool/evaluatePoolNewDemand's
// single-store clamp semantics applied to the summed total instead of one
// value. Not yet implemented -- TDD RED (ga-drb140).
func evaluatePoolFanOutSum(_ string, _ scaleParams, _ []poolStoreProbe, _ ScaleCheckRunner, _ chan struct{}, _ bool) (int, []error) {
	return 0, nil
}
