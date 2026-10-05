package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// Every GC-owned proxied scope gets an explicit idle timeout. Two
// initializers reach bd's proxied path without going through the
// provider-owned front door — the default rig-store initializer here, and the
// legacy script's run_bd_init_proxied — and both once omitted the flag, so a
// scope created through either got bd's 30s idle timeout and no client-info
// sidecar for the lifecycle to read the proxy root from. With nothing
// configured the value is the default.
func TestDefaultRigBdStoreInitPassesDefaultIdleTimeoutOnProxiedScopes(t *testing.T) {
	clearGCEnv(t)
	t.Setenv(config.ProxiedIdleTimeoutEnv, "")
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "r1")
	if err := os.MkdirAll(rig, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(city, "bd-args")
	fakeBd := filepath.Join(city, "bd")
	if err := os.WriteFile(fakeBd, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \""+logPath+"\"\n"), 0o755); err != nil { //nolint:gosec // fixture must be executable
		t.Fatal(err)
	}
	t.Setenv("BD_BIN", fakeBd)
	writeScopeBeadsMetadata(t, rig, `{"database":"dolt","backend":"dolt","dolt_mode":"proxied-server","dolt_database":"r1"}`)

	// The init itself does more than call bd; only the invocation matters here.
	_ = initDefaultRigBdStore(city, rig, "r1", "r1")

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("the initializer did not invoke bd: %v", err)
	}
	invocation := string(data)
	if !strings.Contains(invocation, "--proxied-server") {
		t.Fatalf("bd was not asked for a proxied store: %s", invocation)
	}
	want := "--proxied-server-idle-timeout " + (config.ProxiedIdleTimeout{Duration: config.DefaultProxiedIdleTimeout}).BdFlagValue() + " "
	if !strings.Contains(invocation, want) {
		t.Fatalf("proxied init = %s, want %q", invocation, want)
	}
}

// The legacy script's proxied initializer passes the same resolved value. It
// is the one an ambient BEADS_DOLT_PROXIED_SERVER can still reach.
func TestLegacyScriptProxiedInitPassesResolvedIdleTimeout(t *testing.T) {
	data, err := os.ReadFile("../../examples/bd/assets/scripts/gc-beads-bd.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	marker := "set -- init --quiet --proxied-server"
	idx := strings.Index(script, marker)
	if idx < 0 {
		t.Fatalf("run_bd_init_proxied no longer builds its argv with %q; move this assertion with it", marker)
	}
	line := script[idx:]
	if end := strings.IndexByte(line, '\n'); end >= 0 {
		line = line[:end]
	}
	if !strings.Contains(line, `--proxied-server-idle-timeout "$GC_BEADS_PROXIED_IDLE_TIMEOUT"`) {
		t.Fatalf("run_bd_init_proxied does not pass the resolved idle timeout: %s", line)
	}
}

// A bare non-bd provider (`provider = "doltlite"`) is not a bd-contract city:
// gc runs no provider-owned front door for it, and the only provider shape that
// door accepts is `exec:`. The default rig-store initializer nevertheless took
// bd's proxied arm for such a city, which made the fresh rig provider-owned by
// binding — after which every `gc start`, health tick and `gc stop` refused the
// city with "requires an exec beads provider", and bd's idle-never proxy plus
// its Dolt child stayed resident with no gc verb left to retire them.
func TestDefaultRigBdStoreKeepsDirectServerForNonBdContractCity(t *testing.T) {
	clearGCEnv(t)
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "api")
	if err := os.MkdirAll(rig, 0o755); err != nil {
		t.Fatal(err)
	}
	cityTOML := "[workspace]\nname = \"tincan-city\"\n\n[beads]\nprovider = \"doltlite\"\n\n" +
		"[[rigs]]\nname = \"api\"\npath = " + strconv.Quote(rig) + "\nprefix = \"api\"\n"
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte(cityTOML), 0o644); err != nil { //nolint:gosec // fixture
		t.Fatal(err)
	}
	logPath := filepath.Join(city, "bd-args")
	fakeBd := filepath.Join(city, "bd")
	if err := os.WriteFile(fakeBd, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \""+logPath+"\"\n"), 0o755); err != nil { //nolint:gosec // fixture must be executable
		t.Fatal(err)
	}
	t.Setenv("BD_BIN", fakeBd)

	if err := initDefaultRigBdStore(city, rig, "api", "api"); err != nil {
		t.Fatalf("initDefaultRigBdStore: %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("the initializer did not invoke bd: %v", err)
	}
	invocation := string(data)
	if strings.Contains(invocation, "--proxied-server") {
		t.Fatalf("bd was asked for a proxied store on a non-bd-contract city: %s", invocation)
	}
	if !strings.Contains(invocation, "--server") {
		t.Fatalf("bd was not asked for a direct server store: %s", invocation)
	}

	// The point of keeping --server: the rig must not classify as
	// provider-owned, because nothing here can run its lifecycle.
	if owned, err := scopeProviderOwned(city, rig); err != nil {
		t.Fatalf("scopeProviderOwned: %v", err)
	} else if owned {
		t.Fatalf("a rig store on a non-bd-contract city classified as provider-owned")
	}
	if err := initAndHookDir(city, rig, "api"); err != nil {
		t.Fatalf("second boot door on the same rig: %v", err)
	}
	if err := shutdownBeadsProvider(city); err != nil {
		t.Fatalf("gc stop after the rig add: %v", err)
	}
}

