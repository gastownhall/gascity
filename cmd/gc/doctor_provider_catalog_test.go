package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// providerLocalAuthSkippedMessage is the OK message the check returns when the
// expanded config fails to load. A fixture that reaches it exercises nothing
// about attribution, so tests that expect the check to run assert it is absent.
const providerLocalAuthSkippedMessage = "local provider auth advisory skipped until expanded config loads"

// stubClaudeNeedsAuthProbe replaces the readiness probe with one that reports
// the real "claude" CLI as installed but logged out and every other provider as
// not installed. The returned counter records how many times the probe ran.
func stubClaudeNeedsAuthProbe(t *testing.T) *int {
	t.Helper()
	calls := new(int)
	oldProbe := initProbeProvidersReadiness
	initProbeProvidersReadiness = func(_ context.Context, providers []string, fresh bool) (map[string]api.ReadinessItem, error) {
		*calls++
		if !fresh {
			t.Fatal("auth advisory must use fresh probes")
		}
		out := make(map[string]api.ReadinessItem, len(providers))
		for _, provider := range providers {
			out[provider] = api.ReadinessItem{Name: provider, DisplayName: provider, Status: api.ProbeStatusNotInstalled}
		}
		out["claude"] = api.ReadinessItem{
			Name:        "claude",
			DisplayName: "Claude Code",
			Status:      api.ProbeStatusNeedsAuth,
			Detail:      "claude is installed but not logged in",
		}
		return out, nil
	}
	t.Cleanup(func() { initProbeProvidersReadiness = oldProbe })
	return calls
}

// writeProviderFixtureCity writes cityTOML as a city's city.toml and returns the
// city directory together with the config the check itself loads, failing the
// test if that load fails.
func writeProviderFixtureCity(t *testing.T, cityTOML string) (string, *config.City) {
	t.Helper()
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCityConfigAllowMissingProviderReferences(cityDir)
	if err != nil {
		t.Fatalf("fixture config must load so the check reaches attribution instead of its skip branch: %v", err)
	}
	return cityDir, cfg
}

