package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/pgauth"
)

func mustBdRuntimeEnv(t *testing.T, cityPath string) map[string]string {
	t.Helper()
	env, err := bdRuntimeEnvWithError(cityPath)
	if err != nil {
		t.Fatalf("bdRuntimeEnvWithError() error = %v", err)
	}
	return env
}

func mustBdRuntimeEnvForRig(t *testing.T, cityPath string, cfg *config.City, rigPath string) map[string]string {
	t.Helper()
	env, err := bdRuntimeEnvForRigWithError(cityPath, cfg, rigPath)
	if err != nil {
		t.Fatalf("bdRuntimeEnvForRigWithError() error = %v", err)
	}
	return env
}

func mustCityRuntimeProcessEnv(t *testing.T, cityPath string) []string {
	t.Helper()
	env, err := cityRuntimeProcessEnvWithError(cityPath)
	if err != nil {
		t.Fatalf("cityRuntimeProcessEnvWithError() error = %v", err)
	}
	return env
}

func envEntriesMap(entries []string) map[string]string {
	result := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			result[key] = value
		}
	}
	return result
}

func mustSessionBackendEnv(t *testing.T, cityPath, rigRoot string, rigs []config.Rig) map[string]string {
	t.Helper()
	env, err := sessionBackendEnvWithError(cityPath, rigRoot, rigs)
	if err != nil {
		t.Fatalf("sessionBackendEnvWithError() error = %v", err)
	}
	return env
}

func requireErrorContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want containing %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want containing %q", err.Error(), want)
	}
}

// ── Dolt config wiring tests (issue 011) ──────────────────────────────

func TestCityRuntimeProcessEnvStripsAmbientGCDolt(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "")
	_ = os.Unsetenv("GC_DOLT_HOST")
	t.Setenv("GC_DOLT_PORT", "")
	_ = os.Unsetenv("GC_DOLT_PORT")

	cityPath := t.TempDir()
	env, err := cityRuntimeProcessEnvWithError(cityPath)
	if err != nil {
		t.Fatalf("cityRuntimeProcessEnvWithError() error = %v", err)
	}
	for _, entry := range env {
		if strings.HasPrefix(entry, "GC_DOLT=") {
			t.Fatalf("cityRuntimeProcessEnv leaked ambient GC_DOLT control var: %q", entry)
		}
	}
}

func TestCityRuntimeProcessEnvUsesNativeOpenEnvSnapshotGuard(t *testing.T) {
	orig := processEnvSnapshotExcludingNativeDoltOpen
	called := false
	processEnvSnapshotExcludingNativeDoltOpen = func() []string {
		called = true
		return []string{
			"PATH=" + os.Getenv("PATH"),
			"BEADS_DOLT_SERVER_HOST=ambient.example.com",
		}
	}
	t.Cleanup(func() {
		processEnvSnapshotExcludingNativeDoltOpen = orig
	})

	env, err := cityRuntimeProcessEnvWithError(t.TempDir())
	if err != nil {
		t.Fatalf("cityRuntimeProcessEnvWithError() error = %v", err)
	}
	if !called {
		t.Fatal("cityRuntimeProcessEnvWithError did not use native-open env snapshot guard")
	}
	if got := envEntriesMap(env)["BEADS_DOLT_SERVER_HOST"]; got == "ambient.example.com" {
		t.Fatalf("cityRuntimeProcessEnvWithError inherited unprojected native-open env host %q", got)
	}
}

func TestRecoverManagedBDCommandUsesNativeOpenEnvSnapshotGuard(t *testing.T) {
	orig := processEnvSnapshotExcludingNativeDoltOpen
	called := false
	processEnvSnapshotExcludingNativeDoltOpen = func() []string {
		called = true
		return []string{
			"PATH=" + os.Getenv("PATH"),
			"BEADS_DOLT_SERVER_HOST=ambient.example.com",
		}
	}
	t.Cleanup(func() {
		processEnvSnapshotExcludingNativeDoltOpen = orig
	})

	cityPath := t.TempDir()
	capture := filepath.Join(t.TempDir(), "recover-env.txt")
	script := gcBeadsBdScriptPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	content := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"${BEADS_DOLT_SERVER_HOST:-}\" > %q\n", capture)
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := recoverManagedBDCommand(cityPath); err != nil {
		t.Fatalf("recoverManagedBDCommand: %v", err)
	}
	if !called {
		t.Fatal("recoverManagedBDCommand did not use native-open env snapshot guard")
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("read captured env: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got == "ambient.example.com" {
		t.Fatalf("recoverManagedBDCommand inherited unprojected native-open env host %q", got)
	}
}

func TestBdStoreForCityResolvesIDPrefixFromScopeConfig(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "Metro City"
prefix = "mc"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte("issue_prefix: hq\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := bdStoreForCity(cityDir, cityDir)
	if got := store.IDPrefix(); got != "hq" {
		t.Fatalf("IDPrefix() = %q, want hq", got)
	}
}

func TestBdStoreForCityEnablesSkipLabelsFromBD105Compatibility(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "Metro City"

[beads]
bd_compatibility = "bd-1.0.5"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	store := bdStoreForCity(cityDir, cityDir)
	if !store.ListSkipLabelsEnabled() {
		t.Fatal("bdStoreForCity did not enable bd list --skip-labels for bd-1.0.5 compatibility")
	}
}

func TestBdStoreForCityLeavesSkipLabelsDisabledByDefault(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "Metro City"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	store := bdStoreForCity(cityDir, cityDir)
	if store.ListSkipLabelsEnabled() {
		t.Fatal("bdStoreForCity enabled bd list --skip-labels without bd-1.0.5 compatibility")
	}
}

func TestBdStoreForRigResolvesIDPrefixFromScopeConfig(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte("issue_prefix: repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Rigs: []config.Rig{{Name: "repo", Path: "rigs/repo", Prefix: "ga"}}}

	store := bdStoreForRig(rigDir, cityDir, cfg)
	if got := store.IDPrefix(); got != "repo" {
		t.Fatalf("IDPrefix() = %q, want repo", got)
	}
}

func TestBdStoreForRigEnablesSkipLabelsFromBD105Compatibility(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "repo")
	if err := os.MkdirAll(rigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Beads: config.BeadsConfig{BDCompatibility: config.BeadsBDCompatibility105},
		Rigs:  []config.Rig{{Name: "repo", Path: "rigs/repo", Prefix: "ga"}},
	}

	store := bdStoreForRig(rigDir, cityDir, cfg)
	if !store.ListSkipLabelsEnabled() {
		t.Fatal("bdStoreForRig did not enable bd list --skip-labels for bd-1.0.5 compatibility")
	}
}

func TestBdStoreForRigPrefersScopeConfigOverKnownPrefix(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte("issue_prefix: repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Rigs: []config.Rig{{Name: "repo", Path: "rigs/repo", Prefix: "ga"}}}

	store := bdStoreForRig(rigDir, cityDir, cfg, "stale")
	if got := store.IDPrefix(); got != "repo" {
		t.Fatalf("IDPrefix() = %q, want repo", got)
	}
}

func TestBdStoreForRigFallsBackToConfiguredEffectivePrefix(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "repo")
	if err := os.MkdirAll(rigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Rigs: []config.Rig{{Name: "repo", Path: "rigs/repo", Prefix: "ga"}}}

	store := bdStoreForRig(rigDir, cityDir, cfg)
	if got := store.IDPrefix(); got != "ga" {
		t.Fatalf("IDPrefix() = %q, want ga", got)
	}
}

func TestBdRuntimeEnvIncludesDoltHost(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT_HOST", "mini2.hippo-tilapia.ts.net")
	t.Setenv("GC_DOLT_PORT", "3307")
	t.Setenv("GC_DOLT_USER", "agent")
	t.Setenv("GC_DOLT_PASSWORD", "s3cret")
	t.Setenv("GC_DOLT", "skip")

	cityPath := t.TempDir()
	env, err := bdRuntimeEnvWithError(cityPath)
	if err != nil {
		t.Fatalf("bdRuntimeEnvWithError() error = %v", err)
	}

	if got := env["GC_DOLT_HOST"]; got != "mini2.hippo-tilapia.ts.net" {
		t.Errorf("GC_DOLT_HOST = %q, want %q", got, "mini2.hippo-tilapia.ts.net")
	}
	if got := env["GC_DOLT_PORT"]; got != "3307" {
		t.Errorf("GC_DOLT_PORT = %q, want %q", got, "3307")
	}
	if got := env["BEADS_DOLT_SERVER_HOST"]; got != "mini2.hippo-tilapia.ts.net" {
		t.Errorf("BEADS_DOLT_SERVER_HOST = %q, want %q", got, "mini2.hippo-tilapia.ts.net")
	}
	if got := env["BEADS_DOLT_SERVER_PORT"]; got != "3307" {
		t.Errorf("BEADS_DOLT_SERVER_PORT = %q, want %q", got, "3307")
	}
	if got := env["BEADS_DOLT_SERVER_USER"]; got != "agent" {
		t.Errorf("BEADS_DOLT_SERVER_USER = %q, want %q", got, "agent")
	}
	if got := env["BEADS_DOLT_PASSWORD"]; got != "s3cret" {
		t.Errorf("BEADS_DOLT_PASSWORD = %q, want %q", got, "s3cret")
	}
	if got := env["BEADS_DOLT_AUTO_START"]; got != "0" {
		t.Errorf("BEADS_DOLT_AUTO_START = %q, want %q", got, "0")
	}
}

// TestBdRuntimeEnvNoRecoveryMatchesRecoveryForExternalTarget proves the
// NoRecovery variant (used by ga-cdmx6x's scoped throwaway stores) still
// resolves an explicitly configured external Dolt target identically to
// the recovery-allowing default — only the managed-dolt
// recovery/health-check/autostart side effect is skipped, not the
// ordinary external-config path.
func TestBdRuntimeEnvNoRecoveryMatchesRecoveryForExternalTarget(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT_HOST", "mini2.hippo-tilapia.ts.net")
	t.Setenv("GC_DOLT_PORT", "3307")
	t.Setenv("GC_DOLT_USER", "agent")
	t.Setenv("GC_DOLT_PASSWORD", "s3cret")
	t.Setenv("GC_DOLT", "skip")

	cityPath := t.TempDir()
	env, err := bdRuntimeEnvWithErrorNoRecovery(cityPath)
	if err != nil {
		t.Fatalf("bdRuntimeEnvWithErrorNoRecovery() error = %v", err)
	}

	if got := env["GC_DOLT_HOST"]; got != "mini2.hippo-tilapia.ts.net" {
		t.Errorf("GC_DOLT_HOST = %q, want %q", got, "mini2.hippo-tilapia.ts.net")
	}
	if got := env["GC_DOLT_PORT"]; got != "3307" {
		t.Errorf("GC_DOLT_PORT = %q, want %q", got, "3307")
	}
	if got := env["BEADS_DOLT_AUTO_START"]; got != "0" {
		t.Errorf("BEADS_DOLT_AUTO_START = %q, want %q", got, "0")
	}
}

// TestBdRuntimeEnvForRigNoRecoveryMatchesRecoveryForExternalTarget is
// TestBdRuntimeEnvNoRecoveryMatchesRecoveryForExternalTarget for the
// rig-scoped resolver.
func TestBdRuntimeEnvForRigNoRecoveryMatchesRecoveryForExternalTarget(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT_HOST", "mini2.hippo-tilapia.ts.net")
	t.Setenv("GC_DOLT_PORT", "3307")
	t.Setenv("GC_DOLT_USER", "agent")
	t.Setenv("GC_DOLT_PASSWORD", "s3cret")
	t.Setenv("GC_DOLT", "skip")

	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "rigs", "repo")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Rigs: []config.Rig{{Name: "repo", Path: "rigs/repo"}}}

	env, err := bdRuntimeEnvForRigWithErrorNoRecovery(cityPath, cfg, rigPath)
	if err != nil {
		t.Fatalf("bdRuntimeEnvForRigWithErrorNoRecovery() error = %v", err)
	}
	if got := env["GC_DOLT_HOST"]; got != "mini2.hippo-tilapia.ts.net" {
		t.Errorf("GC_DOLT_HOST = %q, want %q", got, "mini2.hippo-tilapia.ts.net")
	}
	if got := env["GC_RIG"]; got != "repo" {
		t.Errorf("GC_RIG = %q, want repo", got)
	}
}

func TestBdRuntimeEnvDisablesCLIRemoteSync(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("BD_DOLT_SYNC_CLI_REMOTES", "true")
	t.Setenv("BEADS_DOLT_SYNC_CLI_REMOTES", "true")

	env := mustBdRuntimeEnv(t, t.TempDir())
	if got := env["BD_DOLT_SYNC_CLI_REMOTES"]; got != "false" {
		t.Fatalf("BD_DOLT_SYNC_CLI_REMOTES = %q, want false", got)
	}
	if got := env["BEADS_DOLT_SYNC_CLI_REMOTES"]; got != "false" {
		t.Fatalf("BEADS_DOLT_SYNC_CLI_REMOTES = %q, want false", got)
	}
}

func TestCityRuntimeProcessEnvDisablesCLIRemoteSync(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("BD_DOLT_SYNC_CLI_REMOTES", "true")
	t.Setenv("BEADS_DOLT_SYNC_CLI_REMOTES", "true")

	env := mustCityRuntimeProcessEnv(t, t.TempDir())
	values := map[string]string{}
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[key] = value
		}
	}
	if got := values["BD_DOLT_SYNC_CLI_REMOTES"]; got != "false" {
		t.Fatalf("BD_DOLT_SYNC_CLI_REMOTES = %q, want false", got)
	}
	if got := values["BEADS_DOLT_SYNC_CLI_REMOTES"]; got != "false" {
		t.Fatalf("BEADS_DOLT_SYNC_CLI_REMOTES = %q, want false", got)
	}
}

func TestSessionBackendEnvDisablesCLIRemoteSync(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("BD_DOLT_SYNC_CLI_REMOTES", "true")
	t.Setenv("BEADS_DOLT_SYNC_CLI_REMOTES", "true")

	env := mustSessionBackendEnv(t, t.TempDir(), "", nil)
	if got := env["BD_DOLT_SYNC_CLI_REMOTES"]; got != "false" {
		t.Fatalf("BD_DOLT_SYNC_CLI_REMOTES = %q, want false", got)
	}
	if got := env["BEADS_DOLT_SYNC_CLI_REMOTES"]; got != "false" {
		t.Fatalf("BEADS_DOLT_SYNC_CLI_REMOTES = %q, want false", got)
	}
}

func TestRecoverManagedBDCommandDisablesCLIRemoteSync(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("BD_DOLT_SYNC_CLI_REMOTES", "true")
	t.Setenv("BEADS_DOLT_SYNC_CLI_REMOTES", "true")

	cityPath := t.TempDir()
	envFile := filepath.Join(cityPath, "recover-env.txt")
	scriptPath := gcBeadsBdScriptPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(scriptPath), 0o700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"printf 'BD_DOLT_SYNC_CLI_REMOTES=%s\\n' \"$BD_DOLT_SYNC_CLI_REMOTES\" > \"" + envFile + "\"\n" +
		"printf 'BEADS_DOLT_SYNC_CLI_REMOTES=%s\\n' \"$BEADS_DOLT_SYNC_CLI_REMOTES\" >> \"" + envFile + "\"\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := recoverManagedBDCommand(cityPath); err != nil {
		t.Fatalf("recoverManagedBDCommand() error = %v", err)
	}
	data, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[key] = value
		}
	}
	if got := values["BD_DOLT_SYNC_CLI_REMOTES"]; got != "false" {
		t.Fatalf("BD_DOLT_SYNC_CLI_REMOTES = %q, want false", got)
	}
	if got := values["BEADS_DOLT_SYNC_CLI_REMOTES"]; got != "false" {
		t.Fatalf("BEADS_DOLT_SYNC_CLI_REMOTES = %q, want false", got)
	}
}

// The auto-backup opt-out tests mirror the CLI-remote-sync opt-out tests
// above. They guard against recurrence of the 2026-06-08 town-wide Dolt
// wedge (ga-0eq), whose root cause was bd's PersistentPostRun auto-backup
// (the hardcoded "backup_export" Dolt remote) stuck-looping and saturating
// the commit path. gc-managed bd invocations must force BD_BACKUP_ENABLED
// off at every env-projection site, overriding any ambient/config value so
// a fresh or drifted rig scope cannot re-enable it.

func TestBdRuntimeEnvDisablesAutoBackup(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("BD_BACKUP_ENABLED", "true")
	t.Setenv("BEADS_BACKUP_ENABLED", "true")

	env := mustBdRuntimeEnv(t, t.TempDir())
	if got := env["BD_BACKUP_ENABLED"]; got != "false" {
		t.Fatalf("BD_BACKUP_ENABLED = %q, want false", got)
	}
	if got := env["BEADS_BACKUP_ENABLED"]; got != "false" {
		t.Fatalf("BEADS_BACKUP_ENABLED = %q, want false", got)
	}
}

func TestCityRuntimeProcessEnvDisablesAutoBackup(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("BD_BACKUP_ENABLED", "true")
	t.Setenv("BEADS_BACKUP_ENABLED", "true")

	values := envEntriesMap(mustCityRuntimeProcessEnv(t, t.TempDir()))
	if got := values["BD_BACKUP_ENABLED"]; got != "false" {
		t.Fatalf("BD_BACKUP_ENABLED = %q, want false", got)
	}
	if got := values["BEADS_BACKUP_ENABLED"]; got != "false" {
		t.Fatalf("BEADS_BACKUP_ENABLED = %q, want false", got)
	}
}

func TestSessionBackendEnvDisablesAutoBackup(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("BD_BACKUP_ENABLED", "true")
	t.Setenv("BEADS_BACKUP_ENABLED", "true")

	env := mustSessionBackendEnv(t, t.TempDir(), "", nil)
	if got := env["BD_BACKUP_ENABLED"]; got != "false" {
		t.Fatalf("BD_BACKUP_ENABLED = %q, want false", got)
	}
	if got := env["BEADS_BACKUP_ENABLED"]; got != "false" {
		t.Fatalf("BEADS_BACKUP_ENABLED = %q, want false", got)
	}
}

func TestRecoverManagedBDCommandDisablesAutoBackup(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("BD_BACKUP_ENABLED", "true")
	t.Setenv("BEADS_BACKUP_ENABLED", "true")

	cityPath := t.TempDir()
	envFile := filepath.Join(cityPath, "recover-env.txt")
	scriptPath := gcBeadsBdScriptPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(scriptPath), 0o700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"printf 'BD_BACKUP_ENABLED=%s\\n' \"$BD_BACKUP_ENABLED\" > \"" + envFile + "\"\n" +
		"printf 'BEADS_BACKUP_ENABLED=%s\\n' \"$BEADS_BACKUP_ENABLED\" >> \"" + envFile + "\"\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := recoverManagedBDCommand(cityPath); err != nil {
		t.Fatalf("recoverManagedBDCommand() error = %v", err)
	}
	data, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[key] = value
		}
	}
	if got := values["BD_BACKUP_ENABLED"]; got != "false" {
		t.Fatalf("BD_BACKUP_ENABLED = %q, want false", got)
	}
	if got := values["BEADS_BACKUP_ENABLED"]; got != "false" {
		t.Fatalf("BEADS_BACKUP_ENABLED = %q, want false", got)
	}
}

func TestSessionBackendEnvDisablesContributorRouting(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("BD_ROUTING_MODE", "auto")
	t.Setenv("BEADS_ROUTING_MODE", "auto")

	env := mustSessionBackendEnv(t, t.TempDir(), "", nil)
	if got := env["BD_ROUTING_MODE"]; got != "off" {
		t.Fatalf("BD_ROUTING_MODE = %q, want off", got)
	}
	if got := env["BEADS_ROUTING_MODE"]; got != "off" {
		t.Fatalf("BEADS_ROUTING_MODE = %q, want off", got)
	}
}

func TestRecoverManagedBDCommandDisablesContributorRouting(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("BD_ROUTING_MODE", "auto")
	t.Setenv("BEADS_ROUTING_MODE", "auto")

	cityPath := t.TempDir()
	envFile := filepath.Join(cityPath, "recover-env.txt")
	scriptPath := gcBeadsBdScriptPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(scriptPath), 0o700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"printf 'BD_ROUTING_MODE=%s\\n' \"$BD_ROUTING_MODE\" > \"" + envFile + "\"\n" +
		"printf 'BEADS_ROUTING_MODE=%s\\n' \"$BEADS_ROUTING_MODE\" >> \"" + envFile + "\"\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := recoverManagedBDCommand(cityPath); err != nil {
		t.Fatalf("recoverManagedBDCommand() error = %v", err)
	}
	data, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[key] = value
		}
	}
	if got := values["BD_ROUTING_MODE"]; got != "off" {
		t.Fatalf("BD_ROUTING_MODE = %q, want off", got)
	}
	if got := values["BEADS_ROUTING_MODE"]; got != "off" {
		t.Fatalf("BEADS_ROUTING_MODE = %q, want off", got)
	}
}

func TestBdRuntimeEnvExternalHostSkipsLocalState(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT_HOST", "remote.example.com")
	t.Setenv("GC_DOLT_PORT", "3307")
	t.Setenv("GC_DOLT", "skip")

	cityPath := t.TempDir()
	env := mustBdRuntimeEnv(t, cityPath)

	if got := env["GC_DOLT_PORT"]; got != "3307" {
		t.Errorf("GC_DOLT_PORT = %q, want %q (should use env, not local state)", got, "3307")
	}
	if got := env["BEADS_DOLT_SERVER_PORT"]; got != "3307" {
		t.Errorf("BEADS_DOLT_SERVER_PORT = %q, want %q (should mirror external env)", got, "3307")
	}
}

func TestResolvedRuntimeCityDoltTargetUsesEnvOverrideWhenManagedRuntimeUnavailable(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "remote.example.com")
	t.Setenv("GC_DOLT_PORT", "5511")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	target, ok, err := resolvedRuntimeCityDoltTarget(cityPath, true)
	if err != nil {
		t.Fatalf("resolvedRuntimeCityDoltTarget() error = %v", err)
	}
	if !ok {
		t.Fatal("resolvedRuntimeCityDoltTarget() ok = false, want env override fallback")
	}
	if target.Host != "remote.example.com" || target.Port != "5511" || !target.External {
		t.Fatalf("target = %+v, want external env override remote.example.com:5511", target)
	}
}

