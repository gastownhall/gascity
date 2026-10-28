package scripts_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// integrationPackagesShards: //test:integration_packages' test targets and
// each one's shard_count (1 when unsharded), i.e. the integration-packages
// lane's test actions, one per target shard. aquery-verified (79 TestRunner
// actions) with beads_test at 11 shards (gascity #8083), plus
// //test/conversationlane's six targets (OJ-12x, split by its
// INTEGRATION_SOLO_TESTS on 2026-10-24); recompute with
//
//	T=$(bazel query 'tests(//test:integration_packages)' | paste -sd+ | sed 's/+/ + /g')
//	bazel aquery --define=gotags=integration "mnemonic(\"TestRunner\", $T)" --output=text |
//	  grep -o 'Target: //[^ ]*' | sort | uniq -c
//
// (aquery takes build options, not test:integration's, hence --define;
// it does not expand a test_suite under mnemonic(), hence the explicit
// union). TestIntegrationPackagesFitsTheJobsBudget checks this table
// against the suite and, where this test's tree carries the package's
// BUILD.bazel, against its shard_count.
var integrationPackagesShards = map[string]int{
	"//cmd/gc-write-mint:gc-write-mint_test":                     1,
	"//cmd/gc:gc_test":                                           24,
	"//examples/bd/dolt:dolt_test":                               6,
	"//examples/gastown:gastown_test":                            5,
	"//internal/api:api_test":                                    6,
	"//internal/bdflags:bdflags_test":                            1,
	"//internal/beads/exec:exec_test":                            1,
	"//internal/beads:beads_test":                                11,
	"//internal/credentialprovider:credentialprovider_test":      1,
	"//internal/orders:orders_test":                              1,
	"//internal/processgroup:processgroup_test":                  1,
	"//internal/runtime/acp:acp_test":                            1,
	"//internal/runtime/exec:exec_test":                          1,
	"//internal/runtime/herdr:herdr_test":                        1,
	"//internal/runtime/ssh:ssh_test":                            1,
	"//internal/runtime/subprocess:subprocess_test":              1,
	"//internal/runtime/tmux:tmux_test":                          3,
	"//internal/session:session_test":                            1,
	"//internal/storebinding/beadsworkspace:beadsworkspace_test": 1,
	"//internal/storebinding:storebinding_test":                  1,
	"//test/containerhost:containerhost_test":                    1,
	"//test/dashport:dashport_test":                              1,
	"//cmd/gc:gc_bd_runtime_custom_types_heal_test":              1,
	"//cmd/gc:gc_managed_beads_native_cli_test":                  1,
	"//cmd/gc:gc_managed_bd_rig_hard_kill_test":                  1,
	"//cmd/gc:gc_pack_command_cobra_parity_test":                 1,
	"//cmd/gc:gc_provider_owned_real_init_test":                  1,
	"//cmd/gc:gc_phase2_worker_core_transport_claude_test":       1,
	"//cmd/gc:gc_phase2_worker_core_transport_codex_test":        1,
	"//cmd/gc:gc_phase2_worker_core_transport_test":              1,
	// test/conversationlane/BUILD.bazel: the main target and the five its
	// INTEGRATION_SOLO_TESTS and INTEGRATION_SOLO_SPLIT_TESTS define.
	"//test/conversationlane:conversationlane_test":                1,
	"//test/conversationlane:conversationlane_x12_test":            1,
	"//test/conversationlane:conversationlane_x2_cli_test":         1,
	"//test/conversationlane:conversationlane_x2_host_sqlite_test": 1,
	"//test/conversationlane:conversationlane_x2_host_test":        1,
	"//test/conversationlane:conversationlane_x8_test":             1,
}

// integrationPackagesReadBuilds: the packages whose BUILD.bazel
// //scripts:scripts_test's data carries, so the budget check reads their
// shard_count under bazel too, not only under `go test`.
var integrationPackagesReadBuilds = []string{"cmd/gc", "examples/gastown", "internal/beads", "test/conversationlane"}

