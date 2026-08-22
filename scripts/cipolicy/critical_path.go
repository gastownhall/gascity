package cipolicy

import "fmt"

// criticalPathEvidenceNeeds is the exact dependency set the
// critical-path-evidence job must declare: runner-policy and changes (its
// evaluation inputs) plus every job that
// .github/workflows/scripts/critical_path_evidence.py's CRITICAL_PATH_JOBS
// maps a category to. Missing or extra entries both weaken the evidence the
// job can vouch for, so the count and membership are checked exactly.
var criticalPathEvidenceNeeds = map[string]bool{
	"runner-policy":              true,
	"changes":                    true,
	"cmd-gc-process":             true,
	"integration-shards":         true,
	"worker-core-summary":        true,
	"worker-core-phase2-summary": true,
	"pack-gate":                  true,
	"docker-session":             true,
	"k8s-session":                true,
	"openclaw-bridge":            true,
}

// validateCriticalPathEvidenceJob checks that critical-path-evidence is
// wired as an unconditional (if: always()) job that depends on exactly
// runner-policy, changes, and every mapped critical-path job, and that it is
// itself a dependency of ci-required. It does not evaluate per-category
// matched/result outcomes -- that runtime cross-reference lives in
// .github/workflows/scripts/critical_path_evidence.py and is exercised by
// that module's own test suite. This check only guards the static wiring
// that makes the evidence job unconditional and unavoidable: without it, a
// matched-but-skipped suite could still slip past ci-required's
// allow_skipped list undetected.
func validateCriticalPathEvidenceJob(workflow map[string]any) error {
	evidenceJob, err := workflowJob(workflow, "critical-path-evidence")
	if err != nil {
		return err
	}

	if got, _ := evidenceJob["if"].(string); got != "${{ always() }}" {
		return fmt.Errorf("critical-path-evidence must be unconditional (if: ${{ always() }}), got %q", got)
	}

	needs, err := jobNeeds(evidenceJob, "critical-path-evidence")
	if err != nil {
		return err
	}
	if len(needs) != len(criticalPathEvidenceNeeds) {
		return fmt.Errorf(
			"critical-path-evidence needs must depend on exactly runner-policy, changes, and every mapped critical-path job (got %d, want %d)",
			len(needs),
			len(criticalPathEvidenceNeeds),
		)
	}
	seen := make(map[string]bool, len(needs))
	for _, name := range needs {
		if !criticalPathEvidenceNeeds[name] {
			return fmt.Errorf("critical-path-evidence needs an unexpected dependency %q", name)
		}
		if seen[name] {
			return fmt.Errorf("critical-path-evidence needs lists %q more than once", name)
		}
		seen[name] = true
	}

	requiredJob, err := workflowJob(workflow, "ci-required")
	if err != nil {
		return err
	}
	requiredNeeds, err := jobNeeds(requiredJob, "ci-required")
	if err != nil {
		return err
	}
	for _, name := range requiredNeeds {
		if name == "critical-path-evidence" {
			return nil
		}
	}
	return fmt.Errorf("critical-path-evidence must be a non-skippable dependency of ci-required")
}

// jobNeeds returns a job's `needs:` list as strings, failing closed if the
// field is missing or contains a non-string entry.
func jobNeeds(job map[string]any, label string) ([]string, error) {
	items, ok := job["needs"].([]any)
	if !ok {
		return nil, fmt.Errorf("%s needs must be a list", label)
	}
	needs := make([]string, 0, len(items))
	for _, item := range items {
		name, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("%s needs must be a list of strings", label)
		}
		needs = append(needs, name)
	}
	return needs, nil
}
