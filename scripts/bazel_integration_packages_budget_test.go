package scripts_test

import (
	"encoding/json"
	"errors"
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
// lane's test actions, one per target shard. aquery-verified (77 TestRunner
// actions) at 3834a030b8; recompute with
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
	"//internal/beads:beads_test":                                10,
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
}

// integrationPackagesReadBuilds: the packages whose BUILD.bazel
// //scripts:scripts_test's data carries, so the budget check reads their
// shard_count under bazel too, not only under `go test`.
var integrationPackagesReadBuilds = []string{"cmd/gc", "examples/gastown", "internal/beads"}

// integrationPackagesTargets expands //test:integration_packages' tests
// list: go_test labels as they are, and cmd/gc's :gc_integration_solo_tests
// suite into its INTEGRATION_SOLO_TESTS and INTEGRATION_SOLO_SPLIT_TESTS
// target names (each a go_variant_test, unsharded).
func integrationPackagesTargets(t *testing.T, root string) []string {
	t.Helper()
	suite := readFile(t, root, "test/BUILD.bazel")
	body := regexp.MustCompile(`(?s)test_suite\(\n    name = "integration_packages",.*?tests = \[\n(.*?)\n    \],`).FindStringSubmatch(suite)
	if body == nil {
		t.Fatal("test/BUILD.bazel has no integration_packages test_suite tests list")
	}
	var out []string
	for _, m := range regexp.MustCompile(`"(//[^"]+)"`).FindAllStringSubmatch(body[1], -1) {
		if m[1] != "//cmd/gc:gc_integration_solo_tests" {
			out = append(out, m[1])
			continue
		}
		gc := readFile(t, root, "cmd/gc/BUILD.bazel")
		for _, table := range []string{"INTEGRATION_SOLO_TESTS", "INTEGRATION_SOLO_SPLIT_TESTS"} {
			tb := regexp.MustCompile(`(?ms)^` + table + ` = \{\n(.*?)^\}`).FindStringSubmatch(gc)
			if tb == nil {
				t.Fatalf("cmd/gc/BUILD.bazel has no %s dict", table)
			}
			for _, e := range regexp.MustCompile(`(?m)^\s+"(gc_[a-z0-9_]+_test)": `).FindAllStringSubmatch(tb[1], -1) {
				out = append(out, "//cmd/gc:"+e[1])
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
	if total > budget {
		t.Errorf("integration-packages (%s): %d test actions, more than the effective --jobs=%d: a second wave would add its tail test's full duration to the critical path instead of overlapping it (closed PR #7542 measured up to +575s from exactly this)", cmd, total, budget)
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
