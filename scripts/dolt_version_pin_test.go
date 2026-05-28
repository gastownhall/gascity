package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoltVersionPins(t *testing.T) {
	const doltVersion = "2.0.7"
	repoRoot := repoRoot(t)

	assertContains := func(rel, want string) {
		t.Helper()
		content, err := os.ReadFile(filepath.Join(repoRoot, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if !strings.Contains(string(content), want) {
			t.Fatalf("%s missing %q", rel, want)
		}
	}
	assertCount := func(rel, want string, count int) {
		t.Helper()
		content, err := os.ReadFile(filepath.Join(repoRoot, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if got := strings.Count(string(content), want); got != count {
			t.Fatalf("%s has %d copies of %q, want %d", rel, got, want, count)
		}
	}

	assertContains("deps.env", "DOLT_VERSION="+doltVersion)
	assertContains("contrib/k8s/Dockerfile.base", "ARG DOLT_VERSION="+doltVersion)
	assertCount("contrib/k8s/dolt-statefulset.yaml", "image: dolthub/dolt:"+doltVersion, 2)

	for _, platform := range []string{"linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64"} {
		assertContains(".github/scripts/install-dolt-archive.sh", doltVersion+":"+platform)
	}

	workflowDir := filepath.Join(repoRoot, ".github", "workflows")
	err := filepath.WalkDir(workflowDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".yml" {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(content), "DOLT_VERSION:") &&
			!strings.Contains(string(content), `DOLT_VERSION: "`+doltVersion+`"`) {
			rel, _ := filepath.Rel(repoRoot, path)
			t.Fatalf("%s has DOLT_VERSION but is not pinned to %s", rel, doltVersion)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk workflows: %v", err)
	}
}
