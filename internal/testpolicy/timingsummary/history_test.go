package timingsummary

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestBuildSnapshotJSONIsDeterministicAndRetainsSuccessfulObservations(t *testing.T) {
	t.Parallel()

	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	const unitID = "internal/example:TestHistory"

	artifactFor := func(runID, testedSHA, outcome string, duration float64) timingArtifactFixture {
		item := timingArtifact(runID, defaultRunner("ephemeral-"+runID), []timingUnit{{
			UnitID: unitID, Kind: "test", Package: "github.com/gastownhall/gascity/internal/example",
			Test: "TestHistory", Outcome: outcome, DurationSeconds: duration,
		}})
		item.CommitSHA = testedSHA
		return item
	}

	oldest := artifactFor("100", "sha-oldest", "pass", 1)
	writeArtifact(t, firstRoot, "z-oldest.json", oldest)
	writeArtifact(t, secondRoot, "duplicate/oldest.json", oldest)
	writeArtifact(t, secondRoot, "middle-1.json", artifactFor("200", "sha-middle-1", "pass", 2))
	writeArtifact(t, firstRoot, "middle-2.json", artifactFor("250", "sha-middle-2", "pass", 3))
	writeArtifact(t, secondRoot, "outlier.json", artifactFor("300", "sha-outlier", "pass", 900))
	writeArtifact(t, firstRoot, "a-newest-success.json", artifactFor("400", "sha-newest-success", "pass", 4))
	laterFailure := artifactFor("500", "sha-failure", "fail", 5)
	laterFailure.Units = append(laterFailure.Units, timingUnit{
		UnitID: "internal/example:TestOnlyFails", Kind: "test",
		Package: "github.com/gastownhall/gascity/internal/example", Test: "TestOnlyFails",
		Outcome: "fail", DurationSeconds: 6,
	})
	writeArtifact(t, firstRoot, "later-failure.json", laterFailure)
	laterSkip := artifactFor("600", "sha-skip", "skip", 0)
	laterSkip.Units = append(laterSkip.Units, timingUnit{
		UnitID: "internal/example:TestOnlyFails", Kind: "test",
		Package: "github.com/gastownhall/gascity/internal/example", Test: "TestOnlyFails",
		Outcome: "skip", DurationSeconds: 0,
	})
	writeArtifact(t, secondRoot, "later-skip.json", laterSkip)

	otherProfile := artifactFor("700", "sha-other-profile", "pass", 10)
	otherProfile.Runner = timingRunner{
		Label: "blacksmith-64vcpu", Name: "ephemeral-other", OS: "Linux", Arch: "X64", CPUCount: 64,
	}
	writeArtifact(t, secondRoot, "other-profile.json", otherProfile)

	forwardOutput := runJSONHistory(t, firstRoot, secondRoot)
	reverseOutput := runJSONHistory(t, secondRoot, firstRoot)
	if forwardOutput != reverseOutput {
		t.Fatalf("JSON history depends on artifact-root order\nforward:\n%s\nreverse:\n%s", forwardOutput, reverseOutput)
	}

	snapshot := decodeHistorySnapshot(t, forwardOutput)
	if snapshot.Schema != 1 {
		t.Fatalf("snapshot schema = %d, want 1", snapshot.Schema)
	}
	if snapshot.UniqueArtifactCount != 8 || snapshot.DuplicateArtifactCount != 1 {
		t.Fatalf("snapshot artifact counts = unique %d duplicate %d, want 8/1",
			snapshot.UniqueArtifactCount, snapshot.DuplicateArtifactCount)
	}
	if len(snapshot.Profiles) != 2 {
		t.Fatalf("snapshot profiles = %d, want 2: %+v", len(snapshot.Profiles), snapshot.Profiles)
	}

	profile32 := findHistoryProfile(t, snapshot, 32)
	if profile32.Job != "cmd-gc-process" || profile32.Variant != "linux-default" ||
		profile32.Runner != (RunnerProfile{Label: "blacksmith-32vcpu", OS: "Linux", Arch: "X64", CPUCount: 32}) {
		t.Fatalf("32-CPU profile identity = %+v, want canonical cmd/gc Linux profile", profile32)
	}
	if len(profile32.Units) != 2 {
		t.Fatalf("32-CPU profile units = %d, want 2: %+v", len(profile32.Units), profile32.Units)
	}
	unit := findHistoryUnit(t, profile32, unitID)
	if unit.Package != "github.com/gastownhall/gascity/internal/example" || unit.Test != "TestHistory" || unit.Subtest != "" {
		t.Fatalf("history unit identity = %+v", unit)
	}
	if unit.Passes != 5 || unit.Failures != 1 || unit.Skips != 1 {
		t.Fatalf("history outcomes = pass %d fail %d skip %d, want 5/1/1", unit.Passes, unit.Failures, unit.Skips)
	}
	if historyFloat(t, unit.DurationSecondsP50) != 3 || historyFloat(t, unit.DurationSecondsP75) != 4 || historyFloat(t, unit.DurationSecondsP95) != 900 {
		t.Fatalf("history percentiles = p50 %v p75 %v p95 %v, want 3/4/900", unit.DurationSecondsP50, unit.DurationSecondsP75, unit.DurationSecondsP95)
	}
	if got := historyFloat(t, unit.DurationSecondsPopulationVariance); math.Abs(got-128882) > 1e-9 {
		t.Fatalf("history population variance = %.6f, want 128882", got)
	}
	if !unit.P75Authoritative || unit.P95Authoritative {
		t.Fatalf("history authority = p75 %t p95 %t, want true/false", unit.P75Authoritative, unit.P95Authoritative)
	}
	if unit.LastSuccessSHA == nil || *unit.LastSuccessSHA != "sha-newest-success" {
		t.Fatalf("last success SHA = %v, want canonical final success %q", unit.LastSuccessSHA, "sha-newest-success")
	}
	if len(unit.SuccessfulObservations) != 5 {
		t.Fatalf("successful history observations = %d, want 5 after identical duplicate deduplication: %+v", len(unit.SuccessfulObservations), unit.SuccessfulObservations)
	}

	wantRunIDs := []string{"100", "200", "250", "300", "400"}
	wantSHAs := []string{"sha-oldest", "sha-middle-1", "sha-middle-2", "sha-outlier", "sha-newest-success"}
	gotRunIDs := make([]string, 0, len(unit.SuccessfulObservations))
	for index, observation := range unit.SuccessfulObservations {
		gotRunIDs = append(gotRunIDs, observation.ArtifactIdentity.RunID)
		wantIdentity := ArtifactIdentity{
			Workflow: "CI", RunID: wantRunIDs[index], RunAttempt: "1", Job: "cmd-gc-process",
			ShardID: "cmd-gc-process-1-of-12", Variant: "linux-default",
		}
		if observation.ArtifactIdentity != wantIdentity || observation.TestedSHA != wantSHAs[index] {
			t.Fatalf("successful observation %d = %+v, want identity %+v and SHA %q",
				index, observation, wantIdentity, wantSHAs[index])
		}
	}
	if !reflect.DeepEqual(gotRunIDs, wantRunIDs) {
		t.Fatalf("observation run order = %v, want stable artifact-identity order %v", gotRunIDs, wantRunIDs)
	}

	if got := unit.SuccessfulObservations[3].DurationSeconds; got != 900 {
		t.Fatalf("outlier observation duration = %.3f, want retained 900", got)
	}

	onlyFails := findHistoryUnit(t, profile32, "internal/example:TestOnlyFails")
	if onlyFails.Passes != 0 || onlyFails.Failures != 1 || onlyFails.Skips != 1 ||
		onlyFails.DurationSecondsP50 != nil || onlyFails.DurationSecondsP75 != nil ||
		onlyFails.DurationSecondsP95 != nil || onlyFails.DurationSecondsPopulationVariance != nil ||
		onlyFails.P75Authoritative || onlyFails.P95Authoritative || onlyFails.LastSuccessSHA != nil ||
		onlyFails.SuccessfulObservations == nil || len(onlyFails.SuccessfulObservations) != 0 {
		t.Fatalf("zero-success unit must retain outcomes with null statistics and [] observations: %+v", onlyFails)
	}

	profile64 := findHistoryProfile(t, snapshot, 64)
	otherUnit := findHistoryUnit(t, profile64, unitID)
	if len(profile64.Units) != 1 || otherUnit.Passes != 1 || len(otherUnit.SuccessfulObservations) != 1 || historyFloat(t, otherUnit.DurationSecondsP95) != 10 {
		t.Fatalf("64-CPU profile was mixed with 32-CPU history: %+v", otherUnit)
	}
}

