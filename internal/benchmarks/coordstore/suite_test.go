package coordstore_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/benchmarks/coordstore"
	"github.com/gastownhall/gascity/internal/benchmarks/coordstore/adapters/authorcore"
	"github.com/gastownhall/gascity/internal/benchmarks/coordstore/adapters/boltdb"
	"github.com/gastownhall/gascity/internal/benchmarks/coordstore/adapters/couchdb"
	"github.com/gastownhall/gascity/internal/benchmarks/coordstore/adapters/dolt"
	"github.com/gastownhall/gascity/internal/benchmarks/coordstore/adapters/postgres"
	"github.com/gastownhall/gascity/internal/benchmarks/coordstore/adapters/sqlite"
)

// adapterFactory creates a new, uninitialized StoreAdapter.
type adapterFactory struct {
	name  string
	newFn func() coordstore.StoreAdapter
}

// registeredAdapters is the list of backends exercised by the suite.
// External backends are opt-in so normal CI does not require Docker or a
// running Dolt SQL server.
var registeredAdapters = buildRegisteredAdapters()

func buildRegisteredAdapters() []adapterFactory {
	adapters := []adapterFactory{
		{
			name:  "sqlite",
			newFn: func() coordstore.StoreAdapter { return sqlite.New() },
		},
		{
			name:  "bbolt",
			newFn: func() coordstore.StoreAdapter { return boltdb.New() },
		},
		{
			name:  "authorcore",
			newFn: func() coordstore.StoreAdapter { return authorcore.New() },
		},
	}
	if dsn := os.Getenv("COORDSTORE_POSTGRES_DSN"); dsn != "" {
		adapters = append(adapters, adapterFactory{
			name:  "postgres",
			newFn: func() coordstore.StoreAdapter { return postgres.New(dsn) },
		})
	}
	if rawURL := os.Getenv("COORDSTORE_COUCHDB_URL"); rawURL != "" {
		adapters = append(adapters, adapterFactory{
			name:  "couchdb",
			newFn: func() coordstore.StoreAdapter { return couchdb.New(rawURL) },
		})
	}
	if dsn := os.Getenv("COORDSTORE_DOLT_DSN"); dsn != "" {
		adapters = append(adapters, adapterFactory{
			name:  "dolt",
			newFn: func() coordstore.StoreAdapter { return dolt.New(dsn) },
		})
	}
	return adapters
}

// TestBenchmarkSuite is the primary end-to-end benchmark. It:
//  1. Runs correctness checks against every registered adapter.
//  2. Seeds each adapter with a realistic starting population.
//  3. Drives the SmokeWorkload (fast, used in CI).
//  4. Prints a scorecard per adapter.
//
// For the full RealWorldWorkload, use -run TestBenchmarkSuiteRealWorld.
// For the StressWorkload, use -run TestBenchmarkSuiteStress.
func TestBenchmarkSuite(t *testing.T) {
	// Smoke: correctness gated, performance informational (reference adapters may miss targets).
	runSuite(t, coordstore.SmokeWorkload, false)
}

// TestBenchmarkSuiteRealWorld runs the 30-second realistic workload.
// Not included in the standard test pass; run explicitly when evaluating candidates.
func TestBenchmarkSuiteRealWorld(t *testing.T) {
	if os.Getenv("COORDSTORE_BENCH") == "" {
		t.Skip("set COORDSTORE_BENCH=1 to run the 30-second real-world workload")
	}
	runSuite(t, coordstore.RealWorldWorkload, true)
}

// TestBenchmarkSuiteStress runs the burst-throughput stress workload.
func TestBenchmarkSuiteStress(t *testing.T) {
	if os.Getenv("COORDSTORE_BENCH") == "" {
		t.Skip("set COORDSTORE_BENCH=1 to run the 15-second stress workload")
	}
	runSuite(t, coordstore.StressWorkload, true)
}

// TestBenchmarkSoakPhaseA runs the long in-process Phase A soak harness.
func TestBenchmarkSoakPhaseA(t *testing.T) {
	if os.Getenv("COORDSTORE_SOAK") == "" {
		t.Skip("set COORDSTORE_SOAK=1 to run the Phase A soak harness")
	}
	cfg := soakConfigFromEnv(t, 4*time.Hour)
	checkSoakPreflight(t, cfg, len(registeredAdapters))
	runSoakSuite(t, cfg)
}

// TestBenchmarkSoakCalibrate runs the shorter calibration soak pass.
func TestBenchmarkSoakCalibrate(t *testing.T) {
	if os.Getenv("COORDSTORE_SOAK") == "" || os.Getenv("COORDSTORE_SOAK_CALIBRATE") == "" {
		t.Skip("set COORDSTORE_SOAK=1 and COORDSTORE_SOAK_CALIBRATE=1 to run calibration")
	}
	cfg := soakConfigFromEnv(t, 30*time.Minute)
	checkSoakPreflight(t, cfg, len(registeredAdapters))
	runSoakSuite(t, cfg)
}

