package config

import (
	"fmt"
	"strings"
)

// ValidateSemantics checks cross-entity semantic constraints in the config
// and returns warnings for issues that cannot be caught by individual struct
// validation. Unlike ValidateAgents (which returns hard errors), semantic
// warnings are non-fatal — they indicate likely misconfigurations but don't
// prevent the system from starting.
func ValidateSemantics(cfg *City, source string) []string {
	var warnings []string

	// Build known provider name set: built-in + city-defined.
	knownProviders := make(map[string]bool)
	for name := range BuiltinProviders() {
		knownProviders[name] = true
	}
	for name := range cfg.Providers {
		knownProviders[name] = true
	}

	// Check provider references on agents.
	for _, a := range cfg.Agents {
		if a.Provider == "" || a.StartCommand != "" {
			continue // no provider lookup needed
		}
		if !knownProviders[a.Provider] {
			warnings = append(warnings, fmt.Sprintf(
				"%s: agent %q: provider %q is not a built-in or city-defined provider",
				source, a.QualifiedName(), a.Provider))
		}
	}

	// Check workspace default provider.
	if p := cfg.Workspace.Provider; p != "" {
		if !knownProviders[p] {
			warnings = append(warnings, fmt.Sprintf(
				"%s: [workspace] provider %q is not a built-in or city-defined provider",
				source, p))
		}
	}

	// Check agent session field.
	for _, a := range cfg.Agents {
		if a.Session != "" && a.Session != "acp" {
			warnings = append(warnings, fmt.Sprintf(
				"%s: agent %q: session %q is not a valid session transport (use \"acp\" or omit)",
				source, a.QualifiedName(), a.Session))
		}
	}

	// Check namepool on unlimited pools (discovery uses prefix matching,
	// which won't find themed names).
	for _, a := range cfg.Agents {
		if a.Namepool != "" && a.MaxActiveSessions != nil && *a.MaxActiveSessions < 0 {
			warnings = append(warnings, fmt.Sprintf(
				"%s: agent %q: namepool requires bounded max_active_sessions (> 0); unlimited agents use prefix discovery which cannot find themed names",
				source, a.QualifiedName()))
		}
	}

	// Check overlapping idle lifecycle controls.
	for _, a := range cfg.Agents {
		if a.IdleTimeout != "" && a.SleepAfterIdle != "" {
			warnings = append(warnings, fmt.Sprintf(
				"%s: agent %q: idle_timeout and sleep_after_idle are both set; idle_timeout takes precedence and sleep_after_idle only applies when the session survives the idle_timeout check",
				source, a.QualifiedName()))
		}
	}

	// Custom provider names must not contain the reserved ":" character
	// (used by the base = "builtin:..." / "provider:..." namespace prefixes).
	for name := range cfg.Providers {
		if strings.Contains(name, ":") {
			warnings = append(warnings, fmt.Sprintf(
				"%s: [providers.%s] custom provider name contains reserved character \":\" (used for \"builtin:\" / \"provider:\" namespace prefixes on base field)",
				source, name))
		}
	}

	// Validate base field grammar when set.
	for name, spec := range cfg.Providers {
		if spec.Base == nil {
			continue
		}
		bv := *spec.Base
		if bv == "" {
			continue // explicit standalone opt-out is valid
		}
		switch {
		case strings.HasPrefix(bv, BasePrefixBuiltin):
			suffix := strings.TrimPrefix(bv, BasePrefixBuiltin)
			if suffix == "" {
				warnings = append(warnings, fmt.Sprintf(
					"%s: [providers.%s] base %q has empty suffix after %q prefix",
					source, name, bv, BasePrefixBuiltin))
			}
		case strings.HasPrefix(bv, BasePrefixProvider):
			suffix := strings.TrimPrefix(bv, BasePrefixProvider)
			if suffix == "" {
				warnings = append(warnings, fmt.Sprintf(
					"%s: [providers.%s] base %q has empty suffix after %q prefix",
					source, name, bv, BasePrefixProvider))
			}
		}
	}

	// Validate options_schema_merge grammar.
	for name, spec := range cfg.Providers {
		switch spec.OptionsSchemaMerge {
		case "", "replace", "by_key":
			// valid
		default:
			warnings = append(warnings, fmt.Sprintf(
				"%s: [providers.%s] options_schema_merge must be \"replace\" or \"by_key\", got %q",
				source, name, spec.OptionsSchemaMerge))
		}
	}

	// Check PromptMode on city-defined providers.
	for name, spec := range cfg.Providers {
		switch spec.PromptMode {
		case "", "arg", "flag", "none":
			// valid
		default:
			warnings = append(warnings, fmt.Sprintf(
				"%s: [providers.%s] prompt_mode must be \"arg\", \"flag\", \"none\", or empty, got %q",
				source, name, spec.PromptMode))
		}
		if spec.PromptMode == "flag" && spec.PromptFlag == "" {
			warnings = append(warnings, fmt.Sprintf(
				"%s: [providers.%s] prompt_flag is required when prompt_mode = \"flag\"",
				source, name))
		}
	}

	return warnings
}
