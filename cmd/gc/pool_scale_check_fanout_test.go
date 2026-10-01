package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// fanOutProbe is a small constructor helper for readable test tables.
func fanOutProbe(ref, dir string) poolStoreProbe {
	return poolStoreProbe{ref: ref, dir: dir, env: nil}
}

// TestEvaluatePoolFanOutSumSumsAcrossProbes is the core AC2/AC5 regression
// for ga-drb140: a city-scoped custom scale_check must SUM genuinely
// multi-valued counts across every probe, not return a single winner.
// Counts are deliberately non-0/1 (2, 3, 5) so a winner-takes-all
// implementation (which would return 5, the max) or a first-hit
// implementation (which would return 2) is distinguishable from the correct
// sum (10) -- a 0/1-shaped fixture would pass under winner-takes-all by
// accident.
func TestEvaluatePoolFanOutSumSumsAcrossProbes(t *testing.T) {
	probes := []poolStoreProbe{fanOutProbe("city", "city"), fanOutProbe("riga", "riga"), fanOutProbe("rigb", "rigb")}
	counts := map[string]string{"city": "2", "riga": "3", "rigb": "5"}
	runner := func(_, dir string, _ map[string]string) (string, error) {
		return counts[dir], nil
	}
	sem := make(chan struct{}, len(probes))
	sp := scaleParams{Min: 0, Max: 100, Check: "check"}

	got, errs := evaluatePoolFanOutSum("agent", sp, probes, runner, sem, true)
	if len(errs) != 0 {
		t.Fatalf("errs = %v, want none", errs)
	}
	if got != 10 {
		t.Fatalf("evaluatePoolFanOutSum sum = %d, want 10 (2+3+5) — a winner-takes-all or "+
			"first-hit implementation would return 5 or 2 instead of summing every store", got)
	}
}

// TestEvaluatePoolFanOutSumClampsAggregateOnce mirrors evaluatePool's
// single-store clamp semantics applied to the AGGREGATE sum, not per probe:
// the min/max clamp is meaningless per-store (a rig legitimately reporting 5
// must not be silently clamped before summing), so it must apply once, after
// summation, exactly like evaluatePool clamps its single result.
func TestEvaluatePoolFanOutSumClampsAggregateOnce(t *testing.T) {
	probes := []poolStoreProbe{fanOutProbe("city", "city"), fanOutProbe("riga", "riga")}
	counts := map[string]string{"city": "4", "riga": "6"}
	runner := func(_, dir string, _ map[string]string) (string, error) {
		return counts[dir], nil
	}
	sem := make(chan struct{}, len(probes))
	sp := scaleParams{Min: 0, Max: 6, Check: "check"}

	got, errs := evaluatePoolFanOutSum("agent", sp, probes, runner, sem, false)
	if len(errs) != 0 {
		t.Fatalf("errs = %v, want none", errs)
	}
	if got != 6 {
		t.Fatalf("evaluatePoolFanOutSum clamped sum = %d, want 6 (4+6=10 clamped to Max once, "+
			"not each probe clamped before summing)", got)
	}
}

// TestEvaluatePoolFanOutSumNewDemandLeavesAggregateUnclamped covers the
// additive/new-demand semantics (mirroring evaluatePoolNewDemand): when
// newDemand is true, the summed total is the raw new-demand count with no
// min/max clamp, matching evaluatePoolNewDemand's own unclamped contract.
func TestEvaluatePoolFanOutSumNewDemandLeavesAggregateUnclamped(t *testing.T) {
	probes := []poolStoreProbe{fanOutProbe("city", "city"), fanOutProbe("riga", "riga")}
	counts := map[string]string{"city": "4", "riga": "6"}
	runner := func(_, dir string, _ map[string]string) (string, error) {
		return counts[dir], nil
	}
	sem := make(chan struct{}, len(probes))
	sp := scaleParams{Min: 0, Max: 6, Check: "check"}

	got, errs := evaluatePoolFanOutSum("agent", sp, probes, runner, sem, true)
	if len(errs) != 0 {
		t.Fatalf("errs = %v, want none", errs)
	}
	if got != 10 {
		t.Fatalf("evaluatePoolFanOutSum new-demand sum = %d, want 10 (unclamped, mirroring "+
			"evaluatePoolNewDemand's own unclamped contract)", got)
	}
}

