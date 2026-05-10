package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/supervisor"
	"github.com/gastownhall/gascity/internal/workspacesvc"
)

type closerSpy struct {
	closed bool
}

func (c *closerSpy) Close() error {
	c.closed = true
	return nil
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type workspaceServiceSentinel struct {
	pgid int
}

func stubSupervisorRunningPreserveSignalReady(t *testing.T, ready bool) {
	t.Helper()
	old := supervisorRunningPreserveSignalReady
	supervisorRunningPreserveSignalReady = func() (int, bool, error) {
		return 4242, ready, nil
	}
	t.Cleanup(func() {
		supervisorRunningPreserveSignalReady = old
	})
}

func startWorkspaceServiceSentinel(t *testing.T, gcHome, cityPath, serviceName string) workspaceServiceSentinel {
	t.Helper()
	stateRoot := filepath.Join(cityPath, ".gc", "services", serviceName)
	socketPath := filepath.Join(t.TempDir(), serviceName+".sock")
	cmd := exec.Command("sh", "-c", "trap 'exit 0' TERM; while :; do sleep 1; done")
	cmd.Env = append(os.Environ(),
		"GC_HOME="+gcHome,
		"GC_CITY_PATH="+cityPath,
		"GC_SERVICE_NAME="+serviceName,
		"GC_SERVICE_STATE_ROOT="+stateRoot,
		"GC_SERVICE_SOCKET="+socketPath,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start workspace-service sentinel %q: %v", serviceName, err)
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("Getpgid(%d): %v", cmd.Process.Pid, err)
	}
	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()
	t.Cleanup(func() {
		if processGroupAlive(pgid) {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
		select {
		case <-waitCh:
		case <-time.After(time.Second):
			t.Logf("workspace-service sentinel pgid %d did not exit before cleanup timeout", pgid)
		}
	})
	if !processGroupAlive(pgid) {
		t.Fatalf("workspace-service sentinel pgid %d is not alive", pgid)
	}
	return workspaceServiceSentinel{pgid: pgid}
}

func writeSupervisorProcEnv(t *testing.T, procRoot string, pid int, env map[string]string) {
	t.Helper()
	dir := filepath.Join(procRoot, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", dir, err)
	}
	var data []byte
	for key, value := range env {
		data = append(data, (key + "=" + value)...)
		data = append(data, 0)
	}
	if err := os.WriteFile(filepath.Join(dir, "environ"), data, 0o644); err != nil {
		t.Fatalf("WriteFile(environ): %v", err)
	}
}

func setSupervisorProcTestHooks(t *testing.T, procRoot string, getpgid func(int) (int, error)) {
	t.Helper()
	oldRoot := supervisorProcRoot
	oldReadDir := supervisorProcReadDir
	oldReadFile := supervisorProcReadFile
	oldGetpgid := supervisorGetpgid
	oldGetpgrp := supervisorGetpgrp
	supervisorProcRoot = procRoot
	supervisorProcReadDir = os.ReadDir
	supervisorProcReadFile = os.ReadFile
	supervisorGetpgid = getpgid
	supervisorGetpgrp = func() int { return 4242 }
	t.Cleanup(func() {
		supervisorProcRoot = oldRoot
		supervisorProcReadDir = oldReadDir
		supervisorProcReadFile = oldReadFile
		supervisorGetpgid = oldGetpgid
		supervisorGetpgrp = oldGetpgrp
	})
}

func startTestSupervisorSocket(t *testing.T, sockPath string, handler func(string) string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(sockPath), err)
	}
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Listen(unix, %q): %v", sockPath, err)
	}
	t.Cleanup(func() {
		lis.Close()         //nolint:errcheck
		os.Remove(sockPath) //nolint:errcheck
	})

	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close() //nolint:errcheck
				buf := make([]byte, 64)
				n, err := conn.Read(buf)
				if err != nil || n == 0 {
					return
				}
				resp := handler(strings.TrimSpace(string(buf[:n])))
				if resp != "" {
					io.WriteString(conn, resp) //nolint:errcheck
				}
			}(conn)
		}
	}()
}

func shortTempDir(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", prefix)
	if err != nil {
		t.Fatalf("MkdirTemp(/tmp, %q): %v", prefix, err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) }) //nolint:errcheck
	return dir
}

func installFakeSystemctl(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	logFile := filepath.Join(t.TempDir(), "systemctl.log")
	script := filepath.Join(binDir, "systemctl")
	content := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$*\" >> \"$GC_TEST_SYSTEMCTL_LOG\"\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("WriteFile(%q): %v", script, err)
	}
	t.Setenv("GC_TEST_SYSTEMCTL_LOG", logFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logFile
}

func readCommandLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	return string(data)
}

func TestDoSupervisorLogsNoFile(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := doSupervisorLogs(50, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doSupervisorLogs code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "log file not found") {
		t.Fatalf("stderr = %q, want missing log file message", stderr.String())
	}
}

func TestSupervisorAliveFallsBackToDefaultHomeSocket(t *testing.T) {
	gcHome := shortTempDir(t, "gc-home-")
	runtimeDir := shortTempDir(t, "gc-run-")
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	sockPath := filepath.Join(gcHome, "supervisor.sock")
	startTestSupervisorSocket(t, sockPath, func(cmd string) string {
		if cmd == "ping" {
			return "4242\n"
		}
		return ""
	})

	gotPath, gotPID := runningSupervisorSocket()
	if !samePath(gotPath, sockPath) {
		t.Fatalf("runningSupervisorSocket path = %q, want %q", gotPath, sockPath)
	}
	if gotPID != 4242 {
		t.Fatalf("runningSupervisorSocket pid = %d, want 4242", gotPID)
	}
	if pid := supervisorAlive(); pid != 4242 {
		t.Fatalf("supervisorAlive() = %d, want 4242", pid)
	}
}

func TestSupervisorAliveIgnoresSharedXDGSocketForIsolatedGCHome(t *testing.T) {
	homeDir := shortTempDir(t, "home-")
	gcHome := shortTempDir(t, "gc-home-")
	runtimeDir := shortTempDir(t, "gc-run-")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	sockPath := filepath.Join(runtimeDir, "gc", "supervisor.sock")
	startTestSupervisorSocket(t, sockPath, func(cmd string) string {
		if cmd == "ping" {
			return "4242\n"
		}
		return ""
	})

	gotPath, gotPID := runningSupervisorSocket()
	if gotPath != "" || gotPID != 0 {
		t.Fatalf("runningSupervisorSocket() = (%q, %d), want no shared XDG supervisor for isolated GC_HOME", gotPath, gotPID)
	}
	if pid := supervisorAlive(); pid != 0 {
		t.Fatalf("supervisorAlive() = %d, want 0 when only shared XDG socket exists", pid)
	}
}

func TestReloadSupervisorFallsBackToDefaultHomeSocket(t *testing.T) {
	gcHome := shortTempDir(t, "gc-home-")
	runtimeDir := shortTempDir(t, "gc-run-")
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	sockPath := filepath.Join(gcHome, "supervisor.sock")
	startTestSupervisorSocket(t, sockPath, func(cmd string) string {
		switch cmd {
		case "ping":
			return "4242\n"
		case "reload":
			return "ok\n"
		default:
			return ""
		}
	})

	var stdout, stderr bytes.Buffer
	if code := reloadSupervisor(&stdout, &stderr); code != 0 {
		t.Fatalf("reloadSupervisor code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Reconciliation triggered.") {
		t.Fatalf("stdout = %q, want reload confirmation", stdout.String())
	}
}

func TestRenderSupervisorLaunchdTemplate(t *testing.T) {
	data := &supervisorServiceData{
		GCPath:        "/usr/local/bin/gc",
		LogPath:       "/home/user/.gc/supervisor.log",
		GCHome:        "/home/user/.gc",
		XDGRuntimeDir: "/tmp/gc-run",
		LaunchdLabel:  defaultSupervisorLaunchdLabel,
		Path:          "/usr/local/bin:/usr/bin:/bin",
		ExtraEnv: []supervisorServiceEnvVar{
			{Name: "ANTHROPIC_API_KEY", Value: `sk-&<"'>`},
			{Name: "OPENAI_API_KEY", Value: "sk-openai-123"},
		},
	}

	content, err := renderSupervisorTemplate(supervisorLaunchdTemplate, data)
	if err != nil {
		t.Fatal(err)
	}

	for _, check := range []string{
		"com.gascity.supervisor",
		"/usr/local/bin/gc",
		"supervisor",
		"run",
		"/home/user/.gc/supervisor.log",
		"GC_HOME",
		"XDG_RUNTIME_DIR",
		"/tmp/gc-run",
		"<key>PATH</key>",
		"<key>ANTHROPIC_API_KEY</key>",
		"<string>sk-&amp;&lt;&quot;&apos;&gt;</string>",
		"<key>OPENAI_API_KEY</key>",
		"<string>sk-openai-123</string>",
		"<key>GC_SUPERVISOR_PRESERVE_SESSIONS_ON_SIGNAL</key>",
		"<string>1</string>",
	} {
		if !strings.Contains(content, check) {
			t.Fatalf("launchd template missing %q", check)
		}
	}
}

func TestRenderSupervisorLaunchdTemplateUsesPreserveEnvFromData(t *testing.T) {
	content, err := renderSupervisorTemplate(supervisorLaunchdTemplate, &supervisorServiceData{
		GCPath:       "/usr/local/bin/gc",
		LogPath:      "/home/user/.gc/supervisor.log",
		GCHome:       "/home/user/.gc",
		LaunchdLabel: defaultSupervisorLaunchdLabel,
		Path:         "/usr/local/bin:/usr/bin:/bin",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<key>GC_SUPERVISOR_PRESERVE_SESSIONS_ON_SIGNAL</key>",
		"<string>1</string>",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("launchd template missing preserve env %q:\n%s", want, content)
		}
	}
}

func TestRenderSupervisorSystemdTemplate(t *testing.T) {
	data := &supervisorServiceData{
		GCPath:        "/usr/local/bin/gc",
		LogPath:       "/home/user/.gc/supervisor.log",
		GCHome:        "/home/user/.gc",
		XDGRuntimeDir: "/tmp/gc-run",
		LaunchdLabel:  defaultSupervisorLaunchdLabel,
		Path:          "/usr/local/bin:/usr/bin:/bin",
		ExtraEnv: []supervisorServiceEnvVar{
			{Name: "ANTHROPIC_API_KEY", Value: `sk-"ant"\value`},
			{Name: "OPENAI_API_KEY", Value: "sk-openai-123"},
		},
	}

	content, err := renderSupervisorTemplate(supervisorSystemdTemplate, data)
	if err != nil {
		t.Fatal(err)
	}

	for _, check := range []string{
		"[Service]",
		`KillMode=process`,
		`Environment=GC_SUPERVISOR_PRESERVE_SESSIONS_ON_SIGNAL="1"`,
		`ExecStart="/usr/local/bin/gc" supervisor run`,
		`StandardOutput=append:/home/user/.gc/supervisor.log`,
		`Environment=GC_HOME="/home/user/.gc"`,
		`Environment=XDG_RUNTIME_DIR="/tmp/gc-run"`,
		`Environment=PATH="/usr/local/bin:/usr/bin:/bin"`,
		`Environment=ANTHROPIC_API_KEY="sk-\"ant\"\\value"`,
		`Environment=OPENAI_API_KEY="sk-openai-123"`,
	} {
		if !strings.Contains(content, check) {
			t.Fatalf("systemd template missing %q", check)
		}
	}
	wantBlock := "[Service]\nType=simple\n# Signal only the main supervisor PID on stop. The systemd default\n" +
		"# (control-group) would cascade SIGTERM to tmux servers spawned by\n" +
		"# 'gc supervisor run' that live in this cgroup, killing one-per-bead\n" +
		"# session conversation history. The reconciler re-adopts tmux on start.\n" +
		"KillMode=process\nExecStart=\"/usr/local/bin/gc\" supervisor run\n"
	if !strings.Contains(content, wantBlock) {
		t.Fatalf("systemd template missing ordered KillMode=process block under [Service]; got:\n%s", content)
	}
}

func TestRenderSupervisorSystemdTemplateUsesPreserveEnvFromData(t *testing.T) {
	content, err := renderSupervisorTemplate(supervisorSystemdTemplate, &supervisorServiceData{
		GCPath:  "/usr/local/bin/gc",
		LogPath: "/home/user/.gc/supervisor.log",
		GCHome:  "/home/user/.gc",
		Path:    "/usr/local/bin:/usr/bin:/bin",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `Environment=GC_SUPERVISOR_PRESERVE_SESSIONS_ON_SIGNAL="1"`
	if !strings.Contains(content, want) {
		t.Fatalf("systemd template missing preserve env %q:\n%s", want, content)
	}
}

func TestBuildSupervisorServiceDataTreatsPreserveSignalEnvAsFixed(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
	t.Setenv("GC_SUPERVISOR_ENV", supervisorPreserveSessionsOnSignalEnv)
	t.Setenv(supervisorPreserveSessionsOnSignalEnv, "0")

	data, err := buildSupervisorServiceData()
	if err != nil {
		t.Fatalf("buildSupervisorServiceData: %v", err)
	}
	if got := supervisorServiceEnvMap(data.ExtraEnv); got[supervisorPreserveSessionsOnSignalEnv] != "" {
		t.Fatalf("ExtraEnv[%s] = %q, want omitted fixed value (all env: %#v)", supervisorPreserveSessionsOnSignalEnv, got[supervisorPreserveSessionsOnSignalEnv], got)
	}

	launchdContent, err := renderSupervisorTemplate(supervisorLaunchdTemplate, data)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(launchdContent, supervisorPreserveSessionsOnSignalEnv); count != 1 {
		t.Fatalf("launchd preserve env occurrences = %d, want 1:\n%s", count, launchdContent)
	}

	systemdContent, err := renderSupervisorTemplate(supervisorSystemdTemplate, data)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(systemdContent, supervisorPreserveSessionsOnSignalEnv); count != 1 {
		t.Fatalf("systemd preserve env occurrences = %d, want 1:\n%s", count, systemdContent)
	}
}

func TestBuildSupervisorServiceDataIncludesProviderEnv(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
	t.Setenv("XDG_RUNTIME_DIR", "/tmp/gc-run")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-123")
	t.Setenv("ANTHROPIC_BASE_URL", "https://anthropic.example.test")
	t.Setenv("OPENAI_API_KEY", "sk-openai-123")
	t.Setenv("GEMINI_API_KEY", "gemini-123")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "gc-project")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(homeDir, ".claude"))
	t.Setenv("GC_SUPERVISOR_ENV", "CUSTOM_PROVIDER_TOKEN,IGNORED_EMPTY")
	t.Setenv("CUSTOM_PROVIDER_TOKEN", "custom-token")
	t.Setenv("IGNORED_EMPTY", "")
	t.Setenv("UNRELATED_SECRET", "do-not-persist")

	data, err := buildSupervisorServiceData()
	if err != nil {
		t.Fatalf("buildSupervisorServiceData: %v", err)
	}

	got := supervisorServiceEnvMap(data.ExtraEnv)
	for key, want := range map[string]string{
		"ANTHROPIC_API_KEY":     "sk-ant-123",
		"ANTHROPIC_BASE_URL":    "https://anthropic.example.test",
		"OPENAI_API_KEY":        "sk-openai-123",
		"GEMINI_API_KEY":        "gemini-123",
		"GOOGLE_CLOUD_PROJECT":  "gc-project",
		"CLAUDE_CONFIG_DIR":     filepath.Join(homeDir, ".claude"),
		"CUSTOM_PROVIDER_TOKEN": "custom-token",
	} {
		if got[key] != want {
			t.Fatalf("ExtraEnv[%s] = %q, want %q (all env: %#v)", key, got[key], want, got)
		}
	}
	for _, key := range []string{"GC_HOME", "PATH", "XDG_RUNTIME_DIR", "IGNORED_EMPTY", "UNRELATED_SECRET"} {
		if _, ok := got[key]; ok {
			t.Fatalf("ExtraEnv should not include %s: %#v", key, got)
		}
	}
}

func TestBuildSupervisorServiceDataOmitsProviderEnvWhenOptedOut(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
	t.Setenv("XDG_RUNTIME_DIR", "/tmp/gc-run")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-123")
	t.Setenv("ANTHROPIC_BASE_URL", "https://anthropic.example.test")
	t.Setenv("OPENAI_API_KEY", "sk-openai-123")
	t.Setenv("GEMINI_API_KEY", "gemini-123")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "gc-project")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(homeDir, ".claude"))
	t.Setenv("GC_SUPERVISOR_ENV", "CUSTOM_PROVIDER_TOKEN")
	t.Setenv("CUSTOM_PROVIDER_TOKEN", "custom-token")
	t.Setenv(supervisorOmitProviderCredsEnv, "1")

	data, err := buildSupervisorServiceData()
	if err != nil {
		t.Fatalf("buildSupervisorServiceData: %v", err)
	}

	got := supervisorServiceEnvMap(data.ExtraEnv)
	for _, key := range []string{
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_BASE_URL",
		"OPENAI_API_KEY",
		"GEMINI_API_KEY",
		"GOOGLE_CLOUD_PROJECT",
	} {
		if _, ok := got[key]; ok {
			t.Fatalf("ExtraEnv should not include provider key %s when %s=1: %#v",
				key, supervisorOmitProviderCredsEnv, got)
		}
	}
	for key, want := range map[string]string{
		"CLAUDE_CONFIG_DIR":     filepath.Join(homeDir, ".claude"),
		"CUSTOM_PROVIDER_TOKEN": "custom-token",
	} {
		if got[key] != want {
			t.Fatalf("ExtraEnv[%s] = %q, want %q (all env: %#v)", key, got[key], want, got)
		}
	}
}

func supervisorServiceEnvMap(vars []supervisorServiceEnvVar) map[string]string {
	m := make(map[string]string, len(vars))
	for _, item := range vars {
		m[item.Name] = item.Value
	}
	return m
}

func TestBuildSupervisorServiceDataReadsAllowlistedDoltCredentialKeysFromLaunchctl(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
	t.Setenv("XDG_RUNTIME_DIR", "/tmp/gc-run")
	for _, key := range []string{
		"GC_DOLT_USER",
		"GC_DOLT_PASSWORD",
		"GC_DOLT_LOGLEVEL",
	} {
		t.Setenv(key, "")
	}

	stub := map[string]string{
		"GC_DOLT_USER":     "gc_user",
		"GC_DOLT_PASSWORD": "redacted-test-value",
		"GC_DOLT_LOGLEVEL": "debug",
	}
	prev := supervisorLaunchctlGetenv
	supervisorLaunchctlGetenv = func(key string) string { return stub[key] }
	t.Cleanup(func() { supervisorLaunchctlGetenv = prev })

	data, err := buildSupervisorServiceData()
	if err != nil {
		t.Fatalf("buildSupervisorServiceData: %v", err)
	}
	got := supervisorServiceEnvMap(data.ExtraEnv)
	for key, want := range stub {
		if got[key] != want {
			t.Fatalf("ExtraEnv[%s] = %q, want %q (all env: %#v)", key, got[key], want, got)
		}
	}
}

func TestBuildSupervisorServiceDataSkipsDoltEndpointEnvUnlessExplicitlyOptedIn(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
	t.Setenv("XDG_RUNTIME_DIR", "/tmp/gc-run")
	t.Setenv("GC_DOLT_HOST", "127.0.0.1")
	t.Setenv("GC_DOLT_PORT", "3306")

	prev := supervisorLaunchctlGetenv
	supervisorLaunchctlGetenv = func(key string) string {
		switch key {
		case "GC_DOLT_HOST":
			return "launchctl.example"
		case "GC_DOLT_PORT":
			return "4406"
		default:
			return ""
		}
	}
	t.Cleanup(func() { supervisorLaunchctlGetenv = prev })

	data, err := buildSupervisorServiceData()
	if err != nil {
		t.Fatalf("buildSupervisorServiceData: %v", err)
	}
	got := supervisorServiceEnvMap(data.ExtraEnv)
	for _, key := range []string{"GC_DOLT_HOST", "GC_DOLT_PORT"} {
		if _, ok := got[key]; ok {
			t.Fatalf("ExtraEnv should not include default Dolt endpoint key %s (all env: %#v)", key, got)
		}
	}

	t.Setenv("GC_SUPERVISOR_ENV", "GC_DOLT_HOST GC_DOLT_PORT")
	data, err = buildSupervisorServiceData()
	if err != nil {
		t.Fatalf("buildSupervisorServiceData with explicit opt-in: %v", err)
	}
	got = supervisorServiceEnvMap(data.ExtraEnv)
	for key, want := range map[string]string{
		"GC_DOLT_HOST": "127.0.0.1",
		"GC_DOLT_PORT": "3306",
	} {
		if got[key] != want {
			t.Fatalf("ExtraEnv[%s] = %q, want %q after explicit opt-in (all env: %#v)", key, got[key], want, got)
		}
	}
}

