package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
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
//
// This shape deliberately measures the cheapest one: no packs.lock and a warm
// memo. Use BenchmarkBuiltinReadinessPassLocked to measure the pass a city with
// locked bundled imports pays; that benchmark's comment explains what this one
// cannot see.
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

// BenchmarkBuiltinReadinessPassLocked measures the readiness pass a city with
// locked bundled imports pays: a city whose packs.lock pins a bundled source at
// its canonical commit, with the per-city ready memo cleared before each
// iteration.
//
// THE LOCKFILE IS WHAT THIS BENCHMARK IS FOR. BenchmarkBuiltinReadinessPass
// builds a city with no packs.lock, so lockedBundledCanonicalImports finds
// nothing and the locked-imports half of the pass — one of the two places
// EnsureBuiltinRuntimeAssets validates a synthetic pack cache — never runs at
// all. A locked bundled source resolves to its own cache directory, and
// MaterializeSyntheticRepo writes every bundled pack layout into every such
// directory, so validating it is a second full walk of the whole bundled pack
// set rather than an incremental check. Measured on this fixture, adding the
// lockfile is +50.1% allocations; that is essentially the whole gap between
// the two benchmarks.
//
// Clearing the per-city memo is the secondary, and much smaller, half.
// builtinRuntimeReadyCache is a package-level sync.Map, so from the second
// iteration onward the warm benchmark takes the ready fast path, while a
// one-shot `gc` process starts that map empty and always takes the
// non-memoized revalidation path. Measured in isolation that axis is +0.26%
// allocations. It is kept because it makes the shape honest, not because it
// is where the cost is — do not attribute the difference to it.
//
// The consequence is that the warm benchmark is insensitive to changes in most
// of the pass it is named after: work that only the locked-imports half
// performs measures as zero against it.
//
// FIDELITY: this reproduces the SHAPE of the cost, not the exact pass a real
// city runs. The fixture locks gastown, which this fixture city does not
// import, so the locked arm walks the bundled pack set three times. A real
// bd-provider `gc init` city locks core and bd, which normalize to the same
// clone URL, and pays four. The benchmark therefore understates a real city
// and is conservative in the direction that matters.
func BenchmarkBuiltinReadinessPassLocked(b *testing.B) {
	b.Setenv("GC_HOME", b.TempDir())
	cityPath := newReadinessCostCity(b)
	// The canonical pin for gastown is its own public-pack version, not
	// BundledPackImportVersion. A bundled source locked at any other commit is
	// an ordinary remote import that the preflight skips entirely, which would
	// leave this measuring a city with no locked bundled imports after all.
	writePreflightImportLock(b, cityPath, strings.TrimPrefix(config.PublicGastownPackVersion, "sha:"))
	// Fail loudly rather than quietly measuring the warm shape twice if that
	// ever stops holding.
	locked, err := lockedBundledCanonicalImports(cityPath)
	if err != nil {
		b.Fatalf("lockedBundledCanonicalImports: %v", err)
	}
	if len(locked) == 0 {
		b.Fatal("fixture pinned no canonical bundled imports; the locked-imports half of the pass would not run")
	}
	if _, err := loadCityConfig(cityPath, io.Discard); err != nil {
		b.Fatalf("warming: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		resetBuiltinRuntimeReadyCacheFor(cityPath)
		b.StartTimer()
		if err := EnsureBuiltinRuntimeAssets(cityPath, io.Discard); err != nil {
			b.Fatalf("EnsureBuiltinRuntimeAssets: %v", err)
		}
	}
}

// resetBuiltinRuntimeReadyCacheFor clears one city's builtin readiness memo so
// the next EnsureBuiltinRuntimeAssets call for that city runs the pass a fresh
// process would. It deletes only that city's entry: the memo is a package-level
// sync.Map shared by every test in this binary, and ranging over it would evict
// entries this benchmark does not own.
func resetBuiltinRuntimeReadyCacheFor(cityPath string) {
	builtinRuntimeReadyCache.Delete(normalizePathForCompare(cityPath))
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