func TestManagedLocalDoltHostRecognizesIPv6LoopbackAndWildcard(t *testing.T) {
	for _, tc := range []struct {
		host string
		want bool
	}{
		{"", true},
		{"127.0.0.1", true},
		{"127.0.0.2", true},
		{"localhost", true},
		{"0.0.0.0", true},
		{"::1", true},
		{"[::1]", true},
		{"::", true},
		{"db.example.com", false},
	} {
		t.Run(tc.host, func(t *testing.T) {
			if got := managedLocalDoltHost(tc.host); got != tc.want {
				t.Fatalf("managedLocalDoltHost(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
	}
}

func reachableNonLoopbackHost(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, addr := range addrs {
		var ip net.IP
		switch typed := addr.(type) {
		case *net.IPNet:
			ip = typed.IP
		case *net.IPAddr:
			ip = typed.IP
		default:
			continue
		}
		ip = ip.To4()
		if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
			continue
		}
		listener, err := net.Listen("tcp", net.JoinHostPort(ip.String(), "0"))
		if err != nil {
			continue
		}
		_ = listener.Close()
		return ip.String()
	}
	t.Skip("no bindable non-loopback IPv4 address")
	return ""
}

func TestResolvedRuntimeCityDoltTargetIgnoresIPv6LocalEnvOverride(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_PORT", "3307")
	for _, host := range []string{"::1", "::"} {
		t.Run(host, func(t *testing.T) {
			t.Setenv("GC_DOLT_HOST", host)
			cityPath := t.TempDir()
			target, ok, err := resolvedRuntimeCityDoltTarget(cityPath, false)
			if err != nil {
				t.Fatalf("resolvedRuntimeCityDoltTarget() error = %v", err)
			}
			if ok {
				t.Fatalf("resolvedRuntimeCityDoltTarget() = %+v, want no external fallback for local host %q", target, host)
			}
		})
	}
}

func TestBdRuntimeEnvUsesCanonicalExternalUser(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "")
	t.Setenv("GC_DOLT_PORT", "")
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("GC_DOLT_PASSWORD", "")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: db.example.com
dolt.port: 3307
dolt.user: agent
`), 0o644); err != nil {
		t.Fatal(err)
	}

	env := mustBdRuntimeEnv(t, cityPath)
	if got := env["GC_DOLT_HOST"]; got != "db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want %q", got, "db.example.com")
	}
	if got := env["GC_DOLT_PORT"]; got != "3307" {
		t.Fatalf("GC_DOLT_PORT = %q, want %q", got, "3307")
	}
	if got := env["GC_DOLT_USER"]; got != "agent" {
		t.Fatalf("GC_DOLT_USER = %q, want %q", got, "agent")
	}
	if got := env["BEADS_DOLT_SERVER_USER"]; got != "agent" {
		t.Fatalf("BEADS_DOLT_SERVER_USER = %q, want %q", got, "agent")
	}
}

func TestBdRuntimeEnvDoesNotUseStalePortFileWithoutManagedRuntimeState(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "")
	_ = os.Unsetenv("GC_DOLT_HOST")
	t.Setenv("GC_DOLT_PORT", "")
	_ = os.Unsetenv("GC_DOLT_PORT")
	t.Setenv("GC_DOLT_USER", "")
	_ = os.Unsetenv("GC_DOLT_USER")
	t.Setenv("GC_DOLT_PASSWORD", "")
	_ = os.Unsetenv("GC_DOLT_PASSWORD")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo

dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }() //nolint:errcheck // test cleanup
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "dolt-server.port"), []byte(strings.TrimPrefix(ln.Addr().String(), "127.0.0.1:")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	env := mustBdRuntimeEnv(t, cityPath)
	if got := env["GC_DOLT_PORT"]; got != "" {
		t.Fatalf("GC_DOLT_PORT = %q, want empty without managed runtime state", got)
	}
	if got := env["BEADS_DOLT_SERVER_PORT"]; got != "" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want empty without managed runtime state", got)
	}
}

func TestBdRuntimeEnvUsesValidProviderStateWhenPublishedStateIsMissing(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "")
	_ = os.Unsetenv("GC_DOLT_HOST")
	t.Setenv("GC_DOLT_PORT", "")
	_ = os.Unsetenv("GC_DOLT_PORT")
	t.Setenv("GC_DOLT_USER", "")
	_ = os.Unsetenv("GC_DOLT_USER")
	t.Setenv("GC_DOLT_PASSWORD", "")
	_ = os.Unsetenv("GC_DOLT_PASSWORD")

	cityPath := t.TempDir()
	writeMinimalCityToml(t, cityPath)
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	port := reserveRandomTCPPort(t)
	listener := startTCPListenerProcessInDir(t, port, filepath.Join(cityPath, ".beads", "dolt"))
	defer func() {
		_ = listener.Process.Kill()
		_ = listener.Wait()
	}()

	state := doltRuntimeState{
		Running:   true,
		PID:       listener.Process.Pid,
		Port:      port,
		DataDir:   filepath.Join(cityPath, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeDoltRuntimeStateFile(providerManagedDoltStatePath(cityPath), state); err != nil {
		t.Fatalf("write provider state: %v", err)
	}
	if _, err := os.Stat(managedDoltStatePath(cityPath)); !os.IsNotExist(err) {
		t.Fatalf("published state should be absent before fallback test, stat err = %v", err)
	}
	if got := currentResolvableManagedDoltPort(cityPath); got != strconv.Itoa(port) {
		t.Fatalf("currentResolvableManagedDoltPort() = %q, want %d", got, port)
	}

	env, err := bdRuntimeEnvWithError(cityPath)
	if err != nil {
		t.Fatalf("bdRuntimeEnvWithError() error = %v", err)
	}
	want := strconv.Itoa(port)
	if got := env["GC_DOLT_PORT"]; got != want {
		t.Fatalf("GC_DOLT_PORT = %q, want provider-state port %q", got, want)
	}
	if got := env["BEADS_DOLT_SERVER_PORT"]; got != want {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want provider-state port %q", got, want)
	}
	if got := env["GC_DOLT_HOST"]; got != "" {
		t.Fatalf("GC_DOLT_HOST = %q, want empty for managed provider-state target", got)
	}

	publishedState, err := readDoltRuntimeStateFile(managedDoltStatePath(cityPath))
	if err != nil {
		t.Fatalf("read published state: %v", err)
	}
	if publishedState.Port != port || publishedState.PID != listener.Process.Pid {
		t.Fatalf("published state = %+v, want pid %d port %d", publishedState, listener.Process.Pid, port)
	}
	mirror, err := os.ReadFile(filepath.Join(cityPath, ".beads", "dolt-server.port"))
	if err != nil {
		t.Fatalf("read port mirror: %v", err)
	}
	if got := strings.TrimSpace(string(mirror)); got != strconv.Itoa(port) {
		t.Fatalf("port mirror = %q, want %d", got, port)
	}
}

func TestResolvedRuntimeCityDoltTargetWithoutRecoveryDoesNotPublishProviderState(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "")
	_ = os.Unsetenv("GC_DOLT_HOST")
	t.Setenv("GC_DOLT_PORT", "")
	_ = os.Unsetenv("GC_DOLT_PORT")

	cityPath := t.TempDir()
	writeMinimalCityToml(t, cityPath)
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	port := writeReachableProviderManagedDoltState(t, cityPath)

	target, ok, err := resolvedRuntimeCityDoltTarget(cityPath, false)
	if err == nil || !contract.IsManagedRuntimeUnavailable(err) {
		t.Fatalf("resolvedRuntimeCityDoltTarget() error = %v, want managed runtime unavailable", err)
	}
	if ok {
		t.Fatalf("resolvedRuntimeCityDoltTarget() ok = true with target %+v, want no fallback target", target)
	}
	if got := currentResolvableManagedDoltPort(cityPath); got != strconv.Itoa(port) {
		t.Fatalf("currentResolvableManagedDoltPort() = %q, want %d", got, port)
	}
	if _, err := os.Stat(managedDoltStatePath(cityPath)); !os.IsNotExist(err) {
		t.Fatalf("published state should remain absent when recovery is disabled, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(cityPath, ".beads", "dolt-server.port")); !os.IsNotExist(err) {
		t.Fatalf("port mirror should remain absent when recovery is disabled, stat err = %v", err)
	}
}

func TestResolvedRuntimeCityDoltTargetFallsBackToEnvWhenProviderStateIsNotOwned(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "external-db.example.com")
	t.Setenv("GC_DOLT_PORT", "3307")

	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "file"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeReachableProviderManagedDoltState(t, cityPath)

	target, ok, err := resolvedRuntimeCityDoltTarget(cityPath, true)
	if err != nil {
		t.Fatalf("resolvedRuntimeCityDoltTarget() error = %v, want env fallback", err)
	}
	if !ok {
		t.Fatal("resolvedRuntimeCityDoltTarget() ok = false, want env fallback")
	}
	if target.Host != "external-db.example.com" || target.Port != "3307" || !target.External {
		t.Fatalf("resolvedRuntimeCityDoltTarget() = %+v, want external env target", target)
	}
	if _, err := os.Stat(managedDoltStatePath(cityPath)); !os.IsNotExist(err) {
		t.Fatalf("published state should remain absent for not-owned recovery, stat err = %v", err)
	}
}

func TestResolvedRuntimeCityDoltTargetSurfacesNotOwnedProviderStateWhenNoFallbackResolves(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	_ = os.Unsetenv("GC_DOLT_HOST")
	_ = os.Unsetenv("GC_DOLT_PORT")

	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "file"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeReachableProviderManagedDoltState(t, cityPath)

	target, ok, err := resolvedRuntimeCityDoltTarget(cityPath, true)
	if err == nil {
		t.Fatalf("resolvedRuntimeCityDoltTarget() error = nil with ok=%v target=%+v, want not-owned recovery error", ok, target)
	}
	requireErrorContains(t, err, "managed dolt lifecycle is not owned")
}

func TestResolvedRuntimeCityDoltTargetFallsBackToResolvablePortWhenPublishWriteFails(t *testing.T) {
	// Regression test for ga-crh00: when publishManagedDoltRuntimeStateFromState
	// cannot write dolt-state.json (e.g., write permission failure), the function
	// must still return the port via the currentResolvableManagedDoltPort fallback
	// rather than surfacing the publish error.
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	_ = os.Unsetenv("GC_DOLT_HOST")
	_ = os.Unsetenv("GC_DOLT_PORT")
	_ = os.Unsetenv("GC_DOLT_USER")
	_ = os.Unsetenv("GC_DOLT_PASSWORD")

	cityPath := t.TempDir()
	writeMinimalCityToml(t, cityPath)
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	port := writeReachableProviderManagedDoltState(t, cityPath)

	// Make the dolt state dir read-only to force publishManagedDoltRuntimeStateFromState
	// to fail — it cannot write dolt-state.json to the read-only directory.
	doltStateDir := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt")
	if err := os.Chmod(doltStateDir, 0o555); err != nil {
		t.Fatalf("chmod state dir read-only: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(doltStateDir, 0o755)
	})

	target, ok, err := resolvedRuntimeCityDoltTarget(cityPath, true)
	if err != nil {
		t.Fatalf("resolvedRuntimeCityDoltTarget() error = %v, want fallback to resolvable port", err)
	}
	if !ok {
		t.Fatalf("resolvedRuntimeCityDoltTarget() ok = false, want fallback target")
	}
	if target.Port != strconv.Itoa(port) {
		t.Fatalf("resolvedRuntimeCityDoltTarget() port = %q, want %d", target.Port, port)
	}
	if target.Host != defaultManagedDoltHost {
		t.Fatalf("resolvedRuntimeCityDoltTarget() host = %q, want %q", target.Host, defaultManagedDoltHost)
	}
	if _, err := os.Stat(managedDoltStatePath(cityPath)); !os.IsNotExist(err) {
		t.Fatalf("published state should remain absent when write was blocked, stat err = %v", err)
	}
}

func TestResolvedRuntimeCityDoltTargetDoesNotMaskInvalidCanonicalConfigWithProviderState(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	_ = os.Unsetenv("GC_DOLT_HOST")
	_ = os.Unsetenv("GC_DOLT_PORT")
	_ = os.Unsetenv("GC_DOLT_USER")
	_ = os.Unsetenv("GC_DOLT_PASSWORD")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: canonical-db.example.com
`), 0o644); err != nil {
		t.Fatal(err)
	}

	port := reserveRandomTCPPort(t)
	listener := startTCPListenerProcessInDir(t, port, filepath.Join(cityPath, ".beads", "dolt"))
	defer func() {
		_ = listener.Process.Kill()
		_ = listener.Wait()
	}()

	state := doltRuntimeState{
		Running:   true,
		PID:       listener.Process.Pid,
		Port:      port,
		DataDir:   filepath.Join(cityPath, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeDoltRuntimeStateFile(providerManagedDoltStatePath(cityPath), state); err != nil {
		t.Fatalf("write provider state: %v", err)
	}

	_, _, err := resolvedRuntimeCityDoltTarget(cityPath, true)
	requireErrorContains(t, err, "city_canonical config requires dolt.port")
}

func TestBdRuntimeEnvInvalidCanonicalConfigDoesNotFallbackToCompatRegistration(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	_ = os.Unsetenv("GC_DOLT_HOST")
	_ = os.Unsetenv("GC_DOLT_PORT")
	_ = os.Unsetenv("GC_DOLT_USER")
	_ = os.Unsetenv("GC_DOLT_PASSWORD")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: canonical-db.example.com
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Host: "compat-db.example.com", Port: 4406})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	_, err := bdRuntimeEnvWithError(cityPath)
	requireErrorContains(t, err, "city_canonical config requires dolt.port")
}

func TestCityRuntimeProcessEnvInvalidCanonicalConfigDoesNotFallbackToCompatRegistration(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	_ = os.Unsetenv("GC_DOLT_HOST")
	_ = os.Unsetenv("GC_DOLT_PORT")
	_ = os.Unsetenv("GC_DOLT_USER")
	_ = os.Unsetenv("GC_DOLT_PASSWORD")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: canonical-db.example.com
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Host: "compat-db.example.com", Port: 4406})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	_, err := cityRuntimeProcessEnvWithError(cityPath)
	requireErrorContains(t, err, "city_canonical config requires dolt.port")
}

func TestBdRuntimeEnvInvalidCityExplicitOriginDoesNotFallbackToCompatRegistration(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	_ = os.Unsetenv("GC_DOLT_HOST")
	_ = os.Unsetenv("GC_DOLT_PORT")
	_ = os.Unsetenv("GC_DOLT_USER")
	_ = os.Unsetenv("GC_DOLT_PASSWORD")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: explicit
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: invalid-db.example.com
dolt.port: 3307
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Host: "compat-db.example.com", Port: 4406})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	_, err := bdRuntimeEnvWithError(cityPath)
	requireErrorContains(t, err, "explicit endpoint origin is invalid for city scope")
}

func TestBdRuntimeEnvInvalidManagedCityConfigDoesNotProjectTrackedEndpoint(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	_ = os.Unsetenv("GC_DOLT_HOST")
	_ = os.Unsetenv("GC_DOLT_PORT")
	_ = os.Unsetenv("GC_DOLT_USER")
	_ = os.Unsetenv("GC_DOLT_PASSWORD")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: stale-db.example.com
dolt.port: 3307
dolt.user: stale-user
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Host: "compat-db.example.com", Port: 4406})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	_, err := bdRuntimeEnvWithError(cityPath)
	requireErrorContains(t, err, "managed city config must not track dolt.host, dolt.port, or dolt.user")
}

func TestBdRuntimeEnvPrefersCanonicalExternalConfigOverCompatRegistration(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "")
	_ = os.Unsetenv("GC_DOLT_HOST")
	t.Setenv("GC_DOLT_PORT", "")
	_ = os.Unsetenv("GC_DOLT_PORT")
	t.Setenv("GC_DOLT_USER", "")
	_ = os.Unsetenv("GC_DOLT_USER")
	t.Setenv("GC_DOLT_PASSWORD", "")
	_ = os.Unsetenv("GC_DOLT_PASSWORD")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: canonical-db.example.com
dolt.port: 3307
dolt.user: canonical-user
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Host: "compat-db.example.com", Port: 4406})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	env := mustBdRuntimeEnv(t, cityPath)
	if got := env["GC_DOLT_HOST"]; got != "canonical-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want canonical host", got)
	}
	if got := env["GC_DOLT_PORT"]; got != "3307" {
		t.Fatalf("GC_DOLT_PORT = %q, want canonical port", got)
	}
	if got := env["GC_DOLT_USER"]; got != "canonical-user" {
		t.Fatalf("GC_DOLT_USER = %q, want canonical user", got)
	}
	for _, key := range []string{"GC_DOLT_HOST", "BEADS_DOLT_SERVER_HOST"} {
		if strings.Contains(env[key], "compat-db.example.com") {
			t.Fatalf("%s should ignore compat host, env = %#v", key, env)
		}
	}
}

func TestBdRuntimeEnvIgnoresAmbientHostPortOverrideOverCanonicalConfig(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "override-db.example.com")
	t.Setenv("GC_DOLT_PORT", "5511")
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("GC_DOLT_PASSWORD", "")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo

gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: canonical-db.example.com
dolt.port: 3307
dolt.user: canonical-user
`), 0o644); err != nil {
		t.Fatal(err)
	}

	env := mustBdRuntimeEnv(t, cityPath)
	if got := env["GC_DOLT_HOST"]; got != "canonical-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want canonical host", got)
	}
	if got := env["GC_DOLT_PORT"]; got != "3307" {
		t.Fatalf("GC_DOLT_PORT = %q, want canonical port", got)
	}
	if got := env["GC_DOLT_USER"]; got != "canonical-user" {
		t.Fatalf("GC_DOLT_USER = %q, want canonical user", got)
	}
	if got := env["BEADS_DOLT_SERVER_HOST"]; got != "canonical-db.example.com" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want canonical host", got)
	}
	if got := env["BEADS_DOLT_SERVER_PORT"]; got != "3307" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want canonical port", got)
	}
}

func TestSessionDoltEnvFallsBackToCompatCityRegistrationWhenCityConfigLacksEndpointAuthority(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Host: "compat-db.example.com", Port: 4406})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	env := mustSessionBackendEnv(t, cityPath, "", nil)
	if got := env["GC_DOLT_HOST"]; got != "compat-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want compat host", got)
	}
	if got := env["GC_DOLT_PORT"]; got != "4406" {
		t.Fatalf("GC_DOLT_PORT = %q, want compat port", got)
	}
}

func TestSessionDoltEnvInheritsCompatCityTargetWhenRigConfigLacksEndpointAuthority(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	rigDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: repo
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Host: "compat-db.example.com", Port: 4406})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	env := mustSessionBackendEnv(t, cityPath, rigDir, []config.Rig{{Name: "repo", Path: rigDir}})
	if got := env["GC_DOLT_HOST"]; got != "compat-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want inherited compat host", got)
	}
	if got := env["GC_DOLT_PORT"]; got != "4406" {
		t.Fatalf("GC_DOLT_PORT = %q, want inherited compat port", got)
	}
}

func TestSessionDoltEnvFallsBackToCompatRigOverrideWhenRigConfigLacksEndpointAuthority(t *testing.T) {
	cityPath := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: repo
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	env := mustSessionBackendEnv(t, cityPath, rigDir, []config.Rig{{Name: "repo", Path: rigDir, DoltHost: "rig-db.example.com", DoltPort: "3308"}})
	if got := env["GC_DOLT_HOST"]; got != "rig-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want explicit rig compat host", got)
	}
	if got := env["GC_DOLT_PORT"]; got != "3308" {
		t.Fatalf("GC_DOLT_PORT = %q, want explicit rig compat port", got)
	}
}

func TestSessionDoltEnvUsesCanonicalRigUser(t *testing.T) {
	cityPath := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: repo
gc.endpoint_origin: explicit
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: rig-db.example.com
dolt.port: 3308
dolt.user: rig-user
`), 0o644); err != nil {
		t.Fatal(err)
	}

	env := mustSessionBackendEnv(t, cityPath, rigDir, []config.Rig{{Name: "repo", Path: rigDir}})
	if got := env["GC_DOLT_HOST"]; got != "rig-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want %q", got, "rig-db.example.com")
	}
	if got := env["GC_DOLT_PORT"]; got != "3308" {
		t.Fatalf("GC_DOLT_PORT = %q, want %q", got, "3308")
	}
	if got := env["GC_DOLT_USER"]; got != "rig-user" {
		t.Fatalf("GC_DOLT_USER = %q, want %q", got, "rig-user")
	}
	if got := env["BEADS_DOLT_SERVER_USER"]; got != "rig-user" {
		t.Fatalf("BEADS_DOLT_SERVER_USER = %q, want %q", got, "rig-user")
	}
}

func TestSessionDoltEnvPrefersCanonicalCityConfigOverCompatRegistration(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: canonical-db.example.com
dolt.port: 3307
dolt.user: canonical-user
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Host: "compat-db.example.com", Port: 4406})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	env := mustSessionBackendEnv(t, cityPath, "", nil)
	if got := env["GC_DOLT_HOST"]; got != "canonical-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want canonical host", got)
	}
	if got := env["GC_DOLT_PORT"]; got != "3307" {
		t.Fatalf("GC_DOLT_PORT = %q, want canonical port", got)
	}
	if got := env["GC_DOLT_USER"]; got != "canonical-user" {
		t.Fatalf("GC_DOLT_USER = %q, want canonical user", got)
	}
	for _, key := range []string{"GC_DOLT_HOST", "BEADS_DOLT_SERVER_HOST"} {
		if strings.Contains(env[key], "compat-db.example.com") {
			t.Fatalf("%s should ignore compat host, env = %#v", key, env)
		}
	}
}

func TestSessionDoltEnvIgnoresAmbientHostPortOverrideOverCanonicalConfig(t *testing.T) {
	t.Setenv("GC_DOLT_HOST", "override-db.example.com")
	t.Setenv("GC_DOLT_PORT", "5511")
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("GC_DOLT_PASSWORD", "")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo

gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: canonical-db.example.com
dolt.port: 3307
dolt.user: canonical-user
`), 0o644); err != nil {
		t.Fatal(err)
	}

	env := mustSessionBackendEnv(t, cityPath, "", nil)
	if got := env["GC_DOLT_HOST"]; got != "canonical-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want canonical host", got)
	}
	if got := env["GC_DOLT_PORT"]; got != "3307" {
		t.Fatalf("GC_DOLT_PORT = %q, want canonical port", got)
	}
	if got := env["GC_DOLT_USER"]; got != "canonical-user" {
		t.Fatalf("GC_DOLT_USER = %q, want canonical user", got)
	}
	if got := env["BEADS_DOLT_SERVER_HOST"]; got != "canonical-db.example.com" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want canonical host", got)
	}
	if got := env["BEADS_DOLT_SERVER_PORT"]; got != "3307" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want canonical port", got)
	}
}

func TestSessionDoltEnvPrefersInheritedCanonicalRigConfigOverCompatRigOverride(t *testing.T) {
	cityPath := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "repo")
	for _, dir := range []string{cityPath, rigDir} {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: canonical-db.example.com
dolt.port: 3307
dolt.user: canonical-user
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: repo
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: stale-db.example.com
dolt.port: 5507
dolt.user: stale-user
`), 0o644); err != nil {
		t.Fatal(err)
	}

	env := mustSessionBackendEnv(t, cityPath, rigDir, []config.Rig{{Name: "repo", Path: rigDir, DoltHost: "compat-rig-db.example.com", DoltPort: "6608"}})
	if got := env["GC_DOLT_HOST"]; got != "canonical-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want inherited canonical host", got)
	}
	if got := env["GC_DOLT_PORT"]; got != "3307" {
		t.Fatalf("GC_DOLT_PORT = %q, want inherited canonical port", got)
	}
	if got := env["GC_DOLT_USER"]; got != "canonical-user" {
		t.Fatalf("GC_DOLT_USER = %q, want inherited canonical user", got)
	}
	for _, forbidden := range []string{"compat-rig-db.example.com", "6608", "stale-db.example.com", "5507", "stale-user"} {
		for key, value := range env {
			if strings.Contains(value, forbidden) {
				t.Fatalf("%s should ignore non-canonical inherited value %q, env = %#v", key, forbidden, env)
			}
		}
	}
}

func TestCityRuntimeProcessEnvIncludesDoltHost(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT_HOST", "mini2.hippo-tilapia.ts.net")
	t.Setenv("GC_DOLT_PORT", "3307")
	t.Setenv("GC_DOLT_USER", "agent")
	t.Setenv("GC_DOLT_PASSWORD", "s3cret")
	t.Setenv("GC_DOLT", "skip")

	cityPath := t.TempDir()
	env := mustCityRuntimeProcessEnv(t, cityPath)

	var foundHost, foundPort, foundBeadsHost, foundBeadsPort, foundBeadsUser, foundBeadsPass bool
	for _, entry := range env {
		if strings.HasPrefix(entry, "GC_DOLT_HOST=") {
			foundHost = true
			if got := strings.TrimPrefix(entry, "GC_DOLT_HOST="); got != "mini2.hippo-tilapia.ts.net" {
				t.Errorf("GC_DOLT_HOST = %q, want %q", got, "mini2.hippo-tilapia.ts.net")
			}
		}
		if strings.HasPrefix(entry, "GC_DOLT_PORT=") {
			foundPort = true
			if got := strings.TrimPrefix(entry, "GC_DOLT_PORT="); got != "3307" {
				t.Errorf("GC_DOLT_PORT = %q, want %q", got, "3307")
			}
		}
		if strings.HasPrefix(entry, "BEADS_DOLT_SERVER_HOST=") {
			foundBeadsHost = true
			if got := strings.TrimPrefix(entry, "BEADS_DOLT_SERVER_HOST="); got != "mini2.hippo-tilapia.ts.net" {
				t.Errorf("BEADS_DOLT_SERVER_HOST = %q, want %q", got, "mini2.hippo-tilapia.ts.net")
			}
		}
		if strings.HasPrefix(entry, "BEADS_DOLT_SERVER_PORT=") {
			foundBeadsPort = true
			if got := strings.TrimPrefix(entry, "BEADS_DOLT_SERVER_PORT="); got != "3307" {
				t.Errorf("BEADS_DOLT_SERVER_PORT = %q, want %q", got, "3307")
			}
		}
		if strings.HasPrefix(entry, "BEADS_DOLT_SERVER_USER=") {
			foundBeadsUser = true
			if got := strings.TrimPrefix(entry, "BEADS_DOLT_SERVER_USER="); got != "agent" {
				t.Errorf("BEADS_DOLT_SERVER_USER = %q, want %q", got, "agent")
			}
		}
		if strings.HasPrefix(entry, "BEADS_DOLT_PASSWORD=") {
			foundBeadsPass = true
			if got := strings.TrimPrefix(entry, "BEADS_DOLT_PASSWORD="); got != "s3cret" {
				t.Errorf("BEADS_DOLT_PASSWORD = %q, want %q", got, "s3cret")
			}
		}
	}
	if !foundHost {
		t.Error("GC_DOLT_HOST not found in cityRuntimeProcessEnv output")
	}
	if !foundPort {
		t.Error("GC_DOLT_PORT not found in cityRuntimeProcessEnv output")
	}
	if !foundBeadsHost {
		t.Error("BEADS_DOLT_SERVER_HOST not found in cityRuntimeProcessEnv output")
	}
	if !foundBeadsPort {
		t.Error("BEADS_DOLT_SERVER_PORT not found in cityRuntimeProcessEnv output")
	}
	if !foundBeadsUser {
		t.Error("BEADS_DOLT_SERVER_USER not found in cityRuntimeProcessEnv output")
	}
	if !foundBeadsPass {
		t.Error("BEADS_DOLT_PASSWORD not found in cityRuntimeProcessEnv output")
	}
}

