package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaterializeBeadsBdScript(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	path, err := MaterializeBeadsBdScript(dir)
	if err != nil {
		t.Fatalf("MaterializeBeadsBdScript() error: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("script is not executable: mode %v", info.Mode())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 100 {
		t.Errorf("script too small: %d bytes", len(data))
	}
	if string(data[:2]) != "#!" {
		t.Error("script doesn't start with shebang")
	}
}

// TestBeadsBdScript_CanonicalDoltEnvInheritance verifies that gc-beads-bd
// honors the canonical GC_DOLT_HOST/PORT projection that pods now receive.
func TestBeadsBdScript_CanonicalDoltEnvInheritance(t *testing.T) {
	dir := t.TempDir()
	scriptPath, err := MaterializeBeadsBdScript(dir)
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(scriptPath, "start")
	cmd.Env = []string{
		"GC_CITY_PATH=" + dir,
		"GC_DOLT_HOST=dolt.example.com",
		"GC_DOLT_PORT=3307",
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
	}
	out, err := cmd.CombinedOutput()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if exitCode != 2 {
		t.Errorf("gc-beads-bd start with GC_DOLT_HOST: exit %d, want 2 (remote detected)\noutput: %s", exitCode, out)
	}
	configPath := filepath.Join(dir, ".gc", "runtime", "packs", "dolt", "dolt-config.yaml")
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("remote start should not create managed config %s, stat err = %v", configPath, err)
	}
}

func TestBeadsBdScript_LoopbackExternalStillCountsAsRemote(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "localhost"} {
		t.Run(host, func(t *testing.T) {
			dir := t.TempDir()
			scriptPath, err := MaterializeBeadsBdScript(dir)
			if err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command(scriptPath, "start")
			cmd.Env = []string{
				"GC_CITY_PATH=" + dir,
				"GC_DOLT_HOST=" + host,
				"GC_DOLT_PORT=3307",
				"PATH=" + os.Getenv("PATH"),
				"HOME=" + t.TempDir(),
			}
			out, err := cmd.CombinedOutput()
			exitCode := 0
			if err != nil {
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) {
					exitCode = exitErr.ExitCode()
				} else {
					t.Fatalf("unexpected error: %v", err)
				}
			}
			if exitCode != 2 {
				t.Fatalf("gc-beads-bd start with loopback host %q: exit %d, want 2 (remote detected)\noutput: %s", host, exitCode, out)
			}
			configPath := filepath.Join(dir, ".gc", "runtime", "packs", "dolt", "dolt-config.yaml")
			if _, err := os.Stat(configPath); !os.IsNotExist(err) {
				t.Fatalf("loopback remote start should not create managed config %s, stat err = %v", configPath, err)
			}
		})
	}
}

func TestBeadsBdScript_StopFallbackDoesNotKillImposterPIDFileTarget(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	script, err := MaterializeBeadsBdScript(cityPath)
	if err != nil {
		t.Fatalf("MaterializeBeadsBdScript: %v", err)
	}
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(layout.PIDFile), 0o755); err != nil {
		t.Fatal(err)
	}

	proc := exec.Command("sleep", "30")
	if err := proc.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	defer func() {
		_ = proc.Process.Kill()
		_, _ = proc.Process.Wait()
	}()
	if err := os.WriteFile(layout.PIDFile, []byte(fmt.Sprintf("%d\n", proc.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(script, "stop")
	cmd.Env = []string{
		"GC_CITY_PATH=" + cityPath,
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
	}
	out, err := cmd.CombinedOutput()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("gc-beads-bd stop failed unexpectedly: %v\n%s", err, out)
		}
	}
	if exitCode != 2 {
		t.Fatalf("gc-beads-bd stop exit = %d, want 2; output=%s", exitCode, out)
	}
	if !managedStopPIDAlive(proc.Process.Pid) {
		t.Fatalf("imposter pid %d was killed", proc.Process.Pid)
	}
	if _, err := os.Stat(layout.PIDFile); !os.IsNotExist(err) {
		t.Fatalf("pid file still present, err = %v", err)
	}
}

func TestMaterializeBeadsBdScript_idempotent(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	path1, err := MaterializeBeadsBdScript(dir)
	if err != nil {
		t.Fatal(err)
	}
	path2, err := MaterializeBeadsBdScript(dir)
	if err != nil {
		t.Fatal(err)
	}
	if path1 != path2 {
		t.Errorf("paths differ: %s != %s", path1, path2)
	}
}

