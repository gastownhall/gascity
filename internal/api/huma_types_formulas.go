package api

import (
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

// Per-domain Huma input/output types for the formulas handler
// group. Split out of the original huma_types.go; mirrors the layout
// of huma_handlers_formulas.go.

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
//
// This endpoint returns a compiled preview with declared variables at
// their defaults. Callers that need to supply variable values use
// POST /v0/city/{cityName}/formulas/{name}/preview (FormulaPreviewInput)
// so the variable dictionary is a spec-visible typed body rather than
// a dynamic wildcard query scheme. See architecture.md §3.5.1.
type FormulaDetailInput struct {
	CityScope
	Name      string `path:"name" doc:"Formula name."`
	ScopeKind string `query:"scope_kind" required:"false" doc:"Scope kind (city or rig)."`
	ScopeRef  string `query:"scope_ref" required:"false" doc:"Scope reference."`
	Target    string `query:"target" required:"true" doc:"Target agent for preview compilation."`
}

// Resolve rejects legacy `var.<name>=<value>` query parameters with a
// 400 + migration hint so silent-ignore does not mask a bookmark or
// curl script that expects variable substitution.
func (i *FormulaDetailInput) Resolve(ctx huma.Context, _ *huma.PathBuffer) []error {
	u := ctx.URL()
	for name := range u.Query() {
		if strings.HasPrefix(name, "var.") {
			return []error{&huma.ErrorDetail{
				Location: "query." + name,
				Message: "GET formulas/{name} no longer accepts var.* query parameters; " +
					"use POST formulas/{name}/preview with a typed vars body instead",
			}}
		}
	}
	return nil
}

// FormulaPreviewBody is the request body for POST /v0/city/{cityName}/formulas/{name}/preview.
//
// Supplying variable values via a typed map on the body keeps the
// input surface spec-visible. A prior revision accepted dynamic
// var.* query parameters via a huma.Resolver; that scheme was
// removed because OpenAPI 3.1 cannot describe wildcard query keys.
// See architecture.md §3.5.1.
type FormulaPreviewBody struct {
	ScopeKind string            `json:"scope_kind,omitempty" doc:"Scope kind (city or rig)."`
	ScopeRef  string            `json:"scope_ref,omitempty" doc:"Scope reference."`
	Target    string            `json:"target" minLength:"1" doc:"Target agent for preview compilation."`
	Vars      map[string]string `json:"vars,omitempty" doc:"Variable name-to-value overrides applied to the compiled preview."`
}

// FormulaPreviewInput is the Huma input for POST /v0/city/{cityName}/formulas/{name}/preview.
type FormulaPreviewInput struct {
	CityScope
	Name string `path:"name" doc:"Formula name."`
	Body FormulaPreviewBody
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