func TestCityRuntimeProcessEnvIncludesCanonicalExternalHostForExecGcBeadsBd(t *testing.T) {
	t.Setenv("GC_BEADS", "exec:/tmp/gc-beads-bd")
	t.Setenv("GC_DOLT_HOST", "ambient.invalid")
	t.Setenv("GC_DOLT_PORT", "9999")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `issue_prefix: gc
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: city-db.example.com
dolt.port: 3307
dolt.user: canonical-user
`
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=city-pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	entries := mustCityRuntimeProcessEnv(t, cityPath)
	got := map[string]string{}
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			got[key] = value
		}
	}
	want := map[string]string{
		"GC_DOLT_HOST":           "city-db.example.com",
		"GC_DOLT_PORT":           "3307",
		"GC_DOLT_USER":           "canonical-user",
		"GC_DOLT_PASSWORD":       "city-pass",
		"BEADS_DOLT_SERVER_HOST": "city-db.example.com",
		"BEADS_DOLT_SERVER_PORT": "3307",
		"BEADS_DOLT_SERVER_USER": "canonical-user",
		"BEADS_DOLT_PASSWORD":    "city-pass",
	}
	for key, wantValue := range want {
		if got[key] != wantValue {
			t.Fatalf("%s = %q, want %q (env=%#v)", key, got[key], wantValue, got)
		}
	}
}

func TestBdRuntimeEnvFallsBackToCompatRegistrationWhenCityConfigLacksEndpointAuthority(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "")
	_ = os.Unsetenv("GC_DOLT_HOST")
	t.Setenv("GC_DOLT_PORT", "")
	_ = os.Unsetenv("GC_DOLT_PORT")
	t.Setenv("GC_DOLT_USER", "")
	_ = os.Unsetenv("GC_DOLT_USER")
	t.Setenv("GC_DOLT_PASSWORD", "")
	_ = os.Unsetenv("GC_DOLT_PASSWORD")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Host: "compat-db.example.com", Port: 4406})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	env := mustBdRuntimeEnv(t, cityPath)
	if got := env["GC_DOLT_HOST"]; got != "compat-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want compat host", got)
	}
	if got := env["GC_DOLT_PORT"]; got != "4406" {
		t.Fatalf("GC_DOLT_PORT = %q, want compat port", got)
	}
}

func TestCityRuntimeProcessEnvFallsBackToCompatRegistrationWhenCityConfigLacksEndpointAuthority(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "")
	_ = os.Unsetenv("GC_DOLT_HOST")
	t.Setenv("GC_DOLT_PORT", "")
	_ = os.Unsetenv("GC_DOLT_PORT")
	t.Setenv("GC_DOLT_USER", "")
	_ = os.Unsetenv("GC_DOLT_USER")
	t.Setenv("GC_DOLT_PASSWORD", "")
	_ = os.Unsetenv("GC_DOLT_PASSWORD")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Host: "compat-db.example.com", Port: 4406})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	env := mustCityRuntimeProcessEnv(t, cityPath)
	got := map[string]string{}
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			got[key] = value
		}
	}
	if got["GC_DOLT_HOST"] != "compat-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want compat host", got["GC_DOLT_HOST"])
	}
	if got["GC_DOLT_PORT"] != "4406" {
		t.Fatalf("GC_DOLT_PORT = %q, want compat port", got["GC_DOLT_PORT"])
	}
}

func TestCityRuntimeProcessEnvPrefersCanonicalExternalConfigOverCompatRegistration(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "")
	_ = os.Unsetenv("GC_DOLT_HOST")
	t.Setenv("GC_DOLT_PORT", "")
	_ = os.Unsetenv("GC_DOLT_PORT")
	t.Setenv("GC_DOLT_USER", "")
	_ = os.Unsetenv("GC_DOLT_USER")
	t.Setenv("GC_DOLT_PASSWORD", "")
	_ = os.Unsetenv("GC_DOLT_PASSWORD")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: canonical-db.example.com
dolt.port: 3307
dolt.user: canonical-user
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Host: "compat-db.example.com", Port: 4406})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	env := mustCityRuntimeProcessEnv(t, cityPath)
	got := map[string]string{}
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			got[key] = value
		}
	}
	if got["GC_DOLT_HOST"] != "canonical-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want canonical host", got["GC_DOLT_HOST"])
	}
	if got["GC_DOLT_PORT"] != "3307" {
		t.Fatalf("GC_DOLT_PORT = %q, want canonical port", got["GC_DOLT_PORT"])
	}
	if got["GC_DOLT_USER"] != "canonical-user" {
		t.Fatalf("GC_DOLT_USER = %q, want canonical user", got["GC_DOLT_USER"])
	}
	if got["BEADS_DOLT_SERVER_HOST"] != "canonical-db.example.com" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want canonical host", got["BEADS_DOLT_SERVER_HOST"])
	}
	if got["BEADS_DOLT_SERVER_PORT"] != "3307" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want canonical port", got["BEADS_DOLT_SERVER_PORT"])
	}
	if got["BEADS_DOLT_SERVER_USER"] != "canonical-user" {
		t.Fatalf("BEADS_DOLT_SERVER_USER = %q, want canonical user", got["BEADS_DOLT_SERVER_USER"])
	}
}

func TestMergeRuntimeEnvIncludesDoltHost(t *testing.T) {
	parent := []string{
		"BEADS_DOLT_SERVER_HOST=old-beads-host",
		"BEADS_DOLT_SERVER_PORT=9999",
		"PATH=/usr/bin",
		"GC_DOLT_HOST=old-host",
	}
	overrides := map[string]string{
		"BEADS_DOLT_SERVER_HOST": "new-host.example.com",
		"BEADS_DOLT_SERVER_PORT": "3307",
		"GC_DOLT_HOST":           "new-host.example.com",
	}
	result := mergeRuntimeEnv(parent, overrides)

	var count, beadsCount, beadsPortCount int
	for _, entry := range result {
		if strings.HasPrefix(entry, "GC_DOLT_HOST=") {
			count++
			if got := strings.TrimPrefix(entry, "GC_DOLT_HOST="); got != "new-host.example.com" {
				t.Errorf("GC_DOLT_HOST = %q, want %q", got, "new-host.example.com")
			}
		}
		if strings.HasPrefix(entry, "BEADS_DOLT_SERVER_HOST=") {
			beadsCount++
			if got := strings.TrimPrefix(entry, "BEADS_DOLT_SERVER_HOST="); got != "new-host.example.com" {
				t.Errorf("BEADS_DOLT_SERVER_HOST = %q, want %q", got, "new-host.example.com")
			}
		}
		if strings.HasPrefix(entry, "BEADS_DOLT_SERVER_PORT=") {
			beadsPortCount++
			if got := strings.TrimPrefix(entry, "BEADS_DOLT_SERVER_PORT="); got != "3307" {
				t.Errorf("BEADS_DOLT_SERVER_PORT = %q, want %q", got, "3307")
			}
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 GC_DOLT_HOST entry, got %d", count)
	}
	if beadsCount != 1 {
		t.Errorf("expected exactly 1 BEADS_DOLT_SERVER_HOST entry, got %d", beadsCount)
	}
	if beadsPortCount != 1 {
		t.Errorf("expected exactly 1 BEADS_DOLT_SERVER_PORT entry, got %d", beadsPortCount)
	}
}

func TestBdRuntimeEnvLocalHostNoHostKey(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "")
	_ = os.Unsetenv("GC_DOLT_HOST")
	t.Setenv("GC_DOLT_PORT", "")
	_ = os.Unsetenv("GC_DOLT_PORT")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "stale.example.com")

	cityPath := t.TempDir()
	env := mustBdRuntimeEnv(t, cityPath)

	if _, ok := env["GC_DOLT_HOST"]; ok {
		t.Error("GC_DOLT_HOST should not be present when not configured")
	}
	if _, ok := env["BEADS_DOLT_SERVER_HOST"]; ok {
		t.Error("BEADS_DOLT_SERVER_HOST should not be present when not configured")
	}
}

func TestBdRuntimeEnvManagedCityProjectsHostOverride(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	host := reachableNonLoopbackHost(t)
	t.Setenv("GC_DOLT_HOST", host)
	t.Setenv("GC_DOLT_PORT", "9999")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "stale.example.com")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "9999")

	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }() //nolint:errcheck // test cleanup
	port := ln.Addr().(*net.TCPAddr).Port
	if err := writeDoltState(cityDir, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      port,
		DataDir:   filepath.Join(cityDir, ".beads", "dolt"),
		StartedAt: "2026-04-02T08:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}

	env := mustBdRuntimeEnv(t, cityDir)
	wantPort := strconv.Itoa(port)
	if got := env["GC_DOLT_HOST"]; got != host {
		t.Fatalf("GC_DOLT_HOST = %q, want host override", got)
	}
	if got := env["BEADS_DOLT_SERVER_HOST"]; got != host {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want host override", got)
	}
	if got := env["GC_DOLT_PORT"]; got != wantPort {
		t.Fatalf("GC_DOLT_PORT = %q, want runtime port %q", got, wantPort)
	}
	if got := env["BEADS_DOLT_SERVER_PORT"]; got != wantPort {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want runtime port %q", got, wantPort)
	}
}

func TestOpenStoreAtForCityUsesScopeLocalFileStore(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	rigDir := filepath.Join(t.TempDir(), "test-external")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "file")
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(rigDir); err != nil {
		t.Fatal(err)
	}

	rigResult, err := openStoreResultAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}
	if rigResult.Diagnostic.Store != "FileStore" {
		t.Fatalf("rig beads_store = %q, want FileStore", rigResult.Diagnostic.Store)
	}
	rigStore := rigResult.Store
	if _, err := rigStore.Create(beads.Bead{Title: "rig bead", Type: "task"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	cityStore, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	cityList, err := cityStore.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("city List after rig create: %v", err)
	}
	if len(cityList) != 0 {
		t.Fatalf("city store should stay empty after rig create, got %d bead(s)", len(cityList))
	}

	if _, err := cityStore.Create(beads.Bead{Title: "city bead", Type: "task"}); err != nil {
		t.Fatalf("city Create: %v", err)
	}
	rigList, err := rigStore.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("rig List after city create: %v", err)
	}
	if len(rigList) != 1 || rigList[0].Title != "rig bead" {
		t.Fatalf("rig store should still contain only its own bead, got %#v", rigList)
	}
}

func TestOpenStoreAtForCityUsesRigBdStoreUnderFileBackedCity(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "file"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fe"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"embedded","dolt_database":"fe"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := openStoreResultAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}
	if result.Diagnostic.Store != "BdStore" {
		t.Fatalf("beads_store = %q, want BdStore", result.Diagnostic.Store)
	}
	store := underlyingPolicyStoreForTest(result.Store)
	if _, ok := store.(*beads.BdStore); !ok {
		t.Fatalf("openStoreAtForCity(rig) returned %T, want *beads.BdStore", store)
	}
}

func TestOpenStoreAtForCityUsesRigFileStoreUnderBdBackedCity(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(filepath.Join(rigDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "bd"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fe"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".gc", "beads.json"), []byte("{\"seq\":0,\"beads\":[]}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}
	store = underlyingPolicyStoreForTest(store)
	if _, ok := store.(*beads.FileStore); !ok {
		t.Fatalf("openStoreAtForCity(rig) returned %T, want *beads.FileStore", store)
	}
}

func TestOpenStoreAtForCityLegacyEmptyFileCityDoesNotFailOrCreateRigState(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	rigDir := filepath.Join(t.TempDir(), "legacy-rig")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "file")

	rigStore, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}
	rigList, err := rigStore.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("rig List: %v", err)
	}
	if len(rigList) != 0 {
		t.Fatalf("empty legacy file city should list zero beads, got %#v", rigList)
	}
	if _, err := os.Stat(filepath.Join(rigDir, ".gc")); !os.IsNotExist(err) {
		t.Fatalf("legacy empty rig open should not create rig .gc state, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(cityDir, ".gc", "beads.json")); !os.IsNotExist(err) {
		t.Fatalf("legacy empty city open should not create beads.json, stat err = %v", err)
	}
}

func TestOpenStoreAtForCityPreservesLegacySharedFileStoreWithoutCreatingRigState(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	rigDir := filepath.Join(t.TempDir(), "legacy-rig")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "file")

	legacyCityStore, err := openScopeLocalFileStore(cityDir)
	if err != nil {
		t.Fatalf("openScopeLocalFileStore(city): %v", err)
	}
	if _, err := legacyCityStore.Create(beads.Bead{Title: "legacy city bead", Type: "task"}); err != nil {
		t.Fatalf("legacy city Create: %v", err)
	}

	rigStore, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}
	rigList, err := rigStore.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("rig List: %v", err)
	}
	if len(rigList) != 1 || rigList[0].Title != "legacy city bead" {
		t.Fatalf("rig store should read legacy shared city data, got %#v", rigList)
	}
	if _, err := os.Stat(filepath.Join(rigDir, ".gc")); !os.IsNotExist(err) {
		t.Fatalf("legacy rig open should not create rig .gc state, stat err = %v", err)
	}
}

func TestMergeRuntimeEnvReplacesInheritedRuntimeKeys(t *testing.T) {
	env := mergeRuntimeEnv([]string{
		"BEADS_DIR=/rig/.beads",
		"BEADS_DOLT_SERVER_PORT=9999",
		"PATH=/bin",
		"GC_CITY_PATH=/wrong",
		"GC_DOLT_PORT=9999",
		"GC_PACK_STATE_DIR=/wrong/.gc/runtime/packs/dolt",
		"GC_RIG=demo",
		"GC_RIG_ROOT=/rig",
	}, map[string]string{
		"BEADS_DOLT_SERVER_PORT": "31364",
		"GC_CITY_PATH":           "/city",
		"GC_DOLT_PORT":           "31364",
	})

	got := make(map[string]string)
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			got[key] = value
		}
	}

	if got["GC_CITY_PATH"] != "/city" {
		t.Fatalf("GC_CITY_PATH = %q, want %q", got["GC_CITY_PATH"], "/city")
	}
	if got["GC_DOLT_PORT"] != "31364" {
		t.Fatalf("GC_DOLT_PORT = %q, want %q", got["GC_DOLT_PORT"], "31364")
	}
	if got["BEADS_DOLT_SERVER_PORT"] != "31364" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want %q", got["BEADS_DOLT_SERVER_PORT"], "31364")
	}
	if _, ok := got["BEADS_DIR"]; ok {
		t.Fatalf("BEADS_DIR should be removed, env = %#v", got)
	}
	if _, ok := got["GC_PACK_STATE_DIR"]; ok {
		t.Fatalf("GC_PACK_STATE_DIR should be removed, env = %#v", got)
	}
	if _, ok := got["GC_RIG"]; ok {
		t.Fatalf("GC_RIG should be removed, env = %#v", got)
	}
	if _, ok := got["GC_RIG_ROOT"]; ok {
		t.Fatalf("GC_RIG_ROOT should be removed, env = %#v", got)
	}
}

func TestBdCommandRunnerForCityPinsCityStoreEnv(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "file")
	t.Setenv("BEADS_DIR", "/rig/.beads")
	t.Setenv("GC_RIG", "demo-rig")
	t.Setenv("GC_RIG_ROOT", "/rig")

	runner := bdCommandRunnerForCity(cityDir)
	out, err := runner(cityDir, "sh", "-c", `printf '%s\n%s\n%s\n%s\n' "$GC_CITY_PATH" "$BEADS_DIR" "$GC_RIG" "$GC_RIG_ROOT"`)
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(string(out), "\n")
	if len(lines) != 5 {
		t.Fatalf("lines = %q, want 5 lines including trailing newline", string(out))
	}
	lines = lines[:4]
	if len(lines) != 4 {
		t.Fatalf("lines = %q, want 4 lines", string(out))
	}
	if lines[0] != cityDir {
		t.Fatalf("GC_CITY_PATH = %q, want %q", lines[0], cityDir)
	}
	if lines[1] != filepath.Join(cityDir, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want %q", lines[1], filepath.Join(cityDir, ".beads"))
	}
	if lines[2] != "" {
		t.Fatalf("GC_RIG = %q, want empty", lines[2])
	}
	if lines[3] != "" {
		t.Fatalf("GC_RIG_ROOT = %q, want empty", lines[3])
	}
}

func TestBdCommandRunnerForCityClearsAmbientDoltEnvWhenManagedRuntimeUnavailable(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT_HOST", "ambient.invalid")
	t.Setenv("GC_DOLT_PORT", "9999")
	t.Setenv("GC_DOLT_USER", "ambient-user")
	t.Setenv("GC_DOLT_PASSWORD", "ambient-pass")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "ambient.invalid")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "9999")
	t.Setenv("BEADS_DOLT_SERVER_USER", "ambient-user")
	t.Setenv("BEADS_DOLT_PASSWORD", "ambient-pass")

	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte(`issue_prefix: gc
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	runner := bdCommandRunnerForCity(cityDir)
	out, err := runner(cityDir, "sh", "-c", `printf '%s\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n' "$GC_DOLT_HOST" "$GC_DOLT_PORT" "$GC_DOLT_USER" "$GC_DOLT_PASSWORD" "$BEADS_DOLT_SERVER_HOST" "$BEADS_DOLT_SERVER_PORT" "$BEADS_DOLT_SERVER_USER" "$BEADS_DOLT_PASSWORD"`)
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(string(out), "\n")
	if len(lines) != 9 {
		t.Fatalf("lines = %q, want 9 lines including trailing newline", string(out))
	}
	for i, name := range []string{
		"GC_DOLT_HOST",
		"GC_DOLT_PORT",
		"GC_DOLT_USER",
		"GC_DOLT_PASSWORD",
		"BEADS_DOLT_SERVER_HOST",
		"BEADS_DOLT_SERVER_PORT",
		"BEADS_DOLT_SERVER_USER",
		"BEADS_DOLT_PASSWORD",
	} {
		if lines[i] != "" {
			t.Fatalf("%s = %q, want empty when managed runtime is unavailable", name, lines[i])
		}
	}
}

// This test exercises the shared bd opener path for a rig-scoped store.
// It verifies that the opener and runner pick up the rig's canonical
// Dolt target instead of falling back to the city-scoped opener.
func TestOpenStoreAtForCityUsesRigScopedDoltConfigWithoutProcessEnvSync(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte(`issue_prefix: gc
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.host: city-db.example.com
dolt.port: 3307
`), 0o644); err != nil {
		t.Fatal(err)
	}

	rigDir := filepath.Join(t.TempDir(), "my-rig")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: myrig
gc.endpoint_origin: explicit
gc.endpoint_status: verified
dolt.host: rig-db.example.com
dolt.port: 3307
`), 0o644); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	capture := filepath.Join(t.TempDir(), "bd-env.txt")
	script := filepath.Join(binDir, "bd")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
set -eu
{
  printf 'GC_DOLT_HOST=%s\n' "${GC_DOLT_HOST:-}"
  printf 'GC_DOLT_PORT=%s\n' "${GC_DOLT_PORT:-}"
  printf 'BEADS_DOLT_SERVER_HOST=%s\n' "${BEADS_DOLT_SERVER_HOST:-}"
  printf 'BEADS_DOLT_SERVER_PORT=%s\n' "${BEADS_DOLT_SERVER_PORT:-}"
  printf 'BEADS_DIR=%s\n' "${BEADS_DIR:-}"
  printf 'GC_RIG_ROOT=%s\n' "${GC_RIG_ROOT:-}"
} > "${CAPTURE_PATH}"
exit 0
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CAPTURE_PATH", capture)
	t.Setenv("BEADS_DOLT_SERVER_HOST", "stale-city.example.com")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "9999")

	store, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	store = underlyingPolicyStoreForTest(store)
	bdStore, ok := store.(*beads.BdStore)
	if !ok {
		t.Fatalf("openStoreAtForCity returned %T, want *beads.BdStore", store)
	}
	if err := bdStore.Init("myrig", "", ""); err != nil {
		t.Fatalf("bd store init: %v", err)
	}

	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			got[key] = value
		}
	}
	if got["GC_DOLT_HOST"] != "rig-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want %q", got["GC_DOLT_HOST"], "rig-db.example.com")
	}
	if got["GC_DOLT_PORT"] != "3307" {
		t.Fatalf("GC_DOLT_PORT = %q, want %q", got["GC_DOLT_PORT"], "3307")
	}
	if got["BEADS_DOLT_SERVER_HOST"] != "rig-db.example.com" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want %q", got["BEADS_DOLT_SERVER_HOST"], "rig-db.example.com")
	}
	if got["BEADS_DOLT_SERVER_PORT"] != "3307" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want %q", got["BEADS_DOLT_SERVER_PORT"], "3307")
	}
	if got["BEADS_DIR"] != filepath.Join(rigDir, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want %q", got["BEADS_DIR"], filepath.Join(rigDir, ".beads"))
	}
	if got["GC_RIG_ROOT"] != rigDir {
		t.Fatalf("GC_RIG_ROOT = %q, want %q", got["GC_RIG_ROOT"], rigDir)
	}
}

func TestNativeDoltOpenEnvForScopeUsesRigScopedDoltConfig(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte(`issue_prefix: gc
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.host: city-db.example.com
dolt.port: 3307
`), 0o644); err != nil {
		t.Fatal(err)
	}

	rigDir := filepath.Join(t.TempDir(), "my-rig")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: myrig
gc.endpoint_origin: explicit
gc.endpoint_status: verified
dolt.host: rig-db.example.com
dolt.port: 4407
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Rigs: []config.Rig{{Name: "my-rig", Path: rigDir}}}

	env, err := nativeDoltOpenEnvForScope(cityDir, cfg, rigDir)
	if err != nil {
		t.Fatalf("nativeDoltOpenEnvForScope: %v", err)
	}
	if got := env["BEADS_DOLT_SERVER_HOST"]; got != "rig-db.example.com" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want rig scoped host", got)
	}
	if got := env["BEADS_DOLT_SERVER_PORT"]; got != "4407" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want rig scoped port", got)
	}
	if got := env["BEADS_DIR"]; got != filepath.Join(rigDir, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want rig scoped beads dir", got)
	}
}

func TestBdRuntimeEnvForRigUsesCanonicalManagedRigTarget(t *testing.T) {
	t.Setenv("GC_DOLT_HOST", "")
	_ = os.Unsetenv("GC_DOLT_HOST")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "stale.example.com")

	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }() //nolint:errcheck // test cleanup

	if err := writeDoltState(cityDir, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      ln.Addr().(*net.TCPAddr).Port,
		DataDir:   filepath.Join(cityDir, ".beads", "dolt"),
		StartedAt: "2026-04-02T08:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}

	rigDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: repo
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "dolt-server.port"), []byte("31364"), 0o644); err != nil {
		t.Fatal(err)
	}

	env, err := bdRuntimeEnvForRigWithError(cityDir, &config.City{Rigs: []config.Rig{{Name: "repo", Path: rigDir}}}, rigDir)
	if err != nil {
		t.Fatalf("bdRuntimeEnvForRigWithError() error = %v", err)
	}
	wantPort := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	if got := env["GC_DOLT_PORT"]; got != wantPort {
		t.Fatalf("GC_DOLT_PORT = %q, want %q", got, wantPort)
	}
	if got := env["BEADS_DOLT_SERVER_PORT"]; got != wantPort {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want %q", got, wantPort)
	}
	if got := env["GC_DOLT_HOST"]; got != "" {
		t.Fatalf("GC_DOLT_HOST = %q, want empty for managed target", got)
	}
	if got := env["BEADS_DOLT_SERVER_HOST"]; got != "" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want empty for managed target", got)
	}
}

