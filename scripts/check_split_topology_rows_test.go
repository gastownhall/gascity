package scripts_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The split-store conformance suite's whole enforcement deliverable is
// scripts/check-split-topology-rows.sh: every invariant must run on both store
// topologies, or the suite reads as coverage while policing one row.
//
// The guard is wired into `make check`, and `make check` is invoked by no
// workflow, no hook and no script — CI runs the individual check-* targets. So
// a drifted invariant would land: the suite itself stays internally consistent
// and passes in the cmd/gc cover shards, and the one thing that catches the
// drift never runs. This file is the second route, the one
// scripts/gomod_replace_guard_test.go already established: ./scripts is inside
// UNIT_COVER_PKGS_NONCMDGC, which CI runs as "Preflight / unit cover
// (noncmdgc)", so exec'ing the script from a Go test puts it in CI with no
// ci.yml edit and no workflow shape-hash bump.
//
// It also gives the guard the thing it did not have: a test of its own bite. An
// edit to the Rule A pattern or the fan-out helper list can otherwise defang it
// silently, because nothing asserts the script's behavior.

// splitTopologyGuard returns the path to the guard script.
func splitTopologyGuard(t *testing.T) string {
	t.Helper()
	script := filepath.Join(repoRoot(t), "scripts", "check-split-topology-rows.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("check-split-topology-rows.sh not found at %s: %v", script, err)
	}
	return script
}

// runSplitTopologyGuard runs the guard against a repo root and returns its
// combined output and exit code.
func runSplitTopologyGuard(t *testing.T, root string) (string, int) {
	t.Helper()
	out, err := exec.Command("bash", splitTopologyGuard(t), root).CombinedOutput()
	if err != nil {
		exit := &exec.ExitError{}
		if errors.As(err, &exit) {
			return string(out), exit.ExitCode()
		}
		t.Fatalf("exec %s: %v", splitTopologyGuard(t), err)
	}
	return string(out), 0
}

// splitTopologyFixtureFile is the stand-in for split_topology_env_test.go: it
// DEFINES the fan-out helpers, so Rule B must exempt it even though it calls
// newSplitEnv.
const splitTopologyFixtureFile = `package main

func forEachTopology(t *testing.T, fn func(t *testing.T, e splitEnv)) {
	t.Run("single-store", func(t *testing.T) { fn(t, newSplitEnv(t, false)) })
	t.Run("split", func(t *testing.T) { fn(t, newSplitEnv(t, true)) })
}

func forEachTopologyWithRig(t *testing.T, fn func(t *testing.T, e splitEnv)) {
	t.Run("single-store", func(t *testing.T) { fn(t, newSplitEnvWithRig(t, false)) })
	t.Run("split", func(t *testing.T) { fn(t, newSplitEnvWithRig(t, true)) })
}
`

// synthSplitTopologyRepo stages a minimal repo root: the fixture file plus the
// named suite files.
func synthSplitTopologyRepo(t *testing.T, suites map[string]string) string {
	t.Helper()
	root := t.TempDir()
	gcDir := filepath.Join(root, "cmd", "gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatalf("mkdir cmd/gc: %v", err)
	}
	writeTestFile(t, filepath.Join(gcDir, "split_topology_env_test.go"), splitTopologyFixtureFile)
	for name, content := range suites {
		writeTestFile(t, filepath.Join(gcDir, name), content)
	}
	return root
}

// TestCheckSplitTopologyRowsPassesOnThisRepo is the live case: the guard must
// exit 0 against the real tree. Without it every synthetic case below could
// pass while the checked-in suite drifted.
func TestCheckSplitTopologyRowsPassesOnThisRepo(t *testing.T) {
	out, code := runSplitTopologyGuard(t, repoRoot(t))
	if code != 0 {
		t.Fatalf("the guard fails on the checked-in tree (exit %d):\n%s", code, out)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("the guard passed but was not silent:\n%s", out)
	}
}

// TestCheckSplitTopologyRowsAcceptsConformingSuites pins the shapes that must
// NOT be reported. A guard that false-positives on the idiomatic multi-line
// subtest gets edited into uselessness by the first contributor who trips it.
func TestCheckSplitTopologyRowsAcceptsConformingSuites(t *testing.T) {
	for _, tc := range []struct {
		name  string
		suite string
	}{
		{
			"single_line_subtests",
			`package main

func TestConformance(t *testing.T) {
	t.Run("I1-alpha", func(t *testing.T) { forEachTopology(t, bodyAlpha) })
	t.Run("I2-beta", func(t *testing.T) { forEachTopologyWithRig(t, bodyBeta) })
}
`,
		},
		{
			"multi_line_subtests",
			`package main

func TestConformance(t *testing.T) {
	t.Run("I1-alpha", func(t *testing.T) {
		forEachTopology(t, bodyAlpha)
	})
	t.Run("I2-beta", func(t *testing.T) {
		seedSomething(t)
		forEachTopologyWithRig(t, bodyBeta)
	})
}
`,
		},
		{
			"doc_comments_quoting_both_shapes",
			`package main

// Run one invariant with t.Run("I3-example") — and never call
// newSplitEnv(t, true) directly; the fan-out helpers own env construction.
func TestConformance(t *testing.T) {
	t.Run("I1-alpha", func(t *testing.T) { forEachTopology(t, bodyAlpha) })
}
`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := synthSplitTopologyRepo(t, map[string]string{"split_topology_conformance_test.go": tc.suite})
			out, code := runSplitTopologyGuard(t, root)
			if code != 0 {
				t.Fatalf("conforming suite reported a violation (exit %d):\n%s", code, out)
			}
		})
	}
}