func TestBuildSnapshotJSONMarksPercentileAuthorityAtSampleThresholds(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	tests := []struct {
		name      string
		successes int
	}{
		{name: "TestP75Cold", successes: 4},
		{name: "TestP75Warm", successes: 5},
		{name: "TestP95Cold", successes: 19},
		{name: "TestP95Warm", successes: 20},
	}
	for run := 0; run < 20; run++ {
		units := make([]timingUnit, 0, len(tests))
		for _, tc := range tests {
			if run >= tc.successes {
				continue
			}
			units = append(units, timingUnit{
				UnitID: "internal/example:" + tc.name, Kind: "test",
				Package: "github.com/gastownhall/gascity/internal/example", Test: tc.name,
				Outcome: "pass", DurationSeconds: float64(run + 1),
			})
		}
		item := timingArtifact(fmt.Sprintf("%03d", 800+run), defaultRunner(fmt.Sprintf("ephemeral-%02d", run)), units)
		item.CommitSHA = fmt.Sprintf("sha-%02d", run)
		writeArtifact(t, root, fmt.Sprintf("run-%02d.json", 19-run), item)
	}

	snapshot := decodeHistorySnapshot(t, runJSONHistory(t, root))
	profile := findHistoryProfile(t, snapshot, 32)
	assertAuthority := func(name string, passes int, p75, p95 bool) UnitHistory {
		t.Helper()
		unit := findHistoryUnit(t, profile, "internal/example:"+name)
		if unit.Passes != passes || unit.P75Authoritative != p75 || unit.P95Authoritative != p95 {
			t.Fatalf("%s = passes %d p75-authoritative %t p95-authoritative %t, want %d/%t/%t",
				name, unit.Passes, unit.P75Authoritative, unit.P95Authoritative, passes, p75, p95)
		}
		return unit
	}

	assertAuthority("TestP75Cold", 4, false, false)
	p75Warm := assertAuthority("TestP75Warm", 5, true, false)
	assertAuthority("TestP95Cold", 19, true, false)
	p95Warm := assertAuthority("TestP95Warm", 20, true, true)
	if got := historyFloat(t, p75Warm.DurationSecondsP75); got != 4 {
		t.Fatalf("five-sample p75 = %.3f, want nearest-rank 4", got)
	}
	if got := historyFloat(t, p95Warm.DurationSecondsP95); got != 19 {
		t.Fatalf("twenty-sample p95 = %.3f, want nearest-rank 19", got)
	}
}