// integrationPackagesTargets expands //test:integration_packages' tests
// list: go_test labels as they are, and each package's
// :<name>_integration_solo_tests suite (cmd/gc's, test/conversationlane's)
// into the target names of that package's INTEGRATION_SOLO_TESTS and
// INTEGRATION_SOLO_SPLIT_TESTS (each a go_variant_test, unsharded).
func integrationPackagesTargets(t *testing.T, root string) []string {
	t.Helper()
	suite := readFile(t, root, "test/BUILD.bazel")
	body := regexp.MustCompile(`(?s)test_suite\(\n    name = "integration_packages",.*?tests = \[\n(.*?)\n    \],`).FindStringSubmatch(suite)
	if body == nil {
		t.Fatal("test/BUILD.bazel has no integration_packages test_suite tests list")
	}
	soloSuite := regexp.MustCompile(`^//([a-z0-9_/]+):[a-z0-9_]+_integration_solo_tests$`)
	var out []string
	for _, m := range regexp.MustCompile(`"(//[^"]+)"`).FindAllStringSubmatch(body[1], -1) {
		solo := soloSuite.FindStringSubmatch(m[1])
		if solo == nil {
			out = append(out, m[1])
			continue
		}
		pkg := solo[1]
		build := readFile(t, root, pkg+"/BUILD.bazel")
		for _, table := range []string{"INTEGRATION_SOLO_TESTS", "INTEGRATION_SOLO_SPLIT_TESTS"} {
			tb := regexp.MustCompile(`(?ms)^` + table + ` = \{\n(.*?)^\}`).FindStringSubmatch(build)
			if tb == nil {
				t.Fatalf("%s/BUILD.bazel has no %s dict", pkg, table)
			}
			for _, e := range regexp.MustCompile(`(?m)^\s+"([a-z0-9_]+_test)": `).FindAllStringSubmatch(tb[1], -1) {
				out = append(out, "//"+pkg+":"+e[1])
			}
		}
	}
	return out
}

// integrationPackagesLaneCmd returns the integration-packages lane's bazel
// command as bazel.yml's Lanes step builds it (mode remote, a pull request).
func integrationPackagesLaneCmd(t *testing.T) string {
	t.Helper()
	wf := readMultiLaneWorkflow(t)
	raw, err := multiLaneLanes(t, multiLaneRBEStep(t, wf, "lanes").Run, "pull_request", "remote", "false")
	if err != nil {
		t.Fatalf("Lanes step failed: %v\n%s", err, raw)
	}
	var entries []map[string]any
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		t.Fatalf("lanes %q: %v", raw, err)
	}
	for _, e := range entries {
		if e["lane"] == "integration-packages" {
			cmd, _ := e["cmd"].(string)
			return cmd
		}
	}
	t.Fatalf("no integration-packages lane in %s", raw)
	return ""
}

// bazelLaneEffectiveJobs: the --jobs a lane's `bazel <cmd>` runs with in
// mode remote. The generated rc applies `build --config=remote-exec` from an
// rc file, so build:remote-exec's --jobs is the baseline; a --jobs on a
// config the lane's own command line selects (--config=X, expanded with the
// configs X includes) outranks it, because Bazel ranks command-line configs
// above rc-file ones whichever line comes first in .bazelrc.
func bazelLaneEffectiveJobs(rc, cmd string) (int, error) {
	opts := parseBazelRC(rc)
	jobs := 0
	for _, o := range opts {
		if o.command == "build" && o.config == "remote-exec" && strings.HasPrefix(o.flag, "--jobs=") {
			jobs, _ = strconv.Atoi(strings.TrimPrefix(o.flag, "--jobs="))
		}
	}
	if jobs == 0 {
		return 0, errors.New(".bazelrc has no build:remote-exec --jobs=N line")
	}
	var expand func(config string, depth int)
	expand = func(config string, depth int) {
		if depth > 10 {
			return
		}
		for _, o := range opts {
			if o.config != config || (o.command != "build" && o.command != "test" && o.command != "common") {
				continue
			}
			if inner, ok := strings.CutPrefix(o.flag, "--config="); ok {
				expand(inner, depth+1)
			} else if n, ok := strings.CutPrefix(o.flag, "--jobs="); ok {
				jobs, _ = strconv.Atoi(n)
			}
		}
	}
	for _, f := range strings.Fields(cmd) {
		if config, ok := strings.CutPrefix(f, "--config="); ok {
			expand(config, 0)
		}
	}
	return jobs, nil
}

