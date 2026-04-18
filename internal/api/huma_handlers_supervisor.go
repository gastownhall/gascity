package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// --- Supervisor Huma input/output types ---

// SupervisorCitiesOutput is the response for GET /v0/cities.
type SupervisorCitiesOutput struct {
	Body struct {
		Items []CityInfo `json:"items" doc:"Managed cities with status info."`
		Total int        `json:"total" doc:"Total count."`
	}
}

// SupervisorHealthOutput is the response for GET /health (supervisor scope).
type SupervisorHealthOutput struct {
	Body struct {
		Status        string             `json:"status" doc:"Health status (\"ok\")."`
		Version       string             `json:"version" doc:"Supervisor version."`
		UptimeSec     int                `json:"uptime_sec" doc:"Supervisor uptime in seconds."`
		CitiesTotal   int                `json:"cities_total" doc:"Total managed cities."`
		CitiesRunning int                `json:"cities_running" doc:"Cities currently running."`
		Startup       *SupervisorStartup `json:"startup,omitempty" doc:"First-city startup info for single-city deployments."`
	}
}

// SupervisorStartup describes the startup readiness of the first city.
type SupervisorStartup struct {
	Ready           bool     `json:"ready" doc:"True when the city is running."`
	Phase           string   `json:"phase,omitempty" doc:"Current phase (when not ready)."`
	PhasesCompleted []string `json:"phases_completed,omitempty" doc:"Phases completed so far."`
}

// SupervisorReadinessInput is the input for GET /v0/readiness.
type SupervisorReadinessInput struct {
	Items string `query:"items" required:"false" doc:"Comma-separated list of readiness items to check."`
	Fresh bool   `query:"fresh" required:"false" doc:"Force fresh probe, bypassing cache."`
}

// SupervisorReadinessOutput is the response for GET /v0/readiness.
type SupervisorReadinessOutput struct {
	Body readinessResponse
}

// SupervisorProviderReadinessInput is the input for GET /v0/provider-readiness.
type SupervisorProviderReadinessInput struct {
	Providers string `query:"providers" required:"false" doc:"Comma-separated list of providers to probe."`
	Fresh     bool   `query:"fresh" required:"false" doc:"Force fresh probe, bypassing cache."`
}

// SupervisorProviderReadinessOutput is the response for GET /v0/provider-readiness.
type SupervisorProviderReadinessOutput struct {
	Body providerReadinessResponse
}

// cityCreateRequest is the body for POST /v0/city.
type cityCreateRequest struct {
	Dir              string `json:"dir" minLength:"1" doc:"Directory to create the city in. Absolute or relative to $HOME."`
	Provider         string `json:"provider" minLength:"1" doc:"Provider name for the city's default session template."`
	BootstrapProfile string `json:"bootstrap_profile,omitempty" enum:"k8s-cell,kubernetes,kubernetes-cell,single-host-compat" doc:"Optional bootstrap profile."`
}

// cityCreateResponse is the response body for POST /v0/city.
type cityCreateResponse struct {
	OK   bool   `json:"ok" doc:"True on success."`
	Path string `json:"path" doc:"Resolved absolute path of the created city."`
}

// SupervisorCityCreateInput is the input for POST /v0/city.
type SupervisorCityCreateInput struct {
	Body cityCreateRequest
}

// SupervisorCityCreateOutput is the response for POST /v0/city.
type SupervisorCityCreateOutput struct {
	Body cityCreateResponse
}

// SupervisorEventListInput is the input for GET /v0/events (supervisor scope).
type SupervisorEventListInput struct {
	Type  string `query:"type" required:"false" doc:"Filter by event type."`
	Actor string `query:"actor" required:"false" doc:"Filter by actor."`
	Since string `query:"since" required:"false" doc:"Filter to events within the last Go duration (e.g. \"5m\")."`
}

// SupervisorEventListOutput is the response for GET /v0/events (supervisor scope).
type SupervisorEventListOutput struct {
	Body struct {
		Items []events.TaggedEvent `json:"items"`
		Total int                  `json:"total"`
	}
}

// SupervisorEventStreamInput is the input for GET /v0/events/stream (supervisor scope).
type SupervisorEventStreamInput struct {
	LastEventID string `header:"Last-Event-ID" required:"false" doc:"Reconnect cursor (composite per-city cursor)."`
	AfterCursor string `query:"after_cursor" required:"false" doc:"Alternative to Last-Event-ID for browsers that can't set custom headers."`
}

