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
// buildDesiredStateWithSessionBeads. The probe env, not the working
// directory, selects the store: bd honors BEADS_DIR over cwd discovery, so
// each managed rig probe gets its own rig runtime env (BEADS_DIR=<rig>/.beads
// plus that rig's Dolt coordinates). When ownEnv is nil (named-session path
// or an unmanaged city scope) every probe keeps a nil env and bd discovers
// the store from dir, matching the single-store behavior on that path; an
// unmanaged rig likewise gets a nil env. A rig whose env cannot be built is
// skipped and its error returned for the caller to log. The rig env is built
// without managed-dolt recovery because this runs every tick (ga-cdmx6x).
func cityScopedFanOutProbes(cityPath string, cfg *config.City, _ *config.Agent, ownDir string, ownEnv map[string]string, suspendedRigPaths map[string]bool) ([]poolStoreProbe, []error) {
	probes := []poolStoreProbe{{ref: "city", dir: ownDir, env: ownEnv}}
	if cfg == nil {
		return probes, nil
	}
	var errs []error
	for _, rig := range cfg.Rigs {
		if suspendedRigPaths[filepath.Clean(rig.Path)] {
			continue
		}
		rigRoot := resolveAgentDirPath(cityPath, rig.Path)
		var rigEnv map[string]string
		if ownEnv != nil && scopeUsesManagedBdStoreContract(cityPath, rigRoot) {
			env, err := bdRuntimeEnvForRigWithErrorNoRecovery(cityPath, cfg, rigRoot)
			if err != nil {
				errs = append(errs, fmt.Errorf("rig %s: %w", rig.Name, err))
				continue
			}
			rigEnv = env
		}
		probes = append(probes, poolStoreProbe{ref: rig.Name, dir: rigRoot, env: rigEnv})
	}
	return probes, errs
}

// evaluatePoolFanOutSum runs sp.Check via runner against every probe
// concurrently, prefixing the check with each probe's own Dolt connection
// coordinates (sp.Check must arrive unprefixed), sharing the caller's own sem (never a nested semaphore), and
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
			check := prefixShellEnv(controllerQueryPrefixEnv(probe.env), sp.Check)
			out, err := runner(check, probe.dir, probe.env)
			durationMs := float64(time.Since(start).Milliseconds())
			if err != nil {
				telemetry.RecordPoolCheck(context.Background(), agentName, durationMs, 0, err)
				errs[i] = fmt.Errorf("%s: %w", probe.ref, err)
				return
			}
			n, err := parseScaleCheckCount(agentName, check, out)
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
