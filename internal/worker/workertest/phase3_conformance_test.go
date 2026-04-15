package workertest

import "testing"

func TestPhase3Catalog(t *testing.T) {
	expected := []RequirementCode{
		RequirementInferenceFreshSpawn,
		RequirementInferenceFreshTask,
		RequirementInferenceWorkspaceTask,
		RequirementInferenceTranscript,
		RequirementInferenceContinuation,
	}

	catalog := Phase3Catalog()
	if len(catalog) != len(expected) {
		t.Fatalf("catalog entries = %d, want %d", len(catalog), len(expected))
	}

	seen := make(map[RequirementCode]Requirement, len(catalog))
	for _, requirement := range catalog {
		if requirement.Group == "" {
			t.Fatalf("requirement %s has empty group", requirement.Code)
		}
		if requirement.Description == "" {
			t.Fatalf("requirement %s has empty description", requirement.Code)
		}
		seen[requirement.Code] = requirement
	}
	for _, code := range expected {
		if _, ok := seen[code]; !ok {
			t.Fatalf("catalog missing requirement %s", code)
		}
	}
}

func TestPhase3CatalogReport(t *testing.T) {
	reporter := NewSuiteReporter(t, "phase3", map[string]string{
		"tier":  "worker-inference",
		"phase": "phase3",
		"scope": "catalog-only",
	})

	profiles, err := selectedProfiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, profile := range profiles {
		for _, requirement := range Phase3Catalog() {
			reporter.Record(Unsupported(profile.ID, requirement.Code, "cataloged; executable deterministic scenario pending"))
		}
	}
}
