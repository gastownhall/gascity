package api

// Shared Huma input/output types for the Gas City API.
//
// These types define the API contract: wire format, validation, and OpenAPI
// documentation. They are the source of truth for the auto-generated OpenAPI
// 3.1 spec at /openapi.json.

//go:generate sh -c "go run ../../cmd/genspec > openapi.json"

import (
	"errors"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/extmsg"
)

// --- Shared input mixins ---

// BlockingParam is an embeddable input mixin for long-polling endpoints.
// When index is provided, the handler blocks until a newer event arrives.
// Index is a string rather than uint64 because Huma doesn't support pointer
// query params, and we need to distinguish "not provided" from "0" (which
// means "wait for the first event").
type BlockingParam struct {
	Index string `query:"index" doc:"Event sequence number; when provided, blocks until a newer event arrives." required:"false"`
	Wait  string `query:"wait" doc:"How long to block waiting for changes (Go duration string, e.g. 30s). Default 30s, max 2m." required:"false"`
}

// toBlockingParams converts to the internal BlockingParams type.
func (bp *BlockingParam) toBlockingParams() BlockingParams {
	result := BlockingParams{Wait: defaultWait}
	if bp.Index != "" {
		result.Index, _ = strconv.ParseUint(bp.Index, 10, 64)
		result.HasIndex = true
	}
	if bp.Wait != "" {
		if d, err := time.ParseDuration(bp.Wait); err == nil && d > 0 {
			result.Wait = d
		}
	}
	if result.Wait > maxWait {
		result.Wait = maxWait
	}
	return result
}

// WaitParam is an embeddable input mixin for blocking read endpoints.
// Handlers that support ?wait=... should embed this type.
type WaitParam struct {
	Wait string `query:"wait" doc:"Block until state changes, then return. Value is a Go duration string (e.g. 30s, 1m)." required:"false"`
}

// PaginationParam is an embeddable input mixin for paginated list endpoints.
type PaginationParam struct {
	Cursor string `query:"cursor" doc:"Pagination cursor from a previous response's next_cursor field." required:"false"`
	Limit  int    `query:"limit" doc:"Maximum number of results to return." required:"false"`
}

// --- Shared output types ---

// ListBody is the JSON body for list responses. It wraps items with total
// count and optional pagination cursor.
type ListBody[T any] struct {
	Items      []T    `json:"items" doc:"The list of items."`
	Total      int    `json:"total" doc:"Total number of items matching the query."`
	NextCursor string `json:"next_cursor,omitempty" doc:"Cursor for the next page of results."`
}

// ListOutput is a generic output type for list endpoints. It sets the
// X-GC-Index header and returns items in the standard list envelope.
type ListOutput[T any] struct {
	Index uint64 `header:"X-GC-Index" doc:"Latest event sequence number."`
	Body  ListBody[T]
}

// IndexOutput is a generic output type for single-resource endpoints
// that include the X-GC-Index header.
type IndexOutput[T any] struct {
	Index uint64 `header:"X-GC-Index" doc:"Latest event sequence number."`
	Body  T
}

// --- Health / Status output types ---

// HealthOutput is the response body for GET /health.
type HealthOutput struct {
	Body struct {
		Status    string `json:"status" doc:"Health status." example:"ok"`
		Version   string `json:"version,omitempty" doc:"Server version."`
		City      string `json:"city,omitempty" doc:"City name."`
		UptimeSec int    `json:"uptime_sec" doc:"Server uptime in seconds."`
	}
}

// StatusAgentCounts holds agent state counts for the status endpoint.
type StatusAgentCounts struct {
	Total       int `json:"total" doc:"Total number of agents."`
	Running     int `json:"running" doc:"Number of running agents."`
	Suspended   int `json:"suspended" doc:"Number of suspended agents."`
	Quarantined int `json:"quarantined" doc:"Number of quarantined agents."`
}

// StatusRigCounts holds rig state counts for the status endpoint.
type StatusRigCounts struct {
	Total     int `json:"total" doc:"Total number of rigs."`
	Suspended int `json:"suspended" doc:"Number of suspended rigs."`
}

// StatusWorkCounts holds work item counts for the status endpoint.
type StatusWorkCounts struct {
	InProgress int `json:"in_progress" doc:"Number of in-progress work items."`
	Ready      int `json:"ready" doc:"Number of ready work items."`
	Open       int `json:"open" doc:"Number of open work items."`
}

// StatusMailCounts holds mail counts for the status endpoint.
type StatusMailCounts struct {
	Unread int `json:"unread" doc:"Number of unread messages."`
	Total  int `json:"total" doc:"Total number of messages."`
}

// --- Error helpers ---

// mutationError converts a domain error from a create/update/delete operation
// into the appropriate Huma HTTP error.
//
// Uses typed sentinel errors from the configedit package (ErrNotFound,
// ErrAlreadyExists, ErrPackDerived, ErrValidation) via errors.Is instead of
// fragile strings.Contains matching. New domain errors should be added as
// sentinels in their originating package and matched here.
func mutationError(err error) error {
	msg := err.Error()
	switch {
	case errors.Is(err, configedit.ErrNotFound):
		return huma.Error404NotFound(msg)
	case errors.Is(err, configedit.ErrAlreadyExists):
		return huma.Error409Conflict(msg)
	case errors.Is(err, configedit.ErrPackDerived):
		return huma.Error409Conflict(msg)
	case errors.Is(err, configedit.ErrValidation):
		return huma.Error400BadRequest(msg)
	default:
		return huma.Error500InternalServerError(msg)
	}
}

// errMutationsNotSupported is returned when the state doesn't implement StateMutator.
var errMutationsNotSupported = huma.Error501NotImplemented("mutations not supported")

// --- Simple response types ---

// OKResponse is a simple success response body.
type OKResponse struct {
	Body struct {
		Status string `json:"status" doc:"Operation result." example:"ok"`
	}
}

// CreatedResponse is a success response for create operations.
type CreatedResponse struct {
	Body struct {
		Status   string `json:"status" doc:"Operation result." example:"created"`
		Agent    string `json:"agent,omitempty" doc:"Created resource name."`
		Rig      string `json:"rig,omitempty" doc:"Created resource name."`
		Provider string `json:"provider,omitempty" doc:"Created resource name."`
	}
}

// --- Agent types ---

// AgentListInput is the Huma input for GET /v0/agents (still per-city until SSE migration).
type AgentListInput struct {
	BlockingParam
	Pool    string `query:"pool" required:"false" doc:"Filter by pool name."`
	Rig     string `query:"rig" required:"false" doc:"Filter by rig name."`
	Running string `query:"running" required:"false" doc:"Filter by running state (true/false)."`
	Peek    string `query:"peek" required:"false" doc:"Include last output preview (true/false)."`
}

