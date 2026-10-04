package scripts_test

import (
	"go/ast"
	"go/parser"
	"go/token"
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

// TestIntegrationShardsKeepHeavyTestsApart guards the integration lane's
// per-shard time budget. rules_go assigns the tests of a sharded go_test
// round-robin in source order (testsInShard in its generated test main: test i
// runs in shard i % shard_count), so where a long test lands is a pure
// function of the srcs list and shard_count. Two of the tests listed in
// test/integration/heavy_tests.txt in one shard is how the 4-shard target
// reached the 1100 s --test_timeout on main (run 37137993899).
func TestIntegrationShardsKeepHeavyTestsApart(t *testing.T) {
	root := repoRoot(t)
	build := readFile(t, root, integrationBuild)
	shards := integrationShardCount(t, build)
	order := integrationTestOrder(t, root, integrationSrcs(t, build))
	heavy := integrationHeavyTestNames(t, readFile(t, root, integrationHeavyTests))

	index := make(map[string]int, len(order))
	for i, name := range order {
		index[name] = i
	}
	for _, name := range heavy {
		if _, ok := index[name]; !ok {
			t.Errorf("%s lists %s, which is not a test in %s; remove or rename it", integrationHeavyTests, name, integrationBuild)
		}
	}
	if len(heavy) > shards {
		t.Fatalf("%d heavy tests cannot be separated by shard_count = %d in %s; values that keep them apart: %v",
			len(heavy), shards, integrationBuild, separatingShardCounts(order, heavy, len(heavy), len(heavy)+16))
	}
	if clash := heavyShardClashes(order, heavy, shards); len(clash) > 0 {
		t.Errorf("shard_count = %d in %s puts long tests together: %s.\nshard_count values that keep them apart: %v",
			shards, integrationBuild, strings.Join(clash, "; "), separatingShardCounts(order, heavy, len(heavy), shards+16))
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

// integrationTarget returns the go_test(name = "integration_test") rule text.
func integrationTarget(t *testing.T, build string) string {
	t.Helper()
	start := strings.Index(build, "go_test(\n    name = \"integration_test\",")
	if start < 0 {
		t.Fatalf("%s: no go_test named integration_test", integrationBuild)
	}
	rest := build[start:]
	end := strings.Index(rest, "\n)\n")
	if end < 0 {
		t.Fatalf("%s: unterminated integration_test rule", integrationBuild)
	}
	return rest[:end]
}

func integrationShardCount(t *testing.T, build string) int {
	t.Helper()
	m := regexp.MustCompile(`(?m)^\s*shard_count = (\d+),`).FindStringSubmatch(integrationTarget(t, build))
	if m == nil {
		return 1
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 1 {
		t.Fatalf("%s: bad shard_count %q", integrationBuild, m[1])
	}
	return n
}

func integrationSrcs(t *testing.T, build string) []string {
	t.Helper()
	rule := integrationTarget(t, build)
	start := strings.Index(rule, "srcs = [")
	if start < 0 {
		t.Fatalf("%s: integration_test has no srcs list", integrationBuild)
	}
	body := rule[start+len("srcs = ["):]
	body = body[:strings.Index(body, "]")]
	var srcs []string
	for _, m := range regexp.MustCompile(`"([^"]+\.go)"`).FindAllStringSubmatch(body, -1) {
		srcs = append(srcs, m[1])
	}
	if len(srcs) == 0 {
		t.Fatalf("%s: integration_test srcs is empty", integrationBuild)
	}
	return srcs
}

// integrationTestOrder lists the top-level tests the way rules_go's test main
// does: srcs in order, declarations in file order, TestMain excluded.
func integrationTestOrder(t *testing.T, root string, srcs []string) []string {
	t.Helper()
	var order []string
	fset := token.NewFileSet()
	for _, src := range srcs {
		file, err := parser.ParseFile(fset, filepath.Join(root, "test", "integration", src), nil, parser.SkipObjectResolution)
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

func integrationHeavyTestNames(t *testing.T, manifest string) []string {
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
