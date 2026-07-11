package runproj

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// runIssue is the phase classifier's input shape, a faithful port of the TS
// RunIssue (shared/src/runs/phaseMapping.ts). updatedAt is the resolved ISO
// string (zero updated_at falls back to created_at, mirroring fromDashboardBead).
type runIssue struct {
	id        string
	title     string
	desc      string
	status    string
	issueType string
	assignee  string
	updatedAt string
	parent    string
	metadata  map[string]string
}

// phaseMapping is the result of mapRunPhase. Port of TS PhaseMapping.
type phaseMapping struct {
	phase       string
	label       string
	reviewRound int  // valid only when hasReviewRound is true
	hasRound    bool // TS reviewRound: number | null
}

// mapRunPhase classifies a run group into a phase. Port of TS mapRunPhase,
// with the terminal check hoisted above the blocked check: a fully-closed run
// is history, even when a member's status or text mentions "blocked" (the old
// order pinned aborted runs into the blocked lane with a claim-a-worker remedy
// that could do nothing). rootID names the group root so a terminal run whose
// ROOT closed with gc.outcome=fail keeps the honest "failed" label — the phase
// stays "complete" (the RunPhase union has no failed member, and complete is
// what routes the lane to history).
func mapRunPhase(rootID string, issues []runIssue) phaseMapping {
	if len(issues) > 0 {
		allClosed := true
		for _, i := range issues {
			if i.status != "closed" {
				allClosed = false
				break
			}
		}
		if allClosed {
			label := "complete"
			for _, i := range issues {
				if i.id != rootID {
					continue
				}
				outcome := strings.ToLower(stringValue(i.metadata[beadmeta.OutcomeMetadataKey]))
				if outcome == "fail" || outcome == "failed" {
					label = "failed"
				}
				break
			}
			return phaseMapping{phase: "complete", label: label}
		}
	}

	// Status-based blocked branch — authoritative for open runs.
	for _, i := range issues {
		if i.status == "blocked" || strings.Contains(textForIssue(i), "blocked") {
			return phaseMapping{phase: "blocked", label: "blocked"}
		}
	}

	// gascity-dashboard-q3p1: structured-first phase derivation.
	if structured, ok := structuredPhase(issues); ok {
		return structured
	}

	return fallbackPhase(issues)
}

// structuredPhase derives the phase from the run's current step.
// Port of TS structuredPhase. The bool return mirrors TS's `null`.
func structuredPhase(issues []runIssue) (phaseMapping, bool) {
	var primary []runIssue
	for _, i := range issues {
		if isPrimaryStepIssue(i) {
			primary = append(primary, i)
		}
	}
	var inProgress []runIssue
	for _, i := range primary {
		if i.status == "in_progress" {
			inProgress = append(inProgress, i)
		}
	}
	activeStepID, hasActive := latestStepID(inProgress)
	if !hasActive {
		activeStepID, hasActive = furthestStageStepID(primary)
	}
	if !hasActive {
		return phaseMapping{}, false
	}

	phase := stepIDPhase(activeStepID)
	if phase == "review" {
		resolved, ok := reviewRoundForIssues(issues)
		if !ok {
			resolved = fallbackReviewRound(issues)
		}
		return phaseMapping{
			phase:       "review",
			label:       "review round " + strconv.Itoa(resolved),
			reviewRound: resolved,
			hasRound:    true,
		}, true
	}
	return phaseMapping{phase: phase, label: phase}, true
}

var stepIDDelimiters = regexp.MustCompile(`[-._:/]+`)