func TestBdRuntimeEnvForRigInheritedManagedCityProjectsHostOverride(t *testing.T) {
	host := reachableNonLoopbackHost(t)
	t.Setenv("GC_DOLT_HOST", host)
	t.Setenv("GC_DOLT_PORT", "9999")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "stale.example.com")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "9999")

	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }() //nolint:errcheck // test cleanup
	port := ln.Addr().(*net.TCPAddr).Port
	if err := writeDoltState(cityDir, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      port,
		DataDir:   filepath.Join(cityDir, ".beads", "dolt"),
		StartedAt: "2026-04-02T08:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}

	rigDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: repo
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	env := mustBdRuntimeEnvForRig(t, cityDir, &config.City{Rigs: []config.Rig{{Name: "repo", Path: rigDir}}}, rigDir)
	wantPort := strconv.Itoa(port)
	if got := env["GC_DOLT_HOST"]; got != host {
		t.Fatalf("GC_DOLT_HOST = %q, want host override", got)
	}
	if got := env["BEADS_DOLT_SERVER_HOST"]; got != host {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want host override", got)
	}
	if got := env["GC_DOLT_PORT"]; got != wantPort {
		t.Fatalf("GC_DOLT_PORT = %q, want runtime port %q", got, wantPort)
	}
	if got := env["BEADS_DOLT_SERVER_PORT"]; got != wantPort {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want runtime port %q", got, wantPort)
	}
}

func TestBdRuntimeEnvForRigFallsBackToManagedCityPort(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }() //nolint:errcheck // test cleanup

	if err := writeDoltState(cityDir, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      ln.Addr().(*net.TCPAddr).Port,
		DataDir:   filepath.Join(cityDir, ".beads", "dolt"),
		StartedAt: "2026-04-02T08:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}

	rigDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}

	env := mustBdRuntimeEnvForRig(t, cityDir, &config.City{}, rigDir)
	want := strings.TrimSpace(strings.TrimPrefix(ln.Addr().String(), "127.0.0.1:"))
	if got := env["GC_DOLT_PORT"]; got != want {
		t.Fatalf("GC_DOLT_PORT = %q, want %q", got, want)
	}
	if got := env["BEADS_DOLT_SERVER_PORT"]; got != want {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want %q", got, want)
	}
	if got := env["BEADS_DIR"]; got != filepath.Join(rigDir, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want %q", got, filepath.Join(rigDir, ".beads"))
	}
}

func TestBdRuntimeEnvForRigInvalidCanonicalConfigDoesNotFallbackToCompatRegistration(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte(`issue_prefix: demo
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cityDoltConfigs.Store(cityDir, config.DoltConfig{Host: "compat-db.example.com", Port: 4406})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityDir) })

	rigDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: repo
gc.endpoint_origin: explicit
gc.endpoint_status: verified
dolt.auto-start: false
dolt.port: 3308
`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := bdRuntimeEnvForRigWithError(cityDir, &config.City{Rigs: []config.Rig{{Name: "repo", Path: rigDir}}}, rigDir)
	requireErrorContains(t, err, "canonical explicit rig config requires both dolt.host and dolt.port")
}

func TestBdRuntimeEnvForRigInheritedManagedCityConfigDoesNotProjectTrackedEndpoint(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	if err := writeDoltState(cityDir, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      ln.Addr().(*net.TCPAddr).Port,
		DataDir:   filepath.Join(cityDir, ".beads", "dolt"),
		StartedAt: "2026-04-02T08:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}

	rigDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: repo
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: stale-rig-db.example.com
dolt.port: 5507
dolt.user: stale-user
`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = bdRuntimeEnvForRigWithError(cityDir, &config.City{Rigs: []config.Rig{{Name: "repo", Path: rigDir}}}, rigDir)
	requireErrorContains(t, err, "inherited rig under managed city must not track dolt.host, dolt.port, or dolt.user")
}

func TestBdRuntimeEnvForRigPropagatesCityMetadataError(t *testing.T) {
	clearAmbientPostgresEnv(t)
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "metadata.json"), []byte(`{"backend":`), 0o644); err != nil {
		t.Fatal(err)
	}
	rigDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: repo
gc.endpoint_origin: explicit
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: rig-db.example.com
dolt.port: 3308
dolt.user: rig-user
`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := bdRuntimeEnvForRigWithError(cityDir, &config.City{Rigs: []config.Rig{{Name: "repo", Path: rigDir}}}, rigDir)
	if err == nil {
		t.Fatal("bdRuntimeEnvForRigWithError() error = nil, want city metadata parse error")
	}
	if !strings.Contains(err.Error(), "invalid metadata.json") {
		t.Fatalf("bdRuntimeEnvForRigWithError() error = %v, want city metadata parse error", err)
	}
}

func TestBdRuntimeEnvForRigInheritsCompatCityTargetWhenRigConfigLacksEndpointAuthority(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	rigDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: repo
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Host: "compat-db.example.com", Port: 4406})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	env := mustBdRuntimeEnvForRig(t, cityPath, &config.City{Rigs: []config.Rig{{Name: "repo", Path: rigDir}}}, rigDir)
	if got := env["GC_DOLT_HOST"]; got != "compat-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want inherited compat host", got)
	}
	if got := env["GC_DOLT_PORT"]; got != "4406" {
		t.Fatalf("GC_DOLT_PORT = %q, want inherited compat port", got)
	}
}

func TestBdRuntimeEnvForRigInheritsResolvedCityTargetWhenAuthoritativeRigUsesInheritedCity(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT_HOST", "compat-db.example.com")
	t.Setenv("GC_DOLT_PORT", "4406")

	cityPath := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: repo
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	env := mustBdRuntimeEnvForRig(t, cityPath, &config.City{Rigs: []config.Rig{{Name: "repo", Path: rigDir}}}, rigDir)
	if got := env["GC_DOLT_HOST"]; got != "compat-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want inherited resolved city host", got)
	}
	if got := env["GC_DOLT_PORT"]; got != "4406" {
		t.Fatalf("GC_DOLT_PORT = %q, want inherited resolved city port", got)
	}
	if got := env["BEADS_DOLT_SERVER_HOST"]; got != "compat-db.example.com" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want inherited resolved city host", got)
	}
	if got := env["BEADS_DOLT_SERVER_PORT"]; got != "4406" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want inherited resolved city port", got)
	}
}

func TestBdRuntimeEnvForRigPrefersInheritedCanonicalRigConfigOverCompatRigOverride(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: canonical-db.example.com
dolt.port: 3307
dolt.user: canonical-user
`), 0o644); err != nil {
		t.Fatal(err)
	}
	rigDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: repo
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: stale-db.example.com
dolt.port: 5507
dolt.user: stale-user
`), 0o644); err != nil {
		t.Fatal(err)
	}

	env := mustBdRuntimeEnvForRig(t, cityPath, &config.City{Rigs: []config.Rig{{Name: "repo", Path: rigDir, DoltHost: "compat-rig-db.example.com", DoltPort: "6608"}}}, rigDir)
	if got := env["GC_DOLT_HOST"]; got != "canonical-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want inherited canonical host", got)
	}
	if got := env["GC_DOLT_PORT"]; got != "3307" {
		t.Fatalf("GC_DOLT_PORT = %q, want inherited canonical port", got)
	}
	if got := env["GC_DOLT_USER"]; got != "canonical-user" {
		t.Fatalf("GC_DOLT_USER = %q, want inherited canonical user", got)
	}
	// Check dolt-related keys directly for the forbidden values.
	// Scoping by key (rather than a substring scan across every value
	// including path-shaped ones like GC_RIG_ROOT) avoids false
	// positives when Go's t.TempDir random suffix happens to embed one
	// of the forbidden digit sequences — e.g. tempdir
	// ".../Test..2266660824/002/repo" contains "6608" and caused this
	// test to flake in CI.
	forbiddenByKey := map[string][]string{
		"GC_DOLT_HOST":           {"compat-rig-db.example.com", "stale-db.example.com"},
		"GC_DOLT_PORT":           {"6608", "5507"},
		"GC_DOLT_USER":           {"stale-user"},
		"BEADS_DOLT_SERVER_HOST": {"compat-rig-db.example.com", "stale-db.example.com"},
		"BEADS_DOLT_SERVER_PORT": {"6608", "5507"},
		"BEADS_DOLT_SERVER_USER": {"stale-user"},
	}
	for key, bad := range forbiddenByKey {
		value := env[key]
		for _, forbidden := range bad {
			if value == forbidden {
				t.Fatalf("%s = %q is a non-canonical inherited value; env = %#v", key, forbidden, env)
			}
		}
	}
}

func TestBdRuntimeEnvForRigPrefersExplicitRigDoltConfigOverManagedCity(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }() //nolint:errcheck // test cleanup

	if err := writeDoltState(cityDir, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      ln.Addr().(*net.TCPAddr).Port,
		DataDir:   filepath.Join(cityDir, ".beads", "dolt"),
		StartedAt: "2026-04-02T08:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}

	rigDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Rigs: []config.Rig{{
			Name:     "repo",
			Path:     rigDir,
			DoltHost: "rig-db.example.com",
			DoltPort: "3307",
		}},
	}

	env := mustBdRuntimeEnvForRig(t, cityDir, cfg, rigDir)
	if got := env["GC_DOLT_HOST"]; got != "rig-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want %q", got, "rig-db.example.com")
	}
	if got := env["GC_DOLT_PORT"]; got != "3307" {
		t.Fatalf("GC_DOLT_PORT = %q, want %q", got, "3307")
	}
	if got := env["BEADS_DOLT_SERVER_HOST"]; got != "rig-db.example.com" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want %q", got, "rig-db.example.com")
	}
	if got := env["BEADS_DOLT_SERVER_PORT"]; got != "3307" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want %q", got, "3307")
	}
	if got := env["BEADS_DIR"]; got != filepath.Join(rigDir, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want %q", got, filepath.Join(rigDir, ".beads"))
	}
	if got := env["GC_RIG"]; got != "repo" {
		t.Fatalf("GC_RIG = %q, want %q", got, "repo")
	}
	if got := env["GC_RIG_ROOT"]; got != rigDir {
		t.Fatalf("GC_RIG_ROOT = %q, want %q", got, rigDir)
	}
}

func TestBdRuntimeEnvAlwaysIncludesBeadsDoltServerPort(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	// No host/port configured — BEADS_DOLT_SERVER_PORT should still be
	// present (empty) as defense-in-depth against inherited env leakage.
	_ = os.Unsetenv("GC_DOLT_HOST")
	_ = os.Unsetenv("GC_DOLT_PORT")

	cityPath := t.TempDir()
	env := mustBdRuntimeEnv(t, cityPath)

	val, ok := env["BEADS_DOLT_SERVER_PORT"]
	if !ok {
		t.Fatal("BEADS_DOLT_SERVER_PORT must always be present in bdRuntimeEnv output")
	}
	if val != "" {
		t.Errorf("BEADS_DOLT_SERVER_PORT = %q, want empty (no port configured)", val)
	}
}

func TestDoltAutoStartSuppressedInAllEnvPaths(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")

	cityPath := t.TempDir()

	t.Run("bdRuntimeEnv", func(t *testing.T) {
		env := mustBdRuntimeEnv(t, cityPath)
		if got := env["BEADS_DOLT_AUTO_START"]; got != "0" {
			t.Errorf("BEADS_DOLT_AUTO_START = %q, want %q", got, "0")
		}
	})

	t.Run("bdRuntimeEnvForRig", func(t *testing.T) {
		rigDir := filepath.Join(t.TempDir(), "rig")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}
		env := mustBdRuntimeEnvForRig(t, cityPath, &config.City{}, rigDir)
		if got := env["BEADS_DOLT_AUTO_START"]; got != "0" {
			t.Errorf("BEADS_DOLT_AUTO_START = %q, want %q", got, "0")
		}
	})

	t.Run("sessionDoltEnv", func(t *testing.T) {
		env := mustSessionBackendEnv(t, cityPath, "", nil)
		if got := env["BEADS_DOLT_AUTO_START"]; got != "0" {
			t.Errorf("BEADS_DOLT_AUTO_START = %q, want %q", got, "0")
		}
	})
}

// ── cityForStoreDir boundary tests ──────────────────────────────────

func TestCityForStoreDirHonoursGCCity(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))

	cityDir := filepath.Join(homeDir, "mycity")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"mine\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// GC_CITY points to the exact city root — should resolve via
	// validateCityPath without walk-up, even when the store dir is
	// outside bounded discovery range.
	t.Setenv("GC_CITY", cityDir)
	outsideDir := t.TempDir()
	got := cityForStoreDir(outsideDir)
	if canonicalTestPath(got) != canonicalTestPath(cityDir) {
		t.Errorf("cityForStoreDir(%q) = %q, want %q (from GC_CITY)", outsideDir, got, cityDir)
	}
}

func TestCityForStoreDirHonoursGCCityOverDiscoveredCity(t *testing.T) {
	// Ambient store resolution honors an explicit city env before
	// filesystem discovery. Callers that have already resolved --city must
	// pass that city through directly instead of using this helper.
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))

	envCity := filepath.Join(homeDir, "envcity")
	if err := os.MkdirAll(filepath.Join(envCity, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(envCity, "city.toml"), []byte("[workspace]\nname = \"env\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY", envCity)

	dirCity := filepath.Join(homeDir, "dircity")
	if err := os.MkdirAll(filepath.Join(dirCity, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirCity, "city.toml"), []byte("[workspace]\nname = \"dir\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := cityForStoreDir(dirCity)
	if canonicalTestPath(got) != canonicalTestPath(envCity) {
		t.Errorf("cityForStoreDir(%q) = %q, want %q (GC_CITY should win over discovered city %q)", dirCity, got, envCity, dirCity)
	}
}

func TestOpenCityStoreAtUsesExplicitCityOverGCCity(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))
	t.Setenv("GC_BEADS", "bd")

	envCity := filepath.Join(homeDir, "envcity")
	if err := os.MkdirAll(filepath.Join(envCity, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(envCity, "city.toml"), []byte("[workspace]\nname = \"env\"\nprefix = \"env\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY", envCity)

	explicitCity := filepath.Join(homeDir, "explicit")
	if err := os.MkdirAll(filepath.Join(explicitCity, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(explicitCity, "city.toml"), []byte("[workspace]\nname = \"explicit\"\nprefix = \"ex\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := openCityStoreAt(explicitCity)
	if err != nil {
		t.Fatalf("openCityStoreAt(%q): %v", explicitCity, err)
	}
	store = underlyingPolicyStoreForTest(store)
	bdStore, ok := store.(*beads.BdStore)
	if !ok {
		t.Fatalf("openCityStoreAt(%q) returned %T, want *beads.BdStore", explicitCity, store)
	}
	if got := bdStore.IDPrefix(); got != "ex" {
		t.Fatalf("IDPrefix() = %q, want explicit city prefix %q", got, "ex")
	}
}

func TestCityForStoreDirFallsBackToFindCity(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))

	// Unset GC_CITY so cityForStoreDir falls back to findCity.
	t.Setenv("GC_CITY", "")

	cityDir := filepath.Join(homeDir, "projects", "alpha")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"alpha\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Store dir is inside the city — findCity should discover it.
	storeDir := filepath.Join(cityDir, "rigs", "repo")
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		t.Fatal(err)
	}

	got := cityForStoreDir(storeDir)
	if canonicalTestPath(got) != canonicalTestPath(cityDir) {
		t.Errorf("cityForStoreDir(%q) = %q, want %q (from findCity)", storeDir, got, cityDir)
	}
}

func TestCityForStoreDirFallsBackToDirWhenNoCityFound(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))
	t.Setenv("GC_CITY", "")

	// No city.toml anywhere — cityForStoreDir should return dir as fallback.
	noCity := filepath.Join(homeDir, "nocity", "deep")
	if err := os.MkdirAll(noCity, 0o755); err != nil {
		t.Fatal(err)
	}

	got := cityForStoreDir(noCity)
	if canonicalTestPath(got) != canonicalTestPath(noCity) {
		t.Errorf("cityForStoreDir(%q) = %q, want same dir as fallback", noCity, got)
	}
}

func TestBdRuntimeEnvUsesStoreLocalBeadsEnvPassword(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "")
	t.Setenv("GC_DOLT_PORT", "")
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("GC_DOLT_PASSWORD", "")
	t.Setenv("BEADS_CREDENTIALS_FILE", "")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: db.example.com
dolt.port: 3307
dolt.user: canonical-user
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=city-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	env := mustBdRuntimeEnv(t, cityPath)
	if got := env["GC_DOLT_PASSWORD"]; got != "city-secret" {
		t.Fatalf("GC_DOLT_PASSWORD = %q, want %q", got, "city-secret")
	}
	if got := env["BEADS_DOLT_PASSWORD"]; got != "city-secret" {
		t.Fatalf("BEADS_DOLT_PASSWORD = %q, want %q", got, "city-secret")
	}
}

func TestBdRuntimeEnvPrefersProcessPasswordOverStoreAndCredentials(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "")
	t.Setenv("GC_DOLT_PORT", "")
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("GC_DOLT_PASSWORD", "override-secret")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: db.example.com
dolt.port: 3307
dolt.user: canonical-user
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=city-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	credentialsPath := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(credentialsPath, []byte("[db.example.com:3307]\npassword=credentials-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEADS_CREDENTIALS_FILE", credentialsPath)

	env := mustBdRuntimeEnv(t, cityPath)
	if got := env["GC_DOLT_PASSWORD"]; got != "override-secret" {
		t.Fatalf("GC_DOLT_PASSWORD = %q, want %q", got, "override-secret")
	}
	if got := env["BEADS_DOLT_PASSWORD"]; got != "override-secret" {
		t.Fatalf("BEADS_DOLT_PASSWORD = %q, want %q", got, "override-secret")
	}
	if got := env["BEADS_CREDENTIALS_FILE"]; got != credentialsPath {
		t.Fatalf("BEADS_CREDENTIALS_FILE = %q, want %q", got, credentialsPath)
	}
}

func TestBdRuntimeEnvUsesCredentialsFilePasswordWhenStoreSecretMissing(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "")
	t.Setenv("GC_DOLT_PORT", "")
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("GC_DOLT_PASSWORD", "")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: db.example.com
dolt.port: 3307
dolt.user: canonical-user
`), 0o644); err != nil {
		t.Fatal(err)
	}
	credentialsPath := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(credentialsPath, []byte("[db.example.com:3307]\npassword=credentials-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEADS_CREDENTIALS_FILE", credentialsPath)

	env := mustBdRuntimeEnv(t, cityPath)
	if got := env["GC_DOLT_PASSWORD"]; got != "credentials-secret" {
		t.Fatalf("GC_DOLT_PASSWORD = %q, want %q", got, "credentials-secret")
	}
	if got := env["BEADS_DOLT_PASSWORD"]; got != "credentials-secret" {
		t.Fatalf("BEADS_DOLT_PASSWORD = %q, want %q", got, "credentials-secret")
	}
	if got := env["BEADS_CREDENTIALS_FILE"]; got != credentialsPath {
		t.Fatalf("BEADS_CREDENTIALS_FILE = %q, want %q", got, credentialsPath)
	}
}

func TestBdRuntimeEnvIgnoresAmbientBeadsPasswordWithoutScopedSecret(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "")
	t.Setenv("GC_DOLT_PORT", "")
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("GC_DOLT_PASSWORD", "")
	t.Setenv("BEADS_DOLT_PASSWORD", "external-rig-secret")
	t.Setenv("BEADS_CREDENTIALS_FILE", "")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: city-db.example.com
dolt.port: 3307
dolt.user: canonical-user
`), 0o644); err != nil {
		t.Fatal(err)
	}

	env := mustBdRuntimeEnv(t, cityPath)
	if got := env["GC_DOLT_PASSWORD"]; got != "" {
		t.Fatalf("GC_DOLT_PASSWORD = %q, want empty without scoped secret", got)
	}
	if got := env["BEADS_DOLT_PASSWORD"]; got != "" {
		t.Fatalf("BEADS_DOLT_PASSWORD = %q, want empty without scoped secret", got)
	}
}

func TestSessionDoltEnvInheritedRigUsesCityStorePassword(t *testing.T) {
	t.Setenv("GC_DOLT_HOST", "")
	t.Setenv("GC_DOLT_PORT", "")
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("GC_DOLT_PASSWORD", "")
	t.Setenv("BEADS_CREDENTIALS_FILE", "")

	cityPath := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "repo")
	for _, dir := range []string{cityPath, rigDir} {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: city-db.example.com
dolt.port: 3307
dolt.user: city-user
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=city-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: repo
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: city-db.example.com
dolt.port: 3307
dolt.user: city-user
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=rig-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	env := mustSessionBackendEnv(t, cityPath, rigDir, []config.Rig{{Name: "repo", Path: rigDir}})
	if got := env["GC_DOLT_PASSWORD"]; got != "city-secret" {
		t.Fatalf("GC_DOLT_PASSWORD = %q, want %q", got, "city-secret")
	}
	if got := env["BEADS_DOLT_PASSWORD"]; got != "city-secret" {
		t.Fatalf("BEADS_DOLT_PASSWORD = %q, want %q", got, "city-secret")
	}
}

func TestSessionDoltEnvExplicitRigUsesRigStorePassword(t *testing.T) {
	t.Setenv("GC_DOLT_HOST", "")
	t.Setenv("GC_DOLT_PORT", "")
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("GC_DOLT_PASSWORD", "")
	t.Setenv("BEADS_DOLT_PASSWORD", "")
	t.Setenv("BEADS_CREDENTIALS_FILE", "")

	cityPath := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "repo")
	for _, dir := range []string{cityPath, rigDir} {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: city-db.example.com
dolt.port: 3307
dolt.user: city-user
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=city-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: repo
gc.endpoint_origin: explicit
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: rig-db.example.com
dolt.port: 3308
dolt.user: rig-user
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=rig-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	env := mustSessionBackendEnv(t, cityPath, rigDir, []config.Rig{{Name: "repo", Path: rigDir}})
	if got := env["GC_DOLT_PASSWORD"]; got != "rig-secret" {
		t.Fatalf("GC_DOLT_PASSWORD = %q, want %q", got, "rig-secret")
	}
	if got := env["BEADS_DOLT_PASSWORD"]; got != "rig-secret" {
		t.Fatalf("BEADS_DOLT_PASSWORD = %q, want %q", got, "rig-secret")
	}
}

func TestBdRuntimeEnvForExplicitRigUsesCredentialsFileWhenRigStoreSecretMissing(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "")
	t.Setenv("GC_DOLT_PORT", "")
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("GC_DOLT_PASSWORD", "")
	t.Setenv("BEADS_DOLT_PASSWORD", "")

	cityPath := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "repo")
	for _, dir := range []string{cityPath, rigDir} {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: city-db.example.com
dolt.port: 3307
dolt.user: city-user
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=city-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	credentialsPath := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(credentialsPath, []byte("[rig-db.example.com:3308]\npassword=rig-credentials-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEADS_CREDENTIALS_FILE", credentialsPath)

	env := mustBdRuntimeEnvForRig(t, cityPath, &config.City{Rigs: []config.Rig{{
		Name:     "repo",
		Path:     rigDir,
		DoltHost: "rig-db.example.com",
		DoltPort: "3308",
	}}}, rigDir)
	if got := env["GC_DOLT_PASSWORD"]; got != "rig-credentials-secret" {
		t.Fatalf("GC_DOLT_PASSWORD = %q, want %q", got, "rig-credentials-secret")
	}
	if got := env["BEADS_DOLT_PASSWORD"]; got != "rig-credentials-secret" {
		t.Fatalf("BEADS_DOLT_PASSWORD = %q, want %q", got, "rig-credentials-secret")
	}
}

func TestBdRuntimeEnvForCanonicalExplicitRigUsesCredentialsFileWhenRigStoreSecretMissing(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "")
	t.Setenv("GC_DOLT_PORT", "")
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("GC_DOLT_PASSWORD", "")
	t.Setenv("BEADS_DOLT_PASSWORD", "")

	cityPath := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "repo")
	for _, dir := range []string{cityPath, rigDir} {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: city-db.example.com
dolt.port: 3307
dolt.user: city-user
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=city-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: repo
gc.endpoint_origin: explicit
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: rig-db.example.com
dolt.port: 3308
dolt.user: rig-user
`), 0o644); err != nil {
		t.Fatal(err)
	}
	credentialsPath := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(credentialsPath, []byte("[rig-db.example.com:3308]\npassword=rig-credentials-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEADS_CREDENTIALS_FILE", credentialsPath)

	env := mustBdRuntimeEnvForRig(t, cityPath, &config.City{Rigs: []config.Rig{{
		Name: "repo",
		Path: rigDir,
	}}}, rigDir)
	if got := env["GC_DOLT_HOST"]; got != "rig-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want %q", got, "rig-db.example.com")
	}
	if got := env["GC_DOLT_PASSWORD"]; got != "rig-credentials-secret" {
		t.Fatalf("GC_DOLT_PASSWORD = %q, want %q", got, "rig-credentials-secret")
	}
	if got := env["BEADS_DOLT_PASSWORD"]; got != "rig-credentials-secret" {
		t.Fatalf("BEADS_DOLT_PASSWORD = %q, want %q", got, "rig-credentials-secret")
	}
}

func TestCityRuntimeProcessEnvForwardsBeadsCredentialsFile(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "")
	t.Setenv("GC_DOLT_PORT", "")
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("GC_DOLT_PASSWORD", "")
	t.Setenv("BEADS_DOLT_PASSWORD", "")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: db.example.com
dolt.port: 3307
dolt.user: canonical-user
`), 0o644); err != nil {
		t.Fatal(err)
	}
	credentialsPath := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(credentialsPath, []byte("[db.example.com:3307]\npassword=credentials-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEADS_CREDENTIALS_FILE", credentialsPath)

	entries := mustCityRuntimeProcessEnv(t, cityPath)
	got := map[string]string{}
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			got[key] = value
		}
	}
	if got["BEADS_CREDENTIALS_FILE"] != credentialsPath {
		t.Fatalf("BEADS_CREDENTIALS_FILE = %q, want %q", got["BEADS_CREDENTIALS_FILE"], credentialsPath)
	}
	if got["GC_DOLT_PASSWORD"] != "credentials-secret" {
		t.Fatalf("GC_DOLT_PASSWORD = %q, want %q", got["GC_DOLT_PASSWORD"], "credentials-secret")
	}
	if got["BEADS_DOLT_PASSWORD"] != "credentials-secret" {
		t.Fatalf("BEADS_DOLT_PASSWORD = %q, want %q", got["BEADS_DOLT_PASSWORD"], "credentials-secret")
	}
}