// TestBenchmarkSoakPhaseB runs the Phase B chaos soak harness.
func TestBenchmarkSoakPhaseB(t *testing.T) {
	if os.Getenv("COORDSTORE_SOAK") == "" || os.Getenv("COORDSTORE_SOAK_PHASE_B") == "" {
		t.Skip("set COORDSTORE_SOAK=1 and COORDSTORE_SOAK_PHASE_B=1 to run Phase B chaos")
	}
	cfg := soakConfigFromEnv(t, 4*time.Hour)
	cfg.SoakPhase = coordstore.SoakPhaseB
	if cfg.ChaosDuration > 0 {
		cfg.SoakDuration = cfg.ChaosDuration
	}
	cfg.KillCadence = killCadenceFromEnv(t, 30*time.Second)
	checkSoakPreflight(t, cfg, len(registeredAdapters))
	runChaosSuite(t, cfg)
}

// TestBenchmarkSoakTriage synthesizes existing Phase A/B artifacts into a
// Markdown and JSON report.
func TestBenchmarkSoakTriage(t *testing.T) {
	if os.Getenv("COORDSTORE_SOAK") == "" {
		t.Skip("set COORDSTORE_SOAK=1 to run soak triage")
	}
	resultsDir := os.Getenv("COORDSTORE_RESULTS_DIR")
	if resultsDir == "" {
		t.Fatal("COORDSTORE_RESULTS_DIR is required for soak triage")
	}
	writeSoakTriage(t, resultsDir)
}

// TestBenchmarkSoakDolt runs Phase A + Phase B chaos against the dolt adapter
// as the legacy baseline (ga-babhr), in parallel with — not part of — the
// 4-RC FullMatrix run. Gated by COORDSTORE_SOAK=1 + a registered dolt adapter
// (COORDSTORE_DOLT_DSN set). Mirrors the matrix's body for a fair comparison.
func TestBenchmarkSoakDolt(t *testing.T) {
	if os.Getenv("COORDSTORE_SOAK") == "" {
		t.Skip("set COORDSTORE_SOAK=1 to run the dolt soak baseline")
	}
	var dolt *adapterFactory
	for i := range registeredAdapters {
		if registeredAdapters[i].name == "dolt" {
			dolt = &registeredAdapters[i]
			break
		}
	}
	if dolt == nil {
		t.Skip("dolt adapter not registered (set COORDSTORE_DOLT_DSN)")
	}
	cfg := soakConfigFromEnv(t, 4*time.Hour)
	phaseBCfg := cfg
	phaseBCfg.SoakPhase = coordstore.SoakPhaseB
	if cfg.ChaosDuration > 0 {
		phaseBCfg.SoakDuration = cfg.ChaosDuration
	}
	phaseBCfg.KillCadence = killCadenceFromEnv(t, 30*time.Second)
	checkSoakPreflight(t, phaseBCfg, 1)
	t.Run(dolt.name, func(t *testing.T) {
		runSoakBackend(t, cfg, *dolt)
		runChaosBackend(t, phaseBCfg, *dolt)
	})
	writeSoakTriage(t, cfg.ResultsDir)
}