// TestCheckSplitTopologyRowsCatchesDrift is the bite: each case is a way a
// one-topology invariant reaches the tree, and each must exit non-zero with a
// message that names the rule.
func TestCheckSplitTopologyRowsCatchesDrift(t *testing.T) {
	for _, tc := range []struct {
		name   string
		suites map[string]string
		want   string
	}{
		{
			// Rule A + Rule B, the shape the PR's own bite evidence used.
			name: "invariant_pins_one_topology_inline",
			suites: map[string]string{"split_topology_conformance_test.go": `package main

func TestConformance(t *testing.T) {
	t.Run("I1-alpha", func(t *testing.T) { forEachTopology(t, bodyAlpha) })
	t.Run("I12-drifted", func(t *testing.T) { bodyDrift(t, newSplitEnv(t, true)) })
}
`},
			want: "does not run both topologies",
		},
		{
			// Rule A over a multi-line body: the drift is real and the helper
			// really is absent, so the brace-balanced read must still report it.
			name: "multi_line_invariant_without_fan_out",
			suites: map[string]string{"split_topology_conformance_test.go": `package main

func TestConformance(t *testing.T) {
	t.Run("I1-alpha", func(t *testing.T) { forEachTopology(t, bodyAlpha) })
	t.Run("I12-drifted", func(t *testing.T) {
		e := buildEnvSomehow(t)
		bodyDrift(t, e)
	})
}
`},
			want: "does not run both topologies",
		},
		{
			// A fan-out helper renamed to a one-topology lookalike must not
			// satisfy Rule A by resembling the real ones.
			name: "one_topology_helper_lookalike",
			suites: map[string]string{"split_topology_conformance_test.go": `package main

func TestConformance(t *testing.T) {
	t.Run("I1-alpha", func(t *testing.T) { forEachTopologySplitOnly(t, bodyAlpha) })
}
`},
			want: "does not run both topologies",
		},
		{
			// Rule B away from a t.Run line: a suite helper that mints its own env.
			name: "suite_helper_constructs_env_directly",
			suites: map[string]string{"split_topology_conformance_test.go": `package main

func TestConformance(t *testing.T) {
	t.Run("I1-alpha", func(t *testing.T) { forEachTopology(t, bodyAlpha) })
}

func splitOnlyEnv(t *testing.T) splitEnv { return newSplitEnvWithRig(t, true) }
`},
			want: "direct newSplitEnv",
		},
		{
			// Rule C: renaming the convention must not silently empty the
			// denominator and let Rules A and B pass over nothing.
			name: "convention_renamed_away",
			suites: map[string]string{"split_topology_conformance_test.go": `package main

func TestConformance(t *testing.T) {
	t.Run("ZZZ1-alpha", func(t *testing.T) { forEachTopology(t, bodyAlpha) })
}
`},
			want: "found no suite to police",
		},
		{
			// Discovery: the guard used to scan one hard-coded path, so drift in
			// a second conformance file was unpoliced.
			name: "drift_in_a_second_suite_file",
			suites: map[string]string{
				"split_topology_conformance_test.go": `package main

func TestConformance(t *testing.T) {
	t.Run("I1-alpha", func(t *testing.T) { forEachTopology(t, bodyAlpha) })
}
`,
				"split_topology_conformance_more_test.go": `package main

func TestMoreConformance(t *testing.T) {
	t.Run("I12-drifted", func(t *testing.T) { bodyDrift(t, newSplitEnv(t, true)) })
}
`,
			},
			want: "split_topology_conformance_more_test.go",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := synthSplitTopologyRepo(t, tc.suites)
			out, code := runSplitTopologyGuard(t, root)
			if code == 0 {
				t.Fatalf("drifted suite passed the guard:\n%s", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("guard output does not name the rule (want %q):\n%s", tc.want, out)
			}
		})
	}
}

// TestCheckSplitTopologyRowsWiredIntoMakefile keeps the shell route intact: the
// target exists, calls the script, is a declared .PHONY, and is reachable from
// `make check`. The Go route above is what CI executes; this one is what a
// contributor running `make check` gets, and losing it silently would leave the
// guard's own documentation wrong.
func TestCheckSplitTopologyRowsWiredIntoMakefile(t *testing.T) {
	makefile, err := os.ReadFile(filepath.Join(repoRoot(t), "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	content := string(makefile)
	for _, want := range []string{
		"check-split-topology-rows:",
		"./scripts/check-split-topology-rows.sh",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("Makefile missing %q", want)
		}
	}
	if !splitTopologyMakeLineHas(content, "check:", "check-split-topology-rows") {
		t.Error("`make check` does not run check-split-topology-rows")
	}
	if !splitTopologyMakeLineHas(content, ".PHONY:", "check-split-topology-rows") {
		t.Error("check-split-topology-rows is not declared .PHONY; a file of that name in the repo root would silently skip the guard")
	}
}

// splitTopologyMakeLineHas reports whether some Makefile line starting with
// prefix mentions target.
func splitTopologyMakeLineHas(content, prefix, target string) bool {
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, prefix) && strings.Contains(line, target) {
			return true
		}
	}
	return false
}
