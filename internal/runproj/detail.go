package runproj

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// ErrRunNotFound is the sentinel wrapped by SnapshotForRun (and everything
// built on it) when the requested run root is absent from the folded beads —
// the run is truly unknown to the projection, as opposed to present but
// unprojectable (UnsupportedRunError). Callers branch on it with errors.Is;
// the dashboard BFF uses it to grant a just-slung run's deep link a warming
// grace window instead of a terminal 404.
var ErrRunNotFound = errors.New("run not found")

// snapshotScanCount counts every snapshotForRun invocation. It exists so a test
// can prove the single-scan entry points fold a run exactly once (the detail
// path used to scan twice — once for the formula target, once for the build).
// It carries no production behavior.
var snapshotScanCount atomic.Int64

// RunSnapshot is an opaque, already-computed run snapshot. It lets a caller fold
// a run's beads ONCE (SnapshotForRun) and then serve both the formula-target
// extraction (FormulaTargetFromSnapshot) and the full detail build
// (BuildRunDetailFromSnapshot) off that single scan, instead of re-scanning the
// city's beads per operation. The internal shape stays package-private; callers
// treat the value as a token.
type RunSnapshot struct {
	raw runSnapshot
}

// SnapshotForRun folds beadList into the run rooted at runID exactly once,
// returning an opaque snapshot both FormulaTargetFromSnapshot and
// BuildRunDetailFromSnapshot consume. version and eventSeq parameterize the
// snapshot identity (the golden passes 1/100; the live tailer passes a real
// version and its LastSeq cursor). It returns an error only when the run root is
// absent from beadList; that error wraps ErrRunNotFound.
func SnapshotForRun(beadList []beads.Bead, runID string, version int, eventSeq int64) (RunSnapshot, error) {
	raw, err := snapshotForRun(beadList, runID, version, eventSeq)
	if err != nil {
		return RunSnapshot{}, err
	}
	return RunSnapshot{raw: raw}, nil
}

// FormulaTargetFromSnapshot resolves the compiled-formula name, preview target,
// and run scope from an already-computed snapshot, applying the exact identity
// and scope resolution RunFormulaTargetForRun applies. ok is false when the run
// is not a fetchable graph.v2 run, lacks a formula name+target, or has no valid
// scope.
//
// The resolution reads only version/seq-INDEPENDENT snapshot fields (the root
// bead's formula metadata and the scope kind/ref/root-store-ref), so a snapshot
// built with a real version/eventSeq yields the same target a zero-version
// snapshot would — the invariant that lets detail() fold once and serve both.
func FormulaTargetFromSnapshot(snap RunSnapshot) (name, target, scopeKind, scopeRef string, ok bool) {
	return formulaTargetFromSnapshot(snap.raw)
}

// BuildRunDetailFromSnapshot projects an already-computed snapshot into the
// run-detail DTO, layering the optional request-time sessions and compiled
// formula detail (both nil on the golden path). It is the single-scan analog of
// BuildRunDetailWithSessionsAndFormula: same inputs, same output, but off a
// snapshot the caller already folded. fetchFailure records why a live
// formula-detail fetch failed when formulaDetail is nil (empty defaults to
// upstream_error).
func BuildRunDetailFromSnapshot(snap RunSnapshot, sessions []DashboardSession, formulaDetail *FormulaOrderingDetail, fetchFailure RunFormulaDetailFetchFailure) (FormulaRunDetail, error) {
	return enrichFormulaRun(snap.raw, sessions, formulaDetail.toInput(), fetchFailure)
}

