package api

// Shared Huma input/output types for the Gas City API.
//
// These types define the API contract: wire format, validation, and OpenAPI
// documentation. They are the source of truth for the auto-generated OpenAPI
// 3.1 spec at /openapi.json.

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/session"
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

// apiError is a custom error type that implements huma.StatusError and marshals
// into the legacy Error JSON shape (code, message, details). This lets Huma
// handlers return structured error responses that match the original wire format
// expected by existing clients and tests.
type apiError struct {
	StatusCode int          `json:"-"`
	Code       string       `json:"code"`
	Message    string       `json:"message"`
	Details    []FieldError `json:"details,omitempty"`
}

func (e *apiError) Error() string { return e.Message }

// GetStatus implements huma.StatusError.
func (e *apiError) GetStatus() int { return e.StatusCode }

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

// --- Provider types ---

// ProviderListInput is the Huma input for GET /v0/providers.
type ProviderListInput struct {
	View string `query:"view" required:"false" doc:"Response view: 'public' omits command/args/env details."`
}

// ProviderGetInput is the Huma input for GET /v0/provider/{name}.
type ProviderGetInput struct {
	Name string `path:"name" doc:"Provider name."`
}

// ProviderCreateInput is the Huma input for POST /v0/providers.
type ProviderCreateInput struct {
	Body struct {
		Name         string            `json:"name,omitempty" doc:"Provider name."`
		DisplayName  string            `json:"display_name,omitempty" doc:"Human-readable display name."`
		Command      string            `json:"command,omitempty" doc:"Provider command binary."`
		Args         []string          `json:"args,omitempty" doc:"Command arguments."`
		PromptMode   string            `json:"prompt_mode,omitempty" doc:"Prompt delivery mode."`
		PromptFlag   string            `json:"prompt_flag,omitempty" doc:"Flag for prompt delivery."`
		ReadyDelayMs int               `json:"ready_delay_ms,omitempty" doc:"Milliseconds to wait before probing readiness."`
		Env          map[string]string `json:"env,omitempty" doc:"Environment variables."`
	}
}

