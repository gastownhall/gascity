package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/cityinit"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/packman"
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
		Status          string             `json:"status" doc:"Health status (\"ok\")."`
		Version         string             `json:"version" doc:"Supervisor version."`
		BuildID         string             `json:"build_id,omitempty" doc:"Build identity (typically a short git commit hash, with \"-dirty\" suffix when built from an unclean tree). Empty when unavailable."`
		UptimeSec       int                `json:"uptime_sec" doc:"Supervisor uptime in seconds."`
		CitiesTotal     int                `json:"cities_total" doc:"Total managed cities."`
		CitiesRunning   int                `json:"cities_running" doc:"Cities currently running."`
		PacksLockSHA256 string             `json:"packs_lock_sha256,omitempty" doc:"SHA-256 hex digest of the first managed city's packs.lock contents, for single-city deployments (mirrors the startup field's first-city semantics). Drift checkers compare this against the committed lockfile copy. Omitted when no city is registered, the city has no packs.lock, or the lockfile is unreadable (read error logged server-side) — treat absence as unknown, not as proof there is no lockfile."`
		Startup         *SupervisorStartup `json:"startup,omitempty" doc:"First-city startup info for single-city deployments."`
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
// endpoint is asynchronous: a 202 response means the city was scaffolded
// on disk and registered with the supervisor. Clients observe request
// completion by subscribing to /v0/events/stream and waiting for
// request.result.city.create or request.failed with the returned
// request_id. Polling is unnecessary.
type asyncAcceptedResponse struct {
	RequestID   string `json:"request_id" doc:"Correlation ID. Watch /v0/events/stream for request.result.city.create, request.result.city.unregister, or request.failed with this request_id."`
	EventCursor string `json:"event_cursor" doc:"Supervisor event-stream cursor captured before the async request was accepted. Pass this value as after_cursor to /v0/events/stream to receive the request result without replaying unrelated historical backlog. A value of 0 can also mean no event provider is configured or every event log is empty."`
}

// SupervisorCityCreateInput is the input for POST /v0/city.
type SupervisorCityCreateInput struct {
	IdempotencyKey string `header:"Idempotency-Key" required:"false" doc:"Idempotency key for safe retries."`
	Body           cityCreateRequest
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
		EventCursor string            `json:"event_cursor" doc:"Supervisor event-stream cursor captured before the history snapshot was listed. Pass this value as after_cursor to /v0/events/stream to receive events emitted after the snapshot boundary without replaying unrelated historical backlog."`
		Items       []WireTaggedEvent `json:"items"`
		Total       int               `json:"total"`
	}
}

