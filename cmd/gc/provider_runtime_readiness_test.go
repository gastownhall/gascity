package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// These are local protocol fixtures, not model/provider inference tests.
func runtimeReadinessCity(t *testing.T, ref, response string) (string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixture")
	}
	oldProbe, oldCatalog := initProbeProvidersReadiness, doctorProbeCatalogReadiness
	t.Cleanup(func() { initProbeProvidersReadiness = oldProbe; doctorProbeCatalogReadiness = oldCatalog })
	initProbeProvidersReadiness = probeResolvedProviderReadiness
	doctorProbeCatalogReadiness = func(context.Context, []string, bool) (map[string]api.ReadinessItem, error) {
		return map[string]api.ReadinessItem{}, nil
	}
	city := t.TempDir()
	script := filepath.Join(t.TempDir(), "selected-runtime")
	marker := filepath.Join(city, "probe-called")
	body := `#!/bin/sh
[ "$*" = "auth status --json" ] || exit 79
[ "$ANTHROPIC_API_KEY" = "unit-fixture-key" ] || exit 78
printf seen > "$PROBE_MARKER"
printf '%s\n' '` + response + `'
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	toml := fmt.Sprintf(`[workspace]
provider = %q
[providers.%s]
base = "builtin:claude"
command = %q
[providers.%s.env]
ANTHROPIC_API_KEY = "unit-fixture-key"
PROBE_MARKER = %q
`, ref, ref, script, ref, marker)
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	return city, marker
}

func TestRuntimeReadinessUsesSelectedExecutableAndAPIKeyEnvironment(t *testing.T) {
	city, marker := runtimeReadinessCity(t, "claude", `{"loggedIn":true,"authMethod":"api_key","apiProvider":"firstParty"}`)
	result := newProviderCatalogReadinessAdvisoryCheck(city).Run(&doctor.CheckContext{CityPath: city})
	if result.Status == doctor.StatusError {
		t.Fatalf("supported runtime incorrectly blocked: %+v", result)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("selected runtime never probed: %v; result=%+v", err, result)
	}
}

func TestRuntimeReadinessUsesInheritedFamilyForAlias(t *testing.T) {
	city, marker := runtimeReadinessCity(t, "work", `{"loggedIn":false}`)
	result := newProviderCatalogReadinessAdvisoryCheck(city).Run(&doctor.CheckContext{CityPath: city})
	if result.Status != doctor.StatusError || result.Severity != doctor.SeverityBlocking {
		t.Fatalf("lost alias credentials hidden: %+v", result)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("alias runtime never probed: %v", err)
	}
}

func TestInitRuntimeReadinessMissingResultBlocks(t *testing.T) {
	stubInitDependencyChecks(t)
	old := initProbeProvidersReadiness
	t.Cleanup(func() { initProbeProvidersReadiness = old })
	initProbeProvidersReadiness = func(context.Context, []initProviderTarget, bool) (map[string]api.ReadinessItem, error) {
		return map[string]api.ReadinessItem{}, nil
	}
	cfg := &config.City{Workspace: config.Workspace{Provider: "claude"}, Providers: map[string]config.ProviderSpec{"claude": {Base: configBaseForRuntimeTest("builtin:claude")}}}
	var out, errout bytes.Buffer
	err := runInitProviderPreflightForConfig(fileStorePreflightCity(t), cfg, &out, &errout, "gc init")
	if err == nil || !strings.Contains(errout.String(), "could not be verified") {
		t.Fatalf("missing verdict accepted: err=%v stderr=%q", err, errout.String())
	}
}

func configBaseForRuntimeTest(value string) *string { return &value }

func runtimeProbeNamesForTest(targets []initProviderTarget) []string {
	names := make([]string, 0, len(targets))
	for _, target := range targets {
		names = append(names, target.ProbeName)
	}
	return names
}

func TestRuntimeReadinessUsesCodexConfiguredHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixture")
	}
	oldProbe, oldCatalog := initProbeProvidersReadiness, doctorProbeCatalogReadiness
	t.Cleanup(func() { initProbeProvidersReadiness = oldProbe; doctorProbeCatalogReadiness = oldCatalog })
	initProbeProvidersReadiness = probeResolvedProviderReadiness
	doctorProbeCatalogReadiness = func(context.Context, []string, bool) (map[string]api.ReadinessItem, error) {
		return map[string]api.ReadinessItem{}, nil
	}
	city := t.TempDir()
	credentialHome := t.TempDir()
	marker := filepath.Join(city, "codex-probed")
	script := filepath.Join(t.TempDir(), "selected-codex")
	body := fmt.Sprintf("#!/bin/sh\n[ \"$*\" = \"login status\" ] || exit 79\n[ \"$CODEX_HOME\" = %s ] || exit 78\nprintf seen > %s\nprintf 'Logged in using an API key\\n' >&2\n", shellQuotePath(credentialHome), shellQuotePath(marker))
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	text := fmt.Sprintf("[workspace]\nprovider = \"codex\"\n[workspace.env]\nCODEX_HOME = %q\n[providers.codex]\nbase = \"builtin:codex\"\ncommand = %q\nresume_command = %q\n", credentialHome, script, script+" resume {{.SessionKey}}")
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCityConfigAllowMissingProviderReferences(city); err != nil {
		t.Fatalf("loading Codex fixture: %v", err)
	}
	result := newProviderCatalogReadinessAdvisoryCheck(city).Run(&doctor.CheckContext{CityPath: city})
	if result.Status == doctor.StatusError {
		t.Fatalf("supported Codex runtime blocked: %+v", result)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("configured Codex not checked: %v; result=%+v", err, result)
	}
}

func TestRuntimeReadinessChecksEachEffectiveAgentCredentialContext(t *testing.T) {
	city, _ := runtimeReadinessCity(t, "claude", `{"loggedIn":true}`)
	script := filepath.Join(t.TempDir(), "agent-runtime")
	body := `#!/bin/sh
