package api

// Per-domain Huma input/output types for the convoys handler
// group. Split out of the original huma_types.go; mirrors the layout
// of huma_handlers_convoys.go.

import "encoding/json"

// --- Workflow snapshot response types ---
//
// These response bodies are shared between convoy/get and workflow/get
// handlers; both surface "the workflow root bead + its dependency
// graph + its scope groups" to clients. They live in this file rather
// than alongside the dispatch helpers so Fix 3f's "every response-body
// type lives in huma_types_*.go" grep gate catches them.

// workflowSnapshotResponse is the Huma output body for GET
// /v0/city/{cityName}/workflow/{id} and the embedded snapshot in
// GET /v0/city/{cityName}/convoy/{id} when the convoy is a workflow.
type workflowSnapshotResponse struct {
	WorkflowID        string                 `json:"workflow_id"`
	RootBeadID        string                 `json:"root_bead_id"`
	RootStoreRef      string                 `json:"root_store_ref"`
	ScopeKind         string                 `json:"scope_kind"`
	ScopeRef          string                 `json:"scope_ref"`
	Beads             []workflowBeadResponse `json:"beads"`
	Deps              []workflowDepResponse  `json:"deps"`
	LogicalNodes      []logicalNodeResponse  `json:"logical_nodes"`
	LogicalEdges      []workflowDepResponse  `json:"logical_edges"`
	ScopeGroups       []scopeGroupResponse   `json:"scope_groups"`
	Partial           bool                   `json:"partial"`
	ResolvedRootStore string                 `json:"resolved_root_store"`
	StoresScanned     []string               `json:"stores_scanned"`
	SnapshotVersion   uint64                 `json:"snapshot_version"`
	SnapshotEventSeq  *uint64                `json:"snapshot_event_seq,omitempty"`
}

// workflowBeadResponse is one bead node in a workflow snapshot.
type workflowBeadResponse struct {
	ID            string            `json:"id"`
	Title         string            `json:"title"`
	Status        string            `json:"status"`
	Kind          string            `json:"kind"`
	StepRef       string            `json:"step_ref,omitempty"`
	Attempt       *int              `json:"attempt,omitempty"`
	LogicalBeadID string            `json:"logical_bead_id,omitempty"`
	ScopeRef      string            `json:"scope_ref,omitempty"`
	Assignee      string            `json:"assignee,omitempty"`
	Metadata      map[string]string `json:"metadata"`
}

// workflowDepResponse is one dependency edge in a workflow snapshot.
type workflowDepResponse struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind,omitempty"`
}

// Presentation types (logical_nodes, logical_edges, scope_groups) are
// computed by the MC server's workflow_presentation module; GC always
// emits empty arrays here. They stay json.RawMessage so the spec
// declares them as opaque objects — describing them as typed would
// claim a contract GC doesn't own.
type (
	logicalNodeResponse = json.RawMessage
	scopeGroupResponse  = json.RawMessage
)

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

