package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

// FormulaListBody is the response body for GET /v0/formulas.
type FormulaListBody struct {
	Items   []formulaSummaryResponse `json:"items" doc:"Formula summaries."`
	Partial bool                     `json:"partial" doc:"Whether the list is partial."`
}

// FormulaListOutput is the response envelope for GET /v0/formulas.
type FormulaListOutput struct {
	Body FormulaListBody
}

// humaHandleFormulaList is the Huma-typed handler for GET /v0/formulas.
func (s *Server) humaHandleFormulaList(_ context.Context, input *FormulaListInput) (*FormulaListOutput, error) {
	scopeKind, scopeRef, scopeErr := parseWorkflowRequestScope(input.ScopeKind, input.ScopeRef)
	if scopeErr != "" {
		return nil, huma.Error400BadRequest(scopeErr)
	}

	paths, status, msg := s.formulaSearchPaths(scopeKind, scopeRef)
	if status != 200 {
		if status == 404 {
			return nil, huma.Error404NotFound(msg)
		}
		if status == 503 {
			return nil, huma.Error503ServiceUnavailable(msg)
		}
		return nil, huma.Error400BadRequest(msg)
	}

	items, err := buildFormulaCatalog(paths)
	if err != nil {
		return nil, huma.Error500InternalServerError("formula catalog failed")
	}

	out := &FormulaListOutput{}
	out.Body.Items = items
	out.Body.Partial = false
	return out, nil
}

// humaHandleFormulaRuns is the Huma-typed handler for GET /v0/formulas/{name}/runs.
func (s *Server) humaHandleFormulaRuns(_ context.Context, input *FormulaRunsInput) (*struct {
	Body formulaRunsResponse
}, error,
) {
	// Name non-empty-whitespace is enforced by minLength + pattern on FormulaRunsInput.
	name := input.Name

	scopeKind, scopeRef, scopeErr := parseWorkflowRequestScope(input.ScopeKind, input.ScopeRef)
	if scopeErr != "" {
		return nil, huma.Error400BadRequest(scopeErr)
	}
	if _, status, msg := s.formulaSearchPaths(scopeKind, scopeRef); status != 200 {
		if status == 404 {
			return nil, huma.Error404NotFound(msg)
		}
		if status == 503 {
			return nil, huma.Error503ServiceUnavailable(msg)
		}
		return nil, huma.Error400BadRequest(msg)
	}

	limit := defaultFormulaRunsLimit
	if input.Limit > 0 {
		limit = normalizeFormulaRunsLimit(input.Limit)
	}

	resp, err := buildFormulaRuns(s.state, name, scopeKind, scopeRef, limit)
	if err != nil {
		return nil, huma.Error500InternalServerError("formula runs failed")
	}

	return &struct {
		Body formulaRunsResponse
	}{Body: *resp}, nil
}

// humaHandleFormulaDetail is the Huma-typed handler for GET /v0/formulas/{name}
// and GET /v0/formula/{name}. Returns a compiled preview with declared
// variables at their defaults. Callers that need to supply variable
// values use humaHandleFormulaPreview (POST /preview) so the variable
// dictionary is a spec-visible typed body.
//
// Deprecation note: older clients used `GET ?var.<name>=<value>` query
// params to supply variable values. Those values are now ignored. We detect
// legacy callers by scanning the raw request URL via the FormulaDetailInput
// resolver and return 400 with a migration hint, rather than silently
// returning a default-substituted preview the caller thinks is customized.
func (s *Server) humaHandleFormulaDetail(ctx context.Context, input *FormulaDetailInput) (*struct {
	Body formulaDetailResponse
}, error,
) {
	return s.formulaDetail(ctx, input.Name, input.ScopeKind, input.ScopeRef, input.Target, nil, false)
}

// humaHandleFormulaPreview is the Huma-typed handler for
// POST /v0/city/{cityName}/formulas/{name}/preview. It accepts a typed
// body carrying the variable dictionary so the preview inputs are
// fully described by the OpenAPI spec.
func (s *Server) humaHandleFormulaPreview(ctx context.Context, input *FormulaPreviewInput) (*struct {
	Body formulaDetailResponse
}, error,
) {
	return s.formulaDetail(ctx, input.Name, input.Body.ScopeKind, input.Body.ScopeRef, input.Body.Target, input.Body.Vars, true)
}