func TestBuildSupervisorServiceDataPrefersOSEnvOverLaunchctl(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
	t.Setenv("GC_DOLT_LOGLEVEL", "trace")

	prev := supervisorLaunchctlGetenv
	supervisorLaunchctlGetenv = func(key string) string {
		if key == "GC_DOLT_LOGLEVEL" {
			return "debug"
		}
		return ""
	}
	t.Cleanup(func() { supervisorLaunchctlGetenv = prev })

	data, err := buildSupervisorServiceData()
	if err != nil {
		t.Fatalf("buildSupervisorServiceData: %v", err)
	}
	got := supervisorServiceEnvMap(data.ExtraEnv)
	if got["GC_DOLT_LOGLEVEL"] != "trace" {
		t.Fatalf("ExtraEnv[GC_DOLT_LOGLEVEL] = %q, want %q (os.Environ should win over launchctl)",
			got["GC_DOLT_LOGLEVEL"], "trace")
	}
}

func TestBuildSupervisorServiceDataReadsExplicitEnvOptInFromLaunchctl(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
	t.Setenv("GC_SUPERVISOR_ENV", "GC_DOLT_DATA_DIR")
	// GC_DOLT_DATA_DIR is not in os.Environ; only launchctl has it.
	t.Setenv("GC_DOLT_DATA_DIR", "")

	prev := supervisorLaunchctlGetenv
	supervisorLaunchctlGetenv = func(key string) string {
		if key == "GC_DOLT_DATA_DIR" {
			return "/srv/gc/dolt"
		}
		return ""
	}
	t.Cleanup(func() { supervisorLaunchctlGetenv = prev })

	data, err := buildSupervisorServiceData()
	if err != nil {
		t.Fatalf("buildSupervisorServiceData: %v", err)
	}
	got := supervisorServiceEnvMap(data.ExtraEnv)
	if got["GC_DOLT_DATA_DIR"] != "/srv/gc/dolt" {
		t.Fatalf("ExtraEnv[GC_DOLT_DATA_DIR] = %q, want %q (launchctl fallback for GC_SUPERVISOR_ENV opt-in)",
			got["GC_DOLT_DATA_DIR"], "/srv/gc/dolt")
	}
}

func TestBuildSupervisorServiceDataDeduplicatesLaunchctlFallbackProbes(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
	t.Setenv("GC_SUPERVISOR_ENV", "GC_DOLT_LOGLEVEL CUSTOM_ONLY")
	t.Setenv("GC_DOLT_LOGLEVEL", "")
	t.Setenv("CUSTOM_ONLY", "")

	calls := map[string]int{}
	prev := supervisorLaunchctlGetenv
	supervisorLaunchctlGetenv = func(key string) string {
		calls[key]++
		return ""
	}
	t.Cleanup(func() { supervisorLaunchctlGetenv = prev })

	if _, err := buildSupervisorServiceData(); err != nil {
		t.Fatalf("buildSupervisorServiceData: %v", err)
	}
	for _, key := range []string{"GC_DOLT_LOGLEVEL", "CUSTOM_ONLY"} {
		if calls[key] != 1 {
			t.Fatalf("launchctl getenv calls for %s = %d, want 1 (all calls: %#v)", key, calls[key], calls)
		}
	}
}

func TestSupervisorLaunchctlGetenvSkipsNonDarwin(t *testing.T) {
	oldGOOS := supervisorRuntimeGOOS
	supervisorRuntimeGOOS = "linux"
	t.Cleanup(func() { supervisorRuntimeGOOS = oldGOOS })

	binDir := t.TempDir()
	logFile := filepath.Join(t.TempDir(), "launchctl.log")
	script := filepath.Join(binDir, "launchctl")
	content := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$GC_TEST_LAUNCHCTL_LOG\"\nprintf 'should-not-run\\n'\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("WriteFile(%q): %v", script, err)
	}
	t.Setenv("GC_TEST_LAUNCHCTL_LOG", logFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if got := supervisorLaunchctlGetenv("GC_DOLT_LOGLEVEL"); got != "" {
		t.Fatalf("supervisorLaunchctlGetenv on linux = %q, want empty", got)
	}
	if log := readCommandLog(t, logFile); log != "" {
		t.Fatalf("launchctl was invoked on linux: %q", log)
	}
}

func TestSupervisorLaunchctlGetenvStripsDarwinOutputNewline(t *testing.T) {
	oldGOOS := supervisorRuntimeGOOS
	supervisorRuntimeGOOS = "darwin"
	t.Cleanup(func() { supervisorRuntimeGOOS = oldGOOS })

	binDir := t.TempDir()
	logFile := filepath.Join(t.TempDir(), "launchctl.log")
	script := filepath.Join(binDir, "launchctl")
	content := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$*\" >> \"$GC_TEST_LAUNCHCTL_LOG\"\nif [ \"$1\" = \"getenv\" ] && [ \"$2\" = \"GC_DOLT_LOGLEVEL\" ]; then\n  printf '  debug  \\n'\n  exit 0\nfi\nexit 1\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("WriteFile(%q): %v", script, err)
	}
	t.Setenv("GC_TEST_LAUNCHCTL_LOG", logFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if got := supervisorLaunchctlGetenv("GC_DOLT_LOGLEVEL"); got != "  debug  " {
		t.Fatalf("supervisorLaunchctlGetenv = %q, want %q", got, "  debug  ")
	}
	if log := readCommandLog(t, logFile); strings.TrimSpace(log) != "getenv GC_DOLT_LOGLEVEL" {
		t.Fatalf("launchctl log = %q, want getenv GC_DOLT_LOGLEVEL", log)
	}
}

func TestBuildSupervisorServiceDataExpandsUserManagedPath(t *testing.T) {
	homeDir := t.TempDir()
	nvmBin := filepath.Join(homeDir, ".nvm", "versions", "node", "v22.14.0", "bin")
	if err := os.MkdirAll(nvmBin, 0o755); err != nil {
		t.Fatalf("mkdir nvm bin: %v", err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
	t.Setenv("XDG_RUNTIME_DIR", "/tmp/gc-run")

	data, err := buildSupervisorServiceData()
	if err != nil {
		t.Fatalf("buildSupervisorServiceData: %v", err)
	}
	if !slices.Contains(filepath.SplitList(data.Path), nvmBin) {
		t.Fatalf("buildSupervisorServiceData PATH %q missing nvm bin %q", data.Path, nvmBin)
	}
	if data.XDGRuntimeDir != "/tmp/gc-run" {
		t.Fatalf("buildSupervisorServiceData XDGRuntimeDir = %q, want /tmp/gc-run", data.XDGRuntimeDir)
	}
}

func TestEmitSupervisorLoadCityConfigWarningsOncePerCity(t *testing.T) {
	var stderr bytes.Buffer
	prov := &config.Provenance{
		Warnings: []string{
			`/city/pack.toml: [agents] is a deprecated compatibility alias for [agent_defaults]; rewrite the table name to [agent_defaults]`,
			`/city/pack.toml: [agents] is a deprecated compatibility alias for [agent_defaults]; rewrite the table name to [agent_defaults]`,
		},
	}
	cityPath := filepath.Join(t.TempDir(), "city")
	otherCityPath := filepath.Join(t.TempDir(), "other-city")

	emitSupervisorLoadCityConfigWarnings(&stderr, cityPath, prov)
	emitSupervisorLoadCityConfigWarnings(&stderr, cityPath, prov)
	emitSupervisorLoadCityConfigWarnings(&stderr, otherCityPath, prov)

	const want = "[agents] is a deprecated compatibility alias for [agent_defaults]"
	if got := strings.Count(stderr.String(), want); got != 2 {
		t.Fatalf("warning count = %d, want 2 (once per city); stderr=%q", got, stderr.String())
	}
}

func TestBuildSupervisorServiceDataOmitsXDGRuntimeDirForIsolatedGCHome(t *testing.T) {
	homeDir := t.TempDir()
	gcHome := filepath.Join(t.TempDir(), "isolated-home")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", "/tmp/gc-run")

	data, err := buildSupervisorServiceData()
	if err != nil {
		t.Fatalf("buildSupervisorServiceData: %v", err)
	}
	if data.GCHome != gcHome {
		t.Fatalf("buildSupervisorServiceData GCHome = %q, want %q", data.GCHome, gcHome)
	}
	if data.XDGRuntimeDir != "" {
		t.Fatalf("buildSupervisorServiceData XDGRuntimeDir = %q, want empty for isolated GC_HOME", data.XDGRuntimeDir)
	}
}

func TestBuildSupervisorServiceDataCanonicalizesIsolatedGCHome(t *testing.T) {
	homeDir := t.TempDir()
	canonicalHome := filepath.Join(t.TempDir(), "isolated-home")
	if err := os.MkdirAll(canonicalHome, 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkHome := filepath.Join(t.TempDir(), "isolated-home-link")
	if err := os.Symlink(canonicalHome, symlinkHome); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", symlinkHome)

	data, err := buildSupervisorServiceData()
	if err != nil {
		t.Fatalf("buildSupervisorServiceData: %v", err)
	}
	if data.GCHome != normalizePathForCompare(symlinkHome) {
		t.Fatalf("buildSupervisorServiceData GCHome = %q, want canonical %q", data.GCHome, normalizePathForCompare(symlinkHome))
	}
}

func TestRenderSupervisorTemplateUsesCanonicalRelativeGCHome(t *testing.T) {
	homeDir := t.TempDir()
	canonicalHome := filepath.Join(homeDir, "isolated-home")
	if err := os.MkdirAll(canonicalHome, 0o755); err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(homeDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(wd); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	})
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", "isolated-home")

	data, err := buildSupervisorServiceData()
	if err != nil {
		t.Fatalf("buildSupervisorServiceData: %v", err)
	}

	systemdContent, err := renderSupervisorTemplate(supervisorSystemdTemplate, data)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(systemdContent, `Environment=GC_HOME="`+canonicalHome+`"`) {
		t.Fatalf("systemd template missing canonical GC_HOME %q:\n%s", canonicalHome, systemdContent)
	}

	launchdContent, err := renderSupervisorTemplate(supervisorLaunchdTemplate, data)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(launchdContent, "<key>GC_HOME</key>") || !strings.Contains(launchdContent, "<string>"+xmlEscape(canonicalHome)+"</string>") {
		t.Fatalf("launchd template missing canonical GC_HOME %q:\n%s", canonicalHome, launchdContent)
	}
}

func TestSupervisorLaunchdPlistPathUsesIsolatedLabelForIsolatedGCHome(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(t.TempDir(), "isolated-home"))

	label := supervisorLaunchdLabel()
	if label == defaultSupervisorLaunchdLabel {
		t.Fatalf("supervisorLaunchdLabel() = %q, want isolated label", label)
	}
	if !strings.HasPrefix(label, "com.gascity.supervisor.isolated-home-") {
		t.Fatalf("supervisorLaunchdLabel() = %q, want isolated-home-prefixed label", label)
	}
	wantPath := filepath.Join(homeDir, "Library", "LaunchAgents", label+".plist")
	if got := supervisorLaunchdPlistPath(); got != wantPath {
		t.Fatalf("supervisorLaunchdPlistPath() = %q, want %q", got, wantPath)
	}
}

func TestSupervisorServiceSuffixUsesFullGCHomePath(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	first := filepath.Join(t.TempDir(), "isolated-home")
	second := filepath.Join(t.TempDir(), "isolated-home")

	t.Setenv("GC_HOME", first)
	firstName := supervisorSystemdServiceName()
	firstLabel := supervisorLaunchdLabel()

	t.Setenv("GC_HOME", second)
	secondName := supervisorSystemdServiceName()
	secondLabel := supervisorLaunchdLabel()

	if firstName == defaultSupervisorSystemdUnit || secondName == defaultSupervisorSystemdUnit {
		t.Fatalf("isolated service name fell back to default: first=%q second=%q", firstName, secondName)
	}
	if firstName == secondName {
		t.Fatalf("supervisorSystemdServiceName() collided for distinct GC_HOME values: %q", firstName)
	}
	if firstLabel == secondLabel {
		t.Fatalf("supervisorLaunchdLabel() collided for distinct GC_HOME values: %q", firstLabel)
	}
}

func TestSupervisorServiceSuffixNormalizesEquivalentGCHomePaths(t *testing.T) {
	homeDir := t.TempDir()
	canonicalHome := filepath.Join(t.TempDir(), "isolated-home")
	if err := os.MkdirAll(canonicalHome, 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkHome := filepath.Join(t.TempDir(), "isolated-home-link")
	if err := os.Symlink(canonicalHome, symlinkHome); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", homeDir)

	t.Setenv("GC_HOME", canonicalHome)
	canonicalName := supervisorSystemdServiceName()
	canonicalLabel := supervisorLaunchdLabel()

	t.Setenv("GC_HOME", symlinkHome)
	symlinkName := supervisorSystemdServiceName()
	symlinkLabel := supervisorLaunchdLabel()

	if canonicalName != symlinkName {
		t.Fatalf("supervisorSystemdServiceName() mismatch for equivalent GC_HOME paths: canonical=%q symlink=%q", canonicalName, symlinkName)
	}
	if canonicalLabel != symlinkLabel {
		t.Fatalf("supervisorLaunchdLabel() mismatch for equivalent GC_HOME paths: canonical=%q symlink=%q", canonicalLabel, symlinkLabel)
	}
}

func TestSupervisorServiceSuffixNormalizesRelativeGCHomePaths(t *testing.T) {
	homeDir := t.TempDir()
	canonicalHome := filepath.Join(homeDir, "isolated-home")
	if err := os.MkdirAll(canonicalHome, 0o755); err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(homeDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(wd); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	})
	t.Setenv("HOME", homeDir)

	t.Setenv("GC_HOME", canonicalHome)
	canonicalName := supervisorSystemdServiceName()
	canonicalLabel := supervisorLaunchdLabel()

	t.Setenv("GC_HOME", "isolated-home")
	relativeName := supervisorSystemdServiceName()
	relativeLabel := supervisorLaunchdLabel()

	if canonicalName != relativeName {
		t.Fatalf("supervisorSystemdServiceName() mismatch for equivalent GC_HOME paths: canonical=%q relative=%q", canonicalName, relativeName)
	}
	if canonicalLabel != relativeLabel {
		t.Fatalf("supervisorLaunchdLabel() mismatch for equivalent GC_HOME paths: canonical=%q relative=%q", canonicalLabel, relativeLabel)
	}
}

func TestSupervisorServiceSuffixDoesNotFallBackWhenBasenameSanitizesEmpty(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(t.TempDir(), "---"))

	if got := supervisorSystemdServiceName(); got == defaultSupervisorSystemdUnit {
		t.Fatalf("supervisorSystemdServiceName() = %q, want isolated non-default name", got)
	}
	if got := supervisorLaunchdLabel(); got == defaultSupervisorLaunchdLabel {
		t.Fatalf("supervisorLaunchdLabel() = %q, want isolated non-default label", got)
	}
}

func TestLaunchdPrintReportsRunningAnchorsStateLine(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want bool
	}{
		{
			name: "top-level running state",
			out:  "gui/501/com.gascity.supervisor = {\n\tstate = running\n\tprogram = /usr/local/bin/gc\n}\n",
			want: true,
		},
		{
			name: "stopped state with nested running text",
			out:  "gui/501/com.gascity.supervisor = {\n\tstate = waiting\n\tlast exit code = 0\n\tpath = /tmp/state = running.log\n}\n",
			want: false,
		},
		{
			name: "running suffix is not a state token",
			out:  "gui/501/com.gascity.supervisor = {\n\tstate = running-old\n}\n",
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := launchdPrintReportsRunning([]byte(tt.out)); got != tt.want {
				t.Fatalf("launchdPrintReportsRunning() = %v, want %v for output:\n%s", got, tt.want, tt.out)
			}
		})
	}
}

func TestSupervisorInstallUnsupportedOS(t *testing.T) {
	if goruntime.GOOS == "darwin" || goruntime.GOOS == "linux" {
		t.Skip("unsupported-os test only applies outside darwin/linux")
	}
	t.Setenv("GC_HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := doSupervisorInstall(&stdout, &stderr)
	if code != 1 {
		t.Fatalf("doSupervisorInstall code = %d, want 1", code)
	}
}

func TestInstallSupervisorSystemdWarmRefreshGracefullySignalsMainPIDWhenUnitChangesAndServiceActive(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd path only applies on linux")
	}
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))

	data := &supervisorServiceData{
		GCPath:        "/tmp/gc-new",
		LogPath:       "/tmp/gc-home/supervisor.log",
		GCHome:        "/tmp/gc-home",
		XDGRuntimeDir: "/tmp/gc-run",
		Path:          "/usr/local/bin:/usr/bin:/bin",
	}
	path := supervisorSystemdServicePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old unit\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	oldRun := supervisorSystemctlRun
	oldActive := supervisorSystemctlActive
	var calls []string
	supervisorSystemctlRun = func(args ...string) error {
		call := strings.Join(args, " ")
		calls = append(calls, call)
		return nil
	}
	supervisorSystemctlActive = func(service string) bool {
		if service != "gascity-supervisor.service" {
			return false
		}
		for _, call := range calls {
			if call == "--user kill --kill-who=main --signal=SIGTERM "+service {
				return false
			}
		}
		return true
	}
	stubSupervisorRunningPreserveSignalReady(t, true)
	t.Cleanup(func() {
		supervisorSystemctlRun = oldRun
		supervisorSystemctlActive = oldActive
	})

	var stdout, stderr bytes.Buffer
	if code := installSupervisorSystemd(data, &stdout, &stderr); code != 0 {
		t.Fatalf("installSupervisorSystemd code = %d, want 0; stderr=%q", code, stderr.String())
	}
	joined := strings.Join(calls, "\n")
	for _, want := range []string{
		"--user daemon-reload",
		"--user enable gascity-supervisor.service",
		"--user kill --kill-who=main --signal=SIGTERM gascity-supervisor.service",
		"--user reset-failed gascity-supervisor.service",
		"--user start gascity-supervisor.service",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("systemctl calls = %v, want %q", calls, want)
		}
	}
	if strings.Contains(joined, "--user restart gascity-supervisor.service") {
		t.Fatalf("systemctl calls = %v, should signal the old main PID before starting the refreshed unit", calls)
	}
	if strings.Contains(joined, "--signal=SIGKILL") {
		t.Fatalf("systemctl calls = %v, should not hard-kill after graceful warm-refresh stop succeeds", calls)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q): %v", path, err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("systemd unit mode after warm upgrade = %03o, want 600", got)
	}
}

func TestInstallSupervisorSystemdWarmRefreshRefusesActivePrePreserveSupervisor(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd path only applies on linux")
	}
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))

	data := &supervisorServiceData{
		GCPath:        "/tmp/gc-new",
		LogPath:       "/tmp/gc-home/supervisor.log",
		GCHome:        "/tmp/gc-home",
		XDGRuntimeDir: "/tmp/gc-run",
		Path:          "/usr/local/bin:/usr/bin:/bin",
	}
	path := supervisorSystemdServicePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := []byte("old unit\n")
	if err := os.WriteFile(path, previous, 0o644); err != nil {
		t.Fatal(err)
	}

	oldRun := supervisorSystemctlRun
	oldActive := supervisorSystemctlActive
	var calls []string
	supervisorSystemctlRun = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}
	supervisorSystemctlActive = func(service string) bool {
		return service == "gascity-supervisor.service"
	}
	stubSupervisorRunningPreserveSignalReady(t, false)
	t.Cleanup(func() {
		supervisorSystemctlRun = oldRun
		supervisorSystemctlActive = oldActive
	})

	var stdout, stderr bytes.Buffer
	if code := installSupervisorSystemd(data, &stdout, &stderr); code != 1 {
		t.Fatalf("installSupervisorSystemd code = %d, want 1; stderr=%q", code, stderr.String())
	}
	if len(calls) != 0 {
		t.Fatalf("systemctl calls = %v, want none before preserve-mode migration guard passes", calls)
	}
	gotContent, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	if !bytes.Equal(gotContent, previous) {
		t.Fatalf("unit content changed despite guarded warm refresh: got %q want %q", gotContent, previous)
	}
	for _, want := range []string{"does not have " + supervisorPreserveSessionsOnSignalEnv, "gc supervisor stop --wait"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr = %q, want %q", stderr.String(), want)
		}
	}
}

