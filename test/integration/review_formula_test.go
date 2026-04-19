//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testfixtures/reviewworkflows"
)

// reviewWorkflowTimeout bounds waits for review-formula workflow beads to
// close. Successful runs on CI average ~5 min per test, but runner variance
// pushes individual runs past 8 min; keep the budget at 12 min so slow
// runners don't flake the lane.
const reviewWorkflowTimeout = 12 * time.Minute

const testAdoptPRReviewCheck = `#!/usr/bin/env bash
set -euo pipefail

BEAD_ID="${GC_BEAD_ID:-}"
[ -n "$BEAD_ID" ] || exit 1

BEAD_JSON=$(gc bd show "$BEAD_ID" --json 2>/dev/null)
ATTEMPT="${GC_ITERATION:-}"
if [ -z "$ATTEMPT" ]; then
  ATTEMPT=$(printf '%s\n' "$BEAD_JSON" | jq -r 'if type == "array" then (.[0].metadata["gc.attempt"] // "") else (.metadata["gc.attempt"] // "") end')
fi
ROOT_ID=$(printf '%s\n' "$BEAD_JSON" | jq -r 'if type == "array" then (.[0].metadata["gc.root_bead_id"] // "") else (.metadata["gc.root_bead_id"] // "") end')
[ -n "$ATTEMPT" ] && [ -n "$ROOT_ID" ] || exit 1

VERDICT=$(
  gc bd list --all --json --limit=0 2>/dev/null |
    jq -r --arg attempt "$ATTEMPT" --arg root "$ROOT_ID" '
      [
        .[]
        | select(.metadata["gc.root_bead_id"] == $root)
        | select((.metadata["gc.attempt"] // "") == $attempt)
        | select((.metadata["review.verdict"] // "") != "")
        | select((.metadata["gc.step_ref"] // "") | test("(^|\\.)apply-fixes(\\.attempt\\.1|\\.run\\.1)?$"))
        | .metadata["review.verdict"]
      ] | first // ""
    ' 2>/dev/null
)

case "$VERDICT" in
  done|approved|pass) exit 0 ;;
  *) exit 1 ;;
esac
`

const testDesignReviewCheck = `#!/usr/bin/env bash
set -euo pipefail

BEAD_ID="${GC_BEAD_ID:-}"
[ -n "$BEAD_ID" ] || exit 1

BEAD_JSON=$(gc bd show "$BEAD_ID" --json 2>/dev/null)
ATTEMPT="${GC_ITERATION:-}"
if [ -z "$ATTEMPT" ]; then
  ATTEMPT=$(printf '%s\n' "$BEAD_JSON" | jq -r 'if type == "array" then (.[0].metadata["gc.attempt"] // "") else (.metadata["gc.attempt"] // "") end')
fi
ROOT_ID=$(printf '%s\n' "$BEAD_JSON" | jq -r 'if type == "array" then (.[0].metadata["gc.root_bead_id"] // "") else (.metadata["gc.root_bead_id"] // "") end')
[ -n "$ATTEMPT" ] && [ -n "$ROOT_ID" ] || exit 1

VERDICT=$(
  gc bd list --all --json --limit=0 2>/dev/null |
    jq -r --arg attempt "$ATTEMPT" --arg root "$ROOT_ID" '
      [
        .[]
        | select(.metadata["gc.root_bead_id"] == $root)
        | select((.metadata["gc.attempt"] // "") == $attempt)
        | select((.metadata["design_review.verdict"] // "") != "")
        | select((.metadata["gc.step_ref"] // "") | test("(^|\\.)apply-design-changes(\\.attempt\\.1|\\.run\\.1)?$"))
        | .metadata["design_review.verdict"]
      ] | first // ""
    ' 2>/dev/null
)

case "$VERDICT" in
  done|approved|pass) exit 0 ;;
  *) exit 1 ;;
esac
`

