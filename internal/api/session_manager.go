package api

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

func (s *Server) sessionManager(store beads.Store) *session.Manager {
	cfg := s.state.Config()
	opts := []session.ManagerOption{
		session.WithCityPath(s.state.CityPath()),
		session.WithEventRecorder(s.state.EventProvider()),
	}
	if cfg == nil {
		return session.NewManagerWithOptions(store, s.state.SessionProvider(), opts...)
	}
	opts = append(opts, session.WithTransportPolicyResolver(func(template, provider string) (string, bool) {
		return configuredSessionTransportResolution(cfg, template, provider)
	}))
	return session.NewManagerWithOptions(store, s.state.SessionProvider(), opts...)
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
			return strings.TrimSpace(agentCfg.Session), false
		}
		return config.ResolveSessionCreateTransport(agentCfg.Session, resolved), false
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
}
