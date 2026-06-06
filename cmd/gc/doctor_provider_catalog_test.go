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
