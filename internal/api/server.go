package api

import (
	"context"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/sling"
)

// Server is the GC API HTTP server. It serves /v0/* endpoints and /health.
type Server struct {
	state    State
	mux      *http.ServeMux
	humaAPI  huma.API // OpenAPI 3.1 spec generation and typed endpoint registration
	server   *http.Server
	readOnly bool // when true, POST endpoints return 403

	// sessionLogSearchPaths overrides the default search paths for Claude
	// session JSONL files. Nil means use sessionlog.DefaultSearchPaths().
	sessionLogSearchPaths []string

	// idem caches responses for Idempotency-Key replay on create endpoints.
	idem *idempotencyCache

	// lookPathCache caches exec.LookPath results with a short TTL to avoid
	// repeated filesystem scans on every GET /v0/agents request.
	lookPathMu      sync.Mutex
	lookPathEntries map[string]lookPathEntry

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

	// testSupervisor is lazily populated on first ServeHTTP call so the
	// test shim can dispatch through a real SupervisorMux. Never set in
	// production.
	testSupervisor *SupervisorMux
	testHandler    http.Handler
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

// newHumaAPIOnce installs one-time global Huma configuration (error handling
// tweaks). Called from newHumaAPI but idempotent via sync.Once so multiple
// city servers in the same process don't fight over huma.NewError.
var newHumaAPIOnce sync.Once

// newHumaAPI creates a Huma API adapter wrapping the given mux. The adapter
// auto-registers /openapi.json, /openapi.yaml, and /docs on the mux.
//
// CSRF and read-only middleware are attached via api.UseMiddleware here
// (Phase 3 Fix 3d). Per-city Huma ops therefore enforce those policies
// directly and emit RFC 9457 Problem Details on rejection — matching the
// supervisor API's middleware model.
func newHumaAPI(mux *http.ServeMux, readOnly bool) huma.API {
	newHumaAPIOnce.Do(configureHumaGlobals)

	cfg := huma.DefaultConfig("Gas City API", "0.1.0")
	cfg.Info.Description = "Gas City orchestration API"
	// Disable $schema links in response bodies and Link headers — they change
	// the wire format from the original handlers and break backward compatibility.
	// The CreateHooks in DefaultConfig add a SchemaLinkTransformer.
	cfg.SchemasPath = ""
	cfg.CreateHooks = nil
	api := humago.New(mux, cfg)
	api.UseMiddleware(humaCSRFMiddleware(api))
	if readOnly {
		api.UseMiddleware(humaReadOnlyMiddleware(api))
	}
	return api
}

// configureHumaGlobals installs process-wide Huma configuration.
//
// Phase 3 Fix 3k removed the 422→400 override that kept the legacy
// `client.go` parser working. The generated client (Phase 3 Fix 3a) parses
// 422 Problem Details natively, so the spec can now accurately report 422
// on validation failures.
func configureHumaGlobals() {
	// Reserved for future process-wide Huma configuration.
}

// New creates a Server with all routes registered. Does not start listening.
func New(state State) *Server {
	mux := http.NewServeMux()
	s := &Server{
		state:   state,
		mux:     mux,
		humaAPI: newHumaAPI(mux, false),
		idem:    newIdempotencyCache(30 * time.Minute),
	}
	s.registerRoutes()
	return s
}

// NewReadOnly creates a read-only Server that rejects all mutation requests.
// Use this when the server binds to a non-localhost address.
func NewReadOnly(state State) *Server {
	mux := http.NewServeMux()
	s := &Server{
		state:    state,
		mux:      mux,
		humaAPI:  newHumaAPI(mux, true),
		readOnly: true,
		idem:     newIdempotencyCache(30 * time.Minute),
	}
	s.registerRoutes()
	return s
}

// ServeHTTP is a TEST-ONLY shim. Production HTTP is served by
// SupervisorMux directly at the real scoped paths; the OpenAPI spec
// reflects only those real paths. This shim wraps the Server in a
// single-city SupervisorMux and rewrites bare "/v0/foo" (and "/health")
// request paths to "/v0/city/{cityName}/foo" before dispatch, so
// pre-migration tests that hit bare URLs keep working while the
// codebase transitions. Delete it once all tests use newTestCityHandler
// + cityURL directly.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Huma-auto-registered spec/docs endpoints dispatch to this Server's
	// own mux directly. specmerge relies on this to read the per-city
	// spec; if we routed these through testSupervisor we'd get the
	// supervisor's spec instead.
	path := r.URL.Path
	if strings.HasPrefix(path, "/openapi") || path == "/docs" {
		s.mux.ServeHTTP(w, r)
		return
	}
	if s.testSupervisor == nil {
		resolver := &singleCityResolver{name: s.state.CityName(), state: s.state}
		sm := NewSupervisorMux(resolver, s.readOnly, "test", time.Now())
		// Pre-populate the per-city Server cache with THIS Server so
		// handlers run with this Server's injected test fields (LookPathFunc,
		// SlingRunnerFunc, sessionLogSearchPaths, idempotency cache, etc.).
		sm.cache[s.state.CityName()] = cachedCityServer{state: s.state, srv: s}
		s.testSupervisor = sm
		// Apply the same middleware the supervisor's real Handler() does,
		// minus the pprof listener goroutine, so tests see identical wire
		// behavior (CORS, request-id, logging, panic recovery).
		inner := http.HandlerFunc(sm.ServeHTTP)
		s.testHandler = withLogging(withRecovery(withRequestID(withCORS(inner))))
	}
	r2 := r.Clone(r.Context())
	r2.URL.Path = testShimRewritePath(r.URL.Path, s.state.CityName())
	r2.URL.RawPath = ""
	s.testHandler.ServeHTTP(w, r2)
}

