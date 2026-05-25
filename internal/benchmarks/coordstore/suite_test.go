package coordstore_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/benchmarks/coordstore"
	"github.com/gastownhall/gascity/internal/benchmarks/coordstore/adapters/authorcore"
	"github.com/gastownhall/gascity/internal/benchmarks/coordstore/adapters/boltdb"
	"github.com/gastownhall/gascity/internal/benchmarks/coordstore/adapters/couchdb"
	"github.com/gastownhall/gascity/internal/benchmarks/coordstore/adapters/dolt"
	"github.com/gastownhall/gascity/internal/benchmarks/coordstore/adapters/hqstore"
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
		{
			name:  "hqstore",
			newFn: func() coordstore.StoreAdapter { return hqstore.New() },
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