// TestIntegrationPackagesFitsTheJobsBudget: the integration-packages lane
// (the most frequent last lane before the gate) runs every
// //test:integration_packages shard in one invocation, so all of them must
// fit in Bazel's first wave of --jobs actions. Bazel does not schedule the
// longest action first, so a second-wave tail test adds its full duration to
// the critical path instead of overlapping it.
func TestIntegrationPackagesFitsTheJobsBudget(t *testing.T) {
	root := repoRoot(t)
	cmd := integrationPackagesLaneCmd(t)
	budget, err := bazelLaneEffectiveJobs(readFile(t, root, ".bazelrc"), cmd)
	if err != nil {
		t.Fatal(err)
	}

	targets := integrationPackagesTargets(t, root)
	var want []string
	for label := range integrationPackagesShards {
		want = append(want, label)
	}
	sort.Strings(want)
	got := slices.Clone(targets)
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Errorf("//test:integration_packages runs %v; integrationPackagesShards lists %v: re-run its aquery and update it", got, want)
	}

	total := 0
	for _, label := range targets {
		shards, ok := integrationPackagesShards[label]
		if !ok {
			shards = 1
		}
		pkg, name, _ := strings.Cut(strings.TrimPrefix(label, "//"), ":")
		path := pkg + "/BUILD.bazel"
		build, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path))) //nolint:gosec // a path inside the repo
		switch {
		case err == nil && strings.Contains(string(build), "go_test(\n    name = \""+name+"\","):
			if n := goTestShardCount(t, goTestRule(t, string(build), path, name), path); n != shards {
				t.Errorf("%s: shard_count %d, integrationPackagesShards says %d: re-run its aquery and update it", label, n, shards)
				shards = n
			}
		case err != nil && slices.Contains(integrationPackagesReadBuilds, pkg):
			t.Errorf("read %s: %v", path, err)
		}
		total += shards
	}
	if err := checkIntegrationPackagesJobsBudget(total, budget); err != nil {
		t.Error(err)
	}
}

// integrationPackagesTwoWaveExceptionPinned: the integration-packages lane
// used to get its own --jobs=128 (#7795) so its test actions fit Bazel's
// first scheduling wave. Farm owner maintainer-city-c3's before/after found
// no lane gain and a farm guard regression (OSS queue-wait p95 8.9s->22.9s),
// so #7795 was reverted and the lane is back to running in two waves under
// the inherited --jobs=64. 85 test actions (aquery-verified, see
// integrationPackagesShards above) is accepted as a known, pinned exception
// so the budget check still catches further growth. If the count grows
// above this pin, that is this lane asking for a bigger second-wave tail,
// not a smaller one; split it instead of raising --jobs again (e.g. shard
// out //cmd/gc:gc_test's long tail) — see beads closed PR #7542 and gascity
// #7795's before/after for what that tail costs. Raised from 81 on
// 2026-10-24 for //test/conversationlane's split: its one 395-450 s action,
// the lane's last from #8147 on, became six that run side by side (the
// longest about 200 s), so its five new actions shorten the lane's tail.
const integrationPackagesTwoWaveExceptionPinned = 85

