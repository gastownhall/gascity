package k8s

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	gcruntime "github.com/gastownhall/gascity/internal/runtime"

	"github.com/gastownhall/gascity/internal/bazeltest"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
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

// TestSessionScriptStartProjectsWithheldManagedPortToPodAlias pins the exec
// adapter's half of the gastownhall/gascity#1656 withhold: a session payload
// whose managed-local port gc withheld carries no host or port, only
// GC_DOLT_MANAGED_LOCAL=1, and must reach the in-cluster managed service like
// the port-only payload does, without the controller-side marker.
func TestSessionScriptStartProjectsWithheldManagedPortToPodAlias(t *testing.T) {
	result := runSessionScriptStart(t, sessionScriptStartOptions{
		PayloadEnv: runtimetest.WithheldManagedDoltEnv(),
	})
	if result.err != nil {
		t.Fatalf("gc-session-k8s start error = %v\noutput:\n%s", result.err, result.output)
	}
	for key, want := range map[string]string{
		"GC_DOLT_HOST":           podManagedDoltHost,
		"GC_DOLT_PORT":           podManagedDoltPort,
		"BEADS_DOLT_SERVER_HOST": podManagedDoltHost,
		"BEADS_DOLT_SERVER_PORT": podManagedDoltPort,
	} {
		if got := result.manifestEnv[key]; got != want {
			t.Fatalf("manifest %s = %q, want %q", key, got, want)
		}
	}
	if got, ok := result.manifestEnv["GC_DOLT_MANAGED_LOCAL"]; ok {
		t.Fatalf("manifest GC_DOLT_MANAGED_LOCAL = %q; the pod's target is the in-cluster service, not a controller-local server", got)
	}
}