// testShimRewritePath is the path-rewrite half of Server.ServeHTTP's
// test shim. Exposed as a package-private function for table testing.
func testShimRewritePath(path, cityName string) string {
	// Already scoped (/v0/city/<something>) — no rewrite.
	if strings.HasPrefix(path, "/v0/city/") {
		return path
	}
	// Bare /v0/city → /v0/city/{name} (city detail).
	if path == "/v0/city" {
		return "/v0/city/" + cityName
	}
	// /openapi* and /docs pass through for spec fetches.
	if strings.HasPrefix(path, "/openapi") || path == "/docs" {
		return path
	}
	// Bare /svc/foo → /v0/city/{name}/svc/foo (supervisor forwarder strips
	// the city scope and routes to per-city Server.mux's /svc handler).
	if strings.HasPrefix(path, "/svc/") {
		return "/v0/city/" + cityName + path
	}
	// /health on per-city maps to /v0/city/{name}/health.
	if path == "/health" {
		return "/v0/city/" + cityName + "/health"
	}
	// /v0/foo → /v0/city/{name}/foo.
	if strings.HasPrefix(path, "/v0/") {
		return "/v0/city/" + cityName + strings.TrimPrefix(path, "/v0")
	}
	return path
}

// singleCityResolver is a CityResolver used by Server.ServeHTTP (test
// shim) when wrapping a lone Server in a SupervisorMux.
type singleCityResolver struct {
	name  string
	state State
}

func (r *singleCityResolver) ListCities() []CityInfo {
	return []CityInfo{{Name: r.name, Running: true}}
}

func (r *singleCityResolver) CityState(name string) State {
	if name == r.name {
		return r.state
	}
	return nil
}

func (s *Server) handler() http.Handler {
	// CSRF and read-only are enforced inside Huma via api.UseMiddleware
	// (see newHumaAPI). /svc/* routes bypass Huma entirely and apply
	// their own publication + CSRF rules in handleServiceProxy.
	return withLogging(withRecovery(withRequestID(withCORS(s.mux))))
}