// AgentGetInput is the Huma input for GET /v0/agent/{name}.
type AgentGetInput struct {
	Name string `path:"name" doc:"Agent qualified name."`
}

// AgentCreateInput is the Huma input for POST /v0/agents.
type AgentCreateInput struct {
	Body struct {
		Name     string `json:"name" doc:"Agent name." minLength:"1" example:"deacon-1"`
		Dir      string `json:"dir,omitempty" doc:"Working directory (rig name)."`
		Provider string `json:"provider" doc:"Provider name." minLength:"1" example:"claude"`
		Scope    string `json:"scope,omitempty" doc:"Agent scope."`
	}
}

// AgentUpdateInput is the Huma input for PATCH /v0/agent/{name}.
type AgentUpdateInput struct {
	Name string `path:"name" doc:"Agent qualified name."`
	Body struct {
		Provider  string `json:"provider,omitempty" doc:"Provider name."`
		Scope     string `json:"scope,omitempty" doc:"Agent scope."`
		Suspended *bool  `json:"suspended,omitempty" doc:"Whether agent is suspended."`
	}
}

// AgentDeleteInput is the Huma input for DELETE /v0/agent/{name}.
type AgentDeleteInput struct {
	Name string `path:"name" doc:"Agent qualified name."`
}

// AgentActionInput is the Huma input for POST /v0/agent/{name} (actions).
type AgentActionInput struct {
	Name string `path:"name" doc:"Agent qualified name with action suffix (e.g. myagent/suspend)."`
}

// --- Provider types ---

// ProviderListInput is the Huma input for GET /v0/city/{cityName}/providers.
type ProviderListInput struct {
	CityScope
	View string `query:"view" required:"false" doc:"Response view: 'public' omits command/args/env details."`
}

// ProviderGetInput is the Huma input for GET /v0/city/{cityName}/provider/{name}.
type ProviderGetInput struct {
	CityScope
	Name string `path:"name" doc:"Provider name."`
}

// ProviderCreateInput is the Huma input for POST /v0/city/{cityName}/providers.
type ProviderCreateInput struct {
	CityScope
	Body struct {
		Name         string            `json:"name" doc:"Provider name." minLength:"1"`
		DisplayName  string            `json:"display_name,omitempty" doc:"Human-readable display name."`
		Command      string            `json:"command" doc:"Provider command binary." minLength:"1"`
		Args         []string          `json:"args,omitempty" doc:"Command arguments."`
		PromptMode   string            `json:"prompt_mode,omitempty" doc:"Prompt delivery mode."`
		PromptFlag   string            `json:"prompt_flag,omitempty" doc:"Flag for prompt delivery."`
		ReadyDelayMs int               `json:"ready_delay_ms,omitempty" doc:"Milliseconds to wait before probing readiness."`
		Env          map[string]string `json:"env,omitempty" doc:"Environment variables."`
	}
}

// ProviderUpdateInput is the Huma input for PATCH /v0/city/{cityName}/provider/{name}.
type ProviderUpdateInput struct {
	CityScope
	Name string `path:"name" doc:"Provider name."`
	Body struct {
		DisplayName  *string           `json:"display_name,omitempty" doc:"Human-readable display name."`
		Command      *string           `json:"command,omitempty" doc:"Provider command binary."`
		Args         []string          `json:"args,omitempty" doc:"Command arguments."`
		PromptMode   *string           `json:"prompt_mode,omitempty" doc:"Prompt delivery mode."`
		PromptFlag   *string           `json:"prompt_flag,omitempty" doc:"Flag for prompt delivery."`
		ReadyDelayMs *int              `json:"ready_delay_ms,omitempty" doc:"Milliseconds to wait before probing readiness."`
		Env          map[string]string `json:"env,omitempty" doc:"Environment variables."`
	}
}

// ProviderDeleteInput is the Huma input for DELETE /v0/city/{cityName}/provider/{name}.
type ProviderDeleteInput struct {
	CityScope
	Name string `path:"name" doc:"Provider name."`
}

// --- Rig types ---

// RigListInput is the Huma input for GET /v0/city/{cityName}/rigs.
type RigListInput struct {
	CityScope
	BlockingParam
	Git string `query:"git" required:"false" doc:"Include git status (true/false)."`
}

// RigGetInput is the Huma input for GET /v0/city/{cityName}/rig/{name}.
type RigGetInput struct {
	CityScope
	Name string `path:"name" doc:"Rig name."`
	Git  string `query:"git" required:"false" doc:"Include git status (true/false)."`
}

// RigCreateInput is the Huma input for POST /v0/city/{cityName}/rigs.
type RigCreateInput struct {
	CityScope
	Body struct {
		Name   string `json:"name" doc:"Rig name." minLength:"1"`
		Path   string `json:"path" doc:"Filesystem path." minLength:"1"`
		Prefix string `json:"prefix,omitempty" doc:"Session name prefix."`
	}
}

// RigUpdateInput is the Huma input for PATCH /v0/city/{cityName}/rig/{name}.
type RigUpdateInput struct {
	CityScope
	Name string `path:"name" doc:"Rig name."`
	Body struct {
		Path      string `json:"path,omitempty" doc:"Filesystem path."`
		Prefix    string `json:"prefix,omitempty" doc:"Session name prefix."`
		Suspended *bool  `json:"suspended,omitempty" doc:"Whether rig is suspended."`
	}
}

// RigDeleteInput is the Huma input for DELETE /v0/city/{cityName}/rig/{name}.
type RigDeleteInput struct {
	CityScope
	Name string `path:"name" doc:"Rig name."`
}

// RigActionInput is the Huma input for POST /v0/city/{cityName}/rig/{name}/{action}.
type RigActionInput struct {
	CityScope
	Name   string `path:"name" doc:"Rig name."`
	Action string `path:"action" doc:"Action to perform (suspend, resume, restart)."`
}

// RigActionResponse is the response for rig actions (suspend/resume/restart).
type RigActionResponse struct {
	Body RigActionBody
}

// RigActionBody is the JSON body for rig action responses.
type RigActionBody struct {
	Status string   `json:"status" doc:"Operation result (ok, partial, failed)." example:"ok"`
	Action string   `json:"action" doc:"Action that was performed."`
	Rig    string   `json:"rig" doc:"Rig name."`
	Killed []string `json:"killed,omitempty" doc:"Agents that were killed (restart only)."`
	Failed []string `json:"failed,omitempty" doc:"Agents that failed to stop (restart only)."`
}