[ "$*" = "auth status --json" ] || exit 79
if [ "$ANTHROPIC_API_KEY" = "unit-fixture-key" ]; then
 printf '%s\n' '{"loggedIn":true,"authMethod":"api_key"}'
else
 printf '%s\n' '{"loggedIn":false}'
 exit 1
fi
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	text := fmt.Sprintf(`[workspace]
provider = "claude"
[providers.claude]
base = "builtin:claude"
command = %q
[[agent]]
name = "left"
[agent.env]
ANTHROPIC_API_KEY = "unit-fixture-key"
[[agent]]
name = "right"
[agent.env]
ANTHROPIC_API_KEY = ""
`, script)
	path := filepath.Join(city, "city.toml")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	check := newProviderCatalogReadinessAdvisoryCheck(city)
	result := check.Run(&doctor.CheckContext{CityPath: city})
	if result.Status != doctor.StatusError || len(result.Details) != 1 || !strings.Contains(result.Details[0], "right") {
		t.Fatalf("effective contexts not diagnosed separately: %+v", result)
	}
	repaired := strings.Replace(text, `ANTHROPIC_API_KEY = ""`, `ANTHROPIC_API_KEY = "unit-fixture-key"`, 1)
	if err := os.WriteFile(path, []byte(repaired), 0o644); err != nil {
		t.Fatal(err)
	}
	if result := check.Run(&doctor.CheckContext{CityPath: city}); result.Status == doctor.StatusError {
		t.Fatalf("fresh readiness missed repaired context: %+v", result)
	}
}

func TestRuntimeReadinessCanceledDoctorDoesNotExecute(t *testing.T) {
	city, marker := runtimeReadinessCity(t, "claude", `{"loggedIn":true}`)
	done := make(chan struct{})
	close(done)
	result := newProviderCatalogReadinessAdvisoryCheck(city).Run(&doctor.CheckContext{CityPath: city, Done: done})
	if result.Status != doctor.StatusError || !strings.Contains(result.Message, "canceled") {
		t.Fatalf("canceled check accepted: %+v", result)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("canceled check started provider: %v", err)
	}
}

