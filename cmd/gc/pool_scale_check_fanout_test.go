package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
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

	probes, errs := cityScopedFanOutProbes(cityPath, cfg, agentCfg, cityPath, ownEnv, suspended)
	if len(errs) != 0 {
		t.Fatalf("cityScopedFanOutProbes returned errors: %v", errs)
	}

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