// --- Patch types ---

// AgentPatchListInput is the Huma input for GET /v0/city/{cityName}/patches/agents.
type AgentPatchListInput struct {
	CityScope
}

// AgentPatchGetInput is the Huma input for GET /v0/city/{cityName}/patches/agent/{name}.
type AgentPatchGetInput struct {
	CityScope
	Name string `path:"name" doc:"Agent patch qualified name."`
}

// AgentPatchSetInput is the Huma input for PUT /v0/city/{cityName}/patches/agents.
type AgentPatchSetInput struct {
	CityScope
	Body struct {
		Dir       string            `json:"dir,omitempty" doc:"Agent directory scope."`
		Name      string            `json:"name,omitempty" doc:"Agent name."`
		WorkDir   *string           `json:"work_dir,omitempty" doc:"Override session working directory."`
		Scope     *string           `json:"scope,omitempty" doc:"Override agent scope."`
		Suspended *bool             `json:"suspended,omitempty" doc:"Override suspended state."`
		Env       map[string]string `json:"env,omitempty" doc:"Override environment variables."`
	}
}

// AgentPatchDeleteInput is the Huma input for DELETE /v0/city/{cityName}/patches/agent/{name}.
type AgentPatchDeleteInput struct {
	CityScope
	Name string `path:"name" doc:"Agent patch qualified name."`
}

// RigPatchListInput is the Huma input for GET /v0/city/{cityName}/patches/rigs.
type RigPatchListInput struct {
	CityScope
}

// RigPatchGetInput is the Huma input for GET /v0/city/{cityName}/patches/rig/{name}.
type RigPatchGetInput struct {
	CityScope
	Name string `path:"name" doc:"Rig patch name."`
}

// RigPatchSetInput is the Huma input for PUT /v0/city/{cityName}/patches/rigs.
type RigPatchSetInput struct {
	CityScope
	Body struct {
		Name      string `json:"name,omitempty" doc:"Rig name."`
		Path      *string `json:"path,omitempty" doc:"Override filesystem path."`
		Prefix    *string `json:"prefix,omitempty" doc:"Override bead ID prefix."`
		Suspended *bool   `json:"suspended,omitempty" doc:"Override suspended state."`
	}
}

// RigPatchDeleteInput is the Huma input for DELETE /v0/city/{cityName}/patches/rig/{name}.
type RigPatchDeleteInput struct {
	CityScope
	Name string `path:"name" doc:"Rig patch name."`
}

// ProviderPatchListInput is the Huma input for GET /v0/city/{cityName}/patches/providers.
type ProviderPatchListInput struct {
	CityScope
}

// ProviderPatchGetInput is the Huma input for GET /v0/city/{cityName}/patches/provider/{name}.
type ProviderPatchGetInput struct {
	CityScope
	Name string `path:"name" doc:"Provider patch name."`
}

// ProviderPatchSetInput is the Huma input for PUT /v0/city/{cityName}/patches/providers.
type ProviderPatchSetInput struct {
	CityScope
	Body struct {
		Name         string            `json:"name,omitempty" doc:"Provider name."`
		Command      *string           `json:"command,omitempty" doc:"Override command binary."`
		Args         []string          `json:"args,omitempty" doc:"Override command arguments."`
		PromptMode   *string           `json:"prompt_mode,omitempty" doc:"Override prompt delivery mode."`
		PromptFlag   *string           `json:"prompt_flag,omitempty" doc:"Override prompt flag."`
		ReadyDelayMs *int              `json:"ready_delay_ms,omitempty" doc:"Override ready delay in milliseconds."`
		Env          map[string]string `json:"env,omitempty" doc:"Override environment variables."`
	}
}

// ProviderPatchDeleteInput is the Huma input for DELETE /v0/city/{cityName}/patches/provider/{name}.
type ProviderPatchDeleteInput struct {
	CityScope
	Name string `path:"name" doc:"Provider patch name."`
}

// --- Event types ---

// EventListInput is the Huma input for GET /v0/city/{cityName}/events.
type EventListInput struct {
	CityScope
	BlockingParam
	PaginationParam
	Type  string `query:"type" required:"false" doc:"Filter by event type."`
	Actor string `query:"actor" required:"false" doc:"Filter by actor."`
	Since string `query:"since" required:"false" doc:"Filter events since duration ago (Go duration string, e.g. 5m)."`
}

// EventEmitInput is the Huma input for POST /v0/city/{cityName}/events.
type EventEmitInput struct {
	CityScope
	Body struct {
		Type    string `json:"type" doc:"Event type." minLength:"1"`
		Actor   string `json:"actor" doc:"Actor that produced the event." minLength:"1"`
		Subject string `json:"subject,omitempty" doc:"Event subject."`
		Message string `json:"message,omitempty" doc:"Event message."`
	}
}

// EventEmitOutput is the response body for POST /v0/events.
type EventEmitOutput struct {
	Body struct {
		Status string `json:"status" doc:"Operation result." example:"recorded"`
	}
}

// EventStreamInput is the Huma input for GET /v0/events/stream.
type EventStreamInput struct {
	AfterSeq    string `query:"after_seq" required:"false" doc:"Reconnect position: only deliver events after this sequence number."`
	LastEventID string `header:"Last-Event-ID" required:"false" doc:"SSE reconnect position from the last received event ID."`
}

// HeartbeatEvent is an empty event emitted periodically on SSE streams to keep
// the connection alive through proxies. Clients can ignore this event type.
type HeartbeatEvent struct {
	Timestamp string `json:"timestamp" doc:"ISO 8601 timestamp when the heartbeat was sent."`
}

// SessionActivityEvent reports the current activity state of a session stream.
// Emitted whenever the session transitions between idle and in-turn states.
type SessionActivityEvent struct {
	Activity string `json:"activity" doc:"Session activity state: 'idle' or 'in-turn'." example:"idle"`
}

// resolveAfterSeq returns the reconnect position from Last-Event-ID or after_seq.
func (e *EventStreamInput) resolveAfterSeq() uint64 {
	if e.LastEventID != "" {
		if n, err := strconv.ParseUint(e.LastEventID, 10, 64); err == nil {
			return n
		}
	}
	if e.AfterSeq != "" {
		if n, err := strconv.ParseUint(e.AfterSeq, 10, 64); err == nil {
			return n
		}
	}
	return 0
}

// --- Order types ---

