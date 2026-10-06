package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/execenv"
	gitpkg "github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/sling"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

type slingBody struct {
	Rig            string            `json:"rig"`
	Target         string            `json:"target"`
	Bead           string            `json:"bead"`
	Formula        string            `json:"formula"`
	AttachedBeadID string            `json:"attached_bead_id"`
	Title          string            `json:"title"`
	Vars           map[string]string `json:"vars"`
	ScopeKind      string            `json:"scope_kind"`
	ScopeRef       string            `json:"scope_ref"`
	Force          bool              `json:"force"`
	Reassign       bool              `json:"reassign"`
	Merge          string            `json:"merge"`
	NoConvoy       bool              `json:"no_convoy"`
	Owned          bool              `json:"owned"`
	NoFormula      bool              `json:"no_formula"`
}

// Values of the POST /sling response status.
const (
	// SlingStatusSlung reports that the sling succeeded.
	SlingStatusSlung = "slung"
	// SlingStatusPartial reports a convoy sling where some children routed and
	// others failed; batch carries the counts and each failure.
	SlingStatusPartial = "partial"
)

type slingResponse struct {
	Status         string             `json:"status" enum:"slung,partial" doc:"slung when the sling succeeded; partial when a convoy's children were routed one by one and some failed. A partial result is not rolled back: the routed children stay routed, and batch.failures names the ones to retry."`
	Target         string             `json:"target"`
	Formula        string             `json:"formula,omitempty"`
	Bead           string             `json:"bead,omitempty"`
	WorkflowID     string             `json:"workflow_id,omitempty"`
	RootBeadID     string             `json:"root_bead_id,omitempty"`
	AttachedBeadID string             `json:"attached_bead_id,omitempty"`
	Mode           string             `json:"mode,omitempty"`
	Warnings       []string           `json:"warnings,omitempty"`
	DashboardURL   string             `json:"dashboard_url,omitempty" doc:"Absolute dashboard deep link for the slung work: the run detail view when a graph workflow was launched, otherwise the runs list. Present only when the serving process also hosts the dashboard (the supervisor listener); the standalone controller API omits it."`
	Run            *RunRef            `json:"run,omitempty" doc:"Reference to the launched run resource, present only when a graph workflow was launched (the same run the Location header addresses)."`
	MoleculeID     string             `json:"molecule_id,omitempty" doc:"Root of the formula wisp attached to the bead, when a non-graph (v1) formula was attached. Matches gc sling --json molecule_id."`
	ConvoyID       string             `json:"convoy_id,omitempty" doc:"Auto-convoy tracking the routed bead, when one was created or reused. Matches gc sling --json convoy_id."`
	Batch          *SlingBatchSummary `json:"batch,omitempty" doc:"Per-child outcome counts, present only when the bead was a convoy whose open children were routed one by one (as gc sling does). Matches gc sling --json batch."`
}

// SlingBatchSummary counts the outcome of a convoy sling that routed each
// open child separately. Field names match gc sling --json.
type SlingBatchSummary struct {
	ContainerType string              `json:"container_type,omitempty" doc:"Container bead type, e.g. convoy."`
	Total         int                 `json:"total" doc:"Children tracked by the container."`
	Routed        int                 `json:"routed" doc:"Children routed by this sling."`
	Failed        int                 `json:"failed" doc:"Children whose routing failed."`
	Skipped       int                 `json:"skipped" doc:"Children skipped: already routed, or not open."`
	Idempotent    int                 `json:"idempotent" doc:"Children skipped because they were already routed to the target."`
	Failures      []SlingChildFailure `json:"failures,omitempty" doc:"Children whose routing failed, with the reason. Present only when failed > 0."`
}

// SlingChildFailure names one convoy child whose routing failed.
type SlingChildFailure struct {
	BeadID string `json:"bead_id" doc:"Child bead ID."`
	Reason string `json:"reason" doc:"Why routing the child failed."`
}

var apiSlingStderr = func() io.Writer { return os.Stderr }

