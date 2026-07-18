// Package timingplan deterministically assigns a caller-supplied runnable
// inventory to shards using conservative historical timing estimates.
package timingplan

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/testpolicy/timingsummary"
)

const (
	p75AuthoritativeSamples = 5
	p95AuthoritativeSamples = 20
	maxShards               = 256

	sourceStatic    = "static"
	sourceEmpirical = "empirical"
	sourceEstimated = "estimated"

	reasonHistoryMissing         = "history-missing"
	reasonP75SamplesInsufficient = "p75-insufficient-samples"
	reasonP95SamplesInsufficient = "p95-insufficient-samples"
	reasonHistoryAuthoritative   = "history-authoritative"

	hazardP95CapExceeded      = "p95-cap-exceeded"
	hazardShardP95CapExceeded = "shard-p95-cap-exceeded"

	historyProfileMatched = "matched"
	historyProfileMissing = "profile-missing"
)

// InventoryUnit identifies one currently runnable unit. Inventory, rather
// than timing history, is authoritative for what the planner assigns.
type InventoryUnit struct {
	UnitID string `json:"unit_id"`
}

// HistoryUnit contains successful-sample statistics for one runnable unit.
type HistoryUnit struct {
	UnitID            string  `json:"unit_id"`
	SuccessfulSamples int     `json:"successful_samples"`
	P50Seconds        float64 `json:"duration_seconds_p50"`
	P75Seconds        float64 `json:"duration_seconds_p75"`
	P95Seconds        float64 `json:"duration_seconds_p95"`
	Variance          float64 `json:"duration_seconds_population_variance"`
}

// StaticTiming contains conservative costs used when history is absent or has
// not reached the relevant authority threshold.
type StaticTiming struct {
	P50Seconds float64 `json:"duration_seconds_p50"`
	P75Seconds float64 `json:"duration_seconds_p75"`
	P95Seconds float64 `json:"duration_seconds_p95"`
	Variance   float64 `json:"duration_seconds_population_variance"`
}

// Input contains all data needed for a deterministic, side-effect-free plan.
type Input struct {
	Inventory     []InventoryUnit `json:"inventory"`
	History       []HistoryUnit   `json:"history"`
	Shards        int             `json:"shards"`
	Defaults      StaticTiming    `json:"defaults"`
	P95CapSeconds float64         `json:"p95_cap_seconds"`
}

