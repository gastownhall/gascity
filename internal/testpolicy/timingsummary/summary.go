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
	"sort"
	"strconv"
	"strings"
)

const (
	timingArtifactSchema = 1
	summaryLimit         = 10
)

type outputFormat uint8

const (
	formatMarkdown outputFormat = iota
	formatJSON
)

type runOptions struct {
	format          outputFormat
	roots           []string
	historyPath     string
	runEnvelopePath string
	retainRuns      int
	formatSet       bool
	historySet      bool
	envelopeSet     bool
	retentionSet    bool
}

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

// Run loads timing artifacts from args, writes a deterministic Markdown or
// JSON summary to stdout, and returns a process-style exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	options, err := parseRunArgs(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "timing summary: %v\n", err)
		return 2
	}
	if len(options.roots) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: test-timing-summary [options] <artifact-root> [<artifact-root> ...]")
		return 2
	}

	var snapshot Snapshot
	if options.historySet {
		envelope, decodeErr := decodeRunEnvelope(options.runEnvelopePath)
		if decodeErr != nil {
			err = decodeErr
		} else {
			snapshot, err = UpdateHistory(options.historyPath, envelope, options.retainRuns, options.roots)
		}
	} else {
		snapshot, err = BuildSnapshot(options.roots)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "timing summary: %v\n", err)
		return 1
	}

	if options.format == formatJSON {
		if err := json.NewEncoder(stdout).Encode(snapshot); err != nil {
			_, _ = fmt.Fprintf(stderr, "timing summary: write output: %v\n", err)
			return 1
		}
		return 0
	}
	if _, err := io.WriteString(stdout, renderMarkdown(snapshot)); err != nil {
		_, _ = fmt.Fprintf(stderr, "timing summary: write output: %v\n", err)
		return 1
	}
	return 0
}

func parseRunArgs(args []string) (runOptions, error) {
	options := runOptions{format: formatMarkdown, roots: make([]string, 0, len(args))}
	for index := 0; index < len(args); index++ {
		argument := args[index]
		name, value, recognized, consumedNext, err := parseRunFlag(args, index)
		if err != nil {
			return runOptions{}, err
		}
		if !recognized {
			options.roots = append(options.roots, argument)
			continue
		}
		if consumedNext {
			index++
		}
		switch name {
		case "--format":
			if options.formatSet {
				return runOptions{}, errors.New("--format may be specified only once")
			}
			options.formatSet = true
			switch value {
			case "markdown":
				options.format = formatMarkdown
			case "json":
				options.format = formatJSON
			default:
				return runOptions{}, fmt.Errorf("unsupported format %q", value)
			}
		case "--update-history":
			if options.historySet {
				return runOptions{}, errors.New("--update-history may be specified only once")
			}
			options.historySet = true
			options.historyPath = value
		case "--run-envelope":
			if options.envelopeSet {
				return runOptions{}, errors.New("--run-envelope may be specified only once")
			}
			options.envelopeSet = true
			options.runEnvelopePath = value
		case "--retain-runs":
			if options.retentionSet {
				return runOptions{}, errors.New("--retain-runs may be specified only once")
			}
			options.retentionSet = true
			retainRuns, parseErr := strconv.Atoi(value)
			if parseErr != nil || retainRuns <= 0 {
				return runOptions{}, errors.New("--retain-runs must be a positive integer")
			}
			options.retainRuns = retainRuns
		}
	}
	mutationFlags := 0
	for _, set := range []bool{options.historySet, options.envelopeSet, options.retentionSet} {
		if set {
			mutationFlags++
		}
	}
	if mutationFlags != 0 && mutationFlags != 3 {
		return runOptions{}, errors.New("--update-history, --run-envelope, and --retain-runs must be specified together")
	}
	return options, nil
}

func parseRunFlag(args []string, index int) (name, value string, recognized, consumedNext bool, err error) {
	argument := args[index]
	for _, candidate := range []string{"--format", "--update-history", "--run-envelope", "--retain-runs"} {
		if argument == candidate {
			if index+1 >= len(args) || args[index+1] == "" || isRunFlag(args[index+1]) {
				return "", "", true, false, fmt.Errorf("%s requires a value", candidate)
			}
			return candidate, args[index+1], true, true, nil
		}
		prefix := candidate + "="
		if strings.HasPrefix(argument, prefix) {
			value := strings.TrimPrefix(argument, prefix)
			if value == "" {
				return "", "", true, false, fmt.Errorf("%s requires a value", candidate)
			}
			return candidate, value, true, false, nil
		}
	}
	return "", "", false, false, nil
}

func isRunFlag(argument string) bool {
	for _, candidate := range []string{"--format", "--update-history", "--run-envelope", "--retain-runs"} {
		if argument == candidate || strings.HasPrefix(argument, candidate+"=") {
			return true
		}
	}
	return false
}

