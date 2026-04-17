package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/formula"
)

var errFormulaNotWorkflow = errors.New("formula is not a workflow")

// Response types (formulaDetailResponse, formulaSummaryResponse,
// formulaRunsResponse, and the formulaPreview* / formulaVarDef /
// formulaRecentRun building blocks) live in huma_types_formulas.go so
// every response-body struct has one canonical home. This file
// contains only the dispatch helpers that populate them.

const (
	defaultFormulaRunsLimit = 3
	maxFormulaRunsLimit     = 20
)

func (s *Server) formulaSearchPaths(scopeKind, scopeRef string) ([]string, int, string, string) {
	cfg := s.state.Config()
	if cfg == nil {
		return nil, http.StatusServiceUnavailable, "unavailable", "config is unavailable"
	}

	switch scopeKind {
	case "city":
		if scopeRef != strings.TrimSpace(s.state.CityName()) {
			return nil, http.StatusNotFound, "not_found", "city scope " + scopeRef + " not found"
		}
		return cfg.FormulaLayers.City, http.StatusOK, "", ""
	case "rig":
		if s.state.BeadStore(scopeRef) == nil {
			return nil, http.StatusNotFound, "not_found", "rig scope " + scopeRef + " not found"
		}
		return cfg.FormulaLayers.SearchPaths(scopeRef), http.StatusOK, "", ""
	default:
		return nil, http.StatusBadRequest, "invalid", "scope_kind must be 'city' or 'rig'"
	}
}

func buildFormulaCatalog(paths []string) ([]formulaSummaryResponse, error) {
	if len(paths) == 0 {
		return []formulaSummaryResponse{}, nil
	}
	names := discoverFormulaNames(paths)
	parser := formula.NewParser(paths...)
	items := make([]formulaSummaryResponse, 0, len(names))
	for _, name := range names {
		resolved, err := loadResolvedWorkflowFormula(parser, name)
		if err != nil {
			if errors.Is(err, errFormulaNotWorkflow) {
				continue
			}
			return nil, err
		}
		items = append(items, formulaSummaryResponse{
			Name:        resolved.Formula,
			Description: resolved.Description,
			Version:     formulaVersionString(resolved),
			VarDefs:     formulaVarDefs(resolved.Vars),
			RunCount:    0,
			RecentRuns:  []formulaRecentRunResponse{},
		})
	}
	return items, nil
}

func formulaRunCountFor(name string, runs []workflowRunProjection) int {
	count := 0
	for _, run := range runs {
		if run.FormulaName == name {
			count++
		}
	}
	return count
}

func formulaRecentRunsFor(name string, runs []workflowRunProjection, limit int) []formulaRecentRunResponse {
	if limit <= 0 {
		return []formulaRecentRunResponse{}
	}

	capHint := limit
	if len(runs) < capHint {
		capHint = len(runs)
	}
	matching := make([]workflowRunProjection, 0, capHint)
	for _, run := range runs {
		if run.FormulaName != name {
			continue
		}
		matching = append(matching, run)
	}

	sort.SliceStable(matching, func(i, j int) bool {
		if !matching[i].UpdatedAt.Equal(matching[j].UpdatedAt) {
			return matching[i].UpdatedAt.After(matching[j].UpdatedAt)
		}
		return matching[i].StartedAt.After(matching[j].StartedAt)
	})

	if len(matching) > limit {
		matching = matching[:limit]
	}

	items := make([]formulaRecentRunResponse, 0, len(matching))
	for _, run := range matching {
		items = append(items, formulaRecentRunResponse{
			WorkflowID: run.WorkflowID,
			Status:     run.Status,
			Target:     run.Target,
			StartedAt:  run.StartedAt.Format(time.RFC3339),
			UpdatedAt:  run.UpdatedAt.Format(time.RFC3339),
		})
	}
	return items
}

func normalizeFormulaRunsLimit(limit int) int {
	if limit <= 0 {
		return 0
	}
	if limit > maxFormulaRunsLimit {
		return maxFormulaRunsLimit
	}
	return limit
}

func buildFormulaRuns(state State, formulaName, requestedScopeKind, requestedScopeRef string, limit int) (*formulaRunsResponse, error) {
	// Use the full projection path (with per-root child lookups) so that
	// status and UpdatedAt reflect closed children.  The /feed endpoint
	// intentionally uses the cheaper root-only path for monitor views.
	// Pass formulaName to skip child lookups for non-matching roots.
	projectionResult, err := buildWorkflowRunProjections(state, requestedScopeKind, requestedScopeRef, formulaName)
	if err != nil {
		return nil, fmt.Errorf("listing workflow runs for %s:%s: %w", requestedScopeKind, requestedScopeRef, err)
	}

	projections := make([]workflowRunProjection, 0, len(projectionResult.Items))
	for _, projection := range projectionResult.Items {
		if projection.FormulaName != formulaName {
			continue
		}
		if projection.ScopeKind != requestedScopeKind || projection.ScopeRef != requestedScopeRef {
			continue
		}
		projections = append(projections, projection)
	}

	return &formulaRunsResponse{
		Formula:       formulaName,
		RunCount:      formulaRunCountFor(formulaName, projections),
		RecentRuns:    formulaRecentRunsFor(formulaName, projections, limit),
		Partial:       projectionResult.Partial,
		PartialErrors: projectionResult.PartialErrors,
	}, nil
}