func TestInstallSupervisorSystemdWarmRefreshFallsBackToKillWhenGracefulSignalDoesNotStop(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd path only applies on linux")
	}
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))

	data := &supervisorServiceData{
		GCPath:        "/tmp/gc-new",
		LogPath:       "/tmp/gc-home/supervisor.log",
		GCHome:        "/tmp/gc-home",
		XDGRuntimeDir: "/tmp/gc-run",
		Path:          "/usr/local/bin:/usr/bin:/bin",
	}
	path := supervisorSystemdServicePath()
	service := supervisorSystemdServiceName()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old unit\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	oldRun := supervisorSystemctlRun
	oldActive := supervisorSystemctlActive
	oldTimeout := supervisorSystemdWarmRefreshStopTimeout
	oldPoll := supervisorSystemdWarmRefreshPollInterval
	var calls []string
	supervisorSystemctlRun = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}
	supervisorSystemctlActive = func(string) bool { return true }
	stubSupervisorRunningPreserveSignalReady(t, true)
	supervisorSystemdWarmRefreshStopTimeout = time.Millisecond
	supervisorSystemdWarmRefreshPollInterval = time.Millisecond
	t.Cleanup(func() {
		supervisorSystemctlRun = oldRun
		supervisorSystemctlActive = oldActive
		supervisorSystemdWarmRefreshStopTimeout = oldTimeout
		supervisorSystemdWarmRefreshPollInterval = oldPoll
	})

	var stdout, stderr bytes.Buffer
	if code := installSupervisorSystemd(data, &stdout, &stderr); code != 0 {
		t.Fatalf("installSupervisorSystemd code = %d, want 0; stderr=%q", code, stderr.String())
	}
	joined := strings.Join(calls, "\n")
	for _, want := range []string{
		"--user kill --kill-who=main --signal=SIGTERM " + service,
		"--user kill --kill-who=main --signal=SIGKILL " + service,
		"--user reset-failed " + service,
		"--user start " + service,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("systemctl calls = %v, want %q", calls, want)
		}
	}
}

func TestInstallSupervisorSystemdWarmRefreshStopsWorkspaceServicesBeforeStart(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd path only applies on linux")
	}
	homeDir := t.TempDir()
	gcHome := filepath.Join(t.TempDir(), "gc-home")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)

	data := &supervisorServiceData{
		GCPath:        "/tmp/gc-new",
		LogPath:       "/tmp/gc-home/supervisor.log",
		GCHome:        gcHome,
		XDGRuntimeDir: "/tmp/gc-run",
		Path:          "/usr/local/bin:/usr/bin:/bin",
	}
	path := supervisorSystemdServicePath()
	unitName := supervisorSystemdServiceName()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old unit\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cityPath := filepath.Join(t.TempDir(), "city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(cityPath, "bright-lights"); err != nil {
		t.Fatalf("Register(%q): %v", cityPath, err)
	}
	stateRoot := filepath.Join(cityPath, ".gc", "services", "bridge")
	socketPath := filepath.Join(t.TempDir(), "bridge.sock")
	cmd := exec.Command("sh", "-c", "trap 'exit 0' TERM; while :; do sleep 1; done")
	cmd.Env = append(os.Environ(),
		"GC_HOME="+gcHome,
		"GC_CITY_PATH="+cityPath,
		"GC_SERVICE_NAME=bridge",
		"GC_SERVICE_STATE_ROOT="+stateRoot,
		"GC_SERVICE_SOCKET="+socketPath,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start workspace-service sentinel: %v", err)
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("Getpgid(%d): %v", cmd.Process.Pid, err)
	}
	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()
	t.Cleanup(func() {
		if processGroupAlive(pgid) {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
		select {
		case <-waitCh:
		case <-time.After(time.Second):
			t.Logf("workspace-service sentinel pgid %d did not exit before cleanup timeout", pgid)
		}
	})
	if !processGroupAlive(pgid) {
		t.Fatalf("workspace-service sentinel pgid %d is not alive before warm refresh", pgid)
	}
	scope, err := supervisorWorkspaceServiceCleanupScopeFromRegistry(gcHome)
	if err != nil {
		t.Fatalf("supervisorWorkspaceServiceCleanupScopeFromRegistry: %v", err)
	}
	procs, err := findSupervisorWorkspaceServiceProcesses(scope)
	if err != nil {
		t.Fatalf("findSupervisorWorkspaceServiceProcesses: %v", err)
	}
	if !slices.ContainsFunc(procs, func(proc supervisorWorkspaceServiceProcess) bool { return proc.pgid == pgid }) {
		t.Fatalf("workspace-service discovery procs = %#v, want pgid %d", procs, pgid)
	}

	oldRun := supervisorSystemctlRun
	oldActive := supervisorSystemctlActive
	var (
		calls              []string
		startBeforeCleanup bool
	)
	supervisorSystemctlRun = func(args ...string) error {
		call := strings.Join(args, " ")
		calls = append(calls, call)
		if call == "--user start "+unitName && processGroupAlive(pgid) {
			startBeforeCleanup = true
		}
		return nil
	}
	supervisorSystemctlActive = func(service string) bool {
		if service != unitName {
			return false
		}
		for _, call := range calls {
			if call == "--user kill --kill-who=main --signal=SIGTERM "+unitName {
				return false
			}
		}
		return true
	}
	stubSupervisorRunningPreserveSignalReady(t, true)
	t.Cleanup(func() {
		supervisorSystemctlRun = oldRun
		supervisorSystemctlActive = oldActive
	})

	var stdout, stderr bytes.Buffer
	if code := installSupervisorSystemd(data, &stdout, &stderr); code != 0 {
		t.Fatalf("installSupervisorSystemd code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if startBeforeCleanup {
		t.Fatalf("systemctl start ran before workspace-service pgid %d was stopped; calls=%v", pgid, calls)
	}
	if err := waitForProcessGroupExit(pgid, time.Second); err != nil {
		t.Fatalf("workspace-service cleanup: %v", err)
	}
}

func TestInstallSupervisorSystemdWarmRefreshLeavesUnregisteredWorkspaceServices(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd path only applies on linux")
	}
	homeDir := t.TempDir()
	gcHome := filepath.Join(t.TempDir(), "gc-home")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)

	registeredCity := filepath.Join(t.TempDir(), "registered-city")
	unregisteredCity := filepath.Join(t.TempDir(), "unregistered-city")
	if err := os.MkdirAll(registeredCity, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(unregisteredCity, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(registeredCity, "registered-city"); err != nil {
		t.Fatalf("Register(%q): %v", registeredCity, err)
	}

	data := &supervisorServiceData{
		GCPath:        "/tmp/gc-new",
		LogPath:       "/tmp/gc-home/supervisor.log",
		GCHome:        gcHome,
		XDGRuntimeDir: "/tmp/gc-run",
		Path:          "/usr/local/bin:/usr/bin:/bin",
	}
	path := supervisorSystemdServicePath()
	unitName := supervisorSystemdServiceName()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old unit\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	registered := startWorkspaceServiceSentinel(t, gcHome, registeredCity, "bridge")
	unregistered := startWorkspaceServiceSentinel(t, gcHome, unregisteredCity, "other-bridge")

	oldRun := supervisorSystemctlRun
	oldActive := supervisorSystemctlActive
	supervisorSystemctlRun = func(_ ...string) error { return nil }
	supervisorSystemctlActive = func(service string) bool {
		return service == unitName
	}
	stubSupervisorRunningPreserveSignalReady(t, true)
	t.Cleanup(func() {
		supervisorSystemctlRun = oldRun
		supervisorSystemctlActive = oldActive
	})

	var stdout, stderr bytes.Buffer
	if code := installSupervisorSystemd(data, &stdout, &stderr); code != 0 {
		t.Fatalf("installSupervisorSystemd code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if err := waitForProcessGroupExit(registered.pgid, time.Second); err != nil {
		t.Fatalf("registered workspace-service cleanup: %v", err)
	}
	if !processGroupAlive(unregistered.pgid) {
		t.Fatalf("unregistered workspace-service pgid %d was stopped by warm-refresh cleanup", unregistered.pgid)
	}
}

func TestCleanupSupervisorWorkspaceServicesForSupervisorStartSkipsMissingProc(t *testing.T) {
	homeDir := t.TempDir()
	gcHome := filepath.Join(t.TempDir(), "gc-home")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)

	cityPath := filepath.Join(t.TempDir(), "city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(cityPath, "bright-lights"); err != nil {
		t.Fatalf("Register(%q): %v", cityPath, err)
	}

	oldRoot := supervisorProcRoot
	oldReadDir := supervisorProcReadDir
	supervisorProcRoot = filepath.Join(t.TempDir(), "missing-proc")
	supervisorProcReadDir = os.ReadDir
	t.Cleanup(func() {
		supervisorProcRoot = oldRoot
		supervisorProcReadDir = oldReadDir
	})

	if err := cleanupSupervisorWorkspaceServicesForSupervisorStart(gcHome); err != nil {
		t.Fatalf("cleanupSupervisorWorkspaceServicesForSupervisorStart: %v", err)
	}
}

func TestCleanupSupervisorWorkspaceServicesForSupervisorStartWarnsWhenProcCleanupUnsupported(t *testing.T) {
	homeDir := t.TempDir()
	gcHome := filepath.Join(t.TempDir(), "gc-home")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)

	cityPath := filepath.Join(t.TempDir(), "city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(cityPath, "bright-lights"); err != nil {
		t.Fatalf("Register(%q): %v", cityPath, err)
	}

	oldGOOS := supervisorRuntimeGOOS
	oldWarnings := supervisorWorkspaceServiceCleanupWarnings
	var warnings bytes.Buffer
	supervisorRuntimeGOOS = "darwin"
	supervisorWorkspaceServiceCleanupWarnings = &warnings
	t.Cleanup(func() {
		supervisorRuntimeGOOS = oldGOOS
		supervisorWorkspaceServiceCleanupWarnings = oldWarnings
	})

	if err := cleanupSupervisorWorkspaceServicesForSupervisorStart(gcHome); err != nil {
		t.Fatalf("cleanupSupervisorWorkspaceServicesForSupervisorStart: %v", err)
	}
	if got := warnings.String(); !strings.Contains(got, "workspace-service startup cleanup is not available on darwin") ||
		!strings.Contains(got, citylayout.RuntimeServicesDir(cityPath)) ||
		!strings.Contains(got, "GC_SERVICE_STATE_ROOT") {
		t.Fatalf("cleanup warning = %q, want macOS operator guidance", got)
	}
}

func TestFindSupervisorWorkspaceServiceProcessesFiltersOwnershipAndRequiredEnv(t *testing.T) {
	gcHome := filepath.Join(t.TempDir(), "gc-home")
	otherHome := filepath.Join(t.TempDir(), "other-home")
	cityPath := filepath.Join(t.TempDir(), "city")
	otherCity := filepath.Join(t.TempDir(), "other-city")
	serviceRoot := filepath.Join(cityPath, ".gc", "services", "bridge")
	procRoot := t.TempDir()
	baseEnv := map[string]string{
		"GC_HOME":                   gcHome,
		"GC_CITY_PATH":              cityPath,
		"GC_SERVICE_NAME":           "bridge",
		"GC_SERVICE_STATE_ROOT":     serviceRoot,
		"GC_SERVICE_SOCKET":         filepath.Join(t.TempDir(), "bridge.sock"),
		"GC_CITY_RUNTIME_DIR":       filepath.Join(cityPath, ".gc", "runtime"),
		"GC_SERVICE_RUN_ROOT":       filepath.Join(serviceRoot, "run"),
		"GC_SERVICE_URL_PREFIX":     "/svc/bridge",
		"GC_SERVICE_VISIBILITY":     "private",
		"GC_PUBLISHED_SERVICES":     filepath.Join(cityPath, ".gc", "services", ".published"),
		"GC_PUBLISHED_SERVICES_DIR": filepath.Join(cityPath, ".gc", "services", ".published"),
	}
	writeSupervisorProcEnv(t, procRoot, 101, baseEnv)
	missingSocket := map[string]string{}
	for k, v := range baseEnv {
		missingSocket[k] = v
	}
	delete(missingSocket, "GC_SERVICE_SOCKET")
	writeSupervisorProcEnv(t, procRoot, 102, missingSocket)
	otherHomeEnv := map[string]string{}
	for k, v := range baseEnv {
		otherHomeEnv[k] = v
	}
	otherHomeEnv["GC_HOME"] = otherHome
	writeSupervisorProcEnv(t, procRoot, 103, otherHomeEnv)
	otherCityEnv := map[string]string{}
	for k, v := range baseEnv {
		otherCityEnv[k] = v
	}
	otherCityEnv["GC_CITY_PATH"] = otherCity
	otherCityEnv["GC_SERVICE_STATE_ROOT"] = filepath.Join(otherCity, ".gc", "services", "bridge")
	writeSupervisorProcEnv(t, procRoot, 104, otherCityEnv)
	outsideStateEnv := map[string]string{}
	for k, v := range baseEnv {
		outsideStateEnv[k] = v
	}
	outsideStateEnv["GC_SERVICE_STATE_ROOT"] = filepath.Join(cityPath, ".gc", "not-services", "bridge")
	writeSupervisorProcEnv(t, procRoot, 105, outsideStateEnv)

	setSupervisorProcTestHooks(t, procRoot, func(pid int) (int, error) {
		return pid + 1000, nil
	})
	scope := supervisorWorkspaceServiceCleanupScope{
		gcHome: normalizePathForCompare(gcHome),
		cityPaths: map[string]string{
			normalizePathForCompare(cityPath): normalizePathForCompare(cityPath),
		},
	}
	procs, err := findSupervisorWorkspaceServiceProcesses(scope)
	if err != nil {
		t.Fatalf("findSupervisorWorkspaceServiceProcesses: %v", err)
	}
	if len(procs) != 1 || procs[0].pid != 101 || procs[0].pgid != 1101 {
		t.Fatalf("procs = %#v, want only owned pid 101", procs)
	}
}

func TestFindSupervisorWorkspaceServiceProcessesSkipsUnsafeAndVanished(t *testing.T) {
	gcHome := filepath.Join(t.TempDir(), "gc-home")
	cityPath := filepath.Join(t.TempDir(), "city")
	procRoot := t.TempDir()
	for _, pid := range []int{201, 202, 203, 204} {
		writeSupervisorProcEnv(t, procRoot, pid, map[string]string{
			"GC_HOME":               gcHome,
			"GC_CITY_PATH":          cityPath,
			"GC_SERVICE_NAME":       "bridge",
			"GC_SERVICE_STATE_ROOT": filepath.Join(cityPath, ".gc", "services", "bridge"),
			"GC_SERVICE_SOCKET":     filepath.Join(t.TempDir(), "bridge.sock"),
		})
	}
	setSupervisorProcTestHooks(t, procRoot, func(pid int) (int, error) {
		switch pid {
		case 201:
			return 0, syscall.ESRCH
		case 202:
			return 1, nil
		case 203:
			return 4242, nil
		case 204:
			return 5204, nil
		default:
			return pid + 1000, nil
		}
	})
	scope := supervisorWorkspaceServiceCleanupScope{
		gcHome: normalizePathForCompare(gcHome),
		cityPaths: map[string]string{
			normalizePathForCompare(cityPath): normalizePathForCompare(cityPath),
		},
	}
	oldWarnings := supervisorWorkspaceServiceCleanupWarnings
	var warnings bytes.Buffer
	supervisorWorkspaceServiceCleanupWarnings = &warnings
	t.Cleanup(func() {
		supervisorWorkspaceServiceCleanupWarnings = oldWarnings
	})

	procs, err := findSupervisorWorkspaceServiceProcesses(scope)
	if err != nil {
		t.Fatalf("findSupervisorWorkspaceServiceProcesses: %v", err)
	}
	if len(procs) != 1 || procs[0].pid != 204 || procs[0].pgid != 5204 {
		t.Fatalf("procs = %#v, want only safe pid 204", procs)
	}
	if got := warnings.String(); !strings.Contains(got, "unsafe process group 1") || !strings.Contains(got, "unsafe process group 4242") {
		t.Fatalf("warnings = %q, want unsafe process group diagnostics", got)
	}
}

func TestTerminateProcessGroupTreatsESRCHAsAlreadyStopped(t *testing.T) {
	oldKill := supervisorKill
	oldPoll := supervisorProcessGroupPollPeriod
	supervisorKill = func(_ int, _ syscall.Signal) error {
		return syscall.ESRCH
	}
	supervisorProcessGroupPollPeriod = time.Millisecond
	t.Cleanup(func() {
		supervisorKill = oldKill
		supervisorProcessGroupPollPeriod = oldPoll
	})

	if err := terminateProcessGroup(999999, time.Millisecond); err != nil {
		t.Fatalf("terminateProcessGroup ESRCH = %v, want nil", err)
	}
}

func TestTerminateProcessGroupRefusesCurrentProcessGroup(t *testing.T) {
	if err := terminateProcessGroup(syscall.Getpgrp(), time.Millisecond); err == nil {
		t.Fatal("terminateProcessGroup current process group error = nil, want refusal")
	}
}

func TestInstallSupervisorSystemdWarmRefreshPreservesNewUnitWhenStartFails(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd path only applies on linux")
	}
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))

	data := &supervisorServiceData{
		GCPath:        "/tmp/gc-new",
		LogPath:       "/tmp/gc-home/supervisor.log",
		GCHome:        "/tmp/gc-home",
		XDGRuntimeDir: "/tmp/gc-run",
		Path:          "/usr/local/bin:/usr/bin:/bin",
	}
	path := supervisorSystemdServicePath()
	unitName := supervisorSystemdServiceName()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := []byte("old unit\n")
	if err := os.WriteFile(path, previous, 0o644); err != nil {
		t.Fatal(err)
	}

	oldRun := supervisorSystemctlRun
	oldActive := supervisorSystemctlActive
	var (
		calls      []string
		startCalls int
	)
	supervisorSystemctlRun = func(args ...string) error {
		call := strings.Join(args, " ")
		calls = append(calls, call)
		if call == "--user start "+unitName {
			startCalls++
			if startCalls == 1 {
				return errors.New("start failed")
			}
		}
		return nil
	}
	supervisorSystemctlActive = func(service string) bool {
		if service != unitName {
			return false
		}
		for _, call := range calls {
			if call == "--user kill --kill-who=main --signal=SIGTERM "+unitName {
				return false
			}
		}
		return true
	}
	stubSupervisorRunningPreserveSignalReady(t, true)
	t.Cleanup(func() {
		supervisorSystemctlRun = oldRun
		supervisorSystemctlActive = oldActive
	})

	var stdout, stderr bytes.Buffer
	if code := installSupervisorSystemd(data, &stdout, &stderr); code != 1 {
		t.Fatalf("installSupervisorSystemd code = %d, want 1; stderr=%q", code, stderr.String())
	}
	gotContent, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	if bytes.Equal(gotContent, previous) || !bytes.Contains(gotContent, []byte("KillMode=process")) {
		t.Fatalf("unit after failed warm refresh = %q, want refreshed unit with KillMode=process", gotContent)
	}
	joined := strings.Join(calls, "\n")
	for _, want := range []string{
		"--user kill --kill-who=main --signal=SIGTERM " + unitName,
		"--user reset-failed " + unitName,
		"--user start " + unitName,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("systemctl calls = %v, want %q", calls, want)
		}
	}
	if strings.Contains(joined, "--user stop "+unitName) {
		t.Fatalf("systemctl calls = %v, should not stop and restart a previous unit after failed warm refresh", calls)
	}
	if startCalls != 1 {
		t.Fatalf("systemctl start calls = %d, want only failed refresh start; calls=%v", startCalls, calls)
	}
	if !strings.Contains(stderr.String(), "systemctl --user start "+unitName+": start failed") {
		t.Fatalf("stderr = %q, want failed refresh start", stderr.String())
	}
	if !strings.Contains(stderr.String(), "leaving refreshed systemd unit") {
		t.Fatalf("stderr = %q, want refreshed-unit rollback guidance", stderr.String())
	}
}