// SupervisorEventStreamInput is the input for GET /v0/events/stream (supervisor scope).
type SupervisorEventStreamInput struct {
	LastEventID string `header:"Last-Event-ID" required:"false" doc:"Reconnect cursor (composite per-city cursor). Omit Last-Event-ID and after_cursor to start at the current supervisor event head."`
	AfterCursor string `query:"after_cursor" required:"false" doc:"Alternative to Last-Event-ID for browsers that can't set custom headers. Omit after_cursor and Last-Event-ID to start at the current supervisor event head."`
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
		// Enumerating any error drops Huma's catch-all `default` response, so
		// list every status this op actually emits: 401/403 from the
		// write-auth/CSRF middleware, 409 (already initialized / init in
		// progress / idempotency-in-flight), 501 when the supervisor has no
		// initializer (the controller-embedded mux). Huma auto-adds 422/500.
		op.Errors = append(op.Errors, http.StatusUnauthorized, http.StatusForbidden, http.StatusConflict, http.StatusNotImplemented)
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
		Description: "Server-Sent Events stream of supervisor-tagged events. Supports reconnection via Last-Event-ID header or after_cursor query param; omitting both starts at the current supervisor event head.",
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
	var packsLockSHA string
	for _, c := range cities {
		if c.Running {
			running++
		}
		if startup == nil {
			packsLockSHA = packsLockSHA256(c.Path)
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
	out.Body.BuildID = sm.buildID
	out.Body.UptimeSec = int(time.Since(sm.startedAt).Seconds())
	out.Body.CitiesTotal = len(cities)
	out.Body.CitiesRunning = running
	out.Body.PacksLockSHA256 = packsLockSHA
	out.Body.Startup = startup
	return out, nil
}

// packsLockSHA256 returns the hex-encoded SHA-256 digest of the
// packs.lock file under cityPath, or "" when the file is missing.
// Unexpected read errors are logged and reported as absent: /health
// must stay available even when a city directory is unreadable.
func packsLockSHA256(cityPath string) string {
	if cityPath == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(cityPath, packman.LockfileName))
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("health: reading %s for %s: %v", packman.LockfileName, cityPath, err)
		}
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (sm *SupervisorMux) humaHandleReadiness(ctx context.Context, input *SupervisorReadinessInput) (*SupervisorReadinessOutput, error) {
	items, err := parseRequestedReadinessItems(input.Items, "items", defaultReadinessItems, supportedReadiness)
	if err != nil {
		return nil, apierr.InvalidRequest.Msg("invalid: " + err.Error())
	}
	resp, err := buildReadinessResponse(ctx, items, input.Fresh)
	if err != nil {
		return nil, apierr.Internal.Msg("internal: " + err.Error())
	}
	out := &SupervisorReadinessOutput{}
	out.Body = resp
	return out, nil
}

func (sm *SupervisorMux) humaHandleProviderReadiness(ctx context.Context, input *SupervisorProviderReadinessInput) (*SupervisorProviderReadinessOutput, error) {
	providers, err := parseRequestedReadinessItems(input.Providers, "providers", defaultProviderReadinessItems, supportedProviderReadiness)
	if err != nil {
		return nil, apierr.InvalidRequest.Msg("invalid: " + err.Error())
	}
	resp, err := buildReadinessResponse(ctx, providers, input.Fresh)
	if err != nil {
		return nil, apierr.Internal.Msg("internal: " + err.Error())
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
// register the city with the supervisor, stores request_id correlation
// for the reconciler, then returns 202 Accepted. The supervisor
// reconciler emits request.result.city.create after the city runtime
// starts. Clients observe request completion via /v0/events/stream —
// no polling required.
//
// Rationale: full city startup can exceed reasonable HTTP client
// timeouts. The POST returns once scaffold+register succeeds, while
// the terminal request-result event is held until the reconciler has
// started the city runtime. See engdocs/architecture/api-control-plane.md
// §1-§2 on the object model + typed events; §4 on the event registry.
func (sm *SupervisorMux) humaHandleCityCreate(ctx context.Context, input *SupervisorCityCreateInput) (*SupervisorCityCreateOutput, error) {
	// Idempotency: scaffold at most once per Idempotency-Key (supervisor-scope
	// cache — there is no per-city Server yet for a city being created). The
	// cached value is the full accepted body, so a replay returns the ORIGINAL
	// request_id (the client's correlation handle on /v0/events/stream) and
	// the original pre-create event cursor — recomputing either on replay
	// would break result-event correlation.
	accepted, err := withIdempotency(sm.idem, "/v0/city", input.IdempotencyKey, input.Body,
		func() (asyncAcceptedResponse, error) {
			return sm.scaffoldCityOnce(ctx, input.Body)
		})
	if err != nil {
		return nil, err
	}

	out := &SupervisorCityCreateOutput{
		Status: http.StatusAccepted,
	}
	out.Body = accepted
	return out, nil
}

// scaffoldCityOnce performs the one-shot city scaffold+register work behind
// POST /v0/city and returns the accepted {request_id, event_cursor} body. It is
// the operation wrapped by the supervisor idempotency cache in
// humaHandleCityCreate: on the first request for a given Idempotency-Key it
// resolves the target directory, scaffolds and registers the city, and stores
// the request_id correlation for the reconciler. On a replay the cache
// short-circuits before this runs, so it never executes twice for the same key.
func (sm *SupervisorMux) scaffoldCityOnce(ctx context.Context, body cityCreateRequest) (asyncAcceptedResponse, error) {
	var zero asyncAcceptedResponse
	dir := body.Dir
	if !filepath.IsAbs(dir) {
		home, err := os.UserHomeDir()
		if err != nil {
			return zero, apierr.Internal.Msg(fmt.Sprintf("internal: resolving home dir: %v", err))
		}
		dir = filepath.Join(home, dir)
	}

	// Cheap pre-check that does not require a city initializer: if the
	// target directory already looks like an initialized city on disk,
	// return 409 before we try to scaffold. Keeps the API well-behaved
	// in test configurations that build a SupervisorMux without an
	// initializer.
	if cityDirAlreadyInitialized(dir) {
		return zero, apierr.ConflictWrongState.Msg("conflict: city already initialized at " + dir)
	}

	if sm.initializer == nil {
		return zero, apierr.NotImplemented.Msg("city creation is not available in this supervisor (no initializer wired)")
	}

	reqID, err := newRequestID()
	if err != nil {
		return zero, apierr.Internal.Msg(fmt.Sprintf("generating request ID: %v", err))
	}
	eventCursor, cursorErr := sm.currentSupervisorEventCursor()
	if cursorErr != nil {
		return zero, apierr.Internal.Msg(cursorErr.Error())
	}
	pendingStored := false
	if store, ok := sm.resolver.(PendingRequestStore); ok {
		if err := store.StorePendingRequestID(dir, reqID); err != nil {
			if errors.Is(err, ErrPendingRequestExists) {
				return zero, apierr.OperationInProgress.Msg("conflict: city initialization already in progress at " + dir)
			}
			return zero, apierr.Internal.Msg(fmt.Sprintf("storing pending request ID: %v", err))
		}
		pendingStored = true
	}

	result, scaffoldErr := sm.initializer.Scaffold(ctx, cityinit.InitRequest{
		Dir:                   dir,
		Provider:              body.Provider,
		StartCommand:          body.StartCommand,
		BootstrapProfile:      body.BootstrapProfile,
		SkipProviderReadiness: true,
	})
	postRegisterFailed := false
	switch {
	case errors.Is(scaffoldErr, cityinit.ErrAlreadyInitialized):
		sm.clearPendingCityRequestID(dir, pendingStored)
		return zero, apierr.ConflictWrongState.Msg("conflict: city already initialized at " + dir)
	case errors.Is(scaffoldErr, cityinit.ErrInvalidDirectory),
		errors.Is(scaffoldErr, cityinit.ErrInvalidProvider),
		errors.Is(scaffoldErr, cityinit.ErrInvalidBootstrapProfile):
		sm.clearPendingCityRequestID(dir, pendingStored)
		return zero, apierr.ValidationFailed.Msg(scaffoldErr.Error())
	case errors.Is(scaffoldErr, cityinit.ErrPostRegisterFailure):
		failureReqID := reqID
		if consumedReqID, ok := sm.consumePendingCityRequestID(dir, pendingStored); ok {
			failureReqID = consumedReqID
		}
		emitCityCreateFailed(sm.resolver, failureReqID, result, dir, "city_init_failed", scaffoldErr)
		postRegisterFailed = true
	case scaffoldErr != nil:
		sm.clearPendingCityRequestID(dir, pendingStored)
		return zero, apierr.Internal.Msg(scaffoldErr.Error())
	}

	if !pendingStored && !postRegisterFailed {
		emitCityCreateSucceeded(sm.resolver, reqID, result, dir)
	}
	return asyncAcceptedResponse{RequestID: reqID, EventCursor: eventCursor}, nil
}

func (sm *SupervisorMux) clearPendingCityRequestID(cityPath string, stored bool) {
	sm.consumePendingCityRequestID(cityPath, stored)
}

func (sm *SupervisorMux) consumePendingCityRequestID(cityPath string, stored bool) (string, bool) {
	if !stored {
		return "", false
	}
	store, ok := sm.resolver.(PendingRequestStore)
	if !ok {
		return "", false
	}
	reqID, found, err := store.ConsumePendingRequestID(cityPath)
	if err != nil {
		log.Printf("api: consume pending city create request ID for %s: %v", cityPath, err)
		return "", false
	}
	return reqID, found
}

func emitCityCreateSucceeded(resolver CityResolver, requestID string, result *cityinit.InitResult, fallbackPath string) {
	supSrc, ok := resolver.(SupervisorEventSource)
	if !ok {
		log.Printf("api: no supervisor event recorder for city.create result %s", requestID)
		return
	}
	rec := supSrc.SupervisorEventRecorder()
	if rec == nil {
		log.Printf("api: nil supervisor event recorder for city.create result %s", requestID)
		return
	}

	cityPath := fallbackPath
	cityName := filepath.Base(fallbackPath)
	if result != nil {
		if result.CityPath != "" {
			cityPath = result.CityPath
		}
		if result.CityName != "" {
			cityName = result.CityName
		}
	}

	EmitTypedEvent(rec, events.RequestResultCityCreate, cityName, CityCreateSucceededPayload{
		RequestID: requestID,
		Name:      cityName,
		Path:      cityPath,
	})
}

func emitCityCreateFailed(resolver CityResolver, requestID string, result *cityinit.InitResult, fallbackPath, errorCode string, err error) {
	supSrc, ok := resolver.(SupervisorEventSource)
	if !ok {
		log.Printf("api: no supervisor event recorder for city.create failure %s", requestID)
		return
	}
	rec := supSrc.SupervisorEventRecorder()
	if rec == nil {
		log.Printf("api: nil supervisor event recorder for city.create failure %s", requestID)
		return
	}

	cityName := filepath.Base(fallbackPath)
	if result != nil {
		if result.CityName != "" {
			cityName = result.CityName
		}
	}
	EmitTypedEvent(rec, events.RequestFailed, cityName, RequestFailedPayload{
		RequestID:    requestID,
		Operation:    RequestOperationCityCreate,
		ErrorCode:    errorCode,
		ErrorMessage: err.Error(),
	})
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
		return nil, apierr.NotImplemented.Msg("city unregister is not available in this supervisor (no initializer wired)")
	}
	name := strings.TrimSpace(input.CityName)
	if name == "" {
		return nil, apierr.InvalidRequest.Msg("city_name is required")
	}

	reqID, err := newRequestID()
	if err != nil {
		return nil, apierr.Internal.Msg(fmt.Sprintf("generating request ID: %v", err))
	}
	eventCursor, cursorErr := sm.currentSupervisorEventCursor()
	if cursorErr != nil {
		return nil, apierr.Internal.Msg(cursorErr.Error())
	}

	// Store the pending request_id BEFORE Unregister triggers a
	// reconciler reload, so the reconciler can correlate the
	// terminal request.result event. Look up the city path from
	// the resolver first; if the city isn't known, Unregister will
	// return ErrNotRegistered anyway.
	var cityPath string
	if store, ok := sm.resolver.(PendingRequestStore); ok {
		var pathErr error
		cityPath, pathErr = sm.cityPathForPendingRequest(ctx, name)
		if pathErr != nil {
			return nil, apierr.Internal.Msg(fmt.Sprintf("resolving city path: %v", pathErr))
		}
		if cityPath != "" {
			if err := store.StorePendingRequestID(cityPath, reqID); err != nil {
				if errors.Is(err, ErrPendingRequestExists) {
					return nil, apierr.OperationInProgress.Msg("conflict: city operation already in progress at " + cityPath)
				}
				return nil, apierr.Internal.Msg(fmt.Sprintf("storing pending request ID: %v", err))
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
		return nil, apierr.CityNotFound.Msg("not_found: " + unregErr.Error())
	case unregErr != nil:
		if store, ok := sm.resolver.(PendingRequestStore); ok && cityPath != "" {
			if _, _, err := store.ConsumePendingRequestID(cityPath); err != nil {
				log.Printf("api: consume pending city unregister request ID for %s: %v", cityPath, err)
			}
		}
		return nil, apierr.Internal.Msg(unregErr.Error())
	}

	out := &SupervisorCityUnregisterOutput{Status: http.StatusAccepted}
	out.Body = cityUnregisterResponse{RequestID: reqID, EventCursor: eventCursor}
	return out, nil
}

func (sm *SupervisorMux) cityPathForPendingRequest(ctx context.Context, name string) (string, error) {
	for _, c := range sm.resolver.ListCities() {
		if c.Name == name {
			return c.Path, nil
		}
	}
	finder, ok := sm.initializer.(registeredCityFinder)
	if !ok {
		return "", nil
	}
	city, err := finder.FindRegisteredCity(ctx, name)
	if err != nil {
		if errors.Is(err, cityinit.ErrNotRegistered) {
			return "", nil
		}
		return "", err
	}
	return city.Path, nil
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
	eventCursor, cursorErr := supervisorEventCursorFromMux(mux)
	if cursorErr != nil {
		return nil, apierr.Internal.Msg(cursorErr.Error())
	}
	filter := events.Filter{Type: input.Type, Actor: input.Actor}
	if d, ok, err := parseEventSince(input.Since); err != nil {
		return nil, err
	} else if ok {
		filter.Since = time.Now().Add(-d)
	}
	var evts []events.TaggedEvent
	var err error
	optimizedTail := input.Limit > 0 && supervisorEventListFilterIsEmpty(filter)
	if optimizedTail {
		evts, err = mux.ListTail(filter, input.Limit)
	} else {
		evts, err = mux.ListAll(filter)
	}
	if err != nil {
		return nil, apierr.Internal.Msg("internal: " + err.Error())
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
	out.Body.EventCursor = eventCursor
	// Total is the full match count so clients can distinguish "limit
	// truncated" from "the server only had N events."
	out.Body.Total = len(wires)
	if optimizedTail {
		out.Body.Total = sm.currentSupervisorEventTotal()
	}
	// Limit clamp: take the N most recent events (wires is already
	// chronologically ordered). Critical for `gc events --seq` which
	// computes the head cursor from the last event only.
	if !optimizedTail && input.Limit > 0 && input.Limit < len(wires) {
		wires = wires[len(wires)-input.Limit:]
	}
	out.Body.Items = wires
	return out, nil
}

func supervisorEventListFilterIsEmpty(filter events.Filter) bool {
	return filter == (events.Filter{})
}

func (sm *SupervisorMux) currentSupervisorEventTotal() int {
	mux := sm.buildMultiplexer()
	cursors, err := mux.LatestCursor()
	if err != nil {
		log.Printf("api: supervisor events total: %v", err)
	}
	// This optimized unfiltered total treats LatestSeq as an event count because
	// event logs are append-only, gap-free, and unpruned today. Any future
	// retention/pruning/compaction must replace this path with an explicit count
	// API.
	const maxInt = int(^uint(0) >> 1)
	total := 0
	for _, seq := range cursors {
		if seq > uint64(maxInt-total) {
			return maxInt
		}
		total += int(seq)
	}
	return total
}

func (sm *SupervisorMux) currentSupervisorEventCursor() (string, error) {
	return supervisorEventCursorFromMux(sm.buildMultiplexer())
}

func supervisorEventCursorFromMux(mux *events.Multiplexer) (string, error) {
	cursors, err := mux.LatestCursor()
	if err != nil {
		// Async writes and history-to-SSE handoffs need a complete cursor for
		// all cities. Fail before accepting the request or returning history so
		// clients never wait from an ambiguous cursor.
		return "", fmt.Errorf("capturing supervisor event cursor: %w", err)
	}
	if cursor := events.FormatCursor(cursors); cursor != "" {
		return cursor, nil
	}
	return "0", nil
}

// --- Supervisor global events stream (Fix 3g final wiring) ---

// resolveGlobalStreamCursors builds the per-city cursor map for a global
// event-stream Watch so that no registered city falls through to Watch(0),
// which now replays a city's entire retained history across archives.
//
// With no resume cursor — a head-start client or the attach-only precheck —
// every city starts from its latest cursor. With a resume cursor, cities the
// cursor omits are floored to their latest cursor so a cursor that predates a
// newly registered city cannot trigger a full-history flood for it. It fails
// closed on a LatestCursor error rather than letting unresolved cities default
// to cursor 0. The returned map is always non-nil on success.
func resolveGlobalStreamCursors(mux *events.Multiplexer, resumeCursor string) (map[string]uint64, error) {
	resumeCursor = strings.TrimSpace(resumeCursor)
	if resumeCursor == "" {
		cursors, err := mux.LatestCursor()
		if err != nil {
			return nil, err
		}
		if cursors == nil {
			cursors = make(map[string]uint64)
		}
		return cursors, nil
	}
	cursors := events.ParseCursor(resumeCursor)
	if cursors == nil {
		cursors = make(map[string]uint64)
	}
	latest, err := mux.LatestCursor()
	if err != nil {
		return nil, err
	}
	for city, seq := range latest {
		if _, ok := cursors[city]; !ok {
			cursors[city] = seq
		}
	}
	return cursors, nil
}

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
// The precheck attaches a watcher at each city's head cursor and closes it
// immediately — a cheap probe that surfaces per-city watcher failures at the
// point where we can still return a proper HTTP error. It must not attach with
// nil cursors: nil defaults every child to Watch(0), which now replays the
// entire retained history across archives, so a bare probe would gunzip and
// decode archived batches for every city only to discard them when it closes.
// Resolving head cursors keeps the probe cheap and fails closed exactly like
// the streamGlobalEvents head-start path.
func (sm *SupervisorMux) precheckGlobalEventStream(ctx context.Context, _ *SupervisorEventStreamInput) error {
	mux := sm.buildMultiplexer()
	if mux.Len() == 0 {
		return apierr.ServiceUnavailable.Msg("no_providers: no event providers available")
	}
	cursors, err := resolveGlobalStreamCursors(mux, "")
	if err != nil {
		return apierr.ServiceUnavailable.Msg("cursor_failed: " + err.Error())
	}
	probe, err := mux.Watch(ctx, cursors)
	if err != nil {
		if errors.Is(err, events.ErrNoWatchers) {
			return apierr.ServiceUnavailable.Msg("no_watchers: event providers are registered but none are watchable")
		}
		return apierr.ServiceUnavailable.Msg("watch_failed: " + err.Error())
	}
	_ = probe.Close()
	return nil
}

// streamGlobalEvents emits tagged events with composite per-city cursor IDs.
// Once the stream is prepared and headers are committed, failures terminate
// the stream cleanly because there is no way to return an HTTP error.
func (sm *SupervisorMux) streamGlobalEvents(hctx huma.Context, input *SupervisorEventStreamInput, send StringIDSender) {
	cursor := strings.TrimSpace(input.LastEventID)
	if cursor == "" {
		cursor = strings.TrimSpace(input.AfterCursor)
	}

	mux := sm.buildMultiplexer()
	// Resolve per-city cursors so no city falls through to Watch(0) full-history
	// replay: head-start clients start every city from now, and a resume cursor
	// that omits a registered city floors that city to its latest cursor. Fail
	// closed on a LatestCursor error — the client can reconnect.
	cursors, err := resolveGlobalStreamCursors(mux, cursor)
	if err != nil {
		log.Printf("api: supervisor events-stream: resolving stream cursors failed, refusing full-history replay: %v", err)
		return
	}
	mw, err := mux.Watch(hctx.Context(), cursors)
	if err != nil {
		log.Printf("api: supervisor events-stream: Watch failed cursors=%v: %v", cursors, err)
		return
	}
	defer mw.Close() //nolint:errcheck
	flushSSEHeaders(hctx)

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
