package api

// Per-domain Huma input/output types for the formulas handler
// group. Split out of the original huma_types.go; mirrors the layout
// of huma_handlers_formulas.go.

import (
	"github.com/danielgtaylor/huma/v2"
)

// --- Formula response body types ---
//
// These are the shared response shapes returned by formula and
// formula-detail handlers. Keeping them here (rather than alongside
// the handler logic) ensures Fix 3f's grep for response-body types
// in huma_types_*.go sees every spec-surfaced shape.

// formulaRecentRunResponse summarizes one recent run of a formula.
type formulaRecentRunResponse struct {
	WorkflowID string `json:"workflow_id"`
	Status     string `json:"status"`
	Target     string `json:"target"`
	StartedAt  string `json:"started_at"`
	UpdatedAt  string `json:"updated_at"`
}

// formulaVarDefResponse is one declared variable on a formula.
type formulaVarDefResponse struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Default     any      `json:"default,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	Pattern     string   `json:"pattern,omitempty"`
}

// formulaSummaryResponse is the list-entry shape for GET formulas.
type formulaSummaryResponse struct {
	Name        string                     `json:"name"`
	Description string                     `json:"description"`
	Version     string                     `json:"version"`
	VarDefs     []formulaVarDefResponse    `json:"var_defs"`
	RunCount    int                        `json:"run_count"`
	RecentRuns  []formulaRecentRunResponse `json:"recent_runs"`
}

// formulaRunsResponse is the body for GET formulas/{name}/runs.
type formulaRunsResponse struct {
	Formula       string                     `json:"formula"`
	RunCount      int                        `json:"run_count"`
	RecentRuns    []formulaRecentRunResponse `json:"recent_runs"`
	Partial       bool                       `json:"partial"`
	PartialErrors []string                   `json:"partial_errors,omitempty"`
}

// formulaPreviewNodeResponse is one node in a compiled-formula preview.
type formulaPreviewNodeResponse struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Kind     string `json:"kind"`
	ScopeRef string `json:"scope_ref,omitempty"`
}

// formulaPreviewEdgeResponse is one edge in a compiled-formula preview.
type formulaPreviewEdgeResponse struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind,omitempty"`
}

// FormulaStepResponse is one step in a formula detail response. The
// wire fields are uniform across step kinds; the Kind discriminator
// carries the step variant (sling, converge, wait, subflow, etc.) and
// Metadata carries per-kind extras as a string-keyed string-valued
// dictionary.
type FormulaStepResponse struct {
	ID       string            `json:"id"`
	Title    string            `json:"title"`
	Kind     string            `json:"kind"`
	Type     string            `json:"type,omitempty"`
	Assignee string            `json:"assignee,omitempty"`
	Labels   []string          `json:"labels,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// formulaDetailResponse is the body for GET formula/{name}.
type formulaDetailResponse struct {
	Name        string                       `json:"name"`
	Description string                       `json:"description"`
	Version     string                       `json:"version"`
	VarDefs     []formulaVarDefResponse      `json:"var_defs"`
	Steps       []FormulaStepResponse        `json:"steps"`
	Deps        []formulaPreviewEdgeResponse `json:"deps"`
	Preview     FormulaPreviewResponse       `json:"preview"`
}

// FormulaPreviewResponse is the compiled-formula graph preview returned with
// a formula detail response.
type FormulaPreviewResponse struct {
	Nodes []formulaPreviewNodeResponse `json:"nodes"`
	Edges []formulaPreviewEdgeResponse `json:"edges"`
}

// --- Formula types ---

// FormulaFeedInput is the Huma input for GET /v0/city/{cityName}/formulas/feed.
type FormulaFeedInput struct {
	CityScope
	ScopeKind string `query:"scope_kind" required:"false" doc:"Scope kind (city or rig)."`
	ScopeRef  string `query:"scope_ref" required:"false" doc:"Scope reference."`
	Limit     int    `query:"limit" required:"false" minimum:"0" doc:"Maximum number of feed items to return. 0 = default."`
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
	Limit     int    `query:"limit" required:"false" minimum:"0" doc:"Maximum number of recent runs to return. 0 = default."`
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
	Delete     bool   `query:"delete" required:"false" doc:"Permanently delete beads from store."`
}

