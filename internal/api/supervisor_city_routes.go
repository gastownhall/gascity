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
		"turn":      SessionStreamMessageEvent{},
		"message":   SessionStreamRawMessageEvent{},
		"activity":  SessionActivityEvent{},
		"pending":   runtime.PendingInteraction{},
		"heartbeat": HeartbeatEvent{},
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
	cityPost(sm, "/agent/{dir}/{base}/{action}", (*Server).humaHandleAgentActionQualified, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusNotImplemented))
	cityPost(sm, "/agent/{base}/{action}", (*Server).humaHandleAgentAction, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusNotImplemented))

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
	cityGet(sm, "/beads", (*Server).humaHandleBeadList, errorStatuses(http.StatusBadRequest, http.StatusNotFound, http.StatusServiceUnavailable))
	cityGet(sm, "/beads/graph/{rootID}", (*Server).humaHandleBeadGraph, errorStatuses(http.StatusNotFound))
	cityGet(sm, "/beads/ready", (*Server).humaHandleBeadReady, errorStatuses(http.StatusNotFound, http.StatusServiceUnavailable))
	cityRegister(sm, huma.Operation{
		OperationID:   "create-bead",
		Method:        http.MethodPost,
		Path:          "/beads",
		Summary:       "Create a bead",
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict},
	}, (*Server).humaHandleBeadCreate)
	cityGet(sm, "/bead/{id}", (*Server).humaHandleBeadGet, errorStatuses(http.StatusNotFound, http.StatusServiceUnavailable))
	cityGet(sm, "/bead/{id}/deps", (*Server).humaHandleBeadDeps, errorStatuses(http.StatusNotFound))
	cityPost(sm, "/bead/{id}/close", (*Server).humaHandleBeadClose, errorStatuses(http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict))
	cityPost(sm, "/bead/{id}/reopen", (*Server).humaHandleBeadReopen, errorStatuses(http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict))
	cityPost(sm, "/bead/{id}/update", (*Server).humaHandleBeadUpdate, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict))
	cityPatch(sm, "/bead/{id}", (*Server).humaHandleBeadUpdate, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict))
	cityPost(sm, "/bead/{id}/assign", (*Server).humaHandleBeadAssign, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict))
	cityDelete(sm, "/bead/{id}", (*Server).humaHandleBeadDelete, errorStatuses(http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict))

	// Mail. Part of the P12 error-contract slice (see Beads above): each op
	// enumerates the error statuses it can return (Huma adds auto 422/500);
	// mutations declare 403 for the CSRF/read-only middleware.
	cityGet(sm, "/mail", (*Server).humaHandleMailList, errorStatuses(http.StatusBadRequest, http.StatusNotFound, http.StatusServiceUnavailable))
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
	cityGet(sm, "/convoys", (*Server).humaHandleConvoyList, errorStatuses(http.StatusBadRequest, http.StatusNotFound, http.StatusServiceUnavailable))
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
	cityGet(sm, "/events", (*Server).humaHandleEventList, errorStatuses(http.StatusBadRequest, http.StatusNotFound))
	cityRegister(sm, huma.Operation{
		OperationID:   "emit-event",
		Method:        http.MethodPost,
		Path:          "/events",
		Summary:       "Emit an event",
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
	cityGet(sm, "/runs", (*Server).humaHandleRunsList, errorStatuses(http.StatusServiceUnavailable))
	cityGet(sm, "/runs/{run_id}", (*Server).humaHandleRunGet, errorStatuses(http.StatusNotFound, http.StatusServiceUnavailable))
	cityGet(sm, "/runs/{run_id}/steps", (*Server).humaHandleRunSteps, errorStatuses(http.StatusNotFound, http.StatusServiceUnavailable))
	cityPost(sm, "/runs/{run_id}/cancel", (*Server).humaHandleRunCancel, func(op *huma.Operation) {
		op.DefaultStatus = http.StatusAccepted
	}, errorStatuses(http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable))

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
	cityPost(sm, "/sling", (*Server).humaHandleSling, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict))

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
		Errors:        []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusServiceUnavailable},
	}, (*Server).humaHandleSessionCreate)
	// 400: invalid pagination cursor (invalid-cursor problem type).
	cityGet(sm, "/sessions", (*Server).humaHandleSessionList, errorStatuses(http.StatusBadRequest, http.StatusNotFound, http.StatusServiceUnavailable))
	cityGet(sm, "/session/{id}", (*Server).humaHandleSessionGet, errorStatuses(http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable))
	cityGet(sm, "/session/{id}/transcript", (*Server).humaHandleSessionTranscript, errorStatuses(http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable))
	cityGet(sm, "/session/{id}/pending", (*Server).humaHandleSessionPending, errorStatuses(http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable))
	cityGet(sm, "/pending", (*Server).humaHandleCityPending, errorStatuses(http.StatusNotFound, http.StatusServiceUnavailable))
	cityPatch(sm, "/session/{id}", (*Server).humaHandleSessionPatch, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable))
	cityPost(sm, "/session/{id}/permission-mode", (*Server).humaHandleSessionPermissionMode, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusNotImplemented, http.StatusServiceUnavailable))
	cityRegister(sm, huma.Operation{
		OperationID:   "submit-session",
		Method:        http.MethodPost,
		Path:          "/session/{id}/submit",
		Summary:       "Submit a message to a session",
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusServiceUnavailable},
	}, (*Server).humaHandleSessionSubmit)
	cityRegister(sm, huma.Operation{
		OperationID:   "send-session-message",
		Method:        http.MethodPost,
		Path:          "/session/{id}/messages",
		Summary:       "Send a message to a session",
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusServiceUnavailable},
	}, (*Server).humaHandleSessionMessage)
	cityPost(sm, "/session/{id}/stop", (*Server).humaHandleSessionStop, errorStatuses(http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable))
	cityPost(sm, "/session/{id}/kill", (*Server).humaHandleSessionKill, errorStatuses(http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable))
	cityRegister(sm, huma.Operation{
		OperationID:   "respond-session",
		Method:        http.MethodPost,
		Path:          "/session/{id}/respond",
		Summary:       "Respond to a pending interaction",
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusNotImplemented, http.StatusServiceUnavailable},
	}, (*Server).humaHandleSessionRespond)
	cityPost(sm, "/session/{id}/suspend", (*Server).humaHandleSessionSuspend, errorStatuses(http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable))
	cityPost(sm, "/session/{id}/close", (*Server).humaHandleSessionClose, errorStatuses(http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable))
	cityPost(sm, "/session/{id}/wake", (*Server).humaHandleSessionWake, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable))
	cityPost(sm, "/session/{id}/rename", (*Server).humaHandleSessionRename, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable))
	cityGet(sm, "/session/{id}/agents", (*Server).humaHandleSessionAgentList, errorStatuses(http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable))
	cityGet(sm, "/session/{id}/agents/{agentId}", (*Server).humaHandleSessionAgentGet, errorStatuses(http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable))

	// Durable session waits (session coordination-class).
	cityGet(sm, "/waits", (*Server).humaHandleWaitList, errorStatuses(http.StatusNotFound, http.StatusServiceUnavailable))
	cityGet(sm, "/wait/{id}", (*Server).humaHandleWaitGet, errorStatuses(http.StatusNotFound, http.StatusServiceUnavailable))

	// Session SSE stream.
	registerSSE(sm.humaAPI, huma.Operation{
		OperationID: "stream-session",
		Method:      http.MethodGet,
		Path:        cityScopePrefix + "/session/{id}/stream",
		Summary:     "Stream session output in real time",
		Description: "Server-Sent Events stream of session transcript updates. " +
			"Streams turns (conversation format) or raw messages (JSONL format) " +
			"based on the format query parameter. Emits activity and pending events " +
			"for tool approval prompts.",
		Responses: sseResponseHeaders("GC-Session-State", "GC-Session-Status"),
	}, sessionStreamEventMap(),
		sseCityPrecheck(sm, (*Server).checkSessionStream),
		sseCityStream(sm, (*Server).streamSession))

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
