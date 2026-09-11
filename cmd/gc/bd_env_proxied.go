package main

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

// A bd-owned proxied scope publishes no endpoint for gc to project: bd starts
// the proxy on its next command and the child Dolt server listens on a port
// only bd's own client discovers. So the projection is the same map on every
// call — the backend markers and the proxy selector — and rebuilding it per bd
// invocation bought nothing while costing a full city-config load and pack
// expansion each time.
//
// Two things are memoised here, both keyed by (city, scope):
//
//   - the projection itself, so `gc doctor`'s hundreds of bd calls parse
//     city.toml once instead of once per fork;
//   - nothing about liveness. Readiness belongs to bd on this path (D3/R2):
//     it auto-starts the proxy, and a command that cannot reach it reports its
//     own error. gc must not answer that question by running a city-wide
//     health fan-out from inside an environment builder.
//
// The cache is invalidated by a stamp over the files that decide both the
// classification and the projection, so a rewritten city.toml or a migrated
// binding is still picked up inside a long-lived supervisor process.
var (
	proxiedScopeRuntimeEnvCache sync.Map // proxiedScopeRuntimeEnvKey → proxiedScopeRuntimeEnvEntry
	// proxiedScopeRuntimeEnvBuilds counts cache misses so a test can prove the
	// projection is built once rather than per call.
	proxiedScopeRuntimeEnvBuilds atomic.Int64
)

type proxiedScopeRuntimeEnvKey struct {
	city  string
	scope string
}

type proxiedScopeRuntimeEnvEntry struct {
	stamp string
	env   map[string]string
}

// proxiedScopeRuntimeEnvInputs lists the files whose content decides whether a
// scope is proxied and what its projection contains.
func proxiedScopeRuntimeEnvInputs(cityPath, scopeRoot string) []string {
	return []string{
		filepath.Join(cityPath, "city.toml"),
		filepath.Join(cityPath, ".gc", scopeOwnershipFile),
		filepath.Join(scopeRoot, ".beads", "metadata.json"),
		filepath.Join(scopeRoot, ".beads", "proxied_server_client_info.json"),
	}
}

func proxiedScopeRuntimeEnvStamp(cityPath, scopeRoot string) string {
	var stamp strings.Builder
	for _, path := range proxiedScopeRuntimeEnvInputs(cityPath, scopeRoot) {
		info, err := os.Stat(path)
		if err != nil {
			stamp.WriteString("-|")
			continue
		}
		fmt.Fprintf(&stamp, "%d.%d|", info.Size(), info.ModTime().UnixNano())
	}
	return stamp.String()
}

func proxiedScopeRuntimeEnvCacheKey(cityPath, scopeRoot string) proxiedScopeRuntimeEnvKey {
	return proxiedScopeRuntimeEnvKey{
		city:  normalizePathForCompare(cityPath),
		scope: normalizePathForCompare(scopeRoot),
	}
}

// cachedProxiedScopeRuntimeEnv returns a private copy of a still-current
// projection. Callers mutate what they get back.
func cachedProxiedScopeRuntimeEnv(cityPath, scopeRoot string) (map[string]string, bool) {
	value, ok := proxiedScopeRuntimeEnvCache.Load(proxiedScopeRuntimeEnvCacheKey(cityPath, scopeRoot))
	if !ok {
		return nil, false
	}
	entry, ok := value.(proxiedScopeRuntimeEnvEntry)
	if !ok || entry.stamp != proxiedScopeRuntimeEnvStamp(cityPath, scopeRoot) {
		return nil, false
	}
	return maps.Clone(entry.env), true
}

// rememberProxiedScopeRuntimeEnv stores env and returns it, so the caller can
// `return rememberProxiedScopeRuntimeEnv(...), nil` in one line.
func rememberProxiedScopeRuntimeEnv(cityPath, scopeRoot string, env map[string]string) map[string]string {
	proxiedScopeRuntimeEnvBuilds.Add(1)
	proxiedScopeRuntimeEnvCache.Store(proxiedScopeRuntimeEnvCacheKey(cityPath, scopeRoot), proxiedScopeRuntimeEnvEntry{
		stamp: proxiedScopeRuntimeEnvStamp(cityPath, scopeRoot),
		env:   maps.Clone(env),
	})
	return env
}

// forgetProxiedScopeRuntimeEnv drops every scope projection cached for a city.
// The stamp already covers ordinary rewrites; this exists for callers that
// tear a city down and rebuild it at the same path.
func forgetProxiedScopeRuntimeEnv(cityPath string) {
	city := normalizePathForCompare(cityPath)
	proxiedScopeRuntimeEnvCache.Range(func(key, _ any) bool {
		if k, ok := key.(proxiedScopeRuntimeEnvKey); ok && k.city == city {
			proxiedScopeRuntimeEnvCache.Delete(key)
		}
		return true
	})
}

// applyProxiedScopeRuntimeEnv completes the projection for a bd-owned proxied
// scope: the proxy selector plus, for an external upstream, the endpoint bd's
// sidecar recorded. It deliberately resolves no host or port for a local
// upstream — bd's proxy owns that listener.
func applyProxiedScopeRuntimeEnv(env map[string]string, scopeRoot string) error {
	applyProxiedDoltEnv(env)
	external, err := proxiedScopeHasExternalUpstream(scopeRoot)
	if err != nil {
		return err
	}
	if !external {
		return nil
	}
	overrides, err := inheritedProviderExternalEndpointEnv(scopeRoot, providerScopeIntent{Transport: "proxied", Target: "external"})
	if err != nil {
		return err
	}
	maps.Copy(env, overrides)
	return nil
}