func TestProviderCatalogDoctorFixAddsMissingBuiltinAlias(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
provider = "claude"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	check := newProviderCatalogDoctorCheck(cityDir)
	before := check.Run(&doctor.CheckContext{CityPath: cityDir})
	if before.Status != doctor.StatusError {
		t.Fatalf("status before fix = %v, want error", before.Status)
	}
	if !check.CanFix() {
		t.Fatal("CanFix = false, want true for missing builtin provider")
	}
	if err := check.Fix(&doctor.CheckContext{CityPath: cityDir}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	after := check.Run(&doctor.CheckContext{CityPath: cityDir})
	if after.Status != doctor.StatusOK {
		t.Fatalf("status after fix = %v, want OK; details=%v", after.Status, after.Details)
	}
	data, err := os.ReadFile(filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "[providers.claude]\nbase = \"builtin:claude\"") {
		t.Fatalf("city.toml missing builtin alias:\n%s", data)
	}
}

func TestProviderCatalogReadinessAdvisoryCountsImportedProvidersAsExplicit(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, "packs", "local"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[imports.local]
source = "packs/local"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "packs", "local", "pack.toml"), []byte(`[pack]
name = "local"
schema = 2

[providers.codex]
base = "builtin:codex"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	oldProbe := initProbeProvidersReadiness
	initProbeProvidersReadiness = func(_ context.Context, providers []string, fresh bool) (map[string]api.ReadinessItem, error) {
		if !fresh {
			t.Fatal("readiness advisory must use fresh probes")
		}
		out := make(map[string]api.ReadinessItem, len(providers))
		for _, provider := range providers {
			out[provider] = api.ReadinessItem{
				Name:        provider,
				DisplayName: provider,
				Status:      api.ProbeStatusNotInstalled,
			}
		}
		out["claude"] = api.ReadinessItem{Name: "claude", DisplayName: "Claude Code", Status: api.ProbeStatusConfigured}
		out["codex"] = api.ReadinessItem{Name: "codex", DisplayName: "Codex", Status: api.ProbeStatusConfigured}
		return out, nil
	}
	t.Cleanup(func() { initProbeProvidersReadiness = oldProbe })

	result := newProviderCatalogReadinessAdvisoryCheck(cityDir).Run(&doctor.CheckContext{CityPath: cityDir})
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning", result.Status)
	}
	if len(result.Details) != 1 || !strings.Contains(result.Details[0], "Claude Code") {
		t.Fatalf("details = %v, want only missing claude advisory", result.Details)
	}
	if strings.Contains(strings.Join(result.Details, "\n"), "Codex") {
		t.Fatalf("imported codex provider should count as explicit: %v", result.Details)
	}
}

func TestProviderLocalAuthReportsConfiguredLoggedOutProvider(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[providers.claude]
base = "builtin:claude"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	oldProbe := initProbeProvidersReadiness
	initProbeProvidersReadiness = func(_ context.Context, providers []string, fresh bool) (map[string]api.ReadinessItem, error) {
		if !fresh {
			t.Fatal("auth advisory must use fresh probes")
		}
		out := make(map[string]api.ReadinessItem, len(providers))
		for _, provider := range providers {
			out[provider] = api.ReadinessItem{Name: provider, DisplayName: provider, Status: api.ProbeStatusNotInstalled}
		}
		out["claude"] = api.ReadinessItem{
			Name:        "claude",
			DisplayName: "Claude Code",
			Status:      api.ProbeStatusNeedsAuth,
			Detail:      "claude is installed but not logged in",
		}
		return out, nil
	}
	t.Cleanup(func() { initProbeProvidersReadiness = oldProbe })

	result := newProviderLocalAuthCheck(cityDir).Run(&doctor.CheckContext{CityPath: cityDir})
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning", result.Status)
	}
	// SeverityBlocking is the zero value, so the severity must be declared
	// advisory explicitly rather than left unset.
	if result.Severity != doctor.SeverityAdvisory {
		t.Fatalf("severity = %v, want advisory", result.Severity)
	}
	if len(result.Details) != 1 || !strings.Contains(result.Details[0], "Claude Code") {
		t.Fatalf("details = %v, want one Claude Code entry", result.Details)
	}
	if !strings.Contains(result.Details[0], "not logged in") {
		t.Fatalf("details = %v, want the probe detail preserved", result.Details)
	}
	if !strings.Contains(result.FixHint, "claude auth login") {
		t.Fatalf("fix hint = %q, want the claude login remediation", result.FixHint)
	}
}

func TestProviderLocalAuthReportsConfiguredAliasLoggedOutProvider(t *testing.T) {
	cityDir := t.TempDir()
	// claude-fast is a configured alias of the builtin claude provider, not
	// a canonical [providers.claude] entry -- resolving it to the "claude"
	// probe name is the fix for the alias-coverage gap this test guards.
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[providers.claude-fast]
base = "builtin:claude"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	oldProbe := initProbeProvidersReadiness
	initProbeProvidersReadiness = func(_ context.Context, providers []string, fresh bool) (map[string]api.ReadinessItem, error) {
		if !fresh {
			t.Fatal("auth advisory must use fresh probes")
		}
		out := make(map[string]api.ReadinessItem, len(providers))
		for _, provider := range providers {
			out[provider] = api.ReadinessItem{Name: provider, DisplayName: provider, Status: api.ProbeStatusNotInstalled}
		}
		out["claude"] = api.ReadinessItem{
			Name:        "claude",
			DisplayName: "Claude Code",
			Status:      api.ProbeStatusNeedsAuth,
			Detail:      "claude is installed but not logged in",
		}
		return out, nil
	}
	t.Cleanup(func() { initProbeProvidersReadiness = oldProbe })

	result := newProviderLocalAuthCheck(cityDir).Run(&doctor.CheckContext{CityPath: cityDir})
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning; details=%v", result.Status, result.Details)
	}
	// SeverityBlocking is the zero value, so the severity must be declared
	// advisory explicitly rather than left unset.
	if result.Severity != doctor.SeverityAdvisory {
		t.Fatalf("severity = %v, want advisory (non-blocking)", result.Severity)
	}
	if len(result.Details) != 1 || !strings.Contains(result.Details[0], "Claude Code") {
		t.Fatalf("details = %v, want one Claude Code entry", result.Details)
	}
	if !strings.Contains(result.Details[0], "not logged in") {
		t.Fatalf("details = %v, want the probe detail preserved", result.Details)
	}
	if !strings.Contains(result.FixHint, "claude auth login") {
		t.Fatalf("fix hint = %q, want the claude login remediation", result.FixHint)
	}
}

// TestProviderLocalAuthIgnoresNonClaudeLaunchCommands guards the attribution
// rule documented on providerReadinessProbeName: a "claude" provider whose
// effective command launches some other CLI -- set directly, as a multiword
// command line, or inherited from a custom ancestor -- must not be probed as
// the real Claude CLI. The real "claude" probe reports NeedsAuth in every case
// (an unrelated host-level claude install that is logged out), and none of
// these cities launches it, so no advisory may surface.
//
// Each fixture must load through the same loader the check uses, and the probe
// stub must run: an OK status alone is also what the check returns when the
// config fails to load, so the skip message is asserted absent.
func TestProviderLocalAuthIgnoresNonClaudeLaunchCommands(t *testing.T) {
	tests := []struct {
		name         string
		cityTOML     string
		wantCommand  string
		wantCmdLayer string
	}{
		{
			name: "direct single word override",
			cityTOML: `[providers.claude]
command = "kiro-cli"
`,
			wantCommand:  "kiro-cli",
			wantCmdLayer: "providers.claude",
		},
		{
			name: "direct multiword override",
			cityTOML: `[providers.claude]
command = "kiro-cli chat"
`,
			wantCommand:  "kiro-cli chat",
			wantCmdLayer: "providers.claude",
		},
		{
			name: "command inherited from a custom ancestor",
			cityTOML: `[providers.kiro-wrapper]
base = "builtin:claude"
command = "kiro-cli"

[providers.claude]
base = "provider:kiro-wrapper"
`,
			wantCommand:  "kiro-cli",
			wantCmdLayer: "providers.kiro-wrapper",
		},
		{
			name: "multiword command inherited from a custom ancestor",
			cityTOML: `[providers.kiro-wrapper]
base = "builtin:claude"
command = "kiro-cli chat"

[providers.claude]
base = "provider:kiro-wrapper"
`,
			wantCommand:  "kiro-cli chat",
			wantCmdLayer: "providers.kiro-wrapper",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cityDir, cfg := writeProviderFixtureCity(t, tt.cityTOML)

			resolved, ok := cfg.ResolvedProviders["claude"]
			if !ok {
				t.Fatal("loaded config has no resolved entry for provider claude")
			}
			if resolved.Command != tt.wantCommand {
				t.Fatalf("resolved claude command = %q, want %q", resolved.Command, tt.wantCommand)
			}
			if got := resolved.Provenance.FieldLayer["command"]; got != tt.wantCmdLayer {
				t.Fatalf("resolved claude command layer = %q, want %q", got, tt.wantCmdLayer)
			}
			if got := providerReadinessProbeName("claude", cfg); got != "" {
				t.Fatalf("providerReadinessProbeName(claude) = %q, want empty for a %q launch", got, tt.wantCommand)
			}

			probeCalls := stubClaudeNeedsAuthProbe(t)
			result := newProviderLocalAuthCheck(cityDir).Run(&doctor.CheckContext{CityPath: cityDir})
			if result.Message == providerLocalAuthSkippedMessage {
				t.Fatal("check skipped on config load; the fixture never reached attribution")
			}
			if *probeCalls != 1 {
				t.Fatalf("readiness probe ran %d times, want 1", *probeCalls)
			}
			if result.Status != doctor.StatusOK {
				t.Fatalf("status = %v, want OK; details=%v", result.Status, result.Details)
			}
			if result.Message != "configured local providers are logged in" {
				t.Fatalf("message = %q, want the no-advisory message", result.Message)
			}
			if len(result.Details) != 0 {
				t.Fatalf("details = %v, want none -- a %q launch must not be reported as Claude Code", result.Details, tt.wantCommand)
			}
			if result.Severity != doctor.SeverityAdvisory {
				t.Fatalf("severity = %v, want advisory", result.Severity)
			}
		})
	}
}

// TestProviderLocalAuthReportsInheritedBuiltinCommand is the positive
// counterpart: a canonical provider that reaches the built-in claude command
// through a custom ancestor is the real Claude CLI, so its NeedsAuth state is
// reported as an advisory warning.
func TestProviderLocalAuthReportsInheritedBuiltinCommand(t *testing.T) {
	cityDir, cfg := writeProviderFixtureCity(t, `[providers.team-claude]
base = "builtin:claude"

[providers.claude]
base = "provider:team-claude"
`)
	resolved, ok := cfg.ResolvedProviders["claude"]
	if !ok {
		t.Fatal("loaded config has no resolved entry for provider claude")
	}
	if resolved.Command != "claude" {
		t.Fatalf("resolved claude command = %q, want the built-in %q", resolved.Command, "claude")
	}
	if got := resolved.Provenance.FieldLayer["command"]; got != "builtin:claude" {
		t.Fatalf("resolved claude command layer = %q, want %q", got, "builtin:claude")
	}

	probeCalls := stubClaudeNeedsAuthProbe(t)
	result := newProviderLocalAuthCheck(cityDir).Run(&doctor.CheckContext{CityPath: cityDir})
	if *probeCalls != 1 {
		t.Fatalf("readiness probe ran %d times, want 1", *probeCalls)
	}
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning; message=%q details=%v", result.Status, result.Message, result.Details)
	}
	if result.Severity != doctor.SeverityAdvisory {
		t.Fatalf("severity = %v, want advisory", result.Severity)
	}
	if len(result.Details) != 1 || !strings.Contains(result.Details[0], "Claude Code") {
		t.Fatalf("details = %v, want one Claude Code entry", result.Details)
	}
}

// TestProviderLocalAuthWarningDoesNotBlockDoctor pins how the doctor report
// counts the advisory: a StatusWarning result increments Warned only, so it
// never contributes to BlockingFailed.
func TestProviderLocalAuthWarningDoesNotBlockDoctor(t *testing.T) {
	cityDir, _ := writeProviderFixtureCity(t, `[providers.claude]
base = "builtin:claude"
`)
	stubClaudeNeedsAuthProbe(t)

	d := &doctor.Doctor{}
	d.Register(newProviderLocalAuthCheck(cityDir))
	report := d.RunCollect(&doctor.CheckContext{CityPath: cityDir}, false)
	if report.Warned != 1 {
		t.Fatalf("Warned = %d, want 1", report.Warned)
	}
	if report.Failed != 0 || report.BlockingFailed != 0 {
		t.Fatalf("Failed = %d, BlockingFailed = %d, want 0 and 0: a warning must not gate doctor", report.Failed, report.BlockingFailed)
	}
}

func TestProviderLocalAuthIgnoresUnconfiguredLoggedOutProvider(t *testing.T) {
	cityDir := t.TempDir()
	// codex is logged out but this city never declares it: staying quiet is the point.
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[providers.claude]
base = "builtin:claude"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	oldProbe := initProbeProvidersReadiness
	initProbeProvidersReadiness = func(_ context.Context, providers []string, _ bool) (map[string]api.ReadinessItem, error) {
		out := make(map[string]api.ReadinessItem, len(providers))
		for _, provider := range providers {
			out[provider] = api.ReadinessItem{Name: provider, DisplayName: provider, Status: api.ProbeStatusNotInstalled}
		}
		out["claude"] = api.ReadinessItem{Name: "claude", DisplayName: "Claude Code", Status: api.ProbeStatusConfigured}
		out["codex"] = api.ReadinessItem{Name: "codex", DisplayName: "Codex", Status: api.ProbeStatusNeedsAuth}
		return out, nil
	}
	t.Cleanup(func() { initProbeProvidersReadiness = oldProbe })

	result := newProviderLocalAuthCheck(cityDir).Run(&doctor.CheckContext{CityPath: cityDir})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK; details=%v", result.Status, result.Details)
	}
}
