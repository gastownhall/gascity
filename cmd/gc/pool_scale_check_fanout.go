package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/telemetry"
)

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
// per non-suspended rig, mirroring activeStores' suspended-rig filter in
// buildDesiredStateWithSessionBeads. ownEnv is reused unchanged for every
// probe: controllerQueryRuntimeEnv resolves to the city's bd runtime env for
// any city-scoped agent regardless of which store's directory a probe
// happens to run in, so there is no per-rig env to compute here — the probe
// command's working directory (dir) is what selects the store.
func cityScopedFanOutProbes(cityPath string, cfg *config.City, _ *config.Agent, ownDir string, ownEnv map[string]string, suspendedRigPaths map[string]bool) []poolStoreProbe {
	probes := []poolStoreProbe{{ref: "city", dir: ownDir, env: ownEnv}}
	if cfg == nil {
		return probes
	}
	for _, rig := range cfg.Rigs {
		if suspendedRigPaths[filepath.Clean(rig.Path)] {
			continue
		}
		probes = append(probes, poolStoreProbe{ref: rig.Name, dir: resolveAgentDirPath(cityPath, rig.Path), env: ownEnv})
	}
	return probes
}

// evaluatePoolFanOutSum runs sp.Check via runner against every probe
// concurrently, sharing the caller's own sem (never a nested semaphore), and
// sums the parsed per-probe counts -- clamping the aggregate once when
// newDemand is false, mirroring evaluatePool/evaluatePoolNewDemand's
// single-store clamp semantics applied to the summed total instead of one
// value. A probe error contributes 0 to the sum (best-effort, matching
// bestStoreWithWork's federation contract) and is returned for the caller to
// log; it never poisons the other probes' counts.
func evaluatePoolFanOutSum(agentName string, sp scaleParams, probes []poolStoreProbe, runner ScaleCheckRunner, sem chan struct{}, newDemand bool) (int, []error) {
	counts := make([]int, len(probes))
	errs := make([]error, len(probes))
	var wg sync.WaitGroup
	for i, probe := range probes {
		wg.Add(1)
		go func(i int, probe poolStoreProbe) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			start := time.Now()
			out, err := runner(sp.Check, probe.dir, probe.env)
			durationMs := float64(time.Since(start).Milliseconds())
			if err != nil {
				telemetry.RecordPoolCheck(context.Background(), agentName, durationMs, 0, err)
				errs[i] = fmt.Errorf("%s: %w", probe.ref, err)
				return
			}
			n, err := parseScaleCheckCount(agentName, sp.Check, out)
			if err != nil {
				telemetry.RecordPoolCheck(context.Background(), agentName, durationMs, 0, err)
				errs[i] = fmt.Errorf("%s: %w", probe.ref, err)
				return
			}
			telemetry.RecordPoolCheck(context.Background(), agentName, durationMs, n, nil)
			counts[i] = n
		}(i, probe)
	}
	wg.Wait()

	sum := 0
	var outErrs []error
	for i, n := range counts {
		sum += n
		if errs[i] != nil {
			outErrs = append(outErrs, errs[i])
		}
	}
	if !newDemand {
		if sum < sp.Min {
			sum = sp.Min
		}
		if sp.Max >= 0 && sum > sp.Max {
			sum = sp.Max
		}
	}
	return sum, outErrs
}
