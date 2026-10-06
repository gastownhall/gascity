package scripts_test

import (
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const (
	integrationBuild      = "test/integration/BUILD.bazel"
	integrationHeavyTests = "test/integration/heavy_tests.txt"
)

// shardedGoTest names a sharded go_test whose long tests a guard keeps in
// separate shards: the BUILD file and rule, the package directory, the tag
// set its srcs build under, and the manifest of its long tests.
type shardedGoTest struct {
	build, rule, dir, heavyTests string
	tags                         map[string]bool
}

var integrationSuite = shardedGoTest{
	build:      integrationBuild,
	rule:       "integration_test",
	dir:        "test/integration",
	heavyTests: integrationHeavyTests,
	tags:       integrationLaneTags,
}

// TestIntegrationShardsKeepHeavyTestsApart guards the integration lane's
// per-shard time budget. rules_go assigns the tests of a sharded go_test
// round-robin in source order (testsInShard in its generated test main: test i
// runs in shard i % shard_count), so where a long test lands is a pure
// function of the srcs list and shard_count. Two of the tests listed in
// test/integration/heavy_tests.txt in one shard is how the 4-shard target
// reached the 1100 s --test_timeout on main (run 37137993899).
func TestIntegrationShardsKeepHeavyTestsApart(t *testing.T) {
	checkHeavyTestsApart(t, integrationSuite)
}

// checkHeavyTestsApart fails when suite's shard_count puts two of its listed
// long tests in one shard, and prints the shard counts that would not.
func checkHeavyTestsApart(t *testing.T, suite shardedGoTest) {
	t.Helper()
	root := repoRoot(t)
	build := readFile(t, root, suite.build)
	rule := goTestRule(t, build, suite.build, suite.rule)
	shards := goTestShardCount(t, rule, suite.build)
	order := goTestOrder(t, root, suite.dir, goTestSrcs(t, rule, suite.build, suite.rule), suite.tags)
	heavy := heavyTestNames(t, readFile(t, root, suite.heavyTests))

	index := make(map[string]int, len(order))
	for i, name := range order {
		index[name] = i
	}
	for _, name := range heavy {
		if _, ok := index[name]; !ok {
			t.Errorf("%s lists %s, which is not a test in %s; remove or rename it", suite.heavyTests, name, suite.build)
		}
	}
	if len(heavy) > shards {
		t.Fatalf("%d heavy tests cannot be separated by shard_count = %d in %s; values that keep them apart: %v",
			len(heavy), shards, suite.build, separatingShardCounts(order, heavy, len(heavy), len(heavy)+16))
	}
	if clash := heavyShardClashes(order, heavy, shards); len(clash) > 0 {
		t.Errorf("shard_count = %d in %s puts long tests together: %s.\nshard_count values that keep them apart: %v",
			shards, suite.build, strings.Join(clash, "; "), separatingShardCounts(order, heavy, len(heavy), shards+16))
	}
}

func heavyShardClashes(order, heavy []string, shards int) []string {
	isHeavy := make(map[string]bool, len(heavy))
	for _, name := range heavy {
		isHeavy[name] = true
	}
	owner := map[int]string{}
	var clash []string
	for i, name := range order {
		if !isHeavy[name] {
			continue
		}
		shard := i % shards
		if prev, ok := owner[shard]; ok {
			clash = append(clash, prev+" and "+name+" in shard "+strconv.Itoa(shard+1))
			continue
		}
		owner[shard] = name
	}
	return clash
}

func separatingShardCounts(order, heavy []string, from, to int) []int {
	var ok []int
	for n := from; n <= to; n++ {
		if len(heavyShardClashes(order, heavy, n)) == 0 {
			ok = append(ok, n)
		}
	}
	return ok
}

// goTestRule returns the go_test(name = rule) text in build (read from path).
func goTestRule(t *testing.T, build, path, rule string) string {
	t.Helper()
	start := strings.Index(build, "go_test(\n    name = \""+rule+"\",")
	if start < 0 {
		t.Fatalf("%s: no go_test named %s", path, rule)
	}
	rest := build[start:]
	end := strings.Index(rest, "\n)\n")
	if end < 0 {
		t.Fatalf("%s: unterminated %s rule", path, rule)
	}
	return rest[:end]
}

func goTestShardCount(t *testing.T, rule, path string) int {
	t.Helper()
	m := regexp.MustCompile(`(?m)^\s*shard_count = (\d+),`).FindStringSubmatch(rule)
	if m == nil {
		return 1
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 1 {
		t.Fatalf("%s: bad shard_count %q", path, m[1])
	}
	return n
}

func goTestSrcs(t *testing.T, rule, path, name string) []string {
	t.Helper()
	start := strings.Index(rule, "srcs = [")
	if start < 0 {
		t.Fatalf("%s: %s has no srcs list", path, name)
	}
	body, _, ok := strings.Cut(rule[start+len("srcs = ["):], "]")
	if !ok {
		t.Fatalf("%s: %s srcs list is not closed", path, name)
	}
	var srcs []string
	for _, m := range regexp.MustCompile(`"([^"]+\.go)"`).FindAllStringSubmatch(body, -1) {
		srcs = append(srcs, m[1])
	}
	if len(srcs) == 0 {
		t.Fatalf("%s: %s srcs is empty", path, name)
	}
	return srcs
}

// goTestOrder lists the top-level tests the way rules_go's test main does:
// srcs in order, declarations in file order, TestMain excluded. Only srcs
// that actually build for the lane's target (tags on linux/amd64) contribute
// tests: a file excluded by its own //go:build line or by a GOOS/GOARCH
// filename suffix never reaches rules_go's shard assignment, so counting its
// tests here would misplace every test after it.
func goTestOrder(t *testing.T, root, dir string, srcs []string, tags map[string]bool) []string {
	t.Helper()
	var order []string
	fset := token.NewFileSet()
	for _, src := range srcs {
		path := filepath.Join(root, filepath.FromSlash(dir), src)
		if !fileAppliesOnLinuxAMD64(t, path, tags) {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parsing %s: %v", src, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !isGoTestName(fn.Name.Name) || !isGoTestFunc(fn, "T") {
				continue
			}
			order = append(order, fn.Name.Name)
		}
	}
	return order
}

// integrationLaneTags is the tag set the integration lane's shard guard
// evaluates //go:build lines against: the "integration" gazelle:build_tags
// directive plus the lane's canonical linux/amd64 platform (bazel-test.yml
// and scripts/test-integration-shard both run it there).
var integrationLaneTags = map[string]bool{"integration": true, "linux": true, "amd64": true}

// knownGOOS and knownGOARCH list every GOOS/GOARCH recognized by the Go
// toolchain's _GOOS/_GOARCH/_GOOS_GOARCH filename convention (`go tool dist
// list`), so a filename suffix for a platform this lane does not target
// (e.g. _darwin.go, _windows_amd64.go) is recognized as such rather than
// mistaken for an ordinary identifier.
var (
	knownGOOS = map[string]bool{
		"aix": true, "android": true, "darwin": true, "dragonfly": true,
		"freebsd": true, "illumos": true, "ios": true, "js": true,
		"linux": true, "netbsd": true, "openbsd": true, "plan9": true,
		"solaris": true, "wasip1": true, "windows": true,
	}
	knownGOARCH = map[string]bool{
		"386": true, "amd64": true, "arm": true, "arm64": true,
		"loong64": true, "mips": true, "mips64": true, "mips64le": true,
		"mipsle": true, "ppc64": true, "ppc64le": true, "riscv64": true,
		"s390x": true, "wasm": true,
	}
)

// integrationFileAppliesOnLinuxAMD64 reports whether src is part of the
// integration lane's build on linux/amd64: its filename carries no
// GOOS/GOARCH suffix for a different platform, and its //go:build line (if
// any) evaluates true against integrationLaneTags.
func integrationFileAppliesOnLinuxAMD64(t *testing.T, path string) bool {
	t.Helper()
	return fileAppliesOnLinuxAMD64(t, path, integrationLaneTags)
}

// fileAppliesOnLinuxAMD64 reports whether path builds on linux/amd64 with
// tags set: no GOOS/GOARCH filename suffix for another platform, and its
// //go:build line (if any) true against tags.
func fileAppliesOnLinuxAMD64(t *testing.T, path string, tags map[string]bool) bool {
	t.Helper()
	if !goodOSArchFilename(filepath.Base(path)) {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	expr, err := leadingGoBuildConstraint(data)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if expr == nil {
		return true
	}
	return expr.Eval(func(tag string) bool { return tags[tag] })
}

// leadingGoBuildConstraint returns the //go:build expression in data's
// leading comments, or nil if there is none.
func leadingGoBuildConstraint(data []byte) (constraint.Expr, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "", data, parser.ParseComments|parser.PackageClauseOnly)
	if err != nil {
		return nil, err
	}
	for _, group := range file.Comments {
		for _, comment := range group.List {
			line := strings.TrimSpace(comment.Text)
			if constraint.IsGoBuild(line) {
				return constraint.Parse(line)
			}
		}
	}
	return nil, nil
}

// goodOSArchFilename reports whether src's _GOOS, _GOARCH or _GOOS_GOARCH
// filename suffix, if any, names the integration lane's linux/amd64
// platform. A filename with no such suffix always matches.
func goodOSArchFilename(src string) bool {
	name := strings.TrimSuffix(src, ".go")
	name = strings.TrimSuffix(name, "_test")
	parts := strings.Split(name, "_")
	if len(parts) < 2 {
		return true
	}
	last := parts[len(parts)-1]
	switch {
	case knownGOARCH[last]:
		if len(parts) >= 3 && knownGOOS[parts[len(parts)-2]] {
			return parts[len(parts)-2] == "linux" && last == "amd64"
		}
		return last == "amd64"
	case knownGOOS[last]:
		return last == "linux"
	default:
		return true
	}
}

// TestIntegrationFileAppliesOnLinuxAMD64 pins the shard guard's build-tag and
// filename-suffix filtering: a file the integration lane's own
// linux/amd64+integration build would exclude must not contribute tests to
// goTestOrder, or the guard would misplace every test that follows
// it in rules_go's round-robin shard assignment.
func TestIntegrationFileAppliesOnLinuxAMD64(t *testing.T) {
	dir := t.TempDir()
	writeFile := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
		return path
	}

	tests := []struct {
		name    string
		path    string
		content string
		want    bool
	}{
		{
			name: "untagged file",
			path: writeFile("plain_test.go", "package integration\n"),
			want: true,
		},
		{
			name: "go:build integration matches the lane",
			path: writeFile("tagged_test.go", "//go:build integration\n\npackage integration\n"),
			want: true,
		},
		{
			name: "go:build integration && darwin never applies on linux",
			path: writeFile("darwin_tagged_test.go", "//go:build integration && darwin\n\npackage integration\n"),
			want: false,
		},
		{
			name: "go:build !integration excludes the integration lane build",
			path: writeFile("unit_only_test.go", "//go:build !integration\n\npackage integration\n"),
			want: false,
		},
		{
			name: "_darwin.go filename suffix",
			path: writeFile("helpers_darwin_test.go", "package integration\n"),
			want: false,
		},
		{
			name: "_linux_amd64.go filename suffix",
			path: writeFile("helpers_linux_amd64_test.go", "package integration\n"),
			want: true,
		},
		{
			name: "_windows_arm64.go filename suffix",
			path: writeFile("helpers_windows_arm64_test.go", "package integration\n"),
			want: false,
		},
		{
			name: "underscore-separated name with no GOOS/GOARCH suffix",
			path: writeFile("bead_id_test.go", "package integration\n"),
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := integrationFileAppliesOnLinuxAMD64(t, tt.path); got != tt.want {
				t.Errorf("integrationFileAppliesOnLinuxAMD64(%s) = %v, want %v", filepath.Base(tt.path), got, tt.want)
			}
		})
	}
}

func heavyTestNames(t *testing.T, manifest string) []string {
	t.Helper()
	var names []string
	for _, line := range strings.Split(manifest, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		names = append(names, strings.Fields(line)[0])
	}
	sort.Strings(names)
	return names
}