// cityInitExitAlreadyInitialized mirrors initExitAlreadyInitialized in
// cmd/gc/cmd_init.go. Duplicated here because the CLI's exit-code
// constant is declared in the main package and not importable; the two
// must stay in sync. TestCityInitExitCodeInSync enforces that.
const cityInitExitAlreadyInitialized = 2

// --- Huma API setup ---

// newSupervisorHumaAPI builds a huma.API attached to mux for supervisor-
// scope endpoints. CSRF and read-only middleware are attached here via
// api.UseMiddleware (Phase 3 Fix 3d's target pattern); they apply to every
// operation registered after the call.
func newSupervisorHumaAPI(mux *http.ServeMux, readOnly bool) huma.API {
	cfg := huma.DefaultConfig("Gas City Supervisor API", "0.1.0")
	cfg.SchemasPath = ""
	cfg.CreateHooks = nil
	api := humago.New(mux, cfg)

	registerEnumAliases(api.OpenAPI().Components.Schemas)
	// Force-register documentation-only union schemas so they appear in
	// components.schemas even though no handler names them directly.
	_ = SessionStreamCommonEvent{}.Schema(api.OpenAPI().Components.Schemas)

	api.UseMiddleware(humaCSRFMiddleware(api))
	if readOnly {
		api.UseMiddleware(humaReadOnlyMiddleware(api))
	}
	return api
}

// humaCSRFMiddleware enforces X-GC-Request on mutation requests. Emits RFC
// 9457 Problem Details via huma.WriteErr so the wire format matches other
// Huma errors.
func humaCSRFMiddleware(api huma.API) func(ctx huma.Context, next func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		if isMutationMethod(ctx.Method()) && ctx.Header("X-GC-Request") == "" {
			_ = huma.WriteErr(api, ctx, http.StatusForbidden, "csrf: X-GC-Request header required on mutation endpoints")
			return
		}
		next(ctx)
	}
}

// humaReadOnlyMiddleware rejects mutation requests when the server is in
// read-only mode.
func humaReadOnlyMiddleware(api huma.API) func(ctx huma.Context, next func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		if isMutationMethod(ctx.Method()) {
			_ = huma.WriteErr(api, ctx, http.StatusForbidden, "read_only: mutations disabled: server bound to non-localhost address")
			return
		}
		next(ctx)
	}
}

// registerSupervisorRoutes registers all supervisor-scope Huma operations.
func (sm *SupervisorMux) registerSupervisorRoutes() {
	huma.Get(sm.humaAPI, "/v0/cities", sm.humaHandleCities)
	huma.Get(sm.humaAPI, "/health", sm.humaHandleHealth)
	huma.Get(sm.humaAPI, "/v0/readiness", sm.humaHandleReadiness)
	huma.Get(sm.humaAPI, "/v0/provider-readiness", sm.humaHandleProviderReadiness)
	huma.Post(sm.humaAPI, "/v0/city", sm.humaHandleCityCreate)
	huma.Get(sm.humaAPI, "/v0/events", sm.humaHandleEventList)

	registerSSEStringID(sm.humaAPI, huma.Operation{
		OperationID: "stream-supervisor-events",
		Method:      http.MethodGet,
		Path:        "/v0/events/stream",
		Summary:     "Stream tagged events from all running cities.",
	}, map[string]any{
		"tagged_event": &taggedEventStreamEnvelope{},
		"heartbeat":    HeartbeatEvent{},
	}, sm.precheckGlobalEventStream, sm.streamGlobalEvents)
}

// --- Supervisor Huma handlers ---

func (sm *SupervisorMux) humaHandleCities(_ context.Context, _ *struct{}) (*SupervisorCitiesOutput, error) {
	cities := sm.resolver.ListCities()
	sort.Slice(cities, func(i, j int) bool { return cities[i].Name < cities[j].Name })
	out := &SupervisorCitiesOutput{}
	out.Body.Items = cities
	out.Body.Total = len(cities)
	return out, nil
}

