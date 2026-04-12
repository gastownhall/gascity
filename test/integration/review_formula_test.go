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
)

const reviewWorkflowTimeout = 8 * time.Minute

const testReviewExpansionFormula = `description = """
Test-local review expansion used by integration tests.
Exercises compose.expand, pooled reviewer fan-out, Gemini soft-fail retries,
and synthesis without depending on private production formulas.
"""
formula = "expansion-review-pr"
version = 2
type = "expansion"

[vars.skip_gemini]
description = "Skip Gemini reviewer"
default = "false"

[[template]]
id = "{target}.review-claude"
title = "Code review: Claude"
assignee = "polecat"
description = "Claude review lane."

[template.retry]
max_attempts = 3
on_exhausted = "hard_fail"

[[template]]
id = "{target}.review-codex"
title = "Code review: Codex"
assignee = "polecat"
description = "Codex review lane."

[template.retry]
max_attempts = 3
on_exhausted = "hard_fail"

[[template]]
id = "{target}.review-gemini"
title = "Code review: Gemini"
assignee = "polecat"
condition = "!{{skip_gemini}}"
description = """
Optional Gemini lane. If unavailable or rate limited, close the attempt as a
transient failure with reason rate_limited so runtime can retry and
eventually soft-fail this logical step.
"""

[template.retry]
max_attempts = 3
on_exhausted = "soft_fail"

[[template]]
id = "{target}.synthesize"
title = "Synthesize review findings"
needs = ["{target}.review-claude", "{target}.review-codex", "{target}.review-gemini"]
assignee = "worker"
description = "Merge available reviewer outputs."

[template.retry]
max_attempts = 3
on_exhausted = "hard_fail"
`

const testDesignExpansionFormula = `description = """
Test-local design review expansion used by integration tests.
Exercises a second compose.expand path, pooled persona generation/review fan-out,
Gemini soft-fail retries, and final synthesis without depending on private
production formulas.
"""
formula = "expansion-design-review"
version = 2
type = "expansion"

[vars.skip_gemini]
description = "Skip Gemini reviewer"
default = "false"

[[template]]
id = "{target}.persona-gen-claude"
title = "Generate personas: Claude"
assignee = "polecat"
description = "Claude persona generation lane."

[template.retry]
max_attempts = 3
on_exhausted = "hard_fail"

[[template]]
id = "{target}.persona-gen-codex"
title = "Generate personas: Codex"
assignee = "polecat"
description = "Codex persona generation lane."

[template.retry]
max_attempts = 3
on_exhausted = "hard_fail"

[[template]]
id = "{target}.persona-gen-gemini"
title = "Generate personas: Gemini"
assignee = "polecat"
condition = "!{{skip_gemini}}"
description = "Optional Gemini persona generation lane."

[template.retry]
max_attempts = 3
on_exhausted = "soft_fail"

[[template]]
id = "{target}.persona-synthesis"
title = "Synthesize personas"
needs = ["{target}.persona-gen-claude", "{target}.persona-gen-codex", "{target}.persona-gen-gemini"]
assignee = "worker"
description = "Merge persona suggestions."

[template.retry]
max_attempts = 3
on_exhausted = "hard_fail"

[[template]]
id = "{target}.persona-reviews-claude"
title = "Persona reviews: Claude"
needs = ["{target}.persona-synthesis"]
assignee = "polecat"
description = "Claude persona review batch."

[template.retry]
max_attempts = 3
on_exhausted = "hard_fail"

[[template]]
id = "{target}.persona-reviews-codex"
title = "Persona reviews: Codex"
needs = ["{target}.persona-synthesis"]
assignee = "polecat"
description = "Codex persona review batch."

[template.retry]
max_attempts = 3
on_exhausted = "hard_fail"

[[template]]
id = "{target}.persona-reviews-gemini"
title = "Persona reviews: Gemini"
needs = ["{target}.persona-synthesis"]
assignee = "polecat"
condition = "!{{skip_gemini}}"
description = "Optional Gemini persona review batch."

[template.retry]
max_attempts = 3
on_exhausted = "soft_fail"

[[template]]
id = "{target}.review-synthesis"
title = "Synthesize design review"
needs = ["{target}.persona-reviews-claude", "{target}.persona-reviews-codex", "{target}.persona-reviews-gemini"]
assignee = "worker"
description = "Merge design review findings."

[template.retry]
max_attempts = 3
on_exhausted = "hard_fail"
`

