package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
)

var errFormulaNotWorkflow = errors.New("formula is not a workflow")

type formulaRecentRunResponse struct {
	WorkflowID string `json:"workflow_id"`
	Status     string `json:"status"`
	Target     string `json:"target"`
	StartedAt  string `json:"started_at"`
	UpdatedAt  string `json:"updated_at"`
}

type formulaVarDefResponse struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Default     any      `json:"default,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	Pattern     string   `json:"pattern,omitempty"`
}

type formulaSummaryResponse struct {
	Name        string                     `json:"name"`
	Description string                     `json:"description"`
	Version     string                     `json:"version"`
	VarDefs     []formulaVarDefResponse    `json:"var_defs"`
	RunCount    int                        `json:"run_count"`
	RecentRuns  []formulaRecentRunResponse `json:"recent_runs"`
}

type formulaRunsResponse struct {
	Formula       string                     `json:"formula"`
	RunCount      int                        `json:"run_count"`
	RecentRuns    []formulaRecentRunResponse `json:"recent_runs"`
	Partial       bool                       `json:"partial"`
	PartialErrors []string                   `json:"partial_errors,omitempty"`
}

const (
	defaultFormulaRunsLimit = 3
	maxFormulaRunsLimit     = 20
)

type formulaPreviewNodeResponse struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Kind     string `json:"kind"`
	ScopeRef string `json:"scope_ref,omitempty"`
}

type formulaPreviewEdgeResponse struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind,omitempty"`
}

type formulaDetailResponse struct {
	Name        string                       `json:"name"`
	Description string                       `json:"description"`
	Version     string                       `json:"version"`
	VarDefs     []formulaVarDefResponse      `json:"var_defs"`
	Steps       []map[string]any             `json:"steps"`
	Deps        []formulaPreviewEdgeResponse `json:"deps"`
	Preview     struct {
		Nodes []formulaPreviewNodeResponse `json:"nodes"`
		Edges []formulaPreviewEdgeResponse `json:"edges"`
	} `json:"preview"`
}

