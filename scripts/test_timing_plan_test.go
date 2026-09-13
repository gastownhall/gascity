package scripts_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/testpolicy/timingplan"
	"github.com/gastownhall/gascity/internal/testpolicy/timingplancli"
	"github.com/gastownhall/gascity/internal/testpolicy/timingsummary"
)

type timingPlanInventoryDocument struct {
	Schema int                        `json:"schema"`
	Units  []timingplan.InventoryUnit `json:"units"`
}

type timingPlanConfigDocument struct {
	Schema        int                        `json:"schema"`
	Profile       timingplan.ProfileSelector `json:"profile"`
	Shards        int                        `json:"shards"`
	Defaults      timingplan.StaticTiming    `json:"defaults"`
	P95CapSeconds float64                    `json:"p95_cap_seconds"`
}

type timingPlanOutputDocument struct {
	Schema               int                        `json:"schema"`
	Authority            string                     `json:"authority"`
	HistorySchema        int                        `json:"history_schema"`
	Profile              timingplan.ProfileSelector `json:"profile"`
	HistoryProfileStatus string                     `json:"history_profile_status"`
	Plan                 timingplan.Result          `json:"plan"`
}

func TestTimingPlanCommand(t *testing.T) {
	t.Run("adapts exact profile and emits canonical dry-run output", func(t *testing.T) {
		selector := timingPlanCommandSelector()
		target := timingPlanCommandProfile(selector, []timingsummary.UnitHistory{
			timingPlanCommandUnit(selector, "current/warm", "pkg/current", "TestWarm", 20, 7, 8, 9, 1),
			timingPlanCommandUnit(selector, "current/cold", "pkg/current", "TestCold", 4, 1, 2, 3, 0.25),
			timingPlanCommandUnit(selector, "current/oversized", "pkg/current", "TestOversized", 20, 60, 80, 120, 900),
			timingPlanCommandUnit(selector, "deleted/stale", "pkg/deleted", "TestStale", 20, 400, 500, 600, 4),
		})
		otherSelector := selector
		otherSelector.Runner.CPUCount = 16
		other := timingPlanCommandProfile(otherSelector, []timingsummary.UnitHistory{
			timingPlanCommandUnit(otherSelector, "current/warm", "pkg/current", "TestWarm", 20, 70, 80, 90, 10),
		})
		snapshot := timingsummary.Snapshot{
			Schema: timingsummary.SnapshotSchema, UniqueArtifactCount: 20,
			Profiles: []timingsummary.Profile{other, target},
		}
		inventory := timingPlanInventoryDocument{
			Schema: 1,
			Units: []timingplan.InventoryUnit{
				{UnitID: "current/cold"},
				{UnitID: "current/missing"},
				{UnitID: "current/oversized"},
				{UnitID: "current/warm"},
			},
		}
		config := timingPlanConfigDocument{
			Schema: 1, Profile: selector, Shards: 2,
			Defaults:      timingplan.StaticTiming{P50Seconds: 4, P75Seconds: 10, P95Seconds: 20},
			P95CapSeconds: 90,
		}

		firstDir := t.TempDir()
		firstArgs, firstPaths := writeTimingPlanInputs(t, firstDir, inventory, snapshot, config)
		before := readTimingPlanInputs(t, firstPaths)
		firstCode, firstStdout, firstStderr := runTimingPlanCommand(firstArgs...)
		if firstCode != 0 || len(firstStderr) != 0 {
			t.Fatalf("first command exit=%d stderr=%s", firstCode, firstStderr)
		}
		if after := readTimingPlanInputs(t, firstPaths); !reflect.DeepEqual(after, before) {
			t.Fatal("dry-run command modified an input file")
		}

		shuffledInventory := inventory
		shuffledInventory.Units = slices.Clone(inventory.Units)
		slices.Reverse(shuffledInventory.Units)
		shuffledTarget := target
		shuffledTarget.Units = slices.Clone(target.Units)
		slices.Reverse(shuffledTarget.Units)
		shuffledSnapshot := snapshot
		shuffledSnapshot.Profiles = []timingsummary.Profile{shuffledTarget, other}
		secondArgs, _ := writeTimingPlanInputs(t, t.TempDir(), shuffledInventory, shuffledSnapshot, config)
		secondCode, secondStdout, secondStderr := runTimingPlanCommand(secondArgs...)
		if secondCode != 0 || len(secondStderr) != 0 {
			t.Fatalf("shuffled command exit=%d stderr=%s", secondCode, secondStderr)
		}
		if !bytes.Equal(firstStdout, secondStdout) {
			t.Fatalf("shuffled inputs changed canonical output\nfirst:    %s\nshuffled: %s", firstStdout, secondStdout)
		}

		var output timingPlanOutputDocument
		if err := json.Unmarshal(firstStdout, &output); err != nil {
			t.Fatalf("decode output: %v\n%s", err, firstStdout)
		}
		if output.Schema != 1 || output.Authority != "dry-run" || output.HistorySchema != timingsummary.SnapshotSchema {
			t.Fatalf("output envelope = schema %d authority %q history schema %d", output.Schema, output.Authority, output.HistorySchema)
		}
		if output.Profile != selector || output.HistoryProfileStatus != "matched" {
			t.Fatalf("output profile/status = %+v/%q, want exact selector/matched", output.Profile, output.HistoryProfileStatus)
		}
		assignments := timingPlanAssignments(output.Plan)
		if got := sortedTimingPlanKeys(assignments); !reflect.DeepEqual(got, []string{"current/cold", "current/missing", "current/oversized", "current/warm"}) {
			t.Fatalf("assigned units = %v, want exact current inventory", got)
		}
		if assignments["current/warm"].P75Seconds != 8 {
			t.Fatalf("warm assignment = %+v, want exact-profile empirical p75 8", assignments["current/warm"])
		}
		if assignments["current/cold"].P75Source != "static" || assignments["current/missing"].Reason != "history-missing" {
			t.Fatalf("cold/missing fallbacks = %+v / %+v", assignments["current/cold"], assignments["current/missing"])
		}
		if !slices.Contains(assignments["current/oversized"].Hazards, "p95-cap-exceeded") {
			t.Fatalf("oversized hazards = %v, want p95-cap-exceeded", assignments["current/oversized"].Hazards)
		}
	})

	t.Run("missing profile degrades visibly to static", func(t *testing.T) {
		selector := timingPlanCommandSelector()
		snapshot := timingsummary.Snapshot{
			Schema: timingsummary.SnapshotSchema,
			Profiles: []timingsummary.Profile{timingPlanCommandProfile(selector, []timingsummary.UnitHistory{
				timingPlanCommandUnit(selector, "current/warm", "pkg/current", "TestWarm", 20, 7, 8, 9, 1),
			})},
		}
		config := timingPlanConfigDocument{
			Schema: 1, Profile: selector, Shards: 1,
			Defaults:      timingplan.StaticTiming{P50Seconds: 4, P75Seconds: 10, P95Seconds: 20},
			P95CapSeconds: 90,
		}
		config.Profile.Runner.CPUCount++
		args, _ := writeTimingPlanInputs(t, t.TempDir(), timingPlanInventoryDocument{
			Schema: 1, Units: []timingplan.InventoryUnit{{UnitID: "current/warm"}},
		}, snapshot, config)
		code, stdout, stderr := runTimingPlanCommand(args...)
		if code != 0 || len(stderr) != 0 {
			t.Fatalf("command exit=%d stderr=%s", code, stderr)
		}
		var output timingPlanOutputDocument
		if err := json.Unmarshal(stdout, &output); err != nil {
			t.Fatalf("decode output: %v", err)
		}
		assignment := timingPlanAssignments(output.Plan)["current/warm"]
		if output.HistoryProfileStatus != "profile-missing" || assignment.P75Source != "static" || assignment.Reason != "history-missing" {
			t.Fatalf("missing-profile output = status %q assignment %+v", output.HistoryProfileStatus, assignment)
		}
	})

	t.Run("rejects malformed versioned input without stdout", func(t *testing.T) {
		selector := timingPlanCommandSelector()
		validInventory := timingPlanInventoryDocument{Schema: 1, Units: []timingplan.InventoryUnit{{UnitID: "current/warm"}}}
		validSnapshot := timingsummary.Snapshot{
			Schema: timingsummary.SnapshotSchema,
			Profiles: []timingsummary.Profile{timingPlanCommandProfile(selector, []timingsummary.UnitHistory{
				timingPlanCommandUnit(selector, "current/warm", "pkg/current", "TestWarm", 5, 7, 8, 9, 1),
			})},
		}
		validConfig := timingPlanConfigDocument{
			Schema: 1, Profile: selector, Shards: 1,
			Defaults:      timingplan.StaticTiming{P50Seconds: 4, P75Seconds: 10, P95Seconds: 20},
			P95CapSeconds: 90,
		}

		tests := []struct {
			name      string
			inventory any
			history   any
			config    any
			wantError string
		}{
			{name: "missing inventory schema", inventory: json.RawMessage(`{"units":[]}`), history: validSnapshot, config: validConfig, wantError: "inventory schema is required"},
			{name: "unsupported config schema", inventory: validInventory, history: validSnapshot, config: func() any {
				invalid := validConfig
				invalid.Schema = 2
				return invalid
			}(), wantError: "unsupported config schema 2"},
			{name: "null inventory", inventory: json.RawMessage(`{"schema":1,"units":null}`), history: validSnapshot, config: validConfig, wantError: "inventory units must be an array"},
			{name: "unknown history field", inventory: validInventory, history: json.RawMessage(`{"schema":1,"unique_artifact_count":0,"duplicate_artifact_count":0,"profiles":[],"unknown":true}`), config: validConfig, wantError: "unknown field"},
			{name: "trailing JSON", inventory: json.RawMessage(`{"schema":1,"units":[]} {}`), history: validSnapshot, config: validConfig, wantError: "multiple JSON values"},
			{name: "planner validation", inventory: validInventory, history: validSnapshot, config: func() any {
				invalid := validConfig
				invalid.Shards = 257
				return invalid
			}(), wantError: "shards must not exceed 256"},
		}

		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				dir := t.TempDir()
				inventoryPath := writeTimingPlanValue(t, filepath.Join(dir, "inventory.json"), test.inventory)
				historyPath := writeTimingPlanValue(t, filepath.Join(dir, "history.json"), test.history)
				configPath := writeTimingPlanValue(t, filepath.Join(dir, "config.json"), test.config)
				code, stdout, stderr := runTimingPlanCommand(
					"--inventory", inventoryPath, "--history", historyPath, "--config", configPath)
				if code != 1 || len(stdout) != 0 || !strings.Contains(string(stderr), test.wantError) {
					t.Fatalf("command exit=%d stdout=%q stderr=%q, want exit 1/empty stdout/error containing %q", code, stdout, stderr, test.wantError)
				}
			})
		}
	})

	t.Run("rejects invalid command lines", func(t *testing.T) {
		tests := []struct {
			name      string
			args      []string
			wantError string
		}{
			{name: "missing flags", wantError: "--inventory is required"},
			{name: "repeated flag", args: []string{"--inventory", "a", "--inventory", "b", "--history", "h", "--config", "c"}, wantError: "inventory may only be specified once"},
			{name: "unknown flag", args: []string{"--unknown"}, wantError: "flag provided but not defined"},
			{name: "positional", args: []string{"--inventory", "a", "--history", "h", "--config", "c", "extra"}, wantError: "positional arguments are not supported"},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				code, stdout, stderr := runTimingPlanCommand(test.args...)
				if code != 2 || len(stdout) != 0 || !strings.Contains(string(stderr), test.wantError) {
					t.Fatalf("command exit=%d stdout=%q stderr=%q, want exit 2/empty stdout/error containing %q", code, stdout, stderr, test.wantError)
				}
			})
		}
	})
}