// TestEvaluatePoolFanOutSumBestEffortOnProbeError pins the federation
// contract already established by bestStoreWithWork and
// defaultScaleCheckCountsAndDemand: one bad store is best-effort, not fatal.
// A single failing rig probe must contribute 0 (not poison the aggregate)
// while its error is still surfaced to the caller for logging.
func TestEvaluatePoolFanOutSumBestEffortOnProbeError(t *testing.T) {
	probes := []poolStoreProbe{fanOutProbe("city", "city"), fanOutProbe("riga", "riga"), fanOutProbe("rigb", "rigb")}
	boom := fmt.Errorf("boom")
	runner := func(_, dir string, _ map[string]string) (string, error) {
		switch dir {
		case "city":
			return "3", nil
		case "riga":
			return "", boom
		default:
			return "4", nil
		}
	}
	sem := make(chan struct{}, len(probes))
	sp := scaleParams{Min: 0, Max: 100, Check: "check"}

	got, errs := evaluatePoolFanOutSum("agent", sp, probes, runner, sem, true)
	if got != 7 {
		t.Fatalf("evaluatePoolFanOutSum sum = %d, want 7 (3+0+4): one failing rig probe must "+
			"contribute 0, not poison the whole aggregate", got)
	}
	if len(errs) != 1 {
		t.Fatalf("errs = %v, want exactly 1 (the riga probe failure surfaced for logging)", errs)
	}
}

// TestEvaluatePoolFanOutSumRunsProbesConcurrently is AC6's concurrency proof:
// a city-scoped agent's fan-out across N stores must run those N probes
// concurrently, not one at a time. Proven with a WaitGroup barrier rather
// than a wall-clock sleep: every runner blocks until all N have started, so
// a sequential (loop-and-call) implementation deadlocks here — a
// deterministic failure instead of a timing-based one.
//
// No time.Sleep: internal/testpolicy/resourcecensus ratchets fixed-sleep
// test calls down, never up (TestRepositoryLedgerMatchesCensusAndDocumentation).
func TestEvaluatePoolFanOutSumRunsProbesConcurrently(t *testing.T) {
	const n = 4
	probes := make([]poolStoreProbe, n)
	for i := range probes {
		probes[i] = fanOutProbe(fmt.Sprintf("store-%d", i), fmt.Sprintf("dir-%d", i))
	}
	var wg sync.WaitGroup
	wg.Add(n)
	runner := func(_, _ string, _ map[string]string) (string, error) { //nolint:unparam // the runner seam returns an error every barrier-synced probe never produces
		wg.Done()
		wg.Wait() // every probe must have started before any of them may finish
		return "1", nil
	}
	sem := make(chan struct{}, n) // large enough for full concurrency
	sp := scaleParams{Min: 0, Max: 100, Check: "check"}

	type fanOutResult struct {
		got  int
		errs []error
	}
	resultCh := make(chan fanOutResult, 1)
	go func() {
		got, errs := evaluatePoolFanOutSum("agent", sp, probes, runner, sem, true)
		resultCh <- fanOutResult{got, errs}
	}()

	select {
	case res := <-resultCh:
		if len(res.errs) != 0 {
			t.Fatalf("errs = %v, want none", res.errs)
		}
		if res.got != n {
			t.Fatalf("sum = %d, want %d", res.got, n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("evaluatePoolFanOutSum did not return — probes are not running concurrently " +
			"(a sequential implementation deadlocks on the barrier: probe 1 waits for probes " +
			"2..N to start, but they are never invoked until probe 1 returns)")
	}
}

// TestEvaluatePoolFanOutSumSharesCallerSemaphoreNotNested is AC1's explicit
// concurrency-sharing constraint: the fan-out must bound its own concurrency
// through the CALLER's existing probe-concurrency semaphore, not a separate
// one sized to the probe count. With a size-1 semaphore standing in for a
// caller-level semaphore already saturated by other pools, this agent's own
// N probes must still serialize through it — proving no hidden/nested
// semaphore silently grants this call extra concurrency the caller never
// authorized. Proven by tracking peak concurrently-active runners through a
// rendezvous handshake rather than a wall-clock sleep, so an over-concurrent
// implementation is caught deterministically instead of by timing
// proportion (see the sibling test above for why no time.Sleep).
func TestEvaluatePoolFanOutSumSharesCallerSemaphoreNotNested(t *testing.T) {
	const n = 3
	probes := make([]poolStoreProbe, n)
	for i := range probes {
		probes[i] = fanOutProbe(fmt.Sprintf("store-%d", i), fmt.Sprintf("dir-%d", i))
	}

	var active, peak int32
	entered := make(chan struct{})
	release := make(chan struct{})
	runner := func(_, _ string, _ map[string]string) (string, error) { //nolint:unparam // the runner seam returns an error every rendezvous-synced probe never produces
		cur := atomic.AddInt32(&active, 1)
		for {
			p := atomic.LoadInt32(&peak)
			if cur <= p || atomic.CompareAndSwapInt32(&peak, p, cur) {
				break
			}
		}
		entered <- struct{}{}
		<-release
		atomic.AddInt32(&active, -1)
		return "1", nil
	}
	sem := make(chan struct{}, 1) // caller-level capacity of 1: already saturated
	sp := scaleParams{Min: 0, Max: 100, Check: "check"}

	type fanOutResult struct {
		got  int
		errs []error
	}
	resultCh := make(chan fanOutResult, 1)
	go func() {
		got, errs := evaluatePoolFanOutSum("agent", sp, probes, runner, sem, true)
		resultCh <- fanOutResult{got, errs}
	}()

	// Release probes one at a time. If a nested/separate semaphore lets a
	// second probe become active before this loop releases the first, its
	// entry (above) already bumped peak > 1 before it could reach `entered`.
	for i := 0; i < n; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for probe %d of %d to start", i+1, n)
		}
		release <- struct{}{}
	}

	var res fanOutResult
	select {
	case res = <-resultCh:
	case <-time.After(5 * time.Second):
		t.Fatal("evaluatePoolFanOutSum did not return after all probes were released")
	}

	if len(res.errs) != 0 {
		t.Fatalf("errs = %v, want none", res.errs)
	}
	if res.got != n {
		t.Fatalf("sum = %d, want %d", res.got, n)
	}
	if got := atomic.LoadInt32(&peak); got > 1 {
		t.Fatalf("peak concurrently-active probes = %d, want 1 — a nested/separate semaphore let "+
			"more than one probe run at once instead of serializing through the caller's size-1 "+
			"shared semaphore", got)
	}
}