// OrdersFeedInput is the Huma input for GET /v0/city/{cityName}/orders/feed.
type OrdersFeedInput struct {
	CityScope
	ScopeKind string `query:"scope_kind" required:"false" doc:"Scope kind (city or rig)."`
	ScopeRef  string `query:"scope_ref" required:"false" doc:"Scope reference."`
	Limit     string `query:"limit" required:"false" doc:"Maximum number of feed items to return."`
}

// OrderListInput is the Huma input for GET /v0/city/{cityName}/orders.
type OrderListInput struct {
	CityScope
}

// OrderGetInput is the Huma input for GET /v0/city/{cityName}/order/{name}.
type OrderGetInput struct {
	CityScope
	Name string `path:"name" doc:"Order name or scoped name."`
}

// OrderCheckInput is the Huma input for GET /v0/city/{cityName}/orders/check.
type OrderCheckInput struct {
	CityScope
}

// OrderHistoryInput is the Huma input for GET /v0/city/{cityName}/orders/history.
type OrderHistoryInput struct {
	CityScope
	ScopedName string `query:"scoped_name" required:"false" doc:"Scoped order name."`
	Limit      string `query:"limit" required:"false" doc:"Maximum number of history entries."`
	Before     string `query:"before" required:"false" doc:"Return entries before this RFC3339 timestamp."`
}

// OrderHistoryDetailInput is the Huma input for GET /v0/city/{cityName}/order/history/{bead_id}.
type OrderHistoryDetailInput struct {
	CityScope
	BeadID string `path:"bead_id" doc:"Bead ID for the order run."`
}

// OrderEnableInput is the Huma input for POST /v0/city/{cityName}/order/{name}/enable.
type OrderEnableInput struct {
	CityScope
	Name string `path:"name" doc:"Order name or scoped name."`
}

// OrderDisableInput is the Huma input for POST /v0/city/{cityName}/order/{name}/disable.
type OrderDisableInput struct {
	CityScope
	Name string `path:"name" doc:"Order name or scoped name."`
}

// --- Formula types ---

// FormulaFeedInput is the Huma input for GET /v0/city/{cityName}/formulas/feed.
type FormulaFeedInput struct {
	CityScope
	ScopeKind string `query:"scope_kind" required:"false" doc:"Scope kind (city or rig)."`
	ScopeRef  string `query:"scope_ref" required:"false" doc:"Scope reference."`
	Limit     string `query:"limit" required:"false" doc:"Maximum number of feed items to return."`
}

// FormulaListInput is the Huma input for GET /v0/city/{cityName}/formulas.
type FormulaListInput struct {
	CityScope
	ScopeKind string `query:"scope_kind" required:"false" doc:"Scope kind (city or rig)."`
	ScopeRef  string `query:"scope_ref" required:"false" doc:"Scope reference."`
}

// FormulaRunsInput is the Huma input for GET /v0/city/{cityName}/formulas/{name}/runs.
type FormulaRunsInput struct {
	CityScope
	Name      string `path:"name" minLength:"1" pattern:"\\S" doc:"Formula name."`
	ScopeKind string `query:"scope_kind" required:"false" doc:"Scope kind (city or rig)."`
	ScopeRef  string `query:"scope_ref" required:"false" doc:"Scope reference."`
	Limit     string `query:"limit" required:"false" doc:"Maximum number of recent runs to return."`
}

// --- Pack types ---

// PackListInput is the Huma input for GET /v0/city/{cityName}/packs.
type PackListInput struct {
	CityScope
}

// --- Sling types ---

// SlingInput is the Huma input for POST /v0/city/{cityName}/sling.
type SlingInput struct {
	CityScope
	Body struct {
		Rig            string            `json:"rig,omitempty" doc:"Rig name."`
		Target         string            `json:"target,omitempty" doc:"Target agent or pool."`
		Bead           string            `json:"bead,omitempty" doc:"Bead ID to sling."`
		Formula        string            `json:"formula,omitempty" doc:"Formula name for workflow launch."`
		AttachedBeadID string            `json:"attached_bead_id,omitempty" doc:"Bead ID to attach a formula to."`
		Title          string            `json:"title,omitempty" doc:"Workflow title."`
		Vars           map[string]string `json:"vars,omitempty" doc:"Formula variables."`
		ScopeKind      string            `json:"scope_kind,omitempty" doc:"Scope kind (city or rig)."`
		ScopeRef       string            `json:"scope_ref,omitempty" doc:"Scope reference."`
	}
}

// --- Bead types ---

// BeadListInput is the Huma input for GET /v0/city/{cityName}/beads.
type BeadListInput struct {
	CityScope
	BlockingParam
	PaginationParam
	Status   string `query:"status" required:"false" doc:"Filter by bead status."`
	Type     string `query:"type" required:"false" doc:"Filter by bead type."`
	Label    string `query:"label" required:"false" doc:"Filter by label."`
	Assignee string `query:"assignee" required:"false" doc:"Filter by assignee."`
	Rig      string `query:"rig" required:"false" doc:"Filter by rig."`
}

// BeadReadyInput is the Huma input for GET /v0/city/{cityName}/beads/ready.
type BeadReadyInput struct {
	CityScope
	BlockingParam
}

// BeadGraphInput is the Huma input for GET /v0/city/{cityName}/beads/graph/{rootID}.
type BeadGraphInput struct {
	CityScope
	RootID string `path:"rootID" doc:"Root bead ID for the graph."`
}

// BeadGetInput is the Huma input for GET /v0/city/{cityName}/bead/{id}.
type BeadGetInput struct {
	CityScope
	ID string `path:"id" doc:"Bead ID."`
}

// BeadDepsInput is the Huma input for GET /v0/city/{cityName}/bead/{id}/deps.
type BeadDepsInput struct {
	CityScope
	ID string `path:"id" doc:"Bead ID."`
}

// BeadCreateInput is the Huma input for POST /v0/city/{cityName}/beads.
type BeadCreateInput struct {
	CityScope
	IdempotencyKey string `header:"Idempotency-Key" required:"false" doc:"Idempotency key for safe retries."`
	Body           struct {
		Rig         string   `json:"rig,omitempty" doc:"Rig name."`
		Title       string   `json:"title" doc:"Bead title." minLength:"1"`
		Type        string   `json:"type,omitempty" doc:"Bead type."`
		Priority    *int     `json:"priority,omitempty" doc:"Bead priority."`
		Assignee    string   `json:"assignee,omitempty" doc:"Assigned agent."`
		Description string   `json:"description,omitempty" doc:"Bead description."`
		Labels      []string `json:"labels,omitempty" doc:"Bead labels."`
	}
}

// BeadCloseInput is the Huma input for POST /v0/city/{cityName}/bead/{id}/close.
type BeadCloseInput struct {
	CityScope
	ID string `path:"id" doc:"Bead ID."`
}