// execSling calls the intent-based Sling API directly. The Huma handler
// humaHandleSling performs all validation before calling this.
//
// Return tuple:
//   - resp: the success body (nil when code != "")
//   - status: HTTP status for the success or error case
//   - code: short error code ("" on success)
//   - message: human-readable error message ("" on success)
//   - conflict: populated when code == "conflict"; carries the blocking
//     source_bead_id, workflow IDs, and cleanup hint the caller needs
//     to render a rich 409 Problem Details body. Returning it out-of-band
//     keeps Huma's structured error path available without widening the
//     (*slingResponse, int, string, string) shape every non-conflict
//     caller already consumes.
func (s *Server) execSling(ctx context.Context, body slingBody, _ string) (*slingResponse, int, string, string, *sourceworkflow.ConflictError) {
	cfg := s.state.Config()
	agentCfg, _ := findAgent(cfg, body.Target)

	formulaName := strings.TrimSpace(body.Formula)
	attachedBeadID := strings.TrimSpace(body.AttachedBeadID)
	storeBeadID := slingStoreBeadID(body)

	// Build deps and construct Sling instance.
	store := s.findSlingStore(body.Rig, agentCfg, storeBeadID)
	storeRef := s.slingStoreRef(body.Rig, agentCfg, storeBeadID)
	if store == nil && allowsForceStoreFallback(body, agentCfg) {
		store = s.findSlingStore(body.Rig, agentCfg, "")
		storeRef = s.slingStoreRef(body.Rig, agentCfg, "")
	}
	if store == nil {
		message := fmt.Sprintf("bead prefix store %s is not registered; cannot verify bead %q", storeRef, storeBeadID)
		return nil, http.StatusBadRequest, "missing_bead", message, nil
	}
	// Mirror the CLI's tolerant source-workflow scan: a non-source rig store
	// whose live-root scan fails degrades to an operator-visible warning
	// instead of aborting the sling. Without this sink the domain keeps every
	// non-source scan failure fatal (internal/sling/sling_core.go), so a single
	// schema-skewed rig store would abort every workflow-launching sling that
	// routes through a running city. Dedup per store ref so one degraded rig
	// warns once per request, and collect the ordered messages so they surface
	// to the caller in the response `warnings` field, not only the server log: a
	// running-city sling routes through this handler, so the invoking human or
	// agent sees only the JSON response and would otherwise be blind to the
	// degraded cross-store conflict coverage.
	sourceWorkflowScanWarnings := make(map[string]struct{})
	var sourceWorkflowScanMessages []string
	deps := sling.SlingDeps{
		CityName:   s.state.CityName(),
		CityPath:   s.state.CityPath(),
		Cfg:        s.state.Config(),
		SP:         s.state.SessionProvider(),
		Store:      store,
		GraphStore: s.relocatedGraphStore(),
		Events:     s.state.EventProvider(),
		StoreRef:   storeRef,
		SourceWorkflowStores: func() ([]sling.SourceWorkflowStore, error) {
			return s.sourceWorkflowStores(), nil
		},
		SourceWorkflowStoreScanWarning: func(scanStoreRef string, scanErr error) {
			key := strings.TrimSpace(scanStoreRef)
			if _, warned := sourceWorkflowScanWarnings[key]; warned {
				return
			}
			sourceWorkflowScanWarnings[key] = struct{}{}
			message := fmt.Sprintf(
				"source-workflow singleton scan skipped unavailable store %s (%v); cross-store roots in that store are invisible",
				scanStoreRef, scanErr)
			sourceWorkflowScanMessages = append(sourceWorkflowScanMessages, message)
			fmt.Fprintf(apiSlingStderr(), "warning: %s\n", message) //nolint:errcheck
		},
		Runner:   s.slingRunner(),
		Router:   apiBeadRouter{server: s, store: store},
		Resolver: apiAgentResolver{},
		Branches: apiBranchResolver{cityPath: s.state.CityPath()},
		Notify:   &apiNotifier{state: s.state},
		Tracer: func(format string, args ...any) {
			fmt.Fprintf(apiSlingStderr(), format+"\n", args...) //nolint:errcheck
		},
	}
	sl, err := sling.New(deps)
	if err != nil {
		return nil, http.StatusInternalServerError, "internal", err.Error(), nil
	}

	// Build vars slice from map (sorted for determinism).
	var varSlice []string
	if len(body.Vars) > 0 {
		keys := make([]string, 0, len(body.Vars))
		for k := range body.Vars {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			varSlice = append(varSlice, k+"="+body.Vars[k])
		}
	}

	// Build the caller's whole intent and hand it to the same domain entry
	// point `gc sling` uses, so the two cannot drift: the target's default
	// formula, title and vars on it, --on, --no-formula and convoy expansion
	// are all decided in one place.
	opts := sling.SlingOpts{
		Target:    agentCfg,
		NoFormula: body.NoFormula,
		Title:     strings.TrimSpace(body.Title),
		Vars:      varSlice,
		Merge:     body.Merge,
		NoConvoy:  body.NoConvoy,
		Owned:     body.Owned,
		Reassign:  body.Reassign,
		Force:     body.Force,
		ScopeKind: body.ScopeKind,
		ScopeRef:  body.ScopeRef,
	}
	mode := "direct"
	switch {
	case attachedBeadID != "":
		mode = "attached"
		opts.BeadOrFormula = attachedBeadID
		opts.OnFormula = formulaName
	case formulaName != "":
		mode = "standalone"
		opts.BeadOrFormula = formulaName
		opts.IsFormula = true
	default:
		opts.BeadOrFormula = strings.TrimSpace(body.Bead)
	}
	result, err := sl.Dispatch(ctx, opts, store)
	// A convoy whose children were routed one by one reports per-child
	// outcomes even when some failed. The routed children are committed, so
	// the caller gets the batch result (as gc sling prints it) rather than a
	// bare error; a source-workflow conflict keeps its dedicated 409.
	partial := false
	if err != nil {
		var conflictErr *sourceworkflow.ConflictError
		if errors.As(err, &conflictErr) {
			return nil, http.StatusConflict, "conflict", err.Error(), conflictErr
		}
		partial = isPartialBatchResult(result)
	}
	if err != nil && !partial {
		var lookupErr *sling.BeadLookupError
		if errors.As(err, &lookupErr) {
			fmt.Fprintf(apiSlingStderr(), "gc api sling: %v\n", lookupErr) //nolint:errcheck
			return nil, http.StatusInternalServerError, "internal", "sling bead lookup failed", nil
		}
		var missingBeadErr *sling.MissingBeadError
		if errors.As(err, &missingBeadErr) {
			return nil, http.StatusBadRequest, "missing_bead", err.Error(), nil
		}
		var crossRigErr *sling.CrossRigError
		if errors.As(err, &crossRigErr) {
			return nil, http.StatusBadRequest, "cross_rig", err.Error(), nil
		}
		var crossStoreErr *sling.CrossStoreRouteError
		if errors.As(err, &crossStoreErr) {
			return nil, http.StatusBadRequest, "cross_store", err.Error(), nil
		}
		return nil, http.StatusBadRequest, "invalid", err.Error(), nil
	}

	// Surface both the domain's non-fatal metadata errors and the tolerated
	// source-workflow scan warnings to the caller. The scan messages reach only
	// the server log otherwise, leaving a remote caller blind to degraded
	// cross-store conflict coverage.
	warnings := result.MetadataErrors
	if len(sourceWorkflowScanMessages) > 0 {
		warnings = append(append([]string(nil), result.MetadataErrors...), sourceWorkflowScanMessages...)
	}
	resp := &slingResponse{
		Status:     SlingStatusSlung,
		Target:     body.Target,
		Bead:       body.Bead,
		Mode:       mode,
		Warnings:   warnings,
		MoleculeID: result.WispRootID,
		ConvoyID:   result.ConvoyID,
	}
	if result.ContainerType != "" {
		resp.Batch = slingBatchSummary(result)
	}
	if partial {
		resp.Status = SlingStatusPartial
	}
	explicitFormula := opts.IsFormula || opts.OnFormula != ""
	// The domain names the formula it cooked. On a plain bead that is the
	// target's default formula, which `gc sling` reports as an attachment.
	defaultFormulaApplied := !explicitFormula && result.FormulaName != ""
	if !explicitFormula && !defaultFormulaApplied {
		return resp, http.StatusOK, "", "", nil
	}
	if defaultFormulaApplied {
		resp.Mode = "attached"
		formulaName = result.FormulaName
		attachedBeadID = opts.BeadOrFormula
	}

	resp.Formula = formulaName
	resp.AttachedBeadID = attachedBeadID
	// Use structured result fields directly -- no stdout parsing needed.
	resp.WorkflowID = result.WorkflowID
	resp.RootBeadID = result.BeadID
	if resp.WorkflowID == "" && resp.RootBeadID == "" {
		return nil, http.StatusInternalServerError, "internal", "sling did not produce a workflow or bead id", nil
	}
	return resp, http.StatusOK, "", "", nil
}

