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

	"github.com/gastownhall/gascity/internal/bazeltest"
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

// TestBeadsScriptEnsureReadyRunsAsBakedGcagentUID pins the beads-runner pod to
// the uid the agent image actually bakes. contrib/k8s/Dockerfile.base:138 runs
// `useradd -m -s /bin/bash gcagent` with no `-u` on `ubuntu:24.04`, which
// already ships a default `ubuntu` user at 1000 — so `gcagent` lands at 1001.
// A pod pinned to 1000 runs as that unrelated `ubuntu` user, and /workspace
// (owned by gcagent) becomes unwritable.
func TestBeadsScriptEnsureReadyRunsAsBakedGcagentUID(t *testing.T) {
	result := runBeadsScript(t, beadsScriptOptions{
		Op: "ensure-ready",
		Env: map[string]string{
			"GC_K8S_IMAGE": "gc-beads:latest",
		},
	})
	if result.err != nil {
		t.Fatalf("gc-beads-k8s ensure-ready error = %v\noutput:\n%s", result.err, result.output)
	}
	const wantUID = 1001
	got := result.podSecurityContext
	if got.RunAsUser != wantUID || got.RunAsGroup != wantUID || got.FSGroup != wantUID {
		t.Fatalf("pod securityContext = %+v, want runAsUser/runAsGroup/fsGroup all %d", got, wantUID)
	}
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

func TestBeadsScriptCloseForcesCrossActorClose(t *testing.T) {
	result := runBeadsScript(t, beadsScriptOptions{
		Op:       "close",
		Args:     []string{"ga-orphan"},
		PodPhase: "Running",
		Env: map[string]string{
			"GC_CITY_PATH":  "/city",
			"GC_STORE_ROOT": "/city/frontend",
		},
	})
	if result.err != nil {
		t.Fatalf("gc-beads-k8s close error = %v\noutput:\n%s", result.err, result.output)
	}
	// This wrapper's close is an exec Store.Close delegate: it runs under the
	// pod/controller actor against agent-owned beads, so it must force-close to
	// clear bd's cross-actor guard (gastownhall/beads#3734). Pin the full argv so
	// a regression dropping --force (which would leak at the BD_VERSION bump) or
	// reordering the id past --force (the constraint exec.go's Close comment
	// documents) is caught here rather than in a live k8s deploy.
	assertCallContains(t, result.callLog, "close --force --json ga-orphan")
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
	Stdin       string
	// ExecRules make the fake kubectl's `exec gc-beads-runner -- sh -c` branch
	// answer a matching bd call with a chosen status and output instead of the
	// generic `exit 0`. The first matching rule wins.
	ExecRules []kubectlExecRule
}

// kubectlExecRule is one canned answer for the fake kubectl's exec branch.
type kubectlExecRule struct {
	// Match is a substring of the call's joined argv. Match the init call by
	// " init --server" and the types call by "config set types.custom"; neither
	// text occurs in run_bd's `sh -c` body.
	Match string
	// Status is the exit status the fake returns.
	Status int
	// Stdout and Stderr are written verbatim before exiting. They travel
	// through files so multi-line text needs no shell quoting.
	Stdout string
	Stderr string
}

// shellSingleQuote quotes s as one literal bash word.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// execRulesScript renders opts.ExecRules as bash `if` blocks for the fake
// kubectl, writing each rule's stdout and stderr text into dir.
func execRulesScript(t *testing.T, dir string, rules []kubectlExecRule) string {
	t.Helper()
	var b strings.Builder
	for i, rule := range rules {
		outPath := filepath.Join(dir, fmt.Sprintf("rule%d.stdout", i))
		errPath := filepath.Join(dir, fmt.Sprintf("rule%d.stderr", i))
		if err := os.WriteFile(outPath, []byte(rule.Stdout), 0o644); err != nil {
			t.Fatalf("write rule stdout: %v", err)
		}
		if err := os.WriteFile(errPath, []byte(rule.Stderr), 0o644); err != nil {
			t.Fatalf("write rule stderr: %v", err)
		}
		fmt.Fprintf(&b, "  if [[ \"$*\" == *%s* ]]; then\n    cat %s\n    cat %s >&2\n    exit %d\n  fi\n",
			shellSingleQuote(rule.Match), shellSingleQuote(outPath), shellSingleQuote(errPath), rule.Status)
	}
	return b.String()
}

// podSecurityContext is the subset of a pod's `spec.securityContext` the
// script tests assert on.
type podSecurityContext struct {
	RunAsUser  int `json:"runAsUser"`
	RunAsGroup int `json:"runAsGroup"`
	FSGroup    int `json:"fsGroup"`
}

type beadsScriptResult struct {
	manifestEnv        map[string]string
	podSecurityContext podSecurityContext
	callLog            string
	output             string
	err                error
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
%s  if [[ "$*" == *" list --json --limit 0 --all"* ]]; then
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
`, manifestPath, callLogPath, opts.ListOutput, opts.ReadyOutput, opts.PodPhase, execRulesScript(t, tmpDir, opts.ExecRules))
	if err := os.WriteFile(fakeKubectl, []byte(kubectlScript), 0o755); err != nil {
		t.Fatalf("write fake kubectl: %v", err)
	}

	args := append([]string{opts.Op}, opts.Args...)
	cmd := exec.Command(beadsScriptPath(t), args...)
	cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for key, value := range opts.Env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	if opts.Stdin != "" {
		cmd.Stdin = strings.NewReader(opts.Stdin)
	}
	out, err := cmd.CombinedOutput()

	callLogBytes, readCallErr := os.ReadFile(callLogPath)
	if readCallErr != nil && !os.IsNotExist(readCallErr) {
		t.Fatalf("read call log: %v", readCallErr)
	}
	manifestEnv := map[string]string{}
	var podSecCtx podSecurityContext
	manifestBytes, readManifestErr := os.ReadFile(manifestPath)
	if readManifestErr == nil && len(manifestBytes) > 0 {
		var manifest struct {
			Spec struct {
				SecurityContext podSecurityContext `json:"securityContext"`
				Containers      []struct {
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
		podSecCtx = manifest.Spec.SecurityContext
		if len(manifest.Spec.Containers) > 0 {
			for _, item := range manifest.Spec.Containers[0].Env {
				manifestEnv[item.Name] = item.Value
			}
		}
	} else if readManifestErr != nil && !os.IsNotExist(readManifestErr) {
		t.Fatalf("read manifest: %v", readManifestErr)
	}

	return beadsScriptResult{
		manifestEnv:        manifestEnv,
		podSecurityContext: podSecCtx,
		callLog:            string(callLogBytes),
		output:             string(out),
		err:                err,
	}
}

func beadsScriptPath(t *testing.T) string {
	t.Helper()
	if root := bazeltest.OverrideRoot(); root != "" {
		return filepath.Join(root, "contrib", "beads-scripts", "gc-beads-k8s")
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "contrib", "beads-scripts", "gc-beads-k8s"))
}

// beadsScriptUpdateEnv is the projected scope env an update runs under.
var beadsScriptUpdateEnv = map[string]string{
	"GC_CITY_PATH": "/city", "GC_STORE_ROOT": "/city/rigs/testrig", "GC_BEADS_PREFIX": "tr",
}

// TestBeadsScriptUpdateForwardsEveryDocumentedField pins the generated `bd
// update` argv for every field the update request may carry (see
// docs/reference/exec-beads-provider.md). A dropped field makes the write
// silently succeed while the change is lost: dropping `type`, for instance,
// leaves a graph.v2 step at type=gate forever — ready-excluded, so never
// dispatched — even though activation reported success.
func TestBeadsScriptUpdateForwardsEveryDocumentedField(t *testing.T) {
	result := runBeadsScript(t, beadsScriptOptions{
		Op:   "update",
		Args: []string{"tr-abc"},
		Stdin: `{"title":"renamed","status":"in_progress","type":"task","priority":1,` +
			`"description":"note","assignee":"worker-1","parent_id":"tr-parent",` +
			`"labels":["added"],"remove_labels":["dropped"]}`,
		Env: beadsScriptUpdateEnv,
	})
	if result.err != nil {
		t.Fatalf("gc-beads-k8s update error = %v\noutput:\n%s", result.err, result.output)
	}
	for _, want := range []string{
		"--title renamed",
		"--status in_progress",
		"--type task",
		"--priority 1",
		"--description note",
		"--assignee worker-1",
		"--parent tr-parent",
		"--add-label added",
		"--remove-label dropped",
	} {
		assertCallContains(t, result.callLog, want)
	}
}

// TestBeadsScriptUpdateOmitsAbsentFields pins that fields absent from the wire
// are not spuriously passed to bd as empty flags, so updating one field cannot
// clobber the others.
func TestBeadsScriptUpdateOmitsAbsentFields(t *testing.T) {
	result := runBeadsScript(t, beadsScriptOptions{
		Op:    "update",
		Args:  []string{"tr-abc"},
		Stdin: `{"description":"just a note"}`,
		Env:   beadsScriptUpdateEnv,
	})
	if result.err != nil {
		t.Fatalf("gc-beads-k8s update error = %v\noutput:\n%s", result.err, result.output)
	}
	assertCallContains(t, result.callLog, "--description just a note")
	for _, absent := range []string{
		"--title", "--status", "--type", "--priority",
		"--assignee", "--parent", "--add-label", "--remove-label",
	} {
		assertCallNotContains(t, result.callLog, absent)
	}
}

// TestBeadsScriptListProjectsParentAndPriority pins the read half of the write
// path above. The update op writes the parent natively via `bd --parent` and
// forwards `--priority`, so a projection that reconstructs parent_id from
// `parent:` labels alone — or omits priority entirely — turns a successful
// re-parent into a silently lost write on the next read.
func TestBeadsScriptListProjectsParentAndPriority(t *testing.T) {
	tests := []struct {
		name       string
		listOutput string
		wantParent string
	}{
		{
			// Native .parent wins over a stale parent: label, which is what a
			// re-parent leaves behind.
			name:       "native parent wins over legacy label",
			listOutput: `[{"id":"tr-a","title":"t","labels":["parent:tr-old"],"parent":"tr-new","priority":1}]`,
			wantParent: "tr-new",
		},
		{
			// Beads written before --parent carry only the label.
			name:       "legacy label when no native parent",
			listOutput: `[{"id":"tr-a","title":"t","labels":["parent:tr-old"],"priority":1}]`,
			wantParent: "tr-old",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := runBeadsScript(t, beadsScriptOptions{
				Op: "list",
				Env: map[string]string{
					"GC_CITY_PATH": "/city", "GC_STORE_ROOT": "/city/rigs/testrig", "GC_BEADS_PREFIX": "tr",
				},
				ListOutput: tc.listOutput,
			})
			if result.err != nil {
				t.Fatalf("gc-beads-k8s list error = %v\noutput:\n%s", result.err, result.output)
			}
			var got []struct {
				ID       string `json:"id"`
				ParentID string `json:"parent_id"`
				Priority *int   `json:"priority"`
			}
			if err := json.Unmarshal([]byte(result.output), &got); err != nil {
				t.Fatalf("parse list output: %v\noutput:\n%s", err, result.output)
			}
			if len(got) != 1 {
				t.Fatalf("got %d beads, want 1\noutput:\n%s", len(got), result.output)
			}
			if got[0].ParentID != tc.wantParent {
				t.Errorf("parent_id = %q, want %q", got[0].ParentID, tc.wantParent)
			}
			if got[0].Priority == nil || *got[0].Priority != 1 {
				t.Errorf("priority = %v, want 1", got[0].Priority)
			}
		})
	}
}

// beadsScriptInitEnv is the projected scope env an init runs under.
var beadsScriptInitEnv = map[string]string{
	"GC_CITY_PATH":    "/city",
	"GC_STORE_ROOT":   "/city/frontend",
	"GC_BEADS_PREFIX": "fe",
	"GC_DOLT_HOST":    "canonical-dolt.example.com",
	"GC_DOLT_PORT":    "4406",
}

const (
	// beadsInitCallMatch and beadsTypesCallMatch pick the two bd calls the init
	// op makes out of the fake kubectl's joined argv.
	beadsInitCallMatch  = " init --server"
	beadsTypesCallMatch = "config set types.custom"

	// bdDirtyTablesText is beads' dirty-table refusal, verbatim. cmd/gc matches
	// its marker by substring, so the init op must pass it through untouched.
	bdDirtyTablesText = "bd init: pending schema migrations alter pre-existing dirty tables: " +
		"config, dependencies; run 'bd dolt commit' to commit the working set at the current schema, " +
		"then re-run the migration (gastownhall/beads#4566)"

	// bdPrefixMismatchText is bd's abort for an initialized workspace asked for
	// another prefix (architect experiment T4). Its words include "already
	// initialized", so a script that tolerated that text would swallow it.
	bdPrefixMismatchText = `Error: workspace already initialized as database "fe", but --prefix "zz" was requested.`

	// bdProjectIdentityMismatchText is what a recreated pod's fresh workspace
	// gets from every bd call once init has minted it a new project id (E2).
	bdProjectIdentityMismatchText = "Error: failed to open database: PROJECT IDENTITY MISMATCH — refusing to connect"

	// bdInitInstallerChatter is what a successful fresh init still prints
	// despite --quiet (architect experiments T9, E5, E7).
	bdInitInstallerChatter = "Installing git hooks...\nWrote CLAUDE.md\nWrote AGENTS.md\n"
)

func runBeadsInit(t *testing.T, args []string, rules ...kubectlExecRule) beadsScriptResult {
	t.Helper()
	return runBeadsScript(t, beadsScriptOptions{
		Op:        "init",
		Args:      args,
		Env:       beadsScriptInitEnv,
		ExecRules: rules,
	})
}

// requireInitExitOne fails unless the op exited with exactly 1: gc reads exit 2
// as "not needed", so a failure must never leave the script as anything else.
func requireInitExitOne(t *testing.T, result beadsScriptResult) {
	t.Helper()
	var exitErr *exec.ExitError
	if !errors.As(result.err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("init error = %v, want exit code 1\noutput:\n%s", result.err, result.output)
	}
}

// requireTextBeforeStatusLine fails unless text is in output verbatim and is
// followed by the status line the op ends with.
func requireTextBeforeStatusLine(t *testing.T, output, text, status string) {
	t.Helper()
	textAt := strings.Index(output, text)
	if textAt < 0 {
		t.Fatalf("output lacks bd's text verbatim %q:\n%s", text, output)
	}
	if statusAt := strings.Index(output[textAt:], status); statusAt < 0 {
		t.Fatalf("output lacks %q after bd's text:\n%s", status, output)
	}
}

// beadsInitCall returns the call-log line of the bd init call.
func beadsInitCall(t *testing.T, callLog string) string {
	t.Helper()
	var found []string
	for _, line := range strings.Split(callLog, "\n") {
		if strings.Contains(line, beadsInitCallMatch) {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one bd init call, got %d in call log:\n%s", len(found), callLog)
	}
	return found[0]
}

// TestBeadsScriptInitSuccessIsQuiet pins that a successful init prints
// nothing: --quiet alone still lets bd's installer chatter through.
func TestBeadsScriptInitSuccessIsQuiet(t *testing.T) {
	result := runBeadsInit(t, []string{"/city/frontend", "fe"},
		kubectlExecRule{Match: beadsInitCallMatch, Stdout: bdInitInstallerChatter, Stderr: "Initialized.\n"})
	if result.err != nil {
		t.Fatalf("init error = %v\noutput:\n%s", result.err, result.output)
	}
	if strings.TrimSpace(result.output) != "" {
		t.Fatalf("successful init printed output, want none:\n%s", result.output)
	}
}

// TestBeadsScriptInitPropagatesBdFailure pins that a failed bd init fails the
// op: bd's text arrives verbatim (cmd/gc's dirty-table remedy matches it), the
// status line names the original status, and config set does not run.
func TestBeadsScriptInitPropagatesBdFailure(t *testing.T) {
	result := runBeadsInit(t, []string{"/city/frontend", "fe"},
		kubectlExecRule{Match: beadsInitCallMatch, Status: 1, Stderr: bdDirtyTablesText + "\n"})
	requireInitExitOne(t, result)
	requireTextBeforeStatusLine(t, result.output, bdDirtyTablesText, "exit 1")
	assertCallNotContains(t, result.callLog, "config set")
}

// TestBeadsScriptInitFailureNeverExitsTwo pins the trap: gc reads a lifecycle
// op's exit 2 as "not needed" and reports success, so a bd or kubectl status of
// 2 must leave the script as 1 with the original status named in the message.
func TestBeadsScriptInitFailureNeverExitsTwo(t *testing.T) {
	result := runBeadsInit(t, []string{"/city/frontend", "fe"},
		kubectlExecRule{Match: beadsInitCallMatch, Status: 2, Stderr: "Error: unknown flag: --init-if-missing\n"})
	requireInitExitOne(t, result)
	requireTextBeforeStatusLine(t, result.output, "Error: unknown flag: --init-if-missing", "exit 2")
}

// TestBeadsScriptInitSurfacesTransportFailure pins that a kubectl transport
// failure (a pod that is not there to exec into) fails the op with its text.
func TestBeadsScriptInitSurfacesTransportFailure(t *testing.T) {
	const text = `error: unable to upgrade connection: container not found ("gc-beads-runner")`
	result := runBeadsInit(t, []string{"/city/frontend", "fe"},
		kubectlExecRule{Match: beadsInitCallMatch, Status: 1, Stderr: text + "\n"})
	requireInitExitOne(t, result)
	requireTextBeforeStatusLine(t, result.output, text, "exit 1")
}

// TestBeadsScriptInitDoesNotTolerateAlreadyInitializedText pins that the
// idempotent re-init is bd's own typed answer (--init-if-missing exits 0), not
// a match on its output: a real failure's message contains the same words.
func TestBeadsScriptInitDoesNotTolerateAlreadyInitializedText(t *testing.T) {
	result := runBeadsInit(t, []string{"/city/frontend", "zz"},
		kubectlExecRule{Match: beadsInitCallMatch, Status: 1, Stderr: bdPrefixMismatchText + "\n"})
	requireInitExitOne(t, result)
	requireTextBeforeStatusLine(t, result.output, bdPrefixMismatchText, "exit 1")
}

// TestBeadsScriptInitUsesInitIfMissing pins the argv of the tolerated case on
// both branches: the new flags go after the existing arguments, so
// `init --server` still leads, and a prefix that is not a plain identifier
// keeps the line to exactly the old command plus the two flags.
func TestBeadsScriptInitUsesInitIfMissing(t *testing.T) {
	const base = "init --server --server-host canonical-dolt.example.com --server-port 4406"
	tests := []struct {
		prefix string
		want   string
	}{
		{prefix: "my-rig", want: base + " -p my-rig --skip-hooks --init-if-missing --quiet"},
		{prefix: "fe", want: base + " -p fe --skip-hooks --init-if-missing --quiet --database fe"},
	}
	for _, tt := range tests {
		t.Run(tt.prefix, func(t *testing.T) {
			result := runBeadsInit(t, []string{"/city/frontend", tt.prefix})
			if result.err != nil {
				t.Fatalf("init error = %v\noutput:\n%s", result.err, result.output)
			}
			call := beadsInitCall(t, result.callLog)
			if !strings.HasSuffix(strings.TrimSpace(call), tt.want) {
				t.Fatalf("init call = %q, want it to end with %q", call, tt.want)
			}
		})
	}
}

// TestBeadsScriptInitPropagatesConfigSetFailure pins that a failed
// `config set types.custom` fails the op too, with the same shape.
func TestBeadsScriptInitPropagatesConfigSetFailure(t *testing.T) {
	result := runBeadsInit(t, []string{"/city/frontend", "fe"},
		kubectlExecRule{Match: beadsTypesCallMatch, Status: 1, Stderr: bdDirtyTablesText + "\n"})
	requireInitExitOne(t, result)
	requireTextBeforeStatusLine(t, result.output, bdDirtyTablesText, "exit 1")
	assertCallContains(t, result.callLog, beadsTypesCallMatch)
}

// TestBeadsScriptInitIgnoresConfigSetWarnings pins that the op decides by bd's
// status, never by stderr being non-empty: a deprecation warning with exit 0
// (architect experiment T7) is not a failure.
func TestBeadsScriptInitIgnoresConfigSetWarnings(t *testing.T) {
	result := runBeadsInit(t, []string{"/city/frontend", "fe"},
		kubectlExecRule{Match: beadsTypesCallMatch, Status: 0, Stderr: "warning: dolt_server_port is deprecated\n"})
	if result.err != nil {
		t.Fatalf("init error = %v, want success on a warning with exit 0\noutput:\n%s", result.err, result.output)
	}
	assertCallContains(t, result.callLog, beadsTypesCallMatch)
}

// TestBeadsScriptInitFailedConfigSetReportsIdentityMismatch is the recreated
// pod's failure once init stops swallowing it: bd's PROJECT IDENTITY MISMATCH
// reaches gc instead of vanishing.
func TestBeadsScriptInitFailedConfigSetReportsIdentityMismatch(t *testing.T) {
	result := runBeadsInit(t, []string{"/city/frontend", "my-rig"},
		kubectlExecRule{Match: beadsTypesCallMatch, Status: 1, Stderr: bdProjectIdentityMismatchText + "\n"})
	requireInitExitOne(t, result)
	requireTextBeforeStatusLine(t, result.output, bdProjectIdentityMismatchText, "exit 1")
}

// TestBeadsScriptInitPlainPrefixReattachesDatabase pins --database <prefix> for
// a plain-identifier prefix, so a recreated pod's fresh workspace adopts the
// existing database's project id. The name is the prefix, never arg 3: gc
// passes "hq" there for the city scope while every k8s city's database is
// named after its prefix.
func TestBeadsScriptInitPlainPrefixReattachesDatabase(t *testing.T) {
	for _, prefix := range []string{"fe", "My_Rig2"} {
		t.Run(prefix, func(t *testing.T) {
			result := runBeadsInit(t, []string{"/city/frontend", prefix, "hq"})
			if result.err != nil {
				t.Fatalf("init error = %v\noutput:\n%s", result.err, result.output)
			}
			call := beadsInitCall(t, result.callLog)
			if !strings.Contains(call, "init --server") {
				t.Fatalf("init call = %q, want init --server", call)
			}
			fields := strings.Fields(call)
			at := -1
			for i, field := range fields {
				if field == "--database" {
					if at >= 0 {
						t.Fatalf("init call = %q, want one --database", call)
					}
					at = i
				}
			}
			if at < 0 || at+1 >= len(fields) {
				t.Fatalf("init call = %q, want --database <prefix>", call)
			}
			if got := fields[at+1]; got != prefix {
				t.Fatalf("--database = %q, want the prefix %q (never arg 3)", got, prefix)
			}
		})
	}
}

// TestBeadsScriptInitOmitsDatabaseForNonIdentifierPrefix pins that any other
// prefix keeps the command exactly as it was: the script does not copy bd's
// naming rules for it.
func TestBeadsScriptInitOmitsDatabaseForNonIdentifierPrefix(t *testing.T) {
	for _, prefix := range []string{"my-rig", "my.rig", "1rig", "_rig", "rigé"} {
		t.Run(prefix, func(t *testing.T) {
			result := runBeadsInit(t, []string{"/city/frontend", prefix, "hq"})
			if result.err != nil {
				t.Fatalf("init error = %v\noutput:\n%s", result.err, result.output)
			}
			if call := beadsInitCall(t, result.callLog); strings.Contains(call, "--database") {
				t.Fatalf("init call = %q, want no --database for prefix %q", call, prefix)
			}
		})
	}
}
