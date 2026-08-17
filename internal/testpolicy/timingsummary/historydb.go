package timingsummary

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
)

const (
	// HistoryDatabaseSchema is the current normalized timing-history database schema.
	HistoryDatabaseSchema = 1
	// RunEnvelopeSchema is the current trusted-run envelope schema.
	RunEnvelopeSchema = 1
)

// RunEnvelope identifies the trusted workflow run that produced a cohort of
// timing artifacts. The storage layer validates its shape and artifact
// agreement; the caller is responsible for authenticating its provenance.
type RunEnvelope struct {
	Schema      int    `json:"schema"`
	Repository  string `json:"repository"`
	Event       string `json:"event"`
	Ref         string `json:"ref"`
	Workflow    string `json:"workflow"`
	RunID       string `json:"run_id"`
	RunAttempt  string `json:"run_attempt"`
	TestedSHA   string `json:"tested_sha"`
	Conclusion  string `json:"conclusion"`
	CompletedAt string `json:"completed_at"`
}

// HistoryDatabase stores normalized timing evidence. Persisted indexes are a
// compact wire representation only; merge and retention decisions use stable
// run and artifact identities.
type HistoryDatabase struct {
	Schema    int               `json:"schema"`
	Runs      []RunEnvelope     `json:"runs"`
	Artifacts []HistoryArtifact `json:"artifacts"`
	Units     []HistoryUnit     `json:"units"`
}

// HistoryArtifact records the run-local identity and runner profile shared by
// all samples captured in one schema-v1 artifact.
type HistoryArtifact struct {
	RunIndex int           `json:"run_index"`
	Job      string        `json:"job"`
	ShardID  string        `json:"shard_id"`
	Variant  string        `json:"variant"`
	Runner   HistoryRunner `json:"runner"`
}

