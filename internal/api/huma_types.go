package api

// Shared Huma input/output types for the Gas City API.
//
// These types define the API contract: wire format, validation, and OpenAPI
// documentation. They are the source of truth for the auto-generated OpenAPI
// 3.1 spec at /openapi.json.

import (
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
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
func mutationError(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "not found"):
		return huma.Error404NotFound(msg)
	case strings.Contains(msg, "already exists"):
		return huma.Error409Conflict(msg)
	case strings.Contains(msg, "pack-derived"):
		return huma.Error409Conflict(msg)
	case strings.Contains(msg, "validating"):
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
		Status string `json:"status" doc:"Operation result." example:"created"`
		Agent  string `json:"agent,omitempty" doc:"Created resource name."`
		Rig    string `json:"rig,omitempty" doc:"Created resource name."`
	}
}

// --- Agent types ---

// AgentListInput is the Huma input for GET /v0/agents.
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
		Name     string `json:"name,omitempty" doc:"Agent name."`
		Dir      string `json:"dir,omitempty" doc:"Working directory (rig name)."`
		Provider string `json:"provider,omitempty" doc:"Provider name."`
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
