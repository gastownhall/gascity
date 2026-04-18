package api

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

func (s *Server) sessionManager(store beads.Store) *session.Manager {
	cfg := s.state.Config()
	if cfg == nil {
		return session.NewManagerWithCityPath(store, s.state.SessionProvider(), s.state.CityPath())
	}
	return session.NewManagerWithTransportResolverAndCityPath(store, s.state.SessionProvider(), s.state.CityPath(), func(template string) string {
		agentCfg, ok := resolveSessionTemplateAgent(cfg, template)
		if !ok {
			return ""
		}
		return agentCfg.Session
	})
}

func (s *Server) workerSessionCatalog(store beads.Store) (*worker.SessionCatalog, error) {
	return worker.NewSessionCatalog(s.sessionManager(store))
}
