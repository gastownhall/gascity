package shardbudget

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testResultEvent builds one BEP testResult line, matching the JSON shape
// bazel.yml's lanes write and internal/testpolicy/bepsummary's bep.go reads.
// Every test in this file uses label "//x:t" (ReadShardDurations and Run
// cases) or builds its ShardDurations map directly (Evaluate cases), so
// the label is fixed here rather than threaded through every call site.
func testResultEvent(shard int64, ms int64, cachedLocally, cachedRemotely bool) string {
	ev := map[string]any{
		"id": map[string]any{
			"testResult": map[string]any{"label": "//x:t", "shard": shard},
		},
		"testResult": map[string]any{
			"testAttemptDurationMillis": ms,
			"cachedLocally":             cachedLocally,
			"executionInfo":             map[string]any{"cachedRemotely": cachedRemotely, "strategy": "remote"},
		},
	}
	b, err := json.Marshal(ev)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func writeBEP(t *testing.T, lines []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bep.json")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write BEP file: %v", err)
	}
	return path
}

func TestReadShardDurationsCollectsExecutedDurationsPerShard(t *testing.T) {
	r := strings.NewReader(
		testResultEvent(1, 10_000, false, false) + "\n" +
			testResultEvent(2, 20_000, false, false) + "\n",
	)
	got := ReadShardDurations(r)
	want := ShardDurations{"//x:t": {1: 10_000, 2: 20_000}}
	if !shardDurationsEqual(got, want) {
		t.Errorf("ReadShardDurations = %v, want %v", got, want)
	}
}

func TestReadShardDurationsExcludesCachedResults(t *testing.T) {
	r := strings.NewReader(
		testResultEvent(1, 10_000, false, false) + "\n" +
			testResultEvent(2, 999_000, false, true) + "\n" + // remote cache hit
			testResultEvent(3, 888_000, true, false) + "\n", // local cache hit
	)
	got := ReadShardDurations(r)
	want := ShardDurations{"//x:t": {1: 10_000}}
	if !shardDurationsEqual(got, want) {
		t.Errorf("ReadShardDurations = %v, want %v (shards 2 and 3 were cache hits)", got, want)
	}
}

func TestReadShardDurationsUnshardedDefaultsToShard1(t *testing.T) {
	r := strings.NewReader(testResultEvent(0, 5_000, false, false) + "\n")
	got := ReadShardDurations(r)
	want := ShardDurations{"//x:t": {1: 5_000}}
	if !shardDurationsEqual(got, want) {
		t.Errorf("ReadShardDurations = %v, want %v", got, want)
	}
}

func TestReadShardDurationsIgnoresNonTestResultLines(t *testing.T) {
	r := strings.NewReader(`{"id":{"progress":{}},"progress":{}}` + "\n")
	got := ReadShardDurations(r)
	if len(got) != 0 {
		t.Errorf("ReadShardDurations = %v, want empty", got)
	}
}

func TestReadShardDurationsSkipsMalformedLine(t *testing.T) {
	r := strings.NewReader("not json\n" + testResultEvent(1, 1_000, false, false) + "\n")
	got := ReadShardDurations(r)
	want := ShardDurations{"//x:t": {1: 1_000}}
	if !shardDurationsEqual(got, want) {
		t.Errorf("ReadShardDurations = %v, want %v", got, want)
	}
}

func shardDurationsEqual(a, b ShardDurations) bool {
	if len(a) != len(b) {
		return false
	}
	for label, perShard := range a {
		other, ok := b[label]
		if !ok || len(perShard) != len(other) {
			return false
		}
		for shard, ms := range perShard {
			if other[shard] != ms {
				return false
			}
		}
	}
	return true
}

func shardsAt(n int, fill int64) map[int64]int64 {
	m := make(map[int64]int64, n)
	for i := 1; i <= n; i++ {
		m[int64(i)] = fill
	}
	return m
}

func TestEvaluateBalancedShardsDoNotWarn(t *testing.T) {
	// gascity #7241-shaped data: every shard within a few seconds of the
	// others never crosses max(150s, 2x median).
	shards := shardsAt(10, 60_000)
	for i := int64(1); i <= 10; i++ {
		shards[i] += i * 1000
	}
	results := Evaluate(ShardDurations{"//x:t": shards})
	if len(results) != 1 {
		t.Fatalf("Evaluate: got %d results, want 1", len(results))
	}
	r := results[0]
	if r.Warn() || r.Fail() {
		t.Errorf("balanced shards: warn=%v fail=%v, want both false (%s)", r.Warn(), r.Fail(), r.Message())
	}
}

func TestEvaluateSingleLongPoleWarns(t *testing.T) {
	// gascity acceptance shard 4 shape: one shard far above the rest.
	shards := shardsAt(9, 80_000)
	shards[4] = 725_000
	results := Evaluate(ShardDurations{"//acceptance:t": shards})
	if len(results) != 1 {
		t.Fatalf("Evaluate: got %d results, want 1", len(results))
	}
	r := results[0]
	if !r.Warn() {
		t.Errorf("single long pole: warn=false, want true (%s)", r.Message())
	}
	if r.SlowestShard != 4 || r.SlowestMillis != 725_000 {
		t.Errorf("Evaluate slowest shard=%d millis=%d, want 4/725000", r.SlowestShard, r.SlowestMillis)
	}
}