func TestInstallSupervisorSystemdWarmRefreshPreservesNewUnitWhenCleanupFails(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd path only applies on linux")
	}
	homeDir := t.TempDir()
	gcHome := filepath.Join(t.TempDir(), "gc-home")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)

	data := &supervisorServiceData{
		GCPath:        "/tmp/gc-new",
		LogPath:       "/tmp/gc-home/supervisor.log",
		GCHome:        gcHome,
		XDGRuntimeDir: "/tmp/gc-run",
		Path:          "/usr/local/bin:/usr/bin:/bin",
	}
	path := supervisorSystemdServicePath()
	unitName := supervisorSystemdServiceName()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := []byte("old unit\n")
	if err := os.WriteFile(path, previous, 0o644); err != nil {
		t.Fatal(err)
	}

	cityPath := filepath.Join(t.TempDir(), "city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(cityPath, "bright-lights"); err != nil {
		t.Fatalf("Register(%q): %v", cityPath, err)
	}

	oldRun := supervisorSystemctlRun
	oldActive := supervisorSystemctlActive
	oldReadDir := supervisorProcReadDir
	var (
		calls      []string
		startCalls int
	)
	supervisorSystemctlRun = func(args ...string) error {
		call := strings.Join(args, " ")
		calls = append(calls, call)
		if call == "--user start "+unitName {
			startCalls++
		}
		return nil
	}
	supervisorSystemctlActive = func(service string) bool {
		if service != unitName {
			return false
		}
		for _, call := range calls {
			if call == "--user kill --kill-who=main --signal=SIGTERM "+unitName {
				return false
			}
		}
		return true
	}
	stubSupervisorRunningPreserveSignalReady(t, true)
	supervisorProcReadDir = func(string) ([]os.DirEntry, error) {
		return nil, errors.New("proc scan failed")
	}
	t.Cleanup(func() {
		supervisorSystemctlRun = oldRun
		supervisorSystemctlActive = oldActive
		supervisorProcReadDir = oldReadDir
	})

	var stdout, stderr bytes.Buffer
	if code := installSupervisorSystemd(data, &stdout, &stderr); code != 1 {
		t.Fatalf("installSupervisorSystemd code = %d, want 1; stderr=%q", code, stderr.String())
	}
	gotContent, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	if bytes.Equal(gotContent, previous) || !bytes.Contains(gotContent, []byte("KillMode=process")) {
		t.Fatalf("unit after failed cleanup = %q, want refreshed unit with KillMode=process", gotContent)
	}
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "--user kill --kill-who=main --signal=SIGTERM "+unitName) {
		t.Fatalf("systemctl calls = %v, want warm-refresh graceful signal", calls)
	}
	if strings.Contains(joined, "--user stop "+unitName) {
		t.Fatalf("systemctl calls = %v, should not stop and restart a previous unit after failed cleanup", calls)
	}
	if startCalls != 0 {
		t.Fatalf("systemctl start calls = %d, want no start after cleanup failure; calls=%v", startCalls, calls)
	}
	if !strings.Contains(stderr.String(), "workspace-service cleanup after systemctl --user kill") {
		t.Fatalf("stderr = %q, want cleanup failure", stderr.String())
	}
	if !strings.Contains(stderr.String(), "leaving refreshed systemd unit") {
		t.Fatalf("stderr = %q, want refreshed-unit rollback guidance", stderr.String())
	}
}

func TestInstallSupervisorSystemdWritesPrivateUnitFile(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd path only applies on linux")
	}
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))

	data := &supervisorServiceData{
		GCPath:  "/tmp/gc-new",
		LogPath: "/tmp/gc-home/supervisor.log",
		GCHome:  "/tmp/gc-home",
		Path:    "/usr/local/bin:/usr/bin:/bin",
		ExtraEnv: []supervisorServiceEnvVar{
			{Name: "OPENAI_API_KEY", Value: "sk-openai-123"},
		},
	}

	oldRun := supervisorSystemctlRun
	oldActive := supervisorSystemctlActive
	supervisorSystemctlRun = func(_ ...string) error {
		return nil
	}
	supervisorSystemctlActive = func(_ string) bool {
		return false
	}
	t.Cleanup(func() {
		supervisorSystemctlRun = oldRun
		supervisorSystemctlActive = oldActive
	})

	var stdout, stderr bytes.Buffer
	if code := installSupervisorSystemd(data, &stdout, &stderr); code != 0 {
		t.Fatalf("installSupervisorSystemd code = %d, want 0; stderr=%q", code, stderr.String())
	}
	info, err := os.Stat(supervisorSystemdServicePath())
	if err != nil {
		t.Fatalf("Stat(%q): %v", supervisorSystemdServicePath(), err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("systemd unit mode = %03o, want 600", got)
	}
}

func TestInstallSupervisorSystemdStartsInactiveService(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd path only applies on linux")
	}
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))

	data := &supervisorServiceData{
		GCPath:        "/tmp/gc-new",
		LogPath:       "/tmp/gc-home/supervisor.log",
		GCHome:        "/tmp/gc-home",
		XDGRuntimeDir: "/tmp/gc-run",
		Path:          "/usr/local/bin:/usr/bin:/bin",
	}

	oldRun := supervisorSystemctlRun
	oldActive := supervisorSystemctlActive
	var calls []string
	supervisorSystemctlRun = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}
	supervisorSystemctlActive = func(_ string) bool {
		return false
	}
	t.Cleanup(func() {
		supervisorSystemctlRun = oldRun
		supervisorSystemctlActive = oldActive
	})

	var stdout, stderr bytes.Buffer
	if code := installSupervisorSystemd(data, &stdout, &stderr); code != 0 {
		t.Fatalf("installSupervisorSystemd code = %d, want 0; stderr=%q", code, stderr.String())
	}
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "--user start gascity-supervisor.service") {
		t.Fatalf("systemctl calls = %v, want start for inactive service", calls)
	}
	if strings.Contains(joined, "--user restart gascity-supervisor.service") {
		t.Fatalf("systemctl calls = %v, should not restart inactive service", calls)
	}
}

func TestInstallSupervisorSystemdUsesIsolatedUnitNameForIsolatedGCHome(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd path only applies on linux")
	}
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	isolatedHome := filepath.Join(t.TempDir(), "isolated-home")
	t.Setenv("GC_HOME", isolatedHome)

	data := &supervisorServiceData{
		GCPath:        "/tmp/gc-new",
		LogPath:       filepath.Join(isolatedHome, "supervisor.log"),
		GCHome:        isolatedHome,
		XDGRuntimeDir: "",
		Path:          "/usr/local/bin:/usr/bin:/bin",
	}

	oldRun := supervisorSystemctlRun
	oldActive := supervisorSystemctlActive
	var calls []string
	supervisorSystemctlRun = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}
	supervisorSystemctlActive = func(_ string) bool {
		return false
	}
	t.Cleanup(func() {
		supervisorSystemctlRun = oldRun
		supervisorSystemctlActive = oldActive
	})

	var stdout, stderr bytes.Buffer
	if code := installSupervisorSystemd(data, &stdout, &stderr); code != 0 {
		t.Fatalf("installSupervisorSystemd code = %d, want 0; stderr=%q", code, stderr.String())
	}

	wantName := supervisorSystemdServiceName()
	if wantName == defaultSupervisorSystemdUnit {
		t.Fatalf("supervisorSystemdServiceName() = %q, want isolated unit name", wantName)
	}
	if !strings.HasPrefix(wantName, "gascity-supervisor-isolated-home-") {
		t.Fatalf("supervisorSystemdServiceName() = %q, want isolated-home-prefixed name", wantName)
	}
	wantPath := filepath.Join(homeDir, ".local", "share", "systemd", "user", wantName)
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("Stat(%q): %v", wantPath, err)
	}
	defaultPath := filepath.Join(homeDir, ".local", "share", "systemd", "user", "gascity-supervisor.service")
	if _, err := os.Stat(defaultPath); !os.IsNotExist(err) {
		t.Fatalf("default systemd unit %q should stay absent; err=%v", defaultPath, err)
	}

	joined := strings.Join(calls, "\n")
	for _, want := range []string{
		"--user enable " + wantName,
		"--user start " + wantName,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("systemctl calls = %v, want %q", calls, want)
		}
	}
	if strings.Contains(joined, "gascity-supervisor.service") {
		t.Fatalf("systemctl calls = %v, should not target the default unit when GC_HOME is isolated", calls)
	}
}

func TestUnloadSupervisorServiceSkipsDefaultUnitForIsolatedGCHome(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd path only applies on linux")
	}
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(t.TempDir(), "isolated-home"))
	logFile := installFakeSystemctl(t)

	defaultPath := filepath.Join(homeDir, ".local", "share", "systemd", "user", "gascity-supervisor.service")
	if err := os.MkdirAll(filepath.Dir(defaultPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(defaultPath, []byte("[Unit]\nDescription=test\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	unloadSupervisorService()

	if got := strings.TrimSpace(readCommandLog(t, logFile)); got != "" {
		t.Fatalf("unloadSupervisorService invoked systemctl for default unit under isolated GC_HOME: %q", got)
	}
}

func TestUnloadSupervisorServiceUsesIsolatedUnitWhenPresent(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd path only applies on linux")
	}
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(t.TempDir(), "isolated-home"))
	logFile := installFakeSystemctl(t)

	unitPath := filepath.Join(homeDir, ".local", "share", "systemd", "user", supervisorSystemdServiceName())
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unitPath, []byte("[Unit]\nDescription=test\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	unloadSupervisorService()

	got := strings.TrimSpace(readCommandLog(t, logFile))
	if !strings.Contains(got, "--user stop "+supervisorSystemdServiceName()) {
		t.Fatalf("systemctl log = %q, want isolated unit stop", got)
	}
	if strings.Contains(got, "--user stop gascity-supervisor.service") {
		t.Fatalf("systemctl log = %q, should not target the default unit", got)
	}
}

func TestUnloadSupervisorServiceStopsMatchingLegacyDefaultUnitForIsolatedGCHome(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd path only applies on linux")
	}
	homeDir := t.TempDir()
	gcHome := filepath.Join(t.TempDir(), "isolated-home")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)
	logFile := installFakeSystemctl(t)

	legacyPath := filepath.Join(homeDir, ".local", "share", "systemd", "user", defaultSupervisorSystemdUnit)
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte("Environment=GC_HOME=\""+gcHome+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	unloadSupervisorService()

	got := strings.TrimSpace(readCommandLog(t, logFile))
	if !strings.Contains(got, "--user stop "+defaultSupervisorSystemdUnit) {
		t.Fatalf("systemctl log = %q, want legacy default unit stop", got)
	}
}

func TestLegacySupervisorTargetsCurrentHomeLaunchdDecodesEscapedGC_HOME(t *testing.T) {
	homeDir := t.TempDir()
	gcHome := filepath.Join(t.TempDir(), "isolated&home")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)

	legacyPath := legacySupervisorLaunchdPlistPath()
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	content, err := renderSupervisorTemplate(supervisorLaunchdTemplate, &supervisorServiceData{
		GCPath:       "/tmp/gc",
		LogPath:      filepath.Join(gcHome, "supervisor.log"),
		GCHome:       gcHome,
		LaunchdLabel: defaultSupervisorLaunchdLabel,
		Path:         "/usr/local/bin:/usr/bin:/bin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	if !legacySupervisorTargetsCurrentHome(legacyPath) {
		t.Fatalf("legacySupervisorTargetsCurrentHome(%q) = false, want true for escaped GC_HOME", legacyPath)
	}
}

func TestLegacySupervisorTargetsCurrentHomeRequiresExactSystemdGC_HOMEMatch(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd path only applies on linux")
	}
	homeDir := t.TempDir()
	gcHome := filepath.Join(t.TempDir(), "isolated-home")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)

	legacyPath := legacySupervisorSystemdServicePath()
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	content, err := renderSupervisorTemplate(supervisorSystemdTemplate, &supervisorServiceData{
		GCPath: "/tmp/gc",
		LogPath: filepath.Join(
			gcHome,
			"supervisor.log",
		),
		GCHome: filepath.Join(filepath.Dir(gcHome), filepath.Base(gcHome)+"-other"),
		Path:   "/usr/local/bin:/usr/bin:/bin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	if legacySupervisorTargetsCurrentHome(legacyPath) {
		t.Fatalf("legacySupervisorTargetsCurrentHome(%q) = true, want false for non-exact GC_HOME match", legacyPath)
	}
}

func TestLegacySupervisorTargetsCurrentHomeMatchesEquivalentSystemdHomePaths(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd path only applies on linux")
	}
	homeDir := t.TempDir()
	canonicalHome := filepath.Join(t.TempDir(), "isolated-home")
	if err := os.MkdirAll(canonicalHome, 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkHome := filepath.Join(t.TempDir(), "isolated-home-link")
	if err := os.Symlink(canonicalHome, symlinkHome); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", symlinkHome)

	legacyPath := legacySupervisorSystemdServicePath()
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	content, err := renderSupervisorTemplate(supervisorSystemdTemplate, &supervisorServiceData{
		GCPath:   "/tmp/gc",
		LogPath:  filepath.Join(canonicalHome, "supervisor.log"),
		GCHome:   canonicalHome,
		Path:     "/usr/local/bin:/usr/bin:/bin",
		SafeName: sanitizeServiceName(filepath.Base(canonicalHome)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	if !legacySupervisorTargetsCurrentHome(legacyPath) {
		t.Fatalf("legacySupervisorTargetsCurrentHome(%q) = false, want true for equivalent GC_HOME paths", legacyPath)
	}
}

func TestInstallSupervisorSystemdRemovesMatchingLegacyDefaultUnitForIsolatedGCHome(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd path only applies on linux")
	}
	homeDir := t.TempDir()
	gcHome := filepath.Join(t.TempDir(), "isolated-home")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)

	legacyPath := filepath.Join(homeDir, ".local", "share", "systemd", "user", defaultSupervisorSystemdUnit)
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte("Environment=GC_HOME=\""+gcHome+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	data := &supervisorServiceData{
		GCPath:        "/tmp/gc-new",
		LogPath:       filepath.Join(gcHome, "supervisor.log"),
		GCHome:        gcHome,
		XDGRuntimeDir: "",
		LaunchdLabel:  supervisorLaunchdLabel(),
		Path:          "/usr/local/bin:/usr/bin:/bin",
	}

	oldRun := supervisorSystemctlRun
	oldActive := supervisorSystemctlActive
	var calls []string
	supervisorSystemctlRun = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}
	supervisorSystemctlActive = func(_ string) bool {
		return false
	}
	t.Cleanup(func() {
		supervisorSystemctlRun = oldRun
		supervisorSystemctlActive = oldActive
	})

	var stdout, stderr bytes.Buffer
	if code := installSupervisorSystemd(data, &stdout, &stderr); code != 0 {
		t.Fatalf("installSupervisorSystemd code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("legacy systemd unit %q should be removed; err=%v", legacyPath, err)
	}
	joined := strings.Join(calls, "\n")
	for _, want := range []string{
		"--user stop " + defaultSupervisorSystemdUnit,
		"--user disable " + defaultSupervisorSystemdUnit,
		"--user enable " + supervisorSystemdServiceName(),
		"--user start " + supervisorSystemdServiceName(),
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("systemctl calls = %v, want %q", calls, want)
		}
	}
}

func TestInstallSupervisorSystemdIgnoresLegacyStopDisableFailures(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd path only applies on linux")
	}
	homeDir := t.TempDir()
	gcHome := filepath.Join(t.TempDir(), "isolated-home")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)

	legacyPath := legacySupervisorSystemdServicePath()
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte("Environment=GC_HOME=\""+gcHome+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	data := &supervisorServiceData{
		GCPath:        "/tmp/gc-new",
		LogPath:       filepath.Join(gcHome, "supervisor.log"),
		GCHome:        gcHome,
		XDGRuntimeDir: "",
		LaunchdLabel:  supervisorLaunchdLabel(),
		Path:          "/usr/local/bin:/usr/bin:/bin",
	}

	oldRun := supervisorSystemctlRun
	oldActive := supervisorSystemctlActive
	supervisorSystemctlRun = func(args ...string) error {
		if len(args) >= 3 && args[1] == "stop" && args[2] == defaultSupervisorSystemdUnit {
			return errors.New("legacy stop failed")
		}
		if len(args) >= 3 && args[1] == "disable" && args[2] == defaultSupervisorSystemdUnit {
			return errors.New("legacy disable failed")
		}
		return nil
	}
	supervisorSystemctlActive = func(_ string) bool {
		return false
	}
	t.Cleanup(func() {
		supervisorSystemctlRun = oldRun
		supervisorSystemctlActive = oldActive
	})

	var stdout, stderr bytes.Buffer
	if code := installSupervisorSystemd(data, &stdout, &stderr); code != 0 {
		t.Fatalf("installSupervisorSystemd code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("legacy systemd unit %q should be removed despite stop/disable failures; err=%v", legacyPath, err)
	}
}

func TestInstallSupervisorSystemdKeepsLegacyUnitWhenNewServiceFails(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd path only applies on linux")
	}
	homeDir := t.TempDir()
	gcHome := filepath.Join(t.TempDir(), "isolated-home")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)

	legacyPath := legacySupervisorSystemdServicePath()
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte("Environment=GC_HOME=\""+gcHome+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	data := &supervisorServiceData{
		GCPath:        "/tmp/gc-new",
		LogPath:       filepath.Join(gcHome, "supervisor.log"),
		GCHome:        gcHome,
		XDGRuntimeDir: "",
		LaunchdLabel:  supervisorLaunchdLabel(),
		Path:          "/usr/local/bin:/usr/bin:/bin",
	}

	oldRun := supervisorSystemctlRun
	oldActive := supervisorSystemctlActive
	var calls []string
	supervisorSystemctlRun = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		if len(args) >= 3 && args[1] == "start" && args[2] == supervisorSystemdServiceName() {
			return errors.New("new unit failed to start")
		}
		return nil
	}
	supervisorSystemctlActive = func(_ string) bool {
		return false
	}
	t.Cleanup(func() {
		supervisorSystemctlRun = oldRun
		supervisorSystemctlActive = oldActive
	})

	var stdout, stderr bytes.Buffer
	if code := installSupervisorSystemd(data, &stdout, &stderr); code != 1 {
		t.Fatalf("installSupervisorSystemd code = %d, want 1; stderr=%q", code, stderr.String())
	}
	currentPath := supervisorSystemdServicePath()
	if _, err := os.Stat(currentPath); !os.IsNotExist(err) {
		t.Fatalf("new systemd unit %q should be removed during rollback; err=%v", currentPath, err)
	}
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("legacy systemd unit %q should remain after failed install; err=%v", legacyPath, err)
	}
	joined := strings.Join(calls, "\n")
	for _, want := range []string{
		"--user stop " + defaultSupervisorSystemdUnit,
		"--user start " + supervisorSystemdServiceName(),
		"--user disable " + supervisorSystemdServiceName(),
		"--user start " + defaultSupervisorSystemdUnit,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("systemctl calls = %v, want %q", calls, want)
		}
	}
}

func TestInstallSupervisorSystemdKeepsLegacyUnitWhenEarlySetupFails(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd path only applies on linux")
	}
	for _, tc := range []struct {
		name     string
		failVerb string
	}{
		{name: "daemon-reload", failVerb: "daemon-reload"},
		{name: "enable", failVerb: "enable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			homeDir := t.TempDir()
			gcHome := filepath.Join(t.TempDir(), "isolated-home")
			t.Setenv("HOME", homeDir)
			t.Setenv("GC_HOME", gcHome)

			legacyPath := legacySupervisorSystemdServicePath()
			if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(legacyPath, []byte("Environment=GC_HOME=\""+gcHome+"\"\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			data := &supervisorServiceData{
				GCPath:        "/tmp/gc-new",
				LogPath:       filepath.Join(gcHome, "supervisor.log"),
				GCHome:        gcHome,
				XDGRuntimeDir: "",
				LaunchdLabel:  supervisorLaunchdLabel(),
				Path:          "/usr/local/bin:/usr/bin:/bin",
			}

			oldRun := supervisorSystemctlRun
			oldActive := supervisorSystemctlActive
			var calls []string
			failed := false
			supervisorSystemctlRun = func(args ...string) error {
				call := strings.Join(args, " ")
				calls = append(calls, call)
				if !failed && len(args) >= 2 && args[1] == tc.failVerb {
					failed = true
					return errors.New("early setup failed")
				}
				return nil
			}
			supervisorSystemctlActive = func(_ string) bool {
				return false
			}
			t.Cleanup(func() {
				supervisorSystemctlRun = oldRun
				supervisorSystemctlActive = oldActive
			})

			var stdout, stderr bytes.Buffer
			if code := installSupervisorSystemd(data, &stdout, &stderr); code != 1 {
				t.Fatalf("installSupervisorSystemd code = %d, want 1; stderr=%q", code, stderr.String())
			}
			currentPath := supervisorSystemdServicePath()
			if _, err := os.Stat(currentPath); !os.IsNotExist(err) {
				t.Fatalf("new systemd unit %q should be removed during rollback; err=%v", currentPath, err)
			}
			if _, err := os.Stat(legacyPath); err != nil {
				t.Fatalf("legacy systemd unit %q should remain after failed install; err=%v", legacyPath, err)
			}
			joined := strings.Join(calls, "\n")
			for _, notWant := range []string{
				"--user stop " + defaultSupervisorSystemdUnit,
				"--user start " + defaultSupervisorSystemdUnit,
			} {
				if strings.Contains(joined, notWant) {
					t.Fatalf("systemctl calls = %v, did not want %q before legacy unload", calls, notWant)
				}
			}
		})
	}
}

