package worker

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// FactoryConfig constructs worker-owned session handles and catalogs without
// leaking session.Manager setup into higher layers.
type FactoryConfig struct {
	Store            beads.Store
	Provider         runtime.Provider
	CityPath         string
	SearchPaths      []string
	ResolveTransport func(template string) string
}

// Factory centralizes worker-boundary object construction for callers such as
// the API server and gc CLI.
type Factory struct {
	manager     *sessionpkg.Manager
	searchPaths []string
}

// NewFactory constructs a Factory backed by a session.Manager configured for
// the caller's city/runtime context.
func NewFactory(cfg FactoryConfig) (*Factory, error) {
	var manager *sessionpkg.Manager
	switch {
	case cfg.ResolveTransport != nil:
		manager = sessionpkg.NewManagerWithTransportResolverAndCityPath(cfg.Store, cfg.Provider, cfg.CityPath, cfg.ResolveTransport)
	case cfg.CityPath != "":
		manager = sessionpkg.NewManagerWithCityPath(cfg.Store, cfg.Provider, cfg.CityPath)
	default:
		manager = sessionpkg.NewManager(cfg.Store, cfg.Provider)
	}
	return NewFactoryFromManager(manager, cfg.SearchPaths)
}

// NewFactoryFromManager wraps an already-constructed session manager behind the
// worker boundary. Primarily useful in tests.
func NewFactoryFromManager(manager *sessionpkg.Manager, searchPaths []string) (*Factory, error) {
	if manager == nil {
		return nil, fmt.Errorf("%w: manager is required", ErrHandleConfig)
	}
	return &Factory{
		manager:     manager,
		searchPaths: append([]string(nil), searchPaths...),
	}, nil
}

// Catalog returns a worker-owned session catalog backed by the factory's
// session manager.
func (f *Factory) Catalog() (*SessionCatalog, error) {
	return NewSessionCatalog(f.manager)
}

// Session returns a worker-owned session handle backed by the factory's
// session manager and transcript search paths.
func (f *Factory) Session(spec SessionSpec) (*SessionHandle, error) {
	return NewSessionHandle(SessionHandleConfig{
		Manager:     f.manager,
		SearchPaths: append([]string(nil), f.searchPaths...),
		Session:     spec,
	})
}

// Adapter returns a transcript adapter configured with the factory's search
// paths for callers that need transcript reads outside a session handle.
func (f *Factory) Adapter() SessionLogAdapter {
	return SessionLogAdapter{SearchPaths: append([]string(nil), f.searchPaths...)}
}