// ListenAndServe starts the HTTP listener. Blocks until stopped.
func (s *Server) ListenAndServe(addr string) error {
	s.server = &http.Server{
		Addr:    addr,
		Handler: s.handler(),
	}
	return s.server.ListenAndServe()
}

// Serve accepts connections on lis. Blocks until stopped.
// Use this with a pre-created listener for synchronous bind validation.
func (s *Server) Serve(lis net.Listener) error {
	s.server = &http.Server{
		Handler: s.handler(),
	}
	return s.server.Serve(lis)
}

// Shutdown gracefully shuts down the server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.server == nil {
		return nil
	}
	return s.server.Shutdown(ctx)
}

func (s *Server) registerRoutes() {
	// Status / Health / City / Readiness / Provider-readiness moved to
	// SupervisorMux.registerCityRoutes at scoped paths. Per-city POST
	// /v0/city was removed — city creation is supervisor-scope only
	// (humaHandleCityCreate in huma_handlers_supervisor.go).

	// Providers / Rigs / Patches moved to
	// SupervisorMux.registerCityRoutes at scoped paths.
	// Agent routes stay here until the SSE output streams migrate — the
	// /v0/agent/{name...} catch-all on supervisor would otherwise shadow
	// the per-city SSE /v0/agent/{base}/output/stream through the
	// legacyCityForwarder.
	huma.Get(s.humaAPI, "/v0/agents", s.humaHandleAgentList)
	s.registerAgentOutputStreamRoutes()
	huma.Get(s.humaAPI, "/v0/agent/{dir}/{base}/output", s.humaHandleAgentOutputQualified)
	huma.Get(s.humaAPI, "/v0/agent/{base}/output", s.humaHandleAgentOutput)
	huma.Get(s.humaAPI, "/v0/agent/{name...}", s.humaHandleAgent)
	huma.Register(s.humaAPI, huma.Operation{
		OperationID:   "create-agent",
		Method:        http.MethodPost,
		Path:          "/v0/agents",
		Summary:       "Create an agent",
		DefaultStatus: http.StatusCreated,
	}, s.humaHandleAgentCreate)
	huma.Patch(s.humaAPI, "/v0/agent/{name...}", s.humaHandleAgentUpdate)
	huma.Delete(s.humaAPI, "/v0/agent/{name...}", s.humaHandleAgentDelete)
	huma.Post(s.humaAPI, "/v0/agent/{name...}", s.humaHandleAgentAction)

	// Beads / Mail / Convoys / Orders / Formulas / Workflow
	// moved to SupervisorMux.registerCityRoutes at scoped paths.
	// Events list/emit moved to scoped; the /v0/events/stream SSE stream
	// stays on per-city until SSE migration.
	s.registerEventStreamRoute()

	// Sessions — Huma handlers
	huma.Register(s.humaAPI, huma.Operation{
		OperationID:   "create-session",
		Method:        http.MethodPost,
		Path:          "/v0/sessions",
		Summary:       "Create a session",
		DefaultStatus: http.StatusAccepted,
	}, s.humaHandleSessionCreate)
	huma.Get(s.humaAPI, "/v0/sessions", s.humaHandleSessionList)
	huma.Get(s.humaAPI, "/v0/session/{id}", s.humaHandleSessionGet)
	huma.Get(s.humaAPI, "/v0/session/{id}/transcript", s.humaHandleSessionTranscript)
	huma.Get(s.humaAPI, "/v0/session/{id}/pending", s.humaHandleSessionPending)
	// Session stream — SSE streaming via Huma StreamResponse
	s.registerSessionStreamRoute()
	huma.Patch(s.humaAPI, "/v0/session/{id}", s.humaHandleSessionPatch)
	huma.Register(s.humaAPI, huma.Operation{
		OperationID:   "submit-session",
		Method:        http.MethodPost,
		Path:          "/v0/session/{id}/submit",
		Summary:       "Submit a message to a session",
		DefaultStatus: http.StatusAccepted,
	}, s.humaHandleSessionSubmit)
	huma.Register(s.humaAPI, huma.Operation{
		OperationID:   "send-session-message",
		Method:        http.MethodPost,
		Path:          "/v0/session/{id}/messages",
		Summary:       "Send a message to a session",
		DefaultStatus: http.StatusAccepted,
	}, s.humaHandleSessionMessage)
	huma.Post(s.humaAPI, "/v0/session/{id}/stop", s.humaHandleSessionStop)
	huma.Post(s.humaAPI, "/v0/session/{id}/kill", s.humaHandleSessionKill)
	huma.Register(s.humaAPI, huma.Operation{
		OperationID:   "respond-session",
		Method:        http.MethodPost,
		Path:          "/v0/session/{id}/respond",
		Summary:       "Respond to a pending interaction",
		DefaultStatus: http.StatusAccepted,
	}, s.humaHandleSessionRespond)
	huma.Post(s.humaAPI, "/v0/session/{id}/suspend", s.humaHandleSessionSuspend)
	huma.Post(s.humaAPI, "/v0/session/{id}/close", s.humaHandleSessionClose)
	huma.Post(s.humaAPI, "/v0/session/{id}/wake", s.humaHandleSessionWake)
	huma.Post(s.humaAPI, "/v0/session/{id}/rename", s.humaHandleSessionRename)
	huma.Get(s.humaAPI, "/v0/session/{id}/agents", s.humaHandleSessionAgentList)
	huma.Get(s.humaAPI, "/v0/session/{id}/agents/{agentId}", s.humaHandleSessionAgentGet)

	// Packs / Sling / Services migrated to scoped paths.
	// Service proxy /svc/* stays on the per-city mux (pass-through).
	s.mux.HandleFunc("/svc/", s.handleServiceProxy)

	// External messaging (extmsg)
	huma.Post(s.humaAPI, "/v0/extmsg/inbound", s.humaHandleExtMsgInbound)
	huma.Post(s.humaAPI, "/v0/extmsg/outbound", s.humaHandleExtMsgOutbound)
	huma.Get(s.humaAPI, "/v0/extmsg/bindings", s.humaHandleExtMsgBindingList)
	huma.Post(s.humaAPI, "/v0/extmsg/bind", s.humaHandleExtMsgBind)
	huma.Post(s.humaAPI, "/v0/extmsg/unbind", s.humaHandleExtMsgUnbind)
	huma.Get(s.humaAPI, "/v0/extmsg/groups", s.humaHandleExtMsgGroupLookup)
	huma.Register(s.humaAPI, huma.Operation{
		OperationID:   "ensure-extmsg-group",
		Method:        http.MethodPost,
		Path:          "/v0/extmsg/groups",
		Summary:       "Ensure an external messaging group exists",
		DefaultStatus: http.StatusCreated,
	}, s.humaHandleExtMsgGroupEnsure)
	huma.Post(s.humaAPI, "/v0/extmsg/participants", s.humaHandleExtMsgParticipantUpsert)
	huma.Delete(s.humaAPI, "/v0/extmsg/participants", s.humaHandleExtMsgParticipantRemove)
	huma.Get(s.humaAPI, "/v0/extmsg/transcript", s.humaHandleExtMsgTranscriptList)
	huma.Post(s.humaAPI, "/v0/extmsg/transcript/ack", s.humaHandleExtMsgTranscriptAck)
	huma.Get(s.humaAPI, "/v0/extmsg/adapters", s.humaHandleExtMsgAdapterList)
	huma.Register(s.humaAPI, huma.Operation{
		OperationID:   "register-extmsg-adapter",
		Method:        http.MethodPost,
		Path:          "/v0/extmsg/adapters",
		Summary:       "Register an external messaging adapter",
		DefaultStatus: http.StatusCreated,
	}, s.humaHandleExtMsgAdapterRegister)
	huma.Delete(s.humaAPI, "/v0/extmsg/adapters", s.humaHandleExtMsgAdapterUnregister)
}
