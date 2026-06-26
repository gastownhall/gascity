package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// gcEnvVars lists the GC_* identity and session-routing variables that
// tests should clear to isolate from host session state (e.g., running
// inside a gc-managed tmux session).
var gcEnvVars = []string{
	"GC_ALIAS",
	"GC_AGENT",
	"GC_BEADS",
	"GC_SESSION_ID",
	"GC_SESSION_NAME",
	"GC_SESSION_ORIGIN",
	"GC_SHARED_SKILL_CATALOG_SNAPSHOT",
	"GC_TEMPLATE",
	"GC_TMUX_SESSION",
	"GC_CITY",
	"GC_DIR",
}

var liveTestEnvVars = []string{
	"BEADS_ACTOR",
	"BEADS_CREDENTIALS_FILE",
	"BEADS_DB_PATH",
	"BEADS_DIR",
	"BEADS_DOLT_PASSWORD",
	"BEADS_DOLT_SERVER_DATABASE",
	"BEADS_DOLT_SERVER_HOST",
	"BEADS_DOLT_SERVER_PORT",
	"BEADS_DOLT_SERVER_USER",
	"DOLT_CONFIG_PATH",
	"DOLT_ROOT_PATH",
	"GC_BEADS_PREFIX",
	"GC_CITY_RUNTIME_DIR",
	"GC_CONTROL_DISPATCHER_TRACE_DEFAULT",
	"GC_DOLT",
	"GC_DOLT_HOST",
	"GC_DOLT_PASSWORD",
	"GC_DOLT_PORT",
	"GC_DOLT_USER",
	"GC_HOME",
	"GC_INSTANCE_TOKEN",
	"GC_PROVIDER",
	"GC_READY_PROMPT_PREFIX",
	"GC_STARTUP_PROMPT_DELIVERED",
	// Inherited systemd delegation env makes existing lifecycle tests
	// take the delegated branch and exec PATH-resolved systemctl against
	// the operator's real unit (including stop). The dynamic GC_* environ
	// scan in liveEnvKeysForTests already catches these today; listing
	// them pins the protection explicitly.
	"GC_SUPERVISOR_SYSTEMD_SCOPE",
	"GC_SUPERVISOR_SYSTEMD_UNIT",
}

// inheritedCityRoutingEnvVars lists GC_* variables that an outer gc-managed
// shell injects to pin commands at a specific city or scope. Unit tests that
// build a fresh temp city must clear them so cityForStoreDir/findCity and the
// scope-aware bead provider resolution actually resolve to the tempdir rather
// than the parent shell's city.
var inheritedCityRoutingEnvVars = []string{
	"GC_CITY_PATH",
	"GC_CITY_ROOT",
	"GC_BEADS_SCOPE_ROOT",
	"GC_DIR",
	"GC_BIN",
	"GC_RIG",
	"GC_RIG_ROOT",
	"GC_CONTINUATION_EPOCH",
	"GC_RUNTIME_EPOCH",
}

// clearGCEnv clears inherited GC, BEADS, and DOLT state for the duration of
// the test, preventing host session state from redirecting temp fixtures into
// live city, rig, or beads stores. GC_HOME is isolated to a temp dir because
// supervisor registry code fails closed when tests leave it empty.
func clearGCEnv(t *testing.T) {
	t.Helper()
	for _, k := range liveEnvKeysForTests() {
		t.Setenv(k, "")
	}
	t.Setenv("GC_HOME", filepath.Join(t.TempDir(), "gc-home"))
}

func clearProcessLiveEnvForTests() {
	for _, k := range liveEnvKeysForTests() {
		_ = os.Unsetenv(k)
	}
}

