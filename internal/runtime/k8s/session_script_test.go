package k8s

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	gcruntime "github.com/gastownhall/gascity/internal/runtime"
)

func TestSessionScriptProtocolDeclaresOwnedPathCapability(t *testing.T) {
	cmd := sessionScriptCommand(t, "protocol")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("gc-session-k8s protocol: %v", err)
	}
	var handshake struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(out, &handshake); err != nil {
		t.Fatalf("decode protocol response: %v\n%s", err, out)
	}
	for _, capability := range handshake.Capabilities {
		if capability == gcruntime.ProtocolCapabilityReconcilerOwnedMergeablePaths {
			return
		}
	}
	t.Fatalf("protocol capabilities = %v, missing %q", handshake.Capabilities, gcruntime.ProtocolCapabilityReconcilerOwnedMergeablePaths)
}

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

func TestSessionScriptStartFiltersOwnedHookAtRigOverlayDestination(t *testing.T) {
	root := t.TempDir()
	cityDir := filepath.Join(root, "city")
	rigDir := filepath.Join(cityDir, "frontend")
	overlayDir := filepath.Join(root, "overlay")
	for rel, contents := range map[string]string{
		filepath.Join(".codex", "hooks.json"):     "overlay codex hook",
		"AGENTS.codex.md":                         "ordinary sibling",
		filepath.Join(".gemini", "settings.json"): "unowned mergeable sibling",
	} {
		path := filepath.Join(overlayDir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("mkdir rig dir: %v", err)
	}
	canonicalPath := filepath.Join(rigDir, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(canonicalPath), 0o755); err != nil {
		t.Fatalf("mkdir canonical hook dir: %v", err)
	}
	if err := os.WriteFile(canonicalPath, []byte(`{"hooks":{"SessionStart":[]}}`), 0o644); err != nil {
		t.Fatalf("write canonical hook: %v", err)
	}

	result := runSessionScriptStart(t, sessionScriptStartOptions{
		PayloadEnv: map[string]string{"GC_CITY": cityDir, "GC_DIR": rigDir},
		WorkDir:    rigDir,
		OverlayDir: overlayDir,
		OwnedPaths: []string{".codex/hooks.json"},
		CopyFiles: []map[string]any{{
			"src":          canonicalPath,
			"rel_dst":      ".codex/hooks.json",
			"probed":       true,
			"content_hash": gcruntime.HashPathContent(canonicalPath),
		}},
	})
	if result.err != nil {
		t.Fatalf("gc-session-k8s start error = %v\noutput:\n%s", result.err, result.output)
	}
	if !strings.Contains(result.callLog, "cp ") || !strings.Contains(result.callLog, "mayor:/workspace/frontend/") {
		t.Fatalf("overlay cp did not target rig workdir; call log:\n%s", result.callLog)
	}
	if strings.Contains(result.cpSnapshot, ".codex/hooks.json") {
		t.Fatalf("owned Codex hook remained in filtered overlay snapshot:\n%s", result.cpSnapshot)
	}
	for _, want := range []string{"AGENTS.codex.md", ".gemini/settings.json"} {
		if !strings.Contains(result.cpSnapshot, want) {
			t.Fatalf("filtered overlay snapshot missing %s:\n%s", want, result.cpSnapshot)
		}
	}
}

func TestSessionScriptStartStagesOwnedCopyFileAtNamedWorkDir(t *testing.T) {
	root := t.TempDir()
	cityDir := filepath.Join(root, "city")
	workDir := filepath.Join(cityDir, "sessions", "operator")
	hookPath := filepath.Join(workDir, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		t.Fatalf("mkdir hook dir: %v", err)
	}
	if err := os.WriteFile(hookPath, []byte(`{"hooks":{}}`), 0o644); err != nil {
		t.Fatalf("write hook: %v", err)
	}

	result := runSessionScriptStart(t, sessionScriptStartOptions{
		PayloadEnv: map[string]string{"GC_CITY": cityDir, "GC_DIR": workDir},
		WorkDir:    workDir,
		OwnedPaths: []string{".codex/hooks.json"},
		CopyFiles: []map[string]any{{
			"src":          hookPath,
			"rel_dst":      ".codex/hooks.json",
			"probed":       true,
			"content_hash": gcruntime.HashPathContent(hookPath),
		}},
	})
	if result.err != nil {
		t.Fatalf("gc-session-k8s start error = %v\noutput:\n%s", result.err, result.output)
	}
	wantDestination := "mayor:/workspace/sessions/operator/.codex/hooks.json"
	if !strings.Contains(result.callLog, wantDestination) {
		t.Fatalf("owned CopyFile destination missing %q; call log:\n%s", wantDestination, result.callLog)
	}
	if strings.Contains(result.callLog, "mayor:/workspace/.codex/hooks.json") {
		t.Fatalf("owned CopyFile was incorrectly rooted at city workspace; call log:\n%s", result.callLog)
	}
	if strings.Contains(result.callLog, "sha256sum") || strings.Contains(result.callLog, "shasum") {
		t.Fatalf("pod-side verification unexpectedly requires a SHA utility; landed bytes should be streamed to the controller:\n%s", result.callLog)
	}
	if got := strings.Count(result.callLog, "gc-read-owned"); got != 2 {
		t.Fatalf("owned destination verification count = %d, want checks before and after city init; call log:\n%s", got, result.callLog)
	}
}

func TestSessionScriptStartFailsClosedWhenOwnedOverlayFilterFails(t *testing.T) {
	workDir := t.TempDir()
	canonicalPath := filepath.Join(workDir, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(canonicalPath), 0o755); err != nil {
		t.Fatalf("mkdir canonical hook dir: %v", err)
	}
	if err := os.WriteFile(canonicalPath, []byte(`{"hooks":{"SessionStart":[]}}`), 0o644); err != nil {
		t.Fatalf("write canonical hook: %v", err)
	}
	overlayDir := t.TempDir()
	hookPath := filepath.Join(overlayDir, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		t.Fatalf("mkdir hook dir: %v", err)
	}
	if err := os.WriteFile(hookPath, []byte(`{"hooks":{}}`), 0o644); err != nil {
		t.Fatalf("write hook: %v", err)
	}
	result := runSessionScriptStart(t, sessionScriptStartOptions{
		OverlayDir: overlayDir,
		WorkDir:    workDir,
		OwnedPaths: []string{".codex/hooks.json"},
		CopyFiles: []map[string]any{{
			"src":          canonicalPath,
			"rel_dst":      ".codex/hooks.json",
			"probed":       true,
			"content_hash": gcruntime.HashPathContent(canonicalPath),
		}},
		FailTar: true,
	})
	if result.err == nil {
		t.Fatalf("gc-session-k8s unexpectedly started after tar failure; output:\n%s", result.output)
	}
	if !strings.Contains(result.output, "failed to filter reconciler-owned overlay paths") {
		t.Fatalf("error output = %q, want owned-overlay filter failure", result.output)
	}
}

func TestSessionScriptStartRejectsInvalidOwnedCopyContract(t *testing.T) {
	workDir := t.TempDir()
	canonicalPath := filepath.Join(workDir, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(canonicalPath), 0o755); err != nil {
		t.Fatalf("mkdir canonical hook dir: %v", err)
	}
	if err := os.WriteFile(canonicalPath, []byte(`{"hooks":{}}`), 0o644); err != nil {
		t.Fatalf("write canonical hook: %v", err)
	}
	validHash := gcruntime.HashPathContent(canonicalPath)
	validEntry := func() map[string]any {
		return map[string]any{
			"src": canonicalPath, "rel_dst": ".codex/hooks.json",
			"probed": true, "content_hash": validHash,
		}
	}
	tests := []struct {
		name    string
		entries func() []map[string]any
		want    string
	}{
		{
			name: "duplicate destination",
			entries: func() []map[string]any {
				return []map[string]any{validEntry(), validEntry()}
			},
			want: "requires exactly one copy_file",
		},
		{
			name: "not probed",
			entries: func() []map[string]any {
				entry := validEntry()
				entry["probed"] = false
				return []map[string]any{entry}
			},
			want: "must be probed",
		},
		{
			name: "missing digest",
			entries: func() []map[string]any {
				entry := validEntry()
				delete(entry, "content_hash")
				return []map[string]any{entry}
			},
			want: "requires a SHA-256 content_hash",
		},
		{
			name: "stale digest",
			entries: func() []map[string]any {
				entry := validEntry()
				entry["content_hash"] = strings.Repeat("f", 64)
				return []map[string]any{entry}
			},
			want: "changed after reconciliation",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := runSessionScriptStart(t, sessionScriptStartOptions{
				WorkDir:    workDir,
				OwnedPaths: []string{".codex/hooks.json"},
				CopyFiles:  tc.entries(),
			})
			if result.err == nil {
				t.Fatalf("gc-session-k8s accepted invalid ownership contract; output:\n%s", result.output)
			}
			if !strings.Contains(result.output, tc.want) {
				t.Fatalf("error output = %q, want %q", result.output, tc.want)
			}
		})
	}
}

