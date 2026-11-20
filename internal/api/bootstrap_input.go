package api

import (
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

// freshProviderInputConfig applies per-call provenance only to the local
// CreateModeStarted config of a validated bare-provider create. It is never
// used for a configured worker, named session, resume or adopted runtime.
func freshProviderInputConfig(city *config.City, resolved *config.ResolvedProvider, cfg worker.ResolvedSessionConfig) worker.ResolvedSessionConfig {
	cfg.Runtime.Hints.BootstrapInputOrigin = ""
	if cfg.Transport != "" && cfg.Transport != config.SessionTransportTmux {
		return cfg
	}
	family := config.FreshBuiltinCommandFamily(city, resolved)
	hints := &cfg.Runtime.Hints
	if family == "" || hints.PromptSuffix != "" || hints.Nudge != "" ||
		len(hints.SessionSetup) != 0 || hints.SessionSetupScript != "" || len(hints.SessionLive) != 0 {
		return cfg
	}
	hints.ProviderName = family
	hints.BootstrapInputOrigin = runtime.BootstrapInputFreshBuiltin
	return cfg
}
