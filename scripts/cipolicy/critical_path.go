package cipolicy

import "fmt"

// validateCriticalPathEvidenceJob is the not-yet-implemented policy check
// for ga-oaz41a.1 (Fail CI when a matched critical-path suite has no
// successful evidence). RED stub: compiles so the package typechecks under
// the lint-changed pre-commit gate, but does not yet validate anything --
// GREEN replaces this body with the real critical-path-evidence job-wiring
// check (unconditional dependency of ci-required, `if: always()`, depends on
// every mapped critical-path job, requires success when its category
// matched and allows skipped only when unmatched).
func validateCriticalPathEvidenceJob(_ map[string]any) error {
	return fmt.Errorf("validateCriticalPathEvidenceJob: not implemented")
}