func TestSessionDoltEnvCompatCityFallbackUsesCityStorePassword(t *testing.T) {
	t.Setenv("GC_DOLT_HOST", "")
	t.Setenv("GC_DOLT_PORT", "")
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("GC_DOLT_PASSWORD", "")
	t.Setenv("BEADS_CREDENTIALS_FILE", "")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=city-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Host: "compat-db.example.com", Port: 4406})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	env := mustSessionBackendEnv(t, cityPath, "", nil)
	if got := env["GC_DOLT_PASSWORD"]; got != "city-secret" {
		t.Fatalf("GC_DOLT_PASSWORD = %q, want %q", got, "city-secret")
	}
	if got := env["BEADS_DOLT_PASSWORD"]; got != "city-secret" {
		t.Fatalf("BEADS_DOLT_PASSWORD = %q, want %q", got, "city-secret")
	}
}

func TestSessionDoltEnvCompatRigOverrideUsesRigStorePassword(t *testing.T) {
	t.Setenv("GC_DOLT_HOST", "")
	t.Setenv("GC_DOLT_PORT", "")
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("GC_DOLT_PASSWORD", "")
	t.Setenv("BEADS_CREDENTIALS_FILE", "")

	cityPath := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: repo
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=rig-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	env := mustSessionBackendEnv(t, cityPath, rigDir, []config.Rig{{Name: "repo", Path: rigDir, DoltHost: "rig-db.example.com", DoltPort: "3308"}})
	if got := env["GC_DOLT_PASSWORD"]; got != "rig-secret" {
		t.Fatalf("GC_DOLT_PASSWORD = %q, want %q", got, "rig-secret")
	}
	if got := env["BEADS_DOLT_PASSWORD"]; got != "rig-secret" {
		t.Fatalf("BEADS_DOLT_PASSWORD = %q, want %q", got, "rig-secret")
	}
}

func TestBdTransportRetryableErrorDoesNotTreatCommandTimeoutAsTransportFailure(t *testing.T) {
	env := map[string]string{"GC_DOLT_HOST": ""}
	t.Setenv("GC_BEADS", "bd")
	cityPath := t.TempDir()
	if bdTransportRetryableError(cityPath, cityPath, env, fmt.Errorf("timed out after 120s")) {
		t.Fatal("timed out after 120s should not be treated as transport-retryable")
	}
}

func TestBdTransportTransientDisconnectDoesNotTriggerManagedRecovery(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	origRunner := beadsExecCommandRunnerWithEnv
	origRecover := recoverManagedBDCommand
	t.Cleanup(func() {
		beadsExecCommandRunnerWithEnv = origRunner
		recoverManagedBDCommand = origRecover
	})

	attempts := 0
	recoverCalls := 0
	beadsExecCommandRunnerWithEnv = func(_ map[string]string) beads.CommandRunner {
		return func(_ string, _ string, _ ...string) ([]byte, error) {
			attempts++
			if attempts == 1 {
				return nil, fmt.Errorf("bad connection: use of closed network connection")
			}
			return []byte("ok"), nil
		}
	}
	recoverManagedBDCommand = func(_ string) error {
		recoverCalls++
		return nil
	}

	runner := bdCommandRunnerWithManagedRetry(t.TempDir(), func(_ string) map[string]string {
		return map[string]string{"GC_DOLT_PORT": "3307"}
	})

	out, err := runner(t.TempDir(), "bd", "list", "--json")
	if err != nil {
		t.Fatalf("runner error = %v, want nil", err)
	}
	if string(out) != "ok" {
		t.Fatalf("runner output = %q, want %q", out, "ok")
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if recoverCalls != 0 {
		t.Fatalf("recoverCalls = %d, want 0", recoverCalls)
	}
}

// When bd cannot reach the Dolt server it silently falls back to opening the
// on-disk store and triggers a JSONL auto-import, which manifests as a 2-minute
// timeout rather than a transport error. Treat the auto-import marker as a
// transport failure so the managed-retry path can republish the correct port.
// See gastownhall/gascity#1930.
func TestBdTransportRetryableErrorTreatsAutoImportAsTransportFailure(t *testing.T) {
	env := map[string]string{"GC_DOLT_HOST": ""}
	t.Setenv("GC_BEADS", "bd")
	cityPath := t.TempDir()

	cases := []string{
		"bd create: timed out after 2m0s: auto-importing 1927846 bytes from /foo/.beads/issues.jsonl into empty database...",
		"auto-importing 1899171 bytes from issues.jsonl into empty database",
	}
	for _, msg := range cases {
		if !bdTransportRetryableError(cityPath, cityPath, env, fmt.Errorf("%s", msg)) {
			t.Fatalf("auto-import fallback should be transport-retryable: %q", msg)
		}
	}
}

func TestBdTransportRetryableErrorUsesScopeProviderForMixedRig(t *testing.T) {
	cityPath := t.TempDir()
	_ = writeReachableManagedDoltState(t, cityPath)
	rigDir := filepath.Join(cityPath, "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "file"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: repo
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"repo"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"GC_DOLT_HOST": "", "GC_DOLT_PORT": "3307"}

	if !bdTransportRetryableError(cityPath, rigDir, env, fmt.Errorf("server unreachable at 127.0.0.1:3307")) {
		t.Fatal("bd-backed rig under file-backed city should still be transport-retryable")
	}
}

// Regression for gastownhall/gascity#1930: when bd silently falls back to the
// on-disk store and triggers a JSONL auto-import, the managed-retry path must
// republish the Dolt port and rerun the command.
func TestBdCommandRunnerWithManagedRetryRecoversFromAutoImportFallback(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	origRunner := beadsExecCommandRunnerWithEnv
	origRecover := recoverManagedBDCommand
	t.Cleanup(func() {
		beadsExecCommandRunnerWithEnv = origRunner
		recoverManagedBDCommand = origRecover
	})

	port := "3307"
	attempts := 0
	recoverCalls := 0

	beadsExecCommandRunnerWithEnv = func(env map[string]string) beads.CommandRunner {
		copied := map[string]string{}
		for key, value := range env {
			copied[key] = value
		}
		return func(_ string, _ string, _ ...string) ([]byte, error) {
			attempts++
			if attempts == 1 {
				msg := "timed out after 2m0s: auto-importing 1927846 bytes from /foo/.beads/issues.jsonl into empty database"
				return nil, fmt.Errorf("%s", msg)
			}
			return []byte("ok"), nil
		}
	}
	recoverManagedBDCommand = func(_ string) error {
		recoverCalls++
		port = "3308"
		return nil
	}

	runner := bdCommandRunnerWithManagedRetry(t.TempDir(), func(_ string) map[string]string {
		return map[string]string{"GC_DOLT_PORT": port}
	})

	out, err := runner(t.TempDir(), "bd", "create", "--json", "title")
	if err != nil {
		t.Fatalf("runner error = %v, want nil", err)
	}
	if string(out) != "ok" {
		t.Fatalf("runner output = %q, want %q", out, "ok")
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2 (first auto-import, retry succeeds)", attempts)
	}
	if recoverCalls != 1 {
		t.Fatalf("recoverCalls = %d, want 1", recoverCalls)
	}
}

func TestBdCommandRunnerWithManagedRetryRecoversAndRerunsWithFreshEnv(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	origRunner := beadsExecCommandRunnerWithEnv
	origRecover := recoverManagedBDCommand
	t.Cleanup(func() {
		beadsExecCommandRunnerWithEnv = origRunner
		recoverManagedBDCommand = origRecover
	})

	port := "3307"
	attempts := 0
	recoverCalls := 0
	seenPorts := make([]string, 0, 2)

	beadsExecCommandRunnerWithEnv = func(env map[string]string) beads.CommandRunner {
		copied := map[string]string{}
		for key, value := range env {
			copied[key] = value
		}
		return func(_ string, _ string, _ ...string) ([]byte, error) {
			attempts++
			seenPorts = append(seenPorts, copied["GC_DOLT_PORT"])
			if attempts == 1 {
				return nil, fmt.Errorf("server unreachable at 127.0.0.1:%s", copied["GC_DOLT_PORT"])
			}
			return []byte("ok"), nil
		}
	}
	recoverManagedBDCommand = func(_ string) error {
		recoverCalls++
		port = "3308"
		return nil
	}

	runner := bdCommandRunnerWithManagedRetry(t.TempDir(), func(_ string) map[string]string {
		return map[string]string{
			"GC_DOLT_PORT": port,
		}
	})

	out, err := runner(t.TempDir(), "bd", "list", "--json")
	if err != nil {
		t.Fatalf("runner error = %v, want nil", err)
	}
	if string(out) != "ok" {
		t.Fatalf("runner output = %q, want %q", out, "ok")
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if recoverCalls != 1 {
		t.Fatalf("recoverCalls = %d, want 1", recoverCalls)
	}
	if len(seenPorts) != 2 {
		t.Fatalf("seenPorts = %v, want 2 attempts", seenPorts)
	}
	if seenPorts[0] != "3307" || seenPorts[1] != "3308" {
		t.Fatalf("seenPorts = %v, want [3307 3308]", seenPorts)
	}
}

func TestBdCommandRunnerWithManagedRetryReturnsNilOutputOnRetryEnvError(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	origRunner := beadsExecCommandRunnerWithEnv
	origRecover := recoverManagedBDCommand
	t.Cleanup(func() {
		beadsExecCommandRunnerWithEnv = origRunner
		recoverManagedBDCommand = origRecover
	})

	beadsExecCommandRunnerWithEnv = func(_ map[string]string) beads.CommandRunner {
		return func(_ string, _ string, _ ...string) ([]byte, error) {
			return []byte("stale first-attempt output"), fmt.Errorf("bad connection: use of closed network connection")
		}
	}
	recoverManagedBDCommand = func(_ string) error {
		return nil
	}

	retryEnvErr := fmt.Errorf("retry env failed")
	calls := 0
	runner := bdCommandRunnerWithManagedRetryErr(t.TempDir(), func(_ string) (map[string]string, error) {
		calls++
		if calls == 2 {
			return nil, retryEnvErr
		}
		return map[string]string{"GC_DOLT_PORT": "3307"}, nil
	})

	out, err := runner(t.TempDir(), "bd", "list", "--json")
	if !errors.Is(err, retryEnvErr) {
		t.Fatalf("runner error = %v, want retry env error", err)
	}
	if out != nil {
		t.Fatalf("runner output = %q, want nil after retry env error", out)
	}
}

func TestBdCommandRunnerWithManagedRetryReturnsEnvErrorBeforeMutatingNilEnv(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	envErr := fmt.Errorf("env failed")
	runner := bdCommandRunnerWithManagedRetryErr(t.TempDir(), func(_ string) (map[string]string, error) {
		return nil, envErr
	})

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("runner panicked before returning env error: %v", r)
		}
	}()
	out, err := runner(t.TempDir(), "bd", "list", "--json")
	if !errors.Is(err, envErr) {
		t.Fatalf("runner error = %v, want env error", err)
	}
	if out != nil {
		t.Fatalf("runner output = %q, want nil after env error", out)
	}
}

