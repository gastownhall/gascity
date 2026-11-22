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

// stubProviderReadiness swaps the readiness probe seam for the duration of
// the test. Providers absent from items report not_installed.
func stubProviderReadiness(t *testing.T, items map[string]api.ReadinessItem) {
	t.Helper()
	oldProbe := initProbeProvidersReadiness
	initProbeProvidersReadiness = func(_ context.Context, providers []string, fresh bool) (map[string]api.ReadinessItem, error) {
		if !fresh {
			t.Fatal("readiness advisory must use fresh probes")
		}
		out := make(map[string]api.ReadinessItem, len(providers))
		for _, provider := range providers {
			if item, ok := items[provider]; ok {
				out[provider] = item
				continue
			}
			out[provider] = api.ReadinessItem{Name: provider, DisplayName: provider, Status: api.ProbeStatusNotInstalled}
		}
		return out, nil
	}
	t.Cleanup(func() { initProbeProvidersReadiness = oldProbe })
}

func writeDoctorCityToml(t *testing.T, body string) string {
	t.Helper()
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return cityDir
}

func runProviderReadinessAdvisory(cityDir string) *doctor.CheckResult {
	return newProviderCatalogReadinessAdvisoryCheck(cityDir).Run(&doctor.CheckContext{CityPath: cityDir})
}

var claudeNeedsAuthItem = api.ReadinessItem{
	Name:        "claude",
	DisplayName: "Claude Code",
	Status:      api.ProbeStatusNeedsAuth,
	Detail:      "claude is installed but not logged in",
}

func TestProviderCatalogReadinessAdvisoryNeedsAuthResult(t *testing.T) {
	claudeHint := providerStatusFixHint("claude", api.ProbeStatusNeedsAuth)
	if claudeHint == "" {
		t.Fatal("claude needs_auth fix hint is empty")
	}
	cases := []struct {
		name string
		toml string
	}{
		{
			name: "workspace provider",
			toml: "[workspace]\nprovider = \"claude\"\n\n[providers.claude]\nbase = \"builtin:claude\"\n",
		},
		{
			name: "agent provider",
			toml: "[workspace]\nname = \"c\"\n\n[providers.claude]\nbase = \"builtin:claude\"\n\n[[agent]]\nname = \"worker\"\nprovider = \"claude\"\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityDir := writeDoctorCityToml(t, tc.toml)
			stubProviderReadiness(t, map[string]api.ReadinessItem{"claude": claudeNeedsAuthItem})

			result := runProviderReadinessAdvisory(cityDir)
			if result.Status != doctor.StatusWarning {
				t.Fatalf("status = %v (message %q), want warning", result.Status, result.Message)
			}
			if result.Severity != doctor.SeverityAdvisory {
				t.Fatalf("severity = %v, want advisory", result.Severity)
			}
			if want := "1 referenced provider(s) need authentication: Claude Code"; result.Message != want {
				t.Fatalf("message = %q, want %q", result.Message, want)
			}
			wantDetail := `Claude Code needs authentication (referenced as provider "claude"): claude is installed but not logged in`
			if len(result.Details) != 1 || result.Details[0] != wantDetail {
				t.Fatalf("details = %q, want [%q]", result.Details, wantDetail)
			}
			if want := "Claude Code: " + claudeHint; result.FixHint != want {
				t.Fatalf("fix hint = %q, want %q", result.FixHint, want)
			}
		})
	}
}

func TestProviderCatalogReadinessAdvisoryNeedsAuthUsesDetailWhenNoInitHint(t *testing.T) {
	if hint := providerStatusFixHint("zcode", api.ProbeStatusNeedsAuth); hint != "" {
		t.Fatalf("precondition: zcode has an init hint %q; this test covers the detail fallback", hint)
	}
	cityDir := writeDoctorCityToml(t, "[workspace]\nprovider = \"zcode\"\n\n[providers.zcode]\nbase = \"builtin:zcode\"\n")
	stubProviderReadiness(t, map[string]api.ReadinessItem{
		"zcode": {Name: "zcode", DisplayName: "ZCode", Status: api.ProbeStatusNeedsAuth, Detail: "set ZCODE_API_KEY"},
	})

	result := runProviderReadinessAdvisory(cityDir)
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v (message %q), want warning", result.Status, result.Message)
	}
	if want := "ZCode: set ZCODE_API_KEY"; result.FixHint != want {
		t.Fatalf("fix hint = %q, want %q", result.FixHint, want)
	}
}

