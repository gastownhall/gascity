//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/test/tmuxtest"
)

type graphBead struct {
	ID       string         `json:"id"`
	Ref      string         `json:"ref"`
	Status   string         `json:"status"`
	Type     string         `json:"type"`
	Metadata map[string]any `json:"metadata"`
}

func metaValue(bead graphBead, key string) string {
	if bead.Metadata == nil {
		return ""
	}
	raw, ok := bead.Metadata[key]
	if !ok || raw == nil {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return v
	case bool:
		return strconv.FormatBool(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}

var graphWorkflowSupervisorOnce sync.Once

func ensureGraphWorkflowSupervisor(t *testing.T) {
	t.Helper()

	graphWorkflowSupervisorOnce.Do(func() {
		out, err := gcDolt("", "supervisor", "start")
		if err == nil {
			return
		}
		if strings.Contains(out, "already running") {
			return
		}
		t.Fatalf("gc supervisor start failed: %v\noutput: %s", err, out)
	})
}

func TestGraphWorkflowSuccessPath(t *testing.T) {
	cityDir := setupGraphWorkflowCity(t, "success")
	issueID, workflowID := startScopedWorkflow(t, cityDir)

	workflow := waitForBeadClosed(t, cityDir, workflowID, 180*time.Second)
	if got := metaValue(workflow, "gc.outcome"); got != "pass" {
		t.Fatalf("workflow outcome = %q, want pass", got)
	}

	body := mustFindWorkflowBeadByRefSuffix(t, cityDir, workflowID, ".body")
	if got := metaValue(body, "gc.outcome"); got != "pass" {
		t.Fatalf("body outcome = %q, want pass", got)
	}

	issue := showBead(t, cityDir, issueID)
	if got := metaValue(issue, "work_dir"); got != "" {
		t.Fatalf("issue work_dir = %q, want unset after cleanup", got)
	}
	if got := metaValue(issue, "submitted"); got != "true" {
		t.Fatalf("issue submitted = %q, want true", got)
	}

	worktreePath := filepath.Join(cityDir, "worktrees", issueID)
	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree path %s should be removed, stat err=%v", worktreePath, err)
	}

	report := readWorkflowReport(t, cityDir)
	for _, suffix := range []string{
		".load-context",
		".workspace-setup",
		".preflight-tests",
		".implement",
		".self-review",
		".submit",
		".cleanup-worktree",
	} {
		if !strings.Contains(report, suffix) {
			t.Fatalf("report missing %s:\n%s", suffix, report)
		}
	}

	assertWorkflowControlLane(t, cityDir)
}

func TestGraphWorkflowFailureRunsCleanup(t *testing.T) {
	cityDir := setupGraphWorkflowCity(t, "fail-preflight")
	issueID, workflowID := startScopedWorkflow(t, cityDir)

	workflow := waitForBeadClosed(t, cityDir, workflowID, 180*time.Second)
	if got := metaValue(workflow, "gc.outcome"); got != "fail" {
		t.Fatalf("workflow outcome = %q, want fail", got)
	}

	body := mustFindWorkflowBeadByRefSuffix(t, cityDir, workflowID, ".body")
	if got := metaValue(body, "gc.outcome"); got != "fail" {
		t.Fatalf("body outcome = %q, want fail", got)
	}

	issue := showBead(t, cityDir, issueID)
	if got := metaValue(issue, "work_dir"); got != "" {
		t.Fatalf("issue work_dir = %q, want unset after cleanup", got)
	}
	if got := metaValue(issue, "submitted"); got != "" {
		t.Fatalf("issue submitted = %q, want unset on failed workflow", got)
	}

	for _, suffix := range []string{".implement", ".self-review", ".submit"} {
		bead := mustFindWorkflowBeadByRefSuffix(t, cityDir, workflowID, suffix)
		if bead.Status != "closed" {
			t.Fatalf("%s status = %q, want closed", suffix, bead.Status)
		}
		if got := metaValue(bead, "gc.outcome"); got != "skipped" {
			t.Fatalf("%s outcome = %q, want skipped", suffix, got)
		}
	}

	report := readWorkflowReport(t, cityDir)
	for _, suffix := range []string{".load-context", ".workspace-setup", ".preflight-tests", ".cleanup-worktree"} {
		if !strings.Contains(report, suffix) {
			t.Fatalf("report missing %s:\n%s", suffix, report)
		}
	}
	for _, suffix := range []string{".implement", ".self-review", ".submit"} {
		if strings.Contains(report, suffix) {
			t.Fatalf("report should not include %s after abort:\n%s", suffix, report)
		}
	}

	assertWorkflowControlLane(t, cityDir)
}

func assertWorkflowControlLane(t *testing.T, cityDir string) {
	t.Helper()

	workflowTrace := readOptionalFile(filepath.Join(cityDir, "workflow-control-trace.log"))
	if !strings.Contains(workflowTrace, "serve process bead=") {
		t.Fatalf("workflow-control trace missing processed control bead evidence:\n%s", workflowTrace)
	}

	workerTrace := readOptionalFile(filepath.Join(cityDir, "graph-workflow-trace.log"))
	if strings.Contains(workerTrace, "unexpected-control") {
		t.Fatalf("worker should not receive control beads:\n%s", workerTrace)
	}
}

func setupGraphWorkflowCity(t *testing.T, mode string) string {
	t.Helper()

	ensureGraphWorkflowSupervisor(t)

	var cityName string
	if usingSubprocess() {
		cityName = uniqueCityName()
	} else {
		cityName = tmuxtest.NewGuard(t).CityName()
	}
	cityDir := filepath.Join(t.TempDir(), cityName)

	startCommand := "GC_GRAPH_MODE=" + mode + " bash " + agentScript("graph-workflow.sh")
	cityToml := fmt.Sprintf("[workspace]\nname = %q\n\n[session]\nprovider = \"subprocess\"\n\n[daemon]\npatrol_interval = \"100ms\"\n\n[[agent]]\nname = \"worker\"\nstart_command = %q\n", cityName, startCommand)
	configPath := filepath.Join(t.TempDir(), "graph-workflow.toml")
	if err := os.WriteFile(configPath, []byte(cityToml), 0o644); err != nil {
		t.Fatalf("writing graph workflow config: %v", err)
	}

	out, err := gcDolt("", "init", "--skip-provider-readiness", "--file", configPath, cityDir)
	if err != nil {
		t.Fatalf("gc init --file failed: %v\noutput: %s", err, out)
	}
	out, err = gcDolt("", "start", cityDir)
	if err != nil {
		t.Fatalf("gc start failed: %v\noutput: %s", err, out)
	}
	t.Cleanup(func() {
		gcDolt("", "stop", cityDir) //nolint:errcheck // best-effort cleanup
	})

	return cityDir
}

func startScopedWorkflow(t *testing.T, cityDir string) (string, string) {
	t.Helper()

	out, err := bdDolt(cityDir, "create", "--json", "Run built-in scoped workflow")
	if err != nil {
		t.Fatalf("bd create failed: %v\noutput: %s", err, out)
	}
	var created graphBead
	if err := json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(out))), &created); err != nil {
		t.Fatalf("unmarshal created issue: %v\njson: %s", err, out)
	}
	issueID := created.ID
	if issueID == "" {
		t.Fatalf("bd create returned empty issue id\njson: %s", out)
	}

	out, err = gcDolt(cityDir, "sling", "worker", issueID, "--on=mol-scoped-work", "--var", "issue="+issueID)
	if err != nil {
		t.Fatalf("gc sling failed: %v\noutput: %s", err, out)
	}
	slingOutput := out

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		issue := showBead(t, cityDir, issueID)
		if workflowID := metaValue(issue, "workflow_id"); workflowID != "" {
			return issueID, workflowID
		}
		time.Sleep(200 * time.Millisecond)
	}

	issue := showBead(t, cityDir, issueID)
	t.Fatalf("timed out waiting for workflow_id on source bead %s\ngc sling output:\n%s\nsource bead:\n%+v", issueID, slingOutput, issue)
	return "", ""
}