// HistoryRunner retains the complete schema-v1 runner evidence, including the
// ephemeral name used to detect conflicting copies of one artifact.
type HistoryRunner struct {
	Label    string `json:"label"`
	Name     string `json:"name"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	CPUCount int    `json:"cpu_count"`
}

// HistoryUnit stores one stable unit identity and all retained samples for it.
type HistoryUnit struct {
	UnitID  string          `json:"unit_id"`
	Kind    string          `json:"kind"`
	Package string          `json:"package"`
	Test    string          `json:"test"`
	Subtest string          `json:"subtest"`
	Samples []HistorySample `json:"samples"`
}

// HistorySample references the artifact that supplied one terminal unit row.
// Duplicate rows inside an artifact remain distinct samples.
type HistorySample struct {
	ArtifactIndex   int     `json:"artifact_index"`
	Outcome         string  `json:"outcome"`
	DurationSeconds float64 `json:"duration_seconds"`
}

type historyRunKey struct {
	Repository string
	Workflow   string
	RunID      string
	RunAttempt string
}

type historyArtifactKey struct {
	Run     historyRunKey
	Job     string
	ShardID string
	Variant string
}

type historyArtifactEvidence struct {
	key      historyArtifactKey
	artifact artifact
}

type historyUnitIdentity struct {
	UnitID  string
	Kind    string
	Package string
	Test    string
	Subtest string
}

type historyUnitAccumulator struct {
	identity historyUnitIdentity
	samples  []HistorySample
}

// UpdateHistory merges one run's validated schema-v1 artifacts into a
// normalized database, retains the newest retainRuns whole cohorts, publishes
// the database atomically, and returns the existing Snapshot projection of the
// retained evidence.
func UpdateHistory(databasePath string, envelope RunEnvelope, retainRuns int, roots []string) (Snapshot, error) {
	if strings.TrimSpace(databasePath) == "" {
		return Snapshot{}, errors.New("history database path is required")
	}
	if retainRuns <= 0 {
		return Snapshot{}, errors.New("retain-runs must be a positive integer")
	}
	if len(roots) == 0 {
		return Snapshot{}, errors.New("at least one artifact root is required")
	}

	normalizedEnvelope, _, err := normalizeRunEnvelope(envelope)
	if err != nil {
		return Snapshot{}, fmt.Errorf("validate run envelope: %w", err)
	}
	existingBytes, runs, evidence, err := loadHistoryDatabase(databasePath)
	if err != nil {
		return Snapshot{}, err
	}
	if len(runs) > 0 && runs[0].Repository != normalizedEnvelope.Repository {
		return Snapshot{}, fmt.Errorf("history database repository %q does not match run envelope repository %q",
			runs[0].Repository, normalizedEnvelope.Repository)
	}

	incoming, _, err := loadArtifacts(roots)
	if err != nil {
		return Snapshot{}, err
	}
	for index := range incoming {
		incoming[index] = canonicalHistoryArtifact(incoming[index])
		if err := validateArtifactEnvelope(incoming[index], normalizedEnvelope); err != nil {
			return Snapshot{}, fmt.Errorf("validate incoming artifact %s: %w", formatArtifactIdentity(incoming[index]), err)
		}
	}

	runsByKey := make(map[historyRunKey]RunEnvelope, len(runs)+1)
	for _, run := range runs {
		runsByKey[runEnvelopeKey(run)] = run
	}
	incomingRunKey := runEnvelopeKey(normalizedEnvelope)
	if previous, ok := runsByKey[incomingRunKey]; ok {
		if !reflect.DeepEqual(previous, normalizedEnvelope) {
			return Snapshot{}, fmt.Errorf("conflicting run envelope for %s", formatHistoryRunKey(incomingRunKey))
		}
	} else {
		runsByKey[incomingRunKey] = normalizedEnvelope
	}

	evidenceByKey := make(map[historyArtifactKey]artifact, len(evidence)+len(incoming))
	for _, stored := range evidence {
		evidenceByKey[stored.key] = stored.artifact
	}
	for _, item := range incoming {
		key := historyArtifactKeyFor(normalizedEnvelope, item)
		if previous, ok := evidenceByKey[key]; ok {
			if !reflect.DeepEqual(previous, item) {
				return Snapshot{}, fmt.Errorf("conflicting artifact for %s", formatHistoryArtifactKey(key))
			}
			continue
		}
		evidenceByKey[key] = item
	}

	retainedRuns, retainedKeys, err := retainHistoryRuns(runsByKey, retainRuns)
	if err != nil {
		return Snapshot{}, err
	}
	retainedEvidence := make([]historyArtifactEvidence, 0, len(evidenceByKey))
	for key, item := range evidenceByKey {
		if _, keep := retainedKeys[key.Run]; keep {
			retainedEvidence = append(retainedEvidence, historyArtifactEvidence{key: key, artifact: item})
		}
	}

	database, artifacts, err := materializeHistoryDatabase(retainedRuns, retainedEvidence)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot, err := buildSnapshot(artifacts, 0)
	if err != nil {
		return Snapshot{}, fmt.Errorf("build retained snapshot: %w", err)
	}
	encoded, err := encodeHistoryDatabase(database)
	if err != nil {
		return Snapshot{}, err
	}
	if bytes.Equal(existingBytes, encoded) {
		return snapshot, nil
	}
	if err := writeHistoryDatabaseAtomically(databasePath, encoded); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

func normalizeRunEnvelope(envelope RunEnvelope) (RunEnvelope, time.Time, error) {
	if envelope.Schema != RunEnvelopeSchema {
		return RunEnvelope{}, time.Time{}, fmt.Errorf("unsupported schema %d", envelope.Schema)
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "repository", value: envelope.Repository},
		{name: "event", value: envelope.Event},
		{name: "ref", value: envelope.Ref},
		{name: "workflow", value: envelope.Workflow},
		{name: "run_id", value: envelope.RunID},
		{name: "run_attempt", value: envelope.RunAttempt},
		{name: "tested_sha", value: envelope.TestedSHA},
		{name: "conclusion", value: envelope.Conclusion},
		{name: "completed_at", value: envelope.CompletedAt},
	} {
		if strings.TrimSpace(field.value) == "" {
			return RunEnvelope{}, time.Time{}, fmt.Errorf("%s is required", field.name)
		}
	}
	completedAt, err := time.Parse(time.RFC3339Nano, envelope.CompletedAt)
	if err != nil {
		return RunEnvelope{}, time.Time{}, fmt.Errorf("completed_at must be RFC3339: %w", err)
	}
	if completedAt.IsZero() {
		return RunEnvelope{}, time.Time{}, errors.New("completed_at must not be zero")
	}
	envelope.CompletedAt = completedAt.UTC().Format(time.RFC3339Nano)
	return envelope, completedAt.UTC(), nil
}

func validateArtifactEnvelope(item artifact, envelope RunEnvelope) error {
	for _, field := range []struct {
		name     string
		artifact string
		envelope string
	}{
		{name: "workflow", artifact: item.Workflow, envelope: envelope.Workflow},
		{name: "run_id", artifact: item.RunID, envelope: envelope.RunID},
		{name: "run_attempt", artifact: item.RunAttempt, envelope: envelope.RunAttempt},
		{name: "tested_sha", artifact: item.CommitSHA, envelope: envelope.TestedSHA},
	} {
		if field.artifact != field.envelope {
			return fmt.Errorf("%s %q does not match run envelope %q", field.name, field.artifact, field.envelope)
		}
	}
	return nil
}

func loadHistoryDatabase(path string) ([]byte, []RunEnvelope, []historyArtifactEvidence, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil, nil
	}
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read history database %q: %w", path, err)
	}

	var header struct {
		Schema *int `json:"schema"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, nil, nil, fmt.Errorf("decode history database schema %q: %w", path, err)
	}
	if header.Schema == nil {
		return nil, nil, nil, fmt.Errorf("validate history database %q: schema is required", path)
	}
	if *header.Schema != HistoryDatabaseSchema {
		return nil, nil, nil, fmt.Errorf("validate history database %q: unsupported schema %d", path, *header.Schema)
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var database HistoryDatabase
	if err := decoder.Decode(&database); err != nil {
		return nil, nil, nil, fmt.Errorf("decode history database %q: %w", path, err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, nil, nil, fmt.Errorf("decode history database %q: %w", path, err)
	}
	runs, evidence, err := validateStoredHistory(database)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("validate history database %q: %w", path, err)
	}
	return data, runs, evidence, nil
}

