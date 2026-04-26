package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/gastownhall/gascity/internal/cityinit"
	"github.com/gastownhall/gascity/internal/citylayout"
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
	Provider         string `json:"provider,omitempty" minLength:"1" doc:"Provider name for the city's default session template. Mutually exclusive with start_command."`
	StartCommand     string `json:"start_command,omitempty" doc:"Custom workspace start command for the city's default session template. Mutually exclusive with provider."`
	BootstrapProfile string `json:"bootstrap_profile,omitempty" enum:"k8s-cell,kubernetes,kubernetes-cell,single-host-compat" doc:"Optional bootstrap profile."`
}

// cityCreateResponse is the response body for POST /v0/city. This
// endpoint is asynchronous: a 202 response means the city was
// scaffolded on disk and registered with the supervisor, but the
// supervisor reconciler still has to run the slow finalize work
// (pack materialization, bead store startup, formula resolution,
// agent validation). Clients observe completion by subscribing to
// /v0/events/stream and waiting for request.result.city.create or
// request.failed with the returned request_id. Polling is unnecessary.
type asyncAcceptedResponse struct {
	RequestID string `json:"request_id" doc:"Correlation ID. Watch /v0/events/stream for request.result.city.create, request.result.city.unregister, or request.failed with this request_id."`
}

// SupervisorCityCreateInput is the input for POST /v0/city.
type SupervisorCityCreateInput struct {
	Body cityCreateRequest
}

// SupervisorCityCreateOutput is the response for POST /v0/city.
type SupervisorCityCreateOutput struct {
	Status int `json:"-"`
	Body   asyncAcceptedResponse
}

// cityUnregisterResponse is the response body for
// POST /v0/city/{cityName}/unregister. This endpoint is asynchronous:
// a 202 response means the city's registry entry was removed and the
// supervisor was signaled to reconcile, but the city's controller is
// not yet stopped. Clients observe completion by subscribing to
// /v0/events/stream and waiting for request.result.city.unregister or
// request.failed with the returned request_id.
// cityUnregisterResponse is the same as asyncAcceptedResponse.
type cityUnregisterResponse = asyncAcceptedResponse

// SupervisorCityUnregisterInput is the input for
// POST /v0/city/{cityName}/unregister.
type SupervisorCityUnregisterInput struct {
	CityName string `path:"cityName" doc:"Supervisor-registered city name."`
}

// SupervisorCityUnregisterOutput is the response for
// POST /v0/city/{cityName}/unregister. The Status field carries
// 202 Accepted to tell Huma to emit the async status code.
type SupervisorCityUnregisterOutput struct {
	Status int `json:"-"`
	Body   cityUnregisterResponse
}

// SupervisorEventListInput is the input for GET /v0/events (supervisor scope).
type SupervisorEventListInput struct {
	Type  string `query:"type" required:"false" doc:"Filter by event type."`
	Actor string `query:"actor" required:"false" doc:"Filter by actor."`
	Since string `query:"since" required:"false" doc:"Filter to events within the last Go duration (e.g. \"5m\")."`
	Limit int    `query:"limit" minimum:"0" required:"false" doc:"Maximum number of trailing events to return. 0 = no limit. Used by 'gc events --seq' to compute the head cursor cheaply."`
}

// SupervisorEventListOutput is the response for GET /v0/events (supervisor scope).
type SupervisorEventListOutput struct {
	Body struct {
		Items []WireTaggedEvent `json:"items"`
		Total int               `json:"total"`
	}
}