// BeadReopenInput is the Huma input for POST /v0/city/{cityName}/bead/{id}/reopen.
type BeadReopenInput struct {
	CityScope
	ID string `path:"id" doc:"Bead ID."`
}

// BeadUpdateInput is the Huma input for POST /v0/city/{cityName}/bead/{id}/update and PATCH /v0/city/{cityName}/bead/{id}.
type BeadUpdateInput struct {
	CityScope
	ID   string `path:"id" doc:"Bead ID."`
	Body beadUpdateBody
}

// beadUpdateBody is the request body for bead update/patch endpoints.
type beadUpdateBody struct {
	Title        *string           `json:"title,omitempty" doc:"Bead title."`
	Status       *string           `json:"status,omitempty" doc:"Bead status."`
	Type         *string           `json:"type,omitempty" doc:"Bead type."`
	Priority     *int              `json:"priority,omitempty" doc:"Bead priority."`
	Assignee     *string           `json:"assignee,omitempty" doc:"Assigned agent."`
	Description  *string           `json:"description,omitempty" doc:"Bead description."`
	Labels       []string          `json:"labels,omitempty" doc:"Bead labels."`
	RemoveLabels []string          `json:"remove_labels,omitempty" doc:"Labels to remove."`
	Metadata     map[string]string `json:"metadata,omitempty" doc:"Metadata key-value pairs to set."`
}

// BeadAssignInput is the Huma input for POST /v0/city/{cityName}/bead/{id}/assign.
type BeadAssignInput struct {
	CityScope
	ID   string `path:"id" doc:"Bead ID."`
	Body struct {
		Assignee string `json:"assignee,omitempty" doc:"Assignee name."`
	}
}

// BeadDeleteInput is the Huma input for DELETE /v0/city/{cityName}/bead/{id}.
type BeadDeleteInput struct {
	CityScope
	ID string `path:"id" doc:"Bead ID."`
}

// --- Mail types ---

// MailListInput is the Huma input for GET /v0/city/{cityName}/mail.
type MailListInput struct {
	CityScope
	BlockingParam
	PaginationParam
	Agent  string `query:"agent" required:"false" doc:"Filter by agent name."`
	Status string `query:"status" required:"false" doc:"Filter by status (unread, all)."`
	Rig    string `query:"rig" required:"false" doc:"Filter by rig name."`
}

// MailGetInput is the Huma input for GET /v0/city/{cityName}/mail/{id}.
type MailGetInput struct {
	CityScope
	ID  string `path:"id" doc:"Message ID."`
	Rig string `query:"rig" required:"false" doc:"Rig hint for O(1) lookup."`
}

// MailSendInput is the Huma input for POST /v0/city/{cityName}/mail.
type MailSendInput struct {
	CityScope
	IdempotencyKey string `header:"Idempotency-Key" required:"false" doc:"Idempotency key for safe retries."`
	Body           struct {
		Rig     string `json:"rig,omitempty" doc:"Rig name."`
		From    string `json:"from,omitempty" doc:"Sender name."`
		To      string `json:"to" doc:"Recipient name." minLength:"1"`
		Subject string `json:"subject" doc:"Message subject." minLength:"1"`
		Body    string `json:"body,omitempty" doc:"Message body."`
	}
}

// MailReadInput is the Huma input for POST /v0/city/{cityName}/mail/{id}/read.
type MailReadInput struct {
	CityScope
	ID  string `path:"id" doc:"Message ID."`
	Rig string `query:"rig" required:"false" doc:"Rig hint."`
}

// MailMarkUnreadInput is the Huma input for POST /v0/city/{cityName}/mail/{id}/mark-unread.
type MailMarkUnreadInput struct {
	CityScope
	ID  string `path:"id" doc:"Message ID."`
	Rig string `query:"rig" required:"false" doc:"Rig hint."`
}

// MailArchiveInput is the Huma input for POST /v0/city/{cityName}/mail/{id}/archive.
type MailArchiveInput struct {
	CityScope
	ID  string `path:"id" doc:"Message ID."`
	Rig string `query:"rig" required:"false" doc:"Rig hint."`
}

// MailReplyInput is the Huma input for POST /v0/city/{cityName}/mail/{id}/reply.
type MailReplyInput struct {
	CityScope
	ID   string `path:"id" doc:"Message ID."`
	Rig  string `query:"rig" required:"false" doc:"Rig hint."`
	Body struct {
		From    string `json:"from,omitempty" doc:"Sender name."`
		Subject string `json:"subject,omitempty" doc:"Reply subject."`
		Body    string `json:"body,omitempty" doc:"Reply body."`
	}
}

// MailDeleteInput is the Huma input for DELETE /v0/city/{cityName}/mail/{id}.
type MailDeleteInput struct {
	CityScope
	ID  string `path:"id" doc:"Message ID."`
	Rig string `query:"rig" required:"false" doc:"Rig hint."`
}

// MailThreadInput is the Huma input for GET /v0/city/{cityName}/mail/thread/{id}.
type MailThreadInput struct {
	CityScope
	ID  string `path:"id" doc:"Thread ID."`
	Rig string `query:"rig" required:"false" doc:"Filter by rig."`
}

// MailCountInput is the Huma input for GET /v0/city/{cityName}/mail/count.
type MailCountInput struct {
	CityScope
	Agent string `query:"agent" required:"false" doc:"Filter by agent name."`
	Rig   string `query:"rig" required:"false" doc:"Filter by rig name."`
}

// MailCountOutput is the response body for GET /v0/mail/count.
type MailCountOutput struct {
	Body struct {
		Total  int `json:"total" doc:"Total message count."`
		Unread int `json:"unread" doc:"Unread message count."`
	}
}

// --- Convoy types ---

// ConvoyListInput is the Huma input for GET /v0/city/{cityName}/convoys.
type ConvoyListInput struct {
	CityScope
	BlockingParam
	PaginationParam
}

// ConvoyGetInput is the Huma input for GET /v0/city/{cityName}/convoy/{id}.
type ConvoyGetInput struct {
	CityScope
	ID string `path:"id" doc:"Convoy ID."`
}

// ConvoyCreateInput is the Huma input for POST /v0/city/{cityName}/convoys.
type ConvoyCreateInput struct {
	CityScope
	Body struct {
		Rig   string   `json:"rig,omitempty" doc:"Rig name."`
		Title string   `json:"title" doc:"Convoy title." minLength:"1"`
		Items []string `json:"items,omitempty" doc:"Bead IDs to include."`
	}
}