func timingPlanCommandSelector() timingplan.ProfileSelector {
	return timingplan.ProfileSelector{
		Job: "cmd-gc-process", Variant: "linux-default",
		Runner: timingplan.ProfileRunner{
			Label: "blacksmith-32vcpu-ubuntu-2404",
			OS:    "Linux", Arch: "X64", CPUCount: 32,
		},
	}
}

func timingPlanCommandProfile(selector timingplan.ProfileSelector, units []timingsummary.UnitHistory) timingsummary.Profile {
	return timingsummary.Profile{
		Job: selector.Job, Variant: selector.Variant,
		Runner: timingsummary.RunnerProfile{
			Label: selector.Runner.Label, OS: selector.Runner.OS,
			Arch: selector.Runner.Arch, CPUCount: selector.Runner.CPUCount,
		},
		Units: units,
	}
}

func timingPlanCommandUnit(selector timingplan.ProfileSelector, unitID, packageName, testName string, passes int, p50, p75, p95, variance float64) timingsummary.UnitHistory {
	unit := timingsummary.UnitHistory{
		UnitID: unitID, Package: packageName, Test: testName,
		Passes: passes, P75Authoritative: passes >= 5, P95Authoritative: passes >= 20,
		SuccessfulObservations: make([]timingsummary.SuccessfulObservation, passes),
	}
	if passes == 0 {
		return unit
	}
	unit.DurationSecondsP50 = timingPlanFloatPointer(p50)
	unit.DurationSecondsP75 = timingPlanFloatPointer(p75)
	unit.DurationSecondsP95 = timingPlanFloatPointer(p95)
	unit.DurationSecondsPopulationVariance = timingPlanFloatPointer(variance)
	lastSHA := "tested-sha"
	unit.LastSuccessSHA = &lastSHA
	for index := range unit.SuccessfulObservations {
		unit.SuccessfulObservations[index] = timingsummary.SuccessfulObservation{
			ArtifactIdentity: timingsummary.ArtifactIdentity{
				Workflow: "CI", RunID: "run", RunAttempt: "1",
				Job: selector.Job, ShardID: "shard", Variant: selector.Variant,
			},
			TestedSHA: "tested-sha", DurationSeconds: p50,
		}
	}
	return unit
}