// SupervisorEventStreamInput is the input for GET /v0/events/stream (supervisor scope).
type SupervisorEventStreamInput struct {
	LastEventID string `header:"Last-Event-ID" required:"false" doc:"Reconnect cursor (composite per-city cursor)."`
	AfterCursor string `query:"after_cursor" required:"false" doc:"Alternative to Last-Event-ID for browsers that can't set custom headers."`
}

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
	registerEventEnvelopeCompatibilitySchemas(api.OpenAPI().Components.Schemas)

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
	// Async mutation: returns 202 Accepted after scaffold + register;
	// completion is signaled via request.result.city.create or request.failed.
	huma.Post(sm.humaAPI, "/v0/city", sm.humaHandleCityCreate, addMutationCSRFParam, func(op *huma.Operation) {
		op.DefaultStatus = http.StatusAccepted
	})
	// Async unregister: returns 202 after the registry entry is removed
	// and the supervisor is signaled. request.result.city.unregister or
	// request.failed signals completion on the event stream.
	huma.Post(sm.humaAPI, "/v0/city/{cityName}/unregister", sm.humaHandleCityUnregister, addMutationCSRFParam, func(op *huma.Operation) {
		op.DefaultStatus = http.StatusAccepted
	})
	huma.Get(sm.humaAPI, "/v0/events", sm.humaHandleEventList)

	registerSSEStringID(sm.humaAPI, huma.Operation{
		OperationID: "stream-supervisor-events",
		Method:      http.MethodGet,
		Path:        "/v0/events/stream",
		Summary:     "Stream tagged events from all running cities.",
	}, map[string]any{
		"tagged_event": sseEventContract{
			runtimeSample: &taggedEventStreamEnvelope{},
			schemaSample:  typedTaggedEventStreamEnvelopeSchema{},
		},
		"heartbeat": HeartbeatEvent{},
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
			Detail:      item.Detail,
		}
	}
	out := &SupervisorProviderReadinessOutput{}
	out.Body = providerResp
	return out, nil
}

// humaHandleCityCreate handles POST /v0/city asynchronously. Calls
// the city initializer in-process to write the on-disk shape and
// register the city with the supervisor, then returns 202 Accepted
// immediately. The supervisor reconciler runs the slow finalize
// (prepareCityForSupervisor: pack materialization, bead store
// startup, formula resolution, agent validation) on its next tick
// and emits request.result.city.create or request.failed on the
// supervisor event bus when done. Clients observe completion via
// /v0/events/stream — no polling required.
//
// Rationale: full city init takes minutes (dolt startup,
// provider-readiness probes, pack fetch). Blocking the HTTP request
// until finalize completes exceeds reasonable client timeouts
// (a real-world app's harness hit 120s). The fast scaffold+register path takes
// seconds; the async completion contract via SSE is the right shape
// for a long-running operation. See engdocs/architecture/api-control-plane.md §1–§2 on
// the object model + typed events; §4 on the event registry.
func (sm *SupervisorMux) humaHandleCityCreate(ctx context.Context, input *SupervisorCityCreateInput) (*SupervisorCityCreateOutput, error) {
	dir := input.Body.Dir
	if !filepath.IsAbs(dir) {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, huma.Error500InternalServerError(fmt.Sprintf("internal: resolving home dir: %v", err))
		}
		dir = filepath.Join(home, dir)
	}

	// Cheap pre-check that does not require a city initializer: if the
	// target directory already looks like an initialized city on disk,
	// return 409 before we try to scaffold. Keeps the API well-behaved
	// in test configurations that build a SupervisorMux without an
	// initializer.
	if cityDirAlreadyInitialized(dir) {
		return nil, huma.Error409Conflict("conflict: city already initialized at " + dir)
	}

	if sm.initializer == nil {
		return nil, huma.Error501NotImplemented("city creation is not available in this supervisor (no initializer wired)")
	}

	reqID, err := newRequestID()
	if err != nil {
		return nil, huma.Error500InternalServerError(fmt.Sprintf("generating request ID: %v", err))
	}
	if store, ok := sm.resolver.(PendingRequestStore); ok {
		if err := store.StorePendingRequestID(dir, reqID); err != nil {
			return nil, huma.Error500InternalServerError(fmt.Sprintf("storing pending request ID: %v", err))
		}
	}
	cleanupPending := func() {
		if store, ok := sm.resolver.(PendingRequestStore); ok {
			if _, _, err := store.ConsumePendingRequestID(dir); err != nil {
				log.Printf("api: consume pending city create request ID for %s: %v", dir, err)
			}
		}
	}

	_, scaffoldErr := sm.initializer.Scaffold(ctx, cityinit.InitRequest{
		Dir:                   dir,
		Provider:              input.Body.Provider,
		StartCommand:          input.Body.StartCommand,
		BootstrapProfile:      input.Body.BootstrapProfile,
		SkipProviderReadiness: true,
	})
	switch {
	case errors.Is(scaffoldErr, cityinit.ErrAlreadyInitialized):
		cleanupPending()
		return nil, huma.Error409Conflict("conflict: city already initialized at " + dir)
	case errors.Is(scaffoldErr, cityinit.ErrInvalidDirectory),
		errors.Is(scaffoldErr, cityinit.ErrInvalidProvider),
		errors.Is(scaffoldErr, cityinit.ErrInvalidBootstrapProfile):
		cleanupPending()
		return nil, huma.Error422UnprocessableEntity(scaffoldErr.Error())
	case scaffoldErr != nil:
		cleanupPending()
		return nil, huma.Error500InternalServerError(scaffoldErr.Error())
	}

	out := &SupervisorCityCreateOutput{
		Status: http.StatusAccepted,
	}
	out.Body = asyncAcceptedResponse{RequestID: reqID}
	return out, nil
}

