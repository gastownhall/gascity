package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

// humaHandleFormulaList is the Huma-typed handler for GET /v0/formulas.
func (s *Server) humaHandleFormulaList(_ context.Context, input *FormulaListInput) (*struct {
	Body struct {
		Items   []formulaSummaryResponse `json:"items"`
		Partial bool                     `json:"partial"`
	}
}, error) {
	scopeKind, scopeRef, scopeErr := parseWorkflowRequestScope(input.ScopeKind, input.ScopeRef)
	if scopeErr != "" {
		return nil, huma.Error400BadRequest(scopeErr)
	}

	paths, status, _, msg := s.formulaSearchPaths(scopeKind, scopeRef)
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

	out := &struct {
		Body struct {
			Items   []formulaSummaryResponse `json:"items"`
			Partial bool                     `json:"partial"`
		}
	}{}
	out.Body.Items = items
	out.Body.Partial = false
	return out, nil
}

// humaHandleFormulaRuns is the Huma-typed handler for GET /v0/formulas/{name}/runs.
func (s *Server) humaHandleFormulaRuns(_ context.Context, input *FormulaRunsInput) (*struct {
	Body formulaRunsResponse
}, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return nil, huma.Error400BadRequest("formula name is required")
	}

	scopeKind, scopeRef, scopeErr := parseWorkflowRequestScope(input.ScopeKind, input.ScopeRef)
	if scopeErr != "" {
		return nil, huma.Error400BadRequest(scopeErr)
	}
	if _, status, _, msg := s.formulaSearchPaths(scopeKind, scopeRef); status != 200 {
		if status == 404 {
			return nil, huma.Error404NotFound(msg)
		}
		if status == 503 {
			return nil, huma.Error503ServiceUnavailable(msg)
		}
		return nil, huma.Error400BadRequest(msg)
	}

	limit := defaultFormulaRunsLimit
	if raw := strings.TrimSpace(input.Limit); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			return nil, huma.Error400BadRequest("limit must be a non-negative integer")
		}
		limit = normalizeFormulaRunsLimit(parsed)
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
// and GET /v0/formula/{name}. Supports dynamic var.* query params via
// the Resolve interface on FormulaDetailInput.
func (s *Server) humaHandleFormulaDetail(ctx context.Context, input *FormulaDetailInput) (*struct {
	Body formulaDetailResponse
}, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return nil, huma.Error400BadRequest("formula name is required")
	}

	scopeKind, scopeRef, scopeErr := parseWorkflowRequestScope(input.ScopeKind, input.ScopeRef)
	if scopeErr != "" {
		return nil, huma.Error400BadRequest(scopeErr)
	}
	target := strings.TrimSpace(input.Target)
	if target == "" {
		return nil, huma.Error400BadRequest("target is required")
	}

	paths, status, _, msg := s.formulaSearchPaths(scopeKind, scopeRef)
	if status != 200 {
		if status == 404 {
			return nil, huma.Error404NotFound(msg)
		}
		if status == 503 {
			return nil, huma.Error503ServiceUnavailable(msg)
		}
		return nil, huma.Error400BadRequest(msg)
	}

	detail, err := buildFormulaDetail(ctx, name, paths, target, input.vars)
	if err != nil {
		if errors.Is(err, errFormulaNotWorkflow) || strings.Contains(err.Error(), "not found") {
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
}, error) {
	scopeKind, scopeRef, scopeErr := parseWorkflowRequestScope(input.ScopeKind, input.ScopeRef)
	if scopeErr != "" {
		return nil, huma.Error400BadRequest(scopeErr)
	}
	if _, status, _, msg := s.formulaSearchPaths(scopeKind, scopeRef); status != http.StatusOK {
		if status == http.StatusNotFound {
			return nil, huma.Error404NotFound(msg)
		}
		if status == http.StatusServiceUnavailable {
			return nil, huma.Error503ServiceUnavailable(msg)
		}
		return nil, huma.Error400BadRequest(msg)
	}

	limit := parseOrdersFeedLimit(input.Limit)
	index := s.latestIndex()

	cacheKey := "formula-feed?" + scopeKind + "|" + scopeRef + "|" + input.Limit
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
