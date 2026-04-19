package main

import "strings"

// splitStrictConfigWarnings separates warnings that should remain fatal in
// strict mode from compatibility/migration guidance that should stay warnings.
func splitStrictConfigWarnings(warnings []string) (fatal []string, nonFatal []string) {
	for _, warning := range warnings {
		if strictWarningIsNonFatal(warning) {
			nonFatal = append(nonFatal, warning)
			continue
		}
		fatal = append(fatal, warning)
	}
	return fatal, nonFatal
}

func strictWarningIsNonFatal(warning string) bool {
	// Site-binding warnings are compatibility guidance: the loader still
	// returns a usable config by falling back to city.toml paths or leaving
	// rigs unbound. They should be visible, but they must not block startup.
	return strings.Contains(warning, ".gc/site.toml")
}
