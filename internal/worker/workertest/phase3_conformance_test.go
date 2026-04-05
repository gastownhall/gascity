package workertest

import "testing"

func TestPhase3Catalog(t *testing.T) {
	expected := []RequirementCode{
		RequirementStartupCommandMaterialization,
		RequirementStartupRuntimeConfigMaterialization,
		RequirementInputInitialMessageFirstStart,
		RequirementInputInitialMessageResume,
		RequirementInputOverrideDefaults,
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