func TestSessionScriptStartRevalidatesOwnedSourceAfterInitReadiness(t *testing.T) {
	workDir := t.TempDir()
	canonicalPath := filepath.Join(workDir, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(canonicalPath), 0o755); err != nil {
		t.Fatalf("mkdir canonical hook dir: %v", err)
	}
	if err := os.WriteFile(canonicalPath, []byte(`{"hooks":{"SessionStart":[]}}`), 0o644); err != nil {
		t.Fatalf("write canonical hook: %v", err)
	}
	overlayDir := t.TempDir()
	overlayHook := filepath.Join(overlayDir, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(overlayHook), 0o755); err != nil {
		t.Fatalf("mkdir overlay hook dir: %v", err)
	}
	if err := os.WriteFile(overlayHook, []byte(`{"hooks":{}}`), 0o644); err != nil {
		t.Fatalf("write overlay hook: %v", err)
	}

	result := runSessionScriptStart(t, sessionScriptStartOptions{
		PayloadEnv: map[string]string{"GC_CITY": workDir, "GC_DIR": workDir},
		WorkDir:    workDir,
		OverlayDir: overlayDir,
		OwnedPaths: []string{".codex/hooks.json"},
		CopyFiles: []map[string]any{{
			"src":          canonicalPath,
			"rel_dst":      ".codex/hooks.json",
			"probed":       true,
			"content_hash": gcruntime.HashPathContent(canonicalPath),
		}},
		MutateOwnedSourceAfterReady: canonicalPath,
	})
	if result.err == nil {
		t.Fatalf("gc-session-k8s accepted an owned source mutated during readiness; output:\n%s", result.output)
	}
	if !strings.Contains(result.output, "changed after reconciliation") {
		t.Fatalf("error output = %q, want post-readiness digest failure", result.output)
	}
	if strings.Contains(result.callLog, " cp ") {
		t.Fatalf("staging continued after post-readiness revalidation failed:\n%s", result.callLog)
	}
}