// ConvoyAddInput is the Huma input for POST /v0/city/{cityName}/convoy/{id}/add.
type ConvoyAddInput struct {
	CityScope
	ID   string `path:"id" doc:"Convoy ID."`
	Body struct {
		Items []string `json:"items,omitempty" doc:"Bead IDs to add."`
	}
}

// ConvoyRemoveInput is the Huma input for POST /v0/city/{cityName}/convoy/{id}/remove.
type ConvoyRemoveInput struct {
	CityScope
	ID   string `path:"id" doc:"Convoy ID."`
	Body struct {
		Items []string `json:"items,omitempty" doc:"Bead IDs to remove."`
	}
}

// ConvoyCheckInput is the Huma input for GET /v0/city/{cityName}/convoy/{id}/check.
type ConvoyCheckInput struct {
	CityScope
	ID string `path:"id" doc:"Convoy ID."`
}

// ConvoyCloseInput is the Huma input for POST /v0/city/{cityName}/convoy/{id}/close.
type ConvoyCloseInput struct {
	CityScope
	ID string `path:"id" doc:"Convoy ID."`
}

// ConvoyDeleteInput is the Huma input for DELETE /v0/city/{cityName}/convoy/{id}.
type ConvoyDeleteInput struct {
	CityScope
	ID string `path:"id" doc:"Convoy ID."`
}

// --- Config types ---

// ConfigGetInput is the Huma input for GET /v0/city/{cityName}/config.
type ConfigGetInput struct {
	CityScope
}

// ConfigExplainInput is the Huma input for GET /v0/city/{cityName}/config/explain.
type ConfigExplainInput struct {
	CityScope
}

// ConfigValidateInput is the Huma input for GET /v0/city/{cityName}/config/validate.
type ConfigValidateInput struct {
	CityScope
}

// ConfigValidateOutput is the response body for GET /v0/config/validate.
type ConfigValidateOutput struct {
	Body struct {
		Valid    bool     `json:"valid" doc:"Whether the configuration is valid."`
		Errors   []string `json:"errors" doc:"Validation errors."`
		Warnings []string `json:"warnings" doc:"Validation warnings."`
	}
}

// --- City types ---

// CityGetInput is the Huma input for GET /v0/city/{cityName}.
type CityGetInput struct {
	CityScope
}

// CityPatchInput is the Huma input for PATCH /v0/city/{cityName}.
type CityPatchInput struct {
	CityScope
	Body struct {
		Suspended *bool `json:"suspended,omitempty" doc:"Whether the city is suspended."`
	}
}

// ProviderReadinessInput is the Huma input for GET /v0/city/{cityName}/provider-readiness.
type ProviderReadinessInput struct {
	CityScope
	Providers string `query:"providers" required:"false" doc:"Comma-separated provider names to check (default: claude,codex,gemini)."`
	Fresh     string `query:"fresh" required:"false" doc:"Force fresh probe (0 or 1)."`
}

// ProviderReadinessOutput is the response body for GET /v0/provider-readiness.
type ProviderReadinessOutput struct {
	Body providerReadinessResponse
}

// ReadinessInput is the Huma input for GET /v0/city/{cityName}/readiness.
type ReadinessInput struct {
	CityScope
	Items string `query:"items" required:"false" doc:"Comma-separated readiness items to check (default: claude,codex,gemini,github_cli)."`
	Fresh string `query:"fresh" required:"false" doc:"Force fresh probe (0 or 1)."`
}

// ReadinessOutput is the response body for GET /v0/readiness.
type ReadinessOutput struct {
	Body readinessResponse
}

// --- Agent output types ---

// AgentOutputInput is the Huma input for GET /v0/agent/{base}/output.
type AgentOutputInput struct {
	Name   string `path:"base" doc:"Agent base name."`
	Tail   string `query:"tail" required:"false" doc:"Number of compaction segments to return (default 1, 0 = all)."`
	Before string `query:"before" required:"false" doc:"Message UUID cursor for loading older messages."`
}

// AgentOutputQualifiedInput is the Huma input for GET /v0/agent/{dir}/{base}/output.
type AgentOutputQualifiedInput struct {
	Dir    string `path:"dir" doc:"Agent directory (rig name)."`
	Base   string `path:"base" doc:"Agent base name."`
	Tail   string `query:"tail" required:"false" doc:"Number of compaction segments to return (default 1, 0 = all)."`
	Before string `query:"before" required:"false" doc:"Message UUID cursor for loading older messages."`
}

// QualifiedName returns the full qualified agent name from dir/base components.
func (i *AgentOutputQualifiedInput) QualifiedName() string {
	if i.Dir == "" {
		return i.Base
	}
	return i.Dir + "/" + i.Base
}

// AgentOutputStreamInput is the Huma input for the per-city SSE stream
// (still at bare /v0/agent/{base}/output/stream during migration — not
// yet migrated to /v0/city/{cityName}/... due to SSE registration plumbing).
type AgentOutputStreamInput struct {
	Base string `path:"base" doc:"Agent base name."`
}

// AgentOutputStreamQualifiedInput is the qualified variant. Same caveat.
type AgentOutputStreamQualifiedInput struct {
	Dir  string `path:"dir" doc:"Agent directory (rig name)."`
	Base string `path:"base" doc:"Agent base name."`
}

// QualifiedName returns the full qualified agent name from dir/base components.
func (i *AgentOutputStreamQualifiedInput) QualifiedName() string {
	if i.Dir == "" {
		return i.Base
	}
	return i.Dir + "/" + i.Base
}

// --- Formula detail types ---

// FormulaDetailInput is the Huma input for GET /v0/city/{cityName}/formulas/{name} and GET /v0/city/{cityName}/formula/{name}.
type FormulaDetailInput struct {
	CityScope
	Name      string `path:"name" doc:"Formula name."`
	ScopeKind string `query:"scope_kind" required:"false" doc:"Scope kind (city or rig)."`
	ScopeRef  string `query:"scope_ref" required:"false" doc:"Scope reference."`
	Target    string `query:"target" required:"false" doc:"Target agent for preview compilation."`

	// vars holds dynamic var.* query params, populated by Resolve.
	vars map[string]string
}

// Resolve implements huma.Resolver to extract dynamic var.* query params.
func (f *FormulaDetailInput) Resolve(ctx huma.Context) []error {
	u := ctx.URL()
	f.vars = make(map[string]string)
	for key, values := range u.Query() {
		if len(values) > 0 && len(key) > 4 && key[:4] == "var." {
			name := key[4:]
			if name != "" {
				f.vars[name] = values[len(values)-1]
			}
		}
	}
	if len(f.vars) == 0 {
		f.vars = nil
	}
	return nil
}