// TestCityScopedFanOutProbesIncludesCityAndNonSuspendedRigsOnly is AC1's
// store-set-construction half, mirroring activeStores' own suspended-rig
// filter in buildDesiredStateWithSessionBeads: the probe list must contain
// the agent's own (city) probe plus one probe per non-suspended rig, and
// must exclude a suspended rig entirely -- the same store set activeStores
// already computes for the default scale_check path.
func TestCityScopedFanOutProbesIncludesCityAndNonSuspendedRigsOnly(t *testing.T) {
	cityPath := t.TempDir()
	rigAPath := cityPath + "/riga"
	rigBPath := cityPath + "/rigb"
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "riga", Path: rigAPath},
			{Name: "rigb", Path: rigBPath, SuspendedOnStart: true},
		},
	}
	agentCfg := &config.Agent{Name: "worker"}
	ownEnv := map[string]string{"OWN": "1"}
	suspended := map[string]bool{rigBPath: true}

	probes := cityScopedFanOutProbes(cityPath, cfg, agentCfg, cityPath, ownEnv, suspended)

	if len(probes) != 2 {
		t.Fatalf("len(probes) = %d, want 2 (city + riga only; rigb is suspended): %+v", len(probes), probes)
	}
	if probes[0].ref != "city" || probes[0].dir != cityPath {
		t.Fatalf("probes[0] = %+v, want the agent's own city probe (dir=%q)", probes[0], cityPath)
	}
	foundRigA := false
	for _, p := range probes {
		if p.ref == "rigb" {
			t.Fatalf("probes contains a probe for suspended rig rigb: %+v", probes)
		}
		if p.ref == "riga" {
			foundRigA = true
		}
	}
	if !foundRigA {
		t.Fatalf("probes missing non-suspended rig riga: %+v", probes)
	}
}

