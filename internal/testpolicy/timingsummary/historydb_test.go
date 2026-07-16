package timingsummary

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestUpdateHistoryCreateCanonical(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	databasePath := filepath.Join(t.TempDir(), "timing-history.json")
	envelope := historyRunEnvelope("10", "sha-10", "2026-07-15T10:00:00Z")
	item := historyArtifact(envelope, "cmd-gc-process-1-of-12", []timingUnit{
		{UnitID: "internal/example:TestWarm/child", Kind: "test", Package: "github.com/gastownhall/gascity/internal/example", Test: "TestWarm", Subtest: "child", Outcome: "pass", DurationSeconds: 2},
		{UnitID: "internal/example:TestSkipped", Kind: "test", Package: "github.com/gastownhall/gascity/internal/example", Test: "TestSkipped", Outcome: "skip", DurationSeconds: 0},
		{UnitID: "github.com/gastownhall/gascity/internal/example", Kind: "package", Package: "github.com/gastownhall/gascity/internal/example", Outcome: "pass", DurationSeconds: 9},
		{UnitID: "internal/example:TestWarm", Kind: "test", Package: "github.com/gastownhall/gascity/internal/example", Test: "TestWarm", Outcome: "pass", DurationSeconds: 1.5},
		{UnitID: "internal/example:TestCold", Kind: "test", Package: "github.com/gastownhall/gascity/internal/example", Test: "TestCold", Outcome: "fail", DurationSeconds: 3},
	})
	writeArtifact(t, root, "shuffled.json", item)

	snapshot, err := UpdateHistory(databasePath, envelope, 10, []string{root})
	if err != nil {
		t.Fatalf("UpdateHistory: %v", err)
	}
	got, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatalf("read history database: %v", err)
	}
	const want = `{"schema":1,"runs":[{"schema":1,"repository":"gastownhall/gascity","event":"push","ref":"refs/heads/main","workflow":"CI","run_id":"10","run_attempt":"1","tested_sha":"sha-10","conclusion":"success","completed_at":"2026-07-15T10:00:00Z"}],` +
		`"artifacts":[{"run_index":0,"job":"cmd-gc-process","shard_id":"cmd-gc-process-1-of-12","variant":"linux-default","runner":{"label":"blacksmith-32vcpu","name":"ephemeral-10-cmd-gc-process-1-of-12","os":"Linux","arch":"X64","cpu_count":32}}],` +
		`"units":[{"unit_id":"github.com/gastownhall/gascity/internal/example","kind":"package","package":"github.com/gastownhall/gascity/internal/example","test":"","subtest":"","samples":[{"artifact_index":0,"outcome":"pass","duration_seconds":9}]},` +
		`{"unit_id":"internal/example:TestCold","kind":"test","package":"github.com/gastownhall/gascity/internal/example","test":"TestCold","subtest":"","samples":[{"artifact_index":0,"outcome":"fail","duration_seconds":3}]},` +
		`{"unit_id":"internal/example:TestSkipped","kind":"test","package":"github.com/gastownhall/gascity/internal/example","test":"TestSkipped","subtest":"","samples":[{"artifact_index":0,"outcome":"skip","duration_seconds":0}]},` +
		`{"unit_id":"internal/example:TestWarm","kind":"test","package":"github.com/gastownhall/gascity/internal/example","test":"TestWarm","subtest":"","samples":[{"artifact_index":0,"outcome":"pass","duration_seconds":1.5}]},` +
		`{"unit_id":"internal/example:TestWarm/child","kind":"test","package":"github.com/gastownhall/gascity/internal/example","test":"TestWarm","subtest":"child","samples":[{"artifact_index":0,"outcome":"pass","duration_seconds":2}]}]}` + "\n"
	if string(got) != want {
		t.Fatalf("canonical history database changed\ngot:  %q\nwant: %q", got, want)
	}

	profile := findHistoryProfile(t, snapshot, 32)
	if len(profile.Units) != 3 {
		t.Fatalf("derived snapshot top-level units = %d, want 3: %+v", len(profile.Units), profile.Units)
	}
	warm := findHistoryUnit(t, profile, "internal/example:TestWarm")
	if warm.Passes != 1 || warm.Failures != 0 || warm.Skips != 0 || historyFloat(t, warm.DurationSecondsP95) != 1.5 {
		t.Fatalf("derived warm history = %+v, want one 1.5s pass", warm)
	}
	cold := findHistoryUnit(t, profile, "internal/example:TestCold")
	if cold.Passes != 0 || cold.Failures != 1 || cold.SuccessfulObservations == nil || len(cold.SuccessfulObservations) != 0 {
		t.Fatalf("derived cold history = %+v, want one retained failure and [] successes", cold)
	}
}