func validateStoredHistory(database HistoryDatabase) ([]RunEnvelope, []historyArtifactEvidence, error) {
	if database.Schema != HistoryDatabaseSchema {
		return nil, nil, fmt.Errorf("unsupported schema %d", database.Schema)
	}
	if database.Runs == nil || database.Artifacts == nil || database.Units == nil {
		return nil, nil, errors.New("runs, artifacts, and units must be JSON arrays")
	}
	if len(database.Runs) == 0 {
		return nil, nil, errors.New("runs must not be empty")
	}

	runs := make([]RunEnvelope, len(database.Runs))
	runKeys := make(map[historyRunKey]struct{}, len(database.Runs))
	repository := ""
	for index, raw := range database.Runs {
		run, _, err := normalizeRunEnvelope(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("runs[%d]: %w", index, err)
		}
		if repository == "" {
			repository = run.Repository
		} else if run.Repository != repository {
			return nil, nil, fmt.Errorf("runs[%d]: repository %q does not match database repository %q", index, run.Repository, repository)
		}
		key := runEnvelopeKey(run)
		if _, exists := runKeys[key]; exists {
			return nil, nil, fmt.Errorf("duplicate run %s", formatHistoryRunKey(key))
		}
		runKeys[key] = struct{}{}
		runs[index] = run
	}

	evidence := make([]historyArtifactEvidence, len(database.Artifacts))
	artifactKeys := make(map[historyArtifactKey]struct{}, len(database.Artifacts))
	artifactsPerRun := make([]int, len(runs))
	for index, stored := range database.Artifacts {
		if stored.RunIndex < 0 || stored.RunIndex >= len(runs) {
			return nil, nil, fmt.Errorf("artifacts[%d]: run_index %d is out of range", index, stored.RunIndex)
		}
		run := runs[stored.RunIndex]
		item := artifact{
			Schema: timingArtifactSchema, ShardID: stored.ShardID, Variant: stored.Variant,
			CommitSHA: run.TestedSHA, Workflow: run.Workflow, RunID: run.RunID,
			RunAttempt: run.RunAttempt, Job: stored.Job,
			Runner: artifactRunner{
				Label: stored.Runner.Label, Name: stored.Runner.Name, OS: stored.Runner.OS,
				Arch: stored.Runner.Arch, CPUCount: stored.Runner.CPUCount,
			},
			Units: make([]artifactUnit, 0),
		}
		key := historyArtifactKeyFor(run, item)
		if _, exists := artifactKeys[key]; exists {
			return nil, nil, fmt.Errorf("duplicate artifact %s", formatHistoryArtifactKey(key))
		}
		artifactKeys[key] = struct{}{}
		artifactsPerRun[stored.RunIndex]++
		evidence[index] = historyArtifactEvidence{key: key, artifact: item}
	}
	for index, count := range artifactsPerRun {
		if count == 0 {
			return nil, nil, fmt.Errorf("runs[%d]: run has no artifacts", index)
		}
	}

	unitIDs := make(map[string]historyUnitIdentity, len(database.Units))
	for unitIndex, stored := range database.Units {
		identity := historyUnitIdentity{
			UnitID: stored.UnitID, Kind: stored.Kind, Package: stored.Package,
			Test: stored.Test, Subtest: stored.Subtest,
		}
		if previous, exists := unitIDs[identity.UnitID]; exists {
			return nil, nil, fmt.Errorf("units[%d]: duplicate unit %q conflicts with %s", unitIndex, identity.UnitID, formatHistoryUnitIdentity(previous))
		}
		unitIDs[identity.UnitID] = identity
		if len(stored.Samples) == 0 {
			return nil, nil, fmt.Errorf("units[%d]: samples must not be empty", unitIndex)
		}
		for sampleIndex, sample := range stored.Samples {
			if sample.ArtifactIndex < 0 || sample.ArtifactIndex >= len(evidence) {
				return nil, nil, fmt.Errorf("units[%d].samples[%d]: artifact_index %d is out of range", unitIndex, sampleIndex, sample.ArtifactIndex)
			}
			unit := artifactUnit{
				UnitID: identity.UnitID, Kind: identity.Kind, Package: identity.Package,
				Test: identity.Test, Subtest: identity.Subtest, Outcome: sample.Outcome,
				DurationSeconds: canonicalFloat(sample.DurationSeconds),
			}
			if err := validateUnit(unit); err != nil {
				return nil, nil, fmt.Errorf("units[%d].samples[%d]: %w", unitIndex, sampleIndex, err)
			}
			evidence[sample.ArtifactIndex].artifact.Units = append(evidence[sample.ArtifactIndex].artifact.Units, unit)
		}
	}
	for index := range evidence {
		evidence[index].artifact = canonicalHistoryArtifact(evidence[index].artifact)
		if err := validateArtifact(evidence[index].artifact); err != nil {
			return nil, nil, fmt.Errorf("artifacts[%d]: %w", index, err)
		}
	}
	return runs, evidence, nil
}