// BuildRunDetailForRun folds a run ONCE and returns both the detail DTO and the
// run's compiled-formula target in one scan. It is a combined convenience entry
// point for callers (and tests) that already hold the formula and want the
// target plus the detail off a single fold. The dashboard BFF does NOT call it:
// its detail() drives the same primitives directly (SnapshotForRun →
// FormulaTargetFromSnapshot → BuildRunDetailFromSnapshot) so it can cache the
// folded snapshot across requests and layer request-time sessions/formula
// enrichment per build — a chicken-and-egg this combined form cannot express,
// since it needs the fetched formula as input but only returns the target used
// to fetch it. Keeping the composition here gives it one tested home.
//
// The (name, target, scopeKind, scopeRef, targetOK) return is the formula target
// resolved off the SAME snapshot as the detail, so it equals what
// RunFormulaTargetForRun would report for the run. A caller fetches the compiled
// formula from that target and passes it back in on a later build (the formula
// detail is request-time enrichment, layered per build, not carried in the
// snapshot).
func BuildRunDetailForRun(beadList []beads.Bead, runID string, version int, eventSeq int64, sessions []DashboardSession, formulaDetail *FormulaOrderingDetail, fetchFailure RunFormulaDetailFetchFailure) (detail FormulaRunDetail, name, target, scopeKind, scopeRef string, targetOK bool, err error) {
	snap, err := SnapshotForRun(beadList, runID, version, eventSeq)
	if err != nil {
		return FormulaRunDetail{}, "", "", "", "", false, err
	}
	name, target, scopeKind, scopeRef, targetOK = FormulaTargetFromSnapshot(snap)
	detail, err = BuildRunDetailFromSnapshot(snap, sessions, formulaDetail, fetchFailure)
	return detail, name, target, scopeKind, scopeRef, targetOK, err
}

// UnsupportedRunReason distinguishes the expected v1/wisp case ("not_run_view" —
// the run lists but has no graph.v2 detail) from a malformed graph.v2 snapshot
// ("invalid_snapshot" — a genuine load failure). Port of TS UnsupportedRunReason
// (gascity-dashboard-9w3k).
type UnsupportedRunReason string

const (
	// ReasonNotRunView marks a run that has no graph.v2 detail view.
	ReasonNotRunView UnsupportedRunReason = "not_run_view"
	// ReasonInvalidSnapshot marks a malformed graph.v2 snapshot.
	ReasonInvalidSnapshot UnsupportedRunReason = "invalid_snapshot"
)

// UnsupportedRunError is returned when a run cannot be projected into a detail
// view. Port of TS UnsupportedRunError.
type UnsupportedRunError struct {
	Message string
	Reason  UnsupportedRunReason
}

func (e *UnsupportedRunError) Error() string { return e.Message }

func unsupportedRun(message string, reason UnsupportedRunReason) error {
	return &UnsupportedRunError{Message: message, Reason: reason}
}

// BuildRunDetail projects one run's folded beads into the dashboard run-detail
// DTO. It is the bead-derived entry point that reproduces the supervisor's
// /workflow/{id} projection client-side: it synthesizes a run snapshot for runID
// from beadList (member selection + dep synthesis, mirroring the golden
// generator's snapshotForRun), then runs the shared detail pipeline. No sessions
// or compiled formula are layered here — that enrichment is request-time on the
// endpoint. snapshotVersion and snapshotEventSeq parameterize the snapshot
// identity (the golden passes 1/100; the live tailer passes a real version and
// its LastSeq cursor).
//
// It returns an *UnsupportedRunError when the run is not a graph.v2 run or its
// snapshot identity/scope is missing — the same cases the TS enrichFormulaRun
// throws on.
func BuildRunDetail(beadList []beads.Bead, runID string, snapshotVersion int, snapshotEventSeq int64) (FormulaRunDetail, error) {
	return BuildRunDetailWithSessions(beadList, runID, snapshotVersion, snapshotEventSeq, nil)
}

// BuildRunDetailWithSessions is BuildRunDetail with a request-time session list
// layered in, so the detail's execution-instance session links and the
// streamable-session progress resolve against live sessions. The golden path
// passes nil (BuildRunDetail); the live endpoint passes the loopback /v0 sessions
// read. Session enrichment is NOT golden-gated.
func BuildRunDetailWithSessions(beadList []beads.Bead, runID string, snapshotVersion int, snapshotEventSeq int64, sessions []DashboardSession) (FormulaRunDetail, error) {
	return BuildRunDetailWithSessionsAndFormula(beadList, runID, snapshotVersion, snapshotEventSeq, sessions, nil, FormulaDetailUpstreamError)
}