// isPartialBatchResult reports whether a failed dispatch still routed part of
// a container: its children were attempted one by one and at least one was
// routed, so the failure is not the whole outcome. A container where every
// attempted child failed committed nothing and stays an error.
func isPartialBatchResult(result sling.SlingResult) bool {
	return result.ContainerType != "" && result.Routed > 0
}

// slingBatchSummary projects a container sling's per-child outcome onto the
// wire, naming each child whose routing failed.
func slingBatchSummary(result sling.SlingResult) *SlingBatchSummary {
	summary := &SlingBatchSummary{
		ContainerType: result.ContainerType,
		Total:         result.Total,
		Routed:        result.Routed,
		Failed:        result.Failed,
		Skipped:       result.Skipped,
		Idempotent:    result.IdempotentCt,
	}
	for _, child := range result.Children {
		if child.Failed {
			summary.Failures = append(summary.Failures, SlingChildFailure{BeadID: child.BeadID, Reason: child.FailReason})
		}
	}
	return summary
}

// relocatedGraphStore returns the city's graph-class binding only when the city
// relocates the graph class, and nil otherwise. With nil, the sling domain
// cooks a workflow in the sling's selected work store, next to its source bead.
// That is the rule `gc sling` applies through resolveGraphStore: the relocated
// binding when there is one, else the bead's own work store. Passing the
// non-relocated graph store here (the city store) put a rig bead's workflow in
// the city store, where the rig's pool workers never look, so the bead was
// never claimed. On a default city GraphBeadStore() is CityBeadStore(), the
// same identity sourceWorkflowStores relies on.
func (s *Server) relocatedGraphStore() beads.Store {
	return beads.RelocatedGraphStore(s.state.GraphBeadStore().Store, s.state.CityBeadStore())
}

