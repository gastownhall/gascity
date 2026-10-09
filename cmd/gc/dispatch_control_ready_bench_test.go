package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// BenchmarkTryControlReadyFromCacheOrFallbackWarmCache measures the
// control-dispatcher serve loop's steady-state per-tick cost: every call
// after the first should answer from the warm control-ready cache without
// paying for another builtin-pack-readiness config load. Before the fix
// (gcy pending, sample-profiled on jonesy at 52.9% of a core sustained) every
// call reloaded and re-validated the config regardless of cache freshness.
func BenchmarkTryControlReadyFromCacheOrFallbackWarmCache(b *testing.B) {
	b.Setenv("GC_HOME", b.TempDir())
	b.Setenv("XDG_RUNTIME_DIR", b.TempDir())
	b.Setenv("GC_SESSION", "fake")
	b.Setenv("GC_BEADS", "file")
	b.Setenv("GC_DOLT", "skip")
	b.Setenv("GC_BOOTSTRAP", "skip")

	cityDir := b.TempDir()
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		b.Fatalf("ensureScopedFileStoreLayout: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"bench-city\"\n"), 0o644); err != nil {
		b.Fatalf("write city.toml: %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		b.Fatalf("ensurePersistedScopeLocalFileStore: %v", err)
	}

	agentCfg := config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"}
	query := workflowServeControlReadyQuery(agentCfg)

	// Prime once outside the timed loop, matching the real serve loop's
	// shape: the very first tick always pays a real load and build.
	if _, handled, err := tryControlReadyFromCacheOrFallback(query, cityDir, nil); err != nil || !handled {
		b.Fatalf("prime call: handled=%v err=%v", handled, err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, handled, err := tryControlReadyFromCacheOrFallback(query, cityDir, nil); err != nil || !handled {
			b.Fatalf("call %d: handled=%v err=%v", i, handled, err)
		}
	}
}
