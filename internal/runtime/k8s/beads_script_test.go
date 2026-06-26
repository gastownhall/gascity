package k8s

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBeadsScriptEnsureReadyDoesNotAutoInitSharedWorkspace(t *testing.T) {
	result := runBeadsScript(t, beadsScriptOptions{
		Op: "ensure-ready",
		Env: map[string]string{
			"GC_K8S_IMAGE": "gc-beads:latest",
			"GC_DOLT_HOST": "canonical-dolt.example.com",
			"GC_DOLT_PORT": "4406",
		},
	})
	if result.err != nil {
		t.Fatalf("gc-beads-k8s ensure-ready error = %v\noutput:\n%s", result.err, result.output)
	}
	if _, ok := result.manifestEnv["GC_DOLT_HOST"]; ok {
		t.Fatalf("manifest unexpectedly projected GC_DOLT_HOST: %#v", result.manifestEnv)
	}
	if _, ok := result.manifestEnv["GC_DOLT_PORT"]; ok {
		t.Fatalf("manifest unexpectedly projected GC_DOLT_PORT: %#v", result.manifestEnv)
	}
	assertCallNotContains(t, result.callLog, "bd init")
	assertCallNotContains(t, result.callLog, "config set issue_prefix")
}

func TestBeadsScriptInitUsesScopeRootAndCanonicalDoltTarget(t *testing.T) {
	result := runBeadsScript(t, beadsScriptOptions{
		Op:   "init",
		Args: []string{"/city/frontend", "fe"},
		Env: map[string]string{
			"GC_CITY_PATH":     "/city",
			"GC_STORE_ROOT":    "/city/frontend",
			"GC_BEADS_PREFIX":  "fe",
			"GC_DOLT_HOST":     "canonical-dolt.example.com",
			"GC_DOLT_PORT":     "4406",
			"GC_K8S_DOLT_HOST": "legacy-dolt.example.com",
			"GC_K8S_DOLT_PORT": "3308",
		},
	})
	if result.err != nil {
		t.Fatalf("gc-beads-k8s init error = %v\noutput:\n%s", result.err, result.output)
	}
	assertCallContains(t, result.callLog, "/workspace/frontend")
	assertCallContains(t, result.callLog, "--server-host canonical-dolt.example.com")
	assertCallContains(t, result.callLog, "--server-port 4406")
	assertCallNotContains(t, result.callLog, "legacy-dolt.example.com")
	assertCallNotContains(t, result.callLog, "3308")
}

// TestBeadsScriptInitSetsBEADSDIR verifies the contrib gc-beads-k8s script
// exports BEADS_DIR inside the pod before running bd init. Without it, bd
// init creates a .git/ as a side effect in the workspace. Regression for
// #399.
func TestBeadsScriptInitSetsBEADSDIR(t *testing.T) {
	result := runBeadsScript(t, beadsScriptOptions{
		Op:   "init",
		Args: []string{"/city/frontend", "fe"},
		Env: map[string]string{
			"GC_CITY_PATH":    "/city",
			"GC_STORE_ROOT":   "/city/frontend",
			"GC_BEADS_PREFIX": "fe",
			"GC_DOLT_HOST":    "canonical-dolt.example.com",
			"GC_DOLT_PORT":    "4406",
		},
	})
	if result.err != nil {
		t.Fatalf("gc-beads-k8s init error = %v\noutput:\n%s", result.err, result.output)
	}
	assertCallContains(t, result.callLog, `export BEADS_DIR="$workdir/.beads"`)
	assertCallContains(t, result.callLog, `git config --global beads.role`)
	assertCallContains(t, result.callLog, "init --server")
}

func TestBeadsScriptInitDoesNotPreseedIssuePrefixBeforeBdInit(t *testing.T) {
	result := runBeadsScript(t, beadsScriptOptions{
		Op:   "init",
		Args: []string{"/city/frontend", "fe"},
		Env: map[string]string{
			"GC_CITY_PATH":    "/city",
			"GC_STORE_ROOT":   "/city/frontend",
			"GC_BEADS_PREFIX": "fe",
			"GC_DOLT_HOST":    "canonical-dolt.example.com",
			"GC_DOLT_PORT":    "4406",
		},
	})
	if result.err != nil {
		t.Fatalf("gc-beads-k8s init error = %v\noutput:\n%s", result.err, result.output)
	}
	lines := strings.Split(strings.TrimSpace(result.callLog), "\n")
	if len(lines) == 0 {
		t.Fatal("call log was empty")
	}
	if !strings.Contains(lines[0], "init --server") {
		t.Fatalf("first init call = %q, want init --server", lines[0])
	}
	if strings.Contains(result.callLog, "config set issue_prefix") {
		t.Fatalf("init flow should not rewrite issue_prefix around bd init:\n%s", result.callLog)
	}
}