func retainHistoryRuns(runsByKey map[historyRunKey]RunEnvelope, retainRuns int) ([]RunEnvelope, map[historyRunKey]struct{}, error) {
	type completedRun struct {
		key       historyRunKey
		envelope  RunEnvelope
		completed time.Time
	}
	runs := make([]completedRun, 0, len(runsByKey))
	for key, envelope := range runsByKey {
		normalized, completed, err := normalizeRunEnvelope(envelope)
		if err != nil {
			return nil, nil, fmt.Errorf("validate retained run %s: %w", formatHistoryRunKey(key), err)
		}
		runs = append(runs, completedRun{key: key, envelope: normalized, completed: completed})
	}
	sort.Slice(runs, func(i, j int) bool {
		if !runs[i].completed.Equal(runs[j].completed) {
			return runs[i].completed.Before(runs[j].completed)
		}
		return compareHistoryRunKey(runs[i].key, runs[j].key) < 0
	})
	if len(runs) > retainRuns {
		runs = runs[len(runs)-retainRuns:]
	}
	retained := make(map[historyRunKey]struct{}, len(runs))
	result := make([]RunEnvelope, 0, len(runs))
	for _, run := range runs {
		retained[run.key] = struct{}{}
		result = append(result, run.envelope)
	}
	return result, retained, nil
}