// Per-store demand and Dolt ports for the store-binding fixture below. The
// counts are distinct powers of two so every subset sum is unique: a probe
// bound to the wrong store moves the total to a value no correct combination
// can produce, and a suspended rig that leaks into the fan-out is visible as
// its own bit.
const (
	fanOutCityCount   = 1
	fanOutRigACount   = 2
	fanOutRigBCount   = 4
	fanOutParkedCount = 8
	fanOutBrokenCount = 16

	fanOutRigAPort   = "4406"
	fanOutRigBPort   = "4407"
	fanOutParkedPort = "4408"

	// fanOutBeadsDirCheck is a custom scale_check that reports the demand of
	// whichever store its probe is bound to, read through BEADS_DIR. The sh -c
	// wrapper keeps it valid under the GC_DOLT_* assignments prefixShellEnv
	// puts in front of a command.
	fanOutBeadsDirCheck = `sh -c 'cat "$BEADS_DIR/count"'`

	// fanOutRigBoundCheck reads the city's count from the working directory,
	// since a city probe may run without any env of its own, but refuses to
	// guess for a rig: a probe running in a rig (marked by markFanOutRig) must
	// be bound to that rig's store through BEADS_DIR, so a rig probe left
	// without its own env fails instead of quietly reading by working directory.
	fanOutRigBoundCheck = `sh -c 'if [ -e "$PWD/.beads/rig" ]; then cat "$BEADS_DIR/count"; else cat "$PWD/.beads/count"; fi'`
)

// fanOutStoreFixture is a managed-bd city whose city store and rig stores each
// hold a count file in their own .beads directory and answer on their own Dolt
// endpoint, so a custom scale_check that reads "$BEADS_DIR/count" or
// "$GC_DOLT_PORT" reports which store a probe was actually bound to. cfg holds
// one city-scoped agent and three rigs; parked is suspended, so a correct
// fan-out sums city + riga + rigb only.
type fanOutStoreFixture struct {
	cityPath  string
	cityPort  string
	cfg       *config.City
	suspended map[string]bool
}

func newFanOutStoreFixture(t *testing.T) fanOutStoreFixture {
	t.Helper()
	clearGCEnv(t)
	t.Setenv("GC_BEADS", "bd")

	cityPath := t.TempDir()
	writeCanonicalScopeConfig(t, cityPath, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginManagedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	cityPort := writeReachableManagedDoltState(t, cityPath)
	writeFanOutCount(t, cityPath, fanOutCityCount)

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(20),
		}},
	}
	for _, rig := range []struct {
		name, prefix, port string
		count              int
		suspended          bool
	}{
		{"riga", "ra", fanOutRigAPort, fanOutRigACount, false},
		{"rigb", "rb", fanOutRigBPort, fanOutRigBCount, false},
		{"parked", "pk", fanOutParkedPort, fanOutParkedCount, true},
	} {
		rigDir := filepath.Join(cityPath, rig.name)
		writeCanonicalScopeConfig(t, rigDir, contract.ConfigState{
			IssuePrefix:    rig.prefix,
			EndpointOrigin: contract.EndpointOriginExplicit,
			EndpointStatus: contract.EndpointStatusVerified,
			DoltHost:       "rig-db.example.com",
			DoltPort:       rig.port,
			DoltUser:       "rig-user",
		})
		writeFanOutCount(t, rigDir, rig.count)
		markFanOutRig(t, rigDir)
		cfg.Rigs = append(cfg.Rigs, config.Rig{Name: rig.name, Path: rigDir, Prefix: rig.prefix, SuspendedOnStart: rig.suspended})
	}
	return fanOutStoreFixture{
		cityPath:  cityPath,
		cityPort:  strconv.Itoa(cityPort),
		cfg:       cfg,
		suspended: buildSuspendedRigPathsForCity(cfg, cityPath),
	}
}

func writeFanOutCount(t *testing.T, scopeRoot string, count int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(scopeRoot, ".beads", "count"), []byte(strconv.Itoa(count)), 0o644); err != nil {
		t.Fatalf("write %s count: %v", scopeRoot, err)
	}
}