func TestUpdateHistoryIsIdempotentForOverlappingPassFailSkipArtifacts(t *testing.T) {
	t.Parallel()

	databasePath := filepath.Join(t.TempDir(), "timing-history.json")
	envelope := historyRunEnvelope("20", "sha-20", "2026-07-15T11:00:00Z")
	initialRoot := t.TempDir()
	firstArtifact := historyArtifact(envelope, "shard-a", []timingUnit{
		{UnitID: "internal/example:TestPass", Kind: "test", Package: "github.com/gastownhall/gascity/internal/example", Test: "TestPass", Outcome: "pass", DurationSeconds: 1},
		{UnitID: "internal/example:TestFail", Kind: "test", Package: "github.com/gastownhall/gascity/internal/example", Test: "TestFail", Outcome: "fail", DurationSeconds: 2},
	})
	secondArtifact := historyArtifact(envelope, "shard-b", []timingUnit{
		{UnitID: "internal/example:TestSkip", Kind: "test", Package: "github.com/gastownhall/gascity/internal/example", Test: "TestSkip", Outcome: "skip", DurationSeconds: 0},
	})
	writeArtifact(t, initialRoot, "b.json", secondArtifact)
	writeArtifact(t, initialRoot, "a.json", firstArtifact)

	firstSnapshot, err := UpdateHistory(databasePath, envelope, 10, []string{initialRoot})
	if err != nil {
		t.Fatalf("first UpdateHistory: %v", err)
	}
	before, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatalf("read first history database: %v", err)
	}

	overlapRoot := t.TempDir()
	writeArtifact(t, overlapRoot, "copies/second.json", secondArtifact)
	writeArtifact(t, overlapRoot, "copies/first.json", firstArtifact)
	secondSnapshot, err := UpdateHistory(databasePath, envelope, 10, []string{overlapRoot})
	if err != nil {
		t.Fatalf("overlapping UpdateHistory: %v", err)
	}
	after, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatalf("read replayed history database: %v", err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("idempotent replay changed database\nbefore: %s\nafter:  %s", before, after)
	}
	if !reflect.DeepEqual(secondSnapshot, firstSnapshot) {
		t.Fatalf("idempotent replay changed derived snapshot\nbefore: %+v\nafter:  %+v", firstSnapshot, secondSnapshot)
	}

	database := decodeHistoryDatabase(t, after)
	if len(database.Runs) != 1 || len(database.Artifacts) != 2 || len(database.Units) != 3 {
		t.Fatalf("normalized replay cardinality = runs %d artifacts %d units %d, want 1/2/3",
			len(database.Runs), len(database.Artifacts), len(database.Units))
	}
	gotOutcomes := make([]string, 0, 3)
	for _, unit := range database.Units {
		if len(unit.Samples) != 1 {
			t.Fatalf("unit %q samples = %d, want exactly one after replay: %+v", unit.UnitID, len(unit.Samples), unit.Samples)
		}
		gotOutcomes = append(gotOutcomes, unit.Samples[0].Outcome)
	}
	sort.Strings(gotOutcomes)
	if !reflect.DeepEqual(gotOutcomes, []string{"fail", "pass", "skip"}) {
		t.Fatalf("retained replay outcomes = %q, want one pass/fail/skip", gotOutcomes)
	}
	if twoSnapshots := 2 * len(mustJSON(t, firstSnapshot)); len(after) >= twoSnapshots {
		t.Fatalf("normalized database size = %d, want smaller than two concatenated %d-byte snapshots", len(after), len(mustJSON(t, firstSnapshot)))
	}
}

