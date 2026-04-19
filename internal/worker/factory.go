package worker

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// SessionRuntimeResolver resolves provider/runtime details for an existing
// session-backed worker without exposing SessionSpec mutation to callers.
type SessionRuntimeResolver func(info sessionpkg.Info, sessionKind string) (*ResolvedRuntime, error)

// FactoryConfig constructs worker-owned session handles and catalogs without
// leaking session.Manager setup into higher layers.
type FactoryConfig struct {
	Store                 beads.Store
	Provider              runtime.Provider
	CityPath              string
	SearchPaths           []string
	Recorder              events.Recorder
	ResolveTransport      func(template string) string
	ResolveSessionRuntime SessionRuntimeResolver
}

// Factory centralizes worker-boundary object construction for callers such as
// the API server and gc CLI.
type Factory struct {
	manager               *sessionpkg.Manager
	store                 beads.Store
	provider              runtime.Provider
	searchPaths           []string
	recorder              events.Recorder
	resolveSessionRuntime SessionRuntimeResolver
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
	return newFactory(manager, cfg.Store, cfg.Provider, cfg.SearchPaths, cfg.Recorder, cfg.ResolveSessionRuntime)
}

// NewFactoryFromManager wraps an already-constructed session manager behind the
// worker boundary. Primarily useful in tests.
func NewFactoryFromManager(manager *sessionpkg.Manager, searchPaths []string) (*Factory, error) {
	return newFactory(manager, nil, nil, searchPaths, nil, nil)
}

func newFactory(manager *sessionpkg.Manager, store beads.Store, provider runtime.Provider, searchPaths []string, recorder events.Recorder, resolveRuntime SessionRuntimeResolver) (*Factory, error) {
	if manager == nil {
		return nil, fmt.Errorf("%w: manager is required", ErrHandleConfig)
	}
	return &Factory{
		manager:               manager,
		store:                 store,
		provider:              provider,
		searchPaths:           append([]string(nil), searchPaths...),
		recorder:              recorder,
		resolveSessionRuntime: resolveRuntime,
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
		Recorder:    f.recorder,
		Session:     spec,
	})
}

// SessionByID rebuilds a session-backed worker handle from persisted session
// metadata and the factory's optional resolved-runtime hook.
func (f *Factory) SessionByID(id string) (Handle, error) {
	info, err := f.manager.Get(id)
	if err != nil {
		return nil, err
	}

	spec := SessionSpec{
		ID:       id,
		Template: info.Template,
		Title:    info.Title,
		Alias:    info.Alias,
		Command:  info.Command,
		Provider: info.Provider,
		WorkDir:  info.WorkDir,
		Resume: sessionpkg.ProviderResume{
			ResumeFlag:    info.ResumeFlag,
			ResumeStyle:   info.ResumeStyle,
			ResumeCommand: info.ResumeCommand,
		},
	}
	sessionKind := ""
	if f.store != nil {
		if bead, beadErr := f.store.Get(id); beadErr == nil {
			sessionKind = strings.TrimSpace(bead.Metadata["mc_session_kind"])
			if profile := strings.TrimSpace(bead.Metadata["worker_profile"]); profile != "" {
				spec.Profile = Profile(profile)
			}
		}
	}
	if f.resolveSessionRuntime != nil {
		resolved, err := f.resolveSessionRuntime(info, sessionKind)
		if err != nil {
			return nil, err
		}
		applyResolvedRuntimeToSessionSpec(&spec, resolved)
	}
	return f.Session(spec)
}

// HandleForTarget resolves a session target to a session-backed worker when
// possible, falling back to a runtime-only handle for legacy live sessions.
func (f *Factory) HandleForTarget(target string, processNames []string) (Handle, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil, sessionpkg.ErrSessionNotFound
	}
	if f.store != nil {
		if id, err := sessionpkg.ResolveSessionIDByExactID(f.store, target); err == nil {
			return f.SessionByID(id)
		} else if !errors.Is(err, sessionpkg.ErrSessionNotFound) {
			return nil, err
		}
		if id, err := sessionpkg.ResolveSessionID(f.store, target); err == nil {
			return f.SessionByID(id)
		} else if !errors.Is(err, sessionpkg.ErrSessionNotFound) {
			return nil, err
		}
		if f.provider != nil {
			if sessionID, err := f.provider.GetMeta(target, "GC_SESSION_ID"); err == nil && strings.TrimSpace(sessionID) != "" {
				return f.SessionByID(strings.TrimSpace(sessionID))
			}
		}
	}
	if f.provider == nil {
		return nil, sessionpkg.ErrSessionNotFound
	}
	providerName := strings.TrimSpace(target)
	if liveProvider, err := f.provider.GetMeta(target, "GC_PROVIDER"); err == nil && strings.TrimSpace(liveProvider) != "" {
		providerName = strings.TrimSpace(liveProvider)
	}
	return f.RuntimeHandle(target, providerName, "", processNames)
}

// RuntimeHandle constructs a runtime-only worker handle using the factory's
// configured provider and recorder.
func (f *Factory) RuntimeHandle(sessionName, providerName, transport string, processNames []string) (Handle, error) {
	if f.provider == nil {
		return nil, sessionpkg.ErrSessionNotFound
	}
	return NewRuntimeHandle(RuntimeHandleConfig{
		Provider:     f.provider,
		SessionName:  sessionName,
		ProviderName: providerName,
		Transport:    transport,
		ProcessNames: append([]string(nil), processNames...),
		Recorder:     f.recorder,
	})
}

// Adapter returns a transcript adapter configured with the factory's search
// paths for callers that need transcript reads outside a session handle.
func (f *Factory) Adapter() SessionLogAdapter {
	return SessionLogAdapter{SearchPaths: append([]string(nil), f.searchPaths...)}
}

// DiscoverTranscript returns the best available transcript path for a worker.
func (f *Factory) DiscoverTranscript(provider, workDir, gcSessionID string) string {
	return f.Adapter().DiscoverTranscript(provider, workDir, gcSessionID)
}

// DiscoverWorkDirTranscript resolves the best provider-specific transcript for
// a workdir without requiring a stable session identifier.
func (f *Factory) DiscoverWorkDirTranscript(provider, workDir string) string {
	return f.Adapter().DiscoverWorkDirTranscript(provider, workDir)
}

// TailMeta reads model/context metadata from a discovered transcript path.
func (f *Factory) TailMeta(path string) (*TranscriptTailMeta, error) {
	return f.Adapter().TailMeta(path)
}

// AgentMappings lists subagent transcript mappings for a parent transcript.
func (f *Factory) AgentMappings(path string) ([]AgentMapping, error) {
	return f.Adapter().AgentMappings(path)
}

// ReadAgentTranscript loads a subagent transcript while preserving raw
// message fidelity for worker-owned API surfaces.
func (f *Factory) ReadAgentTranscript(path, agentID string) (*AgentTranscriptResult, error) {
	return f.Adapter().ReadAgentTranscript(path, agentID)
}

// ReadTranscript loads a provider transcript while preserving raw pagination
// and message fidelity for worker-owned API/CLI surfaces.
func (f *Factory) ReadTranscript(req TranscriptRequest) (*TranscriptResult, error) {
	return f.Adapter().ReadTranscript(req)
}

// LoadHistory loads and normalizes a provider transcript.
func (f *Factory) LoadHistory(req LoadRequest) (*HistorySnapshot, error) {
	return f.Adapter().LoadHistory(req)
}
