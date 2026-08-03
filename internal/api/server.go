package api

import (
	"context"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/featureflags"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/sling"
	"github.com/gastownhall/gascity/internal/webhookverify"
	"golang.org/x/sync/singleflight"
)

// extmsgNotifyTimeout bounds fire-and-forget goroutines spawned from
// extmsg inbound/outbound handlers so they cannot leak across server
// lifetimes or block shutdown on a slow downstream.
const extmsgNotifyTimeout = 30 * time.Second

// runBackground owns one detached, bounded task. The task is visible to
// waitForBackground so tests and a future server shutdown path can wait for
// side effects before releasing the state they use.
func (s *Server) runBackground(run func(context.Context)) {
	s.backgroundTasks.Add(1)
	go func() {
		defer s.backgroundTasks.Done()
		ctx, cancel := context.WithTimeout(context.Background(), extmsgNotifyTimeout)
		defer cancel()
		run(ctx)
	}()
}

func (s *Server) waitForBackground() {
	s.backgroundTasks.Wait()
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

	// bootFlags is the rollout-gate snapshot latched at Server construction —
	// from the State's boot latch when it implements RolloutFlagsProvider, else
	// resolved once from Config(). Immutable for the Server lifetime, mirroring
	// readOnly; the S2+/S3 handler consumers read it.
	bootFlags rollout.Flags

	runCensusSource RunCensusSource

	backgroundTasks sync.WaitGroup

	// sessionLogSearchPaths overrides the default search paths for Claude
	// session JSONL files. Nil means use worker.DefaultSearchPaths().
	sessionLogSearchPaths []string

	// structuredPeekPoll overrides the structured fallback stream's periodic
	// history check in tests. Nil uses outputStreamPollInterval.
	structuredPeekPoll <-chan time.Time

	// idem caches responses for Idempotency-Key replay on create endpoints.
	idem *idempotencyCache

	// rigIdem is the in-process live index + request_id state machine backing
	// async server-side rig-create (POST /v0/city/{n}/rigs with a git_url). It
	// starts empty at boot and is authoritative for admission (G13). One index
	// per per-city Server (the supervisor caches one Server per city).
	rigIdem *rigIdemIndex

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

	// storeHealth caches the on-disk size walk and maintenance-log read
	// for /v0/status's StoreHealth block. Refreshed on expiry; missing
	// store directories produce a zero-value entry so repeated requests
	// don't re-walk a fresh city between maintenance runs.
	storeHealthMu       sync.Mutex
	storeHealthEntry    *StatusStoreHealth
	storeHealthExpires  time.Time
	storeHealthComputer func(ctx context.Context) (*StatusStoreHealth, error)
	storeHealthFlight   singleflight.Group

	// componentVersions caches the dolt engine and bd CLI versions the
	// supervisor drives for /v0/status. Binary versions are immutable for
	// the process lifetime, so they are resolved once on first read.
	// componentVersionsProbe overrides the real subprocess probe in tests;
	// nil uses the PATH-resolved binaries.
	componentVersionsOnce  sync.Once
	componentVersionsValue componentVersions
	componentVersionsProbe func() componentVersions

	// dashboardBase reports the browser-reachable base URL of the dashboard
	// mounted on the process serving this city's API, or "" when unmounted.
	// Nil (the default) also means unmounted — the standalone controller
	// [api] port serves /v0 without the SPA — so handlers omit dashboard
	// deep links. Populated from SupervisorMux.WithDashboardBase when the
	// supervisor builds per-city servers.
	dashboardBase func() string

	// LookPathFunc can be overridden in tests. Defaults to exec.LookPath.
	LookPathFunc func(string) (string, error)

	// SlingRunnerFunc can be overridden in tests. When nil, uses a real
	// shell runner. Set this to inject a fake runner for unit tests.
	SlingRunnerFunc sling.SlingRunner

	// webhookEvents overrides the E8 webhook.received / webhook.rejected sink.
	// Nil (the default) forwards to the city event bus via cityEventWebhookSink;
	// tests inject a fake to assert emitted events. See webhookEventSink.
	webhookEvents WebhookEventSink

	// webhookDedup is the E8 delivery-idempotency store, keyed (webhook,
	// delivery-id). Shared across deliveries for the process lifetime of this
	// per-city Server (the supervisor caches one Server per city).
	webhookDedup *webhookDedupCache

	// webhookLimiter is the E8 per-webhook token-bucket rate limiter.
	webhookLimiter *webhookRateLimiter

	// webhookVerifiers memoizes the built E4 verifier per webhook so a stateful
	// verifier (the jwt-jwks JWKS cache) persists across deliveries instead of
	// being rebuilt — and its JWKS refetched — on every request. Keyed by webhook
	// name; a cheap config fingerprint guards each entry so a config hot-reload
	// that changes the verify config rebuilds the verifier. Secret resolution
	// stays per-request; only the verifier (the stateful part) is reused.
	webhookVerifiersMu sync.Mutex
	webhookVerifiers   map[string]cachedWebhookVerifier

	// webhookAccessFaultLogged latches which pre-limiter access-gate operator
	// faults (a misconfigured allowed_cidrs, or an unset/empty bearer_env on a
	// hook that still passes config load) have already been reported, so a flood
	// against a misconfigured public hook logs the fault ONCE, not once per
	// request. These gates run BEFORE the delivery limiter, so — unlike the
	// limiter-throttled verifier fault — an unbounded per-request log/event here
	// would be the CWE-400 amplifier the receiver exists to avoid; the 503 itself
	// is still returned per request (as cheap as the other pre-limiter rejects)
	// and is deliberately non-evented. Keyed by (webhook name, fault detail) so a
	// different or changed misconfiguration reports again; keys derive from
	// operator config, never attacker input, so the set is bounded by config.
	webhookAccessFaultMu     sync.Mutex
	webhookAccessFaultLogged map[string]struct{}

	// webhookMaxBody overrides the /hook/ request body cap in tests. Zero uses
	// defaultMaxWebhookBodyBytes.
	webhookMaxBody int64
}