func TestInstallSupervisorSystemdRestoresPreviousCurrentUnitWhenUpdateFails(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd path only applies on linux")
	}
	homeDir := t.TempDir()
	gcHome := filepath.Join(t.TempDir(), "isolated-home")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)

	currentPath := supervisorSystemdServicePath()
	if err := os.MkdirAll(filepath.Dir(currentPath), 0o755); err != nil {
		t.Fatal(err)
	}
	oldContent := []byte("old isolated unit\n")
	if err := os.WriteFile(currentPath, oldContent, 0o644); err != nil {
		t.Fatal(err)
	}

	data := &supervisorServiceData{
		GCPath:        "/tmp/gc-new",
		LogPath:       filepath.Join(gcHome, "supervisor.log"),
		GCHome:        gcHome,
		XDGRuntimeDir: "",
		LaunchdLabel:  supervisorLaunchdLabel(),
		Path:          "/usr/local/bin:/usr/bin:/bin",
	}

	oldRun := supervisorSystemctlRun
	oldActive := supervisorSystemctlActive
	var calls []string
	startCalls := 0
	supervisorSystemctlRun = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		if len(args) >= 3 && args[1] == "start" && args[2] == supervisorSystemdServiceName() {
			startCalls++
			if startCalls == 1 {
				return errors.New("new unit failed to start")
			}
		}
		return nil
	}
	supervisorSystemctlActive = func(_ string) bool {
		return false
	}
	t.Cleanup(func() {
		supervisorSystemctlRun = oldRun
		supervisorSystemctlActive = oldActive
	})

	var stdout, stderr bytes.Buffer
	if code := installSupervisorSystemd(data, &stdout, &stderr); code != 1 {
		t.Fatalf("installSupervisorSystemd code = %d, want 1; stderr=%q", code, stderr.String())
	}
	gotContent, err := os.ReadFile(currentPath)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", currentPath, err)
	}
	if !bytes.Equal(gotContent, oldContent) {
		t.Fatalf("restored systemd unit = %q, want original %q", gotContent, oldContent)
	}
	info, err := os.Stat(currentPath)
	if err != nil {
		t.Fatalf("Stat(%q): %v", currentPath, err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("restored systemd unit mode = %03o, want 600", got)
	}
	if startCalls != 2 {
		t.Fatalf("systemctl start call count = %d, want 2 (failed install + rollback restore); calls=%v", startCalls, calls)
	}
}

func TestUninstallSupervisorSystemdRemovesMatchingLegacyDefaultUnitForIsolatedGCHome(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd path only applies on linux")
	}
	homeDir := t.TempDir()
	gcHome := filepath.Join(t.TempDir(), "isolated-home")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)

	currentPath := filepath.Join(homeDir, ".local", "share", "systemd", "user", supervisorSystemdServiceName())
	legacyPath := filepath.Join(homeDir, ".local", "share", "systemd", "user", defaultSupervisorSystemdUnit)
	for _, path := range []string{currentPath, legacyPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("Environment=GC_HOME=\""+gcHome+"\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	oldRun := supervisorSystemctlRun
	var calls []string
	supervisorSystemctlRun = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}
	t.Cleanup(func() {
		supervisorSystemctlRun = oldRun
	})

	var stdout, stderr bytes.Buffer
	if code := uninstallSupervisorSystemd(&supervisorServiceData{}, &stdout, &stderr); code != 0 {
		t.Fatalf("uninstallSupervisorSystemd code = %d, want 0; stderr=%q", code, stderr.String())
	}
	for _, path := range []string{currentPath, legacyPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("systemd unit %q should be removed; err=%v", path, err)
		}
	}
	joined := strings.Join(calls, "\n")
	for _, want := range []string{
		"--user stop " + supervisorSystemdServiceName(),
		"--user disable " + supervisorSystemdServiceName(),
		"--user stop " + defaultSupervisorSystemdUnit,
		"--user disable " + defaultSupervisorSystemdUnit,
		"--user daemon-reload",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("systemctl calls = %v, want %q", calls, want)
		}
	}
}

func TestUninstallSupervisorSystemdIgnoresLegacyStopDisableFailures(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd path only applies on linux")
	}
	homeDir := t.TempDir()
	gcHome := filepath.Join(t.TempDir(), "isolated-home")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)

	currentPath := filepath.Join(homeDir, ".local", "share", "systemd", "user", supervisorSystemdServiceName())
	legacyPath := legacySupervisorSystemdServicePath()
	for _, path := range []string{currentPath, legacyPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("Environment=GC_HOME=\""+gcHome+"\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	oldRun := supervisorSystemctlRun
	supervisorSystemctlRun = func(args ...string) error {
		if len(args) >= 3 && args[1] == "stop" && args[2] == defaultSupervisorSystemdUnit {
			return errors.New("legacy stop failed")
		}
		if len(args) >= 3 && args[1] == "disable" && args[2] == defaultSupervisorSystemdUnit {
			return errors.New("legacy disable failed")
		}
		return nil
	}
	t.Cleanup(func() {
		supervisorSystemctlRun = oldRun
	})

	var stdout, stderr bytes.Buffer
	if code := uninstallSupervisorSystemd(&supervisorServiceData{}, &stdout, &stderr); code != 0 {
		t.Fatalf("uninstallSupervisorSystemd code = %d, want 0; stderr=%q", code, stderr.String())
	}
	for _, path := range []string{currentPath, legacyPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("systemd unit %q should be removed despite legacy stop/disable failures; err=%v", path, err)
		}
	}
}

func TestUninstallSupervisorSystemdRefusesActiveServiceWithoutControlSocket(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd path only applies on linux")
	}
	homeDir := t.TempDir()
	gcHome := shortTempDir(t, "gc-home-")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	currentPath := filepath.Join(homeDir, ".local", "share", "systemd", "user", supervisorSystemdServiceName())
	if err := os.MkdirAll(filepath.Dir(currentPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(currentPath), err)
	}
	if err := os.WriteFile(currentPath, []byte("current unit\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", currentPath, err)
	}

	oldRun := supervisorSystemctlRun
	oldActive := supervisorSystemctlActive
	var calls []string
	supervisorSystemctlRun = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}
	supervisorSystemctlActive = func(service string) bool {
		return service == supervisorSystemdServiceName()
	}
	t.Cleanup(func() {
		supervisorSystemctlRun = oldRun
		supervisorSystemctlActive = oldActive
	})

	var stdout, stderr bytes.Buffer
	if code := uninstallSupervisorSystemd(&supervisorServiceData{}, &stdout, &stderr); code != 1 {
		t.Fatalf("uninstallSupervisorSystemd code = %d, want 1; stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(currentPath); err != nil {
		t.Fatalf("active systemd unit %q should remain after guarded uninstall; err=%v", currentPath, err)
	}
	if len(calls) != 0 {
		t.Fatalf("systemctl calls = %v, want none when active service has no control socket", calls)
	}
	for _, want := range []string{"control socket is unavailable", "gc supervisor start"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr = %q, want %q", stderr.String(), want)
		}
	}
}

func TestUninstallSupervisorSystemdUsesControlSocketWhenServiceActive(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd path only applies on linux")
	}
	homeDir := t.TempDir()
	gcHome := shortTempDir(t, "gc-home-")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	currentPath := filepath.Join(homeDir, ".local", "share", "systemd", "user", supervisorSystemdServiceName())
	if err := os.MkdirAll(filepath.Dir(currentPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(currentPath), err)
	}
	if err := os.WriteFile(currentPath, []byte("current unit\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", currentPath, err)
	}

	var (
		mu                         sync.Mutex
		socketStopSeen             bool
		stopped                    bool
		systemctlStopBeforeSocket  bool
		systemctlDisableCurrentHit bool
	)
	sockPath := supervisorSocketPath()
	startTestSupervisorSocket(t, sockPath, func(cmd string) string {
		mu.Lock()
		defer mu.Unlock()
		switch cmd {
		case "ping":
			if stopped {
				return ""
			}
			return "4242\n"
		case "stop":
			socketStopSeen = true
			stopped = true
			return "ok\ndone:ok\n"
		}
		return ""
	})

	oldRun := supervisorSystemctlRun
	oldActive := supervisorSystemctlActive
	supervisorSystemctlRun = func(args ...string) error {
		mu.Lock()
		defer mu.Unlock()
		if len(args) >= 3 && args[1] == "stop" && args[2] == supervisorSystemdServiceName() && !socketStopSeen {
			systemctlStopBeforeSocket = true
		}
		if len(args) >= 3 && args[1] == "disable" && args[2] == supervisorSystemdServiceName() {
			systemctlDisableCurrentHit = true
		}
		return nil
	}
	supervisorSystemctlActive = func(service string) bool {
		return service == supervisorSystemdServiceName()
	}
	t.Cleanup(func() {
		supervisorSystemctlRun = oldRun
		supervisorSystemctlActive = oldActive
	})

	var stdout, stderr bytes.Buffer
	if code := uninstallSupervisorSystemd(&supervisorServiceData{}, &stdout, &stderr); code != 0 {
		t.Fatalf("uninstallSupervisorSystemd code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(currentPath); !os.IsNotExist(err) {
		t.Fatalf("systemd unit %q should be removed; err=%v", currentPath, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if systemctlStopBeforeSocket {
		t.Fatal("uninstall stopped the systemd unit before requesting destructive socket shutdown")
	}
	if !socketStopSeen {
		t.Fatal("uninstall did not request shutdown through the supervisor control socket")
	}
	if !systemctlDisableCurrentHit {
		t.Fatal("uninstall did not disable the current systemd unit")
	}
}

func TestInstallSupervisorLaunchdRemovesMatchingLegacyDefaultPlistForIsolatedGCHome(t *testing.T) {
	homeDir := t.TempDir()
	gcHome := filepath.Join(t.TempDir(), "isolated-home")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)

	legacyPath := filepath.Join(homeDir, "Library", "LaunchAgents", defaultSupervisorLaunchdLabel+".plist")
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	legacyContent, err := renderSupervisorTemplate(supervisorLaunchdTemplate, &supervisorServiceData{
		GCPath:       "/tmp/gc-legacy",
		LogPath:      filepath.Join(gcHome, "supervisor.log"),
		GCHome:       gcHome,
		LaunchdLabel: defaultSupervisorLaunchdLabel,
		Path:         "/usr/local/bin:/usr/bin:/bin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte(legacyContent), 0o644); err != nil {
		t.Fatal(err)
	}

	data := &supervisorServiceData{
		GCPath:        "/tmp/gc-new",
		LogPath:       filepath.Join(gcHome, "supervisor.log"),
		GCHome:        gcHome,
		XDGRuntimeDir: "",
		LaunchdLabel:  supervisorLaunchdLabel(),
		Path:          "/usr/local/bin:/usr/bin:/bin",
	}

	oldRun := supervisorLaunchctlRun
	var calls []string
	supervisorLaunchctlRun = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}
	t.Cleanup(func() {
		supervisorLaunchctlRun = oldRun
	})

	var stdout, stderr bytes.Buffer
	if code := installSupervisorLaunchd(data, &stdout, &stderr); code != 0 {
		t.Fatalf("installSupervisorLaunchd code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("legacy launchd plist %q should be removed; err=%v", legacyPath, err)
	}
	joined := strings.Join(calls, "\n")
	currentPath := filepath.Join(homeDir, "Library", "LaunchAgents", supervisorLaunchdLabel()+".plist")
	for _, want := range []string{
		"unload " + legacyPath,
		"unload " + currentPath,
		"load " + currentPath,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("launchctl calls = %v, want %q", calls, want)
		}
	}
}

func TestInstallSupervisorLaunchdWritesPrivatePlist(t *testing.T) {
	homeDir := t.TempDir()
	gcHome := filepath.Join(t.TempDir(), "isolated-home")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)

	data := &supervisorServiceData{
		GCPath:       "/tmp/gc-new",
		LogPath:      filepath.Join(gcHome, "supervisor.log"),
		GCHome:       gcHome,
		LaunchdLabel: supervisorLaunchdLabel(),
		Path:         "/usr/local/bin:/usr/bin:/bin",
		ExtraEnv: []supervisorServiceEnvVar{
			{Name: "OPENAI_API_KEY", Value: "sk-openai-123"},
		},
	}

	oldRun := supervisorLaunchctlRun
	supervisorLaunchctlRun = func(_ ...string) error {
		return nil
	}
	t.Cleanup(func() {
		supervisorLaunchctlRun = oldRun
	})

	var stdout, stderr bytes.Buffer
	if code := installSupervisorLaunchd(data, &stdout, &stderr); code != 0 {
		t.Fatalf("installSupervisorLaunchd code = %d, want 0; stderr=%q", code, stderr.String())
	}
	path := supervisorLaunchdPlistPath()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q): %v", path, err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("launchd plist mode = %03o, want 600", got)
	}
}

func TestInstallSupervisorLaunchdEnablesAndKickstartsLoadedService(t *testing.T) {
	homeDir := t.TempDir()
	gcHome := filepath.Join(t.TempDir(), "isolated-home")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)

	label := supervisorLaunchdLabel()
	data := &supervisorServiceData{
		GCPath:       "/tmp/gc-new",
		LogPath:      filepath.Join(gcHome, "supervisor.log"),
		GCHome:       gcHome,
		LaunchdLabel: label,
		Path:         "/usr/local/bin:/usr/bin:/bin",
	}

	oldRun := supervisorLaunchctlRun
	var calls []string
	supervisorLaunchctlRun = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}
	t.Cleanup(func() {
		supervisorLaunchctlRun = oldRun
	})

	var stdout, stderr bytes.Buffer
	if code := installSupervisorLaunchd(data, &stdout, &stderr); code != 0 {
		t.Fatalf("installSupervisorLaunchd code = %d, want 0; stderr=%q", code, stderr.String())
	}

	path := supervisorLaunchdPlistPath()
	target := "gui/" + strconv.Itoa(os.Getuid()) + "/" + label
	wantSequence := []string{
		"unload " + path,
		"load " + path,
		"enable " + target,
		"kickstart -p " + target,
	}
	last := -1
	for _, want := range wantSequence {
		idx := slices.Index(calls[last+1:], want)
		if idx < 0 {
			t.Fatalf("launchctl calls = %v, want %q after index %d", calls, want, last)
		}
		last += idx + 1
	}
}

func TestInstallSupervisorLaunchdIgnoresLegacyUnloadFailures(t *testing.T) {
	homeDir := t.TempDir()
	gcHome := filepath.Join(t.TempDir(), "isolated-home")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)

	legacyPath := legacySupervisorLaunchdPlistPath()
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	legacyContent, err := renderSupervisorTemplate(supervisorLaunchdTemplate, &supervisorServiceData{
		GCPath:       "/tmp/gc-legacy",
		LogPath:      filepath.Join(gcHome, "supervisor.log"),
		GCHome:       gcHome,
		LaunchdLabel: defaultSupervisorLaunchdLabel,
		Path:         "/usr/local/bin:/usr/bin:/bin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte(legacyContent), 0o644); err != nil {
		t.Fatal(err)
	}

	data := &supervisorServiceData{
		GCPath:        "/tmp/gc-new",
		LogPath:       filepath.Join(gcHome, "supervisor.log"),
		GCHome:        gcHome,
		XDGRuntimeDir: "",
		LaunchdLabel:  supervisorLaunchdLabel(),
		Path:          "/usr/local/bin:/usr/bin:/bin",
	}

	oldRun := supervisorLaunchctlRun
	supervisorLaunchctlRun = func(args ...string) error {
		if len(args) == 2 && args[0] == "unload" && args[1] == legacyPath {
			return errors.New("legacy unload failed")
		}
		return nil
	}
	t.Cleanup(func() {
		supervisorLaunchctlRun = oldRun
	})

	var stdout, stderr bytes.Buffer
	if code := installSupervisorLaunchd(data, &stdout, &stderr); code != 0 {
		t.Fatalf("installSupervisorLaunchd code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("legacy launchd plist %q should be removed despite unload failures; err=%v", legacyPath, err)
	}
}

func TestInstallSupervisorLaunchdKeepsLegacyPlistWhenNewServiceFails(t *testing.T) {
	homeDir := t.TempDir()
	gcHome := filepath.Join(t.TempDir(), "isolated-home")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)

	legacyPath := legacySupervisorLaunchdPlistPath()
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	legacyContent, err := renderSupervisorTemplate(supervisorLaunchdTemplate, &supervisorServiceData{
		GCPath:       "/tmp/gc-legacy",
		LogPath:      filepath.Join(gcHome, "supervisor.log"),
		GCHome:       gcHome,
		LaunchdLabel: defaultSupervisorLaunchdLabel,
		Path:         "/usr/local/bin:/usr/bin:/bin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte(legacyContent), 0o644); err != nil {
		t.Fatal(err)
	}

	data := &supervisorServiceData{
		GCPath:        "/tmp/gc-new",
		LogPath:       filepath.Join(gcHome, "supervisor.log"),
		GCHome:        gcHome,
		XDGRuntimeDir: "",
		LaunchdLabel:  supervisorLaunchdLabel(),
		Path:          "/usr/local/bin:/usr/bin:/bin",
	}

	currentPath := filepath.Join(homeDir, "Library", "LaunchAgents", supervisorLaunchdLabel()+".plist")
	oldRun := supervisorLaunchctlRun
	var calls []string
	supervisorLaunchctlRun = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		if len(args) == 2 && args[0] == "load" && args[1] == currentPath {
			return errors.New("new plist failed to load")
		}
		return nil
	}
	t.Cleanup(func() {
		supervisorLaunchctlRun = oldRun
	})

	var stdout, stderr bytes.Buffer
	if code := installSupervisorLaunchd(data, &stdout, &stderr); code != 1 {
		t.Fatalf("installSupervisorLaunchd code = %d, want 1; stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(currentPath); !os.IsNotExist(err) {
		t.Fatalf("new launchd plist %q should be removed during rollback; err=%v", currentPath, err)
	}
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("legacy launchd plist %q should remain after failed install; err=%v", legacyPath, err)
	}
	joined := strings.Join(calls, "\n")
	for _, want := range []string{
		"unload " + legacyPath,
		"load " + currentPath,
		"load " + legacyPath,
		"enable gui/" + strconv.Itoa(os.Getuid()) + "/" + defaultSupervisorLaunchdLabel,
		"kickstart -p gui/" + strconv.Itoa(os.Getuid()) + "/" + defaultSupervisorLaunchdLabel,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("launchctl calls = %v, want %q", calls, want)
		}
	}
}

func TestInstallSupervisorLaunchdRestoresLegacyPlistWhenEnableFails(t *testing.T) {
	homeDir := t.TempDir()
	gcHome := filepath.Join(t.TempDir(), "isolated-home")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)

	legacyPath := legacySupervisorLaunchdPlistPath()
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	legacyContent, err := renderSupervisorTemplate(supervisorLaunchdTemplate, &supervisorServiceData{
		GCPath:       "/tmp/gc-legacy",
		LogPath:      filepath.Join(gcHome, "supervisor.log"),
		GCHome:       gcHome,
		LaunchdLabel: defaultSupervisorLaunchdLabel,
		Path:         "/usr/local/bin:/usr/bin:/bin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte(legacyContent), 0o644); err != nil {
		t.Fatal(err)
	}

	label := supervisorLaunchdLabel()
	data := &supervisorServiceData{
		GCPath:        "/tmp/gc-new",
		LogPath:       filepath.Join(gcHome, "supervisor.log"),
		GCHome:        gcHome,
		XDGRuntimeDir: "",
		LaunchdLabel:  label,
		Path:          "/usr/local/bin:/usr/bin:/bin",
	}

	currentPath := filepath.Join(homeDir, "Library", "LaunchAgents", label+".plist")
	currentTarget := "gui/" + strconv.Itoa(os.Getuid()) + "/" + label
	legacyTarget := "gui/" + strconv.Itoa(os.Getuid()) + "/" + defaultSupervisorLaunchdLabel
	oldRun := supervisorLaunchctlRun
	var calls []string
	supervisorLaunchctlRun = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		if len(args) == 2 && args[0] == "enable" && args[1] == currentTarget {
			return errors.New("new plist failed to enable")
		}
		if len(args) == 3 && args[0] == "kickstart" && args[2] == legacyTarget {
			return errors.New("legacy plist failed to restart")
		}
		return nil
	}
	t.Cleanup(func() {
		supervisorLaunchctlRun = oldRun
	})

	var stdout, stderr bytes.Buffer
	if code := installSupervisorLaunchd(data, &stdout, &stderr); code != 1 {
		t.Fatalf("installSupervisorLaunchd code = %d, want 1; stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(currentPath); !os.IsNotExist(err) {
		t.Fatalf("new launchd plist %q should be removed during rollback; err=%v", currentPath, err)
	}
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("legacy launchd plist %q should remain after failed install; err=%v", legacyPath, err)
	}
	if strings.Contains(stderr.String(), "rollback after launchctl failure") {
		t.Fatalf("stderr = %q, want rollback restart failure to be warning-only", stderr.String())
	}
	if !strings.Contains(stderr.String(), "warning: restoring launchd service: kickstart -p "+legacyTarget) {
		t.Fatalf("stderr = %q, want warning for best-effort legacy restart", stderr.String())
	}
	joined := strings.Join(calls, "\n")
	for _, want := range []string{
		"enable " + currentTarget,
		"load " + legacyPath,
		"enable " + legacyTarget,
		"kickstart -p " + legacyTarget,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("launchctl calls = %v, want %q", calls, want)
		}
	}
}

