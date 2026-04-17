package api

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// registerCityRoutes registers per-city Huma operations at their
// user-facing scoped paths ("/v0/city/{cityName}/..."). Called from
// NewSupervisorMux after registerSupervisorRoutes.
//
// Each registered route wraps a per-city handler method through
// bindCity, which resolves the target city's Server at request time.
// The input types all embed CityScope so the spec naturally describes
// {cityName} as a path parameter.
//
// As handler groups migrate off per-city Server.registerRoutes and onto
// this function, specific Huma routes take precedence over the
// transitional legacy /v0/city/ prefix forwarder via Go 1.22+ mux
// specificity rules.
func (sm *SupervisorMux) registerCityRoutes() {
	// Status + Health
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/status",
		bindCity(sm, (*Server).humaHandleStatus))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/health",
		bindCity(sm, (*Server).humaHandleHealth))

	// City detail
	huma.Get(sm.humaAPI, "/v0/city/{cityName}",
		bindCity(sm, (*Server).humaHandleCityGet))
	huma.Patch(sm.humaAPI, "/v0/city/{cityName}",
		bindCity(sm, (*Server).humaHandleCityPatch))

	// Readiness (per-city)
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/readiness",
		bindCity(sm, (*Server).humaHandleReadiness))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/provider-readiness",
		bindCity(sm, (*Server).humaHandleProviderReadiness))

	// Config
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/config",
		bindCity(sm, (*Server).humaHandleConfigGet))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/config/explain",
		bindCity(sm, (*Server).humaHandleConfigExplain))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/config/validate",
		bindCity(sm, (*Server).humaHandleConfigValidate))

	// Agents stay on per-city Server.registerRoutes until SSE streams
	// can migrate — the {name...} catch-all would otherwise shadow the
	// SSE stream paths via the legacyCityForwarder.

	// Providers
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/providers",
		bindCity(sm, (*Server).humaHandleProviderList))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/provider/{name}",
		bindCity(sm, (*Server).humaHandleProviderGet))
	huma.Register(sm.humaAPI, huma.Operation{
		OperationID:   "create-provider",
		Method:        http.MethodPost,
		Path:          "/v0/city/{cityName}/providers",
		Summary:       "Create a provider",
		DefaultStatus: http.StatusCreated,
	}, bindCity(sm, (*Server).humaHandleProviderCreate))
	huma.Patch(sm.humaAPI, "/v0/city/{cityName}/provider/{name}",
		bindCity(sm, (*Server).humaHandleProviderUpdate))
	huma.Delete(sm.humaAPI, "/v0/city/{cityName}/provider/{name}",
		bindCity(sm, (*Server).humaHandleProviderDelete))

	// Rigs
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/rigs",
		bindCity(sm, (*Server).humaHandleRigList))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/rig/{name}",
		bindCity(sm, (*Server).humaHandleRigGet))
	huma.Register(sm.humaAPI, huma.Operation{
		OperationID:   "create-rig",
		Method:        http.MethodPost,
		Path:          "/v0/city/{cityName}/rigs",
		Summary:       "Create a rig",
		DefaultStatus: http.StatusCreated,
	}, bindCity(sm, (*Server).humaHandleRigCreate))
	huma.Patch(sm.humaAPI, "/v0/city/{cityName}/rig/{name}",
		bindCity(sm, (*Server).humaHandleRigUpdate))
	huma.Delete(sm.humaAPI, "/v0/city/{cityName}/rig/{name}",
		bindCity(sm, (*Server).humaHandleRigDelete))
	huma.Post(sm.humaAPI, "/v0/city/{cityName}/rig/{name}/{action}",
		bindCity(sm, (*Server).humaHandleRigAction))

	// Patches — agent
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/patches/agents",
		bindCity(sm, (*Server).humaHandleAgentPatchList))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/patches/agent/{name...}",
		bindCity(sm, (*Server).humaHandleAgentPatchGet))
	huma.Put(sm.humaAPI, "/v0/city/{cityName}/patches/agents",
		bindCity(sm, (*Server).humaHandleAgentPatchSet))
	huma.Delete(sm.humaAPI, "/v0/city/{cityName}/patches/agent/{name...}",
		bindCity(sm, (*Server).humaHandleAgentPatchDelete))
	// Patches — rig
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/patches/rigs",
		bindCity(sm, (*Server).humaHandleRigPatchList))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/patches/rig/{name}",
		bindCity(sm, (*Server).humaHandleRigPatchGet))
	huma.Put(sm.humaAPI, "/v0/city/{cityName}/patches/rigs",
		bindCity(sm, (*Server).humaHandleRigPatchSet))
	huma.Delete(sm.humaAPI, "/v0/city/{cityName}/patches/rig/{name}",
		bindCity(sm, (*Server).humaHandleRigPatchDelete))
	// Patches — provider
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/patches/providers",
		bindCity(sm, (*Server).humaHandleProviderPatchList))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/patches/provider/{name}",
		bindCity(sm, (*Server).humaHandleProviderPatchGet))
	huma.Put(sm.humaAPI, "/v0/city/{cityName}/patches/providers",
		bindCity(sm, (*Server).humaHandleProviderPatchSet))
	huma.Delete(sm.humaAPI, "/v0/city/{cityName}/patches/provider/{name}",
		bindCity(sm, (*Server).humaHandleProviderPatchDelete))

	// Beads
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/beads",
		bindCity(sm, (*Server).humaHandleBeadList))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/beads/graph/{rootID}",
		bindCity(sm, (*Server).humaHandleBeadGraph))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/beads/ready",
		bindCity(sm, (*Server).humaHandleBeadReady))
	huma.Register(sm.humaAPI, huma.Operation{
		OperationID:   "create-bead",
		Method:        http.MethodPost,
		Path:          "/v0/city/{cityName}/beads",
		Summary:       "Create a bead",
		DefaultStatus: http.StatusCreated,
	}, bindCity(sm, (*Server).humaHandleBeadCreate))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/bead/{id}",
		bindCity(sm, (*Server).humaHandleBeadGet))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/bead/{id}/deps",
		bindCity(sm, (*Server).humaHandleBeadDeps))
	huma.Post(sm.humaAPI, "/v0/city/{cityName}/bead/{id}/close",
		bindCity(sm, (*Server).humaHandleBeadClose))
	huma.Post(sm.humaAPI, "/v0/city/{cityName}/bead/{id}/reopen",
		bindCity(sm, (*Server).humaHandleBeadReopen))
	huma.Post(sm.humaAPI, "/v0/city/{cityName}/bead/{id}/update",
		bindCity(sm, (*Server).humaHandleBeadUpdate))
	huma.Patch(sm.humaAPI, "/v0/city/{cityName}/bead/{id}",
		bindCity(sm, (*Server).humaHandleBeadUpdate))
	huma.Post(sm.humaAPI, "/v0/city/{cityName}/bead/{id}/assign",
		bindCity(sm, (*Server).humaHandleBeadAssign))
	huma.Delete(sm.humaAPI, "/v0/city/{cityName}/bead/{id}",
		bindCity(sm, (*Server).humaHandleBeadDelete))

	// Mail
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/mail",
		bindCity(sm, (*Server).humaHandleMailList))
	huma.Register(sm.humaAPI, huma.Operation{
		OperationID:   "send-mail",
		Method:        http.MethodPost,
		Path:          "/v0/city/{cityName}/mail",
		Summary:       "Send a mail message",
		DefaultStatus: http.StatusCreated,
	}, bindCity(sm, (*Server).humaHandleMailSend))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/mail/count",
		bindCity(sm, (*Server).humaHandleMailCount))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/mail/thread/{id}",
		bindCity(sm, (*Server).humaHandleMailThread))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/mail/{id}",
		bindCity(sm, (*Server).humaHandleMailGet))
	huma.Post(sm.humaAPI, "/v0/city/{cityName}/mail/{id}/read",
		bindCity(sm, (*Server).humaHandleMailRead))
	huma.Post(sm.humaAPI, "/v0/city/{cityName}/mail/{id}/mark-unread",
		bindCity(sm, (*Server).humaHandleMailMarkUnread))
	huma.Post(sm.humaAPI, "/v0/city/{cityName}/mail/{id}/archive",
		bindCity(sm, (*Server).humaHandleMailArchive))
	huma.Register(sm.humaAPI, huma.Operation{
		OperationID:   "reply-mail",
		Method:        http.MethodPost,
		Path:          "/v0/city/{cityName}/mail/{id}/reply",
		Summary:       "Reply to a mail message",
		DefaultStatus: http.StatusCreated,
	}, bindCity(sm, (*Server).humaHandleMailReply))
	huma.Delete(sm.humaAPI, "/v0/city/{cityName}/mail/{id}",
		bindCity(sm, (*Server).humaHandleMailDelete))

	// Convoys
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/convoys",
		bindCity(sm, (*Server).humaHandleConvoyList))
	huma.Register(sm.humaAPI, huma.Operation{
		OperationID:   "create-convoy",
		Method:        http.MethodPost,
		Path:          "/v0/city/{cityName}/convoys",
		Summary:       "Create a convoy",
		DefaultStatus: http.StatusCreated,
	}, bindCity(sm, (*Server).humaHandleConvoyCreate))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/convoy/{id}",
		bindCity(sm, (*Server).humaHandleConvoyGet))
	huma.Post(sm.humaAPI, "/v0/city/{cityName}/convoy/{id}/add",
		bindCity(sm, (*Server).humaHandleConvoyAdd))
	huma.Post(sm.humaAPI, "/v0/city/{cityName}/convoy/{id}/remove",
		bindCity(sm, (*Server).humaHandleConvoyRemove))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/convoy/{id}/check",
		bindCity(sm, (*Server).humaHandleConvoyCheck))
	huma.Post(sm.humaAPI, "/v0/city/{cityName}/convoy/{id}/close",
		bindCity(sm, (*Server).humaHandleConvoyClose))
	huma.Delete(sm.humaAPI, "/v0/city/{cityName}/convoy/{id}",
		bindCity(sm, (*Server).humaHandleConvoyDelete))

	// Events (list/emit — stream stays on per-city for SSE)
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/events",
		bindCity(sm, (*Server).humaHandleEventList))
	huma.Register(sm.humaAPI, huma.Operation{
		OperationID:   "emit-event",
		Method:        http.MethodPost,
		Path:          "/v0/city/{cityName}/events",
		Summary:       "Emit an event",
		DefaultStatus: http.StatusCreated,
	}, bindCity(sm, (*Server).humaHandleEventEmit))

	// Orders
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/orders",
		bindCity(sm, (*Server).humaHandleOrderList))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/orders/check",
		bindCity(sm, (*Server).humaHandleOrderCheck))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/orders/history",
		bindCity(sm, (*Server).humaHandleOrderHistory))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/order/history/{bead_id}",
		bindCity(sm, (*Server).humaHandleOrderHistoryDetail))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/order/{name}",
		bindCity(sm, (*Server).humaHandleOrderGet))
	huma.Post(sm.humaAPI, "/v0/city/{cityName}/order/{name}/enable",
		bindCity(sm, (*Server).humaHandleOrderEnable))
	huma.Post(sm.humaAPI, "/v0/city/{cityName}/order/{name}/disable",
		bindCity(sm, (*Server).humaHandleOrderDisable))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/orders/feed",
		bindCity(sm, (*Server).humaHandleOrdersFeed))

	// Formulas
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/formulas",
		bindCity(sm, (*Server).humaHandleFormulaList))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/formulas/{name}/runs",
		bindCity(sm, (*Server).humaHandleFormulaRuns))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/formulas/{name}",
		bindCity(sm, (*Server).humaHandleFormulaDetail))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/formula/{name}",
		bindCity(sm, (*Server).humaHandleFormulaDetail))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/formulas/feed",
		bindCity(sm, (*Server).humaHandleFormulaFeed))
	// Backwards-compatible workflow aliases.
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/workflow/{workflow_id}",
		bindCity(sm, (*Server).humaHandleWorkflowGet))
	huma.Delete(sm.humaAPI, "/v0/city/{cityName}/workflow/{workflow_id}",
		bindCity(sm, (*Server).humaHandleWorkflowDelete))

	// Packs
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/packs",
		bindCity(sm, (*Server).humaHandlePackList))

	// Sling
	huma.Post(sm.humaAPI, "/v0/city/{cityName}/sling",
		bindCity(sm, (*Server).humaHandleSling))

	// Services (workspace services)
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/services",
		bindCity(sm, (*Server).humaHandleServiceList))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/service/{name}",
		bindCity(sm, (*Server).humaHandleServiceGet))
	huma.Post(sm.humaAPI, "/v0/city/{cityName}/service/{name}/restart",
		bindCity(sm, (*Server).humaHandleServiceRestart))

	// Sessions (non-stream) — SSE stream stays on per-city until SSE migration.
	huma.Register(sm.humaAPI, huma.Operation{
		OperationID:   "create-session",
		Method:        http.MethodPost,
		Path:          "/v0/city/{cityName}/sessions",
		Summary:       "Create a session",
		DefaultStatus: http.StatusAccepted,
	}, bindCity(sm, (*Server).humaHandleSessionCreate))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/sessions",
		bindCity(sm, (*Server).humaHandleSessionList))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/session/{id}",
		bindCity(sm, (*Server).humaHandleSessionGet))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/session/{id}/transcript",
		bindCity(sm, (*Server).humaHandleSessionTranscript))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/session/{id}/pending",
		bindCity(sm, (*Server).humaHandleSessionPending))
	huma.Patch(sm.humaAPI, "/v0/city/{cityName}/session/{id}",
		bindCity(sm, (*Server).humaHandleSessionPatch))
	huma.Register(sm.humaAPI, huma.Operation{
		OperationID:   "submit-session",
		Method:        http.MethodPost,
		Path:          "/v0/city/{cityName}/session/{id}/submit",
		Summary:       "Submit a message to a session",
		DefaultStatus: http.StatusAccepted,
	}, bindCity(sm, (*Server).humaHandleSessionSubmit))
	huma.Register(sm.humaAPI, huma.Operation{
		OperationID:   "send-session-message",
		Method:        http.MethodPost,
		Path:          "/v0/city/{cityName}/session/{id}/messages",
		Summary:       "Send a message to a session",
		DefaultStatus: http.StatusAccepted,
	}, bindCity(sm, (*Server).humaHandleSessionMessage))
	huma.Post(sm.humaAPI, "/v0/city/{cityName}/session/{id}/stop",
		bindCity(sm, (*Server).humaHandleSessionStop))
	huma.Post(sm.humaAPI, "/v0/city/{cityName}/session/{id}/kill",
		bindCity(sm, (*Server).humaHandleSessionKill))
	huma.Register(sm.humaAPI, huma.Operation{
		OperationID:   "respond-session",
		Method:        http.MethodPost,
		Path:          "/v0/city/{cityName}/session/{id}/respond",
		Summary:       "Respond to a pending interaction",
		DefaultStatus: http.StatusAccepted,
	}, bindCity(sm, (*Server).humaHandleSessionRespond))
	huma.Post(sm.humaAPI, "/v0/city/{cityName}/session/{id}/suspend",
		bindCity(sm, (*Server).humaHandleSessionSuspend))
	huma.Post(sm.humaAPI, "/v0/city/{cityName}/session/{id}/close",
		bindCity(sm, (*Server).humaHandleSessionClose))
	huma.Post(sm.humaAPI, "/v0/city/{cityName}/session/{id}/wake",
		bindCity(sm, (*Server).humaHandleSessionWake))
	huma.Post(sm.humaAPI, "/v0/city/{cityName}/session/{id}/rename",
		bindCity(sm, (*Server).humaHandleSessionRename))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/session/{id}/agents",
		bindCity(sm, (*Server).humaHandleSessionAgentList))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/session/{id}/agents/{agentId}",
		bindCity(sm, (*Server).humaHandleSessionAgentGet))

	// ExtMsg
	huma.Post(sm.humaAPI, "/v0/city/{cityName}/extmsg/inbound",
		bindCity(sm, (*Server).humaHandleExtMsgInbound))
	huma.Post(sm.humaAPI, "/v0/city/{cityName}/extmsg/outbound",
		bindCity(sm, (*Server).humaHandleExtMsgOutbound))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/extmsg/bindings",
		bindCity(sm, (*Server).humaHandleExtMsgBindingList))
	huma.Post(sm.humaAPI, "/v0/city/{cityName}/extmsg/bind",
		bindCity(sm, (*Server).humaHandleExtMsgBind))
	huma.Post(sm.humaAPI, "/v0/city/{cityName}/extmsg/unbind",
		bindCity(sm, (*Server).humaHandleExtMsgUnbind))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/extmsg/groups",
		bindCity(sm, (*Server).humaHandleExtMsgGroupLookup))
	huma.Register(sm.humaAPI, huma.Operation{
		OperationID:   "ensure-extmsg-group",
		Method:        http.MethodPost,
		Path:          "/v0/city/{cityName}/extmsg/groups",
		Summary:       "Ensure an external messaging group exists",
		DefaultStatus: http.StatusCreated,
	}, bindCity(sm, (*Server).humaHandleExtMsgGroupEnsure))
	huma.Post(sm.humaAPI, "/v0/city/{cityName}/extmsg/participants",
		bindCity(sm, (*Server).humaHandleExtMsgParticipantUpsert))
	huma.Delete(sm.humaAPI, "/v0/city/{cityName}/extmsg/participants",
		bindCity(sm, (*Server).humaHandleExtMsgParticipantRemove))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/extmsg/transcript",
		bindCity(sm, (*Server).humaHandleExtMsgTranscriptList))
	huma.Post(sm.humaAPI, "/v0/city/{cityName}/extmsg/transcript/ack",
		bindCity(sm, (*Server).humaHandleExtMsgTranscriptAck))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/extmsg/adapters",
		bindCity(sm, (*Server).humaHandleExtMsgAdapterList))
	huma.Register(sm.humaAPI, huma.Operation{
		OperationID:   "register-extmsg-adapter",
		Method:        http.MethodPost,
		Path:          "/v0/city/{cityName}/extmsg/adapters",
		Summary:       "Register an external messaging adapter",
		DefaultStatus: http.StatusCreated,
	}, bindCity(sm, (*Server).humaHandleExtMsgAdapterRegister))
	huma.Delete(sm.humaAPI, "/v0/city/{cityName}/extmsg/adapters",
		bindCity(sm, (*Server).humaHandleExtMsgAdapterUnregister))
}
