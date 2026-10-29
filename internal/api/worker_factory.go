package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

func (s *Server) workerFactory(store beads.Store) (*worker.Factory, error) {
	cfg := s.state.Config()
	var resolveTransport func(template, provider string) string
	if cfg != nil {
		resolveTransport = func(template, provider string) string {
			return configuredSessionTransport(cfg, template, provider)
		}
	}
	var resolveLaunchEnv func(session.Info, string, map[string]string) (map[string]string, error)
	if p, ok := s.state.(LaunchEnvResolverProvider); ok {
		resolveLaunchEnv = p.LaunchEnvResolver(cfg)
	}
	return worker.NewFactory(worker.FactoryConfig{
		Store:                 store,
		Provider:              s.state.SessionProvider(),
		CityPath:              s.state.CityPath(),
		SearchPaths:           s.sessionLogPaths(),
		Recorder:              s.state.EventProvider(),
		UsageSink:             s.state.UsageSink(),
		ResolveTransport:      resolveTransport,
		ResolveSessionRuntime: s.resolveWorkerSessionRuntimeWithMetadata,
		ResolveLaunchEnv:      resolveLaunchEnv,
		Pricing:               cfg.PricingRegistry(),
		ActivityMemo:          s.activityMemo,
		ResolveBead:           s.formulaBeadResolver(),
		FormulaMemo:           s.formulaMemo,
		RuntimeLeaseTTL:       session.RuntimeLeaseTTLFor(cfg),
		DeferredSubmits:       s.state.NudgeQueue(),
	})
}

func (s *Server) workerSessionCatalog(store beads.Store) (*worker.SessionCatalog, error) {
	factory, err := s.workerFactory(store)
	if err != nil {
		return nil, err
	}
	return factory.Catalog()
}

// formulaBeadResolver is the worker's formula-attribution lookup: the acting
// bead and its run root resolved over this server's residency topology. A clean
// miss is a typed bead-not-found 404 from resolveBeadOwner; the worker's
// contract is beads.ErrNotFound, so the adapter maps one to the other. Any other
// error passes through unchanged.
func (s *Server) formulaBeadResolver() func(id string) (beads.Bead, error) {
	return func(id string) (beads.Bead, error) {
		_, b, err := s.resolveBeadOwner(id)
		if err != nil {
			var typed interface{ GetStatus() int }
			if errors.As(err, &typed) && typed.GetStatus() == http.StatusNotFound {
				return beads.Bead{}, fmt.Errorf("%w: %w", beads.ErrNotFound, err)
			}
			return beads.Bead{}, err
		}
		return b, nil
	}
}