const testCodeReviewCheck = `#!/usr/bin/env bash
set -euo pipefail

BEAD_ID="${GC_BEAD_ID:-}"
[ -n "$BEAD_ID" ] || exit 1

BEAD_JSON=$(gc bd show "$BEAD_ID" --json 2>/dev/null)
ATTEMPT="${GC_ITERATION:-}"
if [ -z "$ATTEMPT" ]; then
  ATTEMPT=$(printf '%s\n' "$BEAD_JSON" | jq -r 'if type == "array" then (.[0].metadata["gc.attempt"] // "") else (.metadata["gc.attempt"] // "") end')
fi
ROOT_ID=$(printf '%s\n' "$BEAD_JSON" | jq -r 'if type == "array" then (.[0].metadata["gc.root_bead_id"] // "") else (.metadata["gc.root_bead_id"] // "") end')
[ -n "$ATTEMPT" ] && [ -n "$ROOT_ID" ] || exit 1

VERDICT=$(
  gc bd list --all --json --limit=0 2>/dev/null |
    jq -r --arg attempt "$ATTEMPT" --arg root "$ROOT_ID" '
      [
        .[]
        | select(.metadata["gc.root_bead_id"] == $root)
        | select((.metadata["gc.attempt"] // "") == $attempt)
        | select((.metadata["code_review.verdict"] // "") != "")
        | select((.metadata["gc.step_ref"] // "") | test("(^|\\.)apply-code-fixes(\\.attempt\\.1|\\.run\\.1)?$"))
        | .metadata["code_review.verdict"]
      ] | first // ""
    ' 2>/dev/null
)

case "$VERDICT" in
  done|approved|pass) exit 0 ;;
  *) exit 1 ;;
esac
`

// TestAdoptPRFormulaCompileAndRun validates a test-local adopt-pr fixture that
// exercises graph.v2 scopes, Ralph, compose.expand, and pooled review fan-out.
func TestAdoptPRFormulaCompileAndRun(t *testing.T) {
	cityDir := setupReviewFormulaCity(t, "success", nil)
	issueID, workflowID := startReviewWorkflow(t, cityDir, "mol-adopt-pr-v2", map[string]string{
		"issue":       "", // filled after create
		"pr_ref":      "refs/heads/test",
		"base_branch": "main",
		"skip_gemini": "false",
	})

	workflow := waitForBeadClosed(t, cityDir, workflowID, reviewWorkflowTimeout)
	if got := metaValue(workflow, "gc.outcome"); got != "pass" {
		dumpWorkflowState(t, cityDir, workflowID)
		t.Fatalf("workflow outcome = %q, want pass", got)
	}

	// Verify the expansion produced reviewer steps inside the Ralph attempt.
	steps := listWorkflowSteps(t, cityDir, workflowID)
	wantSuffixes := []string{
		"review-pipeline.review-claude",
		"review-pipeline.review-codex",
		"review-pipeline.review-gemini",
		"review-pipeline.synthesize",
		"apply-fixes",
		"review-loop.iteration.1",
	}
	for _, suffix := range wantSuffixes {
		if !hasStepWithSuffix(steps, suffix) {
			t.Errorf("missing step with suffix %q in workflow; got: %v", suffix, steps)
		}
	}

	// Verify source bead is clean.
	issue := showBead(t, cityDir, issueID)
	if got := metaValue(issue, "work_dir"); got != "" {
		t.Errorf("source bead work_dir not cleaned up: %q", got)
	}
}

// TestPersonalWorkFormulaCompileAndRun validates a test-local personal-work
// fixture with two Ralph loops and two compose.expand sites.
func TestPersonalWorkFormulaCompileAndRun(t *testing.T) {
	cityDir := setupReviewFormulaCity(t, "success", nil)
	issueID, workflowID := startReviewWorkflow(t, cityDir, "mol-personal-work-v2", map[string]string{
		"issue":         "", // filled after create
		"base_branch":   "main",
		"skip_gemini":   "false",
		"setup_command": "true",
		"test_command":  "true",
	})

	workflow := waitForBeadClosed(t, cityDir, workflowID, reviewWorkflowTimeout)
	if got := metaValue(workflow, "gc.outcome"); got != "pass" {
		dumpWorkflowState(t, cityDir, workflowID)
		t.Fatalf("workflow outcome = %q, want pass", got)
	}

	// Verify both Ralph loops produced steps.
	steps := listWorkflowSteps(t, cityDir, workflowID)
	wantSuffixes := []string{
		"design-review-loop.iteration.1",
		"code-review-loop.iteration.1",
		"review-pipeline.review-claude",
		"review-pipeline.synthesize",
	}
	for _, suffix := range wantSuffixes {
		if !hasStepWithSuffix(steps, suffix) {
			t.Errorf("missing step with suffix %q in workflow; got: %v", suffix, steps)
		}
	}

	issue := showBead(t, cityDir, issueID)
	if got := metaValue(issue, "work_dir"); got != "" {
		t.Errorf("source bead work_dir not cleaned up: %q", got)
	}
}