func (sm *SupervisorMux) humaHandleHealth(_ context.Context, _ *struct{}) (*SupervisorHealthOutput, error) {
	cities := sm.resolver.ListCities()
	var running int
	var startup *SupervisorStartup
	for _, c := range cities {
		if c.Running {
			running++
		}
		if startup == nil {
			if c.Running {
				startup = &SupervisorStartup{
					Ready:           true,
					Phase:           "running",
					PhasesCompleted: allStartupPhases(),
				}
			} else {
				startup = &SupervisorStartup{
					Ready:           false,
					Phase:           c.Status,
					PhasesCompleted: c.PhasesCompleted,
				}
			}
		}
	}
	out := &SupervisorHealthOutput{}
	out.Body.Status = "ok"
	out.Body.Version = sm.version
	out.Body.UptimeSec = int(time.Since(sm.startedAt).Seconds())
	out.Body.CitiesTotal = len(cities)
	out.Body.CitiesRunning = running
	out.Body.Startup = startup
	return out, nil
}

func (sm *SupervisorMux) humaHandleReadiness(ctx context.Context, input *SupervisorReadinessInput) (*SupervisorReadinessOutput, error) {
	items, err := parseRequestedReadinessItems(input.Items, "items", defaultReadinessItems, supportedReadiness)
	if err != nil {
		return nil, huma.Error400BadRequest("invalid: " + err.Error())
	}
	resp, err := buildReadinessResponse(ctx, items, input.Fresh)
	if err != nil {
		return nil, huma.Error500InternalServerError("internal: " + err.Error())
	}
	out := &SupervisorReadinessOutput{}
	out.Body = resp
	return out, nil
}

func (sm *SupervisorMux) humaHandleProviderReadiness(ctx context.Context, input *SupervisorProviderReadinessInput) (*SupervisorProviderReadinessOutput, error) {
	providers, err := parseRequestedReadinessItems(input.Providers, "providers", defaultProviderReadinessItems, supportedProviderReadiness)
	if err != nil {
		return nil, huma.Error400BadRequest("invalid: " + err.Error())
	}
	resp, err := buildReadinessResponse(ctx, providers, input.Fresh)
	if err != nil {
		return nil, huma.Error500InternalServerError("internal: " + err.Error())
	}
	providerResp := providerReadinessResponse{
		Providers: make(map[string]providerReadiness, len(providers)),
	}
	for _, provider := range providers {
		item := resp.Items[provider]
		providerResp.Providers[provider] = providerReadiness{
			DisplayName: item.DisplayName,
			Status:      item.Status,
		}
	}
	out := &SupervisorProviderReadinessOutput{}
	out.Body = providerResp
	return out, nil
}

// humaHandleCityCreate handles POST /v0/city — create a new city by
// shelling out to `gc init`. Stateless; does not require a running city.
func (sm *SupervisorMux) humaHandleCityCreate(ctx context.Context, input *SupervisorCityCreateInput) (*SupervisorCityCreateOutput, error) {
	// Dir/Provider emptiness is enforced by minLength:"1" tags on cityCreateRequest.
	// BootstrapProfile membership is enforced by the enum tag.
	// Provider membership against runtime-loaded builtins stays here —
	// static enum can't express a runtime-loaded set.
	if _, ok := config.BuiltinProviders()[input.Body.Provider]; !ok {
		return nil, huma.Error422UnprocessableEntity(fmt.Sprintf("invalid: unknown provider %q", input.Body.Provider))
	}

	dir := input.Body.Dir
	if !filepath.IsAbs(dir) {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, huma.Error500InternalServerError(fmt.Sprintf("internal: resolving home dir: %v", err))
		}
		dir = filepath.Join(home, dir)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, huma.Error500InternalServerError(fmt.Sprintf("internal: creating directory: %v", err))
	}

	gcBin, err := os.Executable()
	if err != nil {
		return nil, huma.Error500InternalServerError(fmt.Sprintf("internal: finding gc binary: %v", err))
	}
	args := []string{"init", dir, "--provider", input.Body.Provider}
	if input.Body.BootstrapProfile != "" {
		args = append(args, "--bootstrap-profile", input.Body.BootstrapProfile)
	}

	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cctx, gcBin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := stderr.String()
		if msg == "" {
			msg = err.Error()
		}
		// gc init exits with initExitAlreadyInitialized when the
		// target already contains a city. Dispatch on the exit code
		// rather than scraping stderr text.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == cityInitExitAlreadyInitialized {
			return nil, huma.Error409Conflict("conflict: city already initialized at " + dir)
		}
		return nil, huma.Error500InternalServerError("init_failed: " + msg)
	}

	out := &SupervisorCityCreateOutput{}
	out.Body = cityCreateResponse{OK: true, Path: dir}
	return out, nil
}