func TestEvaluateFailThresholdIsStricterThanWarn(t *testing.T) {
	// beads server-Dolt storage tier shape: a straggler 4.3-4.5 min behind
	// an otherwise ~80s median. Over the warn floor (150s, 2x median) but
	// under the fail floor (300s, 3x median) until it grows further.
	shards := shardsAt(15, 80_000)
	shards[16] = 250_000
	r := Evaluate(ShardDurations{"//storage/dolt:t": shards})[0]
	if !r.Warn() || r.Fail() {
		t.Errorf("under fail threshold: warn=%v fail=%v, want true/false (%s)", r.Warn(), r.Fail(), r.Message())
	}

	shards[16] = 400_000
	r = Evaluate(ShardDurations{"//storage/dolt:t": shards})[0]
	if !r.Warn() || !r.Fail() {
		t.Errorf("over fail threshold: warn=%v fail=%v, want true/true (%s)", r.Warn(), r.Fail(), r.Message())
	}
}

func TestEvaluateFloorAppliesBelowTinyMedians(t *testing.T) {
	// A fast, lopsided split (two 5s shards, one 200s shard) must still
	// warn even though 2x the 5s median is nowhere near the slowest shard;
	// the 150s floor catches it, not the ratio. Three shards keep the
	// median an exact element (5s), not an average of two.
	shards := map[int64]int64{1: 5_000, 2: 5_000, 3: 200_000}
	r := Evaluate(ShardDurations{"//x:t": shards})[0]
	if !r.Warn() {
		t.Errorf("tiny median floor: warn=false, want true (%s)", r.Message())
	}
}

func TestEvaluateFewerThanTwoShardsIsSkipped(t *testing.T) {
	for _, shards := range []map[int64]int64{{1: 10_000}, {}} {
		if got := Evaluate(ShardDurations{"//x:t": shards}); len(got) != 0 {
			t.Errorf("Evaluate(%v) = %v, want no results", shards, got)
		}
	}
}

func TestEvaluateSortedSlowestFirst(t *testing.T) {
	results := Evaluate(ShardDurations{
		"//a:t": {1: 10_000, 2: 20_000},
		"//b:t": {1: 10_000, 2: 500_000},
	})
	if len(results) != 2 || results[0].Label != "//b:t" || results[1].Label != "//a:t" {
		var labels []string
		for _, r := range results {
			labels = append(labels, r.Label)
		}
		t.Errorf("Evaluate order = %v, want [//b:t //a:t]", labels)
	}
}

func TestRunWarnOnlyExitsZero(t *testing.T) {
	shards := shardsAt(9, 80_000)
	shards[4] = 900_000
	var lines []string
	for shard, ms := range shards {
		lines = append(lines, testResultEvent(shard, ms, false, false))
	}
	path := writeBEP(t, lines)

	var stdout, stderr bytes.Buffer
	rc := Run([]string{path}, &stdout, &stderr)
	if rc != 0 {
		t.Errorf("Run (warn-only) = %d, want 0; stderr=%s", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "::warning") {
		t.Errorf("Run stdout = %q, want a ::warning line", stdout.String())
	}
}

func TestRunFailOnImbalanceExitsNonzeroPastFailThreshold(t *testing.T) {
	shards := shardsAt(9, 80_000)
	shards[4] = 900_000
	var lines []string
	for shard, ms := range shards {
		lines = append(lines, testResultEvent(shard, ms, false, false))
	}
	path := writeBEP(t, lines)

	var stdout, stderr bytes.Buffer
	rc := Run([]string{"--fail-on-imbalance", path}, &stdout, &stderr)
	if rc != 1 {
		t.Errorf("Run (--fail-on-imbalance, over threshold) = %d, want 1; stdout=%s stderr=%s", rc, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "::error") {
		t.Errorf("Run stdout = %q, want an ::error line", stdout.String())
	}
}

func TestRunFailOnImbalanceStillZeroUnderFailThreshold(t *testing.T) {
	shards := shardsAt(9, 80_000)
	shards[4] = 200_000
	var lines []string
	for shard, ms := range shards {
		lines = append(lines, testResultEvent(shard, ms, false, false))
	}
	path := writeBEP(t, lines)

	var stdout, stderr bytes.Buffer
	rc := Run([]string{"--fail-on-imbalance", path}, &stdout, &stderr)
	if rc != 0 {
		t.Errorf("Run (--fail-on-imbalance, under threshold) = %d, want 0; stdout=%s stderr=%s", rc, stdout.String(), stderr.String())
	}
}

func TestRunAllowMissingSkipsAbsentFile(t *testing.T) {
	present := writeBEP(t, []string{testResultEvent(1, 1_000, false, false)})
	missing := filepath.Join(t.TempDir(), "does-not-exist.json")

	var stdout, stderr bytes.Buffer
	rc := Run([]string{"--allow-missing", present, missing}, &stdout, &stderr)
	if rc != 0 {
		t.Errorf("Run (--allow-missing) = %d, want 0; stderr=%s", rc, stderr.String())
	}
}

func TestRunWithoutAllowMissingFailsOnAbsentFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.json")

	var stdout, stderr bytes.Buffer
	rc := Run([]string{missing}, &stdout, &stderr)
	if rc != 1 {
		t.Errorf("Run (missing file, no --allow-missing) = %d, want 1", rc)
	}
	if !strings.Contains(stderr.String(), "does-not-exist.json") {
		t.Errorf("Run stderr = %q, want it to name the missing file", stderr.String())
	}
}

func TestRunNoArgsIsUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := Run(nil, &stdout, &stderr)
	if rc != 2 {
		t.Errorf("Run() = %d, want 2 (usage error)", rc)
	}
}