func TestUpdateHistoryPreservesDuplicateRowsWithinOneArtifact(t *testing.T) {
	t.Parallel()

	databasePath := filepath.Join(t.TempDir(), "timing-history.json")
	envelope := historyRunEnvelope("25", "sha-25", "2026-07-15T11:30:00Z")
	duplicate := timingUnit{
		UnitID: "internal/example:TestRepeated", Kind: "test",
		Package: "github.com/gastownhall/gascity/internal/example", Test: "TestRepeated",
		Outcome: "pass", DurationSeconds: 1.25,
	}
	root := t.TempDir()
	writeArtifact(t, root, "run.json", historyArtifact(envelope, "shard-a", []timingUnit{duplicate, duplicate}))

	firstSnapshot, err := UpdateHistory(databasePath, envelope, 10, []string{root})
	if err != nil {
		t.Fatalf("first UpdateHistory: %v", err)
	}
	before, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatalf("read first history database: %v", err)
	}
	database := decodeHistoryDatabase(t, before)
	if len(database.Units) != 1 || len(database.Units[0].Samples) != 2 {
		t.Fatalf("stored duplicate-row samples = %+v, want two samples", database.Units)
	}
	unit := findHistoryUnit(t, findHistoryProfile(t, firstSnapshot, 32), duplicate.UnitID)
	if unit.Passes != 2 || len(unit.SuccessfulObservations) != 2 {
		t.Fatalf("derived duplicate-row history = %+v, want two successful observations", unit)
	}

	secondSnapshot, err := UpdateHistory(databasePath, envelope, 10, []string{root})
	if err != nil {
		t.Fatalf("replay UpdateHistory: %v", err)
	}
	after, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatalf("read replayed history database: %v", err)
	}
	if !reflect.DeepEqual(after, before) || !reflect.DeepEqual(secondSnapshot, firstSnapshot) {
		t.Fatalf("duplicate-row replay changed retained evidence\nbefore: %s\nafter:  %s", before, after)
	}
}