// TestSessionScriptStartOmitsDoltEnvForEmptyPayloadPortWithoutMarker guards the
// other side of the marker: a payload whose Dolt keys are present but empty and
// carry no GC_DOLT_MANAGED_LOCAL (a scope without Dolt, or a failed resolution)
// must still omit pod Dolt env.
func TestSessionScriptStartOmitsDoltEnvForEmptyPayloadPortWithoutMarker(t *testing.T) {
	payload := runtimetest.WithheldManagedDoltEnv()
	payload["GC_DOLT_MANAGED_LOCAL"] = ""
	result := runSessionScriptStart(t, sessionScriptStartOptions{PayloadEnv: payload})
	if result.err != nil {
		t.Fatalf("gc-session-k8s start error = %v\noutput:\n%s", result.err, result.output)
	}
	for _, key := range []string{"GC_DOLT_HOST", "GC_DOLT_PORT", "BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT"} {
		if got, ok := result.manifestEnv[key]; ok {
			t.Fatalf("manifest projected %s = %q for a payload without a Dolt target: %#v", key, got, result.manifestEnv)
		}
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
	// that nothing has created yet (a per-bead pool/workflow workDir) would leave
	// the agent in a root-owned directory it cannot write into. The entrypoint
	// creates and enters the pod-mapped agent dir itself.
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
	// FailStage makes the in-pod remote-stage write fail.
	FailStage bool
}

type sessionScriptStartResult struct {
	manifestEnv         map[string]string
	manifestMounts      map[string]string
	containerWorkingDir string
	containerArgs       string
	callLog             string
	// stageRecord is what the script wrote to the pod's remote-stage record.
	stageRecord string
	output      string
	err         error
}

func runSessionScriptStart(t *testing.T, opts sessionScriptStartOptions) sessionScriptStartResult {
	t.Helper()

	tmpDir := t.TempDir()
	manifestPath := filepath.Join(tmpDir, "manifest.json")
	callLogPath := filepath.Join(tmpDir, "call.log")
	stagePath := filepath.Join(tmpDir, "stage.toml")
	binDir := filepath.Join(tmpDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin dir: %v", err)
	}

	fakeKubectl := filepath.Join(binDir, "kubectl")
	kubectlScript := fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
manifest_out=%q
call_log=%q
stage_out=%q
fail_stage=%q
printf '%%s\n' "$*" >> "$call_log"
joined=" $* "
if [[ "$joined" == *" exec "*"remote-stage.toml"* ]]; then
  if [[ "$fail_stage" == "true" ]]; then
    cat >/dev/null
    exit 1
  fi
  cat > "$stage_out"
  exit 0
fi
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
`, manifestPath, callLogPath, stagePath, fmt.Sprint(opts.FailStage))
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

	env := append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"), "GC_K8S_IMAGE=gc-agent:latest")
	for key, value := range opts.ProcessEnv {
		env = append(env, key+"="+value)
	}
	out, err := runScriptCommand(env, bytes.NewReader(configJSON), sessionScriptPath(t), "start", "mayor")

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

	stageBytes, readStageErr := os.ReadFile(stagePath)
	if readStageErr != nil && !os.IsNotExist(readStageErr) {
		t.Fatalf("read stage record: %v", readStageErr)
	}
	return sessionScriptStartResult{
		stageRecord:         string(stageBytes),
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
	if root := bazeltest.OverrideRoot(); root != "" {
		return filepath.Join(root, "contrib", "session-scripts", "gc-session-k8s")
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "contrib", "session-scripts", "gc-session-k8s"))
}

// TestSessionScriptStartRecordsTheRemoteStage (S5-A, owner decision D4): the
// exec k8s script marks its pod as a remote stage exactly as the native
// provider does, staged (a rig work dir) and unstaged (the city root) alike:
// the pod spec carries the remote-stage environment from the first process,
// the record is written by a direct in-pod write before the ready sentinel,
// and it names the controller's city and path.
func TestSessionScriptStartRecordsTheRemoteStage(t *testing.T) {
	root := t.TempDir()
	cityDir := filepath.Join(root, "city")
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"bright-lights\"\n\n[[agent]]\nname = \"mayor\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, workDir string }{{"staged rig work dir", rigDir}, {"city root", cityDir}} {
		t.Run(tc.name, func(t *testing.T) {
			result := runSessionScriptStart(t, sessionScriptStartOptions{
				PayloadEnv: map[string]string{"GC_CITY": cityDir, "GC_DIR": tc.workDir},
				WorkDir:    tc.workDir,
			})
			if result.err != nil {
				t.Fatalf("start: %v\n%s", result.err, result.output)
			}
			if got := result.manifestEnv["GC_REMOTE_STAGE_CONTROLLER"]; got != cityDir {
				t.Errorf("pod env GC_REMOTE_STAGE_CONTROLLER = %q, want %q", got, cityDir)
			}
			if got := result.manifestEnv["GC_REMOTE_STAGE_CITY"]; got != "bright-lights" {
				t.Errorf("pod env GC_REMOTE_STAGE_CITY = %q, want bright-lights", got)
			}
			for _, want := range []string{`controller_city = "bright-lights"`, fmt.Sprintf("controller_path = %q", cityDir), "staged_at = "} {
				if !strings.Contains(result.stageRecord, want) {
					t.Errorf("stage record %q lacks %q", result.stageRecord, want)
				}
			}
			stageAt := strings.Index(result.callLog, "remote-stage.toml")
			readyAt := strings.Index(result.callLog, ".gc-workspace-ready")
			if stageAt < 0 || readyAt < 0 || stageAt > readyAt {
				t.Errorf("the record must be written before the ready sentinel; call log:\n%s", result.callLog)
			}
		})
	}
}

// TestSessionScriptStartFailsClosedWhenTheStageRecordFails: a pod that cannot
// be marked is removed and the start fails, never run unmarked.
func TestSessionScriptStartFailsClosedWhenTheStageRecordFails(t *testing.T) {
	cityDir := filepath.Join(t.TempDir(), "city")
	if err := os.MkdirAll(cityDir, 0o755); err != nil {
		t.Fatal(err)
	}
	result := runSessionScriptStart(t, sessionScriptStartOptions{
		PayloadEnv: map[string]string{"GC_CITY": cityDir},
		WorkDir:    cityDir,
		FailStage:  true,
	})
	if result.err == nil {
		t.Fatalf("start succeeded with an unwritable remote-stage record:\n%s", result.output)
	}
	if !strings.Contains(result.output, "remote stage") {
		t.Errorf("output %q does not say why", result.output)
	}
	if strings.Contains(result.callLog, ".gc-workspace-ready") {
		t.Error("the ready sentinel was touched on a pod that could not be marked")
	}
	if !strings.Contains(result.callLog, "delete pod") {
		t.Error("the unmarked pod was not removed")
	}
}

// runScriptCommand runs one shell-level command with exactly env, the one
// subprocess call site these script tests share.
func runScriptCommand(env []string, stdin io.Reader, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = env
	cmd.Stdin = stdin
	return cmd.CombinedOutput()
}

// runUnderEnvClearingSu runs the su part of a pod command (from "CMD=$(") in
// a shell whose environment carries the pod spec's variables, with fakes for
// su (which, like `su -`, clears the environment), tmux (which records the
// environment its server would start with) and sleep. It returns what tmux
// saw.
func runUnderEnvClearingSu(t *testing.T, command string, podEnv map[string]string) string {
	t.Helper()
	at := strings.Index(command, "CMD=$(")
	if at < 0 {
		t.Fatalf("no agent launch in %q", command)
	}
	dir := t.TempDir()
	envOut := filepath.Join(dir, "tmux.env")
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// su - <user> -c <cmd>: a login shell with a cleared environment. cd is
	// stubbed: the pod's /workspace does not exist on the test host.
	write("su", "#!/bin/sh\nexec env -i PATH=\"$PATH\" /bin/sh -c \"cd() { :; }; $4\"\n")
	write("tmux", "#!/bin/sh\nenv > "+envOut+"\n")
	write("sleep", "#!/bin/sh\nexit 0\n")
	env := []string{"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH")}
	for k, v := range podEnv {
		env = append(env, k+"="+v)
	}
	if out, err := runScriptCommand(env, nil, "/bin/sh", "-c", command[at:]); err != nil {
		t.Fatalf("running the launch: %v\n%s", err, out)
	}
	got, err := os.ReadFile(envOut)
	if err != nil {
		t.Fatalf("tmux never ran: %v", err)
	}
	return string(got)
}

// TestLinuxUsernameAgentCarriesTheRemoteStageEnv: with LINUX_USERNAME the
// agent's tmux starts through `su -`, a login shell that clears the
// environment. The remote-stage variables must still reach it (and its tmux
// server), in the entrypoint and in a respawn, prebaked or not, whatever
// characters the values hold.
func TestLinuxUsernameAgentCarriesTheRemoteStageEnv(t *testing.T) {
	const ctrlCity = `/srv/my city/$HOME"x'`
	p := newProviderWithOps(newFakeK8sOps())
	p.cityName = `bright "lights"`
	cfg := gcruntime.Config{
		Command: "claude",
		WorkDir: ctrlCity,
		Env:     map[string]string{"GC_AGENT": "mayor", "GC_CITY": ctrlCity, "LINUX_USERNAME": "alice"},
	}
	want := func(t *testing.T, env string) {
		t.Helper()
		for _, line := range []string{remoteStageControllerEnv + "=" + ctrlCity, remoteStageCityEnv + "=" + p.cityName} {
			if !strings.Contains(env, line+"\n") {
				t.Errorf("the agent's environment lacks %q:\n%s", line, env)
			}
		}
	}
	podEnv := map[string]string{remoteStageControllerEnv: ctrlCity, remoteStageCityEnv: p.cityName}
	for _, prebaked := range []bool{false, true} {
		p.prebaked = prebaked
		pod, err := buildPod("gc-test-agent", cfg, p)
		if err != nil {
			t.Fatal(err)
		}
		t.Run("entrypoint", func(t *testing.T) {
			want(t, runUnderEnvClearingSu(t, pod.Spec.Containers[0].Args[0], podEnv))
		})
	}
	t.Run("respawn", func(t *testing.T) {
		want(t, runUnderEnvClearingSu(t, buildRespawnCommand(cfg, p.cityName), podEnv))
	})
}