func TestBeadsBdScript_UsesGCBinStoreBridgeForCreate(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath, err := MaterializeBeadsBdScript(cityDir)
	if err != nil {
		t.Fatal(err)
	}

	invocationFile := filepath.Join(t.TempDir(), "gc-invocation.txt")
	stdinFile := filepath.Join(t.TempDir(), "gc-stdin.json")
	envFile := filepath.Join(t.TempDir(), "gc-env.txt")
	fakeGC := filepath.Join(t.TempDir(), "gc")
	fakeGCScript := fmt.Sprintf(`#!/bin/sh
set -eu
invocation_file=%q
stdin_file=%q
env_file=%q
printf '%%s
' "$*" > "$invocation_file"
printf 'GC_DOLT_PASSWORD=%%s
BEADS_DOLT_PASSWORD=%%s
' "${GC_DOLT_PASSWORD:-}" "${BEADS_DOLT_PASSWORD:-}" > "$env_file"
case "$1 ${2:-}" in
  "dolt-state allocate-port")
    printf '3317
'
    exit 0
    ;;
  "bd-store-bridge ${2:-}")
    cat > "$stdin_file"
    cat <<'JSON'
{"id":"EX-1","title":"captured","status":"open","type":"task","created_at":"2026-02-27T10:00:00Z"}
JSON
    ;;
  *)
    echo "unexpected command: $*" >&2
    exit 2
    ;;
esac
`, invocationFile, stdinFile, envFile)
	if err := os.WriteFile(fakeGC, []byte(fakeGCScript), 0o755); err != nil {
		t.Fatal(err)
	}

	payload := `{"title":"captured","type":"task"}`
	cmd := exec.Command(scriptPath, "create")
	cmd.Stdin = strings.NewReader(payload)
	cmd.Env = []string{
		"GC_CITY_PATH=" + cityDir,
		"GC_STORE_ROOT=" + rigDir,
		"GC_BIN=" + fakeGC,
		"GC_DOLT_HOST=db.example.internal",
		"GC_DOLT_PORT=3317",
		"GC_DOLT_PASSWORD=secret",
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-beads-bd create failed: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != `{"id":"EX-1","title":"captured","status":"open","type":"task","created_at":"2026-02-27T10:00:00Z"}` {
		t.Fatalf("create output = %q", got)
	}

	invocation, err := os.ReadFile(invocationFile)
	if err != nil {
		t.Fatalf("ReadFile(invocation): %v", err)
	}
	invocationText := strings.TrimSpace(string(invocation))
	if strings.Contains(invocationText, "--password") {
		t.Fatalf("GC_BIN invocation leaked password argv: %s", invocationText)
	}
	for _, want := range []string{
		"bd-store-bridge",
		"--dir " + rigDir,
		"--host db.example.internal",
		"--port 3317",
		"create",
	} {
		if !strings.Contains(invocationText, want) {
			t.Fatalf("GC_BIN invocation missing %q: %s", want, invocationText)
		}
	}

	envMap := readExecCaptureEnv(t, envFile)
	if got := envMap["GC_DOLT_PASSWORD"]; got != "secret" {
		t.Fatalf("GC_DOLT_PASSWORD = %q, want secret", got)
	}
	if got := envMap["BEADS_DOLT_PASSWORD"]; got != "secret" {
		t.Fatalf("BEADS_DOLT_PASSWORD = %q, want secret", got)
	}

	stdinData, err := os.ReadFile(stdinFile)
	if err != nil {
		t.Fatalf("ReadFile(stdin): %v", err)
	}
	if got := strings.TrimSpace(string(stdinData)); got != payload {
		t.Fatalf("GC_BIN stdin = %q, want %q", got, payload)
	}
}

func TestBeadsBdScript_UsesPathGcStoreBridgeWhenGCBinUnset(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath, err := MaterializeBeadsBdScript(cityDir)
	if err != nil {
		t.Fatal(err)
	}

	invocationFile := filepath.Join(t.TempDir(), "gc-invocation.txt")
	fakeGC := filepath.Join(t.TempDir(), "gc")
	fakeGCScript := fmt.Sprintf(`#!/bin/sh
set -eu
printf '%%s\n' "$*" > %q
case "$1 ${2:-}" in
  "dolt-state allocate-port")
    printf '3317\n'
    ;;
  "bd-store-bridge ${2:-}")
    cat <<'JSON'
[{"id":"BD-2","title":"captured","status":"open","type":"task","created_at":"2026-02-27T10:00:00Z"}]
JSON
    ;;
  *)
    echo "unexpected command: $*" >&2
    exit 2
    ;;
esac
`, invocationFile)
	if err := os.WriteFile(fakeGC, []byte(fakeGCScript), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(scriptPath, "list", "--status=open")
	cmd.Env = []string{
		"GC_CITY_PATH=" + cityDir,
		"GC_STORE_ROOT=" + rigDir,
		"GC_DOLT_HOST=db.example.internal",
		"GC_DOLT_PORT=3317",
		"PATH=" + filepath.Dir(fakeGC) + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-beads-bd list failed: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != `[{"id":"BD-2","title":"captured","status":"open","type":"task","created_at":"2026-02-27T10:00:00Z"}]` {
		t.Fatalf("list output = %q", got)
	}

	invocation, err := os.ReadFile(invocationFile)
	if err != nil {
		t.Fatalf("ReadFile(invocation): %v", err)
	}
	invocationText := strings.TrimSpace(string(invocation))
	for _, want := range []string{"bd-store-bridge", "--dir " + rigDir, "--host db.example.internal", "--port 3317", "list", "--status=open"} {
		if !strings.Contains(invocationText, want) {
			t.Fatalf("PATH gc invocation missing %q: %s", want, invocationText)
		}
	}
}

func TestBeadsBdScript_StoreBridgeFailsWithoutGCBinary(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath, err := MaterializeBeadsBdScript(cityDir)
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(scriptPath, "list")
	cmd.Env = []string{
		"GC_CITY_PATH=" + cityDir,
		"GC_STORE_ROOT=" + rigDir,
		"GC_DOLT_HOST=db.example.internal",
		"GC_DOLT_PORT=3317",
		"PATH=/usr/bin:/bin",
		"HOME=" + t.TempDir(),
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("gc-beads-bd list unexpectedly succeeded: %s", out)
	}
	if !strings.Contains(string(out), "gc binary not found for exec store operations") {
		t.Fatalf("stderr = %q", string(out))
	}
}

func TestBeadsBdScript_UsesGCBinProbeManagedForProbe(t *testing.T) {
	cityDir := t.TempDir()
	statePath := filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt", "dolt-provider-state.json")
	if err := writeDoltRuntimeStateFile(statePath, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      3311,
		DataDir:   filepath.Join(cityDir, ".beads", "dolt"),
		StartedAt: "2026-04-14T00:00:00Z",
	}); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile: %v", err)
	}
	scriptPath, err := MaterializeBeadsBdScript(cityDir)
	if err != nil {
		t.Fatal(err)
	}

	invocationFile := filepath.Join(t.TempDir(), "gc-state-invocation.txt")
	fakeGC := filepath.Join(t.TempDir(), "gc")
	fakeGCScript := fmt.Sprintf(`#!/bin/sh
set -eu
printf '%%s\n' "$*" >> %q
case "$1 $2" in
  "dolt-state runtime-layout")
    printf 'GC_PACK_STATE_DIR	%s\n'
    printf 'GC_DOLT_DATA_DIR	%s\n'
    printf 'GC_DOLT_LOG_FILE	%s\n'
    printf 'GC_DOLT_STATE_FILE	%s\n'
    printf 'GC_DOLT_PID_FILE	%s\n'
    printf 'GC_DOLT_LOCK_FILE	%s\n'
    printf 'GC_DOLT_CONFIG_FILE	%s\n'
    ;;
  "dolt-state allocate-port")
    printf '3311\n'
    ;;
  "dolt-state read-provider")
    case "$4" in
      port)
        printf '3311\n'
        ;;
      pid)
        printf '%d\n'
        ;;
      *)
        echo "unexpected read-provider field: $*" >&2
        exit 2
        ;;
    esac
    ;;
  "dolt-state probe-managed")
    printf 'running\tfalse\n'
    printf 'port_holder_pid\t0\n'
    printf 'port_holder_owned\tfalse\n'
    printf 'port_holder_deleted_inodes\tfalse\n'
    printf 'tcp_reachable\tfalse\n'
    ;;
  *)
    echo "unexpected command: $*" >&2
    exit 2
    ;;
esac
`, invocationFile,
		filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt"),
		filepath.Join(cityDir, ".beads", "dolt"),
		filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt", "dolt.log"),
		filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt", "dolt-provider-state.json"),
		filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt", "dolt.pid"),
		filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt", "dolt.lock"),
		filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt", "dolt-config.yaml"),
		os.Getpid(),
	)
	if err := os.WriteFile(fakeGC, []byte(fakeGCScript), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(scriptPath, "probe")
	cmd.Env = []string{
		"GC_CITY_PATH=" + cityDir,
		"GC_BIN=" + fakeGC,
		"PATH=/usr/bin:/bin",
		"HOME=" + t.TempDir(),
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
			t.Fatalf("probe exit = %v, want code 2 from synthetic no-holder state\n%s", err, out)
		}
	}

	invocation, err := os.ReadFile(invocationFile)
	if err != nil {
		t.Fatalf("ReadFile(invocation): %v", err)
	}
	invocationText := string(invocation)
	if !strings.Contains(invocationText, "dolt-state probe-managed") {
		t.Fatalf("missing dolt-state probe-managed invocation: %s", invocationText)
	}
}

func TestBeadsBdScript_DoesNotUsePathGcProbeManagedWhenGCBinUnset(t *testing.T) {
	cityDir := t.TempDir()
	statePath := filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt", "dolt-provider-state.json")
	if err := writeDoltRuntimeStateFile(statePath, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      3311,
		DataDir:   filepath.Join(cityDir, ".beads", "dolt"),
		StartedAt: "2026-04-14T00:00:00Z",
	}); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile: %v", err)
	}
	scriptPath, err := MaterializeBeadsBdScript(cityDir)
	if err != nil {
		t.Fatal(err)
	}

	invocationFile := filepath.Join(t.TempDir(), "gc-state-invocation.txt")
	fakeGC := filepath.Join(t.TempDir(), "gc")
	fakeGCScript := fmt.Sprintf(`#!/bin/sh
set -eu
printf '%%s\n' "$*" >> %q
exit 2
`, invocationFile)
	if err := os.WriteFile(fakeGC, []byte(fakeGCScript), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(scriptPath, "probe")
	cmd.Env = []string{
		"GC_CITY_PATH=" + cityDir,
		"PATH=" + filepath.Dir(fakeGC) + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
	}
	_, _ = cmd.CombinedOutput()
	if _, err := os.Stat(invocationFile); !os.IsNotExist(err) {
		t.Fatalf("PATH gc should not be used for hidden probe-managed helper, stat err = %v", err)
	}
}

