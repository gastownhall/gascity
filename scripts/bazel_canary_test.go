package scripts_test

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type canarySelection struct {
	Labels       []string `json:"labels"`
	Conservative bool     `json:"conservative"`
	Reason       string   `json:"reason"`
	Error        *string  `json:"error"`
}

func TestBazelCanaryResolveOnlySkipsUnrelatedDiff(t *testing.T) {
	root := repoRoot(t)
	diff := writeDiffFixture(t, "M\tdocs/guide.md\x00")
	result, code := runBazelCanary(t, root, "", "", "resolve", []string{"CHANGED_PATHS_FILE=" + diff})
	if code != 0 {
		t.Fatalf("resolve-only exit = %d, output:\n%s", code, result.output)
	}
	if result.selection.Reason != "unrelated" || result.selection.Conservative || len(result.selection.Labels) != 0 {
		t.Fatalf("selection = %#v, want confident unrelated zero-target result", result.selection)
	}
	if !strings.Contains(result.outputs, "run_bazel=false\n") {
		t.Fatalf("GITHUB_OUTPUT missing skip decision:\n%s", result.outputs)
	}
	if _, err := os.Stat(filepath.Join(result.outDir, "go.log")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("resolve-only unexpectedly attempted Go/Bazel work: %v", err)
	}
}

func TestBazelCanaryResolveOnlySelectsMappedTarget(t *testing.T) {
	root := repoRoot(t)
	diff := writeDiffFixture(t, "M\tinternal/config/storage_endpoint.go\x00")
	result, code := runBazelCanary(t, root, "", "", "resolve", []string{"CHANGED_PATHS_FILE=" + diff})
	if code != 0 {
		t.Fatalf("resolve-only exit = %d, output:\n%s", code, result.output)
	}
	want := "//internal/config:config_storage_endpoint_test"
	if result.selection.Reason != "mapped" || result.selection.Conservative || len(result.selection.Labels) != 1 || result.selection.Labels[0] != want {
		t.Fatalf("selection = %#v, want mapped %s", result.selection, want)
	}
	if !strings.Contains(result.outputs, "run_bazel=true\n") {
		t.Fatalf("GITHUB_OUTPUT missing run decision:\n%s", result.outputs)
	}
}

func TestBazelCanaryResolveOnlySelectsIdentityTarget(t *testing.T) {
	root := repoRoot(t)
	diff := writeDiffFixture(t, "M\tinternal/config/identity_seam.go\x00")
	result, code := runBazelCanary(t, root, "", "", "resolve", []string{"CHANGED_PATHS_FILE=" + diff})
	if code != 0 {
		t.Fatalf("resolve-only exit = %d, output:\n%s", code, result.output)
	}
	want := "//internal/config:config_identity_seam_test"
	if result.selection.Reason != "mapped" || result.selection.Conservative || len(result.selection.Labels) != 1 || result.selection.Labels[0] != want {
		t.Fatalf("selection = %#v, want mapped %s", result.selection, want)
	}
	if !strings.Contains(result.outputs, "run_bazel=true\n") {
		t.Fatalf("GITHUB_OUTPUT missing run decision:\n%s", result.outputs)
	}
}

func TestBazelCanaryResolveOnlySelectsDiagnosticEmbedTarget(t *testing.T) {
	root := repoRoot(t)
	diff := writeDiffFixture(t, "M\tinternal/config/testdata/diagnostic_locator.toml\x00")
	result, code := runBazelCanary(t, root, "", "", "resolve", []string{"CHANGED_PATHS_FILE=" + diff})
	if code != 0 {
		t.Fatalf("resolve-only exit = %d, output:\n%s", code, result.output)
	}
	want := "//internal/config:config_diagnostic_locations_test"
	if result.selection.Reason != "mapped" || result.selection.Conservative || len(result.selection.Labels) != 1 || result.selection.Labels[0] != want {
		t.Fatalf("selection = %#v, want mapped %s", result.selection, want)
	}
	if !strings.Contains(result.outputs, "run_bazel=true\n") {
		t.Fatalf("GITHUB_OUTPUT missing run decision:\n%s", result.outputs)
	}
}

