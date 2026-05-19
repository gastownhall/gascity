package api

import (
	"context"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/sling"
)

// extmsgNotifyTimeout bounds fire-and-forget goroutines spawned from
// extmsg inbound/outbound handlers so they cannot leak across server
// lifetimes or block shutdown on a slow downstream.
const extmsgNotifyTimeout = 30 * time.Second

// backgroundCtx returns a context that is explicitly detached from the
// request but has a bounded timeout. Use for fire-and-forget work
// (extmsg member notification, log-write fanouts) so goroutines cannot
// outlive reasonable bounds. When the server gains a shutdown ctx in
// the future, derive from that instead.
//
// The returned cancel is intentionally captured inside a goroutine that
// exits on ctx.Done(), so go vet's lostcancel check stays happy while
// the timeout still prevents unbounded accumulation.
func (s *Server) backgroundCtx() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), extmsgNotifyTimeout)
	go func() {
		<-ctx.Done()
		cancel()
	}()
	return ctx
}

// Server is the per-city handler-host. It owns the per-city State and
// holds every per-city HTTP handler method (humaHandle*, checkXxxStream,
// streamXxx, handleServiceProxy, etc.). Per-city Huma operations are
// registered on the supervisor's single Huma API at their real
// /v0/city/{cityName}/... paths via SupervisorMux.registerCityRoutes;
// the supervisor resolves and calls these methods through bindCity.
//
// Server's mux is used only for the /svc/* workspace-service
// pass-through, which is explicitly excluded from the typed control
// plane (it proxies arbitrary bodies to user-provided service
// processes).
type Server struct {
	state    State
	mux      *http.ServeMux
	readOnly bool // mirrors supervisor's read-only flag for /svc/ enforcement

	// sessionLogSearchPaths overrides the default search paths for Claude
	// session JSONL files. Nil means use worker.DefaultSearchPaths().
	sessionLogSearchPaths []string

	// idem caches responses for Idempotency-Key replay on create endpoints.
	idem *idempotencyCache

	// lookPathCache caches exec.LookPath results with a short TTL to avoid
	// repeated filesystem scans on every GET /v0/agents request.
	lookPathMu      sync.Mutex
	lookPathEntries map[string]lookPathEntry

	// agentVisibilityWaitTimeout overrides the POST /agents visibility wait
	// in tests. Zero uses defaultAgentVisibilityWaitTimeout.
	agentVisibilityWaitTimeout time.Duration

	// responseCache memoizes expensive read responses for a short TTL so
	// repeated UI polls do not re-run the same bead-store subprocesses when
	// nothing material has changed.
	responseCacheMu      sync.Mutex
	responseCacheEntries map[string]responseCacheEntry

	// LookPathFunc can be overridden in tests. Defaults to exec.LookPath.
	LookPathFunc func(string) (string, error)

	// SlingRunnerFunc can be overridden in tests. When nil, uses a real
	// shell runner. Set this to inject a fake runner for unit tests.
	SlingRunnerFunc sling.SlingRunner
}

type lookPathEntry struct {
	found   bool
	expires time.Time
}

// cachedLookPath checks if a binary is in PATH, caching the result for lookPathCacheTTL.
func (s *Server) cachedLookPath(binary string) bool {
	s.lookPathMu.Lock()
	defer s.lookPathMu.Unlock()

	if s.lookPathEntries == nil {
		s.lookPathEntries = make(map[string]lookPathEntry)
	}

	if e, ok := s.lookPathEntries[binary]; ok && time.Now().Before(e.expires) {
		return e.found
	}

	lookPath := s.LookPathFunc
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	_, err := lookPath(binary)
	found := err == nil
	s.lookPathEntries[binary] = lookPathEntry{found: found, expires: time.Now().Add(lookPathCacheTTL)}
	return found
}

// resolveTitleProvider resolves the workspace default provider for title
// generation. Returns nil if the provider can't be resolved.
func (s *Server) resolveTitleProvider() *config.ResolvedProvider {
	cfg := s.state.Config()
	if cfg == nil {
		return nil
	}
	lookPath := s.LookPathFunc
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	rp, err := config.ResolveProvider(
		&config.Agent{},
		&cfg.Workspace,
		cfg.Providers,
		lookPath,
	)
	if err != nil {
		return nil
	}
	return rp
}

// New creates a per-city Server. The Server owns the per-city State and
// the /svc/* pass-through mux. CSRF and read-only enforcement on the
// typed Huma surface happen on the supervisor's middleware, not here;
// the readOnly flag mirrored on Server is used only by handleServiceProxy
// to gate non-direct service mutations (workspace services live outside
// the typed control plane, so the supervisor's middleware does not run
// for /svc/* requests).
func New(state State) *Server {
	syncFeatureFlags(state.Config())
	return newServer(state, false)
}

