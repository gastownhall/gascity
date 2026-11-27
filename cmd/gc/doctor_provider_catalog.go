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
	referenced := referencedReadinessProviders(cfg)
	var auth []providerAuthFinding
	var missing []string
	for _, name := range names {
		item, ok := items[name]
		if !ok {
			continue
		}
		displayName := strings.TrimSpace(item.DisplayName)
		if displayName == "" {
			displayName = name
		}
		switch item.Status {
		case api.ProbeStatusNeedsAuth:
			refs := referenced[name]
			if len(refs) == 0 {
				continue
			}
			auth = append(auth, providerAuthFinding{
				probeName:   name,
				displayName: displayName,
				refs:        refs,
				detail:      strings.TrimSpace(item.Detail),
			})
		case api.ProbeStatusConfigured:
			if _, explicit := cfg.Providers[name]; explicit {
				continue
			}
			missing = append(missing, fmt.Sprintf("%s is configured locally but not listed in [providers.%s]", displayName, name))
		}
	}
	if len(auth) == 0 && len(missing) == 0 {
		r.Status = doctor.StatusOK
		r.Message = "configured local providers are explicit"
		return r
	}
	r.Status = doctor.StatusWarning
	r.Severity = doctor.SeverityAdvisory
	var messages, hints []string
	if len(auth) > 0 {
		displayNames := make([]string, 0, len(auth))
		for _, finding := range auth {
			displayNames = append(displayNames, finding.displayName)
			r.Details = append(r.Details, finding.detailLine())
			if hint := finding.fixHint(); hint != "" {
				hints = append(hints, finding.displayName+": "+hint)
			}
		}
		messages = append(messages, fmt.Sprintf("%d referenced provider(s) need authentication: %s", len(auth), strings.Join(displayNames, ", ")))
	}
	if len(missing) > 0 {
		messages = append(messages, fmt.Sprintf("%d configured local provider(s) not explicit", len(missing)))
		r.Details = append(r.Details, missing...)
		hints = append(hints, "add provider aliases for local CLIs you want this city to use")
	}
	r.Message = strings.Join(messages, "; ")
	r.FixHint = strings.Join(hints, "\n")
	return r
}

// providerAuthFinding is a provider CLI the city references whose fresh
// readiness probe reported that it needs authentication.
type providerAuthFinding struct {
	probeName   string
	displayName string
	refs        []string
	detail      string
}

func (f providerAuthFinding) detailLine() string {
	quoted := make([]string, 0, len(f.refs))
	for _, ref := range f.refs {
		quoted = append(quoted, fmt.Sprintf("%q", ref))
	}
	noun := "provider"
	if len(quoted) > 1 {
		noun = "providers"
	}
	line := fmt.Sprintf("%s %s (referenced as %s %s)", f.displayName, providerStatusSummary(api.ProbeStatusNeedsAuth), noun, strings.Join(quoted, ", "))
	if f.detail != "" {
		line += ": " + f.detail
	}
	return line
}

func (f providerAuthFinding) fixHint() string {
	if hint := providerStatusFixHint(f.probeName, api.ProbeStatusNeedsAuth); hint != "" {
		return hint
	}
	return f.detail
}

// referencedReadinessProviders maps each readiness probe name to the sorted
// provider keys the city references through it, using gc init's reference
// rules and probe mapping. Implicit agents, which config loading injects for
// every [providers.*] entry, do not count as references: listing a provider
// is not using it. References that no readiness probe can verify are omitted.
func referencedReadinessProviders(cfg *config.City) map[string][]string {
	declared := make([]config.Agent, 0, len(cfg.Agents))
	for _, agent := range cfg.Agents {
		if !agent.Implicit {
			declared = append(declared, agent)
		}
	}
	referenced := make(map[string][]string)
	for _, ref := range providerRefsFor(cfg.Workspace.Provider, declared) {
		probe := providerReadinessProbeName(ref, cfg)
		if probe == "" {
			continue
		}
		referenced[probe] = append(referenced[probe], ref)
	}
	return referenced
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
