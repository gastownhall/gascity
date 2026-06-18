package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionacp "github.com/gastownhall/gascity/internal/runtime/acp"
	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
	sessionk8s "github.com/gastownhall/gascity/internal/runtime/k8s"
	"github.com/gastownhall/gascity/internal/runtime/registry"
	sessionssh "github.com/gastownhall/gascity/internal/runtime/ssh"
	sessionsubprocess "github.com/gastownhall/gascity/internal/runtime/subprocess"
	sessiont3bridge "github.com/gastownhall/gascity/internal/runtime/t3bridge"
	sessiontmux "github.com/gastownhall/gascity/internal/runtime/tmux"
)

// runtimeRegistry resolves session provider selection names. Builtins
// register below; pack-declared runtimes register per city via
// runtimeRegistryForCity — this registry itself is never mutated after
// construction. The behavior contract for selection lives in
// internal/runtime/REQUIREMENTS.md (RUNTIME-SEL rows).
var runtimeRegistry = buildRuntimeRegistry()

// buildRuntimeRegistry registers the builtin runtime providers. Each
// registration mirrors one arm of the pre-registry selection switch;
// constructor helpers (providerStateDir, tmuxConfigFromSession,
// newHybridProvider) stay in providers.go.
func buildRuntimeRegistry() *registry.Registry {
	r := registry.New()
	// Registration failures here are programmer errors (duplicate or
	// blank builtin names) caught by cmd/gc tests; they cannot occur at
	// runtime from configuration input.
	must := func(err error) {
		if err != nil {
			panic("building runtime registry: " + err.Error())
		}
	}

	must(r.Register("fake", func(_ string, _ config.SessionConfig, _, _ string) (runtime.Provider, error) {
		return runtime.NewFake(), nil
	}))
	must(r.Register("fail", func(_ string, _ config.SessionConfig, _, _ string) (runtime.Provider, error) {
		return runtime.NewFailFake(), nil
	}))
	must(r.Register("subprocess", func(_ string, _ config.SessionConfig, _, cityPath string) (runtime.Provider, error) {
		// Cut-over: the subprocess provider is served through the de-conflated
		// seams (runtime.NewProviderFromSeams), validated by the full Provider
		// conformance suite in TestSubprocessSeamConformance.
		if cityPath != "" {
			return sessionsubprocess.NewSeamBackedWithDir(providerStateDir("subprocess", cityPath)), nil
		}
		return sessionsubprocess.NewSeamBacked(), nil
	}))
	must(r.Register("acp", func(_ string, sc config.SessionConfig, _, cityPath string) (runtime.Provider, error) {
		cfg := sessionacp.Config{
			HandshakeTimeout:  sc.ACP.HandshakeTimeoutDuration(),
			NudgeBusyTimeout:  sc.ACP.NudgeBusyTimeoutDuration(),
			OutputBufferLines: sc.ACP.OutputBufferLinesOrDefault(),
		}
		if cityPath != "" {
			return sessionacp.NewProviderWithDir(providerStateDir("acp", cityPath), cfg), nil
		}
		return sessionacp.NewProvider(cfg), nil
	}))
	must(r.Register("t3bridge", func(_ string, _ config.SessionConfig, _, _ string) (runtime.Provider, error) {
		return sessiont3bridge.NewProvider(), nil
	}))
	// "cloudflare" is no longer a builtin: it ships as the runtime-cloudflare
	// pack ([runtimes.cloudflare] → gc-runtime-cloudflare, RPP v0) and
	// resolves per city via runtimeRegistryForCity. A city without that pack
	// that selects session = "cloudflare" falls through to the tmux fallback
	// (RUNTIME-SEL-006) — the delivery-independence boundary (RUNTIME-PLAN-004).
	must(r.Register("k8s", func(_ string, _ config.SessionConfig, _, _ string) (runtime.Provider, error) {
		return sessionk8s.NewProvider()
	}))
	must(r.Register("hybrid", func(_ string, sc config.SessionConfig, cityName, cityPath string) (runtime.Provider, error) {
		return newHybridProvider(sc, cityName, cityPath)
	}))
	must(r.RegisterPrefix("exec:", func(name string, _ config.SessionConfig, _, _ string) (runtime.Provider, error) {
		script := strings.TrimPrefix(name, "exec:")
		if isLegacyT3BridgeExecScript(script) {
			return sessiont3bridge.NewProvider(), nil
		}
		return sessionexec.NewProvider(script), nil
	}))
	// "ssh:<[user@]host[:port]>" selects the SSH backend against a fixed
	// endpoint (the anonymous form; the named, structured form with explicit
	// key/known_hosts is a [runtimes.<name>] ssh declaration). Key + known_hosts
	// resolve via ssh's own config (~/.ssh/config).
	must(r.RegisterPrefix("ssh:", func(name string, _ config.SessionConfig, _, _ string) (runtime.Provider, error) {
		ep, err := sessionssh.ParseEndpoint(strings.TrimPrefix(name, "ssh:"))
		if err != nil {
			return nil, err
		}
		return sessionssh.NewProvider(ep), nil
	}))
	// tmux registers both as an exact name and as the fallback: the exact
	// registration makes a pack-declared runtime named "tmux" a collision
	// error instead of a silent shadow of the default provider.
	tmuxFactory := func(_ string, sc config.SessionConfig, cityName, cityPath string) (runtime.Provider, error) {
		return sessiontmux.NewProviderWithConfig(tmuxConfigFromSession(sc, cityName, cityPath)), nil
	}
	must(r.Register("tmux", tmuxFactory))
	r.SetFallback(tmuxFactory)
	return r
}

