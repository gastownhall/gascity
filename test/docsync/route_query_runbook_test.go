package docsync

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// T-A8's documentation boundary: the operator must have a copyable route
// reader, with explicit limits and repeated exclusions. T-A6's executable
// failure matrix belongs to the pack suite, not to these documentation tests.
func TestRouteQueryRunbookShowsSupportedReader(t *testing.T) {
	const path = "docs/runbooks/split-storage-classes.md"
	data, err := os.ReadFile(filepath.Join(repoRoot(), filepath.FromSlash(path)))
	if err != nil {
		t.Fatal(err)
	}
	var example string
	for _, fence := range regexp.MustCompile("(?s)```[^\n]*\n(.*?)```").FindAllStringSubmatch(string(data), -1) {
		if strings.Contains(fence[1], "gc_out=$(gc ready") && strings.Contains(fence[1], "gc.routed_to=") {
			example = fence[1]
			break
		}
	}
	if example == "" {
		t.Fatalf("%s has no copyable route-query example with a top-level gc ready capture", path)
	}
	for _, flag := range []string{"--metadata-field", "--unassigned", "--limit", "--json"} {
		if !strings.Contains(example, flag) {
			t.Errorf("runbook route reader omits supported flag %s", flag)
		}
	}
	for _, flag := range []string{"--label ", "--label-any", "--rig"} {
		if strings.Contains(example, flag) {
			t.Errorf("runbook dispatch example relies on unsupported selector %s", flag)
		}
	}
	for _, value := range []string{"step", "molecule", "convoy", "hold:mayor", "hold:external", "human", "parent-record"} {
		flag := "--exclude-type"
		if strings.Contains(value, ":") || value == "human" || value == "parent-record" {
			flag = "--exclude-label"
		}
		if !regexp.MustCompile(regexp.QuoteMeta(flag) + `[ =]+["']?` + regexp.QuoteMeta(value) + `(?:["'\s]|$)`).MatchString(example) {
			t.Errorf("runbook omits repeated exclusion %s %s", flag, value)
		}
	}
}
