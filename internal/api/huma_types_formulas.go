package api

// Per-domain Huma input/output types for the formulas handler
// group. Split out of the original huma_types.go; mirrors the layout
// of huma_handlers_formulas.go.

import (
	"github.com/danielgtaylor/huma/v2"
)

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