func TestInitRuntimeReadinessNilResultBlocksAtBothBoundaries(t *testing.T) {
	stubInitDependencyChecks(t)
	old := initProbeProvidersReadiness
	t.Cleanup(func() { initProbeProvidersReadiness = old })
	initProbeProvidersReadiness = func(context.Context, []initProviderTarget, bool) (map[string]api.ReadinessItem, error) {
		return nil, nil
	}
	if err := preflightInitSelectedProviders(wizardConfig{provider: "claude"}, false); err == nil {
		t.Fatal("pre-scaffold preflight accepted missing verdict")
	}
	cfg := &config.City{Workspace: config.Workspace{Provider: "claude"}, Providers: map[string]config.ProviderSpec{"claude": {Base: configBaseForRuntimeTest("builtin:claude")}}}
	var out, errout bytes.Buffer
	if err := runInitProviderPreflightForConfig(fileStorePreflightCity(t), cfg, &out, &errout, "gc init"); err == nil || !strings.Contains(errout.String(), "could not be verified") {
		t.Fatalf("expanded preflight accepted missing verdict: %v %q", err, errout.String())
	}
}

func TestInitRuntimeReadinessRemediesMatchBeforeAndAfterScaffold(t *testing.T) {
	stubInitDependencyChecks(t)
	old := initProbeProvidersReadiness
	t.Cleanup(func() { initProbeProvidersReadiness = old })
	initProbeProvidersReadiness = func(context.Context, []initProviderTarget, bool) (map[string]api.ReadinessItem, error) {
		return map[string]api.ReadinessItem{"claude": {DisplayName: "Claude Code", Status: api.ProbeStatusNeedsAuth}}, nil
	}
	early := preflightInitSelectedProviders(wizardConfig{provider: "claude"}, false)
	cfg := &config.City{Workspace: config.Workspace{Provider: "claude"}, Providers: map[string]config.ProviderSpec{"claude": {Base: configBaseForRuntimeTest("builtin:claude")}}}
	var out, resumed bytes.Buffer
	later := runInitProviderPreflightForConfig(fileStorePreflightCity(t), cfg, &out, &resumed, "gc init")
	if early == nil || later == nil {
		t.Fatalf("expected both boundaries to stop: early=%v later=%v", early, later)
	}
	for _, hint := range []string{"claude auth login", "claude setup-token", "CLAUDE_CODE_OAUTH_TOKEN"} {
		if !strings.Contains(early.Error(), hint) || !strings.Contains(resumed.String(), hint) {
			t.Fatalf("missing common remedy %q: early=%q resumed=%q", hint, early, resumed.String())
		}
	}
}

func fileStorePreflightCity(t *testing.T) string {
	t.Helper()
	city := t.TempDir()
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[beads]\nprovider = \"file\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return city
}

func TestRuntimeReadinessStatusWarningsDoNotOverrideNativeVerdict(t *testing.T) {
	city, marker := runtimeReadinessCity(t, "claude", `{"loggedIn":true,"authMethod":"api_key"}`)
	cfg, err := loadCityConfigAllowMissingProviderReferences(city)
	if err != nil {
		t.Fatal(err)
	}
	path := cfg.Providers["claude"].Command
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body = []byte(strings.Replace(string(body), "#!/bin/sh\n", "#!/bin/sh\nprintf 'Warning: optional PATH update failed\\n' >&2\n", 1))
	if err := os.WriteFile(path, body, 0o755); err != nil {
		t.Fatal(err)
	}
	result := newProviderCatalogReadinessAdvisoryCheck(city).Run(&doctor.CheckContext{CityPath: city})
	if result.Status == doctor.StatusError {
		t.Fatalf("stderr diagnostic overrode valid native verdict: %+v", result)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("configured executable not checked: %v", err)
	}
}