func TestClearProcessLiveEnvForTestsUnsetsInheritedState(t *testing.T) {
	cleared := []string{
		"BEADS_ACTOR",
		"BEADS_DIR",
		"DOLT_CONFIG_PATH",
		"GC_BEADS",
		"GC_BEADS_SCOPE_ROOT",
		"GC_CITY_PATH",
		"GC_DOLT_HOST",
		"GC_RIG",
		"GC_RIG_ROOT",
		"GC_SESSION_NAME",
		"GC_SUPERVISOR_SYSTEMD_SCOPE",
		"GC_SUPERVISOR_SYSTEMD_UNIT",
	}
	preserved := []string{
		"GC_FAST_UNIT",
		"GC_REAL_PROCESS_SIGNAL_TESTS",
		"GC_TEST_KEEP",
	}

	for _, key := range append(cleared, preserved...) {
		t.Setenv(key, "from-parent-session")
	}

	clearProcessLiveEnvForTests()

	for _, key := range cleared {
		if value, ok := os.LookupEnv(key); ok {
			t.Errorf("%s survived scrub with value %q", key, value)
		}
	}
	for _, key := range preserved {
		if value := os.Getenv(key); value != "from-parent-session" {
			t.Errorf("%s = %q, want preserved test-control value", key, value)
		}
	}
}

func TestIsTestscriptCommandInvocation(t *testing.T) {
	tests := []struct {
		name string
		arg0 string
		want bool
	}{
		{name: "gc helper", arg0: "/tmp/testscript-main/bin/gc", want: true},
		{name: "bd helper", arg0: "/tmp/testscript-main/bin/bd", want: true},
		{name: "windows gc helper", arg0: `C:\Temp\testscript-main\bin\gc.exe`, want: true},
		{name: "top level test binary", arg0: "/tmp/go-build/cmd/gc.test", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTestscriptCommandInvocation(tt.arg0); got != tt.want {
				t.Fatalf("isTestscriptCommandInvocation(%q) = %v, want %v", tt.arg0, got, tt.want)
			}
		})
	}
}

func liveEnvKeysForTests() []string {
	keys := make(map[string]struct{})
	for _, group := range [][]string{gcEnvVars, inheritedCityRoutingEnvVars, liveTestEnvVars} {
		for _, k := range group {
			if !preserveTestControlEnv(k) {
				keys[k] = struct{}{}
			}
		}
	}
	for _, env := range os.Environ() {
		k, _, ok := strings.Cut(env, "=")
		if !ok || preserveTestControlEnv(k) {
			continue
		}
		if strings.HasPrefix(k, "GC_") || strings.HasPrefix(k, "BEADS_") || strings.HasPrefix(k, "DOLT_") {
			keys[k] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)
	return ordered
}

func preserveTestControlEnv(key string) bool {
	return key == "GC_FAST_UNIT" ||
		key == "GC_REAL_PROCESS_SIGNAL_TESTS" ||
		key == managedDoltTestModeEnv ||
		key == managedDoltTestParentPIDEnv ||
		key == "GC_DOLT_REAL_BINARY" ||
		strings.HasPrefix(key, "GC_LIVE_") ||
		strings.HasPrefix(key, "GC_SESSION_CHAOS_") ||
		strings.HasPrefix(key, "GC_TEST_")
}

func TestPreserveTestControlEnvKeepsRealProcessSignalGate(t *testing.T) {
	if !preserveTestControlEnv("GC_REAL_PROCESS_SIGNAL_TESTS") {
		t.Fatal("GC_REAL_PROCESS_SIGNAL_TESTS must survive cmd/gc test env scrubbing")
	}
}

// isTestscriptCommandInvocation reports whether this process is a
// testscript-re-executed command (rogpeppe/go-internal/testscript dispatches
// `exec gc` / `exec bd` by re-invoking the test binary with arg0 set to the
// command name). TestMain must skip the live-env scrub in that case so the
// env directives a testscript injects into its subprocess survive.
func isTestscriptCommandInvocation(arg0 string) bool {
	name := strings.TrimSuffix(filepath.Base(strings.ReplaceAll(arg0, "\\", "/")), ".exe")
	return name == "gc" || name == "bd"
}

// clearInheritedCityRoutingEnv unsets only the city-routing env vars listed in
// inheritedCityRoutingEnvVars for tests that need narrower cleanup than
// clearGCEnv.
func clearInheritedCityRoutingEnv(t *testing.T) {
	t.Helper()
	for _, k := range inheritedCityRoutingEnvVars {
		t.Setenv(k, "")
	}
}

func disableManagedDoltRecoveryForTest(t *testing.T) {
	t.Helper()
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "")
	t.Setenv("GC_DOLT_PORT", "")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "")
}