// markFanOutRig drops a marker into a rig's .beads so fanOutRigBoundCheck can
// tell from its working directory alone that it is running in a rig.
func markFanOutRig(t *testing.T, rigDir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "rig"), nil, 0o644); err != nil {
		t.Fatalf("write %s rig marker: %v", rigDir, err)
	}
}

// fanOutProbes builds the probe list the way the generic-pool call site in
// buildDesiredStateWithSessionBeads does: the city-scoped agent's own query
// env is resolved once and handed to cityScopedFanOutProbes.
func (fx fanOutStoreFixture) fanOutProbes(t *testing.T) []poolStoreProbe {
	t.Helper()
	agent := &fx.cfg.Agents[0]
	ownEnv, err := controllerQueryRuntimeEnv(fx.cityPath, fx.cfg, agent)
	if err != nil {
		t.Fatalf("controllerQueryRuntimeEnv(city-scoped agent): %v", err)
	}
	return cityScopedFanOutProbes(fx.cityPath, fx.cfg, agent, fx.cityPath, ownEnv, fx.suspended)
}

// TestCityScopedFanOutProbesBindEachProbeToItsOwnStore is the store-binding
// half of the deployer gate's criterion-2 failure on ga-i7b6sl: a rig probe
// reusing the city's env keeps BEADS_DIR=<city>/.beads, and BEADS_DIR beats
// the working directory, so the probe command changes into the rig but still
// reads the city store. Each rig probe must carry the six store keys gc hook
// varies per leg -- BEADS_DIR, GC_STORE_ROOT, GC_STORE_SCOPE, GC_RIG,
// GC_RIG_ROOT, GC_BEADS_PREFIX -- plus the rig's own Dolt endpoint, exactly as
// the claim side (appendOneRigHookStore) builds them, so a scale_check leg and
// a work_query leg for one rig are bound to the same store.
func TestCityScopedFanOutProbesBindEachProbeToItsOwnStore(t *testing.T) {
	fx := newFanOutStoreFixture(t)

	probes := fx.fanOutProbes(t)

	wantPort := map[string]string{"city": fx.cityPort, "riga": fanOutRigAPort, "rigb": fanOutRigBPort}
	if len(probes) != len(wantPort) {
		t.Fatalf("len(probes) = %d, want %d (city + riga + rigb; parked is suspended): %+v", len(probes), len(wantPort), probes)
	}
	for _, p := range probes {
		if got, want := p.env["GC_DOLT_PORT"], wantPort[p.ref]; got != want {
			t.Errorf("probe %s: GC_DOLT_PORT = %q, want %q (the probe's own Dolt endpoint)", p.ref, got, want)
		}
		if p.ref == "city" {
			if got, want := p.env["BEADS_DIR"], filepath.Join(fx.cityPath, ".beads"); got != want {
				t.Errorf("city probe: BEADS_DIR = %q, want %q", got, want)
			}
			continue
		}
		rigDir := filepath.Join(fx.cityPath, p.ref)
		var prefix string
		for i := range fx.cfg.Rigs {
			if fx.cfg.Rigs[i].Name == p.ref {
				prefix = fx.cfg.Rigs[i].EffectivePrefix()
			}
		}
		for key, want := range map[string]string{
			"BEADS_DIR":       filepath.Join(rigDir, ".beads"),
			"GC_STORE_ROOT":   rigDir,
			"GC_STORE_SCOPE":  "rig",
			"GC_RIG":          p.ref,
			"GC_RIG_ROOT":     rigDir,
			"GC_BEADS_PREFIX": prefix,
		} {
			if got := p.env[key]; got != want {
				t.Errorf("rig probe %s: %s = %q, want %q (the probe's own store, not the city's)", p.ref, key, got, want)
			}
		}
	}
}