func tokenizeStepID(stepID string) []string {
	parts := stepIDDelimiters.Split(strings.ToLower(stepID), -1)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// leadUpQualifierTokens mark a step as LEADING UP TO a gate rather than being
// the gate itself. Port of TS LEAD_UP_QUALIFIER_TOKENS.
var leadUpQualifierTokens = map[string]bool{
	"pre": true, "prepare": true, "wait": true, "await": true,
	"pending": true, "before": true, "for": true, "to": true,
}

var (
	approvalStageTokens       = map[string]bool{"approval": true, "approve": true, "approved": true, "gate": true}
	finalizationStageTokens   = map[string]bool{"finalize": true, "finalization": true, "merge": true, "cleanup": true, "publish": true}
	reviewStageTokens         = map[string]bool{"review": true, "reviewer": true, "scorecard": true, "persona": true, "personas": true, "audit": true, "repro": true, "baseline": true, "investigation": true, "classify": true, "classification": true}
	implementationStageTokens = map[string]bool{"implement": true, "implementation": true, "patch": true, "fixes": true, "repair": true, "work": true, "design": true}
	intakeStageTokens         = map[string]bool{"intake": true, "bootstrap": true, "context": true, "router": true, "request": true, "preflight": true, "setup": true, "rebase": true}
)

// stepIDPhase classifies a single gc.step_id into a generic RunPhase.
// Port of TS stepIdPhase.
func stepIDPhase(stepID string) string {
	tokens := tokenizeStepID(stepID)
	if hasStageToken(tokens, approvalStageTokens, true) {
		return "approval"
	}
	if hasStageToken(tokens, finalizationStageTokens, true) {
		return "finalization"
	}
	// Review rejects lead-up qualifiers like approval/finalization do: a
	// pre-review CI repair step ("repair-PRE-REVIEW-ci-failures") leads up to
	// review, it is not the review.
	if hasStageToken(tokens, reviewStageTokens, true) {
		return "review"
	}
	if hasStageToken(tokens, implementationStageTokens, false) {
		return "implementation"
	}
	if hasStageToken(tokens, intakeStageTokens, false) {
		return "intake"
	}
	return "active"
}

func hasStageToken(tokens []string, stageTokens map[string]bool, rejectWithLeadUpQualifier bool) bool {
	found := false
	for _, t := range tokens {
		if stageTokens[t] {
			found = true
			break
		}
	}
	if !found {
		return false
	}
	if rejectWithLeadUpQualifier {
		for _, t := range tokens {
			if leadUpQualifierTokens[t] {
				return false
			}
		}
	}
	return true
}

// fallbackPhase is the keyword fallback used only when no step carries a
// gc.step_id. Port of TS fallbackPhase.
func fallbackPhase(issues []runIssue) phaseMapping {
	if stepSignalContainsAny(issues, []string{"approval", "approved", "finalize-scope"}) {
		return phaseMapping{phase: "approval", label: "approval"}
	}
	if stepSignalContainsAny(issues, []string{"post-merge", "finalization", "finalize"}) {
		return phaseMapping{phase: "finalization", label: "finalization"}
	}

	round, hasRound := reviewRoundForIssues(issues)
	if hasRound || stepSignalContainsAny(issues, []string{"review", "reviewer", "scorecard"}) {
		resolved := round
		if !hasRound {
			resolved = fallbackReviewRound(issues)
		}
		return phaseMapping{
			phase:       "review",
			label:       "review round " + strconv.Itoa(resolved),
			reviewRound: resolved,
			hasRound:    true,
		}
	}

	if stepSignalContainsAny(issues, []string{"implementation", "patch", "do-work"}) {
		return phaseMapping{phase: "implementation", label: "implementation"}
	}
	if stepSignalContainsAny(issues, []string{"intake", "load-context", "router", "request"}) {
		return phaseMapping{phase: "intake", label: "intake"}
	}
	return phaseMapping{phase: "active", label: "active"}
}

// stepSignalText is the step-identity text for fallback scanning: title plus any
// gc.step_id. Port of TS stepSignalText.
func stepSignalText(issue runIssue) string {
	stepID := stringValue(issue.metadata[beadmeta.StepIDMetadataKey])
	parts := make([]string, 0, 2)
	if issue.title != "" {
		parts = append(parts, issue.title)
	}
	if stepID != "" {
		parts = append(parts, stepID)
	}
	return strings.ToLower(strings.Join(parts, " "))
}

func stepSignalContainsAny(issues []runIssue, needles []string) bool {
	for _, i := range issues {
		text := stepSignalText(i)
		for _, n := range needles {
			if strings.Contains(text, n) {
				return true
			}
		}
	}
	return false
}

var (
	roundInKey      = regexp.MustCompile(`(?:^|\.)(?:iteration|attempt)\.(\d+)$`)
	roundInValue    = regexp.MustCompile(`(?:^|\.)(?:iteration|attempt)\.(\d+)$`)
	roundKeyNoDigit = regexp.MustCompile(`(?:^|\.)(?:iteration|attempt)$`)
)

// reviewRoundForIssue returns the per-issue review round when one is encoded in
// metadata. Port of TS reviewRoundForIssue (three supported shapes). The bool
// mirrors TS's `null`. Go map iteration is unordered, but the TS loop returns on
// the first match in Object.entries order; to stay deterministic we iterate keys
// in sorted order and return the first match (only one round shape exists per
// bead in practice, so order does not change the value).
func reviewRoundForIssue(issue runIssue) (int, bool) {
	keys := make([]string, 0, len(issue.metadata))
	for k := range issue.metadata {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := issue.metadata[key]
		if m := roundInKey.FindStringSubmatch(key); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				return n, true
			}
		}
		if roundKeyNoDigit.MatchString(key) {
			if attempt, ok := parsePositiveInteger(value); ok {
				return attempt, true
			}
		}
		if m := roundInValue.FindStringSubmatch(value); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				return n, true
			}
		}
	}
	return 0, false
}

