// Package timingsummary aggregates schema-v1 Go test timing artifacts.
package timingsummary

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
)

const summaryLimit = 10

type artifact struct {
	Schema     int            `json:"schema"`
	ShardID    string         `json:"shard_id"`
	Variant    string         `json:"variant"`
	CommitSHA  string         `json:"commit_sha"`
	Workflow   string         `json:"workflow"`
	RunID      string         `json:"run_id"`
	RunAttempt string         `json:"run_attempt"`
	Job        string         `json:"job"`
	Runner     artifactRunner `json:"runner"`
	Units      []artifactUnit `json:"units"`
}

type artifactRunner struct {
	Label    string `json:"label"`
	Name     string `json:"name"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	CPUCount int    `json:"cpu_count"`
}

type artifactUnit struct {
	UnitID          string  `json:"unit_id"`
	Kind            string  `json:"kind"`
	Package         string  `json:"package"`
	Test            string  `json:"test"`
	Subtest         string  `json:"subtest"`
	Outcome         string  `json:"outcome"`
	DurationSeconds float64 `json:"duration_seconds"`
}

type artifactIdentity struct {
	Workflow   string
	RunID      string
	RunAttempt string
	Job        string
	ShardID    string
	Variant    string
}

type profileKey struct {
	Job      string
	Variant  string
	Label    string
	OS       string
	Arch     string
	CPUCount int
}

type unitAccumulator struct {
	passes   []float64
	failures int
	skips    int
}

type unitSummary struct {
	unitID   string
	passes   int
	failures int
	skips    int
	p50      float64
	p75      float64
	p95      float64
	variance float64
}

type profileSummary struct {
	profile  profileKey
	units    []unitSummary
	passes   int
	failures int
	skips    int
}

// Run loads timing artifacts from args, writes a deterministic Markdown
// summary to stdout, and returns a process-style exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: test-timing-summary <artifact-root> [<artifact-root> ...]")
		return 2
	}

	artifacts, duplicateCount, err := loadArtifacts(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "timing summary: %v\n", err)
		return 1
	}

	profiles, err := aggregate(artifacts)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "timing summary: %v\n", err)
		return 1
	}
	output := renderMarkdown(profiles, len(artifacts), duplicateCount)
	if _, err := io.WriteString(stdout, output); err != nil {
		_, _ = fmt.Fprintf(stderr, "timing summary: write output: %v\n", err)
		return 1
	}
	return 0
}

func loadArtifacts(roots []string) ([]artifact, int, error) {
	paths, err := artifactPaths(roots)
	if err != nil {
		return nil, 0, err
	}
	if len(paths) == 0 {
		return nil, 0, errors.New("no JSON timing artifacts found")
	}

	seen := make(map[artifactIdentity]artifact, len(paths))
	seenPath := make(map[artifactIdentity]string, len(paths))
	artifacts := make([]artifact, 0, len(paths))
	duplicateCount := 0
	for _, path := range paths {
		item, err := decodeArtifact(path)
		if err != nil {
			return nil, 0, err
		}
		identity := artifactIdentity{
			Workflow: item.Workflow, RunID: item.RunID, RunAttempt: item.RunAttempt,
			Job: item.Job, ShardID: item.ShardID, Variant: item.Variant,
		}
		if previous, ok := seen[identity]; ok {
			if !reflect.DeepEqual(previous, item) {
				return nil, 0, fmt.Errorf("conflicting duplicate artifact %s and %s for %s", seenPath[identity], path, formatIdentity(identity))
			}
			duplicateCount++
			continue
		}
		seen[identity] = item
		seenPath[identity] = path
		artifacts = append(artifacts, item)
	}
	return artifacts, duplicateCount, nil
}

func artifactPaths(roots []string) ([]string, error) {
	var paths []string
	for _, root := range roots {
		info, err := os.Stat(root)
		if err != nil {
			return nil, fmt.Errorf("inspect artifact root %q: %w", root, err)
		}
		if !info.IsDir() {
			paths = append(paths, root)
			continue
		}
		err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.Type().IsRegular() && strings.EqualFold(filepath.Ext(path), ".json") {
				paths = append(paths, path)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walk artifact root %q: %w", root, err)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func decodeArtifact(path string) (artifact, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return artifact{}, fmt.Errorf("read %s: %w", path, err)
	}
	var envelope struct {
		Schema *int `json:"schema"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return artifact{}, fmt.Errorf("decode schema in %s: %w", path, err)
	}
	if envelope.Schema == nil {
		return artifact{}, fmt.Errorf("validate %s: schema is required", path)
	}
	if *envelope.Schema != 1 {
		return artifact{}, fmt.Errorf("validate %s: unsupported schema %d", path, *envelope.Schema)
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var item artifact
	if err := decoder.Decode(&item); err != nil {
		return artifact{}, fmt.Errorf("decode schema-v1 artifact %s: %w", path, err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return artifact{}, fmt.Errorf("decode schema-v1 artifact %s: %w", path, err)
	}
	if err := validateArtifact(item); err != nil {
		return artifact{}, fmt.Errorf("validate %s: %w", path, err)
	}
	return item, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("multiple JSON values")
	}
	return err
}