// checkIntegrationPackagesJobsBudget reports whether total (the lane's test
// action count) is acceptable against effective (its --jobs). total may
// exceed effective only up to integrationPackagesTwoWaveExceptionPinned,
// a documented, known two-wave exception; growing past that pin must fail
// loudly rather than quietly tolerate a longer tail, and if total ever
// drops back to effective or below, the exception should be removed.
func checkIntegrationPackagesJobsBudget(total, effective int) error {
	if total <= effective {
		if integrationPackagesTwoWaveExceptionPinned > effective {
			return fmt.Errorf("integration-packages: %d test actions now fit the effective --jobs=%d: remove the integrationPackagesTwoWaveExceptionPinned (%d) exception, it is no longer needed", total, effective, integrationPackagesTwoWaveExceptionPinned)
		}
		return nil
	}
	if total > integrationPackagesTwoWaveExceptionPinned {
		return fmt.Errorf("integration-packages: %d test actions, more than the pinned two-wave exception (%d) at effective --jobs=%d: the lane is already two waves (second-wave tail, see beads closed PR #7542 and gascity #7795's before/after) and should be split (e.g. shard out //cmd/gc:gc_test's long tail), not given more concurrency", total, integrationPackagesTwoWaveExceptionPinned, effective)
	}
	return nil
}

// TestIntegrationPackagesJobsBudgetException mutation-proves
// checkIntegrationPackagesJobsBudget: growth past the pinned exception must
// fail loudly, and the exception must be flagged for removal once the lane
// no longer needs it.
func TestIntegrationPackagesJobsBudgetException(t *testing.T) {
	const pinned = integrationPackagesTwoWaveExceptionPinned
	for _, tc := range []struct {
		name    string
		total   int
		wantErr bool
	}{
		{"within effective jobs: exception is stale, must say so", 64, true},
		{"within the pinned two-wave exception", pinned, false},
		{"one past the pin: must fail loudly", pinned + 1, true},
		{"far past the pin: must fail loudly", pinned + 20, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkIntegrationPackagesJobsBudget(tc.total, 64)
			if tc.wantErr && err == nil {
				t.Errorf("checkIntegrationPackagesJobsBudget(%d, 64) = nil, want an error (growth past the pin must fail loudly)", tc.total)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("checkIntegrationPackagesJobsBudget(%d, 64) = %v, want nil", tc.total, err)
			}
		})
	}

	// If the lane's total ever drops to the effective jobs or below while
	// the exception is still pinned above it, the check must say so instead
	// of silently passing.
	if err := checkIntegrationPackagesJobsBudget(64, 64); err == nil {
		t.Error("checkIntegrationPackagesJobsBudget(64, 64) = nil, want an error telling us to remove the now-unneeded exception")
	}
	// Once effective jobs catches up to (or passes) the pin, there is
	// nothing left to flag.
	if err := checkIntegrationPackagesJobsBudget(pinned, pinned); err != nil {
		t.Errorf("checkIntegrationPackagesJobsBudget(%d, %d) = %v, want nil", pinned, pinned, err)
	}
}

// TestBazelLaneEffectiveJobs pins the precedence bazelLaneEffectiveJobs
// models.
func TestBazelLaneEffectiveJobs(t *testing.T) {
	rc := "build:remote-exec --jobs=64\ntest:a --jobs=128\ntest:b --config=a\ntest:c --test_timeout=1\n"
	for cmd, want := range map[string]int{
		"test --config=c //x":             64,
		"test --config=a //x":             128,
		"test --config=ci --config=b //x": 128,
	} {
		if got, err := bazelLaneEffectiveJobs(rc, cmd); err != nil || got != want {
			t.Errorf("bazelLaneEffectiveJobs(%q) = %d, %v; want %d", cmd, got, err, want)
		}
	}
	if _, err := bazelLaneEffectiveJobs("test:a --jobs=128\n", "test --config=a"); err == nil {
		t.Error("bazelLaneEffectiveJobs accepted a .bazelrc with no build:remote-exec --jobs")
	}
}