func writeIdleTimeoutCityToml(t *testing.T, city, beads string, rigs ...string) {
	t.Helper()
	content := "[workspace]\nname = \"idle-city\"\n"
	if beads != "" {
		content += "\n[beads]\n" + beads + "\n"
	}
	for _, rig := range rigs {
		content += "\n[[rigs]]\n" + rig + "\n"
	}
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte(content), 0o644); err != nil { //nolint:gosec // fixture
		t.Fatal(err)
	}
}

func recordDefaultRigInit(t *testing.T, city, rig, prefix string) string {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "bd-args")
	fakeBd := filepath.Join(t.TempDir(), "bd")
	if err := os.WriteFile(fakeBd, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \""+logPath+"\"\n"), 0o755); err != nil { //nolint:gosec // fixture must be executable
		t.Fatal(err)
	}
	t.Setenv("BD_BIN", fakeBd)
	writeScopeBeadsMetadata(t, rig, `{"database":"dolt","backend":"dolt","dolt_mode":"proxied-server","dolt_database":"`+prefix+`"}`)
	// The init itself does more than call bd; only the invocation matters here.
	_ = initDefaultRigBdStore(city, rig, prefix, prefix)
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("the initializer did not invoke bd: %v", err)
	}
	return string(data)
}

// The configured idle timeout reaches bd's init argv: the city value, a rig
// override on top of it, and the env override on top of both.
func TestDefaultRigBdStoreInitCarriesConfiguredIdleTimeout(t *testing.T) {
	for _, tc := range []struct {
		name  string
		beads string
		rig   string
		env   string
		want  string
	}{
		{name: "city value", beads: `proxied_idle_timeout = "45m"`, want: "--proxied-server-idle-timeout 45m0s"},
		{name: "city never", beads: `proxied_idle_timeout = "0"`, want: "--proxied-server-idle-timeout 0 "},
		{name: "rig override", beads: `proxied_idle_timeout = "45m"`, rig: `beads_proxied_idle_timeout = "2h"`, want: "--proxied-server-idle-timeout 2h0m0s"},
		{name: "env wins", beads: `proxied_idle_timeout = "45m"`, rig: `beads_proxied_idle_timeout = "2h"`, env: "20s", want: "--proxied-server-idle-timeout 20s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearGCEnv(t)
			t.Setenv(config.ProxiedIdleTimeoutEnv, tc.env)
			city := t.TempDir()
			rig := filepath.Join(city, "rigs", "r1")
			if err := os.MkdirAll(rig, 0o755); err != nil {
				t.Fatal(err)
			}
			writeIdleTimeoutCityToml(t, city, tc.beads, "name = \"r1\"\npath = "+strconv.Quote(rig)+"\nprefix = \"r1\"\n"+tc.rig)
			got := recordDefaultRigInit(t, city, rig, "r1")
			if !strings.Contains(got, tc.want) {
				t.Fatalf("bd init = %q, want %q", got, tc.want)
			}
		})
	}
}

