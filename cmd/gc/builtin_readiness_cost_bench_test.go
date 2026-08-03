package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

// newReadinessCostCity writes a minimal bd-provider city.
func newReadinessCostCity(b *testing.B) string {
	b.Helper()
	cityPath := b.TempDir()
	toml := "name = \"bench\"\nprefix = \"bc\"\n\n[beads]\nprovider = \"bd\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(toml), 0o644); err != nil {
		b.Fatalf("writing city.toml: %v", err)
	}
	return cityPath
}

// BenchmarkBuiltinReadinessPass measures EnsureBuiltinRuntimeAssets on its
// warm memo-hit path: the readiness revalidation that reads every file of
// every cached builtin pack before a config load parses anything.
//
// Read this against BenchmarkCityConfigParseOnly. The readiness pass, not the
// parse, is what a config load costs — which is why skipping a redundant load
// is worth anything, and why the pass itself must still run once per process.
func BenchmarkBuiltinReadinessPass(b *testing.B) {
	b.Setenv("GC_HOME", b.TempDir())
	cityPath := newReadinessCostCity(b)
	if _, err := loadCityConfig(cityPath, io.Discard); err != nil {
		b.Fatalf("warming: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := EnsureBuiltinRuntimeAssets(cityPath, io.Discard); err != nil {
			b.Fatalf("EnsureBuiltinRuntimeAssets: %v", err)
		}
	}
}

// BenchmarkCityConfigParseOnly measures the config parse plus pack expansion
// with the readiness pass skipped.
func BenchmarkCityConfigParseOnly(b *testing.B) {
	b.Setenv("GC_HOME", b.TempDir())
	cityPath := newReadinessCostCity(b)
	if _, err := loadCityConfig(cityPath, io.Discard); err != nil {
		b.Fatalf("warming: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard); err != nil {
			b.Fatalf("loadCityConfigWithoutBuiltinPackRefresh: %v", err)
		}
	}
}

// BenchmarkSuppliedConfigReadinessGuard measures what a store open handed an
// already-loaded config now pays to keep the self-heal contract: a memo lookup
// for a city this process already readied, instead of a second readiness pass.
func BenchmarkSuppliedConfigReadinessGuard(b *testing.B) {
	b.Setenv("GC_HOME", b.TempDir())
	cityPath := newReadinessCostCity(b)
	if _, err := loadCityConfig(cityPath, io.Discard); err != nil {
		b.Fatalf("warming: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := ensureBuiltinRuntimeAssetsForSuppliedConfig(cityPath, io.Discard); err != nil {
			b.Fatalf("ensureBuiltinRuntimeAssetsForSuppliedConfig: %v", err)
		}
	}
}