func TestUpdateHistoryRejectsConflictsAndEnvelopeMismatchesAtomically(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		mutate       func(RunEnvelope, timingArtifactFixture) (RunEnvelope, timingArtifactFixture)
		wantEvidence string
	}{
		{
			name: "stored artifact conflict",
			mutate: func(envelope RunEnvelope, item timingArtifactFixture) (RunEnvelope, timingArtifactFixture) {
				item.Units[0].DurationSeconds = 99
				return envelope, item
			},
			wantEvidence: "conflicting artifact",
		},
		{
			name: "workflow mismatch",
			mutate: func(envelope RunEnvelope, item timingArtifactFixture) (RunEnvelope, timingArtifactFixture) {
				envelope.Workflow = "Other CI"
				return envelope, item
			},
			wantEvidence: "workflow",
		},
		{
			name: "run ID mismatch",
			mutate: func(envelope RunEnvelope, item timingArtifactFixture) (RunEnvelope, timingArtifactFixture) {
				envelope.RunID = "different-run"
				return envelope, item
			},
			wantEvidence: "run_id",
		},
		{
			name: "run attempt mismatch",
			mutate: func(envelope RunEnvelope, item timingArtifactFixture) (RunEnvelope, timingArtifactFixture) {
				envelope.RunAttempt = "2"
				return envelope, item
			},
			wantEvidence: "run_attempt",
		},
		{
			name: "tested SHA mismatch",
			mutate: func(envelope RunEnvelope, item timingArtifactFixture) (RunEnvelope, timingArtifactFixture) {
				envelope.TestedSHA = "different-sha"
				return envelope, item
			},
			wantEvidence: "tested_sha",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			databasePath := filepath.Join(t.TempDir(), "timing-history.json")
			envelope := historyRunEnvelope("30", "sha-30", "2026-07-15T12:00:00Z")
			item := historyArtifact(envelope, "shard-a", []timingUnit{{
				UnitID: "internal/example:TestAtomic", Kind: "test",
				Package: "github.com/gastownhall/gascity/internal/example", Test: "TestAtomic",
				Outcome: "pass", DurationSeconds: 3,
			}})
			seedRoot := t.TempDir()
			writeArtifact(t, seedRoot, "seed.json", item)
			if _, err := UpdateHistory(databasePath, envelope, 10, []string{seedRoot}); err != nil {
				t.Fatalf("seed UpdateHistory: %v", err)
			}
			before, err := os.ReadFile(databasePath)
			if err != nil {
				t.Fatalf("read seeded database: %v", err)
			}

			mutatedEnvelope, mutatedArtifact := tc.mutate(envelope, item)
			badRoot := t.TempDir()
			writeArtifact(t, badRoot, "bad.json", mutatedArtifact)
			_, err = UpdateHistory(databasePath, mutatedEnvelope, 10, []string{badRoot})
			if err == nil || !strings.Contains(err.Error(), tc.wantEvidence) {
				t.Fatalf("UpdateHistory error = %v, want contextual evidence %q", err, tc.wantEvidence)
			}
			after, readErr := os.ReadFile(databasePath)
			if readErr != nil {
				t.Fatalf("read database after rejected update: %v", readErr)
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("rejected update changed database\nbefore: %s\nafter:  %s", before, after)
			}
		})
	}
}

func TestUpdateHistoryRejectsCrossRepositoryUpdateAtomically(t *testing.T) {
	t.Parallel()

	databasePath := filepath.Join(t.TempDir(), "timing-history.json")
	envelope := historyRunEnvelope("35", "sha-35", "2026-07-15T12:30:00Z")
	item := historyArtifact(envelope, "shard-a", []timingUnit{{
		UnitID: "internal/example:TestRepository", Kind: "test",
		Package: "github.com/gastownhall/gascity/internal/example", Test: "TestRepository",
		Outcome: "pass", DurationSeconds: 1,
	}})
	seedRoot := t.TempDir()
	writeArtifact(t, seedRoot, "seed.json", item)
	if _, err := UpdateHistory(databasePath, envelope, 10, []string{seedRoot}); err != nil {
		t.Fatalf("seed UpdateHistory: %v", err)
	}
	before, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatalf("read seeded database: %v", err)
	}

	otherRepository := envelope
	otherRepository.Repository = "example/other"
	_, err = UpdateHistory(databasePath, otherRepository, 10, []string{seedRoot})
	if err == nil || !strings.Contains(err.Error(), "does not match run envelope repository") {
		t.Fatalf("cross-repository UpdateHistory error = %v, want repository mismatch", err)
	}
	after, readErr := os.ReadFile(databasePath)
	if readErr != nil {
		t.Fatalf("read database after rejected update: %v", readErr)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("cross-repository rejection changed database\nbefore: %s\nafter:  %s", before, after)
	}
}