func TestBazelCanaryResolveOnlyFailsClosedWithoutPRObjects(t *testing.T) {
	result, code := runBazelCanary(t, repoRoot(t), strings.Repeat("a", 40), strings.Repeat("b", 40), "resolve", nil)
	if code != 0 {
		t.Fatalf("fail-closed resolve exit = %d, output:\n%s", code, result.output)
	}
	if result.selection.Reason != "unavailable" || !result.selection.Conservative || len(result.selection.Labels) != 4 || result.selection.Error == nil {
		t.Fatalf("selection = %#v, want conservative four-target fallback", result.selection)
	}
	if !strings.Contains(result.outputs, "run_bazel=true\n") {
		t.Fatalf("fallback did not request execution:\n%s", result.outputs)
	}
}

func TestBazelCanaryRunConsumesPersistedSelection(t *testing.T) {
	root := repoRoot(t)
	diff := writeDiffFixture(t, "M\tinternal/config/storage_endpoint.go\x00")
	resolved, code := runBazelCanary(t, root, "", "", "resolve", []string{"CHANGED_PATHS_FILE=" + diff})
	if code != 0 {
		t.Fatalf("resolve exit = %d, output:\n%s", code, resolved.output)
	}

	// Remove Bazel from PATH and pass deliberately invalid event SHAs. The run
	// must still reach the Bazel availability check (127) using the persisted
	// mapped selection; recomputing would instead fail closed to four labels.
	result, code := runBazelCanary(t, root, "bad-base", "bad-head", "run", []string{
		"OUT=" + resolved.outDir,
		"BAZEL_BIN=/nonexistent/bazel",
	})
	if code != 127 {
		t.Fatalf("run exit = %d, want Bazel-unavailable 127; output:\n%s", code, result.output)
	}
	if len(result.selection.Labels) != 1 || result.selection.Labels[0] != "//internal/config:config_storage_endpoint_test" {
		t.Fatalf("run selection = %#v, want persisted mapped target", result.selection)
	}
}

func TestBazelCanaryParsesStringValuedBEPMetrics(t *testing.T) {
	root := repoRoot(t)
	tmp := t.TempDir()
	outDir := filepath.Join(tmp, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const label = "//internal/config:config_storage_endpoint_test"
	selection := `{"labels":["` + label + `"],"conservative":false,"reason":"mapped","error":null}` + "\n"
	if err := os.WriteFile(filepath.Join(outDir, "selection.normalized.json"), []byte(selection), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(bin, "go"), "#!/bin/sh\nexit 0\n")
	fakeBazel := filepath.Join(bin, "fake-bazel")
	writeExecutable(t, fakeBazel, `#!/usr/bin/env python3
import json
import pathlib
import sys

if "shutdown" in sys.argv:
    raise SystemExit(0)
path = next((arg.split("=", 1)[1] for arg in sys.argv if arg.startswith("--build_event_json_file=")), None)
if path:
    label = "//internal/config:config_storage_endpoint_test"
    events = [
        {"id": {"pattern": {"pattern": label}}, "pattern": {"pattern": label}},
        {"id": {"targetConfigured": {"label": label}}, "configured": {}},
        {"id": {"targetCompleted": {"label": label}}, "completed": {}},
        {"id": {"buildMetrics": {}}, "buildMetrics": {"actionSummary": {
            "actionsCreated": "77", "actionsExecuted": "41",
            "actionCacheStatistics": {"hits": "12", "misses": "29"}},
            "timingMetrics": {"analysisPhaseTimeInMs": "2044", "cpuTimeInMs": "171140"}}},
    ]
pathlib.Path(path).write_text("".join(json.dumps(event) + "\n" for event in events), encoding="utf-8")
raise SystemExit(0)
`)
	cmd := exec.Command(filepath.Join(root, "scripts", "bazel-canary.sh"), "run")
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"OUT="+outDir,
		"EVENT_NAME=pull_request",
		"PR_BASE_SHA=bad",
		"PR_HEAD_SHA=bad",
		"BAZEL_BIN="+fakeBazel,
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		debug, _ := os.ReadFile(filepath.Join(outDir, "summary.txt"))
		bazelLog, _ := os.ReadFile(filepath.Join(outDir, "bazel.log"))
		bepErr, _ := os.ReadFile(filepath.Join(outDir, "bep-resolver.stderr"))
		fakeBody, _ := os.ReadFile(fakeBazel)
		t.Logf("fake script:\n%s", fakeBody)
		t.Fatalf("fake canary run failed: %v\n%s\nsummary:\n%s\nbazel:\n%s\nbep err:\n%s", err, output, debug, bazelLog, bepErr)
	}
	summary, err := os.ReadFile(filepath.Join(outDir, "summary.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"configured_count=1",
		"completed_count=1",
		"actions_created=77",
		"actions_executed=41",
		"action_cache_hits=12",
		"action_cache_misses=29",
		"analysis_phase_ms=2044",
		"bep_cpu_ms=171140",
		"graph_metrics_scope=graph-wide (not target-specific)",
		"status=passed",
	} {
		if !strings.Contains(string(summary), want) {
			t.Errorf("summary missing %q:\n%s", want, summary)
		}
	}
}

func TestBazelCanaryWorkflowPolicy(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "workflows", "bazel-canary.yml"))
	if err != nil {
		t.Fatalf("read Bazel canary workflow: %v", err)
	}
	workflow := string(body)
	scriptBody, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "bazel-canary.sh"))
	if err != nil {
		t.Fatalf("read Bazel canary script: %v", err)
	}
	combined := workflow + "\n" + string(scriptBody)
	for _, required := range []string{
		"pull_request:",
		"continue-on-error: true",
		"timeout-minutes: 25",
		"fetch-depth: 0",
		"persist-credentials: false",
		"scripts/bazel-canary.sh resolve",
		"scripts/bazel-canary.sh",
		"actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02",
		"retention-days: 14",
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("workflow missing required policy marker %q", required)
		}
	}
	if strings.Contains(workflow, "pull_request:\n    paths:") {
		t.Fatal("workflow must not use a restrictive pull_request paths filter")
	}
	for _, label := range []string{
		"config_diagnostic_locations_test",
		"config_envname_test",
		"config_identity_seam_test",
		"config_storage_endpoint_test",
	} {
		if !strings.Contains(combined, label) {
			t.Errorf("workflow/script must mention bounded target %q", label)
		}
	}
}