func TestBuildSnapshotJSONOrdersOpaqueRunIDsLexically(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	runIDs := []string{"2", "10", "002"}
	for index, runID := range runIDs {
		item := timingArtifact(runID, defaultRunner("ephemeral-"+runID), []timingUnit{{
			UnitID: "internal/example:TestOpaqueID", Kind: "test",
			Package: "github.com/gastownhall/gascity/internal/example", Test: "TestOpaqueID",
			Outcome: "pass", DurationSeconds: float64(index + 1),
		}})
		item.CommitSHA = "sha-" + runID
		writeArtifact(t, root, fmt.Sprintf("input-%d.json", index), item)
	}

	snapshot := decodeHistorySnapshot(t, runJSONHistory(t, root))
	unit := findHistoryUnit(t, findHistoryProfile(t, snapshot, 32), "internal/example:TestOpaqueID")
	gotRunIDs := make([]string, 0, len(unit.SuccessfulObservations))
	for _, observation := range unit.SuccessfulObservations {
		gotRunIDs = append(gotRunIDs, observation.ArtifactIdentity.RunID)
	}
	wantRunIDs := []string{"002", "10", "2"}
	if !reflect.DeepEqual(gotRunIDs, wantRunIDs) {
		t.Fatalf("opaque run ID order = %q, want raw lexical order %q", gotRunIDs, wantRunIDs)
	}
	if unit.LastSuccessSHA == nil || *unit.LastSuccessSHA != "sha-2" {
		t.Fatalf("last success SHA = %v, want final raw-lexical observation sha-2", unit.LastSuccessSHA)
	}
}

func TestBuildSnapshotJSONRejectsConflictingDuplicate(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	valid := timingArtifact("900", defaultRunner("ephemeral"), []timingUnit{{
		UnitID: "internal/example:TestConflict", Kind: "test",
		Package: "github.com/gastownhall/gascity/internal/example", Test: "TestConflict",
		Outcome: "pass", DurationSeconds: 1,
	}})
	writeArtifact(t, root, "first.json", valid)
	writeArtifact(t, root, "second.json", withFirstDuration(valid, 2))

	stdout, stderr, exitCode := runSummary("--format=json", root)
	if exitCode != 1 || stdout != "" || !strings.Contains(stderr, "conflicting duplicate artifact") {
		t.Fatalf("Run conflicting JSON history = stdout %q stderr %q exit %d", stdout, stderr, exitCode)
	}
}

