package scripts_test

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// reviewFormulaShardBuckets maps each modulo-sharded review-formulas bucket in
// scripts/test-integration-shard to the shell array listing its tests.
var reviewFormulaShardBuckets = map[string]string{
	"review-formulas-basic":   "review_formulas_basic_tests",
	"review-formulas-retries": "review_formulas_retry_tests",
}

// reviewFormulaShardCallers are the files that fan the review-formulas buckets
// out into per-shard jobs. Each one must name the same shard total.
var reviewFormulaShardCallers = []string{
	"scripts/test-local-parallel",
	".github/workflows/rc-gate.yml",
}

// TestReviewFormulaShardsRunOneTestEach pins every review-formulas shard to a
// single test. Each of these tests boots its own managed-Dolt city and runs for
// many minutes, and a shard shares one go test timeout across all the tests
// modulo sharding hands it, so a shard holding two of them overruns that budget
// on a loaded host. A bucket split N ways must have exactly N tests, and every
// caller must schedule shards 1..N, so that adding a test without adding a
// shard fails here instead of timing out in the gate.
func TestReviewFormulaShardsRunOneTestEach(t *testing.T) {
	root := repoRoot(t)
	shardScript := readRepoFile(t, root, "scripts/test-integration-shard")

	for bucket, array := range reviewFormulaShardBuckets {
		tests := shellArrayValues(t, shardScript, array)
		for _, caller := range reviewFormulaShardCallers {
			indices, totals := reviewFormulaShardIndices(t, readRepoFile(t, root, caller), bucket)
			if len(totals) != 1 {
				t.Errorf("%s schedules %s with shard totals %v, want one total", caller, bucket, totals)
				continue
			}
			var total int
			for n := range totals {
				total = n
			}
			if total != len(tests) {
				t.Errorf("%s splits %s %d ways but %s lists %d tests %v; each review-formulas shard must run exactly one test",
					caller, bucket, total, array, len(tests), tests)
			}
			for i := 1; i <= total; i++ {
				if !indices[i] {
					t.Errorf("%s never schedules %s-%d-of-%d", caller, bucket, i, total)
				}
			}
		}
	}
}

// shellArrayValues returns the words of a top-level `name=( ... )` array.
func shellArrayValues(t *testing.T, script, name string) []string {
	t.Helper()
	pattern := regexp.MustCompile(`(?ms)^` + regexp.QuoteMeta(name) + `=\(\n(.*?)^\)`)
	match := pattern.FindStringSubmatch(script)
	if match == nil {
		t.Fatalf("scripts/test-integration-shard defines no %s array", name)
	}
	values := strings.Fields(match[1])
	if len(values) == 0 {
		t.Fatalf("scripts/test-integration-shard %s array is empty", name)
	}
	return values
}

// reviewFormulaShardIndices collects the shard indices and totals a caller
// schedules for bucket. It reads literal `<bucket>-K-of-N` names and expands
// `for i in 1 2 ...; do ... <bucket>-${i}-of-N` loops.
func reviewFormulaShardIndices(t *testing.T, text, bucket string) (map[int]bool, map[int]bool) {
	t.Helper()
	indices := map[int]bool{}
	totals := map[int]bool{}
	quoted := regexp.QuoteMeta(bucket)

	literal := regexp.MustCompile(quoted + `-(\d+)-of-(\d+)`)
	for _, match := range literal.FindAllStringSubmatch(text, -1) {
		indices[parseShardNumber(t, match[1])] = true
		totals[parseShardNumber(t, match[2])] = true
	}

	loop := regexp.MustCompile(`for i in ([\d ]+); do\n[^\n]*` + quoted + `-\$\{i\}-of-(\d+)`)
	for _, match := range loop.FindAllStringSubmatch(text, -1) {
		for _, field := range strings.Fields(match[1]) {
			indices[parseShardNumber(t, field)] = true
		}
		totals[parseShardNumber(t, match[2])] = true
	}

	if len(totals) == 0 {
		t.Fatalf("no %s-N-of-M shards found; the review-formulas fan-out moved", bucket)
	}
	return indices, totals
}

func parseShardNumber(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("parse shard number %q: %v", s, err)
	}
	return n
}