// BuildRunDetailWithSessionsAndFormula is BuildRunDetailWithSessions with the
// supervisor's compiled formula detail layered in as well. Like sessions, this
// is request-time endpoint enrichment (NOT golden-gated): the live endpoint
// fetches the compiled formula for a run's name+target and passes it here so the
// run's nodes honor the authored step order and the formula-detail state resolves
// to "available". A nil formulaDetail keeps the un-enriched projection — the
// detail state then resolves from run metadata alone (a missing_* arm, or, once a
// name+target are known but no detail was supplied, the fetch_failed arm that
// honestly reports a run whose compiled formula could not be layered in).
// fetchFailure is the reason the live fetch failed when formulaDetail is nil
// (FormulaDetailNotFound for a supervisor 404, else FormulaDetailUpstreamError);
// callers with no live fetch pass FormulaDetailUpstreamError.
func BuildRunDetailWithSessionsAndFormula(beadList []beads.Bead, runID string, snapshotVersion int, snapshotEventSeq int64, sessions []DashboardSession, formulaDetail *FormulaOrderingDetail, fetchFailure RunFormulaDetailFetchFailure) (FormulaRunDetail, error) {
	snap, err := snapshotForRun(beadList, runID, snapshotVersion, snapshotEventSeq)
	if err != nil {
		return FormulaRunDetail{}, err
	}
	return enrichFormulaRun(snap, sessions, formulaDetail.toInput(), fetchFailure)
}

// RunFormulaTargetForRun resolves the compiled-formula name, preview target, and
// run scope for the run rooted at runID, reusing the exact identity and scope
// resolution the detail build applies. ok is false when the run is not a
// fetchable graph.v2 run, lacks a formula name+target, or has no valid scope.
//
// The dashboard BFF calls this to decide whether to fetch the supervisor's
// compiled formula detail and to target the correct formula layer. The
// formula-detail endpoint resolves the compiled formula against scope-derived
// search paths, so a rig-scoped run must send its scope_kind/scope_ref or the
// lookup resolves the wrong layer (or is rejected for missing required scope).
func RunFormulaTargetForRun(beadList []beads.Bead, runID string) (name, target, scopeKind, scopeRef string, ok bool) {
	snap, err := snapshotForRun(beadList, runID, 0, 0)
	if err != nil {
		return "", "", "", "", false
	}
	return formulaTargetFromSnapshot(snap)
}

// formulaTargetFromSnapshot is the shared formula-target resolution both
// RunFormulaTargetForRun and FormulaTargetFromSnapshot delegate to. It reads only
// version/seq-independent snapshot fields, so the result is identical whether the
// snapshot was built at version=0 (the old target-only scan) or at the run's real
// version/seq (the single-scan build).
func formulaTargetFromSnapshot(snap runSnapshot) (name, target, scopeKind, scopeRef string, ok bool) {
	if !isGraphV2(snap) {
		return "", "", "", "", false
	}
	root := rootBead(dedupeBeads(snap.beads), nonEmpty(snap.rootBeadID))
	name, _, target, hasName := resolveRunFormulaIdentityDetailState(root, "", false)
	if !hasName || target == "" {
		return "", "", "", "", false
	}
	scopeKind, scopeRef, scopeOK := fromSnapshotScope(snap)
	if !scopeOK {
		return "", "", "", "", false
	}
	return name, target, scopeKind, scopeRef, true
}

