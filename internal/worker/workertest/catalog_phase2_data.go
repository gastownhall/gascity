package workertest

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/phase2/catalog.json testdata/phase2/scenarios.yaml
var phase2CatalogFS embed.FS

const (
	phase2CatalogVersion          = 1
	phase2CatalogRunner           = "fake-worker"
	phase2RealTransportRunner     = "tmux-real-transport"
	phase2CatalogPhase            = "phase2"
	phase2CertificationCertifying = "certifying"
	phase2CertificationProofOnly  = "proof_only"
)

var phase2CatalogProfiles = []ProfileID{
	ProfileClaudeTmuxCLI,
	ProfileCodexTmuxCLI,
	ProfileGeminiTmuxCLI,
	ProfileOpenCodeTmuxCLI,
}

var phase2CatalogOnce struct {
	sync.Once
	bundle phase2CatalogBundle
	err    error
}

// Phase2CatalogEntry binds one stable requirement to the scenario metadata
// that describes how the deterministic slice exercises it.
type Phase2CatalogEntry struct {
	Requirement Requirement
	Scenario    Phase2Scenario
}

// Phase2Scenario describes one executable scenario record in the Phase 2
// worker-conformance slice.
type Phase2Scenario struct {
	ID               string            `json:"id" yaml:"id"`
	Runner           string            `json:"runner" yaml:"runner"`
	Phase            string            `json:"phase" yaml:"phase"`
	Kind             string            `json:"kind" yaml:"kind"`
	Description      string            `json:"description" yaml:"description"`
	Certification    string            `json:"certification" yaml:"certification"`
	Executable       bool              `json:"executable" yaml:"executable"`
	Profiles         []ProfileID       `json:"profiles" yaml:"profiles"`
	RequirementCodes []RequirementCode `json:"requirement_codes" yaml:"requirement_codes"`
}

type phase2CatalogDocument struct {
	Version      int                           `json:"version"`
	Requirements []phase2CatalogRequirementDoc `json:"requirements"`
}

type phase2CatalogRequirementDoc struct {
	Code        RequirementCode `json:"code"`
	Group       string          `json:"group"`
	Description string          `json:"description"`
	ScenarioID  string          `json:"scenario_id"`
}

type phase2ScenarioDocument struct {
	Version   int              `json:"version" yaml:"version"`
	Scenarios []Phase2Scenario `json:"scenarios" yaml:"scenarios"`
}

type phase2CatalogBundle struct {
	requirements []Requirement
	entries      []Phase2CatalogEntry
	scenarios    []Phase2Scenario
	byCode       map[RequirementCode]Phase2CatalogEntry
	byScenarioID map[string]Phase2Scenario
}

func phase2CatalogRequirements() []Requirement {
	return cloneRequirements(phase2CatalogData().requirements)
}

// Phase2CatalogEntries returns the authoritative requirement/scenario joins for
// the deterministic Phase 2 slice.
func Phase2CatalogEntries() []Phase2CatalogEntry {
	return cloneEntries(phase2CatalogData().entries)
}

// Phase2CatalogEntryFor returns the authoritative entry for one requirement code.
func Phase2CatalogEntryFor(code RequirementCode) (Phase2CatalogEntry, bool) {
	entry, ok := phase2CatalogData().byCode[code]
	return cloneEntry(entry), ok
}

// Phase2Scenarios returns the executable scenario metadata for the Phase 2 slice.
func Phase2Scenarios() []Phase2Scenario {
	return cloneScenarios(phase2CatalogData().scenarios)
}

// Phase2ScenarioForID returns the executable scenario metadata for one scenario ID.
func Phase2ScenarioForID(id string) (Phase2Scenario, bool) {
	scenario, ok := phase2CatalogData().byScenarioID[id]
	return cloneScenario(scenario), ok
}

func phase2CatalogData() phase2CatalogBundle {
	phase2CatalogOnce.Do(func() {
		phase2CatalogOnce.bundle, phase2CatalogOnce.err = loadPhase2CatalogBundle()
	})
	if phase2CatalogOnce.err != nil {
		panic(phase2CatalogOnce.err)
	}
	return phase2CatalogOnce.bundle
}