func allowsForceStoreFallback(body slingBody, agentCfg config.Agent) bool {
	if !body.Force || strings.TrimSpace(body.Bead) == "" {
		return false
	}
	if strings.TrimSpace(body.Formula) != "" || strings.TrimSpace(body.AttachedBeadID) != "" {
		return false
	}
	return agentCfg.EffectiveDefaultSlingFormula() == ""
}

func slingStoreBeadID(body slingBody) string {
	// Formula attachment validates the attached bead, not the formula name.
	if attachedBeadID := strings.TrimSpace(body.AttachedBeadID); attachedBeadID != "" {
		return attachedBeadID
	}
	return strings.TrimSpace(body.Bead)
}

// sourceWorkflowCleanupHint renders the CLI command that clears the blocking
// source workflow. Surfaced in the conflict response body so users can fix
// the state without grepping docs.
func sourceWorkflowCleanupHint(sourceBeadID, storeRef string) string {
	args := []string{"gc workflow delete-source", sourceBeadID}
	if storeRef = strings.TrimSpace(storeRef); storeRef != "" {
		args = append(args, "--store-ref", storeRef)
	}
	args = append(args, "--apply")
	return strings.Join(args, " ")
}

// findSlingStore returns the bead store for sling operations.
func (s *Server) findSlingStore(rig string, agentCfg config.Agent, beadID string) beads.Store {
	// Match the CLI's bead-prefix-first resolution so existence checks consult
	// the bead's home store before any cross-rig guard runs.
	if resolvedRig, cityScope := s.slingStoreScopeForBead(beadID); cityScope {
		return s.state.CityBeadStore()
	} else if resolvedRig != "" {
		return s.state.BeadStore(resolvedRig)
	}
	if rig != "" {
		if store := s.state.BeadStore(rig); store != nil {
			return store
		}
	}
	if agentCfg.Dir != "" {
		if store := s.state.BeadStore(agentCfg.Dir); store != nil {
			return store
		}
	}
	return s.state.CityBeadStore()
}