// snapshotForRun synthesizes a run snapshot for one root from the folded beads:
// member selection, the issue_type→kind / ref→step_ref projection (port of the
// golden generator's snapshotForRun + toRunSnapshotBead), snapshot identity from
// root metadata, and the dependency edges via snapshotDeps — the real bead
// dependencies plus the root→member parent edges, so the detail graph renders the
// actual step→step DAG the supervisor snapshot carried, not just parent links.
func snapshotForRun(beadList []beads.Bead, rootID string, version int, eventSeq int64) (runSnapshot, error) {
	snapshotScanCount.Add(1)
	rootIdx := -1
	for i := range beadList {
		if beadList[i].ID == rootID {
			rootIdx = i
			break
		}
	}
	if rootIdx < 0 {
		return runSnapshot{}, fmt.Errorf("runproj: detail run root %q: %w", rootID, ErrRunNotFound)
	}
	root := beadList[rootIdx]

	var members []beads.Bead
	for i := range beadList {
		b := beadList[i]
		if b.ID == rootID ||
			b.ParentID == rootID ||
			b.Metadata[beadmeta.RootBeadIDMetadataKey] == rootID ||
			strings.HasPrefix(b.ID, rootID+".") {
			members = append(members, b)
		}
	}

	snapBeads := make([]runSnapshotBead, 0, len(members))
	for i := range members {
		snapBeads = append(snapBeads, toRunSnapshotBead(members[i]))
	}

	seq := eventSeq
	rootStoreRef := root.Metadata[beadmeta.RootStoreRefMetadataKey]
	return runSnapshot{
		runID:             rootID,
		rootBeadID:        rootID,
		rootStoreRef:      rootStoreRef,
		resolvedRootStore: rootStoreRef,
		scopeKind:         root.Metadata[beadmeta.ScopeKindMetadataKey],
		scopeRef:          root.Metadata[beadmeta.ScopeRefMetadataKey],
		snapshotVersion:   version,
		snapshotEventSeq:  &seq,
		partial:           false,
		storesScanned:     []string{rootStoreRef},
		beads:             snapBeads,
		deps:              snapshotDeps(members),
		// logicalEdges stays nil on the bead-derived path: the supervisor's
		// precomputed logical edges are not available here, so buildRunDisplayEdges
		// derives the display graph from deps (bridging hidden scope-check nodes),
		// which reproduces the supervisor's logical-edge behavior.
		logicalEdges: nil,
	}, nil
}

// toRunSnapshotBead projects a folded bead into the supervisor run-snapshot row.
// Port of the golden generator's toRunSnapshotBead (issue_type→kind unless
// gc.original_kind overrides; ref→step_ref; scope_ref / logical_bead_id mirrored
// from metadata).
func toRunSnapshotBead(b beads.Bead) runSnapshotBead {
	kind := b.Type
	if v, ok := b.Metadata[beadmeta.OriginalKindMetadataKey]; ok {
		kind = v
	}
	return runSnapshotBead{
		id:            b.ID,
		title:         b.Title,
		status:        b.Status,
		kind:          kind,
		stepRef:       b.Ref,
		assignee:      b.Assignee,
		scopeRef:      b.Metadata[beadmeta.ScopeRefMetadataKey],
		logicalBeadID: b.Metadata[beadmeta.LogicalBeadIDMetadataKey],
		metadata:      b.Metadata,
	}
}

// snapshotDeps synthesizes a run snapshot's dependency edges from the folded
// members. It reproduces the supervisor RunSnapshot's dep set on the OSS-local
// path: the real bead dependency edges each member carries (its Dependencies and
// Needs) merged with the root→member parent edges, using the same edge direction
// and dedup semantics as the supervisor graph API's collectWorkflowDeps
// (internal/api/handler_convoy_dispatch.go) — the prerequisite (DependsOnID) is
// the edge source and the dependent (IssueID) the target, carrying the dep type.
// Edges to beads outside the run are dropped, since the detail graph can only
// render members it holds. Without the real edges, buildRunDisplayEdges would
// project only root→member parent edges and every genuine step→step dependency
// (and the logical graph derived from it) would be lost.
func snapshotDeps(members []beads.Bead) []runSnapshotDep {
	if len(members) == 0 {
		return nil
	}
	memberIDs := make(map[string]bool, len(members))
	for i := range members {
		memberIDs[members[i].ID] = true
	}

	deps := make([]runSnapshotDep, 0, len(members))
	seen := make(map[string]bool)
	add := func(from, to, kind string) {
		if from == "" || to == "" || from == to {
			return
		}
		if !memberIDs[from] || !memberIDs[to] {
			return
		}
		key := from + "\x00" + to + "\x00" + kind
		if seen[key] {
			return
		}
		seen[key] = true
		deps = append(deps, runSnapshotDep{from: from, to: to, kind: kind})
	}

	// Real dependency edges the fold preserved on each member: the structured
	// Dependencies (issue depends-on a prerequisite, carrying the dep type) and
	// the simpler Needs prerequisite list. The prerequisite is the source and the
	// dependent the target, matching collectWorkflowDeps.
	for i := range members {
		b := members[i]
		for _, d := range b.Dependencies {
			add(d.DependsOnID, d.IssueID, d.Type)
		}
		for _, need := range b.Needs {
			add(need, b.ID, "")
		}
	}

	// Root→member parent edges (the first-seen member is the root in fold order).
	rootID := members[0].ID
	for i := range members {
		if members[i].ID == rootID {
			continue
		}
		add(rootID, members[i].ID, "parent")
	}
	return deps
}