func (s *Server) handleFormulaList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	scopeKind, scopeRef, scopeErr := parseWorkflowRequestScope(q.Get("scope_kind"), q.Get("scope_ref"))
	if scopeErr != "" {
		writeError(w, http.StatusBadRequest, "invalid", scopeErr)
		return
	}

	paths, status, code, msg := s.formulaSearchPaths(scopeKind, scopeRef)
	if status != http.StatusOK {
		writeError(w, status, code, msg)
		return
	}

	items, err := buildFormulaCatalog(paths)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "formula catalog failed")
		return
	}

	resp := map[string]any{
		"items":   items,
		"partial": false,
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleFormulaRuns(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid", "formula name is required")
		return
	}

	q := r.URL.Query()
	scopeKind, scopeRef, scopeErr := parseWorkflowRequestScope(q.Get("scope_kind"), q.Get("scope_ref"))
	if scopeErr != "" {
		writeError(w, http.StatusBadRequest, "invalid", scopeErr)
		return
	}
	if _, status, code, msg := s.formulaSearchPaths(scopeKind, scopeRef); status != http.StatusOK {
		writeError(w, status, code, msg)
		return
	}
	limit := defaultFormulaRunsLimit
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, "invalid", "limit must be a non-negative integer")
			return
		}
		limit = normalizeFormulaRunsLimit(parsed)
	}

	resp, err := buildFormulaRuns(s.state, name, scopeKind, scopeRef, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "formula runs failed")
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleFormulaDetail(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid", "formula name is required")
		return
	}

	q := r.URL.Query()
	scopeKind, scopeRef, scopeErr := parseWorkflowRequestScope(q.Get("scope_kind"), q.Get("scope_ref"))
	if scopeErr != "" {
		writeError(w, http.StatusBadRequest, "invalid", scopeErr)
		return
	}
	target := strings.TrimSpace(q.Get("target"))
	if target == "" {
		writeError(w, http.StatusBadRequest, "invalid", "target is required")
		return
	}

	paths, status, code, msg := s.formulaSearchPaths(scopeKind, scopeRef)
	if status != http.StatusOK {
		writeError(w, status, code, msg)
		return
	}

	detail, err := buildFormulaDetail(r.Context(), name, paths, target, queryFormulaVars(q))
	if err != nil {
		if errors.Is(err, errFormulaNotWorkflow) || strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, detail)
}

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
	cityScopeRef := workflowCityScopeRef(state.CityName())
	includeAllForCity := requestedScopeKind == "city" && requestedScopeRef == cityScopeRef
	stores := workflowStores(state)
	projections := make([]workflowRunProjection, 0)
	partialErrors := make([]string, 0)
	var requestedScopeErr error

	for _, info := range stores {
		if info.store == nil {
			continue
		}
		if !includeAllForCity && (info.scopeKind != requestedScopeKind || info.scopeRef != requestedScopeRef) {
			continue
		}

		openBeads, err := info.store.ListOpen()
		if err != nil {
			if requestedScopeErr == nil && info.scopeKind == requestedScopeKind && info.scopeRef == requestedScopeRef {
				requestedScopeErr = err
			}
			if includeAllForCity {
				msg := info.ref + " store unavailable"
				log.Printf("api: formula runs open list failed for %s: %v", info.ref, err)
				partialErrors = append(partialErrors, msg)
			}
			continue
		}

		openChildrenByRoot := make(map[string][]beads.Bead)
		for _, bead := range openBeads {
			rootID := strings.TrimSpace(bead.Metadata["gc.root_bead_id"])
			if rootID == "" {
				continue
			}
			openChildrenByRoot[rootID] = append(openChildrenByRoot[rootID], bead)
		}

		roots, err := info.store.ListByMetadata(map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		}, 0, beads.IncludeClosed)
		if err != nil {
			log.Printf("api: formula runs workflow root list failed for %s: %v", info.ref, err)
			partialErrors = append(partialErrors, info.ref+" workflow history incomplete")
			roots = nil
			for _, bead := range openBeads {
				if isWorkflowRoot(bead) && strings.TrimSpace(bead.Metadata["gc.formula_contract"]) == "graph.v2" {
					roots = append(roots, bead)
				}
			}
		}

		for _, root := range roots {
			if !isWorkflowRoot(root) || workflowFormulaName(root) != formulaName {
				continue
			}

			scopeKind, scopeRef := workflowProjectionScope(info, root, cityScopeRef, requestedScopeKind, requestedScopeRef)
			if scopeKind != requestedScopeKind || scopeRef != requestedScopeRef {
				continue
			}

			runBeads := append([]beads.Bead{root}, openChildrenByRoot[root.ID]...)
			children, childErr := info.store.ListByMetadata(map[string]string{"gc.root_bead_id": root.ID}, 0, beads.IncludeClosed)
			if childErr != nil {
				log.Printf("api: formula runs child list failed for %s root %s: %v", info.ref, root.ID, childErr)
				partialErrors = append(partialErrors, root.ID+" workflow history incomplete")
			} else {
				seen := make(map[string]bool, len(runBeads))
				for _, existing := range runBeads {
					seen[existing.ID] = true
				}
				for _, child := range children {
					if seen[child.ID] {
						continue
					}
					runBeads = append(runBeads, child)
				}
			}

			projections = append(projections, workflowRunProjection{
				WorkflowID:     resolvedWorkflowID(root),
				FormulaName:    workflowFormulaName(root),
				Title:          workflowProjectionTitle(root),
				Status:         normalizeMonitorStatus(aggregateWorkflowRunStatus(root, runBeads)),
				Target:         workflowProjectionTarget(root),
				StartedAt:      root.CreatedAt,
				UpdatedAt:      workflowProjectionUpdatedAt(runBeads),
				ScopeKind:      scopeKind,
				ScopeRef:       scopeRef,
				RootBeadID:     root.ID,
				RootStoreRef:   info.ref,
				AttachedBeadID: strings.TrimSpace(root.Metadata["gc.source_bead_id"]),
			})
		}
	}

	if len(projections) == 0 && requestedScopeErr != nil && !includeAllForCity {
		return nil, fmt.Errorf("listing open beads for %s:%s: %w", requestedScopeKind, requestedScopeRef, requestedScopeErr)
	}

	return &formulaRunsResponse{
		Formula:       formulaName,
		RunCount:      formulaRunCountFor(formulaName, projections),
		RecentRuns:    formulaRecentRunsFor(formulaName, projections, limit),
		Partial:       len(partialErrors) > 0,
		PartialErrors: partialErrors,
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
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".formula.toml") {
				continue
			}
			name := strings.TrimSuffix(entry.Name(), ".formula.toml")
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
	case "scope-check", "workflow-finalize":
		return false
	default:
		return true
	}
}