// --- Workflow backward-compat types ---

// WorkflowGetInput is the Huma input for GET /v0/city/{cityName}/workflow/{workflow_id}.
type WorkflowGetInput struct {
	CityScope
	WorkflowID string `path:"workflow_id" doc:"Workflow (convoy) ID."`
	ScopeKind  string `query:"scope_kind" required:"false" doc:"Scope kind (city or rig)."`
	ScopeRef   string `query:"scope_ref" required:"false" doc:"Scope reference."`
}

// WorkflowDeleteInput is the Huma input for DELETE /v0/city/{cityName}/workflow/{workflow_id}.
type WorkflowDeleteInput struct {
	CityScope
	WorkflowID string `path:"workflow_id" doc:"Workflow (convoy) ID."`
	ScopeKind  string `query:"scope_kind" required:"false" doc:"Scope kind (city or rig)."`
	ScopeRef   string `query:"scope_ref" required:"false" doc:"Scope reference."`
	Delete     string `query:"delete" required:"false" doc:"Permanently delete beads from store (true/false)."`
}

// --- Patch response types ---

// PatchOKResponse is a success response for patch set operations.
type PatchOKResponse struct {
	Body struct {
		Status        string `json:"status" doc:"Operation result." example:"ok"`
		AgentPatch    string `json:"agent_patch,omitempty" doc:"Agent patch qualified name."`
		RigPatch      string `json:"rig_patch,omitempty" doc:"Rig patch name."`
		ProviderPatch string `json:"provider_patch,omitempty" doc:"Provider patch name."`
	}
}

// PatchDeletedResponse is a success response for patch delete operations.
type PatchDeletedResponse struct {
	Body struct {
		Status        string `json:"status" doc:"Operation result." example:"deleted"`
		AgentPatch    string `json:"agent_patch,omitempty" doc:"Agent patch qualified name."`
		RigPatch      string `json:"rig_patch,omitempty" doc:"Rig patch name."`
		ProviderPatch string `json:"provider_patch,omitempty" doc:"Provider patch name."`
	}
}

// StatusBody is the response body for GET /v0/status.
type StatusBody struct {
	Name       string            `json:"name" doc:"City name."`
	Path       string            `json:"path" doc:"City directory path."`
	Version    string            `json:"version,omitempty" doc:"Server version."`
	UptimeSec  int               `json:"uptime_sec" doc:"Server uptime in seconds."`
	Suspended  bool              `json:"suspended" doc:"Whether the city is suspended."`
	AgentCount int               `json:"agent_count" doc:"Total agent count (deprecated, use agents.total)."`
	RigCount   int               `json:"rig_count" doc:"Total rig count (deprecated, use rigs.total)."`
	Running    int               `json:"running" doc:"Number of running agent processes."`
	Agents     StatusAgentCounts `json:"agents" doc:"Agent state counts."`
	Rigs       StatusRigCounts   `json:"rigs" doc:"Rig state counts."`
	Work       StatusWorkCounts  `json:"work" doc:"Work item counts."`
	Mail       StatusMailCounts  `json:"mail" doc:"Mail counts."`
}

// Session types moved to huma_types_sessions.go.

// --- Service types ---

// ServiceListInput is the Huma input for GET /v0/city/{cityName}/services.
type ServiceListInput struct {
	CityScope
}

// ServiceGetInput is the Huma input for GET /v0/city/{cityName}/service/{name}.
type ServiceGetInput struct {
	CityScope
	Name string `path:"name" doc:"Service name."`
}

// ServiceRestartInput is the Huma input for POST /v0/city/{cityName}/service/{name}/restart.
type ServiceRestartInput struct {
	CityScope
	Name string `path:"name" doc:"Service name."`
}

// ServiceRestartOutput is the Huma output for POST /v0/service/{name}/restart.
type ServiceRestartOutput struct {
	Body struct {
		Status  string `json:"status" doc:"Operation result." example:"ok"`
		Action  string `json:"action" doc:"Action performed." example:"restart"`
		Service string `json:"service" doc:"Service name."`
	}
}

// --- ExtMsg types ---

// ExtMsgInboundInput is the Huma input for POST /v0/city/{cityName}/extmsg/inbound.
type ExtMsgInboundInput struct {
	CityScope
	Body struct {
		Message   *extmsg.ExternalInboundMessage `json:"message,omitempty" doc:"Pre-normalized inbound message."`
		Provider  string                         `json:"provider,omitempty" doc:"Provider name for raw payloads."`
		AccountID string                         `json:"account_id,omitempty" doc:"Account ID for raw payloads."`
		Payload   []byte                         `json:"payload,omitempty" doc:"Raw payload bytes."`
	}
}

// ExtMsgInboundOutput is the Huma output for POST /v0/extmsg/inbound.
type ExtMsgInboundOutput struct {
	Body extmsg.InboundResult
}

// ExtMsgOutboundInput is the Huma input for POST /v0/city/{cityName}/extmsg/outbound.
type ExtMsgOutboundInput struct {
	CityScope
	Body struct {
		SessionID        string                 `json:"session_id,omitempty" doc:"Session ID."`
		Conversation     extmsg.ConversationRef `json:"conversation,omitempty" doc:"Target conversation."`
		Text             string                 `json:"text,omitempty" doc:"Message text."`
		ReplyToMessageID string                 `json:"reply_to_message_id,omitempty" doc:"Message ID to reply to."`
		IdempotencyKey   string                 `json:"idempotency_key,omitempty" doc:"Idempotency key."`
	}
}

// ExtMsgOutboundOutput is the Huma output for POST /v0/extmsg/outbound.
type ExtMsgOutboundOutput struct {
	Body extmsg.OutboundResult
}

// ExtMsgBindingListInput is the Huma input for GET /v0/city/{cityName}/extmsg/bindings.
type ExtMsgBindingListInput struct {
	CityScope
	SessionID string `query:"session_id" required:"false" doc:"Session ID to list bindings for."`
}

// ExtMsgBindInput is the Huma input for POST /v0/city/{cityName}/extmsg/bind.
type ExtMsgBindInput struct {
	CityScope
	Body struct {
		Conversation extmsg.ConversationRef `json:"conversation,omitempty" doc:"Conversation to bind."`
		SessionID    string                 `json:"session_id,omitempty" doc:"Session ID to bind."`
		Metadata     map[string]string      `json:"metadata,omitempty" doc:"Optional binding metadata."`
	}
}

// ExtMsgBindOutput is the Huma output for POST /v0/extmsg/bind.
type ExtMsgBindOutput struct {
	Body extmsg.SessionBindingRecord
}