func validateArtifact(item artifact) error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "shard_id", value: item.ShardID},
		{name: "variant", value: item.Variant},
		{name: "commit_sha", value: item.CommitSHA},
		{name: "workflow", value: item.Workflow},
		{name: "run_id", value: item.RunID},
		{name: "run_attempt", value: item.RunAttempt},
		{name: "job", value: item.Job},
		{name: "runner label", value: item.Runner.Label},
		{name: "runner os", value: item.Runner.OS},
		{name: "runner arch", value: item.Runner.Arch},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%s is required", field.name)
		}
	}
	if item.Runner.CPUCount < 0 {
		return errors.New("runner cpu_count must be non-negative")
	}
	if len(item.Units) == 0 {
		return errors.New("units must not be empty")
	}
	for index, unit := range item.Units {
		if err := validateUnit(unit); err != nil {
			return fmt.Errorf("units[%d]: %w", index, err)
		}
	}
	return nil
}

func validateUnit(unit artifactUnit) error {
	if strings.TrimSpace(unit.UnitID) == "" {
		return errors.New("unit_id is required")
	}
	if strings.TrimSpace(unit.Package) == "" {
		return errors.New("package is required")
	}
	if math.IsNaN(unit.DurationSeconds) || math.IsInf(unit.DurationSeconds, 0) || unit.DurationSeconds < 0 {
		return errors.New("duration_seconds must be non-negative and finite")
	}
	switch unit.Outcome {
	case "pass", "fail", "skip":
	default:
		return fmt.Errorf("unsupported outcome %q", unit.Outcome)
	}
	switch unit.Kind {
	case "package":
		if unit.Test != "" || unit.Subtest != "" {
			return errors.New("package unit must not name a test or subtest")
		}
	case "test":
		if strings.TrimSpace(unit.Test) == "" {
			return errors.New("test unit must name a test")
		}
	default:
		return fmt.Errorf("unsupported kind %q", unit.Kind)
	}
	return nil
}

func formatIdentity(identity artifactIdentity) string {
	return fmt.Sprintf("workflow=%q run=%q attempt=%q job=%q shard=%q variant=%q",
		identity.Workflow, identity.RunID, identity.RunAttempt, identity.Job, identity.ShardID, identity.Variant)
}

