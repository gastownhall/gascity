package workertest

import (
	"reflect"
	"testing"
)

func TestPhase2CatalogDataIsAuthoritative(t *testing.T) {
	expected := []Requirement{
		{Code: RequirementStartupOutcomeBound, Group: "startup", Description: "The worker fake surfaces a bounded startup outcome."},
		{Code: RequirementStartupCommandMaterialization, Group: "startup_materialization", Description: "Provider defaults and resolved launch semantics materialize into the startup command for canonical worker profiles."},
		{Code: RequirementStartupRuntimeConfigMaterialization, Group: "startup_materialization", Description: "Resolved workdir, env, and startup hints survive templateParamsToConfig into runtime.Config."},
		{Code: RequirementInputInitialMessageFirstStart, Group: "input_delivery", Description: "A configured initial_message is injected into the first start exactly once."},
		{Code: RequirementInputInitialMessageResume, Group: "input_delivery", Description: "A resumed session does not replay the initial_message after the first start has been recorded."},
		{Code: RequirementInputOverrideDefaults, Group: "input_delivery", Description: "Schema overrides and initial_message handling preserve provider default launch flags while separating first-input delivery from option overrides."},
		{Code: RequirementTranscriptDiagnostics, Group: "transcript", Description: "Malformed or torn transcript data is reported as degraded history or a fail-closed load error instead of clean normalized history."},
		{Code: RequirementInteractionSignal, Group: "interaction", Description: "The standalone fake worker surfaces a blocked structured interaction signal and state."},
		{Code: RequirementInteractionPending, Group: "interaction", Description: "Required structured interactions surface through the runtime interaction seam."},
		{Code: RequirementInteractionRespond, Group: "interaction", Description: "Responding to a pending structured interaction clears the pending state."},
		{Code: RequirementInteractionReject, Group: "interaction", Description: "A mismatched interaction response is rejected without clearing the pending interaction."},
		{Code: RequirementInteractionInstanceLocalDedup, Group: "interaction", Description: "Tmux interaction dedup state is instance-local so one worker session does not suppress another."},
		{Code: RequirementInteractionDurableHistory, Group: "interaction", Description: "Required structured interactions are represented durably in normalized history and the pending transcript tail."},
		{Code: RequirementInteractionLifecycleHistory, Group: "interaction", Description: "Dismissed and resumed-after-restart interaction lifecycle records update normalized tail state deterministically."},
		{Code: RequirementToolEventNormalization, Group: "tool", Description: "Normalized history preserves tool_use/tool_result substrate events."},
		{Code: RequirementToolEventOpenTail, Group: "tool", Description: "Open tool_use events remain visible at the normalized transcript tail when unresolved."},
	}

	catalog := Phase2Catalog()
	if !reflect.DeepEqual(catalog, expected) {
		t.Fatalf("Phase2Catalog() = %#v, want %#v", catalog, expected)
	}

	entries := Phase2CatalogEntries()
	if len(entries) != len(expected) {
		t.Fatalf("Phase2CatalogEntries() = %d, want %d", len(entries), len(expected))
	}
	for i, entry := range entries {
		if !reflect.DeepEqual(entry.Requirement, expected[i]) {
			t.Fatalf("entry %d requirement = %#v, want %#v", i, entry.Requirement, expected[i])
		}
		if entry.Scenario.ID == "" || entry.Scenario.Runner == "" || entry.Scenario.Kind == "" {
			t.Fatalf("entry %d has incomplete scenario metadata: %#v", i, entry.Scenario)
		}
		if !entry.Scenario.Executable {
			t.Fatalf("entry %d scenario is not executable: %#v", i, entry.Scenario)
		}
		if got := entry.Scenario.RequirementCodes; len(got) != 1 || got[0] != entry.Requirement.Code {
			t.Fatalf("entry %d scenario requirement codes = %v, want [%s]", i, got, entry.Requirement.Code)
		}
	}
}

func TestPhase2CatalogDataHasUniqueCodesAndScenarioIDs(t *testing.T) {
	entries := Phase2CatalogEntries()
	scenarios := Phase2Scenarios()

	seenCodes := make(map[RequirementCode]struct{}, len(entries))
	seenScenarioIDs := make(map[string]struct{}, len(scenarios))
	for _, entry := range entries {
		if _, ok := seenCodes[entry.Requirement.Code]; ok {
			t.Fatalf("duplicate requirement code %s", entry.Requirement.Code)
		}
		seenCodes[entry.Requirement.Code] = struct{}{}
		if entry.Scenario.ID == "" {
			t.Fatalf("requirement %s has empty scenario id", entry.Requirement.Code)
		}
		if _, ok := seenScenarioIDs[entry.Scenario.ID]; ok {
			t.Fatalf("duplicate scenario id %s", entry.Scenario.ID)
		}
		seenScenarioIDs[entry.Scenario.ID] = struct{}{}
	}
	if len(seenScenarioIDs) != len(scenarios) {
		t.Fatalf("scenario ids = %d, scenario records = %d", len(seenScenarioIDs), len(scenarios))
	}
}

func TestPhase2CatalogDataReturnsCopies(t *testing.T) {
	first := Phase2Catalog()
	if len(first) == 0 {
		t.Fatal("Phase2Catalog returned no requirements")
	}
	first[0].Description = "mutated"

	second := Phase2Catalog()
	if second[0].Description == "mutated" {
		t.Fatal("Phase2Catalog returned shared mutable backing data")
	}
}

func TestPhase2CatalogScenarioCrossReferences(t *testing.T) {
	for _, entry := range Phase2CatalogEntries() {
		scenario, ok := Phase2ScenarioForID(entry.Scenario.ID)
		if !ok {
			t.Fatalf("scenario %s missing for requirement %s", entry.Scenario.ID, entry.Requirement.Code)
		}
		if scenario.ID != entry.Scenario.ID {
			t.Fatalf("scenario id = %s, want %s", scenario.ID, entry.Scenario.ID)
		}
		if scenario.Description == "" {
			t.Fatalf("scenario %s has empty description", scenario.ID)
		}
		if scenario.Runner != "fake-worker" {
			t.Fatalf("scenario %s runner = %q, want fake-worker", scenario.ID, scenario.Runner)
		}
		if scenario.Phase != "phase2" {
			t.Fatalf("scenario %s phase = %q, want phase2", scenario.ID, scenario.Phase)
		}
		if !reflect.DeepEqual(scenario.Profiles, phase2CatalogProfiles) {
			t.Fatalf("scenario %s profiles = %v, want %v", scenario.ID, scenario.Profiles, phase2CatalogProfiles)
		}
		if len(scenario.RequirementCodes) != 1 || scenario.RequirementCodes[0] != entry.Requirement.Code {
			t.Fatalf("scenario %s requirement codes = %v, want [%s]", scenario.ID, scenario.RequirementCodes, entry.Requirement.Code)
		}
	}
}
