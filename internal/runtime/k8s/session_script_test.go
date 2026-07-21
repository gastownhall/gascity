package k8s

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSessionScriptStartProjectsManagedPayloadPortToPodAlias(t *testing.T) {
	result := runSessionScriptStart(t, sessionScriptStartOptions{
		PayloadEnv: map[string]string{
			"GC_DOLT_PORT": "31364",
		},
	})
	if result.err != nil {
		t.Fatalf("gc-session-k8s start error = %v\noutput:\n%s", result.err, result.output)
	}
	if got := result.manifestEnv["GC_DOLT_HOST"]; got != podManagedDoltHost {
		t.Fatalf("manifest GC_DOLT_HOST = %q, want %q", got, podManagedDoltHost)
	}
	if got := result.manifestEnv["GC_DOLT_PORT"]; got != podManagedDoltPort {
		t.Fatalf("manifest GC_DOLT_PORT = %q, want %q", got, podManagedDoltPort)
	}
	if got := result.manifestEnv["BEADS_DOLT_SERVER_HOST"]; got != podManagedDoltHost {
		t.Fatalf("manifest BEADS_DOLT_SERVER_HOST = %q, want %q", got, podManagedDoltHost)
	}
	if got := result.manifestEnv["BEADS_DOLT_SERVER_PORT"]; got != podManagedDoltPort {
		t.Fatalf("manifest BEADS_DOLT_SERVER_PORT = %q, want %q", got, podManagedDoltPort)
	}
}

func TestSessionScriptStartPrefersPayloadOverLegacyCompatEnv(t *testing.T) {
	result := runSessionScriptStart(t, sessionScriptStartOptions{
		ProcessEnv: map[string]string{
			"GC_K8S_DOLT_HOST": "legacy-dolt.example.com",
			"GC_K8S_DOLT_PORT": "3308",
		},
		PayloadEnv: map[string]string{
			"GC_DOLT_HOST": "custom-dolt.example.com",
			"GC_DOLT_PORT": "4406",
		},
	})
	if result.err != nil {
		t.Fatalf("gc-session-k8s start error = %v\noutput:\n%s", result.err, result.output)
	}
	for _, key := range []string{"GC_DOLT_HOST", "BEADS_DOLT_SERVER_HOST"} {
		if got := result.manifestEnv[key]; got != "custom-dolt.example.com" {
			t.Fatalf("manifest %s = %q, want custom-dolt.example.com", key, got)
		}
	}
	for _, key := range []string{"GC_DOLT_PORT", "BEADS_DOLT_SERVER_PORT"} {
		if got := result.manifestEnv[key]; got != "4406" {
			t.Fatalf("manifest %s = %q, want 4406", key, got)
		}
	}
}

func TestSessionScriptStartOmitsDoltEnvWhenPayloadTargetMissingDespiteCompatEnv(t *testing.T) {
	result := runSessionScriptStart(t, sessionScriptStartOptions{
		ProcessEnv: map[string]string{
			"GC_K8S_DOLT_HOST": "legacy-dolt.example.com",
			"GC_K8S_DOLT_PORT": "3308",
		},
	})
	if result.err != nil {
		t.Fatalf("gc-session-k8s start error = %v\noutput:\n%s", result.err, result.output)
	}
	for _, key := range []string{"GC_DOLT_HOST", "GC_DOLT_PORT", "BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT"} {
		if _, ok := result.manifestEnv[key]; ok {
			t.Fatalf("manifest unexpectedly projected %s from compat env: %#v", key, result.manifestEnv)
		}
	}
}

func TestSessionScriptStartOmitsDoltEnvWhenOnlyAmbientCanonicalEnvExists(t *testing.T) {
	result := runSessionScriptStart(t, sessionScriptStartOptions{
		ProcessEnv: map[string]string{
			"GC_DOLT_HOST": "ambient-dolt.example.com",
			"GC_DOLT_PORT": "9911",
		},
	})
	if result.err != nil {
		t.Fatalf("gc-session-k8s start error = %v\noutput:\n%s", result.err, result.output)
	}
	for _, key := range []string{"GC_DOLT_HOST", "GC_DOLT_PORT", "BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT"} {
		if _, ok := result.manifestEnv[key]; ok {
			t.Fatalf("manifest unexpectedly projected %s from ambient canonical env: %#v", key, result.manifestEnv)
		}
	}
}

func TestSessionScriptStartRigManifestUsesPodPaths(t *testing.T) {
	root := t.TempDir()
	cityDir := filepath.Join(root, "city")
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("mkdir rig dir: %v", err)
	}

	result := runSessionScriptStart(t, sessionScriptStartOptions{
		PayloadEnv: map[string]string{
			"GC_CITY": cityDir,
			"GC_DIR":  rigDir,
		},
		WorkDir: rigDir,
	})
	if result.err != nil {
		t.Fatalf("gc-session-k8s start error = %v\noutput:\n%s", result.err, result.output)
	}
	if got := result.manifestEnv["GC_CITY"]; got != "/workspace" {
		t.Fatalf("manifest GC_CITY = %q, want /workspace", got)
	}
	if got := result.manifestEnv["GC_DIR"]; got != "/workspace/frontend" {
		t.Fatalf("manifest GC_DIR = %q, want /workspace/frontend", got)
	}
	// The manifest's workingDir is the workspace root, which always exists: the
	// kubelet chdirs there before the entrypoint runs, so naming a directory
	// that nothing has created yet (a per-bead pool/workflow workDir) would fail
	// the container outright. The entrypoint enters the pod-mapped agent dir.
	if got := result.containerWorkingDir; got != podWorkspaceRoot {
		t.Fatalf("container workingDir = %q, want %q", got, podWorkspaceRoot)
	}
	if got := result.containerArgs; !strings.Contains(got, "mkdir -p '/workspace/frontend'") ||
		!strings.Contains(got, "cd '/workspace/frontend'") {
		t.Fatalf("entrypoint should create and enter the pod-mapped agent dir; got: %s", got)
	}
	if got := result.manifestMounts["ws"]; got != "/workspace" {
		t.Fatalf("ws mount = %q, want /workspace", got)
	}
	if got := result.manifestMounts["city"]; got != cityDir {
		t.Fatalf("city mount = %q, want %q", got, cityDir)
	}
	for name, mountPath := range result.manifestMounts {
		if mountPath == rigDir {
			t.Fatalf("mount %s unexpectedly uses host rig path %q", name, mountPath)
		}
	}
}

