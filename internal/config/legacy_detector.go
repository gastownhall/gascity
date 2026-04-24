package config

import (
	"fmt"
	"sort"
)

// DetectLegacyProviderInheritance emits one Phase A warning per custom
// provider that appears to be relying on the legacy name-match or
// command-match auto-inheritance. Users can silence the warning by
// setting `base` explicitly — either `base = "builtin:<name>"` to opt
// in, or `base = ""` to opt out (declare standalone).
//
// This function runs alongside ValidateSemantics during config load and
// contributes to Provenance.Warnings.
//
// The detector fires when BOTH of these are true:
//   - `base` is unset (nil) — i.e. the user has not opted in or out
//   - The provider's name OR command matches a built-in provider name
//
// Output ordering is deterministic (sorted by provider name) so warning
// text is stable across runs / caching.
func DetectLegacyProviderInheritance(cfg *City, source string) []string {
	if cfg == nil || len(cfg.Providers) == 0 {
		return nil
	}
	builtins := BuiltinProviders()
	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)

	var warnings []string
	for _, name := range names {
		spec := cfg.Providers[name]
		if spec.Base != nil {
			continue // user declared something (opt-in or opt-out); no warning
		}
		// Determine which legacy rule would fire.
		_, nameMatch := builtins[name]
		var cmdMatch string
		if spec.Command != "" {
			if _, ok := builtins[spec.Command]; ok {
				cmdMatch = spec.Command
			}
		}
		if !nameMatch && cmdMatch == "" {
			continue
		}
		suggest := name
		if !nameMatch && cmdMatch != "" {
			suggest = cmdMatch
		}
		var reason string
		switch {
		case nameMatch && cmdMatch != "" && cmdMatch != name:
			reason = fmt.Sprintf("name matches built-in %q and command %q matches built-in %q (name-match wins)", name, spec.Command, cmdMatch)
		case nameMatch:
			reason = fmt.Sprintf("name matches built-in %q", name)
		case cmdMatch != "":
			reason = fmt.Sprintf("command %q matches built-in %q", spec.Command, cmdMatch)
		}
		warnings = append(warnings, fmt.Sprintf(
			"%s: [providers.%s] relying on legacy auto-inheritance: %s. "+
				"This becomes a hard error in a future release. Fix: add "+
				"`base = \"builtin:%s\"` to the provider block. If this "+
				"provider should NOT inherit from the built-in, add "+
				"`base = \"\"` to explicitly opt out.",
			source, name, reason, suggest))
	}
	return warnings
}

// HasLegacyInheritanceWarning reports whether any Provenance warning
// originated from DetectLegacyProviderInheritance. Useful for tests
// and migration tooling.
func HasLegacyInheritanceWarning(prov *Provenance) bool {
	if prov == nil {
		return false
	}
	for _, w := range prov.Warnings {
		if containsLegacyDetectorMarker(w) {
			return true
		}
	}
	return false
}

func containsLegacyDetectorMarker(s string) bool {
	return stringContains(s, "relying on legacy auto-inheritance")
}

// stringContains is a tiny helper to avoid importing strings just for Contains.
// (strings is already imported transitively; this stays focused.)
func stringContains(s, substr string) bool {
	return len(substr) == 0 || indexOf(s, substr) >= 0
}

func indexOf(s, substr string) int {
	if len(substr) == 0 {
		return 0
	}
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