// runningFormulaRunInput mirrors the TS RunningFormulaRunInput. formulaDetail and
// sessions are nil on the bead-derived path; the live endpoint layers sessions in
// at request time. formulaDetailFailure records why the live fetch failed when
// formulaDetail is nil (not_found for a 404, else upstream_error); it is empty on
// the bead-derived path and defaults to upstream_error.
type runningFormulaRunInput struct {
	raw                  runSnapshot
	runID                string
	rootBeadID           string
	rootStoreRef         string
	resolvedRootStore    string
	scopeKind            string
	scopeRef             string
	root                 *runSnapshotBead
	beads                []runSnapshotBead
	rigRoot              string
	sessions             []DashboardSession
	formulaDetail        *formulaDetailInput
	formulaDetailFailure RunFormulaDetailFetchFailure
}

// runningFormulaRun carries the orchestrated detail outputs enrichFormulaRun
// assembles into the DTO. Port of the consumed subset of TS RunningFormulaRun.
type runningFormulaRun struct {
	title         string
	formula       RunFormula
	formulaDetail RunFormulaDetailState
	executionPath RunExecutionPath
	progress      FormulaRunProgress
	phase         string
	stages        []RunStage
	nodes         []RunDisplayNode
	edges         []RunDisplayEdge
	lanes         []RunDisplayLane
}

// enrichFormulaRun is the bead-derived detail pipeline entry. Port of TS
// enrichFormulaRun (enrich.ts). sessions and formulaDetail carry the optional
// request-time enrichment (both nil on the golden path). fetchFailure records why
// a live formula-detail fetch failed when formulaDetail is nil (empty on the
// golden path, defaulting to upstream_error).
func enrichFormulaRun(raw runSnapshot, sessions []DashboardSession, formulaDetail *formulaDetailInput, fetchFailure RunFormulaDetailFetchFailure) (FormulaRunDetail, error) {
	if !isGraphV2(raw) {
		return FormulaRunDetail{}, unsupportedRun("run is not a graph.v2 run", ReasonNotRunView)
	}

	rootBeadID := nonEmpty(raw.rootBeadID)
	runID := nonEmpty(raw.runID)
	rootStoreRef := nonEmpty(raw.rootStoreRef)
	resolvedRootStore := nonEmpty(raw.resolvedRootStore)
	deduped := dedupeBeads(raw.beads)
	root := rootBead(deduped, rootBeadID)
	scopeKind, scopeRef, scopeOK := fromSnapshotScope(raw)

	if runID == "" || rootStoreRef == "" || resolvedRootStore == "" {
		return FormulaRunDetail{}, unsupportedRun("run snapshot identity is missing or invalid", ReasonInvalidSnapshot)
	}
	if !scopeOK {
		return FormulaRunDetail{}, unsupportedRun("run scope is missing or invalid", ReasonInvalidSnapshot)
	}

	formulaRun := buildRunningFormulaRun(runningFormulaRunInput{
		raw:                  raw,
		runID:                runID,
		rootBeadID:           rootBeadID,
		rootStoreRef:         rootStoreRef,
		resolvedRootStore:    resolvedRootStore,
		scopeKind:            scopeKind,
		scopeRef:             scopeRef,
		root:                 root,
		beads:                deduped,
		sessions:             sessions,
		formulaDetail:        formulaDetail,
		formulaDetailFailure: fetchFailure,
	})

	var partialReasons []string
	if raw.partial {
		partialReasons = []string{"supervisor_snapshot_partial"}
	}

	return FormulaRunDetail{
		RunID:             runID,
		RootBeadID:        rootBeadID,
		RootStoreRef:      rootStoreRef,
		ResolvedRootStore: resolvedRootStore,
		ScopeKind:         scopeKind,
		ScopeRef:          scopeRef,
		Title:             formulaRun.title,
		Formula:           formulaRun.formula,
		FormulaDetail:     formulaRun.formulaDetail,
		ExecutionPath:     formulaRun.executionPath,
		SnapshotVersion:   raw.snapshotVersion,
		SnapshotEventSeq:  formulaRun.progress.SnapshotEventSeq,
		Completeness:      formulaRunCompleteness(partialReasons),
		Progress:          formulaRun.progress,
		Phase:             formulaRun.phase,
		Stages:            formulaRun.stages,
		Nodes:             formulaRun.nodes,
		Edges:             formulaRun.edges,
		Lanes:             formulaRun.lanes,
	}, nil
}