func materializeHistoryDatabase(runs []RunEnvelope, evidence []historyArtifactEvidence) (HistoryDatabase, []artifact, error) {
	sort.Slice(runs, func(i, j int) bool {
		return compareHistoryRunKey(runEnvelopeKey(runs[i]), runEnvelopeKey(runs[j])) < 0
	})
	runIndexes := make(map[historyRunKey]int, len(runs))
	for index, run := range runs {
		runIndexes[runEnvelopeKey(run)] = index
	}
	sort.Slice(evidence, func(i, j int) bool {
		return compareHistoryArtifactKey(evidence[i].key, evidence[j].key) < 0
	})

	storedArtifacts := make([]HistoryArtifact, 0, len(evidence))
	artifacts := make([]artifact, 0, len(evidence))
	units := make(map[string]*historyUnitAccumulator)
	for artifactIndex, stored := range evidence {
		runIndex, ok := runIndexes[stored.key.Run]
		if !ok {
			return HistoryDatabase{}, nil, fmt.Errorf("artifact %s references a pruned run", formatHistoryArtifactKey(stored.key))
		}
		item := canonicalHistoryArtifact(stored.artifact)
		storedArtifacts = append(storedArtifacts, HistoryArtifact{
			RunIndex: runIndex, Job: item.Job, ShardID: item.ShardID, Variant: item.Variant,
			Runner: HistoryRunner{
				Label: item.Runner.Label, Name: item.Runner.Name, OS: item.Runner.OS,
				Arch: item.Runner.Arch, CPUCount: item.Runner.CPUCount,
			},
		})
		artifacts = append(artifacts, item)
		for _, unit := range item.Units {
			identity := historyUnitIdentity{
				UnitID: unit.UnitID, Kind: unit.Kind, Package: unit.Package,
				Test: unit.Test, Subtest: unit.Subtest,
			}
			accumulator := units[unit.UnitID]
			if accumulator == nil {
				accumulator = &historyUnitAccumulator{identity: identity, samples: make([]HistorySample, 0)}
				units[unit.UnitID] = accumulator
			} else if accumulator.identity != identity {
				return HistoryDatabase{}, nil, fmt.Errorf("conflicting identity for unit %q: %s != %s",
					unit.UnitID, formatHistoryUnitIdentity(accumulator.identity), formatHistoryUnitIdentity(identity))
			}
			accumulator.samples = append(accumulator.samples, HistorySample{
				ArtifactIndex: artifactIndex, Outcome: unit.Outcome,
				DurationSeconds: canonicalFloat(unit.DurationSeconds),
			})
		}
	}

	unitIDs := make([]string, 0, len(units))
	for unitID := range units {
		unitIDs = append(unitIDs, unitID)
	}
	sort.Strings(unitIDs)
	storedUnits := make([]HistoryUnit, 0, len(unitIDs))
	for _, unitID := range unitIDs {
		accumulator := units[unitID]
		sort.SliceStable(accumulator.samples, func(i, j int) bool {
			left, right := accumulator.samples[i], accumulator.samples[j]
			if left.ArtifactIndex != right.ArtifactIndex {
				return left.ArtifactIndex < right.ArtifactIndex
			}
			if left.Outcome != right.Outcome {
				return left.Outcome < right.Outcome
			}
			return left.DurationSeconds < right.DurationSeconds
		})
		storedUnits = append(storedUnits, HistoryUnit{
			UnitID: accumulator.identity.UnitID, Kind: accumulator.identity.Kind,
			Package: accumulator.identity.Package, Test: accumulator.identity.Test,
			Subtest: accumulator.identity.Subtest, Samples: accumulator.samples,
		})
	}
	return HistoryDatabase{
		Schema: HistoryDatabaseSchema, Runs: runs, Artifacts: storedArtifacts, Units: storedUnits,
	}, artifacts, nil
}