// slingStoreRef returns a store ref string for the sling context.
func (s *Server) slingStoreRef(rig string, agentCfg config.Agent, beadID string) string {
	if resolvedRig, cityScope := s.slingStoreScopeForBead(beadID); cityScope {
		return "city:" + s.state.CityName()
	} else if resolvedRig != "" {
		return "rig:" + resolvedRig
	}
	if rig != "" {
		return "rig:" + rig
	}
	if agentCfg.Dir != "" {
		return "rig:" + agentCfg.Dir
	}
	return "city:" + s.state.CityName()
}

func (s *Server) slingStoreScopeForBead(beadID string) (rigName string, cityScope bool) {
	beadID = strings.TrimSpace(beadID)
	if beadID == "" {
		return "", false
	}
	cfg := s.state.Config()
	prefix := sling.BeadPrefixForCity(cfg, beadID)
	if prefix == "" {
		return "", false
	}
	if sling.IsHQPrefix(cfg, prefix) {
		return "", true
	}
	rig, ok := sling.FindRigByPrefix(cfg, prefix)
	if !ok {
		return "", false
	}
	return rig.Name, false
}

// sourceWorkflowStores lists every store the source-bead singleton guard must
// scan for a live workflow root.
//
// Graph-first, for the same reason workflowStores() leads with the graph store:
// a workflow root is graph class, so on a city that relocates the graph class
// every live root is in the binding and NOT in the work stores below. Scanning
// only those answered "no conflict" from stores that structurally cannot hold
// the answer, and the sling admitted a second live workflow beside the first
// (ga-nqdff). The leg is STRICT — a fault on the store that holds the answer
// refuses the sling instead of degrading to the tolerated non-source-store
// warning, because a binding fault is an error, never absence.
//
// It is skipped on a default (non-relocated) city, where GraphBeadStore() ==
// CityBeadStore(), so the single-store enumeration stays byte-identical and no
// store is scanned twice. Its ref reuses workflowStores()'s "graph:<city>"
// spelling so the store_ref round trip has one parse, not two.
func (s *Server) sourceWorkflowStores() []sling.SourceWorkflowStore {
	stores := make([]sling.SourceWorkflowStore, 0, len(s.state.BeadStores())+2)
	cityStore := s.state.CityBeadStore()
	if graphStore := s.relocatedGraphStore(); graphStore != nil {
		stores = append(stores, sling.SourceWorkflowStore{
			Store:    graphStore,
			StoreRef: sourceworkflow.GraphStoreRef(s.state.CityName()),
			Strict:   true,
		})
	}
	if cityStore != nil {
		stores = append(stores, sling.SourceWorkflowStore{
			Store:    cityStore,
			StoreRef: "city:" + s.state.CityName(),
		})
	}
	for rigName, store := range s.state.BeadStores() {
		if store == nil {
			continue
		}
		stores = append(stores, sling.SourceWorkflowStore{
			Store:    store,
			StoreRef: "rig:" + rigName,
		})
	}
	return stores
}

// slingRunner returns the SlingRunner for the API context.
// Uses SlingRunnerFunc if set (for tests), otherwise a real shell runner.
func (s *Server) slingRunner() sling.SlingRunner {
	if s.SlingRunnerFunc != nil {
		return s.SlingRunnerFunc
	}
	return func(dir, command string, env map[string]string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sh", "-c", command)
		if dir != "" {
			cmd.Dir = dir
		}
		cmd.Env = mergeEnvForSling(env)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return string(out), fmt.Errorf("running %q: %w", command, err)
		}
		return string(out), nil
	}
}

// mergeEnvForSling merges extra env vars into the current process env.
func mergeEnvForSling(extra map[string]string) []string {
	return execenv.MergeMap(os.Environ(), extra)
}

// apiAgentResolver implements sling.AgentResolver for the API context.
// Mirrors the CLI's rig-context behavior for bare agent names while still
// delegating qualified and city-scoped lookups to findAgent.
type apiAgentResolver struct{}