func TestBdCommandRunnerWithManagedRetrySkipsRecoveryForLoopbackExternalEndpoint(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: 127.0.0.1
dolt.port: 3307
`), 0o644); err != nil {
		t.Fatal(err)
	}

	origRunner := beadsExecCommandRunnerWithEnv
	origRecover := recoverManagedBDCommand
	t.Cleanup(func() {
		beadsExecCommandRunnerWithEnv = origRunner
		recoverManagedBDCommand = origRecover
	})

	attempts := 0
	recoverCalls := 0
	beadsExecCommandRunnerWithEnv = func(env map[string]string) beads.CommandRunner {
		return func(_ string, _ string, _ ...string) ([]byte, error) {
			attempts++
			return nil, fmt.Errorf("server unreachable at 127.0.0.1:%s", env["GC_DOLT_PORT"])
		}
	}
	recoverManagedBDCommand = func(_ string) error {
		recoverCalls++
		return nil
	}

	runner := bdCommandRunnerWithManagedRetry(cityPath, func(_ string) map[string]string {
		return map[string]string{
			"GC_DOLT_HOST": "127.0.0.1",
			"GC_DOLT_PORT": "3307",
		}
	})

	_, err := runner(cityPath, "bd", "list", "--json")
	if err == nil || !strings.Contains(err.Error(), "server unreachable") {
		t.Fatalf("runner error = %v, want transport failure", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if recoverCalls != 0 {
		t.Fatalf("recoverCalls = %d, want 0", recoverCalls)
	}
}

func TestBdCommandRunnerWithManagedRetrySkipsRecoveryForNonLocalManagedOverride(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT_HOST", "host.docker.internal")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	origRunner := beadsExecCommandRunnerWithEnv
	origRecover := recoverManagedBDCommand
	t.Cleanup(func() {
		beadsExecCommandRunnerWithEnv = origRunner
		recoverManagedBDCommand = origRecover
	})

	attempts := 0
	recoverCalls := 0
	beadsExecCommandRunnerWithEnv = func(env map[string]string) beads.CommandRunner {
		return func(_ string, _ string, _ ...string) ([]byte, error) {
			attempts++
			return nil, fmt.Errorf("server unreachable at %s:%s", env["GC_DOLT_HOST"], env["GC_DOLT_PORT"])
		}
	}
	recoverManagedBDCommand = func(_ string) error {
		recoverCalls++
		return nil
	}

	runner := bdCommandRunnerWithManagedRetry(cityPath, func(_ string) map[string]string {
		return map[string]string{
			"GC_DOLT_HOST": "host.docker.internal",
			"GC_DOLT_PORT": "3307",
		}
	})

	_, err := runner(cityPath, "bd", "list", "--json")
	if err == nil || !strings.Contains(err.Error(), "server unreachable") {
		t.Fatalf("runner error = %v, want transport failure", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if recoverCalls != 0 {
		t.Fatalf("recoverCalls = %d, want 0", recoverCalls)
	}
}

func TestBdCommandRunnerWithManagedRetrySkipsRecoveryForUnavailableNonLocalManagedRuntime(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT_HOST", "192.0.2.1")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeDoltState(cityPath, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      3307,
		DataDir:   filepath.Join(cityPath, ".beads", "dolt"),
		StartedAt: "2026-04-02T08:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}

	origRunner := beadsExecCommandRunnerWithEnv
	origRecover := recoverManagedBDCommand
	t.Cleanup(func() {
		beadsExecCommandRunnerWithEnv = origRunner
		recoverManagedBDCommand = origRecover
	})

	attempts := 0
	recoverCalls := 0
	beadsExecCommandRunnerWithEnv = func(env map[string]string) beads.CommandRunner {
		return func(_ string, _ string, _ ...string) ([]byte, error) {
			attempts++
			return nil, fmt.Errorf("server unreachable at %s:%s", env["GC_DOLT_HOST"], env["GC_DOLT_PORT"])
		}
	}
	recoverManagedBDCommand = func(_ string) error {
		recoverCalls++
		return nil
	}

	runner := bdCommandRunnerWithManagedRetry(cityPath, func(_ string) map[string]string {
		return map[string]string{
			"GC_DOLT_HOST": "192.0.2.1",
			"GC_DOLT_PORT": "3307",
		}
	})

	_, err := runner(cityPath, "bd", "list", "--json")
	if err == nil || !strings.Contains(err.Error(), "server unreachable") {
		t.Fatalf("runner error = %v, want transport failure", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if recoverCalls != 0 {
		t.Fatalf("recoverCalls = %d, want 0", recoverCalls)
	}
}

func TestBdRuntimeEnvDoesNotDefaultBeadsActorWhenUnset(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	_ = os.Unsetenv("BEADS_ACTOR")

	cityPath := t.TempDir()
	env := mustBdRuntimeEnv(t, cityPath)

	if _, present := env["BEADS_ACTOR"]; present {
		t.Fatalf("BEADS_ACTOR = %q, want absent for neutral bd runtime env", env["BEADS_ACTOR"])
	}
}

// TestBdRuntimeEnvPreservesInheritedBeadsActor verifies that session
// contexts (template_resolve.go sets BEADS_ACTOR=<sessname>) and exec
// orders (orderExecEnv sets BEADS_ACTOR=order:<name>) are not clobbered by
// the neutral bd runtime env. The key is omitted so the inherited value
// passes through mergeEnv unchanged.
func TestBdRuntimeEnvPreservesInheritedBeadsActor(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("BEADS_ACTOR", "mayor")

	cityPath := t.TempDir()
	env := mustBdRuntimeEnv(t, cityPath)

	if _, present := env["BEADS_ACTOR"]; present {
		t.Fatalf("env[BEADS_ACTOR] = %q, expected key absent so parent value passes through", env["BEADS_ACTOR"])
	}
}

func TestControlBdCommandRunnerDefaultsBeadsActorToControllerWhenUnset(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	_ = os.Unsetenv("BEADS_ACTOR")

	origRunner := beadsExecCommandRunnerWithEnv
	t.Cleanup(func() { beadsExecCommandRunnerWithEnv = origRunner })

	var captured map[string]string
	beadsExecCommandRunnerWithEnv = func(env map[string]string) beads.CommandRunner {
		captured = map[string]string{}
		for key, value := range env {
			captured[key] = value
		}
		return func(_ string, _ string, _ ...string) ([]byte, error) {
			return []byte("ok"), nil
		}
	}

	cityPath := t.TempDir()
	runner := controlBdCommandRunnerForCity(cityPath)
	if _, err := runner(cityPath, "bd", "list", "--json"); err != nil {
		t.Fatalf("control runner error = %v, want nil", err)
	}

	if got := captured["BEADS_ACTOR"]; got != "controller" {
		t.Fatalf("BEADS_ACTOR = %q, want controller for controller-owned bd runner", got)
	}
	if got := captured["BD_EXPORT_AUTO"]; got != "false" {
		t.Fatalf("BD_EXPORT_AUTO = %q, want false", got)
	}
}

// ── PG-backend wiring (slice 3 of PG-auth) ─────────────────────────────

func writePGScopeFixture(t *testing.T, scopeRoot, password string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(scopeRoot, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	meta := `{"backend":"postgres","postgres_host":"db.example.test","postgres_port":"5432","postgres_user":"bd","postgres_database":"beads"}`
	if err := os.WriteFile(filepath.Join(scopeRoot, ".beads", "metadata.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	if password != "" {
		envFile := filepath.Join(scopeRoot, ".beads", ".env")
		if err := os.WriteFile(envFile, []byte("BEADS_POSTGRES_PASSWORD="+password+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func clearAmbientPostgresEnv(t *testing.T) {
	t.Helper()
	for _, key := range projectedPostgresEnvKeys {
		t.Setenv(key, "")
		_ = os.Unsetenv(key)
	}
	t.Setenv("BEADS_CREDENTIALS_FILE", "")
	_ = os.Unsetenv("BEADS_CREDENTIALS_FILE")
	t.Setenv("HOME", t.TempDir())
}

func TestApplyResolvedScopePostgresEnv_HappyPath(t *testing.T) {
	clearAmbientPostgresEnv(t)
	cityPath := t.TempDir()
	scopeRoot := t.TempDir()
	writePGScopeFixture(t, scopeRoot, "devpw")

	env := map[string]string{}
	meta := contract.MetadataState{
		Backend:          "postgres",
		PostgresHost:     "db.example.test",
		PostgresPort:     "5432",
		PostgresUser:     "bd",
		PostgresDatabase: "beads",
	}
	if err := applyResolvedScopePostgresEnv(env, cityPath, scopeRoot, meta); err != nil {
		t.Fatalf("applyResolvedScopePostgresEnv: %v", err)
	}
	want := map[string]string{
		"GC_POSTGRES_PASSWORD":    "devpw",
		"BEADS_POSTGRES_PASSWORD": "devpw",
		"BEADS_POSTGRES_HOST":     "db.example.test",
		"BEADS_POSTGRES_PORT":     "5432",
		"BEADS_POSTGRES_USER":     "bd",
		"BEADS_POSTGRES_DATABASE": "beads",
	}
	for key, value := range want {
		if got := env[key]; got != value {
			t.Errorf("env[%q] = %q, want %q", key, got, value)
		}
	}
}

func TestEmitPostgresCredentialResolved_DedupsWithinProcess(t *testing.T) {
	clearAmbientPostgresEnv(t)

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	scopeA := t.TempDir()
	writePGScopeFixture(t, scopeA, "devpw")
	scopeB := t.TempDir()
	writePGScopeFixture(t, scopeB, "devpw")

	meta := contract.MetadataState{
		Backend:          "postgres",
		PostgresHost:     "db.example.test",
		PostgresPort:     "5432",
		PostgresUser:     "bd",
		PostgresDatabase: "beads",
	}
	for i := 0; i < 10; i++ {
		if err := applyResolvedScopePostgresEnv(map[string]string{}, cityPath, scopeA, meta); err != nil {
			t.Fatalf("scopeA call %d: %v", i, err)
		}
	}
	for i := 0; i < 10; i++ {
		if err := applyResolvedScopePostgresEnv(map[string]string{}, cityPath, scopeB, meta); err != nil {
			t.Fatalf("scopeB call %d: %v", i, err)
		}
	}

	got, err := events.ReadFiltered(
		filepath.Join(cityPath, ".gc", "events.jsonl"),
		events.Filter{Type: events.PostgresCredentialResolved},
	)
	if err != nil {
		t.Fatalf("ReadFiltered pg.credential_resolved: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("pg.credential_resolved count = %d, want 2 (one per distinct scope)", len(got))
	}
}

func TestBdRuntimeEnvForRig_PostgresBackend(t *testing.T) {
	clearAmbientPostgresEnv(t)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	for _, key := range []string{"GC_DOLT_HOST", "GC_DOLT_PORT", "GC_DOLT_USER", "GC_DOLT_PASSWORD"} {
		t.Setenv(key, "")
		_ = os.Unsetenv(key)
	}

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	rigDir := filepath.Join(cityPath, "rigs", "pg")
	writePGScopeFixture(t, rigDir, "devpw")
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: pg
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Rigs: []config.Rig{{Name: "pg", Path: "rigs/pg", Prefix: "pg"}}}

	env, err := bdRuntimeEnvForRigWithError(cityPath, cfg, rigDir)
	if err != nil {
		t.Fatalf("bdRuntimeEnvForRigWithError() error = %v", err)
	}

	wantPG := map[string]string{
		"GC_POSTGRES_PASSWORD":    "devpw",
		"BEADS_POSTGRES_PASSWORD": "devpw",
		"BEADS_POSTGRES_HOST":     "db.example.test",
		"BEADS_POSTGRES_PORT":     "5432",
		"BEADS_POSTGRES_USER":     "bd",
		"BEADS_POSTGRES_DATABASE": "beads",
	}
	for key, value := range wantPG {
		if got := env[key]; got != value {
			t.Errorf("env[%q] = %q, want %q", key, got, value)
		}
	}
	for _, key := range []string{"GC_DOLT_HOST", "GC_DOLT_PORT", "GC_DOLT_USER", "GC_DOLT_PASSWORD", "BEADS_DOLT_PASSWORD", "BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_USER"} {
		if value, ok := env[key]; ok && value != "" {
			t.Errorf("env[%q] = %q, want empty/absent for PG-backed rig", key, value)
		}
	}
}

func TestBdRuntimeEnvCity_PostgresBackend(t *testing.T) {
	clearAmbientPostgresEnv(t)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")

	cityPath := t.TempDir()
	writePGScopeFixture(t, cityPath, "citypw")
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: city
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	env, err := bdRuntimeEnvWithError(cityPath)
	if err != nil {
		t.Fatalf("bdRuntimeEnvWithError() error = %v", err)
	}

	wantPG := map[string]string{
		"GC_POSTGRES_PASSWORD":    "citypw",
		"BEADS_POSTGRES_PASSWORD": "citypw",
		"BEADS_POSTGRES_HOST":     "db.example.test",
		"BEADS_POSTGRES_PORT":     "5432",
		"BEADS_POSTGRES_USER":     "bd",
		"BEADS_POSTGRES_DATABASE": "beads",
	}
	for key, value := range wantPG {
		if got := env[key]; got != value {
			t.Errorf("env[%q] = %q, want %q", key, got, value)
		}
	}
	for _, key := range projectedDoltEnvKeys {
		if value, ok := env[key]; ok && value != "" {
			t.Errorf("env[%q] = %q, want empty/absent for PG-backed city", key, value)
		}
	}
}

func TestBdRuntimeEnvForRig_InheritsCityPostgresBackend(t *testing.T) {
	clearAmbientPostgresEnv(t)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "ambient-dolt")
	t.Setenv("GC_DOLT_PORT", "3307")

	cityPath := t.TempDir()
	writePGScopeFixture(t, cityPath, "citypw")
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: city
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	rigDir := filepath.Join(cityPath, "rigs", "pg-inherited")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: pg
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Rigs: []config.Rig{{Name: "pg", Path: "rigs/pg-inherited", Prefix: "pg"}}}

	env, err := bdRuntimeEnvForRigWithError(cityPath, cfg, rigDir)
	if err != nil {
		t.Fatalf("bdRuntimeEnvForRigWithError() error = %v", err)
	}

	wantPG := map[string]string{
		"GC_POSTGRES_PASSWORD":    "citypw",
		"BEADS_POSTGRES_PASSWORD": "citypw",
		"BEADS_POSTGRES_HOST":     "db.example.test",
		"BEADS_POSTGRES_PORT":     "5432",
		"BEADS_POSTGRES_USER":     "bd",
		"BEADS_POSTGRES_DATABASE": "beads",
	}
	for key, value := range wantPG {
		if got := env[key]; got != value {
			t.Errorf("env[%q] = %q, want %q", key, got, value)
		}
	}
	for _, key := range projectedDoltEnvKeys {
		if value, ok := env[key]; ok && value != "" {
			t.Errorf("env[%q] = %q, want empty/absent for inherited PG-backed city", key, value)
		}
	}
	if got := env["BEADS_DIR"]; !samePath(got, filepath.Join(rigDir, ".beads")) {
		t.Fatalf("BEADS_DIR = %q, want rig .beads", got)
	}
}

func TestBdRuntimeEnvForRig_ExplicitLegacyDoltRigClearsCityPostgresProjection(t *testing.T) {
	clearAmbientPostgresEnv(t)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")

	cityPath := t.TempDir()
	writePGScopeFixture(t, cityPath, "citypw")
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: city
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	rigDir := filepath.Join(cityPath, "rigs", "legacy-dolt")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Rigs: []config.Rig{{
		Name:     "legacy-dolt",
		Path:     "rigs/legacy-dolt",
		Prefix:   "ld",
		DoltHost: "rig-db.example.test",
		DoltPort: "4406",
	}}}

	env, err := bdRuntimeEnvForRigWithError(cityPath, cfg, rigDir)
	if err != nil {
		t.Fatalf("bdRuntimeEnvForRigWithError() error = %v", err)
	}

	if got := env["GC_DOLT_HOST"]; got != "rig-db.example.test" {
		t.Fatalf("GC_DOLT_HOST = %q, want rig-db.example.test", got)
	}
	if got := env["GC_DOLT_PORT"]; got != "4406" {
		t.Fatalf("GC_DOLT_PORT = %q, want 4406", got)
	}
	for _, key := range projectedPostgresEnvKeys {
		if value, ok := env[key]; ok && value != "" {
			t.Errorf("env[%q] = %q, want empty/absent for explicit legacy Dolt rig", key, value)
		}
	}
}

// TestBdRuntimeEnvForRig_ExplicitLegacyExternalRigCarriesAmbientTLS proves the
// legacy config.Rig{DoltHost,DoltPort} compatibility path preserves the ambient
// hosted-gateway BEADS_DOLT_SERVER_TLS requirement. A rig configured through the
// explicit legacy endpoint fields resolves an external endpoint, so
// applyResolvedRigDoltEnv mirrors it through mirrorBeadsDoltScopeEnv (External
// carries TLS) rather than the non-scoped mirrorBeadsDoltEnv, which clears it.
// Without the carry a TLS-required gateway rig on this compatibility path
// attempts plaintext and the gateway rejects it even though canonical external
// rigs connect with TLS (review finding F1, PR #4008).
func TestBdRuntimeEnvForRig_ExplicitLegacyExternalRigCarriesAmbientTLS(t *testing.T) {
	clearAmbientPostgresEnv(t)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	// Controller launched on a hosted TLS gateway carries an ambient TLS
	// requirement the native store must negotiate against the external endpoint.
	t.Setenv("BEADS_DOLT_SERVER_TLS", "1")

	cityPath := t.TempDir()
	writePGScopeFixture(t, cityPath, "citypw")
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: city
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	rigDir := filepath.Join(cityPath, "rigs", "legacy-dolt")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Rigs: []config.Rig{{
		Name:     "legacy-dolt",
		Path:     "rigs/legacy-dolt",
		Prefix:   "ld",
		DoltHost: "gw.beads.example",
		DoltPort: "3306",
	}}}

	env, err := bdRuntimeEnvForRigWithError(cityPath, cfg, rigDir)
	if err != nil {
		t.Fatalf("bdRuntimeEnvForRigWithError() error = %v", err)
	}

	if got := env["GC_DOLT_HOST"]; got != "gw.beads.example" {
		t.Fatalf("GC_DOLT_HOST = %q, want gw.beads.example", got)
	}
	if got := env["BEADS_DOLT_SERVER_HOST"]; got != "gw.beads.example" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want gw.beads.example", got)
	}
	if got, ok := env["BEADS_DOLT_SERVER_TLS"]; !ok || got != "1" {
		t.Fatalf("BEADS_DOLT_SERVER_TLS = %q (present=%v), want %q: ambient hosted-gateway TLS must be carried to a legacy external rig", got, ok, "1")
	}
}

// TestNativeDoltOpenEnvForScope_ExplicitLegacyExternalRigCarriesAmbientTLS is the
// native-open companion to
// TestBdRuntimeEnvForRig_ExplicitLegacyExternalRigCarriesAmbientTLS.
// nativeDoltOpenEnvForScope resolves a non-city scope through
// bdRuntimeEnvForRigWithError, so a legacy external rig's native-open env must
// also carry the ambient hosted-gateway TLS requirement (review finding F1,
// PR #4008).
func TestNativeDoltOpenEnvForScope_ExplicitLegacyExternalRigCarriesAmbientTLS(t *testing.T) {
	clearAmbientPostgresEnv(t)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("BEADS_DOLT_SERVER_TLS", "1")

	cityPath := t.TempDir()
	writePGScopeFixture(t, cityPath, "citypw")
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: city
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	rigDir := filepath.Join(cityPath, "rigs", "legacy-dolt")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Rigs: []config.Rig{{
		Name:     "legacy-dolt",
		Path:     "rigs/legacy-dolt",
		Prefix:   "ld",
		DoltHost: "gw.beads.example",
		DoltPort: "3306",
	}}}

	env, err := nativeDoltOpenEnvForScope(cityPath, cfg, rigDir)
	if err != nil {
		t.Fatalf("nativeDoltOpenEnvForScope() error = %v", err)
	}

	if got := env["BEADS_DOLT_SERVER_HOST"]; got != "gw.beads.example" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want gw.beads.example", got)
	}
	if got, ok := env["BEADS_DOLT_SERVER_TLS"]; !ok || got != "1" {
		t.Fatalf("BEADS_DOLT_SERVER_TLS = %q (present=%v), want %q: ambient hosted-gateway TLS must be carried to a legacy external rig native-open env", got, ok, "1")
	}
}

// TestBdRuntimeEnvForRig_ExplicitLocalExternalRigClearsAmbientTLS is the
// plaintext-endpoint counterpart to
// TestBdRuntimeEnvForRig_ExplicitLegacyExternalRigCarriesAmbientTLS. A legacy rig
// with an explicit 127.0.0.1 host resolves External by topology, but it is a
// plaintext local endpoint, not a hosted gateway. Under a controller carrying
// ambient BEADS_DOLT_SERVER_TLS=1, the rig runtime env must clear TLS so the rig's
// bd/native-open connection stays plaintext instead of forcing TLS against a
// non-TLS local server (PR #4008 review finding: gating the carry on External
// alone leaked TLS onto plaintext local endpoints).
func TestBdRuntimeEnvForRig_ExplicitLocalExternalRigClearsAmbientTLS(t *testing.T) {
	clearAmbientPostgresEnv(t)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("BEADS_DOLT_SERVER_TLS", "1")

	cityPath := t.TempDir()
	writePGScopeFixture(t, cityPath, "citypw")
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: city
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	rigDir := filepath.Join(cityPath, "rigs", "legacy-dolt")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Rigs: []config.Rig{{
		Name:     "legacy-dolt",
		Path:     "rigs/legacy-dolt",
		Prefix:   "ld",
		DoltHost: "127.0.0.1",
		DoltPort: "3307",
	}}}

	env, err := bdRuntimeEnvForRigWithError(cityPath, cfg, rigDir)
	if err != nil {
		t.Fatalf("bdRuntimeEnvForRigWithError() error = %v", err)
	}

	if got := env["GC_DOLT_HOST"]; got != "127.0.0.1" {
		t.Fatalf("GC_DOLT_HOST = %q, want 127.0.0.1", got)
	}
	if got, ok := env["BEADS_DOLT_SERVER_TLS"]; !ok || got != "" {
		t.Fatalf("BEADS_DOLT_SERVER_TLS = %q (present=%v), want present and empty: ambient hosted-gateway TLS must not be carried to a plaintext local legacy rig", got, ok)
	}
}

// TestBdRuntimeEnvForRig_PortOnlyLegacyExternalRigClearsAmbientTLS covers the
// port-only legacy rig shape the review scorecard named explicitly: config.Rig
// with a DoltPort and no DoltHost resolves through canonicalExternalHost to a
// 127.0.0.1 host and External=true. It is a plaintext local endpoint, so ambient
// BEADS_DOLT_SERVER_TLS=1 must be cleared, not carried (PR #4008 review finding).
func TestBdRuntimeEnvForRig_PortOnlyLegacyExternalRigClearsAmbientTLS(t *testing.T) {
	clearAmbientPostgresEnv(t)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("BEADS_DOLT_SERVER_TLS", "1")

	cityPath := t.TempDir()
	writePGScopeFixture(t, cityPath, "citypw")
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: city
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	rigDir := filepath.Join(cityPath, "rigs", "legacy-dolt")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Rigs: []config.Rig{{
		Name:     "legacy-dolt",
		Path:     "rigs/legacy-dolt",
		Prefix:   "ld",
		DoltPort: "6608",
	}}}

	env, err := bdRuntimeEnvForRigWithError(cityPath, cfg, rigDir)
	if err != nil {
		t.Fatalf("bdRuntimeEnvForRigWithError() error = %v", err)
	}

	if got, ok := env["BEADS_DOLT_SERVER_TLS"]; !ok || got != "" {
		t.Fatalf("BEADS_DOLT_SERVER_TLS = %q (present=%v), want present and empty: a port-only (loopback-default) legacy rig must not inherit ambient TLS", got, ok)
	}
}

// TestNativeDoltOpenEnvForScope_ExplicitLocalExternalRigClearsAmbientTLS is the
// native-open companion to
// TestBdRuntimeEnvForRig_ExplicitLocalExternalRigClearsAmbientTLS: the native-open
// env for a plaintext local legacy rig must also clear the ambient hosted-gateway
// TLS requirement (PR #4008 review finding).
func TestNativeDoltOpenEnvForScope_ExplicitLocalExternalRigClearsAmbientTLS(t *testing.T) {
	clearAmbientPostgresEnv(t)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("BEADS_DOLT_SERVER_TLS", "1")

	cityPath := t.TempDir()
	writePGScopeFixture(t, cityPath, "citypw")
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: city
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	rigDir := filepath.Join(cityPath, "rigs", "legacy-dolt")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Rigs: []config.Rig{{
		Name:     "legacy-dolt",
		Path:     "rigs/legacy-dolt",
		Prefix:   "ld",
		DoltHost: "127.0.0.1",
		DoltPort: "3307",
	}}}

	env, err := nativeDoltOpenEnvForScope(cityPath, cfg, rigDir)
	if err != nil {
		t.Fatalf("nativeDoltOpenEnvForScope() error = %v", err)
	}

	if got, ok := env["BEADS_DOLT_SERVER_TLS"]; !ok || got != "" {
		t.Fatalf("BEADS_DOLT_SERVER_TLS = %q (present=%v), want present and empty: ambient hosted-gateway TLS must not be carried to a plaintext local legacy rig native-open env", got, ok)
	}
}

func TestBdRuntimeEnvForRig_ExplicitLegacyDoltRigIgnoresUnresolvableCityPostgres(t *testing.T) {
	clearAmbientPostgresEnv(t)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")

	cityPath := t.TempDir()
	writePGScopeFixture(t, cityPath, "")
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: city
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	rigDir := filepath.Join(cityPath, "rigs", "legacy-dolt")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Rigs: []config.Rig{{
		Name:     "legacy-dolt",
		Path:     "rigs/legacy-dolt",
		Prefix:   "ld",
		DoltHost: "rig-db.example.test",
		DoltPort: "4406",
	}}}

	env, err := bdRuntimeEnvForRigWithError(cityPath, cfg, rigDir)
	if err != nil {
		t.Fatalf("bdRuntimeEnvForRigWithError() error = %v", err)
	}

	if got := env["GC_DOLT_HOST"]; got != "rig-db.example.test" {
		t.Fatalf("GC_DOLT_HOST = %q, want rig-db.example.test", got)
	}
	if got := env["GC_DOLT_PORT"]; got != "4406" {
		t.Fatalf("GC_DOLT_PORT = %q, want 4406", got)
	}
	for _, key := range projectedPostgresEnvKeys {
		if value, ok := env[key]; ok && value != "" {
			t.Errorf("env[%q] = %q, want empty/absent for explicit legacy Dolt rig", key, value)
		}
	}
}

func TestBdRuntimeEnvForRig_ExplicitLegacyDoltRigSurfacesInvalidCityConfig(t *testing.T) {
	clearAmbientPostgresEnv(t)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")

	cityPath := t.TempDir()
	writePGScopeFixture(t, cityPath, "citypw")
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: city
gc.endpoint_origin: explicit
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: city-db.example.test
dolt.port: 4406
`), 0o644); err != nil {
		t.Fatal(err)
	}

	rigDir := filepath.Join(cityPath, "rigs", "legacy-dolt")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Rigs: []config.Rig{{
		Name:     "legacy-dolt",
		Path:     "rigs/legacy-dolt",
		Prefix:   "ld",
		DoltHost: "rig-db.example.test",
		DoltPort: "4406",
	}}}

	_, err := bdRuntimeEnvForRigWithError(cityPath, cfg, rigDir)
	if err == nil {
		t.Fatal("bdRuntimeEnvForRigWithError() error = nil, want invalid city endpoint error")
	}
	if errors.Is(err, pgauth.ErrNoPasswordResolvable) {
		t.Fatalf("bdRuntimeEnvForRigWithError() error = %v, want non-credential city config error", err)
	}
	if !strings.Contains(err.Error(), "invalid canonical endpoint state") {
		t.Fatalf("bdRuntimeEnvForRigWithError() error = %v, want invalid canonical endpoint state", err)
	}
}

func TestBdRuntimeEnvForRig_AuthoritativeDoltRigIgnoresUnresolvableCityPostgres(t *testing.T) {
	clearAmbientPostgresEnv(t)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")

	cityPath := t.TempDir()
	writePGScopeFixture(t, cityPath, "")
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: city
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	rigDir := filepath.Join(cityPath, "rigs", "canonical-dolt")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: cd
gc.endpoint_origin: explicit
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: rig-db.example.test
dolt.port: 4407
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Rigs: []config.Rig{{
		Name:   "canonical-dolt",
		Path:   "rigs/canonical-dolt",
		Prefix: "cd",
	}}}

	env, err := bdRuntimeEnvForRigWithError(cityPath, cfg, rigDir)
	if err != nil {
		t.Fatalf("bdRuntimeEnvForRigWithError() error = %v", err)
	}

	if got := env["GC_DOLT_HOST"]; got != "rig-db.example.test" {
		t.Fatalf("GC_DOLT_HOST = %q, want rig-db.example.test", got)
	}
	if got := env["GC_DOLT_PORT"]; got != "4407" {
		t.Fatalf("GC_DOLT_PORT = %q, want 4407", got)
	}
	for _, key := range projectedPostgresEnvKeys {
		if value, ok := env[key]; ok && value != "" {
			t.Errorf("env[%q] = %q, want empty/absent for authoritative Dolt rig", key, value)
		}
	}
}

func TestCityRuntimeProcessEnv_PostgresBackend(t *testing.T) {
	clearAmbientPostgresEnv(t)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT_HOST", "ambient-dolt")
	t.Setenv("BEADS_POSTGRES_PASSWORD", "ambient-pg")

	cityPath := t.TempDir()
	writePGScopeFixture(t, cityPath, "citypw")
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: city
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := cityRuntimeProcessEnvWithError(cityPath)
	if err != nil {
		t.Fatalf("cityRuntimeProcessEnvWithError() error = %v", err)
	}
	env := listToMap(entries)

	if got := env["BEADS_POSTGRES_PASSWORD"]; got != "citypw" {
		t.Fatalf("BEADS_POSTGRES_PASSWORD = %q, want citypw", got)
	}
	if got := env["BEADS_POSTGRES_HOST"]; got != "db.example.test" {
		t.Fatalf("BEADS_POSTGRES_HOST = %q, want db.example.test", got)
	}
	if got := env["GC_DOLT_HOST"]; got != "" {
		t.Fatalf("GC_DOLT_HOST = %q, want empty for PG-backed city process env", got)
	}
	if got := env["BEADS_DOLT_SERVER_PORT"]; got != "" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want explicit empty for PG-backed city process env", got)
	}
}

func TestCityRuntimeProcessEnvWithError_SurfacesPostgresProjectionError(t *testing.T) {
	clearAmbientPostgresEnv(t)
	t.Setenv("GC_BEADS", "bd")

	cityPath := t.TempDir()
	writePGScopeFixture(t, cityPath, "")
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: city
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := cityRuntimeProcessEnvWithError(cityPath)
	if err == nil {
		t.Fatal("cityRuntimeProcessEnvWithError() error = nil, want postgres projection error")
	}
	if !errors.Is(err, pgauth.ErrNoPasswordResolvable) {
		t.Fatalf("errors.Is(err, ErrNoPasswordResolvable) = false, want true; err=%v", err)
	}
}

func TestBdRuntimeEnvForRig_PostgresBackendClearsCityDoltProjection(t *testing.T) {
	clearAmbientPostgresEnv(t)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = writeReachableManagedDoltState(t, cityPath)

	rigDir := filepath.Join(cityPath, "rigs", "pg")
	writePGScopeFixture(t, rigDir, "rigpw")
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: pg
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Rigs: []config.Rig{{Name: "pg", Path: "rigs/pg", Prefix: "pg"}}}

	env, err := bdRuntimeEnvForRigWithError(cityPath, cfg, rigDir)
	if err != nil {
		t.Fatalf("bdRuntimeEnvForRigWithError() error = %v", err)
	}

	if got := env["BEADS_POSTGRES_PASSWORD"]; got != "rigpw" {
		t.Fatalf("BEADS_POSTGRES_PASSWORD = %q, want rigpw", got)
	}
	for _, key := range projectedDoltEnvKeys {
		if value, ok := env[key]; ok && value != "" {
			t.Errorf("env[%q] = %q, want empty/absent for PG-backed rig", key, value)
		}
	}
}

func TestMergeRuntimeEnvScrubsPostgresKeys(t *testing.T) {
	for _, key := range projectedPostgresEnvKeys {
		t.Setenv(key, "PARENT")
	}

	parentEnv := []string{}
	for _, key := range projectedPostgresEnvKeys {
		parentEnv = append(parentEnv, key+"=PARENT")
	}
	overrides := map[string]string{"BEADS_POSTGRES_PASSWORD": "CHILD"}

	merged := mergeRuntimeEnv(parentEnv, overrides)

	got := map[string]string{}
	for _, entry := range merged {
		idx := strings.IndexByte(entry, '=')
		if idx < 0 {
			continue
		}
		got[entry[:idx]] = entry[idx+1:]
	}
	if got["BEADS_POSTGRES_PASSWORD"] != "CHILD" {
		t.Errorf("BEADS_POSTGRES_PASSWORD = %q, want CHILD", got["BEADS_POSTGRES_PASSWORD"])
	}
	for _, key := range projectedPostgresEnvKeys {
		if key == "BEADS_POSTGRES_PASSWORD" {
			continue
		}
		if value, ok := got[key]; ok {
			t.Errorf("env[%q] = %q, want absent (parent value not stripped)", key, value)
		}
	}
}

func TestApplyResolvedScopePostgresEnv_NoPasswordResolvable(t *testing.T) {
	clearAmbientPostgresEnv(t)
	cityPath := t.TempDir()
	scopeRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scopeRoot, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}

	env := map[string]string{}
	meta := contract.MetadataState{
		Backend:          "postgres",
		PostgresHost:     "db.example.test",
		PostgresPort:     "5432",
		PostgresUser:     "bd",
		PostgresDatabase: "beads",
	}
	err := applyResolvedScopePostgresEnv(env, cityPath, scopeRoot, meta)
	if err == nil {
		t.Fatal("applyResolvedScopePostgresEnv = nil error, want resolver exhaustion")
	}
	if !errors.Is(err, pgauth.ErrNoPasswordResolvable) {
		t.Errorf("errors.Is(err, ErrNoPasswordResolvable) = false, want true; err = %v", err)
	}
	if !strings.Contains(err.Error(), "resolving postgres credentials for ") {
		t.Errorf("err.Error() = %q, want prefix %q", err.Error(), "resolving postgres credentials for ")
	}
	if !strings.Contains(err.Error(), scopeRoot) {
		t.Errorf("err.Error() = %q, want scope path %q embedded", err.Error(), scopeRoot)
	}
}

func TestBdCommandRunnerForRigSurfacesPostgresProjectionError(t *testing.T) {
	clearAmbientPostgresEnv(t)
	t.Setenv("GC_BEADS", "bd")

	origRunner := beadsExecCommandRunnerWithEnv
	t.Cleanup(func() { beadsExecCommandRunnerWithEnv = origRunner })

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	rigDir := filepath.Join(cityPath, "rigs", "pg")
	writePGScopeFixture(t, rigDir, "")
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: pg
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Rigs: []config.Rig{{Name: "pg", Path: "rigs/pg", Prefix: "pg"}}}

	attempts := 0
	beadsExecCommandRunnerWithEnv = func(_ map[string]string) beads.CommandRunner {
		return func(_ string, _ string, _ ...string) ([]byte, error) {
			attempts++
			return []byte("should not run"), nil
		}
	}

	runner := bdCommandRunnerForRig(cityPath, cfg, rigDir)
	_, err := runner(rigDir, "bd", "list", "--json")

	if err == nil {
		t.Fatal("runner err = nil, want postgres projection error")
	}
	if !errors.Is(err, pgauth.ErrNoPasswordResolvable) {
		t.Errorf("errors.Is(err, ErrNoPasswordResolvable) = false, want true; err=%v", err)
	}
	if attempts != 0 {
		t.Fatalf("attempts = %d, want 0 because env projection failed before bd invocation", attempts)
	}
}