func TestSessionScriptStartRejectsOwnedDestinationChangedDuringCopy(t *testing.T) {
	workDir := t.TempDir()
	canonicalPath := filepath.Join(workDir, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(canonicalPath), 0o755); err != nil {
		t.Fatalf("mkdir canonical hook dir: %v", err)
	}
	if err := os.WriteFile(canonicalPath, []byte(`{"hooks":{"SessionStart":[]}}`), 0o644); err != nil {
		t.Fatalf("write canonical hook: %v", err)
	}

	result := runSessionScriptStart(t, sessionScriptStartOptions{
		PayloadEnv: map[string]string{"GC_CITY": workDir, "GC_DIR": workDir},
		WorkDir:    workDir,
		OwnedPaths: []string{".codex/hooks.json"},
		CopyFiles: []map[string]any{{
			"src":          canonicalPath,
			"rel_dst":      ".codex/hooks.json",
			"probed":       true,
			"content_hash": gcruntime.HashPathContent(canonicalPath),
		}},
		MutateOwnedSourceOnCopy: canonicalPath,
	})
	if result.err == nil {
		t.Fatalf("gc-session-k8s accepted owned bytes changed during copy; output:\n%s", result.output)
	}
	if !strings.Contains(result.output, "pod destination") || !strings.Contains(result.output, "digest mismatch") {
		t.Fatalf("error output = %q, want post-copy destination digest failure", result.output)
	}
	if strings.Contains(result.callLog, "touch /workspace/.gc-ready") {
		t.Fatalf("init container was released after destination mismatch:\n%s", result.callLog)
	}
}