// humaHandleCityUnregister handles POST /v0/city/{cityName}/unregister
// asynchronously. Calls the city initializer in-process to remove
// the city from the supervisor's registry and signal reconcile, then
// returns 202 Accepted immediately. The supervisor reconciler stops
// the city's controller on its next tick and emits
// request.result.city.unregister or request.failed on the supervisor
// event bus. Clients observe completion via /v0/events/stream — no
// polling required.
//
// The city directory itself is not modified. Purging the directory
// is a separate concern.
//
// Error mapping:
//   - ErrNotRegistered -> 404 Not Found
//   - any other error -> 500 Internal Server Error
func (sm *SupervisorMux) humaHandleCityUnregister(ctx context.Context, input *SupervisorCityUnregisterInput) (*SupervisorCityUnregisterOutput, error) {
	if sm.initializer == nil {
		return nil, huma.Error501NotImplemented("city unregister is not available in this supervisor (no initializer wired)")
	}
	name := strings.TrimSpace(input.CityName)
	if name == "" {
		return nil, huma.Error400BadRequest("city_name is required")
	}

	reqID, err := newRequestID()
	if err != nil {
		return nil, huma.Error500InternalServerError(fmt.Sprintf("generating request ID: %v", err))
	}

	// Store the pending request_id BEFORE Unregister triggers a
	// reconciler reload, so the reconciler can correlate the
	// terminal request.result event. Look up the city path from
	// the resolver first; if the city isn't known, Unregister will
	// return ErrNotRegistered anyway.
	var cityPath string
	if store, ok := sm.resolver.(PendingRequestStore); ok {
		for _, c := range sm.resolver.ListCities() {
			if c.Name == name {
				cityPath = c.Path
				break
			}
		}
		if cityPath != "" {
			if err := store.StorePendingRequestID(cityPath, reqID); err != nil {
				return nil, huma.Error500InternalServerError(fmt.Sprintf("storing pending request ID: %v", err))
			}
		}
	}

	_, unregErr := sm.initializer.Unregister(ctx, cityinit.UnregisterRequest{CityName: name})
	switch {
	case errors.Is(unregErr, cityinit.ErrNotRegistered):
		if store, ok := sm.resolver.(PendingRequestStore); ok && cityPath != "" {
			if _, _, err := store.ConsumePendingRequestID(cityPath); err != nil {
				log.Printf("api: consume pending city unregister request ID for %s: %v", cityPath, err)
			}
		}
		return nil, huma.Error404NotFound("not_found: " + unregErr.Error())
	case unregErr != nil:
		if store, ok := sm.resolver.(PendingRequestStore); ok && cityPath != "" {
			if _, _, err := store.ConsumePendingRequestID(cityPath); err != nil {
				log.Printf("api: consume pending city unregister request ID for %s: %v", cityPath, err)
			}
		}
		return nil, huma.Error500InternalServerError(unregErr.Error())
	}

	out := &SupervisorCityUnregisterOutput{Status: http.StatusAccepted}
	out.Body = cityUnregisterResponse{RequestID: reqID}
	return out, nil
}