func TestProviderCatalogReadinessAdvisoryNeedsAuthOrdersFindingsByProbeOrder(t *testing.T) {
	cityDir := writeDoctorCityToml(t, `[workspace]
provider = "codex"

[providers.claude]
base = "builtin:claude"

[providers.codex]
base = "builtin:codex"

[[agent]]
name = "worker"
provider = "claude"
`)
	stubProviderReadiness(t, map[string]api.ReadinessItem{
		"claude": claudeNeedsAuthItem,
		"codex":  {Name: "codex", DisplayName: "Codex", Status: api.ProbeStatusNeedsAuth, Detail: "codex is not signed in"},
	})

	first, second := "Claude Code", "Codex"
	for _, name := range api.ProviderReadinessNames() {
		if name == "codex" {
			first, second = second, first
			break
		}
		if name == "claude" {
			break
		}
	}
	result := runProviderReadinessAdvisory(cityDir)
	if want := "2 referenced provider(s) need authentication: " + first + ", " + second; result.Message != want {
		t.Fatalf("message = %q, want %q", result.Message, want)
	}
	if len(result.Details) != 2 || !strings.HasPrefix(result.Details[0], first+" ") || !strings.HasPrefix(result.Details[1], second+" ") {
		t.Fatalf("details = %q, want %s then %s", result.Details, first, second)
	}
	hints := strings.Split(result.FixHint, "\n")
	if len(hints) != 2 || !strings.HasPrefix(hints[0], first+": ") || !strings.HasPrefix(hints[1], second+": ") {
		t.Fatalf("fix hint = %q, want one line each for %s then %s", result.FixHint, first, second)
	}
}

func TestProviderCatalogReadinessAdvisoryIgnoresNeedsAuthForUnreferencedProvider(t *testing.T) {
	cityDir := writeDoctorCityToml(t, `[workspace]
provider = "claude"

[providers.claude]
base = "builtin:claude"

[providers.gemini]
base = "builtin:gemini"
`)
	stubProviderReadiness(t, map[string]api.ReadinessItem{
		"claude": {Name: "claude", DisplayName: "Claude Code", Status: api.ProbeStatusConfigured},
		"gemini": {Name: "gemini", DisplayName: "Gemini CLI", Status: api.ProbeStatusNeedsAuth, Detail: "gemini is installed but not signed in"},
	})

	result := runProviderReadinessAdvisory(cityDir)
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v (message %q), want OK", result.Status, result.Message)
	}
	if want := "configured local providers are explicit"; result.Message != want {
		t.Fatalf("message = %q, want %q", result.Message, want)
	}
	report := strings.ToLower(result.Message + "\n" + strings.Join(result.Details, "\n") + "\n" + result.FixHint)
	if strings.Contains(report, "gemini") {
		t.Fatalf("unreferenced gemini must not be reported: message=%q details=%q hint=%q", result.Message, result.Details, result.FixHint)
	}
}

func TestProviderCatalogReadinessAdvisoryNeedsAuthFollowsInitProbeMapping(t *testing.T) {
	cases := []struct {
		name       string
		toml       string
		wantDetail string
	}{
		{
			name:       "path_check alias",
			toml:       "[workspace]\nprovider = \"claude-fast\"\n\n[providers.claude-fast]\npath_check = \"claude\"\n",
			wantDetail: `Claude Code needs authentication (referenced as provider "claude-fast"): claude is installed but not logged in`,
		},
		{
			name:       "alias that only names a builtin base",
			toml:       "[workspace]\nprovider = \"claude-opus\"\n\n[providers.claude-opus]\nbase = \"builtin:claude\"\n",
			wantDetail: `Claude Code needs authentication (referenced as provider "claude-opus"): claude is installed but not logged in`,
		},
		{
			name:       "direct ref and alias share one finding",
			toml:       "[workspace]\nprovider = \"claude\"\n\n[providers.claude]\nbase = \"builtin:claude\"\n\n[providers.claude-fast]\ncommand = \"claude\"\npath_check = \"claude\"\n\n[[agent]]\nname = \"worker\"\nprovider = \"claude-fast\"\n",
			wantDetail: `Claude Code needs authentication (referenced as providers "claude", "claude-fast"): claude is installed but not logged in`,
		},
		{
			name: "wrapper alias command is not probe-supported",
			toml: "[workspace]\nprovider = \"wrapped\"\n\n[providers.wrapped]\ncommand = \"/opt/bin/bwrap-agent\"\n",
		},
		{
			name: "agent with start_command",
			toml: "[workspace]\nprovider = \"wrapped\"\n\n[providers.wrapped]\ncommand = \"/opt/bin/bwrap-agent\"\n\n[[agent]]\nname = \"worker\"\nprovider = \"claude\"\nstart_command = \"claude --resume\"\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityDir := writeDoctorCityToml(t, tc.toml)
			cfg, err := loadCityConfigAllowMissingProviderReferences(cityDir)
			if err != nil {
				t.Fatalf("loading config: %v", err)
			}
			initProbes := false
			for _, ref := range explicitProviderRefs(cfg) {
				if providerReadinessProbeName(ref, cfg) == "claude" {
					initProbes = true
				}
			}
			if initProbes != (tc.wantDetail != "") {
				t.Fatalf("precondition: gc init probes claude = %v (refs %q), but case expects a finding = %v", initProbes, explicitProviderRefs(cfg), tc.wantDetail != "")
			}
			stubProviderReadiness(t, map[string]api.ReadinessItem{"claude": claudeNeedsAuthItem})

			result := runProviderReadinessAdvisory(cityDir)
			if tc.wantDetail == "" {
				if result.Status != doctor.StatusOK {
					t.Fatalf("status = %v (message %q, details %q), want OK: gc init would not probe claude", result.Status, result.Message, result.Details)
				}
				return
			}
			if result.Status != doctor.StatusWarning || result.Severity != doctor.SeverityAdvisory {
				t.Fatalf("status/severity = %v/%v (message %q), want advisory warning", result.Status, result.Severity, result.Message)
			}
			if want := "1 referenced provider(s) need authentication: Claude Code"; result.Message != want {
				t.Fatalf("message = %q, want %q", result.Message, want)
			}
			if len(result.Details) != 1 || result.Details[0] != tc.wantDetail {
				t.Fatalf("details = %q, want [%q]", result.Details, tc.wantDetail)
			}
		})
	}
}