// buildRunningFormulaRun is the central detail aggregation. Port of TS
// buildRunningFormulaRun (formula-run.ts).
func buildRunningFormulaRun(input runningFormulaRunInput) runningFormulaRun {
	bg := groupRunBeads(input.beads, input.rootBeadID)
	groups := orderRunNodeGroups(bg.groups, input.formulaDetail, input.rootBeadID)
	latestIterationByLoop := latestIterationsByLoop(groups)
	sessionIndex := buildRunSessionIndex(input.sessions)
	sessionContext := runSessionLinkContext{sessionIndex: &sessionIndex, scopeRef: input.scopeRef}

	rawNodes := make([]RunDisplayNode, 0, len(groups))
	for _, group := range groups {
		latest, hasLatest := latestIterationByLoop[group.loopControlNodeID]
		rawNodes = append(rawNodes, buildRunDisplayNode(
			group,
			bg.badgesByTarget[group.semanticNodeID],
			latest,
			hasLatest,
			sessionContext,
		))
	}
	edges := buildRunDisplayEdges(input.raw, bg.physicalToSemantic, rawNodes)
	nodes := applyDisplayNodeStates(rawNodes, edges)
	progress := buildFormulaRunProgress(input.raw, nodes, edges)

	hasFormulaDetail := input.formulaDetail != nil
	formula := runFormulaState(input.root, hasFormulaDetail)
	formulaDetail := runFormulaDetailState(input.root, hasFormulaDetail, input.formulaDetailFailure)
	executionPath := resolveRunExecutionPath(input.root, input.beads, input.rigRoot)

	issues := make([]runIssue, 0, len(input.beads))
	for i := range input.beads {
		issues = append(issues, fromRunSnapshotBead(input.beads[i]))
	}
	phase := mapRunPhase(issues)
	formulaName, hasFormulaName := "", false
	if formula.Kind == "known" {
		formulaName, hasFormulaName = formula.Name, true
	}
	stages := stageProgress(phase, formulaName, hasFormulaName, issues)

	title := input.runID
	if input.root != nil {
		if t := nonEmpty(input.root.title); t != "" {
			title = t
		}
	}

	return runningFormulaRun{
		title:         title,
		formula:       formula,
		formulaDetail: formulaDetail,
		executionPath: executionPath,
		progress:      progress,
		phase:         phase.phase,
		stages:        stages,
		nodes:         nodes,
		edges:         edges,
		lanes:         buildRunDisplayLanes(nodes),
	}
}

// fromRunSnapshotBead adapts a run-snapshot bead to the phase classifier's
// runIssue. Port of TS fromRunSnapshotBead (formula-run.ts): kind→issue_type,
// empty updated_at, gc.parent_bead_id → parent.
func fromRunSnapshotBead(bead runSnapshotBead) runIssue {
	issue := runIssue{
		id:        bead.id,
		title:     bead.title,
		status:    bead.status,
		issueType: bead.kind,
		updatedAt: "",
		metadata:  bead.metadata,
	}
	if bead.assignee != "" {
		issue.assignee = bead.assignee
	}
	if parent := beadMeta(bead, beadmeta.ParentBeadIDMetadataKey); parent != "" {
		issue.parent = parent
	}
	return issue
}

// runFormulaState resolves the run's formula identity union. Port of TS
// runFormulaState.
func runFormulaState(root *runSnapshotBead, hasFormulaDetail bool) RunFormula {
	name, source, _, hasName := resolveRunFormulaIdentityDetailState(root, "", hasFormulaDetail)
	if hasName {
		resolvedSource := "metadata"
		if source == "title_fallback" {
			resolvedSource = "title_fallback"
		}
		return RunFormula{Kind: "known", Name: name, Source: resolvedSource}
	}
	return RunFormula{Kind: "unavailable", Reason: "missing_formula_metadata"}
}