func TestAdoptPRFormulaRetriesTransientReviewerStep(t *testing.T) {
	cityDir := setupReviewFormulaCity(t, "success", map[string]string{
		"GC_GRAPH_TRANSIENT_ONCE_SUFFIXES": "review-loop.iteration.1.review-pipeline.review-codex.attempt.1",
	})
	_, workflowID := startReviewWorkflow(t, cityDir, "mol-adopt-pr-v2", map[string]string{
		"issue":       "",
		"pr_ref":      "refs/heads/test",
		"base_branch": "main",
		"skip_gemini": "false",
	})

	workflow := waitForBeadClosed(t, cityDir, workflowID, reviewWorkflowTimeout)
	if got := metaValue(workflow, "gc.outcome"); got != "pass" {
		dumpWorkflowState(t, cityDir, workflowID)
		t.Fatalf("workflow outcome = %q, want pass", got)
	}

	steps := listWorkflowSteps(t, cityDir, workflowID)
	if !hasStepWithSuffix(steps, "review-pipeline.review-codex.attempt.2") {
		t.Fatalf("missing retry attempt for codex reviewer; got: %v", steps)
	}

	logical := mustFindWorkflowBeadByRefSuffix(t, cityDir, workflowID, "review-loop.iteration.1.review-pipeline.review-codex")
	if got := metaValue(logical, "gc.outcome"); got != "pass" {
		t.Fatalf("review-codex logical outcome = %q, want pass", got)
	}
}

func TestAdoptPRFormulaSoftFailsGeminiAfterTransientRetries(t *testing.T) {
	cityDir := setupReviewFormulaCity(t, "success", map[string]string{
		"GC_GRAPH_ALWAYS_TRANSIENT_SUFFIXES": "review-loop.iteration.1.review-pipeline.review-gemini.attempt.",
	})
	_, workflowID := startReviewWorkflow(t, cityDir, "mol-adopt-pr-v2", map[string]string{
		"issue":       "",
		"pr_ref":      "refs/heads/test",
		"base_branch": "main",
		"skip_gemini": "false",
	})

	workflow := waitForBeadClosed(t, cityDir, workflowID, reviewWorkflowTimeout)
	if got := metaValue(workflow, "gc.outcome"); got != "pass" {
		dumpWorkflowState(t, cityDir, workflowID)
		t.Fatalf("workflow outcome = %q, want pass", got)
	}

	steps := listWorkflowSteps(t, cityDir, workflowID)
	for _, suffix := range []string{
		"review-pipeline.review-gemini.attempt.2",
		"review-pipeline.review-gemini.attempt.3",
	} {
		if !hasStepWithSuffix(steps, suffix) {
			t.Fatalf("missing Gemini retry attempt %q; got: %v", suffix, steps)
		}
	}

	logical := mustFindWorkflowBeadByRefSuffix(t, cityDir, workflowID, "review-loop.iteration.1.review-pipeline.review-gemini")
	if got := metaValue(logical, "gc.outcome"); got != "pass" {
		t.Fatalf("review-gemini logical outcome = %q, want pass", got)
	}
	if got := metaValue(logical, "gc.final_disposition"); got != "soft_fail" {
		t.Fatalf("review-gemini gc.final_disposition = %q, want soft_fail", got)
	}
	if got := metaValue(logical, "gc.failure_reason"); got != "rate_limited" {
		t.Fatalf("review-gemini gc.failure_reason = %q, want rate_limited", got)
	}
}

func TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash(t *testing.T) {
	cityDir := setupReviewFormulaCity(t, "success", map[string]string{
		"GC_GRAPH_TRANSIENT_ONCE_SUFFIXES":        "review.attempt.1",
		"GC_GRAPH_EXIT_AFTER_CLAIM_ONCE_SUFFIXES": "review.attempt.2",
	})
	writeLocalFormula(t, cityDir, "mol-retry-recovery-smoke", `description = """
Minimal pooled retry workflow used to verify crash-before-result recovery.
"""
formula = "mol-retry-recovery-smoke"
version = 2

[[steps]]
id = "review"
title = "Single pooled retry step"
metadata = { "gc.run_target" = "polecat" }
description = """
Exercise pooled retry behavior on a single durable step.
"""

[steps.retry]
max_attempts = 3
on_exhausted = "hard_fail"
`)

	_, workflowID := startReviewWorkflow(t, cityDir, "mol-retry-recovery-smoke", map[string]string{
		"issue": "",
	})

	workflow := waitForBeadClosed(t, cityDir, workflowID, 4*time.Minute)
	if got := metaValue(workflow, "gc.outcome"); got != "pass" {
		dumpWorkflowState(t, cityDir, workflowID)
		t.Fatalf("workflow outcome = %q, want pass", got)
	}

	steps := listWorkflowSteps(t, cityDir, workflowID)
	if !hasStepWithSuffix(steps, "review.attempt.2") {
		t.Fatalf("missing retry attempt after transient failure; got: %v", steps)
	}
	attempt2 := mustFindWorkflowBeadByRefSuffix(t, cityDir, workflowID, "review.attempt.2")

	logical := mustFindWorkflowBeadByRefSuffix(t, cityDir, workflowID, ".review")
	if got := metaValue(logical, "gc.outcome"); got != "pass" {
		t.Fatalf("logical review outcome = %q, want pass", got)
	}

	trace := readOptionalFile(filepath.Join(cityDir, "graph-workflow-trace.log"))
	if !traceHasLineWithAll(trace, "exit-after-claim bead="+attempt2.ID, "ref="+attempt2.Ref) {
		t.Fatalf("worker trace missing forced crash evidence:\n%s", trace)
	}
	if countTraceLinesWithAll(trace, "claim bead="+attempt2.ID) < 2 {
		t.Fatalf("worker trace missing reclaim evidence for %s:\n%s", attempt2.ID, trace)
	}
	if !traceHasLineWithAll(trace, "run bead="+attempt2.ID, "ref="+attempt2.Ref) {
		t.Fatalf("worker trace missing reclaimed attempt execution for %s:\n%s", attempt2.ID, trace)
	}
	if !traceHasLineWithAll(trace, "closed bead="+attempt2.ID, "outcome=pass") {
		t.Fatalf("worker trace missing reclaimed attempt success for %s:\n%s", attempt2.ID, trace)
	}
}

// --- helpers ---

func setupReviewFormulaCity(t *testing.T, mode string, extraEnv map[string]string) string {
	t.Helper()
	env := newIsolatedCommandEnv(t, true)

	var cityName string
	if usingSubprocess() {
		cityName = uniqueCityName()
	} else {
		cityName = "review-formula-test"
	}
	cityDir := filepath.Join(t.TempDir(), cityName)

	startCommand := workflowAgentStartCommand(mode, extraEnv)
	cityToml := fmt.Sprintf(
		"[workspace]\nname = %q\n\n[session]\nprovider = \"subprocess\"\n\n[daemon]\nformula_v2 = true\npatrol_interval = \"100ms\"\n\n"+
			"[[agent]]\nname = \"worker\"\nmax_active_sessions = 1\nstart_command = %q\n\n"+
			"[[named_session]]\ntemplate = \"worker\"\nmode = \"always\"\n\n"+
			"[[agent]]\nname = \"polecat\"\nstart_command = %q\nmin_active_sessions = 0\nmax_active_sessions = 3\n",
		cityName, startCommand, startCommand,
	)
	configPath := filepath.Join(t.TempDir(), "review-formula.toml")
	if err := os.WriteFile(configPath, []byte(cityToml), 0o644); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	formulaDir := filepath.Join(cityDir, "formulas")
	if err := os.MkdirAll(formulaDir, 0o755); err != nil {
		t.Fatalf("mkdir formulas: %v", err)
	}
	checksDir := filepath.Join(cityDir, ".gc", "scripts", "checks")
	if err := os.MkdirAll(checksDir, 0o755); err != nil {
		t.Fatalf("mkdir checks: %v", err)
	}
	installReviewFormulaFixtures(t, cityDir)

	initCityWithManagedDoltRecovery(t, env, configPath, cityDir)
	registerCityCommandEnv(cityDir, env)
	t.Cleanup(func() {
		unregisterCityCommandEnv(cityDir)
		runGCDoltWithEnv(env, "", "stop", cityDir)                //nolint:errcheck
		runGCDoltWithEnv(env, "", "supervisor", "stop", "--wait") //nolint:errcheck
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			_ = os.RemoveAll(cityDir)
			if _, err := os.Stat(cityDir); os.IsNotExist(err) {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		beadsEntries, _ := os.ReadDir(filepath.Join(cityDir, ".beads"))
		t.Fatalf("review formula city cleanup did not quiesce; .beads entries=%v", beadsEntries)
	})

	return cityDir
}

func workflowAgentStartCommand(mode string, extraEnv map[string]string) string {
	parts := []string{"GC_GRAPH_MODE=" + mode}
	if len(extraEnv) > 0 {
		keys := make([]string, 0, len(extraEnv))
		for key := range extraEnv {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			parts = append(parts, key+"="+extraEnv[key])
		}
	}
	parts = append(parts, "bash", agentScript("graph-dispatch.sh"))
	return strings.Join(parts, " ")
}

func traceHasLineWithAll(trace string, tokens ...string) bool {
	return countTraceLinesWithAll(trace, tokens...) > 0
}

func countTraceLinesWithAll(trace string, tokens ...string) int {
	count := 0
	for _, line := range strings.Split(trace, "\n") {
		if line == "" {
			continue
		}
		matches := true
		for _, token := range tokens {
			if !strings.Contains(line, token) {
				matches = false
				break
			}
		}
		if matches {
			count++
		}
	}
	return count
}

func startReviewWorkflow(t *testing.T, cityDir, formula string, vars map[string]string) (string, string) {
	t.Helper()

	out, err := bdDolt(cityDir, "create", "--json", "Test review workflow")
	if err != nil {
		t.Fatalf("bd create failed: %v\noutput: %s", err, out)
	}
	var created graphBead
	if err := json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(out))), &created); err != nil {
		t.Fatalf("unmarshal: %v\njson: %s", err, out)
	}
	issueID := created.ID

	// Set issue var to the created bead ID.
	vars["issue"] = issueID

	args := []string{"sling", "worker", issueID, "--on=" + formula}
	for k, v := range vars {
		args = append(args, "--var", k+"="+v)
	}
	out, err = gcDolt(cityDir, args...)
	if err != nil {
		t.Fatalf("gc sling failed: %v\noutput: %s", err, out)
	}
	slingOutput := out

	if _, wid, err := waitForBeadMetadataValue(t, cityDir, issueID, "workflow_id", 10*time.Second); err == nil {
		return issueID, wid
	} else {
		issue := showBead(t, cityDir, issueID)
		t.Fatalf("timed out waiting for workflow_id on %s: %v\ngc sling output:\n%s\nsource bead:\n%+v", issueID, err, slingOutput, issue)
	}
	return "", ""
}