func loadPhase2CatalogBundle() (phase2CatalogBundle, error) {
	catalogData, err := phase2CatalogFS.ReadFile("testdata/phase2/catalog.json")
	if err != nil {
		return phase2CatalogBundle{}, fmt.Errorf("read phase2 catalog: %w", err)
	}
	scenarioData, err := phase2CatalogFS.ReadFile("testdata/phase2/scenarios.yaml")
	if err != nil {
		return phase2CatalogBundle{}, fmt.Errorf("read phase2 scenarios: %w", err)
	}

	var catalogDoc phase2CatalogDocument
	if err := decodeJSONStrict("phase2 catalog", catalogData, &catalogDoc); err != nil {
		return phase2CatalogBundle{}, err
	}
	var scenarioDoc phase2ScenarioDocument
	if err := decodeYAMLStrict("phase2 scenarios", scenarioData, &scenarioDoc); err != nil {
		return phase2CatalogBundle{}, err
	}

	if err := validatePhase2CatalogDocument(catalogDoc); err != nil {
		return phase2CatalogBundle{}, err
	}
	if err := validatePhase2ScenarioDocument(scenarioDoc); err != nil {
		return phase2CatalogBundle{}, err
	}

	scenariosByID := make(map[string]Phase2Scenario, len(scenarioDoc.Scenarios))
	for _, scenario := range scenarioDoc.Scenarios {
		scenariosByID[scenario.ID] = cloneScenario(scenario)
	}

	requirements := make([]Requirement, 0, len(catalogDoc.Requirements))
	entries := make([]Phase2CatalogEntry, 0, len(catalogDoc.Requirements))
	byCode := make(map[RequirementCode]Phase2CatalogEntry, len(catalogDoc.Requirements))
	for _, record := range catalogDoc.Requirements {
		scenario, ok := scenariosByID[record.ScenarioID]
		if !ok {
			return phase2CatalogBundle{}, fmt.Errorf("phase2 catalog requirement %s references missing scenario %q", record.Code, record.ScenarioID)
		}
		if len(scenario.RequirementCodes) != 1 || scenario.RequirementCodes[0] != record.Code {
			return phase2CatalogBundle{}, fmt.Errorf("phase2 scenario %q requirement codes = %v, want [%s]", scenario.ID, scenario.RequirementCodes, record.Code)
		}
		requirement := Requirement{
			Code:        record.Code,
			Group:       record.Group,
			Description: record.Description,
		}
		entry := Phase2CatalogEntry{
			Requirement: requirement,
			Scenario:    scenario,
		}
		requirements = append(requirements, requirement)
		entries = append(entries, entry)
		byCode[record.Code] = entry
	}

	return phase2CatalogBundle{
		requirements: requirements,
		entries:      entries,
		scenarios:    cloneScenarios(scenarioDoc.Scenarios),
		byCode:       byCode,
		byScenarioID: scenariosByID,
	}, nil
}

func decodeJSONStrict(name string, data []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("decode %s: %w", name, err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("decode %s: trailing JSON content", name)
		}
		return fmt.Errorf("decode %s: trailing JSON content: %w", name, err)
	}
	return nil
}

func decodeYAMLStrict(name string, data []byte, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("decode %s: %w", name, err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("decode %s: trailing YAML content", name)
		}
		return fmt.Errorf("decode %s: trailing YAML content: %w", name, err)
	}
	return nil
}

func validatePhase2CatalogDocument(doc phase2CatalogDocument) error {
	if doc.Version != phase2CatalogVersion {
		return fmt.Errorf("phase2 catalog version = %d, want %d", doc.Version, phase2CatalogVersion)
	}
	if len(doc.Requirements) == 0 {
		return fmt.Errorf("phase2 catalog has no requirements")
	}
	seen := make(map[RequirementCode]struct{}, len(doc.Requirements))
	for i, requirement := range doc.Requirements {
		if requirement.Code == "" {
			return fmt.Errorf("phase2 catalog requirement %d has empty code", i)
		}
		if requirement.Group == "" {
			return fmt.Errorf("phase2 catalog requirement %s has empty group", requirement.Code)
		}
		if requirement.Description == "" {
			return fmt.Errorf("phase2 catalog requirement %s has empty description", requirement.Code)
		}
		if requirement.ScenarioID == "" {
			return fmt.Errorf("phase2 catalog requirement %s has empty scenario_id", requirement.Code)
		}
		if _, ok := seen[requirement.Code]; ok {
			return fmt.Errorf("phase2 catalog has duplicate requirement code %s", requirement.Code)
		}
		seen[requirement.Code] = struct{}{}
	}
	return nil
}