// TestEvaluatePoolFanOutSumRealRunnerSumsEachStoresOwnCount drives the real
// shell runner (shellScaleCheck -> runShellCommand, real sh) with distinct
// per-store counts. runShellCommand only changes the working directory, so
// which store a probe reads is decided entirely by the BEADS_DIR in its env:
// two correctly scoped rig probes plus the city must sum to 1+2+4, and the
// suspended rig's 8 must not appear. A fan-out that hands the city env to every
// rig reads the city's count three times and returns 3.
func TestEvaluatePoolFanOutSumRealRunnerSumsEachStoresOwnCount(t *testing.T) {
	fx := newFanOutStoreFixture(t)
	probes := fx.fanOutProbes(t)
	sem := make(chan struct{}, len(probes))
	sp := scaleParams{Min: 0, Max: 100, Check: fanOutBeadsDirCheck}

	got, errs := evaluatePoolFanOutSum("worker", sp, probes, shellScaleCheck, sem, true)

	if len(errs) != 0 {
		t.Fatalf("errs = %v, want none", errs)
	}
	if want := fanOutCityCount + fanOutRigACount + fanOutRigBCount; got != want {
		t.Fatalf("fan-out sum = %d, want %d (city %d + riga %d + rigb %d, suspended rig's %d excluded); "+
			"a sum of %d means every probe read the city store",
			got, want, fanOutCityCount, fanOutRigACount, fanOutRigBCount, fanOutParkedCount, 3*fanOutCityCount)
	}
}

// TestEvaluatePendingPoolsFanOutPrefixesEachProbeWithItsOwnDoltEndpoint is the
// second city-bound setting the gate's "scope-specific transport settings"
// names: evaluatePendingPools prefixes the check command with GC_DOLT_HOST and
// GC_DOLT_PORT from the pool's own env, and a command-line assignment beats
// the subprocess environment. Applied once from the city env, it re-points
// every rig probe at the city's Dolt endpoint no matter what its env says. The
// check reports which endpoint it was prefixed with, so each probe must see its
// own: 1 (city) + 2 (riga) + 4 (rigb), where one shared city prefix gives 3.
func TestEvaluatePendingPoolsFanOutPrefixesEachProbeWithItsOwnDoltEndpoint(t *testing.T) {
	fx := newFanOutStoreFixture(t)
	agent := &fx.cfg.Agents[0]
	ownEnv, err := controllerQueryRuntimeEnv(fx.cityPath, fx.cfg, agent)
	if err != nil {
		t.Fatalf("controllerQueryRuntimeEnv(city-scoped agent): %v", err)
	}
	check := fmt.Sprintf(`sh -c 'case "$GC_DOLT_PORT" in %s) printf %d;; %s) printf %d;; %s) printf %d;; *) printf 0;; esac'`,
		fx.cityPort, fanOutCityCount, fanOutRigAPort, fanOutRigACount, fanOutRigBPort, fanOutRigBCount)
	pending := []poolEvalWork{{
		agentIdx:  0,
		sp:        scaleParams{Min: 0, Max: 100, Check: check},
		poolDir:   fx.cityPath,
		env:       ownEnv,
		newDemand: true,
		probes:    cityScopedFanOutProbes(fx.cityPath, fx.cfg, agent, fx.cityPath, ownEnv, fx.suspended),
	}}

	var stderr strings.Builder
	counts, partials := evaluatePendingPools(fx.cfg, pending, shellScaleCheck, &stderr, nil)

	if partials[0] {
		t.Fatalf("pool reported partial; stderr = %q", stderr.String())
	}
	if want := fanOutCityCount + fanOutRigACount + fanOutRigBCount; counts[0] != want {
		t.Fatalf("pool demand = %d, want %d (each probe prefixed with its own GC_DOLT_PORT); "+
			"a sum of %d means every probe was prefixed with the city's endpoint", counts[0], want, 3*fanOutCityCount)
	}
}

// recordingScaleCheckRunner is the real shell runner plus a record of every
// directory it ran a probe in, so a test can prove a probe never ran at all.
type recordingScaleCheckRunner struct {
	mu   sync.Mutex
	dirs []string
}

func (r *recordingScaleCheckRunner) run(command, dir string, env map[string]string) (string, error) {
	r.mu.Lock()
	r.dirs = append(r.dirs, dir)
	r.mu.Unlock()
	return shellScaleCheck(command, dir, env)
}