func TestUpdateHistoryRejectsStoredEnvelopeMetadataChangesAtomically(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		mutate func(*RunEnvelope)
	}{
		{name: "event", mutate: func(envelope *RunEnvelope) { envelope.Event = "workflow_dispatch" }},
		{name: "ref", mutate: func(envelope *RunEnvelope) { envelope.Ref = "refs/heads/release" }},
		{name: "tested SHA", mutate: func(envelope *RunEnvelope) { envelope.TestedSHA = "different-sha" }},
		{name: "conclusion", mutate: func(envelope *RunEnvelope) { envelope.Conclusion = "failure" }},
		{name: "completion time", mutate: func(envelope *RunEnvelope) { envelope.CompletedAt = "2026-07-15T12:59:00Z" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			databasePath := filepath.Join(t.TempDir(), "timing-history.json")
			envelope := historyRunEnvelope("36", "sha-36", "2026-07-15T12:45:00Z")
			seedRoot := t.TempDir()
			writeArtifact(t, seedRoot, "seed.json", historyArtifact(envelope, "shard-a", []timingUnit{{
				UnitID: "internal/example:TestEnvelope", Kind: "test",
				Package: "github.com/gastownhall/gascity/internal/example", Test: "TestEnvelope",
				Outcome: "pass", DurationSeconds: 1,
			}}))
			if _, err := UpdateHistory(databasePath, envelope, 10, []string{seedRoot}); err != nil {
				t.Fatalf("seed UpdateHistory: %v", err)
			}
			before, err := os.ReadFile(databasePath)
			if err != nil {
				t.Fatalf("read seeded database: %v", err)
			}

			changed := envelope
			tc.mutate(&changed)
			incomingRoot := t.TempDir()
			writeArtifact(t, incomingRoot, "changed.json", historyArtifact(changed, "shard-a", []timingUnit{{
				UnitID: "internal/example:TestEnvelope", Kind: "test",
				Package: "github.com/gastownhall/gascity/internal/example", Test: "TestEnvelope",
				Outcome: "pass", DurationSeconds: 1,
			}}))
			_, err = UpdateHistory(databasePath, changed, 10, []string{incomingRoot})
			if err == nil || !strings.Contains(err.Error(), "conflicting run envelope") {
				t.Fatalf("changed-envelope UpdateHistory error = %v, want stored-envelope conflict", err)
			}
			after, readErr := os.ReadFile(databasePath)
			if readErr != nil {
				t.Fatalf("read database after rejected update: %v", readErr)
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("changed-envelope rejection changed database\nbefore: %s\nafter:  %s", before, after)
			}
		})
	}
}

func TestUpdateHistoryRejectsInvalidStoredDatabaseWithoutRewriting(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name         string
		mutate       func(*testing.T, []byte) []byte
		wantEvidence string
	}{
		{
			name: "unknown field",
			mutate: func(t *testing.T, data []byte) []byte {
				t.Helper()
				return []byte(strings.Replace(string(data), `"schema":1`, `"schema":1,"unexpected":true`, 1))
			},
			wantEvidence: "unknown field",
		},
		{
			name: "trailing JSON value",
			mutate: func(t *testing.T, data []byte) []byte {
				t.Helper()
				return append(append([]byte(nil), data...), []byte("{}\n")...)
			},
			wantEvidence: "decode history database schema",
		},
		{
			name: "invalid artifact reference",
			mutate: func(t *testing.T, data []byte) []byte {
				t.Helper()
				database := decodeHistoryDatabase(t, data)
				database.Units[0].Samples[0].ArtifactIndex = len(database.Artifacts)
				return mustJSON(t, database)
			},
			wantEvidence: "artifact_index",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			databasePath := filepath.Join(t.TempDir(), "timing-history.json")
			envelope := historyRunEnvelope("37", "sha-37", "2026-07-15T12:50:00Z")
			root := t.TempDir()
			writeArtifact(t, root, "run.json", historyArtifact(envelope, "shard-a", []timingUnit{{
				UnitID: "internal/example:TestStoredDatabase", Kind: "test",
				Package: "github.com/gastownhall/gascity/internal/example", Test: "TestStoredDatabase",
				Outcome: "pass", DurationSeconds: 1,
			}}))
			if _, err := UpdateHistory(databasePath, envelope, 10, []string{root}); err != nil {
				t.Fatalf("seed UpdateHistory: %v", err)
			}
			valid, err := os.ReadFile(databasePath)
			if err != nil {
				t.Fatalf("read seeded database: %v", err)
			}
			invalid := tc.mutate(t, valid)
			if err := os.WriteFile(databasePath, invalid, 0o600); err != nil {
				t.Fatalf("write invalid history database: %v", err)
			}

			_, err = UpdateHistory(databasePath, envelope, 10, []string{root})
			if err == nil || !strings.Contains(err.Error(), tc.wantEvidence) {
				t.Fatalf("UpdateHistory error = %v, want evidence %q", err, tc.wantEvidence)
			}
			after, readErr := os.ReadFile(databasePath)
			if readErr != nil {
				t.Fatalf("read rejected database: %v", readErr)
			}
			if !reflect.DeepEqual(after, invalid) {
				t.Fatalf("invalid stored database was rewritten\nbefore: %s\nafter:  %s", invalid, after)
			}
		})
	}
}

