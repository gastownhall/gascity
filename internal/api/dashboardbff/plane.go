// Package dashboardbff implements the host-side "/api/*" plane that the gc
// supervisor serves alongside the typed /v0 API and the embedded SPA. It ports
// the irreducible host-side endpoints of the former Node BFF (config
// projection, git/builds reads, run diffs, health probes, and the slow-status
// samplers) into Go. The bulk of the old BFF — the supervisor proxy and every
// per-city data read — is gone: the SPA calls /v0/* directly, same-origin.
//
// This plane is registered as a non-Huma handler on the supervisor mux — one of
// the three sanctioned non-typed surfaces documented in
// engdocs/architecture/api-control-plane.md §3.9 (alongside the /svc/ proxy and
// the embedded SPA) — so it adds no operations to the OpenAPI contract. Because
// it bypasses Huma's CSRF/read-only middleware, it self-enforces both through
// one shared guard (see guard).
package dashboardbff

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// CityResolver resolves a managed city name to its on-disk root path. The
// supervisor's city registry implements this; resolving the path from the
// registry (instead of joining the untrusted name onto a base) keeps
// city-name path traversal out of the host-side plane entirely.
type CityResolver interface {
	CityPath(name string) (path string, ok bool)
}

// Deps are the collaborators the /api plane needs.
type Deps struct {
	Resolver CityResolver
	// ReadOnly mirrors the supervisor's read-only posture; when true every
	// mutation through the plane is refused.
	ReadOnly bool
	// RunCwdAllowedRoots optionally restricts run-diff git reads to these
	// absolute roots (RUN_CWD_ALLOWED_ROOTS). Empty = shape-only validation.
	RunCwdAllowedRoots []string
	// SupervisorBaseURL is the loopback base URL of the supervisor's own typed
	// API (e.g. "http://127.0.0.1:8372"), used by the host-side samplers to
	// read /v0/city/{name}/status. Empty disables the samplers' status reads.
	SupervisorBaseURL string

	// Runtime-config projection inputs. Neutral defaults are supplied by the
	// caller from gc config/env (ZERO hardcoded roles).
	OperatorAlias     string
	OperatorWireAlias string
	DecisionLabel     string
	EnabledModules    []string
	DefaultView       string
}

// Plane is the host-side /api/* HTTP surface. It owns the shared mutation
// guard, the sandboxed exec runner, and the per-city slow-status samplers.
type Plane struct {
	deps       Deps
	exec       *execRunner
	mux        *http.ServeMux
	samplers   *samplerManager
	runTailers *runTailerManager
	localTools *localToolsCache

	wg   sync.WaitGroup
	stop context.CancelFunc
}

// New builds the /api plane. Call Start to enable background samplers and Stop
// to drain them on shutdown.
func New(deps Deps) *Plane {
	p := &Plane{deps: deps, exec: newExecRunner(), mux: http.NewServeMux(), localTools: &localToolsCache{}}
	p.samplers = newSamplerManager(deps, p.exec)
	p.runTailers = newRunTailerManager(deps)
	p.registerRoutes()
	return p
}

// Handler returns the plane handler wrapped in the shared mutation guard. It is
// mounted at /api/ on the supervisor mux and inherits the supervisor's outer
// middleware (logging, recovery, request-id, host/CORS) via Handler().
func (p *Plane) Handler() http.Handler { return p.guard(p.mux) }

// Start enables the per-city samplers. Each city's sampler is launched lazily
// on first request for that city's data (matching the BFF's lazy per-city
// runtime) and runs until ctx is canceled or Stop is called.
func (p *Plane) Start(ctx context.Context) {
	ctx, p.stop = context.WithCancel(ctx)
	p.samplers.enable(ctx, &p.wg)
	p.runTailers.enable(ctx, &p.wg)
}

// Stop signals the samplers to halt and waits for them to drain.
func (p *Plane) Stop() {
	if p.stop != nil {
		p.stop()
	}
	p.wg.Wait()
}