const testAdoptPRFormula = `description = """
Test-local adopt-pr workflow used by integration tests.
Exercises a body scope, setup retries, a Ralph loop, compose.expand fan-out,
Gemini soft-fail retries, finalize, and teardown.
"""
formula = "mol-adopt-pr-v2"
version = 2

[vars]
[vars.issue]
required = true

[vars.base_branch]
default = "main"

[vars.pr_ref]
required = true

[vars.skip_gemini]
default = "false"

[[steps]]
id = "body"
title = "Adopt PR body"
needs = ["preflight", "rebase-check", "review-loop", "finalize"]
description = "Terminal latch for the workflow body."
metadata = { "gc.kind" = "scope", "gc.scope_name" = "adopt-pr", "gc.scope_role" = "body" }

[[steps]]
id = "preflight"
title = "Preflight"
description = "Read the source bead and prime the city."
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "setup", "gc.on_fail" = "abort_scope" }

[steps.retry]
max_attempts = 3
on_exhausted = "hard_fail"

[[steps]]
id = "rebase-check"
title = "Prepare worktree"
needs = ["preflight"]
description = "Prepare worktree metadata for the review loop."
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "setup", "gc.on_fail" = "abort_scope" }

[steps.retry]
max_attempts = 3
on_exhausted = "hard_fail"

[[steps]]
id = "review-loop"
title = "Review loop"
needs = ["rebase-check"]
description = "Ralph loop for iterative review and fixes."
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "member", "gc.on_fail" = "abort_scope" }

[steps.ralph]
max_attempts = 5

[steps.ralph.check]
mode = "exec"
path = ".gc/scripts/checks/adopt-pr-review-approved.sh"
timeout = "10m"

[[steps.children]]
id = "review-pipeline"
title = "Review pipeline"
description = "Expanded via compose.expand."

[[steps.children]]
id = "apply-fixes"
title = "Apply fixes"
needs = ["review-pipeline"]
description = "Apply review feedback and mark the Ralph verdict."

[steps.children.retry]
max_attempts = 3
on_exhausted = "hard_fail"

[compose]
[[compose.expand]]
target = "review-pipeline"
with = "expansion-review-pr"
vars = { skip_gemini = "{skip_gemini}" }

[[steps]]
id = "finalize"
title = "Finalize"
needs = ["review-loop"]
description = "Finalize the review workflow."
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "member", "gc.on_fail" = "abort_scope" }

[steps.retry]
max_attempts = 3
on_exhausted = "hard_fail"

[[steps]]
id = "cleanup-worktree"
title = "Cleanup worktree"
needs = ["body"]
description = "Teardown after the body reaches terminal state."
metadata = { "gc.kind" = "cleanup", "gc.scope_ref" = "body", "gc.scope_role" = "teardown" }

[steps.retry]
max_attempts = 3
on_exhausted = "hard_fail"
`

