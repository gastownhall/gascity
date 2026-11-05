package main

// Benchmark for the ga-237xpr per-tick store-open fix. Companion file to
// order_dispatch_test.go; measures the config-reparse cost dispatch()
// pays per tick before vs. after the fix, using a multi-pack city fixture
// so the parse cost (the thing the fix eliminates) is large enough to be
// visible against per-op noise.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

// writeMultiPackBenchCityFixture writes a city.toml that includes n pack
// fragments, each declaring several [[agent]] entries, so loadCityConfig
// has real TOML-decode and pack-resolution work to do — the cost
// ga-237xpr's fix removes from the per-tick dispatch path.
func writeMultiPackBenchCityFixture(b *testing.B, cityDir string, n int) {
	b.Helper()
	includes := make([]string, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("pack-%02d.toml", i)
		includes = append(includes, name)
		var frag string
		for j := 0; j < 8; j++ {
			frag += fmt.Sprintf(`[[agent]]
name = "bench-agent-%02d-%02d"
dir = "rig-%02d"
work_query = "bd ready --label=bench-%02d-%02d"
sling_query = "bd update {} --set-metadata gc.routed_to=bench-%02d-%02d"

`, i, j, i, i, j, i, j)
		}
		if err := os.WriteFile(filepath.Join(cityDir, name), []byte(frag), 0o644); err != nil {
			b.Fatalf("writing pack fragment %s: %v", name, err)
		}
	}

	toml := "include = ["
	for i, inc := range includes {
		if i > 0 {
			toml += ", "
		}
		toml += fmt.Sprintf("%q", inc)
	}
	toml += "]\n\n[workspace]\nname = \"bench\"\n\n[beads]\nprovider = \"file\"\n"
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(toml), 0o644); err != nil {
		b.Fatalf("writing city.toml: %v", err)
	}
}

// BenchmarkOrderDispatchTick is the ga-237xpr before/after measurement: the
// per-tick cost of memoryOrderDispatcher.dispatch() with a storeFn that
// re-parses the (multi-pack) city config on every call — simulating the
// pre-fix openStoreAtForCity path — versus the post-fix storeFn that reuses
// the dispatcher's cached *config.City. Run with:
//
//	go test ./cmd/gc/ -run '^$' -bench '^BenchmarkOrderDispatchTick$' -benchtime=200x
//
// and compare ns/op and the reparses/op custom metric between the two
// sub-benchmarks.
func BenchmarkOrderDispatchTick(b *testing.B) {
	cityDir := b.TempDir()
	writeMultiPackBenchCityFixture(b, cityDir, 15)

	cfg, err := loadCityConfig(cityDir, io.Discard)
	if err != nil {
		b.Fatalf("loadCityConfig: %v", err)
	}

	// Interval far longer than the benchmark's synthetic clock advance, so
	// the order is never actually due and dispatch() does only the
	// per-order storeFn open on every call — isolating exactly the cost
	// this bead's fix targets, with no exec/tracking-bead side effects.
	aa := []orders.Order{{
		Name:     "bench-order",
		Trigger:  "cooldown",
		Interval: "1h",
		Exec:     "true",
	}}

	b.Run("PreFix_ReparsePerTick", func(b *testing.B) {
		before := loadCityConfigCalls.Load()
		ad := newMemoryOrderDispatcher(nil, aa, cityDir, cfg, events.Discard, io.Discard)
		// Reproduce the pre-fix storeFn verbatim (order_dispatch.go:431-433
		// before the ga-237xpr fix): ignore the dispatcher's cached cfg and
		// go through the nil-cfg path, which reloads city.toml + every pack
		// include on each call.
		ad.storeFn = func(target execStoreTarget) (beads.Store, error) {
			return openStoreAtForCity(target.ScopeRoot, cityDir)
		}
		now := time.Now()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			ad.dispatch(context.Background(), cityDir, now.Add(time.Duration(i)*time.Millisecond))
		}
		b.StopTimer()
		reparses := loadCityConfigCalls.Load() - before
		b.ReportMetric(float64(reparses)/float64(b.N), "reparses/op")
	})

	b.Run("PostFix_CachedConfig", func(b *testing.B) {
		before := loadCityConfigCalls.Load()
		ad := newMemoryOrderDispatcher(nil, aa, cityDir, cfg, events.Discard, io.Discard)
		now := time.Now()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			ad.dispatch(context.Background(), cityDir, now.Add(time.Duration(i)*time.Millisecond))
		}
		b.StopTimer()
		reparses := loadCityConfigCalls.Load() - before
		b.ReportMetric(float64(reparses)/float64(b.N), "reparses/op")
	})
}

// BenchmarkResolveConfiguredExecStoreTarget is the exec-provider analog of
// BenchmarkOrderDispatchTick, isolated to the function PR #4682 review round
// 1 flagged as the BLOCKER left open by the original fix:
// resolveConfiguredExecStoreTarget ignored a caller's already-resolved
// *config.City and re-parsed city.toml unconditionally on every call.
// PreFix_ReparsePerCall drives the legacy nil-cfg entry point (still live,
// used by the non-hot-path beads_provider_lifecycle.go caller); PostFix
// drives the WithConfig variant openExecStoreAtForCity now calls on the hot
// per-tick path. Run with:
//
//	go test ./cmd/gc/ -run '^$' -bench '^BenchmarkResolveConfiguredExecStoreTarget$' -benchtime=200x
func BenchmarkResolveConfiguredExecStoreTarget(b *testing.B) {
	cityDir := b.TempDir()
	writeMultiPackBenchCityFixture(b, cityDir, 15)

	cfg, err := loadCityConfig(cityDir, io.Discard)
	if err != nil {
		b.Fatalf("loadCityConfig: %v", err)
	}

	b.Run("PreFix_ReparsePerCall", func(b *testing.B) {
		before := loadCityConfigCalls.Load()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := resolveConfiguredExecStoreTarget(cityDir, cityDir); err != nil {
				b.Fatalf("resolveConfiguredExecStoreTarget: %v", err)
			}
		}
		b.StopTimer()
		reparses := loadCityConfigCalls.Load() - before
		b.ReportMetric(float64(reparses)/float64(b.N), "reparses/op")
	})

	b.Run("PostFix_CachedConfig", func(b *testing.B) {
		before := loadCityConfigCalls.Load()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := resolveConfiguredExecStoreTargetWithConfig(cityDir, cityDir, cfg); err != nil {
				b.Fatalf("resolveConfiguredExecStoreTargetWithConfig: %v", err)
			}
		}
		b.StopTimer()
		reparses := loadCityConfigCalls.Load() - before
		b.ReportMetric(float64(reparses)/float64(b.N), "reparses/op")
	})
}