func buildFormulaDetail(ctx context.Context, name string, paths []string, _ string, vars map[string]string) (*formulaDetailResponse, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("formula %q not found in search paths", name)
	}
	parser := formula.NewParser(paths...)
	resolved, err := loadResolvedWorkflowFormula(parser, name)
	if err != nil {
		return nil, err
	}
	recipe, err := formula.Compile(ctx, name, paths, vars)
	if err != nil {
		return nil, err
	}
	displayVars := formula.ApplyDefaults(resolved, vars)

	rootID := ""
	if root := recipe.RootStep(); root != nil {
		rootID = root.ID
	}
	steps := make([]map[string]any, 0, len(recipe.Steps))
	nodes := make([]formulaPreviewNodeResponse, 0, len(recipe.Steps))
	included := make(map[string]bool, len(recipe.Steps))
	for _, step := range recipe.Steps {
		if !includeFormulaPreviewStep(step, rootID) {
			continue
		}
		included[step.ID] = true
		kind := recipeStepKind(step)
		title := formula.Substitute(step.Title, displayVars)
		item := map[string]any{
			"id":    step.ID,
			"title": title,
			"kind":  kind,
		}
		if step.Type != "" {
			item["type"] = step.Type
		}
		if step.Assignee != "" {
			item["assignee"] = step.Assignee
		}
		if len(step.Labels) > 0 {
			item["labels"] = step.Labels
		}
		if len(step.Metadata) > 0 {
			item["metadata"] = step.Metadata
		}
		steps = append(steps, item)

		node := formulaPreviewNodeResponse{
			ID:    step.ID,
			Title: title,
			Kind:  kind,
		}
		if scopeRef := strings.TrimSpace(step.Metadata["gc.scope_ref"]); scopeRef != "" {
			node.ScopeRef = scopeRef
		}
		nodes = append(nodes, node)
	}

	edges := make([]formulaPreviewEdgeResponse, 0, len(recipe.Deps))
	for _, dep := range recipe.Deps {
		if dep.Type == "parent-child" || !included[dep.StepID] || !included[dep.DependsOnID] {
			continue
		}
		edge := formulaPreviewEdgeResponse{
			From: dep.DependsOnID,
			To:   dep.StepID,
		}
		if dep.Type != "" {
			edge.Kind = dep.Type
		}
		edges = append(edges, edge)
	}

	resp := &formulaDetailResponse{
		Name:        resolved.Formula,
		Description: formula.Substitute(resolved.Description, displayVars),
		Version:     formulaVersionString(resolved),
		VarDefs:     formulaVarDefs(resolved.Vars),
		Steps:       steps,
		Deps:        edges,
	}
	resp.Preview.Nodes = nodes
	resp.Preview.Edges = edges
	return resp, nil
}

func discoverFormulaNames(paths []string) []string {
	winners := make(map[string]struct{})
	for _, dir := range paths {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name, ok := formula.TrimTOMLFilename(entry.Name())
			if !ok {
				continue
			}
			winners[name] = struct{}{}
		}
	}

	names := make([]string, 0, len(winners))
	for name := range winners {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func loadResolvedWorkflowFormula(parser *formula.Parser, name string) (*formula.Formula, error) {
	loaded, err := parser.LoadByName(name)
	if err != nil {
		return nil, err
	}
	resolved, err := parser.Resolve(loaded)
	if err != nil {
		return nil, err
	}
	if resolved.Type != formula.TypeWorkflow {
		return nil, fmt.Errorf("%q: %w", name, errFormulaNotWorkflow)
	}
	return resolved, nil
}

func formulaVersionString(f *formula.Formula) string {
	if f == nil || f.Version <= 0 {
		return "1"
	}
	return strconv.Itoa(f.Version)
}

func formulaVarDefs(vars map[string]*formula.VarDef) []formulaVarDefResponse {
	if len(vars) == 0 {
		return []formulaVarDefResponse{}
	}
	names := make([]string, 0, len(vars))
	for name := range vars {
		names = append(names, name)
	}
	sort.Strings(names)

	items := make([]formulaVarDefResponse, 0, len(names))
	for _, name := range names {
		def := vars[name]
		if def == nil {
			continue
		}
		item := formulaVarDefResponse{
			Name:        name,
			Type:        def.Type,
			Description: def.Description,
			Required:    def.Required,
			Enum:        append([]string(nil), def.Enum...),
			Pattern:     def.Pattern,
		}
		if item.Type == "" {
			item.Type = "string"
		}
		if def.Default != nil {
			item.Default = *def.Default
		}
		items = append(items, item)
	}
	return items
}

func queryFormulaVars(q map[string][]string) map[string]string {
	if len(q) == 0 {
		return nil
	}
	out := make(map[string]string)
	for key, values := range q {
		if !strings.HasPrefix(key, "var.") || len(values) == 0 {
			continue
		}
		name := strings.TrimPrefix(key, "var.")
		if name == "" {
			continue
		}
		out[name] = values[len(values)-1]
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func recipeStepKind(step formula.RecipeStep) string {
	if kind := strings.TrimSpace(step.Metadata["gc.kind"]); kind != "" {
		return kind
	}
	if step.Type != "" {
		return step.Type
	}
	return "task"
}

func includeFormulaPreviewStep(step formula.RecipeStep, rootID string) bool {
	if step.ID == rootID {
		return false
	}
	switch strings.TrimSpace(step.Metadata["gc.kind"]) {
	case "scope-check", "workflow-finalize", "spec":
		return false
	default:
		return true
	}
}