const testPersonalWorkFormula = `description = """
Test-local personal-work workflow used by integration tests.
Exercises two Ralph loops, two compose.expand sites, pooled fan-out,
Gemini soft-fail retries, and body teardown without depending on private
production formulas.
"""
formula = "mol-personal-work-v2"
version = 2

[vars]
[vars.issue]
required = true

[vars.base_branch]
default = "main"

[vars.skip_gemini]
default = "false"

[vars.setup_command]
default = ""

[vars.test_command]
default = ""

[[steps]]
id = "body"
title = "Personal work body"
needs = ["load-context", "workspace-setup", "design-review-loop", "implement", "code-review-loop", "submit"]
description = "Terminal latch for the workflow body."
metadata = { "gc.kind" = "scope", "gc.scope_name" = "work", "gc.scope_role" = "body" }

[[steps]]
id = "load-context"
title = "Load context"
description = "Inspect the assigned work bead."

[steps.retry]
max_attempts = 3
on_exhausted = "hard_fail"

[[steps]]
id = "workspace-setup"
title = "Prepare worktree"
needs = ["load-context"]
description = "Prepare worktree metadata for the workflow."
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "setup", "gc.on_fail" = "abort_scope" }

[steps.retry]
max_attempts = 3
on_exhausted = "hard_fail"

[[steps]]
id = "design-review-loop"
title = "Design review loop"
needs = ["workspace-setup"]
description = "Ralph loop for iterative design review."
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "member", "gc.on_fail" = "abort_scope" }

[steps.ralph]
max_attempts = 5

[steps.ralph.check]
mode = "exec"
path = ".gc/scripts/checks/design-review-approved.sh"
timeout = "10m"

[[steps.children]]
id = "design-review-pipeline"
title = "Design review pipeline"
description = "Expanded via compose.expand."

[[steps.children]]
id = "apply-design-changes"
title = "Apply design changes"
needs = ["design-review-pipeline"]
description = "Apply design review feedback and mark the Ralph verdict."

[steps.children.retry]
max_attempts = 3
on_exhausted = "hard_fail"

[[steps]]
id = "implement"
title = "Implement"
needs = ["design-review-loop"]
description = "Perform the main work."
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "member", "gc.on_fail" = "abort_scope" }

[steps.retry]
max_attempts = 3
on_exhausted = "hard_fail"

[[steps]]
id = "code-review-loop"
title = "Code review loop"
needs = ["implement"]
description = "Ralph loop for iterative code review."
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "member", "gc.on_fail" = "abort_scope" }

[steps.ralph]
max_attempts = 5

[steps.ralph.check]
mode = "exec"
path = ".gc/scripts/checks/code-review-approved.sh"
timeout = "10m"

[[steps.children]]
id = "review-pipeline"
title = "Code review pipeline"
description = "Expanded via compose.expand."

[[steps.children]]
id = "apply-code-fixes"
title = "Apply code fixes"
needs = ["review-pipeline"]
description = "Apply code review feedback and mark the Ralph verdict."

[steps.children.retry]
max_attempts = 3
on_exhausted = "hard_fail"

[compose]
[[compose.expand]]
target = "design-review-pipeline"
with = "expansion-design-review"
vars = { skip_gemini = "{skip_gemini}" }

[[compose.expand]]
target = "review-pipeline"
with = "expansion-review-pr"
vars = { skip_gemini = "{skip_gemini}" }

[[steps]]
id = "submit"
title = "Submit"
needs = ["code-review-loop"]
description = "Finalize the work item."
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "member", "gc.on_fail" = "abort_scope" }

[steps.retry]
max_attempts = 3
on_exhausted = "hard_fail"

[[steps]]
id = "cleanup-worktree"
title = "Cleanup worktree"
needs = ["body"]
description = "Teardown after the body reaches terminal state."
metadata = { "gc.kind" = "cleanup", "gc.scope_ref" = "body", "gc.scope_role" = "teardown" }

[steps.retry]
max_attempts = 3
on_exhausted = "hard_fail"
`