func cityDirAlreadyInitialized(dir string) bool {
	requiredDirs := []string{
		filepath.Join(dir, citylayout.RuntimeRoot),
		filepath.Join(dir, citylayout.RuntimeRoot, "cache"),
		filepath.Join(dir, citylayout.RuntimeRoot, "runtime"),
		filepath.Join(dir, citylayout.RuntimeRoot, "system"),
	}
	for _, path := range requiredDirs {
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			return false
		}
	}
	info, err := os.Stat(filepath.Join(dir, citylayout.RuntimeRoot, "events.jsonl"))
	return err == nil && !info.IsDir()
}

func (sm *SupervisorMux) humaHandleEventList(_ context.Context, input *SupervisorEventListInput) (*SupervisorEventListOutput, error) {
	mux := sm.buildMultiplexer()
	filter := events.Filter{Type: input.Type, Actor: input.Actor}
	if d, ok, err := parseEventSince(input.Since); err != nil {
		return nil, err
	} else if ok {
		filter.Since = time.Now().Add(-d)
	}
	evts, err := mux.ListAll(filter)
	if err != nil {
		return nil, huma.Error500InternalServerError("internal: " + err.Error())
	}
	wires := make([]WireTaggedEvent, 0, len(evts))
	for _, e := range evts {
		w, ok := toWireTaggedEvent(e)
		if !ok {
			continue
		}
		wires = append(wires, w)
	}
	out := &SupervisorEventListOutput{}
	// Total is the full match count so clients can distinguish "limit
	// truncated" from "the server only had N events."
	out.Body.Total = len(wires)
	// Limit clamp: take the N most recent events (wires is already
	// chronologically ordered). Critical for `gc events --seq` which
	// computes the head cursor from the last event only.
	if input.Limit > 0 && input.Limit < len(wires) {
		wires = wires[len(wires)-input.Limit:]
	}
	out.Body.Items = wires
	return out, nil
}

// --- Supervisor global events stream (Fix 3g final wiring) ---

// precheckGlobalEventStream validates that the global event stream
// can actually deliver events before committing 200 headers. Two
// failure modes both produce 503 Problem Details instead of 200+EOF:
//
//  1. No event providers registered at all (empty mux). In practice
//     this only happens when zero cities are registered in the
//     supervisor — the TransientCityEventSource resolver extension
//     surfaces event files for every registered city (running,
//     pending, or failed) so any POST /v0/city → subscribe flow
//     finds the newly-registered city in the mux.
//  2. Providers exist but none can attach a watcher right now.
//
// The precheck attaches a watcher and closes it immediately — a
// cheap probe that surfaces per-city watcher failures at the point
// where we can still return a proper HTTP error.
func (sm *SupervisorMux) precheckGlobalEventStream(ctx context.Context, _ *SupervisorEventStreamInput) error {
	mux := sm.buildMultiplexer()
	if mux.Len() == 0 {
		return huma.Error503ServiceUnavailable("no_providers: no event providers available")
	}
	probe, err := mux.Watch(ctx, nil)
	if err != nil {
		if errors.Is(err, events.ErrNoWatchers) {
			return huma.Error503ServiceUnavailable("no_watchers: event providers are registered but none are watchable")
		}
		return huma.Error503ServiceUnavailable("watch_failed: " + err.Error())
	}
	_ = probe.Close()
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
		log.Printf("api: supervisor events-stream: Watch failed cursors=%v: %v", cursors, err)
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
				log.Printf("api: supervisor events-stream: multiplex Next failed: %v", r.err)
				return
			}
			cursors[r.event.City] = r.event.Seq
			var wfp *workflowEventProjection
			if cs := sm.resolver.CityState(r.event.City); cs != nil {
				wfp = projectWorkflowEvent(cs, r.event.Event)
			}
			envelope, decodeErr := wireTaggedEventFrom(r.event, wfp)
			if decodeErr != nil {
				// Strict registry policy (Principle 7): skip
				// unregistered event types and continue the stream.
				// CI's registry-coverage test prevents this path from
				// firing in practice.
				log.Printf("api: supervisor events-stream skip %s seq=%d city=%s: %v",
					r.event.Type, r.event.Seq, r.event.City, decodeErr)
				readNext()
				continue
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