func TestBeadsScriptInitRejectsPartialCanonicalDoltTarget(t *testing.T) {
	clearDoltAndCityEnv(t)
	result := runBeadsScript(t, beadsScriptOptions{
		Op:   "init",
		Args: []string{"/city/frontend", "fe"},
		Env: map[string]string{
			"GC_CITY_PATH":  "/city",
			"GC_STORE_ROOT": "/city/frontend",
			"GC_DOLT_HOST":  "canonical-dolt.example.com",
		},
	})
	if result.err == nil {
		t.Fatalf("gc-beads-k8s init error = nil, want partial GC_DOLT_* rejection\noutput:\n%s", result.output)
	}
	if !strings.Contains(result.output, "init: requires both GC_DOLT_HOST and GC_DOLT_PORT when GC_DOLT_HOST is set") {
		t.Fatalf("partial GC_DOLT_* rejection output = %q", result.output)
	}
}

func TestBeadsScriptInitFallsBackToDirWhenStoreRootUnset(t *testing.T) {
	// This test deliberately omits GC_STORE_ROOT from opts.Env to exercise the
	// init fall-back to the dir argument. Neutralize any ambient GC_STORE_ROOT
	// (set whenever the suite runs inside a gc city) so it cannot leak in via
	// os.Environ() and defeat the fall-back the test is asserting.
	clearDoltAndCityEnv(t)
	result := runBeadsScript(t, beadsScriptOptions{
		Op:   "init",
		Args: []string{"/city/services/api", "ap"},
		Env: map[string]string{
			"GC_CITY_PATH":    "/city",
			"GC_BEADS_PREFIX": "ap",
			"GC_DOLT_HOST":    "canonical-dolt.example.com",
			"GC_DOLT_PORT":    "4406",
		},
	})
	if result.err != nil {
		t.Fatalf("gc-beads-k8s init error = %v\noutput:\n%s", result.err, result.output)
	}
	assertCallContains(t, result.callLog, "/workspace/services/api")
}

func TestBeadsScriptListUsesScopedWorkdir(t *testing.T) {
	result := runBeadsScript(t, beadsScriptOptions{
		Op: "list",
		Env: map[string]string{
			"GC_CITY_PATH":    "/city",
			"GC_STORE_ROOT":   "/city/frontend",
			"GC_BEADS_PREFIX": "fe",
		},
		ListOutput: "[]",
	})
	if result.err != nil {
		t.Fatalf("gc-beads-k8s list error = %v\noutput:\n%s", result.err, result.output)
	}
	assertCallContains(t, result.callLog, "/workspace/frontend")
	assertCallContains(t, result.callLog, "list --json --limit 0 --all")
	assertCallContains(t, result.callLog, `export BEADS_DIR="$workdir/.beads"`)
	assertCallContains(t, result.callLog, `git config --global beads.role`)
}

func TestBeadsScriptListDoesNotRewriteIssuePrefixPerCommand(t *testing.T) {
	result := runBeadsScript(t, beadsScriptOptions{
		Op: "list",
		Env: map[string]string{
			"GC_CITY_PATH":    "/city",
			"GC_STORE_ROOT":   "/city/frontend",
			"GC_BEADS_PREFIX": "fe",
		},
		ListOutput: "[]",
	})
	if result.err != nil {
		t.Fatalf("gc-beads-k8s list error = %v\noutput:\n%s", result.err, result.output)
	}
	assertCallNotContains(t, result.callLog, "config set issue_prefix")
}

func TestBeadsScriptReadyForwardsIncludeEphemeral(t *testing.T) {
	result := runBeadsScript(t, beadsScriptOptions{
		Op:   "ready",
		Args: []string{"--include-ephemeral"},
		Env: map[string]string{
			"GC_CITY_PATH":  "/city",
			"GC_STORE_ROOT": "/city/frontend",
		},
		ReadyOutput: `[{"id":"ga-regular","title":"regular","status":"open","issue_type":"task","created_at":"2026-06-01T00:00:00Z"},{"id":"ga-wisp","title":"wisp","status":"open","issue_type":"task","created_at":"2026-06-01T00:00:01Z","ephemeral":true}]`,
	})
	if result.err != nil {
		t.Fatalf("gc-beads-k8s ready --include-ephemeral error = %v\noutput:\n%s", result.err, result.output)
	}
	assertCallContains(t, result.callLog, "ready --include-ephemeral --json --limit 0")
	if !strings.Contains(result.output, `"id": "ga-wisp"`) || !strings.Contains(result.output, `"ephemeral": true`) {
		t.Fatalf("ready output = %s, want converted ephemeral row", result.output)
	}
}

func TestBeadsScriptReadyRejectsUnsupportedArgs(t *testing.T) {
	result := runBeadsScript(t, beadsScriptOptions{
		Op:   "ready",
		Args: []string{"--unknown"},
		Env: map[string]string{
			"GC_CITY_PATH":  "/city",
			"GC_STORE_ROOT": "/city/frontend",
		},
	})
	if result.err == nil {
		t.Fatalf("gc-beads-k8s ready --unknown error = nil, want failure\noutput:\n%s", result.output)
	}
	var exitErr *exec.ExitError
	if !errors.As(result.err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("ready --unknown error = %v, want exit code 1", result.err)
	}
	if !strings.Contains(result.output, "ready: unsupported argument: --unknown") {
		t.Fatalf("ready --unknown output = %q, want unsupported argument message", result.output)
	}
	if strings.TrimSpace(result.callLog) != "" {
		t.Fatalf("ready --unknown should fail before kubectl calls, got call log:\n%s", result.callLog)
	}
}