func TestProviderCatalogReadinessAdvisoryReportsNeedsAuthAndUnlistedTogether(t *testing.T) {
	cityDir := writeDoctorCityToml(t, "[workspace]\nprovider = \"claude\"\n\n[providers.claude]\nbase = \"builtin:claude\"\n")
	stubProviderReadiness(t, map[string]api.ReadinessItem{
		"claude": claudeNeedsAuthItem,
		"codex":  {Name: "codex", DisplayName: "Codex", Status: api.ProbeStatusConfigured},
	})

	result := runProviderReadinessAdvisory(cityDir)
	if result.Status != doctor.StatusWarning || result.Severity != doctor.SeverityAdvisory {
		t.Fatalf("status/severity = %v/%v (message %q), want advisory warning", result.Status, result.Severity, result.Message)
	}
	if want := "1 referenced provider(s) need authentication: Claude Code; 1 configured local provider(s) not explicit"; result.Message != want {
		t.Fatalf("message = %q, want %q", result.Message, want)
	}
	if len(result.Details) != 2 ||
		!strings.HasPrefix(result.Details[0], "Claude Code needs authentication") ||
		result.Details[1] != "Codex is configured locally but not listed in [providers.codex]" {
		t.Fatalf("details = %q, want the auth finding then the codex not-explicit finding", result.Details)
	}
	wantHint := "Claude Code: " + providerStatusFixHint("claude", api.ProbeStatusNeedsAuth) + "\n" +
		"add provider aliases for local CLIs you want this city to use"
	if result.FixHint != wantHint {
		t.Fatalf("fix hint = %q, want %q", result.FixHint, wantHint)
	}
}

func TestProviderCatalogReadinessAdvisoryUnchangedWithoutNeedsAuth(t *testing.T) {
	t.Run("not explicit only", func(t *testing.T) {
		cityDir := writeDoctorCityToml(t, "[workspace]\nprovider = \"claude\"\n\n[providers.claude]\nbase = \"builtin:claude\"\n")
		stubProviderReadiness(t, map[string]api.ReadinessItem{
			"claude": {Name: "claude", DisplayName: "Claude Code", Status: api.ProbeStatusConfigured},
			"codex":  {Name: "codex", DisplayName: "Codex", Status: api.ProbeStatusConfigured},
		})
		result := runProviderReadinessAdvisory(cityDir)
		if result.Status != doctor.StatusWarning || result.Severity != doctor.SeverityAdvisory {
			t.Fatalf("status/severity = %v/%v, want advisory warning", result.Status, result.Severity)
		}
		if want := "1 configured local provider(s) not explicit"; result.Message != want {
			t.Fatalf("message = %q, want %q", result.Message, want)
		}
		if len(result.Details) != 1 || result.Details[0] != "Codex is configured locally but not listed in [providers.codex]" {
			t.Fatalf("details = %q", result.Details)
		}
		if want := "add provider aliases for local CLIs you want this city to use"; result.FixHint != want {
			t.Fatalf("fix hint = %q, want %q", result.FixHint, want)
		}
	})
	for _, status := range []string{api.ProbeStatusInvalidConfiguration, api.ProbeStatusProbeError, api.ProbeStatusNotInstalled} {
		t.Run("referenced "+status, func(t *testing.T) {
			cityDir := writeDoctorCityToml(t, "[workspace]\nprovider = \"claude\"\n\n[providers.claude]\nbase = \"builtin:claude\"\n")
			stubProviderReadiness(t, map[string]api.ReadinessItem{
				"claude": {Name: "claude", DisplayName: "Claude Code", Status: status, Detail: "probe detail"},
			})
			result := runProviderReadinessAdvisory(cityDir)
			if result.Status != doctor.StatusOK || result.Message != "configured local providers are explicit" ||
				len(result.Details) != 0 || result.FixHint != "" {
				t.Fatalf("result = %+v, want unchanged OK result", result)
			}
		})
	}
}