func loadArtifacts(roots []string) ([]artifact, int, error) {
	paths, err := artifactPaths(roots)
	if err != nil {
		return nil, 0, err
	}
	if len(paths) == 0 {
		return nil, 0, errors.New("no JSON timing artifacts found")
	}

	seen := make(map[ArtifactIdentity]artifact, len(paths))
	seenPath := make(map[ArtifactIdentity]string, len(paths))
	artifacts := make([]artifact, 0, len(paths))
	duplicateCount := 0
	for _, path := range paths {
		item, err := decodeArtifact(path)
		if err != nil {
			return nil, 0, err
		}
		identity := ArtifactIdentity{
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
	if *envelope.Schema != timingArtifactSchema {
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

func formatIdentity(identity ArtifactIdentity) string {
	return fmt.Sprintf("workflow=%q run=%q attempt=%q job=%q shard=%q variant=%q",
		identity.Workflow, identity.RunID, identity.RunAttempt, identity.Job, identity.ShardID, identity.Variant)
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

func renderMarkdown(snapshot Snapshot) string {
	var output strings.Builder
	output.WriteString("# Go test timing summary\n\n")
	fmt.Fprintf(&output, "Analyzed %d unique schema-v1 %s; %d duplicate %s ignored.\n\n",
		snapshot.UniqueArtifactCount, plural(snapshot.UniqueArtifactCount, "artifact", "artifacts"), snapshot.DuplicateArtifactCount,
		plural(snapshot.DuplicateArtifactCount, "download", "downloads"))
	output.WriteString("Rankings use successful durations from top-level tests only. Package totals and nested subtests are excluded. Profiles are never mixed.\n\n")

	for index, profile := range snapshot.Profiles {
		fmt.Fprintf(&output, "## Profile %d\n\n", index+1)
		output.WriteString("| Job | Variant | Runner label | OS | Arch | CPUs |\n")
		output.WriteString("| --- | --- | --- | --- | --- | ---: |\n")
		fmt.Fprintf(&output, "| %s | %s | %s | %s | %s | %d |\n\n",
			codeCell(profile.Job), codeCell(profile.Variant), codeCell(profile.Runner.Label),
			codeCell(profile.Runner.OS), codeCell(profile.Runner.Arch), profile.Runner.CPUCount)
		passes, failures, skips := profileOutcomeCounts(profile.Units)
		fmt.Fprintf(&output, "Top-level outcomes: %d pass, %d fail, %d skip.\n\n",
			passes, failures, skips)

		writeSlowestTable(&output, profile.Units)
		writeVarianceTable(&output, profile.Units)
	}
	return output.String()
}

func profileOutcomeCounts(units []UnitHistory) (int, int, int) {
	var passes, failures, skips int
	for _, unit := range units {
		passes += unit.Passes
		failures += unit.Failures
		skips += unit.Skips
	}
	return passes, failures, skips
}

func writeSlowestTable(output *strings.Builder, units []UnitHistory) {
	output.WriteString("### Ten slowest top-level tests\n\n")
	output.WriteString("| Runnable unit | Pass | Fail | Skip | p50 | p75 | p95 |\n")
	output.WriteString("| --- | ---: | ---: | ---: | ---: | ---: | ---: |\n")
	ordered := make([]UnitHistory, 0, len(units))
	for _, unit := range units {
		if unit.Passes > 0 {
			ordered = append(ordered, unit)
		}
	}
	sort.Slice(ordered, func(i, j int) bool {
		if *ordered[i].DurationSecondsP95 != *ordered[j].DurationSecondsP95 {
			return *ordered[i].DurationSecondsP95 > *ordered[j].DurationSecondsP95
		}
		return ordered[i].UnitID < ordered[j].UnitID
	})
	if len(ordered) == 0 {
		output.WriteString("| _No top-level tests with successful samples_ | 0 | 0 | 0 | — | — | — |\n\n")
		return
	}
	for _, unit := range ordered[:min(summaryLimit, len(ordered))] {
		fmt.Fprintf(output, "| `%s` | %d | %d | %d | %.3fs | %.3fs | %.3fs |\n",
			escapeCode(unit.UnitID), unit.Passes, unit.Failures, unit.Skips,
			*unit.DurationSecondsP50, *unit.DurationSecondsP75, *unit.DurationSecondsP95)
	}
	output.WriteByte('\n')
}

func writeVarianceTable(output *strings.Builder, units []UnitHistory) {
	output.WriteString("### Ten highest-variance top-level tests\n\n")
	output.WriteString("| Runnable unit | Pass | Fail | Skip | Population variance (s²) | p95 |\n")
	output.WriteString("| --- | ---: | ---: | ---: | ---: | ---: |\n")
	eligible := make([]UnitHistory, 0, len(units))
	for _, unit := range units {
		if unit.Passes >= 2 {
			eligible = append(eligible, unit)
		}
	}
	sort.Slice(eligible, func(i, j int) bool {
		if *eligible[i].DurationSecondsPopulationVariance != *eligible[j].DurationSecondsPopulationVariance {
			return *eligible[i].DurationSecondsPopulationVariance > *eligible[j].DurationSecondsPopulationVariance
		}
		return eligible[i].UnitID < eligible[j].UnitID
	})
	if len(eligible) == 0 {
		output.WriteString("| _At least two successful samples are required_ | 0 | 0 | 0 | — | — |\n\n")
		return
	}
	for _, unit := range eligible[:min(summaryLimit, len(eligible))] {
		fmt.Fprintf(output, "| `%s` | %d | %d | %d | %.6f | %.3fs |\n",
			escapeCode(unit.UnitID), unit.Passes, unit.Failures, unit.Skips,
			*unit.DurationSecondsPopulationVariance, *unit.DurationSecondsP95)
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
