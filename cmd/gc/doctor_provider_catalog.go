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

// providerLocalAuthCheck reports providers this city is configured to use whose
// local CLI is installed but not logged in.
//
// The readiness probe already computes this exact condition
// (api.ProbeStatusNeedsAuth, "<cli> is installed but not logged in"), but until
// now only `gc init` consumed it. providerCatalogReadinessAdvisoryCheck probes
// every provider and then skips anything that is not ProbeStatusConfigured, so a
// logged-out CLI fell straight through the one command an operator runs to ask
// "is this city healthy?".
//
// That silence is expensive: an agent whose provider CLI cannot authenticate
// still spawns, still holds a live pane, and still reports its session active --
// it simply never claims work. Nothing else in doctor distinguishes that from an
// idle-but-healthy lane.
//
// Scope is deliberately narrow. Only providers with an explicit [providers.*]
// entry are reported, so a city that never uses a locally installed CLI is not
// nagged about it. An entry counts whether it names a canonical provider
// directly or is an alias resolving to one through a base chain (e.g.
// [providers.claude-fast] base = "builtin:claude").
type providerLocalAuthCheck struct {
	cityPath string
}

func newProviderLocalAuthCheck(cityPath string) *providerLocalAuthCheck {
	return &providerLocalAuthCheck{cityPath: cityPath}
}

func (c *providerLocalAuthCheck) Name() string { return "provider-local-auth" }

func (c *providerLocalAuthCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	// A logged-out provider is reported as StatusWarning, which doctor only
	// counts in Report.Warned; it never contributes to BlockingFailed. The
	// severity is still declared advisory up front because SeverityBlocking is
	// CheckSeverity's zero value, so this check stays non-gating if a path is
	// ever changed to report StatusError.
	r := &doctor.CheckResult{Name: c.Name(), Severity: doctor.SeverityAdvisory}
	cfg, err := loadCityConfigAllowMissingProviderReferences(c.cityPath)
	if err != nil {
		r.Status = doctor.StatusOK
		r.Message = "local provider auth advisory skipped until expanded config loads"
		return r
	}
	names := api.ProviderReadinessNames()
	items, err := initProbeProvidersReadiness(context.Background(), names, true)
	if err != nil {
		r.Status = doctor.StatusWarning
		r.Message = fmt.Sprintf("could not probe local provider readiness: %v", err)
		return r
	}
	configuredProbeNames := explicitProviderProbeNames(cfg)
	var details []string
	var hints []string
	seenHint := map[string]bool{}
	for _, name := range names {
		item, ok := items[name]
		if !ok || item.Status != api.ProbeStatusNeedsAuth {
			continue
		}
		if !configuredProbeNames[name] {
			continue
		}
		displayName := strings.TrimSpace(item.DisplayName)
		if displayName == "" {
			displayName = name
		}
		detail := strings.TrimSpace(item.Detail)
		if detail == "" {
			detail = "installed but not logged in"
		}
		details = append(details, fmt.Sprintf("%s: %s", displayName, detail))
		if hint := providerStatusFixHint(name, api.ProbeStatusNeedsAuth); hint != "" && !seenHint[hint] {
			seenHint[hint] = true
			hints = append(hints, hint)
		}
	}
	if len(details) == 0 {
		r.Status = doctor.StatusOK
		r.Message = "configured local providers are logged in"
		return r
	}
	r.Status = doctor.StatusWarning
	r.Message = fmt.Sprintf("%d configured local provider(s) not logged in", len(details))
	r.Details = details
	r.FixHint = strings.Join(hints, "; ")
	return r
}

func (c *providerLocalAuthCheck) CanFix() bool { return false }

func (c *providerLocalAuthCheck) Fix(_ *doctor.CheckContext) error { return nil }

func (c *providerLocalAuthCheck) WarmupEligible() bool { return false }

// explicitProviderProbeNames resolves every [providers.*] entry this city
// declares -- canonical names and aliases alike -- to the readiness probe
// name it maps to, reusing the same alias resolution `gc init` relies on. A
// configured alias such as [providers.claude-fast] base = "builtin:claude"
// resolves to "claude", the same probe name a direct [providers.claude]
// entry would use, so both count as explicit.
func explicitProviderProbeNames(cfg *config.City) map[string]bool {
	probeNames := make(map[string]bool, len(cfg.Providers))
	for ref := range cfg.Providers {
		if probeName := providerReadinessProbeName(ref, cfg); probeName != "" {
			probeNames[probeName] = true
		}
	}
	return probeNames
}