func TestInstallSupervisorLaunchdRestoresLegacyPlistWhenKickstartFails(t *testing.T) {
	homeDir := t.TempDir()
	gcHome := filepath.Join(t.TempDir(), "isolated-home")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)

	legacyPath := legacySupervisorLaunchdPlistPath()
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	legacyContent, err := renderSupervisorTemplate(supervisorLaunchdTemplate, &supervisorServiceData{
		GCPath:       "/tmp/gc-legacy",
		LogPath:      filepath.Join(gcHome, "supervisor.log"),
		GCHome:       gcHome,
		LaunchdLabel: defaultSupervisorLaunchdLabel,
		Path:         "/usr/local/bin:/usr/bin:/bin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte(legacyContent), 0o644); err != nil {
		t.Fatal(err)
	}

	label := supervisorLaunchdLabel()
	data := &supervisorServiceData{
		GCPath:        "/tmp/gc-new",
		LogPath:       filepath.Join(gcHome, "supervisor.log"),
		GCHome:        gcHome,
		XDGRuntimeDir: "",
		LaunchdLabel:  label,
		Path:          "/usr/local/bin:/usr/bin:/bin",
	}

	currentPath := filepath.Join(homeDir, "Library", "LaunchAgents", label+".plist")
	currentTarget := "gui/" + strconv.Itoa(os.Getuid()) + "/" + label
	oldRun := supervisorLaunchctlRun
	var calls []string
	supervisorLaunchctlRun = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		if len(args) == 3 && args[0] == "kickstart" && args[2] == currentTarget {
			return errors.New("new plist failed to start")
		}
		return nil
	}
	t.Cleanup(func() {
		supervisorLaunchctlRun = oldRun
	})

	var stdout, stderr bytes.Buffer
	if code := installSupervisorLaunchd(data, &stdout, &stderr); code != 1 {
		t.Fatalf("installSupervisorLaunchd code = %d, want 1; stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(currentPath); !os.IsNotExist(err) {
		t.Fatalf("new launchd plist %q should be removed during rollback; err=%v", currentPath, err)
	}
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("legacy launchd plist %q should remain after failed install; err=%v", legacyPath, err)
	}
	joined := strings.Join(calls, "\n")
	for _, want := range []string{
		"kickstart -p " + currentTarget,
		"load " + legacyPath,
		"enable gui/" + strconv.Itoa(os.Getuid()) + "/" + defaultSupervisorLaunchdLabel,
		"kickstart -p gui/" + strconv.Itoa(os.Getuid()) + "/" + defaultSupervisorLaunchdLabel,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("launchctl calls = %v, want %q", calls, want)
		}
	}
}

func TestInstallSupervisorLaunchdRestoresPreviousCurrentPlistWhenUpdateFails(t *testing.T) {
	homeDir := t.TempDir()
	gcHome := filepath.Join(t.TempDir(), "isolated-home")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)

	currentPath := filepath.Join(homeDir, "Library", "LaunchAgents", supervisorLaunchdLabel()+".plist")
	if err := os.MkdirAll(filepath.Dir(currentPath), 0o755); err != nil {
		t.Fatal(err)
	}
	oldContent := []byte("old isolated plist\n")
	if err := os.WriteFile(currentPath, oldContent, 0o644); err != nil {
		t.Fatal(err)
	}

	data := &supervisorServiceData{
		GCPath:        "/tmp/gc-new",
		LogPath:       filepath.Join(gcHome, "supervisor.log"),
		GCHome:        gcHome,
		XDGRuntimeDir: "",
		LaunchdLabel:  supervisorLaunchdLabel(),
		Path:          "/usr/local/bin:/usr/bin:/bin",
	}

	label := supervisorLaunchdLabel()
	target := "gui/" + strconv.Itoa(os.Getuid()) + "/" + label
	oldRun := supervisorLaunchctlRun
	var calls []string
	loadCalls := 0
	supervisorLaunchctlRun = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		if len(args) == 2 && args[0] == "load" && args[1] == currentPath {
			loadCalls++
			if loadCalls == 1 {
				return errors.New("new plist failed to load")
			}
		}
		return nil
	}
	t.Cleanup(func() {
		supervisorLaunchctlRun = oldRun
	})

	var stdout, stderr bytes.Buffer
	if code := installSupervisorLaunchd(data, &stdout, &stderr); code != 1 {
		t.Fatalf("installSupervisorLaunchd code = %d, want 1; stderr=%q", code, stderr.String())
	}
	gotContent, err := os.ReadFile(currentPath)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", currentPath, err)
	}
	if !bytes.Equal(gotContent, oldContent) {
		t.Fatalf("restored launchd plist = %q, want original %q", gotContent, oldContent)
	}
	info, err := os.Stat(currentPath)
	if err != nil {
		t.Fatalf("Stat(%q): %v", currentPath, err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("restored launchd plist mode = %03o, want 600", got)
	}
	if loadCalls != 2 {
		t.Fatalf("launchctl load call count = %d, want 2 (failed install + rollback restore); calls=%v", loadCalls, calls)
	}
	joined := strings.Join(calls, "\n")
	for _, want := range []string{
		"unload " + currentPath,
		"load " + currentPath,
		"enable " + target,
		"kickstart -p " + target,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("launchctl calls = %v, want %q", calls, want)
		}
	}
}

func TestUninstallSupervisorLaunchdRemovesMatchingLegacyDefaultPlistForIsolatedGCHome(t *testing.T) {
	homeDir := t.TempDir()
	gcHome := filepath.Join(t.TempDir(), "isolated-home")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)

	currentPath := filepath.Join(homeDir, "Library", "LaunchAgents", supervisorLaunchdLabel()+".plist")
	legacyPath := filepath.Join(homeDir, "Library", "LaunchAgents", defaultSupervisorLaunchdLabel+".plist")
	for _, path := range []string{currentPath, legacyPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		label := supervisorLaunchdLabel()
		if path == legacyPath {
			label = defaultSupervisorLaunchdLabel
		}
		content, err := renderSupervisorTemplate(supervisorLaunchdTemplate, &supervisorServiceData{
			GCPath:       "/tmp/gc-legacy",
			LogPath:      filepath.Join(gcHome, "supervisor.log"),
			GCHome:       gcHome,
			LaunchdLabel: label,
			Path:         "/usr/local/bin:/usr/bin:/bin",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	oldRun := supervisorLaunchctlRun
	var calls []string
	supervisorLaunchctlRun = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}
	t.Cleanup(func() {
		supervisorLaunchctlRun = oldRun
	})

	var stdout, stderr bytes.Buffer
	if code := uninstallSupervisorLaunchd(&supervisorServiceData{}, &stdout, &stderr); code != 0 {
		t.Fatalf("uninstallSupervisorLaunchd code = %d, want 0; stderr=%q", code, stderr.String())
	}
	for _, path := range []string{currentPath, legacyPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("launchd plist %q should be removed; err=%v", path, err)
		}
	}
	joined := strings.Join(calls, "\n")
	for _, want := range []string{
		"unload " + currentPath,
		"disable gui/" + strconv.Itoa(os.Getuid()) + "/" + supervisorLaunchdLabel(),
		"unload " + legacyPath,
		"disable gui/" + strconv.Itoa(os.Getuid()) + "/" + defaultSupervisorLaunchdLabel,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("launchctl calls = %v, want %q", calls, want)
		}
	}
}