func listWorkflowSteps(t *testing.T, cityDir, workflowID string) []string {
	t.Helper()
	out, err := bdDolt(cityDir, "list", "--json", "--all", "--limit=0")
	if err != nil {
		t.Fatalf("bd list: %v\noutput: %s", err, out)
	}
	var beads []graphBead
	if err := json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(out))), &beads); err != nil {
		t.Fatalf("unmarshal beads: %v", err)
	}
	var refs []string
	for _, b := range beads {
		rootID := metaValue(b, "gc.root_bead_id")
		if rootID != workflowID {
			continue
		}
		ref := b.Ref
		if ref == "" {
			ref = metaValue(b, "gc.step_ref")
		}
		if ref != "" {
			refs = append(refs, ref)
		}
	}
	return refs
}

func hasStepWithSuffix(steps []string, suffix string) bool {
	for _, s := range steps {
		if strings.HasSuffix(s, suffix) || strings.HasSuffix(s, "."+suffix) {
			return true
		}
	}
	return false
}

func dumpWorkflowState(t *testing.T, cityDir, workflowID string) {
	t.Helper()
	out, _ := bdDolt(cityDir, "list", "--json", "--all", "--limit=0")
	t.Logf("all beads:\n%s", out)
	if traceFile := filepath.Join(cityDir, "graph-workflow-trace.log"); fileExists(traceFile) {
		data, _ := os.ReadFile(traceFile)
		t.Logf("agent trace:\n%s", string(data))
	}
}

func writeLocalFormula(t *testing.T, cityDir, name, body string) {
	t.Helper()

	path := filepath.Join(cityDir, "formulas", name+".toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func writeLocalExecutable(t *testing.T, path, body string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func installReviewFormulaFixtures(t *testing.T, cityDir string) {
	t.Helper()

	writeLocalFormula(t, cityDir, "expansion-review-pr", reviewworkflows.ExpansionReviewPR)
	writeLocalFormula(t, cityDir, "expansion-design-review", reviewworkflows.ExpansionDesignReview)
	writeLocalFormula(t, cityDir, "mol-adopt-pr-v2", reviewworkflows.AdoptPR)
	writeLocalFormula(t, cityDir, "mol-personal-work-v2", reviewworkflows.PersonalWork)

	checksDir := filepath.Join(cityDir, ".gc", "scripts", "checks")
	writeLocalExecutable(t, filepath.Join(checksDir, "adopt-pr-review-approved.sh"), testAdoptPRReviewCheck)
	writeLocalExecutable(t, filepath.Join(checksDir, "design-review-approved.sh"), testDesignReviewCheck)
	writeLocalExecutable(t, filepath.Join(checksDir, "code-review-approved.sh"), testCodeReviewCheck)
}