func aggregate(artifacts []artifact) ([]profileSummary, error) {
	byProfile := make(map[profileKey]map[string]*unitAccumulator)
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
			stats := units[unit.UnitID]
			if stats == nil {
				stats = &unitAccumulator{}
				units[unit.UnitID] = stats
			}
			switch unit.Outcome {
			case "pass":
				stats.passes = append(stats.passes, unit.DurationSeconds)
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
	sort.Slice(profileKeys, func(i, j int) bool { return compareProfile(profileKeys[i], profileKeys[j]) < 0 })

	profiles := make([]profileSummary, 0, len(profileKeys))
	for _, profile := range profileKeys {
		accumulated := byProfile[profile]
		units := make([]unitSummary, 0, len(accumulated))
		var totalPasses, totalFailures, totalSkips int
		unitIDs := make([]string, 0, len(accumulated))
		for unitID := range accumulated {
			unitIDs = append(unitIDs, unitID)
		}
		sort.Strings(unitIDs)
		for _, unitID := range unitIDs {
			stats := accumulated[unitID]
			totalPasses += len(stats.passes)
			totalFailures += stats.failures
			totalSkips += stats.skips
			if len(stats.passes) == 0 {
				units = append(units, unitSummary{unitID: unitID, failures: stats.failures, skips: stats.skips})
				continue
			}
			passes := slices.Clone(stats.passes)
			sort.Float64s(passes)
			variance, err := populationVariance(passes)
			if err != nil {
				return nil, fmt.Errorf("aggregate %q: %w", unitID, err)
			}
			units = append(units, unitSummary{
				unitID: unitID, passes: len(passes), failures: stats.failures, skips: stats.skips,
				p50: nearestRank(passes, 0.50), p75: nearestRank(passes, 0.75),
				p95: nearestRank(passes, 0.95), variance: variance,
			})
		}
		profiles = append(profiles, profileSummary{
			profile: profile, units: units, passes: totalPasses, failures: totalFailures, skips: totalSkips,
		})
	}
	return profiles, nil
}

func nearestRank(sortedSamples []float64, percentile float64) float64 {
	index := int(math.Ceil(percentile*float64(len(sortedSamples)))) - 1
	if index < 0 {
		index = 0
	}
	return sortedSamples[index]
}

func populationVariance(samples []float64) (float64, error) {
	var scale float64
	for _, sample := range samples {
		scale = max(scale, math.Abs(sample))
	}
	if scale == 0 {
		return 0, nil
	}

	// Normalize before accumulating so neither a prefix's unnormalized M2 nor
	// scale squared can overflow when the final population variance is finite.
	var mean, normalizedM2 float64
	for index, sample := range samples {
		count := float64(index + 1)
		normalized := sample / scale
		difference := normalized - mean
		mean += difference / count
		adjustedDifference := normalized - mean
		contribution := difference * adjustedDifference
		normalizedM2 += contribution
		if !finite(mean) || !finite(normalizedM2) {
			return 0, errors.New("variance is not representable as float64")
		}
	}
	normalizedVariance := normalizedM2 / float64(len(samples))
	variance := (normalizedVariance * scale) * scale
	if !finite(variance) || variance < 0 {
		return 0, errors.New("variance is not representable as float64")
	}
	return variance, nil
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func compareProfile(left, right profileKey) int {
	leftFields := []string{left.Job, left.Variant, left.Label, left.OS, left.Arch}
	rightFields := []string{right.Job, right.Variant, right.Label, right.OS, right.Arch}
	for index := range leftFields {
		if result := strings.Compare(leftFields[index], rightFields[index]); result != 0 {
			return result
		}
	}
	return left.CPUCount - right.CPUCount
}

func renderMarkdown(profiles []profileSummary, artifactCount, duplicateCount int) string {
	var output strings.Builder
	output.WriteString("# Go test timing summary\n\n")
	fmt.Fprintf(&output, "Analyzed %d unique schema-v1 %s; %d duplicate %s ignored.\n\n",
		artifactCount, plural(artifactCount, "artifact", "artifacts"), duplicateCount,
		plural(duplicateCount, "download", "downloads"))
	output.WriteString("Rankings use successful durations from top-level tests only. Package totals and nested subtests are excluded. Profiles are never mixed.\n\n")

	for index, summary := range profiles {
		fmt.Fprintf(&output, "## Profile %d\n\n", index+1)
		output.WriteString("| Job | Variant | Runner label | OS | Arch | CPUs |\n")
		output.WriteString("| --- | --- | --- | --- | --- | ---: |\n")
		fmt.Fprintf(&output, "| %s | %s | %s | %s | %s | %d |\n\n",
			codeCell(summary.profile.Job), codeCell(summary.profile.Variant), codeCell(summary.profile.Label),
			codeCell(summary.profile.OS), codeCell(summary.profile.Arch), summary.profile.CPUCount)
		fmt.Fprintf(&output, "Top-level outcomes: %d pass, %d fail, %d skip.\n\n",
			summary.passes, summary.failures, summary.skips)

		writeSlowestTable(&output, summary.units)
		writeVarianceTable(&output, summary.units)
	}
	return output.String()
}

func writeSlowestTable(output *strings.Builder, units []unitSummary) {
	output.WriteString("### Ten slowest top-level tests\n\n")
	output.WriteString("| Runnable unit | Pass | Fail | Skip | p50 | p75 | p95 |\n")
	output.WriteString("| --- | ---: | ---: | ---: | ---: | ---: | ---: |\n")
	ordered := make([]unitSummary, 0, len(units))
	for _, unit := range units {
		if unit.passes > 0 {
			ordered = append(ordered, unit)
		}
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].p95 != ordered[j].p95 {
			return ordered[i].p95 > ordered[j].p95
		}
		return ordered[i].unitID < ordered[j].unitID
	})
	if len(ordered) == 0 {
		output.WriteString("| _No top-level tests with successful samples_ | 0 | 0 | 0 | — | — | — |\n\n")
		return
	}
	for _, unit := range ordered[:min(summaryLimit, len(ordered))] {
		fmt.Fprintf(output, "| `%s` | %d | %d | %d | %.3fs | %.3fs | %.3fs |\n",
			escapeCode(unit.unitID), unit.passes, unit.failures, unit.skips, unit.p50, unit.p75, unit.p95)
	}
	output.WriteByte('\n')
}