func canonicalHistoryArtifact(item artifact) artifact {
	item.Units = append([]artifactUnit(nil), item.Units...)
	for index := range item.Units {
		item.Units[index].DurationSeconds = canonicalFloat(item.Units[index].DurationSeconds)
	}
	sort.SliceStable(item.Units, func(i, j int) bool {
		left, right := item.Units[i], item.Units[j]
		leftFields := []string{left.UnitID, left.Kind, left.Package, left.Test, left.Subtest, left.Outcome}
		rightFields := []string{right.UnitID, right.Kind, right.Package, right.Test, right.Subtest, right.Outcome}
		for index := range leftFields {
			if leftFields[index] != rightFields[index] {
				return leftFields[index] < rightFields[index]
			}
		}
		return left.DurationSeconds < right.DurationSeconds
	})
	return item
}

func encodeHistoryDatabase(database HistoryDatabase) ([]byte, error) {
	data, err := json.Marshal(database)
	if err != nil {
		return nil, fmt.Errorf("encode history database: %w", err)
	}
	return append(data, '\n'), nil
}

func writeHistoryDatabaseAtomically(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create history database directory %q: %w", dir, err)
	}
	temporary, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary history database beside %q: %w", path, err)
	}
	temporaryPath := temporary.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o644); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("chmod temporary history database: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary history database: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary history database: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary history database: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish history database %q: %w", path, err)
	}
	cleanup = false
	return nil
}

func decodeRunEnvelope(path string) (RunEnvelope, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return RunEnvelope{}, fmt.Errorf("read run envelope %q: %w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var envelope RunEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return RunEnvelope{}, fmt.Errorf("decode run envelope %q: %w", path, err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return RunEnvelope{}, fmt.Errorf("decode run envelope %q: %w", path, err)
	}
	normalized, _, err := normalizeRunEnvelope(envelope)
	if err != nil {
		return RunEnvelope{}, fmt.Errorf("validate run envelope %q: %w", path, err)
	}
	return normalized, nil
}

func runEnvelopeKey(envelope RunEnvelope) historyRunKey {
	return historyRunKey{
		Repository: envelope.Repository, Workflow: envelope.Workflow,
		RunID: envelope.RunID, RunAttempt: envelope.RunAttempt,
	}
}

func historyArtifactKeyFor(envelope RunEnvelope, item artifact) historyArtifactKey {
	return historyArtifactKey{
		Run: runEnvelopeKey(envelope), Job: item.Job, ShardID: item.ShardID, Variant: item.Variant,
	}
}

func compareHistoryRunKey(left, right historyRunKey) int {
	leftFields := []string{left.Repository, left.Workflow, left.RunID, left.RunAttempt}
	rightFields := []string{right.Repository, right.Workflow, right.RunID, right.RunAttempt}
	for index := range leftFields {
		if leftFields[index] < rightFields[index] {
			return -1
		}
		if leftFields[index] > rightFields[index] {
			return 1
		}
	}
	return 0
}

func compareHistoryArtifactKey(left, right historyArtifactKey) int {
	if result := compareHistoryRunKey(left.Run, right.Run); result != 0 {
		return result
	}
	leftFields := []string{left.Job, left.ShardID, left.Variant}
	rightFields := []string{right.Job, right.ShardID, right.Variant}
	for index := range leftFields {
		if leftFields[index] < rightFields[index] {
			return -1
		}
		if leftFields[index] > rightFields[index] {
			return 1
		}
	}
	return 0
}

func formatHistoryRunKey(key historyRunKey) string {
	return fmt.Sprintf("repository=%q workflow=%q run=%q attempt=%q", key.Repository, key.Workflow, key.RunID, key.RunAttempt)
}

func formatHistoryArtifactKey(key historyArtifactKey) string {
	return fmt.Sprintf("%s job=%q shard=%q variant=%q", formatHistoryRunKey(key.Run), key.Job, key.ShardID, key.Variant)
}

func formatArtifactIdentity(item artifact) string {
	return formatIdentity(ArtifactIdentity{
		Workflow: item.Workflow, RunID: item.RunID, RunAttempt: item.RunAttempt,
		Job: item.Job, ShardID: item.ShardID, Variant: item.Variant,
	})
}

func formatHistoryUnitIdentity(identity historyUnitIdentity) string {
	return fmt.Sprintf("kind=%q package=%q test=%q subtest=%q", identity.Kind, identity.Package, identity.Test, identity.Subtest)
}