// reviewRoundForIssues returns the max per-issue review round. Port of TS
// reviewRoundForIssues.
func reviewRoundForIssues(issues []runIssue) (int, bool) {
	best := 0
	found := false
	for _, i := range issues {
		if r, ok := reviewRoundForIssue(i); ok {
			if !found || r > best {
				best = r
			}
			found = true
		}
	}
	return best, found
}

// fallbackReviewRound counts issues whose text mentions "review", min 1.
// Port of TS fallbackReviewRound.
func fallbackReviewRound(issues []runIssue) int {
	count := 0
	for _, i := range issues {
		if strings.Contains(textForIssue(i), "review") {
			count++
		}
	}
	if count < 1 {
		return 1
	}
	return count
}

// textForIssue concatenates issue fields for keyword scanning, skipping gc.var.*
// keys. Port of TS textForIssue.
func textForIssue(issue runIssue) string {
	metaKeys := make([]string, 0, len(issue.metadata))
	for k := range issue.metadata {
		if strings.HasPrefix(k, "gc.var.") {
			continue
		}
		metaKeys = append(metaKeys, k)
	}
	sort.Strings(metaKeys)
	var metaParts []string
	for _, k := range metaKeys {
		metaParts = append(metaParts, k+" "+issue.metadata[k])
	}
	metadataText := strings.Join(metaParts, " ")

	parts := []string{
		issue.title,
		issue.desc,
		issue.status,
		issue.issueType,
		issue.assignee,
		issue.parent,
		metadataText,
	}

	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.ToLower(strings.Join(out, " "))
}

// stringValue trims a string-typed metadata value; non-strings become "".
// Port of TS stringValue. (Go metadata is map[string]string, so a missing key
// yields "" naturally.) It delegates to nonEmpty so the JS-faithful trim
// (String.prototype.trim(): BOM stripped, NEL kept) is uniform across the
// package's metadata helpers rather than diverging on Go's unicode.IsSpace.
func stringValue(value string) string {
	return nonEmpty(value)
}

func parsePositiveInteger(value string) (int, bool) {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, false
	}
	if parsed > 0 {
		return parsed, true
	}
	return 0, false
}

// runStages is the generic 5-stage ladder. Port of TS runStages.
var runStages = [][2]string{
	{"intake", "Intake"},
	{"implementation", "Implementation"},
	{"review", "Review"},
	{"approval", "Approval"},
	{"finalization", "Finalization"},
}