func writeVarianceTable(output *strings.Builder, units []unitSummary) {
	output.WriteString("### Ten highest-variance top-level tests\n\n")
	output.WriteString("| Runnable unit | Pass | Fail | Skip | Population variance (s²) | p95 |\n")
	output.WriteString("| --- | ---: | ---: | ---: | ---: | ---: |\n")
	eligible := make([]unitSummary, 0, len(units))
	for _, unit := range units {
		if unit.passes >= 2 {
			eligible = append(eligible, unit)
		}
	}
	sort.Slice(eligible, func(i, j int) bool {
		if eligible[i].variance != eligible[j].variance {
			return eligible[i].variance > eligible[j].variance
		}
		return eligible[i].unitID < eligible[j].unitID
	})
	if len(eligible) == 0 {
		output.WriteString("| _At least two successful samples are required_ | 0 | 0 | 0 | — | — |\n\n")
		return
	}
	for _, unit := range eligible[:min(summaryLimit, len(eligible))] {
		fmt.Fprintf(output, "| `%s` | %d | %d | %d | %.6f | %.3fs |\n",
			escapeCode(unit.unitID), unit.passes, unit.failures, unit.skips, unit.variance, unit.p95)
	}
	output.WriteByte('\n')
}

func plural(count int, singular, plural string) string {
	if count == 1 {
		return singular
	}
	return plural
}

func escapeCell(value string) string {
	value = strings.NewReplacer("\r", " ", "\n", " ").Replace(value)
	return strings.ReplaceAll(html.EscapeString(value), "|", "&#124;")
}

func escapeCode(value string) string {
	return strings.ReplaceAll(escapeCell(value), "`", "&#96;")
}

func codeCell(value string) string {
	return "`" + escapeCode(value) + "`"
}
