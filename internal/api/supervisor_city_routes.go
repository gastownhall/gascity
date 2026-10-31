package api

import (
	"net/http"
	"reflect"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/runtime"
)

// sessionStreamEventMap is the event map for the session SSE stream.
// Extracted so it can be referenced from the scoped registration site
// without re-defining the shape.
func sessionStreamEventMap() map[string]any {
	return map[string]any{
		"turn":            SessionStreamMessageEvent{},
		"message":         SessionStreamRawMessageEvent{},
		"structured":      SessionStreamStructuredMessageEvent{},
		"activity":        SessionActivityEvent{},
		"pending":         runtime.PendingInteraction{},
		"pending_cleared": SessionPendingClearedEvent{},
		"heartbeat":       HeartbeatEvent{},
	}
}

// registerCityRoutes registers per-city Huma operations at their
// user-facing scoped paths ("/v0/city/{cityName}/..."). Called from
// NewSupervisorMux after registerSupervisorRoutes.
//
// All entries use the cityGet/Post/Patch/Delete/Put/Register +
// sseCityPrecheck/sseCityStream helpers from city_scope.go, which
// embed the /v0/city/{cityName} prefix and wrap each handler with
// per-request city resolution.
func (sm *SupervisorMux) registerCityRoutes() {
	// Status + Health.
	cityGet(sm, "/status", (*Server).humaHandleStatus, errorStatuses(http.StatusNotFound, http.StatusServiceUnavailable))
	cityGet(sm, "/health", (*Server).humaHandleHealth, errorStatuses(http.StatusNotFound))
	cityGet(sm, "/usage", (*Server).humaHandleUsage, errorStatuses(http.StatusNotFound, http.StatusServiceUnavailable))

	// City detail.
	cityGet(sm, "", (*Server).humaHandleCityGet, errorStatuses(http.StatusNotFound))
	cityPatch(sm, "", (*Server).humaHandleCityPatch, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusNotImplemented))

	// Readiness (per-city).
	cityGet(sm, "/readiness", (*Server).humaHandleReadiness, errorStatuses(http.StatusBadRequest, http.StatusNotFound))
	cityGet(sm, "/provider-readiness", (*Server).humaHandleProviderReadiness, errorStatuses(http.StatusBadRequest, http.StatusNotFound))

	// Config.
	cityGet(sm, "/config", (*Server).humaHandleConfigGet, errorStatuses(http.StatusNotFound))
	cityGet(sm, "/config/explain", (*Server).humaHandleConfigExplain, errorStatuses(http.StatusNotFound))
	cityGet(sm, "/config/validate", (*Server).humaHandleConfigValidate, errorStatuses(http.StatusNotFound))
	cityGet(sm, "/config/defaults", (*Server).humaHandleConfigDefaults, errorStatuses(http.StatusNotFound))

	// Agents — read / CRUD. Agents can be addressed unqualified
	// ({base}) or rig-qualified ({dir}/{base}); there is no third
	// form, so two explicit routes cover every real case without a
	// trailing-path wildcard. The routes we register are the routes
	// we expose.
	cityGet(sm, "/agents", (*Server).humaHandleAgentList, errorStatuses(http.StatusNotFound))
	cityGet(sm, "/agent/{dir}/{base}/output", (*Server).humaHandleAgentOutputQualified, errorStatuses(http.StatusNotFound))
	cityGet(sm, "/agent/{base}/output", (*Server).humaHandleAgentOutput, errorStatuses(http.StatusNotFound))
	cityGet(sm, "/agent/{dir}/{base}", (*Server).humaHandleAgentQualified, errorStatuses(http.StatusNotFound))
	cityGet(sm, "/agent/{base}", (*Server).humaHandleAgent, errorStatuses(http.StatusNotFound))
	cityRegister(sm, huma.Operation{
		OperationID:   "create-agent",
		Method:        http.MethodPost,
		Path:          "/agents",
		Summary:       "Create an agent",
		Description:   "Creates an agent and waits until it is visible to immediate follow-up operations. If the agent is durably created but visibility confirmation is canceled or times out, the retryable 503/504 response includes a Retry-After header.",
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusNotImplemented, http.StatusServiceUnavailable, http.StatusGatewayTimeout},
	}, (*Server).humaHandleAgentCreate)
	cityPatch(sm, "/agent/{dir}/{base}", (*Server).humaHandleAgentUpdateQualified, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusNotImplemented))
	cityPatch(sm, "/agent/{base}", (*Server).humaHandleAgentUpdate, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusNotImplemented))
	cityDelete(sm, "/agent/{dir}/{base}", (*Server).humaHandleAgentDeleteQualified, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusNotImplemented))
	cityDelete(sm, "/agent/{base}", (*Server).humaHandleAgentDelete, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusNotImplemented))
	cityPost(sm, "/agent/{dir}/{base}/{action}", (*Server).humaHandleAgentActionQualified, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusNotImplemented))
	cityPost(sm, "/agent/{base}/{action}", (*Server).humaHandleAgentAction, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusNotImplemented))

	// Agent output SSE streams.
	agentOutputEventMap := map[string]any{
		"turn":      agentOutputResponse{},
		"heartbeat": HeartbeatEvent{},
	}
	registerSSE(sm.humaAPI, huma.Operation{
		OperationID: "stream-agent-output",
		Method:      http.MethodGet,
		Path:        cityScopePrefix + "/agent/{base}/output/stream",
		Summary:     "Stream agent output in real time",
		Description: "Server-Sent Events stream of agent output (session log tail or tmux pane polling).",
		Responses:   sseResponseHeaders("GC-Agent-Status"),
	}, agentOutputEventMap,
		sseCityPrecheck(sm, (*Server).checkAgentOutputStream),
		sseCityStream(sm, (*Server).streamAgentOutput))
	registerSSE(sm.humaAPI, huma.Operation{
		OperationID: "stream-agent-output-qualified",
		Method:      http.MethodGet,
		Path:        cityScopePrefix + "/agent/{dir}/{base}/output/stream",
		Summary:     "Stream agent output in real time (qualified name)",
		Description: "Server-Sent Events stream of agent output for qualified (rig-prefixed) agent names.",
		Responses:   sseResponseHeaders("GC-Agent-Status"),
	}, agentOutputEventMap,
		sseCityPrecheck(sm, (*Server).checkAgentOutputStreamQualified),
		sseCityStream(sm, (*Server).streamAgentOutputQualified))

	// Providers.
	cityGet(sm, "/providers", (*Server).humaHandleProviderList, errorStatuses(http.StatusNotFound))
	cityGet(sm, "/providers/public", (*Server).humaHandleProviderPublicList, errorStatuses(http.StatusNotFound))
	cityGet(sm, "/provider/{name}", (*Server).humaHandleProviderGet, errorStatuses(http.StatusNotFound))
	cityRegister(sm, huma.Operation{
		OperationID:   "create-provider",
		Method:        http.MethodPost,
		Path:          "/providers",
		Summary:       "Create a provider",
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusNotImplemented},
	}, (*Server).humaHandleProviderCreate)
	cityPatch(sm, "/provider/{name}", (*Server).humaHandleProviderUpdate, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusNotImplemented))
	cityDelete(sm, "/provider/{name}", (*Server).humaHandleProviderDelete, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusNotImplemented))

	// Rigs.
	cityGet(sm, "/rigs", (*Server).humaHandleRigList, errorStatuses(http.StatusNotFound, http.StatusServiceUnavailable))
	cityGet(sm, "/rig/{name}", (*Server).humaHandleRigGet, errorStatuses(http.StatusNotFound))
	// create-rig returns one of three success statuses (201 sync create, 202
	// async clone accepted, 200 idempotent replay) over one union body. Huma
	// only auto-schematizes op.DefaultStatus (201), so the 200/202 responses are
	// declared manually here — cityRegister takes op by value with no
	// op-modifier closure (city_scope.go), so they must exist before the call.
	// All three reference the same RigCreateResponseBody registry schema, so
	// genclient/dashboard get one type discriminated by status.
	rigBodyRef := sm.humaAPI.OpenAPI().Components.Schemas.Schema(
		reflect.TypeOf(RigCreateResponseBody{}), true, "RigCreateResponseBody")
	// Huma's defineErrors only synthesizes the default application/problem+json
	// error response when op.Responses has at most one entry (huma.go: the
	// `len(op.Responses) <= 1` guard). Declaring the 200/202 union bodies manually
	// trips that guard, so the default error response would be dropped and the
	// generated CreateRigResponse would lose ApplicationproblemJSONDefault —
	// degrading every 400/409 (including the structured 409 that carries the
	// re-attach request_id + event_cursor) to a detail-less "API returned NNN".
	// Restore it here so it references the same apierr.ErrorModel schema this fork
	// registers under the "ErrorModel" name for every other cityPost op (using
	// huma.ErrorModel here would double-register that name and panic at startup).
	errModelRef := sm.humaAPI.OpenAPI().Components.Schemas.Schema(
		reflect.TypeOf(apierr.ErrorModel{}), true, "ErrorModel")
	cityRegister(sm, huma.Operation{
		OperationID:   "create-rig",
		Method:        http.MethodPost,
		Path:          "/rigs",
		Summary:       "Create a rig",
		Description:   "Create a rig. Without git_url, appends the rig to city.toml synchronously (201). With git_url, clones and provisions asynchronously: returns 202 with an event_cursor — watch the city event stream for request.result.rig.create, rig.provision.progress, or request.failed carrying the request_id — or 200 for an idempotent replay of a succeeded create.",
		DefaultStatus: http.StatusCreated, // 201 — Huma auto-schematizes the union body here
		Responses: map[string]*huma.Response{
			"200": {
				Description: "Rig already exists — idempotent request_id replay of a succeeded async create.",
				Content:     map[string]*huma.MediaType{"application/json": {Schema: rigBodyRef}},
			},
			"202": {
				Description: "Provisioning accepted; watch the city event stream from event_cursor for request.result.rig.create, rig.provision.progress, or request.failed with this request_id.",
				Content:     map[string]*huma.MediaType{"application/json": {Schema: rigBodyRef}},
			},
			"default": {
				Description: "Error",
				Content:     map[string]*huma.MediaType{"application/problem+json": {Schema: errModelRef}},
			},
		},
	}, (*Server).humaHandleRigCreate)
	cityPatch(sm, "/rig/{name}", (*Server).humaHandleRigUpdate, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusNotImplemented))
	cityDelete(sm, "/rig/{name}", (*Server).humaHandleRigDelete, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusNotImplemented))
	cityPost(sm, "/rig/{name}/{action}", (*Server).humaHandleRigAction, errorStatuses(http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusNotImplemented))

	// Patches — agent. Same qualified/unqualified split as /agent: two
	// explicit routes instead of a trailing-path wildcard.
	cityGet(sm, "/patches/agents", (*Server).humaHandleAgentPatchList, errorStatuses(http.StatusNotFound))
	cityGet(sm, "/patches/agent/{dir}/{base}", (*Server).humaHandleAgentPatchGetQualified, errorStatuses(http.StatusNotFound))
	cityGet(sm, "/patches/agent/{base}", (*Server).humaHandleAgentPatchGet, errorStatuses(http.StatusNotFound))
	cityPut(sm, "/patches/agents", (*Server).humaHandleAgentPatchSet, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusNotImplemented))
	cityDelete(sm, "/patches/agent/{dir}/{base}", (*Server).humaHandleAgentPatchDeleteQualified, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusNotImplemented))
	cityDelete(sm, "/patches/agent/{base}", (*Server).humaHandleAgentPatchDelete, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusNotImplemented))
	// Patches — rig.
	cityGet(sm, "/patches/rigs", (*Server).humaHandleRigPatchList, errorStatuses(http.StatusNotFound))
	cityGet(sm, "/patches/rig/{name}", (*Server).humaHandleRigPatchGet, errorStatuses(http.StatusNotFound))
	cityPut(sm, "/patches/rigs", (*Server).humaHandleRigPatchSet, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusNotImplemented))
	cityDelete(sm, "/patches/rig/{name}", (*Server).humaHandleRigPatchDelete, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusNotImplemented))
	// Patches — provider.
	cityGet(sm, "/patches/providers", (*Server).humaHandleProviderPatchList, errorStatuses(http.StatusNotFound))
	cityGet(sm, "/patches/provider/{name}", (*Server).humaHandleProviderPatchGet, errorStatuses(http.StatusNotFound))
	cityPut(sm, "/patches/providers", (*Server).humaHandleProviderPatchSet, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusNotImplemented))
	cityDelete(sm, "/patches/provider/{name}", (*Server).humaHandleProviderPatchDelete, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusNotImplemented))

	// Beads. The bead ops are the P12 error-contract pilot: each declares the
	// error statuses it can return (Huma adds the auto 422/500) so its problem+json
	// responses are enumerated in the spec and machine-branchable via the type/code
	// the handler stamps through the apierr catalog. Mutations additionally declare
	// 403 because the always-installed CSRF middleware (and read-only mode) reject
	// a mutation with a 403 before the handler runs; reads never emit it.
	// GET /beads also declares 400: an invalid pagination cursor is a typed
	// invalid-cursor problem response, never a silent page-1 restart.
	cityGet(sm, "/beads", (*Server).humaHandleBeadList, errorStatuses(http.StatusBadRequest, http.StatusNotFound, http.StatusServiceUnavailable), listOrder("(created_at DESC, id DESC) — newest beads first"))
	cityGet(sm, "/beads/graph/{rootID}", (*Server).humaHandleBeadGraph, errorStatuses(http.StatusNotFound))
	cityGet(sm, "/beads/ready", (*Server).humaHandleBeadReady, errorStatuses(http.StatusNotFound, http.StatusServiceUnavailable))
	// Ready work as `gc ready` answers it: live, the serving frame, fail-loud
	// (see ReadyWorkReader). Distinct from /beads/ready, which prefers the
	// controller's read cache and degrades a failing rig to a partial answer.
	// An unknown query parameter is refused: a misspelled filter would
	// otherwise widen the answer to everything.
	cityGet(sm, "/work/ready", (*Server).humaHandleWorkReady, errorStatuses(http.StatusBadRequest, http.StatusNotFound, http.StatusServiceUnavailable), describes(workReadyDescription),
		func(o *huma.Operation) { o.RejectUnknownQueryParameters = true })
	// A session's step reminder as its hooks read it: live, over the stores the
	// controller holds (see ActiveWorkStepReader). Unknown query parameters are
	// refused for the same reason as /work/ready.
	cityGet(sm, "/work/active-step", (*Server).humaHandleWorkActiveStep, errorStatuses(http.StatusBadRequest, http.StatusNotFound, http.StatusServiceUnavailable), describes(workActiveStepDescription),
		func(o *huma.Operation) { o.RejectUnknownQueryParameters = true })
	cityRegister(sm, huma.Operation{
		OperationID:   "create-bead",
		Method:        http.MethodPost,
		Path:          "/beads",
		Summary:       "Create a bead",
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict},
		Description:   ownedRowGuardDescription,
	}, (*Server).humaHandleBeadCreate)
	cityGet(sm, "/bead/{id}", (*Server).humaHandleBeadGet, errorStatuses(http.StatusNotFound, http.StatusServiceUnavailable))
	cityGet(sm, "/bead/{id}/deps", (*Server).humaHandleBeadDeps, errorStatuses(http.StatusNotFound))
	cityPost(sm, "/bead/{id}/close", (*Server).humaHandleBeadClose, errorStatuses(http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict))
	cityPost(sm, "/bead/{id}/reopen", (*Server).humaHandleBeadReopen, errorStatuses(http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict))
	cityPost(sm, "/bead/{id}/update", (*Server).humaHandleBeadUpdate, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable, http.StatusGatewayTimeout), describes(beadUpdateDescription), describes(ownedRowGuardDescription))
	cityPatch(sm, "/bead/{id}", (*Server).humaHandleBeadUpdate, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable, http.StatusGatewayTimeout), describes(beadUpdateDescription), describes(ownedRowGuardDescription))
	cityPost(sm, "/bead/{id}/assign", (*Server).humaHandleBeadAssign, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict))
	cityDelete(sm, "/bead/{id}", (*Server).humaHandleBeadDelete, errorStatuses(http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict),
		describes("Closes the bead. This is a soft delete: the bead remains readable and retains its history, with status \"closed\". The API exposes no hard delete, so a caller that needs the record actually removed must not rely on this endpoint."))

	// Mail. Part of the P12 error-contract slice (see Beads above): each op
	// enumerates the error statuses it can return (Huma adds auto 422/500);
	// mutations declare 403 for the CSRF/read-only middleware.
	cityGet(sm, "/mail", (*Server).humaHandleMailList, errorStatuses(http.StatusBadRequest, http.StatusNotFound, http.StatusServiceUnavailable), listOrder("(created_at DESC, id DESC) — newest messages first"))
	cityRegister(sm, huma.Operation{
		OperationID:   "send-mail",
		Method:        http.MethodPost,
		Path:          "/mail",
		Summary:       "Send a mail message",
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict},
	}, (*Server).humaHandleMailSend)
	cityGet(sm, "/mail/count", (*Server).humaHandleMailCount, errorStatuses(http.StatusNotFound, http.StatusServiceUnavailable))
	cityGet(sm, "/mail/thread/{id}", (*Server).humaHandleMailThread, errorStatuses(http.StatusNotFound, http.StatusServiceUnavailable))
	cityGet(sm, "/mail/{id}", (*Server).humaHandleMailGet, errorStatuses(http.StatusNotFound, http.StatusServiceUnavailable))
	cityPost(sm, "/mail/{id}/read", (*Server).humaHandleMailRead, errorStatuses(http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound))
	cityPost(sm, "/mail/{id}/mark-unread", (*Server).humaHandleMailMarkUnread, errorStatuses(http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound))
	cityPost(sm, "/mail/{id}/archive", (*Server).humaHandleMailArchive, errorStatuses(http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound))
	cityRegister(sm, huma.Operation{
		OperationID:   "reply-mail",
		Method:        http.MethodPost,
		Path:          "/mail/{id}/reply",
		Summary:       "Reply to a mail message",
		DefaultStatus: http.StatusCreated,
		// 409: a concurrent repeat of the same Idempotency-Key (idempotency-in-flight).
		Errors: []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict},
	}, (*Server).humaHandleMailReply)
	cityDelete(sm, "/mail/{id}", (*Server).humaHandleMailDelete, errorStatuses(http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound))

	// Convoys.
	// 400: invalid pagination cursor (invalid-cursor problem type).
	cityGet(sm, "/convoys", (*Server).humaHandleConvoyList, errorStatuses(http.StatusBadRequest, http.StatusNotFound, http.StatusServiceUnavailable), listOrder("(created_at DESC, id DESC) — newest convoys first"))
	cityRegister(sm, huma.Operation{
		OperationID:   "create-convoy",
		Method:        http.MethodPost,
		Path:          "/convoys",
		Summary:       "Create a convoy",
		DefaultStatus: http.StatusCreated,
		// 409: a concurrent repeat of the same Idempotency-Key (idempotency-in-flight).
		Errors: []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict},
	}, (*Server).humaHandleConvoyCreate)
	cityGet(sm, "/convoy/{id}", (*Server).humaHandleConvoyGet, errorStatuses(http.StatusNotFound, http.StatusServiceUnavailable))
	cityPost(sm, "/convoy/{id}/add", (*Server).humaHandleConvoyAdd, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound))
	cityPost(sm, "/convoy/{id}/remove", (*Server).humaHandleConvoyRemove, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound))
	cityGet(sm, "/convoy/{id}/check", (*Server).humaHandleConvoyCheck, errorStatuses(http.StatusBadRequest, http.StatusNotFound, http.StatusServiceUnavailable))
	cityPost(sm, "/convoy/{id}/close", (*Server).humaHandleConvoyClose, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound))
	cityDelete(sm, "/convoy/{id}", (*Server).humaHandleConvoyDelete, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound))

	// Events (list/emit/rotate — stream is a separate SSE registration below).
	cityGet(sm, "/events", (*Server).humaHandleEventList, errorStatuses(http.StatusBadRequest, http.StatusNotFound), listOrder("seq DESC — newest events first"))
	cityRegister(sm, huma.Operation{
		OperationID:   "emit-event",
		Method:        http.MethodPost,
		Path:          "/events",
		Summary:       "Emit an event",
		Description:   "Appends a caller-authored event to the city's event log. op.changed and fact.recorded, which the controller derives from operation-journal rows, are refused 422.",
		DefaultStatus: http.StatusCreated,
		// 409: a concurrent repeat of the same Idempotency-Key (idempotency-in-flight).
		Errors: []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable},
	}, (*Server).humaHandleEventEmit)
	cityRegister(sm, huma.Operation{
		OperationID: "rotate-events",
		Method:      http.MethodPost,
		Path:        "/events/rotate",
		Summary:     "Force rotate the city event log",
		Errors:      []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusMethodNotAllowed},
	}, (*Server).humaHandleEventRotate)

	// Orders.
	cityGet(sm, "/orders", (*Server).humaHandleOrderList, errorStatuses(http.StatusNotFound))
	cityGet(sm, "/orders/check", (*Server).humaHandleOrderCheck, errorStatuses(http.StatusNotFound))
	cityGet(sm, "/orders/history", (*Server).humaHandleOrderHistory, errorStatuses(http.StatusBadRequest, http.StatusNotFound, http.StatusServiceUnavailable))
	cityGet(sm, "/order/history/{bead_id}", (*Server).humaHandleOrderHistoryDetail, errorStatuses(http.StatusNotFound, http.StatusServiceUnavailable))
	cityGet(sm, "/order/{name}", (*Server).humaHandleOrderGet, errorStatuses(http.StatusNotFound, http.StatusConflict))
	cityPost(sm, "/order/{name}/enable", (*Server).humaHandleOrderEnable, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusNotImplemented))
	cityPost(sm, "/order/{name}/disable", (*Server).humaHandleOrderDisable, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusNotImplemented))
	// Typed operator path to fire a trigger="webhook" order directly with typed
	// params. Inherits write-auth/CSRF/read-only from cityPost (write-auth IS the
	// auth here — no signature). Reuses the E6 sink + E0.5 dispatcher seam.
	cityPost(sm, "/order/{name}/run", (*Server).humaHandleOrderRun, func(op *huma.Operation) {
		op.DefaultStatus = http.StatusAccepted
	}, errorStatuses(http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusServiceUnavailable))
	cityGet(sm, "/orders/feed", (*Server).humaHandleOrdersFeed, errorStatuses(http.StatusBadRequest, http.StatusNotFound))

	// Formulas.
	cityGet(sm, "/formulas", (*Server).humaHandleFormulaList, errorStatuses(http.StatusBadRequest, http.StatusNotFound, http.StatusServiceUnavailable))
	cityGet(sm, "/formulas/{name}/runs", (*Server).humaHandleFormulaRuns, errorStatuses(http.StatusBadRequest, http.StatusNotFound, http.StatusServiceUnavailable))
	cityGet(sm, "/formulas/{name}/source", (*Server).humaHandleFormulaSource, errorStatuses(http.StatusBadRequest, http.StatusNotFound, http.StatusNotImplemented))
	cityGet(sm, "/formulas/{name}", (*Server).humaHandleFormulaDetail, errorStatuses(http.StatusBadRequest, http.StatusNotFound, http.StatusServiceUnavailable))
	cityGet(sm, "/formula/{name}", (*Server).humaHandleFormulaDetail, errorStatuses(http.StatusBadRequest, http.StatusNotFound, http.StatusServiceUnavailable))
	cityPost(sm, "/formulas/{name}/preview", (*Server).humaHandleFormulaPreview, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusServiceUnavailable))
	cityPost(sm, "/formulas/{name}/validate", (*Server).humaHandleFormulaValidate, withMaxFormulaBody, errorStatuses(http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusRequestEntityTooLarge))
	cityPut(sm, "/formulas/{name}", (*Server).humaHandleFormulaUpsert, withMaxFormulaBody, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusRequestEntityTooLarge, http.StatusNotImplemented))
	cityDelete(sm, "/formulas/{name}", (*Server).humaHandleFormulaDelete, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusNotImplemented))
	cityGet(sm, "/formulas/feed", (*Server).humaHandleFormulaFeed, errorStatuses(http.StatusBadRequest, http.StatusNotFound, http.StatusServiceUnavailable))
	// Backwards-compatible workflow aliases.
	cityGet(sm, "/workflow/{workflow_id}", (*Server).humaHandleWorkflowGet, errorStatuses(http.StatusBadRequest, http.StatusNotFound))
	cityDelete(sm, "/workflow/{workflow_id}", (*Server).humaHandleWorkflowDelete, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound))

	// Canonical Run resource — the ONE typed run projection, sourced from the
	// city event log.
	cityGet(sm, "/runs", (*Server).humaHandleRunsList, errorStatuses(http.StatusServiceUnavailable), describes(runProjectionFreshness))
	cityGet(sm, "/runs/census", (*Server).humaHandleRunsCensus, errorStatuses(http.StatusServiceUnavailable), describes(runProjectionFreshness))
	cityGet(sm, "/runs/{run_id}", (*Server).humaHandleRunGet, errorStatuses(http.StatusNotFound, http.StatusServiceUnavailable), describes(runProjectionFreshness))
	cityGet(sm, "/runs/{run_id}/steps", (*Server).humaHandleRunSteps, errorStatuses(http.StatusNotFound, http.StatusServiceUnavailable), describes(runProjectionFreshness))
	cityPost(sm, "/runs/{run_id}/cancel", (*Server).humaHandleRunCancel, func(op *huma.Operation) {
		op.DefaultStatus = http.StatusAccepted
	}, errorStatuses(http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable),
		describes("A successful cancel folds its own writes into the run projection before it responds, so a GET /runs/{run_id} or /runs/{run_id}/steps issued after the 202 reports the run and its open steps canceled."))

	// Packs.
	cityGet(sm, "/packs", (*Server).humaHandlePackList, errorStatuses(http.StatusBadRequest, http.StatusNotFound))
	cityRegister(sm, huma.Operation{
		OperationID:   "add-pack",
		Method:        http.MethodPost,
		Path:          "/packs",
		Summary:       "Add a pack",
		Description:   "Imports a pack into the city by source (a remote git URL or registry ref), resolving + installing it so its templates compose into the city.",
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusBadGateway},
	}, (*Server).humaHandlePackAdd)
	cityDelete(sm, "/packs/{name}", (*Server).humaHandlePackRemove, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound))

	// Sling. Part of the P12 error-contract pilot (see Beads above); a mutation,
	// so it also declares 403 for the CSRF/read-only middleware.
	cityPost(sm, "/sling", (*Server).humaHandleSling, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict),
		describes("A convoy bead is expanded and each open child is routed separately, as gc sling does; the response then carries batch counts and one children entry per child. "+
			"When some children were routed and others failed, the sling has already changed state, so it answers 200 with status \"partial\" and the per-child outcomes (failed children carry a reason, and a conflict naming the blocking workflows when a live source workflow refused them) rather than an error; do not retry such a request as a whole. "+
			"A convoy sling that routed no child and failed at least one is an error, as before."))

	// Maintenance (Dolt store gc + snapshot).
	cityGet(sm, "/maintenance/status", (*Server).humaHandleMaintenanceStatus, errorStatuses(http.StatusNotFound, http.StatusServiceUnavailable))
	cityRegister(sm, huma.Operation{
		OperationID:   "trigger-maintenance-dolt-gc",
		Method:        http.MethodPost,
		Path:          "/maintenance/dolt-gc",
		Summary:       "Trigger a Dolt store maintenance run",
		Description:   "Trigger a one-off maintenance cycle (dolt backup + CALL DOLT_GC + smoke test). Default async (202); ?wait=true blocks until completion (200). Returns 409 when a run is already in flight.",
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable},
	}, (*Server).humaHandleMaintenanceTriggerDoltGC)

	// Services (workspace services).
	cityGet(sm, "/services", (*Server).humaHandleServiceList, errorStatuses(http.StatusNotFound))
	cityGet(sm, "/service/{name}", (*Server).humaHandleServiceGet, errorStatuses(http.StatusNotFound))
	cityPost(sm, "/service/{name}/restart", (*Server).humaHandleServiceRestart, errorStatuses(http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound))

	// Sessions (non-stream — stream is the SSE registration below).
	cityRegister(sm, huma.Operation{
		OperationID:   "create-session",
		Method:        http.MethodPost,
		Path:          "/sessions",
		Summary:       "Create a session",
		DefaultStatus: http.StatusAccepted,
		// 409: a same-key retry while the first create is still being accepted.
		Errors: []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable},
	}, (*Server).humaHandleSessionCreate)
	// 400: invalid pagination cursor (invalid-cursor problem type).
	cityGet(sm, "/sessions", (*Server).humaHandleSessionList, errorStatuses(http.StatusBadRequest, http.StatusNotFound, http.StatusServiceUnavailable), listOrder("(created_at DESC, id DESC) — newest sessions first"))
	cityGet(sm, "/session/{id}", (*Server).humaHandleSessionGet, errorStatuses(http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable))
	cityGet(sm, "/session/{id}/transcript", (*Server).humaHandleSessionTranscript, errorStatuses(http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable))
	cityGet(sm, "/session/{id}/pending", (*Server).humaHandleSessionPending, errorStatuses(http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable))
	cityGet(sm, "/session/{id}/turn", (*Server).humaHandleSessionTurn, errorStatuses(http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable),
		describes("Reads the session's turn identity, read-only: where its turns are read from (source), how far that answer can be trusted (capabilities), and what one read saw now (observation, current, recent). "+
			"A session whose turns cannot be identified answers 200 with source none and observation unsupported, never a 5xx; a source that cannot be read now answers 200 with observation unavailable, meaning its turns are unknown, not idle. "+
			"An open session that no runtime is bound to runs no turn: it answers observed with no current turn, as POST /session/{id}/stop reads it. A closed or unknown session is 404. The read aims nothing at a turn: POST /session/{id}/stop interrupts the turn its turn field names, or whatever turn is current when it names none."))
	cityGet(sm, "/pending", (*Server).humaHandleCityPending, errorStatuses(http.StatusNotFound, http.StatusServiceUnavailable))
	cityPatch(sm, "/session/{id}", (*Server).humaHandleSessionPatch, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable))
	cityPost(sm, "/session/{id}/permission-mode", (*Server).humaHandleSessionPermissionMode, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusNotImplemented, http.StatusServiceUnavailable))
	cityRegister(sm, huma.Operation{
		OperationID:   "submit-session",
		Method:        http.MethodPost,
		Path:          "/session/{id}/submit",
		Summary:       "Submit a message to a session",
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable},
	}, (*Server).humaHandleSessionSubmit)
	cityRegister(sm, huma.Operation{
		OperationID:   "send-session-message",
		Method:        http.MethodPost,
		Path:          "/session/{id}/messages",
		Summary:       "Send a message to a session",
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable},
	}, (*Server).humaHandleSessionMessage)
	cityRegister(sm, huma.Operation{
		OperationID: "get-request-outcome",
		Method:      http.MethodGet,
		Path:        "/request/{requestId}",
		Summary:     "Get the outcome of an async session request",
		Description: "Reads the outcome of a session create, message or submit request by the request_id its 202 returned, " +
			"so a client that missed the request.result.* or request.failed event (a dropped stream, a supervisor restart) can still learn it. " +
			"after_seq is the event_cursor of the same 202: the outcome is derived from the persisted city event log after it, so it survives a supervisor restart and lasts as long as the log retains the request's events. " +
			"pending means the request is still running, or its terminal event is held until the log takes it; succeeded and failed carry the terminal event exactly as the event stream delivered it; " +
			"outcome_unknown means the server that accepted the request stopped before recording an outcome, so the operation may or may not have taken effect. " +
			"An id this city did not accept after after_seq, or whose events have aged out of the log, is a 404 request-not-found.",
		Errors: []int{http.StatusNotFound, http.StatusServiceUnavailable},
	}, (*Server).humaHandleRequestOutcome)
	cityPost(sm, "/session/{id}/stop", (*Server).humaHandleSessionStop, errorStatuses(http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusNotImplemented, http.StatusServiceUnavailable),
		describes("Stops the session's current turn with a provider-appropriate interrupt. The session stays running and keeps its conversation. "+
			"For a Codex session it also closes the session's background terminals, the processes Codex keeps running a turn's commands in after the turn is interrupted. That closes all of them, including ones started by earlier turns, such as a dev server. "+
			"An interrupt_now submit interrupts the turn the same way but leaves background terminals running. "+
			"A runtime that can interrupt the turn neither by keys nor by protocol gets nothing sent and answers 501 turn_interrupt_unsupported."))
	cityPost(sm, "/session/{id}/kill", (*Server).humaHandleSessionKill, errorStatuses(http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable),
		describes("Force-kills the session's runtime without discarding its work, as `gc session kill` does: the session is left asleep with sleep_reason killed, a configured named session's respawn circuit breaker is cleared, the controller is asked to reconcile the session, and session.stopped and worker.operation are recorded. "+
			"A closed session, or one whose runtime is already gone, reports ok. "+
			"409 live_subagents refuses a kill that would destroy live background subagents and names them; force overrides it. 409 session_starting is retryable: another holder has the runtime lease."))
	cityPost(sm, "/session/{id}/reset", (*Server).humaHandleSessionReset, errorStatuses(http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable),
		describes("Requests a fresh restart of an existing session: the session bead is kept, and the controller restarts the runtime on the next continuation epoch, so the provider starts a new conversation instead of resuming the previous one. "+
			"The call records the request and pokes the controller; it returns before the restart happens. A closed session returns 409. "+
			"For a named session, the controller clears a tripped respawn circuit breaker when its next tick consumes the restart request through the restart path. Unlike `gc session reset`, this endpoint does not clear the breaker synchronously, so the clear does not happen if the controller never processes the request, and it also does not happen when a session drain-acks in the same window: the drain-ack path consumes the restart request without touching the breaker. Use `gc session reset` when clearing the breaker is itself the goal. "+
			"This endpoint always requests an in-place restart. Unlike `gc session reset`, it never rolls back an unfinished create, so a session wedged mid-create is retried in place instead of being replaced; use `gc session reset` to roll one back."))
	cityRegister(sm, huma.Operation{
		OperationID:   "respond-session",
		Method:        http.MethodPost,
		Path:          "/session/{id}/respond",
		Summary:       "Respond to a pending interaction",
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusNotImplemented, http.StatusServiceUnavailable},
	}, (*Server).humaHandleSessionRespond)
	cityPost(sm, "/session/{id}/suspend", (*Server).humaHandleSessionSuspend, errorStatuses(http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable))
	cityPost(sm, "/session/{id}/close", (*Server).humaHandleSessionClose, errorStatuses(http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable),
		describes("Ends the session permanently: stops its runtime and closes its session bead. "+
			"Closing a session that is already closed answers 200, so a retry is safe; address it by session ID, because a closed session no longer resolves by its alias or name. "+
			"A close the store committed but could not read back in time is settled by re-reading the session: when it reads closed the answer is 200, not an error. "+
			"When it does not, the answer is 503 with detail prefix write_unconfirmed: the close may still land, so re-read the session or retry the close."))
	cityPost(sm, "/session/{id}/wake", (*Server).humaHandleSessionWake, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable),
		describes("Records an explicit wake and asks the controller to start the session; the API never starts the runtime itself. "+
			"409 wake-will-not-start means the wake is recorded but the controller will not act on it (no runnable template, a dependency-only or drained pool session, a suspended city, rig or agent, a demand-only singleton, an abandoned create, an open circuit breaker, a startup-health quarantine, or an idle latch); the detail names the remedy. "+
			"`gc session wake` applies the same rules."))
	cityPost(sm, "/session/{id}/pin", (*Server).humaHandleSessionPin, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable),
		describes("Pins the session awake (pin_awake), as gc session pin does; a configured named session not yet materialized is. 409 on a closed session."))
	cityPost(sm, "/session/{id}/unpin", (*Server).humaHandleSessionUnpin, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable),
		describes("Clears the session's pin (pin_awake), as gc session unpin does. 409 on a closed session."))
	cityPost(sm, "/session/{id}/rename", (*Server).humaHandleSessionRename, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable))
	// The producer API over a session's conversation lane (DES-O1 §11). A
	// replayed proposal answers 200 with the same body as a fresh one's 201,
	// declared so a generated client decodes both.
	proposeResultRef := sm.humaAPI.OpenAPI().Components.Schemas.Schema(
		reflect.TypeOf(ConversationOpProposeResult{}), true, "ConversationOpProposeResult")
	cityRegister(sm, huma.Operation{
		OperationID:   "propose-session-op",
		Method:        http.MethodPost,
		Path:          "/session/{id}/ops",
		Summary:       "Propose an operation to a session's conversation lane",
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable},
		Responses: map[string]*huma.Response{
			"200": {
				Description: "The same request was proposed before: the stored operation, with created false.",
				Content:     map[string]*huma.MediaType{"application/json": {Schema: proposeResultRef}},
			},
		},
		Description: "Writes a conversation-lane operation, create-if-absent under its id, which derives from the session, the principal and the client key. " +
			"201 when this request wrote it, 200 with the stored operation when the same request was proposed before. " +
			"Only kinds the city's running conversation lane host executes are accepted: those [ops] authoritative listed when the controller started. Any other, or any kind while no lane host runs, is refused 409 kind-not-enabled. " +
			"A client key that already names a different request from the same principal is refused 409 key-reuse, naming the existing op_id. " +
			"A refused request writes nothing and materializes no named session. " +
			"Watch op.changed and fact.recorded on the city event stream from the returned cursor; after reconnecting, read a non-terminal operation again with GET .../ops/{op}, whose row is authoritative.",
	}, (*Server).humaHandleConversationOpPropose)
	cityRegister(sm, huma.Operation{
		OperationID: "list-session-ops",
		Method:      http.MethodGet,
		Path:        "/session/{id}/ops",
		Summary:     "List a session's open operations",
		Errors:      []int{http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable},
		Description: "Lists the session's open operations, legacy and lane, in the conversation lane's order. A row this server cannot read is left out and named in partial_errors.",
	}, (*Server).humaHandleConversationOpList)
	cityRegister(sm, huma.Operation{
		OperationID: "get-session-op",
		Method:      http.MethodGet,
		Path:        "/session/{id}/ops/{op}",
		Summary:     "Get one of a session's operations",
		Errors:      []int{http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable},
	}, (*Server).humaHandleConversationOpGet)
	cityRegister(sm, huma.Operation{
		OperationID: "cancel-session-op",
		Method:      http.MethodPost,
		Path:        "/session/{id}/ops/{op}/cancel",
		Summary:     "Cancel one of a session's operations",
		Errors:      []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable},
		Description: "Cancels a lane operation. One that has not started closes canceled; one executing records the cancel, and its outcome says who won. A legacy operation is closed only by its legacy path and is refused 409.",
	}, (*Server).humaHandleConversationOpCancel)
	cityGet(sm, "/session/{id}/agents", (*Server).humaHandleSessionAgentList, errorStatuses(http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable))
	cityGet(sm, "/session/{id}/agents/{agentId}", (*Server).humaHandleSessionAgentGet, errorStatuses(http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable))

	// Durable session waits (session coordination-class).
	cityGet(sm, "/waits", (*Server).humaHandleWaitList, errorStatuses(http.StatusNotFound, http.StatusServiceUnavailable))
	cityGet(sm, "/wait/{id}", (*Server).humaHandleWaitGet, errorStatuses(http.StatusNotFound, http.StatusServiceUnavailable))

	// Session SSE stream.
	registerSSEStringID(sm.humaAPI, huma.Operation{
		OperationID: "stream-session",
		Method:      http.MethodGet,
		Path:        cityScopePrefix + "/session/{id}/stream",
		Summary:     "Stream session output in real time",
		Description: "Server-Sent Events stream of session transcript updates. " +
			"Streams turns (conversation format), raw messages (JSONL format), or structured messages " +
			"based on the format query parameter. Emits activity and pending events " +
			"for tool approval prompts.",
		Responses: sseResponseHeaders("GC-Session-State", "GC-Session-Status"),
	}, sessionStreamEventMap(),
		sseCityPrecheck(sm, (*Server).checkSessionStream),
		sseCityStringIDStream(sm, (*Server).streamSession))

	// Event SSE stream (per-city).
	registerSSE(sm.humaAPI, huma.Operation{
		OperationID: "stream-events",
		Method:      http.MethodGet,
		Path:        cityScopePrefix + "/events/stream",
		Summary:     "Stream city events in real time",
		Description: "Server-Sent Events stream of city events with optional workflow projections. " +
			"Supports reconnection via Last-Event-ID header or after_seq query param; omitting both starts at the current city event head.",
	}, map[string]any{
		"event": sseEventContract{
			runtimeSample: eventStreamEnvelope{},
			schemaSample:  typedEventStreamEnvelopeSchema{},
		},
		"heartbeat": HeartbeatEvent{},
	},
		sseCityPrecheck(sm, (*Server).checkEventStream),
		sseCityStream(sm, (*Server).streamEvents))

	// ExtMsg.
	cityPost(sm, "/extmsg/inbound", (*Server).humaHandleExtMsgInbound, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusServiceUnavailable))
	cityPost(sm, "/extmsg/outbound", (*Server).humaHandleExtMsgOutbound, errorStatuses(http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusServiceUnavailable))
	cityGet(sm, "/extmsg/bindings", (*Server).humaHandleExtMsgBindingList, errorStatuses(http.StatusBadRequest, http.StatusNotFound, http.StatusServiceUnavailable))
	cityPost(sm, "/extmsg/bind", (*Server).humaHandleExtMsgBind, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable))
	cityPost(sm, "/extmsg/unbind", (*Server).humaHandleExtMsgUnbind, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusServiceUnavailable))
	cityGet(sm, "/extmsg/groups", (*Server).humaHandleExtMsgGroupLookup, errorStatuses(http.StatusNotFound, http.StatusServiceUnavailable))
	cityRegister(sm, huma.Operation{
		OperationID:   "ensure-extmsg-group",
		Method:        http.MethodPost,
		Path:          "/extmsg/groups",
		Summary:       "Ensure an external messaging group exists",
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusServiceUnavailable},
	}, (*Server).humaHandleExtMsgGroupEnsure)
	cityPost(sm, "/extmsg/participants", (*Server).humaHandleExtMsgParticipantUpsert, errorStatuses(http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusServiceUnavailable))
	cityDelete(sm, "/extmsg/participants", (*Server).humaHandleExtMsgParticipantRemove, errorStatuses(http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusServiceUnavailable))
	cityGet(sm, "/extmsg/transcript", (*Server).humaHandleExtMsgTranscriptList, errorStatuses(http.StatusNotFound, http.StatusServiceUnavailable))
	cityPost(sm, "/extmsg/transcript/ack", (*Server).humaHandleExtMsgTranscriptAck, errorStatuses(http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusServiceUnavailable))
	cityGet(sm, "/extmsg/adapters", (*Server).humaHandleExtMsgAdapterList, errorStatuses(http.StatusNotFound, http.StatusServiceUnavailable))
	cityRegister(sm, huma.Operation{
		OperationID:   "register-extmsg-adapter",
		Method:        http.MethodPost,
		Path:          "/extmsg/adapters",
		Summary:       "Register an external messaging adapter",
		DefaultStatus: http.StatusCreated,
		// 409: a concurrent repeat of the same Idempotency-Key (idempotency-in-flight).
		Errors: []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable},
	}, (*Server).humaHandleExtMsgAdapterRegister)
	cityDelete(sm, "/extmsg/adapters", (*Server).humaHandleExtMsgAdapterUnregister, errorStatuses(http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusServiceUnavailable))
}

// ownedRowGuardDescription documents the generic bead write guard
// (refuseGenericWrite, architecture B) on create and update.
const ownedRowGuardDescription = "Refused with 422, nothing written: on any bead, a metadata key that is not a valid bead-metadata key, or a label that does not read back as itself from one CSV record (a comma, a quote, a newline or a carriage return); and any write to a controller-owned row, or one that would make a row controller-owned: a session row (type session, or the gc:session label), a startup-health episode or a convergence root; or a coordination row: a mail message (type message), a session wait (the gc:wait label, or a gate carrying session:<session_id>), a queued nudge (gc:nudge, gc:nudge-queue), an order's run record (order-tracking, order-run:*) or an external-messaging row (gc:extmsg-*); types and labels matched in any case and spacing. Only their owners write them (the session verbs, the controller, the convergence handler, /mail, gc session wait, the nudge queue, order dispatch, /extmsg/*). Each refusal names the offending field in errors[]."