type sessionScriptStartOptions struct {
	ProcessEnv map[string]string
	PayloadEnv map[string]string
	WorkDir    string
}

type sessionScriptStartResult struct {
	manifestEnv         map[string]string
	manifestMounts      map[string]string
	containerWorkingDir string
	containerArgs       string
	callLog             string
	output              string
	err                 error
}

func runSessionScriptStart(t *testing.T, opts sessionScriptStartOptions) sessionScriptStartResult {
	t.Helper()

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
printf '%%s\n' "$*" >> "$call_log"
joined=" $* "
if [[ "$joined" == *" get pods -l "* ]]; then
  exit 0
fi
if [[ "$joined" == *" get pod "*".status.initContainerStatuses[0].state.running"* ]]; then
  printf 'true'
  exit 0
fi
if [[ "$joined" == *" get pod "*".status.phase"* ]]; then
  printf 'Running'
  exit 0
fi
if [[ "$joined" == *" delete pod "* ]]; then
  exit 0
fi
if [[ "$joined" == *" wait --for=delete pod/"* ]]; then
  exit 0
fi
if [[ "$joined" == *" apply -f - "* ]]; then
  payload=$(cat)
  printf '%%s' "$payload" > "$manifest_out"
  exit 0
fi
if [[ "$joined" == *" wait --for=condition=Ready pod/"* ]]; then
  exit 0
fi
if [[ "$joined" == *" exec "* ]]; then
  exit 0
fi
printf 'unexpected kubectl call: %%s\n' "$*" >&2
exit 1
`, manifestPath, callLogPath)
	if err := os.WriteFile(fakeKubectl, []byte(kubectlScript), 0o755); err != nil {
		t.Fatalf("write fake kubectl: %v", err)
	}

	workDir := opts.WorkDir
	if workDir == "" {
		workDir = filepath.Join(tmpDir, "missing-workdir")
	}
	payload := map[string]any{
		"command":  "echo hi",
		"env":      opts.PayloadEnv,
		"work_dir": workDir,
	}
	configJSON, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}

	cmd := exec.Command(sessionScriptPath(t), "start", "mayor")
	cmd.Stdin = bytes.NewReader(configJSON)
	cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"), "GC_K8S_IMAGE=gc-agent:latest")
	for key, value := range opts.ProcessEnv {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	out, err := cmd.CombinedOutput()

	manifestEnv := map[string]string{}
	manifestMounts := map[string]string{}
	containerWorkingDir := ""
	containerArgs := ""
	manifestBytes, readManifestErr := os.ReadFile(manifestPath)
	if readManifestErr == nil && len(manifestBytes) > 0 {
		var manifest struct {
			Spec struct {
				Containers []struct {
					WorkingDir string   `json:"workingDir"`
					Args       []string `json:"args"`
					Env        []struct {
						Name  string `json:"name"`
						Value string `json:"value"`
					} `json:"env"`
					VolumeMounts []struct {
						Name      string `json:"name"`
						MountPath string `json:"mountPath"`
					} `json:"volumeMounts"`
				} `json:"containers"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
			t.Fatalf("parse manifest json: %v\n%s", err, string(manifestBytes))
		}
		if len(manifest.Spec.Containers) > 0 {
			containerWorkingDir = manifest.Spec.Containers[0].WorkingDir
			containerArgs = strings.Join(manifest.Spec.Containers[0].Args, " ")
			for _, item := range manifest.Spec.Containers[0].Env {
				manifestEnv[item.Name] = item.Value
			}
			for _, mount := range manifest.Spec.Containers[0].VolumeMounts {
				manifestMounts[mount.Name] = mount.MountPath
			}
		}
	} else if readManifestErr != nil && !os.IsNotExist(readManifestErr) {
		t.Fatalf("read manifest: %v", readManifestErr)
	}

	callLogBytes, readCallErr := os.ReadFile(callLogPath)
	if readCallErr != nil && !os.IsNotExist(readCallErr) {
		t.Fatalf("read call log: %v", readCallErr)
	}

	return sessionScriptStartResult{
		manifestEnv:         manifestEnv,
		manifestMounts:      manifestMounts,
		containerWorkingDir: containerWorkingDir,
		containerArgs:       containerArgs,
		callLog:             string(callLogBytes),
		output:              string(out),
		err:                 err,
	}
}

func sessionScriptPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "contrib", "session-scripts", "gc-session-k8s"))
}