func waitForBeadClosed(t *testing.T, cityDir, beadID string, timeout time.Duration) graphBead {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		bead := showBead(t, cityDir, beadID)
		if bead.Status == "closed" {
			return bead
		}
		time.Sleep(200 * time.Millisecond)
	}

	out, err := bdDolt(cityDir, "list", "--json", "--all", "--limit=0")
	if err != nil {
		t.Fatalf("timed out waiting for bead %s to close; bd list failed: %v\noutput: %s", beadID, err, out)
	}
	readyOut, readyErr := bdDolt(cityDir, "ready", "--json", "--limit=0")
	if readyErr != nil {
		readyOut = fmt.Sprintf("bd ready failed: %v\noutput: %s", readyErr, readyOut)
	}
	readyAssigneeOut, readyAssigneeErr := bdDolt(cityDir, "ready", "--json", "--limit=0", "--assignee=worker")
	if readyAssigneeErr != nil {
		readyAssigneeOut = fmt.Sprintf("bd ready --assignee failed: %v\noutput: %s", readyAssigneeErr, readyAssigneeOut)
	}
	sessionListOut, sessionListErr := gcDolt(cityDir, "session", "list")
	if sessionListErr != nil {
		sessionListOut = fmt.Sprintf("gc session list failed: %v\noutput: %s", sessionListErr, sessionListOut)
	}
	sessionPeekOut, sessionPeekErr := gcDolt(cityDir, "session", "peek", "worker")
	if sessionPeekErr != nil {
		sessionPeekOut = fmt.Sprintf("gc session peek worker failed: %v\noutput: %s", sessionPeekErr, sessionPeekOut)
	}
	traceOut := readOptionalFile(filepath.Join(cityDir, "graph-workflow-trace.log"))
	workflowTraceOut := readOptionalFile(filepath.Join(cityDir, "workflow-control-trace.log"))
	t.Fatalf("timed out waiting for bead %s to close\nready:\n%s\nready worker:\n%s\nsessions:\n%s\nworker peek:\n%s\ntrace:\n%s\nworkflow trace:\n%s\nbeads:\n%s",
		beadID, readyOut, readyAssigneeOut, sessionListOut, sessionPeekOut, traceOut, workflowTraceOut, out)
	return graphBead{}
}

func readOptionalFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("read %s failed: %v", path, err)
	}
	return string(data)
}

func showBead(t *testing.T, cityDir, beadID string) graphBead {
	t.Helper()

	out, err := bdDolt(cityDir, "show", beadID, "--json")
	if err != nil {
		t.Fatalf("bd show --json %s failed: %v\noutput: %s", beadID, err, out)
	}
	var bead graphBead
	trimmed := strings.TrimSpace(extractJSONPayload(out))
	if err := json.Unmarshal([]byte(trimmed), &bead); err == nil {
		return bead
	}
	var beads []graphBead
	if err := json.Unmarshal([]byte(trimmed), &beads); err != nil {
		t.Fatalf("unmarshal bead %s: %v\njson: %s", beadID, err, out)
	}
	if len(beads) == 0 {
		t.Fatalf("bd show --json %s returned no beads\njson: %s", beadID, out)
	}
	return beads[0]
}

func mustFindWorkflowBeadByRefSuffix(t *testing.T, cityDir, workflowID, suffix string) graphBead {
	t.Helper()

	out, err := bdDolt(cityDir, "list", "--json", "--all", "--limit=0")
	if err != nil {
		t.Fatalf("bd list --json failed: %v\noutput: %s", err, out)
	}
	var beads []graphBead
	if err := json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(out))), &beads); err != nil {
		t.Fatalf("unmarshal bead list: %v\njson: %s", err, out)
	}
	for _, bead := range beads {
		ref := bead.Ref
		if ref == "" {
			ref = metaValue(bead, "gc.step_ref")
		}
		if metaValue(bead, "gc.root_bead_id") == workflowID && strings.HasSuffix(ref, suffix) {
			return bead
		}
	}
	t.Fatalf("no bead with ref suffix %s found for workflow %s", suffix, workflowID)
	return graphBead{}
}

func extractJSONPayload(raw string) string {
	data := []byte(raw)
	objStart := bytes.IndexByte(data, '{')
	arrStart := bytes.IndexByte(data, '[')

	switch {
	case objStart >= 0 && arrStart >= 0:
		if arrStart < objStart {
			return string(data[arrStart:])
		}
		return string(data[objStart:])
	case objStart >= 0:
		return string(data[objStart:])
	case arrStart >= 0:
		return string(data[arrStart:])
	default:
		return raw
	}
}

func readWorkflowReport(t *testing.T, cityDir string) string {
	t.Helper()

	path := filepath.Join(cityDir, "graph-workflow-steps.log")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && len(strings.TrimSpace(string(data))) > 0 {
			return string(data)
		}
		time.Sleep(100 * time.Millisecond)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading workflow report: %v", err)
	}
	return string(data)
}