// ProfileRunner identifies the stable runner properties of one comparable
// timing profile. Ephemeral runner names are intentionally excluded.
type ProfileRunner struct {
	Label    string `json:"label"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	CPUCount int    `json:"cpu_count"`
}

// ProfileSelector identifies one exact comparable timing profile.
type ProfileSelector struct {
	Job     string        `json:"job"`
	Variant string        `json:"variant"`
	Runner  ProfileRunner `json:"runner"`
}

// SnapshotPlanInput adapts one canonical timing snapshot and exact profile to
// the pure planner. Inventory remains authoritative for runnable membership.
type SnapshotPlanInput struct {
	Inventory     []InventoryUnit        `json:"inventory"`
	History       timingsummary.Snapshot `json:"history"`
	Profile       ProfileSelector        `json:"profile"`
	Shards        int                    `json:"shards"`
	Defaults      StaticTiming           `json:"defaults"`
	P95CapSeconds float64                `json:"p95_cap_seconds"`
}

// SnapshotPlanResult records whether the exact history profile was present and
// contains the deterministic plan. A missing profile uses static fallbacks.
type SnapshotPlanResult struct {
	HistoryProfileStatus string `json:"history_profile_status"`
	Plan                 Result `json:"plan"`
}

// Result is a canonical shard plan ordered by shard index.
type Result struct {
	Shards []Shard `json:"shards"`
}

// Shard contains assignments in planning order and their aggregate estimates.
type Shard struct {
	Index      int          `json:"index"`
	Units      []Assignment `json:"units"`
	P75Seconds float64      `json:"expected_seconds_p75"`
	P95Seconds float64      `json:"expected_seconds_p95"`
}

// Assignment records one unit's selected costs, provenance, and any planning
// hazards. A hazardous unit remains assigned.
type Assignment struct {
	UnitID     string   `json:"unit_id"`
	P50Seconds float64  `json:"expected_seconds_p50"`
	P75Seconds float64  `json:"expected_seconds_p75"`
	P95Seconds float64  `json:"expected_seconds_p95"`
	Variance   float64  `json:"population_variance"`
	P75Source  string   `json:"p75_source"`
	P95Source  string   `json:"p95_source"`
	Reason     string   `json:"reason"`
	Hazards    []string `json:"hazards"`
}

// Plan assigns every current inventory unit exactly once. History can predict
// cost but cannot add or remove runnable units.
func Plan(input Input) (Result, error) {
	if err := validateInput(input); err != nil {
		return Result{}, err
	}

	inventoryIDs := make(map[string]struct{}, len(input.Inventory))
	for _, unit := range input.Inventory {
		inventoryIDs[unit.UnitID] = struct{}{}
	}
	historyByID := make(map[string]HistoryUnit, min(len(input.History), len(input.Inventory)))
	for index, history := range input.History {
		if _, current := inventoryIDs[history.UnitID]; !current {
			continue
		}
		if _, duplicate := historyByID[history.UnitID]; duplicate {
			return Result{}, fmt.Errorf("history[%d]: duplicate unit_id %q", index, history.UnitID)
		}
		if err := validateHistory(index, history); err != nil {
			return Result{}, err
		}
		historyByID[history.UnitID] = history
	}

	assignments := make([]Assignment, 0, len(input.Inventory))
	for _, unit := range input.Inventory {
		assignment := Assignment{
			UnitID:     unit.UnitID,
			P50Seconds: input.Defaults.P50Seconds,
			P75Seconds: input.Defaults.P75Seconds,
			Variance:   input.Defaults.Variance,
			P75Source:  sourceStatic,
			P95Source:  sourceEstimated,
			Reason:     reasonHistoryMissing,
			Hazards:    make([]string, 0),
		}

		if history, ok := historyByID[unit.UnitID]; ok {
			assignment.Reason = reasonP75SamplesInsufficient
			if history.SuccessfulSamples >= p75AuthoritativeSamples {
				assignment.P50Seconds = history.P50Seconds
				assignment.P75Seconds = history.P75Seconds
				assignment.Variance = history.Variance
				assignment.P75Source = sourceEmpirical
				assignment.Reason = reasonP95SamplesInsufficient
			}
			if history.SuccessfulSamples >= p95AuthoritativeSamples {
				assignment.P95Seconds = history.P95Seconds
				assignment.P95Source = sourceEmpirical
				assignment.Reason = reasonHistoryAuthoritative
			}
		}
		if assignment.P95Source == sourceEstimated {
			estimatedP95 := 1.5 * assignment.P75Seconds
			if err := validateNonNegativeFinite(fmt.Sprintf("unit %q estimated p95", assignment.UnitID), estimatedP95); err != nil {
				return Result{}, err
			}
			assignment.P95Seconds = max(input.Defaults.P95Seconds, estimatedP95)
		}
		if assignment.P95Seconds > input.P95CapSeconds {
			assignment.Hazards = append(assignment.Hazards, hazardP95CapExceeded)
		}
		assignments = append(assignments, assignment)
	}

	sortAssignments(assignments)
	shards := make([]Shard, input.Shards)
	for index := range shards {
		shards[index] = Shard{Index: index, Units: make([]Assignment, 0)}
	}
	for _, assignment := range assignments {
		shardIndex, withinCap := shortestShard(shards, assignment.P95Seconds, input.P95CapSeconds)
		if !withinCap && !contains(assignment.Hazards, hazardP95CapExceeded) {
			assignment.Hazards = append(assignment.Hazards, hazardShardP95CapExceeded)
		}
		nextP75 := shards[shardIndex].P75Seconds + assignment.P75Seconds
		if err := validateNonNegativeFinite(fmt.Sprintf("shard %d aggregate p75 after unit %q", shardIndex, assignment.UnitID), nextP75); err != nil {
			return Result{}, err
		}
		nextP95 := shards[shardIndex].P95Seconds + assignment.P95Seconds
		if err := validateNonNegativeFinite(fmt.Sprintf("shard %d aggregate p95 after unit %q", shardIndex, assignment.UnitID), nextP95); err != nil {
			return Result{}, err
		}
		shards[shardIndex].Units = append(shards[shardIndex].Units, assignment)
		shards[shardIndex].P75Seconds = nextP75
		shards[shardIndex].P95Seconds = nextP95
	}
	return Result{Shards: shards}, nil
}

// PlanSnapshot validates a canonical timing snapshot, selects one exact
// comparable profile, and plans the caller-supplied inventory. It never merges
// profiles or treats timing history as runnable membership.
func PlanSnapshot(input SnapshotPlanInput) (SnapshotPlanResult, error) {
	if input.History.Schema != timingsummary.SnapshotSchema {
		return SnapshotPlanResult{}, fmt.Errorf("unsupported timing snapshot schema %d", input.History.Schema)
	}
	if input.History.UniqueArtifactCount < 0 {
		return SnapshotPlanResult{}, fmt.Errorf("history.unique_artifact_count must not be negative")
	}
	if input.History.DuplicateArtifactCount < 0 {
		return SnapshotPlanResult{}, fmt.Errorf("history.duplicate_artifact_count must not be negative")
	}
	if err := validateProfileSelector("profile", input.Profile); err != nil {
		return SnapshotPlanResult{}, err
	}

	profileKeys := make(map[ProfileSelector]struct{}, len(input.History.Profiles))
	identities := make(map[string]snapshotUnitIdentity)
	var selectedHistory []HistoryUnit
	matched := false
	for profileIndex, profile := range input.History.Profiles {
		selector := profileSelectorFromSnapshot(profile)
		if err := validateProfileSelector(fmt.Sprintf("history.profiles[%d]", profileIndex), selector); err != nil {
			return SnapshotPlanResult{}, err
		}
		if _, duplicate := profileKeys[selector]; duplicate {
			return SnapshotPlanResult{}, fmt.Errorf("history.profiles[%d]: duplicate timing profile", profileIndex)
		}
		profileKeys[selector] = struct{}{}
		if profile.Units == nil {
			return SnapshotPlanResult{}, fmt.Errorf("history.profiles[%d].units must be an array", profileIndex)
		}

		unitIDs := make(map[string]struct{}, len(profile.Units))
		converted := make([]HistoryUnit, 0, len(profile.Units))
		for unitIndex, unit := range profile.Units {
			path := fmt.Sprintf("history.profiles[%d].units[%d]", profileIndex, unitIndex)
			history, identity, err := convertSnapshotUnit(path, unitIndex, selector, unit)
			if err != nil {
				return SnapshotPlanResult{}, err
			}
			if _, duplicate := unitIDs[unit.UnitID]; duplicate {
				return SnapshotPlanResult{}, fmt.Errorf("%s: duplicate unit_id %q", path, unit.UnitID)
			}
			unitIDs[unit.UnitID] = struct{}{}
			if previous, ok := identities[unit.UnitID]; ok && previous != identity {
				return SnapshotPlanResult{}, fmt.Errorf("%s: conflicting identity for unit_id %q", path, unit.UnitID)
			}
			identities[unit.UnitID] = identity
			converted = append(converted, history)
		}
		if selector == input.Profile {
			selectedHistory = converted
			matched = true
		}
	}

	status := historyProfileMissing
	if matched {
		status = historyProfileMatched
	}
	plan, err := Plan(Input{
		Inventory:     input.Inventory,
		History:       selectedHistory,
		Shards:        input.Shards,
		Defaults:      input.Defaults,
		P95CapSeconds: input.P95CapSeconds,
	})
	if err != nil {
		return SnapshotPlanResult{}, err
	}
	return SnapshotPlanResult{HistoryProfileStatus: status, Plan: plan}, nil
}

type snapshotUnitIdentity struct {
	Package string
	Test    string
	Subtest string
}

func profileSelectorFromSnapshot(profile timingsummary.Profile) ProfileSelector {
	return ProfileSelector{
		Job: profile.Job, Variant: profile.Variant,
		Runner: ProfileRunner{
			Label: profile.Runner.Label, OS: profile.Runner.OS,
			Arch: profile.Runner.Arch, CPUCount: profile.Runner.CPUCount,
		},
	}
}

func validateProfileSelector(name string, selector ProfileSelector) error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "job", value: selector.Job},
		{name: "variant", value: selector.Variant},
		{name: "runner.label", value: selector.Runner.Label},
		{name: "runner.os", value: selector.Runner.OS},
		{name: "runner.arch", value: selector.Runner.Arch},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%s.%s is required", name, field.name)
		}
	}
	if selector.Runner.CPUCount < 0 {
		return fmt.Errorf("%s.runner.cpu_count must not be negative", name)
	}
	return nil
}

func convertSnapshotUnit(path string, unitIndex int, selector ProfileSelector, unit timingsummary.UnitHistory) (HistoryUnit, snapshotUnitIdentity, error) {
	identity := snapshotUnitIdentity{Package: unit.Package, Test: unit.Test, Subtest: unit.Subtest}
	if strings.TrimSpace(unit.UnitID) == "" {
		return HistoryUnit{}, identity, fmt.Errorf("%s: unit_id is required", path)
	}
	if strings.TrimSpace(unit.Package) == "" {
		return HistoryUnit{}, identity, fmt.Errorf("%s: package is required", path)
	}
	if strings.TrimSpace(unit.Test) == "" {
		return HistoryUnit{}, identity, fmt.Errorf("%s: test is required", path)
	}
	if unit.Subtest != "" {
		return HistoryUnit{}, identity, fmt.Errorf("%s: subtest must be empty for a top-level planner unit", path)
	}
	for _, count := range []struct {
		name  string
		value int
	}{
		{name: "passes", value: unit.Passes},
		{name: "failures", value: unit.Failures},
		{name: "skips", value: unit.Skips},
	} {
		if count.value < 0 {
			return HistoryUnit{}, identity, fmt.Errorf("%s.%s must not be negative", path, count.name)
		}
	}
	if unit.SuccessfulObservations == nil {
		return HistoryUnit{}, identity, fmt.Errorf("%s.successful_observations must be an array", path)
	}
	if len(unit.SuccessfulObservations) != unit.Passes {
		return HistoryUnit{}, identity, fmt.Errorf("%s: successful observations = %d, want passes %d", path, len(unit.SuccessfulObservations), unit.Passes)
	}
	if unit.P75Authoritative != (unit.Passes >= p75AuthoritativeSamples) {
		return HistoryUnit{}, identity, fmt.Errorf("%s.p75_authoritative contradicts %d successful samples", path, unit.Passes)
	}
	if unit.P95Authoritative != (unit.Passes >= p95AuthoritativeSamples) {
		return HistoryUnit{}, identity, fmt.Errorf("%s.p95_authoritative contradicts %d successful samples", path, unit.Passes)
	}
	for observationIndex, observation := range unit.SuccessfulObservations {
		observationPath := fmt.Sprintf("%s.successful_observations[%d]", path, observationIndex)
		if err := validateNonNegativeFinite(observationPath+".duration_seconds", observation.DurationSeconds); err != nil {
			return HistoryUnit{}, identity, err
		}
		for _, field := range []struct {
			name  string
			value string
		}{
			{name: "workflow", value: observation.ArtifactIdentity.Workflow},
			{name: "run_id", value: observation.ArtifactIdentity.RunID},
			{name: "run_attempt", value: observation.ArtifactIdentity.RunAttempt},
			{name: "job", value: observation.ArtifactIdentity.Job},
			{name: "shard_id", value: observation.ArtifactIdentity.ShardID},
			{name: "variant", value: observation.ArtifactIdentity.Variant},
		} {
			if strings.TrimSpace(field.value) == "" {
				return HistoryUnit{}, identity, fmt.Errorf("%s.artifact_identity.%s is required", observationPath, field.name)
			}
		}
		if observation.ArtifactIdentity.Job != selector.Job || observation.ArtifactIdentity.Variant != selector.Variant {
			return HistoryUnit{}, identity, fmt.Errorf("%s: job/variant do not match enclosing timing profile", observationPath)
		}
		if strings.TrimSpace(observation.TestedSHA) == "" {
			return HistoryUnit{}, identity, fmt.Errorf("%s.tested_sha is required", observationPath)
		}
	}

	statistics := []*float64{
		unit.DurationSecondsP50,
		unit.DurationSecondsP75,
		unit.DurationSecondsP95,
		unit.DurationSecondsPopulationVariance,
	}
	if unit.Passes == 0 {
		for _, statistic := range statistics {
			if statistic != nil {
				return HistoryUnit{}, identity, fmt.Errorf("%s: zero-pass unit requires null timing statistics", path)
			}
		}
		if unit.LastSuccessSHA != nil {
			return HistoryUnit{}, identity, fmt.Errorf("%s: zero-pass unit requires null last_success_sha", path)
		}
		return HistoryUnit{UnitID: unit.UnitID}, identity, nil
	}
	for _, statistic := range statistics {
		if statistic == nil {
			return HistoryUnit{}, identity, fmt.Errorf("%s: successful unit requires non-null timing statistics", path)
		}
	}
	if unit.LastSuccessSHA == nil || strings.TrimSpace(*unit.LastSuccessSHA) == "" {
		return HistoryUnit{}, identity, fmt.Errorf("%s: successful unit requires non-empty last_success_sha", path)
	}
	if *unit.LastSuccessSHA != unit.SuccessfulObservations[len(unit.SuccessfulObservations)-1].TestedSHA {
		return HistoryUnit{}, identity, fmt.Errorf("%s: last_success_sha does not match the final canonical successful observation", path)
	}

	history := HistoryUnit{
		UnitID: unit.UnitID, SuccessfulSamples: unit.Passes,
		P50Seconds: *unit.DurationSecondsP50,
		P75Seconds: *unit.DurationSecondsP75,
		P95Seconds: *unit.DurationSecondsP95,
		Variance:   *unit.DurationSecondsPopulationVariance,
	}
	if err := validateHistory(unitIndex, history); err != nil {
		return HistoryUnit{}, identity, fmt.Errorf("%s: %w", path, err)
	}
	return history, identity, nil
}

func validateInput(input Input) error {
	if input.Shards <= 0 {
		return fmt.Errorf("shards must be a positive integer")
	}
	if input.Shards > maxShards {
		return fmt.Errorf("shards must not exceed %d", maxShards)
	}
	if err := validateNonNegativeFinite("p95_cap_seconds", input.P95CapSeconds); err != nil {
		return err
	}
	if input.P95CapSeconds == 0 {
		return fmt.Errorf("p95_cap_seconds must be positive")
	}
	for _, value := range []struct {
		name  string
		value float64
	}{
		{name: "defaults.duration_seconds_p50", value: input.Defaults.P50Seconds},
		{name: "defaults.duration_seconds_p75", value: input.Defaults.P75Seconds},
		{name: "defaults.duration_seconds_p95", value: input.Defaults.P95Seconds},
		{name: "defaults.duration_seconds_population_variance", value: input.Defaults.Variance},
	} {
		if err := validateNonNegativeFinite(value.name, value.value); err != nil {
			return err
		}
	}
	if input.Defaults.P75Seconds == 0 {
		return fmt.Errorf("defaults.duration_seconds_p75 must be positive")
	}
	if input.Defaults.P95Seconds == 0 {
		return fmt.Errorf("defaults.duration_seconds_p95 must be positive")
	}
	if err := validatePercentileOrder("defaults", input.Defaults.P50Seconds, input.Defaults.P75Seconds, input.Defaults.P95Seconds); err != nil {
		return err
	}

	seen := make(map[string]struct{}, len(input.Inventory))
	for index, unit := range input.Inventory {
		if strings.TrimSpace(unit.UnitID) == "" {
			return fmt.Errorf("inventory[%d]: unit_id is required", index)
		}
		if _, duplicate := seen[unit.UnitID]; duplicate {
			return fmt.Errorf("inventory[%d]: duplicate unit_id %q", index, unit.UnitID)
		}
		seen[unit.UnitID] = struct{}{}
	}
	return nil
}

func validateHistory(index int, history HistoryUnit) error {
	if strings.TrimSpace(history.UnitID) == "" {
		return fmt.Errorf("history[%d]: unit_id is required", index)
	}
	if history.SuccessfulSamples < 0 {
		return fmt.Errorf("history[%d]: successful_samples must not be negative", index)
	}
	for _, value := range []struct {
		name  string
		value float64
	}{
		{name: "duration_seconds_p50", value: history.P50Seconds},
		{name: "duration_seconds_p75", value: history.P75Seconds},
		{name: "duration_seconds_p95", value: history.P95Seconds},
		{name: "duration_seconds_population_variance", value: history.Variance},
	} {
		if err := validateNonNegativeFinite(fmt.Sprintf("history[%d].%s", index, value.name), value.value); err != nil {
			return err
		}
	}
	if err := validatePercentileOrder(fmt.Sprintf("history[%d]", index), history.P50Seconds, history.P75Seconds, history.P95Seconds); err != nil {
		return err
	}
	return nil
}

func validatePercentileOrder(name string, p50, p75, p95 float64) error {
	if p50 > p75 {
		return fmt.Errorf("%s: duration_seconds_p50 must not exceed duration_seconds_p75", name)
	}
	if p75 > p95 {
		return fmt.Errorf("%s: duration_seconds_p75 must not exceed duration_seconds_p95", name)
	}
	return nil
}

func validateNonNegativeFinite(name string, value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return fmt.Errorf("%s must be finite", name)
	}
	if value < 0 {
		return fmt.Errorf("%s must not be negative", name)
	}
	return nil
}

func sortAssignments(assignments []Assignment) {
	sort.Slice(assignments, func(left, right int) bool {
		a, b := assignments[left], assignments[right]
		for _, values := range [][2]float64{
			{a.P75Seconds, b.P75Seconds},
			{a.P95Seconds, b.P95Seconds},
			{a.Variance, b.Variance},
			{a.P50Seconds, b.P50Seconds},
		} {
			if values[0] != values[1] {
				return values[0] > values[1]
			}
		}
		return a.UnitID < b.UnitID
	})
}

func shortestShard(shards []Shard, unitP95, p95Cap float64) (int, bool) {
	bestEligible := -1
	bestAny := 0
	for index := range shards {
		if shards[index].P75Seconds < shards[bestAny].P75Seconds {
			bestAny = index
		}
		if unitP95 > p95Cap-shards[index].P95Seconds {
			continue
		}
		if bestEligible < 0 || shards[index].P75Seconds < shards[bestEligible].P75Seconds {
			bestEligible = index
		}
	}
	if bestEligible >= 0 {
		return bestEligible, true
	}
	return bestAny, false
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