// ExtMsgUnbindInput is the Huma input for POST /v0/city/{cityName}/extmsg/unbind.
type ExtMsgUnbindInput struct {
	CityScope
	Body struct {
		Conversation *extmsg.ConversationRef `json:"conversation,omitempty" doc:"Conversation to unbind (nil = all)."`
		SessionID    string                  `json:"session_id,omitempty" doc:"Session ID to unbind."`
	}
}

// ExtMsgUnbindOutput is the Huma output for POST /v0/extmsg/unbind.
// ExtMsgUnbindBody is the response body for POST /v0/extmsg/unbind.
type ExtMsgUnbindBody struct {
	Unbound []extmsg.SessionBindingRecord `json:"unbound" doc:"Bindings that were removed."`
}

type ExtMsgUnbindOutput struct {
	Body ExtMsgUnbindBody
}

// ExtMsgGroupLookupInput is the Huma input for GET /v0/city/{cityName}/extmsg/groups.
type ExtMsgGroupLookupInput struct {
	CityScope
	ScopeID        string `query:"scope_id" required:"false" doc:"Scope ID."`
	Provider       string `query:"provider" required:"false" doc:"Provider name."`
	AccountID      string `query:"account_id" required:"false" doc:"Account ID."`
	ConversationID string `query:"conversation_id" required:"false" doc:"Conversation ID."`
	Kind           string `query:"kind" required:"false" doc:"Conversation kind."`
}

// ExtMsgGroupOutput is the Huma output for GET /v0/extmsg/groups.
type ExtMsgGroupOutput struct {
	Body extmsg.ConversationGroupRecord
}

// ExtMsgGroupEnsureInput is the Huma input for POST /v0/city/{cityName}/extmsg/groups.
type ExtMsgGroupEnsureInput struct {
	CityScope
	Body struct {
		RootConversation extmsg.ConversationRef `json:"root_conversation,omitempty" doc:"Root conversation reference."`
		Mode             extmsg.GroupMode       `json:"mode,omitempty" doc:"Group mode (launcher, etc.)."`
		DefaultHandle    string                 `json:"default_handle,omitempty" doc:"Default handle for the group."`
		Metadata         map[string]string      `json:"metadata,omitempty" doc:"Group metadata."`
	}
}

// ExtMsgGroupEnsureOutput is the Huma output for POST /v0/extmsg/groups.
type ExtMsgGroupEnsureOutput struct {
	Body extmsg.ConversationGroupRecord
}

// ExtMsgParticipantUpsertInput is the Huma input for POST /v0/city/{cityName}/extmsg/participants.
type ExtMsgParticipantUpsertInput struct {
	CityScope
	Body struct {
		GroupID   string            `json:"group_id,omitempty" doc:"Group ID."`
		Handle    string            `json:"handle,omitempty" doc:"Participant handle."`
		SessionID string            `json:"session_id,omitempty" doc:"Session ID."`
		Public    bool              `json:"public,omitempty" doc:"Whether participant is public."`
		Metadata  map[string]string `json:"metadata,omitempty" doc:"Participant metadata."`
	}
}

// ExtMsgParticipantOutput is the Huma output for POST /v0/extmsg/participants.
type ExtMsgParticipantOutput struct {
	Body extmsg.ConversationGroupParticipant
}

// ExtMsgParticipantRemoveInput is the Huma input for DELETE /v0/city/{cityName}/extmsg/participants.
type ExtMsgParticipantRemoveInput struct {
	CityScope
	Body struct {
		GroupID string `json:"group_id,omitempty" doc:"Group ID."`
		Handle  string `json:"handle,omitempty" doc:"Participant handle."`
	}
}

// ExtMsgTranscriptListInput is the Huma input for GET /v0/city/{cityName}/extmsg/transcript.
type ExtMsgTranscriptListInput struct {
	CityScope
	ScopeID              string `query:"scope_id" required:"false" doc:"Scope ID."`
	Provider             string `query:"provider" required:"false" doc:"Provider name."`
	AccountID            string `query:"account_id" required:"false" doc:"Account ID."`
	ConversationID       string `query:"conversation_id" required:"false" doc:"Conversation ID."`
	ParentConversationID string `query:"parent_conversation_id" required:"false" doc:"Parent conversation ID."`
	Kind                 string `query:"kind" required:"false" doc:"Conversation kind."`
}

// ExtMsgTranscriptAckInput is the Huma input for POST /v0/city/{cityName}/extmsg/transcript/ack.
type ExtMsgTranscriptAckInput struct {
	CityScope
	Body struct {
		Conversation extmsg.ConversationRef `json:"conversation,omitempty" doc:"Conversation to acknowledge."`
		SessionID    string                 `json:"session_id,omitempty" doc:"Session ID."`
		Sequence     int64                  `json:"sequence,omitempty" doc:"Sequence number to acknowledge up to."`
	}
}

// ExtMsgAdapterListInput is the Huma input for GET /v0/city/{cityName}/extmsg/adapters.
type ExtMsgAdapterListInput struct {
	CityScope
}

// ExtMsgAdapterRegisterInput is the Huma input for POST /v0/city/{cityName}/extmsg/adapters.
type ExtMsgAdapterRegisterInput struct {
	CityScope
	Body struct {
		Provider     string                     `json:"provider,omitempty" doc:"Provider name."`
		AccountID    string                     `json:"account_id,omitempty" doc:"Account ID."`
		Name         string                     `json:"name,omitempty" doc:"Adapter display name."`
		CallbackURL  string                     `json:"callback_url,omitempty" doc:"Callback URL for outbound messages."`
		Capabilities extmsg.AdapterCapabilities `json:"capabilities,omitempty" doc:"Adapter capabilities."`
	}
}

// ExtMsgAdapterRegisterOutput is the Huma output for POST /v0/extmsg/adapters.
type ExtMsgAdapterRegisterOutput struct {
	Body struct {
		Status    string `json:"status" doc:"Operation result." example:"registered"`
		Provider  string `json:"provider" doc:"Provider name."`
		AccountID string `json:"account_id" doc:"Account ID."`
		Name      string `json:"name" doc:"Adapter name."`
	}
}

// ExtMsgAdapterUnregisterInput is the Huma input for DELETE /v0/city/{cityName}/extmsg/adapters.
type ExtMsgAdapterUnregisterInput struct {
	CityScope
	Body struct {
		Provider  string `json:"provider,omitempty" doc:"Provider name."`
		AccountID string `json:"account_id,omitempty" doc:"Account ID."`
	}
}
