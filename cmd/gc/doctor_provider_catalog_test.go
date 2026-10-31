package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/doctor"
)

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
	// SeverityBlocking is the zero value; an unset severity would gate `gc doctor`.
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
	// SeverityBlocking is the zero value; an unset severity on this warning
	// would gate `gc doctor` outright for an aliased provider, the exact
	// alias-coverage gap this test exists to guard.
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

// TestProviderLocalAuthIgnoresExplicitNonClaudeCommandOverride guards the
// case documented on providerReadinessProbeName: a [providers.claude] entry
// whose command is explicitly overridden to a non-Claude binary (e.g. a Kiro
// wrapper declared directly under the "claude" provider name, with no base
// chain back to builtin:claude) must not be probed as -- or misattributed
// to -- the real Claude CLI. Even when the real "claude" readiness probe
// independently reports NeedsAuth (e.g. some unrelated host-level claude CLI
// is installed but logged out), this override must not surface a
// provider-local-auth advisory, because this city's "claude" provider is not
// that CLI.
func TestProviderLocalAuthIgnoresExplicitNonClaudeCommandOverride(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[providers.claude]
command = "kiro-cli"
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
		// The real "claude" CLI is installed but logged out -- but this
		// city's [providers.claude] is a kiro-cli wrapper, not that CLI, so
		// this must not be attributed to it.
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
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK; details=%v", result.Status, result.Details)
	}
	if len(result.Details) != 0 {
		t.Fatalf("details = %v, want none -- the kiro-cli override must not be misidentified as Claude Code", result.Details)
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
