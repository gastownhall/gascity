package main

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/supervisor"
)

type closerSpy struct {
	closed bool
}

func (c *closerSpy) Close() error {
	c.closed = true
	return nil
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
	} {
		if !strings.Contains(content, check) {
			t.Fatalf("launchd template missing %q", check)
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
		`ExecStart=/usr/local/bin/gc supervisor run`,
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

func supervisorServiceEnvMap(vars []supervisorServiceEnvVar) map[string]string {
	m := make(map[string]string, len(vars))
	for _, item := range vars {
		m[item.Name] = item.Value
	}
	return m
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

func TestInstallSupervisorSystemdRestartsWhenUnitChangesAndServiceActive(t *testing.T) {
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
		calls = append(calls, strings.Join(args, " "))
		return nil
	}
	supervisorSystemctlActive = func(service string) bool {
		return service == "gascity-supervisor.service"
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
	for _, want := range []string{
		"--user daemon-reload",
		"--user enable gascity-supervisor.service",
		"--user restart gascity-supervisor.service",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("systemctl calls = %v, want %q", calls, want)
		}
	}
	if strings.Contains(joined, "--user start gascity-supervisor.service") {
		t.Fatalf("systemctl calls = %v, should restart instead of start when unit changes under an active service", calls)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q): %v", path, err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("systemd unit mode after warm upgrade = %03o, want 600", got)
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
		"unload " + legacyPath,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("launchctl calls = %v, want %q", calls, want)
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
