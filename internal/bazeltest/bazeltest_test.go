package bazeltest

import (
	"slices"
	"testing"
)

func TestHelperProcessEnvDropsParentOwnedTestRunnerState(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"COVERAGE_OUTPUT_FILE=/out/coverage.dat",
		"TESTBRIDGE_TEST_ONLY=TestParent",
		"TEST_TOTAL_SHARDS=4",
		"TEST_SHARD_INDEX=2",
		"TEST_SHARD_STATUS_FILE=/out/shard",
		"XML_OUTPUT_FILE=/out/test.xml",
		"COVERAGE_DIR=/out/cov",
		"TEST_TMPDIR=/tmp/bt/x",
		"GC_CHILD=1",
	}
	got := HelperProcessEnv(in)
	want := []string{
		"PATH=/usr/bin",
		"TEST_TMPDIR=/tmp/bt/x",
		"GC_CHILD=1",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("HelperProcessEnv() = %q, want %q", got, want)
	}
	if len(in) != 10 || in[1] != "COVERAGE_OUTPUT_FILE=/out/coverage.dat" {
		t.Fatalf("HelperProcessEnv mutated its input: %q", in)
	}
}

func TestHelperProcessEnvKeepsLookalikeNames(t *testing.T) {
	in := []string{"COVERAGE_OUTPUT_FILE_EXTRA=1", "MY_TEST_SHARD_INDEX=3"}
	if got := HelperProcessEnv(in); !slices.Equal(got, in) {
		t.Fatalf("HelperProcessEnv() = %q, want %q unchanged", got, in)
	}
}
