package scripts_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// goTwinWorkflowExceptions may call the Makefile's plain-go `-go` twins:
// macOS runs its suites natively on Blacksmith (rbe-west has no macOS
// workers, and the Bazel graph's bd, dolt and LLVM archives are
// linux-amd64), so mac-regression.yml keeps the go-test path.
var goTwinWorkflowExceptions = map[string]string{
	"mac-regression.yml": "macOS: no rbe-west workers; native go test on Blacksmith",
}

// TestWorkflowsCallNoGoTwins (ga-96smfk.59): every Linux workflow runs its
// suites through Bazel lanes, so no workflow other than the macOS exception
// calls a `make <target>-go` twin.
func TestWorkflowsCallNoGoTwins(t *testing.T) {
	root := repoRoot(t)
	paths, err := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.y*ml"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("glob workflows: %v (%d files)", err, len(paths))
	}
	sort.Strings(paths)
	twin := regexp.MustCompile(`\bmake\s+(?:-\S+\s+)*([a-z0-9-]+-go)\b`)
	seenException := map[string]bool{}
	for _, path := range paths {
		name := filepath.Base(path)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(body), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			m := twin.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			if _, ok := goTwinWorkflowExceptions[name]; ok {
				seenException[name] = true
				continue
			}
			t.Errorf("%s:%d runs `make %s`; run its Bazel target instead (bazel.yml lane), or document the exception in goTwinWorkflowExceptions", name, i+1, m[1])
		}
	}
	for name := range goTwinWorkflowExceptions {
		if !seenException[name] {
			t.Errorf("goTwinWorkflowExceptions lists %s, which no longer calls a -go twin; drop it", name)
		}
	}
}