// NewReadOnly is New with readOnly=true.
func NewReadOnly(state State) *Server {
	syncFeatureFlags(state.Config())
	return newServer(state, true)
}

func newServer(state State, readOnly bool) *Server {
	mux := http.NewServeMux()
	s := &Server{
		state:    state,
		mux:      mux,
		readOnly: readOnly,
		idem:     newIdempotencyCache(30 * time.Minute),
	}
	mux.HandleFunc("/svc/", s.handleServiceProxy)
	return s
}

// syncFeatureFlags enables/disables graph-formula and graph-apply
// feature flags based on the city's daemon config. Called from New
// and NewReadOnly so both modes observe the same flag state.
func syncFeatureFlags(cfg *config.City) {
	enabled := cfg != nil && cfg.Daemon.FormulaV2
	if formula.IsFormulaV2Enabled() != enabled {
		formula.SetFormulaV2Enabled(enabled)
	}
	if molecule.IsGraphApplyEnabled() != enabled {
		molecule.SetGraphApplyEnabled(enabled)
	}
}

type singleStateResolver struct {
	state State
}

func (r *singleStateResolver) ListCities() []CityInfo {
	return []CityInfo{{
		Name:    r.state.CityName(),
		Path:    r.state.CityPath(),
		Running: true,
	}}
}

func (r *singleStateResolver) CityState(name string) State {
	if name == r.state.CityName() {
		return r.state
	}
	return nil
}

func (s *Server) legacySessionHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v0/sessions", s.handleSessionCreate)
	mux.HandleFunc("GET /v0/sessions", s.handleSessionList)
	mux.HandleFunc("GET /v0/session/{id}", s.handleSessionGet)
	mux.HandleFunc("GET /v0/session/{id}/transcript", s.handleSessionTranscript)
	mux.HandleFunc("GET /v0/session/{id}/pending", s.handleSessionPending)
	mux.HandleFunc("GET /v0/session/{id}/stream", s.handleSessionStream)
	mux.HandleFunc("PATCH /v0/session/{id}", s.handleSessionPatch)
	mux.HandleFunc("POST /v0/session/{id}/messages", s.handleSessionMessage)
	mux.HandleFunc("POST /v0/session/{id}/stop", s.handleSessionStop)
	mux.HandleFunc("POST /v0/session/{id}/kill", s.handleSessionKill)
	mux.HandleFunc("POST /v0/session/{id}/respond", s.handleSessionRespond)
	mux.HandleFunc("POST /v0/session/{id}/suspend", s.handleSessionSuspend)
	mux.HandleFunc("POST /v0/session/{id}/close", s.handleSessionClose)
	mux.HandleFunc("POST /v0/session/{id}/wake", s.handleSessionWake)
	mux.HandleFunc("POST /v0/session/{id}/rename", s.handleSessionRename)
	mux.HandleFunc("GET /v0/session/{id}/agents", s.handleSessionAgentList)
	mux.HandleFunc("GET /v0/session/{id}/agents/{agentId}", s.handleSessionAgentGet)
	return mux
}

func (s *Server) legacyAgentHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/v0/agent/")
		switch {
		case strings.HasSuffix(path, "/output/stream"):
			name := strings.TrimSuffix(path, "/output/stream")
			if strings.TrimSpace(name) == "" {
				http.NotFound(w, r)
				return
			}
			s.handleAgentOutputStream(w, r, name)
		case strings.HasSuffix(path, "/output"):
			name := strings.TrimSuffix(path, "/output")
			if strings.TrimSpace(name) == "" {
				http.NotFound(w, r)
				return
			}
			s.handleAgentOutput(w, r, name)
		default:
			http.NotFound(w, r)
		}
	})
}

// ServeHTTP exists for tests that exercise a caller-provided *Server directly.
// It delegates through the real SupervisorMux so the direct path exercises the
// same typed routes and middleware as production. Legacy no-city session URLs
// are rewritten onto the city-scoped Huma surface for compatibility.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/v0/city/") &&
		(strings.HasPrefix(r.URL.Path, "/v0/session/") || r.URL.Path == "/v0/session" ||
			strings.HasPrefix(r.URL.Path, "/v0/sessions")) {
		s.legacySessionHandler().ServeHTTP(w, r)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/v0/city/") && strings.HasPrefix(r.URL.Path, "/v0/agent/") {
		s.legacyAgentHandler().ServeHTTP(w, r)
		return
	}

	sm := NewSupervisorMux(&singleStateResolver{state: s.state}, nil, s.readOnly, "test", time.Now())
	sm.cacheMu.Lock()
	sm.cache[s.state.CityName()] = cachedCityServer{state: s.state, srv: s}
	sm.cacheMu.Unlock()

	req := r.Clone(r.Context())
	sm.Handler().ServeHTTP(w, req)
}