// stageProgress derives the lane stage ladder. Port of TS stageProgress.
func stageProgress(phase phaseMapping, formula string, hasFormula bool, issues []runIssue) []RunStage {
	formulaStages := stagesForFormula(formula, hasFormula)
	if len(formulaStages) > 0 {
		return formulaStageProgress(formulaStages, issues)
	}

	if phase.phase == "blocked" {
		return []RunStage{{Key: "blocked", Label: "Blocked", Status: "blocked"}}
	}

	if phase.phase == "complete" {
		out := make([]RunStage, len(runStages))
		for i, s := range runStages {
			out[i] = RunStage{Key: s[0], Label: s[1], Status: "complete"}
		}
		return out
	}

	activeIndex := -1
	for i, s := range runStages {
		if s[0] == phase.phase {
			activeIndex = i
			break
		}
	}

	if activeIndex < 0 {
		out := make([]RunStage, len(runStages))
		for i, s := range runStages {
			status := "pending"
			if s[0] == "implementation" {
				status = "active"
			}
			out[i] = RunStage{Key: s[0], Label: s[1], Status: status}
		}
		return out
	}

	out := make([]RunStage, len(runStages))
	for idx, s := range runStages {
		label := s[1]
		if s[0] == "review" && phase.hasRound {
			label = "Review round " + strconv.Itoa(phase.reviewRound)
		}
		var status string
		switch {
		case idx < activeIndex:
			status = "complete"
		case idx == activeIndex:
			status = "active"
		default:
			status = "pending"
		}
		out[idx] = RunStage{Key: s[0], Label: label, Status: status}
	}
	return out
}

// formulaStage is a per-formula stage with its constituent step ids.
type formulaStage struct {
	key   string
	label string
	steps []string
}

// stagesForFormula returns the per-formula stage tables. Port of TS
// stagesForFormula. hasFormula mirrors TS's `formula: string | null`.
func stagesForFormula(formula string, hasFormula bool) []formulaStage {
	if !hasFormula {
		return nil
	}
	switch formula {
	case "mol-adopt-pr-v2":
		return []formulaStage{
			{"preflight", "Preflight", []string{"preflight"}},
			{"rebase", "Worktree / rebase", []string{"rebase-check"}},
			{"pre-review-ci", "Pre-review CI", []string{"pre-review-ci", "repair-pre-review-ci-failures"}},
			{"review", "Review loop", []string{
				"review-loop",
				"review-pipeline.review-claude",
				"review-pipeline.review-codex",
				"review-pipeline.review-gemini",
				"review-pipeline.synthesize",
				"review-pipeline.quality-scorecard",
				"apply-fixes",
			}},
			// repair-ci-failures is the pre-rename id kept for older runs.
			{"ci", "Pre-approval CI", []string{"pre-approval-ci", "repair-pre-approval-ci-failures", "repair-ci-failures"}},
			{"approval", "Human approval", []string{"human-approval"}},
			{"finalize", "Merge-ready", []string{"finalize"}},
			{"cleanup", "Cleanup", []string{"cleanup-worktree"}},
		}
	case "mol-design-review-v2":
		return []formulaStage{
			{"setup", "Setup", []string{"design-review.setup"}},
			{"personas", "Personas", []string{
				"design-review.persona-gen-claude",
				"design-review.persona-gen-codex",
				"design-review.persona-gen-gemini",
				"design-review.persona-synthesis",
			}},
			{"fanout", "Persona fanout", []string{
				"design-review.prepare-review-items",
				"design-review.persona-review-fanout",
			}},
			{"synthesis", "Synthesis", []string{"design-review.global-synthesis"}},
			{"apply", "Apply findings", []string{"design-review.apply-design-changes"}},
			{"finalize", "Finalize", []string{"finalize"}},
		}
	case "mol-bug-report-flow-v2":
		return []formulaStage{
			{"intake", "Intake", []string{"bootstrap-run", "refresh-intake"}},
			{"repro", "Reproduction", []string{"historical-baseline", "reported-build-repro", "main-repro"}},
			{"audit", "Audit", []string{"code-path-audit", "coverage-audit", "related-refs-audit"}},
			{"classify", "Classify", []string{"investigation-synthesis", "followup-evidence", "normalize-outcome"}},
			{"approval", "Human approval", []string{"approve-classification", "verify-classification-approval"}},
			{"publish", "Publish", []string{"publish-classification"}},
			{"dispatch", "Dispatch fix", []string{"dispatch-implementation"}},
		}
	case "mol-bug-report-implementation-v2":
		return []formulaStage{
			{"plan", "Plan approval", []string{"approve-fix-plan", "approve-test-hardening-plan", "verify-selected-plan-approval"}},
			{"design", "Design review", []string{"prepare-design-review-doc", "design-review"}},
			{"implement", "Implement", []string{"implement-change", "prepare-review-context"}},
			{"review", "Code review", []string{"code-review-loop", "apply-code-fixes"}},
			{"pr", "Open PR", []string{"approve-pr-open", "verify-pr-open-approval", "open-or-update-pr"}},
			{"ci", "CI", []string{"wait-for-ci"}},
			{"merge", "Merge", []string{"approve-merge", "verify-merge-approval", "merge-and-finalize"}},
		}
	}
	return nil
}