func TestUpdateHistoryPrunesWholeCohortsByCompletedAtNotLexicalRunID(t *testing.T) {
	t.Parallel()

	databasePath := filepath.Join(t.TempDir(), "timing-history.json")
	runs := []RunEnvelope{
		historyRunEnvelope("999", "sha-oldest", "2026-07-15T01:00:00Z"),
		historyRunEnvelope("100", "sha-middle", "2026-07-15T02:00:00Z"),
		historyRunEnvelope("010", "sha-newest", "2026-07-15T03:00:00Z"),
	}
	var snapshot Snapshot
	for _, envelope := range runs {
		root := t.TempDir()
		writeArtifact(t, root, "z.json", historyArtifact(envelope, "shard-z", []timingUnit{{
			UnitID: "internal/example:TestZ", Kind: "test", Package: "github.com/gastownhall/gascity/internal/example",
			Test: "TestZ", Outcome: "pass", DurationSeconds: 2,
		}}))
		writeArtifact(t, root, "a.json", historyArtifact(envelope, "shard-a", []timingUnit{{
			UnitID: "internal/example:TestA", Kind: "test", Package: "github.com/gastownhall/gascity/internal/example",
			Test: "TestA", Outcome: "fail", DurationSeconds: 1,
		}}))
		var err error
		snapshot, err = UpdateHistory(databasePath, envelope, 2, []string{root})
		if err != nil {
			t.Fatalf("UpdateHistory run %q: %v", envelope.RunID, err)
		}
	}

	data, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatalf("read retained database: %v", err)
	}
	database := decodeHistoryDatabase(t, data)
	gotRunIDs := make([]string, 0, len(database.Runs))
	for _, run := range database.Runs {
		gotRunIDs = append(gotRunIDs, run.RunID)
	}
	sort.Strings(gotRunIDs)
	if !reflect.DeepEqual(gotRunIDs, []string{"010", "100"}) {
		t.Fatalf("retained run IDs = %q, want middle and newest completion cohorts [010 100]", gotRunIDs)
	}

	artifactsPerRun := make(map[string]int)
	for index, artifact := range database.Artifacts {
		if artifact.RunIndex < 0 || artifact.RunIndex >= len(database.Runs) {
			t.Fatalf("artifact %d has orphan run index %d for %d runs", index, artifact.RunIndex, len(database.Runs))
		}
		artifactsPerRun[database.Runs[artifact.RunIndex].RunID]++
	}
	if artifactsPerRun["010"] != 2 || artifactsPerRun["100"] != 2 || len(artifactsPerRun) != 2 {
		t.Fatalf("retained whole-cohort artifact counts = %v, want two artifacts for each retained run", artifactsPerRun)
	}
	for _, unit := range database.Units {
		if len(unit.Samples) != 2 {
			t.Fatalf("retained unit %q samples = %d, want one per retained cohort: %+v", unit.UnitID, len(unit.Samples), unit.Samples)
		}
		for _, sample := range unit.Samples {
			if sample.ArtifactIndex < 0 || sample.ArtifactIndex >= len(database.Artifacts) {
				t.Fatalf("unit %q has orphan artifact index %d for %d artifacts", unit.UnitID, sample.ArtifactIndex, len(database.Artifacts))
			}
		}
	}
	unit := findHistoryUnit(t, findHistoryProfile(t, snapshot, 32), "internal/example:TestZ")
	if unit.Passes != 2 {
		t.Fatalf("derived retained passes = %d, want two whole retained cohorts: %+v", unit.Passes, unit)
	}
}

