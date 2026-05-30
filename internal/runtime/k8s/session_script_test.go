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
	if !result.shareProcessNS {
		t.Fatal("manifest shareProcessNamespace = false, want true")
	}
	if got := result.manifestEnv["GC_CITY"]; got != "/workspace" {
		t.Fatalf("manifest GC_CITY = %q, want /workspace", got)
	}
	if got := result.manifestEnv["GC_DIR"]; got != "/workspace/frontend" {
		t.Fatalf("manifest GC_DIR = %q, want /workspace/frontend", got)
	}
	if got := result.containerWorkingDir; got != "/workspace" {
		t.Fatalf("container workingDir = %q, want stable /workspace entrypoint dir", got)
	}
	if got := result.containerCommand; len(got) != 2 || got[0] != "/bin/sh" || got[1] != "/gc-launch/entrypoint.sh" {
		t.Fatalf("container command = %#v, want bounded launch script command", got)
	}
	if len(result.containerArgs) != 0 {
		t.Fatalf("container args = %#v, want none", result.containerArgs)
	}
	if got := result.manifestMounts["ws"]; got != "/workspace" {
		t.Fatalf("ws mount = %q, want /workspace", got)
	}
	if got := result.manifestMounts["launch"]; got != "/gc-launch" {
		t.Fatalf("launch mount = %q, want /gc-launch", got)
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

func TestSessionScriptStartDoesNotEmbedLargeLaunchMaterialInPodArgs(t *testing.T) {
	largeCommand := "codex " + strings.Repeat("--flag ", 2000)
	largePreStart := "printf %s " + strings.Repeat("x", 20000)
	result := runSessionScriptStart(t, sessionScriptStartOptions{
		Command:  largeCommand,
		PreStart: []string{largePreStart},
	})
	if result.err != nil {
		t.Fatalf("gc-session-k8s start error = %v\noutput:\n%s", result.err, result.output)
	}
	argv := strings.Join(append(append([]string{}, result.containerCommand...), result.containerArgs...), "\x00")
	if len(argv) > 4096 {
		t.Fatalf("container argv length = %d, want bounded under 4096: command=%#v args=%#v", len(argv), result.containerCommand, result.containerArgs)
	}
	for _, forbidden := range []string{largeCommand, largePreStart} {
		if strings.Contains(argv, forbidden) || strings.Contains(result.callLog, forbidden) {
			t.Fatalf("large launch material leaked into pod argv or kubectl command log")
		}
	}
	if !strings.Contains(result.callLog, " cp ") || !strings.Contains(result.callLog, " -c launch") {
		t.Fatalf("launch material was not copied through launch init container:\n%s", result.callLog)
	}
}

type sessionScriptStartOptions struct {
	ProcessEnv map[string]string
	PayloadEnv map[string]string
	WorkDir    string
	Command    string
	PreStart   []string
}

type sessionScriptStartResult struct {
	manifestEnv         map[string]string
	manifestMounts      map[string]string
	containerWorkingDir string
	containerCommand    []string
	containerArgs       []string
	shareProcessNS      bool
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
if [[ "$joined" == *" get pod "*".status.initContainerStatuses["*".state.running"* ]]; then
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
if [[ "$joined" == *" cp "* ]]; then
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
	command := opts.Command
	if command == "" {
		command = "echo hi"
	}
	payload := map[string]any{
		"command":  command,
		"env":      opts.PayloadEnv,
		"work_dir": workDir,
	}
	if opts.PreStart != nil {
		payload["pre_start"] = opts.PreStart
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
	containerCommand := []string{}
	containerArgs := []string{}
	shareProcessNS := false
	manifestBytes, readManifestErr := os.ReadFile(manifestPath)
	if readManifestErr == nil && len(manifestBytes) > 0 {
		var manifest struct {
			Spec struct {
				ShareProcessNamespace bool `json:"shareProcessNamespace"`
				Containers            []struct {
					WorkingDir string   `json:"workingDir"`
					Command    []string `json:"command"`
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
		shareProcessNS = manifest.Spec.ShareProcessNamespace
		if len(manifest.Spec.Containers) > 0 {
			containerWorkingDir = manifest.Spec.Containers[0].WorkingDir
			containerCommand = manifest.Spec.Containers[0].Command
			containerArgs = manifest.Spec.Containers[0].Args
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
		containerCommand:    containerCommand,
		containerArgs:       containerArgs,
		shareProcessNS:      shareProcessNS,
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
