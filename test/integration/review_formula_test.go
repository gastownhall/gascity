//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAdoptPRFormulaCompileAndRun validates that mol-adopt-pr-v2 compiles,
// materializes a bead graph with Ralph + compose.expand, and runs to
// completion under the subprocess provider with the graph-workflow agent.
func TestAdoptPRFormulaCompileAndRun(t *testing.T) {
	cityDir := setupReviewFormulaCity(t, "success")
	issueID, workflowID := startReviewWorkflow(t, cityDir, "mol-adopt-pr-v2", map[string]string{
		"issue":       "", // filled after create
		"pr_ref":      "refs/heads/test",
		"base_branch": "main",
		"skip_gemini": "false",
	})

	workflow := waitForBeadClosed(t, cityDir, workflowID, 180*time.Second)
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

// TestPersonalWorkFormulaCompileAndRun validates mol-personal-work-v2.
func TestPersonalWorkFormulaCompileAndRun(t *testing.T) {
	cityDir := setupReviewFormulaCity(t, "success")
	issueID, workflowID := startReviewWorkflow(t, cityDir, "mol-personal-work-v2", map[string]string{
		"issue":         "", // filled after create
		"base_branch":   "main",
		"skip_gemini":   "false",
		"setup_command": "true",
		"test_command":  "true",
	})

	workflow := waitForBeadClosed(t, cityDir, workflowID, 180*time.Second)
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
	cityDir := setupReviewFormulaCity(t, "success")
	_, workflowID := startReviewWorkflow(t, cityDir, "mol-adopt-pr-v2", map[string]string{
		"issue":       "",
		"pr_ref":      "refs/heads/test",
		"base_branch": "main",
		"skip_gemini": "true",
	})

	workflow := waitForBeadClosed(t, cityDir, workflowID, 180*time.Second)
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

// --- helpers ---

func setupReviewFormulaCity(t *testing.T, mode string) string {
	t.Helper()
	ensureGraphWorkflowSupervisor(t)

	var cityName string
	if usingSubprocess() {
		cityName = uniqueCityName()
	} else {
		cityName = "review-formula-test"
	}
	cityDir := filepath.Join(t.TempDir(), cityName)

	startCommand := "GC_GRAPH_MODE=" + mode + " bash " + agentScript("graph-workflow.sh")
	cityToml := fmt.Sprintf(
		"[workspace]\nname = %q\n\n[session]\nprovider = \"subprocess\"\n\n[daemon]\npatrol_interval = \"100ms\"\n\n"+
			"[[agent]]\nname = \"worker\"\nstart_command = %q\n\n"+
			"[[agent]]\nname = \"polecat\"\nstart_command = %q\n[agent.pool]\nmin = 0\nmax = 3\n",
		cityName, startCommand, startCommand,
	)
	configPath := filepath.Join(t.TempDir(), "review-formula.toml")
	if err := os.WriteFile(configPath, []byte(cityToml), 0o644); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	out, err := gcDolt("", "init", "--skip-provider-readiness", "--file", configPath, cityDir)
	if err != nil {
		t.Fatalf("gc init failed: %v\noutput: %s", err, out)
	}

	// Copy review formulas into the city's local formula directory.
	// The compose layer uses cityRoot/formulas/ as the city-local layer.
	formulaDir := filepath.Join(cityDir, "formulas")
	if err := os.MkdirAll(formulaDir, 0o755); err != nil {
		t.Fatalf("mkdir formulas: %v", err)
	}
	packFormulas := filepath.Join(repoRoot(t), "examples", "gastown", "packs", "gastown", "formulas")
	entries, err := os.ReadDir(packFormulas)
	if err != nil {
		t.Fatalf("reading pack formulas: %v", err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".formula.toml") {
			continue
		}
		src := filepath.Join(packFormulas, e.Name())
		dst := filepath.Join(formulaDir, e.Name())
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("reading %s: %v", src, err)
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			t.Fatalf("writing %s: %v", dst, err)
		}
	}

	// Also copy the check scripts.
	checksDir := filepath.Join(cityDir, ".gc", "scripts", "checks")
	if err := os.MkdirAll(checksDir, 0o755); err != nil {
		t.Fatalf("mkdir checks: %v", err)
	}
	packChecks := filepath.Join(repoRoot(t), "examples", "gastown", "packs", "gastown", "scripts", "checks")
	checkEntries, err := os.ReadDir(packChecks)
	if err != nil {
		t.Fatalf("reading pack checks: %v", err)
	}
	for _, e := range checkEntries {
		src := filepath.Join(packChecks, e.Name())
		dst := filepath.Join(checksDir, e.Name())
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("reading %s: %v", src, err)
		}
		if err := os.WriteFile(dst, data, 0o755); err != nil {
			t.Fatalf("writing %s: %v", dst, err)
		}
	}

	out, err = gcDolt("", "start", cityDir)
	if err != nil {
		t.Fatalf("gc start failed: %v\noutput: %s", err, out)
	}
	t.Cleanup(func() {
		gcDolt("", "stop", cityDir) //nolint:errcheck
	})

	return cityDir
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
		if rootID == workflowID && b.Ref != "" {
			refs = append(refs, b.Ref)
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

func repoRoot(t *testing.T) string {
	t.Helper()
	// Walk up from the test file to find go.mod.
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repo root (go.mod)")
		}
		dir = parent
	}
}
