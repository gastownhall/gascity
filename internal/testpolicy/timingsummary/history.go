package timingsummary

import (
	"cmp"
	"fmt"
	"sort"
	"strings"
)

const (
	// SnapshotSchema is the current machine-readable timing-history schema.
	SnapshotSchema = 1

	p75AuthoritativeSamples = 5
	p95AuthoritativeSamples = 20
)

// Snapshot is a deterministic timing-history projection of validated
// schema-v1 artifacts. It does not assert protected-branch provenance.
type Snapshot struct {
	Schema                 int       `json:"schema"`
	UniqueArtifactCount    int       `json:"unique_artifact_count"`
	DuplicateArtifactCount int       `json:"duplicate_artifact_count"`
	Profiles               []Profile `json:"profiles"`
}

// Profile groups histories measured on comparable jobs and runners.
type Profile struct {
	Job     string        `json:"job"`
	Variant string        `json:"variant"`
	Runner  RunnerProfile `json:"runner"`
	Units   []UnitHistory `json:"units"`
}

// RunnerProfile contains stable runner properties. Ephemeral runner names are
// intentionally excluded so equivalent observations remain comparable.
type RunnerProfile struct {
	Label    string `json:"label"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	CPUCount int    `json:"cpu_count"`
}

// UnitHistory contains the outcome counts and successful observations for one
// runnable top-level test within a comparable profile.
type UnitHistory struct {
	UnitID                            string                  `json:"unit_id"`
	Package                           string                  `json:"package"`
	Test                              string                  `json:"test"`
	Subtest                           string                  `json:"subtest"`
	Passes                            int                     `json:"passes"`
	Failures                          int                     `json:"failures"`
	Skips                             int                     `json:"skips"`
	DurationSecondsP50                *float64                `json:"duration_seconds_p50"`
	DurationSecondsP75                *float64                `json:"duration_seconds_p75"`
	DurationSecondsP95                *float64                `json:"duration_seconds_p95"`
	DurationSecondsPopulationVariance *float64                `json:"duration_seconds_population_variance"`
	P75Authoritative                  bool                    `json:"p75_authoritative"`
	P95Authoritative                  bool                    `json:"p95_authoritative"`
	LastSuccessSHA                    *string                 `json:"last_success_sha"`
	SuccessfulObservations            []SuccessfulObservation `json:"successful_observations"`
}

// SuccessfulObservation records one successful duration and the exact
// schema-v1 artifact that supplied it.
type SuccessfulObservation struct {
	ArtifactIdentity ArtifactIdentity `json:"artifact_identity"`
	TestedSHA        string           `json:"tested_sha"`
	DurationSeconds  float64          `json:"duration_seconds"`
}

// ArtifactIdentity is the schema-v1 artifact uniqueness key.
type ArtifactIdentity struct {
	Workflow   string `json:"workflow"`
	RunID      string `json:"run_id"`
	RunAttempt string `json:"run_attempt"`
	Job        string `json:"job"`
	ShardID    string `json:"shard_id"`
	Variant    string `json:"variant"`
}

type profileKey struct {
	Job      string
	Variant  string
	Label    string
	OS       string
	Arch     string
	CPUCount int
}

type unitIdentity struct {
	UnitID  string
	Package string
	Test    string
	Subtest string
}

type unitAccumulator struct {
	identity     unitIdentity
	failures     int
	skips        int
	observations []SuccessfulObservation
}

// BuildSnapshot loads and validates timing artifacts below roots and returns
// their canonical machine-readable history projection.
func BuildSnapshot(roots []string) (Snapshot, error) {
	artifacts, duplicateCount, err := loadArtifacts(roots)
	if err != nil {
		return Snapshot{}, err
	}
	return buildSnapshot(artifacts, duplicateCount)
}

func buildSnapshot(artifacts []artifact, duplicateCount int) (Snapshot, error) {
	byProfile := make(map[profileKey]map[string]*unitAccumulator)
	identities := make(map[string]unitIdentity)
	for _, item := range artifacts {
		profile := profileKey{
			Job: item.Job, Variant: item.Variant, Label: item.Runner.Label,
			OS: item.Runner.OS, Arch: item.Runner.Arch, CPUCount: item.Runner.CPUCount,
		}
		units := byProfile[profile]
		if units == nil {
			units = make(map[string]*unitAccumulator)
			byProfile[profile] = units
		}
		for _, unit := range item.Units {
			if unit.Kind != "test" || unit.Subtest != "" {
				continue
			}
			identity := unitIdentity{
				UnitID: unit.UnitID, Package: unit.Package, Test: unit.Test, Subtest: unit.Subtest,
			}
			if previous, ok := identities[unit.UnitID]; ok && previous != identity {
				return Snapshot{}, fmt.Errorf("conflicting identity for unit %q: %s != %s",
					unit.UnitID, formatUnitIdentity(previous), formatUnitIdentity(identity))
			}
			identities[unit.UnitID] = identity
			stats := units[unit.UnitID]
			if stats == nil {
				stats = &unitAccumulator{
					identity:     identity,
					observations: make([]SuccessfulObservation, 0),
				}
				units[unit.UnitID] = stats
			}

			switch unit.Outcome {
			case "pass":
				stats.observations = append(stats.observations, SuccessfulObservation{
					ArtifactIdentity: ArtifactIdentity{
						Workflow: item.Workflow, RunID: item.RunID, RunAttempt: item.RunAttempt,
						Job: item.Job, ShardID: item.ShardID, Variant: item.Variant,
					},
					TestedSHA:       item.CommitSHA,
					DurationSeconds: canonicalFloat(unit.DurationSeconds),
				})
			case "fail":
				stats.failures++
			case "skip":
				stats.skips++
			}
		}
	}

	profileKeys := make([]profileKey, 0, len(byProfile))
	for profile := range byProfile {
		profileKeys = append(profileKeys, profile)
	}
	sort.Slice(profileKeys, func(i, j int) bool {
		return compareProfileKey(profileKeys[i], profileKeys[j]) < 0
	})

	profiles := make([]Profile, 0, len(profileKeys))
	for _, profile := range profileKeys {
		accumulated := byProfile[profile]
		unitIDs := make([]string, 0, len(accumulated))
		for unitID := range accumulated {
			unitIDs = append(unitIDs, unitID)
		}
		sort.Strings(unitIDs)

		units := make([]UnitHistory, 0, len(unitIDs))
		for _, unitID := range unitIDs {
			stats := accumulated[unitID]
			observations := append([]SuccessfulObservation(nil), stats.observations...)
			sort.Slice(observations, func(i, j int) bool {
				return compareSuccessfulObservation(observations[i], observations[j]) < 0
			})
			if observations == nil {
				observations = make([]SuccessfulObservation, 0)
			}

			unit := UnitHistory{
				UnitID: stats.identity.UnitID, Package: stats.identity.Package,
				Test: stats.identity.Test, Subtest: stats.identity.Subtest,
				Passes: len(observations), Failures: stats.failures, Skips: stats.skips,
				P75Authoritative:       len(observations) >= p75AuthoritativeSamples,
				P95Authoritative:       len(observations) >= p95AuthoritativeSamples,
				SuccessfulObservations: observations,
			}
			if len(observations) > 0 {
				durations := make([]float64, len(observations))
				for index, observation := range observations {
					durations[index] = observation.DurationSeconds
				}
				sort.Float64s(durations)
				variance, err := populationVariance(durations)
				if err != nil {
					return Snapshot{}, fmt.Errorf("aggregate %q: %w", unitID, err)
				}
				unit.DurationSecondsP50 = floatPointer(nearestRank(durations, 0.50))
				unit.DurationSecondsP75 = floatPointer(nearestRank(durations, 0.75))
				unit.DurationSecondsP95 = floatPointer(nearestRank(durations, 0.95))
				unit.DurationSecondsPopulationVariance = floatPointer(variance)
				lastSuccessSHA := observations[len(observations)-1].TestedSHA
				unit.LastSuccessSHA = &lastSuccessSHA
			}
			units = append(units, unit)
		}

		profiles = append(profiles, Profile{
			Job: profile.Job, Variant: profile.Variant,
			Runner: RunnerProfile{
				Label: profile.Label, OS: profile.OS, Arch: profile.Arch, CPUCount: profile.CPUCount,
			},
			Units: units,
		})
	}

	return Snapshot{
		Schema: SnapshotSchema, UniqueArtifactCount: len(artifacts),
		DuplicateArtifactCount: duplicateCount, Profiles: profiles,
	}, nil
}

func compareProfileKey(left, right profileKey) int {
	leftFields := []string{left.Job, left.Variant, left.Label, left.OS, left.Arch}
	rightFields := []string{right.Job, right.Variant, right.Label, right.OS, right.Arch}
	for index := range leftFields {
		if result := strings.Compare(leftFields[index], rightFields[index]); result != 0 {
			return result
		}
	}
	return cmp.Compare(left.CPUCount, right.CPUCount)
}

func compareSuccessfulObservation(left, right SuccessfulObservation) int {
	leftFields := []string{
		left.ArtifactIdentity.Workflow, left.ArtifactIdentity.RunID, left.ArtifactIdentity.RunAttempt,
		left.ArtifactIdentity.Job, left.ArtifactIdentity.ShardID, left.ArtifactIdentity.Variant, left.TestedSHA,
	}
	rightFields := []string{
		right.ArtifactIdentity.Workflow, right.ArtifactIdentity.RunID, right.ArtifactIdentity.RunAttempt,
		right.ArtifactIdentity.Job, right.ArtifactIdentity.ShardID, right.ArtifactIdentity.Variant, right.TestedSHA,
	}
	for index := range leftFields {
		if result := strings.Compare(leftFields[index], rightFields[index]); result != 0 {
			return result
		}
	}
	if left.DurationSeconds < right.DurationSeconds {
		return -1
	}
	if left.DurationSeconds > right.DurationSeconds {
		return 1
	}
	return 0
}

func formatUnitIdentity(identity unitIdentity) string {
	return fmt.Sprintf("package=%q test=%q subtest=%q", identity.Package, identity.Test, identity.Subtest)
}

func canonicalFloat(value float64) float64 {
	if value == 0 {
		return 0
	}
	return value
}

func floatPointer(value float64) *float64 {
	value = canonicalFloat(value)
	return &value
}