func TestBeadsBdScript_UsesGCBinSQLHelpersForHealth(t *testing.T) {
	cityDir := t.TempDir()
	scriptPath, err := MaterializeBeadsBdScript(cityDir)
	if err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer listener.Close() //nolint:errcheck
	port := listener.Addr().(*net.TCPAddr).Port

	invocationFile := filepath.Join(t.TempDir(), "gc-health-invocation.txt")
	fakeGC := filepath.Join(t.TempDir(), "gc")
	fakeGCScript := fmt.Sprintf(`#!/bin/sh
set -eu
printf '%%s
' "$*" >> %q
case "$1 $2" in
  "dolt-state runtime-layout")
    printf 'GC_PACK_STATE_DIR	%s
'
    printf 'GC_DOLT_DATA_DIR	%s
'
    printf 'GC_DOLT_LOG_FILE	%s
'
    printf 'GC_DOLT_STATE_FILE	%s
'
    printf 'GC_DOLT_PID_FILE	%s
'
    printf 'GC_DOLT_LOCK_FILE	%s
'
    printf 'GC_DOLT_CONFIG_FILE	%s
'
    ;;
  "dolt-state health-check")
    printf 'query_ready	true
'
    printf 'read_only	false
'
    printf 'connection_count	42
'
    ;;
  *)
    echo "unexpected command: $*" >&2
    exit 2
    ;;
esac
`, invocationFile,
		filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt"),
		filepath.Join(cityDir, ".beads", "dolt"),
		filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt", "dolt.log"),
		filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt", "dolt-provider-state.json"),
		filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt", "dolt.pid"),
		filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt", "dolt.lock"),
		filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt", "dolt-config.yaml"))
	if err := os.WriteFile(fakeGC, []byte(fakeGCScript), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(scriptPath, "health")
	cmd.Env = []string{
		"GC_CITY_PATH=" + cityDir,
		"GC_BIN=" + fakeGC,
		"GC_DOLT_PORT=" + fmt.Sprintf("%d", port),
		"PATH=/usr/bin:/bin",
		"HOME=" + t.TempDir(),
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-beads-bd health failed: %v\n%s", err, out)
	}

	invocation, err := os.ReadFile(invocationFile)
	if err != nil {
		t.Fatalf("ReadFile(invocation): %v", err)
	}
	gotInvocations := strings.Split(strings.TrimSpace(string(invocation)), "\n")
	wantInvocations := []string{
		"dolt-state health-check --host 127.0.0.1 --port " + fmt.Sprintf("%d", port) + " --user root --check-read-only",
	}
	if len(gotInvocations) < len(wantInvocations) {
		t.Fatalf("health helper invocations = %v, want suffix %v", gotInvocations, wantInvocations)
	}
	gotInvocations = gotInvocations[len(gotInvocations)-len(wantInvocations):]
	for i, want := range wantInvocations {
		if gotInvocations[i] != want {
			t.Fatalf("health helper invocation suffix[%d] = %q, want %q", i, gotInvocations[i], want)
		}
	}
}

func TestBeadsBdScript_UsesGCBinSQLHelpersForHealthReadOnlyFailure(t *testing.T) {
	cityDir := t.TempDir()
	scriptPath, err := MaterializeBeadsBdScript(cityDir)
	if err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer listener.Close() //nolint:errcheck
	port := listener.Addr().(*net.TCPAddr).Port

	invocationFile := filepath.Join(t.TempDir(), "gc-health-invocation.txt")
	fakeGC := filepath.Join(t.TempDir(), "gc")
	fakeGCScript := fmt.Sprintf(`#!/bin/sh
set -eu
printf '%%s
' "$*" >> %q
case "$1 $2" in
  "dolt-state runtime-layout")
    printf 'GC_PACK_STATE_DIR	%s
'
    printf 'GC_DOLT_DATA_DIR	%s
'
    printf 'GC_DOLT_LOG_FILE	%s
'
    printf 'GC_DOLT_STATE_FILE	%s
'
    printf 'GC_DOLT_PID_FILE	%s
'
    printf 'GC_DOLT_LOCK_FILE	%s
'
    printf 'GC_DOLT_CONFIG_FILE	%s
'
    ;;
  "dolt-state health-check")
    printf 'query_ready	true
'
    printf 'read_only	true
'
    printf 'connection_count	17
'
    ;;
  *)
    echo "unexpected command: $*" >&2
    exit 2
    ;;
esac
`, invocationFile,
		filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt"),
		filepath.Join(cityDir, ".beads", "dolt"),
		filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt", "dolt.log"),
		filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt", "dolt-provider-state.json"),
		filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt", "dolt.pid"),
		filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt", "dolt.lock"),
		filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt", "dolt-config.yaml"))
	if err := os.WriteFile(fakeGC, []byte(fakeGCScript), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(scriptPath, "health")
	cmd.Env = []string{
		"GC_CITY_PATH=" + cityDir,
		"GC_BIN=" + fakeGC,
		"GC_DOLT_PORT=" + fmt.Sprintf("%d", port),
		"PATH=/usr/bin:/bin",
		"HOME=" + t.TempDir(),
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("gc-beads-bd health unexpectedly succeeded: %s", out)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("health exit = %v, want code 1\n%s", err, out)
	}
	if !strings.Contains(string(out), "dolt server is in read-only mode") {
		t.Fatalf("stderr = %q, want read-only failure message", string(out))
	}

	invocation, err := os.ReadFile(invocationFile)
	if err != nil {
		t.Fatalf("ReadFile(invocation): %v", err)
	}
	gotInvocations := strings.Split(strings.TrimSpace(string(invocation)), "\n")
	wantInvocations := []string{
		"dolt-state health-check --host 127.0.0.1 --port " + fmt.Sprintf("%d", port) + " --user root --check-read-only",
	}
	if len(gotInvocations) < len(wantInvocations) {
		t.Fatalf("health helper invocations = %v, want suffix %v", gotInvocations, wantInvocations)
	}
	gotInvocations = gotInvocations[len(gotInvocations)-len(wantInvocations):]
	for i, want := range wantInvocations {
		if gotInvocations[i] != want {
			t.Fatalf("health helper invocation suffix[%d] = %q, want %q", i, gotInvocations[i], want)
		}
	}
}
