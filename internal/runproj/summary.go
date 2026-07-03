package runproj

import (
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// recentChangesCap caps the recentChanges list. Port of TS RECENT_CHANGES_CAP.
const recentChangesCap = 12

// maxHistoricalLanes caps the historical lanes carried on the wire (the true
// count still surfaces via TotalHistorical). Port of TS MAX_HISTORICAL_LANES.
const maxHistoricalLanes = 50

// engineeringTypes is the run-bead-filter allowlist. Port of TS
// ENGINEERING_TYPES. (Used by RunBeadFilter; the summary builder itself does not
// pre-filter — it groups whatever it is given, exactly like buildRunSummary.)
var engineeringTypes = map[string]bool{
	"feature": true, "bug": true, "task": true, "epic": true,
	"chore": true, "decision": true, "molecule": true,
}

// BuildRunSummary projects a set of bead snapshots into the dashboard run-view
// RunSummary DTO. It is a faithful Go port of the TypeScript buildRunSummary
// (internal/api/dashboardspa/web/shared/src/runs/summary.ts) for the
// BEAD-DERIVED result: lane health and the city census are the builder's
// pre-enrich defaults (status "unavailable"); session enrichment is Phase 2.
//
// The input is the latest bead snapshot per id (e.g. the output of Fold). Each
// bead is mapped to the phase classifier's RunIssue shape with the same field
// mapping fromDashboardBead uses (Type→issue_type, ParentID→parent, a zero
// UpdatedAt falling back to CreatedAt).
//
// partial=false and an empty feed-scope map reproduce the golden fixture; the
// optional variadic params mirror the TS signature for downstream callers.
func BuildRunSummary(beadList []beads.Bead, opts ...BuildOption) RunSummary {
	cfg := buildConfig{feedScopes: map[string]RunFeedScope{}}
	for _, o := range opts {
		o(&cfg)
	}

	issues := make([]runIssue, len(beadList))
	for i, b := range beadList {
		issues[i] = fromBead(b)
	}

	// Group by run-root id, preserving first-seen order (mirrors JS Map order).
	groups := map[string][]runIssue{}
	var order []string
	for _, issue := range issues {
		rootID := runRootID(issue)
		if _, ok := groups[rootID]; !ok {
			order = append(order, rootID)
		}
		groups[rootID] = append(groups[rootID], issue)
	}

	// Keep only real run groups (drop dangling roots and non-run groups).
	var runRootIDs []string
	var laneIssues []runIssue
	for _, rootID := range order {
		groupIssues := groups[rootID]
		if isDanglingRootGroup(rootID, groupIssues) || !isRunGroup(rootID, groupIssues) {
			continue
		}
		runRootIDs = append(runRootIDs, rootID)
		laneIssues = append(laneIssues, groupIssues...)
	}

	sortedLanes := make([]RunLane, 0, len(runRootIDs))
	for _, rootID := range runRootIDs {
		sortedLanes = append(sortedLanes, runLane(rootID, groups[rootID], cfg.feedScopes))
	}
	sort.SliceStable(sortedLanes, func(i, j int) bool {
		return compareLanes(sortedLanes[i], sortedLanes[j]) < 0
	})

	// gascity-dashboard-4xcv: blocked lanes are split out of Active.
	activeLanes := make([]RunLane, 0)
	completedLanes := make([]RunLane, 0)
	blockedLanes := make([]RunLane, 0)
	for _, lane := range sortedLanes {
		switch lane.Phase {
		case "complete":
			completedLanes = append(completedLanes, lane)
		case "blocked":
			blockedLanes = append(blockedLanes, lane)
		default:
			activeLanes = append(activeLanes, lane)
		}
	}

	totalHistorical := len(completedLanes)
	historicalLanes := completedLanes
	if len(historicalLanes) > maxHistoricalLanes {
		historicalLanes = historicalLanes[:maxHistoricalLanes]
	}

	summary := RunSummary{
		TotalActive:     len(activeLanes),
		TotalHistorical: totalHistorical,
		RunCounts:       runCounts(activeLanes, len(activeLanes), len(blockedLanes)),
		Lanes:           activeLanes,
		HistoricalLanes: historicalLanes,
		BlockedLanes:    blockedLanes,
		RecentChanges:   recentChanges(laneIssues),
		Census:          runCensusUnavailable(),
	}
	if cfg.partial {
		summary.LanesPartial = true
	}
	return summary
}

// RunFeedScope mirrors the TS RunFeedScope (feed-scope fallback entry).
type RunFeedScope struct {
	ScopeKind    string
	ScopeRef     string
	RootStoreRef string
}

type buildConfig struct {
	feedScopes map[string]RunFeedScope
	partial    bool
}

// BuildOption configures BuildRunSummary. The defaults (empty feed-scope map,
// partial=false) reproduce the golden fixture.
type BuildOption func(*buildConfig)

// WithFeedScopes supplies the feed-scope fallback map (TS feedScopes arg).
func WithFeedScopes(m map[string]RunFeedScope) BuildOption {
	return func(c *buildConfig) {
		if m != nil {
			c.feedScopes = m
		}
	}
}

// WithPartial sets the partial flag (TS partial arg) — emits lanesPartial=true.
func WithPartial(partial bool) BuildOption {
	return func(c *buildConfig) { c.partial = partial }
}

// isRunGroup reports whether a group is a run (root carries a run marker).
// Port of TS isRunGroup.
func isRunGroup(rootID string, issues []runIssue) bool {
	root, ok := findIssue(issues, rootID)
	if !ok {
		return false
	}
	md := root.metadata
	return stringValue(md[beadmeta.FormulaContractMetadataKey]) == "graph.v2" ||
		root.issueType == "molecule" ||
		stringValue(md[beadmeta.KindMetadataKey]) == "run" ||
		stringValue(md[beadmeta.FormulaMetadataKey]) != ""
}

// runCounts tallies lanes per kind. Port of TS runCounts.
func runCounts(lanes []RunLane, visible, blocked int) RunCounts {
	counts := RunCounts{
		Total:   len(lanes),
		Visible: visible,
		Blocked: blocked,
	}
	for _, lane := range lanes {
		switch runKind(lane.Formula) {
		case "prReview":
			counts.PrReview++
		case "designReview":
			counts.DesignReview++
		case "bugfix":
			counts.Bugfix++
		case "other":
			counts.Other++
		}
	}
	return counts
}

// runKind classifies a lane's formula into a count bucket. Port of TS runKind.
func runKind(formula RunLaneFormula) string {
	name, ok := runFormulaName(formula)
	if !ok {
		return "other"
	}
	switch name {
	case "mol-adopt-pr-v2":
		return "prReview"
	case "mol-design-review-v2":
		return "designReview"
	case "mol-bug-report-flow-v2", "mol-bug-report-implementation-v2":
		return "bugfix"
	}
	return "other"
}

// runLane builds a single lane. Port of TS runLane.
func runLane(rootID string, issues []runIssue, feedScopes map[string]RunFeedScope) RunLane {
	phase := mapRunPhase(issues)
	updatedAt := latestUpdatedAt(issues)
	formula := runFormula(rootID, issues)
	formulaName, hasFormula := runFormulaName(formula)
	stages := stageProgress(phase, formulaName, hasFormula, issues)

	foundStageIndex := -1
	for i, s := range stages {
		if s.Status == "active" {
			foundStageIndex = i
			break
		}
	}

	var primaryInProgress []runIssue
	for _, i := range issues {
		if isPrimaryStepIssue(i) && i.status == "in_progress" {
			primaryInProgress = append(primaryInProgress, i)
		}
	}
	activeStepID, hasActiveStep := latestStepID(primaryInProgress)
	progress := runProgress(stages, foundStageIndex, activeStepID, hasActiveStep, issues)

	formulaStages := stagesForFormula(formulaName, hasFormula)
	formulaStageResolved := false
	if len(formulaStages) > 0 && progress.Status == "active_step" {
		for _, st := range formulaStages {
			if containsString(st.steps, progress.StepID) {
				formulaStageResolved = true
				break
			}
		}
	}

	phaseLabel := phase.label
	if formula.Status == "known" && foundStageIndex >= 0 {
		// activeStage?.label ?? phase.label
		phaseLabel = stages[foundStageIndex].Label
	}

	return RunLane{
		ID:                   rootID,
		Title:                displayTitle(rootID, issues),
		Formula:              formula,
		Scope:                runScope(rootID, issues, feedScopes),
		External:             externalReference(issues),
		Phase:                phase.phase,
		PhaseLabel:           phaseLabel,
		StatusCounts:         statusCounts(issues),
		ActiveAssignees:      activeAssignees(issues),
		UpdatedAt:            updatedAt,
		Stages:               stages,
		Progress:             progress,
		FormulaStageResolved: formulaStageResolved,
		Health:               runHealthUnavailable(),
	}
}

// runRootID resolves the run-root id for a bead. Port of TS runRootId.
func runRootID(issue runIssue) string {
	if sourceRoot := sourceRunRootID(issue); sourceRoot != "" {
		return sourceRoot
	}
	md := issue.metadata
	if explicit := stringValue(md[beadmeta.RootBeadIDMetadataKey]); explicit != "" {
		return explicit
	}
	if stringValue(md[beadmeta.KindMetadataKey]) == "run" || issue.issueType == "molecule" {
		return issue.id
	}
	if moleculeID := stringValue(md["molecule_id"]); moleculeID != "" {
		return moleculeID
	}
	return issue.id
}

func sourceRunRootID(issue runIssue) string {
	md := issue.metadata
	keys := []string{
		"pr_review.run_root_id",
		"pr_review.workflow_root_id",
		"bugflow.active_run_id",
		"bugflow.implementation_run_id",
		"bugflow.implementation_workflow_id",
		"design_review.run_root_id",
		"design_review.workflow_root_id",
	}
	for _, k := range keys {
		if v := stringValue(md[k]); v != "" {
			return v
		}
	}
	return ""
}

// runScope resolves a lane's scope from root metadata, then feed scopes.
// Port of TS runScope.
func runScope(rootID string, issues []runIssue, feedScopes map[string]RunFeedScope) RunLaneScope {
	root, hasRoot := findIssue(issues, rootID)

	// ordered = root first, then the rest (root's metadata wins ties). TS
	// excludes the root by identity (issue !== root); bead ids are unique within
	// a group, so id inequality is the faithful predicate.
	var ordered []runIssue
	if hasRoot {
		ordered = append(ordered, root)
		for _, i := range issues {
			if i.id != root.id {
				ordered = append(ordered, i)
			}
		}
	} else {
		ordered = issues
	}

	rootStoreRef := metadataString(ordered, beadmeta.RootStoreRefMetadataKey)

	// Build the metadata map fromRootMetadataScope consumes: root metadata,
	// then overlay gc.root_store_ref and the resolved gc.scope_ref.
	scopeMeta := map[string]string{}
	if hasRoot {
		for k, v := range root.metadata {
			scopeMeta[k] = v
		}
	}
	if rootStoreRef != "" {
		scopeMeta[beadmeta.RootStoreRefMetadataKey] = rootStoreRef
	}
	scopeRef := ""
	if hasRoot {
		scopeRef = stringValue(root.metadata[beadmeta.ScopeRefMetadataKey])
	}
	if scopeRef == "" {
		scopeRef = metadataString(ordered, beadmeta.ScopeRefMetadataKey)
	}
	scopeMeta[beadmeta.ScopeRefMetadataKey] = scopeRef

	if ms, ok := fromRootMetadataScope(scopeMeta); ok {
		return availableScope(ms.scopeKind, ms.scopeRef, ms.rootStoreRef)
	}

	if feedScope, ok := feedScopes[rootID]; ok {
		rsr := rootStoreRef
		if rsr == "" {
			rsr = feedScope.RootStoreRef
		}
		return availableScope(feedScope.ScopeKind, feedScope.ScopeRef, rsr)
	}

	return RunLaneScope{Status: "unavailable", Error: "run scope metadata unavailable"}
}

// availableScope is the single edge that sanitizes scope refs (gascity-dashboard-5e5v).
// Port of TS availableScope.
func availableScope(kind, ref, rootStoreRef string) RunLaneScope {
	return RunLaneScope{
		Status:       "available",
		Kind:         kind,
		Ref:          stripNonPrintable(ref),
		RootStoreRef: stripNonPrintable(rootStoreRef),
	}
}

// runFormula resolves a lane's formula identity. Port of TS runFormula.
func runFormula(rootID string, issues []runIssue) RunLaneFormula {
	root, hasRoot := findIssue(issues, rootID)
	var rootPtr *runIssue
	if hasRoot {
		rootPtr = &root
	}
	name, ok := resolveRunFormulaIdentityLane(rootPtr, issues)
	if ok {
		return RunLaneFormula{Status: "known", Name: name}
	}
	return RunLaneFormula{Status: "unavailable", Error: "run formula unavailable"}
}

func runFormulaName(formula RunLaneFormula) (string, bool) {
	if formula.Status == "known" {
		return formula.Name, true
	}
	return "", false
}

// displayTitle resolves a lane's display title. Port of TS displayTitle.
func displayTitle(rootID string, issues []runIssue) string {
	prTitle := metadataString(issues, "pr_review.github_title")
	prNumber := metadataString(issues, "pr_review.pr_number")
	if prTitle != "" && prNumber != "" {
		return "PR #" + prNumber + ": " + prTitle
	}

	issueURL := metadataString(issues, "bugflow.github_issue_url")
	issueNumber := metadataString(issues, "bugflow.github_issue_number")
	if issueURL != "" && issueNumber != "" {
		first := ""
		if len(issues) > 0 {
			first = issues[0].title
		}
		if first == "" {
			first = rootID
		}
		return "Issue #" + issueNumber + ": " + first
	}

	if root, ok := findIssue(issues, rootID); ok && root.title != "" {
		return root.title
	}
	if len(issues) > 0 && issues[0].title != "" {
		return issues[0].title
	}
	return rootID
}

// statusCounts tallies issue statuses, preserving first-seen status order to
// match the TS Record insertion order. Port of TS statusCounts.
func statusCounts(issues []runIssue) StatusCounts {
	var counts StatusCounts
	for _, i := range issues {
		counts.inc(i.status)
	}
	return counts
}

// activeAssignees returns the sorted unique non-closed assignees.
// Port of TS activeAssignees.
func activeAssignees(issues []runIssue) []string {
	seen := map[string]bool{}
	var out []string
	for _, i := range issues {
		if i.status == "closed" {
			continue
		}
		a := nonEmpty(i.assignee)
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	sort.Strings(out)
	if out == nil {
		return []string{}
	}
	return out
}

// latestUpdatedAt returns the most-recent updated_at as the union value.
// Port of TS latestUpdatedAt.
func latestUpdatedAt(issues []runIssue) RunLaneUpdatedAt {
	best := ""
	bestMS := int64(0)
	found := false
	for _, i := range issues {
		if i.updatedAt == "" {
			continue
		}
		ms := parseTimestamp(i.updatedAt)
		if !found || ms > bestMS {
			best = i.updatedAt
			bestMS = ms
			found = true
		}
	}
	if !found {
		return RunLaneUpdatedAt{Status: "unavailable", Error: "run update time unavailable"}
	}
	return RunLaneUpdatedAt{Status: "available", At: best}
}

// recentChanges returns the newest-first capped recent-change list.
// Port of TS recentChanges (stable sort preserves input order on ties).
func recentChanges(issues []runIssue) []RunChange {
	var filtered []runIssue
	for _, i := range issues {
		if i.updatedAt != "" {
			filtered = append(filtered, i)
		}
	}
	sort.SliceStable(filtered, func(a, b int) bool {
		return parseTimestamp(filtered[b].updatedAt) < parseTimestamp(filtered[a].updatedAt)
	})
	if len(filtered) > recentChangesCap {
		filtered = filtered[:recentChangesCap]
	}
	out := make([]RunChange, 0, len(filtered))
	for _, i := range filtered {
		out = append(out, RunChange{
			ID:        i.id,
			Title:     i.title,
			Status:    i.status,
			UpdatedAt: i.updatedAt,
		})
	}
	return out
}

// compareLanes orders lanes newest-first, then by id. Port of TS compareLanes.
// Returns <0 if a sorts before b.
func compareLanes(a, b RunLane) int {
	aTime := int64(0)
	if a.UpdatedAt.Status == "available" {
		aTime = parseTimestamp(a.UpdatedAt.At)
	}
	bTime := int64(0)
	if b.UpdatedAt.Status == "available" {
		bTime = parseTimestamp(b.UpdatedAt.At)
	}
	if delta := bTime - aTime; delta != 0 {
		if delta < 0 {
			return -1
		}
		return 1
	}
	return strings.Compare(a.ID, b.ID)
}

// externalReference resolves the external PR/issue reference. Port of TS
// externalReference.
func externalReference(issues []runIssue) RunLaneExternalReference {
	label, hasLabel := externalLabel(issues)
	url, hasURL := externalURL(issues)
	if hasLabel && hasURL {
		return RunLaneExternalReference{Status: "available", Label: label, URL: url}
	}
	if hasLabel {
		return RunLaneExternalReference{Status: "label_only", Label: label}
	}
	return RunLaneExternalReference{Status: "unavailable", Error: "external reference unavailable"}
}

var httpURLRe = regexp.MustCompile(`(?i)^https?://`)

func externalURL(issues []runIssue) (string, bool) {
	raw := metadataString(issues, "pr_review.pr_url")
	if raw == "" {
		raw = metadataString(issues, "bugflow.github_issue_url")
	}
	if raw != "" && httpURLRe.MatchString(raw) {
		return raw, true
	}
	return "", false
}

func externalLabel(issues []runIssue) (string, bool) {
	if prNumber := metadataString(issues, "pr_review.pr_number"); prNumber != "" {
		return "PR #" + prNumber, true
	}
	if issueNumber := metadataString(issues, "bugflow.github_issue_number"); issueNumber != "" {
		return "Issue #" + issueNumber, true
	}
	if ref := metadataString(issues, "pr_review.external_ref"); ref != "" {
		return ref, true
	}
	if ref := metadataString(issues, "bugflow.external_ref"); ref != "" {
		return ref, true
	}
	return "", false
}

// metadataString returns the first non-empty metadata value for key across
// issues. Port of TS metadataString.
func metadataString(issues []runIssue, key string) string {
	for _, i := range issues {
		if v := stringValue(i.metadata[key]); v != "" {
			return v
		}
	}
	return ""
}

func runCensusUnavailable() RunCensusState {
	return RunCensusState{Status: "unavailable", Error: "run health has not been derived"}
}

func runHealthUnavailable() RunLaneHealthState {
	return RunLaneHealthState{Status: "unavailable", Error: "run health has not been derived"}
}

// RunBeadFilter reports whether a bead participates in run classification.
// Port of TS runBeadFilter. (Exposed for parity; the builder does not call it —
// live callers apply it at the projection boundary via FilterRunBeads.)
func RunBeadFilter(b beads.Bead) bool {
	for _, l := range b.Labels {
		if strings.HasPrefix(l, "gc:") {
			return false
		}
	}
	if engineeringTypes[b.Type] {
		return true
	}
	return stringValue(b.Metadata[beadmeta.KindMetadataKey]) == "run"
}

// FilterRunBeads returns the subset of beadList that participates in run
// classification, per RunBeadFilter. It is the projection-boundary analog of the
// frontend runBeadFilter (summary.ts): BuildRunSummary and BuildRunDetail
// are faithful ports of buildRunSummary/buildRunDetail, which receive
// already-filtered beads, so a live caller that folds the raw event log — which
// also carries message, session, and gc:-labeled control beads that can share a
// run root — must apply this before building. Dropping those unrelated beads
// keeps them from distorting lane status, counts, recent changes, and detail
// nodes.
func FilterRunBeads(beadList []beads.Bead) []beads.Bead {
	out := make([]beads.Bead, 0, len(beadList))
	for _, b := range beadList {
		if RunBeadFilter(b) {
			out = append(out, b)
		}
	}
	return out
}

// runProgress resolves the lane's progress union. Port of TS runProgress.
func runProgress(stages []RunStage, activeStageIndex int, activeStepID string, hasActiveStep bool, issues []runIssue) RunLaneProgress {
	stage := runStagePosition(stages, activeStageIndex)
	if hasActiveStep {
		return RunLaneProgress{
			Status:  "active_step",
			StepID:  activeStepID,
			Stage:   stage,
			Attempt: runStepAttempt(issues, activeStepID),
		}
	}
	if stage.Status == "available" {
		return RunLaneProgress{Status: "stage_only", Stage: stage, Error: "active run step unavailable"}
	}
	return RunLaneProgress{Status: "unavailable", Error: "run progress unavailable"}
}

// runStagePosition resolves the active-stage position union.
// Port of TS runStagePosition.
func runStagePosition(stages []RunStage, activeStageIndex int) RunLaneStagePosition {
	if activeStageIndex < 0 || activeStageIndex >= len(stages) {
		return RunLaneStagePosition{Status: "unavailable", Error: "active run stage unavailable"}
	}
	stage := stages[activeStageIndex]
	return RunLaneStagePosition{
		Status: "available",
		Index:  activeStageIndex,
		Key:    stage.Key,
		Label:  stage.Label,
	}
}

// runStepAttempt resolves the step-attempt union from the active step's review
// round. Port of TS runStepAttempt.
func runStepAttempt(issues []runIssue, stepID string) RunLaneStepAttempt {
	value, ok := reviewRoundForIssues(stepIssues(issues, stepID))
	if !ok {
		return RunLaneStepAttempt{Status: "unavailable", Error: "run step attempt unavailable"}
	}
	return RunLaneStepAttempt{Status: "available", Value: value}
}

// fromBead maps a beads.Bead to the phase classifier's runIssue, mirroring the
// TS fromDashboardBead adapter (phaseMapping.ts:369). The verified field
// mapping: Type→issue_type, ParentID→parent (falling back to the legacy
// gc.parent_bead_id marker), a zero UpdatedAt falling back to CreatedAt.
func fromBead(b beads.Bead) runIssue {
	parent := b.ParentID
	if parent == "" {
		parent = stringValue(b.Metadata[beadmeta.ParentBeadIDMetadataKey])
	}

	updatedAt := b.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = b.CreatedAt
	}

	issue := runIssue{
		id:        b.ID,
		title:     b.Title,
		desc:      b.Description,
		status:    b.Status,
		issueType: b.Type,
		assignee:  b.Assignee,
		updatedAt: formatTimestamp(updatedAt),
		parent:    parent,
	}
	if len(b.Metadata) > 0 {
		issue.metadata = b.Metadata
	}
	return issue
}

// formatTimestamp renders a time.Time the way the bead JSON wire carries it, so
// the projected updated_at string is byte-identical to the TS input. The fixture
// uses UTC RFC3339 with a "Z" suffix; time.Time.Format(time.RFC3339) on a UTC
// value produces exactly that.
func formatTimestamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// parseTimestamp parses an ISO timestamp to Unix milliseconds, returning 0 on
// failure (mirroring Date.parse → NaN → 0 in the TS comparators).
func parseTimestamp(value string) int64 {
	if value == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t, err = time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return 0
		}
	}
	return t.UnixMilli()
}

func findIssue(issues []runIssue, id string) (runIssue, bool) {
	for _, i := range issues {
		if i.id == id {
			return i, true
		}
	}
	return runIssue{}, false
}

// isDanglingRootGroup reports whether the group's root bead is absent.
// Port of TS isDanglingRootGroup.
func isDanglingRootGroup(rootID string, issues []runIssue) bool {
	for _, i := range issues {
		if i.id == rootID {
			return false
		}
	}
	return true
}