func timingPlanFloatPointer(value float64) *float64 {
	return &value
}

func writeTimingPlanInputs(t *testing.T, dir string, inventory timingPlanInventoryDocument, history timingsummary.Snapshot, config timingPlanConfigDocument) ([]string, []string) {
	t.Helper()
	paths := []string{
		writeTimingPlanValue(t, filepath.Join(dir, "inventory.json"), inventory),
		writeTimingPlanValue(t, filepath.Join(dir, "history.json"), history),
		writeTimingPlanValue(t, filepath.Join(dir, "config.json"), config),
	}
	return []string{"--inventory", paths[0], "--history", paths[1], "--config", paths[2]}, paths
}

func writeTimingPlanValue(t *testing.T, path string, value any) string {
	t.Helper()
	var data []byte
	if raw, ok := value.(json.RawMessage); ok {
		data = raw
	} else {
		var err error
		data, err = json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal %s: %v", path, err)
		}
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func readTimingPlanInputs(t *testing.T, paths []string) map[string]string {
	t.Helper()
	values := make(map[string]string, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		values[path] = string(data)
	}
	return values
}

func runTimingPlanCommand(args ...string) (int, []byte, []byte) {
	var stdout, stderr bytes.Buffer
	code := timingplancli.Run(args, &stdout, &stderr)
	return code, stdout.Bytes(), stderr.Bytes()
}

func timingPlanAssignments(result timingplan.Result) map[string]timingplan.Assignment {
	assignments := make(map[string]timingplan.Assignment)
	for _, shard := range result.Shards {
		for _, assignment := range shard.Units {
			assignments[assignment.UnitID] = assignment
		}
	}
	return assignments
}

func sortedTimingPlanKeys(values map[string]timingplan.Assignment) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
