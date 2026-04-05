package api

import (
	"context"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// Server is the GC API HTTP server. It serves /v0/* endpoints and /health.
type Server struct {
	state    State
	mux      *http.ServeMux
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

// New creates a Server with all routes registered. Does not start listening.
func New(state State) *Server {
	s := &Server{
		state: state,
		mux:   http.NewServeMux(),
		idem:  newIdempotencyCache(30 * time.Minute),
	}
	s.registerRoutes()
	return s
}

// NewReadOnly creates a read-only Server that rejects all mutation requests.
// Use this when the server binds to a non-localhost address.
func NewReadOnly(state State) *Server {
	s := &Server{
		state:    state,
		mux:      http.NewServeMux(),
		readOnly: true,
		idem:     newIdempotencyCache(30 * time.Minute),
	}
	s.registerRoutes()
	return s
}

// ServeHTTP implements http.Handler for testing with httptest.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler().ServeHTTP(w, r)
}

func (s *Server) handler() http.Handler {
	apiInner := withCSRFCheck(s.mux)
	if s.readOnly {
		apiInner = withReadOnly(apiInner)
	}
	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/svc/") {
			// Workspace services apply their own publication and CSRF rules in
			// handleServiceProxy; they do not inherit controller API policy.
			s.mux.ServeHTTP(w, r)
			return
		}
		apiInner.ServeHTTP(w, r)
	})
	return withLogging(withRecovery(withRequestID(withCORS(root))))
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
	// Status + Health
	s.mux.HandleFunc("GET /v0/status", s.handleStatus)
	s.mux.HandleFunc("GET /health", s.handleHealth)

	// City
	s.mux.HandleFunc("GET /v0/provider-readiness", handleProviderReadiness)
	s.mux.HandleFunc("GET /v0/readiness", handleReadiness)
	s.mux.HandleFunc("GET /v0/city", s.handleCityGet)
	s.mux.HandleFunc("PATCH /v0/city", s.handleCityPatch)
	s.mux.HandleFunc("POST /v0/city", handleCityCreate)

	// Agents — read
	s.mux.HandleFunc("GET /v0/agents", s.handleAgentList)
	s.mux.HandleFunc("GET /v0/agent/{name...}", s.handleAgent)
	// Agents — CRUD
	s.mux.HandleFunc("POST /v0/agents", s.handleAgentCreate)
	s.mux.HandleFunc("PATCH /v0/agent/{name...}", s.handleAgentUpdate)
	s.mux.HandleFunc("DELETE /v0/agent/{name...}", s.handleAgentDelete)
	// Agents — actions
	s.mux.HandleFunc("POST /v0/agent/{name...}", s.handleAgentAction)

	// Config
	s.mux.HandleFunc("GET /v0/config", s.handleConfigGet)
	s.mux.HandleFunc("GET /v0/config/explain", s.handleConfigExplain)
	s.mux.HandleFunc("GET /v0/config/validate", s.handleConfigValidate)

	// Patches — agent patches
	s.mux.HandleFunc("GET /v0/patches/agents", s.handleAgentPatchList)
	s.mux.HandleFunc("GET /v0/patches/agent/{name...}", s.handleAgentPatchGet)
	s.mux.HandleFunc("PUT /v0/patches/agents", s.handleAgentPatchSet)
	s.mux.HandleFunc("DELETE /v0/patches/agent/{name...}", s.handleAgentPatchDelete)
	// Patches — rig patches
	s.mux.HandleFunc("GET /v0/patches/rigs", s.handleRigPatchList)
	s.mux.HandleFunc("GET /v0/patches/rig/{name}", s.handleRigPatchGet)
	s.mux.HandleFunc("PUT /v0/patches/rigs", s.handleRigPatchSet)
	s.mux.HandleFunc("DELETE /v0/patches/rig/{name}", s.handleRigPatchDelete)
	// Patches — provider patches
	s.mux.HandleFunc("GET /v0/patches/providers", s.handleProviderPatchList)
	s.mux.HandleFunc("GET /v0/patches/provider/{name}", s.handleProviderPatchGet)
	s.mux.HandleFunc("PUT /v0/patches/providers", s.handleProviderPatchSet)
	s.mux.HandleFunc("DELETE /v0/patches/provider/{name}", s.handleProviderPatchDelete)

	// Providers — read
	s.mux.HandleFunc("GET /v0/providers", s.handleProviderList)
	s.mux.HandleFunc("GET /v0/provider/{name}", s.handleProviderGet)
	// Providers — CRUD
	s.mux.HandleFunc("POST /v0/providers", s.handleProviderCreate)
	s.mux.HandleFunc("PATCH /v0/provider/{name}", s.handleProviderUpdate)
	s.mux.HandleFunc("DELETE /v0/provider/{name}", s.handleProviderDelete)

	// Rigs — read
	s.mux.HandleFunc("GET /v0/rigs", s.handleRigList)
	s.mux.HandleFunc("GET /v0/rig/{name}", s.handleRig)
	// Rigs — CRUD
	s.mux.HandleFunc("POST /v0/rigs", s.handleRigCreate)
	s.mux.HandleFunc("PATCH /v0/rig/{name}", s.handleRigUpdate)
	s.mux.HandleFunc("DELETE /v0/rig/{name}", s.handleRigDelete)
	// Rigs — actions
	s.mux.HandleFunc("POST /v0/rig/{name}/{action}", s.handleRigAction)

	// Beads
	s.mux.HandleFunc("GET /v0/beads", s.handleBeadList)
	s.mux.HandleFunc("GET /v0/beads/graph/{rootID}", s.handleBeadGraph)
	s.mux.HandleFunc("GET /v0/beads/ready", s.handleBeadReady)
	s.mux.HandleFunc("POST /v0/beads", s.handleBeadCreate)
	s.mux.HandleFunc("GET /v0/bead/{id}", s.handleBeadGet)
	s.mux.HandleFunc("GET /v0/bead/{id}/deps", s.handleBeadDeps)
	s.mux.HandleFunc("POST /v0/bead/{id}/close", s.handleBeadClose)
	s.mux.HandleFunc("POST /v0/bead/{id}/reopen", s.handleBeadReopen)
	s.mux.HandleFunc("POST /v0/bead/{id}/update", s.handleBeadUpdate)
	s.mux.HandleFunc("PATCH /v0/bead/{id}", s.handleBeadUpdate)
	s.mux.HandleFunc("POST /v0/bead/{id}/assign", s.handleBeadAssign)
	s.mux.HandleFunc("DELETE /v0/bead/{id}", s.handleBeadDelete)

	// Mail
	s.mux.HandleFunc("GET /v0/mail", s.handleMailList)
	s.mux.HandleFunc("POST /v0/mail", s.handleMailSend)
	s.mux.HandleFunc("GET /v0/mail/count", s.handleMailCount)
	s.mux.HandleFunc("GET /v0/mail/thread/{id}", s.handleMailThread)
	s.mux.HandleFunc("GET /v0/mail/{id}", s.handleMailGet)
	s.mux.HandleFunc("POST /v0/mail/{id}/read", s.handleMailRead)
	s.mux.HandleFunc("POST /v0/mail/{id}/mark-unread", s.handleMailMarkUnread)
	s.mux.HandleFunc("POST /v0/mail/{id}/archive", s.handleMailArchive)
	s.mux.HandleFunc("POST /v0/mail/{id}/reply", s.handleMailReply)
	s.mux.HandleFunc("DELETE /v0/mail/{id}", s.handleMailDelete)

	// Convoys
	s.mux.HandleFunc("GET /v0/convoys", s.handleConvoyList)
	s.mux.HandleFunc("POST /v0/convoys", s.handleConvoyCreate)
	s.mux.HandleFunc("GET /v0/convoy/{id}", s.handleConvoyGet)
	s.mux.HandleFunc("POST /v0/convoy/{id}/add", s.handleConvoyAdd)
	s.mux.HandleFunc("POST /v0/convoy/{id}/remove", s.handleConvoyRemove)
	s.mux.HandleFunc("GET /v0/convoy/{id}/check", s.handleConvoyCheck)
	s.mux.HandleFunc("POST /v0/convoy/{id}/close", s.handleConvoyClose)
	s.mux.HandleFunc("DELETE /v0/convoy/{id}", s.handleConvoyDelete)

	// Events
	s.mux.HandleFunc("GET /v0/events", s.handleEventList)
	s.mux.HandleFunc("GET /v0/events/stream", s.handleEventStream)
	s.mux.HandleFunc("POST /v0/events", s.handleEventEmit)

	// Orders
	s.mux.HandleFunc("GET /v0/orders", s.handleOrderList)
	s.mux.HandleFunc("GET /v0/orders/feed", s.handleOrdersFeed)
	s.mux.HandleFunc("GET /v0/orders/check", s.handleOrderCheck)
	s.mux.HandleFunc("GET /v0/orders/history", s.handleOrderHistory)
	s.mux.HandleFunc("GET /v0/order/history/{bead_id}", s.handleOrderHistoryDetail)
	s.mux.HandleFunc("GET /v0/order/{name}", s.handleOrderGet)
	s.mux.HandleFunc("POST /v0/order/{name}/enable", s.handleOrderEnable)
	s.mux.HandleFunc("POST /v0/order/{name}/disable", s.handleOrderDisable)
	s.mux.HandleFunc("GET /v0/formulas", s.handleFormulaList)
	s.mux.HandleFunc("GET /v0/formulas/{name}/runs", s.handleFormulaRuns)
	s.mux.HandleFunc("GET /v0/formulas/{name}", s.handleFormulaDetail)
	s.mux.HandleFunc("GET /v0/formula/{name}", s.handleFormulaDetail)
	// Backwards-compatible aliases for the old /v0/workflow routes.
	// New code uses /v0/convoy/{id} which delegates to the graph handler
	// for formula-compiled convoys.
	s.mux.HandleFunc("GET /v0/workflow/{workflow_id}", s.handleWorkflowGet)
	s.mux.HandleFunc("DELETE /v0/workflow/{workflow_id}", s.handleWorkflowDelete)

	// Sessions (chat sessions) — id accepts bead ID, alias, or runtime session_name
	s.mux.HandleFunc("POST /v0/sessions", s.handleSessionCreate)
	s.mux.HandleFunc("GET /v0/sessions", s.handleSessionList)
	s.mux.HandleFunc("GET /v0/session/{id}", s.handleSessionGet)
	s.mux.HandleFunc("GET /v0/session/{id}/transcript", s.handleSessionTranscript)
	s.mux.HandleFunc("GET /v0/session/{id}/pending", s.handleSessionPending)
	s.mux.HandleFunc("GET /v0/session/{id}/stream", s.handleSessionStream)
	s.mux.HandleFunc("PATCH /v0/session/{id}", s.handleSessionPatch)
	s.mux.HandleFunc("POST /v0/session/{id}/messages", s.handleSessionMessage)
	s.mux.HandleFunc("POST /v0/session/{id}/stop", s.handleSessionStop)
	s.mux.HandleFunc("POST /v0/session/{id}/kill", s.handleSessionKill)
	s.mux.HandleFunc("POST /v0/session/{id}/respond", s.handleSessionRespond)
	s.mux.HandleFunc("POST /v0/session/{id}/suspend", s.handleSessionSuspend)
	s.mux.HandleFunc("POST /v0/session/{id}/close", s.handleSessionClose)
	s.mux.HandleFunc("POST /v0/session/{id}/wake", s.handleSessionWake)
	s.mux.HandleFunc("POST /v0/session/{id}/rename", s.handleSessionRename)
	s.mux.HandleFunc("GET /v0/session/{id}/agents", s.handleSessionAgentList)
	s.mux.HandleFunc("GET /v0/session/{id}/agents/{agentId}", s.handleSessionAgentGet)

	// Packs
	s.mux.HandleFunc("GET /v0/packs", s.handlePackList)

	// Sling (dispatch)
	s.mux.HandleFunc("POST /v0/sling", s.handleSling)

	// Workspace services
	s.mux.HandleFunc("GET /v0/services", s.handleServiceList)
	s.mux.HandleFunc("GET /v0/service/{name}", s.handleServiceGet)
	s.mux.HandleFunc("POST /v0/service/{name}/restart", s.handleServiceRestart)
	s.mux.HandleFunc("/svc/", s.handleServiceProxy)

	// External messaging (extmsg)
	s.mux.HandleFunc("POST /v0/extmsg/inbound", s.handleExtMsgInbound)
	s.mux.HandleFunc("POST /v0/extmsg/outbound", s.handleExtMsgOutbound)
	s.mux.HandleFunc("GET /v0/extmsg/bindings", s.handleExtMsgBindingList)
	s.mux.HandleFunc("POST /v0/extmsg/bind", s.handleExtMsgBind)
	s.mux.HandleFunc("POST /v0/extmsg/unbind", s.handleExtMsgUnbind)
	s.mux.HandleFunc("GET /v0/extmsg/groups", s.handleExtMsgGroupLookup)
	s.mux.HandleFunc("POST /v0/extmsg/groups", s.handleExtMsgGroupEnsure)
	s.mux.HandleFunc("POST /v0/extmsg/participants", s.handleExtMsgParticipantUpsert)
	s.mux.HandleFunc("DELETE /v0/extmsg/participants", s.handleExtMsgParticipantRemove)
	s.mux.HandleFunc("GET /v0/extmsg/transcript", s.handleExtMsgTranscriptList)
	s.mux.HandleFunc("POST /v0/extmsg/transcript/ack", s.handleExtMsgTranscriptAck)
	s.mux.HandleFunc("GET /v0/extmsg/adapters", s.handleExtMsgAdapterList)
	s.mux.HandleFunc("POST /v0/extmsg/adapters", s.handleExtMsgAdapterRegister)
	s.mux.HandleFunc("DELETE /v0/extmsg/adapters", s.handleExtMsgAdapterUnregister)
}