func TestBdCommandRunnerEnsuresProjectedPostgresEnvExplicit(t *testing.T) {
	t.Setenv("GC_POSTGRES_PASSWORD", "ambient-gc-pg")
	t.Setenv("BEADS_POSTGRES_PASSWORD", "ambient-beads-pg")
	t.Setenv("BEADS_POSTGRES_HOST", "ambient-host")

	origRunner := beadsExecCommandRunnerWithEnv
	t.Cleanup(func() { beadsExecCommandRunnerWithEnv = origRunner })

	var captured map[string]string
	beadsExecCommandRunnerWithEnv = func(env map[string]string) beads.CommandRunner {
		captured = map[string]string{}
		for key, value := range env {
			captured[key] = value
		}
		return func(_ string, _ string, _ ...string) ([]byte, error) {
			return []byte("ok"), nil
		}
	}

	runner := bdCommandRunnerWithManagedRetry(t.TempDir(), func(_ string) map[string]string {
		return map[string]string{}
	})
	if _, err := runner(t.TempDir(), "bd", "list", "--json"); err != nil {
		t.Fatalf("runner err = %v, want nil", err)
	}

	for _, key := range projectedPostgresEnvKeys {
		if value, ok := captured[key]; !ok || value != "" {
			t.Errorf("captured[%q] = %q, present=%v; want explicit empty override", key, value, ok)
		}
	}
}

