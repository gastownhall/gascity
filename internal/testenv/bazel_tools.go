package testenv

import (
	"os"
	"path/filepath"
	"strings"
)

// BazelToolPathsVar names the env var through which a Bazel go_test hands its
// test binary the executables (bd, dolt, ...) the code under test runs by name
// from PATH. Its value is the space-separated $(rootpaths ...) of data deps,
// e.g.
//
//	env = {"GC_TEST_TOOL_PATHS": "$(rootpath @bd_bin_v1_3_1//:bd)"}
//
// Under `bazel test` init prepends each tool's runfiles directory to PATH, so
// the pinned, declared binary wins over anything on the worker's PATH (which
// .bazelrc pins to the host's system directories). Outside Bazel the var is
// ignored: `go test` callers put their tools on PATH themselves.
const BazelToolPathsVar = "GC_TEST_TOOL_PATHS"

// bazelToolPATH returns path with the directory of every tool in tools (a
// space-separated list of rootpaths, relative to the runfiles directory of
// the main repository srcdir/workspace) prepended in order, each directory
// once and only if path lacks it. It returns path unchanged when not under Bazel (srcdir or workspace
// empty) or when tools names nothing.
func bazelToolPATH(path, srcdir, workspace, tools string) string {
	if srcdir == "" || workspace == "" {
		return path
	}
	var dirs []string
	seen := map[string]bool{}
	// A directory already on PATH stays where it is: a re-executed test
	// binary (helper process, testscript command) inherits the prepended
	// PATH, and a test that put its own stubs in front keeps them there.
	for _, dir := range filepath.SplitList(path) {
		seen[dir] = true
	}
	for _, rel := range strings.Fields(tools) {
		dir := filepath.Dir(filepath.Join(srcdir, workspace, filepath.FromSlash(rel)))
		if !seen[dir] {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
	}
	if len(dirs) == 0 {
		return path
	}
	if path != "" {
		dirs = append(dirs, path)
	}
	return strings.Join(dirs, string(filepath.ListSeparator))
}

// prependBazelTools applies bazelToolPATH to the process PATH.
func prependBazelTools() {
	path := os.Getenv("PATH")
	if next := bazelToolPATH(path, os.Getenv("TEST_SRCDIR"), os.Getenv("TEST_WORKSPACE"), os.Getenv(BazelToolPathsVar)); next != path {
		_ = os.Setenv("PATH", next)
	}
}
