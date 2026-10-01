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
// rig stores. err is set when the leg's own store env could not be resolved:
// the leg is then reported and never run, because running it under another
// store's env would read that store instead.
type poolStoreProbe struct {
	ref string
	dir string
	env map[string]string
	err error
}

// cityScopedFanOutProbes builds the probe list for a city-scoped agent's
// custom scale_check fan-out: the agent's own (city) probe plus one probe
// per non-suspended rig, mirroring activeStores' suspended-rig filter in
// buildDesiredStateWithSessionBeads. The city probe keeps ownEnv unchanged.
// Each rig probe is bound to its own store through a per-rig view of the
// agent -- the same view appendOneRigHookStore gives the claim side, so a
// scale_check leg and a work_query leg for one rig share one store binding.
// The working directory alone cannot do that: BEADS_DIR beats it, so a probe
// that inherited the city's env would change into the rig and still read the
// city store. A rig whose env cannot be resolved yields a probe carrying that
// error, never the city's env.
func cityScopedFanOutProbes(cityPath string, cfg *config.City, a *config.Agent, ownDir string, ownEnv map[string]string, suspendedRigPaths map[string]bool) []poolStoreProbe {
	probes := []poolStoreProbe{{ref: "city", dir: ownDir, env: ownEnv}}
	if cfg == nil || a == nil {
		return probes
	}
	for _, rig := range cfg.Rigs {
		if suspendedRigPaths[filepath.Clean(rig.Path)] {
			continue
		}
		view := *a
		view.Dir = rig.Name
		env, err := controllerWorkQueryEnv(cityPath, cfg, &view)
		probes = append(probes, poolStoreProbe{ref: rig.Name, dir: resolveAgentDirPath(cityPath, rig.Path), env: env, err: err})
	}
	return probes
}

// evaluatePoolFanOutSum runs sp.Check via runner against every probe
// concurrently, sharing the caller's own sem (never a nested semaphore), and
// sums the parsed per-probe counts -- clamping the aggregate once when
// newDemand is false, mirroring evaluatePool/evaluatePoolNewDemand's
// single-store clamp semantics applied to the summed total instead of one
// value. sp.Check is the bare operator command: each probe gets its own
// GC_DOLT_HOST/GC_DOLT_PORT prefix derived from that probe's env, because a
// command-line assignment beats the subprocess environment and one prefix
// shared by every probe would re-point them all at a single Dolt endpoint. A
// probe error -- its store env unresolved, its command failing, or its output
// not parsing -- contributes 0 to the sum (best-effort, matching
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
			if probe.err != nil {
				telemetry.RecordPoolCheck(context.Background(), agentName, 0, 0, probe.err)
				errs[i] = fmt.Errorf("%s: %w", probe.ref, probe.err)
				return
			}
			sem <- struct{}{}
			defer func() { <-sem }()

			start := time.Now()
			out, err := runner(prefixShellEnv(controllerQueryPrefixEnv(probe.env), sp.Check), probe.dir, probe.env)
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
