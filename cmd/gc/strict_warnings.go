package main

import "github.com/gastownhall/gascity/internal/config"

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
	return config.IsNonFatalSiteBindingWarning(warning)
}