func TestPGTransportError_MetadataReadErrorSkipsManagedRecovery(t *testing.T) {
	clearAmbientPostgresEnv(t)
	t.Setenv("GC_BEADS", "bd")

	origRunner := beadsExecCommandRunnerWithEnv
	origRecover := recoverManagedBDCommand
	t.Cleanup(func() {
		beadsExecCommandRunnerWithEnv = origRunner
		recoverManagedBDCommand = origRecover
	})

	cityPath := t.TempDir()
	writePGScopeFixture(t, cityPath, "devpw")
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: pg
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	metadataPath := filepath.Join(cityPath, ".beads", "metadata.json")
	originalErr := errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")
	attempts := 0
	recoverCalls := 0
	beadsExecCommandRunnerWithEnv = func(_ map[string]string) beads.CommandRunner {
		return func(_ string, _ string, _ ...string) ([]byte, error) {
			attempts++
			if err := os.WriteFile(metadataPath, []byte(`{"backend":`), 0o644); err != nil {
				t.Fatal(err)
			}
			return nil, originalErr
		}
	}
	recoverManagedBDCommand = func(_ string) error {
		recoverCalls++
		return nil
	}

	runner := bdCommandRunnerWithManagedRetry(cityPath, func(_ string) map[string]string {
		return map[string]string{"GC_DOLT_PORT": "3307"}
	})
	_, err := runner(cityPath, "bd", "list", "--json")

	if err == nil {
		t.Fatal("runner err = nil, want backend classification error")
	}
	if !strings.Contains(err.Error(), "classifying scope backend (bd error:") {
		t.Fatalf("err = %q, want classification context", err.Error())
	}
	if !strings.Contains(err.Error(), "invalid metadata.json") {
		t.Fatalf("err = %q, want metadata parse error", err.Error())
	}
	if !errors.Is(err, originalErr) {
		t.Fatalf("errors.Is(err, originalErr) = false, want original bd error preserved")
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if recoverCalls != 0 {
		t.Fatalf("recoverCalls = %d, want 0", recoverCalls)
	}
}

func TestPGTransportError_NoManagedRecovery(t *testing.T) {
	clearAmbientPostgresEnv(t)
	t.Setenv("GC_BEADS", "bd")

	origRunner := beadsExecCommandRunnerWithEnv
	origRecover := recoverManagedBDCommand
	t.Cleanup(func() {
		beadsExecCommandRunnerWithEnv = origRunner
		recoverManagedBDCommand = origRecover
	})

	cityPath := t.TempDir()
	writePGScopeFixture(t, cityPath, "devpw")
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: pg
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	originalErr := errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")
	attempts := 0
	recoverCalls := 0
	beadsExecCommandRunnerWithEnv = func(_ map[string]string) beads.CommandRunner {
		return func(_ string, _ string, _ ...string) ([]byte, error) {
			attempts++
			return nil, originalErr
		}
	}
	recoverManagedBDCommand = func(_ string) error {
		recoverCalls++
		return nil
	}

	runner := bdCommandRunnerWithManagedRetry(cityPath, func(_ string) map[string]string {
		return map[string]string{}
	})
	_, err := runner(cityPath, "bd", "list", "--json")

	if err == nil {
		t.Fatal("runner err = nil, want wrapped transport error")
	}
	if !strings.Contains(err.Error(), "postgres at ") {
		t.Errorf("err = %q, want substring %q", err.Error(), "postgres at ")
	}
	if !strings.Contains(err.Error(), "gc does not manage external PG endpoints (no managed recovery attempted)") {
		t.Errorf("err = %q, want managed-recovery hint substring", err.Error())
	}
	if strings.Contains(err.Error(), "is unreachable") {
		t.Errorf("err = %q, want no unreachable claim", err.Error())
	}
	if !errors.Is(err, originalErr) {
		t.Errorf("errors.Is(err, originalErr) = false, want true (wrap chain broken)")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (no retry for PG)", attempts)
	}
	if recoverCalls != 0 {
		t.Errorf("recoverCalls = %d, want 0 (managed recovery must not run for PG)", recoverCalls)
	}
}

func TestPostgresBackedScopeWrapsAnyBdErrorWithNoManagedRecoveryHint(t *testing.T) {
	clearAmbientPostgresEnv(t)
	t.Setenv("GC_BEADS", "bd")

	origRunner := beadsExecCommandRunnerWithEnv
	origRecover := recoverManagedBDCommand
	t.Cleanup(func() {
		beadsExecCommandRunnerWithEnv = origRunner
		recoverManagedBDCommand = origRecover
	})

	cityPath := t.TempDir()
	writePGScopeFixture(t, cityPath, "devpw")
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: pg
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	originalErr := errors.New("context deadline exceeded")
	attempts := 0
	recoverCalls := 0
	beadsExecCommandRunnerWithEnv = func(_ map[string]string) beads.CommandRunner {
		return func(_ string, _ string, _ ...string) ([]byte, error) {
			attempts++
			return []byte("partial bd output"), originalErr
		}
	}
	recoverManagedBDCommand = func(_ string) error {
		recoverCalls++
		return nil
	}

	runner := bdCommandRunnerWithManagedRetry(cityPath, func(_ string) map[string]string {
		return map[string]string{}
	})
	out, err := runner(cityPath, "bd", "list", "--json")

	if err == nil {
		t.Fatal("runner err = nil, want wrapped postgres error")
	}
	if !strings.Contains(err.Error(), "gc does not manage external PG endpoints (no managed recovery attempted)") {
		t.Fatalf("err = %q, want managed-recovery hint substring", err.Error())
	}
	if !errors.Is(err, originalErr) {
		t.Fatalf("errors.Is(err, originalErr) = false, want true")
	}
	if string(out) != "partial bd output" {
		t.Fatalf("runner output = %q, want first-attempt output preserved", out)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if recoverCalls != 0 {
		t.Fatalf("recoverCalls = %d, want 0", recoverCalls)
	}
}

func TestPGTransportError_InheritedCityPostgresNoManagedRecovery(t *testing.T) {
	clearAmbientPostgresEnv(t)
	t.Setenv("GC_BEADS", "bd")

	origRunner := beadsExecCommandRunnerWithEnv
	origRecover := recoverManagedBDCommand
	t.Cleanup(func() {
		beadsExecCommandRunnerWithEnv = origRunner
		recoverManagedBDCommand = origRecover
	})

	cityPath := t.TempDir()
	writePGScopeFixture(t, cityPath, "citypw")
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: city
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	rigDir := filepath.Join(cityPath, "rigs", "pg-inherited")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: pg
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Rigs: []config.Rig{{Name: "pg", Path: "rigs/pg-inherited", Prefix: "pg"}}}

	originalErr := errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")
	attempts := 0
	recoverCalls := 0
	beadsExecCommandRunnerWithEnv = func(env map[string]string) beads.CommandRunner {
		if got := env["BEADS_POSTGRES_PASSWORD"]; got != "citypw" {
			t.Fatalf("BEADS_POSTGRES_PASSWORD = %q, want inherited city password", got)
		}
		return func(_ string, _ string, _ ...string) ([]byte, error) {
			attempts++
			return nil, originalErr
		}
	}
	recoverManagedBDCommand = func(_ string) error {
		recoverCalls++
		return nil
	}

	runner := bdCommandRunnerForRig(cityPath, cfg, rigDir)
	_, err := runner(rigDir, "bd", "list", "--json")

	if err == nil {
		t.Fatal("runner err = nil, want wrapped transport error")
	}
	if !strings.Contains(err.Error(), "gc does not manage external PG endpoints (no managed recovery attempted)") {
		t.Errorf("err = %q, want managed-recovery hint substring", err.Error())
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (no retry for inherited PG)", attempts)
	}
	if recoverCalls != 0 {
		t.Errorf("recoverCalls = %d, want 0 (managed recovery must not run for inherited PG)", recoverCalls)
	}
}

func TestMixedBackendCity_PerScopeDispatch(t *testing.T) {
	clearAmbientPostgresEnv(t)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	for _, key := range []string{"GC_DOLT_HOST", "GC_DOLT_PORT", "GC_DOLT_USER", "GC_DOLT_PASSWORD"} {
		t.Setenv(key, "")
		_ = os.Unsetenv(key)
	}

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	pgRig := filepath.Join(cityPath, "rigs", "pg")
	writePGScopeFixture(t, pgRig, "pgpw")
	if err := os.WriteFile(filepath.Join(pgRig, ".beads", "config.yaml"), []byte(`issue_prefix: pg
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	defaultRig := filepath.Join(cityPath, "rigs", "default")
	if err := os.MkdirAll(filepath.Join(defaultRig, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(defaultRig, ".beads", "config.yaml"), []byte(`issue_prefix: default
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{Rigs: []config.Rig{
		{Name: "pg", Path: "rigs/pg", Prefix: "pg"},
		{Name: "default", Path: "rigs/default", Prefix: "default"},
	}}

	pgEnv, err := bdRuntimeEnvForRigWithError(cityPath, cfg, pgRig)
	if err != nil {
		t.Fatalf("bdRuntimeEnvForRigWithError(pg) error = %v", err)
	}
	if got := pgEnv["BEADS_POSTGRES_PASSWORD"]; got != "pgpw" {
		t.Errorf("pg rig BEADS_POSTGRES_PASSWORD = %q, want pgpw", got)
	}
	if got := pgEnv["BEADS_POSTGRES_HOST"]; got != "db.example.test" {
		t.Errorf("pg rig BEADS_POSTGRES_HOST = %q, want db.example.test", got)
	}

	defaultEnv, err := bdRuntimeEnvForRigWithError(cityPath, cfg, defaultRig)
	if err != nil {
		t.Fatalf("bdRuntimeEnvForRigWithError(default) error = %v", err)
	}
	if value, ok := defaultEnv["BEADS_POSTGRES_PASSWORD"]; ok && value != "" {
		t.Errorf("default rig BEADS_POSTGRES_PASSWORD = %q, want absent (no PG leak across scopes)", value)
	}
	if value, ok := defaultEnv["BEADS_POSTGRES_HOST"]; ok && value != "" {
		t.Errorf("default rig BEADS_POSTGRES_HOST = %q, want absent", value)
	}
}

func TestBdRuntimeEnvForRig_PostgresRigOverridesDoltliteCityBackend(t *testing.T) {
	clearAmbientPostgresEnv(t)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS_BACKEND", "")
	_ = os.Unsetenv("GC_BEADS_BACKEND")
	t.Setenv("BEADS_BACKEND", "")
	_ = os.Unsetenv("BEADS_BACKEND")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(`[beads]
provider = "bd"
backend = "doltlite"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(`{"backend":"doltlite","database":"doltlite","dolt_database":"hq"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: city
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	pgRig := filepath.Join(cityPath, "rigs", "pg")
	writePGScopeFixture(t, pgRig, "pgpw")
	if err := os.WriteFile(filepath.Join(pgRig, ".beads", "config.yaml"), []byte(`issue_prefix: pg
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Rigs: []config.Rig{{Name: "pg", Path: "rigs/pg", Prefix: "pg"}}}

	env, err := bdRuntimeEnvForRigWithError(cityPath, cfg, pgRig)
	if err != nil {
		t.Fatalf("bdRuntimeEnvForRigWithError(pg) error = %v", err)
	}
	if got := env["BEADS_POSTGRES_PASSWORD"]; got != "pgpw" {
		t.Fatalf("BEADS_POSTGRES_PASSWORD = %q, want pgpw", got)
	}
	if got := env["BEADS_POSTGRES_HOST"]; got != "db.example.test" {
		t.Fatalf("BEADS_POSTGRES_HOST = %q, want db.example.test", got)
	}
	for _, key := range []string{"GC_BEADS_BACKEND", "BEADS_BACKEND"} {
		if got := env[key]; got == "doltlite" {
			t.Fatalf("%s = %q, want Postgres rig to override DoltLite city backend", key, got)
		}
	}
}

func TestProjectedKeysCoverage(t *testing.T) {
	parentKeys := make([]string, 0, len(projectedBeadsBackendEnvKeys)+len(projectedPostgresEnvKeys)+len(projectedDoltEnvKeys))
	for _, key := range projectedBeadsBackendEnvKeys {
		parentKeys = append(parentKeys, key+"=PARENT")
	}
	for _, key := range projectedPostgresEnvKeys {
		parentKeys = append(parentKeys, key+"=PARENT")
	}
	for _, key := range projectedDoltEnvKeys {
		parentKeys = append(parentKeys, key+"=PARENT")
	}
	stripped := mergeRuntimeEnv(parentKeys, nil)
	if len(stripped) != 0 {
		t.Errorf("mergeRuntimeEnv stripped projected keys = %d entries left, want 0; entries=%v", len(stripped), stripped)
	}

	for _, key := range projectedBeadsBackendEnvKeys {
		if !projectedKeyStripped(key) {
			t.Errorf("projectedBeadsBackendEnvKeys[%q] is not in mergeRuntimeEnv strip list - symmetry broken", key)
		}
	}
	for _, key := range projectedPostgresEnvKeys {
		if !pgKeyStripped(key) {
			t.Errorf("projectedPostgresEnvKeys[%q] is not in mergeRuntimeEnv strip list - symmetry broken", key)
		}
	}
	for _, key := range projectedDoltEnvKeys {
		if !projectedKeyStripped(key) {
			t.Errorf("projectedDoltEnvKeys[%q] is not in mergeRuntimeEnv strip list - symmetry broken", key)
		}
	}
	for _, key := range bdCLIRemoteSyncOptOutEnvKeys {
		if !projectedKeyStripped(key) {
			t.Errorf("bdCLIRemoteSyncOptOutEnvKeys[%q] is not in mergeRuntimeEnv strip list - symmetry broken", key)
		}
	}
	for _, key := range bdAutoBackupOptOutEnvKeys {
		if !projectedKeyStripped(key) {
			t.Errorf("bdAutoBackupOptOutEnvKeys[%q] is not in mergeRuntimeEnv strip list - symmetry broken", key)
		}
	}
	for _, key := range bdContributorRoutingOptOutEnvKeys {
		if !projectedKeyStripped(key) {
			t.Errorf("bdContributorRoutingOptOutEnvKeys[%q] is not in mergeRuntimeEnv strip list - symmetry broken", key)
		}
	}
}

func TestMergeRuntimeEnvStripsInheritedBeadsBackend(t *testing.T) {
	result := mergeRuntimeEnv([]string{
		"GC_BEADS_BACKEND=doltlite",
		"BEADS_BACKEND=doltlite",
		"PATH=/usr/bin",
	}, nil)

	for _, key := range []string{"GC_BEADS_BACKEND", "BEADS_BACKEND"} {
		for _, entry := range result {
			if strings.HasPrefix(entry, key+"=") {
				t.Fatalf("%s leaked into merged runtime env: %v", key, result)
			}
		}
	}
	if !slices.Contains(result, "PATH=/usr/bin") {
		t.Fatalf("PATH was not preserved in merged runtime env: %v", result)
	}
}

func pgKeyStripped(key string) bool {
	return projectedKeyStripped(key)
}

func projectedKeyStripped(key string) bool {
	out := mergeRuntimeEnv([]string{key + "=PARENT"}, nil)
	for _, entry := range out {
		if strings.HasPrefix(entry, key+"=") {
			return false
		}
	}
	return true
}

func TestApplyCanonicalScopeBackendEnv_UnsupportedBackend(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(`{"backend":"sqlite"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	env := map[string]string{}
	used, err := applyCanonicalScopeBackendEnv(env, cityPath, cityPath)
	if err == nil {
		t.Fatal("applyCanonicalScopeBackendEnv = nil error, want unsupported-backend rejection")
	}
	if !used {
		t.Errorf("used = false, want true (scope is authoritative; failure is semantic)")
	}
	var parseErr *contract.MetadataParseError
	if !errors.As(err, &parseErr) {
		t.Errorf("errors.As(*MetadataParseError) = false, want true; err=%v", err)
	}
}

// TestBdRuntimeEnvDisablesAutoExport pins the env-var defense against
// bd's auto-export-on-write trap (sa-41j3kp): every gc-initiated bd call
// must set BD_EXPORT_AUTO=false so that even if .beads/config.yaml has not
// yet been canonicalized with export.auto:false (older cities pre-PR-1965,
// fresh inits, rigs that have not reached normalization), bd's auto-export
// of issues.jsonl on every write is suppressed. Without this, the next bd
// write re-creates the 15MB JSONL, and the bd write after that stalls for
// the full 2m subprocess timeout while bd re-imports the file.
func TestBdRuntimeEnvDisablesAutoExport(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")

	cityPath := t.TempDir()
	env, err := bdRuntimeEnvWithError(cityPath)
	if err != nil {
		t.Fatalf("bdRuntimeEnvWithError: %v", err)
	}

	if got := env["BD_EXPORT_AUTO"]; got != "false" {
		t.Fatalf("BD_EXPORT_AUTO = %q, want false (auto-export-on-write suppression for sa-41j3kp)", got)
	}
}

// TestScopeIsGCManagedRecognizesExplicitAutoOff verifies that a config with
// export.auto:false is recognized as gc-managed even when gc.endpoint_origin
// is absent. This is the steady-state signal post-PR-1965.
func TestScopeIsGCManagedRecognizesExplicitAutoOff(t *testing.T) {
	scope := t.TempDir()
	beadsDir := filepath.Join(scope, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"),
		[]byte("issue_prefix: zz\nexport.auto: false\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if !scopeIsGCManaged(scope) {
		t.Fatalf("scopeIsGCManaged = false, want true for explicit export.auto:false")
	}
}

// TestScopeIsGCManagedRecognizesManagedOrigin verifies that a long-lived
// city whose config still pre-dates PR 1965 (export.auto absent) is still
// recognized as gc-managed because gc.endpoint_origin proves it. This is
// the transitional signal — without it the jsonl reaper would refuse to
// clean up samtown-style cities until they hit a canonicalization event.
func TestScopeIsGCManagedRecognizesManagedOrigin(t *testing.T) {
	scope := t.TempDir()
	beadsDir := filepath.Join(scope, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"),
		[]byte("issue_prefix: zz\ngc.endpoint_origin: managed_city\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if !scopeIsGCManaged(scope) {
		t.Fatalf("scopeIsGCManaged = false, want true for gc.endpoint_origin: managed_city")
	}
}

// TestScopeIsGCManagedDoesNotClaimExplicitOptOut verifies the carve-out
// for rigs that deliberately keep JSONL-based sharing. Per PR 1965 docs,
// gc.endpoint_origin: explicit is the supported opt-out path; issues.jsonl
// there is load-bearing, not stale, so scopeIsGCManaged must return false.
func TestScopeIsGCManagedDoesNotClaimExplicitOptOut(t *testing.T) {
	scope := t.TempDir()
	beadsDir := filepath.Join(scope, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"),
		[]byte("issue_prefix: zz\ngc.endpoint_origin: explicit\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if scopeIsGCManaged(scope) {
		t.Fatalf("scopeIsGCManaged = true, want false for explicit opt-out")
	}
}

// TestScopeIsGCManagedExplicitOptOutBeatsExportAutoFalse verifies the
// precedence contract: when a scope has gc.endpoint_origin: explicit
// (deliberate opt-out, JSONL is load-bearing) AND also has export.auto:
// false (left over from a prior canonicalization, or hand-set), the
// endpoint_origin signal wins. Without this ordering, a stale
// export.auto value could trick the reaper into deleting issues.jsonl
// on an opt-out rig.
func TestScopeIsGCManagedExplicitOptOutBeatsExportAutoFalse(t *testing.T) {
	scope := t.TempDir()
	beadsDir := filepath.Join(scope, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"),
		[]byte("issue_prefix: zz\nexport.auto: false\ngc.endpoint_origin: explicit\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if scopeIsGCManaged(scope) {
		t.Fatalf("scopeIsGCManaged = true, want false for explicit opt-out (export.auto:false must not override endpoint_origin)")
	}
}

// TestReapStaleBdExportJSONLRemovesFileOnManagedScope verifies the
// transitional cleanup that breaks the sa-41j3kp loop on samtown-style
// long-lived cities. issues.jsonl from a pre-PR-1965 auto-export is
// removed on the next bd-store construction, so the next bd create does
// not stall on auto-import.
func TestReapStaleBdExportJSONLRemovesFileOnManagedScope(t *testing.T) {
	scope := t.TempDir()
	beadsDir := filepath.Join(scope, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	jsonlPath := filepath.Join(beadsDir, "issues.jsonl")
	if err := os.WriteFile(jsonlPath, []byte(`{"_type":"issue","id":"zz-1"}`+"\n"), 0o644); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"),
		[]byte("issue_prefix: zz\ngc.endpoint_origin: managed_city\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	reapStaleBdExportJSONL(scope)

	if _, err := os.Stat(jsonlPath); !os.IsNotExist(err) {
		t.Fatalf("jsonl present after reap; stat err = %v, want IsNotExist", err)
	}
}

func TestControlBdStoreForCityReapsStaleBdExportJSONL(t *testing.T) {
	cityPath := t.TempDir()
	beadsDir := filepath.Join(cityPath, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	jsonlPath := filepath.Join(beadsDir, "issues.jsonl")
	if err := os.WriteFile(jsonlPath, []byte(`{"_type":"issue","id":"gc-1"}`+"\n"), 0o644); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"),
		[]byte("issue_prefix: gc\ngc.endpoint_origin: managed_city\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	controlBdStoreForCity(cityPath, cityPath, nil)

	if _, err := os.Stat(jsonlPath); !os.IsNotExist(err) {
		t.Fatalf("jsonl present after control city store construction; stat err = %v, want IsNotExist", err)
	}
}

func TestControlBdStoreForRigReapsStaleBdExportJSONL(t *testing.T) {
	cityPath := t.TempDir()
	rigDir := filepath.Join(cityPath, "repo")
	beadsDir := filepath.Join(rigDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	jsonlPath := filepath.Join(beadsDir, "issues.jsonl")
	if err := os.WriteFile(jsonlPath, []byte(`{"_type":"issue","id":"repo-1"}`+"\n"), 0o644); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"),
		[]byte("issue_prefix: repo\ngc.endpoint_origin: inherited_city\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg := &config.City{Rigs: []config.Rig{{Name: "repo", Path: rigDir, Prefix: "repo"}}}

	controlBdStoreForRig(rigDir, cityPath, cfg)

	if _, err := os.Stat(jsonlPath); !os.IsNotExist(err) {
		t.Fatalf("jsonl present after control rig store construction; stat err = %v, want IsNotExist", err)
	}
}

// TestReapStaleBdExportJSONLLeavesFileOnExplicitOptOut verifies the
// opt-out path: rigs with gc.endpoint_origin: explicit keep issues.jsonl
// because they're deliberately using JSONL-based sharing.
func TestReapStaleBdExportJSONLLeavesFileOnExplicitOptOut(t *testing.T) {
	scope := t.TempDir()
	beadsDir := filepath.Join(scope, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	jsonlPath := filepath.Join(beadsDir, "issues.jsonl")
	if err := os.WriteFile(jsonlPath, []byte(`{"_type":"issue","id":"zz-1"}`+"\n"), 0o644); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"),
		[]byte("issue_prefix: zz\ngc.endpoint_origin: explicit\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	reapStaleBdExportJSONL(scope)

	if _, err := os.Stat(jsonlPath); err != nil {
		t.Fatalf("jsonl removed on explicit opt-out; stat err = %v, want nil", err)
	}
}

// TestReapStaleBdExportJSONLLeavesFileOnUnmanagedScope verifies that an
// unmanaged scope (no config.yaml, or config without any gc/managed signal)
// keeps issues.jsonl intact. The reaper must not be aggressive on scopes
// that gc does not own.
func TestReapStaleBdExportJSONLLeavesFileOnUnmanagedScope(t *testing.T) {
	scope := t.TempDir()
	beadsDir := filepath.Join(scope, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	jsonlPath := filepath.Join(beadsDir, "issues.jsonl")
	if err := os.WriteFile(jsonlPath, []byte(`{"_type":"issue","id":"zz-1"}`+"\n"), 0o644); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}
	// No config.yaml at all — definitely not gc-managed.

	reapStaleBdExportJSONL(scope)

	if _, err := os.Stat(jsonlPath); err != nil {
		t.Fatalf("jsonl removed on unmanaged scope; stat err = %v, want nil", err)
	}
}

// TestMirrorBeadsDoltEnvPropagatesCredentialCommand covers the hosted
// beads-gateway credential helper. bd authenticates by running the command in
// BEADS_DOLT_CREDENTIAL_COMMAND; that key contains "CREDENTIAL" so
// execenv.FilterInherited strips it from every gc-spawned bd subprocess and
// agent session, and the gateway then rejects the root fallback with Error
// 1045. The controller entrypoint also exports the same helper under the
// non-sensitive GC_DOLT_CRED_CMD (which survives filtering), so
// mirrorBeadsDoltEnv must re-derive BEADS_DOLT_CREDENTIAL_COMMAND from it.
func TestMirrorBeadsDoltEnvPropagatesCredentialCommand(t *testing.T) {
	t.Run("from GC_DOLT_CRED_CMD in the map", func(t *testing.T) {
		t.Setenv("GC_DOLT_CRED_CMD", "")
		t.Setenv("BEADS_DOLT_CREDENTIAL_COMMAND", "")
		env := map[string]string{"GC_DOLT_HOST": "gw.beads.example", "GC_DOLT_CRED_CMD": "/usr/local/bin/eia-helper"}
		mirrorBeadsDoltEnv(env)
		if got := env["BEADS_DOLT_CREDENTIAL_COMMAND"]; got != "/usr/local/bin/eia-helper" {
			t.Fatalf("BEADS_DOLT_CREDENTIAL_COMMAND = %q, want %q (from GC_DOLT_CRED_CMD)", got, "/usr/local/bin/eia-helper")
		}
	})
	t.Run("from ambient GC_DOLT_CRED_CMD when map is unset", func(t *testing.T) {
		t.Setenv("GC_DOLT_CRED_CMD", "/usr/local/bin/eia-helper")
		t.Setenv("BEADS_DOLT_CREDENTIAL_COMMAND", "")
		env := map[string]string{"GC_DOLT_HOST": "gw.beads.example"}
		mirrorBeadsDoltEnv(env)
		if got := env["BEADS_DOLT_CREDENTIAL_COMMAND"]; got != "/usr/local/bin/eia-helper" {
			t.Fatalf("BEADS_DOLT_CREDENTIAL_COMMAND = %q, want %q (from ambient GC_DOLT_CRED_CMD)", got, "/usr/local/bin/eia-helper")
		}
	})
	t.Run("from ambient BEADS_DOLT_CREDENTIAL_COMMAND fallback", func(t *testing.T) {
		t.Setenv("GC_DOLT_CRED_CMD", "")
		t.Setenv("BEADS_DOLT_CREDENTIAL_COMMAND", "/usr/local/bin/eia-helper")
		env := map[string]string{"GC_DOLT_HOST": "gw.beads.example"}
		mirrorBeadsDoltEnv(env)
		if got := env["BEADS_DOLT_CREDENTIAL_COMMAND"]; got != "/usr/local/bin/eia-helper" {
			t.Fatalf("BEADS_DOLT_CREDENTIAL_COMMAND = %q, want %q (from ambient fallback)", got, "/usr/local/bin/eia-helper")
		}
	})
	t.Run("GC_DOLT_CRED_CMD in map wins over ambient BEADS_DOLT_CREDENTIAL_COMMAND", func(t *testing.T) {
		t.Setenv("GC_DOLT_CRED_CMD", "")
		t.Setenv("BEADS_DOLT_CREDENTIAL_COMMAND", "/ambient/helper")
		env := map[string]string{"GC_DOLT_HOST": "gw.beads.example", "GC_DOLT_CRED_CMD": "/map/helper"}
		mirrorBeadsDoltEnv(env)
		if got := env["BEADS_DOLT_CREDENTIAL_COMMAND"]; got != "/map/helper" {
			t.Fatalf("BEADS_DOLT_CREDENTIAL_COMMAND = %q, want %q (map GC_DOLT_CRED_CMD wins)", got, "/map/helper")
		}
	})
	t.Run("map BEADS_DOLT_CREDENTIAL_COMMAND wins over ambient GC_DOLT_CRED_CMD", func(t *testing.T) {
		// Locks the branch-2-over-branch-3 precedence: an explicit map
		// BEADS_DOLT_CREDENTIAL_COMMAND must beat an ambient GC_DOLT_CRED_CMD.
		// Without this a branches-2/3 reorder would silently flip precedence and
		// still pass every other subtest.
		t.Setenv("GC_DOLT_CRED_CMD", "/ambient/helper")
		t.Setenv("BEADS_DOLT_CREDENTIAL_COMMAND", "")
		env := map[string]string{"GC_DOLT_HOST": "gw.beads.example", "BEADS_DOLT_CREDENTIAL_COMMAND": "/map/helper"}
		mirrorBeadsDoltEnv(env)
		if got := env["BEADS_DOLT_CREDENTIAL_COMMAND"]; got != "/map/helper" {
			t.Fatalf("BEADS_DOLT_CREDENTIAL_COMMAND = %q, want %q (explicit map value wins over ambient GC_DOLT_CRED_CMD)", got, "/map/helper")
		}
	})
	t.Run("absent when no credential command anywhere", func(t *testing.T) {
		t.Setenv("GC_DOLT_CRED_CMD", "")
		t.Setenv("BEADS_DOLT_CREDENTIAL_COMMAND", "")
		env := map[string]string{"GC_DOLT_HOST": "gw.beads.example"}
		mirrorBeadsDoltEnv(env)
		if got, ok := env["BEADS_DOLT_CREDENTIAL_COMMAND"]; ok && got != "" {
			t.Fatalf("BEADS_DOLT_CREDENTIAL_COMMAND = %q, want unset/empty", got)
		}
	})
}

// TestMirrorBeadsDoltEnvClearsTLSForNonExternalScope covers the safe default:
// mirrorBeadsDoltEnv is called at every non-external terminal projection
// (doltlite, postgres, managed-local dolt, cleared/error fallback), so it must
// never leave a native-open TLS requirement on such a scope. The regression it
// guards: a controller on a hosted TLS gateway (ambient BEADS_DOLT_SERVER_TLS=1)
// stamping that requirement onto a local/plaintext scope — directly, or by way of
// a rig env cloned from the hosted-gateway city env — which would make both the
// native store and the bd fallback attempt TLS against a plaintext server.
func TestMirrorBeadsDoltEnvClearsTLSForNonExternalScope(t *testing.T) {
	t.Run("ambient TLS is not copied into a non-external scope", func(t *testing.T) {
		t.Setenv("BEADS_DOLT_SERVER_TLS", "1")
		env := map[string]string{"GC_DOLT_HOST": "127.0.0.1", "GC_DOLT_PORT": "3307"}
		mirrorBeadsDoltEnv(env)
		if got, ok := env["BEADS_DOLT_SERVER_TLS"]; !ok || got != "" {
			t.Fatalf("BEADS_DOLT_SERVER_TLS = %q (present=%v), want present and empty", got, ok)
		}
	})
	t.Run("inherited hosted-gateway TLS is cleared", func(t *testing.T) {
		t.Setenv("BEADS_DOLT_SERVER_TLS", "1")
		// A rig runtime env is built on top of the city env; simulate one cloned
		// from a hosted-gateway city that already carried BEADS_DOLT_SERVER_TLS=1.
		env := map[string]string{"GC_DOLT_HOST": "127.0.0.1", "BEADS_DOLT_SERVER_TLS": "1"}
		mirrorBeadsDoltEnv(env)
		if got, ok := env["BEADS_DOLT_SERVER_TLS"]; !ok || got != "" {
			t.Fatalf("BEADS_DOLT_SERVER_TLS = %q (present=%v), want present and empty (inherited TLS must be cleared)", got, ok)
		}
	})
}

// TestMirrorBeadsDoltScopeEnvGatesTLSByExternal covers the hosted-gateway gate:
// only a scope whose resolved DoltConnectionTarget is External with a non-local
// host (targetCarriesHostedGatewayTLS) carries the TLS requirement into its
// native-open env. This is deliberately narrower than the identity-deferral
// target.External gate (preflightIdentityDeferredReader) — see
// TestMirrorBeadsDoltScopeEnvClearsTLSForExternalLocalEndpoint for the
// external-but-local case. This is the hosted-gateway path the PR restores: the
// gateway requires client TLS ("TLS required" otherwise), and the native store
// opens beadslib against a projected map that CityRuntimeEnvMapForRuntimeDir
// builds fresh without the ambient value.
func TestMirrorBeadsDoltScopeEnvGatesTLSByExternal(t *testing.T) {
	t.Run("external target carries ambient TLS", func(t *testing.T) {
		t.Setenv("BEADS_DOLT_SERVER_TLS", "1")
		env := map[string]string{"GC_DOLT_HOST": "gw.beads.example", "GC_DOLT_PORT": "3306"}
		mirrorBeadsDoltScopeEnv(env, contract.DoltConnectionTarget{Host: "gw.beads.example", Port: "3306", External: true})
		if got := env["BEADS_DOLT_SERVER_TLS"]; got != "1" {
			t.Fatalf("external BEADS_DOLT_SERVER_TLS = %q, want %q (from ambient)", got, "1")
		}
	})
	t.Run("external target: explicit scoped value wins over ambient", func(t *testing.T) {
		t.Setenv("BEADS_DOLT_SERVER_TLS", "0")
		env := map[string]string{"GC_DOLT_HOST": "gw.beads.example", "BEADS_DOLT_SERVER_TLS": "1"}
		mirrorBeadsDoltScopeEnv(env, contract.DoltConnectionTarget{Host: "gw.beads.example", External: true})
		if got := env["BEADS_DOLT_SERVER_TLS"]; got != "1" {
			t.Fatalf("external BEADS_DOLT_SERVER_TLS = %q, want %q (explicit scoped value wins)", got, "1")
		}
	})
	t.Run("managed-local (non-external) target clears ambient TLS", func(t *testing.T) {
		t.Setenv("BEADS_DOLT_SERVER_TLS", "1")
		env := map[string]string{"GC_DOLT_HOST": "127.0.0.1", "GC_DOLT_PORT": "3307"}
		mirrorBeadsDoltScopeEnv(env, contract.DoltConnectionTarget{Host: "127.0.0.1", Port: "3307", External: false})
		if got, ok := env["BEADS_DOLT_SERVER_TLS"]; !ok || got != "" {
			t.Fatalf("managed-local BEADS_DOLT_SERVER_TLS = %q (present=%v), want present and empty", got, ok)
		}
	})
}

// TestMirrorBeadsDoltScopeEnvClearsTLSForExternalLocalEndpoint locks the split
// between transport policy and endpoint topology (PR #4008 review finding).
// DoltConnectionTarget.External marks every non-managed explicit/city-canonical
// endpoint, including a plaintext 127.0.0.1 or a port-only legacy rig that
// populateExternalTarget/canonicalExternalHost default to loopback. Those speak
// plaintext, so under a controller carrying ambient BEADS_DOLT_SERVER_TLS=1 the
// scope mirror must clear TLS for them — gating the carry on External alone would
// force TLS onto a plaintext local endpoint and break the connection. Only a
// non-local host (a genuine hosted gateway) carries the ambient requirement.
func TestMirrorBeadsDoltScopeEnvClearsTLSForExternalLocalEndpoint(t *testing.T) {
	t.Run("explicit 127.0.0.1 external target clears ambient TLS", func(t *testing.T) {
		t.Setenv("BEADS_DOLT_SERVER_TLS", "1")
		env := map[string]string{"GC_DOLT_HOST": "127.0.0.1", "GC_DOLT_PORT": "3307"}
		mirrorBeadsDoltScopeEnv(env, contract.DoltConnectionTarget{Host: "127.0.0.1", Port: "3307", External: true})
		if got, ok := env["BEADS_DOLT_SERVER_TLS"]; !ok || got != "" {
			t.Fatalf("explicit-local external BEADS_DOLT_SERVER_TLS = %q (present=%v), want present and empty: a plaintext 127.0.0.1 endpoint must not inherit ambient TLS", got, ok)
		}
	})
	t.Run("localhost external target clears ambient TLS", func(t *testing.T) {
		t.Setenv("BEADS_DOLT_SERVER_TLS", "1")
		env := map[string]string{"GC_DOLT_HOST": "localhost", "GC_DOLT_PORT": "3307"}
		mirrorBeadsDoltScopeEnv(env, contract.DoltConnectionTarget{Host: "localhost", Port: "3307", External: true})
		if got, ok := env["BEADS_DOLT_SERVER_TLS"]; !ok || got != "" {
			t.Fatalf("localhost external BEADS_DOLT_SERVER_TLS = %q (present=%v), want present and empty", got, ok)
		}
	})
	t.Run("port-only external target (loopback default) clears ambient TLS", func(t *testing.T) {
		t.Setenv("BEADS_DOLT_SERVER_TLS", "1")
		// A port-only legacy rig resolves through canonicalExternalHost to a
		// 127.0.0.1 host, so the resolved target is External with a local host.
		env := map[string]string{"GC_DOLT_HOST": "127.0.0.1", "GC_DOLT_PORT": "6608"}
		mirrorBeadsDoltScopeEnv(env, contract.DoltConnectionTarget{Host: "127.0.0.1", Port: "6608", External: true})
		if got, ok := env["BEADS_DOLT_SERVER_TLS"]; !ok || got != "" {
			t.Fatalf("port-only external BEADS_DOLT_SERVER_TLS = %q (present=%v), want present and empty", got, ok)
		}
	})
	t.Run("external-local target clears inherited city TLS", func(t *testing.T) {
		// Rig runtime env is built on top of the city env, so a rig under a
		// hosted-gateway city inherits BEADS_DOLT_SERVER_TLS=1 in its map before the
		// rig mirror runs. An external-but-local rig must still clear it rather than
		// treat the inherited value as an intentional scoped TLS.
		t.Setenv("BEADS_DOLT_SERVER_TLS", "1")
		env := map[string]string{"GC_DOLT_HOST": "127.0.0.1", "GC_DOLT_PORT": "3307", "BEADS_DOLT_SERVER_TLS": "1"}
		mirrorBeadsDoltScopeEnv(env, contract.DoltConnectionTarget{Host: "127.0.0.1", Port: "3307", External: true})
		if got, ok := env["BEADS_DOLT_SERVER_TLS"]; !ok || got != "" {
			t.Fatalf("external-local BEADS_DOLT_SERVER_TLS = %q (present=%v), want present and empty (inherited city TLS must be cleared)", got, ok)
		}
	})
}

// TestMirrorBeadsDoltScopeEnvReadsAmbientTLSViaNativeOpenGuard proves the external
// carry resolves the ambient BEADS_DOLT_SERVER_TLS through the native-open env guard
// (ambientNativeDoltOpenEnv, mutex-guarded) rather than a bare os.Getenv.
// withNativeDoltOpenEnv mutates BEADS_DOLT_SERVER_TLS under nativeDoltOpenEnvMu while a
// concurrent scope opens, so an unguarded read could carry another scope's transient TLS
// into this projection. The stub returns a sentinel distinct from the ambient value so a
// regression to os.Getenv (which would read the ambient value and never call the stub)
// fails this test.
func TestMirrorBeadsDoltScopeEnvReadsAmbientTLSViaNativeOpenGuard(t *testing.T) {
	orig := ambientNativeDoltOpenEnv
	calledKey := ""
	ambientNativeDoltOpenEnv = func(key string) string {
		calledKey = key
		return "guarded-tls"
	}
	t.Cleanup(func() { ambientNativeDoltOpenEnv = orig })
	// A bare os.Getenv would read this ambient value; the guarded stub returns a
	// distinct sentinel so the assertions reveal which path the mirror took.
	t.Setenv("BEADS_DOLT_SERVER_TLS", "ambient-unguarded")

	env := map[string]string{"GC_DOLT_HOST": "gw.beads.example", "GC_DOLT_PORT": "3306"}
	mirrorBeadsDoltScopeEnv(env, contract.DoltConnectionTarget{Host: "gw.beads.example", Port: "3306", External: true})

	if calledKey != "BEADS_DOLT_SERVER_TLS" {
		t.Fatalf("mirror did not read ambient TLS via the native-open env guard (guarded accessor key seen = %q)", calledKey)
	}
	if got := env["BEADS_DOLT_SERVER_TLS"]; got != "guarded-tls" {
		t.Fatalf("external BEADS_DOLT_SERVER_TLS = %q, want %q (from the mutex-guarded ambient read)", got, "guarded-tls")
	}
}

// TestBeadsDoltServerTLSIsPassthroughNotProjected documents the intentional
// separation: BEADS_DOLT_SERVER_TLS is an ambient hosted-gateway credential
// passthrough (preserved for gc-spawned bd via preserveHostedBeadsCredentialEnv),
// not a per-scope projected key. Adding it to projectedDoltEnvKeys would make
// mergeRuntimeEnv strip a value the passthrough must keep, breaking hosted-gateway
// auth (and TestProjectedKeysCoverage's strip-symmetry). mirrorBeadsDoltEnv clears
// it for non-external scopes and mirrorBeadsDoltScopeEnv carries it for external
// endpoints, into the native-open env instead.
func TestBeadsDoltServerTLSIsPassthroughNotProjected(t *testing.T) {
	const key = "BEADS_DOLT_SERVER_TLS"
	inList := func(list []string, want string) bool {
		for _, k := range list {
			if k == want {
				return true
			}
		}
		return false
	}
	if !inList(hostedBeadsCredentialPassthroughKeys, key) {
		t.Fatalf("%s must remain a hosted-gateway credential passthrough key", key)
	}
	if inList(projectedDoltEnvKeys, key) {
		t.Fatalf("%s must NOT be a projected Dolt env key: mergeRuntimeEnv would strip a value preserveHostedBeadsCredentialEnv must keep", key)
	}
}

// TestCityRuntimeProcessEnvClearsAmbientTLSForNonExternalCity is the process-env
// companion to the mirror TLS-scoping tests above. cityRuntimeProcessEnvWithError
// builds the gc-spawned bd env for a BdStore-contract city; the scope mirror sets
// BEADS_DOLT_SERVER_TLS="" for a non-external/local city, but that key is
// intentionally absent from execProjectedBackendEnvKeys (it is a hosted-gateway
// credential passthrough, not a projected Dolt key). The cleared value must still
// be carried into the projected overrides, otherwise preserveHostedBeadsCredentialEnv
// re-injects the ambient hosted-gateway BEADS_DOLT_SERVER_TLS=1 and forces TLS
// against the plaintext local Dolt server. Regression for the review finding that
// the empty clear was dropped on the city-process-env path.
func TestCityRuntimeProcessEnvClearsAmbientTLSForNonExternalCity(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	// No external endpoint override: this city resolves to a non-external/local
	// scope, so the mirror clears TLS rather than carrying it.
	_ = os.Unsetenv("GC_DOLT_HOST")
	_ = os.Unsetenv("GC_DOLT_PORT")
	// Controller launched on a hosted TLS gateway carries an ambient TLS
	// requirement that must not bleed into a local/plaintext city's bd env.
	t.Setenv("BEADS_DOLT_SERVER_TLS", "1")

	cityPath := t.TempDir()
	env := envEntriesMap(mustCityRuntimeProcessEnv(t, cityPath))

	if got, ok := env["BEADS_DOLT_SERVER_TLS"]; !ok || got != "" {
		t.Fatalf("BEADS_DOLT_SERVER_TLS = %q (present=%v), want present and empty: ambient hosted-gateway TLS must not be re-injected into a non-external/local city's bd env", got, ok)
	}
}

// TestCityRuntimeProcessEnvCarriesTLSForExternalCity is the external-carry
// companion to TestCityRuntimeProcessEnvClearsAmbientTLSForNonExternalCity. It
// closes the coverage gap the review flagged (finding F2, PR #4008): the carry
// side of the hosted-gateway TLS scoping was proven only at the mirror-helper
// level (TestMirrorBeadsDoltScopeEnvGatesTLSByExternal), never at the
// cityRuntimeProcessEnvWithError integration level. A GC_DOLT_HOST override
// resolves the city to an external endpoint (externalDoltEnvOverrideTarget), so
// the scope mirror must carry the ambient BEADS_DOLT_SERVER_TLS=1 through the
// copy loop, mergeRuntimeEnv, and preserveHostedBeadsCredentialEnv into the
// final bd process env. A future refactor that dropped TLS from the copy-key
// list or mis-ordered the passthrough for the present-key case would still pass
// every clear-side test; this asserts the carry.
func TestCityRuntimeProcessEnvCarriesTLSForExternalCity(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	// A non-managed-local GC_DOLT_HOST override resolves the city to an external
	// hosted-gateway endpoint, the carry case the PR exists to restore.
	t.Setenv("GC_DOLT_HOST", "gw.beads.example")
	t.Setenv("GC_DOLT_PORT", "3306")
	// Controller launched on a hosted TLS gateway carries the ambient TLS
	// requirement the external city's bd process env must negotiate with.
	t.Setenv("BEADS_DOLT_SERVER_TLS", "1")

	cityPath := t.TempDir()
	env := envEntriesMap(mustCityRuntimeProcessEnv(t, cityPath))

	if got := env["BEADS_DOLT_SERVER_HOST"]; got != "gw.beads.example" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want gw.beads.example (external city endpoint)", got)
	}
	if got, ok := env["BEADS_DOLT_SERVER_TLS"]; !ok || got != "1" {
		t.Fatalf("BEADS_DOLT_SERVER_TLS = %q (present=%v), want %q: ambient hosted-gateway TLS must be carried into an external city's bd env", got, ok, "1")
	}
}

// TestCityRuntimeProcessEnvClearsAmbientTLSOnDoltResolutionError is the
// error-branch companion to TestCityRuntimeProcessEnvClearsAmbientTLSForNonExternalCity.
// When applyResolvedCityDoltEnv cannot resolve the city Dolt target (here a managed-city
// config whose Dolt runtime is not published), cityRuntimeProcessEnvWithError clears the
// projected Dolt keys and falls back to a local/plaintext bd env. That fallback must also
// carry BEADS_DOLT_SERVER_TLS="": clearProjectedDoltEnv does not touch TLS (it is a
// hosted-gateway passthrough key, not a projected key), so without the mirror the ambient
// TLS=1 is re-injected by preserveHostedBeadsCredentialEnv and forces TLS against the
// plaintext fallback. Regression for the review finding that the dolt-resolution error
// branch omitted the clear the postgres-error branch already performs.
func TestCityRuntimeProcessEnvClearsAmbientTLSOnDoltResolutionError(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	_ = os.Unsetenv("GC_DOLT")
	_ = os.Unsetenv("GC_DOLT_HOST")
	_ = os.Unsetenv("GC_DOLT_PORT")
	_ = os.Unsetenv("GC_DOLT_USER")
	_ = os.Unsetenv("GC_DOLT_PASSWORD")
	// Controller launched on a hosted TLS gateway carries an ambient TLS
	// requirement that must not bleed into the error fallback env.
	t.Setenv("BEADS_DOLT_SERVER_TLS", "1")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A managed-city canonical config resolves as authoritative (so this is neither
	// the postgres branch nor the empty-config success path), but the Dolt connection
	// target fails to resolve because no managed Dolt runtime state is published,
	// driving the applyResolvedCityDoltEnv error branch.
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// The dolt-resolution error is a soft fallback: cityRuntimeProcessEnvWithError
	// returns a usable env with a nil error (only the postgres branch surfaces a
	// projection error). A non-nil error would mean the config drove a different
	// branch than the one under test.
	entries, err := cityRuntimeProcessEnvWithError(cityPath)
	if err != nil {
		t.Fatalf("cityRuntimeProcessEnvWithError() error = %v, want nil (dolt resolution error is a soft fallback)", err)
	}
	env := envEntriesMap(entries)
	if got, ok := env["BEADS_DOLT_SERVER_TLS"]; !ok || got != "" {
		t.Fatalf("BEADS_DOLT_SERVER_TLS = %q (present=%v), want present and empty: ambient hosted-gateway TLS must not be re-injected on the dolt-resolution error fallback", got, ok)
	}
}
