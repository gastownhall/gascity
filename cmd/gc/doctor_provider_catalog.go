package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
)

type providerCatalogDoctorCheck struct {
	cityPath string
}

func newProviderCatalogDoctorCheck(cityPath string) *providerCatalogDoctorCheck {
	return &providerCatalogDoctorCheck{cityPath: cityPath}
}

func (c *providerCatalogDoctorCheck) Name() string { return "provider-catalog" }

func (c *providerCatalogDoctorCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name()}
	cfg, err := loadCityConfigAllowMissingProviderReferences(c.cityPath)
	if err != nil {
		r.Status = doctor.StatusOK
		r.Message = "provider catalog check skipped until expanded config loads"
		return r
	}
	refs := config.MissingProviderReferences(cfg)
	if len(refs) == 0 {
		r.Status = doctor.StatusOK
		r.Message = "provider references are explicit"
		return r
	}
	r.Status = doctor.StatusError
	r.Message = fmt.Sprintf("%d provider reference(s) missing from [providers]", len(refs))
	r.Details = providerReferenceDetails(refs)
	if len(missingBuiltinProviderRefs(refs)) > 0 {
		r.FixHint = "run `gc doctor --fix` to add missing builtin provider aliases"
	} else {
		r.FixHint = "add the missing [providers.*] entries manually"
	}
	return r
}

func (c *providerCatalogDoctorCheck) CanFix() bool {
	cfg, err := loadCityConfigAllowMissingProviderReferences(c.cityPath)
	if err != nil {
		return false
	}
	return len(missingBuiltinProviderRefs(config.MissingProviderReferences(cfg))) > 0
}

func (c *providerCatalogDoctorCheck) Fix(_ *doctor.CheckContext) error {
	cfg, err := loadCityConfigAllowMissingProviderReferences(c.cityPath)
	if err != nil {
		return err
	}
	return appendBuiltinProviderAliases(c.cityPath, missingBuiltinProviderRefs(config.MissingProviderReferences(cfg)))
}

func (c *providerCatalogDoctorCheck) WarmupEligible() bool { return false }

type providerCatalogReadinessAdvisoryCheck struct {
	cityPath string
}

func newProviderCatalogReadinessAdvisoryCheck(cityPath string) *providerCatalogReadinessAdvisoryCheck {
	return &providerCatalogReadinessAdvisoryCheck{cityPath: cityPath}
}

func (c *providerCatalogReadinessAdvisoryCheck) Name() string {
	return "provider-catalog-local-readiness"
}

func (c *providerCatalogReadinessAdvisoryCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name()}
	cfg, err := loadCityConfigAllowMissingProviderReferences(c.cityPath)
	if err != nil {
		r.Status = doctor.StatusOK
		r.Message = "local provider advisory skipped until expanded config loads"
		return r
	}
	names := api.ProviderReadinessNames()
	items, err := initProbeProvidersReadiness(context.Background(), names, true)
	if err != nil {
		r.Status = doctor.StatusWarning
		r.Message = fmt.Sprintf("could not probe local provider readiness: %v", err)
		r.Severity = doctor.SeverityAdvisory
		return r
	}
	var missing []string
	for _, name := range names {
		item, ok := items[name]
		if !ok || item.Status != api.ProbeStatusConfigured {
			continue
		}
		if _, explicit := cfg.Providers[name]; explicit {
			continue
		}
		displayName := strings.TrimSpace(item.DisplayName)
		if displayName == "" {
			displayName = name
		}
		missing = append(missing, fmt.Sprintf("%s is configured locally but not listed in [providers.%s]", displayName, name))
	}
	if len(missing) == 0 {
		r.Status = doctor.StatusOK
		r.Message = "configured local providers are explicit"
		return r
	}
	r.Status = doctor.StatusWarning
	r.Severity = doctor.SeverityAdvisory
	r.Message = fmt.Sprintf("%d configured local provider(s) not explicit", len(missing))
	r.Details = missing
	r.FixHint = "add provider aliases for local CLIs you want this city to use"
	return r
}

func (c *providerCatalogReadinessAdvisoryCheck) CanFix() bool { return false }

func (c *providerCatalogReadinessAdvisoryCheck) Fix(_ *doctor.CheckContext) error { return nil }

func (c *providerCatalogReadinessAdvisoryCheck) WarmupEligible() bool { return false }

func loadCityConfigAllowMissingProviderReferences(cityPath string) (*config.City, error) {
	tomlPath := filepath.Join(cityPath, citylayout.CityConfigFile)
	extras, err := builtinPackIncludesForConfigLoad(fsys.OSFS{}, tomlPath, io.Discard)
	if err != nil {
		return nil, err
	}
	cfg, _, err := config.LoadWithIncludesOptions(
		fsys.OSFS{},
		tomlPath,
		config.LoadOptions{AllowMissingProviderReferences: true},
		extras...,
	)
	if err != nil {
		return nil, err
	}
	applyFeatureFlags(cfg)
	return cfg, nil
}

func providerReferenceDetails(refs []config.ProviderReference) []string {
	details := make([]string, 0, len(refs))
	for _, ref := range refs {
		switch ref.Kind {
		case "workspace":
			details = append(details, fmt.Sprintf("workspace.provider %q", ref.Provider))
		case "agent":
			details = append(details, fmt.Sprintf("agent %q provider %q", ref.Agent, ref.Provider))
		default:
			details = append(details, fmt.Sprintf("%s provider %q", ref.Kind, ref.Provider))
		}
	}
	return details
}

func missingBuiltinProviderRefs(refs []config.ProviderReference) []string {
	builtins := config.BuiltinProviders()
	seen := make(map[string]bool)
	for _, ref := range refs {
		if _, ok := builtins[ref.Provider]; !ok {
			continue
		}
		seen[ref.Provider] = true
	}
	var out []string
	for _, name := range config.BuiltinProviderOrder() {
		if seen[name] {
			out = append(out, name)
		}
	}
	return out
}

func appendBuiltinProviderAliases(cityPath string, providers []string) error {
	if len(providers) == 0 {
		return nil
	}
	tomlPath := filepath.Join(cityPath, citylayout.CityConfigFile)
	data, err := os.ReadFile(tomlPath)
	if err != nil {
		return err
	}
	var b strings.Builder
	if len(data) > 0 && data[len(data)-1] != '\n' {
		b.WriteString("\n")
	}
	for _, provider := range providers {
		b.WriteString("\n")
		b.WriteString("[providers.")
		b.WriteString(provider)
		b.WriteString("]\n")
		b.WriteString("base = \"")
		b.WriteString(config.BasePrefixBuiltin)
		b.WriteString(provider)
		b.WriteString("\"\n")
	}
	return os.WriteFile(tomlPath, append(data, []byte(b.String())...), 0o644)
}