func TestBuildSnapshotJSONRejectsConflictingStableUnitIdentity(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	first := timingArtifact("910", defaultRunner("ephemeral-a"), []timingUnit{{
		UnitID: "internal/example:TestStable", Kind: "test", Package: "github.com/gastownhall/gascity/internal/example",
		Test: "TestStable", Outcome: "pass", DurationSeconds: 1,
	}})
	second := timingArtifact("911", defaultRunner("ephemeral-b"), []timingUnit{{
		UnitID: "internal/example:TestStable", Kind: "test", Package: "github.com/gastownhall/gascity/internal/other",
		Test: "TestStable", Outcome: "pass", DurationSeconds: 2,
	}})
	second.Runner.Label = "blacksmith-64vcpu"
	second.Runner.CPUCount = 64
	writeArtifact(t, root, "first.json", first)
	writeArtifact(t, root, "second.json", second)

	stdout, stderr, exitCode := runSummary("--format=json", root)
	if exitCode != 1 || stdout != "" || !strings.Contains(stderr, "conflicting identity for unit") {
		t.Fatalf("Run conflicting unit identity = stdout %q stderr %q exit %d", stdout, stderr, exitCode)
	}
}

func TestBuildSnapshotJSONPreservesHostileMetadataAsData(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	item := timingArtifact("920", defaultRunner("ephemeral"), []timingUnit{{
		UnitID: "internal/example:TestJSON\"\\\n", Kind: "test",
		Package: "github.com/gastownhall/gascity/internal/example|quoted", Test: "TestJSON\"\\\n",
		Outcome: "pass", DurationSeconds: 1,
	}})
	item.Job = "cmd/gc\"\njob"
	item.Variant = "linux\\variant"
	item.Runner.Label = "runner\"\nlabel"
	item.Runner.OS = "Linux|row"
	item.Runner.Arch = "X64</script>"
	writeArtifact(t, root, "hostile.json", item)

	snapshot := decodeHistorySnapshot(t, runJSONHistory(t, root))
	profile := findHistoryProfile(t, snapshot, 32)
	if profile.Job != item.Job || profile.Variant != item.Variant ||
		profile.Runner.Label != item.Runner.Label || profile.Runner.OS != item.Runner.OS || profile.Runner.Arch != item.Runner.Arch {
		t.Fatalf("profile metadata changed across JSON round trip: got %+v want %+v", profile, item)
	}
	unit := findHistoryUnit(t, profile, item.Units[0].UnitID)
	if unit.Package != item.Units[0].Package || unit.Test != item.Units[0].Test || unit.Subtest != "" {
		t.Fatalf("unit metadata changed across JSON round trip: got %+v want %+v", unit, item.Units[0])
	}
}

func TestBuildSnapshotJSONWireIsByteExact(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	item := timingArtifact("940", defaultRunner("ephemeral"), []timingUnit{
		{UnitID: "internal/example:TestWarm", Kind: "test", Package: "github.com/gastownhall/gascity/internal/example", Test: "TestWarm", Outcome: "pass", DurationSeconds: 1.5},
		{UnitID: "internal/example:TestCold", Kind: "test", Package: "github.com/gastownhall/gascity/internal/example", Test: "TestCold", Outcome: "fail", DurationSeconds: 2},
	})
	item.CommitSHA = "sha-warm"
	writeArtifact(t, root, "wire.json", item)

	got := runJSONHistory(t, root)
	want := `{"schema":1,"unique_artifact_count":1,"duplicate_artifact_count":0,"profiles":[` +
		`{"job":"cmd-gc-process","variant":"linux-default","runner":{"label":"blacksmith-32vcpu","os":"Linux","arch":"X64","cpu_count":32},"units":[` +
		`{"unit_id":"internal/example:TestCold","package":"github.com/gastownhall/gascity/internal/example","test":"TestCold","subtest":"","passes":0,"failures":1,"skips":0,` +
		`"duration_seconds_p50":null,"duration_seconds_p75":null,"duration_seconds_p95":null,"duration_seconds_population_variance":null,"p75_authoritative":false,"p95_authoritative":false,"last_success_sha":null,"successful_observations":[]},` +
		`{"unit_id":"internal/example:TestWarm","package":"github.com/gastownhall/gascity/internal/example","test":"TestWarm","subtest":"","passes":1,"failures":0,"skips":0,` +
		`"duration_seconds_p50":1.5,"duration_seconds_p75":1.5,"duration_seconds_p95":1.5,"duration_seconds_population_variance":0,"p75_authoritative":false,"p95_authoritative":false,"last_success_sha":"sha-warm","successful_observations":[` +
		`{"artifact_identity":{"workflow":"CI","run_id":"940","run_attempt":"1","job":"cmd-gc-process","shard_id":"cmd-gc-process-1-of-12","variant":"linux-default"},"tested_sha":"sha-warm","duration_seconds":1.5}]}]}]}` + "\n"
	if got != want {
		t.Fatalf("JSON wire changed\ngot:  %q\nwant: %q", got, want)
	}
}

