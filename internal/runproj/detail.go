package runproj

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

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
	snap, err := snapshotForRun(beadList, runID, snapshotVersion, snapshotEventSeq)
	if err != nil {
		return FormulaRunDetail{}, err
	}
	return enrichFormulaRun(snap, sessions)
}

// snapshotForRun synthesizes a run snapshot for one root from the folded beads.
// Faithful port of the golden generator's snapshotForRun + toRunSnapshotBead +
// depsForMembers: member selection, the issue_type→kind / ref→step_ref
// projection, root→member parent deps, and snapshot identity from root metadata.
func snapshotForRun(beadList []beads.Bead, rootID string, version int, eventSeq int64) (runSnapshot, error) {
	rootIdx := -1
	for i := range beadList {
		if beadList[i].ID == rootID {
			rootIdx = i
			break
		}
	}
	if rootIdx < 0 {
		return runSnapshot{}, fmt.Errorf("runproj: detail run root %q not found", rootID)
	}
	root := beadList[rootIdx]

	var members []beads.Bead
	for i := range beadList {
		b := beadList[i]
		if b.ID == rootID ||
			b.ParentID == rootID ||
			b.Metadata["gc.root_bead_id"] == rootID ||
			strings.HasPrefix(b.ID, rootID+".") {
			members = append(members, b)
		}
	}

	snapBeads := make([]runSnapshotBead, 0, len(members))
	for i := range members {
		snapBeads = append(snapBeads, toRunSnapshotBead(members[i]))
	}

	seq := eventSeq
	rootStoreRef := root.Metadata["gc.root_store_ref"]
	return runSnapshot{
		runID:             rootID,
		rootBeadID:        rootID,
		rootStoreRef:      rootStoreRef,
		resolvedRootStore: rootStoreRef,
		scopeKind:         root.Metadata["gc.scope_kind"],
		scopeRef:          root.Metadata["gc.scope_ref"],
		snapshotVersion:   version,
		snapshotEventSeq:  &seq,
		partial:           false,
		storesScanned:     []string{rootStoreRef},
		beads:             snapBeads,
		deps:              depsForMembers(members),
		logicalEdges:      nil,
	}, nil
}

// toRunSnapshotBead projects a folded bead into the supervisor run-snapshot row.
// Port of the golden generator's toRunSnapshotBead (issue_type→kind unless
// gc.original_kind overrides; ref→step_ref; scope_ref / logical_bead_id mirrored
// from metadata).
func toRunSnapshotBead(b beads.Bead) runSnapshotBead {
	kind := b.Type
	if v, ok := b.Metadata["gc.original_kind"]; ok {
		kind = v
	}
	return runSnapshotBead{
		id:            b.ID,
		title:         b.Title,
		status:        b.Status,
		kind:          kind,
		stepRef:       b.Ref,
		assignee:      b.Assignee,
		scopeRef:      b.Metadata["gc.scope_ref"],
		logicalBeadID: b.Metadata["gc.logical_bead_id"],
		metadata:      b.Metadata,
	}
}

// depsForMembers synthesizes root→member parent edges. Port of the generator's
// depsForMembers (each non-root member depends on the first member, which is the
// root in fold order).
func depsForMembers(members []beads.Bead) []runSnapshotDep {
	if len(members) == 0 {
		return nil
	}
	rootID := members[0].ID
	deps := make([]runSnapshotDep, 0, len(members))
	for i := range members {
		if members[i].ID == rootID {
			continue
		}
		deps = append(deps, runSnapshotDep{from: rootID, to: members[i].ID, kind: "parent"})
	}
	return deps
}

// runningFormulaRunInput mirrors the TS RunningFormulaRunInput. formulaDetail and
// sessions are nil on the bead-derived path; the live endpoint layers sessions in
// at request time.
type runningFormulaRunInput struct {
	raw               runSnapshot
	runID             string
	rootBeadID        string
	rootStoreRef      string
	resolvedRootStore string
	scopeKind         string
	scopeRef          string
	root              *runSnapshotBead
	beads             []runSnapshotBead
	rigRoot           string
	sessions          []DashboardSession
	formulaDetail     *formulaDetailInput
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
// enrichFormulaRun (enrich.ts). opts carries the optional session list (nil on
// the golden path).
func enrichFormulaRun(raw runSnapshot, sessions []DashboardSession) (FormulaRunDetail, error) {
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
		raw:               raw,
		runID:             runID,
		rootBeadID:        rootBeadID,
		rootStoreRef:      rootStoreRef,
		resolvedRootStore: resolvedRootStore,
		scopeKind:         scopeKind,
		scopeRef:          scopeRef,
		root:              root,
		beads:             deduped,
		sessions:          sessions,
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
	formulaDetail := runFormulaDetailState(input.root, hasFormulaDetail)
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
		if t := strings.TrimSpace(input.root.title); t != "" {
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
	if parent := beadMeta(bead, "gc.parent_bead_id"); parent != "" {
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
// runFormulaDetailState (the bead-derived path has no compiled detail, so it
// resolves to fetch_failed/upstream_error once a name+target are known).
func runFormulaDetailState(root *runSnapshotBead, hasFormulaDetail bool) RunFormulaDetailState {
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
	return RunFormulaDetailState{
		Kind:    "unavailable",
		Reason:  "fetch_failed",
		Name:    name,
		Target:  target,
		Failure: "upstream_error",
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
	}
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
	return rootMetaPtr(root, "gc.formula_contract") == "graph.v2"
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
// fromSnapshotScope (bool mirrors null).
func fromSnapshotScope(raw runSnapshot) (kind, ref string, ok bool) {
	scopeKind, kindOK := parseRunScopeKind(raw.scopeKind)
	scopeRef := stringValueOrEmpty(raw.scopeRef)
	if kindOK && scopeRef != "" {
		return scopeKind, scopeRef, true
	}
	return "", "", false
}