// runFormulaDetailState resolves the compiled-formula-detail union. Port of TS
// runFormulaDetailState. When a name+target are known but no compiled detail was
// layered in, it reports the fetch_failed arm with fetchFailure as the reason:
// the live BFF passes not_found for a supervisor 404 and upstream_error for every
// other failure, matching the TS formulaDetailFetchFailure mapping. The
// bead-derived path (no live fetch) leaves fetchFailure empty and defaults to
// upstream_error.
func runFormulaDetailState(root *runSnapshotBead, hasFormulaDetail bool, fetchFailure RunFormulaDetailFetchFailure) RunFormulaDetailState {
	name, _, target, hasName := resolveRunFormulaIdentityDetailState(root, "", hasFormulaDetail)
	if !hasName {
		return RunFormulaDetailState{Kind: "unavailable", Reason: "missing_formula_metadata"}
	}
	if target == "" {
		return RunFormulaDetailState{Kind: "unavailable", Reason: "missing_run_target", Name: name}
	}
	if hasFormulaDetail {
		return RunFormulaDetailState{Kind: "available", Name: name, Target: target}
	}
	failure := fetchFailure
	if failure == "" {
		failure = FormulaDetailUpstreamError
	}
	return RunFormulaDetailState{
		Kind:    "unavailable",
		Reason:  "fetch_failed",
		Name:    name,
		Target:  target,
		Failure: string(failure),
	}
}

// buildFormulaRunProgress computes the run progress/census. Port of TS
// buildFormulaRunProgress.
func buildFormulaRunProgress(raw runSnapshot, nodes []RunDisplayNode, edges []RunDisplayEdge) FormulaRunProgress {
	visibleCount := 0
	for _, node := range nodes {
		if node.VisibleInGraph {
			visibleCount++
		}
	}

	streamableSessionIDs := []string{}
	seenStreamable := map[string]bool{}
	executionInstanceCount, sessionLinkCount, streamableSessionCount := 0, 0, 0

	for _, node := range nodes {
		for _, instance := range node.ExecutionInstances {
			executionInstanceCount++
			if instance.Session.Kind == "attached" {
				sessionLinkCount++
				if instance.Session.Streamable {
					streamableSessionCount++
					id := instance.Session.Link.SessionID
					if !seenStreamable[id] {
						seenStreamable[id] = true
						streamableSessionIDs = append(streamableSessionIDs, id)
					}
				}
			}
		}
	}

	var visibleStatuses, allStatuses nodeStatusCounts
	for _, node := range nodes {
		allStatuses.inc(node.Status)
		if node.VisibleInGraph {
			visibleStatuses.inc(node.Status)
		}
	}

	return FormulaRunProgress{
		SnapshotVersion:        raw.snapshotVersion,
		SnapshotEventSeq:       runSnapshotSequenceOf(raw.snapshotEventSeq),
		SnapshotPartial:        raw.partial,
		TotalNodeCount:         len(nodes),
		VisibleNodeCount:       visibleCount,
		EdgeCount:              len(edges),
		ExecutionInstanceCount: executionInstanceCount,
		SessionLinkCount:       sessionLinkCount,
		StreamableSessionCount: streamableSessionCount,
		StreamableSessionIDs:   streamableSessionIDs,
		StatusCounts:           visibleStatuses,
		AllStatusCounts:        allStatuses,
		Terminal:               deriveRunTerminal(visibleStatuses, visibleCount),
	}
}

// terminalRunNodeStatuses and nonTerminalRunNodeStatuses partition every
// RunNodeStatus value (shared/src/run-detail.ts) into its terminality class.
// This is the single Go-side source of the taxonomy the client used to
// duplicate; allRunNodeStatuses is the union the taxonomy test enumerates so a
// newly-added status must be explicitly classified here (or the test fails).
var (
	terminalRunNodeStatuses    = []string{"completed", "done", "failed", "skipped"}
	nonTerminalRunNodeStatuses = []string{"pending", "ready", "running", "active", "blocked"}
)

// deriveRunTerminal reports whether the run has reached a terminal state, using
// the visible-node status census. It matches the retired client isTerminalProgress
// fold exactly: terminal iff there is at least one visible node, no visible node
// sits in a non-terminal status, and the terminal-status tally covers every
// visible node.
func deriveRunTerminal(visibleStatuses nodeStatusCounts, visibleCount int) bool {
	if visibleCount <= 0 {
		return false
	}
	if sumStatusCounts(visibleStatuses, nonTerminalRunNodeStatuses) > 0 {
		return false
	}
	return sumStatusCounts(visibleStatuses, terminalRunNodeStatuses) >= visibleCount
}