// formulaStageProgress maps formula stages to RunStage statuses.
// Port of TS formulaStageProgress.
func formulaStageProgress(stages []formulaStage, issues []runIssue) []RunStage {
	primary := primaryStepIssues(issues)
	activeIndex := formulaActiveStageIndex(stages, primary)
	furthestClosedIndex := furthestClosedStageIndex(stages, primary)

	out := make([]RunStage, len(stages))
	for idx, stage := range stages {
		out[idx] = RunStage{
			Key:    stage.key,
			Label:  stage.label,
			Status: formulaStageStatus(idx, activeIndex, furthestClosedIndex, stage, primary),
		}
	}
	return out
}

// primaryStepIssues keeps only the primary-step issues, mirroring the
// isPrimaryStepIssue filter formulaStageProgress applies before stage mapping.
func primaryStepIssues(issues []runIssue) []runIssue {
	var primary []runIssue
	for _, i := range issues {
		if isPrimaryStepIssue(i) {
			primary = append(primary, i)
		}
	}
	return primary
}

// formulaActiveStageIndex resolves the active stage index: the stage carrying
// the latest in-progress primary step, else the first open stage (-1 when none).
func formulaActiveStageIndex(stages []formulaStage, primary []runIssue) int {
	var inProgress []runIssue
	for _, i := range primary {
		if i.status == "in_progress" {
			inProgress = append(inProgress, i)
		}
	}
	activeStepID, hasActiveStep := latestStepID(inProgress)
	if !hasActiveStep {
		return firstOpenStageIndex(stages, primary)
	}
	// Retry iterations carry attempt-suffixed step ids
	// (repair-x-failures.attempt.1); the tables list the authored base ids.
	activeStepID = stripAttemptSuffix(activeStepID)
	for idx, s := range stages {
		if containsString(s.steps, activeStepID) {
			return idx
		}
	}
	return -1
}

// attemptSuffixRE matches the .attempt.N suffix runtime retries append to a
// step id (possibly stacked, e.g. ".attempt.1.attempt.2").
var attemptSuffixRE = regexp.MustCompile(`(\.attempt\.\d+)+$`)

// stripAttemptSuffix reduces an attempt-suffixed step id to its authored base
// id. A value that is nothing but the suffix is returned unchanged rather than
// stripped to the empty string.
func stripAttemptSuffix(id string) string {
	stripped := attemptSuffixRE.ReplaceAllString(id, "")
	if stripped == "" {
		return id
	}
	return stripped
}

// formulaStageStatus resolves one stage's status relative to the active and
// furthest-closed stage indices. Port of the TS status switch.
func formulaStageStatus(idx, activeIndex, furthestClosedIndex int, stage formulaStage, primary []runIssue) string {
	switch {
	case activeIndex >= 0 && idx < activeIndex:
		return "complete"
	case activeIndex >= 0 && idx == activeIndex:
		return "active"
	case activeIndex >= 0:
		return "pending"
	case stageHasClosedStep(stage, primary) || idx < furthestClosedIndex:
		return "complete"
	default:
		return "pending"
	}
}

// stageHasClosedStep reports whether any of the stage's steps has a closed
// primary issue.
func stageHasClosedStep(stage formulaStage, primary []runIssue) bool {
	for _, step := range stage.steps {
		for _, i := range stepIssues(primary, step) {
			if i.status == "closed" {
				return true
			}
		}
	}
	return false
}

func firstOpenStageIndex(stages []formulaStage, issues []runIssue) int {
	for idx, s := range stages {
		for _, step := range s.steps {
			for _, i := range stepIssues(issues, step) {
				if i.status != "closed" {
					return idx
				}
			}
		}
	}
	return -1
}

