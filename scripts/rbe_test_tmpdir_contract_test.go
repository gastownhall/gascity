package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBazelExecutionHostsProvisionConfiguredTestTmpdir(t *testing.T) {
	root := repoRoot(t)
	const provision = "sudo install -d -m 1777 /tmp/bt"

	for _, rel := range []string{
		filepath.Join(".github", "workflows", "bazel-test.yml"),
		filepath.Join("tools", "rbe", "blacksmith-worker.sh"),
	} {
		body, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if !strings.Contains(string(body), provision) {
			t.Errorf("%s does not provision Bazel's /tmp/bt execution-host contract", rel)
		}
	}
}