func (apiAgentResolver) ResolveAgent(cfg *config.City, name, rigContext string) (config.Agent, bool) {
	if rigContext != "" && !strings.Contains(name, "/") {
		if a, ok := findAgent(cfg, rigContext+"/"+name); ok {
			return a, true
		}
	}
	return findAgent(cfg, name)
}

// qualifySlingTarget prepends a rig directory to a bare target when the
// caller supplied a rig context and the qualified form resolves.
func qualifySlingTarget(cfg *config.City, target, rigContext string) string {
	if rigContext == "" || strings.Contains(target, "/") {
		return target
	}
	qualified := rigContext + "/" + target
	if _, ok := findAgent(cfg, qualified); ok {
		return qualified
	}
	return target
}

// slingRigContext derives the effective rig context for target qualification.
// scope_ref wins for explicit rig scope; otherwise body.Rig is used for legacy
// dashboard dispatches that pass --rig without scope metadata.
func slingRigContext(body slingBody) string {
	if body.ScopeKind == "rig" && body.ScopeRef != "" {
		return body.ScopeRef
	}
	if body.ScopeKind == "" && body.Rig != "" {
		return body.Rig
	}
	return ""
}

// apiBranchResolver implements sling.BranchResolver for the API context.
// Uses the same git resolution as the CLI.
type apiBranchResolver struct {
	cityPath string
}

func (r apiBranchResolver) DefaultBranch(dir string) string {
	if dir == "" {
		dir = r.cityPath
	}
	// Best-effort: read git's origin/HEAD ref for the default branch.
	// Falls back to empty string if git is unavailable.
	cmd := exec.CommandContext(context.Background(), "git", "-C", dir,
		"symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	// Sanitize the environment so a leaked GIT_DIR from a parent repo or hook
	// cannot redirect resolution to the wrong repository's default branch.
	cmd.Env = gitpkg.SanitizedEnv()
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(out)), "origin/"))
}

// apiNotifier implements sling.Notifier for the API context.
type apiNotifier struct {
	state State
}

func (n *apiNotifier) PokeController(_ string) {
	n.state.Poke()
}

func (n *apiNotifier) PokeControlDispatch(_ string) {
	n.state.Poke()
}

type apiBeadRouter struct {
	server *Server
	store  beads.Store
}

func (r apiBeadRouter) Route(_ context.Context, req sling.RouteRequest) error {
	if r.server == nil {
		return fmt.Errorf("sling router: missing server")
	}
	cfg := r.server.state.Config()
	if cfg != nil {
		if agentCfg, ok := findAgentByQualifiedTemplate(cfg, req.Target); ok && sling.IsCustomSlingQuery(agentCfg) {
			runner := r.server.slingRunner()
			if runner == nil {
				return fmt.Errorf("custom sling_query requires a runner")
			}
			slingCmd, slingWarn := sling.BuildSlingCommandForAgent("sling_query", agentCfg.EffectiveSlingQuery(), req.BeadID, r.server.state.CityPath(), r.server.state.CityName(), agentCfg, cfg.Rigs)
			if slingWarn != "" {
				fmt.Fprintf(apiSlingStderr(), "gc api sling: %s\n", slingWarn) //nolint:errcheck
			}
			_, err := runner(req.WorkDir, slingCmd, req.Env)
			return err
		}
	}
	// The core names the store that holds the bead. Honor it only when it is
	// the relocated graph binding (a --formula wisp root minted there, #6054);
	// on a city that relocates nothing the request keeps routing through the
	// sling's own store, unchanged.
	store := r.store
	if req.Store != nil && req.Store == r.server.relocatedGraphStore() {
		store = req.Store
	}
	if store == nil {
		return fmt.Errorf("built-in sling routing requires a store")
	}
	routedTo := req.Target
	if cfg != nil {
		routedTo = agentutil.NormalizePoolRouteTarget(cfg, req.Target)
	}
	if err := store.SetMetadata(req.BeadID, beadmeta.RoutedToMetadataKey, routedTo); err != nil {
		if req.Force && errors.Is(err, beads.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("setting gc.routed_to on %s: %w", req.BeadID, err)
	}
	return nil
}