func TestUninstallSupervisorLaunchdUsesControlSocketWhenSupervisorRunning(t *testing.T) {
	homeDir := t.TempDir()
	gcHome := shortTempDir(t, "gc-home-")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	currentPath := filepath.Join(homeDir, "Library", "LaunchAgents", supervisorLaunchdLabel()+".plist")
	if err := os.MkdirAll(filepath.Dir(currentPath), 0o755); err != nil {
		t.Fatal(err)
	}
	content, err := renderSupervisorTemplate(supervisorLaunchdTemplate, &supervisorServiceData{
		GCPath:       "/tmp/gc-current",
		LogPath:      filepath.Join(gcHome, "supervisor.log"),
		GCHome:       gcHome,
		LaunchdLabel: supervisorLaunchdLabel(),
		Path:         "/usr/local/bin:/usr/bin:/bin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(currentPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	var (
		mu                 sync.Mutex
		socketStopSeen     bool
		stopped            bool
		unloadBeforeSocket bool
		launchdDisableSeen bool
	)
	sockPath := supervisorSocketPath()
	startTestSupervisorSocket(t, sockPath, func(cmd string) string {
		mu.Lock()
		defer mu.Unlock()
		switch cmd {
		case "ping":
			if stopped {
				return ""
			}
			return "4242\n"
		case "stop":
			socketStopSeen = true
			stopped = true
			return "ok\ndone:ok\n"
		}
		return ""
	})

	oldRun := supervisorLaunchctlRun
	supervisorLaunchctlRun = func(args ...string) error {
		mu.Lock()
		defer mu.Unlock()
		if len(args) == 2 && args[0] == "unload" && args[1] == currentPath && !socketStopSeen {
			unloadBeforeSocket = true
		}
		if len(args) == 2 && args[0] == "disable" && args[1] == supervisorLaunchdServiceTarget(supervisorLaunchdLabel()) {
			launchdDisableSeen = true
		}
		return nil
	}
	t.Cleanup(func() {
		supervisorLaunchctlRun = oldRun
	})

	var stdout, stderr bytes.Buffer
	if code := uninstallSupervisorLaunchd(&supervisorServiceData{}, &stdout, &stderr); code != 0 {
		t.Fatalf("uninstallSupervisorLaunchd code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(currentPath); !os.IsNotExist(err) {
		t.Fatalf("launchd plist %q should be removed; err=%v", currentPath, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if unloadBeforeSocket {
		t.Fatal("launchd uninstall unloaded the service before requesting destructive socket shutdown")
	}
	if !socketStopSeen {
		t.Fatal("launchd uninstall did not request shutdown through the supervisor control socket")
	}
	if !launchdDisableSeen {
		t.Fatal("launchd uninstall did not disable the current launchd service")
	}
}

func TestUninstallSupervisorLaunchdRefusesActiveServiceWithoutControlSocket(t *testing.T) {
	homeDir := t.TempDir()
	gcHome := shortTempDir(t, "gc-home-")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	currentPath := filepath.Join(homeDir, "Library", "LaunchAgents", supervisorLaunchdLabel()+".plist")
	if err := os.MkdirAll(filepath.Dir(currentPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(currentPath, []byte("current plist\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	oldRun := supervisorLaunchctlRun
	oldActive := supervisorLaunchdActive
	var calls []string
	supervisorLaunchctlRun = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}
	supervisorLaunchdActive = func(label string) bool {
		return label == supervisorLaunchdLabel()
	}
	t.Cleanup(func() {
		supervisorLaunchctlRun = oldRun
		supervisorLaunchdActive = oldActive
	})

	var stdout, stderr bytes.Buffer
	if code := uninstallSupervisorLaunchd(&supervisorServiceData{}, &stdout, &stderr); code != 1 {
		t.Fatalf("uninstallSupervisorLaunchd code = %d, want 1; stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(currentPath); err != nil {
		t.Fatalf("active launchd plist %q should remain after guarded uninstall; err=%v", currentPath, err)
	}
	if len(calls) != 0 {
		t.Fatalf("launchctl calls = %v, want none when active service has no control socket", calls)
	}
	for _, want := range []string{"launchd service", "control socket is unavailable", "gc supervisor start"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr = %q, want %q", stderr.String(), want)
		}
	}
}

func TestUninstallSupervisorLaunchdIgnoresLegacyUnloadFailures(t *testing.T) {
	homeDir := t.TempDir()
	gcHome := filepath.Join(t.TempDir(), "isolated-home")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)

	currentPath := filepath.Join(homeDir, "Library", "LaunchAgents", supervisorLaunchdLabel()+".plist")
	legacyPath := legacySupervisorLaunchdPlistPath()
	for _, path := range []string{currentPath, legacyPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		label := supervisorLaunchdLabel()
		if path == legacyPath {
			label = defaultSupervisorLaunchdLabel
		}
		content, err := renderSupervisorTemplate(supervisorLaunchdTemplate, &supervisorServiceData{
			GCPath:       "/tmp/gc-legacy",
			LogPath:      filepath.Join(gcHome, "supervisor.log"),
			GCHome:       gcHome,
			LaunchdLabel: label,
			Path:         "/usr/local/bin:/usr/bin:/bin",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	oldRun := supervisorLaunchctlRun
	supervisorLaunchctlRun = func(args ...string) error {
		if len(args) == 2 && args[0] == "unload" && args[1] == legacyPath {
			return errors.New("legacy unload failed")
		}
		return nil
	}
	t.Cleanup(func() {
		supervisorLaunchctlRun = oldRun
	})

	var stdout, stderr bytes.Buffer
	if code := uninstallSupervisorLaunchd(&supervisorServiceData{}, &stdout, &stderr); code != 0 {
		t.Fatalf("uninstallSupervisorLaunchd code = %d, want 0; stderr=%q", code, stderr.String())
	}
	for _, path := range []string{currentPath, legacyPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("launchd plist %q should be removed despite legacy unload failures; err=%v", path, err)
		}
	}
}

func TestDoSupervisorStartRejectsHomeOverride(t *testing.T) {
	if goruntime.GOOS != "linux" && goruntime.GOOS != "darwin" {
		t.Skip("platform supervisor home override guard only applies on linux/darwin")
	}
	lookup, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil || strings.TrimSpace(lookup.HomeDir) == "" {
		t.Skip("user lookup home unavailable")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	var stdout, stderr bytes.Buffer
	if code := doSupervisorStart(&stdout, &stderr); code != 1 {
		t.Fatalf("doSupervisorStart code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "Keep HOME unchanged and use GC_HOME for isolated runs") {
		t.Fatalf("stderr = %q, want HOME override guidance", stderr.String())
	}
	if !strings.Contains(stderr.String(), lookup.HomeDir) {
		t.Fatalf("stderr = %q, want current home %q", stderr.String(), lookup.HomeDir)
	}
}

func TestDoSupervisorInstallRejectsHomeOverride(t *testing.T) {
	if goruntime.GOOS != "linux" && goruntime.GOOS != "darwin" {
		t.Skip("platform supervisor home override guard only applies on linux/darwin")
	}
	lookup, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil || strings.TrimSpace(lookup.HomeDir) == "" {
		t.Skip("user lookup home unavailable")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GC_HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	if code := doSupervisorInstall(&stdout, &stderr); code != 1 {
		t.Fatalf("doSupervisorInstall code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "Keep HOME unchanged and use GC_HOME for isolated runs") {
		t.Fatalf("stderr = %q, want HOME override guidance", stderr.String())
	}
	if !strings.Contains(stderr.String(), lookup.HomeDir) {
		t.Fatalf("stderr = %q, want current home %q", stderr.String(), lookup.HomeDir)
	}
}

func TestEnsureSupervisorRunningRejectsHomeOverride(t *testing.T) {
	if goruntime.GOOS != "linux" && goruntime.GOOS != "darwin" {
		t.Skip("platform supervisor home override guard only applies on linux/darwin")
	}
	lookup, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil || strings.TrimSpace(lookup.HomeDir) == "" {
		t.Skip("user lookup home unavailable")
	}
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	if code := ensureSupervisorRunning(&stdout, &stderr); code != 1 {
		t.Fatalf("ensureSupervisorRunning code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "Keep HOME unchanged and use GC_HOME for isolated runs") {
		t.Fatalf("stderr = %q, want HOME override guidance", stderr.String())
	}
	if !strings.Contains(stderr.String(), lookup.HomeDir) {
		t.Fatalf("stderr = %q, want current home %q", stderr.String(), lookup.HomeDir)
	}
}

func TestWaitForSupervisorReadyUsesHookedTimeout(t *testing.T) {
	oldAlive := supervisorAliveHook
	oldTimeout := supervisorReadyTimeout
	oldPoll := supervisorReadyPollInterval
	calls := 0
	supervisorAliveHook = func() int {
		calls++
		if calls < 4 {
			return 0
		}
		return 4242
	}
	supervisorReadyTimeout = 25 * time.Millisecond
	supervisorReadyPollInterval = time.Millisecond
	t.Cleanup(func() {
		supervisorAliveHook = oldAlive
		supervisorReadyTimeout = oldTimeout
		supervisorReadyPollInterval = oldPoll
	})

	var stderr bytes.Buffer
	if code := waitForSupervisorReady(&stderr); code != 0 {
		t.Fatalf("waitForSupervisorReady code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if calls < 4 {
		t.Fatalf("supervisorAliveHook called %d times, want at least 4", calls)
	}
}

func TestWaitForSupervisorReadySucceedsWhenAlreadyReadyEvenWithZeroTimeout(t *testing.T) {
	oldAlive := supervisorAliveHook
	oldTimeout := supervisorReadyTimeout
	oldPoll := supervisorReadyPollInterval
	supervisorAliveHook = func() int { return 4242 }
	supervisorReadyTimeout = 0
	supervisorReadyPollInterval = time.Millisecond
	t.Cleanup(func() {
		supervisorAliveHook = oldAlive
		supervisorReadyTimeout = oldTimeout
		supervisorReadyPollInterval = oldPoll
	})

	var stderr bytes.Buffer
	if code := waitForSupervisorReady(&stderr); code != 0 {
		t.Fatalf("waitForSupervisorReady code = %d, want 0; stderr=%q", code, stderr.String())
	}
}

func TestDoSupervisorStartAlreadyRunning(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	lock, err := acquireSupervisorLock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close() //nolint:errcheck // test cleanup

	var stdout, stderr bytes.Buffer
	code := doSupervisorStart(&stdout, &stderr)
	if code != 1 {
		t.Fatalf("doSupervisorStart code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "already running") {
		t.Fatalf("stderr = %q, want already running message", stderr.String())
	}
}

func TestDoSupervisorStartDetectsSupervisorOnFallbackSocket(t *testing.T) {
	gcHome := shortTempDir(t, "gc-home-")
	runtimeDir := shortTempDir(t, "gc-run-")
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	sockPath := filepath.Join(gcHome, "supervisor.sock")
	startTestSupervisorSocket(t, sockPath, func(cmd string) string {
		if cmd == "ping" {
			return "4242\n"
		}
		return ""
	})

	var stdout, stderr bytes.Buffer
	code := doSupervisorStart(&stdout, &stderr)
	if code != 1 {
		t.Fatalf("doSupervisorStart code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "already running") {
		t.Fatalf("stderr = %q, want already running message", stderr.String())
	}
}

func TestRunSupervisorRejectsSupervisorOnFallbackSocket(t *testing.T) {
	gcHome := shortTempDir(t, "gc-home-")
	runtimeDir := shortTempDir(t, "gc-run-")
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	sockPath := filepath.Join(gcHome, "supervisor.sock")
	startTestSupervisorSocket(t, sockPath, func(cmd string) string {
		if cmd == "ping" {
			return "4242\n"
		}
		return ""
	})

	var stdout, stderr bytes.Buffer
	code := runSupervisor(&stdout, &stderr)
	if code != 1 {
		t.Fatalf("runSupervisor code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "already running") {
		t.Fatalf("stderr = %q, want already running message", stderr.String())
	}
}

func TestRunSupervisorSIGTERMPreservesSessionsEndToEnd(t *testing.T) {
	gcHome := shortTempDir(t, "gc-home-")
	runtimeDir := shortTempDir(t, "gc-run-")
	t.Setenv("HOME", filepath.Dir(gcHome))
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("GC_BEADS", "file")
	t.Setenv(supervisorPreserveSessionsOnSignalEnv, "1")

	if err := os.WriteFile(supervisor.ConfigPath(), []byte("[supervisor]\nport = "+freeLoopbackPort(t)+"\npatrol_interval = \"10m\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"bright-lights\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(cityPath, "bright-lights"); err != nil {
		t.Fatal(err)
	}

	sigChReady := make(chan chan<- os.Signal, 1)
	oldSignalNotify := supervisorSignalNotify
	supervisorSignalNotify = func(c chan<- os.Signal, _ ...os.Signal) {
		sigChReady <- c
	}
	t.Cleanup(func() {
		supervisorSignalNotify = oldSignalNotify
	})

	var stdout, stderr lockedBuffer
	done := make(chan int, 1)
	go func() {
		done <- runSupervisor(&stdout, &stderr)
	}()

	var sigCh chan<- os.Signal
	select {
	case sigCh = <-sigChReady:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for supervisor signal hook; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(stdout.String(), "Launching city 'bright-lights'") {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(stdout.String(), "Launching city 'bright-lights'") {
		t.Fatalf("timed out waiting for city launch; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	sigCh <- syscall.SIGTERM

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("runSupervisor code = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("runSupervisor did not exit after SIGTERM; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	got := stdout.String()
	for _, want := range []string{
		"Preserving city '" + cityPath + "' sessions for re-adoption...",
		"Preserving agent sessions for supervisor re-adoption.",
		"City '" + cityPath + "' preserved.",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stdout = %q, want %q; stderr=%q", got, want, stderr.String())
		}
	}
	if strings.Contains(got, "Stopping city 'bright-lights'") {
		t.Fatalf("stdout = %q, should use preserve-mode shutdown for SIGTERM", got)
	}
}

func TestRunSupervisorFailsWhenAPIPortUnavailable(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close() //nolint:errcheck

	port := lis.Addr().(*net.TCPAddr).Port
	cfg := []byte("[supervisor]\nport = " + strconv.Itoa(port) + "\n")
	if err := os.WriteFile(supervisor.ConfigPath(), cfg, 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runSupervisor(&stdout, &stderr)
	if code != 1 {
		t.Fatalf("runSupervisor code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "api: listen") {
		t.Fatalf("stderr = %q, want API listen failure", stderr.String())
	}
}

func TestControllerStatusForSupervisorManagedCity(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	dir := t.TempDir()
	cityPath := filepath.Join(dir, "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "bright-lights"); err != nil {
		t.Fatal(err)
	}

	oldAlive := supervisorAliveHook
	oldRunning := supervisorCityRunningHook
	supervisorAliveHook = func() int { return 4242 }
	supervisorCityRunningHook = func(string) (bool, string, bool) { return true, "", true }
	defer func() {
		supervisorAliveHook = oldAlive
		supervisorCityRunningHook = oldRunning
	}()

	ctrl := controllerStatusForCity(cityPath)
	if !ctrl.Running || ctrl.PID != 4242 || ctrl.Mode != "supervisor" {
		t.Fatalf("controller status = %+v, want running supervisor PID", ctrl)
	}
}

func TestSupervisorCityAPIClientRequiresRunning(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"bright-lights\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "bright-lights"); err != nil {
		t.Fatal(err)
	}

	oldAlive := supervisorAliveHook
	oldRunning := supervisorCityRunningHook
	supervisorAliveHook = func() int { return 4242 }
	supervisorCityRunningHook = func(string) (bool, string, bool) { return false, "", true }
	t.Cleanup(func() {
		supervisorAliveHook = oldAlive
		supervisorCityRunningHook = oldRunning
	})

	if client := supervisorCityAPIClient(cityPath); client != nil {
		t.Fatalf("supervisorCityAPIClient(%q) = %#v, want nil for stopped city", cityPath, client)
	}
}

func TestCityRegistryReportsRunningOnlyAfterStartup(t *testing.T) {
	cs := &controllerState{}
	mc := &managedCity{
		cr:     &CityRuntime{cityName: "bright-lights", cs: cs},
		name:   "bright-lights",
		status: "adopting_sessions",
	}
	reg := newCityRegistry()
	reg.Add("/city", mc)

	cities := reg.ListCities()
	if len(cities) != 1 || cities[0].Running {
		t.Fatalf("ListCities before startup = %+v, want one stopped city", cities)
	}
	if cities[0].Status != "adopting_sessions" {
		t.Fatalf("ListCities before startup Status = %q, want adopting_sessions", cities[0].Status)
	}
	if got := reg.CityState("bright-lights"); got != nil {
		t.Fatalf("CityState before startup = %#v, want nil", got)
	}

	reg.UpdateCallback("/city", func(m *managedCity) {
		m.started = true
	})
	cities = reg.ListCities()
	if len(cities) != 1 || !cities[0].Running {
		t.Fatalf("ListCities after startup = %+v, want one running city", cities)
	}
	if cities[0].Status != "" {
		t.Fatalf("ListCities after startup Status = %q, want empty", cities[0].Status)
	}
	if got := reg.CityState("bright-lights"); got != cs {
		t.Fatalf("CityState after startup = %#v, want controller state", got)
	}
}

func TestDeleteManagedCityIfCurrentKeepsReplacementCity(t *testing.T) {
	oldCity := &managedCity{name: "bright-lights"}
	newCity := &managedCity{name: "bright-lights"}
	cities := map[string]*managedCity{"/city": newCity}

	if deleted := deleteManagedCityIfCurrent(cities, "/city", oldCity); deleted {
		t.Fatal("deleteManagedCityIfCurrent returned true for stale city pointer")
	}
	if got := cities["/city"]; got != newCity {
		t.Fatalf("city at /city = %#v, want replacement city %#v", got, newCity)
	}
}

func TestDeleteManagedCityIfCurrentRemovesMatchingCity(t *testing.T) {
	current := &managedCity{name: "bright-lights"}
	cities := map[string]*managedCity{"/city": current}

	if deleted := deleteManagedCityIfCurrent(cities, "/city", current); !deleted {
		t.Fatal("deleteManagedCityIfCurrent returned false, want true")
	}
	if _, ok := cities["/city"]; ok {
		t.Fatalf("cities still contains /city after delete: %#v", cities["/city"])
	}
}

func TestControllerAliveNoSocket(t *testing.T) {
	dir := t.TempDir()
	gcDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := controllerAlive(dir); got != 0 {
		t.Fatalf("controllerAlive = %d, want 0", got)
	}
}

func TestStartHiddenLegacyFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newStartCmd(&stdout, &stderr)

	for _, name := range []string{"foreground", "controller", "file", "no-strict"} {
		flag := cmd.Flags().Lookup(name)
		if flag == nil {
			t.Fatalf("missing %s flag", name)
		}
		if !flag.Hidden {
			t.Fatalf("%s flag should be hidden", name)
		}
	}

	if flag := cmd.Flags().Lookup("dry-run"); flag == nil || flag.Hidden {
		t.Fatal("dry-run flag should remain visible")
	}
}

func TestDoStartRequiresInitializedCity(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doStart([]string{dir}, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doStart code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "not in a city directory") {
		t.Fatalf("stderr = %q, want city-directory error", stderr.String())
	}
	if !strings.Contains(stderr.String(), `gc init `+dir) {
		t.Fatalf("stderr = %q, want init guidance", stderr.String())
	}
}

func TestDoStartRejectsUnbootstrappedCityConfig(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[workspace]\nname = \"bright-lights\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doStart([]string{dir}, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doStart code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "city runtime not bootstrapped") {
		t.Fatalf("stderr = %q, want bootstrap error", stderr.String())
	}
	if !strings.Contains(stderr.String(), `gc init `+dir) && !strings.Contains(stderr.String(), `gc init `+canonicalTestPath(dir)) {
		t.Fatalf("stderr = %q, want init guidance", stderr.String())
	}
}

func TestDoStartForegroundRejectsSupervisorManagedCity(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"bright-lights\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "bright-lights"); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doStart([]string{cityPath}, true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doStart code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "registered with the supervisor") {
		t.Fatalf("stderr = %q, want supervisor registration error", stderr.String())
	}
}

func TestDoStartRejectsStandaloneOnlyFlagsUnderSupervisor(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"bright-lights\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	oldExtraConfigFiles := extraConfigFiles
	oldNoStrictMode := noStrictMode
	extraConfigFiles = []string{"override.toml"}
	noStrictMode = true
	t.Cleanup(func() {
		extraConfigFiles = oldExtraConfigFiles
		noStrictMode = oldNoStrictMode
	})

	var stdout, stderr bytes.Buffer
	code := doStart([]string{cityPath}, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doStart code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "only apply to the legacy standalone controller") {
		t.Fatalf("stderr = %q, want standalone-flag rejection", stderr.String())
	}
}

func TestStopManagedCityForcesCleanupAfterTimeout(t *testing.T) {
	cityPath := t.TempDir()
	logFile := filepath.Join(t.TempDir(), "ops.log")
	script := writeSpyScript(t, logFile)
	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)

	closer := &closerSpy{}
	mc := &managedCity{
		name:   "bright-lights",
		cancel: func() {},
		done:   make(chan struct{}),
		closer: closer,
		cr: &CityRuntime{
			cfg: &config.City{
				Session: config.SessionConfig{StartupTimeout: "20ms"},
				Daemon: config.DaemonConfig{
					ShutdownTimeout:   "20ms",
					DriftDrainTimeout: "20ms",
				},
			},
			sp:     runtime.NewFake(),
			rec:    events.Discard,
			stdout: io.Discard,
			stderr: io.Discard,
		},
	}

	var stderr bytes.Buffer
	start := time.Now()
	err := stopManagedCity(mc, cityPath, &stderr)
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("stopManagedCity took %s, want bounded timeout", elapsed)
	}
	if err == nil {
		t.Fatal("stopManagedCity err = nil, want non-nil because city never exited")
	}
	if !strings.Contains(err.Error(), "did not exit") {
		t.Fatalf("stopManagedCity err = %q, want 'did not exit' detail", err.Error())
	}
	if !strings.Contains(stderr.String(), "did not exit within") {
		t.Fatalf("stderr = %q, want forced-timeout warning", stderr.String())
	}
	if !closer.closed {
		t.Fatal("expected closer to be closed after forced cleanup")
	}

	ops := readOpLog(t, logFile)
	assertSingleStopWithBenignNoise(t, ops)
}

func TestStopManagedCityDoesNotUseStartupOrDriftTimeouts(t *testing.T) {
	cityPath := t.TempDir()
	logFile := filepath.Join(t.TempDir(), "ops.log")
	script := writeSpyScript(t, logFile)
	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)

	closer := &closerSpy{}
	mc := &managedCity{
		name:   "bright-lights",
		cancel: func() {},
		done:   make(chan struct{}),
		closer: closer,
		cr: &CityRuntime{
			cfg: &config.City{
				Session: config.SessionConfig{StartupTimeout: "3m"},
				Daemon: config.DaemonConfig{
					ShutdownTimeout:   "20ms",
					DriftDrainTimeout: "2m",
				},
			},
			sp:     runtime.NewFake(),
			rec:    events.Discard,
			stdout: io.Discard,
			stderr: io.Discard,
		},
	}

	var stderr bytes.Buffer
	start := time.Now()
	err := stopManagedCity(mc, cityPath, &stderr)
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("stopManagedCity took %s, want shutdown-timeout bound", elapsed)
	}
	if err == nil {
		t.Fatal("stopManagedCity err = nil, want non-nil because city never exited")
	}
	if !strings.Contains(stderr.String(), "20ms") {
		t.Fatalf("stderr = %q, want shutdown-timeout warning", stderr.String())
	}
	if !closer.closed {
		t.Fatal("expected closer to be closed after forced cleanup")
	}

	ops := readOpLog(t, logFile)
	assertSingleStopWithBenignNoise(t, ops)
}

func TestCityRuntimeShutdownPreservesSessionsWhenRequested(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "agent-one", runtime.Config{}); err != nil {
		t.Fatalf("Start(agent-one): %v", err)
	}
	cr := &CityRuntime{
		cfg: &config.City{
			Daemon: config.DaemonConfig{ShutdownTimeout: "20ms"},
		},
		sp:     sp,
		rec:    events.Discard,
		stdout: io.Discard,
		stderr: io.Discard,
	}
	cr.preserveSessionsOnShutdown()

	cr.shutdown()

	running, err := sp.ListRunning("")
	if err != nil {
		t.Fatalf("ListRunning: %v", err)
	}
	if !slices.Contains(running, "agent-one") {
		t.Fatalf("running sessions = %v, want agent-one preserved", running)
	}
	for _, call := range sp.Calls {
		if call.Method == "Interrupt" || call.Method == "Stop" {
			t.Fatalf("preserve-mode shutdown called %s for %q; calls=%v", call.Method, call.Name, sp.Calls)
		}
	}
}

func TestCityRuntimeShutdownPreserveModeRecordsTrace(t *testing.T) {
	cityPath := t.TempDir()
	cr := &CityRuntime{
		cityPath: cityPath,
		cityName: "bright-lights",
		cfg: &config.City{
			Daemon: config.DaemonConfig{ShutdownTimeout: "20ms"},
		},
		sp:     runtime.NewFake(),
		rec:    events.Discard,
		stdout: io.Discard,
		stderr: io.Discard,
		trace:  newSessionReconcilerTraceManager(cityPath, "bright-lights", io.Discard),
	}
	cr.preserveSessionsOnShutdown()

	cr.shutdown()

	records, err := ReadTraceRecords(traceCityRuntimeDir(cityPath), TraceFilter{})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	if !slices.ContainsFunc(records, func(record SessionReconcilerTraceRecord) bool {
		return record.RecordType == TraceRecordCycleResult &&
			record.Fields["mode"] == "preserve_sessions" &&
			record.Fields["city_name"] == "bright-lights" &&
			record.Fields["reason"] == "supervisor_shutdown_preserve_mode"
	}) {
		t.Fatalf("trace records missing preserve shutdown cycle result: %#v", records)
	}
}

func TestStopManagedCityPreservingSessionsSkipsBeadsProviderShutdown(t *testing.T) {
	cityPath := t.TempDir()
	logFile := filepath.Join(t.TempDir(), "ops.log")
	script := writeSpyScript(t, logFile)
	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)

	closer := &closerSpy{}
	done := make(chan struct{})
	canceled := false
	mc := &managedCity{
		name: "bright-lights",
		cancel: func() {
			canceled = true
			close(done)
		},
		done:   done,
		closer: closer,
		cr: &CityRuntime{
			cfg: &config.City{
				Daemon: config.DaemonConfig{ShutdownTimeout: "20ms"},
			},
			sp:     runtime.NewFake(),
			rec:    events.Discard,
			stdout: io.Discard,
			stderr: io.Discard,
		},
	}

	if err := stopManagedCityPreservingSessions(mc, cityPath, io.Discard); err != nil {
		t.Fatalf("stopManagedCityPreservingSessions: %v", err)
	}
	if !canceled {
		t.Fatal("expected city context to be canceled so the CityRuntime goroutine can exit")
	}
	if !closer.closed {
		t.Fatal("expected recorder closer to be closed after preserve-mode teardown")
	}
	if ops := readOpLog(t, logFile); len(ops) != 0 {
		t.Fatalf("beads provider ops = %v, want none in preserve mode", ops)
	}
}

func TestStopManagedCityPreservingSessionsWaitsForRuntimeShutdownOnTimeout(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("proxy process service shutdown uses process groups on linux")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not in PATH")
	}
	cityPath := t.TempDir()
	serviceScript := filepath.Join(t.TempDir(), "service.sh")
	if err := os.WriteFile(serviceScript, []byte(`#!/usr/bin/env python3
import os
import signal
import socket
import sys

sock_path = os.environ["GC_SERVICE_SOCKET"]
try:
    os.unlink(sock_path)
except FileNotFoundError:
    pass

def stop(_signum, _frame):
    sys.exit(0)

signal.signal(signal.SIGTERM, stop)
listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
listener.bind(sock_path)
listener.listen(1)
while True:
    conn, _ = listener.accept()
    conn.close()
`), 0o755); err != nil {
		t.Fatalf("WriteFile(service script): %v", err)
	}
	var runtimeStdout bytes.Buffer
	cr := &CityRuntime{
		cityPath: cityPath,
		cityName: "bright-lights",
		cfg: &config.City{
			Daemon: config.DaemonConfig{ShutdownTimeout: "20ms"},
			Services: []config.Service{{
				Name: "bridge",
				Kind: "proxy_process",
				Process: config.ServiceProcessConfig{
					Command: []string{serviceScript},
				},
			}},
		},
		sp:     runtime.NewFake(),
		rec:    events.Discard,
		stdout: &runtimeStdout,
		stderr: io.Discard,
	}
	cr.svc = workspacesvc.NewManager(&serviceRuntime{cr: cr})
	if err := cr.svc.Reload(); err != nil {
		t.Fatalf("service Reload: %v", err)
	}
	status, ok := cr.svc.Get("bridge")
	if !ok {
		t.Fatal("service bridge missing after Reload")
	}
	if status.LocalState != "ready" {
		t.Fatalf("service bridge local_state = %q, want ready; status=%#v", status.LocalState, status)
	}

	mc := &managedCity{
		name:   "bright-lights",
		cancel: func() {},
		done:   make(chan struct{}),
		cr:     cr,
	}

	err := stopManagedCityPreservingSessions(mc, cityPath, io.Discard)
	if err == nil {
		t.Fatal("stopManagedCityPreservingSessions error = nil, want timeout error")
	}
	status, ok = cr.svc.Get("bridge")
	if !ok {
		t.Fatal("service bridge missing after preserve-mode shutdown wait")
	}
	if status.LocalState != "stopped" {
		t.Fatalf("service bridge local_state = %q, want stopped after preserve-mode shutdown wait; status=%#v", status.LocalState, status)
	}
	if !strings.Contains(runtimeStdout.String(), "Preserving agent sessions for supervisor re-adoption.") {
		t.Fatalf("runtime stdout = %q, want preserve-mode shutdown message", runtimeStdout.String())
	}
}

func TestShutdownSupervisorCitiesPreserveSessions(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "agent-one", runtime.Config{}); err != nil {
		t.Fatalf("Start(agent-one): %v", err)
	}
	done := make(chan struct{})
	mc := &managedCity{
		name: "bright-lights",
		cancel: func() {
			close(done)
		},
		done: done,
		cr: &CityRuntime{
			cfg: &config.City{Daemon: config.DaemonConfig{ShutdownTimeout: "20ms"}},
			sp:  sp, rec: events.Discard, stdout: io.Discard, stderr: io.Discard,
		},
	}
	if err := stopManagedCityPreservingSessions(mc, t.TempDir(), io.Discard); err != nil {
		t.Fatalf("stopManagedCityPreservingSessions: %v", err)
	}
	mc.cr.shutdown()
	running, err := sp.ListRunning("")
	if err != nil {
		t.Fatalf("ListRunning: %v", err)
	}
	if !slices.Contains(running, "agent-one") {
		t.Fatalf("running sessions = %v, want agent-one preserved", running)
	}
}

func TestSupervisorShutdownControllerDestructiveRequestIsSticky(t *testing.T) {
	tests := []struct {
		name     string
		requests []supervisorShutdownMode
		want     bool
	}{
		{name: "no request", want: false},
		{name: "preserve only", requests: []supervisorShutdownMode{supervisorShutdownPreserveSessions}, want: true},
		{name: "destructive only", requests: []supervisorShutdownMode{supervisorShutdownDestructive}, want: false},
		{name: "destructive then preserve", requests: []supervisorShutdownMode{supervisorShutdownDestructive, supervisorShutdownPreserveSessions}, want: false},
		{name: "preserve then destructive", requests: []supervisorShutdownMode{supervisorShutdownPreserveSessions, supervisorShutdownDestructive}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctl := newSupervisorShutdownController()
			for _, req := range tt.requests {
				ctl.request(req)
			}
			if got := ctl.preservesSessions(); got != tt.want {
				t.Fatalf("preservesSessions() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSupervisorShutdownControllerSettlesLateDestructiveRequest(t *testing.T) {
	ctl := newSupervisorShutdownController()
	ctl.request(supervisorShutdownPreserveSessions)

	go func() {
		time.Sleep(10 * time.Millisecond)
		ctl.request(supervisorShutdownDestructive)
	}()

	if got := ctl.preservesSessionsAfterSettle(200 * time.Millisecond); got {
		t.Fatal("preservesSessionsAfterSettle() = true, want false after late destructive request")
	}
}

func TestSupervisorSignalLoopKeepsLateDestructiveEscalationUntilShutdownDone(t *testing.T) {
	t.Setenv(supervisorPreserveSessionsOnSignalEnv, "1")
	sigCh := make(chan os.Signal, 2)
	done := make(chan struct{})
	shutdownStarted := make(chan struct{})
	var shutdownStartedOnce sync.Once
	ctl := newSupervisorShutdownController()

	go supervisorSignalLoop(sigCh, done, func(mode supervisorShutdownMode) {
		ctl.request(mode)
		shutdownStartedOnce.Do(func() { close(shutdownStarted) })
	}, func() {})

	sigCh <- syscall.SIGTERM
	select {
	case <-shutdownStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for preserve shutdown request")
	}
	sigCh <- syscall.SIGINT
	defer close(done)

	if got := ctl.preservesSessionsAfterSettle(200 * time.Millisecond); got {
		t.Fatal("preservesSessionsAfterSettle() = true, want false after late SIGINT escalation")
	}
}

func TestSupervisorShutdownModeForSignalPreservesOnlySIGTERMWhenConfigured(t *testing.T) {
	t.Setenv(supervisorPreserveSessionsOnSignalEnv, "1")
	if got := supervisorShutdownModeForSignal(syscall.SIGTERM); got != supervisorShutdownPreserveSessions {
		t.Fatalf("SIGTERM shutdown mode = %v, want preserve", got)
	}
	if got := supervisorShutdownModeForSignal(syscall.SIGINT); got != supervisorShutdownDestructive {
		t.Fatalf("SIGINT shutdown mode = %v, want destructive", got)
	}
}

func TestStopSupervisorWithWaitStopsSystemdServiceAfterAckBeforeDone(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd path only applies on linux")
	}
	gcHome := shortTempDir(t, "gc-home-")
	runtimeDir := shortTempDir(t, "gc-run-")
	t.Setenv("HOME", filepath.Dir(gcHome))
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	unitPath := supervisorSystemdServicePath()
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(unitPath), err)
	}
	if err := os.WriteFile(unitPath, []byte("unit\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", unitPath, err)
	}

	var (
		mu                    sync.Mutex
		stopped               bool
		serviceStopBeforeAck  bool
		doneSentBeforeService bool
		serviceStopSeen       bool
		serviceStopOnce       sync.Once
	)
	ackSent := make(chan struct{})
	serviceStopped := make(chan struct{})
	oldRun := supervisorSystemctlRun
	supervisorSystemctlRun = func(args ...string) error {
		mu.Lock()
		if len(args) >= 3 && args[1] == "stop" && args[2] == supervisorSystemdServiceName() {
			select {
			case <-ackSent:
			default:
				serviceStopBeforeAck = true
			}
			serviceStopSeen = true
			serviceStopOnce.Do(func() { close(serviceStopped) })
		}
		mu.Unlock()
		return nil
	}
	t.Cleanup(func() {
		supervisorSystemctlRun = oldRun
	})

	sockPath := supervisorSocketPath()
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(sockPath), err)
	}
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Listen(unix, %q): %v", sockPath, err)
	}
	t.Cleanup(func() {
		lis.Close()         //nolint:errcheck
		os.Remove(sockPath) //nolint:errcheck
	})
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close() //nolint:errcheck
				r := bufio.NewReader(conn)
				line, err := r.ReadString('\n')
				if err != nil {
					return
				}
				switch strings.TrimSpace(line) {
				case "ping":
					mu.Lock()
					defer mu.Unlock()
					if stopped {
						return
					}
					io.WriteString(conn, "4242\n") //nolint:errcheck
				case "stop":
					mu.Lock()
					stopped = true
					mu.Unlock()
					io.WriteString(conn, "ok\n") //nolint:errcheck
					close(ackSent)
					select {
					case <-serviceStopped:
					case <-time.After(200 * time.Millisecond):
						mu.Lock()
						doneSentBeforeService = true
						mu.Unlock()
					}
					io.WriteString(conn, "done:ok\n") //nolint:errcheck
				}
			}(conn)
		}
	}()

	var stdout, stderr bytes.Buffer
	if code := stopSupervisorWithWait(&stdout, &stderr, true, time.Second); code != 0 {
		t.Fatalf("stopSupervisorWithWait code = %d, want 0; stderr=%q", code, stderr.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if serviceStopBeforeAck {
		t.Fatal("platform service was stopped before the supervisor acknowledged the destructive socket stop")
	}
	if doneSentBeforeService {
		t.Fatal("supervisor reported done:ok before systemd stop was requested")
	}
	if !serviceStopSeen {
		t.Fatal("systemd service was not stopped after the supervisor acknowledged the destructive socket stop")
	}
}

// TestStopSupervisorWithWaitBlocksUntilSocketStops exercises the --wait
// path of `gc supervisor stop`. The fake socket answers "ping" with a PID
// (so supervisorAliveAtPath keeps returning alive) for ~200ms after the
// "stop" request, then closes the listener. stopSupervisorWithWait must
// block across that window and return success.
func TestStopSupervisorWithWaitBlocksUntilSocketStops(t *testing.T) {
	gcHome := shortTempDir(t, "gc-home-")
	runtimeDir := shortTempDir(t, "gc-run-")
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	sockPath := filepath.Join(gcHome, "supervisor.sock")
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() {
		lis.Close()         //nolint:errcheck
		os.Remove(sockPath) //nolint:errcheck
	})

	stopDelay := 200 * time.Millisecond
	// stopRequested/stopAt are touched by the "stop" handler goroutine and
	// read concurrently by every "ping" handler goroutine. Guard with a
	// mutex so `go test -race` doesn't flag this fake server.
	var (
		mu            sync.Mutex
		stopRequested bool
		stopAt        time.Time
	)
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close() //nolint:errcheck
				buf := make([]byte, 64)
				n, err := conn.Read(buf)
				if err != nil || n == 0 {
					return
				}
				cmd := strings.TrimSpace(string(buf[:n]))
				switch cmd {
				case "ping":
					mu.Lock()
					finished := stopRequested && time.Now().After(stopAt)
					mu.Unlock()
					if finished {
						// Stop answering ping so the waiter sees us as gone.
						return
					}
					io.WriteString(conn, "4242\n") //nolint:errcheck
				case "stop":
					mu.Lock()
					stopRequested = true
					stopAt = time.Now().Add(stopDelay)
					mu.Unlock()
					io.WriteString(conn, "ok\n") //nolint:errcheck
					// New protocol: --wait clients also read a final
					// status line. Emit done:ok after the stop delay so
					// this test exercises the happy path of the new
					// protocol in addition to the socket-close fallback.
					time.Sleep(stopDelay)
					io.WriteString(conn, "done:ok\n") //nolint:errcheck
				}
			}(conn)
		}
	}()

	start := time.Now()
	var stdout, stderr bytes.Buffer
	code := stopSupervisorWithWait(&stdout, &stderr, true, 5*time.Second)
	elapsed := time.Since(start)

	if code != 0 {
		t.Fatalf("stopSupervisorWithWait code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if elapsed < stopDelay {
		t.Fatalf("returned after %s, expected at least %s (must have waited for socket to stop answering)", elapsed, stopDelay)
	}
	if !strings.Contains(stdout.String(), "Supervisor stopped.") {
		t.Fatalf("stdout = %q, want final confirmation message", stdout.String())
	}
}

// TestStopSupervisorWithoutWaitReturnsAfterAck confirms the default
// (non-wait) path returns as soon as the supervisor ACKs the stop. The
// fake socket keeps answering "ping" indefinitely; without --wait,
// stopSupervisor must not block on the ping result.
func TestStopSupervisorWithoutWaitReturnsAfterAck(t *testing.T) {
	gcHome := shortTempDir(t, "gc-home-")
	runtimeDir := shortTempDir(t, "gc-run-")
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	sockPath := filepath.Join(gcHome, "supervisor.sock")
	startTestSupervisorSocket(t, sockPath, func(cmd string) string {
		switch cmd {
		case "ping":
			return "4242\n"
		case "stop":
			return "ok\n"
		}
		return ""
	})

	start := time.Now()
	var stdout, stderr bytes.Buffer
	code := stopSupervisor(&stdout, &stderr)
	elapsed := time.Since(start)

	if code != 0 {
		t.Fatalf("stopSupervisor code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if elapsed > 2*time.Second {
		t.Fatalf("returned after %s, expected fast return (no wait) — waited anyway?", elapsed)
	}
	if !strings.Contains(stdout.String(), "Supervisor stopping...") {
		t.Fatalf("stdout = %q, want 'Supervisor stopping...' message", stdout.String())
	}
	if strings.Contains(stdout.String(), "Supervisor stopped.") {
		t.Fatalf("stdout unexpectedly contains 'Supervisor stopped.' — wait flag was false")
	}
}

// TestStopSupervisorWithWaitPropagatesDoneErr exercises the new
// post-shutdown status protocol: the server sends "ok\n" to ack the
// stop request, then "done:err:<detail>\n" when shutdown finished with
// errors (e.g., a managed city failed to quiesce). --wait must surface
// the error to stderr and exit non-zero so test cleanup sees the flake
// instead of believing shutdown was clean.
func TestStopSupervisorWithWaitPropagatesDoneErr(t *testing.T) {
	gcHome := shortTempDir(t, "gc-home-")
	runtimeDir := shortTempDir(t, "gc-run-")
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	sockPath := filepath.Join(gcHome, "supervisor.sock")
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() {
		lis.Close()         //nolint:errcheck
		os.Remove(sockPath) //nolint:errcheck
	})

	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close() //nolint:errcheck
				r := bufio.NewReader(conn)
				line, err := r.ReadString('\n')
				if err != nil {
					return
				}
				switch strings.TrimSpace(line) {
				case "ping":
					io.WriteString(conn, "4242\n") //nolint:errcheck
				case "stop":
					io.WriteString(conn, "ok\n")                                             //nolint:errcheck
					io.WriteString(conn, "done:err:city \"alpha\" did not exit within 5s\n") //nolint:errcheck
				}
			}(conn)
		}
	}()

	var stdout, stderr bytes.Buffer
	code := stopSupervisorWithWait(&stdout, &stderr, true, 2*time.Second)

	if code != 1 {
		t.Fatalf("stopSupervisorWithWait code = %d, want 1 (propagated done:err); stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "alpha") || !strings.Contains(stderr.String(), "did not exit") {
		t.Fatalf("stderr = %q, want it to include the server's done:err detail", stderr.String())
	}
	if strings.Contains(stdout.String(), "Supervisor stopped.") {
		t.Fatalf("stdout unexpectedly contains 'Supervisor stopped.' — shutdown reported errors")
	}
}

// TestStopSupervisorWithWaitTimesOutWhenSocketKeepsAnswering guards the
// wait-timeout path. The fake socket keeps answering ping forever; --wait
// with a tiny timeout must return non-zero and mention the timeout.
func TestStopSupervisorWithWaitTimesOutWhenSocketKeepsAnswering(t *testing.T) {
	gcHome := shortTempDir(t, "gc-home-")
	runtimeDir := shortTempDir(t, "gc-run-")
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	sockPath := filepath.Join(gcHome, "supervisor.sock")
	startTestSupervisorSocket(t, sockPath, func(cmd string) string {
		switch cmd {
		case "ping":
			return "4242\n"
		case "stop":
			return "ok\n"
		}
		return ""
	})

	var stdout, stderr bytes.Buffer
	code := stopSupervisorWithWait(&stdout, &stderr, true, 300*time.Millisecond)

	if code != 1 {
		t.Fatalf("stopSupervisorWithWait code = %d, want 1 (timeout)", code)
	}
	if !strings.Contains(stderr.String(), "timed out") {
		t.Fatalf("stderr = %q, want timeout message", stderr.String())
	}
}

func TestResolveStableSupervisorBinaryPath(t *testing.T) {
	newRunningExe := func(t *testing.T) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "gc")
		if err := os.WriteFile(path, []byte{}, 0o755); err != nil {
			t.Fatalf("write running exe: %v", err)
		}
		return path
	}

	cases := []struct {
		name  string
		setup func(t *testing.T) (homeDir, gopath, currentExe, want string)
	}{
		{
			name: "local_bin_symlink_resolves_to_running_exe",
			setup: func(t *testing.T) (string, string, string, string) {
				running := newRunningExe(t)
				homeDir := t.TempDir()
				binDir := filepath.Join(homeDir, ".local", "bin")
				if err := os.MkdirAll(binDir, 0o755); err != nil {
					t.Fatalf("mkdir local bin: %v", err)
				}
				symlink := filepath.Join(binDir, "gc")
				if err := os.Symlink(running, symlink); err != nil {
					t.Fatalf("symlink: %v", err)
				}
				return homeDir, "", running, symlink
			},
		},
		{
			name: "local_bin_hardlink_matches_running_exe_inode",
			setup: func(t *testing.T) (string, string, string, string) {
				running := newRunningExe(t)
				homeDir := t.TempDir()
				binDir := filepath.Join(homeDir, ".local", "bin")
				if err := os.MkdirAll(binDir, 0o755); err != nil {
					t.Fatalf("mkdir local bin: %v", err)
				}
				hardlink := filepath.Join(binDir, "gc")
				if err := os.Link(running, hardlink); err != nil {
					t.Skipf("os.Link not supported on this filesystem: %v", err)
				}
				return homeDir, "", running, hardlink
			},
		},
		{
			name: "only_gopath_bin_matches_running_exe",
			setup: func(t *testing.T) (string, string, string, string) {
				running := newRunningExe(t)
				homeDir := t.TempDir()
				gopath := t.TempDir()
				binDir := filepath.Join(gopath, "bin")
				if err := os.MkdirAll(binDir, 0o755); err != nil {
					t.Fatalf("mkdir gopath bin: %v", err)
				}
				symlink := filepath.Join(binDir, "gc")
				if err := os.Symlink(running, symlink); err != nil {
					t.Fatalf("symlink: %v", err)
				}
				return homeDir, gopath, running, symlink
			},
		},
		{
			name: "candidates_point_to_different_binary_falls_through",
			setup: func(t *testing.T) (string, string, string, string) {
				running := newRunningExe(t)
				other := filepath.Join(t.TempDir(), "other-gc")
				if err := os.WriteFile(other, []byte("decoy"), 0o755); err != nil {
					t.Fatalf("write decoy: %v", err)
				}
				homeDir := t.TempDir()
				gopath := t.TempDir()
				localBin := filepath.Join(homeDir, ".local", "bin")
				if err := os.MkdirAll(localBin, 0o755); err != nil {
					t.Fatalf("mkdir local bin: %v", err)
				}
				if err := os.Symlink(other, filepath.Join(localBin, "gc")); err != nil {
					t.Fatalf("symlink local: %v", err)
				}
				gopathBin := filepath.Join(gopath, "bin")
				if err := os.MkdirAll(gopathBin, 0o755); err != nil {
					t.Fatalf("mkdir gopath bin: %v", err)
				}
				if err := os.Symlink(other, filepath.Join(gopathBin, "gc")); err != nil {
					t.Fatalf("symlink gopath: %v", err)
				}
				return homeDir, gopath, running, running
			},
		},
		{
			name: "no_candidates_exist_falls_through",
			setup: func(t *testing.T) (string, string, string, string) {
				running := newRunningExe(t)
				return t.TempDir(), t.TempDir(), running, running
			},
		},
		{
			name: "local_bin_not_executable_falls_through",
			setup: func(t *testing.T) (string, string, string, string) {
				running := newRunningExe(t)
				homeDir := t.TempDir()
				binDir := filepath.Join(homeDir, ".local", "bin")
				if err := os.MkdirAll(binDir, 0o755); err != nil {
					t.Fatalf("mkdir local bin: %v", err)
				}
				if err := os.WriteFile(filepath.Join(binDir, "gc"), []byte{}, 0o644); err != nil {
					t.Fatalf("write non-executable: %v", err)
				}
				return homeDir, "", running, running
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			homeDir, gopath, currentExe, want := tc.setup(t)
			got := resolveStableSupervisorBinaryPath(homeDir, gopath, currentExe)
			if got != want {
				t.Fatalf("resolveStableSupervisorBinaryPath(%q, %q, %q) = %q, want %q",
					homeDir, gopath, currentExe, got, want)
			}
		})
	}
}

func TestBuildSupervisorServiceDataPrefersUserLocalBinExecPath(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("symlink behavior not exercised on windows here")
	}
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
	t.Setenv("XDG_RUNTIME_DIR", "/tmp/gc-run")

	runningExe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	binDir := filepath.Join(homeDir, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir local bin: %v", err)
	}
	stable := filepath.Join(binDir, "gc")
	if err := os.Symlink(runningExe, stable); err != nil {
		t.Fatalf("symlink stable path: %v", err)
	}

	data, err := buildSupervisorServiceData()
	if err != nil {
		t.Fatalf("buildSupervisorServiceData: %v", err)
	}
	if data.GCPath != stable {
		t.Fatalf("GCPath = %q, want %q (stable user-local-bin path)", data.GCPath, stable)
	}

	systemdContent, err := renderSupervisorTemplate(supervisorSystemdTemplate, data)
	if err != nil {
		t.Fatalf("renderSupervisorTemplate: %v", err)
	}
	wantExec := `ExecStart="` + stable + `" supervisor run`
	if !strings.Contains(systemdContent, wantExec) {
		t.Fatalf("systemd unit missing %q:\n%s", wantExec, systemdContent)
	}
}

func TestInstallSupervisorSystemdRefreshesStaleTmpExecStart(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd path only applies on linux")
	}
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))

	runningExe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	binDir := filepath.Join(homeDir, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir local bin: %v", err)
	}
	stable := filepath.Join(binDir, "gc")
	if err := os.Symlink(runningExe, stable); err != nil {
		t.Fatalf("symlink stable: %v", err)
	}

	data, err := buildSupervisorServiceData()
	if err != nil {
		t.Fatalf("buildSupervisorServiceData: %v", err)
	}
	if data.GCPath != stable {
		t.Fatalf("GCPath = %q, want %q", data.GCPath, stable)
	}

	path := supervisorSystemdServicePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir unit dir: %v", err)
	}
	stale := "[Unit]\nDescription=Gas City machine supervisor\n[Service]\nExecStart=/tmp/gc supervisor run\n"
	if err := os.WriteFile(path, []byte(stale), 0o600); err != nil {
		t.Fatalf("write stale unit: %v", err)
	}

	oldRun := supervisorSystemctlRun
	oldActive := supervisorSystemctlActive
	var calls []string
	supervisorSystemctlRun = func(args ...string) error {
		call := strings.Join(args, " ")
		calls = append(calls, call)
		return nil
	}
	supervisorSystemctlActive = func(service string) bool {
		if service != "gascity-supervisor.service" {
			return false
		}
		for _, call := range calls {
			if call == "--user kill --kill-who=main --signal=SIGTERM "+service {
				return false
			}
		}
		return true
	}
	stubSupervisorRunningPreserveSignalReady(t, true)
	t.Cleanup(func() {
		supervisorSystemctlRun = oldRun
		supervisorSystemctlActive = oldActive
	})

	var stdout, stderr bytes.Buffer
	if code := installSupervisorSystemd(data, &stdout, &stderr); code != 0 {
		t.Fatalf("installSupervisorSystemd code = %d, want 0; stderr=%q", code, stderr.String())
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read refreshed unit: %v", err)
	}
	wantExec := `ExecStart="` + stable + `" supervisor run`
	if !strings.Contains(string(contents), wantExec) {
		t.Fatalf("refreshed unit missing %q:\n%s", wantExec, string(contents))
	}
	if strings.Contains(string(contents), `ExecStart="/tmp/gc"`) {
		t.Fatalf("refreshed unit still references stale /tmp/gc:\n%s", string(contents))
	}
	joined := strings.Join(calls, "\n")
	for _, want := range []string{
		"--user daemon-reload",
		"--user kill --kill-who=main --signal=SIGTERM gascity-supervisor.service",
		"--user start gascity-supervisor.service",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("systemctl calls = %v, want %q (warm-refresh path)", calls, want)
		}
	}
}

