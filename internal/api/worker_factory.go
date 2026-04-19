package api

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/worker"
)

func (s *Server) workerFactory(store beads.Store) (*worker.Factory, error) {
	cfg := s.state.Config()
	var resolveTransport func(template string) string
	if cfg != nil {
		resolveTransport = func(template string) string {
			agentCfg, ok := resolveSessionTemplateAgent(cfg, template)
			if !ok {
				return ""
			}
			return agentCfg.Session
		}
	}
	return worker.NewFactory(worker.FactoryConfig{
		Store:                 store,
		Provider:              s.state.SessionProvider(),
		CityPath:              s.state.CityPath(),
		SearchPaths:           s.sessionLogPaths(),
		Recorder:              s.state.EventProvider(),
		ResolveTransport:      resolveTransport,
		ResolveSessionRuntime: s.resolveWorkerSessionRuntime,
	})
}

func (s *Server) workerSessionCatalog(store beads.Store) (*worker.SessionCatalog, error) {
	factory, err := s.workerFactory(store)
	if err != nil {
		return nil, err
	}
	return factory.Catalog()
}