func TestUpdateHistoryRetainsColdUnitAbsentFromNewestCohort(t *testing.T) {
	t.Parallel()

	databasePath := filepath.Join(t.TempDir(), "timing-history.json")
	runs := []struct {
		envelope RunEnvelope
		cold     bool
	}{
		{envelope: historyRunEnvelope("700", "sha-oldest", "2026-07-15T04:00:00Z"), cold: true},
		{envelope: historyRunEnvelope("200", "sha-middle", "2026-07-15T05:00:00Z"), cold: true},
		{envelope: historyRunEnvelope("001", "sha-newest", "2026-07-15T06:00:00Z"), cold: false},
	}
	var snapshot Snapshot
	for _, run := range runs {
		units := []timingUnit{{
			UnitID: "internal/example:TestHot", Kind: "test", Package: "github.com/gastownhall/gascity/internal/example",
			Test: "TestHot", Outcome: "pass", DurationSeconds: 1,
		}}
		if run.cold {
			units = append(units, timingUnit{
				UnitID: "internal/example:TestCold", Kind: "test", Package: "github.com/gastownhall/gascity/internal/example",
				Test: "TestCold", Outcome: "pass", DurationSeconds: 5,
			})
		}
		root := t.TempDir()
		writeArtifact(t, root, "run.json", historyArtifact(run.envelope, "shard-a", units))
		var err error
		snapshot, err = UpdateHistory(databasePath, run.envelope, 2, []string{root})
		if err != nil {
			t.Fatalf("UpdateHistory run %q: %v", run.envelope.RunID, err)
		}
	}

	cold := findHistoryUnit(t, findHistoryProfile(t, snapshot, 32), "internal/example:TestCold")
	if cold.Passes != 1 || len(cold.SuccessfulObservations) != 1 || cold.LastSuccessSHA == nil || *cold.LastSuccessSHA != "sha-middle" {
		t.Fatalf("cold retained unit = %+v, want the middle cohort's one success despite absence from newest", cold)
	}
}

func TestUpdateHistoryRetainsAndRecomputesFiveAndTwentySampleAuthority(t *testing.T) {
	t.Parallel()

	databasePath := filepath.Join(t.TempDir(), "timing-history.json")
	var snapshot Snapshot
	for run := 1; run <= 20; run++ {
		envelope := historyRunEnvelope(fmt.Sprintf("%03d", run), fmt.Sprintf("sha-%02d", run), fmt.Sprintf("2026-07-15T00:%02d:00Z", run))
		units := []timingUnit{{
			UnitID: "internal/example:TestP95", Kind: "test", Package: "github.com/gastownhall/gascity/internal/example",
			Test: "TestP95", Outcome: "pass", DurationSeconds: float64(run),
		}}
		if run <= 5 {
			units = append(units, timingUnit{
				UnitID: "internal/example:TestP75", Kind: "test", Package: "github.com/gastownhall/gascity/internal/example",
				Test: "TestP75", Outcome: "pass", DurationSeconds: float64(run),
			})
		}
		root := t.TempDir()
		writeArtifact(t, root, "run.json", historyArtifact(envelope, "shard-a", units))
		var err error
		snapshot, err = UpdateHistory(databasePath, envelope, 20, []string{root})
		if err != nil {
			t.Fatalf("UpdateHistory run %d: %v", run, err)
		}
	}

	profile := findHistoryProfile(t, snapshot, 32)
	p75 := findHistoryUnit(t, profile, "internal/example:TestP75")
	p95 := findHistoryUnit(t, profile, "internal/example:TestP95")
	if p75.Passes != 5 || !p75.P75Authoritative || p75.P95Authoritative {
		t.Fatalf("five-sample authority before pruning = %+v, want p75 only", p75)
	}
	if p95.Passes != 20 || !p95.P75Authoritative || !p95.P95Authoritative {
		t.Fatalf("twenty-sample authority before pruning = %+v, want p75 and p95", p95)
	}

	newest := historyRunEnvelope("999-new", "sha-new", "2026-07-15T00:21:30Z")
	root := t.TempDir()
	writeArtifact(t, root, "run.json", historyArtifact(newest, "shard-a", []timingUnit{{
		UnitID: "internal/example:TestNewest", Kind: "test", Package: "github.com/gastownhall/gascity/internal/example",
		Test: "TestNewest", Outcome: "pass", DurationSeconds: 1,
	}}))
	var err error
	snapshot, err = UpdateHistory(databasePath, newest, 20, []string{root})
	if err != nil {
		t.Fatalf("UpdateHistory pruning threshold cohort: %v", err)
	}

	profile = findHistoryProfile(t, snapshot, 32)
	p75 = findHistoryUnit(t, profile, "internal/example:TestP75")
	p95 = findHistoryUnit(t, profile, "internal/example:TestP95")
	if p75.Passes != 4 || p75.P75Authoritative || p75.P95Authoritative {
		t.Fatalf("five-sample authority after oldest-cohort pruning = %+v, want four cold samples and no authority", p75)
	}
	if p95.Passes != 19 || !p95.P75Authoritative || p95.P95Authoritative {
		t.Fatalf("twenty-sample authority after oldest-cohort pruning = %+v, want 19 samples and p75 only", p95)
	}
}

