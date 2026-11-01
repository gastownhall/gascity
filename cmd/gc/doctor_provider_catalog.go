package main

import (
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
	"github.com/gastownhall/gascity/internal/providerreadiness"
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

var doctorProbeCatalogReadiness = api.ProbeProviders

type providerCatalogReadinessAdvisoryCheck struct {
	cityPath string
}

func newProviderCatalogReadinessAdvisoryCheck(cityPath string) *providerCatalogReadinessAdvisoryCheck {
	return &providerCatalogReadinessAdvisoryCheck{cityPath: cityPath}
}

func (c *providerCatalogReadinessAdvisoryCheck) Name() string {
	return "provider-catalog-local-readiness"
}

func (c *providerCatalogReadinessAdvisoryCheck) Run(checkCtx *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name()}
	ctx, cancel := checkCtx.Context()
	defer cancel()
	if ctx.Err() != nil {
		r.Status = doctor.StatusError
		r.Message = "provider readiness check canceled"
		return r
	}
	cfg, err := loadCityConfigAllowMissingProviderReferences(c.cityPath)
	if err != nil {
		r.Status = doctor.StatusOK
		r.Message = "local provider advisory skipped until expanded config loads"
		return r
	}
	targets, notes, err := collectInitProviderTargets(cfg)
	if err != nil {
		r.Status = doctor.StatusError
		r.Message = fmt.Sprintf("could not resolve selected provider: %v", err)
		r.FixHint = "fix the selected provider configuration and rerun gc doctor"
		return r
	}
	items, err := initProbeProvidersReadiness(ctx, targets, true)
	if err != nil {
		r.Status = doctor.StatusError
		r.Message = fmt.Sprintf("could not verify selected provider readiness: %v", err)
		r.FixHint = "restore the configured provider and rerun gc doctor"
		return r
	}
	var unavailable []string
	for _, target := range targets {
		item, ok := items[target.ProbeName]
		if ok && item.Status == api.ProbeStatusConfigured {
			continue
		}
		if ok && item.Status == providerreadiness.Unverified {
			notes = append(notes, fmt.Sprintf("%s: %s", target.DisplayName, item.Detail))
			continue
		}
		if !ok {
			item.Status = api.ProbeStatusProbeError
			item.Detail = "provider returned no readiness verdict"
		}
		detail := fmt.Sprintf("%s [%s] %s (command %q)", target.DisplayName, target.RefName, providerStatusSummary(item.Status), target.Runtime.Command)
		if item.Detail != "" {
			detail += ": " + item.Detail
		}
		unavailable = append(unavailable, detail)
	}
	if len(unavailable) > 0 {
		r.Status = doctor.StatusError
		r.Severity = doctor.SeverityBlocking
		r.Message = fmt.Sprintf("%d selected provider(s) not ready", len(unavailable))
		r.Details = unavailable
		r.FixHint = "restore the listed configured provider executable, configuration or login, then rerun gc doctor"
		return r
	}
	// Discovering other local providers is advisory. Its onboarding policy is
	// not the readiness contract of an explicitly configured city runtime.
	names := api.ProviderReadinessNames()
	catalog, catalogErr := doctorProbeCatalogReadiness(ctx, names, true)
	if catalogErr != nil {
		notes = append(notes, fmt.Sprintf("could not discover other local providers: %v", catalogErr))
	} else {
		for _, name := range names {
			item, ok := catalog[name]
			if !ok || item.Status != api.ProbeStatusConfigured {
				continue
			}
			if _, explicit := cfg.Providers[name]; explicit {
				continue
			}
			display := strings.TrimSpace(item.DisplayName)
			if display == "" {
				display = name
			}
			notes = append(notes, fmt.Sprintf("%s is configured locally but not listed in [providers.%s]", display, name))
		}
	}
	if len(notes) > 0 {
		r.Status = doctor.StatusWarning
		r.Severity = doctor.SeverityAdvisory
		r.Message = "local provider configuration needs review"
		r.Details = notes
		r.FixHint = "verify unprobed providers with a real task; add aliases for other local CLIs this city should use"
		return r
	}
	r.Status = doctor.StatusOK
	r.Message = "selected providers are locally configured; other configured local providers are explicit"
	return r
}

func (c *providerCatalogReadinessAdvisoryCheck) CanFix() bool { return false }

func (c *providerCatalogReadinessAdvisoryCheck) Fix(_ *doctor.CheckContext) error { return nil }

func (c *providerCatalogReadinessAdvisoryCheck) WarmupEligible() bool { return false }

func loadCityConfigAllowMissingProviderReferences(cityPath string) (*config.City, error) {
	tomlPath := filepath.Join(cityPath, citylayout.CityConfigFile)
	if err := ensureBuiltinPacksForConfigLoad(fsys.OSFS{}, tomlPath, io.Discard); err != nil {
		return nil, err
	}
	cfg, _, err := config.LoadWithIncludesOptions(
		fsys.OSFS{},
		tomlPath,
		config.LoadOptions{AllowMissingProviderReferences: true},
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

// appendBuiltinProviderAliases is exempt from config.ResolveCityRewritePath:
// os.WriteFile truncates through a symlinked city.toml and the append keeps
// every existing byte, so neither link identity nor unknown keys are at
// risk. Switching this to a temp-file + rename write revokes the exemption.
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
