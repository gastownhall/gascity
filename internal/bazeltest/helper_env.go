package bazeltest

import (
	"slices"
	"strings"
)

// parentOwnedTestEnv names the variables through which Bazel's test runner
// hands exactly one test process its private outputs and its test selection.
// rules_go's generated test main acts on each of them at startup, so a child
// that re-executes the test binary must not inherit them:
//
//   - COVERAGE_OUTPUT_FILE: the child would write -test.coverprofile to the
//     parent's path; concurrent children interleave writes into that one file
//     and fail converting it to lcov ("invalid go cover line", exit 2).
//   - TESTBRIDGE_TEST_ONLY: --test_filter; it overrides the child's own
//     -test.run, re-running the parent test instead of the helper.
//   - TEST_TOTAL_SHARDS / TEST_SHARD_INDEX / TEST_SHARD_STATUS_FILE: the child
//     would run only the parent's shard of tests, which may exclude the helper.
//   - XML_OUTPUT_FILE: the child would overwrite the parent's test.xml report.
var parentOwnedTestEnv = []string{
	"COVERAGE_OUTPUT_FILE",
	"TESTBRIDGE_TEST_ONLY",
	"TEST_TOTAL_SHARDS",
	"TEST_SHARD_INDEX",
	"TEST_SHARD_STATUS_FILE",
	"XML_OUTPUT_FILE",
}

// HelperProcessEnv returns a copy of env, in order, without the Bazel test
// runner variables that belong to the parent test process alone. Use it for
// the environment of a helper process that re-executes the test binary
// (exec.Command(os.Args[0], "-test.run=^TestHelper$")). It is a no-op under
// plain go test, where none of these variables are set.
func HelperProcessEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if slices.Contains(parentOwnedTestEnv, name) {
			continue
		}
		out = append(out, kv)
	}
	return out
}