func runSuite(t *testing.T, wl coordstore.WorkloadConfig, enforceTargets bool) {
	t.Helper()
	ctx := context.Background()

	var scorecards []coordstore.Scorecard

	for _, af := range registeredAdapters {
		af := af
		t.Run(af.name, func(t *testing.T) {
			dir := t.TempDir()

			adapter := af.newFn()
			if err := adapter.Open(ctx, coordstore.Config{DataDir: dir}); err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer adapter.Close() //nolint:errcheck

			// Phase 1: Correctness checks.
			t.Log("  → running FR correctness checks")
			failures := coordstore.CorrectnessChecker(ctx, adapter)
			for _, f := range failures {
				t.Errorf("  FAIL correctness: %s", f)
			}
			if len(failures) > 0 {
				t.Fatalf("  %d correctness failures — skipping performance benchmark", len(failures))
			}
			t.Logf("  ✓ all FR checks passed")

			// Phase 2: Reset and seed.
			if err := adapter.Reset(ctx); err != nil {
				t.Fatalf("Reset: %v", err)
			}
			seeder := coordstore.NewSeeder(0x1234abcd)
			seed, err := seeder.Seed(ctx, adapter, wl)
			if err != nil {
				t.Fatalf("Seed: %v", err)
			}
			t.Logf("  seeded: %d main open, %d main closed, %d wisps, %d deps",
				len(seed.MainOpenIDs), len(seed.MainClosedIDs), len(seed.WispOpenIDs), len(seed.DepEdges))

			// Phase 3: Prime scan timing (FR-15).
			start := time.Now()
			count, err := adapter.PrimeScan(ctx)
			primeElapsed := time.Since(start)
			if err != nil {
				t.Errorf("PrimeScan: %v", err)
			}
			t.Logf("  PrimeScan: %d records in %s", count, coordstore.FormatDuration(primeElapsed))
			if primeElapsed > 5*time.Second {
				t.Errorf("  FAIL FR-15: PrimeScan took %s > 5s target", coordstore.FormatDuration(primeElapsed))
			}

			// Phase 4: Workload run.
			t.Logf("  → running workload %q (%s, concurrency=%d)",
				wl.Name, wl.Duration, wl.Concurrency)
			runner := coordstore.NewRunner(adapter, wl, seed)
			sc, err := runner.Run(ctx, testWriter{t})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			sc.Backend = af.name

			// Phase 5: Print scorecard.
			sc.PrintTable(testWriter{t})

			// Report target outcomes; only gate the test when enforceTargets is set.
			for _, r := range sc.Results {
				if r.Measured && !r.Pass {
					if enforceTargets {
						t.Errorf("  FAIL target %q: %s", r.Target.Name, r.Reason)
					} else {
						t.Logf("  INFO target %q: %s (informational in smoke run)", r.Target.Name, r.Reason)
					}
				}
			}

			scorecards = append(scorecards, sc)
		})
	}

	// Summary across all backends.
	if len(scorecards) > 1 {
		printComparison(t, scorecards)
	}
}

func runSoakSuite(t *testing.T, cfg coordstore.SoakConfig) {
	t.Helper()
	runSoakSuiteForAdapters(t, cfg, registeredAdapters)
}

func runSoakSuiteForAdapters(t *testing.T, cfg coordstore.SoakConfig, adapters []adapterFactory) {
	t.Helper()
	for _, af := range adapters {
		af := af
		t.Run(af.name, func(t *testing.T) {
			runSoakBackend(t, cfg, af)
		})
	}
}

func runChaosSuite(t *testing.T, cfg coordstore.SoakConfig) {
	t.Helper()
	runChaosSuiteForAdapters(t, cfg, registeredAdapters)
}

func runChaosSuiteForAdapters(t *testing.T, cfg coordstore.SoakConfig, adapters []adapterFactory) {
	t.Helper()
	for _, af := range adapters {
		af := af
		t.Run(af.name, func(t *testing.T) {
			runChaosBackend(t, cfg, af)
		})
	}
}

func runSoakBackend(t *testing.T, cfg coordstore.SoakConfig, af adapterFactory) {
	t.Helper()
	ctx := context.Background()
	workload := coordstore.RealWorldWorkload

	dir := t.TempDir()
	dataDir := filepath.Join(dir, "store")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir store: %v", err)
	}
	adapter := af.newFn()
	if err := adapter.Open(ctx, coordstore.Config{DataDir: dataDir}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer adapter.Close() //nolint:errcheck

	failures := coordstore.CorrectnessChecker(ctx, adapter)
	for _, f := range failures {
		t.Errorf("  FAIL correctness: %s", f)
	}
	if len(failures) > 0 {
		t.Fatalf("  %d correctness failures — skipping soak", len(failures))
	}
	soakCfg := cfg
	soakCfg.DataDir = dataDir
	result, err := coordstore.NewSoakRunner(af.name, adapter, workload, soakCfg).Run(ctx, testWriter{t})
	if err != nil {
		t.Fatalf("SoakRunner: %v", err)
	}
	t.Logf("soak artifacts: %s", result.ResultsDir)
}

func runChaosBackend(t *testing.T, cfg coordstore.SoakConfig, af adapterFactory) {
	t.Helper()
	ctx := context.Background()
	workload := coordstore.RealWorldWorkload

	dir := t.TempDir()
	dataDir := filepath.Join(dir, "store")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir store: %v", err)
	}
	soakCfg := cfg
	soakCfg.DataDir = dataDir
	process := coordstore.NewChaosProcess(coordstore.ChaosProcessConfig{
		Backend:    af.name,
		SocketPath: filepath.Join(dir, "chaos.sock"),
		DataDir:    dataDir,
	})
	if err := process.Start(ctx); err != nil {
		t.Fatalf("ChaosProcess start: %v", err)
	}
	defer process.Close() //nolint:errcheck
	result, err := coordstore.NewChaosRunner(af.name, process, workload, soakCfg).Run(ctx, testWriter{t})
	if err != nil {
		t.Fatalf("ChaosRunner: %v", err)
	}
	t.Logf("chaos artifacts: %s", result.ResultsDir)
}