// readOnlySafePostRE matches the run-diff endpoint — the one POST on the plane
// that only READS git state. It carries its execution path in the request body
// (so it cannot be a GET) but mutates nothing, so it must stay reachable on a
// read-only supervisor; classifying the plane's write policy by HTTP method
// alone would otherwise 405 the SPA's run Diff tab on every non-loopback bind.
var readOnlySafePostRE = regexp.MustCompile(`^/api/city/[^/]+/runs/[^/]+/diff$`)

// isReadOnlySafeRequest reports whether an unsafe-method request is in fact a
// pure read that must survive the read-only gate. Only the run-diff POST
// qualifies today; it is still subject to the CSRF checks in guard.
func isReadOnlySafeRequest(r *http.Request) bool {
	return r.Method == http.MethodPost && readOnlySafePostRE.MatchString(r.URL.Path)
}

// guard enforces the plane's write policy. Unsafe-method requests must (a) be
// same-origin and (b) carry a non-empty X-GC-Request header (the supervisor's
// CSRF convention); the same-origin assertion is defense-in-depth so a CORS
// regression elsewhere cannot reopen CSRF on its own. In read-only mode a
// genuine mutation is refused outright, but a read-only-SAFE request (run-diff,
// which only reads git) is classified by semantics rather than method, so it
// passes the read-only gate while staying behind CSRF. Safe methods pass
// straight through. One shared gate so no per-handler check can be forgotten.
func (p *Plane) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if p.deps.ReadOnly && !isReadOnlySafeRequest(r) {
				writeError(w, http.StatusMethodNotAllowed, "dashboard is read-only")
				return
			}
			if !sameOriginMutation(r) {
				writeError(w, http.StatusForbidden, "cross-origin request rejected")
				return
			}
			if strings.TrimSpace(r.Header.Get("X-GC-Request")) == "" {
				writeError(w, http.StatusForbidden, "missing X-GC-Request header")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// sameOriginMutation reports whether an unsafe-method request originates from
// the dashboard's own origin. It trusts the browser-set Sec-Fetch-Site signal
// when present, and otherwise compares the Origin host to the request Host. A
// request with no Origin (common for same-origin navigations/fetches) is
// allowed; a present cross-origin Origin/Sec-Fetch-Site is rejected.
func sameOriginMutation(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return true
	case "cross-site", "same-site":
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return u.Host == r.Host
}

// registerRoutes wires every plane endpoint. Each registerX lives in its own
// file next to the logic it serves.
func (p *Plane) registerRoutes() {
	p.mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, apiHealthResponse{OK: true, TS: time.Now().UTC().Format(time.RFC3339Nano)})
	})
	p.registerConfig()
	p.registerGit()
	p.registerBuilds()
	p.registerClientLog()
	p.registerHealth()
	p.registerRunDiff()
	p.registerSamplers()
	p.registerRunSummary()
}

// resolveCityPath validates a city name and resolves its host root path. It
// returns ("", false) for an unknown or malformed name; callers translate that
// into a 404.
func (p *Plane) resolveCityPath(name string) (string, bool) {
	if !validCityName(name) || p.deps.Resolver == nil {
		return "", false
	}
	return p.deps.Resolver.CityPath(name)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	// Security headers on every /api JSON response (writeError routes through
	// here too): never sniff the type, never frameable.
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// apiHealthResponse is the GET /api/health body. Typed (not map[string]any) so
// every knowable wire shape on this non-typed plane is still a named struct;
// the genuinely-dynamic supervisor-status pass-through (samplers.go) is the
// only json.RawMessage on the plane (see the §3.9 non-typed-plane note in
// engdocs/architecture/api-control-plane.md).
type apiHealthResponse struct {
	OK bool   `json:"ok"`
	TS string `json:"ts"`
}

// apiErrorBody is the shared { "error": msg } shape every plane handler returns
// on failure. Typed so the error wire shape is named like the success shapes
// (the SPA's parseApiErrorBody reads the `error` field).
type apiErrorBody struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, apiErrorBody{Error: msg})
}