// TestCityScopedFanOutRigEnvFailureContributesZeroAndIsNeverRunUnderCityEnv
// pins the best-effort federation contract for a rig whose own store env
// cannot be resolved: the probe contributes 0 and its failure is reported, and
// the check is never executed for it -- in particular not under the city's env,
// which would read the city store a second time and silently inflate the total.
func TestCityScopedFanOutRigEnvFailureContributesZeroAndIsNeverRunUnderCityEnv(t *testing.T) {
	fx := newFanOutStoreFixture(t)
	brokenDir := filepath.Join(fx.cityPath, "broken")
	mustMkdirAll(t, filepath.Join(brokenDir, ".beads"))
	// An explicit endpoint with no host or port is an invalid canonical
	// config: this rig's store env cannot be resolved.
	if err := os.WriteFile(filepath.Join(brokenDir, ".beads", "config.yaml"), []byte(`issue_prefix: bk
gc.endpoint_origin: explicit
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFanOutCount(t, brokenDir, fanOutBrokenCount)
	fx.cfg.Rigs = append(fx.cfg.Rigs, config.Rig{Name: "broken", Path: brokenDir, Prefix: "bk"})

	// Prove the fixture still errors -- otherwise this test silently stops
	// exercising the unresolvable-env branch.
	rigView := fx.cfg.Agents[0]
	rigView.Dir = "broken"
	if _, err := controllerQueryRuntimeEnv(fx.cityPath, fx.cfg, &rigView); err == nil {
		t.Fatal("fixture did not produce a rig env error; the unresolvable-env branch is no longer reachable from this test")
	}

	probes := fx.fanOutProbes(t)
	runner := &recordingScaleCheckRunner{}
	sem := make(chan struct{}, len(probes))
	sp := scaleParams{Min: 0, Max: 100, Check: fanOutBeadsDirCheck}

	got, errs := evaluatePoolFanOutSum("worker", sp, probes, runner.run, sem, true)

	if want := fanOutCityCount + fanOutRigACount + fanOutRigBCount; got != want {
		t.Fatalf("fan-out sum = %d, want %d: the broken rig must contribute 0, not another read of "+
			"the city store (%d) or its own count (%d)", got, want, want+fanOutCityCount, fanOutBrokenCount)
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "broken") {
		t.Fatalf("errs = %v, want exactly one error naming the broken rig", errs)
	}
	for _, dir := range runner.dirs {
		if dir == brokenDir {
			t.Fatalf("the check ran in the broken rig's directory %q; a probe whose store env is unresolvable must not run at all", dir)
		}
	}
}

// TestBuildDesiredState_CityScopedCustomScaleCheckSumsEveryActiveStore is the
// end-to-end proof through the production flow at both fan-out call sites in
// buildDesiredStateWithSessionBeads: the generic pool, which hands the
// city-scoped agent's resolved env to the fan-out, and the pool backing a
// named session, which hands it none. evaluatePendingPools runs the check
// through the real shell, and the pool's desired slots are the sum of the
// stores' own counts -- city + riga + rigb, with the suspended rig contributing
// nothing. fanOutRigBoundCheck makes an unbound rig probe visible at either
// site: it reads the city's count at the generic site (3 slots) and fails, so
// contributes nothing, at the named-session site (1 slot).
func TestBuildDesiredState_CityScopedCustomScaleCheckSumsEveryActiveStore(t *testing.T) {
	for _, tc := range []struct {
		name         string
		namedSession bool
	}{
		{"generic pool", false},
		{"named-session backing pool", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFanOutStoreFixture(t)
			fx.cfg.Agents[0].ScaleCheck = fanOutRigBoundCheck
			if tc.namedSession {
				fx.cfg.NamedSessions = []config.NamedSession{{Template: "worker", Mode: "on_demand"}}
			}

			desired := buildDesiredState("test-city", fx.cityPath, time.Now().UTC(), fx.cfg, runtime.NewFake(), nil, io.Discard)

			slots := 0
			for _, tp := range desired.State {
				if tp.TemplateName == "worker" {
					slots++
				}
			}
			if want := fanOutCityCount + fanOutRigACount + fanOutRigBCount; slots != want {
				t.Fatalf("worker desired slots = %d, want %d (city + riga + rigb, suspended rig excluded)", slots, want)
			}
		})
	}
}