const testAdoptPRReviewCheck = `#!/usr/bin/env bash
set -euo pipefail

BEAD_ID="${GC_BEAD_ID:-}"
[ -n "$BEAD_ID" ] || exit 1

BEAD_JSON=$(bd show "$BEAD_ID" --json 2>/dev/null)
ATTEMPT=$(printf '%s\n' "$BEAD_JSON" | jq -r 'if type == "array" then (.[0].metadata["gc.attempt"] // "") else (.metadata["gc.attempt"] // "") end')
ROOT_ID=$(printf '%s\n' "$BEAD_JSON" | jq -r 'if type == "array" then (.[0].metadata["gc.root_bead_id"] // "") else (.metadata["gc.root_bead_id"] // "") end')
[ -n "$ATTEMPT" ] && [ -n "$ROOT_ID" ] || exit 1

REF="mol-adopt-pr-v2.review-loop.run.${ATTEMPT}.apply-fixes"
VERDICT=$(
  bd list --all --json --limit=0 2>/dev/null |
    jq -r --arg ref "$REF" --arg root "$ROOT_ID" '
      [ .[] | select(.metadata["gc.step_ref"] == $ref and .metadata["gc.root_bead_id"] == $root) | .metadata["review.verdict"] ] | first // ""
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

BEAD_JSON=$(bd show "$BEAD_ID" --json 2>/dev/null)
ATTEMPT=$(printf '%s\n' "$BEAD_JSON" | jq -r 'if type == "array" then (.[0].metadata["gc.attempt"] // "") else (.metadata["gc.attempt"] // "") end')
ROOT_ID=$(printf '%s\n' "$BEAD_JSON" | jq -r 'if type == "array" then (.[0].metadata["gc.root_bead_id"] // "") else (.metadata["gc.root_bead_id"] // "") end')
[ -n "$ATTEMPT" ] && [ -n "$ROOT_ID" ] || exit 1

REF="mol-personal-work-v2.design-review-loop.run.${ATTEMPT}.apply-design-changes"
VERDICT=$(
  bd list --all --json --limit=0 2>/dev/null |
    jq -r --arg ref "$REF" --arg root "$ROOT_ID" '
      [ .[] | select(.metadata["gc.step_ref"] == $ref and .metadata["gc.root_bead_id"] == $root) | .metadata["design_review.verdict"] ] | first // ""
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

BEAD_JSON=$(bd show "$BEAD_ID" --json 2>/dev/null)
ATTEMPT=$(printf '%s\n' "$BEAD_JSON" | jq -r 'if type == "array" then (.[0].metadata["gc.attempt"] // "") else (.metadata["gc.attempt"] // "") end')
ROOT_ID=$(printf '%s\n' "$BEAD_JSON" | jq -r 'if type == "array" then (.[0].metadata["gc.root_bead_id"] // "") else (.metadata["gc.root_bead_id"] // "") end')
[ -n "$ATTEMPT" ] && [ -n "$ROOT_ID" ] || exit 1

REF="mol-personal-work-v2.code-review-loop.run.${ATTEMPT}.apply-code-fixes"
VERDICT=$(
  bd list --all --json --limit=0 2>/dev/null |
    jq -r --arg ref "$REF" --arg root "$ROOT_ID" '
      [ .[] | select(.metadata["gc.step_ref"] == $ref and .metadata["gc.root_bead_id"] == $root) | .metadata["code_review.verdict"] ] | first // ""
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
		"review-loop.check.1",
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
		"design-review-loop.check.1",
		"code-review-loop.check.1",
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

// TestAdoptPRSkipGemini validates that skip_gemini=true omits the Gemini step.
func TestAdoptPRSkipGemini(t *testing.T) {
	cityDir := setupReviewFormulaCity(t, "success", nil)
	_, workflowID := startReviewWorkflow(t, cityDir, "mol-adopt-pr-v2", map[string]string{
		"issue":       "",
		"pr_ref":      "refs/heads/test",
		"base_branch": "main",
		"skip_gemini": "true",
	})

	workflow := waitForBeadClosed(t, cityDir, workflowID, reviewWorkflowTimeout)
	if got := metaValue(workflow, "gc.outcome"); got != "pass" {
		dumpWorkflowState(t, cityDir, workflowID)
		t.Fatalf("workflow outcome = %q, want pass", got)
	}

	steps := listWorkflowSteps(t, cityDir, workflowID)
	if hasStepWithSuffix(steps, "review-gemini") {
		t.Errorf("Gemini step should be omitted with skip_gemini=true; got: %v", steps)
	}
	if !hasStepWithSuffix(steps, "review-claude") {
		t.Errorf("Claude step missing; got: %v", steps)
	}
}

func TestAdoptPRFormulaRetriesTransientReviewerStep(t *testing.T) {
	cityDir := setupReviewFormulaCity(t, "success", map[string]string{
		"GC_GRAPH_TRANSIENT_ONCE_SUFFIXES": "review-pipeline.review-codex.run.1",
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
	if !hasStepWithSuffix(steps, "review-pipeline.review-codex.run.2") {
		t.Fatalf("missing retry attempt for codex reviewer; got: %v", steps)
	}

	logical := mustFindWorkflowBeadByRefSuffix(t, cityDir, workflowID, "review-pipeline.review-codex")
	if got := metaValue(logical, "gc.outcome"); got != "pass" {
		t.Fatalf("review-codex logical outcome = %q, want pass", got)
	}
}

func TestAdoptPRFormulaSoftFailsGeminiAfterTransientRetries(t *testing.T) {
	cityDir := setupReviewFormulaCity(t, "success", map[string]string{
		"GC_GRAPH_ALWAYS_TRANSIENT_SUFFIXES": "review-pipeline.review-gemini.run.",
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
		"review-pipeline.review-gemini.run.2",
		"review-pipeline.review-gemini.run.3",
	} {
		if !hasStepWithSuffix(steps, suffix) {
			t.Fatalf("missing Gemini retry attempt %q; got: %v", suffix, steps)
		}
	}

	logical := mustFindWorkflowBeadByRefSuffix(t, cityDir, workflowID, "review-pipeline.review-gemini")
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
		"GC_GRAPH_TRANSIENT_ONCE_SUFFIXES":        "review.run.1",
		"GC_GRAPH_EXIT_AFTER_CLAIM_ONCE_SUFFIXES": "review.run.2",
	})
	writeLocalFormula(t, cityDir, "mol-retry-recovery-smoke", `description = """
Minimal pooled retry workflow used to verify crash-before-result recovery.
"""
formula = "mol-retry-recovery-smoke"
version = 2