// ProviderUpdateInput is the Huma input for PATCH /v0/provider/{name}.
type ProviderUpdateInput struct {
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

// ProviderDeleteInput is the Huma input for DELETE /v0/provider/{name}.
type ProviderDeleteInput struct {
	Name string `path:"name" doc:"Provider name."`
}

// --- Rig types ---

// RigListInput is the Huma input for GET /v0/rigs.
type RigListInput struct {
	BlockingParam
	Git string `query:"git" required:"false" doc:"Include git status (true/false)."`
}

// RigGetInput is the Huma input for GET /v0/rig/{name}.
type RigGetInput struct {
	Name string `path:"name" doc:"Rig name."`
	Git  string `query:"git" required:"false" doc:"Include git status (true/false)."`
}

// RigCreateInput is the Huma input for POST /v0/rigs.
type RigCreateInput struct {
	Body struct {
		Name   string `json:"name,omitempty" doc:"Rig name."`
		Path   string `json:"path,omitempty" doc:"Filesystem path."`
		Prefix string `json:"prefix,omitempty" doc:"Session name prefix."`
	}
}

// RigUpdateInput is the Huma input for PATCH /v0/rig/{name}.
type RigUpdateInput struct {
	Name string `path:"name" doc:"Rig name."`
	Body struct {
		Path      string `json:"path,omitempty" doc:"Filesystem path."`
		Prefix    string `json:"prefix,omitempty" doc:"Session name prefix."`
		Suspended *bool  `json:"suspended,omitempty" doc:"Whether rig is suspended."`
	}
}

// RigDeleteInput is the Huma input for DELETE /v0/rig/{name}.
type RigDeleteInput struct {
	Name string `path:"name" doc:"Rig name."`
}

// RigActionInput is the Huma input for POST /v0/rig/{name}/{action}.
type RigActionInput struct {
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

// AgentPatchListInput is the Huma input for GET /v0/patches/agents.
type AgentPatchListInput struct{}

// AgentPatchGetInput is the Huma input for GET /v0/patches/agent/{name}.
type AgentPatchGetInput struct {
	Name string `path:"name" doc:"Agent patch qualified name."`
}

// AgentPatchSetInput is the Huma input for PUT /v0/patches/agents.
type AgentPatchSetInput struct {
	Body struct {
		Dir       string            `json:"dir,omitempty" doc:"Agent directory scope."`
		Name      string            `json:"name,omitempty" doc:"Agent name."`
		WorkDir   *string           `json:"work_dir,omitempty" doc:"Override session working directory."`
		Scope     *string           `json:"scope,omitempty" doc:"Override agent scope."`
		Suspended *bool             `json:"suspended,omitempty" doc:"Override suspended state."`
		Env       map[string]string `json:"env,omitempty" doc:"Override environment variables."`
	}
}

// AgentPatchDeleteInput is the Huma input for DELETE /v0/patches/agent/{name}.
type AgentPatchDeleteInput struct {
	Name string `path:"name" doc:"Agent patch qualified name."`
}

// RigPatchListInput is the Huma input for GET /v0/patches/rigs.
type RigPatchListInput struct{}

// RigPatchGetInput is the Huma input for GET /v0/patches/rig/{name}.
type RigPatchGetInput struct {
	Name string `path:"name" doc:"Rig patch name."`
}

// RigPatchSetInput is the Huma input for PUT /v0/patches/rigs.
type RigPatchSetInput struct {
	Body struct {
		Name      string `json:"name,omitempty" doc:"Rig name."`
		Path      *string `json:"path,omitempty" doc:"Override filesystem path."`
		Prefix    *string `json:"prefix,omitempty" doc:"Override bead ID prefix."`
		Suspended *bool   `json:"suspended,omitempty" doc:"Override suspended state."`
	}
}

// RigPatchDeleteInput is the Huma input for DELETE /v0/patches/rig/{name}.
type RigPatchDeleteInput struct {
	Name string `path:"name" doc:"Rig patch name."`
}

// ProviderPatchListInput is the Huma input for GET /v0/patches/providers.
type ProviderPatchListInput struct{}

// ProviderPatchGetInput is the Huma input for GET /v0/patches/provider/{name}.
type ProviderPatchGetInput struct {
	Name string `path:"name" doc:"Provider patch name."`
}

// ProviderPatchSetInput is the Huma input for PUT /v0/patches/providers.
type ProviderPatchSetInput struct {
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

// ProviderPatchDeleteInput is the Huma input for DELETE /v0/patches/provider/{name}.
type ProviderPatchDeleteInput struct {
	Name string `path:"name" doc:"Provider patch name."`
}

// --- Event types ---

// EventListInput is the Huma input for GET /v0/events.
type EventListInput struct {
	BlockingParam
	PaginationParam
	Type  string `query:"type" required:"false" doc:"Filter by event type."`
	Actor string `query:"actor" required:"false" doc:"Filter by actor."`
	Since string `query:"since" required:"false" doc:"Filter events since duration ago (Go duration string, e.g. 5m)."`
}

// EventEmitInput is the Huma input for POST /v0/events.
type EventEmitInput struct {
	Body struct {
		Type    string `json:"type,omitempty" doc:"Event type."`
		Actor   string `json:"actor,omitempty" doc:"Actor that produced the event."`
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

// --- Order types ---

// OrderListInput is the Huma input for GET /v0/orders.
type OrderListInput struct{}

// OrderGetInput is the Huma input for GET /v0/order/{name}.
type OrderGetInput struct {
	Name string `path:"name" doc:"Order name or scoped name."`
}

// OrderCheckInput is the Huma input for GET /v0/orders/check.
type OrderCheckInput struct{}

// OrderHistoryInput is the Huma input for GET /v0/orders/history.
type OrderHistoryInput struct {
	ScopedName string `query:"scoped_name" required:"false" doc:"Scoped order name."`
	Limit      string `query:"limit" required:"false" doc:"Maximum number of history entries."`
	Before     string `query:"before" required:"false" doc:"Return entries before this RFC3339 timestamp."`
}

// OrderHistoryDetailInput is the Huma input for GET /v0/order/history/{bead_id}.
type OrderHistoryDetailInput struct {
	BeadID string `path:"bead_id" doc:"Bead ID for the order run."`
}

// OrderEnableInput is the Huma input for POST /v0/order/{name}/enable.
type OrderEnableInput struct {
	Name string `path:"name" doc:"Order name or scoped name."`
}

// OrderDisableInput is the Huma input for POST /v0/order/{name}/disable.
type OrderDisableInput struct {
	Name string `path:"name" doc:"Order name or scoped name."`
}

// --- Formula types ---

// FormulaListInput is the Huma input for GET /v0/formulas.
type FormulaListInput struct {
	ScopeKind string `query:"scope_kind" required:"false" doc:"Scope kind (city or rig)."`
	ScopeRef  string `query:"scope_ref" required:"false" doc:"Scope reference."`
}

// FormulaRunsInput is the Huma input for GET /v0/formulas/{name}/runs.
type FormulaRunsInput struct {
	Name      string `path:"name" doc:"Formula name."`
	ScopeKind string `query:"scope_kind" required:"false" doc:"Scope kind (city or rig)."`
	ScopeRef  string `query:"scope_ref" required:"false" doc:"Scope reference."`
	Limit     string `query:"limit" required:"false" doc:"Maximum number of recent runs to return."`
}

// --- Pack types ---

// PackListInput is the Huma input for GET /v0/packs.
type PackListInput struct{}

// --- Sling types ---

// SlingInput is the Huma input for POST /v0/sling.
type SlingInput struct {
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

// BeadListInput is the Huma input for GET /v0/beads.
type BeadListInput struct {
	BlockingParam
	PaginationParam
	Status   string `query:"status" required:"false" doc:"Filter by bead status."`
	Type     string `query:"type" required:"false" doc:"Filter by bead type."`
	Label    string `query:"label" required:"false" doc:"Filter by label."`
	Assignee string `query:"assignee" required:"false" doc:"Filter by assignee."`
	Rig      string `query:"rig" required:"false" doc:"Filter by rig."`
}

// BeadReadyInput is the Huma input for GET /v0/beads/ready.
type BeadReadyInput struct {
	BlockingParam
}

// BeadGraphInput is the Huma input for GET /v0/beads/graph/{rootID}.
type BeadGraphInput struct {
	RootID string `path:"rootID" doc:"Root bead ID for the graph."`
}

// BeadGetInput is the Huma input for GET /v0/bead/{id}.
type BeadGetInput struct {
	ID string `path:"id" doc:"Bead ID."`
}

// BeadDepsInput is the Huma input for GET /v0/bead/{id}/deps.
type BeadDepsInput struct {
	ID string `path:"id" doc:"Bead ID."`
}

// BeadCreateInput is the Huma input for POST /v0/beads.
type BeadCreateInput struct {
	IdempotencyKey string `header:"Idempotency-Key" required:"false" doc:"Idempotency key for safe retries."`
	Body           struct {
		Rig         string   `json:"rig,omitempty" doc:"Rig name."`
		Title       string   `json:"title,omitempty" doc:"Bead title."`
		Type        string   `json:"type,omitempty" doc:"Bead type."`
		Priority    *int     `json:"priority,omitempty" doc:"Bead priority."`
		Assignee    string   `json:"assignee,omitempty" doc:"Assigned agent."`
		Description string   `json:"description,omitempty" doc:"Bead description."`
		Labels      []string `json:"labels,omitempty" doc:"Bead labels."`
	}
}

// BeadCloseInput is the Huma input for POST /v0/bead/{id}/close.
type BeadCloseInput struct {
	ID string `path:"id" doc:"Bead ID."`
}

// BeadReopenInput is the Huma input for POST /v0/bead/{id}/reopen.
type BeadReopenInput struct {
	ID string `path:"id" doc:"Bead ID."`
}

// BeadUpdateInput is the Huma input for POST /v0/bead/{id}/update and PATCH /v0/bead/{id}.
type BeadUpdateInput struct {
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

// BeadAssignInput is the Huma input for POST /v0/bead/{id}/assign.
type BeadAssignInput struct {
	ID   string `path:"id" doc:"Bead ID."`
	Body struct {
		Assignee string `json:"assignee,omitempty" doc:"Assignee name."`
	}
}

// BeadDeleteInput is the Huma input for DELETE /v0/bead/{id}.
type BeadDeleteInput struct {
	ID string `path:"id" doc:"Bead ID."`
}

// --- Mail types ---

// MailListInput is the Huma input for GET /v0/mail.
type MailListInput struct {
	BlockingParam
	PaginationParam
	Agent  string `query:"agent" required:"false" doc:"Filter by agent name."`
	Status string `query:"status" required:"false" doc:"Filter by status (unread, all)."`
	Rig    string `query:"rig" required:"false" doc:"Filter by rig name."`
}

// MailGetInput is the Huma input for GET /v0/mail/{id}.
type MailGetInput struct {
	ID  string `path:"id" doc:"Message ID."`
	Rig string `query:"rig" required:"false" doc:"Rig hint for O(1) lookup."`
}

// MailSendInput is the Huma input for POST /v0/mail.
type MailSendInput struct {
	IdempotencyKey string `header:"Idempotency-Key" required:"false" doc:"Idempotency key for safe retries."`
	Body           struct {
		Rig     string `json:"rig,omitempty" doc:"Rig name."`
		From    string `json:"from,omitempty" doc:"Sender name."`
		To      string `json:"to,omitempty" doc:"Recipient name."`
		Subject string `json:"subject,omitempty" doc:"Message subject."`
		Body    string `json:"body,omitempty" doc:"Message body."`
	}
}

// MailReadInput is the Huma input for POST /v0/mail/{id}/read.
type MailReadInput struct {
	ID  string `path:"id" doc:"Message ID."`
	Rig string `query:"rig" required:"false" doc:"Rig hint."`
}

// MailMarkUnreadInput is the Huma input for POST /v0/mail/{id}/mark-unread.
type MailMarkUnreadInput struct {
	ID  string `path:"id" doc:"Message ID."`
	Rig string `query:"rig" required:"false" doc:"Rig hint."`
}

// MailArchiveInput is the Huma input for POST /v0/mail/{id}/archive.
type MailArchiveInput struct {
	ID  string `path:"id" doc:"Message ID."`
	Rig string `query:"rig" required:"false" doc:"Rig hint."`
}

// MailReplyInput is the Huma input for POST /v0/mail/{id}/reply.
type MailReplyInput struct {
	ID   string `path:"id" doc:"Message ID."`
	Rig  string `query:"rig" required:"false" doc:"Rig hint."`
	Body struct {
		From    string `json:"from,omitempty" doc:"Sender name."`
		Subject string `json:"subject,omitempty" doc:"Reply subject."`
		Body    string `json:"body,omitempty" doc:"Reply body."`
	}
}

// MailDeleteInput is the Huma input for DELETE /v0/mail/{id}.
type MailDeleteInput struct {
	ID  string `path:"id" doc:"Message ID."`
	Rig string `query:"rig" required:"false" doc:"Rig hint."`
}

// MailThreadInput is the Huma input for GET /v0/mail/thread/{id}.
type MailThreadInput struct {
	ID  string `path:"id" doc:"Thread ID."`
	Rig string `query:"rig" required:"false" doc:"Filter by rig."`
}

// MailCountInput is the Huma input for GET /v0/mail/count.
type MailCountInput struct {
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

// ConvoyListInput is the Huma input for GET /v0/convoys.
type ConvoyListInput struct {
	BlockingParam
	PaginationParam
}

// ConvoyGetInput is the Huma input for GET /v0/convoy/{id}.
type ConvoyGetInput struct {
	ID string `path:"id" doc:"Convoy ID."`
}

// ConvoyCreateInput is the Huma input for POST /v0/convoys.
type ConvoyCreateInput struct {
	Body struct {
		Rig   string   `json:"rig,omitempty" doc:"Rig name."`
		Title string   `json:"title,omitempty" doc:"Convoy title."`
		Items []string `json:"items,omitempty" doc:"Bead IDs to include."`
	}
}

// ConvoyAddInput is the Huma input for POST /v0/convoy/{id}/add.
type ConvoyAddInput struct {
	ID   string `path:"id" doc:"Convoy ID."`
	Body struct {
		Items []string `json:"items,omitempty" doc:"Bead IDs to add."`
	}
}

// ConvoyRemoveInput is the Huma input for POST /v0/convoy/{id}/remove.
type ConvoyRemoveInput struct {
	ID   string `path:"id" doc:"Convoy ID."`
	Body struct {
		Items []string `json:"items,omitempty" doc:"Bead IDs to remove."`
	}
}

// ConvoyCheckInput is the Huma input for GET /v0/convoy/{id}/check.
type ConvoyCheckInput struct {
	ID string `path:"id" doc:"Convoy ID."`
}

// ConvoyCloseInput is the Huma input for POST /v0/convoy/{id}/close.
type ConvoyCloseInput struct {
	ID string `path:"id" doc:"Convoy ID."`
}

// ConvoyDeleteInput is the Huma input for DELETE /v0/convoy/{id}.
type ConvoyDeleteInput struct {
	ID string `path:"id" doc:"Convoy ID."`
}

// --- Config types ---

// ConfigGetInput is the Huma input for GET /v0/config.
type ConfigGetInput struct{}

// ConfigExplainInput is the Huma input for GET /v0/config/explain.
type ConfigExplainInput struct{}

// ConfigValidateInput is the Huma input for GET /v0/config/validate.
type ConfigValidateInput struct{}

// ConfigValidateOutput is the response body for GET /v0/config/validate.
type ConfigValidateOutput struct {
	Body struct {
		Valid    bool     `json:"valid" doc:"Whether the configuration is valid."`
		Errors   []string `json:"errors" doc:"Validation errors."`
		Warnings []string `json:"warnings" doc:"Validation warnings."`
	}
}

// --- City types ---

// CityGetInput is the Huma input for GET /v0/city.
type CityGetInput struct{}

// CityPatchInput is the Huma input for PATCH /v0/city.
type CityPatchInput struct {
	Body struct {
		Suspended *bool `json:"suspended,omitempty" doc:"Whether the city is suspended."`
	}
}

// CityCreateInput is the Huma input for POST /v0/city.
type CityCreateInput struct {
	Body struct {
		Dir              string `json:"dir,omitempty" doc:"Directory path for the new city."`
		Provider         string `json:"provider,omitempty" doc:"Provider name."`
		BootstrapProfile string `json:"bootstrap_profile,omitempty" doc:"Bootstrap profile name."`
	}
}

// CityCreateOutput is the response body for POST /v0/city.
type CityCreateOutput struct {
	Body struct {
		OK   bool   `json:"ok" doc:"Whether the city was created successfully."`
		Path string `json:"path" doc:"Absolute path to the created city."`
	}
}

// ProviderReadinessInput is the Huma input for GET /v0/provider-readiness.
type ProviderReadinessInput struct {
	Providers string `query:"providers" required:"false" doc:"Comma-separated provider names to check (default: claude,codex,gemini)."`
	Fresh     string `query:"fresh" required:"false" doc:"Force fresh probe (0 or 1)."`
}

// ProviderReadinessOutput is the response body for GET /v0/provider-readiness.
type ProviderReadinessOutput struct {
	Body providerReadinessResponse
}

// ReadinessInput is the Huma input for GET /v0/readiness.
type ReadinessInput struct {
	Items string `query:"items" required:"false" doc:"Comma-separated readiness items to check (default: claude,codex,gemini,github_cli)."`
	Fresh string `query:"fresh" required:"false" doc:"Force fresh probe (0 or 1)."`
}

// ReadinessOutput is the response body for GET /v0/readiness.
type ReadinessOutput struct {
	Body readinessResponse
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

// --- Session types ---

// SessionListInput is the Huma input for GET /v0/sessions.
type SessionListInput struct {
	PaginationParam
	State    string `query:"state" required:"false" doc:"Filter by session state (e.g. active, closed)."`
	Template string `query:"template" required:"false" doc:"Filter by session template (agent qualified name)."`
	Peek     string `query:"peek" required:"false" doc:"Include last output preview (true/false)."`
}

// SessionGetInput is the Huma input for GET /v0/session/{id}.
type SessionGetInput struct {
	ID   string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	Peek string `query:"peek" required:"false" doc:"Include last output preview (true/false)."`
}

// sessionCreateBody is the request body for POST /v0/sessions.
type sessionCreateBody struct {
	Kind              string            `json:"kind,omitempty" doc:"Session target kind: agent or provider."`
	Name              string            `json:"name,omitempty" doc:"Agent or provider name."`
	Alias             string            `json:"alias,omitempty" doc:"Optional session alias."`
	LegacySessionName *string           `json:"session_name,omitempty" doc:"Deprecated: use alias."`
	Message           string            `json:"message,omitempty" doc:"Initial message to send to the session."`
	Async             bool              `json:"async,omitempty" doc:"Create session asynchronously (agent only)."`
	Options           map[string]string `json:"options,omitempty" doc:"Provider/agent option overrides."`
	ProjectID         string            `json:"project_id,omitempty" doc:"Opaque project context identifier."`
	Title             string            `json:"title,omitempty" doc:"Session title."`
}

// SessionCreateInput is the Huma input for POST /v0/sessions.
type SessionCreateInput struct {
	Body sessionCreateBody
}

// SessionCreateOutput is the Huma output for POST /v0/sessions.
type SessionCreateOutput struct {
	Body sessionResponse
}

// SessionIDInput is a generic Huma input for session endpoints that only need {id}.
type SessionIDInput struct {
	ID string `path:"id" doc:"Session ID, alias, or runtime session_name."`
}

// SessionTranscriptInput is the Huma input for GET /v0/session/{id}/transcript.
type SessionTranscriptInput struct {
	ID     string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	Format string `query:"format" required:"false" doc:"Transcript format: conversation (default) or raw."`
	Tail   string `query:"tail" required:"false" doc:"Number of recent entries to return."`
	Before string `query:"before" required:"false" doc:"Pagination cursor: return entries before this UUID."`
}

// SessionPatchInput is the Huma input for PATCH /v0/session/{id}.
type SessionPatchInput struct {
	ID   string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	Body struct {
		Title *string `json:"title,omitempty" doc:"Session title."`
		Alias *string `json:"alias,omitempty" doc:"Session alias."`
	}
}

// SessionCloseInput is the Huma input for POST /v0/session/{id}/close.
type SessionCloseInput struct {
	ID     string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	Delete string `query:"delete" required:"false" doc:"Permanently delete bead after closing (true/false)."`
}

// SessionSubmitInput is the Huma input for POST /v0/session/{id}/submit.
type SessionSubmitInput struct {
	ID   string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	Body struct {
		Message string               `json:"message,omitempty" doc:"Message text to submit."`
		Intent  session.SubmitIntent `json:"intent,omitempty" doc:"Submit intent: default, follow-up, or interrupt-now."`
	}
}

// SessionSubmitOutput is the Huma output for POST /v0/session/{id}/submit.
type SessionSubmitOutput struct {
	Body struct {
		Status string `json:"status" doc:"Operation result." example:"accepted"`
		ID     string `json:"id" doc:"Session ID."`
		Queued bool   `json:"queued" doc:"Whether the message was queued."`
		Intent string `json:"intent" doc:"Resolved submit intent."`
	}
}

// SessionMessageInput is the Huma input for POST /v0/session/{id}/messages.
type SessionMessageInput struct {
	ID   string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	Body struct {
		Message string `json:"message,omitempty" doc:"Message text to send."`
	}
}

// SessionMessageOutput is the Huma output for POST /v0/session/{id}/messages.
type SessionMessageOutput struct {
	Body struct {
		Status string `json:"status" doc:"Operation result." example:"accepted"`
		ID     string `json:"id" doc:"Session ID."`
	}
}

// SessionRespondInput is the Huma input for POST /v0/session/{id}/respond.
type SessionRespondInput struct {
	ID   string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	Body struct {
		RequestID string            `json:"request_id,omitempty" doc:"Pending interaction request ID."`
		Action    string            `json:"action,omitempty" doc:"Response action (e.g. allow, deny)."`
		Text      string            `json:"text,omitempty" doc:"Optional response text."`
		Metadata  map[string]string `json:"metadata,omitempty" doc:"Optional response metadata."`
	}
}

// SessionRespondOutput is the Huma output for POST /v0/session/{id}/respond.
type SessionRespondOutput struct {
	Body struct {
		Status string `json:"status" doc:"Operation result." example:"accepted"`
		ID     string `json:"id" doc:"Session ID."`
	}
}

// SessionRenameInput is the Huma input for POST /v0/session/{id}/rename.
type SessionRenameInput struct {
	ID   string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	Body struct {
		Title string `json:"title,omitempty" doc:"New session title."`
	}
}

// SessionAgentGetInput is the Huma input for GET /v0/session/{id}/agents/{agentId}.
type SessionAgentGetInput struct {
	ID      string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	AgentID string `path:"agentId" doc:"Subagent ID within the session."`
}

// OKWithIDResponse is a success response with an ID field.
type OKWithIDResponse struct {
	Body struct {
		Status string `json:"status" doc:"Operation result." example:"ok"`
		ID     string `json:"id,omitempty" doc:"Resource ID."`
	}
}

// --- Service types ---

// ServiceListInput is the Huma input for GET /v0/services.
type ServiceListInput struct{}

// ServiceGetInput is the Huma input for GET /v0/service/{name}.
type ServiceGetInput struct {
	Name string `path:"name" doc:"Service name."`
}

// ServiceRestartInput is the Huma input for POST /v0/service/{name}/restart.
type ServiceRestartInput struct {
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

// ExtMsgInboundInput is the Huma input for POST /v0/extmsg/inbound.
type ExtMsgInboundInput struct {
	Body struct {
		Message   *extmsg.ExternalInboundMessage `json:"message,omitempty" doc:"Pre-normalized inbound message."`
		Provider  string                         `json:"provider,omitempty" doc:"Provider name for raw payloads."`
		AccountID string                         `json:"account_id,omitempty" doc:"Account ID for raw payloads."`
		Payload   []byte                         `json:"payload,omitempty" doc:"Raw payload bytes."`
	}
}

// ExtMsgInboundOutput is the Huma output for POST /v0/extmsg/inbound.
type ExtMsgInboundOutput struct {
	Body json.RawMessage
}

// ExtMsgOutboundInput is the Huma input for POST /v0/extmsg/outbound.
type ExtMsgOutboundInput struct {
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
	Body json.RawMessage
}

// ExtMsgBindingListInput is the Huma input for GET /v0/extmsg/bindings.
type ExtMsgBindingListInput struct {
	SessionID string `query:"session_id" required:"false" doc:"Session ID to list bindings for."`
}

// ExtMsgBindInput is the Huma input for POST /v0/extmsg/bind.
type ExtMsgBindInput struct {
	Body struct {
		Conversation extmsg.ConversationRef `json:"conversation,omitempty" doc:"Conversation to bind."`
		SessionID    string                 `json:"session_id,omitempty" doc:"Session ID to bind."`
		Metadata     map[string]string      `json:"metadata,omitempty" doc:"Optional binding metadata."`
	}
}

// ExtMsgBindOutput is the Huma output for POST /v0/extmsg/bind.
type ExtMsgBindOutput struct {
	Body json.RawMessage
}

// ExtMsgUnbindInput is the Huma input for POST /v0/extmsg/unbind.
type ExtMsgUnbindInput struct {
	Body struct {
		Conversation *extmsg.ConversationRef `json:"conversation,omitempty" doc:"Conversation to unbind (nil = all)."`
		SessionID    string                  `json:"session_id,omitempty" doc:"Session ID to unbind."`
	}
}

// ExtMsgUnbindOutput is the Huma output for POST /v0/extmsg/unbind.
type ExtMsgUnbindOutput struct {
	Body json.RawMessage
}

// ExtMsgGroupLookupInput is the Huma input for GET /v0/extmsg/groups.
type ExtMsgGroupLookupInput struct {
	ScopeID        string `query:"scope_id" required:"false" doc:"Scope ID."`
	Provider       string `query:"provider" required:"false" doc:"Provider name."`
	AccountID      string `query:"account_id" required:"false" doc:"Account ID."`
	ConversationID string `query:"conversation_id" required:"false" doc:"Conversation ID."`
	Kind           string `query:"kind" required:"false" doc:"Conversation kind."`
}

// ExtMsgGroupOutput is the Huma output for GET /v0/extmsg/groups.
type ExtMsgGroupOutput struct {
	Body json.RawMessage
}

// ExtMsgGroupEnsureInput is the Huma input for POST /v0/extmsg/groups.
type ExtMsgGroupEnsureInput struct {
	Body struct {
		RootConversation extmsg.ConversationRef `json:"root_conversation,omitempty" doc:"Root conversation reference."`
		Mode             extmsg.GroupMode       `json:"mode,omitempty" doc:"Group mode (launcher, etc.)."`
		DefaultHandle    string                 `json:"default_handle,omitempty" doc:"Default handle for the group."`
		Metadata         map[string]string      `json:"metadata,omitempty" doc:"Group metadata."`
	}
}

// ExtMsgGroupEnsureOutput is the Huma output for POST /v0/extmsg/groups.
type ExtMsgGroupEnsureOutput struct {
	Body json.RawMessage
}

// ExtMsgParticipantUpsertInput is the Huma input for POST /v0/extmsg/participants.
type ExtMsgParticipantUpsertInput struct {
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
	Body json.RawMessage
}

// ExtMsgParticipantRemoveInput is the Huma input for DELETE /v0/extmsg/participants.
type ExtMsgParticipantRemoveInput struct {
	Body struct {
		GroupID string `json:"group_id,omitempty" doc:"Group ID."`
		Handle  string `json:"handle,omitempty" doc:"Participant handle."`
	}
}

// ExtMsgTranscriptListInput is the Huma input for GET /v0/extmsg/transcript.
type ExtMsgTranscriptListInput struct {
	ScopeID              string `query:"scope_id" required:"false" doc:"Scope ID."`
	Provider             string `query:"provider" required:"false" doc:"Provider name."`
	AccountID            string `query:"account_id" required:"false" doc:"Account ID."`
	ConversationID       string `query:"conversation_id" required:"false" doc:"Conversation ID."`
	ParentConversationID string `query:"parent_conversation_id" required:"false" doc:"Parent conversation ID."`
	Kind                 string `query:"kind" required:"false" doc:"Conversation kind."`
}

// ExtMsgTranscriptAckInput is the Huma input for POST /v0/extmsg/transcript/ack.
type ExtMsgTranscriptAckInput struct {
	Body struct {
		Conversation extmsg.ConversationRef `json:"conversation,omitempty" doc:"Conversation to acknowledge."`
		SessionID    string                 `json:"session_id,omitempty" doc:"Session ID."`
		Sequence     int64                  `json:"sequence,omitempty" doc:"Sequence number to acknowledge up to."`
	}
}

// ExtMsgAdapterListInput is the Huma input for GET /v0/extmsg/adapters.
type ExtMsgAdapterListInput struct{}

// ExtMsgAdapterRegisterInput is the Huma input for POST /v0/extmsg/adapters.
type ExtMsgAdapterRegisterInput struct {
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

// ExtMsgAdapterUnregisterInput is the Huma input for DELETE /v0/extmsg/adapters.
type ExtMsgAdapterUnregisterInput struct {
	Body struct {
		Provider  string `json:"provider,omitempty" doc:"Provider name."`
		AccountID string `json:"account_id,omitempty" doc:"Account ID."`
	}
}
