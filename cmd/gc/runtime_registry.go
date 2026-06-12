package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionacp "github.com/gastownhall/gascity/internal/runtime/acp"
	sessioncloudflare "github.com/gastownhall/gascity/internal/runtime/cloudflare"
	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
	sessionk8s "github.com/gastownhall/gascity/internal/runtime/k8s"
	"github.com/gastownhall/gascity/internal/runtime/registry"
	sessionsubprocess "github.com/gastownhall/gascity/internal/runtime/subprocess"
	sessiont3bridge "github.com/gastownhall/gascity/internal/runtime/t3bridge"
	sessiontmux "github.com/gastownhall/gascity/internal/runtime/tmux"
)

// runtimeRegistry resolves session provider selection names. Builtins
// register below; pack-declared runtimes will register during city
// composition (ga-h504e5). The behavior contract for selection lives in
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
		if cityPath != "" {
			return sessionsubprocess.NewProviderWithDir(providerStateDir("subprocess", cityPath)), nil
		}
		return sessionsubprocess.NewProvider(), nil
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
	must(r.Register("cloudflare", func(_ string, _ config.SessionConfig, _, _ string) (runtime.Provider, error) {
		return sessioncloudflare.NewProvider()
	}))
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
	r.SetFallback(func(_ string, sc config.SessionConfig, cityName, cityPath string) (runtime.Provider, error) {
		return sessiontmux.NewProviderWithConfig(tmuxConfigFromSession(sc, cityName, cityPath)), nil
	})
	return r
}