func TestBazelConfigBacktestIncludesIdentityBaseline(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "bazel-config-backtest.sh"))
	if err != nil {
		t.Fatalf("read Bazel config backtest: %v", err)
	}
	script := string(body)
	for _, required := range []string{
		"BACKTEST_TARGETS:-//internal/config:config_diagnostic_locations_test,//internal/config:config_envname_test,//internal/config:config_identity_seam_test,//internal/config:config_storage_endpoint_test",
		"internal/config/identity_seam.go internal/config/identity_seam_bazel_test.go",
		"internal/config/diagnostic_locations_fixture_bazel_test.go internal/config/diagnostic_locations_test.go internal/config/testdata/diagnostic_locator.toml",
		`"${#target_labels[@]}" -eq 4`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("backtest script missing four-target identity marker %q", required)
		}
	}
}

type canaryResult struct {
	outDir    string
	output    string
	outputs   string
	selection canarySelection
}

func runBazelCanary(t *testing.T, root, base, head, mode string, overrides []string) (canaryResult, int) {
	t.Helper()
	tmp := t.TempDir()
	outDir := filepath.Join(tmp, "out")
	for _, override := range overrides {
		if strings.HasPrefix(override, "OUT=") {
			outDir = strings.TrimPrefix(override, "OUT=")
		}
	}
	outputFile := filepath.Join(tmp, "github-output")
	cmd := exec.Command(filepath.Join(root, "scripts", "bazel-canary.sh"), mode)
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"OUT="+outDir,
		"EVENT_NAME=pull_request",
		"PR_BASE_SHA="+base,
		"PR_HEAD_SHA="+head,
		"GITHUB_OUTPUT="+outputFile,
	)
	cmd.Env = append(cmd.Env, overrides...)
	output, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("run Bazel canary: %v", err)
		}
		code = exit.ExitCode()
	}
	outputValues, _ := os.ReadFile(outputFile)
	selectionBody, readErr := os.ReadFile(filepath.Join(outDir, "selection.normalized.json"))
	if readErr != nil {
		t.Fatalf("read normalized selection: %v\n%s", readErr, output)
	}
	var selection canarySelection
	if err := json.Unmarshal(selectionBody, &selection); err != nil {
		t.Fatalf("parse normalized selection: %v\n%s", err, selectionBody)
	}
	return canaryResult{outDir: outDir, output: string(output), outputs: string(outputValues), selection: selection}, code
}

func writeDiffFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "changed.name-status.z")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write changed-path fixture: %v", err)
	}
	return path
}