func validatePhase2ScenarioDocument(doc phase2ScenarioDocument) error {
	if doc.Version != phase2CatalogVersion {
		return fmt.Errorf("phase2 scenario version = %d, want %d", doc.Version, phase2CatalogVersion)
	}
	if len(doc.Scenarios) == 0 {
		return fmt.Errorf("phase2 scenarios has no records")
	}
	seen := make(map[string]struct{}, len(doc.Scenarios))
	for i, scenario := range doc.Scenarios {
		if scenario.ID == "" {
			return fmt.Errorf("phase2 scenario %d has empty id", i)
		}
		if !phase2KnownRunner(scenario.Runner) {
			return fmt.Errorf("phase2 scenario %s has unsupported runner %q", scenario.ID, scenario.Runner)
		}
		if scenario.Phase != phase2CatalogPhase {
			return fmt.Errorf("phase2 scenario %s phase = %q, want %q", scenario.ID, scenario.Phase, phase2CatalogPhase)
		}
		if scenario.Kind == "" {
			return fmt.Errorf("phase2 scenario %s has empty kind", scenario.ID)
		}
		if scenario.Description == "" {
			return fmt.Errorf("phase2 scenario %s has empty description", scenario.ID)
		}
		if !phase2KnownCertification(phase2ScenarioCertification(scenario)) {
			return fmt.Errorf("phase2 scenario %s has unsupported certification %q", scenario.ID, scenario.Certification)
		}
		if !scenario.Executable {
			return fmt.Errorf("phase2 scenario %s is not marked executable", scenario.ID)
		}
		if len(scenario.Profiles) == 0 {
			return fmt.Errorf("phase2 scenario %s has no profiles", scenario.ID)
		}
		if len(scenario.Profiles) != len(phase2CatalogProfiles) {
			return fmt.Errorf("phase2 scenario %s profiles = %v, want %v", scenario.ID, scenario.Profiles, phase2CatalogProfiles)
		}
		for i, profile := range phase2CatalogProfiles {
			if scenario.Profiles[i] != profile {
				return fmt.Errorf("phase2 scenario %s profiles = %v, want %v", scenario.ID, scenario.Profiles, phase2CatalogProfiles)
			}
		}
		if len(scenario.RequirementCodes) != 1 {
			return fmt.Errorf("phase2 scenario %s requirement codes = %v, want one code", scenario.ID, scenario.RequirementCodes)
		}
		if _, ok := seen[scenario.ID]; ok {
			return fmt.Errorf("phase2 scenarios has duplicate id %s", scenario.ID)
		}
		seen[scenario.ID] = struct{}{}
	}
	return nil
}

func phase2ScenarioCertification(scenario Phase2Scenario) string {
	if scenario.Certification == "" {
		return phase2CertificationCertifying
	}
	return scenario.Certification
}

func phase2KnownRunner(runner string) bool {
	switch runner {
	case phase2CatalogRunner, phase2RealTransportRunner:
		return true
	default:
		return false
	}
}

func phase2KnownCertification(certification string) bool {
	switch certification {
	case phase2CertificationCertifying, phase2CertificationProofOnly:
		return true
	default:
		return false
	}
}

func cloneRequirements(values []Requirement) []Requirement {
	if len(values) == 0 {
		return nil
	}
	out := make([]Requirement, len(values))
	copy(out, values)
	return out
}

func cloneEntries(values []Phase2CatalogEntry) []Phase2CatalogEntry {
	if len(values) == 0 {
		return nil
	}
	out := make([]Phase2CatalogEntry, len(values))
	for i, value := range values {
		out[i] = cloneEntry(value)
	}
	return out
}

func cloneEntry(value Phase2CatalogEntry) Phase2CatalogEntry {
	return Phase2CatalogEntry{
		Requirement: value.Requirement,
		Scenario:    cloneScenario(value.Scenario),
	}
}

func cloneScenarios(values []Phase2Scenario) []Phase2Scenario {
	if len(values) == 0 {
		return nil
	}
	out := make([]Phase2Scenario, len(values))
	for i, value := range values {
		out[i] = cloneScenario(value)
	}
	return out
}

func cloneScenario(value Phase2Scenario) Phase2Scenario {
	out := value
	if len(value.Profiles) > 0 {
		out.Profiles = append([]ProfileID(nil), value.Profiles...)
	}
	if len(value.RequirementCodes) > 0 {
		out.RequirementCodes = append([]RequirementCode(nil), value.RequirementCodes...)
	}
	return out
}