// validatePackRuntimeRegistrations fails city config loading when a
// pack-declared runtime collides with a builtin selection name, so the
// error surfaces at composition instead of at first session construction
// (RUNTIME-SEL-007).
func validatePackRuntimeRegistrations(cfg *config.City) error {
	_, err := runtimeRegistryForCity(cfg)
	return err
}

// runtimeRegistryForCity returns the selection registry for one city: the
// process-global builtins plus the city's pack-declared runtimes
// ([runtimes.<name>] in pack.toml, RUNTIME-SEL-011) registered on a clone,
// so concurrent cities in one process never observe each other's runtimes.
// Each pack runtime resolves to the exec proxy provider bound to its
// declared command. A name collision with a builtin is an error — no
// silent shadowing (RUNTIME-SEL-007) — and registration happens before any
// resolution so the tmux fallback can never swallow a declared name
// (RUNTIME-SEL-006).
func runtimeRegistryForCity(cfg *config.City) (*registry.Registry, error) {
	if cfg == nil || len(cfg.Runtimes) == 0 {
		return runtimeRegistry, nil
	}
	r := runtimeRegistry.Clone()
	names := make([]string, 0, len(cfg.Runtimes))
	for name := range cfg.Runtimes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		rt := cfg.Runtimes[name]
		if err := r.Register(name, func(_ string, _ config.SessionConfig, _, _ string) (runtime.Provider, error) {
			return sessionexec.NewProvider(rt.Command), nil
		}); err != nil {
			return nil, fmt.Errorf("pack %q: %w", rt.PackName, err)
		}
	}
	return r, nil
}

// packRuntimeDeclarationChanged reports whether the pack-declared runtime
// backing a selection name differs between two configs. The exec proxy
// binds the declared command at construction time, so a config reload that
// changes (or adds/removes) the declaration behind an unchanged selection
// name must rebuild the provider — the same provider a cold start with the
// new config would construct. Attribution-only changes (the declaring pack
// renamed, same command and protocol) keep the provider.
func packRuntimeDeclarationChanged(oldCfg, newCfg *config.City, name string) bool {
	if oldCfg == nil || newCfg == nil {
		return false
	}
	oldRT, oldOK := oldCfg.Runtimes[name]
	newRT, newOK := newCfg.Runtimes[name]
	if oldOK != newOK {
		return true
	}
	return oldOK && (oldRT.Command != newRT.Command || oldRT.Protocol != newRT.Protocol)
}