func TestSessionScriptStartRejectsOwnedDestinationChangedByCityInit(t *testing.T) {
	workDir := t.TempDir()
	canonicalPath := filepath.Join(workDir, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(canonicalPath), 0o755); err != nil {
		t.Fatalf("mkdir canonical hook dir: %v", err)
	}
	if err := os.WriteFile(canonicalPath, []byte(`{"hooks":{"SessionStart":[]}}`), 0o644); err != nil {
		t.Fatalf("write canonical hook: %v", err)
	}

	result := runSessionScriptStart(t, sessionScriptStartOptions{
		PayloadEnv: map[string]string{"GC_CITY": workDir, "GC_DIR": workDir},
		WorkDir:    workDir,
		OwnedPaths: []string{".codex/hooks.json"},
		CopyFiles: []map[string]any{{
			"src":          canonicalPath,
			"rel_dst":      ".codex/hooks.json",
			"probed":       true,
			"content_hash": gcruntime.HashPathContent(canonicalPath),
		}},
		MutateOwnedDestinationOnInit: true,
	})
	if result.err == nil {
		t.Fatalf("gc-session-k8s accepted an owned destination rewritten by city init; output:\n%s", result.output)
	}
	if !strings.Contains(result.output, "pod destination") || !strings.Contains(result.output, "digest mismatch") {
		t.Fatalf("error output = %q, want post-init destination digest failure", result.output)
	}
	if strings.Contains(result.callLog, "touch /workspace/.gc-workspace-ready") {
		t.Fatalf("workspace entrypoint was released after post-init destination mismatch:\n%s", result.callLog)
	}
}

type sessionScriptStartOptions struct {
	ProcessEnv                   map[string]string
	PayloadEnv                   map[string]string
	WorkDir                      string
	OverlayDir                   string
	OwnedPaths                   []string
	CopyFiles                    []map[string]any
	FailTar                      bool
	MutateOwnedSourceAfterReady  string
	MutateOwnedSourceOnCopy      string
	MutateOwnedDestinationOnInit bool
}

type sessionScriptStartResult struct {
	manifestEnv         map[string]string
	manifestMounts      map[string]string
	containerWorkingDir string
	containerArgs       string
	callLog             string
	cpSnapshot          string
	output              string
	err                 error
}

