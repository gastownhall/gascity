package api

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
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
	cityGet(sm, "/status", (*Server).humaHandleStatus)
	cityGet(sm, "/health", (*Server).humaHandleHealth)

	// City detail.
	cityGet(sm, "", (*Server).humaHandleCityGet)
	cityPatch(sm, "", (*Server).humaHandleCityPatch)

	// Readiness (per-city).
	cityGet(sm, "/readiness", (*Server).humaHandleReadiness)
	cityGet(sm, "/provider-readiness", (*Server).humaHandleProviderReadiness)

	// Config.
	cityGet(sm, "/config", (*Server).humaHandleConfigGet)
	cityGet(sm, "/config/explain", (*Server).humaHandleConfigExplain)
	cityGet(sm, "/config/validate", (*Server).humaHandleConfigValidate)

	// Agents — read / CRUD. Agents can be addressed unqualified
	// ({base}) or rig-qualified ({dir}/{base}); there is no third
	// form, so two explicit routes cover every real case without a
	// trailing-path wildcard. The routes we register are the routes
	// we expose.
	cityGet(sm, "/agents", (*Server).humaHandleAgentList)
	cityGet(sm, "/agent/{dir}/{base}/output", (*Server).humaHandleAgentOutputQualified)
	cityGet(sm, "/agent/{base}/output", (*Server).humaHandleAgentOutput)
	cityGet(sm, "/agent/{dir}/{base}", (*Server).humaHandleAgentQualified)
	cityGet(sm, "/agent/{base}", (*Server).humaHandleAgent)
	cityRegister(sm, huma.Operation{
		OperationID:   "create-agent",
		Method:        http.MethodPost,
		Path:          "/agents",
		Summary:       "Create an agent",
		DefaultStatus: http.StatusCreated,
	}, (*Server).humaHandleAgentCreate)
	cityPatch(sm, "/agent/{dir}/{base}", (*Server).humaHandleAgentUpdateQualified)
	cityPatch(sm, "/agent/{base}", (*Server).humaHandleAgentUpdate)
	cityDelete(sm, "/agent/{dir}/{base}", (*Server).humaHandleAgentDeleteQualified)
	cityDelete(sm, "/agent/{base}", (*Server).humaHandleAgentDelete)
	cityPost(sm, "/agent/{dir}/{base}/{action}", (*Server).humaHandleAgentActionQualified)
	cityPost(sm, "/agent/{base}/{action}", (*Server).humaHandleAgentAction)

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
	cityGet(sm, "/providers", (*Server).humaHandleProviderList)
	cityGet(sm, "/providers/public", (*Server).humaHandleProviderPublicList)
	cityGet(sm, "/provider/{name}", (*Server).humaHandleProviderGet)
	cityRegister(sm, huma.Operation{
		OperationID:   "create-provider",
		Method:        http.MethodPost,
		Path:          "/providers",
		Summary:       "Create a provider",
		DefaultStatus: http.StatusCreated,
	}, (*Server).humaHandleProviderCreate)
	cityPatch(sm, "/provider/{name}", (*Server).humaHandleProviderUpdate)
	cityDelete(sm, "/provider/{name}", (*Server).humaHandleProviderDelete)

	// Rigs.
	cityGet(sm, "/rigs", (*Server).humaHandleRigList)
	cityGet(sm, "/rig/{name}", (*Server).humaHandleRigGet)
	cityRegister(sm, huma.Operation{
		OperationID:   "create-rig",
		Method:        http.MethodPost,
		Path:          "/rigs",
		Summary:       "Create a rig",
		DefaultStatus: http.StatusCreated,
	}, (*Server).humaHandleRigCreate)
	cityPatch(sm, "/rig/{name}", (*Server).humaHandleRigUpdate)
	cityDelete(sm, "/rig/{name}", (*Server).humaHandleRigDelete)
	cityPost(sm, "/rig/{name}/{action}", (*Server).humaHandleRigAction)

	// Patches — agent. Same qualified/unqualified split as /agent: two
	// explicit routes instead of a trailing-path wildcard.
	cityGet(sm, "/patches/agents", (*Server).humaHandleAgentPatchList)
	cityGet(sm, "/patches/agent/{dir}/{base}", (*Server).humaHandleAgentPatchGetQualified)
	cityGet(sm, "/patches/agent/{base}", (*Server).humaHandleAgentPatchGet)
	cityPut(sm, "/patches/agents", (*Server).humaHandleAgentPatchSet)
	cityDelete(sm, "/patches/agent/{dir}/{base}", (*Server).humaHandleAgentPatchDeleteQualified)
	cityDelete(sm, "/patches/agent/{base}", (*Server).humaHandleAgentPatchDelete)
	// Patches — rig.
	cityGet(sm, "/patches/rigs", (*Server).humaHandleRigPatchList)
	cityGet(sm, "/patches/rig/{name}", (*Server).humaHandleRigPatchGet)
	cityPut(sm, "/patches/rigs", (*Server).humaHandleRigPatchSet)
	cityDelete(sm, "/patches/rig/{name}", (*Server).humaHandleRigPatchDelete)
	// Patches — provider.
	cityGet(sm, "/patches/providers", (*Server).humaHandleProviderPatchList)
	cityGet(sm, "/patches/provider/{name}", (*Server).humaHandleProviderPatchGet)
	cityPut(sm, "/patches/providers", (*Server).humaHandleProviderPatchSet)
	cityDelete(sm, "/patches/provider/{name}", (*Server).humaHandleProviderPatchDelete)

	// Beads.
	cityGet(sm, "/beads", (*Server).humaHandleBeadList)
	cityGet(sm, "/beads/graph/{rootID}", (*Server).humaHandleBeadGraph)
	cityGet(sm, "/beads/ready", (*Server).humaHandleBeadReady)
	cityRegister(sm, huma.Operation{
		OperationID:   "create-bead",
		Method:        http.MethodPost,
		Path:          "/beads",
		Summary:       "Create a bead",
		DefaultStatus: http.StatusCreated,
	}, (*Server).humaHandleBeadCreate)
	cityGet(sm, "/bead/{id}", (*Server).humaHandleBeadGet)
	cityGet(sm, "/bead/{id}/deps", (*Server).humaHandleBeadDeps)
	cityPost(sm, "/bead/{id}/close", (*Server).humaHandleBeadClose)
	cityPost(sm, "/bead/{id}/reopen", (*Server).humaHandleBeadReopen)
	cityPost(sm, "/bead/{id}/update", (*Server).humaHandleBeadUpdate)
	cityPatch(sm, "/bead/{id}", (*Server).humaHandleBeadUpdate)
	cityPost(sm, "/bead/{id}/assign", (*Server).humaHandleBeadAssign)
	cityDelete(sm, "/bead/{id}", (*Server).humaHandleBeadDelete)

	// Mail.
	cityGet(sm, "/mail", (*Server).humaHandleMailList)
	cityRegister(sm, huma.Operation{
		OperationID:   "send-mail",
		Method:        http.MethodPost,
		Path:          "/mail",
		Summary:       "Send a mail message",
		DefaultStatus: http.StatusCreated,
	}, (*Server).humaHandleMailSend)
	cityGet(sm, "/mail/count", (*Server).humaHandleMailCount)
	cityGet(sm, "/mail/thread/{id}", (*Server).humaHandleMailThread)
	cityGet(sm, "/mail/{id}", (*Server).humaHandleMailGet)
	cityPost(sm, "/mail/{id}/read", (*Server).humaHandleMailRead)
	cityPost(sm, "/mail/{id}/mark-unread", (*Server).humaHandleMailMarkUnread)
	cityPost(sm, "/mail/{id}/archive", (*Server).humaHandleMailArchive)
	cityRegister(sm, huma.Operation{
		OperationID:   "reply-mail",
		Method:        http.MethodPost,
		Path:          "/mail/{id}/reply",
		Summary:       "Reply to a mail message",
		DefaultStatus: http.StatusCreated,
	}, (*Server).humaHandleMailReply)
	cityDelete(sm, "/mail/{id}", (*Server).humaHandleMailDelete)

	// Convoys.
	cityGet(sm, "/convoys", (*Server).humaHandleConvoyList)
	cityRegister(sm, huma.Operation{
		OperationID:   "create-convoy",
		Method:        http.MethodPost,
		Path:          "/convoys",
		Summary:       "Create a convoy",
		DefaultStatus: http.StatusCreated,
	}, (*Server).humaHandleConvoyCreate)
	cityGet(sm, "/convoy/{id}", (*Server).humaHandleConvoyGet)
	cityPost(sm, "/convoy/{id}/add", (*Server).humaHandleConvoyAdd)
	cityPost(sm, "/convoy/{id}/remove", (*Server).humaHandleConvoyRemove)
	cityGet(sm, "/convoy/{id}/check", (*Server).humaHandleConvoyCheck)
	cityPost(sm, "/convoy/{id}/close", (*Server).humaHandleConvoyClose)
	cityDelete(sm, "/convoy/{id}", (*Server).humaHandleConvoyDelete)

	// Events (list/emit — stream is a separate SSE registration below).
	cityGet(sm, "/events", (*Server).humaHandleEventList)
	cityRegister(sm, huma.Operation{
		OperationID:   "emit-event",
		Method:        http.MethodPost,
		Path:          "/events",
		Summary:       "Emit an event",
		DefaultStatus: http.StatusCreated,
	}, (*Server).humaHandleEventEmit)

	// Orders.
	cityGet(sm, "/orders", (*Server).humaHandleOrderList)
	cityGet(sm, "/orders/check", (*Server).humaHandleOrderCheck)
	cityGet(sm, "/orders/history", (*Server).humaHandleOrderHistory)
	cityGet(sm, "/order/history/{bead_id}", (*Server).humaHandleOrderHistoryDetail)
	cityGet(sm, "/order/{name}", (*Server).humaHandleOrderGet)
	cityPost(sm, "/order/{name}/enable", (*Server).humaHandleOrderEnable)
	cityPost(sm, "/order/{name}/disable", (*Server).humaHandleOrderDisable)
	cityGet(sm, "/orders/feed", (*Server).humaHandleOrdersFeed)

	// Formulas.
	cityGet(sm, "/formulas", (*Server).humaHandleFormulaList)
	cityGet(sm, "/formulas/{name}/runs", (*Server).humaHandleFormulaRuns)
	cityGet(sm, "/formulas/{name}", (*Server).humaHandleFormulaDetail)
	cityGet(sm, "/formula/{name}", (*Server).humaHandleFormulaDetail)
	cityPost(sm, "/formulas/{name}/preview", (*Server).humaHandleFormulaPreview)
	cityGet(sm, "/formulas/feed", (*Server).humaHandleFormulaFeed)
	// Backwards-compatible workflow aliases.
	cityGet(sm, "/workflow/{workflow_id}", (*Server).humaHandleWorkflowGet)
	cityDelete(sm, "/workflow/{workflow_id}", (*Server).humaHandleWorkflowDelete)

	// Packs.
	cityGet(sm, "/packs", (*Server).humaHandlePackList)

	// Sling.
	cityPost(sm, "/sling", (*Server).humaHandleSling)

	// Services (workspace services).
	cityGet(sm, "/services", (*Server).humaHandleServiceList)
	cityGet(sm, "/service/{name}", (*Server).humaHandleServiceGet)
	cityPost(sm, "/service/{name}/restart", (*Server).humaHandleServiceRestart)

	// Sessions (non-stream — stream is the SSE registration below).
	cityRegister(sm, huma.Operation{
		OperationID:   "create-session",
		Method:        http.MethodPost,
		Path:          "/sessions",
		Summary:       "Create a session",
		DefaultStatus: http.StatusAccepted,
	}, (*Server).humaHandleSessionCreate)
	cityGet(sm, "/sessions", (*Server).humaHandleSessionList)
	cityGet(sm, "/session/{id}", (*Server).humaHandleSessionGet)
	cityGet(sm, "/session/{id}/transcript", (*Server).humaHandleSessionTranscript)
	cityGet(sm, "/session/{id}/pending", (*Server).humaHandleSessionPending)
	cityPatch(sm, "/session/{id}", (*Server).humaHandleSessionPatch)
	cityRegister(sm, huma.Operation{
		OperationID:   "submit-session",
		Method:        http.MethodPost,
		Path:          "/session/{id}/submit",
		Summary:       "Submit a message to a session",
		DefaultStatus: http.StatusAccepted,
	}, (*Server).humaHandleSessionSubmit)
	cityRegister(sm, huma.Operation{
		OperationID:   "send-session-message",
		Method:        http.MethodPost,
		Path:          "/session/{id}/messages",
		Summary:       "Send a message to a session",
		DefaultStatus: http.StatusAccepted,
	}, (*Server).humaHandleSessionMessage)
	cityPost(sm, "/session/{id}/stop", (*Server).humaHandleSessionStop)
	cityPost(sm, "/session/{id}/kill", (*Server).humaHandleSessionKill)
	cityRegister(sm, huma.Operation{
		OperationID:   "respond-session",
		Method:        http.MethodPost,
		Path:          "/session/{id}/respond",
		Summary:       "Respond to a pending interaction",
		DefaultStatus: http.StatusAccepted,
	}, (*Server).humaHandleSessionRespond)
	cityPost(sm, "/session/{id}/suspend", (*Server).humaHandleSessionSuspend)
	cityPost(sm, "/session/{id}/close", (*Server).humaHandleSessionClose)
	cityPost(sm, "/session/{id}/wake", (*Server).humaHandleSessionWake)
	cityPost(sm, "/session/{id}/rename", (*Server).humaHandleSessionRename)
	cityGet(sm, "/session/{id}/agents", (*Server).humaHandleSessionAgentList)
	cityGet(sm, "/session/{id}/agents/{agentId}", (*Server).humaHandleSessionAgentGet)

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
	cityPost(sm, "/extmsg/inbound", (*Server).humaHandleExtMsgInbound)
	cityPost(sm, "/extmsg/outbound", (*Server).humaHandleExtMsgOutbound)
	cityGet(sm, "/extmsg/bindings", (*Server).humaHandleExtMsgBindingList)
	cityPost(sm, "/extmsg/bind", (*Server).humaHandleExtMsgBind)
	cityPost(sm, "/extmsg/unbind", (*Server).humaHandleExtMsgUnbind)
	cityGet(sm, "/extmsg/groups", (*Server).humaHandleExtMsgGroupLookup)
	cityRegister(sm, huma.Operation{
		OperationID:   "ensure-extmsg-group",
		Method:        http.MethodPost,
		Path:          "/extmsg/groups",
		Summary:       "Ensure an external messaging group exists",
		DefaultStatus: http.StatusCreated,
	}, (*Server).humaHandleExtMsgGroupEnsure)
	cityPost(sm, "/extmsg/participants", (*Server).humaHandleExtMsgParticipantUpsert)
	cityDelete(sm, "/extmsg/participants", (*Server).humaHandleExtMsgParticipantRemove)
	cityGet(sm, "/extmsg/transcript", (*Server).humaHandleExtMsgTranscriptList)
	cityPost(sm, "/extmsg/transcript/ack", (*Server).humaHandleExtMsgTranscriptAck)
	cityGet(sm, "/extmsg/adapters", (*Server).humaHandleExtMsgAdapterList)
	cityRegister(sm, huma.Operation{
		OperationID:   "register-extmsg-adapter",
		Method:        http.MethodPost,
		Path:          "/extmsg/adapters",
		Summary:       "Register an external messaging adapter",
		DefaultStatus: http.StatusCreated,
	}, (*Server).humaHandleExtMsgAdapterRegister)
	cityDelete(sm, "/extmsg/adapters", (*Server).humaHandleExtMsgAdapterUnregister)
}
