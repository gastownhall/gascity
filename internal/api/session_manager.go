package api

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

func (s *Server) sessionManager(store beads.Store) *session.Manager {
	cfg := s.state.Config()
	if cfg == nil {
		return session.NewManagerWithOptions(store, s.state.SessionProvider(), session.WithCityPath(s.state.CityPath()))
	}
	return session.NewManagerWithOptions(
		store,
		s.state.SessionProvider(),
		session.WithCityPath(s.state.CityPath()),
		session.WithTransportPolicyResolver(func(template, provider string) (string, bool) {
			return configuredSessionTransportResolution(cfg, template, provider)
		}),
	)
}

func configuredSessionTransport(cfg *config.City, template, provider string) string {
	transport, _ := configuredSessionTransportResolution(cfg, template, provider)
	return transport
}

func configuredSessionTransportResolution(cfg *config.City, template, provider string) (string, bool) {
	if cfg == nil {
		return "", false
	}
	if agentCfg, ok := resolveSessionTemplateAgent(cfg, template); ok {
		resolved, err := config.ResolveProvider(
			&agentCfg,
			&cfg.Workspace,
			cfg.Providers,
			func(name string) (string, error) { return name, nil },
		)
		if err != nil {
			return session.ResolveEffectiveTransport(
				effectiveSessionRuntimeName(agentCfg.Session, cfg.Session.Provider),
				agentCfg.Session,
			), false
		}
		return session.ResolveEffectiveTransport(
			effectiveSessionRuntimeName(agentCfg.Session, cfg.Session.Provider),
			config.ResolveSessionCreateTransport(agentCfg.Session, resolved),
		), false
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
		return session.ResolveEffectiveTransport(cfg.Session.Provider, ""), false
	}
	return session.ResolveEffectiveTransport(
		cfg.Session.Provider,
		resolved.ProviderSessionCreateTransport(),
	), false
}

func effectiveSessionRuntimeName(sessionOverride, cityRuntime string) string {
	if override := strings.TrimSpace(sessionOverride); override != "" {
		return override
	}
	return strings.TrimSpace(cityRuntime)
}
