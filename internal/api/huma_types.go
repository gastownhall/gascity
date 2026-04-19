package api

// Shared Huma input/output types for the Gas City API.
//
// These types define the API contract: wire format, validation, and OpenAPI
// documentation. They are the source of truth for the auto-generated OpenAPI
// 3.1 spec at /openapi.json.

//go:generate sh -c "cd ../.. && go run ./cmd/genspec"

import (
	"errors"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/configedit"
)

// --- Shared input mixins ---

// BlockingParam is an embeddable input mixin for long-polling endpoints.
// When index is provided, the handler blocks until a newer event arrives.
// Index stays typed as a string on the wire so "not provided" is
// distinguishable from "0" (which means "wait for the first event");
// Resolve validates the value so garbage input returns 400 instead of
// silently blocking.
type BlockingParam struct {
	Index string `query:"index" doc:"Event sequence number; when provided, blocks until a newer event arrives." required:"false"`
	Wait  string `query:"wait" doc:"How long to block waiting for changes (Go duration string, e.g. 30s). Default 30s, max 2m." required:"false"`
}

// Resolve validates the blocking-query parameters. Implements huma.Resolver;
// Huma calls this after binding the struct, so invalid values turn into
// 422 responses rather than default-0 behavior.
func (bp *BlockingParam) Resolve(_ huma.Context) []error {
	var errs []error
	if bp.Index != "" {
		if _, err := strconv.ParseUint(bp.Index, 10, 64); err != nil {
			errs = append(errs, &huma.ErrorDetail{
				Location: "query.index",
				Message:  "index must be a non-negative integer",
				Value:    bp.Index,
			})
		}
	}
	if bp.Wait != "" {
		if d, err := time.ParseDuration(bp.Wait); err != nil || d <= 0 {
			errs = append(errs, &huma.ErrorDetail{
				Location: "query.wait",
				Message:  "wait must be a positive Go duration string (e.g. 30s)",
				Value:    bp.Wait,
			})
		}
	}
	return errs
}

// toBlockingParams converts to the internal BlockingParams type. Values
// have already been validated by Resolve, so parse errors are impossible
// here.
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

// TailParam is an embeddable input mixin for transcript/agent-output
// endpoints that use the sessionlog "tail N compaction segments" shape.
//
// tail stays typed as a string on the wire so three request states are
// distinguishable:
//
//   - absent ("")   → handler applies its own default (usually 1)
//   - "0"           → return all segments (no pagination)
//   - "N" where N>0 → return the last N segments
//
// A prior refactor typed this as int and collapsed the first two states
// into "tail=0", which silently broke the "return all" contract. The
// Resolve method validates non-negative integer format and returns 422
// for garbage.
type TailParam struct {
	Tail string `query:"tail" required:"false" doc:"Number of recent compaction segments to return. Omit for the endpoint default (usually 1); 0 returns all segments; N>0 returns the last N."`
}

// Resolve validates Tail. Huma calls this during binding.
func (t *TailParam) Resolve(_ huma.Context) []error {
	if t.Tail == "" {
		return nil
	}
	n, err := strconv.Atoi(t.Tail)
	if err != nil || n < 0 {
		return []error{&huma.ErrorDetail{
			Location: "query.tail",
			Message:  "tail must be a non-negative integer",
			Value:    t.Tail,
		}}
	}
	_ = n
	return nil
}

// Compactions returns (n, provided). When provided is false, callers
// should apply their own default. n is guaranteed valid because Resolve
// rejected malformed input before the handler ran.
func (t *TailParam) Compactions() (n int, provided bool) {
	if t.Tail == "" {
		return 0, false
	}
	n, _ = strconv.Atoi(t.Tail)
	return n, true
}

// PaginationParam is an embeddable input mixin for paginated list endpoints.
// Limit carries a minimum: validation tag so malformed requests (e.g.
// limit=-1) fail Huma validation with 422 instead of silently defaulting
// or — under older paginate() behavior — panicking with a slice-bounds
// error.
type PaginationParam struct {
	Cursor string `query:"cursor" doc:"Pagination cursor from a previous response's next_cursor field." required:"false"`
	Limit  int    `query:"limit" minimum:"0" doc:"Maximum number of results to return. 0 = server default." required:"false"`
}

// --- Shared output types ---

// ListBody is the JSON body for list responses. It wraps items with total
// count and optional pagination cursor. Partial/PartialErrors signal that
// the aggregation swept over multiple backends and at least one of them
// failed — callers then know the list is not authoritative without the
// endpoint having to return a 5xx.
type ListBody[T any] struct {
	Items         []T      `json:"items" doc:"The list of items."`
	Total         int      `json:"total" doc:"Total number of items matching the query."`
	NextCursor    string   `json:"next_cursor,omitempty" doc:"Cursor for the next page of results."`
	Partial       bool     `json:"partial,omitempty" doc:"True when one or more backends failed and the list is incomplete."`
	PartialErrors []string `json:"partial_errors,omitempty" doc:"Human-readable errors from backends that failed during aggregation."`
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

// AgentCreatedOutput is the 201 response for POST /agents.
type AgentCreatedOutput struct {
	Body struct {
		Status string `json:"status" doc:"Operation result." example:"created"`
		Agent  string `json:"agent" doc:"Created agent name."`
	}
}

// RigCreatedOutput is the 201 response for POST /rigs.
type RigCreatedOutput struct {
	Body struct {
		Status string `json:"status" doc:"Operation result." example:"created"`
		Rig    string `json:"rig" doc:"Created rig name."`
	}
}

// ProviderCreatedOutput is the 201 response for POST /providers.
type ProviderCreatedOutput struct {
	Body struct {
		Status   string `json:"status" doc:"Operation result." example:"created"`
		Provider string `json:"provider" doc:"Created provider name."`
	}
}