func checkSoakPreflight(t *testing.T, cfg coordstore.SoakConfig, backendCount int) {
	t.Helper()
	result, err := coordstore.CheckSoakPreflight(context.Background(), coordstore.SoakPreflightConfig{
		ResultsDir:   cfg.ResultsDir,
		SoakConfig:   cfg,
		BackendCount: backendCount,
	})
	t.Log(result.Message)
	if err != nil {
		t.Fatalf("soak preflight: %v", err)
	}
}

func writeSoakTriage(t *testing.T, resultsDir string) {
	t.Helper()
	var report coordstore.TriageReport
	if err := report.Synthesize(context.Background(), resultsDir); err != nil {
		t.Fatalf("triage synthesize: %v", err)
	}
	var markdown bytes.Buffer
	if err := report.PrintMarkdown(&markdown); err != nil {
		t.Fatalf("triage markdown: %v", err)
	}
	markdownPath := filepath.Join(resultsDir, "triage.md")
	if err := os.WriteFile(markdownPath, markdown.Bytes(), 0o644); err != nil {
		t.Fatalf("write triage markdown: %v", err)
	}
	if err := report.WriteJSON(filepath.Join(resultsDir, "triage.json")); err != nil {
		t.Fatalf("write triage json: %v", err)
	}
	t.Log(markdown.String())
}

func soakConfigFromEnv(t *testing.T, defaultDuration time.Duration) coordstore.SoakConfig {
	t.Helper()
	resultsDir := os.Getenv("COORDSTORE_RESULTS_DIR")
	if resultsDir == "" {
		resultsDir = "/var/tmp/coordstore-soak"
	}
	duration := defaultDuration
	if raw := os.Getenv("COORDSTORE_SOAK_DURATION"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			t.Fatalf("invalid COORDSTORE_SOAK_DURATION %q: %v", raw, err)
		}
		duration = parsed
	}
	sampleInterval := 10 * time.Second
	if raw := os.Getenv("COORDSTORE_SOAK_SAMPLE_INTERVAL"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			t.Fatalf("invalid COORDSTORE_SOAK_SAMPLE_INTERVAL %q: %v", raw, err)
		}
		sampleInterval = parsed
	}
	scale := 1.0
	if raw := os.Getenv("COORDSTORE_SOAK_SCALE"); raw != "" {
		parsed, err := strconv.ParseFloat(raw, 64)
		if err != nil || parsed <= 0 {
			t.Fatalf("invalid COORDSTORE_SOAK_SCALE %q: %v", raw, err)
		}
		scale = parsed
	}
	var chaosDuration time.Duration
	if raw := os.Getenv("COORDSTORE_CHAOS_DURATION"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			t.Fatalf("invalid COORDSTORE_CHAOS_DURATION %q: %v", raw, err)
		}
		chaosDuration = parsed
	}
	return coordstore.SoakConfig{
		SoakPhase:      coordstore.SoakPhaseA,
		SoakDuration:   duration,
		ChaosDuration:  chaosDuration,
		SampleInterval: sampleInterval,
		ResultsDir:     resultsDir,
		ScaleFactor:    scale,
	}
}

func killCadenceFromEnv(t *testing.T, defaultCadence time.Duration) time.Duration {
	t.Helper()
	if raw := os.Getenv("COORDSTORE_KILL_CADENCE"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			t.Fatalf("invalid COORDSTORE_KILL_CADENCE %q: %v", raw, err)
		}
		return parsed
	}
	return defaultCadence
}

// printComparison prints a side-by-side pass/fail matrix for all backends.
func printComparison(t *testing.T, scorecards []coordstore.Scorecard) {
	t.Helper()
	t.Logf("\n=== Cross-Backend Comparison ===")
	header := fmt.Sprintf("  %-38s", "Target")
	for _, sc := range scorecards {
		header += fmt.Sprintf("  %-12s", sc.Backend)
	}
	t.Log(header)

	// Collect all target names from the first scorecard.
	if len(scorecards) == 0 {
		return
	}
	for i, r := range scorecards[0].Results {
		if !r.Measured {
			continue
		}
		row := fmt.Sprintf("  %-38s", r.Target.Name)
		for _, sc := range scorecards {
			if i >= len(sc.Results) {
				row += fmt.Sprintf("  %-12s", "-")
				continue
			}
			sr := sc.Results[i]
			if !sr.Measured {
				row += fmt.Sprintf("  %-12s", "skip")
				continue
			}
			status := "PASS"
			if !sr.Pass {
				status = "FAIL"
			}
			row += fmt.Sprintf("  %-12s", status)
		}
		t.Log(row)
	}
	t.Logf("")
}

// testWriter adapts t.Log to io.Writer so runner.Run can write progress.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}