func TestBuildSnapshotDefaultMarkdownRemainsByteForByte(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeArtifact(t, root, "one.json", timingArtifact("930", defaultRunner("ephemeral"), []timingUnit{{
		UnitID: "internal/example:TestOne", Kind: "test",
		Package: "github.com/gastownhall/gascity/internal/example", Test: "TestOne",
		Outcome: "pass", DurationSeconds: 1,
	}}))

	stdout, stderr, exitCode := runSummary(root)
	if exitCode != 0 || stderr != "" {
		t.Fatalf("default Markdown Run = stdout %q stderr %q exit %d", stdout, stderr, exitCode)
	}
	const want = "# Go test timing summary\n\n" +
		"Analyzed 1 unique schema-v1 artifact; 0 duplicate downloads ignored.\n\n" +
		"Rankings use successful durations from top-level tests only. Package totals and nested subtests are excluded. Profiles are never mixed.\n\n" +
		"## Profile 1\n\n" +
		"| Job | Variant | Runner label | OS | Arch | CPUs |\n" +
		"| --- | --- | --- | --- | --- | ---: |\n" +
		"| `cmd-gc-process` | `linux-default` | `blacksmith-32vcpu` | `Linux` | `X64` | 32 |\n\n" +
		"Top-level outcomes: 1 pass, 0 fail, 0 skip.\n\n" +
		"### Ten slowest top-level tests\n\n" +
		"| Runnable unit | Pass | Fail | Skip | p50 | p75 | p95 |\n" +
		"| --- | ---: | ---: | ---: | ---: | ---: | ---: |\n" +
		"| `internal/example:TestOne` | 1 | 0 | 0 | 1.000s | 1.000s | 1.000s |\n\n" +
		"### Ten highest-variance top-level tests\n\n" +
		"| Runnable unit | Pass | Fail | Skip | Population variance (s²) | p95 |\n" +
		"| --- | ---: | ---: | ---: | ---: | ---: |\n" +
		"| _At least two successful samples are required_ | 0 | 0 | 0 | — | — |\n\n"
	if stdout != want {
		t.Fatalf("default Markdown changed\ngot:\n%s\nwant:\n%s", stdout, want)
	}
	explicit, explicitStderr, explicitExitCode := runSummary("--format=markdown", root)
	if explicitExitCode != 0 || explicitStderr != "" || explicit != want {
		t.Fatalf("explicit Markdown = stdout %q stderr %q exit %d", explicit, explicitStderr, explicitExitCode)
	}
}

func runJSONHistory(t *testing.T, roots ...string) string {
	t.Helper()
	args := append([]string{"--format=json"}, roots...)
	stdout, stderr, exitCode := runSummary(args...)
	if exitCode != 0 {
		t.Fatalf("Run(--format=json) exit = %d, stderr:\n%s", exitCode, stderr)
	}
	if stderr != "" {
		t.Fatalf("Run(--format=json) wrote stderr on success: %s", stderr)
	}
	return stdout
}

func decodeHistorySnapshot(t *testing.T, output string) Snapshot {
	t.Helper()
	var snapshot Snapshot
	decoder := json.NewDecoder(strings.NewReader(output))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		t.Fatalf("decode JSON history snapshot: %v\n%s", err, output)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		t.Fatalf("decode JSON history snapshot trailing data: %v\n%s", err, output)
	}
	return snapshot
}

func historyFloat(t *testing.T, value *float64) float64 {
	t.Helper()
	if value == nil {
		t.Fatal("history statistic is null, want a finite value")
	}
	return *value
}

func findHistoryProfile(t *testing.T, snapshot Snapshot, cpuCount int) Profile {
	t.Helper()
	for _, profile := range snapshot.Profiles {
		if profile.Runner.CPUCount == cpuCount {
			return profile
		}
	}
	t.Fatalf("history has no %d-CPU profile: %+v", cpuCount, snapshot.Profiles)
	return Profile{}
}

func findHistoryUnit(t *testing.T, profile Profile, unitID string) UnitHistory {
	t.Helper()
	for _, unit := range profile.Units {
		if unit.UnitID == unitID {
			return unit
		}
	}
	t.Fatalf("profile has no unit %q: %+v", unitID, profile.Units)
	return UnitHistory{}
}