func TestBeadsScriptConfigSetKeepsBEADSDIRScoped(t *testing.T) {
	result := runBeadsScript(t, beadsScriptOptions{
		Op:   "config-set",
		Args: []string{"issue_prefix", "fe"},
		Env: map[string]string{
			"GC_CITY_PATH":  "/city",
			"GC_STORE_ROOT": "/city/frontend",
		},
	})
	if result.err != nil {
		t.Fatalf("gc-beads-k8s config-set error = %v\noutput:\n%s", result.err, result.output)
	}
	assertCallContains(t, result.callLog, "/workspace/frontend")
	assertCallContains(t, result.callLog, "config set issue_prefix fe")
	assertCallContains(t, result.callLog, `export BEADS_DIR="$workdir/.beads"`)
}

type beadsScriptOptions struct {
	Op          string
	Args        []string
	Env         map[string]string
	PodPhase    string
	ListOutput  string
	ReadyOutput string
}

type beadsScriptResult struct {
	manifestEnv map[string]string
	callLog     string
	output      string
	err         error
}

func runBeadsScript(t *testing.T, opts beadsScriptOptions) beadsScriptResult {
	t.Helper()
	if opts.ListOutput == "" {
		opts.ListOutput = "[]"
	}
	if opts.ReadyOutput == "" {
		opts.ReadyOutput = "[]"
	}

	tmpDir := t.TempDir()
	manifestPath := filepath.Join(tmpDir, "manifest.json")
	callLogPath := filepath.Join(tmpDir, "call.log")
	binDir := filepath.Join(tmpDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin dir: %v", err)
	}

	fakeKubectl := filepath.Join(binDir, "kubectl")
	kubectlScript := fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
manifest_out=%q
call_log=%q
list_output=%q
ready_output=%q
printf '%%s\n' "$*" >> "$call_log"
joined=" $* "
if [[ "$joined" == *" get pod gc-beads-runner -o jsonpath={.status.phase} "* ]]; then
  printf '%%s' %q
  exit 0
fi
if [[ "$joined" == *" delete pod gc-beads-runner "* ]]; then
  exit 0
fi
if [[ "$joined" == *" wait --for=delete pod/gc-beads-runner "* ]]; then
  exit 0
fi
if [[ "$joined" == *" apply -f - "* ]]; then
  payload=$(cat)
  printf '%%s' "$payload" > "$manifest_out"
  exit 0
fi
if [[ "$joined" == *" wait --for=condition=Ready pod/gc-beads-runner "* ]]; then
  exit 0
fi
if [[ "$joined" == *" exec gc-beads-runner -- sh -c "* ]]; then
  if [[ "$*" == *"bd list --json --limit 0 --all"* ]]; then
    printf '%%s' "$list_output"
    exit 0
  fi
  if [[ "$*" == *" ready --include-ephemeral --json --limit 0"* ]]; then
    printf '%%s' "$ready_output"
    exit 0
  fi
  if [[ "$*" == *" ready --json --limit 0"* ]]; then
    printf '%%s' "$ready_output"
    exit 0
  fi
  exit 0
fi
printf 'unexpected kubectl call: %%s\n' "$*" >&2
exit 1
`, manifestPath, callLogPath, opts.ListOutput, opts.ReadyOutput, opts.PodPhase)
	if err := os.WriteFile(fakeKubectl, []byte(kubectlScript), 0o755); err != nil {
		t.Fatalf("write fake kubectl: %v", err)
	}

	args := append([]string{opts.Op}, opts.Args...)
	cmd := exec.Command(beadsScriptPath(t), args...)
	cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for key, value := range opts.Env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	out, err := cmd.CombinedOutput()

	callLogBytes, readCallErr := os.ReadFile(callLogPath)
	if readCallErr != nil && !os.IsNotExist(readCallErr) {
		t.Fatalf("read call log: %v", readCallErr)
	}
	manifestEnv := map[string]string{}
	manifestBytes, readManifestErr := os.ReadFile(manifestPath)
	if readManifestErr == nil && len(manifestBytes) > 0 {
		var manifest struct {
			Spec struct {
				Containers []struct {
					Env []struct {
						Name  string `json:"name"`
						Value string `json:"value"`
					} `json:"env"`
				} `json:"containers"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
			t.Fatalf("parse manifest json: %v\n%s", err, string(manifestBytes))
		}
		if len(manifest.Spec.Containers) > 0 {
			for _, item := range manifest.Spec.Containers[0].Env {
				manifestEnv[item.Name] = item.Value
			}
		}
	} else if readManifestErr != nil && !os.IsNotExist(readManifestErr) {
		t.Fatalf("read manifest: %v", readManifestErr)
	}

	return beadsScriptResult{
		manifestEnv: manifestEnv,
		callLog:     string(callLogBytes),
		output:      string(out),
		err:         err,
	}
}

func beadsScriptPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "contrib", "beads-scripts", "gc-beads-k8s"))
}