func (sm *SupervisorMux) humaHandleEventList(_ context.Context, input *SupervisorEventListInput) (*SupervisorEventListOutput, error) {
	mux := sm.buildMultiplexer()
	filter := events.Filter{Type: input.Type, Actor: input.Actor}
	if v := strings.TrimSpace(input.Since); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			filter.Since = time.Now().Add(-d)
		}
	}
	evts, err := mux.ListAll(filter)
	if err != nil {
		return nil, huma.Error500InternalServerError("internal: " + err.Error())
	}
	if evts == nil {
		evts = []events.TaggedEvent{}
	}
	out := &SupervisorEventListOutput{}
	out.Body.Items = evts
	out.Body.Total = len(evts)
	return out, nil
}

// --- Supervisor global events stream (Fix 3g final wiring) ---

// precheckGlobalEventStream validates that at least one running city
// has an event provider before the SSE stream commits its 200 headers.
// buildMultiplexer always returns a non-nil *Multiplexer; the real
// "nothing to watch" signal is its provider count. Returning a 503 here
// means the client sees a proper Problem Details body instead of a
// 200 text/event-stream with immediate EOF — which was the old bug
// (Codex Critical, 2026-04-17).
func (sm *SupervisorMux) precheckGlobalEventStream(_ context.Context, _ *SupervisorEventStreamInput) error {
	if sm.buildMultiplexer().Len() == 0 {
		return huma.Error503ServiceUnavailable("no_providers: no event providers available")
	}
	return nil
}

// streamGlobalEvents emits tagged events with composite per-city cursor
// IDs. Called after headers commit; failures terminate the stream cleanly
// (there's no way to return an HTTP error at this point). This is the
// final wiring of Fix 3g — it replaces the raw writeSSEWithStringID loop
// that previously lived in streamProjectedGlobalEvents.
func (sm *SupervisorMux) streamGlobalEvents(hctx huma.Context, input *SupervisorEventStreamInput, send StringIDSender) {
	cursor := strings.TrimSpace(input.LastEventID)
	if cursor == "" {
		cursor = strings.TrimSpace(input.AfterCursor)
	}
	cursors := events.ParseCursor(cursor)
	if cursors == nil {
		cursors = make(map[string]uint64)
	}

	mux := sm.buildMultiplexer()
	mw, err := mux.Watch(hctx.Context(), cursors)
	if err != nil {
		return
	}
	defer mw.Close() //nolint:errcheck

	keepalive := time.NewTicker(sseKeepalive)
	defer keepalive.Stop()

	type result struct {
		event events.TaggedEvent
		err   error
	}
	ch := make(chan result, 1)
	readNext := func() {
		go func() {
			te, err := mw.Next()
			select {
			case ch <- result{event: te, err: err}:
			case <-hctx.Context().Done():
			}
		}()
	}
	readNext()

	for {
		select {
		case <-hctx.Context().Done():
			return
		case r := <-ch:
			if r.err != nil {
				return
			}
			cursors[r.event.City] = r.event.Seq
			var wfp *workflowEventProjection
			if cs := sm.resolver.CityState(r.event.City); cs != nil {
				wfp = projectWorkflowEvent(cs, r.event.Event)
			}
			envelope := &taggedEventStreamEnvelope{
				TaggedEvent: r.event,
				Workflow:    wfp,
			}
			if err := send(StringIDMessage{ID: events.FormatCursor(cursors), Data: envelope}); err != nil {
				// Client disconnected or encoding failed — draining
				// further events off the multiplexer wastes work and
				// masks the disconnect. Exit; the per-city stream
				// endpoints do the same on send failure.
				return
			}
			readNext()
		case t := <-keepalive.C:
			// Emit a heartbeat frame (no ID so reconnect cursor is preserved).
			// Idle proxies drop long-lived SSE without traffic; skipping this
			// makes the stream look healthy to EventSource while the
			// connection has silently died.
			if err := send(StringIDMessage{Data: HeartbeatEvent{Timestamp: t.UTC().Format(time.RFC3339)}}); err != nil {
				return
			}
		}
	}
}