func furthestClosedStageIndex(stages []formulaStage, issues []runIssue) int {
	furthest := -1
	for idx, s := range stages {
		for _, step := range s.steps {
			closed := false
			for _, i := range stepIssues(issues, step) {
				if i.status == "closed" {
					closed = true
					break
				}
			}
			if closed {
				furthest = idx
				break
			}
		}
	}
	return furthest
}

// latestStepID returns the gc.step_id of the most-recent-then-furthest issue.
// Port of TS latestStepId. The bool mirrors TS's `null`.
func latestStepID(issues []runIssue) (string, bool) {
	sorted := make([]runIssue, len(issues))
	copy(sorted, issues)
	sort.SliceStable(sorted, func(i, j int) bool {
		return byMostRecentThenStage(sorted[i], sorted[j]) < 0
	})
	for _, i := range sorted {
		if s := stringValue(i.metadata[beadmeta.StepIDMetadataKey]); s != "" {
			return s, true
		}
	}
	return "", false
}

// furthestStageStepID picks the step whose stage is furthest along the ladder.
// Port of TS furthestStageStepId. The bool mirrors TS's `null`.
func furthestStageStepID(issues []runIssue) (string, bool) {
	var stepIDs []string
	for _, i := range issues {
		if id := stringValue(i.metadata[beadmeta.StepIDMetadataKey]); id != "" {
			stepIDs = append(stepIDs, id)
		}
	}
	if len(stepIDs) == 0 {
		return "", false
	}
	sorted := make([]string, len(stepIDs))
	copy(sorted, stepIDs)
	sort.SliceStable(sorted, func(i, j int) bool {
		rankDelta := stageRank(stepIDPhase(sorted[j])) - stageRank(stepIDPhase(sorted[i]))
		if rankDelta != 0 {
			return rankDelta < 0
		}
		return sorted[i] < sorted[j]
	})
	return sorted[0], true
}

// lifecycleRank is the lifecycle rank (higher = further along). Port of TS
// LIFECYCLE_RANK.
var lifecycleRank = map[string]int{
	"active":         0,
	"intake":         1,
	"implementation": 2,
	"review":         3,
	"approval":       4,
	"finalization":   5,
	"blocked":        6,
	"complete":       7,
}

func stageRank(phase string) int {
	return lifecycleRank[phase]
}

// byMostRecentThenStage is the deterministic step ordering comparator.
// Port of TS byMostRecentThenStage. Returns <0 if a sorts before b.
func byMostRecentThenStage(a, b runIssue) int {
	timeDelta := parseTimestamp(b.updatedAt) - parseTimestamp(a.updatedAt)
	if timeDelta != 0 {
		if timeDelta < 0 {
			return -1
		}
		return 1
	}
	aStep := stringValue(a.metadata[beadmeta.StepIDMetadataKey])
	bStep := stringValue(b.metadata[beadmeta.StepIDMetadataKey])
	rankDelta := stageRank(stepIDPhase(bStep)) - stageRank(stepIDPhase(aStep))
	if rankDelta != 0 {
		return rankDelta
	}
	if aStep < bStep {
		return -1
	}
	if aStep > bStep {
		return 1
	}
	return 0
}

// stepIssues returns the issues whose gc.step_id equals step, treating an
// attempt-suffixed id (step.attempt.N) as its authored base id. Port of TS
// stepIssues.
func stepIssues(issues []runIssue, step string) []runIssue {
	var out []runIssue
	for _, i := range issues {
		if stripAttemptSuffix(stringValue(i.metadata[beadmeta.StepIDMetadataKey])) == step {
			out = append(out, i)
		}
	}
	return out
}

// isPrimaryStepIssue excludes spec / scope-check / workflow-finalize kinds.
// Port of TS isPrimaryStepIssue.
func isPrimaryStepIssue(issue runIssue) bool {
	kind := stringValue(issue.metadata[beadmeta.KindMetadataKey])
	return kind != "spec" && kind != "scope-check" && kind != "workflow-finalize"
}

func containsString(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