// sumStatusCounts totals the counts of the given statuses (absent statuses
// contribute zero), mirroring the client's `?? 0` reduction.
func sumStatusCounts(counts nodeStatusCounts, statuses []string) int {
	total := 0
	for _, status := range statuses {
		total += counts.counts[status]
	}
	return total
}

// runSnapshotSequenceOf renders the snapshot-sequence union. Port of TS
// runSnapshotSequence (nil seq → supervisor_omitted).
func runSnapshotSequenceOf(seq *int64) RunSnapshotSequence {
	if seq != nil {
		return RunSnapshotSequence{Kind: "known", Seq: *seq}
	}
	return RunSnapshotSequence{Kind: "unavailable", Reason: "supervisor_omitted"}
}

// formulaRunCompleteness collapses partial reasons into the completeness union.
// Port of TS formulaRunCompleteness (dedupes reasons, preserving first-seen order).
func formulaRunCompleteness(reasons []string) FormulaRunCompleteness {
	seen := map[string]bool{}
	var unique []string
	for _, r := range reasons {
		if !seen[r] {
			seen[r] = true
			unique = append(unique, r)
		}
	}
	if len(unique) == 0 {
		return FormulaRunCompleteness{Kind: "complete"}
	}
	return FormulaRunCompleteness{Kind: "partial", Reasons: unique}
}

// isGraphV2 reports whether the snapshot's root carries gc.formula_contract =
// graph.v2. Port of TS isGraphV2.
func isGraphV2(raw runSnapshot) bool {
	root := rootBead(raw.beads, raw.rootBeadID)
	return rootMetaPtr(root, beadmeta.FormulaContractMetadataKey) == "graph.v2"
}

// rootBead finds the root bead by id. Port of TS rootBead (nil mirrors undefined).
func rootBead(beads []runSnapshotBead, rootBeadID string) *runSnapshotBead {
	rootID := nonEmpty(rootBeadID)
	if rootID == "" {
		return nil
	}
	for i := range beads {
		if nonEmpty(beads[i].id) == rootID {
			return &beads[i]
		}
	}
	return nil
}

// dedupeBeads drops beads whose id repeats, keeping the first. Port of TS
// dedupeBeads (empty-id beads are kept).
func dedupeBeads(in []runSnapshotBead) []runSnapshotBead {
	seen := map[string]bool{}
	out := make([]runSnapshotBead, 0, len(in))
	for _, bead := range in {
		id := nonEmpty(bead.id)
		if id != "" {
			if seen[id] {
				continue
			}
			seen[id] = true
		}
		out = append(out, bead)
	}
	return out
}

// fromSnapshotScope resolves the run scope from the snapshot identity. Port of TS
// fromSnapshotScope (bool mirrors null), extended with the same gc.root_store_ref
// fallback summary already applies via fromRootMetadataScope: the explicit
// gc.scope_kind/gc.scope_ref pair wins, but a root carrying only gc.root_store_ref
// recovers its scope from that store ref. Without the fallback a run that lists
// successfully in /runs/summary (which uses fromRootMetadataScope) would 422 with
// invalid_snapshot when opened in /runs/{id}/detail.
func fromSnapshotScope(raw runSnapshot) (kind, ref string, ok bool) {
	scopeKind, kindOK := parseRunScopeKind(raw.scopeKind)
	scopeRef := stringValueOrEmpty(raw.scopeRef)
	if kindOK && scopeRef != "" {
		return scopeKind, scopeRef, true
	}
	// Fallback (gascity-dashboard-km0w): recover the scope from the snapshot's
	// gc.root_store_ref, matching summary's fromRootMetadataScope so a run scoped
	// only by its root store ref opens in detail instead of failing invalid.
	parsedKind, parsedRef, storeOK := fromStoreRef(raw.rootStoreRef)
	if storeOK && scopeRefRe.MatchString(parsedRef) {
		return parsedKind, parsedRef, true
	}
	return "", "", false
}
