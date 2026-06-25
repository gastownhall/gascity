// Package dashboardbff implements the host-side "/api/*" plane that the gc
// supervisor serves alongside the typed /v0 API and the embedded SPA. It ports
// the irreducible host-side endpoints of the former Node BFF (config
// projection, git/builds reads, run diffs, health probes, and the slow-status
// samplers) into Go. The bulk of the old BFF — the supervisor proxy and every
// per-city data read — is gone: the SPA calls /v0/* directly, same-origin.
//
// This plane is registered as a non-Huma handler on the supervisor mux (the
// documented exception, like the /svc/ proxy), so it adds no operations to the
// OpenAPI contract. Because it bypasses Huma's CSRF/read-only middleware, it
// self-enforces both through one shared guard (see guard).
package dashboardbff

import (
	"context"
	"encoding/json"
	"net/http"
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
	deps     Deps
	exec     *execRunner
	mux      *http.ServeMux
	samplers *samplerManager

	wg   sync.WaitGroup
	stop context.CancelFunc
}

// New builds the /api plane. Call Start to enable background samplers and Stop
// to drain them on shutdown.
func New(deps Deps) *Plane {
	p := &Plane{deps: deps, exec: newExecRunner(), mux: http.NewServeMux()}
	p.samplers = newSamplerManager(deps, p.exec)
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
}

// Stop signals the samplers to halt and waits for them to drain.
func (p *Plane) Stop() {
	if p.stop != nil {
		p.stop()
	}
	p.wg.Wait()
}

// guard enforces the plane's write policy: when read-only, every mutation is
// refused; otherwise every mutation must carry a non-empty X-GC-Request header
// (the supervisor's CSRF convention). Safe methods pass through. One shared
// gate so no per-handler check can be forgotten.
func (p *Plane) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if p.deps.ReadOnly {
				writeError(w, http.StatusMethodNotAllowed, "dashboard is read-only")
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

// registerRoutes wires every plane endpoint. Each registerX lives in its own
// file next to the logic it serves.
func (p *Plane) registerRoutes() {
	p.mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ts": time.Now().UTC().Format(time.RFC3339Nano)})
	})
	p.registerConfig()
	p.registerGit()
	p.registerBuilds()
	p.registerClientLog()
	p.registerHealth()
	p.registerRunDiff()
	p.registerSamplers()
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
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