func historyRunEnvelope(runID, testedSHA, completedAt string) RunEnvelope {
	return RunEnvelope{
		Schema: 1, Repository: "gastownhall/gascity", Event: "push", Ref: "refs/heads/main",
		Workflow: "CI", RunID: runID, RunAttempt: "1", TestedSHA: testedSHA,
		Conclusion: "success", CompletedAt: completedAt,
	}
}

func historyArtifact(envelope RunEnvelope, shardID string, units []timingUnit) timingArtifactFixture {
	item := timingArtifact(envelope.RunID, defaultRunner("ephemeral-"+envelope.RunID+"-"+shardID), units)
	item.Workflow = envelope.Workflow
	item.RunAttempt = envelope.RunAttempt
	item.CommitSHA = envelope.TestedSHA
	item.ShardID = shardID
	return item
}

type historyDatabaseWire struct {
	Schema    int                   `json:"schema"`
	Runs      []historyRunWire      `json:"runs"`
	Artifacts []historyArtifactWire `json:"artifacts"`
	Units     []historyUnitWire     `json:"units"`
}

type historyRunWire struct {
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

type historyArtifactWire struct {
	RunIndex int               `json:"run_index"`
	Job      string            `json:"job"`
	ShardID  string            `json:"shard_id"`
	Variant  string            `json:"variant"`
	Runner   historyRunnerWire `json:"runner"`
}

type historyRunnerWire struct {
	Label    string `json:"label"`
	Name     string `json:"name"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	CPUCount int    `json:"cpu_count"`
}

type historyUnitWire struct {
	UnitID  string              `json:"unit_id"`
	Kind    string              `json:"kind"`
	Package string              `json:"package"`
	Test    string              `json:"test"`
	Subtest string              `json:"subtest"`
	Samples []historySampleWire `json:"samples"`
}

type historySampleWire struct {
	ArtifactIndex   int     `json:"artifact_index"`
	Outcome         string  `json:"outcome"`
	DurationSeconds float64 `json:"duration_seconds"`
}

func decodeHistoryDatabase(t *testing.T, data []byte) historyDatabaseWire {
	t.Helper()
	var database historyDatabaseWire
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&database); err != nil {
		t.Fatalf("decode history database: %v\n%s", err, data)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		t.Fatalf("decode history database trailing data: %v\n%s", err, data)
	}
	return database
}