func runSessionScriptStart(t *testing.T, opts sessionScriptStartOptions) sessionScriptStartResult {
	t.Helper()

	tmpDir := t.TempDir()
	manifestPath := filepath.Join(tmpDir, "manifest.json")
	callLogPath := filepath.Join(tmpDir, "call.log")
	cpSnapshotPath := filepath.Join(tmpDir, "cp-snapshot.log")
	remoteContentPath := filepath.Join(tmpDir, "remote-content")
	binDir := filepath.Join(tmpDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin dir: %v", err)
	}

	fakeKubectl := filepath.Join(binDir, "kubectl")
	kubectlScript := fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
	manifest_out=%q
	call_log=%q
	cp_snapshot=%q
	mutate_owned_source=%q
	mutate_owned_source_on_copy=%q
	mutate_owned_destination_on_init=%t
	remote_content=%q
printf '%%s\n' "$*" >> "$call_log"
joined=" $* "
if [[ "$joined" == *" get pods -l "* ]]; then
  exit 0
fi
	if [[ "$joined" == *" get pod "*".status.initContainerStatuses[0].state.running"* ]]; then
	  if [ -n "$mutate_owned_source" ]; then
	    printf 'mutated-after-ready\n' > "$mutate_owned_source"
	  fi
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
if [[ "$joined" == *" exec "*" gc-read-owned "* ]]; then
  [ -s "$remote_content" ] || exit 1
  cat "$remote_content"
  exit 0
fi
if [[ "$joined" == *" exec "*" gc init --from "* ]]; then
  if [ "$mutate_owned_destination_on_init" = "true" ]; then
    printf 'mutated-by-city-init\n' > "$remote_content"
  fi
  exit 0
fi
if [[ "$joined" == *" exec "* ]]; then
  exit 0
fi
if [[ "$joined" == *" cp "* ]]; then
  args=("$@")
  src=""
  for ((i=0; i<${#args[@]}; i++)); do
    if [ "${args[$i]}" = "cp" ] && [ $((i + 1)) -lt ${#args[@]} ]; then
      src="${args[$((i + 1))]}"
      break
    fi
  done
  src_dir="${src%%/.}"
  if [ -f "$src" ]; then
    if [ -n "$mutate_owned_source_on_copy" ] && [ "$src" = "$mutate_owned_source_on_copy" ]; then
      printf 'mutated-during-copy\n' > "$src"
    fi
    cp "$src" "$remote_content"
  fi
  if [ -d "$src_dir" ] && { [ -e "$src_dir/AGENTS.codex.md" ] || [ -e "$src_dir/.gemini/settings.json" ]; }; then
    find "$src_dir" -type f | sed "s#^$src_dir/##" | sort > "$cp_snapshot"
  fi
  exit 0
fi
printf 'unexpected kubectl call: %%s\n' "$*" >&2
exit 1
	`, manifestPath, callLogPath, cpSnapshotPath, opts.MutateOwnedSourceAfterReady, opts.MutateOwnedSourceOnCopy, opts.MutateOwnedDestinationOnInit, remoteContentPath)
	if err := os.WriteFile(fakeKubectl, []byte(kubectlScript), 0o755); err != nil {
		t.Fatalf("write fake kubectl: %v", err)
	}
	if opts.FailTar {
		if err := os.WriteFile(filepath.Join(binDir, "tar"), []byte("#!/bin/sh\nexit 42\n"), 0o755); err != nil {
			t.Fatalf("write fake tar: %v", err)
		}
	}

	workDir := opts.WorkDir
	if workDir == "" {
		workDir = filepath.Join(tmpDir, "missing-workdir")
	}
	payload := map[string]any{
		"command":                          "echo hi",
		"env":                              opts.PayloadEnv,
		"work_dir":                         workDir,
		"overlay_dir":                      opts.OverlayDir,
		"reconciler_owned_mergeable_paths": opts.OwnedPaths,
		"copy_files":                       opts.CopyFiles,
	}
	configJSON, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}

	cmd := sessionScriptCommand(t, "start", "mayor")
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
	cpSnapshotBytes, readSnapshotErr := os.ReadFile(cpSnapshotPath)
	if readSnapshotErr != nil && !os.IsNotExist(readSnapshotErr) {
		t.Fatalf("read cp snapshot: %v", readSnapshotErr)
	}

	return sessionScriptStartResult{
		manifestEnv:         manifestEnv,
		manifestMounts:      manifestMounts,
		containerWorkingDir: containerWorkingDir,
		containerArgs:       containerArgs,
		callLog:             string(callLogBytes),
		cpSnapshot:          string(cpSnapshotBytes),
		output:              string(out),
		err:                 err,
	}
}

func sessionScriptCommand(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	return exec.Command(sessionScriptPath(t), args...)
}

func sessionScriptPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "contrib", "session-scripts", "gc-session-k8s"))
}