// A rig on the city's proxy root is served by the city's proxy, so its own
// override cannot apply: it inherits the city's value.
func TestScopeIdleTimeoutSharedRootRigUsesCityValue(t *testing.T) {
	clearGCEnv(t)
	t.Setenv(config.ProxiedIdleTimeoutEnv, "")
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "r1")
	if err := os.MkdirAll(filepath.Join(city, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rig, 0o755); err != nil {
		t.Fatal(err)
	}
	writeIdleTimeoutCityToml(t, city, `proxied_idle_timeout = "45m"`,
		"name = \"r1\"\npath = "+strconv.Quote(rig)+"\nprefix = \"r1\"\nbeads_proxied_idle_timeout = \"0\"")
	writeScopeBeadsMetadata(t, rig, `{"database":"dolt","backend":"dolt","dolt_mode":"proxied-server","dolt_database":"r1","dolt_data_dir":"../../../.beads/dolt"}`)

	var warn strings.Builder
	got, err := resolveScopeProxiedIdleTimeout(city, rig, &warn)
	if err != nil {
		t.Fatal(err)
	}
	if got.Duration != 45*time.Minute || got.Source != config.ProxiedIdleTimeoutSourceCity {
		t.Fatalf("shared-root rig idle = %+v, want the city's 45m", got)
	}
	if !strings.Contains(warn.String(), "shares the city's proxy root") {
		t.Fatalf("warning = %q, want the ignored-override warning", warn.String())
	}

	// The same rig on its own root keeps its override.
	writeScopeBeadsMetadata(t, rig, `{"database":"dolt","backend":"dolt","dolt_mode":"proxied-server","dolt_database":"r1"}`)
	got, err = resolveScopeProxiedIdleTimeout(city, rig, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Never() || got.Source != config.ProxiedIdleTimeoutSourceRig {
		t.Fatalf("own-root rig idle = %+v, want its own never", got)
	}
}

// The script's init arms get the resolved value through the env gc projects
// for the init op, and only for it: start, health, recover and stop never
// resolve it.
func TestScopeInitEnvProjectsResolvedIdleTimeout(t *testing.T) {
	clearGCEnv(t)
	t.Setenv(config.ProxiedIdleTimeoutEnv, "")
	city := t.TempDir()
	writeIdleTimeoutCityToml(t, city, `proxied_idle_timeout = "45m"`)
	provider := "exec:" + filepath.Join(city, "gc-beads-bd")
	env, err := providerLifecycleProcessEnvForScopeInitWithError(city, city, provider)
	if err != nil {
		t.Fatal(err)
	}
	if got := runtimeEnvEntriesToMap(env)[config.ProxiedIdleTimeoutEnv]; got != "" {
		t.Fatalf("the shared lifecycle env carries %s=%q; only init may", config.ProxiedIdleTimeoutEnv, got)
	}
	env, err = withProxiedInitIdleTimeout(env, city, city, provider)
	if err != nil {
		t.Fatal(err)
	}
	if got := runtimeEnvEntriesToMap(env)[config.ProxiedIdleTimeoutEnv]; got != "45m0s" {
		t.Fatalf("%s = %q, want 45m0s", config.ProxiedIdleTimeoutEnv, got)
	}
}

// An invalid idle timeout must never stop gc from retiring a city: the stop
// op does not resolve it, and the config still loads (with a warning).
func TestInvalidProxiedIdleTimeoutDoesNotBreakStop(t *testing.T) {
	clearGCEnv(t)
	t.Setenv(config.ProxiedIdleTimeoutEnv, "")
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "r1")
	if err := os.MkdirAll(rig, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(city, "provider.log")
	provider := filepath.Join(city, "gc-beads-bd.sh")
	if err := os.WriteFile(provider, []byte("#!/bin/sh\nprintf '%s\\n' \"$1\" >> \"$GC_TEST_PROVIDER_LOG\"\n"), 0o755); err != nil { //nolint:gosec // fixture must be executable
		t.Fatal(err)
	}
	t.Setenv("GC_TEST_PROVIDER_LOG", logPath)
	toml := "[beads]\nprovider = \"exec:" + provider + "\"\nproxied_idle_timeout = \"bogus\"\n[[rigs]]\nname = \"r1\"\npath = \"rigs/r1\"\n"
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte(toml), 0o644); err != nil { //nolint:gosec // fixture
		t.Fatal(err)
	}
	if err := persistProviderScopeOwnership(city, rig, providerScopeIntent{Transport: "direct", Target: "local"}); err != nil {
		t.Fatal(err)
	}
	if err := markProviderScopeOwnershipReady(city, rig); err != nil {
		t.Fatal(err)
	}
	if err := runProviderOwnedScopesLifecycleOp(city, "stop"); err != nil {
		t.Fatalf("stop with an invalid idle timeout: %v", err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil || strings.TrimSpace(string(data)) != "stop" {
		t.Fatalf("provider ops = %q (%v), want the stop to have run", data, err)
	}
}

// A proxied init that reaches the script without a resolved idle timeout
// dies instead of letting bd pick one.
func TestGcBeadsBdProxiedInitDiesWithoutIdleTimeout(t *testing.T) {
	scriptPath := filepath.Join(repoRootForLint(t), "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	scopeDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "bd.log")
	bdPath := filepath.Join(t.TempDir(), "bd")
	if err := os.WriteFile(bdPath, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+strconv.Quote(logPath)+"\n[ \"$1\" != context ] || exit 1\n"), 0o755); err != nil { //nolint:gosec // fixture must be executable
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		env  []string
	}{
		{name: "provider-owned", env: []string{"GC_BEADS_PROVIDER_OWNED=1", "GC_BEADS_TRANSPORT=proxied", "GC_BEADS_TARGET=local"}},
		{name: "legacy proxied", env: []string{"BEADS_DOLT_PROXIED_SERVER=1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.Remove(logPath)
			writeScopeBeadsMetadata(t, scopeDir, `{"database":"dolt","backend":"dolt","dolt_mode":"proxied-server","dolt_database":"prov"}`)
			cmd := exec.Command(scriptPath, "init", scopeDir, "prov")
			cmd.Env = sanitizedBaseEnv(append([]string{
				"GC_CITY_PATH=" + scopeDir,
				"BEADS_DIR=" + filepath.Join(scopeDir, ".beads"),
				"BD_BIN=" + bdPath,
				"HOME=" + t.TempDir(),
			}, tc.env...)...)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("proxied init without %s succeeded:\n%s", config.ProxiedIdleTimeoutEnv, out)
			}
			if !strings.Contains(string(out), config.ProxiedIdleTimeoutEnv) {
				t.Fatalf("output = %s, want it to name %s", out, config.ProxiedIdleTimeoutEnv)
			}
			if data, _ := os.ReadFile(logPath); strings.Contains(string(data), "--proxied-server") {
				t.Fatalf("bd init ran anyway: %s", data)
			}
		})
	}
}
