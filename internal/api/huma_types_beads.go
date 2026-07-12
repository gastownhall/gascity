package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// Per-domain Huma input/output types for the beads handler
// group. Split out of the original huma_types.go; mirrors the layout
// of huma_handlers_beads.go.

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
	All      bool   `query:"all" required:"false" doc:"Include closed beads."`
}

// BeadReadyInput is the Huma input for GET /v0/city/{cityName}/beads/ready.
type BeadReadyInput struct {
	CityScope
	BlockingParam
	// Cached opts into the supervisor cache projection instead of the
	// authoritative live backing store. The control dispatcher
	// (api.Client.ListReadyBeads) sets it so its tight serve loop reads ready
	// work from the in-memory projection and avoids the blocking/failing
	// backing-store scan the control-ready cache exists to avoid; cache
	// unavailability surfaces as a partial/outage so the dispatcher backs off.
	// Omitted (the default) the endpoint stays authoritative live readiness, so
	// a closed or newly-blocked bead is never reported as ready.
	Cached bool `query:"cached" required:"false" doc:"Serve ready work from the supervisor cache projection (eventually consistent) instead of the authoritative live backing store. Defaults to false (live)."`
	// ControlAssignees and ControlRoutes opt into server-side control-ready
	// filtering: only ready beads assigned to one of the comma-separated
	// assignees, or unassigned beads routed (by run-target, routed-to, or
	// execution-routed-to metadata) to one of the comma-separated routes, are
	// returned. The control dispatcher sends its resolved assignee and route sets
	// so the supervisor filters its control work out of the federated ready set
	// before serialization, instead of shipping every rig's ready beads for the
	// dispatcher to filter client-side. Empty (the default) returns the full
	// federated ready set unchanged, so generic callers (CLI, dashboard) are
	// unaffected.
	ControlAssignees string `query:"control_assignees" required:"false" doc:"Comma-separated assignees: keep only ready beads assigned to one of these. Empty returns the full ready set. Used by the control dispatcher to filter server-side."`
	ControlRoutes    string `query:"control_routes" required:"false" doc:"Comma-separated routes: keep unassigned ready beads routed (run-target/routed-to/execution-routed-to) to one of these. Empty returns the full ready set. Used by the control dispatcher to filter server-side."`
	// ControlLimit bounds how many ready beads each control assignee and each
	// control route may contribute (per-group; 0 = unbounded). It is applied with
	// ControlAssignees/ControlRoutes and ignored when both are empty.
	ControlLimit int `query:"control_limit" required:"false" minimum:"0" doc:"Per-group cap on ready beads contributed by each control assignee and route. 0 = unbounded. Ignored unless control_assignees or control_routes is set."`
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
		Rig         string            `json:"rig,omitempty" doc:"Rig name."`
		Title       string            `json:"title" doc:"Bead title." minLength:"1"`
		Type        string            `json:"type,omitempty" doc:"Bead type."`
		Priority    *int              `json:"priority,omitempty" doc:"Bead priority."`
		Assignee    string            `json:"assignee,omitempty" doc:"Assigned agent."`
		Description string            `json:"description,omitempty" doc:"Bead description."`
		Labels      []string          `json:"labels,omitempty" doc:"Bead labels."`
		Parent      string            `json:"parent,omitempty" doc:"Parent bead ID."`
		Metadata    map[string]string `json:"metadata,omitempty" doc:"Metadata key-value pairs to set at create time."`
		DeferUntil  *time.Time        `json:"defer_until,omitempty" doc:"Hide the bead from ready views until this time."`
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
	Parent       *string           `json:"parent,omitempty" nullable:"true" doc:"Parent bead ID. Use null or an empty string to clear."`
	Metadata     map[string]string `json:"metadata,omitempty" doc:"Metadata key-value pairs to set."`
	parentSet    bool
}

// UnmarshalJSON rejects `"priority": null` explicitly. Standard Go JSON decoding
// folds null and absent into a nil pointer, which silently drops clear-intent
// requests. Clients that want to clear priority must use a dedicated endpoint
// (not yet available); until then, null is a 400.
func (b *beadUpdateBody) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if p, ok := raw["priority"]; ok {
		trimmed := bytes.TrimSpace(p)
		if bytes.Equal(trimmed, []byte("null")) {
			return fmt.Errorf("clearing priority via null is not supported; omit the field to leave it unchanged")
		}
	}
	type alias beadUpdateBody
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*b = beadUpdateBody(a)
	if p, ok := raw["parent"]; ok {
		b.parentSet = true
		if bytes.Equal(bytes.TrimSpace(p), []byte("null")) {
			parent := ""
			b.Parent = &parent
		}
	}
	return nil
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