[[steps]]
id = "review"
title = "Single pooled retry step"
assignee = "polecat"
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
	if !hasStepWithSuffix(steps, "review.run.2") {
		t.Fatalf("missing retry attempt after transient failure; got: %v", steps)
	}

	logical := mustFindWorkflowBeadByRefSuffix(t, cityDir, workflowID, ".review")
	if got := metaValue(logical, "gc.outcome"); got != "pass" {
		t.Fatalf("logical review outcome = %q, want pass", got)
	}

	trace := readOptionalFile(filepath.Join(cityDir, "graph-workflow-trace.log"))
	if !strings.Contains(trace, "exit-after-claim") {
		t.Fatalf("worker trace missing forced crash evidence:\n%s", trace)
	}
	if !strings.Contains(trace, "resume bead=") {
		t.Fatalf("worker trace missing claimed-attempt resume evidence:\n%s", trace)
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
			"[[agent]]\nname = \"polecat\"\nstart_command = %q\n[agent.pool]\nmin = 0\nmax = 3\n",
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

	out, err := runGCDoltWithEnv(env, "", "init", "--skip-provider-readiness", "--file", configPath, cityDir)
	if err != nil {
		t.Fatalf("gc init failed: %v\noutput: %s", err, out)
	}
	registerCityCommandEnv(cityDir, env)
	t.Cleanup(func() {
		unregisterCityCommandEnv(cityDir)
		runGCDoltWithEnv(env, "", "stop", cityDir)      //nolint:errcheck
		runGCDoltWithEnv(env, "", "supervisor", "stop") //nolint:errcheck
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

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		issue := showBead(t, cityDir, issueID)
		if wid := metaValue(issue, "workflow_id"); wid != "" {
			return issueID, wid
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for workflow_id on %s", issueID)
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

	path := filepath.Join(cityDir, "formulas", name+".formula.toml")
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

	writeLocalFormula(t, cityDir, "expansion-review-pr", testReviewExpansionFormula)
	writeLocalFormula(t, cityDir, "expansion-design-review", testDesignExpansionFormula)
	writeLocalFormula(t, cityDir, "mol-adopt-pr-v2", testAdoptPRFormula)
	writeLocalFormula(t, cityDir, "mol-personal-work-v2", testPersonalWorkFormula)

	checksDir := filepath.Join(cityDir, ".gc", "scripts", "checks")
	writeLocalExecutable(t, filepath.Join(checksDir, "adopt-pr-review-approved.sh"), testAdoptPRReviewCheck)
	writeLocalExecutable(t, filepath.Join(checksDir, "design-review-approved.sh"), testDesignReviewCheck)
	writeLocalExecutable(t, filepath.Join(checksDir, "code-review-approved.sh"), testCodeReviewCheck)
}
