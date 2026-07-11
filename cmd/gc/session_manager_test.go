package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

func newSessionManagerWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City) *session.Manager {
	if cfg == nil {
		return session.NewManagerWithCityPath(store, sp, cityPath)
	}
	rigContext := currentRigContext(cfg)
	return session.NewManagerWithTransportPolicyResolverAndCityPath(store, sp, cityPath, func(template, provider string) (string, bool) {
		agentCfg, ok := resolveAgentIdentity(cfg, template, rigContext)
		if ok {
			resolved, err := config.ResolveProvider(
				&agentCfg,
				&cfg.Workspace,
				cfg.Providers,
				func(name string) (string, error) { return name, nil },
			)
			if err != nil {
				return agentCfg.Session, strings.TrimSpace(agentCfg.Session) != ""
			}
			return config.ResolveSessionCreateTransport(agentCfg.Session, resolved), strings.TrimSpace(agentCfg.Session) != ""
		}
		provider = strings.TrimSpace(provider)
		if provider == "" {
			provider = strings.TrimSpace(template)
		}
		if provider == "" {
			return "", false
		}
		resolved, err := config.ResolveProvider(
			&config.Agent{Provider: provider},
			&cfg.Workspace,
			cfg.Providers,
			func(name string) (string, error) { return name, nil },
		)
		if err != nil {
			return "", false
		}
		return strings.TrimSpace(resolved.ProviderSessionCreateTransport()), false
	})
}