var testProviderStubCommands = []string{
	"claude",
	"codex",
	"gemini",
	"cursor",
	"copilot",
	"amp",
	"opencode",
	"mimo",
	"auggie",
	"pi",
	"omp",
}

func installTestProviderStubs() (string, error) {
	dir, err := os.MkdirTemp("", pidPrefixedTempPattern(testProviderStubDirPrefix))
	if err != nil {
		return "", err
	}
	for _, name := range testProviderStubCommands {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			_ = os.RemoveAll(dir)
			return "", err
		}
	}
	return dir, nil
}

func builtinProviderAliasesForTest(names ...string) map[string]config.ProviderSpec {
	providers := make(map[string]config.ProviderSpec, len(names))
	for _, name := range names {
		providers[name] = config.BuiltinProviderAlias(name)
	}
	return providers
}

func builtinProviderAliasTOMLForTest(names ...string) string {
	var b strings.Builder
	for _, name := range names {
		b.WriteString("\n[providers.")
		b.WriteString(name)
		b.WriteString("]\nbase = \"builtin:")
		b.WriteString(name)
		b.WriteString("\"\n")
	}
	return b.String()
}

func withBuiltinProviderAliasesTOMLForTest(content string, names ...string) string {
	content = strings.TrimRight(content, "\n")
	if content == "" {
		return strings.TrimLeft(builtinProviderAliasTOMLForTest(names...), "\n")
	}
	return content + "\n" + builtinProviderAliasTOMLForTest(names...)
}

func TestInstallTestProviderStubsUsesPIDPrefixedDir(t *testing.T) {
	dir, err := installTestProviderStubs()
	if err != nil {
		t.Fatalf("installTestProviderStubs: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	pid, ok := pidFromPrefixedDirName(filepath.Base(dir), testProviderStubDirPrefix)
	if !ok {
		t.Fatalf("provider stubs dir %q does not use prefix %q", dir, testProviderStubDirPrefix)
	}
	if pid != os.Getpid() {
		t.Fatalf("provider stubs dir PID = %d, want current PID %d", pid, os.Getpid())
	}
}

func writeTestGitIdentity(homeDir string) error {
	gitConfig := filepath.Join(homeDir, ".gitconfig")
	data := []byte("[user]\n\tname = gc-test\n\temail = gc-test@test.local\n[beads]\n\trole = maintainer\n")
	return os.WriteFile(gitConfig, data, 0o644)
}

// gcBeadsBdTestHomeEnv creates a temp HOME with a .gitconfig containing user
// identity and beads.role = maintainer, then returns extra env entries suitable
// for appending to sanitizedBaseEnv. Use this for any test that runs the real
// gc-beads-bd.sh op_init, which calls ensure_beads_role and requires a writable
// global git config.
func gcBeadsBdTestHomeEnv(t *testing.T) []string {
	t.Helper()
	homeDir := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(beads-bd test home): %v", err)
	}
	if err := writeTestGitIdentity(homeDir); err != nil {
		t.Fatalf("write test git identity for beads-bd: %v", err)
	}
	return []string{
		"HOME=" + homeDir,
		"GIT_CONFIG_GLOBAL=" + filepath.Join(homeDir, ".gitconfig"),
	}
}

func writeTestDoltIdentity(homeDir string) error {
	doltDir := filepath.Join(homeDir, ".dolt")
	if err := os.MkdirAll(doltDir, 0o755); err != nil {
		return err
	}
	data := []byte(`{"user.name":"gc-test","user.email":"gc-test@test.local"}`)
	return os.WriteFile(filepath.Join(doltDir, "config_global.json"), data, 0o644)
}

func configureTestDoltIdentityEnv(t *testing.T) {
	t.Helper()

	homeDir := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(test home): %v", err)
	}
	if err := writeTestGitIdentity(homeDir); err != nil {
		t.Fatalf("write test git identity: %v", err)
	}
	if err := writeTestDoltIdentity(homeDir); err != nil {
		t.Fatalf("write test dolt identity: %v", err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(homeDir, ".gitconfig"))
	t.Setenv("DOLT_ROOT_PATH", homeDir)
}