// cachedWebhookVerifier is a memoized verifier plus the config fingerprint it
// was built from; a fingerprint mismatch on the next delivery triggers a rebuild.
type cachedWebhookVerifier struct {
	verifier    webhookverify.Verifier
	fingerprint string
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
		state:          state,
		mux:            mux,
		readOnly:       readOnly,
		idem:           newIdempotencyCache(30 * time.Minute),
		rigIdem:        newRigIdemIndex(),
		webhookDedup:   newWebhookDedupCache(defaultWebhookDedupTTL),
		webhookLimiter: newWebhookRateLimiter(),
	}
	// Latch the rollout snapshot once: prefer the State's boot latch (the
	// production controllerState); fall back to resolving from Config() for
	// States without it (test fakes). A Resolve error leaves the zero Flags —
	// the documented degraded-safe legacy value; the production root already
	// surfaced the error at boot, and this fallback only runs for provider-less
	// States, so the error is intentionally not re-surfaced here.
	if p, ok := state.(RolloutFlagsProvider); ok {
		s.bootFlags = p.RolloutFlags()
	} else if cfg := state.Config(); cfg != nil {
		s.bootFlags, _ = rollout.Resolve(cfg, rollout.ResolveOptions{})
	}
	mux.HandleFunc("/svc/", s.handleServiceProxy)
	// /hook/* webhook receiver — the fourth sanctioned non-Huma surface. Like
	// /svc/* it is a raw-body pass-through (HMAC/ed25519 sign the exact bytes),
	// so it lives on the per-city mux outside the typed Huma control plane; its
	// gates are the R2 perimeter + E4 signature verification, not Huma middleware.
	mux.HandleFunc("/hook/", s.handleHookProxy)
	return s
}

// syncFeatureFlags enables/disables graph-formula and graph-apply
// feature flags based on the city's daemon config. Called from New
// and NewReadOnly so both modes observe the same flag state.
func syncFeatureFlags(cfg *config.City) {
	featureflags.Apply(featureflags.FromConfig(cfg))
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
	mux.HandleFunc("POST /v0/session/{id}/permission-mode", s.handleSessionPermissionMode)
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

	sm := NewSupervisorMux(&singleStateResolver{state: s.state}, nil, s.readOnly, "test", "", time.Now())
	sm.WithAnyHostAllowed()
	sm.cacheMu.Lock()
	sm.cache[s.state.CityName()] = cachedCityServer{state: s.state, srv: s}
	sm.cacheMu.Unlock()

	req := r.Clone(r.Context())
	sm.Handler().ServeHTTP(w, req)
}