func TestRenderSupervisorSystemdTemplateQuotesGCPathWithSpaces(t *testing.T) {
	cases := []struct {
		name   string
		gcPath string
		want   string
	}{
		{
			name:   "plain_ascii",
			gcPath: "/usr/local/bin/gc",
			want:   `ExecStart="/usr/local/bin/gc" supervisor run`,
		},
		{
			name:   "home_derived_spacy_path",
			gcPath: "/home/user with spaces/.local/bin/gc",
			want:   `ExecStart="/home/user with spaces/.local/bin/gc" supervisor run`,
		},
		{
			name:   "gopath_derived_spacy_path",
			gcPath: "/opt/go path/bin/gc",
			want:   `ExecStart="/opt/go path/bin/gc" supervisor run`,
		},
		{
			name:   `path_with_embedded_backslash`,
			gcPath: `/opt/foo\bar/gc`,
			want:   `ExecStart="/opt/foo\\bar/gc" supervisor run`,
		},
		{
			name:   `paranoia_spaces_and_embedded_quotes`,
			gcPath: `/srv/binaries/edge "case"/gc`,
			want:   `ExecStart="/srv/binaries/edge \"case\"/gc" supervisor run`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := &supervisorServiceData{
				GCPath:  tc.gcPath,
				LogPath: "/tmp/gc-home/supervisor.log",
				GCHome:  "/tmp/gc-home",
				Path:    "/usr/local/bin:/usr/bin:/bin",
			}
			content, err := renderSupervisorTemplate(supervisorSystemdTemplate, data)
			if err != nil {
				t.Fatalf("renderSupervisorTemplate: %v", err)
			}
			if !strings.Contains(content, tc.want) {
				t.Fatalf("rendered systemd unit missing %q; full:\n%s", tc.want, content)
			}
		})
	}
}