// formulaDetail is the shared backing implementation for the GET detail
// and POST preview endpoints. The two endpoints differ only in how they
// receive the variable dictionary: GET compiles with defaults, POST
// accepts a caller-supplied map.
func (s *Server) formulaDetail(ctx context.Context, rawName, rawScopeKind, rawScopeRef, rawTarget string, vars map[string]string, validateRuntimeVars bool) (*struct {
	Body formulaDetailResponse
}, error,
) {
	name := strings.TrimSpace(rawName)
	if name == "" {
		return nil, huma.Error400BadRequest("formula name is required")
	}

	scopeKind, scopeRef, scopeErr := parseWorkflowRequestScope(rawScopeKind, rawScopeRef)
	if scopeErr != "" {
		return nil, huma.Error400BadRequest(scopeErr)
	}
	target := strings.TrimSpace(rawTarget)
	if target == "" {
		return nil, huma.Error400BadRequest("target is required")
	}

	paths, status, msg := s.formulaSearchPaths(scopeKind, scopeRef)
	if status != 200 {
		if status == 404 {
			return nil, huma.Error404NotFound(msg)
		}
		if status == 503 {
			return nil, huma.Error503ServiceUnavailable(msg)
		}
		return nil, huma.Error400BadRequest(msg)
	}

	detail, err := buildFormulaDetail(ctx, name, paths, target, vars, validateRuntimeVars)
	if err != nil {
		if errors.Is(err, errFormulaNotWorkflow) || errors.Is(err, errFormulaNotFound) {
			return nil, huma.Error404NotFound(err.Error())
		}
		return nil, huma.Error400BadRequest(err.Error())
	}

	return &struct {
		Body formulaDetailResponse
	}{Body: *detail}, nil
}

// formulaFeedBody is the response body for GET /v0/formulas/feed.
type formulaFeedBody struct {
	Items         []monitorFeedItemResponse `json:"items"`
	Partial       bool                      `json:"partial"`
	PartialErrors []string                  `json:"partial_errors,omitempty"`
}

// humaHandleFormulaFeed is the Huma-typed handler for GET /v0/formulas/feed.
func (s *Server) humaHandleFormulaFeed(_ context.Context, input *FormulaFeedInput) (*struct {
	Body formulaFeedBody
}, error,
) {
	scopeKind, scopeRef, scopeErr := parseWorkflowRequestScope(input.ScopeKind, input.ScopeRef)
	if scopeErr != "" {
		return nil, huma.Error400BadRequest(scopeErr)
	}
	if _, status, msg := s.formulaSearchPaths(scopeKind, scopeRef); status != http.StatusOK {
		if status == http.StatusNotFound {
			return nil, huma.Error404NotFound(msg)
		}
		if status == http.StatusServiceUnavailable {
			return nil, huma.Error503ServiceUnavailable(msg)
		}
		return nil, huma.Error400BadRequest(msg)
	}

	limit := normalizeFeedLimit(input.Limit)
	index := s.latestIndex()

	cacheKey := "formula-feed?" + scopeKind + "|" + scopeRef + "|" + strconv.Itoa(input.Limit)
	if body, ok := cachedResponseAs[formulaFeedBody](s, cacheKey, index); ok {
		return &struct {
			Body formulaFeedBody
		}{Body: body}, nil
	}

	projections, err := buildWorkflowRunProjectionsRootOnly(s.state, scopeKind, scopeRef)
	if err != nil {
		return nil, huma.Error500InternalServerError("formula feed failed")
	}

	items := make([]monitorFeedItemResponse, 0, len(projections.Items))
	for _, run := range projections.Items {
		items = append(items, workflowRunProjectionFeedItem(run))
	}
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}

	body := formulaFeedBody{
		Items:   items,
		Partial: projections.Partial,
	}
	if len(projections.PartialErrors) > 0 {
		body.PartialErrors = projections.PartialErrors
	}

	s.storeResponse(cacheKey, index, body)

	return &struct {
		Body formulaFeedBody
	}{Body: body}, nil
}
